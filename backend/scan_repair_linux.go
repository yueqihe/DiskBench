//go:build linux

package main

import (
	"fmt"
	"os"
	"syscall"
)

func openRepairDevice(path string) (RepairHandle, error) {
	h, err := os.OpenFile(path, os.O_RDWR|syscall.O_DIRECT|syscall.O_EXCL, 0)
	if err != nil {
		return nil, fmt.Errorf("无法独占开启慢扇区修复，请卸载磁盘并停止其他磁盘工具：%w", err)
	}
	info, err := h.Stat()
	if err != nil || info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
		h.Close()
		return nil, fmt.Errorf("慢扇区修复只允许访问原始块设备")
	}
	return h, nil
}

func syncRepairBackupDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
