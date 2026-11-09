// Shared helpers for every Archive Workspace screen: API access, DOM
// construction, labels, status icons and the signal vocabulary the screens
// colour by.

export const byId = (id) => document.getElementById(id);

let csrfToken = "";
let csrfRequest = null;
let locked = false;
const pendingMutations = new Set();

// setLocked holds every Command while the screen is no longer refreshed, so a
// decision is never made from stale state.
export function setLocked(value) {
  locked = value;
}

export async function api(path, options = {}) {
  const response = await fetch(path, { ...options, headers: { Accept: "application/json", ...(options.headers || {}) } });
  const payload = await response.json();
  if (!response.ok) throw new Error(payload.error?.code || `HTTP_${response.status}`);
  return payload && typeof payload === "object" && "data" in payload ? payload.data : payload;
}

export async function mutation(path, payload) {
  if (locked) throw new Error("연결 끊김");
  if (pendingMutations.has(path)) throw new Error("요청 처리 중");
  pendingMutations.add(path);
  try {
    if (!csrfToken) {
      if (!csrfRequest) csrfRequest = api("/api/v1/session").then((session) => {
        csrfToken = session.csrf_token;
        return csrfToken;
      }).finally(() => { csrfRequest = null; });
      await csrfRequest;
    }
    const response = await fetch(path, {
      method: "POST", credentials: "same-origin",
      headers: { Accept: "application/json", "Content-Type": "application/json", "X-CSRF-Token": csrfToken },
      body: JSON.stringify(payload),
    });
    const result = await response.json();
    if (!response.ok) throw new Error(result.error?.code || `HTTP_${response.status}`);
    return result.data;
  } finally {
    pendingMutations.delete(path);
  }
}

function uuidV4() {
  if (typeof globalThis.crypto?.randomUUID === "function") return globalThis.crypto.randomUUID();
  const bytes = new Uint8Array(16);
  globalThis.crypto.getRandomValues(bytes);
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = [...bytes].map((value) => value.toString(16).padStart(2, "0"));
  return `${hex.slice(0, 4).join("")}-${hex.slice(4, 6).join("")}-${hex.slice(6, 8).join("")}-${hex.slice(8, 10).join("")}-${hex.slice(10).join("")}`;
}

export function newID(prefix) {
  return `${prefix}-${uuidV4()}`;
}

export function node(tag, value = "", className = "") {
  const element = document.createElement(tag);
  element.textContent = value;
  if (className) element.className = className;
  return element;
}

// el builds an element from attributes and children. `on*` keys attach
// listeners, `dataset` and `aria` take objects, everything else is an attribute.
export function el(tag, attributes = {}, ...children) {
  const element = document.createElement(tag);
  for (const [key, value] of Object.entries(attributes)) {
    if (value === undefined || value === null || value === false) continue;
    if (key === "class") element.className = value;
    else if (key === "text") element.textContent = value;
    else if (key === "dataset") {
      for (const [name, data] of Object.entries(value)) if (data !== undefined && data !== null) element.dataset[name] = String(data);
    }
    else if (key.startsWith("on")) element.addEventListener(key.slice(2).toLowerCase(), value);
    else element.setAttribute(key, value === true ? "" : String(value));
  }
  element.append(...children.flat().filter((child) => child !== null && child !== undefined && child !== false));
  return element;
}

export function svgIcon(path, box = "0 0 24 24", className = "") {
  const namespace = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(namespace, "svg");
  const shape = document.createElementNS(namespace, "path");
  svg.setAttribute("viewBox", box);
  svg.setAttribute("aria-hidden", "true");
  if (className) svg.setAttribute("class", className);
  shape.setAttribute("d", path);
  svg.append(shape);
  return svg;
}

export const icons = {
  search: "M10.5 4a6.5 6.5 0 1 0 0 13a6.5 6.5 0 1 0 0-13zM15.5 15.5 20 20",
  chat: "M4 5h16v11H10l-4 4v-4H4z",
  settings: "M12 15.5a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7ZM19.4 15a1.7 1.7 0 0 0 .34 1.88l.06.06-2.83 2.83-.06-.06a1.7 1.7 0 0 0-1.88-.34 1.7 1.7 0 0 0-1.03 1.56V21h-4v-.08A1.7 1.7 0 0 0 9 19.37a1.7 1.7 0 0 0-1.88.34l-.06.06-2.83-2.83.06-.06A1.7 1.7 0 0 0 4.63 15 1.7 1.7 0 0 0 3.08 14H3v-4h.08A1.7 1.7 0 0 0 4.63 9a1.7 1.7 0 0 0-.34-1.88l-.06-.06 2.83-2.83.06.06A1.7 1.7 0 0 0 9 4.63 1.7 1.7 0 0 0 10 3.08V3h4v.08A1.7 1.7 0 0 0 15 4.63a1.7 1.7 0 0 0 1.88-.34l.06-.06 2.83 2.83-.06.06A1.7 1.7 0 0 0 19.37 9 1.7 1.7 0 0 0 20.92 10H21v4h-.08A1.7 1.7 0 0 0 19.4 15Z",
  plus: "M12 5v14M5 12h14",
  minus: "M6 12h12",
  send: "m4 4 17 8-17 8 3-8-3-8Zm3 8h14",
  refresh: "M20 11a8 8 0 1 0-2.34 5.66M20 4v7h-7",
  next: "m9 6 6 6-6 6",
  back: "m15 6-6 6 6 6",
  expand: "m6 9 6 6 6-6",
  status: "M3 12h4l2.5-6 5 12 2.5-6h4",
  drives: "M3 6.5h18v11H3zM7 12h.01M10.5 12h6.5",
  jobs: "M7 4h10v3h3v14H4V7h3zM9 4v3h6V4M8 12h8M8 16h8",
  workers: "M4 4h16v6H4zM4 14h16v6H4zM8 7h.01M8 17h.01",
  slash: "M15 4 9 20",
  close: "M6 6l12 12M18 6 6 18",
  up: "M12 19V5M6 11l6-6 6 6",
};

export const signals = {
  FAILED: "fault", OFFLINE: "fault", RUNNING: "work", RESERVED: "work", VERIFYING: "work", BUSY: "work",
	CONNECTING: "work", LEASED: "work", SUBMITTED: "work", COMPLETED: "done", CONNECTED: "done", ONLINE: "done",
	DISCONNECTED: "fault", HAS_HOLD: "decide",
};
export const humanWait = new Set(["INPUT", "APPROVAL", "DEVICE"]);
export const activeStates = new Set(["RUNNING", "RESERVED", "VERIFYING"]);
export const finishedTurns = new Set(["COMPLETED", "FAILED", "CANCELLED"]);
export const finishedSteps = new Set(["COMPLETED", "FAILED", "CANCELLED"]);

export const labels = {
  drive: { UNCLASSIFIED: "미분류", IN_PROGRESS: "진행 중", HAS_HOLD: "보류 있음", COMPLETED: "완료", CONTENT_CHANGED: "내용 바뀜" },
  run: { STAGE_ONE_RUNNING: "1단계 진행", TRANSCRIPTION_READY: "STT 가능", STAGE_TWO_RUNNING: "2단계 진행", COMPLETED: "완료" },
  disposition: { ORGANIZE_AS_IS: "그대로 보관", RENDER_RECORDINGS: "개별 렌더", COMPILE_SOURCE_SEQUENCE: "소스 시퀀스", HOLD: "보류" },
  wait: { INPUT: "입력", APPROVAL: "승인", DEVICE: "장치", MEMORY: "메모리", SCRATCH: "작업 공간", SLOT: "슬롯", DISK: "디스크", IDLE: "유휴", PRIORITY: "우선순위" },
  hold: {
    NO_MEDIA: "영상 없음", MISSING_INSPECTION: "검사 없음", AMBIGUOUS_MULTIPLE_VIDEOS: "영상 여러 개", MIXED_READY_STATE: "상태 혼재",
    NONCONSECUTIVE_SEQUENCE: "시퀀스 불연속", INCOMPATIBLE_SEQUENCE: "시퀀스 불일치", INVALID_OVERRIDE: "판정 무효",
  },
  stage: {
    classification: "분류", markers: "소스 타임라인", render: "렌더", qc: "QC", archive: "보관", gate: "STT 시작", audio: "오디오 추출",
    transcript: "STT", transcript_text: "전사 텍스트", subtitle: "자막", add: "패키지 추가", catalog: "CSV", job_pending: "작업 생성",
  },
  step: { QUEUED: "대기열", WAITING: "대기", RESERVED: "예약", RUNNING: "실행 중", VERIFYING: "검증 중", PAUSED: "일시 정지", FAILED: "실패", COMPLETED: "완료", CANCELLED: "취소" },
  worker: { ONLINE: "연결됨", BUSY: "처리 중", DRAINING: "정리 중", OFFLINE: "꺼짐", DISABLED: "사용 안 함" },
  host: { ONLINE: "연결됨", BUSY: "처리 중", CONNECTING: "연결 중", OFFLINE: "꺼짐", DISABLED: "사용 안 함" },
  turn: { QUEUED: "대기열", LEASED: "전달 중", SUBMITTED: "전달됨", RUNNING: "처리 중", WAITING: "대기", COMPLETED: "완료", FAILED: "실패", CANCELLED: "취소" },
  tool: {
    "archive.intake.selection.get.v1": "선택 확인", "archive.run.status.v1": "진행 조회", "archive.run.start.v1": "아카이브 실행 시작",
    "archive.run.transcription.start.v1": "STT 시작", "archive.jobs.get.v1": "작업 조회", "archive.work.reference.get.v1": "이어갈 작업 확인",
    "archive.assets.search.v1": "자료 검색", "archive.jobs.create.v1": "작업 생성",
  },
  call: { COMPLETED: "완료", ACTION_CARD: "결정 요청", FAILED: "실패", RUNNING: "처리 중" },
  card: { "archive_run.start": "아카이브 실행 시작", "archive_run.transcription.start": "STT 시작", "archive_intake.selection.required": "선택 필요", "job.create": "작업 생성" },
  cardState: { PENDING: "결정 대기", COMPLETED: "실행됨", REJECTED: "거절됨" },
  storageRole: { SOURCE_MEDIA: "원본", ARCHIVE_PRIMARY: "아카이브" },
  storageAccess: { LOCAL: "로컬 저장소", MOUNTED_NETWORK: "네트워크 드라이브", ATTACHED_VOLUME: "새 드라이브", FTP: "FTP", SFTP: "SFTP" },
	volumeConnection: { CONNECTED: "연결됨", DISCONNECTED: "연결 해제됨", UNKNOWN: "확인 불가" },
  transcriptQuality: { SEGMENT_TOO_LONG: "긴 구간", LOW_TEXT_DENSITY: "낮은 문자 밀도" },
  admission: { OFFLINE: "꺼짐", DISABLED: "사용 안 함", NOT_ACCEPTING_WORK: "정리 중" },
};

export function label(group, value) {
  return labels[group]?.[value] || value || "—";
}

export function stateSignal(state) {
  return signals[state] || "idle";
}

export function stageSignal(stage) {
  if (stage.state === "WAITING" && humanWait.has(stage.wait_reason)) return "decide";
  return stateSignal(stage.state);
}

// Status icons pair a shape and a glyph with the signal colour, so no state is
// told by colour alone: failure ✕ circle, human decision ! diamond, work an
// arc that turns, completion ✓ circle, not started a hollow circle.
const CIRCLE = "M8 1.75a6.25 6.25 0 1 0 0 12.5a6.25 6.25 0 1 0 0-12.5z";
const ARC = "M8 1.75a6.25 6.25 0 0 1 6.25 6.25";
const STATUS_ICON = {
  fault: [CIRCLE, "M5.7 5.7l4.6 4.6M10.3 5.7l-4.6 4.6"],
  decide: ["M8 1.1 14.9 8 8 14.9 1.1 8z", "M8 4.8v3.9M8 11.1v.1"],
  work: [CIRCLE, ARC],
  resource: [CIRCLE, ARC],
  done: [CIRCLE, "M5.1 8.2l2 2 3.9-4.2"],
  idle: [CIRCLE, ""],
  online: ["M8 4.25a3.75 3.75 0 1 0 0 7.5a3.75 3.75 0 1 0 0-7.5z", ""],
};

export function setStatusIcon(svg, kind) {
  const [shape, glyph] = STATUS_ICON[kind] || STATUS_ICON.idle;
  svg.dataset.kind = STATUS_ICON[kind] ? kind : "idle";
  svg.querySelector(".sicon-shape").setAttribute("d", shape);
  svg.querySelector(".sicon-glyph").setAttribute("d", glyph);
  return svg;
}

export function statusIcon(kind, text = "") {
  const namespace = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(namespace, "svg");
  svg.setAttribute("class", "sicon");
  svg.setAttribute("viewBox", "0 0 16 16");
  if (text) {
    svg.setAttribute("role", "img");
    svg.setAttribute("aria-label", text);
  } else svg.setAttribute("aria-hidden", "true");
  for (const part of ["sicon-shape", "sicon-glyph"]) {
    const path = document.createElementNS(namespace, "path");
    path.setAttribute("class", part);
    svg.append(path);
  }
  return setStatusIcon(svg, kind);
}

// workerIcon reads a Worker or Host state: off is a failure, anything else is
// a quiet presence dot.
export function workerIcon(state) {
  return statusIcon(stateSignal(state) === "fault" ? "fault" : "online");
}

// problemCounts shows failures and human waits as icon and count pairs.
export function problemCounts(fault, decide, className = "mini-list") {
  const items = [];
  if (fault) items.push(el("span", { class: "mini", title: `실패 ${fault}` }, statusIcon("fault"), String(fault)));
  if (decide) items.push(el("span", { class: "mini", title: `확인 필요 ${decide}` }, statusIcon("decide"), String(decide)));
  return items.length ? el("span", { class: className }, ...items) : null;
}

export function token(text, signal = "idle", title = "") {
  return el("span", { class: "token", title: title || undefined, dataset: { signal } }, text);
}

export function meter(pct, done = pct === 100) {
  const fill = el("span", { dataset: { done: String(done) } });
  fill.style.width = `${pct}%`;
  return el("span", { class: "meter-bar", role: "img", "aria-label": `${pct}%` }, fill);
}

export function formatCount(value) {
  return new Intl.NumberFormat("ko-KR").format(value || 0);
}

export function formatBytes(value) {
  if (!value) return "0 GB";
  return `${(value / (1024 ** 3)).toFixed(value >= 10 * (1024 ** 3) ? 0 : 1)} GB`;
}

export function formatSize(value) {
  if (!value) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let size = value;
  let unit = 0;
  while (size >= 1024 && unit < units.length - 1) {
    size /= 1024;
    unit += 1;
  }
  return `${size.toFixed(size >= 10 || unit === 0 ? 0 : 1)} ${units[unit]}`;
}

export function formatPercent(value) {
  return `${Number(value || 0).toFixed(Number(value || 0) % 1 ? 1 : 0)}%`;
}

// knownTime rejects empty values and Go zero times ("0001-01-01T00:00:00Z").
function knownTime(value) {
  if (!value) return null;
  const date = new Date(value);
  return Number.isNaN(date.getTime()) || date.getUTCFullYear() < 2000 ? null : date;
}

export function shortTime(value) {
  const date = knownTime(value);
  if (!date) return "—";
  return new Intl.DateTimeFormat("ko-KR", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", hour12: false }).format(date);
}

export function clockTime(value, seconds = false) {
  const date = knownTime(value);
  if (!date) return "—";
  return new Intl.DateTimeFormat("ko-KR", { hour: "2-digit", minute: "2-digit", second: seconds ? "2-digit" : undefined, hour12: false }).format(date);
}

export function emptyState(text = "—") {
  return el("p", { class: "empty" }, text);
}
