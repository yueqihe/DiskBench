//go:build linux

package main

import (
	"os"
	"path/filepath"
)

func readScanActivity(device string) (ScanActivity, error) {
	data, err := os.ReadFile(filepath.Join("/sys/class/block", filepath.Base(device), "stat"))
	if err != nil {
		return ScanActivity{}, err
	}
	return parseScanActivity(string(data))
}
