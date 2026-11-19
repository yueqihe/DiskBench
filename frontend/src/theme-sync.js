async function loadFnOsHost() {
  const { TrimApp } = await import("@trimjs/web-app");
  return new TrimApp();
}

export function createThemeSync({ onTheme, media = window.matchMedia?.("(prefers-color-scheme: dark)"), loadHost = loadFnOsHost }) {
  let theme = media?.matches ? "dark" : "light";
  let stopped = false;
  let hostTheme = null;
  let manualChanges = 0;
  let sdk;
  let subscribed = false;
  const validTheme = value => value === "light" || value === "dark";
  const apply = next => {
    if (stopped || !validTheme(next)) return;
    theme = next;
    onTheme(next);
  };
  const onMediaChange = event => {
    if (!hostTheme) apply(event.matches ? "dark" : "light");
  };
  const onHostTheme = next => {
    if (stopped || !validTheme(next)) return;
    hostTheme = next;
    apply(next);
  };
  const unsubscribeHost = () => {
    if (!subscribed) return;
    subscribed = false;
    Promise.resolve().then(() => sdk.$off("os/theme", onHostTheme)).catch(() => {});
  };

  apply(theme);
  media?.addEventListener("change", onMediaChange);

  const ready = (async () => {
    sdk = await loadHost();
    if (stopped) return;
    await sdk.ready();
    if (stopped || sdk.isStandaloneWeb) return;
    if (sdk.isWeb) {
      try {
        await sdk.$on("os/theme", onHostTheme);
        subscribed = true;
      } catch {
        // Older hosts can provide a snapshot without live theme events.
      }
      if (stopped) { unsubscribeHost(); return; }
    }
    const config = await sdk.getPlatformConfig();
    // A live event or a manual choice can arrive while the snapshot is pending.
    if (stopped || hostTheme || !validTheme(config?.theme)) return;
    hostTheme = config.theme;
    if (manualChanges === 0) apply(hostTheme);
  })().catch(() => {
    // Plain browsers and older fnOS versions keep the system-theme fallback.
  });

  return {
    ready,
    toggle() {
      manualChanges++;
      apply(theme === "dark" ? "light" : "dark");
    },
    stop() {
      stopped = true;
      media?.removeEventListener("change", onMediaChange);
      unsubscribeHost();
    }
  };
}
