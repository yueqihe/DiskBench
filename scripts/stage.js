"use strict";

const fs = require("fs");
const path = require("path");

const root = path.resolve(__dirname, "..");
const packServer = path.join(root, "package", "diskbench", "app", "server");
const frontendDist = path.join(root, "frontend", "dist");
const backendDir = path.join(root, "backend");
const runtimeDepsRoot = path.join(root, "runtime-deps");
const targetArch = process.env.TARGET_ARCH || "";

if (!fs.existsSync(path.join(frontendDist, "index.html"))) {
  throw new Error("frontend/dist 不存在，请先运行 npm run build:frontend");
}

// Prefer the Linux binary (produced by `npm run fpk` cross-compilation), but
// fall back to a Windows build for local staging.
const candidates = [
  path.join(backendDir, "diskbench"),
  path.join(backendDir, "diskbench.exe")
];
const binary = candidates.find((candidate) => fs.existsSync(candidate));
if (!binary) {
  throw new Error("backend/diskbench 不存在，请先运行 npm run build:go");
}

fs.rmSync(packServer, { recursive: true, force: true });
fs.mkdirSync(packServer, { recursive: true });
fs.copyFileSync(binary, path.join(packServer, "diskbench"));
fs.cpSync(frontendDist, path.join(packServer, "public"), { recursive: true });

const dependencySources = [
  targetArch ? path.join(runtimeDepsRoot, targetArch) : null,
  path.join(runtimeDepsRoot, "common")
].filter(Boolean);
const dependencySource = dependencySources.find((candidate) => fs.existsSync(candidate));
if (dependencySource) {
  fs.cpSync(dependencySource, packServer, { recursive: true, force: true });
  console.log(`Staged bundled runtime dependencies from ${path.relative(root, dependencySource)}`);
} else if (targetArch) {
  console.warn(`No bundled runtime dependencies found for ${targetArch}; using fnOS system tools.`);
}

console.log(`Staged FPK application files in ${packServer}`);
