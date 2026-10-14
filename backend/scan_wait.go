package main

import (
	"errors"
	"fmt"
	"time"
)

var errScanWaitTimeout = errors.New("主机读取等待超时")

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
