// 사이드바: the 상태·드라이브·Worker menus with their counts, every Worker with
// the drives and drive-free Jobs it processes as one tree, settings and the
// light or dark theme. The theme is a convenience of this browser only.

import { byId, el, svgIcon, icons, statusIcon, workerIcon, problemCounts, label, stateSignal, formatCount } from "./core.js";
import { model, sortedProblems, drivePhase, driveName, driveProgress, driveProblemCounts, driveFreeJobs, jobLane, PHASE_ICON } from "./store.js";
import { menus } from "./router.js";

const THEME_KEY = "archive.theme";
const MENU_ICON = { status: icons.status, drives: icons.drives, workers: icons.workers };

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

function driveItem(drive, route) {
  const phase = drivePhase(drive);
  const counts = driveProblemCounts(drive);
  const current = route.name === "drive" && route.driveID === drive.workspace_drive_id;
  const meta = phase === "intake" ? "실행 전" : phase === "done" ? "완료" : `${driveProgress(drive).pct}%`;
  return el("a", { class: "tree-item", href: `#/drives/${encodeURIComponent(drive.workspace_drive_id)}`, "aria-current": current ? "page" : undefined, dataset: { driveId: drive.workspace_drive_id } },
    statusIcon(PHASE_ICON[phase]), el("span", { class: "tree-name" }, driveName(drive)),
    problemCounts(counts.fault, counts.decide), el("span", { class: "tree-meta num" }, meta));
}

function jobItem(job, route) {
  const lane = jobLane(job);
  const current = route.name === "workers" && route.query.get("job") === job.job_id;
  return el("a", { class: "tree-item", href: `#/workers?job=${encodeURIComponent(job.job_id)}`, "aria-current": current ? "page" : undefined, title: `드라이브 밖 작업 · ${job.job_id}`, dataset: { jobId: job.job_id } },
    statusIcon(lane === "queue" ? "idle" : lane), el("span", { class: "tree-name" }, job.target_title || job.job_id),
    el("span", { class: "tree-meta num" }, `${Math.round((job.progress || 0) * 100)}%`));
}

export function mountSidebar() {
  const nav = byId("menu");
  const counts = {
    status: el("span", { class: "menu-badges", id: "status-counts" }),
    drives: el("span", { class: "menu-count num", id: "drive-count" }),
    workers: el("span", { class: "menu-count num", id: "worker-count" }),
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

      const problems = sortedProblems();
      const faults = problems.filter((item) => item.signal === "fault").length;
      counts.status.replaceChildren(...[problemCounts(faults, problems.length - faults)].filter(Boolean));
      counts.drives.textContent = formatCount(model.drives.length);
      counts.workers.textContent = formatCount(model.workers.length);

      const freeJobs = driveFreeJobs();
      const known = new Set(model.workers.map((worker) => worker.worker_id));
      const groups = model.workers.map((worker) => {
        const focused = route.name === "workers" && route.query.get("worker") === worker.worker_id;
        const offline = stateSignal(worker.state) === "fault";
        const drives = model.drives.filter((drive) => drive.worker_id === worker.worker_id)
          .sort((left, right) => left.workspace_drive_id.localeCompare(right.workspace_drive_id));
        return el("div", { class: "tree-group", dataset: { workerId: worker.worker_id } },
          el("a", { class: "tree-worker", href: `#/workers?worker=${encodeURIComponent(worker.worker_id)}`, "aria-current": focused ? "page" : undefined },
            workerIcon(worker.state), el("span", { class: "tree-name" }, worker.worker_id),
            el("span", { class: "tree-state", dataset: { signal: offline ? "fault" : undefined } }, label("worker", worker.state))),
          ...drives.map((drive) => driveItem(drive, route)),
          ...freeJobs.filter((job) => job.worker_id === worker.worker_id).map((job) => jobItem(job, route)));
      });
      const others = freeJobs.filter((job) => !known.has(job.worker_id));
      if (others.length) {
        groups.push(el("div", { class: "tree-group", dataset: { workerId: "" } },
          el("span", { class: "tree-worker tree-other" }, el("span", { class: "tree-name" }, "그 밖의 작업")),
          ...others.map((job) => jobItem(job, route))));
      }
      tree.replaceChildren(el("span", { class: "tree-title" }, "Worker · 드라이브"), ...groups);
    },
  };
}
