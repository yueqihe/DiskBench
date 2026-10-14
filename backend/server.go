package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const maxJSONBytes = 32 * 1024

var (
	jobDetailRe       = regexp.MustCompile(`(?i)^/api/jobs/[0-9a-f-]+$`)
	jobCancelRe       = regexp.MustCompile(`(?i)^/api/jobs/[0-9a-f-]+/cancel$`)
	jobScanProgressRe = regexp.MustCompile(`(?i)^/api/jobs/[0-9a-f-]+/scan-progress$`)
	jobScanIssuesRe   = regexp.MustCompile(`(?i)^/api/jobs/[0-9a-f-]+/scan-issues$`)
	jobScanReportRe   = regexp.MustCompile(`(?i)^/api/jobs/[0-9a-f-]+/scan-report$`)
)

// jobsStore is the contract the HTTP layer needs from the job engine.
type jobsStore interface {
	list(summary bool) []interface{}
	listCurrent(summary bool, includeIDs []string) []interface{}
	get(id string) *Job
	scanProgress(id string, after int) (*ScanProgressSnapshot, error)
	create(typeName, devicePath string, options map[string]interface{}) (*Job, error)
	cancel(id string) (*Job, error)
	remove(id string) error
	shutdown()
}

type apiServer struct {
	jobs             jobsStore
	localDev         bool
	mockDev          bool
	mockDisks        []Disk
	listDisksFn      func(func(string) string) ([]Disk, error)
	listSpacesFn     func([]Disk, func(string) string) ([]StorageSpace, error)
	findExecutableFn func(string) string
	smartExecutorFn  smartExecutor
	prefix           string
	publicDir        string
	logger           logger
}

type apiServerOptions struct {
	Jobs           jobsStore
	LocalDev       bool
	MockDev        bool
	MockDisks      []Disk
	ListDisks      func(func(string) string) ([]Disk, error)
	ListSpaces     func([]Disk, func(string) string) ([]StorageSpace, error)
	FindExecutable func(string) string
	SmartExecutor  smartExecutor
	Prefix         string
	PublicDir      string
	Logger         logger
}

func newAPIServer(options apiServerOptions) *apiServer {
	prefix := options.Prefix
	if prefix == "" {
		prefix = "/app/diskbench"
	}
	logger := options.Logger
	if logger == nil {
		logger = defaultLogger{}
	}
	if options.MockDisks == nil {
		options.MockDisks = defaultMockDisks()
	}
	if options.ListDisks == nil {
		options.ListDisks = listDisks
	}
	if options.ListSpaces == nil {
		options.ListSpaces = listStorageSpaces
	}
	if options.FindExecutable == nil {
		options.FindExecutable = findExecutable
	}
	return &apiServer{
		jobs:             options.Jobs,
		localDev:         options.LocalDev,
		mockDev:          options.MockDev,
		mockDisks:        options.MockDisks,
		listDisksFn:      options.ListDisks,
		listSpacesFn:     options.ListSpaces,
		findExecutableFn: options.FindExecutable,
		smartExecutorFn:  options.SmartExecutor,
		prefix:           prefix,
		publicDir:        options.PublicDir,
		logger:           logger,
	}
}

func defaultMockDisks() []Disk {
	fw1 := "5B2QJXD7"
	fw2 := "85.00A85"
	fw3 := "SVT03B6Q"
	return []Disk{
		{
			ID: "/dev/nvme0n1", Name: "nvme0n1", Path: "/dev/nvme0n1", Size: 2000398934016,
			Model: "Samsung SSD 990 PRO 2TB", Serial: "S7DNNX0W123456", Transport: "nvme",
			LogicalSectorSize: 512, PhysicalSectorSize: 4096,
			Rotational: false, ReadOnly: false, Mountpoints: []string{"/", "/boot"}, FileSystems: []string{"ext4"}, HasPartitions: true, IsSystem: true,
			SmartAvailable: true, SmartStatus: "passed", Firmware: &fw1,
			TemperatureC: floatPtr(38), PowerOnHours: floatPtr(1284), PowerCycles: floatPtr(326),
			BytesWritten: intPtr(18_640_000_000_000), PercentageUsed: floatPtr(4),
			NVMeMediaErrors: intPtr(0), CriticalWarning: intPtr(0),
		},
		{
			ID: "/dev/sda", Name: "sda", Path: "/dev/sda", Size: 12000138625024,
			Model: "WDC WD120EFBX-68B0EN0", Serial: "5QJ0A1BC", Transport: "sata",
			LogicalSectorSize: 512, PhysicalSectorSize: 4096,
			Rotational: true, ReadOnly: false, Mountpoints: []string{"/vol1"}, FileSystems: []string{"btrfs"}, HasPartitions: true, IsSystem: false,
			SmartAvailable: true, SmartStatus: "passed", Firmware: &fw2,
			TemperatureC: floatPtr(34), PowerOnHours: floatPtr(9876), PowerCycles: floatPtr(214),
			BytesWritten: nil, PercentageUsed: nil,
			ReallocatedSectors: intPtr(0), PendingSectors: intPtr(0),
			UncorrectableSectors: intPtr(0), ReportedUncorrectable: intPtr(0),
		},
		{
			ID: "/dev/sdb", Name: "sdb", Path: "/dev/sdb", Size: 4000787030016,
			Model: "Samsung SSD 870 EVO 4TB", Serial: "S6BCNX0R789012", Transport: "sata",
			LogicalSectorSize: 512, PhysicalSectorSize: 4096,
			Rotational: false, ReadOnly: false, Mountpoints: []string{}, IsSystem: false,
			SmartAvailable: true, SmartStatus: "passed", Firmware: &fw3,
			TemperatureC: floatPtr(31), PowerOnHours: floatPtr(4280), PowerCycles: floatPtr(488),
			BytesWritten: intPtr(32_180_000_000_000), PercentageUsed: nil,
			ReallocatedSectors: intPtr(2), PendingSectors: intPtr(3),
			UncorrectableSectors: intPtr(0), ReportedUncorrectable: intPtr(1),
		},
	}
}

func defaultMockSpaces(disks []Disk) []StorageSpace {
	var mounts []storageMount
	seen := map[string]bool{}
	for _, disk := range disks {
		for _, mountpoint := range disk.Mountpoints {
			if safeStoragePathPattern.MatchString(mountpoint) && !seen[mountpoint] {
				seen[mountpoint] = true
				mounts = append(mounts, storageMount{
					Path: mountpoint, Source: disk.Path, FileSystem: "btrfs", Size: disk.Size, FreeBytes: disk.Size / 2,
				})
			}
		}
	}
	return deriveStorageSpaces(disks, mounts)
}

func (a *apiServer) applyHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	frameAncestors := "'self'"
	frameOptions := "SAMEORIGIN"
	if a.localDev {
		// Local preview may be embedded in cross-origin dev tooling (e.g. a
		// sidebar iframe); the production gateway stays same-origin, so keep
		// the strict clickjacking policy there.
		frameAncestors = "*"
		frameOptions = ""
	}
	if frameOptions != "" {
		w.Header().Set("X-Frame-Options", frameOptions)
	} else {
		w.Header().Del("X-Frame-Options")
	}
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; font-src 'self'; frame-ancestors "+frameAncestors)
	w.Header().Set("Cache-Control", "no-store")
}

func (a *apiServer) json(w http.ResponseWriter, status int, payload interface{}) {
	a.applyHeaders(w)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func (a *apiServer) apiError(w http.ResponseWriter, err *jobError) {
	a.json(w, err.statusCode, map[string]interface{}{"error": err.message, "code": err.apiCode})
}

func (a *apiServer) isAdmin(r *http.Request) bool {
	if a.localDev {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Trim-Isadmin")), "true")
}

func relativePath(pathname, prefix string) string {
	if pathname == prefix {
		return "/"
	}
	if strings.HasPrefix(pathname, prefix+"/") {
		return pathname[len(prefix):]
	}
	return pathname
}

func allowedApiMethods(pathname string) ([]string, bool) {
	switch {
	case pathname == "/api/health" || pathname == "/api/disks" || pathname == "/api/disks/smart":
		return []string{"GET"}, true
	case pathname == "/api/jobs":
		return []string{"GET", "POST"}, true
	case jobDetailRe.MatchString(pathname):
		return []string{"GET", "DELETE"}, true
	case jobScanProgressRe.MatchString(pathname), jobScanIssuesRe.MatchString(pathname), jobScanReportRe.MatchString(pathname):
		return []string{"GET"}, true
	case jobCancelRe.MatchString(pathname):
		return []string{"POST"}, true
	}
	return nil, false
}

func mapJobError(err error) *jobError {
	if err == nil {
		return newJobError("请求处理失败", "INTERNAL_ERROR", 500)
	}
	var je *jobError
	if errors.As(err, &je) {
		return je
	}
	known := map[string][2]interface{}{
		"已有任务正在运行，请等待完成或先取消": {409, "ACTIVE_JOB_CONFLICT"},
		"所选磁盘信息已变化，请刷新并重新确认": {409, "DISK_IDENTITY_CHANGED"},
		"任务不存在":            {404, "JOB_NOT_FOUND"},
		"任务已结束，无法取消":       {409, "JOB_NOT_ACTIVE"},
		"磁盘不存在或已离线，请刷新后重试": {404, "DISK_NOT_FOUND"},
		"不支持的任务类型":         {400, "INVALID_ARGUMENT"},
		"应用服务已停止，无法创建任务":   {503, "SERVICE_UNAVAILABLE"},
		"系统未找到 dd":         {503, "CAPABILITY_UNAVAILABLE"},
	}
	message := err.Error()
	if mapped, ok := known[message]; ok {
		status := mapped[0].(int)
		code := mapped[1].(string)
		return newJobError(message, code, status)
	}
	return newJobError("服务器内部错误", "INTERNAL_ERROR", 500)
}

func readJson(w http.ResponseWriter, r *http.Request) (map[string]interface{}, error) {
	contentType := r.Header.Get("Content-Type")
	mediaType := strings.TrimSpace(strings.Split(contentType, ";")[0])
	if !strings.EqualFold(mediaType, "application/json") {
		return nil, newJobError("请求必须使用 application/json", "UNSUPPORTED_MEDIA_TYPE", 415)
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, newJobError("请求内容过大", "PAYLOAD_TOO_LARGE", 413)
		}
		return nil, newJobError("请求读取失败", "INVALID_JSON", 400)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return map[string]interface{}{}, nil
	}
	var parsed interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, newJobError("JSON 格式无效", "INVALID_JSON", 400)
	}
	obj, ok := parsed.(map[string]interface{})
	if !ok {
		return nil, newJobError("请求参数必须是 JSON 对象", "INVALID_ARGUMENT", 400)
	}
	return obj, nil
}

func validateCreateJobBody(body map[string]interface{}) error {
	typeName, _ := body["type"].(string)
	if typeName == "write" {
		return newJobError("深度检测已停用，请使用坏道扫描及慢扇区修复", "JOB_TYPE_RETIRED", 410)
	}
	if typeName != "scan" && typeName != "speed" {
		return newJobError("type 必须是 scan 或 speed", "INVALID_ARGUMENT", 400)
	}
	if raw, exists := body["resumeFrom"]; exists {
		id, valid := raw.(string)
		if !valid || typeName != "scan" || !jobIDPattern.MatchString(id) {
			return newJobError("resumeFrom 必须是有效的扫描任务 ID", "INVALID_ARGUMENT", 400)
		}
	}
	device, _ := body["device"].(string)
	device = strings.TrimSpace(device)
	if device == "" || len(device) > 4096 || strings.ContainsRune(device, '\x00') {
		return newJobError("device 必须是有效的磁盘路径", "INVALID_ARGUMENT", 400)
	}
	if v, ok := body["scanMode"]; ok && v != nil {
		mode, _ := v.(string)
		if mode != "quick" && mode != "full" {
			return newJobError("scanMode 必须是 quick 或 full", "INVALID_ARGUMENT", 400)
		}
	}
	if v, ok := body["scanExactSectors"]; ok && v != nil {
		if _, ok := v.(bool); !ok {
			return newJobError("scanExactSectors 必须是布尔值", "INVALID_ARGUMENT", 400)
		}
	}
	if v, ok := body["scanSelfTest"]; ok && v != nil {
		if _, ok := v.(bool); !ok {
			return newJobError("scanSelfTest 必须是布尔值", "INVALID_ARGUMENT", 400)
		}
		if v == true && typeName != "scan" {
			return newJobError("固件自检仅适用于只读扫描", "INVALID_ARGUMENT", 400)
		}
	}
	if v, ok := body["scanRepair"]; ok {
		enabled, valid := v.(bool)
		if !valid || (enabled && typeName != "scan") {
			return newJobError("scanRepair 必须是布尔值且仅用于扫描", "INVALID_ARGUMENT", 400)
		}
	}
	if v, ok := body["scanRepairAfterMs"]; ok {
		_, valid := parseScanRepairThreshold(v)
		if !valid {
			return newJobError("修复阈值必须是 1～60000 毫秒的整数", "INVALID_ARGUMENT", 400)
		}
	}
	if v, ok := body["scanRepairAcknowledged"]; ok {
		if _, valid := v.(bool); !valid {
			return newJobError("scanRepairAcknowledged 必须是布尔值", "INVALID_ARGUMENT", 400)
		}
	}
	if v, ok := body["sampleMiB"]; ok && v != nil {
		n, ok := toInt64(v)
		if !ok || !speedSizes[n] {
			return newJobError("sampleMiB 必须是 256、1024 或 4096", "INVALID_ARGUMENT", 400)
		}
	}
	if v, ok := body["speedRuns"]; ok && v != nil {
		n, ok := toInt64(v)
		if !ok || !speedRuns[n] {
			return newJobError("speedRuns 必须是 1、3 或 5", "INVALID_ARGUMENT", 400)
		}
	}
	if v, ok := body["speedPosition"]; ok && v != nil {
		p, _ := v.(string)
		if !speedPositions[p] {
			return newJobError("speedPosition 必须是 all、start、middle 或 end", "INVALID_ARGUMENT", 400)
		}
	}
	if v, ok := body["speedOperation"]; ok && v != nil {
		p, _ := v.(string)
		if !speedOperations[p] {
			return newJobError("speedOperation 必须是 read、write 或 mixed", "INVALID_ARGUMENT", 400)
		}
	}
	if v, ok := body["speedPattern"]; ok && v != nil {
		p, _ := v.(string)
		if !speedPatterns[p] {
			return newJobError("speedPattern 必须是 sequential 或 random", "INVALID_ARGUMENT", 400)
		}
	}
	if v, ok := body["speedBlockSize"]; ok && v != nil {
		n, ok := toInt64(v)
		if !ok || !speedBlockSizes[n] {
			return newJobError("speedBlockSize 必须是 4096、65536、1048576 或 4194304", "INVALID_ARGUMENT", 400)
		}
	}
	if v, ok := body["speedQueueDepth"]; ok && v != nil {
		n, ok := toInt64(v)
		if !ok || !speedQueueDepths[n] {
			return newJobError("speedQueueDepth 必须是 1、8、16、32、64 或 128", "INVALID_ARGUMENT", 400)
		}
	}
	if v, ok := body["speedThreads"]; ok && v != nil {
		n, ok := toInt64(v)
		if !ok || !speedThreads[n] {
			return newJobError("speedThreads 必须是 1、2、4 或 8", "INVALID_ARGUMENT", 400)
		}
	}
	if v, ok := body["speedIOEngine"]; ok && v != nil {
		engine, _ := v.(string)
		if !speedIOEngines[engine] {
			return newJobError("speedIOEngine 必须是 io_uring 或 psync", "INVALID_ARGUMENT", 400)
		}
	}
	if v, ok := body["speedProfileRow"]; ok && v != nil {
		row, _ := v.(string)
		if !speedProfileRows[row] {
			return newJobError("speedProfileRow 无效", "INVALID_ARGUMENT", 400)
		}
	}
	if v, ok := body["speedTarget"]; ok && v != nil {
		target, _ := v.(string)
		if !speedTargetModes[target] {
			return newJobError("speedTarget 必须是 filesystem 或 raw", "INVALID_ARGUMENT", 400)
		}
	}
	for _, field := range []string{"speedDirectIO", "speedTrim", "speedProfile", "rawWriteAcknowledged"} {
		if v, ok := body[field]; ok && v != nil {
			if _, valid := v.(bool); !valid {
				return newJobError(field+" 必须是布尔值", "INVALID_ARGUMENT", 400)
			}
		}
	}
	if v, ok := body["expectedDisk"]; ok && v != nil {
		ed, ok := v.(map[string]interface{})
		if !ok {
			return newJobError("expectedDisk 必须是 JSON 对象", "INVALID_ARGUMENT", 400)
		}
		for _, field := range []string{"path", "serial", "model"} {
			if fv, ok := ed[field]; ok && fv != nil {
				if _, isStr := fv.(string); !isStr {
					return newJobError(fmt.Sprintf("expectedDisk.%s 必须是字符串", field), "INVALID_ARGUMENT", 400)
				}
			}
		}
		if sv, ok := ed["size"]; ok && sv != nil {
			n, ok := toInt64(sv)
			if !ok || n <= 0 {
				return newJobError("expectedDisk.size 必须是正整数", "INVALID_ARGUMENT", 400)
			}
		}
	}
	if v, ok := body["expectedStorageSpace"]; ok && v != nil {
		space, ok := v.(map[string]interface{})
		if !ok {
			return newJobError("expectedStorageSpace 必须是 JSON 对象", "INVALID_ARGUMENT", 400)
		}
		for _, field := range []string{"path", "source", "fileSystem", "layout"} {
			if value, exists := space[field]; exists && value != nil {
				if _, isString := value.(string); !isString {
					return newJobError(fmt.Sprintf("expectedStorageSpace.%s 必须是字符串", field), "INVALID_ARGUMENT", 400)
				}
			}
		}
		if value, exists := space["members"]; exists && value != nil {
			members, isArray := value.([]interface{})
			if !isArray {
				return newJobError("expectedStorageSpace.members 必须是字符串数组", "INVALID_ARGUMENT", 400)
			}
			for _, member := range members {
				if _, isString := member.(string); !isString {
					return newJobError("expectedStorageSpace.members 必须是字符串数组", "INVALID_ARGUMENT", 400)
				}
			}
		}
		if value, exists := space["readOnly"]; exists && value != nil {
			if _, isBool := value.(bool); !isBool {
				return newJobError("expectedStorageSpace.readOnly 必须是布尔值", "INVALID_ARGUMENT", 400)
			}
		}
		if value, exists := space["size"]; exists && value != nil {
			n, valid := toInt64(value)
			if !valid || n <= 0 {
				return newJobError("expectedStorageSpace.size 必须是正整数", "INVALID_ARGUMENT", 400)
			}
		}
	}
	return nil
}

func (a *apiServer) handleApi(w http.ResponseWriter, r *http.Request, pathname string, searchParams url.Values) {
	if !a.isAdmin(r) {
		a.apiError(w, newJobError("仅 NAS 管理员可以使用磁盘检测", "ADMIN_REQUIRED", 403))
		return
	}

	allowed, ok := allowedApiMethods(pathname)
	if ok && !containsString(allowed, r.Method) {
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		a.apiError(w, newJobError("不支持的请求方法", "METHOD_NOT_ALLOWED", 405))
		return
	}
	if !ok {
		a.apiError(w, newJobError("接口不存在", "API_NOT_FOUND", 404))
		return
	}

	switch {
	case pathname == "/api/disks/smart":
		devices := searchParams["device"]
		if len(devices) != 1 || !safeDevicePattern.MatchString(devices[0]) {
			a.apiError(w, newJobError("请指定一个有效的物理磁盘路径", "INVALID_ARGUMENT", 400))
			return
		}
		disks := a.mockDisks
		if !a.mockDev {
			var err error
			disks, err = a.listDisksFn(a.findExecutableFn)
			if err != nil {
				a.apiError(w, newJobError("无法读取当前磁盘列表", "DISK_LIST_FAILED", 500))
				return
			}
		}
		for _, disk := range disks {
			if disk.Path != devices[0] {
				continue
			}
			var report SmartReport
			var err error
			if a.mockDev {
				report = mockSmartReport(disk)
			} else {
				report, err = readSmartReport(disk, a.findExecutableFn, a.smartExecutorFn)
			}
			if err != nil {
				a.apiError(w, mapJobError(err))
				return
			}
			a.json(w, 200, map[string]interface{}{"smart": report})
			return
		}
		a.apiError(w, newJobError("所选物理磁盘已断开或不存在", "DISK_NOT_FOUND", 404))
		return
	case jobScanIssuesRe.MatchString(pathname):
		store, ok := a.jobs.(interface {
			scanIssues(string, int64, int) (*ScanIssuePage, error)
		})
		if !ok {
			a.apiError(w, newJobError("异常明细暂不可用", "SCAN_ISSUES_UNAVAILABLE", 409))
			return
		}
		parse := func(key string, fallback int64) (int64, error) {
			values := searchParams[key]
			if len(values) == 0 {
				return fallback, nil
			}
			if len(values) != 1 || values[0] == "" {
				return 0, newJobError("分页参数无效", "INVALID_ARGUMENT", 400)
			}
			value, err := strconv.ParseInt(values[0], 10, 64)
			if err != nil || value < 0 {
				return 0, newJobError("分页参数必须是非负整数", "INVALID_ARGUMENT", 400)
			}
			return value, nil
		}
		offset, err := parse("offset", 0)
		if err != nil {
			a.apiError(w, mapJobError(err))
			return
		}
		limit, err := parse("limit", 50)
		if err != nil || limit < 1 || limit > 200 {
			a.apiError(w, newJobError("limit 必须为 1–200", "INVALID_ARGUMENT", 400))
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(pathname, "/api/jobs/"), "/scan-issues")
		page, err := store.scanIssues(id, offset, int(limit))
		if err != nil {
			a.apiError(w, mapJobError(err))
			return
		}
		a.json(w, 200, map[string]interface{}{"issues": page})
		return
	case jobScanReportRe.MatchString(pathname):
		store, ok := a.jobs.(interface {
			prepareScanReport(string) (func(io.Writer) error, error)
		})
		if !ok {
			a.apiError(w, newJobError("扫描报告暂不可用", "SCAN_ISSUES_UNAVAILABLE", 409))
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(pathname, "/api/jobs/"), "/scan-report")
		stream, err := store.prepareScanReport(id)
		if err != nil {
			a.apiError(w, mapJobError(err))
			return
		}
		a.applyHeaders(w)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="diskbench-scan-`+id+`.json"`)
		if err := stream(w); err != nil {
			a.logger.Errorf("[diskbench] 导出扫描报告失败 (%v)", err)
		}
		return
	case pathname == "/api/health":
		capabilities := map[string]interface{}{
			"scan":   a.mockDev || a.findExecutableFn("lsblk") != "",
			"dd":     true,
			"fio":    a.mockDev || a.findExecutableFn("fio") != "",
			"fstrim": a.mockDev || a.findExecutableFn("fstrim") != "",
			"smart":  a.mockDev || a.findExecutableFn("smartctl") != "",
			"mock":   a.mockDev,
		}
		username := r.Header.Get("X-Trim-Username")
		if a.localDev {
			username = "local-dev"
		}
		a.json(w, 200, map[string]interface{}{
			"ok":           capabilities["scan"],
			"capabilities": capabilities,
			"testPlan": map[string]interface{}{
				"status":    "provisional",
				"scan":      "read-only",
				"speed":     "temporary-file",
				"writeTest": "disabled",
			},
			"user": map[string]interface{}{"username": username},
		})
		return

	case pathname == "/api/disks":
		var disks []Disk
		var spaces []StorageSpace
		if a.mockDev {
			disks = a.mockDisks
			spaces = defaultMockSpaces(disks)
		} else {
			var err error
			disks, err = a.listDisksFn(a.findExecutableFn)
			if err != nil {
				a.logger.Errorf("[diskbench] list disks failed: %v", err)
				a.apiError(w, newJobError("服务器内部错误", "INTERNAL_ERROR", 500))
				return
			}
			spaces, err = a.listSpacesFn(disks, a.findExecutableFn)
			if err != nil {
				a.logger.Errorf("[diskbench] list storage spaces failed: %v", err)
				a.apiError(w, newJobError("服务器内部错误", "INTERNAL_ERROR", 500))
				return
			}
		}
		a.json(w, 200, map[string]interface{}{"disks": disks, "storageSpaces": spaces})
		return

	case pathname == "/api/jobs" && r.Method == "GET":
		summaries := searchParams["summary"]
		if len(summaries) > 1 || (len(summaries) == 1 && summaries[0] != "0" && summaries[0] != "1") {
			a.apiError(w, newJobError("summary 必须是 0 或 1", "INVALID_ARGUMENT", 400))
			return
		}
		summary := len(summaries) == 1 && summaries[0] == "1"
		activeValues := searchParams["active"]
		if len(activeValues) > 1 || (len(activeValues) == 1 && activeValues[0] != "0" && activeValues[0] != "1") {
			a.apiError(w, newJobError("active 必须是 0 或 1", "INVALID_ARGUMENT", 400))
			return
		}
		if len(activeValues) == 1 && activeValues[0] == "1" {
			includeIDs := searchParams["include"]
			for _, id := range includeIDs {
				if !jobDetailRe.MatchString("/api/jobs/" + id) {
					a.apiError(w, newJobError("include 必须是有效的任务 ID", "INVALID_ARGUMENT", 400))
					return
				}
			}
			a.json(w, 200, map[string]interface{}{"jobs": a.jobs.listCurrent(summary, includeIDs)})
			return
		}
		a.json(w, 200, map[string]interface{}{"jobs": a.jobs.list(summary)})
		return

	case jobScanProgressRe.MatchString(pathname):
		afterValues := searchParams["after"]
		if len(afterValues) > 1 {
			a.apiError(w, newJobError("after 必须是单个整数", "INVALID_ARGUMENT", 400))
			return
		}
		after := 0
		if len(afterValues) == 1 {
			parsed, err := strconv.Atoi(afterValues[0])
			if err != nil || parsed < 0 || parsed > quickScanCellCount {
				a.apiError(w, newJobError("after 必须是有效的块图位置", "INVALID_ARGUMENT", 400))
				return
			}
			after = parsed
		}
		id := strings.TrimSuffix(strings.TrimPrefix(pathname, "/api/jobs/"), "/scan-progress")
		progress, err := a.jobs.scanProgress(id, after)
		if err != nil {
			a.apiError(w, mapJobError(err))
			return
		}
		a.json(w, 200, map[string]interface{}{"progress": progress})
		return
	}

	if jobDetailRe.MatchString(pathname) {
		id := strings.TrimPrefix(pathname, "/api/jobs/")
		if r.Method == "DELETE" {
			if r.Header.Get("X-Requested-With") != "DiskBench" {
				a.apiError(w, newJobError("请求来源校验失败", "REQUEST_SOURCE_INVALID", 403))
				return
			}
			if err := a.jobs.remove(id); err != nil {
				a.apiError(w, mapJobError(err))
				return
			}
			a.json(w, 200, map[string]interface{}{"deletedId": id})
			return
		}
		job := a.jobs.get(id)
		if job == nil {
			a.apiError(w, newJobError("任务不存在", "JOB_NOT_FOUND", 404))
			return
		}
		a.json(w, 200, map[string]interface{}{"job": job})
		return
	}

	if pathname == "/api/jobs" && r.Method == "POST" {
		if r.Header.Get("X-Requested-With") != "DiskBench" {
			a.apiError(w, newJobError("请求来源校验失败", "REQUEST_SOURCE_INVALID", 403))
			return
		}
		body, err := readJson(w, r)
		if err != nil {
			a.apiError(w, mapJobError(err))
			return
		}
		if err := validateCreateJobBody(body); err != nil {
			a.apiError(w, mapJobError(err))
			return
		}
		job, err := a.jobs.create(body["type"].(string), body["device"].(string), body)
		if err != nil {
			a.apiError(w, mapJobError(err))
			return
		}
		a.json(w, 202, map[string]interface{}{"job": job})
		return
	}

	if jobCancelRe.MatchString(pathname) {
		if r.Header.Get("X-Requested-With") != "DiskBench" {
			a.apiError(w, newJobError("请求来源校验失败", "REQUEST_SOURCE_INVALID", 403))
			return
		}
		if _, err := readJson(w, r); err != nil {
			a.apiError(w, mapJobError(err))
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(pathname, "/api/jobs/"), "/cancel")
		job, err := a.jobs.cancel(id)
		if err != nil {
			a.apiError(w, mapJobError(err))
			return
		}
		a.json(w, 200, map[string]interface{}{"job": job})
		return
	}

	a.apiError(w, newJobError("接口不存在", "API_NOT_FOUND", 404))
}

func containsString(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

func (a *apiServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			a.logger.Errorf("[diskbench] request panic: %v", rec)
			a.apiError(w, newJobError("服务器内部错误", "INTERNAL_ERROR", 500))
		}
	}()
	pathname := relativePath(r.URL.Path, a.prefix)
	if strings.HasPrefix(pathname, "/api/") {
		a.handleApi(w, r, pathname, r.URL.Query())
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		a.json(w, 405, map[string]interface{}{"error": "不支持的请求方法"})
		return
	}
	a.serveStatic(w, r, pathname)
}

func (a *apiServer) serveStatic(w http.ResponseWriter, r *http.Request, pathname string) {
	requested := pathname
	if requested == "/" {
		requested = "index.html"
	} else {
		requested = strings.TrimPrefix(requested, "/")
	}
	resolvedPublic := filepath.Clean(a.publicDir)
	target := filepath.Join(resolvedPublic, filepath.FromSlash(requested))
	if !strings.HasPrefix(target, resolvedPublic+string(os.PathSeparator)) && target != resolvedPublic {
		a.json(w, 400, map[string]interface{}{"error": "非法路径"})
		return
	}
	info, err := os.Stat(target)
	if err != nil || info.IsDir() {
		target = filepath.Join(resolvedPublic, "index.html")
	}
	if _, err := os.Stat(target); err != nil {
		a.json(w, 404, map[string]interface{}{"error": "页面资源不存在"})
		return
	}
	extension := strings.ToLower(filepath.Ext(target))
	types := map[string]string{
		".html":  "text/html; charset=utf-8",
		".css":   "text/css; charset=utf-8",
		".js":    "application/javascript; charset=utf-8",
		".png":   "image/png",
		".svg":   "image/svg+xml",
		".woff2": "font/woff2",
	}
	contentType, ok := types[extension]
	if !ok {
		contentType = "application/octet-stream"
	}
	a.applyHeaders(w)
	if extension != ".html" {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	w.Header().Set("Content-Type", contentType)
	http.ServeFile(w, r, target)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func resolveRuntimePath(value, fallback string) string {
	if value == "" {
		return fallback
	}
	if filepath.IsAbs(value) {
		return value
	}
	exe, err := os.Executable()
	if err != nil {
		return value
	}
	return filepath.Join(filepath.Dir(exe), value)
}
