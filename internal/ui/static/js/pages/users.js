// Users: admins (dashboard + every repo) and deployers (registry only).
import { h } from "../lib/dom.js";
import { api } from "../lib/api.js";
import { enc } from "../lib/format.js";
import { app } from "../state.js";
import { toast, when, page, head, panel, empty, table, alertBox, failed, busy, confirmDialog } from "../ui.js";

export async function usersPage(alive, created) {
  let users;
  try { users = await api("GET", "/users"); } catch (e) { return failed(e, alive); }
  if (!alive()) return;
  const name = h("input", { placeholder: "shop-ci", required: true, pattern: "[a-z0-9][a-z0-9._-]*" });
  const role = h("select", {}, h("option", { value: "deployer" }, "Deployer (registry only)"), h("option", { value: "admin" }, "Admin (dashboard + every repo)"));
  const global = h("input", { type: "checkbox" });
  const repos = h("input", { placeholder: "shop/web, shop/api", class: "mono" });
  const password = h("input", { type: "password", placeholder: "at least 12 characters", autocomplete: "new-password", minlength: 12 });
  const globalBox = h("label", { class: "check" }, global, "Every repository");
  const reposBox = h("label", { class: "field" }, "Repositories", repos);
  const pwBox = h("label", { class: "field" }, "Password", password);
  const sync = () => {
    const admin = role.value === "admin";
    globalBox.hidden = admin;
    reposBox.hidden = admin || global.checked;
    pwBox.hidden = !admin;
  };
  role.onchange = global.onchange = sync;
  const saveBtn = h("button", { class: "btn primary" }, "Save");
  const save = async (body) => {
    try {
      const r = await api("POST", "/users", body);
      if (!r.token) toast("Saved " + r.name);
      usersPage(alive, r.token ? r : null);
    } catch (x) { toast(x.message); }
  };
  const form = h("form", { class: "inline", onsubmit: (e) => {
    e.preventDefault();
    const body = { name: name.value, role: role.value };
    if (role.value === "admin") { if (password.value) body.password = password.value; }
    else { body.global = global.checked; body.repos = global.checked ? [] : repos.value.split(",").map((s) => s.trim()).filter(Boolean); }
    busy(saveBtn, () => save(body));
  } }, h("label", { class: "field" }, "Name", name), h("label", { class: "field" }, "Role", role), globalBox, reposBox, pwBox, saveBtn);
  sync();

  const edit = (u) => {
    name.value = u.name; role.value = u.role; global.checked = u.global; repos.value = (u.repos || []).join(", "); password.value = "";
    sync();
    name.focus();
  };
  const rotate = async (u) => {
    if (!(await confirmDialog({ title: `New token for ${u.name}?`, ok: "Generate new token", danger: true,
      body: ["The current token stops working right away: update it wherever it's used (CI secrets, other hosts)."] }))) return;
    save({ name: u.name, new_token: true });
  };
  const del = async (u) => {
    if (!(await confirmDialog({ title: `Delete ${u.name}?`, ok: "Delete user", danger: true,
      body: [u.role === "admin" ? "Its sessions end and its password stops working right away, in the dashboard and the registry." : "Its token stops working right away: anything logging in as it loses registry access. Images stay."] }))) return;
    try { await api("DELETE", "/users?name=" + enc(u.name)); toast("Deleted " + u.name); usersPage(alive); } catch (x) { toast(x.message); }
  };
  const dash = h("span", { class: "faint" }, "—");
  page(head({ title: "Users", sub: "Admins log into the dashboard and push and pull every repository (podman login with their password). Deployers only use the registry, with a token: every repository, or a list." }),
    created ? alertBox("info", `Token for ${created.name} (shown once)`, [
      h("div", { class: "token mono" }, created.token),
      h("pre", {}, `podman login -u ${created.name} ${location.hostname.replace(/^vops\./, "registry.")}`)]) : null,
    panel(users.length ? table(["Name", "Role", "Access", "Created", ""], users.map((u) => h("tr", {},
      h("td", { class: "mono" }, u.name, app.get().status?.user === u.name ? h("span", { class: "tag" }, "you") : null),
      h("td", {}, u.role),
      h("td", { class: "mono small wrapany" }, u.global ? "every repository" : (u.repos || []).join(", ") || dash.cloneNode(true)),
      h("td", { class: "muted" }, when(u.created_at)),
      h("td", { class: "actions-cell" }, h("div", { class: "actions" },
        h("button", { class: "btn sm", onclick: () => edit(u) }, "Edit"),
        u.role === "deployer" ? h("button", { class: "btn sm", onclick: () => rotate(u) }, "New token") : null,
        h("button", { class: "btn sm danger", onclick: () => del(u) }, "Delete"))))))
      : empty("No users yet", "Add one below to push from CI or another machine.")),
    h("div", { class: "section" }, h("div", { class: "section-head" }, h("h2", {}, "Add or edit")), panel(h("div", { class: "panel-body" }, form))));
}
