// The request log as the screens read it (GET /api/requests, the request
// log of usage.db plus the live rows). A view asks for a feed and gets its
// rows through a callback; it never knows how they arrive.
//
// Today they arrive by polling: every 2 s while the page is visible, and
// not at all while it is hidden (one read at once when it shows again).
// When the WebSocket gets a requests topic, requestFeed() subscribes to it
// instead and the views stay as they are.
//
//   const feed = requestFeed({limit: 8}, data => draw(data));
//   feed.set({from, to, limit});   // new parameters: read again at once
//   feed.stop();
//
// data: {rows, on, historyBeginsAt, next, at (unix ms of the last good
// read), error (the last read's failure, or ''), loading (no read yet)}.

import * as api from './api.js';

export const POLL_MS = 2000;

// every(ms, fn) -> stop(): the one timer the new shell's views use, kept here
// with the poll so all the shell's timers are in one place (and leave with the
// poll when the server pushes requests): the Overview's 100 ms clock of the
// running rows (it fetches nothing), and its slower reads of the summary and
// of the availability series, which have no push topic either.
export function every(ms, fn) { const t = setInterval(fn, ms); return () => clearInterval(t); }

export function requestFeed(params, onData) {
  let p = Object.assign({}, params);
  let timer = 0, seq = 0, dead = false;
  const data = { rows: [], on: true, historyBeginsAt: 0, next: 0, at: 0, error: '', loading: true };

  async function read() {
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
    onData(data);
  }

  function schedule() {
    clearInterval(timer);
    timer = 0;
    if (dead || document.hidden || p.poll === false) return;
    timer = setInterval(read, POLL_MS);
  }

  const onVisible = () => {
    if (dead) return;
    if (!document.hidden) read();
    schedule();
  };
  document.addEventListener('visibilitychange', onVisible);
  read();
  schedule();

  return {
    data,
    set(np) { p = Object.assign({}, np); data.loading = true; read(); schedule(); },
    refresh: read,
    stop() { dead = true; clearInterval(timer); document.removeEventListener('visibilitychange', onVisible); },
  };
}
