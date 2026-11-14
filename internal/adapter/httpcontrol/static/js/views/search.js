// 검색 결과: the sidebar's search box opens catalog and transcript search
// with one record's detail. Search is a tool beside the menus, not a menu.

import { html, useEffect, useState } from "../lib.js";
import { formatSize, shortTime, api } from "../core.js";
import { route } from "../router.js";
import { Fact, Empty, Skeleton, highlight } from "../ui.js";

const AUTHOR = { HUMAN: "담당자", ARCHIVIST: "에이전트", SYSTEM: "시스템" };

function timecode(millis) {
  const total = Math.floor((millis || 0) / 1000);
  return [Math.floor(total / 3600), Math.floor((total % 3600) / 60), total % 60].map((value) => String(value).padStart(2, "0")).join(":");
}

const duration = (millis) => (millis ? timecode(millis) : "—");

export function SearchScreen() {
  const query = (route.value.query.get("q") || "").trim();
  const [scope, setScope] = useState(route.value.query.get("scope") === "text" ? "text" : "assets");
  const [answer, setAnswer] = useState(null);
  const [selected, setSelected] = useState("");
  const [summaries, setSummaries] = useState([]);
  useEffect(() => {
    const requests = new AbortController();
    setAnswer(null);
    if (!query) { setAnswer({ assets: [], hits: [] }); return undefined; }
    Promise.all([
      api(`/api/v1/assets?text=${encodeURIComponent(query)}`, { signal: requests.signal }),
      api(`/api/v1/transcripts/search?text=${encodeURIComponent(query)}`, { signal: requests.signal }),
    ]).then(([assets, hits]) => {
      const found = { assets: assets || [], hits: hits || [] };
      setAnswer(found);
      setSelected(found.assets[0]?.original.id || found.hits[0]?.original_asset_id || "");
    }).catch((error) => { if (error.name !== "AbortError") setAnswer({ assets: [], hits: [], error: error.message }); });
    return () => requests.abort();
  }, [query]);
  useEffect(() => {
    if (!selected) { setSummaries([]); return undefined; }
    const requests = new AbortController();
    api(`/api/v1/assets/${encodeURIComponent(selected)}/summaries`, { signal: requests.signal })
      .then((values) => setSummaries(values || [])).catch(() => {});
    return () => requests.abort();
  }, [selected]);
  const assets = answer?.assets || [];
  const hits = answer?.hits || [];
  const item = assets.find((entry) => entry.original.id === selected);
  let list;
  if (!query) list = html`<${Empty} text="검색어 없음" />`;
  else if (!answer) list = html`<${Skeleton} n=${4} />`;
  else if (answer.error) list = html`<${Empty} text=${answer.error} />`;
  else if (scope === "assets") {
    list = assets.length ? assets.map((entry) => html`<button type="button" class="result-item result-row" key=${entry.original.id} data-asset-id=${entry.original.id}
      aria-pressed=${entry.original.id === selected ? "true" : "false"} onClick=${() => setSelected(entry.original.id)}>
      <b title=${[entry.original.original_name, duration(entry.inspection?.duration_millis)].join(" · ")}>${highlight(entry.metadata.title || entry.original.original_name, query)}</b>
      <small>${entry.metadata.description || ""}</small>
    </button>`) : html`<${Empty} text="결과 없음" />`;
  } else {
    list = hits.length ? hits.map((hit, index) => html`<button type="button" class="result-item result-row hit" key=${`${hit.original_asset_id}${index}`} data-asset-id=${hit.original_asset_id}
      onClick=${() => setSelected(hit.original_asset_id)}>
      <span class="timecode">${timecode(hit.segment.start_millis)}</span><span>${highlight(hit.segment.text, query)}<br /><small class="num">${hit.original_asset_id}</small></span>
    </button>`) : html`<${Empty} text="자막·텍스트에서 찾지 못함" />`;
  }
  const inspection = item?.inspection;
  const video = inspection?.video_streams?.[0];
  return [
    html`<div key="head" class="page-head">
      <div class="page-title"><h1 id="search-query" title=${query ? `“${query}”` : "검색어 없음"}>${query ? `“${query}” 검색 결과` : "검색"}</h1><small class="sub">사이드바 검색칸에 쓰고 Enter · 단축키 /</small></div>
      <div class="seg plain-seg" id="search-scopes" role="group" aria-label="검색 대상">
        ${[["assets", "보관 영상", assets.length], ["text", "자막·텍스트", hits.length]].map(([key, text, count]) => html`<button type="button" key=${key} data-scope=${key}
          aria-pressed=${scope === key ? "true" : "false"} onClick=${() => setScope(key)}>${text} <span class="num">${count}</span></button>`)}
      </div>
    </div>`,
    html`<div key="split" class="split">
      <section class="panel results-panel" id="search-results" aria-label="검색 결과">${list}</section>
      <section class="panel detail-panel" id="record-detail" aria-label="보관 영상 상세">
        ${item ? html`<div class="panel-toolbar"><b>${item.metadata.title || item.original.original_name}</b><span class="faint-id">${item.original.id}</span></div>
          <div class="detail-body">
            <div class="facts">
              <${Fact} label="원래 이름" value=${item.original.original_name} />
              <${Fact} label="크기" value=${formatSize(item.original.size_bytes)} />
              <${Fact} label="길이" value=${duration(inspection?.duration_millis)} />
              <${Fact} label="기술정보" value=${video ? `${video.codec} · ${video.width}×${video.height}` : "—"} />
              <${Fact} label="checksum" value=${`${item.original.checksum.slice(0, 12)}…`} />
              <${Fact} label="등록" value=${shortTime(item.original.registered_at)} />
            </div>
            <div class="catalog-box">
              <span class="fact-label">영상 정보 r${item.metadata.revision}</span>
              <p class="summary-text">${item.metadata.description || "—"}</p>
              <div class="keyword-list mentions">${(item.metadata.keywords || []).map((keyword) => html`<span class="ref-chip mention" key=${keyword}>${keyword}</span>`)}</div>
              <small class="muted">${AUTHOR[item.metadata.author_type] || item.metadata.author_type} 작성</small>
            </div>
            <div class="segments"><span class="fact-label">요약</span>
              ${summaries.length ? summaries.map((entry) => html`<span key=${entry.id}>r${entry.revision} · ${entry.content?.text || entry.id}</span>`) : html`<span class="muted">요약 없음</span>`}
            </div>
          </div>` : html`<${Empty} text="보관 영상을 선택하세요" />`}
      </section>
    </div>`,
  ];
}
