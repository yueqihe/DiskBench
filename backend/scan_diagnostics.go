package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

type ScanLogHint struct {
	Source string `json:"source"`
	LBA    int64  `json:"lba"`
	NSID   uint32 `json:"nsid,omitempty"`
}

type ScanEvidence struct {
	NVMeSelfTestSupported *bool            `json:"nvmeSelfTestSupported,omitempty"`
	Available             bool             `json:"available"`
	Status                string           `json:"status"`
	Counters              map[string]int64 `json:"counters"`
	Warnings              []string         `json:"warnings"`
	LogAvailable          bool             `json:"logAvailable"`
	LogHints              []ScanLogHint    `json:"logHints"`
	Note                  string           `json:"note,omitempty"`
}

// Evidence objects are immutable once published in a job snapshot.
type ScanDiagnostics struct {
	NVMe       *NVMeScanInfo       `json:"nvme,omitempty"`
	SelfTest   *NVMeSelfTestResult `json:"selfTest,omitempty"`
	ReadMethod string              `json:"readMethod"`
	ReadNote   string              `json:"readNote,omitempty"`
	Before     *ScanEvidence       `json:"before,omitempty"`
	After      *ScanEvidence       `json:"after,omitempty"`
	Increased  []string            `json:"increased,omitempty"`
}

func parseScanEvidence(output string, disk Disk) ScanEvidence {
	e := ScanEvidence{Status: "unavailable", Counters: map[string]int64{}, Warnings: []string{}, LogHints: []ScanLogHint{}}
	smart, err := parseSmartctlJson(output, disk.LogicalSectorSize)
	if err != nil {
		e.Note = "SMART／错误日志读取失败或设备不支持"
		return e
	}
	e.Available, e.Status = smart.SmartAvailable, smart.SmartStatus
	if smart.SmartStatus == "failed" {
		e.Warnings = append(e.Warnings, "SMART 总体健康检查未通过")
	}
	for _, metric := range []struct {
		key, label string
		value      *int64
	}{
		{"reallocated", "重映射扇区", smart.ReallocatedSectors},
		{"pending", "待处理扇区", smart.PendingSectors},
		{"uncorrectable", "离线无法校正扇区", smart.UncorrectableSectors},
		{"reported", "已报告无法校正错误（厂商原始值）", smart.ReportedUncorrectable},
		{"nvmeMedia", "NVMe 介质／数据完整性错误", smart.NVMeMediaErrors},
		{"critical", "NVMe 严重警告位", smart.CriticalWarning},
	} {
		if metric.value != nil {
			e.Counters[metric.key] = *metric.value
			if *metric.value > 0 {
				e.Warnings = append(e.Warnings, fmt.Sprintf("%s：%d", metric.label, *metric.value))
			}
		}
	}
	// Decode only documented LBA fields. In particular do not mistake ATA
	// command-history addresses or NVMe controller-wide namespace LBAs for
	// a confirmed failing address on this disk.
	var logs struct {
		ATAErrors map[string]struct {
			Count int64 `json:"count"`
			Table []struct {
				Registers struct {
					LBA *int64 `json:"lba"`
				} `json:"completion_registers"`
			} `json:"table"`
		} `json:"ata_smart_error_log"`
		ATATests map[string]struct {
			Table []struct {
				Status struct {
					Passed *bool `json:"passed"`
				} `json:"status"`
				LBA json.RawMessage `json:"lba"`
			} `json:"table"`
		} `json:"ata_smart_self_test_log"`
		NVMeErrors *struct {
			Count int64 `json:"logged_count"`
		} `json:"nvme_error_information_log"`
		SCSIErrors map[string]struct {
			Uncorrected *int64 `json:"total_uncorrected_errors"`
		} `json:"scsi_error_counter_log"`
	}
	if err := json.Unmarshal([]byte(output), &logs); err != nil {
		e.Note = "部分错误日志无法解析"
		return e
	}
	seen := map[string]bool{}
	addHint := func(source string, lba int64) {
		sector := disk.LogicalSectorSize
		if sector <= 0 {
			sector = 512
		}
		if lba < 0 || lba >= disk.Size/sector || len(e.LogHints) >= 64 {
			return
		}
		key := fmt.Sprintf("%s:%d", source, lba)
		if !seen[key] {
			e.LogHints = append(e.LogHints, ScanLogHint{Source: source, LBA: lba})
			seen[key] = true
		}
	}
	for _, name := range []string{"extended", "summary"} {
		if log, ok := logs.ATAErrors[name]; ok {
			e.LogAvailable = true
			if log.Count > e.Counters["ataErrors"] {
				e.Counters["ataErrors"] = log.Count
			}
			for _, entry := range log.Table {
				if entry.Registers.LBA != nil {
					addHint("ATA 错误日志", *entry.Registers.LBA)
				}
			}
		}
	}
	for _, name := range []string{"extended", "standard"} {
		if log, ok := logs.ATATests[name]; ok {
			e.LogAvailable = true
			for _, entry := range log.Table {
				if entry.Status.Passed != nil && !*entry.Status.Passed {
					var lba int64
					if len(entry.LBA) > 0 && string(entry.LBA) != "null" && json.Unmarshal(entry.LBA, &lba) == nil {
						addHint("ATA 自检日志", lba)
					}
				}
			}
		}
	}
	if logs.NVMeErrors != nil {
		e.LogAvailable = true
		e.Counters["nvmeLog"] = logs.NVMeErrors.Count
	}
	for _, operation := range []string{"read", "write", "verify"} {
		if log, ok := logs.SCSIErrors[operation]; ok && log.Uncorrected != nil {
			e.LogAvailable = true
			e.Counters["scsi_"+operation] = *log.Uncorrected
			if *log.Uncorrected > 0 {
				e.Warnings = append(e.Warnings, fmt.Sprintf("SCSI %s 未校正错误：%d", operation, *log.Uncorrected))
			}
		}
	}
	appendNVMeLogEvidence(&e, output, disk)
	if e.Counters["ataErrors"] > 0 || e.Counters["nvmeLog"] > 0 || len(e.LogHints) > 0 {
		e.Warnings = append(e.Warnings, "硬盘日志存在历史错误记录，不能直接视为当前坏道")
	}
	if !e.Available {
		e.Note = "SMART 不可用或仅返回部分信息"
	}
	if !e.LogAvailable {
		e.Note += "；错误／自检日志不可用或设备不支持"
	}
	if logs.NVMeErrors != nil {
		e.Note += "；NVMe 计数可能来自整个控制器，日志位置仅在 NSID 已确认且匹配时显示"
	}
	return e
}

func readScanEvidence(disk Disk, findExe func(string) string) ScanEvidence {
	exe := findExe("smartctl")
	if exe == "" {
		return parseScanEvidence("", disk)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// Read existing logs only. Do not start a device self-test or repair.
	disk.NVMeNamespaceID = nativeNVMeNSID(disk)
	output, _ := exec.CommandContext(ctx, exe, "--xall", "--json", disk.Path).Output()
	if ctx.Err() != nil {
		return parseScanEvidence("", disk)
	}
	return parseScanEvidence(string(output), disk)
}

func (m *JobManager) collectScanEvidence(job *Job, disk Disk, after bool) error {
	evidence, err := awaitCancelable(m, job, func() (ScanEvidence, error) { return m.scanEvidenceFn(disk), nil })
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	result := job.Result.(*ScanResult)
	diagnostics := ScanDiagnostics{ReadMethod: scanReadMethod}
	if result.Diagnostics != nil {
		diagnostics = *result.Diagnostics
	}
	if after {
		diagnostics.After = &evidence
		if diagnostics.Before != nil {
			for key, value := range evidence.Counters {
				if before, ok := diagnostics.Before.Counters[key]; ok && value > before {
					diagnostics.Increased = append(diagnostics.Increased, key)
				}
			}
		}
	} else {
		diagnostics.Before = &evidence
	}
	result.Diagnostics = &diagnostics
	return nil
}
