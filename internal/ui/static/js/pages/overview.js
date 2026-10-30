// Overview: one answer first (is everything fine?), then what needs you, then every project as a card.
import { h, put, icon } from "../lib/dom.js";
import { ago, fullDate, short, plural } from "../lib/format.js";
import { domainURL, health, previewsOf, projectHref, stateOf, treeRows } from "../model.js";
import { getStatus } from "../state.js";
import { watch } from "../scope.js";
import { alertBox, dots, empty, failed, head, healthDot, mono, page, section, sep, statusPill, when } from "../ui.js";
import { openPlan } from "../plan.js";
import { holdTag, releasesPanel } from "../releases.js";

export async function overview(alive) {
  let st;
  try { st = await getStatus(); } catch (e) { return failed(e, alive); }
  if (!alive()) return;
  const top = h("div", {}), bottom = h("div", {}), releases = releasesPanel();
  page(top, releases.el, bottom);
  const render = (st) => {
    put(top,
      head({ title: "Overview", sub: [st.domain || "no domain set in vops.yml", sep(), h("span", { title: "~/vops on the host is at this commit" }, "~/vops at ", mono(short(st.commit, 8) || "no commits yet"))] }),
      attention(st));
    releases.update(st);
    put(bottom, numbers(st), projects(st));
  };
  render(st);
  watch((s) => s.status, (st) => st && render(st));
}

// attention: "everything is fine", or the list of what isn't, each with where to go
function attention(st) {
  const items = [];
  const item = (kind, text, action) => items.push(h("li", { class: "need" }, h("span", { class: "dot " + kind }), h("div", { class: "what" }, text), action || null));
  for (const p of st.projects) {
    const x = health(p);
    if (x.kind === "bad") item("bad", [h("a", { href: projectHref(p.path) }, p.path), ": " + x.label.toLowerCase()], p.error ? null : h("a", { class: "btn sm", href: projectHref(p.path, "logs") }, "Logs"));
  }
  if (st.changes) item("warn", "The host differs from git: a push, env vars or enable/disable wait for an apply.", h("button", { class: "btn primary sm", onclick: openPlan }, "Review and apply"));
  // a pin is a temporary emergency state: always visible
  for (const p of st.projects) for (const s of p.services.filter((s) => s.pin))
    item("warn", [h("a", { href: projectHref(p.path) }, p.path + "/" + s.name), " is pinned to the image of #" + s.pin.deploy_id + " (rolled back); compose says ", mono(s.pin.compose_image)]);
  for (const w of st.warnings || []) item("warn", w);
  if (!items.length) {
    const n = st.projects.flatMap((p) => p.services.flatMap((s) => s.containers)).length;
    return alertBox("ok", "Everything is running", h("span", { class: "small muted" }, plural(st.projects.length, "project") + ", " + plural(n, "container") + ", in sync with git."));
  }
  return h("div", { class: "panel attention" }, h("div", { class: "panel-head" }, icon("alert"), h("h2", {}, items.length === 1 ? "1 thing needs you" : items.length + " things need you")), h("ul", { class: "needs-list" }, items));
}

function numbers(st) {
  const all = st.projects.flatMap((p) => p.services.flatMap((s) => s.containers));
  const running = all.filter((c) => c.State === "running").length;
  const down = all.filter((c) => !["running", "done"].includes(stateOf(c))).length;
  const last = st.projects.reduce((a, p) => (p.applied_at > (a ? a.applied_at : 0) ? p : a), null);
  const pvs = st.previews || [];
  const stat = (label, value, foot, cls = "", title) => h("div", { class: "stat " + cls, title }, h("div", { class: "label" }, label), h("div", { class: "value" }, value), h("div", { class: "foot" }, foot));
  return h("div", { class: "stats" },
    stat("Containers", [running, h("span", { class: "of" }, " / " + all.length)], down ? down + " not running" : "running", down ? "bad" : ""),
    stat("Last deploy", last ? ago(last.applied_at) : "never", last ? h("a", { href: projectHref(last.path, "timeline") }, last.path) : "", "", last ? fullDate(last.applied_at) : null),
    stat("Previews", pvs.length, pvs.length ? [...new Set(pvs.map((x) => x.project))].map((p, i) => [i ? ", " : "", h("a", { href: projectHref(p, "previews") }, p)]) : "none running"));
}

function projects(st) {
  if (!st.projects.length) return section("Projects", null, h("div", { class: "panel" }, empty("No projects yet", "Add a folder with a compose.yml to the repo and run vops sync.")));
  const cards = treeRows(st.projects).filter((r) => r.project).map(({ project: p }) => {
    const pvs = previewsOf(st, p.path).length;
    return h("article", { class: "pcard " + health(p).kind },
      h("div", { class: "pcard-head" }, healthDot(p), h("a", { class: "pname stretch", href: projectHref(p.path) }, p.path), h("span", { class: "spacer" }), statusPill(p)),
      p.services.length ? h("ul", { class: "pcard-svcs" }, p.services.map((s) => h("li", {},
        dots(s.containers), h("span", { class: "mono" }, s.name),
        s.domains.length ? h("a", { class: "dom", href: domainURL(st, s.domains[0]), target: "_blank", rel: "noopener", title: s.domains.join("\n") }, s.domains[0]) : null,
        holdTag(s)))) : h("div", { class: "small muted" }, p.error ? "Fix the compose file first." : "No services"),
      h("div", { class: "pcard-foot" }, p.commit ? ["applied ", mono(short(p.commit, 8)), " · ", when(p.applied_at)] : "never applied",
        pvs ? [h("span", { class: "spacer" }), h("a", { class: "tag accent", href: projectHref(p.path, "previews") }, icon("git"), plural(pvs, "preview"))] : null));
  });
  return section("Projects", plural(st.projects.length, "project"), h("div", { class: "pgrid" }, cards));
}
