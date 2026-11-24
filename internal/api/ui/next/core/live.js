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

import { state, set, emit, publish } from './store.js';
import { setServerTime } from './format.js';
import { url } from './api.js';

const TOPICS = ['status', 'event', 'bench', 'config', 'models'];
const OPT_IN = ['requests', 'health'];
const SILENCE_MS = 40000;
const MAX_EVENTS = 300;

let ws = null;
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
      set('hello', d);
      {
        const extra = OPT_IN.filter(t => Array.isArray(d.topics) && d.topics.includes(t));
        if (extra.length) try { ws && ws.send(JSON.stringify({ type: 'subscribe', topics: TOPICS.concat(extra) })); } catch { /* closing */ }
      }
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
  sock.onopen = () => {
    attempt = 0;
    state.lastMsg = Date.now();
    setConn('live');
    try { sock.send(JSON.stringify({ type: 'subscribe', topics: TOPICS })); } catch { /* closing */ }
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
