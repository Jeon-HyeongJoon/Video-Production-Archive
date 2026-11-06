// 검색 결과: the top bar's search box opens catalog and transcript search with
// one record's detail. Search is a tool beside the menus, not a menu.

import { el, formatSize, shortTime, api, emptyState } from "../core.js";

const AUTHOR = { HUMAN: "담당자", ARCHIVIST: "Agent", SYSTEM: "시스템" };

function timecode(millis) {
  const total = Math.floor((millis || 0) / 1000);
  const parts = [Math.floor(total / 3600), Math.floor((total % 3600) / 60), total % 60];
  return parts.map((value) => String(value).padStart(2, "0")).join(":");
}

function duration(millis) {
  return millis ? timecode(millis) : "—";
}

function highlight(text, query) {
  const index = query ? text.indexOf(query) : -1;
  if (index < 0) return [text];
  return [text.slice(0, index), el("mark", {}, text.slice(index, index + query.length)), text.slice(index + query.length)];
}

export function mount(root, route) {
  const requests = new AbortController();
  let disposed = false;
  let query = route.query.get("q") || "";
  let scope = route.query.get("scope") === "text" ? "text" : "assets";
  let assets = [];
  let hits = [];
  let selected = "";
  let summaries = [];
  let searched = false;
  const scopes = el("div", { class: "seg", role: "group", "aria-label": "검색 대상", id: "search-scopes" });
  const results = el("section", { class: "panel results-panel", id: "search-results", "aria-label": "검색 결과" });
  const detail = el("section", { class: "panel detail-panel", id: "record-detail", "aria-label": "기록물 상세" });
  const heading = el("h1", { id: "search-query" }, "검색 결과");
  root.append(
    el("div", { class: "page-head" }, el("div", { class: "page-title" }, heading), scopes),
    el("div", { class: "split" }, results, detail));

  async function search() {
    if (!query) { assets = []; hits = []; searched = false; return; }
    const [foundAssets, foundHits] = await Promise.all([
      api(`/api/v1/assets?text=${encodeURIComponent(query)}`, { signal: requests.signal }),
      api(`/api/v1/transcripts/search?text=${encodeURIComponent(query)}`, { signal: requests.signal }),
    ]);
    assets = foundAssets || [];
    hits = foundHits || [];
    searched = true;
    if (!assets.some((item) => item.original.id === selected)) selected = assets[0]?.original.id || hits[0]?.original_asset_id || "";
    summaries = selected ? (await api(`/api/v1/assets/${encodeURIComponent(selected)}/summaries`, { signal: requests.signal })) || [] : [];
  }

  async function choose(assetID) {
    selected = assetID;
    summaries = (await api(`/api/v1/assets/${encodeURIComponent(assetID)}/summaries`, { signal: requests.signal })) || [];
    if (disposed) return;
    render();
  }

  function render() {
    scopes.replaceChildren(...[["assets", "자료", assets.length], ["text", "전사문", hits.length]].map(([id, name, count]) =>
      el("button", { type: "button", "aria-pressed": String(scope === id), dataset: { scope: id }, onclick: () => { scope = id; render(); } }, name, el("span", { class: "num" }, String(count)))));
    const rows = scope === "assets"
      ? assets.map((item) => el("button", { type: "button", class: "result-row", "aria-pressed": String(item.original.id === selected), dataset: { assetId: item.original.id }, onclick: () => choose(item.original.id) },
        el("b", { title: [item.original.original_name, duration(item.inspection?.duration_millis)].join(" · ") }, ...highlight(item.metadata.title || item.original.original_name, query)),
        el("span", { class: "muted" }, item.metadata.description || "")))
      : hits.map((hit) => el("button", { type: "button", class: "result-row hit-row", dataset: { assetId: hit.original_asset_id }, onclick: () => choose(hit.original_asset_id) },
        el("span", { class: "timecode num" }, timecode(hit.segment.start_millis)), el("span", {}, ...highlight(hit.segment.text, query)), el("small", { class: "num muted" }, hit.original_asset_id)));
    results.replaceChildren(...(rows.length ? rows : [emptyState(searched ? "결과 없음" : "검색어 없음")]));
    const item = assets.find((entry) => entry.original.id === selected);
    if (!item) { detail.replaceChildren(emptyState("기록물 선택 없음")); return; }
    const inspection = item.inspection;
    const video = inspection?.video_streams?.[0];
    const facts = [
      ["원래 이름", item.original.original_name], ["크기", formatSize(item.original.size_bytes)],
      ["길이", duration(inspection?.duration_millis)], ["기술정보", video ? `${video.codec} · ${video.width}×${video.height}` : "—"],
      ["checksum", `${item.original.checksum.slice(0, 12)}…`], ["입고", shortTime(item.original.registered_at)],
    ];
    detail.replaceChildren(
      el("div", { class: "detail-head" }, el("b", {}, item.metadata.title || item.original.original_name)),
      el("span", { class: "detail-meta num" }, item.original.id),
      el("dl", { class: "facts" }, ...facts.flatMap(([key, value]) => [el("dt", {}, key), el("dd", {}, value)])),
      el("div", { class: "catalog-box" },
        el("span", { class: "list-label" }, `카탈로그 r${item.metadata.revision}`),
        el("p", {}, item.metadata.description || "—"),
        el("div", { class: "keyword-list" }, ...(item.metadata.keywords || []).map((keyword) => el("span", { class: "ref-chip" }, keyword))),
        el("small", { class: "muted" }, `${AUTHOR[item.metadata.author_type] || item.metadata.author_type} 작성`)),
      el("div", { class: "timeline" }, el("span", { class: "list-label" }, "요약"),
        ...(summaries.length ? summaries.map((entry) => el("span", {}, `r${entry.revision} · ${entry.content?.text || entry.id}`)) : [el("span", { class: "muted" }, "요약 없음")])));
  }

  return {
    async update(next = route) {
      const nextQuery = next.query.get("q") || "";
      if (nextQuery !== query || !searched) {
        query = nextQuery;
        heading.title = query ? `“${query}”` : "검색어 없음";
        try { await search(); } catch (error) { if (error.name !== "AbortError" && !disposed) results.replaceChildren(emptyState(error.message)); return; }
      }
      if (disposed) return;
      render();
    },
    dispose() {
      disposed = true;
      requests.abort();
    },
  };
}
