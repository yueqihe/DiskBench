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
	var conflict string
	for _, current := range m.jobs {
		if !m.jobBusyLocked(current) {
			continue
		}
		if candidate.Type == "speed" && current.Type == "speed" {
			return newJobError("一次只能运行一个测速任务，请等待当前测速结束", "JOB_CONFLICT", 409)
		}
		if conflict != "" {
			continue
		}
		switch {
		case candidate.Type == "scan" && current.Type == "scan":
			if sameTaskDisk(candidate.Device, candidate.Disk.Serial, current.Device, current.Disk.Serial) {
				conflict = "这块硬盘已有任务运行或正在停止，请等待任务结束"
			}
		case candidate.Type == "speed" && current.Type == "scan":
			conflict = speedScanConflict(candidate, current)
		case candidate.Type == "scan" && current.Type == "speed":
			conflict = speedScanConflict(current, candidate)
		default:
			conflict = "请等待其他检测任务结束"
		}
	}
	if conflict != "" {
		return newJobError(conflict, "JOB_CONFLICT", 409)
	}
	return nil
}

func sameTaskDisk(firstPath, firstSerial, secondPath, secondSerial string) bool {
	firstSerial = strings.TrimSpace(firstSerial)
	return (firstPath != "" && firstPath == secondPath) ||
		(firstSerial != "" && firstSerial == strings.TrimSpace(secondSerial))
}

func speedScanConflict(speed, scan *Job) string {
	const overlap = "测速目标与坏道扫描使用同一块硬盘，请等待任务结束"
	const unknown = "无法确认测速目标的物理硬盘，暂不能与坏道扫描同时运行"
	if speed.StorageSpace == nil {
		if !safeDevicePattern.MatchString(speed.Device) {
			return unknown
		}
		if sameTaskDisk(speed.Device, speed.Disk.Serial, scan.Device, scan.Disk.Serial) {
			return overlap
		}
		return ""
	}
	if len(speed.StorageSpace.Members) == 0 {
		return unknown
	}
	for _, member := range speed.StorageSpace.Members {
		if !safeDevicePattern.MatchString(member) {
			return unknown
		}
		if sameTaskDisk(member, "", scan.Device, scan.Disk.Serial) {
			return overlap
		}
	}
	for _, member := range speed.StorageSpace.MemberDisks {
		if sameTaskDisk("", member.Serial, scan.Device, scan.Disk.Serial) {
			return overlap
		}
	}
	return ""
}
