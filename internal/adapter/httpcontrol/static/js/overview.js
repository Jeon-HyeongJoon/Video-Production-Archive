// Overview of one drive, shared by 상태 and the drive's own screen: the phase
// and archived video progress over the stage summary of its ArchiveRun. The
// Worker shows only while it is off and the archive only while it is still
// undecided; otherwise both wait on the drive title's hover. Problems are
// listed once, in the 확인 필요 section below. Counts behind a fact wait on hover.

import { el, svgIcon, icons, statusIcon, label, stateSignal, formatCount, formatBytes, shortTime, clockTime, meter } from "./core.js";
import { model, drivePhase, driveName, driveProgress, driveConnection, folderDiscoveryText, PHASE_LABEL, PHASE_ICON } from "./store.js";
import { stageMatrix } from "./flow.js";

function fact(name, value, attributes = {}) {
  return el("div", { class: "fact", ...attributes }, el("span", { class: "fact-label" }, name), value);
}

// progressHover lists what the percentage stands for: archived videos and
// folders, the run's fixed scope and its CSV record.
function progressHover(drive) {
  const progress = driveProgress(drive);
  const folders = drive.folders || [];
  const transcribed = folders.filter((folder) => folder.transcription_job_id && folder.step_state === "COMPLETED").length;
  const bytes = folders.reduce((sum, folder) => sum + (folder.estimated_output_bytes || 0), 0);
  return [
    `보관 ${formatCount(progress.stored)} / ${formatCount(progress.total)} 영상`,
    `보관 폴더 ${progress.storedFolders} / ${progress.folders}${transcribed ? ` · STT ${transcribed} 폴더` : ""}`,
    `범위 r${drive.input_revision} 고정 · ${folders.length} 폴더 · ${formatCount(progress.total)} 영상 · ${formatBytes(bytes)}`,
    drive.archive_run_state === "COMPLETED" ? `CSV ${formatCount(drive.catalog_row_count)}행 · ${shortTime(drive.catalog_updated_at)} 기록` : "",
  ].filter(Boolean).join("\n");
}

export function driveFacts(drive, progressID = "") {
  const phase = drivePhase(drive);
  const worker = model.workers.find((item) => item.worker_id === drive.worker_id);
  const offline = stateSignal(worker?.state) === "fault";
  const progress = driveProgress(drive);
	const connection = driveConnection(drive);
	const discoveryIcon = drive.folder_discovery_state === "SCANNING" ? "work" : drive.folder_discovery_state === "FAILED" ? "fault" : "";
	const discoveryValue = el("span", { class: "fact-value", dataset: { signal: drive.folder_discovery_state === "FAILED" ? "fault" : undefined } },
		discoveryIcon ? statusIcon(discoveryIcon) : null, el("span", { class: "num" }, folderDiscoveryText(drive)));
  const facts = [
		fact("연결", el("span", { class: "fact-value", dataset: { signal: connection.signal } },
			statusIcon(connection.signal), el("span", {}, connection.text)),
		{ title: drive.connection_observed_at ? `관측 ${clockTime(drive.connection_observed_at, true)}` : "현재 연결을 확인할 수 없음", dataset: { fact: "connection" } }),
    fact("상태", el("span", { class: "fact-value" }, statusIcon(PHASE_ICON[phase]), el("span", {}, PHASE_LABEL[phase])), { dataset: { fact: "state", phase } }),
    drive.archive_run_id
      ? fact("보관 진행", el("span", { class: "fact-value" }, el("span", { class: "progress-pct num" }, `${progress.pct}%`)),
        { id: progressID || undefined, title: progressHover(drive), dataset: { fact: "progress" } })
      : fact("발견", discoveryValue, { dataset: { fact: "found" } }),
    offline ? fact("Worker", el("span", { class: "fact-value", dataset: { signal: "fault" } },
      statusIcon("fault"), el("span", {}, `${drive.worker_id} ${label("worker", worker?.state)}`)),
    { title: `마지막 응답 ${clockTime(worker?.last_heartbeat, true)}`, dataset: { fact: "worker" } }) : null,
    drive.archive_storage_id ? null
      : fact("아카이브", el("span", { class: "fact-value", dataset: { muted: "true" } }, el("span", {}, "미정")), { dataset: { fact: "archive" } }),
  ].filter(Boolean);
	if (drive.archive_run_id) facts[2].append(meter(progress.pct, phase === "done"));
  return el("div", { class: "facts-grid" }, ...facts);
}

// driveOverview is the card 상태 shows for each drive a Worker processes now.
export function driveOverview(drive) {
  const href = `#/drives/${encodeURIComponent(drive.workspace_drive_id)}`;
  return el("section", { class: "overview", "aria-label": driveName(drive), dataset: { driveId: drive.workspace_drive_id } },
    el("div", { class: "overview-head" },
      el("b", {}, driveName(drive)), el("small", {}, drive.worker_id),
      el("a", { class: "btn btn-sm", href }, "드라이브 열기", svgIcon(icons.next))),
    driveFacts(drive), stageMatrix(drive));
}
