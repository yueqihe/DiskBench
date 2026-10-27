(() => {
  let saved;
  try {
    saved = localStorage.getItem("diskbench-theme-choice");
  } catch {}
  const theme = saved === "dark" || saved === "light"
    ? saved
    : (window.matchMedia?.("(prefers-color-scheme: dark)")?.matches ? "dark" : "light");
  document.documentElement.dataset.theme = theme;
  document.documentElement.style.colorScheme = theme;
  document.querySelector('meta[name="theme-color"]').content = theme === "dark" ? "#1c1d20" : "#f6f7f9";
})();
