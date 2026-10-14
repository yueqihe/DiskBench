package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Repair writes only successfully read original bytes, never a test pattern.
type RepairHandle interface {
	WriteHandle
	Sync() error
}

type ScanRepairRecord struct {
	Outcome      string   `json:"outcome"`
	BeforeMs     float64  `json:"beforeMs"`
	AfterMs      *float64 `json:"afterMs,omitempty"`
	WrittenBytes int64    `json:"writtenBytes"`
	BackupFile   string   `json:"backupFile,omitempty"`
	Detail       string   `json:"detail,omitempty"`
}

type ScanRepairStats struct {
	Candidates   int64 `json:"candidates"`
	Pending      int64 `json:"pending"`
	Verified     int64 `json:"verified"`
	StillSlow    int64 `json:"stillSlow"`
	Skipped      int64 `json:"skipped"`
	Failed       int64 `json:"failed"`
	WrittenBytes int64 `json:"writtenBytes"`
}

func parseScanRepairThreshold(value interface{}) (int64, bool) {
	var number float64
	switch value := value.(type) {
	case int:
		number = float64(value)
	case int64:
		number = float64(value)
	case float64:
		number = value
	case json.Number:
		var err error
		number, err = value.Float64()
		if err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || number != math.Trunc(number) || number < 1 || number > 60000 {
		return 0, false
	}
	return int64(number), true
}

func scanRepairThreshold(job *Job) float64 {
	if job.Options.ScanRepair == nil || !*job.Options.ScanRepair {
		return 0
	}
	if job.Options.ScanRepairAfterMs == nil {
		return 500
	}
	return float64(*job.Options.ScanRepairAfterMs)
}

func scanRepairBlockedReason(disk *Disk) string {
	if disk == nil || strings.TrimSpace(disk.Serial) == "" {
		return "无法可靠核对磁盘身份，不能执行慢扇区修复"
	}
	if disk.IsSystem {
		return "系统盘不能执行慢扇区修复"
	}
	if disk.ReadOnly {
		return "只读磁盘不能执行慢扇区修复"
	}
	if len(disk.Mountpoints) > 0 {
		return "请先卸载磁盘及其分区，再执行慢扇区修复"
	}
	if disk.LogicalSectorSize != 512 && disk.LogicalSectorSize != 4096 {
		return "慢扇区修复需要明确的 512 B 或 4 KiB 逻辑扇区信息"
	}
	var blockedFileSystems []string
	for _, fs := range disk.FileSystems {
		kind := strings.ToLower(fs)
		if strings.Contains(kind, "raid") || strings.Contains(kind, "lvm") || strings.Contains(kind, "luks") || strings.Contains(kind, "zfs") || strings.Contains(kind, "bcache") || kind == "swap" {
			blockedFileSystems = append(blockedFileSystems, strings.TrimSpace(fs))
		}
	}
	if len(blockedFileSystems) > 0 {
		return fmt.Sprintf("检测到 %s 标记，不能直接执行慢扇区修复", strings.Join(blockedFileSystems, "、"))
	}
	return ""
}

// The open itself can block. Cancellation keeps its late close accounted for.
func (m *JobManager) awaitOpenRepair(job *Job) (RepairHandle, error) {
	m.mu.Lock()
	cancel := m.cancels[job.ID]
	m.mu.Unlock()
	if cancel == nil {
		return m.openRepairFn(job.Device)
	}
	select {
	case <-cancel.ctx.Done():
		return nil, errCancelled
	default:
	}
	type result struct {
		handle RepairHandle
		err    error
	}
	ch := make(chan result, 1)
	cancel.pending.Add(1)
	go func() {
		defer cancel.pending.Done()
		h, err := m.openRepairFn(job.Device)
		ch <- result{h, err}
	}()
	select {
	case r := <-ch:
		return r.handle, r.err
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
	}
}

func applyScanRepairStats(stats *ScanRepairStats, record *ScanRepairRecord) {
	switch record.Outcome {
	case "pending":
		stats.Candidates++
		stats.Pending++
	case "skipped":
		stats.Candidates++
		stats.Skipped++
	case "verified", "still_slow", "failed":
		stats.Pending = max(0, stats.Pending-1)
		if record.Outcome == "verified" {
			stats.Verified++
		} else if record.Outcome == "still_slow" {
			stats.StillSlow++
		} else {
			stats.Failed++
		}
		stats.WrittenBytes += record.WrittenBytes
	}
}

func (m *JobManager) recordRepairEvent(job *Job, cell *ScanCell, read ScanReadLatency, record ScanRepairRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := BadRange{Offset: read.Offset, Length: int64(read.Bytes), Outcome: "rewrite", Code: "SLOW_REWRITE_" + strings.ToUpper(record.Outcome), Repair: &record}
	return m.recordScanIssuesLocked(job, cell, []BadRange{r})
}

// Keep original bytes after history deletion/pruning. Metadata identifies the
// disk and exact byte ranges; the UI/report never embeds disk content.
func (m *JobManager) saveRepairBackup(job *Job, read ScanReadLatency, original []byte) (string, error) {
	if !jobIDPattern.MatchString(job.ID) {
		return "", errors.New("修复任务 ID 无效")
	}
	root := filepath.Join(m.dataDir, "repair-backups")
	dir := filepath.Join(root, job.ID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	metadata, err := json.Marshal(struct {
		Version int          `json:"version"`
		JobID   string       `json:"jobId"`
		Device  string       `json:"device"`
		Disk    DiskSnapshot `json:"disk"`
		Options JobOptions   `json:"options"`
	}{1, job.ID, job.Device, job.Disk, job.Options})
	if err != nil {
		return "", err
	}
	writeOnce := func(path string, data []byte) error {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
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
		return err
	}
	metaPath := filepath.Join(dir, "metadata.json")
	if _, err := os.Stat(metaPath); os.IsNotExist(err) {
		if err := writeOnce(metaPath, metadata); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	name := fmt.Sprintf("%d-%d.bin", read.Offset, read.Bytes)
	if err := writeOnce(filepath.Join(dir, name), original); err != nil {
		return "", err
	}
	if err := syncRepairBackupDirectory(dir); err != nil {
		return "", err
	}
	if err := syncRepairBackupDirectory(root); err != nil {
		return "", err
	}
	if err := syncRepairBackupDirectory(m.dataDir); err != nil {
		return "", err
	}
	return name, nil
}

func (m *JobManager) repairSlowRequests(job *Job, cell *ScanCell, handle RepairHandle, source []byte, start int64, region readRegionResult) error {
	threshold := scanRepairThreshold(job)
	if threshold == 0 || handle == nil || region.errorCount != 0 || region.bytesRead != len(source) {
		return nil
	}
	for _, sample := range region.slowReads {
		if sample.Ms <= threshold {
			continue
		}
		sector := job.Disk.LogicalSectorSize
		index := sample.Offset - start
		if sector <= 0 || sample.Offset%sector != 0 || int64(sample.Bytes)%sector != 0 || sample.Bytes <= 0 || sample.Bytes > scanLatencyRequestBytes || index < 0 || index > int64(len(source)-sample.Bytes) {
			return errors.New("慢扇区修复范围未正确对齐，已停止")
		}
		current, err := m.assertDiskIdentity(job)
		if err != nil {
			return err
		}
		if reason := scanRepairBlockedReason(current); reason != "" {
			return newJobError(reason, "SCAN_REPAIR_NOT_ALLOWED", 409)
		}
		physicalSectorSize := current.PhysicalSectorSize
		if physicalSectorSize <= 0 {
			physicalSectorSize = current.LogicalSectorSize
		}
		if current.LogicalSectorSize != job.Disk.LogicalSectorSize || physicalSectorSize != job.Disk.PhysicalSectorSize {
			return errors.New("磁盘扇区格式已变化，停止慢扇区修复")
		}
		m.mu.Lock()
		job.Phase = fmt.Sprintf("备份并写回慢区 LBA %d（%.1f ms）", sample.Offset/sector, sample.Ms)
		m.mu.Unlock()
		original := scanBuffer(sample.Bytes)
		copy(original, source[int(index):int(index)+sample.Bytes])
		check := scanBuffer(sample.Bytes)
		n, err := m.readScanRequest(job, handle, check, sample.Offset)
		if err != nil || n != len(check) || !bytes.Equal(original, check) {
			detail := "复读不完整或数据与首次读取不同，跳过写入"
			if logErr := m.recordRepairEvent(job, cell, sample, ScanRepairRecord{Outcome: "skipped", BeforeMs: sample.Ms, Detail: detail}); logErr != nil {
				return logErr
			}
			if err != nil {
				return err
			}
			continue
		}
		backup, err := m.saveRepairBackup(job, sample, original)
		if err != nil {
			return fmt.Errorf("修复备份保存失败，未提交写入：%w", err)
		}
		pending := ScanRepairRecord{Outcome: "pending", BeforeMs: sample.Ms, BackupFile: backup}
		if err := m.recordRepairEvent(job, cell, sample, pending); err != nil {
			return err
		}
		if err := syncRepairBackupDirectory(m.jobsDir); err != nil {
			return fmt.Errorf("修复记录无法持久保存，未提交写入：%w", err)
		}
		// A durable intent and original-data backup must precede the write.
		written, writeErr := waitScanOperation(m, job, "写回", sample.Offset, sample.Bytes, func() (int, error) {
			n, err := handle.WriteAt(original, sample.Offset)
			if n < 0 || n > len(original) {
				return 0, errors.New("写回返回无效字节数")
			}
			if err == nil && n != len(original) {
				err = io.ErrShortWrite
			}
			if err != nil {
				return n, err
			}
			return n, handle.Sync()
		})
		final := pending
		final.Outcome, final.WrittenBytes = "failed", int64(written)
		if writeErr == nil {
			verify := scanBuffer(sample.Bytes)
			began := time.Now()
			n, readErr := m.readScanRequest(job, handle, verify, sample.Offset)
			elapsed := float64(time.Since(began).Nanoseconds()) / 1e6
			final.AfterMs = &elapsed
			if readErr != nil {
				writeErr = readErr
			} else if n != len(verify) || !bytes.Equal(original, verify) {
				writeErr = errors.New("写回后读回校验失败，原始数据备份已保留")
			} else {
				final.Outcome = "verified"
				if elapsed > threshold {
					final.Outcome = "still_slow"
				}
			}
		}
		if writeErr != nil {
			final.Detail = writeErr.Error()
			if errors.Is(writeErr, errCancelled) || errors.Is(writeErr, errScanWaitTimeout) {
				// Leave the durable intent pending: interruption cannot establish
				// whether the device finished a write or persisted its contents.
				final.Outcome = "uncertain"
				final.Detail += "；写回可能仍在执行，最终写回结果无法确定，原始备份已保留"
			}
		}
		if err := m.recordRepairEvent(job, cell, sample, final); err != nil {
			return err
		}
		if writeErr != nil {
			return writeErr
		}
	}
	return nil
}
