//go:build !linux

package main

func configureHDDScan(handle ReadHandle, disk DiskSnapshot) ReadHandle { return handle }

func configureScanReader(handle ReadHandle, disk Disk, snapshot DiskSnapshot) ReadHandle {
	return handle
}
func nativeNVMeNSID(disk Disk) uint32 { return 0 }
