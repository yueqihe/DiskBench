package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SAFE_DEVICE mirrors the Node whitelist: only flat /dev/<name> paths pass.
var safeDevicePattern = regexp.MustCompile(`^/dev/[A-Za-z0-9._+\-]+$`)
var safeStoragePathPattern = regexp.MustCompile(`^/vol[0-9]+$`)

func safeStorageMountPath(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || value == "/" || !strings.HasPrefix(value, "/") || path.Clean(value) != value || strings.ContainsAny(value, "\x00\r\n") {
		return false
	}
	for _, blocked := range []string{"/dev", "/proc", "/sys"} {
		if value == blocked || strings.HasPrefix(value, blocked+"/") {
			return false
		}
	}
	return true
}

const smartCacheTTL = 60 * time.Second
const smartStandbyCacheTTL = 5 * time.Second

// smartctlUnavailableExitMask matches the exit-status bits that mean "the
// device/command is unavailable" rather than "health data was reported".
const smartctlUnavailableExitMask = (1 << 0) | (1 << 1) | (1 << 2)

type smartCacheEntry struct {
	value     SmartData
	expiresAt time.Time
}

var smartCache = struct {
	sync.Mutex
	entries map[string]smartCacheEntry
}{entries: map[string]smartCacheEntry{}}

// Disk is the public device shape returned by /api/disks. SMART fields are
// filled by readSmartData and omitted when the source cannot provide them.
type Disk struct {
	ID                    string   `json:"id"`
	Name                  string   `json:"name"`
	Path                  string   `json:"path"`
	Size                  int64    `json:"size"`
	Model                 string   `json:"model"`
	Serial                string   `json:"serial"`
	Transport             string   `json:"transport"`
	NVMeNamespaceID       uint32   `json:"nvmeNamespaceId,omitempty"`
	LogicalSectorSize     int64    `json:"logicalSectorSize"`
	PhysicalSectorSize    int64    `json:"physicalSectorSize"`
	Rotational            bool     `json:"rotational"`
	ReadOnly              bool     `json:"readOnly"`
	Mountpoints           []string `json:"mountpoints"`
	FileSystems           []string `json:"fileSystems"`
	HasPartitions         bool     `json:"hasPartitions"`
	PartitionTable        string   `json:"partitionTable"`
	PartitionTableUnknown bool     `json:"partitionTableUnknown"`
	IsSystem              bool     `json:"isSystem"`

	SmartAvailable bool     `json:"smartAvailable"`
	SmartStatus    string   `json:"smartStatus"`
	Firmware       *string  `json:"firmware,omitempty"`
	TemperatureC   *float64 `json:"temperatureC,omitempty"`
	PowerOnHours   *float64 `json:"powerOnHours,omitempty"`
	PowerCycles    *float64 `json:"powerCycles,omitempty"`
	BytesWritten   *int64   `json:"bytesWritten,omitempty"`
	PercentageUsed *float64 `json:"percentageUsed,omitempty"`

	// Critical media attributes: the first line of bad-sector detection.
	ReallocatedSectors    *int64 `json:"reallocatedSectors,omitempty"`    // ATA 5
	PendingSectors        *int64 `json:"pendingSectors,omitempty"`        // ATA 197
	UncorrectableSectors  *int64 `json:"uncorrectableSectors,omitempty"`  // ATA 198
	ReportedUncorrectable *int64 `json:"reportedUncorrectable,omitempty"` // ATA 187
	NVMeMediaErrors       *int64 `json:"nvmeMediaErrors,omitempty"`       // NVMe media errors
	CriticalWarning       *int64 `json:"criticalWarning,omitempty"`       // NVMe critical_warning bitmask
}

// StorageSpace is a mounted fnOS data volume. Speed tests target this logical
// layer so an array is measured as a whole instead of benchmarking one member.
type StorageSpace struct {
	Path       string   `json:"path"`
	Source     string   `json:"source"`
	FileSystem string   `json:"fileSystem"`
	Layout     string   `json:"layout"`
	Members    []string `json:"members"`
	Size       int64    `json:"size"`
	FreeBytes  int64    `json:"freeBytes"`
	ReadOnly   bool     `json:"readOnly"`
}

type storageMount struct {
	Path       string
	Source     string
	FileSystem string
	Size       int64
	FreeBytes  int64
	ReadOnly   bool
}

// SmartData carries the SMART-derived subset merged into a Disk.
type SmartData struct {
	SmartAvailable bool     `json:"smartAvailable"`
	SmartStatus    string   `json:"smartStatus"`
	Firmware       *string  `json:"firmware,omitempty"`
	TemperatureC   *float64 `json:"temperatureC,omitempty"`
	PowerOnHours   *float64 `json:"powerOnHours,omitempty"`
	PowerCycles    *float64 `json:"powerCycles,omitempty"`
	BytesWritten   *int64   `json:"bytesWritten,omitempty"`
	PercentageUsed *float64 `json:"percentageUsed,omitempty"`

	ReallocatedSectors    *int64 `json:"reallocatedSectors,omitempty"`
	PendingSectors        *int64 `json:"pendingSectors,omitempty"`
	UncorrectableSectors  *int64 `json:"uncorrectableSectors,omitempty"`
	ReportedUncorrectable *int64 `json:"reportedUncorrectable,omitempty"`
	NVMeMediaErrors       *int64 `json:"nvmeMediaErrors,omitempty"`
	CriticalWarning       *int64 `json:"criticalWarning,omitempty"`
}

func (d *Disk) applySmart(s SmartData) {
	d.SmartAvailable = s.SmartAvailable
	d.SmartStatus = s.SmartStatus
	d.Firmware = s.Firmware
	d.TemperatureC = s.TemperatureC
	d.PowerOnHours = s.PowerOnHours
	d.PowerCycles = s.PowerCycles
	d.BytesWritten = s.BytesWritten
	d.PercentageUsed = s.PercentageUsed
	d.ReallocatedSectors = s.ReallocatedSectors
	d.PendingSectors = s.PendingSectors
	d.UncorrectableSectors = s.UncorrectableSectors
	d.ReportedUncorrectable = s.ReportedUncorrectable
	d.NVMeMediaErrors = s.NVMeMediaErrors
	d.CriticalWarning = s.CriticalWarning
}

// lsblkNode mirrors the lsblk --json tree. Numeric columns are json.Number so
// both string and number encodings are accepted.
type lsblkNode struct {
	Name        string      `json:"name"`
	Kname       string      `json:"kname"`
	Path        string      `json:"path"`
	Type        string      `json:"type"`
	Size        json.Number `json:"size"`
	Model       string      `json:"model"`
	Serial      string      `json:"serial"`
	Tran        string      `json:"tran"`
	LogSec      json.Number `json:"log-sec"`
	PhySec      json.Number `json:"phy-sec"`
	Rota        interface{} `json:"rota"`
	Ro          interface{} `json:"ro"`
	Mountpoints []string    `json:"mountpoints"`
	Mountpoint  string      `json:"mountpoint"`
	FSType      string      `json:"fstype"`
	PTType      string      `json:"pttype"`
	Children    []lsblkNode `json:"children"`
}

func toMountpoints(node lsblkNode) []string {
	var values []string
	values = append(values, node.Mountpoints...)
	if node.Mountpoint != "" {
		values = append(values, node.Mountpoint)
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func descendants(node lsblkNode) []lsblkNode {
	result := []lsblkNode{node}
	for _, child := range node.Children {
		result = append(result, descendants(child)...)
	}
	return result
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func toInt64(v interface{}) (int64, bool) {
	switch x := v.(type) {
	case json.Number:
		n, err := x.Int64()
		if err != nil {
			f, ferr := x.Float64()
			if ferr != nil {
				return 0, false
			}
			return int64(f), true
		}
		return n, true
	case float64:
		return int64(x), true
	case int64:
		return x, true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

func toFloat64(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}

func boolFlag(v interface{}) bool {
	switch x := v.(type) {
	case bool:
		return x
	case float64:
		return x == 1
	case json.Number:
		return x.String() == "1"
	case string:
		return x == "1"
	}
	return false
}

func flattenDisks(blockdevices []lsblkNode, systemPaths map[string]bool) []Disk {
	return flattenDisksWithPartitionInfo(blockdevices, systemPaths, true)
}

func flattenDisksWithPartitionInfo(blockdevices []lsblkNode, systemPaths map[string]bool, partitionInfoKnown bool) []Disk {
	disks := make([]Disk, 0)
	for _, node := range blockdevices {
		if node.Type != "disk" {
			continue
		}
		if isHiddenVirtualOrEmmcPartition(node) {
			continue
		}
		tree := descendants(node)
		var mountpoints []string
		var fileSystems []string
		for _, item := range tree {
			mountpoints = append(mountpoints, toMountpoints(item)...)
			if fileSystem := strings.TrimSpace(item.FSType); fileSystem != "" {
				fileSystems = append(fileSystems, fileSystem)
			}
		}
		mountpoints = uniqueStrings(mountpoints)
		fileSystems = uniqueStrings(fileSystems)

		devicePath := node.Path
		if devicePath == "" {
			kname := node.Kname
			if kname == "" {
				kname = node.Name
			}
			devicePath = "/dev/" + kname
		}
		size, _ := toInt64(node.Size)
		if size <= 0 {
			size = 0
		}
		logSec, _ := toInt64(node.LogSec)
		if logSec <= 0 {
			logSec = 512
		}
		phySec, _ := toInt64(node.PhySec)
		if phySec <= 0 {
			phySec = logSec
		}

		isSystem := false
		for _, item := range tree {
			itemPath := item.Path
			if itemPath == "" && item.Kname != "" {
				itemPath = "/dev/" + item.Kname
			}
			mp := toMountpoints(item)
			hasRoot := false
			for _, m := range mp {
				if m == "/" {
					hasRoot = true
					break
				}
			}
			if hasRoot || systemPaths[itemPath] {
				isSystem = true
				break
			}
		}

		disk := Disk{
			ID:                    devicePath,
			Name:                  node.Name,
			Path:                  devicePath,
			Size:                  size,
			Model:                 strings.TrimSpace(node.Model),
			Serial:                strings.TrimSpace(node.Serial),
			Transport:             strings.TrimSpace(node.Tran),
			LogicalSectorSize:     logSec,
			PhysicalSectorSize:    phySec,
			Rotational:            boolFlag(node.Rota),
			ReadOnly:              boolFlag(node.Ro),
			Mountpoints:           mountpoints,
			FileSystems:           fileSystems,
			HasPartitions:         len(node.Children) > 0,
			PartitionTable:        strings.TrimSpace(node.PTType),
			PartitionTableUnknown: !partitionInfoKnown,
			IsSystem:              isSystem,
		}
		if disk.Name == "" {
			disk.Name = path.Base(devicePath)
		}
		if disk.Model == "" {
			disk.Model = "未知型号"
		}
		if disk.Transport == "" {
			disk.Transport = "unknown"
		}
		if !safeDevicePattern.MatchString(disk.Path) || size <= 0 {
			continue
		}
		disks = append(disks, disk)
	}
	return disks
}

// isHiddenVirtualOrEmmcPartition filters devices that are not usable media:
// eMMC boot/RPMB areas are firmware-reserved partitions and zram is a
// compressed-memory block device. The main mmcblkN data disk remains visible.
func isHiddenVirtualOrEmmcPartition(node lsblkNode) bool {
	name := strings.ToLower(strings.TrimSpace(node.Name))
	if name == "" {
		name = strings.ToLower(strings.TrimSpace(node.Kname))
	}
	if strings.HasPrefix(name, "zram") {
		return true
	}
	if strings.Contains(name, "boot") || strings.Contains(name, "rpmb") {
		return strings.HasPrefix(name, "mmcblk") || strings.HasPrefix(name, "mmc")
	}
	return false
}

// finiteNumber returns the first finite number among the given values.
func finiteNumber(values ...interface{}) *float64 {
	for _, v := range values {
		if v == nil {
			continue
		}
		f, ok := toFloat64(v)
		if ok {
			return &f
		}
	}
	return nil
}

type smartAttribute struct {
	ID  int64       `json:"id"`
	Raw interface{} `json:"raw"`
}

type smartctlRawValue struct {
	Value interface{} `json:"value"`
}

type smartctlOutput struct {
	Smartctl *struct {
		Version    []int  `json:"version"`
		ExitStatus *int64 `json:"exit_status"`
		Messages   []struct {
			String string `json:"string"`
		} `json:"messages"`
	} `json:"smartctl"`
	SmartStatus *struct {
		Passed *bool `json:"passed"`
	} `json:"smart_status"`
	FirmwareVersion *string `json:"firmware_version"`
	Temperature     *struct {
		Current interface{} `json:"current"`
	} `json:"temperature"`
	PowerOnTime *struct {
		Hours interface{} `json:"hours"`
	} `json:"power_on_time"`
	PowerCycleCount interface{} `json:"power_cycle_count"`
	NVMeSmartHealth *struct {
		DataUnitsWritten         interface{} `json:"data_units_written"`
		PowerOnHours             interface{} `json:"power_on_hours"`
		PowerCycles              interface{} `json:"power_cycles"`
		Temperature              interface{} `json:"temperature"`
		PercentageUsed           interface{} `json:"percentage_used"`
		MediaDataIntegrityErrors interface{} `json:"media_and_data_integrity_errors"`
		MediaErrors              interface{} `json:"media_errors"`
		CriticalWarning          interface{} `json:"critical_warning"`
	} `json:"nvme_smart_health_information_log"`
	AtaSmartAttributes *struct {
		Table []smartAttribute `json:"table"`
	} `json:"ata_smart_attributes"`
}

func findSmartAttribute(data *smartctlOutput, ids ...int64) *smartAttribute {
	table := data.AtaSmartAttributes
	if table == nil {
		return nil
	}
	for i := range table.Table {
		attr := &table.Table[i]
		for _, id := range ids {
			if attr.ID == id {
				return attr
			}
		}
	}
	return nil
}

func rawValueOf(attr *smartAttribute) interface{} {
	if attr == nil {
		return nil
	}
	if raw, ok := attr.Raw.(map[string]interface{}); ok {
		return raw["value"]
	}
	return nil
}

func smartCacheKey(disk Disk) string {
	return disk.Path + "\x00" + disk.Serial + "\x00" + disk.Model + "\x00" + strconv.FormatInt(disk.Size, 10)
}

func parseSmartctlJson(output string, logicalSectorSize int64) (SmartData, error) {
	var data smartctlOutput
	if err := json.Unmarshal([]byte(output), &data); err != nil {
		return SmartData{}, err
	}

	nvme := data.NVMeSmartHealth
	var nvmeDataUnitsWritten, nvmePowerOnHours, nvmePowerCycles, nvmeTemperature, nvmePercentageUsed interface{}
	var nvmeMediaErrors, nvmeCriticalWarning interface{}
	if nvme != nil {
		nvmeDataUnitsWritten = nvme.DataUnitsWritten
		nvmePowerOnHours = nvme.PowerOnHours
		nvmePowerCycles = nvme.PowerCycles
		nvmeTemperature = nvme.Temperature
		nvmePercentageUsed = nvme.PercentageUsed
		nvmeMediaErrors = nvme.MediaDataIntegrityErrors
		if nvmeMediaErrors == nil {
			nvmeMediaErrors = nvme.MediaErrors
		}
		nvmeCriticalWarning = nvme.CriticalWarning
	}

	powerOnAttribute := findSmartAttribute(&data, 9)
	powerCycleAttribute := findSmartAttribute(&data, 12)
	writtenAttribute := findSmartAttribute(&data, 241, 246)
	temperatureAttribute := findSmartAttribute(&data, 190, 194)
	reallocatedAttribute := findSmartAttribute(&data, 5)
	pendingAttribute := findSmartAttribute(&data, 197)
	uncorrectableAttribute := findSmartAttribute(&data, 198)
	reportedUncorrectableAttribute := findSmartAttribute(&data, 187)

	var temperatureCurrent, powerOnTimeHours interface{}
	if data.Temperature != nil {
		temperatureCurrent = data.Temperature.Current
	}
	if data.PowerOnTime != nil {
		powerOnTimeHours = data.PowerOnTime.Hours
	}

	dataUnitsWritten := finiteNumber(nvmeDataUnitsWritten)
	writtenLbas := finiteNumber(rawValueOf(writtenAttribute))

	var passed *bool
	if data.SmartStatus != nil {
		passed = data.SmartStatus.Passed
	}

	commandUnavailable := false
	standby := false
	if data.Smartctl != nil && data.Smartctl.ExitStatus != nil {
		commandUnavailable = (*data.Smartctl.ExitStatus & smartctlUnavailableExitMask) != 0
		if *data.Smartctl.ExitStatus&(1<<1) != 0 {
			for _, message := range data.Smartctl.Messages {
				text := strings.ToLower(message.String)
				if strings.Contains(text, "standby") || strings.Contains(text, "sleep") {
					standby = true
					break
				}
			}
		}
	}
	smartAvailable := data.Smartctl != nil && (!commandUnavailable || standby)

	smartStatus := "unavailable"
	if standby {
		smartStatus = "standby"
	} else if smartAvailable {
		if passed == nil {
			smartStatus = "unknown"
		} else if *passed {
			smartStatus = "passed"
		} else {
			smartStatus = "failed"
		}
	}

	firmware := ""
	if data.FirmwareVersion != nil {
		firmware = strings.TrimSpace(*data.FirmwareVersion)
	}

	temperatureC := finiteNumber(temperatureCurrent, nvmeTemperature, rawValueOf(temperatureAttribute))
	powerOnHours := finiteNumber(powerOnTimeHours, nvmePowerOnHours, rawValueOf(powerOnAttribute))
	powerCycles := finiteNumber(data.PowerCycleCount, nvmePowerCycles, rawValueOf(powerCycleAttribute))

	var bytesWritten *int64
	if dataUnitsWritten != nil {
		v := int64(*dataUnitsWritten * 512000)
		bytesWritten = &v
	} else if writtenLbas != nil {
		v := int64(*writtenLbas * float64(logicalSectorSize))
		bytesWritten = &v
	}

	percentageUsed := finiteNumber(nvmePercentageUsed)

	return SmartData{
		SmartAvailable: smartAvailable,
		SmartStatus:    smartStatus,
		Firmware:       strPtr(firmware),
		TemperatureC:   temperatureC,
		PowerOnHours:   powerOnHours,
		PowerCycles:    powerCycles,
		BytesWritten:   bytesWritten,
		PercentageUsed: percentageUsed,

		ReallocatedSectors:    toInt64Ptr(rawValueOf(reallocatedAttribute)),
		PendingSectors:        toInt64Ptr(rawValueOf(pendingAttribute)),
		UncorrectableSectors:  toInt64Ptr(rawValueOf(uncorrectableAttribute)),
		ReportedUncorrectable: toInt64Ptr(rawValueOf(reportedUncorrectableAttribute)),
		NVMeMediaErrors:       toInt64Ptr(nvmeMediaErrors),
		CriticalWarning:       toInt64Ptr(nvmeCriticalWarning),
	}, nil
}

func strPtr(s string) *string { return &s }

func toInt64Ptr(v interface{}) *int64 {
	if v == nil {
		return nil
	}
	n, ok := toInt64(v)
	if !ok {
		return nil
	}
	return &n
}

type smartExecutor func(exe string, args []string) (string, error)

func execSmartctl(exe string, args []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	out, err := cmd.Output()
	if err != nil {
		// smartctl returns non-zero for unavailable devices and for failed
		// health; the JSON payload on stdout is still meaningful.
		return string(out), nil
	}
	return string(out), nil
}

func readSmartData(disk Disk, findExe func(string) string, nowFn func() time.Time, execute smartExecutor) SmartData {
	unavailable := SmartData{SmartAvailable: false, SmartStatus: "unavailable"}
	if execute == nil {
		execute = execSmartctl
	}
	exe := findExe("smartctl")
	if exe == "" {
		return unavailable
	}

	key := smartCacheKey(disk)
	now := nowFn()
	smartCache.Lock()
	if cached, ok := smartCache.entries[key]; ok && cached.expiresAt.After(now) {
		smartCache.Unlock()
		return cached.value
	}
	smartCache.Unlock()

	stdout, _ := execute(exe, []string{"--all", "--json", "--nocheck=standby", disk.Path})
	value := unavailable
	if strings.TrimSpace(stdout) != "" {
		if parsed, err := parseSmartctlJson(stdout, disk.LogicalSectorSize); err == nil {
			value = parsed
		}
	}

	ttl := smartCacheTTL
	if value.SmartStatus == "standby" {
		ttl = smartStandbyCacheTTL
	}
	smartCache.Lock()
	smartCache.entries[key] = smartCacheEntry{value: value, expiresAt: now.Add(ttl)}
	smartCache.Unlock()
	return value
}

// wakeDiskWithSmartctl explicitly queries SMART without standby protection.
// Normal inventory refreshes still use --nocheck=standby and preserve sleep.
func wakeDiskWithSmartctl(disk Disk, findExe func(string) string) error {
	exe := findExe("smartctl")
	if exe == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "--all", "--json", disk.Path)
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// SMART health findings use non-zero exit bits even after a successful
	// query. Receiving JSON is enough to know the command completed.
	if strings.TrimSpace(string(out)) != "" {
		if value, parseErr := parseSmartctlJson(string(out), disk.LogicalSectorSize); parseErr == nil {
			now := time.Now()
			smartCache.Lock()
			smartCache.entries[smartCacheKey(disk)] = smartCacheEntry{value: value, expiresAt: now.Add(smartCacheTTL)}
			smartCache.Unlock()
		}
		return nil
	}
	return err
}

func findExecutable(name string) string {
	searchPath := os.Getenv("PATH")
	if searchPath == "" {
		searchPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	for _, directory := range filepath.SplitList(searchPath) {
		candidate := filepath.Join(directory, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}

func getSystemPaths() map[string]bool {
	paths := map[string]bool{}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rootSource, err := exec.CommandContext(ctx, "findmnt", "-n", "-o", "SOURCE", "/").Output()
	if err != nil {
		return paths
	}
	source := strings.TrimSpace(string(rootSource))
	if !strings.HasPrefix(source, "/dev/") {
		return paths
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	out, err := exec.CommandContext(ctx2, "lsblk", "-s", "-n", "-p", "-o", "PATH", source).Output()
	if err != nil {
		return paths
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			paths[line] = true
		}
	}
	return paths
}

func parseStorageMounts(output string) []storageMount {
	var mounts []storageMount
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		for i := range fields {
			fields[i] = strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(fields[i])
		}
		if !safeStorageMountPath(fields[0]) {
			continue
		}
		size, sizeErr := strconv.ParseInt(fields[3], 10, 64)
		freeBytes, freeErr := strconv.ParseInt(fields[4], 10, 64)
		if sizeErr != nil || freeErr != nil || size <= 0 || freeBytes < 0 {
			continue
		}
		readOnly := false
		if len(fields) >= 6 {
			for _, option := range strings.Split(fields[5], ",") {
				if option == "ro" {
					readOnly = true
					break
				}
			}
		}
		mounts = append(mounts, storageMount{
			Path: fields[0], Source: fields[1], FileSystem: fields[2], Size: size, FreeBytes: freeBytes, ReadOnly: readOnly,
		})
	}
	return mounts
}

func storageDiskAncestors(source string) ([]string, string) {
	// Btrfs may suffix its source with a subvolume path, which lsblk does not accept.
	if index := strings.Index(source, "["); index >= 0 {
		source = source[:index]
	}
	if !strings.HasPrefix(source, "/dev/") {
		return nil, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsblk", "-s", "-n", "-p", "-o", "PATH,TYPE", source).Output()
	if err != nil {
		return nil, ""
	}
	var members []string
	layout := ""
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		kind := strings.ToLower(fields[1])
		if kind == "disk" {
			members = append(members, fields[0])
		}
		if strings.HasPrefix(kind, "raid") {
			layout = strings.ToUpper(kind)
		}
	}
	return uniqueStrings(members), layout
}

func zfsPoolMembers(source, zpoolExe string) ([]string, string) {
	pool := strings.Split(strings.TrimSpace(source), "/")[0]
	if pool == "" || zpoolExe == "" {
		return nil, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, zpoolExe, "status", "-P", pool).Output()
	if err != nil {
		return nil, ""
	}
	var members []string
	layout := ""
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "mirror-") {
			layout = "MIRROR"
		} else if strings.HasPrefix(lower, "raidz") || strings.HasPrefix(lower, "draid") {
			layout = strings.ToUpper(strings.Split(lower, "-")[0])
		}
		if !strings.HasPrefix(name, "/dev/") {
			continue
		}
		resolved := name
		if value, resolveErr := filepath.EvalSymlinks(name); resolveErr == nil {
			resolved = value
		}
		ancestors, _ := storageDiskAncestors(resolved)
		members = append(members, ancestors...)
	}
	return uniqueStrings(members), layout
}

func deriveStorageSpaces(disks []Disk, mounts []storageMount) []StorageSpace {
	byPath := make(map[string]*StorageSpace)
	for _, mount := range mounts {
		if !safeStorageMountPath(mount.Path) {
			continue
		}
		if _, exists := byPath[mount.Path]; exists {
			continue
		}
		byPath[mount.Path] = &StorageSpace{
			Path: mount.Path, Source: mount.Source, FileSystem: strings.ToLower(mount.FileSystem),
			Layout: "single", Members: []string{}, Size: mount.Size, FreeBytes: mount.FreeBytes, ReadOnly: mount.ReadOnly,
		}
	}
	for _, disk := range disks {
		for _, mountpoint := range disk.Mountpoints {
			space := byPath[mountpoint]
			if space != nil {
				space.Members = append(space.Members, disk.Path)
			}
		}
	}
	spaces := make([]StorageSpace, 0, len(byPath))
	for _, space := range byPath {
		space.Members = uniqueStrings(space.Members)
		if len(space.Members) > 1 {
			space.Layout = "raid"
		}
		spaces = append(spaces, *space)
	}
	sort.Slice(spaces, func(i, j int) bool { return spaces[i].Path < spaces[j].Path })
	return spaces
}

func listStorageSpaces(disks []Disk, findExe func(string) string) ([]StorageSpace, error) {
	if findExe == nil {
		findExe = findExecutable
	}
	findmnt := findExe("findmnt")
	if findmnt == "" {
		return []StorageSpace{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, findmnt, "-rn", "-b", "-o", "TARGET,SOURCE,FSTYPE,SIZE,AVAIL,OPTIONS").Output()
	if err != nil {
		// Older findmnt versions may not expose OPTIONS in this output mode.
		out, err = exec.CommandContext(ctx, findmnt, "-rn", "-b", "-o", "TARGET,SOURCE,FSTYPE,SIZE,AVAIL").Output()
		if err != nil {
			return nil, fmt.Errorf("list mounted storage spaces: %w", err)
		}
	}
	spaces := deriveStorageSpaces(disks, parseStorageMounts(string(out)))
	systemPaths := getSystemPaths()
	disksByPath := make(map[string]Disk, len(disks))
	for _, disk := range disks {
		disksByPath[disk.Path] = disk
	}
	validSpaces := make([]StorageSpace, 0, len(spaces))
	for i := range spaces {
		var members []string
		layout := ""
		if spaces[i].FileSystem == "zfs" {
			members, layout = zfsPoolMembers(spaces[i].Source, findExe("zpool"))
		} else {
			members, layout = storageDiskAncestors(spaces[i].Source)
		}
		if len(members) > 0 {
			spaces[i].Members = members
		}
		if len(spaces[i].Members) <= 1 {
			spaces[i].Layout = "single"
		} else if layout != "" {
			spaces[i].Layout = strings.ToLower(layout)
		} else {
			spaces[i].Layout = "raid"
		}
		eligible := len(spaces[i].Members) > 0
		for _, member := range spaces[i].Members {
			if systemPaths[member] || disksByPath[member].IsSystem {
				eligible = false
				break
			}
		}
		if eligible {
			validSpaces = append(validSpaces, spaces[i])
		}
	}
	return validSpaces, nil
}

func resolveStorageSpace(storagePath string, findExe func(string) string) (*StorageSpace, error) {
	if !safeStorageMountPath(storagePath) {
		return nil, nil
	}
	spaces, err := listStorageSpaces(nil, findExe)
	if err != nil {
		return nil, err
	}
	for i := range spaces {
		if spaces[i].Path == storagePath {
			return &spaces[i], nil
		}
	}
	return nil, nil
}

func listDisks(findExe func(string) string) ([]Disk, error) {
	if findExe == nil {
		findExe = findExecutable
	}
	columns := "NAME,KNAME,PATH,TYPE,SIZE,MODEL,SERIAL,TRAN,LOG-SEC,PHY-SEC,ROTA,RO,FSTYPE,PTTYPE,MOUNTPOINTS,MOUNTPOINT"
	out, err := exec.Command("lsblk", "--json", "--bytes", "-o", columns).Output()
	if err != nil {
		// Older lsblk lacks MOUNTPOINTS; retry without it.
		out, err = exec.Command("lsblk", "--json", "--bytes", "-o", strings.ReplaceAll(columns, ",MOUNTPOINTS", "")).Output()
		if err != nil {
			// Keep listing disks on older systems, but do not permit raw writes
			// when their partition-table state cannot be established.
			columns = strings.ReplaceAll(columns, ",PTTYPE", "")
			out, err = exec.Command("lsblk", "--json", "--bytes", "-o", strings.ReplaceAll(columns, ",MOUNTPOINTS", "")).Output()
			if err != nil {
				return nil, err
			}
		}
	}
	var parsed struct {
		Blockdevices []lsblkNode `json:"blockdevices"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, err
	}
	systemPaths := getSystemPaths()
	disks := flattenDisksWithPartitionInfo(parsed.Blockdevices, systemPaths, strings.Contains(columns, ",PTTYPE"))

	var wg sync.WaitGroup
	for i := range disks {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			disks[idx].applySmart(readSmartData(disks[idx], findExe, time.Now, execSmartctl))
		}(i)
	}
	wg.Wait()
	return disks, nil
}

func resolveDisk(devicePath string, findExe func(string) string) (*Disk, error) {
	if !safeDevicePattern.MatchString(devicePath) {
		return nil, nil
	}
	disks, err := listDisks(findExe)
	if err != nil {
		return nil, err
	}
	for i := range disks {
		if disks[i].Path == devicePath {
			return &disks[i], nil
		}
	}
	return nil, nil
}
