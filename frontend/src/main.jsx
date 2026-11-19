import React, { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import { createRoot } from "react-dom/client";
import {
  AlertTriangle,
  ArrowLeft,
  ChevronDown,
  CircleStop,
  Gauge,
  HardDrive,
  History,
  Info,
  LoaderCircle,
  Moon,
  Play,
  RefreshCw,
  ScanLine,
  ScanSearch,
  Server,
  Settings2,
  ShieldCheck,
  Sun,
  Trash2,
  X
} from "lucide-react";
import {
  activeJobForTarget,
  buildSpeedTargets,
  chooseShownJob,
  detailRetryDelay,
  estimateRemainingMilliseconds,
  findCurrentDiskForJob,
  findLatestScanJob,
  findResumableScan,
  formatEstimatedDuration,
  isCompleteScanResult,
  isCompleteSpeedResult,
  jobBelongsToDisk,
  jobBelongsToSpeedTarget,
  mergeJobLists,
  mergeScanProgress,
  rawSpeedDiskBlockedReason,
  scanRepairDiskBlockedReason,
  recordedRepairCandidates,
  isCompleteRecordedRepairResult,
  safeScanMap,
  scanOutcomeSummary,
  scanIssueDisplayCount,
  scanAbnormalCellCount,
	 scanBadBlockSummary,
	 scanBadBlockKind,
	 scanSlowBadBlock,
  scanRangeOutcomeLabel,
  mergeTimeoutSkipRanges,
  scanRepairStages,
  scanRepairSpeedNote,
  scanRangeLocation,
  scanCellLocationSummary,
  validScanIssuePage,
  scanStatusGroup,
  speedBenchmarkCells,
  speedHistorySummary,
  jobDiskModel,
  isCrystalSpeedPreset,
  CRYSTAL_SPEED_PRESET,
  CRYSTAL_SPEED_RUNS,
  CRYSTAL_SPEED_SIZES,
  storageSpaceLabel,
  storageSpacesForDisk,
  terminalScanPrefix,
  taskStartBlockedReason
} from "./job-model.js";
import { createSmartReportCache, smartReportKey } from "./smart-report-cache.js";
import { ImmediateTooltip, ImmediateTooltipProvider } from "./immediate-tooltip.jsx";
import { createThemeSync } from "./theme-sync.js";
import { Select } from "./select.jsx";
import "./styles.css";

const API = `${import.meta.env.BASE_URL.replace(/\/$/, "")}/api`;
const ACTIVE = new Set(["queued", "running", "cancelling"]);
const SCAN_CELL_COUNT = 4096;
const QUICK_SCAN_CELL_COUNT = 4096;
const QUICK_SAMPLE_BYTES = 1024 * 1024;
const LEGACY_QUICK_SAMPLE_BYTES = 256 * 1024;
const ACTIVE_SCAN_FALLBACK_POLL_MS = 1_000;

function currentJobListPath(jobs) {
  const query = new URLSearchParams({ summary: "1", active: "1" });
  for (const job of jobs) query.append("include", job.id);
  return `/jobs?${query}`;
}

function isFnOsStorageSpacePath(value) {
  return typeof value === "string" && /^\/vol[0-9]+$/.test(value);
}

function isStorageTargetPath(value) {
  if (typeof value !== "string" || value === "/" || !value.startsWith("/") || /[\0\r\n]/.test(value)) return false;
  if (value.split("/").some((segment) => segment === "." || segment === "..")) return false;
  return !["/dev", "/proc", "/sys"].some((blocked) => value === blocked || value.startsWith(`${blocked}/`));
}

function isRecord(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

async function api(path, options = {}) {
  const { signal: upstreamSignal, timeoutMs, ...fetchOptions } = options;
  const method = String(fetchOptions.method || "GET").toUpperCase();
  const requestTimeout = Number.isFinite(timeoutMs) && timeoutMs > 0
    ? timeoutMs
    : (method === "GET" ? 15_000 : 30_000);
  const controller = new AbortController();
  let timedOut = false;
  const abortFromUpstream = () => controller.abort(upstreamSignal?.reason);
  if (upstreamSignal?.aborted) abortFromUpstream();
  else upstreamSignal?.addEventListener("abort", abortFromUpstream, { once: true });
  const timeout = window.setTimeout(() => {
    timedOut = true;
    controller.abort();
  }, requestTimeout);

  let response;
  try {
    response = await fetch(`${API}${path}`, {
      ...fetchOptions,
      signal: controller.signal,
      headers: {
        ...(fetchOptions.body ? { "Content-Type": "application/json", "X-Requested-With": "DiskBench" } : {}),
        ...fetchOptions.headers
      }
    });
  } catch (error) {
    if (timedOut) {
      const timeoutError = new Error("请求超时，请检查检测服务后重试");
      timeoutError.code = "REQUEST_TIMEOUT";
      timeoutError.status = 408;
      throw timeoutError;
    }
    if (!upstreamSignal?.aborted && error instanceof TypeError) {
      const networkError = new Error("无法连接检测服务");
      networkError.code = "NETWORK_ERROR";
      throw networkError;
    }
    throw error;
  } finally {
    window.clearTimeout(timeout);
    upstreamSignal?.removeEventListener("abort", abortFromUpstream);
  }
  let data;
  try {
    data = JSON.parse(await response.text());
  } catch {
    if (!response.ok) throw new Error(`请求失败（${response.status}）`);
    throw new Error("服务返回了无法解析的 JSON 数据");
  }
  if (!response.ok) {
    const message = isRecord(data) && typeof data.error === "string" && data.error.trim()
      ? data.error
      : `请求失败（${response.status}）`;
    const error = new Error(message);
    error.status = response.status;
    if (isRecord(data) && typeof data.code === "string" && data.code.trim()) error.code = data.code.trim();
    throw error;
  }
  if (!isRecord(data)) throw new Error("服务返回的数据格式不正确");
  return data;
}

function readStorageInventory(data) {
  if (!Array.isArray(data.disks)
    || data.disks.some((disk) => !isRecord(disk) || typeof disk.path !== "string" || !disk.path.trim())) {
    throw new Error("磁盘列表响应格式不正确");
  }
  if (!Array.isArray(data.storageSpaces)
    || data.storageSpaces.some((space) => !isRecord(space)
      || typeof space.path !== "string"
      || !isStorageTargetPath(space.path)
      || !Number.isSafeInteger(space.size)
      || space.size <= 0
      || !Array.isArray(space.members))) {
    throw new Error("存储空间列表响应格式不正确");
  }
  return { disks: data.disks, storageSpaces: data.storageSpaces, storageWarning: typeof data.storageWarning === "string" ? data.storageWarning : "" };
}

function isJob(value) {
  return isRecord(value) && typeof value.id === "string" && Boolean(value.id.trim());
}

function readJobList(data) {
  if (!Array.isArray(data.jobs) || data.jobs.some((job) => !isJob(job))) {
    throw new Error("任务列表响应格式不正确");
  }
  return data.jobs;
}

function readHealth(data) {
  if (typeof data.ok !== "boolean"
    || (data.capabilities !== undefined && !isRecord(data.capabilities))
    || (data.testPlan !== undefined && (!isRecord(data.testPlan) || typeof data.testPlan.status !== "string"))) {
    throw new Error("检测服务响应格式不正确");
  }
  return data;
}

function readJob(data, action) {
  if (!isJob(data.job)) throw new Error(`${action}响应格式不正确`);
  return data.job;
}

function readScanProgress(data) {
  const progress = data.progress;
  if (!isRecord(progress)
    || typeof progress.id !== "string"
    || !Number.isSafeInteger(progress.next)
    || !Array.isArray(progress.cells)) throw new Error("块图进度响应格式不正确");
  return progress;
}

function errorMessage(error) {
  if (!(error instanceof Error) || !error.message) return "请求失败";
  return typeof error.code === "string" && error.code ? `${error.message}（${error.code}）` : error.message;
}

function snapshotDisk(disk) {
  if (!isRecord(disk)
    || typeof disk.path !== "string"
    || !disk.path.trim()
    || !Number.isSafeInteger(disk.size)
    || disk.size <= 0) {
    return null;
  }
  return {
    path: disk.path,
    serial: typeof disk.serial === "string" ? disk.serial : "",
    model: typeof disk.model === "string" ? disk.model : "",
    size: disk.size,
    logicalSectorSize: Number.isSafeInteger(disk.logicalSectorSize) && disk.logicalSectorSize > 0 ? disk.logicalSectorSize : 512,
    physicalSectorSize: Number.isSafeInteger(disk.physicalSectorSize) && disk.physicalSectorSize > 0 ? disk.physicalSectorSize : (disk.logicalSectorSize || 512),
    rotational: disk.rotational === true,
    isSystem: disk.isSystem === true
  };
}

function snapshotStorageSpace(space) {
  if (!isRecord(space)
    || typeof space.path !== "string"
    || !isStorageTargetPath(space.path)
    || !Number.isSafeInteger(space.size)
    || space.size <= 0
    || !Array.isArray(space.members)) {
    return null;
  }
  return {
    path: space.path,
    source: typeof space.source === "string" ? space.source : "",
    fileSystem: typeof space.fileSystem === "string" ? space.fileSystem : "",
    layout: typeof space.layout === "string" ? space.layout : "single",
    members: space.members.filter((member) => typeof member === "string"),
    size: space.size,
    freeBytes: Number.isSafeInteger(space.freeBytes) && space.freeBytes >= 0 ? space.freeBytes : 0,
    readOnly: space.readOnly === true,
    isSystem: space.isSystem === true
  };
}

function storageLayoutLabel(space) {
  const layout = String(space?.layout || "single").toLowerCase();
  if (layout === "single") return "单盘";
  if (layout === "mirror") return "镜像";
  return layout === "raid" ? "阵列" : layout.toUpperCase();
}

function formatBytes(value) {
  if (!Number.isFinite(value) || value <= 0) return "-";
  const units = ["B", "KB", "MB", "GB", "TB", "PB"];
  const index = Math.min(Math.floor(Math.log(value) / Math.log(1000)), units.length - 1);
  return `${(value / 1000 ** index).toFixed(index < 3 ? 0 : 2)} ${units[index]}`;
}

function formatDiskOffset(value) {
  if (!Number.isFinite(value) || value < 0) return "-";
  if (value === 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB", "PB"];
  const index = Math.min(Math.floor(Math.log(value) / Math.log(1000)), units.length - 1);
  const digits = index >= 4 ? 4 : (index >= 3 ? 2 : 0);
  return `${(value / 1000 ** index).toFixed(digits)} ${units[index]}`;
}

function formatBinaryBytes(value) {
  if (!Number.isFinite(value) || value <= 0) return "-";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  const index = Math.min(Math.floor(Math.log(value) / Math.log(1024)), units.length - 1);
  const amount = value / 1024 ** index;
  return `${amount.toFixed(amount >= 10 || Number.isInteger(amount) ? 0 : 1)} ${units[index]}`;
}

function formatInteger(value) {
  if (!Number.isFinite(value)) return "-";
  return new Intl.NumberFormat("en-US", { maximumFractionDigits: 0 }).format(Math.floor(value));
}

function formatMilliseconds(value, precise = false) {
  if (!Number.isFinite(value)) return "-";
  if (precise && value < 1) return `${value.toFixed(2)} ms`;
  return `${value < 10 ? value.toFixed(1) : Math.round(value)} ms`;
}

function formatSeconds(value) {
  if (!Number.isFinite(value) || value < 0) return "-";
  return `${value < 10 ? value.toFixed(2) : value.toFixed(1)} s`;
}

function formatTime(value) {
  if (!value) return "-";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "-";
  return new Intl.DateTimeFormat("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", second: "2-digit" }).format(date);
}

function formatPowerOnTime(value) {
  if (!Number.isFinite(value) || value < 0) return "-";
  const hours = Math.floor(value);
  const days = Math.floor(hours / 24);
  return days > 0 ? `${formatInteger(days)} 天 ${hours % 24} 小时` : `${hours} 小时`;
}

function smartStatusMeta(disk) {
  if (disk.smartStatus === "standby") return ["休眠中", "neutral"];
  if (!disk.smartAvailable || disk.smartStatus === "unavailable") return ["不可用", "neutral"];
  if (disk.smartStatus === "passed") return ["正常", "success"];
  if (disk.smartStatus === "failed") return ["警告", "danger"];
  return ["未知", "neutral"];
}

function criticalAttributeRows(disk) {
  if (!isRecord(disk)) return [];
  const value = (candidate) => Number.isFinite(candidate) ? candidate : null;
  if (disk.transport?.toLowerCase() === "nvme" || disk.nvmeMediaErrors != null || disk.criticalWarning != null) {
    return [
      ["介质与数据错误", value(disk.nvmeMediaErrors), "NVMe 介质与数据完整性错误累计次数，不等于本次扫描确认的坏扇区数量"],
      ["严重告警标志", value(disk.criticalWarning), "NVMe Critical Warning 位掩码；0 表示设备未报告此类告警，缺失时不作正常判断"]
    ];
  }
  return [
    ["重映射扇区", value(disk.reallocatedSectors), "ATA 属性 5：已被备用扇区替换的坏扇区数"],
    ["待重映射扇区", value(disk.pendingSectors), "ATA 属性 197：读不稳定、等待写入验证后重映射的扇区"],
    ["不可纠正扇区", value(disk.uncorrectableSectors), "ATA 属性 198：离线扫描发现的不可纠正扇区"],
    ["报告不可纠正", value(disk.reportedUncorrectable), "ATA 属性 187：ECC 无法恢复的读取错误次数"]
  ];
}

function statusMeta(status) {
  return {
    idle: ["待扫描", "neutral"],
    queued: ["排队中", "neutral"],
    running: ["运行中", "running"],
    cancelling: ["停止中", "warning"],
    completed: ["已完成", "success"],
    failed: ["失败", "danger"],
    cancelled: ["已取消", "neutral"],
    interrupted: ["已中断", "warning"]
  }[status] || [status, "neutral"];
}

function StatusBadge({ status }) {
  const [label, tone] = statusMeta(status);
  return <span className={`badge badge-${tone}`}>{status === "running" && <span className="pulse" />} {label}</span>;
}

function createPendingMap(diskSize, count = SCAN_CELL_COUNT) {
  return Array.from({ length: count }, (_, index) => ({
    index,
    start: Math.floor((diskSize * index) / count),
    end: Math.floor((diskSize * (index + 1)) / count),
    status: "pending",
    maxMs: null,
    avgMs: null,
    bytesRead: 0,
    errorCount: 0,
    sampleStart: null,
    sampleEnd: null
  }));
}

async function fetchSmartReport(disk) {
  const data = await api(`/disks/smart?${new URLSearchParams({ device: disk.path })}`, { timeoutMs: 35_000 });
  const report = data.smart;
  if (!isRecord(report) || report.device !== disk.path || typeof report.output !== "string" || !report.output.trim()) {
    throw new Error("SMART 信息响应格式不正确");
  }
  return report;
}

function DiskDetails({ disk, showHealth = true, smartCache }) {
  const [smartOpen, setSmartOpen] = useState(false);
  const smartState = useSyncExternalStore(smartCache.subscribe, () => smartCache.get(disk));
  const diskKey = smartReportKey(disk);
  useEffect(() => {
    if (showHealth) smartCache.load(disk);
  }, [smartCache, diskKey, showHealth]);
  const [smartLabel, smartTone] = smartStatusMeta(disk);
  const criticalRows = criticalAttributeRows(disk);
  const mountpoints = Array.isArray(disk.mountpoints)
    ? disk.mountpoints.filter((mountpoint) => typeof mountpoint === "string" && mountpoint)
    : [];
  return <>
    <dl className="device-specs">
      <div><dt>型号</dt><dd data-tooltip={disk.model || "未知型号"}>{disk.model || "未知型号"}</dd></div>
      <div><dt>容量</dt><dd>{formatBytes(disk.size)}</dd></div>
      <div><dt>接口</dt><dd>{disk.transport?.toUpperCase() || "-"}</dd></div>
      <div><dt>固件版本</dt><dd data-tooltip={disk.firmware || "未提供"}>{disk.firmware || "-"}</dd></div>
      <div><dt>序列号</dt><dd data-tooltip={disk.serial || "未提供"}>{disk.serial || "未提供"}</dd></div>
      <div><dt>挂载位置</dt><dd data-tooltip={mountpoints.join(", ")}>{mountpoints.length ? mountpoints.join(", ") : "未挂载"}</dd></div>
      <div><dt>逻辑扇区</dt><dd>{formatBinaryBytes(disk.logicalSectorSize)}</dd></div>
      <div><dt>物理扇区</dt><dd>{formatBinaryBytes(disk.physicalSectorSize)}</dd></div>
      <div><dt>系统访问</dt><dd>{disk.readOnly ? "只读" : "可读写"}</dd></div>
    </dl>
    {showHealth && <div className="device-health">
      <dl className="health-metrics">
        <div><dt>SMART 健康</dt><dd className={`smart-value ${smartTone}`}>{smartLabel}</dd></div>
        <div><dt>当前温度</dt><dd>{Number.isFinite(disk.temperatureC) ? `${disk.temperatureC} °C` : "-"}</dd></div>
        <div><dt>通电时间</dt><dd>{formatPowerOnTime(disk.powerOnHours)}</dd></div>
        <div><dt>通电次数</dt><dd>{Number.isFinite(disk.powerCycles) ? `${formatInteger(disk.powerCycles)} 次` : "-"}</dd></div>
        <div><dt>累计写入</dt><dd>{formatBytes(disk.bytesWritten)}</dd></div>
        <div><dt>寿命已使用</dt><dd>{Number.isFinite(disk.percentageUsed) ? `${disk.percentageUsed}%` : "-"}</dd></div>
      </dl>
      {criticalRows.length > 0 && <div className="critical-metrics">
        <dl className="health-metrics">
          {criticalRows.map(([label, value, hint]) => <div key={label}>
            <dt data-tooltip={hint}>{label}</dt>
            <dd className={value > 0 ? "value-danger" : ""} data-tooltip={value == null ? "设备未提供此项信息" : hint}>{value == null ? "未提供" : formatInteger(value)}</dd>
          </div>)}
        </dl>
      </div>}
      <div className="disk-state">
        {disk.isSystem && <span className="badge badge-warning"><AlertTriangle size={13} /> 系统盘</span>}
        <span className="badge badge-neutral">{disk.rotational ? "HDD" : "SSD"}</span>
        <button type="button" className={`badge badge-${smartTone} smart-info-button`} aria-haspopup="dialog" data-tooltip="查看原始 SMART 信息" onClick={() => setSmartOpen(true)}><ShieldCheck size={13} aria-hidden="true" /> SMART 信息</button>
      </div>
    </div>}
    {smartOpen && <SmartInfoDialog disk={disk} state={smartState} onRefresh={() => smartCache.load(disk, true)} onClose={() => setSmartOpen(false)} />}
  </>;
}

function DiskSelector({ disks, selected, onSelect, loading, locked, onRefresh, refreshing, availabilityError, storageWarning, mode, speedTargets, selectedSpeedTarget, onSelectSpeedTarget, theme, onToggleTheme }) {
	const speedMode = mode === "speed";
  const [smartCache] = useState(() => createSmartReportCache(fetchSmartReport));
  const [detailsExpanded, setDetailsExpanded] = useState(true);
  const [memberDetailPath, setMemberDetailPath] = useState("");
  useEffect(() => setDetailsExpanded(true), [mode, selected?.path, selectedSpeedTarget?.key]);
  useEffect(() => setMemberDetailPath(""), [selectedSpeedTarget?.key]);
  const selectedStorageSpace = selectedSpeedTarget?.storageSpace;
  const rawDisk = selectedSpeedTarget?.type === "raw" ? selectedSpeedTarget.disk : null;
  const memberDisks = Array.isArray(selectedSpeedTarget?.memberDisks) ? selectedSpeedTarget.memberDisks : [];
  const detailDisk = speedMode
    ? (rawDisk || memberDisks.find((disk) => disk.path === memberDetailPath) || memberDisks.find((disk) => disk.path === selected?.path) || memberDisks[0] || null)
    : selected;
  const [smartLabel] = selected ? smartStatusMeta(selected) : ["SMART 未知"];
	const storageMembers = Array.isArray(selectedStorageSpace?.members) ? selectedStorageSpace.members : [];
	const internalFileTargets = speedTargets.filter((target) => target.type === "filesystem" && /^\/vol\d+$/.test(target.path));
	const systemFileTargets = speedTargets.filter((target) => target.type === "filesystem" && target.storageSpace?.isSystem);
	const externalFileTargets = speedTargets.filter((target) => target.type === "filesystem" && !target.storageSpace?.isSystem && !/^\/vol\d+$/.test(target.path));
	const hasTarget = speedMode ? Boolean(selectedSpeedTarget && !selectedSpeedTarget.disabledReason) : Boolean(selected);
  const targetOptions = speedMode ? [
    { label: "存储空间 · 文件测速", options: internalFileTargets.map(target => ({ value: target.key, label: `${storageSpaceLabel(target.storageSpace)} · ${storageLayoutLabel(target.storageSpace)} · ${String(target.storageSpace.fileSystem || "未知").toUpperCase()} · ${formatBytes(target.storageSpace.size)}` })) },
    { label: "系统盘 · 文件测速", options: systemFileTargets.map(target => ({ value: target.key, label: `${storageSpaceLabel(target.storageSpace)} · ${String(target.storageSpace.fileSystem || "未知").toUpperCase()} · ${formatBytes(target.storageSpace.size)}` })) },
    { label: "外接硬盘 · 文件测速", options: externalFileTargets.map(target => ({ value: target.key, label: `${target.path} · ${String(target.storageSpace.fileSystem || "未知").toUpperCase()} · ${formatBytes(target.storageSpace.size)}${target.storageSpace.readOnly ? " · 只读" : ""}` })) },
    { label: "物理磁盘 · 底层测速", options: speedTargets.filter(target => target.type === "raw").map(target => ({ value: target.key, label: `${target.disk.model} · ${target.disk.name || target.path} · ${target.disabledReason || "未识别文件系统，可底层测速"}`, disabled: Boolean(target.disabledReason) })) }
  ].filter(group => group.options.length) : disks.map(disk => ({ value: disk.path, label: `${disk.model} · ${formatBytes(disk.size)}` }));
  return (
    <section className="device-panel disk-sidebar" aria-label={speedMode ? "测速目标" : "目标磁盘"}>
      <div className="device-heading">
        <div className="panel-heading"><HardDrive size={18} aria-hidden="true" /><h2>磁盘信息</h2></div>
        <div className="device-toolbar-actions">
            <button
              className="icon-button device-refresh"
              type="button"
              onClick={() => {
                onRefresh();
                if (!speedMode && detailDisk) smartCache.load(detailDisk, true);
              }}
              disabled={refreshing}
              aria-label={refreshing ? "正在刷新磁盘和任务" : "刷新磁盘和任务"}
              aria-busy={refreshing ? "true" : undefined}
              data-tooltip={refreshing ? "正在刷新" : "刷新磁盘和任务"}
            >
              <RefreshCw size={16} className={refreshing ? "spin" : ""} aria-hidden="true" />
            </button>
            <button
              className="icon-button theme-toggle"
              type="button"
              onClick={onToggleTheme}
              aria-label={theme === "dark" ? "切换到浅色主题" : "切换到深色主题"}
              aria-pressed={theme === "dark"}
              data-tooltip={theme === "dark" ? "浅色主题" : "深色主题"}
            >
              <span className="theme-light" aria-hidden="true"><Sun size={16} /></span>
              <span className="theme-dark" aria-hidden="true"><Moon size={16} /></span>
            </button>
        </div>
      </div>
      <div className="device-toolbar">
        <label className="disk-select-field disk-select-unlabeled">
          <Select
            value={speedMode ? (selectedSpeedTarget?.key || "") : (selected?.path || "")}
            onValueChange={speedMode ? onSelectSpeedTarget : onSelect}
            disabled={locked || loading || (speedMode ? !speedTargets.some((target) => !target.disabledReason) : disks.length === 0)}
            aria-label={speedMode ? "选择测速目标" : "选择目标磁盘"}
            placeholder={loading ? "正在读取存储信息..." : speedMode ? "没有可测速的目标" : "没有可用磁盘"}
            options={loading ? [] : targetOptions}
          />
        </label>
      </div>
      {speedMode && storageWarning && <div className="inline-alert warning" role="status"><AlertTriangle size={18} aria-hidden="true" /><span>{storageWarning}</span></div>}
		{hasTarget && <div className="compact-disk-summary">
          <strong>{speedMode ? (selectedStorageSpace ? `${String(selectedStorageSpace.fileSystem || "未知").toUpperCase()} · ${formatBytes(selectedStorageSpace.size)}` : formatBytes(rawDisk?.size)) : `${smartLabel} · ${Number.isFinite(selected?.temperatureC) ? `${selected.temperatureC} °C` : formatBytes(selected?.size)}`}</strong>
          <button type="button" className="compact-detail-toggle" aria-expanded={detailsExpanded} aria-controls="disk-detail-content" onClick={() => setDetailsExpanded((current) => !current)}>
            磁盘详情 <ChevronDown size={15} aria-hidden="true" />
          </button>
        </div>}
		{hasTarget && (
        <div id="disk-detail-content" className={`device-meta compact-detail-content${detailsExpanded ? " compact-expanded" : ""}`}>
          {speedMode && memberDisks.length > 1 && <label className="disk-select-field member-disk-select-field">
            <span>查看成员盘</span>
            <Select value={detailDisk?.path || ""} disabled={locked || loading} aria-label="选择查看的成员盘" onValueChange={setMemberDetailPath}
              options={memberDisks.map(disk => ({ value: disk.path, label: `${disk.model || "未知型号"} · ${disk.path}` }))} />
          </label>}
          {detailDisk ? <DiskDetails key={smartReportKey(detailDisk)} disk={detailDisk} showHealth={!speedMode} smartCache={smartCache} /> : <p className="disk-detail-unavailable">未识别到对应的物理磁盘，暂无法显示硬盘详情。</p>}
          {speedMode && selectedStorageSpace && <div className="speed-target-details">
		    <dl className="device-specs">
		      <div><dt>{selectedStorageSpace.isSystem ? "测速目标" : (/^\/vol\d+$/.test(selectedStorageSpace.path) ? "存储空间" : "外接挂载")}</dt><dd>{storageSpaceLabel(selectedStorageSpace)}</dd></div>
              {selectedStorageSpace.isSystem && <div><dt>临时目录</dt><dd>{selectedStorageSpace.path}</dd></div>}
		      <div><dt>阵列结构</dt><dd>{storageLayoutLabel(selectedStorageSpace)}</dd></div>
		      <div><dt>文件系统</dt><dd>{String(selectedStorageSpace.fileSystem || "-").toUpperCase()}</dd></div>
		      <div><dt>总容量</dt><dd>{formatBytes(selectedStorageSpace.size)}</dd></div>
		      <div><dt>可用空间</dt><dd>{formatBytes(selectedStorageSpace.freeBytes)}</dd></div>
		      <div><dt>成员盘</dt><dd data-tooltip={storageMembers.join(" + ")}>{storageMembers.length ? storageMembers.map((member) => member.replace(/^\/dev\//, "")).join(" + ") : "未知"}</dd></div>
		    </dl>
		    <div className="disk-state">
		      <span className="badge badge-neutral">{storageLayoutLabel(selectedStorageSpace)}</span>
		      <span className="badge badge-success"><ShieldCheck size={13} /> {selectedStorageSpace.isSystem ? "系统盘文件测速" : (/^\/vol\d+$/.test(selectedStorageSpace.path) ? "逻辑存储空间" : "外接文件系统")}</span>
		    </div>
          </div>}
          {speedMode && rawDisk && <div className="speed-target-details">
        <dl className="device-specs">
          <div><dt>设备</dt><dd>{rawDisk.path}</dd></div>
          <div><dt>分区</dt><dd>{rawDisk.hasPartitions ? "已存在" : (rawDisk.partitionTable ? String(rawDisk.partitionTable).toUpperCase() : "未识别")}</dd></div>
          <div><dt>文件系统</dt><dd>无</dd></div>
        </dl>
        <div className="disk-state"><span className="badge badge-neutral">物理磁盘</span><span className="badge badge-neutral">底层读写</span></div>
          </div>}
        </div>
      )}
		{!loading && !hasTarget && (
        <div className={`device-empty${availabilityError ? " error" : ""}`} role={availabilityError ? "alert" : "status"}>
          {availabilityError ? <AlertTriangle size={22} aria-hidden="true" /> : <Server size={22} aria-hidden="true" />}
          <div>
			  <strong>{availabilityError ? "无法读取磁盘" : (speedMode ? "没有可测速的目标" : "没有可用磁盘")}</strong>
			  <span>{availabilityError || (speedMode ? "需要存储空间、已挂载外接硬盘，或未识别文件系统且不属于存储空间的磁盘。" : "连接物理磁盘后点击刷新。")}</span>
          </div>
        </div>
      )}
    </section>
  );
}

function ModeSwitcher({ mode, setMode }) {
  return (
    <section className="mode-switcher" aria-label="检测类型">
      <div className="segmented" role="tablist" aria-label="测试类型">
        <button role="tab" aria-selected={mode === "scan"} className={mode === "scan" ? "active" : ""} onClick={() => setMode("scan")}>
          <ScanSearch size={16} /> 坏道扫描
        </button>
        <button role="tab" aria-selected={mode === "speed"} className={mode === "speed" ? "active" : ""} onClick={() => setMode("speed")}>
          <Gauge size={16} /> 磁盘测速
        </button>
      </div>
    </section>
  );
}

function ScanRepairSummary({ job }) {
  if (job?.type !== "scan" || !job.options?.scanRepair) return null;
  const repair = job.result?.stats?.repair || {};
  return (
    <div className="scan-repair-result" role="group" aria-label="慢扇区写回结果">
      <strong>慢扇区写回 · 阈值 {job.options.scanRepairAfterMs || 500} ms</strong>
      {job.result
        ? <p>候选 {repair.candidates || 0} · 写回校验通过 {(repair.verified || 0) + (repair.stillSlow || 0)} · 慢速改善 {repair.improved || 0} · 修复后仍慢 {repair.stillSlow || 0} · 跳过 {repair.skipped || 0} · 失败 {repair.failed || 0} · {ACTIVE.has(job.status) ? "处理中" : "结果未确定"} {repair.pending || 0}</p>
        : <p>正在加载写回统计…</p>}
      <p>写回校验通过表示数据一致；同范围连续复测恢复到门槛以内，才记为慢速改善。首次慢响应和原始数据备份保留。</p>
      {repair.stillSlow > 0 && <p className="value-danger">修复后仍慢：已停止对这些地址反复写入，建议更换硬盘。</p>}
    </div>
  );
}

function TaskSetup({ mode, scanMode, setScanMode, scanExactSectors, setScanExactSectors, scanDeepLocate, setScanDeepLocate, scanSelfTest, setScanSelfTest, scanRepair, setScanRepair, scanRepairAfterMs, setScanRepairAfterMs, speedConfig, setSpeedConfig, health, onStart, onCancel, activeJob, resultJob, submitting, checkingResume, cancelPending, targetAvailable, disk, speedTarget, startBlockedReason }) {
  const [etaClock, setEtaClock] = useState(() => Date.now());
  const scanAvailable = health?.capabilities?.scan === true;
  const speedAvailable = scanAvailable && health?.capabilities?.fio === true;
  const speedReadOnly = speedConfig.speedOperation === "read";
  const speedWillWrite = Boolean(speedTarget) && !speedReadOnly;
	const readOnlyFileTarget = speedTarget?.type === "filesystem" && speedTarget.storageSpace?.readOnly === true;
  const unavailableMessage = !scanAvailable
    ? "系统缺少 lsblk，无法识别可检测的物理磁盘。"
    : "系统缺少 fio，无法执行测速。";
  const repairBlockedReason = scanRepairDiskBlockedReason(disk);
  const repairEnabled = mode === "scan" && (scanRepair || Boolean(activeJob?.options?.scanRepairFrom));
  const repairThresholdValid = Number.isInteger(Number(scanRepairAfterMs)) && Number(scanRepairAfterMs) >= 1 && Number(scanRepairAfterMs) <= 60000;
  const stopping = activeJob?.status === "cancelling" || cancelPending;
  const startLabel = mode === "speed"
    ? "开始测速"
    : (scanMode === "full" ? "开始全盘扫描" : "开始快速扫描");
  const controlsLocked = Boolean(activeJob) || submitting || Boolean(startBlockedReason);
  const trimTargetAllowed = speedTarget?.type === "filesystem" && isFnOsStorageSpacePath(speedTarget.path);
  const trimAvailable = health?.capabilities?.fstrim === true && trimTargetAllowed;
  const trimTitle = speedTarget?.type === "raw"
    ? "底层测速不执行文件系统 TRIM"
    : (!isFnOsStorageSpacePath(speedTarget?.path)
      ? "外置挂载不执行测速前 TRIM"
      : (health?.capabilities?.fstrim === true ? "测速前对所选存储空间执行 TRIM" : "系统未提供 fstrim"));
  const actionLabel = activeJob ? (stopping ? "正在停止" : "停止任务") : (checkingResume ? "正在检查扫描记录" : startLabel);
  const startOptions = mode === "speed"
    ? speedRunOptions(speedConfig)
    : { type: "scan", scanMode, scanExactSectors, scanDeepLocate, scanRepair: repairEnabled, ...(repairEnabled ? { scanRepairAfterMs: Number(scanRepairAfterMs) } : {}), scanSelfTest: scanSelfTest && /^\/dev\/nvme\d+(?:c\d+)?n\d+$/.test(disk?.path || "") && !health?.capabilities?.mock };
  const actionDisabled = activeJob
    ? stopping
    : (Boolean(startBlockedReason) || !targetAvailable || submitting || (repairEnabled && (Boolean(repairBlockedReason) || !repairThresholdValid)) || (readOnlyFileTarget && speedWillWrite) || (mode === "speed" ? !speedAvailable : !scanAvailable));
  const speedFileNote = health?.testPlan?.status === "provisional" && mode === "speed" && speedTarget?.type === "filesystem" && speedWillWrite
    ? `测速会在 ${speedTarget.path} 创建临时测试文件，结束后自动删除。`
    : "";
  const actionTitle = activeJob
    ? (stopping ? "正在停止当前任务" : "停止当前任务")
    : (startBlockedReason || (!targetAvailable
      ? (mode === "speed" ? "没有可测速的存储空间或空盘" : "请先连接并选择磁盘")
      : (submitting
        ? (checkingResume ? "正在检查扫描记录" : "正在创建任务")
        : (readOnlyFileTarget && speedWillWrite ? "只读外接盘只能执行读取测速" : ""))));
  const showEta = Boolean(activeJob && ["scan", "write"].includes(activeJob.type) && activeJob.status === "running");
  const etaText = formatEstimatedDuration(estimateRemainingMilliseconds(activeJob, etaClock));

  useLayoutEffect(() => {
    if (!showEta) return undefined;
    setEtaClock(Date.now());
    const timer = window.setInterval(() => setEtaClock(Date.now()), 5_000);
    return () => window.clearInterval(timer);
  }, [activeJob?.id, showEta]);

  return (
    <section className="task-setup" aria-labelledby="test-heading">
      <div className="section-heading task-heading">
        <div>
          <div className="task-heading-title">
            <Settings2 className="section-icon" size={17} aria-hidden="true" />
            <h2 id="test-heading">检测配置</h2>
          </div>
        </div>
        <div className="task-meta">
          <span className={`meta-chip${repairEnabled || (mode === "speed" && speedWillWrite) ? " danger" : ""}`}>
            {repairEnabled || (mode === "speed" && speedWillWrite) ? <AlertTriangle size={14} aria-hidden="true" /> : <ShieldCheck size={14} aria-hidden="true" />}
            {mode === "speed" ? (speedWillWrite ? "含写入测速" : "只读测速") : (repairEnabled ? "含慢扇区写回" : "检测不写盘")}
          </span>
          <span className="meta-chip">磁盘负载：{mode === "speed" ? "较高" : (scanMode === "full" ? "持续" : "短时")}</span>
        </div>
      </div>

      <div className="task-action-stack">
        {showEta && (
          <div className="task-eta" role="status" aria-live="polite">
            <span>预计剩余时间</span>
            <strong>{etaText}</strong>
          </div>
        )}
        <div className="task-actions">
          <button
            className={`button ${activeJob ? "danger" : "primary"} cta`}
            onClick={() => activeJob ? onCancel(activeJob.id) : onStart(startOptions)}
            disabled={actionDisabled}
            aria-busy={stopping || submitting ? "true" : undefined}
            data-tooltip={[actionTitle, !activeJob && speedFileNote].filter(Boolean).join("\n")}
            aria-describedby={!activeJob && speedFileNote ? "speed-temp-file-note" : undefined}
          >
            {activeJob
              ? (stopping ? <LoaderCircle className="spin" size={18} aria-hidden="true" /> : <CircleStop size={18} aria-hidden="true" />)
              : (checkingResume ? <LoaderCircle className="spin" size={18} aria-hidden="true" /> : (mode === "speed" ? <Gauge size={18} /> : <ScanSearch size={18} />))}
            {actionLabel}
          </button>
          {!activeJob && speedFileNote && <span id="speed-temp-file-note" className="sr-only">{speedFileNote}</span>}
        </div>
        {!activeJob && startBlockedReason && <p className="task-blocked-note" role="status">{startBlockedReason}</p>}
      </div>

      <fieldset className="task-panel" disabled={controlsLocked} aria-label="检测参数">
        {mode === "scan" && (
          <div className="mode-cards" role="radiogroup" aria-label="扫描方式">
            <button type="button" role="radio" aria-checked={scanMode === "quick"} className={`mode-card${scanMode === "quick" ? " selected" : ""}`} onClick={() => { setScanMode("quick"); setScanDeepLocate(false); }}>
              <span className="mode-card-icon" aria-hidden="true"><ScanSearch size={17} strokeWidth={1.8} /></span>
              <span className="mode-card-copy">
                <strong>快速扫描</strong>
              </span>
            </button>
            <button type="button" role="radio" aria-checked={scanMode === "full"} className={`mode-card${scanMode === "full" ? " selected" : ""}`} onClick={() => { setScanMode("full"); setScanDeepLocate(true); }}>
              <span className="mode-card-icon" aria-hidden="true"><ScanLine size={17} strokeWidth={1.8} /></span>
              <span className="mode-card-copy">
                <strong>全盘扫描</strong>
              </span>
            </button>
          </div>
        )}
        {mode === "scan" && (
          <fieldset className="speed-extra-options scan-extra-options">
            <legend className="sr-only">扫描附加选项</legend>
            {/^\/dev\/nvme\d+(?:c\d+)?n\d+$/.test(disk?.path || "") && <label data-tooltip={health?.capabilities?.mock ? "演示模式不执行固件自检" : "先运行设备支持的 NVMe 固件扩展自检，再进行读取扫描；会增加检测时间，不支持时提示停止。取消任务会尝试中止本次自检。"}>
              <input type="checkbox" checked={scanSelfTest && !health?.capabilities?.mock} disabled={health?.capabilities?.mock === true} onChange={(event) => setScanSelfTest(event.target.checked)} />
              <span>NVMe 固件自检（可选）</span>
            </label>}
            <label data-tooltip="发现读取异常后细分到逻辑扇区；达到定位预算时保留待细查范围，继续后面的扫描">
              <input type="checkbox" checked={scanExactSectors} onChange={(event) => setScanExactSectors(event.target.checked)} />
              <span>精确定位错误扇区</span>
            </label>
            <Select className="scan-probe-mode" aria-label="异常定位预算" value={scanDeepLocate ? "deep" : "standard"} onValueChange={value => setScanDeepLocate(value === "deep")} data-tooltip="每个异常范围：标准最多 256 次／15 秒内提交新请求；深入最多 4096 次／2 分钟。已提交的读取仍需等待设备返回。"
              options={[{ value: "standard", label: "标准定位" }, { value: "deep", label: "深入定位" }]} />
            <div className="scan-repair-option">
              <ImmediateTooltip text={repairBlockedReason ? `修复慢扇区不可用：${repairBlockedReason}` : ""} focusable={Boolean(repairBlockedReason)}>
                <label className={repairBlockedReason ? "disabled" : ""}>
                  <input type="checkbox" checked={scanRepair} disabled={Boolean(repairBlockedReason)} aria-describedby={repairBlockedReason ? "scan-repair-blocked-reason" : undefined} onChange={(event) => setScanRepair(event.target.checked)} />
                  <span>修复慢扇区</span>
                </label>
              </ImmediateTooltip>
              {repairEnabled && <label className="scan-repair-threshold">
                <span>超过</span>
                <input id="scan-repair-threshold" type="number" inputMode="numeric" min="1" max="60000" step="1" value={scanRepairAfterMs} onChange={(event) => setScanRepairAfterMs(event.target.value)} aria-label="慢扇区修复阈值（毫秒）" aria-describedby={!repairThresholdValid ? "scan-repair-error" : undefined} aria-invalid={!repairThresholdValid} />
                <span>ms</span>
              </label>}
            </div>
            {repairBlockedReason && <span id="scan-repair-blocked-reason" className="sr-only">修复慢扇区不可用：{repairBlockedReason}</span>}
            {!resultJob?.options?.scanRepairFrom && <ScanRepairSummary job={resultJob} />}
          </fieldset>
        )}
        {repairEnabled && !repairThresholdValid && <p id="scan-repair-error" className="scan-repair-error value-danger" role="alert">请输入 1～60000 的整数。</p>}
        {mode === "speed" && (
          <div className="speed-config-stack">
            <div className="speed-sidebar-config">
              <label><span data-tooltip="正式测试重复执行，取最快一次的平均速度。">测试次数</span><Select aria-label="测试次数" value={speedConfig.speedRuns} onValueChange={value => setSpeedConfig(current => ({ ...current, speedRuns: Number(value) }))} options={Array.from({ length: 9 }, (_, index) => ({ value: index + 1, label: `${index + 1} 次` }))} /></label>
              <label><span data-tooltip="指定测试文件大小或裸盘访问范围，可在范围内重复读写。">测试范围</span><Select aria-label="测试范围" value={speedConfig.sampleMiB} onValueChange={value => setSpeedConfig(current => ({ ...current, sampleMiB: Number(value) }))} options={CRYSTAL_SPEED_SIZES.map(value => ({ value, label: formatBinaryBytes(value * 1048576) }))} /></label>
              <label><span>测试组合</span><Select aria-label="测试组合" value={speedConfig.speedOperation} onValueChange={value => setSpeedConfig(current => ({ ...current, speedOperation: value }))} options={[{ value: "auto", label: "全部" }, { value: "read", label: "读取" }, { value: "write", label: "写入" }, { value: "mixed", label: "混合 50/50" }]} /></label>
              <label><span data-tooltip="设置顺序测试块大小，随机测试固定使用 4 KiB">块大小</span><Select aria-label="块大小" value={speedConfig.speedBlockSize} onValueChange={value => setSpeedConfig(current => ({ ...current, speedBlockSize: Number(value) }))} options={[4096, 65536, 1048576, 4194304].map(value => ({ value, label: formatBinaryBytes(value) }))} /></label>
              <label><span data-tooltip="仅调整高队列项目；按项目使用顺序 Q8、随机 Q32">队列深度</span><Select aria-label="队列深度" value={speedConfig.speedIOEngine === "psync" ? 1 : speedConfig.speedQueueDepth} disabled={speedConfig.speedIOEngine === "psync"} onValueChange={value => setSpeedConfig(current => ({ ...current, speedQueueDepth: Number(value) }))} options={[{ value: 0, label: "按项目" }, ...[1, 8, 16, 32, 64, 128].map(value => ({ value, label: String(value) }))]} /></label>
              <label><span data-tooltip="仅调整高队列项目；Q1T1 基准固定为单任务">并发任务</span><Select aria-label="并发任务" value={speedConfig.speedIOEngine === "psync" ? 1 : speedConfig.speedThreads} disabled={speedConfig.speedIOEngine === "psync"} onValueChange={value => setSpeedConfig(current => ({ ...current, speedThreads: Number(value) }))} options={[1, 2, 4, 8].map(value => ({ value, label: String(value) }))} /></label>
            </div>
            <div className="speed-engine-row">
              <span id="speed-engine-label">I/O 引擎</span>
              <div className="segmented speed-engine-options" role="group" aria-labelledby="speed-engine-label">
                <button type="button" className={speedConfig.speedIOEngine === "io_uring" ? "active" : ""} aria-pressed={speedConfig.speedIOEngine === "io_uring"} onClick={() => setSpeedConfig(current => ({ ...current, speedIOEngine: "io_uring" }))}>io_uring</button>
                <button type="button" data-tooltip="同步 I/O 只执行 Q1 项目" className={speedConfig.speedIOEngine === "psync" ? "active" : ""} aria-pressed={speedConfig.speedIOEngine === "psync"} onClick={() => setSpeedConfig(current => ({ ...current, speedIOEngine: "psync" }))}>psync</button>
              </div>
            </div>
            <fieldset className="speed-extra-options">
              <legend className="sr-only">测速附加选项</legend>
              <label data-tooltip="绕过系统页缓存，测量更接近存储介质的性能"><input type="checkbox" checked={speedConfig.speedDirectIO} onChange={event => setSpeedConfig(current => ({ ...current, speedDirectIO: event.target.checked }))} /><span>Direct I/O</span></label>
              <label className={trimAvailable ? "" : "disabled"} data-tooltip={trimTitle}><input type="checkbox" checked={trimTargetAllowed && speedConfig.speedTrim} disabled={!trimAvailable} onChange={event => setSpeedConfig(current => ({ ...current, speedTrim: event.target.checked }))} /><span>测速前 TRIM</span></label>
            </fieldset>
          </div>
        )}
        {health && ((mode === "scan" && !scanAvailable) || (mode === "speed" && !speedAvailable)) && (
          <div className="inline-alert danger" role="alert"><AlertTriangle size={18} /><span>{unavailableMessage}</span></div>
        )}
        {mode === "speed" && speedTarget?.type === "raw" && (
          <div className="inline-alert warning" role="status"><AlertTriangle size={18} aria-hidden="true" /><span>所选硬盘没有可用文件系统，将自动使用底层测速；写入及混合项目会覆写原有数据，造成数据损坏且无法恢复。</span></div>
        )}
		{mode === "speed" && readOnlyFileTarget && speedWillWrite && (
		  <div className="inline-alert warning" role="status"><AlertTriangle size={18} aria-hidden="true" /><span>此外接文件系统当前为只读；请选择“读取”后再开始测速。</span></div>
		)}
      </fieldset>
    </section>
  );
}

function ScanRepairEvidence({ repair, job }) {
  const threshold = Math.min(job.options?.scanRepairAfterMs ?? 500, job.disk?.rotational ? 800 : 500);
  const note = ["verified", "still_slow"].includes(repair.outcome)
    ? (repair.speedChecks?.length === 2 ? scanRepairSpeedNote(repair, threshold) : "") : repair.detail;
  return <>
    <div className="scan-repair-stages">{scanRepairStages(repair).map((stage) => <span key={stage.label}>
      {stage.label} {stage.text ?? (stage.values ? `${stage.values.map((ms) => formatMilliseconds(ms, true).replace(/ ms$/, "")).join(" / ")} ms` : formatMilliseconds(stage.ms, true))}
    </span>)}</div>
    {note && <small>{note}</small>}
  </>;
}

function scanIssueAddress(range, location) {
  const start = location.startLba ?? range.offset;
  const end = location.endLba ?? range.offset + range.length - 1;
  return start === end ? formatInteger(start) : `${formatInteger(start)} – ${formatInteger(end)}`;
}

function ScanIssueBrowser({ job }) {
  const [offset, setOffset] = useState(0);
  const [revision, setRevision] = useState(0);
  const [page, setPage] = useState(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const limit = 50;
  const logicalSectorSize = job.disk?.logicalSectorSize;
  const sectorBytes = scanRangeLocation(null, logicalSectorSize).sectorBytes;
  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError("");
    api(`/jobs/${job.id}/scan-issues?view=ranges&offset=${offset}&limit=${limit}`, { signal: controller.signal })
      .then((response) => {
        if (!validScanIssuePage(response.issues, job.disk.size, offset)) throw new Error("异常明细响应不完整");
        if (!controller.signal.aborted) setPage(response.issues);
      })
      .catch((err) => { if (!controller.signal.aborted) setError(errorMessage(err)); })
      .finally(() => { if (!controller.signal.aborted) setLoading(false); });
    return () => controller.abort();
  }, [job.id, job.disk.size, offset, revision]);
  const count = scanIssueDisplayCount(job);
  if (!count) return null;
  const current = page?.offset === offset ? page : null;
  const totalPages = Math.max(1, Math.ceil((current?.total ?? count) / limit));
  const currentPage = Math.floor(offset / limit) + 1;
  return (
    <div className="scan-issues-body" aria-busy={loading}>
      <div className="scan-issues-toolbar">
        <div className="scan-issues-summary">
          <strong>{formatInteger(current?.total ?? count)} 条异常记录</strong>
          <span data-tooltip={`${job.disk?.model || "未知硬盘型号"} · ${formatBytes(job.disk?.size)}`}>{job.disk?.model || "未知硬盘型号"} · {formatBytes(job.disk?.size)}</span>
        </div>
        <button type="button" className="icon-button scan-issues-refresh" aria-label="刷新明细" data-tooltip="刷新异常明细" disabled={loading} onClick={() => setRevision(value => value + 1)}><RefreshCw className={loading ? "spin" : ""} size={16} aria-hidden="true" /></button>
      </div>
      {loading && !current && <p className="scan-issues-loading" role="status"><LoaderCircle className="spin" size={16} aria-hidden="true" />正在加载异常明细…</p>}
      {error && <div className="inline-alert warning" role="alert"><AlertTriangle size={16} /><span>{error}</span></div>}
      {current && <>
        {(current.legacy || !current.complete) && <p className="scan-issues-notice" role="status"><AlertTriangle size={14} aria-hidden="true" />{current.legacy ? "旧记录仅保留摘要，异常地址可能不完整。" : "异常记录不完整，请同时查看报告中的保存告警。"}</p>}
        <div className="scan-issue-scroll">
          <ol className="scan-issue-list" aria-label="扫描异常范围及读取结论" start={offset + 1}>
            {current.records.map((range, index) => {
              const location = scanRangeLocation(range, logicalSectorSize);
              const skipped = range.skippedRange;
              const skippedLocation = skipped ? scanRangeLocation(skipped, logicalSectorSize) : null;
              const bad = range.outcome === "unreadable" || range.code === "PERSISTENT_SLOW_READ" || ["failed", "still_slow"].includes(range.repair?.outcome);
              const evidence = skipped ? "读取超时；跳过部分未完成读取检测" : range.detail || (range.code === "EIO" ? "I/O 错误，原因未细分" : range.code === "TIMEOUT" ? "读取超时" : range.code === "TRANSPORT_ERROR" ? "连接或传输错误" : range.code === "MEDIUM_ERROR" ? "设备报告介质错误" : range.code === "SHORT_READ" ? "未完整读取" : range.code);
              return <li className="scan-issue-card" key={offset + index}>
                <div className="scan-issue-heading">
                  <strong className={`scan-issue-verdict${bad ? " value-danger" : ""}`}>{bad ? <AlertTriangle size={14} aria-hidden="true" /> : <Info size={14} aria-hidden="true" />}{scanRangeOutcomeLabel(range)}</strong>
                  <span className="scan-issue-precision">{location.label}</span>
                </div>
                <div className="scan-issue-address"><span>{location.startLba == null ? "字节" : "LBA"}</span><strong>{scanIssueAddress(range, location)}</strong><small>{formatBinaryBytes(range.length)}</small></div>
                {location.precision === "range" && <p className="scan-issue-probe">定位单位 {formatBinaryBytes(range.probeBytes)}</p>}
                <div className="scan-issue-evidence"><span>{range.repair ? "修复" : "原因"}</span><div>{range.repair ? <ScanRepairEvidence repair={range.repair} job={job} /> : evidence}</div></div>
                {skipped && <div className="scan-issue-skipped"><span>跳过未检测</span><strong>{skippedLocation.startLba == null ? "字节" : "LBA"} {scanIssueAddress(skipped, skippedLocation)}</strong><small>{formatBinaryBytes(skipped.length)}</small></div>}
              </li>;
            })}
          </ol>
          {!current.records.length && <p className="scan-record-note">当前没有异常记录。</p>}
          <details className="scan-issues-help">
            <summary>定位说明<ChevronDown size={14} aria-hidden="true" /></summary>
            <p>{sectorBytes ? `逻辑扇区 ${formatInteger(sectorBytes)} B；LBA 从 0 编号，起止地址均包含。` : "逻辑扇区大小未记录，仅显示字节地址。"}</p>
            <p>逐扇区确认表示已定位到单个不可读逻辑扇区；范围确认不代表每个扇区均损坏。待定位和跳过范围不计为已确认坏扇区，慢读及修复记录显示请求范围。</p>
          </details>
        </div>
        {totalPages > 1 && <nav className="scan-issues-pagination" aria-label="异常明细分页">
          <button type="button" className="button secondary" disabled={loading || offset === 0} onClick={() => setOffset(Math.max(0, offset - limit))}>上一页</button>
          <span aria-live="polite">第 {currentPage} / {totalPages} 页</span>
          <button type="button" className="button secondary" disabled={loading || current.next >= current.total} onClick={() => setOffset(current.next)}>下一页</button>
        </nav>}
      </>}
    </div>
  );
}

const SCAN_LEGEND = [
  ["fast", "优秀", "折算耗时 < 5 ms"],
  ["good", "良好", "折算耗时 5–< 20 ms"],
  ["normal", "正常", "折算耗时 20–< 50 ms"],
  ["slow", "一般", "折算耗时 50–< 150 ms"],
  ["slower", "较差", "折算耗时 150–< 500 ms"],
  ["severe", "严重", "折算耗时 ≥ 500 ms"],
  ["error", "损坏", "读取、写入或校验发生错误，与耗时无关"]
];
const SCAN_STATUS_LABELS = Object.fromEntries(SCAN_LEGEND);

function scanStatusLabel(status) {
  return SCAN_STATUS_LABELS[status] || ({ pending: "未扫描", reading: "读取中" }[status] || status);
}

const SCAN_COUNTER_LABELS = {
  reallocated: "重映射扇区", pending: "待处理扇区", uncorrectable: "离线无法校正扇区",
  reported: "已报告无法校正错误（原始值）", nvmeMedia: "NVMe 介质错误", critical: "NVMe 警告位",
  ataErrors: "ATA 日志错误", nvmeLog: "NVMe 日志错误", scsi_read: "SCSI 读取错误",
  scsi_write: "SCSI 写入错误", scsi_verify: "SCSI 校验错误"
};

function ScanReportContext({ job }) {
  const timestamp = job.startedAt || job.createdAt;
  return <div className="scan-report-context">
    <HardDrive size={19} aria-hidden="true" />
    <div>
      <strong>{job.disk?.model || "未知硬盘型号"}</strong>
      <p>{[formatBytes(job.disk?.size), job.device, timestamp && `扫描时间 ${formatTime(timestamp)}`].filter(Boolean).join(" · ")}</p>
    </div>
    {job.status === "idle" ? <span className="badge badge-neutral">未开始</span> : <StatusBadge status={job.status} />}
  </div>;
}

function ScanReportHelp({ title, children }) {
  return <details className="scan-report-help">
    <summary>{title}<ChevronDown size={15} aria-hidden="true" /></summary>
    <div>{children}</div>
  </details>;
}

function ScanEvidenceSnapshot({ label, evidence, mock, pending }) {
  const hints = evidence?.logHints || [];
  const smartState = !evidence ? (pending ? "待采集" : "未记录") : mock ? "未查询"
    : !evidence.available ? "不可用" : ({ passed: "通过", failed: "告警" }[evidence.status] || "状态未知");
  return <section className="scan-evidence-snapshot" aria-label={`${label}健康信息`}>
    <h3>{label}</h3>
    <dl>
      <div><dt>SMART</dt><dd className={evidence?.status === "failed" ? "value-danger" : ""}>{smartState}</dd></div>
      <div><dt>错误日志</dt><dd>{!evidence ? (pending ? "待采集" : "未记录") : mock ? "未查询" : evidence.logAvailable ? "已读取" : "不可用"}</dd></div>
    </dl>
    {!mock && evidence?.note && <p className="scan-evidence-note">{evidence.note}</p>}
    {hints.length > 0 && <ScanReportHelp title={`历史地址 · ${hints.length} 条`}>
      <p>来自设备历史日志，不等于本次扫描确认的坏道。</p>
      <ul>{hints.map((hint, index) => <li key={index}>{hint.source}{hint.nsid > 0 && ` · NSID ${hint.nsid}`}<br />LBA {formatInteger(hint.lba)}</li>)}</ul>
    </ScanReportHelp>}
  </section>;
}

function ScanEvidenceDetails({ diagnostics, status }) {
  if (!diagnostics) return null;
  const before = diagnostics.before;
  const after = diagnostics.after;
  const evidence = after || before;
  if (!evidence) return null;
  const warnings = [...new Set([...(before?.warnings || []), ...(after?.warnings || [])].filter(Boolean))];
  const selfTestWarning = ["failed", "unknown"].includes(diagnostics.selfTest?.state);
  const increased = diagnostics.increased || [];
  const hasWarnings = warnings.length > 0 || increased.length > 0 || selfTestWarning;
  const mock = diagnostics.readMethod === "mock";
  const counters = Object.entries(SCAN_COUNTER_LABELS).filter(([key]) => before?.counters?.[key] != null || after?.counters?.[key] != null);
  const readMethod = { "nvme-read": "NVMe 原生读取", "scsi-read": "SCSI 协议读取", direct: "直接 I/O 读取", mock: "本地演示" }[diagnostics.readMethod] || "普通读取";
  const summary = mock ? "本地演示" : increased.length ? "扫描期间有指标增长" : hasWarnings ? "存在告警或历史记录" : evidence.available ? "未见已解析项目告警" : "健康信息不可用";
  const summaryNote = mock ? "未查询真实硬盘，以下为演示状态。" : increased.length
    ? increased.map((key) => SCAN_COUNTER_LABELS[key] || key).join("、")
    : hasWarnings ? "请结合告警记录和本次扫描结果判断。"
    : !evidence.available ? "设备未提供可用信息，无法据此判断健康状态。"
    : after && before ? "已对比扫描前后的健康信息。" : "目前仅有一次采集结果，暂不能判断扫描期间的变化。";
  return <section className="scan-evidence" aria-label="扫描健康信息">
    <div className={`scan-report-callout${hasWarnings && !mock ? " warning" : ""}`}>
      {hasWarnings && !mock ? <AlertTriangle size={19} aria-hidden="true" /> : <Info size={19} aria-hidden="true" />}
      <div><strong>{summary}</strong><p>{summaryNote}</p></div>
    </div>
    <div className="scan-evidence-snapshots">
      <ScanEvidenceSnapshot label="扫描前" evidence={before} mock={mock} />
      <ScanEvidenceSnapshot label="扫描后" evidence={after} mock={mock} pending={ACTIVE.has(status)} />
    </div>
    {warnings.length > 0 && <section className="scan-evidence-alerts" aria-label="告警记录">
      <h3>告警记录</h3><ul>{warnings.map((warning) => <li key={warning}>{warning}</li>)}</ul>
    </section>}
    {counters.length > 0 && <table className="scan-counter-table">
      <caption>健康计数对比</caption>
      <thead><tr><th scope="col">指标</th><th scope="col">扫描前</th><th scope="col">扫描后</th></tr></thead>
      <tbody>{counters.map(([key, label]) => <tr key={key}>
        <th scope="row">{label}</th>
        <td>{before?.counters?.[key] == null ? "未记录" : formatInteger(before.counters[key])}</td>
        <td className={increased.includes(key) ? "value-danger" : ""}>{after?.counters?.[key] == null ? (after ? "未提供" : "未采集") : formatInteger(after.counters[key])}{increased.includes(key) && <small>增长</small>}</td>
      </tr>)}</tbody>
    </table>}
    {diagnostics.selfTest && <section className="scan-self-test">
      <h3>固件自检</h3><p className={selfTestWarning ? "value-danger" : ""}>{diagnostics.selfTest.message}{diagnostics.selfTest.state === "running" ? ` · ${diagnostics.selfTest.percent}%` : ""}</p>
      {Number.isSafeInteger(diagnostics.selfTest.lba) && <p>NSID {diagnostics.selfTest.nsid} · LBA {formatInteger(diagnostics.selfTest.lba)}<small>固件报告线索，需结合扫描确认。</small></p>}
    </section>}
    <ScanReportHelp title={`读取与判定说明 · ${readMethod}`}>
      <p>{diagnostics.readMethod === "nvme-read" ? "NVMe 原生读取记录当前命名空间的完成状态，不使用机械盘相对耗时规则。" : diagnostics.readMethod === "scsi-read" ? "协议读取解析设备返回的介质、连接和超时错误。" : diagnostics.readMethod === "direct" ? "直接 I/O 绕过系统页缓存。" : mock ? "演示模式不读取真实硬盘。" : "当前使用非直接读取方式。"}</p>
      {diagnostics.readNote && <p>{diagnostics.readNote}</p>}
      {diagnostics.nvme?.nsid > 0 && <p>当前 NSID：{diagnostics.nvme.nsid} · 固件扩展自检：{diagnostics.nvme.selfTestSupported ? "支持" : "不支持或未确认"}</p>}
      <p>SMART／日志与本次读盘结果分别判断。读取成功不代表写入正常；缓存和已重映射的物理扇区由硬盘固件管理。</p>
    </ScanReportHelp>
  </section>;
}

function SmartInfoDialog({ disk, state, onRefresh, onClose }) {
  const report = state.report;
  return <ScanResultDialog title="原始 SMART 信息" className="smart-report-dialog" onClose={onClose} actions={
    <button type="button" className="button secondary" disabled={state.loading} onClick={onRefresh}><RefreshCw size={15} aria-hidden="true" />{state.error ? "重试" : "刷新"}</button>
  }>
    <p className="smart-report-meta">{report?.model || disk.model} · {disk.path}{report?.capturedAt && <span>{formatTime(report.capturedAt)}</span>}</p>
    {state.loading && <p className="smart-report-loading" role="status"><LoaderCircle size={18} className="spin" aria-hidden="true" />正在读取 SMART 信息…</p>}
    {state.error && <p className="inline-alert danger" role="alert">{errorMessage(state.error)}</p>}
    {report?.mock && <p className="scan-record-note">本地演示数据，未查询真实硬盘。</p>}
    {report?.warning && <p className="inline-alert" role="status">{report.warning}</p>}
    {report && <pre className="smart-report-output" tabIndex={0} aria-label="SMART 原始输出">{report.output}</pre>}
  </ScanResultDialog>;
}

function ScanResultDialog({ title, onClose, reportUrl, children, actions, className = "" }) {
  const dialogRef = useRef(null);
  useLayoutEffect(() => {
    const dialog = dialogRef.current;
    dialog.showModal();
    return () => dialog.close();
  }, []);
  return (
    <dialog ref={dialogRef} className={`region-dialog scan-result-dialog ${className}`} aria-labelledby="scan-result-dialog-title" onCancel={(event) => { event.preventDefault(); onClose(); }}>
      <div className="dialog-title">
        <Info size={20} aria-hidden="true" />
        <div><h2 id="scan-result-dialog-title">{title}</h2></div>
        <button type="button" className="icon-button" aria-label={`关闭${title}`} onClick={onClose} autoFocus><X size={20} /></button>
      </div>
      <div className="scan-result-dialog-body">{children}</div>
      <div className="dialog-actions">
        {actions}
        {reportUrl && <a className="button secondary" href={reportUrl} data-tooltip="下载可离线阅读、打印或保存为 PDF 的扫描报告" download>导出扫描报告</a>}
        <button type="button" className="button secondary" onClick={onClose}>关闭</button>
      </div>
    </dialog>
  );
}

function RecordedRepairDialog({ request, submitting, error, onClose, onConfirm }) {
  const dialogRef = useRef(null);
  const { job, disk } = request;
  const [threshold, setThreshold] = useState(disk.rotational ? 800 : 500);
  const [confirmation, setConfirmation] = useState("");
  const [acknowledged, setAcknowledged] = useState(false);
  const validThreshold = Number.isInteger(Number(threshold)) && Number(threshold) >= 1 && Number(threshold) <= 60000;
  const candidates = validThreshold ? recordedRepairCandidates(job, Number(threshold)) : [];
  useLayoutEffect(() => {
    const previousFocus = document.activeElement;
    const dialog = dialogRef.current;
    dialog.showModal();
    return () => { dialog.close(); previousFocus?.focus?.(); };
  }, []);
  return <dialog ref={dialogRef} className="confirm-dialog" role="alertdialog" aria-labelledby="recorded-repair-title" onCancel={(event) => { event.preventDefault(); if (!submitting) onClose(); }}>
    <div className="dialog-title"><AlertTriangle size={20} aria-hidden="true" /><h2 id="recorded-repair-title">按扫描记录修复慢区</h2><button type="button" className="icon-button" aria-label="关闭修复配置" disabled={submitting} onClick={onClose}><X size={20} /></button></div>
    <div className="dialog-body">
      <dl className="confirm-specs">
        <div><dt>硬盘</dt><dd>{disk.model} · {formatBytes(disk.size)}</dd></div>
        <div><dt>设备 / 序列号</dt><dd>{disk.path} · {disk.serial}</dd></div>
        <div><dt>扫描记录</dt><dd>{formatTime(job.finishedAt || job.createdAt)}</dd></div>
      </dl>
      <label className="recorded-repair-threshold">修复延迟门槛（毫秒）<input type="number" min="1" max="60000" step="1" value={threshold} disabled={submitting} onChange={(event) => setThreshold(event.target.value)} aria-invalid={!validThreshold} /></label>
      <p role="status">{validThreshold ? `已记录单次读取 ≥${threshold} ms 的准确范围：${candidates.length} 个 · ${formatBinaryBytes(candidates.reduce((total, cell) => total + cell.maxReadLatency.bytes, 0))}` : "请输入 1～60000 的整数。"}</p>
      <p className="scan-record-note">每个区域仅处理记录中保留的最慢请求。重新读取当前数据，确认完整、稳定后，备份并原样写回，再读回校验。即使本次读取变快，仍按你选择的历史慢读范围处理。原扫描记录保留。</p>
      {validThreshold && candidates.length === 0 && <p className="inline-alert warning">当前门槛下没有准确慢读地址。可调整门槛；缺少地址的旧记录需要重新扫描。</p>}
      <div className="inline-alert danger"><AlertTriangle size={18} aria-hidden="true" /><span>这是实际写盘操作。请先完成独立备份；断电、设备故障或写回失败可能损坏数据。写回校验通过只表示数据一致，速度另行复测。</span></div>
      <label className="recorded-repair-threshold">输入 REPAIR 确认写回风险<input value={confirmation} disabled={submitting} onChange={(event) => setConfirmation(event.target.value)} autoComplete="off" spellCheck="false" /></label>
      <label className="confirm-check"><input type="checkbox" disabled={submitting} checked={acknowledged} onChange={(event) => setAcknowledged(event.target.checked)} /><span>我已核对硬盘、完成独立备份，并了解写入风险。</span></label>
      {error && <p className="inline-alert danger" role="alert">{error}</p>}
    </div>
    <div className="dialog-actions"><button type="button" className="button secondary" disabled={submitting} onClick={onClose}>取消</button><button type="button" className="button danger" disabled={submitting || !validThreshold || !candidates.length || !acknowledged || confirmation.trim().toUpperCase() !== "REPAIR"} aria-busy={submitting} onClick={() => onConfirm({type:"scan", device:disk.path, scanMode:job.options?.scanMode || "full", scanRepair:true, scanRepairFrom:job.id, scanRepairAfterMs:Number(threshold), scanRepairAcknowledged:true, expectedDisk:{path:disk.path,serial:disk.serial,model:disk.model,size:disk.size}})}>{submitting ? "正在创建…" : `修复 ${candidates.length} 个范围`}</button></div>
  </dialog>;
}

function RecordedRepairPanel({ job, onReturnToScan, returning, returnDisabled, onOpenHistory }) {
  const [page, setPage] = useState(0);
  const targets = job.result?.repairTargets || [];
  const map = job.result?.map || [];
  const sector = job.disk?.logicalSectorSize || 512;
  const completed = job.result?.stats?.testedCells || 0;
  return <section className="recorded-repair-panel" aria-label="按记录修复结果">
    <div className="recorded-repair-heading">
      <div className="task-heading"><h2>按记录修复慢区</h2><StatusBadge status={job.status} /></div>
      <button type="button" className="button secondary" disabled={returnDisabled || returning} aria-busy={returning} data-tooltip={returnDisabled ? "请等待当前任务结束后返回" : "返回本次修复对应的扫描块图"} onClick={() => onReturnToScan(job)}>
        {returning ? <LoaderCircle size={16} className="spin" aria-hidden="true" /> : <ArrowLeft size={16} aria-hidden="true" />}{returning ? "正在返回…" : "返回扫描结果"}
      </button>
    </div>
    <p>{job.disk?.model} · {job.device}</p>
    <p role="status">{job.phase} · {completed} / {targets.length} 个范围</p>
    <progress aria-label="慢区修复进度" max="100" value={job.progress || 0} />
    <p className="scan-record-note">仅处理选定的历史慢读地址，不代表重新扫描整盘。来源记录：{job.options.scanRepairFrom}</p>
    {job.message && <p role="status">{job.message}</p>}
    <ScanRepairSummary job={job} />
    <div className="recorded-repair-table"><table><caption className="sr-only">历史慢读地址与本次写回结果</caption><thead><tr><th>原区域</th><th>LBA / 大小</th><th>历史耗时</th><th>本次响应</th><th>本次结果</th></tr></thead><tbody>{targets.slice(page*50, (page+1)*50).map((target, offset) => {
      const cell = map[page*50+offset];
      const repair = cell?.repair;
      const outcome = repair?.pending ? "结果未确定" : repair?.failed ? "写回失败" : repair?.stillSlow ? "修复后仍慢" : repair?.verified ? (repair.improved ? "慢速改善 · 写回校验通过" : "写回校验通过") : repair?.skipped ? "已跳过，未写入" : cell?.status === "reading" ? "处理中" : "等待处理";
      const responseStatus = cell ? scanStatusGroup(cell.status, cell.maxUnitMs, job.result?.latencyBasis) : "pending";
      const responseMs = cell?.maxReadLatency?.ms;
      return <tr key={target.cellIndex}><td>{target.cellIndex+1}</td><td>{formatInteger(Math.floor(target.read.offset/sector))}–{formatInteger(Math.floor((target.read.offset+target.read.bytes-1)/sector))}<small>{formatBinaryBytes(target.read.bytes)}</small></td><td>{formatMilliseconds(target.read.ms)}</td><td><span className="recorded-repair-response" data-tooltip={`按本次读取判定：${scanStatusLabel(responseStatus)}`}><i className={`block-${responseStatus}`} aria-hidden="true" />{Number.isFinite(responseMs) ? formatMilliseconds(responseMs) : "—"}</span></td><td>{outcome}</td></tr>;
    })}</tbody></table></div>
    {targets.length > 50 && <nav className="dialog-actions" aria-label="修复范围分页"><button type="button" className="button secondary" disabled={page === 0} onClick={() => setPage(page-1)}>上一页</button><span>{page+1} / {Math.ceil(targets.length/50)}</span><button type="button" className="button secondary" disabled={(page+1)*50 >= targets.length} onClick={() => setPage(page+1)}>下一页</button></nav>}
    <div className="recorded-repair-actions">
      <a className="button secondary" href={`${API}/jobs/${job.id}/scan-report`} download>导出修复报告</a>
      <HistoryTrigger onOpen={onOpenHistory} inline />
    </div>
  </section>;
}

const ScanCellButton = React.memo(function ScanCellButton({ cell, columnCount, inspected, pinned, keyboard, writeMode, latencyBasis }) {
  const displayStatus = cell.status !== "reading" ? scanStatusGroup(cell.status, cell.maxUnitMs, latencyBasis) : "pending";
  return (
    <button
      type="button"
      role="gridcell"
      data-cell-index={cell.index}
      tabIndex={keyboard ? 0 : -1}
      aria-selected={pinned}
      aria-rowindex={Math.floor(cell.index / columnCount) + 1}
      aria-colindex={(cell.index % columnCount) + 1}
      aria-label={`区域 ${cell.index + 1}，${scanStatusLabel(displayStatus)}`}
      className={`block-cell block-${displayStatus} ${cell.index < columnCount ? "is-top-edge" : ""} ${cell.index % columnCount === 0 ? "is-left-edge" : ""} ${(cell.index + 1) % columnCount === 0 ? "is-right-edge" : ""} ${inspected ? "is-inspected" : ""} ${pinned ? "is-pinned" : ""}`}
      data-tooltip={`区域 ${cell.index + 1} · ${formatDiskOffset(cell.start)}–${formatDiskOffset(cell.end)}\n状态：${scanStatusLabel(displayStatus)}${cell.maxMs == null ? "" : `\n最大响应：${cell.maxMs} ms\n平均响应：${cell.avgMs} ms\n已读：${formatBytes(cell.bytesRead)}`}${cell.errorCount ? `\n${writeMode ? "读写/校验错误" : "读取异常"}：${cell.errorCount}` : ""}`}
    />
  );
});

function ScanBlockMap({ job, onOpenHistory, onRepairRecords, repairDisabledReason }) {
  const latencyBasis = job.result?.latencyBasis || "";
  const mediaLatency = job.status === "idle" || /-dual-v4$/.test(latencyBasis);
  const thousandMsLatency = /-dual-v3$/.test(latencyBasis);
  const writeLatency = /^(hdd|solid)-write-dual-v[234]$/.test(latencyBasis);
  const dualLatency = /^(hdd|solid)-(write-)?dual-v[234]$/.test(latencyBasis);
  const hddLatency = dualLatency || latencyBasis === "hdd-relative-v1";
  const rotationalLatency = /^hdd-(write-)?dual-v[234]$/.test(latencyBasis);
  const readSlowMs = rotationalLatency ? 50 : 20;
  const readPoorMs = rotationalLatency ? mediaLatency ? 200 : 150 : 50;
  const readSevereMs = mediaLatency ? rotationalLatency ? 800 : 500 : thousandMsLatency ? 1000 : rotationalLatency ? 500 : 100;
  const transferSlowMs = mediaLatency ? readSlowMs : 50;
  const transferPoorMs = mediaLatency ? readPoorMs : 150;
  const transferSevereMs = mediaLatency ? readSevereMs : thousandMsLatency ? 1000 : 500;
  const scanLegend = SCAN_LEGEND.map(([status, label, description]) => [status, label,
    status === "normal" && mediaLatency ? "未达到一般、较差或严重阈值；较快结果细分为优秀、良好"
      : status === "slow" ? `折算耗时 ${transferSlowMs}–< ${transferPoorMs} ms`
      : status === "slower" ? `折算耗时 ${transferPoorMs}–< ${transferSevereMs} ms`
      : status === "severe" ? `折算耗时 ≥ ${transferSevereMs} ms` : description]);
  const requestThresholds = rotationalLatency
    ? `HDD 单次读取 ≥${readSlowMs} ms 一般、≥${readPoorMs} ms 较差、≥${readSevereMs} ms 严重${writeLatency ? "" : mediaLatency ? "；相对基准波动仅作一般告警，较差与严重按绝对耗时判断" : `；另结合近期同长度读取基准，3 倍一般、10 倍${thousandMsLatency ? "较差；相对倍数不单独判为严重" : "严重"}`}`
    : `SSD/NVMe 等非机械设备单次读取 ≥20 ms 一般、≥50 ms 较差、≥${readSevereMs} ms 严重`;
  const writeThresholds = rotationalLatency
    ? "HDD 单次写入 ≥100 ms 一般、≥300 ms 较差、≥1000 ms 严重"
    : "SSD/NVMe 等非机械设备单次写入 ≥50 ms 一般、≥150 ms 较差、≥500 ms 严重";
  const rawMap = job.result?.map;
  const map = safeScanMap(rawMap);
  const stats = job.result?.stats || {};
  const outcomeSummary = scanOutcomeSummary(job);
  const mapRef = useRef(null);
  const followPausedUntilRef = useRef(0);
  const detailDialogRef = useRef(null);
  const [hoveredIndex, setHoveredIndex] = useState(null);
  const [pinnedIndex, setPinnedIndex] = useState(null);
  const [detailIndex, setDetailIndex] = useState(null);
  const [summaryView, setSummaryView] = useState(null);
  // Render the latest snapshot directly, including history loaded after mount.
  // Progress updates must not queue a second, artificial scan in the UI.
  const completedPrefix = terminalScanPrefix(map);
  useEffect(() => {
    setHoveredIndex(null);
    setPinnedIndex(null);
    setDetailIndex(null);
    setSummaryView(null);
  }, [job.id, map.length]);

  useEffect(() => {
    const dialog = detailDialogRef.current;
    if (!dialog) return;
    if (Number.isInteger(detailIndex) && !dialog.open) dialog.showModal();
    if (!Number.isInteger(detailIndex) && dialog.open) dialog.close();
  }, [detailIndex]);

  const followIndex = ACTIVE.has(job.status) && completedPrefix < map.length
    ? completedPrefix
    : -1;
  const columnCount = map.length > 512 ? 64 : 32;
  const followRow = followIndex < 0 ? -1 : Math.floor(followIndex / columnCount);
  useLayoutEffect(() => {
    const grid = mapRef.current;
    if (followRow < 0 || !grid || Number.isInteger(detailIndex) || Number.isInteger(pinnedIndex)
      || Date.now() < followPausedUntilRef.current || grid.scrollHeight <= grid.clientHeight) return;

    const cell = grid.children[followRow * columnCount];
    const firstCell = grid.firstElementChild;
    if (!cell || !firstCell) return;
    const cellTop = cell.offsetTop - firstCell.offsetTop;
    const rowHeight = cell.offsetHeight + (parseFloat(getComputedStyle(grid).rowGap) || 0);
    const viewTop = grid.scrollTop;
    const viewBottom = viewTop + grid.clientHeight;
    if (cellTop + cell.offsetHeight > viewBottom - rowHeight) {
      grid.scrollTop = Math.min(grid.scrollHeight - grid.clientHeight,
        cellTop + cell.offsetHeight - grid.clientHeight + rowHeight);
    } else if (cellTop < viewTop) {
      grid.scrollTop = Math.max(0, cellTop - rowHeight);
    }
  }, [job.id, job.status, followRow, columnCount, detailIndex, pinnedIndex, map.length]);

  if (!map.length) {
    return rawMap === undefined ? null : (
      <div className="inline-alert warning" role="alert"><AlertTriangle size={18} /><span>历史块图数据不完整，无法安全显示扫描区域。</span></div>
    );
  }
  const diskSize = job.disk?.size || map.at(-1)?.end || 0;
  const logicalSectorSize = Number(job.disk?.logicalSectorSize) || 512;
  const bytesPerCell = diskSize / map.length;
  const sampleBytes = job.result?.sampleBytes || (map.length === QUICK_SCAN_CELL_COUNT ? QUICK_SAMPLE_BYTES : LEGACY_QUICK_SAMPLE_BYTES);
  const statusCounts = map.reduce((counts, cell) => {
    const status = cell.status !== "reading" ? scanStatusGroup(cell.status, cell.maxUnitMs, latencyBasis) : "pending";
    counts[status] = (counts[status] || 0) + 1;
    return counts;
  }, {});
  const severeOrErrorCount = (statusCounts.severe || 0) + (statusCounts.error || 0);
  const abnormalCount = job.type === "write" ? statusCounts.error || 0 : severeOrErrorCount;
  const completedCount = map.length - (statusCounts.pending || 0);
  const badBlockCounts = scanBadBlockSummary(map,Boolean(job.disk?.rotational));
  const mapReadBytes = map.reduce((total, cell) => total + (cell.bytesRead || 0), 0);
  const readBytes = Number.isSafeInteger(stats.bytesRead) && stats.bytesRead >= 0 ? stats.bytesRead : mapReadBytes;
  const sampledScan = job.result.coverage === "sampled";
  const scanTypeLabel = job.type === "write" ? "深度检测（旧记录）" : sampledScan ? "抽样检测" : "全盘检测";
  const readMethodLabel = job.type === "write" ? "每格写读校验" : sampledScan ? `${job.result.samplingPlan === "stratified-v1" ? "每格分散抽读" : "每格抽读"} ${formatBinaryBytes(sampleBytes)}` : "完整读取每格";
  const issueCount = scanIssueDisplayCount(job);
  const diagnostics = job.result?.diagnostics;
  const hasEvidence = Boolean(diagnostics?.before || diagnostics?.after);
  const evidenceHasWarnings = Boolean(diagnostics?.increased?.length || diagnostics?.before?.warnings?.length || diagnostics?.after?.warnings?.length || ["failed", "unknown"].includes(diagnostics?.selfTest?.state));
  const reportUrl = !job.id.startsWith("idle-") && job.status !== "idle" ? `${API}/jobs/${job.id}/scan-report` : "";
  const scanErrorLabel = job.type === "write" ? "读写/校验错误" : "读取异常";
	const liveCell = ACTIVE.has(job.status) && completedPrefix < map.length
	  ? map[completedPrefix]
	  : null;
  let lastTestedCell = null;
  for (let index = map.length - 1; index >= 0; index -= 1) {
	if (!["pending", "reading"].includes(map[index].status)) {
      lastTestedCell = map[index];
      break;
    }
  }
  const inspectedCell = (Number.isInteger(hoveredIndex) && map[hoveredIndex])
    || (Number.isInteger(pinnedIndex) && map[pinnedIndex])
    || liveCell
    || lastTestedCell
    || map[0];
  const keyboardIndex = Number.isInteger(pinnedIndex) ? pinnedIndex : inspectedCell.index;
  const positionPercent = diskSize > 0 ? (inspectedCell.start / diskSize) * 100 : 0;
  const startLba = Math.floor(inspectedCell.start / logicalSectorSize);
  const endLba = Math.max(startLba, Math.ceil(inspectedCell.end / logicalSectorSize) - 1);
  const logicalCylinder = Math.floor(startLba / (255 * 63));
  const sampledRegionLength = Math.max(4096, inspectedCell.end - inspectedCell.start);
  const derivedSampleLength = Math.min(sampleBytes, Math.floor(sampledRegionLength / 4096) * 4096 || 4096);
  const derivedSampleCenter = inspectedCell.start + Math.max(0, Math.floor((sampledRegionLength - derivedSampleLength) / 2));
  const derivedSampleStart = Math.min(Math.max(0, diskSize - derivedSampleLength), Math.floor(derivedSampleCenter / 4096) * 4096);
  const sampleStart = Number.isFinite(inspectedCell.sampleStart)
    ? inspectedCell.sampleStart
    : (job.result.coverage === "sampled" && inspectedCell.bytesRead > 0 ? derivedSampleStart : null);
  const sampleEnd = Number.isFinite(inspectedCell.sampleEnd)
    ? inspectedCell.sampleEnd
    : (Number.isFinite(sampleStart) ? sampleStart + derivedSampleLength : null);
  const hasSampleRange = job.result.coverage === "sampled" && Number.isFinite(sampleStart) && Number.isFinite(sampleEnd);
  const sampleStartLba = hasSampleRange ? Math.floor(sampleStart / logicalSectorSize) : null;
  const sampleEndLba = hasSampleRange ? Math.max(sampleStartLba, Math.ceil(sampleEnd / logicalSectorSize) - 1) : null;
  const sampleLbaLabel = inspectedCell.sampleRanges?.length
    ? inspectedCell.sampleRanges.map((range) => formatInteger(Math.floor(range.start / logicalSectorSize)) + " - " + formatInteger(Math.ceil(range.end / logicalSectorSize)-1)).join("；")
    : hasSampleRange ? formatInteger(sampleStartLba) + " - " + formatInteger(sampleEndLba) : "尚未抽读";
  const badRanges = Array.isArray(stats.badRanges) ? stats.badRanges : [];
  const cellBadRanges = mergeTimeoutSkipRanges(badRanges.filter((range) => (
    Number.isFinite(range?.offset)
    && Number.isFinite(range?.length)
    && range.length > 0
    && range.offset < inspectedCell.end
    && range.offset + range.length > inspectedCell.start
  )));
  const inspectedSlowProof = scanSlowBadBlock(inspectedCell,Boolean(job.disk?.rotational));
  const inspectedLocation = scanCellLocationSummary(inspectedCell, cellBadRanges, job.disk?.logicalSectorSize);
  const inspectedStatus = job.type === "scan" && inspectedCell.status === "error"
    ? (inspectedCell.unreadableBytes > 0 ? "坏块（不可读）" : inspectedCell.errorCount > 0 ? "读取异常" : "严重卡读")
    : scanStatusLabel(scanStatusGroup(inspectedCell.status, inspectedCell.maxUnitMs, latencyBasis));

  function focusCell(index) {
    const nextIndex = Math.max(0, Math.min(map.length - 1, index));
    setHoveredIndex(null);
    setPinnedIndex(nextIndex);
    window.requestAnimationFrame(() => {
      mapRef.current?.querySelector(`[data-cell-index="${nextIndex}"]`)?.focus();
    });
  }

  function handleCellKeyDown(event, index) {
    let nextIndex = null;
    if (event.key === "ArrowLeft") nextIndex = index - 1;
    if (event.key === "ArrowRight") nextIndex = index + 1;
    if (event.key === "ArrowUp") nextIndex = index - columnCount;
    if (event.key === "ArrowDown") nextIndex = index + columnCount;
    if (event.key === "Home") nextIndex = event.ctrlKey ? 0 : index - (index % columnCount);
    if (event.key === "End") nextIndex = event.ctrlKey ? map.length - 1 : Math.min(map.length - 1, index + columnCount - 1 - (index % columnCount));
    if (nextIndex == null) return;
    event.preventDefault();
    focusCell(nextIndex);
  }

  function openDetail(index) {
    setHoveredIndex(null);
    setPinnedIndex(index);
    setDetailIndex(index);
  }

  function closeDetail() {
    setDetailIndex(null);
    setPinnedIndex(null);
  }

  return (
    <div className="scan-map-panel">
      <div className="scan-map-layout">
        <div className="map-visual">
          <div
            ref={mapRef}
            className={`block-map${map.length === QUICK_SCAN_CELL_COUNT ? " dense" : ""}`}
            style={{ "--map-columns": columnCount }}
            role="grid"
            aria-label={`磁盘扫描块图，已扫描 ${completedCount} 个区域，${abnormalCount} 个异常区域`}
            aria-colcount={columnCount}
            aria-rowcount={Math.ceil(map.length / columnCount)}
            onWheel={() => { followPausedUntilRef.current = Date.now() + 5_000; }}
            onTouchStart={() => { followPausedUntilRef.current = Date.now() + 5_000; }}
            onPointerOver={(event) => {
              const index = Number(event.target?.dataset?.cellIndex);
              if (Number.isInteger(index) && index >= 0 && index < map.length) setHoveredIndex(index);
            }}
            onPointerLeave={() => setHoveredIndex(null)}
            onClick={(event) => {
              const index = Number(event.target?.dataset?.cellIndex);
              if (Number.isInteger(index) && index >= 0 && index < map.length) openDetail(index);
            }}
            onFocusCapture={(event) => {
              const index = Number(event.target?.dataset?.cellIndex);
              if (Number.isInteger(index) && index >= 0 && index < map.length) setPinnedIndex(index);
            }}
            onKeyDown={(event) => {
              const index = Number(event.target?.dataset?.cellIndex);
              if (Number.isInteger(index) && index >= 0 && index < map.length) handleCellKeyDown(event, index);
            }}
          >
            {map.map((cell) => (
              <ScanCellButton
                key={cell.index}
                cell={cell}
                columnCount={columnCount}
                inspected={inspectedCell.index === cell.index}
                pinned={pinnedIndex === cell.index}
                keyboard={cell.index === keyboardIndex}
                writeMode={job.type === "write"}
                latencyBasis={latencyBasis}
              />
            ))}
          </div>
          <div className="block-legend" aria-label="区域检测结果图例">
            {scanLegend.slice(0, 5).map(([status, label, description]) => <span key={status} data-tooltip={dualLatency ? `传输折算档位：${description}；${requestThresholds}${writeLatency ? `；${writeThresholds}` : ""}。请求不超过 128 KiB，记录主机读写等待时间；传输折算与单次请求分别判断，取较严重结果，读写及校验错误优先。单次超限保留告警；同范围复测持续超限按慢速坏块统计。` : hddLatency ? `${description}；另结合近期同长度读取的中位基准，3 倍标一般、10 倍标严重，单次等待 500 ms 也标一般。保留最差结果；同范围复测持续超限按慢速坏块统计，异常原因见区域详情。` : job.result?.latencyBasis === "per-mib-v1" || job.status === "idle" ? `${description}。每次传输按 1 MiB 折算，不足 1 MiB 按 1 MiB；取区域内最差值。深度检测计入写入与读回总量。正常仅代表本次检测未见异常。` : "旧记录保留原耗时判定；重新扫描使用按传输量折算的新规则。"}><i className={`block-${status}`} />{label}<b>{statusCounts[status] || 0}</b></span>)}
            <span className="block-legend-combined" data-tooltip={`红色表示坏块、严重慢读、卡读跳过或${scanErrorLabel}。坏块按正常使用要求判断，保留慢速／不可读原因。${dualLatency ? `${requestThresholds}；传输折算耗时 ≥${transferSevereMs} ms 也判为严重。${writeLatency ? `；${writeThresholds}。` : ""}` : "按扫描记录的严重档位显示。"}\n严重慢读 ${statusCounts.severe || 0} · ${scanErrorLabel} ${statusCounts.error || 0}`}>
              <i className="block-error" aria-hidden="true" />严重<b>{severeOrErrorCount}</b>
            </span>
            <span><i className="block-pending" />未扫描<b>{(statusCounts.pending || 0) + (statusCounts.reading || 0)}</b></span>
          </div>
        </div>
        <aside className="map-summary" aria-label="扫描关键统计">
          <dl className="scan-map-stats">
            <div><dt>已处理区域</dt><dd>{completedCount} / {map.length}</dd></div>
            <div><dt>{job.type === "write" ? "写入量" : "读取量"}</dt><dd>{formatBytes(readBytes)}</dd></div>
            <div><dt>慢速区域</dt><dd>{(statusCounts.slow || 0) + (statusCounts.slower || 0) + (job.type === "write" ? statusCounts.severe || 0 : 0)}</dd></div>
            <div data-tooltip={`坏块（慢速）${badBlockCounts.slow} · 坏块（不可读）${badBlockCounts.unreadable}；同一区域只计一次，数量不等于物理扇区数。其他红色卡顿仍保留告警。`}><dt>{job.type === "write" ? "异常区域" : "坏块区域"}</dt><dd className={(job.type === "write" ? abnormalCount : badBlockCounts.total) ? "value-danger" : ""}>{job.type === "write" ? abnormalCount : badBlockCounts.total}</dd></div>
          </dl>
        </aside>
      </div>
      <div className="scan-detail-toolbar" aria-label="扫描详细信息">
        <span className="scan-type-label">{scanTypeLabel}</span>
        <div className="scan-detail-actions">
          <button type="button" className="scan-detail-button" aria-haspopup="dialog" onClick={() => setSummaryView("overview")}>扫描详情</button>
          {job.type === "scan" && <button type="button" className={`scan-detail-button${issueCount > 0 ? " value-danger" : ""}`} aria-haspopup="dialog" onClick={() => setSummaryView("issues")}>异常明细{issueCount > 0 ? ` · ${formatInteger(issueCount)}` : ""}</button>}
          <button type="button" className={`scan-detail-button${evidenceHasWarnings ? " value-danger" : ""}`} aria-haspopup="dialog" onClick={() => setSummaryView("evidence")}>SMART／日志{evidenceHasWarnings ? " · 告警" : ""}</button>
          {job.type === "scan" && job.status === "completed" && <ImmediateTooltip text={repairDisabledReason || "按本次扫描保存的准确慢读地址修复"} focusable={Boolean(repairDisabledReason)}><button type="button" className="scan-detail-button" disabled={Boolean(repairDisabledReason)} aria-haspopup="dialog" onClick={() => onRepairRecords(job)}>修复慢区</button></ImmediateTooltip>}
          <HistoryTrigger onOpen={onOpenHistory} inline />
        </div>
      </div>
      {stats.issueWarning && <div className="inline-alert warning" role="alert"><AlertTriangle size={16} /><span>{stats.issueWarning}</span></div>}
      {summaryView && <ScanResultDialog title={{ overview: "扫描详情", issues: "异常明细", evidence: "SMART／日志" }[summaryView]} className={summaryView === "issues" ? "scan-issues-dialog" : "scan-report-dialog"} onClose={() => setSummaryView(null)} reportUrl={reportUrl}>
        {summaryView !== "issues" && <ScanReportContext job={job} />}
        {summaryView === "overview" && <div className="scan-overview">
          <dl className="scan-overview-metrics">
            <div><dt>任务进度</dt><dd>{((completedCount / map.length) * 100).toFixed(1)}<span>%</span></dd><progress aria-label="扫描任务进度" max={map.length} value={completedCount} /><small>{formatInteger(completedCount)} / {formatInteger(map.length)} 个区域</small></div>
            <div><dt>已判定范围占整盘</dt><dd>{outcomeSummary ? (diskSize > 0 ? ((outcomeSummary.coveredBytes / diskSize) * 100).toFixed(3) : "0.000") : "—"}{outcomeSummary && <span>%</span>}</dd>{outcomeSummary ? <><progress aria-label="整盘判定覆盖率" max={diskSize || 1} value={outcomeSummary.coveredBytes} /><small>{sampledScan ? "抽样检测，仅覆盖样本" : "包含已读出与持续不可读范围"}</small></> : <small>此记录未提供覆盖数据</small>}</div>
            <div><dt>{job.type === "write" ? "写入量" : "读取量"}</dt><dd>{formatBytes(readBytes)}</dd><small>{job.type === "write" ? "旧版写读校验记录" : "实际完整读出的数据"}</small></div>
          </dl>
          <dl className="scan-overview-config">
            <div><dt>检测方式</dt><dd>{scanTypeLabel}</dd></div>
            <div><dt>块图划分</dt><dd>{formatInteger(map.length)} 格 · 每格约 {formatBytes(bytesPerCell)}</dd></div>
            <div><dt>读取方式</dt><dd>{readMethodLabel}</dd></div>
          </dl>
          {outcomeSummary && <section className="scan-findings" aria-labelledby="scan-findings-title">
            <div className="scan-report-section-heading"><h3 id="scan-findings-title">异常与告警</h3>{issueCount > 0 && <button type="button" className="scan-detail-button" onClick={() => setSummaryView("issues")}>查看异常明细 · {formatInteger(issueCount)} 条</button>}</div>
            <dl className="scan-findings-primary">
              <div data-tooltip="同一范围复测后仍持续超过可接受延迟"><dt>坏块（慢速）</dt><dd className={badBlockCounts.slow ? "value-danger" : ""}>{formatInteger(badBlockCounts.slow)}<span> 个区域</span></dd></div>
              <div data-tooltip="重试与细分定位后，仍无法完整读取"><dt>坏块（不可读）</dt><dd className={badBlockCounts.unreadable ? "value-danger" : ""}>{formatInteger(badBlockCounts.unreadable)}<span> 个区域</span></dd></div>
              <div data-tooltip="包含严重慢读、读取错误、待确认卡顿与设备异常"><dt>异常区域总计</dt><dd>{formatInteger(abnormalCount)}<span> 个区域</span></dd></div>
            </dl>
            <dl className="scan-findings-secondary">
              <div data-tooltip="定位后仍无法完整读取的范围；范围数不等于物理坏扇区数"><dt>持续不可读</dt><dd className={outcomeSummary.unreadableRanges ? "value-danger" : ""}>{formatInteger(outcomeSummary.unreadableRanges)} 个范围 · {formatBinaryBytes(outcomeSummary.unreadableBytes)}</dd></div>
              <div data-tooltip="复读恢复与固件已纠正的记录，保留首次告警"><dt>恢复告警</dt><dd>{formatInteger(outcomeSummary.recoveredRanges)} 条</dd></div>
              <div data-tooltip="超时、连接、控制器等错误，不能据此判定介质坏区"><dt>设备异常</dt><dd>{formatInteger(outcomeSummary.deviceErrorRanges)} 条</dd></div>
              {outcomeSummary.skippedBytes > 0 && <div><dt>卡读跳过</dt><dd>{formatBinaryBytes(outcomeSummary.skippedBytes)}<small>未完成读取检测</small></dd></div>}
              {outcomeSummary.unresolvedBytes > outcomeSummary.skippedBytes && <div><dt>待细查范围</dt><dd>{formatBinaryBytes(outcomeSummary.unresolvedBytes - outcomeSummary.skippedBytes)}</dd></div>}
            </dl>
          </section>}
          <ScanReportHelp title="如何理解扫描结果">
            <p>{sampledScan ? "抽样检测只读取每格中的样本，进度完成不等于读完整盘。" : "全盘检测逐段读取磁盘，未完成或中断的任务仍有未检测范围。"}</p>
            <p>覆盖率包含已读出和已确认持续不可读的范围，跳过及待细查范围不计入；它与任务进度含义不同。</p>
            <p>慢速坏块可读，但同范围复测持续超限；不可读坏块经重试定位仍未完整读出。异常区域还包含尚待确认的严重慢读、卡顿和设备错误。</p>
          </ScanReportHelp>
        </div>}
        {summaryView === "issues" && (issueCount > 0
          ? <ScanIssueBrowser key={job.id} job={job} />
          : <p className="scan-record-note">{job.status === "idle" ? "尚未开始扫描，暂无异常明细。" : "本次扫描尚未记录到异常。"}</p>)}
        {summaryView === "evidence" && (hasEvidence
          ? <ScanEvidenceDetails diagnostics={diagnostics} status={job.status} />
          : <div className="scan-report-callout"><Info size={19} aria-hidden="true" /><div><strong>{job.status === "idle" ? "尚未开始扫描" : "未记录健康信息"}</strong><p>{job.status === "idle" ? "开始扫描后，在这里查看本次 SMART／日志。" : "此记录没有可用的 SMART／日志，无法进行扫描前后对比。"}</p></div></div>)}
      </ScanResultDialog>}
      <dialog
        ref={detailDialogRef}
        className="region-dialog"
        aria-labelledby="region-detail-title"
        onCancel={(event) => { event.preventDefault(); closeDetail(); }}
        onClose={closeDetail}
        onKeyDown={(event) => {
          if (event.key === "Escape") {
            event.preventDefault();
            closeDetail();
          }
        }}
      >
        <div className="dialog-title">
          <span className={`dialog-icon region-dialog-swatch block-${scanStatusGroup(inspectedCell.status, inspectedCell.maxUnitMs, latencyBasis)}`} aria-hidden="true" />
          <div><p className="eyebrow">区域详情</p><h2 id="region-detail-title">区域 {inspectedCell.index + 1}</h2></div>
          <button className="icon-button" onClick={closeDetail} aria-label="关闭区域详情" data-tooltip="关闭"><X size={20} /></button>
        </div>
        <div className="dialog-body">
          <div className="region-dialog-status"><span>{inspectedStatus}</span><small>磁盘位置 {positionPercent.toFixed(2)}%</small></div>
          <dl className="region-detail-grid">
            <div><dt>容量范围</dt><dd>{formatDiskOffset(inspectedCell.start)} - {formatDiskOffset(inspectedCell.end)}</dd></div>
            <div><dt>LBA 范围</dt><dd>{formatInteger(startLba)} - {formatInteger(endLba)}</dd></div>
            <div><dt>逻辑柱面</dt><dd>{formatInteger(logicalCylinder)}</dd></div>
            <div><dt>{writeLatency ? "整次写读最大耗时" : dualLatency ? "整次读取最大耗时" : "最大响应"}</dt><dd>{formatMilliseconds(inspectedCell.maxMs)}</dd></div>
            <div><dt>{writeLatency ? "整次写读平均耗时" : dualLatency ? "整次读取平均耗时" : "平均响应"}</dt><dd>{formatMilliseconds(inspectedCell.avgMs)}</dd></div>
            {inspectedCell.maxWriteLatency && <>
              <div data-tooltip="不超过 128 KiB 的主机写入请求耗时，包含驱动与调度等待，不代表断电后数据已持久保存"><dt>最慢单次写入</dt><dd>{formatMilliseconds(inspectedCell.maxWriteLatency.ms)} · {formatBytes(inspectedCell.maxWriteLatency.bytes)}</dd></div>
              <div data-tooltip={writeThresholds}><dt>最慢写入 LBA</dt><dd>{formatInteger(Math.floor(inspectedCell.maxWriteLatency.offset / logicalSectorSize))}</dd></div>
            </>}
            {inspectedCell.maxReadLatency && <>
              <div data-tooltip="不超过 128 KiB 的主机读取请求耗时，包含驱动与调度等待，不是纯固件服务时间"><dt>{job.type === "write" ? "最慢单次读回" : "最慢单次读取"}</dt><dd>{formatMilliseconds(inspectedCell.maxReadLatency.ms)} · {formatBytes(inspectedCell.maxReadLatency.bytes)}</dd></div>
              <div data-tooltip={requestThresholds}><dt>{job.type === "write" ? "最慢读回 LBA" : "最慢读取 LBA"}</dt><dd>{formatInteger(Math.floor(inspectedCell.maxReadLatency.offset / logicalSectorSize))}</dd></div>
            </>}
            {inspectedCell.latencyWarning && <div><dt>响应异常依据</dt><dd>{inspectedCell.latencyWarning === "stall" ? "单次读取等待达到 500 ms" : `达到近期同长度读取基准的 ${Number(inspectedCell.maxLatencyRatio).toFixed(1)} 倍`}（持续性见同范围复测）</dd></div>}
            {Number.isFinite(inspectedCell.maxUnitMs) && <div data-tooltip="每次传输按 1 MiB 折算，不足 1 MiB 按 1 MiB，取区域内最差值；原始响应时间保持不变"><dt>{dualLatency ? "传输折算耗时" : "判色折算耗时"}</dt><dd>{formatMilliseconds(inspectedCell.maxUnitMs)}</dd></div>}
            <div><dt>{job.type === "write" ? "实际写入" : "实际读取"}</dt><dd>{formatBytes(inspectedCell.bytesRead)}</dd></div>
            <div><dt>{job.result.coverage === "sampled" ? "抽读 LBA" : job.type === "write" ? "写读轮次" : "读取次数"}</dt><dd>{job.result.coverage === "sampled" ? sampleLbaLabel : formatInteger(inspectedCell.reads || 0)}</dd></div>
            <div><dt>{job.type === "write" ? "读写/校验错误" : "读取异常记录"}</dt><dd className={inspectedCell.errorCount ? "value-danger" : ""}>{inspectedCell.errorCount || 0}</dd></div>
            {outcomeSummary && <>
              <div><dt>坏扇区定位</dt><dd>{inspectedLocation}</dd></div>
              <div><dt>读取结论</dt><dd>{scanBadBlockKind(inspectedCell,Boolean(job.disk?.rotational)) === "unreadable" ? "坏块（不可读）" : scanBadBlockKind(inspectedCell,Boolean(job.disk?.rotational)) === "slow" ? "坏块（慢速）· 已不满足正常使用要求" : inspectedCell.skippedBytes > 0 ? "严重卡读，已跳过剩余范围" : inspectedCell.readOutcome === "readable" && scanStatusGroup(inspectedCell.status, inspectedCell.maxUnitMs, latencyBasis) === "severe" ? "严重慢读，持续性尚待确认" : ({ readable: "完整读出", recovered: "复读恢复，保留告警", device_error: "设备异常，需核查连接和控制器", unresolved: "异常范围未确定", partial: "部分读取，尚未完成" }[inspectedCell.readOutcome] || "尚未完成")}</dd></div>
              <div><dt>已判定覆盖</dt><dd>{formatBinaryBytes(inspectedCell.coveredBytes || 0)}{job.result.coverage === "full" && (inspectedCell.coveredBytes || 0) < inspectedCell.end - inspectedCell.start ? " · 未覆盖完整区域" : ""}</dd></div>
            </>}
            {inspectedCell.load?.backgroundActivity && <div><dt>扫描时磁盘活动</dt><dd>检测到并发磁盘活动，首次慢响应保留；不能仅凭耗时判定介质异常。</dd></div>}
            {inspectedCell.skippedBytes > 0 && <div><dt>卡读跳过</dt><dd>{formatBinaryBytes(inspectedCell.skippedBytes)} · 未完成读取检测</dd></div>}
            {inspectedCell.unresolvedBytes > (inspectedCell.skippedBytes || 0) && <div><dt>待细查范围</dt><dd>{formatBinaryBytes(inspectedCell.unresolvedBytes - (inspectedCell.skippedBytes || 0))} · 达到定位限额，可使用深入定位重新检测</dd></div>}
            {inspectedSlowProof && <div><dt>慢速坏块依据</dt><dd>LBA {formatInteger(Math.floor(inspectedSlowProof.offset/logicalSectorSize))} · 相同 {formatBinaryBytes(inspectedSlowProof.bytes)} 请求 · {formatMilliseconds(inspectedSlowProof.firstMs)} / {formatMilliseconds(inspectedSlowProof.repeatMs)}；复测期间未检测到并发负载</dd></div>}
            {inspectedCell.recheck && <>
              <div><dt>慢速只读复测</dt><dd>{{ not_reproduced: "未触发复测慢速阈值（保留首次响应）", slow: "复测仍然偏慢", read_issue: "复测出现读取异常" }[inspectedCell.recheck.outcome] || "已复测"}</dd></div>
              {inspectedCell.recheck.load?.backgroundActivity && <div><dt>复测环境</dt><dd>复测期间仍有并发磁盘活动，耗时结论需空闲时确认</dd></div>}
              <div><dt>复测最差折算耗时</dt><dd>{formatMilliseconds(inspectedCell.recheck.maxUnitMs)}</dd></div>
              {inspectedCell.recheck.maxReadLatency && <div><dt>复测最慢单次读取</dt><dd>{formatMilliseconds(inspectedCell.recheck.maxReadLatency.ms)} · {formatBytes(inspectedCell.recheck.maxReadLatency.bytes)}</dd></div>}
              <div><dt>额外复测读取量</dt><dd>{formatBytes(inspectedCell.recheck.bytesRead)} · {inspectedCell.recheck.reads} 次</dd></div>
            </>}
            {inspectedCell.repair && <div><dt>慢扇区写回</dt><dd>{inspectedCell.repair.verified + inspectedCell.repair.stillSlow} 个写回校验通过 · {inspectedCell.repair.improved || 0} 个慢速改善 · {inspectedCell.repair.stillSlow} 个修复后仍慢 · {inspectedCell.repair.skipped} 个跳过 · {inspectedCell.repair.failed} 个失败 · {ACTIVE.has(job.status) ? "处理中" : "结果未确定"} {inspectedCell.repair.pending} 个</dd></div>}
            {cellBadRanges.length > 0 && (
              <div className="region-issue-details">
                <dt>异常明细</dt>
                <dd>
                  {cellBadRanges.slice(0, 20).map((range, index) => {
                    const location = scanRangeLocation(range, job.disk?.logicalSectorSize);
                    const address = location.startLba == null
                      ? `字节 ${formatInteger(range.offset)}–${formatInteger(range.offset + range.length - 1)}`
                      : `LBA ${formatInteger(location.startLba)}–${formatInteger(location.endLba)}`;
                    const skippedLocation = range.skippedRange ? scanRangeLocation(range.skippedRange, job.disk?.logicalSectorSize) : null;
                    const skippedAddress = skippedLocation ? (skippedLocation.startLba == null ? `字节 ${formatInteger(range.skippedRange.offset)}–${formatInteger(range.skippedRange.offset + range.skippedRange.length - 1)}` : `LBA ${formatInteger(skippedLocation.startLba)}–${formatInteger(skippedLocation.endLba)}`) : "";
                    const recovery = range.skippedRange ? "" : range.code === "RECOVERED_ERROR" && range.recovered === true ? " · 固件已纠正（保留告警）" : range.recovered === true ? " · 复读恢复（保留首次异常）"
                      : range.recovered === false ? " · 复读仍未完全读取" : "";
                    const labels = { SLOW_READ_LIMIT: "卡读跳过，未完成定位", LOCALIZATION_LIMIT: "定位达到限额", MEDIUM_ERROR: "设备报告介质错误", RECOVERED_ERROR: "固件报告已纠正错误", TRANSPORT_ERROR: "连接或传输错误", TIMEOUT: "读取超时", HARDWARE_ERROR: "设备硬件错误", CONTROLLER_ERROR: "控制器错误", NOT_READY: "设备未就绪", UNIT_ATTENTION: "设备状态变化", COMMAND_ABORTED: "读取命令中止", COMMAND_ERROR: "读取命令失败", ILLEGAL_REQUEST: "设备拒绝读取命令", DEVICE_ERROR: "设备报告错误", EIO: "I/O 读取错误（原因未细分）", READ_RETRY: "首次读取异常", SHORT_READ: "未完整读取" };
                    const evidence = range.evidence;
                    labels.WRITE_ERROR = "写入失败";
                    labels.READ_AFTER_WRITE_ERROR = "写入后读回失败";
                    labels.WRITE_VERIFY_MISMATCH = "读回数据与写入内容不一致";
                    labels.DEFERRED_ERROR = "设备延迟报告错误（不能归因于当前扇区）";
                    Object.assign(labels, { DATA_INTEGRITY_ERROR: "NVMe 数据完整性错误", UNWRITTEN_LBA: "未分配或未写入区域（不等于介质坏块）", ACCESS_DENIED: "设备拒绝访问" });
                    return <React.Fragment key={`${range.offset}-${range.length}-${index}`}>{index > 0 && <br />}{address} · {range.skippedRange ? "读取超时，已跳过" : labels[range.code] || range.code || labels.EIO} · {formatBinaryBytes(range.length)} · {location.label}{skippedAddress && ` · 跳过 ${skippedAddress}（${formatBinaryBytes(range.skippedRange.length)}）`}{recovery}{evidence?.protocol === "nvme" ? ` · NSID ${evidence.nsid}${evidence.nvmeStatus > 0 ? ` / SCT ${evidence.statusCodeType ?? 0} / SC 0x${Number(evidence.statusCode ?? 0).toString(16)}${evidence.doNotRetry ? " / DNR" : ""}` : " / 未取得设备完成状态"}` : evidence && ` · sense ${[evidence.senseKey, evidence.asc, evidence.ascq].map((value) => Number(value).toString(16).padStart(2, "0")).join("/")}`}{Number.isSafeInteger(evidence?.lba) && ` · 设备返回 LBA ${formatInteger(evidence.lba)}（定位线索）`}</React.Fragment>;
                  })}
                  {(cellBadRanges.length > 20 || stats.issuesTruncated) && <p className="scan-record-note">此处展示部分摘要，完整地址请打开异常明细或导出扫描报告。</p>}
                </dd>
              </div>
            )}
          </dl>
          {inspectedCell.errorCount > 0 && (
            <div className="inline-alert danger region-dialog-alert"><AlertTriangle size={17} /><span>{cellBadRanges.length ? "异常地址及定位精度见上方明细；卡读跳过或待定位范围不代表已确认坏扇区。" : outcomeSummary ? "该区域发生过读取异常，完整地址请查看异常明细或导出扫描报告。" : "旧记录未保留该区域的完整异常地址，不能据此确定坏区范围。"}</span></div>
          )}
        </div>
        <div className="dialog-actions"><button className="button secondary" onClick={closeDetail}>关闭</button></div>
      </dialog>
    </div>
  );
}

const LEGACY_SPEED_ROWS = [
  { key: "seq-q8t1", profileRow: "seq-high", pattern: "sequential", blockSize: 1048576, queueDepth: 8, threads: 1, code: "SEQ1M", subcode: "Q8T1", title: "SEQ1M Q8T1" },
  { key: "seq-q1t1", profileRow: "seq-low", pattern: "sequential", blockSize: 1048576, queueDepth: 1, threads: 1, code: "SEQ1M", subcode: "Q1T1", title: "SEQ1M Q1T1" },
  { key: "rnd-q32t1", profileRow: "rnd-high", pattern: "random", blockSize: 4096, queueDepth: 32, threads: 1, code: "RND4K", subcode: "Q32T1", title: "RND4K Q32T1" },
  { key: "rnd-q1t1", profileRow: "rnd-low", pattern: "random", blockSize: 4096, queueDepth: 1, threads: 1, code: "RND4K", subcode: "Q1T1", title: "RND4K Q1T1" }
];

function speedBlockCode(blockSize) {
  return ({ 4096: "4K", 65536: "64K", 1048576: "1M", 4194304: "4M" })[blockSize] || "1M";
}

function configuredSpeedRows(config, crystal = false) {
  const blockSize = [4096, 65536, 1048576, 4194304].includes(config.speedBlockSize) ? config.speedBlockSize : 1048576;
  const queueDepth = config.speedIOEngine === "psync" ? 1 : ([1, 8, 16, 32, 64, 128].includes(config.speedQueueDepth) ? config.speedQueueDepth : 8);
  const threads = [1, 2, 4, 8].includes(config.speedThreads) ? config.speedThreads : 1;
  const randomDepth = crystal && !config.speedQueueDepth ? 32 : queueDepth;
  const seqCode = `SEQ${speedBlockCode(blockSize)}`;
  return [
    { key: `seq-q${queueDepth}t${threads}`, profileRow: "seq-high", pattern: "sequential", blockSize, queueDepth, threads, code: seqCode, subcode: `Q${queueDepth}T${threads}`, title: `${seqCode} Q${queueDepth}T${threads}` },
    { key: `seq-q1t1-${blockSize}`, profileRow: "seq-low", pattern: "sequential", blockSize, queueDepth: 1, threads: 1, code: seqCode, subcode: "Q1T1", title: `${seqCode} Q1T1` },
    { key: `rnd-q${randomDepth}t${threads}-high`, profileRow: "rnd-high", pattern: "random", blockSize: 4096, queueDepth: randomDepth, threads, code: "RND4K", subcode: `Q${randomDepth}T${threads}`, title: `RND4K Q${randomDepth}T${threads}` },
    { key: "rnd-q1t1-base", profileRow: "rnd-low", pattern: "random", blockSize: 4096, queueDepth: 1, threads: 1, code: "RND4K", subcode: "Q1T1", title: "RND4K Q1T1" }
  ];
}

function speedRunOptions(config) {
  const { speedRuns, sampleMiB, speedIOEngine, speedDirectIO, speedTrim, speedBlockSize, speedQueueDepth, speedThreads, speedOperation } = config;
  return {
    type: "speed", speedPreset: CRYSTAL_SPEED_PRESET, speedRuns, sampleMiB, speedIOEngine, speedDirectIO, speedTrim, speedBlockSize,
    ...(speedIOEngine !== "psync" && speedQueueDepth ? { speedQueueDepth } : {}),
    ...(speedIOEngine !== "psync" ? { speedThreads } : {}),
    ...(speedOperation !== "auto" ? { speedOperation } : {})
  };
}

const SPEED_COLUMNS = [
  { operation: "read", label: "读取" },
  { operation: "write", label: "写入" },
  { operation: "mixed", label: "混合" }
];

function benchmarkDetail(benchmark, cached) {
  if (!Number.isFinite(benchmark.averageMBps)) return "";
  let iops = benchmark.iops;
  if (!Number.isFinite(iops) && Number.isSafeInteger(benchmark.blockSize) && benchmark.blockSize > 0 && Array.isArray(benchmark.samples)) {
    const totalBytes = benchmark.samples.reduce((sum, sample) => sum + (Number.isSafeInteger(sample?.bytes) ? sample.bytes : 0), 0);
    const totalSeconds = benchmark.samples.reduce((sum, sample) => sum + (Number.isFinite(sample?.seconds) ? sample.seconds : 0), 0);
    if (totalBytes > 0 && totalSeconds > 0) iops = totalBytes / benchmark.blockSize / totalSeconds;
  }
  return Number.isFinite(iops)
    ? `${formatInteger(iops)} IOPS${cached ? " · 缓存" : ""}`
    : cached ? "缓存" : "";
}

function SpeedBenchmark({ job, config, onStart, startDisabled }) {
  const completedResultValid = job.status !== "completed" || isCompleteSpeedResult(job);
  const finished = !ACTIVE.has(job.status) && job.status !== "idle";
  const idle = job.status === "idle";
  const options = idle ? config : job.options || {};
  const crystal = idle || isCrystalSpeedPreset(options.speedPreset);
  const configured = idle || options.speedPreset === CRYSTAL_SPEED_PRESET || (!crystal && options.speedProfile === true);
  const rows = configured ? configuredSpeedRows(options, crystal) : LEGACY_SPEED_ROWS;
  const speedRows = crystal && options.speedIOEngine === "psync" ? rows.filter(row => row.profileRow.endsWith("-low")) : rows;
  const columns = SPEED_COLUMNS;
  const cached = job.options?.speedDirectIO === false;
  const cells = speedBenchmarkCells(job, speedRows, columns.map(column => column.operation));
  const testingCell = cells.find((cell) => cell.active);
  const startBenchmark = (row) => onStart({ ...speedRunOptions(config), speedProfileRow: row.profileRow });

  return (
    <div className="benchmark-panel">
      <span className="sr-only" role="status">{testingCell ? `正在测试 ${testingCell.row.title} ${SPEED_COLUMNS.find((column) => column.operation === testingCell.operation).label}` : ""}</span>
      {job.status === "failed" && (
        <div className="inline-alert danger benchmark-failure" role="alert"><AlertTriangle size={18} aria-hidden="true" /><span>测速失败：{job.message || "任务异常结束，请重新测速。"}</span></div>
      )}
      {job.status === "completed" && Object.prototype.hasOwnProperty.call(job, "result") && !completedResultValid && (
        <div className="inline-alert warning" role="alert"><AlertTriangle size={18} /><span>历史测速明细格式不正确，无法显示部分或全部结果。</span></div>
      )}
      <div className="benchmark-matrix" style={{ "--benchmark-columns": columns.length, "--benchmark-rows": speedRows.length }} role="table" aria-label="磁盘测速结果">
        <div className="benchmark-matrix-header" role="row">
          <div className="benchmark-corner" role="columnheader">测试项目</div>
          {columns.map((column) => (
            <div className={`benchmark-column ${column.operation}`} role="columnheader" key={column.operation}>
              <strong>{column.label}</strong><small>MB/s</small>
            </div>
          ))}
        </div>
        {speedRows.map((row, rowIndex) => {
          return (
            <div className={`benchmark-matrix-row${rowIndex > 0 && speedRows[rowIndex - 1].pattern !== row.pattern ? " benchmark-group-start" : ""}`} role="row" key={row.key}>
              <div className="benchmark-test-cell" role="rowheader">
                <button
                  className="benchmark-test-label"
                  type="button"
                  onClick={() => startBenchmark(row)}
                  disabled={startDisabled || (config.speedIOEngine === "psync" && row.profileRow.endsWith("-high"))}
                  aria-label={`单独运行${row.title}测速`}
                  data-tooltip={`单独运行 ${row.pattern === "sequential" ? "顺序" : "随机"} ${formatBinaryBytes(row.blockSize)} 测速\n队列深度 ${row.queueDepth} · 并发任务 ${row.threads}\n${row.title}`}
                >
                  <span className="benchmark-test-name"><strong>{row.pattern === "sequential" ? "顺序" : "随机"}</strong><Play size={11} aria-hidden="true" /></span>
                  <span className="benchmark-test-size">{formatBinaryBytes(row.blockSize)}</span>
                  <small>Q{row.queueDepth} · T{row.threads}</small>
                </button>
              </div>
              {columns.map(({ operation, label }, columnIndex) => {
                  const { benchmark, live, active: testing, skipped: combinationSkipped } = cells[rowIndex * columns.length + columnIndex];
                  const write = operation === "write";
                  const liveValue = testing && Number.isFinite(live?.mbps) ? live.mbps : null;
                  const averageMBps = benchmark?.averageMBps;
                  const hasValue = Number.isFinite(averageMBps);
                  const status = testing ? "测试中" : (idle ? "尚未开始" : (finished ? (combinationSkipped ? "未执行" : "无结果") : "等待测试"));
                  const detail = hasValue ? benchmarkDetail(benchmark, cached) : "";
                  const formattedValue = hasValue ? averageMBps.toFixed(1) : liveValue !== null ? liveValue.toFixed(1) : "--";
                  const accessibleValue = hasValue || liveValue !== null
                    ? `${formattedValue} MB/s`
                    : status;
                  return (
                    <div
                      className={`benchmark-result-cell ${operation}${hasValue ? "" : " pending"}${testing ? " testing" : ""}${liveValue !== null ? " has-live-value" : ""}`}
                      role="cell"
                      key={operation}
                      aria-busy={testing || undefined}
                      aria-label={`${row.title}${operation === "read" ? "读取" : (write ? "写入" : "混合")}，${accessibleValue}`}
                    >
                      <span className="benchmark-cell-operation" aria-hidden="true">{label} · MB/s</span>
                      <strong className="benchmark-result-number" data-tooltip={hasValue ? `${formattedValue} MB/s` : undefined}>
                        {formattedValue}
                      </strong>
                      <div className="benchmark-result-meta">
                        {detail && <small className="benchmark-result-detail" data-tooltip={detail}>{detail}</small>}
                        {testing && live && <>
                          <span className="benchmark-cell-animation" aria-hidden="true" />
                          <progress className="sr-only" max={live.durationSeconds} value={live.elapsedSeconds || 0} aria-label="当前测试进度" />
                        </>}
                      </div>
                    </div>
                  );
                })}
            </div>
          );
        })}
      </div>
    </div>
  );
}

function ActiveTask({ job, contextDisk, detailState, onReloadDetail, speedConfig, onStartSpeed, speedStartDisabled, onOpenHistory, onRepairRecords, repairDisabledReason, onReturnToScan, returningRepairId, returnDisabled }) {
  if (!job) return null;
  const idle = job.status === "idle";
  const contextMismatch = job.type !== "speed" && !idle && contextDisk && !jobBelongsToDisk(job, contextDisk);
  const recordedRepair = Boolean(job.options?.scanRepairFrom);
  const badBlocks = scanBadBlockSummary(job.result?.map,Boolean(job.disk?.rotational));
  const scanResultComplete = (job.type !== "scan" && job.type !== "write") || job.status !== "completed" || (recordedRepair ? isCompleteRecordedRepairResult(job) : isCompleteScanResult(job));
  const speedResultComplete = job.type !== "speed" || job.status !== "completed" || isCompleteSpeedResult(job);
  const scanHasErrors = (job.type === "scan" || job.type === "write")
    && job.status === "completed"
    && scanResultComplete
    && (job.type === "scan" ? scanAbnormalCellCount(job.result?.map) : Number(job.result?.stats?.errorCells || 0)) > 0;
  const resultIncomplete = job.status === "completed" && job.result && (!scanResultComplete || !speedResultComplete);
  return (
    <section className="active-task" aria-label={job.type === "speed" ? "磁盘测速" : (job.type === "write" ? "深度检测" : "磁盘扫描")} data-tooltip={job.type !== "speed" && !idle ? `${job.disk?.model || "未知型号"} · ${job.device || "未知设备"}` : undefined}>
      {scanHasErrors && (
        <div className="completion-result has-errors task-result-alert" role="alert">
          <AlertTriangle size={20} />
          <span>{job.type === "write" ? `发现 ${job.result.stats.errorCells} 个写入或校验异常区域` : badBlocks.total ? `发现 ${badBlocks.total} 个坏块区域（慢速 ${badBlocks.slow} · 不可读 ${badBlocks.unreadable}），已不满足正常使用要求，建议更换硬盘` : `发现 ${scanAbnormalCellCount(job.result.map)} 个异常区域，严重慢读或设备异常需进一步确认`}</span>
        </div>
      )}
      {job.type === "scan" && ACTIVE.has(job.status) && job.phase === "读取超时，等待设备请求结束" && (
        <div className="inline-alert danger" role="alert"><AlertTriangle size={18} /><span>硬盘读取超时，正在等待设备返回；当前请求结束前无法继续读取后面的区域。</span></div>
      )}
      {detailState && ["loading", "retrying"].includes(detailState.status) && (
        <div className="inline-alert warning" role="status">
          <LoaderCircle className="spin" size={18} aria-hidden="true" />
          <span>{detailState.status === "retrying" ? detailState.error : "正在加载任务详细数据…"}</span>
        </div>
      )}
      {detailState?.status === "error" && (
        <div className="inline-alert danger detail-load-error" role="alert">
          <AlertTriangle size={18} aria-hidden="true" />
          <span>任务详情加载失败：{detailState.error}</span>
          <button className="button secondary" type="button" onClick={onReloadDetail}>重新加载详情</button>
        </div>
      )}
      {contextMismatch && <div className="inline-alert warning" role="status"><AlertTriangle size={18} /><span>当前显示的是 {job.disk?.model || "历史磁盘"}（{job.device}）的任务结果，不属于上方所选磁盘。</span></div>}
      {job.persistenceWarning && <div className="inline-alert warning" role="alert"><AlertTriangle size={18} /><span>{job.persistenceWarning}</span></div>}
      {recordedRepair ? <RecordedRepairPanel key={job.id} job={job} onReturnToScan={onReturnToScan} returning={returningRepairId === job.id} returnDisabled={returnDisabled} onOpenHistory={onOpenHistory} /> : (job.type === "scan" || job.type === "write") && <ScanBlockMap key={job.id} job={job} onOpenHistory={onOpenHistory} onRepairRecords={onRepairRecords} repairDisabledReason={repairDisabledReason} />}
      {job.type === "speed" && (
        <SpeedBenchmark
          job={job}
          config={speedConfig}
          onStart={onStartSpeed}
          startDisabled={speedStartDisabled}
        />
      )}
      {resultIncomplete && <div className="task-footer-slot">
        <div className="completion-result incomplete" role="alert">
          <AlertTriangle size={20} />
          <span>结果数据不完整，不能据此判断磁盘状态；请刷新详情或重新运行测试。</span>
        </div>
      </div>}
      {!recordedRepair && (job.type === "speed" || !safeScanMap(job.result?.map).length) && <HistoryTrigger onOpen={onOpenHistory} />}
    </section>
  );
}

function HistoryTrigger({ onOpen, inline = false }) {
  const button = (
    <button type="button" className={inline ? "scan-detail-button scan-history-button" : "history-open-button"} aria-haspopup="dialog" onClick={onOpen}>
      <History size={16} aria-hidden="true" />历史记录
    </button>
  );
  return inline ? button : <div className="history-access">{button}</div>;
}

function HistoryDialog({ disks, storageSpaces, mode, selectedId, deletedId, onSelect, onDelete, onClose, selectionLocked }) {
  const dialogRef = useRef(null);
  const [jobs, setJobs] = useState([]);
  const [loadState, setLoadState] = useState({ status: "loading", error: "" });
  const [reloadVersion, setReloadVersion] = useState(0);

  useLayoutEffect(() => {
    const dialog = dialogRef.current;
    dialog.showModal();
    return () => dialog.close();
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    setLoadState({ status: "loading", error: "" });
    api("/jobs?summary=1", { signal: controller.signal })
      .then(readJobList)
      .then((nextJobs) => {
        if (controller.signal.aborted) return;
        setJobs(nextJobs.filter((job) => mode === "speed" ? job.type === "speed" : ["scan", "write"].includes(job.type)));
        setLoadState({ status: "loaded", error: "" });
      })
      .catch((error) => {
        if (!controller.signal.aborted) setLoadState({ status: "error", error: errorMessage(error) });
      });
    return () => controller.abort();
  }, [mode, reloadVersion]);

  useEffect(() => {
    if (deletedId) setJobs((current) => current.filter((job) => job.id !== deletedId));
  }, [deletedId]);

  return (
    <dialog ref={dialogRef} className="history-dialog" aria-labelledby="history-dialog-title" onCancel={(event) => { event.preventDefault(); onClose(); }}>
      <div className="dialog-title">
        <History size={20} aria-hidden="true" />
        <div>
          <h2 id="history-dialog-title">历史记录</h2>
        </div>
        {loadState.status === "loaded" && <span className="history-count">{jobs.length} 条</span>}
        <button type="button" className="icon-button" aria-label="关闭历史记录" onClick={onClose} autoFocus><X size={20} /></button>
      </div>
      <div className="history-dialog-body" aria-busy={loadState.status === "loading"}>
        {loadState.status === "loading" ? (
          <div className="history-panel-empty" role="status"><LoaderCircle className="spin" size={20} aria-hidden="true" /><span>正在加载历史记录…</span></div>
        ) : loadState.status === "error" ? (
          <div className="history-load-error" role="alert">
            <p>历史记录加载失败：{loadState.error}</p>
            <button type="button" className="button secondary" onClick={() => setReloadVersion((current) => current + 1)}>重新加载</button>
          </div>
        ) : jobs.length === 0 ? (
          <div className="history-panel-empty">
            <History size={22} aria-hidden="true" />
            <span>完成检测后，记录会显示在这里。</span>
          </div>
        ) : (
          <ul className="history-list" aria-label="检测任务历史">
            {jobs.map((job) => {
              const speed = speedHistorySummary(job);
              const model = jobDiskModel(job, disks, storageSpaces);
              const note = job.message || job.phase || "";
              return (
              <li key={job.id}>
                <div className="history-record-row">
                  <button
                    className={`history-record${selectedId === job.id ? " active" : ""}`}
                    type="button"
                    aria-current={selectedId === job.id ? "true" : undefined}
                    onClick={() => { onSelect(job); onClose(); }}
                    disabled={selectionLocked}
                  >
                    <span className="history-record-header">
                      <strong>{job.options?.scanRepairFrom ? "按记录修复慢区" : job.type === "write" ? "深度检测（旧记录）" : (job.type === "scan" ? (job.options?.scanMode === "full" ? "完整扫描" : "快速扫描") : "速度测试")}</strong>
                      <StatusBadge status={job.status} />
                    </span>
                    <span className="history-record-disk">
                      <strong data-tooltip={job.device}>{job.storageSpace ? storageSpaceLabel(job.storageSpace) : job.device}</strong>
                      <small data-tooltip={model.tooltip}>{model.text}</small>
                    </span>
                    <span className={`history-record-result${speed ? " history-record-speed" : ""}`} data-tooltip={speed ? [speed.tooltip, note].filter(Boolean).join("\n") : undefined}>
                      {speed?.text || job.message || (job.type === "speed" ? (job.phase || "尚无测速结果") : "暂无结果摘要")}
                    </span>
                    <span className="history-record-time">{formatTime(job.startedAt || job.createdAt)}</span>
                  </button>
                  <button
                    className="history-delete-button"
                    type="button"
                    onClick={() => onDelete(job)}
                    disabled={ACTIVE.has(job.status)}
                    aria-label={`删除${job.device || "此任务"}的历史记录`}
                    data-tooltip={ACTIVE.has(job.status) ? "运行中的任务不能删除" : "删除历史记录"}
                  >
                    <Trash2 size={15} aria-hidden="true" />
                  </button>
                </div>
              </li>
              );
            })}
          </ul>
        )}
      </div>
      <div className="dialog-actions"><button type="button" className="button secondary" onClick={onClose}>关闭</button></div>
    </dialog>
  );
}

function DeleteHistoryDialog({ job, deleting, error, onClose, onConfirm }) {
  const dialogRef = useRef(null);

  useLayoutEffect(() => {
    const dialog = dialogRef.current;
    if (!job || !dialog) return;
    dialog.showModal();
    return () => dialog.close();
  }, [job]);

  if (!job) return null;
  return (
    <dialog
      ref={dialogRef}
      className="confirm-dialog"
      role="alertdialog"
      aria-labelledby="delete-history-title"
      onCancel={(event) => {
        event.preventDefault();
        if (!deleting) onClose();
      }}
    >
      <div className="dialog-title">
        <span className="dialog-icon dialog-icon-danger"><Trash2 size={21} aria-hidden="true" /></span>
        <div><p className="eyebrow">历史记录</p><h2 id="delete-history-title">删除这条记录？</h2></div>
        <button className="icon-button" type="button" onClick={onClose} disabled={deleting} aria-label="关闭" data-tooltip="关闭"><X size={20} /></button>
      </div>
      <div className="dialog-body delete-history-body">
        <p>将删除 {job.device || "所选设备"} 的这条检测记录，删除后无法恢复。磁盘中的数据不会受到影响。</p>
        {error && <div className="inline-alert danger" role="alert">删除失败：{error}</div>}
      </div>
      <div className="dialog-actions">
        <button className="button secondary" type="button" onClick={onClose} disabled={deleting}>取消</button>
        <button className="button danger" type="button" onClick={onConfirm} disabled={deleting}>
          {deleting ? <LoaderCircle className="spin" size={17} aria-hidden="true" /> : <Trash2 size={17} aria-hidden="true" />}
          {deleting ? "正在删除" : "删除记录"}
        </button>
      </div>
    </dialog>
  );
}

function ConfirmDialog({ request, onClose, onConfirm, submitting }) {
  const dialogRef = useRef(null);
  const [confirmed, setConfirmed] = useState(false);
  const [eraseConfirmation, setEraseConfirmation] = useState("");

  useEffect(() => {
    const dialog = dialogRef.current;
    if (request && dialog && !dialog.open) dialog.showModal();
    if (!request && dialog?.open) dialog.close();
    setConfirmed(false);
    setEraseConfirmation("");
  }, [request]);

  if (!request) return null;
  const disk = request.disk;
  const storageSpace = request.storageSpace;
  const scan = request.type === "scan";
  const speed = request.type === "speed";
  const rawSpeed = speed && request.speedTarget === "raw";
  const rawWillWrite = rawSpeed && request.speedOperation !== "read";
  const requestedSpeedRow = speed && configuredSpeedRows(request, true).find(row => row.profileRow === request.speedProfileRow);
  const repair = scan && request.scanRepair === true;
  const repairConfirmed = !repair || String(eraseConfirmation || "").trim().toUpperCase() === "REPAIR";
  const fullScan = scan && request.scanMode === "full";
  const taskLabel = scan
    ? (fullScan ? "全盘扫描" : "快速扫描")
    : (request.speedPattern === "sequential" ? "顺序测速" : (request.speedPattern === "random" ? "随机测速" : "一键测速"));
  const eraseConfirmed = !rawWillWrite || String(eraseConfirmation || "").trim().toUpperCase() === "ERASE";
  return (
    <dialog
      ref={dialogRef}
      className="confirm-dialog"
      role={rawWillWrite || repair ? "alertdialog" : "dialog"}
      aria-labelledby="confirm-dialog-title"
      onCancel={(event) => {
        if (submitting) {
          event.preventDefault();
          return;
        }
        onClose();
      }}
      onClose={onClose}
    >
      <div className="dialog-title">
        <span className={`dialog-icon${rawWillWrite || repair ? " dialog-icon-danger" : ""}`}><AlertTriangle size={22} /></span>
        <div><p className="eyebrow">{rawWillWrite ? "破坏性操作" : (repair ? "包含磁盘写入" : "确认任务")}</p><h2 id="confirm-dialog-title">{repair ? `${taskLabel}与慢扇区修复` : taskLabel}</h2></div>
        <button className="icon-button" onClick={onClose} disabled={submitting} aria-label="关闭" data-tooltip="关闭"><X size={20} /></button>
      </div>
      <div className="dialog-body">
        {!speed && !repair && disk.isSystem && <div className="inline-alert warning"><AlertTriangle size={19} /><span>所选设备包含系统挂载点。任务不会写盘，但会显著增加 I/O 负载。</span></div>}
        <dl className="confirm-specs">
          {speed && !rawSpeed ? <div><dt>测速目标</dt><dd>{storageSpaceLabel(storageSpace)}</dd></div> : <div><dt>设备</dt><dd>{disk.path}</dd></div>}
          {speed && !rawSpeed && storageSpace.isSystem && <div><dt>临时目录</dt><dd>{storageSpace.path} · 测试后自动清理</dd></div>}
          {speed && !rawSpeed ? <div><dt>结构</dt><dd>{storageLayoutLabel(storageSpace)}</dd></div> : <div><dt>型号</dt><dd>{disk.model}</dd></div>}
          {speed && !rawSpeed ? <div><dt>文件系统</dt><dd>{String(storageSpace.fileSystem || "-").toUpperCase()}</dd></div> : <div><dt>序列号</dt><dd>{disk.serial || "未提供"}</dd></div>}
          {speed && !rawSpeed && <div><dt>成员盘</dt><dd>{storageSpace.members.length ? storageSpace.members.map((member) => member.replace(/^\/dev\//, "")).join(" + ") : "未知"}</dd></div>}
          {rawSpeed && <div><dt>方式</dt><dd>直接访问原始块设备</dd></div>}
          <div><dt>容量</dt><dd>{formatBytes(speed && !rawSpeed ? storageSpace.size : disk.size)}</dd></div>
          {scan && <div><dt>扫描覆盖</dt><dd>{fullScan ? "顺序读取完整磁盘" : `${QUICK_SCAN_CELL_COUNT} 个区域各抽读 ${formatBinaryBytes(QUICK_SAMPLE_BYTES)}`}</dd></div>}
          {scan && <div><dt>错误定位</dt><dd>{request.scanExactSectors ? `逻辑扇区（${formatBinaryBytes(disk.logicalSectorSize || 512)}）` : "64 KiB 区段"}</dd></div>}
          {scan && <div><dt>定位预算</dt><dd>{request.scanDeepLocate ? "深入：最多 4096 次／2 分钟" : "标准：最多 256 次／15 秒"}；达到限额后记录待细查范围并继续</dd></div>}
          {scan && request.scanSelfTest && <div><dt>固件自检</dt><dd>先运行 NVMe 扩展自检，再扫描；不支持时会提示停止</dd></div>}
          {repair && <div><dt>慢扇区修复</dt><dd>单次读取超过 {request.scanRepairAfterMs} ms 后，备份并写回同一范围的原始数据，再读回校验</dd></div>}
          {scan && <div><dt>结果形式</dt><dd>{fullScan ? SCAN_CELL_COUNT : QUICK_SCAN_CELL_COUNT} 格响应时间块图</dd></div>}
          {speed && <div><dt>测试组合</dt><dd>{({ read: "仅读取", write: "仅写入", mixed: "混合 50/50" })[request.speedOperation] || "读取、写入和混合"} · {requestedSpeedRow?.title || "全部测试项目"}</dd></div>}
          {speed && <div><dt>测试次数</dt><dd>{request.speedRuns || CRYSTAL_SPEED_RUNS} 次</dd></div>}
          {speed && <div><dt>测试范围</dt><dd>{rawSpeed && isCrystalSpeedPreset(request.speedPreset) ? "从磁盘起始位置起 " : ""}{formatBinaryBytes((request.sampleMiB || 1024) * 1024 * 1024)}</dd></div>}
          {speed && <div><dt>I/O 引擎</dt><dd>{request.speedIOEngine || "io_uring"}{request.speedIOEngine === "psync" ? " · 仅 Q1 项目" : ""}</dd></div>}
          {speed && <div><dt>缓存策略</dt><dd>{request.speedDirectIO === false ? "使用系统缓存" : "Direct I/O"}{request.speedTrim && !rawSpeed ? " · 测速前 TRIM" : ""}</dd></div>}
        </dl>
        {rawWillWrite && (
          <div className="erase-confirm-field">
            <div className="inline-alert danger" role="alert">
              <AlertTriangle size={19} aria-hidden="true" />
              <span>所选硬盘没有可用文件系统，测速将直接访问原始块设备。写入及混合项目会<strong>覆写 {disk.path}（{disk.model}）上被测区域的原有数据</strong>，造成数据损坏且无法恢复。只允许对无需保留数据的空盘执行。</span>
            </div>
            <label>
              <span>输入 <strong>ERASE</strong> 以确认擦除</span>
              <input
                type="text"
                value={eraseConfirmation}
                onChange={(event) => setEraseConfirmation(event.target.value)}
                placeholder="ERASE"
                autoComplete="off"
                spellCheck="false"
              />
            </label>
          </div>
        )}
        {repair && <div className="erase-confirm-field">
          <div className="inline-alert danger" role="alert"><AlertTriangle size={19} /><span>将对 {disk.path}（{disk.model}）进行实际写入。虽然写回原数据，断电、设备故障或写入失败仍可能造成数据损坏；请先完成独立备份。读不完整的范围会跳过，不能保证修复物理损伤。</span></div>
          <label><span>输入 <strong>REPAIR</strong> 以确认写回风险</span><input type="text" value={eraseConfirmation} onChange={(event) => setEraseConfirmation(event.target.value)} placeholder="REPAIR" autoComplete="off" spellCheck="false" /></label>
        </div>}
        <label className="confirm-check"><input type="checkbox" checked={confirmed} onChange={(event) => setConfirmed(event.target.checked)} /><span>{repair ? "我已核对磁盘、完成独立备份，并了解此次扫描包含实际写入。" : (rawSpeed ? (rawWillWrite ? `我已确认 ${disk.path} 没有需要保留的数据，并了解底层测速会覆写原有数据。` : `我已核对 ${disk.path}，了解这次仅做底层读取测速。`) : (speed ? `我已核对${storageSpaceLabel(storageSpace)}，了解测速会创建并自动删除临时测试文件。` : "我已核对设备，了解测试期间磁盘性能会受到影响。"))}</span></label>
      </div>
      <div className="dialog-actions">
        <button className="button secondary" onClick={onClose} disabled={submitting}>取消</button>
        <button
          className={rawWillWrite || repair ? "button danger" : "button primary"}
          disabled={!confirmed || !eraseConfirmed || !repairConfirmed || submitting}
          aria-busy={submitting ? "true" : undefined}
          onClick={() => onConfirm({
            type: request.type,
            ...(request.resumeFrom ? { resumeFrom: request.resumeFrom } : {}),
            ...(scan
              ? { scanMode: request.scanMode, scanExactSectors: request.scanExactSectors === true, scanDeepLocate: request.scanDeepLocate === true, scanSelfTest: request.scanSelfTest === true, scanRepair: repair, ...(repair ? { scanRepairAfterMs: request.scanRepairAfterMs, scanRepairAcknowledged: true } : {}) }
              : {
                    sampleMiB: request.sampleMiB || 1024,
                    speedRuns: request.speedRuns || CRYSTAL_SPEED_RUNS,
                    ...(request.speedPreset ? { speedPreset: request.speedPreset } : {}),
                    ...(request.speedPosition ? { speedPosition: request.speedPosition } : {}),
                    ...(request.speedOperation ? { speedOperation: request.speedOperation } : {}),
                    ...(request.speedPattern ? { speedPattern: request.speedPattern } : {}),
                    ...(request.speedBlockSize ? { speedBlockSize: request.speedBlockSize } : {}),
                    ...(request.speedQueueDepth ? { speedQueueDepth: request.speedQueueDepth } : {}),
                    ...(request.speedThreads ? { speedThreads: request.speedThreads } : {}),
                    ...(request.speedIOEngine ? { speedIOEngine: request.speedIOEngine } : {}),
                    ...(typeof request.speedDirectIO === "boolean" ? { speedDirectIO: request.speedDirectIO } : {}),
                    ...(typeof request.speedTrim === "boolean" ? { speedTrim: request.speedTrim } : {}),
                    ...(typeof request.speedProfile === "boolean" ? { speedProfile: request.speedProfile } : {}),
                    ...(request.speedProfileRow ? { speedProfileRow: request.speedProfileRow } : {}),
                    speedTarget: rawSpeed ? "raw" : "filesystem",
                    ...(rawWillWrite ? { rawWriteAcknowledged: true } : {})
                }),
            device: speed && !rawSpeed ? storageSpace.path : disk.path,
            ...(speed && !rawSpeed ? { expectedStorageSpace: request.expectedStorageSpace } : { expectedDisk: request.expectedDisk })
          })}
        >{submitting ? <><LoaderCircle className="spin" size={18} aria-hidden="true" />正在创建任务</> : `${request.resumeFrom ? "继续" : "开始"}${taskLabel}`}</button>
      </div>
    </dialog>
  );
}

function ResumeScanDialog({ request, onClose, onContinue, onRestart, submitting, error }) {
  const dialogRef = useRef(null);
  useEffect(() => {
    const dialog = dialogRef.current;
    const returnFocus = document.querySelector(".task-actions .cta");
    if (dialog && !dialog.open) dialog.showModal();
    return () => {
      if (dialog?.open) dialog.close();
      if (returnFocus?.isConnected && !returnFocus.disabled) returnFocus.focus();
    };
  }, []);
  const { job, disk } = request;
  const resume = job.resume;
  return <dialog ref={dialogRef} className="confirm-dialog resume-dialog" aria-labelledby="resume-dialog-title" aria-describedby="resume-dialog-note"
    onCancel={(event) => { if (submitting) event.preventDefault(); else onClose(); }} onClose={onClose}>
    <div className="dialog-title">
      <span className="dialog-icon"><ScanSearch size={22} /></span>
      <div><p className="eyebrow">发现未完成的扫描</p><h2 id="resume-dialog-title">从上次位置继续？</h2></div>
      <button className="icon-button" onClick={onClose} disabled={submitting} aria-label="关闭"><X size={20} /></button>
    </div>
    <div className="dialog-body">
      <dl className="confirm-specs">
        <div><dt>硬盘</dt><dd>{disk.model}</dd></div>
        <div><dt>设备</dt><dd>{disk.path}</dd></div>
        <div><dt>扫描方式</dt><dd>{job.options.scanMode === "full" ? "全盘扫描" : "快速扫描"}</dd></div>
        <div><dt>上次进度</dt><dd>{resume.cellIndex} / {resume.cellCount} 个区域 · {resume.progress}%</dd></div>
        <div><dt>中断时间</dt><dd>{formatTime(job.finishedAt || job.createdAt)}</dd></div>
        <div><dt>继续位置</dt><dd>区域 {resume.cellIndex + 1} · {formatBytes(resume.offset)}</dd></div>
      </dl>
      <p id="resume-dialog-note" className="resume-note">保留已完成区域的结果，从未完成区域继续；中断时未扫完的当前区域会重新检测。</p>
      {job.options.scanRepair && <p className="resume-note">上次包含慢扇区修复，继续前需要重新确认写回风险。</p>}
      {error && <div className="inline-alert danger" role="alert">{error}</div>}
    </div>
    <div className="dialog-actions resume-actions">
      <button className="button secondary" onClick={onClose} disabled={submitting}>暂不扫描</button>
      <button className="button secondary" onClick={onRestart} disabled={submitting}>从头扫描</button>
      <button className="button primary" onClick={onContinue} disabled={submitting} aria-busy={submitting || undefined}>
        {submitting ? <><LoaderCircle className="spin" size={18} />正在继续</> : "继续扫描"}
      </button>
    </div>
  </dialog>;
}

function RunningTasks({ disks, storageSpaces, jobs, selectedId, onSelect, onCancel, cancellingIds, locked }) {
  if (!jobs.length) return null;
  return <section className="running-tasks" aria-label="运行中的任务">
    <div className="running-tasks-heading">运行中 <span>{jobs.length}</span></div>
    <div className="running-task-list">
      {jobs.map(job => {
        const stopping = job.status === "cancelling" || cancellingIds.has(job.id);
        const label = job.type === "speed" ? "测速" : job.options?.scanRepairFrom ? "修复" : "扫描";
        const model = jobDiskModel(job, disks, storageSpaces).text;
        const progress = Math.max(0, Math.min(100, Number(job.progress) || 0));
        return <div className={`running-task${selectedId === job.id ? " selected" : ""}`} key={job.id}>
          <button type="button" className="running-task-view" disabled={locked} aria-pressed={selectedId === job.id} aria-label={`查看 ${model}（${job.device}）${label}`} data-tooltip={`${model} · ${job.device}\n${job.phase || label}`} onClick={() => onSelect(job)}>
            <span className="running-task-name"><span className="running-task-model">{model}</span><small>{stopping ? "正在停止" : label}</small></span>
            <span className="running-task-progress"><progress aria-label={`${model}（${job.device}）${label}进度`} max="100" value={progress} /><span>{progress.toFixed(1)}%</span></span>
          </button>
          <button type="button" className="running-task-stop" aria-label={`停止 ${model}（${job.device}）的任务`} disabled={stopping || locked} data-tooltip={stopping ? "正在等待设备请求结束" : `停止 ${model}（${job.device}）的任务`} onClick={() => onCancel(job.id)}>
            {stopping ? <LoaderCircle className="spin" size={16} aria-hidden="true" /> : <CircleStop size={16} aria-hidden="true" />}
          </button>
        </div>;
      })}
    </div>
  </section>;
}

function App() {
  useLayoutEffect(() => {
    const root = document.documentElement;
    const pointer = () => { root.dataset.inputModality = "pointer"; };
    const keyboard = (event) => {
      if (event.altKey || event.ctrlKey || event.metaKey || ["Shift", "Control", "Alt", "Meta"].includes(event.key)) return;
      root.dataset.inputModality = "keyboard";
    };
    pointer();
    document.addEventListener("pointerdown", pointer, true);
    document.addEventListener("keydown", keyboard, true);
    return () => {
      document.removeEventListener("pointerdown", pointer, true);
      document.removeEventListener("keydown", keyboard, true);
      delete root.dataset.inputModality;
    };
  }, []);

  const [theme, setTheme] = useState(() => document.documentElement.dataset.theme === "dark" ? "dark" : "light");
  const themeSyncRef = useRef(null);
  const [disks, setDisks] = useState([]);
  const [storageSpaces, setStorageSpaces] = useState([]);
  const [storageWarning, setStorageWarning] = useState("");
  const [selectedPath, setSelectedPath] = useState("");
  const [selectedStoragePath, setSelectedStoragePath] = useState("");
  const [selectedSpeedKey, setSelectedSpeedKey] = useState("");
  const [jobs, setJobs] = useState([]);
  const [selectedJob, setSelectedJob] = useState(null);
  const [recordedRepairRequest, setRecordedRepairRequest] = useState(null);
  const [returningRepairId, setReturningRepairId] = useState("");
  const [health, setHealth] = useState(null);
  const [mode, setMode] = useState("scan");
  const [scanMode, setScanMode] = useState("quick");
  const [scanExactSectors, setScanExactSectors] = useState(true);
  const [scanDeepLocate, setScanDeepLocate] = useState(false);
  const [scanSelfTest, setScanSelfTest] = useState(false);
  const [scanRepair, setScanRepair] = useState(false);
  const [scanRepairAfterMs, setScanRepairAfterMs] = useState(500);
  const [speedConfig, setSpeedConfig] = useState({
    speedRuns: CRYSTAL_SPEED_RUNS,
    sampleMiB: 1024,
    speedOperation: "auto",
    speedBlockSize: 1048576,
    speedQueueDepth: 0,
    speedThreads: 1,
    speedIOEngine: "io_uring",
    speedDirectIO: true,
    speedTrim: false
  });
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [resourceError, setResourceError] = useState("");
  const [jobsError, setJobsError] = useState("");
  const [actionError, setActionError] = useState("");
  const [detailState, setDetailState] = useState({ jobId: "", status: "idle", error: "" });
  const [detailReloadVersion, setDetailReloadVersion] = useState(0);
  const [confirmRequest, setConfirmRequest] = useState(null);
  const [resumeRequest, setResumeRequest] = useState(null);
  const [checkingResume, setCheckingResume] = useState(false);
  const [creatingJob, setCreatingJob] = useState(false);
  const [cancellingJobIds, setCancellingJobIds] = useState(new Set());
  const [deleteRequest, setDeleteRequest] = useState(null);
  const [historyOpen, setHistoryOpen] = useState(false);
  const [deletingJobId, setDeletingJobId] = useState("");
  const [deletedHistoryId, setDeletedHistoryId] = useState("");
  const [deleteError, setDeleteError] = useState("");

  useEffect(() => {
    const sync = createThemeSync({ onTheme: setTheme });
    themeSyncRef.current = sync;
    return () => {
      sync.stop();
      themeSyncRef.current = null;
    };
  }, []);

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    document.documentElement.style.colorScheme = theme;
    const themeColor = document.querySelector('meta[name="theme-color"]');
    if (themeColor) themeColor.setAttribute("content", theme === "dark" ? "#17191d" : "#e4e8ed");
  }, [theme]);

  const toggleTheme = () => {
    themeSyncRef.current?.toggle();
  };
  const mountedRef = useRef(false);
  const refreshGenerationRef = useRef(0);
  const refreshControllerRef = useRef(null);
  const refreshPromiseRef = useRef(null);
  const jobsPollGenerationRef = useRef(0);
  const jobsPollControllerRef = useRef(null);
  const jobsVersionRef = useRef(0);
  const jobsRef = useRef([]);
  const createPendingRef = useRef(false);
  const cancelPendingRef = useRef(new Set());
  const selectedDiskRef = useRef(null);
  const initialSelectionPendingRef = useRef(true);
  const selectedStorageRef = useRef(null);
  const resumeLookupRef = useRef(null);
  const returnToScanRef = useRef(null);

  const selectedDisk = useMemo(() => disks.find((disk) => disk.path === selectedPath) || disks[0] || null, [disks, selectedPath]);
  const selectedStorageSpace = useMemo(() => (
    storageSpaces.find((space) => space.path === selectedStoragePath) || storageSpaces[0] || null
  ), [selectedStoragePath, storageSpaces]);
  const speedTargets = useMemo(() => buildSpeedTargets(disks, storageSpaces), [disks, storageSpaces]);
  const activeJobs = useMemo(() => jobs.filter(job => ACTIVE.has(job.status)), [jobs]);
  const activeSpeedJob = mode === "speed" ? activeJobs.find(job => job.type === "speed") : null;
  const selectedSpeedTarget = useMemo(() => {
    if (activeSpeedJob) return speedTargets.find((target) => jobBelongsToSpeedTarget(activeSpeedJob, target)) || null;
    return speedTargets.find((target) => target.key === selectedSpeedKey && !target.disabledReason)
      || speedTargets.find((target) => !target.disabledReason)
      || null;
  }, [activeSpeedJob, selectedSpeedKey, speedTargets]);
  const speedTargetModels = [...new Set((selectedSpeedTarget?.type === "raw"
    ? [selectedSpeedTarget.disk]
    : selectedSpeedTarget?.memberDisks || [])
    .map(disk => disk?.model?.trim()).filter(Boolean))].join(" + ") || "未知硬盘型号";
  const resultDeviceLabel = mode === "speed"
    ? (!selectedSpeedTarget && activeSpeedJob
      ? jobDiskModel(activeSpeedJob, disks, storageSpaces).text
      : [isFnOsStorageSpacePath(selectedSpeedTarget?.path) ? storageSpaceLabel(selectedSpeedTarget.storageSpace) : "", speedTargetModels].filter(Boolean).join(" · "))
    : selectedDisk?.model || "未知硬盘型号";
  const activeJob = activeSpeedJob || activeJobForTarget(jobs, mode, selectedDisk, selectedSpeedTarget);
  const startBlockedReason = activeJob ? "" : taskStartBlockedReason(jobs, mode, selectedDisk, selectedSpeedTarget);
  const diskJobs = useMemo(() => (
    selectedDisk ? jobs.filter((job) => jobBelongsToDisk(job, selectedDisk)) : []
  ), [jobs, selectedDisk]);

  useEffect(() => {
    resumeLookupRef.current?.abort();
    resumeLookupRef.current = null;
    setCheckingResume(false);
    setResumeRequest(null);
    return () => resumeLookupRef.current?.abort();
  }, [mode, selectedDisk?.path, selectedDisk?.serial, selectedDisk?.size]);
  const resolvedSelectedJob = useMemo(() => (
    selectedJob ? jobs.find((job) => job.id === selectedJob.id) || null : null
  ), [jobs, selectedJob]);
  const pendingMapJob = useMemo(() => selectedDisk ? ({
    id: `pending-${selectedDisk.path}-${scanMode}`,
    type: "scan",
    device: selectedDisk.path,
    status: "idle",
    phase: scanMode === "full" ? "等待完整扫描" : "等待快速扫描",
    progress: 0,
    disk: selectedDisk,
    options: { scanMode, scanExactSectors },
    result: {
      latencyBasis: selectedDisk.rotational ? "hdd-dual-v4" : "solid-dual-v4",
      coverage: scanMode === "full" ? "full" : "sampled",
      ...(scanMode === "quick" ? { sampleBytes: QUICK_SAMPLE_BYTES } : {}),
      map: createPendingMap(selectedDisk.size, scanMode === "quick" ? QUICK_SCAN_CELL_COUNT : SCAN_CELL_COUNT),
      stats: { testedCells: 0, errorCells: 0, slowCells: 0, bytesRead: 0 }
    }
  }) : null, [selectedDisk, scanMode, scanExactSectors]);
  const pendingSpeedJob = useMemo(() => selectedSpeedTarget ? ({
    id: `pending-speed-${selectedSpeedTarget.key}`,
    type: "speed",
    device: selectedSpeedTarget.path,
    status: "idle",
    phase: "等待测速",
    progress: 0,
    disk: selectedSpeedTarget.type === "raw"
      ? selectedSpeedTarget.disk
      : { name: selectedSpeedTarget.path.replace(/^\//, ""), model: String(selectedSpeedTarget.storageSpace.fileSystem || "").toUpperCase(), size: selectedSpeedTarget.storageSpace.size },
    ...(selectedSpeedTarget.storageSpace ? { storageSpace: selectedSpeedTarget.storageSpace } : {}),
    result: { benchmarks: [] },
    options: { speedPosition: "all" }
  }) : null, [selectedSpeedTarget]);
  // The large result area still follows the selected disk/volume so a target
  // switch does not unexpectedly display another disk's benchmark map.
  const targetModeJobs = useMemo(() => mode === "speed"
    ? jobs.filter((job) => jobBelongsToSpeedTarget(job, selectedSpeedTarget))
    : diskJobs.filter((job) => job.type === "scan" || job.type === "write"), [diskJobs, jobs, mode, selectedSpeedTarget]);
  const shownJob = chooseShownJob({
    activeJob,
    selectedJob: resolvedSelectedJob,
    modeJobs: targetModeJobs,
    fallbackJobs: !(mode === "speed" ? selectedSpeedTarget : selectedDisk)
      ? jobs.filter((job) => (mode === "scan" ? (job.type === "scan" || job.type === "write") : job.type === mode))
      : [],
    pendingJob: mode === "scan" ? pendingMapJob : pendingSpeedJob,
    mode
  });
  const controlsLocked = Boolean(activeJob) || creatingJob || checkingResume || Boolean(startBlockedReason);
  useEffect(() => () => returnToScanRef.current?.abort(), [mode, shownJob?.id]);
  const recordedRepairBlockedReason = health?.capabilities?.recordedRepair !== true ? "当前后台不支持按记录修复，请更新并重启应用"
    : controlsLocked ? "请先等待当前任务结束"
    : !jobBelongsToDisk(shownJob, selectedDisk) ? "扫描记录与当前所选硬盘不匹配"
    : scanRepairDiskBlockedReason(selectedDisk) || (!recordedRepairCandidates(shownJob).length ? "记录中没有可修复的准确慢读地址，请重新扫描定位" : "");

  useEffect(() => {
    selectedDiskRef.current = selectedDisk;
  }, [selectedDisk]);

  useEffect(() => {
    jobsRef.current = jobs;
  }, [jobs]);

  useEffect(() => {
    selectedStorageRef.current = selectedStorageSpace;
  }, [selectedStorageSpace]);

  useEffect(() => {
    setSpeedConfig(current => current.speedTrim ? { ...current, speedTrim: false } : current);
  }, [selectedSpeedTarget?.key]);

  useEffect(() => {
    if (mode !== "speed" || loading) return;
    if (!selectedSpeedTarget) return;
    if (selectedSpeedKey !== selectedSpeedTarget.key) setSelectedSpeedKey(selectedSpeedTarget.key);
    const memberDisk = selectedSpeedTarget.disk
      || disks.find((disk) => storageSpacesForDisk([selectedSpeedTarget.storageSpace], disk).length > 0)
      || null;
    if (memberDisk && selectedPath !== memberDisk.path) setSelectedPath(memberDisk.path);
  }, [disks, loading, mode, selectedPath, selectedSpeedKey, selectedSpeedTarget]);

  useEffect(() => {
    if (!activeJob) return;
    if (activeJob.type === "write") {
      setScanMode("full");
      setScanRepair(false);
      setScanSelfTest(false);
    }
    if (activeJob.type === "scan" && ["quick", "full"].includes(activeJob.options?.scanMode)) {
      setScanMode(activeJob.options.scanMode);
      setScanExactSectors(activeJob.options?.scanExactSectors !== false);
      setScanDeepLocate(activeJob.options?.scanDeepLocate === true);
      setScanSelfTest(activeJob.options?.scanSelfTest === true);
      setScanRepair(activeJob.options?.scanRepair === true);
      setScanRepairAfterMs(activeJob.options?.scanRepairAfterMs || 500);
    }
  }, [activeJob?.id, activeJob?.type, activeJob?.options?.scanMode, activeJob?.options?.scanExactSectors, activeJob?.options?.scanDeepLocate, activeJob?.options?.scanSelfTest, activeJob?.options?.scanRepair, activeJob?.options?.scanRepairAfterMs]);

  const invalidateRefresh = useCallback(() => {
    refreshGenerationRef.current += 1;
    refreshControllerRef.current?.abort();
    refreshControllerRef.current = null;
    refreshPromiseRef.current = null;
    jobsPollGenerationRef.current += 1;
    jobsPollControllerRef.current?.abort();
    jobsPollControllerRef.current = null;
    if (mountedRef.current) setRefreshing(false);
  }, []);

  const refresh = useCallback((quiet = false) => {
    if (quiet && refreshPromiseRef.current) return refreshPromiseRef.current;

    const generation = refreshGenerationRef.current + 1;
    refreshGenerationRef.current = generation;
    refreshControllerRef.current?.abort();
    jobsPollGenerationRef.current += 1;
    jobsPollControllerRef.current?.abort();
    jobsPollControllerRef.current = null;
    const controller = new AbortController();
    const jobsVersion = jobsVersionRef.current;
    refreshControllerRef.current = controller;
    if (!quiet && mountedRef.current) setRefreshing(true);

    const request = Promise.allSettled([
      api("/disks", { signal: controller.signal }).then(readStorageInventory),
      api("/jobs?summary=1", { signal: controller.signal }).then(readJobList),
      api("/health", { signal: controller.signal }).then(readHealth)
    ]).then(([diskResult, jobResult, healthResult]) => {
      if (!mountedRef.current || controller.signal.aborted || generation !== refreshGenerationRef.current) return;

      const resourceFailures = [];
      if (diskResult.status === "fulfilled") {
        const { disks: nextDisks, storageSpaces: nextSpaces, storageWarning: nextStorageWarning } = diskResult.value;
        setStorageWarning(nextStorageWarning);
        const previousDisk = selectedDiskRef.current;
        const continuedDisk = previousDisk
          ? findCurrentDiskForJob(nextDisks, { device: previousDisk.path, disk: previousDisk })
          : null;
        let restoredJob = null;
        if (initialSelectionPendingRef.current && jobResult.status === "fulfilled" && jobsVersion === jobsVersionRef.current) {
          initialSelectionPendingRef.current = false;
          const nextSpeedTargets = buildSpeedTargets(nextDisks, nextSpaces);
          restoredJob = jobResult.value.find(job => ACTIVE.has(job.status)
            && (findCurrentDiskForJob(nextDisks, job) || nextSpeedTargets.some(target => jobBelongsToSpeedTarget(job, target))))
            || findLatestScanJob(jobResult.value, nextDisks);
          if (restoredJob) {
            setSelectedJob(restoredJob);
            setMode(restoredJob.type === "speed" ? "speed" : "scan");
            const restoredTarget = restoredJob.type === "speed" && nextSpeedTargets.find(target => jobBelongsToSpeedTarget(restoredJob, target));
            if (restoredTarget) setSelectedSpeedKey(restoredTarget.key);
            setScanMode(restoredJob.type === "write" || restoredJob.options?.scanMode === "full" ? "full" : "quick");
            setScanExactSectors(restoredJob.options?.scanExactSectors !== false);
            setScanDeepLocate(restoredJob.options?.scanDeepLocate === true);
          }
        }
        const nextSelectedDisk = findCurrentDiskForJob(nextDisks, restoredJob) || continuedDisk || nextDisks[0] || null;
        setDisks(nextDisks);
        selectedDiskRef.current = nextSelectedDisk;
        setSelectedPath(nextSelectedDisk?.path || "");
        const previousSpace = selectedStorageRef.current;
        const nextSelectedSpace = (previousSpace && nextSpaces.find((space) => space.path === previousSpace.path)) || nextSpaces[0] || null;
        setStorageSpaces(nextSpaces);
        selectedStorageRef.current = nextSelectedSpace;
        setSelectedStoragePath(nextSelectedSpace?.path || "");
      } else {
        resourceFailures.push(`读取磁盘失败：${errorMessage(diskResult.reason)}`);
      }

      if (jobResult.status === "fulfilled") {
        if (jobsVersion === jobsVersionRef.current) {
          setJobs((current) => mergeJobLists(current, jobResult.value));
        }
        setJobsError("");
      } else {
        setJobsError(`读取任务失败：${errorMessage(jobResult.reason)}`);
      }

      if (healthResult.status === "fulfilled") {
        setHealth(healthResult.value);
      } else {
        setHealth(null);
        resourceFailures.push(`检测服务失败：${errorMessage(healthResult.reason)}`);
      }
      setResourceError(resourceFailures.join("；"));
    }).finally(() => {
      if (generation !== refreshGenerationRef.current) return;
      if (refreshControllerRef.current === controller) refreshControllerRef.current = null;
      if (refreshPromiseRef.current === request) refreshPromiseRef.current = null;
      if (mountedRef.current) {
        setLoading(false);
        setRefreshing(false);
      }
    });

    refreshPromiseRef.current = request;
    return request;
  }, []);

  const refreshJobs = useCallback(async () => {
    if (refreshPromiseRef.current) return refreshPromiseRef.current;
    const generation = jobsPollGenerationRef.current + 1;
    jobsPollGenerationRef.current = generation;
    jobsPollControllerRef.current?.abort();
    const controller = new AbortController();
    const jobsVersion = jobsVersionRef.current;
    jobsPollControllerRef.current = controller;
    try {
      const nextJobs = readJobList(await api(currentJobListPath(jobsRef.current), { signal: controller.signal }));
      if (!mountedRef.current
        || controller.signal.aborted
        || generation !== jobsPollGenerationRef.current
        || jobsVersion !== jobsVersionRef.current) return;
      setJobs((current) => mergeJobLists(current, nextJobs));
      setJobsError("");
    } catch (error) {
      if (!mountedRef.current || controller.signal.aborted || generation !== jobsPollGenerationRef.current) return;
      setJobsError(`读取任务失败：${errorMessage(error)}`);
    } finally {
      if (jobsPollControllerRef.current === controller) jobsPollControllerRef.current = null;
    }
  }, []);

  useEffect(() => {
    mountedRef.current = true;
    refresh();
    return () => {
      mountedRef.current = false;
      refreshGenerationRef.current += 1;
      refreshControllerRef.current?.abort();
      refreshControllerRef.current = null;
      refreshPromiseRef.current = null;
      jobsPollGenerationRef.current += 1;
      jobsPollControllerRef.current?.abort();
      jobsPollControllerRef.current = null;
    };
  }, [refresh]);

  const activeScan = Boolean(activeJob && !activeJob.options?.scanRepairFrom && (activeJob.type === "scan" || activeJob.type === "write"));
  const hasActiveScan = activeJobs.some(job => ["scan", "write"].includes(job.type));
  // Keep a low-frequency full-job sync alongside incremental cell updates.
  // This also supports older packaged backends that do not expose scan-progress.
  const pollInterval = activeJobs.some(job => job.type === "speed") ? 300 : (hasActiveScan ? ACTIVE_SCAN_FALLBACK_POLL_MS : 6000);

  useEffect(() => {
    if (!activeScan || !activeJob) return undefined;
    const jobId = activeJob.id;
    const currentMap = safeScanMap(activeJob.result?.map);
    let after = 0;
    while (after < currentMap.length && !["pending", "reading"].includes(currentMap[after].status)) after += 1;
    let stopped = false;
    let timer = null;
    let controller = null;

    const update = async () => {
      const latestJob = jobsRef.current.find((job) => job.id === jobId);
      const latestMap = safeScanMap(latestJob?.result?.map);
      if (latestMap.length) after = terminalScanPrefix(latestMap);
      controller = new AbortController();
      let retryDelay = 50;
      try {
        const progress = readScanProgress(await api(`/jobs/${jobId}/scan-progress?after=${after}`, {
          signal: controller.signal,
          timeoutMs: 5_000
        }));
        if (stopped || controller.signal.aborted) return;
        if (Number.isSafeInteger(progress.next) && progress.next >= after) after = progress.next;
        setJobs((current) => current.map((job) => job.id === jobId ? mergeScanProgress(job, progress) : job));
        if (!ACTIVE.has(progress.status)) return;
      } catch (error) {
        if (stopped || controller.signal.aborted) return;
        retryDelay = 500;
      }
      timer = window.setTimeout(update, retryDelay);
    };
    update();
    return () => {
      stopped = true;
      controller?.abort();
      if (timer !== null) window.clearTimeout(timer);
    };
  }, [activeJob?.id, activeJob?.type, activeScan]);

  useEffect(() => {
    let stopped = false;
    let timer = null;
    const schedule = () => {
      timer = window.setTimeout(async () => {
        try {
          await refreshJobs();
        } finally {
          if (!stopped) schedule();
        }
      }, pollInterval);
    };
    schedule();
    return () => {
      stopped = true;
      if (timer !== null) window.clearTimeout(timer);
    };
  }, [pollInterval, refreshJobs]);

  useEffect(() => {
    if (hasActiveScan) return undefined;
    const timer = window.setInterval(() => refresh(true), 60_000);
    return () => window.clearInterval(timer);
  }, [hasActiveScan, refresh]);

  const shownJobNeedsDetail = Boolean(
    shownJob
    && !shownJob.id.startsWith("pending-")
    && !(shownJob.type === "speed" && ACTIVE.has(shownJob.status))
    && !Object.prototype.hasOwnProperty.call(shownJob, "result")
  );
  useEffect(() => {
    if (!shownJobNeedsDetail) {
      setDetailState((current) => current.status === "idle"
        ? current
        : { jobId: shownJob?.id || "", status: "idle", error: "" });
      return undefined;
    }
    const controller = new AbortController();
    const jobId = shownJob.id;
    let retryTimer = null;
    let failedAttempt = 0;
    setDetailState({ jobId, status: "loading", error: "" });
    const loadDetail = () => {
      api(`/jobs/${jobId}`, { signal: controller.signal })
        .then((data) => readJob(data, "读取任务详情"))
        .then((job) => {
          if (job.id !== jobId) throw new Error("任务详情响应与请求不匹配");
          if (!mountedRef.current || controller.signal.aborted) return;
          setJobs((current) => current.map((item) => item.id === job.id ? job : item));
          setDetailState({ jobId, status: "loaded", error: "" });
        })
        .catch((err) => {
          if (!mountedRef.current || controller.signal.aborted) return;
          const message = errorMessage(err);
          const retryDelay = detailRetryDelay(err, failedAttempt);
          if (retryDelay === null) {
            setDetailState({ jobId, status: "error", error: message });
            return;
          }
          failedAttempt += 1;
          setDetailState({ jobId, status: "retrying", error: `${message}，正在重试…` });
          retryTimer = window.setTimeout(loadDetail, retryDelay);
        });
    };
    loadDetail();
    return () => {
      controller.abort();
      if (retryTimer !== null) window.clearTimeout(retryTimer);
    };
  }, [detailReloadVersion, shownJob?.id, shownJobNeedsDetail]);

  async function startTask(options) {
    if (createPendingRef.current || controlsLocked || resumeLookupRef.current) return;
    if (options.type !== "scan") {
      openConfirmation(options);
      return;
    }
    const disk = snapshotDisk(selectedDisk);
    if (!disk) {
      setActionError("所选磁盘信息不完整，请刷新后重试");
      return;
    }
    const controller = new AbortController();
    resumeLookupRef.current = controller;
    setCheckingResume(true);
    setActionError("");
    try {
      const history = readJobList(await api("/jobs?summary=1", { signal: controller.signal }));
      if (controller.signal.aborted || !mountedRef.current || createPendingRef.current) return;
      const conflict = taskStartBlockedReason(history, "scan", disk);
      if (conflict) {
        setActionError(conflict);
        setJobs(current => mergeJobLists(current, history));
        return;
      }
      const job = findResumableScan(history, disk);
      if (job) setResumeRequest({ job, disk, startOptions: options });
      else openConfirmation(options);
    } catch (err) {
      if (!controller.signal.aborted && mountedRef.current) setActionError(`读取续扫记录失败：${errorMessage(err)}`);
    } finally {
      if (resumeLookupRef.current === controller) {
        resumeLookupRef.current = null;
        if (mountedRef.current) setCheckingResume(false);
      }
    }
  }

  function openConfirmation(options) {
    if (createPendingRef.current || controlsLocked) return;
    if (options.type === "speed") {
      if (!selectedSpeedTarget || selectedSpeedTarget.disabledReason) {
        setActionError("没有可测速的存储空间或无文件系统磁盘，请刷新后重试");
        return;
      }
      if (selectedSpeedTarget.type === "raw") {
        const disk = snapshotDisk(selectedSpeedTarget.disk);
        if (!disk || rawSpeedDiskBlockedReason(selectedSpeedTarget.disk, storageSpaces)) {
          setActionError("磁盘状态已变化，已识别到文件系统、挂载或存储空间，请刷新后重试");
          return;
        }
        setConfirmRequest({
          ...options,
          speedTarget: "raw",
          speedTrim: false,
          disk,
          expectedDisk: { path: disk.path, serial: disk.serial, model: disk.model, size: disk.size }
        });
        return;
      }
      const storageSpace = snapshotStorageSpace(selectedSpeedTarget.storageSpace);
	  if (!storageSpace) {
		setActionError("挂载目标信息不完整，请刷新后重试");
		return;
	  }
      setConfirmRequest({
        ...options,
        speedTarget: "filesystem",
        storageSpace,
        expectedStorageSpace: {
          path: storageSpace.path,
          source: storageSpace.source,
          fileSystem: storageSpace.fileSystem,
          layout: storageSpace.layout,
          members: storageSpace.members,
		  size: storageSpace.size,
		  readOnly: storageSpace.readOnly,
          isSystem: storageSpace.isSystem
        }
      });
      return;
    }
    const disk = snapshotDisk(selectedDisk);
    if (!disk) {
      setActionError("所选磁盘信息不完整，请刷新后重试");
      return;
    }
    setConfirmRequest({
      ...options,
      disk,
      expectedDisk: {
        path: disk.path,
        serial: disk.serial,
        model: disk.model,
        size: disk.size
      }
    });
  }

  function openRecordedRepair(job) {
    if (recordedRepairBlockedReason || controlsLocked) return;
    const disk = snapshotDisk(selectedDisk);
    if (!disk || !jobBelongsToDisk(job, selectedDisk)) return;
    setActionError("");
    setRecordedRepairRequest({ job, disk });
  }

  function cancelResumeLookup() {
    resumeLookupRef.current?.abort();
    resumeLookupRef.current = null;
    setCheckingResume(false);
    setResumeRequest(null);
  }

  function selectMode(nextMode) {
    initialSelectionPendingRef.current = false;
    cancelResumeLookup();
    setMode(nextMode);
    setSelectedJob(null);
  }

  function selectDisk(path) {
    if (createPendingRef.current) return;
    initialSelectionPendingRef.current = false;
    cancelResumeLookup();
    setScanRepair(false);
    const nextDisk = disks.find((disk) => disk.path === path) || null;
    selectedDiskRef.current = nextDisk;
    setSelectedPath(path);
    setSelectedJob(null);
    setActionError("");
  }

  function selectSpeedTarget(key) {
    if (createPendingRef.current) return;
    cancelResumeLookup();
    const target = speedTargets.find((candidate) => candidate.key === key && !candidate.disabledReason);
    if (!target) return;
    initialSelectionPendingRef.current = false;
    setSelectedSpeedKey(target.key);
    if (target.type === "filesystem") {
      selectedStorageRef.current = target.storageSpace;
      setSelectedStoragePath(target.path);
    }
    const memberDisk = target.disk || disks.find((disk) => storageSpacesForDisk([target.storageSpace], disk).length > 0);
    if (memberDisk) {
      selectedDiskRef.current = memberDisk;
      setSelectedPath(memberDisk.path);
    }
    setSelectedJob(null);
  }

  function selectHistoryJob(job) {
    if (createPendingRef.current) return;
    initialSelectionPendingRef.current = false;
    cancelResumeLookup();
    jobsVersionRef.current += 1;
    invalidateRefresh();
    setJobs((current) => mergeJobLists(current, [job, ...current.filter((item) => ACTIVE.has(item.status) && item.id !== job.id)]));
    setSelectedJob(job);
    setMode(job.type === "write" ? "scan" : job.type);
    if (job.type === "write") {
      setScanMode("full");
      setScanRepair(false);
      setScanSelfTest(false);
    } else if (job.type === "scan" && (job.options?.scanMode === "quick" || job.options?.scanMode === "full")) {
      setScanMode(job.options.scanMode);
      setScanExactSectors(job.options?.scanExactSectors !== false);
      setScanDeepLocate(job.options?.scanDeepLocate === true);
      setScanSelfTest(job.options?.scanSelfTest === true);
      setScanRepair(job.options?.scanRepair === true);
      setScanRepairAfterMs(job.options?.scanRepairAfterMs || 500);
    }
    const currentDisk = findCurrentDiskForJob(disks, job);
    if (job.type === "speed") {
      const target = speedTargets.find((candidate) => !candidate.disabledReason && jobBelongsToSpeedTarget(job, candidate));
      if (target) selectSpeedTarget(target.key);
      setSelectedJob(job);
    } else if (currentDisk) {
      selectedDiskRef.current = currentDisk;
      setSelectedPath(currentDisk.path);
    }
  }

  async function returnToScan(repairJob) {
    const sourceId = repairJob.options?.scanRepairFrom;
    if (!sourceId || controlsLocked || returnToScanRef.current) return;
    const controller = new AbortController();
    returnToScanRef.current = controller;
    setReturningRepairId(repairJob.id);
    setActionError("");
    try {
      const source = jobsRef.current.find((job) => job.id === sourceId)
        || readJob(await api(`/jobs/${sourceId}`, { signal: controller.signal }), "读取来源扫描记录");
      if (controller.signal.aborted || !mountedRef.current) return;
      if (source.id !== sourceId || !["scan", "write"].includes(source.type) || source.options?.scanRepairFrom
        || !jobBelongsToDisk(source, { ...repairJob.disk, path: repairJob.device })) {
        throw new Error("来源扫描记录与本次修复不匹配");
      }
      selectHistoryJob(source);
    } catch (error) {
      if (!controller.signal.aborted && mountedRef.current) {
        setActionError(error.status === 404
          ? "来源扫描记录已删除，可通过历史记录查看其他扫描结果。"
          : `返回扫描结果失败：${errorMessage(error)}`);
      }
    } finally {
      if (returnToScanRef.current === controller) {
        returnToScanRef.current = null;
        if (mountedRef.current) setReturningRepairId("");
      }
    }
  }

  async function createJob(options) {
    if (createPendingRef.current || controlsLocked) return;
    if (options.scanRepairFrom && health?.capabilities?.recordedRepair !== true) {
      setActionError("当前后台不支持按记录修复，请更新并重启应用");
      return;
    }
    initialSelectionPendingRef.current = false;
    createPendingRef.current = true;
    setCreatingJob(true);
    setActionError("");
    jobsVersionRef.current += 1;
    invalidateRefresh();
    try {
      const data = await api("/jobs", { method: "POST", body: JSON.stringify(options) });
      const job = readJob(data, "创建任务");
      if (!mountedRef.current) return;
      setJobs((current) => [job, ...current.filter((item) => item?.id !== job.id)]);
      setSelectedJob(job);
      setConfirmRequest(null);
      setRecordedRepairRequest(null);
      setResumeRequest(null);
    } catch (err) {
      if (mountedRef.current) setActionError(errorMessage(err));
    } finally {
      jobsVersionRef.current += 1;
      createPendingRef.current = false;
      if (mountedRef.current) setCreatingJob(false);
    }
  }

  function resumeScan(restart = false) {
    if (!resumeRequest || controlsLocked) return;
    const { job, disk, startOptions } = resumeRequest;
    const options = {
      type: "scan", device: disk.path, ...(restart ? startOptions : job.options),
      ...(!restart ? { resumeFrom: job.id } : {}),
      expectedDisk: { path: disk.path, serial: disk.serial, model: disk.model, size: disk.size }
    };
    setScanMode(options.scanMode);
    setScanExactSectors(options.scanExactSectors !== false);
    setScanDeepLocate(options.scanDeepLocate === true);
    setScanSelfTest(options.scanSelfTest === true);
    setScanRepair(options.scanRepair === true);
    setScanRepairAfterMs(options.scanRepairAfterMs || 500);
    if (restart || options.scanRepair) {
      setResumeRequest(null);
      openConfirmation(options);
    } else createJob(options);
  }

  async function cancelJob(id) {
    if (typeof id !== "string" || !id.trim()) {
      setActionError("停止任务响应格式不正确");
      return;
    }
    if (cancelPendingRef.current.has(id)) return;
    cancelPendingRef.current.add(id);
    setCancellingJobIds(current => new Set(current).add(id));
    setActionError("");
    jobsVersionRef.current += 1;
    invalidateRefresh();
    try {
      const data = await api(`/jobs/${id}/cancel`, { method: "POST", body: "{}" });
      const job = readJob(data, "停止任务");
      if (job.id !== id) throw new Error("停止任务响应与请求不匹配");
      if (!mountedRef.current) return;
      setJobs((current) => current.map((item) => item?.id === id ? job : item));
    } catch (err) {
      if (mountedRef.current) setActionError(errorMessage(err));
    } finally {
      jobsVersionRef.current += 1;
      cancelPendingRef.current.delete(id);
      if (mountedRef.current) setCancellingJobIds(current => new Set([...current].filter(jobId => jobId !== id)));
    }
  }

  async function deleteHistoryJob() {
    const id = deleteRequest?.id;
    if (typeof id !== "string" || !id.trim() || deletingJobId) return;
    setDeletingJobId(id);
    setDeleteError("");
    setActionError("");
    jobsVersionRef.current += 1;
    invalidateRefresh();
    try {
      const data = await api(`/jobs/${id}`, { method: "DELETE", body: "{}" });
      if (data.deletedId !== id) throw new Error("删除历史记录响应不正确");
      if (!mountedRef.current) return;
      setJobs((current) => current.filter((job) => job?.id !== id));
      setSelectedJob((current) => current?.id === id ? null : current);
      setDetailState((current) => current.jobId === id ? { jobId: "", status: "idle", error: "" } : current);
      setDeletedHistoryId(id);
      setDeleteRequest(null);
    } catch (err) {
      if (mountedRef.current) {
        setDeleteError(errorMessage(err));
      }
    } finally {
      jobsVersionRef.current += 1;
      if (mountedRef.current) setDeletingJobId("");
    }
  }

  const topError = actionError
    ? { message: actionError, dismiss: () => setActionError("") }
    : (jobsError
      ? { message: jobsError, dismiss: () => setJobsError("") }
      : (resourceError ? { message: resourceError, dismiss: () => setResourceError("") } : null));
  const diskAvailabilityError = !selectedDisk
    ? (health && health.capabilities?.scan !== true
      ? "系统缺少 lsblk，无法识别可检测的物理磁盘。"
      : resourceError)
    : "";
  const shownDetailState = shownJobNeedsDetail && detailState.jobId === shownJob?.id ? detailState : null;

  return (
    <div className="app-shell">
      <a className="skip-link" href="#main-content">跳到主要内容</a>
      {topError && <div className="top-alert" role="alert"><AlertTriangle size={18} /><span>{topError.message}</span><button onClick={topError.dismiss} aria-label="关闭错误提示"><X size={17} /></button></div>}
      <div className="workspace">
        <main id="main-content" className="main-content">
          <div className={`dashboard-grid${!selectedDisk && jobs.length === 0 ? " single-column" : ""}`}>
            <div className="control-column" data-mode={mode}>
              <DiskSelector
                disks={disks}
                selected={selectedDisk}
                onSelect={selectDisk}
                loading={loading}
                locked={creatingJob || Boolean(activeSpeedJob)}
                onRefresh={() => refresh()}
                refreshing={refreshing}
                availabilityError={diskAvailabilityError}
                storageWarning={storageWarning}
                mode={mode}
                speedTargets={speedTargets}
                selectedSpeedTarget={selectedSpeedTarget}
                onSelectSpeedTarget={selectSpeedTarget}
                theme={theme}
                onToggleTheme={toggleTheme}
              />
              {!selectedDisk && jobs.length === 0 && <HistoryTrigger onOpen={() => setHistoryOpen(true)} />}
              {(selectedDisk || jobs.length > 0) && (
                <TaskSetup
                  mode={mode}
                  scanMode={scanMode}
                  setScanMode={setScanMode}
                  scanExactSectors={scanExactSectors}
                  scanDeepLocate={scanDeepLocate}
                  setScanDeepLocate={setScanDeepLocate}
                  setScanExactSectors={setScanExactSectors}
                  scanSelfTest={scanSelfTest}
                  setScanSelfTest={setScanSelfTest}
                  scanRepair={scanRepair}
                  setScanRepair={setScanRepair}
                  scanRepairAfterMs={scanRepairAfterMs}
                  setScanRepairAfterMs={setScanRepairAfterMs}
                  speedConfig={speedConfig}
                  setSpeedConfig={setSpeedConfig}
                  health={health}
                  onStart={startTask}
                  onCancel={cancelJob}
                  activeJob={activeJob}
                  resultJob={shownJob}
                  submitting={creatingJob || checkingResume}
                  checkingResume={checkingResume}
                  cancelPending={cancellingJobIds.has(activeJob?.id)}
                  startBlockedReason={startBlockedReason}
                  targetAvailable={Boolean(mode === "speed" ? selectedSpeedTarget : selectedDisk)}
                  disk={selectedDisk}
                  speedTarget={selectedSpeedTarget}
                />
              )}
            </div>
            {(selectedDisk || jobs.length > 0) && (
              <div className="result-column">
                <div className="result-module">
                  <div className="result-module-heading">
                    <div className="result-titlebar">
                      <div className="result-title">
                        <h1>{mode === "speed" ? "磁盘测速" : "坏道扫描"}</h1>
                        <p data-tooltip={resultDeviceLabel}>{resultDeviceLabel}</p>
                      </div>
                      <ModeSwitcher mode={mode} setMode={selectMode} />
                    </div>
                    {mode === "scan" && <RunningTasks disks={disks} storageSpaces={storageSpaces} jobs={activeJobs} selectedId={shownJob?.id} onSelect={selectHistoryJob} onCancel={cancelJob} cancellingIds={cancellingJobIds} locked={creatingJob} />}
                  </div>
                  <ActiveTask
                    job={shownJob}
                    contextDisk={selectedDisk}
                    detailState={shownDetailState}
                    onReloadDetail={() => setDetailReloadVersion((version) => version + 1)}
                    speedConfig={speedConfig}
                    onStartSpeed={openConfirmation}
                    speedStartDisabled={!selectedSpeedTarget || controlsLocked || health?.capabilities?.scan !== true || health?.capabilities?.fio !== true}
                    onOpenHistory={() => setHistoryOpen(true)}
                    onRepairRecords={openRecordedRepair}
                    repairDisabledReason={recordedRepairBlockedReason}
                    onReturnToScan={returnToScan}
                    returningRepairId={returningRepairId}
                    returnDisabled={controlsLocked}
                  />
                </div>
              </div>
            )}
          </div>
        </main>
      </div>
      {recordedRepairRequest && <RecordedRepairDialog request={recordedRepairRequest} submitting={creatingJob} error={actionError} onClose={() => { if (!creatingJob) setRecordedRepairRequest(null); }} onConfirm={createJob} />}
      {confirmRequest && (
        <ConfirmDialog
          request={confirmRequest}
          onClose={() => { if (!creatingJob) setConfirmRequest(null); }}
          onConfirm={createJob}
          submitting={creatingJob}
        />
      )}
      {resumeRequest && <ResumeScanDialog request={resumeRequest} onClose={() => { if (!creatingJob) setResumeRequest(null); }}
        onContinue={() => resumeScan()} onRestart={() => resumeScan(true)} submitting={creatingJob} error={actionError} />}
      {historyOpen && <HistoryDialog
        disks={disks} storageSpaces={storageSpaces}
        mode={mode}
        selectedId={shownJob?.id}
        deletedId={deletedHistoryId}
        onSelect={selectHistoryJob}
        onDelete={(job) => { setDeleteError(""); setDeleteRequest(job); }}
        onClose={() => setHistoryOpen(false)}
        selectionLocked={creatingJob}
      />}
      <DeleteHistoryDialog
        job={deleteRequest}
        deleting={Boolean(deletingJobId)}
        error={deleteError}
        onClose={() => { if (!deletingJobId) setDeleteRequest(null); }}
        onConfirm={deleteHistoryJob}
      />
    </div>
  );
}

createRoot(document.getElementById("root")).render(<ImmediateTooltipProvider><App /></ImmediateTooltipProvider>);
