// Shared helpers for every Archive Workspace screen: API access, labels, the
// signal vocabulary the screens colour by, and number and time formats.

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
  drive: { UNCLASSIFIED: "아직 처리 안 함", IN_PROGRESS: "처리 중", HAS_HOLD: "처리 방법 선택 필요", COMPLETED: "최근 작업 완료", CONTENT_CHANGED: "드라이브 내용 변경" },
  run: { STAGE_ONE_RUNNING: "영상 정리·저장 중", TRANSCRIPTION_READY: "자막·텍스트 만들기 가능", STAGE_TWO_RUNNING: "자막·텍스트 만드는 중", COMPLETED: "작업 완료" },
  disposition: { ORGANIZE_AS_IS: "원본 그대로 저장", RENDER_RECORDINGS: "영상별 변환", COMPILE_SOURCE_SEQUENCE: "여러 영상 합치기", HOLD: "처리 방법 선택 필요" },
  wait: { INPUT: "직접 입력", APPROVAL: "승인", DEVICE: "장비 연결", MEMORY: "메모리 여유", SCRATCH: "작업 공간", SLOT: "처리 순서", DISK: "디스크 여유", IDLE: "시작 시점", PRIORITY: "우선순위" },
  hold: {
    NO_MEDIA: "영상 없음", MISSING_INSPECTION: "검사 없음", AMBIGUOUS_MULTIPLE_VIDEOS: "영상 여러 개", MIXED_READY_STATE: "상태 혼재",
    NONCONSECUTIVE_SEQUENCE: "영상 순서가 이어지지 않음", INCOMPATIBLE_SEQUENCE: "영상 형식이 서로 다름", INVALID_OVERRIDE: "선택한 처리 방법을 적용할 수 없음",
  },
  stage: {
    classification: "처리 방식 결정", markers: "영상 구간 정리", render: "영상 변환", qc: "결과 검사", archive: "결과 저장", gate: "자막·텍스트 시작", audio: "음성 준비",
    transcript: "음성을 텍스트로 변환", transcript_text: "텍스트 파일 생성", subtitle: "자막 생성", add: "결과에 추가", catalog: "영상 목록 갱신", job_pending: "작업 준비",
  },
  step: { QUEUED: "시작 대기", WAITING: "대기", RESERVED: "시작 준비", RUNNING: "처리 중", VERIFYING: "결과 확인 중", PAUSED: "일시 정지", FAILED: "실패", COMPLETED: "완료", CANCELLED: "취소" },
  worker: { ONLINE: "연결됨", BUSY: "처리 중", DRAINING: "정리 중", OFFLINE: "꺼짐", DISABLED: "사용 안 함" },
  host: { ONLINE: "연결됨", BUSY: "처리 중", CONNECTING: "연결 중", OFFLINE: "꺼짐", DISABLED: "사용 안 함" },
  turn: { QUEUED: "시작 대기", LEASED: "전달 중", SUBMITTED: "전달됨", RUNNING: "처리 중", WAITING: "대기", COMPLETED: "완료", FAILED: "실패", CANCELLED: "취소" },
  tool: {
    "archive.intake.selection.get.v1": "선택 내용 확인", "archive.run.status.v1": "진행 상황 확인", "archive.run.start.v1": "영상 정리·보관 시작",
    "archive.run.transcription.start.v1": "자막·텍스트 만들기", "archive.jobs.get.v1": "작업 확인", "archive.work.reference.get.v1": "이어갈 작업 확인",
    "archive.assets.search.v1": "보관 영상 검색", "archive.jobs.create.v1": "작업 추가",
  },
  call: { COMPLETED: "완료", ACTION_CARD: "결정 요청", FAILED: "실패", RUNNING: "처리 중" },
  card: { "archive_run.start": "영상 정리·보관 시작", "archive_run.transcription.start": "자막·텍스트 만들기", "archive_intake.selection.required": "작업 대상 선택 필요", "job.create": "작업 추가" },
  cardState: { PENDING: "실행 전 확인 필요", COMPLETED: "진행함", REJECTED: "진행하지 않음" },
  storageRole: { SOURCE_MEDIA: "가져올 영상", ARCHIVE_PRIMARY: "결과 저장 위치" },
  storageAccess: { LOCAL: "로컬 저장소", MOUNTED_NETWORK: "네트워크 드라이브", ATTACHED_VOLUME: "새 드라이브", FTP: "FTP", SFTP: "SFTP" },
  volumeConnection: { CONNECTED: "연결됨", DISCONNECTED: "연결 해제됨", UNKNOWN: "확인 불가" },
  transcriptQuality: { SEGMENT_TOO_LONG: "음성 인식 구간이 김", LOW_TEXT_DENSITY: "인식된 글자가 적음" },
  admission: { OFFLINE: "연결되지 않음", DISABLED: "사용 안 함", NOT_ACCEPTING_WORK: "새 작업을 받지 않음" },
  template: { "occasion-recording": "행사 기록", "vj-compilation": "VJ 모음", "final-edit": "최종 편집", "large-file-intake": "대용량 입고" },
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

// Intl formatters are built once: building one costs far more than using it.
const COUNT = new Intl.NumberFormat("ko-KR");
const SHORT = new Intl.DateTimeFormat("ko-KR", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", hour12: false });
const CLOCK = new Intl.DateTimeFormat("ko-KR", { hour: "2-digit", minute: "2-digit", hour12: false });
const CLOCK_SECONDS = new Intl.DateTimeFormat("ko-KR", { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false });
const DAY = new Intl.DateTimeFormat("ko-KR", { year: "numeric", month: "2-digit", day: "2-digit" });
const MONTH_DAY = new Intl.DateTimeFormat("ko-KR", { month: "2-digit", day: "2-digit" });

export function formatCount(value) {
  return COUNT.format(value || 0);
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
export function knownTime(value) {
  if (!value) return null;
  const date = new Date(value);
  return Number.isNaN(date.getTime()) || date.getUTCFullYear() < 2000 ? null : date;
}

export function shortTime(value) {
  const date = knownTime(value);
  if (!date) return "—";
  return SHORT.format(date);
}

export function clockTime(value, seconds = false) {
  const date = knownTime(value);
  if (!date) return "—";
  return (seconds ? CLOCK_SECONDS : CLOCK).format(date);
}

export function dateOnly(date) {
  return DAY.format(date);
}

// dayTime is a list row's time: the clock for today, the date for another day.
export function dayTime(value) {
  const date = knownTime(value);
  if (!date) return "—";
  return DAY.format(date) === DAY.format(new Date()) ? CLOCK.format(date) : MONTH_DAY.format(date);
}
