//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBenchmarkOpensDecodedMountPath(t *testing.T) {
	for _, tc := range []struct{ name, encoded string }{
		{"HGST HSH721414ALE6M4", `HGST\x20HSH721414ALE6M4`},
		{`literal\x20name`, `literal\x5cx20name`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			mountpoint := filepath.Join(parent, tc.name)
			if err := os.Mkdir(mountpoint, 0o700); err != nil {
				t.Fatal(err)
			}
			mounts := parseStorageMounts(parent + "/" + tc.encoded + " /dev/test ext4 1000 500 rw\n")
			if len(mounts) != 1 {
				t.Fatalf("mounts = %+v", mounts)
			}
			filename := filepath.Join(mounts[0].Path, ".diskbench-benchmark-test")
			file, err := openBenchmarkFile(filename, false)
			if err != nil {
				t.Fatalf("benchmark cannot open the decoded directory: %v", err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(mountpoint, ".diskbench-benchmark-test")); err != nil {
				t.Fatalf("temporary file was not created in the real mountpoint: %v", err)
			}
		})
	}
}
