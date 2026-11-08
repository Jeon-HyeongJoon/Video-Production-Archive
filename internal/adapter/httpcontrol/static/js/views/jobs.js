// 작업: every persisted Job has one addressable detail route. Other screens
// link here by job_id; they do not create their own expandable Job detail.

import {
  el, token, statusIcon, label, stateSignal, stageSignal, finishedSteps, formatCount, formatPercent,
  shortTime, clockTime, meter, api, mutation, newID, emptyState,
} from "../core.js";
import { model, ui, notify, refresh, jobLane, driveName, workLane, workName } from "../store.js";

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

function table(heads, rows, empty) {
  return el("div", { class: "data-table jobs-table", role: "table" },
    el("div", { class: "data-row data-head", role: "row" }, ...heads.map((head) => el("span", { role: "columnheader" }, head))),
    ...(rows.length ? rows : [emptyState(empty)]));
}

function workHref(work) {
  return work.kind === "ARCHIVE_RUN" ? `#/runs/${encodeURIComponent(work.work_id)}` : `#/jobs/${encodeURIComponent(work.work_id)}`;
}

function workStateText(work) {
  return { FAILED: "실패", ATTENTION: "확인 필요", ACTIVE: "실행 중", QUEUED: "대기열", COMPLETED: "완료" }[work.state] || work.state;
}

function workScope(work) {
  return work.kind === "ARCHIVE_RUN"
    ? `${formatCount(work.folder_count)} 폴더 · ${formatCount(work.video_count)} 영상`
    : "독립 작업";
}

function workRow(work) {
  const lane = workLane(work);
  const pct = Math.round((work.progress || 0) * 100);
  return el("a", { class: "data-row work-row", role: "row", href: workHref(work), dataset: { workId: work.work_id, kind: work.kind, lane } },
    el("b", { class: "cell-main" }, workName(work)),
    el("span", {}, workScope(work)),
    el("span", {}, work.worker_id || "배정 없음"),
    el("span", { class: "cell-state" }, statusIcon(lane === "queue" ? "idle" : lane),
      el("span", { class: "job-state", dataset: { signal: ["fault", "decide"].includes(lane) ? lane : undefined } }, workStateText(work))),
    el("span", { class: "cell-meter" }, meter(pct, lane === "done"), el("span", { class: "num job-progress" }, `${pct}%`)),
    el("span", { class: "num muted" }, shortTime(work.last_event_at)));
}

function workSection(title, work, empty, id = "") {
  return el("section", { class: "section jobs-section", id: id || undefined },
    el("h2", { class: "section-title" }, `${title} `, el("span", { class: "num" }, String(work.length))),
    table(["작업", "범위", "Worker", "상태", "진행", "최근"], work.map(workRow), empty));
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
  el("summary", { class: "data-row job-row", role: "row", dataset: { jobId: job.job_id, lane } },
    el("span", { class: "cell-main" },
      el("a", { href: `#/jobs/${encodeURIComponent(job.job_id)}`, onclick: (event) => event.stopPropagation() }, child.folder || childRole(child.role)),
      el("small", { class: "media-hint" }, "영상 정보")),
    el("span", {}, childRole(child.role)),
    el("span", {}, job.worker_id || "배정 없음"),
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

function renderRunDetail(root, detail, media, route) {
  const summary = detail?.summary;
  if (!summary) {
    root.replaceChildren(emptyState("아카이브 작업을 찾을 수 없음"));
    return;
  }
  const lane = workLane(summary);
  const facts = [
    ["작업 ID", summary.work_id], ["폴더", formatCount(summary.folder_count)], ["영상", formatCount(summary.video_count)],
    ["Worker", summary.worker_id], ["원본", summary.source_volume_id || summary.source_storage_id], ["아카이브", summary.archive_storage_id],
    ["하위 작업", formatCount(summary.child_job_count)], ["보류", formatCount(summary.hold_count)], ["결과", formatCount(summary.result_count)],
  ];
  const folders = (detail.folder_ids || []).map((folder) => el("div", { class: "data-row", role: "row" }, el("b", { class: "cell-main" }, folder)));
  const children = detail.children || [];
  const completed = detail.completed_work || [];
  const selectedPackage = route?.query?.get("result") || "";
  root.replaceChildren(...[
    el("div", { class: "page-head" },
      el("div", { class: "page-title" }, el("h1", { id: "run-title" }, workName(summary)), token(workStateText(summary), LANE[lane]?.[1] || "idle")),
      el("a", { class: "btn", href: "#/jobs" }, "작업 관리")),
    el("section", { class: "panel job-summary", id: "run-summary" },
      el("div", { class: "panel-toolbar" }, el("b", { class: "panel-title" }, "작업 정보")),
      el("div", { class: "job-facts" }, ...facts.map(([name, value]) => fact(name, value)))),
    el("section", { class: "section jobs-section", id: "run-children" },
      el("h2", { class: "section-title" }, "하위 작업 ", el("span", { class: "num" }, String(children.length))),
      table(["대상", "단계", "Worker", "상태", "진행", "최근"], children.map((child) => childRow(child, media)), "하위 작업 없음")),
    completed.length ? el("section", { class: "section jobs-section", id: "run-results" },
      el("h2", { class: "section-title" }, "완료 결과 ", el("span", { class: "num" }, String(completed.length))),
      el("div", { class: "completed-table" },
        el("div", { class: "completed-head", "aria-hidden": "true" }, ...["결과", "작업", "원본 → 결과", "완료", "아카이브"].map((text) => el("span", {}, text))),
        ...completed.map((item) => runResultRow(item, item.archive_package_id === selectedPackage)))) : null,
    el("section", { class: "section jobs-section", id: "run-scope" },
      el("h2", { class: "section-title" }, "고정 폴더 범위 ", el("span", { class: "num" }, String(folders.length))),
      el("div", { class: "data-table scope-table", role: "table" }, ...(folders.length ? folders : [emptyState("폴더 없음")])))
  ].filter(Boolean));
}

export function mount(root, route) {
  let detail = null;
  let workDetail = null;
  let loadedKey = "";
  let loadError = "";
  let disposed = false;
  let request = null;
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

  function renderList() {
    const work = [...model.work];
    const active = work.filter((item) => workLane(item) === "work");
    const queued = work.filter((item) => workLane(item) === "queue");
    const attention = work.filter((item) => ["fault", "decide"].includes(workLane(item)));
    const completed = work.filter((item) => workLane(item) === "done");
    root.replaceChildren(
      el("div", { class: "page-head" }, el("div", { class: "page-title" }, el("h1", { id: "jobs-title" }, "작업 관리"), el("span", { class: "num muted" }, String(work.length)))),
      el("div", { class: "jobs-sections" },
        workSection("진행 중", active, "진행 중인 작업 없음", "active-jobs"),
        workSection("작업 큐", queued, "대기 중인 작업 없음", "queued-jobs"),
        fold("attention-jobs", "확인 필요", attention.length, ui.jobAttentionOpen, (open) => { ui.jobAttentionOpen = open; },
          table(["작업", "범위", "Worker", "상태", "진행", "최근"], attention.map(workRow), "확인 필요한 작업 없음")),
        fold("completed-jobs", "완료", completed.length, ui.jobCompletedOpen, (open) => { ui.jobCompletedOpen = open; },
          table(["작업", "범위", "Worker", "상태", "진행", "최근"], completed.map(workRow), "완료 작업 없음"))));
  }

  function renderDetail(job) {
    if (!job) {
      root.replaceChildren(
        el("div", { class: "page-head" }, el("div", { class: "page-title" }, el("h1", {}, "작업 관리")), el("a", { class: "btn", href: "#/jobs" }, "작업 관리")),
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
          cancellable && !confirm ? el("button", { type: "button", class: "btn danger", id: "cancel-job", dataset: { action: "cancel" }, onclick: () => { ui.jobCancel = job.job_id; notify(); } }, "작업 취소…") : null,
          el("a", { class: "btn", href: "#/jobs" }, "작업 관리"))),
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
