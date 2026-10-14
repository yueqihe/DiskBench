"use strict";

const fs = require("fs");
const path = require("path");

const root = path.resolve(__dirname, "..");
const manifestPath = path.join(root, "package", "diskbench", "manifest");
const platform = process.argv[2] || process.env.PLATFORM || "x86";

let content = fs.readFileSync(manifestPath, "utf8");
if (!/^platform=/m.test(content)) {
  content += `\nplatform=${platform}\n`;
} else {
  content = content.replace(/^platform=.*$/m, `platform=${platform}`);
}
fs.writeFileSync(manifestPath, content);
console.log(`manifest platform=${platform}`);
