// Logs: the project tab and the full-page view.
import { h, icon } from "../lib/dom.js";
import { api, errText, stream } from "../lib/api.js";
import { enc, fmtDate, fmtUnix, localInput, pad } from "../lib/format.js";
import { projectHref } from "../model.js";
import { getStatus } from "../state.js";
import { onLeave } from "../scope.js";
import { failed, head, healthDot, page, setFill, toast } from "../ui.js";

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

// date + time as two inputs; .value is "YYYY-MM-DDTHH:mm" like datetime-local, an empty time falls back to def
function dtField(def, onchange) {
  const d = h("input", { type: "date", "aria-label": "date", onchange });
  const t = h("input", { type: "time", "aria-label": "time", onchange });
  t.title = "empty = " + def;
  return { el: h("span", { class: "dtf" }, d, t), get value() { return d.value ? d.value + "T" + (t.value || def) : ""; },
    set value(v) { [d.value, t.value] = v ? v.split("T") : ["", ""]; } };
}

export function logView({ path, service, services, hrefFor }) {
  const maxLines = 5000;
  const narrow = matchMedia("(max-width: 700px)").matches;
  const view = h("div", { class: "logview" + (narrow ? " wrap" : ""), tabindex: "0" });
  const lines = view.getElementsByClassName("ll");
  const grep = h("input", { type: "search", class: "search", placeholder: "Search all history", "aria-label": "search" });
  const from = dtField("00:00", () => onRange());
  const to = dtField("23:59", () => onRange());
  const clear = h("button", { type: "button", class: "btn sm", hidden: true, onclick: () => { from.value = to.value = ""; follow.checked = true; onRange(); } }, "Clear");
  const follow = h("input", { type: "checkbox", checked: true, onchange: () => start() });
  const wrap = h("input", { type: "checkbox", checked: narrow, onchange: () => view.classList.toggle("wrap", wrap.checked) });
  const status = h("span", {}), avail = h("span", {}), loaded = h("span", {});
  const more = h("button", { type: "button", class: "linkbtn", hidden: true, onclick: () => older() }, "load older");
  const svcNames = service && !services.includes(service) ? [...services, service] : services;
  const svcSel = svcNames.length > 1 || service ? h("select", { "aria-label": "service", onchange: (e) => { location.hash = hrefFor(e.target.value); } },
    h("option", { value: "" }, "All services"), svcNames.map((s) => h("option", { value: s, selected: s === service }, s))) : null;
  let cancel = () => {}, before = null, busyOlder = false, gen = 0, partial = "", dropped = false, metaQueued = false;

  // from/to are local time, minute precision; "to" includes its whole minute
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
  const fmt = h("select", { "aria-label": "download format" }, h("option", { value: "txt" }, ".txt"), h("option", { value: "csv" }, ".csv"));
  const csvCell = (v) => '"' + v.replaceAll('"', '""') + '"';
  const csvRow = (t) => { // time, service, message
    let m;
    if ((m = JLINE.exec(t))) return [m[1], m[2], t.slice(m[0].length)];
    if ((m = PLINE.exec(t))) return [m[2], m[1], t.slice(m[0].length)];
    return ["", "", t];
  };
  const download = () => {
    if (!lines.length) return toast("Nothing to download");
    const csv = fmt.value === "csv";
    const text = (csv ? ["time,service,message\n"] : []).concat(Array.from(lines, (l) => (csv ? csvRow(l.textContent).map(csvCell).join(",") : l.textContent) + "\n")).join("");
    const d = new Date();
    const name = `${path.replaceAll("/", "-")}${service ? "-" + service : ""}-${d.getFullYear()}${pad(d.getMonth() + 1)}${pad(d.getDate())}-${pad(d.getHours())}${pad(d.getMinutes())}.${fmt.value}`;
    const a = h("a", { href: URL.createObjectURL(new Blob([text], { type: csv ? "text/csv;charset=utf-8" : "text/plain;charset=utf-8" })), download: name });
    document.body.append(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(a.href), 2000);
  };

  const el = h("div", { class: "grow" },
    h("form", { class: "logbar", onsubmit: (e) => { e.preventDefault(); start(); } },
      svcSel, grep,
      h("div", { class: "row end" },
        h("label", { class: "check" }, follow, "follow"),
        h("label", { class: "check" }, wrap, "wrap"),
        h("button", { class: "btn" }, icon("refresh"), "Reload"),
        h("button", { type: "button", class: "btn", title: "Download the lines in view", onclick: download }, icon("download"), "Download"), fmt),
      h("div", { class: "row daterow" }, h("label", { class: "dt" }, "from", from.el), h("label", { class: "dt" }, "to", to.el), clear)),
    h("div", { class: "logmeta" }, avail, loaded, more, h("span", { class: "spacer" }), status),
    view);
  return { el, start, stop: () => { cancel(); gen++; } };
}

export async function logsPage(alive, path, service) {
  setFill(true);
  let st;
  try { st = await getStatus(); } catch (e) { return failed(e, alive); }
  if (!alive()) return;
  const q = service ? "?service=" + enc(service) : "";
  // a preview (shop@pr-42) has no status entry: its services come from the previews api, and "back" is its project's Previews tab
  const [base, preview] = path.split("@");
  const p = st.projects.find((x) => x.path === path);
  let services = p ? p.services.map((s) => s.name) : [];
  if (preview) {
    try { services = ((await api("GET", "/previews?project=" + enc(base))).find((x) => x.name === preview) || { services: [] }).services.map((s) => s.name); } catch { /* no services: the selector shows all */ }
    if (!alive()) return;
  }
  const lv = logView({ path, service, services, hrefFor: (svc) => "#/logs/" + enc(path) + (svc ? "?service=" + enc(svc) : "") });
  page(head({ title: [path, service ? " / " + service : ""], crumbs: preview ? "Preview logs · " : "Logs · ", dot: p ? healthDot(p, "lg") : null,
    actions: h("a", { class: "btn", href: preview ? projectHref(base, "previews") : projectHref(path, "logs", q) }, icon("back"), preview ? "Previews" : "Project") }), lv.el);
  lv.start();
  onLeave(lv.stop);
}
