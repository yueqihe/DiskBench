package main

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

const scanSlowWaitLimit = 5 * time.Second

var errScanSlowBudget = errors.New("本区域严重慢读等待达到上限")

func scanTimeoutPrefix(read readWithRetriesResult, position int64) int {
	if e := scanProtocolError(read.err); e != nil && e.Evidence.RequestOffset != nil {
		return min(read.bytesRead, int(max(0, *e.Evidence.RequestOffset-position)))
	}
	return 0 // Bytes from an unsuccessful command are not proof of readability.
}

// Only severe request waits consume this budget. A large healthy region can
// take longer than the limit without losing coverage. A new region resets it.
// Check only between finished requests; never bypass a pending kernel I/O.
type scanSlowBudgetReader struct {
	ReadHandle
	mu         sync.Mutex
	rotational bool
	limit      time.Duration
	waited     time.Duration
	now        func() time.Time
}

func newScanSlowBudgetReader(handle ReadHandle, rotational bool) *scanSlowBudgetReader {
	return &scanSlowBudgetReader{ReadHandle: handle, rotational: rotational, limit: scanSlowWaitLimit, now: time.Now}
}

func (r *scanSlowBudgetReader) exhausted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.waited >= r.limit
}

func (r *scanSlowBudgetReader) ReadAt(p []byte, offset int64) (int, error) {
	if r.exhausted() {
		return 0, errScanSlowBudget
	}
	return r.read(p, offset)
}

func (r *scanSlowBudgetReader) read(p []byte, offset int64) (int, error) {
	started := r.now()
	n, err := r.ReadHandle.ReadAt(p, offset)
	elapsed := r.now().Sub(started)
	if scanRequestStatus(float64(elapsed)/float64(time.Millisecond), r.rotational) == "severe" {
		r.mu.Lock()
		r.waited += elapsed
		r.mu.Unlock()
	}
	return n, err
}

// Explicit deep localization has its own deadline and request limit. Continue
// charging its waits to the region, so the primary scan skips the remainder
// afterwards if needed. This never bypasses pending-I/O or cancellation checks.
type scanLocalizationReader struct{ *scanSlowBudgetReader }

func (r *scanLocalizationReader) ReadAt(p []byte, offset int64) (int, error) {
	return r.scanSlowBudgetReader.read(p, offset)
}

func slowSkippedRange(offset, length int64) BadRange {
	return BadRange{Offset: offset, Length: length, Code: "SLOW_READ_LIMIT", Outcome: "unresolved", UnresolvedBytes: length,
		Detail: fmt.Sprintf("严重卡读，本区域累计等待达到约 %d 秒或设备读取超时；已跳过此范围并继续后续区域，跳过范围未完成读取检测", int(scanSlowWaitLimit.Seconds()))}
}

func appendSlowSkippedRange(ranges *[]BadRange, offset, length int64, limit int) {
	if length <= 0 {
		return
	}
	if n := len(*ranges); n > 0 {
		last := &(*ranges)[n-1]
		if last.Code == "SLOW_READ_LIMIT" && last.Offset+last.Length == offset {
			last.Length += length
			last.UnresolvedBytes += length
			return
		}
	}
	appendBadRanges(ranges, []BadRange{slowSkippedRange(offset, length)}, limit)
}

// Account for every remaining planned byte, excluding gaps between samples.
// Existing unresolved ranges describe the current transfer; add later ones.
func finishSlowLimitedRegion(result *ScanResult, cell *ScanCell, read *readRegionResult, position, length int64) int64 {
	if !read.slowLimited {
		return length
	}
	end := position + length
	if result.ScanMode == "full" {
		appendSlowSkippedRange(&read.badRanges, end, cell.End-end, -1)
		return cell.End - position
	}
	for _, span := range cell.SampleRanges {
		start := max(end, span.Start)
		if span.End > start {
			appendSlowSkippedRange(&read.badRanges, start, span.End-start, -1)
			length += span.End - start
		}
	}
	return length
}
