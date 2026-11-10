// The screen library, served from the bundled vendor files: the Control
// server allows scripts only from itself. A component reads a signal's value
// and redraws when that value changes; nothing else redraws with it.

import { h, render, options, Fragment } from "./vendor/preact.js";
import { useEffect, useLayoutEffect, useRef, useState } from "./vendor/hooks.js";
import { signal, computed, effect, batch, useComputed } from "./vendor/signals.js";
import htm from "./vendor/htm.js";

export const html = htm.bind(h);
export { h, render, options, Fragment, useEffect, useLayoutEffect, useRef, useState, signal, computed, effect, batch, useComputed };
