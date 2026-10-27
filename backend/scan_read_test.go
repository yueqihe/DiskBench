package main

import (
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

type scanReaderFunc func([]byte, int64) (int, error)

func (f scanReaderFunc) ReadAt(p []byte, offset int64) (int, error) { return f(p, offset) }
func (f scanReaderFunc) Close() error                               { return nil }

// Function values cannot be map keys, unlike the production file handles.
type scanTestHandle struct{ read scanReaderFunc }

func (h *scanTestHandle) ReadAt(p []byte, offset int64) (int, error) { return h.read(p, offset) }
func (h *scanTestHandle) Close() error                               { return nil }

func TestScanRealErrnoAndAlignedBuffer(t *testing.T) {
	if !isRecoverableMediaError(&os.PathError{Op: "read", Err: syscall.EIO}) {
		t.Fatal("real EIO not recognized")
	}
	if isRecoverableMediaError(&os.PathError{Op: "read", Err: syscall.EINVAL}) {
		t.Fatal("alignment error treated as bad sector")
	}
	for _, size := range []int{512, 4096, fullScanReadSize} {
		b := scanBuffer(size)
		if len(b) != size || uintptr(unsafe.Pointer(&b[0]))%4096 != 0 {
			t.Fatal("unaligned direct I/O buffer")
		}
	}
}

func TestScanBlockIOErrorLocatesLargeDiskAnd4KnAddresses(t *testing.T) {
	for _, sector := range []int64{512, 4096} {
		m := newTestManager(t, JobManagerOptions{})
		job := &Job{ID: "block-io-location", Status: "running"}
		const start = int64(3 * 1024 * 1024 * 1024 * 1024)
		bad := start + 32768
		h := scanReaderFunc(func(p []byte, offset int64) (int, error) {
			if offset%sector != 0 || int64(len(p))%sector != 0 {
				t.Fatalf("unaligned probe: offset=%d bytes=%d sector=%d", offset, len(p), sector)
			}
			if offset <= bad && offset+int64(len(p)) > bad {
				return 0, &os.PathError{Op: "read", Path: "/dev/test", Err: syscall.EIO}
			}
			return len(p), nil
		})
		ranges, read, err := m.locateReadErrors(h, start, 65536, job, 200, sector)
		if err != nil || len(ranges) != 1 {
			t.Fatalf("sector=%d ranges=%+v err=%v", sector, ranges, err)
		}
		r := ranges[0]
		if r.Offset != bad || r.Length != sector || r.Outcome != "unreadable" || r.Code != "EIO" || r.Recovered == nil || *r.Recovered || read != 65536-int(sector) {
			t.Fatalf("incorrect block I/O localization: sector=%d range=%+v read=%d", sector, r, read)
		}
		if r.Evidence != nil {
			t.Fatal("ordinary EIO fabricated a device protocol response")
		}
	}
}

func TestScanHierarchicalLocationAndTransientRecovery(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := &Job{ID: "locate", Status: "running"}
	for _, recovered := range []bool{false, true} {
		calls, leafReads := 0, 0
		h := scanReaderFunc(func(p []byte, offset int64) (int, error) {
			calls++
			if offset <= 32768 && offset+int64(len(p)) > 32768 {
				if len(p) == 512 {
					leafReads++
				}
				if !recovered || leafReads < 2 {
					return 0, syscall.EIO
				}
			}
			return len(p), nil
		})
		ranges, count, err := m.locateReadErrors(h, 0, 65536, job, 200, 512)
		if err != nil || len(ranges) != 1 || ranges[0].Offset != 32768 || ranges[0].Length != 512 || ranges[0].Recovered == nil || *ranges[0].Recovered != recovered {
			t.Fatalf("recovered=%v: ranges=%+v err=%v", recovered, ranges, err)
		}
		want := 65536 - 512
		if recovered {
			want = 65536
		}
		if count != want || calls > 20 {
			t.Fatalf("count=%d calls=%d", count, calls)
		}
	}
}

func TestScanFatalErrorsStopLocation(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := &Job{ID: "fatal", Status: "running"}
	h := scanReaderFunc(func([]byte, int64) (int, error) { return 0, syscall.EINVAL })
	if _, _, err := m.locateReadErrors(h, 0, 4096, job, 200, 512); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("lost fatal error: %v", err)
	}
	if _, err := m.readRegion(h, scanBuffer(4096), 4096, 0, job, 200, 512); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("lost primary error: %v", err)
	}
	job.Status = "cancelling"
	if _, _, err := m.locateReadErrors(h, 0, 4096, job, 200, 512); err != errCancelled {
		t.Fatalf("lost cancellation: %v", err)
	}
}

func TestScanPartialReadAndRecoveredPrimaryError(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := &Job{ID: "partial", Status: "running"}
	for _, primaryErr := range []error{io.EOF, syscall.EIO} {
		calls := 0
		h := scanReaderFunc(func(p []byte, offset int64) (int, error) {
			calls++
			if calls == 1 {
				return 512, primaryErr
			}
			if offset < 512 {
				t.Fatal("discarded successful prefix")
			}
			return len(p), nil
		})
		r, err := m.readRegion(h, scanBuffer(4096), 4096, 0, job, 200, 512)
		if err != nil || r.bytesRead != 4096 || r.errorCount == 0 || len(r.badRanges) != 1 || r.badRanges[0].Recovered == nil || !*r.badRanges[0].Recovered {
			t.Fatalf("%v: result=%+v err=%v", primaryErr, r, err)
		}
	}
}

func TestSlowRecheckReadsWholeSuspiciousSpan(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := &Job{ID: "recheck", Status: "running"}
	const start = int64(8 * 1024 * 1024)
	const length = 2*1024*1024 + 512
	position := start
	h := scanReaderFunc(func(p []byte, off int64) (int, error) {
		if off != position || len(p) > fastSampleSize {
			t.Fatal("recheck skipped range or used large read")
		}
		position += int64(len(p))
		return len(p), nil
	})
	r, ranges, err := m.recheckSlowRead(h, start, length, job, 200, 512)
	if err != nil || len(ranges) != 0 || r.BytesRead != length || r.Reads != 3 || r.Outcome != "not_reproduced" || position != start+length {
		t.Fatalf("recheck=%+v ranges=%v err=%v", r, ranges, err)
	}
}

func TestRecoveredShortReadDetailAndFullCountError(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := &Job{ID: "recovery-detail", Status: "running"}
	for _, fullCountError := range []bool{false, true} {
		calls := 0
		h := scanReaderFunc(func(p []byte, off int64) (int, error) {
			calls++
			if fullCountError && calls == 1 {
				return len(p), syscall.EIO
			}
			if !fullCountError && calls <= maxShortReadRetries+1 {
				return 0, nil
			}
			return len(p), nil
		})
		r, err := m.readRegion(h, scanBuffer(4096), 4096, 0, job, 200, 512)
		if err != nil || calls < 2 || r.bytesRead != 4096 || r.errorCount == 0 || len(r.badRanges) == 0 || r.badRanges[0].Recovered == nil || !*r.badRanges[0].Recovered {
			t.Fatalf("fullCountError=%v calls=%d result=%+v err=%v", fullCountError, calls, r, err)
		}
	}
}

func TestSlowRecheckKeepsErrorsEvenWhenDetailsFull(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := &Job{ID: "recheck-error", Status: "running"}
	h := &failingSectorReadHandle{badStart: 512, badLength: 512}
	r, ranges, err := m.recheckSlowRead(h, 0, 4096, job, 0, 512)
	if err != nil || r.Outcome != "read_issue" || r.ErrorCount == 0 || len(ranges) != 0 || r.BytesRead != 3584 {
		t.Fatalf("%+v %v %v", r, ranges, err)
	}
}

func TestFullScanRechecksWithoutDoubleCountingCoverage(t *testing.T) {
	disk := testDisk()
	disk.Size = 16 * 1024 * 1024
	calls, evidenceCalls := 0, 0
	var offsets []int64
	h := &scanTestHandle{read: func(p []byte, offset int64) (int, error) {
		calls++
		offsets = append(offsets, offset)
		if calls == 1 {
			time.Sleep(410 * time.Millisecond)
		}
		return len(p), nil
	}}
	m := newTestManager(t, JobManagerOptions{
		ResolveDisk: func(string) (*Disk, error) { return disk, nil },
		Open:        func(string) (ReadHandle, error) { return h, nil },
		ScanEvidence: func(Disk) ScanEvidence {
			evidenceCalls++
			return ScanEvidence{Available: true, Status: "passed", Counters: map[string]int64{"pending": int64(evidenceCalls - 1)}}
		},
	})
	job := &Job{ID: "12345678-1234-4123-8123-123456789abc", Type: "scan", Status: "queued", Device: disk.Path,
		Disk:   DiskSnapshot{Size: disk.Size, Model: disk.Model, Serial: disk.Serial, LogicalSectorSize: 512},
		Result: &ScanResult{ScanMode: "full", Coverage: "full", Map: createScanMap(disk.Size, 2, 512)},
	}
	m.jobs[job.ID] = job
	if err := m.runScan(job, "full"); err != nil {
		t.Fatal(err)
	}
	r := job.Result.(*ScanResult)
	requestsPerRegion := fullScanReadSize / scanLatencyRequestBytes
	if job.Status != "completed" || calls != 2*requestsPerRegion+2 || evidenceCalls != 2 || r.Stats.BytesRead != disk.Size {
		t.Fatalf("calls=%d evidence=%d result=%+v", calls, evidenceCalls, r.Stats)
	}
	for i, offset := range offsets {
		want := int64(i * scanLatencyRequestBytes)
		if i >= requestsPerRegion && i < requestsPerRegion+2 {
			want = int64((i - requestsPerRegion) * scanLatencyRequestBytes)
		} else if i >= requestsPerRegion+2 {
			want = int64((i - 2) * scanLatencyRequestBytes)
		}
		if offset != want {
			t.Fatalf("full coverage/recheck skipped: request %d offset %d want %d", i, offset, want)
		}
	}
	if r.Map[0].Recheck == nil || r.Map[0].Recheck.BytesRead != 2*scanLatencyRequestBytes || r.Map[0].Status != "slower" || r.Map[0].MaxReadLatency == nil || r.Map[0].MaxReadLatency.Ms < 400 {
		t.Fatalf("lost original slow verdict: %+v", r.Map[0])
	}
	if len(r.Diagnostics.Increased) != 1 || r.Diagnostics.Increased[0] != "pending" || !strings.Contains(job.Message, "计数增长") {
		t.Fatalf("diagnostics=%+v message=%s", r.Diagnostics, job.Message)
	}
	progress, err := m.scanProgress(job.ID, 0)
	if err != nil || progress.Diagnostics == nil || progress.Cells[0].Recheck == nil {
		t.Fatalf("missing progress diagnostics: %v", err)
	}
}
