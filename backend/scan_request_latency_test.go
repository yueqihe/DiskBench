package main

import (
	"errors"
	"io"
	"testing"
)

func TestRequestLatencyThresholds(t *testing.T) {
	for _, tc := range []struct {
		ms   float64
		hdd  bool
		want string
	}{
		{19.99, false, "normal"}, {20, false, "slow"}, {49.99, false, "slow"},
		{50, false, "slower"}, {99.99, false, "slower"}, {100, false, "severe"}, {172, false, "severe"},
		{49.99, true, "normal"}, {50, true, "slow"}, {149.99, true, "slow"},
		{150, true, "slower"}, {499.99, true, "slower"}, {500, true, "severe"},
	} {
		if got := scanRequestStatus(tc.ms, tc.hdd); got != tc.want {
			t.Errorf("ms=%v hdd=%v: %s, want %s", tc.ms, tc.hdd, got, tc.want)
		}
	}
}

func TestRequestStallSurvivesNormalizationLaterReadsAndRecovery(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	cell := &ScanCell{}
	m.updateCell(cell, 172, 8*1024*1024, 0, 8*1024*1024)
	applyScanReadLatency(cell, &ScanReadLatency{Ms: 172, Bytes: 128 * 1024, Offset: 0}, false)
	if cell.Status != "severe" || *cell.MaxUnitMs != 21.5 {
		t.Fatalf("stall diluted: %+v", cell)
	}
	m.updateCell(cell, 8, 8*1024*1024, 0, 8*1024*1024)
	applyScanReadLatency(cell, &ScanReadLatency{Ms: 1, Bytes: 128 * 1024, Offset: 8 * 1024 * 1024}, false)
	if cell.Status != "severe" || cell.MaxReadLatency.Ms != 172 {
		t.Fatal("later fast read erased stall")
	}
	m.updateCell(cell, 1, 0, 1, 512)
	applyScanReadLatency(cell, nil, false)
	if cell.Status != "error" {
		t.Fatal("timing hid a read error")
	}
	// An 84 ms large transfer made of short healthy requests is not a stall.
	healthy := &ScanCell{}
	m.updateCell(healthy, 84, 8*1024*1024, 0, 8*1024*1024)
	applyScanReadLatency(healthy, &ScanReadLatency{Ms: 2, Bytes: 128 * 1024}, true)
	if healthy.Status != "normal" {
		t.Fatal("normal HDD throughput classified as stall")
	}
	// Neither the absolute policy nor a later transfer may lower HDD evidence.
	applyHDDLatency(healthy, "relative-severe", 12)
	applyScanReadLatency(healthy, nil, true)
	if healthy.Status != "severe" {
		t.Fatal("lost relative evidence")
	}
}

func TestLatencyReaderCoverageTailShortReadAndCancellation(t *testing.T) {
	const length = 8*1024*1024 + 512
	position, calls := int64(4096), 0
	h := &scanRequestReader{check: func() error { return nil }, ReadHandle: scanReaderFunc(func(p []byte, offset int64) (int, error) {
		if len(p) > scanLatencyRequestBytes || offset != position || len(p)%512 != 0 {
			t.Fatalf("bad request: offset=%d size=%d", offset, len(p))
		}
		position += int64(len(p))
		calls++
		return len(p), nil
	})}
	n, err := h.ReadAt(scanBuffer(length), 4096)
	if err != nil || n != length || calls != 65 || position != 4096+length || h.snapshot() == nil {
		t.Fatalf("coverage: n=%d calls=%d position=%d err=%v", n, calls, position, err)
	}
	h.ReadHandle = scanReaderFunc(func(p []byte, off int64) (int, error) { return 512, io.EOF })
	if n, err := h.ReadAt(scanBuffer(4096), 0); n != 512 || !errors.Is(err, io.EOF) {
		t.Fatalf("lost short read: %d %v", n, err)
	}
	h.check = func() error { return errCancelled }
	if n, err := h.ReadAt(scanBuffer(4096), 0); n != 0 || err != errCancelled {
		t.Fatalf("ignored cancellation: %d %v", n, err)
	}
}

func TestRecheckMergesRequestLatencyAndPreservesSlowOutcome(t *testing.T) {
	previous := &ScanRecheck{Outcome: "slow", MaxReadLatency: &ScanReadLatency{Ms: 172, Bytes: 131072}, MaxUnitMs: 21.5}
	next := &ScanRecheck{Outcome: "not_reproduced", MaxReadLatency: &ScanReadLatency{Ms: 1, Bytes: 131072}}
	merged := mergeScanRecheck(previous, next)
	if merged.Outcome != "slow" || merged.MaxReadLatency.Ms != 172 || next.Outcome != "not_reproduced" {
		t.Fatal("merged recheck lost evidence or mutated snapshot")
	}
	merged = mergeScanRecheck(merged, &ScanRecheck{Outcome: "read_issue", ErrorCount: 1})
	if merged.Outcome != "read_issue" {
		t.Fatal("read error was hidden")
	}
}
