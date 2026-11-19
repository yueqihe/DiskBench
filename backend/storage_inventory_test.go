package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func excludedStorageMounts() []storageMount {
	return []storageMount{
		{Path: "/vol9", Source: "//nas/share", FileSystem: "cifs"},
		{Path: "/mnt/nfs", Source: "nas:/export", FileSystem: "nfs"},
		{Path: "/mnt/nfs4", Source: "nas:/export", FileSystem: "nfs4"},
		{Path: "/mnt/ftp", Source: "curlftpfs#ftp://nas/", FileSystem: "fuse"},
		{Path: "/mnt/sftp", Source: "user@nas:/data", FileSystem: "fuse.sshfs"},
		{Path: "/mnt/cloud", Source: "cloud:backup", FileSystem: "fuse.rclone"},
		{Path: "/mnt/webdav", Source: "https://nas/dav", FileSystem: "fuse.davfs"},
		{Path: "/mnt/fuseblk", Source: "//nas/share", FileSystem: "fuseblk"},
		{Path: "/mnt/unverified", Source: "nas:/export", FileSystem: "ext4"},
		{Path: "/mnt/virtual", Source: "overlay", FileSystem: "overlay"},
	}
}

func TestStorageInventoryKeepsOnlyLocalMounts(t *testing.T) {
	localMounts := []storageMount{
		{Path: "/vol1", Source: "/dev/mapper/data", FileSystem: "btrfs"},
		{Path: "/vol2", Source: "/dev/sdb1[/data]", FileSystem: "btrfs"},
		{Path: "/media/usb", Source: "/dev/sdc1", FileSystem: "exfat"},
		{Path: "/media/ntfs", Source: "/dev/sdd1", FileSystem: "fuseblk"},
		{Path: "/vol3", Source: "tank/data", FileSystem: "zfs"},
	}
	allMounts := append(localMounts, excludedStorageMounts()...)
	var output strings.Builder
	var mountpoints []string
	for i := range allMounts {
		allMounts[i].Size, allMounts[i].FreeBytes = 1000, 500
		mountpoints = append(mountpoints, allMounts[i].Path)
		fmt.Fprintf(&output, "%s %s %s 1000 500 rw\n", allMounts[i].Path, allMounts[i].Source, allMounts[i].FileSystem)
	}
	mounts := parseStorageMounts(output.String())
	if len(mounts) != len(localMounts) {
		t.Fatalf("remote mount reached parsed inventory: %+v", mounts)
	}
	// A remote mount must remain excluded even if a physical disk's recorded
	// mountpoint matches its path, or the caller bypasses the text parser.
	disks := []Disk{{Path: "/dev/sda", Mountpoints: mountpoints}}
	for _, input := range [][]storageMount{mounts, allMounts} {
		spaces := deriveStorageSpaces(disks, input)
		if len(spaces) != len(localMounts) {
			t.Fatalf("nonlocal mount became a speed target: %+v", spaces)
		}
		for _, local := range localMounts {
			found := false
			for _, space := range spaces {
				if space.Path == local.Path && space.Source == local.Source {
					found = true
				}
			}
			if !found {
				t.Errorf("lost local storage %s (%s)", local.Path, local.FileSystem)
			}
		}
	}
}

func TestStorageInventoryCollapsesApplicationBindMounts(t *testing.T) {
	// Keep the order from affecting discovery: an app bind may be listed
	// before the canonical volume and may be nested below another bind.
	mounts := parseStorageMounts(strings.Join([]string{
		"/vol3/@appshare/fnos_toolbox_go/appdata /dev/mapper/data[/@appdata/fnos_toolbox_go] btrfs 1000 500 rw",
		"/vol3/@appshare/fnos_toolbox_go/logs /dev/mapper/data[/@appdata/fnos_toolbox_go/logs] btrfs 1000 500 rw",
		"/vol3/@appshare/fnos_toolbox_go/logs/archive /dev/mapper/data[/@appdata/fnos_toolbox_go/logs/archive] btrfs 1000 500 rw",
		"/vol3 /dev/mapper/data btrfs 1000 500 rw",
		"/media/USB /dev/sdc1 exfat 2000 1500 rw",
		"/vol3/external /dev/sdd1 btrfs 3000 2500 rw",
		"/vol30 /dev/mapper/data btrfs 1000 500 rw",
		"/vol4 tank zfs 4000 3500 rw",
		"/vol4/dataset tank/dataset zfs 2000 1500 rw",
	}, "\n"))
	disks := []Disk{
		{Path: "/dev/sdb", Mountpoints: []string{"/vol3", "/vol3/@appshare/fnos_toolbox_go/appdata", "/vol3/@appshare/fnos_toolbox_go/logs"}},
		{Path: "/dev/sdc", Mountpoints: []string{"/media/USB"}},
		{Path: "/dev/sdd", Mountpoints: []string{"/vol3/external"}},
	}
	spaces := deriveStorageSpaces(disks, mounts)
	wantPaths := []string{"/media/USB", "/vol3", "/vol3/external", "/vol30", "/vol4", "/vol4/dataset"}
	if len(spaces) != len(wantPaths) {
		t.Fatalf("application aliases leaked or independent mounts were lost: %+v", spaces)
	}
	for i, want := range wantPaths {
		if spaces[i].Path != want {
			t.Fatalf("target %d = %s, want %s", i, spaces[i].Path, want)
		}
	}
	if spaces[1].Size != 1000 || len(spaces[1].Members) != 1 || spaces[1].Members[0] != "/dev/sdb" {
		t.Fatalf("canonical volume lost capacity or disk membership: %+v", spaces[1])
	}
}

func TestRemoteStorageCannotStartOrResumeSpeedTest(t *testing.T) {
	for _, mount := range excludedStorageMounts() {
		t.Run(mount.FileSystem+mount.Path, func(t *testing.T) {
			space := testSpace()
			space.Path, space.Source, space.FileSystem = mount.Path, mount.Source, mount.FileSystem
			m := newTestManager(t, JobManagerOptions{Mock: true, ResolveSpace: func(string) (*StorageSpace, error) {
				return space, nil
			}})
			job, err := m.create("speed", space.Path, map[string]interface{}{"speedTarget": "filesystem"})
			var jobErr *jobError
			if job != nil || !errors.As(err, &jobErr) || jobErr.apiCode != "STORAGE_SPACE_NOT_LOCAL" {
				t.Fatalf("remote speed target accepted: job=%+v err=%v", job, err)
			}
			// Even a saved snapshot of that same mount must not pass runtime
			// validation, so a stale target cannot bypass the current policy.
			snapshot := storageSnapshot(space)
			if !storageIdentityChanged(&snapshot, space) {
				t.Fatal("saved remote target passed runtime validation")
			}
		})
	}
}

func TestStorageCapacityProbeExcludesNetworkAndVirtualMounts(t *testing.T) {
	mounts, err := readStorageMounts("/usr/bin/findmnt", func(ctx context.Context, exe string, args []string) ([]byte, error) {
		if exe != "/usr/bin/findmnt" {
			t.Fatalf("wrong executable: %s", exe)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			t.Fatal("capacity probe is not bounded")
		}
		var types string
		for i, arg := range args {
			if arg == "-t" && i+1 < len(args) {
				types = args[i+1]
			}
		}
		allowed := map[string]bool{}
		for _, kind := range strings.Split(types, ",") {
			allowed[kind] = true
		}
		for _, kind := range []string{"btrfs", "ext4", "zfs", "f2fs", "exfat", "ntfs3", "fuseblk"} {
			if !allowed[kind] {
				t.Fatalf("local filesystem %s excluded", kind)
			}
		}
		for _, kind := range []string{"cifs", "nfs", "nfs4", "fuse", "fuse.rclone", "autofs", "overlay", "tmpfs", "trimafs"} {
			if allowed[kind] {
				t.Fatalf("unsafe global capacity probe includes %s", kind)
			}
		}
		return []byte("/vol3 /dev/mapper/data btrfs 1000 500 rw\n/media/SSD\\x20Disk /dev/sdb1 exfat 2000 1000 ro\n"), nil
	})
	if err != nil || len(mounts) != 2 || mounts[1].Path != "/media/SSD Disk" || !mounts[1].ReadOnly {
		t.Fatalf("lost usable local mounts: %+v, %v", mounts, err)
	}
}

func TestStorageMountFallbackRetainsFilterAndDoesNotRepeatTimeout(t *testing.T) {
	t.Run("older findmnt", func(t *testing.T) {
		calls := 0
		mounts, err := readStorageMounts("findmnt", func(ctx context.Context, _ string, args []string) ([]byte, error) {
			calls++
			if ctx.Err() != nil || !strings.Contains(strings.Join(args, " "), "-t "+localStorageFileSystems) {
				t.Fatal("fallback lost live context or local filesystem filter")
			}
			if calls == 1 {
				return nil, errors.New("unsupported OPTIONS column")
			}
			if args[len(args)-1] != "TARGET,SOURCE,FSTYPE,SIZE,AVAIL" {
				t.Fatal("older findmnt fallback did not remove OPTIONS")
			}
			return []byte("/vol1 /dev/sda1 ext4 1000 500\n"), nil
		})
		if err != nil || calls != 2 || len(mounts) != 1 {
			t.Fatalf("fallback failed: %d, %+v, %v", calls, mounts, err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		calls := 0
		_, err := readStorageMounts("findmnt", func(context.Context, string, []string) ([]byte, error) {
			calls++
			return nil, context.DeadlineExceeded
		})
		if !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
			t.Fatalf("timeout was retried: calls=%d err=%v", calls, err)
		}
	})
}

func TestDiskInventorySurvivesStorageCapacityFailure(t *testing.T) {
	privateDetail := "unavailable remote source with private credentials"
	s := newTestServer(t, nil, apiServerOptions{
		LocalDev: true,
		ListDisks: func(func(string) string) ([]Disk, error) {
			return []Disk{{Path: "/dev/sda", Model: "Local disk", Size: 1000, Mountpoints: []string{"/vol1"}, FileSystems: []string{"ext4"}}}, nil
		},
		ListSpaces: func([]Disk, func(string) string) ([]StorageSpace, error) {
			return nil, errors.New(privateDetail)
		},
	})
	defer s.Close()
	status, body := doRequest(s, "GET", "/api/disks", "", nil)
	disks, ok := body["disks"].([]interface{})
	if status != 200 || !ok || len(disks) != 1 || disks[0].(map[string]interface{})["path"] != "/dev/sda" {
		t.Fatalf("mount failure hid physical disks: %d %+v", status, body)
	}
	spaces, ok := body["storageSpaces"].([]interface{})
	if !ok || len(spaces) != 0 || body["storageWarning"] == nil || body["error"] != nil {
		t.Fatalf("unavailable spaces must be an empty array with a warning: %+v", body)
	}
	if strings.Contains(string(mustJSON(body)), privateDetail) {
		t.Fatal("private mount error leaked into API response")
	}
	if disks[0].(map[string]interface{})["mountpoints"] == nil || disks[0].(map[string]interface{})["fileSystems"] == nil {
		t.Fatal("disk write protection metadata was lost")
	}
}
