// Overview: the home page (design: Main board, "Overview"). A header row
// (period switch, date stepper, Customize, View 24-hour report), then the
// Tokens panel and the Requests panel side by side, then the zones panel
// with each zone's availability hour by hour.
//
// The design titles the requests panel "Runs"; it is "Requests" here (the
// owner's answer to Q2: Benchmarks owns "runs"). Everything else about it is
// as drawn.
//
// Clicking a request's Zones toggle pins it: the zones that served it (or,
// for a queued one, the zones it waits for) are outlined in the zones panel,
// neighbours joined into one box, the others dimmed, and the first one is
// scrolled into view inside the list (Q6). Hovering a toggle shows another
// request's zones while the pointer is on it and drops the pin (the design's
// script). On load the first running request is pinned, as drawn.
//
// On a past day the zones panel shows the current state in its first five
// columns and the chosen day in the strip (Q4); for a week or a month the
// strip shows its last day. The page opens with the section menu closed
// (Q9; core/shell.js).
//
// Reads: GET /api/requests (data layer core/requests.js: a 2 s poll while
// the page is visible, to be replaced by a push topic), GET
// /api/requests/summary (the totals and the series of the period), GET
// /api/nodes/availability (the strips, once a minute), and the live status
// (zones). Writes nothing; Customize is kept in this browser.

import { h } from '../core/dom.js';
import { registerView, navigate, href } from '../core/router.js';
import { state } from '../core/store.js';
import * as api from '../core/api.js';
import { liveView, segmented, dateStepper, button, chips, dropButton, onDropChange, zoneStatus, colourKey, availStrip, availClass, tokensChart, tokensLegend,
  runsTable, runLegend, tickRuns, groupClass, dialog, toggle, skeleton } from '../core/ui.js';
import { num, gb, gbNum, dur, clock, dateTime, DASH } from '../core/format.js';
import { viewNodes } from '../core/status.js';
import { agentOf, notPlaceable } from '../core/nodeactions.js';
import { requestFeed, every } from '../core/requests.js';
import { PERIODS, isPeriod, periodOf, bucketTitle, shortDay, ymd } from '../core/period.js';

// ---------- Customize (Q1): kept in this browser until the owner has a mockup for it ----------

const LAYOUT_KEY = 'potluck.overview.layout';
const PANELS = { tokens: 'Tokens', requests: 'Requests', zones: 'Zones' };
export function readLayout() {
  let v = null;
  try { v = JSON.parse(localStorage.getItem(LAYOUT_KEY) || 'null'); } catch { v = null; }
  const order = Array.isArray(v && v.order) ? v.order.filter(k => PANELS[k]) : [];
  for (const k of Object.keys(PANELS)) if (!order.includes(k)) order.push(k);
  const hidden = Array.isArray(v && v.hidden) ? v.hidden.filter(k => PANELS[k]) : [];
  return { order, hidden, period: v && isPeriod(v.period) ? v.period : 'day' };
}
function saveLayout(l) { try { localStorage.setItem(LAYOUT_KEY, JSON.stringify(l)); } catch { /* private mode: for this visit only */ } }

function customize(onSave) {
  const l = readLayout();
  const draw = box => {
    box.replaceChildren(...l.order.map((k, i) => h('div.cz-item', { key: k },
      toggle(!l.hidden.includes(k), on => { l.hidden = on ? l.hidden.filter(x => x !== k) : l.hidden.concat(k); }, 'Show ' + PANELS[k]),
      h('b', PANELS[k]),
      button('↑', () => { [l.order[i - 1], l.order[i]] = [l.order[i], l.order[i - 1]]; draw(box); }, 'sm', { 'aria-label': 'Move ' + PANELS[k] + ' up', disabled: i === 0 }),
      button('↓', () => { [l.order[i + 1], l.order[i]] = [l.order[i], l.order[i + 1]]; draw(box); }, 'sm', { 'aria-label': 'Move ' + PANELS[k] + ' down', disabled: i === l.order.length - 1 }))));
  };
  const box = h('div.cz-list');
  draw(box);
  const sel = h('select.select', { 'aria-label': 'Default period' }, PERIODS.map(p => h('option', { value: p.id, selected: p.id === l.period }, p.label)));
  sel.addEventListener('change', () => { l.period = sel.value; });
  return dialog({
    title: 'Customize the Overview',
    body: h('div.col', { style: 'gap:14px' },
      h('p', 'Show, hide and order the three panels, and choose the period the page opens on. This is kept in this browser.'),
      box, h('div.field', h('label', 'Opens on'), sel)),
    actions: [{ label: 'Reset', value: 'reset' }, { label: 'Cancel', value: null }, { label: 'Save', kind: 'pri', value: 'save', autofocus: true }],
  }).then(v => {
    if (v === 'reset') { try { localStorage.removeItem(LAYOUT_KEY); } catch { /* nothing kept */ } onSave(); }
    if (v === 'save') { saveLayout(l); onSave(); }
  });
}

// ---------- zones: the state of each machine in the design's words ----------

const ROLE = { host: 'Host', helper: 'Helper' };
const ASIDE = { preflight_failed: 'Load failed', agent_unreachable: 'Agent not reachable', worker_hung: 'Worker stopped answering', node_lost: 'Lost for a while' };
const DEFAULT_ACTOR = 'anonymous'; // the default user of the site (no login): everyone using it is "you"
const byWhom = actor => (!actor ? '' : actor === DEFAULT_ACTOR || actor === api.getActor() ? ' by you' : ' by ' + actor);
const tms = t => { const v = t ? Date.parse(t) : NaN; return Number.isNaN(v) ? null : v; };
const timeAt = (ms, title) => h('time', { datetime: new Date(ms).toISOString(), title: title || dateTime(ms) }, clock(ms, false));

// member(st, id) -> {inst, role, share} of the instance on node id, or null.
function member(st, id) {
  for (const inst of st.instances || []) {
    const m = (inst.nodes || []).find(x => x.id === id);
    if (m) return { inst, role: m.role || '', share: typeof m.share === 'number' ? m.share : null };
  }
  return null;
}

// zoneOf(st, n, i) -> one zones row: its group (use, free, down, off: the
// pills), its square and status word, what it is doing, memory, temperature,
// chip, and its rank for "needs attention first".
export function zoneOf(st, n, i) {
  const a = agentOf(st, n) || {};
  const name = n.hostname || a.name || n.id;
  const chip = a.chip || n.chip || '';
  const mem = memOf(n);
  const temp = a.stats && typeof a.stats.headline_temp_c === 'number' ? a.stats.headline_temp_c : null;
  const base = { id: n.id, name, chip, mem, temp, idx: i, title: null };
  const mono = t => h('span.mono', t);
  const sep = h('span.mut', ' · ');
  const m = member(st, n.id);
  const inst = m ? m.inst : n.holder ? (st.instances || []).find(x => x.profile === n.holder) : null;
  const q = (st.quarantines || []).find(x => x.node === n.id);
  const nowMs = Date.now();
  if (n.excluded || a.excluded) {
    const at = tms(a.excluded_at);
    return Object.assign(base, { group: 'off', square: 'off', label: 'Off', rank: 4,
      title: 'Excluded in the configuration' + (n.excluded_by || a.excluded_by ? ' (exclude_nodes: ' + (n.excluded_by || a.excluded_by) + ')' : '') + (at ? ', ' + dateTime(at) : ''),
      doing: h('span.mut', 'Switched off' + byWhom(a.excluded_actor)) });
  }
  if (!n.online) {
    const since = tms(n.missing_since);
    const seen = typeof a.last_seen_ago_s === 'number' ? nowMs - a.last_seen_ago_s * 1000 : since;
    return Object.assign(base, { group: 'down', square: 'down', label: 'Offline', rank: 1, mem: null, temp: null,
      doing: h('span.mut', since ? ['Gone for ', h('time', { title: seen ? 'Agent last seen ' + clock(seen) : dateTime(since) }, dur(Math.max(0, (nowMs - since) / 1000)))] : 'Not reporting') });
  }
  if (n.health && n.health.state === 'hung') return Object.assign(base, { group: 'down', square: 'down', label: 'Not answering', rank: 1, doing: h('span.mut', 'The worker stopped answering') });
  if (q || (n.quarantined && n.quarantined !== '0001-01-01T00:00:00Z')) {
    const until = tms(q && q.until) || tms(a.set_aside && a.set_aside.until);
    const reason = (q && q.reason) || (a.set_aside && a.set_aside.reason) || '';
    const msg = (q && q.message) || (a.set_aside && a.set_aside.message) || '';
    return Object.assign(base, { group: 'down', square: 'down', label: 'Set aside', rank: 0, title: msg || null,
      doing: h('span.mut', { title: msg || null }, (ASIDE[reason] || 'Set aside after a failed check') + (until ? ', back at ' : ''), until ? timeAt(until) : null) });
  }
  const paused = !!a.paused;
  if (inst && inst.state === 'starting') {
    const frac = inst.load && inst.load.progress != null ? inst.load.progress : inst.load_progress;
    return Object.assign(base, { group: 'use', square: 'load', label: 'Loading', rank: 2,
      doing: [frac != null ? `${num(frac * 100)}% loaded` : 'Loading', sep, mono(inst.profile)] });
  }
  if (inst || n.holder) {
    const prof = (inst && inst.profile) || n.holder;
    const role = m && ROLE[m.role] ? ROLE[m.role] + (m.share != null ? ` ${num(m.share * 100)}%` : '') : null;
    return Object.assign(base, { group: 'use', square: 'use4', label: 'In use', rank: 2,
      title: m && m.share != null ? 'Its share of the model: the split the load was given' : null,
      doing: [role ? [role, sep] : null, mono(prof), paused ? h('span.mut', ' · not accepting models') : null] });
  }
  if (paused) {
    return Object.assign(base, { group: 'off', square: 'off', label: 'Off', rank: 4, title: a.paused_at ? 'Paused ' + dateTime(tms(a.paused_at)) : null,
      doing: h('span.mut', 'Paused' + (byWhom(a.paused_actor) || ': takes no new model')) });
  }
  const np = notPlaceable(n, agentOf(st, n));
  if (np && !np.paused && np.code !== 'excluded') {
    const down = np.code === 'silent' || np.code === 'agent_unreachable';
    return Object.assign(base, { group: down ? 'down' : 'free', square: down ? 'down' : 'free', label: np.short, rank: down ? 1 : 3, title: np.text, doing: h('span.mut', 'Takes no new model') });
  }
  const freeAt = tms(n.free_at) || (n.settling ? tms(n.settle_until) : null);
  const next = n.claimed_by || a.next || '';
  if (freeAt && freeAt > nowMs) {
    return Object.assign(base, { group: 'free', square: 'free', label: `Free in ${num(Math.ceil((freeAt - nowMs) / 1000))} s`, rank: 3,
      title: 'Settling after an unload until ' + clock(freeAt),
      doing: next ? [h('span.mut', 'Next: '), mono(next)] : h('span.mut', 'Settling after an unload') });
  }
  if (next) return Object.assign(base, { group: 'free', square: 'free', label: 'Free', rank: 3, doing: [h('span.mut', 'Next: '), mono(next)] });
  return Object.assign(base, { group: 'free', square: 'free', label: 'Free', rank: 3, doing: h('span.mut', 'Nothing loaded') });
}

// memOf(n) -> {used, total} in MiB, or null when the machine reports nothing.
function memOf(n) {
  const total = n.total_mib || 0;
  if (!total) return null;
  const free = Math.min(Math.max(0, n.free_mib || 0), total);
  return { used: total - free, total };
}

const PILLS = [['use', 'in use'], ['free', 'free'], ['down', 'down'], ['off', 'off']];
const SORTS = [{ id: 'attention', label: 'needs attention first' }, { id: 'name', label: 'name' }, { id: 'memory', label: 'memory used' }, { id: 'temp', label: 'hottest first' }];
const MEMS = [{ id: '', label: 'any' }, { id: 'low', label: 'under 50% used' }, { id: 'mid', label: '50 to 80% used' }, { id: 'high', label: 'over 80% used' }];
const TEMPS = [{ id: '', label: 'any' }, { id: 'cool', label: 'under 60 °C' }, { id: 'warm', label: '60 to 75 °C' }, { id: 'hot', label: 'over 75 °C' }];
const memBand = m => (!m ? '' : m.used / m.total < 0.5 ? 'low' : m.used / m.total <= 0.8 ? 'mid' : 'high');
const tempBand = t => (t == null ? '' : t < 60 ? 'cool' : t <= 75 ? 'warm' : 'hot');
const ROWS_SHOWN = 6; // the list is 204 px: six rows of 34, then it scrolls

// stripOf(series, z, day) -> the 24 cells of one zone and their words.
const STATE_WORD = { free: 'Free', in_use: 'In use', loading: 'Loading', down: 'Down', off: 'Switched off' };
function stripOf(series, z, current) {
  const ns = series && (series.nodes || []).find(x => x.id === z.id || x.name === z.name);
  const cells = (ns && ns.cells) || [];
  const out = cells.slice(-24).map(c => {
    const t = tms(c.start);
    const word = STATE_WORD[c.state];
    return { cls: availClass(c), title: (t ? bucketTitle(t, 'hour') + ' · ' : '') + (word ? word + (c.state === 'in_use' && c.busy_level ? `, busy ${c.busy_level} of 4` : '') : 'Nothing known') };
  });
  while (out.length < 24) out.unshift(null);
  const count = {};
  for (const c of cells.slice(-24)) count[c.state] = (count[c.state] || 0) + 1;
  const parts = Object.keys(STATE_WORD).filter(k => count[k]).map(k => `${STATE_WORD[k].toLowerCase()} ${count[k]} h`);
  const span = current ? 'last 24 hours' : 'that day';
  return { cells: out, label: `${z.name}, ${span}: ${parts.length ? parts.join(', ') : 'nothing known'}` };
}

// zonesOf(st, row) -> the view node ids of a request: the machines that
// served it; for a queued one the machines of its model's load, else those
// claimed for it.
export function zonesOf(st, r) {
  const nodes = viewNodes(st);
  const idOf = x => { const n = nodes.find(v => v.id === x.id || (x.name && v.hostname === x.name)); return n ? n.id : null; };
  let ids = (r.nodes || []).map(idOf).filter(Boolean);
  if (!ids.length && r.status === 'queued' && r.profile) {
    const inst = (st.instances || []).find(i => i.profile === r.profile && i.state === 'starting');
    if (inst) ids = (inst.nodes || []).map(x => x.id);
    if (!ids.length) ids = nodes.filter(n => n.claimed_by === r.profile).map(n => n.id);
    if (!ids.length) { const e = (st.queue || []).find(q => q.profile === r.profile); if (e) ids = (e.claim || []).slice(); }
  }
  return ids;
}

// ---------- the view ----------

registerView({
  id: 'overview', title: 'Overview',
  mount(el, route) {
    let r = route;
    let pin; // undefined until the first draw with a running request, then a request id or null
    let hov = null, sum = null, sumErr = '', avail = null, availErr = '', scrollTo = false, dead = false;
    let lastKey = '';
    const layout = () => readLayout();
    const per = () => {
      const oldest = (sum && (sum.oldest_ts || sum.history_begins_at)) || feed.data.historyBeginsAt || 0;
      return periodOf(r.query.period || layout().period, r.query.date, Date.now(), oldest);
    };
    const setQuery = q => navigate('overview', [], Object.assign({}, r.query, q), true);

    // the request rows of the panel: live first, then the newest finished
    const feedParams = p => (p.current && p.period !== 'all' && p.from <= Date.now() ? { from: p.from, limit: 8 } : p.current ? { limit: 8 } : { from: p.from, to: p.to, limit: 8 });
    const feed = requestFeed(feedParams(periodOf(r.query.period || readLayout().period, r.query.date, Date.now(), 0)), () => view.refresh());

    async function readSummary(p) {
      try {
        const s = await api.get('/api/requests/summary', { from: p.from, to: p.to, step: p.step });
        if (dead) return;
        sum = s; sumErr = '';
      } catch (e) { if (!dead) sumErr = (e && e.message) || 'the request failed'; }
      view.refresh();
    }
    async function readAvail(p) {
      const q = p.current ? {} : { from: p.stripDay, to: Math.min(Date.now(), p.stripDay + 24 * 3600000), step: 'hour' };
      try {
        const s = await api.get('/api/nodes/availability', q);
        if (dead) return;
        avail = s; availErr = '';
      } catch (e) { if (!dead) availErr = (e && e.message) || 'the request failed'; }
      view.refresh();
    }
    // a new period: read again at once
    const sync = () => {
      const p = per();
      const key = p.period + '|' + p.from + '|' + p.to;
      if (key === lastKey) return;
      const first = !lastKey;
      lastKey = key;
      if (!first) { sum = null; avail = null; }
      feed.set(feedParams(p));
      readSummary(p);
      readAvail(p);
    };
    // the totals follow the current period every 10 s, the strips every minute, while the page is visible
    const slow = every(10000, () => { if (document.hidden) return; const p = per(); if (p.current) readSummary(p); });
    const slower = every(60000, () => { if (document.hidden) return; readAvail(per()); });
    const ticker = every(100, () => tickRuns(el));
    const offDrop = onDropChange(() => view.refresh());

    const pick = id => { pin = pin === id ? null : id; hov = null; scrollTo = !!pin; view.refresh(); };
    const over = id => { hov = id; if (pin && pin !== id) pin = null; scrollTo = true; view.refresh(); };
    const out = () => { hov = null; view.refresh(); };

    function header(p) {
      return h('div.ov-head', { key: 'head' },
        segmented(PERIODS, p.period, id => setQuery({ period: id === layout().period ? '' : id, date: r.query.date }), 'Period'),
        p.period === 'all' ? null : dateStepper({
          label: p.label, value: p.date, max: ymd(Date.now()),
          prevLabel: 'Previous ' + p.period, nextLabel: 'Next ' + p.period,
          prevDisabled: !p.prev || (oldestOf() && p.from <= oldestOf()),
          nextDisabled: p.current || !p.next,
          onPrev: () => setQuery({ date: p.prev }), onNext: () => setQuery({ date: p.next >= ymd(Date.now()) ? '' : p.next }),
          onPick: d => setQuery({ date: d >= ymd(Date.now()) ? '' : d }),
        }),
        h('div.acts', button('Customize', () => customize(() => view.refresh())),
          button('View 24-hour report', () => navigate('report'), 'pri')));
    }
    const oldestOf = () => (sum && (sum.oldest_ts || sum.history_begins_at)) || 0;

    // the quiet lines under the header: history begins, the store is off, the feed is stale
    function notes(p) {
      const out_ = [];
      const begins = (sum && sum.history_begins_at) || feed.data.historyBeginsAt;
      const off = (sum && sum.on === false) || feed.data.on === false;
      if (off) out_.push(h('div.ov-note.warn', { key: 'off', role: 'status' }, 'The request log is off on this scheduler: requests are not recorded, so the totals and past requests are empty. The live ones still show.'));
      else if (begins && p.from < begins) out_.push(h('div.ov-note', { key: 'begins' }, `History begins at ${dateTime(begins)}: nothing was recorded before it.`));
      if (feed.data.error) out_.push(h('div.ov-note.warn', { key: 'stale', role: 'status' }, `The requests could not be read (${feed.data.error}).` + (feed.data.at ? ` Last updated ${clock(feed.data.at)}.` : '')));
      if (sumErr) out_.push(h('div.ov-note.warn', { key: 'sumerr' }, `The totals could not be read (${sumErr}).`));
      if (availErr) out_.push(h('div.ov-note.warn', { key: 'avaerr' }, `The availability could not be read (${availErr}).`));
      return out_;
    }

    function tokensPanel(p, solo) {
      const series = (sum && sum.series) || [];
      const byT = new Map(series.map(b => [b.t, b]));
      const nowMs = Date.now();
      const buckets = p.buckets.map((t, i) => {
        const b = byT.get(t) || null;
        const future = t > nowMs;
        return { in: b ? b.tokens_in : 0, out: b ? b.tokens_out : 0, label: p.labels[i],
          title: future ? null : `${bucketTitle(t, p.step)} · ${num(b ? b.tokens_in : 0)} in · ${num(b ? b.tokens_out : 0)} out` + (b && b.requests ? ` · ${num(b.requests)} requests` : '') };
      });
      const peak = (key, word) => {
        let best = null;
        buckets.forEach((b, i) => { if (b[key] > 0 && (!best || b[key] > best.v)) best = { v: b[key], t: p.buckets[i] }; });
        return best ? `Tokens ${word} ${p.unit}, ${p.label.replace(' · ', ', ')}, peaking at ${num(best.v)} at ${bucketTitle(best.t, p.step).split('–')[0]}` : `Tokens ${word} ${p.unit}: none`;
      };
      const t = sum && sum.totals;
      const off = sum && sum.on === false;
      const empty = !sum ? (sumErr ? 'The totals could not be read.' : 'Reading the totals…') : off ? 'The request log is off.' : !t.requests ? 'No requests in this period.' : null;
      return h('section', { key: 'tokens', class: 'panel ov-tok' + (solo ? ' solo' : ''), 'aria-label': 'Tokens' },
        h('div.wh', h('h2', 'Tokens'), tokensLegend(p.unit)),
        tokensChart({ tin: t ? t.tokens_in : null, tout: t ? t.tokens_out : null, buckets, ariaIn: peak('in', 'in'), ariaOut: peak('out', 'out'), empty }));
    }

    function requestsPanel(p, st, solo) {
      const rows = feed.data.rows || [];
      const t = sum && sum.totals;
      const total = t ? t.requests + (p.current ? (sum.live.queued + sum.live.running) : 0) : null;
      const allLink = h('a', { href: href('requests', [], p.current && p.period === 'day' ? {} : { from: p.from, to: p.to }), 'data-all': '' }, total == null ? 'All' : 'All ' + num(total));
      // the pin starts on the first running request, as drawn
      if (pin === undefined) { const run = rows.find(x => x.status === 'running'); if (run) pin = String(run.id); }
      if (pin && !rows.some(x => String(x.id) === pin)) pin = null;
      const word = { day: 'on this day', week: 'in this week', month: 'in this month', all: 'yet' }[p.period];
      const body = feed.data.loading && !rows.length ? skeleton(5)
        : runsTable({ rows, now: Date.now(), pin: pin || null, onPick: pick, onOver: over, onOut: out,
          zonesTitle: x => { const ids = st ? zonesOf(st, x) : []; return ids.length ? 'Show its zones: ' + ids.map(id => nameOf(st, id)).join(', ') : 'No zone is known for it'; },
          empty: [h('b', 'No requests ' + word), feed.data.on === false ? 'The request log is off on this scheduler.' : 'Requests sent to the scheduler appear here as they arrive.'] });
      return h('section', { key: 'requests', class: 'panel ov-req' + (solo ? ' solo' : ''), 'aria-label': 'Requests' },
        h('div.wh', h('h2', 'Requests'), runLegend(allLink)),
        h('div.runs-box', body));
    }

    function zonesPanel(p, st) {
      if (!st) return h('section.panel.ov-zones', { key: 'zones', 'aria-label': 'Zones' }, h('div.zp-head', h('h2.sr', 'Zones')), skeleton(4));
      const all = viewNodes(st).map((n, i) => zoneOf(st, n, i));
      const count = g => all.filter(z => z.group === g).length;
      const show = PILLS.some(([g]) => g === r.query.show) ? r.query.show : '';
      const sort = SORTS.some(s => s.id === r.query.sort) ? r.query.sort : 'attention';
      const chipsKnown = [...new Set(all.map(z => z.chip).filter(Boolean))].sort();
      const chip = chipsKnown.includes(r.query.chip) ? r.query.chip : '';
      const memF = MEMS.some(x => x.id === r.query.mem) ? r.query.mem : '';
      const tempF = TEMPS.some(x => x.id === r.query.temp) ? r.query.temp : '';
      let rows = all.filter(z => (!show || z.group === show) && (!chip || z.chip === chip) && (!memF || memBand(z.mem) === memF) && (!tempF || tempBand(z.temp) === tempF));
      const byName = (a, b) => String(a.name).localeCompare(String(b.name), undefined, { numeric: true });
      const cmp = {
        attention: (a, b) => a.rank - b.rank || a.idx - b.idx,
        name: byName,
        memory: (a, b) => ((b.mem ? b.mem.used / b.mem.total : -1) - (a.mem ? a.mem.used / a.mem.total : -1)) || byName(a, b),
        temp: (a, b) => ((b.temp ?? -999) - (a.temp ?? -999)) || byName(a, b),
      }[sort];
      rows = rows.slice().sort(cmp);
      // the highlight of the pinned or hovered request
      const act = hov || pin || null;
      const actRow = act ? (feed.data.rows || []).find(x => String(x.id) === act) : null;
      const ids = actRow ? zonesOf(st, actRow) : [];
      const hi = groupClass(ids, rows.map(z => z.id));
      const dim = ids.length > 0;
      const fs = (label, opts, cur, key) => dropButton(label, opts, cur, v => setQuery({ [key]: v === opts[0].id ? '' : v }), 'ov-' + key);
      const head = h('div.zp-head', { key: 'zhead' },
        h('div', h('h2.sr', 'Zones'),
          chips([{ id: '', label: 'ALL ZONES', cls: 'all' }].concat(PILLS.map(([g, w]) => ({ id: g, label: `${num(count(g))} ${w}` }))), show, id => setQuery({ show: id }), 'Show')),
        h('div', fs('Sort', SORTS, sort, 'sort'), fs('Chip', [{ id: '', label: 'any' }].concat(chipsKnown.map(c => ({ id: c, label: c }))), chip, 'chip'),
          fs('Memory', MEMS, memF, 'mem'), fs('Temperature', TEMPS, tempF, 'temp')));
      const dayEnd = !p.current;
      const axis = h('div.zr.axr', { key: 'axis' },
        h('span.lbl', 'Status'), h('span.lbl', 'Zone'), h('span.lbl', 'Doing'), h('span.lbl.r', 'Memory'), h('span.lbl.r', 'Temp'),
        h('span.lbl.strip-h', h('span', dayEnd ? shortDay(p.stripDay) + ' 00:00' : '24 h ago'),
          h('span', 'Availability, hour by hour'), h('span', dayEnd ? '24:00' : 'now')));
      const list = rows.map(z => {
        const s = stripOf(avail, z, p.current);
        const off = z.group === 'off';
        return h('div', { key: 'z-' + z.id, 'data-zone': z.id, class: 'zr' + (hi[z.id] ? ' ' + hi[z.id] : '') + (off ? ' isoff' : '') },
          h('span', zoneStatus(z.square, z.label, z.title)),
          h('span', h('span.zname', z.name), z.chip ? h('span.mut', ' | ' + z.chip) : null),
          h('span', z.doing),
          h('span', { class: 'mono r' + (off || !z.mem ? ' mut' : '') }, z.mem ? `${gbNum(z.mem.used)} / ${gb(z.mem.total)}` : DASH),
          h('span', { class: 'mono r' + (off || z.temp == null ? ' mut' : '') }, z.temp == null ? DASH : `${num(z.temp)} °C`),
          availStrip(s.cells, s.label));
      });
      const shown = Math.min(ROWS_SHOWN, rows.length);
      const filtered = show || chip || memF || tempF;
      return h('section.panel.ov-zones', { key: 'zones', 'aria-label': 'Zones' }, head,
        h('div.zp-body', h('div.zp-grid', axis,
          h('div', { class: 'zsc' + (dim ? ' dim' : ''), key: 'zsc' },
            list.length ? list : h('div.zempty', all.length ? 'No zone matches these filters.' : 'No zones are known yet. Machines join through their agent.')))),
        colourKey(`Showing ${num(shown)} of ${num(rows.length)}` + (rows.length > shown ? ' · scroll for more' : '') + (filtered && rows.length !== all.length ? ` (${num(all.length)} in all)` : '')));
    }

    const view = liveView(el, () => {
      const st = state.status;
      const p = per();
      const l = layout();
      const vis = l.order.filter(k => !l.hidden.includes(k));
      const parts = [];
      for (let i = 0; i < vis.length; i++) {
        const k = vis[i];
        if (k === 'zones') { parts.push(zonesPanel(p, st)); continue; }
        // Tokens and Requests share a row when they follow each other, as drawn
        const nx = vis[i + 1];
        if (nx && nx !== 'zones') {
          parts.push(h('div.ov-row', { key: 'row-' + k }, [k, nx].map(x => (x === 'tokens' ? tokensPanel(p, false) : requestsPanel(p, st, false)))));
          i++;
        } else parts.push(h('div.ov-row', { key: 'row-' + k }, k === 'tokens' ? tokensPanel(p, true) : requestsPanel(p, st, true)));
      }
      return h('div.ov', header(p), notes(p), parts);
    }, ['status', 'tick', 'conn']);

    // after a draw: the first highlighted zone, scrolled into view inside its list (Q6)
    const afterDraw = () => {
      if (!scrollTo) return;
      scrollTo = false;
      const box = el.querySelector('.zsc');
      const first = box && box.querySelector('.zr.hi');
      if (!box || !first) return;
      const top = first.offsetTop - box.offsetTop;
      if (top < box.scrollTop || top + first.offsetHeight > box.scrollTop + box.clientHeight) box.scrollTop = Math.max(0, top);
    };
    const refresh = view.refresh;
    view.refresh = () => { refresh(); queueMicrotask(() => queueMicrotask(afterDraw)); };

    sync();
    return {
      update(nr) { r = nr; sync(); view.refresh(); },
      unmount() { dead = true; feed.stop(); slow(); slower(); ticker(); offDrop(); view.unmount(); },
    };
  },
});

function nameOf(st, id) { const n = viewNodes(st).find(x => x.id === id); return n ? n.hostname || n.id : id; }
