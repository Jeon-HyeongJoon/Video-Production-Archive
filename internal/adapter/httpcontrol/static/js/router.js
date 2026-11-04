// Hash routes keep every screen addressable without server-side routing:
//   #/status                          상태
//   #/drives                          드라이브 목록
//   #/drives/<drive>[?folder=<f>]     드라이브 상세, 그 폴더의 문제를 펼침
//   #/workers[?worker=<w>|job=<j>]    Worker, 그 Worker나 드라이브 밖 작업을 강조
//   #/search[?q=<text>]               검색 결과
//   #/settings                        설정

export const menus = [
  { id: "status", label: "상태", path: "#/status" },
  { id: "drives", label: "드라이브", path: "#/drives" },
  { id: "workers", label: "Worker", path: "#/workers" },
];

export function currentRoute() {
  const raw = location.hash.replace(/^#/, "") || "/status";
  const [pathPart, queryPart = ""] = raw.split("?");
  const parts = pathPart.split("/").filter(Boolean).map(decodeURIComponent);
  const query = new URLSearchParams(queryPart);
  const menu = parts[0] || "status";
  if (menu === "drives" && parts[1]) return { menu, name: "drive", key: `drive:${parts[1]}`, driveID: parts[1], query };
  if (menu === "drives") return { menu, name: "drives", key: "drives", query };
  if (menu === "workers") return { menu, name: "workers", key: "workers", query };
  if (menu === "search") return { menu, name: "search", key: "search", query };
  if (menu === "settings") return { menu, name: "settings", key: "settings", query };
  return { menu: "status", name: "status", key: "status", query };
}

export function go(path) {
  if (location.hash === path) window.dispatchEvent(new HashChangeEvent("hashchange"));
  else location.hash = path;
}
