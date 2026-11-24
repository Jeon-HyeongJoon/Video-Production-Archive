// One read model for every screen. The workspace polls the same six
// projections every 5 seconds. An answer whose text did not change is not
// parsed again, and a record whose JSON did not change keeps its signal, so a
// refresh redraws only the rows that moved.

import { signal, computed, batch } from "./lib.js";
import {
  api, mutation, newID, label, stageSignal, stateSignal, humanWait, activeStates, finishedTurns, finishedSteps, formatCount,
  setLocked,
} from "./core.js";
import { transition } from "./motion.js";

// Three missed 5 second refreshes in a row mean the screen is stale.
const STALE_AFTER = 3;
export const REFRESH_MS = 5000;
const ARCHIVE_CAPACITY_MINIMUM_RESERVE_BYTES = 64 * 1024 * 1024;

export const store = {
  drives: new Map(), folders: new Map(), works: new Map(), jobs: new Map(), sessions: new Map(),
  driveIds: signal([]), workIds: signal([]), jobIds: signal([]), sessionIds: signal([]),
  workers: signal([]), hosts: signal([]),
  ready: signal(false), failures: signal(0), refreshedAt: signal(0),
};

export const stale = computed(() => store.failures.value >= STALE_AFTER);

const ACKNOWLEDGED_PROBLEMS_KEY = "archive-workspace.acknowledged-problems.v1";

function readAcknowledgedProblems() {
  try {
    const values = JSON.parse(localStorage.getItem(ACKNOWLEDGED_PROBLEMS_KEY) || "[]");
    return new Set(Array.isArray(values) ? values.filter((value) => typeof value === "string") : []);
  } catch {
    return new Set();
  }
}

function writeAcknowledgedProblems(values) {
  try { localStorage.setItem(ACKNOWLEDGED_PROBLEMS_KEY, JSON.stringify([...values])); } catch { /* unavailable storage */ }
}

// Screen-local choices that must survive the 5 second refresh.
export const ui = {
  openProblem: signal(""), openFolder: signal(""), openSteps: signal(""), stagesOpen: signal(new Set()),
  folderView: signal("list"), dayFilter: signal(""), folderRange: signal({ driveID: "", from: 0, to: 40 }),
  lanes: signal({ attention: true, done: false }), expanded: signal(new Set()), scopeOpen: signal(false),
  hold: signal(null), cancelConfirm: signal(""), status: signal(""),
  newWork: signal(null),
  alertsOpen: signal(null), fresh: signal(""), newProblems: signal(new Set()), acknowledgedProblems: signal(readAcknowledgedProblems()),
  sessionID: signal(""), focusCard: signal(""), agentOpen: signal(false), agentDraft: signal(""),
};

// ─── Records ────────────────────────────────────────────────────────────────

const texts = new Map();
const sameList = (left, right) => left.length === right.length && left.every((item, index) => item === right[index]);

// put keeps one signal per record and writes it only when the record changed.
function put(map, kind, key, value) {
  const text = JSON.stringify(value);
  const cacheKey = `${kind}:${key}`;
  const held = map.get(key);
  if (held && texts.get(cacheKey) === text) return held;
  texts.set(cacheKey, text);
  if (held) {
    held.value = value;
    return held;
  }
  const created = signal(value);
  map.set(key, created);
  return created;
}

function setList(list, next) {
  if (!sameList(list.peek(), next)) list.value = next;
}

function prune(map, kind, keep) {
  for (const key of map.keys()) {
    if (keep.has(key)) continue;
    map.delete(key);
    texts.delete(`${kind}:${key}`);
  }
}

function setValue(holder, kind, value) {
  const text = JSON.stringify(value);
  if (texts.get(kind) === text) return;
  texts.set(kind, text);
  holder.value = value;
}

export const folderKey = (driveID, folder) => `${driveID}\u0001${folder}`;

// A drive's folders are records of their own. The drive keeps the names of
// its ArchiveRun's fixed scope and of its whole discovered inventory; a scope
// folder's run state is laid over its inventory record.
const inventoryCache = new Map();
function applyDrives(list) {
  const ids = [];
  const kept = new Set();
  for (const raw of list || []) {
    const id = raw.workspace_drive_id;
    ids.push(id);
    const { folders, discovered_folders: discovered, ...rest } = raw;

    const cached = inventoryCache.get(id);
    const inventoryRows = raw.inventory_unchanged && cached?.token === raw.inventory_token ? cached.rows : discovered;
    if (raw.inventory_token && Array.isArray(inventoryRows)) inventoryCache.set(id, { token: raw.inventory_token, rows: inventoryRows });
    else inventoryCache.delete(id);
    const scopeFolders = raw.scope_is_inventory ? inventoryRows || [] : folders || [];
    const byName = new Map(scopeFolders.map((folder) => [folder.folder, folder]));
    const source = Array.isArray(inventoryRows) ? inventoryRows : scopeFolders;
    const inventory = [];
    for (const item of source) {
      const current = byName.get(item.folder);
      const merged = current ? { ...item, ...current, files_deferred: item.files_deferred || current.files_deferred, videos: current.videos?.length ? current.videos : item.videos } : item;
      inventory.push(item.folder);
      kept.add(folderKey(id, item.folder));
      put(store.folders, "f", folderKey(id, item.folder), { ...merged, inventory_role: Array.isArray(inventoryRows) ? item.source_role : null, inventory_videos: item.source_role?.role === "CAMERA_PROXY" ? item.videos : undefined, inventory_deferred: item.files_deferred });
    }
    for (const folder of scopeFolders) {
      if (kept.has(folderKey(id, folder.folder))) continue;
      kept.add(folderKey(id, folder.folder));
      put(store.folders, "f", folderKey(id, folder.folder), folder);
    }
    put(store.drives, "d", id, { ...rest, scope: scopeFolders.map((folder) => folder.folder), inventory });
  }
  for (const id of inventoryCache.keys()) if (!ids.includes(id)) inventoryCache.delete(id);
  prune(store.folders, "f", kept);
  prune(store.drives, "d", new Set(ids));
  setList(store.driveIds, ids);
}

function applyKeyed(map, kind, list, ids, idOf) {
  const next = [];
  for (const item of list || []) {
    next.push(idOf(item));
    put(map, kind, idOf(item), item);
  }
  prune(map, kind, new Set(next));
  setList(ids, next);
}

export const drive = (id) => store.drives.get(id)?.value || null;
export const folderOf = (driveID, name) => store.folders.get(folderKey(driveID, name))?.value || null;
export const workOf = (id) => store.works.get(id)?.value || null;
export const jobOf = (id) => store.jobs.get(id)?.value || null;

// driveView joins a drive with its folders for the counts a screen shows; it
// changes whenever one of the folders does.
const views = new Map();
export function driveView(id) {
  let view = views.get(id);
  if (!view) {
    view = computed(() => {
      store.driveIds.value;
      const current = store.drives.get(id)?.value;
      if (!current) return null;
      const read = (name) => store.folders.get(folderKey(id, name))?.value;
      return { ...current, folders: current.scope.map(read).filter(Boolean), inventoryFolders: current.inventory.map(read).filter(Boolean) };
    });
    views.set(id, view);
  }
  return view.value;
}

export const allDrives = computed(() => store.driveIds.value.map((id) => driveView(id)).filter(Boolean));
export const allWork = computed(() => store.workIds.value.map((id) => store.works.get(id)?.value).filter(Boolean));
export const allJobs = computed(() => store.jobIds.value.map((id) => store.jobs.get(id)?.value).filter(Boolean));
export const allSessions = computed(() => store.sessionIds.value.map((id) => store.sessions.get(id)?.value).filter(Boolean));

// ─── Refresh ────────────────────────────────────────────────────────────────

const ENDPOINTS = ["/api/v1/workers", "/api/v1/archive-intake", "/api/v1/archivist-hosts", "/api/v1/archivist-sessions", "/api/v1/jobs", "/api/v1/work"];
const lastText = new Map();
let refreshSequence = 0;

// read returns the endpoint's data, or undefined when its text is unchanged.
async function read(path) {
  const headers = { Accept: "application/json" };
  if (path === "/api/v1/archive-intake") {
    const tokens = Object.fromEntries([...inventoryCache].map(([id, entry]) => [id, entry.token]));
    // HTTP header values must remain ASCII even for a non-Latin drive ID.
    headers["X-Archive-Inventory"] = JSON.stringify(tokens).replace(/[^\x20-\x7e]/g, (ch) => "\\u" + ch.charCodeAt(0).toString(16).padStart(4, "0"));
  }
  const response = await fetch(path, { headers });
  const text = await response.text();
  if (!response.ok) {
    let code = `HTTP_${response.status}`;
    try { code = JSON.parse(text).error?.code || code; } catch { /* not JSON */ }
    throw new Error(code);
  }
  if (lastText.get(path) === text) return undefined;
  const payload = JSON.parse(text);
  return { path, text, data: payload && typeof payload === "object" && "data" in payload ? payload.data : payload };
}

// lanesMoved says whether a work changes lane, appears or goes away.
function lanesMoved(list) {
  const before = new Map(store.workIds.peek().map((id) => [id, workLane(store.works.get(id)?.peek())]));
  return list.length !== before.size || list.some((item) => before.get(item.work_id) !== workLane(item));
}

function apply(answers) {
  const [workers, drives, hosts, sessions, jobs, work] = answers;
  const write = () => batch(() => {
    if (workers) setValue(store.workers, "workers", workers.data || []);
    if (drives) applyDrives(drives.data);
    if (hosts) setValue(store.hosts, "hosts", hosts.data || []);
    if (sessions) applyKeyed(store.sessions, "s", sessions.data, store.sessionIds, (item) => item.session_id);
    if (jobs) applyKeyed(store.jobs, "j", jobs.data, store.jobIds, (item) => item.job_id);
    if (work) applyKeyed(store.works, "w", work.data, store.workIds, (item) => item.work_id);
    for (const answer of answers) if (answer) lastText.set(answer.path, answer.text);
    store.failures.value = 0;
    store.refreshedAt.value = Date.now();
    store.ready.value = true;
  });
  // A work that changes lane moves between the lists as one row.
  if (store.ready.peek() && work && lanesMoved(work.data || [])) return transition("list", write);
  write();
}

// The polling projection may replace a changed conversation subtree. Keep a
// person who is composing in the same editor, at the same caret, instead of
// letting a five-second refresh move focus back to the document.
function captureComposerFocus() {
  const element = document.activeElement;
  if (!(element instanceof HTMLTextAreaElement) || element.id !== "archivist-message") return null;
  return {
    element,
    start: element.selectionStart,
    end: element.selectionEnd,
    direction: element.selectionDirection,
  };
}

function restoreComposerFocus(saved) {
  if (!saved) return;
  const element = document.getElementById("archivist-message");
  if (!(element instanceof HTMLTextAreaElement) || element.disabled) return;
  if (document.activeElement === element) return;
  // Never steal focus if the person moved to another control while the
  // network reads were in flight.
  if (document.activeElement !== document.body && document.activeElement !== saved.element) return;
  element.focus({ preventScroll: true });
  element.setSelectionRange(saved.start, saved.end, saved.direction || "none");
}

export async function refresh() {
  const sequence = ++refreshSequence;
  try {
    const answers = await Promise.all(ENDPOINTS.map(read));
    if (sequence !== refreshSequence) return;
    const composer = captureComposerFocus();
    await apply(answers);
    restoreComposerFocus(composer);
    trackProblems();
  } catch (error) {
    if (sequence !== refreshSequence) return;
    store.failures.value += 1;
  }
  setLocked(stale.peek());
}

export function startPolling() {
  refresh();
  setInterval(() => {
    if (!document.hidden) refresh();
  }, REFRESH_MS);
  // A tab shown again asks at once instead of waiting for the next tick.
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) refresh();
  });
}

// ─── Drives ─────────────────────────────────────────────────────────────────

const RUN_PHASE = { STAGE_ONE_RUNNING: "stage1", TRANSCRIPTION_READY: "stt", STAGE_TWO_RUNNING: "stage2", COMPLETED: "done" };
export const PHASE_LABEL = { intake: "아직 작업 전", stage1: "영상 정리·저장 중", stt: "자막·텍스트 자동 시작 중", stage2: "자막·텍스트 만드는 중", done: "최근 작업 완료" };
// The transcription gate is a transient Control-owned continuation, not a
// person decision.
export const PHASE_ICON = { intake: "idle", stage1: "work", stt: "work", stage2: "work", done: "done" };

export function drivePhase(current) {
  return current?.archive_run_id ? RUN_PHASE[current.archive_run_state] || "stage1" : "intake";
}

export function driveOutcome(current) {
  const work = workOf(current.archive_run_id);
  if (work) return { text: workText(work), kind: workKind(work) };
  if (current.folders?.some((folder) => folderState(folder).signal === "fault")) return { text: WORK_TEXT.FAILED, kind: "fault" };
  const phase = drivePhase(current);
  return { text: PHASE_LABEL[phase], kind: PHASE_ICON[phase] };
}

export function driveName(current) {
  if (!current) return "";
  const source = (current.storage_candidates || []).find((item) => item.storage_id === current.source_storage_id);
  return source?.label || source?.model || current.workspace_drive_id;
}

export function driveNameOf(id) {
  return driveName(drive(id)) || id || "";
}

export function driveConnection(current) {
  const state = current?.connection_state || "UNKNOWN";
  return { state, text: label("volumeConnection", state), signal: stateSignal(state) };
}

// driveProgress is explicitly the current/latest ArchiveRun scope. The whole
// discovered inventory is a separate denominator and must never inherit a
// run's 100% as if the whole source volume had been archived.
export function driveProgress(current) {
  const folders = current.folders || [];
  const total = folders.reduce((sum, folder) => sum + (folder.video_count || 0), 0);
  const storedFolders = folders.filter((folder) => folder.archive_state);
  const stored = storedFolders.reduce((sum, folder) => sum + (folder.video_count || 0), 0);
  return {
    total, stored, storedFolders: storedFolders.length, folders: folders.length,
    discoveredFolders: current.discovered_folder_count || current.inventoryFolders?.length || 0,
    pct: total ? Math.round((stored / total) * 100) : 0,
  };
}

export function isRunning(current) {
  return Boolean(current?.archive_run_id) && current.archive_run_state !== "COMPLETED";
}

export function folderDiscoveryReady(current) {
  return current?.folder_discovery_state === "READY" && Boolean(current.folder_discovery_revision);
}

export function folderDiscoveryText(current) {
  const folders = formatCount(current?.discovered_folder_count || 0);
  const videos = formatCount(current?.discovered_file_count || 0);
  if (current?.folder_discovery_state === "SCANNING") return `검색 중 · ${folders} 폴더 · ${videos} 영상`;
  if (current?.discovery_gap) return `읽지 못한 폴더 ${current.discovery_gap.unreadable_folder_count}개 · ${current.discovery_gap.first_unreadable_folder}`;
  if (current?.folder_discovery_state === "FAILED") return "검색 실패";
  if (folderDiscoveryReady(current)) return `${folders} 폴더 · ${videos} 영상`;
  return "검색 대기";
}

export function archiveCandidates(current) {
  return (current?.storage_candidates || [])
    .filter((item) => item.role === "ARCHIVE_PRIMARY" && item.storage_id !== current.source_storage_id)
    .sort((left, right) => left.storage_id.localeCompare(right.storage_id));
}

// ─── Problems: failures and human waits, with the place that resolves them ──

// folderProblem reads stored StepState and WaitReason only. Resource waits and
// normal progress are not problems.
export function folderProblem(folder) {
  const stages = folder.stages || [];
  const failed = stages.find((stage) => stage.state === "FAILED");
  const waiting = stages.find((stage) => stage.state === "WAITING" && humanWait.has(stage.wait_reason));
  if (failed) return { kind: "fault", signal: "fault", title: "처리 실패", word: "실패", token: "실패", stage: failed };
  if (folder.disposition === "HOLD") return { kind: "hold", signal: "decide", title: "처리 방법 선택 필요", word: "처리 방법 선택 필요", token: "선택", reason: waiting?.wait_reason || "" };
  if (waiting?.wait_reason === "DEVICE") return { kind: "device", signal: "decide", title: "저장 위치 연결 필요", word: "저장 위치 연결 필요", token: "연결", reason: "DEVICE", stage: waiting, target: waiting.wait_target_storage_id || "" };
  if (waiting) {
    const word = waiting.wait_reason === "APPROVAL" ? "승인 필요" : "직접 입력 필요";
    return { kind: "wait", signal: "decide", title: word, word, token: "대기", reason: waiting.wait_reason, stage: waiting };
  }
  const reviews = (folder.transcript_qualities || []).filter((quality) => quality.state === "REVIEW_RECOMMENDED");
  if (reviews.length) return { kind: "review", signal: "decide", title: "자막·텍스트 확인 권장", word: "자막·텍스트 확인 권장", token: "확인" };
  return null;
}

const SIGNAL_RANK = { fault: 2, decide: 1 };

// driveProblems lists a drive's folder problems, failures first.
export function driveProblems(current) {
  return (current?.folders || []).map((folder) => [folder, folderProblem(folder)]).filter(([, problem]) => problem)
    .sort(([, left], [, right]) => (SIGNAL_RANK[right.signal] || 0) - (SIGNAL_RANK[left.signal] || 0));
}

// folderState is the one word a folder row shows when nothing needs a person.
export function folderState(folder) {
  if (folder.disposition === "EXCLUDE_FROM_RUN") return { word: "이번 작업에서 제외", signal: "done" };
  const stages = folder.stages || [];
  if (folder.step_state === "FAILED" || stages.some((stage) => stage.state === "FAILED")) return { word: "실패", signal: "fault" };
  if (folder.disposition === "HOLD" || stages.some((stage) => stage.state === "WAITING" && humanWait.has(stage.wait_reason))) return { word: "직접 확인 필요", signal: "decide" };
  if (!stages.length) return { word: "—", signal: "idle" };
  if (stages.some((stage) => activeStates.has(stage.state) || (stage.state === "WAITING" && !humanWait.has(stage.wait_reason)))) return { word: "진행 중", signal: "work" };
  if (stages.every((stage) => stage.state === "COMPLETED")) return { word: folder.archive_state ? "" : "완료", signal: "done" };
  return { word: "시작 대기", signal: "idle" };
}

// folderSignal is the one signal of a folder: its problem, its stored record,
// or the state of its steps.
export function folderSignal(folder) {
  const problem = folderProblem(folder);
  if (problem) return problem.signal;
  if (folder.archive_state) return "done";
  return folderState(folder).signal;
}

// folderProgress is the share of a running folder's current step that is done.
export function folderProgress(folder) {
  const stages = folder.stages || [];
  if (!stages.length) return 0;
  if (stages.every((stage) => stage.state === "COMPLETED")) return 1;
  const done = stages.filter((stage) => stage.state === "COMPLETED").length;
  const now = stages.find((stage) => !finishedSteps.has(stage.state));
  const part = now?.total_count ? (now.completed_count || 0) / now.total_count : 0;
  return Math.min(1, (done + part) / stages.length);
}

function pendingCards() {
  return allSessions.value.flatMap((session) => (session.action_cards || []).filter((card) => card.state === "PENDING").map((card) => ({ card, session })));
}

// Jobs that belong to no ArchiveRun, such as Template Jobs, stay their own unit.
export const standaloneJobs = computed(() => {
  const standalone = new Set(allWork.value.filter((item) => item.kind === "JOB").map((item) => item.work_id));
  return allJobs.value.filter((job) => jobLane(job) && standalone.has(job.job_id));
});

// problemItems lists every failure and human wait once, keyed so a newly
// appearing one can be told apart from those already on screen.
export const problemItems = computed(() => {
  const items = [];
  const cards = pendingCards();
  for (const current of allDrives.value) {
    const base = { driveID: current.workspace_drive_id, workerID: current.worker_id };
    if (current.connection_state === "DISCONNECTED") {
      items.push({ ...base, key: `drive-disconnected:${current.workspace_drive_id}`, kind: "drive", signal: "fault", title: "드라이브 연결 해제됨" });
    }
    for (const folder of current.folders || []) {
      const problem = folderProblem(folder);
      if (problem) items.push({ ...base, key: `${problem.kind}:${current.workspace_drive_id}:${folder.folder}`, kind: problem.kind, signal: problem.signal, title: problem.title, folder: folder.folder, target: problem.target || "" });
    }
  }
  for (const worker of store.workers.value) {
    if (stateSignal(worker.state) === "fault") items.push({ key: `worker:${worker.worker_id}`, kind: "worker", signal: "fault", title: "작업 PC 연결 끊김", workerID: worker.worker_id });
  }
  for (const host of store.hosts.value) {
    if (stateSignal(host.state) === "fault") items.push({ key: `host:${host.archivist_host_id}`, kind: "host", signal: "fault", title: "에이전트 실행 PC 연결 끊김", hostID: host.archivist_host_id });
  }
  for (const job of standaloneJobs.value) {
    const lane = jobLane(job);
    if (lane !== "fault" && lane !== "decide") continue;
    const title = lane === "fault" ? "작업 실패" : job.wait_reason ? `작업 ${label("wait", job.wait_reason)} 대기` : "작업 사람 대기";
    items.push({ key: `job:${job.job_id}`, kind: "job", signal: lane, title, jobID: job.job_id, workerID: job.worker_id || "" });
  }
  for (const { card, session } of cards) {
    items.push({ key: `card:${card.action_card_id}`, kind: "card", signal: "decide", title: "에이전트 요청 확인 필요", cardID: card.action_card_id, sessionID: session.session_id, label: label("card", card.command_type) });
  }
  // A past ArchiveRun is no longer present in its drive's current-scope
  // projection. Keep an unresolved failed work visible in the global bell;
  // a run with a persisted replacement link has already been handed over.
  for (const work of allWork.value) {
    if (work.kind !== "ARCHIVE_RUN" || workLane(work) !== "fault" || workDrive(work)) continue;
    items.push({ key: `work:${work.work_id}`, kind: "work", signal: "fault", title: "영상 정리·보관 실패", workID: work.work_id, driveID: work.source_volume_id || "" });
  }
  return items.sort((left, right) => SIGNAL_RANK[right.signal] - SIGNAL_RANK[left.signal]);
});

// Acknowledging an alert silences that occurrence without changing the work
// or fault itself. The work remains available at its normal management
// surface, and the key becomes eligible for notification again after the
// underlying problem disappears.
export const visibleProblems = computed(() => {
  const acknowledged = ui.acknowledgedProblems.value;
  return problemItems.value.filter((item) => !acknowledged.has(item.key));
});

export function acknowledgeProblem(key) {
  key = String(key || "").trim();
  if (!key) return;
  const next = new Set(ui.acknowledgedProblems.peek());
  next.add(key);
  writeAcknowledgedProblems(next);
  batch(() => {
    ui.acknowledgedProblems.value = next;
    const fresh = new Set(ui.newProblems.peek());
    fresh.delete(key);
    ui.newProblems.value = fresh;
    if (ui.fresh.peek() === key) ui.fresh.value = "";
  });
}

// problemSubject names one problem: the drive and folder, the Worker, the Job.
export function problemSubject(item) {
  if (item.driveID) {
    const name = driveNameOf(item.driveID);
    return item.folder ? `${name} / ${item.folder}` : name;
  }
  if (item.kind === "work") return workTitle(workOf(item.workID));
  if (item.kind === "job") return jobOf(item.jobID)?.target_title || item.jobID;
  return item.workerID || item.hostID || item.label || "";
}

// problemPath is the route where a person resolves the problem.
export function problemPath(item) {
  if (item.kind === "work") return `#/runs/${encodeURIComponent(item.workID)}`;
  if (item.driveID) return `#/drives/${encodeURIComponent(item.driveID)}${item.folder ? `?folder=${encodeURIComponent(item.folder)}` : ""}`;
  if (item.kind === "worker") return `#/connections?worker=${encodeURIComponent(item.workerID)}`;
  if (item.kind === "job") return `#/jobs/${encodeURIComponent(item.jobID)}`;
  if (item.kind === "host") return `#/connections?host=${encodeURIComponent(item.hostID)}`;
  return "";
}

const seenProblems = new Set();
let primed = false;
let freshTimer = 0;

// trackProblems marks problems that appeared after the first load; the newest
// one rises as a toast for a few seconds.
function trackProblems() {
  const rawKeys = problemItems.peek().map((item) => item.key);
  const current = new Set(rawKeys);
  const acknowledged = new Set([...ui.acknowledgedProblems.peek()].filter((key) => current.has(key)));
  if (acknowledged.size !== ui.acknowledgedProblems.peek().size) {
    writeAcknowledgedProblems(acknowledged);
    ui.acknowledgedProblems.value = acknowledged;
  }
  for (const key of [...seenProblems]) if (!current.has(key)) seenProblems.delete(key);
  const keys = rawKeys.filter((key) => !acknowledged.has(key));
  if (!primed) {
    for (const key of keys) seenProblems.add(key);
    primed = true;
    return;
  }
  const fresh = keys.filter((key) => !seenProblems.has(key));
  if (!fresh.length) return;
  for (const key of fresh) seenProblems.add(key);
  batch(() => {
    ui.newProblems.value = new Set([...ui.newProblems.peek(), ...fresh]);
    ui.fresh.value = fresh[fresh.length - 1];
  });
  clearTimeout(freshTimer);
  freshTimer = setTimeout(() => { ui.fresh.value = ""; }, 8000);
}

// ─── Work and Jobs ──────────────────────────────────────────────────────────

// jobLane places a Job by its stored StepState and WaitReason only.
export function jobLane(job) {
  if (!job) return "";
  if (job.step_state === "FAILED") return "fault";
  if (job.step_state === "WAITING") return stageSignal({ state: "WAITING", wait_reason: job.wait_reason }) === "decide" || job.needs_human_intervention ? "decide" : "resource";
  if (["RUNNING", "RESERVED", "VERIFYING"].includes(job.step_state)) return "work";
  if (finishedSteps.has(job.step_state)) return "";
  return "queue";
}

export function workLane(work) {
  if (work?.replacement_work_id) return "done";
  return { FAILED: "fault", ATTENTION: "decide", ACTIVE: "work", QUEUED: "queue", COMPLETED: "done" }[work?.state] || "queue";
}

export const WORK_TEXT = { FAILED: "실패", ATTENTION: "직접 확인 필요", ACTIVE: "처리 중", QUEUED: "시작 대기", COMPLETED: "완료" };

// workDrive is the drive whose current ArchiveRun is this work, if it still is.
export function workDrive(work) {
  if (work?.kind !== "ARCHIVE_RUN") return null;
  const current = driveView(work.source_volume_id);
  return current?.archive_run_id === work.work_id ? current : null;
}

// workPhase names where an archive work is: its drive's phase while it is the
// drive's current run, its stored state after that.
export function workPhase(work) {
  const current = workDrive(work);
  if (current) return drivePhase(current);
  return work?.state === "COMPLETED" ? "done" : work?.state === "QUEUED" ? "intake" : "stage1";
}

export function workTitle(work) {
  if (!work) return "—";
  if (work.kind === "JOB") return work.title || work.work_id;
  return work.title || driveNameOf(work.source_volume_id) || work.source_storage_id || "원본";
}

const LANE_RANK = { fault: 5, decide: 4, work: 3, resource: 2, queue: 1, done: 0 };
const LANE_KIND = { fault: "fault", decide: "decide", work: "work", resource: "work", queue: "idle", done: "done" };

// workText is a work's state in words: an archive work names its phase.
export function workText(work) {
  if (work.replacement_work_id) return "새 작업으로 넘김";
  if (work.kind === "JOB" || ["QUEUED", "FAILED", "ATTENTION"].includes(work.state)) return WORK_TEXT[work.state] || work.state;
  return PHASE_LABEL[workPhase(work)] || WORK_TEXT[work.state];
}

export function workKind(work) {
  if (work.replacement_work_id) return "done";
  if (work.kind === "ARCHIVE_RUN" && !["QUEUED", "FAILED", "ATTENTION"].includes(work.state)) return PHASE_ICON[workPhase(work)] || "idle";
  return LANE_KIND[workLane(work)] || "idle";
}

// workIssues counts the open problems of an archive work's own folders.
export function workIssues(work) {
  const current = workDrive(work);
  if (!current) return { fault: 0, decide: 0 };
  const problems = driveProblems(current);
  const fault = problems.filter(([, problem]) => problem.signal === "fault").length;
  return { fault, decide: problems.length - fault };
}

export const workHref = (work) => (work.kind === "ARCHIVE_RUN" ? `#/runs/${encodeURIComponent(work.work_id)}` : `#/jobs/${encodeURIComponent(work.work_id)}`);

// The workspace list: unfinished work, what waits on a person first.
export const workspaces = computed(() => allWork.value.filter((work) => ["work", "decide"].includes(workLane(work))).sort((left, right) => {
  const lane = LANE_RANK[workLane(right)] - LANE_RANK[workLane(left)];
  if (lane) return lane;
  const recent = new Date(right.last_event_at || 0) - new Date(left.last_event_at || 0);
  return recent || left.work_id.localeCompare(right.work_id);
}));

// workFirstFolder is the first folder of an archive work's fixed scope, when
// the drive projection or a loaded work detail names it.
export function workFirstFolder(work) {
  const current = workDrive(work);
  const names = current?.scope?.length ? current.scope : workDetails.get(work.work_id)?.state.value.value?.folder_ids || [];
  return names[0] || "";
}

export function workScopeText(work) {
  if (work.kind === "ARCHIVE_RUN" && work.title) {
    const drive = driveNameOf(work.source_volume_id) || work.source_storage_id || "원본";
    return `${drive} · 영상 ${formatCount(work.video_count)}`;
  }
  const count = work.folder_count || 0;
  const first = workFirstFolder(work);
  const folders = first ? `${first}${count > 1 ? ` 외 ${formatCount(count - 1)}개` : ""}` : `${formatCount(count)}개 폴더`;
  return `${folders} · 영상 ${formatCount(work.video_count)}`;
}

// Work and Job details load once per screen visit and refresh when the
// summary they belong to changes.
const workDetails = new Map();
const jobDetails = new Map();

// A loader returns what it holds ({ value, error }) and asks again when the
// summary's version moves; value is undefined until the first answer.
function loader(cache, path, versionOf) {
  return (id) => {
    let entry = cache.get(id);
    if (!entry) {
      entry = { version: "", pending: null, state: signal({ value: undefined, error: "" }) };
      cache.set(id, entry);
    }
    const version = versionOf(id);
    if (entry.version !== version && !entry.pending) {
      entry.version = version;
      entry.pending = api(path(id))
        .then((value) => { entry.state.value = { value: value || null, error: "" }; })
        .catch((error) => { entry.state.value = { value: entry.state.peek().value ?? null, error: error.message }; })
        .finally(() => { entry.pending = null; });
    }
    return entry.state.value;
  };
}

export const workDetail = loader(workDetails, (id) => `/api/v1/work/${encodeURIComponent(id)}`, (id) => {
  const work = workOf(id);
  return work ? `${work.last_event_at}@${work.child_job_count}@${work.result_count}@${work.state}` : id;
});

export const jobDetail = loader(jobDetails, (id) => `/api/v1/jobs/${encodeURIComponent(id)}`, (id) => {
  const job = jobOf(id);
  return job ? `${job.revision}@${job.last_event_at}@${job.step_state}` : id;
});

// The media of a Job is asked for only when its disclosure opens, and kept for
// the rest of the visit.
const media = new Map();
export function jobMedia(jobID) {
  let entry = media.get(jobID);
  if (!entry) {
    entry = signal({ value: undefined, error: "" });
    media.set(jobID, entry);
    api(`/api/v1/jobs/${encodeURIComponent(jobID)}/media`)
      .then((value) => { entry.value = { value: value || [], error: "" }; })
      .catch((error) => { media.delete(jobID); entry.value = { value: null, error: error.message }; });
  }
  return entry.value;
}

// startVisit begins a screen visit: nothing is open yet, and video facts are
// asked for again when a disclosure on the new screen opens.
export function startVisit() {
  media.clear();
  batch(() => {
    for (const field of ["openProblem", "openFolder", "openSteps", "dayFilter", "cancelConfirm", "status"]) ui[field].value = "";
    ui.hold.value = null;
    ui.alertsOpen.value = null;
    ui.agentOpen.value = false;
    ui.scopeOpen.value = false;
    const expanded = [...ui.expanded.peek()].filter((key) => !key.startsWith("media:"));
    if (expanded.length !== ui.expanded.peek().size) ui.expanded.value = new Set(expanded);
  });
}

// linkedFolder finds the drive folder whose stage-one or STT Job this is.
export function linkedFolder(jobID) {
  for (const current of allDrives.value) {
    const folder = (current.folders || []).find((entry) => entry.job_id === jobID || entry.transcription_job_id === jobID);
    if (folder) return { folder, drive: current };
  }
  return null;
}

export function completion(jobID) {
  for (const current of allDrives.value) {
    const item = (current.completed_work || []).find((entry) => entry.job_id === jobID);
    if (item) return { item, drive: current };
  }
  return null;
}

// ─── New work: drive, archive storage and fixed folder scope ────────────────

// startBlocker is the one reason an archive work cannot be requested yet.
export function startBlocker(current, draft, selected) {
  const worker = store.workers.peek().find((item) => item.worker_id === current.worker_id);
  const source = (current.storage_candidates || []).find((item) => item.storage_id === current.source_storage_id);
  const archive = archiveCandidates(current).find((item) => item.storage_id === draft.archiveID);
  const { required: requiredCapacity } = archiveCapacityEstimate(selected);
  if (current.connection_state === "DISCONNECTED") return "드라이브 연결 해제됨";
  if (current.connection_state !== "CONNECTED") return "드라이브 연결 확인 불가";
  if (isRunning(current)) return "이 드라이브에서 작업 진행 중";
  if (current.folder_discovery_state === "SCANNING") return "폴더 검색 중";
  if (current.folder_discovery_state === "FAILED") return "폴더 검색 실패";
  if (!folderDiscoveryReady(current)) return "폴더 검색 대기";
  if (!selected.length) return "처리할 폴더를 선택하세요";
  if (!draft.archiveID) return "저장할 위치를 선택하세요";
  if (!source?.accessible) return "가져올 드라이브를 사용할 수 없음";
  if (!archive?.accessible) return "저장할 위치를 사용할 수 없음";
  if (archive.free_bytes < requiredCapacity) return "저장 공간 부족";
  if (!worker || worker.state === "OFFLINE" || worker.state === "DISABLED") return "작업 PC가 연결되지 않음";
  if (!current.source_volume_id) return "원본 정보 없음";
  return "";
}

export function archiveCapacityEstimate(selected) {
  const estimated = selected.reduce((sum, folder) => sum + (folder.estimated_output_bytes || 0), 0);
  const reserve = Math.max(ARCHIVE_CAPACITY_MINIMUM_RESERVE_BYTES, Math.ceil(estimated / 20));
  return { estimated, reserve, required: estimated + reserve };
}

function freshRunIntent() {
  return { archiveRunID: newID("ARCHIVE-RUN"), archiveRunKey: newID("ARCHIVE-RUN-START"), starting: false };
}

// openNewWork opens the new-work form; a folder handed over from a failure
// arrives with its drive and its original archive storage chosen.
export function openNewWork(driveID = "", folders = [], archiveID = "") {
  ui.newWork.value = {
    kind: "ARCHIVE", driveID: "", revision: 0, archiveID: "", folders: new Set(), intakeSessionID: newID("INTAKE"),
    title: "", template: "occasion-recording", pending: false, error: "", ...freshRunIntent(),
  };
  if (driveID) prepareDrive(driveID, folders, archiveID);
}

export function setDraft(patch) {
  const draft = ui.newWork.peek();
  if (draft) ui.newWork.value = { ...draft, ...patch };
}

// prepareDrive opens a new ArchiveRun scope for one drive. It fixes the human
// volume choice to an ArchiveIntakeSession when the drive has none yet.
export async function prepareDrive(driveID, folders = [], archiveID = "") {
  const current = drive(driveID);
  if (current?.connection_state !== "CONNECTED") return;
  setDraft({
    driveID, revision: current.folder_discovery_revision || 0, archiveID, folders: new Set(folders), error: "",
    intakeSessionID: current.selection_session_id || newID("INTAKE"), ...freshRunIntent(),
  });
  if (current.selection_session_id || !current.source_volume_id || !current.candidate_revision) return;
  try {
    const result = await mutation("/api/v1/archive-intake-sessions", {
      session_id: ui.newWork.peek().intakeSessionID, volume_id: current.source_volume_id, expected_candidate_revision: current.candidate_revision,
    });
    setDraft({ intakeSessionID: result.session_id, error: "원본 탐색 중" });
    await refresh();
  } catch (error) {
    setDraft({ error: error.message });
  }
}

// syncDraft keeps the picks valid for the drive's current discovery revision.
export function syncDraft(current, draft) {
  let next = draft;
  if (folderDiscoveryReady(current) && draft.revision && draft.revision !== current.folder_discovery_revision) {
    next = { ...next, revision: current.folder_discovery_revision, folders: new Set(), ...freshRunIntent() };
  }
  const known = new Set(folderDiscoveryReady(current) ? current.inventory : []);
  const folders = new Set([...next.folders].filter((folder) => known.has(folder)));
  if (folders.size !== next.folders.size) next = { ...next, folders };
  if (next.archiveID && !archiveCandidates(current).some((item) => item.storage_id === next.archiveID)) next = { ...next, archiveID: "" };
  if (next !== draft) ui.newWork.value = next;
  return next;
}

export function resetRunIntent() {
  setDraft(freshRunIntent());
}

// driveSelection is the structured reference an ArchivistTurn carries; it is
// never inferred from message text. A drive with an ArchiveRun hands over the
// run's fixed scope, a drive being prepared hands over the person's picks.
export function driveSelection(current, draft) {
  if (!current) return null;
  const preparing = draft?.kind === "ARCHIVE" && draft.driveID === current.workspace_drive_id && !isRunning(current);
  const fixed = Boolean(current.archive_run_id) && !preparing;
  if (!fixed && !folderDiscoveryReady(current)) return null;
  const archiveID = fixed ? current.archive_storage_id : draft?.archiveID;
  const folders = fixed ? [...current.scope] : [...(draft?.folders || [])];
  if (!archiveID || !folders.length) return null;
  return {
    archive_run_id: fixed ? current.archive_run_id : undefined,
    archive_run_revision: fixed ? current.archive_run_revision : undefined,
    worker_id: current.worker_id, workspace_drive_id: current.workspace_drive_id,
    intake_session_id: fixed ? current.session_id : draft.intakeSessionID,
    source_volume_id: current.source_volume_id, source_storage_id: current.source_storage_id, archive_storage_id: archiveID,
    candidate_revision: current.candidate_revision || current.input_revision,
    folder_discovery_revision: current.folder_discovery_revision || current.input_revision,
    folder_ids: folders.sort(), selected_by: "browser-principal",
  };
}

export function selectionRefs(selection) {
  return selection
    ? [selection.worker_id, selection.workspace_drive_id, selection.source_storage_id, selection.archive_storage_id, `${(selection.folder_ids || []).length} 폴더`].filter(Boolean)
    : [];
}

// ─── Archivist ──────────────────────────────────────────────────────────────

export const currentSession = computed(() => {
  const sessions = allSessions.value;
  return sessions.find((item) => item.session_id === ui.sessionID.value) || sessions[0] || null;
});

export function hostFor(session) {
  return store.hosts.value.find((host) => host.archivist_host_id === session?.archivist_host_id);
}

export function activeTurn(session) {
  return [...(session?.turns || [])].reverse().find((turn) => !finishedTurns.has(turn.state));
}

export function availability(session) {
  const host = hostFor(session);
  if (!session) return ["에이전트 없음", "idle"];
  if (host?.state !== "ONLINE") return ["실행 PC 연결 끊김", "fault"];
  if (activeTurn(session)) return ["처리 중", "work"];
  return ["요청 가능", "online"];
}
