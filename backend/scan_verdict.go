package main

import (
	"fmt"
	"math"
	"strings"
)

// A usability verdict needs two slow successful reads of the exact same
// request, with a usable quiet-load observation. It does not infer unread bytes.
type ScanSlowBlock struct {
	Offset   int64     `json:"offset"`
	Bytes    int       `json:"bytes"`
	FirstMs  float64   `json:"firstMs"`
	RepeatMs float64   `json:"repeatMs"`
	Source   string    `json:"source"`
	Load     *ScanLoad `json:"load"`
}

type ScanBadBlockSummary struct{ Total, Slow, Unreadable int64 }

func scanPersistentSlowBlock(cell ScanCell, rotational bool) *ScanSlowBlock {
	proof := cell.SlowBadBlock
	_, _, threshold := scanReadThresholds(rotational)
	if proof == nil || proof.Load == nil || proof.Load.BackgroundActivity || proof.Bytes <= 0 || proof.Bytes > scanLatencyRequestBytes || proof.Offset < cell.Start || proof.Offset > cell.End-int64(proof.Bytes) || math.IsNaN(proof.FirstMs) || math.IsNaN(proof.RepeatMs) || math.IsInf(proof.FirstMs, 0) || math.IsInf(proof.RepeatMs, 0) || proof.FirstMs < threshold || proof.RepeatMs < threshold {
		return nil
	}
	return proof
}

func scanBadBlocks(result *ScanResult, rotational bool) ScanBadBlockSummary {
	var summary ScanBadBlockSummary
	for _, cell := range result.Map {
		if cell.UnreadableBytes > 0 || cell.ReadOutcome == "unreadable" {
			summary.Unreadable++
		} else if scanPersistentSlowBlock(cell, rotational) != nil {
			summary.Slow++
		}
	}
	summary.Total = summary.Slow + summary.Unreadable
	return summary
}

func severeScanStatus(status string) bool {
	return status == "severe" || status == "over500"
}

// Usability verdicts include severe stalls. ErrorCount and unreadable-byte
// accounting still describe actual I/O failures, never inferred media damage.
func scanAbnormalCellCount(result *ScanResult) int64 {
	var count int64
	for _, cell := range result.Map {
		if cell.Status == "error" || severeScanStatus(cell.Status) {
			count++
		}
	}
	return count
}

// Derive one display row per severe region from the saved map. These rows do
// not add fabricated read errors or repair events to the append-only journal.
func scanSevereRanges(result *ScanResult, media ...bool) []BadRange {
	rotational := !strings.HasPrefix(result.LatencyBasis, "solid-")
	if len(media) > 0 {
		rotational = media[0]
	}
	var records []BadRange
	for _, cell := range result.Map {
		proof := scanPersistentSlowBlock(cell, rotational)
		if (!severeScanStatus(cell.Status) && proof == nil) || cell.End <= cell.Start || (cell.SkippedBytes > 0 && proof == nil) {
			continue
		}
		r := BadRange{Offset: cell.Start, Length: cell.End - cell.Start, Outcome: "severe", Code: "SEVERE_SLOW_READ"}
		parts := []string{fmt.Sprintf("区域 %d：严重慢读，已影响正常使用", cell.Index+1)}
		if proof != nil {
			r.Code, r.Offset, r.Length, r.ProbeBytes = "PERSISTENT_SLOW_READ", proof.Offset, int64(proof.Bytes), int64(proof.Bytes)
			r.Detail = fmt.Sprintf("区域 %d：坏块（慢速），已影响正常使用；同范围 %d B 两次读取 %.1f / %.1f ms；复测期间未检测到并发磁盘活动", cell.Index+1, proof.Bytes, proof.FirstMs, proof.RepeatMs)
			if proof.Source == "repair" {
				r.Detail += "；修复后仍慢，已停止反复写入，建议更换硬盘"
			}
			records = append(records, r)
			continue
		}
		if read := cell.MaxReadLatency; read != nil && read.Bytes > 0 && read.Offset >= cell.Start && int64(read.Bytes) <= cell.End-read.Offset {
			r.Offset, r.Length, r.ProbeBytes = read.Offset, int64(read.Bytes), int64(read.Bytes)
			parts = append(parts, fmt.Sprintf("最慢单次读取 %.1f ms / %d B", read.Ms, read.Bytes))
		} else if cell.MaxMs != nil {
			parts = append(parts, fmt.Sprintf("最慢整次读取 %.1f ms，地址为区域范围", *cell.MaxMs))
		}
		if cell.Recheck != nil {
			if read := cell.Recheck.MaxReadLatency; read != nil {
				parts = append(parts, fmt.Sprintf("复测最慢单次读取 %.1f ms / %d B", read.Ms, read.Bytes))
			}
			if cell.Recheck.Outcome == "not_reproduced" {
				parts = append(parts, "复测未再出现慢响应，保留首次异常")
			}
		}
		r.Detail = strings.Join(parts, "；")
		records = append(records, r)
	}
	return records
}
