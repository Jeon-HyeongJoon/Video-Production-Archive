// 아카이브 작업 화면: one ArchiveRun a person requested. It shows the drive the
// work runs on, its progress, the problems of its own folders, the folders
// running now, its child Jobs (video facts open only on request), its results
// and, folded, the fixed folder scope.

import { html, useComputed } from "../lib.js";
import { label, formatCount, shortTime, dayTime } from "../core.js";
import {
  ui, store, workOf, workDetail, workDrive, workPhase, workTitle, workScopeText, workText, workKind, driveProblems, folderKey, folderState,
  folderProgress, jobLane, jobMedia,
} from "../store.js";
import { route } from "../router.js";
import { StatusIcon, Glyph, GLYPH, Chevron, Swap, Meter, Fold, Presence, Crumbs, Fact, Empty, Skeleton, useLinger } from "../ui.js";
import { DriveOverview, Problems, SttButton, StatusLine, titleShared, useOpenProblem } from "./drive.js";
import { MediaTable } from "./job.js";

const ROLE = { INSPECTION: "검사", FOLDER: "폴더 처리", TRANSCRIPTION: "STT" };
const LANE_TEXT = { fault: "실패", decide: "사람 대기", work: "실행 중", resource: "자원 대기", queue: "대기열", done: "완료" };
const LANE_KIND = { fault: "fault", decide: "decide", work: "work", resource: "work", queue: "idle", done: "done" };

const archiveBadge = (state) => html`<b class="archive-badge" data-state=${state}>${state === "COMMITTED" ? "확정됨" : "보관됨"}</b>`;

function RunFacts({ summary }) {
  const source = summary.source_volume_id || summary.source_storage_id || "—";
  return html`<div class="facts run-facts">
    <${Fact} label="원본 → 아카이브" value=${`${source} → ${summary.archive_storage_id || "—"}`} />
    <${Fact} label="폴더" value=${formatCount(summary.folder_count)} />
    <${Fact} label="영상" value=${formatCount(summary.video_count)} />
  </div>`;
}

// WorkOverview stands in for the drive overview once the work is no longer
// its drive's current run: the stored state and progress only.
function WorkOverview({ summary, trace }) {
  const phase = workPhase(summary);
  return html`<section class="overview" id="run-summary" aria-label="개요" title=${trace}>
    <div class="overview-head">
      <span class="state" data-fact="state" data-phase=${phase}><${StatusIcon} kind=${workKind(summary)} /><${Swap} text=${workText(summary)} /></span>
      ${summary.replacement_work_id
        ? html`<span class="progress-fact"><span class="muted">후속 작업</span><b class="num">연결됨</b></span>`
        : html`<span class="progress-fact"><span class="muted">진행</span><b class="num">${Math.round((summary.progress || 0) * 100)}%</b></span>`}
    </div>
    ${summary.replacement_work_id ? null : html`<div class="overview-meter"><${Meter} value=${summary.progress || 0} done=${summary.state === "COMPLETED"} label="진행" /></div>`}
    <${RunFacts} summary=${summary} />
  </section>`;
}

// FailureHistory keeps the reason at the ArchiveRun level. The child Job link
// remains available for deeper evidence, but the person does not need to open
// it merely to learn why the parent work ended.
function FailureHistory({ failures, summary }) {
  if (!failures?.length) return null;
  return html`<section class="section" id="run-failures" aria-label="실패 기록">
    <div class="section-head"><h2 class="section-title">실패 기록${" "}<span class="num">${formatCount(failures.length)}</span></h2>
      ${summary.replacement_work_id ? html`<a class="text-btn" href=${`#/runs/${encodeURIComponent(summary.replacement_work_id)}`}>새 작업으로 넘김<${Glyph} d=${GLYPH.next} /></a>` : null}
    </div>
    <div class="card-list">
      ${failures.map((failure) => html`<a class="problem-panel failure-record" key=${failure.job_id} data-signal="fault" href=${`#/jobs/${encodeURIComponent(failure.job_id)}`}>
        <span class="problem-head"><${StatusIcon} kind="fault" /><span class="problem-line"><b>${failure.folder || "원본 검사"}</b><span>${label("stage", failure.step_id)} · ${failure.error_class || "처리 실패"}</span></span><span class="token">실패</span><${Glyph} d=${GLYPH.next} /></span>
        ${failure.log_reference ? html`<small class="muted failure-reference">${failure.log_reference}</small>` : null}
      </a>`)}
    </div>
  </section>`;
}

// RunningJobs are the folders working now, the same rows as the 진행 중 lane.
function RunningJobs({ drive: current }) {
  const running = current.scope.filter((name) => folderState(store.folders.get(folderKey(current.workspace_drive_id, name))?.value || {}).signal === "work");
  const counts = { queued: 0, done: 0 };
  for (const folder of current.folders) {
    const state = folderState(folder);
    if (state.word === "대기열") counts.queued += 1;
    else if (state.signal === "done") counts.done += 1;
  }
  return html`<section class="section" id="run-running" aria-label="진행 중 작업">
    <div class="section-head">
      <h2 class="section-title">진행 중 작업${" "}<span class="num">${running.length}</span></h2>
      <small class="muted">대기 ${formatCount(counts.queued)} · 끝남 ${formatCount(counts.done)}</small>
    </div>
    <div class="stack">
      ${running.length
        ? html`<${Presence} items=${running} keyOf=${(name) => name}>${(name) => html`<${FolderJobRow} driveID=${current.workspace_drive_id} name=${name} />`}<//>`
        : html`<${Empty} text="진행 중인 폴더 없음" />`}
    </div>
  </section>`;
}

function FolderJobRow({ driveID, name }) {
  const folder = store.folders.get(folderKey(driveID, name))?.value;
  if (!folder) return null;
  const stage = folder.transcription_job_id ? "2단계 전사" : "1단계 정리";
  // A resource wait shows as waiting; its reason stays in the folder's steps.
  const now = (folder.stages || []).find((item) => !["COMPLETED", "FAILED", "CANCELLED"].includes(item.state));
  const word = now ? `${label("stage", now.stage_id)} ${now.state === "WAITING" ? "대기" : "중"}` : "진행 중";
  const href = folder.job_id ? `#/jobs/${encodeURIComponent(folder.job_id)}` : `#/drives/${encodeURIComponent(driveID)}?folder=${encodeURIComponent(name)}`;
  return html`<a class="job-row" href=${href} data-shared="row" data-folder-id=${name} data-job-id=${folder.job_id || null}>
    <span class="job-name"><b>${name}</b><small>${stage} · 영상 ${formatCount(folder.video_count)} · ${label("disposition", folder.disposition)}</small></span>
    <span class="state"><${StatusIcon} kind="work" /><${Swap} text=${word} /></span>
    <span class="job-progress"><${Meter} value=${folderProgress(folder)} label="진행" /></span>
    <span class="job-time">${folder.last_event_at ? dayTime(folder.last_event_at) : ""}</span>
  </a>`;
}

// ChildWork is one child Job. Its video facts are asked for only when the row
// opens, and kept for the rest of the visit.
function ChildWork({ child }) {
  const job = child.job;
  const open = useComputed(() => ui.expanded.value.has(`media:${job.job_id}`)).value;
  const shown = useLinger(open);
  const lane = jobLane(job) || "done";
  const pct = Math.round((job.progress || 0) * 100);
  const toggle = () => {
    const next = new Set(ui.expanded.peek());
    if (!next.delete(`media:${job.job_id}`)) next.add(`media:${job.job_id}`);
    ui.expanded.value = next;
  };
  return html`<div class="child-work" data-job-id=${job.job_id} data-lane=${lane} data-open=${open ? "true" : "false"}>
    <div class="child-line">
      <button type="button" class="child-row" aria-expanded=${open ? "true" : "false"} title=${`${job.job_id}\nWorker ${job.worker_id || "배정 없음"}`} onClick=${toggle}>
        <b>${child.folder || ROLE[child.role] || child.role}</b>
        <span class="muted">${ROLE[child.role] || child.role} · 영상 정보</span>
        <span class="state"><${StatusIcon} kind=${LANE_KIND[lane]} />${LANE_TEXT[lane] || label("step", job.step_state)}</span>
        <span class="cell-meter"><${Meter} value=${job.progress || 0} done=${lane === "done"} label="진행" /><span class="num">${pct}%</span></span>
        <span class="job-time child-recent">${dayTime(job.last_event_at)}</span>
        <${Chevron} />
      </button>
      <a class="icon-btn child-link" href=${`#/jobs/${encodeURIComponent(job.job_id)}`} data-shared="child" aria-label=${`${child.folder || job.job_id} 작업 상세`} title="작업 상세"><${Glyph} d=${GLYPH.next} /></a>
    </div>
    <${Fold} open=${open}>${shown ? html`<${ChildMedia} jobID=${job.job_id} />` : null}<//>
  </div>`;
}

function ChildMedia({ jobID }) {
  const answer = jobMedia(jobID);
  if (answer.value === undefined && !answer.error) return html`<div class="job-media-body"><${Skeleton} n=${2} cls="skel-result" /></div>`;
  if (answer.error) return html`<div class="job-media-body"><${Empty} text="영상 정보 조회 실패" /></div>`;
  return html`<div class="job-media-body"><${MediaTable} items=${answer.value} /></div>`;
}

function Results({ items, selected }) {
  if (!items.length) return null;
  return html`<section class="section" id="run-results" aria-label="결과">
    <h2 class="section-title">결과${" "}<span class="num">${formatCount(items.length)}</span></h2>
    <div class="results">
      ${items.map((item) => html`<div class="result-row run-result" key=${item.archive_package_id} data-package-id=${item.archive_package_id} data-job-id=${item.job_id}
        data-selected=${item.archive_package_id === selected ? "true" : null}>
        <span class="result-name"><b>${item.archive_video_file_name || item.archive_package_id}</b><small>${item.folder} · ${shortTime(item.completed_at)}</small></span>
        <span class="result-kind">${label("disposition", item.disposition)}${item.profile_reference ? html`<small>${item.profile_reference}</small>` : null}</span>
        <span class="num result-count">${formatCount(item.source_video_count)} → 1</span>
        <span class="result-place">${archiveBadge(item.archive_state)}<small>${item.archive_storage_id}</small>
          <small class="archive-relative-path" title=${item.archive_relative_path || ""}>${item.archive_relative_path || "—"}</small></span>
      </div>`)}
    </div>
  </section>`;
}

// The fixed scope starts folded: it is the record of what was asked for, not
// something to act on.
function Scope({ names, current }) {
  const open = ui.scopeOpen.value;
  return html`<section class="lane" id="run-scope" data-open=${open ? "true" : "false"} aria-label="고정 폴더 범위">
    <div class="lane-head"><button type="button" class="lane-toggle" aria-expanded=${open ? "true" : "false"} onClick=${() => { ui.scopeOpen.value = !open; }}>
      <${Chevron} />고정 폴더 범위${" "}<span class="num">${formatCount(names.length)}</span></button></div>
    <${Fold} open=${open}>
      <div class="table scope-table" role="table">
        ${names.length ? names.map((name) => {
          const folder = current ? store.folders.get(folderKey(current.workspace_drive_id, name))?.value : null;
          const state = folder ? folderState(folder) : null;
          return html`<div class="trow scope-row" role="row" key=${name} data-folder-id=${name}>
            <b>${name}</b>${state ? html`<span class="state"><${StatusIcon} kind=${folder.archive_state ? "done" : state.signal} />${folder.archive_state ? (folder.archive_state === "COMMITTED" ? "확정됨" : "보관됨") : state.word}</span>` : html`<span></span>`}
          </div>`;
        }) : html`<${Empty} text="폴더 없음" />`}
      </div>
    <//>
  </section>`;
}

export function RunScreen({ id }) {
  const work = workOf(id);
  const answer = workDetail(id);
  const detail = answer.value;
  const summary = detail?.summary || work;
  const current = summary ? workDrive(summary) : null;
  useOpenProblem(current);
  if (!summary) {
    return answer.value === undefined && !answer.error
      ? html`<div class="boot-view"><span class="skel boot-title"></span><span class="skel-card boot-card"></span></div>`
      : html`<${Empty} text="아카이브 작업을 찾을 수 없음" />`;
  }
  const title = workTitle(summary);
  const trace = [`작업 ID ${summary.work_id}`, `Worker ${summary.worker_id || "배정 없음"}`, `하위 작업 ${formatCount(summary.child_job_count)}`, `결과 ${formatCount(summary.result_count)}`].join("\n");
  const children = detail?.children || [];
  const results = detail?.completed_work || [];
  const scope = detail?.folder_ids || current?.scope || [];
  const holdLink = !current && summary.hold_count > 0;
  return [
    html`<${Crumbs} key="crumbs" items=${[["작업 관리", "#/jobs"], [title]]} />`,
    html`<div key="head" class="page-head">
      <div class="page-title">
        <h1 id="run-title" class=${titleShared(route.value.full) ? "shared" : null}>${title}</h1>
        <small class="sub"><span>${workScopeText(summary)}</span><span>· 요청 ${shortTime(summary.created_at)}</span><i class="faint-id">${summary.work_id}</i></small>
      </div>
      <div class="page-actions">
        ${summary.source_volume_id ? html`<a class="btn" href=${`#/drives/${encodeURIComponent(summary.source_volume_id)}`}>드라이브 열기</a>` : null}
        ${current ? html`<${SttButton} drive=${current} />` : null}
      </div>
    </div>`,
    current
      ? html`<${DriveOverview} key="overview" drive=${current} id="run-summary" title=${trace} facts=${html`<${RunFacts} summary=${summary} />`} />`
      : html`<${WorkOverview} key="overview" summary=${summary} trace=${trace} />`,
    html`<${StatusLine} key="status" />`,
    html`<${FailureHistory} key="failures" failures=${detail?.failures || []} summary=${summary} />`,
    current ? html`<${Problems} key="problems" drive=${current} items=${driveProblems(current)} id="run-attention" />` : null,
    holdLink ? html`<section key="holds" class="section" id="run-attention" aria-label="확인 필요">
      <h2 class="section-title">확인 필요${" "}<span class="num">1</span></h2>
      <a class="problem-panel attention-link" data-kind="hold" data-signal="decide" href=${`#/drives/${encodeURIComponent(summary.source_volume_id)}`}>
        <span class="problem-head"><${StatusIcon} kind="decide" /><span class="problem-line"><b>분류 확인 필요</b><span>${formatCount(summary.hold_count)} 폴더 · 드라이브에서 판정</span></span><span></span><${Glyph} d=${GLYPH.next} /></span>
      </a>
    </section>` : null,
    current && ["stage1", "stage2"].includes(workPhase(summary)) ? html`<${RunningJobs} key="running" drive=${current} />` : null,
    html`<section key="children" class="section" id="run-children" aria-label="하위 작업">
      <h2 class="section-title">하위 작업${" "}<span class="num">${formatCount(children.length)}</span></h2>
      <div class="child-list">
        ${detail === undefined ? html`<${Skeleton} n=${Math.min(summary.child_job_count || 1, 6)} cls="skel-child" />`
          : children.length ? children.map((child) => html`<${ChildWork} key=${child.job.job_id} child=${child} />`) : html`<${Empty} text="하위 작업 없음" />`}
      </div>
    </section>`,
    html`<${Results} key="results" items=${results} selected=${route.value.query.get("result") || ""} />`,
    html`<${Scope} key="scope" names=${scope} current=${current} />`,
  ];
}
