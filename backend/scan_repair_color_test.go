package main

import (
	"strings"
	"testing"
	"time"
)

type colorRepairHandle struct {
	*memoryRepairHandle
	neighborSlow  bool
	postReadDelay time.Duration
	postReads     int
}

func (h *colorRepairHandle) ReadAt(p []byte, offset int64) (int, error) {
	if offset == 0 && h.writes.Load() == 0 {
		time.Sleep(510 * time.Millisecond)
	} else if h.neighborSlow && offset >= scanLatencyRequestBytes {
		time.Sleep(60 * time.Millisecond)
	} else if offset == 0 && h.writes.Load() > 0 {
		h.postReads++
		if h.postReads <= 2 {
			time.Sleep(h.postReadDelay)
		}
	}
	return h.memoryRepairHandle.ReadAt(p, offset)
}

func TestRepairRefreshesMapColorWithoutHidingUntouchedSlowRequests(t *testing.T) {
	for _, scenario := range []string{"restored-fast", "neighbor-still-slow", "recent-post-write-slow"} {
		t.Run(scenario, func(t *testing.T) {
			m, job, base, _, _ := repairFixture(t)
			m.scanActivityFn = func(string) (ScanActivity, error) { return ScanActivity{}, nil }
			length := 2 * scanLatencyRequestBytes
			base.data = make([]byte, length)
			h := &colorRepairHandle{memoryRepairHandle: base, neighborSlow: scenario == "neighbor-still-slow"}
			if scenario == "recent-post-write-slow" {
				h.postReadDelay = 60 * time.Millisecond
			}
			r := job.Result.(*ScanResult)
			cell := &r.Map[0]
			cell.End = int64(length)
			read, warning, ratio, recheck, err := m.inspectScanRegion(job, cell, h, h, scanBuffer(length), 0, length, 512, scanLatencyReference{}, m.newScanLoadMonitor(job.Device))
			if err != nil {
				t.Fatal(err)
			}
			m.updateCell(cell, read.milliseconds, int64(read.bytesRead), int64(read.errorCount), int64(length))
			applyHDDLatency(cell, warning, ratio)
			applyScanReadLatency(cell, read.maxReadLatency, false)
			if recheck != nil {
				applyScanReadLatency(cell, recheck.MaxReadLatency, false)
			}
			want := "normal"
			if scenario != "restored-fast" {
				want = "slower"
			}
			if cell.Status != want || base.writes.Load() != 1 || r.Stats.Repair.Improved != 1 || cell.MaxReadLatency.Ms >= 500 || scanBadBlocks(r, false).Total != 0 {
				t.Fatalf("wrong current response: %+v / %+v / writes %d", cell, r.Stats.Repair, base.writes.Load())
			}
			page, err := m.scanIssues(job.ID, 0, 50)
			if err != nil || len(page.Records) != 2 || page.Records[1].Repair.BeforeMs < 500 || page.Records[1].Repair.AfterMs == nil || *page.Records[1].Repair.AfterMs >= 500 {
				t.Fatalf("lost original before/after evidence: %+v / %v", page, err)
			}
			if !m.persistSafe(job, "repair color test") {
				t.Fatal("persist failed")
			}
			restored := NewJobManager(m.dataDir, JobManagerOptions{Logger: silentLogger{}})
			defer restored.shutdown()
			if got := restored.get(job.ID).Result.(*ScanResult).Map[0]; got.Status != want || got.MaxReadLatency.Ms >= 500 {
				t.Fatalf("restart lost updated color: %+v", got)
			}
			if scenario != "restored-fast" && reportCellGroup(*cell, "solid-dual-v4") != "slower" {
				t.Fatal("report disagrees with current map")
			}
		})
	}
}

func TestRepairFailureOrPersistentSlowProofCannotTurnGreen(t *testing.T) {
	for _, scenario := range []string{"failed", "uncertain", "unreadable", "still-slow"} {
		t.Run(scenario, func(t *testing.T) {
			cell := ScanCell{Start: 0, End: 4096, Status: "normal", MaxUnitMs: floatPtr(1)}
			want := "severe"
			switch scenario {
			case "failed":
				cell.Repair, want = &ScanRepairStats{Failed: 1}, "error"
			case "uncertain":
				cell.Repair = &ScanRepairStats{Pending: 1}
			case "unreadable":
				cell.ErrorCount, cell.UnreadableBytes, cell.ReadOutcome, want = 1, 512, "unreadable", "error"
			case "still-slow":
				cell.SlowBadBlock = &ScanSlowBlock{Offset: 0, Bytes: 512, FirstMs: 510, RepeatMs: 510, Source: "repair", Load: &ScanLoad{}}
			}
			applyScanReadLatency(&cell, &ScanReadLatency{Ms: 1, Bytes: 512}, false)
			if cell.Status != want {
				t.Fatalf("cleared %s warning: %+v", scenario, cell)
			}
		})
	}
}

func TestRecordedRepairUsesPostWriteColorWhilePreservingSource(t *testing.T) {
	base := &memoryRepairHandle{data: make([]byte, 16384)}
	h := &colorRepairHandle{memoryRepairHandle: base}
	m, source, disk := recordedRepairFixture(t, func(string) (RepairHandle, error) { return h, nil })
	m.scanActivityFn = func(string) (ScanActivity, error) { return ScanActivity{}, nil }
	// Include the truly slow request in this fixture; the second target is
	// already fast and must not be credited as a speed improvement.
	source.Result.(*ScanResult).Map[0].MaxReadLatency.Offset = 0
	created, err := m.create("scan", disk.Path, recordedRepairOptions(source, disk))
	if err != nil {
		t.Fatal(err)
	}
	done := waitForStatus(t, m, created.ID, "completed", 3*time.Second)
	r := done.Result.(*ScanResult)
	if r.Map[0].Status != "normal" || r.Map[0].MaxReadLatency.Ms >= 20 || r.Stats.Repair.Improved != 1 {
		t.Fatalf("retained pre-write red color: %+v", r.Map[0])
	}
	if source.Result.(*ScanResult).Map[0].Status != "severe" {
		t.Fatal("rewrote original scan history")
	}
	if body := renderHumanReport(t, m, done); !strings.Contains(body, "慢速改善") {
		t.Fatal("report lost repair result")
	}
}
