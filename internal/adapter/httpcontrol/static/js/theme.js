// Screen theme: dark grey by default. A light choice is made on the settings
// screen and this browser keeps it across visits.

import { el, svgIcon } from "./core.js";

const THEME_KEY = "archive.theme";
const CHOICES = [
  ["light", "라이트", "M12 8a4 4 0 1 0 0 8a4 4 0 1 0 0-8zM12 2.5v2M12 19.5v2M2.5 12h2M19.5 12h2M5.3 5.3l1.4 1.4M17.3 17.3l1.4 1.4M5.3 18.7l1.4-1.4M17.3 6.7l1.4-1.4"],
  ["dark", "다크", "M20 14.5A8 8 0 1 1 9.5 4a6.5 6.5 0 0 0 10.5 10.5z"],
];

function storedTheme() {
  try {
    return localStorage.getItem(THEME_KEY);
  } catch {
    return null;
  }
}

function applyTheme(theme) {
  document.documentElement.dataset.theme = theme === "light" ? "light" : "dark";
}

export function applyStoredTheme() {
  applyTheme(storedTheme());
}

// themeSwitch is the two-button choice the settings screen shows.
export function themeSwitch() {
  const group = el("div", { id: "theme-switch", class: "theme-switch", role: "group", "aria-label": "화면 테마" });
  const sync = () => {
    for (const button of group.querySelectorAll("[data-theme-choice]")) {
      button.setAttribute("aria-pressed", String(button.dataset.themeChoice === document.documentElement.dataset.theme));
    }
  };
  for (const [choice, name, path] of CHOICES) {
    group.append(el("button", {
      type: "button", "aria-label": name, title: name, dataset: { themeChoice: choice },
      onclick: () => {
        try {
          localStorage.setItem(THEME_KEY, choice);
        } catch {
          // The choice still holds until this page is closed.
        }
        applyTheme(choice);
        sync();
      },
    }, svgIcon(path)));
  }
  sync();
  return group;
}
