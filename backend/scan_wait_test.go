package main

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type blockedScanHandle struct {
	started      chan struct{}
	releaseRead  chan struct{}
	releaseClose chan struct{}
	once         sync.Once
	calls        atomic.Int64
	firstReads   atomic.Int64
}

func (h *blockedScanHandle) ReadAt(p []byte, offset int64) (int, error) {
	h.calls.Add(1)
	if offset == 0 {
		h.firstReads.Add(1)
	}
	h.once.Do(func() { close(h.started) })
	<-h.releaseRead
	return len(p), nil
}
func (h *blockedScanHandle) Close() error               { return nil }
func (h *blockedScanHandle) CloseDone() <-chan struct{} { return h.releaseClose }

func blockedScanManager(t *testing.T, timeout time.Duration) (*JobManager, *Job, *blockedScanHandle) {
	t.Helper()
	h := &blockedScanHandle{started: make(chan struct{}), releaseRead: make(chan struct{}), releaseClose: make(chan struct{})}
	disk := testDisk()
	disk.Size = 4096 * 4096
	m := newTestManager(t, JobManagerOptions{
		ScanReadTimeout: timeout, ResolveDisk: func(string) (*Disk, error) { return disk, nil },
		Open:         func(string) (ReadHandle, error) { return h, nil },
		ScanEvidence: func(Disk) ScanEvidence { return ScanEvidence{} },
	})
	job, err := m.create("scan", disk.Path, map[string]interface{}{"scanMode": "full"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.started:
	case <-time.After(2 * time.Second):
		t.Fatal("read did not start")
	}
	return m, job, h
}

func TestCancelKeepsDeviceOccupiedUntilReadAndCloseReallyFinish(t *testing.T) {
	m, job, h := blockedScanManager(t, time.Second)
	defer func() {
		select {
		case <-h.releaseRead:
		default:
			close(h.releaseRead)
		}
		select {
		case <-h.releaseClose:
		default:
			close(h.releaseClose)
		}
	}()
	returned := make(chan *Job, 1)
	go func() { response, _ := m.cancel(job.ID); returned <- response }()
	select {
	case response := <-returned:
		if response.Status != "cancelling" {
			t.Fatalf("cancel returned %s", response.Status)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel waited on kernel I/O")
	}
	_, err := m.create("scan", "/dev/sda", map[string]interface{}{"scanMode": "full"})
	var conflict *jobError
	if !errors.As(err, &conflict) || conflict.apiCode != "JOB_CONFLICT" {
		t.Fatalf("new job bypassed device occupancy: %v", err)
	}
	close(h.releaseRead)
	if m.get(job.ID).Status != "cancelling" {
		t.Fatal("finished before close completed")
	}
	close(h.releaseClose)
	done := waitForStatus(t, m, job.ID, "cancelled", 2*time.Second)
	if done.Result.(*ScanResult).Stats.UnreadableRanges != 0 {
		t.Fatal("cancellation marked bad sectors")
	}
	if h.calls.Load() != 1 {
		t.Fatalf("continued reading after cancellation: %d", h.calls.Load())
	}
	m.mu.Lock()
	remaining := len(m.handles) + len(m.cancels)
	m.mu.Unlock()
	if remaining != 0 {
		t.Fatal("device occupancy not released")
	}
}

func TestHostReadTimeoutNeverRetriesWhileOriginalReadIsPending(t *testing.T) {
	m, job, h := blockedScanManager(t, 30*time.Millisecond)
	defer func() {
		select {
		case <-h.releaseRead:
		default:
			close(h.releaseRead)
		}
		select {
		case <-h.releaseClose:
		default:
			close(h.releaseClose)
		}
	}()
	close(h.releaseClose)
	deadline := time.Now().Add(2 * time.Second)
	for {
		snapshot := m.get(job.ID)
		result := snapshot.Result.(*ScanResult)
		if snapshot.Phase == "读取超时，等待设备请求结束" {
			if snapshot.Status != "running" || result.Stats.UnreadableRanges != 0 || result.Stats.CoveredBytes != 0 {
				t.Fatalf("wrong pending timeout state: %+v", snapshot)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("host timeout not recorded")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if h.calls.Load() != 1 {
		t.Fatalf("retry overlapped pending I/O: %d", h.calls.Load())
	}
	close(h.releaseRead)
	done := waitForStatus(t, m, job.ID, "completed", 2*time.Second)
	result := done.Result.(*ScanResult)
	if result.Stats.UnreadableBytes != 0 || result.Stats.DeviceErrorRanges != 1 || result.Stats.SkippedBytes != 4096 || result.Stats.CoveredBytes != done.Disk.Size-4096 || result.Map[1].Status == "pending" {
		t.Fatalf("timeout polluted scan results: %+v", result.Stats)
	}
	// Scheduling stalls can trigger a valid recheck of a later healthy cell.
	// Account for those reads, while forbidding a retry of the timed-out LBA.
	wantCalls := int64(4096)
	for _, cell := range result.Map {
		if cell.Recheck != nil {
			wantCalls += cell.Recheck.Reads
		}
	}
	if h.firstReads.Load() != 1 || h.calls.Load() != wantCalls {
		t.Fatalf("unexpected reads after the timed-out request settled: first LBA=%d, total=%d, want=%d", h.firstReads.Load(), h.calls.Load(), wantCalls)
	}
}

func TestCancelWhileWaitingForTimedOutReadDoesNotContinueToNextRegion(t *testing.T) {
	m, job, h := blockedScanManager(t, 20*time.Millisecond)
	defer func() {
		select {
		case <-h.releaseRead:
		default:
			close(h.releaseRead)
		}
		select {
		case <-h.releaseClose:
		default:
			close(h.releaseClose)
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for m.get(job.ID).Phase != "读取超时，等待设备请求结束" {
		if time.Now().After(deadline) {
			t.Fatal("host timeout did not become visible")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if _, err := m.cancel(job.ID); err != nil {
		t.Fatal(err)
	}
	if h.calls.Load() != 1 || m.get(job.ID).Status != "cancelling" {
		t.Fatal("cancellation lost device occupancy while the request was pending")
	}
	close(h.releaseRead)
	close(h.releaseClose)
	done := waitForStatus(t, m, job.ID, "cancelled", 2*time.Second)
	if h.calls.Load() != 1 || done.Result.(*ScanResult).Stats.UnreadableBytes != 0 {
		t.Fatal("continued after cancellation or invented unreadable sectors")
	}
}
