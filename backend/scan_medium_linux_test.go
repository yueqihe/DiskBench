//go:build linux

package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestLinuxBlockMediumErrorClassification(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EIO, syscall.EREMOTEIO, syscall.ENODATA} {
		err := fmt.Errorf("block read: %w", &os.PathError{Op: "read", Path: "/dev/test", Err: errno})
		if !isRecoverableMediaError(err) {
			t.Errorf("block error %v would abort the scan instead of locating the failure", errno)
		}
	}
	for _, errno := range []syscall.Errno{syscall.EINVAL, syscall.EPERM, syscall.EBADF, syscall.ENODEV, syscall.ENXIO} {
		if isRecoverableMediaError(&os.PathError{Op: "read", Err: errno}) {
			t.Errorf("non-media error %v accepted as a localizable media failure", errno)
		}
	}
	r := classifyScanRange(scanIssueRange(1024, 512, &os.PathError{Op: "read", Err: syscall.ENODATA}, boolPtr(true)))
	if r.Code != "MEDIUM_ERROR" || r.Outcome != "recovered" || r.Evidence != nil || !strings.Contains(r.Detail, "ENODATA") {
		t.Fatalf("block-layer medium error lost its cause or fabricated protocol evidence: %+v", r)
	}
}

func TestLinuxMediumErrorScanContinuesAndPersists(t *testing.T) {
	for _, mode := range []string{"quick", "full"} {
		for _, sector := range []int64{512, 4096} {
			for _, transient := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%d/transient=%t", mode, sector, transient), func(t *testing.T) {
					disk := testDisk()
					disk.Size, disk.LogicalSectorSize, disk.PhysicalSectorSize, disk.Rotational = 3*fullScanReadSize, sector, 4096, true
					r := &ScanResult{Version: 2, ScanMode: mode, Coverage: "full", Map: createScanMap(disk.Size, 3, sector)}
					var targets []ScanSampleRange
					targetBytes := disk.Size
					if mode == "quick" {
						r.Coverage, r.SamplingPlan, r.SamplingSeed, r.SampleBytes = "sampled", stratifiedScanPlan, 3246593705, intPtr(fastSampleSize)
						for i := range r.Map {
							targets = append(targets, quickSampleRanges(r, &r.Map[i], disk.Size, sector, fastSampleSize)...)
						}
						targetBytes = 3 * fastSampleSize
					} else {
						for _, cell := range r.Map {
							targets = append(targets, ScanSampleRange{Start: cell.Start, End: cell.End})
						}
					}
					bad := targets[len(targets)/2].Start + 17*sector
					failures, laterReads := 0, 0
					h := &scanTestHandle{read: func(p []byte, offset int64) (int, error) {
						inside := false
						for _, span := range targets {
							inside = inside || offset >= span.Start && offset+int64(len(p)) <= span.End
						}
						if !inside || offset%sector != 0 || int64(len(p))%sector != 0 {
							t.Errorf("read escaped planned/aligned ranges: offset=%d length=%d", offset, len(p))
							return 0, syscall.EINVAL
						}
						if offset >= r.Map[2].Start {
							laterReads++
						}
						if offset <= bad && offset+int64(len(p)) > bad && (!transient || failures == 0) {
							failures++
							return 0, &os.PathError{Op: "read", Path: disk.Path, Err: syscall.ENODATA}
						}
						return len(p), nil
					}}
					m := newTestManager(t, JobManagerOptions{
						ResolveDisk:  func(string) (*Disk, error) { return disk, nil },
						Open:         func(string) (ReadHandle, error) { return h, nil },
						ScanEvidence: func(Disk) ScanEvidence { return ScanEvidence{} },
					})
					job := &Job{ID: newUUID(), Type: "scan", Status: "queued", Device: disk.Path,
						Disk:   DiskSnapshot{Size: disk.Size, Model: disk.Model, Serial: disk.Serial, Rotational: true, LogicalSectorSize: sector, PhysicalSectorSize: 4096},
						Result: r, Options: JobOptions{ScanMode: &mode, ScanExactSectors: boolPtr(true)}}
					m.jobs[job.ID] = job
					if err := m.runScan(job, mode); err != nil {
						t.Fatalf("media error aborted %s scan: %v", mode, err)
					}
					wantUnreadable := sector
					if transient {
						wantUnreadable = 0
					}
					if job.Status != "completed" || job.Progress != 100 || laterReads == 0 || failures == 0 || r.Stats.TestedCells != 3 ||
						r.Stats.BytesRead != targetBytes-wantUnreadable || r.Stats.CoveredBytes != targetBytes || r.Stats.UnreadableBytes != wantUnreadable || r.Stats.UnresolvedBytes != 0 {
						t.Fatalf("continuation or byte accounting failed: status=%s laterReads=%d stats=%+v", job.Status, laterReads, r.Stats)
					}
					for i := range r.Map {
						if !scanCellComplete(r, &r.Map[i]) {
							t.Fatalf("cell %d did not finish: %+v", i, r.Map[i])
						}
					}
					restored := NewJobManager(m.dataDir, JobManagerOptions{Mock: true})
					loaded := restored.get(job.ID)
					if loaded == nil || loaded.Status != "completed" || loaded.Result.(*ScanResult).Stats.UnreadableBytes != wantUnreadable {
						t.Fatal("restart lost the scan result")
					}
					page, err := restored.scanIssueRanges(job.ID, 0, 50)
					if err != nil || !page.Complete || page.Total == 0 {
						t.Fatalf("missing saved medium-error details: %+v / %v", page, err)
					}
					found := false
					for _, issue := range page.Records {
						if issue.Code != "MEDIUM_ERROR" || issue.Evidence != nil || !validScanJournalRecord(issue, disk.Size) {
							t.Fatalf("invalid or fabricated medium-error evidence: %+v", issue)
						}
						if transient {
							found = found || issue.Outcome == "recovered" && issue.Recovered != nil && *issue.Recovered
						} else {
							found = found || issue.Offset == bad && issue.Length == sector && issue.Outcome == "unreadable" && scanRangePrecision(issue, sector) == "sector"
						}
					}
					if !found {
						t.Fatalf("lost exact bad LBA or transient recovery: %+v", page.Records)
					}
					if body := renderHumanReport(t, restored, loaded); !strings.Contains(body, "介质读取错误") {
						t.Fatal("human report hides the medium-error cause")
					}
				})
			}
		}
	}
}
