package main

import (
	"strings"
	"syscall"
	"testing"
)

func TestScanRangePrecisionRequiresRecordedSectorEvidence(t *testing.T) {
	confirmed := BadRange{Offset: 4096, Length: 4096, ProbeBytes: 4096, Outcome: "unreadable", Code: "EIO", Recovered: boolPtr(false)}
	for _, tc := range []struct {
		name   string
		edit   func(*BadRange)
		sector int64
		want   string
	}{
		{"4Kn", nil, 4096, "sector"},
		{"512B-sector-range", nil, 512, "range"},
		{"64KiB-range", func(r *BadRange) { r.Length, r.ProbeBytes = 65536, 65536 }, 4096, "range"},
		{"unknown-sector", nil, 0, "unknown"},
		{"missing-probe", func(r *BadRange) { r.ProbeBytes = 0 }, 4096, "unknown"},
		{"missing-retry-result", func(r *BadRange) { r.Recovered = nil }, 4096, "unknown"},
		{"inconsistent-range", func(r *BadRange) { r.Length = 8192 }, 4096, "unknown"},
		{"unaligned", func(r *BadRange) { r.Offset = 512 }, 4096, "unknown"},
		{"recovered", func(r *BadRange) { r.Outcome, r.Recovered = "recovered", boolPtr(true) }, 4096, "request"},
		{"contradictory-recovery", func(r *BadRange) { r.Recovered = boolPtr(true) }, 4096, "unknown"},
		{"device-error", func(r *BadRange) { r.Outcome, r.Code = "device_error", "TIMEOUT" }, 4096, "request"},
		{"legacy", func(r *BadRange) { r.Outcome = "" }, 4096, "unknown"},
		{"limit", func(r *BadRange) { r.Outcome, r.Code = "unresolved", "LOCALIZATION_LIMIT" }, 4096, "unresolved"},
		{"skipped", func(r *BadRange) { r.Outcome, r.Code = "unresolved", "SLOW_READ_LIMIT" }, 4096, "unresolved"},
		{"slow", func(r *BadRange) { r.Outcome, r.Code = "severe", "PERSISTENT_SLOW_READ" }, 4096, "request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := confirmed
			if tc.edit != nil {
				tc.edit(&r)
			}
			if got := scanRangePrecision(r, tc.sector); got != tc.want {
				t.Fatalf("precision=%s want=%s range=%+v", got, tc.want, r)
			}
		})
	}
}

func TestScanLocationRecordsActualTailProbe(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	for _, sector := range []int64{512, 4096} {
		h := scanReaderFunc(func(p []byte, offset int64) (int, error) { return 0, syscall.EIO })
		ranges, read, err := m.locateReadErrors(h, 4096, sector, &Job{ID: "tail", Status: "running"}, 200, 65536)
		if err != nil || read != 0 || len(ranges) != 1 || ranges[0].ProbeBytes != sector || scanRangePrecision(ranges[0], sector) != "sector" {
			t.Fatalf("tail precision lost: sector=%d ranges=%+v read=%d err=%v", sector, ranges, read, err)
		}
	}
}

func TestHumanReportDistinguishesSectorRangeAndUnresolvedAddresses(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := humanReportJob(m)
	job.Disk.LogicalSectorSize = 4096
	saveDisplayRecords(t, m, job,
		BadRange{Offset: 4096, Length: 4096, ProbeBytes: 4096, Outcome: "unreadable", Code: "EIO", Recovered: boolPtr(false)},
		BadRange{Offset: 65536, Length: 65536, ProbeBytes: 65536, Outcome: "unreadable", Code: "EIO", Recovered: boolPtr(false)},
		unresolvedProbeRange(131072, 65536),
		BadRange{Offset: 196608, Length: 4096, Outcome: "unreadable", Code: "EIO"},
	)
	body := renderHumanReport(t, m, job)
	for _, want := range []string{
		"LBA 1–1", "LBA 16–31", "LBA 32–47", "逻辑扇区 4096 B",
		`class="location-precision">逐扇区确认<br>实际定位单位 4096 B`,
		`class="location-precision">范围确认（未逐扇区）<br>实际定位单位 65536 B`,
		`class="location-precision">待定位（未确认坏扇区）`,
		`class="location-precision">定位精度未记录`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("report missing %q", want)
		}
	}
	if strings.Count(body, `class="location-precision">逐扇区确认`) != 1 {
		t.Fatal("report claimed unverified sector precision")
	}
	row := reportIssue(BadRange{Offset: 4096, Length: 4096, Code: "EIO", Outcome: "unreadable"}, 0, 0)
	if row.Address != "字节 4096–8191" || strings.Contains(row.Size, "逻辑扇区") || row.Precision != "定位精度未记录" {
		t.Fatalf("unknown sector size fabricated LBA: %+v", row)
	}
}
