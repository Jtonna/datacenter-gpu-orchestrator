// System health (design: Health board, the popup anchored to the heart at
// the foot of the rail): a title, the summary sentence with the worst dot,
// "Updated N s ago", the seven rows (Zones, Runs, Scheduler, MCP server,
// Storage, Queue, Alerts), "Last 2 hours" with "All activity", and "Alert
// settings".
//
// Everything in it is the server's: GET /api/health and the live channel's
// "health" topic carry one judged document (internal/api/health.go). The
// page draws it as written: the rows' sentences, the summary sentence, the
// heart's colour and issue count. Nothing is judged or reworded here.
//
// - GET /api/health when the shell starts, when the popup opens and after a
//   reconnect to a server without the topic;
// - the "health" topic (core/live.js subscribes when the hello lists it):
//   the whole document on subscribe, then again whenever something a person
//   reads changes.
//
// Row states: ok, attention, problem, and unknown for a row the server
// cannot judge (Alerts: "Not set up yet"). The design draws no row of that
// kind; it is drawn neutral, with a grey ring for a dot, and is not counted.
// The design draws no count on a row and none on the heart (the heart has a
// dot; its label says "System health, 2 issues"), so the rows' `count` is
// not drawn.

import { h, icon } from './dom.js';
import { state, set, listen } from './store.js';
import * as api from './api.js';
import { clock, durShort, plural } from './format.js';

const I = {
  chev: 'M8 5l5 5-5 5',
  x: 'M5 5l10 10M15 5L5 15',
};

// a row's state -> the dot's class (app.css .hd)
const DOT = { ok: 'ok', attention: 'warn', problem: 'bad', unknown: 'wait' };
// the heart's colour -> the rail dot's class (app.css .rb .dotmark)
const HEART = { red: 'bad', amber: 'warn' };

// setHealth(doc): a new document from either source.
function setHealth(d) {
  if (!d || !Array.isArray(d.rows)) return;
  state.healthAt = Date.now();
  set('health', d);
}
let frames = 0; // frames of the topic so far
listen('health', d => { frames++; setHealth(d); });

let reading = null;
// readHealth(onDone): GET /api/health (at most one read at a time). An
// answer is dropped when a frame of the topic came while it was on its way:
// the frame is the newer document.
export function readHealth(onDone) {
  if (!reading) {
    const seen = frames;
    reading = api.get('/api/health').then(d => { if (frames === seen) setHealth(d); }, () => {}).finally(() => { reading = null; });
  }
  return reading.then(onDone);
}

// heart() -> {level, issues}: the rail dot's class ('' for none) and the
// issue count, both from the server's document.
export function heart() {
  const d = state.health;
  if (!d) return { level: '', issues: 0 };
  const issues = Number(d.issues) || 0;
  return { level: issues > 0 ? HEART[d.color] || '' : '', issues };
}

// a "Last 2 hours" line: the zone it starts with in mono, then the server's
// words; a set-aside's end time as the design writes it ("until 16:20")
function line(e) {
  const t = String(e.text || '');
  const until = e.until ? ' until ' + clock(Date.parse(e.until), false) : '';
  if (e.node && t.startsWith(e.node)) return [h('span.mono', e.node), t.slice(e.node.length) + until];
  return t + until;
}

function updated() {
  if (state.conn !== 'live') return state.conn === 'reconnecting' ? 'Reconnecting…' : 'Connecting…';
  const at = Math.max(state.healthAt || 0, state.health ? state.statusAt || 0 : 0);
  return at ? 'Updated ' + durShort(Math.max(0, (Date.now() - at) / 1000)) + ' ago' : 'Waiting for data';
}

// healthPopup(onClose, onGo) -> the popup (the design's .hp). onGo(hash)
// follows a link of it (the shell closes the popup and goes there).
// rowText(row) -> the row's sentence as the server wrote it, with
// facts.release set in mono where the sentence names it (the Scheduler row,
// as the Health board draws it). The sentence wraps; it is never cut.
function rowText(r) {
  const rel = r.facts && typeof r.facts.release === 'string' ? r.facts.release : '';
  const t = String(r.text || '');
  const i = rel ? t.indexOf(rel) : -1;
  return i < 0 ? t : [t.slice(0, i), h('span.mono', rel), t.slice(i + rel.length)];
}

export function healthPopup(onClose, onGo) {
  const d = state.health;
  const go = to => e => { if (!to) return; if (e) e.preventDefault(); onGo(to); };
  const link = (cls, to, attrs, ...kids) => h('a', Object.assign({ class: cls, href: to, onclick: go(to) }, attrs), ...kids);
  const rows = d ? d.rows : [];
  const recent = d ? (d.recent || []).slice(0, 5) : [];
  return h('div.hp', { role: 'dialog', 'aria-label': 'System health', key: 'hp' },
    h('div.hp-head',
      h('div', { style: 'flex:1' },
        h('div.hp-title', 'System health'),
        h('div.hp-sum', h('span', { class: 'hd ' + (d ? DOT[d.state] || 'wait' : 'wait') }), d ? d.sentence : 'Waiting for data')),
      h('span.mut.hp-upd', updated()),
      h('button.hp-x', { type: 'button', 'aria-label': 'Close', onclick: onClose }, icon(I.x))),
    rows.map(r => link('hp-row' + (r.state === 'unknown' ? ' unknown' : ''), r.link || null,
      { key: 'hr-' + r.id, 'data-health': r.id, 'data-state': r.state },
      h('span', { class: 'hd ' + (DOT[r.state] || 'wait') }),
      h('span.hr-name', r.label),
      h('span.mut.hr-text', rowText(r)),
      h('span.hr-chev', { 'aria-hidden': 'true' }, icon(I.chev)))),
    h('div.hp-last',
      h('span', h('b', 'Last 2 hours')),
      d && d.recent_link ? link('', d.recent_link, {}, 'All activity') : null),
    recent.length
      ? recent.map(e => h('div.hp-ev', { key: 'hi-' + e.seq }, h('time.mono.mut', { title: clock(Date.parse(e.at)) }, clock(Date.parse(e.at), false)), h('span', line(e))))
      : h('div.hp-ev', h('span'), h('span.mut', d ? 'Nothing to report in the last 2 hours.' : 'Waiting for data')),
    d && d.alerts_link ? link('hp-foot', d.alerts_link, {}, 'Alert settings', icon(I.chev)) : null);
}

// the heart's label: "System health, 2 issues", as drawn
export function heartLabel() {
  const { issues } = heart();
  return issues ? 'System health, ' + plural(issues, 'issue') : 'System health';
}
