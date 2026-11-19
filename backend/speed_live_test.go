package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Child process simulates fio output only; it never opens the supplied target.
func init() {
	mode := os.Getenv("DISKBENCH_TEST_FIO_STREAM")
	if mode == "" {
		return
	}
	args := strings.Join(os.Args[1:], " ")
	if !strings.Contains(args, "--time_based=1") || !strings.Contains(args, "--status-interval=1") {
		fmt.Fprintln(os.Stderr, "missing timed/stream arguments")
		os.Exit(2)
	}
	if mode == "mixed" && (!strings.Contains(args, "--rw=randrw ") || !strings.Contains(args, "--rwmixread=50 ")) {
		fmt.Fprintln(os.Stderr, "expected even random read/write mix")
		os.Exit(4)
	}
	duration := 0
	for _, arg := range os.Args[1:] {
		if strings.HasPrefix(arg, "--runtime=") {
			duration, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(arg, "--runtime="), "s"))
		}
	}
	if duration < 1 || duration > 10 {
		os.Exit(3)
	}
	fmt.Println(`{"jobs":[{"read":{"io_bytes":1000000,"runtime":1000}}]}`)
	if mode == "cancel" {
		time.Sleep(20 * time.Second)
		os.Exit(0)
	}
	time.Sleep(120 * time.Millisecond)
	if mode == "error" {
		fmt.Println(`{"jobs":[{"error":5,"read":{"io_bytes":1000000,"runtime":1000}}]}`)
		os.Exit(0)
	}
	if mode == "mixed" {
		fmt.Println(`{"jobs":[{"read":{"io_bytes":1500000,"runtime":2000},"write":{"io_bytes":1500000,"runtime":2000}}]}`)
	} else {
		fmt.Println(`{"jobs":[{"read":{"io_bytes":3000000,"runtime":2000}}]}`)
	}
	os.Exit(0)
}

func TestFioStreamDecodesConcatenatedDocuments(t *testing.T) {
	input := " {\n\"jobs\": [{\"read\": {\"io_bytes\": 0}}]\n}\n" + `{"jobs":[{"read":{"io_bytes":1048576,"runtime":1000}},{"read":{"io_bytes":1048576,"runtime":950}}]}`
	var decoded []json.RawMessage
	err := decodeFioStream(bytes.NewBufferString(input), func(raw json.RawMessage) error { decoded = append(decoded, raw); return nil })
	if err != nil || len(decoded) != 2 {
		t.Fatalf("stream: %v, %d", err, len(decoded))
	}
	b, seconds, err := fioCounters(decoded[0])
	if err != nil || b != 0 || seconds != 0 {
		t.Fatalf("zero startup must be valid: %v", err)
	}
	b, seconds, err = fioCounters(decoded[1])
	if err != nil || b != 2097152 || seconds != 1 {
		t.Fatalf("worker aggregation: %d %f %v", b, seconds, err)
	}
	if err := decodeFioStream(strings.NewReader(`{"jobs":`), func(json.RawMessage) error { return nil }); err == nil {
		t.Fatal("accepted truncated stream")
	}
}

func TestTimedFioFeedbackFinalResultAndCancellation(t *testing.T) {
	for _, mode := range []string{"success", "mixed", "cancel", "error"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("DISKBENCH_TEST_FIO_STREAM", mode)
			job := &Job{ID: "stream-test", Type: "speed", Status: "running", StorageSpace: &StorageSpaceSnapshot{}}
			m := &JobManager{}
			m.beginSpeedRun(job, 0, 1, 0, 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			spec := benchmarkSpec{Operation: "read", Pattern: "random", BlockSize: 4096, QueueDepth: 1, Threads: 1}
			if mode == "mixed" {
				spec.Operation = "mixed"
			}
			var sample SpeedSample
			done := make(chan error, 1)
			go func() {
				var err error
				sample, err = m.runFioSample(os.Args[0], "unused-test-target", job, ctx, spec, "随机", 0, 1024*1024, "psync", true, 10, 0)
				done <- err
			}()
			deadline := time.After(3 * time.Second)
			seen := false
			for !seen {
				m.mu.Lock()
				seen = job.SpeedLive.MBps != nil
				m.mu.Unlock()
				if seen {
					break
				}
				select {
				case err := <-done:
					t.Fatalf("finished before live feedback: %v", err)
				case <-deadline:
					t.Fatal("no live feedback")
				case <-time.After(5 * time.Millisecond):
				}
			}
			m.mu.Lock()
			live := *job.SpeedLive
			m.mu.Unlock()
			if live.Bytes != 1000000 || *live.MBps != 1 {
				t.Fatalf("bad live snapshot: %+v", live)
			}
			if mode == "cancel" {
				cancel()
			}
			select {
			case err := <-done:
				switch mode {
				case "success", "mixed":
					if err != nil || sample.Bytes != 3000000 || sample.RangeBytes != 1024*1024 || sample.MBps != 1.5 {
						t.Fatalf("final sample: %+v %v", sample, err)
					}
					if *job.SpeedLive.MBps != 2 {
						t.Fatal("live rate must use interval delta, not cumulative average")
					}
				case "cancel":
					if !errors.Is(err, errCancelled) {
						t.Fatalf("cancel: %v", err)
					}
				case "error":
					if err == nil || !strings.Contains(err.Error(), "错误码 5") {
						t.Fatalf("I/O error hidden: %v", err)
					}
				}
			case <-time.After(3 * time.Second):
				t.Fatal("fio did not finish/cancel")
			}
		})
	}
}

func TestTimedSpeedBudgetAndUnequalRepetitions(t *testing.T) {
	for _, count := range []int{1, 3} {
		var total int64
		for i := 0; i < count; i++ {
			total += speedSegmentSeconds(i, count)
		}
		if total != 10 {
			t.Fatalf("%d positions took %d seconds", count, total)
		}
	}
	spec := benchmarkSpec{Operation: "read", Pattern: "random", BlockSize: 4096, QueueDepth: 1, Threads: 1}
	results := []BenchmarkResult{
		{Samples: []SpeedSample{{Label: "随机", RangeBytes: 1048576, Bytes: 10000000, Seconds: 10}}},
		{Samples: []SpeedSample{{Label: "随机", RangeBytes: 1048576, Bytes: 30000000, Seconds: 10}}},
	}
	combined := combineBenchmarkRuns(spec, results)
	if combined.AverageMBps != 2 || combined.Samples[0].RangeBytes != 1048576 || combined.Samples[0].Bytes != 40000000 || combined.Samples[0].Seconds != 20 {
		t.Fatalf("wrong aggregation: %+v", combined)
	}
}

func TestCrystalSpeedOrderAndEngineLimits(t *testing.T) {
	specs := speedJobBenchmarks(crystalSpeedPreset, "", "", 4096, 64, 8, true, false, "", "io_uring")
	if len(specs) != 8 {
		t.Fatalf("expected four reads then four writes, got %+v", specs)
	}
	for i, spec := range specs {
		wantBlock := []int64{1048576, 1048576, 4096, 4096}[i%4]
		wantDepth := []int64{8, 1, 32, 1}[i%4]
		wantOperation := []string{"read", "write"}[i/4]
		if spec.BlockSize != wantBlock || spec.QueueDepth != wantDepth || spec.Threads != 1 || spec.Operation != wantOperation {
			t.Fatalf("unexpected spec %d: %+v", i, spec)
		}
	}
	filtered := speedJobBenchmarks(crystalSpeedPreset, "read", "", 0, 0, 0, true, false, "", "psync")
	if len(filtered) != 2 || filtered[0].ProfileRow != "seq-low" || filtered[1].ProfileRow != "rnd-low" {
		t.Fatalf("synchronous engine must only run real Q1 workloads: %+v", filtered)
	}
}

func TestCrystalAllSpeedUsesConfiguredWorkloadsAndMixed(t *testing.T) {
	specs := speedJobBenchmarks(crystalAllSpeedPreset, "", "", 65536, 16, 2, true, false, "", "io_uring")
	if len(specs) != 12 {
		t.Fatalf("expected four tests for each of three operations, got %d", len(specs))
	}
	for i, spec := range specs {
		if spec.Operation != []string{"read", "write", "mixed"}[i/4] ||
			spec.BlockSize != []int64{65536, 65536, 4096, 4096}[i%4] ||
			spec.QueueDepth != []int64{16, 1, 16, 1}[i%4] || spec.Threads != []int64{2, 1, 2, 1}[i%4] {
			t.Fatalf("unexpected spec %d: %+v", i, spec)
		}
	}
	filtered := speedJobBenchmarks(crystalAllSpeedPreset, "mixed", "", 65536, 1, 0, true, false, "seq-low", "psync")
	if len(filtered) != 1 || filtered[0].ProfileRow != "seq-low" || filtered[0].Operation != "mixed" || filtered[0].BlockSize != 65536 {
		t.Fatalf("synchronous single-row mixed test changed: %+v", filtered)
	}
}

func TestCrystalSpeedRoundsExcludeWarmupAndSelectFastestCompleteRun(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{Mock: true})
	job := &Job{ID: "5ec3b03d-0d17-4560-a542-d330137dfce6", Type: "speed", Status: "running", Options: JobOptions{SpeedPreset: crystalSpeedPreset}}
	specs := speedJobBenchmarks(crystalSpeedPreset, "read", "", 0, 0, 0, true, false, "rnd-low", "io_uring")
	transfers := []int64{90000000, 10000000, 20000000, 30000000}
	seconds := []float64{5, 5, 5, 10}
	var calls int
	results, err := m.runSpeedRounds(context.Background(), job, specs, 3, func(spec benchmarkSpec) (BenchmarkResult, error) {
		if job.SpeedLive.Run != int64(calls) || job.SpeedLive.DurationSeconds != 5 || calls >= len(transfers) {
			t.Fatalf("unexpected run state: %+v", job.SpeedLive)
		}
		bytes, duration := transfers[calls], seconds[calls]
		calls++
		return BenchmarkResult{Operation: spec.Operation, Pattern: spec.Pattern, BlockSize: spec.BlockSize, AverageMBps: float64(bytes) / duration / 1e6,
			Samples: []SpeedSample{{Label: "随机", RangeBytes: 1 << 20, Bytes: bytes, Seconds: duration}},
			IOPS:    calculateBenchmarkIOPS(bytes, spec.BlockSize, duration)}, nil
	})
	if err != nil || calls != 4 || len(results) != 1 {
		t.Fatalf("rounds=%d results=%+v err=%v", calls, results, err)
	}
	best := results[0]
	if best.AverageMBps != 4 || best.Samples[0].Bytes != 20000000 || best.Samples[0].Seconds != 5 || best.IOPS == nil || *best.IOPS != 976.6 {
		t.Fatalf("expected fastest measured round, with its sample and IOPS: %+v", best)
	}
}

func TestCrystalSpeedCreateDefaultsToOneGiBAndThreeFiveSecondRounds(t *testing.T) {
	space := testSpace()
	m := newTestManager(t, JobManagerOptions{Mock: true, ResolveSpace: func(string) (*StorageSpace, error) { return space, nil }})
	job, err := m.create("speed", space.Path, map[string]interface{}{"speedPreset": crystalSpeedPreset, "speedOperation": "read", "speedProfileRow": "seq-high"})
	if err != nil {
		t.Fatal(err)
	}
	completed := waitForStatus(t, m, job.ID, "completed", 5*time.Second)
	options := completed.Options
	if *options.SampleMiB != 1024 || *options.SpeedRuns != 3 || *options.SpeedDurationSeconds != 5 || *options.SpeedPosition != "start" {
		t.Fatalf("wrong default options: %+v", options)
	}
	result, ok := completed.Result.(*SpeedResult)
	if !ok || len(result.Benchmarks) != 1 || len(result.Benchmarks[0].Samples) != 1 || result.Benchmarks[0].Samples[0].RangeBytes != 1<<30 || result.Benchmarks[0].Samples[0].Seconds != 5 {
		t.Fatalf("expected a single measured five-second result across the whole file: %+v", completed.Result)
	}
}

func TestSpeedHistorySnapshotsMembersAndDetachesLiveState(t *testing.T) {
	space := &StorageSpace{Members: []string{"/dev/sda", "/dev/sdb"}}
	m := &JobManager{resolveDisk: func(device string) (*Disk, error) {
		if device == "/dev/sdb" {
			return nil, errors.New("unavailable")
		}
		return &Disk{Name: "sda", Model: "Example HDD", Serial: "serial", Size: 1234}, nil
	}}
	members := m.snapshotStorageDisks(space)
	if len(members) != 2 || members[0].Model != "Example HDD" || members[1].Name != "sdb" {
		t.Fatalf("snapshots: %+v", members)
	}
	job := &Job{Type: "speed", Status: "running", StorageSpace: &StorageSpaceSnapshot{MemberDisks: members}, SpeedLive: &SpeedLiveStatus{Stage: "running", Bytes: 123}}
	copy := cloneJobLocked(job)
	job.StorageSpace.MemberDisks[0].Model = "replacement"
	job.SpeedLive.Bytes = 456
	if copy.StorageSpace.MemberDisks[0].Model != "Example HDD" || copy.SpeedLive.Bytes != 123 {
		t.Fatal("API snapshot aliases live state")
	}
	wire, err := json.Marshal(summarizeJobJSON(copy))
	if err != nil || !strings.Contains(string(wire), "Example HDD") || !strings.Contains(string(wire), "speedLive") {
		t.Fatalf("summary lost model/live feedback: %s %v", wire, err)
	}
}

func TestFilesystemSpeedPersistsPhysicalModelAndTimedOptions(t *testing.T) {
	space := testSpace()
	m := newTestManager(t, JobManagerOptions{Mock: true,
		ResolveSpace: func(string) (*StorageSpace, error) { return space, nil },
		ResolveDisk:  func(string) (*Disk, error) { disk := testDisk(); disk.Model = "Recorded HDD model"; return disk, nil },
	})
	job, err := m.create("speed", space.Path, map[string]interface{}{"speedTarget": "filesystem", "sampleMiB": float64(256), "speedOperation": "read", "speedPattern": "random"})
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, m, job.ID, "completed", 5*time.Second)
	stored := m.get(job.ID)
	if stored.Options.SpeedDurationSeconds == nil || *stored.Options.SpeedDurationSeconds != 10 || stored.StorageSpace == nil || len(stored.StorageSpace.MemberDisks) == 0 || stored.StorageSpace.MemberDisks[0].Model != "Recorded HDD model" || stored.SpeedLive != nil {
		t.Fatalf("created job did not retain model/duration or clear live state: %+v", stored)
	}
}

func TestSpeedLiveTimeDoesNotJumpBackWhenFioReportLagsHeartbeat(t *testing.T) {
	job := &Job{Type: "speed", Status: "running"}
	m := &JobManager{}
	m.beginSpeedRun(job, 0, 1, 0, 1)
	m.updateSpeedLive(job, "running", 2.1, 0, nil)
	m.updateSpeedLive(job, "running", 1.9, 1000000, nil)
	if job.SpeedLive.ElapsedSeconds != 2.1 {
		t.Fatal("delayed report rewound elapsed time")
	}
	m.beginSpeedRun(job, 0, 1, 1, 3)
	if job.SpeedLive.ElapsedSeconds != 0 {
		t.Fatal("new repetition did not reset its clock")
	}
}

func TestSpeedLiveHoldsLastRateThroughSameWorkloadStartup(t *testing.T) {
	job := &Job{Type: "speed", Status: "running", Options: JobOptions{SpeedPreset: crystalSpeedPreset}}
	m := &JobManager{}
	m.beginSpeedRun(job, 0, 2, 0, 2)
	m.updateSpeedLive(job, "finishing", 5, 414000000, floatPtr(82.8))
	m.updateSpeedLive(job, "finishing", 5, 414000000, nil)
	if job.SpeedLive.MBps == nil || *job.SpeedLive.MBps != 82.8 {
		t.Fatalf("finishing without a new reading cleared the last rate: %+v", job.SpeedLive)
	}
	m.beginSpeedRun(job, 0, 2, 1, 2)
	m.updateSpeedLive(job, "running", 1, 0, nil)
	if job.SpeedLive.MBps == nil || *job.SpeedLive.MBps != 82.8 || job.SpeedLive.ElapsedSeconds != 1 {
		t.Fatalf("new run should restart its clock but show the last reading: %+v", job.SpeedLive)
	}
	m.updateSpeedLive(job, "running", 2, 0, floatPtr(0))
	if job.SpeedLive.MBps == nil || *job.SpeedLive.MBps != 0 {
		t.Fatalf("a measured zero should replace the previous rate: %+v", job.SpeedLive)
	}
	m.beginSpeedRun(job, 1, 2, -1, 2)
	if job.SpeedLive.MBps != nil {
		t.Fatalf("next workload inherited the previous workload's rate: %+v", job.SpeedLive)
	}
}
