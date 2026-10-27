package main

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// Host read requests, not pure firmware service time. Capping the request
// avoids treating an entire 8 MiB transfer as one device stall. The kernel
// may split further for device limits; the measured unit stays <= 128 KiB.
const scanLatencyRequestBytes = 128 * 1024

const scanLegacyTransferSevereMS = 1000.0

type ScanReadLatency struct {
	Ms     float64 `json:"ms"`
	Bytes  int     `json:"bytes"`
	Offset int64   `json:"offset"`
}

type scanReadFailure struct {
	Offset int64
	Bytes  int
	Err    error
}

type scanRequestReader struct {
	ReadHandle
	check          func() error
	read           func([]byte, int64) (int, error)
	mu             sync.Mutex // cancellation may return before the pending read does
	worst          *ScanReadLatency
	repairAfterMs  float64
	slowReads      []ScanReadLatency
	recheckAfterMs float64
	suspectReads   []ScanReadLatency
	ioBytes        int64
	failure        *scanReadFailure
}

func (h *scanRequestReader) failureSnapshot() *scanReadFailure {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failure == nil {
		return nil
	}
	copy := *h.failure
	return &copy
}

func (h *scanRequestReader) record(ms float64, size int, offset int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.worst == nil || ms > h.worst.Ms {
		h.worst = &ScanReadLatency{Ms: ms, Bytes: size, Offset: offset}
	}
	if h.repairAfterMs > 0 && ms > h.repairAfterMs {
		h.slowReads = append(h.slowReads, ScanReadLatency{Ms: ms, Bytes: size, Offset: offset})
	}
	if h.recheckAfterMs > 0 && ms >= h.recheckAfterMs && len(h.suspectReads) < fullScanReadSize/scanLatencyRequestBytes {
		h.suspectReads = append(h.suspectReads, ScanReadLatency{Ms: ms, Bytes: size, Offset: offset})
	}
}

func (h *scanRequestReader) recheckSnapshot() ([]ScanReadLatency, int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ScanReadLatency(nil), h.suspectReads...), h.ioBytes
}

func (h *scanRequestReader) slowSnapshot() []ScanReadLatency {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ScanReadLatency(nil), h.slowReads...)
}

func (h *scanRequestReader) snapshot() *ScanReadLatency {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.worst == nil {
		return nil
	}
	v := *h.worst
	return &v
}

func (h *scanRequestReader) ReadAt(p []byte, offset int64) (int, error) {
	completed := 0
	var recovered error
	for completed < len(p) {
		if err := h.check(); err != nil {
			return completed, err
		}
		size := min(scanLatencyRequestBytes, len(p)-completed)
		started := time.Now()
		read := h.read
		if read == nil {
			read = h.ReadHandle.ReadAt
		}
		n, err := read(p[completed:completed+size], offset+int64(completed))
		ms := float64(time.Since(started).Nanoseconds()) / 1e6
		if n < 0 || n > size {
			return completed, io.ErrUnexpectedEOF
		}
		h.mu.Lock()
		h.ioBytes += int64(n)
		h.mu.Unlock()
		if evidence := scanProtocolError(err); evidence != nil && evidence.Evidence.Kind == "RECOVERED_ERROR" && n == size {
			if recovered == nil {
				recovered = err
			}
			h.record(ms, size, offset+int64(completed))
			completed += n
			continue
		}
		if err == nil && n == size {
			h.record(ms, size, offset+int64(completed))
		}
		completed += n
		if err != nil || n != size {
			if err != nil && !errors.Is(err, errScanSlowBudget) && !errors.Is(err, errScanProbeBudget) {
				h.mu.Lock()
				h.failure = &scanReadFailure{Offset: offset + int64(completed-n), Bytes: size, Err: err}
				h.mu.Unlock()
			}
			return completed, err
		}
	}
	return completed, recovered
}

// Project response-warning policy; slow responses alone do not establish bad sectors.
func scanReadThresholds(rotational bool) (slow, slower, severe float64) {
	if rotational {
		return 50, 200, 800
	}
	return 20, 50, 500
}

func scanRequestStatus(ms float64, rotational bool) string {
	slow, slower, severe := scanReadThresholds(rotational)
	switch {
	case ms >= severe:
		return "severe"
	case ms >= slower:
		return "slower"
	case ms >= slow:
		return "slow"
	default:
		return "normal"
	}
}

func scanStatusRank(status string) int {
	return map[string]int{"normal": 0, "slow": 1, "slower": 2, "severe": 3, "error": 4}[status]
}

func scanReadNeedsRecheck(read readRegionResult, rotational bool) bool {
	return read.maxReadLatency != nil && scanRequestStatus(read.maxReadLatency.Ms, rotational) != "normal"
}

func applyScanReadLatency(cell *ScanCell, sample *ScanReadLatency, rotational bool) {
	if sample != nil && (cell.MaxReadLatency == nil || sample.Ms > cell.MaxReadLatency.Ms) {
		v := *sample
		cell.MaxReadLatency = &v
	}
	if cell.ErrorCount > 0 || cell.Repair != nil && cell.Repair.Failed > 0 {
		cell.Status = "error"
		return
	}
	if cell.Repair != nil && cell.Repair.Pending > 0 || scanPersistentSlowBlock(*cell, rotational) != nil {
		cell.Status = "severe"
		return
	}
	status := "normal"
	if cell.MaxUnitMs != nil {
		status = scanRequestStatus(*cell.MaxUnitMs, rotational)
	}
	if cell.MaxReadLatency != nil {
		requestStatus := scanRequestStatus(cell.MaxReadLatency.Ms, rotational)
		if scanStatusRank(requestStatus) > scanStatusRank(status) {
			status = requestStatus
		}
	}
	if scanStatusRank(status) > scanStatusRank(cell.Status) {
		cell.Status = status
	}
}

func scanLatencyBasis(rotational bool) string {
	if rotational {
		return "hdd-dual-v4"
	}
	return "solid-dual-v4"
}

// Writes have separate warning bands: device write-cache and flash-program
// latency differ from reads. These are host response warnings, not defects.
func scanWriteRequestStatus(ms float64, rotational bool) string {
	slow, slower, severe := 50.0, 150.0, 500.0
	if rotational {
		slow, slower, severe = 100, 300, 1000
	}
	switch {
	case ms >= severe:
		return "severe"
	case ms >= slower:
		return "slower"
	case ms >= slow:
		return "slow"
	default:
		return "normal"
	}
}

func applyScanWriteLatency(cell *ScanCell, sample *ScanReadLatency, rotational bool) {
	if sample != nil && (cell.MaxWriteLatency == nil || sample.Ms > cell.MaxWriteLatency.Ms) {
		v := *sample
		cell.MaxWriteLatency = &v
	}
	if cell.ErrorCount > 0 || cell.MaxWriteLatency == nil {
		return
	}
	status := scanWriteRequestStatus(cell.MaxWriteLatency.Ms, rotational)
	if scanStatusRank(status) > scanStatusRank(cell.Status) {
		cell.Status = status
	}
}

func writeLatencyBasis(rotational bool) string {
	if rotational {
		return "hdd-write-dual-v4"
	}
	return "solid-write-dual-v4"
}

func (m *JobManager) updateWriteCell(cell *ScanCell, read writeRegionResult, length int64, rotational bool) {
	m.updateCell(cell, read.milliseconds, int64(read.bytesWritten), int64(read.errorCount), length*2)
	applyScanReadLatency(cell, read.maxReadLatency, rotational)
	applyScanWriteLatency(cell, read.maxWriteLatency, rotational)
}

func writeCompletionMessage(errorCells, slowCells int64) string {
	if errorCells > 0 {
		return fmt.Sprintf("深度检测发现 %d 个写入、读回或校验异常区域", errorCells)
	}
	if slowCells > 0 {
		return fmt.Sprintf("深度检测发现 %d 个响应偏慢区域，未发现写入、读回或校验错误", slowCells)
	}
	return "深度检测未发现写入、读回或校验异常"
}
