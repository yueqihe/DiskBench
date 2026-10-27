package main

import (
	"strings"
	"testing"
	"time"
)

func TestUsabilityBadBlocksRequireRepeatableEvidenceAndCountRegionsOnce(t *testing.T) {
	proof := ScanSlowBlock{Offset: 512, Bytes: 512, FirstMs: 500, RepeatMs: 500, Source: "recheck", Load: &ScanLoad{}}
	cell := ScanCell{Start: 0, End: 4096, Status: "severe", ReadOutcome: "readable", SlowBadBlock: &proof}
	for _, scenario := range []string{"confirmed", "one-off", "busy", "unknown-load", "outside", "hdd-below", "hdd-equal", "unreadable-overlap", "legacy-max"} {
		t.Run(scenario, func(t *testing.T) {
			current, p := cell, proof
			current.SlowBadBlock = &p
			rotational, want := false, int64(1)
			switch scenario {
			case "one-off":
				p.RepeatMs, want = 1, 0
			case "busy":
				p.Load, want = &ScanLoad{BackgroundActivity: true}, 0
			case "unknown-load":
				p.Load, want = nil, 0
			case "outside":
				p.Offset, want = 4096, 0
			case "hdd-below":
				rotational, want = true, 0
				p.FirstMs, p.RepeatMs = 800, 799
			case "hdd-equal":
				rotational = true
				p.FirstMs, p.RepeatMs = 800, 800
			case "unreadable-overlap":
				current.UnreadableBytes = 512
				current.ReadOutcome = "unreadable"
			case "legacy-max":
				current.SlowBadBlock = nil
				current.MaxReadLatency = &ScanReadLatency{Offset: 512, Bytes: 512, Ms: 1000}
				current.Recheck = &ScanRecheck{MaxReadLatency: current.MaxReadLatency, Load: &ScanLoad{}}
				want = 0
			}
			r := &ScanResult{Version: 2, LatencyBasis: scanLatencyBasis(rotational), Map: []ScanCell{current, {Start: 4096, End: 8192, Status: "error", ReadOutcome: "device_error", ErrorCount: 1}}}
			got := scanBadBlocks(r, rotational)
			if got.Total != want || got.Slow+got.Unreadable != want {
				t.Fatalf("wrong usability count: %+v", got)
			}
			if scenario == "unreadable-overlap" && (got.Slow != 0 || got.Unreadable != 1) {
				t.Fatal("counted one region twice")
			}
			if current.ErrorCount != 0 || r.Stats.UnreadableBytes != 0 {
				t.Fatal("fabricated I/O failures")
			}
		})
	}
}

func TestSlowProofMatchesExactRequestRatherThanNeighborOrAggregatedMax(t *testing.T) {
	first := []ScanReadLatency{{Offset: 512, Bytes: 512, Ms: 501}}
	for _, repeated := range []ScanReadLatency{{Offset: 1024, Bytes: 512, Ms: 1000}, {Offset: 512, Bytes: 1024, Ms: 1000}, {Offset: 512, Bytes: 512, Ms: 1}, {Offset: 512, Bytes: 512, Ms: 500}} {
		r := &ScanRecheck{requests: []ScanReadLatency{repeated}}
		confirmRepeatedSlowReads(r, first, false)
		want := repeated.Offset == 512 && repeated.Bytes == 512 && repeated.Ms >= 500
		if (len(r.confirmedSlow) == 1) != want {
			t.Fatalf("incorrect exact-request evidence: %+v", r.confirmedSlow)
		}
	}
}

func TestPersistentSlowReadProducesUsabilityVerdictAndReportWithoutReadError(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{ScanActivity: func(string) (ScanActivity, error) { return ScanActivity{}, nil }})
	job := issueTestJob(m)
	job.Status = "running"
	r := job.Result.(*ScanResult)
	r.LatencyBasis = scanLatencyBasis(false)
	cell := &r.Map[0]
	h := scanReaderFunc(func(p []byte, offset int64) (int, error) { time.Sleep(510 * time.Millisecond); return len(p), nil })
	read, _, _, _, err := m.inspectScanRegion(job, cell, h, nil, scanBuffer(512), 0, 512, 512, scanLatencyReference{}, m.newScanLoadMonitor(job.Device))
	if err != nil || read.errorCount != 0 || scanPersistentSlowBlock(*cell, false) == nil {
		t.Fatalf("failed to confirm exact persistent read: %+v / %v", cell, err)
	}
	cell.Status, cell.ReadOutcome = "severe", "readable"
	job.Status = "completed"
	message := scanOutcomeCompletionMessage("扫描", r, 0, 1, false)
	if !strings.Contains(message, "1 个坏块区域（慢速 1、不可读 0）") {
		t.Fatal(message)
	}
	body := renderHumanReport(t, m, job)
	for _, want := range []string{"坏块（慢速）", "建议更换硬盘", "同范围 512 B 两次读取"} {
		if !strings.Contains(body, want) {
			t.Fatalf("report lost %q", want)
		}
	}
	if r.Stats.UnreadableBytes != 0 || r.Stats.IssueCount != 0 {
		t.Fatal("slow verdict changed read-error accounting")
	}
}

func TestRepairSpeedClaimsRequireTwoConsistentReadsAndQuietLoad(t *testing.T) {
	quiet, busy := &ScanLoad{}, &ScanLoad{BackgroundActivity: true}
	for _, c := range []struct {
		name                  string
		before, repeat        float64
		after                 []float64
		beforeLoad, afterLoad *ScanLoad
		want                  string
	}{
		{"improved", 501, 500, []float64{10, 11}, quiet, quiet, "improved"},
		{"still-slow", 501, 500, []float64{500, 600}, quiet, quiet, "still_slow"},
		{"one-fast", 501, 500, []float64{10, 600}, quiet, quiet, "unconfirmed"},
		{"already-fast", 1, 1, []float64{1, 1}, quiet, quiet, "unconfirmed"},
		{"wake-up", 1000, 1, []float64{1, 1}, quiet, quiet, "unconfirmed"},
		{"busy-before", 1000, 1000, []float64{1, 1}, busy, quiet, "unconfirmed"},
		{"busy-after", 1000, 1000, []float64{1000, 1000}, quiet, busy, "unconfirmed"},
		{"unknown-load", 1000, 1000, []float64{1000, 1000}, quiet, nil, "unconfirmed"},
		{"one-read", 1000, 1000, []float64{1}, quiet, quiet, "unconfirmed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := repairSpeedOutcome(c.before, c.repeat, c.after, 500, c.beforeLoad, c.afterLoad); got != c.want {
				t.Fatalf("%s, want %s", got, c.want)
			}
		})
	}
}

type persistentSlowRepairHandle struct{ *memoryRepairHandle }

func (h *persistentSlowRepairHandle) ReadAt(p []byte, offset int64) (int, error) {
	if h.writes.Load() > 0 {
		time.Sleep(510 * time.Millisecond)
	}
	return h.memoryRepairHandle.ReadAt(p, offset)
}

func TestRepairStillSlowWritesOnlyOnceAndRetainsBadBlockEvidence(t *testing.T) {
	m, job, base, source, region := repairFixture(t)
	m.scanActivityFn = func(string) (ScanActivity, error) { return ScanActivity{}, nil }
	h := &persistentSlowRepairHandle{base}
	r := job.Result.(*ScanResult)
	if err := m.repairSlowRequests(job, &r.Map[0], h, source, 0, &region); err != nil {
		t.Fatal(err)
	}
	if h.writes.Load() != 1 || r.Stats.Repair.StillSlow != 1 || r.Stats.Repair.Improved != 0 || scanBadBlocks(r, false).Slow != 1 {
		t.Fatalf("incorrect still-slow result: %+v", r.Stats.Repair)
	}
	page, err := m.scanIssues(job.ID, 0, 50)
	if err != nil || len(page.Records) != 2 || len(page.Records[1].Repair.SpeedChecks) != 2 || !strings.Contains(page.Records[1].Repair.Detail, "建议更换硬盘") {
		t.Fatalf("missing evidence: %+v / %v", page, err)
	}
}

type improvingRepairHandle struct{ *memoryRepairHandle }

func (h *improvingRepairHandle) ReadAt(p []byte, offset int64) (int, error) {
	if h.writes.Load() == 0 {
		time.Sleep(30 * time.Millisecond)
	}
	return h.memoryRepairHandle.ReadAt(p, offset)
}

func TestRepairImprovementIsSeparateFromContentVerification(t *testing.T) {
	m, job, base, source, region := repairFixture(t)
	m.scanActivityFn = func(string) (ScanActivity, error) { return ScanActivity{}, nil }
	job.Options.ScanRepairAfterMs = intPtr(25)
	r := job.Result.(*ScanResult)
	if err := m.repairSlowRequests(job, &r.Map[0], &improvingRepairHandle{base}, source, 0, &region); err != nil {
		t.Fatal(err)
	}
	if base.writes.Load() != 1 || r.Stats.Repair.Verified != 1 || r.Stats.Repair.Improved != 1 || r.Stats.Repair.StillSlow != 0 {
		t.Fatalf("lost speed improvement: %+v", r.Stats.Repair)
	}
	page, err := m.scanIssues(job.ID, 0, 50)
	if err != nil || page.Records[1].Repair.SpeedOutcome != "improved" {
		t.Fatalf("speed evidence not saved: %+v / %v", page, err)
	}
	if got := reportIssue(page.Records[1], 512, 25).Outcome; !strings.Contains(got, "慢速改善") || !strings.Contains(got, "写回校验通过") {
		t.Fatal(got)
	}
}

func TestRepairRetestsWhenPreWriteReadHasRecovered(t *testing.T) {
	m, job, h, source, region := repairFixture(t)
	m.scanActivityFn = func(string) (ScanActivity, error) { return ScanActivity{}, nil }
	region.slowReads[0].Ms = 4336.8
	r := job.Result.(*ScanResult)
	if err := m.repairSlowRequests(job, &r.Map[0], h, source, 0, &region); err != nil {
		t.Fatal(err)
	}
	page, err := m.scanIssues(job.ID, 0, 50)
	if err != nil || len(page.Records) != 2 {
		t.Fatalf("repair journal: %+v / %v", page, err)
	}
	repair := page.Records[1].Repair
	if h.reads != 3 || h.writes.Load() != 1 || len(repair.SpeedChecks) != 2 || repair.BeforeRepeatMs == nil {
		t.Fatalf("missing pre-write read or post-write retests: %+v; reads=%d", repair, h.reads)
	}
	if repair.SpeedOutcome != "unconfirmed" || repair.Detail != "写回校验通过；修复前已恢复" || r.Stats.Repair.Improved != 0 {
		t.Fatalf("credited an already-fast range as repaired: %+v", repair)
	}
	row := reportIssue(page.Records[1], 512, 500)
	if row.BeforeRepair == "" || row.AfterLabel != "复测" || !strings.Contains(row.After, " / ") || row.Detail != "修复前已恢复" {
		t.Fatalf("report hid the retests: %+v", row)
	}
	legacy := reportIssue(BadRange{Outcome: "rewrite", Repair: &ScanRepairRecord{Outcome: "verified", BeforeMs: 4337, AfterMs: floatPtr(0.4)}}, 512, 500)
	if legacy.AfterLabel != "校验" || legacy.Detail != "复测未记录" {
		t.Fatalf("invented legacy retests: %+v", legacy)
	}
}
