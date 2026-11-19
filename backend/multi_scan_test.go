package main

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func multiScanFixture(t *testing.T) (*JobManager, map[string]*blockedScanHandle) {
	t.Helper()
	handles := map[string]*blockedScanHandle{}
	for _, name := range []string{"sda", "sdb", "sdc"} {
		handles["/dev/"+name] = &blockedScanHandle{
			started: make(chan struct{}), releaseRead: make(chan struct{}), releaseClose: make(chan struct{}),
		}
	}
	m := newTestManager(t, JobManagerOptions{
		ResolveDisk: func(path string) (*Disk, error) {
			if path == "/dev/alias-a" {
				path = "/dev/sda"
			}
			disk := testDisk()
			disk.Path, disk.Name, disk.Serial, disk.Size = path, path[5:], path, 4096*4096
			return disk, nil
		},
		ResolveSpace:    func(string) (*StorageSpace, error) { return testSpace(), nil },
		FindExecutable:  func(string) string { return "fixture-only" },
		Open:            func(path string) (ReadHandle, error) { return handles[path], nil },
		ScanEvidence:    func(Disk) ScanEvidence { return ScanEvidence{} },
		ScanReadTimeout: time.Minute,
	})
	t.Cleanup(func() {
		for _, h := range handles {
			for _, channel := range []chan struct{}{h.releaseRead, h.releaseClose} {
				select {
				case <-channel:
				default:
					close(channel)
				}
			}
		}
	})
	return m, handles
}

func startBlockedDiskScan(t *testing.T, m *JobManager, handles map[string]*blockedScanHandle, device string) *Job {
	t.Helper()
	job, err := m.create("scan", device, map[string]interface{}{"scanMode": "full"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-handles[device].started:
	case <-time.After(3 * time.Second):
		t.Fatal("scan never reached independent device I/O: " + device)
	}
	return job
}

func requireJobConflict(t *testing.T, err error) {
	t.Helper()
	var conflict *jobError
	if !errors.As(err, &conflict) || conflict.apiCode != "JOB_CONFLICT" || conflict.statusCode != 409 {
		t.Fatalf("expected device conflict, got %v", err)
	}
}

func TestParallelScansKeepCancellationAndPendingIOOnTheirOwnDisk(t *testing.T) {
	m, handles := multiScanFixture(t)
	a := startBlockedDiskScan(t, m, handles, "/dev/sda")
	b := startBlockedDiskScan(t, m, handles, "/dev/sdb")
	_, err := m.create("scan", "/dev/alias-a", nil)
	requireJobConflict(t, err)
	_, err = m.create("speed", "/vol1", map[string]interface{}{"speedTarget": "filesystem"})
	requireJobConflict(t, err)
	if len(m.listCurrent(true, nil)) != 2 {
		t.Fatal("current job list omitted a parallel scan")
	}
	if _, err = m.cancel(a.ID); err != nil {
		t.Fatal(err)
	}
	close(handles[a.Device].releaseRead)
	if m.get(a.ID).Status != "cancelling" || m.get(b.ID).Status != "running" {
		t.Fatal("stopping one disk affected another, or released a pending close")
	}
	_, err = m.create("scan", a.Device, nil)
	requireJobConflict(t, err)
	c := startBlockedDiskScan(t, m, handles, "/dev/sdc")
	close(handles[a.Device].releaseClose)
	waitForStatus(t, m, a.ID, "cancelled", 3*time.Second)
	if m.get(b.ID).Status != "running" || m.get(c.ID).Status != "running" {
		t.Fatal("cancelling a scan stopped unrelated disks")
	}
	for _, job := range []*Job{b, c} {
		if _, err = m.cancel(job.ID); err != nil {
			t.Fatal(err)
		}
		close(handles[job.Device].releaseRead)
		close(handles[job.Device].releaseClose)
		waitForStatus(t, m, job.ID, "cancelled", 3*time.Second)
	}
}

func TestOneDiskCanCompleteWhileAnotherDiskIsBlocked(t *testing.T) {
	m, handles := multiScanFixture(t)
	a := startBlockedDiskScan(t, m, handles, "/dev/sda")
	b := startBlockedDiskScan(t, m, handles, "/dev/sdb")
	close(handles[b.Device].releaseRead)
	close(handles[b.Device].releaseClose)
	done := waitForStatus(t, m, b.ID, "completed", 5*time.Second)
	if done.Result.(*ScanResult).Stats.TestedCells != 4096 || m.get(a.ID).Status != "running" {
		t.Fatal("blocked disk prevented another disk from completing")
	}
	if _, err := m.scanProgress(b.ID, 0); err != nil {
		t.Fatal(err)
	}
	shutdownDone := make(chan struct{})
	go func() { m.shutdown(); close(shutdownDone) }()
	waitForStatus(t, m, a.ID, "interrupted", 3*time.Second)
	close(handles[a.Device].releaseRead)
	close(handles[a.Device].releaseClose)
	<-shutdownDone
	restored := NewJobManager(m.dataDir, JobManagerOptions{Logger: silentLogger{}})
	if restored.get(a.ID).Status != "interrupted" || restored.get(b.ID).Status != "completed" {
		t.Fatal("restart mixed the disks' saved scan states")
	}
}

func TestOneDiskFailureDoesNotStopOtherScans(t *testing.T) {
	m, handles := multiScanFixture(t)
	open := m.openFn
	m.openFn = func(device string) (ReadHandle, error) {
		if device == "/dev/sdb" {
			return nil, errors.New("fixture device disconnected")
		}
		return open(device)
	}
	a := startBlockedDiskScan(t, m, handles, "/dev/sda")
	b, err := m.create("scan", "/dev/sdb", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, m, b.ID, "failed", 3*time.Second)
	if m.get(a.ID).Status != "running" {
		t.Fatal("another disk's failure stopped a healthy scan")
	}
	startBlockedDiskScan(t, m, handles, "/dev/sdc")
}

func TestShutdownSavesEveryParallelScanSeparately(t *testing.T) {
	m, handles := multiScanFixture(t)
	a := startBlockedDiskScan(t, m, handles, "/dev/sda")
	b := startBlockedDiskScan(t, m, handles, "/dev/sdb")
	done := make(chan struct{})
	go func() { m.shutdown(); close(done) }()
	for _, job := range []*Job{a, b} {
		waitForStatus(t, m, job.ID, "interrupted", 3*time.Second)
		close(handles[job.Device].releaseRead)
		close(handles[job.Device].releaseClose)
	}
	<-done
	restored := NewJobManager(m.dataDir, JobManagerOptions{Logger: silentLogger{}})
	for _, job := range []*Job{a, b} {
		saved := restored.get(job.ID)
		if saved.Status != "interrupted" || saved.Device != job.Device || saved.Disk.Serial != job.Disk.Serial {
			t.Fatalf("lost a disk's checkpoint on restart: %+v", saved)
		}
	}
}

func TestSimultaneousScanAdmissionIsAtomicPerDisk(t *testing.T) {
	for _, sameDisk := range []bool{false, true} {
		t.Run(fmt.Sprintf("sameDisk=%v", sameDisk), func(t *testing.T) {
			m, _ := multiScanFixture(t)
			resolve := m.resolveDisk
			var arrived sync.WaitGroup
			arrived.Add(2)
			proceed := make(chan struct{})
			var resolves sync.Mutex
			calls := 0
			m.resolveDisk = func(path string) (*Disk, error) {
				resolves.Lock()
				calls++
				initial := calls <= 2
				resolves.Unlock()
				if initial {
					arrived.Done()
					<-proceed
				}
				return resolve(path)
			}
			errors := make(chan error, 2)
			for i := 0; i < 2; i++ {
				device := "/dev/sda"
				if i == 1 && !sameDisk {
					device = "/dev/sdb"
				}
				go func() { _, err := m.create("scan", device, nil); errors <- err }()
			}
			arrived.Wait()
			close(proceed)
			successes := 0
			for i := 0; i < 2; i++ {
				if err := <-errors; err == nil {
					successes++
				} else {
					requireJobConflict(t, err)
				}
			}
			want := 2
			if sameDisk {
				want = 1
			}
			if successes != want {
				t.Fatalf("admitted %d scans, want %d", successes, want)
			}
		})
	}
}

func TestScanAdmissionRetainsResourcesAfterTerminalStatus(t *testing.T) {
	m, _ := multiScanFixture(t)
	job := &Job{ID: newUUID(), Type: "scan", Device: "/dev/sda", Disk: DiskSnapshot{Serial: "A"}, Status: "failed"}
	m.jobs[job.ID] = job
	cancel := m.beginCancellation(job)
	m.mu.Lock()
	same := &Job{Type: "scan", Device: "/dev/sda"}
	other := &Job{Type: "scan", Device: "/dev/sdb", Disk: DiskSnapshot{Serial: "B"}}
	sameError := m.jobAdmissionErrorLocked(same)
	otherError := m.jobAdmissionErrorLocked(other)
	m.mu.Unlock()
	requireJobConflict(t, sameError)
	if otherError != nil {
		t.Fatal(otherError)
	}
	if err := m.remove(job.ID); err == nil {
		t.Fatal("deleted a task with pending I/O")
	}
	m.endCancellation(job, cancel)
}

func TestScanAdmissionRejectsSameSerialAtAnotherPath(t *testing.T) {
	m := &JobManager{jobs: map[string]*Job{
		"running": {ID: "running", Type: "scan", Device: "/dev/sda", Disk: DiskSnapshot{Serial: "same-physical-disk"}, Status: "running"},
	}}
	candidate := &Job{Type: "scan", Device: "/dev/sdz", Disk: DiskSnapshot{Serial: "same-physical-disk"}}
	requireJobConflict(t, m.jobAdmissionErrorLocked(candidate))
}

func TestScanAndSpeedAdmissionByPhysicalDisk(t *testing.T) {
	scan := &Job{ID: "scan", Type: "scan", Status: "running", Device: "/dev/sdb", Disk: DiskSnapshot{Serial: "B"}}
	for _, tc := range []struct {
		name     string
		device   string
		serial   string
		space    *StorageSpaceSnapshot
		conflict bool
	}{
		{name: "raw same path", device: "/dev/sdb", conflict: true},
		{name: "raw same serial", device: "/dev/sdz", serial: "B", conflict: true},
		{name: "raw other disk", device: "/dev/sdc", serial: "C"},
		{name: "array second member", device: "/vol1", space: &StorageSpaceSnapshot{Members: []string{"/dev/sda", "/dev/sdb"}}, conflict: true},
		{name: "array member by serial", device: "/vol1", space: &StorageSpaceSnapshot{Members: []string{"/dev/sda", "/dev/sdz"}, MemberDisks: []DiskSnapshot{{Serial: "A"}, {Serial: "B"}}}, conflict: true},
		{name: "array other disks", device: "/vol1", space: &StorageSpaceSnapshot{Members: []string{"/dev/sda", "/dev/sdc"}}},
		{name: "system disk", device: "/tmp", space: &StorageSpaceSnapshot{Members: []string{"/dev/sdb"}, IsSystem: true}, conflict: true},
		{name: "unresolved members", device: "/vol1", space: &StorageSpaceSnapshot{}, conflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			speed := &Job{ID: "speed", Type: "speed", Status: "running", Device: tc.device, Disk: DiskSnapshot{Serial: tc.serial}, StorageSpace: tc.space}
			for _, pair := range [][2]*Job{{scan, speed}, {speed, scan}} {
				current, candidate := pair[0], pair[1]
				m := &JobManager{jobs: map[string]*Job{current.ID: current}}
				err := m.jobAdmissionErrorLocked(candidate)
				if tc.conflict {
					requireJobConflict(t, err)
				} else if err != nil {
					t.Fatalf("%s after %s should start: %v", candidate.Type, current.Type, err)
				}
			}
		})
	}
}

func TestSpeedAdmissionDoesNotDependOnTarget(t *testing.T) {
	current := &Job{ID: "first", Type: "speed", Device: "/vol1"}
	m := &JobManager{jobs: map[string]*Job{current.ID: current}}
	candidate := &Job{Type: "speed", Device: "/dev/sdb"}
	for _, status := range []string{"queued", "running", "cancelling"} {
		current.Status = status
		requireJobConflict(t, m.jobAdmissionErrorLocked(candidate))
	}
	current.Status = "completed"
	if err := m.jobAdmissionErrorLocked(candidate); err != nil {
		t.Fatalf("finished benchmark still blocks another target: %v", err)
	}
}
