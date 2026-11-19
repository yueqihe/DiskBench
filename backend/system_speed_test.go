package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSystemSpeedSelectsTemporaryDirectoryOnSystemDisk(t *testing.T) {
	for _, tc := range []struct {
		name, tmpSource, tmpType, fallbackSource, fallbackType, wantPath string
	}{
		{"disk tmp", "/dev/nvme0n1p2", "ext4", "", "", "/tmp"},
		{"memory tmp", "tmpfs", "tmpfs", "/dev/nvme0n1p2", "ext4", "/var/tmp"},
		{"remote tmp", "nas:/export", "nfs", "/dev/nvme0n1p2", "ext4", "/var/tmp"},
		{"data disk tmp", "/dev/sdb1", "ext4", "/dev/nvme0n1p2", "ext4", "/var/tmp"},
		{"no disk temporary directory", "tmpfs", "tmpfs", "//nas/share", "cifs", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probes := map[string]int{}
			execute := func(ctx context.Context, _ string, args []string) ([]byte, error) {
				if _, bounded := ctx.Deadline(); !bounded {
					t.Fatal("unbounded mount query")
				}
				directory := "/tmp"
				if strings.Contains(strings.Join(args, " "), " /var/tmp ") {
					directory = "/var/tmp"
				}
				source, kind := tc.tmpSource, tc.tmpType
				if directory == "/var/tmp" {
					source, kind = tc.fallbackSource, tc.fallbackType
				}
				if source == "" {
					t.Fatal("unnecessary fallback query")
				}
				if strings.Contains(args[len(args)-1], "SIZE") {
					probes[directory]++
					if kind != "ext4" || source != "/dev/nvme0n1p2" {
						t.Fatal("capacity queried on memory, remote or non-system filesystem")
					}
					return []byte(fmt.Sprintf("/ %s %s 21474836480 12884901888 rw", source, kind)), nil
				}
				return []byte(fmt.Sprintf("/ %s %s", source, kind)), nil
			}
			ancestors := func(source string) ([]string, string) {
				if source == "/dev/nvme0n1p2" {
					return []string{"/dev/nvme0n1"}, ""
				}
				return []string{"/dev/sdb"}, ""
			}
			space := readSystemStorageSpace("findmnt", map[string]bool{"/dev/nvme0n1": true}, execute, ancestors)
			if tc.wantPath == "" {
				if space != nil || len(probes) != 0 {
					t.Fatalf("unexpected system target: %+v, probes=%+v", space, probes)
				}
				return
			}
			if space == nil || space.Path != tc.wantPath || !space.IsSystem || space.Source != "/dev/nvme0n1p2" ||
				space.FileSystem != "ext4" || space.Size != 21474836480 || space.FreeBytes != 12884901888 || probes[tc.wantPath] != 1 {
				t.Fatalf("wrong system file target: %+v, probes=%+v", space, probes)
			}
		})
	}
}

func TestSystemSpeedRejectsReadOnlyOrChangedTemporaryMount(t *testing.T) {
	for _, capacityLine := range []string{
		"/ /dev/system ext4 1000 500 ro",
		"/ /dev/data ext4 1000 500 rw",
		"/ tmpfs tmpfs 1000 500 rw",
	} {
		t.Run(capacityLine, func(t *testing.T) {
			space := readSystemStorageSpace("findmnt", map[string]bool{"/dev/system": true},
				func(_ context.Context, _ string, args []string) ([]byte, error) {
					if strings.Contains(args[len(args)-1], "SIZE") {
						return []byte(capacityLine), nil
					}
					return []byte("/ /dev/system ext4"), nil
				}, func(string) ([]string, string) { return []string{"/dev/system"}, "" })
			if space != nil {
				t.Fatalf("unsafe temporary target accepted: %+v", space)
			}
		})
	}
}

func TestSystemFileSpeedUsesTemporaryFileTargetAndKeepsRawBlocked(t *testing.T) {
	space := testSpace()
	space.Path, space.IsSystem = "/tmp", true
	m := newTestManager(t, JobManagerOptions{Mock: true,
		ResolveSpace: func(string) (*StorageSpace, error) { return space, nil },
		ResolveDisk: func(string) (*Disk, error) {
			disk := testDisk()
			disk.IsSystem = true
			return disk, nil
		},
	})
	job, err := m.create("speed", "/tmp", map[string]interface{}{
		"speedTarget": "filesystem", "sampleMiB": float64(256), "speedTrim": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.StorageSpace == nil || !job.StorageSpace.IsSystem || !job.Disk.IsSystem || job.Device != "/tmp" || *job.Options.SpeedTrim {
		t.Fatalf("system target did not use file mode without TRIM: %+v", job)
	}
	waitForStatus(t, m, job.ID, "completed", 5*time.Second)
	_, err = m.create("speed", "/dev/sda", map[string]interface{}{"speedTarget": "raw"})
	var jobErr *jobError
	if !errors.As(err, &jobErr) || jobErr.apiCode != "RAW_SPEED_NOT_ALLOWED" {
		t.Fatalf("raw system speed was allowed: %v", err)
	}
	snapshot := storageSnapshot(space)
	space.IsSystem = false
	if !storageIdentityChanged(&snapshot, space) {
		t.Fatal("system target identity change was ignored")
	}
}

func TestSystemSpeedReservesCapacityAndRejectsOtherDirectories(t *testing.T) {
	const gib = int64(1024 * 1024 * 1024)
	for _, capacity := range []int64{4 * gib, 20 * gib} {
		space := testSpace()
		space.Path, space.IsSystem, space.Size = "/var/tmp", true, capacity
		reserve := maxInt64(gib, capacity/10)
		space.FreeBytes = gib + reserve
		if err := benchmarkSpaceCapacityError(space, gib); err != nil {
			t.Fatal(err)
		}
		for _, free := range []int64{0, gib + reserve - 1} {
			space.FreeBytes = free
			m := newTestManager(t, JobManagerOptions{Mock: true, ResolveSpace: func(string) (*StorageSpace, error) { return space, nil }})
			_, err := m.create("speed", space.Path, map[string]interface{}{"speedTarget": "filesystem", "sampleMiB": float64(1024)})
			var jobErr *jobError
			if !errors.As(err, &jobErr) || jobErr.apiCode != "STORAGE_SPACE_FULL" {
				t.Fatalf("reserve ignored: capacity=%d free=%d err=%v", capacity, free, err)
			}
		}
	}
	space := testSpace()
	space.Path, space.IsSystem = "/etc", true
	m := newTestManager(t, JobManagerOptions{Mock: true, ResolveSpace: func(string) (*StorageSpace, error) { return space, nil }})
	_, err := m.create("speed", space.Path, map[string]interface{}{"speedTarget": "filesystem"})
	var jobErr *jobError
	if !errors.As(err, &jobErr) || jobErr.apiCode != "SYSTEM_SPEED_TARGET_INVALID" {
		t.Fatalf("unexpected system directory allowed: %v", err)
	}
}
