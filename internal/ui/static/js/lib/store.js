// createStore: a tiny zustand. get() reads, set(patch | state => patch) merges and notifies, subscribe(fn) returns its unsubscribe.
export function createStore(initial) {
  let state = initial;
  const subs = new Set();
  return {
    get: () => state,
    set(patch) {
      const next = typeof patch === "function" ? patch(state) : patch;
      if (!next) return;
      const prev = state;
      state = { ...state, ...next };
      for (const fn of Array.from(subs)) fn(state, prev); // a copy: a subscriber may unsubscribe while notified
    },
    subscribe(fn) {
      subs.add(fn);
      return () => subs.delete(fn);
    },
  };
}

// select calls fn(value) when pick(state) changes (Object.is); returns the unsubscribe.
export const select = (store, pick, fn) => store.subscribe((s, prev) => {
  const v = pick(s);
  if (!Object.is(v, pick(prev))) fn(v);
});
