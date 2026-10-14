package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestSmartReportKeepsOriginalOutputDespiteHealthExitStatus(t *testing.T) {
	output := "smartctl 7.4\nSMART overall-health self-assessment test result: FAILED!\n  5 Reallocated_Sector_Ct    123\n<vendor text>\n"
	report, err := readSmartReport(*testDisk(), func(string) string { return "/usr/sbin/smartctl" }, func(exe string, args []string) (string, error) {
		if exe != "/usr/sbin/smartctl" || !reflect.DeepEqual(args, []string{"--xall", "--nocheck=standby", "/dev/sda"}) {
			t.Fatalf("unexpected SMART command: %s %v", exe, args)
		}
		return output, errors.New("exit status 8")
	})
	if err != nil || report.Output != output || report.Device != "/dev/sda" || report.Serial != "TEST" || report.Mock || report.Warning != "" || report.CapturedAt == "" {
		t.Fatalf("original evidence lost: %+v %v", report, err)
	}
}

func TestSmartReportUnavailableAndTimeoutKeepClearResults(t *testing.T) {
	for _, tc := range []struct {
		name, exe, output, code string
		err                     error
	}{
		{name: "missing", code: "SMART_UNAVAILABLE"},
		{name: "empty", exe: "smartctl", code: "SMART_READ_FAILED"},
		{name: "timeout", exe: "smartctl", err: context.DeadlineExceeded, code: "SMART_TIMEOUT"},
		{name: "partial timeout", exe: "smartctl", output: "partial output\n", err: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report, err := readSmartReport(*testDisk(), func(string) string { return tc.exe }, func(string, []string) (string, error) { return tc.output, tc.err })
			if tc.code != "" {
				var apiErr *jobError
				if !errors.As(err, &apiErr) || apiErr.apiCode != tc.code {
					t.Fatalf("unexpected error: %v", err)
				}
			} else if err != nil || report.Output != tc.output || report.Warning == "" {
				t.Fatalf("partial evidence not identified: %+v %v", report, err)
			}
		})
	}
}

func TestSmartReportAPIOnlyReadsAnEnumeratedPhysicalDisk(t *testing.T) {
	calls := 0
	s := newTestServer(t, nil, apiServerOptions{LocalDev: true,
		ListDisks:      func(func(string) string) ([]Disk, error) { return []Disk{*testDisk()}, nil },
		FindExecutable: func(string) string { return "smartctl" },
		SmartExecutor:  func(string, []string) (string, error) { calls++; return "original SMART output\n", nil },
	})
	defer s.Close()
	for _, path := range []string{"/api/disks/smart", "/api/disks/smart?device=/dev/sda&device=/dev/sdb", "/api/disks/smart?device=/dev/../etc/passwd", "/api/disks/smart?device=/vol1"} {
		status, body := doRequest(s, "GET", path, "", nil)
		assertAPIError(t, status, body, 400, "INVALID_ARGUMENT")
	}
	status, body := doRequest(s, "GET", "/api/disks/smart?device=/dev/sda1", "", nil)
	assertAPIError(t, status, body, 404, "DISK_NOT_FOUND")
	status, body = doRequest(s, "POST", "/api/disks/smart?device=/dev/sda", "", nil)
	assertAPIError(t, status, body, 405, "METHOD_NOT_ALLOWED")
	if calls != 0 {
		t.Fatal("invalid target reached smartctl")
	}
	status, body = doRequest(s, "GET", "/api/disks/smart?device=/dev/sda", "", nil)
	if status != 200 || body["smart"].(map[string]interface{})["output"] != "original SMART output\n" || calls != 1 {
		t.Fatalf("SMART response lost original text: %d %+v", status, body)
	}
}

func TestSmartReportAPIMockIsExplicitAndAdminOnly(t *testing.T) {
	s := newTestServer(t, nil, apiServerOptions{MockDev: true, SmartExecutor: func(string, []string) (string, error) {
		t.Fatal("mock queried real hardware")
		return "", nil
	}})
	defer s.Close()
	status, body := doRequest(s, "GET", "/api/disks/smart?device=/dev/nvme0n1", "", nil)
	assertAPIError(t, status, body, 403, "ADMIN_REQUIRED")
	status, body = doRequest(s, "GET", "/api/disks/smart?device=/dev/nvme0n1", "", map[string]string{"X-Trim-Isadmin": "true"})
	if status != 200 {
		t.Fatalf("mock report unavailable: %d %+v", status, body)
	}
	report := body["smart"].(map[string]interface{})
	if report["mock"] != true || !strings.Contains(report["output"].(string), "未查询真实硬盘") {
		t.Fatal("mock output mistaken for hardware evidence")
	}
}
