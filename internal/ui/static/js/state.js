// The app store: auth, the polled host status, the sidebar's size, the current route. Pages watch it through router.watch.
import { createStore, select } from "./lib/store.js";
import { api } from "./lib/api.js";

export const SIDE = { min: 208, max: 440, def: 264, rail: 64 };
const savedSide = Number(localStorage.getItem("vops.side"));

export const app = createStore({
  authed: true,
  status: null,
  statusAt: 0,
  route: { path: "/", params: new URLSearchParams() },
  navOpen: false, // the drawer on small screens
  side: savedSide >= SIDE.min && savedSide <= SIDE.max ? savedSide : SIDE.def,
  rail: localStorage.getItem("vops.rail") === "1", // collapsed to icons
});

select(app, (s) => s.side, (v) => localStorage.setItem("vops.side", v));
select(app, (s) => s.rail, (v) => localStorage.setItem("vops.rail", v ? "1" : "0"));
window.addEventListener("vops:logout", () => app.set({ authed: false, status: null, statusAt: 0 }));

let inflight = null;
export function refreshStatus() {
  inflight ??= api("GET", "/status").then((status) => {
    app.set({ status, statusAt: Date.now() });
    return status;
  }).finally(() => { inflight = null; });
  return inflight;
}
// refreshes in the background; errors show where status is used
export const refresh = () => { refreshStatus().catch(() => {}); };
export const getStatus = (maxAge = 4000) => {
  const s = app.get();
  return s.status && Date.now() - s.statusAt < maxAge ? Promise.resolve(s.status) : refreshStatus();
};

// the shell polls every 10s while visible and logged in
setInterval(() => { if (!document.hidden && app.get().authed) refresh(); }, 10000);
document.addEventListener("visibilitychange", () => { if (!document.hidden && app.get().authed && Date.now() - app.get().statusAt > 10000) refresh(); });
