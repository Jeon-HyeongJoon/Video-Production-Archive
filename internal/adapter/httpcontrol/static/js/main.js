// Archive Workspace entry: mounts the screen for the current route, the top
// bar, the sidebar on the left and the Archivist pane on the right, and
// redraws them all from one model.

import { byId, setLocked } from "./core.js";
import { subscribe, startPolling, isStale } from "./store.js";
import { currentRoute } from "./router.js";
import { mountTopbar } from "./topbar.js";
import { mountSidebar } from "./sidebar.js";
import { mountAgent } from "./agent.js";
import * as drive from "./views/drive.js";
import * as jobs from "./views/jobs.js";
import * as connections from "./views/workers.js";
import * as search from "./views/search.js";
import * as settings from "./views/settings.js";

const screens = { drive, jobs, job: jobs, connections, search, settings };
const root = byId("view");
const topbar = mountTopbar();
const sidebar = mountSidebar();
const agent = mountAgent(byId("archivist-pane"));
let active = null;
let activeKey = "";

function render() {
  const route = currentRoute();
  const key = `${route.key}?${route.name === "drive" || route.name === "connections" ? route.query.toString() : ""}`;
  if (key !== activeKey) {
    active?.dispose?.();
    activeKey = key;
    root.replaceChildren();
    root.dataset.screen = route.name;
    active = screens[route.name].mount(root, route);
    root.scrollTop = 0;
  }
  const stale = isStale();
  setLocked(stale);
  active.update(route);
  topbar.update(route);
  sidebar.update(route);
  agent.update(route);
  if (stale) for (const button of document.querySelectorAll("[data-action]")) button.disabled = true;
}

subscribe(render);
window.addEventListener("hashchange", render);
render();
startPolling();
