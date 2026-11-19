"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

const html = fs.readFileSync(path.join(__dirname, "../../frontend/index.html"), "utf8");
const script = fs.readFileSync(path.join(__dirname, "../../frontend/public/theme-init.js"), "utf8");

test("theme initialization uses a same-origin script permitted by the production CSP", () => {
  assert.match(html, /<script src="\/theme-init\.js"><\/script>/);
  assert.doesNotMatch(html, /<script>\s*[\s\S]+?<\/script>/);
});

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
    theme: "light", colorScheme: "light", themeColor: "#e4e8ed"
  });
  assert.deepEqual(initialTheme(null, true), {
    theme: "dark", colorScheme: "dark", themeColor: "#17191d"
  });
  assert.deepEqual(initialTheme(null, true, true), {
    theme: "dark", colorScheme: "dark", themeColor: "#17191d"
  });
});

test("legacy saved choices do not block following the system theme", () => {
  assert.deepEqual(initialTheme("light", true), {
    theme: "dark", colorScheme: "dark", themeColor: "#17191d"
  });
  assert.deepEqual(initialTheme("dark", false), {
    theme: "light", colorScheme: "light", themeColor: "#e4e8ed"
  });
});
