// 상태: every failure and human wait across drives, Workers, drive-free Jobs,
// Agent Hosts and Agent decisions as its own card that leads to the place that
// resolves it, then what is processed now — one overview card with its flow
// per drive in work, and the Jobs that belong to no drive.

import { el, svgIcon, icons, statusIcon, label, stateSignal, formatCount, clockTime, meter, emptyState } from "../core.js";
import {
  model, ui, notify, sortedProblems, problemPath, problemSubject, newProblems, findDrive, driveGroup, drivePhase,
  driveFreeJobs, jobLane, PHASE_LABEL,
} from "../store.js";
import { go } from "../router.js";
import { driveOverview } from "../overview.js";

const WHERE = { worker: "Worker", job: "Worker", host: "설정", card: "Agent" };

function folderFacts(item) {
  const drive = findDrive(item.driveID);
  const folder = (drive?.folders || []).find((entry) => entry.folder === item.folder);
  if (!folder) return [item.workerID];
  const stages = folder.stages || [];
  const failed = stages.find((stage) => stage.state === "FAILED");
  const waiting = stages.find((stage) => stage.state === "WAITING" && stage.wait_reason);
  const facts = [item.workerID, `영상 ${formatCount(folder.video_count)}`];
  if (item.kind === "fault" && failed) facts.push(`${label("stage", failed.stage_id)} 단계`);
  if (item.kind === "hold") facts.push((folder.hold_reasons || []).map((reason) => label("hold", reason)).join(" · "));
  if (item.kind === "device") facts.push(`${label("stage", waiting?.stage_id)} 단계`, item.target);
  if (item.kind === "wait" && waiting) facts.push(`${label("stage", waiting.stage_id)} 단계`);
  return facts;
}

function problemFacts(item) {
  if (item.folder) return folderFacts(item);
  if (item.kind === "stt") {
    const drive = findDrive(item.driveID);
    return [item.workerID, `대상 ${formatCount(drive?.eligible_transcription_count)} 폴더`];
  }
  if (item.kind === "worker") {
    const worker = model.workers.find((entry) => entry.worker_id === item.workerID);
    const drives = model.drives.filter((drive) => drive.worker_id === item.workerID).map((drive) => `${drive.workspace_drive_id} ${PHASE_LABEL[drivePhase(drive)]}`);
    return [`마지막 응답 ${clockTime(worker?.last_heartbeat)}`, drives.length ? `드라이브 ${drives.join(", ")}` : ""];
  }
  if (item.kind === "job") {
    const job = model.jobs.find((entry) => entry.job_id === item.jobID);
    return [job?.template_name, item.jobID, job?.worker_id || "Worker 배정 없음"];
  }
  return [];
}

function open(item) {
  newProblems.delete(item.key);
  if (item.kind === "card") {
    ui.focusCard = item.cardID;
    ui.sessionID = item.sessionID;
    notify();
    return;
  }
  go(problemPath(item));
}

// A card shows what and where; the facts and the destination wait on hover.
function problemCard(item) {
  const subject = problemSubject(item);
  const where = item.driveID ? "드라이브" : WHERE[item.kind];
  const facts = problemFacts(item).filter(Boolean).join(" · ");
  return el("button", {
    type: "button", class: "problem-card", "aria-label": `${item.title} ${subject}, ${where}로 이동`,
    title: [facts, `${where}에서 해결`].filter(Boolean).join("\n"),
    dataset: { kind: item.kind, signal: item.signal, driveId: item.driveID, folderId: item.folder, workerId: item.kind === "worker" ? item.workerID : undefined, jobId: item.jobID, cardId: item.cardID },
    onclick: () => open(item),
  },
  el("span", { class: "problem-card-head" }, statusIcon(item.signal), el("b", { class: "problem-title" }, item.title),
    newProblems.has(item.key) ? el("span", { class: "new-tag" }, "새") : null, svgIcon(icons.next)),
  el("b", { class: "problem-subject" }, subject));
}

function jobRow(job) {
  const lane = jobLane(job);
  const pct = Math.round((job.progress || 0) * 100);
  const state = lane === "fault" ? "실패" : lane === "decide" ? (job.wait_reason ? `${label("wait", job.wait_reason)} 대기` : "사람 대기") : lane === "queue" ? "대기열" : `${pct}%`;
  return el("a", { class: "data-row doing-item", role: "row", href: `#/workers?job=${encodeURIComponent(job.job_id)}`, dataset: { jobId: job.job_id },
    title: [job.template_name, job.job_id].filter(Boolean).join(" · ") },
  el("span", { class: "cell-main" }, statusIcon(lane === "queue" ? "idle" : lane), el("b", {}, job.target_title || job.job_id)),
  el("span", {}, job.worker_id || "Worker 배정 없음"),
  el("span", { class: "cell-meter" }, meter(pct, false), el("span", { class: "num", dataset: { signal: lane === "fault" || lane === "decide" ? lane : undefined } }, state)));
}

export function mount(root) {
  const heading = el("h1", { id: "status-title" }, "상태");
  const problems = el("div", { class: "card-grid", id: "problem-cards" });
  const problemCount = el("span", { class: "num" });
  const doing = el("div", { class: "doing-list", id: "doing-rows" });
  root.append(
    el("div", { class: "page-head" }, el("div", { class: "page-title" }, heading)),
    el("section", { class: "section", "aria-label": "확인 필요" }, el("h2", { class: "section-title" }, "확인 필요 ", problemCount), problems),
    el("section", { class: "section", "aria-label": "지금 하는 일" }, el("h2", { class: "section-title" }, "지금 하는 일"), doing));
  return {
    update() {
      const groups = { before: 0, during: 0, after: 0 };
      for (const drive of model.drives) groups[driveGroup(drive)] += 1;
      const online = model.workers.filter((worker) => stateSignal(worker.state) !== "fault").length;
      const freeJobs = driveFreeJobs();
      heading.title = `드라이브 ${model.drives.length} · 처리 중 ${groups.during} · 실행 전 ${groups.before} · 완료 ${groups.after} · Worker ${model.workers.length} · 연결 ${online} · 드라이브 밖 작업 ${freeJobs.length}`;

      const cards = sortedProblems().map(problemCard);
      problemCount.textContent = String(cards.length);
      problems.replaceChildren(...(cards.length ? cards : [emptyState("없음")]));

      const running = model.drives.filter((drive) => driveGroup(drive) === "during")
        .sort((left, right) => left.workspace_drive_id.localeCompare(right.workspace_drive_id));
      const items = running.map(driveOverview);
      if (freeJobs.length) {
        items.push(el("div", { class: "data-table job-table doing-jobs", role: "table", "aria-label": "드라이브 밖 작업" },
          el("div", { class: "data-row data-head", role: "row" }, ...["드라이브 밖 작업", "Worker", "진행"].map((head) => el("span", { role: "columnheader" }, head))),
          ...freeJobs.map(jobRow)));
      }
      doing.replaceChildren(...(items.length ? items : [emptyState("처리 중인 일 없음")]));
    },
  };
}
