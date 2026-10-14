package main

import (
	"encoding/binary"
	"errors"
	"testing"
)

func TestNVMeStatusClassificationAndDNR(t *testing.T) {
	for _, tc := range []struct {
		status uint16
		kind   string
		local  bool
	}{
		{0x281, "MEDIUM_ERROR", true}, {0x4281, "MEDIUM_ERROR", true},
		{0x282, "DATA_INTEGRITY_ERROR", true}, {0x284, "DATA_INTEGRITY_ERROR", true},
		{0x287, "UNWRITTEN_LBA", false}, {0x286, "ACCESS_DENIED", false},
		{0x370, "TRANSPORT_ERROR", false}, {6, "CONTROLLER_ERROR", false},
		{0x82, "NOT_READY", false}, {2, "ILLEGAL_REQUEST", false},
	} {
		e := decodeNVMeScanStatus(tc.status, 7)
		if e == nil || e.Evidence.Kind != tc.kind || localizableScanIssue(e) != tc.local || e.Evidence.NSID != 7 || e.Evidence.DoNotRetry != (tc.status&0x4000 != 0) {
			t.Fatalf("%x: %+v", tc.status, e)
		}
		if e.Evidence.StatusCode != byte(tc.status) || e.Evidence.StatusCodeType != byte(tc.status>>8&7) {
			t.Fatal("status was phase-shifted twice")
		}
	}
	if decodeNVMeScanStatus(0, 7) != nil {
		t.Fatal("success marked failed")
	}
}

func TestNVMeReadWordsUse64BitLBAAndZeroBasedCount(t *testing.T) {
	for _, sector := range []int64{512, 4096} {
		lba := int64(1<<32) + 17
		lo, hi, nlb, err := nvmeReadWords(lba*sector, int(sector), sector)
		if err != nil || lo != 17 || hi != 1 || nlb != 0 {
			t.Fatalf("%x %x %d %v", lo, hi, nlb, err)
		}
		_, _, nlb, err = nvmeReadWords(0, int(sector)*65536, sector)
		if err != nil || nlb != 65535 {
			t.Fatal("maximum block count")
		}
		if _, _, _, err = nvmeReadWords(1, 4096, sector); err == nil {
			t.Fatal("unaligned command")
		}
	}
}

func nvmeIdentifyFixture() ([]byte, []byte) {
	ctrl, ns := make([]byte, 4096), make([]byte, 4096)
	ctrl[77] = 1
	binary.LittleEndian.PutUint16(ctrl[256:258], 0x10)
	binary.LittleEndian.PutUint16(ctrl[316:318], 12)
	binary.LittleEndian.PutUint64(ns[:8], 64*1024*1024/512)
	ns[130] = 9
	return ctrl, ns
}

func TestNVMeIdentifyGeometryMetadataAndMDTS(t *testing.T) {
	ctrl, ns := nvmeIdentifyFixture()
	info, err := parseNVMeIdentify(ctrl, ns, 3, 512, 64*1024*1024)
	if err != nil || info.MaxReadBytes != 8192 || !info.SelfTestSupported || info.ExtendedMinutes != 12 || info.NSID != 3 {
		t.Fatalf("%+v %v", info, err)
	}
	if _, err = parseNVMeIdentify(ctrl, ns, 3, 4096, 64*1024*1024); err == nil {
		t.Fatal("wrong logical sector accepted")
	}
	if _, err = parseNVMeIdentify(ctrl, ns, 3, 512, 32*1024*1024); err == nil {
		t.Fatal("wrong capacity accepted")
	}
	ns[128] = 8
	if _, err = parseNVMeIdentify(ctrl, ns, 3, 512, 64*1024*1024); !errors.Is(err, errNVMeMetadata) {
		t.Fatal("metadata format not delegated to kernel")
	}
	ctrl, ns = nvmeIdentifyFixture()
	ns[25], ns[26] = 17, 0x21 // format 17, including the extended index bit
	ns[128+17*4+2] = 12
	binary.LittleEndian.PutUint64(ns[:8], 64*1024*1024/4096)
	if _, err = parseNVMeIdentify(ctrl, ns, 3, 4096, 64*1024*1024); err != nil {
		t.Fatal(err)
	}
}

func TestNVMeLogLocationsRequireMatchingNamespaceAndMediaStatus(t *testing.T) {
	disk := *testDisk()
	disk.Size = 1024 * 1024
	disk.LogicalSectorSize = 512
	disk.NVMeNamespaceID = 2
	output := `{"smartctl":{"exit_status":0},"nvme_optional_admin_commands":{"self_test":true},
	"nvme_error_information_log":{"logged_count":8,"table":[
	 {"nsid":2,"lba":{"value":0},"status_field":{"status_code_type":2,"status_code":129}},
	 {"nsid":1,"lba":{"value":15},"status_field":{"status_code_type":2,"status_code":129}},
	 {"nsid":2,"lba":{"value":16},"status_field":{"status_code_type":0,"status_code":2}},
	 {"nsid":2,"lba":{"value":17},"status_field":{"status_code_type":2,"status_code":135}},
	 {"nsid":2,"lba":{"value":18446744073709551615},"status_field":{"status_code_type":2,"status_code":129}},
	 {"lba":{"value":22},"status_field":{"status_code_type":2,"status_code":129}}
	]},"nvme_self_test_log":{"table":[
	 {"nsid":2,"lba":33,"self_test_result":{"value":7}},
	 {"nsid":-1,"lba":34,"self_test_result":{"value":7}},
	 {"nsid":2,"lba":35,"self_test_result":{"value":0}},
	 {"nsid":2,"self_test_result":{"value":7}}
	]}}`
	r := parseScanEvidence(output, disk)
	if len(r.LogHints) != 2 || r.LogHints[0].LBA != 0 || r.LogHints[1].LBA != 33 || r.LogHints[0].NSID != 2 || r.NVMeSelfTestSupported == nil || !*r.NVMeSelfTestSupported {
		t.Fatalf("%+v", r)
	}
	disk.NVMeNamespaceID = 0
	r = parseScanEvidence(output, disk)
	if len(r.LogHints) != 0 || r.Counters["nvmeLog"] != 8 {
		t.Fatal("guessed namespace or discarded global counters")
	}
}

func TestNVMeUnwrittenAndDNRDoNotBecomeSectorSearch(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := &Job{ID: "nvme-unwritten", Status: "running"}
	for _, status := range []uint16{0x4287, 0x4282} {
		calls := 0
		h := scanReaderFunc(func(p []byte, off int64) (int, error) { calls++; return 0, decodeNVMeScanStatus(status, 2) })
		r, err := m.readRegion(h, scanBuffer(512), 512, 0, job, 200, 512)
		if calls != 1 || len(r.badRanges) != 1 || r.badRanges[0].Evidence.NSID != 2 {
			t.Fatalf("calls=%d %+v", calls, r)
		}
		if status == 0x4287 && (err == nil || r.badRanges[0].Code != "UNWRITTEN_LBA") {
			t.Fatal("unwritten misclassified")
		}
		if status == 0x4282 && (err != nil || r.badRanges[0].Code != "DATA_INTEGRITY_ERROR") {
			t.Fatal("integrity error not localized")
		}
	}
}

func selfTestFixture(operation, result, valid byte, nsid uint32, lba uint64) []byte {
	b := make([]byte, 564)
	b[0] = operation
	b[1] = 45
	b[4] = 0x20 | result
	b[6] = valid
	binary.LittleEndian.PutUint32(b[16:20], nsid)
	binary.LittleEndian.PutUint64(b[20:28], lba)
	return b
}

func TestNVMeSelfTestValidityAndNamespace(t *testing.T) {
	b := selfTestFixture(0, 7, 3, 2, 0)
	r, err := parseNVMeSelfTestLog(b, 2, 2048)
	if err != nil || r.State != "failed" || r.LBA == nil || *r.LBA != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	b[6] = 1
	r, _ = parseNVMeSelfTestLog(b, 2, 2048)
	if r.LBA != nil {
		t.Fatal("invalid LBA bit ignored")
	}
	b = selfTestFixture(0, 7, 3, 3, 10)
	r, _ = parseNVMeSelfTestLog(b, 2, 2048)
	if r.LBA != nil || r.State != "unknown" {
		t.Fatal("other namespace result used")
	}
	if _, err = parseNVMeSelfTestLog(b[:100], 2, 2048); err == nil {
		t.Fatal("truncated self-test log")
	}
}

type fakeNVMeSelfTester struct {
	logs                    [][]byte
	reads, starts, finished int
	beforeLog               func()
}

func (h *fakeNVMeSelfTester) ReadAt(p []byte, off int64) (int, error) { return len(p), nil }
func (h *fakeNVMeSelfTester) Close() error                            { return nil }
func (h *fakeNVMeSelfTester) nvmeInfo() NVMeScanInfo {
	return NVMeScanInfo{NSID: 2, SelfTestSupported: true}
}
func (h *fakeNVMeSelfTester) selfTestLog() ([]byte, error) {
	if h.beforeLog != nil {
		h.beforeLog()
	}
	i := min(h.reads, len(h.logs)-1)
	h.reads++
	return h.logs[i], nil
}
func (h *fakeNVMeSelfTester) startSelfTest() error { h.starts++; return nil }
func (h *fakeNVMeSelfTester) finishSelfTest()      { h.finished++ }

func TestNVMeSelfTestDoesNotAcceptOldPassOrInterruptOthers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		logs    [][]byte
		wantErr bool
		starts  int
	}{
		{"busy", [][]byte{selfTestFixture(2, 0, 0, 0, 0)}, true, 0},
		{"stale", [][]byte{selfTestFixture(0, 0, 0, 0, 0)}, true, 1},
		{"different-test", [][]byte{selfTestFixture(0, 0, 0, 0, 0), selfTestFixture(1, 0, 0, 0, 0)}, true, 1},
		{"pass", [][]byte{selfTestFixture(0, 0, 0, 0, 0), selfTestFixture(2, 0, 0, 0, 0), selfTestFixture(0, 0, 0, 0, 0)}, false, 1},
		{"fault", [][]byte{selfTestFixture(0, 0, 0, 0, 0), selfTestFixture(0, 7, 3, 2, 12)}, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager(t, JobManagerOptions{})
			job := &Job{ID: tc.name, Status: "running", Disk: DiskSnapshot{Size: 1024 * 1024, LogicalSectorSize: 512}, Result: &ScanResult{}}
			h := &fakeNVMeSelfTester{logs: tc.logs}
			err := m.runNVMeSelfTest(job, h)
			if (err != nil) != tc.wantErr || h.starts != tc.starts {
				t.Fatalf("%v starts=%d", err, h.starts)
			}
			if tc.name == "fault" && job.Result.(*ScanResult).Diagnostics.SelfTest.State != "failed" {
				t.Fatal("firmware failure lost")
			}
		})
	}
}

func TestNVMeSelfTestCancellationDoesNotReportPass(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := &Job{ID: "nvme-cancel", Status: "running", Disk: DiskSnapshot{Size: 1024 * 1024, LogicalSectorSize: 512}, Result: &ScanResult{}}
	c := m.beginCancellation(job)
	defer m.endCancellation(job, c)
	h := &fakeNVMeSelfTester{logs: [][]byte{selfTestFixture(0, 0, 0, 0, 0)}}
	h.beforeLog = func() {
		if h.reads > 0 {
			c.cancel()
		}
	}
	if err := m.runNVMeSelfTest(job, h); err != errCancelled {
		t.Fatalf("%v", err)
	}
	if h.finished != 0 {
		t.Fatal("cancellation accepted an old result")
	}
}

func TestNVMeFirmwareTestIsStrictlyOptInDuringScan(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		disk := testDisk()
		disk.Size = 16 * 1024 * 1024
		h := &fakeNVMeSelfTester{logs: [][]byte{selfTestFixture(0, 0, 0, 0, 0), selfTestFixture(2, 0, 0, 0, 0), selfTestFixture(0, 0, 0, 0, 0)}}
		m := newTestManager(t, JobManagerOptions{ResolveDisk: func(string) (*Disk, error) { return disk, nil }, Open: func(string) (ReadHandle, error) { return h, nil }})
		job := &Job{ID: "12345678-1234-4123-8123-123456789abc", Type: "scan", Status: "queued", Device: disk.Path,
			Disk:   DiskSnapshot{Size: disk.Size, Model: disk.Model, Serial: disk.Serial, LogicalSectorSize: 512},
			Result: &ScanResult{ScanMode: "full", Coverage: "full", Map: createScanMap(disk.Size, 2, 512)}}
		if enabled {
			job.Options.ScanSelfTest = boolPtr(true)
		}
		m.jobs[job.ID] = job
		if err := m.runScan(job, "full"); err != nil {
			t.Fatal(err)
		}
		want := 0
		if enabled {
			want = 1
		}
		if h.starts != want || job.Status != "completed" || job.Result.(*ScanResult).Stats.BytesRead != disk.Size {
			t.Fatalf("enabled=%t starts=%d status=%s", enabled, h.starts, job.Status)
		}
	}
}

func TestNVMeSelfTestAPIRejectsAmbiguousOrWrongTaskOptions(t *testing.T) {
	for _, value := range []interface{}{"true", 1, []bool{true}} {
		if err := validateCreateJobBody(map[string]interface{}{"type": "scan", "device": "/dev/nvme0n1", "scanSelfTest": value}); err == nil {
			t.Fatalf("accepted %v", value)
		}
	}
	if err := validateCreateJobBody(map[string]interface{}{"type": "speed", "device": "/dev/nvme0n1", "scanSelfTest": true}); err == nil {
		t.Fatal("selftest accepted for benchmark")
	}
}
