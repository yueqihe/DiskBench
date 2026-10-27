package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"mime"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func humanReportJob(m *JobManager) *Job {
	job := issueTestJob(m)
	job.Disk.Model, job.Disk.Serial, job.Disk.Rotational = "WDC WD120EFBX", "DISK-SERIAL-123", true
	job.CreatedAt = "2026-10-15T08:00:00+08:00"
	start, finish := job.CreatedAt, "2026-10-15T08:12:34+08:00"
	job.StartedAt, job.FinishedAt, job.Progress = &start, &finish, 100
	r := job.Result.(*ScanResult)
	r.ScanMode = "full"
	r.Stats.CoveredBytes, r.Stats.BytesRead, r.Stats.TestedCells = job.Disk.Size, job.Disk.Size, 2
	return job
}

func renderHumanReport(t *testing.T, m *JobManager, job *Job) string {
	t.Helper()
	stream, filename, err := m.prepareScanHTMLReport(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(filename, ".html") {
		t.Fatalf("not a human-readable report: %s", filename)
	}
	var output bytes.Buffer
	if err := stream(&output); err != nil {
		t.Fatal(err)
	}
	return output.String()
}

func TestHumanReportUsesMediaBandsAndRetainsLegacyClassification(t *testing.T) {
	for _, tc := range []struct {
		basis string
		ms    float64
		want  string
	}{
		{"hdd-dual-v4", 199.99, "slow"}, {"hdd-dual-v4", 200, "slower"},
		{"solid-dual-v4", 49.99, "slow"}, {"solid-dual-v4", 50, "slower"},
		{"hdd-dual-v3", 175, "slower"}, {"solid-dual-v2", 100, "slow"},
	} {
		cell := ScanCell{Status: "slow", MaxUnitMs: &tc.ms}
		if got := reportCellGroup(cell, tc.basis); got != tc.want {
			t.Errorf("%s at %v ms: %s, want %s", tc.basis, tc.ms, got, tc.want)
		}
	}
	for _, hdd := range []bool{false, true} {
		m := newTestManager(t, JobManagerOptions{})
		job := humanReportJob(m)
		job.Disk.Rotational = hdd
		job.Result.(*ScanResult).LatencyBasis = scanLatencyBasis(hdd)
		body := html.UnescapeString(renderHumanReport(t, m, job))
		want := "一般 20–<50 ms；较差 50–<500 ms；严重 ≥500 ms（红色）"
		if hdd {
			want = "一般 50–<200 ms；较差 200–<800 ms；严重 ≥800 ms（红色）"
		}
		if !strings.Contains(body, want) {
			t.Fatalf("media thresholds missing from report: %q", want)
		}
	}
}

func TestHumanReportIncludesDiskSnapshotAndMergedRepairResult(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := humanReportJob(m)
	job.Disk.Model += `<script>alert("model")</script>`
	job.Disk.Serial = `<img src=x onerror=alert(1)>`
	job.Options.ScanRepair = boolPtr(true)
	r := job.Result.(*ScanResult)
	r.Diagnostics = &ScanDiagnostics{
		Before: &ScanEvidence{Available: true, Status: "passed", Counters: map[string]int64{"pending": 2}},
		After:  &ScanEvidence{Available: true, Status: "passed", Counters: map[string]int64{"pending": 3, "reported": 141741719552}},
	}
	final := repairDisplayRecord(512, "verified", "512-512.bin")
	final.Repair.Detail = `<script>alert("detail")</script>`
	saveDisplayRecords(t, m, job, repairDisplayRecord(512, "pending", "512-512.bin"), final)
	stream, _, err := m.prepareScanHTMLReport(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	job.Disk.Model, job.Disk.Serial = "CHANGED-LIVE-DISK", "CHANGED-LIVE-SERIAL"
	var output bytes.Buffer
	if err := stream(&output); err != nil {
		t.Fatal(err)
	}
	body := output.String()
	for _, want := range []string{"<!doctype html>", "硬盘扫描检测报告", "全盘扫描", "WDC WD120EFBX", "1048576 字节", "1.00 MiB", "逻辑 / 物理扇区", "12:34", "0 小时 12 分 34 秒", "LBA 1–1", "写回校验通过", "1296.0 ms", "0.8 ms", "待处理扇区", "+1", "141741719552", "无法比较", "打印 / 保存为 PDF", "异常记录已完整读取", "&lt;script&gt;", "&lt;img"} {
		if !strings.Contains(body, want) && !strings.Contains(html.UnescapeString(body), want) {
			t.Errorf("report missing %q", want)
		}
	}
	for _, unwanted := range []string{`<script>alert`, `<img src=x`, "CHANGED-LIVE", "已备份，等待最终结果", `"reportVersion"`, `"map":[`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("report exposed unsafe, duplicate, or raw content: %q", unwanted)
		}
	}
}

func TestHumanReportIncludesCompleteJournalBeyondPreview(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := humanReportJob(m)
	ranges := make([]BadRange, 405)
	for i := range ranges {
		ranges[i] = BadRange{Offset: int64(i) * 512, Length: 512, Code: "EIO", Outcome: "unreadable"}
	}
	saveDisplayRecords(t, m, job, ranges...)
	body := renderHumanReport(t, m, job)
	for _, want := range []string{"LBA 404–404", "已列出 405 条明细", "坏块（不可读）", "异常记录已完整读取"} {
		if !strings.Contains(body, want) {
			t.Fatalf("complete report lost %q", want)
		}
	}
}

func TestHumanReportLabelsPartialSampledLegacyAndMockResults(t *testing.T) {
	for _, kind := range []string{"interrupted", "sampled", "legacy", "mock", "selftest-failed", "truncated", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			m := newTestManager(t, JobManagerOptions{})
			job := humanReportJob(m)
			r := job.Result.(*ScanResult)
			want := ""
			switch kind {
			case "interrupted":
				job.Status, job.Progress, job.FinishedAt = "interrupted", 50, nil
				want = "扫描尚未完成"
			case "sampled":
				r.ScanMode, r.Coverage = "quick", "sampled"
				want = "不代表逐字节读过整盘"
			case "legacy":
				r.Version = 0
				r.Stats.BadRanges = []BadRange{{Offset: 0, Length: 512, Code: "EIO"}}
				want = "旧记录仅保留当时的摘要"
			case "mock":
				r.Diagnostics = &ScanDiagnostics{ReadMethod: "mock"}
				want = "本报告未对真实硬盘执行检测"
			case "selftest-failed":
				r.Diagnostics = &ScanDiagnostics{SelfTest: &NVMeSelfTestResult{State: "failed", Message: "设备报告自检失败"}}
				want = "NVMe 固件自检未通过"
			case "truncated", "corrupt":
				saveDisplayRecords(t, m, job, BadRange{Offset: 0, Length: 512, Code: "EIO", Outcome: "unreadable"})
				r.Stats.IssueCount, r.Stats.SavedIssueCount = 2, 2
				if kind == "corrupt" {
					f, err := os.OpenFile(m.scanIssuePath(job.ID), os.O_APPEND|os.O_WRONLY, 0600)
					if err != nil {
						t.Fatal(err)
					}
					_, err = f.WriteString("{broken\n")
					f.Close()
					if err != nil {
						t.Fatal(err)
					}
				}
				want = "异常记录不完整"
			}
			body := renderHumanReport(t, m, job)
			if !strings.Contains(body, want) || !strings.Contains(body, "</body></html>") {
				t.Fatalf("lost report state %q", want)
			}
			if kind == "truncated" || kind == "corrupt" || kind == "legacy" {
				if strings.Contains(body, "异常记录已完整读取") {
					t.Fatal("partial report claimed complete")
				}
			}
		})
	}
}

func TestScanReportAPIDefaultsToDownloadableHTMLAndKeepsRawJSON(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := humanReportJob(m)
	server := newTestServer(t, m, apiServerOptions{})
	defer server.Close()
	url := "/api/jobs/" + job.ID + "/scan-report"
	status, body := doRequest(server, "GET", url, "", nil)
	assertAPIError(t, status, body, 403, "ADMIN_REQUIRED")
	for _, format := range []string{"", "?format=html", "?format=json"} {
		req := httptest.NewRequest("GET", url+format, nil)
		req.Header.Set("X-Trim-Isadmin", "true")
		w := httptest.NewRecorder()
		server.Config.Handler.ServeHTTP(w, req)
		_, params, err := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
		if err != nil || w.Code != 200 || !strings.Contains(params["filename"], "DiskBench") && format != "?format=json" {
			t.Fatalf("report download unavailable: %d %v %v", w.Code, params, err)
		}
		if format == "?format=json" {
			if !json.Valid(w.Body.Bytes()) || !strings.HasSuffix(params["filename"], ".json") {
				t.Fatal("raw export changed")
			}
		} else if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || !strings.HasSuffix(params["filename"], ".html") || !strings.Contains(w.Body.String(), job.Disk.Model) || w.Header().Get("Content-Security-Policy") != scanReportCSP() {
			t.Fatal("human report headers or metadata missing")
		}
	}
	for _, query := range []string{"?format=", "?format=pdf", "?format=html&format=json"} {
		status, body := doRequest(server, "GET", url+query, "", map[string]string{"X-Trim-Isadmin": "true"})
		assertAPIError(t, status, body, 400, "INVALID_ARGUMENT")
	}
	status, body = doRequest(server, "GET", fmt.Sprintf("/api/jobs/%s/scan-report", "12345678-1234-4123-8123-000000000000"), "", map[string]string{"X-Trim-Isadmin": "true"})
	assertAPIError(t, status, body, 404, "JOB_NOT_FOUND")
}
