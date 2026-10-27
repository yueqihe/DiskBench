package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"syscall"
)

// Protocol evidence describes the command response, not a physical defect.
// An information LBA is only kept when sense marks it valid and it lies in
// the requested range. It is a hint; localization still verifies by reading.
type ScanIOEvidence struct {
	RequestOffset  *int64 `json:"requestOffset,omitempty"`
	RequestBytes   int64  `json:"requestBytes,omitempty"`
	Protocol       string `json:"protocol,omitempty"`
	NSID           uint32 `json:"nsid,omitempty"`
	NVMeStatus     uint16 `json:"nvmeStatus,omitempty"`
	StatusCodeType byte   `json:"statusCodeType,omitempty"`
	StatusCode     byte   `json:"statusCode,omitempty"`
	DoNotRetry     bool   `json:"doNotRetry,omitempty"`
	Kind           string `json:"kind"`
	SenseKey       byte   `json:"senseKey"`
	ASC            byte   `json:"asc"`
	ASCQ           byte   `json:"ascq"`
	HostStatus     uint16 `json:"hostStatus"`
	DriverStatus   uint16 `json:"driverStatus"`
	Status         byte   `json:"status"`
	LBA            *int64 `json:"lba,omitempty"`
}

type scanIOError struct{ Evidence ScanIOEvidence }

func (e *scanIOError) Error() string {
	if e.Evidence.Protocol == "nvme" {
		return fmt.Sprintf("%s (NVMe NSID=%d, SCT=%x, SC=%02x, DNR=%t)", e.Evidence.Kind, e.Evidence.NSID, e.Evidence.StatusCodeType, e.Evidence.StatusCode, e.Evidence.DoNotRetry)
	}
	return fmt.Sprintf("%s (sense=%02x/%02x/%02x, host=%02x, driver=%02x)",
		e.Evidence.Kind, e.Evidence.SenseKey, e.Evidence.ASC, e.Evidence.ASCQ, e.Evidence.HostStatus, e.Evidence.DriverStatus)
}

func scanProtocolError(err error) *scanIOError {
	var e *scanIOError
	if errors.As(err, &e) {
		return e
	}
	return nil
}

func classifyScanSystemError(err error) error {
	if scanProtocolError(err) != nil {
		return err
	}
	if errors.Is(err, syscall.ETIMEDOUT) {
		return &scanIOError{Evidence: ScanIOEvidence{Kind: "TIMEOUT"}}
	}
	if errors.Is(err, syscall.ENODEV) || errors.Is(err, syscall.ENXIO) {
		return &scanIOError{Evidence: ScanIOEvidence{Kind: "TRANSPORT_ERROR"}}
	}
	return err // EIO alone cannot distinguish medium from transport failure.
}

func scanIssueRange(offset, length int64, err error, recovered *bool) BadRange {
	r := BadRange{Offset: offset, Length: length, Code: "EIO", Recovered: recovered}
	if e := scanProtocolError(err); e != nil {
		v := e.Evidence
		r.Code, r.Evidence = v.Kind, &v
		if errors.Is(err, errScanWaitTimeout) && v.RequestOffset != nil && v.RequestBytes > 0 {
			r.Offset, r.Length = *v.RequestOffset, v.RequestBytes
		}
	} else if errors.Is(err, syscall.ENODATA) {
		// This is a block-layer error, not a captured SCSI/NVMe response.
		// Keep the cause without inventing sense data or a device-reported LBA.
		r.Code, r.Detail = "MEDIUM_ERROR", "Linux 块层报告介质读取错误（ENODATA）"
	}
	return r
}

func scsiRead16(offset int64, length int, sector int64) ([16]byte, error) {
	var cdb [16]byte
	if sector <= 0 || offset < 0 || length <= 0 || offset%sector != 0 || int64(length)%sector != 0 || uint64(int64(length)/sector) > 0xffffffff {
		return cdb, errors.New("SCSI 读取范围未按逻辑扇区对齐")
	}
	cdb[0] = 0x88 // READ(16), no write, repair, or remap command.
	binary.BigEndian.PutUint64(cdb[2:10], uint64(offset/sector))
	binary.BigEndian.PutUint32(cdb[10:14], uint32(int64(length)/sector))
	return cdb, nil
}

func decodeScanSense(sense []byte, status byte, host, driver uint16, firstLBA, blocks int64) *scanIOError {
	e := ScanIOEvidence{Status: status, HostStatus: host, DriverStatus: driver}
	validSense := false
	deferred := false
	var lba uint64
	validLBA := false
	if len(sense) >= 8 {
		deferred = sense[0]&0x7f == 0x71 || sense[0]&0x7f == 0x73
		end := min(len(sense), 8+int(sense[7]))
		switch sense[0] & 0x7f {
		case 0x70, 0x71:
			if end >= 14 {
				validSense = true
				e.SenseKey, e.ASC, e.ASCQ = sense[2]&0xf, sense[12], sense[13]
				// Deferred errors need not belong to this command.
				validLBA = sense[0]&0x80 != 0 && sense[0]&0x7f == 0x70
				lba = uint64(binary.BigEndian.Uint32(sense[3:7]))
			}
		case 0x72, 0x73:
			validSense = true
			e.SenseKey, e.ASC, e.ASCQ = sense[1]&0xf, sense[2], sense[3]
			for pos := 8; pos+2 <= end; {
				next := pos + 2 + int(sense[pos+1])
				if next > end {
					break
				}
				if sense[pos] == 0 && sense[pos+1] >= 10 && sense[pos+2]&0x80 != 0 && sense[0] == 0x72 {
					lba, validLBA = binary.BigEndian.Uint64(sense[pos+4:pos+12]), true
				}
				pos = next
			}
		}
	}
	if validLBA && firstLBA >= 0 && blocks > 0 && lba >= uint64(firstLBA) && lba-uint64(firstLBA) < uint64(blocks) {
		v := int64(lba)
		e.LBA = &v
	}
	// Transport failure takes precedence over sense left by an incomplete I/O.
	switch {
	case host == 3 || driver&0xf == 6:
		e.Kind = "TIMEOUT"
	case host != 0:
		e.Kind = "TRANSPORT_ERROR"
	case driver&0xf != 0 && driver&0xf != 8:
		e.Kind = "CONTROLLER_ERROR"
	case validSense && deferred:
		e.Kind = "DEFERRED_ERROR"
	case validSense && e.SenseKey == 3:
		e.Kind = "MEDIUM_ERROR"
	case validSense && e.SenseKey == 1:
		e.Kind = "RECOVERED_ERROR"
	case validSense && e.SenseKey == 4:
		e.Kind = "HARDWARE_ERROR"
	case validSense && e.SenseKey == 5:
		e.Kind = "ILLEGAL_REQUEST"
	case validSense && e.SenseKey == 2:
		e.Kind = "NOT_READY"
	case validSense && e.SenseKey == 6:
		e.Kind = "UNIT_ATTENTION"
	case validSense && e.SenseKey == 0xb:
		e.Kind = "COMMAND_ABORTED"
	case validSense && e.SenseKey != 0:
		e.Kind = "DEVICE_ERROR"
	case status != 0 || driver != 0:
		e.Kind = "COMMAND_ERROR"
	default:
		return nil
	}
	return &scanIOError{Evidence: e}
}
