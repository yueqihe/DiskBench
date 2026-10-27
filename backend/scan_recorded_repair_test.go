package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func recordedRepairFixture(t *testing.T, open func(string) (RepairHandle, error)) (*JobManager, *Job, *Disk) {
	t.Helper()
	disk := testDisk()
	disk.Size = 4096 * 4096
	m := newTestManager(t, JobManagerOptions{ResolveDisk: func(string) (*Disk, error) { return disk, nil }, OpenRepair: open})
	source := &Job{ID: newUUID(), Type: "scan", Device: disk.Path, Status: "completed", CreatedAt: isoTime(time.Now()), Progress: 100,
		Disk:    DiskSnapshot{Name: disk.Name, Model: disk.Model, Serial: disk.Serial, Size: disk.Size, LogicalSectorSize: 512, PhysicalSectorSize: 4096},
		Options: JobOptions{ScanMode: strPtr("full")}, Result: &ScanResult{Version: 2, Coverage: "full", ScanMode: "full", LatencyBasis: "solid-dual-v4", Map: createScanMap(disk.Size, 4096, 512), Stats: ScanStats{IssuesComplete: true}}}
	r := source.Result.(*ScanResult)
	for i := range r.Map {
		r.Map[i].Status = "normal"
		r.Map[i].ReadOutcome = "readable"
		r.Map[i].CoveredBytes = 4096
	}
	for _, i := range []int{0, 2} {
		cell := &r.Map[i]
		cell.Status = "severe"
		cell.MaxReadLatency = &ScanReadLatency{Ms: 500, Bytes: 512, Offset: cell.Start + 512}
	}
	m.mu.Lock()
	m.jobs[source.ID] = source
	m.persistSafeLocked(source, "test")
	m.mu.Unlock()
	return m, source, disk
}

func recordedRepairOptions(source *Job, disk *Disk) map[string]interface{} {
	return map[string]interface{}{"scanRepair": true, "scanRepairFrom": source.ID, "scanRepairAfterMs": 500, "scanRepairAcknowledged": true, "expectedDisk": map[string]interface{}{"path": disk.Path, "serial": disk.Serial, "model": disk.Model, "size": disk.Size}}
}

func TestRecordedRepairWritesFreshBytesOnlyAtSavedAddressesEvenWhenNowFast(t *testing.T) {
	data := make([]byte, 16384)
	for i := range data {
		data[i] = byte(i*17 + 5)
	}
	h := &memoryRepairHandle{data: append([]byte(nil), data...)}
	m, source, disk := recordedRepairFixture(t, func(string) (RepairHandle, error) { return h, nil })
	before, _ := os.ReadFile(filepath.Join(m.jobsDir, source.ID+".json"))
	h.beforeWrite = func(p []byte, offset int64) {
		if offset != 512 && offset != 8704 {
			t.Errorf("wrote outside recorded requests: %d", offset)
		}
		if !bytes.Equal(p, data[offset:offset+int64(len(p))]) {
			t.Error("did not use fresh disk data")
		}
	}
	created, err := m.create("scan", disk.Path, recordedRepairOptions(source, disk))
	if err != nil {
		t.Fatal(err)
	}
	done := waitForStatus(t, m, created.ID, "completed", time.Second)
	r := done.Result.(*ScanResult)
	if h.writes.Load() != 2 || r.Coverage != "targeted" || r.Stats.Repair.Verified != 2 || r.Stats.Repair.Improved != 0 || r.Stats.Repair.WrittenBytes != 1024 || r.Stats.Repair.Pending != 0 || done.Phase != "慢区修复完成" {
		t.Fatalf("incorrect targeted result: %+v / writes %d", r, h.writes.Load())
	}
	if !bytes.Equal(h.data, data) {
		t.Fatal("original contents changed")
	}
	after, _ := os.ReadFile(filepath.Join(m.jobsDir, source.ID+".json"))
	if !bytes.Equal(before, after) {
		t.Fatal("source scan was modified")
	}
	for _, name := range []string{"512-512.bin", "8704-512.bin"} {
		if _, err := os.Stat(filepath.Join(m.dataDir, "repair-backups", done.ID, name)); err != nil {
			t.Fatal(err)
		}
	}
	body := renderHumanReport(t, m, done)
	if !strings.Contains(body, "按记录修复慢区") || !strings.Contains(body, source.ID) || !strings.Contains(body, "未重新扫描整盘") {
		t.Fatal("report lost targeted scope or source")
	}
	restored := NewJobManager(m.dataDir, JobManagerOptions{Logger: silentLogger{}})
	defer restored.shutdown()
	loaded := restored.get(done.ID)
	if loaded == nil || loaded.Phase != "慢区修复完成" || loaded.Result.(*ScanResult).Stats.Repair.Verified != 2 || len(loaded.Result.(*ScanResult).RepairTargets) != 2 {
		t.Fatal("restart lost repair results")
	}
}

func TestRecordedRepairRejectsUnavailableSourcesAndUnsafeTargets(t *testing.T) {
	for _, scenario := range []string{"unfinished", "legacy", "repair-source", "unaligned", "outside", "unreadable", "skipped", "missing-address", "different-disk", "mounted", "unacknowledged", "no-identity"} {
		t.Run(scenario, func(t *testing.T) {
			opened := false
			m, source, disk := recordedRepairFixture(t, func(string) (RepairHandle, error) { opened = true; return nil, io.ErrClosedPipe })
			r := source.Result.(*ScanResult)
			opts := recordedRepairOptions(source, disk)
			switch scenario {
			case "unfinished":
				source.Status = "cancelled"
			case "legacy":
				r.Version = 1
			case "repair-source":
				source.Options.ScanRepairFrom = newUUID()
			case "unaligned":
				r.Map[0].MaxReadLatency.Offset++
			case "outside":
				r.Map[0].MaxReadLatency.Offset = r.Map[0].End
			case "unreadable":
				for _, i := range []int{0, 2} {
					r.Map[i].ErrorCount = 1
					r.Map[i].ReadOutcome = "unreadable"
				}
			case "skipped":
				for _, i := range []int{0, 2} {
					r.Map[i].SkippedBytes = 512
				}
			case "missing-address":
				for _, i := range []int{0, 2} {
					r.Map[i].MaxReadLatency = nil
				}
			case "different-disk":
				source.Disk.Serial = "REPLACED"
			case "mounted":
				disk.Mountpoints = []string{"/vol1"}
			case "unacknowledged":
				delete(opts, "scanRepairAcknowledged")
			case "no-identity":
				delete(opts, "expectedDisk")
			}
			if _, err := m.create("scan", disk.Path, opts); err == nil || opened {
				t.Fatalf("unsafe request accepted: %v", err)
			}
		})
	}
}

type recordedRepairReadHandle struct {
	*memoryRepairHandle
	fail    bool
	changed bool
	calls   int
}

func (h *recordedRepairReadHandle) ReadAt(p []byte, off int64) (int, error) {
	h.calls++
	if h.fail {
		return 0, io.EOF
	}
	n, err := h.memoryRepairHandle.ReadAt(p, off)
	if h.changed && h.calls%2 == 0 {
		p[0] ^= 1
	}
	return n, err
}

func TestRecordedRepairNeverWritesIncompleteOrUnstableCurrentData(t *testing.T) {
	for _, changed := range []bool{false, true} {
		h := &recordedRepairReadHandle{memoryRepairHandle: &memoryRepairHandle{data: make([]byte, 16384)}, fail: !changed, changed: changed}
		m, source, disk := recordedRepairFixture(t, func(string) (RepairHandle, error) { return h, nil })
		created, err := m.create("scan", disk.Path, recordedRepairOptions(source, disk))
		if err != nil {
			t.Fatal(err)
		}
		done := waitForStatus(t, m, created.ID, "completed", time.Second)
		if h.writes.Load() != 0 || done.Result.(*ScanResult).Stats.Repair.Skipped != 2 {
			t.Fatal("wrote incomplete or unstable data")
		}
	}
}

func TestRecordedRepairCancellationPreservesPendingIntent(t *testing.T) {
	h := &memoryRepairHandle{data: make([]byte, 16384), writeStarted: make(chan struct{}), releaseWrite: make(chan struct{})}
	m, source, disk := recordedRepairFixture(t, func(string) (RepairHandle, error) { return h, nil })
	created, err := m.create("scan", disk.Path, recordedRepairOptions(source, disk))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("write did not start")
	}
	if _, err = m.cancel(created.ID); err != nil {
		t.Fatal(err)
	}
	close(h.releaseWrite)
	done := waitForStatus(t, m, created.ID, "cancelled", time.Second)
	if h.writes.Load() != 1 || done.Result.(*ScanResult).Stats.Repair.Pending != 1 {
		t.Fatal("lost uncertain write or continued after cancel")
	}
	if scanResumeInfo(done) != nil {
		t.Fatal("repair job was offered as an ordinary resumed scan")
	}
}

func TestRecordedRepairAPIRejectsInvalidSourceCombinations(t *testing.T) {
	for _, raw := range []string{`{"scanRepairFrom":123}`, `{"scanRepairFrom":"bad"}`, `{"scanRepairFrom":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","scanRepair":false}`, `{"scanRepairFrom":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","scanRepair":true,"resumeFrom":"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"}`} {
		var body map[string]interface{}
		json.Unmarshal([]byte(raw), &body)
		body["type"], body["device"] = "scan", "/dev/sda"
		if validateCreateJobBody(body) == nil {
			t.Fatal("invalid repair request accepted")
		}
	}
}

func TestRecordedRepairSampleMustContainExactRequest(t *testing.T) {
	_, source, _ := recordedRepairFixture(t, nil)
	r := source.Result.(*ScanResult)
	r.Coverage = "sampled"
	for _, i := range []int{0, 2} {
		cell := &r.Map[i]
		cell.SampleRanges = []ScanSampleRange{{Start: cell.Start + 512, End: cell.Start + 1024}}
	}
	if targets, err := recordedRepairTargets(source, 500); err != nil || len(targets) != 2 {
		t.Fatalf("valid sampled addresses rejected: %v", err)
	}
	r.Map[2].MaxReadLatency.Offset = r.Map[2].Start + 1024
	if _, err := recordedRepairTargets(source, 500); err == nil {
		t.Fatal("accepted an address outside the recorded sample")
	}
}
