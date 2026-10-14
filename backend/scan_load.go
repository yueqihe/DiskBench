package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type ScanActivity struct {
	ReadBytes  uint64
	WriteBytes uint64
	InFlight   int64
}

type ScanLoad struct {
	BackgroundActivity bool  `json:"backgroundActivity"`
	OtherReadBytes     int64 `json:"otherReadBytes"`
	WriteBytes         int64 `json:"writeBytes"`
	InFlight           int64 `json:"inFlight"`
}

func parseScanActivity(data string) (ScanActivity, error) {
	f := strings.Fields(data)
	if len(f) < 11 {
		return ScanActivity{}, fmt.Errorf("磁盘活动统计不完整")
	}
	values := make([]uint64, 3)
	for i, index := range []int{2, 6, 8} {
		n, err := strconv.ParseUint(f[index], 10, 64)
		if err != nil || n > uint64(1<<63-1)/512 {
			return ScanActivity{}, fmt.Errorf("磁盘活动统计无效")
		}
		values[i] = n
	}
	// Linux diskstats sectors are always 512 bytes, including 4Kn devices.
	return ScanActivity{ReadBytes: values[0] * 512, WriteBytes: values[1] * 512, InFlight: int64(values[2])}, nil
}

type scanLoadMonitor struct {
	read     func(string) (ScanActivity, error)
	path     string
	previous ScanActivity
	at       time.Time
	valid    bool
	ownBytes int64
}

func (m *JobManager) newScanLoadMonitor(path string) *scanLoadMonitor {
	l := &scanLoadMonitor{read: m.scanActivityFn, path: path}
	l.reset()
	return l
}

func (l *scanLoadMonitor) reset() {
	current, err := l.read(l.path)
	l.previous, l.at, l.valid, l.ownBytes = current, time.Now(), err == nil, 0
}

func (l *scanLoadMonitor) observe(ownBytes int64, force bool) *ScanLoad {
	l.ownBytes += ownBytes
	if !force && time.Since(l.at) < time.Second {
		return nil
	}
	current, err := l.read(l.path)
	if err != nil {
		l.at, l.valid, l.ownBytes = time.Now(), false, 0
		return nil
	}
	previous, own := l.previous, l.ownBytes
	valid := l.valid && current.ReadBytes >= previous.ReadBytes && current.WriteBytes >= previous.WriteBytes
	l.previous, l.at, l.valid, l.ownBytes = current, time.Now(), true, 0
	if !valid {
		return nil
	}
	load := &ScanLoad{OtherReadBytes: max(0, int64(current.ReadBytes-previous.ReadBytes)-own),
		WriteBytes: int64(current.WriteBytes - previous.WriteBytes), InFlight: current.InFlight}
	// This is evidence of concurrent work, not proof of what caused a stall.
	// Passthrough I/O may bypass these counters, so read activity is conservative.
	load.BackgroundActivity = load.OtherReadBytes >= 1024*1024 || load.WriteBytes >= 64*1024 || load.InFlight > 0
	return load
}

func (m *JobManager) waitScanQuiet(job *Job, load *ScanLoad) error {
	if load == nil || !load.BackgroundActivity {
		return nil
	}
	// Give foreground work a bounded chance to finish before confirming a stall.
	for i := 0; i < 10; i++ {
		if _, err := awaitCancelable(m, job, func() (struct{}, error) {
			m.delayFn(200 * time.Millisecond)
			return struct{}{}, nil
		}); err != nil {
			return err
		}
		activity, err := m.scanActivityFn(job.Device)
		if err != nil || activity.InFlight == 0 {
			return nil
		}
	}
	return nil
}
