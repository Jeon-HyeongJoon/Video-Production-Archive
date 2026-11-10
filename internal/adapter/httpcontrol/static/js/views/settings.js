// 설정: Archivist 목록과 추가, Archivist Host 상태, 화면 테마. The screen
// calls them Agent and Agent Host. Host, backend and workspace registration
// stay in the local configuration files; the form takes only name, Host,
// detected CLI.

import { html, useState } from "../lib.js";
import { label, mutation, newID } from "../core.js";
import { store, allSessions, refresh, stale } from "../store.js";
import { AgentTable, HostTable } from "./connections.js";
import { ThemeSwitch } from "../theme.js";

export function SettingsScreen() {
  const hosts = store.hosts.value;
  const [form, setForm] = useState({ name: "", host: "", cli: "" });
  const [status, setStatus] = useState("");
  const hostID = hosts.some((host) => host.archivist_host_id === form.host) ? form.host : hosts.find((host) => host.state !== "DISABLED")?.archivist_host_id || "";
  const clis = hosts.find((host) => host.archivist_host_id === hostID)?.detected_cli_ids || [];
  const cli = clis.includes(form.cli) ? form.cli : clis[0] || "";
  const set = (patch) => setForm({ ...form, ...patch });
  const add = async (event) => {
    event.preventDefault();
    const name = form.name.trim();
    if (!name || !cli) return;
    try {
      await mutation("/api/v1/archivist-sessions", { session_id: newID("ARCHIVIST"), name, archivist_host_id: hostID, cli_id: cli });
      setForm({ ...form, name: "" });
      setStatus("추가됨");
      await refresh();
    } catch (error) {
      setStatus(error.message);
    }
  };
  return [
    html`<div key="head" class="page-head"><div class="page-title"><h1>설정</h1><small class="sub">등록·정책 변경은 설정 파일 수정 뒤 서비스 재시작</small></div></div>`,
    html`<section key="agents" class="section" id="archivist-list" aria-label="Agent"><h2 class="section-title">Agent${" "}<span class="num">${allSessions.value.length}</span></h2>
      <${AgentTable} sessions=${allSessions.value} rowClass="archivist-row" /></section>`,
    html`<section key="add" class="panel" aria-label="Agent 추가">
      <div class="panel-toolbar"><b>Agent 추가</b></div>
      <form id="archivist-add" class="settings-form" onSubmit=${add}>
        <label class="field"><span>이름</span><input name="name" required autocomplete="off" value=${form.name} placeholder="예: 편집 도우미" onInput=${(event) => set({ name: event.currentTarget.value })} /></label>
        <label class="field"><span>Agent Host</span><select id="archivist-host-select" name="archivist_host_id" required value=${hostID} onChange=${(event) => set({ host: event.currentTarget.value, cli: "" })}>
          ${hosts.map((host) => html`<option key=${host.archivist_host_id} value=${host.archivist_host_id} disabled=${host.state === "DISABLED"}>${host.archivist_host_id} · ${label("host", host.state)}</option>`)}
        </select></label>
        <label class="field"><span>CLI</span><select id="archivist-cli-select" name="cli_id" required value=${cli} disabled=${!clis.length} onChange=${(event) => set({ cli: event.currentTarget.value })}>
          ${clis.length ? clis.map((value) => html`<option key=${value} value=${value}>${value}</option>`) : html`<option value="">감지된 CLI 없음</option>`}
        </select></label>
        <div class="form-actions"><button type="submit" class="btn primary" id="archivist-add-submit" data-action="archivist-add" disabled=${!clis.length || stale.value}>추가</button>
          <span id="archivist-add-status" class="muted" role="status">${status}</span></div>
      </form>
    </section>`,
    html`<section key="hosts" class="section" aria-label="Agent Host"><h2 class="section-title">Agent Host${" "}<span class="num">${hosts.length}</span></h2><${HostTable} hosts=${hosts} /></section>`,
    html`<section key="theme" class="panel" aria-label="화면">
      <div class="panel-toolbar"><b>화면</b></div>
      <div class="settings-row"><span>테마</span><${ThemeSwitch} /></div>
    </section>`,
  ];
}
