// Previews tab and the dialogs that create previews.
import { h, put, icon } from "../lib/dom.js";
import { api } from "../lib/api.js";
import { ago, enc, short, imgName, pinned, until, fullDate } from "../lib/format.js";
import { domainURL, projectHref } from "../model.js";
import { refresh } from "../state.js";
import { toast, when, mono, commitLine, panel, panelHead, loading, alertBox, errorBox, dots, busy, modal, confirmDialog, streamDialog } from "../ui.js";

const nameRe = "[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?";
const dataWarning = () => alertBox("warn", "Previews hold a copy of production data",
  h("span", { class: "small" }, "Real personal data, and whatever the code does with it: queued emails, webhooks, scheduled charges. Previews never get production's env (Environment → Previews), but anything they don't redirect still goes out for real."));

// previewUp streams a preview creation from a body the dialogs built
function previewUp(ctx, body) {
  streamDialog({ title: `Preview ${body.name} of ${ctx.path}`, path: "/previews", body: { project: ctx.path, ...body },
    done: (ok) => {
      if (ok) toast("Preview " + body.name + " is up");
      refresh();
      ctx.reload && ctx.reload();
    } });
}

// previewFrom: "Preview from here" on a timeline node: its commit, its images (by digest), its snapshot
export function previewFrom(ctx, n, t) {
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
  modal(d);
  name.select();
}

// previews of this project: what runs, where it came from, when it expires
export function previewsTab(ctx, box) {
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
    put(formBox, panel(panelHead(h("h2", {}, "New preview"), h("span", { class: "muted small" }, "what " + path + "'s x-vops.preview declarations run, on their own urls")),
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
      const svcs = ctx.p.services, declared = svcs.filter((x) => x.preview);
      if (!declared.length) {
        // previews are opt-in per service: show the declaration to add, built from this project's services
        const main = svcs.find((x) => x.domains.length) || svcs[0] || { name: "web" };
        const others = svcs.filter((x) => x !== main).map((x) => x.name);
        return put(list, h("div", { class: "empty" },
          h("strong", {}, "No service can get a preview"),
          h("div", { class: "small" }, "Previews are opt-in per service. Add ", mono("x-vops.preview"), " to the service a preview is for: ", mono("with"), " runs other services next to it (empty), ", mono("copy"), " also copies their data from production. Services in no declaration never run in a preview."),
          h("pre", { class: "cmd" }, `services:\n  ${main.name}:\n    x-vops:\n      preview:\n        with: [${others.join(", ")}]\n        copy: []   # e.g. your database${others.length ? "" : "\n# or just: preview: {}"}`),
          h("div", { class: "small" }, "Then push its image with a ", mono("preview-*"), " tag, or use New preview above.")));
      }
      const own = declared.find((x) => x.image.startsWith(reg + "/"));
      const repo = own ? own.image.slice(reg.length + 1).replace(/[:@].*$/, "") : path + "/" + declared[0].name;
      const dom = ((own && own.domains.length ? own : declared.find((x) => x.domains.length)) || { domains: [] }).domains[0];
      return put(list, h("div", { class: "empty" },
        h("strong", {}, "No previews"),
        h("div", { class: "small" }, "Push the image of a service with ", mono("x-vops.preview"), " (", declared.map((x) => x.name).join(", "), ") with a ", mono("preview-*"), " tag: vops creates preview ", mono("pr-42"), " with it and what its declaration runs (pushing another declared image with the same tag lands in the same preview). Production is never redeployed by these tags."),
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
          refresh();
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
