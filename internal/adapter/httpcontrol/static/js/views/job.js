// 작업 상세: every persisted Job has one addressable detail route. A folder's
// Job names the folder and the work it belongs to; a failed one offers a new
// work for that folder instead of a retry in place. Video facts load only
// when their panel opens.

import { html, useState } from "../lib.js";
import { label, stageSignal, stateSignal, finishedSteps, formatCount, formatPercent, shortTime, clockTime, dateOnly, mutation, newID } from "../core.js";
import {
  ui, stale, refresh, jobOf, jobDetail, jobMedia, jobLane, linkedFolder, completion, folderProblem, isRunning, openNewWork, driveName,
} from "../store.js";
import { route, go } from "../router.js";
import { StatusIcon, Chevron, Swap, Meter, Fold, Crumbs, Fact, Empty, Skeleton, useLinger } from "../ui.js";
import { StatusLine, titleShared } from "./drive.js";

const LANE = { fault: ["실패", "fault"], decide: ["직접 확인 필요", "decide"], work: ["처리 중", "work"], resource: ["작업 PC 여유 대기", "work"], queue: ["시작 대기", "idle"], done: ["완료", "done"] };

export function laneText(job) {
  const lane = jobLane(job) || "done";
  if (lane === "decide") return job.wait_reason ? `${label("wait", job.wait_reason)} 필요` : "직접 확인 필요";
  return LANE[lane]?.[0] || label("step", job.step_state);
}

function mediaDate(value) {
  if (!value || String(value).startsWith("0001-01-01")) return "날짜 없음";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "날짜 없음";
  return dateOnly(date);
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

// MediaTable lists one Job's source videos: the person-facing date and name
// first, then length, size and technical facts.
export function MediaTable({ items }) {
  if (!items.length) return html`<${Empty} text="연결된 영상 정보 없음" />`;
  return html`<div class="video-table" role="table">
    <div class="media-row media-head" role="row">${["원본", "길이 · 용량", "기술정보", "Profile"].map((text) => html`<span role="columnheader" key=${text}>${text}</span>`)}</div>
    ${items.map((item) => {
      const video = [item.container, item.video_codec, item.width && item.height ? `${item.width}×${item.height}` : "", mediaRate(item.video_bitrate_bps || item.bitrate_bps)].filter(Boolean).join(" · ") || "—";
      const audio = [item.audio_codec, item.audio_sample_rate_hz ? `${Math.round(item.audio_sample_rate_hz / 1000)} kHz` : "", item.audio_channels ? `${item.audio_channels}ch` : ""].filter(Boolean).join(" · ") || "—";
      return html`<div class="media-row video-row" role="row" key=${item.original_asset_id} data-asset-id=${item.original_asset_id}>
        <span><b>${mediaDate(item.source_created_at)} · ${item.original_name || "이름 없음"}</b><small title=${item.relative_path || ""}>${item.relative_path || "—"}</small></span>
        <span><b class="num">${mediaDuration(item.duration_millis)}</b><small class="num">${mediaBytes(item.size_bytes)}</small></span>
        <span><b>${video}</b><small>${audio}</small></span>
        <span title=${item.profile_reference || ""}><span>${item.profile_reference || "—"}</span></span>
      </div>`;
    })}
  </div>`;
}

function MediaBody({ jobID }) {
  const answer = jobMedia(jobID);
  if (answer.value === undefined && !answer.error) return html`<${Skeleton} n=${2} cls="skel-result" />`;
  if (answer.error) return html`<${Empty} text="영상 정보 조회 실패" />`;
  return html`<${MediaTable} items=${answer.value} />`;
}

function MediaPanel({ jobID }) {
  const [open, setOpen] = useState(false);
  const shown = useLinger(open);
  return html`<section class="panel" id="job-media" aria-label="영상 정보" data-open=${open ? "true" : "false"}>
    <div class="panel-toolbar"><b>영상 정보</b>
      <button type="button" class="text-btn media-toggle" aria-expanded=${open ? "true" : "false"} onClick=${() => setOpen(!open)}>${open ? "접기" : "펼치기"}<${Chevron} /></button></div>
    <${Fold} open=${open}>${shown ? html`<${MediaBody} jobID=${jobID} />` : null}<//>
  </section>`;
}

function ResultPanel({ found }) {
  if (!found) return null;
  const { item, drive } = found;
  const href = `#/runs/${encodeURIComponent(item.archive_run_id)}?result=${encodeURIComponent(item.archive_package_id)}`;
  return html`<section class="panel job-result" id="job-result" aria-label="저장된 결과" data-package-id=${item.archive_package_id}>
    <div class="panel-toolbar"><b>저장된 결과</b><b class="archive-badge" data-state=${item.archive_state}>${item.archive_state === "COMMITTED" ? "보존 완료" : "저장 완료"}</b>
      <a class="btn btn-sm archive-result-link" href=${href} data-shared="row">저장 결과 보기</a></div>
    <div class="facts">
      <${Fact} label="결과 영상" value=${item.archive_video_file_name || item.archive_package_id} />
      <${Fact} label="원본 폴더" value=${item.folder} />
      <${Fact} label="처리 방식" value=${label("disposition", item.disposition)} />
      <${Fact} label="원본 → 결과" value=${`${formatCount(item.source_video_count)} → 1`} />
      <${Fact} label="완료" value=${shortTime(item.completed_at)} />
      <${Fact} label="저장 위치" value=${item.archive_storage_id} />
      <${Fact} label="결과 폴더" value=${item.archive_relative_path || "—"} />
      <${Fact} label="원본 드라이브" value=${driveName(drive)} />
    </div>
  </section>`;
}

// ProblemAction is the one next step a folder's problem asks for here.
function ProblemAction({ linked }) {
  if (!linked) return null;
  const problem = folderProblem(linked.folder);
  if (problem?.kind === "hold") {
    return html`<a class="btn" href=${`#/drives/${encodeURIComponent(linked.drive.workspace_drive_id)}?folder=${encodeURIComponent(linked.folder.folder)}`}>처리 방법 선택</a>`;
  }
  if (problem?.kind !== "fault") return null;
  const redo = () => {
    openNewWork(linked.drive.workspace_drive_id, [linked.folder.folder], linked.drive.archive_storage_id || "");
    go("#/create");
  };
  return html`<button type="button" class="btn" data-action="redo" disabled=${isRunning(linked.drive) || stale.value} title=${isRunning(linked.drive) ? "현재 작업이 끝나면 다시 처리할 수 있음" : ""} onClick=${redo}>이 폴더 다시 처리</button>`;
}

function stepLine(step) {
  return [
    step.state === "WAITING" && step.wait_reason ? `WAITING · ${step.wait_reason}` : step.state,
    formatPercent(Math.round((step.progress || 0) * 100)), step.worker_id, step.error_class,
    step.log_reference ? `로그 ${step.log_reference}` : "",
  ].filter(Boolean).join(" · ");
}

export function JobScreen({ id }) {
  const job = jobOf(id);
  const answer = jobDetail(id);
  const detail = answer.value?.summary?.job_id === id ? answer.value : null;
  const summary = detail?.summary || job;
  const [pending, setPending] = useState(false);
  if (!summary) {
    return answer.value === undefined && !answer.error
      ? html`<div class="boot-view"><span class="skel boot-title"></span><span class="skel-card boot-card"></span></div>`
      : [html`<div key="head" class="page-head"><div class="page-title"><h1>작업</h1></div></div>`, html`<${Empty} key="empty" text="작업을 찾을 수 없음" />`];
  }
  const lane = jobLane(summary) || "done";
  const linked = linkedFolder(id);
  const steps = detail?.steps || [];
  const timeline = detail?.timeline || [];
  const cancellable = !finishedSteps.has(summary.step_state);
  const confirming = ui.cancelConfirm.value === id;
  const title = summary.target_title || summary.job_id;
  const cancel = async () => {
    setPending(true);
    try {
      await mutation(`/api/v1/jobs/${encodeURIComponent(summary.job_id)}/cancel`, {
        command_id: newID("COMMAND"), idempotency_key: newID("CANCEL"), reason: "담당자 취소", expected_revision: summary.revision,
      });
      ui.cancelConfirm.value = "";
      ui.status.value = `${summary.job_id} 취소 요청`;
      await refresh();
    } catch (error) {
      ui.status.value = error.message;
    } finally {
      setPending(false);
    }
  };
  const crumbs = [["작업", "#/jobs"]];
  if (linked?.drive.archive_run_id) crumbs.push([driveName(linked.drive), `#/runs/${encodeURIComponent(linked.drive.archive_run_id)}`]);
  crumbs.push([title]);
  return [
    html`<${Crumbs} key="crumbs" items=${crumbs} />`,
    html`<div key="head" class="page-head">
      <div class="detail-title">
        <h1 id="job-title" class=${titleShared(route.value.full) ? "shared" : null}>${title}</h1>
        <span class="state job-head-state" data-signal=${LANE[lane]?.[1]} title=${summary.step_state}><${StatusIcon} kind=${LANE[lane]?.[1] || stateSignal(summary.step_state)} /><${Swap} text=${laneText(summary)} /></span>
      </div>
      <div class="page-actions">
        <${ProblemAction} linked=${linked} />
        ${cancellable && !confirming ? html`<button type="button" class="btn danger" id="cancel-job" data-action="cancel" disabled=${stale.value} onClick=${() => { ui.cancelConfirm.value = id; }}>작업 취소…</button>` : null}
      </div>
    </div>`,
    html`<${Fold} key="confirm" open=${confirming}>
      <div class="confirm"><span>작업 취소 확인 · 대기 중인 단계는 바로 취소하고, 실행 중인 단계는 정리한 뒤 취소합니다</span>
        <button type="button" class="btn danger-solid" id="cancel-job-confirm" data-action="cancel-confirm" disabled=${pending || stale.value} onClick=${cancel}>${pending ? "요청 보냄…" : "취소 확정"}</button>
        <button type="button" class="btn" onClick=${() => { ui.cancelConfirm.value = ""; }}>돌아가기</button>
      </div>
    <//>`,
    html`<${StatusLine} key="status" />`,
    html`<${ResultPanel} key="result" found=${completion(id)} />`,
    html`<section key="facts" class="panel" id="job-summary" aria-label="작업 정보"><div class="panel-toolbar"><b>작업 정보</b></div><div class="facts">
      <${Fact} label="작업 ID" value=${html`<span class="faint-id">${summary.job_id}</span>`} />
      <${Fact} label="작업 유형" value=${html`${label("template", summary.template_name)} <span class="faint-id">${summary.template_name}</span>`} />
      <${Fact} label="리비전" value=${`r${summary.revision}`} />
      <${Fact} label="작업 PC" value=${summary.worker_id || "배정 없음"} />
      <${Fact} label="현재 단계" value=${label("stage", summary.current_step_id)} />
      <${Fact} label="최근" value=${shortTime(summary.last_event_at)} />
      <${Fact} label="원본 위치" value=${linked ? `${driveName(linked.drive)} / ${linked.folder.folder}` : "—"} />
    </div></section>`,
    html`<section key="steps" class="panel" id="job-steps" aria-label="처리 과정">
      <div class="panel-toolbar"><b>처리 과정</b><span class="muted num">${steps.length}</span></div>
      <ol class="step-list">
        ${steps.length ? steps.map((step) => {
          const signal = stageSignal({ state: step.state, wait_reason: step.wait_reason });
          const kind = step.state === "COMPLETED" ? "done" : signal;
          return html`<li class="step-item" key=${step.id} data-step=${step.id} data-signal=${signal} data-state=${kind}>
            <${StatusIcon} kind=${kind} /><span><b>${label("stage", step.id)}</b><small class="stage-state">${stepLine(step)}</small></span>
            ${step.state === "RUNNING" ? html`<${Meter} value=${step.progress || 0} label="진행" />` : html`<span></span>`}
          </li>`;
        }) : html`<${Empty} text=${answer.error ? "상세 조회 실패" : answer.value === undefined ? "불러오는 중" : "단계 없음"} />`}
      </ol>
    </section>`,
    timeline.length ? html`<section key="timeline" class="panel" id="job-timeline" aria-label="기록">
      <div class="panel-toolbar"><b>기록</b><span class="muted num">${timeline.length}</span></div>
      <ol class="timeline">${timeline.map((item, index) => html`<li key=${index} data-kind=${item.kind}><time>${clockTime(item.occurred_at, true)}</time><${StatusIcon} kind="done" /><span><b>${item.kind}</b> ${item.summary || item.reference || "—"}</span></li>`)}</ol>
    </section>` : null,
    html`<${MediaPanel} key="media" jobID=${summary.job_id} />`,
  ];
}
