// Archivist pane, shown as "Agent": it holds the right side of every screen
// and scrolls its conversation on its own.
//
// Fold rule (after Orca's native chat turn fold): a person's instructions, the
// latest answer and any decision waiting on a person stay visible. While the
// work runs, only its last progress line shows beside the answer. Recalls,
// progress, tool calls, earlier answers, settled cards and result IDs fold
// behind one "작업 내역" row, and references fold behind one "참조 N" row.

import { byId, el, svgIcon, icons, statusIcon, setStatusIcon, label, stateSignal, finishedTurns, formatCount, clockTime, mutation, newID, token } from "./core.js";
import { model, ui, notify, refresh, currentSession, hostFor, activeTurn, driveSelection, selectionRefs, findDrive } from "./store.js";
import { go } from "./router.js";

function minutesBetween(start, end) {
  const minutes = Math.max(0, Math.round((new Date(end) - new Date(start)) / 60000));
  return minutes < 1 ? "1분 미만" : `${minutes}분`;
}

function toolRow(call) {
  return el("div", { class: "tool-row", title: call.tool_name, dataset: { state: call.state } },
    el("span", {}, label("tool", call.tool_name)), el("code", {}, call.target_id || ""), el("span", { class: "tool-state" }, label("call", call.state)));
}

function cardScope(card) {
  const scope = card.scope || {};
  if (card.command_type === "archive_run.start") return `${scope.workspace_drive_id || "—"} → ${scope.archive_storage_id || "—"} · ${formatCount(scope.folder_count)} 폴더`;
  if (card.command_type === "archive_run.transcription.start") {
    const drive = model.drives.find((item) => item.archive_run_id === card.target_id);
    return `${card.target_id} · 대상 ${formatCount(drive?.eligible_transcription_count)} 폴더`;
  }
  if (card.command_type === "archive_intake.selection.required") return "Worker · 원본 · 아카이브 · 폴더";
  return card.target_id || "—";
}

async function decideCard(cardID, action) {
  try {
    await mutation(`/api/v1/action-cards/${encodeURIComponent(cardID)}/${action}`, action === "reject" ? { reason: "담당자 거절" } : {});
    await refresh();
  } catch (error) {
    byId("archivist-disabled-reason").textContent = error.message;
  }
}

function actionCard(card) {
  const item = el("div", { class: "action-card", dataset: { cardId: card.action_card_id, command: card.command_type, state: card.state } },
    el("div", { class: "card-line" }, el("b", {}, label("card", card.command_type)), token(label("cardState", card.state), card.state === "PENDING" ? "decide" : stateSignal(card.state))),
    el("span", { class: "card-scope" }, cardScope(card)));
  if (card.state !== "PENDING") return item;
  const actions = el("div", { class: "card-actions" });
  if (card.command_type === "archive_intake.selection.required") {
    actions.append(el("button", { type: "button", class: "btn primary", onclick: () => go("#/drives") }, "드라이브 선택"));
  } else {
    actions.append(
      el("button", { type: "button", class: "btn", dataset: { action: "card-reject" }, onclick: () => decideCard(card.action_card_id, "reject") }, "거절"),
      el("button", { type: "button", class: "btn primary", dataset: { action: "card-execute" }, onclick: () => decideCard(card.action_card_id, "execute") }, card.approval_required ? "승인" : "실행"));
  }
  item.append(actions);
  return item;
}

function focusResult(resultID) {
  const drive = model.drives.find((item) => item.archive_run_id === resultID);
  if (drive) go(`#/drives/${encodeURIComponent(drive.workspace_drive_id)}`);
}

function isSystem(turn) {
  return turn.submitted_by === "SYSTEM" || turn.turn_id.startsWith("SYSTEM-RECALL-");
}

// refsFold keeps a selection's references behind one "참조 N" row.
function refsFold(refs, key, open, className) {
  const fold = el("details", { class: `refs-fold ${className}` },
    el("summary", {}, "참조 ", el("span", { class: "num" }, String(refs.length))),
    el("div", { class: "message-refs" }, ...refs.map((value) => el("span", { class: "ref-chip" }, value))));
  fold.open = open.has(key);
  fold.addEventListener("toggle", () => { fold.open ? open.add(key) : open.delete(key); });
  return fold;
}

function turnSource(turn, open = new Set()) {
  if (isSystem(turn)) {
    return el("div", { class: "message system", dataset: { turnId: turn.turn_id } },
      el("span", {}, `재호출 · ${turn.prompt || turn.request_id}`), el("span", { class: "num" }, clockTime(turn.created_at)),
      finishedTurns.has(turn.state) ? null : token(label("turn", turn.state), stateSignal(turn.state)));
  }
  const refs = selectionRefs(turn.drive_selection);
  return el("article", { class: "message user", dataset: { turnId: turn.turn_id } },
    el("div", { class: "message-head" }, el("span", {}, "담당자"), el("span", { class: "num" }, `${label("turn", turn.state)} · ${clockTime(turn.created_at)}`)),
    el("p", { class: "message-body" }, turn.prompt || turn.request_id),
    refs.length ? refsFold(refs, `refs:${turn.turn_id}`, open, "turn-refs") : null);
}

function entryRow(entry) {
  const failure = entry.kind === "FAILURE";
  return el("div", { class: "turn-entry", dataset: { kind: entry.kind, signal: failure ? "fault" : "work" } },
    el("span", { class: "turn-entry-kind" }, failure ? (entry.code || "실패") : "진행"),
    el("span", {}, entry.summary), el("span", { class: "num" }, clockTime(entry.created_at)));
}

function appendResults(answer, results) {
  if (results.length) answer.append(el("div", { class: "message-refs" }, ...results.map((resultID) =>
    el("button", { type: "button", class: "ref-chip", onclick: () => focusResult(resultID) }, resultID))));
}

// liveLine is the one row a running Turn shows: its running tool call, else
// its latest progress entry, else its queued state.
function liveLine(turn) {
  const entries = turn.entries || [];
  const calls = turn.tool_calls || [];
  const running = [...calls].reverse().find((call) => call.state === "RUNNING");
  const row = running ? toolRow(running)
    : entries.length ? entryRow(entries[entries.length - 1])
      : calls.length ? toolRow(calls[calls.length - 1])
        : el("div", { class: "turn-entry", dataset: { kind: "STATE", signal: "work" } },
          el("span", { class: "turn-entry-kind" }, isSystem(turn) ? "재호출" : "진행"),
          el("span", {}, isSystem(turn) ? turn.prompt || turn.request_id : ""), token(label("turn", turn.state), stateSignal(turn.state)));
  return el("div", { class: "turn-live", dataset: { turnId: turn.turn_id } }, row);
}

// traceTurn is one Turn inside the 작업 내역 fold: its recall, its answer,
// progress entries and tool calls, and its settled cards.
function traceTurn(turn, session, options = {}) {
  const settled = finishedTurns.has(turn.state);
  const rows = [];
  if (options.source) rows.push(turnSource(turn));
  const calls = turn.tool_calls || [];
  const entries = (turn.entries || []).filter((entry) => !(options.excludeFailure && entry.kind === "FAILURE"));
  const summary = options.summary ? turn.final_response?.summary : "";
  if (calls.length || entries.length || summary) {
    const answer = el("article", { class: "message archivist", dataset: { turnId: turn.turn_id, settled: String(settled) } },
      el("div", { class: "message-head" }, el("span", {}, "Agent"), el("span", { class: "num" }, settled ? `${label("turn", turn.state)} · ${clockTime(turn.updated_at)}` : label("turn", turn.state))));
    if (summary) {
      answer.append(el("p", { class: "message-body" }, summary));
      appendResults(answer, turn.final_response?.result_ids || []);
    }
    if (calls.length || entries.length) answer.append(el("div", { class: "turn-trace" }, ...entries.map(entryRow), ...calls.map(toolRow)));
    rows.push(answer);
  }
  for (const card of session.action_cards || []) {
    if (card.archivist_turn_id === turn.turn_id && card.state !== "PENDING") rows.push(actionCard(card));
  }
  return rows;
}

function workUnits(turns) {
  const units = [];
  const byID = new Map();
  for (const turn of turns) {
    const workID = turn.work_context?.work_id;
    if (!workID) { units.push({ turns: [turn] }); continue; }
    let unit = byID.get(workID);
    if (!unit) { unit = { workID, turns: [] }; byID.set(workID, unit); units.push(unit); }
    unit.turns.push(turn);
  }
  return units;
}

// renderUnit draws one request: a standalone Turn, or every Turn of one
// ArchiveRun work. Only instructions, the latest answer, a running Turn's last
// line and pending cards stay outside the fold.
function renderUnit(unit, session, open) {
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

  const rows = [];
  for (const turn of turns) if (turn === origin || !isSystem(turn)) rows.push(turnSource(turn, open));

  const body = el("div", { class: "work-fold-body" });
  if (shown) appendResults(body, shown.final_response.result_ids || []);
  for (const turn of turns) {
    body.append(...traceTurn(turn, session, { source: turn !== origin && isSystem(turn), summary: turn !== shown, excludeFailure: turn === failed }));
  }
  const calls = turns.reduce((count, turn) => count + (turn.tool_calls || []).length, 0);
  const recalls = turns.filter((turn) => turn !== origin && isSystem(turn)).length;
  const key = unit.workID || origin.turn_id;
  let fold = null;
  if (body.childElementCount) {
    const end = (final || active || turns[turns.length - 1]).updated_at;
    fold = el("details", { class: `turn-work${unit.workID ? " work-fold" : ""}`, dataset: { workId: unit.workID, turnId: unit.workID ? undefined : origin.turn_id } },
      el("summary", {}, `작업 내역 · 도구 ${calls}${recalls ? ` · 재호출 ${recalls}` : ""}${settled ? ` · ${minutesBetween(origin.created_at, end)}` : ""}`), body);
    fold.open = open.has(key);
    fold.addEventListener("toggle", () => { fold.open ? open.add(key) : open.delete(key); });
  }

  if (shown || active || failure || fold) {
    const head = active ? label("turn", active.state) : shown ? `${label("turn", shown.state)} · ${clockTime(shown.updated_at)}` : `${label("turn", origin.state)} · ${clockTime(origin.updated_at)}`;
    const answer = el("article", {
      class: `message archivist${unit.workID && settled ? " work-result" : ""}`,
      dataset: { turnId: unit.workID ? undefined : origin.turn_id, workId: unit.workID, settled: String(settled) },
    }, el("div", { class: "message-head" }, el("span", {}, "Agent"), el("span", { class: "num" }, head)));
    if (shown) answer.append(el("p", { class: "message-body" }, shown.final_response.summary));
    else if (failure) answer.append(entryRow(failure));
    if (active) answer.append(liveLine(active));
    if (fold) answer.append(fold);
    rows.push(answer);
  }
  for (const turn of turns) {
    for (const card of session.action_cards || []) {
      if (card.archivist_turn_id === turn.turn_id && card.state === "PENDING") rows.push(actionCard(card));
    }
  }
  return rows;
}

export function mountAgent(container) {
  const openWork = new Set();
  let currentSelection = null;
  let focused = "";
  const select = el("select", { id: "archivist-session-select", "aria-label": "Agent 선택" });
  const availability = el("span", { id: "archivist-availability" });
  const availabilityIcon = statusIcon("idle");
  const thread = el("div", { id: "archivist-thread", class: "pane-thread", "aria-live": "polite" });
  const chips = el("div", { id: "selection-chips", class: "pane-chips" });
  const refCount = el("span", { class: "num" });
  const refs = el("details", { id: "pane-refs", class: "refs-fold pane-refs" }, el("summary", {}, "참조 ", refCount), chips);
  const message = el("textarea", { id: "archivist-message", name: "prompt", rows: "2", required: true, placeholder: "Agent에게 지시", "aria-label": "메시지" });
  const send = el("button", { type: "submit", class: "pane-send", "aria-label": "전송", title: "전송", dataset: { action: "send" } }, svgIcon(icons.up));
  const reason = el("span", { id: "archivist-disabled-reason", class: "pane-reason" });
  const form = el("form", { id: "archivist-compose", class: "pane-compose" },
    refs, el("div", { class: "pane-input" }, message, send), reason);
  // Below the narrowest two-column width the pane slides in from the right
  // edge over the screen; the handle on that edge opens and closes it.
  const handle = byId("agent-handle");
  const toggle = () => { ui.agentOpen = !ui.agentOpen; notify(); if (ui.agentOpen) message.focus(); };
  handle.addEventListener("click", toggle);
  container.append(
    el("div", { class: "pane-head" }, el("b", {}, "Agent"), el("span", { class: "pane-availability" }, availabilityIcon, availability),
      el("span", { class: "pane-session" }, select),
      el("a", { class: "pane-icon", href: "#/settings", "aria-label": "Agent 추가 · 설정", title: "Agent 추가 · 설정" }, svgIcon(icons.plus)),
      el("button", { type: "button", class: "pane-icon pane-close", "aria-label": "Agent 칸 닫기", title: "Agent 칸 닫기", onclick: toggle }, svgIcon(icons.close))),
    el("section", { id: "archivist-panel", class: "pane-body", "aria-label": "Agent 대화" }, thread, form));

  // The bundled font arrives after the first render and grows every message;
  // start the conversation at its latest turn once it has.
  document.fonts?.ready.then(() => { thread.scrollTop = thread.scrollHeight; });
  select.addEventListener("change", () => { ui.sessionID = select.value; notify(); });
  message.addEventListener("keydown", (event) => {
    if (event.key !== "Enter" || event.shiftKey || event.isComposing) return;
    event.preventDefault();
    if (!message.disabled && !send.disabled) form.requestSubmit();
  });
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const session = currentSession();
    if (!session || send.disabled) return;
    try {
      await mutation(`/api/v1/archivist-sessions/${encodeURIComponent(session.session_id)}/turns`, {
        turn_id: newID("TURN"), request_id: newID("REQUEST"), prompt: message.value, drive_selection: currentSelection || undefined,
      });
      form.reset();
      await refresh();
    } catch (error) {
      reason.textContent = error.message;
    }
  });

  return {
    update(route) {
      document.body.dataset.agent = ui.agentOpen ? "open" : "closed";
      handle.setAttribute("aria-expanded", String(ui.agentOpen));
      const session = currentSession();
      const before = select.value;
      select.replaceChildren(...model.sessions.map((item) => el("option", { value: item.session_id }, `${item.cli_id} · ${item.name || item.session_id}`)));
      if (model.sessions.some((item) => item.session_id === before)) select.value = before;
      if (session) select.value = session.session_id;
      const host = hostFor(session);
      const active = activeTurn(session);
      const online = host?.state === "ONLINE";
      const available = Boolean(session && online && !active);
      const [text, signal] = !session ? ["Agent 없음", "idle"] : !online ? ["Host 꺼짐", "fault"] : active ? ["처리 중", "work"] : ["사용 가능", "online"];
      availability.textContent = text;
      availability.dataset.signal = signal;
      setStatusIcon(availabilityIcon, signal);
      select.title = session ? `${session.archivist_host_id} · ${session.cli_id} · ${session.control_mode || "ASSISTED"}` : "";

      const stuck = !thread.childElementCount || thread.scrollHeight - thread.scrollTop - thread.clientHeight < 24;
      const turns = session?.turns || [];
      const placed = new Set(turns.map((turn) => turn.turn_id));
      const rows = session ? workUnits(turns).flatMap((unit) => renderUnit(unit, session, openWork)) : [];
      for (const card of session?.action_cards || []) if (!placed.has(card.archivist_turn_id)) rows.push(actionCard(card));
      thread.replaceChildren(...(rows.length ? rows : [el("p", { class: "empty" }, "대화 없음")]));
      const target = ui.focusCard ? thread.querySelector(`[data-card-id="${CSS.escape(ui.focusCard)}"]`) : null;
      if (target) {
        target.dataset.focus = "true";
        if (focused !== ui.focusCard) { target.scrollIntoView({ block: "nearest" }); focused = ui.focusCard; }
      } else if (stuck) thread.scrollTop = thread.scrollHeight;

      const screenDrive = route.name === "drive" ? findDrive(route.driveID) : route.name === "jobs" ? findDrive(ui.prepDriveID) : null;
      currentSelection = driveSelection(screenDrive);
      const selected = selectionRefs(currentSelection);
      chips.replaceChildren(...selected.map((value) => el("span", { class: "selection-chip" }, value)));
      refCount.textContent = selected.length ? String(selected.length) : "선택 미완료";
      refs.hidden = !selected.length && !screenDrive;
      message.disabled = !available;
      send.disabled = !available;
      message.placeholder = `${session?.name || "Agent"}에게 지시`;
      reason.textContent = !session ? "Agent 없음" : !online ? "Host 꺼짐" : active ? `응답 대기 · ${label("turn", active.state)}` : "";
    },
  };
}
