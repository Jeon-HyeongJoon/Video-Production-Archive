// 드라이브: one card per source drive across every Worker, grouped by its
// ArchiveRun — none yet, unfinished, completed. The drive is the unit of work,
// so a card carries the drive's phase, progress and problem counts but no
// folders; its Worker, archive and counts wait on hover.

import { el, statusIcon, problemCounts, label, stateSignal, formatCount, meter, emptyState } from "../core.js";
import { model, driveGroup, drivePhase, driveName, driveProgress, driveProblemCounts, PHASE_LABEL, PHASE_ICON } from "../store.js";

const GROUPS = [["before", "실행 전"], ["during", "처리 중"], ["after", "완료"]];

function card(drive) {
  const worker = model.workers.find((item) => item.worker_id === drive.worker_id);
  const phase = drivePhase(drive);
  const counts = driveProblemCounts(drive);
  const discovered = Array.isArray(drive.discovered_folders);
  const before = phase === "intake";
  const progress = driveProgress(drive);
  const scope = before
    ? `${formatCount(discovered ? drive.discovered_folder_count : drive.folder_count)} 폴더 · ${formatCount(discovered ? drive.discovered_file_count : drive.file_count)} 영상`
    : `보관 ${formatCount(progress.stored)} / ${formatCount(progress.total)} 영상`;
  const offline = stateSignal(worker?.state) === "fault";
  return el("a", { class: "drive-card", href: `#/drives/${encodeURIComponent(drive.workspace_drive_id)}`, title: `${drive.worker_id} · → ${drive.archive_storage_id || "아카이브 미정"} · ${scope}`,
    dataset: { driveId: drive.workspace_drive_id, signal: counts.fault ? "fault" : counts.decide ? "decide" : undefined } },
  el("span", { class: "drive-card-head" }, statusIcon(PHASE_ICON[phase]), el("b", {}, driveName(drive)), problemCounts(counts.fault, counts.decide)),
  el("span", { class: "drive-card-line" },
    el("span", { dataset: { signal: phase === "stt" ? "decide" : undefined } }, PHASE_LABEL[phase]),
    el("b", { class: "num" }, before ? "—" : `${progress.pct}%`)),
  meter(before ? 0 : progress.pct, phase === "done"),
  // Only an off Worker stays on the card; it stops the drive.
  offline ? el("span", { class: "drive-card-worker", dataset: { signal: "fault" } }, statusIcon("fault"), `${drive.worker_id} ${label("worker", worker.state)}`) : null);
}

export function mount(root) {
  const heading = el("h1", {}, "드라이브");
  const groups = el("div", { class: "drive-groups", id: "drive-groups" });
  root.append(el("div", { class: "page-head" }, el("div", { class: "page-title" }, heading)), groups);
  return {
    update() {
      const drives = [...model.drives].sort((left, right) => left.workspace_drive_id.localeCompare(right.workspace_drive_id));
      heading.title = `원본 드라이브 ${drives.length} · Worker ${model.workers.length}`;
      groups.replaceChildren(...GROUPS.map(([id, title]) => {
        const list = drives.filter((drive) => driveGroup(drive) === id);
        return el("section", { class: "section", "aria-label": title, dataset: { group: id } },
          el("h2", { class: "section-title" }, `${title} `, el("span", { class: "num" }, String(list.length))),
          list.length ? el("div", { class: "drive-cards" }, ...list.map(card)) : emptyState("없음"));
      }));
    },
  };
}
