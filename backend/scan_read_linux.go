//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func openScanDevice(path string) (ReadHandle, error) {
	h, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECT, 0)
	if err != nil {
		return nil, fmt.Errorf("无法开启直接读取扫描（不会降级为缓存读取）：%w", err)
	}
	return h, nil
}

func isScanMediaError(err error) bool {
	return errors.Is(err, syscall.EIO) || errors.Is(err, syscall.EREMOTEIO)
}

const scanReadMethod = "direct"
