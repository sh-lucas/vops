// Page scope: cleanups registered while a page is shown run when the user navigates away.
import { select } from "./lib/store.js";
import { app } from "./state.js";

let gen = 0;
const cleanups = [];
export const onLeave = (fn) => cleanups.push(fn);
// watch runs fn(value) when pick(state) changes, until the user leaves the page
export const watch = (pick, fn) => onLeave(select(app, pick, fn));

// leave ends the current page and returns alive() for the next one
export function leave() {
  const mine = ++gen;
  for (const f of cleanups.splice(0)) { try { f(); } catch { /* a cleanup must not stop the others */ } }
  return () => mine === gen;
}
