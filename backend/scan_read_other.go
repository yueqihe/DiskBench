//go:build !linux

package main

import (
	"errors"
	"syscall"
)

func openScanDevice(path string) (ReadHandle, error) {
	return nil, errors.New("真实直接读取扫描仅支持 Linux；其他平台请使用演示模式")
}

func isScanMediaError(err error) bool { return errors.Is(err, syscall.EIO) }

const scanReadMethod = "unsupported"
