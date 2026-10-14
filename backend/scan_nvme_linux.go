//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// nvme_passthru_cmd: Linux amd64/arm64 UAPI, 72 bytes.
type scanNVMeCommand struct {
	Opcode, Flags                            byte
	Reserved                                 uint16
	NSID, CDW2, CDW3                         uint32
	Metadata                                 uint64
	Addr                                     unsafe.Pointer
	MetadataLen, DataLen                     uint32
	CDW10, CDW11, CDW12, CDW13, CDW14, CDW15 uint32
	Timeout, Result                          uint32
}

var _ [72 - unsafe.Sizeof(scanNVMeCommand{})]byte
var _ [unsafe.Sizeof(scanNVMeCommand{}) - 72]byte
var _ [24 - unsafe.Offsetof(scanNVMeCommand{}.Addr)]byte
var _ [unsafe.Offsetof(scanNVMeCommand{}.Addr) - 24]byte

type nvmeScanHandle struct {
	file          *os.File
	disk          Disk
	once          sync.Once
	mu            sync.Mutex
	info          NVMeScanInfo
	native        bool
	note          string
	initErr       error
	closed        atomic.Bool
	selfTestOwned atomic.Bool
	closeOnce     sync.Once
	closeDone     chan struct{}
}

func configureScanReader(handle ReadHandle, disk Disk, snapshot DiskSnapshot) ReadHandle {
	if file, ok := handle.(*os.File); ok && isNativeNVMe(disk) {
		return &nvmeScanHandle{file: file, disk: disk, closeDone: make(chan struct{})}
	}
	return configureHDDScan(handle, snapshot)
}

func (h *nvmeScanHandle) control(request uintptr, arg unsafe.Pointer) (uintptr, error) {
	raw, err := h.file.SyscallConn()
	if err != nil {
		return 0, err
	}
	var ret uintptr
	var errno syscall.Errno
	err = raw.Control(func(fd uintptr) { ret, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, request, uintptr(arg)) })
	if err != nil {
		return 0, err
	}
	if errno != 0 {
		return 0, errno
	}
	return ret, nil
}

func (h *nvmeScanHandle) command(admin bool, cmd *scanNVMeCommand, data []byte) error {
	if len(data) > 0 {
		cmd.Addr = unsafe.Pointer(&data[0])
		cmd.DataLen = uint32(len(data))
	}
	request := uintptr(0xc0484e43) // NVME_IOCTL_IO_CMD
	if admin {
		request = 0xc0484e41
	}
	status, err := h.control(request, unsafe.Pointer(cmd))
	runtime.KeepAlive(data)
	runtime.KeepAlive(cmd)
	if err != nil {
		if errors.Is(err, syscall.EIO) {
			return &scanIOError{Evidence: ScanIOEvidence{Protocol: "nvme", NSID: cmd.NSID, Kind: "COMMAND_ERROR"}}
		}
		classified := classifyScanSystemError(err)
		if e := scanProtocolError(classified); e != nil {
			v := e.Evidence
			v.Protocol = "nvme"
			v.NSID = cmd.NSID
			return &scanIOError{Evidence: v}
		}
		return classified
	}
	if e := decodeNVMeScanStatus(uint16(status), cmd.NSID); e != nil {
		return e
	}
	return nil
}

func (h *nvmeScanHandle) prepare() error {
	h.once.Do(func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		fallback := func(err error) {
			if e := scanProtocolError(err); e != nil && e.Evidence.Kind != "ILLEGAL_REQUEST" {
				h.initErr = err
				return
			}
			h.note = fmt.Sprintf("NVMe 原生命令不可用，使用 O_DIRECT；无法细分原生状态：%v", err)
		}
		nsid, err := h.control(0x4e40, nil) // NVME_IOCTL_ID, never derive from the path.
		if err != nil || nsid == 0 || nsid >= 0xffffffff {
			fallback(errors.New("无法确认有效 NSID"))
			return
		}
		h.info.NSID = uint32(nsid)
		var sector uint32
		var size uint64
		if _, err = h.control(0x1268, unsafe.Pointer(&sector)); err != nil {
			fallback(err)
			return
		}
		if _, err = h.control(0x80081272, unsafe.Pointer(&size)); err != nil {
			fallback(err)
			return
		}
		if int64(sector) != h.disk.LogicalSectorSize || size != uint64(h.disk.Size) {
			h.initErr = errors.New("NVMe 内核容量或逻辑扇区与记录不一致，请刷新后重试")
			return
		}
		ctrl, ns := make([]byte, 4096), make([]byte, 4096)
		if err = h.command(true, &scanNVMeCommand{Opcode: 6, CDW10: 1, Timeout: 10000}, ctrl); err != nil {
			fallback(err)
			return
		}
		if err = h.command(true, &scanNVMeCommand{Opcode: 6, NSID: uint32(nsid), CDW10: 0, Timeout: 10000}, ns); err != nil {
			fallback(err)
			return
		}
		info, err := parseNVMeIdentify(ctrl, ns, uint32(nsid), int64(sector), int64(size))
		if errors.Is(err, errNVMeMetadata) {
			fallback(err)
			return
		}
		if err != nil {
			h.initErr = err
			return
		}
		h.info, h.native = info, true
		h.note = fmt.Sprintf("NVMe 原生只读，NSID %d；逻辑地址由控制器映射，TRIM 后的可读区域不代表全部物理 NAND 已检测", nsid)
	})
	return h.initErr
}

func (h *nvmeScanHandle) scanReadInfo() (string, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.native {
		return "nvme-read", h.note
	}
	return "direct", h.note
}

func (h *nvmeScanHandle) nvmeInfo() NVMeScanInfo { h.mu.Lock(); defer h.mu.Unlock(); return h.info }

func (h *nvmeScanHandle) ReadAt(p []byte, offset int64) (int, error) {
	if h.closed.Load() {
		return 0, os.ErrClosed
	}
	if err := h.prepare(); err != nil {
		return 0, err
	}
	if !h.native {
		return h.file.ReadAt(p, offset)
	}
	completed := 0
	for completed < len(p) {
		if h.closed.Load() {
			return completed, os.ErrClosed
		}
		length := min(h.info.MaxReadBytes, len(p)-completed)
		lo, hi, count, err := nvmeReadWords(offset+int64(completed), length, h.disk.LogicalSectorSize)
		if err != nil {
			return completed, err
		}
		cmd := scanNVMeCommand{Opcode: 2, NSID: h.info.NSID, CDW10: lo, CDW11: hi, CDW12: count, Timeout: 60000}
		if err = h.command(false, &cmd, p[completed:completed+length]); err != nil {
			// Only an unavailable interface may fall back. Never hide a device
			// completion status (including invalid-field/permission statuses).
			if errors.Is(err, syscall.ENOTTY) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
				h.mu.Lock()
				h.native = false
				h.note = "NVMe I/O 透传不可用，使用 O_DIRECT，错误原因无法细分"
				h.mu.Unlock()
				n, readErr := h.file.ReadAt(p[completed:], offset+int64(completed))
				return completed + n, readErr
			}
			return completed, classifyScanSystemError(err)
		}
		completed += length
	}
	return completed, nil
}

func (h *nvmeScanHandle) selfTestLog() ([]byte, error) {
	data := make([]byte, 564)
	// Controller-wide log; entries are filtered by valid namespace/LBA bits.
	err := h.command(true, &scanNVMeCommand{Opcode: 2, NSID: 0xffffffff, CDW10: 6 | ((564/4 - 1) << 16), Timeout: 10000}, data)
	return data, err
}

func (h *nvmeScanHandle) startSelfTest() error {
	if h.closed.Load() {
		return os.ErrClosed
	}
	if err := h.prepare(); err != nil {
		return err
	}
	if !h.native || !h.info.SelfTestSupported {
		return errors.New("当前 NVMe 不支持或无法访问固件自检")
	}
	// Set ownership before submission so cancellation during the ioctl will
	// wait for its completion, then abort only a test we successfully started.
	h.mu.Lock()
	defer h.mu.Unlock()
	err := h.command(true, &scanNVMeCommand{Opcode: 0x14, NSID: h.info.NSID, CDW10: 2, Timeout: 10000}, nil)
	if err == nil {
		h.selfTestOwned.Store(true)
	}
	return err
}

func (h *nvmeScanHandle) finishSelfTest() { h.selfTestOwned.Store(false) }

func (h *nvmeScanHandle) Close() error {
	h.closed.Store(true)
	h.closeOnce.Do(func() {
		if h.closeDone == nil {
			h.closeDone = make(chan struct{})
		}
		go func() {
			defer close(h.closeDone)
			h.mu.Lock()
			if h.selfTestOwned.Swap(false) {
				_ = h.command(true, &scanNVMeCommand{Opcode: 0x14, NSID: h.info.NSID, CDW10: 0xf, Timeout: 10000}, nil)
			}
			h.mu.Unlock()
			_ = h.file.Close()
		}()
	})
	return nil
}

func (h *nvmeScanHandle) CloseDone() <-chan struct{} { return h.closeDone }

// Only a kernel-confirmed namespace ID is used to associate controller logs.
func nativeNVMeNSID(disk Disk) uint32 {
	if !isNativeNVMe(disk) {
		return 0
	}
	f, err := os.Open(disk.Path)
	if err != nil {
		return 0
	}
	defer f.Close()
	h := nvmeScanHandle{file: f}
	nsid, err := h.control(0x4e40, nil)
	if err != nil || nsid == 0 || nsid >= 0xffffffff {
		return 0
	}
	return uint32(nsid)
}
