"use strict";

const path = require("path");
const { backendDir, runGo, goEnv } = require("./lib-go");

const goos = process.env.GOOS || (process.platform === "win32" ? "windows" : "linux");
const goarch = process.env.GOARCH || (process.arch === "arm64" ? "arm64" : "amd64");
const exeName = goos === "windows" ? "diskbench.exe" : "diskbench";
const outPath = path.join(backendDir, exeName);

const env = goEnv();
env.GOOS = goos;
env.GOARCH = goarch;
env.CGO_ENABLED = "0";

const status = runGo(["build", "-o", outPath, "."], { env });
if (status !== 0) process.exit(status);
console.log(`Built ${path.relative(backendDir, outPath)} (${goos}/${goarch})`);
