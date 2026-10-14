//go:build linux

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
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

func TestLinuxHDDUnsupportedProtocolFallsBackToDirectRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-scsi-device")
	data := make([]byte, 8192)
	data[4096] = 0x5a
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	file, err := openScanDevice(path)
	if err != nil {
		t.Fatal(err)
	}
	h := configureHDDScan(file, DiskSnapshot{Rotational: true, LogicalSectorSize: 512})
	defer h.Close()
	b := scanBuffer(4096)
	if n, err := h.ReadAt(b, 4096); err != nil || n != 4096 || b[0] != 0x5a {
		t.Fatalf("fallback read offset: %d %v", n, err)
	}
	method, note := h.(*hddScanHandle).scanReadInfo()
	if method != "direct" || note == "" {
		t.Fatal("fallback not disclosed")
	}
}
