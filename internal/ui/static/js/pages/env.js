// Environment tab: env vars, write-only.
import { h, put } from "../lib/dom.js";
import { api } from "../lib/api.js";
import { enc } from "../lib/format.js";
import { projectHref } from "../model.js";
import { refresh } from "../state.js";
import { toast, when, mono, panel, panelHead, empty, loading, table, alertBox, errorBox, busy, confirmDialog } from "../ui.js";

// env vars, write-only. Scope: production, or the previews' secrets (previews never get production's env).
export function envTab(ctx, box) {
  const { path } = ctx;
  const preview = ctx.params.get("scope") === "previews";
  const scope = preview ? "&preview=1" : "";
  const key = h("input", { placeholder: "KEY", pattern: "[A-Za-z_][A-Za-z0-9_]*", required: true, class: "mono", autocomplete: "off" });
  const val = h("input", { placeholder: "value", type: "password", autocomplete: "new-password" });
  const setBtn = h("button", { class: "btn primary" }, "Set");
  const form = h("form", { class: "inline", onsubmit: (e) => {
    e.preventDefault();
    busy(setBtn, async () => {
      await api("POST", "/env", { project: path, key: key.value, value: val.value, preview });
      toast(key.value + (preview ? " set. Previews get it on their next update." : " set. Apply to deploy it."));
      key.value = val.value = "";
      load();
      refresh();
    });
  } }, h("label", { class: "field" }, "Name", key), h("label", { class: "field" }, "Value (write-only)", val), setBtn);
  const prefill = (k) => {
    key.value = k;
    form.scrollIntoView({ block: "center", behavior: "smooth" });
    val.focus({ preventScroll: true });
  };
  const list = h("div", {}, loading("Loading…", "pad"));
  const services = h("div", {});
  const backup = h("div", {});
  const loadBackup = async () => {
    let b;
    try { b = await api("GET", "/secrets"); } catch (e) { return put(backup, errorBox(e)); }
    const state = {
      current: "saved in git, up to date",
      behind: ["the copy in git is behind: run ", mono("vops sync"), " to save it"],
      foreign: ["the copy in git isn't one this host wrote: ", mono("vops sync"), " offers to restore its secrets"],
      none: b.sha ? ["not in git yet: run ", mono("vops sync")] : "nothing to back up yet",
    }[b.repo];
    put(backup, panel(panelHead(h("h2", {}, "Backup in git"), h("span", { class: "muted small" }, mono(b.file), " · encrypted to the ssh keys below")),
      h("div", { class: "panel-note small" + (b.repo === "behind" || b.repo === "foreign" ? " warn-text" : " muted") }, b.secrets + " secret(s) of every project and previews · ", state),
      b.warning ? alertBox("bad", "Nobody can decrypt a new backup", h("span", { class: "small" }, b.warning)) : null,
      b.recipients.length ? table(["Can decrypt", "Type", "From"], b.recipients.map((r) => h("tr", {},
        h("td", { class: "wrapany", title: r.fingerprint || null }, r.comment || mono(r.fingerprint || r.key)), h("td", { class: "mono small" }, r.type), h("td", { class: "small" }, r.source)))) : null,
      b.skipped.map((r) => h("div", { class: "panel-note small warn-text" }, "Skipped ", mono(r.type), " ", r.comment || r.fingerprint || r.key, " (", r.source, "): ", r.reason)),
      h("div", { class: "panel-note small muted" }, "Restore on a new host: ", mono("vops sync"), " or ", mono("vops env restore"), " decrypts it with your ssh key. More people: ", mono("secrets.recipients"), " in vops.yml.")));
  };
  const load = async () => {
    loadBackup();
    let envs, usage = null, uerr = null;
    try {
      [envs, usage] = await Promise.all([api("GET", "/env?project=" + enc(path) + scope), api("GET", "/env/usage?project=" + enc(path) + scope).catch((e) => { uerr = e; return null; })]);
    } catch (e) { return list.replaceChildren(errorBox(e)); }
    const unused = new Set(usage ? usage.unused : []);
    const svcs = usage ? Object.keys(usage.services).sort() : [];
    const composeOnly = svcs.some((s) => usage.services[s].length);
    list.replaceChildren(envs.length ? table(["Name", "Value", ""], envs.map((e) => h("tr", {},
      h("td", { class: "mono wrapany" }, e.key, unused.has(e.key) ? [" ", h("span", { class: "tag", title: "No service uses it: not in any environment: [" + e.key + "] or ${" + e.key + "}" }, "unused")] : null),
      h("td", { class: "muted small" }, "••••••••  set ", when(e.updated_at)),
      h("td", { class: "actions-cell" }, h("button", { class: "btn sm danger", onclick: async () => {
        if (!(await confirmDialog({ title: `Remove ${e.key}?`, ok: "Remove", danger: true,
          body: [h("p", {}, preview ? "Previews of " : "Services of ", h("strong", {}, path), preview ? " lose it on their next update." : " that use it lose it on the next apply."), "Values are write-only: to restore it you have to set it again."] }))) return;
        try {
          await api("DELETE", "/env?project=" + enc(path) + "&key=" + enc(e.key) + scope);
          toast(e.key + " removed");
          load();
          refresh();
        } catch (x) { toast(x.message); }
      } }, "Remove")))))
      : h("div", { class: "panel-body muted small" }, preview ? "No preview secrets set. " : "No secrets set with vops env. ",
        composeOnly ? "The services still get the variables listed below (from compose and env files)." : preview ? "Previews get preview.env, then these on top." : "Set one below and use it with environment: [KEY] or ${KEY} in compose.yml."));
    // per service: every variable the containers get, and from where (never values)
    const hint = (en) => {
      if (en.source === "missing") {
        const k = en.vars && en.vars.length ? en.vars[0] : en.key;
        return [h("span", { class: "warn-text" }, en.vars && en.vars.length ? "${" + k + "} is set nowhere: empty" : "set nowhere: empty"), " ", h("button", { class: "btn sm", onclick: () => prefill(k) }, "Set it")];
      }
      if (en.secret) return h("span", { class: "warn-text", title: `Replace the literal with environment: [${en.key}] in compose.yml and set the value with vops env set (or the form above).` }, "committed to git: move to vops env");
      if (en.file === "builtin") return "set by vops";
      if (en.file === "preview.env") return "from preview.env in the repo";
      if (en.vars && en.vars.length) return "via " + en.vars.map((v) => "${" + v + "}").join(", ");
      return "";
    };
    const src = (en) => h("span", { class: "tag" + (en.source === "missing" ? " warn" : en.source === "vops" ? " accent" : "") }, en.source, en.file && en.file !== "builtin" ? " · " + en.file : "");
    put(services, uerr ? alertBox("warn", "Can't read the compose files", h("span", { class: "small wrapany" }, uerr.message)) : null,
      usage ? panel(panelHead(h("h2", {}, "What each service gets"), h("span", { class: "muted small" }, preview ? "in previews" : "names and sources only, never values")),
        svcs.length ? table(["Variable", "Source", ""], svcs.flatMap((svc) => [
          h("tr", { class: "grp" }, h("td", { colspan: 3 }, h("span", { class: "mono" }, svc), usage.services[svc].length ? null : h("span", { class: "muted small" }, "  no variables"))),
          ...usage.services[svc].map((en) => h("tr", { class: en.source === "missing" ? "warnrow" : null },
            h("td", { class: "mono wrapany" }, en.key), h("td", { class: "nowrap" }, src(en)), h("td", { class: "small" }, hint(en))))]), "envuse")
          : empty("No services", "This project defines no services.")) : null);
  };
  const seg = h("div", { class: "seg", role: "tablist" },
    h("a", { href: projectHref(path, "env"), class: preview ? null : "active" }, "Production"),
    h("a", { href: projectHref(path, "env", "?scope=previews"), class: preview ? "active" : null }, "Previews"));
  box.replaceChildren(h("div", { class: "stack" }, panel(panelHead(h("h2", {}, preview ? "Preview secrets" : "vops env"), seg, h("span", { class: "muted small" }, "write-only: values are never shown again")),
    h("div", { class: "panel-note small muted" }, preview
      ? ["Previews don't inherit production's env: they read ", mono("preview.env"), " from ", mono(path + "/"), " in the repo, then these secrets on top."]
      : ["Used as ", mono("${KEY}"), " in compose.yml or with ", mono("environment: [KEY]"), ". Changes deploy on the next apply."]),
    list, h("div", { class: "panel-foot" }, form)), services, backup));
  load();
}
