package main

import (
	"bytes"
	"errors"
)

func (m *JobManager) inspectScanRegion(job *Job, cell *ScanCell, handle ReadHandle, repair RepairHandle, buffer []byte, position int64, length int, probeSize int64, reference scanLatencyReference, load *scanLoadMonitor) (read readRegionResult, warning string, ratio float64, recheck *ScanRecheck, err error) {
	read, err = m.readRegion(handle, buffer, length, position, job, -1, probeSize)
	if err != nil {
		return
	}
	if budget, ok := handle.(*scanSlowBudgetReader); read.slowLimited || ok && budget.exhausted() {
		read.slowLimited = true
		return
	}
	if job.Disk.Rotational {
		warning, ratio = reference.observe(read.milliseconds, int64(length), read.errorCount > 0)
	}
	needsRecheck := read.errorCount == 0 && (warning != "" || scanUnitLatency(read.milliseconds, int64(length)) >= 50 || scanReadNeedsRecheck(read, job.Disk.Rotational))
	activity := load.observe(read.ioBytes, needsRecheck)
	if err = m.repairSlowRequests(job, cell, repair, buffer[:length], position, &read); err != nil {
		return
	}
	if repair != nil && len(read.slowReads) > 0 {
		load.reset() // Repair I/O is ours; do not report it as concurrent work.
	}
	if len(read.postRepairReads) > 0 && len(read.postRepairReads) == len(read.slowReads) {
		// Refresh the complete primary span, including untouched requests. A
		// repaired fast request must not hide another slow request in this cell.
		verify := scanBuffer(length)
		latest, refreshErr := m.readRegion(handle, verify, length, position, job, -1, probeSize)
		latestLoad := load.observe(latest.ioBytes, true)
		if refreshErr != nil || latest.errorCount > 0 || latest.bytesRead != length {
			read.errorCount += latest.errorCount
			for _, issue := range latest.badRanges {
				issue.UnresolvedBytes = 0 // The primary read already established coverage.
				read.badRanges = append(read.badRanges, issue)
			}
			if refreshErr != nil {
				err = refreshErr
				return
			}
		} else if !bytes.Equal(buffer[:length], verify) {
			read.errorCount++
			err = errors.New("修复后区域复读数据与原始数据不一致，已停止扫描，原始备份已保留")
			return
		} else if latestLoad != nil && !latestLoad.BackgroundActivity {
			// Before/after evidence remains in the repair journal. Only current,
			// verified measurements feed this span's map color and timing totals.
			// Keep the slower of the two exact post-write checks as well: a
			// single fast refresh must not erase a recent slow verified response.
			for _, response := range read.postRepairReads {
				if latest.maxReadLatency == nil || response.Ms > latest.maxReadLatency.Ms {
					copyResponse := response
					latest.maxReadLatency = &copyResponse
				}
				if scanRequestStatus(response.Ms, job.Disk.Rotational) != "normal" {
					latest.suspectReads = append(latest.suspectReads, response)
				}
			}
			read = latest
			warning, ratio = "", 0
			if job.Disk.Rotational {
				warning, ratio = reference.observe(read.milliseconds, int64(length), false)
			}
			needsRecheck = scanUnitLatency(read.milliseconds, int64(length)) >= 50 || scanReadNeedsRecheck(read, job.Disk.Rotational) || warning != ""
			activity = latestLoad
		}
		load.reset()
	}
	if !needsRecheck {
		return
	}
	m.mu.Lock()
	job.Phase = "只读复测慢速请求"
	if activity != nil && activity.BackgroundActivity {
		cell.Load = activity
	}
	m.mu.Unlock()
	if err = m.waitScanQuiet(job, activity); err != nil {
		return
	}
	load.reset() // Recheck context covers only its reads, excluding the idle wait.
	recheck, read.badRanges, err = m.recheckSlowRequests(handle, position, length, job, -1, probeSize, read)
	if errors.Is(err, errScanSlowBudget) {
		read.slowLimited, err = true, nil
	}
	if budget, ok := handle.(*scanSlowBudgetReader); ok && budget.exhausted() {
		read.slowLimited = true
	}
	// A skipped recheck does not un-read data established by the primary read.
	issues := read.badRanges[:0]
	for _, r := range read.badRanges {
		if r.Code != "SLOW_READ_LIMIT" {
			issues = append(issues, r)
		}
	}
	read.badRanges = issues
	// The primary read already established coverage. An unfinished recheck
	// keeps its warning without subtracting those successfully read bytes.
	for i := range read.badRanges {
		read.badRanges[i].UnresolvedBytes = 0
	}
	if recheck != nil {
		read.errorCount += int(recheck.ErrorCount)
		recheck.Load = load.observe(recheck.ioBytes, true)
		if recheck.Load != nil && !recheck.Load.BackgroundActivity {
			for _, match := range recheck.confirmedSlow {
				match.Load = recheck.Load
				m.mu.Lock()
				if cell.SlowBadBlock == nil || match.RepeatMs > cell.SlowBadBlock.RepeatMs {
					proof := match
					cell.SlowBadBlock = &proof
				}
				m.mu.Unlock()
			}
		}
	}
	return
}

// Incremental totals keep the hot path independent of the 4096-cell map size.
// Caller holds m.mu; issue counters are maintained by the journal separately.
func scanCellTotals(cell ScanCell, version int) ScanStats {
	s := ScanStats{}
	if version >= 2 || (cell.Status != "pending" && cell.Status != "reading") {
		s.BytesRead = cell.BytesRead
	}
	if version >= 2 {
		s.CoveredBytes = cell.CoveredBytes
	}
	if cell.Status == "pending" || cell.Status == "reading" {
		return s
	}
	s.TestedCells = 1
	if cell.Status == "error" {
		s.ErrorCells = 1
	}
	switch cell.Status {
	case "slow", "slower", "severe", "under150", "under500", "over500":
		s.SlowCells = 1
	}
	return s
}

func (m *JobManager) updateScanCellStats(job *Job, before ScanCell, cell *ScanCell) {
	r := job.Result.(*ScanResult)
	old, next := scanCellTotals(before, r.Version), scanCellTotals(*cell, r.Version)
	r.Stats.TestedCells += next.TestedCells - old.TestedCells
	r.Stats.ErrorCells += next.ErrorCells - old.ErrorCells
	r.Stats.SlowCells += next.SlowCells - old.SlowCells
	r.Stats.BytesRead += next.BytesRead - old.BytesRead
	r.Stats.CoveredBytes += next.CoveredBytes - old.CoveredBytes
}
