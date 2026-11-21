"use strict";

const fs = require("node:fs");
const path = require("node:path");
const { Resvg } = require("@resvg/resvg-js");

const root = path.resolve(__dirname, "..");
const source = fs.readFileSync(path.join(root, "assets/icon-source.svg"), "utf8");
const packageDir = path.join(root, "package/diskbench");
const outputDir = path.join(packageDir, "app/ui/images");
const frontendPublicDir = path.join(root, "frontend/public");
const frameCount = 80;
const frameMilliseconds = 50;
const signature = Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]);

function activityLevels(seed) {
  // Fixed seeds keep builds reproducible while giving each LED an irregular
  // pattern of short flashes, short gaps and occasional idle pauses.
  const random = () => {
    seed = (Math.imul(seed, 1664525) + 1013904223) >>> 0;
    return seed / 0x100000000;
  };
  const levels = [];
  let lit = false;
  while (levels.length < frameCount) {
    const duration = lit ? 1 + Math.floor(random() * 2)
      : random() < .2 ? 5 + Math.floor(random() * 4) : 1 + Math.floor(random() * 3);
    const opacity = lit ? .8 + random() * .2 : .14;
    for (let frame = 0; frame < duration && levels.length < frameCount; frame++) levels.push(opacity);
    lit = !lit;
  }
  return levels;
}

const ledActivity = [activityLevels(0x6d2b79f5), activityLevels(0x1b873593)];

const crcTable = Array.from({ length: 256 }, (_, value) => {
  for (let bit = 0; bit < 8; bit++) value = value & 1 ? 0xedb88320 ^ (value >>> 1) : value >>> 1;
  return value >>> 0;
});

function chunk(type, data) {
  const name = Buffer.from(type, "ascii");
  let crc = 0xffffffff;
  for (const buffer of [name, data]) {
    for (const byte of buffer) crc = crcTable[(crc ^ byte) & 255] ^ (crc >>> 8);
  }
  const result = Buffer.alloc(data.length + 12);
  result.writeUInt32BE(data.length, 0);
  name.copy(result, 4);
  data.copy(result, 8);
  result.writeUInt32BE((crc ^ 0xffffffff) >>> 0, data.length + 8);
  return result;
}

function pngParts(png) {
  if (!png.subarray(0, 8).equals(signature)) throw new Error("Invalid rendered PNG");
  const parts = { image: [] };
  for (let offset = 8; offset < png.length;) {
    const length = png.readUInt32BE(offset);
    const type = png.toString("ascii", offset + 4, offset + 8);
    const data = png.subarray(offset + 8, offset + 8 + length);
    if (type === "IHDR") parts.header = data;
    if (type === "IDAT") parts.image.push(data);
    offset += length + 12;
  }
  if (!parts.header || !parts.image.length) throw new Error("Incomplete rendered PNG");
  return parts;
}

function animatedPng(still, frames) {
  const base = pngParts(still);
  const control = Buffer.alloc(8);
  control.writeUInt32BE(frames.length, 0);
  // A separate default image lets non-APNG decoders display the complete logo.
  // Loop count 0 means repeat indefinitely; every frame fully replaces the canvas.
  const chunks = [signature, chunk("IHDR", base.header), chunk("sRGB", Buffer.from([0])), chunk("acTL", control)];
  for (const data of base.image) chunks.push(chunk("IDAT", data));
  let sequence = 0;
  for (const frame of frames) {
    const parts = pngParts(frame);
    if (!parts.header.equals(base.header)) throw new Error("Animation frame format mismatch");
    const frameControl = Buffer.alloc(26);
    frameControl.writeUInt32BE(sequence++, 0);
    base.header.copy(frameControl, 4, 0, 8);
    frameControl.writeUInt16BE(frameMilliseconds, 20);
    frameControl.writeUInt16BE(1000, 22);
    chunks.push(chunk("fcTL", frameControl));
    const frameData = Buffer.alloc(4);
    frameData.writeUInt32BE(sequence++, 0);
    chunks.push(chunk("fdAT", Buffer.concat([frameData, ...parts.image])));
  }
  chunks.push(chunk("IEND", Buffer.alloc(0)));
  const result = Buffer.concat(chunks);
  if (result.length > 1024 * 1024) throw new Error("Animated icon exceeds fnOS's 1024 KB limit");
  return result;
}

function animationFrame(index) {
  const phase = index / frameCount;
  const progress = Math.max(0, Math.min(1, (phase - .1) / .8));
  const opacity = Math.sin(Math.PI * progress) ** 2;
  const scanY = 5.2 + 6.1 * progress;
  const activity = `
    <defs>
      <clipPath id="scan-face"><path d="M7.24 5H16.76L19.75 10.9H4.25Z"/></clipPath>
      <linearGradient id="scan-light"><stop stop-color="#8db9e9" stop-opacity="0"/><stop offset=".5" stop-color="#a9cff9"/><stop offset="1" stop-color="#8db9e9" stop-opacity="0"/></linearGradient>
    </defs>
    <g clip-path="url(#scan-face)" opacity="${opacity.toFixed(4)}" stroke="none">
      <rect x="3" y="${(scanY - 1.1).toFixed(4)}" width="18" height="2.2" fill="url(#scan-light)" opacity=".12"/>
      <rect x="3" y="${(scanY - .17).toFixed(4)}" width="18" height=".34" fill="url(#scan-light)"/>
    </g>`;
  return source.replace("<!-- activity -->", activity)
    .replace('id="status-led-1"', `id="status-led-1" stroke-opacity="${ledActivity[0][index].toFixed(4)}"`)
    .replace('id="status-led-2"', `id="status-led-2" stroke-opacity="${ledActivity[1][index].toFixed(4)}"`);
}

function render(svg, size) {
  return new Resvg(svg, { fitTo: { mode: "width", value: size }, font: { loadSystemFonts: false } }).render().asPng();
}

if (!source.includes("<!-- activity -->")) throw new Error("Missing icon activity layer");
fs.mkdirSync(outputDir, { recursive: true });
fs.mkdirSync(frontendPublicDir, { recursive: true });
for (const size of [64, 256]) {
  const still = render(source, size);
  const frames = Array.from({ length: frameCount }, (_, index) => render(animationFrame(index), size));
  const icon = animatedPng(still, frames);
  fs.writeFileSync(path.join(outputDir, `icon_${size}.png`), icon);
  // Keep the browser tab favicon still; only fnOS launcher/package icons animate.
  if (size === 64) fs.writeFileSync(path.join(frontendPublicDir, "favicon.png"), still);
  console.log(`Rendered ${size}px APNG: ${frameCount} frames, ${frameCount * frameMilliseconds} ms, ${icon.length} bytes`);
}

for (const [size, filename] of [[64, "ICON.PNG"], [256, "ICON_256.PNG"]]) {
  fs.copyFileSync(path.join(outputDir, `icon_${size}.png`), path.join(packageDir, filename));
}
console.log(`Rendered animated desktop and installer icons in ${packageDir}`);
