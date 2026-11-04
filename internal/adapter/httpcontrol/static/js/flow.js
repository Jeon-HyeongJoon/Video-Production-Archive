// Flow graph of one drive: the ArchiveRun's steps in work order as square
// icon nodes joined by a line that fills as the drive passes each step. A node
// only says how far the drive's folders got through it — not yet, partly, now,
// all — and carries a badge for failures and human waits. Details sit in the
// node's tooltip on hover or focus.

import { el, svgIcon, label, humanWait, activeStates, formatCount, shortTime } from "./core.js";

const STEPS = ["classification", "markers", "render", "qc", "archive", "gate", "audio", "transcript", "fork", "add", "catalog"];

const ICONS = {
  classification: "M3 12h6M9 12l5-6h6M9 12h11M9 12l5 6h6",
  markers: "M3 9h4.5v6H3zM9.5 9h5v6h-5zM16.5 9H21v6h-4.5zM8.5 5.5v13M15.5 5.5v13",
  render: "M4 5h16v14H4zM8 5v14M16 5v14M4 9.5h4M4 14.5h4M16 9.5h4M16 14.5h4",
  qc: "M10.5 4a6.5 6.5 0 1 0 0 13a6.5 6.5 0 1 0 0-13zM15.5 15.5 20 20M7.8 10.6l1.9 1.9 3.3-3.4",
  archive: "M3 5h18v4H3zM5 9v10h14V9M10 13h4",
  gate: "M9 4a3.5 3.5 0 1 0 0 7a3.5 3.5 0 1 0 0-7zM3 20c.5-3.5 3-5.5 6-5.5s5.5 2 6 5.5M16 11.5l2 2 3.5-3.5",
  audio: "M3 12h1.5M6.5 9v6M10 5v14M13.5 8v8M17 10.5v3M20.5 11.5v1",
  transcript: "M4 4h16v12H10l-4 4v-4H4zM8 8.5h8M8 11.5h5",
  subtitle: "M3 5h18v14H3zM7 12.5h6M15 12.5h2M7 15.5h10",
  transcript_text: "M6 3h8l4 4v14H6zM14 3v4h4M9 11h6M9 14h6M9 17h4",
  add: "M3 5h18v4H3zM5 9v10h14V9M12 11.5v5M9.5 14h5",
  catalog: "M3 5h18v14H3zM3 10h18M3 15h18M9 5v14",
};

const INFO = {
  classification: ["영상 기술정보와 정책으로 처리 방식 결정 · 그대로 보관 / 개별 렌더 / 소스 시퀀스 / 보류", "모든 폴더"],
  markers: ["짧은 Clip 순서와 컷 마커 고정 · 통합 렌더 입력", "소스 시퀀스"],
  render: ["1080p 렌더 · 개별 렌더 6Mbps(선택 시 20Mbps) · 소스 시퀀스 20Mbps 통합", "개별 렌더 · 소스 시퀀스"],
  qc: ["결과 영상을 원본 기술정보·렌더 Profile과 대조하는 검사", "보류를 뺀 모든 폴더"],
  archive: ["아카이브 저장소에 복사 · checksum 검증 뒤 보관됨", "보류를 뺀 모든 폴더"],
  gate: ["1단계 실행 작업이 0이면 열림 · 담당자 승인 뒤 2단계 시작", "보관된 그대로 보관 · 개별 렌더"],
  audio: ["STT 입력용 16kHz mono 오디오 추출", "STT 대상"],
  transcript: ["음성을 시간 구간 전사문으로 인식", "STT 대상"],
  subtitle: ["전사문 시간 구간으로 SRT 자막 생성 · 전사 텍스트와 동시 진행", "STT 대상"],
  transcript_text: ["전사문을 시간순으로 합친 전사 텍스트 · 자막과 동시 진행", "STT 대상"],
  add: ["전사문·자막·전사 텍스트를 보관 패키지의 새 리비전으로 게시", "STT 대상"],
  catalog: ["아카이브 카탈로그 · 원본 영상마다 1행 · 1단계와 2단계 뒤 갱신", "모든 원본 영상"],
};

const WORDS = { todo: "아직", part: "일부 지남", now: "지금 진행", done: "모두 지남", closed: "닫힘", ask: "승인 필요" };
const GLYPHS = { fault: "M3.2 3.2l3.6 3.6M6.8 3.2 3.2 6.8", decide: "M5 2.6v3M5 7.3v.1" };

// tally counts the drive's folders at one step by stored StepState and
// WaitReason only.
function tally(drive, stageID) {
  const counts = { done: 0, now: 0, queued: 0, fault: 0, decide: 0, total: 0 };
  for (const folder of drive.folders || []) {
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
  if (!counts.total) return "todo";
  if (counts.now) return "now";
  if (counts.done === counts.total) return "done";
  return counts.done ? "part" : "todo";
}

function tallyLines(counts) {
  return [["완료", counts.done, "done"], ["진행", counts.now, ""], ["대기열", counts.queued, "muted"], ["확인 필요", counts.decide, "decide"], ["실패", counts.fault, "fault"]]
    .filter(([, value]) => value).map(([name, value, tone]) => [`${name} ${formatCount(value)} 폴더`, tone]);
}

function gateState(drive) {
  const state = drive.archive_run_state;
  if (state === "TRANSCRIPTION_READY") return ["ask", `대상 ${formatCount(drive.eligible_transcription_count)} 폴더`];
  if (state === "STAGE_TWO_RUNNING" || state === "COMPLETED") return ["done", "승인됨"];
  if (drive.archive_run_id) return ["closed", `1단계 ${formatCount(drive.stage_one_remaining)} 폴더 남음`];
  return ["todo", ""];
}

function node(drive, id, state, lines, badges = []) {
  const name = label("stage", id);
  const title = `${name} · ${WORDS[state]}`;
  const tipID = `flow-tip-${drive.workspace_drive_id}-${id}`;
  const item = el("span", { class: "flow-node", tabindex: "0", role: "img", "aria-label": title, "aria-describedby": tipID, dataset: { step: id, state } },
    svgIcon(ICONS[id]),
    badges.length ? el("span", { class: "flow-badges" }, ...badges.map(([signal, count]) =>
      el("span", { class: "flow-badge", dataset: { signal } }, svgIcon(GLYPHS[signal], "0 0 10 10"), el("span", {}, String(count))))) : null,
    el("span", { class: "flow-tip", id: tipID, role: "tooltip" },
      el("b", {}, title), el("span", {}, INFO[id][0]), el("small", {}, `적용 · ${INFO[id][1]}`),
      lines.length ? el("ul", {}, ...lines.map(([text, tone]) => el("li", { dataset: { tone: tone || undefined } }, text))) : null));
  return item;
}

function tallyNode(drive, id) {
  const counts = tally(drive, id);
  const badges = [];
  if (counts.fault) badges.push(["fault", counts.fault]);
  if (counts.decide) badges.push(["decide", counts.decide]);
  const state = tallyState(counts);
  return [node(drive, id, state, tallyLines(counts), badges), state];
}

export function flowGraph(drive) {
  let reached = false;
  const steps = STEPS.map((id) => {
    const area = el("span", { class: "flow-area" });
    const name = el("span", { class: "flow-name" });
    const step = el("li", { class: "flow-step", dataset: { step: id } });
    let state;
    if (id === "gate") {
      const [gate, text] = gateState(drive);
      state = gate;
      area.append(node(drive, id, gate, text ? [[text, gate === "ask" ? "decide" : ""]] : []));
      name.textContent = label("stage", id);
      if (gate === "ask") name.dataset.signal = "decide";
    } else if (id === "fork") {
      const pair = el("span", { class: "flow-pair" });
      const states = ["subtitle", "transcript_text"].map((stage) => {
        const [child, childState] = tallyNode(drive, stage);
        pair.append(child);
        return childState;
      });
      step.classList.add("flow-fork");
      area.append(pair);
      name.textContent = `${label("stage", "subtitle")} · ${label("stage", "transcript_text")}`;
      state = states.every((value) => value === "done") ? "done" : states.includes("now") ? "now" : states.some((value) => value !== "todo") ? "part" : "todo";
    } else if (id === "catalog") {
      state = drive.archive_run_state === "COMPLETED" && drive.catalog_row_count ? "done" : drive.catalog_row_count ? "part" : "todo";
      area.append(node(drive, id, state, [[drive.catalog_row_count ? `${formatCount(drive.catalog_row_count)}행 · ${shortTime(drive.catalog_updated_at)} 기록` : "실행 뒤 기록", ""]]));
      name.textContent = label("stage", id);
    } else {
      const [child, childState] = tallyNode(drive, id);
      state = childState;
      area.append(child);
      name.textContent = label("stage", id);
    }
    step.dataset.state = state;
    step.dataset.in = reached ? "on" : "";
    reached = state === "done";
    step.dataset.out = reached ? "on" : "";
    step.append(area, name);
    return step;
  });
  return el("div", { class: "flow-graph", dataset: { driveId: drive.workspace_drive_id } },
    el("div", { class: "flow-stages", "aria-hidden": "true" },
      el("span", { class: "flow-stage", dataset: { stage: "one" } }, "1단계 정리"),
      el("span", { class: "flow-stage", dataset: { stage: "two" } }, "2단계 전사")),
    el("ol", { class: "flow-nodes", "aria-label": `${drive.workspace_drive_id} 처리 흐름` }, ...steps));
}
