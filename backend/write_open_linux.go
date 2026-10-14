//go:build linux

package main

import (
	"os"
	"syscall"
)

// writeOpenFlags uses O_DIRECT so the verify pass reads back from the medium
// instead of the page cache.
const writeOpenFlags = os.O_RDWR | syscall.O_DIRECT

// readDirectFlags uses O_DIRECT so benchmark reads bypass the page cache and
// measure real media throughput.
const readDirectFlags = os.O_RDONLY | syscall.O_DIRECT

func openWriteDevice(path string) (WriteHandle, error) {
	return os.OpenFile(path, writeOpenFlags, 0)
}

func openReadDirect(path string) (WriteHandle, error) {
	return os.OpenFile(path, readDirectFlags, 0)
}

func openBenchmarkFile(path string, directIO bool) (WriteHandle, error) {
	flags := os.O_CREATE | os.O_EXCL | os.O_RDWR
	if directIO {
		flags |= syscall.O_DIRECT
	}
	return os.OpenFile(path, flags, 0o600)
}

// allocateAligned returns a buffer whose memory satisfies O_DIRECT alignment
// (page-aligned via mmap) and a release function.
func allocateAligned(size int) ([]byte, func()) {
	b, err := syscall.Mmap(-1, 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS)
	if err != nil {
		return make([]byte, size), func() {}
	}
	return b, func() { _ = syscall.Munmap(b) }
}
