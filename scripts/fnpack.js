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

const args = process.argv.slice(2);
if (args[0] === "build") {
  const help = spawnSync(fnpack, ["build", "--help"], { encoding: "utf8" });
  if (help.status !== 0 || !help.stdout.includes("--nosign")) {
    console.error("This project requires a signing-capable fnpack. Run node scripts/download-fnpack.js first.");
    process.exit(1);
  }
  const { FNPACK_KEY: key, FNPACK_CERT: cert } = process.env;
  if (!args.includes("--nosign") && (key || cert)) {
    if (!key || !cert) {
      console.error("Set both FNPACK_KEY and FNPACK_CERT, or use the certificate already imported into fnpack.");
      process.exit(1);
    }
    args.push("--key", key, "--cert", cert);
  }
}

const result = spawnSync(fnpack, args, { cwd: root, encoding: "utf8", stdio: ["inherit", "pipe", "pipe"] });
if (result.stdout) process.stdout.write(result.stdout);
if (result.stderr) process.stderr.write(result.stderr);
if (result.error) console.error(result.error.message);
// fnpack can report a compression error with exit code zero.
const packingFailed = args[0] === "build" && /Packing failed\./i.test(`${result.stdout}\n${result.stderr}`);
process.exit(result.status == null || packingFailed ? 1 : result.status);
