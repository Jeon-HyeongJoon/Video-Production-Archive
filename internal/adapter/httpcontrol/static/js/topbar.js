// 상단 막대: where the screen is, search across records and transcripts, and
// the 알림 button that lists every failure and human wait one by one. Under
// it sits the 연결 끊김 line while refreshes fail; a problem that appeared
// after the screen was opened rises as a toast at the bottom right.

import { byId, el, svgIcon, icons, statusIcon, setStatusIcon, clockTime } from "./core.js";
import { model, ui, notify, isStale, problemItems, sortedProblems, problemPath, problemSubject, newProblems, findDrive, driveName, workName } from "./store.js";
import { go } from "./router.js";

const PLACE = { drive: "연결 관리", jobs: "작업 관리", job: "작업 관리", run: "작업 관리", connections: "연결 관리", search: "검색", settings: "설정" };

function resolve(item) {
  newProblems.delete(item.key);
  ui.alertOpen = false;
  if (item.kind === "card") {
    ui.focusCard = item.cardID;
    ui.sessionID = item.sessionID;
    notify();
    return;
  }
  go(problemPath(item));
}

function alertItem(item) {
  return el("button", { type: "button", class: "alert-item", dataset: { kind: item.kind, signal: item.signal, key: item.key }, onclick: () => resolve(item) },
    statusIcon(item.signal),
    el("span", { class: "alert-item-text" }, el("b", {}, item.title), el("span", {}, problemSubject(item))),
    newProblems.has(item.key) ? el("span", { class: "new-tag" }, "새") : null,
    svgIcon(icons.next));
}

// crumbs names the menu and, below it, the drive, Worker or Job in view.
function crumbs(route) {
  const items = [[PLACE[route.name] || "작업 관리", route.name === "drive" || route.name === "job" || route.name === "run" || (route.name === "connections" && route.query.get("worker")) ? `#/${route.menu}` : ""]];
  if (route.name === "drive") items.push([driveName(findDrive(route.driveID) || { workspace_drive_id: route.driveID }), ""]);
  if (route.name === "connections" && route.query.get("worker")) items.push([route.query.get("worker"), ""]);
  if (route.name === "job") {
    const job = model.jobs.find((entry) => entry.job_id === route.jobID);
    items.push([job?.target_title || route.jobID, ""]);
  }
  if (route.name === "run") items.push([workName(model.work.find((entry) => entry.work_id === route.workID)) || route.workID, ""]);
  return items.flatMap(([text, href], index) => [
    svgIcon(icons.slash, "0 0 24 24", "crumb-sep"),
    href ? el("a", { class: "crumb", href }, text) : el("span", { class: "crumb", "aria-current": index === items.length - 1 ? "page" : undefined }, text),
  ]);
}

export function mountTopbar() {
  const search = byId("search-text");
  byId("search-form").addEventListener("submit", (event) => {
    event.preventDefault();
    go(`#/search?q=${encodeURIComponent(search.value.trim())}`);
  });
  // "/" puts the cursor in the search box from anywhere but a text field.
  document.addEventListener("keydown", (event) => {
    if (event.key !== "/" || event.metaKey || event.ctrlKey || event.altKey) return;
    if (event.target.closest?.("input, textarea, select, [contenteditable]")) return;
    event.preventDefault();
    search.focus();
  });

  const alertButton = byId("alert-button");
  const alertPanel = byId("alert-panel");
  const close = () => {
    if (!ui.alertOpen) return;
    ui.alertOpen = false;
    newProblems.clear();
    notify();
  };
  alertButton.addEventListener("click", () => {
    if (ui.alertOpen) { close(); return; }
    ui.alertOpen = true;
    notify();
  });
  document.addEventListener("click", (event) => { if (!byId("alert-wrap").contains(event.target)) close(); });
  window.addEventListener("hashchange", () => { ui.alertOpen = false; });
  alertPanel.addEventListener("keydown", (event) => { if (event.key === "Escape") { close(); alertButton.focus(); } });

  const fresh = byId("fresh-banner");
  byId("fresh-close").addEventListener("click", () => { ui.fresh = ""; notify(); });
  byId("fresh-go").addEventListener("click", () => {
    const item = problemItems().find((entry) => entry.key === ui.fresh);
    ui.fresh = "";
    if (item) resolve(item);
    else notify();
  });

  return {
    update(route) {
      byId("crumbs").replaceChildren(...crumbs(route));
      if (route.name === "search" && document.activeElement !== search) search.value = route.query.get("q") || "";

      const items = sortedProblems();
      const count = byId("alert-count");
      const faults = items.filter((item) => item.signal === "fault").length;
      count.hidden = !items.length;
      count.textContent = String(items.length);
      count.dataset.signal = faults ? "fault" : "decide";
      alertButton.dataset.new = String(newProblems.size > 0);
      alertButton.setAttribute("aria-label", `알림 · 실패 ${faults} · 확인 필요 ${items.length - faults}${newProblems.size ? ` · 새 ${newProblems.size}` : ""}`);
      alertButton.setAttribute("aria-expanded", String(ui.alertOpen));
      alertPanel.hidden = !ui.alertOpen;
      if (ui.alertOpen) {
        alertPanel.replaceChildren(
          el("div", { class: "alert-head" }, el("b", {}, "알림"), el("span", { class: "num" }, `실패 ${faults} · 확인 필요 ${items.length - faults}`)),
          el("div", { class: "alert-list" }, ...(items.length ? items.map(alertItem) : [el("p", { class: "empty" }, "확인할 것 없음")])));
      }

      const stale = byId("stale-banner");
      stale.hidden = !isStale();
      byId("stale-text").textContent = `${model.refreshedAt ? `${clockTime(model.refreshedAt, true)} 이후 갱신 없음` : "갱신 없음"} · 다시 연결 중 · 판정·실행 잠김`;

      const item = items.find((entry) => entry.key === ui.fresh);
      fresh.hidden = !item;
      if (item) {
        setStatusIcon(fresh.querySelector(".sicon"), item.signal);
        byId("fresh-text").textContent = `${problemSubject(item)} · ${item.title}`;
      }
    },
  };
}
