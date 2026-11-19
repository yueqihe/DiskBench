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
	mathrand "math/rand"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	speedBlockSize              = 4 * 1024 * 1024
	sequentialBenchmarkSize     = 1024 * 1024 // 1 MiB
	randomBenchmarkSize         = 4096        // 4 KiB
	scanCellCount               = 4096
	quickScanCellCount          = 4096
	fastSampleSize              = 1024 * 1024
	fullScanReadSize            = 8 * 1024 * 1024
	fullReadSize                = 4 * 1024 * 1024
	errorProbeSize              = 64 * 1024
	maxShortReadRetries         = 3
	sectorAlignment             = 4096
	progressPersistInterval     = 60 * time.Second
	scanProgressPersistInterval = 5 * time.Second
	maxJobHistory               = 30
)

var (
	speedSizes       = map[int64]bool{256: true, 1024: true, 4096: true}
	speedRuns        = map[int64]bool{1: true, 3: true, 5: true}
	speedPositions   = map[string]bool{"all": true, "start": true, "middle": true, "end": true}
	speedOperations  = map[string]bool{"read": true, "write": true, "mixed": true}
	speedPatterns    = map[string]bool{"sequential": true, "random": true}
	speedBlockSizes  = map[int64]bool{4096: true, 65536: true, 1048576: true, 4194304: true}
	speedQueueDepths = map[int64]bool{1: true, 8: true, 16: true, 32: true, 64: true, 128: true}
	speedThreads     = map[int64]bool{1: true, 2: true, 4: true, 8: true}
	speedIOEngines   = map[string]bool{"io_uring": true, "psync": true}
	speedProfileRows = map[string]bool{"seq-high": true, "seq-low": true, "rnd-high": true, "rnd-low": true}
	speedTargetModes = map[string]bool{"filesystem": true, "raw": true}
	scanModes        = map[string]bool{"quick": true, "full": true}

	speedTargets = []speedTarget{
		{ID: "start", Label: "头部"},
		{ID: "middle", Label: "中部"},
		{ID: "end", Label: "尾部"},
	}

	activeJobStatuses   = map[string]bool{"queued": true, "running": true, "cancelling": true}
	terminalJobStatuses = map[string]bool{"completed": true, "failed": true, "cancelled": true, "interrupted": true}

	recoverableMediaErrors = map[string]bool{"EIO": true, "EREMOTEIO": true}

	jobIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

	ddResultRe   = regexp.MustCompile(`([0-9]+) bytes[^\n]*?copied,\s*([0-9.]+) s,\s*([0-9.]+)\s*([kMGT]?B/s)`)
	ddProgressRe = regexp.MustCompile(`(?:^|\n)([0-9]+) bytes`)
)

var errCancelled = errors.New("任务已由用户取消")

// jobError carries HTTP metadata alongside a plain error message.
type jobError struct {
	message    string
	apiCode    string
	statusCode int
}

func (e *jobError) Error() string { return e.message }

func newJobError(message, apiCode string, statusCode int) *jobError {
	return &jobError{message: message, apiCode: apiCode, statusCode: statusCode}
}

type speedTarget struct {
	ID    string
	Label string
}

type benchmarkSpec struct {
	Operation  string
	Pattern    string
	BlockSize  int64
	QueueDepth int64
	Threads    int64
	ProfileRow string
}

// standardBenchmarks mirrors the visible CrystalDiskMark-style matrix.
var standardBenchmarks = func() []benchmarkSpec {
	rows := []benchmarkSpec{
		{Pattern: "sequential", BlockSize: 1048576, QueueDepth: 8, Threads: 1},
		{Pattern: "sequential", BlockSize: 1048576, QueueDepth: 1, Threads: 1},
		{Pattern: "random", BlockSize: 4096, QueueDepth: 32, Threads: 1},
		{Pattern: "random", BlockSize: 4096, QueueDepth: 1, Threads: 1},
	}
	operations := []string{"read", "write", "mixed"}
	benchmarks := make([]benchmarkSpec, 0, len(rows)*len(operations))
	for _, row := range rows {
		for _, operation := range operations {
			spec := row
			spec.Operation = operation
			benchmarks = append(benchmarks, spec)
		}
	}
	return benchmarks
}()

type SpeedPlan struct {
	SegmentBlocks int64
	PlannedBlocks int64
	Targets       []plannedSpeedTarget
}

type plannedSpeedTarget struct {
	ID           string
	Label        string
	BlockCount   int64
	OffsetBlocks int64
}

type DiskSnapshot struct {
	Name               string `json:"name"`
	Model              string `json:"model"`
	Serial             string `json:"serial"`
	Size               int64  `json:"size"`
	LogicalSectorSize  int64  `json:"logicalSectorSize"`
	PhysicalSectorSize int64  `json:"physicalSectorSize"`
	Rotational         bool   `json:"rotational"`
	IsSystem           bool   `json:"isSystem"`
}

type StorageSpaceSnapshot struct {
	Path       string   `json:"path"`
	Source     string   `json:"source"`
	FileSystem string   `json:"fileSystem"`
	Layout     string   `json:"layout"`
	Members    []string `json:"members"`
	Size       int64    `json:"size"`
	ReadOnly   bool     `json:"readOnly"`
	IsSystem   bool     `json:"isSystem,omitempty"`
}

type JobOptions struct {
	ScanRepairFrom    string  `json:"scanRepairFrom,omitempty"`
	ScanDeepLocate    *bool   `json:"scanDeepLocate,omitempty"`
	ScanMode          *string `json:"scanMode,omitempty"`
	ScanExactSectors  *bool   `json:"scanExactSectors,omitempty"`
	ScanSelfTest      *bool   `json:"scanSelfTest,omitempty"`
	ScanRepair        *bool   `json:"scanRepair,omitempty"`
	ScanRepairAfterMs *int64  `json:"scanRepairAfterMs,omitempty"`
	SampleMiB         *int64  `json:"sampleMiB,omitempty"`
	SpeedRuns         *int64  `json:"speedRuns,omitempty"` // 1 | 3 | 5
	SpeedPosition     *string `json:"speedPosition,omitempty"`
	SpeedOperation    *string `json:"speedOperation,omitempty"`  // "read" | "write"
	SpeedPattern      *string `json:"speedPattern,omitempty"`    // "sequential" | "random"
	SpeedBlockSize    *int64  `json:"speedBlockSize,omitempty"`  // 4096 | 65536 | 1048576 | 4194304
	SpeedQueueDepth   *int64  `json:"speedQueueDepth,omitempty"` // 1 | 8 | 16 | 32 | 64 | 128
	SpeedThreads      *int64  `json:"speedThreads,omitempty"`
	SpeedIOEngine     *string `json:"speedIOEngine,omitempty"`
	SpeedDirectIO     *bool   `json:"speedDirectIO,omitempty"`
	SpeedTrim         *bool   `json:"speedTrim,omitempty"`
	SpeedProfile      *bool   `json:"speedProfile,omitempty"`
	SpeedProfileRow   *string `json:"speedProfileRow,omitempty"`
	SpeedTarget       *string `json:"speedTarget,omitempty"`
}

type BadRange struct {
	SkippedRange    *ScanSkippedRange `json:"skippedRange,omitempty"` // Display only; journal events remain separate.
	Detail          string            `json:"detail,omitempty"`
	UnresolvedBytes int64             `json:"unresolvedBytes,omitempty"`
	Repair          *ScanRepairRecord `json:"repair,omitempty"`
	Outcome         string            `json:"outcome,omitempty"`
	ProbeBytes      int64             `json:"probeBytes,omitempty"`
	Offset          int64             `json:"offset"`
	Length          int64             `json:"length"`
	Code            string            `json:"code"`
	Recovered       *bool             `json:"recovered,omitempty"`
	Evidence        *ScanIOEvidence   `json:"evidence,omitempty"`
}

type ScanSkippedRange struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}

type ScanCell struct {
	SlowBadBlock      *ScanSlowBlock    `json:"slowBadBlock,omitempty"`
	SkippedBytes      int64             `json:"skippedBytes,omitempty"`
	SampleRanges      []ScanSampleRange `json:"sampleRanges,omitempty"`
	UnresolvedBytes   int64             `json:"unresolvedBytes,omitempty"`
	Load              *ScanLoad         `json:"load,omitempty"`
	Repair            *ScanRepairStats  `json:"repair,omitempty"`
	CoveredBytes      int64             `json:"coveredBytes,omitempty"`
	UnreadableBytes   int64             `json:"unreadableBytes,omitempty"`
	RecoveredRanges   int64             `json:"recoveredRanges,omitempty"`
	DeviceErrorRanges int64             `json:"deviceErrorRanges,omitempty"`
	ReadOutcome       string            `json:"readOutcome,omitempty"`
	MaxWriteLatency   *ScanReadLatency  `json:"maxWriteLatency,omitempty"`
	MaxReadLatency    *ScanReadLatency  `json:"maxReadLatency,omitempty"`
	LatencyWarning    string            `json:"latencyWarning,omitempty"`
	MaxLatencyRatio   float64           `json:"maxLatencyRatio,omitempty"`
	Recheck           *ScanRecheck      `json:"recheck,omitempty"`
	Index             int               `json:"index"`
	Start             int64             `json:"start"`
	End               int64             `json:"end"`
	Status            string            `json:"status"`
	MaxMs             *float64          `json:"maxMs"`
	MaxUnitMs         *float64          `json:"maxUnitMs,omitempty"`
	AvgMs             *float64          `json:"avgMs"`
	Reads             int64             `json:"reads"`
	BytesRead         int64             `json:"bytesRead"`
	ErrorCount        int64             `json:"errorCount"`
	SampleStart       *int64            `json:"sampleStart"`
	SampleEnd         *int64            `json:"sampleEnd"`
	TotalMs           *float64          `json:"totalMs,omitempty"`
}

type ScanStats struct {
	TimeoutSkipPairs  int64 `json:"timeoutSkipPairs,omitempty"`
	lastIssue         *BadRange
	SkippedBytes      int64           `json:"skippedBytes,omitempty"`
	UnresolvedBytes   int64           `json:"unresolvedBytes,omitempty"`
	Repair            ScanRepairStats `json:"repair"`
	CoveredBytes      int64           `json:"coveredBytes,omitempty"`
	IssueCount        int64           `json:"issueCount,omitempty"`
	SavedIssueCount   int64           `json:"savedIssueCount,omitempty"`
	UnreadableRanges  int64           `json:"unreadableRanges,omitempty"`
	UnreadableBytes   int64           `json:"unreadableBytes,omitempty"`
	RecoveredRanges   int64           `json:"recoveredRanges,omitempty"`
	DeviceErrorRanges int64           `json:"deviceErrorRanges,omitempty"`
	UnresolvedRanges  int64           `json:"unresolvedRanges,omitempty"`
	IssuesComplete    bool            `json:"issuesComplete"`
	IssuesTruncated   bool            `json:"issuesTruncated,omitempty"`
	IssueWarning      string          `json:"issueWarning,omitempty"`
	TestedCells       int64           `json:"testedCells"`
	ErrorCells        int64           `json:"errorCells"`
	SlowCells         int64           `json:"slowCells"`
	BytesRead         int64           `json:"bytesRead"`
	BadRanges         []BadRange      `json:"badRanges"`
}

type ScanResult struct {
	RepairTargets []RecordedRepairTarget `json:"repairTargets,omitempty"`
	SamplingPlan  string                 `json:"samplingPlan,omitempty"`
	SamplingSeed  uint32                 `json:"samplingSeed,omitempty"`
	Version       int                    `json:"version,omitempty"`
	Diagnostics   *ScanDiagnostics       `json:"diagnostics,omitempty"`
	LatencyBasis  string                 `json:"latencyBasis,omitempty"`
	ScanMode      string                 `json:"scanMode"`
	Coverage      string                 `json:"coverage"`
	SampleBytes   *int64                 `json:"sampleBytes"`
	Map           []ScanCell             `json:"map"`
	Stats         ScanStats              `json:"stats"`
}

type ScanProgressSnapshot struct {
	Diagnostics        *ScanDiagnostics `json:"diagnostics,omitempty"`
	ID                 string           `json:"id"`
	Status             string           `json:"status"`
	Progress           float64          `json:"progress"`
	Phase              string           `json:"phase"`
	Message            string           `json:"message"`
	FinishedAt         *string          `json:"finishedAt"`
	PersistenceWarning string           `json:"persistenceWarning,omitempty"`
	Cells              []ScanCell       `json:"cells"`
	Next               int              `json:"next"`
	Stats              ScanStats        `json:"stats"`
}

type SpeedSample struct {
	Label       string  `json:"label"`
	OffsetBytes int64   `json:"offsetBytes"`
	Bytes       int64   `json:"bytes"`
	Seconds     float64 `json:"seconds"`
	MBps        float64 `json:"mbps"`
}

// speedInProgress is the intermediate result shape during a speed run: the
// completed benchmarks so far.
type speedInProgress struct {
	Benchmarks []BenchmarkResult `json:"benchmarks"`
}

// BenchmarkResult is a single measured combination (operation × pattern ×
// block size).
type BenchmarkResult struct {
	Operation   string        `json:"operation"`
	Pattern     string        `json:"pattern"`
	BlockSize   int64         `json:"blockSize"`
	QueueDepth  int64         `json:"queueDepth"`
	Threads     int64         `json:"threads"`
	AverageMBps float64       `json:"averageMBps"`
	MinimumMBps float64       `json:"minimumMBps"`
	MaximumMBps float64       `json:"maximumMBps"`
	IOPS        *float64      `json:"iops,omitempty"`
	Samples     []SpeedSample `json:"samples"`
}

type SpeedResult struct {
	SampleMiB  int64             `json:"sampleMiB"`
	Runs       int64             `json:"runs"`
	Benchmarks []BenchmarkResult `json:"benchmarks"`
}

type speedHistoryBenchmark struct {
	Operation   string   `json:"operation"`
	Pattern     string   `json:"pattern"`
	BlockSize   int64    `json:"blockSize"`
	QueueDepth  int64    `json:"queueDepth,omitempty"`
	Threads     int64    `json:"threads,omitempty"`
	AverageMBps *float64 `json:"averageMBps"`
}

type speedHistorySummary struct {
	Benchmarks []speedHistoryBenchmark `json:"benchmarks"`
}

type Job struct {
	ResumeFrom          string                `json:"resumeFrom,omitempty"`
	ResumeStartProgress float64               `json:"resumeStartProgress,omitempty"`
	ID                  string                `json:"id"`
	Type                string                `json:"type"`
	Device              string                `json:"device"`
	Disk                DiskSnapshot          `json:"disk"`
	StorageSpace        *StorageSpaceSnapshot `json:"storageSpace,omitempty"`
	Status              string                `json:"status"`
	HistoryOrder        int                   `json:"historyOrder"`
	Progress            float64               `json:"progress"`
	Phase               string                `json:"phase"`
	CreatedAt           string                `json:"createdAt"`
	StartedAt           *string               `json:"startedAt"`
	FinishedAt          *string               `json:"finishedAt"`
	Message             string                `json:"message"`
	Result              interface{}           `json:"result"`
	Log                 []string              `json:"log"`
	Options             JobOptions            `json:"options"`
	PersistenceWarning  string                `json:"persistenceWarning,omitempty"`
}

// ReadHandle abstracts a read-only file handle so tests can inject fakes.
type ReadHandle interface {
	ReadAt(p []byte, off int64) (int, error)
	Close() error
}

// ddProcess abstracts a spawned dd process so tests can inject fakes.
type ddProcess interface {
	Stderr() io.ReadCloser
	Wait() (int, error)
	Kill() error
}

type ddProcessImpl struct {
	cmd    *exec.Cmd
	stderr io.ReadCloser
}

func (p *ddProcessImpl) Stderr() io.ReadCloser { return p.stderr }
func (p *ddProcessImpl) Kill() error           { return p.cmd.Process.Kill() }
func (p *ddProcessImpl) Wait() (int, error) {
	err := p.cmd.Wait()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return -1, err
}

type readOpener func(path string) (ReadHandle, error)
type ddSpawner func(exe string, args []string) (ddProcess, error)

type logger interface {
	Errorf(format string, v ...interface{})
}

type defaultLogger struct{}

func (defaultLogger) Errorf(format string, v ...interface{}) {
	fmt.Fprintf(os.Stderr, "[diskbench] "+format+"\n", v...)
}

func openReadOnly(path string) (ReadHandle, error) {
	return openScanDevice(path)
}

func spawnDD(exe string, args []string) (ddProcess, error) {
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &ddProcessImpl{cmd: cmd, stderr: stderr}, nil
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func floatPtr(v float64) *float64 { return &v }
func intPtr(v int64) *int64       { return &v }
func boolPtr(b bool) *bool        { return &b }

func alignDown(value, alignment int64) int64 {
	return value / alignment * alignment
}

func isoTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Compare transfers on a 1 MiB basis, with a 1 MiB floor so sector-sized
// tail reads are not penalized for fixed seek/command overhead.
func scanUnitLatency(milliseconds float64, transferBytes int64) float64 {
	units := math.Max(1, float64(transferBytes)/(1024*1024))
	return milliseconds / units
}

func classifyLatency(milliseconds float64, errorCount int64) string {
	if errorCount > 0 {
		return "error"
	}
	if milliseconds < 50 {
		return "normal"
	}
	if milliseconds < scanLegacyTransferSevereMS {
		return "slow"
	}
	return "severe"
}

func createScanMap(diskSize int64, count int, logicalSectorSize int64) []ScanCell {
	if count <= 0 {
		count = scanCellCount
	}
	alignment := int64(1)
	if logicalSectorSize > 0 && diskSize%logicalSectorSize == 0 {
		alignment = logicalSectorSize
	}
	alignedUnits := diskSize / alignment
	boundary := func(index int64) int64 {
		return (alignedUnits * index / int64(count)) * alignment
	}
	cells := make([]ScanCell, count)
	for i := range cells {
		cells[i] = ScanCell{
			Index:  i,
			Start:  boundary(int64(i)),
			End:    boundary(int64(i + 1)),
			Status: "pending",
		}
	}
	return cells
}

func diskIdentityChanged(expectedPath, expectedSerial, expectedModel string, expectedSize int64, actual *Disk) bool {
	if actual == nil {
		return true
	}
	expectedPath = strings.TrimSpace(expectedPath)
	expectedSerial = strings.TrimSpace(expectedSerial)
	actualSerial := strings.TrimSpace(actual.Serial)
	expectedModel = strings.TrimSpace(expectedModel)
	actualModel := strings.TrimSpace(actual.Model)
	if expectedPath != "" && expectedPath != actual.Path {
		return true
	}
	if expectedSerial != "" && expectedSerial != actualSerial {
		return true
	}
	if expectedModel != "" && expectedModel != actualModel {
		return true
	}
	if expectedSize > 0 && expectedSize != actual.Size {
		return true
	}
	return false
}

func appendBadRanges(target *[]BadRange, ranges []BadRange, limit int) {
	if limit < 0 {
		*target = append(*target, ranges...)
		return
	}
	if limit <= 0 {
		return
	}
	remaining := limit - len(*target)
	if remaining <= 0 {
		return
	}
	if remaining > len(ranges) {
		remaining = len(ranges)
	}
	*target = append(*target, ranges[:remaining]...)
}

func parseDdResult(text string) *SpeedSample {
	normalized := strings.ReplaceAll(text, "\r", "\n")
	matches := ddResultRe.FindAllStringSubmatch(normalized, -1)
	if len(matches) == 0 {
		return nil
	}
	match := matches[len(matches)-1]
	bytes, err1 := strconv.ParseInt(match[1], 10, 64)
	seconds, err2 := strconv.ParseFloat(match[2], 64)
	rate, err3 := strconv.ParseFloat(match[3], 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return nil
	}
	factors := map[string]float64{"B/s": 1, "kB/s": 1e3, "MB/s": 1e6, "GB/s": 1e9, "TB/s": 1e12}
	factor, ok := factors[match[4]]
	if !ok {
		return nil
	}
	return &SpeedSample{Bytes: bytes, Seconds: seconds, MBps: round1(rate * factor / 1e6)}
}

func parseDdProgress(text string) int64 {
	normalized := strings.ReplaceAll(text, "\r", "\n")
	matches := ddProgressRe.FindAllStringSubmatch(normalized, -1)
	if len(matches) == 0 {
		return 0
	}
	value, err := strconv.ParseInt(matches[len(matches)-1][1], 10, 64)
	if err != nil {
		return 0
	}
	return value
}

func createSpeedPlan(diskSize int64, sampleMiB int64, speedPosition string) (SpeedPlan, error) {
	if !speedPositions[speedPosition] {
		speedPosition = "all"
	}
	totalBlocks := diskSize / speedBlockSize
	if totalBlocks < 0 {
		totalBlocks = 0
	}
	requestedBlocks := (sampleMiB * 1024 * 1024) / speedBlockSize
	if requestedBlocks < 1 {
		requestedBlocks = 1
	}

	var targets []speedTarget
	if speedPosition == "all" {
		targets = speedTargets
	} else {
		for _, t := range speedTargets {
			if t.ID == speedPosition {
				targets = []speedTarget{t}
				break
			}
		}
	}
	if totalBlocks < int64(len(targets)) {
		return SpeedPlan{}, newJobError("磁盘容量不足以执行所选测速", "DISK_TOO_SMALL", 400)
	}

	plannedBlocks := requestedBlocks
	if speedPosition == "all" {
		if plannedBlocks > totalBlocks {
			plannedBlocks = totalBlocks
		}
	} else {
		if plannedBlocks > totalBlocks {
			plannedBlocks = totalBlocks
		}
		if plannedBlocks < 1 {
			plannedBlocks = 1
		}
	}
	baseBlocks := plannedBlocks / int64(len(targets))
	remainder := plannedBlocks % int64(len(targets))
	blockCounts := make([]int64, len(targets))
	for i := range targets {
		count := baseBlocks
		if int64(i) < remainder {
			count++
		}
		if count < 1 {
			count = 1
		}
		blockCounts[i] = count
	}
	firstEnd := blockCounts[0]
	lastStart := totalBlocks - blockCounts[len(blockCounts)-1]
	offsetFor := func(target speedTarget, blockCount int64) int64 {
		switch target.ID {
		case "middle":
			centered := totalBlocks/2 - blockCount/2
			if centered < 0 {
				centered = 0
			}
			if speedPosition == "all" {
				v := firstEnd
				if centered > v {
					v = centered
				}
				limit := lastStart - blockCount
				if v > limit {
					v = limit
				}
				return v
			}
			return centered
		case "end":
			v := totalBlocks - blockCount
			if v < 0 {
				v = 0
			}
			return v
		default:
			return 0
		}
	}
	planned := make([]plannedSpeedTarget, len(targets))
	var sum int64
	for i, t := range targets {
		planned[i] = plannedSpeedTarget{ID: t.ID, Label: t.Label, BlockCount: blockCounts[i], OffsetBlocks: offsetFor(t, blockCounts[i])}
		sum += blockCounts[i]
	}
	return SpeedPlan{SegmentBlocks: planned[0].BlockCount, PlannedBlocks: sum, Targets: planned}, nil
}

func summarizeJobJSON(job *Job) map[string]interface{} {
	summary := map[string]interface{}{
		"id":                  job.ID,
		"type":                job.Type,
		"device":              job.Device,
		"disk":                job.Disk,
		"storageSpace":        job.StorageSpace,
		"status":              job.Status,
		"historyOrder":        job.HistoryOrder,
		"progress":            job.Progress,
		"phase":               job.Phase,
		"createdAt":           job.CreatedAt,
		"startedAt":           job.StartedAt,
		"finishedAt":          job.FinishedAt,
		"message":             job.Message,
		"options":             job.Options,
		"resume":              scanResumeInfo(job),
		"resumeFrom":          job.ResumeFrom,
		"resumeStartProgress": job.ResumeStartProgress,
	}
	if job.Type == "speed" && job.Result != nil {
		// Reloaded history stores Result as a JSON map; live jobs use typed
		// results. Decode only the measured rates and test parameters, keeping
		// sample arrays out of the history-list response in either case.
		encoded, err := json.Marshal(job.Result)
		var speed speedHistorySummary
		if err == nil && json.Unmarshal(encoded, &speed) == nil && len(speed.Benchmarks) > 0 {
			summary["speedSummary"] = speed
		}
	}
	return summary
}

func cloneBenchmarkResults(benchmarks []BenchmarkResult) []BenchmarkResult {
	cloned := append([]BenchmarkResult(nil), benchmarks...)
	for i := range cloned {
		cloned[i].Samples = append([]SpeedSample(nil), benchmarks[i].Samples...)
	}
	return cloned
}

// cloneJobLocked detaches API responses from the live job mutated by worker
// goroutines. Callers must hold m.mu while taking this snapshot.
func cloneJobLocked(job *Job) *Job {
	if job == nil {
		return nil
	}
	cloned := *job
	cloned.Log = append([]string(nil), job.Log...)
	if job.StorageSpace != nil {
		space := *job.StorageSpace
		space.Members = append([]string(nil), job.StorageSpace.Members...)
		cloned.StorageSpace = &space
	}
	switch result := job.Result.(type) {
	case *ScanResult:
		copyResult := *result
		copyResult.RepairTargets = append([]RecordedRepairTarget(nil), result.RepairTargets...)
		copyResult.Map = append([]ScanCell{}, result.Map...)
		copyResult.Stats.BadRanges = append([]BadRange{}, result.Stats.BadRanges...)
		cloned.Result = &copyResult
	case *SpeedResult:
		copyResult := *result
		copyResult.Benchmarks = cloneBenchmarkResults(result.Benchmarks)
		cloned.Result = &copyResult
	case *speedInProgress:
		copyResult := *result
		copyResult.Benchmarks = cloneBenchmarkResults(result.Benchmarks)
		cloned.Result = &copyResult
	}
	return &cloned
}

type JobManager struct {
	scanProbeTimeout time.Duration
	scanProbeLimit   int
	scanActivityFn   func(string) (ScanActivity, error)
	mu               sync.Mutex
	dataDir          string
	jobsDir          string
	jobs             map[string]*Job
	processes        map[string]ddProcess
	handles          map[string]ReadHandle
	cancels          map[string]*jobCancel
	closedHandles    map[ReadHandle]bool
	handleCloseDone  map[ReadHandle]chan struct{}
	scanReadTimeout  time.Duration
	lastPersisted    map[string]time.Time
	resolveDisk      func(string) (*Disk, error)
	resolveSpace     func(string) (*StorageSpace, error)
	findExecutable   func(string) string
	openFn           readOpener
	openRepairFn     func(string) (RepairHandle, error)
	spawnFn          ddSpawner
	delayFn          func(time.Duration)
	wakeDiskFn       func(Disk) error
	scanEvidenceFn   func(Disk) ScanEvidence
	nowFn            func() time.Time
	logger           logger
	mock             bool
	stopped          bool
	nextHistoryOrder int
}

type JobManagerOptions struct {
	ScanProbeTimeout time.Duration
	ScanProbeLimit   int
	ScanActivity     func(string) (ScanActivity, error)
	ScanReadTimeout  time.Duration
	Mock             bool
	ResolveDisk      func(string) (*Disk, error)
	ResolveSpace     func(string) (*StorageSpace, error)
	FindExecutable   func(string) string
	Open             readOpener
	OpenRepair       func(string) (RepairHandle, error)
	Spawn            ddSpawner
	Delay            func(time.Duration)
	WakeDisk         func(Disk) error
	ScanEvidence     func(Disk) ScanEvidence
	Now              func() time.Time
	Logger           logger
}

func NewJobManager(dataDir string, options JobManagerOptions) *JobManager {
	m := &JobManager{
		scanProbeTimeout: options.ScanProbeTimeout,
		scanProbeLimit:   options.ScanProbeLimit,
		scanActivityFn:   options.ScanActivity,
		dataDir:          dataDir,
		jobsDir:          filepath.Join(dataDir, "jobs"),
		jobs:             map[string]*Job{},
		processes:        map[string]ddProcess{},
		handles:          map[string]ReadHandle{},
		cancels:          map[string]*jobCancel{},
		closedHandles:    map[ReadHandle]bool{},
		handleCloseDone:  map[ReadHandle]chan struct{}{},
		scanReadTimeout:  options.ScanReadTimeout,
		lastPersisted:    map[string]time.Time{},
		resolveDisk:      options.ResolveDisk,
		resolveSpace:     options.ResolveSpace,
		findExecutable:   options.FindExecutable,
		openFn:           options.Open,
		openRepairFn:     options.OpenRepair,
		spawnFn:          options.Spawn,
		delayFn:          options.Delay,
		wakeDiskFn:       options.WakeDisk,
		scanEvidenceFn:   options.ScanEvidence,
		nowFn:            options.Now,
		logger:           options.Logger,
		mock:             options.Mock,
		nextHistoryOrder: 1,
	}
	if m.resolveDisk == nil {
		m.resolveDisk = func(path string) (*Disk, error) { return resolveDisk(path, m.findExecutable) }
	}
	if m.resolveSpace == nil {
		m.resolveSpace = func(path string) (*StorageSpace, error) { return resolveStorageSpace(path, m.findExecutable) }
	}
	if m.findExecutable == nil {
		m.findExecutable = findExecutable
	}
	if m.openFn == nil {
		m.openFn = openReadOnly
	}
	if m.openRepairFn == nil {
		m.openRepairFn = openRepairDevice
	}
	if m.spawnFn == nil {
		m.spawnFn = spawnDD
	}
	if m.delayFn == nil {
		m.delayFn = func(d time.Duration) { time.Sleep(d) }
	}
	if m.wakeDiskFn == nil {
		m.wakeDiskFn = func(disk Disk) error { return wakeDiskWithSmartctl(disk, m.findExecutable) }
	}
	if m.scanEvidenceFn == nil {
		m.scanEvidenceFn = func(disk Disk) ScanEvidence { return readScanEvidence(disk, m.findExecutable) }
	}
	if m.nowFn == nil {
		m.nowFn = time.Now
	}
	if m.scanReadTimeout <= 0 {
		m.scanReadTimeout = 10 * time.Second
	}
	if m.scanProbeTimeout <= 0 {
		m.scanProbeTimeout = 15 * time.Second
	}
	if m.scanProbeLimit <= 0 {
		m.scanProbeLimit = 256
	}
	if m.scanActivityFn == nil {
		m.scanActivityFn = readScanActivity
	}
	if m.logger == nil {
		m.logger = defaultLogger{}
	}
	os.MkdirAll(m.jobsDir, 0o700)
	m.loadJobs()
	return m
}

func (m *JobManager) now() time.Time { return m.nowFn() }

func (m *JobManager) loadJobs() {
	entries, err := os.ReadDir(m.jobsDir)
	if err != nil {
		return
	}
	var loaded []*Job
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		data, err := os.ReadFile(filepath.Join(m.jobsDir, name))
		if err != nil {
			continue
		}
		var job Job
		if err := json.Unmarshal(data, &job); err != nil {
			continue
		}
		if !jobIDPattern.MatchString(job.ID) || job.ID != id {
			continue
		}
		if job.Type == "scan" || job.Type == "write" {
			encoded, marshalErr := json.Marshal(job.Result)
			var result ScanResult
			if marshalErr == nil && json.Unmarshal(encoded, &result) == nil && len(result.Map) > 0 {
				job.Result = &result
			}
		}
		switch job.Status {
		case "running", "cancelling", "queued":
			job.Status = "interrupted"
			finished := isoTime(m.now())
			job.FinishedAt = &finished
			job.Message = "应用服务重启，任务已中断"
		}
		switch job.Status {
		case "cancelled":
			job.Phase = "任务已取消"
		case "completed":
			switch job.Type {
			case "scan":
				job.Phase = "扫描完成"
				if job.Options.ScanRepairFrom != "" {
					job.Phase = "慢区修复完成"
				}
			case "speed":
				job.Phase = "测速完成"
			case "write":
				job.Phase = "深度检测完成"
			}
		case "failed":
			job.Phase = "任务失败"
		case "interrupted":
			job.Phase = "任务已中断"
		}
		m.restoreScanIssues(&job)
		loaded = append(loaded, &job)
	}

	var ordered, legacy []*Job
	for _, job := range loaded {
		if job.HistoryOrder > 0 {
			ordered = append(ordered, job)
		} else {
			legacy = append(legacy, job)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].HistoryOrder < ordered[j].HistoryOrder })
	sort.Slice(legacy, func(i, j int) bool { return legacy[i].CreatedAt < legacy[j].CreatedAt })

	monotonic := true
	for i := 1; i < len(ordered); i++ {
		if ordered[i-1].CreatedAt > ordered[i].CreatedAt {
			monotonic = false
			break
		}
	}
	var normalized []*Job
	if len(ordered) == 0 {
		normalized = legacy
	} else if monotonic {
		normalized = append(append([]*Job{}, ordered...), legacy...)
		sort.Slice(normalized, func(i, j int) bool {
			if normalized[i].CreatedAt != normalized[j].CreatedAt {
				return normalized[i].CreatedAt < normalized[j].CreatedAt
			}
			aOrder, bOrder := normalized[i].HistoryOrder, normalized[j].HistoryOrder
			if aOrder <= 0 {
				aOrder = int(^uint(0) >> 1)
			}
			if bOrder <= 0 {
				bOrder = int(^uint(0) >> 1)
			}
			return aOrder < bOrder
		})
	} else {
		normalized = append(append([]*Job{}, legacy...), ordered...)
	}
	for i, job := range normalized {
		job.HistoryOrder = i + 1
	}
	for _, job := range normalized {
		m.jobs[job.ID] = job
	}
	m.nextHistoryOrder = len(normalized) + 1
	m.pruneHistoryLocked()
	for _, job := range m.jobs {
		m.persistSafeLocked(job, "恢复任务历史")
	}
}

func (m *JobManager) persist(job *Job) error {
	if !jobIDPattern.MatchString(job.ID) {
		return errors.New("任务 ID 无效，拒绝保存")
	}
	target := filepath.Join(m.jobsDir, job.ID+".json")
	temporary := target + ".tmp"
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	f, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, target); err != nil {
		os.Remove(temporary)
		return err
	}
	m.lastPersisted[job.ID] = m.now()
	return nil
}

func (m *JobManager) persistSafe(job *Job, contextMsg string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.persistSafeLocked(job, contextMsg)
}

func (m *JobManager) persistSafeLocked(job *Job, contextMsg string) bool {
	previousWarning := job.PersistenceWarning
	job.PersistenceWarning = ""
	err := m.persist(job)
	if err == nil {
		return true
	}
	m.lastPersisted[job.ID] = m.now()
	job.PersistenceWarning = contextMsg + "失败；服务重启后可能无法恢复本次进度"
	m.logger.Errorf("[diskbench] %s失败 (%v)", contextMsg, err)
	if previousWarning != "" {
		job.PersistenceWarning = previousWarning
	}
	return false
}

func (m *JobManager) persistThrottled(job *Job, force bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	last, ok := m.lastPersisted[job.ID]
	now := m.now()
	interval := progressPersistInterval
	if job.Type == "scan" {
		interval = scanProgressPersistInterval
	}
	if !ok || force || now.Sub(last) >= interval {
		m.persistSafeLocked(job, "保存任务状态")
	}
}

func (m *JobManager) appendLog(job *Job, message string) {
	clean := strings.Map(func(r rune) rune {
		if r >= 0 && r <= 8 || r == 11 || r == 12 || r >= 14 && r <= 31 || r == 127 {
			return -1
		}
		return r
	}, message)
	clean = strings.TrimSpace(clean)
	if clean == "" {
		return
	}
	job.Log = append(job.Log, clean)
	if len(job.Log) > 120 {
		job.Log = job.Log[len(job.Log)-120:]
	}
}

func (m *JobManager) pruneHistoryLocked() {
	var ordered []*Job
	for _, job := range m.jobs {
		if m.jobBusyLocked(job) {
			continue
		}
		ordered = append(ordered, job)
	}
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.HistoryOrder != b.HistoryOrder {
			return a.HistoryOrder > b.HistoryOrder
		}
		return a.CreatedAt > b.CreatedAt
	})
	if len(ordered) > maxJobHistory {
		jobsDirectory := filepath.Clean(m.jobsDir)
		for _, job := range ordered[maxJobHistory:] {
			target := filepath.Join(m.jobsDir, job.ID+".json")
			if filepath.Dir(target) != jobsDirectory {
				continue
			}
			if err := os.Remove(target); err != nil {
				continue
			}
			delete(m.jobs, job.ID)
			m.removeScanIssueFile(job.ID)
			delete(m.lastPersisted, job.ID)
		}
	}
}

func (m *JobManager) list(summary bool) []interface{} {
	return m.listMatching(summary, false, nil)
}

func (m *JobManager) listCurrent(summary bool, includeIDs []string) []interface{} {
	included := make(map[string]bool, len(includeIDs))
	for _, id := range includeIDs {
		included[id] = true
	}
	return m.listMatching(summary, true, included)
}

func (m *JobManager) listMatching(summary, activeOnly bool, included map[string]bool) []interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	var active, history []*Job
	for _, job := range m.jobs {
		if activeOnly && !activeJobStatuses[job.Status] && !included[job.ID] {
			continue
		}
		if activeJobStatuses[job.Status] {
			active = append(active, job)
		} else {
			history = append(history, job)
		}
	}
	sortByNewest := func(jobs []*Job) {
		sort.Slice(jobs, func(i, j int) bool {
			a, b := jobs[i], jobs[j]
			if a.HistoryOrder != b.HistoryOrder {
				return a.HistoryOrder > b.HistoryOrder
			}
			return a.CreatedAt > b.CreatedAt
		})
	}
	sortByNewest(active)
	sortByNewest(history)
	if len(history) > maxJobHistory {
		history = history[:maxJobHistory]
	}
	all := append(active, history...)
	out := make([]interface{}, 0, len(all))
	for _, job := range all {
		if summary && !activeJobStatuses[job.Status] {
			out = append(out, summarizeJobJSON(job))
		} else {
			out = append(out, cloneJobLocked(job))
		}
	}
	return out
}

func (m *JobManager) get(id string) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneJobLocked(m.jobs[id])
}

func (m *JobManager) scanProgress(id string, after int) (*ScanProgressSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[id]
	if job == nil {
		return nil, newJobError("任务不存在", "JOB_NOT_FOUND", 404)
	}
	if job.Type != "scan" && job.Type != "write" {
		return nil, newJobError("该任务没有块图进度", "SCAN_PROGRESS_UNAVAILABLE", 409)
	}
	result, ok := job.Result.(*ScanResult)
	if !ok || result == nil {
		return nil, newJobError("块图进度暂不可用", "SCAN_PROGRESS_UNAVAILABLE", 409)
	}
	if after < 0 || after > len(result.Map) {
		return nil, newJobError("after 超出块图范围", "INVALID_ARGUMENT", 400)
	}

	end := after
	for end < len(result.Map) {
		status := result.Map[end].Status
		if status == "pending" || status == "reading" {
			break
		}
		end++
	}
	cells := append([]ScanCell{}, result.Map[after:end]...)
	stats := result.Stats
	stats.BadRanges = append([]BadRange{}, result.Stats.BadRanges...)
	return &ScanProgressSnapshot{
		ID: job.ID, Status: job.Status, Progress: job.Progress, Phase: job.Phase,
		Message: job.Message, FinishedAt: job.FinishedAt,
		PersistenceWarning: job.PersistenceWarning,
		Cells:              cells, Next: end, Stats: stats,
		Diagnostics: result.Diagnostics,
	}, nil
}

func (m *JobManager) remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !jobIDPattern.MatchString(id) || m.jobs[id] == nil {
		return newJobError("任务不存在", "JOB_NOT_FOUND", 404)
	}
	if m.jobBusyLocked(m.jobs[id]) {
		return newJobError("运行中的任务不能删除", "JOB_ACTIVE", 409)
	}
	target := filepath.Join(m.jobsDir, id+".json")
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return newJobError("删除历史记录失败", "HISTORY_DELETE_FAILED", 500)
	}
	delete(m.jobs, id)
	m.removeScanIssueFile(id)
	delete(m.lastPersisted, id)
	return nil
}

func (m *JobManager) checkCancelled(job *Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if job.Status == "cancelling" {
		return errCancelled
	}
	if job.Status != "running" {
		return errors.New("任务已停止")
	}
	return nil
}

func (m *JobManager) start(job *Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped || job.Status != "queued" {
		if job.Status == "cancelling" {
			return errCancelled
		}
		return errors.New("任务已停止")
	}
	job.Status = "running"
	started := isoTime(m.now())
	job.StartedAt = &started
	m.persistSafeLocked(job, "保存任务启动状态")
	return nil
}

func (m *JobManager) complete(job *Job, result interface{}, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if job.Status == "cancelling" {
		return
	}
	job.Status = "completed"
	job.Progress = 100
	switch job.Type {
	case "scan":
		job.Phase = "扫描完成"
		if job.Options.ScanRepairFrom != "" {
			job.Phase = "慢区修复完成"
		}
	case "speed":
		job.Phase = "测速完成"
	case "write":
		job.Phase = "深度检测完成"
	}
	finished := isoTime(m.now())
	job.FinishedAt = &finished
	job.Result = result
	job.Message = message
	m.persistSafeLocked(job, "保存任务完成状态")
	m.pruneHistoryLocked()
}

func (m *JobManager) fail(job *Job, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if terminalJobStatuses[job.Status] {
		return
	}
	status := "failed"
	if job.Status == "cancelling" {
		status = "cancelled"
	}
	job.Status = status
	finished := isoTime(m.now())
	job.FinishedAt = &finished
	if status == "cancelled" {
		job.Message = "任务已由用户取消"
		job.Phase = "任务已取消"
	} else {
		message := "任务执行失败"
		if err != nil && err.Error() != "" {
			message = err.Error()
		}
		job.Message = message
		job.Phase = "任务失败"
	}
	m.appendLog(job, job.Message)
	m.persistSafeLocked(job, "保存任务失败状态")
	m.pruneHistoryLocked()
}

func (m *JobManager) updateCell(cell *ScanCell, milliseconds float64, bytesRead int64, errorCount int64, transferBytes int64) {
	cell.Reads++
	cell.BytesRead += bytesRead
	cell.ErrorCount += errorCount
	maxMs := milliseconds
	if cell.MaxMs != nil && *cell.MaxMs > maxMs {
		maxMs = *cell.MaxMs
	}
	totalMs := milliseconds
	if cell.TotalMs != nil {
		totalMs = *cell.TotalMs + milliseconds
	}
	cell.MaxMs = floatPtr(round1(maxMs))
	cell.TotalMs = floatPtr(totalMs)
	cell.AvgMs = floatPtr(round1(totalMs / float64(cell.Reads)))
	unitMs := scanUnitLatency(milliseconds, transferBytes)
	if cell.MaxUnitMs == nil || unitMs > *cell.MaxUnitMs {
		cell.MaxUnitMs = floatPtr(unitMs)
	}
	cell.Status = classifyLatency(*cell.MaxUnitMs, cell.ErrorCount)
}

func (m *JobManager) updateScanStats(job *Job) {
	result := job.Result.(*ScanResult)
	tested := 0
	errorCells := 0
	slowCells := 0
	var bytesRead int64
	var coveredBytes int64
	for i := range result.Map {
		cell := &result.Map[i]
		if result.Version >= 2 {
			bytesRead += cell.BytesRead
			coveredBytes += cell.CoveredBytes
		}
		if cell.Status == "pending" || cell.Status == "reading" {
			continue
		}
		tested++
		if cell.Status == "error" {
			errorCells++
		}
		if cell.Status == "slow" || cell.Status == "slower" || cell.Status == "severe" || cell.Status == "under150" || cell.Status == "under500" || cell.Status == "over500" {
			slowCells++
		}
		if result.Version < 2 {
			bytesRead += cell.BytesRead
		}
	}
	result.Stats.TestedCells = int64(tested)
	result.Stats.ErrorCells = int64(errorCells)
	result.Stats.SlowCells = int64(slowCells)
	result.Stats.BytesRead = bytesRead
	result.Stats.CoveredBytes = coveredBytes
}

type timedReadResult struct {
	bytesRead    int
	milliseconds float64
}

func (m *JobManager) timedRead(handle ReadHandle, buffer []byte, length int, position int64, bufferOffset int) (timedReadResult, error) {
	// A short read can leave a sub-slice unaligned for O_DIRECT. Bounce the
	// retry through aligned memory; keep it alive even if cancellation wins.
	target := buffer[bufferOffset : bufferOffset+length]
	if bufferOffset != 0 {
		target = scanBuffer(length)
	}
	started := time.Now()
	n, err := handle.ReadAt(target, position)
	elapsed := float64(time.Since(started).Nanoseconds()) / 1e6
	if bufferOffset != 0 && n > 0 && n <= length {
		copy(buffer[bufferOffset:], target[:n])
	}
	return timedReadResult{bytesRead: n, milliseconds: elapsed}, err
}

func quickSampleBounds(cell *ScanCell, diskSize, logicalSectorSize, maxSampleLength int64) (int64, int64) {
	regionLength := cell.End - cell.Start
	if regionLength < logicalSectorSize {
		regionLength = logicalSectorSize
	}
	length := alignDown(regionLength, logicalSectorSize)
	if length <= 0 {
		length = logicalSectorSize
	}
	if length > maxSampleLength {
		length = maxSampleLength
	}
	center := cell.Start + (regionLength-length)/2
	if center < 0 {
		center = 0
	}
	position := alignDown(center, logicalSectorSize)
	limit := diskSize - length
	if position > limit {
		position = limit
	}
	if position < 0 {
		position = 0
	}
	return position, length
}

type readWithRetriesResult struct {
	bytesRead int
	shortRead *BadRange
	err       error
}

func (m *JobManager) readWithRetries(handle ReadHandle, buffer []byte, length int, position int64, job *Job) (readWithRetriesResult, error) {
	bytesRead := 0
	var shortRead *BadRange
	shortReadRetries := 0

	for bytesRead < length {
		if err := m.checkCancelled(job); err != nil {
			return readWithRetriesResult{}, err
		}
		remaining := length - bytesRead
		result, err := awaitCancelable(m, job, func() (timedReadResult, error) {
			return m.timedRead(handle, buffer, remaining, position+int64(bytesRead), bytesRead)
		})
		if result.bytesRead < 0 || result.bytesRead > remaining {
			return readWithRetriesResult{}, errors.New("读取返回了无效的字节数")
		}
		if errors.Is(err, io.EOF) {
			err = nil
		}
		if err != nil {
			if m.checkCancelled(job) != nil {
				return readWithRetriesResult{}, errCancelled
			}
			if errors.Is(err, errScanSlowBudget) {
				return readWithRetriesResult{bytesRead: bytesRead + result.bytesRead, shortRead: shortRead}, err
			}
			if errors.Is(err, errScanWaitTimeout) {
				if waitErr := m.waitPendingScanRead(job); waitErr != nil {
					return readWithRetriesResult{}, waitErr
				}
				// The original request has settled; this timeout may now skip
				// the region without overlapping the next kernel read.
				if evidence := scanProtocolError(err); evidence != nil {
					err = evidence
				} else {
					return readWithRetriesResult{}, err
				}
			}
			err = classifyScanSystemError(err)
			var cerr error
			if err == errCancelled {
				cerr = err
			} else {
				cerr = err
			}
			if !isRecoverableMediaError(cerr) && scanProtocolError(cerr) == nil {
				return readWithRetriesResult{}, cerr
			}
			if shortRead != nil {
				recovered := false
				shortRead.Recovered = &recovered
			}
			return readWithRetriesResult{bytesRead: bytesRead + result.bytesRead, shortRead: shortRead, err: cerr}, nil
		}
		if m.checkCancelled(job) != nil {
			return readWithRetriesResult{}, errCancelled
		}

		currentBytes := result.bytesRead
		if currentBytes < 0 || currentBytes > remaining {
			return readWithRetriesResult{}, errors.New("读取返回了无效的字节数")
		}
		bytesRead += currentBytes
		if currentBytes == remaining {
			break
		}

		if shortRead == nil {
			shortRead = &BadRange{
				Offset: position + int64(bytesRead),
				Length: int64(length - bytesRead),
				Code:   "SHORT_READ",
			}
		}
		shortReadRetries++
		if shortReadRetries > maxShortReadRetries {
			break
		}
	}

	if shortRead != nil {
		recovered := bytesRead == length
		shortRead.Recovered = &recovered
	}
	return readWithRetriesResult{bytesRead: bytesRead, shortRead: shortRead, err: nil}, nil
}

func isRecoverableMediaError(err error) bool {
	if err == nil {
		return false
	}
	var je *jobError
	if errors.As(err, &je) {
		return false
	}
	if err == errCancelled {
		return false
	}
	if e := scanProtocolError(err); e != nil {
		return localizableScanIssue(e)
	}
	if isScanMediaError(err) {
		return true
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return recoverableMediaErrors[pathErr.Err.Error()]
	}
	return false
}

func (m *JobManager) locateReadErrors(handle ReadHandle, start, length int64, job *Job, limit int, probeSize int64) ([]BadRange, int, error) {
	if probeSize <= 0 {
		probeSize = errorProbeSize
	}
	buffer := scanBuffer(errorProbeSize)
	timeout, requests := m.scanProbeTimeout, m.scanProbeLimit
	if job.Options.ScanDeepLocate != nil && *job.Options.ScanDeepLocate {
		timeout, requests = max(timeout, 2*time.Minute), max(requests, 4096)
	}
	handle = &scanProbeReader{ReadHandle: handle, deadline: time.Now().Add(timeout), remaining: requests}
	var badRanges []BadRange
	bytesRead := 0
	var lastEvidence *ScanIOEvidence
	slowLimited := false
	var probe func(int64, int64) error
	probe = func(position, size int64) error {
		if slowLimited {
			appendSlowSkippedRange(&badRanges, position, size, limit)
			return nil
		}
		result, err := m.readWithRetries(handle, buffer, int(size), position, job)
		if errors.Is(err, errScanSlowBudget) {
			bytesRead += result.bytesRead
			appendSlowSkippedRange(&badRanges, position+int64(result.bytesRead), size-int64(result.bytesRead), limit)
			return nil
		}
		if errors.Is(err, errScanProbeBudget) {
			appendUnresolvedProbeRange(&badRanges, position, size, lastEvidence, limit)
			return nil
		}
		if err != nil {
			return err
		}
		if e := scanProtocolError(result.err); e != nil && e.Evidence.Kind == "TIMEOUT" {
			prefix := scanTimeoutPrefix(result, position)
			bytesRead += prefix
			appendBadRanges(&badRanges, []BadRange{scanIssueRange(position, size, result.err, boolPtr(false))}, limit)
			appendSlowSkippedRange(&badRanges, position+int64(prefix), size-int64(prefix), limit)
			slowLimited = true
			return nil
		}
		issue := result.err != nil || result.shortRead != nil || result.bytesRead != int(size)
		if e := scanProtocolError(result.err); e != nil {
			evidence := e.Evidence
			lastEvidence = &evidence
		}
		if !issue {
			bytesRead += int(size)
			return nil
		}
		if e := scanProtocolError(result.err); e != nil && !localizableScanIssue(e) {
			recovered := e.Evidence.Kind == "RECOVERED_ERROR" && result.bytesRead == int(size)
			appendBadRanges(&badRanges, []BadRange{scanIssueRange(position, size, result.err, &recovered)}, limit)
			if recovered {
				bytesRead += int(size)
				return nil
			}
			return result.err // A link/controller failure is not a sector search.
		}
		if size > probeSize {
			half := alignDown(size/2, probeSize)
			if half <= 0 {
				half = probeSize
			}
			before := bytesRead
			detailBefore := len(badRanges)
			if err := probe(position, half); err != nil {
				return err
			}
			if err := probe(position+half, size-half); err != nil {
				return err
			}
			if bytesRead-before == int(size) && len(badRanges) == detailBefore {
				recovered := true
				r := scanIssueRange(position, size, result.err, &recovered)
				if r.Code == "EIO" && scanProtocolError(result.err) == nil {
					r.Code = "READ_RETRY"
				}
				appendBadRanges(&badRanges, []BadRange{r}, limit)
			}
			return nil
		}
		// A second direct read distinguishes a transient failure from a range
		// still unreadable now. Never erase the first failure.
		if e := scanProtocolError(result.err); e != nil && e.Evidence.DoNotRetry {
			r := scanIssueRange(position, size, result.err, boolPtr(false))
			r.Outcome, r.ProbeBytes = "unreadable", min(probeSize, size)
			appendBadRanges(&badRanges, []BadRange{r}, limit)
			return nil
		}
		retry, err := m.readWithRetries(handle, buffer, int(size), position, job)
		if errors.Is(err, errScanSlowBudget) {
			bytesRead += retry.bytesRead
			appendSlowSkippedRange(&badRanges, position+int64(retry.bytesRead), size-int64(retry.bytesRead), limit)
			return nil
		}
		if errors.Is(err, errScanProbeBudget) {
			appendUnresolvedProbeRange(&badRanges, position, size, lastEvidence, limit)
			return nil
		}
		if err != nil {
			return err
		}
		if e := scanProtocolError(retry.err); e != nil && e.Evidence.Kind == "TIMEOUT" {
			prefix := scanTimeoutPrefix(retry, position)
			bytesRead += prefix
			appendBadRanges(&badRanges, []BadRange{scanIssueRange(position, size, retry.err, boolPtr(false))}, limit)
			appendSlowSkippedRange(&badRanges, position+int64(prefix), size-int64(prefix), limit)
			slowLimited = true
			return nil
		}
		if e := scanProtocolError(retry.err); e != nil && !localizableScanIssue(e) && e.Evidence.Kind != "RECOVERED_ERROR" {
			appendBadRanges(&badRanges, []BadRange{scanIssueRange(position, size, retry.err, boolPtr(false))}, limit)
			return retry.err
		}
		firmwareRecovered := scanProtocolError(retry.err) != nil && scanProtocolError(retry.err).Evidence.Kind == "RECOVERED_ERROR"
		recovered := (retry.err == nil || firmwareRecovered) && retry.bytesRead == int(size)
		if retry.err == nil || firmwareRecovered {
			bytesRead += retry.bytesRead
		}
		code := "SHORT_READ"
		if result.err != nil {
			code = "EIO"
		}
		r := scanIssueRange(position, size, result.err, &recovered)
		r.ProbeBytes = min(probeSize, size)
		if !recovered {
			r.Outcome = "unreadable"
		}
		if result.err == nil {
			r.Code = code
		}
		appendBadRanges(&badRanges, []BadRange{r}, limit)
		return nil
	}
	for position := start; position < start+length; {
		size := min(int64(errorProbeSize), start+length-position)
		if err := probe(position, size); err != nil {
			return badRanges, bytesRead, err
		}
		position += size
	}
	return badRanges, bytesRead, nil
}

type readRegionResult struct {
	postRepairReads []ScanReadLatency
	slowLimited     bool
	suspectReads    []ScanReadLatency
	ioBytes         int64
	slowReads       []ScanReadLatency
	maxReadLatency  *ScanReadLatency
	bytesRead       int
	milliseconds    float64
	badRanges       []BadRange
	errorCount      int
}

func (m *JobManager) readRegion(handle ReadHandle, buffer []byte, length int, position int64, job *Job, detailLimit int, probeSize int64) (result readRegionResult, resultErr error) {
	reader := &scanRequestReader{ReadHandle: handle, check: func() error { return m.checkCancelled(job) }}
	reader.recheckAfterMs = 20
	if job.Disk.Rotational {
		reader.recheckAfterMs = 50
	}
	reader.repairAfterMs = scanRepairThreshold(job)
	baseHandle := handle
	reader.read = func(p []byte, offset int64) (int, error) { return m.readScanRequest(job, baseHandle, p, offset) }
	defer func() {
		result.maxReadLatency = reader.snapshot()
		result.slowReads = reader.slowSnapshot()
		result.suspectReads, result.ioBytes = reader.recheckSnapshot()
		for i := range result.badRanges {
			result.badRanges[i] = classifyScanRange(result.badRanges[i])
			if result.badRanges[i].Code == "SLOW_READ_LIMIT" {
				result.slowLimited = true
			}
		}
	}()
	handle = reader
	started := time.Now()
	rangeLimit := detailLimit
	primary, err := m.readWithRetries(handle, buffer, length, position, job)
	primaryFailure := reader.failureSnapshot()
	if errors.Is(err, errScanSlowBudget) {
		ranges := []BadRange{}
		issueCount := 0
		if primary.shortRead != nil {
			appendBadRanges(&ranges, []BadRange{*primary.shortRead}, rangeLimit)
			issueCount = 1
		}
		appendSlowSkippedRange(&ranges, position+int64(primary.bytesRead), int64(length-primary.bytesRead), rangeLimit)
		return readRegionResult{slowLimited: true, bytesRead: primary.bytesRead, milliseconds: float64(time.Since(started)) / float64(time.Millisecond), badRanges: ranges, errorCount: issueCount}, nil
	}
	if err != nil {
		return readRegionResult{}, err
	}
	if e := scanProtocolError(primary.err); e != nil && !localizableScanIssue(e) {
		if e.Evidence.Kind == "TIMEOUT" && !errors.Is(primary.err, errScanWaitTimeout) {
			readBytes := scanTimeoutPrefix(primary, position)
			ranges := []BadRange{scanIssueRange(position, int64(length), primary.err, boolPtr(false))}
			appendSlowSkippedRange(&ranges, position+int64(readBytes), int64(length-readBytes), rangeLimit)
			return readRegionResult{slowLimited: true, bytesRead: readBytes, milliseconds: float64(time.Since(started)) / float64(time.Millisecond), badRanges: ranges, errorCount: 1}, nil
		}
		// Preserve recovered firmware errors. Retry transient command/transport
		// failures once, then stop instead of painting a whole disk as bad.
		recovered := e.Evidence.Kind == "RECOVERED_ERROR" && primary.bytesRead == length
		readBytes := primary.bytesRead
		var stopErr error
		if errors.Is(primary.err, errScanWaitTimeout) {
			stopErr = primary.err
		} else if !recovered && e.Evidence.DoNotRetry {
			stopErr = fmt.Errorf("设备要求不重试，已停止扫描（不能据此判定介质坏块）：%w", primary.err)
		} else if !recovered {
			retry, retryErr := m.readWithRetries(handle, buffer, length, position, job)
			if retryErr == errCancelled {
				return readRegionResult{}, retryErr
			}
			recovered = retryErr == nil && retry.err == nil && retry.bytesRead == length
			readBytes = retry.bytesRead
			if !recovered {
				stopErr = fmt.Errorf("读取命令异常，已停止扫描（不能据此判定盘面坏道）：%w", primary.err)
			}
		}
		ranges := []BadRange{}
		appendBadRanges(&ranges, []BadRange{scanIssueRange(position, int64(length), primary.err, &recovered)}, rangeLimit)
		return readRegionResult{bytesRead: readBytes, milliseconds: float64(time.Since(started).Nanoseconds()) / 1e6, badRanges: ranges, errorCount: 1}, stopErr
	}
	if e := scanProtocolError(primary.err); e != nil && localizableScanIssue(e) && e.Evidence.DoNotRetry && int64(length) <= probeSize && primary.bytesRead == 0 {
		ranges := []BadRange{}
		appendBadRanges(&ranges, []BadRange{scanIssueRange(position, int64(length), primary.err, boolPtr(false))}, rangeLimit)
		for i := range ranges {
			ranges[i].Outcome = "unreadable"
			ranges[i].ProbeBytes = min(probeSize, int64(length))
		}
		return readRegionResult{milliseconds: float64(time.Since(started).Nanoseconds()) / 1e6, badRanges: ranges, errorCount: 1}, nil
	}
	var badRanges []BadRange
	bytesRead := primary.bytesRead
	hadReadIssue := primary.err != nil || primary.shortRead != nil || bytesRead != length

	if primary.shortRead != nil {
		appendBadRanges(&badRanges, []BadRange{*primary.shortRead}, rangeLimit)
	}
	// A driver can return a full byte count alongside an error. Those bytes
	// are not evidence of recovery: explicitly re-read the affected request.
	if primary.err != nil && bytesRead == length {
		bytesRead = 0
	}
	if bytesRead < length {
		if budget, ok := baseHandle.(*scanSlowBudgetReader); ok && job.Options.ScanDeepLocate != nil && *job.Options.ScanDeepLocate {
			// The normal five-second region limit must not silently disable
			// explicitly requested deep localization. scanProbeReader still
			// bounds this phase by its deadline and number of requests.
			baseHandle = &scanLocalizationReader{budget}
		}
		located, locatedBytes, locateErr := m.locateReadErrors(handle, position+int64(bytesRead), int64(length-bytesRead), job, rangeLimit-len(badRanges), probeSize)
		bytesRead += locatedBytes
		appendBadRanges(&badRanges, located, rangeLimit)
		if primary.err != nil && primaryFailure != nil && sumUnresolvedProbeBytes(located) > 0 {
			preserved := false
			for _, issue := range located {
				if issue.Code != "SLOW_READ_LIMIT" && issue.Code != "LOCALIZATION_LIMIT" {
					preserved = true
					break
				}
			}
			if !preserved {
				issue := scanIssueRange(primaryFailure.Offset, int64(primaryFailure.Bytes), primaryFailure.Err, nil)
				issue.Outcome = "unresolved"
				issue.Detail = "读取请求失败，定位未完成；请求范围不代表每个扇区均损坏"
				// This observation overlaps the unresolved coverage. Do not
				// count its bytes twice or infer unreadable sectors from it.
				appendBadRanges(&badRanges, []BadRange{issue}, rangeLimit)
			}
		}
		if locateErr != nil {
			return readRegionResult{bytesRead: bytesRead, milliseconds: float64(time.Since(started).Nanoseconds()) / 1e6, badRanges: badRanges, errorCount: max(1, len(badRanges))}, locateErr
		}
	}
	if primary.shortRead != nil && bytesRead == length {
		recovered := true
		primary.shortRead.Recovered = &recovered
		if len(badRanges) > 0 && badRanges[0].Code == "SHORT_READ" {
			badRanges[0].Recovered = &recovered
		}
	}
	if hadReadIssue && len(badRanges) == 0 && bytesRead == length {
		recovered := true
		r := scanIssueRange(position, int64(length), primary.err, &recovered)
		if r.Code == "EIO" && scanProtocolError(primary.err) == nil {
			r.Code = "READ_RETRY"
		}
		appendBadRanges(&badRanges, []BadRange{r}, rangeLimit)
	}
	errorCount := 0
	if hadReadIssue {
		errorCount = len(badRanges)
		if errorCount < 1 {
			errorCount = 1
		}
	}
	return readRegionResult{
		bytesRead:    bytesRead,
		milliseconds: float64(time.Since(started).Nanoseconds()) / 1e6,
		badRanges:    badRanges,
		errorCount:   errorCount,
	}, err
}

type jobCancel struct {
	ctx     context.Context
	cancel  context.CancelFunc
	pending sync.WaitGroup
}

func awaitCancelable[T any](m *JobManager, job *Job, fn func() (T, error)) (T, error) {
	var zero T
	m.mu.Lock()
	cancel, hasCancel := m.cancels[job.ID]
	m.mu.Unlock()
	if !hasCancel {
		return fn()
	}
	select {
	case <-cancel.ctx.Done():
		return zero, errCancelled
	default:
	}
	type outcome struct {
		val T
		err error
	}
	ch := make(chan outcome, 1)
	cancel.pending.Add(1)
	go func() {
		defer cancel.pending.Done()
		val, err := fn()
		ch <- outcome{val, err}
	}()
	select {
	case <-cancel.ctx.Done():
		go func() { <-ch }()
		return zero, errCancelled
	case result := <-ch:
		return result.val, result.err
	}
}

func (m *JobManager) beginCancellation(job *Job) *jobCancel {
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	h := &jobCancel{ctx: ctx, cancel: cancel}
	m.cancels[job.ID] = h
	return h
}

func (m *JobManager) endCancellation(job *Job, h *jobCancel) {
	// The job remains active until abandoned operations have actually returned.
	h.pending.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.cancels[job.ID]; ok && cur == h {
		delete(m.cancels, job.ID)
	}
}

func (m *JobManager) abortCancellation(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if h, ok := m.cancels[id]; ok {
		h.cancel()
	}
}

func (m *JobManager) closeHandle(handle ReadHandle) {
	if handle == nil {
		return
	}
	m.mu.Lock()
	if done := m.handleCloseDone[handle]; done != nil {
		m.mu.Unlock()
		<-done
		return
	}
	done := make(chan struct{})
	m.handleCloseDone[handle] = done
	m.closedHandles[handle] = true
	m.mu.Unlock()
	defer close(done)
	if err := handle.Close(); err != nil {
		m.logger.Errorf("[diskbench] 关闭扫描设备失败 (%v)", err)
	}
	if h, ok := handle.(interface{ CloseDone() <-chan struct{} }); ok {
		<-h.CloseDone()
	}
}

func (m *JobManager) assertDiskIdentity(job *Job) (*Disk, error) {
	if err := m.checkCancelled(job); err != nil {
		return nil, err
	}
	currentDisk, err := awaitCancelable(m, job, func() (*Disk, error) {
		return m.resolveDisk(job.Device)
	})
	if err != nil {
		return nil, err
	}
	if err := m.checkCancelled(job); err != nil {
		return nil, err
	}
	if diskIdentityChanged(job.Device, job.Disk.Serial, job.Disk.Model, job.Disk.Size, currentDisk) {
		return nil, newJobError("磁盘信息已变化，任务已停止", "DISK_CHANGED", 409)
	}
	return currentDisk, nil
}

func sameStrings(first, second []string) bool {
	if len(first) != len(second) {
		return false
	}
	a := append([]string(nil), first...)
	b := append([]string(nil), second...)
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func storageIdentityChanged(expected *StorageSpaceSnapshot, current *StorageSpace) bool {
	return expected == nil || current == nil || !isLocalStorageMount(current.Source, current.FileSystem) ||
		(current.IsSystem && !systemBenchmarkPath(current.Path)) ||
		expected.Path != current.Path || expected.Source != current.Source ||
		expected.FileSystem != current.FileSystem || expected.Layout != current.Layout || expected.Size != current.Size ||
		expected.ReadOnly != current.ReadOnly || expected.IsSystem != current.IsSystem || !sameStrings(expected.Members, current.Members)
}

func benchmarkSpaceCapacityError(space *StorageSpace, fileSize int64) error {
	reserve := int64(64 * 1024 * 1024)
	if space.IsSystem {
		reserve = maxInt64(1024*1024*1024, space.Size/10)
	}
	if space.FreeBytes >= fileSize+reserve {
		return nil
	}
	message := "存储空间可用容量不足以创建测速临时文件"
	if space.IsSystem {
		message = "系统盘空间不足：测速后需保留至少 1 GiB 且不少于总容量的 10%，请减小样本或释放空间"
	}
	return newJobError(message, "STORAGE_SPACE_FULL", 409)
}

func (m *JobManager) assertStorageIdentity(job *Job) (*StorageSpace, error) {
	if err := m.checkCancelled(job); err != nil {
		return nil, err
	}
	current, err := awaitCancelable(m, job, func() (*StorageSpace, error) {
		return m.resolveSpace(job.Device)
	})
	if err != nil {
		return nil, err
	}
	if err := m.checkCancelled(job); err != nil {
		return nil, err
	}
	if storageIdentityChanged(job.StorageSpace, current) {
		return nil, newJobError("存储空间信息已变化，任务已停止", "STORAGE_SPACE_CHANGED", 409)
	}
	return current, nil
}

func rawSpeedBlockedReason(disk *Disk) string {
	if disk == nil {
		return "磁盘不存在或已离线"
	}
	if disk.IsSystem {
		return "系统盘不能执行底层测速"
	}
	if disk.ReadOnly {
		return "磁盘当前为只读状态"
	}
	if len(disk.Mountpoints) > 0 {
		return "磁盘或其下层设备仍处于挂载状态"
	}
	if len(disk.FileSystems) > 0 {
		return "磁盘仍包含文件系统或阵列签名"
	}
	return ""
}

func (m *JobManager) assertRawSpeedIdentity(job *Job) (*Disk, error) {
	disk, err := m.assertDiskIdentity(job)
	if err != nil {
		return nil, err
	}
	if reason := rawSpeedBlockedReason(disk); reason != "" {
		return nil, newJobError(reason+"，底层测速已停止", "RAW_SPEED_TARGET_CHANGED", 409)
	}
	return disk, nil
}

func (m *JobManager) create(typeName, devicePath string, options map[string]interface{}) (*Job, error) {
	if typeName == "write" {
		return nil, newJobError("深度检测已停用，请使用坏道扫描及慢扇区修复", "JOB_TYPE_RETIRED", 410)
	}
	if typeName != "scan" && typeName != "speed" {
		return nil, newJobError("不支持的任务类型", "INVALID_JOB_TYPE", 400)
	}
	resumeID, _ := options["resumeFrom"].(string)
	repairFrom, _ := options["scanRepairFrom"].(string)
	if raw, exists := options["scanRepairFrom"]; exists && (raw != repairFrom || typeName != "scan" || !jobIDPattern.MatchString(repairFrom) || resumeID != "" || options["scanRepair"] != true) {
		return nil, newJobError("按记录修复需要有效的扫描记录、开启修复且不能同时续扫", "INVALID_ARGUMENT", 400)
	}
	var repairSource *Job
	if repairFrom != "" {
		m.mu.Lock()
		repairSource = cloneJobLocked(m.jobs[repairFrom])
		m.mu.Unlock()
	}
	if raw, exists := options["resumeFrom"]; exists && (raw != resumeID || typeName != "scan" || !jobIDPattern.MatchString(resumeID)) {
		return nil, newJobError("resumeFrom 必须是有效的扫描任务 ID", "INVALID_ARGUMENT", 400)
	}
	var resumeSource *Job
	if resumeID != "" {
		m.mu.Lock()
		resumeSource = cloneJobLocked(m.jobs[resumeID])
		m.mu.Unlock()
		if scanResumeInfo(resumeSource) == nil {
			return nil, newJobError("该记录没有可继续的扫描进度", "SCAN_RESUME_UNAVAILABLE", 409)
		}
		// Resume always keeps the original scan geometry and options.
		copied := make(map[string]interface{}, len(options)+5)
		for key, value := range options {
			copied[key] = value
		}
		copied["scanMode"] = *resumeSource.Options.ScanMode
		copied["scanExactSectors"] = resumeSource.Options.ScanExactSectors == nil || *resumeSource.Options.ScanExactSectors
		copied["scanDeepLocate"] = resumeSource.Options.ScanDeepLocate != nil && *resumeSource.Options.ScanDeepLocate
		copied["scanSelfTest"] = resumeSource.Options.ScanSelfTest != nil && *resumeSource.Options.ScanSelfTest
		copied["scanRepair"] = resumeSource.Options.ScanRepair != nil && *resumeSource.Options.ScanRepair
		if resumeSource.Options.ScanRepairAfterMs != nil {
			copied["scanRepairAfterMs"] = *resumeSource.Options.ScanRepairAfterMs
		}
		options = copied
	}
	scanMode, _ := options["scanMode"].(string)
	scanExactSectors, exactSectorsProvided := options["scanExactSectors"].(bool)
	scanDeepLocate := scanMode == "full"
	if value, exists := options["scanDeepLocate"]; exists {
		deep, ok := value.(bool)
		if !ok || typeName != "scan" {
			return nil, newJobError("深入定位选项仅接受扫描任务的布尔值", "INVALID_ARGUMENT", 400)
		}
		scanDeepLocate = deep
	}
	scanSelfTest, _ := options["scanSelfTest"].(bool)
	scanRepair, _ := options["scanRepair"].(bool)
	repairAfterMs := int64(500)
	if value, exists := options["scanRepairAfterMs"]; exists {
		var valid bool
		repairAfterMs, valid = parseScanRepairThreshold(value)
		if !valid {
			return nil, newJobError("修复阈值必须是 1～60000 毫秒的整数", "INVALID_ARGUMENT", 400)
		}
	}
	if value, exists := options["scanRepair"]; exists {
		if _, valid := value.(bool); !valid {
			return nil, newJobError("scanRepair 必须是布尔值", "INVALID_ARGUMENT", 400)
		}
	}
	if scanRepair && typeName != "scan" {
		return nil, newJobError("慢扇区修复仅用于扫描任务", "INVALID_ARGUMENT", 400)
	}
	if !exactSectorsProvided {
		scanExactSectors = true
	}
	sampleMiB, _ := toInt64(options["sampleMiB"])
	runs, _ := toInt64(options["speedRuns"])
	speedPosition, _ := options["speedPosition"].(string)
	speedOperation, _ := options["speedOperation"].(string)
	speedPattern, _ := options["speedPattern"].(string)
	speedBlockSize, _ := toInt64(options["speedBlockSize"])
	speedQueueDepth, _ := toInt64(options["speedQueueDepth"])
	speedThreadCount, _ := toInt64(options["speedThreads"])
	speedIOEngine, _ := options["speedIOEngine"].(string)
	speedDirectIO, directIOProvided := options["speedDirectIO"].(bool)
	speedTrim, _ := options["speedTrim"].(bool)
	speedProfile, _ := options["speedProfile"].(bool)
	speedProfileRow, _ := options["speedProfileRow"].(string)
	speedTargetMode, _ := options["speedTarget"].(string)
	rawWriteAcknowledged, _ := options["rawWriteAcknowledged"].(bool)
	var expectedDisk *Disk
	if raw, ok := options["expectedDisk"].(map[string]interface{}); ok {
		expectedDisk = diskFromMap(raw)
	}
	var expectedSpace *StorageSpace
	if raw, ok := options["expectedStorageSpace"].(map[string]interface{}); ok {
		expectedSpace = storageSpaceFromMap(raw)
	}

	if typeName == "scan" && scanMode != "" && !scanModes[scanMode] {
		return nil, newJobError("扫描方式无效", "INVALID_SCAN_MODE", 400)
	}
	if typeName == "speed" && options["sampleMiB"] != nil && !speedSizes[sampleMiB] {
		return nil, newJobError("测速样本大小无效", "INVALID_SAMPLE_SIZE", 400)
	}
	if typeName == "speed" && options["speedRuns"] != nil && !speedRuns[runs] {
		return nil, newJobError("测速次数无效", "INVALID_SPEED_RUNS", 400)
	}
	if typeName == "speed" && speedPosition != "" && !speedPositions[speedPosition] {
		return nil, newJobError("测速位置无效", "INVALID_SPEED_POSITION", 400)
	}
	if typeName == "speed" && speedOperation != "" && !speedOperations[speedOperation] {
		return nil, newJobError("测速读写范围无效", "INVALID_SPEED_OPERATION", 400)
	}
	if typeName == "speed" && speedPattern != "" && !speedPatterns[speedPattern] {
		return nil, newJobError("测速类型无效", "INVALID_SPEED_PATTERN", 400)
	}
	if typeName == "speed" && options["speedBlockSize"] != nil && !speedBlockSizes[speedBlockSize] {
		return nil, newJobError("测速块大小无效", "INVALID_SPEED_BLOCK_SIZE", 400)
	}
	if typeName == "speed" && options["speedQueueDepth"] != nil && !speedQueueDepths[speedQueueDepth] {
		return nil, newJobError("测速队列深度无效", "INVALID_SPEED_QUEUE_DEPTH", 400)
	}
	if typeName == "speed" && options["speedThreads"] != nil && !speedThreads[speedThreadCount] {
		return nil, newJobError("测速线程数无效", "INVALID_SPEED_THREADS", 400)
	}
	if typeName == "speed" && speedIOEngine != "" && !speedIOEngines[speedIOEngine] {
		return nil, newJobError("测速 I/O 引擎无效", "INVALID_SPEED_IO_ENGINE", 400)
	}
	if typeName == "speed" && speedIOEngine == "psync" && speedQueueDepth > 1 {
		return nil, newJobError("psync 仅支持队列深度 1", "INVALID_SPEED_QUEUE_DEPTH", 400)
	}
	if typeName == "speed" && speedProfileRow != "" && (!speedProfile || !speedProfileRows[speedProfileRow]) {
		return nil, newJobError("测速矩阵行无效", "INVALID_SPEED_PROFILE_ROW", 400)
	}
	if typeName == "speed" && speedTargetMode != "" && !speedTargetModes[speedTargetMode] {
		return nil, newJobError("测速目标类型无效", "INVALID_SPEED_TARGET", 400)
	}
	if typeName == "speed" && speedTargetMode == "" {
		if safeStorageMountPath(devicePath) {
			speedTargetMode = "filesystem"
		} else {
			speedTargetMode = "raw"
		}
	}

	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return nil, newJobError("应用服务已停止，无法创建任务", "SERVICE_STOPPED", 503)
	}
	m.mu.Unlock()

	var disk *Disk
	var space *StorageSpace
	var err error
	if typeName == "speed" && speedTargetMode == "filesystem" {
		space, err = m.resolveSpace(devicePath)
	} else if typeName == "speed" {
		disk, err = m.resolveDisk(devicePath)
	} else {
		disk, err = m.resolveDisk(devicePath)
	}
	if err != nil {
		return nil, err
	}
	candidate := &Job{Type: typeName, Device: devicePath}
	if disk != nil {
		candidate.Device, candidate.Disk.Serial = disk.Path, disk.Serial
	} else if space != nil {
		candidate.Device = space.Path
	}
	m.mu.Lock()
	admissionErr := m.jobAdmissionErrorLocked(candidate)
	m.mu.Unlock()
	if admissionErr != nil {
		return nil, admissionErr
	}
	if scanSelfTest && (typeName != "scan" || disk == nil || !isNativeNVMe(*disk) || m.mock) {
		return nil, newJobError("固件自检仅支持真实 NVMe 命名空间的只读扫描", "CAPABILITY_UNAVAILABLE", 400)
	}
	if typeName == "speed" && speedTargetMode == "filesystem" {
		if space == nil {
			return nil, newJobError("存储空间不存在或未挂载，请刷新后重试", "STORAGE_SPACE_NOT_FOUND", 404)
		}
		if !isLocalStorageMount(space.Source, space.FileSystem) {
			return nil, newJobError("仅支持本地存储空间测速，远程挂载不可用", "STORAGE_SPACE_NOT_LOCAL", 400)
		}
		if space.IsSystem && !systemBenchmarkPath(space.Path) {
			return nil, newJobError("系统盘文件测速需要使用已核对的临时目录", "SYSTEM_SPEED_TARGET_INVALID", 409)
		}
		if space.Size <= 0 {
			return nil, newJobError("存储空间容量信息无效，请刷新后重试", "STORAGE_SPACE_SIZE_INVALID", 409)
		}
		if space.ReadOnly && speedOperation != "read" {
			return nil, newJobError("所选文件系统为只读状态，不能执行包含写入的测速", "STORAGE_SPACE_READ_ONLY", 409)
		}
		// fstrim is supported only for fnOS-managed /volN spaces. External
		// mounts may expose a synthetic or transient path that fstrim cannot
		// stat; never let that optional optimization block the benchmark.
		if !safeStoragePathPattern.MatchString(space.Path) {
			speedTrim = false
		}
	} else {
		if disk == nil {
			return nil, newJobError("磁盘不存在或已离线，请刷新后重试", "DISK_NOT_FOUND", 404)
		}
		if disk.Size <= 0 {
			return nil, newJobError("磁盘容量信息无效，请刷新设备后重试", "DISK_SIZE_INVALID", 409)
		}
	}
	if typeName == "speed" && speedTargetMode == "raw" {
		if reason := rawSpeedBlockedReason(disk); reason != "" {
			return nil, newJobError(reason+"，不能执行底层测速", "RAW_SPEED_NOT_ALLOWED", 409)
		}
		if speedTrim {
			return nil, newJobError("底层测速不支持文件系统 TRIM", "RAW_SPEED_TRIM_UNAVAILABLE", 400)
		}
		if !m.mock && speedOperation != "read" && !rawWriteAcknowledged {
			return nil, newJobError("底层写入测速需要确认将覆盖磁盘数据", "RAW_SPEED_CONFIRMATION_REQUIRED", 409)
		}
	}
	logicalSectorSize := int64(512)
	if disk != nil {
		logicalSectorSize = disk.LogicalSectorSize
	}
	if logicalSectorSize <= 0 {
		logicalSectorSize = 512
	}
	if typeName == "scan" {
		cellCount := scanCellCount
		if scanMode != "full" {
			cellCount = quickScanCellCount
		}
		minimumScanSize := int64(cellCount) * maxInt64(sectorAlignment, logicalSectorSize)
		if logicalSectorSize <= 0 || disk.Size < minimumScanSize || disk.Size%logicalSectorSize != 0 {
			return nil, newJobError("磁盘容量或扇区信息不足以生成有效扫描块图", "DISK_GEOMETRY_INVALID", 409)
		}
		if resumeSource != nil && !scanResumeMatchesDisk(resumeSource, disk) {
			return nil, newJobError("硬盘身份或扇区信息与上次扫描不一致，无法续扫", "DISK_CHANGED", 409)
		}
	}
	if scanRepair {
		if reason := scanRepairBlockedReason(disk); reason != "" {
			return nil, newJobError(reason, "SCAN_REPAIR_NOT_ALLOWED", 409)
		}
		acknowledged, _ := options["scanRepairAcknowledged"].(bool)
		if !acknowledged || expectedDisk == nil || expectedDisk.Path == "" || expectedDisk.Serial == "" || expectedDisk.Model == "" || expectedDisk.Size <= 0 {
			return nil, newJobError("慢扇区修复需要确认写入风险并核对磁盘身份", "SCAN_REPAIR_CONFIRMATION_REQUIRED", 409)
		}
	}
	var repairTargets []RecordedRepairTarget
	if repairFrom != "" {
		if repairSource == nil || !scanResumeMatchesDisk(repairSource, disk) {
			return nil, newJobError("扫描记录与当前硬盘身份或扇区信息不一致", "DISK_CHANGED", 409)
		}
		repairTargets, err = recordedRepairTargets(repairSource, float64(repairAfterMs))
		if err != nil {
			return nil, err
		}
	}
	if typeName != "speed" && expectedDisk != nil {
		if diskIdentityChanged(expectedDisk.Path, expectedDisk.Serial, expectedDisk.Model, expectedDisk.Size, disk) {
			return nil, newJobError("所选磁盘信息已变化，请刷新并重新确认", "DISK_CHANGED", 409)
		}
	}
	if typeName == "speed" && speedTargetMode == "filesystem" && expectedSpace != nil {
		expectedSnapshot := storageSnapshot(expectedSpace)
		if storageIdentityChanged(&expectedSnapshot, space) {
			return nil, newJobError("所选存储空间信息已变化，请刷新并重新确认", "STORAGE_SPACE_CHANGED", 409)
		}
	}
	if typeName == "speed" && speedTargetMode == "raw" && expectedDisk != nil {
		if diskIdentityChanged(expectedDisk.Path, expectedDisk.Serial, expectedDisk.Model, expectedDisk.Size, disk) {
			return nil, newJobError("所选磁盘信息已变化，请刷新并重新确认", "DISK_CHANGED", 409)
		}
	}

	if !scanModes[scanMode] {
		scanMode = "quick"
	}
	if !speedSizes[sampleMiB] {
		sampleMiB = 1024
	}
	if !speedRuns[runs] {
		runs = 1
	}
	if !speedPositions[speedPosition] {
		speedPosition = "all"
	}
	if speedIOEngine == "" {
		speedIOEngine = "io_uring"
	}
	if speedIOEngine == "psync" && speedQueueDepth == 0 {
		speedQueueDepth = 1
	}
	if !directIOProvided {
		speedDirectIO = true
	}
	if speedProfile {
		if !speedBlockSizes[speedBlockSize] {
			speedBlockSize = sequentialBenchmarkSize
		}
		if !speedQueueDepths[speedQueueDepth] {
			if speedIOEngine == "psync" {
				speedQueueDepth = 1
			} else {
				speedQueueDepth = 64
			}
		}
		if !speedThreads[speedThreadCount] {
			speedThreadCount = 1
		}
	}
	writeAllowed := typeName == "speed"
	if typeName == "speed" {
		if !m.mock && m.findExecutable("fio") == "" {
			return nil, newJobError("系统未找到 fio，无法执行磁盘测速", "CAPABILITY_UNAVAILABLE", 503)
		}
		if speedTrim && !m.mock && m.findExecutable("fstrim") == "" {
			return nil, newJobError("系统未找到 fstrim，无法在测速前执行 TRIM", "CAPABILITY_UNAVAILABLE", 503)
		}
		if len(selectSpeedBenchmarks(speedOperation, speedPattern, speedBlockSize, speedQueueDepth, speedThreadCount, writeAllowed, speedProfile, speedProfileRow)) == 0 {
			return nil, newJobError("所选测速组合不存在", "INVALID_SPEED_COMBINATION", 400)
		}
		fileSize := sampleMiB * 1024 * 1024
		if speedTargetMode == "filesystem" {
			if err := benchmarkSpaceCapacityError(space, fileSize); err != nil {
				return nil, err
			}
		}
		targetSize := fileSize
		if speedTargetMode == "raw" {
			targetSize = disk.Size
			if fileSize > targetSize {
				return nil, newJobError("磁盘容量小于所选测速样本", "DISK_TOO_SMALL", 400)
			}
		}
		if _, err := createSpeedPlan(targetSize, sampleMiB, speedPosition); err != nil {
			return nil, err
		}
	}

	now := isoTime(m.now())
	var result interface{}
	if typeName == "scan" {
		coverage := "full"
		sampleBytes := (*int64)(nil)
		cellCount := scanCellCount
		if scanMode == "quick" {
			coverage = "sampled"
			cellCount = quickScanCellCount
			v := int64(fastSampleSize)
			sampleBytes = &v
		}
		result = &ScanResult{
			Version:      2,
			LatencyBasis: scanLatencyBasis(disk.Rotational),
			ScanMode:     scanMode,
			Coverage:     coverage,
			SampleBytes:  sampleBytes,
			Map:          createScanMap(disk.Size, cellCount, logicalSectorSize),
			Stats:        ScanStats{BadRanges: []BadRange{}, IssuesComplete: true},
		}
		if scanMode == "quick" {
			r := result.(*ScanResult)
			r.SamplingPlan, r.SamplingSeed = stratifiedScanPlan, newScanSamplingSeed()
		}
		if repairFrom != "" {
			r := result.(*ScanResult)
			r.ScanMode, r.Coverage, r.SampleBytes, r.SamplingPlan = "repair", "targeted", nil, ""
			r.RepairTargets = repairTargets
			r.Map = make([]ScanCell, len(repairTargets))
			for i, target := range repairTargets {
				r.Map[i] = ScanCell{Index: i, Start: target.Read.Offset, End: target.Read.Offset + int64(target.Read.Bytes), Status: "pending"}
			}
		}
	}
	phase := ""
	if typeName == "scan" {
		if scanMode == "quick" {
			phase = "准备快速抽样扫描"
		} else {
			phase = "准备完整慢速扫描"
		}
	} else {
		phase = "准备一键测速"
	}
	if repairFrom != "" {
		phase = "准备按扫描记录修复慢区"
	}

	optionsField := JobOptions{}
	if typeName == "speed" {
		optionsField.SampleMiB = intPtr(sampleMiB)
		optionsField.SpeedRuns = intPtr(runs)
		optionsField.SpeedPosition = strPtr(speedPosition)
		optionsField.SpeedIOEngine = strPtr(speedIOEngine)
		optionsField.SpeedDirectIO = boolPtr(speedDirectIO)
		optionsField.SpeedTrim = boolPtr(speedTrim)
		optionsField.SpeedProfile = boolPtr(speedProfile)
		if speedProfileRow != "" {
			optionsField.SpeedProfileRow = strPtr(speedProfileRow)
		}
		optionsField.SpeedTarget = strPtr(speedTargetMode)
		if speedOperation != "" {
			optionsField.SpeedOperation = strPtr(speedOperation)
		}
		if speedPattern != "" {
			optionsField.SpeedPattern = strPtr(speedPattern)
		}
		if speedBlockSize != 0 {
			optionsField.SpeedBlockSize = intPtr(speedBlockSize)
		}
		if speedQueueDepth != 0 {
			optionsField.SpeedQueueDepth = intPtr(speedQueueDepth)
		}
		if speedThreadCount != 0 {
			optionsField.SpeedThreads = intPtr(speedThreadCount)
		}
	} else if typeName == "scan" {
		optionsField.ScanRepairFrom = repairFrom
		optionsField.ScanMode = strPtr(scanMode)
		optionsField.ScanExactSectors = boolPtr(scanExactSectors)
		optionsField.ScanDeepLocate = boolPtr(scanDeepLocate)
		optionsField.ScanSelfTest = boolPtr(scanSelfTest)
		optionsField.ScanRepair = boolPtr(scanRepair)
		if scanRepair {
			optionsField.ScanRepairAfterMs = intPtr(repairAfterMs)
		}
	}

	device := ""
	diskSnapshot := DiskSnapshot{}
	var spaceSnapshot *StorageSpaceSnapshot
	if typeName == "speed" && speedTargetMode == "filesystem" {
		device = space.Path
		snapshot := storageSnapshot(space)
		spaceSnapshot = &snapshot
		diskSnapshot = DiskSnapshot{
			Name: path.Base(space.Path), Model: strings.ToUpper(space.FileSystem), Size: space.Size,
			LogicalSectorSize: 4096, PhysicalSectorSize: 4096,
			IsSystem: space.IsSystem,
		}
	} else {
		device = disk.Path
		diskSnapshot = DiskSnapshot{
			Name: disk.Name, Model: disk.Model, Serial: disk.Serial, Size: disk.Size,
			LogicalSectorSize: logicalSectorSize, PhysicalSectorSize: disk.PhysicalSectorSize,
			Rotational: disk.Rotational, IsSystem: disk.IsSystem,
		}
	}

	job := &Job{
		ID:           newUUID(),
		Type:         typeName,
		Device:       device,
		Disk:         diskSnapshot,
		StorageSpace: spaceSnapshot,
		Status:       "queued",
		Progress:     0,
		Phase:        phase,
		CreatedAt:    now,
		Message:      "",
		Result:       result,
		Log:          []string{},
		Options:      optionsField,
	}
	if job.Disk.PhysicalSectorSize <= 0 {
		job.Disk.PhysicalSectorSize = logicalSectorSize
	}
	if resumeSource != nil {
		if err := m.prepareResumedScan(job, resumeSource); err != nil {
			m.removeScanIssueFile(job.ID)
			return nil, err
		}
	}

	m.mu.Lock()
	// Admit jobs atomically after resolving their real device identity. Separate
	// disks can scan together; a cancelled request owns its disk until I/O settles.
	if err := m.jobAdmissionErrorLocked(job); err != nil {
		m.mu.Unlock()
		m.removeScanIssueFile(job.ID)
		return nil, err
	}
	// The source may have been deleted while resolving the current disk.
	if (resumeSource != nil && m.jobs[resumeID] == nil) || (repairSource != nil && m.jobs[repairFrom] == nil) {
		m.mu.Unlock()
		m.removeScanIssueFile(job.ID)
		return nil, newJobError("原扫描记录已删除，请刷新后重试", "SCAN_RESUME_UNAVAILABLE", 409)
	}
	job.HistoryOrder = m.nextHistoryOrder
	m.nextHistoryOrder++
	m.jobs[job.ID] = job
	if err := m.persist(job); err != nil {
		delete(m.jobs, job.ID)
		delete(m.lastPersisted, job.ID)
		m.mu.Unlock()
		m.removeScanIssueFile(job.ID)
		return nil, newJobError("无法保存任务状态，请检查应用数据目录", "PERSISTENCE_UNAVAILABLE", 500)
	}
	m.pruneHistoryLocked()
	responseJob := cloneJobLocked(job)
	m.mu.Unlock()

	go func() {
		m.mu.Lock()
		queued := job.Status == "queued" && !m.stopped
		m.mu.Unlock()
		if !queued {
			return
		}
		var runErr error
		if repairFrom != "" {
			runErr = m.runRecordedRepair(job)
		} else if m.mock {
			if typeName == "scan" {
				runErr = m.runMockScan(job)
			} else {
				runErr = m.runMockSpeed(job, sampleMiB, runs, speedPosition, speedOperation, speedPattern, speedBlockSize, speedQueueDepth, speedThreadCount, writeAllowed, speedProfile, speedProfileRow)
			}
		} else {
			if typeName == "scan" {
				runErr = m.runScan(job, scanMode)
			} else {
				runErr = m.runSpeed(job, sampleMiB, runs, speedPosition, speedOperation, speedPattern, speedBlockSize, speedQueueDepth, speedThreadCount, writeAllowed, speedProfile, speedProfileRow, speedIOEngine, speedDirectIO, speedTrim)
			}
		}
		if runErr != nil {
			m.fail(job, runErr)
		}
	}()

	return responseJob, nil
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func diskFromMap(raw map[string]interface{}) *Disk {
	disk := &Disk{}
	if v, ok := raw["path"].(string); ok {
		disk.Path = v
	}
	if v, ok := raw["serial"].(string); ok {
		disk.Serial = v
	}
	if v, ok := raw["model"].(string); ok {
		disk.Model = v
	}
	if v, ok := toInt64(raw["size"]); ok {
		disk.Size = v
	}
	return disk
}

func storageSpaceFromMap(raw map[string]interface{}) *StorageSpace {
	space := &StorageSpace{}
	if v, ok := raw["path"].(string); ok {
		space.Path = v
	}
	if v, ok := raw["source"].(string); ok {
		space.Source = v
	}
	if v, ok := raw["fileSystem"].(string); ok {
		space.FileSystem = v
	}
	if v, ok := raw["layout"].(string); ok {
		space.Layout = v
	}
	if values, ok := raw["members"].([]interface{}); ok {
		for _, value := range values {
			if member, isString := value.(string); isString {
				space.Members = append(space.Members, member)
			}
		}
	} else if values, ok := raw["members"].([]string); ok {
		space.Members = append(space.Members, values...)
	}
	if v, ok := toInt64(raw["size"]); ok {
		space.Size = v
	}
	if v, ok := raw["readOnly"].(bool); ok {
		space.ReadOnly = v
	}
	if v, ok := raw["isSystem"].(bool); ok {
		space.IsSystem = v
	}
	return space
}

func storageSnapshot(space *StorageSpace) StorageSpaceSnapshot {
	if space == nil {
		return StorageSpaceSnapshot{}
	}
	return StorageSpaceSnapshot{
		Path: space.Path, Source: space.Source, FileSystem: space.FileSystem, Layout: space.Layout,
		Members: append([]string(nil), space.Members...), Size: space.Size, ReadOnly: space.ReadOnly, IsSystem: space.IsSystem,
	}
}

// awaitOpen opens the device while honoring cancellation, and closes any
// handle that arrives after cancellation so the descriptor is not leaked.
func (m *JobManager) awaitOpen(job *Job) (ReadHandle, error) {
	m.mu.Lock()
	cancel, hasCancel := m.cancels[job.ID]
	m.mu.Unlock()
	if !hasCancel {
		return m.openFn(job.Device)
	}
	select {
	case <-cancel.ctx.Done():
		return nil, errCancelled
	default:
	}
	type result struct {
		handle ReadHandle
		err    error
	}
	ch := make(chan result, 1)
	cancel.pending.Add(1)
	go func() {
		defer cancel.pending.Done()
		h, err := m.openFn(job.Device)
		ch <- result{handle: h, err: err}
	}()
	select {
	case <-cancel.ctx.Done():
		cancel.pending.Add(1)
		go func() {
			defer cancel.pending.Done()
			r := <-ch
			if r.handle != nil {
				m.closeHandle(r.handle)
			}
		}()
		return nil, errCancelled
	case r := <-ch:
		return r.handle, r.err
	}
}

func scanCompletionMessage(label string, errors, slow int64) string {
	if errors > 0 {
		return fmt.Sprintf("%s发现 %d 个异常区域", label, errors)
	}
	if slow > 0 {
		return fmt.Sprintf("%s未发现读取错误，%d 个区域响应偏慢", label, slow)
	}
	return label + "未发现读取错误"
}

func (m *JobManager) runScan(job *Job, scanMode string) (runErr error) {
	if err := m.start(job); err != nil {
		return err
	}
	cancel := m.beginCancellation(job)
	var handle ReadHandle
	var repairHandle RepairHandle
	var completionMessage string
	defer func() {
		m.mu.Lock()
		if job.Status == "cancelling" {
			job.Phase = "正在停止，等待设备请求结束"
		} else if errors.Is(runErr, errScanWaitTimeout) {
			job.Phase = "读取超时，等待设备请求结束"
		}
		m.mu.Unlock()
		if handle != nil {
			m.closeHandle(handle)
		}
		if repairHandle != nil {
			m.closeHandle(repairHandle)
		}
		m.endCancellation(job, cancel)
		m.mu.Lock()
		if handle != nil && m.handles[job.ID] == handle {
			delete(m.handles, job.ID)
		}
		delete(m.handles, job.ID+"/repair")
		m.mu.Unlock()
		if runErr == nil && completionMessage != "" {
			m.complete(job, job.Result, completionMessage)
		}
	}()

	currentDisk, err := m.assertDiskIdentity(job)
	if err != nil {
		return err
	}
	if err := m.checkCancelled(job); err != nil {
		return err
	}
	if scanRepairThreshold(job) > 0 {
		if reason := scanRepairBlockedReason(currentDisk); reason != "" {
			return newJobError(reason, "SCAN_REPAIR_NOT_ALLOWED", 409)
		}
		repairHandle, err = m.awaitOpenRepair(job)
		if err != nil {
			return err
		}
		m.mu.Lock()
		m.handles[job.ID+"/repair"] = repairHandle
		m.mu.Unlock()
	}
	if job.Disk.Rotational {
		m.mu.Lock()
		job.Phase = "正在唤醒机械硬盘"
		m.mu.Unlock()
		_, wakeErr := awaitCancelable(m, job, func() (struct{}, error) {
			return struct{}{}, m.wakeDiskFn(*currentDisk)
		})
		if wakeErr == errCancelled || m.checkCancelled(job) != nil {
			return errCancelled
		}
		// SMART may be unavailable behind some controllers. The scan remains
		// the fallback and will still report any actual read errors.
	}
	opened, err := m.awaitOpen(job)
	if err != nil {
		return err
	}
	handle = configureScanReader(opened, *currentDisk, job.Disk)
	m.mu.Lock()
	m.handles[job.ID] = handle
	m.mu.Unlock()

	result := job.Result.(*ScanResult)
	badRanges := append([]BadRange{}, job.Result.(*ScanResult).Stats.BadRanges...)
	latencyReference := scanLatencyReference{}
	loadMonitor := m.newScanLoadMonitor(job.Device)
	m.mu.Lock()
	result.Version = 2
	result.Stats.IssuesComplete = true
	result.LatencyBasis = scanLatencyBasis(job.Disk.Rotational)
	job.Phase = "读取扫描前 SMART 与错误日志"
	m.mu.Unlock()
	if result.Diagnostics == nil || result.Diagnostics.Before == nil {
		if err := m.collectScanEvidence(job, *currentDisk, false); err != nil {
			return err
		}
	}
	if ready, ok := handle.(interface{ prepare() error }); ok {
		if _, err := awaitCancelable(m, job, func() (struct{}, error) { return struct{}{}, ready.prepare() }); err != nil {
			return err
		}
	}
	m.mu.Lock()
	updateScanReadInfo(result, handle)
	m.mu.Unlock()
	if job.Options.ScanSelfTest != nil && *job.Options.ScanSelfTest && (result.Diagnostics == nil || result.Diagnostics.SelfTest == nil || result.Diagnostics.SelfTest.State != "passed") {
		if err := m.runNVMeSelfTest(job, handle); err != nil {
			return err
		}
	}
	if _, err := m.assertDiskIdentity(job); err != nil {
		return err
	}
	if err := m.checkCancelled(job); err != nil {
		return err
	}

	logicalSectorSize := job.Disk.LogicalSectorSize
	if logicalSectorSize <= 0 {
		logicalSectorSize = 512
	}
	errorProbeLength := int64(errorProbeSize)
	if job.Options.ScanExactSectors != nil && *job.Options.ScanExactSectors {
		errorProbeLength = logicalSectorSize
	}
	maxSampleLength := alignDown(fastSampleSize, logicalSectorSize)
	if maxSampleLength <= 0 {
		maxSampleLength = logicalSectorSize
	}

	if scanMode == "quick" {
		buffer := scanBuffer(int(maxSampleLength))
		for index := 0; index < len(result.Map); index++ {
			cell := &result.Map[index]
			if scanCellComplete(result, cell) {
				continue
			}
			budget := newScanSlowBudgetReader(handle, job.Disk.Rotational)
			budget.now = m.nowFn
			m.mu.Lock()
			ranges := setScanSamples(result, cell, job.Disk.Size, logicalSectorSize, maxSampleLength)
			m.mu.Unlock()
			for sampleIndex, sample := range ranges {
				if err := m.checkCancelled(job); err != nil {
					return err
				}
				position, length := sample.Start, sample.End-sample.Start
				m.mu.Lock()
				before := *cell
				cell.Status = "reading"
				job.Phase = fmt.Sprintf("快速抽样 %d/%d · %d/%d", index+1, len(result.Map), sampleIndex+1, len(ranges))
				m.mu.Unlock()
				regionResult, warning, ratio, recheck, err := m.inspectScanRegion(job, cell, budget, repairHandle, buffer, position, int(length), errorProbeLength, latencyReference, loadMonitor)
				if err != nil {
					m.recordScanReadFailure(job, cell, regionResult, length, badRanges, handle)
					return err
				}
				m.mu.Lock()
				length = finishSlowLimitedRegion(result, cell, &regionResult, position, length)
				appendBadRanges(&badRanges, regionResult.badRanges, 200)
				m.updateCell(cell, regionResult.milliseconds, int64(regionResult.bytesRead), int64(regionResult.errorCount), length)
				cell.CoveredBytes += length - sumUnresolvedProbeBytes(regionResult.badRanges)
				recordErr := m.recordScanIssuesLocked(job, cell, regionResult.badRanges)
				if recheck != nil {
					cell.Recheck = mergeScanRecheck(cell.Recheck, recheck)
				}
				applyHDDLatency(cell, warning, ratio)
				applyScanReadLatency(cell, regionResult.maxReadLatency, job.Disk.Rotational)
				if recheck != nil {
					applyScanReadLatency(cell, recheck.MaxReadLatency, job.Disk.Rotational)
				}
				if regionResult.slowLimited && cell.ErrorCount == 0 {
					cell.Status = "severe"
				}
				if sampleIndex+1 < len(ranges) && !regionResult.slowLimited {
					cell.Status = "reading"
				}
				updateScanReadInfo(result, handle)
				job.Progress = round1(float64(index) / float64(len(result.Map)) * 100)
				if sampleIndex+1 == len(ranges) || regionResult.slowLimited {
					job.Progress = round1(float64(index+1) / float64(len(result.Map)) * 100)
				}
				m.updateScanCellStats(job, before, cell)
				result.Stats.BadRanges = append([]BadRange{}, badRanges...)
				m.mu.Unlock()
				if recordErr != nil {
					return recordErr
				}
				m.persistThrottled(job, false)
				if regionResult.slowLimited {
					break
				}
			}
		}
	} else {
		buffer := scanBuffer(fullScanReadSize)
		for index := 0; index < len(result.Map); index++ {
			cell := &result.Map[index]
			if scanCellComplete(result, cell) {
				continue
			}
			budget := newScanSlowBudgetReader(handle, job.Disk.Rotational)
			budget.now = m.nowFn
			position := cell.Start
			for position < cell.End {
				if err := m.checkCancelled(job); err != nil {
					return err
				}
				m.mu.Lock()
				before := *cell
				cell.Status = "reading"
				length := cell.End - position
				if length > fullScanReadSize {
					length = fullScanReadSize
				}
				job.Phase = fmt.Sprintf("完整读取 %d/%d", index+1, len(result.Map))
				m.mu.Unlock()

				regionResult, warning, ratio, recheck, err := m.inspectScanRegion(job, cell, budget, repairHandle, buffer, position, int(length), errorProbeLength, latencyReference, loadMonitor)
				if err != nil {
					m.recordScanReadFailure(job, cell, regionResult, length, badRanges, handle)
					return err
				}

				m.mu.Lock()
				length = finishSlowLimitedRegion(result, cell, &regionResult, position, length)
				if recheck != nil {
					cell.Recheck = mergeScanRecheck(cell.Recheck, recheck)
				}
				appendBadRanges(&badRanges, regionResult.badRanges, 200)
				m.updateCell(cell, regionResult.milliseconds, int64(regionResult.bytesRead), int64(regionResult.errorCount), length)
				cell.CoveredBytes += length - sumUnresolvedProbeBytes(regionResult.badRanges)
				recordErr := m.recordScanIssuesLocked(job, cell, regionResult.badRanges)
				applyHDDLatency(cell, warning, ratio)
				applyScanReadLatency(cell, regionResult.maxReadLatency, job.Disk.Rotational)
				if recheck != nil {
					applyScanReadLatency(cell, recheck.MaxReadLatency, job.Disk.Rotational)
				}
				if regionResult.slowLimited && cell.ErrorCount == 0 {
					cell.Status = "severe"
				}
				updateScanReadInfo(result, handle)
				position += length
				if position < cell.End {
					cell.Status = "reading"
				}
				job.Progress = round1(float64(position) / float64(job.Disk.Size) * 100)
				m.updateScanCellStats(job, before, cell)
				result.Stats.BadRanges = append([]BadRange{}, badRanges...)
				m.mu.Unlock()
				if recordErr != nil {
					return recordErr
				}

				m.persistThrottled(job, false)
			}
		}
	}

	if err := m.checkCancelled(job); err != nil {
		return err
	}
	currentDisk, err = m.assertDiskIdentity(job)
	if err != nil {
		return err
	}
	m.mu.Lock()
	job.Phase = "核对扫描后 SMART 与错误日志"
	m.mu.Unlock()
	if err := m.collectScanEvidence(job, *currentDisk, true); err != nil {
		return err
	}
	m.mu.Lock()
	m.updateScanStats(job)
	errorCells := result.Stats.ErrorCells
	slowCells := result.Stats.SlowCells
	m.mu.Unlock()

	modeLabel := "快速抽样"
	if scanMode == "full" {
		modeLabel = "完整扫描"
	}
	message := scanOutcomeCompletionMessage(modeLabel, result, errorCells, slowCells, job.Disk.Rotational)
	if scanRepairThreshold(job) > 0 {
		r := result.Stats.Repair
		message += fmt.Sprintf("；慢区写回：%d 个写回校验通过，%d 个慢速改善，%d 个修复后仍慢，%d 个跳过，%d 个失败", r.Verified+r.StillSlow, r.Improved, r.StillSlow, r.Skipped, r.Failed)
	}
	if result.Diagnostics != nil && result.Diagnostics.SelfTest != nil && result.Diagnostics.SelfTest.State != "passed" {
		message += "；固件自检另有异常，请查看详情"
	}
	if d := result.Diagnostics; d != nil && d.After != nil {
		if len(d.Increased) > 0 {
			message += "；扫描期间 SMART／日志计数增长，请查看详情"
		} else if len(d.After.Warnings) > 0 || (d.Before != nil && len(d.Before.Warnings) > 0) {
			message += "；SMART／硬盘日志另有异常记录"
		} else if !d.After.Available {
			message += "；SMART 不可用，无法评估总体健康"
		}
	}
	completionMessage = message
	return nil
}

func (m *JobManager) runMockScan(job *Job) error {
	if err := m.start(job); err != nil {
		return err
	}
	result := job.Result.(*ScanResult)
	mockEvidence := &ScanEvidence{Status: "unavailable", Note: "本地演示未查询真实硬盘 SMART 或错误日志"}
	m.mu.Lock()
	result.Version = 2
	result.Stats.IssuesComplete = true
	result.Diagnostics = &ScanDiagnostics{ReadMethod: "mock", Before: mockEvidence}
	m.mu.Unlock()
	mode := "quick"
	if job.Options.ScanMode != nil {
		mode = *job.Options.ScanMode
	}
	full := mode == "full"
	logicalSectorSize := job.Disk.LogicalSectorSize
	if logicalSectorSize <= 0 {
		logicalSectorSize = 512
	}
	maxSampleLength := alignDown(fastSampleSize, logicalSectorSize)
	if maxSampleLength <= 0 {
		maxSampleLength = logicalSectorSize
	}

	for index := 0; index < len(result.Map); index++ {
		if err := m.checkCancelled(job); err != nil {
			return err
		}
		cell := &result.Map[index]
		if scanCellComplete(result, cell) {
			continue
		}

		m.mu.Lock()
		cell.Status = "reading"
		regionLength := cell.End - cell.Start
		if regionLength < sectorAlignment {
			regionLength = sectorAlignment
		}
		var bytesRead int64
		if full {
			bytesRead = cell.End - cell.Start
		} else {
			ranges := setScanSamples(result, cell, job.Disk.Size, logicalSectorSize, maxSampleLength)
			bytesRead = sampleRangeBytes(ranges)
		}
		job.Phase = fmt.Sprintf("%s %d/%d", map[bool]string{true: "完整读取", false: "快速抽样"}[full], index+1, len(result.Map))

		milliseconds := float64(2 + ((index * 17) % 16))
		if index == 118 || index == 119 || index == 277 {
			milliseconds = float64(74 + (index % 20))
		}
		if index == 376 || index == 377 {
			milliseconds = float64(238 + (index % 50))
		}
		if index == 441 {
			milliseconds = 690
		}
		var errorCount int64
		if index == 468 {
			errorCount = 1
		}
		timingBytes := bytesRead
		if full && timingBytes > fullScanReadSize {
			timingBytes = fullScanReadSize
		}
		m.updateCell(cell, milliseconds, bytesRead, errorCount, timingBytes)
		// Demo request timing is synthetic, like all other mock scan data.
		requestMs := milliseconds / math.Max(1, float64(timingBytes)/scanLatencyRequestBytes)
		if index == 376 || index == 377 || index == 441 {
			requestMs = milliseconds
		}
		cell.CoveredBytes += bytesRead
		if threshold := scanRepairThreshold(job); threshold > 0 && requestMs > threshold && errorCount == 0 {
			offset := cell.Start
			if cell.SampleStart != nil {
				offset = *cell.SampleStart
			}
			r := BadRange{Offset: offset, Length: min(bytesRead, scanLatencyRequestBytes), Outcome: "rewrite", Code: "SLOW_REWRITE_SKIPPED", Repair: &ScanRepairRecord{Outcome: "skipped", BeforeMs: requestMs, Detail: "演示模式不执行实际写回"}}
			if err := m.recordScanIssuesLocked(job, cell, []BadRange{r}); err != nil {
				m.mu.Unlock()
				return err
			}
		}
		requestOffset := cell.Start
		if cell.SampleStart != nil {
			requestOffset = *cell.SampleStart
		}
		applyScanReadLatency(cell, &ScanReadLatency{Ms: requestMs, Bytes: int(min(timingBytes, scanLatencyRequestBytes)), Offset: requestOffset}, job.Disk.Rotational)
		if full && errorCount == 0 && scanUnitLatency(milliseconds, timingBytes) >= 50 {
			cell.Recheck = &ScanRecheck{Outcome: "slow", Reads: 8, BytesRead: timingBytes, MaxUnitMs: scanUnitLatency(milliseconds, timingBytes)}
		}
		_, _, severeThreshold := scanReadThresholds(job.Disk.Rotational)
		if errorCount == 0 && requestMs >= severeThreshold {
			// Synthetic persistent-slow evidence for the demo only.
			cell.SlowBadBlock = &ScanSlowBlock{Offset: requestOffset, Bytes: int(min(timingBytes, scanLatencyRequestBytes)), FirstMs: requestMs, RepeatMs: requestMs, Source: "recheck", Load: &ScanLoad{}}
		}
		if full {
			reads := int64(math.Ceil(float64(bytesRead) / float64(fullScanReadSize)))
			if reads < 1 {
				reads = 1
			}
			cell.Reads = reads
			cell.TotalMs = floatPtr(milliseconds * float64(reads))
			cell.AvgMs = floatPtr(milliseconds)
		}
		if errorCount > 0 {
			length := int64(errorProbeSize)
			if job.Options.ScanExactSectors != nil && *job.Options.ScanExactSectors {
				length = logicalSectorSize
			}
			if length > bytesRead {
				length = bytesRead
			}
			rangeStart := cell.Start
			if cell.SampleStart != nil {
				rangeStart = *cell.SampleStart
			}
			r := BadRange{Offset: rangeStart, Length: length, Code: "EIO", Recovered: boolPtr(false), Outcome: "unreadable", ProbeBytes: logicalSectorSize}
			appendBadRanges(&result.Stats.BadRanges, []BadRange{r}, scanIssuePreviewLimit)
			if err := m.recordScanIssuesLocked(job, cell, []BadRange{r}); err != nil {
				m.mu.Unlock()
				return err
			}
			cell.BytesRead -= length
		}
		if cell.ReadOutcome == "" {
			cell.ReadOutcome = "readable"
		}
		job.Progress = round1(float64(index+1) / float64(len(result.Map)) * 100)
		m.updateScanStats(job)
		m.mu.Unlock()

		m.persistThrottled(job, false)
		if index%4 == 0 {
			d := 12 * time.Millisecond
			if full {
				d = 24 * time.Millisecond
			}
			m.delayFn(d)
		}
	}

	m.mu.Lock()
	errorCells := result.Stats.ErrorCells
	m.mu.Unlock()

	modeLabel := "快速抽样"
	if full {
		modeLabel = "完整扫描"
	}
	message := scanOutcomeCompletionMessage("演示"+modeLabel, result, errorCells, result.Stats.SlowCells, job.Disk.Rotational)
	m.mu.Lock()
	result.Diagnostics = &ScanDiagnostics{ReadMethod: "mock", Before: mockEvidence, After: mockEvidence}
	m.mu.Unlock()
	m.complete(job, result, message)
	return nil
}

func formatBlockSize(blockSize int64) string {
	switch blockSize {
	case 4096:
		return "4 KiB"
	case 65536:
		return "64 KiB"
	case 1048576:
		return "1 MiB"
	case 4194304:
		return "4 MiB"
	}
	return fmt.Sprintf("%d B", blockSize)
}

func benchmarkOperationLabel(operation string) string {
	switch operation {
	case "write":
		return "写入"
	case "mixed":
		return "混合读写"
	default:
		return "读取"
	}
}

func selectBenchmarks(speedOperation, speedPattern string, speedBlockSize, speedQueueDepth, speedThreadCount int64, writeAllowed bool) []benchmarkSpec {
	filtered := make([]benchmarkSpec, 0, len(standardBenchmarks))
	for _, benchmark := range standardBenchmarks {
		if (speedOperation != "" && benchmark.Operation != speedOperation) ||
			(!writeAllowed && benchmark.Operation != "read") ||
			(speedPattern != "" && benchmark.Pattern != speedPattern) ||
			(speedBlockSize != 0 && benchmark.BlockSize != speedBlockSize) ||
			(speedQueueDepth != 0 && benchmark.QueueDepth != speedQueueDepth) ||
			(speedThreadCount != 0 && benchmark.Threads != speedThreadCount) {
			continue
		}
		filtered = append(filtered, benchmark)
	}
	return filtered
}

func selectSpeedBenchmarks(speedOperation, speedPattern string, speedBlockSize, speedQueueDepth, speedThreadCount int64, writeAllowed, profile bool, profileRow string) []benchmarkSpec {
	if !profile && speedPattern != "" && speedBlockSize != 0 && speedQueueDepth != 0 && speedThreadCount != 0 {
		operations := []string{"read", "write", "mixed"}
		var benchmarks []benchmarkSpec
		for _, operation := range operations {
			if (speedOperation != "" && speedOperation != operation) || (!writeAllowed && operation != "read") {
				continue
			}
			benchmarks = append(benchmarks, benchmarkSpec{
				Operation: operation, Pattern: speedPattern, BlockSize: speedBlockSize,
				QueueDepth: speedQueueDepth, Threads: speedThreadCount,
			})
		}
		return benchmarks
	}
	if !profile {
		return selectBenchmarks(speedOperation, speedPattern, speedBlockSize, speedQueueDepth, speedThreadCount, writeAllowed)
	}

	rows := []benchmarkSpec{
		{Pattern: "sequential", BlockSize: speedBlockSize, QueueDepth: speedQueueDepth, Threads: speedThreadCount, ProfileRow: "seq-high"},
		{Pattern: "sequential", BlockSize: speedBlockSize, QueueDepth: 1, Threads: 1, ProfileRow: "seq-low"},
		{Pattern: "random", BlockSize: randomBenchmarkSize, QueueDepth: speedQueueDepth, Threads: speedThreadCount, ProfileRow: "rnd-high"},
		{Pattern: "random", BlockSize: randomBenchmarkSize, QueueDepth: 1, Threads: 1, ProfileRow: "rnd-low"},
	}
	operations := []string{"read", "write", "mixed"}
	benchmarks := make([]benchmarkSpec, 0, len(rows)*len(operations))
	for _, row := range rows {
		if (speedPattern != "" && speedPattern != row.Pattern) || (profileRow != "" && profileRow != row.ProfileRow) {
			continue
		}
		for _, operation := range operations {
			if (speedOperation != "" && speedOperation != operation) || (!writeAllowed && operation != "read") {
				continue
			}
			spec := row
			spec.Operation = operation
			benchmarks = append(benchmarks, spec)
		}
	}
	return benchmarks
}

func combineBenchmarkRuns(spec benchmarkSpec, results []BenchmarkResult) BenchmarkResult {
	if len(results) == 1 {
		return results[0]
	}
	combined := BenchmarkResult{
		Operation:  spec.Operation,
		Pattern:    spec.Pattern,
		BlockSize:  spec.BlockSize,
		QueueDepth: spec.QueueDepth,
		Threads:    spec.Threads,
	}
	for sampleIndex, base := range results[0].Samples {
		var totalSeconds float64
		for _, result := range results {
			if sampleIndex >= len(result.Samples) {
				continue
			}
			sample := result.Samples[sampleIndex]
			totalSeconds += sample.Seconds
		}
		averageSeconds := totalSeconds / float64(len(results))
		mbps := round1(float64(base.Bytes) / averageSeconds / 1e6)
		combined.Samples = append(combined.Samples, SpeedSample{
			Label:       base.Label,
			OffsetBytes: base.OffsetBytes,
			Bytes:       base.Bytes,
			Seconds:     averageSeconds,
			MBps:        mbps,
		})
	}
	var bytes int64
	var seconds float64
	var speeds []float64
	for _, sample := range combined.Samples {
		bytes += sample.Bytes
		seconds += sample.Seconds
		speeds = append(speeds, sample.MBps)
	}
	combined.AverageMBps = round1(float64(bytes) / seconds / 1e6)
	combined.MinimumMBps = minFloat(speeds)
	combined.MaximumMBps = maxFloat(speeds)
	combined.IOPS = calculateBenchmarkIOPS(bytes, spec.BlockSize, seconds)
	return combined
}

func calculateBenchmarkIOPS(bytes, blockSize int64, seconds float64) *float64 {
	if bytes <= 0 || blockSize <= 0 || seconds <= 0 {
		return nil
	}
	iops := round1(float64(bytes) / float64(blockSize) / seconds)
	return &iops
}

func (m *JobManager) runMockSpeed(job *Job, sampleMiB, runs int64, speedPosition, speedOperation, speedPattern string, speedBlockSize, speedQueueDepth, speedThreadCount int64, writeAllowed, speedProfile bool, speedProfileRow string) error {
	if err := m.start(job); err != nil {
		return err
	}
	specs := selectSpeedBenchmarks(speedOperation, speedPattern, speedBlockSize, speedQueueDepth, speedThreadCount, writeAllowed, speedProfile, speedProfileRow)
	var benchmarks []BenchmarkResult
	for i, spec := range specs {
		var runResults []BenchmarkResult
		for run := int64(0); run < runs; run++ {
			benchmark, err := m.mockBenchmark(job, spec, sampleMiB, speedPosition)
			if err != nil {
				return err
			}
			runResults = append(runResults, benchmark)
			m.mu.Lock()
			job.Progress = float64(int64(i)*runs+run+1) / float64(int64(len(specs))*runs) * 100
			m.mu.Unlock()
		}
		benchmark := combineBenchmarkRuns(spec, runResults)
		benchmarks = append(benchmarks, benchmark)
		m.mu.Lock()
		job.Result = &speedInProgress{Benchmarks: benchmarks}
		m.mu.Unlock()
		m.persistSafe(job, "保存任务状态")
	}
	result := &SpeedResult{SampleMiB: sampleMiB, Runs: runs, Benchmarks: benchmarks}
	m.complete(job, result, "一键测速完成（演示）")
	return nil
}

func (m *JobManager) mockBenchmark(job *Job, spec benchmarkSpec, sampleMiB int64, speedPosition string) (BenchmarkResult, error) {
	write := spec.Operation == "write"
	verb := benchmarkOperationLabel(spec.Operation)
	var samples []SpeedSample
	if spec.Pattern == "random" {
		m.mu.Lock()
		job.Phase = fmt.Sprintf("随机%s（%s Q%dT%d）", verb, formatBlockSize(spec.BlockSize), spec.QueueDepth, spec.Threads)
		m.mu.Unlock()
		m.delayFn(600 * time.Millisecond)
		if err := m.checkCancelled(job); err != nil {
			return BenchmarkResult{}, err
		}
		mbps := 42.0
		if spec.QueueDepth == 32 {
			mbps = 168.0
		}
		if write {
			mbps = 38.0
			if spec.QueueDepth == 32 {
				mbps = 144.0
			}
		} else if spec.Operation == "mixed" {
			mbps = 40.0
			if spec.QueueDepth == 32 {
				mbps = 156.0
			}
		}
		bytes := sampleMiB * 1024 * 1024
		seconds := float64(bytes) / (mbps * 1e6)
		samples = append(samples, SpeedSample{Label: "随机", OffsetBytes: 0, Bytes: bytes, Seconds: seconds, MBps: mbps})
	} else {
		plan, err := createSpeedPlan(job.Disk.Size, sampleMiB, speedPosition)
		if err != nil {
			return BenchmarkResult{}, err
		}
		demoSpeeds := map[string]float64{"start": 286, "middle": 262, "end": 238}
		if spec.QueueDepth == 8 {
			demoSpeeds = map[string]float64{"start": 328, "middle": 306, "end": 282}
		}
		if write {
			demoSpeeds = map[string]float64{"start": 240, "middle": 220, "end": 198}
			if spec.QueueDepth == 8 {
				demoSpeeds = map[string]float64{"start": 275, "middle": 254, "end": 230}
			}
		} else if spec.Operation == "mixed" {
			demoSpeeds = map[string]float64{"start": 266, "middle": 244, "end": 220}
			if spec.QueueDepth == 8 {
				demoSpeeds = map[string]float64{"start": 301, "middle": 280, "end": 256}
			}
		}
		for i, target := range plan.Targets {
			if err := m.checkCancelled(job); err != nil {
				return BenchmarkResult{}, err
			}
			m.mu.Lock()
			job.Phase = fmt.Sprintf("%s位置顺序%s Q%dT%d（%d/%d）", target.Label, verb, spec.QueueDepth, spec.Threads, i+1, len(plan.Targets))
			m.mu.Unlock()
			m.delayFn(350 * time.Millisecond)
			if err := m.checkCancelled(job); err != nil {
				return BenchmarkResult{}, err
			}
			mbps := demoSpeeds[target.ID]
			bytes := target.BlockCount * speedBlockSize
			samples = append(samples, SpeedSample{
				Label:       target.Label,
				OffsetBytes: target.OffsetBlocks * speedBlockSize,
				Bytes:       bytes,
				Seconds:     float64(bytes) / (mbps * 1e6),
				MBps:        mbps,
			})
		}
	}
	var bytes int64
	var seconds float64
	var speeds []float64
	for _, s := range samples {
		bytes += s.Bytes
		seconds += s.Seconds
		speeds = append(speeds, s.MBps)
	}
	average := round1(float64(bytes) / seconds / 1e6)
	result := BenchmarkResult{
		Operation:   spec.Operation,
		Pattern:     spec.Pattern,
		BlockSize:   spec.BlockSize,
		QueueDepth:  spec.QueueDepth,
		Threads:     spec.Threads,
		AverageMBps: average,
		MinimumMBps: minFloat(speeds),
		MaximumMBps: maxFloat(speeds),
		Samples:     samples,
		IOPS:        calculateBenchmarkIOPS(bytes, spec.BlockSize, seconds),
	}
	return result, nil
}

func (m *JobManager) runSpeed(job *Job, sampleMiB, runs int64, speedPosition, speedOperation, speedPattern string, speedBlockSize, speedQueueDepth, speedThreadCount int64, writeAllowed, speedProfile bool, speedProfileRow, ioEngine string, directIO, trimBefore bool) error {
	if err := m.start(job); err != nil {
		return err
	}
	cancel := m.beginCancellation(job)
	defer m.endCancellation(job, cancel)

	rawTarget := job.StorageSpace == nil
	if rawTarget {
		if _, err := m.assertRawSpeedIdentity(job); err != nil {
			return err
		}
	} else {
		space, err := m.assertStorageIdentity(job)
		if err != nil {
			return err
		}
		if err := benchmarkSpaceCapacityError(space, sampleMiB*1024*1024); err != nil {
			return err
		}
	}
	if trimBefore && !rawTarget {
		if err := m.trimStorageSpace(job, cancel.ctx); err != nil {
			return err
		}
	}
	benchmarkSize := sampleMiB * 1024 * 1024
	benchmarkPath := job.Device
	targetSize := job.Disk.Size
	if !rawTarget {
		opened, temporaryPath, err := m.awaitOpenBenchmark(job, directIO)
		if err != nil {
			return err
		}
		handle := opened
		benchmarkPath = temporaryPath
		targetSize = benchmarkSize
		m.mu.Lock()
		m.handles[job.ID] = handle
		m.mu.Unlock()
		defer func() {
			m.mu.Lock()
			if m.handles[job.ID] == handle {
				delete(m.handles, job.ID)
			}
			m.mu.Unlock()
			if handle != nil {
				m.closeHandle(handle)
			}
			if err := os.Remove(temporaryPath); err != nil && !os.IsNotExist(err) {
				m.logger.Errorf("[diskbench] 删除测速临时文件失败 (%v)", err)
			}
		}()
		if err := m.checkCancelled(job); err != nil {
			return err
		}
		if err := m.prepareBenchmarkFile(handle, job, benchmarkSize); err != nil {
			return err
		}
		m.mu.Lock()
		if m.handles[job.ID] == handle {
			delete(m.handles, job.ID)
		}
		m.mu.Unlock()
		m.closeHandle(handle)
		handle = nil
	}

	fioPath := m.findExecutable("fio")
	if fioPath == "" {
		return newJobError("系统未找到 fio，无法执行磁盘测速", "CAPABILITY_UNAVAILABLE", 503)
	}
	specs := selectSpeedBenchmarks(speedOperation, speedPattern, speedBlockSize, speedQueueDepth, speedThreadCount, writeAllowed, speedProfile, speedProfileRow)
	var benchmarks []BenchmarkResult
	for i, spec := range specs {
		var runResults []BenchmarkResult
		for run := int64(0); run < runs; run++ {
			benchmark, err := m.runFioBenchmark(fioPath, benchmarkPath, job, cancel.ctx, spec, sampleMiB, speedPosition, benchmarkSize, targetSize, ioEngine, directIO)
			if err != nil {
				return err
			}
			runResults = append(runResults, benchmark)
			m.mu.Lock()
			job.Progress = float64(int64(i)*runs+run+1) / float64(int64(len(specs))*runs) * 100
			m.mu.Unlock()
		}
		benchmark := combineBenchmarkRuns(spec, runResults)
		benchmarks = append(benchmarks, benchmark)
		m.mu.Lock()
		job.Result = &speedInProgress{Benchmarks: benchmarks}
		m.mu.Unlock()
		m.persistSafe(job, "保存任务状态")
	}
	result := &SpeedResult{SampleMiB: sampleMiB, Runs: runs, Benchmarks: benchmarks}
	m.complete(job, result, "一键测速完成")
	return nil
}

type fioIOStats struct {
	IOBytes int64   `json:"io_bytes"`
	Runtime float64 `json:"runtime"`
}

type fioJobResult struct {
	Read  fioIOStats `json:"read"`
	Write fioIOStats `json:"write"`
}

type fioOutput struct {
	Jobs []fioJobResult `json:"jobs"`
}

func parseFioSample(output []byte, label string, offset int64) (SpeedSample, error) {
	var result fioOutput
	if err := json.Unmarshal(output, &result); err != nil || len(result.Jobs) == 0 {
		return SpeedSample{}, errors.New("fio 返回了无法解析的测速结果")
	}
	var bytesTransferred int64
	var runtimeMs float64
	for _, fioJob := range result.Jobs {
		bytesTransferred += fioJob.Read.IOBytes + fioJob.Write.IOBytes
		if fioJob.Read.Runtime > runtimeMs {
			runtimeMs = fioJob.Read.Runtime
		}
		if fioJob.Write.Runtime > runtimeMs {
			runtimeMs = fioJob.Write.Runtime
		}
	}
	if bytesTransferred <= 0 || runtimeMs <= 0 {
		return SpeedSample{}, errors.New("fio 未返回有效的 I/O 统计")
	}
	seconds := runtimeMs / 1000
	return SpeedSample{
		Label: label, OffsetBytes: offset, Bytes: bytesTransferred,
		Seconds: seconds, MBps: round1(float64(bytesTransferred) / seconds / 1e6),
	}, nil
}

func fioReadWriteMode(spec benchmarkSpec) string {
	prefix := ""
	if spec.Pattern == "random" {
		prefix = "rand"
	}
	switch spec.Operation {
	case "write":
		return prefix + "write"
	case "mixed":
		return prefix + "rw"
	default:
		return prefix + "read"
	}
}

func conciseCommandError(prefix string, err error, stderr string) error {
	message := strings.TrimSpace(stderr)
	if len(message) > 240 {
		message = message[len(message)-240:]
	}
	if message == "" {
		message = err.Error()
	}
	return fmt.Errorf("%s：%s", prefix, message)
}

func (m *JobManager) runFioSample(fioPath, temporaryPath string, job *Job, cancelContext context.Context, spec benchmarkSpec, label string, offset, length int64, ioEngine string, directIO bool) (SpeedSample, error) {
	if job.StorageSpace == nil {
		current, err := m.assertRawSpeedIdentity(job)
		if err != nil {
			return SpeedSample{}, err
		}
		if offset < 0 || length <= 0 || offset > current.Size || length > current.Size-offset {
			return SpeedSample{}, newJobError("测速区域超出当前磁盘容量", "RAW_SPEED_TARGET_CHANGED", 409)
		}
	}
	perJobSize := alignDown(length/spec.Threads, spec.BlockSize)
	if perJobSize < spec.BlockSize {
		return SpeedSample{}, newJobError("测速样本不足以分配给所选并发任务数", "INVALID_SPEED_COMBINATION", 400)
	}
	direct := "0"
	if directIO {
		direct = "1"
	}
	args := []string{
		"--name=diskbench", "--filename=" + temporaryPath,
		"--rw=" + fioReadWriteMode(spec), "--bs=" + strconv.FormatInt(spec.BlockSize, 10),
		"--ioengine=" + ioEngine, "--iodepth=" + strconv.FormatInt(spec.QueueDepth, 10),
		"--numjobs=" + strconv.FormatInt(spec.Threads, 10), "--direct=" + direct,
		"--size=" + strconv.FormatInt(perJobSize, 10), "--offset=" + strconv.FormatInt(offset, 10),
		"--offset_increment=" + strconv.FormatInt(perJobSize, 10),
		"--create_on_open=0", "--allow_file_create=0", "--fallocate=none", "--thread=1", "--eta=never", "--output-format=json",
	}
	if spec.Operation == "mixed" {
		args = append(args, "--rwmixread=70")
	}
	if spec.Pattern == "random" {
		args = append(args, "--randrepeat=0", "--norandommap=1")
	}
	if benchmarkIncludesWrite(spec.Operation) {
		args = append(args, "--end_fsync=1")
	}

	command := exec.CommandContext(cancelContext, fioPath, args...)
	command.Env = append(os.Environ(), "LC_ALL=C")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if cancelContext.Err() != nil || m.checkCancelled(job) != nil {
		return SpeedSample{}, errCancelled
	}
	if err != nil {
		return SpeedSample{}, conciseCommandError("fio 测速失败", err, stderr.String())
	}
	return parseFioSample(output, label, offset)
}

func (m *JobManager) runFioBenchmark(fioPath, benchmarkPath string, job *Job, cancelContext context.Context, spec benchmarkSpec, sampleMiB int64, speedPosition string, benchmarkSize, targetSize int64, ioEngine string, directIO bool) (BenchmarkResult, error) {
	var samples []SpeedSample
	if job.StorageSpace == nil {
		if _, err := m.assertRawSpeedIdentity(job); err != nil {
			return BenchmarkResult{}, err
		}
	} else if _, err := m.assertStorageIdentity(job); err != nil {
		return BenchmarkResult{}, err
	}
	if spec.Pattern == "random" {
		m.mu.Lock()
		job.Phase = fmt.Sprintf("随机%s（%s Q%dT%d）", benchmarkOperationLabel(spec.Operation), formatBlockSize(spec.BlockSize), spec.QueueDepth, spec.Threads)
		m.mu.Unlock()
		m.persistSafe(job, "保存任务状态")
		sample, err := m.runFioSample(fioPath, benchmarkPath, job, cancelContext, spec, "随机", 0, benchmarkSize, ioEngine, directIO)
		if err != nil {
			return BenchmarkResult{}, err
		}
		samples = append(samples, sample)
	} else {
		plan, err := createSpeedPlan(targetSize, sampleMiB, speedPosition)
		if err != nil {
			return BenchmarkResult{}, err
		}
		for index, target := range plan.Targets {
			m.mu.Lock()
			job.Phase = fmt.Sprintf("%s位置顺序%s Q%dT%d（%d/%d）", target.Label, benchmarkOperationLabel(spec.Operation), spec.QueueDepth, spec.Threads, index+1, len(plan.Targets))
			m.mu.Unlock()
			m.persistSafe(job, "保存任务状态")
			offset := target.OffsetBlocks * speedBlockSize
			length := target.BlockCount * speedBlockSize
			sample, err := m.runFioSample(fioPath, benchmarkPath, job, cancelContext, spec, target.Label, offset, length, ioEngine, directIO)
			if err != nil {
				return BenchmarkResult{}, err
			}
			samples = append(samples, sample)
		}
	}

	var bytesTransferred int64
	var seconds float64
	var speeds []float64
	for _, sample := range samples {
		bytesTransferred += sample.Bytes
		seconds += sample.Seconds
		speeds = append(speeds, sample.MBps)
	}
	result := BenchmarkResult{
		Operation: spec.Operation, Pattern: spec.Pattern, BlockSize: spec.BlockSize,
		QueueDepth: spec.QueueDepth, Threads: spec.Threads,
		AverageMBps: round1(float64(bytesTransferred) / seconds / 1e6),
		MinimumMBps: minFloat(speeds), MaximumMBps: maxFloat(speeds), Samples: samples,
		IOPS: calculateBenchmarkIOPS(bytesTransferred, spec.BlockSize, seconds),
	}
	return result, nil
}

func (m *JobManager) trimStorageSpace(job *Job, cancelContext context.Context) error {
	fstrimPath := m.findExecutable("fstrim")
	if fstrimPath == "" {
		return newJobError("系统未找到 fstrim，无法在测速前执行 TRIM", "CAPABILITY_UNAVAILABLE", 503)
	}
	m.mu.Lock()
	job.Phase = "正在执行测速前 TRIM"
	m.mu.Unlock()
	m.persistSafe(job, "保存任务状态")
	command := exec.CommandContext(cancelContext, fstrimPath, "--", job.Device)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if cancelContext.Err() != nil || m.checkCancelled(job) != nil {
			return errCancelled
		}
		return conciseCommandError("TRIM 执行失败", err, stderr.String())
	}
	return nil
}

func (m *JobManager) runBenchmark(handle WriteHandle, job *Job, spec benchmarkSpec, sampleMiB int64, speedPosition string, benchmarkSize int64) (BenchmarkResult, error) {
	var samples []SpeedSample
	var err error
	if spec.Pattern == "random" {
		samples, err = m.runRandomBenchmark(handle, job, spec, sampleMiB, benchmarkSize)
	} else {
		samples, err = m.runSequentialBenchmark(handle, job, spec, sampleMiB, speedPosition, benchmarkSize)
	}
	if err != nil {
		return BenchmarkResult{}, err
	}
	var bytes int64
	var seconds float64
	var speeds []float64
	for _, s := range samples {
		bytes += s.Bytes
		seconds += s.Seconds
		speeds = append(speeds, s.MBps)
	}
	average := round1(float64(bytes) / seconds / 1e6)
	result := BenchmarkResult{
		Operation:   spec.Operation,
		Pattern:     spec.Pattern,
		BlockSize:   spec.BlockSize,
		QueueDepth:  spec.QueueDepth,
		Threads:     spec.Threads,
		AverageMBps: average,
		MinimumMBps: minFloat(speeds),
		MaximumMBps: maxFloat(speeds),
		Samples:     samples,
		IOPS:        calculateBenchmarkIOPS(bytes, spec.BlockSize, seconds),
	}
	return result, nil
}

func (m *JobManager) awaitOpenBenchmark(job *Job, directIO bool) (WriteHandle, string, error) {
	temporaryPath := filepath.Join(job.Device, ".diskbench-benchmark-"+job.ID)
	openFn := func() (WriteHandle, error) {
		return openBenchmarkFile(temporaryPath, directIO)
	}
	m.mu.Lock()
	cancel, hasCancel := m.cancels[job.ID]
	m.mu.Unlock()
	if !hasCancel {
		handle, err := openFn()
		return handle, temporaryPath, err
	}
	select {
	case <-cancel.ctx.Done():
		return nil, temporaryPath, errCancelled
	default:
	}
	type result struct {
		handle WriteHandle
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		h, err := openFn()
		ch <- result{handle: h, err: err}
	}()
	select {
	case <-cancel.ctx.Done():
		go func() {
			r := <-ch
			if r.handle != nil {
				m.closeHandle(r.handle)
				_ = os.Remove(temporaryPath)
			}
		}()
		return nil, temporaryPath, errCancelled
	case r := <-ch:
		return r.handle, temporaryPath, r.err
	}
}

func syncBenchmark(handle WriteHandle) error {
	if syncer, ok := handle.(interface{ Sync() error }); ok {
		return syncer.Sync()
	}
	return nil
}

func (m *JobManager) prepareBenchmarkFile(handle WriteHandle, job *Job, size int64) error {
	buffer, release := allocateAligned(speedBlockSize)
	defer release()
	fillPattern(buffer, writePatternA)
	m.mu.Lock()
	job.Phase = "准备测速临时文件"
	m.mu.Unlock()
	m.persistSafe(job, "保存任务状态")
	for offset := int64(0); offset < size; offset += int64(len(buffer)) {
		length := int64(len(buffer))
		if remaining := size - offset; remaining < length {
			length = remaining
		}
		if err := m.benchmarkWrite(handle, buffer[:length], offset, job); err != nil {
			return err
		}
	}
	return syncBenchmark(handle)
}

func (m *JobManager) benchmarkRead(handle WriteHandle, buffer []byte, pos int64, job *Job) error {
	read := 0
	for read < len(buffer) {
		if err := m.checkCancelled(job); err != nil {
			return err
		}
		n, err := awaitCancelable(m, job, func() (int, error) {
			return handle.ReadAt(buffer[read:], pos+int64(read))
		})
		if err != nil {
			if m.checkCancelled(job) != nil {
				return errCancelled
			}
			return err
		}
		if n <= 0 || n > len(buffer)-read {
			return errors.New("读取返回了无效的字节数")
		}
		read += n
	}
	return nil
}

func (m *JobManager) benchmarkWrite(handle WriteHandle, buffer []byte, pos int64, job *Job) error {
	written := 0
	for written < len(buffer) {
		if err := m.checkCancelled(job); err != nil {
			return err
		}
		n, err := awaitCancelable(m, job, func() (int, error) {
			return handle.WriteAt(buffer[written:], pos+int64(written))
		})
		if err != nil {
			if m.checkCancelled(job) != nil {
				return errCancelled
			}
			return err
		}
		if n <= 0 || n > len(buffer)-written {
			return errors.New("写入返回了无效的字节数")
		}
		written += n
	}
	return nil
}

type benchmarkIORequest struct {
	position int64
	write    bool
}

func benchmarkIncludesWrite(operation string) bool {
	return operation == "write" || operation == "mixed"
}

func benchmarkRequestWrites(operation string, index int64) bool {
	if operation == "write" {
		return true
	}
	// Match CrystalDiskMark's mixed profile: 70% reads and 30% writes.
	return operation == "mixed" && index%10 >= 7
}

func (m *JobManager) runConcurrentBenchmarkIO(handle WriteHandle, job *Job, blockSize, queueDepth int64, requests []benchmarkIORequest) error {
	if len(requests) == 0 {
		return nil
	}
	workers := int(queueDepth)
	if workers < 1 {
		workers = 1
	}
	if workers > len(requests) {
		workers = len(requests)
	}

	var next atomic.Int64
	var completed atomic.Int64
	var wg sync.WaitGroup
	var errorMu sync.Mutex
	var firstErr error
	worker := func() {
		defer wg.Done()
		buffer, release := allocateAligned(int(blockSize))
		defer release()
		fillPattern(buffer, writePatternA)
		for {
			errorMu.Lock()
			failed := firstErr != nil
			errorMu.Unlock()
			if failed {
				return
			}
			index := next.Add(1) - 1
			if index >= int64(len(requests)) {
				return
			}
			request := requests[index]
			var err error
			if request.write {
				err = m.benchmarkWrite(handle, buffer, request.position, job)
			} else {
				err = m.benchmarkRead(handle, buffer, request.position, job)
			}
			if err != nil {
				errorMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errorMu.Unlock()
				return
			}
			completed.Add(1)
		}
	}

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go worker()
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	if completed.Load() != int64(len(requests)) {
		return errors.New("测速 I/O 未完整执行")
	}
	return nil
}

func (m *JobManager) runSequentialBenchmark(handle WriteHandle, job *Job, spec benchmarkSpec, sampleMiB int64, speedPosition string, benchmarkSize int64) ([]SpeedSample, error) {
	plan, err := createSpeedPlan(benchmarkSize, sampleMiB, speedPosition)
	if err != nil {
		return nil, err
	}

	var samples []SpeedSample
	for i, target := range plan.Targets {
		if err := m.checkCancelled(job); err != nil {
			return nil, err
		}
		if _, err := m.assertStorageIdentity(job); err != nil {
			return nil, err
		}
		m.mu.Lock()
		job.Phase = fmt.Sprintf("%s位置顺序%s Q%dT%d（%d/%d）", target.Label, benchmarkOperationLabel(spec.Operation), spec.QueueDepth, spec.Threads, i+1, len(plan.Targets))
		m.mu.Unlock()
		m.persistSafe(job, "保存任务状态")

		offset := target.OffsetBlocks * speedBlockSize
		length := target.BlockCount * speedBlockSize
		blocks := length / spec.BlockSize
		requests := make([]benchmarkIORequest, blocks)
		for block := int64(0); block < blocks; block++ {
			requests[block] = benchmarkIORequest{
				position: offset + block*spec.BlockSize,
				write:    benchmarkRequestWrites(spec.Operation, block),
			}
		}
		started := time.Now()
		if err := m.runConcurrentBenchmarkIO(handle, job, spec.BlockSize, spec.QueueDepth, requests); err != nil {
			return nil, err
		}
		if benchmarkIncludesWrite(spec.Operation) {
			if err := syncBenchmark(handle); err != nil {
				return nil, err
			}
		}
		elapsed := time.Since(started).Seconds()
		bytes := blocks * spec.BlockSize
		mbps := round1(float64(bytes) / elapsed / 1e6)
		samples = append(samples, SpeedSample{
			Label:       target.Label,
			OffsetBytes: offset,
			Bytes:       bytes,
			Seconds:     elapsed,
			MBps:        mbps,
		})
		m.mu.Lock()
		job.Progress = round1(float64(i+1) / float64(len(plan.Targets)) * 100)
		m.mu.Unlock()
		m.persistSafe(job, "保存任务状态")
	}
	return samples, nil
}

func (m *JobManager) runRandomBenchmark(handle WriteHandle, job *Job, spec benchmarkSpec, sampleMiB int64, benchmarkSize int64) ([]SpeedSample, error) {
	totalBlocks := sampleMiB * 1024 * 1024 / spec.BlockSize
	maxOffset := benchmarkSize - spec.BlockSize
	if maxOffset < spec.BlockSize {
		return nil, newJobError("磁盘容量不足以执行随机测速", "DISK_TOO_SMALL", 400)
	}
	alignedBlocks := maxOffset / spec.BlockSize
	m.mu.Lock()
	job.Phase = fmt.Sprintf("随机%s（4K Q%dT%d）", benchmarkOperationLabel(spec.Operation), spec.QueueDepth, spec.Threads)
	m.mu.Unlock()
	m.persistSafe(job, "保存任务状态")

	started := time.Now()
	if _, err := m.assertStorageIdentity(job); err != nil {
		return nil, err
	}
	requests := make([]benchmarkIORequest, totalBlocks)
	for block := int64(0); block < totalBlocks; block++ {
		requests[block] = benchmarkIORequest{
			position: mathrand.Int63n(alignedBlocks) * spec.BlockSize,
			write:    benchmarkRequestWrites(spec.Operation, block),
		}
	}
	if err := m.runConcurrentBenchmarkIO(handle, job, spec.BlockSize, spec.QueueDepth, requests); err != nil {
		return nil, err
	}
	if benchmarkIncludesWrite(spec.Operation) {
		if err := syncBenchmark(handle); err != nil {
			return nil, err
		}
	}
	elapsed := time.Since(started).Seconds()
	bytes := totalBlocks * spec.BlockSize
	mbps := round1(float64(bytes) / elapsed / 1e6)
	sample := SpeedSample{Label: "随机", OffsetBytes: 0, Bytes: bytes, Seconds: elapsed, MBps: mbps}
	m.mu.Lock()
	job.Progress = 100
	m.mu.Unlock()
	m.persistSafe(job, "保存任务状态")
	return []SpeedSample{sample}, nil
}

func minFloat(values []float64) float64 {
	m := values[0]
	for _, v := range values[1:] {
		if v < m {
			m = v
		}
	}
	return m
}

func maxFloat(values []float64) float64 {
	m := values[0]
	for _, v := range values[1:] {
		if v > m {
			m = v
		}
	}
	return m
}

func (m *JobManager) cancel(id string) (*Job, error) {
	var handleToClose ReadHandle
	var repairToClose ReadHandle
	var childToKill ddProcess
	var result *Job
	var retErr error

	m.mu.Lock()
	job := m.jobs[id]
	if job == nil {
		m.mu.Unlock()
		return nil, newJobError("任务不存在", "JOB_NOT_FOUND", 404)
	}
	if !activeJobStatuses[job.Status] {
		m.mu.Unlock()
		return nil, newJobError("任务已结束，无法取消", "JOB_FINISHED", 409)
	}
	if job.Status == "queued" {
		job.Status = "cancelled"
		job.Phase = "任务已取消"
		finished := isoTime(m.now())
		job.FinishedAt = &finished
		job.Message = "任务已由用户取消"
		m.appendLog(job, job.Message)
		m.persistSafeLocked(job, "保存任务取消状态")
		m.pruneHistoryLocked()
		result = cloneJobLocked(job)
		m.mu.Unlock()
		return result, nil
	}
	if job.Status == "cancelling" {
		result = cloneJobLocked(job)
		m.mu.Unlock()
		return result, nil
	}
	job.Status = "cancelling"
	job.Phase = "正在停止任务"
	if c, ok := m.cancels[id]; ok {
		c.cancel()
	}
	if h, ok := m.handles[id]; ok {
		delete(m.handles, id)
		handleToClose = h
	}
	if h := m.handles[id+"/repair"]; h != nil {
		repairToClose = h
	}
	if c, ok := m.processes[id]; ok {
		childToKill = c
	}
	m.persistSafeLocked(job, "保存任务取消状态")
	result = cloneJobLocked(job)
	m.mu.Unlock()

	if handleToClose != nil {
		// Close may wait behind an in-flight kernel operation. Keep HTTP responsive.
		go m.closeHandle(handleToClose)
	}
	if repairToClose != nil {
		go m.closeHandle(repairToClose)
	}
	if childToKill != nil {
		if err := childToKill.Kill(); err != nil {
			retErr = err
		}
	}
	return result, retErr
}

func (m *JobManager) shutdown() {
	var handlesToClose []ReadHandle
	var childrenToKill []ddProcess

	m.mu.Lock()
	m.stopped = true
	for id, c := range m.cancels {
		c.cancel()
		if h, ok := m.handles[id]; ok {
			delete(m.handles, id)
			handlesToClose = append(handlesToClose, h)
		}
	}
	for id, h := range m.handles {
		delete(m.handles, id)
		handlesToClose = append(handlesToClose, h)
	}
	for _, c := range m.processes {
		childrenToKill = append(childrenToKill, c)
	}
	for _, job := range m.jobs {
		if activeJobStatuses[job.Status] {
			job.Status = "interrupted"
			job.Phase = "任务已中断"
			finished := isoTime(m.now())
			job.FinishedAt = &finished
			job.Message = "应用服务已停止"
			m.appendLog(job, job.Message)
			m.persistSafeLocked(job, "保存任务中断状态")
		}
	}
	m.pruneHistoryLocked()
	m.mu.Unlock()

	for _, h := range handlesToClose {
		m.closeHandle(h)
	}
	for _, c := range childrenToKill {
		c.Kill()
	}
}

// WriteHandle abstracts a read-write device handle for the destructive write
// test. It embeds ReadHandle so the same handle map and close path apply.
type WriteHandle interface {
	ReadHandle
	WriteAt(p []byte, off int64) (int, error)
}

const (
	writePatternA       = byte(0x55)
	writePatternB       = byte(0xAA)
	writeVerifyPageSize = 4096
)

func fillPattern(b []byte, pattern byte) {
	for i := range b {
		b[i] = pattern
	}
}

// findPatternMismatches reports pages that do not fully match the expected
// pattern, bounded by limit entries.
func findPatternMismatches(buffer []byte, pattern byte, baseOffset int64, limit int) []BadRange {
	ranges, _ := inspectPatternMismatches(buffer, pattern, baseOffset, limit)
	return ranges
}

// The detail cap only limits stored addresses, never the verification itself.
func inspectPatternMismatches(buffer []byte, pattern byte, baseOffset int64, limit int) ([]BadRange, int) {
	var ranges []BadRange
	errors := 0
	for offset := 0; offset < len(buffer); offset += writeVerifyPageSize {
		size := min(writeVerifyPageSize, len(buffer)-offset)
		matches := true
		for i := 0; i < size; i++ {
			if buffer[offset+i] != pattern {
				matches = false
				break
			}
		}
		if !matches {
			errors++
			if len(ranges) >= limit {
				continue
			}
			ranges = append(ranges, BadRange{
				Offset: baseOffset + int64(offset),
				Length: int64(size),
				Code:   "WRITE_VERIFY_MISMATCH",
			})
		}
	}
	return ranges, errors
}

type writeRegionResult struct {
	maxReadLatency  *ScanReadLatency
	maxWriteLatency *ScanReadLatency
	bytesWritten    int
	milliseconds    float64
	badRanges       []BadRange
	errorCount      int
}

// writeVerifyRegion writes and reads back through O_DIRECT. OS page cache is
// bypassed, but device caches remain; this is not a power-loss durability test.
func (m *JobManager) writeVerifyRegion(handle WriteHandle, pattern byte, buffer []byte, length int, position int64, job *Job, detailLimit int) (result writeRegionResult, resultErr error) {
	started := time.Now()
	defer func() { result.milliseconds = float64(time.Since(started).Nanoseconds()) / 1e6 }()
	if length <= 0 || length > len(buffer) {
		return result, errors.New("写读校验范围无效")
	}
	// GC-owned aligned memory remains alive if a cancelled I/O outlives us.
	requestBuffer := scanBuffer(min(length, scanLatencyRequestBytes))
	type timedIO struct {
		n  int
		ms float64
	}
	fail := func(code string, offset int64, size int, err error) (writeRegionResult, error) {
		result.errorCount++
		appendBadRanges(&result.badRanges, []BadRange{{Offset: offset, Length: int64(size), Code: code}}, max(0, detailLimit))
		if isRecoverableMediaError(err) {
			return result, nil
		}
		return result, err
	}
	for result.bytesWritten < length {
		if err := m.checkCancelled(job); err != nil {
			return result, err
		}
		size := min(scanLatencyRequestBytes, length-result.bytesWritten)
		offset := position + int64(result.bytesWritten)
		target := requestBuffer[:size]
		fillPattern(target, pattern)
		timing, err := awaitCancelable(m, job, func() (timedIO, error) {
			start := time.Now()
			n, err := handle.WriteAt(target, offset)
			return timedIO{n: n, ms: float64(time.Since(start).Nanoseconds()) / 1e6}, err
		})
		if err == errCancelled || m.checkCancelled(job) != nil {
			return result, errCancelled
		}
		if timing.n < 0 || timing.n > size {
			return fail("WRITE_ERROR", offset, size, errors.New("写入返回了无效的字节数"))
		}
		result.bytesWritten += timing.n
		if err != nil {
			return fail("WRITE_ERROR", offset, size, err)
		}
		if timing.n == 0 {
			return fail("WRITE_ERROR", offset, size, io.ErrShortWrite)
		}
		if result.maxWriteLatency == nil || timing.ms > result.maxWriteLatency.Ms {
			result.maxWriteLatency = &ScanReadLatency{Ms: timing.ms, Bytes: size, Offset: offset}
		}
	}
	read := 0
	for read < length {
		if err := m.checkCancelled(job); err != nil {
			return result, err
		}
		size := min(scanLatencyRequestBytes, length-read)
		offset := position + int64(read)
		target := requestBuffer[:size]
		// Do not leave the expected pattern in a buffer handed to ReadAt.
		fillPattern(target, ^pattern)
		timing, err := awaitCancelable(m, job, func() (timedIO, error) {
			start := time.Now()
			n, err := handle.ReadAt(target, offset)
			return timedIO{n: n, ms: float64(time.Since(start).Nanoseconds()) / 1e6}, err
		})
		if err == errCancelled || m.checkCancelled(job) != nil {
			return result, errCancelled
		}
		if timing.n < 0 || timing.n > size {
			return fail("READ_AFTER_WRITE_ERROR", offset, size, errors.New("读回返回了无效的字节数"))
		}
		if err != nil {
			return fail("READ_AFTER_WRITE_ERROR", offset, size, err)
		}
		if timing.n == 0 {
			return fail("READ_AFTER_WRITE_ERROR", offset, size, io.ErrUnexpectedEOF)
		}
		copy(buffer[read:read+timing.n], target[:timing.n])
		read += timing.n
		if result.maxReadLatency == nil || timing.ms > result.maxReadLatency.Ms {
			result.maxReadLatency = &ScanReadLatency{Ms: timing.ms, Bytes: size, Offset: offset}
		}
	}
	result.badRanges, result.errorCount = inspectPatternMismatches(buffer[:length], pattern, position, max(0, detailLimit))
	return result, nil
}

func (m *JobManager) awaitOpenWrite(job *Job) (WriteHandle, error) {
	m.mu.Lock()
	cancel, hasCancel := m.cancels[job.ID]
	m.mu.Unlock()
	if !hasCancel {
		return openWriteDevice(job.Device)
	}
	select {
	case <-cancel.ctx.Done():
		return nil, errCancelled
	default:
	}
	type result struct {
		handle WriteHandle
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		h, err := openWriteDevice(job.Device)
		ch <- result{handle: h, err: err}
	}()
	select {
	case <-cancel.ctx.Done():
		go func() {
			r := <-ch
			if r.handle != nil {
				m.closeHandle(r.handle)
			}
		}()
		return nil, errCancelled
	case r := <-ch:
		return r.handle, r.err
	}
}

// runWriteTest performs the destructive write test: alternating pattern
// write, read-back and verify over the whole device.
func (m *JobManager) runWriteTest(job *Job) error {
	if err := m.start(job); err != nil {
		return err
	}
	cancel := m.beginCancellation(job)
	var handle WriteHandle
	defer func() {
		m.mu.Lock()
		if handle != nil && m.handles[job.ID] == handle {
			delete(m.handles, job.ID)
		}
		m.mu.Unlock()
		if handle != nil {
			m.closeHandle(handle)
		}
		m.endCancellation(job, cancel)
	}()

	if _, err := m.assertDiskIdentity(job); err != nil {
		return err
	}
	if err := m.checkCancelled(job); err != nil {
		return err
	}
	opened, err := m.awaitOpenWrite(job)
	if err != nil {
		return err
	}
	handle = opened
	m.mu.Lock()
	m.handles[job.ID] = handle
	m.mu.Unlock()

	result := job.Result.(*ScanResult)
	var badRanges []BadRange
	if _, err := m.assertDiskIdentity(job); err != nil {
		return err
	}
	if err := m.checkCancelled(job); err != nil {
		return err
	}

	buffer := scanBuffer(fullReadSize)

	for index := 0; index < len(result.Map); index++ {
		cell := &result.Map[index]
		position := cell.Start
		for position < cell.End {
			if err := m.checkCancelled(job); err != nil {
				return err
			}
			m.mu.Lock()
			cell.Status = "reading"
			length := cell.End - position
			if length > fullReadSize {
				length = fullReadSize
			}
			pattern := writePatternA
			if (position/fullReadSize)%2 == 1 {
				pattern = writePatternB
			}
			job.Phase = fmt.Sprintf("写读校验 %d/%d", index+1, len(result.Map))
			m.mu.Unlock()

			regionResult, err := m.writeVerifyRegion(handle, pattern, buffer, int(length), position, job, 200-len(badRanges))
			if err == errCancelled {
				return err
			}

			m.mu.Lock()
			appendBadRanges(&badRanges, regionResult.badRanges, 200)
			// Aggregate transfer cost and individual write/read waits are independent.
			m.updateWriteCell(cell, regionResult, length, job.Disk.Rotational)
			if err != nil {
				m.updateScanStats(job)
				result.Stats.BadRanges = append([]BadRange{}, badRanges...)
				m.mu.Unlock()
				return err
			}
			position += length
			if position < cell.End && cell.ErrorCount == 0 {
				cell.Status = "reading"
			}
			job.Progress = round1(float64(position) / float64(job.Disk.Size) * 100)
			m.updateScanStats(job)
			result.Stats.BadRanges = append([]BadRange{}, badRanges...)
			m.mu.Unlock()

			m.persistThrottled(job, false)
		}
	}

	if err := m.checkCancelled(job); err != nil {
		return err
	}
	m.mu.Lock()
	m.updateScanStats(job)
	errorCells := result.Stats.ErrorCells
	m.mu.Unlock()

	message := writeCompletionMessage(errorCells, result.Stats.SlowCells)
	m.complete(job, result, message)
	return nil
}

// runMockWriteTest models the destructive write test in demo mode without
// touching a device.
func (m *JobManager) runMockWriteTest(job *Job) error {
	if err := m.start(job); err != nil {
		return err
	}
	result := job.Result.(*ScanResult)
	for index := 0; index < len(result.Map); index++ {
		if err := m.checkCancelled(job); err != nil {
			return err
		}
		cell := &result.Map[index]

		m.mu.Lock()
		cell.Status = "reading"
		bytesWritten := cell.End - cell.Start
		job.Phase = fmt.Sprintf("写读校验 %d/%d", index+1, len(result.Map))
		milliseconds := float64(3 + ((index * 23) % 18))
		var errorCount int64
		if index == 517 {
			errorCount = 1
		}
		timingBytes := bytesWritten
		if timingBytes > fullReadSize {
			timingBytes = fullReadSize
		}
		requestBytes := int(min(timingBytes, scanLatencyRequestBytes))
		readMs := milliseconds / math.Max(1, float64(timingBytes)/scanLatencyRequestBytes) / 2
		writeMs := readMs
		if index == 376 {
			readMs = 172
		}
		if index == 441 {
			writeMs = 1100
		}
		milliseconds = math.Max(milliseconds, readMs+writeMs)
		m.updateWriteCell(cell, writeRegionResult{
			bytesWritten: int(bytesWritten), milliseconds: milliseconds, errorCount: int(errorCount),
			maxReadLatency:  &ScanReadLatency{Ms: readMs, Bytes: requestBytes, Offset: cell.Start},
			maxWriteLatency: &ScanReadLatency{Ms: writeMs, Bytes: requestBytes, Offset: cell.Start},
		}, timingBytes, job.Disk.Rotational)
		reads := int64(math.Ceil(float64(bytesWritten) / float64(fullReadSize)))
		if reads < 1 {
			reads = 1
		}
		cell.Reads = reads
		cell.TotalMs = floatPtr(milliseconds * float64(reads))
		cell.AvgMs = floatPtr(milliseconds)
		if errorCount > 0 {
			length := int64(errorProbeSize)
			if length > bytesWritten {
				length = bytesWritten
			}
			result.Stats.BadRanges = append(result.Stats.BadRanges, BadRange{Offset: cell.Start, Length: length, Code: "WRITE_VERIFY_MISMATCH"})
		}
		job.Progress = round1(float64(index+1) / float64(len(result.Map)) * 100)
		m.updateScanStats(job)
		m.mu.Unlock()

		m.persistThrottled(job, false)
		if index%4 == 0 {
			m.delayFn(24 * time.Millisecond)
		}
	}

	m.mu.Lock()
	errorCells := result.Stats.ErrorCells
	m.mu.Unlock()

	message := "演示" + writeCompletionMessage(errorCells, result.Stats.SlowCells)
	m.complete(job, result, message)
	return nil
}
