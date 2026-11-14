// 설정: Archivist 목록과 추가, Archivist Host 상태, 화면 테마. The screen
// calls them 에이전트 and 에이전트 실행 PC. Host, backend and workspace registration
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
  const [removing, setRemoving] = useState("");
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
  const remove = async (session) => {
    if (removing || !globalThis.confirm(`${session.name || session.session_id} 에이전트를 제거할까요?`)) return;
    setRemoving(session.session_id);
    setStatus("");
    try {
      await mutation(`/api/v1/archivist-sessions/${encodeURIComponent(session.session_id)}/remove`, {});
      setStatus("제거됨");
      await refresh();
    } catch (error) {
      setStatus(error.message);
    } finally {
      setRemoving("");
    }
  };
  return [
    html`<div key="head" class="page-head"><div class="page-title"><h1>설정</h1><small class="sub">고급 연결 설정은 프로그램을 다시 시작한 뒤 적용</small></div></div>`,
    html`<section key="agents" class="section" id="archivist-list" aria-label="에이전트"><h2 class="section-title">에이전트${" "}<span class="num">${allSessions.value.length}</span></h2>
      <${AgentTable} sessions=${allSessions.value} rowClass="archivist-row" onRemove=${remove} removing=${removing} /></section>`,
    html`<section key="add" class="panel" aria-label="에이전트 추가">
      <div class="panel-toolbar"><b>에이전트 추가</b></div>
      <form id="archivist-add" class="settings-form" onSubmit=${add}>
        <label class="field"><span>이름</span><input name="name" required autocomplete="off" value=${form.name} placeholder="예: 편집 도우미" onInput=${(event) => set({ name: event.currentTarget.value })} /></label>
        <label class="field"><span>실행 PC</span><select id="archivist-host-select" name="archivist_host_id" required value=${hostID} onChange=${(event) => set({ host: event.currentTarget.value, cli: "" })}>
          ${hosts.map((host) => html`<option key=${host.archivist_host_id} value=${host.archivist_host_id} disabled=${host.state === "DISABLED"}>${host.archivist_host_id} · ${label("host", host.state)}</option>`)}
        </select></label>
        <label class="field"><span>실행 도구</span><select id="archivist-cli-select" name="cli_id" required value=${cli} disabled=${!clis.length} onChange=${(event) => set({ cli: event.currentTarget.value })}>
          ${clis.length ? clis.map((value) => html`<option key=${value} value=${value}>${value}</option>`) : html`<option value="">사용 가능한 실행 도구 없음</option>`}
        </select></label>
        <div class="form-actions"><button type="submit" class="btn primary" id="archivist-add-submit" data-action="archivist-add" disabled=${!clis.length || stale.value}>추가</button>
          <span id="archivist-add-status" class="muted" role="status">${status}</span></div>
      </form>
    </section>`,
    html`<section key="hosts" class="section" aria-label="에이전트 실행 PC"><h2 class="section-title">에이전트 실행 PC${" "}<span class="num">${hosts.length}</span></h2><${HostTable} hosts=${hosts} /></section>`,
    html`<section key="theme" class="panel" aria-label="화면">
      <div class="panel-toolbar"><b>화면</b></div>
      <div class="settings-row"><span>테마</span><${ThemeSwitch} /></div>
    </section>`,
  ];
}
