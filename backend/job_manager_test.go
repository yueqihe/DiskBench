package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testDisk() *Disk {
	return &Disk{
		Name: "sda", Path: "/dev/sda", Model: "Test disk", Serial: "TEST",
		Size: 2_000_000_000_000, LogicalSectorSize: 512, PhysicalSectorSize: 4096, IsSystem: false,
	}
}

func TestCurrentJobListKeepsTrackedTaskAfterCompletion(t *testing.T) {
	m := &JobManager{jobs: map[string]*Job{
		"deadbeef": {ID: "deadbeef", Status: "running"},
		"cafe":     {ID: "cafe", Status: "completed"},
		"baad":     {ID: "baad", Status: "cancelling"},
		"feed":     {ID: "feed", Status: "queued"},
	}}
	for _, summary := range []bool{false, true} {
		listed := m.listCurrent(summary, nil)
		if len(listed) != 3 {
			t.Fatalf("initial current job count = %d, want 3", len(listed))
		}
		for _, item := range listed {
			if item.(*Job).ID == "cafe" {
				t.Fatal("unselected historical task loaded with current jobs")
			}
		}
	}
	m.jobs["deadbeef"].Status = "completed"
	for _, summary := range []bool{false, true} {
		listed := m.listCurrent(summary, []string{"deadbeef", "missing"})
		encoded, err := json.Marshal(listed)
		if err != nil {
			t.Fatal(err)
		}
		var decoded []map[string]interface{}
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, job := range decoded {
			if job["id"] == "deadbeef" && job["status"] == "completed" {
				found = true
			}
			if job["id"] == "cafe" {
				t.Fatal("unselected historical task returned after tracked task completed")
			}
		}
		if len(listed) != 3 || !found {
			t.Fatalf("completed tracked task missing: %s", encoded)
		}
	}
}

func testSpace() *StorageSpace {
	return &StorageSpace{
		Path: "/vol1", Source: "/dev/sda1", FileSystem: "btrfs", Layout: "single",
		Members: []string{"/dev/sda"}, Size: 1_000_000_000_000, FreeBytes: 500_000_000_000,
	}
}

func newTestManager(t *testing.T, opts JobManagerOptions) *JobManager {
	t.Helper()
	if opts.ResolveDisk == nil {
		opts.ResolveDisk = func(string) (*Disk, error) { return testDisk(), nil }
	}
	if opts.Delay == nil {
		opts.Delay = func(time.Duration) {}
	}
	if opts.Logger == nil {
		opts.Logger = silentLogger{}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	m := NewJobManager(t.TempDir(), opts)
	t.Cleanup(m.shutdown)
	return m
}

func waitForStatus(t *testing.T, m *JobManager, id, status string, timeout time.Duration) *Job {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		job := m.get(id)
		if job != nil && job.Status == status {
			return job
		}
		time.Sleep(2 * time.Millisecond)
	}
	job := m.get(id)
	t.Fatalf("job %s did not reach %s (current %v)", id, status, jobStatus(job))
	return nil
}

func jobStatus(job *Job) string {
	if job == nil {
		return "nil"
	}
	return job.Status
}

func TestMockFullScanCoversWholeDevice(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{Mock: true})
	job, err := m.create("scan", "/dev/sda", map[string]interface{}{"scanMode": "full"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	done := waitForStatus(t, m, job.ID, "completed", 5*time.Second)
	result := done.Result.(*ScanResult)
	if result.LatencyBasis != scanLatencyBasis(done.Disk.Rotational) || result.Map[0].MaxUnitMs == nil || result.Map[0].MaxReadLatency == nil {
		t.Fatal("missing dual latency metadata")
	}
	if result.Coverage != "full" {
		t.Errorf("coverage = %s", result.Coverage)
	}
	if result.SampleBytes != nil {
		t.Errorf("sampleBytes = %v, want nil", result.SampleBytes)
	}
	if len(result.Map) != 4096 || result.Stats.TestedCells != 4096 {
		t.Errorf("testedCells = %d", result.Stats.TestedCells)
	}
	if result.Stats.CoveredBytes != testDisk().Size || result.Stats.BytesRead+result.Stats.UnreadableBytes != testDisk().Size {
		t.Errorf("coverage/read outcome stats = %+v", result.Stats)
	}
	var sum int64
	for _, cell := range result.Map {
		sum += cell.BytesRead
		if cell.SampleStart != nil || cell.SampleEnd != nil {
			t.Errorf("cell %d has sample range in full mode", cell.Index)
		}
	}
	if sum+result.Stats.UnreadableBytes != testDisk().Size {
		t.Errorf("map bytesRead sum = %d, unreadable = %d", sum, result.Stats.UnreadableBytes)
	}
}

func TestScanLatencyAccountsForTransferSize(t *testing.T) {
	const mib = 1024 * 1024
	for _, tc := range []struct {
		name  string
		ms    float64
		bytes int64
		want  string
	}{
		{"normal HDD full read", 84, 8 * mib, "normal"},
		{"same throughput quick read", 10.5, mib, "normal"},
		{"slow full read boundary", 400, 8 * mib, "slow"},
		{"slow full read below red boundary", 7999, 8 * mib, "slow"},
		{"severe full read boundary", 8000, 8 * mib, "severe"},
		{"slow quick read", 50, mib, "slow"},
		{"slow quick read below red boundary", 999.99, mib, "slow"},
		{"severe quick read", 1000, mib, "severe"},
		{"small tail command overhead", 10, 512, "normal"},
		{"slow small tail below red boundary", 700, 512, "slow"},
		{"stalled small tail", 1000, 512, "severe"},
		{"write plus read back", 84, 2 * 4 * mib, "normal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyLatency(scanUnitLatency(tc.ms, tc.bytes), 0); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestScanCellKeepsWorstNormalizedReadAndErrors(t *testing.T) {
	m := &JobManager{}
	cell := ScanCell{}
	m.updateCell(&cell, 84, 8*1024*1024, 0, 8*1024*1024)
	m.updateCell(&cell, 60, 1024*1024, 0, 1024*1024)
	if cell.Status != "slow" || *cell.MaxMs != 84 || *cell.MaxUnitMs != 60 {
		t.Fatalf("small slower transfer lost: %+v", cell)
	}
	m.updateCell(&cell, 10, 8*1024*1024, 0, 8*1024*1024)
	if cell.Status != "slow" {
		t.Fatal("fast read erased slow read")
	}
	m.updateCell(&cell, 1, 0, 1, 8*1024*1024)
	m.updateCell(&cell, 10, 8*1024*1024, 0, 8*1024*1024)
	if cell.Status != "error" {
		t.Fatal("recovered read erased I/O error")
	}
	job := &Job{Result: &ScanResult{Map: []ScanCell{
		cell, {Status: "normal"}, {Status: "slow"}, {Status: "severe"}, {Status: "pending"},
	}}}
	m.updateScanStats(job)
	stats := job.Result.(*ScanResult).Stats
	if stats.TestedCells != 4 || stats.SlowCells != 2 || stats.ErrorCells != 1 {
		t.Fatalf("incorrect four-category stats: %+v", stats)
	}
}

func TestMockQuickScanSamples4096Regions(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{Mock: true})
	job, err := m.create("scan", "/dev/sda", map[string]interface{}{"scanMode": "quick"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	done := waitForStatus(t, m, job.ID, "completed", 5*time.Second)
	result := done.Result.(*ScanResult)
	if result.Coverage != "sampled" || result.SampleBytes == nil || *result.SampleBytes != fastSampleSize {
		t.Fatalf("quick scan sampling = %s/%v", result.Coverage, result.SampleBytes)
	}
	if len(result.Map) != quickScanCellCount || result.Stats.TestedCells != quickScanCellCount {
		t.Fatalf("quick scan regions = %d/%d, want %d", len(result.Map), result.Stats.TestedCells, quickScanCellCount)
	}
	if result.Stats.CoveredBytes != int64(quickScanCellCount)*fastSampleSize || result.Stats.BytesRead+result.Stats.UnreadableBytes != result.Stats.CoveredBytes {
		t.Fatalf("quick scan bytes = %d", result.Stats.BytesRead)
	}
	for _, index := range []int{0, quickScanCellCount / 2, quickScanCellCount - 1} {
		cell := result.Map[index]
		if cell.SampleStart == nil || cell.SampleEnd == nil || sampleRangeBytes(cell.SampleRanges) != fastSampleSize {
			t.Fatalf("region %d sample = %v..%v", index, cell.SampleStart, cell.SampleEnd)
		}
	}
}

func TestScanProgressReturnsOnlyContiguousCompletedCells(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{Mock: true})
	result := &ScanResult{
		Map:   createScanMap(testDisk().Size, scanCellCount, 512),
		Stats: ScanStats{TestedCells: 2, BytesRead: 1024, BadRanges: []BadRange{}},
	}
	result.Map[0].Status = "under5"
	result.Map[1].Status = "under20"
	result.Map[2].Status = "reading"
	job := &Job{ID: "deadbeef", Type: "scan", Status: "running", Progress: 0.1, Result: result}
	m.mu.Lock()
	m.jobs[job.ID] = job
	m.mu.Unlock()

	progress, err := m.scanProgress(job.ID, 1)
	if err != nil {
		t.Fatalf("scanProgress: %v", err)
	}
	if progress.Next != 2 || len(progress.Cells) != 1 || progress.Cells[0].Index != 1 {
		t.Fatalf("progress = %+v", progress)
	}
}

func TestScanProgressEncodesEmptyCollectionsAsArrays(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{Mock: true})
	result := &ScanResult{
		Map:   createScanMap(testDisk().Size, scanCellCount, 512),
		Stats: ScanStats{BadRanges: []BadRange{}},
	}
	job := &Job{ID: "empty-progress", Type: "scan", Status: "running", Result: result}
	m.mu.Lock()
	m.jobs[job.ID] = job
	m.mu.Unlock()

	progress, err := m.scanProgress(job.ID, 0)
	if err != nil {
		t.Fatalf("scanProgress: %v", err)
	}
	payload, err := json.Marshal(progress)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if cells, ok := decoded["cells"].([]interface{}); !ok || len(cells) != 0 {
		t.Fatalf("cells must encode as [], got %s", payload)
	}
	stats, ok := decoded["stats"].(map[string]interface{})
	if !ok {
		t.Fatalf("stats missing from %s", payload)
	}
	if badRanges, ok := stats["badRanges"].([]interface{}); !ok || len(badRanges) != 0 {
		t.Fatalf("badRanges must encode as [], got %s", payload)
	}
}

func TestActiveJobResponsesAreDetachedFromLiveScanState(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{Mock: true})
	result := &ScanResult{
		Map:   createScanMap(testDisk().Size, scanCellCount, 512),
		Stats: ScanStats{BadRanges: []BadRange{}},
	}
	job := &Job{ID: "detached", Type: "scan", Status: "running", Result: result, Log: []string{"started"}}
	m.mu.Lock()
	m.jobs[job.ID] = job
	m.mu.Unlock()

	snapshot := m.list(false)[0].(*Job)
	result.Map[0].Status = "under5"
	result.Stats.TestedCells = 1
	job.Log[0] = "changed"
	if got := snapshot.Result.(*ScanResult); got.Map[0].Status != "pending" || got.Stats.TestedCells != 0 {
		t.Fatalf("snapshot changed with live scan: %+v", got)
	}
	if snapshot.Log[0] != "started" {
		t.Fatalf("snapshot log changed with live job: %v", snapshot.Log)
	}
}

type recordingReadHandle struct{ calls int }

func (h *recordingReadHandle) ReadAt(p []byte, off int64) (int, error) {
	h.calls++
	clear(p)
	return len(p), nil
}

func (h *recordingReadHandle) Close() error { return nil }

type failingSectorReadHandle struct {
	badStart  int64
	badLength int64
}

func (h *failingSectorReadHandle) ReadAt(p []byte, off int64) (int, error) {
	if off < h.badStart+h.badLength && off+int64(len(p)) > h.badStart {
		return 0, &os.PathError{Op: "read", Path: "/dev/test", Err: fmt.Errorf("EIO")}
	}
	return len(p), nil
}

func (h *failingSectorReadHandle) Close() error { return nil }

func TestReadErrorLocationUsesConfiguredProbeSize(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := &Job{ID: "probe", Status: "running"}
	const regionSize = int64(128 * 1024)
	const badOffset = int64(64 * 1024)

	coarse, coarseBytes, err := m.locateReadErrors(
		&failingSectorReadHandle{badStart: badOffset, badLength: 512},
		0, regionSize, job, 200, errorProbeSize,
	)
	if err != nil {
		t.Fatalf("coarse locate: %v", err)
	}
	if len(coarse) != 1 || coarse[0].Offset != badOffset || coarse[0].Length != errorProbeSize {
		t.Fatalf("coarse ranges = %+v", coarse)
	}
	if coarseBytes != int(regionSize-errorProbeSize) {
		t.Fatalf("coarse bytes = %d", coarseBytes)
	}

	exact, exactBytes, err := m.locateReadErrors(
		&failingSectorReadHandle{badStart: badOffset, badLength: 512},
		0, regionSize, job, 200, 512,
	)
	if err != nil {
		t.Fatalf("exact locate: %v", err)
	}
	if len(exact) != 1 || exact[0].Offset != badOffset || exact[0].Length != 512 {
		t.Fatalf("exact ranges = %+v", exact)
	}
	if exactBytes != int(regionSize-512) {
		t.Fatalf("exact bytes = %d", exactBytes)
	}
}

func TestScanExactSectorsOptionIsStored(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options map[string]interface{}
		want    bool
	}{
		{name: "default on", options: map[string]interface{}{"scanMode": "quick"}, want: true},
		{name: "explicit on", options: map[string]interface{}{"scanMode": "quick", "scanExactSectors": true}, want: true},
		{name: "explicit off", options: map[string]interface{}{"scanMode": "quick", "scanExactSectors": false}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager(t, JobManagerOptions{Mock: true})
			job, err := m.create("scan", "/dev/sda", tc.options)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if job.Options.ScanExactSectors == nil || *job.Options.ScanExactSectors != tc.want {
				t.Fatalf("scanExactSectors = %v, want %v", job.Options.ScanExactSectors, tc.want)
			}
		})
	}
}

func TestFullScanReadUnitMatchesDGScale(t *testing.T) {
	if fullScanReadSize != 8*1024*1024 {
		t.Fatalf("fullScanReadSize = %d", fullScanReadSize)
	}
}

func TestRotationalQuickScanWakesWithSmartBeforeMeasuredReads(t *testing.T) {
	disk := &Disk{
		Name: "sda", Path: "/dev/sda", Model: "Rotational test disk", Serial: "HDD-TEST",
		Size: 16 * 1024 * 1024, LogicalSectorSize: 512, PhysicalSectorSize: 4096, Rotational: true,
	}
	handle := &recordingReadHandle{}
	wakeCalls := 0
	readsWhenWoken := -1
	m := newTestManager(t, JobManagerOptions{
		ResolveDisk: func(string) (*Disk, error) { return disk, nil },
		Open:        func(string) (ReadHandle, error) { return handle, nil },
		WakeDisk: func(got Disk) error {
			wakeCalls++
			readsWhenWoken = handle.calls
			if got.Path != disk.Path {
				t.Fatalf("wake disk = %s, want %s", got.Path, disk.Path)
			}
			return nil
		},
	})
	job, err := m.create("scan", disk.Path, map[string]interface{}{"scanMode": "quick"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	done := waitForStatus(t, m, job.ID, "completed", 5*time.Second)
	result := done.Result.(*ScanResult)
	if wakeCalls != 1 || readsWhenWoken != 0 {
		t.Fatalf("wake calls = %d, reads before wake = %d", wakeCalls, readsWhenWoken)
	}
	extraReads := 0
	for _, cell := range result.Map {
		if cell.Recheck != nil {
			extraReads += int(cell.Recheck.Reads)
		}
	}
	if handle.calls != quickScanCellCount+extraReads {
		t.Fatalf("read calls = %d, want %d scan and recheck reads", handle.calls, quickScanCellCount+extraReads)
	}
	if result.Stats.TestedCells != quickScanCellCount || result.Stats.BytesRead != disk.Size {
		t.Fatalf("unexpected scan stats: %+v", result.Stats)
	}
	if result.Map[0].Reads != 1 || result.Map[0].BytesRead != disk.Size/quickScanCellCount {
		t.Fatalf("unexpected first cell: %+v", result.Map[0])
	}
}

func TestMockSpeedOneClick(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{Mock: true})
	job, err := m.create("speed", "/dev/sda", map[string]interface{}{"sampleMiB": float64(256), "speedRuns": float64(3), "speedPosition": "middle"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	done := waitForStatus(t, m, job.ID, "completed", 10*time.Second)
	if done.Options.SpeedPosition == nil || *done.Options.SpeedPosition != "middle" {
		t.Errorf("speedPosition = %v", done.Options.SpeedPosition)
	}
	if done.Options.SpeedRuns == nil || *done.Options.SpeedRuns != 3 {
		t.Errorf("speedRuns = %v", done.Options.SpeedRuns)
	}
	result := done.Result.(*SpeedResult)
	if result.SampleMiB != 256 {
		t.Errorf("sampleMiB = %d", result.SampleMiB)
	}
	if result.Runs != 3 {
		t.Errorf("runs = %d", result.Runs)
	}
	if len(result.Benchmarks) != 12 {
		t.Fatalf("benchmarks = %d, want 12", len(result.Benchmarks))
	}
	if result.Benchmarks[0].Operation != "read" || result.Benchmarks[0].Pattern != "sequential" || result.Benchmarks[0].QueueDepth != 8 {
		t.Errorf("benchmark 0 = %+v", result.Benchmarks[0])
	}
	if result.Benchmarks[6].Pattern != "random" || result.Benchmarks[6].QueueDepth != 32 || result.Benchmarks[6].IOPS == nil {
		t.Errorf("benchmark 6 = %+v", result.Benchmarks[6])
	}
	if len(result.Benchmarks[0].Samples) != 1 || result.Benchmarks[0].Samples[0].Label != "中部" {
		t.Errorf("sequential samples = %+v", result.Benchmarks[0].Samples)
	}
}

func TestSpeedTargetsMountedStorageSpaceSnapshot(t *testing.T) {
	space := &StorageSpace{
		Path: "/vol3", Source: "/dev/mapper/vg-lv", FileSystem: "btrfs", Layout: "raid1",
		Members: []string{"/dev/sdb", "/dev/sdf"}, Size: 2_000_000_000_000, FreeBytes: 1_000_000_000_000,
	}
	m := newTestManager(t, JobManagerOptions{
		Mock: true,
		ResolveSpace: func(path string) (*StorageSpace, error) {
			if path == space.Path {
				return space, nil
			}
			return nil, nil
		},
	})
	job, err := m.create("speed", "/vol3", map[string]interface{}{
		"sampleMiB": float64(256),
		"expectedStorageSpace": map[string]interface{}{
			"path": space.Path, "source": space.Source, "fileSystem": space.FileSystem,
			"layout": space.Layout, "members": []interface{}{space.Members[0], space.Members[1]}, "size": float64(space.Size),
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if job.Device != "/vol3" || job.StorageSpace == nil || job.StorageSpace.Layout != "raid1" || len(job.StorageSpace.Members) != 2 {
		t.Fatalf("job target = %+v", job)
	}
	waitForStatus(t, m, job.ID, "completed", 10*time.Second)
}

func TestExternalMountedDiskUsesFilesystemSpeedTarget(t *testing.T) {
	space := &StorageSpace{
		Path: "/media/Backup SSD", Source: "/dev/sdg1", FileSystem: "exfat", Layout: "single",
		Members: []string{"/dev/sdg"}, Size: 1_000_000_000_000, FreeBytes: 500_000_000_000,
	}
	m := newTestManager(t, JobManagerOptions{Mock: true, ResolveSpace: func(value string) (*StorageSpace, error) {
		if value == space.Path {
			return space, nil
		}
		return nil, nil
	}})
	job, err := m.create("speed", space.Path, map[string]interface{}{
		"speedTarget": "filesystem", "sampleMiB": float64(256),
	})
	if err != nil {
		t.Fatalf("create external speed: %v", err)
	}
	if job.StorageSpace == nil || job.StorageSpace.Path != space.Path || job.Device != space.Path {
		t.Fatalf("external speed target = %+v", job)
	}
	waitForStatus(t, m, job.ID, "completed", 10*time.Second)
}

func TestReadOnlyExternalMountAllowsReadButRejectsWrites(t *testing.T) {
	space := &StorageSpace{
		Path: "/media/Archive", Source: "/dev/sdh1", FileSystem: "ntfs", Layout: "single",
		Members: []string{"/dev/sdh"}, Size: 1_000_000_000_000, FreeBytes: 500_000_000_000, ReadOnly: true,
	}
	resolver := func(string) (*StorageSpace, error) { return space, nil }
	readManager := newTestManager(t, JobManagerOptions{Mock: true, ResolveSpace: resolver})
	readJob, err := readManager.create("speed", space.Path, map[string]interface{}{
		"speedTarget": "filesystem", "speedOperation": "read", "sampleMiB": float64(256),
	})
	if err != nil {
		t.Fatalf("read-only external read: %v", err)
	}
	waitForStatus(t, readManager, readJob.ID, "completed", 10*time.Second)

	writeManager := newTestManager(t, JobManagerOptions{Mock: true, ResolveSpace: resolver})
	_, err = writeManager.create("speed", space.Path, map[string]interface{}{
		"speedTarget": "filesystem", "speedOperation": "write", "sampleMiB": float64(256),
	})
	var jobErr *jobError
	if !errorsAs(err, &jobErr) || jobErr.apiCode != "STORAGE_SPACE_READ_ONLY" {
		t.Fatalf("write err = %v, want STORAGE_SPACE_READ_ONLY", err)
	}
}

func TestRealSpeedRejectsRawPhysicalDeviceWithRecognizedFileSystem(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{
		Mock: false,
		ResolveDisk: func(string) (*Disk, error) {
			d := testDisk()
			d.HasPartitions = true
			d.FileSystems = []string{"ext4"}
			return d, nil
		},
	})
	_, err := m.create("speed", "/dev/sda", map[string]interface{}{"sampleMiB": float64(256)})
	var jobErr *jobError
	if !errorsAs(err, &jobErr) || jobErr.apiCode != "RAW_SPEED_NOT_ALLOWED" {
		t.Fatalf("err = %v, want RAW_SPEED_NOT_ALLOWED", err)
	}
}

func TestRealRawWriteRequiresAcknowledgement(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{Mock: false})
	_, err := m.create("speed", "/dev/sda", map[string]interface{}{
		"speedTarget": "raw", "sampleMiB": float64(256), "speedOperation": "write",
	})
	var jobErr *jobError
	if !errorsAs(err, &jobErr) || jobErr.apiCode != "RAW_SPEED_CONFIRMATION_REQUIRED" {
		t.Fatalf("err = %v, want RAW_SPEED_CONFIRMATION_REQUIRED", err)
	}
}

func TestRawSpeedRejectsSampleLargerThanDevice(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{Mock: true, ResolveDisk: func(string) (*Disk, error) {
		d := testDisk()
		d.Size = 128 * 1024 * 1024
		return d, nil
	}})
	_, err := m.create("speed", "/dev/sda", map[string]interface{}{"speedTarget": "raw", "sampleMiB": float64(256)})
	var jobErr *jobError
	if !errorsAs(err, &jobErr) || jobErr.apiCode != "DISK_TOO_SMALL" {
		t.Fatalf("err = %v, want DISK_TOO_SMALL", err)
	}
}

func TestCancelJob(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{Mock: true})
	job, err := m.create("scan", "/dev/sda", map[string]interface{}{"scanMode": "quick"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := m.cancel(job.ID); err != nil {
		// A queued job may already have started; cancellation of a running job
		// still settles as cancelled.
		if err.Error() != "任务已结束，无法取消" {
			t.Fatalf("cancel: %v", err)
		}
	}
	done := waitForStatus(t, m, job.ID, "cancelled", 5*time.Second)
	if done.Phase != "任务已取消" {
		t.Errorf("phase = %q", done.Phase)
	}
}

func TestRemoveCompletedJobDeletesMemoryAndHistoryFile(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{Mock: true})
	job, err := m.create("scan", "/dev/sda", map[string]interface{}{"scanMode": "quick"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	waitForStatus(t, m, job.ID, "completed", 5*time.Second)
	path := filepath.Join(m.jobsDir, job.ID+".json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("history file before remove: %v", err)
	}
	if err := m.remove(job.ID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if m.get(job.ID) != nil {
		t.Fatal("job remained in memory after remove")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("history file still exists after remove: %v", err)
	}
}

func TestRemoveRejectsActiveJob(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{Mock: true})
	id := "00000000-0000-4000-8000-000000000001"
	m.mu.Lock()
	m.jobs[id] = &Job{ID: id, Status: "running"}
	m.mu.Unlock()
	var jobErr *jobError
	if err := m.remove(id); !errorsAs(err, &jobErr) || jobErr.apiCode != "JOB_ACTIVE" {
		t.Fatalf("remove active err = %v, want JOB_ACTIVE", err)
	}
}

func TestHistoryPruneKeeps30(t *testing.T) {
	dataDir := t.TempDir()
	jobsDir := filepath.Join(dataDir, "jobs")
	if err := os.MkdirAll(jobsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := 0; i < 32; i++ {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		ids = append(ids, id)
		job := map[string]interface{}{
			"id": id, "type": "scan", "device": "/dev/sda", "status": "completed",
			"createdAt": time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC).Format("2006-01-02T15:04:05.000Z"),
			"result":    nil, "log": []interface{}{},
		}
		data, _ := json.Marshal(job)
		_ = os.WriteFile(filepath.Join(jobsDir, id+".json"), data, 0o600)
	}
	m := NewJobManager(dataDir, JobManagerOptions{Mock: true, ResolveDisk: func(string) (*Disk, error) { return testDisk(), nil }, Logger: silentLogger{}, Now: time.Now})
	defer m.shutdown()

	if got := len(m.list(false)); got != 30 {
		t.Fatalf("list length = %d, want 30", got)
	}
	if m.get(ids[0]) != nil || m.get(ids[1]) != nil {
		t.Error("oldest jobs should be pruned")
	}
	first := m.list(false)[0].(*Job)
	if first.ID != ids[31] {
		t.Errorf("newest = %s, want %s", first.ID, ids[31])
	}
}

func TestCreateRejectsInvalidOptions(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{Mock: true})
	cases := []struct {
		typeName string
		options  map[string]interface{}
		apiCode  string
	}{
		{"scan", map[string]interface{}{"scanMode": "ful"}, "INVALID_SCAN_MODE"},
		{"speed", map[string]interface{}{"sampleMiB": float64(123), "speedPosition": "all"}, "INVALID_SAMPLE_SIZE"},
		{"speed", map[string]interface{}{"sampleMiB": float64(256), "speedRuns": float64(10)}, "INVALID_SPEED_RUNS"},
		{"speed", map[string]interface{}{"speedPreset": "invalid"}, "INVALID_SPEED_PRESET"},
		{"speed", map[string]interface{}{"sampleMiB": float64(256), "speedPosition": "middel"}, "INVALID_SPEED_POSITION"},
		{"speed", map[string]interface{}{"sampleMiB": float64(256), "speedOperation": "invalid"}, "INVALID_SPEED_OPERATION"},
		{"speed", map[string]interface{}{"sampleMiB": float64(256), "speedQueueDepth": float64(7)}, "INVALID_SPEED_QUEUE_DEPTH"},
		{"speed", map[string]interface{}{"sampleMiB": float64(256), "speedThreads": float64(3)}, "INVALID_SPEED_THREADS"},
		{"speed", map[string]interface{}{"sampleMiB": float64(256), "speedIOEngine": "threads"}, "INVALID_SPEED_IO_ENGINE"},
		{"speed", map[string]interface{}{"sampleMiB": float64(256), "speedIOEngine": "psync", "speedQueueDepth": float64(8)}, "INVALID_SPEED_QUEUE_DEPTH"},
	}
	for _, c := range cases {
		_, err := m.create(c.typeName, "/dev/sda", c.options)
		var je *jobError
		if !errorsAs(err, &je) || je.apiCode != c.apiCode {
			t.Errorf("create(%s, %v) = %v, want %s", c.typeName, c.options, err, c.apiCode)
		}
	}
	if got := len(m.list(false)); got != 0 {
		t.Errorf("list length = %d, want 0", got)
	}
}

func errorsAs(err error, target **jobError) bool {
	if err == nil {
		return false
	}
	je, ok := err.(*jobError)
	if ok {
		*target = je
	}
	return ok
}

func TestCreateRejectsZeroCapacity(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{Mock: true, ResolveDisk: func(string) (*Disk, error) {
		d := testDisk()
		d.Size = 0
		return d, nil
	}})
	_, err := m.create("scan", "/dev/sda", map[string]interface{}{"scanMode": "full"})
	var je *jobError
	if !errorsAs(err, &je) || je.apiCode != "DISK_SIZE_INVALID" || je.statusCode != 409 {
		t.Errorf("err = %v", err)
	}
	if got := len(m.list(false)); got != 0 {
		t.Errorf("list length = %d", got)
	}
}

func TestCreateRejectsTooSmall(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{Mock: true, ResolveDisk: func(string) (*Disk, error) {
		d := testDisk()
		d.Size = scanCellCount*sectorAlignment - 512
		return d, nil
	}})
	_, err := m.create("scan", "/dev/sda", map[string]interface{}{"scanMode": "full"})
	var je *jobError
	if !errorsAs(err, &je) || je.apiCode != "DISK_GEOMETRY_INVALID" {
		t.Errorf("err = %v", err)
	}
}

func TestCreateRejectsIdentityChanged(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{Mock: true, ResolveDisk: func(string) (*Disk, error) {
		d := testDisk()
		d.Serial = "REPLACEMENT"
		return d, nil
	}})
	_, err := m.create("scan", "/dev/sda", map[string]interface{}{
		"scanMode": "quick",
		"expectedDisk": map[string]interface{}{
			"serial": "TEST", "model": "Test disk", "size": float64(testDisk().Size),
		},
	})
	var je *jobError
	if !errorsAs(err, &je) || je.apiCode != "DISK_CHANGED" {
		t.Errorf("err = %v", err)
	}
}

func TestShutdownInterruptsActiveJob(t *testing.T) {
	blocked := make(chan struct{})
	m := newTestManager(t, JobManagerOptions{
		Mock: true,
		Delay: func(time.Duration) {
			<-blocked
		},
	})
	job, err := m.create("speed", "/dev/sda", map[string]interface{}{"sampleMiB": float64(256), "speedPosition": "middle"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	waitForStatus(t, m, job.ID, "running", 5*time.Second)

	m.shutdown()
	close(blocked)

	done := m.get(job.ID)
	if done.Status != "interrupted" || done.Phase != "任务已中断" || done.Message != "应用服务已停止" {
		t.Errorf("status=%s phase=%s message=%s", done.Status, done.Phase, done.Message)
	}
}

func TestCreateRejectsRetiredDeepScanBeforeAccessingDisk(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{Mock: true, ResolveDisk: func(string) (*Disk, error) {
		t.Fatal("retired deep scan accessed the disk")
		return nil, nil
	}})
	_, err := m.create("write", "/dev/sda", map[string]interface{}{})
	var je *jobError
	if !errorsAs(err, &je) || je.apiCode != "JOB_TYPE_RETIRED" || je.statusCode != 410 {
		t.Errorf("err = %v", err)
	}
	if got := len(m.list(false)); got != 0 {
		t.Errorf("list length = %d", got)
	}
}

func TestRetiredDeepScanHistoryStillLoadsWithoutResuming(t *testing.T) {
	for _, status := range []string{"completed", "running"} {
		t.Run(status, func(t *testing.T) {
			m := newTestManager(t, JobManagerOptions{Mock: true})
			job := &Job{
				ID: "00000000-0000-4000-8000-000000000001", Type: "write", Device: "/dev/sda", Status: status,
				Result: &ScanResult{
					ScanMode: "write", Coverage: "full", LatencyBasis: "solid-write-dual-v2",
					Map: []ScanCell{{Index: 0, Start: 0, End: 512, Status: "normal",
						MaxReadLatency:  &ScanReadLatency{Ms: 1, Bytes: 512, Offset: 0},
						MaxWriteLatency: &ScanReadLatency{Ms: 2, Bytes: 512, Offset: 0}}},
				},
			}
			data, err := json.Marshal(job)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(m.jobsDir, job.ID+".json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			m.loadJobs()
			loaded := m.get(job.ID)
			if loaded == nil || loaded.Type != "write" {
				t.Fatal("legacy deep scan history missing")
			}
			wantStatus := status
			if status == "running" {
				wantStatus = "interrupted"
			}
			if loaded.Status != wantStatus {
				t.Fatalf("status = %q, want %q", loaded.Status, wantStatus)
			}
			result, ok := loaded.Result.(*ScanResult)
			if !ok || result.ScanMode != "write" || result.Coverage != "full" || len(result.Map) != 1 || result.Map[0].MaxReadLatency == nil || result.Map[0].MaxWriteLatency == nil || result.Map[0].MaxReadLatency.Ms != 1 || result.Map[0].MaxWriteLatency.Ms != 2 {
				t.Fatalf("legacy write/read result changed: %+v", loaded.Result)
			}
		})
	}
}

func TestFindPatternMismatches(t *testing.T) {
	buffer := make([]byte, 3*writeVerifyPageSize)
	fillPattern(buffer, 0x55)
	buffer[writeVerifyPageSize+10] = 0x00
	buffer[writeVerifyPageSize+11] = 0x00
	for i := 0; i < writeVerifyPageSize; i++ {
		buffer[2*writeVerifyPageSize+i] = 0x00
	}
	ranges := findPatternMismatches(buffer, 0x55, 100000, 10)
	if len(ranges) != 2 {
		t.Fatalf("ranges = %d, want 2", len(ranges))
	}
	if ranges[0].Offset != 100000+writeVerifyPageSize || ranges[0].Length != writeVerifyPageSize || ranges[0].Code != "WRITE_VERIFY_MISMATCH" {
		t.Errorf("range0 = %+v", ranges[0])
	}
	if ranges[1].Offset != 100000+2*writeVerifyPageSize {
		t.Errorf("range1 = %+v", ranges[1])
	}
}

type corruptingHandle struct {
	data          []byte
	corruptOffset int
}

func (h *corruptingHandle) WriteAt(p []byte, off int64) (int, error) {
	copy(h.data[off:], p)
	return len(p), nil
}

func (h *corruptingHandle) ReadAt(p []byte, off int64) (int, error) {
	copy(p, h.data[off:])
	if int64(h.corruptOffset) >= off && int64(h.corruptOffset) < off+int64(len(p)) {
		p[h.corruptOffset-int(off)] ^= 0xFF
	}
	return len(p), nil
}

func (h *corruptingHandle) Close() error { return nil }

func TestWriteVerifyRegionRoundTripAndMismatch(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{Mock: true})
	job := &Job{Status: "running"}
	const length = 3 * writeVerifyPageSize

	clean := &corruptingHandle{data: make([]byte, length), corruptOffset: -1}
	buffer := make([]byte, length)
	cleanResult, err := m.writeVerifyRegion(clean, 0x55, buffer, length, 0, job, 10)
	if err != nil {
		t.Fatalf("clean write: %v", err)
	}
	if cleanResult.errorCount != 0 || len(cleanResult.badRanges) != 0 || cleanResult.bytesWritten != length {
		t.Errorf("clean result = %+v", cleanResult)
	}

	corrupt := &corruptingHandle{data: make([]byte, length), corruptOffset: writeVerifyPageSize + 5}
	corruptResult, err := m.writeVerifyRegion(corrupt, 0x55, buffer, length, 0, job, 10)
	if err != nil {
		t.Fatalf("corrupt write: %v", err)
	}
	if corruptResult.errorCount != 1 || len(corruptResult.badRanges) != 1 {
		t.Fatalf("corrupt result = %+v", corruptResult)
	}
	if corruptResult.badRanges[0].Offset != writeVerifyPageSize || corruptResult.badRanges[0].Code != "WRITE_VERIFY_MISMATCH" {
		t.Errorf("badRange = %+v", corruptResult.badRanges[0])
	}
}

func TestMockSpeedOneClickOnMountedStorageIncludesWrites(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{Mock: true, ResolveSpace: func(string) (*StorageSpace, error) { return testSpace(), nil }})
	job, err := m.create("speed", "/vol1", map[string]interface{}{"sampleMiB": float64(256)})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	done := waitForStatus(t, m, job.ID, "completed", 10*time.Second)
	result := done.Result.(*SpeedResult)
	if len(result.Benchmarks) != 12 {
		t.Fatalf("benchmarks = %d, want 12", len(result.Benchmarks))
	}
	if result.Benchmarks[0].Operation != "read" || result.Benchmarks[1].Operation != "write" || result.Benchmarks[2].Operation != "mixed" {
		t.Errorf("benchmarks should include temporary-file reads and writes: %+v", result.Benchmarks)
	}
	if result.Benchmarks[0].Pattern != "sequential" || result.Benchmarks[6].Pattern != "random" {
		t.Errorf("patterns = %s, %s", result.Benchmarks[0].Pattern, result.Benchmarks[6].Pattern)
	}
	for _, benchmark := range result.Benchmarks {
		if benchmark.IOPS == nil || *benchmark.IOPS <= 0 {
			t.Errorf("%s %s should report iops", benchmark.Pattern, benchmark.Operation)
		}
	}
}

func TestMockRawSpeedOneClickIncludesReadsWritesAndMixed(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{Mock: true})
	job, err := m.create("speed", "/dev/sda", map[string]interface{}{
		"speedTarget": "raw", "sampleMiB": float64(256),
	})
	if err != nil {
		t.Fatalf("create raw speed: %v", err)
	}
	done := waitForStatus(t, m, job.ID, "completed", 10*time.Second)
	if done.StorageSpace != nil || done.Options.SpeedTarget == nil || *done.Options.SpeedTarget != "raw" {
		t.Fatalf("target = %+v", done)
	}
	result := done.Result.(*SpeedResult)
	if len(result.Benchmarks) != 12 || result.Benchmarks[0].Operation != "read" || result.Benchmarks[1].Operation != "write" || result.Benchmarks[2].Operation != "mixed" {
		t.Fatalf("raw speed benchmarks = %+v", result.Benchmarks)
	}
}

func TestMockSpeedExplicitReadOnlyRandomOnWritableDisk(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{Mock: true})
	job, err := m.create("speed", "/dev/sda", map[string]interface{}{
		"sampleMiB":      float64(256),
		"speedOperation": "read",
		"speedPattern":   "random",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	done := waitForStatus(t, m, job.ID, "completed", 10*time.Second)
	result := done.Result.(*SpeedResult)
	if len(result.Benchmarks) != 2 {
		t.Fatalf("benchmarks = %d, want 2", len(result.Benchmarks))
	}
	for _, benchmark := range result.Benchmarks {
		if benchmark.Operation != "read" || benchmark.Pattern != "random" {
			t.Errorf("benchmark = %+v, want random read", benchmark)
		}
	}
}

func TestMockSpeedSequentialMiddleReturnsOnlyMiddleSample(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{Mock: true})
	job, err := m.create("speed", "/dev/sda", map[string]interface{}{
		"sampleMiB":      float64(256),
		"speedOperation": "read",
		"speedPattern":   "sequential",
		"speedPosition":  "middle",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	done := waitForStatus(t, m, job.ID, "completed", 10*time.Second)
	result := done.Result.(*SpeedResult)
	if len(result.Benchmarks) != 2 {
		t.Fatalf("benchmarks = %d, want 2", len(result.Benchmarks))
	}
	for _, benchmark := range result.Benchmarks {
		if benchmark.Operation != "read" || benchmark.Pattern != "sequential" {
			t.Fatalf("benchmark = %+v, want sequential read", benchmark)
		}
		if len(benchmark.Samples) != 1 || benchmark.Samples[0].Label != "中部" {
			t.Fatalf("samples = %+v, want only middle", benchmark.Samples)
		}
	}
}

func TestMockSpeedOneClickUsesStorageLayerEvenWhenMemberIsSystemDisk(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{Mock: true, ResolveSpace: func(string) (*StorageSpace, error) { return testSpace(), nil }, ResolveDisk: func(string) (*Disk, error) {
		d := testDisk()
		d.IsSystem = true
		return d, nil
	}})
	job, err := m.create("speed", "/vol1", map[string]interface{}{"sampleMiB": float64(256)})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	done := waitForStatus(t, m, job.ID, "completed", 10*time.Second)
	result := done.Result.(*SpeedResult)
	if len(result.Benchmarks) != 12 {
		t.Fatalf("benchmarks = %d, want 12", len(result.Benchmarks))
	}
}

func TestMockSpeedExactMatrixCombination(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{Mock: true})
	job, err := m.create("speed", "/dev/sda", map[string]interface{}{
		"sampleMiB":       float64(256),
		"speedOperation":  "mixed",
		"speedPattern":    "random",
		"speedBlockSize":  float64(4096),
		"speedQueueDepth": float64(32),
		"speedThreads":    float64(1),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	done := waitForStatus(t, m, job.ID, "completed", 10*time.Second)
	result := done.Result.(*SpeedResult)
	if len(result.Benchmarks) != 1 {
		t.Fatalf("benchmarks = %d, want 1", len(result.Benchmarks))
	}
	benchmark := result.Benchmarks[0]
	if benchmark.Operation != "mixed" || benchmark.Pattern != "random" || benchmark.BlockSize != 4096 || benchmark.QueueDepth != 32 || benchmark.Threads != 1 {
		t.Fatalf("benchmark = %+v, want RND4K Q32T1 mixed", benchmark)
	}
}

func TestMockSpeedConfiguredProfileUsesSelectedQueueAndThreads(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{Mock: true, ResolveSpace: func(string) (*StorageSpace, error) { return testSpace(), nil }})
	job, err := m.create("speed", "/vol1", map[string]interface{}{
		"sampleMiB":       float64(256),
		"speedBlockSize":  float64(65536),
		"speedQueueDepth": float64(64),
		"speedThreads":    float64(2),
		"speedIOEngine":   "io_uring",
		"speedDirectIO":   false,
		"speedTrim":       true,
		"speedProfile":    true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	done := waitForStatus(t, m, job.ID, "completed", 10*time.Second)
	result := done.Result.(*SpeedResult)
	if len(result.Benchmarks) != 12 {
		t.Fatalf("benchmarks = %d, want 12", len(result.Benchmarks))
	}
	if result.Benchmarks[0].BlockSize != 65536 || result.Benchmarks[0].QueueDepth != 64 || result.Benchmarks[0].Threads != 2 {
		t.Fatalf("configured benchmark = %+v", result.Benchmarks[0])
	}
	if done.Options.SpeedDirectIO == nil || *done.Options.SpeedDirectIO || done.Options.SpeedTrim == nil || !*done.Options.SpeedTrim {
		t.Fatalf("options = %+v", done.Options)
	}
}

func TestExternalFilesystemSpeedDisablesTrim(t *testing.T) {
	external := &StorageSpace{
		Path: "/vol00/INTEL\\x20SSDSC2BA400G4R_1", Source: "/dev/sdh1", FileSystem: "ext4",
		Members: []string{"/dev/sdh"}, Size: 1_000_000_000_000, FreeBytes: 500_000_000_000,
	}
	m := newTestManager(t, JobManagerOptions{Mock: true, ResolveSpace: func(string) (*StorageSpace, error) { return external, nil }})
	job, err := m.create("speed", external.Path, map[string]interface{}{
		"speedTarget": "filesystem", "speedTrim": true, "sampleMiB": float64(256),
	})
	if err != nil {
		t.Fatalf("create external speed: %v", err)
	}
	if job.Options.SpeedTrim == nil || *job.Options.SpeedTrim {
		t.Fatalf("external trim must be disabled, options = %+v", job.Options)
	}
}

func TestParseFioSampleCombinesConcurrentJobs(t *testing.T) {
	output := []byte(`{"jobs":[{"read":{"io_bytes":1048576,"runtime":1000},"write":{"io_bytes":0,"runtime":0}},{"read":{"io_bytes":1048576,"runtime":950},"write":{"io_bytes":0,"runtime":0}}]}`)
	sample, err := parseFioSample(output, "随机", 4096)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sample.Bytes != 2*1048576 || sample.Seconds != 1 || sample.MBps != 2.1 || sample.Label != "随机" || sample.OffsetBytes != 4096 {
		t.Fatalf("sample = %+v", sample)
	}
}
