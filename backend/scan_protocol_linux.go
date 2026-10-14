//go:build linux

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// Linux sg_io_hdr_t, native pointer alignment (amd64 and arm64).
type scanSGHeader struct {
	InterfaceID   int32
	Direction     int32
	CommandLen    uint8
	MaxSenseLen   uint8
	IOVecCount    uint16
	TransferLen   uint32
	Transfer      unsafe.Pointer
	Command       unsafe.Pointer
	Sense         unsafe.Pointer
	Timeout       uint32
	Flags         uint32
	PackID        int32
	User          unsafe.Pointer
	Status        uint8
	MaskedStatus  uint8
	MessageStatus uint8
	SenseLen      uint8
	HostStatus    uint16
	DriverStatus  uint16
	Residual      int32
	Duration      uint32
	Info          uint32
}

// Both packaged architectures use the 88-byte Linux sg v3 ABI. Fail the
// build rather than issuing an ioctl with a silently incorrect layout.
var _ [88 - unsafe.Sizeof(scanSGHeader{})]byte
var _ [unsafe.Sizeof(scanSGHeader{}) - 88]byte
var _ [64 - unsafe.Offsetof(scanSGHeader{}.Status)]byte
var _ [unsafe.Offsetof(scanSGHeader{}.Status) - 64]byte

type hddScanHandle struct {
	file        *os.File
	sector      int64
	direct      atomic.Bool
	closed      atomic.Bool
	closeOnce   sync.Once
	closeDone   chan struct{}
	mu          sync.Mutex
	note        string
	geometryErr error
}

func configureHDDScan(handle ReadHandle, disk DiskSnapshot) ReadHandle {
	f, ok := handle.(*os.File)
	if !ok || !disk.Rotational {
		return handle
	}
	sector := disk.LogicalSectorSize
	if sector <= 0 {
		sector = 512
	}
	h := &hddScanHandle{file: f, sector: sector, closeDone: make(chan struct{})}
	// CDB addresses are in device logical sectors, not byte offsets. Verify
	// the kernel geometry instead of trusting a possibly defaulted inventory.
	var actualSector uint32
	var queryErr error
	raw, err := f.SyscallConn()
	if err == nil {
		err = raw.Control(func(fd uintptr) {
			_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, 0x1268, uintptr(unsafe.Pointer(&actualSector))) // BLKSSZGET
			if errno != 0 {
				queryErr = errno
			}
		})
	}
	if err != nil || queryErr != nil || actualSector == 0 {
		h.direct.Store(true)
		h.note = "未能核对设备逻辑扇区大小，使用 O_DIRECT 字节定位读取；普通 I/O 错误无法细分原因"
	} else if int64(actualSector) != sector {
		h.geometryErr = fmt.Errorf("磁盘逻辑扇区大小已变化（记录 %d，实际 %d），请刷新硬盘信息后重新扫描", sector, actualSector)
	}
	return h
}

func (h *hddScanHandle) scanReadInfo() (string, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.direct.Load() {
		return "direct", h.note
	}
	return "scsi-read", "HDD 使用 SCSI READ(16)；固件缓存与重映射仍由硬盘管理"
}

func (h *hddScanHandle) Close() error {
	h.closed.Store(true)
	// Control holds the descriptor until ioctl returns. Closing asynchronously
	// lets cancellation return without reusing an fd still used by the kernel.
	h.closeOnce.Do(func() {
		if h.closeDone == nil {
			h.closeDone = make(chan struct{})
		}
		go func() { defer close(h.closeDone); _ = h.file.Close() }()
	})
	return nil
}

func (h *hddScanHandle) CloseDone() <-chan struct{} { return h.closeDone }

func (h *hddScanHandle) ReadAt(p []byte, offset int64) (int, error) {
	if h.closed.Load() {
		return 0, os.ErrClosed
	}
	if h.geometryErr != nil {
		return 0, h.geometryErr
	}
	if h.direct.Load() {
		return h.file.ReadAt(p, offset)
	}
	if len(p) == 0 {
		return 0, nil
	}
	// Keep individual SG requests small enough for common SATA/USB bridges.
	// This does not change coverage or the UI region size.
	completed := 0
	var recovered error
	for completed < len(p) {
		if h.closed.Load() {
			return completed, os.ErrClosed
		}
		length := min(128*1024, len(p)-completed)
		n, err := h.readCommand(p[completed:completed+length], offset+int64(completed))
		completed += n
		if e := scanProtocolError(err); e != nil && e.Evidence.Kind == "RECOVERED_ERROR" && n == length {
			if recovered == nil {
				recovered = err
			}
			continue
		}
		if err != nil {
			return completed, err
		}
	}
	return completed, recovered
}

func (h *hddScanHandle) readCommand(p []byte, offset int64) (int, error) {
	if h.direct.Load() {
		return h.file.ReadAt(p, offset)
	}
	cdb, err := scsiRead16(offset, len(p), h.sector)
	if err != nil {
		return 0, err
	}
	sense := make([]byte, 96)
	hdr := scanSGHeader{InterfaceID: 'S', Direction: -3, CommandLen: 16, MaxSenseLen: uint8(len(sense)),
		TransferLen: uint32(len(p)), Transfer: unsafe.Pointer(&p[0]), Command: unsafe.Pointer(&cdb[0]),
		Sense: unsafe.Pointer(&sense[0]), Timeout: 60000}
	raw, err := h.file.SyscallConn()
	if err != nil {
		return 0, err
	}
	var ioctlErr syscall.Errno
	err = raw.Control(func(fd uintptr) {
		_, _, ioctlErr = syscall.Syscall(syscall.SYS_IOCTL, fd, 0x2285, uintptr(unsafe.Pointer(&hdr)))
	})
	runtime.KeepAlive(p)
	runtime.KeepAlive(cdb)
	runtime.KeepAlive(sense)
	if err != nil {
		return 0, err
	}
	response := decodeScanSense(sense[:min(int(hdr.SenseLen), len(sense))], hdr.Status, hdr.HostStatus, hdr.DriverStatus, offset/h.sector, int64(len(p))/h.sector)
	// Unsupported command/transport only: never hide media, timeout, or link
	// errors by silently trying another backend.
	unsupported := ioctlErr == syscall.ENOTTY || ioctlErr == syscall.EOPNOTSUPP ||
		(ioctlErr == 0 && response != nil && response.Evidence.Kind == "ILLEGAL_REQUEST" &&
			(response.Evidence.ASC == 0x20 || response.Evidence.ASC == 0x24) && response.Evidence.ASCQ == 0)
	if unsupported {
		h.mu.Lock()
		h.note = "设备或桥接器不支持 SCSI READ(16)，使用 O_DIRECT 读取；普通 I/O 错误无法细分为盘面或连接故障"
		h.direct.Store(true)
		h.mu.Unlock()
		return h.file.ReadAt(p, offset)
	}
	if ioctlErr != 0 {
		kind := "COMMAND_ERROR"
		if ioctlErr == syscall.ETIMEDOUT {
			kind = "TIMEOUT"
		}
		if ioctlErr == syscall.ENODEV || ioctlErr == syscall.ENXIO {
			kind = "TRANSPORT_ERROR"
		}
		return 0, &scanIOError{Evidence: ScanIOEvidence{Kind: kind}}
	}
	if hdr.Residual < 0 || int64(hdr.Residual) > int64(len(p)) {
		return 0, errors.New("SCSI 返回无效的剩余读取长度")
	}
	n := len(p) - int(hdr.Residual)
	if response != nil {
		// Residual on failed commands is not a trustworthy successful prefix.
		if response.Evidence.Kind != "RECOVERED_ERROR" {
			n = 0
		}
		return n, response
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
