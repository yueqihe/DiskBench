package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestFlattenDisksKeepsPhysicalDisksAndRollsUpMountpoints(t *testing.T) {
	disks := flattenDisks([]lsblkNode{
		{Name: "sda", Path: "/dev/sda", Type: "disk", Size: json.Number("1000"), Model: " Test ", Rota: true, LogSec: json.Number("512"), PhySec: json.Number("4096"), Children: []lsblkNode{
			{Name: "sda1", Path: "/dev/sda1", Type: "part", FSType: "ext4", Mountpoints: []string{"/", ""}},
		}},
	}, map[string]bool{})
	if len(disks) != 1 {
		t.Fatalf("expected 1 disk, got %d", len(disks))
	}
	d := disks[0]
	if d.Model != "Test" {
		t.Errorf("model = %q, want %q", d.Model, "Test")
	}
	if !d.IsSystem {
		t.Error("expected isSystem true")
	}
	if d.LogicalSectorSize != 512 || d.PhysicalSectorSize != 4096 {
		t.Errorf("sector sizes wrong: %d/%d", d.LogicalSectorSize, d.PhysicalSectorSize)
	}
	if len(d.Mountpoints) != 1 || d.Mountpoints[0] != "/" {
		t.Errorf("mountpoints = %v", d.Mountpoints)
	}
	if !d.HasPartitions || len(d.FileSystems) != 1 || d.FileSystems[0] != "ext4" {
		t.Errorf("media usage = partitions:%v filesystems:%v", d.HasPartitions, d.FileSystems)
	}
	blank := flattenDisks([]lsblkNode{{Name: "sdb", Path: "/dev/sdb", Type: "disk", Size: json.Number("1000000000"), PTType: "gpt"}}, nil)
	if len(blank) != 1 || blank[0].PartitionTable != "gpt" || rawSpeedBlockedReason(&blank[0]) != "" {
		t.Errorf("GPT disk without a recognized filesystem must allow raw testing: %+v", blank)
	}
	unknown := flattenDisksWithPartitionInfo([]lsblkNode{{Name: "sdc", Path: "/dev/sdc", Type: "disk", Size: json.Number("1000000000")}}, nil, false)
	if len(unknown) != 1 || !unknown[0].PartitionTableUnknown || rawSpeedBlockedReason(&unknown[0]) != "" {
		t.Errorf("unknown partition state without a recognized filesystem must allow raw testing: %+v", unknown)
	}
	if got := flattenDisks([]lsblkNode{{Name: "empty", Path: "/dev/empty", Type: "disk", Size: json.Number("0")}}, map[string]bool{}); len(got) != 0 {
		t.Errorf("expected empty disk filtered, got %d", len(got))
	}
}

func TestFlattenDisksHidesEmmcReservedAndZramDevices(t *testing.T) {
	disks := flattenDisks([]lsblkNode{
		{Name: "mmcblk0", Kname: "mmcblk0", Path: "/dev/mmcblk0", Type: "disk", Size: json.Number("128000000000")},
		{Name: "mmcblk0boot0", Kname: "mmcblk0boot0", Path: "/dev/mmcblk0boot0", Type: "disk", Size: json.Number("4194304")},
		{Name: "mmcblk0boot1", Kname: "mmcblk0boot1", Path: "/dev/mmcblk0boot1", Type: "disk", Size: json.Number("4194304")},
		{Name: "mmcblk0rpmb", Kname: "mmcblk0rpmb", Path: "/dev/mmcblk0rpmb", Type: "disk", Size: json.Number("4194304")},
		{Name: "zram0", Kname: "zram0", Path: "/dev/zram0", Type: "disk", Size: json.Number("4294967296")},
	}, nil)
	if len(disks) != 1 || disks[0].Path != "/dev/mmcblk0" {
		t.Fatalf("disks = %+v, want only the eMMC data disk", disks)
	}
}

func TestParseStorageMountsKeepsFnOSAndExternalDiskMounts(t *testing.T) {
	mounts := parseStorageMounts("/ /dev/root ext4 1000 500 rw,relatime\n/vol3 /dev/mapper/vg-lv btrfs 200000 150000 rw,relatime\n/vol4 tank zfs 300000 200000 rw\n/media/My\\040Disk /dev/sda1 exfat 400000 100000 ro,nosuid\n")
	if len(mounts) != 3 {
		t.Fatalf("mounts = %+v, want fnOS volumes and external mount", mounts)
	}
	if mounts[0].Path != "/vol3" || mounts[1].Path != "/vol4" || mounts[2].Path != "/media/My Disk" || !mounts[2].ReadOnly {
		t.Errorf("mounts = %+v", mounts)
	}
}

func TestSafeStorageMountPathRejectsRootAndTraversal(t *testing.T) {
	for value, want := range map[string]bool{
		"/vol1": true, "/media/USB Disk": true, "/": false, "relative": false,
		"/media/../etc": false, "/media/disk\nother": false, "/dev/sda": false, "/proc/data": false,
	} {
		if got := safeStorageMountPath(value); got != want {
			t.Errorf("safeStorageMountPath(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestDeriveStorageSpacesDeduplicatesArrayMembers(t *testing.T) {
	disks := []Disk{
		{Path: "/dev/sdb", Mountpoints: []string{"/vol3"}},
		{Path: "/dev/sdf", Mountpoints: []string{"/vol3"}},
		{Path: "/dev/sda", Mountpoints: []string{}},
	}
	spaces := deriveStorageSpaces(disks, []storageMount{
		{Path: "/vol3", Source: "/dev/mapper/vg-lv", FileSystem: "btrfs", Size: 1000, FreeBytes: 500},
		{Path: "/vol3", Source: "/dev/duplicate", FileSystem: "btrfs", Size: 1000, FreeBytes: 500},
	})
	if len(spaces) != 1 {
		t.Fatalf("spaces = %+v, want one deduplicated volume", spaces)
	}
	if spaces[0].Path != "/vol3" || spaces[0].Layout != "raid" {
		t.Errorf("space = %+v", spaces[0])
	}
	if len(spaces[0].Members) != 2 || spaces[0].Members[0] != "/dev/sdb" || spaces[0].Members[1] != "/dev/sdf" {
		t.Errorf("members = %+v", spaces[0].Members)
	}
}

func TestDeriveStorageSpacesLinksExternalMountToPhysicalDisk(t *testing.T) {
	disks := []Disk{{Path: "/dev/sdg", Mountpoints: []string{"/media/Backup SSD"}}}
	spaces := deriveStorageSpaces(disks, []storageMount{{
		Path: "/media/Backup SSD", Source: "/dev/sdg1", FileSystem: "exfat", Size: 2000, FreeBytes: 1200,
	}})
	if len(spaces) != 1 || spaces[0].Path != "/media/Backup SSD" || len(spaces[0].Members) != 1 || spaces[0].Members[0] != "/dev/sdg" {
		t.Fatalf("external space = %+v", spaces)
	}
}

func TestSafeDevicePattern(t *testing.T) {
	cases := map[string]bool{
		"/dev/sda":                true,
		"/dev/nvme0n1":            true,
		"/dev/sda;touch /tmp/pwn": false,
		"/dev/disk/by-id/example": false,
		"../../dev/sda":           false,
	}
	for path, want := range cases {
		if got := safeDevicePattern.MatchString(path); got != want {
			t.Errorf("safeDevicePattern.MatchString(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestParseSmartctlJsonNVMe(t *testing.T) {
	parsed, err := parseSmartctlJson(`{
		"smartctl": {"version": [7, 4]},
		"smart_status": {"passed": true},
		"firmware_version": "5B2QJXD7",
		"temperature": {"current": 38},
		"power_on_time": {"hours": 1284},
		"power_cycle_count": 326,
		"nvme_smart_health_information_log": {"data_units_written": 1000, "percentage_used": 4}
	}`, 512)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if !parsed.SmartAvailable || parsed.SmartStatus != "passed" {
		t.Errorf("smart = %v/%s", parsed.SmartAvailable, parsed.SmartStatus)
	}
	if parsed.Firmware == nil || *parsed.Firmware != "5B2QJXD7" {
		t.Errorf("firmware wrong")
	}
	if parsed.TemperatureC == nil || *parsed.TemperatureC != 38 {
		t.Errorf("temperature wrong")
	}
	if parsed.PowerOnHours == nil || *parsed.PowerOnHours != 1284 {
		t.Errorf("powerOnHours wrong")
	}
	if parsed.PowerCycles == nil || *parsed.PowerCycles != 326 {
		t.Errorf("powerCycles wrong")
	}
	if parsed.BytesWritten == nil || *parsed.BytesWritten != 512000000 {
		t.Errorf("bytesWritten wrong: %v", parsed.BytesWritten)
	}
	if parsed.PercentageUsed == nil || *parsed.PercentageUsed != 4 {
		t.Errorf("percentageUsed wrong")
	}
}

func TestParseSmartctlJsonRejectsUnavailable(t *testing.T) {
	for _, exitStatus := range []int64{1, 2, 4, 6, 10} {
		raw, _ := json.Marshal(map[string]interface{}{
			"smartctl":     map[string]interface{}{"version": []int{7, 4}, "exit_status": exitStatus},
			"smart_status": map[string]interface{}{"passed": true},
		})
		parsed, err := parseSmartctlJson(string(raw), 512)
		if err != nil {
			t.Fatalf("parse error: %v", err)
		}
		if parsed.SmartAvailable || parsed.SmartStatus != "unavailable" {
			t.Errorf("exit %d: expected unavailable, got %v/%s", exitStatus, parsed.SmartAvailable, parsed.SmartStatus)
		}
	}

	raw, _ := json.Marshal(map[string]interface{}{
		"smartctl":     map[string]interface{}{"version": []int{7, 4}, "exit_status": 8},
		"smart_status": map[string]interface{}{"passed": false},
	})
	parsed, err := parseSmartctlJson(string(raw), 512)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if !parsed.SmartAvailable || parsed.SmartStatus != "failed" {
		t.Errorf("exit 8: expected failed, got %v/%s", parsed.SmartAvailable, parsed.SmartStatus)
	}
}

func TestParseSmartctlJsonRecognizesSleepingDisk(t *testing.T) {
	parsed, err := parseSmartctlJson(`{
		"smartctl": {
			"version": [7, 3],
			"exit_status": 2,
			"messages": [{"string": "Device is in STANDBY mode, exit(2)"}]
		}
	}`, 512)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if !parsed.SmartAvailable || parsed.SmartStatus != "standby" {
		t.Errorf("sleeping disk = %v/%s, want available/standby", parsed.SmartAvailable, parsed.SmartStatus)
	}
}

func TestParseSmartctlJsonCriticalAttributes(t *testing.T) {
	parsed, err := parseSmartctlJson(`{
		"smartctl": {"version": [7, 4]},
		"smart_status": {"passed": true},
		"ata_smart_attributes": {"table": [
			{"id": 5, "raw": {"value": 2}},
			{"id": 197, "raw": {"value": 3}},
			{"id": 198, "raw": {"value": 0}},
			{"id": 187, "raw": {"value": 1}}
		]}
	}`, 512)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if parsed.ReallocatedSectors == nil || *parsed.ReallocatedSectors != 2 {
		t.Errorf("reallocated = %v, want 2", parsed.ReallocatedSectors)
	}
	if parsed.PendingSectors == nil || *parsed.PendingSectors != 3 {
		t.Errorf("pending = %v, want 3", parsed.PendingSectors)
	}
	if parsed.UncorrectableSectors == nil || *parsed.UncorrectableSectors != 0 {
		t.Errorf("uncorrectable = %v, want 0", parsed.UncorrectableSectors)
	}
	if parsed.ReportedUncorrectable == nil || *parsed.ReportedUncorrectable != 1 {
		t.Errorf("reportedUncorrectable = %v, want 1", parsed.ReportedUncorrectable)
	}
	if parsed.NVMeMediaErrors != nil || parsed.CriticalWarning != nil {
		t.Errorf("ATA disk should not have NVMe fields")
	}
}

func TestParseSmartctlJsonNVMeMediaErrors(t *testing.T) {
	parsed, err := parseSmartctlJson(`{
		"smartctl": {"version": [7, 4]},
		"smart_status": {"passed": true},
		"nvme_smart_health_information_log": {
			"media_and_data_integrity_errors": 5,
			"critical_warning": 2
		}
	}`, 512)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if parsed.NVMeMediaErrors == nil || *parsed.NVMeMediaErrors != 5 {
		t.Errorf("nvmeMediaErrors = %v, want 5", parsed.NVMeMediaErrors)
	}
	if parsed.CriticalWarning == nil || *parsed.CriticalWarning != 2 {
		t.Errorf("criticalWarning = %v, want 2", parsed.CriticalWarning)
	}
	if parsed.ReallocatedSectors != nil || parsed.PendingSectors != nil {
		t.Errorf("NVMe disk should not have ATA fields")
	}
}

func TestSmartCacheFollowsDiskIdentity(t *testing.T) {
	calls := 0
	execute := func(exe string, args []string) (string, error) {
		calls++
		return `{"smartctl":{"version":[7,4],"exit_status":0},"smart_status":{"passed":true}}`, nil
	}
	findExe := func(string) string { return "/usr/sbin/smartctl" }
	now := func() time.Time { return time.Unix(1000, 0) }

	base := Disk{Path: "/dev/smart-cache-test", Serial: "SERIAL-A", Model: "MODEL-A", Size: 1000, LogicalSectorSize: 512}
	readSmartData(base, findExe, now, execute)
	readSmartData(base, findExe, now, execute)
	readSmartData(Disk{Path: "/dev/smart-cache-test-2", Serial: "SERIAL-A", Model: "MODEL-A", Size: 1000, LogicalSectorSize: 512}, findExe, now, execute)
	readSmartData(Disk{Path: "/dev/smart-cache-test", Serial: "SERIAL-B", Model: "MODEL-A", Size: 1000, LogicalSectorSize: 512}, findExe, now, execute)
	readSmartData(Disk{Path: "/dev/smart-cache-test", Serial: "SERIAL-A", Model: "MODEL-B", Size: 1000, LogicalSectorSize: 512}, findExe, now, execute)
	readSmartData(Disk{Path: "/dev/smart-cache-test", Serial: "SERIAL-A", Model: "MODEL-A", Size: 2000, LogicalSectorSize: 512}, findExe, now, execute)

	if calls != 5 {
		t.Errorf("expected 5 smartctl calls, got %d", calls)
	}
}

func TestClassifyLatencyBuckets(t *testing.T) {
	cases := []struct {
		ms    float64
		err   int64
		label string
	}{
		{4.9, 0, "normal"},
		{19.9, 0, "normal"},
		{49.9, 0, "normal"},
		{50, 0, "slow"},
		{499.9, 0, "slow"},
		{500, 0, "severe"},
		{1, 1, "error"},
	}
	for _, c := range cases {
		if got := classifyLatency(c.ms, c.err); got != c.label {
			t.Errorf("classifyLatency(%v, %d) = %s, want %s", c.ms, c.err, got, c.label)
		}
	}
}

func TestCreateScanMapCoversWholeDevice(t *testing.T) {
	const size = 12000138625024
	m := createScanMap(size, 2048, 1)
	if len(m) != 2048 {
		t.Fatalf("map length = %d", len(m))
	}
	if m[0].Start != 0 || m[len(m)-1].End != size {
		t.Errorf("bounds wrong: %d..%d", m[0].Start, m[len(m)-1].End)
	}
	for i := 1; i < len(m); i++ {
		if m[i-1].End != m[i].Start {
			t.Fatalf("cell %d boundary broken", i)
		}
	}
}

func TestCreateSpeedPlanTargets(t *testing.T) {
	const diskSize = 2000000000000
	all, err := createSpeedPlan(diskSize, 1024, "all")
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if len(all.Targets) != 3 {
		t.Fatalf("targets = %d", len(all.Targets))
	}
	var sum int64
	for _, target := range all.Targets {
		sum += target.BlockCount
	}
	if sum != 256 || all.PlannedBlocks*4 != 1024 {
		t.Errorf("planned blocks wrong: sum=%d planned=%d", sum, all.PlannedBlocks)
	}
	if all.Targets[0].OffsetBlocks != 0 {
		t.Errorf("start offset = %d", all.Targets[0].OffsetBlocks)
	}
	if !(all.Targets[1].OffsetBlocks > all.Targets[0].OffsetBlocks) || !(all.Targets[2].OffsetBlocks > all.Targets[1].OffsetBlocks) {
		t.Errorf("offsets not increasing: %v", all.Targets)
	}

	middle, err := createSpeedPlan(diskSize, 1024, "middle")
	if err != nil {
		t.Fatalf("middle: %v", err)
	}
	if len(middle.Targets) != 1 || middle.Targets[0].ID != "middle" {
		t.Errorf("middle target wrong")
	}
	if middle.SegmentBlocks <= all.SegmentBlocks {
		t.Errorf("middle segment %d not > all segment %d", middle.SegmentBlocks, all.SegmentBlocks)
	}
}

func TestCreateSpeedPlanNeverOverlaps(t *testing.T) {
	blockSize := int64(speedBlockSize)
	for _, totalBlocks := range []int64{3, 4, 5, 64} {
		plan, err := createSpeedPlan(totalBlocks*blockSize, 256, "all")
		if err != nil {
			t.Fatalf("totalBlocks %d: %v", totalBlocks, err)
		}
		visited := map[int64]bool{}
		for _, target := range plan.Targets {
			for block := target.OffsetBlocks; block < target.OffsetBlocks+target.BlockCount; block++ {
				if visited[block] {
					t.Errorf("totalBlocks %d: block %d overlaps", totalBlocks, block)
				}
				visited[block] = true
			}
		}
		if int64(len(visited)) != totalBlocks {
			t.Errorf("totalBlocks %d: visited %d", totalBlocks, len(visited))
		}
	}
	if _, err := createSpeedPlan(2*blockSize, 256, "all"); err == nil {
		t.Error("expected DISK_TOO_SMALL error")
	}
}

func TestParseDd(t *testing.T) {
	output := "536870912 bytes copied, 2.00012 s, 268 MB/s\n"
	if got := parseDdProgress("123 bytes\r" + output); got != 536870912 {
		t.Errorf("progress = %d", got)
	}
	r := parseDdResult(output)
	if r == nil || r.Bytes != 536870912 || r.Seconds != 2.00012 || r.MBps != 268 {
		t.Errorf("result = %+v", r)
	}
}

func TestAppendBadRangesBounded(t *testing.T) {
	var collected []BadRange
	for i := 0; i < 150; i++ {
		collected = append(collected, BadRange{Offset: int64(i)})
	}
	var extra []BadRange
	for i := 0; i < 100; i++ {
		extra = append(extra, BadRange{Offset: int64(i + 150)})
	}
	appendBadRanges(&collected, extra, 200)
	appendBadRanges(&collected, []BadRange{{Offset: 999}}, 200)
	if len(collected) != 200 {
		t.Errorf("length = %d, want 200", len(collected))
	}
	if collected[len(collected)-1].Offset != 199 {
		t.Errorf("last offset = %d", collected[len(collected)-1].Offset)
	}
}
