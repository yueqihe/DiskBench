"use strict";

const { spawn } = require("child_process");
const path = require("path");
const { backendDir } = require("./lib-go");

const exeName = process.platform === "win32" ? "diskbench.exe" : "diskbench";
const exe = path.join(backendDir, exeName);

const child = spawn(exe, [], { stdio: "inherit", env: process.env });
child.on("error", (error) => {
  console.error(`Failed to start ${exe}: ${error.message}`);
  process.exit(1);
});
child.on("exit", (code) => {
  process.exit(code == null ? 0 : code);
});
