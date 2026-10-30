// vops dashboard: plain ES modules, no build step. main wires the store, the shell and the router.
import { select } from "./lib/store.js";
import { app, refresh } from "./state.js";
import { mountShell, showLogin } from "./shell.js";
import { leave } from "./scope.js";
import { route } from "./router.js";

mountShell();
const shell = document.getElementById("shell");

const start = () => {
  shell.hidden = false;
  route();
  refresh();
};
select(app, (s) => s.authed, (authed) => {
  if (authed) return start();
  leave();
  shell.hidden = true;
  showLogin(() => app.set({ authed: true }));
});
window.addEventListener("hashchange", () => { if (app.get().authed) route(); });

// a click on a notification focuses an open dashboard and sends it where the notification points
if ("serviceWorker" in navigator) navigator.serviceWorker.addEventListener("message", (e) => { if (e.data && e.data.open) location.href = e.data.open; });

start();
