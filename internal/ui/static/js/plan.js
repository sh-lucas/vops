// The plan/apply flow, reachable from anywhere through the "Pending changes" button.
import { h } from "./lib/dom.js";
import { apiText, stream, preSink } from "./lib/api.js";
import { short } from "./lib/format.js";
import { app, refresh } from "./state.js";
import { modal, mono, toast } from "./ui.js";

export async function openPlan() {
  const st = app.get().status || {};
  const out = h("pre", { class: "out" }, "Loading plan…");
  const result = h("span", {});
  let running = false;
  const apply = h("button", { class: "btn primary", disabled: true, onclick: () => {
    running = true;
    apply.disabled = true;
    apply.classList.add("busy");
    out.textContent = "";
    const finish = (ok) => {
      running = false;
      apply.classList.remove("busy");
      apply.hidden = true;
      result.replaceChildren(h("span", { class: "tag " + (ok ? "ok" : "bad") }, ok ? "Applied" : "Apply failed"));
      toast(ok ? "Applied" : "Apply failed");
      refresh();
    };
    stream("/apply", preSink(out), { method: "POST", body: JSON.stringify({ commit: st.commit, trigger: "ui" }),
      done: () => finish(/==> ok\s*$/.test(out.textContent)),
      error: (msg) => { out.append("\n" + msg); finish(false); } });
  } }, "Apply");
  const d = h("dialog", { class: "modal wide" }, h("div", { class: "inner" },
    h("div", { class: "modal-head" }, h("h3", {}, "Pending changes")),
    h("div", { class: "modal-body" },
      h("p", {}, "What runs differs from ~/vops at ", mono(short(st.commit, 8) || "?"), " (a push, env vars or enable/disable). Applying deploys it."),
      out),
    h("div", { class: "modal-foot" }, result, h("span", { class: "spacer" }), h("button", { class: "btn", onclick: () => d.close() }, "Close"), apply)));
  d.addEventListener("close", () => {
    if (running) toast("Apply keeps running on the host");
    d.remove();
  });
  modal(d);
  try {
    out.textContent = await apiText("/plan?format=text");
    apply.disabled = false;
  } catch (e) { out.textContent = e.message; }
}
