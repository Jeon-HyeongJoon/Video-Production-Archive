// One read model for every screen. The workspace polls the same projections
// every 5 seconds and each mounted screen re-renders from this model.

import { api, mutation, newID, label, stageSignal, stateSignal, humanWait, activeStates, finishedTurns, finishedSteps, formatCount } from "./core.js";

// Three missed 5 second refreshes in a row mean the screen is stale.
const STALE_AFTER = 3;

export const model = { workers: [], drives: [], hosts: [], sessions: [], jobs: [], work: [], refreshedAt: 0, failures: 0, error: "" };

// Screen-local choices that must survive the 5 second refresh.
export const ui = {
  prepDriveID: "", prepRevision: 0, archiveID: "", intakeSessionID: newID("INTAKE"), archiveRunID: "", archiveRunKey: "", runStarting: false,
  folders: new Set(), prepFolderScroll: 0, hold: null,
  folderView: "", openFolder: "", openSteps: "", jobSteps: false, jobCancel: "", jobAttentionOpen: true, jobCompletedOpen: false, runScopeOpen: false,
  sessionID: "", focusCard: "", fresh: "", status: "", alertOpen: false, agentOpen: false,
};

const listeners = new Set();
let refreshSequence = 0;

export function subscribe(listener) {
  listeners.add(listener);
}

export function notify() {
  for (const listener of listeners) listener();
}

export function isStale() {
  return model.failures >= STALE_AFTER;
}

export async function refresh() {
  const sequence = ++refreshSequence;
  try {
    const [workers, drives, hosts, sessions, jobs, work] = await Promise.all([
      api("/api/v1/workers"), api("/api/v1/archive-intake"), api("/api/v1/archivist-hosts"),
      api("/api/v1/archivist-sessions"), api("/api/v1/jobs"), api("/api/v1/work"),
    ]);
    if (sequence !== refreshSequence) return;
    Object.assign(model, { workers, drives, hosts, sessions, jobs: jobs || [], work: work || [], refreshedAt: Date.now(), failures: 0, error: "" });
    trackProblems();
  } catch (error) {
    if (sequence !== refreshSequence) return;
    model.failures += 1;
    model.error = error.message;
  }
  notify();
}

export function startPolling() {
  refresh();
  setInterval(() => {
    if (!document.hidden) refresh();
  }, 5000);
}

// ─── Drives ─────────────────────────────────────────────────────────────────

export const PHASES = [["intake", "접수"], ["stage1", "1단계 정리"], ["stt", "STT 시작"], ["stage2", "2단계 전사"], ["done", "완료"]];
const RUN_PHASE = { STAGE_ONE_RUNNING: "stage1", TRANSCRIPTION_READY: "stt", STAGE_TWO_RUNNING: "stage2", COMPLETED: "done" };
export const PHASE_LABEL = { intake: "실행 전", stage1: "1단계 정리 중", stt: "STT 시작 가능", stage2: "2단계 전사 중", done: "완료" };
// The status icon of each phase: running phases turn, the STT gate waits on a
// person, a finished run is done.
export const PHASE_ICON = { intake: "idle", stage1: "work", stt: "decide", stage2: "work", done: "done" };

export function drivePhase(drive) {
  return drive.archive_run_id ? RUN_PHASE[drive.archive_run_state] || "stage1" : "intake";
}

// driveGroup places a drive by its ArchiveRun: none yet, unfinished or completed.
export function driveGroup(drive) {
  const phase = drivePhase(drive);
  return phase === "intake" ? "before" : phase === "done" ? "after" : "during";
}

export function driveName(drive) {
  const source = (drive.storage_candidates || []).find((item) => item.storage_id === drive.source_storage_id);
  return source?.label || source?.model || drive.workspace_drive_id;
}

export function findDrive(driveID) {
  return model.drives.find((drive) => drive.workspace_drive_id === driveID);
}

// driveProgress counts videos in folders whose package is stored or committed.
export function driveProgress(drive) {
  const folders = drive.folders || [];
  const total = folders.reduce((sum, folder) => sum + (folder.video_count || 0), 0);
  const storedFolders = folders.filter((folder) => folder.archive_state);
  const stored = storedFolders.reduce((sum, folder) => sum + (folder.video_count || 0), 0);
  return { total, stored, storedFolders: storedFolders.length, folders: folders.length, pct: total ? Math.round((stored / total) * 100) : 0 };
}

export function isRunning(drive) {
  return Boolean(drive?.archive_run_id) && drive.archive_run_state !== "COMPLETED";
}

export function discoveredFolders(drive) {
  return Array.isArray(drive?.discovered_folders) ? drive.discovered_folders : (drive?.folders || []);
}

export function folderDiscoveryReady(drive) {
  return drive?.folder_discovery_state === "READY" && Boolean(drive.folder_discovery_revision);
}

export function folderDiscoveryText(drive) {
  const folders = formatCount(drive?.discovered_folder_count || 0);
  const videos = formatCount(drive?.discovered_file_count || 0);
  if (drive?.folder_discovery_state === "SCANNING") return `검색 중 · ${folders} 폴더 · ${videos} 영상`;
  if (drive?.folder_discovery_state === "FAILED") return "검색 실패";
  if (folderDiscoveryReady(drive)) return `${folders} 폴더 · ${videos} 영상`;
  return "검색 대기";
}

export function archiveCandidates(drive) {
  return (drive?.storage_candidates || [])
    .filter((item) => item.role === "ARCHIVE_PRIMARY" && item.storage_id !== drive.source_storage_id)
    .sort((left, right) => left.storage_id.localeCompare(right.storage_id));
}

// prepareDrive opens a new ArchiveRun scope for one drive. It fixes the human
// volume choice to an ArchiveIntakeSession when the drive has none yet.
export async function prepareDrive(drive) {
  ui.prepDriveID = drive.workspace_drive_id;
  ui.prepRevision = drive.folder_discovery_revision || 0;
  ui.archiveID = "";
  ui.folders = new Set();
  ui.prepFolderScroll = 0;
  ui.intakeSessionID = drive.session_id || newID("INTAKE");
  ui.archiveRunID = newID("ARCHIVE-RUN");
  ui.archiveRunKey = newID("ARCHIVE-RUN-START");
  ui.runStarting = false;
  ui.status = "";
  notify();
  if (drive.session_id || !drive.source_volume_id || !drive.candidate_revision) return;
  try {
    const result = await mutation("/api/v1/archive-intake-sessions", {
      session_id: ui.intakeSessionID, volume_id: drive.source_volume_id, expected_candidate_revision: drive.candidate_revision,
    });
    ui.intakeSessionID = result.session_id;
    ui.status = "원본 탐색 중";
    await refresh();
  } catch (error) {
    ui.status = error.message;
    notify();
  }
}

export function preparing(drive) {
  return Boolean(drive) && ui.prepDriveID === drive.workspace_drive_id && !isRunning(drive);
}

export function syncPreparation(drive) {
  if (!preparing(drive)) return;
  if (folderDiscoveryReady(drive) && ui.prepRevision && ui.prepRevision !== drive.folder_discovery_revision) {
    ui.prepRevision = drive.folder_discovery_revision;
    ui.folders = new Set();
    ui.archiveRunID = newID("ARCHIVE-RUN");
    ui.archiveRunKey = newID("ARCHIVE-RUN-START");
    ui.runStarting = false;
  }
  if (!folderDiscoveryReady(drive)) {
    ui.folders = new Set();
  } else {
    const known = new Set(discoveredFolders(drive).map((folder) => folder.folder));
    ui.folders = new Set([...ui.folders].filter((folder) => known.has(folder)));
  }
  if (ui.archiveID && !archiveCandidates(drive).some((item) => item.storage_id === ui.archiveID)) ui.archiveID = "";
}

export function startBlocker(drive, selected) {
  const worker = model.workers.find((item) => item.worker_id === drive.worker_id);
  const source = (drive.storage_candidates || []).find((item) => item.storage_id === drive.source_storage_id);
  const archive = archiveCandidates(drive).find((item) => item.storage_id === ui.archiveID);
  const estimated = selected.reduce((sum, folder) => sum + (folder.estimated_output_bytes || 0), 0);
  if (drive.folder_discovery_state === "SCANNING") return "폴더 검색 중";
  if (drive.folder_discovery_state === "FAILED") return "폴더 검색 실패";
  if (!folderDiscoveryReady(drive)) return "폴더 검색 대기";
  if (!selected.length) return "폴더 선택 없음";
  if (!ui.archiveID) return "아카이브 선택 없음";
  if (!source?.accessible) return "원본 접근 불가";
  if (!archive?.accessible) return "아카이브 접근 불가";
  if (archive.free_bytes < estimated) return "아카이브 여유 용량 부족";
  if (!worker || worker.state === "OFFLINE" || worker.state === "DISABLED") return "Worker 꺼짐";
  if (!drive.source_volume_id) return "원본 정보 없음";
  return "";
}

// driveSelection is the structured reference an ArchivistTurn carries; it is
// never inferred from message text. A drive with an ArchiveRun hands over the
// run's fixed scope, a drive being prepared hands over the person's picks.
export function driveSelection(drive) {
  if (!drive) return null;
  const fixed = Boolean(drive.archive_run_id) && !preparing(drive);
  if (!fixed && !folderDiscoveryReady(drive)) return null;
  const archiveID = fixed ? drive.archive_storage_id : ui.archiveID;
  const folders = fixed ? (drive.folders || []).map((folder) => folder.folder) : [...ui.folders];
  if (!archiveID || !folders.length) return null;
  return {
    archive_run_id: fixed ? drive.archive_run_id : undefined,
    archive_run_revision: fixed ? drive.archive_run_revision : undefined,
    worker_id: drive.worker_id, workspace_drive_id: drive.workspace_drive_id,
    intake_session_id: fixed ? drive.session_id : ui.intakeSessionID,
    source_volume_id: drive.source_volume_id, source_storage_id: drive.source_storage_id, archive_storage_id: archiveID,
    candidate_revision: drive.candidate_revision || drive.input_revision,
    folder_discovery_revision: drive.folder_discovery_revision || drive.input_revision,
    folder_ids: folders.sort(), selected_by: "browser-principal",
  };
}

export function selectionRefs(selection) {
  return selection
    ? [selection.worker_id, selection.workspace_drive_id, selection.source_storage_id, selection.archive_storage_id, `${(selection.folder_ids || []).length} 폴더`].filter(Boolean)
    : [];
}

// ─── Problems: failures and human waits, with the place that resolves them ──

// folderProblem reads stored StepState and WaitReason only. Resource waits and
// normal progress are not problems.
export function folderProblem(folder) {
  const stages = folder.stages || [];
  const failed = stages.find((stage) => stage.state === "FAILED");
  const waiting = stages.find((stage) => stage.state === "WAITING" && humanWait.has(stage.wait_reason));
  if (failed) return { kind: "fault", signal: "fault", title: "처리 실패", word: "실패", stage: failed };
  if (folder.disposition === "HOLD") return { kind: "hold", signal: "decide", title: "분류 확인 필요", word: "분류 확인 필요", reason: waiting?.wait_reason || "" };
  if (waiting?.wait_reason === "DEVICE") return { kind: "device", signal: "decide", title: "아카이브 저장소 연결 대기", word: "저장소 연결 대기", reason: "DEVICE", stage: waiting, target: waiting.wait_target_storage_id || "" };
  if (waiting) {
    const word = waiting.wait_reason === "APPROVAL" ? "승인 필요" : "판정 필요";
    return { kind: "wait", signal: "decide", title: word, word, reason: waiting.wait_reason, stage: waiting };
  }
  const reviews = (folder.transcript_qualities || []).filter((quality) => quality.state === "REVIEW_RECOMMENDED");
  if (reviews.length) return { kind: "review", signal: "decide", title: "전사 검토 권장", word: "전사 검토 권장" };
  return null;
}

// driveProblemCounts counts a drive's folder failures and human waits.
export function driveProblemCounts(drive) {
  const problems = (drive?.folders || []).map(folderProblem).filter(Boolean);
  const fault = problems.filter((problem) => problem.signal === "fault").length;
  return { fault, decide: problems.length - fault };
}

// folderSignal is the one signal of a folder: its problem, its stored record,
// or the state of its steps.
export function folderSignal(folder) {
  const problem = folderProblem(folder);
  if (problem) return problem.signal;
  if (folder.archive_state) return "done";
  return folderState(folder).signal;
}

// folderState is the one word a folder row shows when nothing needs a person.
export function folderState(folder) {
  const stages = folder.stages || [];
  if (!stages.length) return { word: "—", signal: "idle" };
  if (stages.some((stage) => activeStates.has(stage.state) || (stage.state === "WAITING" && !humanWait.has(stage.wait_reason)))) return { word: "진행 중", signal: "work" };
  if (stages.every((stage) => stage.state === "COMPLETED")) return { word: folder.archive_state ? "" : "완료", signal: "done" };
  return { word: "대기열", signal: "idle" };
}

export function driveJobIDs() {
  const ids = new Set();
  for (const drive of model.drives) {
    for (const folder of drive.folders || []) {
      if (folder.job_id) ids.add(folder.job_id);
      if (folder.transcription_job_id) ids.add(folder.transcription_job_id);
    }
  }
  return ids;
}

// Jobs that belong to no drive, such as Template Jobs, stay their own unit.
export function driveFreeJobs() {
  const standalone = new Set(model.work.filter((item) => item.kind === "JOB").map((item) => item.work_id));
  if (standalone.size) return model.jobs.filter((job) => jobLane(job) && standalone.has(job.job_id));
  const owned = driveJobIDs();
  return model.jobs.filter((job) => jobLane(job) && !owned.has(job.job_id));
}

function pendingCards() {
  return model.sessions.flatMap((session) => (session.action_cards || []).filter((card) => card.state === "PENDING").map((card) => ({ card, session })));
}

// problemItems lists every failure and human wait once, keyed so a newly
// appearing one can be told apart from those already on screen.
export function problemItems() {
  const items = [];
  const cards = pendingCards();
  for (const drive of model.drives) {
    const base = { driveID: drive.workspace_drive_id, workerID: drive.worker_id };
    for (const folder of drive.folders || []) {
      const problem = folderProblem(folder);
      if (problem) items.push({ ...base, key: `${problem.kind}:${drive.workspace_drive_id}:${folder.folder}`, kind: problem.kind, signal: problem.signal, title: problem.title, folder: folder.folder, target: problem.target || "" });
    }
    const sttCard = cards.some(({ card }) => card.command_type === "archive_run.transcription.start" && card.target_id === drive.archive_run_id);
    if (drive.archive_run_state === "TRANSCRIPTION_READY" && !sttCard) items.push({ ...base, key: `stt:${drive.workspace_drive_id}`, kind: "stt", signal: "decide", title: "STT 시작 가능" });
  }
  for (const worker of model.workers) {
    if (stateSignal(worker.state) === "fault") items.push({ key: `worker:${worker.worker_id}`, kind: "worker", signal: "fault", title: "Worker 꺼짐", workerID: worker.worker_id });
  }
  for (const host of model.hosts) {
    if (stateSignal(host.state) === "fault") items.push({ key: `host:${host.archivist_host_id}`, kind: "host", signal: "fault", title: "Agent Host 꺼짐", hostID: host.archivist_host_id });
  }
  for (const job of driveFreeJobs()) {
    const lane = jobLane(job);
    if (lane !== "fault" && lane !== "decide") continue;
    const title = lane === "fault" ? "작업 실패" : job.wait_reason ? `작업 ${label("wait", job.wait_reason)} 대기` : "작업 사람 대기";
    items.push({ key: `job:${job.job_id}`, kind: "job", signal: lane, title, jobID: job.job_id, workerID: job.worker_id || "" });
  }
  for (const { card, session } of cards) {
    items.push({ key: `card:${card.action_card_id}`, kind: "card", signal: "decide", title: "Agent 결정 대기", cardID: card.action_card_id, sessionID: session.session_id, label: label("card", card.command_type) });
  }
  return items;
}

// problemSubject names one problem: the drive and folder, the Worker, the Job.
export function problemSubject(item) {
  if (item.driveID) {
    const name = driveName(findDrive(item.driveID) || { workspace_drive_id: item.driveID });
    return item.folder ? `${name} / ${item.folder}` : name;
  }
  if (item.kind === "job") return model.jobs.find((job) => job.job_id === item.jobID)?.target_title || item.jobID;
  return item.workerID || item.hostID || item.label || "";
}

const SIGNAL_RANK = { fault: 2, decide: 1 };

export function sortedProblems() {
  return problemItems().sort((left, right) => SIGNAL_RANK[right.signal] - SIGNAL_RANK[left.signal]);
}

// problemPath is the route where a person resolves the problem.
export function problemPath(item) {
  if (item.driveID) return `#/drives/${encodeURIComponent(item.driveID)}${item.folder ? `?folder=${encodeURIComponent(item.folder)}` : ""}`;
  if (item.kind === "worker") return `#/connections?worker=${encodeURIComponent(item.workerID)}`;
  if (item.kind === "job") return `#/jobs/${encodeURIComponent(item.jobID)}`;
  if (item.kind === "host") return `#/connections?host=${encodeURIComponent(item.hostID)}`;
  return "";
}

const seenProblems = new Set();
// Problems that appeared after the screen was opened and are not yet read.
export const newProblems = new Set();
let primed = false;

// trackProblems marks problems that appeared after the first load; the
// newest one also drives the one-line banner.
function trackProblems() {
  const keys = problemItems().map((item) => item.key);
  if (!primed) {
    for (const key of keys) seenProblems.add(key);
    primed = true;
    return;
  }
  for (const key of keys) {
    if (seenProblems.has(key)) continue;
    seenProblems.add(key);
    newProblems.add(key);
    ui.fresh = key;
  }
}

// ─── Jobs ───────────────────────────────────────────────────────────────────

// jobLane places a Job by its stored StepState and WaitReason only.
export function jobLane(job) {
  if (job.step_state === "FAILED") return "fault";
  if (job.step_state === "WAITING") return stageSignal({ state: "WAITING", wait_reason: job.wait_reason }) === "decide" || job.needs_human_intervention ? "decide" : "resource";
  if (["RUNNING", "RESERVED", "VERIFYING"].includes(job.step_state)) return "work";
  if (finishedSteps.has(job.step_state)) return "";
  return "queue";
}

export function workLane(work) {
  return { FAILED: "fault", ATTENTION: "decide", ACTIVE: "work", QUEUED: "queue", COMPLETED: "done" }[work?.state] || "queue";
}

export function workName(work) {
  if (!work) return "—";
  if (work.kind === "JOB") return work.title || work.work_id;
  const drive = findDrive(work.source_volume_id);
  const source = drive ? driveName(drive) : work.source_volume_id || work.source_storage_id || "원본";
  const rawDate = String(work.created_at || "").slice(0, 10);
  const date = rawDate && rawDate !== "0001-01-01" ? rawDate : "";
  return [date, source, `${formatCount(work.folder_count)}개 폴더`].filter(Boolean).join(" · ");
}

// ─── Archivist ──────────────────────────────────────────────────────────────

export function currentSession() {
  return model.sessions.find((item) => item.session_id === ui.sessionID) || model.sessions[0];
}

export function hostFor(session) {
  return model.hosts.find((host) => host.archivist_host_id === session?.archivist_host_id);
}

export function activeTurn(session) {
  return [...(session?.turns || [])].reverse().find((turn) => !finishedTurns.has(turn.state));
}
