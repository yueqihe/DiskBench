package main

import "strings"

func (m *JobManager) jobBusyLocked(job *Job) bool {
	return activeJobStatuses[job.Status] || m.handles[job.ID] != nil ||
		m.handles[job.ID+"/repair"] != nil || m.cancels[job.ID] != nil || m.processes[job.ID] != nil
}

func (m *JobManager) jobAdmissionErrorLocked(candidate *Job) error {
	if m.stopped {
		return newJobError("应用服务已停止，无法创建任务", "SERVICE_STOPPED", 503)
	}
	for _, current := range m.jobs {
		if !m.jobBusyLocked(current) {
			continue
		}
		// Speed measurements retain exclusive access to avoid interference from
		// another task. Scan and repair work on different physical disks may overlap.
		if candidate.Type != "scan" || current.Type != "scan" {
			return newJobError("测速与其他检测任务不能同时运行，请等待任务结束", "JOB_CONFLICT", 409)
		}
		samePath := candidate.Device == current.Device
		sameSerial := strings.TrimSpace(candidate.Disk.Serial) != "" &&
			strings.TrimSpace(candidate.Disk.Serial) == strings.TrimSpace(current.Disk.Serial)
		if samePath || sameSerial {
			return newJobError("这块硬盘已有任务运行或正在停止，请等待任务结束", "JOB_CONFLICT", 409)
		}
	}
	return nil
}
