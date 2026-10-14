package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// Resume at a completed cell boundary. A request interrupted partway through
// a cell may have uncommitted reads or repairs, so its observations stay in the
// original report and the new scan reads that cell again.
type ScanResumeInfo struct {
	CellIndex int     `json:"cellIndex"`
	CellCount int     `json:"cellCount"`
	Offset    int64   `json:"offset"`
	Progress  float64 `json:"progress"`
}

func scanCellComplete(result *ScanResult, cell *ScanCell) bool {
	if cell.Status == "pending" || cell.Status == "reading" {
		return false
	}
	target := scanCellTarget(result, cell)
	return target > 0 && cell.CoveredBytes+cell.UnresolvedBytes == target
}

func scanResumeInfo(job *Job) *ScanResumeInfo {
	if job == nil || job.Type != "scan" || (job.Status != "cancelled" && job.Status != "interrupted") || job.Disk.Serial == "" {
		return nil
	}
	r, ok := job.Result.(*ScanResult)
	if !ok || r.Version < 2 || !r.Stats.IssuesComplete || r.Stats.Repair.Pending > 0 || job.Options.ScanMode == nil || *job.Options.ScanMode != r.ScanMode {
		return nil
	}
	if r.ScanMode != "quick" && r.ScanMode != "full" {
		return nil
	}
	if r.ScanMode == "quick" && (r.SampleBytes == nil || *r.SampleBytes != fastSampleSize || r.Coverage != "sampled") {
		return nil
	}
	if r.SamplingPlan != "" && r.SamplingPlan != stratifiedScanPlan {
		return nil
	}
	if r.ScanMode == "full" && (r.Coverage != "full" || r.SamplingPlan != "") {
		return nil
	}
	if len(r.Map) != scanCellCount || job.Disk.Size <= 0 || job.Disk.LogicalSectorSize <= 0 {
		return nil
	}
	want := createScanMap(job.Disk.Size, len(r.Map), job.Disk.LogicalSectorSize)
	index, sawIncomplete, observed := 0, false, false
	for i := range r.Map {
		c := &r.Map[i]
		if c.Index != i || c.Start != want[i].Start || c.End != want[i].End || c.BytesRead < 0 || c.CoveredBytes < 0 || c.UnresolvedBytes < 0 {
			return nil
		}
		switch c.Status {
		case "pending", "reading", "excellent", "good", "normal", "slow", "slower", "severe", "error", "under5", "under20", "under50", "under150", "under500", "over500":
		default:
			return nil
		}
		target := c.End - c.Start
		if r.ScanMode == "quick" {
			target = min(target, fastSampleSize)
		}
		if c.CoveredBytes+c.UnresolvedBytes > target {
			return nil
		}
		if (r.SamplingPlan == "" && len(c.SampleRanges) > 0) || (r.ScanMode == "full" && (c.SampleStart != nil || c.SampleEnd != nil)) {
			return nil
		}
		if c.CoveredBytes+c.UnresolvedBytes > 0 {
			observed = true
		}
		if r.ScanMode == "quick" && scanCellComplete(r, c) {
			ranges := quickSampleRanges(r, c, job.Disk.Size, job.Disk.LogicalSectorSize, fastSampleSize)
			if c.SampleStart == nil || c.SampleEnd == nil || *c.SampleStart != ranges[0].Start || *c.SampleEnd != ranges[len(ranges)-1].End {
				return nil
			}
			if r.SamplingPlan == stratifiedScanPlan {
				if len(c.SampleRanges) != len(ranges) {
					return nil
				}
				for j := range ranges {
					if c.SampleRanges[j] != ranges[j] {
						return nil
					}
				}
			}
		}
		if scanCellComplete(r, c) {
			if sawIncomplete {
				return nil
			}
			index++
		} else {
			sawIncomplete = true
		}
	}
	if !observed || index == len(r.Map) {
		return nil
	}
	offset := r.Map[index].Start
	progress := float64(index) / float64(len(r.Map)) * 100
	if r.ScanMode == "full" {
		progress = float64(offset) / float64(job.Disk.Size) * 100
	}
	return &ScanResumeInfo{CellIndex: index, CellCount: len(r.Map), Offset: offset, Progress: round1(progress)}
}

func scanResumeMatchesDisk(job *Job, disk *Disk) bool {
	return disk != nil && job.Disk.Serial != "" && job.Disk.Serial == disk.Serial && job.Disk.Model == disk.Model && job.Disk.Size == disk.Size && job.Disk.LogicalSectorSize == disk.LogicalSectorSize && job.Disk.PhysicalSectorSize == disk.PhysicalSectorSize && job.Disk.Rotational == disk.Rotational
}

func (m *JobManager) prepareResumedScan(job, source *Job) error {
	info := scanResumeInfo(source)
	if info == nil {
		return newJobError("该记录没有可继续的扫描进度", "SCAN_RESUME_UNAVAILABLE", 409)
	}
	result, previous := job.Result.(*ScanResult), source.Result.(*ScanResult)
	result.SamplingPlan, result.SamplingSeed = previous.SamplingPlan, previous.SamplingSeed
	copy(result.Map[:info.CellIndex], previous.Map[:info.CellIndex])
	if previous.Diagnostics != nil {
		d := *previous.Diagnostics
		d.After, d.Increased = nil, nil
		result.Diagnostics = &d
	}
	// Stream the complete journal; the in-memory preview is limited to 200.
	f, err := os.Open(m.scanIssuePath(source.ID))
	if err != nil && (!os.IsNotExist(err) || previous.Stats.SavedIssueCount > 0) {
		return fmt.Errorf("无法读取原扫描异常记录：%w", err)
	}
	if err == nil {
		defer f.Close()
		out, err := os.OpenFile(m.scanIssuePath(job.ID), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer out.Close()
		encoder, scanner := json.NewEncoder(out), issueScanner(f)
		var count int64
		for scanner.Scan() {
			var record BadRange
			if json.Unmarshal(scanner.Bytes(), &record) != nil || !validScanJournalRecord(record, source.Disk.Size) {
				return fmt.Errorf("原扫描异常记录损坏，无法续扫")
			}
			count++
			if record.Offset+record.Length > info.Offset {
				continue
			}
			if err := encoder.Encode(record); err != nil {
				return err
			}
			countScanIssue(&result.Stats, nil, record)
			result.Stats.SavedIssueCount++
			appendBadRanges(&result.Stats.BadRanges, []BadRange{record}, scanIssuePreviewLimit)
		}
		if err := scanner.Err(); err != nil {
			return err
		}
		if count < previous.Stats.SavedIssueCount {
			return fmt.Errorf("原扫描异常记录不完整，无法续扫")
		}
		if err := out.Sync(); err != nil {
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
	}
	result.Stats.IssuesTruncated = result.Stats.IssueCount > scanIssuePreviewLimit
	job.ResumeFrom, job.Progress, job.ResumeStartProgress = source.ID, info.Progress, info.Progress
	job.Phase = fmt.Sprintf("准备从区域 %d/%d 继续扫描", info.CellIndex+1, info.CellCount)
	job.Log = append(job.Log, fmt.Sprintf("续扫记录 %s，起始位置 %d B；中断的区域重新检测", source.ID, info.Offset))
	m.updateScanStats(job)
	return nil
}
