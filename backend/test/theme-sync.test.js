"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");
let createThemeSync;
test.before(async () => ({ createThemeSync } = await import("../../frontend/src/theme-sync.js")));

function mediaQuery(matches = false) {
  const listeners = new Set();
  return {
    matches,
    addEventListener: (event, handler) => listeners.add(handler),
    removeEventListener: (event, handler) => listeners.delete(handler),
    change(matches) { this.matches = matches; listeners.forEach(handler => handler({ matches })); },
    get listeners() { return listeners.size; }
  };
}

function deferred() {
  let resolve;
  const promise = new Promise(done => { resolve = done; });
  return { promise, resolve };
}

function host(theme = "light") {
  const listeners = new Set();
  return {
    isWeb: true,
    isStandaloneWeb: false,
    ready: async () => {},
    getPlatformConfig: async () => ({ theme }),
    $on: async (event, handler) => { assert.equal(event, "os/theme"); listeners.add(handler); },
    $off: async (event, handler) => { assert.equal(event, "os/theme"); listeners.delete(handler); },
    change: value => listeners.forEach(handler => handler(value)),
    get listeners() { return listeners.size; }
  };
}

test("without a host, defaults to light and resumes following system changes after a manual toggle", async () => {
  const media = mediaQuery();
  const seen = [];
  const sync = createThemeSync({ media, onTheme: theme => seen.push(theme), loadHost: async () => { throw new Error("no host"); } });
  await sync.ready;
  sync.toggle();
  media.change(true);
  media.change(false);
  assert.deepEqual(seen, ["light", "dark", "dark", "light"]);
  sync.stop();
  media.change(true);
  assert.equal(seen.at(-1), "light");
  assert.equal(media.listeners, 0);
});

test("fnOS theme takes precedence over browser theme and live events override a manual choice", async () => {
  const sdk = host("light");
  const media = mediaQuery(true);
  const seen = [];
  const sync = createThemeSync({ media, onTheme: theme => seen.push(theme), loadHost: async () => sdk });
  await sync.ready;
  assert.equal(seen.at(-1), "light");
  media.change(false);
  media.change(true);
  assert.equal(seen.at(-1), "light");
  sync.toggle();
  sdk.change("light");
  sdk.change("invalid");
  assert.equal(seen.at(-1), "light");
  sdk.change("dark");
  assert.equal(seen.at(-1), "dark");
  sync.stop();
  await Promise.resolve();
  assert.equal(sdk.listeners, 0);
});

test("a delayed host snapshot cannot overwrite a newer theme event", async () => {
  const sdk = host();
  const snapshot = deferred();
  const requested = deferred();
  sdk.getPlatformConfig = () => { requested.resolve(); return snapshot.promise; };
  const seen = [];
  const sync = createThemeSync({ media: null, onTheme: theme => seen.push(theme), loadHost: async () => sdk });
  await requested.promise;
  sdk.change("dark");
  snapshot.resolve({ theme: "light" });
  await sync.ready;
  assert.deepEqual(seen, ["light", "dark"]);
  sync.stop();
});

test("a delayed host snapshot preserves a manual choice until the next host change", async () => {
  const sdk = host();
  const snapshot = deferred();
  const requested = deferred();
  sdk.getPlatformConfig = () => { requested.resolve(); return snapshot.promise; };
  const seen = [];
  const sync = createThemeSync({ media: null, onTheme: theme => seen.push(theme), loadHost: async () => sdk });
  await requested.promise;
  sync.toggle();
  snapshot.resolve({ theme: "light" });
  await sync.ready;
  assert.equal(seen.at(-1), "dark");
  sdk.change("light");
  assert.equal(seen.at(-1), "light");
  sync.stop();
});

test("stopping during host subscription removes the late listener without updating the page", async () => {
  const sdk = host();
  const registering = deferred();
  const registered = deferred();
  const on = sdk.$on;
  sdk.$on = async (...args) => { registering.resolve(); await registered.promise; await on(...args); };
  let queries = 0;
  sdk.getPlatformConfig = async () => { queries++; return { theme: "dark" }; };
  const seen = [];
  const sync = createThemeSync({ media: null, onTheme: theme => seen.push(theme), loadHost: async () => sdk });
  await registering.promise;
  sync.stop();
  registered.resolve();
  await sync.ready;
  await Promise.resolve();
  sdk.change("dark");
  assert.deepEqual(seen, ["light"]);
  assert.equal(sdk.listeners, 0);
  assert.equal(queries, 0);
});

test("standalone pages skip host APIs and mobile hosts only read their theme", async () => {
  const standalone = host();
  standalone.isStandaloneWeb = true;
  let standaloneCalls = 0;
  standalone.$on = standalone.getPlatformConfig = async () => { standaloneCalls++; };
  const first = createThemeSync({ media: null, onTheme: () => {}, loadHost: async () => standalone });
  await first.ready;
  assert.equal(standaloneCalls, 0);
  first.stop();
  const mobile = host("dark");
  mobile.isWeb = false;
  let mobileSubscriptions = 0;
  mobile.$on = async () => { mobileSubscriptions++; };
  const seen = [];
  const second = createThemeSync({ media: null, onTheme: theme => seen.push(theme), loadHost: async () => mobile });
  await second.ready;
  assert.deepEqual(seen, ["light", "dark"]);
  assert.equal(mobileSubscriptions, 0);
  second.stop();
});
