package main

import (
	"errors"
	"fmt"
	"time"
)

var errScanWaitTimeout = errors.New("主机读取等待超时")

// A timed-out host wait leaves its kernel read alive. Wait for that request
// before skipping the region, so continuing never queues I/O on top of it.
func (m *JobManager) waitPendingScanRead(job *Job) error {
	m.mu.Lock()
	cancel := m.cancels[job.ID]
	job.Phase = "读取超时，等待设备请求结束"
	job.Message = "硬盘读取超时，等待当前设备请求结束后跳过该区域，继续后续扫描"
	m.appendLog(job, job.Message)
	m.mu.Unlock()
	if cancel == nil {
		return nil
	}
	m.persistThrottled(job, true)
	done := make(chan struct{})
	go func() { cancel.pending.Wait(); close(done) }()
	select {
	case <-done:
		return m.checkCancelled(job)
	case <-cancel.ctx.Done():
		return errCancelled
	}
}

// A host deadline stops submission, not a kernel operation already in progress.
// The cancellation group's pending count keeps the device occupied until it exits.
func (m *JobManager) readScanRequest(job *Job, handle ReadHandle, p []byte, offset int64) (int, error) {
	return waitScanOperation(m, job, "读取", offset, len(p), func() (int, error) { return handle.ReadAt(p, offset) })
}

func waitScanOperation[T any](m *JobManager, job *Job, operation string, offset int64, size int, fn func() (T, error)) (T, error) {
	var zero T
	m.mu.Lock()
	cancel := m.cancels[job.ID]
	m.mu.Unlock()
	if cancel == nil {
		return fn()
	}
	select {
	case <-cancel.ctx.Done():
		return zero, errCancelled
	default:
	}
	type outcome struct {
		n   T
		err error
	}
	ch := make(chan outcome, 1)
	cancel.pending.Add(1)
	go func() {
		defer cancel.pending.Done()
		n, err := fn()
		ch <- outcome{n, err}
	}()
	timer := time.NewTimer(m.scanReadTimeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		return r.n, r.err
	case <-cancel.ctx.Done():
		return zero, errCancelled
	case <-timer.C:
		return zero, fmt.Errorf("%s等待超过 %s，已停止提交新请求：%w: %w", operation, m.scanReadTimeout,
			errScanWaitTimeout, &scanIOError{Evidence: ScanIOEvidence{
				Kind: "TIMEOUT", RequestOffset: intPtr(offset), RequestBytes: int64(size),
			}})
	}
}
