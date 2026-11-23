// The request log as the screens read it (GET /api/requests, the request
// log of usage.db plus the live rows). A view asks for a feed and gets its
// rows through a callback; it never knows how they arrive.
//
// Two ways, chosen per moment:
// - push: the WebSocket topic "requests" (core/live.js subscribes when the
//   server's hello lists it). A snapshot (the first page of GET
//   /api/requests), then queued / started / ended frames and, while
//   something runs, a tick every second with the running rows. Rows are kept
//   here by id, the newest frame wins. A feed uses it for the first page with
//   no state or model filter: {limit, from} (the Overview, the first page of
//   Requests).
// - poll: GET /api/requests every 2 s while the page is visible, not at all
//   while it is hidden (one read at once when it shows again). Used for any
//   other page, on a server without the topic, and whenever the channel is
//   not live or its snapshot has not come yet (the poll timer runs in both
//   modes and reads only when push cannot serve).
//
//   const feed = requestFeed({limit: 8}, data => draw(data));
//   feed.set({from, to, limit});   // new parameters: read again at once
//   feed.stop();
//
// data: {rows, on, historyBeginsAt, next, at (unix ms of the last good
// read or frame), error (the last read's failure, or ''), loading (no read
// yet), push (true while rows come from the topic)}.

import * as api from './api.js';
import { state, listen } from './store.js';

export const POLL_MS = 2000;
const KEEP_DONE = 500; // finished rows kept from the topic

// every(ms, fn) -> stop(): the one timer the new shell's views use, kept here
// with the poll so all the shell's timers are in one place: the Overview's
// 100 ms clock of the running rows (it fetches nothing), and its slower reads
// of the summary and of the availability series, which have no push topic.
export function every(ms, fn) { const t = setInterval(fn, ms); return () => clearInterval(t); }

// ---------- the topic's copy of the log ----------

const push = { have: false, rows: new Map(), on: true, historyBeginsAt: 0, next: 0, at: 0 };
const pushFeeds = new Set();
const LIVE = { queued: 0, running: 1 };
const rank = r => (r.status in LIVE ? LIVE[r.status] : 2);
// queued (newest first), then running, then finished (newest first), as GET /api/requests
const order = (a, b) => rank(a) - rank(b) || b.id - a.id;

function putRow(r) {
  if (!r || r.id == null) return;
  push.rows.set(r.id, r);
}
function trim() {
  const done = Array.from(push.rows.values()).filter(r => rank(r) === 2).sort(order);
  for (const r of done.slice(KEEP_DONE)) push.rows.delete(r.id);
}
// applyRequestsFrame(d): one frame of the topic (exported for tests).
export function applyRequestsFrame(d) {
  if (!d || !d.kind) return;
  switch (d.kind) {
    case 'snapshot':
      push.rows = new Map();
      for (const r of d.requests || []) putRow(r);
      push.on = d.on !== false;
      push.historyBeginsAt = d.history_begins_at || 0;
      push.next = d.next || 0;
      push.have = true;
      break;
    case 'queued': case 'started': case 'ended':
      if (!push.have) return;
      putRow(d.request);
      if (d.kind === 'ended') trim();
      break;
    case 'tick':
      if (!push.have) return;
      for (const r of d.requests || []) putRow(r);
      break;
    default: return;
  }
  push.at = Date.now();
  for (const fn of Array.from(pushFeeds)) fn();
}
listen('requests', applyRequestsFrame);
// a new connection: wait for its snapshot before trusting the copy
listen('hello', () => { push.have = false; });

const topicOffered = () => !!(state.hello && Array.isArray(state.hello.topics) && state.hello.topics.includes('requests'));
const pushReady = () => push.have && state.conn === 'live' && topicOffered();
// the parameters push can answer: the first page, no state or model filter, up to now
const pushable = p => p.poll !== false && !p.before && !p.to && !p.status && !p.profile;

function fromPush(p) {
  const from = Number(p.from) || 0;
  const all = Array.from(push.rows.values()).sort(order);
  const rows = all.filter(r => rank(r) < 2 || !from || (r.queued_at || 0) >= from);
  const limit = Number(p.limit) || 50;
  const shown = rows.slice(0, limit);
  const last = shown[shown.length - 1];
  return { rows: shown, next: rows.length > limit && last ? last.id : push.next };
}

// ---------- the feed ----------

export function requestFeed(params, onData) {
  let p = Object.assign({}, params);
  let timer = 0, seq = 0, dead = false;
  const data = { rows: [], on: true, historyBeginsAt: 0, next: 0, at: 0, error: '', loading: true, push: false };

  function serve() {
    if (dead || !pushable(p) || !pushReady()) { data.push = false; return false; }
    const v = fromPush(p);
    data.rows = v.rows;
    data.next = v.next;
    data.on = push.on;
    data.historyBeginsAt = push.historyBeginsAt;
    data.at = push.at;
    data.error = '';
    data.loading = false;
    data.push = true;
    seq++; // a read still on its way is older than this
    onData(data);
    return true;
  }
  const onPush = () => { serve(); };
  pushFeeds.add(onPush);

  async function read() {
    if (serve()) return;
    const mine = ++seq;
    try {
      const q = Object.assign({}, p);
      delete q.poll; // the feed's own option, not a query parameter
      const res = await api.get('/api/requests', q);
      if (dead || mine !== seq) return;
      data.rows = Array.isArray(res && res.requests) ? res.requests : [];
      data.on = !res || res.on !== false;
      data.historyBeginsAt = (res && res.history_begins_at) || 0;
      data.next = (res && res.next) || 0;
      data.at = Date.now();
      data.error = '';
    } catch (e) {
      if (dead || mine !== seq) return;
      data.error = (e && e.message) || 'the request failed';
    }
    data.loading = false;
    data.push = false;
    onData(data);
  }

  function schedule() {
    clearInterval(timer);
    timer = 0;
    if (dead || document.hidden || p.poll === false) return;
    // in push mode the tick reads nothing (read() serves from the copy)
    timer = setInterval(() => { if (!data.push || !pushReady()) read(); }, POLL_MS);
  }

  const onVisible = () => {
    if (dead) return;
    if (!document.hidden) read();
    schedule();
  };
  document.addEventListener('visibilitychange', onVisible);
  // the first answer comes after the caller has its feed, even when push has it at hand
  queueMicrotask(read);
  schedule();

  return {
    data,
    set(np) { p = Object.assign({}, np); data.loading = true; read(); schedule(); },
    refresh: read,
    stop() { dead = true; clearInterval(timer); pushFeeds.delete(onPush); document.removeEventListener('visibilitychange', onVisible); },
  };
}
