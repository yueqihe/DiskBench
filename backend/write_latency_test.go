package main

import (
	"errors"
	"io"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

type deepTestHandle struct {
	read, write func([]byte, int64) (int, error)
}

func (h *deepTestHandle) ReadAt(p []byte, off int64) (int, error)  { return h.read(p, off) }
func (h *deepTestHandle) WriteAt(p []byte, off int64) (int, error) { return h.write(p, off) }
func (h *deepTestHandle) Close() error                             { return nil }

func TestDeepLatencyUsesIndependentWriteAndReadBands(t *testing.T) {
	for _, tc := range []struct {
		ms   float64
		hdd  bool
		want string
	}{
		{49.99, false, "normal"}, {50, false, "slow"}, {149.99, false, "slow"},
		{150, false, "slower"}, {499.99, false, "slower"}, {500, false, "severe"},
		{99.99, true, "normal"}, {100, true, "slow"}, {299.99, true, "slow"},
		{300, true, "slower"}, {999.99, true, "slower"}, {1000, true, "severe"},
	} {
		if got := scanWriteRequestStatus(tc.ms, tc.hdd); got != tc.want {
			t.Errorf("write %v hdd=%v: got %s want %s", tc.ms, tc.hdd, got, tc.want)
		}
	}
	m := newTestManager(t, JobManagerOptions{})
	for _, tc := range []struct {
		read, write float64
		want        string
	}{
		{60, 60, "slower"}, // Sum is 120, but neither read nor write is severe.
		{172, 1, "severe"}, {1, 172, "slower"}, {1, 600, "severe"},
	} {
		cell := &ScanCell{}
		r := writeRegionResult{bytesWritten: 8 * 1024 * 1024, milliseconds: tc.read + tc.write,
			maxReadLatency:  &ScanReadLatency{Ms: tc.read, Bytes: 131072},
			maxWriteLatency: &ScanReadLatency{Ms: tc.write, Bytes: 131072}}
		m.updateWriteCell(cell, r, 8*1024*1024, false)
		if cell.Status != tc.want {
			t.Fatalf("read=%v write=%v: %+v", tc.read, tc.write, cell)
		}
		m.updateWriteCell(cell, writeRegionResult{bytesWritten: 131072, milliseconds: 1}, 131072, false)
		if cell.Status != tc.want {
			t.Fatal("later fast transfer erased evidence")
		}
		m.updateWriteCell(cell, writeRegionResult{errorCount: 1, milliseconds: 1}, 512, false)
		if cell.Status != "error" {
			t.Fatal("latency hid an error")
		}
	}
}

func TestDeepTimedIOCoversTailAndHandlesAlignedShortTransfers(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := &Job{Status: "running"}
	const length = 2*scanLatencyRequestBytes + 512
	data := make([]byte, length)
	writeAt, readAt, writes, reads := int64(0), int64(0), 0, 0
	check := func(p []byte, off, expected int64) {
		if len(p) > scanLatencyRequestBytes || uintptr(unsafe.Pointer(&p[0]))%4096 != 0 || off != expected || off+int64(len(p)) > length {
			t.Fatalf("bad request: off=%d expected=%d len=%d", off, expected, len(p))
		}
	}
	h := &deepTestHandle{
		write: func(p []byte, off int64) (int, error) {
			check(p, off, writeAt)
			writes++
			n := len(p)
			if writes == 1 {
				n = 512
			}
			copy(data[off:], p[:n])
			writeAt += int64(n)
			return n, nil
		},
		read: func(p []byte, off int64) (int, error) {
			check(p, off, readAt)
			reads++
			n := len(p)
			if reads == 1 {
				n = 512
			}
			copy(p[:n], data[off:])
			readAt += int64(n)
			return n, nil
		},
	}
	r, err := m.writeVerifyRegion(h, writePatternB, scanBuffer(length), length, 0, job, 200)
	if err != nil || r.errorCount != 0 || r.bytesWritten != length || writeAt != length || readAt != length || writes < 3 || reads < 3 || r.maxReadLatency == nil || r.maxWriteLatency == nil {
		t.Fatalf("incomplete timed roundtrip: %+v %v", r, err)
	}
}

func TestDeepVerificationDoesNotDependOnDetailsCapacity(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := &Job{Status: "running"}
	const length = writeVerifyPageSize + 512
	for _, limit := range []int{0, 1, 200} {
		h := &corruptingHandle{data: make([]byte, length), corruptOffset: length - 1}
		r, err := m.writeVerifyRegion(h, writePatternA, scanBuffer(length), length, 0, job, limit)
		if err != nil || r.errorCount != 1 || len(r.badRanges) != min(limit, 1) {
			t.Fatalf("tail corruption missed at detail cap %d: %+v %v", limit, r, err)
		}
		if limit > 0 && (r.badRanges[0].Offset != writeVerifyPageSize || r.badRanges[0].Length != 512) {
			t.Fatal("incorrect tail range")
		}
	}
	bad := make([]byte, 205*writeVerifyPageSize+512)
	ranges, count := inspectPatternMismatches(bad, writePatternA, 0, 200)
	if count != 206 || len(ranges) != 200 {
		t.Fatal("detail cap suppressed verification")
	}
}

func TestDeepIOErrorsRemainErrorsWithFullDetailList(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := &Job{Status: "running"}
	for _, readFailure := range []bool{false, true} {
		for _, limit := range []int{0, 200} {
			h := &deepTestHandle{
				write: func(p []byte, off int64) (int, error) {
					if !readFailure {
						return 0, syscall.EIO
					}
					return len(p), nil
				},
				read: func(p []byte, off int64) (int, error) { return 0, syscall.EIO },
			}
			r, err := m.writeVerifyRegion(h, writePatternA, scanBuffer(512), 512, 0, job, limit)
			if err != nil || r.errorCount != 1 || len(r.badRanges) != min(limit, 1) {
				t.Fatalf("lost media error: %+v %v", r, err)
			}
			if limit > 0 {
				want := "WRITE_ERROR"
				if readFailure {
					want = "READ_AFTER_WRITE_ERROR"
				}
				if r.badRanges[0].Code != want {
					t.Fatal("incorrect error phase")
				}
			}
		}
	}
	h := &deepTestHandle{write: func([]byte, int64) (int, error) { return 0, io.ErrShortWrite }}
	r, err := m.writeVerifyRegion(h, writePatternA, scanBuffer(512), 512, 0, job, 200)
	if !errors.Is(err, io.ErrShortWrite) || r.errorCount != 1 || r.badRanges[0].Code != "WRITE_ERROR" {
		t.Fatal("fatal write lost evidence")
	}
	if !strings.Contains(writeCompletionMessage(0, 2), "响应偏慢") {
		t.Fatal("slow-only test reported clean")
	}
}

func TestDeepCancellationDoesNotReusePendingWriteBuffer(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := &Job{ID: "deep-cancel", Status: "running"}
	cancel := m.beginCancellation(job)
	defer m.endCancellation(job, cancel)
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan bool, 1)
	h := &deepTestHandle{write: func(p []byte, off int64) (int, error) {
		close(entered)
		<-release
		valid := true
		for _, b := range p {
			if b != writePatternA {
				valid = false
			}
		}
		finished <- valid
		return len(p), nil
	}}
	done := make(chan error, 1)
	go func() {
		_, err := m.writeVerifyRegion(h, writePatternA, scanBuffer(4096), 4096, 0, job, 200)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("write not started")
	}
	cancel.cancel()
	select {
	case err := <-done:
		if err != errCancelled {
			t.Errorf("cancel=%v", err)
		}
	case <-time.After(time.Second):
		t.Error("cancellation blocked")
	}
	close(release)
	select {
	case valid := <-finished:
		if !valid {
			t.Fatal("pending buffer mutated")
		}
	case <-time.After(time.Second):
		t.Fatal("write did not finish")
	}
}
