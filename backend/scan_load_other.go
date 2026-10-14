//go:build !linux

package main

import "errors"

func readScanActivity(string) (ScanActivity, error) {
	return ScanActivity{}, errors.New("当前系统不提供 Linux 磁盘活动统计")
}
