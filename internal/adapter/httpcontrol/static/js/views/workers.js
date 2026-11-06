// 연결 관리: Worker PC, logical drives and Agent/Host connections in one
// operational screen. Registration and credentials remain local-config work;
// this screen shows live state and links to the owning detail.

import {
  el, statusIcon, workerIcon, label, stateSignal, formatBytes, formatPercent, formatCount, clockTime, meter, emptyState,
} from "../core.js";
import {
  model, drivePhase, driveName, driveProgress, driveProblemCounts, PHASE_LABEL, PHASE_ICON, hostFor, activeTurn,
} from "../store.js";

function admissionLabel(admission) {
  if (admission?.available) return "추가 가능";
  if (admission?.wait_reason) return `보류 · ${label("wait", admission.wait_reason)}`;
  return `보류 · ${label("admission", admission?.code)}`;
}

function capacityItem(name, value, detail = "", signal = "idle", hover = "") {
  return el("div", { class: "capacity-item", title: hover || undefined, dataset: { signal } },
    el("span", {}, name), el("strong", { class: "num" }, value), detail ? el("small", {}, detail) : null);
}

function capacity(worker) {
  const heavy = worker.heavy_admission || {};
  const light = worker.light_admission || {};
  return el("div", { class: "capacity-grid", "aria-label": "자원" },
    capacityItem("CPU", formatPercent(worker.cpu_average_5m)),
    capacityItem("GPU", formatPercent(worker.gpu_average_5m)),
    capacityItem("메모리", formatBytes(worker.capacity_available_memory_bytes), "", "idle", `예약 ${formatBytes(worker.reserved_memory_bytes)} 차감`),
    capacityItem("작업 공간", formatBytes(worker.capacity_scratch_free_bytes), "", "idle", `예약 ${formatBytes(worker.reserved_scratch_bytes)} 차감`),
    capacityItem("HEAVY", `${worker.heavy_slots_used || 0}/${worker.heavy_slots_maximum || 0}`, admissionLabel(heavy), heavy.available ? "done" : "decide"),
    capacityItem("LIGHT", `${worker.light_slots_used || 0}/${worker.light_slots_maximum || 0}`, admissionLabel(light), light.available ? "done" : "decide"),
    capacityItem("관측", clockTime(worker.observed_at || worker.last_heartbeat, true), worker.additional_work_possible ? "수용 가능" : "보류", worker.additional_work_possible ? "done" : "decide"));
}

function table(kind, heads, rows, empty) {
  return el("div", { class: `data-table ${kind}`, role: "table" },
    el("div", { class: "data-row data-head", role: "row" }, ...heads.map((head) => el("span", { role: "columnheader" }, head))),
    ...(rows.length ? rows : [el("p", { class: "empty" }, empty)]));
}

function storageRows(worker) {
  const seen = new Map();
  for (const drive of model.drives.filter((item) => item.worker_id === worker.worker_id)) {
    for (const storage of drive.storage_candidates || []) {
      if (storage.role === "ARCHIVE_PRIMARY" && !seen.has(storage.storage_id)) seen.set(storage.storage_id, storage);
    }
  }
  const offline = stateSignal(worker.state) === "fault";
  return [...seen.values()].sort((left, right) => left.storage_id.localeCompare(right.storage_id)).map((storage) =>
    el("div", { class: "data-row", role: "row", dataset: { storageId: storage.storage_id } },
      el("b", { class: "cell-main" }, storage.label || storage.storage_id),
      el("span", {}, label("storageAccess", storage.access_kind)),
      el("span", { class: offline || !storage.accessible ? "muted" : "" }, offline ? "확인 불가" : storage.accessible ? "접근 가능" : "접근 안 됨"),
      el("span", { class: "num" }, !offline && storage.accessible && storage.capacity_bytes ? `${formatBytes(storage.free_bytes)} / ${formatBytes(storage.capacity_bytes)}` : "—")));
}

function workerCard(worker, focusWorker) {
  const signal = stateSignal(worker.state);
  const offline = signal === "fault";
  return el("section", { class: "worker-row", "aria-label": worker.worker_id, dataset: { workerId: worker.worker_id, signal: offline ? "fault" : undefined, focus: String(focusWorker === worker.worker_id) } },
    el("div", { class: "worker-head", title: `heartbeat ${clockTime(worker.last_heartbeat, true)}` },
      workerIcon(worker.state), el("b", { class: "worker-name" }, worker.worker_id),
      el("span", { class: "state", dataset: { signal } }, label("worker", worker.state)),
      offline ? el("span", { class: "worker-seen num" }, `마지막 응답 ${clockTime(worker.last_heartbeat)}`) : null),
    el("div", { class: "worker-body" },
      table("storage-table", ["아카이브 저장소", "접근 방식", "접근", "여유 / 용량"], storageRows(worker), "저장소 없음"),
      capacity(worker)));
}

function driveRow(drive) {
  const phase = drivePhase(drive);
  const progress = driveProgress(drive);
  const { fault: faults, decide: decisions } = driveProblemCounts(drive);
  const issues = [faults ? `실패 ${faults}` : "", decisions ? `확인 ${decisions}` : ""].filter(Boolean).join(" · ");
  return el("a", { class: "data-row", role: "row", href: `#/drives/${encodeURIComponent(drive.workspace_drive_id)}`, dataset: { driveId: drive.workspace_drive_id } },
    el("b", { class: "cell-main" }, driveName(drive)),
    el("span", { class: "num" }, drive.worker_id || "배정 없음"),
    el("span", { class: "cell-state" }, statusIcon(PHASE_ICON[phase]), PHASE_LABEL[phase]),
    phase === "intake" ? el("span", { class: "muted" }, "—")
      : el("span", { class: "cell-meter" }, meter(progress.pct), el("span", { class: "num" }, `${progress.pct}% · ${formatCount(progress.stored)} / ${formatCount(progress.total)}`)),
    el("span", {}, drive.archive_storage_id || "—"),
    el("span", { class: "cell-issue", dataset: { signal: faults ? "fault" : decisions ? "decide" : undefined } }, issues || "—"));
}

function availability(session) {
  const host = hostFor(session);
  if (host?.state !== "ONLINE") return ["Host 꺼짐", "fault"];
  if (activeTurn(session)) return ["처리 중", "work"];
  return ["사용 가능", "online"];
}

function agentRow(session) {
  const [text, signal] = availability(session);
  return el("div", { class: "data-row", role: "row", dataset: { sessionId: session.session_id } },
    el("b", { class: "cell-main" }, session.name || session.session_id),
    el("span", { class: "num" }, session.archivist_host_id),
    el("span", { class: "num" }, session.cli_id),
    el("span", {}, label("mode", session.control_mode || "ASSISTED")),
    el("span", { class: "cell-state", dataset: { signal } }, statusIcon(signal), text));
}

function hostRow(host, focusHost) {
  const signal = stateSignal(host.state);
  return el("div", { class: "data-row", role: "row", dataset: { hostId: host.archivist_host_id, focus: String(focusHost === host.archivist_host_id) } },
    el("b", { class: "cell-main num" }, host.archivist_host_id),
    el("span", { class: "cell-state", dataset: { signal } }, workerIcon(host.state), label("host", host.state)),
    el("span", { class: "num" }, (host.detected_cli_ids || []).join(" · ") || "감지된 CLI 없음"),
    el("span", { class: "num muted" }, clockTime(host.last_heartbeat, true)));
}

export function mount(root, route) {
  const focusWorker = route.query.get("worker") || "";
  const focusHost = route.query.get("host") || "";
  let scrolled = false;
  const heading = el("h1", { id: "connections-title" }, "연결 관리");
  const workers = el("div", { class: "worker-list", id: "worker-list" });
  const drives = el("div", { id: "connection-drive-list" });
  const agents = el("div", { id: "connection-agent-list" });
  const hosts = el("div", { id: "connection-host-list" });
  root.append(
    el("div", { class: "page-head" }, el("div", { class: "page-title" }, heading),
      el("a", { class: "btn", href: "#/settings" }, "Agent 추가")),
    el("div", { class: "connection-sections" },
      el("section", { class: "section", id: "connection-workers" },
        el("h2", { class: "section-title" }, "Worker PC ", el("span", { class: "num muted", id: "connection-worker-count" })), workers),
      el("section", { class: "section", id: "connection-drives" },
        el("h2", { class: "section-title" }, "드라이브 ", el("span", { class: "num muted", id: "connection-drive-count" })), drives),
      el("section", { class: "section", id: "connection-agents" },
        el("h2", { class: "section-title" }, "Agent ", el("span", { class: "num muted", id: "connection-agent-count" })), agents,
        el("h3", { class: "connection-subtitle" }, "Agent Host"), hosts)));

  return {
    update() {
      const online = model.workers.filter((worker) => stateSignal(worker.state) !== "fault").length;
      heading.title = `Worker PC ${model.workers.length} · 연결 ${online} · 드라이브 ${model.drives.length} · Agent ${model.sessions.length}`;
      root.querySelector("#connection-worker-count").textContent = String(model.workers.length);
      root.querySelector("#connection-drive-count").textContent = String(model.drives.length);
      root.querySelector("#connection-agent-count").textContent = String(model.sessions.length);
      workers.replaceChildren(...(model.workers.length ? model.workers.map((worker) => workerCard(worker, focusWorker)) : [emptyState("Worker PC 없음")]));
      drives.replaceChildren(table("connection-drive-table", ["드라이브", "Worker", "단계", "보관 진행", "아카이브", "확인 필요"], model.drives.map(driveRow), "드라이브 없음"));
      agents.replaceChildren(table("connection-agent-table", ["Agent", "Agent Host", "CLI", "제어 모드", "상태"], model.sessions.map(agentRow), "Agent 없음"));
      hosts.replaceChildren(table("connection-host-table", ["Agent Host", "상태", "감지된 CLI", "heartbeat"], model.hosts.map((host) => hostRow(host, focusHost)), "Agent Host 없음"));
      if (!scrolled && (focusWorker || focusHost)) {
        const selector = focusWorker ? `[data-worker-id="${CSS.escape(focusWorker)}"]` : `[data-host-id="${CSS.escape(focusHost)}"]`;
        const target = root.querySelector(selector);
        if (target) { target.scrollIntoView({ block: "nearest" }); scrolled = true; }
      }
    },
  };
}
