package main

import "sort"

// A rolling reference uses like-sized commands from this scan only. Tails
// cannot distort the 8 MiB baseline; a bounded window follows HDD zone speed.
type scanLatencyReference map[int64][]float64

func (b scanLatencyReference) observe(ms float64, size int64, failed bool) (string, float64) {
	if failed || size <= 0 {
		return "", 0
	}
	samples := b[size]
	ratio := 0.0
	warning := ""
	if len(samples) >= 16 {
		sorted := append([]float64(nil), samples...)
		sort.Float64s(sorted)
		median := (sorted[(len(sorted)-1)/2] + sorted[len(sorted)/2]) / 2
		// Sub-millisecond cache hits are not a credible HDD media baseline.
		ratio = ms / max(1, median)
		if ratio >= 10 {
			warning = "relative-severe"
		} else if ratio >= 3 {
			warning = "relative"
		}
	}
	if ms >= 500 && warning == "" {
		warning = "stall"
	}
	// Keep the first 16 observations, then exclude detected outliers from
	// the moving baseline so a run of retries does not normalize itself.
	if warning == "" || len(samples) < 16 {
		if len(samples) >= 128 {
			samples = samples[1:]
		}
		b[size] = append(samples, ms)
	}
	return warning, ratio
}

func applyHDDLatency(cell *ScanCell, warning string, ratio float64) {
	cell.MaxLatencyRatio = max(cell.MaxLatencyRatio, ratio)
	if warning != "" && cell.LatencyWarning != "relative-severe" {
		cell.LatencyWarning = warning
	}
	if cell.ErrorCount > 0 {
		return
	}
	if cell.LatencyWarning == "relative-severe" {
		cell.Status = "severe"
	} else if cell.LatencyWarning != "" && cell.Status == "normal" {
		cell.Status = "slow"
	}
}

// Caller holds m.mu. Copy diagnostics because API snapshots may share it.
func updateScanReadInfo(result *ScanResult, handle ReadHandle) {
	if h, ok := handle.(interface{ scanReadInfo() (string, string) }); ok {
		d := ScanDiagnostics{}
		if result.Diagnostics != nil {
			d = *result.Diagnostics
		}
		d.ReadMethod, d.ReadNote = h.scanReadInfo()
		if nvme, ok := handle.(interface{ nvmeInfo() NVMeScanInfo }); ok {
			info := nvme.nvmeInfo()
			d.NVMe = &info
		}
		result.Diagnostics = &d
	}
}

// Persist evidence from the command that stopped a scan, not just earlier cells.
func (m *JobManager) recordScanReadFailure(job *Job, cell *ScanCell, read readRegionResult, length int64, previous []BadRange, handle ReadHandle) {
	if read.errorCount == 0 && read.bytesRead == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r := job.Result.(*ScanResult)
	m.updateCell(cell, read.milliseconds, int64(read.bytesRead), int64(read.errorCount), length)
	if r.Version >= 2 {
		cell.CoveredBytes += int64(read.bytesRead)
		_ = m.recordScanIssuesLocked(job, cell, read.badRanges)
		cell.CoveredBytes += sumUnreadableBytes(read.badRanges)
		if cell.CoveredBytes < cell.End-cell.Start && cell.ReadOutcome == "readable" {
			cell.ReadOutcome = "partial"
		}
	}
	applyHDDLatency(cell, "", 0)
	applyScanReadLatency(cell, read.maxReadLatency, job.Disk.Rotational)
	ranges := append([]BadRange{}, previous...)
	appendBadRanges(&ranges, read.badRanges, 200)
	r.Stats.BadRanges = ranges
	m.updateScanStats(job)
	updateScanReadInfo(r, handle)
}
