package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path"
	"time"
)

const (
	speedDurationSeconds        int64 = 10
	crystalSpeedDurationSeconds int64 = 5
	crystalSpeedDefaultRuns     int64 = 3
	crystalSpeedInterval              = 5 * time.Second
	crystalSpeedPreset                = "crystal-default"
	crystalAllSpeedPreset             = "crystal-all"
)

func isCrystalSpeedPreset(preset string) bool {
	return preset == crystalSpeedPreset || preset == crystalAllSpeedPreset
}

func speedRunSeconds(job *Job) int64 {
	if isCrystalSpeedPreset(job.Options.SpeedPreset) {
		return crystalSpeedDurationSeconds
	}
	return speedDurationSeconds
}

func speedJobBenchmarks(preset, operation, pattern string, block, depth, threads int64, writeAllowed, profile bool, profileRow, ioEngine string) []benchmarkSpec {
	if !isCrystalSpeedPreset(preset) {
		return selectSpeedBenchmarks(operation, pattern, block, depth, threads, writeAllowed, profile, profileRow)
	}
	rows := []benchmarkSpec{
		{Pattern: "sequential", BlockSize: sequentialBenchmarkSize, QueueDepth: 8, Threads: 1, ProfileRow: "seq-high"},
		{Pattern: "sequential", BlockSize: sequentialBenchmarkSize, QueueDepth: 1, Threads: 1, ProfileRow: "seq-low"},
		{Pattern: "random", BlockSize: randomBenchmarkSize, QueueDepth: 32, Threads: 1, ProfileRow: "rnd-high"},
		{Pattern: "random", BlockSize: randomBenchmarkSize, QueueDepth: 1, Threads: 1, ProfileRow: "rnd-low"},
	}
	operations := []string{"read", "write"}
	if preset == crystalAllSpeedPreset {
		operations = append(operations, "mixed")
		for i := range rows {
			row := &rows[i]
			if row.Pattern == "sequential" && block > 0 {
				row.BlockSize = block
			}
			if row.ProfileRow == "seq-high" || row.ProfileRow == "rnd-high" {
				if depth > 0 {
					row.QueueDepth = depth
				}
				if threads > 0 {
					row.Threads = threads
				}
			}
		}
	}
	if operation != "" {
		operations = []string{operation}
	}
	var specs []benchmarkSpec
	for _, op := range operations {
		if !writeAllowed && op != "read" {
			continue
		}
		for _, row := range rows {
			if (profileRow != "" && row.ProfileRow != profileRow) || (pattern != "" && row.Pattern != pattern) || (ioEngine == "psync" && (row.ProfileRow == "seq-high" || row.ProfileRow == "rnd-high")) {
				continue
			}
			row.Operation = op
			specs = append(specs, row)
		}
	}
	return specs
}

func speedFilePattern(job *Job, buffer []byte) error {
	if isCrystalSpeedPreset(job.Options.SpeedPreset) {
		_, err := rand.Read(buffer)
		return err
	}
	fillPattern(buffer, writePatternA)
	return nil
}

func selectFastestSpeedRun(results []BenchmarkResult) BenchmarkResult {
	throughput := func(result BenchmarkResult) float64 {
		var bytes int64
		var seconds float64
		for _, sample := range result.Samples {
			bytes += sample.Bytes
			seconds += sample.Seconds
		}
		return float64(bytes) / seconds
	}
	best := results[0]
	for _, result := range results[1:] {
		if throughput(result) > throughput(best) {
			best = result
		}
	}
	return best
}

func (m *JobManager) runSpeedRounds(ctx context.Context, job *Job, specs []benchmarkSpec, runs int64, measure func(benchmarkSpec) (BenchmarkResult, error)) ([]BenchmarkResult, error) {
	crystal := isCrystalSpeedPreset(job.Options.SpeedPreset)
	firstRun := int64(0)
	if crystal {
		firstRun = -1 // The first complete round warms up this workload.
	}
	var benchmarks []BenchmarkResult
	for i, spec := range specs {
		if crystal && i > 0 {
			if err := m.waitSpeedInterval(ctx, job); err != nil {
				return nil, err
			}
		}
		results := make([]BenchmarkResult, 0, runs)
		for run := firstRun; run < runs; run++ {
			if ctx.Err() != nil || m.checkCancelled(job) != nil {
				return nil, errCancelled
			}
			m.beginSpeedRun(job, i, len(specs), run, runs)
			result, err := measure(spec)
			if err != nil {
				return nil, err
			}
			if run >= 0 {
				results = append(results, result)
			}
			m.mu.Lock()
			m.updateSpeedProgressLocked(job, 1)
			m.mu.Unlock()
		}
		var benchmark BenchmarkResult
		if crystal {
			benchmark = selectFastestSpeedRun(results)
		} else {
			benchmark = combineBenchmarkRuns(spec, results)
		}
		benchmarks = append(benchmarks, benchmark)
		m.mu.Lock()
		job.Result = &speedInProgress{Benchmarks: benchmarks}
		m.mu.Unlock()
		m.persistSafe(job, "保存任务状态")
	}
	return benchmarks, nil
}

func (m *JobManager) waitSpeedInterval(ctx context.Context, job *Job) error {
	duration := crystalSpeedInterval
	if m.mock {
		duration /= 10
	}
	m.mu.Lock()
	job.Phase = "测试间隔"
	job.SpeedLive = &SpeedLiveStatus{Stage: "interval", DurationSeconds: int64(crystalSpeedInterval / time.Second)}
	m.mu.Unlock()
	for second := int64(1); second <= int64(crystalSpeedInterval/time.Second); second++ {
		timer := time.NewTimer(duration / (crystalSpeedInterval / time.Second))
		select {
		case <-ctx.Done():
			timer.Stop()
			return errCancelled
		case <-timer.C:
			m.mu.Lock()
			job.SpeedLive.ElapsedSeconds = float64(second)
			m.mu.Unlock()
		}
	}
	return nil
}

// SpeedLiveStatus is transient feedback; it never replaces measured results.
type SpeedLiveStatus struct {
	Stage           string   `json:"stage"`
	BenchmarkIndex  int      `json:"benchmarkIndex"`
	BenchmarkCount  int      `json:"benchmarkCount"`
	Run             int64    `json:"run"`
	Runs            int64    `json:"runs"`
	ElapsedSeconds  float64  `json:"elapsedSeconds"`
	DurationSeconds int64    `json:"durationSeconds"`
	Bytes           int64    `json:"bytes"`
	TotalBytes      int64    `json:"totalBytes,omitempty"`
	MBps            *float64 `json:"mbps,omitempty"`
}

func (m *JobManager) snapshotStorageDisks(space *StorageSpace) []DiskSnapshot {
	snapshots := make([]DiskSnapshot, 0, len(space.Members))
	for _, member := range space.Members {
		snapshot := DiskSnapshot{Name: path.Base(member)}
		if m.resolveDisk != nil {
			if disk, err := m.resolveDisk(member); err == nil && disk != nil {
				snapshot = DiskSnapshot{Name: disk.Name, Model: disk.Model, Serial: disk.Serial,
					Size: disk.Size, LogicalSectorSize: disk.LogicalSectorSize,
					PhysicalSectorSize: disk.PhysicalSectorSize, Rotational: disk.Rotational, IsSystem: disk.IsSystem}
			}
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots
}

func speedSampleRangeBytes(sample SpeedSample) int64 {
	if sample.RangeBytes > 0 {
		return sample.RangeBytes
	}
	return sample.Bytes
}

func speedSegmentSeconds(index, count int) int64 {
	return splitSpeedSeconds(speedDurationSeconds, index, count)
}

func splitSpeedSeconds(total int64, index, count int) int64 {
	seconds := total / int64(count)
	if int64(index) < total%int64(count) {
		seconds++
	}
	return seconds
}

func speedOverallProgress(job *Job, fraction float64) float64 {
	preparation := 0.0
	if job.StorageSpace != nil {
		preparation = 5
	}
	return math.Min(99.9, preparation+(100-preparation)*fraction)
}

func (m *JobManager) beginSpeedRun(job *Job, index, count int, run, runs int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var mbps *float64
	if live := job.SpeedLive; live != nil && live.BenchmarkIndex == index && (live.Stage == "running" || live.Stage == "finishing") {
		mbps = live.MBps
	}
	job.SpeedLive = &SpeedLiveStatus{Stage: "running", BenchmarkIndex: index, BenchmarkCount: count,
		Run: run + 1, Runs: runs, DurationSeconds: speedRunSeconds(job), MBps: mbps}
}

func (m *JobManager) updateSpeedLive(job *Job, stage string, elapsed float64, transferred int64, mbps *float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if job.SpeedLive == nil || job.Status != "running" {
		return
	}
	live := job.SpeedLive
	live.Stage, live.Bytes = stage, transferred
	if mbps != nil {
		live.MBps = mbps
	}
	live.ElapsedSeconds = math.Min(float64(live.DurationSeconds), math.Max(live.ElapsedSeconds, math.Max(0, elapsed)))
	m.updateSpeedProgressLocked(job, live.ElapsedSeconds/float64(live.DurationSeconds))
}

func (m *JobManager) updateSpeedProgressLocked(job *Job, runFraction float64) {
	live := job.SpeedLive
	if live.BenchmarkCount <= 0 || live.Runs <= 0 {
		return
	}
	runs, index := live.Runs, live.Run-1
	if isCrystalSpeedPreset(job.Options.SpeedPreset) {
		runs++
		index++
	}
	fraction := (float64(int64(live.BenchmarkIndex)*runs+index) + runFraction) / float64(int64(live.BenchmarkCount)*runs)
	job.Progress = math.Max(job.Progress, speedOverallProgress(job, fraction))
}

type fioSnapshot struct {
	raw json.RawMessage
	err error
}

// fio status-interval emits consecutive, pretty-printed JSON documents, not
// a JSON array or newline-delimited JSON. Decode one document at a time.
func decodeFioStream(reader io.Reader, emit func(json.RawMessage) error) error {
	decoder := json.NewDecoder(reader)
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("fio 实时结果无法解析：%w", err)
		}
		if err := emit(raw); err != nil {
			return err
		}
	}
}

func fioCounters(raw []byte) (int64, float64, error) {
	var output fioOutput
	if err := json.Unmarshal(raw, &output); err != nil || len(output.Jobs) == 0 {
		return 0, 0, errors.New("fio 未返回任务统计")
	}
	var transferred int64
	var milliseconds float64
	for _, job := range output.Jobs {
		if job.Error != 0 {
			return 0, 0, fmt.Errorf("fio I/O 失败（错误码 %d）", job.Error)
		}
		transferred += job.Read.IOBytes + job.Write.IOBytes
		milliseconds = math.Max(milliseconds, math.Max(job.Read.Runtime, math.Max(job.Write.Runtime, job.JobRuntime)))
	}
	return transferred, milliseconds / 1000, nil
}

func (m *JobManager) streamFioSample(ctx context.Context, executable string, args []string, job *Job, label string, offset, duration, elapsedBefore int64) (SpeedSample, error) {
	command := exec.CommandContext(ctx, executable, args...)
	command.Env = append(os.Environ(), "LC_ALL=C")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	pipe, err := command.StdoutPipe()
	if err != nil {
		return SpeedSample{}, err
	}
	if err = command.Start(); err != nil {
		return SpeedSample{}, err
	}
	snapshots := make(chan fioSnapshot, 2)
	go func() {
		defer close(snapshots)
		err := decodeFioStream(pipe, func(raw json.RawMessage) error {
			select {
			case snapshots <- fioSnapshot{raw: raw}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		if err != nil {
			select {
			case snapshots <- fioSnapshot{err: err}:
			case <-ctx.Done():
			}
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	started := time.Now()
	var last json.RawMessage
	var previousBytes, currentBytes int64
	var previousSeconds float64
	var rate *float64
	var streamErr error
	m.updateSpeedLive(job, "running", float64(elapsedBefore), 0, nil)
	for snapshots != nil {
		select {
		case snapshot, ok := <-snapshots:
			if !ok {
				snapshots = nil
				break
			}
			if snapshot.err != nil {
				streamErr = snapshot.err
				_ = command.Process.Kill()
				continue
			}
			last = snapshot.raw
			transferred, seconds, parseErr := fioCounters(last)
			if parseErr != nil {
				streamErr = parseErr
				_ = command.Process.Kill()
				continue
			}
			currentBytes = transferred
			if seconds > previousSeconds && transferred >= previousBytes {
				value := round1(float64(transferred-previousBytes) / (seconds - previousSeconds) / 1e6)
				rate = &value
				previousSeconds, previousBytes = seconds, transferred
			}
			stage := "running"
			if seconds >= float64(duration) {
				stage = "finishing"
			}
			m.updateSpeedLive(job, stage, float64(elapsedBefore)+math.Min(seconds, float64(duration)), transferred, rate)
		case <-ticker.C:
			elapsed := time.Since(started).Seconds()
			stage := "running"
			if elapsed >= float64(duration)+1 {
				stage = "finishing"
			}
			m.updateSpeedLive(job, stage, float64(elapsedBefore)+math.Min(elapsed, float64(duration)), currentBytes, rate)
		}
	}
	waitErr := command.Wait()
	if ctx.Err() != nil || m.checkCancelled(job) != nil {
		return SpeedSample{}, errCancelled
	}
	if streamErr != nil {
		return SpeedSample{}, streamErr
	}
	if waitErr != nil {
		return SpeedSample{}, conciseCommandError("fio 测速失败", waitErr, stderr.String())
	}
	return parseFioSample(last, label, offset)
}

// The demo advances virtual seconds without touching a real device.
func (m *JobManager) mockSpeedFeedback(job *Job, rate float64, duration, before int64) error {
	for second := int64(1); second <= duration; second++ {
		m.delayFn(100 * time.Millisecond)
		if err := m.checkCancelled(job); err != nil {
			return err
		}
		m.updateSpeedLive(job, "running", float64(before+second), int64(rate*1e6*float64(second)), &rate)
	}
	return nil
}

// Keep elapsed time observable even while a slow WriteAt or final Sync waits.
func (m *JobManager) trackSpeedPreparationTime(job *Job, started time.Time) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				m.mu.Lock()
				if job.SpeedLive != nil && job.Status == "running" {
					job.SpeedLive.ElapsedSeconds = time.Since(started).Seconds()
				}
				m.mu.Unlock()
			}
		}
	}()
	return func() { close(stop); <-done }
}
