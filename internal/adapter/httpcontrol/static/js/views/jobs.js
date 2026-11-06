// 작업: every persisted Job has one addressable detail route. Other screens
// link here by job_id; they do not create their own expandable Job detail.

import {
  el, token, statusIcon, label, stateSignal, stageSignal, finishedSteps, formatCount, formatPercent,
  shortTime, clockTime, meter, api, mutation, newID, emptyState,
} from "../core.js";
import { model, ui, notify, refresh, jobLane, driveName } from "../store.js";

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

function orderedJobs() {
  const rank = { fault: 5, decide: 4, work: 3, resource: 2, queue: 1, done: 0 };
  return [...model.jobs].sort((left, right) => {
    const lane = rank[laneOf(right)] - rank[laneOf(left)];
    if (lane) return lane;
    const time = new Date(right.last_event_at || 0) - new Date(left.last_event_at || 0);
    return time || left.job_id.localeCompare(right.job_id);
  });
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

function jobRow(job) {
  const lane = laneOf(job);
  const pct = Math.round((job.progress || 0) * 100);
  return el("a", { class: "data-row job-row", role: "row", href: `#/jobs/${encodeURIComponent(job.job_id)}`, dataset: { jobId: job.job_id, lane } },
    el("b", { class: "cell-main" }, job.target_title || job.job_id),
    el("span", {}, job.template_name || "—"),
    el("span", {}, job.worker_id || "배정 없음"),
    el("span", { class: "cell-state" }, statusIcon(LANE[lane]?.[1] || "idle"),
      el("span", { class: "job-state", dataset: { signal: lane === "fault" || lane === "decide" ? lane : undefined } }, laneText(job))),
    el("span", { class: "cell-meter" }, meter(pct, lane === "done"), el("span", { class: "num job-progress" }, `${pct}%`)),
    el("span", { class: "num muted" }, shortTime(job.last_event_at)));
}

function completedRow(job) {
  const found = completion(job.job_id);
  const item = found?.item;
  const trace = item ? [
    item.archive_display_name, `패키지 ${item.archive_package_id}`, `작업 ${item.job_id}`, `실행 ${item.archive_run_id}`,
  ].filter(Boolean).join("\n") : `작업 ${job.job_id}`;
  return el("a", {
    class: "completed-row", href: `#/jobs/${encodeURIComponent(job.job_id)}`,
    dataset: { packageId: item?.archive_package_id || "", jobId: job.job_id }, title: trace,
  },
  el("span", { class: "completed-result" },
    el("b", {}, item?.archive_video_file_name || job.target_title || job.job_id),
    el("small", {}, item?.folder || job.job_id)),
  el("span", { class: "completed-kind" }, item ? label("disposition", item.disposition) : job.template_name || "작업 완료",
    el("small", {}, item?.profile_reference || (item ? "" : "결과 기록 없음"))),
  el("span", { class: "completed-source num" }, item ? `${formatCount(item.source_video_count)} → 1` : "—"),
  el("span", { class: "completed-time num" }, shortTime(item?.completed_at || job.last_event_at)),
  el("span", { class: "completed-place" },
    token(item ? (item.archive_state === "COMMITTED" ? "확정됨" : "보관됨") : "작업 완료", item?.archive_state === "COMMITTED" || !item ? "done" : "idle"),
    item?.archive_storage_id ? el("small", {}, item.archive_storage_id) : null));
}

function jobSection(title, jobs, empty, id = "") {
  return el("section", { class: "section jobs-section", id: id || undefined },
    el("h2", { class: "section-title" }, `${title} `, el("span", { class: "num" }, String(jobs.length))),
    table(["작업", "Template", "Worker", "상태", "진행", "최근"], jobs.map(jobRow), empty));
}

function completedResults(jobs) {
  return el("section", { class: "section jobs-section", "aria-label": "보관 결과" },
    el("h3", { class: "section-title" }, "보관 결과 ", el("span", { class: "num" }, String(jobs.length))),
    el("div", { class: "completed-table" },
      el("div", { class: "completed-head", "aria-hidden": "true" }, ...["결과", "작업", "원본 → 결과", "완료", "아카이브"].map((text) => el("span", {}, text))),
      ...(jobs.length ? jobs.map(completedRow) : [emptyState("완료 내역 없음")])));
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
  return el("section", { class: "panel job-result", id: "job-result", dataset: { packageId: item.archive_package_id } },
    el("div", { class: "panel-toolbar" }, el("b", { class: "panel-title" }, "완료 결과"),
      token(item.archive_state === "COMMITTED" ? "확정됨" : "보관됨", item.archive_state === "COMMITTED" ? "done" : "idle")),
    el("div", { class: "job-result-grid" },
      fact("결과 영상", item.archive_video_file_name || item.archive_package_id, "wide"),
      fact("원본 폴더", item.folder),
      fact("처리 방식", label("disposition", item.disposition)),
      fact("원본 → 결과", source),
      fact("완료", shortTime(item.completed_at)),
      fact("아카이브", item.archive_storage_id),
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

export function mount(root, route) {
  let detail = null;
  let loadedKey = "";
  let loadError = "";
  let disposed = false;
  let request = null;
  ui.jobCancel = "";
  ui.status = "";

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
    const jobs = orderedJobs();
    const active = jobs.filter((job) => laneOf(job) === "work");
    const queued = jobs.filter((job) => ["resource", "queue"].includes(laneOf(job)));
    const attention = jobs.filter((job) => ["fault", "decide"].includes(laneOf(job)));
    const completed = jobs.filter((job) => laneOf(job) === "done");
    const completedWithResults = completed.filter((job) => completion(job.job_id));
    const completedWithoutResults = completed.filter((job) => !completion(job.job_id));
    const completedBody = [
      completedResults(completedWithResults),
      completedWithoutResults.length ? jobSection("기타 완료 작업", completedWithoutResults, "완료 작업 없음") : null,
    ].filter(Boolean);
    root.replaceChildren(
      el("div", { class: "page-head" }, el("div", { class: "page-title" }, el("h1", { id: "jobs-title" }, "작업 관리"), el("span", { class: "num muted" }, String(jobs.length)))),
      el("div", { class: "jobs-sections" },
        jobSection("진행 중", active, "진행 중인 작업 없음", "active-jobs"),
        jobSection("작업 큐", queued, "대기 중인 작업 없음", "queued-jobs"),
        fold("attention-jobs", "확인 필요", attention.length, ui.jobAttentionOpen, (open) => { ui.jobAttentionOpen = open; },
          table(["작업", "Template", "Worker", "상태", "진행", "최근"], attention.map(jobRow), "확인 필요한 작업 없음")),
        fold("completed-jobs", "완료", completed.length, ui.jobCompletedOpen, (open) => { ui.jobCompletedOpen = open; }, ...completedBody)));
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
    },
  };
}
