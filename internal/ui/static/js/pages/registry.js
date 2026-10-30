// Registry: repositories, tags, garbage collection.
import { h, icon } from "../lib/dom.js";
import { api } from "../lib/api.js";
import { enc, short, size } from "../lib/format.js";
import { toast, when, mono, sep, page, head, panel, panelHead, empty, table, failed, busy, confirmDialog } from "../ui.js";

export async function registryPage(alive) {
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
  page(head({ title: "Registry", sub: [h("span", {}, "podman login ", mono(host)), sep(), "admins and global users push and pull everything, others their repositories"], actions: gcBtn }),
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
