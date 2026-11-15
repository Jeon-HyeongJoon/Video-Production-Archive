// Archivist pane, shown as "Agent": a quiet panel on the right of every
// screen that scrolls its conversation on its own.
//
// Fold rule: a person's instructions, the latest answer and any decision
// waiting on a person stay visible. While the work runs, only its last
// progress line shows beside the answer. Recalls, progress, tool calls,
// earlier answers, settled cards and result IDs fold behind one "작업 내역"
// row, and references fold behind one "참조 N" row.

import { html, useEffect, useRef, useState, useComputed, batch } from "./lib.js";
import { label, finishedTurns, formatCount, clockTime, mutation, newID, startArchivistHost } from "./core.js";
import {
  ui, store, stale, refresh, allDrives, currentSession, allSessions, hostFor, activeTurn, availability, driveView, driveSelection,
  selectionRefs, jobOf, workDrive, workspaces, workTitle, openNewWork,
} from "./store.js";
import { route, go } from "./router.js";
import { StatusIcon, Glyph, GLYPH, Presence } from "./ui.js";
import { reducedMotion, settle } from "./motion.js";

export function openAgentCard(sessionID, cardID) {
  batch(() => {
    ui.sessionID.value = sessionID;
    ui.focusCard.value = cardID;
    ui.agentOpen.value = true;
  });
}

function minutesBetween(start, end) {
  const minutes = Math.max(0, Math.round((new Date(end) - new Date(start)) / 60000));
  return minutes < 1 ? "1분 미만" : `${minutes}분`;
}

const isSystem = (turn) => turn.submitted_by === "SYSTEM" || turn.turn_id.startsWith("SYSTEM-RECALL-");

function cardScope(card) {
  const scope = card.scope || {};
  if (card.command_type === "archive_run.start") return `${scope.workspace_drive_id || "—"} → ${scope.archive_storage_id || "—"} · ${formatCount(scope.folder_count)} 폴더`;
  if (card.command_type === "archive_run.transcription.start") {
    const current = allDrives.value.find((item) => item.archive_run_id === card.target_id);
    return `${card.target_id} · 대상 ${formatCount(current?.eligible_transcription_count)} 폴더`;
  }
  if (card.command_type === "archive_intake.selection.required") return "작업 PC · 가져올 드라이브 · 저장할 위치 · 처리할 폴더";
  return card.target_id || "—";
}

// ─── Thread pieces ──────────────────────────────────────────────────────────

const ToolRow = ({ call }) => html`<li class="tool tool-row" title=${call.tool_name} data-state=${call.state}>
  <${StatusIcon} kind=${call.state === "FAILED" ? "fault" : call.state === "RUNNING" ? "work" : call.state === "ACTION_CARD" ? "decide" : "done"} />
  <b>${label("tool", call.tool_name)}</b><span>${call.target_id || ""} · ${label("call", call.state)}</span></li>`;

const EntryRow = ({ entry }) => {
  const failure = entry.kind === "FAILURE";
  return html`<li class="tool turn-entry" data-kind=${entry.kind} data-signal=${failure ? "fault" : "work"}>
    <${StatusIcon} kind=${failure ? "fault" : "done"} /><b class="turn-entry-kind">${failure ? entry.code || "실패" : "진행"}</b><span>${entry.summary} · ${clockTime(entry.created_at)}</span></li>`;
};

function RefsFold({ refs, cls }) {
  if (!refs.length) return null;
  return html`<details class=${`refs-fold ${cls}`}>
    <summary>첨부한 작업 <span class="num">${refs.length}</span></summary>
    <div class="message-refs">${refs.map((value) => html`<span class="ref-chip" key=${value}>${value}</span>`)}</div>
  </details>`;
}

function referenceLabels(selection, work) {
  const values = selectionRefs(selection);
  if (work) values.unshift(`${work.kind === "JOB" ? "작업" : "영상 정리·보관"} · ${work.work_id} · r${work.revision}`);
  return values;
}

function UserBubble({ turn }) {
  return html`<div class="bubble message user" data-turn-id=${turn.turn_id} title=${`담당자 · ${label("turn", turn.state)} · ${clockTime(turn.created_at)}`}>
    <div class="text message-body">${turn.prompt || turn.request_id}</div>
    <${RefsFold} refs=${referenceLabels(turn.drive_selection, turn.work_reference)} cls="turn-refs" />
  </div>`;
}

const SystemRow = ({ turn }) => html`<div class="message system" data-turn-id=${turn.turn_id}>
  <span>자동으로 이어서 진행 · ${turn.prompt || turn.request_id}</span><span class="num">${clockTime(turn.created_at)}</span>
  ${finishedTurns.has(turn.state) ? null : html`<span class="token">${label("turn", turn.state)}</span>`}
</div>`;

function focusResult(resultID) {
  const current = allDrives.value.find((item) => item.archive_run_id === resultID);
  if (current) go(`#/runs/${encodeURIComponent(resultID)}`);
}

const Mentions = ({ ids }) => (ids?.length ? html`<div class="mentions message-refs">${ids.map((id) => html`<button type="button" class="mention ref-chip" key=${id} onClick=${() => focusResult(id)}>${id}</button>`)}</div>` : null);

async function decideCard(cardID, action, reason = "담당자 거절") {
  try {
    await mutation(`/api/v1/action-cards/${encodeURIComponent(cardID)}/${action}`, action === "reject" ? { reason } : {});
    await refresh();
  } catch (error) {
    ui.status.value = error.message;
  }
}

// Receipt is a decided card: one line that says what was decided.
const Receipt = ({ card }) => html`<div class="receipt action-card" data-card-id=${card.action_card_id} data-command=${card.command_type} data-state=${card.state}>
  <${StatusIcon} kind=${card.state === "REJECTED" ? "idle" : "done"} /><span>${label("cardState", card.state)} · ${label("card", card.command_type)} · ${cardScope(card)}</span>
</div>`;

// AskCard is a decision the Agent proposed; it stays out of every fold until
// a person decides it.
function AskCard({ card }) {
  const focused = useComputed(() => ui.focusCard.value === card.action_card_id).value;
  const box = useRef();
  useEffect(() => {
    if (!focused) return undefined;
    box.current?.scrollIntoView({ block: "nearest", behavior: reducedMotion.matches ? "auto" : "smooth" });
    const timer = setTimeout(() => { if (ui.focusCard.peek() === card.action_card_id) ui.focusCard.value = ""; }, 1600);
    return () => clearTimeout(timer);
  }, [focused]);
  const selection = card.command_type === "archive_intake.selection.required";
  return html`<div class="ask action-card" ref=${box} data-card-id=${card.action_card_id} data-command=${card.command_type} data-state=${card.state} data-signal="decide" data-focus=${focused ? "true" : null}>
    <div class="ask-body">
      <div class="ask-kicker"><${StatusIcon} kind="decide" />${label("cardState", card.state)}</div>
      <div class="ask-title">${label("card", card.command_type)}</div>
      <ul class="ask-effects"><li class="card-scope">${cardScope(card)}</li></ul>
      <div class="ask-actions">
        ${selection
          ? [html`<button type="button" key="close" class="btn btn-sm" data-action="card-reject" disabled=${stale.value} onClick=${() => decideCard(card.action_card_id, "reject", "선택 요청 닫음")}>닫기</button>`,
            html`<button type="button" key="select" class="btn btn-sm primary" onClick=${() => { openNewWork(); go("#/create"); }}>작업 대상 선택</button>`]
          : [html`<button type="button" key="reject" class="btn btn-sm" data-action="card-reject" disabled=${stale.value} onClick=${() => decideCard(card.action_card_id, "reject")}>하지 않기</button>`,
            html`<button type="button" key="run" class="btn btn-sm primary" data-action="card-execute" disabled=${stale.value} onClick=${() => decideCard(card.action_card_id, "execute")}>${card.approval_required ? "승인" : "진행"}</button>`]}
      </div>
    </div>
  </div>`;
}

// LiveLine is the one row a running Turn shows: its running tool call, else
// its latest progress entry, else its queued state.
function LiveLine({ turn }) {
  const entries = turn.entries || [];
  const calls = turn.tool_calls || [];
  const running = [...calls].reverse().find((call) => call.state === "RUNNING");
  const row = running ? html`<${ToolRow} call=${running} />`
    : entries.length ? html`<${EntryRow} entry=${entries[entries.length - 1]} />`
      : calls.length ? html`<${ToolRow} call=${calls[calls.length - 1]} />`
        : html`<li class="tool turn-entry" data-kind="STATE" data-signal="work"><${StatusIcon} kind="work" /><b class="turn-entry-kind">${isSystem(turn) ? "자동 진행" : "진행"}</b><span>${isSystem(turn) ? turn.prompt || turn.request_id : ""}</span></li>`;
  return html`<div class="turn-live" data-turn-id=${turn.turn_id}><ul class="tools">${row}</ul><span class="token">${label("turn", turn.state)}</span></div>`;
}

// TraceTurn is one Turn inside the 작업 내역 fold: its recall, its answer,
// progress entries and tool calls, and its settled cards.
function TraceTurn({ turn, session, source, summary, excludeFailure }) {
  const settled = finishedTurns.has(turn.state);
  const calls = turn.tool_calls || [];
  const entries = (turn.entries || []).filter((entry) => !(excludeFailure && entry.kind === "FAILURE"));
  const text = summary ? turn.final_response?.summary : "";
  return [
    source ? html`<${SystemRow} key="source" turn=${turn} />` : null,
    calls.length || entries.length || text ? html`<div key="answer" class="turn message archivist" data-turn-id=${turn.turn_id} data-settled=${String(settled)}>
      <div class="turn-head message-head">에이전트 · ${settled ? `${label("turn", turn.state)} · ${clockTime(turn.updated_at)}` : label("turn", turn.state)}</div>
      ${text ? html`<div class="text message-body">${text}</div>` : null}
      ${text ? html`<${Mentions} ids=${turn.final_response?.result_ids} />` : null}
      ${calls.length || entries.length ? html`<ul class="tools turn-trace">${entries.map((entry, index) => html`<${EntryRow} key=${`e${index}`} entry=${entry} />`)}${calls.map((call) => html`<${ToolRow} key=${call.tool_call_id || call.tool_name} call=${call} />`)}</ul>` : null}
    </div>` : null,
    ...(session.action_cards || []).filter((card) => card.archivist_turn_id === turn.turn_id && card.state !== "PENDING").map((card) => html`<${Receipt} key=${card.action_card_id} card=${card} />`),
  ];
}

function workUnits(turns) {
  const units = [];
  const byID = new Map();
  for (const turn of turns) {
    const workID = turn.work_context?.work_id;
    if (!workID) { units.push({ key: turn.turn_id, turns: [turn] }); continue; }
    let unit = byID.get(workID);
    if (!unit) { unit = { key: workID, workID, turns: [] }; byID.set(workID, unit); units.push(unit); }
    unit.turns.push(turn);
  }
  return units;
}

// unitItems draws one request: a standalone Turn, or every Turn of one
// ArchiveRun work. Only instructions, the latest answer, a running Turn's
// last line and pending cards stay outside the fold.
function unitItems(unit, session) {
  const turns = unit.turns;
  const origin = (unit.workID && turns.find((turn) => turn.turn_id === turn.work_context?.origin_turn_id)) || turns[0];
  const active = [...turns].reverse().find((turn) => !finishedTurns.has(turn.state));
  const final = unit.workID
    ? turns.find((turn) => turn.work_context?.trigger === "ARCHIVE_RUN_COMPLETE" && finishedTurns.has(turn.state))
    : (finishedTurns.has(origin.state) ? origin : null);
  const settled = Boolean(final) && !active;
  const shown = (final?.final_response?.summary ? final : null) || [...turns].reverse().find((turn) => turn.final_response?.summary);
  const failed = settled && !shown ? final : null;
  const failure = failed ? [...(failed.entries || [])].reverse().find((entry) => entry.kind === "FAILURE") : null;
  const items = [];
  for (const turn of turns) if (turn === origin || !isSystem(turn)) items.push({ key: `u:${turn.turn_id}`, kind: "user", turn });
  const traced = turns.filter((turn) => (turn !== origin && isSystem(turn)) || (turn.tool_calls || []).length || (turn.entries || []).length
    || (turn !== shown && turn.final_response?.summary) || (session.action_cards || []).some((card) => card.archivist_turn_id === turn.turn_id && card.state !== "PENDING")
    || (shown && (shown.final_response.result_ids || []).length));
  const calls = turns.reduce((count, turn) => count + (turn.tool_calls || []).length, 0);
  const recalls = turns.filter((turn) => turn !== origin && isSystem(turn)).length;
  const end = (final || active || turns[turns.length - 1]).updated_at;
  const foldText = traced.length ? `처리 내역 · 실행 ${calls}${recalls ? ` · 자동 이어서 ${recalls}` : ""}${settled ? ` · ${minutesBetween(origin.created_at, end)}` : ""}` : "";
  if (shown || active || failure || foldText) {
    items.push({ key: `a:${unit.key}`, kind: "answer", unit, origin, active, shown, failed, failure, settled, foldText, session });
  }
  for (const turn of turns) {
    for (const card of session.action_cards || []) if (card.archivist_turn_id === turn.turn_id && card.state === "PENDING") items.push({ key: `c:${card.action_card_id}`, kind: "card", card });
  }
  return items;
}

const head = (item) => (item.active ? label("turn", item.active.state)
  : item.shown ? `${label("turn", item.shown.state)} · ${clockTime(item.shown.updated_at)}` : `${label("turn", item.origin.state)} · ${clockTime(item.origin.updated_at)}`);

function Answer({ item }) {
  const { unit, origin, active, shown, failed, failure, settled, foldText, session } = item;
  return html`<div class=${`turn message archivist${unit.workID && settled ? " work-result" : ""}`} data-turn-id=${unit.workID ? null : origin.turn_id} data-work-id=${unit.workID || null} data-settled=${String(settled)}>
    <div class="turn-head message-head">에이전트 · ${head(item)}</div>
    ${shown ? html`<div class="text message-body">${shown.final_response.summary}</div>` : failure ? html`<ul class="tools"><${EntryRow} entry=${failure} /></ul>` : null}
    ${active ? html`<${LiveLine} turn=${active} />` : null}
    ${foldText ? html`<details class=${`turn-work${unit.workID ? " work-fold" : ""}`} data-work-id=${unit.workID || null} data-turn-id=${unit.workID ? null : origin.turn_id}>
      <summary class="tools-toggle">${foldText}</summary>
      <div class="work-fold-body">
        ${shown ? html`<${Mentions} ids=${shown.final_response.result_ids} />` : null}
        ${unit.turns.map((turn) => html`<${TraceTurn} key=${turn.turn_id} turn=${turn} session=${session} source=${turn !== origin && isSystem(turn)} summary=${turn !== shown} excludeFailure=${turn === failed} />`)}
      </div>
    </details>` : null}
  </div>`;
}

function ThreadItem({ item }) {
  if (item.kind === "user") return html`<${UserBubble} turn=${item.turn} />`;
  if (item.kind === "card") return html`<${AskCard} card=${item.card} />`;
  if (item.kind === "orphan") return item.card.state === "PENDING" ? html`<${AskCard} card=${item.card} />` : html`<${Receipt} card=${item.card} />`;
  return html`<${Answer} item=${item} />`;
}

// ─── Composer ───────────────────────────────────────────────────────────────

const STARTER = "진행 상황 알려줘";

function workReference(now, selection, picked) {
  if (picked) return picked;
  if (now.name === "job") {
    const job = jobOf(now.jobID);
    return job?.revision ? { kind: "JOB", work_id: job.job_id, revision: job.revision } : null;
  }
  if (now.name === "run") {
    const current = allDrives.value.find((item) => item.archive_run_id === now.workID);
    return current?.archive_run_revision ? { kind: "ARCHIVE_RUN", work_id: now.workID, revision: current.archive_run_revision } : null;
  }
  if (selection?.archive_run_id && selection.archive_run_revision) return { kind: "ARCHIVE_RUN", work_id: selection.archive_run_id, revision: selection.archive_run_revision };
  return null;
}

// referenceOf is a work the person adds to an instruction by hand.
function referenceOf(work) {
  if (work.kind === "JOB") {
    const job = jobOf(work.work_id);
    return job?.revision ? { kind: "JOB", work_id: job.job_id, revision: job.revision } : null;
  }
  const current = workDrive(work);
  return current?.archive_run_revision ? { kind: "ARCHIVE_RUN", work_id: work.work_id, revision: current.archive_run_revision } : null;
}

function Composer({ session, enabled, reason, canWake, waking, wake, selectSession }) {
  const now = route.value;
  const draft = ui.newWork.value;
  const [useScreen, setUseScreen] = useState(true);
  const [picked, setPicked] = useState(null);
  const [picker, setPicker] = useState(false);
  const [text, setText] = useState("");
  const [sending, setSending] = useState(false);
  const input = useRef();
  useEffect(() => { setUseScreen(true); setPicked(null); setPicker(false); }, [now.full]);
  const screenDrive = now.name === "drive" ? driveView(now.driveID)
    : now.name === "create" && draft?.kind === "ARCHIVE" && draft.driveID ? driveView(draft.driveID) : null;
  const selection = useScreen ? driveSelection(screenDrive, draft) : null;
  const work = useScreen || picked ? workReference(now, selection, picked) : null;
  const refs = referenceLabels(selection, work);
  const showRefs = refs.length > 0 || (useScreen && Boolean(screenDrive));
  const choices = workspaces.value.map((item) => [item, referenceOf(item)]).filter(([, ref]) => ref && ref.work_id !== work?.work_id);
  const send = async (prompt) => {
    const body = prompt.trim();
    if (!session || !enabled || sending || !body) return;
    setSending(true);
    try {
      await mutation(`/api/v1/archivist-sessions/${encodeURIComponent(session.session_id)}/turns`, {
        turn_id: newID("TURN"), request_id: newID("REQUEST"), prompt: body,
        drive_selection: selection || undefined, work_reference: work || undefined,
      });
      setText("");
      setPicked(null);
      await refresh();
    } catch (error) {
      ui.status.value = error.message;
    } finally {
      setSending(false);
    }
  };
  const submit = (event) => { event.preventDefault(); send(text); };
  const disabled = !enabled || sending || stale.value;
  return html`<form id="archivist-compose" class="composer" onSubmit=${submit}>
    ${enabled && !text ? html`<button type="button" class="starter" data-action="send" disabled=${disabled} onClick=${() => send(STARTER)}>“${STARTER}”</button>` : null}
    <div class="composer-box">
      ${picker ? html`<div class="picker" role="listbox" aria-label="첨부할 작업">
        ${choices.length ? choices.map(([item, ref]) => html`<button type="button" key=${item.work_id} onClick=${() => { setPicked(ref); setUseScreen(false); setPicker(false); input.current?.focus(); }}>
          <${StatusIcon} kind="idle" />${workTitle(item)}<small class="faint-id">${item.work_id}</small></button>`) : html`<p class="chooser-empty">고를 작업 없음</p>`}
      </div>` : null}
      <div class="chips">
        ${showRefs ? html`<span class="chip ctx-chip" title=${picked ? "첨부한 작업" : "지금 보고 있는 화면"}>
          <${Glyph} d=${GLYPH.screen} />
          <details id="pane-refs" class="refs-fold pane-refs">
            <summary>${refs.length ? html`작업 첨부 <span class="num">${refs.length}</span>` : "작업 대상 선택 미완료"}</summary>
            <span id="selection-chips" class="pane-chips">${refs.map((value) => html`<span class="selection-chip" key=${value}>${value}</span>`)}</span>
          </details>
          <button type="button" aria-label="첨부한 작업 빼기" onClick=${() => { setUseScreen(false); setPicked(null); }}><${Glyph} d=${GLYPH.close} /></button>
        </span>` : null}
        <button type="button" class="chip chip-add" aria-expanded=${picker ? "true" : "false"} onClick=${() => setPicker(!picker)}>+ 작업 첨부</button>
      </div>
      <textarea ref=${input} id="archivist-message" name="prompt" rows="1" aria-label="메시지" placeholder="할 일을 입력하세요"
        value=${text} disabled=${!enabled} onInput=${(event) => setText(event.currentTarget.value)}
        onKeyDown=${(event) => { if (event.key === "Enter" && !event.shiftKey && !event.isComposing) { event.preventDefault(); send(event.currentTarget.value); } }}></textarea>
      <div class="composer-foot">
        <select id="archivist-session-select" class="model" aria-label="에이전트 선택" value=${session?.session_id || ""} title=${session ? `${session.archivist_host_id} · ${session.cli_id}` : ""}
          onChange=${(event) => selectSession(event.currentTarget.value)}>
          ${allSessions.value.map((item) => html`<option key=${item.session_id} value=${item.session_id}>${item.cli_id} · ${item.name || item.session_id}</option>`)}
        </select>
        <button type="submit" class="send" data-action="send" aria-label="전송" title="전송" disabled=${disabled || !text.trim()}><${Glyph} d=${GLYPH.up} /></button>
      </div>
    </div>
    <span id="archivist-disabled-reason" class="pane-reason">${reason}</span>
    ${session && canWake ? html`<button type="button" class="btn btn-sm agent-wake" data-action="archivist-start" disabled=${waking || stale.value} onClick=${wake}>${waking ? "에이전트 켜는 중" : "에이전트 켜기"}</button>` : null}
  </form>`;
}

// ─── Pane ───────────────────────────────────────────────────────────────────

let stuck = true;

export function AgentPane() {
  const session = currentSession.value;
  const host = hostFor(session);
  const [wakingHostID, setWakingHostID] = useState("");
  const waking = Boolean(session && wakingHostID === session.archivist_host_id && host?.state !== "ONLINE");
  const available = availability(session);
  const [text, signal] = waking ? ["연결 중", "work"] : available;
  const active = activeTurn(session);
  const enabled = Boolean(session && host?.state === "ONLINE" && !active);
  const reason = !session ? "에이전트 없음" : waking ? "에이전트를 자동으로 켜고 있음" : host?.state !== "ONLINE" ? "에이전트가 꺼져 있음" : active ? `응답 대기 · ${label("turn", active.state)}` : "";
  const wake = async () => {
    if (!session || waking) return;
    setWakingHostID(session.archivist_host_id);
    try {
      await startArchivistHost(session.archivist_host_id);
      await refresh();
    } catch (error) {
      setWakingHostID("");
      ui.status.value = error.message;
    }
  };
  const selectSession = (sessionID) => {
    ui.sessionID.value = sessionID;
    const selected = allSessions.value.find((item) => item.session_id === sessionID);
    const selectedHost = hostFor(selected);
    if (selected && selectedHost?.state !== "ONLINE") {
      setWakingHostID(selected.archivist_host_id);
      startArchivistHost(selected.archivist_host_id).then(refresh).catch((error) => {
        setWakingHostID("");
        ui.status.value = error.message;
      });
    }
  };
  useEffect(() => {
    if (host?.state === "ONLINE" && wakingHostID === session?.archivist_host_id) setWakingHostID("");
  }, [host?.state, session?.archivist_host_id, wakingHostID]);
  const thread = useRef();
  const inner = useRef();
  const [unread, setUnread] = useState(0);
  const turns = session?.turns || [];
  const placed = new Set(turns.map((turn) => turn.turn_id));
  const items = session ? workUnits(turns).flatMap((unit) => unitItems(unit, session)) : [];
  if (session) for (const card of session.action_cards || []) if (!placed.has(card.archivist_turn_id)) items.push({ key: `o:${card.action_card_id}`, kind: "orphan", card });
  const count = items.length;
  const last = useRef(count);
  // While the reader is at the bottom, growth keeps the newest line in view.
  useEffect(() => {
    const element = thread.current;
    const pin = new ResizeObserver(() => { if (stuck && element) element.scrollTop = element.scrollHeight; });
    pin.observe(inner.current);
    document.fonts?.ready.then(() => { if (element) element.scrollTop = element.scrollHeight; });
    return () => pin.disconnect();
  }, []);
  useEffect(() => {
    if (count > last.current && !stuck) setUnread((value) => value + count - last.current);
    last.current = count;
  }, [count]);
  const onScroll = (event) => {
    const element = event.currentTarget;
    stuck = element.scrollHeight - element.scrollTop - element.clientHeight < 48;
    if (stuck && unread) setUnread(0);
  };
  const jump = async () => {
    stuck = true;
    setUnread(0);
    await settle();
    thread.current?.scrollTo({ top: thread.current.scrollHeight, behavior: reducedMotion.matches ? "auto" : "smooth" });
  };
  return html`<aside id="archivist-pane" class="agent-pane" data-open=${ui.agentOpen.value ? "true" : "false"} aria-label="에이전트">
    <div class="pane-head">
      <div class="pane-title"><b>에이전트</b><small class="pane-availability"><${StatusIcon} kind=${signal} /><span id="archivist-availability" data-signal=${signal}>${text}</span></small></div>
      <button type="button" class="pane-icon pane-close" aria-label="에이전트 칸 닫기" title="에이전트 칸 닫기" onClick=${() => { ui.agentOpen.value = false; }}><${Glyph} d=${GLYPH.close} /></button>
    </div>
    <section id="archivist-panel" class="pane-body" aria-label="에이전트 대화">
      <div class="thread-wrap">
        <div id="archivist-thread" class="thread" ref=${thread} onScroll=${onScroll} aria-live="polite">
          <div class="thread-inner" ref=${inner}>
            ${!store.ready.value ? html`<p class="thread-empty">불러오는 중…</p>`
              : items.length ? html`<${Presence} items=${items} keyOf=${(item) => item.key}>${(item) => html`<${ThreadItem} item=${item} />`}<//>`
                : html`<p class="thread-empty">대화 없음</p>`}
          </div>
        </div>
        <button type="button" class="jump" data-show=${unread > 0 ? "true" : "false"} onClick=${jump}>새 활동 ${unread}<${Glyph} d=${GLYPH.down} /></button>
      </div>
      <${Composer} session=${session} enabled=${enabled} reason=${reason} canWake=${host?.state !== "ONLINE"} waking=${waking} wake=${wake} selectSession=${selectSession} />
    </section>
  </aside>`;
}
