// The live store. Everything that arrives over the WebSocket lands here and
// views subscribe to the keys they draw. Views never poll.
//
//   state.conn      'connecting' | 'live' | 'reconnecting'
//   state.connSince unix ms of the last change of conn
//   state.lastMsg   unix ms (browser clock) of the last frame of any type
//   state.hello     the hello message data, or null before the first one
//   state.status    the newest status document (GET /api/status shape), or null
//   state.statusAt  unix ms (browser clock) when it arrived
//   state.events    newest history events first, as they arrived live (at most 300)
//   state.bench     {state, run}: the /api/bench/state document and the progress
//                   object of the active run (null when none)
//   state.configRevision  the newest config revision heard of, or null
//   state.tick      a counter that rises once a second (for "38 min ago")

export const state = {
  conn: 'connecting', connSince: Date.now(), lastMsg: 0,
  hello: null, status: null, statusAt: 0,
  events: [], bench: { state: null, run: null }, configRevision: null, tick: 0,
};

const subs = new Map(); // key -> Set<fn>

// on('status', fn) calls fn(value, key) after every change of that key;
// on(['status','tick'], fn) for several. Returns an unsubscribe function.
export function on(keys, fn) {
  const ks = [].concat(keys);
  for (const k of ks) {
    if (!subs.has(k)) subs.set(k, new Set());
    subs.get(k).add(fn);
  }
  return () => { for (const k of ks) subs.get(k)?.delete(fn); };
}

export function set(key, value) {
  state[key] = value;
  emit(key);
}

export function emit(key) {
  const s = subs.get(key);
  if (!s) return;
  for (const fn of Array.from(s)) {
    try { fn(state[key], key); } catch (e) { console.error('store subscriber for', key, e); }
  }
}

// Raw message bus: listen('bench', fn) gets every frame's data of that type
// (hello, status, event, bench, config, ping), for views that need each
// message and not only the latest value. Returns an unsubscribe function.
const listeners = new Map();
export function listen(type, fn) {
  if (!listeners.has(type)) listeners.set(type, new Set());
  listeners.get(type).add(fn);
  return () => listeners.get(type)?.delete(fn);
}
export function publish(type, data, ts) {
  const s = listeners.get(type);
  if (!s) return;
  for (const fn of Array.from(s)) {
    try { fn(data, ts); } catch (e) { console.error('listener for', type, e); }
  }
}

setInterval(() => set('tick', state.tick + 1), 1000);
