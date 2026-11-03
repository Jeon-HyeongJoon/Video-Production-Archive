// 설정: Archivist 목록과 추가, Archivist Host 상태. The screen calls them
// Agent and Agent Host. Host, backend and workspace registration stay in the
// local configuration files; the form takes only name, Host, detected CLI and
// default control mode.

import { el, statusIcon, workerIcon, label, stateSignal, clockTime, mutation, newID, emptyState } from "../core.js";
import { model, refresh, hostFor, activeTurn } from "../store.js";

function availability(session) {
  const host = hostFor(session);
  if (host?.state !== "ONLINE") return ["Host 꺼짐", "fault"];
  if (activeTurn(session)) return ["처리 중", "work"];
  return ["사용 가능", "online"];
}

export function mount(root) {
  const list = el("div", { id: "archivist-list" });
  const hostSelect = el("select", { id: "archivist-host-select", name: "archivist_host_id", required: true });
  const cliSelect = el("select", { id: "archivist-cli-select", name: "cli_id", required: true });
  const modeSelect = el("select", { id: "archivist-mode-select", name: "control_mode" },
    ...["ASSISTED", "ARCHIVIST", "MANUAL"].map((mode) => el("option", { value: mode }, `${mode} · ${label("mode", mode)}`)));
  const submit = el("button", { type: "submit", class: "btn primary", id: "archivist-add-submit" }, "추가");
  const status = el("span", { id: "archivist-add-status", role: "status" });
  const form = el("form", { id: "archivist-add", class: "settings-form" },
    el("label", { class: "field" }, el("span", {}, "이름"), el("input", { name: "name", required: true, autocomplete: "off" })),
    el("label", { class: "field" }, el("span", {}, "Agent Host"), hostSelect),
    el("label", { class: "field" }, el("span", {}, "CLI"), cliSelect),
    el("label", { class: "field" }, el("span", {}, "기본 제어 모드"), modeSelect),
    el("div", { class: "form-actions" }, submit, status));

  function renderOptions() {
    const selected = hostSelect.value;
    hostSelect.replaceChildren(...model.hosts.map((host) => el("option", { value: host.archivist_host_id, disabled: host.state === "DISABLED" }, `${host.archivist_host_id} · ${label("host", host.state)}`)));
    if (model.hosts.some((host) => host.archivist_host_id === selected)) hostSelect.value = selected;
    const host = model.hosts.find((item) => item.archivist_host_id === hostSelect.value);
    const clis = host?.detected_cli_ids || [];
    const chosen = cliSelect.value;
    cliSelect.replaceChildren(...(clis.length ? clis.map((value) => el("option", { value }, value)) : [el("option", { value: "" }, "감지된 CLI 없음")]));
    if (clis.includes(chosen)) cliSelect.value = chosen;
    cliSelect.disabled = !clis.length;
    submit.disabled = !clis.length;
  }

  hostSelect.addEventListener("change", renderOptions);
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const data = Object.fromEntries(new FormData(form));
    try {
      await mutation("/api/v1/archivist-sessions", { session_id: newID("ARCHIVIST"), name: data.name, archivist_host_id: data.archivist_host_id, cli_id: data.cli_id, control_mode: data.control_mode });
      form.reset();
      status.textContent = "추가됨";
      await refresh();
    } catch (error) {
      status.textContent = error.message;
    }
  });

  const hostRows = el("div", {});
  root.append(
    el("div", { class: "page-head" }, el("div", { class: "page-title" }, el("h1", { title: "등록·정책 변경 · 설정 파일 수정 뒤 서비스 재시작" }, "설정"))),
    el("section", { class: "panel table-panel", "aria-label": "Agent" },
      el("div", { class: "panel-toolbar" }, el("b", { class: "panel-title" }, "Agent")),
      el("div", { class: "grid-row grid-head archivist-row" }, ...["이름", "Agent Host", "CLI", "제어 모드", "상태"].map((text) => el("span", {}, text))),
      list),
    el("section", { class: "panel form-panel", "aria-label": "Agent 추가" },
      el("div", { class: "panel-toolbar" }, el("b", { class: "panel-title" }, "Agent 추가")), form),
    el("section", { class: "panel table-panel", "aria-label": "Agent Host", id: "host-table" },
      el("div", { class: "panel-toolbar" }, el("b", { class: "panel-title" }, "Agent Host")),
      el("div", { class: "grid-row grid-head host-row" }, ...["Agent Host", "상태", "감지된 CLI", "heartbeat"].map((text) => el("span", {}, text))),
      hostRows));

  return {
    update() {
      list.replaceChildren(...(model.sessions.length ? model.sessions.map((session) => {
        const [text, signal] = availability(session);
        return el("div", { class: "grid-row archivist-row", dataset: { sessionId: session.session_id } },
          el("b", {}, session.name || session.session_id), el("span", { class: "num" }, session.archivist_host_id), el("span", { class: "num" }, session.cli_id),
          el("span", { class: "num" }, session.control_mode || "ASSISTED"),
          el("span", { class: "state", dataset: { signal } }, statusIcon(signal), text));
      }) : [emptyState("Agent 없음")]));
      hostRows.replaceChildren(...model.hosts.map((host) => {
        const signal = stateSignal(host.state);
        return el("div", { class: "grid-row host-row", dataset: { hostId: host.archivist_host_id } },
          el("b", { class: "num" }, host.archivist_host_id),
          el("span", { class: "state", dataset: { signal } }, workerIcon(host.state), label("host", host.state)),
          el("span", { class: "num" }, (host.detected_cli_ids || []).join(" · ") || "감지된 CLI 없음"),
          el("span", { class: "num muted" }, clockTime(host.last_heartbeat, true)));
      }));
      renderOptions();
    },
  };
}
