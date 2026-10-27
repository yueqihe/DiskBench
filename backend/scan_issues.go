package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const scanIssuePreviewLimit = 200

func validScanJournalRecord(r BadRange, size int64) bool {
	if r.SkippedRange != nil {
		return false // Combined display rows must never replace original events.
	}
	if r.Offset < 0 || r.Length <= 0 || r.Offset > size-r.Length || r.ProbeBytes < 0 || r.Code == "" || r.UnresolvedBytes < 0 || r.UnresolvedBytes > r.Length || (r.UnresolvedBytes > 0 && ((r.Code != "LOCALIZATION_LIMIT" && r.Code != "SLOW_READ_LIMIT") || r.Outcome != "unresolved")) {
		return false
	}
	if r.Code == "SLOW_READ_LIMIT" && (r.Outcome != "unresolved" || r.UnresolvedBytes != r.Length || r.Detail == "") {
		return false
	}
	switch r.Outcome {
	case "recovered", "unreadable", "device_error", "unresolved":
		return true
	case "rewrite":
		if r.Repair == nil || r.Repair.BeforeMs < 0 || r.Repair.WrittenBytes < 0 || r.Repair.WrittenBytes > r.Length || (r.Repair.AfterMs != nil && *r.Repair.AfterMs < 0) {
			return false
		}
		switch r.Repair.Outcome {
		case "pending", "uncertain", "verified", "still_slow", "failed", "skipped":
			return true
		}
	}
	return false
}

func sumUnreadableBytes(ranges []BadRange) int64 {
	var total int64
	for _, r := range ranges {
		if r.Outcome == "unreadable" {
			total += r.Length
		}
	}
	return total
}

type ScanIssuePage struct {
	Offset   int64      `json:"offset"`
	Next     int64      `json:"next"`
	Total    int64      `json:"total"`
	Complete bool       `json:"complete"`
	Legacy   bool       `json:"legacy"`
	Records  []BadRange `json:"records"`
}

func classifyScanRange(r BadRange) BadRange {
	if r.Outcome != "" {
		return r
	}
	if r.Code == "RECOVERED_ERROR" || (r.Recovered != nil && *r.Recovered && !deviceScanIssue(r)) {
		r.Outcome = "recovered"
	} else if deviceScanIssue(r) {
		r.Outcome = "device_error"
	} else {
		// A parent failed read is not evidence that every sector in it failed.
		r.Outcome = "unresolved"
	}
	return r
}

func deviceScanIssue(r BadRange) bool {
	switch r.Code {
	case "EIO", "EREMOTEIO", "SHORT_READ", "READ_RETRY", "MEDIUM_ERROR", "DATA_INTEGRITY_ERROR", "RECOVERED_ERROR":
		return false
	default:
		return true
	}
}

func countScanIssue(stats *ScanStats, cell *ScanCell, r BadRange) {
	if stats.lastIssue != nil && timeoutSkipPair(*stats.lastIssue, r) {
		stats.TimeoutSkipPairs++
	}
	previous := r
	stats.lastIssue = &previous
	stats.IssueCount++
	switch r.Outcome {
	case "rewrite":
		applyScanRepairStats(&stats.Repair, r.Repair)
		if cell != nil {
			copyStats := ScanRepairStats{}
			if cell.Repair != nil {
				copyStats = *cell.Repair
			}
			applyScanRepairStats(&copyStats, r.Repair)
			cell.Repair = &copyStats
		}
	case "unreadable":
		stats.UnreadableRanges++
		stats.UnreadableBytes += r.Length
		if cell != nil {
			cell.UnreadableBytes += r.Length
			cell.ReadOutcome = "unreadable"
		}
	case "recovered":
		stats.RecoveredRanges++
		if cell != nil {
			cell.RecoveredRanges++
			if cell.ReadOutcome == "" || cell.ReadOutcome == "readable" {
				cell.ReadOutcome = "recovered"
			}
		}
	case "device_error":
		stats.DeviceErrorRanges++
		if cell != nil {
			cell.DeviceErrorRanges++
			if cell.ReadOutcome != "unreadable" {
				cell.ReadOutcome = "device_error"
			}
		}
	default:
		stats.UnresolvedRanges++
		if r.Code == "LOCALIZATION_LIMIT" || r.Code == "SLOW_READ_LIMIT" {
			stats.UnresolvedBytes += r.UnresolvedBytes
			if cell != nil {
				cell.UnresolvedBytes += r.UnresolvedBytes
			}
		}
		if r.Code == "SLOW_READ_LIMIT" {
			stats.SkippedBytes += r.UnresolvedBytes
			if cell != nil {
				cell.SkippedBytes += r.UnresolvedBytes
			}
		}
		if cell != nil && (cell.ReadOutcome == "" || cell.ReadOutcome == "readable") {
			cell.ReadOutcome = "unresolved"
		}
	}
}

func (m *JobManager) scanIssuePath(id string) string {
	return filepath.Join(m.jobsDir, id+".scan-issues.jsonl")
}

// Caller holds m.mu. All records go to disk; only the API preview is bounded.
func (m *JobManager) recordScanIssuesLocked(job *Job, cell *ScanCell, ranges []BadRange) error {
	result := job.Result.(*ScanResult)
	if result.Version < 2 {
		return nil
	}
	if cell.ReadOutcome == "" || cell.ReadOutcome == "partial" {
		cell.ReadOutcome = "readable"
	}
	for _, r := range ranges {
		countScanIssue(&result.Stats, cell, classifyScanRange(r))
	}
	targetBytes := scanCellTarget(result, cell)
	if cell.CoveredBytes < targetBytes && cell.ReadOutcome == "readable" {
		cell.ReadOutcome = "partial"
	}
	result.Stats.IssuesTruncated = result.Stats.IssueCount > scanIssuePreviewLimit
	if len(ranges) == 0 {
		return nil
	}
	fail := func(err error) error {
		result.Stats.IssuesComplete = false
		result.Stats.IssueWarning = "完整异常记录保存失败，扫描已停止；报告可能缺少地址"
		return fmt.Errorf("%s：%w", result.Stats.IssueWarning, err)
	}
	if !jobIDPattern.MatchString(job.ID) {
		return fail(errors.New("任务 ID 无效"))
	}
	f, err := os.OpenFile(m.scanIssuePath(job.ID), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fail(err)
	}
	encoder := json.NewEncoder(f)
	for _, r := range ranges {
		if err = encoder.Encode(classifyScanRange(r)); err != nil {
			break
		}
		result.Stats.SavedIssueCount++
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return fail(err)
	}
	return nil
}

func scanOutcomeCompletionMessage(label string, result *ScanResult, errorCells, slowCells int64, media ...bool) string {
	rotational := !strings.HasPrefix(result.LatencyBasis, "solid-")
	if len(media) > 0 {
		rotational = media[0]
	}
	bad := scanBadBlocks(result, rotational)
	if bad.Total > 0 {
		return fmt.Sprintf("%s发现 %d 个坏块区域（慢速 %d、不可读 %d），已不满足正常使用要求，建议更换硬盘", label, bad.Total, bad.Slow, bad.Unreadable)
	}
	if result.Stats.SkippedBytes > 0 {
		return fmt.Sprintf("%s发现 %d 个异常区域，严重卡读已影响正常使用；跳过 %s 未读范围并继续后续区域", label, scanAbnormalCellCount(result), reportBytes(result.Stats.SkippedBytes))
	}
	severe := int64(0)
	for _, cell := range result.Map {
		if severeScanStatus(cell.Status) {
			severe++
		}
	}
	if severe > 0 {
		return fmt.Sprintf("%s发现 %d 个异常区域，存在严重慢读，已影响正常使用", label, scanAbnormalCellCount(result))
	}
	if result.Version < 2 || result.Stats.IssueCount == 0 {
		return scanCompletionMessage(label, errorCells, slowCells)
	}
	s := result.Stats
	parts := []string{}
	if s.UnreadableRanges > 0 {
		parts = append(parts, fmt.Sprintf("%d 条持续读取失败范围", s.UnreadableRanges))
	}
	if s.RecoveredRanges > 0 {
		parts = append(parts, fmt.Sprintf("%d 条复读恢复或固件纠正告警", s.RecoveredRanges))
	}
	if s.DeviceErrorRanges > 0 {
		parts = append(parts, fmt.Sprintf("%d 条设备/连接异常", s.DeviceErrorRanges))
	}
	if s.UnresolvedRanges > 0 {
		parts = append(parts, fmt.Sprintf("%d 条未确定范围的读取异常", s.UnresolvedRanges))
	}
	if slowCells > 0 {
		parts = append(parts, fmt.Sprintf("%d 个慢响应区域", slowCells))
	}
	return label + "记录了" + strings.Join(parts, "；")
}

// Snapshot the file length and count together; a page cannot see half an append.
func (m *JobManager) scanIssueSnapshot(id string) (*Job, *os.File, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[id]
	if !jobIDPattern.MatchString(id) || job == nil {
		return nil, nil, 0, newJobError("任务不存在", "JOB_NOT_FOUND", 404)
	}
	result, ok := job.Result.(*ScanResult)
	if !ok || job.Type != "scan" {
		return nil, nil, 0, newJobError("该任务没有只读扫描异常记录", "SCAN_ISSUES_UNAVAILABLE", 409)
	}
	snapshot := cloneJobLocked(job)
	if result.Version < 2 || result.Stats.SavedIssueCount == 0 {
		return snapshot, nil, 0, nil
	}
	f, err := os.Open(m.scanIssuePath(id))
	if err != nil {
		return nil, nil, 0, newJobError("完整异常记录不可用，请检查本地报告文件", "SCAN_ISSUES_UNAVAILABLE", 409)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, 0, err
	}
	return snapshot, f, info.Size(), nil
}

func issueScanner(reader io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	return scanner
}

func (m *JobManager) scanIssues(id string, offset int64, limit int) (*ScanIssuePage, error) {
	if offset < 0 || limit < 1 || limit > 200 {
		return nil, newJobError("异常明细分页参数无效", "INVALID_ARGUMENT", 400)
	}
	job, f, size, err := m.scanIssueSnapshot(id)
	if err != nil {
		return nil, err
	}
	s := job.Result.(*ScanResult)
	page := &ScanIssuePage{Offset: offset, Next: offset, Records: []BadRange{}, Legacy: s.Version < 2, Complete: s.Version >= 2 && s.Stats.IssuesComplete}
	page.Total = s.Stats.SavedIssueCount
	if page.Legacy {
		page.Total = int64(len(s.Stats.BadRanges))
	}
	if offset > page.Total {
		if f != nil {
			f.Close()
		}
		return nil, newJobError("offset 超出异常记录范围", "INVALID_ARGUMENT", 400)
	}
	if f == nil {
		if page.Legacy {
			end := min(page.Total, offset+int64(limit))
			page.Records = append(page.Records, s.Stats.BadRanges[offset:end]...)
			page.Next = end
		}
		return page, nil
	}
	defer f.Close()
	if offset == page.Total {
		return page, nil
	}
	scanner := issueScanner(io.LimitReader(f, size))
	for index := int64(0); index < page.Total && scanner.Scan(); index++ {
		if index < offset {
			continue
		}
		var r BadRange
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil || !validScanJournalRecord(r, job.Disk.Size) {
			return nil, errors.New("异常记录损坏，无法完整读取")
		}
		page.Records = append(page.Records, r)
		page.Next++
		if len(page.Records) == limit {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if page.Next < min(page.Total, offset+int64(limit)) {
		return nil, errors.New("异常记录被截断，报告不完整")
	}
	return page, nil
}

func completesRepairAttempt(pending, final BadRange) bool {
	if pending.Outcome != "rewrite" || pending.Repair == nil || pending.Repair.Outcome != "pending" ||
		final.Outcome != "rewrite" || final.Repair == nil || pending.Repair.BackupFile == "" ||
		pending.Offset != final.Offset || pending.Length != final.Length ||
		pending.Repair.BackupFile != final.Repair.BackupFile || pending.Repair.BeforeMs != final.Repair.BeforeMs {
		return false
	}
	switch final.Repair.Outcome {
	case "verified", "still_slow", "failed", "uncertain":
		return true
	}
	return false
}

type scanIssueRows struct {
	pending *BadRange
	emit    func(BadRange) error
}

// A timeout and its immediately following skipped suffix describe one event.
// Keep the full failed request and the exact unread suffix as distinct ranges.
func timeoutSkipPair(failed, skipped BadRange) bool {
	return failed.Code == "TIMEOUT" && failed.Outcome == "device_error" &&
		(failed.Recovered == nil || !*failed.Recovered) && failed.SkippedRange == nil &&
		skipped.Code == "SLOW_READ_LIMIT" && skipped.Outcome == "unresolved" &&
		skipped.Length > 0 && skipped.UnresolvedBytes == skipped.Length &&
		failed.Length > 0 && skipped.Offset >= failed.Offset &&
		skipped.Offset+skipped.Length == failed.Offset+failed.Length
}

func (rows *scanIssueRows) add(r BadRange) error {
	if rows.pending != nil {
		if timeoutSkipPair(*rows.pending, r) {
			merged := *rows.pending
			merged.SkippedRange = &ScanSkippedRange{Offset: r.Offset, Length: r.Length}
			rows.pending = nil
			return rows.emit(merged)
		}
		if completesRepairAttempt(*rows.pending, r) {
			rows.pending = nil
			return rows.emit(r)
		}
		if err := rows.flush(); err != nil {
			return err
		}
	}
	if r.Outcome == "rewrite" && r.Repair != nil && r.Repair.Outcome == "pending" || r.Code == "TIMEOUT" && r.Outcome == "device_error" {
		rows.pending = &r
		return nil
	}
	return rows.emit(r)
}

func (rows *scanIssueRows) flush() error {
	if rows.pending == nil {
		return nil
	}
	r := *rows.pending
	rows.pending = nil
	return rows.emit(r)
}

// Paginate merged display rows, keeping the append-only audit journal intact.
// Adjacent repair events and timeout/skip pairs share one row.
func (m *JobManager) scanIssueRanges(id string, offset int64, limit int) (*ScanIssuePage, error) {
	if offset < 0 || limit < 1 || limit > 200 {
		return nil, newJobError("异常明细分页参数无效", "INVALID_ARGUMENT", 400)
	}
	job, f, size, err := m.scanIssueSnapshot(id)
	if err != nil {
		return nil, err
	}
	s := job.Result.(*ScanResult)
	page := &ScanIssuePage{Offset: offset, Next: offset, Records: []BadRange{}, Legacy: s.Version < 2, Complete: s.Version >= 2 && s.Stats.IssuesComplete}
	rows := scanIssueRows{emit: func(r BadRange) error {
		if page.Total >= offset && len(page.Records) < limit {
			page.Records = append(page.Records, r)
			page.Next++
		}
		page.Total++
		return nil
	}}
	if f != nil {
		defer f.Close()
		// Stream the snapshot to count logical rows without retaining the journal.
		scanner := issueScanner(io.LimitReader(f, size))
		for index := int64(0); index < s.Stats.SavedIssueCount; index++ {
			if !scanner.Scan() {
				if err := scanner.Err(); err != nil {
					return nil, err
				}
				return nil, errors.New("异常记录被截断，报告不完整")
			}
			var r BadRange
			if err := json.Unmarshal(scanner.Bytes(), &r); err != nil || !validScanJournalRecord(r, job.Disk.Size) {
				return nil, errors.New("异常记录损坏，无法完整读取")
			}
			if err := rows.add(r); err != nil {
				return nil, err
			}
		}
	} else if page.Legacy {
		for _, r := range s.Stats.BadRanges {
			if err := rows.add(r); err != nil {
				return nil, err
			}
		}
	}
	if err := rows.flush(); err != nil {
		return nil, err
	}
	for _, r := range scanSevereRanges(s, job.Disk.Rotational) {
		if err := rows.emit(r); err != nil {
			return nil, err
		}
	}
	if offset > page.Total {
		return nil, newJobError("offset 超出异常记录范围", "INVALID_ARGUMENT", 400)
	}
	return page, nil
}

func (m *JobManager) removeScanIssueFile(id string) {
	if !jobIDPattern.MatchString(id) {
		return
	}
	if err := os.Remove(m.scanIssuePath(id)); err != nil && !os.IsNotExist(err) {
		m.logger.Errorf("[diskbench] 删除扫描异常记录失败 (%v)", err)
	}
}

// Rebuild counters and the bounded preview after restart, including records saved
// after the last progress checkpoint. Older reports keep their original semantics.
func (m *JobManager) restoreScanIssues(job *Job) {
	r, ok := job.Result.(*ScanResult)
	if !ok || r.Version < 2 || job.Type != "scan" {
		return
	}
	f, err := os.Open(m.scanIssuePath(job.ID))
	if err != nil {
		if r.Stats.IssueCount > 0 || r.Stats.SavedIssueCount > 0 || !os.IsNotExist(err) {
			r.Stats.IssuesComplete = false
			r.Stats.IssueWarning = "完整异常记录文件不可用"
		}
		return
	}
	defer f.Close()
	stats := r.Stats
	stats.TimeoutSkipPairs, stats.lastIssue = 0, nil
	stats.IssueCount, stats.SavedIssueCount, stats.UnreadableRanges, stats.UnreadableBytes = 0, 0, 0, 0
	stats.RecoveredRanges, stats.DeviceErrorRanges, stats.UnresolvedRanges = 0, 0, 0
	stats.UnresolvedBytes, stats.SkippedBytes = 0, 0
	stats.BadRanges = []BadRange{}
	stats.Repair = ScanRepairStats{}
	scanner := issueScanner(f)
	for scanner.Scan() {
		var record BadRange
		if json.Unmarshal(scanner.Bytes(), &record) != nil || !validScanJournalRecord(record, job.Disk.Size) {
			stats.IssuesComplete = false
			stats.IssueWarning = "异常记录损坏，报告不完整"
			break
		}
		countScanIssue(&stats, nil, classifyScanRange(record))
		stats.SavedIssueCount++
		appendBadRanges(&stats.BadRanges, []BadRange{record}, scanIssuePreviewLimit)
	}
	if scanner.Err() != nil {
		stats.IssuesComplete = false
		stats.IssueWarning = "异常记录读取失败，报告不完整"
	}
	if stats.SavedIssueCount < r.Stats.SavedIssueCount {
		stats.IssuesComplete = false
		if stats.IssueWarning == "" {
			stats.IssueWarning = "异常记录缺少已保存的地址，报告不完整"
		}
	}
	// Keep already observed failures visible even if their journal entries were
	// lost or could not be saved. SavedIssueCount describes the readable journal.
	if stats.IssueCount < r.Stats.IssueCount {
		stats.IssuesComplete = false
		if stats.IssueWarning == "" {
			stats.IssueWarning = "部分已发现的异常缺少完整地址，报告不完整"
		}
		stats.IssueCount = r.Stats.IssueCount
		stats.TimeoutSkipPairs = r.Stats.TimeoutSkipPairs
		stats.UnreadableRanges, stats.UnreadableBytes = r.Stats.UnreadableRanges, r.Stats.UnreadableBytes
		stats.RecoveredRanges, stats.DeviceErrorRanges, stats.UnresolvedRanges = r.Stats.RecoveredRanges, r.Stats.DeviceErrorRanges, r.Stats.UnresolvedRanges
		stats.UnresolvedBytes = r.Stats.UnresolvedBytes
		stats.SkippedBytes = r.Stats.SkippedBytes
		stats.Repair = r.Stats.Repair
		if len(r.Stats.BadRanges) > len(stats.BadRanges) {
			stats.BadRanges = r.Stats.BadRanges
		}
	}
	stats.IssuesTruncated = stats.IssueCount > scanIssuePreviewLimit
	r.Stats = stats
}
