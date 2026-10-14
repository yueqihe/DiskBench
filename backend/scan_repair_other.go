//go:build !linux

package main

import "errors"

func openRepairDevice(path string) (RepairHandle, error) {
	return nil, errors.New("实际慢扇区修复仅支持 Linux/fnOS")
}

func syncRepairBackupDirectory(path string) error { return nil }
