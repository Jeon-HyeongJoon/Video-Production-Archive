// Motion shared by navigation and data arrival: a View Transition when the
// browser has one, a plain update otherwise.

const $root = document.documentElement;
const canTransition = typeof document.startViewTransition === "function";
let token = 0;

// settle lets Preact draw the update before a transition takes its picture.
export const settle = () => new Promise((resolve) => setTimeout(resolve, 0));

// How long leaving content stays mounted while it folds or fades out.
export const EXIT_MS = 260;

export const reducedMotion = matchMedia("(prefers-reduced-motion: reduce)");

// transition runs update inside a View Transition of the given kind; CSS picks
// the movement by html[data-vt]. A hidden tab updates without one.
export function transition(kind, update, after) {
  const run = async () => {
    update();
    await settle();
    after?.();
  };
  if (!canTransition || document.hidden) return run();
  const mine = ++token;
  $root.dataset.vt = kind;
  const view = document.startViewTransition(run);
  view.ready.catch(() => {});
  return view.finished.catch(() => {}).finally(() => {
    if (mine === token) delete $root.dataset.vt;
  });
}
