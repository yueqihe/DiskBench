"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
let jobDiskModel, speedBenchmarkCells, isCompleteSpeedResult, mergeJobLists, CRYSTAL_SPEED_PRESET;
test.before(async () => ({ jobDiskModel, speedBenchmarkCells, isCompleteSpeedResult, mergeJobLists, CRYSTAL_SPEED_PRESET } = await import("../../frontend/src/job-model.js")));
const space = { path: "/vol2", source: "/dev/md0", fileSystem: "btrfs", layout: "raid1", members: ["/dev/sda", "/dev/sdb"], size: 8*1024**3 };
const disks = [{ path: "/dev/sda", model: "Example HDD" }, { path: "/dev/sdb", model: "Example HDD" }];

test("history uses saved models, groups identical members and labels current-only legacy fallback", () => {
  const job = { storageSpace: { ...space, memberDisks: [{ model: "Original HDD" }, { model: "Original HDD" }] }, disk: { model: "BTRFS" } };
  assert.equal(jobDiskModel(job,disks,[space]).text, "Original HDD ×2");
  delete job.storageSpace.memberDisks;
  assert.equal(jobDiskModel(job,disks,[space]).text,"当前：Example HDD ×2");
  assert.equal(jobDiskModel(job,disks,[space]).current,true);
  for (const changed of [{ ...space, source: "/dev/md1" }, { ...space, members:["/dev/sdc"] }, { ...space, fileSystem:"ext4" }]) {
    assert.equal(jobDiskModel(job,disks,[changed]).text,"型号未记录");
  }
  assert.equal(jobDiskModel(job,disks.slice(0,1),[space]).text,"型号未记录");
  assert.equal(jobDiskModel({disk:{model:"Raw disk"}}).text,"Raw disk");
});

test("live measurements belong only to the executing cell and never become completed results", () => {
  const rows = [{ pattern:"random",blockSize:4096,queueDepth:1,threads:1,profileRow:"rnd-low" }];
  const job = {type:"speed",status:"running",options:{speedProfile:true},phase:"随机读取（4 KiB Q1T1）",result:{benchmarks:[]},speedLive:{stage:"running",benchmarkIndex:0,mbps:0,elapsedSeconds:4,durationSeconds:10,run:2,runs:3}};
  let cells = speedBenchmarkCells(job,rows);
  assert.equal(cells[0].live.mbps,0);
  assert.equal(cells[0].benchmark,null);
  for (const phase of ["准备测速临时文件","正在执行测速前 TRIM"]) {
    assert.equal(speedBenchmarkCells({...job,phase},rows).some(cell=>cell.live),false);
  }
  assert.equal(speedBenchmarkCells({...job,status:"cancelled"},rows).some(cell=>cell.live),false);
  job.speedLive.benchmarkIndex=1;
  assert.equal(speedBenchmarkCells(job,rows).some(cell=>cell.live),false);
});

test("timed results validate access ranges separately from transferred bytes and retain legacy validation", () => {
  const sample = {label:"随机",offsetBytes:0,rangeBytes:1024**2,bytes:20*1024**2,seconds:10,mbps:2.1};
  const benchmark = {operation:"read",pattern:"random",blockSize:4096,queueDepth:1,threads:1,averageMBps:2.1,minimumMBps:2.1,maximumMBps:2.1,iops:512,samples:[sample]};
  const job = {type:"speed",status:"completed",disk:{size:4*1024**2},result:{sampleMiB:1,runs:1,benchmarks:[benchmark]}};
  assert.equal(isCompleteSpeedResult(job),true);
  sample.rangeBytes=8*1024**2;
  assert.equal(isCompleteSpeedResult(job),false);
  sample.rangeBytes=-1;
  assert.equal(isCompleteSpeedResult(job),false);
  delete sample.rangeBytes;
  assert.equal(isCompleteSpeedResult(job),false);
  job.disk.size=32*1024**2;
  assert.equal(isCompleteSpeedResult(job),true);
});

test("timed read/write records retain empty mixed cells and clear the active cell during intervals", () => {
  const rows = [
    { pattern: "sequential", blockSize: 1048576, queueDepth: 8, threads: 1, profileRow: "seq-high" },
    { pattern: "random", blockSize: 4096, queueDepth: 1, threads: 1, profileRow: "rnd-low" }
  ];
  const job = {
    type: "speed", status: "running", options: { speedPreset: "crystal-default" },
    result: { benchmarks: [{ ...rows[0], operation: "read", averageMBps: 128 }] },
    speedLive: { stage: "running", benchmarkIndex: 1, run: 0, runs: 3, elapsedSeconds: 2, durationSeconds: 5, mbps: 20 }
  };
  const cells = speedBenchmarkCells(job, rows);
  assert.equal(cells.find(cell => cell.active), cells[3]);
  assert.equal(cells[3].operation, "read");
  assert.ok(cells.filter(cell => cell.operation === "mixed").every(cell => cell.skipped && !cell.benchmark));
  job.speedLive.stage = "interval";
  assert.ok(!speedBenchmarkCells(job, rows).some(cell => cell.active));
  job.speedLive = { stage: "running", benchmarkIndex: 2, run: 1, runs: 3, durationSeconds: 5 };
  assert.equal(speedBenchmarkCells(job, rows).find(cell => cell.active).operation, "write");
  job.speedLive.benchmarkIndex = 4;
  assert.ok(!speedBenchmarkCells(job, rows).some(cell => cell.active));
  job.options.speedPreset = CRYSTAL_SPEED_PRESET;
  assert.equal(speedBenchmarkCells(job, rows).find(cell => cell.active).operation, "mixed");
});

test("the completion summary replaces a live rate before the full result arrives", () => {
  const row = { pattern: "random", blockSize: 4096, queueDepth: 1, threads: 1, profileRow: "rnd-low" };
  const job = {
    id: "timed-speed", type: "speed", status: "running", options: { speedPreset: CRYSTAL_SPEED_PRESET, speedOperation: "read" },
    result: { benchmarks: [] }, speedLive: { stage: "finishing", benchmarkIndex: 0, mbps: 82.8 }
  };
  assert.equal(speedBenchmarkCells(job, [row])[0].live.mbps, 82.8);
  const benchmark = { ...row, operation: "read", averageMBps: 81.2 };
  const [completed] = mergeJobLists([job], [{ id: job.id, type: "speed", status: "completed", speedLive: null, speedSummary: { benchmarks: [benchmark] } }]);
  assert.equal(completed.result, undefined);
  const [cell] = speedBenchmarkCells(completed, [row]);
  assert.equal(cell.benchmark.averageMBps, 81.2);
  assert.equal(cell.active, false);
  const [full] = speedBenchmarkCells({ ...completed, result: { benchmarks: [{ ...benchmark, iops: 19824 }] } }, [row]);
  assert.equal(full.benchmark.iops, 19824);
});
