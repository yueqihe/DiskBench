package main

import (
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestSlowBudgetCountsSevereWaitsAndResetsForNextRegion(t *testing.T) {
	for _, severe := range []bool{false, true} {
		clock, calls := time.Now(), 0
		h := scanReaderFunc(func(p []byte, _ int64) (int, error) {
			calls++
			elapsed := 400 * time.Millisecond // Still below the HDD severe threshold.
			if severe {
				elapsed = 2 * time.Second
			}
			clock = clock.Add(elapsed)
			return len(p), nil
		})
		budget := newScanSlowBudgetReader(h, true)
		budget.now = func() time.Time { return clock }
		buffer := make([]byte, scanLatencyRequestBytes)
		for i := 0; i < 100; i++ {
			_, err := budget.ReadAt(buffer, int64(i*len(buffer)))
			if severe && i >= 3 {
				if !errors.Is(err, errScanSlowBudget) || calls != 3 {
					t.Fatalf("severe budget kept reading: %d / %v", calls, err)
				}
				break
			}
			if err != nil {
				t.Fatalf("normal region was limited by its total duration: %v", err)
			}
		}
		next := newScanSlowBudgetReader(h, true)
		next.now = budget.now
		if _, err := next.ReadAt(buffer, 1<<30); err != nil {
			t.Fatal("previous region's limit blocked the next region")
		}
	}
}

func TestSlowBudgetKeepsSuccessfulPrefixWithoutRetryOrUnreadableBytes(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := issueTestJob(m)
	job.Status, job.Disk.Rotational = "running", true
	clock, calls := time.Now(), 0
	h := newScanSlowBudgetReader(scanReaderFunc(func(p []byte, _ int64) (int, error) {
		calls++
		clock = clock.Add(2 * time.Second)
		return len(p), nil
	}), true)
	h.now = func() time.Time { return clock }
	read, err := m.readRegion(h, scanBuffer(fullScanReadSize), fullScanReadSize, 0, job, -1, 512)
	wantRead := 3 * scanLatencyRequestBytes
	if err != nil || !read.slowLimited || calls != 3 || read.bytesRead != wantRead || read.errorCount != 0 || sumUnreadableBytes(read.badRanges) != 0 || sumUnresolvedProbeBytes(read.badRanges) != int64(fullScanReadSize-wantRead) {
		t.Fatalf("lost prefix, retried, or invented unreadable bytes: %+v / %v / calls %d", read, err, calls)
	}
}

func TestSlowScanSkipsRemainderContinuesAndRestoresAccurateCoverage(t *testing.T) {
	for _, mode := range []string{"full", "quick"} {
		t.Run(mode, func(t *testing.T) {
			disk := testDisk()
			disk.Size = 3 * fullScanReadSize
			var clock atomic.Int64
			base := time.Now()
			calls := [3]int{}
			h := &scanTestHandle{read: func(p []byte, offset int64) (int, error) {
				index := int(offset / fullScanReadSize)
				calls[index]++
				elapsed := time.Millisecond
				if index != 1 {
					elapsed = 2 * time.Second
				}
				clock.Add(int64(elapsed))
				return len(p), nil
			}}
			m := newTestManager(t, JobManagerOptions{Now: func() time.Time { return base.Add(time.Duration(clock.Load())) },
				ResolveDisk: func(string) (*Disk, error) { return disk, nil }, Open: func(string) (ReadHandle, error) { return h, nil },
				ScanEvidence: func(Disk) ScanEvidence { return ScanEvidence{} }})
			r := &ScanResult{Version: 2, ScanMode: mode, Coverage: "full", Map: createScanMap(disk.Size, 3, 512)}
			target := int64(fullScanReadSize)
			if mode == "quick" {
				r.Coverage, r.SamplingPlan, r.SampleBytes = "sampled", stratifiedScanPlan, intPtr(fastSampleSize)
				target = fastSampleSize
			}
			job := &Job{ID: newUUID(), Type: "scan", Status: "queued", Device: disk.Path,
				Disk:   DiskSnapshot{Size: disk.Size, Model: disk.Model, Serial: disk.Serial, Rotational: true, LogicalSectorSize: 512},
				Result: r, Options: JobOptions{ScanMode: &mode}}
			m.jobs[job.ID] = job
			if err := m.runScan(job, mode); err != nil {
				t.Fatal(err)
			}
			wantSkipped := 2 * (target - 3*scanLatencyRequestBytes)
			if calls[0] != 3 || calls[2] != 3 || calls[1] != int(target/scanLatencyRequestBytes) {
				t.Fatalf("continued reading/rechecking a stalled region or skipped a healthy one: %v", calls)
			}
			if job.Status != "completed" || job.Progress != 100 || scanAbnormalCellCount(r) != 2 || !strings.Contains(job.Message, "跳过") {
				t.Fatalf("scan did not continue with an abnormal conclusion: %+v", job)
			}
			if r.Stats.SkippedBytes != wantSkipped || r.Stats.UnresolvedBytes != wantSkipped || r.Stats.CoveredBytes != 3*target-wantSkipped || r.Stats.BytesRead != r.Stats.CoveredBytes || r.Stats.UnreadableBytes != 0 || r.Stats.ErrorCells != 0 {
				t.Fatalf("skipped bytes counted as read, failed, or covered: %+v", r.Stats)
			}
			for i := range r.Map {
				cell := &r.Map[i]
				if !scanCellComplete(r, cell) || cell.CoveredBytes+cell.UnresolvedBytes != target {
					t.Fatalf("wrong cell completion/coverage: %+v", cell)
				}
				if i != 1 && (cell.Status != "severe" || cell.SkippedBytes != target-3*scanLatencyRequestBytes || cell.ReadOutcome != "unresolved") {
					t.Fatalf("stalled region marked normal: %+v", cell)
				}
			}
			before := r.Stats
			m.updateScanStats(job)
			if !reflect.DeepEqual(before, r.Stats) {
				t.Fatal("incremental statistics drifted after skips")
			}
			restored := NewJobManager(m.dataDir, JobManagerOptions{Mock: true})
			loaded := restored.get(job.ID)
			if loaded == nil || loaded.Result.(*ScanResult).Stats.SkippedBytes != wantSkipped || loaded.Result.(*ScanResult).Stats.CoveredBytes != r.Stats.CoveredBytes {
				t.Fatal("restart lost skipped or covered bytes")
			}
			page, err := restored.scanIssueRanges(job.ID, 0, 50)
			wantRows := int64(2)
			if mode == "quick" {
				wantRows = 4 // Two disjoint sampled ranges in each stalled cell.
			}
			if err != nil || page.Total != wantRows || !page.Complete {
				t.Fatalf("lost exact skipped ranges: %+v / %v", page, err)
			}
			for _, row := range page.Records {
				if !validScanJournalRecord(row, disk.Size) || row.Code != "SLOW_READ_LIMIT" || row.UnresolvedBytes != row.Length {
					t.Fatalf("invalid skip record: %+v", row)
				}
			}
			body := renderHumanReport(t, restored, loaded)
			for _, want := range []string{"发现 2 个异常区域", "严重卡读，已跳过", "未完成读取检测", "不计入读取覆盖率"} {
				if !strings.Contains(body, want) {
					t.Fatalf("report hides skipped coverage: %s", want)
				}
			}
		})
	}
}

func TestReturnedDeviceTimeoutSkipsRegionWithoutRetryAndKeepsLaterReads(t *testing.T) {
	for _, prefixRequests := range []int{0, 2} {
		disk := testDisk()
		disk.Size = 2 * fullScanReadSize
		calls := [2]int{}
		h := &scanTestHandle{read: func(p []byte, offset int64) (int, error) {
			index := int(offset / fullScanReadSize)
			calls[index]++
			if index == 0 && calls[0] > prefixRequests {
				// Even a full byte count alongside a timeout is not successful data.
				return len(p), &scanIOError{Evidence: ScanIOEvidence{Kind: "TIMEOUT", RequestOffset: intPtr(offset), RequestBytes: int64(len(p))}}
			}
			return len(p), nil
		}}
		m := newTestManager(t, JobManagerOptions{ResolveDisk: func(string) (*Disk, error) { return disk, nil },
			Open: func(string) (ReadHandle, error) { return h, nil }, ScanEvidence: func(Disk) ScanEvidence { return ScanEvidence{} }})
		job := &Job{ID: newUUID(), Type: "scan", Status: "queued", Device: disk.Path,
			Disk:   DiskSnapshot{Size: disk.Size, Model: disk.Model, Serial: disk.Serial, Rotational: true, LogicalSectorSize: 512},
			Result: &ScanResult{Version: 2, ScanMode: "full", Coverage: "full", Map: createScanMap(disk.Size, 2, 512)}}
		m.jobs[job.ID] = job
		if err := m.runScan(job, "full"); err != nil {
			t.Fatal(err)
		}
		r := job.Result.(*ScanResult)
		wantRead := int64(fullScanReadSize + prefixRequests*scanLatencyRequestBytes)
		if calls[0] != prefixRequests+1 || calls[1] != fullScanReadSize/scanLatencyRequestBytes || r.Map[0].Status != "error" || r.Map[1].Status == "pending" || r.Stats.DeviceErrorRanges != 1 || r.Stats.UnreadableBytes != 0 || r.Stats.BytesRead != wantRead || r.Stats.SkippedBytes != disk.Size-wantRead || r.Stats.CoveredBytes != wantRead {
			t.Fatalf("timeout retried, lost prefix, or blocked next region: calls %v / %+v", calls, r.Stats)
		}
	}
}

func TestResumeRetainsSkippedRegionsAndTheirJournalAfterRestart(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{Mock: true})
	source := resumeFixture(t, m, "full", 2, 0)
	r := source.Result.(*ScanResult)
	cell := &r.Map[0]
	cell.Status, cell.CoveredBytes, cell.BytesRead = "severe", 0, 0
	saveDisplayRecords(t, m, source, slowSkippedRange(cell.Start, cell.End-cell.Start))
	m.updateScanStats(source)
	if !m.persistSafe(source, "测试卡读断点") {
		t.Fatal("failed to save resume state")
	}
	restarted := NewJobManager(m.dataDir, JobManagerOptions{Mock: true})
	saved := restarted.get(source.ID)
	if info := scanResumeInfo(saved); info == nil || info.CellIndex != 2 {
		t.Fatalf("skipped region prevented resuming at next completed boundary: %+v", info)
	}
	continued := issueTestJob(restarted)
	continued.ID, continued.Status = newUUID(), "queued"
	continued.Result = &ScanResult{Version: 2, ScanMode: "full", Coverage: "full", Map: createScanMap(source.Disk.Size, scanCellCount, 512), Stats: ScanStats{IssuesComplete: true}}
	if err := restarted.prepareResumedScan(continued, saved); err != nil {
		t.Fatal(err)
	}
	resumed := continued.Result.(*ScanResult)
	if resumed.Map[0].SkippedBytes != cell.End-cell.Start || resumed.Stats.SkippedBytes != cell.End-cell.Start || resumed.Stats.CoveredBytes != r.Map[1].CoveredBytes || resumed.Map[2].Status != "pending" || !scanCellComplete(resumed, &resumed.Map[0]) {
		t.Fatalf("resume reclassified skipped bytes: %+v / %+v", resumed.Map[0], resumed.Stats)
	}
}

func TestTimeoutDuringLocalizationSkipsRestWithoutMoreProbeRequests(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := &Job{ID: newUUID(), Status: "running"}
	calls := 0
	h := scanReaderFunc(func(_ []byte, offset int64) (int, error) {
		calls++
		if calls == 1 {
			return 0, syscall.EIO
		}
		return 0, &scanIOError{Evidence: ScanIOEvidence{Kind: "TIMEOUT", RequestOffset: intPtr(offset)}}
	})
	read, err := m.readRegion(h, scanBuffer(fullScanReadSize), fullScanReadSize, 0, job, -1, 512)
	if err != nil || !read.slowLimited || calls != 2 || read.errorCount == 0 || read.bytesRead != 0 || sumUnresolvedProbeBytes(read.badRanges) != fullScanReadSize || sumUnreadableBytes(read.badRanges) != 0 {
		t.Fatalf("localization kept probing a timeout or lost coverage: %+v / %v / %d", read, err, calls)
	}
}
