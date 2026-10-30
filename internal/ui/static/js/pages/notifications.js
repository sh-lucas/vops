// Notifications: Web Push to this device, alerts, per-user toggles, devices, thresholds.
import { h, put, icon } from "../lib/dom.js";
import { api } from "../lib/api.js";
import { enc, until, duration } from "../lib/format.js";
import { projectHref } from "../model.js";
import { watch } from "../scope.js";
import { toast, when, page, head, panel, empty, table, alertBox, failed, busy, confirmDialog } from "../ui.js";

const pushSupported = () => "serviceWorker" in navigator && "PushManager" in window && "Notification" in window;
const isIOS = () => /iPad|iPhone|iPod/.test(navigator.userAgent) || (navigator.platform === "MacIntel" && navigator.maxTouchPoints > 1);
const standalone = () => matchMedia("(display-mode: standalone)").matches || navigator.standalone === true;
const b64url = (buf) => btoa(String.fromCharCode(...new Uint8Array(buf))).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
const fromB64url = (s) => Uint8Array.from(atob(s.replace(/-/g, "+").replace(/_/g, "/")), (c) => c.charCodeAt(0));
function deviceLabel() {
  const ua = navigator.userAgent;
  const browser = /Edg\//.test(ua) ? "Edge" : /Firefox\//.test(ua) ? "Firefox" : /Chrome\//.test(ua) ? "Chrome" : /Safari\//.test(ua) ? "Safari" : "Browser";
  const os = isIOS() ? "iOS" : /Android/.test(ua) ? "Android" : /Mac OS X/.test(ua) ? "macOS" : /Windows/.test(ua) ? "Windows" : /Linux/.test(ua) ? "Linux" : "";
  return browser + (os ? " on " + os : "") + (standalone() ? " (app)" : "");
}
async function pushSubscription() {
  if (!pushSupported()) return null;
  const reg = await navigator.serviceWorker.getRegistration("/");
  return reg ? reg.pushManager.getSubscription() : null;
}
async function enablePush(publicKey) {
  const perm = await Notification.requestPermission();
  if (perm !== "granted") throw new Error(perm === "denied" ? "Notifications are blocked for this site: allow them in the browser's site settings" : "Permission not given");
  const reg = await navigator.serviceWorker.register("/sw.js", { scope: "/" });
  await navigator.serviceWorker.ready;
  let sub = await reg.pushManager.getSubscription();
  if (sub && sub.options.applicationServerKey && b64url(sub.options.applicationServerKey) !== publicKey) { await sub.unsubscribe(); sub = null; } // the host's key changed
  sub ||= await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: fromB64url(publicKey) });
  await api("POST", "/notify/devices", { ...sub.toJSON(), label: deviceLabel() });
}
// a click on a notification focuses an open dashboard and sends it where the notification points


const ALERT_STATE = { open: "bad", silenced: "", expired: "warn", resolved: "ok" };
const alertTarget = (a) => a.project ? h("a", { href: projectHref(a.project) }, a.project + (a.service ? "/" + a.service : "")) : h("span", { class: "muted" }, "host");

export async function notificationsPage(alive) {
  let data, sub;
  const load = async () => {
    sub = await pushSubscription().catch(() => null);
    data = await api("GET", "/notify" + (sub ? "?endpoint=" + enc(sub.endpoint) : ""));
  };
  try { await load(); } catch (e) { return failed(e, alive); }
  if (!alive()) return;
  const deviceBox = h("div", {}), activeBox = h("div", {}), recentBox = h("div", {}), devicesBox = h("div", {});
  const refresh = async () => {
    try { await load(); } catch (e) { return toast(e.message); }
    if (!alive()) return;
    renderDevice(); renderAlerts(); renderDevices();
  };
  watch((s) => s.status, () => refresh());

  const renderDevice = () => {
    const mine = data.devices.find((d) => d.mine);
    let body;
    if (!window.isSecureContext) {
      body = alertBox("warn", "Not a secure context", "Push needs https (open the dashboard at vops.<domain>) or localhost (vops ui).");
    } else if (!pushSupported()) {
      body = isIOS() && !standalone()
        ? alertBox("info", "Install vops as an app first", "On iPhone and iPad, notifications only work for web apps on the home screen: tap Share, then Add to Home Screen, open vops from there and enable them.")
        : alertBox("warn", "This browser can't receive push notifications", "Try a recent Chrome, Firefox, Edge or Safari.");
    } else if (Notification.permission === "denied") {
      body = alertBox("warn", "Notifications are blocked for this site", "Allow them in the browser's site settings, then reload.");
    } else if (sub && mine) {
      const test = h("button", { class: "btn", onclick: () => busy(test, async () => {
        const res = await api("POST", "/notify/test", { endpoint: sub.endpoint });
        toast(res.every((r) => !r.error) ? "Sent: it should show up in a few seconds" : res.map((r) => r.error).join("; "));
        refresh();
      }) }, "Send test");
      const off = h("button", { class: "btn", onclick: () => busy(off, async () => {
        await api("DELETE", "/notify/devices?endpoint=" + enc(sub.endpoint)).catch(() => {});
        await sub.unsubscribe();
        toast("Notifications off on this device");
        refresh();
      }) }, "Turn off");
      body = h("div", { class: "row" }, h("span", { class: "tag ok" }, "On"), h("span", { class: "muted" }, "This device (" + mine.label + ") gets your notifications."), h("span", { class: "spacer" }), test, off);
    } else {
      const on = h("button", { class: "btn primary", onclick: () => busy(on, async () => {
        await enablePush(data.public_key);
        toast("Notifications on");
        await refresh();
      }) }, icon("bell"), "Enable notifications on this device");
      body = h("div", { class: "row" }, on, isIOS() ? h("span", { class: "muted small" }, "iPhone/iPad: works from the home screen app only.") : null);
    }
    put(deviceBox, panel(h("div", { class: "panel-body" }, body)));
  };

  const renderAlerts = () => {
    const st = data.settings;
    const open = data.alerts.filter((a) => a.state === "open");
    const silence = async (a, btn) => busy(btn, async () => { await api("POST", "/notify/silence", { id: a.id }); toast("Silenced for everyone"); refresh(); refresh(); });
    put(activeBox, open.length ? panel(table(["Kind", "Target", "Alert", "First seen", "Sends", "Next", "Times", ""], open.map((a) => {
      const btn = h("button", { class: "btn sm" }, "Silence");
      btn.onclick = () => silence(a, btn);
      return h("tr", {},
        h("td", {}, h("span", { class: "tag bad" }, a.kind)),
        h("td", { class: "nowrap" }, alertTarget(a)),
        h("td", { class: "wrapany" }, a.url ? h("a", { href: a.url }, a.title) : a.title, a.body ? h("div", { class: "muted small" }, a.body) : null),
        h("td", { class: "muted small" }, when(a.first_at)),
        h("td", {}, a.sends),
        h("td", { class: "small nowrap" }, "resend " + until(a.sent_at + st.repeat), h("div", { class: "muted" }, "stops " + until(a.first_at + st.expire))),
        h("td", {}, a.occurrences),
        h("td", { class: "actions-cell" }, btn));
    }))) : panel(empty("Nothing is being sent", "Alerts show up here while they last; each is re-sent every " + duration(st.repeat) + " until it ends, is silenced, or " + duration(st.expire) + " passed.")));
    const closed = data.alerts.filter((a) => a.state !== "open").slice(0, 50);
    put(recentBox, closed.length ? panel(table(["When", "Kind", "Target", "Alert", "State", "Times"], closed.map((a) => h("tr", {},
      h("td", { class: "muted small" }, when(a.first_at)),
      h("td", {}, h("span", { class: "tag" }, a.kind)),
      h("td", { class: "nowrap" }, alertTarget(a)),
      h("td", { class: "wrapany" }, a.title, a.body ? h("div", { class: "muted small" }, a.body) : null),
      h("td", { class: "small" }, h("span", { class: "tag " + ALERT_STATE[a.state] }, a.state), a.closed_by ? " by " + a.closed_by : "", a.ended_at ? h("div", { class: "muted" }, "ended ", when(a.ended_at)) : h("div", { class: "warn-text" }, "still happening")),
      h("td", {}, a.occurrences))))) : panel(empty("No past alerts", null)));
  };

  const renderDevices = () => {
    const del = async (d) => {
      if (!(await confirmDialog({ title: `Remove ${d.label || "this device"} of ${d.user}?`, ok: "Remove device", danger: true,
        body: ["It stops getting notifications right away. Enabling them again on that device subscribes it anew."] }))) return;
      try { await api("DELETE", "/notify/devices?id=" + d.id); toast("Removed"); refresh(); } catch (x) { toast(x.message); }
    };
    put(devicesBox, data.devices.length ? panel(table(["User", "Device", "Added", "Last delivered", "Last error", ""], data.devices.map((d) => h("tr", {},
      h("td", { class: "mono" }, d.user),
      h("td", {}, d.label || "?", d.mine ? h("span", { class: "tag accent" }, "this device") : null),
      h("td", { class: "muted small" }, when(d.created_at)),
      h("td", { class: "muted small" }, d.last_ok_at ? when(d.last_ok_at) : "never"),
      h("td", { class: "small wrapany" + (d.last_error ? " warn-text" : "") }, d.last_error || "—"),
      h("td", { class: "actions-cell" }, h("button", { class: "btn sm danger", onclick: () => del(d) }, "Remove"))))))
      : panel(empty("No devices yet", "Enable notifications on a phone or a computer above.")));
  };

  const prefs = panel(h("div", { class: "panel-body stack-tight" }, data.kinds.map((k) => {
    const box = h("input", { type: "checkbox", checked: k.enabled });
    box.onchange = async () => {
      try { await api("POST", "/notify/prefs", { kind: k.id, enabled: box.checked }); toast((box.checked ? "On: " : "Off: ") + k.title); } catch (x) { box.checked = !box.checked; toast(x.message); }
    };
    return h("label", { class: "pref" }, box, h("span", {}, h("strong", {}, k.title), k.default ? null : h("span", { class: "tag faint" }, "off by default"), h("div", { class: "muted small" }, k.desc)));
  })));

  // thresholds: stored in seconds and percent, shown in friendlier units
  const FIELDS = [["repeat", "Re-send an open alert every", "min", 60], ["expire", "Stop sending after", "h", 3600], ["quiet", "Event alerts end after a quiet", "min", 60],
    ["probe_interval", "Probe routed services every", "s", 1], ["down_after", "Down/unhealthy after failed probes", "probes", 1], ["sample_every", "Sample resources every", "s", 1],
    ["cpu", "Host cpu above", "%", 1], ["cpu_for", "… for", "min", 60], ["mem_avail", "Host memory available below", "%", 1], ["mem_for", "… for", "min", 60],
    ["disk", "Filesystem used above", "%", 1], ["psi", "Memory pressure (PSI some avg60) above", "%", 1],
    ["ctr_mem", "Container memory above (of its limit)", "%", 1], ["ctr_mem_for", "… for", "min", 60], ["ctr_cpu", "Container cpu above (of its quota or the host)", "%", 1], ["ctr_cpu_for", "… for", "min", 60]];
  const inputs = {};
  const fill = (s) => { for (const [k, , , f] of FIELDS) inputs[k].value = +(s[k] / f).toFixed(2); };
  const saveBtn = h("button", { class: "btn primary" }, "Save");
  const resetBtn = h("button", { class: "btn", type: "button", onclick: () => busy(resetBtn, async () => { data.settings = await api("DELETE", "/notify/settings"); fill(data.settings); toast("Back to defaults"); renderAlerts(); }) }, "Reset to defaults");
  const form = h("form", { class: "inline settings", onsubmit: (e) => {
    e.preventDefault();
    const body = {};
    for (const [k, , , f] of FIELDS) body[k] = Math.round(+inputs[k].value * f);
    busy(saveBtn, async () => { data.settings = await api("POST", "/notify/settings", body); toast("Saved"); renderAlerts(); });
  } }, FIELDS.map(([k, label, unit, f]) => {
    inputs[k] = h("input", { type: "number", min: 0, step: "any", required: true });
    return h("label", { class: "field", title: "default " + +(data.defaults[k] / f).toFixed(2) + " " + unit }, label + " (" + unit + ")", inputs[k]);
  }), h("div", { class: "row" }, saveBtn, resetBtn));
  fill(data.settings);

  renderDevice(); renderAlerts(); renderDevices();
  page(head({ title: "Notifications", sub: "Web Push to your phone or computer when something breaks. Settings live on the host, not in vops.yml." }),
    deviceBox,
    h("div", { class: "section" }, h("div", { class: "section-head" }, h("h2", {}, "Active"), h("span", { class: "hint" }, "being sent now; anyone can silence one for everyone")), activeBox),
    h("div", { class: "section" }, h("div", { class: "section-head" }, h("h2", {}, "Recent")), recentBox),
    h("div", { class: "section" }, h("div", { class: "section-head" }, h("h2", {}, "My notifications"), h("span", { class: "hint" }, "what " + data.user + " gets, on every device")), prefs),
    h("div", { class: "section" }, h("div", { class: "section-head" }, h("h2", {}, "Devices"), h("span", { class: "hint" }, "everyone's; anyone can remove any")), devicesBox),
    h("details", { class: "section" }, h("summary", {}, h("h2", { class: "inline-h" }, "Advanced")), h("p", { class: "muted small" }, "Thresholds and intervals for everyone. Hover a field for its default."), panel(h("div", { class: "panel-body" }, form))));
}
