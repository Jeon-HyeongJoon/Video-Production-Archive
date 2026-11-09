// 작업: every persisted Job has one addressable detail route. Other screens
// link here by job_id; they do not create their own expandable Job detail.
// Lists show a work's name, state, recent time and — only while it runs — its
// progress; the Worker and IDs wait on hover.

import {
  el, svgIcon, icons, token, statusIcon, label, stateSignal, stageSignal, finishedSteps, formatCount, formatPercent,
  formatBytes, shortTime, clockTime, meter, api, mutation, newID, emptyState,
} from "../core.js";
import {
  model, ui, notify, refresh, jobLane, driveName, findDrive, folderProblem, workLane, workName,
  isRunning, prepareDrive, syncPreparation, discoveredFolders, archiveCandidates,
  folderDiscoveryReady, folderDiscoveryText, startBlocker,
} from "../store.js";

const LANE = {
  fault: ["실패", "fault"], decide: ["사람 대기", "decide"], work: ["실행 중", "work"],
  resource: ["자원 대기", "resource"], queue: ["대기열", "idle"], done: ["완료", "done"],
};

function laneOf(job) {
  return jobLane(job) || "done";
}

function laneText(job) {
  const lane = laneOf(job);
  if (lane === "decide") return job.wait_reason ? `${label("wait", job.wait_reason)} 대기` : "사람 대기";
  return LANE[lane]?.[0] || label("step", job.step_state);
}

function completion(jobID) {
  for (const drive of model.drives) {
    const item = (drive.completed_work || []).find((entry) => entry.job_id === jobID);
    if (item) return { item, drive };
  }
  return null;
}

function linkedFolder(jobID) {
  for (const drive of model.drives) {
    const folder = (drive.folders || []).find((entry) => entry.job_id === jobID || entry.transcription_job_id === jobID);
    if (folder) return { folder, drive };
  }
  return null;
}

function table(heads, rows, empty, className = "jobs-table") {
  return el("div", { class: `data-table ${className}`, role: "table" },
    el("div", { class: "data-row data-head", role: "row" }, ...heads.map((head) => el("span", { role: "columnheader" }, head))),
    ...(rows.length ? rows : [emptyState(empty)]));
}

function workHref(work) {
  return work.kind === "ARCHIVE_RUN" ? `#/runs/${encodeURIComponent(work.work_id)}` : `#/jobs/${encodeURIComponent(work.work_id)}`;
}

function workStateText(work) {
  return { FAILED: "실패", ATTENTION: "확인 필요", ACTIVE: "실행 중", QUEUED: "대기열", COMPLETED: "완료" }[work.state] || work.state;
}

const WORK_HEADS = ["작업", "상태", "진행", "최근"];

function workRow(work) {
  const lane = workLane(work);
  const pct = Math.round((work.progress || 0) * 100);
  const scope = work.kind === "ARCHIVE_RUN" ? `${formatCount(work.folder_count)} 폴더 · ${formatCount(work.video_count)} 영상` : "";
  return el("a", {
    class: "data-row work-row", role: "row", href: workHref(work), dataset: { workId: work.work_id, kind: work.kind, lane },
    title: [work.work_id, `Worker ${work.worker_id || "배정 없음"}`].join("\n"),
  },
  el("span", { class: "cell-main" }, el("b", {}, workName(work)), scope ? el("small", { class: "work-scope num" }, scope) : null),
  el("span", { class: "cell-state" }, statusIcon(lane === "queue" ? "idle" : lane),
    el("span", { class: "job-state", dataset: { signal: ["fault", "decide"].includes(lane) ? lane : undefined } }, workStateText(work))),
  lane === "work" ? el("span", { class: "cell-meter" }, meter(pct, false), el("span", { class: "num job-progress" }, `${pct}%`)) : el("span", {}),
  el("span", { class: "num muted" }, shortTime(work.last_event_at)));
}

function workSection(title, work, empty, id = "") {
  return el("section", { class: "section jobs-section", id: id || undefined },
    el("h2", { class: "section-title" }, `${title} `, el("span", { class: "num" }, String(work.length))),
    table(WORK_HEADS, work.map(workRow), empty));
}

function childRole(role) {
  return { INSPECTION: "검사", FOLDER: "폴더 처리", TRANSCRIPTION: "STT" }[role] || role;
}

function childRow(child, media) {
  const job = child.job;
  const lane = laneOf(job);
  const body = el("div", { class: "job-media-body" });
  let disclosure;
  disclosure = el("details", {
    class: "child-work", open: media.isOpen(job.job_id), dataset: { jobId: job.job_id, lane },
    ontoggle: () => media.toggle(disclosure, body, job.job_id),
  },
  el("summary", { class: "data-row job-row", role: "row", title: `${job.job_id}\nWorker ${job.worker_id || "배정 없음"}`, dataset: { jobId: job.job_id, lane } },
    el("span", { class: "cell-main" },
      el("a", { href: `#/jobs/${encodeURIComponent(job.job_id)}`, onclick: (event) => event.stopPropagation() }, child.folder || childRole(child.role)),
      el("small", { class: "media-hint" }, "영상 정보")),
    el("span", {}, childRole(child.role)),
    el("span", { class: "cell-state" }, statusIcon(LANE[lane]?.[1] || "idle"), el("span", {}, laneText(job))),
    el("span", { class: "cell-meter" }, meter(Math.round((job.progress || 0) * 100), lane === "done"), el("span", { class: "num" }, `${Math.round((job.progress || 0) * 100)}%`)),
    el("span", { class: "num muted child-recent" }, shortTime(job.last_event_at))), body);
  return disclosure;
}

function fold(id, title, count, open, onToggle, ...children) {
  const details = el("details", { class: "job-fold", id, open, ontoggle: () => onToggle(details.open) },
    el("summary", {}, el("span", {}, title), el("span", { class: "num" }, String(count))),
    el("div", { class: "job-fold-body" }, ...children));
  return details;
}

function fact(labelText, value, className = "") {
  return el("div", { class: `job-fact ${className}`.trim() }, el("span", {}, labelText), el("b", {}, value || "—"));
}

function resultPanel(found) {
  if (!found) return null;
  const { item, drive } = found;
  const source = `${formatCount(item.source_video_count)} → 1`;
  const resultHref = `#/runs/${encodeURIComponent(item.archive_run_id)}?result=${encodeURIComponent(item.archive_package_id)}`;
  return el("section", { class: "panel job-result", id: "job-result", dataset: { packageId: item.archive_package_id } },
    el("div", { class: "panel-toolbar" }, el("b", { class: "panel-title" }, "완료 결과"),
      token(item.archive_state === "COMMITTED" ? "확정됨" : "보관됨", item.archive_state === "COMMITTED" ? "done" : "idle"),
      el("a", { class: "btn archive-result-link", href: resultHref }, "아카이브 결과")),
    el("div", { class: "job-result-grid" },
      fact("결과 영상", item.archive_video_file_name || item.archive_package_id, "wide"),
      fact("원본 폴더", item.folder),
      fact("처리 방식", label("disposition", item.disposition)),
      fact("원본 → 결과", source),
      fact("완료", shortTime(item.completed_at)),
      fact("아카이브", item.archive_storage_id),
      fact("아카이브 위치", item.archive_relative_path || "—", "wide"),
      fact("Profile", item.profile_reference),
      fact("패키지", item.archive_package_id),
      fact("실행", item.archive_run_id),
      fact("원본 드라이브", driveName(drive))));
}

function stepRow(step) {
  const signal = stageSignal({ state: step.state, wait_reason: step.wait_reason });
  const detail = [
    step.state === "WAITING" && step.wait_reason ? `WAITING · ${step.wait_reason}` : step.state,
    formatPercent(Math.round((step.progress || 0) * 100)), step.worker_id, step.error_class,
    step.log_reference ? `로그 ${step.log_reference}` : "",
  ].filter(Boolean).join(" · ");
  return el("div", { class: "stage-row", dataset: { step: step.id, signal } },
    el("i", { class: "step-dot", dataset: { signal } }), el("b", {}, step.id), el("span", { class: "stage-state" }, detail));
}

function timelineRow(item) {
  return el("div", { class: "timeline-row", dataset: { kind: item.kind } },
    el("time", { class: "num" }, clockTime(item.occurred_at, true)),
    el("b", {}, item.kind), el("span", {}, item.summary || item.reference || "—"));
}

function runResultRow(item, selected) {
  return el("div", { class: "completed-row", dataset: { packageId: item.archive_package_id, jobId: item.job_id, selected: selected ? "true" : undefined } },
    el("span", { class: "completed-result" }, el("b", {}, item.archive_video_file_name || item.archive_package_id), el("small", {}, item.folder)),
    el("span", { class: "completed-kind" }, label("disposition", item.disposition), item.profile_reference ? el("small", {}, item.profile_reference) : null),
    el("span", { class: "completed-source num" }, `${formatCount(item.source_video_count)} → 1`),
    el("span", { class: "completed-time num" }, shortTime(item.completed_at)),
    el("span", { class: "completed-place" }, token(item.archive_state === "COMMITTED" ? "확정됨" : "보관됨", item.archive_state === "COMMITTED" ? "done" : "idle"),
      el("small", {}, item.archive_storage_id), el("small", { class: "archive-relative-path", title: item.archive_relative_path || "" }, item.archive_relative_path || "—")));
}

function mediaDate(value) {
  if (!value || String(value).startsWith("0001-01-01")) return "날짜 없음";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "날짜 없음";
  return new Intl.DateTimeFormat("ko-KR", { year: "numeric", month: "2-digit", day: "2-digit" }).format(date);
}

function mediaDuration(millis) {
  if (!millis) return "—";
  const total = Math.max(0, Math.round(millis / 1000));
  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const seconds = total % 60;
  return hours ? `${hours}:${String(minutes).padStart(2, "0")}:${String(seconds).padStart(2, "0")}` : `${minutes}:${String(seconds).padStart(2, "0")}`;
}

function mediaBytes(value) {
  if (!value) return "—";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let amount = Number(value);
  let unit = 0;
  while (amount >= 1000 && unit < units.length - 1) { amount /= 1000; unit += 1; }
  return `${amount >= 100 || unit === 0 ? Math.round(amount) : amount.toFixed(1)} ${units[unit]}`;
}

function mediaRate(value) {
  if (!value) return "—";
  return value >= 1_000_000 ? `${(value / 1_000_000).toFixed(value >= 10_000_000 ? 1 : 2)} Mbps` : `${Math.round(value / 1000)} kbps`;
}

function mediaTable(items) {
  if (!items.length) return emptyState("연결된 영상 정보 없음");
  const rows = items.map((item) => {
    const video = [item.container, item.video_codec, item.width && item.height ? `${item.width}×${item.height}` : "", mediaRate(item.video_bitrate_bps || item.bitrate_bps)].filter(Boolean).join(" · ") || "—";
    const audio = [item.audio_codec, item.audio_sample_rate_hz ? `${Math.round(item.audio_sample_rate_hz / 1000)} kHz` : "", item.audio_channels ? `${item.audio_channels}ch` : ""].filter(Boolean).join(" · ") || "—";
    return el("div", { class: "video-row", role: "row", dataset: { assetId: item.original_asset_id } },
      el("span", { class: "video-source" }, el("b", {}, `${mediaDate(item.source_created_at)} · ${item.original_name || "이름 없음"}`), el("small", { title: item.relative_path || "" }, item.relative_path || "—")),
      el("span", { class: "num" }, mediaDuration(item.duration_millis), el("small", {}, mediaBytes(item.size_bytes))),
      el("span", { class: "video-tech" }, el("b", {}, video), el("small", {}, audio)),
      el("span", { class: "video-profile", title: item.profile_reference || "" }, item.profile_reference || "—"));
  });
  return el("div", { class: "video-table", role: "table" },
    el("div", { class: "video-head", role: "row" }, ...["원본", "길이 · 용량", "기술정보", "Profile"].map((text) => el("span", { role: "columnheader" }, text))),
    ...rows);
}

function jobMediaDisclosure(jobID, media) {
  const body = el("div", { class: "job-media-body" });
  const toggleLabel = el("span", { class: "muted media-disclosure-label" }, media.isOpen(jobID) ? "접기" : "펼치기");
  let disclosure;
  disclosure = el("details", {
    class: "panel job-media-disclosure", id: "job-media", open: media.isOpen(jobID), dataset: { jobId: jobID },
    ontoggle: () => { toggleLabel.textContent = disclosure.open ? "접기" : "펼치기"; media.toggle(disclosure, body, jobID); },
  }, el("summary", { class: "panel-toolbar" }, el("b", { class: "panel-title" }, "영상 정보"), toggleLabel), body);
  return disclosure;
}

// runAttention lists the folder problems of this run's fixed scope while the
// run is still its drive's current one; an older run only reports its holds.
function runAttention(summary, folderIDs) {
  const drive = findDrive(summary.source_volume_id);
  const scope = new Set(folderIDs);
  const items = drive?.archive_run_id === summary.work_id
    ? (drive.folders || []).filter((folder) => scope.has(folder.folder)).map((folder) => [folder, folderProblem(folder)]).filter(([, problem]) => problem)
      .sort(([, left], [, right]) => (left.signal === right.signal ? 0 : left.signal === "fault" ? -1 : 1))
    : [];
  const driveHref = (folder = "") => `#/drives/${encodeURIComponent(summary.source_volume_id)}${folder ? `?folder=${encodeURIComponent(folder)}` : ""}`;
  const rows = items.map(([folder, problem]) => el("a", { class: "attention-link", href: driveHref(folder.folder), dataset: { folderId: folder.folder, kind: problem.kind, signal: problem.signal } },
    statusIcon(problem.signal), el("b", {}, problem.title), el("span", {}, folder.folder),
    el("span", { class: "num muted" }, `영상 ${formatCount(folder.video_count)}`), svgIcon(icons.next)));
  if (!rows.length && summary.hold_count > 0) {
    rows.push(el("a", { class: "attention-link", href: driveHref(), dataset: { kind: "hold", signal: "decide" } },
      statusIcon("decide"), el("b", {}, "분류 확인 필요"), el("span", { class: "num" }, `${formatCount(summary.hold_count)} 폴더`), el("span", {}), svgIcon(icons.next)));
  }
  if (!rows.length) return null;
  return el("section", { class: "section jobs-section", id: "run-attention", "aria-label": "확인 필요" },
    el("h2", { class: "section-title" }, "확인 필요 ", el("span", { class: "num" }, String(rows.length))),
    el("div", { class: "attention-list" }, ...rows));
}

function renderRunDetail(root, detail, media, route) {
  const summary = detail?.summary;
  if (!summary) {
    root.replaceChildren(emptyState("아카이브 작업을 찾을 수 없음"));
    return;
  }
  const lane = workLane(summary);
  const source = summary.source_volume_id || summary.source_storage_id;
  const facts = [
    ["원본 → 아카이브", `${source || "—"} → ${summary.archive_storage_id || "—"}`], ["폴더", formatCount(summary.folder_count)], ["영상", formatCount(summary.video_count)],
  ];
  const trace = [
    `작업 ID ${summary.work_id}`, `Worker ${summary.worker_id || "배정 없음"}`,
    `하위 작업 ${formatCount(summary.child_job_count)}`, `결과 ${formatCount(summary.result_count)}`,
  ].join("\n");
  const folderIDs = detail.folder_ids || [];
  const folders = folderIDs.map((folder) => el("div", { class: "data-row", role: "row" }, el("b", { class: "cell-main" }, folder)));
  const children = detail.children || [];
  const completed = detail.completed_work || [];
  const selectedPackage = route?.query?.get("result") || "";
  root.replaceChildren(...[
    el("div", { class: "page-head" },
      el("div", { class: "page-title" }, el("h1", { id: "run-title" }, workName(summary)), token(workStateText(summary), LANE[lane]?.[1] || "idle"))),
    runAttention(summary, folderIDs),
    el("section", { class: "panel job-summary", id: "run-summary", title: trace },
      el("div", { class: "job-facts" }, ...facts.map(([name, value]) => fact(name, value)))),
    el("section", { class: "section jobs-section", id: "run-children" },
      el("h2", { class: "section-title" }, "하위 작업 ", el("span", { class: "num" }, String(children.length))),
      table(["대상", "단계", "상태", "진행", "최근"], children.map((child) => childRow(child, media)), "하위 작업 없음", "children-table")),
    completed.length ? el("section", { class: "section jobs-section", id: "run-results" },
      el("h2", { class: "section-title" }, "완료 결과 ", el("span", { class: "num" }, String(completed.length))),
      el("div", { class: "completed-table" },
        el("div", { class: "completed-head", "aria-hidden": "true" }, ...["결과", "작업", "원본 → 결과", "완료", "아카이브"].map((text) => el("span", {}, text))),
        ...completed.map((item) => runResultRow(item, item.archive_package_id === selectedPackage)))) : null,
    fold("run-scope", "고정 폴더 범위", folders.length, ui.runScopeOpen, (open) => { ui.runScopeOpen = open; },
      el("div", { class: "data-table scope-table", role: "table" }, ...(folders.length ? folders : [emptyState("폴더 없음")]))),
  ].filter(Boolean));
}

export function mount(root, route) {
  let detail = null;
  let workDetail = null;
  let loadedKey = "";
  let loadError = "";
  let disposed = false;
  let request = null;
  let createOpen = false;
  let createPending = false;
  let createError = "";
  const createDraft = { kind: "ARCHIVE", target_title: "", template_name: "occasion-recording", control_mode: "ASSISTED" };
  const mediaCache = new Map();
  const mediaOpen = new Set();
  const mediaRequests = new Map();
  ui.jobCancel = "";
  ui.status = "";

  const media = {
    isOpen(jobID) { return mediaOpen.has(jobID); },
    async toggle(disclosure, body, jobID) {
      if (!disclosure.open) {
        mediaOpen.delete(jobID);
        body.replaceChildren();
        return;
      }
      mediaOpen.add(jobID);
      if (mediaCache.has(jobID)) {
        body.replaceChildren(mediaTable(mediaCache.get(jobID)));
        return;
      }
      body.replaceChildren(emptyState("영상 정보 불러오는 중"));
      let pending = mediaRequests.get(jobID);
      if (!pending) {
        const controller = new AbortController();
        pending = { controller, value: api(`/api/v1/jobs/${encodeURIComponent(jobID)}/media`, { signal: controller.signal }) };
        mediaRequests.set(jobID, pending);
      }
      try {
        const values = await pending.value;
        if (disposed) return;
        mediaCache.set(jobID, values || []);
        if (disclosure.open) body.replaceChildren(mediaTable(mediaCache.get(jobID)));
      } catch (error) {
        if (error.name !== "AbortError" && !disposed && disclosure.open) body.replaceChildren(emptyState("영상 정보 조회 실패"));
      } finally {
        mediaRequests.delete(jobID);
      }
    },
  };

  async function cancel(job) {
    try {
      await mutation(`/api/v1/jobs/${encodeURIComponent(job.job_id)}/cancel`, {
        command_id: newID("COMMAND"), idempotency_key: newID("CANCEL"), reason: "담당자 취소", expected_revision: job.revision,
      });
      ui.jobCancel = "";
      ui.status = `${job.job_id} 취소 요청`;
      loadedKey = "";
      await refresh();
    } catch (error) {
      ui.status = error.message;
      notify();
    }
  }

  async function createJob(event) {
    event.preventDefault();
    if (createPending) return;
    const jobID = newID("JOB");
    createPending = true;
    createError = "";
    renderList();
    try {
      await mutation("/api/v1/jobs", {
        job_id: jobID, command_id: newID("COMMAND"), idempotency_key: `${jobID}:create`,
        reason: "작업 관리에서 생성", template_name: createDraft.template_name,
        target_title: createDraft.target_title.trim(), control_mode: createDraft.control_mode, expected_revision: 0,
      });
      await refresh();
      if (!disposed) window.location.hash = `#/jobs/${encodeURIComponent(jobID)}`;
    } catch (error) {
      createPending = false;
      createError = error.message;
      if (!disposed) renderList();
    }
  }

  function resetArchiveRunIntent() {
    ui.archiveRunID = newID("ARCHIVE-RUN");
    ui.archiveRunKey = newID("ARCHIVE-RUN-START");
    ui.runStarting = false;
  }

  async function startArchiveRun(drive) {
    if (ui.runStarting) return;
    if (!ui.archiveRunID) resetArchiveRunIntent();
    ui.runStarting = true;
    createError = "";
    renderList();
    try {
      const result = await mutation("/api/v1/archive-intake/runs", {
        archive_run_id: ui.archiveRunID, idempotency_key: ui.archiveRunKey,
        session_id: ui.intakeSessionID,
        source_volume_id: drive.source_volume_id,
        expected_candidate_revision: drive.candidate_revision || drive.input_revision,
        archive_storage_id: ui.archiveID,
        input_revision: drive.folder_discovery_revision || drive.input_revision,
        folder_ids: [...ui.folders].sort(),
      });
      ui.prepDriveID = "";
      await refresh();
      if (!disposed) window.location.hash = `#/runs/${encodeURIComponent(result.archive_run_id)}`;
    } catch (error) {
      ui.runStarting = false;
      createError = error.message;
      if (!disposed) renderList();
    }
  }

  function archiveOption(storage) {
    return el("button", {
      type: "button", class: "option", "aria-pressed": String(storage.storage_id === ui.archiveID), disabled: !storage.accessible,
      dataset: { storageId: storage.storage_id, accessKind: storage.access_kind || "" },
      onclick: () => { ui.archiveID = ui.archiveID === storage.storage_id ? "" : storage.storage_id; resetArchiveRunIntent(); renderList(); },
    },
    el("span", { class: "option-main" }, el("b", {}, storage.label || storage.storage_id)),
    el("span", { class: "option-meta" }, `${label("storageRole", storage.role)} · ${label("storageAccess", storage.access_kind)}`,
      el("small", {}, storage.accessible ? `여유 ${formatBytes(storage.free_bytes)} / ${formatBytes(storage.capacity_bytes)}` : "연결 필요")));
  }

  function archiveCreatePanel() {
    const available = model.drives.filter((drive) => drive.connection_state === "CONNECTED" && !isRunning(drive));
    const selectedDrive = findDrive(ui.prepDriveID);
    const driveOptions = available.map((drive) => el("button", {
      type: "button", class: "option", "aria-pressed": String(drive.workspace_drive_id === ui.prepDriveID),
      dataset: { driveId: drive.workspace_drive_id },
      onclick: async () => { createError = ""; await prepareDrive(drive); },
    }, el("span", { class: "option-main" }, el("b", {}, driveName(drive))),
    el("span", { class: "option-meta" }, `${drive.worker_id} · ${folderDiscoveryText(drive)}`)));
    if (!selectedDrive) {
      return el("div", { class: "archive-create", id: "archive-create" },
        el("div", { class: "chooser-col", id: "prep-drives", "aria-label": "원본 드라이브" },
          el("div", { class: "chooser-head" }, el("b", {}, "원본 드라이브"), el("span", { class: "chooser-value" }, "선택 없음")),
          ...(driveOptions.length ? driveOptions : [emptyState("연결된 드라이브 없음")])),
        createError ? el("span", { id: "new-job-status", role: "status" }, createError) : null);
    }

    syncPreparation(selectedDrive);
    const folders = discoveredFolders(selectedDrive);
    const ready = folderDiscoveryReady(selectedDrive);
    const selected = folders.filter((folder) => ui.folders.has(folder.folder));
    const all = el("input", { type: "checkbox", id: "select-all", "aria-label": "전체 선택", disabled: !ready });
    all.checked = ready && folders.length > 0 && selected.length === folders.length;
    all.indeterminate = selected.length > 0 && selected.length < folders.length;
    all.addEventListener("change", () => { ui.folders = new Set(all.checked ? folders.map((folder) => folder.folder) : []); resetArchiveRunIntent(); renderList(); });
    const picks = folders.map((folder) => {
      const check = el("input", { type: "checkbox", "aria-label": `${folder.folder} 선택`, disabled: !ready });
      check.checked = ui.folders.has(folder.folder);
      check.addEventListener("change", () => { check.checked ? ui.folders.add(folder.folder) : ui.folders.delete(folder.folder); resetArchiveRunIntent(); renderList(); });
      return el("label", { class: "folder-pick", dataset: { folderId: folder.folder } }, check,
        el("span", { class: "folder-pick-name" }, folder.folder),
        el("small", { class: "num" }, `${formatCount(folder.video_count)} 영상 · ${formatBytes(folder.estimated_output_bytes)}`));
    });
    const candidates = archiveCandidates(selectedDrive);
    const videos = selected.reduce((sum, folder) => sum + (folder.video_count || 0), 0);
    const bytes = selected.reduce((sum, folder) => sum + (folder.estimated_output_bytes || 0), 0);
    const blocker = startBlocker(selectedDrive, selected);
    const folderHeading = ready ? `탐색 r${selectedDrive.folder_discovery_revision} · ${folders.length}` : folderDiscoveryText(selectedDrive);
    return el("div", { class: "archive-create", id: "archive-create" },
      el("div", { class: "chooser-col", id: "prep-drives", "aria-label": "원본 드라이브" },
        el("div", { class: "chooser-head" }, el("b", {}, "원본 드라이브"), el("span", { class: "chooser-value" }, driveName(selectedDrive))),
        ...driveOptions),
      el("div", { class: "chooser", id: "archive-scope" },
        el("div", { class: "chooser-col", id: "prep-archives", "aria-label": "아카이브 저장소" },
          el("div", { class: "chooser-head" }, el("b", {}, "아카이브 저장소"), el("span", { class: "chooser-value" }, ui.archiveID || "선택 없음")),
          ...(candidates.length ? candidates.map(archiveOption) : [emptyState("후보 없음")])),
        el("div", { class: "chooser-col", id: "prep-folders", "aria-label": "폴더" },
          el("div", { class: "chooser-head" }, el("b", {}, "폴더"), el("span", { class: "chooser-value num" }, folderHeading)),
          el("label", { class: "folder-pick folder-all" }, all, el("span", {}, "전체")),
          ...(picks.length ? picks : [emptyState(selectedDrive.folder_discovery_state === "SCANNING" ? "검색 중" : selectedDrive.folder_discovery_state === "FAILED" ? "검색 실패" : "폴더 없음")]))),
      el("div", { class: "start-bar" },
        el("span", { id: "selection-summary", class: "num" }, `${selected.length} 폴더 · ${formatCount(videos)} 영상 · ${formatBytes(bytes)}`),
        el("span", { id: "start-reason", class: "start-reason" }, createError || blocker),
        el("button", { type: "button", class: "btn primary", id: "start-archive-run", disabled: Boolean(blocker) || ui.runStarting, onclick: () => startArchiveRun(selectedDrive) }, ui.runStarting ? "생성 중" : "아카이브 작업 생성")));
  }

  function templateCreatePanel() {
    const title = el("input", {
      id: "new-job-title", name: "target_title", required: true, autocomplete: "off", value: createDraft.target_title,
      oninput: (event) => { createDraft.target_title = event.target.value; },
    });
    const template = el("select", {
      id: "new-job-template", name: "template_name", value: createDraft.template_name,
      onchange: (event) => { createDraft.template_name = event.target.value; },
    },
    ...[
      ["occasion-recording", "행사 기록"], ["vj-compilation", "VJ 모음"],
      ["final-edit", "최종 편집"], ["large-file-intake", "대용량 입고"],
    ].map(([value, text]) => el("option", { value, selected: value === createDraft.template_name }, text)));
    const mode = el("select", {
      id: "new-job-mode", name: "control_mode",
      onchange: (event) => { createDraft.control_mode = event.target.value; },
    }, ...["ASSISTED", "ARCHIVIST", "MANUAL"].map((value) => el("option", {
      value, selected: value === createDraft.control_mode,
    }, label("mode", value))));
    const submit = el("button", { type: "submit", class: "btn primary", id: "new-job-submit", disabled: createPending }, createPending ? "생성 중" : "생성");
    const form = el("form", { id: "new-job-form", class: "job-create-form", onsubmit: createJob },
      el("label", { class: "field" }, el("span", {}, "작업 이름"), title),
      el("label", { class: "field" }, el("span", {}, "작업 유형"), template),
      el("label", { class: "field" }, el("span", {}, "제어 모드"), mode),
      el("div", { class: "form-actions" }, submit,
        el("button", { type: "button", class: "btn", disabled: createPending, onclick: () => { createOpen = false; createError = ""; renderList(); } }, "취소"),
        createError ? el("span", { id: "new-job-status", role: "status" }, createError) : null));
    queueMicrotask(() => { if (!disposed && createOpen && !createDraft.target_title) title.focus(); });
    return form;
  }

  function createPanel() {
    if (!createOpen) return null;
    const kinds = el("div", { class: "seg job-kind", id: "new-work-kind", role: "group", "aria-label": "작업 종류" },
      ...[["ARCHIVE", "아카이브"], ["TEMPLATE", "Template 작업"]].map(([value, text]) => el("button", {
        type: "button", "aria-pressed": String(createDraft.kind === value), dataset: { kind: value },
        onclick: () => { createDraft.kind = value; createError = ""; renderList(); },
      }, text)));
    return el("section", { class: "panel job-create-panel", id: "new-job", "aria-label": "새 작업 생성" },
      el("div", { class: "panel-toolbar" }, el("b", { class: "panel-title" }, "새 작업 생성"), kinds),
      createDraft.kind === "ARCHIVE" ? archiveCreatePanel() : templateCreatePanel());
  }

  function renderList() {
    const previousFolderChoices = root.querySelector("#prep-folders");
    if (previousFolderChoices) ui.prepFolderScroll = previousFolderChoices.scrollTop;
    const work = [...model.work];
    const active = work.filter((item) => workLane(item) === "work");
    const queued = work.filter((item) => workLane(item) === "queue");
    const attention = work.filter((item) => ["fault", "decide"].includes(workLane(item)));
    const completed = work.filter((item) => workLane(item) === "done");
    root.replaceChildren(
      el("div", { class: "page-head" }, el("div", { class: "page-title" }, el("h1", { id: "jobs-title" }, "작업 관리")),
        el("button", { type: "button", class: "btn primary", id: "new-job-open", "aria-expanded": createOpen ? "true" : "false", onclick: () => { createOpen = !createOpen; createError = ""; renderList(); } },
          svgIcon(icons.plus), "새 작업 생성")),
      ...(createOpen ? [createPanel()] : []),
      el("div", { class: "jobs-sections" },
        workSection("진행 중", active, "진행 중인 작업 없음", "active-jobs"),
        workSection("작업 큐", queued, "대기 중인 작업 없음", "queued-jobs"),
        fold("attention-jobs", "확인 필요", attention.length, ui.jobAttentionOpen, (open) => { ui.jobAttentionOpen = open; },
          table(WORK_HEADS, attention.map(workRow), "확인 필요한 작업 없음")),
        fold("completed-jobs", "완료", completed.length, ui.jobCompletedOpen, (open) => { ui.jobCompletedOpen = open; },
          table(WORK_HEADS, completed.map(workRow), "완료 작업 없음"))));
    const currentFolderChoices = root.querySelector("#prep-folders");
    if (currentFolderChoices && ui.prepFolderScroll > 0) currentFolderChoices.scrollTop = ui.prepFolderScroll;
  }

  function renderDetail(job) {
    if (!job) {
      root.replaceChildren(
        el("div", { class: "page-head" }, el("div", { class: "page-title" }, el("h1", {}, "작업 관리"))),
        emptyState("작업을 찾을 수 없음"));
      return;
    }
    const summary = detail?.summary?.job_id === job.job_id ? detail.summary : job;
    const lane = laneOf(summary);
    const linked = linkedFolder(job.job_id);
    const facts = [
      ["작업 ID", summary.job_id], ["Template", summary.template_name], ["제어 모드", summary.control_mode],
      ["리비전", `r${summary.revision}`], ["Worker", summary.worker_id || "배정 없음"],
      ["현재 단계", summary.current_step_id], ["최근", shortTime(summary.last_event_at)],
      ["원본 위치", linked ? `${driveName(linked.drive)} / ${linked.folder.folder}` : "—"],
    ];
    const steps = detail?.summary?.job_id === job.job_id ? detail.steps || [] : [];
    const timeline = detail?.summary?.job_id === job.job_id ? detail.timeline || [] : [];
    const cancellable = !finishedSteps.has(summary.step_state);
    const confirm = ui.jobCancel === job.job_id;
    root.replaceChildren(...[
      el("div", { class: "page-head" },
        el("div", { class: "page-title" }, el("h1", { id: "job-title" }, summary.target_title || summary.job_id),
          token(`${laneText(summary)} · ${summary.step_state}`, LANE[lane]?.[1] || stateSignal(summary.step_state))),
        el("div", { class: "detail-actions" },
          cancellable && !confirm ? el("button", { type: "button", class: "btn danger", id: "cancel-job", dataset: { action: "cancel" }, onclick: () => { ui.jobCancel = job.job_id; notify(); } }, "작업 취소…") : null)),
      resultPanel(completion(job.job_id)),
      el("section", { class: "panel job-summary", id: "job-summary" },
        el("div", { class: "panel-toolbar" }, el("b", { class: "panel-title" }, "작업 정보")),
        el("div", { class: "job-facts" }, ...facts.map(([name, value]) => fact(name, value)))),
      jobMediaDisclosure(summary.job_id, media),
      confirm ? el("div", { class: "confirm-box", title: "대기 단계 즉시 취소 · 실행 중 단계는 정지·정리 확인 뒤 취소" },
        el("span", {}, "작업 취소 확인"),
        el("div", { class: "detail-actions" },
          el("button", { type: "button", class: "btn danger-solid", id: "cancel-job-confirm", dataset: { action: "cancel-confirm" }, onclick: () => cancel(summary) }, "취소 확정"),
          el("button", { type: "button", class: "btn", onclick: () => { ui.jobCancel = ""; notify(); } }, "돌아가기"))) : null,
      el("section", { class: "panel job-steps", id: "job-steps" },
        el("div", { class: "panel-toolbar" }, el("b", { class: "panel-title" }, "단계"), el("span", { class: "num muted" }, String(steps.length))),
        el("div", { class: "stage-list" }, ...(steps.length ? steps.map(stepRow) : [emptyState(loadError ? "상세 조회 실패" : "단계 없음")]))),
      timeline.length ? el("section", { class: "panel job-timeline", id: "job-timeline" },
        el("div", { class: "panel-toolbar" }, el("b", { class: "panel-title" }, "기록"), el("span", { class: "num muted" }, String(timeline.length))),
        el("div", { class: "timeline-list" }, ...timeline.map(timelineRow))) : null,
      ui.status ? el("p", { class: "status-line", role: "status", id: "command-status" }, ui.status) : null,
    ].filter(Boolean));
  }

  return {
    async update(nextRoute) {
      if (nextRoute.name === "jobs") {
        renderList();
        return;
      }
      if (nextRoute.name === "run") {
        const work = model.work.find((item) => item.work_id === nextRoute.workID);
        const key = work ? `${work.work_id}@${work.last_event_at}@${work.child_job_count}` : nextRoute.workID;
        if (key !== loadedKey) {
          loadedKey = key;
          request?.abort();
          request = new AbortController();
          try {
            workDetail = await api(`/api/v1/work/${encodeURIComponent(nextRoute.workID)}`, { signal: request.signal });
          } catch (error) {
            if (error.name === "AbortError" || disposed) return;
            workDetail = null;
          }
        }
        if (!disposed) renderRunDetail(root, workDetail, media, nextRoute);
        return;
      }
      const job = model.jobs.find((item) => item.job_id === nextRoute.jobID);
      const key = job ? `${job.job_id}@${job.revision}@${job.last_event_at}` : nextRoute.jobID;
      if (key !== loadedKey) {
        loadedKey = key;
        loadError = "";
        request?.abort();
        request = new AbortController();
        try {
          detail = await api(`/api/v1/jobs/${encodeURIComponent(nextRoute.jobID)}`, { signal: request.signal });
        } catch (error) {
          if (error.name === "AbortError" || disposed) return;
          detail = null;
          loadError = error.message;
        }
      }
      if (disposed) return;
      renderDetail(job || detail?.summary || null);
    },
    dispose() {
      disposed = true;
      request?.abort();
      for (const pending of mediaRequests.values()) pending.controller.abort();
    },
  };
}
