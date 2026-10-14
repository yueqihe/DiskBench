//go:build !linux

package main

import "os"

// Non-Linux builds (mock/demo mode) cannot use O_DIRECT; plain buffered I/O
// keeps the write test runnable for UI previews. Real write tests must run on
// Linux with the linux build.
const writeOpenFlags = os.O_RDWR

func openWriteDevice(path string) (WriteHandle, error) {
	return os.OpenFile(path, writeOpenFlags, 0)
}

func openReadDirect(path string) (WriteHandle, error) {
	return os.OpenFile(path, os.O_RDONLY, 0)
}

func openBenchmarkFile(path string, _ bool) (WriteHandle, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
}

func allocateAligned(size int) ([]byte, func()) {
	return make([]byte, size), func() {}
}
