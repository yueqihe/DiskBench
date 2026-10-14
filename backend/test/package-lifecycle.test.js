"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const { spawnSync } = require("node:child_process");

const root = path.resolve(__dirname, "../..");
const cmdSource = path.join(root, "package/diskbench/cmd");
const bash = process.env.DISKBENCH_TEST_BASH || (process.platform === "win32"
  ? path.join(process.env.ProgramFiles || "C:/Program Files", "Git/bin/bash.exe")
  : "bash");
const hasBash = spawnSync(bash, ["--version"], { encoding: "utf8" }).status === 0;
const lifecycleTest = (name, fn) => test(name, { skip: !hasBash && "Bash is required for lifecycle integration tests" }, fn);

function shellPath(filename) {
  return process.platform === "win32"
    ? filename.replace(/^([a-z]):/i, (_, drive) => `/${drive.toLowerCase()}`).replaceAll("\\", "/")
    : filename;
}

function fixture(t) {
  const tmpRoot = path.join(root, ".tools/tmp");
  fs.mkdirSync(tmpRoot, { recursive: true });
  const directory = fs.mkdtempSync(path.join(tmpRoot, "diskbench-lifecycle-"));
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
  const volume = path.join(directory, "volume");
  const data = path.join(volume, "@appdata/diskbench");
  const backup = path.join(volume, "@appdata/.diskbench-history");
  const app = path.join(volume, "@appcenter/diskbench");
  const cmd = path.join(directory, "cmd");
  const errorLog = path.join(directory, "error.txt");
  fs.mkdirSync(path.join(data, "jobs"), { recursive: true });
  fs.mkdirSync(path.join(app, "server"), { recursive: true });
  fs.cpSync(cmdSource, cmd, { recursive: true });
  const env = {
    ...process.env,
    TRIM_APPDEST_VOL: shellPath(volume),
    TRIM_PKGVAR: shellPath(data),
    TRIM_APPDEST: shellPath(app),
    TRIM_TEMP_LOGFILE: shellPath(errorLog),
  };
  delete env.wizard_history_action;
  return {
    directory, volume, data, backup, app, cmd, errorLog,
    run(script, options = {}) {
      return spawnSync(bash, ["-c", 'if [ -n "${DISKBENCH_TEST_COMMAND_PATH:-}" ]; then export PATH="$DISKBENCH_TEST_COMMAND_PATH:$PATH"; fi; exec bash "$1"', "lifecycle-test", shellPath(path.join(cmd, script))], {
        cwd: directory, env: { ...env, ...options }, encoding: "utf8", timeout: 15000,
      });
    },
    failCommand(command) {
      const bin = path.join(directory, "fail-bin");
      fs.mkdirSync(bin, { recursive: true });
      fs.writeFileSync(path.join(bin, command), "#!/bin/bash\nexit 1\n", { mode: 0o755 });
      return { DISKBENCH_TEST_COMMAND_PATH: shellPath(bin) };
    },
  };
}

function succeeds(result) {
  assert.equal(result.status, 0, result.error?.message || result.stderr || result.stdout);
}

function seedHistory(f) {
  const records = {
    "scan.json": '{"id":"scan","type":"scan","status":"completed","result":{"issueLog":"scan.scan-issues.jsonl"}}',
    "scan.scan-issues.jsonl": '{"offset":1024,"length":512,"code":"EIO"}\n',
    "speed.json": '{"id":"speed","type":"speed","status":"completed"}',
    "deep.json": '{"id":"deep","type":"write","status":"completed"}',
  };
  for (const [name, content] of Object.entries(records)) {
    fs.writeFileSync(path.join(f.data, "jobs", name), content);
  }
  return records;
}

lifecycleTest("fnOS retains var in place while removing app files, with no backup or restore", (t) => {
  const f = fixture(t);
  const records = seedHistory(f);
  fs.writeFileSync(path.join(f.data, "app.log"), "runtime log");
  const original = fs.statSync(path.join(f.data, "jobs/scan.json"));
  succeeds(f.run("uninstall_init"));
  // fnOS removes target/tmp/home/etc and leaves var in place.
  for (const area of ["@appconf", "@apptemp", "@apphome"]) {
    const directory = path.join(f.volume, area, "diskbench");
    fs.mkdirSync(directory, { recursive: true });
    fs.rmSync(directory, { recursive: true });
  }
  fs.rmSync(f.app, { recursive: true });
  succeeds(f.run("uninstall_callback"));
  assert.equal(fs.existsSync(f.backup), false);
  const retained = fs.statSync(path.join(f.data, "jobs/scan.json"));
  assert.equal(retained.ino, original.ino);
  assert.equal(retained.mtimeMs, original.mtimeMs);
  succeeds(f.run("install_callback"));
  for (const [name, content] of Object.entries(records)) {
    assert.equal(fs.readFileSync(path.join(f.data, "jobs", name), "utf8"), content);
  }
  assert.equal(fs.existsSync(f.backup), false);
  succeeds(f.run("install_callback"));
  // Reinstall never recreates a record the user previously deleted.
  fs.unlinkSync(path.join(f.data, "jobs/scan.json"));
  succeeds(f.run("install_callback"));
  assert.equal(fs.existsSync(path.join(f.data, "jobs/scan.json")), false);
});

lifecycleTest("delete clears both live and retained history while preserving other files", (t) => {
  const f = fixture(t);
  seedHistory(f);
  // A 0.1.10 duplicate may exist if the earlier package was used.
  fs.cpSync(path.join(f.data, "jobs"), path.join(f.backup, "jobs"), { recursive: true });
  const sentinel = path.join(f.volume, "@appdata/other-app/keep.txt");
  fs.mkdirSync(path.dirname(sentinel), { recursive: true });
  fs.writeFileSync(sentinel, "personal data");
  fs.writeFileSync(path.join(f.data, "keep.txt"), "unrelated file");
  succeeds(f.run("uninstall_init", { wizard_history_action: "delete" }));
  assert.equal(fs.existsSync(path.join(f.data, "jobs/scan.json")), true);
  fs.rmSync(f.app, { recursive: true });
  succeeds(f.run("uninstall_callback", { wizard_history_action: "delete" }));
  assert.equal(fs.existsSync(path.join(f.data, "jobs")), false);
  assert.equal(fs.existsSync(f.backup), false);
  assert.equal(fs.readFileSync(sentinel, "utf8"), "personal data");
  assert.equal(fs.readFileSync(path.join(f.data, "keep.txt"), "utf8"), "unrelated file");
  succeeds(f.run("install_callback"));
  assert.equal(fs.existsSync(path.join(f.data, "jobs/scan.json")), false);
  succeeds(f.run("uninstall_callback", { wizard_history_action: "delete" }));
});

lifecycleTest("reinstall uses original records without importing stale 0.1.10 duplicates", (t) => {
  const f = fixture(t);
  seedHistory(f);
  fs.cpSync(path.join(f.data, "jobs"), path.join(f.backup, "jobs"), { recursive: true });
  fs.writeFileSync(path.join(f.data, "jobs/scan.json"), "newer record");
  fs.unlinkSync(path.join(f.data, "jobs/scan.scan-issues.jsonl"));
  succeeds(f.run("install_callback"));
  assert.equal(fs.readFileSync(path.join(f.data, "jobs/scan.json"), "utf8"), "newer record");
  assert.equal(fs.existsSync(path.join(f.data, "jobs/scan.scan-issues.jsonl")), false);
});

lifecycleTest("uninstall stops the service and preserves interrupted state in its original file", (t) => {
  const f = fixture(t);
  fs.writeFileSync(path.join(f.data, "jobs/active.json"), '{"status":"running"}');
  fs.writeFileSync(path.join(f.cmd, "main"), '#!/bin/bash\ncase "$1" in\nstop) printf \'{"status":"interrupted"}\' > "$TRIM_PKGVAR/jobs/active.json" ;;\nstatus) exit 3 ;;\nesac\n');
  succeeds(f.run("uninstall_init"));
  assert.equal(fs.readFileSync(path.join(f.data, "jobs/active.json"), "utf8"), '{"status":"interrupted"}');
  assert.equal(fs.existsSync(f.backup), false);
});

lifecycleTest("uninstall aborts without modifying records if the service cannot stop", (t) => {
  const f = fixture(t);
  seedHistory(f);
  fs.writeFileSync(path.join(f.cmd, "main"), "#!/bin/bash\nexit 0\n");
  assert.notEqual(f.run("uninstall_init", { wizard_history_action: "delete" }).status, 0);
  assert.equal(fs.existsSync(path.join(f.data, "jobs/scan.json")), true);
  assert.equal(fs.existsSync(f.backup), false);
  assert.match(fs.readFileSync(f.errorLog, "utf8"), /尚未停止/);
});

lifecycleTest("keep and reinstall require no copy operation", (t) => {
  const f = fixture(t);
  const records = seedHistory(f);
  succeeds(f.run("uninstall_init", f.failCommand("cp")));
  succeeds(f.run("uninstall_callback", f.failCommand("cp")));
  succeeds(f.run("install_callback", f.failCommand("cp")));
  assert.equal(fs.readFileSync(path.join(f.data, "jobs/scan.json"), "utf8"), records["scan.json"]);
  assert.equal(fs.existsSync(f.backup), false);
});

lifecycleTest("failed deletion reports an error and can be retried", (t) => {
  const f = fixture(t);
  const records = seedHistory(f);
  succeeds(f.run("uninstall_init"));
  assert.notEqual(f.run("uninstall_callback", { ...f.failCommand("rm"), wizard_history_action: "delete" }).status, 0);
  assert.equal(fs.readFileSync(path.join(f.data, "jobs/scan.json"), "utf8"), records["scan.json"]);
  assert.match(fs.readFileSync(f.errorLog, "utf8"), /删除失败/);
  succeeds(f.run("uninstall_callback", { wizard_history_action: "delete" }));
  assert.equal(fs.existsSync(path.join(f.data, "jobs")), false);
});

lifecycleTest("invalid options and mismatched data paths never delete history", (t) => {
  const f = fixture(t);
  seedHistory(f);
  assert.notEqual(f.run("uninstall_init", { wizard_history_action: "unknown" }).status, 0);
  for (const options of [
    { wizard_history_action: "unknown" },
    { wizard_history_action: "delete", TRIM_PKGVAR: "" },
    { wizard_history_action: "delete", TRIM_PKGVAR: shellPath(f.volume) },
    { wizard_history_action: "delete", TRIM_PKGVAR: "/" },
    { wizard_history_action: "delete", TRIM_PKGVAR: "relative-path" },
  ]) {
    assert.notEqual(f.run("uninstall_callback", options).status, 0);
    assert.equal(fs.existsSync(path.join(f.data, "jobs/scan.json")), true);
  }
});

lifecycleTest("a history symlink cannot delete files outside the app data directory", (t) => {
  const f = fixture(t);
  const personal = path.join(f.directory, "personal-data");
  fs.mkdirSync(personal);
  fs.writeFileSync(path.join(personal, "keep.txt"), "personal data");
  fs.rmdirSync(path.join(f.data, "jobs"));
  fs.symlinkSync(personal, path.join(f.data, "jobs"), process.platform === "win32" ? "junction" : "dir");
  assert.notEqual(f.run("uninstall_callback", { wizard_history_action: "delete" }).status, 0);
  assert.equal(fs.readFileSync(path.join(personal, "keep.txt"), "utf8"), "personal data");
  assert.match(fs.readFileSync(f.errorLog, "utf8"), /目录无效/);
});

lifecycleTest("a stale PID belonging to another executable is never signalled", (t) => {
  const f = fixture(t);
  fs.writeFileSync(path.join(f.cmd, "stale-pid-test"), '#!/bin/bash\nprintf "%s" "$$" > "$TRIM_PKGVAR/app.pid"\nbash "$(dirname "$0")/main" stop || exit 1\nkill -0 "$$"\n');
  succeeds(f.run("stale-pid-test"));
  assert.equal(fs.existsSync(path.join(f.data, "app.pid")), false);
});

lifecycleTest("main identifies and stops its own executable", (t) => {
  const f = fixture(t);
  fs.writeFileSync(path.join(f.cmd, "main-stop-test"), `#!/bin/bash
cp "$(command -v sleep)" "$TRIM_APPDEST/server/diskbench" || exit 1
"$TRIM_APPDEST/server/diskbench" 60 &
service=$!
trap 'kill "$service" 2>/dev/null; wait "$service" 2>/dev/null' EXIT
printf '%s' "$service" > "$TRIM_PKGVAR/app.pid"
bash "$(dirname "$0")/main" status || exit 1
bash "$(dirname "$0")/main" stop || exit 1
if kill -0 "$service" 2>/dev/null; then exit 1; fi
exit 0
`);
  succeeds(f.run("main-stop-test"));
  assert.equal(fs.existsSync(path.join(f.data, "app.pid")), false);
});
