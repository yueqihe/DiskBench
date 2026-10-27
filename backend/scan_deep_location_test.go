package main

import (
	"errors"
	"syscall"
	"testing"
	"time"
)

func TestDeepLocalizationCanLocateSectorAfterPrimarySlowLimit(t *testing.T) {
	for _, deep := range []bool{false, true} {
		t.Run(map[bool]string{false: "bounded-scan", true: "deep-location"}[deep], func(t *testing.T) {
			m := newTestManager(t, JobManagerOptions{})
			job := &Job{ID: newUUID(), Status: "running", Options: JobOptions{ScanDeepLocate: boolPtr(deep)}}
			const size, bad = 128 * 1024, int64(73 * 512)
			clock, calls := time.Now(), 0
			h := newScanSlowBudgetReader(scanReaderFunc(func(p []byte, offset int64) (int, error) {
				calls++
				if offset <= bad && offset+int64(len(p)) > bad {
					clock = clock.Add(6 * time.Second)
					return 0, syscall.EIO
				}
				return len(p), nil
			}), true)
			h.now = func() time.Time { return clock }
			read, err := m.readRegion(h, scanBuffer(size), size, 0, job, -1, 512)
			if err != nil || !h.exhausted() {
				t.Fatalf("read failed or localization waits were not charged: %+v / %v", read, err)
			}
			if deep {
				if calls <= 1 || read.bytesRead != size-512 || sumUnreadableBytes(read.badRanges) != 512 || sumUnresolvedProbeBytes(read.badRanges) != 0 {
					t.Fatalf("deep location was blocked by the primary budget: calls=%d, %+v", calls, read)
				}
				if len(read.badRanges) != 1 || read.badRanges[0].Offset != bad || scanRangePrecision(read.badRanges[0], 512) != "sector" {
					t.Fatalf("did not confirm exact bad LBA %d: %+v", bad/512, read.badRanges)
				}
			} else {
				if calls != 1 || read.bytesRead != 0 || sumUnreadableBytes(read.badRanges) != 0 || sumUnresolvedProbeBytes(read.badRanges) != size {
					t.Fatalf("normal mode exceeded its wait limit or invented coverage: calls=%d, %+v", calls, read)
				}
				found := false
				for _, issue := range read.badRanges {
					if issue.Code == "EIO" && issue.Outcome == "unresolved" && issue.Offset == 0 && issue.Length == size && issue.UnresolvedBytes == 0 {
						found = true
					}
				}
				if !found {
					t.Fatal("original I/O failure disappeared behind the skip record")
				}
			}
		})
	}
}

func TestDeepLocalizationRetainsItsRequestLimit(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := &Job{ID: newUUID(), Status: "running", Options: JobOptions{ScanDeepLocate: boolPtr(true)}}
	clock, calls := time.Now(), 0
	h := newScanSlowBudgetReader(scanReaderFunc(func(p []byte, offset int64) (int, error) {
		calls++
		clock = clock.Add(6 * time.Second)
		return 0, syscall.EIO
	}), true)
	h.now = func() time.Time { return clock }
	read, err := m.readRegion(h, scanBuffer(fullScanReadSize), fullScanReadSize, 0, job, -1, 512)
	if err != nil || calls != 4097 || sumUnresolvedProbeBytes(read.badRanges) <= 0 {
		t.Fatalf("deep limit missing: calls=%d, %+v / %v", calls, read, err)
	}
	if int64(read.bytesRead)+sumUnreadableBytes(read.badRanges)+sumUnresolvedProbeBytes(read.badRanges) != fullScanReadSize {
		t.Fatal("deep localization lost or duplicated coverage")
	}
	if _, err := h.ReadAt(scanBuffer(512), fullScanReadSize); !errors.Is(err, errScanSlowBudget) || calls != 4097 {
		t.Fatal("deep location reset the main region budget")
	}
}

func TestUnfinishedLocalizationKeepsActualFailedRequestAddress(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := &Job{ID: newUUID(), Status: "running"}
	clock := time.Now()
	const start = int64(3 << 40)
	h := newScanSlowBudgetReader(scanReaderFunc(func(p []byte, offset int64) (int, error) {
		if offset == start {
			return len(p), nil
		}
		clock = clock.Add(6 * time.Second)
		return 0, syscall.EIO
	}), true)
	h.now = func() time.Time { return clock }
	read, err := m.readRegion(h, scanBuffer(fullScanReadSize), fullScanReadSize, start, job, -1, 512)
	if err != nil || read.bytesRead != scanLatencyRequestBytes || sumUnreadableBytes(read.badRanges) != 0 {
		t.Fatalf("wrong readable prefix or false unreadable sectors: %+v / %v", read, err)
	}
	found := false
	for _, issue := range read.badRanges {
		if issue.Code == "EIO" {
			found = issue.Offset == start+scanLatencyRequestBytes && issue.Length == scanLatencyRequestBytes &&
				issue.Outcome == "unresolved" && issue.UnresolvedBytes == 0
			if !validScanJournalRecord(issue, start+fullScanReadSize) {
				t.Fatal("original error cannot be saved to the journal")
			}
		}
	}
	if !found || int64(read.bytesRead)+sumUnresolvedProbeBytes(read.badRanges) != fullScanReadSize {
		t.Fatalf("failed request address was lost or overlap counted twice: %+v", read)
	}
}
