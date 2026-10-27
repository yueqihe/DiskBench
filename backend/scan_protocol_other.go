//go:build !linux

package main

func configureScanReader(handle ReadHandle, disk Disk, snapshot DiskSnapshot) ReadHandle {
	return handle
}
func nativeNVMeNSID(disk Disk) uint32 { return 0 }
