// Archive Workspace entry. The page painted a placeholder shell from plain
// HTML. The first refresh is asked for before anything is drawn, and the app
// takes the shell's place in one step once it answers (or fails), so the
// screen is drawn once instead of a placeholder and then the screen.

import { html, render, effect } from "./lib.js";
import { store, startPolling } from "./store.js";
import { setScroller } from "./router.js";
import { App } from "./shell.js";

startPolling();
await new Promise((resolve) => {
  const stop = effect(() => {
    if (!store.ready.value && !store.failures.value) return;
    resolve();
    queueMicrotask(() => stop());
  });
});
const $app = document.getElementById("app");
$app.replaceChildren();
render(html`<${App} onView=${setScroller} />`, $app);
