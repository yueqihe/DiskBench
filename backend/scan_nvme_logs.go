package main

import (
	"encoding/json"
	"fmt"
)

func appendNVMeLogEvidence(e *ScanEvidence, output string, disk Disk) {
	var data struct {
		Admin *struct {
			SelfTest *bool `json:"self_test"`
		} `json:"nvme_optional_admin_commands"`
		Errors *struct {
			Table []struct {
				NSID *uint32 `json:"nsid"`
				LBA  *struct {
					Value *uint64 `json:"value"`
				} `json:"lba"`
				Status *struct {
					SCT *byte `json:"status_code_type"`
					SC  *byte `json:"status_code"`
				} `json:"status_field"`
			} `json:"table"`
		} `json:"nvme_error_information_log"`
		Tests *struct {
			Table []struct {
				NSID   *int64  `json:"nsid"`
				LBA    *uint64 `json:"lba"`
				Result struct {
					Value byte `json:"value"`
				} `json:"self_test_result"`
			} `json:"table"`
		} `json:"nvme_self_test_log"`
	}
	if json.Unmarshal([]byte(output), &data) != nil {
		return
	}
	if data.Admin != nil {
		e.NVMeSelfTestSupported = data.Admin.SelfTest
	}
	if data.Tests != nil {
		e.LogAvailable = true
	}
	// NVMe logs may be controller-wide. Without a kernel-confirmed NSID we
	// retain health counters but attach no location to the selected disk.
	if disk.NVMeNamespaceID == 0 || disk.NVMeNamespaceID == 0xffffffff || disk.LogicalSectorSize <= 0 || disk.Size <= 0 {
		return
	}
	seen := map[string]bool{}
	add := func(source string, lba uint64) {
		if lba >= uint64(disk.Size/disk.LogicalSectorSize) || len(e.LogHints) >= 64 {
			return
		}
		key := fmt.Sprintf("%s:%d", source, lba)
		if seen[key] {
			return
		}
		seen[key] = true
		e.LogHints = append(e.LogHints, ScanLogHint{Source: source, LBA: int64(lba), NSID: disk.NVMeNamespaceID})
	}
	if data.Errors != nil {
		for _, row := range data.Errors.Table {
			if row.NSID == nil || *row.NSID != disk.NVMeNamespaceID || row.LBA == nil || row.LBA.Value == nil || row.Status == nil || row.Status.SCT == nil || row.Status.SC == nil {
				continue
			}
			if localizableScanIssue(decodeNVMeScanStatus(uint16(*row.Status.SCT)<<8|uint16(*row.Status.SC), disk.NVMeNamespaceID)) {
				add("NVMe 介质／完整性错误日志", *row.LBA.Value)
			}
		}
	}
	if data.Tests != nil {
		for _, row := range data.Tests.Table {
			// smartctl emits NSID/LBA only when the corresponding validity bits
			// are set. Missing fields and broadcast NSID must never become LBA 0.
			if row.NSID == nil || *row.NSID != int64(disk.NVMeNamespaceID) || row.LBA == nil || row.Result.Value < 5 || row.Result.Value > 7 {
				continue
			}
			add("NVMe 固件自检日志", *row.LBA)
		}
	}
}
