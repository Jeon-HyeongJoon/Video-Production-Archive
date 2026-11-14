// Hash routes keep every screen addressable without server-side routing:
//   #/connections                     작업 PC, 드라이브, 에이전트 관리
//   #/drives/<drive>[?folder=<f>]     드라이브 상세, 그 폴더의 문제를 펼침
//   #/jobs                            작업
//   #/jobs/<job>                      영속 작업 상세
//   #/runs/<archive-run>[?result=<p>] 아카이브 작업 화면
//   #/search[?q=<text>]               검색 결과
//   #/settings                        설정
// A move between them runs as a View Transition whose direction follows the
// depth of the two screens: deeper slides forward, shallower slides back.

import { signal } from "./lib.js";
import { transition, settle } from "./motion.js";

export const menus = [
  { id: "jobs", label: "작업", path: "#/jobs" },
  { id: "connections", label: "장비·에이전트", path: "#/connections" },
];

export function parseRoute(hash) {
  const raw = String(hash || "").replace(/^#/, "") || "/jobs";
  const [pathPart, queryPart = ""] = raw.split("?");
  const parts = pathPart.split("/").filter(Boolean).map(decodeURIComponent);
  const query = new URLSearchParams(queryPart);
  const menu = parts[0] || "jobs";
  const full = `#${raw}`;
  if (menu === "drives" && parts[1]) return { menu: "connections", name: "drive", driveID: parts[1], query, full };
  if (menu === "jobs" && parts[1]) return { menu: "jobs", name: "job", jobID: parts[1], query, full };
  if (menu === "runs" && parts[1]) return { menu: "jobs", name: "run", workID: parts[1], query, full };
  // Old Worker and drive-list bookmarks land on 장비·에이전트.
  if (menu === "connections" || menu === "workers" || menu === "drives") return { menu: "connections", name: "connections", query, full };
  if (menu === "search") return { menu: "search", name: "search", query, full };
  if (menu === "settings") return { menu: "settings", name: "settings", query, full };
  // Old bookmarks and unknown routes land on 작업.
  return { menu: "jobs", name: "jobs", query, full: menu === "jobs" ? full : "#/jobs" };
}

export const route = signal(parseRoute(location.hash));

// shared names where a navigation started ({ key, src }): that element and the
// destination heading carry one view-transition name and move as one title.
export const shared = signal(null);

const DEPTH = { jobs: 1, connections: 1, search: 1, settings: 1, run: 2, drive: 2, job: 3 };
const moved = new Set();
const repeated = new Set();

// onRoute runs after every move; onRepeat when a link points at the screen
// already shown (an alert for the folder already in view).
export const onRoute = (listener) => moved.add(listener);
export const onRepeat = (listener) => repeated.add(listener);

let scroller = null;
export function setScroller(element) {
  scroller = element;
}

export async function go(path, src = "") {
  const next = parseRoute(path);
  const prev = route.peek();
  if (next.full === prev.full) {
    for (const listener of repeated) listener(next);
    return;
  }
  const back = DEPTH[next.name] < DEPTH[prev.name];
  const keep = back && shared.peek()?.key === prev.full;
  const target = src ? { key: next.full, src } : keep ? shared.peek() : null;
  if (target !== shared.peek()) {
    shared.value = target;
    await settle();
  }
  const direction = DEPTH[next.name] > DEPTH[prev.name] ? "forward" : back ? "back" : "swap";
  await transition(`nav-${direction}`, () => {
    if (location.hash !== next.full) location.hash = next.full;
    route.value = next;
    for (const listener of moved) listener(next, prev);
  }, () => { if (scroller) scroller.scrollTop = 0; });
}

// A link keeps its address for a new tab; a plain click moves inside the page.
document.addEventListener("click", (event) => {
  if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
  const link = event.target.closest?.('a[href^="#/"]');
  if (!link || link.target) return;
  event.preventDefault();
  go(link.getAttribute("href"), link.dataset.shared || "");
});

// Back, forward and typed addresses arrive as hash changes.
addEventListener("hashchange", () => {
  if (parseRoute(location.hash).full !== route.peek().full) go(location.hash);
});
