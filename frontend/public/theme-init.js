(() => {
  const theme = window.matchMedia?.("(prefers-color-scheme: dark)")?.matches ? "dark" : "light";
  document.documentElement.dataset.theme = theme;
  document.documentElement.dataset.embedded = String(window.self !== window.top);
  document.documentElement.style.colorScheme = theme;
  document.querySelector('meta[name="theme-color"]').content = theme === "dark" ? "#17191d" : "#e4e8ed";
})();
