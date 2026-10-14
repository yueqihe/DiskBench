package main

import (
	"sort"
	"unsafe"
)

// GC-owned aligned memory is safe when a cancelled blocking read outlives its
// caller. Unlike an mmap buffer, it is not unmapped while the kernel uses it.
func scanBuffer(size int) []byte {
	const alignment = 4096
	backing := make([]byte, size+alignment)
	offset := int((alignment - uintptr(unsafe.Pointer(&backing[0]))%alignment) % alignment)
	return backing[offset : offset+size]
}

type ScanRecheck struct {
	Load           *ScanLoad `json:"load,omitempty"`
	ioBytes        int64
	MaxReadLatency *ScanReadLatency `json:"maxReadLatency,omitempty"`
	Outcome        string           `json:"outcome"`
	Reads          int64            `json:"reads"`
	BytesRead      int64            `json:"bytesRead"`
	MaxUnitMs      float64          `json:"maxUnitMs"`
	MaxMs          float64          `json:"maxMs"`
	ErrorCount     int64            `json:"errorCount"`
}

func (m *JobManager) recheckSlowRead(handle ReadHandle, position int64, length int, job *Job, detailLimit int, probeSize int64) (*ScanRecheck, []BadRange, error) {
	recheck := &ScanRecheck{Outcome: "not_reproduced"}
	buffer := scanBuffer(fastSampleSize)
	var ranges []BadRange
	for offset := int64(0); offset < int64(length); {
		size := min(int64(fastSampleSize), int64(length)-offset)
		read, err := m.readRegion(handle, buffer, int(size), position+offset, job, detailLimit-len(ranges), probeSize)
		recheck.ioBytes += read.ioBytes
		recheck.Reads++
		recheck.BytesRead += int64(read.bytesRead)
		recheck.ErrorCount += int64(read.errorCount)
		recheck.MaxUnitMs = max(recheck.MaxUnitMs, scanUnitLatency(read.milliseconds, size))
		recheck.MaxMs = max(recheck.MaxMs, read.milliseconds)
		if read.maxReadLatency != nil && (recheck.MaxReadLatency == nil || read.maxReadLatency.Ms > recheck.MaxReadLatency.Ms) {
			recheck.MaxReadLatency = read.maxReadLatency
		}
		if scanReadNeedsRecheck(read, job.Disk.Rotational) {
			recheck.Outcome = "slow"
		}
		appendBadRanges(&ranges, read.badRanges, detailLimit)
		if err != nil {
			recheck.Outcome = "read_issue"
			return recheck, ranges, err
		}
		offset += size
	}
	if recheck.MaxUnitMs >= 50 {
		recheck.Outcome = "slow"
	}
	if recheck.ErrorCount > 0 {
		recheck.Outcome = "read_issue"
	}
	return recheck, ranges, nil
}

// Confirm the slow requests plus one neighboring request on either side.
// Every byte in the primary span was already read; the gaps need no reread.
func (m *JobManager) recheckSlowRequests(handle ReadHandle, position int64, length int, job *Job, limit int, probeSize int64, first readRegionResult) (*ScanRecheck, []BadRange, error) {
	samples := first.suspectReads
	if len(samples) == 0 && first.maxReadLatency != nil {
		samples = []ScanReadLatency{*first.maxReadLatency}
	}
	if len(samples) == 0 {
		return m.recheckSlowRead(handle, position, min(length, scanLatencyRequestBytes), job, limit, probeSize)
	}
	spans := make([]ScanSampleRange, 0, len(samples))
	for _, sample := range samples {
		start := max(position, sample.Offset-scanLatencyRequestBytes)
		end := min(position+int64(length), sample.Offset+int64(sample.Bytes)+scanLatencyRequestBytes)
		if start < end {
			spans = append(spans, ScanSampleRange{Start: start, End: end})
		}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].Start < spans[j].Start })
	var combined *ScanRecheck
	var ranges []BadRange
	for i := 0; i < len(spans); {
		span := spans[i]
		i++
		for i < len(spans) && spans[i].Start <= span.End {
			span.End = max(span.End, spans[i].End)
			i++
		}
		r, issues, err := m.recheckSlowRead(handle, span.Start, int(span.End-span.Start), job, limit-len(ranges), probeSize)
		combined = mergeScanRecheck(combined, r)
		appendBadRanges(&ranges, issues, limit)
		if err != nil {
			return combined, ranges, err
		}
	}
	return combined, ranges, nil
}

// Replace rather than mutate: API snapshots may share the previous pointer.
func mergeScanRecheck(previous, next *ScanRecheck) *ScanRecheck {
	if previous == nil {
		return next
	}
	combined := *next
	combined.Reads += previous.Reads
	combined.BytesRead += previous.BytesRead
	combined.ErrorCount += previous.ErrorCount
	combined.ioBytes += previous.ioBytes
	if combined.Load == nil {
		combined.Load = previous.Load
	}
	combined.MaxUnitMs = max(previous.MaxUnitMs, next.MaxUnitMs)
	combined.MaxMs = max(previous.MaxMs, next.MaxMs)
	if previous.MaxReadLatency != nil && (combined.MaxReadLatency == nil || previous.MaxReadLatency.Ms > combined.MaxReadLatency.Ms) {
		combined.MaxReadLatency = previous.MaxReadLatency
	}
	if combined.MaxUnitMs >= 50 || previous.Outcome == "slow" || next.Outcome == "slow" {
		combined.Outcome = "slow"
	}
	if combined.ErrorCount > 0 {
		combined.Outcome = "read_issue"
	}
	return &combined
}
