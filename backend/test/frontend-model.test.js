"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");

let jobBelongsToDisk;
let findCurrentDiskForJob;
let findResumableScan;
let mergeJobLists;
let mergeScanProgress;
let planScanReveal;
let advanceScanReveal;
let safeScanMap;
let scanStatusGroup;
let safeSpeedSamples;
let isCompleteScanResult;
let isCompleteSpeedResult;
let chooseShownJob;
let detailRetryDelay;
let storageSpaceForDisk;
let storageSpacesForDisk;
let storageSpaceUsesOnlySolidStateDisks;
let estimateRemainingMilliseconds;
let formatEstimatedDuration;
let terminalScanPrefix;
let buildSpeedTargets;
let jobBelongsToSpeedTarget;
let rawSpeedDiskBlockedReason;
let scanOutcomeSummary;
let scanRangeOutcomeLabel;
let validScanIssuePage;
let scanRepairDiskBlockedReason;

test.before(async () => {
  ({
    jobBelongsToDisk,
    findCurrentDiskForJob,
    findResumableScan,
    mergeJobLists,
    mergeScanProgress,
    planScanReveal,
    advanceScanReveal,
    safeScanMap,
    scanStatusGroup,
    safeSpeedSamples,
    isCompleteScanResult,
    isCompleteSpeedResult,
    chooseShownJob,
    detailRetryDelay,
    estimateRemainingMilliseconds,
    formatEstimatedDuration,
    terminalScanPrefix,
    storageSpaceForDisk,
    storageSpacesForDisk,
    storageSpaceUsesOnlySolidStateDisks,
    buildSpeedTargets,
    jobBelongsToSpeedTarget,
    rawSpeedDiskBlockedReason,
    scanOutcomeSummary,
    scanRangeOutcomeLabel,
    validScanIssuePage,
    scanRepairDiskBlockedReason
  } = await import("../../frontend/src/job-model.js"));
});

test("scan ETA uses elapsed time and actual task progress", () => {
  const startedAt = "2026-09-11T10:00:00.000Z";
  const now = Date.parse("2026-09-11T10:10:00.000Z");
  assert.equal(estimateRemainingMilliseconds({ type: "scan", status: "running", startedAt, progress: 25 }, now), 30 * 60_000);
  assert.equal(estimateRemainingMilliseconds({ type: "write", status: "running", startedAt, progress: 50 }, now), 10 * 60_000);
});

test("resume selects the newest unfinished scan by disk identity, even after a path change", () => {
  const disk = { path: "/dev/sdc", serial: "SAME", model: "Disk", size: 4096000, logicalSectorSize: 512, physicalSectorSize: 4096 };
  const job = { id: "old", type: "scan", device: "/dev/sdb", disk: { ...disk, path: undefined }, status: "cancelled", historyOrder: 1, options: { scanMode: "full" }, resume: { cellIndex: 10, cellCount: 4096, offset: 10000, progress: 0.2 } };
  const newer = { ...job, id: "newer", status: "interrupted", historyOrder: 2 };
  assert.equal(findResumableScan([job, newer], disk), newer);
  assert.equal(findResumableScan([job], { ...disk, serial: "OTHER" }), null);
  assert.equal(findResumableScan([job], { ...disk, logicalSectorSize: 4096 }), null);
  assert.equal(findResumableScan([job], { ...disk, serial: "" }), null);
  assert.equal(findResumableScan([{ ...job, resume: null }], disk), null);
});

test("resume skips superseded history and scans older than a completed run", () => {
  const disk = { path: "/dev/sdb", serial: "SAME", model: "Disk", size: 4096000, logicalSectorSize: 512, physicalSectorSize: 4096 };
  const old = { id: "old", type: "scan", disk, status: "cancelled", historyOrder: 1, options: { scanMode: "quick" }, resume: { cellIndex: 10, cellCount: 4096, offset: 10000, progress: 0.2 } };
  const resumed = { ...old, id: "resumed", historyOrder: 2, resumeFrom: "old" };
  assert.equal(findResumableScan([old, resumed], disk), resumed);
  assert.equal(findResumableScan([old, { ...resumed, status: "running", resume: null }], disk), null);
  assert.equal(findResumableScan([old, { ...resumed, status: "failed", resume: null }], disk), old);
  assert.equal(findResumableScan([old, { ...resumed, status: "completed", resume: null }], disk), null);
  const full = { ...old, options: { scanMode: "full" } };
  assert.equal(findResumableScan([full, { ...resumed, resumeFrom: "", status: "completed", resume: null }], disk), full);
  assert.equal(findResumableScan([{ ...old, resume: { ...old.resume, cellIndex: 4096 } }], disk), null);
});

test("resumed scan ETA measures only progress made after resuming", () => {
  const startedAt = "2026-10-14T10:00:00.000Z";
  const job = { type: "scan", status: "running", startedAt, progress: 60, resumeStartProgress: 50 };
  assert.equal(estimateRemainingMilliseconds(job, Date.parse("2026-10-14T10:10:00.000Z")), 40 * 60_000);
  assert.equal(estimateRemainingMilliseconds({ ...job, progress: 50 }, Date.parse("2026-10-14T10:10:00.000Z")), null);
});

test("scan ETA starts after the first measured second, including low progress", () => {
  const startedAt = "2026-09-11T10:00:00.000Z";
  const now = Date.parse("2026-09-11T10:10:00.000Z");
  assert.equal(estimateRemainingMilliseconds({ type: "scan", status: "running", startedAt, progress: 0.4 }, now), 149_400_000);
  assert.equal(estimateRemainingMilliseconds({ type: "scan", status: "running", startedAt, progress: 0 }, now), null);
  assert.equal(estimateRemainingMilliseconds({ type: "scan", status: "running", startedAt, progress: 5 }, Date.parse(startedAt) + 500), null);
  assert.equal(estimateRemainingMilliseconds({ type: "speed", status: "running", startedAt, progress: 25 }, now), null);
  assert.equal(estimateRemainingMilliseconds({ type: "scan", status: "completed", startedAt, progress: 100 }, now), null);
});

test("scan ETA uses completed samples when rounded progress still reads zero", () => {
  const startedAt = "2026-09-11T10:00:00.000Z";
  const now = Date.parse(startedAt) + 2_000;
  const quick = {
    type: "scan", status: "running", startedAt, progress: 0,
    result: { coverage: "sampled", map: Array(2048), stats: { testedCells: 2, bytesRead: 512 * 1024 } }
  };
  assert.equal(estimateRemainingMilliseconds(quick, now), 2_046_000);

  const full = {
    type: "scan", status: "running", startedAt, progress: 0,
    disk: { size: 8 * 1024 * 1024 * 1024 },
    result: { coverage: "full", stats: { bytesRead: 4 * 1024 * 1024 } }
  };
  assert.equal(estimateRemainingMilliseconds(full, now), 4_094_000);
  assert.equal(estimateRemainingMilliseconds({ ...full, result: { coverage: "full", stats: { bytesRead: 0 } } }, now), null);
});

test("estimated durations stay compact and readable", () => {
  assert.equal(formatEstimatedDuration(null), "计算中");
  assert.equal(formatEstimatedDuration(45_000), "少于 1 分钟");
  assert.equal(formatEstimatedDuration(38 * 60_000), "约 38 分钟");
  assert.equal(formatEstimatedDuration((2 * 60 + 14) * 60_000), "约 2 小时 14 分钟");
  assert.equal(formatEstimatedDuration((3 * 24 + 4) * 60 * 60_000), "约 3 天 4 小时");
});

test("scan cells reveal in a stable order across fast progress batches", () => {
  const first = planScanReveal(0, 6, 1_000);
  assert.deepEqual(first.delays, [0, 4, 8, 12, 16, 20]);
  const second = planScanReveal(first.finishAt, 100, 1_050);
  assert.equal(second.delays[0], 0);
  assert.ok(second.delays.every((delay, index) => index === 0 || delay - second.delays[index - 1] === 4));
  const queued = planScanReveal(second.finishAt, 20, 1_100);
  assert.equal(queued.delays[0], second.finishAt - 1_100);
  assert.ok(queued.delays[0] >= 0);
  assert.equal(queued.finishAt, second.finishAt + 19 * 4);
  assert.equal(planScanReveal(second.finishAt, 0, 1_100).delays.length, 0);
});

test("frame-driven scan reveal advances slow batches individually and caps fast catch-up", () => {
  assert.deepEqual(advanceScanReveal(10, 11, 17), { prefix: 11, carry: 0 });
  assert.deepEqual(advanceScanReveal(10, 13, 17), { prefix: 11, carry: 0 });
  const fast = advanceScanReveal(0, 2048, 17);
  assert.ok(fast.prefix >= 4 && fast.prefix <= 5);
  const delayedFrame = advanceScanReveal(0, 2048, 500);
  assert.ok(delayedFrame.prefix <= 9);
  assert.deepEqual(advanceScanReveal(20, 20, 17, 0.8), { prefix: 20, carry: 0 });
});

function job(overrides = {}) {
  return {
    device: "/dev/sda",
    disk: { serial: "SERIAL-A", model: "Disk A", size: 1_000_000 },
    ...overrides
  };
}

function disk(overrides = {}) {
  return {
    path: "/dev/sda",
    serial: "SERIAL-A",
    model: "Disk A",
    size: 1_000_000,
    ...overrides
  };
}

test("history does not follow a reused device path onto another disk", () => {
  assert.equal(jobBelongsToDisk(job(), disk({ serial: "SERIAL-B" })), false);
});

test("history follows the same serialled disk when its device path changes", () => {
  assert.equal(jobBelongsToDisk(job(), disk({ path: "/dev/sdb" })), true);
});

test("serial identity still rejects a conflicting capacity", () => {
  assert.equal(jobBelongsToDisk(job(), disk({ size: 2_000_000 })), false);
});

test("legacy jobs without serials use path, model, and capacity", () => {
  const legacy = job({ disk: { serial: "", model: "Disk A", size: 1_000_000 } });
  assert.equal(jobBelongsToDisk(legacy, disk({ serial: "" })), true);
  assert.equal(jobBelongsToDisk(legacy, disk({ serial: "", model: "Disk B" })), false);
  assert.equal(jobBelongsToDisk(legacy, disk({ path: "/dev/sdb", serial: "" })), false);
});

test("a one-sided serial never falls back to a reused device path", () => {
  const legacy = job({ disk: { serial: "", model: "Disk A", size: 1_000_000 } });
  assert.equal(jobBelongsToDisk(legacy, disk()), false);
  assert.equal(jobBelongsToDisk(job(), disk({ serial: "" })), false);
});

test("current disk lookup uses stable identity rather than the first matching path", () => {
  const disks = [
    disk({ serial: "SERIAL-B" }),
    disk({ path: "/dev/sdb" })
  ];
  assert.equal(findCurrentDiskForJob(disks, job())?.path, "/dev/sdb");
});

test("pending jobs on serialless disks retain their current path identity", () => {
  const currentDisk = disk({ serial: "" });
  const pending = { device: currentDisk.path, disk: currentDisk };
  assert.equal(findCurrentDiskForJob([currentDisk], pending), currentDisk);
});

test("speed targets map physical array members to the same storage space", () => {
  const spaces = [
    { path: "/vol1", members: ["/dev/sda"] },
    { path: "/vol2", members: ["/dev/sdb", "/dev/sdc"] }
  ];
  assert.deepEqual(storageSpacesForDisk(spaces, "/dev/sdb"), [spaces[1]]);
  assert.equal(storageSpaceForDisk(spaces, disk({ path: "/dev/sdc" })), spaces[1]);
  assert.equal(storageSpaceForDisk(spaces, "/dev/sdd"), null);
});

test("a preferred volume is retained when one disk belongs to multiple storage spaces", () => {
  const spaces = [
    { path: "/vol1", members: ["/dev/sda"] },
    { path: "/vol2", members: ["/dev/sda"] }
  ];
  assert.equal(storageSpaceForDisk(spaces, "/dev/sda", "/vol2"), spaces[1]);
  assert.equal(storageSpaceForDisk(spaces, "/dev/sda", "/vol9"), spaces[0]);
});

test("TRIM defaults on only when every storage-space member is solid state", () => {
  const disks = [
    disk({ path: "/dev/nvme0n1", rotational: false }),
    disk({ path: "/dev/sdb", rotational: false }),
    disk({ path: "/dev/sdc", rotational: true })
  ];
  assert.equal(storageSpaceUsesOnlySolidStateDisks({ path: "/vol1", members: ["/dev/nvme0n1"] }, disks), true);
  assert.equal(storageSpaceUsesOnlySolidStateDisks({ path: "/vol2", members: ["/dev/nvme0n1", "/dev/sdb"] }, disks), true);
  assert.equal(storageSpaceUsesOnlySolidStateDisks({ path: "/vol3", members: ["/dev/nvme0n1", "/dev/sdc"] }, disks), false);
  assert.equal(storageSpaceUsesOnlySolidStateDisks({ path: "/vol4", members: ["/dev/missing"] }, disks), false);
  assert.equal(storageSpaceUsesOnlySolidStateDisks({ path: "/vol5", members: [] }, disks), false);
});

test("speed targets use raw I/O whenever no filesystem or storage space is recognized", () => {
  const space = { path: "/vol1", members: ["/dev/sda"], fileSystem: "btrfs", size: 1000 };
  const targets = buildSpeedTargets([
    disk({ path: "/dev/sda", mountpoints: ["/vol1"], hasPartitions: true, fileSystems: ["btrfs"] }),
    disk({ path: "/dev/sdb", model: "Blank", mountpoints: [], hasPartitions: false, fileSystems: [], partitionTableUnknown: false }),
    disk({ path: "/dev/sdc", model: "Partitioned", mountpoints: [], hasPartitions: false, partitionTable: "gpt", partitionTableUnknown: false })
  ], [space]);
  assert.equal(targets.find((target) => target.key === "filesystem:/vol1").type, "filesystem");
  assert.equal(targets.find((target) => target.key === "raw:/dev/sda").disabledReason.includes("文件测速"), true);
  assert.equal(targets.find((target) => target.key === "raw:/dev/sdb").disabledReason, "");
  assert.equal(targets.find((target) => target.key === "raw:/dev/sdc").disabledReason, "");
  assert.equal(rawSpeedDiskBlockedReason(disk({ path: "/dev/sdd", hasPartitions: true, partitionTableUnknown: true, fileSystems: [] })), "");
  assert.equal(rawSpeedDiskBlockedReason(disk({ path: "/dev/sde", mountpoints: [], fileSystems: ["ext4"] })).includes("文件系统"), true);
});

test("mounted external disks use their filesystem target instead of raw I/O", () => {
  const externalDisk = disk({
    path: "/dev/sdg", model: "USB SSD", mountpoints: ["/media/Backup SSD"],
    hasPartitions: true, fileSystems: ["exfat"], transport: "usb"
  });
  const externalSpace = {
    path: "/media/Backup SSD", source: "/dev/sdg1", fileSystem: "exfat",
    members: ["/dev/sdg"], size: 2_000_000, freeBytes: 1_000_000, readOnly: false
  };
  const targets = buildSpeedTargets([externalDisk], [externalSpace]);
  assert.equal(targets.find((target) => target.key === "filesystem:/media/Backup SSD").disabledReason, "");
  assert.equal(targets.find((target) => target.key === "raw:/dev/sdg").disabledReason.includes("文件测速"), true);
});

test("raw speed history follows the physical target instead of its member volume", () => {
  const target = buildSpeedTargets([disk({ path: "/dev/sdb", mountpoints: [], hasPartitions: false, fileSystems: [], partitionTableUnknown: false })], [])[0];
  assert.equal(jobBelongsToSpeedTarget({ type: "speed", device: "/dev/sdb", disk: disk({ path: "/dev/sdb" }) }, target), true);
  assert.equal(jobBelongsToSpeedTarget({ type: "speed", device: "/vol1", storageSpace: { path: "/vol1" } }, target), false);
});

test("legacy filesystem speed history follows its volume members after upgrade", () => {
  const member = disk({ path: "/dev/sdb", serial: "SERIAL-B", model: "Disk B" });
  const target = buildSpeedTargets([member], [{
    path: "/vol2",
    fileSystem: "btrfs",
    members: ["/dev/sdb"],
    size: 500_000
  }])[0];
  const legacyJob = {
    type: "speed",
    device: "/dev/sdb",
    disk: { serial: "SERIAL-B", model: "Disk B", size: member.size }
  };
  assert.equal(jobBelongsToSpeedTarget(legacyJob, target), true);
  assert.equal(jobBelongsToSpeedTarget({ ...legacyJob, disk: { ...legacyJob.disk, serial: "SERIAL-C" } }, target), false);
});

test("fresh summaries clear a recovered persistence warning", () => {
  const current = [{ id: "job-1", status: "running", persistenceWarning: "保存失败", result: { map: [] } }];
  const next = [{ id: "job-1", status: "running", progress: 25 }];
  assert.deepEqual(mergeJobLists(current, next), [{ id: "job-1", status: "running", progress: 25, result: { map: [] } }]);
});

test("an active task takes precedence only in its own mode", () => {
  const activeJob = { id: "active", type: "speed" };
  const selectedJob = { id: "history" };
  const pendingScan = { id: "pending-scan", type: "scan" };
  assert.equal(chooseShownJob({ activeJob, selectedJob, modeJobs: [], fallbackJobs: [], pendingJob: null, mode: "speed" }), activeJob);
  assert.equal(chooseShownJob({ activeJob, selectedJob, modeJobs: [], fallbackJobs: [], pendingJob: pendingScan, mode: "scan" }), selectedJob);
  assert.equal(chooseShownJob({ activeJob, selectedJob: null, modeJobs: [], fallbackJobs: [], pendingJob: pendingScan, mode: "scan" }), pendingScan);
});

test("history detail retries only transient failures and stops after two retries", () => {
  assert.equal(detailRetryDelay({ status: 404 }, 0), null);
  assert.equal(detailRetryDelay({ status: 409 }, 0), null);
  assert.equal(detailRetryDelay({ status: 500 }, 0), 1000);
  assert.equal(detailRetryDelay({ status: 408 }, 1), 3000);
  assert.equal(detailRetryDelay({ code: "NETWORK_ERROR" }, 2), null);
});

const SCAN_STATUSES = [
  "normal",
  "slow",
  "severe",
  "pending",
  "reading",
  "under5",
  "under20",
  "under50",
  "under150",
  "under500",
  "over500",
  "error"
];

test("scan maps accept only supported statuses and preserve valid arrays", () => {
  for (const status of SCAN_STATUSES) {
    const map = [{ index: 0, start: 0, end: 4096, status }];
    assert.equal(safeScanMap(map), map);
  }
  assert.deepEqual(safeScanMap([{ index: 0, start: 0, end: 4096, status: "ok" }]), []);
  assert.deepEqual(safeScanMap("not-an-array"), []);
  assert.deepEqual(safeScanMap([{ index: 0, start: 0, end: 4096, status: "under5" }, null]), []);
});

test("seven display groups preserve legacy verdicts and split normalized results", () => {
  assert.equal(scanStatusGroup("under5"), "fast");
  assert.equal(scanStatusGroup("under20"), "good");
  for (const status of ["under50", "normal"]) assert.equal(scanStatusGroup(status), "normal");
  assert.equal(scanStatusGroup("under150"), "slow");
  for (const status of ["under500", "slow"]) assert.equal(scanStatusGroup(status), "slower");
  for (const status of ["over500", "severe"]) assert.equal(scanStatusGroup(status), "severe");
  assert.equal(scanStatusGroup("error"), "error");
  assert.equal(scanStatusGroup("pending"), "pending");
  for (const [status, ms, expected] of [
    ["normal", 0, "fast"], ["normal", 4.99, "fast"], ["normal", 5, "good"],
    ["normal", 19.99, "good"], ["normal", 20, "normal"],
    ["normal", 49.99, "normal"], ["slow", 50, "slow"], ["slow", 149.99, "slow"],
    ["slow", 150, "slower"], ["slow", 499.99, "slower"], ["severe", 500, "severe"],
    ["error", 1, "error"], ["pending", 1, "pending"], ["reading", 1, "reading"]
  ]) assert.equal(scanStatusGroup(status, ms), expected, `${status}: ${ms}`);
});

test("scan map indexes and byte ranges must be ordered, positive, and contiguous", () => {
  const valid = [
    { index: 0, start: 0, end: 4096, status: "under5" },
    { index: 1, start: 4096, end: 8192, status: "under20" }
  ];
  assert.equal(safeScanMap(valid), valid);

  const invalid = [
    [{ ...valid[0], index: 1 }],
    [{ ...valid[0], start: 1 }],
    [{ ...valid[0], end: 0 }],
    [valid[0], { ...valid[1], start: 4095 }],
    [valid[0], { ...valid[1], start: 4097 }],
    [{ ...valid[0], end: 4096.5 }],
    [{ ...valid[0], end: Number.MAX_SAFE_INTEGER + 1 }]
  ];
  for (const map of invalid) assert.deepEqual(safeScanMap(map), []);
});

test("request latency verdicts survive normalized display and completed history validation", () => {
  assert.equal(scanStatusGroup("severe", 21.5), "severe");
  assert.equal(scanStatusGroup("slower", 2), "slower");
  assert.equal(scanStatusGroup("slow", 2), "slow");
  const map = Array.from({ length: SCAN_CELL_COUNT }, (_, index) => scanCell(index, "full", {
    status: index === 0 ? "severe" : index === 1 ? "slower" : "normal",
    maxUnitMs: 2,
    maxReadLatency: { ms: index === 0 ? 172 : index === 1 ? 60 : 1, bytes: 4096, offset: index * SCAN_CELL_SIZE }
  }));
  const job = completedScan({ map, result: { latencyBasis: "solid-dual-v2" } });
  assert.equal(safeScanMap(map), map);
  assert.equal(isCompleteScanResult(job), true);
  assert.equal(terminalScanPrefix(map), map.length);
  for (const sample of [
    { ms: -1, bytes: 4096, offset: 0 },
    { ms: 172, bytes: 131073, offset: 0 },
    { ms: 172, bytes: 4096, offset: 512 },
    { ms: Infinity, bytes: 4096, offset: 0 }
  ]) assert.deepEqual(safeScanMap([{ ...map[0], maxReadLatency: sample }]), []);
});

test("scan map optional numeric fields must be finite and non-negative", () => {
  const base = { index: 0, start: 0, end: 4096, status: "under5" };
  const valid = [{ ...base, maxMs: null, avgMs: 0.5, totalMs: 0, reads: 0, bytesRead: 0, errorCount: 0 }];
  assert.equal(safeScanMap(valid), valid);

  for (const [field, value] of [
    ["maxMs", Number.NaN],
    ["avgMs", Number.POSITIVE_INFINITY],
    ["totalMs", -0.1],
    ["reads", -1],
    ["bytesRead", -1],
    ["errorCount", -1]
  ]) {
    assert.deepEqual(safeScanMap([{ ...base, [field]: value }]), [], field);
  }
  assert.deepEqual(safeScanMap([{ ...base, sampleStart: 0, sampleEnd: Number.POSITIVE_INFINITY }]), []);
  assert.deepEqual(safeScanMap([{ ...base, sampleStart: 0 }]), []);
});

test("deep scan history retains independent read and write latency verdicts", () => {
  const map = Array.from({ length: SCAN_CELL_COUNT }, (_, index) => scanCell(index, "full", {
    status: index === 0 ? "severe" : "normal",
    maxUnitMs: 1,
    maxReadLatency: { ms: 1, bytes: 4096, offset: index * SCAN_CELL_SIZE },
    maxWriteLatency: { ms: index === 0 ? 600 : 2, bytes: 4096, offset: index * SCAN_CELL_SIZE }
  }));
  const job = completedScan({ type: "write", options: {}, map, result: { scanMode: "write", latencyBasis: "solid-write-dual-v2" } });
  assert.equal(safeScanMap(map), map);
  assert.equal(isCompleteScanResult(job), true);
  assert.equal(scanStatusGroup(map[0].status, map[0].maxUnitMs), "severe");
  for (const invalid of [
    { ms: NaN, bytes: 4096, offset: 0 },
    { ms: 1, bytes: 0, offset: 0 },
    { ms: 1, bytes: 131073, offset: 0 },
    { ms: 1, bytes: 512, offset: 4096 }
  ]) assert.deepEqual(safeScanMap([{ ...map[0], maxWriteLatency: invalid }]), []);
});

function speedSample(label, offsetBytes, mbps, bytes = 4 * 1024 * 1024) {
  return { label, offsetBytes, bytes, seconds: bytes / (mbps * 1e6), mbps };
}

test("speed samples are validated atomically instead of filtering bad entries", () => {
  const head = speedSample("头部", 0, 200);
  const middle = speedSample("中部", 8 * 1024 * 1024, 180);
  const valid = [head, middle];
  assert.equal(safeSpeedSamples(valid), valid);
  assert.deepEqual(safeSpeedSamples({ bad: true }), []);
  assert.deepEqual(safeSpeedSamples([head, null, middle]), []);
  assert.deepEqual(safeSpeedSamples([head, { ...head }]), []);
  assert.deepEqual(safeSpeedSamples([{ ...head, label: "unknown" }]), []);
});

test("speed sample offsets and measurements must satisfy strict numeric bounds", () => {
  const base = speedSample("头部", 0, 200);
  assert.equal(safeSpeedSamples([{ ...base, mbps: 400 }]).length, 1);
  for (const [field, value] of [
    ["offsetBytes", -1],
    ["offsetBytes", 0.5],
    ["offsetBytes", Number.MAX_SAFE_INTEGER + 1],
    ["bytes", 0],
    ["bytes", 1.5],
    ["bytes", Number.MAX_SAFE_INTEGER + 1],
    ["seconds", 0],
    ["seconds", -1],
    ["seconds", Number.POSITIVE_INFINITY],
    ["mbps", -1],
    ["mbps", Number.NaN]
  ]) {
    assert.deepEqual(safeSpeedSamples([{ ...base, [field]: value }]), [], field);
  }
  assert.equal(safeSpeedSamples([{ ...base, mbps: 0 }]).length, 1);
});

test("a throughput rounded to zero remains a valid measured speed", () => {
  const slow = speedSample("中部", 12 * 1024 * 1024, 1);
  slow.seconds = 100_000_000;
  slow.mbps = 0;
  assert.equal(isCompleteSpeedResult(completedSpeed({ benchmarks: [benchmarkResult([slow])] })), true);
});

const SCAN_CELL_COUNT = 2048;
const SCAN_CELL_SIZE = 4096;
const SCAN_DISK_SIZE = SCAN_CELL_COUNT * SCAN_CELL_SIZE;
const QUICK_SAMPLE_SIZE = 1024;

function scanCell(index, coverage = "full", overrides = {}) {
  const start = index * SCAN_CELL_SIZE;
  const sampled = coverage === "sampled";
  return {
    index,
    start,
    end: start + SCAN_CELL_SIZE,
    status: "under5",
    maxMs: 2,
    avgMs: 2,
    totalMs: 2,
    reads: 1,
    bytesRead: sampled ? QUICK_SAMPLE_SIZE : SCAN_CELL_SIZE,
    errorCount: 0,
    sampleStart: sampled ? start + 512 : null,
    sampleEnd: sampled ? start + 512 + QUICK_SAMPLE_SIZE : null,
    ...overrides
  };
}

function scanStats(map, overrides = {}) {
  return {
    testedCells: map.filter((cell) => !["pending", "reading"].includes(cell.status)).length,
    errorCells: map.filter((cell) => cell.status === "error").length,
    slowCells: map.filter((cell) => ["slow", "slower", "severe", "under150", "under500", "over500"].includes(cell.status)).length,
    bytesRead: map.reduce((sum, cell) => sum + cell.bytesRead, 0),
    badRanges: [],
    ...overrides
  };
}

function completedScan({ coverage = "full", map, diskSize = SCAN_DISK_SIZE, status = "completed", type = "scan", options, result = {} } = {}) {
  const cells = map || Array.from({ length: SCAN_CELL_COUNT }, (_, index) => scanCell(index, coverage));
  const scanMode = coverage === "sampled" ? "quick" : "full";
  return {
    type,
    status,
    disk: { size: diskSize },
    options: options || { scanMode },
    result: {
      scanMode,
      coverage,
      sampleBytes: coverage === "sampled" ? QUICK_SAMPLE_SIZE : null,
      map: cells,
      stats: scanStats(cells),
      ...result
    }
  };
}

test("distributed samples validate actual read segments rather than the untested gap", () => {
  const map = Array.from({ length: 4096 }, (_, index) => {
    const start = index * SCAN_CELL_SIZE;
    return scanCell(index, "sampled", { sampleStart: start, sampleEnd: start + SCAN_CELL_SIZE,
      sampleRanges: [{ start, end: start + 512 }, { start: start + SCAN_CELL_SIZE - 512, end: start + SCAN_CELL_SIZE }],
      coveredBytes: 1024, readOutcome: "readable" });
  });
  const job = completedScan({ coverage: "sampled", map, diskSize: 4096 * SCAN_CELL_SIZE });
  Object.assign(job.result, { version: 2, samplingPlan: "stratified-v1", samplingSeed: 123 });
  Object.assign(job.result.stats, { coveredBytes: 4096 * 1024, issuesComplete: true });
  assert.equal(isCompleteScanResult(job), true);
  for (const sampleRanges of [[], [{ start: 0, end: 2048 }, { start: 1024, end: 4096 }], [{ start: 0, end: 4097 }]]) {
    const invalid = { ...job, result: { ...job.result, map: [{ ...map[0], sampleRanges }, ...map.slice(1)] } };
    assert.equal(isCompleteScanResult(invalid), false);
  }
  assert.equal(isCompleteScanResult({ ...job, result: { ...job.result, samplingPlan: "unknown" } }), false);
});

test("localization limits retain unknown bytes without claiming full confirmed coverage", () => {
  const map = Array.from({ length: SCAN_CELL_COUNT }, (_, index) => scanCell(index, "full", {
    coveredBytes: index === 0 ? 0 : SCAN_CELL_SIZE,
    unresolvedBytes: index === 0 ? SCAN_CELL_SIZE : 0,
    bytesRead: index === 0 ? 0 : SCAN_CELL_SIZE,
    status: index === 0 ? "error" : "normal", errorCount: index === 0 ? 1 : 0,
    readOutcome: index === 0 ? "unresolved" : "readable"
  }));
  const job = completedScan({ map, result: { version: 2, scanMode: "full", coverage: "full", sampleBytes: null, map,
    stats: scanStats(map, { coveredBytes: SCAN_DISK_SIZE - SCAN_CELL_SIZE, unresolvedBytes: SCAN_CELL_SIZE,
      issueCount: 1, savedIssueCount: 1, unresolvedRanges: 1, issuesComplete: true }) } });
  assert.equal(isCompleteScanResult(job), true);
  assert.equal(scanOutcomeSummary(job).coveredBytes, SCAN_DISK_SIZE - SCAN_CELL_SIZE);
  assert.equal(scanRangeOutcomeLabel({ code: "LOCALIZATION_LIMIT", outcome: "unresolved" }), "定位达到限额，待细查");
  assert.equal(isCompleteScanResult({ ...job, result: { ...job.result, stats: { ...job.result.stats, unresolvedBytes: 0 } } }), false);
  assert.deepEqual(safeScanMap([{ ...map[0], coveredBytes: 1 }]), []);
  const range = { offset: 0, length: 512, code: "LOCALIZATION_LIMIT", outcome: "unresolved", unresolvedBytes: 512 };
  const page = { offset: 0, next: 1, total: 1, complete: true, legacy: false, records: [range] };
  assert.equal(validScanIssuePage(page, 4096, 0), true);
  for (const patch of [{ unresolvedBytes: 513 }, { unresolvedBytes: -1 }, { code: "EIO" }, { outcome: "unreadable" }]) {
    assert.equal(validScanIssuePage({ ...page, records: [{ ...range, ...patch }] }, 4096, 0), false);
  }
});

test("scan progress merges completed cells without replacing the whole map", () => {
  const pending = Array.from({ length: SCAN_CELL_COUNT }, (_, index) => scanCell(index, "full", {
    status: "pending", maxMs: null, avgMs: null, totalMs: null, reads: 0, bytesRead: 0
  }));
  const job = { ...completedScan({ map: pending, status: "running" }), id: "scan-progress-job" };
  const cells = [scanCell(0), scanCell(1)];
  const merged = mergeScanProgress(job, {
    id: job.id,
    status: "running",
    progress: 0.1,
    phase: "完整读取 3/2048",
    message: "",
    cells,
    next: 2,
    stats: scanStats(cells)
  });
  assert.equal(merged.result.map[0].status, "under5");
  assert.equal(merged.result.map[1].status, "under5");
  assert.equal(merged.result.map[2].status, "pending");
  assert.equal(merged.result.map.length, SCAN_CELL_COUNT);
});

test("stale full scan refreshes cannot punch gaps into fast incremental progress", () => {
  const pending = Array.from({ length: SCAN_CELL_COUNT }, (_, index) => scanCell(index, "full", {
    status: "pending", maxMs: null, avgMs: null, totalMs: null, reads: 0, bytesRead: 0
  }));
  const progressedMap = pending.map((cell, index) => index < 500 ? scanCell(index) : cell);
  const staleMap = pending.map((cell, index) => index < 200 ? scanCell(index) : cell);
  const current = {
    ...completedScan({ map: progressedMap, status: "running" }),
    id: "fast-scan-job",
    progress: 24.4,
    phase: "完整读取 500/2048",
    message: "newer"
  };
  const stale = {
    ...completedScan({ map: staleMap, status: "running" }),
    id: current.id,
    progress: 9.8,
    phase: "完整读取 200/2048",
    message: "stale"
  };

  const [afterRefresh] = mergeJobLists([current], [stale]);
  assert.equal(afterRefresh.result, current.result);
  assert.equal(afterRefresh.progress, current.progress);
  assert.equal(afterRefresh.phase, current.phase);
  assert.equal(afterRefresh.message, current.message);

  const nextCells = Array.from({ length: 50 }, (_, offset) => scanCell(500 + offset));
  const afterIncrement = mergeScanProgress(afterRefresh, {
    id: current.id,
    status: "running",
    progress: 26.9,
    phase: "完整读取 550/2048",
    message: "",
    cells: nextCells,
    next: 550,
    stats: scanStats([...progressedMap.slice(0, 500), ...nextCells])
  });
  assert.equal(afterIncrement.result.map.slice(0, 550).every((cell) => !["pending", "reading"].includes(cell.status)), true);
});

test("a contiguous server snapshot repairs a faster but gapped client map", () => {
  const pending = Array.from({ length: SCAN_CELL_COUNT }, (_, index) => scanCell(index, "full", {
    status: "pending", maxMs: null, avgMs: null, totalMs: null, reads: 0, bytesRead: 0
  }));
  const gappedMap = pending.map((cell, index) => (index < 300 || (index >= 500 && index < 950)) ? scanCell(index) : cell);
  const serverMap = pending.map((cell, index) => index < 400 ? scanCell(index) : cell);
  const current = {
    ...completedScan({ map: gappedMap, status: "running" }),
    id: "gapped-fast-scan",
    progress: 46.4
  };
  const incoming = {
    ...completedScan({ map: serverMap, status: "running" }),
    id: current.id,
    progress: 19.5
  };

  assert.equal(current.result.stats.testedCells > incoming.result.stats.testedCells, true);
  assert.equal(terminalScanPrefix(current.result.map), 300);
  const [repaired] = mergeJobLists([current], [incoming]);
  assert.equal(terminalScanPrefix(repaired.result.map), 400);
  assert.equal(repaired.result, incoming.result);
  assert.equal(repaired.progress, current.progress);
});

test("incremental scan updates cannot create a gap ahead of the completed prefix", () => {
  const pending = Array.from({ length: SCAN_CELL_COUNT }, (_, index) => scanCell(index, "full", {
    status: "pending", maxMs: null, avgMs: null, totalMs: null, reads: 0, bytesRead: 0
  }));
  const map = pending.map((cell, index) => index < 300 ? scanCell(index) : cell);
  const job = { ...completedScan({ map, status: "running" }), id: "gap-guard-job", progress: 14.6 };
  const cells = Array.from({ length: 50 }, (_, offset) => scanCell(500 + offset));
  const merged = mergeScanProgress(job, {
    id: job.id,
    status: "running",
    progress: 26.9,
    phase: "快速抽样 550/2048",
    message: "",
    cells,
    next: 550,
    stats: scanStats([...map.slice(0, 300), ...cells])
  });
  assert.equal(merged, job);
});

test("a completed scan requires a supported number of terminal cells spanning the disk", () => {
  const valid = completedScan();
  assert.equal(isCompleteScanResult(valid), true);
  assert.equal(isCompleteScanResult({ ...valid, status: "running" }), false);
  assert.equal(isCompleteScanResult({ ...valid, type: "speed" }), false);
  assert.equal(isCompleteScanResult({ ...valid, disk: { size: 0 } }), false);
  assert.equal(isCompleteScanResult({ ...valid, disk: { size: Number.MAX_SAFE_INTEGER + 1 } }), false);

  const shortMap = valid.result.map.slice(0, -1);
  assert.equal(isCompleteScanResult(completedScan({ map: shortMap, diskSize: shortMap.at(-1).end })), false);
  assert.equal(isCompleteScanResult({ ...valid, disk: { size: valid.disk.size + 1 } }), false);
  const pendingMap = valid.result.map.map((cell, index) => index === 20 ? { ...cell, status: "pending", reads: 0, bytesRead: 0 } : cell);
  assert.equal(isCompleteScanResult(completedScan({ map: pendingMap })), false);
});

test("completed scan coverage must agree with every declared scan mode", () => {
  const valid = completedScan({ coverage: "sampled" });
  assert.equal(isCompleteScanResult(valid), true);
  assert.equal(isCompleteScanResult({ ...valid, options: { scanMode: "full" } }), false);
  assert.equal(isCompleteScanResult({ ...valid, options: { scanMode: "quick" }, result: { ...valid.result, scanMode: "full" } }), false);
  assert.equal(isCompleteScanResult({ ...valid, result: { ...valid.result, coverage: "partial" } }), false);
  assert.equal(isCompleteScanResult({ ...valid, options: { scanMode: "turbo" } }), false);
});

test("new 4096-region quick scans and legacy 2048-region history are both valid", () => {
  const legacy = completedScan({ coverage: "sampled" });
  const cells = Array.from({ length: 4096 }, (_, index) => scanCell(index, "sampled"));
  const expanded = completedScan({
    coverage: "sampled", map: cells, diskSize: 4096 * SCAN_CELL_SIZE,
    result: { sampleBytes: 1024 * 1024 }
  });
  assert.equal(isCompleteScanResult(legacy), true);
  assert.equal(isCompleteScanResult(expanded), true);
  assert.equal(isCompleteScanResult(completedScan({
    coverage: "sampled", map: cells.slice(0, -1), diskSize: (cells.length - 1) * SCAN_CELL_SIZE
  })), false);
  assert.equal(isCompleteScanResult({ ...expanded, result: { ...expanded.result, sampleBytes: null } }), false);
  assert.equal(isCompleteScanResult({ ...expanded, result: { ...expanded.result, coverage: "full" } }), false);
});

test("full scans and deep tests accept 4096 regions and retain 2048-region history", () => {
  for (const type of ["scan", "write"]) {
    for (const count of [2048, 4096]) {
      const map = Array.from({ length: count }, (_, index) => scanCell(index));
      const mode = type === "write" ? "write" : "full";
      const job = completedScan({
        type, map, diskSize: count * SCAN_CELL_SIZE,
        options: type === "write" ? {} : { scanMode: mode },
        result: { scanMode: mode }
      });
      assert.equal(isCompleteScanResult(job), true, `${type}: ${count} regions`);
      const truncated = map.slice(0, -1);
      assert.equal(isCompleteScanResult({
        ...job, disk: { size: truncated.at(-1).end },
        result: { ...job.result, map: truncated, stats: scanStats(truncated) }
      }), false, `${type}: incomplete region count`);
    }
  }
});

test("normalized scan results retain all four categories through progress and completion", () => {
  const states = ["normal", "slow", "severe", "error"];
  const map = Array.from({ length: 4096 }, (_, index) => scanCell(index, "full", {
    status: states[index % 4], maxUnitMs: [10, 100, 700, 1][index % 4],
    errorCount: index % 4 === 3 ? 1 : 0
  }));
  const job = { ...completedScan({ map, diskSize: 4096 * SCAN_CELL_SIZE, result: { latencyBasis: "per-mib-v1" } }), id: "normalized-scan" };
  assert.equal(isCompleteScanResult(job), true);
  assert.equal(job.result.stats.slowCells, 2048);
  assert.equal(terminalScanPrefix(map), 4096);
  const pending = map.map((cell) => ({ ...cell, status: "pending" }));
  const merged = mergeScanProgress({ ...job, status: "running", result: { ...job.result, map: pending } }, {
    id: job.id, status: "completed", progress: 100, phase: "扫描完成", message: "读取结果", cells: map, next: 4096, stats: job.result.stats
  });
  assert.equal(isCompleteScanResult(merged), true);
  const invalidMap = map.map((cell, index) => index === 0 ? { ...cell, maxUnitMs: -1 } : cell);
  assert.deepEqual(safeScanMap(invalidMap), []);
});

test("completed scan statistics must exactly match the sanitized map", () => {
  const valid = completedScan();
  for (const [field, value] of [
    ["testedCells", SCAN_CELL_COUNT - 1],
    ["errorCells", 1],
    ["slowCells", 1],
    ["bytesRead", valid.result.stats.bytesRead - 1]
  ]) {
    assert.equal(isCompleteScanResult({
      ...valid,
      result: { ...valid.result, stats: { ...valid.result.stats, [field]: value } }
    }), false, field);
  }
  assert.equal(isCompleteScanResult({
    ...valid,
    result: { ...valid.result, stats: { ...valid.result.stats, testedCells: Number.NaN } }
  }), false);
});

test("full scan byte accounting distinguishes unread error regions from clean truncation", () => {
  const valid = completedScan();
  const truncated = valid.result.map.map((cell, index) => index === 10 ? { ...cell, bytesRead: cell.bytesRead - 1 } : cell);
  assert.equal(isCompleteScanResult(completedScan({ map: truncated })), false);

  const withError = valid.result.map.map((cell, index) => index === 10
    ? { ...cell, status: "error", errorCount: 1, bytesRead: 0 }
    : cell);
  assert.equal(isCompleteScanResult(completedScan({
    map: withError,
    result: { stats: scanStats(withError, { badRanges: [{ offset: 10 * SCAN_CELL_SIZE, length: 512, code: "EIO" }] }) }
  })), true);
  assert.equal(isCompleteScanResult(completedScan({
    map: withError,
    result: { stats: scanStats(withError, { badRanges: [{ offset: SCAN_DISK_SIZE, length: 512 }] }) }
  })), false);
});

test("sampled scan completion requires bounded sample ranges and a declared sample size", () => {
  const valid = completedScan({ coverage: "sampled" });
  assert.equal(isCompleteScanResult(valid), true);
  assert.equal(isCompleteScanResult({ ...valid, result: { ...valid.result, sampleBytes: null } }), false);

  const missingRange = valid.result.map.map((cell, index) => index === 0 ? { ...cell, sampleStart: null, sampleEnd: null } : cell);
  assert.equal(isCompleteScanResult(completedScan({ coverage: "sampled", map: missingRange })), false);
  const oversizedRange = valid.result.map.map((cell, index) => index === 0
    ? { ...cell, sampleStart: cell.start, sampleEnd: cell.end, bytesRead: cell.end - cell.start }
    : cell);
  assert.equal(isCompleteScanResult(completedScan({ coverage: "sampled", map: oversizedRange })), false);
});

const SPEED_SAMPLE_BYTES = 4 * 1024 * 1024;

test("scan outcome counters preserve recovery and reject incomplete address accounting", () => {
  const job = { type: "scan", disk: { size: 8192 }, result: { version: 2, stats: {
    coveredBytes: 4096, issueCount: 3, savedIssueCount: 3, recoveredRanges: 2,
    unreadableRanges: 0, unreadableBytes: 0, deviceErrorRanges: 1, issuesComplete: true
  } } };
  const summary = scanOutcomeSummary(job);
  assert.equal(summary.unreadableBytes, 0);
  assert.equal(summary.recoveredRanges, 2);
  assert.equal(summary.deviceErrorRanges, 1);
  assert.equal(scanOutcomeSummary({ ...job, result: { ...job.result, version: undefined } }), null);
  job.result.stats.savedIssueCount = 2;
  assert.equal(scanOutcomeSummary(job), null);
  job.result.stats.issuesComplete = false;
  assert.equal(scanOutcomeSummary(job).savedIssueCount, 2);
  job.result.stats.coveredBytes = 8193;
  assert.equal(scanOutcomeSummary(job), null);
});

test("new completed scans require full planned coverage and complete record persistence", () => {
  const map = Array.from({ length: SCAN_CELL_COUNT }, (_, index) => scanCell(index, "full", { coveredBytes: SCAN_CELL_SIZE, readOutcome: "readable" }));
  const job = completedScan({ map, result: { version: 2 } });
  job.result.stats.coveredBytes = SCAN_DISK_SIZE;
  job.result.stats.issuesComplete = true;
  assert.equal(isCompleteScanResult(job), true);
  map[0].coveredBytes -= 512;
  assert.equal(isCompleteScanResult(job), false);
  map[0].coveredBytes += 512;
  job.result.stats.issuesComplete = false;
  assert.equal(isCompleteScanResult(job), false);
});

test("issue pages preserve uncertain legacy evidence and validate exact page boundaries", () => {
  const page = { offset: 200, next: 201, total: 405, complete: true, legacy: false,
    records: [{ offset: 2048, length: 512, code: "EIO", outcome: "unreadable" }] };
  assert.equal(validScanIssuePage(page, 4096, 200), true);
  assert.equal(validScanIssuePage({ ...page, next: 202 }, 4096, 200), false);
  assert.equal(validScanIssuePage(page, 2500, 200), false);
  assert.equal(validScanIssuePage({ ...page, records: [{ ...page.records[0], outcome: "damaged" }] }, 4096, 200), false);
  assert.equal(scanRangeOutcomeLabel({ code: "EIO" }), "历史异常");
  assert.equal(scanRangeOutcomeLabel({ code: "EIO", outcome: "recovered" }), "复读已恢复");
  assert.equal(scanRangeOutcomeLabel({ code: "TIMEOUT", outcome: "device_error", recovered: true }), "设备异常已恢复");
});

test("slow repair requires an identified offline writable disk", () => {
  const disk = { path: "/dev/sda", serial: "TEST", logicalSectorSize:512, fileSystems: ["ext4"], mountpoints: [], readOnly: false, isSystem: false };
  assert.equal(scanRepairDiskBlockedReason(disk), "");
  for (const patch of [{serial:""},{logicalSectorSize:0},{isSystem:true},{readOnly:true},{mountpoints:["/vol1"]},{fileSystems:["linux_raid_member"]},{fileSystems:["zfs_member"]},{fileSystems:["LVM2_member"]},{fileSystems:["swap"]}]) {
    assert.notEqual(scanRepairDiskBlockedReason({...disk,...patch}), "");
  }
});

test("repair pages validate write results separately from unreadable ranges", () => {
  const range = {offset:512,length:512,code:"SLOW_REWRITE_VERIFIED",outcome:"rewrite",repair:{outcome:"verified",beforeMs:501,afterMs:12,writtenBytes:512}};
  const page = {offset:0,next:1,total:1,complete:true,legacy:false,records:[range]};
  assert.equal(validScanIssuePage(page,4096,0),true);
  assert.equal(scanRangeOutcomeLabel(range),"写回校验通过");
  const uncertain = {...range,repair:{...range.repair,outcome:"uncertain",afterMs:undefined,writtenBytes:0}};
  assert.equal(validScanIssuePage({...page,records:[uncertain]},4096,0),true);
  assert.equal(scanRangeOutcomeLabel(uncertain),"写回中断，结果未确定");
  for (const patch of [{outcome:"repaired"},{writtenBytes:513},{beforeMs:-1},{afterMs:-1}]) {
    assert.equal(validScanIssuePage({...page,records:[{...range,repair:{...range.repair,...patch}}]},4096,0),false);
  }
});
const SPEED_DISK_SIZE = 32 * 1024 * 1024;

function speedSamples() {
  return [
    speedSample("头部", 0, 200, SPEED_SAMPLE_BYTES),
    speedSample("中部", 12 * 1024 * 1024, 180, SPEED_SAMPLE_BYTES),
    speedSample("尾部", 28 * 1024 * 1024, 160, SPEED_SAMPLE_BYTES)
  ];
}

function benchmarkResult(samples, overrides = {}) {
  const totalBytes = samples.reduce((sum, sample) => sum + sample.bytes, 0);
  const totalSeconds = samples.reduce((sum, sample) => sum + sample.seconds, 0);
  const speeds = samples.map((sample) => sample.mbps);
  return {
    samples,
    averageMBps: Number((totalBytes / totalSeconds / 1e6).toFixed(1)),
    minimumMBps: Math.min(...speeds),
    maximumMBps: Math.max(...speeds),
    operation: "read",
    pattern: "sequential",
    blockSize: 1024 * 1024,
    ...overrides
  };
}

function randomBenchmark(overrides = {}) {
  const bytes = 256 * 1024 * 1024;
  const seconds = 6.1;
  const mbps = Number((bytes / seconds / 1e6).toFixed(1));
  return benchmarkResult([speedSample("随机", 0, mbps, bytes)], {
    operation: "read",
    pattern: "random",
    blockSize: 4096,
    iops: Number((bytes / 4096 / seconds).toFixed(1)),
    ...overrides
  });
}

function completedSpeed({ benchmarks, diskSize = 512 * 1024 * 1024, status = "completed", type = "speed", result = {} } = {}) {
  return {
    type,
    status,
    disk: { size: diskSize },
    options: {},
    result: { sampleMiB: 256, benchmarks: benchmarks || [benchmarkResult(speedSamples()), randomBenchmark()], ...result }
  };
}

test("completed speed results require a valid benchmark set", () => {
  assert.equal(isCompleteSpeedResult(completedSpeed()), true);
  assert.equal(isCompleteSpeedResult(completedSpeed({ benchmarks: [randomBenchmark({ operation: "write" })] })), true);
  assert.equal(isCompleteSpeedResult(completedSpeed({ benchmarks: [randomBenchmark({ operation: "mixed", queueDepth: 32, threads: 1 })] })), true);
  assert.equal(isCompleteSpeedResult(completedSpeed({ benchmarks: [null] })), false);
  assert.equal(isCompleteSpeedResult(completedSpeed({ benchmarks: [] })), false);
  assert.equal(isCompleteSpeedResult({ ...completedSpeed(), result: { sampleMiB: 256 } }), false);
});

test("benchmark queue depth and thread metadata accept configured fio profiles", () => {
  assert.equal(isCompleteSpeedResult(completedSpeed({ benchmarks: [benchmarkResult(speedSamples(), { queueDepth: 8, threads: 1 })] })), true);
  assert.equal(isCompleteSpeedResult(completedSpeed({ benchmarks: [randomBenchmark({ queueDepth: 32, threads: 1 })] })), true);
  assert.equal(isCompleteSpeedResult(completedSpeed({ benchmarks: [benchmarkResult(speedSamples(), { blockSize: 65536, queueDepth: 64, threads: 2 })] })), true);
  assert.equal(isCompleteSpeedResult(completedSpeed({ benchmarks: [randomBenchmark({ queueDepth: 128, threads: 8 })] })), true);
  assert.equal(isCompleteSpeedResult(completedSpeed({ benchmarks: [randomBenchmark({ queueDepth: 7, threads: 1 })] })), false);
  assert.equal(isCompleteSpeedResult(completedSpeed({ benchmarks: [randomBenchmark({ queueDepth: 32, threads: 3 })] })), false);
});

test("sequential benchmarks accept a positive optional iops value for new results", () => {
  const benchmark = benchmarkResult(speedSamples());
  const totalBytes = benchmark.samples.reduce((sum, sample) => sum + sample.bytes, 0);
  const totalSeconds = benchmark.samples.reduce((sum, sample) => sum + sample.seconds, 0);
  const iops = Number((totalBytes / benchmark.blockSize / totalSeconds).toFixed(1));
  assert.equal(isCompleteSpeedResult(completedSpeed({ benchmarks: [{ ...benchmark, iops }] })), true);
  assert.equal(isCompleteSpeedResult(completedSpeed({ benchmarks: [{ ...benchmark, iops: 0 }] })), false);
});

test("random benchmarks require iops and a single random sample", () => {
  assert.equal(isCompleteSpeedResult(completedSpeed({ benchmarks: [randomBenchmark({ iops: null })] })), false);
  assert.equal(isCompleteSpeedResult(completedSpeed({ benchmarks: [randomBenchmark({ blockSize: 1048576 })] })), false);
  assert.equal(isCompleteSpeedResult(completedSpeed({ benchmarks: [randomBenchmark({ pattern: "sequential" })] })), false);
});

test("benchmark aggregate values must exactly match the samples", () => {
  const seq = benchmarkResult(speedSamples());
  assert.equal(isCompleteSpeedResult(completedSpeed({ benchmarks: [{ ...seq, averageMBps: seq.averageMBps + 0.1 }] })), false);
  assert.equal(isCompleteSpeedResult(completedSpeed({ benchmarks: [{ ...seq, minimumMBps: seq.minimumMBps - 0.1 }] })), false);
});

test("out-of-disk and overlapping samples are rejected", () => {
  const beyondDisk = speedSamples().map((sample, index) => index === 2 ? { ...sample, offsetBytes: SPEED_DISK_SIZE } : sample);
  assert.equal(isCompleteSpeedResult(completedSpeed({ benchmarks: [benchmarkResult(beyondDisk)], diskSize: SPEED_DISK_SIZE })), false);

  const overlapping = speedSamples().map((sample, index) => index === 1 ? { ...sample, offsetBytes: SPEED_SAMPLE_BYTES - 1 } : sample);
  assert.equal(isCompleteSpeedResult(completedSpeed({ benchmarks: [benchmarkResult(overlapping)] })), false);
});

test("only completed speed jobs with a positive safe disk capacity are trusted", () => {
  const valid = completedSpeed();
  assert.equal(isCompleteSpeedResult({ ...valid, status: "running" }), false);
  assert.equal(isCompleteSpeedResult({ ...valid, type: "scan" }), false);
  assert.equal(isCompleteSpeedResult({ ...valid, disk: { size: 0 } }), false);
  assert.equal(isCompleteSpeedResult({ ...valid, disk: { size: Number.MAX_SAFE_INTEGER + 1 } }), false);
});
