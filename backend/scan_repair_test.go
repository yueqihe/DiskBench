package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type memoryRepairHandle struct {
	data          []byte
	reads         int
	writes        atomic.Int64
	writeOffset   int64
	writeLength   int
	shortWrite    bool
	syncErr       error
	corruptVerify bool
	slowVerify    bool
	beforeWrite   func([]byte, int64)
	writeStarted  chan struct{}
	releaseWrite  chan struct{}
}

type repairScanReader struct {
	read func([]byte, int64) (int, error)
}

func (h *repairScanReader) ReadAt(p []byte, off int64) (int, error) { return h.read(p, off) }
func (h *repairScanReader) Close() error                            { return nil }

func (h *memoryRepairHandle) ReadAt(p []byte, off int64) (int, error) {
	h.reads++
	if h.slowVerify && h.writes.Load() > 0 {
		time.Sleep(5 * time.Millisecond)
	}
	copy(p, h.data[int(off):int(off)+len(p)])
	if h.corruptVerify && h.writes.Load() > 0 {
		p[0] ^= 1
	}
	return len(p), nil
}
func (h *memoryRepairHandle) WriteAt(p []byte, off int64) (int, error) {
	h.writes.Add(1)
	if h.beforeWrite != nil {
		h.beforeWrite(p, off)
	}
	if h.writeStarted != nil {
		close(h.writeStarted)
		<-h.releaseWrite
	}
	h.writeOffset, h.writeLength = off, len(p)
	if h.shortWrite {
		return len(p) / 2, nil
	}
	copy(h.data[int(off):int(off)+len(p)], p)
	return len(p), nil
}
func (h *memoryRepairHandle) Sync() error  { return h.syncErr }
func (h *memoryRepairHandle) Close() error { return nil }

func repairFixture(t *testing.T) (*JobManager, *Job, *memoryRepairHandle, []byte, readRegionResult) {
	t.Helper()
	disk := testDisk()
	disk.Size = 1024 * 1024
	m := newTestManager(t, JobManagerOptions{ResolveDisk: func(string) (*Disk, error) { return disk, nil }})
	job := issueTestJob(m)
	job.Device = disk.Path
	job.Status = "running"
	job.Disk.Model, job.Disk.Serial = disk.Model, disk.Serial
	job.Disk.PhysicalSectorSize = disk.PhysicalSectorSize
	job.Options.ScanRepair, job.Options.ScanRepairAfterMs = boolPtr(true), intPtr(500)
	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte((i*13 + 7) % 251)
	}
	h := &memoryRepairHandle{data: append([]byte(nil), data...)}
	region := readRegionResult{bytesRead: len(data), slowReads: []ScanReadLatency{{Ms: 501, Bytes: 512, Offset: 512}}}
	return m, job, h, data, region
}

func TestSlowRepairBacksUpOriginalBeforeWritingOnlyTheSlowRequest(t *testing.T) {
	m, job, h, source, region := repairFixture(t)
	h.beforeWrite = func(data []byte, off int64) {
		backup, err := os.ReadFile(filepath.Join(m.dataDir, "repair-backups", job.ID, "512-512.bin"))
		if err != nil || !bytes.Equal(backup, source[512:1024]) || !bytes.Equal(data, backup) {
			t.Errorf("original backup absent before write: %v", err)
		}
		journal, err := os.ReadFile(m.scanIssuePath(job.ID))
		if err != nil || !bytes.Contains(journal, []byte(`"outcome":"pending"`)) {
			t.Errorf("durable intent absent before write: %v", err)
		}
	}
	r := job.Result.(*ScanResult)
	if err := m.repairSlowRequests(job, &r.Map[0], h, source, 0, &region); err != nil {
		t.Fatal(err)
	}
	if h.writes.Load() != 1 || h.writeOffset != 512 || h.writeLength != 512 || !bytes.Equal(h.data, source) {
		t.Fatalf("rewrote wrong data or range: %+v", h)
	}
	if r.Stats.Repair.Verified != 1 || r.Stats.Repair.Pending != 0 || r.Stats.Repair.WrittenBytes != 512 || r.Stats.UnreadableRanges != 0 {
		t.Fatalf("incorrect repair result: %+v", r.Stats)
	}
	page, err := m.scanIssues(job.ID, 0, 50)
	if err != nil || len(page.Records) != 2 || page.Records[1].Repair.Outcome != "verified" {
		t.Fatalf("missing repair timeline: %+v / %v", page, err)
	}
	if !m.persistSafe(job, "test") {
		t.Fatal("persist failed")
	}
	restored := NewJobManager(m.dataDir, JobManagerOptions{Logger: silentLogger{}})
	if restored.get(job.ID).Result.(*ScanResult).Stats.Repair.Verified != 1 {
		t.Fatal("restart lost repair outcome")
	}
	job.Status = "completed"
	if err := m.remove(job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(m.dataDir, "repair-backups", job.ID, "512-512.bin")); err != nil {
		t.Fatal("history deletion removed original backup")
	}
}

func TestRepairNeverWritesBelowThresholdOrIncompleteOrChangedData(t *testing.T) {
	for _, scenario := range []string{"disabled", "equal_threshold", "read_error", "incomplete", "changed_data"} {
		t.Run(scenario, func(t *testing.T) {
			m, job, h, source, region := repairFixture(t)
			switch scenario {
			case "disabled":
				job.Options.ScanRepair = boolPtr(false)
			case "equal_threshold":
				region.slowReads[0].Ms = 500
			case "read_error":
				region.errorCount = 1
			case "incomplete":
				region.bytesRead--
			case "changed_data":
				h.data[512] ^= 1
			}
			r := job.Result.(*ScanResult)
			if err := m.repairSlowRequests(job, &r.Map[0], h, source, 0, &region); err != nil {
				t.Fatal(err)
			}
			if h.writes.Load() != 0 {
				t.Fatal("unsafe write occurred")
			}
			if scenario == "changed_data" && r.Stats.Repair.Skipped != 1 {
				t.Fatal("changed data was not marked skipped")
			}
		})
	}
}

func TestRepairBackupOrIntentFailurePreventsWrite(t *testing.T) {
	for _, intent := range []bool{false, true} {
		m, job, h, source, region := repairFixture(t)
		if intent {
			if err := os.Mkdir(m.scanIssuePath(job.ID), 0700); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.WriteFile(filepath.Join(m.dataDir, "repair-backups"), []byte("blocked"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := m.repairSlowRequests(job, &job.Result.(*ScanResult).Map[0], h, source, 0, &region); err == nil {
			t.Fatal("ignored persistence failure")
		}
		if h.writes.Load() != 0 {
			t.Fatal("write happened without durable backup and intent")
		}
	}
}

func TestRepairThresholdValidationDoesNotRoundOrAcceptStrings(t *testing.T) {
	for _, value := range []interface{}{0, 60001, 500.5, "500", nil, true} {
		if _, ok := parseScanRepairThreshold(value); ok {
			t.Fatalf("invalid threshold accepted: %v", value)
		}
	}
	for _, value := range []interface{}{1, int64(500), float64(60000)} {
		if _, ok := parseScanRepairThreshold(value); !ok {
			t.Fatalf("valid threshold rejected: %v", value)
		}
	}
}

func TestScanRepairAPIValidatesThresholdAndAcknowledgement(t *testing.T) {
	for _, body := range []map[string]interface{}{
		{"scanRepair": "true"}, {"scanRepairAfterMs": 0}, {"scanRepairAfterMs": 1.5}, {"scanRepairAfterMs": "500"}, {"scanRepairAcknowledged": "true"},
	} {
		body["type"], body["device"] = "scan", "/dev/sda"
		if err := validateCreateJobBody(body); err == nil {
			t.Fatalf("invalid API options accepted: %v", body)
		}
	}
}

func TestRepairStopsOnShortWriteFlushOrReadbackFailure(t *testing.T) {
	for _, scenario := range []string{"short_write", "flush", "mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			m, job, h, source, region := repairFixture(t)
			switch scenario {
			case "short_write":
				h.shortWrite = true
			case "flush":
				h.syncErr = io.ErrClosedPipe
			case "mismatch":
				h.corruptVerify = true
			}
			region.slowReads = append(region.slowReads, ScanReadLatency{Ms: 502, Bytes: 512, Offset: 1024})
			r := job.Result.(*ScanResult)
			if err := m.repairSlowRequests(job, &r.Map[0], h, source, 0, &region); err == nil {
				t.Fatal("ignored repair failure")
			}
			if h.writes.Load() != 1 || r.Stats.Repair.Failed != 1 || r.Stats.Repair.Verified != 0 || r.Stats.Repair.Pending != 0 {
				t.Fatalf("continued or claimed success: %+v", r.Stats.Repair)
			}
		})
	}
}

func TestRepairSeparatesVerifiedDataFromStillSlowResponse(t *testing.T) {
	m, job, h, source, region := repairFixture(t)
	job.Options.ScanRepairAfterMs = intPtr(1)
	m.scanActivityFn = func(string) (ScanActivity, error) { return ScanActivity{}, nil }
	h.slowVerify = true
	r := job.Result.(*ScanResult)
	if err := m.repairSlowRequests(job, &r.Map[0], h, source, 0, &region); err != nil {
		t.Fatal(err)
	}
	if r.Stats.Repair.StillSlow != 1 || r.Stats.Repair.Verified != 0 {
		t.Fatalf("slow response claimed repaired: %+v", r.Stats.Repair)
	}
}

func TestRepairNormalizesUnknownPhysicalSectorSizeAndRejectsChangedFormat(t *testing.T) {
	for _, changed := range []bool{false, true} {
		m, job, h, source, region := repairFixture(t)
		disk := testDisk()
		disk.Size = job.Disk.Size
		disk.PhysicalSectorSize = 0
		job.Disk.PhysicalSectorSize = job.Disk.LogicalSectorSize
		if changed {
			disk.PhysicalSectorSize = 4096
		}
		m.resolveDisk = func(string) (*Disk, error) { return disk, nil }
		err := m.repairSlowRequests(job, &job.Result.(*ScanResult).Map[0], h, source, 0, &region)
		if changed {
			if err == nil || h.writes.Load() != 0 {
				t.Fatal("changed sector format permitted a write")
			}
		} else if err != nil || h.writes.Load() != 1 {
			t.Fatalf("consistent unknown physical size rejected: %v", err)
		}
	}
}

func TestRepairCancellationAndTimeoutKeepPendingWriteOccupied(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "timeout"}[timeout], func(t *testing.T) {
			m, job, h, source, region := repairFixture(t)
			if timeout {
				m.scanReadTimeout = 30 * time.Millisecond
			}
			cancel := m.beginCancellation(job)
			h.writeStarted, h.releaseWrite = make(chan struct{}), make(chan struct{})
			finished := make(chan error, 1)
			go func() { finished <- m.repairSlowRequests(job, &job.Result.(*ScanResult).Map[0], h, source, 0, &region) }()
			select {
			case <-h.writeStarted:
			case <-time.After(time.Second):
				t.Fatal("write did not start")
			}
			if !timeout {
				cancel.cancel()
			}
			select {
			case err := <-finished:
				if timeout && !errors.Is(err, errScanWaitTimeout) || !timeout && !errors.Is(err, errCancelled) {
					t.Fatalf("unexpected stop: %v", err)
				}
			case <-time.After(time.Second):
				close(h.releaseWrite)
				t.Fatal("caller remained blocked")
			}
			stats := job.Result.(*ScanResult).Stats.Repair
			page, pageErr := m.scanIssues(job.ID, 0, 50)
			if stats.Pending != 1 || stats.Failed != 0 || stats.Verified != 0 || pageErr != nil || len(page.Records) != 2 || page.Records[1].Repair.Outcome != "uncertain" {
				close(h.releaseWrite)
				t.Fatalf("interrupted write claimed a final result: %+v / %+v / %v", stats, page, pageErr)
			}
			if !m.persistSafe(job, "interrupted repair") {
				close(h.releaseWrite)
				t.Fatal("persist failed")
			}
			restored := NewJobManager(m.dataDir, JobManagerOptions{Logger: silentLogger{}})
			if restored.get(job.ID).Result.(*ScanResult).Stats.Repair.Pending != 1 {
				close(h.releaseWrite)
				t.Fatal("restart lost uncertain write")
			}
			_, err := m.create("scan", job.Device, map[string]interface{}{})
			var conflict *jobError
			if !errors.As(err, &conflict) || conflict.apiCode != "JOB_CONFLICT" {
				close(h.releaseWrite)
				t.Fatalf("pending write lost occupancy: %v", err)
			}
			if h.writes.Load() != 1 {
				close(h.releaseWrite)
				t.Fatal("submitted overlapping writes")
			}
			close(h.releaseWrite)
			m.endCancellation(job, cancel)
		})
	}
}

func TestScanRepairRejectsUnsafeTargetsAndUnacknowledgedRequests(t *testing.T) {
	for _, scenario := range []string{"system", "mounted", "readonly", "raid", "no_serial", "no_ack", "no_identity", "bad_threshold"} {
		t.Run(scenario, func(t *testing.T) {
			disk := testDisk()
			options := map[string]interface{}{"scanRepair": true, "scanRepairAfterMs": 500, "scanRepairAcknowledged": true, "expectedDisk": map[string]interface{}{"path": disk.Path, "serial": disk.Serial, "model": disk.Model, "size": disk.Size}}
			switch scenario {
			case "system":
				disk.IsSystem = true
			case "mounted":
				disk.Mountpoints = []string{"/vol1"}
			case "readonly":
				disk.ReadOnly = true
			case "raid":
				disk.FileSystems = []string{"linux_raid_member"}
			case "no_serial":
				disk.Serial = ""
			case "no_ack":
				delete(options, "scanRepairAcknowledged")
			case "no_identity":
				delete(options, "expectedDisk")
			case "bad_threshold":
				options["scanRepairAfterMs"] = 0
			}
			m := newTestManager(t, JobManagerOptions{Mock: true, ResolveDisk: func(string) (*Disk, error) { return disk, nil }})
			if _, err := m.create("scan", disk.Path, options); err == nil {
				t.Fatal("unsafe repair was accepted")
			}
		})
	}
}

func TestScanRepairAllowsResidualZFSLabelsAndUnknownFilesystems(t *testing.T) {
	for _, fileSystems := range [][]string{nil, {"unknown"}, {"ext4"}, {"zfs_member"}, {"ZFS_MEMBER", "ext4"}} {
		disk := testDisk()
		disk.FileSystems = fileSystems
		if reason := scanRepairBlockedReason(disk); reason != "" {
			t.Fatalf("unused disk with %v was blocked: %s", fileSystems, reason)
		}
		for _, other := range []string{"linux_raid_member", "LVM2_member", "crypto_LUKS", "bcache", "swap"} {
			blocked := *disk
			blocked.FileSystems = append(append([]string(nil), fileSystems...), other)
			if scanRepairBlockedReason(&blocked) == "" {
				t.Fatalf("ZFS exception bypassed %s restriction", other)
			}
		}
	}
}

func TestScanRepairZFSDeviceMustBeExclusiveBeforeAnyReadsOrWrites(t *testing.T) {
	disk := testDisk()
	disk.FileSystems = []string{"zfs_member"}
	var repairOpens, readOpens atomic.Int64
	busy := errors.New("device or resource busy")
	m := newTestManager(t, JobManagerOptions{
		ResolveDisk: func(string) (*Disk, error) { return disk, nil },
		OpenRepair: func(string) (RepairHandle, error) {
			repairOpens.Add(1)
			return nil, busy
		},
		Open: func(string) (ReadHandle, error) {
			readOpens.Add(1)
			return nil, errors.New("scan must not start")
		},
	})
	job, err := m.create("scan", disk.Path, map[string]interface{}{
		"scanMode": "full", "scanRepair": true, "scanRepairAcknowledged": true,
		"expectedDisk": map[string]interface{}{"path": disk.Path, "serial": disk.Serial, "model": disk.Model, "size": disk.Size},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := waitForStatus(t, m, job.ID, "failed", time.Second)
	r := done.Result.(*ScanResult)
	if repairOpens.Load() != 1 || readOpens.Load() != 0 || r.Stats.BytesRead != 0 || r.Stats.Repair.WrittenBytes != 0 || !strings.Contains(done.Message, busy.Error()) {
		t.Fatalf("busy ZFS device was used: opens %d/%d, stats %+v, message %s", repairOpens.Load(), readOpens.Load(), r.Stats, done.Message)
	}
}

func TestScanIntegratesThresholdRepairAndKeepsFirstSlowEvidence(t *testing.T) {
	disk := testDisk()
	disk.FileSystems = []string{"zfs_member"}
	disk.Size = 4096 * 4096
	h := &memoryRepairHandle{data: bytes.Repeat([]byte{0x3b}, int(disk.Size))}
	var delayed atomic.Bool
	var repairOpens atomic.Int64
	m := newTestManager(t, JobManagerOptions{
		ResolveDisk: func(string) (*Disk, error) { return disk, nil },
		Open: func(string) (ReadHandle, error) {
			return &repairScanReader{read: func(p []byte, off int64) (int, error) {
				n, err := h.ReadAt(p, off)
				if off == 0 && delayed.CompareAndSwap(false, true) {
					time.Sleep(50 * time.Millisecond)
				}
				return n, err
			}}, nil
		},
		OpenRepair:   func(string) (RepairHandle, error) { repairOpens.Add(1); return h, nil },
		ScanEvidence: func(Disk) ScanEvidence { return ScanEvidence{} },
		ScanActivity: func(string) (ScanActivity, error) { return ScanActivity{}, nil },
	})
	job, err := m.create("scan", disk.Path, map[string]interface{}{"scanMode": "full", "scanRepair": true, "scanRepairAfterMs": 25, "scanRepairAcknowledged": true, "expectedDisk": map[string]interface{}{"path": disk.Path, "serial": disk.Serial, "model": disk.Model, "size": disk.Size}})
	if err != nil {
		t.Fatal(err)
	}
	done := waitForStatus(t, m, job.ID, "completed", 5*time.Second)
	r := done.Result.(*ScanResult)
	if repairOpens.Load() != 1 || h.writes.Load() < 1 || r.Stats.Repair.Verified < 1 || r.Map[0].MaxReadLatency.Ms >= 25 || r.Stats.CoveredBytes != disk.Size || r.Stats.UnreadableRanges != 0 {
		t.Fatalf("repair not integrated or verified timing not refreshed: %+v", r.Stats)
	}
	if r.Map[0].Repair == nil || r.Map[0].Repair.Verified != 1 {
		t.Fatalf("cell repair missing: %+v", r.Map[0])
	}
	page, err := m.scanIssues(job.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, issue := range page.Records {
		if issue.Repair != nil && issue.Repair.BeforeMs >= 25 {
			return // First slow evidence remains in the journal, not the new map color.
		}
	}
	t.Fatal("repair journal lost the first slow response")
}
