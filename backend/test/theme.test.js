"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

const html = fs.readFileSync(path.join(__dirname, "../../frontend/index.html"), "utf8");
const script = html.match(/<script>([\s\S]*?)<\/script>/)?.[1];

function initialTheme(preference, systemDark = false, storageUnavailable = false) {
  const root = { dataset: {}, style: {} };
  const themeColor = { content: "" };
  vm.runInNewContext(script, {
    document: {
      documentElement: root,
      querySelector: () => themeColor
    },
    window: { matchMedia: () => ({ matches: systemDark }) },
    localStorage: { getItem: (key) => {
      if (storageUnavailable) throw new Error("Storage unavailable");
      return key === "diskbench-theme-choice" ? preference : null;
    } }
  });
  return { theme: root.dataset.theme, colorScheme: root.style.colorScheme, themeColor: themeColor.content };
}

test("new installations follow the system theme before the UI loads", () => {
  assert.deepEqual(initialTheme(null), {
    theme: "light", colorScheme: "light", themeColor: "#f6f7f9"
  });
  assert.deepEqual(initialTheme(null, true), {
    theme: "dark", colorScheme: "dark", themeColor: "#1c1d20"
  });
  assert.deepEqual(initialTheme(null, true, true), {
    theme: "dark", colorScheme: "dark", themeColor: "#1c1d20"
  });
});

test("an explicit theme choice overrides the system theme", () => {
  assert.deepEqual(initialTheme("light", true), {
    theme: "light", colorScheme: "light", themeColor: "#f6f7f9"
  });
  assert.deepEqual(initialTheme("dark", false), {
    theme: "dark", colorScheme: "dark", themeColor: "#1c1d20"
  });
});
