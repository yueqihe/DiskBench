package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func severeVerdictJob(m *JobManager) *Job {
	job := humanReportJob(m)
	r := job.Result.(*ScanResult)
	for i := range r.Map {
		cell := &r.Map[i]
		cell.Status, cell.ReadOutcome = "normal", "readable"
		cell.BytesRead, cell.CoveredBytes = cell.End-cell.Start, cell.End-cell.Start
	}
	cell := &r.Map[1]
	cell.MaxMs = floatPtr(16391)
	applyScanReadLatency(cell, &ScanReadLatency{Ms: 1962, Bytes: 128 * 1024, Offset: cell.Start + 512}, true)
	cell.Recheck = &ScanRecheck{Outcome: "slow", MaxReadLatency: &ScanReadLatency{Ms: 1940, Bytes: 128 * 1024, Offset: cell.Start + 512}}
	r.Stats.SlowCells = 1
	return job
}

func TestSevereReadableRegionIsAbnormalWithoutInventingReadErrors(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := severeVerdictJob(m)
	r := job.Result.(*ScanResult)
	cell := r.Map[1]
	if cell.Status != "severe" || scanAbnormalCellCount(r) != 1 {
		t.Fatalf("1962ms successful read was not abnormal: %+v", cell)
	}
	message := scanOutcomeCompletionMessage("完整扫描", r, 0, 1)
	if !strings.Contains(message, "1 个异常区域") || !strings.Contains(message, "已影响正常使用") || strings.Contains(message, "未发现") {
		t.Fatalf("severe stall got a reassuring conclusion: %s", message)
	}
	page, err := m.scanIssueRanges(job.ID, 0, 50)
	if err != nil || page.Total != 1 || page.Next != 1 || !page.Complete {
		t.Fatalf("severe-only details unavailable: %+v / %v", page, err)
	}
	row := page.Records[0]
	if row.Outcome != "severe" || row.Code != "SEVERE_SLOW_READ" || row.Offset != cell.Start+512 || row.Length != 128*1024 || row.ProbeBytes != row.Length {
		t.Fatalf("lost worst request address: %+v", row)
	}
	for _, want := range []string{"区域 2", "1962.0 ms", "1940.0 ms", "已影响正常使用"} {
		if !strings.Contains(row.Detail, want) {
			t.Errorf("missing evidence %q in %s", want, row.Detail)
		}
	}
	if r.Stats.ErrorCells != 0 || r.Stats.UnreadableRanges != 0 || r.Stats.UnreadableBytes != 0 || cell.ErrorCount != 0 || cell.ReadOutcome != "readable" || r.Stats.CoveredBytes != job.Disk.Size {
		t.Fatalf("usability verdict changed actual I/O accounting: %+v / %+v", r.Stats, cell)
	}
	raw, err := m.scanIssues(job.ID, 0, 50)
	if err != nil || raw.Total != 0 || len(raw.Records) != 0 || r.Stats.IssueCount != 0 || r.Stats.SavedIssueCount != 0 {
		t.Fatalf("derived severity polluted audit events: %+v / %v", raw, err)
	}
	if _, err := os.Stat(m.scanIssuePath(job.ID)); !os.IsNotExist(err) {
		t.Fatalf("severe display created an audit journal: %v", err)
	}
	stream, err := m.prepareScanReport(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := stream(&output); err != nil {
		t.Fatal(err)
	}
	var report struct{ Issues []BadRange }
	if err := json.Unmarshal(output.Bytes(), &report); err != nil || len(report.Issues) != 0 {
		t.Fatalf("raw report gained fabricated read errors: %s / %v", output.String(), err)
	}
}

func TestAbnormalCountKeepsMilderSlownessSeparateAndSupportsLegacy(t *testing.T) {
	r := &ScanResult{Map: []ScanCell{
		{Status: "normal"}, {Status: "slow"}, {Status: "slower"}, {Status: "under150"}, {Status: "under500"},
		{Status: "pending"}, {Status: "reading"}, {Status: "severe"}, {Status: "over500"}, {Status: "error", ErrorCount: 3},
	}}
	if count := scanAbnormalCellCount(r); count != 3 {
		t.Fatalf("abnormal count should count each severe/error region once: %d", count)
	}
	r.Map = []ScanCell{{Index: 0, Start: 0, End: 4096, Status: "over500", MaxMs: floatPtr(1200), Recheck: &ScanRecheck{Outcome: "not_reproduced"}}}
	rows := scanSevereRanges(r)
	if len(rows) != 1 || rows[0].Offset != 0 || rows[0].Length != 4096 || !strings.Contains(rows[0].Detail, "地址为区域范围") || !strings.Contains(rows[0].Detail, "保留首次异常") {
		t.Fatalf("legacy timing evidence lost: %+v", rows)
	}
}

func TestSevereDetailsPaginateAfterGroupedRepairWithoutChangingJournal(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := severeVerdictJob(m)
	r := job.Result.(*ScanResult)
	applyScanReadLatency(&r.Map[0], &ScanReadLatency{Ms: 1000, Bytes: 512, Offset: 0}, true)
	saveDisplayRecords(t, m, job, repairDisplayRecord(0, "pending", "0-512.bin"), repairDisplayRecord(0, "verified", "0-512.bin"))
	for offset := int64(0); offset <= 3; offset++ {
		page, err := m.scanIssueRanges(job.ID, offset, 1)
		if err != nil || page.Total != 3 || page.Next != min(3, offset+1) || len(page.Records) != int(min(1, 3-offset)) {
			t.Fatalf("wrong mixed page at %d: %+v / %v", offset, page, err)
		}
		if offset == 0 && page.Records[0].Repair.Outcome != "verified" || offset > 0 && offset < 3 && page.Records[0].Outcome != "severe" {
			t.Fatalf("wrong mixed row at %d: %+v", offset, page.Records)
		}
	}
	if _, err := m.scanIssueRanges(job.ID, 4, 1); err == nil {
		t.Fatal("accepted an offset beyond severe details")
	}
	raw, err := m.scanIssues(job.ID, 0, 50)
	if err != nil || raw.Total != 2 || len(raw.Records) != 2 || raw.Records[0].Repair.Outcome != "pending" {
		t.Fatalf("severity changed repair audit events: %+v / %v", raw, err)
	}
	body := renderHumanReport(t, m, job)
	if !strings.Contains(body, "已列出 3 条明细") || !strings.Contains(body, "写回校验通过") || !strings.Contains(body, "1940.0 ms") {
		t.Fatal("human report lost mixed repair and severity details")
	}
}

func TestHumanReportTreatsSevereSuccessfulReadsAsAbnormalAndKeepsSnapshot(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := severeVerdictJob(m)
	job.Message = "完整扫描未发现读取错误，1 个区域响应偏慢"
	stream, _, err := m.prepareScanHTMLReport(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	job.Result.(*ScanResult).Map[1].Status = "normal"
	var output bytes.Buffer
	if err := stream(&output); err != nil {
		t.Fatal(err)
	}
	body := output.String()
	for _, want := range []string{`verdict danger`, "发现 1 个异常区域", "已影响正常使用", "严重慢读", "1962.0 ms", "1940.0 ms", "LBA 1025–1280", "已列出 1 条明细", "异常记录已完整读取", "</body></html>"} {
		if !strings.Contains(body, want) {
			t.Errorf("human report lost severe result %q", want)
		}
	}
	if strings.Contains(body, "本次检测未记录读取异常") || strings.Contains(body, job.Message) || strings.Contains(body, "发现持续不可读范围") {
		t.Fatal("human report reassured on severe slowness or invented unreadable sectors")
	}
}
