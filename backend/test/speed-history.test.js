"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");
let speedHistorySummary;
test.before(async () => {
  ({ speedHistorySummary } = await import("../../frontend/src/job-model.js"));
});
const benchmark = (operation, speed, pattern = "sequential", queueDepth = 64) => ({
  operation, pattern, averageMBps: speed, queueDepth, threads: 1, blockSize: pattern === "sequential" ? 1048576 : 4096
});

test("speed history defaults to the first sequential read/write tests, using both summaries and details", () => {
  const benchmarks = [benchmark("read", 1234.5), benchmark("write", 987.6),
    benchmark("read", 600, "sequential", 1), benchmark("write", 500, "sequential", 1), benchmark("read", 40, "random")];
  for (const data of [{ result: { benchmarks } }, { speedSummary: { benchmarks } }]) {
    const summary = speedHistorySummary({ type: "speed", ...data });
    assert.equal(summary.text, "读 1,234.5 MB/s · 写 987.6 MB/s");
    assert.match(summary.tooltip, /顺序读取 · 1 MiB Q64T1/);
    assert.match(summary.tooltip, /顺序写入 · 1 MiB Q64T1/);
    assert.doesNotMatch(summary.tooltip, /600 MB|随机/);
  }
});

test("partial, read-only, random and mixed histories keep unmeasured rates distinct", () => {
  const summary = (benchmarks) => speedHistorySummary({ type: "speed", result: { benchmarks } });
  assert.equal(summary([benchmark("read", 0)]).text, "读 0 MB/s · 写 —");
  assert.equal(summary([benchmark("write", 80)]).text, "读 — · 写 80 MB/s");
  assert.equal(summary([benchmark("read", 30, "random"), benchmark("write", 20, "random")]).text, "随机读 30 MB/s · 写 20 MB/s");
  assert.equal(summary([benchmark("mixed", 50)]).text, "混合 50 MB/s");
  assert.equal(summary([benchmark("mixed", 10, "random")]).text, "随机混合 10 MB/s");
});

test("invalid or absent measurements never turn into displayed speed numbers", () => {
  for (const invalid of [null, {}, { operation: "read", pattern: "sequential" }, benchmark("read", null), benchmark("read", -1), benchmark("read", Infinity)]) {
    assert.equal(speedHistorySummary({ type: "speed", speedSummary: { benchmarks: [invalid] } }), null);
  }
  assert.equal(speedHistorySummary({ type: "scan", result: { benchmarks: [benchmark("read", 100)] } }), null);
  assert.equal(speedHistorySummary({ type: "speed" }), null);
  assert.equal(speedHistorySummary({ type: "speed", result: { benchmarks: [benchmark("read", 200)] }, speedSummary: { benchmarks: [benchmark("read", 100)] } }).text, "读 200 MB/s · 写 —");
});
