// vops dashboard: vanilla js, no build step. Everything is rendered with h() (no innerHTML), so data can't inject markup.
"use strict";

const $app = document.getElementById("app");
let stopCurrent = () => {}; // cancels polling/streams of the current page

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

function toast(msg) {
  const t = h("div", { class: "toast" }, msg);
  document.body.append(t);
  setTimeout(() => t.remove(), 3500);
}

async function api(method, path, body) {
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
  let data = text;
  try { data = JSON.parse(text); } catch {}
  if (!res.ok) throw new Error((data && data.error) || text || res.statusText);
  return data;
}

// stream reads a text response line by line into a <pre>; returns a cancel function.
function stream(path, pre, opts = {}) {
  const ctl = new AbortController();
  (async () => {
    try {
      const res = await fetch("/api" + path, { method: opts.method || "GET", headers: { "X-Vops": "1", "Content-Type": "application/json" }, body: opts.body, signal: ctl.signal });
      if (!res.ok) { pre.append(await res.text()); return; }
      const reader = res.body.getReader();
      const dec = new TextDecoder();
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        const stick = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 30;
        pre.append(dec.decode(value, { stream: true }));
        if (stick) pre.scrollTop = pre.scrollHeight;
      }
      opts.done && opts.done();
    } catch (e) {
      if (e.name !== "AbortError") pre.append("\n" + e.message);
    }
  })();
  return () => ctl.abort();
}

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
const stateOf = (c) => (c.Labels && c.Labels["vops.job"] && c.State === "exited" && c.ExitCode === 0 ? "done" : c.State);
const short = (s, n = 12) => (s || "").replace("sha256:", "").slice(0, n);
const enc = encodeURIComponent;

function page(title, sub, ...body) {
  $app.replaceChildren(h("h1", {}, title), ...(sub ? [h("div", { class: "sub" }, sub)] : []), ...body.flat(Infinity).filter((x) => x !== null && x !== undefined && x !== false));
}

function failed(e) {
  if (e.message !== "login required") $app.replaceChildren(h("p", { class: "error" }, e.message));
}

// ---- login

function showLogin() {
  stopCurrent();
  document.getElementById("top").hidden = true;
  const pw = h("input", { type: "password", placeholder: "password", autocomplete: "current-password", autofocus: true });
  const err = h("div", { class: "error small" });
  const form = h("form", { class: "panel pad login", onsubmit: async (e) => {
    e.preventDefault();
    try {
      await api("POST", "/login", { password: pw.value });
      route();
    } catch (x) { err.textContent = x.message; }
  } },
    h("div", { class: "center" }, h("span", { class: "logo" }, "v")),
    h("h1", { class: "center" }, "vops"),
    h("p", { class: "muted center small" }, "Log in as admin"),
    h("div", { style: "display:grid;gap:10px" }, pw, h("button", { class: "primary" }, "Log in"), err),
  );
  $app.replaceChildren(form);
  pw.focus();
}

document.getElementById("logout").onclick = async () => {
  await api("POST", "/logout").catch(() => {});
  showLogin();
};

// ---- projects overview: the tree of the host

function dots(containers) {
  return h("span", { class: "dots" }, containers.length ? containers.map((c) => h("span", { class: "dot " + stateOf(c), title: c.Names[0] + ": " + c.Status })) : h("span", { class: "dot", title: "no containers" }));
}

function projectTree(projects, domain) {
  // nest projects by path: shop/api goes under shop
  const root = { children: {}, project: null };
  for (const p of projects) {
    let node = root;
    for (const part of p.path.split("/")) {
      node.children[part] = node.children[part] || { children: {}, project: null };
      node = node.children[part];
    }
    node.project = p;
  }
  const render = (node) => h("ul", { class: "tree" }, Object.entries(node.children).sort().map(([name, child]) => {
    const p = child.project;
    const head = p
      ? h("div", { class: "node" },
          h("a", { class: "project", href: "#/p/" + enc(p.path) }, name),
          p.disabled ? h("span", { class: "tag warn" }, "disabled") : null,
          p.gone ? h("span", { class: "tag bad" }, "removed from git") : null,
          p.error ? h("span", { class: "tag bad", title: p.error }, "invalid compose") : null,
          h("span", { class: "muted small" }, p.commit ? "applied " + short(p.commit, 8) + " " + ago(p.applied_at) : "never applied"))
      : h("div", { class: "node folder" }, name + "/");
    const services = p ? h("div", { style: "padding-left:18px" }, p.services.map((s) => h("div", { class: "svc" },
      dots(s.containers),
      h("span", { class: "mono" }, s.name),
      h("span", { class: "row small" },
        s.domains.slice(0, 2).map((d) => h("a", { href: (domain ? "https://" : "http://") + d, target: "_blank", rel: "noopener" }, d)),
        s.pending ? h("span", { class: "tag warn" }, "pending " + s.pending) : null),
    ))) : null;
    return h("li", {}, head, services, Object.keys(child.children).length ? render(child) : null);
  }));
  return render(root);
}

function planBanner(st, reload) {
  if (!st.changes) return null;
  const out = h("pre", { hidden: true });
  const btn = h("button", { class: "primary", onclick: async () => {
    btn.disabled = true;
    out.hidden = false;
    out.textContent = "";
    stream("/apply", out, { method: "POST", body: JSON.stringify({ commit: st.commit }), done: () => { btn.disabled = false; reload(); } });
  } }, "Apply");
  const show = h("button", { onclick: async () => {
    out.hidden = false;
    out.textContent = await api("GET", "/plan?format=text");
  } }, "Show plan");
  return h("div", { class: "banner" }, h("div", { class: "row" }, h("strong", {}, "The host differs from git"), h("span", { class: "spacer" }), show, btn), out);
}

async function overview() {
  let timer;
  const load = async () => {
    try {
      const st = await api("GET", "/status");
      page("Projects", ["~/vops at ", h("span", { class: "mono" }, short(st.commit) || "no commits yet"), st.domain ? " · " + st.domain : " · no domain set in vops.yml"],
        st.warnings.map((w) => h("p", { class: "tag warn" }, w)),
        planBanner(st, load),
        st.projects.length
          ? h("div", { class: "panel pad" }, projectTree(st.projects, st.domain))
          : h("div", { class: "panel pad muted" }, "No projects yet. Add a folder with a compose.yml to the repo and run vops sync."));
    } catch (e) { failed(e); }
  };
  await load();
  timer = setInterval(load, 10000);
  stopCurrent = () => clearInterval(timer);
}

// ---- one project

async function projectPage(path) {
  let st, envs;
  try {
    [st, envs] = await Promise.all([api("GET", "/status"), api("GET", "/env?project=" + enc(path))]);
  } catch (e) { return failed(e); }
  const p = st.projects.find((x) => x.path === path);
  if (!p) return page(path, "Unknown project");
  const reload = () => projectPage(path);

  const toggle = h("button", { onclick: async () => {
    await api("POST", "/project", { project: path, disabled: !p.disabled });
    toast(p.disabled ? "Enabled: apply to start it" : "Disabled: apply to stop it");
    reload();
  } }, p.disabled ? "Enable" : "Disable");

  const services = h("table", {},
    h("tr", {}, h("th", {}, "Service"), h("th", {}, "Containers"), h("th", {}, "Image"), h("th", {})),
    p.services.map((s) => h("tr", {},
      h("td", {}, h("div", { class: "mono" }, s.name), s.domains.map((d) => h("div", { class: "small" }, h("a", { href: (st.domain ? "https://" : "http://") + d, target: "_blank", rel: "noopener" }, d))),
        s.pending ? h("span", { class: "tag warn" }, "pending " + s.pending + (s.reason ? ": " + s.reason : "")) : null),
      h("td", {}, s.containers.length ? s.containers.map((c) => h("div", { class: "row small" }, h("span", { class: "dot " + stateOf(c) }), h("span", { class: "mono" }, c.Names[0]), h("span", { class: "muted" }, c.Status))) : h("span", { class: "muted small" }, "none")),
      h("td", { class: "mono small" }, s.image),
      h("td", {}, h("div", { class: "row" },
        h("a", { class: "btn", href: "#/logs/" + enc(path) + "?service=" + enc(s.name) }, "Logs"),
        s.containers.length ? h("button", { onclick: async (e) => {
          e.target.disabled = true;
          try { await api("POST", "/restart", { project: path, service: s.name }); toast("Restarted " + s.name); } catch (x) { toast(x.message); }
          reload();
        } }, "Restart") : null)),
    )));

  const key = h("input", { placeholder: "KEY", pattern: "[A-Za-z_][A-Za-z0-9_]*", required: true, class: "mono" });
  const val = h("input", { placeholder: "value", type: "password", autocomplete: "off" });
  const envForm = h("form", { class: "inline", onsubmit: async (e) => {
    e.preventDefault();
    try {
      await api("POST", "/env", { project: path, key: key.value, value: val.value });
      toast(key.value + " set. Apply to deploy it.");
      reload();
    } catch (x) { toast(x.message); }
  } }, h("label", {}, "Name", key), h("label", {}, "Value (write-only)", val), h("button", { class: "primary" }, "Set"));

  const envTable = envs.length ? h("table", {}, envs.map((e) => h("tr", {},
    h("td", { class: "mono" }, e.key), h("td", { class: "muted small" }, "••••••••  set " + ago(e.updated_at)),
    h("td", { style: "text-align:right" }, h("button", { class: "danger", onclick: async () => {
      if (!confirm("Remove " + e.key + "?")) return;
      await api("DELETE", "/env?project=" + enc(path) + "&key=" + enc(e.key));
      reload();
    } }, "Remove"))))) : h("p", { class: "muted small" }, "No variables. They are available as ${KEY} in compose.yml and to `environment: [KEY]`.");

  const events = h("div", {});
  api("GET", "/events?project=" + enc(path)).then((evs) => events.replaceChildren(eventTable(evs.slice(0, 15)))).catch(() => {});
  const snapshots = h("div", {}, h("p", { class: "muted small" }, "Loading…"));
  loadSnapshots(path, snapshots);

  page(path, [p.commit ? "Applied " + short(p.commit, 8) + " " + ago(p.applied_at) : "Never applied", p.disabled ? " · disabled" : ""],
    p.error ? h("pre", { class: "error" }, p.error) : null,
    planBanner(st, reload),
    h("div", { class: "row" }, h("a", { class: "btn", href: "#/logs/" + enc(path) }, "All logs"), h("a", { class: "btn", href: "#/files?dir=" + enc(path) }, "Files"), h("span", { class: "spacer" }), toggle),
    h("h2", {}, "Services"), h("div", { class: "panel" }, services),
    h("h2", {}, "Environment"), h("div", { class: "panel pad" }, envTable, h("div", { style: "margin-top:12px" }, envForm)),
    h("h2", {}, "Data & snapshots"), snapshots,
    h("h2", {}, "Recent events"), events,
  );
}

// ---- snapshots: btrfs copies of a project's data; taken before every deploy, restorable

async function loadSnapshots(path, box) {
  let res;
  try { res = await api("GET", "/snapshots?project=" + enc(path)); } catch (e) { return box.replaceChildren(h("p", { class: "error" }, e.message)); }
  const d = res.data;
  const reload = () => loadSnapshots(path, box);
  const out = h("pre", { hidden: true });
  const note = h("input", { placeholder: "note (optional)" });
  const take = h("form", { class: "inline", onsubmit: async (e) => {
    e.preventDefault();
    try {
      const r = await api("POST", "/snapshots", { project: path, note: note.value });
      toast("Snapshot #" + r.snapshot.id + " taken");
      reload();
    } catch (x) { toast(x.message); }
  } }, note, h("button", { class: "primary", disabled: !d.supported || !d.protected.length }, "Snapshot now"));
  const restore = (s) => {
    if (!confirm(`Restore ${path} to snapshot #${s.id}?\n\nIts containers stop while the data is restored. The current data is snapshotted first, so you can undo this.\nCode is not rolled back.`)) return;
    out.hidden = false;
    out.textContent = "";
    stream("/rollback", out, { method: "POST", body: JSON.stringify({ project: path, id: s.id }), done: reload });
  };
  box.replaceChildren(h("div", { class: "panel pad" },
    d.supported
      ? h("p", { class: "small" }, d.protected.length ? ["Protected: ", h("span", { class: "mono" }, d.protected.join(", "))] : h("span", { class: "muted" }, "No volumes or data dirs to protect."))
      : h("p", { class: "small muted" }, "Snapshots unavailable: " + d.reason),
    d.unprotected && d.unprotected.length ? h("p", { class: "small tag warn" }, "Not covered (not btrfs subvolumes): " + d.unprotected.join(", ")) : null,
    res.snapshots.length ? h("table", {},
      h("tr", {}, h("th", {}, "#"), h("th", {}, "Kind"), h("th", {}, "Taken"), h("th", {}, "Commit"), h("th", {}, "Note"), h("th", {})),
      res.snapshots.map((s) => h("tr", {},
        h("td", { class: "mono" }, s.id),
        h("td", {}, h("span", { class: "tag" }, s.reason)),
        h("td", { class: "muted", title: new Date(s.created_at * 1000).toLocaleString() }, ago(s.created_at)),
        h("td", { class: "mono small" }, short(s.commit, 8)),
        h("td", { class: "small" }, s.note),
        h("td", {}, h("div", { class: "row", style: "justify-content:end" },
          h("button", { onclick: () => restore(s) }, "Restore"),
          h("button", { class: "danger", onclick: async () => {
            if (!confirm("Delete snapshot #" + s.id + "?")) return;
            try { await api("DELETE", "/snapshots?id=" + s.id); reload(); } catch (x) { toast(x.message); }
          } }, "Delete")))))) : h("p", { class: "muted small" }, "No snapshots yet. One is taken automatically before every deploy that changes this project."),
    h("div", { style: "margin-top:12px" }, take),
    out));
}

// ---- logs

function logsPage(path, service) {
  const pre = h("pre", { class: "logs" });
  const grep = h("input", { placeholder: "filter", value: "" });
  const follow = h("input", { type: "checkbox", checked: true });
  let cancel = () => {};
  const start = () => {
    cancel();
    pre.textContent = "";
    const q = new URLSearchParams({ project: path, service: service || "", n: "500", grep: grep.value });
    if (follow.checked) q.set("follow", "1");
    cancel = stream("/logs?" + q, pre);
  };
  stopCurrent = () => cancel();
  page("Logs", [h("a", { href: "#/p/" + enc(path) }, path), service ? " / " + service : ""],
    h("form", { class: "inline", onsubmit: (e) => { e.preventDefault(); start(); } },
      grep, h("label", { class: "row" }, follow, "follow"), h("button", {}, "Reload")),
    pre);
  start();
}

// ---- files: git-tracked folders and common text files only

async function filesPage(params) {
  let files;
  try { files = await api("GET", "/tree"); } catch (e) { return failed(e); }
  const viewer = h("div", { class: "panel pad" }, h("p", { class: "muted" }, "Pick a file."));
  const open = async (f) => {
    try {
      const text = await api("GET", "/file?path=" + enc(f.path));
      viewer.replaceChildren(h("div", { class: "row" }, h("strong", { class: "mono" }, f.path), h("span", { class: "muted small" }, size(f.size))), h("pre", {}, text));
    } catch (e) { viewer.replaceChildren(h("p", { class: "error" }, e.message)); }
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
  const render = (node) => h("ul", { class: "tree" }, Object.keys(node).sort((a, b) => (b.endsWith("/") - a.endsWith("/")) || a.localeCompare(b)).map((k) =>
    k.endsWith("/") ? h("li", {}, h("span", { class: "folder" }, k), render(node[k])) : h("li", {}, h("a", { href: "#", onclick: (e) => { e.preventDefault(); open(node[k]); } }, k))));
  page("Files", ["Tracked by git in ~/vops", dir ? " · " + dir : "", " · " + shown.length + " text files"],
    h("div", { class: "grid2" }, h("div", { class: "panel pad filelist" }, render(tree)), viewer));
}

// ---- registry

async function registryPage() {
  let reg;
  try { reg = await api("GET", "/registry"); } catch (e) { return failed(e); }
  const host = reg.host || location.host;
  const gcBtn = h("button", { onclick: async () => {
    gcBtn.disabled = true;
    try {
      const r = await api("POST", "/registry/gc");
      toast(`Removed ${r.manifests} manifests, ${r.blobs} blobs, freed ${size(r.freed)}`);
    } catch (x) { toast(x.message); }
    gcBtn.disabled = false;
  } }, "Garbage collect");
  page("Registry", ["podman login ", h("span", { class: "mono" }, host), " · admin can pull everything and push nothing"],
    h("div", { class: "row" }, h("span", { class: "spacer" }), gcBtn),
    reg.repos.length ? reg.repos.map((r) => [
      h("h2", { class: "mono" }, r.name),
      h("div", { class: "panel" }, h("table", {},
        h("tr", {}, h("th", {}, "Tag"), h("th", {}, "Digest"), h("th", {}, "Size"), h("th", {}, "Pushed"), h("th", {})),
        r.tags.map((t) => h("tr", {},
          h("td", { class: "mono" }, t.name),
          h("td", { class: "mono small", title: t.digest }, short(t.digest)),
          h("td", {}, size(t.size)),
          h("td", { class: "muted" }, ago(Date.parse(t.pushed_at) / 1000)),
          h("td", { style: "text-align:right" }, h("button", { class: "danger", onclick: async () => {
            if (!confirm(`Delete ${r.name}:${t.name}?`)) return;
            await api("DELETE", "/registry?repo=" + enc(r.name) + "&tag=" + enc(t.name));
            registryPage();
          } }, "Delete"))))))]) : h("div", { class: "panel pad muted" }, "Nothing pushed yet."));
}

// ---- registry users

async function usersPage(created) {
  let users;
  try { users = await api("GET", "/users"); } catch (e) { return failed(e); }
  const name = h("input", { placeholder: "shop-ci", required: true, pattern: "[a-z0-9][a-z0-9._-]*" });
  const pattern = h("input", { placeholder: "shop/.*", class: "mono" });
  const repos = h("input", { placeholder: "shop/web, shop/api", class: "mono" });
  const save = async (body) => {
    try {
      const r = await api("POST", "/users", body);
      usersPage(r.token ? r : null);
    } catch (x) { toast(x.message); }
  };
  const form = h("form", { class: "inline", onsubmit: (e) => {
    e.preventDefault();
    save({ name: name.value, pattern: pattern.value, repos: repos.value.split(",").map((s) => s.trim()).filter(Boolean) });
  } }, h("label", {}, "Name", name), h("label", {}, "Regex (anchored)", pattern), h("label", {}, "Or exact repositories", repos), h("button", { class: "primary" }, "Save"));

  page("Registry users", "Each user can push and pull the repositories matching its regex or list.",
    created ? h("div", { class: "banner" },
      h("strong", {}, `Token for ${created.name} (shown once)`),
      h("div", { class: "token mono" }, created.token),
      h("pre", {}, `podman login -u ${created.name} ${location.hostname.replace(/^vops\./, "registry.")}`)) : null,
    h("div", { class: "panel" }, h("table", {},
      h("tr", {}, h("th", {}, "Name"), h("th", {}, "Regex"), h("th", {}, "Repositories"), h("th", {}, "Created"), h("th", {})),
      users.map((u) => h("tr", {},
        h("td", { class: "mono" }, u.name),
        h("td", { class: "mono" }, u.pattern || h("span", { class: "muted" }, "—")),
        h("td", { class: "mono small" }, (u.repos || []).join(", ") || h("span", { class: "muted" }, "—")),
        h("td", { class: "muted" }, ago(u.created_at)),
        h("td", {}, h("div", { class: "row", style: "justify-content:end" },
          h("button", { onclick: () => { name.value = u.name; pattern.value = u.pattern; repos.value = (u.repos || []).join(", "); name.focus(); } }, "Edit"),
          h("button", { onclick: () => confirm(`New token for ${u.name}? The old one stops working.`) && save({ name: u.name, pattern: u.pattern, repos: u.repos, new_token: true }) }, "New token"),
          h("button", { class: "danger", onclick: async () => {
            if (!confirm("Delete " + u.name + "?")) return;
            await api("DELETE", "/users?name=" + enc(u.name));
            usersPage();
          } }, "Delete")))),
      ))),
    h("h2", {}, "Add or edit"), h("div", { class: "panel pad" }, form));
}

// ---- events

function eventTable(evs) {
  if (!evs.length) return h("p", { class: "muted" }, "No events.");
  return h("div", { class: "panel" }, h("table", {}, evs.map((e) => h("tr", {},
    h("td", { class: "muted small", title: new Date(e.at * 1000).toLocaleString() }, ago(e.at)),
    h("td", {}, h("span", { class: "tag" + (e.kind === "error" ? " bad" : "") }, e.kind)),
    h("td", {}, e.project ? h("a", { href: "#/p/" + enc(e.project) }, e.project) : ""),
    h("td", {}, e.message)))));
}

async function eventsPage() {
  try {
    const [evs, audit] = await Promise.all([api("GET", "/events"), api("GET", "/audit?n=200")]);
    page("Events", "Deploys, config changes, pushes and logins.", eventTable(evs),
      h("h2", {}, "Audit log"), h("p", { class: "muted small" }, "Every write to the host's database, recorded by sqlite triggers. Secrets are never recorded."),
      audit.length ? h("div", { class: "panel" }, h("table", {}, audit.map((a) => h("tr", {},
        h("td", { class: "muted small", title: new Date(a.at * 1000).toLocaleString() }, ago(a.at)),
        h("td", { class: "mono small" }, a.tbl),
        h("td", {}, h("span", { class: "tag" }, a.op)),
        h("td", { class: "mono small" }, a.key),
        h("td", { class: "small" }, a.detail))))) : h("p", { class: "muted" }, "Empty."));
  } catch (e) { failed(e); }
}

// ---- router

function route() {
  stopCurrent();
  stopCurrent = () => {};
  document.getElementById("top").hidden = false;
  const [path, query] = location.hash.slice(1).split("?");
  const params = new URLSearchParams(query || "");
  for (const a of document.querySelectorAll("nav a")) {
    const target = a.getAttribute("href").slice(1);
    a.classList.toggle("active", target === "/" ? path === "/" || path === "" || path.startsWith("/p/") || path.startsWith("/logs/") : path.startsWith(target));
  }
  let m;
  if ((m = path.match(/^\/p\/(.+)$/))) return projectPage(decodeURIComponent(m[1]));
  if ((m = path.match(/^\/logs\/(.+)$/))) return logsPage(decodeURIComponent(m[1]), params.get("service"));
  if (path === "/files") return filesPage(params);
  if (path === "/registry") return registryPage();
  if (path === "/users") return usersPage();
  if (path === "/events") return eventsPage();
  return overview();
}

window.addEventListener("hashchange", route);
route();
