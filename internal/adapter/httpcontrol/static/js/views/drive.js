// 드라이브 상세: one source drive as the unit of work. Its overview card holds
// the phase, quiet video progress, Worker, archive and problem counts over the
// stage status matrix of its ArchiveRun; the STT gate and run preparation sit beside the
// title. Below come the problems a person resolves here and a table with one
// row per folder; a folder's steps open only on request.

import {
  el, svgIcon, icons, statusIcon, label, stageSignal, activeStates, formatCount, formatBytes,
  shortTime, api, mutation, newID, emptyState,
} from "../core.js";
import {
  model, ui, notify, refresh, findDrive, driveName, folderProblem, folderState, folderSignal, isRunning, preparing,
  prepareDrive, syncPreparation, discoveredFolders, archiveCandidates, folderDiscoveryReady, folderDiscoveryText, startBlocker,
} from "../store.js";
import { driveFacts } from "../overview.js";
import { stageMatrix } from "../flow.js";

const WORK_CHOICE_LABELS = {
  "RENDER_RECORDINGS:6000": ["render-6000", "개별 렌더 6M"],
  "RENDER_RECORDINGS:20000": ["render-20000", "개별 렌더 20M"],
  "COMPILE_SOURCE_SEQUENCE:0": ["source", "소스 시퀀스"],
  "ORGANIZE_AS_IS:0": ["as-is", "그대로 보관"],
};
const DATE = /(\d{4})-(\d{2})-(\d{2})/;
const MONTH_DAYS = (year, month) => new Date(Date.UTC(year, month, 0)).getUTCDate();
const RANK = { fault: 4, decide: 3, work: 2, idle: 1, done: 0 };

let jobDetails = new Map();

// loadJob reads a failed folder's Job once so its stored error class and log
// reference can sit next to the failure.
function loadJob(jobID) {
  if (!jobID || jobDetails.has(jobID)) return;
  jobDetails.set(jobID, null);
  api(`/api/v1/jobs/${encodeURIComponent(jobID)}`).then((detail) => { jobDetails.set(jobID, detail); notify(); }).catch(() => {});
}

function stageProgress(stage) {
  return stage.total_count > 0 && (stage.completed_count > 0 || activeStates.has(stage.state)) ? `${formatCount(stage.completed_count)}/${formatCount(stage.total_count)}` : "";
}

function stageLine(stage) {
  const parts = [stageProgress(stage)];
  if (stage.state === "WAITING") parts.push(stage.wait_reason ? `${label("wait", stage.wait_reason)} 대기` : "대기", stage.wait_target_storage_id);
  else parts.push(label("step", stage.state));
  return parts.filter(Boolean).join(" · ");
}

function stageRow(stage) {
  const signal = stageSignal(stage);
  return el("div", { class: "stage-row", dataset: { stage: stage.stage_id, signal, waitReason: stage.wait_reason || undefined } },
    el("i", { class: "step-dot", dataset: { signal } }),
    el("b", {}, label("stage", stage.stage_id)),
    el("span", { class: "stage-state" }, stageLine(stage)));
}

function archiveBadge(state) {
  return el("b", { class: "archive-badge", dataset: { state } }, state === "STORED" ? "보관됨" : "확정됨");
}

function sttButton(drive) {
  const button = el("button", { type: "button", class: "btn primary", id: "start-stt", dataset: { action: "stt" } });
  button.disabled = drive.archive_run_state !== "TRANSCRIPTION_READY" || !drive.archive_run_revision;
  if (!button.disabled) button.dataset.signal = "decide";
  button.textContent = drive.stage_two_remaining ? "STT 진행 중"
    : drive.archive_run_state === "TRANSCRIPTION_READY" ? `STT 시작 · ${formatCount(drive.eligible_transcription_count)} 폴더`
      : drive.stage_one_remaining ? `STT · 1단계 ${drive.stage_one_remaining} 폴더 남음`
        : drive.archive_run_state === "COMPLETED" && !drive.eligible_transcription_count ? "STT 대상 없음"
          : drive.archive_run_state === "COMPLETED" ? "STT 완료" : "STT";
  button.addEventListener("click", async () => {
    try {
      const result = await mutation(`/api/v1/archive-runs/${encodeURIComponent(drive.archive_run_id)}/transcription`, { idempotency_key: newID("STT"), expected_archive_run_revision: drive.archive_run_revision });
      ui.status = `STT ${result.eligible_count} 폴더`;
      await refresh();
    } catch (error) {
      ui.status = error.message;
      notify();
    }
  });
  return button;
}

// overview is the drive's card: five facts and the stage matrix of its ArchiveRun.
function overview(drive) {
  return el("section", { class: "overview", id: "drive-overview", "aria-label": "진행" }, driveFacts(drive, "drive-progress"), stageMatrix(drive));
}

function transcriptQualityPanel(folder) {
  const reviews = (folder.transcript_qualities || []).filter((quality) => quality.state === "REVIEW_RECOMMENDED");
  return el("div", { class: "transcript-quality" },
    ...reviews.map((quality) => el("div", { class: "quality-row", dataset: { transcriptId: quality.transcript_id } },
      el("code", {}, quality.transcript_id),
      el("span", { class: "quality-reasons" }, (quality.reasons || []).map((reason) => label("transcriptQuality", reason)).join(" · ")),
      el("span", { class: "quality-metrics num" }, `${(quality.duration_millis / 1000).toFixed(1)}초 · ${formatCount(quality.segment_count)}구간 · ${formatCount(quality.text_character_count)}자`))));
}

function holdDecision(drive, folder) {
  const available = (folder.work_options || []).map((option) => {
    const target = option.target_video_bitrate_kbps || 0;
    const label = WORK_CHOICE_LABELS[`${option.disposition}:${target}`];
    return label ? [label[0], label[1], option.disposition, target] : null;
  }).filter(Boolean);
  const choices = el("div", { class: "seg hold-choices", role: "group", "aria-label": "작업 선택" });
  const submit = el("button", { type: "button", class: "btn primary", id: "hold-submit", dataset: { action: "hold" } }, "작업 선택");
  const chosen = ui.hold?.key === folder.folder ? ui.hold.choice : "";
  submit.disabled = !chosen || !available.some(([value]) => value === chosen);
  for (const [value, text] of available) {
    choices.append(el("button", { type: "button", "aria-pressed": String(chosen === value), dataset: { choice: value }, onclick: () => { ui.hold = { key: folder.folder, choice: value }; notify(); } }, text));
  }
  submit.addEventListener("click", async () => {
    const option = available.find(([value]) => value === ui.hold?.choice);
    if (!option) return;
    try {
      await mutation(`/api/v1/archive-intake/${encodeURIComponent(drive.workspace_drive_id)}/override`, {
        command_id: newID("COMMAND"), decision_id: newID("DECISION"), idempotency_key: newID("OVERRIDE"),
        archive_run_id: drive.archive_run_id, folder: folder.folder,
        expected_revision: folder.input_revision, disposition: option[2], target_video_bitrate_kbps: option[3],
      });
      ui.hold = null;
      ui.status = `${folder.folder} · ${option[1]}`;
      await refresh();
    } catch (error) {
      ui.status = error.message;
      notify();
    }
  });
  return el("div", { class: "problem-body-block" },
    el("span", { class: "hold-reasons" }, `보류 사유 · ${(folder.hold_reasons || []).map((reason) => label("hold", reason)).join(" · ") || "—"}`),
    available.length ? choices : el("span", { class: "muted" }, "선택 가능한 작업 없음"),
    el("div", { class: "detail-actions" }, submit));
}

function problemBody(drive, folder, problem) {
  const stage = problem.stage;
  const count = stage?.total_count ? ` · ${formatCount(stage.completed_count)} / ${formatCount(stage.total_count)} 영상` : "";
  if (problem.kind === "hold") return holdDecision(drive, folder);
  if (problem.kind === "review") return transcriptQualityPanel(folder);
  if (problem.kind === "fault") {
    loadJob(folder.job_id);
    const failedStep = (jobDetails.get(folder.job_id)?.steps || []).find((step) => step.state === "FAILED");
    const hover = [count.replace(/^ · /, ""), folder.job_id && `작업 ${folder.job_id}`, failedStep?.error_class && `오류 종류 ${failedStep.error_class}`, failedStep?.log_reference && `로그 ${failedStep.log_reference}`];
    return el("div", { class: "problem-body-block" },
      el("span", { title: hover.filter(Boolean).join("\n") }, `${label("stage", stage.stage_id)} 단계 실패`));
  }
  if (problem.kind === "device") {
    return el("div", { class: "problem-body-block" },
      el("span", { title: `${label("stage", stage.stage_id)} 단계 대기${count}` }, `${problem.target || "아카이브 저장소"} 연결 시 자동 재개`),
      el("a", { class: "link", href: `#/workers?worker=${encodeURIComponent(drive.worker_id)}` }, `${drive.worker_id} 저장소 접근`));
  }
  return el("div", { class: "problem-body-block" }, el("span", {}, `${label("stage", stage.stage_id)} · ${label("wait", stage.wait_reason)} 대기${count}`));
}

function problemPanel(drive, folder, problem) {
  const open = ui.openFolder === folder.folder;
  const panel = el("section", { class: "problem-panel", "aria-label": `${problem.title} ${folder.folder}`, dataset: { folderId: folder.folder, kind: problem.kind, signal: problem.signal, open: String(open) } },
    el("button", { type: "button", class: "problem-head", "aria-expanded": String(open), onclick: () => { ui.openFolder = open ? "" : folder.folder; notify(); } },
      statusIcon(problem.signal),
      el("span", { class: "problem-line" }, el("b", { class: "problem-title" }, problem.title), el("b", {}, folder.folder)),
      el("span", { class: "problem-videos num" }, `영상 ${formatCount(folder.video_count)}`),
      svgIcon(icons.expand)));
  if (open) panel.append(problemBody(drive, folder, problem));
  return panel;
}

function folderDetail(folder) {
  const showSteps = ui.openSteps === folder.folder;
  const notes = [];
  const archiveResult = folder.archive_date && folder.archive_video_file_name
    ? `${folder.archive_date} · ${folder.archive_video_file_name}`
    : "";
  const trace = [folder.archive_display_name, folder.archive_package_id, folder.job_id].filter(Boolean).join("\n");
  if (folder.archive_state) notes.push(archiveBadge(folder.archive_state));
  if (folder.can_add_transcription) notes.push(el("span", { class: "folder-note" }, "STT 추가 가능"));
  if (folder.transcription_job_id && folder.step_state === "COMPLETED") notes.push(el("span", { class: "folder-note" }, "STT 완료"));
  return el("div", { class: "folder-detail", id: "folder-detail", dataset: { folderId: folder.folder } },
    el("span", { class: "detail-meta", title: [trace, `${formatCount(folder.video_count)} 영상`, formatBytes(folder.estimated_output_bytes)].filter(Boolean).join("\n") }, archiveResult || (folder.job_id ? `작업 ${folder.job_id}` : "작업 없음")),
    notes.length ? el("div", { class: "detail-notes" }, ...notes) : null,
    el("div", { class: "detail-actions" },
      el("button", { type: "button", class: "btn btn-sm steps-toggle", "aria-expanded": String(showSteps), onclick: () => { ui.openSteps = showSteps ? "" : folder.folder; notify(); } },
        showSteps ? "세부 단계 숨기기" : "세부 단계 보기")),
    showSteps ? el("div", { class: "stage-list" }, ...((folder.stages || []).length ? folder.stages.map(stageRow) : [emptyState("단계 없음")])) : null);
}

// folderRow is one table row: the folder's signal and name, one state word
// with its stored or committed badge, disposition, videos and step dots.
function folderRow(folder) {
  const open = ui.openFolder === folder.folder;
  const problem = folderProblem(folder);
  const state = folderState(folder);
  const signal = folderSignal(folder);
  const stages = folder.stages || [];
  const word = problem
    ? el("span", { class: "status-text", dataset: { signal: problem.signal, waitReason: problem.reason || undefined } }, problem.word)
    : folder.archive_state ? null : el("span", { class: "folder-word", dataset: { signal: state.signal } }, state.word);
  const row = el("button", { type: "button", class: "folder-row", "aria-expanded": String(open), title: `${label("disposition", folder.disposition)} · ${formatCount(folder.video_count)} 영상`, dataset: { folderId: folder.folder, signal },
    onclick: () => { ui.openFolder = open ? "" : folder.folder; ui.openSteps = ""; notify(); } },
  el("span", { class: "folder-name" }, statusIcon(signal), el("b", {}, folder.folder)),
  el("span", { class: "folder-state" }, word, folder.archive_state ? archiveBadge(folder.archive_state) : null),
  el("span", { class: "folder-disp" }, label("disposition", folder.disposition)),
  el("span", { class: "folder-videos num" }, formatCount(folder.video_count)),
  el("span", { class: "step-dots", "aria-hidden": "true" }, ...stages.map((stage) => el("i", { class: "step-dot", title: `${label("stage", stage.stage_id)} · ${label("step", stage.state)}`, dataset: { signal: stageSignal(stage) } }))),
  svgIcon(icons.expand));
  return open ? [row, folderDetail(folder)] : [row];
}

function calendar(folders) {
  const byDate = new Map();
  for (const folder of folders) {
    const match = folder.folder.match(DATE);
    if (!match) continue;
    const key = `${match[1]}-${match[2]}-${match[3]}`;
    byDate.set(key, [...(byDate.get(key) || []), folder]);
  }
  const years = [...new Set([...byDate.keys()].map((key) => Number(key.slice(0, 4))))].sort();
  const grids = years.map((year) => {
    const months = [];
    for (let month = 1; month <= 12; month += 1) {
      const cells = [];
      let stored = 0;
      let present = 0;
      for (let day = 1; day <= 31; day += 1) {
        if (day > MONTH_DAYS(year, month)) { cells.push(el("span", { class: "cal-cell", dataset: { state: "none" } })); continue; }
        const key = `${year}-${String(month).padStart(2, "0")}-${String(day).padStart(2, "0")}`;
        const items = byDate.get(key) || [];
        if (!items.length) { cells.push(el("span", { class: "cal-cell", dataset: { state: "empty" }, title: key })); continue; }
        present += 1;
        if (items.every((folder) => folder.archive_state)) stored += 1;
        const signal = items.map(folderSignal).sort((left, right) => RANK[right] - RANK[left])[0];
        const pressed = items.some((folder) => folder.folder === ui.openFolder);
        cells.push(el("button", { type: "button", class: "cal-cell", "aria-pressed": String(pressed), dataset: { signal, date: key }, title: `${key} · ${items.length} 폴더`, "aria-label": `${key} · ${items.length} 폴더`,
          onclick: () => { ui.openFolder = items[0].folder; ui.folderView = "list"; notify(); } }));
      }
      if (present) months.push(el("div", { class: "cal-row" }, el("span", { class: "cal-month" }, `${month}월`), ...cells, el("span", { class: "cal-sum num" }, `${stored} / ${present}`)));
    }
    return el("div", { class: "calendar", role: "group", "aria-label": `${year} 폴더 달력` }, el("div", { class: "cal-year" }, `${year}`), ...months);
  });
  return el("div", { class: "calendar-wrap" }, ...grids);
}

function folderSection(drive) {
  const folders = drive.folders || [];
  const dated = folders.filter((folder) => DATE.test(folder.folder)).length;
  const canCalendar = dated > 0 && dated >= folders.length * 0.6;
  const view = canCalendar ? (ui.folderView || "calendar") : "list";
  const rows = [];
  let month = "";
  for (const folder of folders) {
    const match = folder.folder.match(DATE);
    const current = match ? `${match[1]}-${match[2]}` : "";
    if (current && current !== month) { month = current; rows.push(el("div", { class: "month-head" }, current)); }
    rows.push(...folderRow(folder));
  }
  return el("section", { class: "section", "aria-label": "폴더" },
    el("div", { class: "section-head" },
      el("h2", { class: "section-title" }, "폴더 ", el("span", { class: "num" }, String(folders.length))),
      canCalendar ? el("div", { class: "seg", role: "group", "aria-label": "보기" }, ...[["calendar", "달력"], ["list", "목록"]].map(([id, name]) =>
        el("button", { type: "button", "aria-pressed": String(view === id), onclick: () => { ui.folderView = id; notify(); } }, name))) : null),
    view === "calendar"
      ? el("div", { class: "folder-table" }, calendar(folders))
      : el("div", { class: "folder-table folder-list", id: "drive-folders" },
        el("div", { class: "folder-head", "aria-hidden": "true" }, ...["폴더", "상태", "처리 방식", "영상", "단계", ""].map((text) => el("span", {}, text))),
        ...(rows.length ? rows : [emptyState("폴더 없음")])));
}

function completedWorkSection(drive) {
  const items = drive.completed_work || [];
  if (!items.length) return null;
  const rows = items.map((item) => {
    const trace = [
      item.archive_display_name,
      `패키지 ${item.archive_package_id}`,
      `작업 ${item.job_id}`,
      `실행 ${item.archive_run_id}`,
    ].filter(Boolean).join("\n");
    const sourceCount = item.source_video_count > 1 ? `${formatCount(item.source_video_count)} → 1` : formatCount(item.source_video_count);
    return el("a", { class: "completed-row", href: `#/runs/${encodeURIComponent(item.archive_run_id)}?result=${encodeURIComponent(item.archive_package_id)}`, dataset: { packageId: item.archive_package_id, jobId: item.job_id, archiveRunId: item.archive_run_id }, title: trace },
      el("span", { class: "completed-result" },
        el("b", {}, item.archive_video_file_name || item.archive_package_id),
        el("small", {}, item.folder)),
      el("span", { class: "completed-kind" }, label("disposition", item.disposition), item.profile_reference ? el("small", {}, item.profile_reference) : null),
      el("span", { class: "completed-source num" }, sourceCount),
      el("span", { class: "completed-time num" }, shortTime(item.completed_at)),
      el("span", { class: "completed-place" }, archiveBadge(item.archive_state), el("small", {}, item.archive_storage_id),
        el("small", { class: "archive-relative-path", title: item.archive_relative_path || "" }, item.archive_relative_path || "—")));
  });
  return el("section", { class: "section", id: "completed-work", "aria-label": "완료 내역" },
    el("h2", { class: "section-title" }, "완료 내역 ", el("span", { class: "num" }, String(items.length))),
    el("div", { class: "completed-table" },
      el("div", { class: "completed-head", "aria-hidden": "true" }, ...["결과", "작업", "원본 → 결과", "완료", "아카이브"].map((text) => el("span", {}, text))),
      ...rows));
}

async function startArchiveRun(drive) {
  if (ui.runStarting) return;
  if (!ui.archiveRunID) ui.archiveRunID = newID("ARCHIVE-RUN");
  if (!ui.archiveRunKey) ui.archiveRunKey = newID("ARCHIVE-RUN-START");
  ui.runStarting = true;
  notify();
  try {
    const result = await mutation("/api/v1/archive-intake/runs", {
      archive_run_id: ui.archiveRunID, idempotency_key: ui.archiveRunKey, session_id: drive.session_id || ui.intakeSessionID,
      source_volume_id: drive.source_volume_id, expected_candidate_revision: drive.candidate_revision || drive.input_revision,
      archive_storage_id: ui.archiveID, input_revision: drive.folder_discovery_revision || drive.input_revision, folder_ids: [...ui.folders].sort(),
    });
    ui.status = `${result.folder_count} 폴더 · ${result.archive_run_id}`;
    ui.prepDriveID = "";
    await refresh();
  } catch (error) {
    ui.status = error.message;
    ui.runStarting = false;
    notify();
  }
}

function resetArchiveRunIntent() {
  ui.archiveRunID = newID("ARCHIVE-RUN");
  ui.archiveRunKey = newID("ARCHIVE-RUN-START");
  ui.runStarting = false;
}

function archiveOption(storage) {
  return el("button", {
    type: "button", class: "option", "aria-pressed": String(storage.storage_id === ui.archiveID), disabled: !storage.accessible,
    dataset: { storageId: storage.storage_id, accessKind: storage.access_kind || "" },
    onclick: () => { ui.archiveID = ui.archiveID === storage.storage_id ? "" : storage.storage_id; resetArchiveRunIntent(); notify(); },
  },
  el("span", { class: "option-main" }, el("b", {}, storage.label || storage.storage_id)),
  el("span", { class: "option-meta" }, `${label("storageRole", storage.role)} · ${label("storageAccess", storage.access_kind)}`,
    el("small", {}, storage.accessible ? `여유 ${formatBytes(storage.free_bytes)} / ${formatBytes(storage.capacity_bytes)}` : "연결 필요")));
}

function prepPanel(drive) {
  syncPreparation(drive);
  const folders = discoveredFolders(drive);
  const ready = folderDiscoveryReady(drive);
  const all = el("input", { type: "checkbox", id: "select-all", "aria-label": "전체 선택", disabled: !ready });
  const selected = folders.filter((folder) => ui.folders.has(folder.folder));
  all.checked = ready && folders.length > 0 && selected.length === folders.length;
  all.indeterminate = selected.length > 0 && selected.length < folders.length;
  all.addEventListener("change", () => { ui.folders = new Set(all.checked ? folders.map((folder) => folder.folder) : []); resetArchiveRunIntent(); notify(); });
  const picks = folders.map((folder) => {
    const check = el("input", { type: "checkbox", "aria-label": `${folder.folder} 선택`, disabled: !ready });
    check.checked = ui.folders.has(folder.folder);
    check.addEventListener("change", () => { check.checked ? ui.folders.add(folder.folder) : ui.folders.delete(folder.folder); resetArchiveRunIntent(); notify(); });
    return el("label", { class: "folder-pick", dataset: { folderId: folder.folder } }, check,
      el("span", { class: "folder-pick-name" }, folder.folder),
      el("small", { class: "num" }, `${formatCount(folder.video_count)} 영상 · ${formatBytes(folder.estimated_output_bytes)}`));
  });
  const candidates = archiveCandidates(drive);
  const videos = selected.reduce((sum, folder) => sum + (folder.video_count || 0), 0);
  const bytes = selected.reduce((sum, folder) => sum + (folder.estimated_output_bytes || 0), 0);
  const blocker = startBlocker(drive, selected);
  const folderHeading = ready ? `탐색 r${drive.folder_discovery_revision} · ${folders.length}` : folderDiscoveryText(drive);
  return el("section", { class: "section", id: "drive-prep", "aria-label": "아카이브 실행 준비" },
    el("h2", { class: "section-title" }, "아카이브 실행 준비"),
    el("div", { class: "chooser" },
      el("div", { class: "chooser-col", id: "prep-archives", "aria-label": "아카이브 저장소" },
        el("div", { class: "chooser-head" }, el("b", {}, "아카이브 저장소"), el("span", { class: "chooser-value" }, ui.archiveID || "선택 없음")),
        ...(candidates.length ? candidates.map(archiveOption) : [emptyState("후보 없음")])),
      el("div", { class: "chooser-col", id: "prep-folders", "aria-label": "폴더" },
        el("div", { class: "chooser-head" }, el("b", {}, "폴더"), el("span", { class: "chooser-value num" }, folderHeading)),
        el("label", { class: "folder-pick folder-all" }, all, el("span", {}, "전체")),
        ...(picks.length ? picks : [emptyState(drive.folder_discovery_state === "SCANNING" ? "검색 중" : drive.folder_discovery_state === "FAILED" ? "검색 실패" : "폴더 없음")]))),
    el("div", { class: "panel start-bar" },
      el("span", { id: "selection-summary", class: "num" }, `${selected.length} 폴더 · ${formatCount(videos)} 영상 · ${formatBytes(bytes)}`),
      el("span", { id: "start-reason", class: "start-reason" }, blocker),
      el("button", { type: "button", class: "btn primary", id: "start-archive-run", dataset: { action: "start" }, disabled: Boolean(blocker) || ui.runStarting, onclick: () => startArchiveRun(drive) }, ui.runStarting ? "시작 중" : "아카이브 실행 시작")));
}

export function mount(root, route) {
  jobDetails = new Map();
  ui.openFolder = route.query.get("folder") || "";
  ui.openSteps = "";
  ui.folderView = "";
  ui.hold = null;
  ui.status = "";
  let placed = false;
  const title = el("h1", { id: "drive-title" });
  const actions = el("div", { class: "page-actions" });
  const body = el("div", { class: "drive-body" });
  root.append(el("div", { class: "page-head" }, el("div", { class: "page-title" }, title), actions), body);
  return {
    update() {
      const drive = findDrive(route.driveID);
      if (!drive) {
        title.textContent = route.driveID;
        actions.replaceChildren();
        body.replaceChildren(emptyState("드라이브 없음"));
        return;
      }
      const folders = drive.folders || [];
      const problems = folders.map((folder) => [folder, folderProblem(folder)]).filter(([, problem]) => problem)
        .sort(([, left], [, right]) => RANK[right.signal] - RANK[left.signal]);
      if (!placed) {
        placed = true;
        if (!ui.openFolder && problems.length) ui.openFolder = problems[0][0].folder;
      }
      const worker = model.workers.find((item) => item.worker_id === drive.worker_id);
      title.textContent = driveName(drive);
      title.title = `${drive.worker_id} ${label("worker", worker?.state)} · → ${drive.archive_storage_id || "아카이브 미정"} · ${drive.archive_run_id || "실행 없음"} · r${drive.input_revision} · ${formatCount(drive.folder_count)} 폴더 · ${formatCount(drive.file_count)} 영상`;

      // The STT gate of a run and the preparation of a new one sit beside the title.
      const buttons = [];
      if (drive.archive_run_id) buttons.push(sttButton(drive));
      if (!isRunning(drive) && !preparing(drive)) {
        buttons.push(el("button", { type: "button", class: `btn${drive.archive_run_id ? "" : " primary"}`, id: "prepare-run", dataset: { action: "prepare" }, onclick: () => prepareDrive(drive) },
          drive.archive_run_id ? "새 아카이브 실행 준비" : "아카이브 실행 준비"));
      }
      actions.replaceChildren(...buttons);

      const sections = [overview(drive)];
      if (ui.status) sections.push(el("p", { class: "status-line", role: "status", id: "command-status" }, ui.status));
      if (preparing(drive)) sections.push(prepPanel(drive));
      const completed = completedWorkSection(drive);
      if (completed) sections.push(completed);
      if (problems.length) {
        sections.push(el("section", { class: "section", id: "drive-problems", "aria-label": "확인 필요" },
          el("h2", { class: "section-title" }, "확인 필요 ", el("span", { class: "num" }, String(problems.length))),
          el("div", { class: "card-list" }, ...problems.map(([folder, problem]) => problemPanel(drive, folder, problem)))));
      }
      if (folders.length && drive.archive_run_id) sections.push(folderSection(drive));
      const previousFolderChoices = body.querySelector("#prep-folders");
      if (previousFolderChoices) ui.prepFolderScroll = previousFolderChoices.scrollTop;
      body.replaceChildren(...sections);
      const currentFolderChoices = body.querySelector("#prep-folders");
      if (currentFolderChoices && ui.prepFolderScroll > 0) currentFolderChoices.scrollTop = ui.prepFolderScroll;
    },
  };
}
