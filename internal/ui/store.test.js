import { test } from "node:test";
import assert from "node:assert/strict";
import { createStore, select } from "./static/js/lib/store.js";

test("set merges and notifies; select fires only when its slice changes; unsubscribe stops it", () => {
  const s = createStore({ a: 1, b: 1 });
  const seen = [];
  const off = select(s, (x) => x.a, (v) => seen.push(v));
  s.set({ b: 2 });
  s.set((x) => ({ a: x.a + 1 }));
  s.set(null); // no-op
  assert.deepEqual(s.get(), { a: 2, b: 2 });
  assert.deepEqual(seen, [2]);
  off();
  s.set({ a: 3 });
  assert.deepEqual(seen, [2]);
});
