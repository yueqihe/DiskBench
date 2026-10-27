//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

func openScanDevice(path string) (ReadHandle, error) {
	h, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECT, 0)
	if err != nil {
		return nil, fmt.Errorf("无法开启直接读取扫描（不会降级为缓存读取）：%w", err)
	}
	return h, nil
}

// Data reads go through the block layer, including its I/O accounting and
// driver error recovery. O_DIRECT bypasses the page cache, not device caches.
const scanDirectReadNote = "块设备 O_DIRECT 只读扫描，绕过系统页缓存，读取进入 Linux 块 I/O 路径；错误位置通过细分复读确认，普通 EIO 无法细分介质、连接或控制器故障"

type blockScanHandle struct {
	file      *os.File
	snapshot  DiskSnapshot
	once      sync.Once
	initErr   error
	closed    atomic.Bool
	closeOnce sync.Once
	closeDone chan struct{}
}

func configureScanReader(handle ReadHandle, disk Disk, snapshot DiskSnapshot) ReadHandle {
	file, ok := handle.(*os.File)
	if !ok {
		return handle
	}
	block := &blockScanHandle{file: file, snapshot: snapshot, closeDone: make(chan struct{})}
	if isNativeNVMe(disk) {
		return &nvmeScanHandle{blockScanHandle: block, disk: disk}
	}
	return block
}

func (h *blockScanHandle) prepare() error {
	h.once.Do(func() {
		stat, err := h.file.Stat()
		if err != nil {
			h.initErr = err
			return
		}
		// Regular files are used by scanner tests. Real scans resolve and verify
		// the physical disk identity before this handle is created.
		if stat.Mode()&os.ModeDevice == 0 {
			return
		}
		var sector uint32
		var size uint64
		raw, err := h.file.SyscallConn()
		if err == nil {
			var queryErr syscall.Errno
			err = raw.Control(func(fd uintptr) {
				_, _, queryErr = syscall.Syscall(syscall.SYS_IOCTL, fd, 0x1268, uintptr(unsafe.Pointer(&sector))) // BLKSSZGET
				if queryErr == 0 {
					_, _, queryErr = syscall.Syscall(syscall.SYS_IOCTL, fd, 0x80081272, uintptr(unsafe.Pointer(&size))) // BLKGETSIZE64
				}
			})
			if err == nil && queryErr != 0 {
				err = queryErr
			}
		}
		if err != nil {
			h.initErr = fmt.Errorf("无法核对块设备容量与逻辑扇区：%w", err)
			return
		}
		if sector == 0 || size == 0 || h.snapshot.LogicalSectorSize > 0 && int64(sector) != h.snapshot.LogicalSectorSize || h.snapshot.Size > 0 && size != uint64(h.snapshot.Size) {
			h.initErr = errors.New("块设备容量或逻辑扇区与记录不一致，请刷新硬盘信息后重新扫描")
		}
	})
	return h.initErr
}

func (h *blockScanHandle) scanReadInfo() (string, string) { return scanReadMethod, scanDirectReadNote }

func (h *blockScanHandle) ReadAt(p []byte, offset int64) (int, error) {
	if h.closed.Load() {
		return 0, os.ErrClosed
	}
	if err := h.prepare(); err != nil {
		return 0, err
	}
	return h.file.ReadAt(p, offset)
}

func (h *blockScanHandle) Close() error {
	h.closed.Store(true)
	h.closeOnce.Do(func() {
		go func() { defer close(h.closeDone); _ = h.file.Close() }()
	})
	return nil
}

func (h *blockScanHandle) CloseDone() <-chan struct{} { return h.closeDone }

func isScanMediaError(err error) bool {
	// Linux maps BLK_STS_MEDIUM to ENODATA for block-device reads. It needs
	// the same bounded localization as EIO, rather than aborting the scan.
	return errors.Is(err, syscall.EIO) || errors.Is(err, syscall.EREMOTEIO) || errors.Is(err, syscall.ENODATA)
}

const scanReadMethod = "direct"
