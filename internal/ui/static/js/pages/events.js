// Events: host-wide, per project (a tab), and the audit log.
import { h, put } from "../lib/dom.js";
import { api } from "../lib/api.js";
import { enc } from "../lib/format.js";
import { projectHref } from "../model.js";
import { when, page, head, panel, empty, loading, table, errorBox, failed, busy } from "../ui.js";

const kindClass = (k) => (k === "error" ? " bad" : k === "rollback" || k === "restart" ? " warn" : k === "push" || k === "snapshot" ? " accent" : "");
function eventList(evs, withProject = true) {
  if (!evs.length) return panel(empty("No events yet", "Deploys, config changes, pushes and logins show up here."));
  return panel(table(["When", "Kind", withProject ? "Project" : null, "Message"].filter((x) => x !== null), evs.map((e) => h("tr", {},
    h("td", { class: "muted small" }, when(e.at)),
    h("td", {}, h("span", { class: "tag" + kindClass(e.kind) }, e.kind)),
    withProject ? h("td", { class: "nowrap" }, e.project ? h("a", { href: projectHref(e.project) }, e.project) : "") : null,
    h("td", { class: "wrapany" }, e.message)))));
}

const EVENTS_PAGE = 50;
// pagedEvents: the newest EVENTS_PAGE events, "Load more" appends the next page (by id, so new events don't shift it)
function pagedEvents(evs, query, withProject = true) {
  const box = h("div", {});
  let all = evs;
  const render = (more) => {
    const btn = more ? h("button", { class: "btn sm", onclick: () => busy(btn, async () => {
      const next = await api("GET", `/events?n=${EVENTS_PAGE}&before=${all[all.length - 1].id}${query}`);
      all = all.concat(next);
      render(next.length === EVENTS_PAGE);
    }) }, "Load more") : null;
    put(box, eventList(all, withProject), btn ? h("div", { class: "row", style: "margin-top:8px" }, btn) : null);
  };
  render(evs.length === EVENTS_PAGE);
  return box;
}

export async function eventsPage(alive) {
  let evs, audit;
  try { [evs, audit] = await Promise.all([api("GET", "/events?n=" + EVENTS_PAGE), api("GET", "/audit?n=200")]); } catch (e) { return failed(e, alive); }
  if (!alive()) return;
  page(head({ title: "Events", sub: "Deploys, config changes, pushes and logins." }), pagedEvents(evs, ""),
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

export async function eventsTab(ctx, box) {
  box.replaceChildren(loading());
  const q = "&project=" + enc(ctx.path);
  try { box.replaceChildren(pagedEvents(await api("GET", "/events?n=" + EVENTS_PAGE + q), q, false)); } catch (e) { box.replaceChildren(errorBox(e)); }
}
