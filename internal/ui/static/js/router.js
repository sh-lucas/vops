// Hash router. A page is async (alive, ...args); alive() turns false once the user navigates away.
import { dec } from "./lib/format.js";
import { app } from "./state.js";
import { leave } from "./scope.js";
import { PROJECT_RE } from "./model.js";
import { loading, page, setFill } from "./ui.js";
import { overview } from "./pages/overview.js";
import { projectPage } from "./pages/project.js";
import { logsPage } from "./pages/logs.js";
import { filesPage } from "./pages/files.js";
import { registryPage } from "./pages/registry.js";
import { usersPage } from "./pages/users.js";
import { eventsPage } from "./pages/events.js";
import { systemPage } from "./pages/system.js";
import { notificationsPage } from "./pages/notifications.js";

// Old links keep working: #/p/<path> is the services tab, #/logs/<path>?service=x the full-page logs.
export function route() {
  const alive = leave();
  const [path, query] = location.hash.slice(1).split("?");
  const params = new URLSearchParams(query || "");
  app.set({ route: { path: path || "/", params }, navOpen: false });
  setFill(false);
  page(loading());
  window.scrollTo(0, 0);
  let m;
  if ((m = path.match(PROJECT_RE))) return projectPage(alive, dec(m[1]), m[2] || "services", params);
  if ((m = path.match(/^\/logs\/(.+)$/))) return logsPage(alive, dec(m[1]), params.get("service"));
  if (path === "/files") return filesPage(alive, params);
  if (path === "/registry") return registryPage(alive);
  if (path === "/users") return usersPage(alive);
  if (path === "/events") return eventsPage(alive);
  if (path === "/system") return systemPage(alive);
  if (path === "/notifications") return notificationsPage(alive);
  return overview(alive);
}
