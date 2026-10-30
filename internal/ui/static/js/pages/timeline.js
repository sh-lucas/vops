// Timeline tab: the history of a project as nodes (deploys, rollbacks, snapshots, releases); a node's snapshot
// is always the data right before it happened, so "restore" and "preview from here" start there.
import { h, put, icon } from "../lib/dom.js";
import { api } from "../lib/api.js";
import { ago, enc, fullDate, short, imgName } from "../lib/format.js";
import { projectHref } from "../model.js";
import { refresh } from "../state.js";
import { toast, when, mono, commitLine, panel, loading, alertBox, errorBox, busy, modal, confirmDialog, streamDialog } from "../ui.js";
import { openPlan } from "../plan.js";
import { releaseNode } from "../releases.js";
import { previewFrom } from "./previews.js";

// the code note of a rollback: the data's commit vs the running one, and the git commands when they differ
function codeNote(path, dataCommit, now) {
  if (!dataCommit || !now || dataCommit === now) return h("p", { class: "small" }, "Compose and code are not touched", dataCommit ? ["; this data belongs to the commit running now (", mono(short(dataCommit, 8)), ")."] : ".");
  return [h("p", { class: "small" }, "Compose and code are not touched: this data belongs to ", mono(short(dataCommit, 8)), ", while ", h("strong", {}, path), " runs ", mono(short(now, 8)), ". To run that code too, revert it in git and sync:"),
    h("pre", { class: "cmd" }, `git restore --source=${short(dataCommit)} --staged --worktree -- ${path}/\ngit commit -m "${path}: code back to ${short(dataCommit, 8)}"\nvops sync`)];
}

const runRollback = (ctx, title, body) => streamDialog({ title, path: "/rollback", body: { project: ctx.path, ...body },
  done: (ok) => { if (ok) toast("Rolled back"); ctx.reload && ctx.reload(); refresh(); } });

// restoreSnapshot: data only, from a snapshot node (manual snapshots and the like)
async function restoreSnapshot(ctx, snap) {
  const { path } = ctx;
  if (!(await confirmDialog({ title: `Restore ${path} data to #${snap.id}?`, ok: "Restore data", danger: true, body: [
    h("p", {}, "Puts back snapshot #" + snap.id + " (", snap.reason, ", taken ", when(snap.created_at), "). Images are not touched."),
    "Its containers stop for a few seconds while the data is put back, then start again.",
    "The current data is snapshotted first, so this can be undone.",
    codeNote(path, snap.commit, ctx.p.commit)] }))) return;
  runRollback(ctx, `Restoring ${path} to #${snap.id}`, { snapshot: snap.id });
}

// rollbackDialog: "Roll back…" on a deploy (or "Undo…" on a rollback): back to right before it. Images per
// service (pinned by digest), data for the whole project; what the server can't do is shown disabled with why.
export async function rollbackDialog(ctx, before) {
  const { path } = ctx;
  let rp;
  try { rp = await api("GET", "/rollback?project=" + enc(path) + "&before=" + before); } catch (e) { return toast(e.message); }
  const img = (x) => x ? h("span", { class: "mono", title: x.image + (x.digest ? "@" + x.digest : "") }, imgName(x.image.split("@")[0]), x.digest ? h("span", { class: "faint" }, " @" + short(x.digest, 7)) : null) : h("span", { class: "faint" }, "none");
  const boxes = [];
  const rows = rp.images.map((st) => {
    const can = st.do === "pin" || st.do === "unpin";
    const cb = h("input", { type: "checkbox", checked: st.selected, disabled: !can, "aria-label": st.service });
    if (can) boxes.push([st.service, cb]);
    const why = st.do === "skip" ? h("div", { class: "small warn-text" }, st.reason) : st.do === "same" ? h("div", { class: "small faint" }, "unchanged") : st.do === "unpin" ? h("div", { class: "small faint" }, "back to what compose says (unpinned)") : null;
    return h("label", { class: "rb-row" + (can ? "" : " off") }, cb, h("div", {}, h("span", { class: "mono" }, st.service), " ", st.do === "same" ? img(st.now) : [img(st.now), " → ", img(st.to)], why));
  });
  const s = rp.snapshot;
  const dataBox = h("input", { type: "checkbox", checked: rp.parts.includes("data"), disabled: !s, "aria-label": "data" });
  const submit = h("button", { class: "btn danger solid" }, "Roll back");
  const sync = () => { submit.disabled = !dataBox.checked && !boxes.some(([, cb]) => cb.checked); };
  [dataBox, ...boxes.map(([, cb]) => cb)].forEach((cb) => cb.addEventListener("change", sync));
  sync();
  const title = rp.undoes ? `Undo rollback #${rp.undoes}` : `Roll ${path} back to before #${before}`;
  const d = h("dialog", { class: "modal" }, h("form", { onsubmit: (e) => {
    e.preventDefault();
    d.close();
    const services = boxes.filter(([, cb]) => cb.checked).map(([svc]) => svc);
    runRollback(ctx, title, { before, images: services.length > 0, services, data: dataBox.checked });
  } },
    h("div", { class: "modal-head" }, h("h3", {}, title)),
    h("div", { class: "modal-body" },
      h("p", { class: "small" }, "Back to right before ", rp.undoes ? "this rollback" : "deploy #" + before, rp.from ? ": the images deploy #" + rp.from + " left running and the data from just before." : "."),
      h("h4", { class: "rb-h" }, "Images"),
      rp.no_images ? h("p", { class: "small warn-text" }, rp.no_images) : rows,
      rp.images.some((st) => st.do === "pin") ? h("p", { class: "small" }, "Rolling release, no downtime. Pinned services keep these images until a newer version is pushed, compose changes their image, or you unpin them.") : null,
      h("h4", { class: "rb-h" }, "Data"),
      h("label", { class: "rb-row" + (s ? "" : " off") }, dataBox, h("div", {},
        s ? ["Snapshot #" + s.id + " (" + s.reason + ", ", when(s.created_at), ")", h("div", { class: "small" }, "The project stops for a few seconds while it is put back; the current data is snapshotted first, so this can be undone.")]
          : h("span", { class: "warn-text small" }, rp.no_data || "no snapshot for this point"))),
      s ? codeNote(path, s.commit, rp.commit) : null),
    h("div", { class: "modal-foot" }, h("button", { type: "button", class: "btn", onclick: () => d.close() }, "Cancel"), submit)));
  d.addEventListener("close", () => d.remove());
  modal(d);
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

const exportHref = (s) => "/api/snapshots/export?id=" + s.id + "&project=" + enc(s.project);

export function timelineTab(ctx, box) {
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
      if (n.kind === "release") return releaseNode(n.release, n.at);
      const dep = n.deploy, s = n.snapshot;
      let title, cls = n.kind;
      if (n.kind === "deploy") {
        title = ["Deploy ", h("span", { class: "faint" }, "#" + dep.id)];
        if (dep.result === "failed") cls += " failed";
      } else if (n.kind === "rollback") {
        title = dep.undoes ? ["Undo of ", h("span", { class: "faint" }, "#" + dep.undoes)] : [dep.before_id ? "Rolled back to before #" + dep.before_id + " " : "Data restored ", h("span", { class: "faint" }, "#" + dep.id)];
        if (dep.result === "failed") cls += " failed";
      } else title = [s.reason === "manual" ? "Snapshot" : s.reason + " snapshot", " ", h("span", { class: "faint" }, "#" + s.id)];
      const restore = dep ? h("button", { class: "btn sm", title: n.kind === "rollback" ? "Back to right before this rollback" : "Back to right before this deploy: its images, its data",
        onclick: () => rollbackDialog(ctx, dep.id) }, n.kind === "rollback" ? [icon("undo"), "Undo…"] : [icon("undo"), "Roll back…"])
        : s ? h("button", { class: "btn sm", title: "Put back this snapshot's data", onclick: () => restoreSnapshot(ctx, s) }, "Restore data") : null;
      const download = n.kind === "snapshot" ? h("a", { class: "btn sm", href: exportHref(s), download: "", title: "This snapshot as a .tar.gz (vops snapshot import restores it, here or on another host)" }, icon("download"), "Download") : null;
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
          n.kind === "rollback" ? h("div", { class: "small" }, (dep.parts || "data").split(",").join(" and "), dep.restored_id ? [" · put back snapshot #" + dep.restored_id, restoredFrom(dep.restored_id)] : null, h("span", { class: "muted" }, " · code unchanged")) : null,
          n.kind === "rollback" && dep.parts && dep.parts.includes("images") && n.changes.length ? h("div", { class: "tl-changes" }, n.changes.map(changeLine)) : null,
          h("div", { class: "tl-line" }, n.commit ? commitLine(n.commit, t.subjects) : h("span", { class: "faint small" }, "no commit")),
          n.kind === "deploy" && n.changes.length ? h("div", { class: "tl-changes" }, n.changes.map(changeLine)) : null,
          n.gone.length ? h("div", { class: "small faint", title: "Registry gc keeps the images of the newest image_keep deploys (vops.yml)" }, "image gone from the registry: ", n.gone.join(", ")) : null,
          n.kind === "deploy" && !n.changes.length && dep.summary ? h("div", { class: "muted small" }, dep.summary) : null,
          dep && dep.error ? h("div", { class: "small error wrapany" }, dep.error) : null,
          s && n.kind === "snapshot" && s.note ? h("div", { class: "small muted wrapany" }, "“" + s.note + "”") : null,
          h("div", { class: "tl-foot" }, snapBadge(n), chips(n), h("span", { class: "spacer" }),
            h("div", { class: "actions" }, restore,
              h("button", { class: "btn sm", onclick: () => previewFrom(ctx, n, t), title: "A preview of this point: its commit and images" + (s ? ", on a copy of snapshot #" + s.id : ", on a copy of the current data") }, icon("git"), "Preview from here"),
              download, del))));
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
    // backups: download = a manual snapshot, then its .tar.gz; upload = a new snapshot, then the usual restore dialog
    const off = !d.supported ? d.reason : !d.protected.length ? "no data to back up" : null;
    const dlBtn = h("button", { class: "btn sm", disabled: !!off, title: off || "Takes a snapshot now and downloads it as a .tar.gz" }, icon("download"), "Download current data");
    dlBtn.onclick = () => busy(dlBtn, async () => {
      const r = await api("POST", "/snapshots", { project: path, note: "downloaded" });
      location.href = exportHref(r.snapshot);
      load();
    });
    const file = h("input", { type: "file", accept: ".gz,.tgz,application/gzip", hidden: true });
    const upBtn = h("button", { class: "btn sm", disabled: !!off, title: off || "A .tar.gz from Download or vops snapshot export: it becomes a snapshot you can restore", onclick: () => file.click() }, "Upload backup");
    file.onchange = () => {
      const f = file.files[0];
      file.value = "";
      if (f) busy(upBtn, async () => {
        const r = await api("POST", "/snapshots/import?project=" + enc(path), f);
        toast("Uploaded as snapshot #" + r.snapshot.id);
        await load();
        restoreSnapshot(ctx, r.snapshot);
      });
    };
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
          h("span", { class: "spacer" }), take, h("div", { class: "actions" }, dlBtn, upBtn, file))));
    put(box,
      !d.supported ? alertBox("warn", "Snapshots unavailable", h("span", { class: "small" }, d.reason + ". Deploys are still recorded; restoring data and branching from a point need snapshots.")) : null,
      d.unprotected && d.unprotected.length ? alertBox("warn", "Not covered by snapshots", h("span", { class: "small" }, "Not btrfs subvolumes: ", mono(d.unprotected.join(", ")))) : null,
      panel(h("ol", { class: "tl" }, now, t.nodes.map(node)),
        t.nodes.length ? null : h("div", { class: "panel-foot muted small" }, "No history yet: every apply that changes this project is recorded here, with a snapshot of its data taken right before.")));
  };
  ctx.reload = load;
  load();
}
