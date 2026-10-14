package main

import (
	"os"
	"reflect"
	"testing"
	"time"
)

func resumeFixture(t *testing.T, m *JobManager, mode string, completed, issues int) *Job {
	t.Helper()
	disk := testDisk()
	r := &ScanResult{Version: 2, ScanMode: mode, Coverage: "full", Map: createScanMap(disk.Size, scanCellCount, disk.LogicalSectorSize), Stats: ScanStats{IssuesComplete: true, BadRanges: []BadRange{}}}
	if mode == "quick" {
		r.Coverage, r.SampleBytes = "sampled", intPtr(fastSampleSize)
	}
	j := &Job{ID: newUUID(), Type: "scan", Device: disk.Path, Disk: DiskSnapshot{Model: disk.Model, Serial: disk.Serial, Size: disk.Size, LogicalSectorSize: disk.LogicalSectorSize, PhysicalSectorSize: disk.PhysicalSectorSize}, Status: "cancelled", CreatedAt: isoTime(time.Now()), HistoryOrder: 1, Result: r, Options: JobOptions{ScanMode: strPtr(mode), ScanExactSectors: boolPtr(true), ScanRepair: boolPtr(false)}}
	for i := 0; i < completed; i++ {
		c := &r.Map[i]
		length := c.End - c.Start
		if mode == "quick" {
			start, sampleLength := quickSampleBounds(c, disk.Size, disk.LogicalSectorSize, fastSampleSize)
			c.SampleStart, c.SampleEnd, length = intPtr(start), intPtr(start+sampleLength), sampleLength
		}
		m.updateCell(c, 1, length, 0, length)
		c.CoveredBytes = length
		if err := m.recordScanIssuesLocked(j, c, nil); err != nil {
			t.Fatal(err)
		}
	}
	if issues > 0 {
		c := &r.Map[1]
		start := c.Start
		if c.SampleStart != nil {
			start = *c.SampleStart
		}
		for i := 0; i < issues; i++ {
			record := BadRange{Offset: start + int64(i)*512, Length: 512, Code: "EIO", Outcome: "unreadable", ProbeBytes: 512}
			if err := m.recordScanIssuesLocked(j, c, []BadRange{record}); err != nil {
				t.Fatal(err)
			}
			appendBadRanges(&r.Stats.BadRanges, []BadRange{record}, scanIssuePreviewLimit)
		}
		c.BytesRead -= int64(issues) * 512
		c.ErrorCount, c.Status = int64(issues), "error"
	}
	// Partial evidence belongs to the old report, not the inherited prefix.
	c := &r.Map[completed]
	c.Status, c.CoveredBytes, c.BytesRead = "reading", 512, 512
	if err := m.recordScanIssuesLocked(j, c, []BadRange{{Offset: c.Start + 512, Length: 512, Code: "EIO", Outcome: "unreadable", ProbeBytes: 512}}); err != nil {
		t.Fatal(err)
	}
	m.updateScanStats(j)
	j.Progress = float64(completed) / scanCellCount * 100
	m.mu.Lock()
	m.jobs[j.ID] = j
	m.nextHistoryOrder = 2
	if err := m.persist(j); err != nil {
		t.Fatal(err)
	}
	m.mu.Unlock()
	return j
}

func TestResumePreservesPrefixAndCompleteJournal(t *testing.T) {
	for _, mode := range []string{"quick", "full"} {
		t.Run(mode, func(t *testing.T) {
			m := newTestManager(t, JobManagerOptions{Mock: true})
			old := resumeFixture(t, m, mode, 500, 230)
			original := cloneJobLocked(old)
			oldJournal, _ := os.ReadFile(m.scanIssuePath(old.ID))
			j, err := m.create("scan", old.Device, map[string]interface{}{"resumeFrom": old.ID, "scanMode": "quick", "scanExactSectors": false})
			if err != nil {
				t.Fatal(err)
			}
			if j.ID == old.ID || j.ResumeFrom != old.ID || *j.Options.ScanMode != mode || !*j.Options.ScanExactSectors {
				t.Fatal("resume settings or lineage lost")
			}
			done := waitForStatus(t, m, j.ID, "completed", 5*time.Second)
			r := done.Result.(*ScanResult)
			if !reflect.DeepEqual(r.Map[:500], original.Result.(*ScanResult).Map[:500]) {
				t.Fatal("completed cells changed")
			}
			if r.Stats.TestedCells != scanCellCount || r.Stats.IssueCount != 230 || r.Stats.SavedIssueCount != 230 || r.Stats.UnreadableBytes != 230*512 {
				t.Fatalf("duplicated or missing evidence: %+v", r.Stats)
			}
			page, err := m.scanIssues(j.ID, 200, 100)
			if err != nil || len(page.Records) != 30 || page.Total != 230 {
				t.Fatalf("journal beyond preview lost: %+v %v", page, err)
			}
			currentJournal, _ := os.ReadFile(m.scanIssuePath(old.ID))
			if string(oldJournal) != string(currentJournal) || !reflect.DeepEqual(m.get(old.ID), original) {
				t.Fatal("original history changed")
			}
		})
	}
}

func TestResumeRestoresRunningCheckpointAfterRestart(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{Mock: true})
	j := resumeFixture(t, m, "full", 20, 1)
	j.Status = "running"
	if err := m.persist(j); err != nil {
		t.Fatal(err)
	}
	restored := NewJobManager(m.dataDir, JobManagerOptions{Mock: true, ResolveDisk: func(string) (*Disk, error) { return testDisk(), nil }, Delay: func(time.Duration) {}, Logger: silentLogger{}})
	t.Cleanup(restored.shutdown)
	interrupted := restored.get(j.ID)
	if interrupted.Status != "interrupted" || scanResumeInfo(interrupted).CellIndex != 20 {
		t.Fatal("restart did not restore resumable checkpoint")
	}
	resumed, err := restored.create("scan", j.Device, map[string]interface{}{"resumeFrom": j.ID})
	if err != nil {
		t.Fatal(err)
	}
	done := waitForStatus(t, restored, resumed.ID, "completed", 5*time.Second)
	if done.Result.(*ScanResult).Stats.IssueCount != 2 {
		t.Fatal("old anomaly or newly scanned anomaly lost")
	}
}

func TestRealResumeStartsAtIncompleteCell(t *testing.T) {
	var firstOffset int64 = -1
	disk := testDisk()
	disk.Size = int64(scanCellCount) * 1024 * 1024
	m := newTestManager(t, JobManagerOptions{ResolveDisk: func(string) (*Disk, error) { return disk, nil }, Open: func(string) (ReadHandle, error) {
		return &scanTestHandle{read: func(p []byte, offset int64) (int, error) {
			if firstOffset < 0 {
				firstOffset = offset
			}
			return len(p), nil
		}}, nil
	}})
	old := resumeFixture(t, m, "full", 4090, 1)
	// Use a small device for actual reader verification.
	old.Disk.Size = disk.Size
	r := old.Result.(*ScanResult)
	r.Map = createScanMap(disk.Size, scanCellCount, disk.LogicalSectorSize)
	for i := 0; i < 4090; i++ {
		c := &r.Map[i]
		m.updateCell(c, 1, c.End-c.Start, 0, c.End-c.Start)
		c.CoveredBytes = c.End - c.Start
	}
	// This fixture's previous addresses use the large geometry; clear its journal.
	m.removeScanIssueFile(old.ID)
	r.Stats = ScanStats{IssuesComplete: true, BadRanges: []BadRange{}}
	m.updateScanStats(old)
	j, err := m.create("scan", old.Device, map[string]interface{}{"resumeFrom": old.ID})
	if err != nil {
		t.Fatal(err)
	}
	done := waitForStatus(t, m, j.ID, "completed", 5*time.Second)
	if firstOffset != r.Map[4090].Start || done.Result.(*ScanResult).Stats.BytesRead != disk.Size {
		t.Fatal("real scan reread completed prefix or skipped partial cell")
	}
}

func TestResumeRejectsReplacementGeometryAndActiveJob(t *testing.T) {
	disk := testDisk()
	m := newTestManager(t, JobManagerOptions{Mock: true, ResolveDisk: func(string) (*Disk, error) { return disk, nil }})
	j := resumeFixture(t, m, "quick", 10, 0)
	for _, mutate := range []func(){func() { disk.Serial = "OTHER" }, func() { disk.Size += 4096 }, func() { disk.LogicalSectorSize = 4096 }} {
		disk = testDisk()
		mutate()
		if _, err := m.create("scan", disk.Path, map[string]interface{}{"resumeFrom": j.ID}); err == nil {
			t.Fatal("different disk accepted")
		}
	}
	disk = testDisk()
	m.jobs["busy"] = &Job{ID: "busy", Status: "running"}
	if _, err := m.create("scan", disk.Path, map[string]interface{}{"resumeFrom": j.ID}); err == nil {
		t.Fatal("resumed over active task")
	}
	delete(m.jobs, "busy")
	disk.Path = "/dev/sdc"
	resumed, err := m.create("scan", disk.Path, map[string]interface{}{"resumeFrom": j.ID})
	if err != nil || resumed.Device != disk.Path {
		t.Fatal("same disk with new path rejected")
	}
	waitForStatus(t, m, resumed.ID, "completed", 5*time.Second)
}

func TestResumeRejectsInvalidOrUncertainHistory(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{Mock: true})
	j := resumeFixture(t, m, "full", 3, 0)
	for _, mutate := range []func(*Job){func(j *Job) { j.Status = "completed" }, func(j *Job) { j.Disk.Serial = "" }, func(j *Job) { j.Result.(*ScanResult).Version = 1 }, func(j *Job) { j.Result.(*ScanResult).Stats.IssuesComplete = false }, func(j *Job) { j.Result.(*ScanResult).Stats.Repair.Pending = 1 }, func(j *Job) { j.Result.(*ScanResult).Map[0].End += 512 }} {
		copy := cloneJobLocked(j)
		mutate(copy)
		if scanResumeInfo(copy) != nil {
			t.Fatal("unsafe history offered for resume")
		}
	}
	if _, err := m.create("scan", j.Device, map[string]interface{}{"resumeFrom": "../../invalid"}); err == nil {
		t.Fatal("invalid resume id accepted")
	}
}

func TestManualCancelThenResumeKeepsMeasuredResults(t *testing.T) {
	blocked, release := make(chan struct{}), make(chan struct{})
	calls := 0
	m := newTestManager(t, JobManagerOptions{Mock: true, Delay: func(time.Duration) {
		calls++
		if calls == 126 {
			close(blocked)
			<-release
		}
	}})
	j, err := m.create("scan", "/dev/sda", map[string]interface{}{"scanMode": "quick"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("scan did not reach cancellation point")
	}
	_, cancelErr := m.cancel(j.ID)
	close(release)
	if cancelErr != nil {
		t.Fatal(cancelErr)
	}
	cancelled := waitForStatus(t, m, j.ID, "cancelled", 5*time.Second)
	if info := scanResumeInfo(cancelled); info == nil || info.CellIndex != 501 {
		t.Fatal("manual stop checkpoint missing")
	}
	next, err := m.create("scan", j.Device, map[string]interface{}{"resumeFrom": j.ID})
	if err != nil {
		t.Fatal(err)
	}
	done := waitForStatus(t, m, next.ID, "completed", 5*time.Second)
	if done.Result.(*ScanResult).Stats.IssueCount != 1 || !reflect.DeepEqual(done.Result.(*ScanResult).Map[:501], cancelled.Result.(*ScanResult).Map[:501]) {
		t.Fatal("manual resume lost or duplicated results")
	}
}
