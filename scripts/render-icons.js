"use strict";

const { spawnSync } = require("child_process");
const fs = require("fs");
const os = require("os");
const path = require("path");

const root = path.resolve(__dirname, "..");
const source = path.join(root, "assets", "icon-source.svg");
const packageDir = path.join(root, "package", "diskbench");
const outputDir = path.join(root, "package", "diskbench", "app", "ui", "images");
const frontendPublicDir = path.join(root, "frontend", "public");
const edge = process.platform === "win32"
  ? "C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe"
  : "microsoft-edge";

function waitForStableFile(filePath, timeoutMs = 10000) {
  const startedAt = Date.now();
  let previousSize = -1;
  let stableChecks = 0;
  while (Date.now() - startedAt < timeoutMs) {
    if (fs.existsSync(filePath)) {
      const size = fs.statSync(filePath).size;
      if (size > 0 && size === previousSize) stableChecks += 1;
      else stableChecks = 0;
      previousSize = size;
      if (stableChecks >= 5) return true;
    }
    Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 100);
  }
  return false;
}

function makeRoundedCornersTransparent(output, size) {
  if (process.platform !== "win32") return;
  const escaped = output.replace(/'/g, "''");
  const script = `
Add-Type -AssemblyName System.Drawing
$path = '${escaped}'
$source = [System.Drawing.Bitmap]::FromFile($path)
$bitmap = New-Object System.Drawing.Bitmap($source.Width, $source.Height, [System.Drawing.Imaging.PixelFormat]::Format32bppArgb)
$graphics = [System.Drawing.Graphics]::FromImage($bitmap)
$graphics.DrawImageUnscaled($source, 0, 0)
$graphics.Dispose()
$source.Dispose()
$radius = ${size} * 43.0 / 256.0
for ($y = 0; $y -lt ${size}; $y++) {
  for ($x = 0; $x -lt ${size}; $x++) {
    $dx = 0.0
    $dy = 0.0
    if ($x -lt $radius) { $dx = $radius - $x }
    elseif ($x -ge ${size} - $radius) { $dx = $x - (${size} - $radius - 1) }
    if ($y -lt $radius) { $dy = $radius - $y }
    elseif ($y -ge ${size} - $radius) { $dy = $y - (${size} - $radius - 1) }
    if (($dx -gt 0) -and ($dy -gt 0) -and (($dx * $dx + $dy * $dy) -gt ($radius * $radius))) {
      $pixel = $bitmap.GetPixel($x, $y)
      $bitmap.SetPixel($x, $y, [System.Drawing.Color]::FromArgb(0, $pixel.R, $pixel.G, $pixel.B))
    }
  }
}
$bitmap.Save($path, [System.Drawing.Imaging.ImageFormat]::Png)
$bitmap.Dispose()
`;
  const result = spawnSync("powershell.exe", ["-NoProfile", "-NonInteractive", "-Command", script], { stdio: "inherit" });
  if (result.status !== 0) throw new Error(`处理 ${size}x${size} 图标透明角失败`);
}

if (!fs.existsSync(source)) throw new Error("找不到 assets/icon-source.svg");
if (process.platform === "win32" && !fs.existsSync(edge)) throw new Error("找不到 Microsoft Edge，无法生成应用图标");

fs.mkdirSync(outputDir, { recursive: true });
fs.mkdirSync(frontendPublicDir, { recursive: true });
const profileRoot = fs.mkdtempSync(path.join(os.tmpdir(), "diskbench-icon-"));
const sourceSvg = fs.readFileSync(source, "utf8");

try {
  for (const size of [64, 256]) {
    const output = path.join(outputDir, `icon_${size}.png`);
    const renderSource = path.join(profileRoot, `icon_${size}.svg`);
    fs.writeFileSync(renderSource, sourceSvg.replace('width="256" height="256"', `width="${size}" height="${size}"`));
    const sourceUrl = `file:///${renderSource.replace(/\\/g, "/")}`;
    fs.rmSync(output, { force: true });
    const result = spawnSync(edge, [
      "--headless", "--disable-gpu", "--no-sandbox", "--hide-scrollbars",
      "--default-background-color=00000000",
      `--user-data-dir=${path.join(profileRoot, String(size))}`,
      `--screenshot=${output}`, `--window-size=${size},${size}`, sourceUrl
    ], { stdio: "inherit" });
    if (result.status !== 0 || !waitForStableFile(output)) throw new Error(`生成 ${size}x${size} 图标失败`);
    makeRoundedCornersTransparent(output, size);
  }

  // fnOS also enlarges ICON.PNG in the app detail view. Use the 256px
  // rendering there so its edges stay sharp at the larger display size.
  fs.copyFileSync(path.join(outputDir, "icon_256.png"), path.join(packageDir, "ICON.PNG"));
  fs.copyFileSync(path.join(outputDir, "icon_256.png"), path.join(packageDir, "ICON_256.PNG"));
  fs.copyFileSync(path.join(outputDir, "icon_64.png"), path.join(frontendPublicDir, "favicon.png"));
} finally {
  // Edge can briefly retain profile files after the headless process exits.
  // The directory is already isolated under the operating-system temp path.
  try {
    fs.rmSync(profileRoot, { recursive: true, force: true, maxRetries: 5, retryDelay: 200 });
  } catch (error) {
    if (error.code !== "EPERM" && error.code !== "EBUSY") throw error;
  }
}

console.log(`Rendered desktop and installer icons in ${packageDir}`);
