"use strict";

const { spawnSync } = require("child_process");
const fs = require("fs");
const path = require("path");

const root = path.resolve(__dirname, "..");
const backendDir = path.join(root, "backend");
const toolsDir = path.join(root, ".tools");

function goExeName() {
  return process.platform === "win32" ? "go.exe" : "go";
}

// findGo locates the Go toolchain: GOROOT, the bundled .tools copy, then PATH.
function findGo() {
  if (process.env.GOROOT) {
    const candidate = path.join(process.env.GOROOT, "bin", goExeName());
    if (fs.existsSync(candidate)) return candidate;
  }
  const bundled = path.join(toolsDir, "go", "bin", goExeName());
  if (fs.existsSync(bundled)) return bundled;
  const probe = spawnSync("go", ["version"], { stdio: "ignore" });
  if (probe.status === 0) return "go";
  return null;
}

// goEnv builds an environment for go commands. The build cache is kept inside
// the project so read-only system directories never block a build.
function goEnv(env = process.env) {
  const next = { ...env };
  next.GOCACHE = env.GOCACHE || path.join(toolsDir, "go-cache");
  next.GOPATH = env.GOPATH || path.join(toolsDir, "gopath");
  fs.mkdirSync(next.GOCACHE, { recursive: true });
  return next;
}

// runGo executes a go subcommand in the backend module and returns its status.
function runGo(args, options = {}) {
  const go = findGo();
  if (!go) {
    console.error("Go toolchain not found. Install Go or set GOROOT.");
    return 1;
  }
  const result = spawnSync(go, args, {
    cwd: options.cwd || backendDir,
    env: options.env || goEnv(),
    stdio: options.stdio || "inherit",
  });
  return result.status == null ? 1 : result.status;
}

module.exports = { root, backendDir, toolsDir, findGo, goEnv, runGo };
