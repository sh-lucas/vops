// One project: header + tabs. Add a tab by adding an entry to TABS (and its id to model.TAB_IDS).
import { h, put, icon } from "../lib/dom.js";
import { api } from "../lib/api.js";
import { enc, shortImage, short } from "../lib/format.js";
import { TAB_ALIASES, domainURL, previewsOf, projectHref, stateOf } from "../model.js";
import { getStatus, refresh, refreshStatus } from "../state.js";
import { onLeave, watch } from "../scope.js";
import { alertBox, busy, confirmDialog, empty, failed, head, healthDot, mono, page, panel, projectTags, sep, setFill, statusPill, streamDialog, toast, when } from "../ui.js";
import { holdTag, releasesPanel } from "../releases.js";
import { rollbackDialog, timelineTab } from "./timeline.js";
import { previewsTab } from "./previews.js";
import { envTab } from "./env.js";
import { logView } from "./logs.js";
import { eventsTab } from "./events.js";

const TABS = [
  { id: "services", label: "Services", live: true, render: servicesTab, count: (ctx) => ctx.p.services.length },
  { id: "timeline", label: "Timeline", render: timelineTab },
  { id: "previews", label: "Previews", render: previewsTab, count: (ctx) => previewsOf(ctx.st, ctx.path).length },
  { id: "logs", label: "Logs", fill: true, render: logsTab },
  { id: "env", label: "Environment", render: envTab },
  { id: "events", label: "Events", render: eventsTab },
];

export async function projectPage(alive, path, tabId, params) {
  let st;
  try { st = await getStatus(); } catch (e) { return failed(e, alive); }
  if (!alive()) return;
  const find = (st) => st.projects.find((x) => x.path === path);
  if (!find(st)) return page(head({ title: path }), panel(empty("Unknown project", "Nothing at this path in ~/vops. It may have been removed from git.", h("a", { class: "btn", href: "#/" }, "Back to overview"))));
  const tab = TABS.find((t) => t.id === (TAB_ALIASES[tabId] || tabId)) || TABS[0];
  const ctx = { path, p: find(st), st, params, reload: null };
  const headBox = h("div", {}), alerts = h("div", {}), body = h("div", { class: tab.fill ? "grow" : "" }), tabs = h("nav", { class: "tabs", "aria-label": "Project" });
  const releases = releasesPanel(path);
  const renderHead = () => {
    headBox.replaceChildren(projectHead(ctx));
    put(alerts, ctx.p.error ? alertBox("bad", "Invalid compose file", h("pre", {}, ctx.p.error)) : null, restoredBanner(ctx));
    releases.update(ctx.st);
    tabs.replaceChildren(...TABS.map((t) => {
      const n = t.count ? t.count(ctx) : null;
      return h("a", { href: projectHref(path, t.id), class: t === tab ? "active" : null, "aria-current": t === tab ? "page" : null }, t.label, n ? h("span", { class: "count" }, n) : null);
    }));
  };
  setFill(tab.fill);
  page(headBox, alerts, releases.el, tabs, body);
  renderHead();
  tab.render(ctx, body);
  watch((s) => s.status, (st) => {
    const p = st && find(st);
    if (!p) return;
    Object.assign(ctx, { p, st });
    renderHead();
    if (tab.live) tab.render(ctx, body);
  });
}

function projectHead(ctx) {
  const { p, path } = ctx;
  const toggle = h("button", { class: "btn" + (p.disabled ? " primary" : ""), onclick: async () => {
    if (!p.disabled && !(await confirmDialog({ title: `Disable ${path}?`, ok: "Disable", danger: true,
      body: ["On the next apply its containers stop. Volumes, data and env vars stay, and you can enable it again anytime."] }))) return;
    await busy(toggle, async () => {
      await api("POST", "/project", { project: path, disabled: !p.disabled });
      toast(p.disabled ? "Enabled: apply to start it" : "Disabled: apply to stop it");
      await refreshStatus();
    });
  } }, p.disabled ? "Enable" : "Disable");
  return head({ title: path.split("/").pop(), dot: healthDot(p, "lg"), pill: statusPill(p),
    sub: [p.commit ? ["applied ", mono(short(p.commit, 8)), " · ", when(p.applied_at)] : "never applied", projectTags(p),
      p.limits ? [sep(), h("span", { title: "x-vops limits in the proxy" + (p.refused ? `; refused since it started: ${p.refused.limited} too many requests (429), ${p.refused.too_large} too large (413)` : "") }, p.limits)] : null],
    actions: [h("a", { class: "btn", href: "#/files?dir=" + enc(path) }, icon("file"), "Files"), toggle] });
}

// svcState: a service's containers in a few words
function svcState(cs) {
  if (!cs.length) return { kind: "idle", label: "No containers" };
  const n = (st) => cs.filter((c) => stateOf(c) === st).length;
  const up = n("running"), done = n("done"), down = cs.length - up - done;
  if (down) return { kind: "bad", label: down === cs.length ? "Down" : `${down} of ${cs.length} down` };
  if (done === cs.length) return { kind: "idle", label: "Finished" };
  return { kind: "ok", label: cs.length > 1 ? `Running · ${up}` : "Running" };
}

function servicesTab(ctx, box) {
  const { p, path, st } = ctx;
  if (!p.services.length) return box.replaceChildren(panel(empty("No services", p.error ? "Fix the compose file first." : "This project defines no services.")));
  box.replaceChildren(h("div", { class: "panel svc-list" }, p.services.map((s) => {
    const restart = s.containers.length ? h("button", { class: "btn sm", onclick: () => busy(restart, async () => {
      await api("POST", "/restart", { project: path, service: s.name });
      toast("Restarted " + s.name);
      await refreshStatus();
    }) }, "Restart") : null;
    const state = svcState(s.containers);
    // containers are listed only when they don't all run: otherwise the state says it all
    const odd = state.kind === "bad" ? s.containers.filter((c) => stateOf(c) !== "running") : [];
    return h("div", { class: "svc-row" },
      h("div", { class: "svc-main" },
        h("div", { class: "svc-title" }, h("strong", { class: "mono" }, s.name), h("span", { class: "pill " + state.kind, title: s.containers.map((c) => c.Names[0] + ": " + c.Status).join("\n") || null }, h("span", { class: "dot " + state.kind }), state.label),
          s.pending ? h("span", { class: "tag warn", title: s.reason || null }, "pending " + s.pending) : null,
          s.pin ? h("span", { class: "tag warn", title: "Rolled back: runs the image deploy #" + s.pin.deploy_id + " ran; compose says " + s.pin.compose_image }, "pinned · #" + s.pin.deploy_id) : null,
          holdTag(s)),
        s.domains.length ? h("div", { class: "svc-doms" }, s.domains.map((d) => h("a", { href: domainURL(st, d), target: "_blank", rel: "noopener" }, d, icon("external")))) : null,
        h("div", { class: "svc-img mono", title: s.image }, shortImage(s.image)),
        odd.map((c) => h("div", { class: "small error" }, mono(c.Names[0]), " ", c.Status)),
        canaryInfo(ctx, s)),
      h("div", { class: "actions" }, h("a", { class: "btn sm", href: projectHref(path, "logs", "?service=" + enc(s.name)) }, "Logs"), restart,
        s.pin ? h("button", { class: "btn sm", onclick: () => unpinService(ctx, s) }, "Unpin") : null));
  })));
}

// canaryInfo: a service's canary container, its share, age and 5xx next to the stable's, and links that put
// this browser on it (or back) on the real domain (?vops-canary=on|off sets the proxy's cookie)
function canaryInfo(ctx, s) {
  const c = s.canary;
  if (!c || c.action === "remove") return null;
  const stats = (ctx.p.canaries || {})[s.name];
  const errRate = (g) => g.requests ? `${g.errors} of ${g.requests} (${(100 * g.errors / g.requests).toFixed(1)}%)` : "no requests yet";
  const url = s.domains.length ? domainURL(ctx.st, s.domains[0]) + "/?vops-canary=" : null;
  return h("div", { class: "small canary" },
    h("span", { class: "tag accent" }, "canary · " + c.weight + "% · #" + c.release), " ", c.since ? ["since ", when(c.since)] : c.action,
    stats ? h("div", { class: "muted" }, "5xx: canary " + errRate(stats.canary) + ", stable " + errRate(stats.stable)) : null,
    url ? h("div", {}, h("a", { href: url + "on", target: "_blank", rel: "noopener", title: "Sets a cookie on that domain: this browser gets the canary" }, "Open the canary"), " · ",
      h("a", { href: url + "off", target: "_blank", rel: "noopener" }, "back to stable")) : null);
}

// after a rollback, until the next deploy: "Rolled back to before #N (images, data) · when · Undo"
function restoredBanner(ctx) {
  const d = ctx.p.last_deploy;
  if (!d || d.trigger !== "rollback" || d.result !== "ok" || d.undoes) return null;
  const parts = (d.parts || "data").split(",");
  const undo = h("button", { class: "btn sm", onclick: () => rollbackDialog(ctx, d.id) }, icon("undo"), "Undo…");
  return alertBox("warn", d.before_id ? `Rolled back to before #${d.before_id} (${parts.join(", ")})` : `Data restored to snapshot #${d.restored_id}`,
    h("span", { class: "small" }, when(d.finished_at), parts.includes("images") ? " · pinned services keep these images until a newer version is pushed" : " · code was not rolled back",
      " · shown until the next deploy of this project"), undo);
}

// unpin: back to what compose says (rolling)
async function unpinService(ctx, s) {
  if (!(await confirmDialog({ title: `Unpin ${s.name}?`, ok: "Unpin",
    body: [h("p", {}, "Runs ", mono(s.pin.compose_image), " again, as compose says, with a rolling release."),
      h("p", { class: "small" }, "It is pinned to the image deploy #" + s.pin.deploy_id + " ran (", mono(shortImage(s.image)), ").")] }))) return;
  streamDialog({ title: `Unpinning ${ctx.path}/${s.name}`, path: "/unpin", body: { project: ctx.path, services: [s.name] },
    done: (ok) => { if (ok) toast("Unpinned " + s.name); refresh(); } });
}

function logsTab(ctx, box) {
  const service = ctx.params.get("service");
  const lv = logView({ path: ctx.path, service, services: ctx.p.services.map((s) => s.name),
    hrefFor: (svc) => projectHref(ctx.path, "logs", svc ? "?service=" + enc(svc) : "") });
  box.replaceChildren(lv.el);
  lv.start();
  onLeave(lv.stop);
}
