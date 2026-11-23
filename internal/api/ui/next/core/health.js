// System health (design: Health board, the popup anchored to the heart at
// the foot of the rail). Rows in the design's order: Zones, Runs, Scheduler,
// MCP server, Storage, Queue, Alerts; then "Last 2 hours", "All activity" and
// "Alert settings". Each row has a dot (ok, warn, bad), a name, one line of
// facts and a chevron to the page that explains it.
//
// What feeds it, without a timer of its own:
// - the live status (Zones, Queue, the "Updated" age) and the requests
//   topic's copy (Runs: failed and refused requests of the last hour);
// - reads made when the shell starts, after each reconnect and when the
//   popup opens: GET /api/system (Scheduler, Storage), GET /api/agents (MCP),
//   GET /api/config/settings (the queue time limit), GET
//   /api/requests/summary (Runs, while the topic is not serving) and GET
//   /api/history (the last 2 hours, warnings and errors).
// Rows whose data does not exist yet are drawn and marked "waiting for
// data" with a grey dot and no count toward the heart: Alerts (no alert
// store), the disk size in Storage (the scheduler reports only its own
// files).

import { h, icon } from './dom.js';
import { state } from './store.js';
import * as api from './api.js';
import { href } from './router.js';
import { num, dur, durShort, clock, bytes, plural, parseGoDuration } from './format.js';
import { failedSince, pushReady } from './requests.js';

const HOUR = 3600000;
const I = {
  chev: 'M8 5l5 5-5 5',
  x: 'M5 5l10 10M15 5L5 15',
};

const got = { system: null, agents: null, limitS: 0, sum: null, events: null, at: 0 };
let reading = null;
// readHealth(onDone): the reads the rows need (at most one at a time).
export function readHealth(onDone) {
  if (reading) return reading.then(onDone);
  const now = Date.now();
  const one = (p, f) => p.then(f, () => {});
  reading = Promise.all([
    one(api.get('/api/system'), v => { got.system = v; }),
    one(api.get('/api/agents'), v => { got.agents = (v && v.agents) || []; }),
    one(api.get('/api/config/settings'), v => { got.limitS = parseGoDuration(((v && v.settings) || v || {}).queue_timeout) || 0; }),
    one(api.get('/api/requests/summary', { from: now - HOUR, to: now, step: 'hour' }), v => { got.sum = v; }),
    one(api.get('/api/history', { since: now - 2 * HOUR, severity: 'warn', limit: 50 }), v => { got.events = (v && v.events) || []; }),
  ]).then(() => { got.at = Date.now(); }).finally(() => { reading = null; });
  return reading.then(onDone);
}

const nodesOf = st => (st && st.view && st.view.nodes) || [];
const agentOff = (st, n) => ((st && st.agents) || []).some(a => a.id === n.id && a.excluded);

// healthRows() -> [{id, name, level: ok|warn|bad|wait, text, title, to}]
export function healthRows() {
  const st = state.status;
  const rows = [];
  const now = Date.now();

  // Zones: down = offline or set aside; switched off zones are not down
  const nodes = nodesOf(st);
  if (!st) rows.push({ id: 'zones', name: 'Zones', level: 'wait', text: 'Waiting for the first status', to: href('zones') });
  else {
    const down = nodes.filter(n => !(n.excluded || agentOff(st, n)) && (!n.online || n.quarantined)).length;
    rows.push({ id: 'zones', name: 'Zones', level: down ? 'bad' : 'ok',
      text: `${num(nodes.length - down)} of ${num(nodes.length)} reachable` + (down ? ` · ${num(down)} down` : ''),
      to: href('zones', [], down ? { show: 'down' } : {}) });
  }

  // Runs: client requests that failed or were refused in the last hour
  // the topic's copy is live but holds only the newest rows; the summary read
  // counts the whole hour as of its read: the larger of the two is shown
  const live = pushReady() ? failedSince(now - HOUR) : null;
  const read = got.sum && got.sum.on !== false ? ((got.sum.totals || {}).failed || 0) + ((got.sum.totals || {}).refused || 0) : null;
  const failed = live == null ? read : read == null ? live : Math.max(live, read);
  if (failed == null) rows.push({ id: 'runs', name: 'Runs', level: 'wait', text: got.sum && got.sum.on === false ? 'The request log is off' : 'Waiting for data', to: href('requests') });
  else rows.push({ id: 'runs', name: 'Runs', level: failed ? 'warn' : 'ok', text: failed ? `${num(failed)} failed in the last hour` : 'None failed in the last hour',
    title: 'Client requests that failed or were refused. Failed benchmark runs are not counted yet.',
    to: href('requests', [], { status: 'failed,refused', from: now - HOUR }) });

  // Scheduler: release and uptime; amber on recent background failures,
  // recovered read panics or a stored configuration from a newer version
  const sys = got.system;
  if (!sys) rows.push({ id: 'scheduler', name: 'Scheduler', level: 'wait', text: 'Waiting for data', to: href('about') });
  else {
    const tasks = sys.tasks || [];
    const dead = tasks.filter(t => t.policy === 'exit' && t.state !== 'running' && t.state !== 'done');
    const recent = tasks.filter(t => t.last_failure_at && now - t.last_failure_at < HOUR);
    const why = [];
    if (dead.length) why.push(plural(dead.length, 'essential task') + ' stopped');
    if (recent.length) why.push(plural(recent.length, 'background task') + ' failed in the last hour');
    if (sys.read_panics > 0) why.push(plural(sys.read_panics, 'read panic') + ' recovered');
    if (sys.stored_config && sys.stored_config.writes_need_force) why.push('the stored configuration is from a newer version');
    rows.push({ id: 'scheduler', name: 'Scheduler', level: dead.length ? 'bad' : why.length ? 'warn' : 'ok',
      text: ['Running · release ', h('span.mono', sys.version || '?'), ' · up ' + durShort(sys.uptime_s)],
      title: why.length ? why.join('; ') : null, to: href('about') });
  }

  // MCP server: it runs inside the scheduler; the keys that are not revoked
  if (!got.agents) rows.push({ id: 'mcp', name: 'MCP server', level: 'wait', text: 'Waiting for data', to: href('mcp') });
  else {
    const active = got.agents.filter(a => !a.revoked_at).length;
    rows.push({ id: 'mcp', name: 'MCP server', level: 'ok', text: `Running · ${plural(active, 'key')} active`, to: href('mcp') });
  }

  // Storage: the scheduler's own files; the disk size is not reported yet
  if (!sys) rows.push({ id: 'storage', name: 'Storage', level: 'wait', text: 'Waiting for data', to: href('about') });
  else {
    const s = sys.storage || {};
    const used = ['history', 'database', 'usage', 'child_logs'].reduce((t, k) => t + ((s[k] && (s[k].bytes || s[k].size_bytes)) || 0), 0);
    rows.push({ id: 'storage', name: 'Storage', level: 'wait', text: `${bytes(used)} of Potluck data · disk: waiting for data`,
      title: 'The scheduler reports the size of its own files; the size and free space of the disk are not reported yet.', to: href('about') });
  }

  // Queue: waiting requests and the oldest wait; amber past half the time limit
  if (!st) rows.push({ id: 'queue', name: 'Queue', level: 'wait', text: 'Waiting for the first status', to: href('queue') });
  else {
    const q = st.queue || [];
    const waiting = q.reduce((n, e) => n + Math.max(1, e.waiters || 0), 0);
    const oldest = q.reduce((m, e) => Math.max(m, e.age || 0), 0);
    const late = got.limitS && oldest > got.limitS / 2;
    rows.push({ id: 'queue', name: 'Queue', level: late ? 'warn' : 'ok',
      text: waiting ? `${num(waiting)} waiting · longest ${dur(Math.round(oldest))}` : 'Nothing waiting',
      title: got.limitS ? 'The queue time limit is ' + dur(got.limitS) : null, to: href('queue') });
  }

  // Alerts: there is no alert store yet
  rows.push({ id: 'alerts', name: 'Alerts', level: 'wait', text: 'Not built yet: waiting for data', title: 'There is no alert store yet: webhooks and their deliveries come with it.', to: null });
  return rows;
}

// heart() -> {level, issues}: the worst row and how many need attention.
export function heart() {
  const rows = healthRows();
  const issues = rows.filter(r => r.level === 'bad' || r.level === 'warn').length;
  const level = rows.some(r => r.level === 'bad') ? 'bad' : issues ? 'warn' : 'ok';
  return { level, issues };
}

// a history event in a few words, as the design writes them
function sentence(e) {
  const node = (e.nodes || [])[0];
  const d = e.data || {};
  if (e.type === 'nodeQuarantined' && node) return [h('span.mono', node), ' failed a load and was set aside' + (d.for_s ? ' until ' + clock(e.ts + d.for_s * 1000, false) : '')];
  if (e.type === 'nodeLost' && node) return [h('span.mono', node), ' went offline'];
  const s = String(e.summary || e.type || '');
  const cut = s.indexOf('. ');
  return cut > 0 && cut < 140 ? s.slice(0, cut + 1) : s.length > 140 ? s.slice(0, 139) + '…' : s;
}

function recent() {
  const since = Date.now() - 2 * HOUR;
  const seen = new Set();
  const all = [].concat(state.events || [], got.events || [])
    .filter(e => e && e.ts >= since && (e.severity === 'warn' || e.severity === 'error'))
    .filter(e => { const k = e.seq != null ? 's' + e.seq : e.ts + e.type; if (seen.has(k)) return false; seen.add(k); return true; })
    .sort((a, b) => b.ts - a.ts);
  return all.slice(0, 5);
}

// healthPopup(onClose, onGo) -> the popup (the design's .hp).
export function healthPopup(onClose, onGo) {
  const rows = healthRows();
  const issues = rows.filter(r => r.level === 'bad' || r.level === 'warn').length;
  const worst = rows.some(r => r.level === 'bad') ? 'bad' : issues ? 'warn' : 'ok';
  const age = state.statusAt ? Math.max(0, (Date.now() - state.statusAt) / 1000) : null;
  const updated = state.conn !== 'live' ? 'Reconnecting…' : age == null ? 'Waiting for data' : 'Updated ' + durShort(age) + ' ago';
  const ev = recent();
  const go = to => e => { if (!to) return; if (e) e.preventDefault(); onGo(to); };
  return h('div.hp', { role: 'dialog', 'aria-label': 'System health', key: 'hp' },
    h('div.hp-head',
      h('div', { style: 'flex:1' },
        h('div.hp-title', 'System health'),
        h('div.hp-sum', h('span', { class: 'hd ' + worst }), issues ? `${plural(issues, 'thing')} need${issues === 1 ? 's' : ''} attention` : !state.status ? 'Waiting for data' : 'All good')),
      h('span.mut.hp-upd', updated),
      h('button.hp-x', { type: 'button', 'aria-label': 'Close', onclick: onClose }, icon(I.x))),
    // a row is a clickable line, as drawn (not a link): Enter opens it from the keyboard
    rows.map(r => h('div', { class: 'hp-row' + (r.to ? '' : ' nolink'), key: 'hr-' + r.id, tabindex: r.to ? '0' : null, onclick: r.to ? go(r.to) : null,
      onkeydown: r.to ? (e => { if (e.key === 'Enter') go(r.to)(e); }) : null, title: r.title || null, 'data-health': r.id },
      h('span', { class: 'hd ' + r.level }),
      h('span.hr-name', r.name),
      h('span.mut.hr-text', r.text),
      h('span.hr-chev', { 'aria-hidden': 'true' }, r.to ? icon(I.chev) : null))),
    h('div.hp-last',
      h('span', h('b', 'Last 2 hours')),
      h('a', { href: href('activity', [], { range: '6h', sev: 'warn' }), onclick: go(href('activity', [], { range: '6h', sev: 'warn' })) }, 'All activity')),
    ev.length
      ? ev.map(e => h('div.hp-ev', { key: 'hi-' + (e.seq != null ? e.seq : e.ts) }, h('time.mono.mut', { title: clock(e.ts) }, clock(e.ts, false)), h('span', sentence(e))))
      : h('div.hp-ev', h('span'), h('span.mut', got.events == null && !(state.events || []).length ? 'Reading the last 2 hours…' : 'Nothing to report in the last 2 hours.')),
    h('span.hp-foot', { title: 'Alert settings are not built yet (waiting for the alert store)', 'aria-disabled': 'true' }, 'Alert settings', icon(I.chev)));
}
