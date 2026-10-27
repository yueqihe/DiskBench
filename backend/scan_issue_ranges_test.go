package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

func repairDisplayRecord(offset int64, outcome, backup string) BadRange {
	r := BadRange{Offset: offset, Length: 512, Outcome: "rewrite", Code: "SLOW_REWRITE_" + strings.ToUpper(outcome),
		Repair: &ScanRepairRecord{Outcome: outcome, BeforeMs: 1296, BackupFile: backup}}
	if outcome == "verified" || outcome == "still_slow" {
		after := 0.8
		r.Repair.AfterMs, r.Repair.WrittenBytes = &after, 512
	}
	return r
}

func saveDisplayRecords(t *testing.T, m *JobManager, job *Job, records ...BadRange) {
	t.Helper()
	m.mu.Lock()
	err := m.recordScanIssuesLocked(job, &job.Result.(*ScanResult).Map[0], records)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
}

func TestScanIssueRangesPaginateRepairAttemptsAndKeepAuditEvents(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := issueTestJob(m)
	records := []BadRange{}
	for i := 0; i < 51; i++ {
		backup := fmt.Sprintf("%d-512.bin", i*512)
		records = append(records, repairDisplayRecord(int64(i)*512, "pending", backup), repairDisplayRecord(int64(i)*512, "verified", backup))
	}
	// A read error and another attempt at the same LBA must remain separate rows.
	records = append(records, BadRange{Offset: 512, Length: 512, Outcome: "unreadable", Code: "EIO"},
		repairDisplayRecord(0, "pending", "0-512.bin"), repairDisplayRecord(0, "verified", "0-512.bin"))
	saveDisplayRecords(t, m, job, records...)
	for _, offset := range []int64{0, 50, 53} {
		page, err := m.scanIssueRanges(job.ID, offset, 50)
		if err != nil {
			t.Fatal(err)
		}
		want := min(int64(50), 53-offset)
		if page.Total != 53 || int64(len(page.Records)) != want || page.Next != offset+want || !page.Complete {
			t.Fatalf("invalid display page: %+v", page)
		}
		for _, r := range page.Records {
			if r.Repair != nil && (r.Repair.Outcome != "verified" || r.Repair.BeforeMs != 1296 || r.Repair.AfterMs == nil || *r.Repair.AfterMs != 0.8 || r.Repair.WrittenBytes != 512) {
				t.Fatalf("lost repair result or timings: %+v", r)
			}
		}
		if offset == 50 && (page.Records[0].Offset != 50*512 || page.Records[1].Outcome != "unreadable" || page.Records[2].Offset != 0) {
			t.Fatalf("page boundary lost or merged records: %+v", page.Records)
		}
	}
	if _, err := m.scanIssueRanges(job.ID, 54, 50); err == nil {
		t.Fatal("accepted offset beyond display rows")
	}
	raw, err := m.scanIssues(job.ID, 0, 200)
	if err != nil || raw.Total != 105 || len(raw.Records) != 105 || raw.Records[0].Repair.Outcome != "pending" {
		t.Fatalf("raw events changed: %+v / %v", raw, err)
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
	if err := json.Unmarshal(report.Bytes(), &exported); err != nil || len(exported.Issues) != 105 {
		t.Fatalf("export lost audit events: %d / %v", len(exported.Issues), err)
	}
	server := newTestServer(t, m, apiServerOptions{})
	defer server.Close()
	status, body := doRequest(server, "GET", "/api/jobs/"+job.ID+"/scan-issues?view=ranges&offset=50&limit=50", "", map[string]string{"X-Trim-Isadmin": "true"})
	if status != 200 {
		t.Fatalf("display API failed: %d %v", status, body)
	}
	page := body["issues"].(map[string]interface{})
	if page["total"] != float64(53) || page["next"] != float64(53) || len(page["records"].([]interface{})) != 3 {
		t.Fatalf("API paginated raw events: %v", page)
	}
}

func TestScanIssueRangesKeepUnfinishedAndDistinctRepairAttempts(t *testing.T) {
	for _, outcome := range []string{"verified", "still_slow", "failed", "uncertain"} {
		t.Run(outcome, func(t *testing.T) {
			m := newTestManager(t, JobManagerOptions{})
			job := issueTestJob(m)
			saveDisplayRecords(t, m, job, repairDisplayRecord(0, "pending", "0-512.bin"))
			page, err := m.scanIssueRanges(job.ID, 0, 50)
			if err != nil || page.Total != 1 || page.Records[0].Repair.Outcome != "pending" {
				t.Fatalf("unfinished repair hidden: %+v / %v", page, err)
			}
			final := repairDisplayRecord(0, outcome, "0-512.bin")
			final.Repair.Detail = "repair result detail"
			saveDisplayRecords(t, m, job, final)
			page, err = m.scanIssueRanges(job.ID, 0, 50)
			if err != nil || page.Total != 1 || page.Records[0].Repair.Outcome != outcome || page.Records[0].Repair.Detail != final.Repair.Detail {
				t.Fatalf("repair result lost: %+v / %v", page, err)
			}
		})
	}
	for _, mismatch := range []string{"backup", "missing_backup", "offset", "length", "before_ms", "skipped", "nonadjacent"} {
		t.Run(mismatch, func(t *testing.T) {
			m := newTestManager(t, JobManagerOptions{})
			job := issueTestJob(m)
			pending := repairDisplayRecord(0, "pending", "0-512.bin")
			final := repairDisplayRecord(0, "verified", "0-512.bin")
			switch mismatch {
			case "backup":
				final.Repair.BackupFile = "another-attempt.bin"
			case "missing_backup":
				pending.Repair.BackupFile, final.Repair.BackupFile = "", ""
			case "offset":
				final.Offset = 512
			case "length":
				final.Length = 1024
			case "before_ms":
				final.Repair.BeforeMs++
			case "skipped":
				final = repairDisplayRecord(0, "skipped", "0-512.bin")
			}
			records := []BadRange{pending}
			if mismatch == "nonadjacent" {
				records = append(records, BadRange{Offset: 512, Length: 512, Outcome: "unreadable", Code: "EIO"})
			}
			records = append(records, final)
			saveDisplayRecords(t, m, job, records...)
			page, err := m.scanIssueRanges(job.ID, 0, 50)
			if err != nil || page.Total != int64(len(records)) || page.Records[0].Repair.Outcome != "pending" {
				t.Fatalf("merged distinct records: %+v / %v", page, err)
			}
		})
	}
}

func TestScanIssueRangesRejectInvalidTailBeyondRequestedPage(t *testing.T) {
	for _, tail := range []string{"", "{broken\n", "{\"offset\":-1,\"length\":512,\"code\":\"EIO\",\"outcome\":\"unreadable\"}\n"} {
		m := newTestManager(t, JobManagerOptions{})
		job := issueTestJob(m)
		saveDisplayRecords(t, m, job, repairDisplayRecord(0, "pending", "0-512.bin"))
		job.Result.(*ScanResult).Stats.SavedIssueCount = 2
		f, err := os.OpenFile(m.scanIssuePath(job.ID), os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.WriteString(tail)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.scanIssueRanges(job.ID, 0, 1); err == nil {
			t.Fatalf("accepted invalid tail %q", tail)
		}
	}
}
