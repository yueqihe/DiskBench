"use strict";

const fs = require("fs");
const path = require("path");
const { Readable } = require("stream");
const { pipeline } = require("stream/promises");

const root = path.resolve(__dirname, "..");
const dir = path.join(root, ".tools", "fnpack");
const platform = process.platform === "win32" ? "windows" : (process.platform === "darwin" ? "darwin" : "linux");
const arch = platform === "windows" || process.arch !== "arm64" ? "amd64" : (platform === "linux" ? "arm" : "arm64");
const version = "1.3.3";
const name = `fnpack-${version}-${platform}-${arch}`;
const url = `https://static2.fnnas.com/fnpack/${name}`;
const dest = path.join(dir, process.platform === "win32" ? "fnpack.exe" : "fnpack");

(async () => {
  fs.mkdirSync(dir, { recursive: true });
  const res = await fetch(url);
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  await pipeline(Readable.fromWeb(res.body), fs.createWriteStream(dest));
  if (process.platform !== "win32") fs.chmodSync(dest, 0o755);
  console.log(`Downloaded ${name} (${fs.statSync(dest).size} bytes) -> ${dest}`);
})().catch((error) => {
  console.error(`Download failed: ${error.message}`);
  process.exit(1);
});
