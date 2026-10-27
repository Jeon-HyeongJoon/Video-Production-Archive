const byId = (id) => document.getElementById(id);
let csrfToken = "";

async function api(path) {
  const response = await fetch(path, { headers: { Accept: "application/json" } });
  const payload = await response.json();
  if (!response.ok) throw new Error(payload.error?.code || `HTTP_${response.status}`);
  return payload.data ?? payload;
}

async function mutation(path, payload) {
  if (!csrfToken) {
    const session = await api("/api/v1/session");
    csrfToken = session.csrf_token;
  }
  const response = await fetch(path, {
    method: "POST",
    credentials: "same-origin",
    headers: { Accept: "application/json", "Content-Type": "application/json", "X-CSRF-Token": csrfToken },
    body: JSON.stringify(payload),
  });
  const result = await response.json();
  if (!response.ok) throw new Error(result.error?.code || `HTTP_${response.status}`);
  return result.data;
}

function identifier(prefix) {
  return `${prefix}-${crypto.randomUUID()}`;
}

function empty(container) {
  container.replaceChildren(byId("empty").content.cloneNode(true));
}

function text(tag, value, className = "") {
  const element = document.createElement(tag);
  element.textContent = value;
  if (className) element.className = className;
  return element;
}

async function loadHealth() {
  const health = byId("health");
  const label = health.querySelector(".status-label");
  try {
    const payload = await api("/healthz");
    label.textContent = `LIVE · ${payload.version}`;
    health.classList.add("ok");
  } catch (error) {
    label.textContent = `OFFLINE · ${error.message}`;
    health.classList.remove("ok");
  }
}

async function loadJobs() {
  const container = byId("jobs");
  try {
    const jobs = await api("/api/v1/jobs");
    if (!jobs.length) return empty(container);
    container.replaceChildren(...jobs.map((job) => {
      const card = document.createElement("article");
      card.className = `card ${job.needs_human_intervention ? "attention" : ""}`;

      const top = document.createElement("div");
      top.className = "card-top";
      top.append(
        text("p", `${job.template_name} / ${job.control_mode || "—"}`, "meta"),
        text("span", job.step_state, "token"),
      );

      const progress = Math.max(0, Math.min(100, Math.round(job.progress * 100)));
      const detail = document.createElement("div");
      detail.className = "job-detail";
      detail.append(
        text("span", "STEP", "detail-label"),
        text("span", job.current_step_id, "detail-value"),
        text("span", "PROGRESS", "detail-label"),
        text("span", `${progress}%`, "detail-value"),
      );
      const meter = document.createElement("div");
      meter.className = "meter";
      meter.role = "progressbar";
      meter.ariaValueMin = "0";
      meter.ariaValueMax = "100";
      meter.ariaValueNow = String(progress);
      meter.style.setProperty("--progress", `${progress}%`);
      detail.append(meter);

      card.append(top);
      card.append(text("h3", job.target_title));
      card.append(detail);
      if (job.intervention_reason) card.append(text("div", job.intervention_reason, "signal"));

      const foot = document.createElement("div");
      foot.className = "card-foot";
      foot.append(text("code", job.job_id));
      if (!["COMPLETED", "FAILED", "CANCELLED", "VERIFYING"].includes(job.step_state)) {
        const cancel = text("button", "취소", "secondary");
        cancel.type = "button";
        cancel.addEventListener("click", async () => {
          try {
            await mutation(`/api/v1/jobs/${encodeURIComponent(job.job_id)}/cancel`, {
              command_id: identifier("COMMAND"), idempotency_key: identifier(`${job.job_id}-cancel`),
              expected_revision: job.revision, reason: "dashboard operator cancelled job",
            });
            await loadJobs();
          } catch (error) {
            byId("job-command-status").textContent = `CANCEL / ${error.message}`;
          }
        });
        foot.append(cancel);
      }
      card.append(foot);
      return card;
    }));
  } catch (error) {
    container.replaceChildren(text("p", error.message, "error"));
  }
}

async function loadWorkers() {
  const container = byId("workers");
  try {
    const workers = await api("/api/v1/workers");
    if (!workers.length) return empty(container);
    container.replaceChildren(...workers.map((worker) => {
      const card = document.createElement("article");
      card.className = `card ${worker.state === "OFFLINE" ? "attention" : ""}`;
      const state = document.createElement("div");
      state.className = "worker-state";
      state.append(text("span", "", "worker-dot"), text("span", worker.state, "meta"));
      card.append(state);
      card.append(text("h3", worker.worker_id));
      const storage = document.createElement("div");
      storage.className = "storage-list";
      const storageIDs = worker.accessible_storage_ids.length ? worker.accessible_storage_ids : ["STORAGE —"];
      storage.append(...storageIDs.map((id) => text("span", id, "token")));
      card.append(storage);
      return card;
    }));
  } catch (error) {
    container.replaceChildren(text("p", error.message, "error"));
  }
}

byId("job-create").addEventListener("submit", async (event) => {
  event.preventDefault();
  const values = Object.fromEntries(new FormData(event.currentTarget));
  const jobId = identifier("JOB");
  try {
    await mutation("/api/v1/jobs", {
      job_id: jobId, command_id: identifier("COMMAND"), idempotency_key: `${jobId}:create`,
      expected_revision: 0, reason: "dashboard operator created job", ...values,
    });
    byId("job-command-status").textContent = jobId;
    event.currentTarget.reset();
    await loadJobs();
  } catch (error) {
    byId("job-command-status").textContent = `CREATE / ${error.message}`;
  }
});

byId("asset-search").addEventListener("submit", async (event) => {
  event.preventDefault();
  const query = new URLSearchParams(new FormData(event.currentTarget));
  const container = byId("assets");
  try {
    const assets = await api(`/api/v1/assets?${query}`);
    if (!assets.length) return empty(container);
    container.replaceChildren(...assets.map((item) => {
      const row = document.createElement("article");
      row.className = "result";
      const head = document.createElement("div");
      head.className = "result-head";
      head.append(
        text("h3", item.Metadata?.title || item.metadata?.title || "UNTITLED"),
        text("code", item.Original?.id || item.original?.id || ""),
      );
      row.append(head);
      const description = item.Metadata?.description || item.metadata?.description || "";
      if (description) row.append(text("p", description));
      return row;
    }));
  } catch (error) {
    container.replaceChildren(text("p", error.message, "error"));
  }
});

byId("transcript-search").addEventListener("submit", async (event) => {
  event.preventDefault();
  const query = new URLSearchParams(new FormData(event.currentTarget));
  const container = byId("transcripts");
  try {
    const hits = await api(`/api/v1/transcripts/search?${query}`);
    if (!hits.length) return empty(container);
    container.replaceChildren(...hits.map((hit) => {
      const row = document.createElement("article");
      row.className = "result";
      const head = document.createElement("div");
      head.className = "result-head";
      head.append(
        text("code", hit.original_asset_id),
        text("code", `${hit.segment.start_millis}–${hit.segment.end_millis} ms`),
      );
      row.append(head);
      row.append(text("p", hit.segment.text));
      return row;
    }));
  } catch (error) {
    container.replaceChildren(text("p", error.message, "error"));
  }
});

byId("refresh-jobs").addEventListener("click", loadJobs);
byId("refresh-workers").addEventListener("click", loadWorkers);
loadHealth();
loadJobs();
loadWorkers();
