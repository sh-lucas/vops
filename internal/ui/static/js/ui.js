// Shared components. Pages build with these and the classes in style.css, nothing else.
import { h, put, icon } from "./lib/dom.js";
import { stream, preSink } from "./lib/api.js";
import { ago, fullDate, short } from "./lib/format.js";
import { health, stateOf } from "./model.js";

export function toast(msg) {
  const t = h("div", { class: "toast", role: "status" }, msg);
  document.getElementById("toasts").append(t);
  setTimeout(() => t.remove(), 3500);
}

export const when = (u) => h("span", { class: "nowrap", title: u ? fullDate(u) : null }, ago(u));
export const mono = (s) => h("span", { class: "mono" }, s);
export const sep = () => h("span", { class: "faint" }, "·");
export const commitLine = (sha, subjects) => h("span", { class: "cline", title: sha || null }, icon("commit"), mono(short(sha, 8) || "?"), subjects && subjects[sha] ? h("span", { class: "subject" }, subjects[sha]) : null);

// ---- page structure

const $app = () => document.getElementById("app");
export const page = (...children) => put($app(), ...children);
export const setFill = (fill) => { $app().className = fill ? "fill" : ""; }; // full height pages (logs)

export function head({ title, crumbs, sub, actions, dot, pill }) {
  return h("div", { class: "page-head" },
    h("div", { class: "titles" },
      crumbs ? h("div", { class: "crumb" }, crumbs) : null,
      h("h1", {}, dot, h("span", { class: "t" }, title), pill),
      sub ? h("div", { class: "sub" }, sub) : null),
    actions ? h("div", { class: "actions" }, actions) : null);
}
export const panel = (...c) => h("div", { class: "panel" }, ...c);
export const panelHead = (...c) => h("div", { class: "panel-head" }, ...c);
export const section = (title, hint, ...c) => h("section", { class: "section" }, h("div", { class: "section-head" }, h("h2", {}, title), hint ? h("span", { class: "hint" }, hint) : null), ...c);
export const empty = (title, hint, action) => h("div", { class: "empty" }, h("strong", {}, title), hint ? h("div", { class: "small" }, hint) : null, action ? h("div", { class: "empty-action" }, action) : null);
export const loading = (text = "Loading…", cls = "") => h("div", { class: "loading " + cls }, h("span", { class: "spin" }), text);

// table: on small screens each row becomes a card; data-label names each cell there
export function table(headers, rows, cls) {
  const body = h("tbody", {}, rows);
  for (const tr of body.rows) [...tr.cells].forEach((td, i) => { if (headers[i] && td.colSpan === 1) td.dataset.label = headers[i]; });
  return h("div", { class: "table-wrap" }, h("table", { class: "cards " + (cls || "") }, h("thead", {}, h("tr", {}, headers.map((x) => h("th", {}, x)))), body));
}

export function alertBox(kind, title, body, action) {
  const ic = kind === "info" ? "info" : kind === "ok" ? "check" : "alert";
  return h("div", { class: "alert " + kind, role: kind === "info" || kind === "ok" ? null : "alert" }, icon(ic),
    h("div", { class: "body" }, title ? h("strong", {}, title) : null, body), action || null);
}
export const errorBox = (e) => alertBox("bad", "Something went wrong", h("span", { class: "small wrapany" }, e.message));
export function failed(e, alive) {
  if (e.message === "login required" || (alive && !alive())) return;
  page(errorBox(e));
}

// ---- status

export const healthDot = (p, cls = "") => { const x = health(p); return h("span", { class: "dot " + x.kind + " " + cls, title: x.why }); };
export const statusPill = (p) => { const x = health(p); return h("span", { class: "pill " + x.kind, title: x.why }, h("span", { class: "dot " + x.kind }), x.label); };
export const projectTags = (p) => [
  p.gone && !p.disabled ? h("span", { class: "tag bad" }, "removed from git") : null,
];
export function dots(containers) {
  return h("span", { class: "dots" }, containers.length ? containers.map((c) => h("span", { class: "dot " + stateOf(c), title: c.Names[0] + ": " + c.Status })) : h("span", { class: "dot", title: "no containers" }));
}

// ---- actions

// busy runs fn with the button disabled and spinning; errors become toasts.
export async function busy(btn, fn) {
  btn.disabled = true;
  btn.classList.add("busy");
  try { return await fn(); } catch (x) { if (x.message !== "login required") toast(x.message); } finally {
    btn.disabled = false;
    btn.classList.remove("busy");
  }
}

// modal opens a <dialog> and removes it once closed
export function modal(d) {
  document.body.append(d);
  d.showModal();
  return d;
}

// confirmDialog replaces confirm(): states the consequences, resolves true on confirm. Cancel has focus, Escape cancels.
export function confirmDialog({ title, body, ok = "Confirm", danger = false }) {
  return new Promise((resolve) => {
    const d = h("dialog", { class: "modal" }, h("form", { method: "dialog" },
      h("div", { class: "modal-head" }, h("h3", {}, title)),
      h("div", { class: "modal-body" }, [body].flat().map((x) => (typeof x === "string" ? h("p", {}, x) : x))),
      h("div", { class: "modal-foot" }, h("button", { class: "btn", value: "cancel" }, "Cancel"), h("button", { class: "btn " + (danger ? "danger solid" : "primary"), value: "ok" }, ok))));
    d.addEventListener("close", () => { resolve(d.returnValue === "ok"); d.remove(); });
    modal(d);
  });
}

// streamDialog runs a streamed POST (rollback, preview up) in a modal that shows its progress; done(ok) runs at the end.
export function streamDialog({ title, intro, path, body, done }) {
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
  modal(d);
  const finish = (ok) => {
    running = false;
    result.replaceChildren(h("span", { class: "tag " + (ok ? "ok" : "bad") }, ok ? "Done" : "Failed"));
    done && done(ok);
  };
  stream(path, preSink(out), { method: "POST", body: JSON.stringify(body),
    done: () => finish(/==> ok\s*$/.test(out.textContent)),
    error: (msg) => { out.append("\n" + msg); finish(false); } });
}
