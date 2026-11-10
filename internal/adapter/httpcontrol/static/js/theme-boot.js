// Applies the theme this browser chose before the first paint, so a light
// choice never flashes dark. It is a classic script: the page allows no inline one.
try {
  if (localStorage.getItem("archive.theme") === "light") document.documentElement.dataset.theme = "light";
} catch {
  // Storage is unavailable: the default dark theme stays.
}
