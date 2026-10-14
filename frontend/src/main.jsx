import React, { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import {
  AlertTriangle,
  Activity,
  Check,
  ChevronDown,
  CircleStop,
  Gauge,
  History,
  Info,
  LoaderCircle,
  Moon,
  RefreshCw,
  ScanSearch,
  Server,
  Settings2,
  ShieldCheck,
  Sun,
  Trash2,
  X
} from "lucide-react";
import {
  advanceScanReveal,
  buildSpeedTargets,
  chooseShownJob,
  detailRetryDelay,
  estimateRemainingMilliseconds,
  findCurrentDiskForJob,
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
  safeScanMap,
  scanOutcomeSummary,
  scanRangeOutcomeLabel,
  validScanIssuePage,
  scanStatusGroup,
  storageSpaceUsesOnlySolidStateDisks,
  storageSpacesForDisk,
  terminalScanPrefix
} from "./job-model.js";
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
  return { disks: data.disks, storageSpaces: data.storageSpaces };
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
    readOnly: space.readOnly === true
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

function formatMilliseconds(value) {
  if (!Number.isFinite(value)) return "-";
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
  const value = (candidate) => Number.isFinite(candidate) ? candidate : 0;
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

function DiskDetails({ disk, showHealth = true }) {
  const [smartOpen, setSmartOpen] = useState(false);
  const [smartLabel, smartTone] = smartStatusMeta(disk);
  const criticalRows = criticalAttributeRows(disk);
  const mountpoints = Array.isArray(disk.mountpoints)
    ? disk.mountpoints.filter((mountpoint) => typeof mountpoint === "string" && mountpoint)
    : [];
  return <>
    <dl className="device-specs">
      <div><dt>型号</dt><dd title={disk.model || "未知型号"}>{disk.model || "未知型号"}</dd></div>
      <div><dt>容量</dt><dd>{formatBytes(disk.size)}</dd></div>
      <div><dt>接口</dt><dd>{disk.transport?.toUpperCase() || "-"}</dd></div>
      <div><dt>固件版本</dt><dd title={disk.firmware || "未提供"}>{disk.firmware || "-"}</dd></div>
      <div><dt>序列号</dt><dd title={disk.serial || "未提供"}>{disk.serial || "未提供"}</dd></div>
      <div><dt>挂载位置</dt><dd title={mountpoints.join(", ")}>{mountpoints.length ? mountpoints.join(", ") : "未挂载"}</dd></div>
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
            <dt title={hint}>{label}</dt>
            <dd className={value > 0 ? "value-danger" : ""} title={hint}>{formatInteger(value)}</dd>
          </div>)}
        </dl>
      </div>}
      <div className="disk-state">
        {disk.isSystem && <span className="badge badge-warning"><AlertTriangle size={13} /> 系统盘</span>}
        <span className="badge badge-neutral">{disk.rotational ? "HDD" : "SSD"}</span>
        <button type="button" className="badge badge-success smart-info-button" aria-haspopup="dialog" title="查看原始 SMART 信息" onClick={() => setSmartOpen(true)}><ShieldCheck size={13} aria-hidden="true" /> SMART 信息</button>
      </div>
    </div>}
    {smartOpen && <SmartInfoDialog disk={disk} onClose={() => setSmartOpen(false)} />}
  </>;
}

function DiskSelector({ disks, selected, onSelect, loading, locked, onRefresh, refreshing, availabilityError, mode, speedTargets, selectedSpeedTarget, onSelectSpeedTarget, theme, onToggleTheme }) {
	const speedMode = mode === "speed";
  const [detailsExpanded, setDetailsExpanded] = useState(false);
  const [memberDetailPath, setMemberDetailPath] = useState("");
  useEffect(() => setDetailsExpanded(false), [mode, selected?.path, selectedSpeedTarget?.key]);
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
	const externalFileTargets = speedTargets.filter((target) => target.type === "filesystem" && !/^\/vol\d+$/.test(target.path));
	const hasTarget = speedMode ? Boolean(selectedSpeedTarget && !selectedSpeedTarget.disabledReason) : Boolean(selected);
  return (
    <section className="device-panel disk-sidebar" aria-label={speedMode ? "测速目标" : "目标磁盘"}>
      <div className="device-toolbar">
        <div className="device-toolbar-actions">
            <button
              className="icon-button device-refresh"
              type="button"
              onClick={onRefresh}
              disabled={refreshing}
              aria-label={refreshing ? "正在刷新磁盘和任务" : "刷新磁盘和任务"}
              aria-busy={refreshing ? "true" : undefined}
              title={refreshing ? "正在刷新" : "刷新磁盘和任务"}
            >
              <RefreshCw size={16} className={refreshing ? "spin" : ""} aria-hidden="true" />
            </button>
            <button
              className="icon-button theme-toggle"
              type="button"
              onClick={onToggleTheme}
              aria-label={theme === "dark" ? "切换到浅色主题" : "切换到深色主题"}
              aria-pressed={theme === "dark"}
              title={theme === "dark" ? "浅色主题" : "深色主题"}
            >
              <span className="theme-light" aria-hidden="true"><Sun size={16} /></span>
              <span className="theme-dark" aria-hidden="true"><Moon size={16} /></span>
            </button>
        </div>
        <label className="disk-select-field">
          <span>{speedMode ? "测速目标" : "目标磁盘"}</span>
          <select
            value={speedMode ? (selectedSpeedTarget?.key || "") : (selected?.path || "")}
            onChange={(event) => speedMode ? onSelectSpeedTarget(event.target.value) : onSelect(event.target.value)}
            disabled={locked || loading || (speedMode ? !speedTargets.some((target) => !target.disabledReason) : disks.length === 0)}
            aria-label={speedMode ? "选择测速目标" : "选择目标磁盘"}
          >
			{loading && <option>正在读取存储信息...</option>}
			{!loading && speedMode && !speedTargets.some((target) => !target.disabledReason) && <option value="">没有可测速的目标</option>}
			{!loading && !speedMode && disks.length === 0 && <option>没有可用磁盘</option>}
			{speedMode
			  ? <>
              {internalFileTargets.length > 0 && <optgroup label="存储空间 · 文件测速">
                {internalFileTargets.map((target) => (
                  <option value={target.key} key={target.key}>{target.path} · {storageLayoutLabel(target.storageSpace)} · {String(target.storageSpace.fileSystem || "未知").toUpperCase()} · {formatBytes(target.storageSpace.size)}</option>
                ))}
              </optgroup>}
              {externalFileTargets.length > 0 && <optgroup label="外接硬盘 · 文件测速">
                {externalFileTargets.map((target) => (
                  <option value={target.key} key={target.key}>{target.path} · {String(target.storageSpace.fileSystem || "未知").toUpperCase()} · {formatBytes(target.storageSpace.size)}{target.storageSpace.readOnly ? " · 只读" : ""}</option>
                ))}
              </optgroup>}
              {speedTargets.some((target) => target.type === "raw") && <optgroup label="物理磁盘 · 底层测速">
                {speedTargets.filter((target) => target.type === "raw").map((target) => (
                  <option value={target.key} key={target.key} disabled={Boolean(target.disabledReason)}>
                    {target.disk.model} · {target.disk.name || target.path} · {target.disabledReason || "未识别文件系统，可底层测速"}
                  </option>
                ))}
              </optgroup>}
            </>
			  : disks.map((disk) => (
			    <option value={disk.path} key={disk.path}>
			      {disk.model} · {disk.path} · {formatBytes(disk.size)}
			    </option>
			  ))}
          </select>
        </label>
      </div>
		{hasTarget && <div className="compact-disk-summary">
          <span>{speedMode ? (selectedStorageSpace?.path || rawDisk?.model || "物理磁盘") : (selected?.model || "物理磁盘")}</span>
          <strong>{speedMode ? (selectedStorageSpace ? `${String(selectedStorageSpace.fileSystem || "未知").toUpperCase()} · ${formatBytes(selectedStorageSpace.size)}` : formatBytes(rawDisk?.size)) : `${smartLabel} · ${Number.isFinite(selected?.temperatureC) ? `${selected.temperatureC} °C` : formatBytes(selected?.size)}`}</strong>
          <button type="button" className="compact-detail-toggle" aria-expanded={detailsExpanded} aria-controls="disk-detail-content" onClick={() => setDetailsExpanded((current) => !current)}>
            磁盘详情 <ChevronDown size={15} aria-hidden="true" />
          </button>
        </div>}
		{hasTarget && (
        <div id="disk-detail-content" className={`device-meta compact-detail-content${detailsExpanded ? " compact-expanded" : ""}`}>
          {speedMode && memberDisks.length > 1 && <label className="disk-select-field member-disk-select-field">
            <span>查看成员盘</span>
            <select value={detailDisk?.path || ""} disabled={locked || loading} aria-label="选择查看的成员盘" onChange={(event) => setMemberDetailPath(event.target.value)}>
              {memberDisks.map((disk) => <option key={disk.path} value={disk.path}>{disk.model || "未知型号"} · {disk.path}</option>)}
            </select>
          </label>}
          {detailDisk ? <DiskDetails key={detailDisk.path} disk={detailDisk} showHealth={!speedMode} /> : <p className="disk-detail-unavailable">未识别到对应的物理磁盘，暂无法显示硬盘详情。</p>}
          {speedMode && selectedStorageSpace && <div className="speed-target-details">
		    <dl className="device-specs">
		      <div><dt>{/^\/vol\d+$/.test(selectedStorageSpace.path) ? "存储空间" : "外接挂载"}</dt><dd>{selectedStorageSpace.path}</dd></div>
		      <div><dt>阵列结构</dt><dd>{storageLayoutLabel(selectedStorageSpace)}</dd></div>
		      <div><dt>文件系统</dt><dd>{String(selectedStorageSpace.fileSystem || "-").toUpperCase()}</dd></div>
		      <div><dt>总容量</dt><dd>{formatBytes(selectedStorageSpace.size)}</dd></div>
		      <div><dt>可用空间</dt><dd>{formatBytes(selectedStorageSpace.freeBytes)}</dd></div>
		      <div><dt>成员盘</dt><dd title={storageMembers.join(" + ")}>{storageMembers.length ? storageMembers.map((member) => member.replace(/^\/dev\//, "")).join(" + ") : "未知"}</dd></div>
		    </dl>
		    <div className="disk-state">
		      <span className="badge badge-neutral">{storageLayoutLabel(selectedStorageSpace)}</span>
		      <span className="badge badge-success"><ShieldCheck size={13} /> {/^\/vol\d+$/.test(selectedStorageSpace.path) ? "逻辑存储空间" : "外接文件系统"}</span>
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
        ? <p>候选 {repair.candidates || 0} · 数据校验通过 {(repair.verified || 0) + (repair.stillSlow || 0)} · 其中仍慢 {repair.stillSlow || 0} · 跳过 {repair.skipped || 0} · 失败 {repair.failed || 0} · {ACTIVE.has(job.status) ? "处理中" : "结果未确定"} {repair.pending || 0}</p>
        : <p>正在加载写回统计…</p>}
      <p>首次慢响应仍保留。写回校验通过不能保证物理损伤已修复。原始数据备份保存在应用数据目录 repair-backups/{job.id}；删除历史不会删除备份。</p>
    </div>
  );
}

function SpeedSettingsDialog({ speedConfig, health, speedTarget, controlsLocked, triggerRef, onApply, onClose }) {
  const dialogRef = useRef(null);
  const [draft, setDraft] = useState(() => ({ ...speedConfig }));
  const trimTargetAllowed = speedTarget?.type === "filesystem" && isFnOsStorageSpacePath(speedTarget.path);
  const trimAvailable = health?.capabilities?.fstrim === true && trimTargetAllowed;
  const trimTitle = speedTarget?.type === "raw"
    ? "底层测速不执行文件系统 TRIM"
    : (!isFnOsStorageSpacePath(speedTarget?.path)
      ? "外置挂载不执行测速前 TRIM"
      : (health?.capabilities?.fstrim === true ? "测速前对所选存储空间执行 TRIM" : "系统未提供 fstrim"));

  useLayoutEffect(() => {
    const dialog = dialogRef.current;
    dialog.showModal();
    dialog.querySelector(".speed-engine-options button:not(:disabled)")?.focus();
    return () => {
      dialog.close();
      triggerRef.current?.focus();
    };
  }, [triggerRef]);

  return (
    <dialog id="speed-settings-dialog" ref={dialogRef} className="confirm-dialog speed-settings-dialog" aria-labelledby="speed-settings-title" onCancel={(event) => { event.preventDefault(); onClose(); }} onKeyDown={(event) => {
      if (event.key !== "Tab") return;
      const controls = event.currentTarget.querySelectorAll("button:not(:disabled), select:not(:disabled), input:not(:disabled)");
      const first = controls[0];
      const last = controls[controls.length - 1];
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last?.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first?.focus();
      }
    }}>
      <div className="dialog-title">
        <Settings2 size={20} aria-hidden="true" />
        <div><h2 id="speed-settings-title">测速设置</h2></div>
        <button type="button" className="icon-button" aria-label="关闭测速设置" onClick={onClose}><X size={20} /></button>
      </div>
      <div className="dialog-body">
        <fieldset className="speed-settings-fields" disabled={controlsLocked} aria-label="测速高级参数">
          <div className="speed-runtime-config">
            <fieldset className="speed-engine-field">
              <legend>FIO 任务队列</legend>
              <div className="segmented speed-engine-options">
                <button type="button" autoFocus className={draft.speedIOEngine === "io_uring" ? "active" : ""} aria-pressed={draft.speedIOEngine === "io_uring"} onClick={() => setDraft((current) => ({ ...current, speedIOEngine: "io_uring", speedQueueDepth: Math.max(8, current.speedQueueDepth) }))}>io_uring</button>
                <button type="button" className={draft.speedIOEngine === "psync" ? "active" : ""} aria-pressed={draft.speedIOEngine === "psync"} onClick={() => setDraft((current) => ({ ...current, speedIOEngine: "psync", speedQueueDepth: 1 }))}>psync</button>
              </div>
            </fieldset>
            <label><span>队列深度</span><select value={draft.speedIOEngine === "psync" ? 1 : draft.speedQueueDepth} disabled={draft.speedIOEngine === "psync"} onChange={(event) => setDraft((current) => ({ ...current, speedQueueDepth: Number(event.target.value) }))}><option value="1">1</option><option value="8">8</option><option value="16">16</option><option value="32">32</option><option value="64">64</option><option value="128">128</option></select></label>
            <label><span>并发任务</span><select value={draft.speedThreads} onChange={(event) => setDraft((current) => ({ ...current, speedThreads: Number(event.target.value) }))}><option value="1">1</option><option value="2">2</option><option value="4">4</option><option value="8">8</option></select></label>
          </div>
          <fieldset className="speed-extra-options">
            <legend className="sr-only">附加选项</legend>
            <label title="绕过系统页缓存，测量更接近存储介质的性能"><input type="checkbox" checked={draft.speedDirectIO} onChange={(event) => setDraft((current) => ({ ...current, speedDirectIO: event.target.checked }))} /><span>Direct I/O</span></label>
            <label className={trimAvailable ? "" : "disabled"} title={trimTitle}><input type="checkbox" checked={trimTargetAllowed && draft.speedTrim} disabled={!trimAvailable} onChange={(event) => setDraft((current) => ({ ...current, speedTrim: event.target.checked }))} /><span>测速前 TRIM</span></label>
          </fieldset>
        </fieldset>
      </div>
      <div className="dialog-actions">
        <button type="button" className="button secondary" onClick={onClose}>取消</button>
        <button type="button" className="button primary" disabled={controlsLocked} onClick={() => onApply(draft)}>应用设置</button>
      </div>
    </dialog>
  );
}

function TaskSetup({ mode, scanMode, setScanMode, scanExactSectors, setScanExactSectors, scanDeepLocate, setScanDeepLocate, scanSelfTest, setScanSelfTest, scanRepair, setScanRepair, scanRepairAfterMs, setScanRepairAfterMs, speedConfig, setSpeedConfig, health, onStart, onCancel, activeJob, resultJob, finishing, submitting, checkingResume, cancelPending, targetAvailable, disk, speedTarget }) {
  const [etaClock, setEtaClock] = useState(() => Date.now());
  const [speedSettingsOpen, setSpeedSettingsOpen] = useState(false);
  const speedSettingsTriggerRef = useRef(null);
  const scanAvailable = health?.capabilities?.scan === true;
  const speedAvailable = scanAvailable && health?.capabilities?.fio === true;
  const speedReadOnly = speedConfig.speedOperation === "read";
  const speedWillWrite = Boolean(speedTarget) && !speedReadOnly;
	const readOnlyFileTarget = speedTarget?.type === "filesystem" && speedTarget.storageSpace?.readOnly === true;
  const unavailableMessage = !scanAvailable
    ? "系统缺少 lsblk，无法识别可检测的物理磁盘。"
    : "系统缺少 fio，无法执行测速。";
  const repairBlockedReason = scanRepairDiskBlockedReason(disk);
  const repairEnabled = mode === "scan" && scanRepair;
  const repairThresholdValid = Number.isInteger(Number(scanRepairAfterMs)) && Number(scanRepairAfterMs) >= 1 && Number(scanRepairAfterMs) <= 60000;
  const stopping = activeJob?.status === "cancelling" || cancelPending;
  const startLabel = mode === "speed"
    ? "开始测速"
    : (scanMode === "full" ? "开始全盘扫描" : "开始快速扫描");
  const controlsLocked = Boolean(activeJob) || finishing || submitting;
  const actionLabel = activeJob ? (stopping ? "正在停止" : "停止任务") : (finishing ? "正在完成块图" : (checkingResume ? "正在检查扫描记录" : startLabel));
  const startOptions = mode === "speed"
    ? {
        type: "speed",
        speedRuns: speedConfig.speedRuns,
        sampleMiB: speedConfig.sampleMiB,
        speedPosition: "all",
        speedBlockSize: speedConfig.speedBlockSize,
        speedQueueDepth: speedConfig.speedIOEngine === "psync" ? 1 : speedConfig.speedQueueDepth,
        speedThreads: speedConfig.speedThreads,
        speedIOEngine: speedConfig.speedIOEngine,
        speedDirectIO: speedConfig.speedDirectIO,
        speedTrim: speedTarget?.type === "raw" ? false : speedConfig.speedTrim,
        speedProfile: true,
        ...(speedConfig.speedOperation !== "auto" ? { speedOperation: speedConfig.speedOperation } : {})
      }
    : { type: "scan", scanMode, scanExactSectors, scanDeepLocate, scanRepair: repairEnabled, ...(repairEnabled ? { scanRepairAfterMs: Number(scanRepairAfterMs) } : {}), scanSelfTest: scanSelfTest && /^\/dev\/nvme\d+(?:c\d+)?n\d+$/.test(disk?.path || "") && !health?.capabilities?.mock };
  const actionDisabled = activeJob
    ? stopping
    : (finishing || !targetAvailable || submitting || (repairEnabled && (Boolean(repairBlockedReason) || !repairThresholdValid)) || (readOnlyFileTarget && speedWillWrite) || (mode === "speed" ? !speedAvailable : !scanAvailable));
  const speedFileNote = health?.testPlan?.status === "provisional" && mode === "speed" && speedTarget?.type === "filesystem" && speedWillWrite
    ? `测速会在 ${speedTarget.path} 创建临时测试文件，结束后自动删除。`
    : "";
  const actionTitle = activeJob
    ? (stopping ? "正在停止当前任务" : "停止当前任务")
    : (finishing ? "等待扫描块图显示完成" : !targetAvailable
      ? (mode === "speed" ? "没有可测速的存储空间或空盘" : "请先连接并选择磁盘")
      : (submitting
        ? (checkingResume ? "正在检查扫描记录" : "正在创建任务")
        : (readOnlyFileTarget && speedWillWrite ? "只读外接盘只能执行读取测速" : "")));
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
          <p className="eyebrow">Test profile</p>
          <div className="task-heading-title">
            <h2 id="test-heading">检测配置</h2>
            {mode === "speed" && <button ref={speedSettingsTriggerRef} type="button" className="icon-button speed-settings-trigger" disabled={controlsLocked} aria-label="测速设置" title="测速设置" aria-haspopup="dialog" aria-controls="speed-settings-dialog" onClick={() => setSpeedSettingsOpen(true)}><Settings2 size={16} aria-hidden="true" /></button>}
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
            aria-busy={stopping || submitting || finishing ? "true" : undefined}
            title={[actionTitle, !activeJob && speedFileNote].filter(Boolean).join("\n")}
            aria-describedby={!activeJob && speedFileNote ? "speed-temp-file-note" : undefined}
          >
            {activeJob
              ? (stopping ? <LoaderCircle className="spin" size={18} aria-hidden="true" /> : <CircleStop size={18} aria-hidden="true" />)
              : (finishing || checkingResume ? <LoaderCircle className="spin" size={18} aria-hidden="true" /> : (mode === "speed" ? <Gauge size={18} /> : <ScanSearch size={18} />))}
            {actionLabel}
          </button>
          {!activeJob && speedFileNote && <span id="speed-temp-file-note" className="sr-only">{speedFileNote}</span>}
        </div>
      </div>

      <fieldset className="task-panel" disabled={controlsLocked} aria-label="检测参数">
        {mode === "scan" && (
          <div className="mode-cards" role="radiogroup" aria-label="扫描方式">
            <button type="button" role="radio" aria-checked={scanMode === "quick"} className={`mode-card${scanMode === "quick" ? " selected" : ""}`} onClick={() => { setScanMode("quick"); setScanDeepLocate(false); }}>
              <span className="mode-card-icon" aria-hidden="true"><ScanSearch size={20} /></span>
              <span className="mode-card-copy">
                <strong>快速扫描</strong>
              </span>
              <span className="mode-card-check" aria-hidden="true">{scanMode === "quick" && <Check size={16} />}</span>
            </button>
            <button type="button" role="radio" aria-checked={scanMode === "full"} className={`mode-card${scanMode === "full" ? " selected" : ""}`} onClick={() => { setScanMode("full"); setScanDeepLocate(true); }}>
              <span className="mode-card-icon" aria-hidden="true"><Activity size={20} /></span>
              <span className="mode-card-copy">
                <strong>全盘扫描</strong>
              </span>
              <span className="mode-card-check" aria-hidden="true">{scanMode === "full" && <Check size={16} />}</span>
            </button>
          </div>
        )}
        {mode === "scan" && (
          <fieldset className="speed-extra-options scan-extra-options">
            <legend className="sr-only">扫描附加选项</legend>
            {/^\/dev\/nvme\d+(?:c\d+)?n\d+$/.test(disk?.path || "") && <label title={health?.capabilities?.mock ? "演示模式不执行固件自检" : "先运行设备支持的 NVMe 固件扩展自检，再进行读取扫描；会增加检测时间，不支持时提示停止。取消任务会尝试中止本次自检。"}>
              <input type="checkbox" checked={scanSelfTest && !health?.capabilities?.mock} disabled={health?.capabilities?.mock === true} onChange={(event) => setScanSelfTest(event.target.checked)} />
              <span>NVMe 固件自检（可选）</span>
            </label>}
            <label title="发现读取异常后细分到逻辑扇区；达到定位预算时保留待细查范围，继续后面的扫描">
              <input type="checkbox" checked={scanExactSectors} onChange={(event) => setScanExactSectors(event.target.checked)} />
              <span>精确定位错误扇区</span>
            </label>
            <select className="scan-probe-mode" aria-label="异常定位预算" value={scanDeepLocate ? "deep" : "standard"} onChange={(event) => setScanDeepLocate(event.target.value === "deep")} title="每个异常范围：标准最多 256 次／15 秒内提交新请求；深入最多 4096 次／2 分钟。已提交的读取仍需等待设备返回。">
              <option value="standard">标准定位</option>
              <option value="deep">深入定位</option>
            </select>
            <div className="scan-repair-option">
              <label title={repairBlockedReason ? `修复慢扇区不可用：${repairBlockedReason}` : undefined} className={repairBlockedReason ? "disabled" : ""}>
                <input type="checkbox" checked={scanRepair} disabled={Boolean(repairBlockedReason)} aria-describedby={repairBlockedReason ? "scan-repair-blocked-reason" : undefined} onChange={(event) => setScanRepair(event.target.checked)} />
                <span>修复慢扇区</span>
              </label>
              {repairEnabled && <label className="scan-repair-threshold">
                <span>超过</span>
                <input id="scan-repair-threshold" type="number" inputMode="numeric" min="1" max="60000" step="1" value={scanRepairAfterMs} onChange={(event) => setScanRepairAfterMs(event.target.value)} aria-label="慢扇区修复阈值（毫秒）" aria-describedby={!repairThresholdValid ? "scan-repair-error" : undefined} aria-invalid={!repairThresholdValid} />
                <span>ms</span>
              </label>}
            </div>
            {repairBlockedReason && <span id="scan-repair-blocked-reason" className="sr-only">修复慢扇区不可用：{repairBlockedReason}</span>}
            <ScanRepairSummary job={resultJob} />
          </fieldset>
        )}
        {repairEnabled && !repairThresholdValid && <p id="scan-repair-error" className="scan-repair-error value-danger" role="alert">请输入 1～60000 的整数。</p>}
        {mode === "speed" && (
          <div className="speed-config-stack">
            <div className="speed-sidebar-config">
              <label><span>测试次数</span><select value={speedConfig.speedRuns} onChange={(event) => setSpeedConfig((current) => ({ ...current, speedRuns: Number(event.target.value) }))}><option value="1">1 次</option><option value="3">3 次</option><option value="5">5 次</option></select></label>
              <label><span>每项样本</span><select value={speedConfig.sampleMiB} onChange={(event) => setSpeedConfig((current) => ({ ...current, sampleMiB: Number(event.target.value) }))}><option value="256">256 MiB</option><option value="1024">1 GiB</option><option value="4096">4 GiB</option></select></label>
              <label><span>块大小</span><select value={speedConfig.speedBlockSize} onChange={(event) => setSpeedConfig((current) => ({ ...current, speedBlockSize: Number(event.target.value) }))}><option value="4096">4 KiB</option><option value="65536">64 KiB</option><option value="1048576">1 MiB</option><option value="4194304">4 MiB</option></select></label>
              <label><span>测试组合</span><select value={speedConfig.speedOperation} onChange={(event) => setSpeedConfig((current) => ({ ...current, speedOperation: event.target.value }))}><option value="auto">全部</option><option value="read">读取</option><option value="write">写入</option><option value="mixed">混合 70/30</option></select></label>
            </div>
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
      {mode === "speed" && speedSettingsOpen && <SpeedSettingsDialog
        speedConfig={speedConfig}
        health={health}
        speedTarget={speedTarget}
        controlsLocked={controlsLocked}
        triggerRef={speedSettingsTriggerRef}
        onClose={() => setSpeedSettingsOpen(false)}
        onApply={(draft) => {
          if (controlsLocked) return;
          setSpeedConfig((current) => ({ ...current,
            speedIOEngine: draft.speedIOEngine,
            speedQueueDepth: draft.speedIOEngine === "psync" ? 1 : draft.speedQueueDepth,
            speedThreads: draft.speedThreads,
            speedDirectIO: draft.speedDirectIO,
            speedTrim: draft.speedTrim
          }));
          setSpeedSettingsOpen(false);
        }}
      />}
    </section>
  );
}

function ScanIssueBrowser({ job }) {
  const [offset, setOffset] = useState(0);
  const [revision, setRevision] = useState(0);
  const [page, setPage] = useState(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const limit = 50;
  const logicalSectorSize = Number(job.disk?.logicalSectorSize) || 512;
  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError("");
    api(`/jobs/${job.id}/scan-issues?offset=${offset}&limit=${limit}`, { signal: controller.signal })
      .then((response) => {
        if (!validScanIssuePage(response.issues, job.disk.size, offset)) throw new Error("异常明细响应不完整");
        if (!controller.signal.aborted) setPage(response.issues);
      })
      .catch((err) => { if (!controller.signal.aborted) setError(errorMessage(err)); })
      .finally(() => { if (!controller.signal.aborted) setLoading(false); });
    return () => controller.abort();
  }, [job.id, job.disk.size, offset, revision]);
  const outcome = scanOutcomeSummary(job);
  const count = outcome?.issueCount ?? job.result?.stats?.badRanges?.length ?? 0;
  if (!count) return null;
  const current = page?.offset === offset ? page : null;
  const totalPages = Math.max(1, Math.ceil((current?.total ?? count) / limit));
  const currentPage = Math.floor(offset / limit) + 1;
  return (
    <div className="scan-issues-body" aria-busy={loading}>
        <div className="scan-issues-toolbar">
          <span>{current ? `第 ${currentPage} / ${totalPages} 页 · 共 ${formatInteger(current.total)} 条` : "正在读取明细"}</span>
          <button type="button" className="button secondary" disabled={loading} onClick={() => setRevision((value) => value + 1)}>刷新明细</button>
        </div>
        {loading && <p role="status"><LoaderCircle className="spin" size={14} aria-hidden="true" /> 正在加载异常明细…</p>}
        {error && <div className="inline-alert warning" role="alert"><AlertTriangle size={16} /><span>{error}</span></div>}
        {current && <>
          {(current.legacy || !current.complete) && <p className="scan-record-note">{current.legacy ? "旧记录仅保留原有摘要，无法确认异常地址是否完整。" : "异常记录不完整，请同时查看报告中的保存告警。"}</p>}
          <div className="scan-issues-table-wrap">
            <table className="scan-issues-table">
              <caption className="sr-only">扫描异常范围及读取结论</caption>
              <thead><tr><th scope="col">LBA 范围</th><th scope="col">读取结论</th><th scope="col">错误证据</th><th scope="col">定位单位</th></tr></thead>
              <tbody>{current.records.map((range, index) => <tr key={`${offset + index}`}>
                <td>{formatInteger(Math.floor(range.offset / logicalSectorSize))}–{formatInteger(Math.ceil((range.offset + range.length) / logicalSectorSize) - 1)}</td>
                <td>{scanRangeOutcomeLabel(range)}</td>
                <td title={range.repair?.detail || range.code}>{range.repair ? `首次 ${formatMilliseconds(range.repair.beforeMs)}${range.repair.afterMs != null ? ` · 写回后 ${formatMilliseconds(range.repair.afterMs)}` : ""}${range.repair.detail ? ` · ${range.repair.detail}` : ""}` : (range.code === "EIO" ? "I/O 错误，原因未细分" : range.code === "TIMEOUT" ? "读取超时" : range.code === "TRANSPORT_ERROR" ? "连接或传输错误" : range.code === "MEDIUM_ERROR" ? "设备报告介质错误" : range.code === "SHORT_READ" ? "未完整读取" : range.code)}</td>
                <td>{range.probeBytes > 0 ? formatBinaryBytes(range.probeBytes) : "请求范围"}</td>
              </tr>)}</tbody>
            </table>
          </div>
          {!current.records.length && <p>当前没有异常记录。</p>}
          <nav className="scan-issues-pagination" aria-label="异常明细分页">
            <button type="button" className="button secondary" disabled={loading || offset === 0} onClick={() => setOffset(0)}>首页</button>
            <button type="button" className="button secondary" disabled={loading || offset === 0} onClick={() => setOffset(Math.max(0, offset - limit))}>上一页</button>
            <span aria-live="polite">{currentPage} / {totalPages}</span>
            <button type="button" className="button secondary" disabled={loading || current.next >= current.total} onClick={() => setOffset(current.next)}>下一页</button>
            <button type="button" className="button secondary" disabled={loading || currentPage >= totalPages} onClick={() => setOffset((totalPages - 1) * limit)}>末页</button>
          </nav>
        </>}
        <p className="scan-record-note">明细显示本次加载时的记录。失败范围按定位精度记录，不能推断范围内每个扇区都已损坏。</p>
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

function ScanEvidenceDetails({ diagnostics }) {
  if (!diagnostics) return null;
  const before = diagnostics.before;
  const after = diagnostics.after;
  const evidence = after || before;
  if (!evidence) return null;
  const warnings = [...new Set([...(before?.warnings || []), ...(after?.warnings || [])])];
  if (diagnostics.selfTest && ["failed", "unknown"].includes(diagnostics.selfTest.state)) warnings.push(diagnostics.selfTest.message);
  const increased = diagnostics.increased || [];
  return <section className="scan-evidence" aria-label="扫描健康信息">
    <p className={`scan-evidence-summary${warnings.length || increased.length ? " value-danger" : ""}`}>
      SMART／日志：{diagnostics.readMethod === "mock" ? "本地演示，未查询真实硬盘" : increased.length ? "扫描期间计数增长" : warnings.length ? "存在异常或历史记录" : evidence.available ? "未见已解析项目告警" : "信息不可用"}
      {!after && "（扫描前）"}
    </p>
    <div className="scan-evidence-body">
      <p>{diagnostics.readMethod === "nvme-read" ? "NVMe 原生读取，记录当前命名空间的完成状态；不使用机械盘相对耗时规则。" : diagnostics.readMethod === "scsi-read" ? "机械盘协议读取，解析命令返回的介质、连接和超时错误。" : diagnostics.readMethod === "direct" ? "直接读取，绕过系统页缓存。" : "演示或非直接读取环境。"}读取结果与硬盘健康信息分别判断；读取成功不代表写入正常，固件缓存与已重映射物理扇区仍由硬盘管理。</p>
      {diagnostics.readNote && <p>{diagnostics.readNote}</p>}
      {diagnostics.nvme?.nsid > 0 && <p>当前 NSID：{diagnostics.nvme.nsid} · 固件扩展自检：{diagnostics.nvme.selfTestSupported ? "支持" : "不支持或未确认"}</p>}
      {diagnostics.selfTest && <p>本次固件自检：{diagnostics.selfTest.message}{diagnostics.selfTest.state === "running" ? ` · ${diagnostics.selfTest.percent}%` : ""}{Number.isSafeInteger(diagnostics.selfTest.lba) && ` · NSID ${diagnostics.selfTest.nsid} / LBA ${formatInteger(diagnostics.selfTest.lba)}（固件报告线索）`}</p>}
      {increased.length > 0 && <p className="value-danger">扫描期间增长：{increased.map((key) => SCAN_COUNTER_LABELS[key] || key).join("、")}</p>}
      {warnings.length > 0 && <ul>{warnings.map((warning) => <li key={warning}>{warning}</li>)}</ul>}
      <dl className="region-detail-grid">
        {Object.entries(SCAN_COUNTER_LABELS).filter(([key]) => before?.counters?.[key] != null || after?.counters?.[key] != null).map(([key, label]) =>
          <div key={key}><dt>{label}（前 → 后）</dt><dd>{before?.counters?.[key] ?? "未知"} → {after?.counters?.[key] ?? "尚未取得"}</dd></div>)}
      </dl>
      {[ ["扫描前", before], ["扫描后", after] ].filter(([, value]) => value).map(([label, value]) => <div key={label}>
        <p>{label}：{value.note || (value.logAvailable ? "已读取硬盘日志" : "日志不可用")}</p>
        {(value.logHints || []).map((hint, index) => <p key={index}>{hint.source}{hint.nsid > 0 && ` · NSID ${hint.nsid}`} · LBA {formatInteger(hint.lba)}（历史线索，不等于本次确认坏道）</p>)}
      </div>)}
    </div>
  </section>;
}

function SmartInfoDialog({ disk, onClose }) {
  const [refreshKey, setRefreshKey] = useState(0);
  const [state, setState] = useState({ loading: true, report: null, error: "" });
  useEffect(() => {
    const controller = new AbortController();
    setState({ loading: true, report: null, error: "" });
    api(`/disks/smart?${new URLSearchParams({ device: disk.path })}`, { signal: controller.signal, timeoutMs: 35_000 })
      .then((data) => {
        if (controller.signal.aborted) return;
        const report = data.smart;
        if (!isRecord(report) || report.device !== disk.path || typeof report.output !== "string" || !report.output.trim()) {
          throw new Error("SMART 信息响应格式不正确");
        }
        setState({ loading: false, report, error: "" });
      })
      .catch((error) => {
        if (!controller.signal.aborted) setState({ loading: false, report: null, error: errorMessage(error) });
      });
    return () => controller.abort();
  }, [disk.path, refreshKey]);
  const report = state.report;
  return <ScanResultDialog title="原始 SMART 信息" className="smart-report-dialog" onClose={onClose} actions={
    <button type="button" className="button secondary" disabled={state.loading} onClick={() => setRefreshKey((value) => value + 1)}><RefreshCw size={15} aria-hidden="true" />{state.error ? "重试" : "刷新"}</button>
  }>
    <p className="smart-report-meta">{report?.model || disk.model} · {disk.path}{report?.capturedAt && <span>{formatTime(report.capturedAt)}</span>}</p>
    {state.loading && <p className="smart-report-loading" role="status"><LoaderCircle size={18} className="spin" aria-hidden="true" />正在读取 SMART 信息…</p>}
    {state.error && <p className="inline-alert danger" role="alert">{state.error}</p>}
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
        {reportUrl && <a className="button secondary" href={reportUrl} download>导出扫描报告</a>}
        <button type="button" className="button secondary" onClick={onClose}>关闭</button>
      </div>
    </dialog>
  );
}

const ScanCellButton = React.memo(function ScanCellButton({ cell, columnCount, inspected, pinned, keyboard, revealed, writeMode }) {
  const displayStatus = revealed && cell.status !== "reading" ? scanStatusGroup(cell.status, cell.maxUnitMs) : "pending";
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
      title={`区域 ${cell.index + 1} · ${formatDiskOffset(cell.start)}–${formatDiskOffset(cell.end)}\n状态：${scanStatusLabel(displayStatus)}${!revealed || cell.maxMs == null ? "" : `\n最大响应：${cell.maxMs} ms\n平均响应：${cell.avgMs} ms\n已读：${formatBytes(cell.bytesRead)}`}${revealed && cell.errorCount ? `\n${writeMode ? "读写/校验错误" : "读取异常"}：${cell.errorCount}` : ""}`}
    />
  );
});

function ScanBlockMap({ job, onPlaybackChange, onOpenHistory }) {
  const writeLatency = ["hdd-write-dual-v2", "solid-write-dual-v2"].includes(job.result?.latencyBasis);
  const dualLatency = writeLatency || ["hdd-dual-v2", "solid-dual-v2"].includes(job.result?.latencyBasis);
  const hddLatency = dualLatency || job.result?.latencyBasis === "hdd-relative-v1";
  const rotationalLatency = ["hdd-dual-v2", "hdd-write-dual-v2"].includes(job.result?.latencyBasis);
  const requestThresholds = rotationalLatency
    ? `HDD 单次读取 ≥50 ms 一般、≥150 ms 较差、≥500 ms 严重${writeLatency ? "" : "；另结合近期同长度读取基准，3 倍一般、10 倍严重"}`
    : "SSD/NVMe 等非机械设备单次读取 ≥20 ms 一般、≥50 ms 较差、≥100 ms 严重";
  const writeThresholds = rotationalLatency
    ? "HDD 单次写入 ≥100 ms 一般、≥300 ms 较差、≥1000 ms 严重"
    : "SSD/NVMe 等非机械设备单次写入 ≥50 ms 一般、≥150 ms 较差、≥500 ms 严重";
  const rawMap = job.result?.map;
  const map = safeScanMap(rawMap);
  const stats = job.result?.stats || {};
  const outcomeSummary = scanOutcomeSummary(job);
  const panelRef = useRef(null);
  const mapRef = useRef(null);
  const followPausedUntilRef = useRef(0);
  const detailDialogRef = useRef(null);
  const [hoveredIndex, setHoveredIndex] = useState(null);
  const [pinnedIndex, setPinnedIndex] = useState(null);
  const [detailIndex, setDetailIndex] = useState(null);
  const [summaryView, setSummaryView] = useState(null);
  const alignPanelRef = useRef(null);

  useLayoutEffect(() => {
    const panel = panelRef.current;
    const dashboard = panel?.closest(".dashboard-grid");
    const options = dashboard?.querySelector(".scan-extra-options");
    const controls = dashboard?.querySelector(".control-column");
    if (!panel || !options || !controls) return;
    const align = () => {
      if (!window.matchMedia("(min-width: 861px)").matches) {
        panel.style.removeProperty("--scan-panel-height");
        return;
      }
      const height = Math.max(0, options.getBoundingClientRect().bottom - panel.getBoundingClientRect().top);
      panel.style.setProperty("--scan-panel-height", `${height}px`);
    };
    alignPanelRef.current = align;
    const observer = new ResizeObserver(align);
    observer.observe(controls);
    observer.observe(options);
    window.addEventListener("resize", align);
    align();
    return () => {
      observer.disconnect();
      window.removeEventListener("resize", align);
      alignPanelRef.current = null;
      panel.style.removeProperty("--scan-panel-height");
    };
  }, [job.id, map.length]);
  useLayoutEffect(() => { alignPanelRef.current?.(); });
  const [revealedPrefix, setRevealedPrefix] = useState(() => terminalScanPrefix(map));
  const revealRef = useRef(null);
  const completedPrefix = terminalScanPrefix(map);
  if (!revealRef.current || revealRef.current.jobId !== job.id || revealRef.current.length !== map.length) {
    revealRef.current = {
      jobId: job.id,
      length: map.length,
      targetPrefix: completedPrefix,
      revealedPrefix: completedPrefix,
      carry: 0,
      lastFrameAt: 0
    };
  } else {
    revealRef.current.targetPrefix = completedPrefix;
  }

  // A single frame-driven queue avoids thousands of independent CSS timers.
  // Small real-time batches advance one cell at a time; large SSD batches catch
  // up at a bounded, continuous rate without periodic multi-column jumps.
  useEffect(() => {
    let frame = 0;
    const tick = (now) => {
      const reveal = revealRef.current;
      if (!reveal || reveal.jobId !== job.id) return;
      if (!reveal.lastFrameAt) reveal.lastFrameAt = now;
      const next = advanceScanReveal(
        reveal.revealedPrefix,
        reveal.targetPrefix,
        now - reveal.lastFrameAt,
        reveal.carry
      );
      reveal.lastFrameAt = now;
      reveal.carry = next.carry;
      if (next.prefix !== reveal.revealedPrefix) {
        reveal.revealedPrefix = next.prefix;
        setRevealedPrefix(next.prefix);
      }
      if (reveal.revealedPrefix < reveal.targetPrefix) frame = window.requestAnimationFrame(tick);
    };
    frame = window.requestAnimationFrame(tick);
    return () => window.cancelAnimationFrame(frame);
  }, [job.id, map.length, completedPrefix]);
  useEffect(() => {
    setRevealedPrefix(completedPrefix);
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

  const visiblePrefix = Math.min(revealedPrefix, completedPrefix, map.length);
  const playbackActive = visiblePrefix < completedPrefix;
  // Report before paint so the final backend update cannot briefly enable Start
  // while the visible map is still catching up. Unmounting ends this playback.
  useLayoutEffect(() => {
    onPlaybackChange(job.id, playbackActive);
  }, [job.id, playbackActive, onPlaybackChange]);
  useLayoutEffect(() => () => onPlaybackChange(job.id, false), [job.id, onPlaybackChange]);
  const followIndex = (ACTIVE.has(job.status) || playbackActive) && visiblePrefix < map.length
    ? visiblePrefix
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
    const status = cell.index < visiblePrefix && cell.status !== "reading" ? scanStatusGroup(cell.status, cell.maxUnitMs) : "pending";
    counts[status] = (counts[status] || 0) + 1;
    return counts;
  }, {});
  const severeOrErrorCount = (statusCounts.severe || 0) + (statusCounts.error || 0);
  const visibleReadBytes = map.slice(0, visiblePrefix).reduce((total, cell) => total + (cell.bytesRead || 0), 0);
  const readBytes = Number.isSafeInteger(stats.bytesRead) && stats.bytesRead >= 0 ? stats.bytesRead : visibleReadBytes;
  const sampledScan = job.result.coverage === "sampled";
  const scanTypeLabel = job.type === "write" ? "深度检测（旧记录）" : sampledScan ? "抽样检测" : "全盘检测";
  const readMethodLabel = job.type === "write" ? "每格写读校验" : sampledScan ? `${job.result.samplingPlan === "stratified-v1" ? "每格分散抽读" : "每格抽读"} ${formatBinaryBytes(sampleBytes)}` : "完整读取每格";
  const issueCount = outcomeSummary?.issueCount ?? stats.badRanges?.length ?? 0;
  const diagnostics = job.result?.diagnostics;
  const hasEvidence = Boolean(diagnostics?.before || diagnostics?.after);
  const evidenceHasWarnings = Boolean(diagnostics?.increased?.length || diagnostics?.before?.warnings?.length || diagnostics?.after?.warnings?.length || ["failed", "unknown"].includes(diagnostics?.selfTest?.state));
  const reportUrl = !job.id.startsWith("idle-") && job.status !== "idle" ? `${API}/jobs/${job.id}/scan-report` : "";
  const scanErrorLabel = job.type === "write" ? "读写/校验错误" : "读取异常";
	const liveCell = (ACTIVE.has(job.status) || playbackActive) && visiblePrefix < map.length
	  ? map[visiblePrefix]
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
  const cellBadRanges = badRanges.filter((range) => (
    Number.isFinite(range?.offset)
    && Number.isFinite(range?.length)
    && range.length > 0
    && range.offset < inspectedCell.end
    && range.offset + range.length > inspectedCell.start
  ));
  const firstBadRangeLba = cellBadRanges.length ? Math.floor(cellBadRanges[0].offset / logicalSectorSize) : null;

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
    <div ref={panelRef} className="scan-map-panel">
      <div className="scan-map-layout">
        <div className="map-visual">
          <div
            ref={mapRef}
            className={`block-map${map.length === QUICK_SCAN_CELL_COUNT ? " dense" : ""}`}
            style={{ "--map-columns": columnCount }}
            role="grid"
            aria-label={`磁盘扫描块图，已扫描 ${stats.testedCells || 0} 个区域，${stats.errorCells || 0} 个异常区域`}
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
                revealed={cell.index < visiblePrefix}
                writeMode={job.type === "write"}
              />
            ))}
          </div>
          <div className="block-legend" aria-label="区域检测结果图例">
            {SCAN_LEGEND.slice(0, 5).map(([status, label, description]) => <span key={status} title={dualLatency ? `传输折算档位：${description}；${requestThresholds}${writeLatency ? `；${writeThresholds}` : ""}。请求不超过 128 KiB，记录主机读写等待时间；传输折算与单次请求分别判断，取较严重结果，读写及校验错误优先。慢响应不等于坏道。` : hddLatency ? `${description}；另结合近期同长度读取的中位基准，3 倍标一般、10 倍标严重，单次等待 500 ms 也标一般。保留最差结果；慢读不等于坏道，异常原因见区域详情。` : job.result?.latencyBasis === "per-mib-v1" || job.status === "idle" ? `${description}。每次传输按 1 MiB 折算，不足 1 MiB 按 1 MiB；取区域内最差值。深度检测计入写入与读回总量。正常仅代表本次检测未见异常。` : "旧记录保留原耗时判定；重新扫描使用按传输量折算的新规则。"}><i className={`block-${status}`} />{label}<b>{statusCounts[status] || 0}</b></span>)}
            <span className="block-legend-combined" title={`严重 ${statusCounts.severe || 0} · 损坏（${scanErrorLabel}）${statusCounts.error || 0}`}>
              <i className="block-error" aria-hidden="true" />严重<b>{severeOrErrorCount}</b>
            </span>
            <span><i className="block-pending" />未扫描<b>{(statusCounts.pending || 0) + (statusCounts.reading || 0)}</b></span>
          </div>
        </div>
        <aside className="map-summary" aria-label="扫描关键统计">
          <dl className="scan-map-stats">
            <div><dt>已处理区域</dt><dd>{visiblePrefix} / {map.length}</dd></div>
            <div><dt>{job.type === "write" ? "写入量" : "读取量"}</dt><dd>{formatBytes(readBytes)}</dd></div>
            <div><dt>慢速区域</dt><dd>{(statusCounts.slow || 0) + (statusCounts.slower || 0) + (statusCounts.severe || 0)}</dd></div>
            <div><dt>异常区域</dt><dd className={statusCounts.error ? "value-danger" : ""}>{statusCounts.error || 0}</dd></div>
          </dl>
        </aside>
      </div>
      <div className="scan-detail-toolbar" aria-label="扫描详细信息">
        <span className="scan-type-label">{scanTypeLabel}</span>
        <div className="scan-detail-actions">
          <button type="button" className="scan-detail-button" aria-haspopup="dialog" onClick={() => setSummaryView("overview")}>扫描详情</button>
          {job.type === "scan" && <button type="button" className={`scan-detail-button${issueCount > 0 ? " value-danger" : ""}`} aria-haspopup="dialog" onClick={() => setSummaryView("issues")}>异常明细{issueCount > 0 ? ` · ${formatInteger(issueCount)}` : ""}</button>}
          <button type="button" className={`scan-detail-button${evidenceHasWarnings ? " value-danger" : ""}`} aria-haspopup="dialog" onClick={() => setSummaryView("evidence")}>SMART／日志{evidenceHasWarnings ? " · 告警" : ""}</button>
          <HistoryTrigger onOpen={onOpenHistory} inline />
        </div>
      </div>
      {stats.issueWarning && <div className="inline-alert warning" role="alert"><AlertTriangle size={16} /><span>{stats.issueWarning}</span></div>}
      {summaryView && <ScanResultDialog title={{ overview: "扫描详情", issues: "异常明细", evidence: "SMART／日志" }[summaryView]} onClose={() => setSummaryView(null)} reportUrl={reportUrl}>
        {summaryView === "overview" && <>
          <dl className="scan-detail-grid">
            <div><dt>检测方式</dt><dd>{scanTypeLabel}</dd></div>
            <div><dt>区域数量</dt><dd>{formatInteger(map.length)} 格</dd></div>
            <div><dt>单格容量</dt><dd>约 {formatBytes(bytesPerCell)}</dd></div>
            <div><dt>读取方式</dt><dd>{readMethodLabel}</dd></div>
            <div><dt>任务进度</dt><dd>{((visiblePrefix / map.length) * 100).toFixed(1)}%<small>{formatInteger(visiblePrefix)} / {formatInteger(map.length)} 个区域</small></dd></div>
            <div><dt>{job.type === "write" ? "写入量" : "读取量"}</dt><dd>{formatBytes(readBytes)}</dd></div>
          </dl>
          {outcomeSummary && <section className="scan-coverage-detail" aria-labelledby="scan-coverage-title">
            <h3 id="scan-coverage-title">已判定范围占整盘 <strong>{diskSize > 0 ? ((outcomeSummary.coveredBytes / diskSize) * 100).toFixed(3) : "0.000"}%</strong></h3>
            <p>{sampledScan ? "抽样扫描只检查每个区域中的样本；任务完成后，也不代表整盘数据都已读取。" : "全盘扫描逐段读取磁盘；未完成或中断的任务仍有未检测范围。"}覆盖率包含已读出和已确认持续不可读的范围，不包含定位达到限额的待细查范围，与任务进度不同。</p>
          </section>}
          {outcomeSummary && <dl className="scan-outcome-stats">
            <div title="细分定位后仍未完整读出的范围；范围数不等于物理坏扇区数"><dt>持续不可读</dt><dd className={outcomeSummary.unreadableRanges ? "value-danger" : ""}>{formatInteger(outcomeSummary.unreadableRanges)} 个范围 · {formatBinaryBytes(outcomeSummary.unreadableBytes)}</dd></div>
            <div title="复读恢复与固件已纠正记录，保留首次告警"><dt>恢复告警</dt><dd>{formatInteger(outcomeSummary.recoveredRanges)} 条</dd></div>
            <div title="超时、连接、控制器等错误，不能据此判定介质坏区"><dt>设备异常</dt><dd>{formatInteger(outcomeSummary.deviceErrorRanges)} 条</dd></div>
            {outcomeSummary.unresolvedRanges > 0 && <div><dt>范围未确定</dt><dd>{formatInteger(outcomeSummary.unresolvedRanges)} 条{outcomeSummary.unresolvedBytes > 0 && <> · {formatBinaryBytes(outcomeSummary.unresolvedBytes)} 待细查</>}</dd></div>}
          </dl>}
        </>}
        {summaryView === "issues" && (issueCount > 0
          ? <ScanIssueBrowser key={job.id} job={job} />
          : <p className="scan-record-note">{job.status === "idle" ? "尚未开始扫描，暂无异常明细。" : "本次扫描尚未记录到异常。"}</p>)}
        {summaryView === "evidence" && (hasEvidence
          ? <ScanEvidenceDetails diagnostics={diagnostics} />
          : <p className="scan-record-note">{job.status === "idle" ? "尚未开始扫描，暂无本次扫描的 SMART／日志。" : "本次扫描暂无可用的 SMART／日志。"}</p>)}
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
          <span className={`dialog-icon region-dialog-swatch block-${scanStatusGroup(inspectedCell.status, inspectedCell.maxUnitMs)}`} aria-hidden="true" />
          <div><p className="eyebrow">区域详情</p><h2 id="region-detail-title">区域 {inspectedCell.index + 1}</h2></div>
          <button className="icon-button" onClick={closeDetail} aria-label="关闭区域详情" title="关闭"><X size={20} /></button>
        </div>
        <div className="dialog-body">
          <div className="region-dialog-status"><span>{scanStatusLabel(scanStatusGroup(inspectedCell.status, inspectedCell.maxUnitMs))}</span><small>磁盘位置 {positionPercent.toFixed(2)}%</small></div>
          <dl className="region-detail-grid">
            <div><dt>容量范围</dt><dd>{formatDiskOffset(inspectedCell.start)} - {formatDiskOffset(inspectedCell.end)}</dd></div>
            <div><dt>LBA 范围</dt><dd>{formatInteger(startLba)} - {formatInteger(endLba)}</dd></div>
            <div><dt>逻辑柱面</dt><dd>{formatInteger(logicalCylinder)}</dd></div>
            <div><dt>{writeLatency ? "整次写读最大耗时" : dualLatency ? "整次读取最大耗时" : "最大响应"}</dt><dd>{formatMilliseconds(inspectedCell.maxMs)}</dd></div>
            <div><dt>{writeLatency ? "整次写读平均耗时" : dualLatency ? "整次读取平均耗时" : "平均响应"}</dt><dd>{formatMilliseconds(inspectedCell.avgMs)}</dd></div>
            {inspectedCell.maxWriteLatency && <>
              <div title="不超过 128 KiB 的主机写入请求耗时，包含驱动与调度等待，不代表断电后数据已持久保存"><dt>最慢单次写入</dt><dd>{formatMilliseconds(inspectedCell.maxWriteLatency.ms)} · {formatBytes(inspectedCell.maxWriteLatency.bytes)}</dd></div>
              <div title={writeThresholds}><dt>最慢写入 LBA</dt><dd>{formatInteger(Math.floor(inspectedCell.maxWriteLatency.offset / logicalSectorSize))}</dd></div>
            </>}
            {inspectedCell.maxReadLatency && <>
              <div title="不超过 128 KiB 的主机读取请求耗时，包含驱动与调度等待，不是纯固件服务时间"><dt>{job.type === "write" ? "最慢单次读回" : "最慢单次读取"}</dt><dd>{formatMilliseconds(inspectedCell.maxReadLatency.ms)} · {formatBytes(inspectedCell.maxReadLatency.bytes)}</dd></div>
              <div title={requestThresholds}><dt>{job.type === "write" ? "最慢读回 LBA" : "最慢读取 LBA"}</dt><dd>{formatInteger(Math.floor(inspectedCell.maxReadLatency.offset / logicalSectorSize))}</dd></div>
            </>}
            {inspectedCell.latencyWarning && <div><dt>响应异常依据</dt><dd>{inspectedCell.latencyWarning === "stall" ? "单次读取等待达到 500 ms" : `达到近期同长度读取基准的 ${Number(inspectedCell.maxLatencyRatio).toFixed(1)} 倍`}（慢读不等于坏道）</dd></div>}
            {Number.isFinite(inspectedCell.maxUnitMs) && <div title="每次传输按 1 MiB 折算，不足 1 MiB 按 1 MiB，取区域内最差值；原始响应时间保持不变"><dt>{dualLatency ? "传输折算耗时" : "判色折算耗时"}</dt><dd>{formatMilliseconds(inspectedCell.maxUnitMs)}</dd></div>}
            <div><dt>{job.type === "write" ? "实际写入" : "实际读取"}</dt><dd>{formatBytes(inspectedCell.bytesRead)}</dd></div>
            <div><dt>{job.result.coverage === "sampled" ? "抽读 LBA" : job.type === "write" ? "写读轮次" : "读取次数"}</dt><dd>{job.result.coverage === "sampled" ? sampleLbaLabel : formatInteger(inspectedCell.reads || 0)}</dd></div>
            <div><dt>{job.type === "write" ? "读写/校验错误" : "读取异常记录"}</dt><dd className={inspectedCell.errorCount ? "value-danger" : ""}>{inspectedCell.errorCount ? `${inspectedCell.errorCount}${firstBadRangeLba == null ? "" : ` · LBA ${formatInteger(firstBadRangeLba)}`}` : "0"}</dd></div>
            {outcomeSummary && <>
              <div><dt>读取结论</dt><dd>{{ readable: "完整读出", recovered: "复读恢复，保留告警", unreadable: "存在持续不可读范围", device_error: "设备异常，不能判定介质坏区", unresolved: "异常范围未确定", partial: "部分读取，尚未完成" }[inspectedCell.readOutcome] || "尚未完成"}</dd></div>
              <div><dt>已判定覆盖</dt><dd>{formatBinaryBytes(inspectedCell.coveredBytes || 0)}{job.result.coverage === "full" && (inspectedCell.coveredBytes || 0) < inspectedCell.end - inspectedCell.start ? " · 未覆盖完整区域" : ""}</dd></div>
            </>}
            {inspectedCell.load?.backgroundActivity && <div><dt>扫描时磁盘活动</dt><dd>检测到并发磁盘活动，首次慢响应保留；不能仅凭耗时判定介质异常。</dd></div>}
            {inspectedCell.unresolvedBytes > 0 && <div><dt>待细查范围</dt><dd>{formatBinaryBytes(inspectedCell.unresolvedBytes)} · 达到定位限额，可使用深入定位重新检测</dd></div>}
            {inspectedCell.recheck && <>
              <div><dt>慢速只读复测</dt><dd>{{ not_reproduced: "未触发复测慢速阈值（保留首次响应）", slow: "复测仍然偏慢", read_issue: "复测出现读取异常" }[inspectedCell.recheck.outcome] || "已复测"}</dd></div>
              {inspectedCell.recheck.load?.backgroundActivity && <div><dt>复测环境</dt><dd>复测期间仍有并发磁盘活动，耗时结论需空闲时确认</dd></div>}
              <div><dt>复测最差折算耗时</dt><dd>{formatMilliseconds(inspectedCell.recheck.maxUnitMs)}</dd></div>
              {inspectedCell.recheck.maxReadLatency && <div><dt>复测最慢单次读取</dt><dd>{formatMilliseconds(inspectedCell.recheck.maxReadLatency.ms)} · {formatBytes(inspectedCell.recheck.maxReadLatency.bytes)}</dd></div>}
              <div><dt>额外复测读取量</dt><dd>{formatBytes(inspectedCell.recheck.bytesRead)} · {inspectedCell.recheck.reads} 次</dd></div>
            </>}
            {inspectedCell.repair && <div><dt>慢扇区写回</dt><dd>{inspectedCell.repair.verified + inspectedCell.repair.stillSlow} 个数据校验通过 · {inspectedCell.repair.stillSlow} 个仍慢 · {inspectedCell.repair.skipped} 个跳过 · {inspectedCell.repair.failed} 个失败 · {ACTIVE.has(job.status) ? "处理中" : "结果未确定"} {inspectedCell.repair.pending} 个</dd></div>}
            {cellBadRanges.length > 0 && (
              <div>
                <dt>异常明细</dt>
                <dd>
                  {cellBadRanges.slice(0, 20).map((range, index) => {
                    const start = Math.floor(range.offset / logicalSectorSize);
                    const end = Math.max(start, Math.ceil((range.offset + range.length) / logicalSectorSize) - 1);
                    const recovery = range.code === "RECOVERED_ERROR" && range.recovered === true ? " · 固件已纠正（保留告警）" : range.recovered === true ? " · 复读恢复（保留首次异常）"
                      : range.recovered === false ? " · 复读仍未完全读取" : "";
                    const labels = { MEDIUM_ERROR: "设备报告介质错误", RECOVERED_ERROR: "固件报告已纠正错误", TRANSPORT_ERROR: "连接或传输错误", TIMEOUT: "读取超时", HARDWARE_ERROR: "设备硬件错误", CONTROLLER_ERROR: "控制器错误", NOT_READY: "设备未就绪", UNIT_ATTENTION: "设备状态变化", COMMAND_ABORTED: "读取命令中止", COMMAND_ERROR: "读取命令失败", ILLEGAL_REQUEST: "设备拒绝读取命令", DEVICE_ERROR: "设备报告错误", EIO: "I/O 读取错误（原因未细分）", READ_RETRY: "首次读取异常", SHORT_READ: "未完整读取" };
                    const evidence = range.evidence;
                    labels.WRITE_ERROR = "写入失败";
                    labels.READ_AFTER_WRITE_ERROR = "写入后读回失败";
                    labels.WRITE_VERIFY_MISMATCH = "读回数据与写入内容不一致";
                    labels.DEFERRED_ERROR = "设备延迟报告错误（不能归因于当前扇区）";
                    Object.assign(labels, { DATA_INTEGRITY_ERROR: "NVMe 数据完整性错误", UNWRITTEN_LBA: "未分配或未写入区域（不等于介质坏块）", ACCESS_DENIED: "设备拒绝访问" });
                    return <React.Fragment key={`${range.offset}-${range.length}-${index}`}>{index > 0 && <br />}LBA {formatInteger(start)}–{formatInteger(end)} · {labels[range.code] || range.code || labels.EIO} · {formatBinaryBytes(range.length)}{recovery}{evidence?.protocol === "nvme" ? ` · NSID ${evidence.nsid}${evidence.nvmeStatus > 0 ? ` / SCT ${evidence.statusCodeType ?? 0} / SC 0x${Number(evidence.statusCode ?? 0).toString(16)}${evidence.doNotRetry ? " / DNR" : ""}` : " / 未取得设备完成状态"}` : evidence && ` · sense ${[evidence.senseKey, evidence.asc, evidence.ascq].map((value) => Number(value).toString(16).padStart(2, "0")).join("/")}`}{Number.isSafeInteger(evidence?.lba) && ` · 设备返回 LBA ${formatInteger(evidence.lba)}（定位线索）`}</React.Fragment>;
                  })}
                  {(cellBadRanges.length > 20 || stats.issuesTruncated) && <p className="scan-record-note">此处展示部分摘要，完整地址请打开异常明细或导出扫描报告。</p>}
                </dd>
              </div>
            )}
          </dl>
          {inspectedCell.errorCount > 0 && (
            <div className="inline-alert danger region-dialog-alert"><AlertTriangle size={17} /><span>{cellBadRanges.length ? `摘要包含 ${cellBadRanges.length} 条异常范围，首个位置约为 LBA ${formatInteger(firstBadRangeLba)}。异常不代表整个区域不可读。` : outcomeSummary ? "该区域发生过读取异常，完整地址请查看异常明细或导出扫描报告。" : "旧记录未保留该区域的完整异常地址，不能据此确定坏区范围。"}</span></div>
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

function configuredSpeedRows(config) {
  const blockSize = [4096, 65536, 1048576, 4194304].includes(config.speedBlockSize) ? config.speedBlockSize : 1048576;
  const queueDepth = config.speedIOEngine === "psync" ? 1 : ([1, 8, 16, 32, 64, 128].includes(config.speedQueueDepth) ? config.speedQueueDepth : 64);
  const threads = [1, 2, 4, 8].includes(config.speedThreads) ? config.speedThreads : 1;
  const seqCode = `SEQ${speedBlockCode(blockSize)}`;
  return [
    { key: `seq-q${queueDepth}t${threads}`, profileRow: "seq-high", pattern: "sequential", blockSize, queueDepth, threads, code: seqCode, subcode: `Q${queueDepth}T${threads}`, title: `${seqCode} Q${queueDepth}T${threads}` },
    { key: `seq-q1t1-${blockSize}`, profileRow: "seq-low", pattern: "sequential", blockSize, queueDepth: 1, threads: 1, code: seqCode, subcode: "Q1T1", title: `${seqCode} Q1T1` },
    { key: `rnd-q${queueDepth}t${threads}-high`, profileRow: "rnd-high", pattern: "random", blockSize: 4096, queueDepth, threads, code: "RND4K", subcode: `Q${queueDepth}T${threads}`, title: `RND4K Q${queueDepth}T${threads}` },
    { key: "rnd-q1t1-base", profileRow: "rnd-low", pattern: "random", blockSize: 4096, queueDepth: 1, threads: 1, code: "RND4K", subcode: "Q1T1", title: "RND4K Q1T1" }
  ];
}

const SPEED_COLUMNS = [
  { operation: "read", label: "Read" },
  { operation: "write", label: "Write" },
  { operation: "mixed", label: "Mix" }
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
    : `已完成${cached ? " · 缓存" : ""}`;
}

function SpeedBenchmark({ job, config, onStart, startDisabled }) {
  const measured = Array.isArray(job.result?.benchmarks) ? job.result.benchmarks : [];
  const completedResultValid = job.status !== "completed" || isCompleteSpeedResult(job);
  const finished = !ACTIVE.has(job.status) && job.status !== "idle";
  const hasModernProfile = job.options?.speedProfile === true || typeof job.options?.speedIOEngine === "string";
  const matrixConfig = job.status === "idle"
    ? config
    : {
        speedBlockSize: job.options?.speedBlockSize ?? config.speedBlockSize,
        speedQueueDepth: job.options?.speedQueueDepth ?? config.speedQueueDepth,
        speedThreads: job.options?.speedThreads ?? config.speedThreads,
        speedIOEngine: job.options?.speedIOEngine ?? config.speedIOEngine
      };
  const speedRows = hasModernProfile || job.status === "idle" ? configuredSpeedRows(matrixConfig) : LEGACY_SPEED_ROWS;
  const cached = job.options?.speedDirectIO === false;
  const findBenchmark = (row, operation) => measured.find((benchmark) => (
    benchmark.operation === operation
      && benchmark.pattern === row.pattern
      && benchmark.blockSize === row.blockSize
      && (benchmark.queueDepth ?? 1) === row.queueDepth
      && (benchmark.threads ?? 1) === row.threads
  )) || { operation, ...row, averageMBps: null, iops: null, samples: [] };
  const startBenchmark = (row) => onStart({
    type: "speed",
    speedRuns: config.speedRuns,
    sampleMiB: config.sampleMiB,
    speedPosition: "all",
    speedIOEngine: config.speedIOEngine,
    speedDirectIO: config.speedDirectIO,
    speedTrim: config.speedTrim,
    speedProfile: true,
    speedProfileRow: row.profileRow,
    ...(config.speedOperation !== "auto" ? { speedOperation: config.speedOperation } : {}),
    ...(row ? {
      speedPattern: row.pattern,
      speedBlockSize: row.blockSize,
      speedQueueDepth: row.queueDepth,
      speedThreads: row.threads
    } : {})
  });

  return (
    <div className="benchmark-panel">
      {job.status === "completed" && !completedResultValid && (
        <div className="inline-alert warning" role="alert"><AlertTriangle size={18} /><span>历史测速明细格式不正确，无法显示部分或全部结果。</span></div>
      )}
      <div className="benchmark-matrix" role="table" aria-label="磁盘读取、写入和混合测速结果">
        <div className="benchmark-matrix-header" role="row">
          <div className="benchmark-corner" role="columnheader" aria-label="测试项目" />
          {SPEED_COLUMNS.map((column) => (
            <div className={`benchmark-column ${column.operation}`} role="columnheader" key={column.operation}>
              <strong>{column.label} (MB/s)</strong>
            </div>
          ))}
        </div>
        {speedRows.map((row) => {
          return (
            <div className="benchmark-matrix-row" role="row" key={row.key}>
              <div className="benchmark-test-cell" role="rowheader">
                <button
                  className="benchmark-test-label"
                  type="button"
                  onClick={() => startBenchmark(row)}
                  disabled={startDisabled}
                  aria-label={`单独运行${row.title}测速`}
                  title={`单独运行 ${row.title}`}
                >
                  <strong>{row.code}</strong>
                  <small>{row.subcode}</small>
                </button>
              </div>
              {SPEED_COLUMNS.map(({ operation }) => {
                  const benchmark = findBenchmark(row, operation);
                  const write = operation === "write";
                  const averageMBps = benchmark.averageMBps;
                  const hasValue = Number.isFinite(averageMBps);
                  const combinationSkipped = (Boolean(job.options?.speedPattern) && job.options.speedPattern !== row.pattern)
                    || (typeof job.options?.speedOperation === "string" && job.options.speedOperation !== operation)
                    || (typeof job.options?.speedProfileRow === "string" && job.options.speedProfileRow !== row.profileRow);
                  const status = hasValue
                    ? "已完成"
                    : (job.status === "idle" ? "尚未开始" : (finished ? (combinationSkipped ? "未执行" : "未完成") : "等待测试"));
                  const detail = hasValue ? benchmarkDetail(benchmark, cached) : status;
                  const formattedValue = hasValue
                    ? averageMBps.toFixed(1)
                    : "--";
                  const accessibleValue = hasValue
                    ? `${formattedValue} MB/s`
                    : status;
                  return (
                    <div
                      className={`benchmark-result-cell ${operation}${hasValue ? "" : " pending"}`}
                      role="cell"
                      key={operation}
                      aria-label={`${row.title}${operation === "read" ? "读取" : (write ? "写入" : "混合")}，${accessibleValue}`}
                    >
                      <strong className="benchmark-result-number">{formattedValue}</strong>
                      <small className="benchmark-result-detail" title={detail}>{detail || status}</small>
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

function ActiveTask({ job, contextDisk, detailState, onReloadDetail, speedConfig, onStartSpeed, speedStartDisabled, onPlaybackChange, onOpenHistory }) {
  if (!job) return null;
  const idle = job.status === "idle";
  const contextMismatch = job.type !== "speed" && !idle && contextDisk && !jobBelongsToDisk(job, contextDisk);
  const scanResultComplete = (job.type !== "scan" && job.type !== "write") || job.status !== "completed" || isCompleteScanResult(job);
  const speedResultComplete = job.type !== "speed" || job.status !== "completed" || isCompleteSpeedResult(job);
  const scanHasErrors = (job.type === "scan" || job.type === "write")
    && job.status === "completed"
    && scanResultComplete
    && Number(job.result?.stats?.errorCells || 0) > 0;
  const resultIncomplete = job.status === "completed" && job.result && (!scanResultComplete || !speedResultComplete);
  return (
    <section className="active-task" aria-label={job.type === "speed" ? "磁盘测速" : (job.type === "write" ? "深度检测" : "磁盘扫描")} title={!idle ? `${job.disk?.model || "未知型号"} · ${job.device || "未知设备"}` : undefined}>
      {scanHasErrors && (
        <div className="completion-result has-errors task-result-alert" role="alert">
          <AlertTriangle size={20} />
          <span>{job.type === "write" ? `发现 ${job.result.stats.errorCells} 个写入或校验异常区域` : `发现 ${job.result.stats.errorCells} 个读取异常区域`}</span>
        </div>
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
      {(job.type === "scan" || job.type === "write") && <ScanBlockMap key={job.id} job={job} onPlaybackChange={onPlaybackChange} onOpenHistory={onOpenHistory} />}
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
      {(job.type === "speed" || !safeScanMap(job.result?.map).length) && <HistoryTrigger onOpen={onOpenHistory} />}
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

function HistoryDialog({ mode, selectedId, onSelect, onDelete, onClose, selectionLocked }) {
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
            {jobs.map((job) => (
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
                      <strong>{job.type === "write" ? "深度检测（旧记录）" : (job.type === "scan" ? (job.options?.scanMode === "full" ? "完整扫描" : "快速扫描") : "速度测试")}</strong>
                      <StatusBadge status={job.status} />
                    </span>
                    <span className="history-record-disk">
                      <strong>{job.device}</strong>
                      <small>{job.disk?.model || "未知型号"}</small>
                    </span>
                    <span className="history-record-result">{job.message || "暂无结果摘要"}</span>
                    <span className="history-record-time">{formatTime(job.startedAt || job.createdAt)}</span>
                  </button>
                  <button
                    className="history-delete-button"
                    type="button"
                    onClick={() => { onClose(); onDelete(job); }}
                    disabled={ACTIVE.has(job.status)}
                    aria-label={`删除${job.device || "此任务"}的历史记录`}
                    title={ACTIVE.has(job.status) ? "运行中的任务不能删除" : "删除历史记录"}
                  >
                    <Trash2 size={15} aria-hidden="true" />
                  </button>
                </div>
              </li>
            ))}
          </ul>
        )}
      </div>
      <div className="dialog-actions"><button type="button" className="button secondary" onClick={onClose}>关闭</button></div>
    </dialog>
  );
}

function DeleteHistoryDialog({ job, deleting, onClose, onConfirm }) {
  const dialogRef = useRef(null);

  useEffect(() => {
    const dialog = dialogRef.current;
    if (job && dialog && !dialog.open) dialog.showModal();
    if (!job && dialog?.open) dialog.close();
  }, [job]);

  if (!job) return null;
  return (
    <dialog
      ref={dialogRef}
      className="confirm-dialog"
      role="alertdialog"
      aria-labelledby="delete-history-title"
      onCancel={(event) => {
        if (deleting) event.preventDefault();
        else onClose();
      }}
    >
      <div className="dialog-title">
        <span className="dialog-icon dialog-icon-danger"><Trash2 size={21} aria-hidden="true" /></span>
        <div><p className="eyebrow">历史记录</p><h2 id="delete-history-title">删除这条记录？</h2></div>
        <button className="icon-button" type="button" onClick={onClose} disabled={deleting} aria-label="关闭" title="关闭"><X size={20} /></button>
      </div>
      <div className="dialog-body delete-history-body">
        <p>将删除 {job.device || "所选设备"} 的这条检测记录，删除后无法恢复。磁盘中的数据不会受到影响。</p>
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
        <button className="icon-button" onClick={onClose} disabled={submitting} aria-label="关闭" title="关闭"><X size={20} /></button>
      </div>
      <div className="dialog-body">
        {!speed && !repair && disk.isSystem && <div className="inline-alert warning"><AlertTriangle size={19} /><span>所选设备包含系统挂载点。任务不会写盘，但会显著增加 I/O 负载。</span></div>}
        <dl className="confirm-specs">
          {speed && !rawSpeed ? <div><dt>{/^\/vol\d+$/.test(storageSpace.path) ? "存储空间" : "外接挂载"}</dt><dd>{storageSpace.path}</dd></div> : <div><dt>设备</dt><dd>{disk.path}</dd></div>}
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
          {speed && <div><dt>测试组合</dt><dd>{request.speedOperation === "read" ? "仅读取" : (request.speedOperation === "write" ? "仅写入" : (request.speedOperation === "mixed" ? "混合 70/30" : "全部组合"))} · {request.speedPattern === "sequential" ? `SEQ${speedBlockCode(request.speedBlockSize || 1048576)} Q${request.speedQueueDepth || 1}T${request.speedThreads || 1}` : (request.speedPattern === "random" ? `RND4K Q${request.speedQueueDepth || 1}T${request.speedThreads || 1}` : "全部测试项目")}</dd></div>}
          {speed && <div><dt>测试次数</dt><dd>{request.speedRuns || 1} 次</dd></div>}
          {speed && <div><dt>每项样本</dt><dd>{formatBinaryBytes((request.sampleMiB || 1024) * 1024 * 1024)}</dd></div>}
          {speed && <div><dt>I/O 引擎</dt><dd>{request.speedIOEngine || "io_uring"} · Q{request.speedQueueDepth || 1}T{request.speedThreads || 1}</dd></div>}
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
        <label className="confirm-check"><input type="checkbox" checked={confirmed} onChange={(event) => setConfirmed(event.target.checked)} /><span>{repair ? "我已核对磁盘、完成独立备份，并了解此次扫描包含实际写入。" : (rawSpeed ? (rawWillWrite ? `我已确认 ${disk.path} 没有需要保留的数据，并了解底层测速会覆写原有数据。` : `我已核对 ${disk.path}，了解这次仅做底层读取测速。`) : (speed ? `我已核对 ${storageSpace.path}，了解测速会创建并自动删除临时测试文件。` : "我已核对设备，了解测试期间磁盘性能会受到影响。"))}</span></label>
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
                    speedRuns: request.speedRuns || 1,
                    speedPosition: request.speedPosition || "all",
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

function App() {
  const [themeChoice, setThemeChoice] = useState(() => {
    try {
      const saved = window.localStorage.getItem("diskbench-theme-choice");
      if (saved === "light" || saved === "dark") return saved;
    } catch {
      // Storage can be unavailable in restricted fnOS webviews.
    }
    return null;
  });
  const [systemDark, setSystemDark] = useState(() => window.matchMedia?.("(prefers-color-scheme: dark)")?.matches ?? false);
  const theme = themeChoice ?? (systemDark ? "dark" : "light");
  const [disks, setDisks] = useState([]);
  const [storageSpaces, setStorageSpaces] = useState([]);
  const [selectedPath, setSelectedPath] = useState("");
  const [selectedStoragePath, setSelectedStoragePath] = useState("");
  const [selectedSpeedKey, setSelectedSpeedKey] = useState("");
  const [jobs, setJobs] = useState([]);
  const [selectedJob, setSelectedJob] = useState(null);
  const [health, setHealth] = useState(null);
  const [mode, setMode] = useState("scan");
  const [scanMode, setScanMode] = useState("quick");
  const [scanExactSectors, setScanExactSectors] = useState(true);
  const [scanDeepLocate, setScanDeepLocate] = useState(false);
  const [scanSelfTest, setScanSelfTest] = useState(false);
  const [scanRepair, setScanRepair] = useState(false);
  const [scanRepairAfterMs, setScanRepairAfterMs] = useState(500);
  const [speedConfig, setSpeedConfig] = useState({
    speedRuns: 1,
    sampleMiB: 1024,
    speedOperation: "auto",
    speedBlockSize: 1048576,
    speedQueueDepth: 64,
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
  const [cancellingJobId, setCancellingJobId] = useState("");
  const [deleteRequest, setDeleteRequest] = useState(null);
  const [historyOpen, setHistoryOpen] = useState(false);
  const [deletingJobId, setDeletingJobId] = useState("");
  const [playbackJobId, setPlaybackJobId] = useState("");
  const handlePlaybackChange = useCallback((jobId, playing) => {
    setPlaybackJobId((current) => playing ? jobId : (current === jobId ? "" : current));
  }, []);

  useEffect(() => {
    const media = window.matchMedia?.("(prefers-color-scheme: dark)");
    if (!media) return;
    const update = (event) => setSystemDark(event.matches);
    media.addEventListener("change", update);
    return () => media.removeEventListener("change", update);
  }, []);

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    document.documentElement.style.colorScheme = theme;
    const themeColor = document.querySelector('meta[name="theme-color"]');
    if (themeColor) themeColor.setAttribute("content", theme === "dark" ? "#1c1d20" : "#f6f7f9");
  }, [theme]);

  const toggleTheme = () => {
    const next = theme === "dark" ? "light" : "dark";
    try {
      window.localStorage.setItem("diskbench-theme-choice", next);
    } catch {
      // Theme still applies for the current session when storage is unavailable.
    }
    setThemeChoice(next);
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
  const selectedStorageRef = useRef(null);
  const trimDefaultTargetRef = useRef("");
  const resumeLookupRef = useRef(null);

  const selectedDisk = useMemo(() => disks.find((disk) => disk.path === selectedPath) || disks[0] || null, [disks, selectedPath]);
  const selectedStorageSpace = useMemo(() => (
    storageSpaces.find((space) => space.path === selectedStoragePath) || storageSpaces[0] || null
  ), [selectedStoragePath, storageSpaces]);
  const speedTargets = useMemo(() => buildSpeedTargets(disks, storageSpaces), [disks, storageSpaces]);
  const selectedSpeedTarget = useMemo(() => (
    speedTargets.find((target) => target.key === selectedSpeedKey && !target.disabledReason)
      || speedTargets.find((target) => !target.disabledReason)
      || null
  ), [selectedSpeedKey, speedTargets]);
  const activeJob = useMemo(() => jobs.find((job) => ACTIVE.has(job.status)) || null, [jobs]);
  const finishing = !activeJob && Boolean(playbackJobId);
  const controlsLocked = Boolean(activeJob) || finishing || creatingJob || checkingResume;
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
      latencyBasis: selectedDisk.rotational ? "hdd-dual-v2" : "solid-dual-v2",
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
  const shownJobDisk = useMemo(() => findCurrentDiskForJob(disks, shownJob), [disks, shownJob]);

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
    const targetPath = selectedSpeedTarget?.key || "";
    if (!targetPath) {
      trimDefaultTargetRef.current = "";
      return;
    }
    if (!health || trimDefaultTargetRef.current === targetPath) return;
    if (selectedSpeedTarget.type === "raw") {
      trimDefaultTargetRef.current = targetPath;
      setSpeedConfig((current) => ({ ...current, speedTrim: false }));
      return;
    }
    const currentSpace = selectedSpeedTarget.storageSpace;
    const members = Array.isArray(currentSpace.members) ? currentSpace.members : [];
    const inventoryComplete = members.length > 0 && members.every((member) => disks.some((disk) => disk.path === member));
    if (!inventoryComplete) return;

    trimDefaultTargetRef.current = targetPath;
    const trimByDefault = health.capabilities?.fstrim === true
      && isFnOsStorageSpacePath(currentSpace.path)
      && storageSpaceUsesOnlySolidStateDisks(currentSpace, disks);
    setSpeedConfig((current) => ({ ...current, speedTrim: trimByDefault }));
  }, [disks, health, selectedSpeedTarget]);

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
    if (activeJob.type === "scan" || activeJob.type === "speed") setMode(activeJob.type);
    if (activeJob.type === "write") {
      setMode("scan");
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

  useEffect(() => {
    if (!activeJob) return;
    if (activeJob.type === "speed") {
      const target = speedTargets.find((candidate) => !candidate.disabledReason && jobBelongsToSpeedTarget(activeJob, candidate));
      if (target) setSelectedSpeedKey(target.key);
    } else if (shownJobDisk) {
      selectedDiskRef.current = shownJobDisk;
      setSelectedPath(shownJobDisk.path);
    }
  }, [activeJob?.id, activeJob?.type, activeJob?.options?.scanMode, disks, shownJobDisk?.path, speedTargets]);

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
      api(currentJobListPath(jobsRef.current), { signal: controller.signal }).then(readJobList),
      api("/health", { signal: controller.signal }).then(readHealth)
    ]).then(([diskResult, jobResult, healthResult]) => {
      if (!mountedRef.current || controller.signal.aborted || generation !== refreshGenerationRef.current) return;

      const resourceFailures = [];
      if (diskResult.status === "fulfilled") {
        const { disks: nextDisks, storageSpaces: nextSpaces } = diskResult.value;
        const previousDisk = selectedDiskRef.current;
        const continuedDisk = previousDisk
          ? findCurrentDiskForJob(nextDisks, { device: previousDisk.path, disk: previousDisk })
          : null;
        const nextSelectedDisk = continuedDisk || nextDisks[0] || null;
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

  const activeScan = Boolean(activeJob && (activeJob.type === "scan" || activeJob.type === "write"));
  // Keep a low-frequency full-job sync alongside incremental cell updates.
  // This also supports older packaged backends that do not expose scan-progress.
  const pollInterval = activeScan ? ACTIVE_SCAN_FALLBACK_POLL_MS : (activeJob ? 300 : 6000);

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
    if (activeScan) return undefined;
    const timer = window.setInterval(() => refresh(true), 60_000);
    return () => window.clearInterval(timer);
  }, [activeScan, refresh]);

  const shownJobNeedsDetail = Boolean(
    shownJob
    && !shownJob.id.startsWith("pending-")
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
      if (controller.signal.aborted || !mountedRef.current || createPendingRef.current || jobsRef.current.some((job) => ACTIVE.has(job.status))) return;
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
		  readOnly: storageSpace.readOnly
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

  function cancelResumeLookup() {
    resumeLookupRef.current?.abort();
    resumeLookupRef.current = null;
    setCheckingResume(false);
    setResumeRequest(null);
  }

  function selectMode(nextMode) {
    cancelResumeLookup();
    setMode(nextMode);
    setSelectedJob(null);
  }

  function selectDisk(path) {
    if (controlsLocked) return;
    cancelResumeLookup();
    setScanRepair(false);
    const nextDisk = disks.find((disk) => disk.path === path) || null;
    selectedDiskRef.current = nextDisk;
    setSelectedPath(path);
    setSelectedJob(null);
    setActionError("");
  }

  function selectSpeedTarget(key) {
    if (controlsLocked) return;
    cancelResumeLookup();
    const target = speedTargets.find((candidate) => candidate.key === key && !candidate.disabledReason);
    if (!target) return;
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
    if (controlsLocked) return;
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

  async function createJob(options) {
    if (createPendingRef.current || controlsLocked) return;
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
    setCancellingJobId(id);
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
      if (mountedRef.current) setCancellingJobId((current) => current === id ? "" : current);
    }
  }

  async function deleteHistoryJob() {
    const id = deleteRequest?.id;
    if (typeof id !== "string" || !id.trim() || deletingJobId) return;
    setDeletingJobId(id);
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
      setDeleteRequest(null);
    } catch (err) {
      if (mountedRef.current) {
        setActionError(errorMessage(err));
        setDeleteRequest(null);
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
            <div className="control-column">
              <DiskSelector
                disks={disks}
                selected={selectedDisk}
                onSelect={selectDisk}
                loading={loading}
                locked={controlsLocked}
                onRefresh={() => refresh()}
                refreshing={refreshing}
                availabilityError={diskAvailabilityError}
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
                  finishing={finishing}
                  submitting={creatingJob || checkingResume}
                  checkingResume={checkingResume}
                  cancelPending={cancellingJobId === activeJob?.id}
                  targetAvailable={Boolean(mode === "speed" ? selectedSpeedTarget : selectedDisk)}
                  disk={selectedDisk}
                  speedTarget={selectedSpeedTarget}
                />
              )}
            </div>
            {(selectedDisk || jobs.length > 0) && (
              <div className="result-column">
                <div className="result-module">
                  <ModeSwitcher
                    mode={mode}
                    setMode={selectMode}
                  />
                  <ActiveTask
                    job={shownJob}
                    contextDisk={selectedDisk}
                    detailState={shownDetailState}
                    onReloadDetail={() => setDetailReloadVersion((version) => version + 1)}
                    speedConfig={speedConfig}
                    onStartSpeed={openConfirmation}
                    speedStartDisabled={!selectedSpeedTarget || controlsLocked || health?.capabilities?.scan !== true || health?.capabilities?.fio !== true}
                    onPlaybackChange={handlePlaybackChange}
                    onOpenHistory={() => setHistoryOpen(true)}
                  />
                </div>
              </div>
            )}
          </div>
        </main>
      </div>
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
        mode={mode}
        selectedId={shownJob?.id}
        onSelect={selectHistoryJob}
        onDelete={setDeleteRequest}
        onClose={() => setHistoryOpen(false)}
        selectionLocked={controlsLocked}
      />}
      <DeleteHistoryDialog
        job={deleteRequest}
        deleting={Boolean(deletingJobId)}
        onClose={() => { if (!deletingJobId) setDeleteRequest(null); }}
        onConfirm={deleteHistoryJob}
      />
    </div>
  );
}

createRoot(document.getElementById("root")).render(<App />);
