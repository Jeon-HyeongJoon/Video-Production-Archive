// 작업 관리: one row per work a person requested. An archive work names its
// drive, its first folder and when it was asked for; the work ID stays faint
// because nobody tells works apart by it. A row opens into its folders, or a
// Template Job's steps. Running work stays on top, queued work in its own
// list, work that needs a person below, and finished work folded at the end.

import { html, useEffect, useComputed, batch } from "../lib.js";
import { label, formatCount, formatBytes, shortTime, dayTime, mutation, newID } from "../core.js";
import {
  ui, store, stale, refresh, allWork, allDrives, workLane, workTitle, workScopeText, workText, workKind, workIssues, workDrive, workDetail,
  jobDetail, jobOf, driveView, folderKey, folderState, folderProblem, folderProgress, driveName, isRunning, archiveCandidates,
  folderDiscoveryReady, folderDiscoveryText, startBlocker, openNewWork, prepareDrive, syncDraft, setDraft, resetRunIntent,
} from "../store.js";
import { route, go, shared } from "../router.js";
import { StatusIcon, Glyph, GLYPH, Chevron, Swap, Meter, Fold, IssueTags, Empty, Skeleton, useLinger, cssName } from "../ui.js";

const LANES = [
  ["work", "active-jobs", "진행 중", "진행 중인 작업 없음"],
  ["queue", "queued-jobs", "작업 큐", "대기 중인 작업 없음"],
  ["attention", "attention-jobs", "확인 필요", "확인할 작업 없음"],
  ["done", "completed-jobs", "완료", "완료된 작업 없음"],
];
const IN_LANE = { work: ["work"], queue: ["queue"], attention: ["fault", "decide"], done: ["done"] };
const TEMPLATES = [["occasion-recording", "행사 기록"], ["vj-compilation", "VJ 모음"], ["final-edit", "최종 편집"], ["large-file-intake", "대용량 입고"]];
const CHILDREN_SHOWN = 60;

const time = (value) => new Date(value || 0).getTime();
const laneItems = (lane) => {
  const items = allWork.value.filter((work) => IN_LANE[lane].includes(workLane(work)));
  return lane === "done" ? items.sort((left, right) => time(right.last_event_at) - time(left.last_event_at)) : items.sort((left, right) => time(right.created_at) - time(left.created_at));
};

export function JobsScreen() {
  const draft = ui.newWork.value;
  return [
    html`<div key="head" class="page-head">
      <div class="page-title"><h1 id="jobs-title">작업 관리</h1></div>
      <button type="button" class="btn" id="new-job-open" aria-expanded=${draft ? "true" : "false"} onClick=${() => (draft ? (ui.newWork.value = null) : openNewWork())}>
        <${Glyph} d=${GLYPH.plus} />새 작업 생성</button>
    </div>`,
    html`<${Fold} key="new" open=${!!draft}>${draft ? html`<${NewWork} draft=${draft} />` : null}<//>`,
    html`<div key="cols" class="job-cols with-chev" aria-hidden="true"><span>작업</span><span>상태</span><span>진행</span><span>요청</span><span></span></div>`,
    ...LANES.map(([lane, id, title, empty]) => html`<${Lane} key=${lane} lane=${lane} id=${id} title=${title} empty=${empty} />`),
  ];
}

function Lane({ lane, id, title, empty }) {
  const items = laneItems(lane);
  const foldable = lane === "attention" || lane === "done";
  const open = foldable ? ui.lanes.value[lane] : true;
  const toggle = () => { ui.lanes.value = { ...ui.lanes.peek(), [lane]: !open }; };
  return html`<section class="lane" id=${id} data-open=${open ? "true" : "false"} aria-label=${title}>
    <div class="lane-head vt" style=${`--vtn:lane-${lane}`}>
      ${foldable
        ? html`<button type="button" class="lane-toggle" aria-expanded=${open ? "true" : "false"} onClick=${toggle}><${Chevron} /><span class="lane-title">${title}</span>${" "}<span class="num">${items.length}</span></button>`
        : html`<h2 class="section-title"><span class="lane-title">${title}</span>${" "}<span class="num">${items.length}</span></h2>`}
    </div>
    <${Fold} open=${open}>
      <div class="lane-rows">
        ${items.length ? items.map((work) => html`<${WorkBlock} key=${work.work_id} work=${work} meter=${lane === "work"} />`) : html`<${Empty} text=${empty} />`}
      </div>
    <//>
  </section>`;
}

function WorkBlock({ work, meter }) {
  const open = useComputed(() => ui.expanded.value.has(work.work_id)).value;
  const shown = useLinger(open);
  const run = work.kind === "ARCHIVE_RUN";
  const href = run ? `#/runs/${encodeURIComponent(work.work_id)}` : `#/jobs/${encodeURIComponent(work.work_id)}`;
  const isShared = useComputed(() => shared.value?.key === href && shared.value.src === "row" && route.value.full !== href).value;
  const job = run ? null : jobOf(work.work_id);
  const toggle = () => {
    const next = new Set(ui.expanded.peek());
    if (!next.delete(work.work_id)) next.add(work.work_id);
    ui.expanded.value = next;
  };
  const pct = Math.round((work.progress || 0) * 100);
  return html`<div class="work-block vt" style=${`--vtn:work-${cssName(work.work_id)}`} data-work-id=${work.work_id}>
    <button type="button" class="job-row work-row" aria-expanded=${open ? "true" : "false"} data-work-id=${work.work_id} data-kind=${work.kind} data-lane=${workLane(work)}
      title=${[work.work_id, `Worker ${work.worker_id || "배정 없음"}`].join("\n")} onClick=${toggle}>
      <span class="job-name">
        <b class=${isShared ? "shared" : null}>${workTitle(work)}</b>
        <small class="work-scope">${run ? workScopeText(work) : `Template 작업 · ${label("template", job?.template_name)}`} <i class="faint-id">${work.work_id}</i></small>
      </span>
      <span class="state"><${StatusIcon} kind=${workKind(work)} /><${Swap} cls="job-state" text=${workText(work)} />${run ? html`<${IssueTags} ...${workIssues(work)} cls="row-issue" />` : null}</span>
      <span class="job-progress">${meter ? html`<${Meter} value=${work.progress || 0} label=${`진행 ${pct}%`} />` : null}</span>
      <span class="job-time" title=${shortTime(work.created_at)}>${dayTime(work.created_at)}</span>
      <${Chevron} />
    </button>
    <${Fold} open=${open}>${shown ? (run ? html`<${WorkFolders} work=${work} href=${href} />` : html`<${TemplateSteps} work=${work} href=${href} />`) : null}<//>
  </div>`;
}

// An opened archive work shows its first folders; a folder handed over to a
// newer work of the same drive says where it went.
function WorkFolders({ work, href }) {
  const current = workDrive(work);
  const answer = current ? null : workDetail(work.work_id);
  const names = current ? current.scope : answer?.value?.folder_ids || null;
  const children = new Map((answer?.value?.children || []).filter((child) => child.folder).map((child) => [child.folder, child.job]));
  const latest = driveView(work.source_volume_id);
  const newer = latest && latest.archive_run_id && latest.archive_run_id !== work.work_id ? new Set(latest.scope) : null;
  const total = work.folder_count || names?.length || 0;
  let running = 0;
  let queued = 0;
  let done = 0;
  for (const folder of current?.folders || []) {
    const state = folderState(folder);
    if (state.signal === "work") running += 1;
    else if (state.word === "대기열") queued += 1;
    else if (state.signal === "done") done += 1;
  }
  const moved = newer && names ? names.filter((name) => newer.has(name)).length : 0;
  const counts = current ? `폴더 ${formatCount(total)} · 진행 ${running} · 대기 ${formatCount(queued)} · 끝남 ${formatCount(done)}` : `폴더 ${formatCount(total)} · 영상 ${formatCount(work.video_count)}`;
  return html`<div class="work-children">
    <div class="children-head">
      <span>${counts}${moved ? ` · 새 작업으로 넘김 ${moved}` : ""}</span>
      <a class="text-btn" href=${href} data-shared="row">작업 화면 열기<${Glyph} d=${GLYPH.next} /></a>
    </div>
    <div class="child-list">
      ${names ? names.slice(0, CHILDREN_SHOWN).map((name) => html`<${ChildFolder} key=${name} work=${work} current=${current} name=${name}
        job=${children.get(name)} movedTo=${newer?.has(name) ? latest.archive_run_id : ""} />`)
        : html`<${Skeleton} n=${Math.min(total || 1, 6)} cls="skel-child" />`}
    </div>
    ${names && names.length > CHILDREN_SHOWN ? html`<span class="more">외 ${formatCount(names.length - CHILDREN_SHOWN)}개 · 작업 화면에서 모두 보기</span>` : null}
  </div>`;
}

function ChildFolder({ work, current, name, job, movedTo }) {
  if (movedTo) {
    return html`<a class="child-row" href=${`#/runs/${encodeURIComponent(movedTo)}`} data-folder-id=${name}>
      <b>${name}</b><span class="state"><${StatusIcon} kind="idle" />새 작업으로 넘김</span><span><i class="faint-id">${movedTo}</i></span><span></span>
    </a>`;
  }
  const folder = current ? store.folders.get(folderKey(current.workspace_drive_id, name))?.value : null;
  if (folder) {
    const problem = folderProblem(folder);
    const state = folderState(folder);
    const signal = problem?.signal || (folder.archive_state ? "done" : state.signal);
    const word = problem?.word || (folder.archive_state ? (folder.archive_state === "COMMITTED" ? "확정됨" : "보관됨") : state.word);
    const href = folder.job_id ? `#/jobs/${encodeURIComponent(folder.job_id)}` : `#/drives/${encodeURIComponent(current.workspace_drive_id)}?folder=${encodeURIComponent(name)}`;
    return html`<a class="child-row" href=${href} data-shared="child" data-folder-id=${name} data-job-id=${folder.job_id || null}>
      <b>${name}</b>
      <span class="state"><${StatusIcon} kind=${signal} /><${Swap} text=${word} /></span>
      <span>${state.signal === "work" && !problem ? html`<${Meter} value=${folderProgress(folder)} label="진행" />` : null}</span>
      <span class="job-time">${formatCount(folder.video_count)} 영상</span>
    </a>`;
  }
  const lane = job ? workLane({ state: { COMPLETED: "COMPLETED", FAILED: "FAILED", RUNNING: "ACTIVE" }[job.step_state] || "QUEUED" }) : "";
  return html`<a class="child-row" href=${job ? `#/jobs/${encodeURIComponent(job.job_id)}` : `#/runs/${encodeURIComponent(work.work_id)}`} data-folder-id=${name} data-job-id=${job?.job_id || null}>
    <b>${name}</b>
    <span class="state">${job ? html`<${StatusIcon} kind=${{ done: "done", fault: "fault", work: "work" }[lane] || "idle"} />${label("step", job.step_state)}` : html`<span class="muted">고정 범위</span>`}</span>
    <span></span>
    <span class="job-time">${job ? dayTime(job.last_event_at) : ""}</span>
  </a>`;
}

function TemplateSteps({ work, href }) {
  const answer = jobDetail(work.work_id);
  const steps = answer.value?.steps || null;
  return html`<div class="work-children">
    <div class="children-head"><span>단계 ${steps ? steps.length : "—"}</span><a class="text-btn" href=${href} data-shared="row">작업 화면 열기<${Glyph} d=${GLYPH.next} /></a></div>
    ${steps ? html`<ol class="child-steps">
      ${steps.map((step) => {
        const kind = step.state === "COMPLETED" ? "done" : step.state === "FAILED" ? "fault" : step.state === "WAITING" && ["INPUT", "APPROVAL", "DEVICE"].includes(step.wait_reason) ? "decide" : ["RUNNING", "RESERVED", "VERIFYING"].includes(step.state) ? "work" : "idle";
        return html`<li class="stage-row" key=${step.id}><${StatusIcon} kind=${kind} /><b>${label("stage", step.id)}</b><span class="muted">${step.state === "WAITING" && step.wait_reason ? `${label("wait", step.wait_reason)} 대기` : label("step", step.state)} · ${Math.round((step.progress || 0) * 100)}%</span></li>`;
      })}
    </ol>` : html`<${Skeleton} n=${2} cls="skel-child" />`}
  </div>`;
}

// ─── New work ───────────────────────────────────────────────────────────────

function NewWork({ draft }) {
  const head = html`<div class="new-work-head">
    <b>새 작업</b>
    <div class="seg plain-seg job-kind" id="new-work-kind" role="group" aria-label="작업 종류">
      ${[["ARCHIVE", "아카이브"], ["TEMPLATE", "Template 작업"]].map(([kind, text]) => html`<button type="button" key=${kind} data-kind=${kind} aria-pressed=${draft.kind === kind ? "true" : "false"}
        onClick=${() => setDraft({ kind, error: "" })}>${text}</button>`)}
    </div>
  </div>`;
  return html`<section class="new-work" id="new-job" aria-label="새 작업 생성">${head}${draft.kind === "ARCHIVE" ? html`<${ArchiveScope} draft=${draft} />` : html`<${TemplateForm} draft=${draft} />`}</section>`;
}

function TemplateForm({ draft }) {
  const ready = draft.title.trim() && !draft.pending;
  const submit = async (event) => {
    event.preventDefault();
    if (!ready) return;
    const jobID = newID("JOB");
    setDraft({ pending: true, error: "" });
    try {
      await mutation("/api/v1/jobs", {
        job_id: jobID, command_id: newID("COMMAND"), idempotency_key: `${jobID}:create`,
        reason: "작업 관리에서 생성", template_name: draft.template, target_title: draft.title.trim(), expected_revision: 0,
      });
      ui.newWork.value = null;
      await refresh();
      go(`#/jobs/${encodeURIComponent(jobID)}`);
    } catch (error) {
      setDraft({ pending: false, error: error.message });
    }
  };
  return html`<form id="new-job-form" onSubmit=${submit}>
    <div class="form-grid">
      <label class="field"><span>작업 이름</span><input id="new-job-title" name="target_title" required autocomplete="off" value=${draft.title}
        onInput=${(event) => setDraft({ title: event.currentTarget.value })} placeholder="예: 11월 행사 하이라이트" /></label>
      <label class="field"><span>작업 유형</span><select id="new-job-template" name="template_name" value=${draft.template} onChange=${(event) => setDraft({ template: event.currentTarget.value })}>
        ${TEMPLATES.map(([value, text]) => html`<option key=${value} value=${value}>${text}</option>`)}</select></label>
    </div>
    <div class="new-work-foot">
      ${draft.error ? html`<span class="summary" id="new-job-status" role="status">${draft.error}</span>` : null}
      <button type="button" class="btn" disabled=${draft.pending} onClick=${() => { ui.newWork.value = null; }}>취소</button>
      <button type="submit" class="btn primary" id="new-job-submit" data-action="create" disabled=${!ready || stale.value}>${draft.pending ? "요청 보냄…" : "작업 요청"}</button>
    </div>
  </form>`;
}

function ArchiveScope({ draft }) {
  const available = allDrives.value.filter((item) => item.connection_state === "CONNECTED" && !isRunning(item));
  const current = draft.driveID ? driveView(draft.driveID) : null;
  useEffect(() => { if (current) syncDraft(current, ui.newWork.peek()); }, [current, draft]);
  const chooseDrive = (id) => { setDraft({ error: "" }); prepareDrive(id); };
  const drives = html`<div class="chooser-col" id="prep-drives" aria-label="원본 드라이브">
    <div class="chooser-head"><span>원본 드라이브</span><span class="chooser-value">${current ? driveName(current) : "선택 없음"}</span></div>
    ${available.length ? available.map((item) => html`<button type="button" class="option" key=${item.workspace_drive_id} data-drive-id=${item.workspace_drive_id}
      aria-pressed=${draft.driveID === item.workspace_drive_id ? "true" : "false"} onClick=${() => chooseDrive(item.workspace_drive_id)}>
      <b>${driveName(item)}</b><small>${item.worker_id} · ${folderDiscoveryText(item)}</small></button>`) : html`<p class="chooser-empty">연결된 드라이브 없음</p>`}
  </div>`;
  if (!current) {
    return html`<div class="chooser archive-create" id="archive-create">${drives}<div class="chooser-col chooser-hint"><p class="chooser-empty">드라이브를 고르세요</p></div></div>
      ${draft.error ? html`<div class="new-work-foot"><span class="summary" id="new-job-status" role="status">${draft.error}</span></div>` : null}`;
  }
  const ready = folderDiscoveryReady(current);
  const folders = current.inventoryFolders.length ? current.inventoryFolders : current.folders;
  const picked = draft.folders;
  const selected = folders.filter((folder) => picked.has(folder.folder));
  const candidates = archiveCandidates(current);
  const videos = selected.reduce((sum, folder) => sum + (folder.video_count || 0), 0);
  const bytes = selected.reduce((sum, folder) => sum + (folder.estimated_output_bytes || 0), 0);
  const blocker = startBlocker(current, draft, selected);
  const heading = ready ? `탐색 r${current.folder_discovery_revision} · ${folders.length}` : folderDiscoveryText(current);
  const pick = (names) => batch(() => { setDraft({ folders: new Set(names) }); resetRunIntent(); });
  const toggle = (name) => {
    const next = new Set(picked);
    if (!next.delete(name)) next.add(name);
    pick(next);
  };
  const start = async () => {
    if (draft.starting || blocker) return;
    setDraft({ starting: true, error: "" });
    try {
      const result = await mutation("/api/v1/archive-intake/runs", {
        archive_run_id: draft.archiveRunID, idempotency_key: draft.archiveRunKey, session_id: draft.intakeSessionID,
        source_volume_id: current.source_volume_id, expected_candidate_revision: current.candidate_revision || current.input_revision,
        archive_storage_id: draft.archiveID, input_revision: current.folder_discovery_revision || current.input_revision,
        folder_ids: [...picked].sort(),
      });
      ui.newWork.value = null;
      await refresh();
      go(`#/runs/${encodeURIComponent(result.archive_run_id)}`);
    } catch (error) {
      setDraft({ starting: false, error: error.message });
    }
  };
  return html`<div class="chooser archive-create" id="archive-create">
      ${drives}
      <div class="chooser-stack" id="archive-scope">
        <div class="chooser-col" id="prep-archives" aria-label="아카이브 저장소">
          <div class="chooser-head"><span>아카이브 저장소</span><span class="chooser-value">${draft.archiveID || "선택 없음"}</span></div>
          ${candidates.length ? candidates.map((storage) => html`<button type="button" class="option" key=${storage.storage_id} data-storage-id=${storage.storage_id} data-access-kind=${storage.access_kind || ""}
            aria-pressed=${storage.storage_id === draft.archiveID ? "true" : "false"} disabled=${!storage.accessible}
            onClick=${() => batch(() => { setDraft({ archiveID: draft.archiveID === storage.storage_id ? "" : storage.storage_id }); resetRunIntent(); })}>
            <b>${storage.label || storage.storage_id}</b>
            <small>${label("storageRole", storage.role)} · ${label("storageAccess", storage.access_kind)} · ${storage.accessible ? `여유 ${formatBytes(storage.free_bytes)} / ${formatBytes(storage.capacity_bytes)}` : "연결 필요"}</small>
          </button>`) : html`<p class="chooser-empty">후보 없음</p>`}
        </div>
        <div class="chooser-col folder-picks" id="prep-folders" aria-label="폴더">
          <div class="chooser-head"><span>폴더</span><span class="chooser-value num">${heading}</span></div>
          <label class="pick all folder-pick folder-all"><input type="checkbox" id="select-all" aria-label="전체 선택" disabled=${!ready}
            checked=${ready && folders.length > 0 && selected.length === folders.length} indeterminate=${selected.length > 0 && selected.length < folders.length}
            onChange=${(event) => pick(event.currentTarget.checked ? folders.map((folder) => folder.folder) : [])} /><span>전체</span><small class="num">${selected.length} / ${folders.length}</small></label>
          ${folders.length ? folders.map((folder) => html`<label class="pick folder-pick" key=${folder.folder} data-folder-id=${folder.folder}>
            <input type="checkbox" aria-label=${`${folder.folder} 선택`} disabled=${!ready} checked=${picked.has(folder.folder)} onChange=${() => toggle(folder.folder)} />
            <span class="folder-pick-name">${folderProblem(folder)?.kind === "fault" ? html`<${StatusIcon} kind="fault" /> ` : null}${folder.folder}</span>
            <small class="num">${formatCount(folder.video_count)} 영상 · ${formatBytes(folder.estimated_output_bytes)}</small>
          </label>`) : html`<p class="chooser-empty">${current.folder_discovery_state === "SCANNING" ? "검색 중" : current.folder_discovery_state === "FAILED" ? "검색 실패" : "폴더 없음"}</p>`}
        </div>
      </div>
    </div>
    <div class="new-work-foot start-bar">
      <span class="summary num" id="selection-summary">${selected.length} 폴더 · ${formatCount(videos)} 영상 · ${formatBytes(bytes)}</span>
      <span class="start-reason" id="start-reason">${draft.error || blocker}</span>
      <button type="button" class="btn" disabled=${draft.starting} onClick=${() => { ui.newWork.value = null; }}>취소</button>
      <button type="button" class="btn primary" id="start-archive-run" data-action="create" disabled=${Boolean(blocker) || draft.starting || stale.value} onClick=${start}>${draft.starting ? "요청 보냄…" : "작업 요청"}</button>
    </div>`;
}
