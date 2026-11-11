// 연결 관리: Worker PC, logical drives and Agent/Host connections in one
// operational screen. Registration and credentials remain local-config work;
// this screen shows live state and links to the owning detail. A normal state
// in a labelled column is its icon alone.

import { html, useEffect, useRef } from "../lib.js";
import { label, stateSignal, formatBytes, formatPercent, formatCount, clockTime } from "../core.js";
import {
  store, allDrives, allSessions, drivePhase, driveName, driveProgress, driveProblems, driveConnection, availability, PHASE_LABEL, PHASE_ICON,
} from "../store.js";
import { route } from "../router.js";
import { StatusIcon, Chevron, Swap, Meter, Fine, IssueTags, Empty } from "../ui.js";

function admissionLabel(admission) {
  if (admission?.available) return "추가 가능";
  if (admission?.wait_reason) return `보류 · ${label("wait", admission.wait_reason)}`;
  return `보류 · ${label("admission", admission?.code)}`;
}

const Capacity = ({ name, value, detail = "", signal = "", hover = "" }) => html`<div class="capacity-item" title=${hover || null} data-signal=${signal || null}>
  <span>${name}</span><strong class="num">${value}</strong>${detail ? html`<small>${detail}</small>` : null}</div>`;

function CapacityGrid({ worker }) {
  const heavy = worker.heavy_admission || {};
  const light = worker.light_admission || {};
  return html`<div class="capacity capacity-grid" aria-label="자원">
    <${Capacity} name="CPU" value=${formatPercent(worker.cpu_average_5m)} />
    <${Capacity} name="GPU" value=${formatPercent(worker.gpu_average_5m)} />
    <${Capacity} name="메모리" value=${formatBytes(worker.capacity_available_memory_bytes)} hover=${`예약 ${formatBytes(worker.reserved_memory_bytes)} 차감`} />
    <${Capacity} name="작업 공간" value=${formatBytes(worker.capacity_scratch_free_bytes)} hover=${`예약 ${formatBytes(worker.reserved_scratch_bytes)} 차감`} />
    <${Capacity} name="HEAVY" value=${`${worker.heavy_slots_used || 0}/${worker.heavy_slots_maximum || 0}`} detail=${admissionLabel(heavy)} signal=${heavy.available ? "" : "decide"} />
    <${Capacity} name="LIGHT" value=${`${worker.light_slots_used || 0}/${worker.light_slots_maximum || 0}`} detail=${admissionLabel(light)} signal=${light.available ? "" : "decide"} />
    <${Capacity} name="관측" value=${clockTime(worker.observed_at || worker.last_heartbeat, true)} detail=${worker.additional_work_possible ? "수용 가능" : "보류"} signal=${worker.additional_work_possible ? "" : "decide"} />
  </div>`;
}

function StorageTable({ worker }) {
  const seen = new Map();
  for (const current of allDrives.value.filter((item) => item.worker_id === worker.worker_id)) {
    for (const storage of current.storage_candidates || []) {
      if (storage.role === "ARCHIVE_PRIMARY" && !seen.has(storage.storage_id)) seen.set(storage.storage_id, storage);
    }
  }
  const offline = stateSignal(worker.state) === "fault";
  const rows = [...seen.values()].sort((left, right) => left.storage_id.localeCompare(right.storage_id));
  return html`<div class="table storage-table" role="table">
    <div class="trow head" role="row"><span role="columnheader">아카이브 저장소</span><span role="columnheader">접근 방식</span><span role="columnheader">접근</span><span role="columnheader">여유 / 용량</span></div>
    ${rows.length ? rows.map((storage) => html`<div class="trow" role="row" key=${storage.storage_id} data-storage-id=${storage.storage_id}>
      <b>${storage.label || storage.storage_id}</b>
      <span>${label("storageAccess", storage.access_kind)}</span>
      ${offline ? html`<span class="muted">확인 불가</span>` : storage.accessible ? html`<${Fine} text="접근 가능" />` : html`<span class="ok"><${StatusIcon} kind="fault" />접근 안 됨</span>`}
      <span class="num">${!offline && storage.accessible && storage.capacity_bytes ? `${formatBytes(storage.free_bytes)} / ${formatBytes(storage.capacity_bytes)}` : "—"}</span>
    </div>`) : html`<${Empty} text="저장소 없음" />`}
  </div>`;
}

function WorkerCard({ worker, focused }) {
  const signal = stateSignal(worker.state);
  const offline = signal === "fault";
  const box = useRef();
  useEffect(() => { if (focused) box.current?.scrollIntoView({ block: "nearest" }); }, [focused]);
  return html`<section class="worker-card worker-row" ref=${box} aria-label=${worker.worker_id} data-worker-id=${worker.worker_id} data-signal=${offline ? "fault" : null} data-focus=${focused ? "true" : null}>
    <div class="worker-head" title=${`heartbeat ${clockTime(worker.last_heartbeat, true)}`}>
      ${offline ? html`<span class="ok"><${StatusIcon} kind="fault" /></span>` : html`<${Fine} text=${label("worker", worker.state)} kind=${signal === "work" ? "work" : "online"} />`}
      <b class="worker-name">${worker.worker_id}</b>
      ${offline ? html`<span class="state">${label("worker", worker.state)}</span>` : null}
      <small>${offline ? `마지막 응답 ${clockTime(worker.last_heartbeat)}` : `heartbeat ${clockTime(worker.last_heartbeat, true)}`}</small>
    </div>
    <div class="worker-body"><${StorageTable} worker=${worker} /><${CapacityGrid} worker=${worker} /></div>
  </section>`;
}

function DriveRow({ drive: current }) {
  const phase = drivePhase(current);
  const connection = driveConnection(current);
  const progress = driveProgress(current);
  const problems = driveProblems(current);
  const fault = problems.filter(([, problem]) => problem.signal === "fault").length;
  return html`<a class="trow" role="row" href=${`#/drives/${encodeURIComponent(current.workspace_drive_id)}`} data-shared="row" data-drive-id=${current.workspace_drive_id}>
    <b>${driveName(current)}</b>
    ${connection.state === "CONNECTED" ? html`<${Fine} text=${connection.text} />` : html`<span class="ok" data-signal=${connection.signal}><${StatusIcon} kind=${connection.signal === "fault" ? "fault" : "idle"} />${connection.text}</span>`}
    <span class="num">${current.connection_worker_id || current.worker_id || "배정 없음"}</span>
    <span class="state"><${StatusIcon} kind=${PHASE_ICON[phase]} /><${Swap} text=${PHASE_LABEL[phase]} /></span>
    ${phase === "intake" ? html`<span class="muted">—</span>`
      : phase === "done"
        ? html`<span class="num latest-scope">보관 ${formatCount(progress.storedFolders)} / ${formatCount(progress.folders)} 폴더 · 발견 ${formatCount(progress.discoveredFolders)} 폴더</span>`
        : html`<span class="cell-meter"><${Meter} value=${progress.pct / 100} label="현재 작업 보관 진행" /><span class="num">현재 작업 ${progress.pct}% · ${formatCount(progress.storedFolders)} / ${formatCount(progress.folders)} 폴더</span></span>`}
    <span>${current.archive_storage_id || "—"}</span>
    <span>${problems.length ? html`<${IssueTags} fault=${fault} decide=${problems.length - fault} />` : html`<span class="muted">—</span>`}</span>
    <${Chevron} />
  </a>`;
}

export function ConnectionsScreen() {
  const workers = store.workers.value;
  const drives = allDrives.value;
  const sessions = allSessions.value;
  const hosts = store.hosts.value;
  const focusWorker = route.value.query.get("worker") || "";
  const focusHost = route.value.query.get("host") || "";
  const online = workers.filter((worker) => stateSignal(worker.state) !== "fault").length;
  const connected = drives.filter((item) => item.connection_state === "CONNECTED").length;
  const detached = drives.filter((item) => item.connection_state === "DISCONNECTED").length;
  return [
    html`<div key="head" class="page-head">
      <div class="page-title"><h1 id="connections-title" title=${`Worker PC ${workers.length} · 연결 ${online} · 드라이브 ${drives.length} (연결 ${connected} · 해제 ${detached}) · Agent ${sessions.length}`}>연결 관리</h1></div>
      <a class="btn" href="#/settings">Agent 추가</a>
    </div>`,
    html`<section key="workers" class="section" id="connection-workers" aria-label="Worker PC">
      <h2 class="section-title">Worker PC${" "}<span class="num" id="connection-worker-count">${workers.length}</span></h2>
      <div class="worker-list" id="worker-list">
        ${workers.length ? workers.map((worker) => html`<${WorkerCard} key=${worker.worker_id} worker=${worker} focused=${focusWorker === worker.worker_id} />`) : html`<${Empty} text="Worker PC 없음" />`}
      </div>
    </section>`,
    html`<section key="drives" class="section" id="connection-drives" aria-label="드라이브">
      <h2 class="section-title">드라이브${" "}<span class="num" id="connection-drive-count">${drives.length}</span></h2>
      <div class="table drives-table" role="table">
        <div class="trow head" role="row">${["드라이브", "연결", "Worker", "단계", "최근 작업 범위", "아카이브", "확인 필요", ""].map((text, index) => html`<span role="columnheader" key=${index}>${text}</span>`)}</div>
        ${drives.length ? drives.map((current) => html`<${DriveRow} key=${current.workspace_drive_id} drive=${current} />`) : html`<${Empty} text="드라이브 없음" />`}
      </div>
    </section>`,
    html`<section key="agents" class="section" id="connection-agents" aria-label="Agent">
      <h2 class="section-title">Agent${" "}<span class="num" id="connection-agent-count">${sessions.length}</span></h2>
      <${AgentTable} sessions=${sessions} />
      <h2 class="section-title connection-subtitle">Agent Host${" "}<span class="num">${hosts.length}</span></h2>
      <${HostTable} hosts=${hosts} focus=${focusHost} />
    </section>`,
  ];
}

export function AgentTable({ sessions, rowClass = "", onRemove = null, removing = "" }) {
  return html`<div class=${`table agents-table${onRemove ? " has-actions" : ""}`} role="table">
    <div class="trow head" role="row"><span role="columnheader">Agent</span><span role="columnheader">Agent Host</span><span role="columnheader">CLI</span><span role="columnheader">상태</span>${onRemove ? html`<span role="columnheader"></span>` : null}</div>
    ${sessions.length ? sessions.map((session) => {
      const [text, signal] = availability(session);
      return html`<div class=${`trow ${rowClass}`.trim()} role="row" key=${session.session_id} data-session-id=${session.session_id}>
        <b>${session.name || session.session_id}</b><span class="num">${session.archivist_host_id}</span><span class="num">${session.cli_id}</span>
        ${signal === "online" ? html`<${Fine} text=${text} kind="online" />` : html`<span class="ok" data-signal=${signal}><${StatusIcon} kind=${signal} />${text}</span>`}
        ${onRemove ? html`<button type="button" class="btn btn-sm" data-action="archivist-remove" disabled=${Boolean(removing)} onClick=${() => onRemove(session)}>${removing === session.session_id ? "제거 중" : "제거"}</button>` : null}
      </div>`;
    }) : html`<${Empty} text="Agent 없음" />`}
  </div>`;
}

export function HostTable({ hosts, focus = "" }) {
  return html`<div class="table hosts-table" role="table" id="host-table">
    <div class="trow head" role="row"><span role="columnheader">Agent Host</span><span role="columnheader">상태</span><span role="columnheader">감지된 CLI</span><span role="columnheader">heartbeat</span></div>
    ${hosts.length ? hosts.map((host) => {
      const signal = stateSignal(host.state);
      return html`<div class="trow" role="row" key=${host.archivist_host_id} data-host-id=${host.archivist_host_id} data-focus=${focus === host.archivist_host_id ? "true" : null}>
        <b class="num">${host.archivist_host_id}</b>
        ${signal === "fault" ? html`<span class="ok"><${StatusIcon} kind="fault" />${label("host", host.state)}</span>` : html`<${Fine} text=${label("host", host.state)} kind=${signal === "work" ? "work" : "online"} />`}
        <span class="num">${(host.detected_cli_ids || []).join(" · ") || "감지된 CLI 없음"}</span>
        <span class="num muted">${clockTime(host.last_heartbeat, true)}</span>
      </div>`;
    }) : html`<${Empty} text="Agent Host 없음" />`}
  </div>`;
}
