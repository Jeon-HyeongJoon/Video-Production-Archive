// 드라이브 상세: one source drive. Its overview holds the phase and quiet video
// progress over the stage summary of its ArchiveRun; the step nodes open only
// on request. Below come the problems a person resolves here, the completed
// results and a table with one name-and-state row per folder; a folder's
// disposition, videos and steps open only on request. The pieces are shared
// with the archive work screen.

import { html, useEffect, useLayoutEffect, useRef, useState, useComputed, batch } from "../lib.js";
import {
  label, stageSignal, stateSignal, activeStates, humanWait, formatCount, formatBytes, formatSize, shortTime, clockTime, mutation, newID,
} from "../core.js";
import {
  store, ui, stale, refresh, driveView, folderKey, folderProblem, folderState, folderSignal, folderProgress, driveProblems,
  drivePhase, driveName, driveProgress, driveConnection, folderDiscoveryText, isRunning, jobDetail, openNewWork, driveOutcome,
} from "../store.js";
import { DiagnoseFailure } from "../agent.js";
import { route, go, onRepeat, shared } from "../router.js";
import { StatusIcon, Glyph, GLYPH, Chevron, Swap, Meter, Fold, Presence, Crumbs, Fine, Empty, useLinger } from "../ui.js";

const WORK_CHOICE_LABELS = {
  "HOLD:0": ["hold", "보류 유지"],
  "EXCLUDE_FROM_RUN:0": ["exclude", "이번 작업에서 제외 · 원본 유지"],
  "RENDER_RECORDINGS:6000": ["render-6000", "영상별 변환 · 6Mbps"],
  "RENDER_RECORDINGS:20000": ["render-20000", "영상별 변환 · 20Mbps"],
  "COMPILE_SOURCE_SEQUENCE:0": ["source", "여러 영상 합치기"],
  "ORGANIZE_AS_IS:0": ["as-is", "원본 그대로 저장"],
};
const DATE = /(\d{4})-(\d{2})-(\d{2})/;
const pad = (value) => String(value).padStart(2, "0");

// ─── Overview: phase, archived video share and the stage summary ───────────

const INFO = {
  classification: ["영상 기술정보와 정책으로 처리 방식 결정 · 원본 그대로 저장 / 영상별 변환 / 여러 영상 합치기 / 직접 선택", "모든 폴더"],
  markers: ["짧은 영상의 순서와 구간 표시 고정 · 합칠 영상 입력", "여러 영상 합치기"],
  render: ["1080p 변환 · 영상별 6Mbps(선택 시 20Mbps) · 여러 영상은 20Mbps로 합침", "영상별 변환 · 여러 영상 합치기"],
  qc: ["결과 영상을 원본 기술정보·변환 설정과 대조하는 검사", "영상 게시가 결정된 폴더"],
  archive: ["결과 저장 위치에 복사 · checksum 검증 뒤 저장 완료", "영상 게시가 결정된 폴더"],
  gate: ["영상 정리·저장에 남은 작업이 없으면 Control이 자막·텍스트 작업을 자동으로 이어서 시작", "저장 완료된 원본 그대로 저장 · 영상별 변환"],
  audio: ["음성 인식용 16kHz mono 오디오 준비", "자막·텍스트 대상"],
  transcript: ["음성을 시간 구간 텍스트로 인식", "자막·텍스트 대상"],
  subtitle: ["인식한 시간 구간으로 SRT 자막 생성 · 텍스트 파일과 동시 진행", "자막·텍스트 대상"],
  transcript_text: ["인식한 내용을 시간순으로 합친 텍스트 파일 생성 · 자막과 동시 진행", "자막·텍스트 대상"],
  add: ["음성 인식 결과·자막·텍스트 파일을 저장된 결과의 새 버전으로 게시", "자막·텍스트 대상"],
  catalog: ["영상 목록 파일 · 원본 영상마다 1행 · 영상 저장과 자막·텍스트 생성 뒤 갱신", "모든 원본 영상"],
};
const GROUPS = [
  ["one", "영상 정리·저장", ["classification", "markers", "render", "qc", "archive"]],
  ["gate", "", ["gate"]],
  ["two", "자막·텍스트 만들기", ["audio", "transcript", "subtitle", "transcript_text", "add"]],
  ["record", "영상 목록", ["catalog"]],
];
const WORDS = { fault: "실패", decide: "직접 확인 필요", todo: "아직", part: "일부 지남", now: "지금 진행", done: "모두 지남", closed: "닫힘", ask: "시작 선택", skip: "대상 없음" };
const PROBLEM_STEPS = ["classification", "markers", "render", "qc", "archive", "audio", "transcript", "subtitle", "transcript_text", "add"];
const STAGE_STATES = { intake: ["todo", "todo"], stage1: ["now", "todo"], stt: ["done", "now"], stage2: ["done", "now"], done: ["done", "done"] };
const STAGE_ICON = { fault: "fault", decide: "decide", todo: "idle", part: "part", now: "work", ask: "decide", done: "done", skip: "idle" };

// tally counts the drive's folders at one step by stored StepState and
// WaitReason only.
function tally(current, stageID) {
  const counts = { done: 0, now: 0, queued: 0, fault: 0, decide: 0, total: 0 };
  for (const folder of current.folders || []) {
    const stage = (folder.stages || []).find((item) => item.stage_id === stageID);
    if (!stage) continue;
    counts.total += 1;
    if (stage.state === "COMPLETED") counts.done += 1;
    else if (stage.state === "FAILED") counts.fault += 1;
    else if (stage.state === "WAITING" && humanWait.has(stage.wait_reason)) counts.decide += 1;
    else if (stage.state === "WAITING" || activeStates.has(stage.state)) counts.now += 1;
    else counts.queued += 1;
  }
  return counts;
}

function tallyState(counts) {
  if (counts.fault) return "fault";
  if (counts.decide) return "decide";
  if (!counts.total) return "todo";
  if (counts.now) return "now";
  if (counts.done === counts.total) return "done";
  return counts.done ? "part" : "todo";
}

function tallyLines(counts) {
  return [["완료", counts.done], ["진행", counts.now], ["시작 대기", counts.queued], ["직접 확인", counts.decide], ["실패", counts.fault]]
    .filter(([, value]) => value).map(([name, value]) => `${name} ${formatCount(value)} 폴더`);
}

function gateState(current) {
  const state = current.archive_run_state;
  if (state === "TRANSCRIPTION_READY") return ["now", `자동 시작 준비 · 대상 ${formatCount(current.eligible_transcription_count)} 폴더`];
  if (state === "STAGE_TWO_RUNNING" || (state === "COMPLETED" && current.eligible_transcription_count > 0)) return ["done", "시작됨"];
  if (state === "COMPLETED") return ["skip", "대상 없음"];
  if (current.archive_run_id) return ["closed", `영상 정리·저장 ${formatCount(current.stage_one_remaining)} 폴더 남음`];
  return ["todo", ""];
}

function catalogState(current) {
  if (current.catalog_count_unknown) return "part";
  if (!current.catalog_row_count) return "todo";
  return current.archive_run_state === "COMPLETED" ? "done" : "part";
}

function catalogRecord(current) {
  if (current.catalog_count_unknown) {
    return current.catalog_row_count ? `${formatCount(current.catalog_row_count)}건 확인 · 전체 게시 건수 기록 없음` : "게시 건수 기록 없음";
  }
  if (current.catalog_row_count) return `${formatCount(current.catalog_row_count)}행 · ${shortTime(current.catalog_updated_at)} 기록`;
  return current.archive_run_id ? "게시 0건" : "실행 뒤 기록";
}

// StepNodes are the stage's steps grouped without connector lines: the
// first-stage, optional second-stage and catalog records do not form one
// total order, and each node counts only the folders it applies to.
function StepNodes({ drive: current }) {
  const node = (id) => {
    let state;
    let lines;
    let badges = [];
    let now = 0;
    if (id === "gate") {
      const [gate, text] = gateState(current);
      state = gate;
      lines = text ? [text] : [];
    } else if (id === "catalog") {
      state = catalogState(current);
      lines = [catalogRecord(current)];
    } else {
      const counts = tally(current, id);
      state = tallyState(counts);
      lines = tallyLines(counts);
      badges = [["fault", counts.fault], ["decide", counts.decide]].filter(([, count]) => count);
      now = counts.now;
    }
    const name = label("stage", id);
    const tip = [`${name} · ${WORDS[state]}`, INFO[id][0], `적용 · ${INFO[id][1]}`, ...lines].join("\n");
    return html`<li class="step-node flow-node" key=${id} data-step=${id} data-state=${state} title=${tip} aria-label=${`${name} · ${WORDS[state]}`}>
      <span>${name}</span>
      ${badges.map(([signal, count]) => html`<span class="node-badge" key=${signal}><${StatusIcon} kind=${signal} />${count}</span>`)}
      ${now ? html`<b class="num">${now}</b>` : null}
    </li>`;
  };
  return html`<div class="step-groups" aria-label=${`${current.workspace_drive_id} 단계 상태`}>
    ${GROUPS.map(([id, name, steps]) => html`<div class="step-group" key=${id} data-stage=${id}>
      ${name ? html`<span class="step-group-name">${name}</span>` : null}
      <ol class="step-track">${steps.map(node)}</ol>
    </div>`)}
  </div>`;
}

function StageSummary({ drive: current }) {
  const id = current.workspace_drive_id;
  const open = ui.stagesOpen.value.has(id);
  let [one, two] = STAGE_STATES[drivePhase(current)] || STAGE_STATES.intake;
  if (drivePhase(current) === "done" && !(current.folders || []).some((folder) => folder.transcription_job_id)) two = "skip";

  const problems = [];
  for (const step of PROBLEM_STEPS) {
    const counts = tally(current, step);
    if (counts.fault) problems.push(["fault", step, counts.fault]);
    if (counts.decide) problems.push(["decide", step, counts.decide]);
  }
  problems.sort((left, right) => (left[0] === right[0] ? 0 : left[0] === "fault" ? -1 : 1));
  const stageState = (index, fallback) => problems.find(([, step]) => GROUPS[index][2].includes(step))?.[0] || fallback;
  const stages = [["one", "영상 정리·저장", stageState(0, one)], ["two", "자막·텍스트", stageState(2, two)], ["record", "영상 목록", catalogState(current)]];
  const toggle = () => {
    const next = new Set(ui.stagesOpen.peek());
    if (!next.delete(id)) next.add(id);
    ui.stagesOpen.value = next;
  };
  return html`<div class="stage-matrix" data-drive-id=${id} data-open=${open ? "true" : "false"}>
    <div class="stage-summary">
      <ol class="stage-track" aria-label=${`${id} 단계`}>
        ${stages.map(([stage, name, state]) => html`<li class="stage-chip" key=${stage} data-stage=${stage} data-state=${state} data-kind=${STAGE_ICON[state]} aria-label=${`${name} · ${WORDS[state]}`}>
          <${StatusIcon} kind=${STAGE_ICON[state]} /><span class="stage-name">${name}</span></li>`)}
      </ol>
      <span class="stage-problems">
        ${problems.map(([signal, step, count]) => html`<span class="stage-problem" key=${`${signal}${step}`} data-signal=${signal} data-step=${step}><${StatusIcon} kind=${signal} />${label("stage", step)} ${formatCount(count)}</span>`)}
      </span>
      <button type="button" class="btn btn-sm stage-toggle" aria-expanded=${open ? "true" : "false"} onClick=${toggle}>${open ? "처리 과정 접기" : "처리 과정 보기"}</button>
    </div>
    <${Fold} open=${open}><${StepNodes} drive=${current} /><//>
  </div>`;
}

// progressHover names the latest run scope and the whole discovered inventory
// separately. A completed one-folder run is never described as a completed
// 221-folder source volume.
function progressHover(current) {
  const progress = driveProgress(current);
  const folders = current.folders || [];
  const transcribed = folders.filter((folder) => folder.transcription_job_id && folder.step_state === "COMPLETED").length;
  const bytes = folders.reduce((sum, folder) => sum + (folder.estimated_output_bytes || 0), 0);
  return [
    `최근 작업 보관 ${formatCount(progress.stored)} / ${formatCount(progress.total)} 영상`,
    `최근 작업 폴더 ${progress.storedFolders} / ${progress.folders}${transcribed ? ` · 자막·텍스트 ${transcribed} 폴더` : ""}`,
    `전체 발견 ${formatCount(progress.discoveredFolders)} 폴더`,
    `작업 범위 r${current.input_revision} 고정 · ${folders.length} 폴더 · ${formatCount(progress.total)} 영상 · ${formatBytes(bytes)}`,
    current.catalog_row_count || current.catalog_count_unknown || current.archive_run_state === "COMPLETED" ? `CSV ${catalogRecord(current)}` : "",
  ].filter(Boolean).join("\n");
}

// ConnectionFact says whether the drive is attached now; a normal connection
// is its icon alone.
function ConnectionFact({ drive: current }) {
  const connection = driveConnection(current);
  const title = current.connection_observed_at ? `관측 ${clockTime(current.connection_observed_at, true)}` : "현재 연결을 확인할 수 없음";
  return html`<span class="fact-inline" data-fact="connection" title=${title}><span class="muted">연결</span>
    ${connection.state === "CONNECTED" ? html`<${Fine} text=${connection.text} />` : html`<span class="ok"><${StatusIcon} kind=${connection.signal === "fault" ? "fault" : "idle"} />${connection.text}</span>`}
  </span>`;
}

export function DriveOverview({ drive: current, id = "", progressID = "", facts = null, title = null }) {
  const phase = drivePhase(current);
  const outcome = driveOutcome(current);
  const progress = driveProgress(current);
  const worker = store.workers.value.find((item) => item.worker_id === current.worker_id);
  const offline = stateSignal(worker?.state) === "fault";
  return html`<section class="overview" id=${id || null} aria-label="진행" title=${title}>
    <div class="overview-head">
      <span class="state" data-fact="state" data-phase=${phase}><${StatusIcon} kind=${outcome.kind} /><${Swap} text=${outcome.text} /></span>
      <${ConnectionFact} drive=${current} />
      ${offline ? html`<span class="fact-inline" data-fact="worker" title=${`마지막 응답 ${clockTime(worker?.last_heartbeat, true)}`}><span class="ok"><${StatusIcon} kind="fault" />${current.worker_id} ${label("worker", worker?.state)}</span></span>` : null}
      ${current.archive_storage_id ? null : html`<span class="fact-inline muted" data-fact="archive">저장 위치 미선택</span>`}
      ${current.archive_run_id
        ? html`<span class="progress-fact" id=${progressID || null} data-fact="progress" title=${progressHover(current)}><span class="muted">${phase === "done" ? "최근 작업" : "현재 작업"}</span><b class="progress-pct num">${formatCount(progress.storedFolders)} / ${formatCount(progress.folders)} 폴더</b><small>발견 ${formatCount(progress.discoveredFolders)} 폴더</small></span>`
        : html`<span class="progress-fact" data-fact="found"><span class="muted">발견</span><b class="num">${folderDiscoveryText(current)}</b></span>`}
    </div>
    ${current.archive_run_id && phase !== "done" ? html`<div class="overview-meter"><${Meter} value=${progress.pct / 100} label="현재 작업 보관 진행" /></div>` : null}
    ${facts}
    <${StageSummary} drive=${current} />
  </section>`;
}

// ─── Problems a person resolves here ────────────────────────────────────────

function reasonOf(current, folder, problem) {
  if (problem.kind === "fault") return `${label("stage", problem.stage.stage_id)} 단계 실패`;
  if (problem.kind === "hold") return (folder.hold_reasons || []).map((reason) => label("hold", reason)).join(" · ") || "처리 방식을 고르기 전";
  if (problem.kind === "device") return `${problem.target || "저장 위치"} 연결 시 자동 재개`;
  if (problem.kind === "review") return (folder.transcript_qualities || []).flatMap((quality) => quality.reasons || []).map((reason) => label("transcriptQuality", reason)).filter((value, index, all) => all.indexOf(value) === index).join(" · ");
  return `${label("stage", problem.stage.stage_id)} · ${label("wait", problem.stage.wait_reason)} 대기`;
}

function HoldDecision({ drive: current, folder, open }) {
  const available = (folder.work_options || []).map((option) => {
    const target = option.target_video_bitrate_kbps || 0;
    const choice = WORK_CHOICE_LABELS[`${option.disposition}:${target}`];
    return choice ? [choice[0], choice[1], option.disposition, target] : null;
  }).filter(Boolean);
  const review = folder.noise_review;
  const choiceKey = JSON.stringify([current.archive_run_id, folder.folder, folder.input_revision, folder.decision_id]);
  const chosen = ui.hold.value?.key === choiceKey ? ui.hold.value.choice : "";
  const [pending, setPending] = useState(false);
  const submit = async () => {
    const option = available.find(([value]) => value === chosen);
    if (!option) return;
    setPending(true);
    try {
      await mutation(`/api/v1/archive-intake/${encodeURIComponent(current.workspace_drive_id)}/override`, {
        command_id: newID("COMMAND"), decision_id: newID("DECISION"), idempotency_key: newID("OVERRIDE"),
        archive_run_id: current.archive_run_id, folder: folder.folder,
        expected_revision: folder.input_revision, expected_decision_id: folder.decision_id, disposition: option[2], target_video_bitrate_kbps: option[3],
      });
      batch(() => { ui.hold.value = null; ui.status.value = `${folder.folder} · ${option[1]}`; });
      await refresh();
    } catch (error) {
      ui.status.value = error.message;
    } finally {
      setPending(false);
    }
  };
  return html`<div class="problem-body">
    <span class="muted hold-reasons">선택이 필요한 이유 · ${(folder.hold_reasons || []).map((reason) => label("hold", reason)).join(" · ") || "—"} · 영상 ${formatCount(folder.video_count)}</span>
    ${review && html`<div class="noise-review" aria-label="노이즈 후보 근거">
      <strong>${review.kind === "PROJECT_RENDER_CACHE" ? "편집 라이브러리의 렌더 캐시 후보" : "카메라 프록시 후보"}</strong>
      <p>대응 원본 확인 ${formatCount(review.matched_count)}개 · 미확인 ${formatCount(review.unconfirmed_count)}개</p>
      <details class="evidence-fold"><summary>판단 근거 보기</summary>
      ${review.matched_count > 0 && html`<p>같은 카드에서 파일 번호가 대응하고 원본 후보보다 크기가 작습니다.</p>`}
      ${(review.matched_examples || []).map((item) => html`<div key=${item.relative_path}>${item.relative_path} → ${item.matched_original}</div>`)}
      ${Object.entries(review.unconfirmed_reasons || {}).map(([reason, count]) => html`<div key=${reason}>${label("noiseReason", reason)} · ${formatCount(count)}개</div>`)}
      ${(review.unconfirmed_examples || []).map((item) => html`<div key=${item.relative_path}>추가 확인 · ${item.relative_path} · ${label("noiseReason", item.reason_code)}</div>`)}
      </details><p>선택은 이 폴더의 영상 전체에 적용됩니다. 이번 작업에서 제외해도 원본은 보존됩니다.</p>
    </div>`}
    ${available.length ? html`<div class="seg plain-seg hold-choices" role="group" aria-label="처리 방법 선택">
      ${available.map(([value, text]) => html`<button type="button" key=${value} data-choice=${value} aria-pressed=${chosen === value ? "true" : "false"} disabled=${pending || stale.value}
        onClick=${() => { ui.hold.value = { key: choiceKey, choice: value }; }}>${text}</button>`)}
    </div>` : html`<span class="muted">선택 가능한 작업 없음</span>`}
    <button type="button" class="btn primary" id=${open ? "hold-submit" : null} data-action="hold" disabled=${!chosen || pending || stale.value} onClick=${submit}>${pending ? "적용 중…" : "선택한 방식으로 처리"}</button>
  </div>`;
}

function TranscriptQuality({ folder }) {
  const reviews = (folder.transcript_qualities || []).filter((quality) => quality.state === "REVIEW_RECOMMENDED");
  return html`<div class="problem-body"><div class="transcript-quality">
    ${reviews.map((quality) => html`<div class="quality" key=${quality.transcript_id} data-transcript-id=${quality.transcript_id}>
      <code>${quality.transcript_id}</code>
      <span class="reasons">${(quality.reasons || []).map((reason) => label("transcriptQuality", reason)).join(" · ")}</span>
      <span class="metrics num">${(quality.duration_millis / 1000).toFixed(1)}초 · ${formatCount(quality.segment_count)}구간 · ${formatCount(quality.text_character_count)}자</span>
    </div>`)}
  </div></div>`;
}

// A failed folder is not retried in place: it goes into a new work a person
// requests, with its drive, the folder and the original archive storage chosen.
export function RedoFolder({ drive: current, folder }) {
  const running = isRunning(current);
  const redo = () => {
    openNewWork(current.workspace_drive_id, [folder], current.archive_storage_id || "");
    go("#/create");
  };
  return html`<button type="button" class="btn" data-action="redo" disabled=${running || stale.value} title=${running ? "현재 작업 종료 후 가능" : ""} onClick=${redo}>이 폴더 다시 처리</button>`;
}

function FaultBody({ drive: current, folder, problem }) {
  const isTranscription = GROUPS[2][2].includes(problem.stage.stage_id);
  const jobID = isTranscription ? folder.transcription_job_id : folder.job_id;
  const detail = jobID ? jobDetail(jobID).value : null;
  const failedStep = (detail?.steps || []).find((step) => step.state === "FAILED");
  const stage = problem.stage;
  const hover = [
    stage?.total_count ? `${formatCount(stage.completed_count)} / ${formatCount(stage.total_count)} 영상` : "",
    jobID && `작업 ${jobID}`, failedStep?.error_class && `오류 종류 ${failedStep.error_class}`, failedStep?.log_reference && `로그 ${failedStep.log_reference}`,
  ].filter(Boolean).join("\n");
  return html`<div class="problem-body">
    <span class="muted" title=${hover}>${label("disposition", folder.disposition)} · ${formatCount(folder.video_count)} 영상${failedStep?.error_class ? ` · ${failedStep.error_class}` : ""}</span>
    <div class="fault-actions">
    ${jobID && html`<${DiagnoseFailure} jobID=${jobID} stepID=${failedStep?.id || stage.stage_id} />`}
    <${RedoFolder} drive=${current} folder=${folder.folder} /></div>
  </div>`;
}

function ProblemBody({ drive: current, folder, problem, open }) {
  if (problem.kind === "hold") return html`<${HoldDecision} drive=${current} folder=${folder} open=${open} />`;
  if (problem.kind === "review") return html`<${TranscriptQuality} folder=${folder} />`;
  if (problem.kind === "fault") return html`<${FaultBody} drive=${current} folder=${folder} problem=${problem} />`;
  const stage = problem.stage;
  const count = stage?.total_count ? ` · ${formatCount(stage.completed_count)} / ${formatCount(stage.total_count)} 영상` : "";
  if (problem.kind === "device") {
    return html`<div class="problem-body">
      <span class="muted" title=${`${label("stage", stage.stage_id)} 단계 대기${count}`}>${problem.target || "저장 위치"} 연결 시 자동 재개</span>
      <a class="text-btn" href=${`#/connections?worker=${encodeURIComponent(current.worker_id)}`}>작업 PC와 저장 위치 보기</a>
    </div>`;
  }
  return html`<div class="problem-body"><span class="muted">${label("stage", stage.stage_id)} · ${label("wait", stage.wait_reason)} 대기${count}</span></div>`;
}

function ProblemPanel({ drive: current, folder, problem, entering }) {
  const open = useComputed(() => ui.openProblem.value === folder.folder).value;
  const last = useRef(problem);
  if (problem) last.current = problem;
  const shown = last.current;
  return html`<div class="problem-panel" data-folder-id=${folder.folder} data-kind=${shown.kind} data-signal=${shown.signal} data-new=${entering ? "true" : null} data-open=${open ? "true" : "false"}
    aria-label=${`${shown.title} ${folder.folder}`}>
    <button type="button" class="problem-head" aria-expanded=${open ? "true" : "false"} onClick=${() => { ui.openProblem.value = open ? "" : folder.folder; }}>
      <${StatusIcon} kind=${shown.signal} />
      <span class="problem-line"><b class="problem-title">${folder.folder}</b><span>${shown.kind === "fault" ? reasonOf(current, folder, shown) : `${shown.title} · ${reasonOf(current, folder, shown)}`}</span></span>
      <span class="token" hidden=${shown.kind === "fault"}>${shown.token}</span>
      <${Chevron} />
    </button>
    <${Fold} open=${open}><${ProblemBody} drive=${current} folder=${folder} problem=${shown} open=${open} /><//>
  </div>`;
}

const PROBLEMS_SHOWN = 30;

// The problem list stays mounted while empty, so the first new problem still
// enters with its highlight. A drive with many problems shows the first ones
// and the one a link opened; the folder table holds the rest.
export function Problems({ drive: current, items, id = "drive-problems" }) {
  const open = ui.openProblem.value;
  const shown = items.length > PROBLEMS_SHOWN
    ? items.filter(([folder], index) => index < PROBLEMS_SHOWN || folder.folder === open) : items;
  return html`<${Fold} open=${items.length > 0} keep=${true}>
    <section class="section" id=${id} aria-label="직접 확인할 항목">
      <h2 class="section-title">직접 확인할 항목${" "}<span class="num">${formatCount(items.length)}</span></h2>
      <div class="card-list">
        <${Presence} items=${shown} keyOf=${([folder]) => folder.folder}>${([folder, problem], entry) => html`<${ProblemPanel} drive=${current} folder=${folder} problem=${problem} entering=${entry.entering} />`}<//>
      </div>
      ${items.length > shown.length ? html`<p class="more-note">외 ${formatCount(items.length - shown.length)}건 · 아래 폴더 표에서 확인</p>` : null}
    </section>
  <//>`;
}

// openFirstProblem opens the problem a link names, else the first problem.
export function useOpenProblem(current, items = driveProblems(current)) {
  useEffect(() => {
    const named = route.peek().query.get("folder") || "";
    const first = items[0]?.[0]?.folder || "";
    const problems = new Set(items.map(([folder]) => folder.folder));
    batch(() => {
      ui.openProblem.value = named && problems.has(named) ? named : first;
      if (named && !problems.has(named)) ui.openFolder.value = named;
    });
  }, [current?.workspace_drive_id, route.value.full]);
}

onRepeat((next) => {
  const named = next.query.get("folder");
  if (named) ui.openProblem.value = named;
});

// ─── Folders: list or calendar, drawn by position for a long drive ──────────

const archiveBadge = (state) => html`<b class="archive-badge" data-state=${state}>${state === "STORED" ? "저장 완료" : "보존 완료"}</b>`;

function FolderVideos({ folder, driveID, open, inventoryOnly = false }) {
  const drive = store.drives.get(driveID)?.value;
  const query = new URLSearchParams({ folder: folder.folder, revision: drive?.folder_discovery_revision || 0, run: drive?.archive_run_id || "", decision: folder.decision_id || "" });
  if (inventoryOnly) query.set("scope", "inventory");
  const url = folder.files_deferred && open ? `/api/v1/archive-intake/${encodeURIComponent(driveID)}/files?${query}` : "";
  const [answer, setAnswer] = useState(null);
  const [attempt, setAttempt] = useState(0);
  useEffect(() => {
    if (!url) return;
    const controller = new AbortController();
    (async () => {
      try {
        const response = await fetch(url, { signal: controller.signal });
        if (!response.ok) throw new Error(String(response.status));
        const { data } = await response.json();
        if (!controller.signal.aborted) setAnswer({ url, videos: data });
      } catch {
        if (!controller.signal.aborted) setAnswer({ url, error: true });
      }
    })();
    return () => controller.abort();
  }, [url, attempt]);
  const loaded = answer?.url === url ? answer : null;
  const videos = url ? loaded?.videos || [] : folder.videos || [];
  const [allFiles, setAllFiles] = useState(false);
  const exceptions = inventoryOnly ? videos.filter((video) => video.context_evidence?.case === "UNPAIRED_PROXY") : [];
  const filtered = !allFiles && exceptions.length ? exceptions : videos;
  return videos.length ? html`${exceptions.length > 0 && html`<div class="seg plain-seg" role="group" aria-label="파일 근거 범위">
      <button type="button" aria-pressed=${!allFiles} onClick=${() => setAllFiles(false)}>미확인 ${formatCount(exceptions.length)}</button>
      <button type="button" aria-pressed=${allFiles} onClick=${() => setAllFiles(true)}>전체 ${formatCount(videos.length)}</button>
    </div>`}<div class="source-video-list" role="table" aria-label=${`${folder.folder} 영상 기록물`}>
      <div class="source-video-row source-video-head" role="row"><span role="columnheader">영상</span><span role="columnheader">수정</span><span role="columnheader">용량</span></div>
      ${filtered.map((video) => html`<div class="source-video-row" role="row" key=${video.relative_path} data-relative-path=${video.relative_path} title=${video.relative_path}>
        ${inventoryOnly ? html`<details class="source-video-name file-proof"><summary>${video.file_name || video.relative_path}</summary><small>${video.relative_path}</small>
          ${video.matched_original ? html`<small>대응 원본 · ${video.matched_original}</small>` : html`<small>대응 원본 미확인</small>`}
        </details>` : html`<span class="source-video-name"><b>${video.file_name || video.relative_path}</b><small>${video.relative_path}</small>${video.context_evidence?.case === "EDITORIAL_INPUT" ? html`<small>편집 입력 · 같은 프로젝트의 ${video.context_evidence.witness}</small>` : null}${video.context_evidence?.case === "UNPAIRED_PROXY" ? html`<small>대응 원본 미확인 · ${video.context_evidence.witness}</small>` : null}${video.matched_original ? html`<small>대응 원본 · ${video.matched_original}</small>` : null}${video.measurement ? html`<small title=${video.measurement.inspection_id}>${video.measurement.width}×${video.measurement.height} · ${Math.round(video.measurement.duration_millis / 1000)}초 · ${video.measurement.video_bitrate_bps ? (video.measurement.video_bitrate_bps / 1000000).toFixed(1) + " Mbps" : "영상 비트레이트 미확인"}</small>` : null}${video.metadata_hint ? html`<small>${label("metadataHint", video.metadata_hint)}</small>` : null}</span>`}
        <span class="num muted">${video.modified_at ? shortTime(video.modified_at) : "—"}</span>
        <span class="num muted">${video.size_bytes < 1024 ** 3 ? formatSize(video.size_bytes || 0) : formatBytes(video.size_bytes || 0)}</span>
      </div>`)}
    </div>`  : url && !loaded ? html`<span role="status">영상 목록 불러오는 중…</span>`
      : loaded?.error ? html`<button class="btn btn-sm" onClick=${() => { setAnswer(null); setAttempt(attempt + 1); }}>영상 목록 다시 조회</button>`
      : html`<${Empty} text="영상 기록 없음" />`;
}

function FolderDetail({ folder, current, driveID }) {
  const steps = useComputed(() => ui.openSteps.value === folder.folder).value;
  const archiveResult = folder.archive_date && folder.archive_video_file_name ? `${folder.archive_date} · ${folder.archive_video_file_name}` : "";
  const trace = [folder.archive_display_name, folder.archive_package_id, folder.job_id].filter(Boolean).join("\n");
  const state = folderState(folder);
  return html`<div class="folder-detail" id=${current ? "folder-detail" : null} data-folder-id=${folder.folder}>
    <div class="detail-line">
      <b class="detail-meta" title=${[trace, formatBytes(folder.estimated_output_bytes)].filter(Boolean).join("\n")}>${archiveResult || (folder.job_id ? `작업 ${folder.job_id}` : "원본 기록")}</b>
      <span class="detail-facts num muted">${folder.disposition ? label("disposition", folder.disposition) : "미처리"} · ${formatCount(folder.video_count)} 영상 · ${formatBytes(folder.estimated_output_bytes)}</span>
      ${folder.archive_state || folder.can_add_transcription || (folder.transcription_job_id && folder.step_state === "COMPLETED") ? html`<span class="detail-notes">
        ${folder.archive_state ? archiveBadge(folder.archive_state) : null}
        ${folder.can_add_transcription ? html`<span class="note-tag folder-note">자막·텍스트 자동 시작 대상</span>` : null}
        ${folder.transcription_job_id && folder.step_state === "COMPLETED" ? html`<span class="note-tag folder-note">자막·텍스트 완료</span>` : null}
      </span>` : null}
    </div>
    ${folder.source_role ? html`<p class="muted"><b>${label("sourceRole", folder.source_role.role)}</b> · ${folder.source_role.basis}
      ${folder.source_role.role === "CAMERA_PROXY" ? ` · 대응 원본 ${folder.source_role.matched_proxies || 0}개 · 미확인 ${folder.source_role.unpaired_proxies || 0}개` : ""}</p>` : null}
    ${folder.event_preview ? html`<p class="muted">행사 배치 미리보기 · ${folder.event_preview.directory}/${folder.event_preview.video} · ${folder.event_preview.subtitle} · ${folder.event_preview.metadata}</p>` : null}
    ${folder.copy_candidates?.length ? html`<p class="muted">파일명·크기 목록이 같은 폴더 · ${folder.copy_candidates.join(" · ")}</p>` : null}
    ${state.signal === "work" ? html`<${Meter} value=${folderProgress(folder)} label="진행" />` : null}
    <${FolderVideos} folder=${folder} driveID=${driveID} open=${current} />
    ${folder.job_id ? html`<button type="button" class="text-btn steps-toggle" aria-expanded=${steps ? "true" : "false"} onClick=${() => { ui.openSteps.value = steps ? "" : folder.folder; }}>${steps ? "처리 과정 숨기기" : "처리 과정 보기"}</button>` : null}
    <${Fold} open=${steps}><div class="stage-list">
      ${(folder.stages || []).length ? folder.stages.map((stage) => {
        const signal = stageSignal(stage);
        const progress = stage.total_count > 0 && (stage.completed_count > 0 || activeStates.has(stage.state)) ? `${formatCount(stage.completed_count)}/${formatCount(stage.total_count)}` : "";
        const word = stage.state === "WAITING" ? [stage.wait_reason ? `${label("wait", stage.wait_reason)} 대기` : "대기", stage.wait_target_storage_id].filter(Boolean).join(" · ") : label("step", stage.state);
        return html`<div class="stage-row" key=${stage.stage_id} data-stage=${stage.stage_id} data-signal=${signal} data-wait-reason=${stage.wait_reason || null}>
          <${StatusIcon} kind=${signal === "decide" || signal === "fault" || signal === "done" || signal === "work" ? signal : "idle"} /><b>${label("stage", stage.stage_id)}</b><span class="muted">${[progress, word].filter(Boolean).join(" · ")}</span></div>`;
      }) : html`<${Empty} text="단계 없음" />`}
    </div><//>
  </div>`;
}

function FolderRow({ driveID, name, index, sizer }) {
  const open = useComputed(() => ui.openFolder.value === name).value;
  const shown = useLinger(open);
  const folder = store.folders.get(folderKey(driveID, name))?.value;
  if (!folder) return null;
  const problem = folderProblem(folder);
  const state = folderState(folder);
  const signal = folderSignal(folder);
  const word = problem
    ? html`<span class="status-text" data-signal=${problem.signal} data-wait-reason=${problem.reason || null}>${problem.word}</span>`
    : folder.archive_state ? null : html`<span class="folder-word" data-signal=${state.signal}>${folder.job_id || folder.disposition ? state.word : `${formatCount(folder.video_count)} 영상`}</span>`;
  return html`<div class="frow" data-index=${index} ref=${sizer ? (element) => element && sizer.current?.observe(element) : null}>
    <button type="button" class="folder-row" data-folder-id=${name} data-signal=${signal} aria-expanded=${open ? "true" : "false"}
      title=${`${label("disposition", folder.disposition)} · ${formatCount(folder.video_count)} 영상`}
      onClick=${() => batch(() => { ui.openFolder.value = open ? "" : name; ui.openSteps.value = ""; })}>
      <span class="folder-name">${name}</span>
      <span class="folder-state"><${StatusIcon} kind=${signal} />${word}${folder.archive_state ? archiveBadge(folder.archive_state) : null}</span>
      <${Chevron} />
    </button>
    <${Fold} open=${open}>${shown ? html`<${FolderDetail} driveID=${driveID} folder=${folder} current=${open} />` : null}<//>
  </div>`;
}

const WINDOW_FROM = 200;
const ROW_HEIGHT = 43;

// WindowedFolders draws only the rows near the screen, so a drive with
// thousands of folders mounts a few dozen. Rows keep their measured height,
// so an opened folder pushes the rest down correctly.
function WindowedFolders({ driveID, names }) {
  const box = useRef();
  const heights = useRef(new Map());
  const sizer = useRef(null);
  const layout = useRef(null);
  const [range, setRange] = useState([0, 40]);
  const [, bump] = useState(0);
  const total = names.length;
  const offsets = new Float64Array(total + 1);
  for (let index = 0; index < total; index += 1) offsets[index + 1] = offsets[index] + (heights.current.get(index) || ROW_HEIGHT);
  layout.current = offsets;
  useEffect(() => {
    const observer = new ResizeObserver((entries) => {
      let changed = false;
      for (const entry of entries) {
        const index = Number(entry.target.dataset.index);
        const height = entry.target.offsetHeight;
        if (height && heights.current.get(index) !== height) { heights.current.set(index, height); changed = true; }
      }
      if (changed) bump((count) => count + 1);
    });
    sizer.current = observer;
    bump((count) => count + 1);
    return () => observer.disconnect();
  }, []);
  // Measured before paint, and again whenever the page above moves the list.
  useLayoutEffect(() => {
    const view = document.getElementById("view");
    let frame = 0;
    const measure = () => {
      frame = 0;
      const element = box.current;
      const marks = layout.current;
      if (!view || !element) return;
      const top = element.getBoundingClientRect().top - view.getBoundingClientRect().top;
      const from = -top - 600;
      const to = -top + view.clientHeight + 600;
      const count = marks.length - 1;
      let low = 0;
      let high = count;
      while (low < high) { const mid = (low + high) >> 1; if (marks[mid + 1] <= from) low = mid + 1; else high = mid; }
      const start = low;
      high = count;
      while (low < high) { const mid = (low + high) >> 1; if (marks[mid] < to) low = mid + 1; else high = mid; }
      setRange((current) => (current[0] === start && current[1] === low ? current : [start, low]));
    };
    const schedule = () => { if (!frame) frame = requestAnimationFrame(measure); };
    const moved = new ResizeObserver(schedule);
    if (view?.firstElementChild) moved.observe(view.firstElementChild);
    view?.addEventListener("scroll", schedule, { passive: true });
    addEventListener("resize", schedule);
    measure();
    return () => { cancelAnimationFrame(frame); moved.disconnect(); view?.removeEventListener("scroll", schedule); removeEventListener("resize", schedule); };
  }, [total]);
  const [start, end] = range;
  const rows = [];
  for (let index = start; index < Math.min(end, total); index += 1) rows.push(html`<${FolderRow} key=${names[index]} driveID=${driveID} name=${names[index]} index=${index} sizer=${sizer} />`);
  return html`<div class="vlist" ref=${box} style=${`height:${offsets[total]}px`}>
    <div class="vlist-window" style=${`transform:translateY(${offsets[Math.min(start, total)]}px)`}>${rows}</div>
  </div>`;
}

// Calendar counts folders per day and marks the day by its worst folder; a
// day opens the list for that day.
function Calendar({ days }) {
  const months = new Map();
  for (const [date, info] of days) {
    const key = date.slice(0, 7);
    if (!months.has(key)) months.set(key, new Map());
    months.get(key).set(Number(date.slice(8)), info);
  }
  return html`<div class="calendar">
    ${[...months].sort(([left], [right]) => left.localeCompare(right)).map(([month, dayMap]) => {
      const [year, monthNumber] = month.split("-").map(Number);
      const lead = new Date(year, monthNumber - 1, 1).getDay();
      const length = new Date(year, monthNumber, 0).getDate();
      return html`<div class="cal-month" key=${month}>
        <b>${month}</b>
        <div class="cal-grid">
          ${["일", "월", "화", "수", "목", "금", "토"].map((name) => html`<span class="cal-dow" key=${name}>${name}</span>`)}
          ${Array.from({ length: lead }, (_, index) => html`<span key=${`lead${index}`}></span>`)}
          ${Array.from({ length }, (_, index) => {
            const info = dayMap.get(index + 1);
            const date = `${month}-${pad(index + 1)}`;
            return html`<button type="button" class="cal-cell" key=${date} data-date=${date} data-has=${info ? "true" : "false"} disabled=${!info}
              aria-label=${info ? `${date} · ${info.count} 폴더` : date} onClick=${() => batch(() => { ui.dayFilter.value = date; ui.folderView.value = "list"; })}>
              <b>${index + 1}</b>${info ? html`<span data-signal=${info.worst}><i class="cal-dot"></i>${info.stored} / ${info.count}</span>` : null}
            </button>`;
          })}
        </div>
      </div>`;
    })}
  </div>`;
}

const RANK = { fault: 4, decide: 3, work: 2, idle: 1, done: 0 };

export function Folders({ drive: current, names, title = "영상 폴더", id = "drive-contents" }) {
  const driveID = current.workspace_drive_id;
  const view = ui.folderView.value;
  const day = ui.dayFilter.value;
  const dated = names.filter((name) => DATE.test(name));
  const canCalendar = dated.length > 0 && dated.length >= names.length * 0.6;
  let days = [];
  if (canCalendar) {
    const byDate = new Map();
    for (const name of dated) {
      const folder = store.folders.get(folderKey(driveID, name))?.value;
      if (!folder) continue;
      const [, year, month, date] = name.match(DATE);
      const key = `${year}-${month}-${date}`;
      const info = byDate.get(key) || { count: 0, stored: 0, worst: "done" };
      info.count += 1;
      if (folder.archive_state) info.stored += 1;
      const signal = folderSignal(folder);
      if (RANK[signal] > RANK[info.worst]) info.worst = signal;
      byDate.set(key, info);
    }
    days = [...byDate];
  }
  const shown = day ? names.filter((name) => name.match(DATE)?.slice(1).join("-") === day) : names;
  const rows = shown.length > WINDOW_FROM && !day
    ? html`<${WindowedFolders} key=${driveID} driveID=${driveID} names=${shown} />`
    : shown.map((name, index) => html`<${FolderRow} key=${name} driveID=${driveID} name=${name} index=${index} />`);
  return html`<section class="section" id=${id} aria-label="드라이브 폴더와 영상 기록물">
    <div class="section-head">
      <h2 class="section-title">${title}${" "}<span class="num">${formatCount(names.length)}</span></h2>
      ${day ? html`<span class="day-filter">${day} · ${shown.length} 폴더<button type="button" class="icon-btn" aria-label="날짜 거르기 풀기" onClick=${() => { ui.dayFilter.value = ""; }}><${Glyph} d=${GLYPH.close} /></button></span>` : null}
      ${canCalendar ? html`<div class="seg plain-seg" role="group" aria-label="보기">
        ${[["list", "목록"], ["calendar", "달력"]].map(([key, text]) => html`<button type="button" key=${key} aria-pressed=${view === key ? "true" : "false"} onClick=${() => { ui.folderView.value = key; }}>${text}</button>`)}
      </div>` : null}
    </div>
    ${view === "calendar" && canCalendar
      ? html`<div class="folder-table"><${Calendar} days=${days} /></div>`
      : html`<div class="folder-table folder-list" id="drive-folders">
        <div class="folder-head" aria-hidden="true"><span>폴더</span><span>상태</span><span></span></div>
        ${shown.length ? rows : html`<${Empty} text="폴더 없음" />`}
      </div>`}
  </section>`;
}

// ─── Completed results ──────────────────────────────────────────────────────

export function CompletedWork({ items }) {
  if (!items.length) return null;
  const shown = items.slice(0, 30);
  // A row shows the result, its folder and its stored or committed state. The
  // disposition, source count, time, storage and package path wait on hover
  // and in the archive work's result the row opens.
  return html`<section class="section" id="completed-work" aria-label="저장된 결과">
    <h2 class="section-title">저장된 결과${" "}<span class="num">${formatCount(items.length)}</span></h2>
    <div class="results drive-completed">
      <${Presence} items=${shown} keyOf=${(item) => item.archive_package_id}>${(item) => {
        const sourceCount = item.source_video_count > 1 ? `${formatCount(item.source_video_count)} → 1` : formatCount(item.source_video_count);
        const facts = [
          [label("disposition", item.disposition), item.profile_reference].filter(Boolean).join(" · "),
          `원본 → 결과 ${sourceCount}`, `완료 ${shortTime(item.completed_at)}`,
          [item.archive_storage_id, item.archive_relative_path].filter(Boolean).join(" · "),
          item.archive_display_name, `패키지 ${item.archive_package_id}`, `작업 ${item.job_id}`, `실행 ${item.archive_run_id}`,
        ].filter(Boolean).join("\n");
        return html`<a class="result-row completed-row" href=${`#/runs/${encodeURIComponent(item.archive_run_id)}?result=${encodeURIComponent(item.archive_package_id)}`}
          data-package-id=${item.archive_package_id} data-job-id=${item.job_id} data-archive-run-id=${item.archive_run_id} title=${facts}>
          <span class="result-name completed-result"><b>${item.archive_video_file_name || item.archive_package_id}</b><small>${item.folder}</small></span>
          <span class="completed-place">${archiveBadge(item.archive_state)}</span>
          <${Glyph} d=${GLYPH.next} />
        </a>`;
      }}<//>
      ${items.length > shown.length ? html`<div class="more">외 ${formatCount(items.length - shown.length)}개</div>` : null}
    </div>
  </section>`;
}

export const StatusLine = () => (ui.status.value ? html`<p class="status-line" role="status" id="command-status">${ui.status.value}</p>` : null);

export const titleShared = (key) => shared.value?.key === key && route.value.full === key;

// ─── Screen ─────────────────────────────────────────────────────────────────

function ProxyInventory({ drive }) {
  const [expanded, setExpanded] = useState(false);
  const [selectedName, setSelectedName] = useState("");
  if (drive.folder_discovery_state !== "READY" || drive.connection_state !== "CONNECTED") return null;
  const folders = drive.inventoryFolders.filter((folder) => folder.inventory_role?.role === "CAMERA_PROXY");
  if (!folders.length) return null;
  const priority = ({ inventory_role: role }) => role.unpaired_proxies ? (role.matched_proxies ? 0 : 1) : 2;
  folders.sort((a, b) => priority(a) - priority(b) || a.folder.localeCompare(b.folder));
  const total = (key) => folders.reduce((count, folder) => count + (folder.inventory_role[key] || 0), 0);
  const selected = folders.find((folder) => folder.folder === selectedName) || folders[0];
  return html`<section class="inventory-review" aria-label="발견된 프록시 후보">
    <button type="button" class="inventory-head" aria-expanded=${expanded} onClick=${() => setExpanded(!expanded)}>
      <b>프록시 후보 <span class="num">${formatCount(folders.length)} 폴더</span></b>
      <span class="inventory-count">대응 원본 <b>${formatCount(total("matched_proxies"))}</b></span>
      <span class="inventory-count" data-signal=${total("unpaired_proxies") ? "decide" : "idle"}><${StatusIcon} kind=${total("unpaired_proxies") ? "decide" : "done"} />미확인 <b>${formatCount(total("unpaired_proxies"))}</b></span><${Chevron} />
    </button>
    ${expanded && html`<div class="inventory-body">
      <label class="field"><span>폴더별 근거</span><select value=${selected.folder} onChange=${(e) => setSelectedName(e.currentTarget.value)}>
        ${folders.map((folder) => html`<option key=${folder.folder} value=${folder.folder}>${folder.folder} · 미확인 ${formatCount(folder.inventory_role.unpaired_proxies || 0)}</option>`)}
      </select></label>
      <${FolderVideos} key=${selected.folder} folder=${{ folder: selected.folder, files_deferred: selected.inventory_deferred, videos: selected.inventory_videos }} driveID=${drive.workspace_drive_id} open=${true} inventoryOnly=${true} />
    </div>`}
  </section>`;
}

export function DriveScreen({ id }) {
  const current = driveView(id);
  useOpenProblem(current);
  if (!current) return html`<div class="page-head"><div class="page-title"><h1 id="drive-title">${id}</h1></div></div><${Empty} text="드라이브 없음" />`;
  const worker = store.workers.value.find((item) => item.worker_id === current.worker_id);
  const hover = `${current.worker_id} ${label("worker", worker?.state)} · → ${current.archive_storage_id || "저장 위치 미선택"} · ${current.archive_run_id || "작업 없음"} · r${current.input_revision} · ${formatCount(current.folder_count)} 폴더 · ${formatCount(current.file_count)} 영상`;
  const runHref = current.archive_run_id ? `#/runs/${encodeURIComponent(current.archive_run_id)}` : "";
  return [
    html`<${Crumbs} key="crumbs" items=${[["장비·에이전트", "#/connections"], [driveName(current)]]} />`,
    html`<div key="head" class="page-head">
      <div class="page-title"><h1 id="drive-title" class=${titleShared(route.value.full) ? "shared" : null} title=${hover}>${driveName(current)}</h1></div>
      <div class="page-actions">
        ${runHref ? html`<a class="btn" href=${runHref}>최근 작업 보기</a>` : null}
        ${!current.archive_run_id && current.connection_state === "CONNECTED"
          ? html`<button type="button" class="btn" id="open-new-work" onClick=${() => { openNewWork(current.workspace_drive_id); go("#/create"); }}>이 드라이브 처리</button>` : null}
      </div>
    </div>`,
    html`<${DriveOverview} key="overview" drive=${current} id="drive-overview" progressID="drive-progress" />`,
    html`<${StatusLine} key="status" />`,
    html`<${ProxyInventory} key=${id} drive=${current} />`,
    html`<${Problems} key="problems" drive=${current} items=${driveProblems(current)} />`,
    html`<${CompletedWork} key="completed" items=${current.completed_work || []} />`,
    current.inventory.length || current.scope.length
      ? html`<${Folders} key="folders" drive=${current} names=${current.inventory.length ? current.inventory : current.scope} />` : null,
  ];
}
