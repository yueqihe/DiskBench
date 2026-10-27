//go:build linux

package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLinuxScanOpenIsReadOnlyAndDirect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scan-fixture")
	if err := os.WriteFile(path, make([]byte, 8192), 0600); err != nil {
		t.Fatal(err)
	}
	h, err := openScanDevice(path)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	file := h.(*os.File)
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, file.Fd(), syscall.F_GETFL, 0)
	if errno != 0 || flags&syscall.O_DIRECT == 0 || flags&syscall.O_ACCMODE != syscall.O_RDONLY {
		t.Fatalf("flags=%x errno=%v", flags, errno)
	}
	if n, err := h.ReadAt(scanBuffer(4096), 4096); err != nil || n != 4096 {
		t.Fatalf("aligned read n=%d err=%v", n, err)
	}
}

func TestLinuxAllScanMediaUseBlockReads(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		rotational bool
	}{
		{"HDD", "/dev/sda", true},
		{"SATA-SSD", "/dev/sdb", false},
		{"NVMe", "/dev/nvme0n1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := make([]byte, 16384)
			data[4096], data[8192] = 0x5a, 0xa5
			path := filepath.Join(t.TempDir(), "block-read-fixture")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			file, err := openScanDevice(path)
			if err != nil {
				t.Fatal(err)
			}
			h := configureScanReader(file, Disk{Path: tc.path}, DiskSnapshot{Rotational: tc.rotational, LogicalSectorSize: 512})
			defer func() {
				_ = h.Close()
				select {
				case <-h.(interface{ CloseDone() <-chan struct{} }).CloseDone():
				case <-time.After(time.Second):
					t.Error("device close did not finish")
				}
			}()
			// A regular file has no protocol ioctl. NVMe Identify is optional
			// for data reads and its unavailability must not stop this scan.
			if err := h.(interface{ prepare() error }).prepare(); err != nil {
				t.Fatal(err)
			}
			for _, off := range []int64{4096, 8192} {
				b := scanBuffer(4096)
				if n, err := h.ReadAt(b, off); err != nil || n != len(b) || b[0] != data[off] {
					t.Fatalf("block read at %d: n=%d err=%v data=%x", off, n, err, b[0])
				}
			}
			method, note := h.(interface{ scanReadInfo() (string, string) }).scanReadInfo()
			if method != "direct" || !strings.Contains(note, "块 I/O") {
				t.Fatalf("wrong read method: %s %s", method, note)
			}
			if n, err := h.ReadAt(scanBuffer(4096), 16384); n != 0 || !errors.Is(err, io.EOF) {
				t.Fatalf("out-of-range read masked: n=%d err=%v", n, err)
			}
			// Some regular-file filesystems (including tmpfs) accept an
			// unaligned O_DIRECT read. When rejected, it must remain an
			// alignment error rather than a recoverable media error.
			if _, err := h.ReadAt(scanBuffer(4096), 1); err != nil && (!errors.Is(err, syscall.EINVAL) || isRecoverableMediaError(err)) {
				t.Fatalf("alignment error treated as media error: %v", err)
			}
			_ = h.Close()
			if _, err := h.ReadAt(scanBuffer(4096), 0); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("read continued after close: %v", err)
			}
		})
	}
}

func TestLinuxNVMeWithAdminSupportStillUsesBlockReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nvme-block-read-fixture")
	if err := os.WriteFile(path, make([]byte, 8192), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := openScanDevice(path)
	if err != nil {
		t.Fatal(err)
	}
	h := configureScanReader(file, Disk{Path: "/dev/nvme0n1"}, DiskSnapshot{}).(*nvmeScanHandle)
	defer func() { _ = h.Close(); <-h.CloseDone() }()
	// Model a controller whose admin capability was already queried. A data
	// ioctl against this regular file would fail and clear that capability.
	h.once.Do(func() {
		h.adminReady = true
		h.info = NVMeScanInfo{NSID: 7, SelfTestSupported: true, MaxReadBytes: 4096}
	})
	if n, err := h.ReadAt(scanBuffer(8192), 0); err != nil || n != 8192 {
		t.Fatalf("NVMe block read: n=%d err=%v", n, err)
	}
	if !h.adminReady || !h.nvmeInfo().SelfTestSupported {
		t.Fatal("data reading interfered with firmware self-test support")
	}
	var _ nvmeSelfTester = h
}
