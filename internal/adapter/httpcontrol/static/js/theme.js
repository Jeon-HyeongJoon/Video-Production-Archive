// Screen theme: dark grey by default. A light choice is made on the settings
// screen and this browser keeps it across visits; theme-boot.js applies it
// before the first paint.

import { html, signal } from "./lib.js";

const THEME_KEY = "archive.theme";
const CHOICES = [["dark", "어둡게"], ["light", "밝게"]];
const theme = signal(document.documentElement.dataset.theme === "light" ? "light" : "dark");

function choose(choice) {
  try {
    localStorage.setItem(THEME_KEY, choice);
  } catch {
    // The choice still holds until this page is closed.
  }
  document.documentElement.dataset.theme = choice;
  theme.value = choice;
}

export function ThemeSwitch() {
  return html`<div id="theme-switch" class="seg plain-seg theme-switch" role="group" aria-label="화면 테마">
    ${CHOICES.map(([choice, name]) => html`<button type="button" key=${choice} data-theme-choice=${choice} aria-pressed=${theme.value === choice ? "true" : "false"} onClick=${() => choose(choice)}>${name}</button>`)}
  </div>`;
}
