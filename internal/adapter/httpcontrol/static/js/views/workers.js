// Worker: what each Worker processes now — drives as the unit of work and Jobs
// that belong to no drive — with its archive storage access and the
// reservation-adjusted capacity. Registration stays in local configuration.

import {
  el, token, statusIcon, workerIcon, label, stateSignal, stageSignal, finishedSteps, formatBytes, formatPercent, formatCount, shortTime, clockTime, meter,
  api, mutation, newID, emptyState,
} from "../core.js";
import { model, ui, notify, refresh, drivePhase, driveName, driveProgress, driveProblemCounts, driveFreeJobs, jobLane, PHASE_LABEL, PHASE_ICON } from "../store.js";

const LANE = { fault: ["실패", "fault"], decide: ["사람 대기", "decide"], work: ["실행 중", "work"], resource: ["자원 대기", "resource"], queue: ["대기열", "idle"] };

function admissionLabel(admission) {
  if (admission?.available) return "추가 가능";
  if (admission?.wait_reason) return `보류 · ${label("wait", admission.wait_reason)}`;
  return `보류 · ${label("admission", admission?.code)}`;
}

// capacityItem keeps the value and its admission on the page; a reservation
// amount behind a value waits on hover.
function capacityItem(name, value, detail = "", signal = "idle", hover = "") {
  return el("div", { class: "capacity-item", title: hover || undefined, dataset: { signal } }, el("span", {}, name), el("strong", { class: "num" }, value), detail ? el("small", {}, detail) : null);
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

// table lays one item per row under fixed column heads so no cell wraps; a
// cell cut short on a narrow screen keeps its full text as its title.
function table(kind, heads, rows, empty) {
  for (const row of rows) {
    if (!row.classList.contains("data-row")) continue;
    for (const cell of row.children) if (cell.textContent && !cell.title) cell.title = cell.textContent;
  }
  return el("div", { class: `data-table ${kind}`, role: "table" },
    el("div", { class: "data-row data-head", role: "row" }, ...heads.map((head) => el("span", { role: "columnheader" }, head))),
    ...(rows.length ? rows : [el("p", { class: "empty" }, empty)]));
}

function driveRow(drive) {
  const phase = drivePhase(drive);
  const progress = driveProgress(drive);
  const { fault: faults, decide: decisions } = driveProblemCounts(drive);
  const issues = [faults ? `실패 ${faults}` : "", decisions ? `확인 ${decisions}` : ""].filter(Boolean).join(" · ");
  return el("a", { class: "data-row", role: "row", href: `#/drives/${encodeURIComponent(drive.workspace_drive_id)}`, dataset: { driveId: drive.workspace_drive_id } },
    el("b", { class: "cell-main" }, driveName(drive)),
    el("span", { class: "cell-state" }, statusIcon(PHASE_ICON[phase]), PHASE_LABEL[phase]),
    phase === "intake" ? el("span", { class: "muted" }, "—")
      : el("span", { class: "cell-meter" }, meter(progress.pct), el("span", { class: "num" }, `${progress.pct}% · ${formatCount(progress.stored)} / ${formatCount(progress.total)}`)),
    el("span", {}, drive.archive_storage_id || "—"),
    el("span", { class: "cell-issue", dataset: { signal: faults ? "fault" : decisions ? "decide" : undefined } }, issues || "—"));
}

// storageRows lists the archive storages this Worker's drives can use, as
// reported by the Worker; an offline Worker's access cannot be confirmed.
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

function laneText(job) {
  const lane = jobLane(job);
  if (lane === "decide") return job.wait_reason ? `${label("wait", job.wait_reason)} 대기` : "사람 대기";
  if (lane === "work" || lane === "resource") return `${Math.round((job.progress || 0) * 100)}%`;
  return LANE[lane]?.[0] || label("step", job.step_state);
}

function stepRow(step) {
  const signal = stageSignal({ state: step.state, wait_reason: step.wait_reason });
  const detail = [
    step.state === "WAITING" && step.wait_reason ? `WAITING · ${step.wait_reason}` : step.state,
    step.progress ? formatPercent(Math.round(step.progress * 100)) : "",
    step.worker_id, step.error_class, step.log_reference ? `로그 ${step.log_reference}` : "",
  ].filter(Boolean).join(" · ");
  return el("div", { class: "stage-row", dataset: { step: step.id, signal } }, el("i", { class: "step-dot", dataset: { signal } }), el("b", {}, step.id), el("span", { class: "stage-state" }, detail));
}

export function mount(root, route) {
  let openJob = route.query.get("job") || "";
  const focusWorker = route.query.get("worker") || "";
  ui.jobSteps = false;
  ui.jobCancel = "";
  ui.status = "";
  let detail = null;
  let loadedKey = "";
  let scrolled = false;
  const heading = el("h1", { id: "workers-title" }, "Worker");
  const list = el("div", { class: "worker-list", id: "worker-list" });
  const others = el("div", {});
  root.append(el("div", { class: "page-head" }, el("div", { class: "page-title" }, heading)), list, others);

  async function cancel(job) {
    try {
      await mutation(`/api/v1/jobs/${encodeURIComponent(job.job_id)}/cancel`, { command_id: newID("COMMAND"), idempotency_key: newID("CANCEL"), reason: "담당자 취소", expected_revision: job.revision });
      ui.jobCancel = "";
      ui.status = `${job.job_id} 취소 요청`;
      loadedKey = "";
      await refresh();
    } catch (error) {
      ui.status = error.message;
      notify();
    }
  }

  function jobDetail(job) {
    const summaryView = detail?.summary?.job_id === job.job_id ? detail.summary : job;
    const lane = jobLane(summaryView);
    const facts = [
      ["Template", summaryView.template_name], ["제어 모드", summaryView.control_mode], ["리비전", `r${summaryView.revision}`],
      ["Worker", summaryView.worker_id || "배정 없음"], ["시작 주체", summaryView.actor_id || "—"],
      ["사람 개입", summaryView.needs_human_intervention ? summaryView.intervention_reason || "필요" : "없음"], ["최근", shortTime(summaryView.last_event_at)],
    ];
    const steps = detail?.summary?.job_id === job.job_id ? detail.steps || [] : [];
    const cancellable = !finishedSteps.has(summaryView.step_state);
    const confirm = ui.jobCancel === job.job_id;
    return el("div", { class: "job-detail", id: "job-detail", dataset: { jobId: job.job_id } },
      el("div", { class: "detail-head" }, el("b", { title: facts.map(([key, value]) => `${key} ${value}`).join("\n") }, summaryView.target_title || job.job_id),
        token(`${LANE[lane]?.[0] || label("step", summaryView.step_state)} · ${summaryView.step_state === "WAITING" && summaryView.wait_reason ? summaryView.wait_reason : summaryView.step_state}`, LANE[lane]?.[1] || stateSignal(summaryView.step_state))),
      el("div", { class: "detail-actions" },
        el("button", { type: "button", class: "btn btn-sm steps-toggle", "aria-expanded": String(ui.jobSteps), onclick: () => { ui.jobSteps = !ui.jobSteps; notify(); } }, ui.jobSteps ? "세부 단계 숨기기" : "세부 단계 보기"),
        cancellable && !confirm ? el("button", { type: "button", class: "btn btn-sm danger", id: "cancel-job", dataset: { action: "cancel" }, onclick: () => { ui.jobCancel = job.job_id; notify(); } }, "작업 취소…") : null),
      ui.jobSteps ? el("div", { class: "stage-list" }, ...(steps.length ? steps.map(stepRow) : [emptyState("단계 없음")])) : null,
      confirm ? el("div", { class: "confirm-box", title: "대기 단계 즉시 취소 · 실행 중 단계는 정지·정리 확인 뒤 취소" },
        el("span", {}, "작업 취소 확인"),
        el("div", { class: "detail-actions" },
          el("button", { type: "button", class: "btn danger-solid", id: "cancel-job-confirm", dataset: { action: "cancel-confirm" }, onclick: () => cancel(summaryView) }, "취소 확정"),
          el("button", { type: "button", class: "btn", onclick: () => { ui.jobCancel = ""; notify(); } }, "돌아가기"))) : null);
  }

  function jobRow(job, showWorker) {
    const open = openJob === job.job_id;
    const lane = jobLane(job);
    const pct = Math.round((job.progress || 0) * 100);
    const row = el("button", { type: "button", class: "data-row job-row", role: "row", "aria-expanded": String(open), dataset: { jobId: job.job_id, lane },
      onclick: () => { openJob = open ? "" : job.job_id; ui.jobSteps = false; ui.jobCancel = ""; loadedKey = ""; notify(); } },
    el("b", { class: "cell-main" }, job.target_title || job.job_id),
    el("span", {}, job.template_name || "—"),
    showWorker ? el("span", {}, job.worker_id || "배정 없음") : null,
    el("span", { class: "cell-state" }, statusIcon(lane === "queue" ? "idle" : lane || "done"),
      el("span", { class: "job-state", dataset: { signal: lane === "fault" || lane === "decide" ? lane : undefined } }, lane === "decide" ? laneText(job) : LANE[lane]?.[0] || label("step", job.step_state))),
    el("span", { class: "cell-meter" }, meter(pct, false), el("span", { class: "num job-progress" }, `${pct}%`)));
    return open ? [row, jobDetail(job)] : [row];
  }

  function jobTable(jobs, showWorker) {
    const heads = showWorker ? ["작업", "Template", "Worker", "상태", "진행"] : ["드라이브 밖 작업", "Template", "상태", "진행"];
    return table(showWorker ? "job-table with-worker" : "job-table", heads, jobs.flatMap((job) => jobRow(job, showWorker)), "없음");
  }

  function workerRow(worker, freeJobs) {
    const signal = stateSignal(worker.state);
    const mine = model.drives.filter((drive) => drive.worker_id === worker.worker_id);
    const jobs = freeJobs.filter((job) => job.worker_id === worker.worker_id);
    const offline = signal === "fault";
    return el("section", { class: "worker-row", "aria-label": worker.worker_id, dataset: { workerId: worker.worker_id, signal: offline ? "fault" : undefined, focus: String(focusWorker === worker.worker_id) } },
      el("div", { class: "worker-head", title: `heartbeat ${clockTime(worker.last_heartbeat, true)}` },
        workerIcon(worker.state),
        el("b", { class: "worker-name" }, worker.worker_id),
        el("span", { class: "state", dataset: { signal } }, label("worker", worker.state)),
        offline ? el("span", { class: "worker-seen num" }, `마지막 응답 ${clockTime(worker.last_heartbeat)}`) : null),
      el("div", { class: "worker-body" },
        table("drive-table", ["드라이브", "단계", "보관 진행", "아카이브", "확인 필요"], mine.map(driveRow), "연결된 드라이브 없음"),
        jobs.length ? jobTable(jobs, false) : null,
        table("storage-table", ["저장소", "접근 방식", "접근", "여유 / 용량"], storageRows(worker), "없음"),
        capacity(worker)));
  }

  return {
    async update() {
      const freeJobs = driveFreeJobs();
      const online = model.workers.filter((worker) => stateSignal(worker.state) !== "fault").length;
      heading.title = `Worker ${model.workers.length} · 연결 ${online} · 꺼짐 ${model.workers.length - online} · 드라이브 밖 작업 ${freeJobs.length}`;
      if (openJob && !model.jobs.some((job) => job.job_id === openJob)) openJob = "";
      const job = model.jobs.find((item) => item.job_id === openJob);
      const key = job ? `${job.job_id}@${job.revision}@${job.last_event_at}` : "";
      if (key !== loadedKey) {
        loadedKey = key;
        try {
          detail = job ? await api(`/api/v1/jobs/${encodeURIComponent(job.job_id)}`) : null;
        } catch {
          detail = null;
        }
      }
      list.replaceChildren(...(model.workers.length ? model.workers.map((worker) => workerRow(worker, freeJobs)) : [emptyState("Worker 없음")]));
      const known = new Set(model.workers.map((worker) => worker.worker_id));
      const rest = freeJobs.filter((item) => !known.has(item.worker_id));
      others.replaceChildren(...(rest.length ? [el("section", { class: "section", id: "other-jobs", "aria-label": "그 밖의 작업" },
        el("h2", { class: "section-title" }, "그 밖의 작업 ", el("span", { class: "num muted" }, String(rest.length))),
        jobTable(rest, true))] : []));
      if (ui.status) others.append(el("p", { class: "status-line", role: "status", id: "command-status" }, ui.status));
      if (!scrolled && (focusWorker || openJob)) {
        const target = root.querySelector(openJob ? `[data-job-id="${CSS.escape(openJob)}"]` : `[data-worker-id="${CSS.escape(focusWorker)}"]`);
        if (target) { target.scrollIntoView({ block: "nearest" }); scrolled = true; }
      }
    },
  };
}
