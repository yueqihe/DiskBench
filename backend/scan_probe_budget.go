package main

import (
	"errors"
	"time"
)

var errScanProbeBudget = errors.New("异常范围定位达到限额")

type scanProbeReader struct {
	ReadHandle
	deadline  time.Time
	remaining int
}

func (r *scanProbeReader) ReadAt(p []byte, offset int64) (int, error) {
	// Never issue a replacement while an earlier kernel request is pending.
	// The normal read timeout and cancellation bookkeeping still own that I/O.
	if r.remaining <= 0 || !time.Now().Before(r.deadline) {
		return 0, errScanProbeBudget
	}
	r.remaining--
	return r.ReadHandle.ReadAt(p, offset)
}

func unresolvedProbeRange(offset, length int64) BadRange {
	return BadRange{Offset: offset, Length: length, Code: "LOCALIZATION_LIMIT", Outcome: "unresolved", UnresolvedBytes: length}
}

func appendUnresolvedProbeRange(ranges *[]BadRange, offset, length int64, evidence *ScanIOEvidence, limit int) {
	r := unresolvedProbeRange(offset, length)
	r.Evidence = evidence
	if n := len(*ranges); n > 0 {
		last := &(*ranges)[n-1]
		if last.Code == r.Code && last.Outcome == r.Outcome && last.Offset+last.Length == offset && last.Evidence == evidence {
			last.Length += length
			last.UnresolvedBytes += length
			return
		}
	}
	appendBadRanges(ranges, []BadRange{r}, limit)
}

func sumUnresolvedProbeBytes(ranges []BadRange) int64 {
	var total int64
	for _, r := range ranges {
		if (r.Code == "LOCALIZATION_LIMIT" || r.Code == "SLOW_READ_LIMIT") && r.Outcome == "unresolved" {
			total += r.UnresolvedBytes
		}
	}
	return total
}
