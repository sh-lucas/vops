// System: the host's load, polled every 3s while the page is open (rates are since the previous poll).
import { h } from "../lib/dom.js";
import { api } from "../lib/api.js";
import { duration, size, pct, rate } from "../lib/format.js";
import { onLeave } from "../scope.js";
import { mono, sep, page, head, panel, table, failed } from "../ui.js";

const meter = (x) => h("div", { class: "meter" + (x >= 90 ? " bad" : x >= 70 ? " warn" : "") }, h("span", { style: `width:${Math.min(100, Math.max(0, x)).toFixed(1)}%` }));


export async function systemPage(alive) {
  let timer = null;
  onLeave(() => clearTimeout(timer));
  const tick = async () => {
    let sys;
    try { sys = await api("GET", "/system"); } catch (e) { return failed(e, alive); }
    if (!alive()) return;
    renderSystem(sys);
    timer = setTimeout(tick, document.hidden ? 15000 : 3000);
  };
  await tick();
}

function renderSystem(sys) {
  const card = (label, value, foot, x) => h("div", { class: "panel stat" }, h("div", { class: "label" }, label), h("div", { class: "value" }, value), x === undefined ? null : meter(x), h("div", { class: "foot" }, foot));
  const mem = sys.mem_total ? (sys.mem_used / sys.mem_total) * 100 : 0;
  const busiest = sys.disks.reduce((a, d) => (!a || d.busy > a.busy ? d : a), null);
  const rx = sys.net.reduce((n, x) => n + x.rx_bps, 0), tx = sys.net.reduce((n, x) => n + x.tx_bps, 0);
  const psi = Object.entries(sys.pressure || {});
  page(head({ title: "System", sub: [mono(sys.hostname || "host"), sep(), "Linux ", mono(sys.kernel || "?"), sep(), "updates every 3s"] }),
    h("div", { class: "stats" },
      card("CPU", pct(sys.cpu), `${sys.cpus} cpu${sys.cpus === 1 ? "" : "s"} · load ${sys.load.map((x) => x.toFixed(2)).join(" ")}`, sys.cpu),
      card("Memory", pct(mem), `${size(sys.mem_used)} of ${size(sys.mem_total)}` + (sys.swap_total ? ` · swap ${size(sys.swap_used)} of ${size(sys.swap_total)}` : ""), mem),
      busiest ? card("Disk IO", pct(busiest.busy), `${busiest.name} busy · read ${rate(busiest.read_bps)} · write ${rate(busiest.write_bps)}`, busiest.busy) : null,
      card("Network", ["↓ ", rate(rx)], ["↑ ", rate(tx)]),
      card("Uptime", duration(sys.uptime), "vops daemon up " + duration(sys.daemon_uptime))),
    h("div", { class: "section" }, h("div", { class: "section-head" }, h("h2", {}, "Disk space"), h("span", { class: "hint" }, "the filesystems holding ~/.vops and the repo")),
      panel(table(["Paths", "Used", "", "Available", "Size"], sys.filesystems.map((f) => {
        const used = f.total ? (f.used / f.total) * 100 : 0;
        return h("tr", {}, h("td", { class: "mono small wrapany" }, f.paths.join(", ")), h("td", { class: "nowrap" }, pct(used)), h("td", { style: "width:30%" }, meter(used)), h("td", { class: "nowrap" }, size(f.avail)), h("td", { class: "nowrap muted" }, size(f.total)));
      })))),
    sys.disks.length > 1 ? h("div", { class: "section" }, h("div", { class: "section-head" }, h("h2", {}, "Disks")),
      panel(table(["Device", "Busy", "", "Read", "Write"], sys.disks.map((d) => h("tr", {}, h("td", { class: "mono" }, d.name), h("td", { class: "nowrap" }, pct(d.busy)), h("td", { style: "width:30%" }, meter(d.busy)), h("td", { class: "nowrap" }, rate(d.read_bps)), h("td", { class: "nowrap" }, rate(d.write_bps))))))) : null,
    sys.net.length ? h("div", { class: "section" }, h("div", { class: "section-head" }, h("h2", {}, "Network")),
      panel(table(["Interface", "Receive", "Send", "Link"], sys.net.map((n) => {
        const cap = n.speed ? (n.speed * 1e6) / 8 : 0; // Mbit/s -> bytes/s
        return h("tr", {}, h("td", { class: "mono" }, n.name), h("td", { class: "nowrap" }, rate(n.rx_bps), cap ? h("span", { class: "muted" }, " · " + pct((n.rx_bps / cap) * 100)) : null),
          h("td", { class: "nowrap" }, rate(n.tx_bps), cap ? h("span", { class: "muted" }, " · " + pct((n.tx_bps / cap) * 100)) : null), h("td", { class: "muted nowrap" }, n.speed ? (n.speed >= 1000 ? n.speed / 1000 + " Gbit/s" : n.speed + " Mbit/s") : "unknown"));
      })))) : null,
    psi.length ? h("div", { class: "section" }, h("div", { class: "section-head" }, h("h2", {}, "Pressure"), h("span", { class: "hint" }, "share of time tasks waited on the resource (PSI): some = at least one, full = all; averages over 10s, 60s, 300s")),
      panel(table(["Resource", "Some", "Full"], ["cpu", "memory", "io"].filter((r) => sys.pressure[r]).map((r) => {
        const p = sys.pressure[r], fmt = (v) => v.map((x) => x.toFixed(2) + "%").join(" · ");
        return h("tr", {}, h("td", {}, r), h("td", { class: "mono small" + (p.some[0] >= 10 ? " warn-text" : "") }, fmt(p.some)), h("td", { class: "mono small" }, p.full ? fmt(p.full) : "—"));
      })))) : null);
}
