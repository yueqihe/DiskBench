package main

import (
	"encoding/binary"
	"errors"
	"testing"
)

func fixedScanSense(key, asc byte, lba uint32) []byte {
	s := make([]byte, 18)
	s[0], s[2], s[7], s[12] = 0xf0, key, 10, asc
	binary.BigEndian.PutUint32(s[3:7], lba)
	return s
}

func TestSCSIRead16KeepsLargeAddressesAndSectorUnits(t *testing.T) {
	for _, sector := range []int64{512, 4096} {
		offset := int64(5)*1024*1024*1024*1024 + sector
		cdb, err := scsiRead16(offset, 128*1024, sector)
		if err != nil || cdb[0] != 0x88 || binary.BigEndian.Uint64(cdb[2:10]) != uint64(offset/sector) || binary.BigEndian.Uint32(cdb[10:14]) != uint32(128*1024/sector) {
			t.Fatalf("wrong READ16 address: %x %v", cdb, err)
		}
		if _, err := scsiRead16(offset+1, 4096, sector); err == nil {
			t.Fatal("unaligned offset accepted")
		}
		if _, err := scsiRead16(offset, 4095, sector); err == nil {
			t.Fatal("unaligned length accepted")
		}
	}
}

func TestScanSenseClassifiesCauseAndValidatesLBA(t *testing.T) {
	for _, tc := range []struct {
		key, asc     byte
		host, driver uint16
		want         string
	}{
		{3, 0x11, 0, 8, "MEDIUM_ERROR"}, {1, 0x17, 0, 8, "RECOVERED_ERROR"},
		{3, 0x11, 3, 8, "TIMEOUT"}, {3, 0x11, 7, 8, "TRANSPORT_ERROR"},
		{4, 0x44, 0, 8, "HARDWARE_ERROR"}, {5, 0x20, 0, 8, "ILLEGAL_REQUEST"},
		{2, 4, 0, 8, "NOT_READY"}, {6, 0x29, 0, 8, "UNIT_ATTENTION"},
		{0xb, 0x47, 0, 8, "COMMAND_ABORTED"}, {0, 0, 0, 6, "TIMEOUT"},
	} {
		e := decodeScanSense(fixedScanSense(tc.key, tc.asc, 123), 2, tc.host, tc.driver, 120, 10)
		if e == nil || e.Evidence.Kind != tc.want {
			t.Fatalf("%+v: %+v", tc, e)
		}
		if isRecoverableMediaError(e) != (tc.want == "MEDIUM_ERROR") {
			t.Fatal("non-media error treated as medium")
		}
	}
	s := fixedScanSense(3, 0x11, 123)
	if e := decodeScanSense(s, 2, 0, 8, 120, 10); e.Evidence.LBA == nil || *e.Evidence.LBA != 123 {
		t.Fatal("valid LBA lost")
	}
	if e := decodeScanSense(s, 2, 0, 8, 124, 10); e.Evidence.LBA != nil {
		t.Fatal("unrelated LBA accepted")
	}
	s[0] = 0xf1 // deferred, not tied to current command
	if e := decodeScanSense(s, 2, 0, 8, 120, 10); e.Evidence.LBA != nil || e.Evidence.Kind != "DEFERRED_ERROR" {
		t.Fatal("deferred LBA accepted")
	}
	if e := decodeScanSense(nil, 0, 0, 0, 0, 1); e != nil {
		t.Fatal("good status rejected")
	}
	if e := decodeScanSense([]byte{0x70}, 2, 0, 8, 0, 1); e == nil || e.Evidence.Kind != "COMMAND_ERROR" {
		t.Fatal("truncated sense treated as good/media")
	}
}

func TestDescriptorSenseLargeLBAAndTruncation(t *testing.T) {
	s := make([]byte, 20)
	s[0], s[1], s[2], s[7] = 0x72, 3, 0x11, 12
	s[8], s[9], s[10] = 0, 10, 0x80
	lba := int64(1<<34) + 12
	binary.BigEndian.PutUint64(s[12:20], uint64(lba))
	e := decodeScanSense(s, 2, 0, 8, lba-1, 8)
	if e.Evidence.LBA == nil || *e.Evidence.LBA != lba {
		t.Fatal("64-bit LBA truncated")
	}
	for n := 0; n < len(s); n++ {
		e = decodeScanSense(s[:n], 2, 0, 8, lba-1, 8)
		if e == nil || e.Evidence.LBA != nil {
			t.Fatalf("truncated descriptor accepted at %d", n)
		}
	}
}

func TestProtocolMediumErrorLocatedToSector(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := &Job{ID: "protocol-medium", Status: "running"}
	const bad = int64(7 * 512)
	h := scanReaderFunc(func(p []byte, off int64) (int, error) {
		if off <= bad && off+int64(len(p)) > bad {
			return 0, decodeScanSense(fixedScanSense(3, 0x11, 7), 2, 0, 8, off/512, int64(len(p))/512)
		}
		return len(p), nil
	})
	r, err := m.readRegion(h, scanBuffer(65536), 65536, 0, job, 200, 512)
	if err != nil || r.bytesRead != 65536-512 || len(r.badRanges) != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	b := r.badRanges[0]
	if b.Offset != bad || b.Length != 512 || b.Code != "MEDIUM_ERROR" || b.Evidence == nil || *b.Evidence.LBA != 7 || b.Recovered == nil || *b.Recovered {
		t.Fatalf("lost exact evidence: %+v", b)
	}
}

func TestTransportFailureNeverSplitsIntoBadSectors(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := &Job{ID: "transport", Status: "running"}
	for _, recover := range []bool{true, false} {
		calls := 0
		h := scanReaderFunc(func(p []byte, off int64) (int, error) {
			calls++
			if off != 8192 || len(p) != 65536 {
				t.Fatal("transport failure split into sector probes")
			}
			if recover && calls == 2 {
				return len(p), nil
			}
			return 0, &scanIOError{Evidence: ScanIOEvidence{Kind: "TIMEOUT"}}
		})
		r, err := m.readRegion(h, scanBuffer(65536), 65536, 8192, job, 200, 512)
		if calls != 2 || (err == nil) != recover || len(r.badRanges) != 1 || r.badRanges[0].Code != "TIMEOUT" || *r.badRanges[0].Recovered != recover || r.errorCount == 0 {
			t.Fatalf("calls=%d %+v %v", calls, r, err)
		}
	}
}

func TestFirmwareRecoveredErrorIsNotLost(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := &Job{ID: "recovered", Status: "running"}
	calls := 0
	h := scanReaderFunc(func(p []byte, off int64) (int, error) {
		calls++
		return len(p), &scanIOError{Evidence: ScanIOEvidence{Kind: "RECOVERED_ERROR"}}
	})
	r, err := m.readRegion(h, scanBuffer(4096), 4096, 0, job, 200, 512)
	if err != nil || calls != 1 || r.errorCount != 1 || r.bytesRead != 4096 || !*r.badRanges[0].Recovered {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestHDDLatencyReferenceKeepsStallsWithoutPenalizingNormalTransfer(t *testing.T) {
	b := scanLatencyReference{}
	for i := 0; i < 20; i++ {
		if warning, _ := b.observe(84, 8<<20, false); warning != "" {
			t.Fatal("normal HDD transfer flagged")
		}
	}
	if warning, ratio := b.observe(300, 8<<20, false); warning != "relative" || ratio < 3 {
		t.Fatalf("stall hidden: %s %f", warning, ratio)
	}
	if warning, _ := b.observe(900, 8<<20, false); warning != "relative-severe" {
		t.Fatal("severe stall hidden")
	}
	if warning, _ := b.observe(84, 8<<20, false); warning != "" {
		t.Fatal("outlier polluted reference")
	}
	if warning, _ := b.observe(80, 512, false); warning != "" {
		t.Fatal("tail compared against unrelated command")
	}
	if warning, _ := b.observe(600, 1<<20, false); warning != "stall" {
		t.Fatal("raw stall without baseline hidden")
	}
	cell := &ScanCell{Status: "normal"}
	applyHDDLatency(cell, "relative", 4)
	if cell.Status != "slow" {
		t.Fatal("relative warning invisible")
	}
	cell.Status = "normal"
	applyHDDLatency(cell, "", 1)
	if cell.Status != "slow" {
		t.Fatal("later normal read erased anomaly")
	}
}

func TestFullScanCoversEveryByteIncludingFinalSector(t *testing.T) {
	disk := testDisk()
	disk.Size = 2*fullScanReadSize + 512
	position := int64(0)
	h := &scanTestHandle{read: func(p []byte, off int64) (int, error) {
		if off != position || off+int64(len(p)) > disk.Size || len(p)%512 != 0 {
			t.Fatalf("gap, repeat, or overrun: %d %d expected %d", off, len(p), position)
		}
		position += int64(len(p))
		return len(p), nil
	}}
	m := newTestManager(t, JobManagerOptions{ResolveDisk: func(string) (*Disk, error) { return disk, nil }, Open: func(string) (ReadHandle, error) { return h, nil }})
	job := &Job{ID: "12345678-1234-4123-8123-123456789abc", Type: "scan", Status: "queued", Device: disk.Path,
		Disk: DiskSnapshot{Size: disk.Size, Model: disk.Model, Serial: disk.Serial, LogicalSectorSize: 512}, Result: &ScanResult{ScanMode: "full", Coverage: "full", Map: createScanMap(disk.Size, 2, 512)}}
	m.jobs[job.ID] = job
	if err := m.runScan(job, "full"); err != nil {
		t.Fatal(err)
	}
	if position != disk.Size || job.Result.(*ScanResult).Stats.BytesRead != disk.Size {
		t.Fatal("tail omitted")
	}
}

func TestFailureDuringLocationRetainsProtocolEvidence(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := &Job{ID: "locate-link", Status: "running"}
	calls := 0
	h := scanReaderFunc(func(p []byte, off int64) (int, error) {
		calls++
		kind := "MEDIUM_ERROR"
		if calls > 1 {
			kind = "TRANSPORT_ERROR"
		}
		return 0, &scanIOError{Evidence: ScanIOEvidence{Kind: kind}}
	})
	r, err := m.readRegion(h, scanBuffer(65536), 65536, 0, job, 200, 512)
	var protocol *scanIOError
	if !errors.As(err, &protocol) || len(r.badRanges) == 0 || r.badRanges[0].Code != "TRANSPORT_ERROR" || calls != 2 {
		t.Fatalf("%+v %v calls=%d", r, err, calls)
	}
}

func TestStoppedFullScanPublishesFailingCommand(t *testing.T) {
	disk := testDisk()
	disk.Size = 16 * 1024 * 1024
	calls := 0
	h := &scanTestHandle{read: func(p []byte, off int64) (int, error) {
		calls++
		return 0, &scanIOError{Evidence: ScanIOEvidence{Kind: "TIMEOUT", HostStatus: 3}}
	}}
	m := newTestManager(t, JobManagerOptions{ResolveDisk: func(string) (*Disk, error) { return disk, nil }, Open: func(string) (ReadHandle, error) { return h, nil }})
	job := &Job{ID: "12345678-1234-4123-8123-123456789abc", Type: "scan", Status: "queued", Device: disk.Path,
		Disk:   DiskSnapshot{Size: disk.Size, Model: disk.Model, Serial: disk.Serial, LogicalSectorSize: 512},
		Result: &ScanResult{ScanMode: "full", Coverage: "full", Map: createScanMap(disk.Size, 2, 512)}}
	m.jobs[job.ID] = job
	err := m.runScan(job, "full")
	if err == nil {
		t.Fatal("timeout scan completed")
	}
	m.fail(job, err)
	r := job.Result.(*ScanResult)
	if calls != 2 || job.Status != "failed" || r.Map[0].Status != "error" || r.Map[1].Status != "pending" || r.Stats.ErrorCells != 1 || len(r.Stats.BadRanges) != 1 || r.Stats.BadRanges[0].Code != "TIMEOUT" {
		t.Fatalf("calls=%d job=%s result=%+v", calls, job.Status, r)
	}
}

func TestTransientMediumSenseIsPreservedWhenChildrenReadClean(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := &Job{ID: "transient-medium", Status: "running"}
	calls := 0
	h := scanReaderFunc(func(p []byte, off int64) (int, error) {
		calls++
		if calls <= 2 {
			return 0, decodeScanSense(fixedScanSense(3, 0x11, 7), 2, 0, 8, off/512, int64(len(p))/512)
		}
		return len(p), nil
	})
	r, err := m.readRegion(h, scanBuffer(65536), 65536, 0, job, 200, 512)
	if err != nil || r.bytesRead != 65536 || r.errorCount == 0 || len(r.badRanges) != 1 || r.badRanges[0].Code != "MEDIUM_ERROR" || !*r.badRanges[0].Recovered {
		t.Fatalf("%+v %v", r, err)
	}
}
