"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");

let createSmartReportCache;
test.before(async () => {
  ({ createSmartReportCache } = await import("../../frontend/src/smart-report-cache.js"));
});

const disk = { path: "/dev/sdb", serial: "DISK-A", model: "Test SSD", size: 1000000 };
const report = (target, capturedAt = "first") => ({ device: target.path, output: target.serial, capturedAt });
function deferred() {
  let resolve;
  const promise = new Promise((done) => { resolve = done; });
  return { promise, resolve };
}

test("SMART prefetch survives dialog remounts and inventory updates without another read", async () => {
  let reads = 0;
  const cache = createSmartReportCache(async (target) => { reads++; return report(target); });
  const unsubscribe = cache.subscribe(() => {});
  await cache.load(disk);
  const first = cache.get(disk);
  unsubscribe();
  await cache.load({ ...disk, temperatureC: 40 });
  await cache.load(disk);
  assert.equal(cache.get(disk), first);
  assert.equal(first.report.capturedAt, "first");
  assert.equal(reads, 1);
  await cache.load(disk, true);
  assert.equal(reads, 2, "only an explicit refresh should perform another read");
});

test("opening or refreshing SMART while prefetch is pending shares the existing read", async () => {
  const response = deferred();
  let reads = 0;
  const cache = createSmartReportCache(() => { reads++; return response.promise; });
  const first = cache.load(disk);
  assert.equal(cache.get(disk).loading, true);
  assert.equal(cache.load({ ...disk }), first);
  assert.equal(cache.load(disk, true), first);
  response.resolve(report(disk));
  await first;
  assert.equal(cache.get(disk).loading, false);
  assert.equal(reads, 1);
});

test("switching disks while reads are pending keeps their reports separate", async () => {
  const other = { ...disk, path: "/dev/sdc", serial: "DISK-B" };
  const firstResponse = deferred();
  const secondResponse = deferred();
  let reads = 0;
  const cache = createSmartReportCache((target) => {
    reads++;
    return target.serial === disk.serial ? firstResponse.promise : secondResponse.promise;
  });
  const first = cache.load(disk);
  const second = cache.load(other);
  secondResponse.resolve(report(other));
  await second;
  assert.equal(cache.get(other).report.output, "DISK-B");
  assert.equal(cache.get(disk).loading, true);
  firstResponse.resolve(report(disk));
  await first;
  await cache.load(disk);
  assert.equal(cache.get(disk).report.output, "DISK-A");
  assert.equal(cache.get(other).report.output, "DISK-B");
  assert.equal(reads, 2, "returning to a previously selected disk must reuse its report");
});

test("a replacement disk at the same path does not inherit the previous SMART report", async () => {
  let reads = 0;
  const cache = createSmartReportCache(async (target) => { reads++; return report(target); });
  await cache.load(disk);
  for (const replacement of [
    { ...disk, serial: "NEW-DISK" },
    { ...disk, model: "Another SSD" },
    { ...disk, size: disk.size * 2 }
  ]) {
    assert.equal(cache.get(replacement).report, null);
    await cache.load(replacement);
  }
  assert.equal(reads, 4);
  assert.equal(cache.get(disk).report.output, disk.serial);
});

test("SMART failures remain cached until the user explicitly retries", async () => {
  let reads = 0;
  const failure = new Error("SMART timeout");
  const cache = createSmartReportCache(async (target) => {
    if (++reads === 1) throw failure;
    return report(target, "retry");
  });
  await cache.load(disk);
  await cache.load({ ...disk });
  assert.equal(reads, 1);
  assert.equal(cache.get(disk).error, failure);
  await cache.load(disk, true);
  assert.equal(reads, 2);
  assert.equal(cache.get(disk).error, null);
  assert.equal(cache.get(disk).report.capturedAt, "retry");
});

test("manual refresh keeps the cached report during loading and after a failed read", async () => {
  let reads = 0;
  const cache = createSmartReportCache(async (target) => {
    if (++reads === 2) throw new Error("Disk temporarily unavailable");
    return report(target, String(reads));
  });
  await cache.load(disk);
  const first = cache.get(disk).report;
  const refresh = cache.load(disk, true);
  assert.equal(cache.get(disk).loading, true);
  assert.equal(cache.get(disk).report, first);
  await refresh;
  assert.equal(cache.get(disk).loading, false);
  assert.equal(cache.get(disk).report, first);
  assert.match(cache.get(disk).error.message, /temporarily unavailable/);
  await cache.load(disk, true);
  assert.equal(cache.get(disk).report.capturedAt, "3");
});
