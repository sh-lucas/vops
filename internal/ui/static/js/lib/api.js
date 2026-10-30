// The JSON api. A 401 fires "vops:logout" on window: the app store listens and shows the login.

const loggedOut = () => window.dispatchEvent(new Event("vops:logout"));

export async function request(method, path, body) {
  const opts = { method, headers: {} };
  if (method !== "GET") opts.headers["X-Vops"] = "1";
  if (body instanceof Blob) {
    opts.headers["Content-Type"] = "application/gzip"; // a file (backup upload): streamed as is
    opts.body = body;
  } else if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const res = await fetch("/api" + path, opts);
  if (res.status === 401 && path !== "/login") {
    loggedOut();
    throw new Error("login required");
  }
  const text = await res.text();
  if (!res.ok) throw new Error(errText(text) || res.statusText);
  return text;
}
export const errText = (text) => { try { return JSON.parse(text).error || text; } catch { return text; } };
export async function api(method, path, body) {
  const text = await request(method, path, body);
  try { return JSON.parse(text); } catch { return text; }
}
export const apiText = (path) => request("GET", path); // plain text (plan, files): never parsed as json

// stream reads a text response chunk by chunk into sink.write, keeping sink.el scrolled to the bottom when it was; returns a cancel function.
export function stream(path, sink, opts = {}) {
  const ctl = new AbortController();
  const fail = (msg) => (opts.error ? opts.error(msg) : sink.write("\n" + msg));
  (async () => {
    try {
      const res = await fetch("/api" + path, { method: opts.method || "GET", headers: { "X-Vops": "1", "Content-Type": "application/json" }, body: opts.body, signal: ctl.signal });
      if (res.status === 401) return loggedOut();
      if (!res.ok) return fail(errText(await res.text()));
      opts.response && opts.response(res);
      const reader = res.body.getReader();
      const dec = new TextDecoder();
      for (;;) {
        // oxlint-disable-next-line no-await-in-loop -- chunks are read in order
        const { value, done } = await reader.read();
        if (done) break;
        const el = sink.el;
        const stick = el.scrollTop + el.clientHeight >= el.scrollHeight - 30;
        sink.write(dec.decode(value, { stream: true }));
        if (stick) {
          opts.stuck && opts.stuck();
          el.scrollTop = el.scrollHeight;
        }
      }
      opts.done && opts.done();
    } catch (e) {
      if (e.name !== "AbortError") fail(e.message);
    }
  })();
  return () => ctl.abort();
}
export const preSink = (pre) => ({ el: pre, write: (t) => pre.append(t) });
