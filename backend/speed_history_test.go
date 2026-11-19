package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSpeedHistorySummaryIncludesRatesWithoutSamples(t *testing.T) {
	benchmarks := []BenchmarkResult{
		{Operation: "read", Pattern: "sequential", BlockSize: 1048576, QueueDepth: 64, Threads: 1, AverageMBps: 1234.5,
			Samples: []SpeedSample{{Label: "sample-must-stay-in-detail", Bytes: 1048576}}},
		{Operation: "write", Pattern: "sequential", BlockSize: 1048576, QueueDepth: 64, Threads: 1, AverageMBps: 987.6},
	}
	completed := &SpeedResult{SampleMiB: 1024, Runs: 1, Benchmarks: benchmarks}
	encoded, err := json.Marshal(completed)
	if err != nil {
		t.Fatal(err)
	}
	var reloaded interface{}
	if err := json.Unmarshal(encoded, &reloaded); err != nil {
		t.Fatal(err)
	}
	for name, result := range map[string]interface{}{
		"completed": completed, "partial": &speedInProgress{Benchmarks: benchmarks}, "reloaded": reloaded,
	} {
		t.Run(name, func(t *testing.T) {
			job := &Job{ID: "record", Type: "speed", Status: "completed", Result: result}
			m := &JobManager{jobs: map[string]*Job{job.ID: job}}
			listed := m.list(true)
			if len(listed) != 1 {
				t.Fatalf("missing history: %+v", listed)
			}
			summary := listed[0].(map[string]interface{})
			if _, exists := summary["result"]; exists {
				t.Fatal("full result leaked into summary")
			}
			speed, ok := summary["speedSummary"].(speedHistorySummary)
			if !ok || len(speed.Benchmarks) != 2 || speed.Benchmarks[0].AverageMBps == nil ||
				*speed.Benchmarks[0].AverageMBps != 1234.5 || *speed.Benchmarks[1].AverageMBps != 987.6 ||
				speed.Benchmarks[0].QueueDepth != 64 || speed.Benchmarks[0].BlockSize != 1048576 {
				t.Fatalf("rates or test parameters missing: %+v", speed)
			}
			wire, err := json.Marshal(summary)
			if err != nil || strings.Contains(string(wire), "samples") || strings.Contains(string(wire), "sample-must-stay-in-detail") {
				t.Fatalf("summary contains detailed samples: %s, %v", wire, err)
			}
			if m.get(job.ID).Result == nil {
				t.Fatal("summarizing removed the full result")
			}
		})
	}
}

func TestSpeedHistorySummaryDoesNotInventMissingMeasurements(t *testing.T) {
	for _, job := range []*Job{
		{Type: "scan", Result: &SpeedResult{Benchmarks: []BenchmarkResult{{AverageMBps: 1}}}},
		{Type: "speed"},
		{Type: "speed", Result: &SpeedResult{}},
	} {
		if _, exists := summarizeJobJSON(job)["speedSummary"]; exists {
			t.Fatal("non-speed or empty result received speed measurements")
		}
	}
	job := &Job{Type: "speed", Result: map[string]interface{}{"benchmarks": []interface{}{
		map[string]interface{}{"operation": "read", "pattern": "sequential"},
	}}}
	speed := summarizeJobJSON(job)["speedSummary"].(speedHistorySummary)
	if speed.Benchmarks[0].AverageMBps != nil {
		t.Fatal("a missing rate became a zero measurement")
	}
}
