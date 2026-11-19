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
export function scanStatusGroup(status, maxUnitMs, latencyBasis = "") {
  if (status === "normal" && Number.isFinite(maxUnitMs) && maxUnitMs >= 0) {
    return maxUnitMs < 5 ? "fast" : maxUnitMs < 20 ? "good" : "normal";
  }
  if (status === "slow") {
    const poorAfterMs = /-dual-v4$/.test(latencyBasis) ? latencyBasis.startsWith("hdd-") ? 200 : 50 : 150;
    return Number.isFinite(maxUnitMs) && maxUnitMs >= 0 && maxUnitMs < poorAfterMs ? "slow" : "slower";
  }
  return ({ under5: "fast", under20: "good", under50: "normal",
    under150: "slow", under500: "slower", over500: "severe" })[status] || status;
}

function severeScanStatus(status) {
  return status === "severe" || status === "over500";
}

export function scanAbnormalCellCount(map) {
  return Array.isArray(map) ? map.filter((cell) => cell?.status === "error" || severeScanStatus(cell?.status)).length : 0;
}

export function scanSlowBadBlock(cell, rotational = false) {
  const proof = cell?.slowBadBlock;
  const threshold = rotational ? 800 : 500;
  return proof && Number.isSafeInteger(proof.offset) && Number.isSafeInteger(proof.bytes) && proof.bytes > 0 && proof.bytes <= 128*1024
    && proof.offset >= cell.start && proof.offset + proof.bytes <= cell.end
    && Number.isFinite(proof.firstMs) && proof.firstMs >= threshold && Number.isFinite(proof.repeatMs) && proof.repeatMs >= threshold
    && proof.load?.backgroundActivity === false ? proof : null;
}

export function scanBadBlockKind(cell, rotational = false) {
  if (cell?.unreadableBytes > 0 || cell?.readOutcome === "unreadable") return "unreadable";
  return scanSlowBadBlock(cell,rotational) ? "slow" : "";
}

export function scanBadBlockSummary(map, rotational = false) {
  const summary = { total:0, slow:0, unreadable:0 };
  for (const cell of Array.isArray(map) ? map : []) {
    const kind = scanBadBlockKind(cell,rotational);
    if (kind) { summary[kind]++; summary.total++; }
  }
  return summary;
}

export function scanRepairOutcomeLabel(repair) {
  if (repair?.outcome === "verified" && repair.speedOutcome === "improved") return "慢速改善 · 写回校验通过";
  return ({ pending:"已备份，准备写回", uncertain:"写回中断，结果未确定", verified:"写回校验通过", still_slow:"修复后仍慢", skipped:"已跳过写回", failed:"写回或校验失败" })[repair?.outcome] || "写回状态未知";
}

export function scanRepairSpeedNote(repair, threshold) {
  if (repair?.speedOutcome === "improved") return "慢速改善";
  if (repair?.speedOutcome === "still_slow") return "复测仍慢，建议更换硬盘";
  const checks = repair?.speedChecks || [];
  if (!checks.length) return "复测未记录";
  if (checks.length !== 2) return "复测未完成";
  if (!repair.speedLoad) return "复测负载未知";
  if (repair.speedLoad.backgroundActivity) return "复测有其他磁盘活动";
  if (!(threshold > 0)) return "改善未确认";
  const firstFast = checks[0] < threshold, repeatFast = checks[1] < threshold;
  if (firstFast !== repeatFast) return "复测波动";
  if (firstFast && repeatFast) {
    if ((Number.isFinite(repair.currentBeforeMs) && repair.currentBeforeMs < threshold)
      || (Number.isFinite(repair.beforeRepeatMs) && repair.beforeRepeatMs < threshold)) return "修复前已恢复";
    if (!repair.beforeLoad) return "修复前负载未知";
    if (repair.beforeLoad.backgroundActivity) return "修复前有其他磁盘活动";
  }
  return "改善未确认";
}

export function scanRepairStages(repair) {
  const write = ({ pending:"待处理", uncertain:"未确认", verified:"已校验", still_slow:"已校验", skipped:"已跳过", failed:"未通过" })[repair?.outcome] || "未知";
  const stages = [{ label:"首次", ms:repair?.beforeMs }];
  if (Number.isFinite(repair?.beforeRepeatMs)) stages.push({ label:"修复前", ms:repair.beforeRepeatMs });
  stages.push({ label:"写回", text:write });
  const checks = repair?.speedChecks || [];
  if (["verified", "still_slow"].includes(repair?.outcome) && checks.length === 2) {
    stages.push({ label:"复测", values:checks });
  } else {
    // A legacy afterMs is a read-back timing, not a pair of speed retests.
    if (!checks.length && Number.isFinite(repair?.afterMs)) stages.push({ label:"校验", ms:repair.afterMs });
    const state = repair?.outcome === "skipped" ? "未执行"
      : ["pending", "uncertain"].includes(repair?.outcome) || checks.length === 1 ? "未完成"
      : repair?.outcome === "failed" ? (checks.length ? "未通过" : "未执行") : "未记录";
    stages.push({ label:"复测", text:state });
  }
  return stages;
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
    || !optionalSafeNonNegativeInteger(cell.skippedBytes)
    || (cell.skippedBytes ?? 0) > (cell.unresolvedBytes ?? 0)
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
      || !optionalSafeNonNegativeInteger(range.probeBytes)
      || !optionalSafeNonNegativeInteger(range.unresolvedBytes)
      || (range.unresolvedBytes ?? 0) > range.length
      || ((range.unresolvedBytes ?? 0) > 0 && (!["LOCALIZATION_LIMIT", "SLOW_READ_LIMIT"].includes(range.code) || range.outcome !== "unresolved"))
      || (range.code === "SLOW_READ_LIMIT" && (range.outcome !== "unresolved" || range.unresolvedBytes !== range.length || !text(range.detail)))
      || (range.skippedRange != null && !validTimeoutSkippedRange(range))) {
      return false;
    }
    const end = range.offset + range.length;
    return Number.isSafeInteger(end) && end <= diskSize;
  });
}

function validTimeoutSkippedRange(range) {
  const skipped = range.skippedRange;
  return range.code === "TIMEOUT" && range.outcome === "device_error" && range.recovered !== true
    && record(skipped) && safeNonNegativeInteger(skipped.offset)
    && Number.isSafeInteger(skipped.length) && skipped.length > 0
    && skipped.offset >= range.offset && Number.isSafeInteger(skipped.offset + skipped.length)
    && skipped.offset + skipped.length === range.offset + range.length;
}

export function mergeTimeoutSkipRanges(ranges) {
  const rows = [];
  for (const range of Array.isArray(ranges) ? ranges : []) {
    const previous = rows.at(-1);
    if (previous && !previous.skippedRange && range.code === "SLOW_READ_LIMIT" && range.outcome === "unresolved"
      && range.unresolvedBytes === range.length) {
      const merged = {...previous, skippedRange:{offset:range.offset,length:range.length}};
      if (validTimeoutSkippedRange(merged)) {
        rows[rows.length-1] = merged;
        continue;
      }
    }
    rows.push(range);
  }
  return rows;
}

export function scanOutcomeSummary(job) {
  if (job?.type !== "scan" || job.result?.version !== 2 || !record(job.result.stats)) return null;
  const stats = job.result.stats;
  const keys = ["coveredBytes", "issueCount", "savedIssueCount", "unreadableRanges", "unreadableBytes", "recoveredRanges", "deviceErrorRanges", "unresolvedRanges", "unresolvedBytes", "skippedBytes", "timeoutSkipPairs"];
  if (keys.some((key) => !optionalSafeNonNegativeInteger(stats[key]))) return null;
  if ((stats.coveredBytes ?? 0) > job.disk?.size
    || (stats.skippedBytes ?? 0) > (stats.unresolvedBytes ?? 0)
    || (stats.savedIssueCount ?? 0) > (stats.issueCount ?? 0)
    || (stats.timeoutSkipPairs ?? 0) > Math.min(stats.deviceErrorRanges ?? 0, stats.unresolvedRanges ?? 0)
    || (stats.issuesComplete === true && (stats.savedIssueCount ?? 0) !== (stats.issueCount ?? 0))) return null;
  return Object.fromEntries(keys.map((key) => [key, stats[key] ?? 0]));
}

export function scanIssueDisplayCount(job) {
  const summary = scanOutcomeSummary(job);
  const rawCount = summary?.issueCount ?? job?.result?.stats?.badRanges?.length ?? 0;
  const candidates = job?.result?.stats?.repair?.candidates;
  const severe = job?.type === "scan" && Array.isArray(job.result?.map)
    ? job.result.map.filter((cell) => scanSlowBadBlock(cell,Boolean(job.disk?.rotational)) || (severeScanStatus(cell?.status) && !(cell.skippedBytes > 0))).length : 0;
  const pairs = summary?.timeoutSkipPairs ?? 0;
  if (!summary || !safeNonNegativeInteger(candidates)) return rawCount - pairs + severe;
  // A repair candidate has one row even when it has both intent and result events.
  const count = summary.unreadableRanges + summary.recoveredRanges
    + summary.deviceErrorRanges + summary.unresolvedRanges + candidates;
  return (count > 0 && count <= rawCount ? count : rawCount) - pairs + severe;
}

export function validScanIssuePage(page, diskSize, offset) {
  return record(page) && page.offset === offset
    && safeNonNegativeInteger(page.total) && safeNonNegativeInteger(page.next)
    && page.next >= offset && page.next <= page.total
    && typeof page.complete === "boolean" && typeof page.legacy === "boolean"
    && Array.isArray(page.records) && page.records.length <= 200
    && page.next - offset === page.records.length && validBadRanges(page.records, diskSize)
    && page.records.every((range) => typeof range.code === "string"
      && (range.outcome === undefined || ["recovered", "unreadable", "device_error", "unresolved", "rewrite", "severe"].includes(range.outcome))
      && (range.outcome !== "severe" || (["SEVERE_SLOW_READ","PERSISTENT_SLOW_READ"].includes(range.code) && Boolean(text(range.detail))))
      && validScanRepairRecord(range));
}

export function scanRangeOutcomeLabel(range) {
	if (range?.skippedRange) return "读取超时，已跳过";
	if (range?.code === "SLOW_READ_LIMIT") return "严重卡读，已跳过";
	if (range?.outcome === "severe") return range.code === "PERSISTENT_SLOW_READ" ? "坏块（慢速）" : "严重慢读（待确认）";
	if (range?.outcome === "rewrite") return scanRepairOutcomeLabel(range.repair);
  if (range?.outcome === "unreadable") return "坏块（不可读）";
  if (range?.outcome === "recovered") return range.code === "RECOVERED_ERROR" ? "固件已纠正" : "复读已恢复";
  if (range?.outcome === "device_error") return range.recovered === true ? "设备异常已恢复" : "设备异常";
  if (range?.outcome === "unresolved") return range.code === "LOCALIZATION_LIMIT" ? "定位达到限额，待细查" : "范围未确定";
  return "历史异常";
}

export function scanRangeLocation(range, logicalSectorSize) {
  const sector = Number.isSafeInteger(logicalSectorSize) && logicalSectorSize > 0 ? logicalSectorSize : null;
  const validRange = record(range) && safeNonNegativeInteger(range.offset)
    && Number.isSafeInteger(range.length) && range.length > 0
    && Number.isSafeInteger(range.offset + range.length);
  let precision = "unknown";
  if (range?.outcome === "unresolved" || ["LOCALIZATION_LIMIT", "SLOW_READ_LIMIT"].includes(range?.code)) {
    precision = "unresolved";
  } else if (range?.outcome && range.outcome !== "unreadable") {
    precision = "request";
  } else if (validRange && range.outcome === "unreadable" && sector
    && range.offset % sector === 0 && range.length % sector === 0
    && Number.isSafeInteger(range.probeBytes) && range.probeBytes >= range.length && range.probeBytes % sector === 0
    && range.recovered === false && ["EIO", "EREMOTEIO", "SHORT_READ", "READ_RETRY", "MEDIUM_ERROR", "DATA_INTEGRITY_ERROR", "RECOVERED_ERROR"].includes(range.code)) {
    precision = range.probeBytes === sector && range.length === sector ? "sector" : "range";
  }
  return {
    sectorBytes: sector,
    startLba: validRange && sector ? Math.floor(range.offset / sector) : null,
    endLba: validRange && sector ? Math.floor((range.offset + range.length - 1) / sector) : null,
    precision,
    label: range?.skippedRange ? "请求范围 · 跳过部分待定位" : { sector: "逐扇区确认", range: "范围确认（未逐扇区）", unresolved: "待定位（未确认坏扇区）", unknown: "定位精度未记录", request: "请求范围" }[precision]
  };
}

export function scanCellLocationSummary(cell, ranges, logicalSectorSize) {
  const locations = (Array.isArray(ranges) ? ranges : []).map((range) => scanRangeLocation(range, logicalSectorSize));
  const exact = locations.some((value) => value.precision === "sector");
  const coarse = locations.some((value) => value.precision === "range");
  const incomplete = cell?.unresolvedBytes > 0 || locations.some((value) => value.precision === "unresolved");
  if (exact) return `已定位到坏扇区${incomplete ? " · 其余待定位" : coarse ? " · 另有未细分范围" : ""}`;
  if (coarse) return `已确认不可读范围 · 未细分到扇区${incomplete ? " · 其余待定位" : ""}`;
  if (cell?.skippedBytes > 0) return "未完成 · 卡读跳过";
  if (incomplete) return locations.some((value, index) => value.precision === "unresolved" && ranges[index].code === "LOCALIZATION_LIMIT")
    ? "未完成 · 定位限额已到" : "定位未完成";
  if (cell?.unreadableBytes > 0) return "旧记录未保存定位精度";
  if (cell?.errorCount > 0) return "未确认坏扇区 · 查看读取异常";
  return !cell || ["pending", "reading"].includes(cell.status) ? "尚未完成检测" : "本次已读范围未发现不可读扇区";
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

export function findLatestScanJob(jobs, disks) {
  if (!Array.isArray(jobs) || jobs.some((job) => ACTIVE_JOB_STATUSES.has(job?.status))) return null;
  const order = (job) => Number.isSafeInteger(job.historyOrder) ? job.historyOrder : 0;
  return jobs.filter((job) => ["scan", "write"].includes(job?.type) && findCurrentDiskForJob(disks, job))
    .sort((a, b) => order(b) - order(a) || (Date.parse(b.createdAt) || 0) - (Date.parse(a.createdAt) || 0))[0] || null;
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

export function speedTargetDefaults(target, disks) {
  const solidState = record(target) && (target.type === "raw"
    ? target.disk?.rotational === false
    : target.type === "filesystem" && storageSpaceUsesOnlySolidStateDisks(target.storageSpace, disks));
  return solidState ? { speedQueueDepth: 64, speedThreads: 8 } : { speedQueueDepth: 8, speedThreads: 1 };
}

export function storageSpaceLabel(space) {
  if (record(space) && space.isSystem === true) return "系统盘";
  const path = text(record(space) ? space.path : space);
  const volume = /^\/vol(\d+)$/.exec(path);
  return volume ? `存储空间 ${volume[1]}` : path;
}

export function rawSpeedDiskBlockedReason(disk, storageSpaces = []) {
  if (!record(disk) || !text(disk.path)) return "磁盘信息无效";
  const spaces = storageSpacesForDisk(storageSpaces, disk);
  if (spaces.length > 0) return `由 ${spaces.map(storageSpaceLabel).filter(Boolean).join("、")} 提供文件测速`;
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
  // A residual ZFS label is not proof of current use. The backend must obtain
  // an exclusive device handle before starting a repair scan.
  const blockedFileSystems = Array.isArray(disk.fileSystems)
    ? disk.fileSystems.map(text).filter((value) => /raid|bcache|lvm|luks|^swap$/i.test(value))
    : [];
  if (blockedFileSystems.length > 0) return `检测到 ${blockedFileSystems.join("、")} 标记，不能直接执行慢扇区修复`;
  return "";
}

export function recordedRepairCandidates(job, threshold = 1) {
  if (job?.type !== "scan" || job.status !== "completed" || job.options?.scanRepairFrom || job.result?.version !== 2
    || !["full", "sampled"].includes(job.result?.coverage) || !Number.isFinite(threshold) || threshold < 1) return [];
  const map = safeScanMap(job.result.map);
  const sector = job.disk?.logicalSectorSize;
  if (![512, 4096].includes(sector) || ![2048, 4096].includes(map.length) || map.at(-1)?.end !== job.disk?.size) return [];
  return map.filter((cell) => {
    const read = cell.maxReadLatency;
    if (!read || read.ms < threshold || cell.errorCount || cell.skippedBytes || !["readable", "recovered"].includes(cell.readOutcome)
      || read.offset % sector !== 0 || read.bytes % sector !== 0) return false;
    if (job.result.coverage !== "sampled") return true;
    const ranges = cell.sampleRanges?.length ? cell.sampleRanges : [{ start: cell.sampleStart, end: cell.sampleEnd }];
    return ranges.some((span) => Number.isSafeInteger(span.start) && Number.isSafeInteger(span.end)
      && span.start >= cell.start && span.end <= cell.end && read.offset >= span.start && read.offset + read.bytes <= span.end);
  });
}

export function isCompleteRecordedRepairResult(job) {
  const result = job?.result;
  const targets = result?.repairTargets;
  const map = result?.map;
  const repair = result?.stats?.repair;
  if (!job?.options?.scanRepairFrom || job.status !== "completed" || result?.coverage !== "targeted"
    || !Array.isArray(targets) || !targets.length || !Array.isArray(map) || map.length !== targets.length
    || !repair || !["candidates", "pending", "verified", "stillSlow", "skipped", "failed", "writtenBytes"].every((key) => safeNonNegativeInteger(repair[key]))
    || repair.pending !== 0 || repair.candidates !== targets.length
    || !optionalSafeNonNegativeInteger(repair.improved) || (repair.improved || 0) > repair.verified
    || repair.verified + repair.stillSlow + repair.skipped + repair.failed !== targets.length
    || result.stats.testedCells !== targets.length || result.stats.issuesComplete !== true) return false;
  return targets.every((target, index) => {
    const cell = map[index];
    const read = target?.read;
    return Number.isSafeInteger(target?.cellIndex) && target.cellIndex >= 0 && target.cellIndex < 4096
      && finiteNonNegative(read?.ms) && Number.isSafeInteger(read?.offset) && read.offset >= 0
      && Number.isSafeInteger(read?.bytes) && read.bytes > 0 && read.bytes <= 128 * 1024
      && read.offset + read.bytes <= job.disk?.size
      && read.offset % job.disk.logicalSectorSize === 0 && read.bytes % job.disk.logicalSectorSize === 0
      && cell?.index === index && cell.start === read.offset && cell.end === read.offset + read.bytes
      && TERMINAL_SCAN_STATUSES.has(cell.status);
  });
}

export function validScanRepairRecord(range) {
  if (range?.outcome !== "rewrite") return true;
  const repair = range.repair;
  return record(repair) && ["pending", "uncertain", "verified", "still_slow", "skipped", "failed"].includes(repair.outcome)
    && finiteNonNegative(repair.beforeMs) && optionalFiniteNonNegative(repair.afterMs)
    && safeNonNegativeInteger(repair.writtenBytes) && repair.writtenBytes <= range.length
    && optionalFiniteNonNegative(repair.currentBeforeMs) && optionalFiniteNonNegative(repair.beforeRepeatMs)
    && (repair.speedChecks == null || (Array.isArray(repair.speedChecks) && repair.speedChecks.length <= 2 && repair.speedChecks.every(finiteNonNegative)))
    && (repair.speedOutcome == null || ["improved","still_slow","unconfirmed"].includes(repair.speedOutcome))
    && (repair.speedOutcome !== "improved" || (repair.outcome === "verified" && repair.speedChecks?.length === 2 && repair.speedLoad?.backgroundActivity === false && repair.beforeLoad?.backgroundActivity === false))
    && (repair.speedOutcome !== "still_slow" || (repair.outcome === "still_slow" && repair.speedChecks?.length === 2 && repair.speedLoad?.backgroundActivity === false));
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

export function activeJobForTarget(jobs, mode, disk, speedTarget) {
  return (Array.isArray(jobs) ? jobs : []).find(job => ACTIVE_JOB_STATUSES.has(job?.status)
    && (mode === "speed"
      ? job.type === "speed" && jobBelongsToSpeedTarget(job, speedTarget)
      : ["scan", "write"].includes(job.type) && jobBelongsToDisk(job, disk))) || null;
}

export function taskStartBlockedReason(jobs, mode, disk) {
  const active = (Array.isArray(jobs) ? jobs : []).filter(job => ACTIVE_JOB_STATUSES.has(job?.status));
  if (mode === "speed") return active.length ? "测速需等待其他检测任务结束" : "";
  if (active.some(job => job.type !== "scan")) return "请等待测速或其他检测任务结束后再扫描";
  if (active.some(job => job.device === disk?.path
    || (text(job.disk?.serial) && text(job.disk?.serial) === text(disk?.serial)))) {
    return "这块硬盘已有任务运行或正在停止，请等待任务结束";
  }
  return "";
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
      || (stats.skippedBytes ?? 0) !== map.reduce((sum, cell) => sum + (cell.skippedBytes ?? 0), 0)
      || (stats.unresolvedBytes ?? 0) !== map.reduce((sum, cell) => sum + (cell.unresolvedBytes ?? 0), 0)
      || map.some((cell) => cell.coveredBytes + (cell.unresolvedBytes ?? 0) !== (coverage === "sampled" ? sampledCellBytes(cell) : cell.end - cell.start))) return false;
  }

  if (coverage === "full") {
    if (job.result.sampleBytes != null
      || expectedBytes > job.disk.size
      || (expectedErrors === 0 && expectedBytes + (job.type === "scan" && job.result.version === 2 ? stats.skippedBytes ?? 0 : 0) !== job.disk.size)) {
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

export function speedHistorySummary(job) {
  if (job?.type !== "speed") return null;
  const results = Array.isArray(job.result?.benchmarks) ? job.result.benchmarks : job.speedSummary?.benchmarks;
  const valid = (Array.isArray(results) ? results : []).filter((benchmark) => record(benchmark)
    && ["read", "write", "mixed"].includes(benchmark.operation)
    && ["sequential", "random"].includes(benchmark.pattern)
    && Number.isFinite(benchmark.averageMBps) && benchmark.averageMBps >= 0);
  const pattern = valid.some((benchmark) => benchmark.pattern === "sequential") ? "sequential" : "random";
  const primary = valid.filter((benchmark) => benchmark.pattern === pattern);
  if (!primary.length) return null;
  const read = primary.find((benchmark) => benchmark.operation === "read");
  const write = primary.find((benchmark) => benchmark.operation === "write");
  const mixed = primary.find((benchmark) => benchmark.operation === "mixed");
  const rate = (benchmark) => benchmark ? `${benchmark.averageMBps.toLocaleString("zh-CN", { maximumFractionDigits: 1 })} MB/s` : "—";
  const prefix = pattern === "sequential" ? "" : "随机";
  const text = read || write ? `${prefix}读 ${rate(read)} · 写 ${rate(write)}` : `${prefix}混合 ${rate(mixed)}`;
  const details = [read, write, ...(!read && !write ? [mixed] : [])].filter(Boolean).map((benchmark) => {
    const block = Number.isSafeInteger(benchmark.blockSize) && benchmark.blockSize > 0
      ? (benchmark.blockSize % 1048576 === 0 ? `${benchmark.blockSize / 1048576} MiB` : `${benchmark.blockSize / 1024} KiB`) : "块大小未记录";
    return `${pattern === "sequential" ? "顺序" : "随机"}${{ read: "读取", write: "写入", mixed: "混合读写" }[benchmark.operation]} · ${block} Q${benchmark.queueDepth || 1}T${benchmark.threads || 1} · ${rate(benchmark)}`;
  });
  return { text, tooltip: details.join("\n") };
}

export function speedBenchmarkCells(job, rows) {
  const options = job.options || {};
  const cells = rows.flatMap((row) => ["read", "write", "mixed"].map((operation) => ({
    row, operation, benchmark: null, active: false,
    skipped: (Boolean(options.speedPattern) && options.speedPattern !== row.pattern)
      || (["read", "write", "mixed"].includes(options.speedOperation) && options.speedOperation !== operation)
      || (Boolean(options.speedProfileRow) && options.speedProfileRow !== row.profileRow)
      || (options.speedProfile !== true && [
        [options.speedBlockSize, row.blockSize], [options.speedQueueDepth, row.queueDepth], [options.speedThreads, row.threads]
      ].some(([filter, value]) => filter > 0 && filter !== value))
  })));
  // Results arrive in execution order. Consume each once: Q1T1 profile rows
  // can share every I/O parameter but must retain separate results and states.
  for (const benchmark of Array.isArray(job.result?.benchmarks) ? job.result.benchmarks : []) {
    if (!record(benchmark) || !Number.isFinite(benchmark.averageMBps)) continue;
    const cell = cells.find(({ row, operation, skipped, benchmark: assigned }) => !skipped && !assigned
      && benchmark.operation === operation && benchmark.pattern === row.pattern
      && benchmark.blockSize === row.blockSize
      && (benchmark.queueDepth ?? 1) === row.queueDepth
      && (benchmark.threads ?? 1) === row.threads);
    if (cell) cell.benchmark = benchmark;
  }
  const next = cells.find((cell) => !cell.skipped && !cell.benchmark);
  const phase = text(job.phase);
  const stage = /^(随机|(?:头部|中部|尾部)位置顺序)(混合读写|读取|写入)/.exec(phase);
  const parameters = /Q(\d+)T(\d+)/.exec(phase);
  // Preparation/TRIM are not a matrix test. Require both the next unfinished
  // item and the actual I/O phase to agree, including during multi-run tests.
  if (job.type === "speed" && job.status === "running" && next && stage && parameters) {
    next.active = next.row.pattern === (stage[1] === "随机" ? "random" : "sequential")
      && next.operation === ({ 读取: "read", 写入: "write", 混合读写: "mixed" })[stage[2]]
      && next.row.queueDepth === Number(parameters[1]) && next.row.threads === Number(parameters[2]);
  }
  return cells;
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
