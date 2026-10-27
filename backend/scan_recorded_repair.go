package main

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// Only exact, successfully read requests are eligible. A slow region alone
// cannot authorize a write to the untested bytes around that request.
type RecordedRepairTarget struct {
	CellIndex int             `json:"cellIndex"`
	Read      ScanReadLatency `json:"read"`
}

func recordedRepairTargets(source *Job, threshold float64) ([]RecordedRepairTarget, error) {
	unavailable := func() error {
		return newJobError("该记录没有可修复的准确慢读地址，请重新扫描定位", "SCAN_REPAIR_RECORD_UNAVAILABLE", 409)
	}
	if source == nil || source.Type != "scan" || source.Status != "completed" || source.Options.ScanRepairFrom != "" {
		return nil, unavailable()
	}
	r, ok := source.Result.(*ScanResult)
	if !ok || r.Version < 2 || (r.Coverage != "full" && r.Coverage != "sampled") || (len(r.Map) != scanCellCount && len(r.Map) != 2048) {
		return nil, unavailable()
	}
	sector := source.Disk.LogicalSectorSize
	if (sector != 512 && sector != 4096) || source.Disk.Size <= 0 {
		return nil, unavailable()
	}
	var targets []RecordedRepairTarget
	end := int64(0)
	for i, cell := range r.Map {
		if cell.Index != i || cell.Start != end || cell.End <= cell.Start || cell.End > source.Disk.Size {
			return nil, unavailable()
		}
		end = cell.End
		read := cell.MaxReadLatency
		if read == nil || math.IsNaN(read.Ms) || math.IsInf(read.Ms, 0) || read.Ms < threshold || cell.ErrorCount != 0 || cell.SkippedBytes != 0 || (cell.ReadOutcome != "readable" && cell.ReadOutcome != "recovered") {
			continue
		}
		if read.Bytes <= 0 || read.Bytes > scanLatencyRequestBytes || read.Offset < cell.Start || read.Offset > cell.End-int64(read.Bytes) || read.Offset%sector != 0 || int64(read.Bytes)%sector != 0 {
			return nil, unavailable()
		}
		if r.Coverage == "sampled" {
			inside := false
			for _, span := range cell.SampleRanges {
				if span.Start >= cell.Start && span.End <= cell.End && read.Offset >= span.Start && read.Offset+int64(read.Bytes) <= span.End {
					inside = true
				}
			}
			if len(cell.SampleRanges) == 0 && cell.SampleStart != nil && cell.SampleEnd != nil {
				inside = *cell.SampleStart >= cell.Start && *cell.SampleEnd <= cell.End && read.Offset >= *cell.SampleStart && read.Offset+int64(read.Bytes) <= *cell.SampleEnd
			}
			if !inside {
				return nil, unavailable()
			}
		}
		targets = append(targets, RecordedRepairTarget{CellIndex: i, Read: *read})
	}
	if end != source.Disk.Size || len(targets) == 0 {
		return nil, unavailable()
	}
	return targets, nil
}

func (m *JobManager) runRecordedRepair(job *Job) (runErr error) {
	if err := m.start(job); err != nil {
		return err
	}
	cancel := m.beginCancellation(job)
	var handle RepairHandle
	r := job.Result.(*ScanResult)
	defer func() {
		if handle != nil {
			m.closeHandle(handle)
		}
		m.endCancellation(job, cancel)
		m.mu.Lock()
		delete(m.handles, job.ID+"/repair")
		m.mu.Unlock()
		if runErr == nil {
			stats := r.Stats.Repair
			message := fmt.Sprintf("按记录处理 %d 个慢读范围：写回校验通过 %d、慢速改善 %d、修复后仍慢 %d、跳过 %d、失败 %d", len(r.RepairTargets), stats.Verified+stats.StillSlow, stats.Improved, stats.StillSlow, stats.Skipped, stats.Failed)
			if stats.StillSlow > 0 {
				message += "；未反复写入，建议更换硬盘"
			}
			m.complete(job, r, message)
		}
	}()
	current, err := m.assertDiskIdentity(job)
	if err != nil {
		return err
	}
	if reason := scanRepairBlockedReason(current); reason != "" {
		return newJobError(reason, "SCAN_REPAIR_NOT_ALLOWED", 409)
	}
	if !m.mock {
		handle, err = m.awaitOpenRepair(job)
		if err != nil {
			return err
		}
		m.mu.Lock()
		m.handles[job.ID+"/repair"] = handle
		m.mu.Unlock()
	} else {
		m.mu.Lock()
		r.Diagnostics = &ScanDiagnostics{ReadMethod: "mock"}
		m.mu.Unlock()
	}
	for i, target := range r.RepairTargets {
		if err := m.checkCancelled(job); err != nil {
			return err
		}
		m.mu.Lock()
		cell := &r.Map[i]
		cell.Status = "reading"
		job.Phase = fmt.Sprintf("按记录修复 %d/%d · 原区域 %d", i+1, len(r.RepairTargets), target.CellIndex+1)
		m.mu.Unlock()
		if m.mock {
			if err := m.recordRepairEvent(job, cell, target.Read, ScanRepairRecord{Outcome: "skipped", BeforeMs: target.Read.Ms, Detail: "演示模式不执行实际读取或写回"}); err != nil {
				return err
			}
			m.mu.Lock()
			cell.Status = "normal"
			m.mu.Unlock()
		} else {
			original := scanBuffer(target.Read.Bytes)
			began := time.Now()
			n, readErr := m.readScanRequest(job, handle, original, target.Read.Offset)
			ms := float64(time.Since(began).Nanoseconds()) / 1e6
			m.mu.Lock()
			errorsRead := int64(0)
			if readErr != nil || n != len(original) {
				errorsRead = 1
			}
			m.updateCell(cell, ms, int64(max(0, n)), errorsRead, int64(len(original)))
			applyScanReadLatency(cell, &ScanReadLatency{Ms: ms, Bytes: len(original), Offset: target.Read.Offset}, job.Disk.Rotational)
			if errorsRead == 0 {
				cell.CoveredBytes = int64(n)
				cell.ReadOutcome = "readable"
			}
			m.mu.Unlock()
			if readErr != nil || n != len(original) {
				if err := m.recordRepairEvent(job, cell, target.Read, ScanRepairRecord{Outcome: "skipped", BeforeMs: target.Read.Ms, Detail: "当前数据未能完整读出，未提交写入"}); err != nil {
					return err
				}
				if errors.Is(readErr, errCancelled) || errors.Is(readErr, errScanWaitTimeout) {
					return readErr
				}
			} else {
				// Historical slowness authorizes the selected range even if this
				// fresh read is fast. The bytes always come from the current disk.
				region := readRegionResult{bytesRead: n, slowReads: []ScanReadLatency{target.Read}}
				if err := m.repairSlowRequests(job, cell, handle, original, target.Read.Offset, &region); err != nil {
					return err
				}
				if len(region.postRepairReads) == 1 {
					// This cell represents exactly one historical request. Replace
					// its pre-write timing with the slower of the two verified reads.
					latest := region.postRepairReads[0]
					m.mu.Lock()
					cell.MaxMs, cell.AvgMs, cell.TotalMs = floatPtr(latest.Ms), floatPtr(latest.Ms), floatPtr(latest.Ms)
					cell.MaxUnitMs = floatPtr(scanUnitLatency(latest.Ms, int64(latest.Bytes)))
					cell.MaxReadLatency = &latest
					cell.Status = "normal"
					applyScanReadLatency(cell, &latest, job.Disk.Rotational)
					m.mu.Unlock()
				}
			}
		}
		m.mu.Lock()
		m.updateScanStats(job)
		job.Progress = round1(float64(i+1) * 100 / float64(len(r.RepairTargets)))
		m.mu.Unlock()
		m.persistThrottled(job, true)
	}
	return nil
}
