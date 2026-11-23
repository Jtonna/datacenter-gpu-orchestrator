// The request log as the screens read it. A view asks for a feed and gets
// its rows through a callback; it never knows how they arrive.
//
// The rows come from the live channel's "requests" topic (core/live.js
// subscribes when the server's hello lists it): a snapshot (the first page
// of GET /api/requests), then queued / started / ended frames and, while
// something runs, a tick every second with the running rows and their live
// figures. They are kept here by id; the newest frame wins, and an ended row
// replaces its live one.
//
// - The first page with no filter (the Overview, the first page of
//   Requests) is served from that copy alone.
// - Any other page (a state or model filter, a past range, an older page)
//   is read once with GET /api/requests, and the topic's rows are laid over
//   it: a row that now matches is added or updated, one that no longer
//   matches (a running row that ended, under ?status=running) leaves.
// - Only while the channel is down (or on a server without the topic) does
//   the feed poll GET /api/requests every 2 s, and only while the page is
//   visible. When the channel is back, its snapshot takes over and the
//   pages read once again.
//
//   const feed = requestFeed({limit: 8}, data => draw(data));
//   feed.set({from, to, limit, status, profile, before});
//   feed.stop();
//
// data: {rows, on, historyBeginsAt, next, at (unix ms of the last good read
// or frame), error (the last read's failure, or ''), loading (no answer
// yet), push (true while the topic serves), ended (a number that changes
// whenever a request has ended: a view re-reads its totals on it)}.

import * as api from './api.js';
import { state, listen, on } from './store.js';

export const POLL_MS = 2000;
const KEEP_DONE = 500; // finished rows kept from the topic

// every(ms, fn) -> stop(): the only timer of the new shell's views, kept in
// this file with the poll so the allowance of TestUIHasNoPollingTimer is one
// file: the 100 ms clock of the running rows' length (it reads nothing).
export function every(ms, fn) { const t = setInterval(fn, ms); return () => clearInterval(t); }

// ---------- the topic's copy of the log ----------

const push = { have: false, rows: new Map(), on: true, historyBeginsAt: 0, next: 0, at: 0, ended: 0 };
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
      push.ended++;
      break;
    case 'queued': case 'started': case 'ended':
      if (!push.have) return;
      putRow(d.request);
      if (d.kind === 'ended') { trim(); push.ended++; }
      break;
    case 'tick':
      if (!push.have) return;
      for (const r of d.requests || []) putRow(r);
      break;
    default: return;
  }
  push.at = Date.now();
  for (const fn of Array.from(pushFeeds)) fn(d.kind);
}
listen('requests', applyRequestsFrame);
// a new connection: wait for its snapshot before trusting the copy
listen('hello', () => { push.have = false; });

const topicOffered = () => !!(state.hello && Array.isArray(state.hello.topics) && state.hello.topics.includes('requests'));
// pushReady() -> true while the topic serves (live channel, topic offered, snapshot in).
export const pushReady = () => push.have && state.conn === 'live' && topicOffered();
// failedSince(ms) -> how many requests failed or were refused since ms, by
// the topic's copy (null while it does not serve). The health popup's Runs row.
export function failedSince(ms) {
  if (!pushReady()) return null;
  let n = 0;
  for (const r of push.rows.values()) if ((r.status === 'failed' || r.status === 'refused') && (r.ended_at || r.queued_at || 0) >= ms) n++;
  return n;
}
// the first page with no filter: the topic's copy alone answers it
const whole = p => !p.before && !p.to && !p.status && !p.profile;

// matches(p, row): does a row belong on the page p asks for?
function matches(p, r) {
  const st = String(p.status || '').split(',').filter(Boolean);
  if (st.length && !st.includes(r.status)) return false;
  if (p.profile && r.profile !== p.profile) return false;
  const from = Number(p.from) || 0, to = Number(p.to) || 0, before = Number(p.before) || 0;
  if (from && rank(r) === 2 && (r.queued_at || 0) < from) return false;
  if (to && (r.queued_at || 0) >= to) return false;
  if (before && r.id >= before) return false;
  return true;
}

// ---------- the feed ----------

export function requestFeed(params, onData) {
  let p = Object.assign({}, params);
  let timer = 0, seq = 0, dead = false;
  let base = null; // the rows of the last REST read, by id (null: none yet)
  let baseNext = 0, endedSig = '';
  const data = { rows: [], on: true, historyBeginsAt: 0, next: 0, at: 0, error: '', loading: true, push: false, ended: 0 };

  // the rows of the page from what is held: the topic's copy, the REST read, or both
  function compose() {
    const limit = Number(p.limit) || 50;
    let rows, next;
    if (pushReady() && whole(p)) {
      const all = Array.from(push.rows.values()).filter(r => matches(p, r)).sort(order);
      rows = all.slice(0, limit);
      const last = rows[rows.length - 1];
      next = all.length > limit && last ? last.id : push.next;
      data.on = push.on;
      data.historyBeginsAt = push.historyBeginsAt;
      data.at = push.at;
      data.push = true;
    } else if (base) {
      const merged = new Map(base);
      if (pushReady()) {
        for (const r of push.rows.values()) {
          if (matches(p, r)) merged.set(r.id, r); else merged.delete(r.id);
        }
        data.at = Math.max(data.at, push.at);
      }
      rows = Array.from(merged.values()).sort(order).slice(0, limit);
      next = baseNext;
      data.push = pushReady();
    } else return false;
    data.rows = rows;
    data.next = next;
    data.error = '';
    data.loading = false;
    // ended: the topic counts its ended frames; a read counts by its finished rows
    if (data.push) data.ended = 'p' + push.ended;
    else {
      const sig = rows.filter(r => rank(r) === 2).map(r => r.id + r.status).join(',');
      if (sig !== endedSig) { endedSig = sig; data.ended = 'r' + sig.length + ':' + rows.length + ':' + (rows[0] ? rows[0].id : 0) + ':' + Date.now(); }
    }
    return true;
  }

  function show() { if (!dead && compose()) onData(data); }

  async function read() {
    const mine = ++seq;
    try {
      const q = Object.assign({}, p);
      delete q.poll; // the feed's own option, not a query parameter
      const res = await api.get('/api/requests', q);
      if (dead || mine !== seq) return;
      base = new Map((Array.isArray(res && res.requests) ? res.requests : []).map(r => [r.id, r]));
      baseNext = (res && res.next) || 0;
      data.on = !res || res.on !== false;
      data.historyBeginsAt = (res && res.history_begins_at) || 0;
      data.at = Date.now();
      if (!compose()) return;
    } catch (e) {
      if (dead || mine !== seq) return;
      data.error = (e && e.message) || 'the request failed';
      data.loading = false;
    }
    onData(data);
  }

  // load(): what a page needs now. The topic's first page needs no read; any
  // other page is read once (and again after a reconnect's snapshot).
  function load() {
    if (pushReady() && whole(p)) { seq++; show(); return; }
    read();
  }

  // the poll runs only while the topic cannot serve
  function schedule() {
    clearInterval(timer);
    timer = 0;
    if (dead || document.hidden || p.poll === false || pushReady()) return;
    timer = setInterval(read, POLL_MS);
  }

  const onPush = kind => {
    if (dead) return;
    if (kind === 'snapshot') { schedule(); load(); return; }
    show();
  };
  pushFeeds.add(onPush);
  const offConn = on(['conn', 'hello'], () => { if (!dead) { schedule(); if (!pushReady() && !document.hidden) read(); } });

  const onVisible = () => {
    if (dead) return;
    if (!document.hidden) load();
    schedule();
  };
  document.addEventListener('visibilitychange', onVisible);
  // the first answer comes after the caller has its feed, even when the topic has it at hand
  queueMicrotask(load);
  schedule();

  return {
    data,
    set(np) { p = Object.assign({}, np); base = null; endedSig = ''; data.loading = true; load(); schedule(); },
    refresh: load,
    stop() { dead = true; clearInterval(timer); pushFeeds.delete(onPush); offConn(); document.removeEventListener('visibilitychange', onVisible); },
  };
}
