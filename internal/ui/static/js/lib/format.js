// Plain text formatting: times, sizes, refs.

export const enc = encodeURIComponent;
export const dec = (s) => { try { return decodeURIComponent(s); } catch { return s; } };
export const pad = (n) => String(n).padStart(2, "0");

// "repo@sha256:<64 hex>" → "repo@sha256:<12 hex>…" (the full ref goes in the title)
export const shortImage = (ref) => (ref || "").replace(/@sha256:([0-9a-f]{12})[0-9a-f]+/, "@sha256:$1…");
export const short = (s, n = 12) => (s || "").replace("sha256:", "").slice(0, n);
// images in the history: "registry.x/shop/web:v1" shows as "web:v1"
export const imgName = (ref) => shortImage((ref || "").split("/").pop());
// pinned is the ref by digest, like the engine's
export const pinned = (img) => {
  if (!img.digest) return img.image;
  let r = img.image;
  const at = r.indexOf("@"), c = r.lastIndexOf(":");
  if (at >= 0) r = r.slice(0, at);
  else if (c > r.lastIndexOf("/")) r = r.slice(0, c);
  return r + "@" + img.digest;
};

export const ago = (unix) => {
  if (!unix) return "never";
  const s = Date.now() / 1000 - unix;
  if (s < 60) return "just now";
  if (s < 3600) return Math.floor(s / 60) + "m ago";
  if (s < 172800) return Math.floor(s / 3600) + "h ago";
  return Math.floor(s / 86400) + "d ago";
};
export const until = (u) => {
  const s = u - Date.now() / 1000;
  if (s <= 0) return "now";
  if (s < 3600) return "in " + Math.max(1, Math.floor(s / 60)) + "m";
  if (s < 172800) return "in " + Math.floor(s / 3600) + "h";
  return "in " + Math.floor(s / 86400) + "d";
};
export const duration = (s) => { const d = Math.floor(s / 86400), hh = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60); return d ? `${d}d ${hh}h` : hh ? `${hh}h ${m}m` : `${m}m`; };
const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
export const fmtDate = (d) => `${d.getDate()} ${MONTHS[d.getMonth()]}${d.getFullYear() !== new Date().getFullYear() ? " " + d.getFullYear() : ""} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
export const fmtUnix = (u) => fmtDate(new Date(u * 1000));
export const fullDate = (u) => new Date(u * 1000).toLocaleString();
export const localInput = (u) => { const d = new Date(u * 1000); return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`; };

export const size = (n) => n >= 1 << 30 ? (n / (1 << 30)).toFixed(1) + " GB" : n >= 1 << 20 ? (n / (1 << 20)).toFixed(1) + " MB" : n >= 1024 ? (n / 1024).toFixed(1) + " KB" : n + " B";
export const pct = (x) => (x < 10 ? x.toFixed(1) : Math.round(x)) + "%";
export const rate = (bps) => size(Math.round(bps)) + "/s";
export const plural = (n, word) => n + " " + word + (n === 1 ? "" : "s");
