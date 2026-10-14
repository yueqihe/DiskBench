package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const issueTestID = "12345678-1234-4123-8123-123456789abc"

func issueTestJob(m *JobManager) *Job {
	job := &Job{ID: issueTestID, Type: "scan", Status: "completed", Device: "/dev/test",
		Disk:   DiskSnapshot{Size: 1024 * 1024, LogicalSectorSize: 512},
		Result: &ScanResult{Version: 2, Coverage: "full", Map: createScanMap(1024*1024, 2, 512), Stats: ScanStats{IssuesComplete: true, BadRanges: []BadRange{}}},
	}
	m.jobs[job.ID] = job
	return job
}

func TestScanReadOutcomesDoNotTurnRecoveryOrTransportIntoUnreadable(t *testing.T) {
	for _, kind := range []string{"recovered", "unreadable", "transport"} {
		t.Run(kind, func(t *testing.T) {
			m := newTestManager(t, JobManagerOptions{})
			job := issueTestJob(m)
			job.Status = "running"
			calls := 0
			handle := scanReaderFunc(func(p []byte, offset int64) (int, error) {
				calls++
				if kind == "transport" {
					return 0, &scanIOError{Evidence: ScanIOEvidence{Kind: "TRANSPORT_ERROR"}}
				}
				if kind == "recovered" && calls == 1 {
					return 0, syscall.EIO
				}
				if kind == "unreadable" && offset <= 512 && offset+int64(len(p)) > 512 {
					return 0, syscall.EIO
				}
				return len(p), nil
			})
			read, err := m.readRegion(handle, scanBuffer(2048), 2048, 0, job, -1, 512)
			if (err != nil) != (kind == "transport") {
				t.Fatalf("unexpected read error: %v", err)
			}
			result := job.Result.(*ScanResult)
			m.mu.Lock()
			recordErr := m.recordScanIssuesLocked(job, &result.Map[0], read.badRanges)
			m.mu.Unlock()
			if recordErr != nil {
				t.Fatal(recordErr)
			}
			s := result.Stats
			if kind == "recovered" && (s.RecoveredRanges != 1 || s.UnreadableRanges != 0 || read.bytesRead != 2048) {
				t.Fatalf("recovery counted as unreadable: %+v", s)
			}
			if kind == "unreadable" && (s.UnreadableRanges != 1 || s.UnreadableBytes != 512 || read.bytesRead != 1536) {
				t.Fatalf("wrong localized range: %+v", s)
			}
			if kind == "transport" && (s.DeviceErrorRanges != 1 || s.UnreadableRanges != 0 || calls != 2) {
				t.Fatalf("transport became bad sectors: %+v calls=%d", s, calls)
			}
		})
	}
}

func TestFirmwareCorrectionInLargeTransferRemainsReadable(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := &Job{ID: "large-recovered", Status: "running"}
	calls := 0
	h := scanReaderFunc(func(p []byte, offset int64) (int, error) {
		calls++
		return len(p), &scanIOError{Evidence: ScanIOEvidence{Kind: "RECOVERED_ERROR"}}
	})
	read, err := m.readRegion(h, scanBuffer(4*scanLatencyRequestBytes), 4*scanLatencyRequestBytes, 0, job, -1, 512)
	if err != nil || read.bytesRead != 4*scanLatencyRequestBytes || calls != 4 || len(read.badRanges) != 1 || read.badRanges[0].Outcome != "recovered" {
		t.Fatalf("firmware correction stopped valid transfer: %+v calls=%d err=%v", read, calls, err)
	}
}

func TestShortReadsDuringFinalRetryCanStillRecover(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := &Job{ID: "short-retry", Status: "running"}
	calls := 0
	h := scanReaderFunc(func(p []byte, offset int64) (int, error) {
		calls++
		if calls <= 2 {
			return 0, syscall.EIO
		}
		if calls == 3 {
			return 256, nil
		}
		return len(p), nil
	})
	read, err := m.readRegion(h, scanBuffer(512), 512, 0, job, -1, 512)
	if err != nil || read.bytesRead != 512 || len(read.badRanges) != 1 || read.badRanges[0].Outcome != "recovered" {
		t.Fatalf("complete retried data became unreadable: %+v / %v", read, err)
	}
}

func TestCompleteScanIssueJournalSurvivesPreviewLimitRestartAndExport(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := issueTestJob(m)
	result := job.Result.(*ScanResult)
	ranges := make([]BadRange, 405)
	for i := range ranges {
		ranges[i] = BadRange{Offset: int64(i) * 512, Length: 512, Code: "EIO", Recovered: boolPtr(false), Outcome: "unreadable", ProbeBytes: 512}
	}
	m.mu.Lock()
	err := m.recordScanIssuesLocked(job, &result.Map[0], ranges)
	appendBadRanges(&result.Stats.BadRanges, ranges, scanIssuePreviewLimit)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Stats.BadRanges) != 200 || result.Stats.IssueCount != 405 || result.Stats.UnreadableBytes != 405*512 || !result.Stats.IssuesTruncated {
		t.Fatalf("lost records: %+v", result.Stats)
	}
	page, err := m.scanIssues(job.ID, 400, 50)
	if err != nil || page.Total != 405 || len(page.Records) != 5 || page.Records[0].Offset != 400*512 || !page.Complete {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	stream, err := m.prepareScanReport(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := stream(&output); err != nil {
		t.Fatal(err)
	}
	var report struct {
		RecordsComplete    bool
		ExportedIssueCount int
		Issues             []BadRange
	}
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if !report.RecordsComplete || report.ExportedIssueCount != 405 || len(report.Issues) != 405 {
		t.Fatalf("incomplete export: %+v", report)
	}
	if !m.persistSafe(job, "test") {
		t.Fatal("cannot persist job")
	}
	restored := NewJobManager(m.dataDir, JobManagerOptions{Logger: silentLogger{}})
	loaded := restored.get(job.ID).Result.(*ScanResult)
	if loaded.Stats.SavedIssueCount != 405 || loaded.Stats.UnreadableBytes != 405*512 || len(loaded.Stats.BadRanges) != 200 {
		t.Fatalf("restart lost journal: %+v", loaded.Stats)
	}
	if err := restored.remove(job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.scanIssuePath(job.ID)); !os.IsNotExist(err) {
		t.Fatalf("journal not removed: %v", err)
	}
}

func TestScanIssuePersistenceFailureStopsAndMarksIncomplete(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := issueTestJob(m)
	result := job.Result.(*ScanResult)
	if err := os.Mkdir(m.scanIssuePath(job.ID), 0700); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	err := m.recordScanIssuesLocked(job, &result.Map[0], []BadRange{{Offset: 0, Length: 512, Code: "EIO", Outcome: "unreadable"}})
	m.mu.Unlock()
	if err == nil || result.Stats.IssuesComplete || result.Stats.IssueWarning == "" || result.Stats.SavedIssueCount != 0 {
		t.Fatalf("hidden persistence failure: %+v / %v", result.Stats, err)
	}
}

func TestLegacyScanReportIsNeverUpgradedToConfirmedUnreadable(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := issueTestJob(m)
	r := job.Result.(*ScanResult)
	r.Version = 0
	r.Stats.BadRanges = []BadRange{{Offset: 0, Length: 512, Code: "EIO"}}
	page, err := m.scanIssues(job.ID, 0, 50)
	if err != nil || !page.Legacy || page.Complete || page.Records[0].Outcome != "" {
		t.Fatalf("legacy wrongly classified: %+v / %v", page, err)
	}
	stream, err := m.prepareScanReport(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := stream(&output); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(output.Bytes()) || !strings.Contains(output.String(), `"recordsComplete":false`) {
		t.Fatalf("legacy export claimed complete: %s", output.String())
	}
}

func TestScanIssueAPIProtectsRecordsAndChecksPagination(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := issueTestJob(m)
	server := newTestServer(t, m, apiServerOptions{})
	defer server.Close()
	url := "/api/jobs/" + job.ID + "/scan-issues"
	status, body := doRequest(server, "GET", url, "", nil)
	assertAPIError(t, status, body, 403, "ADMIN_REQUIRED")
	headers := map[string]string{"X-Trim-Isadmin": "true"}
	for _, query := range []string{"?limit=0", "?limit=201", "?offset=-1", "?offset=1", "?offset=0&offset=0", "?limit="} {
		status, body = doRequest(server, "GET", url+query, "", headers)
		assertAPIError(t, status, body, 400, "INVALID_ARGUMENT")
	}
	status, body = doRequest(server, "GET", url, "", headers)
	if status != 200 || body["issues"] == nil {
		t.Fatalf("page unavailable: %d %v", status, body)
	}
	status, body = doRequest(server, "GET", "/api/jobs/"+job.ID+"/scan-report", "", headers)
	if status != 200 || body["recordsComplete"] != true {
		t.Fatalf("invalid export: %d %v", status, body)
	}
	if _, err := m.scanIssues("../escape", 0, 50); err == nil {
		t.Fatal("accepted unsafe ID")
	}
}

func TestCorruptScanJournalCannotProduceCompleteReport(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := issueTestJob(m)
	result := job.Result.(*ScanResult)
	result.Stats.IssueCount, result.Stats.SavedIssueCount = 2, 2
	path := filepath.Join(m.jobsDir, fmt.Sprintf("%s.scan-issues.jsonl", job.ID))
	if err := os.WriteFile(path, []byte("{\"offset\":0,\"length\":512,\"code\":\"EIO\"}\n{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.scanIssues(job.ID, 0, 50); err == nil {
		t.Fatal("accepted corrupt journal")
	}
	stream, err := m.prepareScanReport(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := stream(&output); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(output.Bytes()) || !strings.Contains(output.String(), `"recordsComplete":false`) {
		t.Fatalf("corrupt export claimed complete: %s", output.String())
	}
}

func TestRestartPreservesObservedIssuesWhenJournalLosesCompleteRecords(t *testing.T) {
	for _, saved := range []int64{1, 2} {
		t.Run(fmt.Sprintf("saved_%d", saved), func(t *testing.T) {
			m := newTestManager(t, JobManagerOptions{})
			job := issueTestJob(m)
			result := job.Result.(*ScanResult)
			result.Stats.IssueCount, result.Stats.SavedIssueCount = 2, saved
			result.Stats.UnreadableRanges, result.Stats.UnreadableBytes = 2, 1024
			result.Stats.IssuesComplete = saved == 2
			if saved == 1 {
				result.Stats.IssueWarning = "完整异常记录保存失败"
			}
			record := BadRange{Offset: 0, Length: 512, Code: "EIO", Outcome: "unreadable"}
			encoded, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(m.scanIssuePath(job.ID), append(encoded, '\n'), 0600); err != nil {
				t.Fatal(err)
			}
			if !m.persistSafe(job, "test") {
				t.Fatal("cannot persist job")
			}
			restored := NewJobManager(m.dataDir, JobManagerOptions{Logger: silentLogger{}})
			loaded := restored.get(job.ID).Result.(*ScanResult)
			if loaded.Stats.IssuesComplete || loaded.Stats.IssueWarning == "" || loaded.Stats.IssueCount != 2 || loaded.Stats.SavedIssueCount != 1 || loaded.Stats.UnreadableRanges != 2 || loaded.Stats.UnreadableBytes != 1024 {
				t.Fatalf("lost failures or claimed completeness after restart: %+v", loaded.Stats)
			}
			stream, err := restored.prepareScanReport(job.ID)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			if err := stream(&output); err != nil {
				t.Fatal(err)
			}
			if !json.Valid(output.Bytes()) || !strings.Contains(output.String(), `"recordsComplete":false`) || !strings.Contains(output.String(), `"exportedIssueCount":1`) {
				t.Fatalf("lost journal entries hidden in export: %s", output.String())
			}
		})
	}
}
