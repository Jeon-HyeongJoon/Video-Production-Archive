// Hash routes keep every screen addressable without server-side routing:
//   #/connections                     Worker PC, 드라이브, Agent 연결 관리
//   #/drives/<drive>[?folder=<f>]     드라이브 상세, 그 폴더의 문제를 펼침
//   #/jobs                            작업 목록
//   #/jobs/<job>                      영속 작업 상세
//   #/runs/<archive-run>              아카이브 실행 작업 상세
//   #/search[?q=<text>]               검색 결과
//   #/settings                        설정

export const menus = [
  { id: "jobs", label: "작업 관리", path: "#/jobs" },
  { id: "connections", label: "연결 관리", path: "#/connections" },
];

export function currentRoute() {
  const raw = location.hash.replace(/^#/, "") || "/jobs";
  const [pathPart, queryPart = ""] = raw.split("?");
  const parts = pathPart.split("/").filter(Boolean).map(decodeURIComponent);
  const query = new URLSearchParams(queryPart);
  const menu = parts[0] || "jobs";
  if (menu === "drives" && parts[1]) return { menu: "connections", name: "drive", key: `drive:${parts[1]}`, driveID: parts[1], query };
  if (menu === "drives") return { menu: "connections", name: "connections", key: "connections", query };
  if (menu === "jobs" && parts[1]) return { menu, name: "job", key: `job:${parts[1]}`, jobID: parts[1], query };
  if (menu === "jobs") return { menu, name: "jobs", key: "jobs", query };
  if (menu === "runs" && parts[1]) return { menu: "jobs", name: "run", key: `run:${parts[1]}`, workID: parts[1], query };
  if (menu === "connections") return { menu, name: "connections", key: "connections", query };
  // Keep old Worker bookmarks usable while the menu is consolidated.
  if (menu === "workers") return { menu: "connections", name: "connections", key: "connections", query };
  if (menu === "search") return { menu, name: "search", key: "search", query };
  if (menu === "settings") return { menu, name: "settings", key: "settings", query };
  // Old bookmarks and unknown routes land on the primary Job management view.
  return { menu: "jobs", name: "jobs", key: "jobs", query };
}

export function go(path) {
  if (location.hash === path) window.dispatchEvent(new HashChangeEvent("hashchange"));
  else location.hash = path;
}
