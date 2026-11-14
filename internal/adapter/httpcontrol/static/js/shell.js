// The frame every screen sits in: the sidebar on the left (search and alerts
// on top, the two menus, the list of unfinished work, settings at the foot),
// the screen in the middle and the Agent on the right. Problems a person must
// see from anywhere live in the bell; one that appears after the screen was
// opened also rises as a toast. While refreshes fail, one line says so and
// every operation waits.

import { html, useEffect, useRef, useState, useComputed, effect } from "./lib.js";
import { clockTime } from "./core.js";
import {
  store, ui, stale, problemItems, problemSubject, problemPath, workLane, workTitle, workDrive, driveNameOf, workspaces,
  workText, workKind, workIssues, workHref, startVisit,
} from "./store.js";
import { route, menus, go, shared, onRoute } from "./router.js";
import { StatusIcon, Glyph, GLYPH, Swap, IssueTags, cssName } from "./ui.js";
import { AgentPane, openAgentCard } from "./agent.js";
import { JobsScreen } from "./views/jobs.js";
import { RunScreen } from "./views/run.js";
import { JobScreen } from "./views/job.js";
import { DriveScreen } from "./views/drive.js";
import { ConnectionsScreen } from "./views/connections.js";
import { SearchScreen } from "./views/search.js";
import { SettingsScreen } from "./views/settings.js";

// isShared marks the element a navigation started from, so it and the next
// screen's heading move as one title.
export const useShared = (key, src) => useComputed(() => shared.value?.key === key && shared.value.src === src && route.value.full !== key).value;

function WorkspaceCard({ work }) {
  const href = workHref(work);
  const current = useComputed(() => {
    const now = route.value;
    return (now.name === "run" && now.workID === work.work_id) || (now.name === "job" && now.jobID === work.work_id)
      || (now.name === "drive" && work.kind === "ARCHIVE_RUN" && workDrive(work)?.workspace_drive_id === now.driveID);
  }).value;
  const isShared = useShared(href, "card");
  const drive = work.kind === "ARCHIVE_RUN" ? driveNameOf(work.source_volume_id) || work.source_volume_id : "드라이브 없음";
  const title = [workTitle(work), work.work_id, drive || "원본 없음", work.worker_id || "배정 없음"].join(" · ");
  const issues = work.kind === "ARCHIVE_RUN" ? workIssues(work) : null;
  return html`<a class="workspace-card vt" href=${href} data-shared="card" style=${`--vtn:card-${cssName(work.work_id)}`}
    aria-current=${current ? "page" : null} data-work-id=${work.work_id} data-kind=${work.kind} data-lane=${workLane(work)} title=${title}>
    <span class="card-head"><${StatusIcon} kind=${workKind(work)} /><b class=${`workspace-work-name${isShared ? " shared" : ""}`}>${workTitle(work)}</b></span>
    <span class="card-state"><${Swap} cls="workspace-state" text=${workText(work)} />${issues ? html`<${IssueTags} ...${issues} cls="card-issue" />` : null}</span>
  </a>`;
}

function Sidebar() {
  const now = route.value;
  const items = problemItems.value;
  const faults = items.filter((item) => item.signal === "fault").length;
  const fresh = ui.newProblems.value.size;
  const [query, setQuery] = useState(now.name === "search" ? now.query.get("q") || "" : "");
  const input = useRef();
  useEffect(() => {
    if (now.name === "search" && document.activeElement !== input.current) setQuery(now.query.get("q") || "");
  }, [now.full]);
  useEffect(() => {
    // "/" puts the cursor in the search box from anywhere but a text field.
    const onSlash = (event) => {
      if (event.key !== "/" || event.metaKey || event.ctrlKey || event.altKey) return;
      if (event.target.closest?.("input, textarea, select, [contenteditable]")) return;
      event.preventDefault();
      input.current?.focus();
    };
    document.addEventListener("keydown", onSlash);
    return () => document.removeEventListener("keydown", onSlash);
  }, []);
  const search = (event) => {
    event.preventDefault();
    go(`#/search?q=${encodeURIComponent(query.trim())}`);
  };
  const list = store.ready.value ? workspaces.value : null;
  return html`<nav class="sidebar" aria-label="메뉴">
    <div class="side-tools">
      <form id="search-form" class="side-search" role="search" onSubmit=${search}>
        <${Glyph} d=${GLYPH.search} />
        <input ref=${input} id="search-text" type="search" name="q" value=${query} onInput=${(event) => setQuery(event.currentTarget.value)}
          placeholder="보관 영상·자막 검색" aria-label="보관 영상·자막 검색" autocomplete="off" />
        <kbd aria-hidden="true">/</kbd>
      </form>
      <button type="button" id="alert-button" class="bell" aria-haspopup="dialog" aria-controls="alert-panel" aria-expanded=${ui.alertsOpen.value ? "true" : "false"}
        data-new=${fresh ? "true" : "false"} aria-label=${`알림 · 실패 ${faults} · 직접 확인 ${items.length - faults}${fresh ? ` · 새 ${fresh}` : ""}`} onClick=${toggleAlerts}>
        <${Glyph} d=${GLYPH.bell} />
        ${items.length ? html`<b id="alert-count" class="bell-count num" data-signal=${faults ? "fault" : "decide"}><${Swap} text=${String(items.length)} /></b>` : null}
        ${fresh ? html`<i class="bell-new" aria-hidden="true"></i>` : null}
      </button>
    </div>
    <div id="menu" class="menu">
      ${menus.map((menu) => html`<a class="menu-link" key=${menu.id} href=${menu.path} data-menu=${menu.id} aria-current=${now.menu === menu.id ? "page" : null}>
        <${Glyph} d=${GLYPH[menu.id]} /><span class="menu-label">${menu.label}</span></a>`)}
    </div>
    <section id="tree" class="tree" aria-label="현재 작업">
      <span class="tree-title">현재 작업</span>
      ${list === null ? html`<span class="skel-card"></span><span class="skel-card"></span>`
        : list.length ? list.map((work) => html`<${WorkspaceCard} key=${work.work_id} work=${work} />`) : html`<p class="empty">작업 없음</p>`}
    </section>
    <div class="side-foot">
      <a id="open-settings" class="menu-link" href="#/settings" aria-current=${now.menu === "settings" ? "page" : null}><${Glyph} d=${GLYPH.settings} /><span class="menu-label">설정</span></a>
    </div>
  </nav>`;
}

// ─── Alerts ─────────────────────────────────────────────────────────────────

function toggleAlerts(event) {
  if (ui.alertsOpen.peek()) return closeAlerts();
  const box = event.currentTarget.getBoundingClientRect();
  ui.alertsOpen.value = { top: box.bottom + 6, left: Math.max(12, Math.min(box.left, innerWidth - 352)) };
}

function closeAlerts() {
  if (!ui.alertsOpen.peek()) return;
  ui.alertsOpen.value = null;
  ui.newProblems.value = new Set();
}

// resolve takes the person to where the item is decided: the problem opened
// in its drive, the Worker or Job, or the Agent's card for a decision it proposed.
function resolve(item) {
  closeAlerts();
  ui.fresh.value = "";
  const next = new Set(ui.newProblems.peek());
  if (next.delete(item.key)) ui.newProblems.value = next;
  if (item.kind === "card") return openAgentCard(item.sessionID, item.cardID);
  go(problemPath(item));
}

const ALERTS_SHOWN = 100;

function AlertPanel() {
  const at = ui.alertsOpen.value;
  const panel = useRef();
  useEffect(() => {
    if (!at) return undefined;
    const outside = (event) => {
      if (panel.current?.contains(event.target) || event.target.closest?.("#alert-button")) return;
      closeAlerts();
    };
    const escape = (event) => { if (event.key === "Escape") { closeAlerts(); document.getElementById("alert-button")?.focus(); } };
    document.addEventListener("pointerdown", outside, true);
    document.addEventListener("keydown", escape);
    return () => { document.removeEventListener("pointerdown", outside, true); document.removeEventListener("keydown", escape); };
  }, [!!at]);
  if (!at) return null;
  const items = problemItems.value;
  const faults = items.filter((item) => item.signal === "fault").length;
  const fresh = ui.newProblems.value;
  return html`<section ref=${panel} id="alert-panel" class="alert-panel" role="dialog" aria-label="알림" style=${`top:${at.top}px;left:${at.left}px`}>
    <div class="alert-head"><span>알림<span class="num">실패 ${faults} · 직접 확인 ${items.length - faults}</span></span>
      <button type="button" class="icon-btn" aria-label="닫기" onClick=${closeAlerts}><${Glyph} d=${GLYPH.close} /></button></div>
    <div class="alert-list">
      ${items.length ? items.slice(0, ALERTS_SHOWN).map((item) => html`<button type="button" class="alert-item" key=${item.key} data-key=${item.key} data-kind=${item.kind} data-signal=${item.signal} onClick=${() => resolve(item)}>
        <${StatusIcon} kind=${item.signal} />
        <span><b>${item.title}</b><small>${problemSubject(item)}</small></span>
        ${fresh.has(item.key) ? html`<span class="new-tag">새</span>` : html`<span></span>`}
        <${Glyph} d=${GLYPH.next} />
      </button>`) : html`<p class="alert-empty">확인할 것 없음</p>`}
      ${items.length > ALERTS_SHOWN ? html`<p class="alert-empty">외 ${items.length - ALERTS_SHOWN}건 · 각 드라이브의 직접 확인할 항목에서 확인</p>` : null}
    </div>
  </section>`;
}

function AlertToast() {
  const item = useComputed(() => problemItems.value.find((entry) => entry.key === ui.fresh.value) || null).value;
  const kept = useRef(null);
  if (item) kept.current = item;
  const shown = kept.current;
  return html`<div id="fresh-banner" class="alert-toast" role="status" data-show=${item ? "true" : "false"} aria-hidden=${item ? null : "true"}>
    ${shown ? [
      html`<${StatusIcon} key="icon" kind=${shown.signal} />`,
      html`<span key="text" class="toast-text"><b>새 ${shown.title}</b><small id="fresh-text">${problemSubject(shown)}</small></span>`,
      html`<button key="go" type="button" id="fresh-go" class="btn btn-sm" onClick=${() => resolve(shown)}>열기</button>`,
      html`<button key="close" type="button" id="fresh-close" class="icon-btn" aria-label="알림 닫기" onClick=${() => { ui.fresh.value = ""; }}><${Glyph} d=${GLYPH.close} /></button>`,
    ] : null}
  </div>`;
}

function Stale() {
  if (!stale.value) return null;
  const at = store.refreshedAt.value;
  return html`<div id="stale-banner" class="stale" role="alert"><${StatusIcon} kind="fault" />
    <span><b>화면 갱신 중단</b> · <span id="stale-text">${at ? `${clockTime(at, true)} 이후 새 정보 없음` : "새 정보 없음"} · 다시 연결 중 · 선택과 작업 시작 잠김</span></span></div>`;
}

// ─── Screens ────────────────────────────────────────────────────────────────

const BootScreen = () => html`<div class="boot-view" aria-busy="true"><span class="skel boot-line"></span><span class="skel boot-title"></span><span class="skel-card boot-card"></span><span class="skel-card boot-card tall"></span></div>`;

function Screen() {
  const now = route.value;
  if (!store.ready.value) return html`<${BootScreen} />`;
  if (now.name === "jobs") return html`<${JobsScreen} />`;
  if (now.name === "run") return html`<${RunScreen} key=${now.workID} id=${now.workID} />`;
  if (now.name === "job") return html`<${JobScreen} key=${now.jobID} id=${now.jobID} />`;
  if (now.name === "drive") return html`<${DriveScreen} key=${now.driveID} id=${now.driveID} />`;
  if (now.name === "connections") return html`<${ConnectionsScreen} />`;
  if (now.name === "search") return html`<${SearchScreen} key=${now.query.get("q") || ""} />`;
  return html`<${SettingsScreen} />`;
}

effect(() => {
  document.documentElement.toggleAttribute("data-stale", stale.value);
});
onRoute(startVisit);

export function App({ onView }) {
  const view = useRef();
  useEffect(() => { onView(view.current); }, []);
  const waiting = useComputed(() => problemItems.value.some((item) => item.kind === "card")).value;
  return html`<div class="page">
    <div class="shell">
      <${Sidebar} />
      <main id="view" class="view" ref=${view} data-screen=${route.value.name}><div class="view-inner"><${Stale} /><${Screen} /></div></main>
      <${AgentPane} />
    </div>
    <button type="button" id="agent-handle" class="agent-handle" aria-controls="archivist-pane" aria-expanded=${ui.agentOpen.value ? "true" : "false"}
      onClick=${() => { ui.agentOpen.value = !ui.agentOpen.peek(); }}>${waiting ? html`<i aria-label="실행 전 확인 필요"></i>` : null}에이전트</button>
    <div class="agent-scrim" data-open=${ui.agentOpen.value ? "true" : "false"} onClick=${() => { ui.agentOpen.value = false; }}></div>
    <${AlertPanel} />
    <${AlertToast} />
  </div>`;
}
