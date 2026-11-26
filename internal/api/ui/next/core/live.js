// The WebSocket client for GET /ws (design 9.2). It reconnects on its own
// with a growing delay, and treats a connection that has been silent for 40 s
// as dead (the server pings every 15 s).
//
// The new shell's copy of core/live.js (same exports). One change: when the
// server's hello lists the "requests" topic (the request log, live) or the
// "health" topic (the judged health summary), it subscribes again with those
// topics added. A subscribe replaces the whole set,
// so every topic is named again; on a server without the topic nothing
// changes. core/requests.js reads the frames.
//
// The "models" topic (the model catalog, core/catalog.js) is named only while
// the open screen asked for it (state.catalogLive, set by needCatalog()): the
// server computes every profile's plan for it, so a screen that does not draw
// the catalog must not keep that going.

import { state, set, emit, publish, on } from './store.js';
import { setServerTime } from './format.js';
import { url } from './api.js';

const TOPICS = ['status', 'event', 'bench', 'config'];
const OPT_IN = ['requests', 'health'];
const SILENCE_MS = 40000;
const MAX_EVENTS = 300;

let ws = null;
let sent = ''; // the topics of the last subscribe on this connection
let attempt = 0;
let retryTimer = 0;
let watchdog = 0;

function wsURL() {
  // The page lives at <base>/ui/; the channel at <base>/ws.
  const u = new URL(url('/ws'), location.href);
  u.protocol = u.protocol === 'https:' ? 'wss:' : 'ws:';
  u.hash = '';
  u.search = '';
  return u.href;
}

// topics(): what this page subscribes to now
function topics() {
  const t = TOPICS.slice();
  if (state.catalogLive) t.push('models');
  const hello = state.hello;
  if (ws && ws.helloSeen && hello) for (const x of OPT_IN) if (Array.isArray(hello.topics) && hello.topics.includes(x)) t.push(x);
  return t;
}
// subscribe(): send the topics when they differ from the last subscribe
function subscribe(sock) {
  const t = topics();
  const key = t.join(',');
  if (!sock || key === sent) return;
  try { sock.send(JSON.stringify({ type: 'subscribe', topics: t })); sent = key; } catch { /* closing */ }
}
// The open screen asked for the catalog or let it go: subscribe again once
// the screen change is over (a release and the next screen's ask in one
// change send nothing).
let pending = false;
on('catalogLive', () => {
  if (pending) return;
  pending = true;
  queueMicrotask(() => { pending = false; if (ws && ws.readyState === WebSocket.OPEN) subscribe(ws); });
});

function setConn(c) {
  if (state.conn === c) return;
  state.connSince = Date.now();
  set('conn', c);
}

function handle(msg) {
  state.lastMsg = Date.now();
  const d = msg.data || {};
  switch (msg.type) {
    case 'hello':
      if (d.server_time) setServerTime(d.server_time);
      if (d.config_revision != null) set('configRevision', d.config_revision);
      if (ws) ws.helloSeen = true;
      set('hello', d);
      subscribe(ws);
      break;
    case 'status':
      state.statusAt = Date.now();
      if (d.config_revision != null && d.config_revision !== state.configRevision) set('configRevision', d.config_revision);
      set('status', d);
      break;
    case 'event':
      state.events.unshift(d);
      if (state.events.length > MAX_EVENTS) state.events.length = MAX_EVENTS;
      emit('events');
      break;
    case 'bench':
      if (d.kind === 'state') {
        state.bench.state = d.state || null;
        state.bench.run = (d.state && d.state.run) || null;
      } else if (d.kind === 'progress') {
        if (d.run) state.bench.run = d.run;
      } else if (d.kind === 'run') {
        const r = d.run || {};
        const final = ['completed', 'failed', 'cancelled', 'skipped'].includes(r.state);
        // the queue (the bench state's "queue", read on connect) follows each
        // run's events: queued joins its end, any other state leaves it
        if (state.bench.state && r.id) {
          const q = (state.bench.state.queue || []).filter(x => x.id !== r.id);
          if (r.state === 'queued') q.push(r);
          state.bench.state = Object.assign({}, state.bench.state, { queue: q });
        }
        if (final && state.bench.run && state.bench.run.id === r.id) state.bench.run = null;
      }
      emit('bench');
      break;
    case 'config':
      if (d.revision != null) set('configRevision', d.revision);
      break;
    case 'ping':
      try { ws && ws.send('{"type":"pong"}'); } catch { /* closing */ }
      break;
  }
  publish(msg.type, d, msg.ts);
}

function connect() {
  clearTimeout(retryTimer);
  let sock;
  try { sock = new WebSocket(wsURL()); } catch { schedule(); return; }
  ws = sock;
  sent = '';
  sock.onopen = () => {
    attempt = 0;
    state.lastMsg = Date.now();
    setConn('live');
    subscribe(sock);
  };
  sock.onmessage = e => {
    let msg;
    try { msg = JSON.parse(e.data); } catch { return; }
    if (msg && msg.type) handle(msg);
  };
  sock.onclose = () => {
    if (ws !== sock) return;
    ws = null;
    schedule();
  };
  sock.onerror = () => { try { sock.close(); } catch { /* already closed */ } };
}

function schedule() {
  setConn(state.hello ? 'reconnecting' : 'connecting');
  attempt++;
  const delay = Math.min(5000, 400 * 2 ** Math.min(attempt, 4)) + Math.random() * 300;
  clearTimeout(retryTimer);
  retryTimer = setTimeout(connect, delay);
}

export function start() {
  connect();
  clearInterval(watchdog);
  watchdog = setInterval(() => {
    if (ws && ws.readyState === WebSocket.OPEN && Date.now() - state.lastMsg > SILENCE_MS) {
      const dead = ws;
      ws = null;
      try { dead.close(); } catch { /* ignore */ }
      schedule();
    }
  }, 5000);
  // A phone that wakes up reconnects at once instead of waiting out the delay.
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden && !ws) { attempt = 0; connect(); }
  });
}
