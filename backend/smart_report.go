package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

type SmartReport struct {
	Device     string `json:"device"`
	Model      string `json:"model"`
	Serial     string `json:"serial"`
	CapturedAt string `json:"capturedAt"`
	Output     string `json:"output"`
	Warning    string `json:"warning,omitempty"`
	Mock       bool   `json:"mock"`
}

func execSmartReport(exe string, args []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, exe, args...).CombinedOutput()
	if ctx.Err() != nil {
		return string(output), ctx.Err()
	}
	return string(output), err
}

func readSmartReport(disk Disk, findExe func(string) string, execute smartExecutor) (SmartReport, error) {
	if !safeDevicePattern.MatchString(disk.Path) {
		return SmartReport{}, newJobError("设备路径无效", "INVALID_ARGUMENT", 400)
	}
	exe := findExe("smartctl")
	if exe == "" {
		return SmartReport{}, newJobError("系统未安装 smartctl，无法读取原始 SMART 信息", "SMART_UNAVAILABLE", 503)
	}
	if execute == nil {
		execute = execSmartReport
	}
	// Read existing information/logs only; respect standby without waking a disk.
	output, err := execute(exe, []string{"--xall", "--nocheck=standby", disk.Path})
	if strings.TrimSpace(output) == "" {
		if errors.Is(err, context.DeadlineExceeded) {
			return SmartReport{}, newJobError("读取 SMART 信息超时，请稍后重试", "SMART_TIMEOUT", 504)
		}
		return SmartReport{}, newJobError("未能读取 SMART 输出，请检查设备连接与读取权限", "SMART_READ_FAILED", 502)
	}
	report := SmartReport{Device: disk.Path, Model: disk.Model, Serial: disk.Serial, CapturedAt: isoTime(time.Now()), Output: output}
	// Nonzero smartctl health bits still carry useful original device evidence.
	if errors.Is(err, context.DeadlineExceeded) {
		report.Warning = "读取超时，以下仅为设备已返回的部分输出。"
	}
	return report, nil
}

func mockSmartReport(disk Disk) SmartReport {
	value := func(n *int64) string {
		if n == nil {
			return "-"
		}
		return fmt.Sprint(*n)
	}
	firmware := "-"
	if disk.Firmware != nil {
		firmware = *disk.Firmware
	}
	health := "UNKNOWN"
	if disk.SmartStatus == "passed" {
		health = "PASSED"
	} else if disk.SmartStatus == "failed" {
		health = "FAILED"
	}
	var out strings.Builder
	fmt.Fprintf(&out, "本地演示数据（未查询真实硬盘）\n\n=== START OF INFORMATION SECTION ===\nModel:            %s\nSerial Number:    %s\nFirmware Version: %s\nDevice:           %s\nUser Capacity:    %d bytes\nLogical Sector:   %d bytes\n\n=== START OF SMART DATA SECTION ===\nSMART overall-health self-assessment test result: %s\n", disk.Model, disk.Serial, firmware, disk.Path, disk.Size, disk.LogicalSectorSize, health)
	if disk.TemperatureC != nil {
		fmt.Fprintf(&out, "Temperature:      %.0f Celsius\n", *disk.TemperatureC)
	}
	if disk.PowerOnHours != nil {
		fmt.Fprintf(&out, "Power On Hours:   %.0f\n", *disk.PowerOnHours)
	}
	if disk.PowerCycles != nil {
		fmt.Fprintf(&out, "Power Cycles:     %.0f\n", *disk.PowerCycles)
	}
	if strings.EqualFold(disk.Transport, "nvme") {
		fmt.Fprintf(&out, "\nSMART/Health Information (NVMe Log 0x02)\nCritical Warning: %s\nMedia and Data Integrity Errors: %s\n", value(disk.CriticalWarning), value(disk.NVMeMediaErrors))
		if disk.PercentageUsed != nil {
			fmt.Fprintf(&out, "Percentage Used:  %.0f%%\n", *disk.PercentageUsed)
		}
	} else {
		fmt.Fprintf(&out, "\nID# ATTRIBUTE_NAME            RAW_VALUE\n  5 Reallocated_Sector_Ct     %s\n187 Reported_Uncorrect        %s\n197 Current_Pending_Sector    %s\n198 Offline_Uncorrectable     %s\n", value(disk.ReallocatedSectors), value(disk.ReportedUncorrectable), value(disk.PendingSectors), value(disk.UncorrectableSectors))
	}
	return SmartReport{Device: disk.Path, Model: disk.Model, Serial: disk.Serial, CapturedAt: isoTime(time.Now()), Output: out.String(), Mock: true}
}
