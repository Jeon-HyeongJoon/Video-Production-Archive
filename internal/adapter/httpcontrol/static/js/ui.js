// Shared pieces every screen draws with: status icons (shape, glyph and colour
// together, so no state is told by colour alone), folds that mount their
// content only while open, rows that enter and leave with a short movement,
// and text that fades from one value to the next.

import { html, useEffect, useRef, useState } from "./lib.js";
import { EXIT_MS, reducedMotion } from "./motion.js";

const CIRCLE = "M8 1.75a6.25 6.25 0 1 0 0 12.5a6.25 6.25 0 1 0 0-12.5z";
const ARC = "M8 1.75a6.25 6.25 0 0 1 6.25 6.25";
const ICONS = {
  fault: [CIRCLE, "M5.7 5.7l4.6 4.6M10.3 5.7l-4.6 4.6"],
  decide: ["M8 1.1 14.9 8 8 14.9 1.1 8z", "M8 4.8v3.9M8 11.1v.1"],
  work: [CIRCLE, ARC],
  part: [CIRCLE, ARC],
  resource: [CIRCLE, ARC],
  done: [CIRCLE, "M5.1 8.2l2 2 3.9-4.2"],
  idle: [CIRCLE, ""],
  online: [CIRCLE, ""],
};

export const GLYPH = {
  jobs: "M3 4.5h10M3 8h10M3 11.5h6",
  connections: "M2.5 3h11v4h-11zM2.5 9h11v4h-11zM5 5h.01M5 11h.01",
  settings: "M8 5.6a2.4 2.4 0 1 0 0 4.8 2.4 2.4 0 0 0 0-4.8zM8 1.8v1.6M8 12.6v1.6M1.8 8h1.6M12.6 8h1.6M3.6 3.6l1.1 1.1M11.3 11.3l1.1 1.1M3.6 12.4l1.1-1.1M11.3 4.7l1.1-1.1",
  search: "M7 2.5a4.5 4.5 0 1 0 0 9 4.5 4.5 0 0 0 0-9zM10.4 10.4 13.5 13.5",
  bell: "M4 10.5V7a4 4 0 0 1 8 0v3.5l1 1.5H3zM6.6 13.8h2.8",
  plus: "M8 3.5v9M3.5 8h9",
  close: "M4 4l8 8M12 4l-8 8",
  up: "M8 12.5v-9M4 7.5l4-4 4 4",
  next: "M6 3.5 10.5 8 6 12.5",
  screen: "M2 3.5h12v7.5H2zM5.5 13.5h5",
  down: "M8 3.5v9M4 8.5l4 4 4-4",
};

// A running icon turns inside an HTML box: the compositor turns a box without
// repainting it, while a turning SVG is repainted on the main thread every frame.
export function StatusIcon({ kind, label = "" }) {
  const known = ICONS[kind] ? kind : "idle";
  const [shape, glyph] = ICONS[known];
  const icon = html`<svg class="sicon" viewBox="0 0 16 16" data-kind=${known} role=${label ? "img" : null} aria-label=${label || null} aria-hidden=${label ? null : "true"}><path class="sicon-shape" d=${shape} /><path class="sicon-glyph" d=${glyph} /></svg>`;
  return known === "work" ? html`<span class="spin">${icon}</span>` : icon;
}

export const Glyph = ({ d }) => html`<svg class="glyph" viewBox="0 0 16 16" aria-hidden="true"><path d=${d} /></svg>`;
export const Chevron = () => html`<svg class="chev" viewBox="0 0 16 16" aria-hidden="true"><path d="M4.5 6.5 8 10l3.5-3.5" /></svg>`;

// Fine shows a normal state by its icon alone; the column already says what it
// is, and the word stays for screen readers and the hover.
export const Fine = ({ text, kind = "done" }) => html`<span class="ok" title=${text}><${StatusIcon} kind=${kind} /><span class="sr-only">${text}</span></span>`;

// IssueTags counts problems; the icon carries the colour, the words stay in
// the normal ink.
export function IssueTags({ fault = 0, decide = 0, cls = "" }) {
  if (!fault && !decide) return null;
  return html`<span class=${`issues ${cls}`.trim()}>
    ${fault ? html`<span title=${`실패 ${fault}`}><${StatusIcon} kind="fault" />실패 ${fault}</span>` : null}
    ${decide ? html`<span title=${`직접 확인 필요 ${decide}`}><${StatusIcon} kind="decide" />확인 ${decide}</span>` : null}
  </span>`;
}

// A keyed span: a new value is a new element, so it fades in instead of
// jumping. The first value does not animate, so mounting a long list stays cheap.
export function Swap({ text, cls = "" }) {
  const first = useRef(text);
  if (first.current !== text) first.current = null;
  return html`<span class=${`swap-box ${cls}`.trim()}><span class=${first.current === null ? "swap swap-in" : "swap"} key=${text}>${text}</span></span>`;
}

export const Meter = ({ value, done = false, label = "" }) => {
  const share = Math.max(0, Math.min(1, value || 0));
  return html`<span class="meter" role="progressbar" aria-valuemin="0" aria-valuemax="100" aria-valuenow=${Math.round(share * 100)} aria-label=${label || null}><span style=${`--p:${share.toFixed(4)}`} data-done=${done ? "true" : null}></span></span>`;
};

// Placeholder rows hold the place of content still on its way, in its own
// height, so nothing jumps when it lands. They do not shimmer: a shimmer
// repaints every frame.
export const Skeleton = ({ n, cls = "" }) => Array.from({ length: n }, (_, index) => html`<div class=${`skel-row ${cls}`.trim()} key=${`skel${index}`} aria-hidden="true"><span class="skel"></span></div>`);

export const Fact = ({ label, value, ...attributes }) => html`<div class="fact" ...${attributes}><span class="fact-label">${label}</span><span class="fact-value">${value}</span></div>`;

export const Empty = ({ text, cls = "" }) => html`<p class=${`empty ${cls}`.trim()}>${text}</p>`;

// useLinger keeps closing content mounted until its fold has collapsed.
export function useLinger(open) {
  const [shown, setShown] = useState(open);
  useEffect(() => {
    if (open) { setShown(true); return undefined; }
    const timer = setTimeout(() => setShown(false), reducedMotion.matches ? 0 : EXIT_MS);
    return () => clearTimeout(timer);
  }, [open]);
  return open || shown;
}

// Fold mounts its content only while open or closing: hidden content would
// still run its transitions and animations, and each of those asks the main
// thread for frames.
export function Fold({ open, keep = false, id = null, children }) {
  const shown = useLinger(open) || keep;
  return html`<div class="fold" id=${id} data-open=${open ? "true" : "false"}><div class="fold-clip" inert=${!open}>${shown ? children : null}</div></div>`;
}

// Presence keeps a removed item on screen while it animates out, which a
// framework alone does not do.
export function Presence({ items, keyOf, children: draw }) {
  const mounted = useRef(false);
  const entries = useRef(new Map());
  const order = useRef([]);
  const [, bump] = useState(0);
  const keys = items.map(keyOf);
  const live = new Set(keys);
  for (const item of items) {
    const key = keyOf(item);
    const entry = entries.current.get(key);
    if (!entry) entries.current.set(key, { item, leaving: false, entering: mounted.current });
    else {
      entry.item = item;
      if (entry.leaving) { clearTimeout(entry.timer); entry.leaving = false; }
    }
  }
  for (const [key, entry] of entries.current) {
    if (live.has(key) || entry.leaving) continue;
    entry.leaving = true;
    entry.timer = setTimeout(() => { entries.current.delete(key); bump((count) => count + 1); }, reducedMotion.matches ? 0 : EXIT_MS);
  }
  const merged = [...keys];
  order.current.forEach((key, index) => {
    if (!live.has(key) && entries.current.get(key)?.leaving) merged.splice(Math.min(index, merged.length), 0, key);
  });
  order.current = merged;
  useEffect(() => { mounted.current = true; }, []);
  useEffect(() => () => entries.current.forEach((entry) => clearTimeout(entry.timer)), []);
  return merged.map((key) => {
    const entry = entries.current.get(key);
    return html`<div class=${entry.entering ? "pres pres-in" : "pres"} key=${key} data-leaving=${entry.leaving ? "true" : null}><div class="pres-clip">${draw(entry.item, entry)}</div></div>`;
  });
}

// Crumbs name the menu and the object in view; each step back is a link.
export const Crumbs = ({ items }) => html`<nav class="crumbs" aria-label="위치">
  ${items.map(([text, href], index) => [
    index ? html`<span key=${`s${index}`} class="crumb-sep" aria-hidden="true">/</span>` : null,
    href ? html`<a key=${`c${index}`} class="crumb" href=${href}>${text}</a>` : html`<span key=${`c${index}`} class="crumb" aria-current="page">${text}</span>`,
  ])}
</nav>`;

export function highlight(text, query) {
  if (!query) return text;
  const at = text.toLowerCase().indexOf(query.toLowerCase());
  if (at < 0) return text;
  return [text.slice(0, at), html`<mark>${text.slice(at, at + query.length)}</mark>`, text.slice(at + query.length)];
}

export const cssName = (id) => String(id).replace(/[^a-zA-Z0-9_-]/g, "-");
