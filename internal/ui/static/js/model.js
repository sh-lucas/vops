// What the status api means: project health in plain words, the project tree, links.
import { enc } from "./lib/format.js";

// a finished job (exit 0) is fine, not an error
export const stateOf = (c) => (c.Labels && c.Labels["vops.job"] && (c.State === "exited" || c.State === "stopped") && c.ExitCode === 0 ? "done" : c.State);
const DOWN = new Set(["exited", "dead", "stopped"]);

// health: kind (ok, warn, bad, idle) and a short label; why is the long form for titles
export function health(p) {
  if (p.disabled) return { kind: "idle", label: "Disabled", why: "disabled: its containers stop on the next apply" };
  if (p.error) return { kind: "bad", label: "Invalid compose", why: "the compose file doesn't load" };
  const cs = p.services.flatMap((s) => s.containers);
  const down = cs.filter((c) => DOWN.has(stateOf(c))).length;
  if (down) return { kind: "bad", label: down === cs.length ? "Down" : `${down} of ${cs.length} down`, why: "some containers are not running" };
  if (p.gone) return { kind: "warn", label: "Removed from git", why: "removed from git: an apply removes it" };
  if (p.services.some((s) => s.pending)) return { kind: "warn", label: "Pending changes", why: "what runs differs from git: apply to deploy" };
  if (cs.some((c) => ["created", "paused", "restarting"].includes(stateOf(c)))) return { kind: "warn", label: "Starting", why: "some containers are starting or paused" };
  if (!cs.length) return { kind: "idle", label: "Not running", why: "no containers" };
  return { kind: "ok", label: "Running", why: "all running" };
}

// nest projects by path (shop/api goes under shop) and flatten to rows with a depth
export function treeRows(projects) {
  const root = { children: {} };
  for (const p of projects) {
    let node = root;
    for (const part of p.path.split("/")) node = node.children[part] ??= { children: {}, project: null };
    node.project = p;
  }
  const rows = [];
  const walk = (node, depth) => {
    for (const [name, child] of Object.entries(node.children).sort(([a], [b]) => a.localeCompare(b))) {
      rows.push({ name, depth, project: child.project });
      walk(child, depth + 1);
    }
  };
  walk(root, 0);
  return rows;
}

export const previewsOf = (st, path) => (st.previews || []).filter((x) => x.project === path);
export const domainURL = (st, d) => (st.domain ? "https://" : "http://") + d;

export const TAB_IDS = ["services", "timeline", "previews", "logs", "env", "events"];
export const TAB_ALIASES = { data: "timeline" }; // old links
export const PROJECT_RE = new RegExp("^/p/(.+?)(?:/(" + [...TAB_IDS, ...Object.keys(TAB_ALIASES)].join("|") + "))?/?$");
export const projectHref = (path, tab = "services", q = "") => "#/p/" + enc(path) + "/" + tab + q;
