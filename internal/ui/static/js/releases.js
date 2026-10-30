// Releases (x-vops.release manual or canary): pushes waiting for an admin, and their timeline nodes.
import { h, put } from "./lib/dom.js";
import { api } from "./lib/api.js";
import { short, fmtUnix, localInput } from "./lib/format.js";
import { projectHref } from "./model.js";
import { refresh } from "./state.js";
import { toast, when, mono, alertBox, confirmDialog, streamDialog } from "./ui.js";

const relImage = (r) => [mono(r.repo + ":" + r.tag), h("span", { class: "faint mono" }, " @" + short(r.digest, 12))];
const RELEASE_STATE = { pending: "warn", approved: "ok", rejected: "bad", failed: "bad", superseded: "" };

// holdTag: a service keeping what it runs while its tag points at a push nobody approved
export function holdTag(s, style) {
  const x = s.hold;
  if (!x) return null;
  const r = x.release;
  const text = r && r.status === "pending" ? "waiting approval · #" + r.id : r ? "#" + r.id + " " + r.status : "held";
  return h("span", { class: "tag warn", style, title: `${x.image} points at ${short(x.pushed, 12)}; runs ${x.running ? short(x.running, 12) : "nothing yet"} until a release is approved` }, text);
}



function releaseRow(r) {
  const approve = async () => (await confirmDialog({ title: `Approve #${r.id}?`, ok: "Approve",
    body: [h("p", {}, "Deploys " + r.repo + ":" + r.tag + " " + short(r.digest, 12) + " to " + r.project + " now (rolling)" + (r.mode === "canary" ? ", then removes the canary." : "."))] })) && streamDialog({ title: `Approving #${r.id} in ${r.project}`, path: "/releases/approve", body: { id: r.id },
    done: (ok) => { if (ok) toast("Approved #" + r.id); refresh(); } });
  const reject = async () => {
    if (!(await confirmDialog({ title: `Reject #${r.id}?`, ok: "Reject", danger: true,
      body: [h("p", {}, r.repo + ":" + r.tag + " " + short(r.digest, 12) + " won't be deployed. The services keep what they run until a newer push of the tag.")] }))) return;
    streamDialog({ title: `Rejecting #${r.id}`, path: "/releases/reject", body: { id: r.id },
      done: (ok) => { if (ok) toast("Rejected #" + r.id); refresh(); } });
  };
  let inputValue = localInput(r.approve_at || Math.floor(Date.now() / 1000) + 3600);
  const at = h("input", { type: "datetime-local", value: inputValue, "aria-label": "approve at" });
  const schedule = async (unix) => {
    try {
      await api("POST", "/releases/schedule", { id: r.id, at: unix });
      toast(unix ? "#" + r.id + " will be approved " + fmtUnix(unix) : "Schedule cancelled");
      refresh();
    } catch (e) { toast(e.message); }
  };
  const later = h("form", { class: "row", onsubmit: (e) => { e.preventDefault(); const t = Math.floor(new Date(at.value).getTime() / 1000); if (t) schedule(t); } },
    at, h("button", { class: "btn sm" }, "Approve at"));
  const scheduled = h("div", { class: "small" });
  const pushed = h("span", { class: "muted small" });
  const el = h("div", { class: "release" },
    h("div", { class: "row wrap" }, h("strong", {}, "#" + r.id), h("a", { href: projectHref(r.project) }, r.project), relImage(r),
      h("span", { class: "tag" + (r.mode === "canary" ? " accent" : "") }, r.mode === "canary" ? "canary " + r.weight + "%" : "manual"),
      pushed),
    scheduled,
    h("div", { class: "row wrap" }, h("button", { class: "btn primary sm", onclick: approve }, "Approve now"), later, h("button", { class: "btn danger sm", onclick: reject }, "Reject")));
  const update = (next) => {
    if (next.approve_at !== r.approve_at) {
      const value = localInput(next.approve_at || Math.floor(Date.now() / 1000) + 3600);
      if (at.value === inputValue && document.activeElement !== at) at.value = value;
      inputValue = value;
    }
    r = next;
    put(pushed, "pushed by " + (r.pushed_by || "?") + " ", when(r.created_at));
    scheduled.hidden = !r.approve_at;
    put(scheduled, r.approve_at ? ["Approved automatically " + fmtUnix(r.approve_at) + " (scheduled by " + r.decided_by + ") ", h("button", { class: "linkbtn", onclick: () => schedule(0) }, "Cancel")] : []);
  };
  update(r);
  return { el, update };
}

// releasesPanel lists the pending releases (of one project, or all of them)
export function releasesPanel(path) {
  const title = h("span", {}), list = h("div", { class: "releases" }), rows = new Map();
  const el = alertBox("warn", title,
    h("div", { class: "releases" }, h("span", { class: "small muted" }, "Approve deploys exactly that digest (rolling); Reject keeps what runs until a newer push. Nothing else deploys it, not even an apply."), list));
  return { el, update(st) {
    const rs = (st.releases || []).filter((r) => !path || r.project === path);
    el.hidden = !rs.length;
    title.textContent = rs.length === 1 ? "A push waits for approval" : rs.length + " pushes wait for approval";
    for (const [id, row] of rows) {
      if (!rs.some((r) => r.id === id)) { row.el.remove(); rows.delete(id); }
    }
    rs.forEach((r, i) => {
      let row = rows.get(r.id);
      if (!row) { row = releaseRow(r); rows.set(r.id, row); list.insertBefore(row.el, list.children[i] || null); }
      row.update(r);
    });
  } };
}

export function releaseNode(r, at) {
  return h("li", { class: "tl-node release" + (r.status === "rejected" || r.status === "failed" ? " failed" : "") },
    h("span", { class: "tl-dot" }),
    h("div", { class: "tl-body" },
      h("div", { class: "tl-head" }, h("strong", {}, ["Push ", h("span", { class: "faint" }, "r#" + r.id)]), h("span", { class: "tag " + (RELEASE_STATE[r.status] || "") }, r.status),
        h("span", { class: "tag" + (r.mode === "canary" ? " accent" : "") }, r.mode === "canary" ? "canary " + r.weight + "%" : "manual"),
        h("span", { class: "spacer" }), h("span", { class: "muted small" }, when(at))),
      h("div", { class: "tl-line small" }, relImage(r), h("span", { class: "muted" }, " pushed by " + (r.pushed_by || "?"))),
      r.status !== "pending" && (r.decided_by || r.note) ? h("div", { class: "small muted wrapany" }, r.status, r.decided_by ? " by " + r.decided_by : "", r.decided_at ? [" ", when(r.decided_at)] : "", r.note ? ": " + r.note : "") : null));
}
