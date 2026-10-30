// Files: git-tracked folders and common text files only.
import { h, icon } from "../lib/dom.js";
import { api, apiText } from "../lib/api.js";
import { enc, size } from "../lib/format.js";
import { sep, page, head, panel, panelHead, empty, loading, errorBox, failed } from "../ui.js";

export async function filesPage(alive, params) {
  let files;
  try { files = await api("GET", "/tree"); } catch (e) { return failed(e, alive); }
  if (!alive()) return;
  const viewer = panel(empty("Pick a file", "Only git-tracked text files are shown."));
  let active = null;
  const open = async (f, a) => {
    active && active.classList.remove("active");
    (active = a).classList.add("active");
    viewer.replaceChildren(loading("Loading…", "pad"));
    try {
      const text = await apiText("/file?path=" + enc(f.path));
      viewer.replaceChildren(panelHead(h("strong", { class: "mono wrapany" }, f.path), h("span", { class: "spacer" }), h("span", { class: "muted small nowrap" }, size(f.size) + " · " + text.split("\n").length + " lines")), h("pre", { class: "file" }, text));
    } catch (e) { viewer.replaceChildren(h("div", { class: "panel-body" }, errorBox(e))); }
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
  const render = (node) => h("ul", { class: "ftree" }, Object.keys(node).sort((a, b) => (b.endsWith("/") - a.endsWith("/")) || a.localeCompare(b)).map((k) => {
    if (k.endsWith("/")) return h("li", {}, h("div", { class: "fdir" }, icon("folder"), k), render(node[k]));
    const a = h("a", { href: "#", onclick: (e) => { e.preventDefault(); open(node[k], a); } }, icon("file"), k);
    return h("li", {}, a);
  }));
  page(head({ title: "Files", sub: ["Tracked by git in ~/vops", sep(), shown.length + " text files", dir ? h("span", { class: "tag mono" }, dir, " ", h("a", { href: "#/files", title: "all files", "aria-label": "show all files" }, "×")) : null] }),
    shown.length ? h("div", { class: "split" }, h("div", { class: "panel filelist" }, render(tree)), viewer) : panel(empty("No text files", dir ? "Nothing tracked under " + dir + "." : "Nothing tracked by git yet.")));
}
