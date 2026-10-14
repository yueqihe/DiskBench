package main

import (
	"bytes"
	"errors"
	"fmt"
	"time"
)

type nvmeSelfTester interface {
	nvmeInfo() NVMeScanInfo
	selfTestLog() ([]byte, error)
	startSelfTest() error
	finishSelfTest()
}

func (m *JobManager) publishNVMeSelfTest(job *Job, r NVMeSelfTestResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := job.Result.(*ScanResult)
	d := ScanDiagnostics{}
	if result.Diagnostics != nil {
		d = *result.Diagnostics
	}
	d.SelfTest = &r
	result.Diagnostics = &d
	job.Phase = fmt.Sprintf("NVMe 固件扩展自检 %d%%", r.Percent)
}

func (m *JobManager) runNVMeSelfTest(job *Job, handle ReadHandle) (runErr error) {
	defer func() {
		if runErr == nil {
			return
		}
		m.mu.Lock()
		d := job.Result.(*ScanResult).Diagnostics
		var stopped *NVMeSelfTestResult
		if d != nil && d.SelfTest != nil && d.SelfTest.State == "running" {
			v := *d.SelfTest
			stopped = &v
		}
		m.mu.Unlock()
		if stopped != nil {
			stopped.State = "unknown"
			if errors.Is(runErr, errCancelled) {
				stopped.State = "cancelled"
			}
			stopped.Message = "任务已停止，将尝试中止本次固件自检；设备最终状态需重新读取日志确认"
			m.publishNVMeSelfTest(job, *stopped)
		}
	}()
	h, ok := handle.(nvmeSelfTester)
	if !ok || !h.nvmeInfo().SelfTestSupported {
		return errors.New("所选设备不支持或无法访问 NVMe 固件自检，请取消该选项后扫描")
	}
	info := h.nvmeInfo()
	if job.Disk.LogicalSectorSize <= 0 {
		return errors.New("缺少有效逻辑扇区大小，无法核对自检 LBA")
	}
	before, err := awaitCancelable(m, job, h.selfTestLog)
	if err != nil {
		return err
	}
	if len(before) < 564 {
		return errors.New("无法核对 NVMe 固件自检状态")
	}
	if before[0]&0xf != 0 {
		return errors.New("控制器已有固件自检正在运行，请等待其完成；本次未启动或中止其他自检")
	}
	if _, err = awaitCancelable(m, job, func() (struct{}, error) { return struct{}{}, h.startSelfTest() }); err != nil {
		return err
	}
	m.publishNVMeSelfTest(job, NVMeSelfTestResult{State: "running", NSID: info.NSID, Message: "正在执行用户选择的 NVMe 固件扩展自检"})
	// EDSTT is advisory. Bound waiting even if firmware never completes.
	limit := max(30*time.Minute, time.Duration(info.ExtendedMinutes)*2*time.Minute+5*time.Minute)
	limit = min(limit, 48*time.Hour)
	deadline := time.Now().Add(limit)
	sawRunning := false
	for polls := 0; time.Now().Before(deadline); polls++ {
		if _, err = awaitCancelable(m, job, func() (struct{}, error) { m.delayFn(2 * time.Second); return struct{}{}, nil }); err != nil {
			return err
		}
		data, readErr := awaitCancelable(m, job, h.selfTestLog)
		if readErr != nil {
			return readErr
		}
		r, parseErr := parseNVMeSelfTestLog(data, info.NSID, job.Disk.Size/job.Disk.LogicalSectorSize)
		if parseErr != nil {
			return parseErr
		}
		if op := data[0] & 0xf; op != 0 && op != 2 {
			h.finishSelfTest() // Do not abort a different test started externally.
			r.State, r.Message = "unknown", "设备正在执行另一种自检，无法确认本次扩展自检结果；未中止该自检"
			m.publishNVMeSelfTest(job, r)
			return errors.New(r.Message)
		}
		if r.State == "running" {
			sawRunning = true
			m.publishNVMeSelfTest(job, r)
			m.persistThrottled(job, false)
			continue
		}
		// A previous successful test must not be mistaken for this request.
		if !sawRunning && bytes.Equal(before[4:32], data[4:32]) {
			if polls < 15 {
				continue
			}
			r.State, r.Message = "unknown", "固件未提供可确认属于本次自检的新结果"
		}
		if data[4]>>4 != 2 {
			r.State, r.Message = "unknown", "固件日志最新记录不是本次扩展自检"
		}
		h.finishSelfTest()
		m.publishNVMeSelfTest(job, r)
		if r.State == "passed" || (r.State == "failed" && r.Result >= 5 && r.Result <= 7) {
			return nil
		}
		return errors.New(r.Message)
	}
	return errors.New("等待 NVMe 固件自检超时，正在尝试中止本次自检")
}
