package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
)

var nvmeNamespacePath = regexp.MustCompile(`^/dev/nvme[0-9]+(?:c[0-9]+)?n[0-9]+$`)

func isNativeNVMe(disk Disk) bool { return nvmeNamespacePath.MatchString(disk.Path) }

type NVMeScanInfo struct {
	NSID              uint32 `json:"nsid"`
	SelfTestSupported bool   `json:"selfTestSupported"`
	ExtendedMinutes   uint16 `json:"extendedMinutes"`
	MaxReadBytes      int    `json:"maxReadBytes"`
}

// The Linux ioctl status is already shifted past the CQ phase bit. SMART
// JSON status_field.value uses the same representation; do not shift again.
func decodeNVMeScanStatus(status uint16, nsid uint32) *scanIOError {
	if status == 0 {
		return nil
	}
	sct, sc := byte((status>>8)&7), byte(status)
	kind := "COMMAND_ERROR"
	switch {
	case sct == 2 && (sc == 0x80 || sc == 0x81):
		kind = "MEDIUM_ERROR"
	case sct == 2 && sc >= 0x82 && sc <= 0x84:
		kind = "DATA_INTEGRITY_ERROR"
	case sct == 2 && sc == 0x87:
		kind = "UNWRITTEN_LBA"
	case sct == 2 && sc == 0x86:
		kind = "ACCESS_DENIED"
	case sct == 3:
		kind = "TRANSPORT_ERROR"
	case sct == 0 && sc == 6:
		kind = "CONTROLLER_ERROR"
	case sct == 0 && (sc == 7 || sc == 8):
		kind = "COMMAND_ABORTED"
	case sct == 0 && sc == 0x82:
		kind = "NOT_READY"
	case sct == 0 && (sc == 1 || sc == 2):
		kind = "ILLEGAL_REQUEST"
	}
	return &scanIOError{Evidence: ScanIOEvidence{Protocol: "nvme", Kind: kind, NSID: nsid, NVMeStatus: status, StatusCodeType: sct, StatusCode: sc, DoNotRetry: status&0x4000 != 0}}
}

func localizableScanIssue(err error) bool {
	e := scanProtocolError(err)
	return e != nil && (e.Evidence.Kind == "MEDIUM_ERROR" || e.Evidence.Kind == "DATA_INTEGRITY_ERROR")
}

func nvmeReadWords(offset int64, length int, sector int64) (uint32, uint32, uint32, error) {
	if sector <= 0 || offset < 0 || length <= 0 || offset%sector != 0 || int64(length)%sector != 0 || int64(length)/sector > 65536 {
		return 0, 0, 0, errors.New("NVMe 读取范围未按逻辑扇区对齐或超过命令上限")
	}
	lba := uint64(offset / sector)
	return uint32(lba), uint32(lba >> 32), uint32(int64(length)/sector - 1), nil
}

func parseNVMeIdentify(ctrl, ns []byte, nsid uint32, sector, size int64) (NVMeScanInfo, error) {
	info := NVMeScanInfo{NSID: nsid}
	if len(ctrl) < 4096 || len(ns) < 4096 || nsid == 0 || nsid == 0xffffffff {
		return info, errors.New("NVMe Identify 数据不完整")
	}
	format := int(ns[26]&0xf | (ns[26]&0x60)>>1)
	if format > int(ns[25]) || format >= 64 {
		return info, errors.New("NVMe 活动 LBA 格式无效")
	}
	lbaf := ns[128+format*4 : 132+format*4]
	if lbaf[2] < 9 || lbaf[2] > 16 || sector != int64(1)<<lbaf[2] || size <= 0 || size%sector != 0 || binary.LittleEndian.Uint64(ns[:8]) != uint64(size/sector) {
		return info, errors.New("NVMe 命名空间容量或逻辑扇区与磁盘信息不一致，请刷新后重新扫描")
	}
	if binary.LittleEndian.Uint16(lbaf[:2]) != 0 || ns[29]&7 != 0 {
		return info, errNVMeMetadata
	}
	info.SelfTestSupported = binary.LittleEndian.Uint16(ctrl[256:258])&0x10 != 0
	info.ExtendedMinutes = binary.LittleEndian.Uint16(ctrl[316:318])
	info.MaxReadBytes = 128 * 1024
	// MDTS is in units of the controller minimum page size (at least 4K).
	// Using 4K is conservative even when the controller minimum is larger.
	if mdts := ctrl[77]; mdts > 0 && mdts < 6 {
		info.MaxReadBytes = min(info.MaxReadBytes, 4096<<mdts)
	}
	if int64(info.MaxReadBytes) < sector {
		return info, errors.New("NVMe 最大传输长度小于逻辑扇区")
	}
	return info, nil
}

var errNVMeMetadata = errors.New("命名空间启用了元数据或保护信息，使用内核直接读取以保留其处理")

type NVMeSelfTestResult struct {
	State   string `json:"state"`
	Percent int    `json:"percent"`
	Result  byte   `json:"result"`
	NSID    uint32 `json:"nsid"`
	LBA     *int64 `json:"lba,omitempty"`
	Message string `json:"message"`
}

func parseNVMeSelfTestLog(data []byte, nsid uint32, blocks int64) (NVMeSelfTestResult, error) {
	r := NVMeSelfTestResult{NSID: nsid}
	if len(data) < 564 {
		return r, errors.New("NVMe 自检日志不完整")
	}
	if data[0]&0xf != 0 {
		r.State, r.Percent, r.Message = "running", min(int(data[1]&0x7f), 100), "NVMe 固件自检正在运行"
		return r, nil
	}
	e := data[4:32]
	r.Result = e[0] & 0xf
	if e[0]>>4 == 0 || r.Result == 0xf {
		r.State, r.Message = "unknown", "没有有效的固件自检结果"
		return r, nil
	}
	if e[2]&1 != 0 && binary.LittleEndian.Uint32(e[12:16]) != nsid {
		r.State, r.Message = "unknown", "自检结果命名空间与当前磁盘不一致，不能归因于本盘"
		return r, nil
	}
	if e[2]&3 == 3 && binary.LittleEndian.Uint32(e[12:16]) == nsid {
		lba := binary.LittleEndian.Uint64(e[16:24])
		if lba < uint64(blocks) {
			v := int64(lba)
			r.LBA = &v
		}
	}
	if r.Result == 0 {
		r.State, r.Message = "passed", "固件自检完成，未报告错误"
	} else {
		r.State = "failed"
		r.Message = fmt.Sprintf("固件自检未通过或被中止（结果码 0x%x）；不等于已定位物理坏块", r.Result)
	}
	return r, nil
}
