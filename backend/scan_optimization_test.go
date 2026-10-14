package main

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestStratifiedScanSamplingCoversEndsAndSurvivesOddGeometry(t *testing.T) {
	for _, sector := range []int64{512, 4096} {
		for _, size := range []int64{scanCellCount * 4096, 1000000000000/4096*4096 + 4096} {
			r := &ScanResult{SamplingPlan: stratifiedScanPlan, SamplingSeed: 123, ScanMode: "quick", Map: createScanMap(size, scanCellCount, sector)}
			var previousEnd, total int64
			changed := false
			for i := range r.Map {
				cell := &r.Map[i]
				samples := setScanSamples(r, cell, size, sector, fastSampleSize)
				if sampleRangeBytes(samples) != min(fastSampleSize, cell.End-cell.Start) {
					t.Fatal("sample budget changed")
				}
				for _, sample := range samples {
					if sample.Start < cell.Start || sample.End > cell.End || sample.Start < previousEnd || sample.Start%sector != 0 || sample.End%sector != 0 {
						t.Fatalf("bad sample: %+v", sample)
					}
					previousEnd, total = sample.End, total+sample.End-sample.Start
				}
				if !reflect.DeepEqual(samples, quickSampleRanges(r, cell, size, sector, fastSampleSize)) {
					t.Fatal("sampling is not repeatable")
				}
				other := *r
				other.SamplingSeed++
				changed = changed || !reflect.DeepEqual(samples, quickSampleRanges(&other, cell, size, sector, fastSampleSize))
			}
			if *r.Map[0].SampleStart != 0 || *r.Map[len(r.Map)-1].SampleEnd != size || total != min(size, int64(scanCellCount)*fastSampleSize) {
				t.Fatal("missed disk end or wrong volume")
			}
			if size > int64(scanCellCount)*fastSampleSize && !changed {
				t.Fatal("new seed did not change samples")
			}
		}
	}
}

func TestStratifiedSamplingResumePreservesSeedAndRangesAfterRestart(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{Mock: true})
	source := resumeFixture(t, m, "quick", 17, 0)
	r := source.Result.(*ScanResult)
	r.SamplingPlan, r.SamplingSeed = stratifiedScanPlan, 321
	source.Options.ScanDeepLocate = boolPtr(true)
	for i := 0; i < 17; i++ {
		setScanSamples(r, &r.Map[i], source.Disk.Size, source.Disk.LogicalSectorSize, fastSampleSize)
	}
	if !m.persistSafe(source, "测试保存") {
		t.Fatal("persist failed")
	}
	restarted := NewJobManager(m.dataDir, JobManagerOptions{Mock: true, Delay: func(time.Duration) {}, ResolveDisk: func(string) (*Disk, error) { return testDisk(), nil }})
	old := restarted.get(source.ID)
	if scanResumeInfo(old) == nil {
		t.Fatal("checkpoint unavailable after restart")
	}
	job, err := restarted.create("scan", source.Device, map[string]interface{}{"resumeFrom": source.ID})
	if err != nil {
		t.Fatal(err)
	}
	done := waitForStatus(t, restarted, job.ID, "completed", 5*time.Second)
	result := done.Result.(*ScanResult)
	if result.SamplingSeed != 321 || result.SamplingPlan != stratifiedScanPlan || !reflect.DeepEqual(result.Map[:17], old.Result.(*ScanResult).Map[:17]) {
		t.Fatal("resume changed sample locations")
	}
	if done.Options.ScanDeepLocate == nil || !*done.Options.ScanDeepLocate {
		t.Fatal("resume lost deep localization setting")
	}
	corrupt := cloneJobLocked(old)
	corrupt.Result.(*ScanResult).SamplingSeed++
	if scanResumeInfo(corrupt) != nil {
		t.Fatal("changed seed accepted")
	}
}

func TestScanProbeBudgetPreservesUnknownCoverageInsteadOfInventingBadSectors(t *testing.T) {
	for _, timed := range []bool{false, true} {
		m := newTestManager(t, JobManagerOptions{ScanProbeLimit: 32, ScanProbeTimeout: time.Hour})
		if timed {
			m.scanProbeTimeout = -time.Hour
		}
		calls := 0
		h := scanReaderFunc(func(p []byte, offset int64) (int, error) {
			calls++
			if offset+int64(len(p)) <= 4096 {
				return len(p), nil
			}
			return 0, syscall.EIO
		})
		job := &Job{Status: "running"}
		ranges, read, err := m.locateReadErrors(h, 0, 65536, job, -1, 512)
		if err != nil || calls > 32 {
			t.Fatalf("budget exceeded: calls=%d err=%v", calls, err)
		}
		for i := range ranges {
			ranges[i] = classifyScanRange(ranges[i])
		}
		unknown := sumUnresolvedProbeBytes(ranges)
		if unknown <= 0 || int64(read)+sumUnreadableBytes(ranges)+unknown != 65536 {
			t.Fatalf("lost or invented coverage: read=%d ranges=%+v", read, ranges)
		}
		if timed && (calls != 0 || read != 0 || sumUnreadableBytes(ranges) != 0) {
			t.Fatal("expired deadline issued reads or invented unreadable sectors")
		}
	}
}

func TestScanContinuesAfterProbeLimitAndRestoresUnknownTotals(t *testing.T) {
	disk := testDisk()
	disk.Size = 2 * fullScanReadSize
	m := newTestManager(t, JobManagerOptions{ScanProbeLimit: 1, ResolveDisk: func(string) (*Disk, error) { return disk, nil },
		Open: func(string) (ReadHandle, error) {
			return &scanTestHandle{read: func(p []byte, offset int64) (int, error) {
				if offset < fullScanReadSize {
					return 0, syscall.EIO
				}
				return len(p), nil
			}}, nil
		}, ScanEvidence: func(Disk) ScanEvidence { return ScanEvidence{} }})
	job := &Job{ID: newUUID(), Type: "scan", Status: "queued", Device: disk.Path, Disk: DiskSnapshot{Size: disk.Size, Model: disk.Model, Serial: disk.Serial, LogicalSectorSize: 512},
		Result: &ScanResult{Version: 2, ScanMode: "full", Coverage: "full", Map: createScanMap(disk.Size, 2, 512)}}
	m.jobs[job.ID] = job
	if err := m.runScan(job, "full"); err != nil {
		t.Fatal(err)
	}
	r := job.Result.(*ScanResult)
	if job.Status != "completed" || r.Stats.UnreadableBytes != 0 || r.Stats.UnresolvedBytes != fullScanReadSize || r.Stats.CoveredBytes != fullScanReadSize || r.Stats.BytesRead != fullScanReadSize || r.Stats.TestedCells != 2 {
		t.Fatalf("incorrect scan outcome: %+v", r.Stats)
	}
	if !scanCellComplete(r, &r.Map[0]) || r.Map[0].ReadOutcome != "unresolved" {
		t.Fatal("unknown range painted as confirmed")
	}
	before := r.Stats
	m.updateScanStats(job)
	if !reflect.DeepEqual(before, r.Stats) {
		t.Fatal("incremental totals drifted")
	}
	restored := NewJobManager(m.dataDir, JobManagerOptions{Mock: true})
	loaded := restored.get(job.ID)
	if loaded == nil || loaded.Result.(*ScanResult).Stats.UnresolvedBytes != fullScanReadSize {
		t.Fatal("restart duplicated or lost unknown bytes")
	}
	export, err := restored.prepareScanReport(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if err := export(&output); err != nil {
		t.Fatal(err)
	}
	var report struct {
		RecordsComplete bool `json:"recordsComplete"`
		Job             struct {
			Result ScanResult `json:"result"`
		} `json:"job"`
	}
	if json.Unmarshal([]byte(output.String()), &report) != nil || !report.RecordsComplete || report.Job.Result.Stats.UnresolvedBytes != fullScanReadSize {
		t.Fatal("export lost unknown evidence")
	}
}

func TestSlowRequestRecheckMergesNeighborsAndKeepsRestOfScanUntouched(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	start := int64(8 * 1024 * 1024)
	const step = scanLatencyRequestBytes
	var offsets []int64
	h := scanReaderFunc(func(p []byte, offset int64) (int, error) {
		offsets = append(offsets, offset)
		if offset < start || offset+int64(len(p)) > start+4*step {
			t.Fatal("reread unrelated area")
		}
		return len(p), nil
	})
	r, issues, err := m.recheckSlowRequests(h, start, fullScanReadSize, &Job{Status: "running"}, -1, 512, readRegionResult{suspectReads: []ScanReadLatency{{Offset: start, Bytes: step, Ms: 100}, {Offset: start + 2*step, Bytes: step, Ms: 100}}})
	if err != nil || len(issues) != 0 || r.BytesRead != 4*step || len(offsets) != 4 {
		t.Fatalf("recheck duplicated reads: %+v %v %v", r, offsets, err)
	}
}

func TestScanLoadSubtractsOwnIOAndRejectsResetCounters(t *testing.T) {
	parsed, err := parseScanActivity("1 0 8 0 2 0 16 0 3 0 0")
	if err != nil || parsed.ReadBytes != 4096 || parsed.WriteBytes != 8192 || parsed.InFlight != 3 {
		t.Fatal("wrong diskstats sector units")
	}
	for _, bad := range []string{"", "1 0 -1 0 2 0 16 0 3 0 0", "1 0 x 0 2 0 16 0 3 0 0"} {
		if _, err := parseScanActivity(bad); err == nil {
			t.Fatal("invalid counters accepted")
		}
	}
	current := ScanActivity{ReadBytes: 1024 * 1024}
	m := newTestManager(t, JobManagerOptions{ScanActivity: func(string) (ScanActivity, error) { return current, nil }})
	monitor := m.newScanLoadMonitor("/dev/test")
	current.ReadBytes += 1024 * 1024
	if load := monitor.observe(1024*1024, true); load == nil || load.BackgroundActivity || load.OtherReadBytes != 0 {
		t.Fatal("own scan mistaken for concurrent work")
	}
	current.WriteBytes += 128 * 1024
	if load := monitor.observe(0, true); load == nil || !load.BackgroundActivity || load.WriteBytes != 128*1024 {
		t.Fatal("concurrent writes lost")
	}
	current.ReadBytes = 0
	if monitor.observe(0, true) != nil {
		t.Fatal("counter reset accepted as large workload")
	}
}

func TestQuickScanReadsOnlyDistributedSamples(t *testing.T) {
	disk := testDisk()
	disk.Size = 4 * fullScanReadSize
	var requests []ScanSampleRange
	m := newTestManager(t, JobManagerOptions{ResolveDisk: func(string) (*Disk, error) { return disk, nil },
		Open: func(string) (ReadHandle, error) {
			return &scanTestHandle{read: func(p []byte, offset int64) (int, error) {
				requests = append(requests, ScanSampleRange{Start: offset, End: offset + int64(len(p))})
				return len(p), nil
			}}, nil
		}, ScanEvidence: func(Disk) ScanEvidence { return ScanEvidence{} }})
	job := &Job{ID: newUUID(), Type: "scan", Status: "queued", Device: disk.Path,
		Disk: DiskSnapshot{Size: disk.Size, Model: disk.Model, Serial: disk.Serial, LogicalSectorSize: 512},
		Result: &ScanResult{Version: 2, ScanMode: "quick", Coverage: "sampled", SamplingPlan: stratifiedScanPlan, SamplingSeed: 987,
			SampleBytes: intPtr(fastSampleSize), Map: createScanMap(disk.Size, 4, 512)}}
	m.jobs[job.ID] = job
	if err := m.runScan(job, "quick"); err != nil {
		t.Fatal(err)
	}
	r := job.Result.(*ScanResult)
	if job.Status != "completed" || r.Stats.CoveredBytes != 4*fastSampleSize || r.Stats.BytesRead != 4*fastSampleSize || r.Stats.TestedCells != 4 {
		t.Fatalf("incorrect sample totals: %+v", r.Stats)
	}
	var total int64
	for _, request := range requests {
		inside := false
		for _, cell := range r.Map {
			for _, sample := range cell.SampleRanges {
				inside = inside || (request.Start >= sample.Start && request.End <= sample.End)
			}
		}
		if !inside {
			t.Fatalf("read skipped sampling gap: %+v", request)
		}
		total += request.End - request.Start
	}
	if total != 4*fastSampleSize || requests[0].Start != 0 || requests[len(requests)-1].End != disk.Size {
		t.Fatal("missed disk ends or duplicated sampling volume")
	}
	before := r.Stats
	m.updateScanStats(job)
	if !reflect.DeepEqual(before, r.Stats) {
		t.Fatal("incremental quick totals drifted")
	}
}

func TestDeepLocalizationIsExplicitAndHasLargerBoundedBudget(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{Mock: true, ScanProbeLimit: 1})
	for _, request := range []struct {
		task  string
		value interface{}
	}{{"scan", "true"}, {"scan", 1}, {"speed", true}} {
		if _, err := m.create(request.task, testDisk().Path, map[string]interface{}{"scanDeepLocate": request.value}); err == nil {
			t.Fatal("invalid deep localization option accepted")
		}
	}
	for _, deep := range []bool{false, true} {
		calls := 0
		h := scanReaderFunc(func(p []byte, offset int64) (int, error) {
			calls++
			return 0, syscall.EIO
		})
		ranges, _, err := m.locateReadErrors(h, 0, errorProbeSize, &Job{Status: "running", Options: JobOptions{ScanDeepLocate: boolPtr(deep)}}, -1, 512)
		if err != nil || calls > 4096 {
			t.Fatalf("unbounded localization: %d %v", calls, err)
		}
		if deep && (calls <= 1 || sumUnresolvedProbeBytes(ranges) != 0) {
			t.Fatal("deep budget did not finish small range")
		}
		if !deep && (calls != 1 || sumUnresolvedProbeBytes(ranges) != errorProbeSize) {
			t.Fatal("standard budget not respected")
		}
	}
}

func TestLimitedRecheckKeepsPrimaryCoverage(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{ScanProbeLimit: 1})
	job := &Job{ID: newUUID(), Status: "running", Result: &ScanResult{Version: 2}, Disk: DiskSnapshot{LogicalSectorSize: 512}}
	cell := ScanCell{Start: 0, End: fastSampleSize}
	calls := 0
	h := scanReaderFunc(func(p []byte, offset int64) (int, error) {
		calls++
		if calls == 1 {
			time.Sleep(25 * time.Millisecond)
		}
		if calls > fastSampleSize/scanLatencyRequestBytes {
			return 0, syscall.EIO
		}
		return len(p), nil
	})
	read, _, _, recheck, err := m.inspectScanRegion(job, &cell, h, nil, scanBuffer(fastSampleSize), 0, fastSampleSize, 512, scanLatencyReference{}, m.newScanLoadMonitor("/dev/test"))
	if err != nil || recheck == nil || recheck.Outcome != "read_issue" || read.bytesRead != fastSampleSize || sumUnresolvedProbeBytes(read.badRanges) != 0 {
		t.Fatalf("recheck changed confirmed primary coverage: %+v %+v %v", read, recheck, err)
	}
	if len(read.badRanges) == 0 || read.errorCount == 0 {
		t.Fatal("limited recheck lost warning")
	}
}

func TestScanQuietWaitIsBoundedAndCancellationStopsIt(t *testing.T) {
	delays := 0
	m := newTestManager(t, JobManagerOptions{Delay: func(time.Duration) { delays++ },
		ScanActivity: func(string) (ScanActivity, error) { return ScanActivity{InFlight: 1}, nil }})
	job := &Job{ID: newUUID(), Status: "running"}
	if err := m.waitScanQuiet(job, &ScanLoad{BackgroundActivity: true}); err != nil || delays != 10 {
		t.Fatalf("wait did not honor bound: %d %v", delays, err)
	}
	cancel := m.beginCancellation(job)
	cancel.cancel()
	if err := m.waitScanQuiet(job, &ScanLoad{BackgroundActivity: true}); !errors.Is(err, errCancelled) || delays != 10 {
		t.Fatalf("cancelled wait continued: %d %v", delays, err)
	}
}

func TestRecheckLoadExcludesActivityDuringIdleWait(t *testing.T) {
	current := ScanActivity{}
	waits, calls := 0, 0
	m := newTestManager(t, JobManagerOptions{ScanActivity: func(string) (ScanActivity, error) { return current, nil },
		Delay: func(time.Duration) {
			waits++
			current.ReadBytes += 4 * 1024 * 1024 // Other workload finishes during the wait.
			current.InFlight = 0
		}})
	job := &Job{ID: newUUID(), Status: "running", Disk: DiskSnapshot{LogicalSectorSize: 512}}
	cell := ScanCell{Start: 0, End: fastSampleSize}
	h := scanReaderFunc(func(p []byte, offset int64) (int, error) {
		calls++
		if calls == 1 {
			time.Sleep(25 * time.Millisecond)
			current.WriteBytes += 128 * 1024
			current.InFlight = 1
		}
		current.ReadBytes += uint64(len(p))
		return len(p), nil
	})
	_, _, _, recheck, err := m.inspectScanRegion(job, &cell, h, nil, scanBuffer(fastSampleSize), 0, fastSampleSize, 512, scanLatencyReference{}, m.newScanLoadMonitor("/dev/test"))
	if err != nil || waits != 1 || cell.Load == nil || !cell.Load.BackgroundActivity || recheck == nil || recheck.Load == nil || recheck.Load.BackgroundActivity {
		t.Fatalf("idle activity attributed to recheck: cell=%+v recheck=%+v waits=%d err=%v", cell.Load, recheck, waits, err)
	}
}
