"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");

let speedBenchmarkCells;
test.before(async () => {
  ({ speedBenchmarkCells } = await import("../../frontend/src/job-model.js"));
});

function rows(queueDepth = 64, threads = 1) {
  return [
    { key: "seq-high", profileRow: "seq-high", pattern: "sequential", blockSize: 1048576, queueDepth, threads },
    { key: "seq-low", profileRow: "seq-low", pattern: "sequential", blockSize: 1048576, queueDepth: 1, threads: 1 },
    { key: "rnd-high", profileRow: "rnd-high", pattern: "random", blockSize: 4096, queueDepth, threads },
    { key: "rnd-low", profileRow: "rnd-low", pattern: "random", blockSize: 4096, queueDepth: 1, threads: 1 }
  ];
}
function phase(cell, position = "头部") {
  const verb = { read: "读取", write: "写入", mixed: "混合读写" }[cell.operation];
  const parameters = `Q${cell.row.queueDepth}T${cell.row.threads}`;
  return cell.row.pattern === "random" ? `随机${verb}（4 KiB ${parameters}）` : `${position}位置顺序${verb} ${parameters}（1/3）`;
}
function result(cell, averageMBps = 42) {
  return { ...cell.row, operation: cell.operation, averageMBps };
}
function job(options = {}) {
  return { type: "speed", status: "running", phase: "准备测速临时文件", options: { speedProfile: true, ...options }, result: { benchmarks: [] } };
}

test("the active indicator follows every sequential/random read/write/mixed test, including multiple runs", () => {
  for (const matrixRows of [rows(), rows(128, 4), rows(1)]) {
    const current = job({ speedRuns: 3 });
    const plan = speedBenchmarkCells(current, matrixRows);
    for (let index = 0; index < plan.length; index++) {
      current.phase = phase(plan[index]);
      current.progress = index / plan.length * 100;
      for (const position of ["头部", "中部", "尾部"]) {
        current.phase = phase(plan[index], position);
        const cells = speedBenchmarkCells(current, matrixRows);
        assert.deepEqual(cells.flatMap((cell, i) => cell.active ? [i] : []), [index]);
        assert.equal(cells.filter((cell) => cell.benchmark).length, index);
      }
      current.result.benchmarks.push(result(plan[index], index + 1));
      // A completed item can briefly retain its old phase before the next starts.
      assert.equal(speedBenchmarkCells(current, matrixRows).some((cell) => cell.active), false);
    }
    assert.deepEqual(speedBenchmarkCells(current, matrixRows).map((cell) => cell.benchmark.averageMBps), Array.from({ length: 12 }, (_, i) => i + 1));
  }
});

test("single-row and operation filters only highlight tests that will actually execute", () => {
  for (const speedOperation of [undefined, "read", "write", "mixed"]) {
    const current = job({ speedProfileRow: "rnd-low", speedPattern: "random", speedOperation });
    const plan = speedBenchmarkCells(current, rows()).filter((cell) => !cell.skipped);
    assert.equal(plan.length, speedOperation ? 1 : 3);
    for (const expected of plan) {
      current.phase = phase(expected);
      const active = speedBenchmarkCells(current, rows()).filter((cell) => cell.active);
      assert.equal(active.length, 1);
      assert.equal(active[0].row.key, "rnd-low");
      assert.equal(active[0].operation, expected.operation);
      current.result.benchmarks.push(result(expected));
    }
  }
  const readOnly = job({ speedOperation: "read" });
  for (const row of rows()) {
    readOnly.phase = phase({ row, operation: "read" });
    assert.equal(speedBenchmarkCells(readOnly, rows()).filter((cell) => cell.active).length, 1);
    readOnly.result.benchmarks.push(result({ row, operation: "read" }));
  }
});

test("preparation, TRIM, stopped and terminal tasks have no running indicator", () => {
  const current = job();
  for (const stage of ["准备一键测速", "准备测速临时文件", "正在执行测速前 TRIM", "", "随机读取（4 KiB Q8T1）"]) {
    current.phase = stage;
    assert.equal(speedBenchmarkCells(current, rows()).some((cell) => cell.active), false);
  }
  current.phase = "头部位置顺序读取 Q64T1（1/3）";
  for (const status of ["idle", "queued", "cancelling", "cancelled", "interrupted", "failed", "completed"]) {
    current.status = status;
    assert.equal(speedBenchmarkCells(current, rows()).some((cell) => cell.active), false);
  }
});

test("legacy filtered results and 4K phase format still locate the correct cell", () => {
  const current = job({ speedProfile: false, speedPattern: "random", speedQueueDepth: 1, speedOperation: "read" });
  current.phase = "随机读取（4K Q1T1）";
  const cells = speedBenchmarkCells(current, rows(32));
  assert.deepEqual(cells.filter((cell) => cell.active).map((cell) => cell.row.key), ["rnd-low"]);
});
