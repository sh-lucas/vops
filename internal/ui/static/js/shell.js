// The shell: a resizable sidebar (projects first, then the host's pages), a top bar with where you are and what
// needs you, and the login. Everything renders from the app store.
import { h, put, icon } from "./lib/dom.js";
import { select } from "./lib/store.js";
import { api } from "./lib/api.js";
import { dec } from "./lib/format.js";
import { app, SIDE } from "./state.js";
import { PROJECT_RE, health, previewsOf, projectHref, treeRows } from "./model.js";
import { busy, healthDot } from "./ui.js";
import { openPlan } from "./plan.js";

const NAV = [
  ["#/files", "Files", "file"], ["#/registry", "Registry", "box"], ["#/users", "Users", "user"],
  ["#/events", "Events", "activity"], ["#/system", "System", "cpu"], ["#/notifications", "Notifications", "bell"],
];
const $ = (id) => document.getElementById(id);

// the project a route is about (shop@pr-42 is a preview of shop)
function currentProject(path) {
  const m = path.match(PROJECT_RE) || path.match(/^\/logs\/(.+)$/);
  return m ? dec(m[1]).split("@")[0] : null;
}

export function mountShell() {
  const shell = $("shell"), side = $("side"), top = $("topbar");

  // ---- sidebar
  const navLink = (href, label, ic, extra) => h("a", { href, class: "nav", title: label }, icon(ic), h("span", { class: "label" }, label), extra);
  const alertBadge = h("span", { class: "badge", title: "open alerts", hidden: true });
  const overviewLink = navLink("#/", "Overview", "grid");
  const projects = h("div", { class: "side-projects" });
  const projectCount = h("span", { class: "count" });
  const domain = h("span", { class: "domain" });
  const user = h("span", { class: "who" });
  const logout = h("button", { class: "btn ghost icon", title: "Log out", "aria-label": "Log out", onclick: () => busy(logout, async () => {
    await api("POST", "/logout").catch(() => {});
    app.set({ authed: false, status: null });
  }) }, icon("logout"));
  const collapse = h("button", { class: "btn ghost icon collapse", "aria-label": "Collapse the sidebar", onclick: () => app.set((s) => ({ rail: !s.rail })) }, icon("sidebar"));
  const handle = resizeHandle(side);
  put(side,
    h("div", { class: "side-top" }, h("a", { href: "#/", class: "brand" }, h("span", { class: "logo" }, "v"), h("span", { class: "label" }, h("strong", {}, "vops"), domain)), collapse),
    h("div", { class: "side-scroll" },
      h("nav", { class: "side-nav" }, overviewLink),
      h("div", { class: "side-label" }, h("span", {}, "Projects"), projectCount),
      projects,
      h("div", { class: "side-label" }, h("span", {}, "Host")),
      h("nav", { class: "side-nav" }, NAV.map(([href, label, ic]) => navLink(href, label, ic, href === "#/notifications" ? alertBadge : null)))),
    h("div", { class: "side-foot" }, h("span", { class: "avatar", "aria-hidden": "true" }), user, logout),
    handle);

  const layout = (s) => {
    shell.classList.toggle("rail", s.rail);
    shell.style.setProperty("--side-w", (s.rail ? SIDE.rail : s.side) + "px");
    handle.setAttribute("aria-valuenow", s.rail ? SIDE.rail : s.side);
    collapse.title = s.rail ? "Expand the sidebar" : "Collapse the sidebar";
  };
  layout(app.get());
  select(app, (s) => s.side + (s.rail ? "r" : ""), () => layout(app.get()));

  const projectRows = (st) => treeRows(st.projects).flatMap((r) => r.project
    ? [h("a", { class: "sp", href: projectHref(r.project.path), "data-path": r.project.path, style: "--d:" + r.depth, title: r.project.path + " · " + health(r.project).label },
      h("span", { class: "ini", "aria-hidden": "true" }, r.name.slice(0, 2)), healthDot(r.project), h("span", { class: "label" }, r.name)),
    ...previewsOf(st, r.project.path).map((x) => h("a", { class: "sp pv", href: projectHref(x.project, "previews"), style: "--d:" + (r.depth + 1), title: "preview " + x.name + " of " + x.project }, icon("git"), h("span", { class: "label" }, x.name)))]
    : [h("div", { class: "sp folder", style: "--d:" + r.depth }, h("span", { class: "label" }, r.name + "/"))]);
  // re-render the tree only when a name, a health or a preview changes, not on every poll
  select(app, (s) => s.status ? JSON.stringify([s.status.projects.map((p) => [p.path, health(p).kind, health(p).label]), s.status.previews]) : "", () => {
    const st = app.get().status;
    if (!st) return;
    projectCount.textContent = st.projects.length || "";
    put(projects, st.projects.length ? projectRows(st) : h("div", { class: "side-empty" }, "No projects yet"));
    markActive();
  });
  select(app, (s) => s.status, (st) => {
    if (!st) return;
    domain.textContent = st.domain || "no domain";
    user.textContent = st.user || "";
    side.querySelector(".avatar").textContent = (st.user || "?").slice(0, 1).toUpperCase();
    alertBadge.hidden = !st.alerts;
    alertBadge.textContent = st.alerts || "";
  });

  const markActive = () => {
    const path = app.get().route.path;
    for (const a of side.querySelectorAll("a.nav")) {
      const t = a.getAttribute("href").slice(1);
      a.classList.toggle("active", t === "/" ? path === "/" : path.startsWith(t));
    }
    const cur = currentProject(path);
    for (const a of projects.querySelectorAll("a.sp")) a.classList.toggle("active", !a.classList.contains("pv") && a.dataset.path === cur);
  };
  select(app, (s) => s.route, markActive);

  // ---- drawer (small screens)
  $("scrim").onclick = () => app.set({ navOpen: false });
  document.addEventListener("keydown", (e) => { if (e.key === "Escape") app.set({ navOpen: false }); });
  select(app, (s) => s.navOpen, (open) => document.body.classList.toggle("nav-open", open));

  // ---- top bar: where you are, and what needs you
  const crumbs = h("div", { class: "crumbs" });
  const needs = h("div", { class: "needs" });
  put(top, h("button", { class: "btn ghost icon menu", "aria-label": "Menu", onclick: () => app.set({ navOpen: true }) }, icon("menu")), crumbs, h("span", { class: "spacer" }), needs);
  const renderTop = () => {
    const { route, status: st } = app.get();
    const cur = currentProject(route.path);
    const nav = NAV.find(([href]) => route.path.startsWith(href.slice(1)));
    crumbs.classList.toggle("solo", !cur); // the page title already says it on wide screens
    put(crumbs, cur ? cur.split("/").map((part, i, all) => h("span", { class: i === all.length - 1 ? "here" : null }, part))
      : h("span", { class: "here" }, nav ? nav[1] : "Overview"));
    if (!st) return put(needs);
    const px = st.proxy, waiting = (st.releases || []).length;
    put(needs,
      px && !px.up ? h("span", { class: "pill bad", title: "the proxy is not running: sites are down" }, h("span", { class: "dot bad" }), "Proxy down") : null,
      px && px.up && !px.in_sync ? h("span", { class: "pill warn", title: "the proxy doesn't run the daemon's current routing table" }, h("span", { class: "dot warn" }), "Proxy out of sync") : null,
      waiting ? h("a", { class: "pill warn", href: "#/", title: "pushes waiting for Approve (x-vops.release)" }, h("span", { class: "dot warn" }), waiting + " to approve") : null,
      st.changes ? h("button", { class: "pill warn", onclick: openPlan, title: "What runs differs from git: review and apply" }, h("span", { class: "dot warn" }), "Pending", h("span", { class: "long" }, " changes")) : null);
  };
  select(app, (s) => s.route, renderTop);
  select(app, (s) => s.status, renderTop);
}

// resizeHandle: drag to resize (below the minimum it collapses to icons), double-click resets, arrows from the keyboard
function resizeHandle(side) {
  const handle = h("div", { class: "side-handle", role: "separator", tabindex: "0", "aria-orientation": "vertical", "aria-label": "Resize the sidebar",
    "aria-valuemin": SIDE.rail, "aria-valuemax": SIDE.max, title: "Drag to resize · double-click to reset" });
  const setWidth = (x) => {
    if (x < SIDE.min - 56) return app.set({ rail: true });
    app.set({ rail: false, side: Math.round(Math.min(SIDE.max, Math.max(SIDE.min, x))) });
  };
  handle.addEventListener("pointerdown", (e) => {
    if (e.button !== 0) return;
    e.preventDefault();
    handle.setPointerCapture(e.pointerId);
    document.body.classList.add("resizing");
    const left = side.getBoundingClientRect().left;
    const move = (ev) => setWidth(ev.clientX - left);
    const up = () => {
      handle.removeEventListener("pointermove", move);
      document.body.classList.remove("resizing");
    };
    handle.addEventListener("pointermove", move);
    handle.addEventListener("pointerup", up, { once: true });
    handle.addEventListener("pointercancel", up, { once: true });
  });
  handle.addEventListener("dblclick", () => app.set({ rail: false, side: SIDE.def }));
  handle.addEventListener("keydown", (e) => {
    const s = app.get(), w = s.rail ? SIDE.rail : s.side;
    if (e.key === "ArrowLeft") setWidth(w - 24);
    else if (e.key === "ArrowRight") setWidth(s.rail ? SIDE.min : w + 24);
    else if (e.key === "Enter") app.set({ rail: !s.rail });
    else return;
    e.preventDefault();
  });
  return handle;
}

export function showLogin(onDone) {
  const box = $("login");
  if (box.firstChild) return;
  const user = h("input", { placeholder: "User", value: "admin", autocomplete: "username", "aria-label": "user", autocapitalize: "none" });
  const pw = h("input", { type: "password", placeholder: "Password", autocomplete: "current-password", "aria-label": "password" });
  const err = h("div", { class: "error small center" });
  const btn = h("button", { class: "btn primary" }, "Log in");
  put(box, h("div", { class: "login-wrap" }, h("form", { class: "login", onsubmit: async (e) => {
    e.preventDefault();
    err.textContent = "";
    await busy(btn, async () => {
      try {
        await api("POST", "/login", { user: user.value.trim(), password: pw.value });
        box.replaceChildren();
        onDone();
      } catch (x) { err.textContent = x.message; }
    });
  } },
    h("span", { class: "logo" }, "v"),
    h("h1", {}, "vops"),
    h("p", { class: "muted small center" }, "Log in with an admin account"),
    user, pw, btn, err)));
  pw.focus();
}
