package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func timeoutDisplayPair() (BadRange, BadRange) {
	failed := BadRange{Offset: 65536, Length: 512 * 1024, Code: "TIMEOUT", Outcome: "device_error", Recovered: boolPtr(false), Evidence: &ScanIOEvidence{Kind: "TIMEOUT"}}
	return failed, slowSkippedRange(failed.Offset+128*1024, 384*1024)
}

func TestTimeoutDisplayRowsMergeSuffixAndPaginateWithoutChangingEvents(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := issueTestJob(m)
	failed, skipped := timeoutDisplayPair()
	var records []BadRange
	for i := 0; i < 49; i++ {
		records = append(records, BadRange{Offset: int64(i) * 512, Length: 512, Code: "EIO", Outcome: "unreadable"})
	}
	records = append(records, failed, skipped, BadRange{Offset: 800 * 1024, Length: 512, Code: "EIO", Outcome: "unreadable"})
	// Also exercise a pair split between two journal appends.
	saveDisplayRecords(t, m, job, records[:50]...)
	saveDisplayRecords(t, m, job, records[50:]...)
	before, err := os.ReadFile(m.scanIssuePath(job.ID))
	if err != nil {
		t.Fatal(err)
	}
	page, err := m.scanIssueRanges(job.ID, 49, 1)
	if err != nil || page.Total != 51 || page.Next != 50 || len(page.Records) != 1 {
		t.Fatalf("invalid merged page: %+v / %v", page, err)
	}
	merged := page.Records[0]
	if merged.Offset != failed.Offset || merged.Length != failed.Length || merged.Code != failed.Code || merged.Evidence == nil ||
		merged.SkippedRange == nil || merged.SkippedRange.Offset != skipped.Offset || merged.SkippedRange.Length != skipped.Length {
		t.Fatalf("merged row lost request or skipped suffix: %+v", merged)
	}
	if validScanJournalRecord(merged, job.Disk.Size) || scanRangePrecision(merged, 512) != "request" {
		t.Fatal("display row became a journal event or a confirmed bad sector")
	}
	page, err = m.scanIssueRanges(job.ID, 50, 1)
	if err != nil || page.Next != 51 || len(page.Records) != 1 || page.Records[0].Code != "EIO" {
		t.Fatalf("pagination lost following row: %+v / %v", page, err)
	}
	stats := job.Result.(*ScanResult).Stats
	if stats.TimeoutSkipPairs != 1 || stats.IssueCount != 52 || stats.DeviceErrorRanges != 1 || stats.UnresolvedRanges != 1 || stats.SkippedBytes != skipped.Length {
		t.Fatalf("merge changed raw counters: %+v", stats)
	}
	raw, err := m.scanIssues(job.ID, 49, 3)
	if err != nil || raw.Total != 52 || len(raw.Records) != 3 || raw.Records[0].Code != "TIMEOUT" || raw.Records[1].Code != "SLOW_READ_LIMIT" {
		t.Fatalf("raw audit events changed: %+v / %v", raw, err)
	}
	stream, err := m.prepareScanReport(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var report bytes.Buffer
	if err := stream(&report); err != nil {
		t.Fatal(err)
	}
	var exported struct{ Issues []BadRange }
	if err := json.Unmarshal(report.Bytes(), &exported); err != nil || len(exported.Issues) != 52 || exported.Issues[49].SkippedRange != nil {
		t.Fatalf("raw export lost original events: %v", err)
	}
	if !m.persistSafe(job, "test merged timeout") {
		t.Fatal("could not save history")
	}
	restarted := NewJobManager(m.dataDir, JobManagerOptions{})
	restored := restarted.get(job.ID)
	if restored == nil || restored.Result.(*ScanResult).Stats.TimeoutSkipPairs != 1 {
		t.Fatal("restart did not reconstruct merged count")
	}
	after, err := os.ReadFile(m.scanIssuePath(job.ID))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("display or restart rewrote audit journal")
	}
}

func TestTimeoutDisplayRowsKeepUnrelatedAndUnfinishedEvents(t *testing.T) {
	failed, skipped := timeoutDisplayPair()
	other := BadRange{Offset: 512, Length: 512, Code: "EIO", Outcome: "unreadable"}
	for _, name := range []string{"alone", "nonadjacent", "different_end", "outside", "recovered", "unreadable", "transport"} {
		t.Run(name, func(t *testing.T) {
			first, next := failed, skipped
			records := []BadRange{first, next}
			switch name {
			case "alone":
				records = records[:1]
			case "nonadjacent":
				records = []BadRange{first, other, next}
			case "different_end":
				records[1].Length -= 512
			case "outside":
				records[1].Offset -= first.Length
				records[1].Length += first.Length
			case "recovered":
				records[0].Recovered = boolPtr(true)
			case "unreadable":
				records[0].Outcome = "unreadable"
			case "transport":
				records[0].Code = "TRANSPORT_ERROR"
			}
			var output []BadRange
			rows := scanIssueRows{emit: func(r BadRange) error { output = append(output, r); return nil }}
			for _, record := range records {
				if err := rows.add(record); err != nil {
					t.Fatal(err)
				}
			}
			if err := rows.flush(); err != nil || len(output) != len(records) {
				t.Fatalf("unrelated or unfinished events merged: %+v / %v", output, err)
			}
		})
	}
}

func TestHumanReportMergesTimeoutWithSkippedSuffix(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := humanReportJob(m)
	failed, skipped := timeoutDisplayPair()
	saveDisplayRecords(t, m, job, failed, skipped)
	body := renderHumanReport(t, m, job)
	for _, want := range []string{"已列出 1 条明细", "读取超时，已跳过", "LBA 128–1151", "跳过 LBA 384–1151", "384.00 KiB", "跳过部分待定位", "未完成读取检测"} {
		if !strings.Contains(body, want) {
			t.Fatalf("report lost merged timeout detail %q", want)
		}
	}
}
