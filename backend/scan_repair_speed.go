package main

// Successful write verification alone makes no speed-improvement claim.
// Unknown/busy load and a one-off fast read remain unconfirmed.
func repairSpeedOutcome(first, repeat float64, after []float64, threshold float64, beforeLoad, afterLoad *ScanLoad) string {
	if len(after) != 2 || afterLoad == nil || afterLoad.BackgroundActivity {
		return "unconfirmed"
	}
	if after[0] >= threshold && after[1] >= threshold {
		return "still_slow"
	}
	if after[0] < threshold && after[1] < threshold && first >= threshold && repeat >= threshold && beforeLoad != nil && !beforeLoad.BackgroundActivity {
		return "improved"
	}
	return "unconfirmed"
}

// Describe the recorded reason, rather than listing every possible cause.
// Missing legacy measurements are not evidence that a retest was skipped.
func repairSpeedNote(repair *ScanRepairRecord, threshold float64) string {
	switch repair.SpeedOutcome {
	case "improved":
		return "慢速改善"
	case "still_slow":
		return "复测仍慢，建议更换硬盘"
	}
	if len(repair.SpeedChecks) == 0 {
		return "复测未记录"
	}
	if len(repair.SpeedChecks) != 2 {
		return "复测未完成"
	}
	if repair.SpeedLoad == nil {
		return "复测负载未知"
	}
	if repair.SpeedLoad.BackgroundActivity {
		return "复测有其他磁盘活动"
	}
	if threshold <= 0 {
		return "改善未确认"
	}
	firstFast, repeatFast := repair.SpeedChecks[0] < threshold, repair.SpeedChecks[1] < threshold
	if firstFast != repeatFast {
		return "复测波动"
	}
	if firstFast && repeatFast {
		if repair.CurrentBeforeMs != nil && *repair.CurrentBeforeMs < threshold || repair.BeforeRepeatMs != nil && *repair.BeforeRepeatMs < threshold {
			return "修复前已恢复"
		}
		if repair.BeforeLoad == nil {
			return "修复前负载未知"
		}
		if repair.BeforeLoad.BackgroundActivity {
			return "修复前有其他磁盘活动"
		}
	}
	return "改善未确认"
}
