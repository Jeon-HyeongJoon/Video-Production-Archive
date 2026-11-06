// 사이드바: two primary menus and a scrollable work index. ArchiveRun cards
// group their internal Jobs; unrelated Template Jobs remain standalone.

import { byId, el, svgIcon, icons, statusIcon, formatCount } from "./core.js";
import { model, driveName, findDrive, workLane, workName } from "./store.js";
import { menus } from "./router.js";

const THEME_KEY = "archive.theme";
const MENU_ICON = { jobs: icons.jobs, connections: icons.workers };
const LANE_LABEL = {
  fault: "실패", decide: "확인 필요", work: "실행 중", resource: "자원 대기", queue: "대기열", done: "완료",
};
const LANE_RANK = { fault: 5, decide: 4, work: 3, resource: 2, queue: 1, done: 0 };

function storedTheme() {
  try {
    return localStorage.getItem(THEME_KEY);
  } catch {
    return null;
  }
}

function applyTheme(theme) {
  document.documentElement.dataset.theme = theme === "light" ? "light" : "dark";
  for (const button of byId("theme-switch").querySelectorAll("[data-theme-choice]")) {
    button.setAttribute("aria-pressed", String(button.dataset.themeChoice === document.documentElement.dataset.theme));
  }
}

function workspaceContext(work) {
  const drive = findDrive(work.source_volume_id);
  return {
    drive: drive ? driveName(drive) : work.source_volume_id || (work.kind === "JOB" ? "드라이브 없음" : "원본 없음"),
    worker: work.worker_id || drive?.worker_id || "배정 없음",
  };
}

function workspaceCard(work, route) {
  const lane = workLane(work);
  const context = workspaceContext(work);
  const current = (route.name === "run" && route.workID === work.work_id) || (route.name === "job" && route.jobID === work.work_id);
  const state = LANE_LABEL[lane];
  const href = work.kind === "ARCHIVE_RUN" ? `#/runs/${encodeURIComponent(work.work_id)}` : `#/jobs/${encodeURIComponent(work.work_id)}`;
  return el("a", {
    class: "workspace-card", href,
    "aria-current": current ? "page" : undefined, dataset: { workId: work.work_id, kind: work.kind, lane },
    title: [workName(work), work.work_id, context.drive, context.worker].filter(Boolean).join(" · "),
  },
  el("span", { class: "workspace-card-head" },
    statusIcon(lane === "queue" ? "idle" : lane),
    el("b", { class: "workspace-work-name" }, workName(work)),
    el("span", { class: "workspace-state", dataset: { signal: ["fault", "decide"].includes(lane) ? lane : undefined } }, state)),
  el("span", { class: "workspace-card-meta" },
    el("span", { class: "workspace-drive" }, context.drive),
    el("span", { "aria-hidden": "true" }, "·"),
    el("span", { class: "workspace-worker num" }, context.worker)),
  el("span", { class: "workspace-card-id num" }, work.work_id));
}

function orderedWorkspaces() {
  return [...model.work].sort((left, right) => {
    const leftLane = workLane(left);
    const rightLane = workLane(right);
    const lane = LANE_RANK[rightLane] - LANE_RANK[leftLane];
    if (lane) return lane;
    const recent = new Date(right.last_event_at || 0) - new Date(left.last_event_at || 0);
    return recent || left.work_id.localeCompare(right.work_id);
  });
}

export function mountSidebar() {
  const nav = byId("menu");
  const counts = {
    jobs: el("span", { class: "menu-count num", id: "job-count" }),
    connections: el("span", { class: "menu-count num", id: "connection-count" }),
  };
  nav.replaceChildren(...menus.map((menu) => el("a", { href: menu.path, class: "menu-link", dataset: { menu: menu.id } },
    svgIcon(MENU_ICON[menu.id]), el("span", { class: "menu-label" }, menu.label), counts[menu.id])));
  const tree = byId("tree");

  applyTheme(storedTheme());
  byId("theme-switch").addEventListener("click", (event) => {
    const choice = event.target.closest("[data-theme-choice]")?.dataset.themeChoice;
    if (!choice) return;
    try {
      localStorage.setItem(THEME_KEY, choice);
    } catch {
      // The choice still holds until this page is closed.
    }
    applyTheme(choice);
  });

  return {
    update(route) {
      for (const link of nav.querySelectorAll(".menu-link")) {
        if (link.dataset.menu === route.menu) link.setAttribute("aria-current", "page");
        else link.removeAttribute("aria-current");
      }
      const settings = byId("open-settings");
      if (route.menu === "settings") settings.setAttribute("aria-current", "page");
      else settings.removeAttribute("aria-current");

      counts.jobs.textContent = formatCount(model.work.length);
      counts.connections.textContent = formatCount(model.workers.length + model.drives.length + model.sessions.length);
      const workspaces = orderedWorkspaces();
      tree.replaceChildren(
        el("span", { class: "tree-title" }, "워크스페이스"),
        el("div", { class: "workspace-list" }, ...(workspaces.length
          ? workspaces.map((work) => workspaceCard(work, route))
          : [el("p", { class: "workspace-empty" }, "작업 없음")])));
    },
  };
}
