package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type silentLogger struct{}

func TestReleaseStaticScriptsFollowCSPAndRefreshUnversionedFiles(t *testing.T) {
	public := t.TempDir()
	if err := os.Mkdir(filepath.Join(public, "assets"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"index.html": "<!doctype html><script src=\"/app/diskbench/theme-init.js\"></script>",
		"theme-init.js": "document.documentElement.dataset.theme = 'dark';",
		"assets/index-012345.js": "console.log('built');",
	} {
		if err := os.WriteFile(filepath.Join(public, filepath.FromSlash(name)), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	handler := newAPIServer(apiServerOptions{PublicDir: public})
	for _, test := range []struct{ name, cache string }{
		{"/", "no-store"}, {"/theme-init.js", "no-store"}, {"/assets/index-012345.js", "public, max-age=31536000, immutable"},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", "/app/diskbench"+test.name, nil))
		if response.Code != 200 || response.Header().Get("Cache-Control") != test.cache {
			t.Fatalf("%s: status=%d cache=%q", test.name, response.Code, response.Header().Get("Cache-Control"))
		}
		csp := response.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-inline") {
			t.Fatalf("unexpected production script policy: %s", csp)
		}
	}
}

func (silentLogger) Errorf(string, ...interface{}) {}

type fakeJobs struct {
	listFn         func(summary bool) []interface{}
	listCurrentFn  func(summary bool, includeIDs []string) []interface{}
	getFn          func(id string) *Job
	scanProgressFn func(id string, after int) (*ScanProgressSnapshot, error)
	createFn       func(typeName, devicePath string, options map[string]interface{}) (*Job, error)
	cancelFn       func(id string) (*Job, error)
	removeFn       func(id string) error
}

func (f *fakeJobs) list(summary bool) []interface{} {
	if f.listFn != nil {
		return f.listFn(summary)
	}
	return nil
}
func (f *fakeJobs) listCurrent(summary bool, includeIDs []string) []interface{} {
	if f.listCurrentFn != nil {
		return f.listCurrentFn(summary, includeIDs)
	}
	return nil
}
func (f *fakeJobs) get(id string) *Job {
	if f.getFn != nil {
		return f.getFn(id)
	}
	return nil
}
func (f *fakeJobs) scanProgress(id string, after int) (*ScanProgressSnapshot, error) {
	if f.scanProgressFn != nil {
		return f.scanProgressFn(id, after)
	}
	return nil, newJobError("任务不存在", "JOB_NOT_FOUND", 404)
}
func (f *fakeJobs) create(typeName, devicePath string, options map[string]interface{}) (*Job, error) {
	if f.createFn != nil {
		return f.createFn(typeName, devicePath, options)
	}
	return nil, errors.New("unexpected create")
}
func (f *fakeJobs) cancel(id string) (*Job, error) {
	if f.cancelFn != nil {
		return f.cancelFn(id)
	}
	return nil, errors.New("unexpected cancel")
}
func (f *fakeJobs) remove(id string) error {
	if f.removeFn != nil {
		return f.removeFn(id)
	}
	return errors.New("unexpected remove")
}
func (f *fakeJobs) shutdown() {}

func newTestServer(t *testing.T, jobs jobsStore, opts apiServerOptions) *httptest.Server {
	t.Helper()
	if jobs == nil {
		jobs = &fakeJobs{}
	}
	opts.Jobs = jobs
	if opts.Logger == nil {
		opts.Logger = silentLogger{}
	}
	if opts.Prefix == "" {
		opts.Prefix = "/app/diskbench"
	}
	if opts.LocalDev {
		opts.LocalDev = true
	}
	handler := newAPIServer(opts)
	return httptest.NewServer(handler)
}

func doRequest(s *httptest.Server, method, path, body string, headers map[string]string) (int, map[string]interface{}) {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Config.Handler.ServeHTTP(w, req)
	var parsed map[string]interface{}
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &parsed)
	}
	return w.Code, parsed
}

func assertAPIError(t *testing.T, status int, body map[string]interface{}, wantStatus int, wantCode string) {
	t.Helper()
	if status != wantStatus {
		t.Errorf("status = %d, want %d (body %v)", status, wantStatus, body)
	}
	code, _ := body["code"].(string)
	if code != wantCode {
		t.Errorf("code = %q, want %q", code, wantCode)
	}
	errMsg, _ := body["error"].(string)
	if errMsg == "" {
		t.Errorf("error message empty")
	}
}

var createHeaders = map[string]string{
	"Content-Type":     "application/json; charset=utf-8",
	"X-Requested-With": "DiskBench",
}

func TestPostJobsRejectsInvalidParams(t *testing.T) {
	s := newTestServer(t, &fakeJobs{}, apiServerOptions{LocalDev: true, MockDev: true})
	defer s.Close()
	status, body := doRequest(s, "POST", "/api/jobs", `{"type":"scan"}`, createHeaders)
	assertAPIError(t, status, body, 400, "INVALID_ARGUMENT")
}

func TestValidateResumeID(t *testing.T) {
	for _, value := range []interface{}{nil, true, "", "../../etc/passwd"} {
		if validateCreateJobBody(map[string]interface{}{"type": "scan", "device": "/dev/sda", "resumeFrom": value}) == nil {
			t.Fatalf("accepted resumeFrom %v", value)
		}
	}
	if err := validateCreateJobBody(map[string]interface{}{"type": "scan", "device": "/dev/sda", "resumeFrom": newUUID()}); err != nil {
		t.Fatal(err)
	}
	if validateCreateJobBody(map[string]interface{}{"type": "speed", "device": "/dev/sda", "resumeFrom": newUUID()}) == nil {
		t.Fatal("speed resume accepted")
	}
}

func TestPostJobsRejectsRetiredDeepScanBeforeCreatingJob(t *testing.T) {
	jobs := &fakeJobs{createFn: func(string, string, map[string]interface{}) (*Job, error) {
		t.Fatal("retired deep scan reached the job engine")
		return nil, nil
	}}
	s := newTestServer(t, jobs, apiServerOptions{LocalDev: true, MockDev: true})
	defer s.Close()
	status, body := doRequest(s, "POST", "/api/jobs", `{"type":"write","device":"/dev/sda"}`, createHeaders)
	assertAPIError(t, status, body, 410, "JOB_TYPE_RETIRED")
}

func TestValidateCreateJobBodyAcceptsExactSectorBoolean(t *testing.T) {
	for _, value := range []bool{false, true} {
		err := validateCreateJobBody(map[string]interface{}{
			"type":             "scan",
			"device":           "/dev/sda",
			"scanMode":         "full",
			"scanExactSectors": value,
		})
		if err != nil {
			t.Fatalf("scanExactSectors=%v: %v", value, err)
		}
	}
}

func TestValidateCreateJobBodyRejectsInvalidExactSectorType(t *testing.T) {
	err := validateCreateJobBody(map[string]interface{}{
		"type":             "scan",
		"device":           "/dev/sda",
		"scanMode":         "quick",
		"scanExactSectors": "true",
	})
	var jobErr *jobError
	if !errors.As(err, &jobErr) || jobErr.apiCode != "INVALID_ARGUMENT" {
		t.Fatalf("err = %v", err)
	}
}

func TestPostJobsReportsMalformedJSON(t *testing.T) {
	s := newTestServer(t, nil, apiServerOptions{LocalDev: true, MockDev: true})
	defer s.Close()
	status, body := doRequest(s, "POST", "/api/jobs", "{not-json", createHeaders)
	assertAPIError(t, status, body, 400, "INVALID_JSON")
}

func TestGetJobMissingReturns404(t *testing.T) {
	s := newTestServer(t, nil, apiServerOptions{LocalDev: true, MockDev: true})
	defer s.Close()
	status, body := doRequest(s, "GET", "/api/jobs/deadbeef", "", nil)
	assertAPIError(t, status, body, 404, "JOB_NOT_FOUND")
}

func TestGetCurrentJobsIncludesTrackedTasks(t *testing.T) {
	jobs := &fakeJobs{listCurrentFn: func(summary bool, includeIDs []string) []interface{} {
		if !summary || len(includeIDs) != 2 || includeIDs[0] != "deadbeef" || includeIDs[1] != "cafe" {
			t.Fatalf("current job query = summary %v, ids %v", summary, includeIDs)
		}
		return []interface{}{&Job{ID: "deadbeef", Status: "completed"}}
	}}
	s := newTestServer(t, jobs, apiServerOptions{LocalDev: true})
	defer s.Close()
	status, body := doRequest(s, "GET", "/api/jobs?summary=1&active=1&include=deadbeef&include=cafe", "", nil)
	listed, ok := body["jobs"].([]interface{})
	if status != 200 || !ok || len(listed) != 1 {
		t.Fatalf("current jobs response = %d, %v", status, body)
	}
}

func TestGetCurrentJobsRejectsInvalidFilters(t *testing.T) {
	s := newTestServer(t, nil, apiServerOptions{LocalDev: true})
	defer s.Close()
	for _, query := range []string{"active=bad", "active=1&active=0", "active=1&include=", "active=1&include=not-an-id"} {
		status, body := doRequest(s, "GET", "/api/jobs?"+query, "", nil)
		assertAPIError(t, status, body, 400, "INVALID_ARGUMENT")
	}
}

func TestGetScanProgressReturnsIncrementalCells(t *testing.T) {
	jobs := &fakeJobs{scanProgressFn: func(id string, after int) (*ScanProgressSnapshot, error) {
		if id != "deadbeef" || after != 3 {
			t.Fatalf("scan progress args = %q/%d", id, after)
		}
		return &ScanProgressSnapshot{ID: id, Status: "running", Progress: 0.2, Next: 4, Cells: []ScanCell{{Index: 3, Status: "under5"}}}, nil
	}}
	s := newTestServer(t, jobs, apiServerOptions{LocalDev: true, MockDev: true})
	defer s.Close()
	status, body := doRequest(s, "GET", "/api/jobs/deadbeef/scan-progress?after=3", "", nil)
	progress, _ := body["progress"].(map[string]interface{})
	if status != 200 || progress["id"] != "deadbeef" || progress["next"] != float64(4) {
		t.Fatalf("scan progress response = status %d, body %v", status, body)
	}
}

func TestGetScanProgressRejectsInvalidCursor(t *testing.T) {
	s := newTestServer(t, nil, apiServerOptions{LocalDev: true, MockDev: true})
	defer s.Close()
	status, body := doRequest(s, "GET", "/api/jobs/deadbeef/scan-progress?after=bad", "", nil)
	assertAPIError(t, status, body, 400, "INVALID_ARGUMENT")
}

func TestGetScanProgressAcceptsQuickScanFinalCursor(t *testing.T) {
	jobs := &fakeJobs{scanProgressFn: func(id string, after int) (*ScanProgressSnapshot, error) {
		if after != quickScanCellCount {
			t.Fatalf("after = %d, want %d", after, quickScanCellCount)
		}
		return &ScanProgressSnapshot{ID: id, Status: "completed", Next: after, Cells: []ScanCell{}}, nil
	}}
	s := newTestServer(t, jobs, apiServerOptions{LocalDev: true, MockDev: true})
	defer s.Close()
	status, _ := doRequest(s, "GET", "/api/jobs/deadbeef/scan-progress?after=4096", "", nil)
	if status != 200 {
		t.Fatalf("scan progress status = %d, want 200", status)
	}
	status, body := doRequest(s, "GET", "/api/jobs/deadbeef/scan-progress?after=4097", "", nil)
	assertAPIError(t, status, body, 400, "INVALID_ARGUMENT")
}

func TestDeleteJobRecord(t *testing.T) {
	removedID := ""
	jobs := &fakeJobs{removeFn: func(id string) error {
		removedID = id
		return nil
	}}
	s := newTestServer(t, jobs, apiServerOptions{LocalDev: true, MockDev: true})
	defer s.Close()
	status, body := doRequest(s, "DELETE", "/api/jobs/deadbeef", "", createHeaders)
	if status != 200 || removedID != "deadbeef" || body["deletedId"] != "deadbeef" {
		t.Fatalf("delete response = status %d, body %v, removed %q", status, body, removedID)
	}
}

func TestDeleteJobRecordRequiresRequestSource(t *testing.T) {
	s := newTestServer(t, &fakeJobs{}, apiServerOptions{LocalDev: true, MockDev: true})
	defer s.Close()
	status, body := doRequest(s, "DELETE", "/api/jobs/deadbeef", "", nil)
	assertAPIError(t, status, body, 403, "REQUEST_SOURCE_INVALID")
}

func TestUnknownAPIRouteReturns404(t *testing.T) {
	s := newTestServer(t, nil, apiServerOptions{LocalDev: true, MockDev: true})
	defer s.Close()
	status, body := doRequest(s, "GET", "/api/unknown", "", nil)
	assertAPIError(t, status, body, 404, "API_NOT_FOUND")
}

func TestJobConflictMappedTo409(t *testing.T) {
	jobs := &fakeJobs{createFn: func(string, string, map[string]interface{}) (*Job, error) {
		return nil, errors.New("已有任务正在运行，请等待完成或先取消")
	}}
	s := newTestServer(t, jobs, apiServerOptions{LocalDev: true, MockDev: true})
	defer s.Close()
	status, body := doRequest(s, "POST", "/api/jobs", `{"type":"scan","device":"/dev/sda"}`, createHeaders)
	assertAPIError(t, status, body, 409, "ACTIVE_JOB_CONFLICT")
}

func TestPostJobsRejectsOversizedBody(t *testing.T) {
	s := newTestServer(t, nil, apiServerOptions{LocalDev: true, MockDev: true})
	defer s.Close()
	oversized := `{"type":"scan","device":"/dev/sda","padding":"` + strings.Repeat("x", maxJSONBytes) + `"}`
	status, body := doRequest(s, "POST", "/api/jobs", oversized, createHeaders)
	assertAPIError(t, status, body, 413, "PAYLOAD_TOO_LARGE")
}

func TestPostJobsRequiresJSON(t *testing.T) {
	s := newTestServer(t, nil, apiServerOptions{LocalDev: true, MockDev: true})
	defer s.Close()
	headers := map[string]string{"Content-Type": "text/plain", "X-Requested-With": "DiskBench"}
	status, body := doRequest(s, "POST", "/api/jobs", `{"type":"scan","device":"/dev/sda"}`, headers)
	assertAPIError(t, status, body, 415, "UNSUPPORTED_MEDIA_TYPE")
}

func TestUnsupportedMethodReturns405(t *testing.T) {
	s := newTestServer(t, nil, apiServerOptions{LocalDev: true, MockDev: true})
	defer s.Close()
	req := httptest.NewRequest("DELETE", "/api/jobs", nil)
	w := httptest.NewRecorder()
	s.Config.Handler.ServeHTTP(w, req)
	if w.Code != 405 {
		t.Fatalf("status = %d, want 405", w.Code)
	}
	if got := w.Header().Get("Allow"); got != "GET, POST" {
		t.Errorf("Allow = %q, want %q", got, "GET, POST")
	}
}

func TestUnexpectedErrorReturnsGeneric500(t *testing.T) {
	secret := "private device exception: serial=SECRET123"
	s := newTestServer(t, nil, apiServerOptions{
		LocalDev: true,
		MockDev:  false,
		ListDisks: func(func(string) string) ([]Disk, error) {
			return nil, errors.New(secret)
		},
	})
	defer s.Close()
	status, body := doRequest(s, "GET", "/api/disks", "", nil)
	assertAPIError(t, status, body, 500, "INTERNAL_ERROR")
	if strings.Contains(string(mustJSON(body)), secret) {
		t.Error("secret leaked in response")
	}
}

func mustJSON(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}

func TestHealthReportsUnavailableWithoutLsblk(t *testing.T) {
	s := newTestServer(t, nil, apiServerOptions{
		LocalDev:       true,
		MockDev:        false,
		FindExecutable: func(string) string { return "" },
	})
	defer s.Close()
	status, body := doRequest(s, "GET", "/api/health", "", nil)
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	if ok, _ := body["ok"].(bool); ok {
		t.Errorf("ok = true, want false")
	}
	caps, _ := body["capabilities"].(map[string]interface{})
	if caps["scan"] != false || caps["dd"] != true || caps["recordedRepair"] != true {
		t.Errorf("capabilities = %v", caps)
	}
	plan, _ := body["testPlan"].(map[string]interface{})
	if plan["status"] != "provisional" || plan["scan"] != "read-only" || plan["speed"] != "temporary-file" || plan["writeTest"] != "disabled" {
		t.Errorf("testPlan = %v", plan)
	}
}

func TestSuccessfulCreationPreservesShape(t *testing.T) {
	var received map[string]interface{}
	jobs := &fakeJobs{createFn: func(typeName, devicePath string, options map[string]interface{}) (*Job, error) {
		received = map[string]interface{}{"type": typeName, "device": devicePath, "options": options}
		return &Job{ID: "new-job", Type: typeName, Device: devicePath}, nil
	}}
	s := newTestServer(t, jobs, apiServerOptions{LocalDev: true, MockDev: true})
	defer s.Close()

	expectedDisk := map[string]interface{}{"path": "/dev/sda", "serial": "ABC", "model": "Disk", "size": float64(4096)}
	payload := `{"type":"scan","device":"/dev/sda","scanMode":"full","expectedDisk":{"path":"/dev/sda","serial":"ABC","model":"Disk","size":4096}}`
	status, body := doRequest(s, "POST", "/api/jobs", payload, createHeaders)
	if status != 202 {
		t.Fatalf("status = %d, want 202 (body %v)", status, body)
	}
	job, _ := body["job"].(map[string]interface{})
	if job["id"] != "new-job" {
		t.Errorf("job id = %v", job["id"])
	}
	if received["type"] != "scan" || received["device"] != "/dev/sda" {
		t.Errorf("received = %v", received)
	}
	opts, _ := received["options"].(map[string]interface{})
	if opts["scanMode"] != "full" {
		t.Errorf("scanMode = %v", opts["scanMode"])
	}
	ed, _ := opts["expectedDisk"].(map[string]interface{})
	if ed["serial"] != expectedDisk["serial"] {
		t.Errorf("expectedDisk = %v", ed)
	}
}
