// vops dashboard: vanilla js, no build step. Everything is rendered with h() (no innerHTML), so data can't inject markup.
"use strict";

const $ = (id) => document.getElementById(id);
const $app = $("app"), $shell = $("shell"), $login = $("login"), $nav = $("nav"), $sideProjects = $("side-projects"), $hoststat = $("hoststat"), $pending = $("pending");

function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === null || v === undefined || v === false) continue;
    if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "class") el.className = v;
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat(Infinity)) {
    if (c === null || c === undefined || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

const ICONS = {
  grid: "M4 4h6v6H4zM14 4h6v6h-6zM4 14h6v6H4zM14 14h6v6h-6z",
  file: "M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8zM14 3v5h5",
  box: "M21 8l-9-5-9 5v8l9 5 9-5zM3 8l9 5 9-5M12 13v8",
  user: "M12 12a4 4 0 1 0 0-8 4 4 0 0 0 0 8zM4 21a8 8 0 0 1 16 0",
  activity: "M3 12h4l3-8 4 16 3-8h4",
  menu: "M4 7h16M4 12h16M4 17h16",
  logout: "M9 4H6a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h3M16 8l4 4-4 4M20 12H9",
  download: "M12 4v11M7 10l5 5 5-5M5 20h14",
  expand: "M4 9V4h5M20 9V4h-5M4 15v5h5M20 15v5h-5",
  refresh: "M20 12a8 8 0 1 1-2.34-5.66M20 4v5h-5",
  commit: "M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM3 12h6M15 12h6",
  globe: "M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM3.5 9h17M3.5 15h17M12 3a14 14 0 0 1 0 18M12 3a14 14 0 0 0 0 18",
  folder: "M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z",
  alert: "M12 4L2.5 20h19zM12 10v4M12 17h.01",
  info: "M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM12 11v5M12 8h.01",
  git: "M6 3v12M18 9a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM6 21a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM18 9a9 9 0 0 1-9 9",
  back: "M15 6l-6 6 6 6",
  camera: "M4 8h3l2-3h6l2 3h3v11H4zM12 16.5a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7z",
  undo: "M9 14L4 9l5-5M4 9h10a6 6 0 0 1 0 12h-3",
  external: "M14 4h6v6M20 4l-9 9M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5",
};

function icon(name) {
  const NS = "http://www.w3.org/2000/svg";
  const s = document.createElementNS(NS, "svg");
  s.setAttribute("viewBox", "0 0 24 24");
  s.setAttribute("class", "i");
  s.setAttribute("aria-hidden", "true");
  const p = document.createElementNS(NS, "path");
  p.setAttribute("d", ICONS[name]);
  s.append(p);
  return s;
}

function toast(msg) {
  const t = h("div", { class: "toast", role: "status" }, msg);
  $("toasts").append(t);
  setTimeout(() => t.remove(), 3500);
}

async function request(method, path, body) {
  const opts = { method, headers: {} };
  if (method !== "GET") opts.headers["X-Vops"] = "1";
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const res = await fetch("/api" + path, opts);
  if (res.status === 401 && path !== "/login") {
    showLogin();
    throw new Error("login required");
  }
  const text = await res.text();
  if (!res.ok) throw new Error(errText(text) || res.statusText);
  return text;
}
const errText = (text) => { try { return JSON.parse(text).error || text; } catch { return text; } };
async function api(method, path, body) {
  const text = await request(method, path, body);
  try { return JSON.parse(text); } catch { return text; }
}
const apiText = (path) => request("GET", path); // plain text (plan, files): never parsed as json

// stream reads a text response chunk by chunk into sink.write, keeping sink.el scrolled to the bottom when it was; returns a cancel function.
function stream(path, sink, opts = {}) {
  const ctl = new AbortController();
  const fail = (msg) => (opts.error ? opts.error(msg) : sink.write("\n" + msg));
  (async () => {
    try {
      const res = await fetch("/api" + path, { method: opts.method || "GET", headers: { "X-Vops": "1", "Content-Type": "application/json" }, body: opts.body, signal: ctl.signal });
      if (res.status === 401) return showLogin();
      if (!res.ok) return fail(errText(await res.text()));
      opts.response && opts.response(res);
      const reader = res.body.getReader();
      const dec = new TextDecoder();
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        const el = sink.el;
        const stick = el.scrollTop + el.clientHeight >= el.scrollHeight - 30;
        sink.write(dec.decode(value, { stream: true }));
        if (stick) {
          opts.stuck && opts.stuck();
          el.scrollTop = el.scrollHeight;
        }
      }
      opts.done && opts.done();
    } catch (e) {
      if (e.name !== "AbortError") fail(e.message);
    }
  })();
  return () => ctl.abort();
}
const preSink = (pre) => ({ el: pre, write: (t) => pre.append(t) });

// ---- formatting

// "repo@sha256:<64 hex>" → "repo@sha256:<12 hex>…" (the full ref goes in the title)
const shortImage = (ref) => (ref || "").replace(/@sha256:([0-9a-f]{12})[0-9a-f]+/, "@sha256:$1…");
const ago = (unix) => {
  if (!unix) return "never";
  const s = Date.now() / 1000 - unix;
  if (s < 60) return "just now";
  if (s < 3600) return Math.floor(s / 60) + "m ago";
  if (s < 172800) return Math.floor(s / 3600) + "h ago";
  return Math.floor(s / 86400) + "d ago";
};
const size = (n) => n >= 1 << 30 ? (n / (1 << 30)).toFixed(1) + " GB" : n >= 1 << 20 ? (n / (1 << 20)).toFixed(1) + " MB" : n >= 1024 ? (n / 1024).toFixed(1) + " KB" : n + " B";
// a finished job (exit 0) is fine, not an error
const stateOf = (c) => (c.Labels && c.Labels["vops.job"] && (c.State === "exited" || c.State === "stopped") && c.ExitCode === 0 ? "done" : c.State);
const short = (s, n = 12) => (s || "").replace("sha256:", "").slice(0, n);
const enc = encodeURIComponent;
const dec = (s) => { try { return decodeURIComponent(s); } catch { return s; } };
const pad = (n) => String(n).padStart(2, "0");
const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
const fmtDate = (d) => `${d.getDate()} ${MONTHS[d.getMonth()]}${d.getFullYear() !== new Date().getFullYear() ? " " + d.getFullYear() : ""} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
const fmtUnix = (u) => fmtDate(new Date(u * 1000));
const fullDate = (u) => new Date(u * 1000).toLocaleString();
const localInput = (u) => { const d = new Date(u * 1000); return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`; };
const when = (u) => h("span", { class: "nowrap", title: u ? fullDate(u) : null }, ago(u));
const until = (u) => {
  const s = u - Date.now() / 1000;
  if (s <= 0) return "now";
  if (s < 3600) return "in " + Math.max(1, Math.floor(s / 60)) + "m";
  if (s < 172800) return "in " + Math.floor(s / 3600) + "h";
  return "in " + Math.floor(s / 86400) + "d";
};
const sep = () => h("span", { class: "faint" }, "·");
const mono = (s) => h("span", { class: "mono" }, s);
const domainURL = (st, d) => (st.domain ? "https://" : "http://") + d;
// images in the history: "registry.x/shop/web:v1" shows as "web:v1"; pinned() is the ref by digest, like the engine's
const imgName = (ref) => shortImage((ref || "").split("/").pop());
const pinned = (img) => {
  if (!img.digest) return img.image;
  let r = img.image;
  const at = r.indexOf("@"), c = r.lastIndexOf(":");
  if (at >= 0) r = r.slice(0, at);
  else if (c > r.lastIndexOf("/")) r = r.slice(0, c);
  return r + "@" + img.digest;
};
const commitLine = (sha, subjects) => h("span", { class: "cline", title: sha || null }, icon("commit"), mono(short(sha, 8) || "?"), subjects && subjects[sha] ? h("span", { class: "subject" }, subjects[sha]) : null);
const previewsOf = (st, path) => (st.previews || []).filter((x) => x.project === path);

// project health for its dot: grey disabled, red failing/invalid, amber pending/partial, green all running
function health(p) {
  if (p.disabled) return ["idle", "disabled"];
  if (p.error) return ["bad", "invalid compose"];
  const cs = p.services.flatMap((s) => s.containers);
  if (cs.some((c) => ["exited", "dead", "stopped"].includes(stateOf(c)))) return ["bad", "some containers are not running"];
  if (p.gone) return ["warn", "removed from git"];
  if (p.services.some((s) => s.pending) || cs.some((c) => ["created", "paused", "restarting"].includes(stateOf(c)))) return ["warn", "pending changes"];
  if (!cs.length) return ["idle", "no containers"];
  return ["ok", "all running"];
}
const healthDot = (p, cls = "") => { const [k, why] = health(p); return h("span", { class: "dot " + k + " " + cls, title: why }); };
const projectTags = (p) => [
  p.disabled ? h("span", { class: "tag" }, "disabled") : null,
  p.gone ? h("span", { class: "tag bad" }, "removed from git") : null,
  p.error ? h("span", { class: "tag bad", title: p.error }, "invalid compose") : null,
];

// nest projects by path (shop/api goes under shop) and flatten to rows with a depth
function treeRows(projects) {
  const root = { children: {} };
  for (const p of projects) {
    let node = root;
    for (const part of p.path.split("/")) node = node.children[part] ??= { children: {}, project: null };
    node.project = p;
  }
  const rows = [];
  const walk = (node, depth) => {
    for (const [name, child] of Object.entries(node.children).sort(([a], [b]) => a.localeCompare(b))) {
      rows.push({ name, depth, project: child.project });
      walk(child, depth + 1);
    }
  };
  walk(root, 0);
  return rows;
}

// ---- components

function head({ title, crumbs, sub, actions, dot }) {
  return h("div", { class: "page-head" },
    h("div", { class: "titles" }, h("h1", {}, dot, crumbs ? h("span", { class: "crumb" }, crumbs) : null, title), sub ? h("div", { class: "sub" }, sub) : null),
    actions ? h("div", { class: "actions" }, actions) : null);
}
// put replaces el's children, skipping null/false (replaceChildren would print them)
const put = (el, ...children) => el.replaceChildren(...children.flat(Infinity).filter((x) => x !== null && x !== undefined && x !== false));
const page = (...children) => put($app, ...children);
const panel = (...c) => h("div", { class: "panel" }, ...c);
const panelHead = (...c) => h("div", { class: "panel-head" }, ...c);
const empty = (title, hint, action) => h("div", { class: "empty" }, h("strong", {}, title), hint ? h("div", { class: "small" }, hint) : null, action ? h("div", { style: "margin-top:8px" }, action) : null);
const loading = (text = "Loading…", cls = "") => h("div", { class: "loading " + cls }, h("span", { class: "spin" }), text);
function table(headers, rows, cls) {
  return h("div", { class: "table-wrap" }, h("table", { class: cls }, h("thead", {}, h("tr", {}, headers.map((x) => h("th", {}, x)))), h("tbody", {}, rows)));
}
function alertBox(kind, title, body, action) {
  return h("div", { class: "alert " + kind, role: kind === "info" ? null : "alert" }, icon(kind === "info" ? "info" : "alert"),
    h("div", { class: "body" }, title ? h("strong", {}, title) : null, body), action || null);
}
const errorBox = (e) => alertBox("bad", "Something went wrong", h("span", { class: "small wrapany" }, e.message));

// busy runs fn with the button disabled and spinning; errors become toasts.
async function busy(btn, fn) {
  btn.disabled = true;
  btn.classList.add("busy");
  try { return await fn(); } catch (x) { if (x.message !== "login required") toast(x.message); } finally {
    btn.disabled = false;
    btn.classList.remove("busy");
  }
}

// confirmDialog replaces confirm(): states the consequences, resolves true on confirm. Cancel has focus, Escape cancels.
function confirmDialog({ title, body, ok = "Confirm", danger = false }) {
  return new Promise((resolve) => {
    const d = h("dialog", { class: "modal" }, h("form", { method: "dialog" },
      h("div", { class: "modal-head" }, h("h3", {}, title)),
      h("div", { class: "modal-body" }, [body].flat().map((x) => (typeof x === "string" ? h("p", {}, x) : x))),
      h("div", { class: "modal-foot" }, h("button", { class: "btn", value: "cancel" }, "Cancel"), h("button", { class: "btn " + (danger ? "danger solid" : "primary"), value: "ok" }, ok))));
    d.addEventListener("close", () => { resolve(d.returnValue === "ok"); d.remove(); });
    document.body.append(d);
    d.showModal();
  });
}

// streamDialog runs a streamed POST (rollback, preview up) in a modal that shows its progress; done(ok) runs at the end.
function streamDialog({ title, intro, path, body, done }) {
  const out = h("pre", { class: "out" });
  const result = h("span", { class: "row" }, h("span", { class: "spin" }), h("span", { class: "muted small" }, "Running…"));
  let running = true;
  const d = h("dialog", { class: "modal wide" }, h("div", { class: "inner" },
    h("div", { class: "modal-head" }, h("h3", {}, title)),
    h("div", { class: "modal-body" }, intro ? h("p", {}, intro) : null, out),
    h("div", { class: "modal-foot" }, result, h("span", { class: "spacer" }), h("button", { class: "btn", onclick: () => d.close() }, "Close"))));
  d.addEventListener("close", () => {
    if (running) toast("It keeps running on the host");
    d.remove();
  });
  document.body.append(d);
  d.showModal();
  const finish = (ok) => {
    running = false;
    result.replaceChildren(h("span", { class: "tag " + (ok ? "ok" : "bad") }, ok ? "Done" : "Failed"));
    done && done(ok);
  };
  stream(path, preSink(out), { method: "POST", body: JSON.stringify(body),
    done: () => finish(/==> ok\s*$/.test(out.textContent)),
    error: (msg) => { out.append("\n" + msg); finish(false); } });
}

// the plan/apply flow, reachable from anywhere through the "pending changes" badge
async function openPlan() {
  const st = status || {};
  const out = h("pre", { class: "out" }, "Loading plan…");
  const result = h("span", {});
  let running = false;
  const apply = h("button", { class: "btn primary", disabled: true, onclick: () => {
    running = true;
    apply.disabled = true;
    apply.classList.add("busy");
    out.textContent = "";
    const finish = (ok) => {
      running = false;
      apply.classList.remove("busy");
      apply.hidden = true;
      result.replaceChildren(h("span", { class: "tag " + (ok ? "ok" : "bad") }, ok ? "Applied" : "Apply failed"));
      toast(ok ? "Applied" : "Apply failed");
      refreshStatus().catch(() => {});
    };
    stream("/apply", preSink(out), { method: "POST", body: JSON.stringify({ commit: st.commit, trigger: "ui" }),
      done: () => finish(/==> ok\s*$/.test(out.textContent)),
      error: (msg) => { out.append("\n" + msg); finish(false); } });
  } }, "Apply");
  const d = h("dialog", { class: "modal wide" }, h("div", { class: "inner" },
    h("div", { class: "modal-head" }, h("h3", {}, "Pending changes")),
    h("div", { class: "modal-body" },
      h("p", {}, "What runs differs from ~/vops at ", h("span", { class: "mono" }, short(st.commit, 8) || "?"), " (a push, env vars or enable/disable). Applying deploys it."),
      out),
    h("div", { class: "modal-foot" }, result, h("span", { class: "spacer" }), h("button", { class: "btn", onclick: () => d.close() }, "Close"), apply)));
  d.addEventListener("close", () => {
    if (running) toast("Apply keeps running on the host");
    d.remove();
  });
  document.body.append(d);
  d.showModal();
  try {
    out.textContent = await apiText("/plan?format=text");
    apply.disabled = false;
  } catch (e) { out.textContent = e.message; }
}

// ---- status: polled for the sidebar and top bar; pages subscribe through onStatus

let status = null, statusAt = 0, statusReq = null, onStatus = null;
function refreshStatus() {
  statusReq ??= api("GET", "/status").then((st) => {
    status = st;
    statusAt = Date.now();
    renderChrome(st);
    onStatus && onStatus(st);
    return st;
  }).finally(() => { statusReq = null; });
  return statusReq;
}
const getStatus = (maxAge = 4000) => (status && Date.now() - statusAt < maxAge ? Promise.resolve(status) : refreshStatus());
setInterval(() => { if (!document.hidden && !$shell.hidden) refreshStatus().catch(() => {}); }, 10000);
document.addEventListener("visibilitychange", () => { if (!document.hidden && !$shell.hidden && Date.now() - statusAt > 10000) refreshStatus().catch(() => {}); });

// ---- chrome: sidebar, top bar

const NAV = [["#/", "Overview", "grid"], ["#/files", "Files", "file"], ["#/registry", "Registry", "box"], ["#/users", "Users", "user"], ["#/events", "Events", "activity"]];
$nav.append(...NAV.map(([href, label, ic]) => h("a", { href }, icon(ic), label)));
$("logout").append(icon("logout"), "Log out");
$("logout").onclick = async () => {
  await api("POST", "/logout").catch(() => {});
  showLogin();
};
$("menu").append(icon("menu"));
$("menu").onclick = () => document.body.classList.toggle("nav-open");
$("scrim").onclick = () => document.body.classList.remove("nav-open");
document.addEventListener("keydown", (e) => { if (e.key === "Escape") document.body.classList.remove("nav-open"); });
$pending.append(h("span", { class: "dot warn" }), h("span", {}, "Pending", h("span", { class: "long" }, " changes")));
$pending.onclick = openPlan;

let sideSig = "";
function renderChrome(st) {
  $hoststat.replaceChildren(
    h("span", { title: "~/vops on the host is at this commit" }, icon("commit"), h("span", { class: "mono" }, short(st.commit, 8) || "no commits")),
    h("span", { class: "dom", title: "domain (vops.yml)" }, icon("globe"), st.domain || "no domain"));
  $pending.hidden = !st.changes;
  const sig = JSON.stringify([st.projects.map((p) => [p.path, health(p)]), st.previews]);
  if (sig !== sideSig) {
    sideSig = sig;
    $sideProjects.replaceChildren(...(st.projects.length ? treeRows(st.projects).flatMap((r) => r.project
      ? [h("a", { class: "sp", href: "#/p/" + enc(r.project.path) + "/services", "data-path": r.project.path, style: "--d:" + r.depth, title: r.project.path }, healthDot(r.project), h("span", { class: "name" }, r.name)),
        ...previewsOf(st, r.project.path).map((x) => h("a", { class: "sp pv", href: projectHref(x.project, "previews"), style: "--d:" + (r.depth + 1), title: "preview " + x.name + " of " + x.project }, icon("git"), h("span", { class: "name" }, x.name)))]
      : [h("div", { class: "sp folder", style: "--d:" + r.depth }, r.name + "/")]) : [h("div", { class: "side-empty" }, "No projects yet")]));
  }
  markActive();
}

function currentProject(path) {
  const m = path.match(PROJECT_RE) || path.match(/^\/logs\/(.+)$/);
  return m ? dec(m[1]).split("@")[0] : null;
}
function markActive() {
  const path = location.hash.slice(1).split("?")[0] || "/";
  for (const a of $nav.querySelectorAll("a")) {
    const t = a.getAttribute("href").slice(1);
    a.classList.toggle("active", t === "/" ? path === "/" : path.startsWith(t));
  }
  const cur = currentProject(path);
  for (const a of $sideProjects.querySelectorAll("a.sp")) a.classList.toggle("active", a.dataset.path === cur);
}

// ---- login

function showLogin() {
  leave();
  routeGen++;
  status = null;
  sideSig = "";
  $shell.hidden = true;
  document.body.classList.remove("nav-open");
  if ($login.firstChild) return;
  const pw = h("input", { type: "password", placeholder: "Password", autocomplete: "current-password", "aria-label": "password" });
  const err = h("div", { class: "error small center" });
  const btn = h("button", { class: "btn primary" }, "Log in");
  $login.replaceChildren(h("div", { class: "login-wrap" }, h("form", { class: "panel login", onsubmit: async (e) => {
    e.preventDefault();
    err.textContent = "";
    await busy(btn, async () => {
      try {
        await api("POST", "/login", { password: pw.value });
        $login.replaceChildren();
        route();
      } catch (x) { err.textContent = x.message; }
    });
  } },
    h("span", { class: "logo" }, "v"),
    h("h1", {}, "vops"),
    h("p", { class: "muted small center" }, "Log in as admin"),
    pw, btn, err)));
  pw.focus();
}

// ---- overview

function stats(st) {
  const all = st.projects.flatMap((p) => p.services.flatMap((s) => s.containers));
  const running = all.filter((c) => c.State === "running").length;
  const done = all.filter((c) => stateOf(c) === "done").length;
  const down = all.length - running - done;
  const disabled = st.projects.filter((p) => p.disabled).length;
  const invalid = st.projects.filter((p) => p.error).length;
  const pending = st.projects.reduce((n, p) => n + p.services.filter((s) => s.pending).length, 0);
  const last = st.projects.reduce((a, p) => (p.applied_at > (a ? a.applied_at : 0) ? p : a), null);
  const card = (label, value, foot, cls = "", title) => h("div", { class: "panel stat " + cls, title }, h("div", { class: "label" }, label), h("div", { class: "value" }, value), h("div", { class: "foot" }, foot));
  return h("div", { class: "stats" },
    card("Projects", st.projects.length, [disabled ? disabled + " disabled" : "", disabled && invalid ? " · " : "", invalid ? h("span", { class: "error" }, invalid + " invalid") : "", !disabled && !invalid ? "all enabled" : ""]),
    card("Containers running", [running, h("span", { class: "of" }, " / " + all.length)], down ? down + " not running" : done ? done + " finished job" + (done > 1 ? "s" : "") : "none down", down ? "bad" : ""),
    card("Pending changes", st.changes ? pending || "yes" : 0, st.changes ? h("button", { class: "linkbtn", onclick: openPlan }, "Review and apply") : "in sync with git", st.changes ? "warn" : ""),
    card("Last deploy", last ? ago(last.applied_at) : "never", last ? h("a", { href: projectHref(last.path, "timeline") }, last.path) : "", "", last ? fullDate(last.applied_at) : null),
    card("Previews", (st.previews || []).length, (st.previews || []).length ? [...new Set(st.previews.map((x) => x.project))].map((p, i) => [i ? ", " : "", h("a", { href: projectHref(p, "previews") }, p)]) : "none running"));
}

function dots(containers) {
  return h("span", { class: "dots" }, containers.length ? containers.map((c) => h("span", { class: "dot " + stateOf(c), title: c.Names[0] + ": " + c.Status })) : h("span", { class: "dot", title: "no containers" }));
}

function projectList(st) {
  if (!st.projects.length) return panel(empty("No projects yet", "Add a folder with a compose.yml to the repo and run vops sync."));
  const rows = [];
  for (const r of treeRows(st.projects)) {
    const p = r.project;
    if (!p) {
      rows.push(h("div", { class: "prow folder", style: "--d:" + r.depth }, icon("folder"), r.name + "/"));
      continue;
    }
    rows.push(h("div", { class: "prow" + (p.services.length ? " has-svc" : ""), style: "--d:" + r.depth },
      healthDot(p), h("a", { class: "pname", href: "#/p/" + enc(p.path) + "/services" }, r.name), projectTags(p),
      previewsOf(st, p.path).length ? h("a", { class: "tag accent", href: projectHref(p.path, "previews"), title: "previews" }, icon("git"), previewsOf(st, p.path).length) : null,
      h("span", { class: "meta" }, p.commit ? ["applied ", mono(short(p.commit, 8)), " · ", when(p.applied_at)] : "never applied")));
    p.services.forEach((s, i) => rows.push(h("div", { class: "srow" + (i === p.services.length - 1 ? " last" : ""), style: "--d:" + r.depth },
      dots(s.containers),
      h("span", { class: "mono sname" }, s.name),
      h("span", { class: "doms small" }, s.domains.slice(0, 2).map((d) => h("a", { href: domainURL(st, d), target: "_blank", rel: "noopener" }, d))),
      s.pending ? h("span", { class: "tag warn" }, "pending " + s.pending) : null)));
  }
  return panel(panelHead(h("h2", {}, "Projects"), h("span", { class: "tag" }, st.projects.length)), h("div", { class: "plist" }, rows));
}

async function overview(alive) {
  let st;
  try { st = await getStatus(); } catch (e) { return failed(e, alive); }
  if (!alive()) return;
  const render = (st) => page(
    head({ title: "Overview", sub: [h("span", {}, "~/vops at ", mono(short(st.commit, 8) || "no commits yet")), sep(), st.domain || "no domain set in vops.yml"] }),
    st.warnings && st.warnings.length ? alertBox("warn", st.warnings.length === 1 ? "Warning" : st.warnings.length + " warnings", h("ul", { class: "small" }, st.warnings.map((w) => h("li", {}, w)))) : null,
    st.changes ? alertBox("info", "The host differs from git", h("span", { class: "small muted" }, "Changes are waiting to be applied (a push, env vars or enable/disable)."), h("button", { class: "btn primary sm", onclick: openPlan }, "Review and apply")) : null,
    stats(st),
    projectList(st));
  render(st);
  onStatus = render;
}

// ---- one project: header + tabs. Add a tab by adding an entry here.

const TABS = [
  { id: "services", label: "Services", live: true, render: servicesTab },
  { id: "timeline", label: "Timeline", render: timelineTab },
  { id: "previews", label: "Previews", render: previewsTab, count: (ctx) => previewsOf(ctx.st, ctx.path).length },
  { id: "logs", label: "Logs", fill: true, render: logsTab },
  { id: "env", label: "Environment", render: envTab },
  { id: "events", label: "Events", render: eventsTab },
];
const TAB_ALIASES = { data: "timeline" }; // old links
const PROJECT_RE = new RegExp("^/p/(.+?)(?:/(" + [...TABS.map((t) => t.id), ...Object.keys(TAB_ALIASES)].join("|") + "))?/?$");
const projectHref = (path, tab = "services", q = "") => "#/p/" + enc(path) + "/" + tab + q;

async function projectPage(alive, path, tabId, params) {
  let st;
  try { st = await getStatus(); } catch (e) { return failed(e, alive); }
  if (!alive()) return;
  const find = (st) => st.projects.find((x) => x.path === path);
  if (!find(st)) return page(head({ title: path }), panel(empty("Unknown project", "Nothing at this path in ~/vops. It may have been removed from git.", h("a", { class: "btn", href: "#/" }, "Back to overview"))));
  const tab = TABS.find((t) => t.id === (TAB_ALIASES[tabId] || tabId)) || TABS[0];
  const ctx = { path, p: find(st), st, params, reload: null };
  const headBox = h("div", {}), alerts = h("div", {}), body = h("div", { class: tab.fill ? "grow" : "" }), tabs = h("nav", { class: "tabs" });
  const renderHead = () => {
    headBox.replaceChildren(projectHead(ctx));
    put(alerts, ctx.p.error ? alertBox("bad", "Invalid compose file", h("pre", {}, ctx.p.error)) : null, restoredBanner(ctx));
    tabs.replaceChildren(...TABS.map((t) => {
      const n = t.id === "services" ? ctx.p.services.length : t.count ? t.count(ctx) : null;
      return h("a", { href: projectHref(path, t.id), class: t === tab ? "active" : null }, t.label, n ? h("span", { class: "count" }, n) : null);
    }));
  };
  $app.className = tab.fill ? "fill" : "";
  page(headBox, alerts, tabs, body);
  renderHead();
  tab.render(ctx, body);
  onStatus = (st) => {
    const p = find(st);
    if (!p) return;
    Object.assign(ctx, { p, st });
    renderHead();
    if (tab.live) tab.render(ctx, body);
  };
}

function projectHead(ctx) {
  const { p, path } = ctx;
  const parts = path.split("/");
  const name = parts.pop();
  const toggle = h("button", { class: "btn" + (p.disabled ? " primary" : ""), onclick: async () => {
    if (!p.disabled && !(await confirmDialog({ title: `Disable ${path}?`, ok: "Disable", danger: true,
      body: ["On the next apply its containers stop. Volumes, data and env vars stay, and you can enable it again anytime."] }))) return;
    await busy(toggle, async () => {
      await api("POST", "/project", { project: path, disabled: !p.disabled });
      toast(p.disabled ? "Enabled: apply to start it" : "Disabled: apply to stop it");
      await refreshStatus();
    });
  } }, p.disabled ? "Enable" : "Disable");
  return head({ title: name, crumbs: parts.length ? parts.join(" / ") + " / " : null, dot: healthDot(p, "lg"),
    sub: [p.commit ? ["applied ", mono(short(p.commit, 8)), " · ", when(p.applied_at)] : "never applied", projectTags(p)],
    actions: [h("a", { class: "btn", href: "#/files?dir=" + enc(path) }, icon("file"), "Files"), toggle] });
}

function servicesTab(ctx, box) {
  const { p, path, st } = ctx;
  if (!p.services.length) return box.replaceChildren(panel(empty("No services", p.error ? "Fix the compose file first." : "This project defines no services.")));
  box.replaceChildren(panel(table(["Service", "Containers", "Image", ""], p.services.map((s) => {
    const restart = s.containers.length ? h("button", { class: "btn sm", onclick: () => busy(restart, async () => {
      await api("POST", "/restart", { project: path, service: s.name });
      toast("Restarted " + s.name);
      await refreshStatus();
    }) }, "Restart") : null;
    return h("tr", {},
      h("td", {}, h("div", { class: "mono" }, s.name),
        s.domains.map((d) => h("div", { class: "small wrapany" }, h("a", { href: domainURL(st, d), target: "_blank", rel: "noopener" }, d))),
        s.pending ? h("span", { class: "tag warn", style: "margin-top:4px" }, "pending " + s.pending + (s.reason ? ": " + s.reason : "")) : null),
      h("td", {}, s.containers.length ? s.containers.map((c) => h("div", { class: "ctr small" }, h("span", { class: "dot " + stateOf(c) }), h("span", { class: "mono" }, c.Names[0]), h("span", { class: "muted" }, c.Status))) : h("span", { class: "muted small" }, "none")),
      h("td", { class: "mono small wrap", title: s.image }, shortImage(s.image)),
      h("td", { class: "actions-cell" }, h("div", { class: "actions" }, h("a", { class: "btn sm", href: projectHref(path, "logs", "?service=" + enc(s.name)) }, "Logs"), restart)));
  }), "top")));
}

function logsTab(ctx, box) {
  const service = ctx.params.get("service");
  const lv = logView({ path: ctx.path, service, services: ctx.p.services.map((s) => s.name),
    hrefFor: (svc) => projectHref(ctx.path, "logs", svc ? "?service=" + enc(svc) : ""),
    fullHref: "#/logs/" + enc(ctx.path) + (service ? "?service=" + enc(service) : "") });
  box.replaceChildren(lv.el);
  lv.start();
  onLeave(lv.stop);
}

// env vars, write-only. Scope: production, or the previews' secrets (previews never get production's env).
function envTab(ctx, box) {
  const { path } = ctx;
  const preview = ctx.params.get("scope") === "previews";
  const scope = preview ? "&preview=1" : "";
  const key = h("input", { placeholder: "KEY", pattern: "[A-Za-z_][A-Za-z0-9_]*", required: true, class: "mono", autocomplete: "off" });
  const val = h("input", { placeholder: "value", type: "password", autocomplete: "new-password" });
  const setBtn = h("button", { class: "btn primary" }, "Set");
  const form = h("form", { class: "inline", onsubmit: (e) => {
    e.preventDefault();
    busy(setBtn, async () => {
      await api("POST", "/env", { project: path, key: key.value, value: val.value, preview });
      toast(key.value + (preview ? " set. Previews get it on their next update." : " set. Apply to deploy it."));
      key.value = val.value = "";
      load();
      refreshStatus().catch(() => {});
    });
  } }, h("label", { class: "field" }, "Name", key), h("label", { class: "field" }, "Value (write-only)", val), setBtn);
  const prefill = (k) => {
    key.value = k;
    form.scrollIntoView({ block: "center", behavior: "smooth" });
    val.focus({ preventScroll: true });
  };
  const list = h("div", {}, loading("Loading…", "pad"));
  const services = h("div", {});
  const load = async () => {
    let envs, usage = null, uerr = null;
    try {
      [envs, usage] = await Promise.all([api("GET", "/env?project=" + enc(path) + scope), api("GET", "/env/usage?project=" + enc(path) + scope).catch((e) => { uerr = e; return null; })]);
    } catch (e) { return list.replaceChildren(errorBox(e)); }
    const unused = new Set(usage ? usage.unused : []);
    const svcs = usage ? Object.keys(usage.services).sort() : [];
    const composeOnly = svcs.some((s) => usage.services[s].length);
    list.replaceChildren(envs.length ? table(["Name", "Value", ""], envs.map((e) => h("tr", {},
      h("td", { class: "mono wrapany" }, e.key, unused.has(e.key) ? [" ", h("span", { class: "tag", title: "No service uses it: not in any environment: [" + e.key + "] or ${" + e.key + "}" }, "unused")] : null),
      h("td", { class: "muted small" }, "••••••••  set ", when(e.updated_at)),
      h("td", { class: "actions-cell" }, h("button", { class: "btn sm danger", onclick: async () => {
        if (!(await confirmDialog({ title: `Remove ${e.key}?`, ok: "Remove", danger: true,
          body: [h("p", {}, preview ? "Previews of " : "Services of ", h("strong", {}, path), preview ? " lose it on their next update." : " that use it lose it on the next apply."), "Values are write-only: to restore it you have to set it again."] }))) return;
        try {
          await api("DELETE", "/env?project=" + enc(path) + "&key=" + enc(e.key) + scope);
          toast(e.key + " removed");
          load();
          refreshStatus().catch(() => {});
        } catch (x) { toast(x.message); }
      } }, "Remove")))))
      : h("div", { class: "panel-body muted small" }, preview ? "No preview secrets set. " : "No secrets set with vops env. ",
        composeOnly ? "The services still get the variables listed below (from compose and env files)." : preview ? "Previews get preview.env, then these on top." : "Set one below and use it with environment: [KEY] or ${KEY} in compose.yml."));
    // per service: every variable the containers get, and from where (never values)
    const hint = (en) => {
      if (en.source === "missing") {
        const k = en.vars && en.vars.length ? en.vars[0] : en.key;
        return [h("span", { class: "warn-text" }, en.vars && en.vars.length ? "${" + k + "} is set nowhere: empty" : "set nowhere: empty"), " ", h("button", { class: "btn sm", onclick: () => prefill(k) }, "Set it")];
      }
      if (en.secret) return h("span", { class: "warn-text", title: `Replace the literal with environment: [${en.key}] in compose.yml and set the value with vops env set (or the form above).` }, "committed to git: move to vops env");
      if (en.file === "builtin") return "set by vops";
      if (en.file === "preview.env") return "from preview.env in the repo";
      if (en.vars && en.vars.length) return "via " + en.vars.map((v) => "${" + v + "}").join(", ");
      return "";
    };
    const src = (en) => h("span", { class: "tag" + (en.source === "missing" ? " warn" : en.source === "vops" ? " accent" : "") }, en.source, en.file && en.file !== "builtin" ? " · " + en.file : "");
    put(services, uerr ? alertBox("warn", "Can't read the compose files", h("span", { class: "small wrapany" }, uerr.message)) : null,
      usage ? panel(panelHead(h("h2", {}, "What each service gets"), h("span", { class: "muted small" }, preview ? "in previews" : "names and sources only, never values")),
        svcs.length ? table(["Variable", "Source", ""], svcs.flatMap((svc) => [
          h("tr", { class: "grp" }, h("td", { colspan: 3 }, h("span", { class: "mono" }, svc), usage.services[svc].length ? null : h("span", { class: "muted small" }, "  no variables"))),
          ...usage.services[svc].map((en) => h("tr", { class: en.source === "missing" ? "warnrow" : null },
            h("td", { class: "mono wrapany" }, en.key), h("td", { class: "nowrap" }, src(en)), h("td", { class: "small" }, hint(en))))]), "envuse")
          : empty("No services", "This project defines no services.")) : null);
  };
  const seg = h("div", { class: "seg", role: "tablist" },
    h("a", { href: projectHref(path, "env"), class: preview ? null : "active" }, "Production"),
    h("a", { href: projectHref(path, "env", "?scope=previews"), class: preview ? "active" : null }, "Previews"));
  box.replaceChildren(h("div", { class: "stack" }, panel(panelHead(h("h2", {}, preview ? "Preview secrets" : "vops env"), seg, h("span", { class: "muted small" }, "write-only: values are never shown again")),
    h("div", { class: "panel-note small muted" }, preview
      ? ["Previews don't inherit production's env: they read ", mono("preview.env"), " from ", mono(path + "/"), " in the repo, then these secrets on top."]
      : ["Used as ", mono("${KEY}"), " in compose.yml or with ", mono("environment: [KEY]"), ". Changes deploy on the next apply."]),
    list, h("div", { class: "panel-foot" }, form)), services));
  load();
}

// ---- data: restore and branch. The history of a project is a list of nodes (deploys, rollbacks, snapshots);
// a node's snapshot is always the data right before it happened, so "restore" and "preview from here" start there.

// after a restore, until the next deploy: "Data restored to #N · when · Undo"
function restoredBanner(ctx) {
  const d = ctx.p.last_deploy;
  if (!d || d.trigger !== "rollback" || d.result !== "ok" || d.undoes) return null;
  const undo = h("button", { class: "btn sm", onclick: () => restoreSnapshot(ctx, { id: d.snapshot_id, commit: d.commit, created_at: d.started_at }, "the data from right before this restore") }, icon("undo"), "Undo");
  return alertBox("warn", `Data restored to snapshot #${d.restored_id}`,
    h("span", { class: "small" }, when(d.finished_at), " · code was not rolled back · shown until the next deploy of this project"), d.snapshot_id ? undo : null);
}

// restoreSnapshot asks (stating what happens to containers, data and code), then streams the rollback
async function restoreSnapshot(ctx, snap, what) {
  const { path } = ctx;
  const now = ctx.p.commit;
  const code = snap.commit && now && snap.commit !== now
    ? [h("p", {}, "Code is not rolled back: this data belongs to ", mono(short(snap.commit, 8)), ", while ", h("strong", {}, path), " runs ", mono(short(now, 8)), ". To run the matching code, revert it in git and sync:"),
      h("pre", { class: "cmd" }, `git restore --source=${short(snap.commit)} --staged --worktree -- ${path}/\ngit commit -m "${path}: code back to ${short(snap.commit, 8)}"\nvops sync`)]
    : h("p", {}, "Code is not touched", snap.commit ? ["; this data belongs to the commit running now (", mono(short(snap.commit, 8)), ")."] : ".");
  if (!(await confirmDialog({ title: `Restore ${path} data to #${snap.id}?`, ok: "Restore data", danger: true, body: [
    h("p", {}, "Puts back ", h("strong", {}, what), snap.created_at ? [" (", what.includes("#" + snap.id) ? "" : "snapshot #" + snap.id + ", ", "taken ", when(snap.created_at), ")"] : "", "."),
    "Its containers stop for a few seconds while the data is put back, then start again.",
    "The current data is snapshotted first, so this can be undone.",
    code] }))) return;
  streamDialog({ title: `Restoring ${path} to #${snap.id}`, path: "/rollback", body: { project: path, id: snap.id },
    done: (ok) => { if (ok) toast("Data restored"); ctx.reload && ctx.reload(); refreshStatus().catch(() => {}); } });
}

const nameRe = "[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?";
const dataWarning = () => alertBox("warn", "Previews hold a copy of production data",
  h("span", { class: "small" }, "Real personal data, and whatever the code does with it: queued emails, webhooks, scheduled charges. Previews never get production's env (Environment → Previews), but anything they don't redirect still goes out for real."));

// previewUp streams a preview creation from a body the dialogs built
function previewUp(ctx, body) {
  streamDialog({ title: `Preview ${body.name} of ${ctx.path}`, path: "/previews", body: { project: ctx.path, ...body },
    done: (ok) => {
      if (ok) toast("Preview " + body.name + " is up");
      refreshStatus().catch(() => {});
      ctx.reload && ctx.reload();
    } });
}

// previewFrom: "Preview from here" on a timeline node: its commit, its images (by digest), its snapshot
function previewFrom(ctx, n, t) {
  const dep = n.deploy, s = n.snapshot;
  const commit = dep ? dep.commit : s.commit;
  const images = {};
  for (const [svc, img] of Object.entries(n.images || {})) if (!img.built) images[svc] = pinned(img);
  const name = h("input", { value: dep ? "at-" + dep.id : "snap-" + s.id, required: true, pattern: nameRe, class: "mono", "aria-label": "name", maxlength: 32 });
  const d = h("dialog", { class: "modal" }, h("form", { onsubmit: (e) => {
    e.preventDefault();
    d.close();
    previewUp(ctx, { name: name.value, ref: commit, images, from: s ? s.id : 0, deploy: dep ? dep.id : 0 });
  } },
    h("div", { class: "modal-head" }, h("h3", {}, dep ? `Preview from #${dep.id}` : `Preview from snapshot #${s.id}`)),
    h("div", { class: "modal-body" },
      h("label", { class: "field" }, "Name (its urls are <service>.<name>." + ctx.path.split("/").reverse().join(".") + ".…)", name),
      h("dl", { class: "kv small" },
        h("dt", {}, "Code"), h("dd", {}, commit ? commitLine(commit, t.subjects) : "the applied commit"),
        h("dt", {}, "Images"), h("dd", {}, Object.keys(images).length ? Object.entries(n.images).filter(([, img]) => !img.built).map(([svc, img]) => h("div", { class: "mono wrapany", title: images[svc] }, svc + ": " + imgName(img.image), img.digest ? h("span", { class: "faint" }, " @" + short(img.digest, 7)) : null)) : "as in that commit"),
        h("dt", {}, "Data"), h("dd", {}, s ? ["a copy of snapshot #" + s.id + " (", n.kind === "snapshot" ? when(s.created_at) : "right before this " + (n.kind === "rollback" ? "restore" : "deploy"), ")", n.kind === "deploy" ? "; its migrations run again on the copy" : ""] : h("span", { class: "warn-text" }, "this point has no snapshot: the preview gets a copy of the current data"))),
      h("p", { class: "small" }, "The data is a copy of production's: personal data, and whatever the code does with it.")),
    h("div", { class: "modal-foot" }, h("button", { type: "button", class: "btn", onclick: () => d.close() }, "Cancel"), h("button", { class: "btn primary" }, "Create preview"))));
  d.addEventListener("close", () => d.remove());
  document.body.append(d);
  d.showModal();
  name.select();
}

function changeLine(c) {
  const img = (x) => h("span", { title: x.image + (x.digest ? "@" + x.digest : "") }, imgName(x.image));
  let what;
  if (!c.from) what = ["+ ", img(c.to)];
  else if (!c.to) what = h("span", { class: "faint" }, "removed");
  else if (c.from.image === c.to.image) what = [img(c.to), " ", h("span", { class: "faint" }, "@" + short(c.from.digest, 7)), " → @" + short(c.to.digest, 7)];
  else what = [img(c.from), " → ", img(c.to)];
  return h("div", { class: "chg mono small wrapany" }, h("span", { class: "svc" }, c.service), " ", what);
}

function timelineTab(ctx, box) {
  const { path } = ctx;
  box.replaceChildren(loading());
  const load = async () => {
    let t;
    try { t = await api("GET", "/timeline?project=" + enc(path)); } catch (e) { return box.replaceChildren(errorBox(e)); }
    const d = t.data;
    const snapBadge = (n) => {
      const s = n.snapshot;
      if (!s) return n.kind === "snapshot" ? null : h("span", { class: "tag faint", title: "No data to snapshot then, snapshots off, or pruned (snapshot_keep)" }, "no snapshot");
      const why = n.kind === "deploy" ? "the data right before this deploy" : n.kind === "rollback" ? "the data right before this restore" : s.reason + " snapshot";
      return h("span", { class: "tag", title: `snapshot #${s.id}: ${why} (${fullDate(s.created_at)})` }, icon("camera"), "#" + s.id);
    };
    // "put back snapshot #40: the data from right before deploy #12"
    const restoredFrom = (id) => {
      const o = t.nodes.find((x) => x.snapshot && x.snapshot.id === id);
      if (!o || o.kind === "snapshot") return o && o.snapshot.note ? " (“" + o.snapshot.note + "”)" : "";
      return ": the data from right before " + (o.kind === "deploy" ? "deploy #" : "restore #") + o.deploy.id;
    };
    const chips = (n) => n.previews.map((name) => h("a", { class: "tag accent", href: projectHref(path, "previews"), title: "preview " + name + " branched from here" }, icon("git"), name));
    const node = (n) => {
      const dep = n.deploy, s = n.snapshot;
      let title, cls = n.kind;
      if (n.kind === "deploy") {
        title = ["Deploy ", h("span", { class: "faint" }, "#" + dep.id)];
        if (dep.result === "failed") cls += " failed";
      } else if (n.kind === "rollback") {
        title = dep.undoes ? ["Undo of ", h("span", { class: "faint" }, "#" + dep.undoes)] : ["Data restored ", h("span", { class: "faint" }, "#" + dep.id)];
        if (dep.result === "failed") cls += " failed";
      } else title = [s.reason === "manual" ? "Snapshot" : s.reason + " snapshot", " ", h("span", { class: "faint" }, "#" + s.id)];
      const restore = s ? h("button", { class: "btn sm", title: n.kind === "rollback" ? "Put back the data from right before this restore" : "Put back " + (n.kind === "deploy" ? "the data from right before this deploy" : "this snapshot"),
        onclick: () => restoreSnapshot(ctx, s, n.kind === "deploy" ? `the data from right before deploy #${dep.id}` : n.kind === "rollback" ? "the data from right before this restore" : `snapshot #${s.id}`) },
        n.kind === "rollback" ? [icon("undo"), "Undo"] : "Restore data") : null;
      const del = n.kind === "snapshot" ? h("button", { class: "btn sm danger", onclick: async () => {
        if (!(await confirmDialog({ title: `Delete snapshot #${s.id}?`, ok: "Delete", danger: true,
          body: [`${s.reason} snapshot taken ${ago(s.created_at)}${s.note ? ` (${s.note})` : ""}. It can't be recovered.`] }))) return;
        try { await api("DELETE", "/snapshots?id=" + s.id); toast("Snapshot #" + s.id + " deleted"); load(); } catch (x) { toast(x.message); }
      } }, "Delete") : null;
      return h("li", { class: "tl-node " + cls },
        h("span", { class: "tl-dot" }),
        h("div", { class: "tl-body" },
          h("div", { class: "tl-head" }, h("strong", {}, title),
            dep && n.kind === "deploy" ? h("span", { class: "tag" + (dep.trigger === "push" ? " accent" : "") }, dep.trigger) : null,
            dep && dep.result === "failed" ? h("span", { class: "tag bad" }, "failed") : null,
            h("span", { class: "spacer" }), h("span", { class: "muted small" }, when(n.at))),
          n.kind === "rollback" ? h("div", { class: "small" }, "put back snapshot #" + dep.restored_id, restoredFrom(dep.restored_id), h("span", { class: "muted" }, " · code unchanged")) : null,
          h("div", { class: "tl-line" }, n.commit ? commitLine(n.commit, t.subjects) : h("span", { class: "faint small" }, "no commit")),
          n.kind === "deploy" && n.changes.length ? h("div", { class: "tl-changes" }, n.changes.map(changeLine)) : null,
          n.kind === "deploy" && !n.changes.length && dep.summary ? h("div", { class: "muted small" }, dep.summary) : null,
          dep && dep.error ? h("div", { class: "small error wrapany" }, dep.error) : null,
          s && n.kind === "snapshot" && s.note ? h("div", { class: "small muted wrapany" }, "“" + s.note + "”") : null,
          h("div", { class: "tl-foot" }, snapBadge(n), chips(n), h("span", { class: "spacer" }),
            h("div", { class: "actions" }, restore,
              h("button", { class: "btn sm", onclick: () => previewFrom(ctx, n, t), title: "A preview of this point: its commit and images" + (s ? ", on a copy of snapshot #" + s.id : ", on a copy of the current data") }, icon("git"), "Preview from here"),
              del))));
    };
    // now: what runs, the data it has, and a manual snapshot
    const p = ctx.p;
    const images = Object.keys(t.images).length ? Object.entries(t.images).map(([svc, img]) => [svc, img.image, img.digest]) : p.services.map((x) => [x.name, x.image, ""]);
    const note = h("input", { placeholder: "note (optional)", "aria-label": "note" });
    const takeBtn = h("button", { class: "btn sm" }, icon("camera"), "Snapshot now");
    const take = d.supported && d.protected.length ? h("form", { class: "row", onsubmit: (e) => {
      e.preventDefault();
      busy(takeBtn, async () => {
        const r = await api("POST", "/snapshots", { project: path, note: note.value });
        toast("Snapshot #" + r.snapshot.id + " taken");
        load();
      });
    } }, note, takeBtn) : null;
    const pending = p.services.some((x) => x.pending);
    const now = h("li", { class: "tl-node now" },
      h("span", { class: "tl-dot" }),
      h("div", { class: "tl-body" },
        h("div", { class: "tl-head" }, h("strong", {}, "Now"), pending ? h("button", { class: "tag warn", onclick: openPlan }, "pending changes") : null,
          h("span", { class: "spacer" }), t.applied_at ? h("span", { class: "muted small" }, "applied ", when(t.applied_at)) : null),
        h("div", { class: "tl-line" }, t.commit ? commitLine(t.commit, t.subjects) : h("span", { class: "faint small" }, "never applied")),
        images.length ? h("div", { class: "tl-imgs" }, images.map(([svc, image, digest]) => h("span", { class: "tag mono", title: svc + ": " + image + (digest ? "@" + digest : "") }, imgName(image).startsWith(svc + ":") ? imgName(image) : [h("span", { class: "faint" }, svc), " " + imgName(image)]))) : null,
        h("div", { class: "tl-foot" },
          h("span", { class: "small muted" }, d.supported ? (d.protected.length ? ["Data: ", mono(d.protected.join(", "))] : "No data to snapshot") : "Snapshots unavailable"),
          h("span", { class: "spacer" }), take)));
    put(box,
      !d.supported ? alertBox("warn", "Snapshots unavailable", h("span", { class: "small" }, d.reason + ". Deploys are still recorded; restoring data and branching from a point need snapshots.")) : null,
      d.unprotected && d.unprotected.length ? alertBox("warn", "Not covered by snapshots", h("span", { class: "small" }, "Not btrfs subvolumes: ", mono(d.unprotected.join(", ")))) : null,
      panel(h("ol", { class: "tl" }, now, t.nodes.map(node)),
        t.nodes.length ? null : h("div", { class: "panel-foot muted small" }, "No history yet: every apply that changes this project is recorded here, with a snapshot of its data taken right before.")));
  };
  ctx.reload = load;
  load();
}

// previews of this project: what runs, where it came from, when it expires
function previewsTab(ctx, box) {
  const { path, st } = ctx;
  const formBox = h("div", { hidden: true });
  const list = h("div", {}, loading("Loading…", "pad"));
  const newBtn = h("button", { class: "btn primary sm", onclick: () => { formBox.hidden = !formBox.hidden; if (!formBox.hidden) openForm(); } }, "New preview");
  box.replaceChildren(h("div", { class: "stack" }, dataWarning(), formBox, panel(panelHead(h("h2", {}, "Previews"), h("span", { class: "spacer" }), newBtn), list)));

  const openForm = async () => {
    const name = h("input", { required: true, pattern: nameRe, class: "mono", placeholder: "pr-42", maxlength: 32 });
    const ref = h("input", { class: "mono", placeholder: "branch, tag or commit" });
    const images = h("textarea", { class: "mono", rows: 2, placeholder: "web=registry.example.com/shop/web:pr-42" });
    const from = h("select", {}, h("option", { value: "0" }, "Copy of the current data"));
    const go = h("button", { class: "btn primary" }, "Create preview");
    api("GET", "/snapshots?project=" + enc(path)).then((r) => from.append(...r.snapshots.map((x) =>
      h("option", { value: x.id }, `Snapshot #${x.id} · ${x.reason} · ${ago(x.created_at)}${x.note ? " · " + x.note : ""}`)))).catch(() => {});
    put(formBox, panel(panelHead(h("h2", {}, "New preview"), h("span", { class: "muted small" }, "a full copy of " + path + " on its own urls")),
      h("form", { class: "panel-body pvform", onsubmit: (e) => {
        e.preventDefault();
        const imgs = {};
        for (const line of images.value.split(/[\n,]/).map((x) => x.trim()).filter(Boolean)) {
          const i = line.indexOf("=");
          if (i < 1) return toast(`"${line}": expected service=image`);
          imgs[line.slice(0, i).trim()] = line.slice(i + 1).trim();
        }
        formBox.hidden = true;
        previewUp(ctx, { name: name.value, ref: ref.value.trim(), images: imgs, from: Number(from.value) });
      } },
        h("label", { class: "field" }, "Name", name),
        h("label", { class: "field" }, "Git ref (default: the applied commit)", ref),
        h("label", { class: "field" }, "Data", from),
        h("label", { class: "field wide" }, "Image overrides (service=image, one per line)", images),
        h("div", { class: "row" }, go))));
    name.focus();
  };

  const load = async () => {
    let pvs;
    try { pvs = await api("GET", "/previews?project=" + enc(path)); } catch (e) { return list.replaceChildren(errorBox(e)); }
    if (!pvs.length) {
      const reg = st.domain ? "registry." + st.domain : "registry.<domain>";
      const own = ctx.p.services.find((x) => x.image.startsWith(reg + "/"));
      const repo = own ? own.image.slice(reg.length + 1).replace(/[:@].*$/, "") : path + "/" + ((ctx.p.services[0] || {}).name || "web");
      const dom = ((own && own.domains.length ? own : ctx.p.services.find((x) => x.domains.length)) || { domains: [] }).domains[0];
      return put(list, h("div", { class: "empty" },
        h("strong", {}, "No previews"),
        h("div", { class: "small" }, "Push an image with a ", mono("preview-*"), " tag: vops creates preview ", mono("pr-42"), " with it (pushing the project's other images with the same tag lands in the same preview). Production is never redeployed by these tags."),
        h("pre", { class: "cmd" }, `podman push ${reg}/${repo}:preview-pr-42`),
        dom ? h("div", { class: "small" }, "→ ", mono(domainURL(st, dom.replace(/^([^.]+)\./, "$1.pr-42.")))) : null,
        h("div", { class: "small" }, "Or: New preview above, Preview from here on the Timeline, or ", mono(`vops preview up ${path} --name pr-42`), ".")));
    }
    put(list, h("div", { class: "pvlist" }, pvs.map((pv) => {
      const urls = pv.services.flatMap((x) => x.domains.slice(-1)).map((dm) => domainURL(st, dm));
      const cs = pv.services.flatMap((x) => x.containers);
      const del = h("button", { class: "btn sm danger", onclick: async () => {
        if (!(await confirmDialog({ title: `Delete preview ${pv.name}?`, ok: "Delete preview", danger: true,
          body: [`Removes its containers, networks and volumes, its copy of the data and its worktree (${pv.path}). Production is not touched.`, "Registry tags pushed for it stay."] }))) return;
        await busy(del, async () => {
          await api("DELETE", "/previews?project=" + enc(path) + "&name=" + enc(pv.name));
          toast("Preview " + pv.name + " deleted");
          refreshStatus().catch(() => {});
          load();
        });
      } }, "Delete");
      const origin = [pv.deploy_id ? h("a", { href: projectHref(path, "timeline") }, "deploy #" + pv.deploy_id) : pv.snapshot_id ? "snapshot #" + pv.snapshot_id : null,
        pv.deploy_id || pv.snapshot_id ? " · " : null, "commit ", mono(short(pv.commit, 8)), pv.ref && !pv.commit.startsWith(pv.ref) ? [" (", mono(pv.ref), ")"] : null];
      const overrides = Object.entries(pv.images || {});
      return h("div", { class: "pv" },
        h("div", { class: "pv-head" }, dots(cs), h("strong", { class: "mono" }, pv.name), pv.error ? h("span", { class: "tag bad", title: pv.error }, "error") : null,
          h("span", { class: "muted small", title: "removed when not updated for preview_ttl (vops.yml); an update or push resets it · " + fullDate(pv.expires_at) }, "expires " + until(pv.expires_at)),
          h("span", { class: "spacer" }),
          h("div", { class: "actions" },
            urls.length ? h("a", { class: "btn sm", href: urls[0], target: "_blank", rel: "noopener" }, icon("external"), "Open") : null,
            h("a", { class: "btn sm", href: "#/logs/" + enc(pv.path) }, "Logs"), del)),
        pv.error ? h("div", { class: "small error wrapany" }, pv.error) : null,
        h("dl", { class: "kv small" },
          urls.length ? [h("dt", {}, "URLs"), h("dd", {}, urls.map((u) => h("div", { class: "wrapany" }, h("a", { href: u, target: "_blank", rel: "noopener" }, u))))] : null,
          h("dt", {}, "From"), h("dd", {}, origin),
          h("dt", {}, "Data"), h("dd", { class: "wrapany" }, pv.data || "—"),
          h("dt", {}, "Images"), h("dd", {}, overrides.length ? overrides.map(([svc, ref]) => h("div", { class: "mono wrapany", title: ref }, svc + ": " + imgName(ref))) : h("span", { class: "muted" }, "production's")),
          h("dt", {}, "Created"), h("dd", {}, when(pv.created_at), " · updated ", when(pv.updated_at))));
    })));
  };
  ctx.reload = load;
  load();
}

async function eventsTab(ctx, box) {
  box.replaceChildren(loading());
  try { box.replaceChildren(eventList(await api("GET", "/events?project=" + enc(ctx.path)), false)); } catch (e) { box.replaceChildren(errorBox(e)); }
}

// ---- logs
// Lines stream at the bottom; scrolling to the top loads older ones (X-Vops-Before is the journald cursor to continue from).
// While following, the view keeps the last maxLines lines so a tab left open doesn't grow forever.
// since/until filter by time (unix seconds); X-Vops-First/Last give the whole available range, when the host sends them.

const hue = (name) => { let x = 7; for (let i = 0; i < name.length; i++) x = (x * 31 + name.charCodeAt(i)) >>> 0; return x % 360; };
const JLINE = /^(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d) (\S+) \| /; // journald: "2026-09-27 10:15:02 vops-x-web-1 | msg"
const PLINE = /^(\S+) (\d{4}-\d\d-\d\dT[\d:.]+\S*) /; // podman logs --names --timestamps
// host UTC offset in seconds (X-Vops-Offset): journald times are rewritten to the browser's zone, like the from/to inputs
let hostOffset = null;
function toLocal(ts) {
  if (hostOffset === null || hostOffset === -new Date().getTimezoneOffset() * 60) return ts;
  const [y, mo, d, hh, mi, ss] = ts.split(/[- :]/).map(Number);
  const x = new Date(Date.UTC(y, mo - 1, d, hh, mi, ss) - hostOffset * 1000);
  return `${x.getFullYear()}-${pad(x.getMonth() + 1)}-${pad(x.getDate())} ${pad(x.getHours())}:${pad(x.getMinutes())}:${pad(x.getSeconds())}`;
}
function logLine(t) {
  let m;
  if ((m = JLINE.exec(t))) return h("div", { class: "ll" }, h("span", { class: "lt" }, toLocal(m[1])), " ", h("span", { class: "ln", style: "--h:" + hue(m[2]) }, m[2]), t.slice(m[0].length - 3));
  if ((m = PLINE.exec(t))) return h("div", { class: "ll" }, h("span", { class: "ln", style: "--h:" + hue(m[1]) }, m[1]), " ", h("span", { class: "lt" }, m[2]), t.slice(m[0].length - 1));
  return h("div", { class: "ll" }, t);
}
function lineTime(el) {
  const m = el && /(\d{4}-\d\d-\d\d)[ T](\d\d:\d\d:\d\d)(?:\.\d+)?(Z|[+-]\d\d:?\d\d)?/.exec(el.textContent.slice(0, 100));
  if (!m) return null;
  const d = new Date(m[1] + "T" + m[2] + (m[3] ? m[3].replace(/^([+-]\d\d)(\d\d)$/, "$1:$2") : ""));
  return isNaN(d) ? null : d;
}

function logView({ path, service, services, hrefFor, fullHref }) {
  const maxLines = 5000;
  const narrow = matchMedia("(max-width: 700px)").matches;
  const view = h("div", { class: "logview" + (narrow ? " wrap" : ""), tabindex: "0" });
  const lines = view.getElementsByClassName("ll");
  const grep = h("input", { type: "search", class: "search", placeholder: "Search all history", "aria-label": "search" });
  const from = h("input", { type: "datetime-local", step: "60", "aria-label": "from", onchange: () => onRange() });
  const to = h("input", { type: "datetime-local", step: "60", "aria-label": "to", onchange: () => onRange() });
  const clear = h("button", { type: "button", class: "btn ghost sm", hidden: true, onclick: () => { from.value = to.value = ""; follow.checked = true; onRange(); } }, "Clear");
  const follow = h("input", { type: "checkbox", checked: true, onchange: () => start() });
  const wrap = h("input", { type: "checkbox", checked: narrow, onchange: () => view.classList.toggle("wrap", wrap.checked) });
  const status = h("span", {}), avail = h("span", {}), loaded = h("span", {});
  const more = h("button", { type: "button", class: "linkbtn", hidden: true, onclick: () => older() }, "load older");
  const svcNames = service && !services.includes(service) ? [...services, service] : services;
  const svcSel = svcNames.length > 1 || service ? h("select", { "aria-label": "service", onchange: (e) => { location.hash = hrefFor(e.target.value); } },
    h("option", { value: "" }, "All services"), svcNames.map((s) => h("option", { value: s, selected: s === service }, s))) : null;
  let cancel = () => {}, before = null, busyOlder = false, gen = 0, partial = "", dropped = false, metaQueued = false;

  // datetime-local is local time, minute precision; "to" includes its whole minute
  const unix = (input, end) => {
    if (!input.value) return null;
    const t = Math.floor(new Date(input.value).getTime() / 1000);
    return isNaN(t) ? null : t + (end && input.value.length <= 16 ? 59 : 0);
  };
  const query = (extra) => {
    const q = { project: path, service: service || "", n: "500", grep: grep.value, ...extra };
    const s = unix(from), u = unix(to, true);
    if (s !== null) q.since = String(s);
    if (u !== null) q.until = String(u);
    return "/logs?" + new URLSearchParams(q);
  };
  const setBefore = (c) => {
    before = c;
    more.hidden = !c;
    if (!dropped) status.textContent = c ? "" : from.value ? "start of range" : "start of logs";
  };
  const renderMeta = () => {
    metaQueued = false;
    const n = lines.length, a = lineTime(lines[0]), b = lineTime(lines[n - 1]);
    loaded.textContent = n ? `loaded: ${n.toLocaleString()} line${n === 1 ? "" : "s"}` + (a && b ? `, ${fmtDate(a)} → ${fmtDate(b)}` : "") : "loaded: none";
  };
  const meta = () => { if (!metaQueued) { metaQueued = true; requestAnimationFrame(renderMeta); } };
  const setAvail = (res) => {
    const off = res.headers.get("X-Vops-Offset");
    if (off !== null) hostOffset = Number(off);
    const first = Number(res.headers.get("X-Vops-First")) || 0, last = Number(res.headers.get("X-Vops-Last")) || 0;
    if (!first && !last) return; // older hosts don't send them
    const at = (u, title, fill) => (u ? h("button", { type: "button", class: "linkbtn", title, onclick: fill }, fmtUnix(u)) : "?");
    avail.replaceChildren("available: ",
      at(first, "Show from here", () => { from.value = localInput(first); onRange(); }), " → ",
      at(last, "Show until here", () => { to.value = localInput(last); onRange(); }));
  };
  const write = (chunk) => {
    const parts = (partial + chunk).split("\n");
    partial = parts.pop();
    if (!parts.length) return;
    const f = document.createDocumentFragment();
    for (const t of parts) f.append(logLine(t));
    view.append(f);
    meta();
  };
  const trim = () => {
    const extra = lines.length - maxLines;
    if (extra <= 0) return;
    for (let i = 0; i < extra; i++) lines[0].remove();
    dropped = true;
    setBefore(null);
    status.textContent = "older lines dropped while following; Reload to browse history";
  };
  const older = async () => {
    if (!before || busyOlder) return;
    busyOlder = true;
    const g = gen;
    status.textContent = "loading older…";
    try {
      const res = await fetch("/api" + query({ before }));
      const text = await res.text();
      if (g !== gen) return;
      if (!res.ok) { status.textContent = errText(text); return; }
      setAvail(res);
      const ls = text.split("\n");
      if (ls.at(-1) === "") ls.pop();
      const f = document.createDocumentFragment();
      for (const t of ls) f.append(logLine(t));
      const h0 = view.scrollHeight;
      view.prepend(f);
      view.scrollTop += view.scrollHeight - h0;
      setBefore(res.headers.get("X-Vops-Before"));
      meta();
    } catch (e) {
      if (g === gen) status.textContent = e.message;
    } finally {
      busyOlder = false;
    }
  };
  view.addEventListener("scroll", () => { if (view.scrollTop < 80) older(); });

  // a closed range ("to" set) can't follow
  const onRange = () => {
    follow.disabled = !!to.value;
    if (to.value) follow.checked = false;
    clear.hidden = !from.value && !to.value;
    start();
  };
  const start = () => {
    cancel();
    gen++;
    partial = "";
    dropped = false;
    view.replaceChildren();
    setBefore(null);
    meta();
    const s = unix(from), u = unix(to, true);
    if (s !== null && u !== null && s > u) { status.textContent = "“from” is after “to”"; return; }
    status.textContent = "loading…";
    const filtered = grep.value || s !== null || u !== null;
    cancel = stream(query(follow.checked && u === null ? { follow: "1" } : {}), { el: view, write }, {
      response: (res) => { setAvail(res); setBefore(res.headers.get("X-Vops-Before")); },
      stuck: trim,
      done: () => {
        if (partial) { view.append(logLine(partial)); partial = ""; meta(); }
        if (!lines.length) view.append(h("div", { class: "note" }, filtered ? "No lines match these filters." : "No lines."));
      },
      error: (msg) => { status.textContent = msg; },
    });
  };
  const download = () => {
    if (!lines.length) return toast("Nothing to download");
    const text = Array.from(lines, (l) => l.textContent).join("\n") + "\n";
    const d = new Date();
    const name = `${path.replaceAll("/", "-")}${service ? "-" + service : ""}-${d.getFullYear()}${pad(d.getMonth() + 1)}${pad(d.getDate())}-${pad(d.getHours())}${pad(d.getMinutes())}.txt`;
    const a = h("a", { href: URL.createObjectURL(new Blob([text], { type: "text/plain;charset=utf-8" })), download: name });
    document.body.append(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(a.href), 2000);
  };

  const el = h("div", { class: "grow" },
    h("form", { class: "logbar", onsubmit: (e) => { e.preventDefault(); start(); } },
      svcSel, grep,
      h("label", { class: "dt" }, "from", from), h("label", { class: "dt" }, "to", to), clear,
      h("div", { class: "row end" },
        h("label", { class: "check" }, follow, "follow"),
        h("label", { class: "check" }, wrap, "wrap"),
        h("button", { class: "btn" }, icon("refresh"), "Reload"),
        h("button", { type: "button", class: "btn", title: "Download the lines in view", onclick: download }, icon("download"), "Download .txt"),
        fullHref ? h("a", { class: "btn icon", href: fullHref, title: "Full page", "aria-label": "full page" }, icon("expand")) : null)),
    h("div", { class: "logmeta" }, avail, loaded, more, h("span", { class: "spacer" }), status),
    view);
  return { el, start, stop: () => { cancel(); gen++; } };
}

async function logsPage(alive, path, service) {
  $app.className = "fill";
  let st;
  try { st = await getStatus(); } catch (e) { return failed(e, alive); }
  if (!alive()) return;
  const q = service ? "?service=" + enc(service) : "";
  // a preview (shop@pr-42) has no status entry: its services come from the previews api, and "back" is its project's Previews tab
  const [base, preview] = path.split("@");
  let p = st.projects.find((x) => x.path === path), services = p ? p.services.map((s) => s.name) : [];
  if (preview) {
    try { services = ((await api("GET", "/previews?project=" + enc(base))).find((x) => x.name === preview) || { services: [] }).services.map((s) => s.name); } catch {}
    if (!alive()) return;
  }
  const lv = logView({ path, service, services, hrefFor: (svc) => "#/logs/" + enc(path) + (svc ? "?service=" + enc(svc) : "") });
  page(head({ title: [path, service ? " / " + service : ""], crumbs: preview ? "Preview logs · " : "Logs · ", dot: p ? healthDot(p, "lg") : null,
    actions: h("a", { class: "btn", href: preview ? projectHref(base, "previews") : projectHref(path, "logs", q) }, icon("back"), preview ? "Previews" : "Project") }), lv.el);
  lv.start();
  onLeave(lv.stop);
}

// ---- files: git-tracked folders and common text files only

async function filesPage(alive, params) {
  let files;
  try { files = await api("GET", "/tree"); } catch (e) { return failed(e, alive); }
  if (!alive()) return;
  const viewer = panel(empty("Pick a file", "Only git-tracked text files are shown."));
  let active = null;
  const open = async (f, a) => {
    active && active.classList.remove("active");
    (active = a).classList.add("active");
    viewer.replaceChildren(loading("Loading…", "pad"));
    try {
      const text = await apiText("/file?path=" + enc(f.path));
      viewer.replaceChildren(panelHead(h("strong", { class: "mono wrapany" }, f.path), h("span", { class: "spacer" }), h("span", { class: "muted small nowrap" }, size(f.size) + " · " + text.split("\n").length + " lines")), h("pre", { class: "file" }, text));
    } catch (e) { viewer.replaceChildren(h("div", { class: "panel-body" }, errorBox(e))); }
  };
  const dir = params.get("dir") || "";
  const shown = files.filter((f) => f.text && (!dir || f.path.startsWith(dir + "/")));
  const tree = {};
  for (const f of shown) {
    let node = tree;
    const parts = f.path.split("/");
    parts.slice(0, -1).forEach((p) => (node = node[p + "/"] = node[p + "/"] || {}));
    node[parts.at(-1)] = f;
  }
  const render = (node) => h("ul", { class: "ftree" }, Object.keys(node).sort((a, b) => (b.endsWith("/") - a.endsWith("/")) || a.localeCompare(b)).map((k) => {
    if (k.endsWith("/")) return h("li", {}, h("div", { class: "fdir" }, icon("folder"), k), render(node[k]));
    const a = h("a", { href: "#", onclick: (e) => { e.preventDefault(); open(node[k], a); } }, icon("file"), k);
    return h("li", {}, a);
  }));
  page(head({ title: "Files", sub: ["Tracked by git in ~/vops", sep(), shown.length + " text files", dir ? h("span", { class: "tag mono" }, dir, " ", h("a", { href: "#/files", title: "all files", "aria-label": "show all files" }, "×")) : null] }),
    shown.length ? h("div", { class: "split" }, h("div", { class: "panel filelist" }, render(tree)), viewer) : panel(empty("No text files", dir ? "Nothing tracked under " + dir + "." : "Nothing tracked by git yet.")));
}

// ---- registry

async function registryPage(alive) {
  let reg;
  try { reg = await api("GET", "/registry"); } catch (e) { return failed(e, alive); }
  if (!alive()) return;
  const host = reg.host || location.host;
  const gcBtn = h("button", { class: "btn", onclick: () => busy(gcBtn, async () => {
    const r = await api("POST", "/registry/gc");
    toast(`Removed ${r.manifests} manifests, ${r.blobs} blobs, freed ${size(r.freed)}`);
  }) }, "Garbage collect");
  const del = async (r, t) => {
    if (!(await confirmDialog({ title: `Delete ${r.name}:${t.name}?`, ok: "Delete tag", danger: true,
      body: ["Pulls of this tag fail from now on; running containers keep running. Its layers are freed by the next garbage collection."] }))) return;
    try { await api("DELETE", "/registry?repo=" + enc(r.name) + "&tag=" + enc(t.name)); toast("Deleted " + r.name + ":" + t.name); registryPage(alive); } catch (x) { toast(x.message); }
  };
  page(head({ title: "Registry", sub: [h("span", {}, "podman login ", mono(host)), sep(), "admin can pull everything and push nothing"], actions: gcBtn }),
    reg.repos.length ? h("div", { class: "stack" }, reg.repos.map((r) => panel(
      panelHead(icon("box"), h("h2", { class: "mono wrapany" }, r.name), h("span", { class: "tag" }, r.tags.length + " tag" + (r.tags.length === 1 ? "" : "s"))),
      table(["Tag", "Digest", "Size", "Pushed", ""], r.tags.map((t) => h("tr", {},
        h("td", { class: "mono wrapany" }, t.name),
        h("td", { class: "mono small", title: t.digest }, short(t.digest)),
        h("td", { class: "nowrap" }, size(t.size)),
        h("td", { class: "muted" }, when(Date.parse(t.pushed_at) / 1000)),
        h("td", { class: "actions-cell" }, h("button", { class: "btn sm danger", onclick: () => del(r, t) }, "Delete"))))))))
      : panel(empty("Nothing pushed yet", ["podman push ", h("span", { class: "mono" }, host + "/<project>/<image>:<tag>")])));
}

// ---- registry users

async function usersPage(alive, created) {
  let users;
  try { users = await api("GET", "/users"); } catch (e) { return failed(e, alive); }
  if (!alive()) return;
  const name = h("input", { placeholder: "shop-ci", required: true, pattern: "[a-z0-9][a-z0-9._-]*" });
  const pattern = h("input", { placeholder: "shop/.*", class: "mono" });
  const repos = h("input", { placeholder: "shop/web, shop/api", class: "mono" });
  const saveBtn = h("button", { class: "btn primary" }, "Save");
  const save = async (body) => {
    try {
      const r = await api("POST", "/users", body);
      usersPage(alive, r.token ? r : null);
    } catch (x) { toast(x.message); }
  };
  const form = h("form", { class: "inline", onsubmit: (e) => {
    e.preventDefault();
    busy(saveBtn, () => save({ name: name.value, pattern: pattern.value, repos: repos.value.split(",").map((s) => s.trim()).filter(Boolean) }));
  } }, h("label", { class: "field" }, "Name", name), h("label", { class: "field" }, "Regex (anchored)", pattern), h("label", { class: "field" }, "Or exact repositories", repos), saveBtn);

  const rotate = async (u) => {
    if (!(await confirmDialog({ title: `New token for ${u.name}?`, ok: "Generate new token", danger: true,
      body: ["The current token stops working right away: update it wherever it's used (CI secrets, other hosts)."] }))) return;
    save({ name: u.name, pattern: u.pattern, repos: u.repos, new_token: true });
  };
  const del = async (u) => {
    if (!(await confirmDialog({ title: `Delete ${u.name}?`, ok: "Delete user", danger: true,
      body: ["Its token stops working right away: anything logging in as it loses registry access. Images stay."] }))) return;
    try { await api("DELETE", "/users?name=" + enc(u.name)); toast("Deleted " + u.name); usersPage(alive); } catch (x) { toast(x.message); }
  };
  const dash = h("span", { class: "faint" }, "—");
  page(head({ title: "Registry users", sub: "Each user can push and pull the repositories matching its regex or list." }),
    created ? alertBox("info", `Token for ${created.name} (shown once)`, [
      h("div", { class: "token mono" }, created.token),
      h("pre", {}, `podman login -u ${created.name} ${location.hostname.replace(/^vops\./, "registry.")}`)]) : null,
    panel(users.length ? table(["Name", "Regex", "Repositories", "Created", ""], users.map((u) => h("tr", {},
      h("td", { class: "mono" }, u.name),
      h("td", { class: "mono small wrapany" }, u.pattern || dash.cloneNode(true)),
      h("td", { class: "mono small wrapany" }, (u.repos || []).join(", ") || dash.cloneNode(true)),
      h("td", { class: "muted" }, when(u.created_at)),
      h("td", { class: "actions-cell" }, h("div", { class: "actions" },
        h("button", { class: "btn sm", onclick: () => { name.value = u.name; pattern.value = u.pattern; repos.value = (u.repos || []).join(", "); name.focus(); } }, "Edit"),
        h("button", { class: "btn sm", onclick: () => rotate(u) }, "New token"),
        h("button", { class: "btn sm danger", onclick: () => del(u) }, "Delete"))))))
      : empty("No users yet", "Add one below to push from CI or another machine.")),
    h("div", { class: "section" }, h("div", { class: "section-head" }, h("h2", {}, "Add or edit")), panel(h("div", { class: "panel-body" }, form))));
}

// ---- events

const kindClass = (k) => (k === "error" ? " bad" : k === "rollback" || k === "restart" ? " warn" : k === "push" || k === "snapshot" ? " accent" : "");
function eventList(evs, withProject = true) {
  if (!evs.length) return panel(empty("No events yet", "Deploys, config changes, pushes and logins show up here."));
  return panel(table(["When", "Kind", withProject ? "Project" : null, "Message"].filter((x) => x !== null), evs.map((e) => h("tr", {},
    h("td", { class: "muted small" }, when(e.at)),
    h("td", {}, h("span", { class: "tag" + kindClass(e.kind) }, e.kind)),
    withProject ? h("td", { class: "nowrap" }, e.project ? h("a", { href: projectHref(e.project) }, e.project) : "") : null,
    h("td", { class: "wrapany" }, e.message)))));
}

async function eventsPage(alive) {
  let evs, audit;
  try { [evs, audit] = await Promise.all([api("GET", "/events"), api("GET", "/audit?n=200")]); } catch (e) { return failed(e, alive); }
  if (!alive()) return;
  page(head({ title: "Events", sub: "Deploys, config changes, pushes and logins." }), eventList(evs),
    h("div", { class: "section" },
      h("div", { class: "section-head" }, h("h2", {}, "Audit log"), h("span", { class: "hint" }, "Every write to the host's database, recorded by sqlite triggers. Secrets are never recorded.")),
      audit.length ? panel(table(["When", "Table", "Op", "Key", "Detail"], audit.map((a) => h("tr", {},
        h("td", { class: "muted small" }, when(a.at)),
        h("td", { class: "mono small" }, a.tbl),
        h("td", {}, h("span", { class: "tag" }, a.op)),
        h("td", { class: "mono small wrapany" }, a.key),
        h("td", { class: "small wrapany" }, a.detail)))))
        : panel(empty("Empty", null))));
}

// ---- router. Old links keep working: #/p/<path> is the services tab, #/logs/<path>?service=x the full-page logs.

let routeGen = 0, cleanups = [];
const onLeave = (fn) => cleanups.push(fn);
function leave() {
  for (const f of cleanups.splice(0)) { try { f(); } catch {} }
  onStatus = null;
}
function failed(e, alive) {
  if (e.message === "login required" || (alive && !alive())) return;
  page(errorBox(e));
}

function route() {
  leave();
  const gen = ++routeGen;
  const alive = () => gen === routeGen;
  $login.replaceChildren();
  $shell.hidden = false;
  document.body.classList.remove("nav-open");
  $app.className = "";
  $app.replaceChildren(loading());
  window.scrollTo(0, 0);
  markActive();
  if (!status) refreshStatus().catch(() => {});
  const [path, query] = location.hash.slice(1).split("?");
  const params = new URLSearchParams(query || "");
  let m;
  if ((m = path.match(PROJECT_RE))) return projectPage(alive, dec(m[1]), m[2] || "services", params);
  if ((m = path.match(/^\/logs\/(.+)$/))) return logsPage(alive, dec(m[1]), params.get("service"));
  if (path === "/files") return filesPage(alive, params);
  if (path === "/registry") return registryPage(alive);
  if (path === "/users") return usersPage(alive);
  if (path === "/events") return eventsPage(alive);
  return overview(alive);
}

window.addEventListener("hashchange", route);
route();
