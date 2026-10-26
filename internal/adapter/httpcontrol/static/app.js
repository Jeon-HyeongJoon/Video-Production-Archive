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
  try {
    const health = await api("/healthz");
    byId("health").textContent = `정상 · ${health.version}`;
    byId("health").classList.add("ok");
  } catch (error) {
    byId("health").textContent = `연결 실패 · ${error.message}`;
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
      card.append(text("p", job.template_name, "meta"));
      card.append(text("h3", job.target_title));
      card.append(text("p", `${job.current_step_id} · ${job.step_state}`, "state"));
      card.append(text("p", job.intervention_reason || `${Math.round(job.progress * 100)}%`, "detail"));
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
            byId("job-command-status").textContent = `취소 실패 · ${error.message}`;
          }
        });
        card.append(cancel);
      }
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
      card.append(text("p", worker.state, "meta"));
      card.append(text("h3", worker.worker_id));
      card.append(text("p", worker.accessible_storage_ids.join(" · ") || "Storage 보고 없음", "detail"));
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
    byId("job-command-status").textContent = `작업 생성됨 · ${jobId}`;
    event.currentTarget.reset();
    await loadJobs();
  } catch (error) {
    byId("job-command-status").textContent = `생성 실패 · ${error.message}`;
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
      row.append(text("h3", item.Metadata?.title || item.metadata?.title || "제목 없음"));
      row.append(text("p", item.Metadata?.description || item.metadata?.description || ""));
      row.append(text("code", item.Original?.id || item.original?.id || ""));
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
      row.append(text("p", hit.segment.text));
      row.append(text("code", `${hit.original_asset_id} · ${hit.segment.start_millis}–${hit.segment.end_millis} ms`));
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
