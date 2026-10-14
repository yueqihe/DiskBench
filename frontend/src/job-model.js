function text(value) {
  return typeof value === "string" ? value.trim() : "";
}

function positiveSize(value) {
  const size = Number(value);
  return Number.isSafeInteger(size) && size > 0 ? size : null;
}

function record(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

const SCAN_STATUSES = new Set([
  "pending",
  "reading",
  "normal",
  "slow",
  "slower",
  "severe",
  "under5",
  "under20",
  "under50",
  "under150",
  "under500",
  "over500",
  "error"
]);
const TERMINAL_SCAN_STATUSES = new Set([...SCAN_STATUSES].filter((status) => !["pending", "reading"].includes(status)));
const SLOW_SCAN_STATUSES = new Set(["slow", "slower", "severe", "under150", "under500", "over500"]);

// Split normalized timing bands, preserving explicit response-warning verdicts.
// Legacy history retains its original timing bands.
export function scanStatusGroup(status, maxUnitMs) {
  if (status === "normal" && Number.isFinite(maxUnitMs) && maxUnitMs >= 0) {
    return maxUnitMs < 5 ? "fast" : maxUnitMs < 20 ? "good" : "normal";
  }
  if (status === "slow") {
    return Number.isFinite(maxUnitMs) && maxUnitMs >= 0 && maxUnitMs < 150 ? "slow" : "slower";
  }
  return ({ under5: "fast", under20: "good", under50: "normal",
    under150: "slow", under500: "slower", over500: "severe" })[status] || status;
}
const SPEED_LABELS = new Set(["头部", "中部", "尾部", "随机"]);
const SPEED_POSITION_LABELS = {
  start: "头部",
  middle: "中部",
  end: "尾部"
};
const SCAN_CELL_COUNT = 4096;
const LEGACY_SCAN_CELL_COUNT = 2048;
const MEBIBYTE = 1024 * 1024;
const SPEED_BLOCK_SIZES = new Set([4096, 65536, MEBIBYTE, 4 * MEBIBYTE]);
const SPEED_QUEUE_DEPTHS = new Set([1, 8, 16, 32, 64, 128]);
const SPEED_THREAD_COUNTS = new Set([1, 2, 4, 8]);
const JOB_STATUSES = new Set(["queued", "running", "cancelling", "completed", "failed", "cancelled", "interrupted"]);
const ACTIVE_JOB_STATUSES = new Set(["queued", "running", "cancelling"]);

export function terminalScanPrefix(map) {
  let count = 0;
  while (count < map.length && TERMINAL_SCAN_STATUSES.has(map[count].status)) count += 1;
  return count;
}

function finiteNonNegative(value) {
  return Number.isFinite(value) && value >= 0;
}

function safeNonNegativeInteger(value) {
  return Number.isSafeInteger(value) && value >= 0;
}

function optionalFiniteNonNegative(value) {
  return value === undefined || value === null || finiteNonNegative(value);
}

function optionalSafeNonNegativeInteger(value) {
  return value === undefined || value === null || safeNonNegativeInteger(value);
}

function scanCellIsValid(cell, index, previousEnd) {
  if (!record(cell)
    || cell.index !== index
    || !Number.isSafeInteger(cell.start)
    || !Number.isSafeInteger(cell.end)
    || cell.start !== previousEnd
    || cell.end <= cell.start
    || !SCAN_STATUSES.has(cell.status)
    || !optionalFiniteNonNegative(cell.maxMs)
    || !optionalFiniteNonNegative(cell.maxUnitMs)
    || [cell.maxReadLatency, cell.maxWriteLatency].some((sample) => sample != null && (!record(sample)
      || !finiteNonNegative(sample.ms)
      || !Number.isSafeInteger(sample.bytes) || sample.bytes <= 0
      || sample.bytes > 128 * 1024
      || !Number.isSafeInteger(sample.offset) || sample.offset < cell.start
      || sample.offset + sample.bytes > cell.end))
    || !optionalFiniteNonNegative(cell.avgMs)
    || !optionalFiniteNonNegative(cell.totalMs)
    || !optionalSafeNonNegativeInteger(cell.reads)
    || !optionalSafeNonNegativeInteger(cell.bytesRead)
    || !optionalSafeNonNegativeInteger(cell.coveredBytes)
    || !optionalSafeNonNegativeInteger(cell.unreadableBytes)
    || !optionalSafeNonNegativeInteger(cell.unresolvedBytes)
    || (cell.coveredBytes ?? 0) + (cell.unresolvedBytes ?? 0) > cell.end - cell.start
    || !optionalSafeNonNegativeInteger(cell.recoveredRanges)
    || !optionalSafeNonNegativeInteger(cell.deviceErrorRanges)
    || (cell.coveredBytes != null && cell.coveredBytes > cell.end - cell.start)
    || (cell.readOutcome != null && !["readable", "recovered", "unreadable", "device_error", "unresolved", "partial"].includes(cell.readOutcome))
    || !optionalSafeNonNegativeInteger(cell.errorCount)) {
    return false;
  }

  const hasSampleStart = cell.sampleStart !== undefined && cell.sampleStart !== null;
  const hasSampleEnd = cell.sampleEnd !== undefined && cell.sampleEnd !== null;
  if (hasSampleStart !== hasSampleEnd) return false;
  if (hasSampleStart && (
    !Number.isSafeInteger(cell.sampleStart)
    || !Number.isSafeInteger(cell.sampleEnd)
    || cell.sampleStart < cell.start
    || cell.sampleEnd <= cell.sampleStart
    || cell.sampleEnd > cell.end
  )) return false;

  if (cell.sampleRanges != null) {
    if (!Array.isArray(cell.sampleRanges) || cell.sampleRanges.length < 1 || cell.sampleRanges.length > 2 || !hasSampleStart) return false;
    let end = cell.start;
    for (const range of cell.sampleRanges) {
      if (!record(range) || !Number.isSafeInteger(range.start) || !Number.isSafeInteger(range.end)
        || range.start < end || range.end <= range.start || range.end > cell.end) return false;
      end = range.end;
    }
    if (cell.sampleRanges[0].start !== cell.sampleStart || cell.sampleRanges.at(-1).end !== cell.sampleEnd) return false;
  }

  return true;
}

function sampledCellBytes(cell) {
  return cell.sampleRanges?.length
    ? cell.sampleRanges.reduce((sum, range) => sum + range.end - range.start, 0)
    : cell.sampleEnd - cell.sampleStart;
}

function speedSampleIsValid(sample) {
  if (!record(sample)
    || !SPEED_LABELS.has(sample.label)
    || !safeNonNegativeInteger(sample.offsetBytes)
    || !Number.isSafeInteger(sample.bytes)
    || sample.bytes <= 0
    || !Number.isFinite(sample.seconds)
    || sample.seconds <= 0
    || !Number.isFinite(sample.mbps)
    || sample.mbps < 0) {
    return false;
  }
  return true;
}

function validBadRanges(value, diskSize) {
  if (value === undefined) return true;
  if (!Array.isArray(value)) return false;
  return value.every((range) => {
    if (!record(range)
      || !safeNonNegativeInteger(range.offset)
      || !Number.isSafeInteger(range.length)
      || range.length <= 0
      || !optionalSafeNonNegativeInteger(range.unresolvedBytes)
      || (range.unresolvedBytes ?? 0) > range.length
      || ((range.unresolvedBytes ?? 0) > 0 && (range.code !== "LOCALIZATION_LIMIT" || range.outcome !== "unresolved"))) {
      return false;
    }
    const end = range.offset + range.length;
    return Number.isSafeInteger(end) && end <= diskSize;
  });
}

export function scanOutcomeSummary(job) {
  if (job?.type !== "scan" || job.result?.version !== 2 || !record(job.result.stats)) return null;
  const stats = job.result.stats;
  const keys = ["coveredBytes", "issueCount", "savedIssueCount", "unreadableRanges", "unreadableBytes", "recoveredRanges", "deviceErrorRanges", "unresolvedRanges", "unresolvedBytes"];
  if (keys.some((key) => !optionalSafeNonNegativeInteger(stats[key]))) return null;
  if ((stats.coveredBytes ?? 0) > job.disk?.size
    || (stats.savedIssueCount ?? 0) > (stats.issueCount ?? 0)
    || (stats.issuesComplete === true && (stats.savedIssueCount ?? 0) !== (stats.issueCount ?? 0))) return null;
  return Object.fromEntries(keys.map((key) => [key, stats[key] ?? 0]));
}

export function validScanIssuePage(page, diskSize, offset) {
  return record(page) && page.offset === offset
    && safeNonNegativeInteger(page.total) && safeNonNegativeInteger(page.next)
    && page.next >= offset && page.next <= page.total
    && typeof page.complete === "boolean" && typeof page.legacy === "boolean"
    && Array.isArray(page.records) && page.records.length <= 200
    && page.next - offset === page.records.length && validBadRanges(page.records, diskSize)
    && page.records.every((range) => typeof range.code === "string"
      && (range.outcome === undefined || ["recovered", "unreadable", "device_error", "unresolved", "rewrite"].includes(range.outcome))
      && validScanRepairRecord(range));
}

export function scanRangeOutcomeLabel(range) {
	if (range?.outcome === "rewrite") return ({ pending: "已备份，准备写回", uncertain: "写回中断，结果未确定", verified: "写回校验通过", still_slow: "写回后仍慢", skipped: "已跳过写回", failed: "写回或校验失败" })[range.repair?.outcome] || "写回状态未知";
  if (range?.outcome === "unreadable") return "持续不可读";
  if (range?.outcome === "recovered") return range.code === "RECOVERED_ERROR" ? "固件已纠正" : "复读已恢复";
  if (range?.outcome === "device_error") return range.recovered === true ? "设备异常已恢复" : "设备异常";
  if (range?.outcome === "unresolved") return range.code === "LOCALIZATION_LIMIT" ? "定位达到限额，待细查" : "范围未确定";
  return "历史异常";
}

export function jobBelongsToDisk(job, disk) {
  if (!job || !disk) return false;
  const snapshot = job.disk && typeof job.disk === "object" ? job.disk : {};
  const jobSerial = text(snapshot.serial);
  const diskSerial = text(disk.serial);
  const jobSize = positiveSize(snapshot.size);
  const diskSize = positiveSize(disk.size);

  if (jobSerial || diskSerial) {
    if (!jobSerial || !diskSerial) return false;
    return jobSerial === diskSerial && (jobSize === null || diskSize === null || jobSize === diskSize);
  }

  if (text(job.device) !== text(disk.path)) return false;
  if (jobSize !== null && diskSize !== null && jobSize !== diskSize) return false;
  const jobModel = text(snapshot.model);
  const diskModel = text(disk.model);
  return !jobModel || !diskModel || jobModel === diskModel;
}

export function findCurrentDiskForJob(disks, job) {
  return Array.isArray(disks) ? disks.find((disk) => jobBelongsToDisk(job, disk)) || null : null;
}

export function findResumableScan(jobs, disk) {
  if (!Array.isArray(jobs) || !text(disk?.serial)) return null;
  const scans = jobs.filter((job) => job?.type === "scan" && jobBelongsToDisk(job, disk));
  const order = (job) => Number.isSafeInteger(job.historyOrder) ? job.historyOrder : (Date.parse(job.createdAt) || 0);
  const superseded = new Set(scans.filter((job) => job.resume || job.status === "completed" || ACTIVE_JOB_STATUSES.has(job.status)).map((job) => job.resumeFrom).filter(Boolean));
  return scans.filter((job) => {
    const resume = job.resume;
    const completedOrder = Math.max(-1, ...scans.filter((finished) => finished.status === "completed" && finished.options?.scanMode === job.options?.scanMode).map(order));
    return ["cancelled", "interrupted"].includes(job.status)
      && !superseded.has(job.id) && order(job) > completedOrder
      && job.disk?.model === disk.model && job.disk?.size === disk.size
      && job.disk?.logicalSectorSize === disk.logicalSectorSize
      && job.disk?.physicalSectorSize === disk.physicalSectorSize
      && ["quick", "full"].includes(job.options?.scanMode)
      && Number.isSafeInteger(resume?.cellIndex) && resume.cellIndex >= 0
      && Number.isSafeInteger(resume?.cellCount) && resume.cellIndex < resume.cellCount
      && Number.isSafeInteger(resume?.offset) && resume.offset >= 0 && resume.offset < disk.size
      && Number.isFinite(resume?.progress) && resume.progress >= 0 && resume.progress < 100;
  }).sort((a, b) => order(b) - order(a))[0] || null;
}

export function storageSpacesForDisk(storageSpaces, disk) {
  const diskPath = text(record(disk) ? disk.path : disk);
  if (!diskPath || !Array.isArray(storageSpaces)) return [];
  return storageSpaces.filter((space) => (
    record(space)
    && Array.isArray(space.members)
    && space.members.some((member) => text(member) === diskPath)
  ));
}

export function storageSpaceForDisk(storageSpaces, disk, preferredPath = "") {
  const matches = storageSpacesForDisk(storageSpaces, disk);
  const preferred = text(preferredPath);
  return matches.find((space) => text(space.path) === preferred) || matches[0] || null;
}

export function storageSpaceUsesOnlySolidStateDisks(storageSpace, disks) {
  if (!record(storageSpace) || !Array.isArray(storageSpace.members) || storageSpace.members.length === 0 || !Array.isArray(disks)) {
    return false;
  }
  const disksByPath = new Map(disks.filter(record).map((disk) => [text(disk.path), disk]));
  return storageSpace.members.every((member) => {
    const disk = disksByPath.get(text(member));
    return record(disk) && disk.rotational === false;
  });
}

export function rawSpeedDiskBlockedReason(disk, storageSpaces = []) {
  if (!record(disk) || !text(disk.path)) return "磁盘信息无效";
  const spaces = storageSpacesForDisk(storageSpaces, disk);
  if (spaces.length > 0) return `由 ${spaces.map((space) => text(space.path)).filter(Boolean).join("、")} 提供文件测速`;
  if (disk.isSystem === true) return "系统盘不能执行底层测速";
  if (disk.readOnly === true) return "磁盘为只读状态";
  if (Array.isArray(disk.mountpoints) && disk.mountpoints.some((mountpoint) => text(mountpoint))) return "磁盘仍处于挂载状态";
  if (Array.isArray(disk.fileSystems) && disk.fileSystems.some((fileSystem) => text(fileSystem))) return "磁盘仍包含文件系统或阵列签名";
  return "";
}

export function scanRepairDiskBlockedReason(disk) {
  if (!record(disk) || !text(disk.serial)) return "无法可靠核对磁盘身份";
  if (disk.isSystem === true) return "系统盘禁止慢扇区修复";
  if (disk.readOnly === true) return "只读磁盘禁止慢扇区修复";
  if (Array.isArray(disk.mountpoints) && disk.mountpoints.some((value) => text(value))) return "请先卸载磁盘及其分区";
  if (![512,4096].includes(disk.logicalSectorSize)) return "需要明确的 512 B 或 4 KiB 逻辑扇区信息";
  const blockedFileSystems = Array.isArray(disk.fileSystems)
    ? disk.fileSystems.map(text).filter((value) => /raid|zfs|bcache|lvm|luks|^swap$/i.test(value))
    : [];
  if (blockedFileSystems.length > 0) return `检测到 ${blockedFileSystems.join("、")} 标记，不能直接执行慢扇区修复`;
  return "";
}

export function validScanRepairRecord(range) {
  if (range?.outcome !== "rewrite") return true;
  const repair = range.repair;
  return record(repair) && ["pending", "uncertain", "verified", "still_slow", "skipped", "failed"].includes(repair.outcome)
    && finiteNonNegative(repair.beforeMs) && optionalFiniteNonNegative(repair.afterMs)
    && safeNonNegativeInteger(repair.writtenBytes) && repair.writtenBytes <= range.length;
}

export function buildSpeedTargets(disks, storageSpaces) {
  const safeDisks = Array.isArray(disks) ? disks.filter(record) : [];
  const safeSpaces = Array.isArray(storageSpaces) ? storageSpaces.filter(record) : [];
  const fileTargets = safeSpaces
    .filter((space) => text(space.path))
    .map((space) => ({
      key: `filesystem:${space.path}`,
      type: "filesystem",
      path: space.path,
      storageSpace: space,
      // Keep the physical members on the target so legacy speed jobs that
      // predate storageSpace snapshots can still be matched to this volume.
      memberDisks: safeDisks.filter((disk) => (
        Array.isArray(space.members)
        && space.members.some((member) => text(member) === text(disk.path))
      )),
      disabledReason: ""
    }));
  const rawTargets = safeDisks
    .filter((disk) => text(disk.path))
    .map((disk) => ({
      key: `raw:${disk.path}`,
      type: "raw",
      path: disk.path,
      disk,
      disabledReason: rawSpeedDiskBlockedReason(disk, safeSpaces)
    }));
  return [...fileTargets, ...rawTargets];
}

export function jobBelongsToSpeedTarget(job, target) {
  if (!record(job) || job.type !== "speed" || !record(target)) return false;
  if (target.type === "filesystem") {
    if (text(job.storageSpace?.path) === text(target.path)) return true;
    // Jobs created before filesystem-aware speed targets stored the physical
    // device in `device` and had no `storageSpace` snapshot. Match those
    // records through the current volume members so switching targets does
    // not make the existing history disappear after an upgrade.
    if (job.storageSpace) return false;
    const members = Array.isArray(target.storageSpace?.members) ? target.storageSpace.members : [];
    const snapshotSerial = text(job.disk?.serial);
    if (!snapshotSerial && members.some((member) => text(member) === text(job.device))) return true;
    return Array.isArray(target.memberDisks)
      && target.memberDisks.some((disk) => jobBelongsToDisk(job, disk));
  }
  return target.type === "raw" && !job.storageSpace && jobBelongsToDisk(job, target.disk);
}

export function mergeJobLists(currentJobs, nextJobs) {
  const currentById = new Map(currentJobs.map((job) => [job.id, job]));
  return nextJobs.map((job) => {
    const current = currentById.get(job.id);
    if (!current) return job;
    const merged = { ...current, ...job };
    const statusChanged = current.status !== job.status;
    const scanJob = ["scan", "write"].includes(current.type) && current.type === job.type;
    const bothActive = ACTIVE_JOB_STATUSES.has(current.status) && ACTIVE_JOB_STATUSES.has(job.status);
    if (scanJob && bothActive) {
      const currentMap = safeScanMap(current.result?.map);
      const incomingMap = safeScanMap(job.result?.map);
      const currentAhead = currentMap.length > 0
        && incomingMap.length > 0
        && terminalScanPrefix(currentMap) > terminalScanPrefix(incomingMap);

      if (currentAhead) {
        merged.result = current.result;
        merged.phase = current.phase;
        merged.message = current.message;
      }
      if (Number.isFinite(current.progress) && Number.isFinite(job.progress)) {
        merged.progress = Math.max(current.progress, job.progress);
      }
    }
    if (!Object.prototype.hasOwnProperty.call(job, "result") && statusChanged) delete merged.result;
    if (!Object.prototype.hasOwnProperty.call(job, "log") && statusChanged) delete merged.log;
    if (!Object.prototype.hasOwnProperty.call(job, "persistenceWarning")) delete merged.persistenceWarning;
    return merged;
  });
}

export function mergeScanProgress(job, update) {
  if (!record(job)
    || !record(update)
    || typeof update.id !== "string"
    || update.id !== job.id
    || !JOB_STATUSES.has(update.status)
    || !Number.isFinite(update.progress)
    || update.progress < 0
    || update.progress > 100
    || typeof update.phase !== "string"
    || typeof update.message !== "string"
    || !Array.isArray(update.cells)
    || !Number.isSafeInteger(update.next)
    || !record(update.stats)) return job;

  const currentMap = safeScanMap(job.result?.map);
  if (!currentMap.length || update.next < update.cells.length || update.next > currentMap.length) return job;
  const start = update.next - update.cells.length;
  if (start > terminalScanPrefix(currentMap)) return job;
  const nextMap = [...currentMap];
  for (let offset = 0; offset < update.cells.length; offset += 1) {
    const index = start + offset;
    const cell = update.cells[offset];
    const previousEnd = index === 0 ? 0 : nextMap[index - 1].end;
    if (!scanCellIsValid(cell, index, previousEnd) || !TERMINAL_SCAN_STATUSES.has(cell.status)) return job;
    nextMap[index] = cell;
  }

  const stats = update.stats;
  if (!safeNonNegativeInteger(stats.testedCells)
    || !safeNonNegativeInteger(stats.errorCells)
    || !safeNonNegativeInteger(stats.slowCells)
    || !safeNonNegativeInteger(stats.bytesRead)
    || !validBadRanges(stats.badRanges, job.disk?.size)) return job;
  if (job.type === "scan" && job.result.version === 2
    && !scanOutcomeSummary({ ...job, result: { ...job.result, stats } })) return job;

  return {
    ...job,
    status: update.status,
    progress: update.progress,
    phase: update.phase,
    message: update.message,
    finishedAt: update.finishedAt ?? job.finishedAt,
    ...(typeof update.persistenceWarning === "string" ? { persistenceWarning: update.persistenceWarning } : {}),
    result: { ...job.result, map: nextMap, stats,
      ...(record(update.diagnostics) ? { diagnostics: update.diagnostics } : {}) }
  };
}

export function planScanReveal(previousFinish, count, now) {
  if (!Number.isSafeInteger(count) || count <= 0) return { delays: [], finishAt: now };
  const start = Math.max(now, Number.isFinite(previousFinish) ? previousFinish : now);
  const spacing = 4;
  return {
    delays: Array.from({ length: count }, (_, index) => Math.round(start - now + index * spacing)),
    finishAt: start + (count - 1) * spacing
  };
}

export function advanceScanReveal(current, target, elapsedMilliseconds, carry = 0) {
  if (!Number.isSafeInteger(current)
    || !Number.isSafeInteger(target)
    || target <= current) return { prefix: current, carry: 0 };
  const backlog = target - current;
  if (backlog <= 4) return { prefix: current + 1, carry: 0 };
  const elapsed = Math.max(0, Math.min(34, Number(elapsedMilliseconds) || 0));
  const rate = Math.min(240, Math.max(60, backlog / 4));
  const budget = Math.max(0, Number(carry) || 0) + elapsed * rate / 1000;
  const step = Math.min(backlog, Math.floor(budget));
  return {
    prefix: current + step,
    carry: step === backlog ? 0 : budget - step
  };
}

export function estimateRemainingMilliseconds(job, now = Date.now()) {
  if (!record(job)
    || !["scan", "write"].includes(job.type)
    || job.status !== "running"
    || typeof job.startedAt !== "string") return null;

  const startedAt = Date.parse(job.startedAt);
  let progress = Number(job.progress);
  const stats = job.result?.stats;
  if (job.type === "scan" && job.result?.coverage === "sampled"
    && Number.isSafeInteger(stats?.testedCells)
    && Array.isArray(job.result?.map)
    && job.result.map.length > 0
    && stats.testedCells >= 0
    && stats.testedCells <= job.result.map.length) {
    progress = stats.testedCells / job.result.map.length * 100;
  } else if (Number.isFinite(stats?.bytesRead)
    && Number.isFinite(job.disk?.size)
    && job.disk.size > 0
    && stats.bytesRead >= 0
    && stats.bytesRead <= job.disk.size) {
    progress = Math.max(progress, stats.bytesRead / job.disk.size * 100);
  }
  const currentTime = Number(now);
  const elapsed = currentTime - startedAt;
  const resumeStart = Number.isFinite(job.resumeStartProgress) ? job.resumeStartProgress : 0;
  if (!Number.isFinite(startedAt)
    || !Number.isFinite(currentTime)
    || !Number.isFinite(progress)
    || progress <= resumeStart
    || progress >= 100
    || elapsed < 1_000) return null;

  const remaining = elapsed * (100 - progress) / (progress - resumeStart);
  return Number.isFinite(remaining) && remaining >= 0 ? remaining : null;
}

export function formatEstimatedDuration(milliseconds) {
  if (!Number.isFinite(milliseconds) || milliseconds < 0) return "计算中";
  if (milliseconds < 60_000) return "少于 1 分钟";

  const totalMinutes = Math.ceil(milliseconds / 60_000);
  if (totalMinutes < 60) return `约 ${totalMinutes} 分钟`;

  const totalHours = Math.floor(totalMinutes / 60);
  const minutes = totalMinutes % 60;
  if (totalHours < 24) return minutes > 0
    ? `约 ${totalHours} 小时 ${minutes} 分钟`
    : `约 ${totalHours} 小时`;

  const days = Math.floor(totalHours / 24);
  const hours = totalHours % 24;
  return hours > 0 ? `约 ${days} 天 ${hours} 小时` : `约 ${days} 天`;
}

export function chooseShownJob({ activeJob, selectedJob, modeJobs, fallbackJobs, pendingJob, mode }) {
  const visibleActiveJob = mode === "speed"
    ? (activeJob?.type === "speed" ? activeJob : null)
    : (mode === "scan" && ["scan", "write"].includes(activeJob?.type) ? activeJob : null);
  return visibleActiveJob
    || selectedJob
    || (Array.isArray(modeJobs) ? modeJobs[0] : null)
    || (Array.isArray(fallbackJobs) ? fallbackJobs[0] : null)
    || pendingJob
    || null;
}

export function detailRetryDelay(error, failedAttempt) {
  const status = Number(error?.status);
  if (Number.isInteger(status) && status >= 400 && status < 500 && ![408, 429].includes(status)) return null;
  const delays = [1000, 3000];
  return Number.isSafeInteger(failedAttempt) && failedAttempt >= 0 ? (delays[failedAttempt] ?? null) : null;
}

export function safeScanMap(value) {
  if (!Array.isArray(value) || value.length === 0) return [];
  let previousEnd = 0;
  for (let index = 0; index < value.length; index += 1) {
    const cell = value[index];
    if (!scanCellIsValid(cell, index, previousEnd)) return [];
    previousEnd = cell.end;
  }
  return value;
}

export function safeSpeedSamples(value) {
  if (!Array.isArray(value)) return [];
  const labels = new Set();
  for (let index = 0; index < value.length; index += 1) {
    if (!Object.prototype.hasOwnProperty.call(value, index)) return [];
    const sample = value[index];
    if (!speedSampleIsValid(sample) || labels.has(sample.label)) return [];
    labels.add(sample.label);
  }
  return value;
}

export function isCompleteScanResult(job) {
  if (!record(job)
    || (job.type !== "scan" && job.type !== "write")
    || job.status !== "completed"
    || !record(job.disk)
    || !Number.isSafeInteger(job.disk.size)
    || job.disk.size <= 0
    || !record(job.result)
    || !record(job.result.stats)) {
    return false;
  }

  const rawMap = job.result.map;
  const map = safeScanMap(rawMap);
  if (!Array.isArray(rawMap)
    || map !== rawMap
    || ![LEGACY_SCAN_CELL_COUNT, SCAN_CELL_COUNT].includes(map.length)
    || map.at(-1).end !== job.disk.size
    || map.some((cell) => !TERMINAL_SCAN_STATUSES.has(cell.status))) {
    return false;
  }

  const coverage = job.result.coverage;
  const samplingPlan = job.result.samplingPlan;
  if (samplingPlan != null && (samplingPlan !== "stratified-v1" || coverage !== "sampled"
    || !Number.isInteger(job.result.samplingSeed ?? 0) || (job.result.samplingSeed ?? 0) < 0 || (job.result.samplingSeed ?? 0) > 0xffffffff
    || map.some((cell) => !cell.sampleRanges?.length))) return false;
  if (samplingPlan == null && map.some((cell) => cell.sampleRanges != null)) return false;
  const declaredScanModes = [job.options?.scanMode, job.result.scanMode].filter((mode) => mode != null);
  if (!["sampled", "full"].includes(coverage)
    || declaredScanModes.some((mode) => !["quick", "full", "write"].includes(mode))
    || declaredScanModes.some((mode) => (mode === "quick") !== (coverage === "sampled"))
    || declaredScanModes.some((mode) => mode === "write" && coverage !== "full")) {
    return false;
  }

  const expectedErrors = map.filter((cell) => cell.status === "error").length;
  const expectedSlow = map.filter((cell) => SLOW_SCAN_STATUSES.has(cell.status)).length;
  const { stats } = job.result;
  if (![stats.testedCells, stats.errorCells, stats.slowCells, stats.bytesRead].every(safeNonNegativeInteger)
    || !validBadRanges(stats.badRanges, job.disk.size)) {
    return false;
  }

  let expectedBytes = 0;
  for (const cell of map) {
    if (!Number.isSafeInteger(cell.reads)
      || cell.reads <= 0
      || !safeNonNegativeInteger(cell.bytesRead)
      || !safeNonNegativeInteger(cell.errorCount)
      || !finiteNonNegative(cell.maxMs)
      || !finiteNonNegative(cell.avgMs)
      || (cell.status === "error") !== (cell.errorCount > 0)) {
      return false;
    }

    if (coverage === "full") {
      if (cell.sampleStart != null
        || cell.sampleEnd != null
        || cell.bytesRead > cell.end - cell.start) {
        return false;
      }
    } else if (!Number.isSafeInteger(cell.sampleStart)
      || !Number.isSafeInteger(cell.sampleEnd)
      || cell.bytesRead > sampledCellBytes(cell)) {
      return false;
    }

    expectedBytes += cell.bytesRead;
    if (!Number.isSafeInteger(expectedBytes)) return false;
  }

  if (stats.testedCells !== map.length
    || stats.errorCells !== expectedErrors
    || stats.slowCells !== expectedSlow
    || stats.bytesRead !== expectedBytes) {
    return false;
  }

  if (job.type === "scan" && job.result.version === 2) {
    if (!scanOutcomeSummary(job) || stats.issuesComplete !== true) return false;
    const expectedCoverage = map.reduce((sum, cell) => sum + (coverage === "sampled" ? sampledCellBytes(cell) : cell.end - cell.start), 0);
    if ((stats.coveredBytes ?? 0) + (stats.unresolvedBytes ?? 0) !== expectedCoverage
      || (stats.unresolvedBytes ?? 0) !== map.reduce((sum, cell) => sum + (cell.unresolvedBytes ?? 0), 0)
      || map.some((cell) => cell.coveredBytes + (cell.unresolvedBytes ?? 0) !== (coverage === "sampled" ? sampledCellBytes(cell) : cell.end - cell.start))) return false;
  }

  if (coverage === "full") {
    if (job.result.sampleBytes != null
      || expectedBytes > job.disk.size
      || (expectedErrors === 0 && expectedBytes !== job.disk.size)) {
      return false;
    }
  } else {
    if (!Number.isSafeInteger(job.result.sampleBytes)
      || job.result.sampleBytes <= 0
      || map.some((cell) => sampledCellBytes(cell) > job.result.sampleBytes)) {
      return false;
    }
  }
  return true;
}

export function isCompleteSpeedResult(job) {
  const targetSize = record(job?.storageSpace) && Number.isSafeInteger(job.storageSpace.size)
    ? job.storageSpace.size
    : job?.disk?.size;
  if (!record(job)
    || job.type !== "speed"
    || job.status !== "completed"
    || !record(job.result)
    || !record(job.disk)
    || !Number.isSafeInteger(targetSize)
    || targetSize <= 0
    || !Number.isSafeInteger(job.result.sampleMiB)
    || job.result.sampleMiB < 0
    || (job.result.runs !== undefined && ![1, 3, 5].includes(job.result.runs))) {
    return false;
  }

  const benchmarks = job.result.benchmarks;
  if (!Array.isArray(benchmarks) || benchmarks.length === 0) return false;

  for (const benchmark of benchmarks) {
    if (!record(benchmark)) return false;
    const queueDepth = benchmark.queueDepth ?? 1;
    const threads = benchmark.threads ?? 1;
    if (!["read", "write", "mixed"].includes(benchmark.operation)
      || !["sequential", "random"].includes(benchmark.pattern)
      || !Number.isSafeInteger(benchmark.blockSize)
      || (benchmark.pattern === "sequential" ? !SPEED_BLOCK_SIZES.has(benchmark.blockSize) : benchmark.blockSize !== 4096)
      || !Number.isSafeInteger(queueDepth)
      || !Number.isSafeInteger(threads)
      || !SPEED_QUEUE_DEPTHS.has(queueDepth)
      || !SPEED_THREAD_COUNTS.has(threads)
      || !Number.isFinite(benchmark.averageMBps)
      || benchmark.averageMBps < 0
      || !Number.isFinite(benchmark.minimumMBps)
      || !Number.isFinite(benchmark.maximumMBps)) {
      return false;
    }

    const rawSamples = benchmark.samples;
    const samples = safeSpeedSamples(rawSamples);
    if (!Array.isArray(rawSamples) || samples !== rawSamples || samples.length === 0) return false;

    if (benchmark.pattern === "random") {
      if (samples.length !== 1 || samples[0].label !== "随机") return false;
      if (!Number.isFinite(benchmark.iops) || benchmark.iops <= 0) return false;
    } else if (samples.some((sample) => sample.label === "随机" || !SPEED_LABELS.has(sample.label))) {
      return false;
    }

    const sorted = [...samples].sort((first, second) => first.offsetBytes - second.offsetBytes);
    if (sorted.some((sample, index) => {
      const end = sample.offsetBytes + sample.bytes;
      return !Number.isSafeInteger(end)
        || end > targetSize
        || (index > 0 && sorted[index - 1].offsetBytes + sorted[index - 1].bytes > sample.offsetBytes);
    })) {
      return false;
    }

    let totalBytes = 0;
    for (const sample of samples) {
      totalBytes += sample.bytes;
      if (!Number.isSafeInteger(totalBytes)) return false;
    }
    const totalSeconds = samples.reduce((sum, sample) => sum + sample.seconds, 0);
    if (!Number.isFinite(totalSeconds) || totalSeconds <= 0) return false;
    if (benchmark.iops !== undefined
      && (!Number.isFinite(benchmark.iops) || benchmark.iops <= 0)) {
      return false;
    }
    const speeds = samples.map((sample) => sample.mbps);
    const weightedAverage = Number((totalBytes / totalSeconds / 1e6).toFixed(1));
    if (benchmark.averageMBps !== weightedAverage
      || benchmark.minimumMBps !== Math.min(...speeds)
      || benchmark.maximumMBps !== Math.max(...speeds)) {
      return false;
    }
  }

  return true;
}
