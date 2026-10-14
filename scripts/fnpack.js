"use strict";

const { spawnSync } = require("child_process");
const fs = require("fs");
const path = require("path");

const root = path.resolve(__dirname, "..");
const toolsDir = path.join(root, ".tools");

// findFnpack locates the packer: FNPACK env, the bundled .tools copy, then PATH.
function findFnpack() {
  if (process.env.FNPACK && fs.existsSync(process.env.FNPACK)) return process.env.FNPACK;
  const bundled = path.join(toolsDir, "fnpack", process.platform === "win32" ? "fnpack.exe" : "fnpack");
  if (fs.existsSync(bundled)) return bundled;
  const probe = spawnSync("fnpack", ["--help"], { stdio: "ignore" });
  if (probe.status === 0) return "fnpack";
  return null;
}

const fnpack = findFnpack();
if (!fnpack) {
  console.error("fnpack not found. Run `node scripts/download-fnpack.js` or install it from");
  console.error("https://developer.fnnas.com/docs/cli/fnpack/ (or set the FNPACK environment variable).");
  process.exit(1);
}

const result = spawnSync(fnpack, process.argv.slice(2), { cwd: root, stdio: "inherit" });
process.exit(result.status == null ? 1 : result.status);
