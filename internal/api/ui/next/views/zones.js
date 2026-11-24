// Zones (design: Zones-Fleet, -Selected, -Group, -Detail, -Confirm). "Zone"
// is the UI's word for a node: one registered machine.
//
// The page, as Zones-Fleet draws it:
// - the count pills above the card: ALL N ZONES, in use, free, loading,
//   down, off. They come from the ONE set of counts (GET /api/nodes
//   `counts`, the same count as the health popup's Zones row); the ALL
//   pill's hover is the popup's sentence, "6 of 8 reachable · 2 down".
//   Create new zone at the right.
// - the card: search (name, chip, model or address), Chip, Model and Memory
//   free filters, Grouped, Collapse all, Sort; the column heads; a row per
//   group (GET /api/groups: its count and common chip, its zones by state,
//   memory, average temperature, 7-day uptime, its 24-hour strip) with its
//   zones under it, then "No group"; each zone's status words, memory,
//   temperature, 7-day uptime (GET /api/nodes/uptime) and 24-hour strip
//   (GET /api/nodes/availability); the footer with the colour key and the
//   pager (16 zones a page).
// - the zone's state comes from the server's zone_state (one
//   availability.Classify for pills, groups and health); the words from
//   the live status.
//
// Address: #/zones/<zone name> opens a zone's panel; ?show= (a pill), q
// (search), chip, model, mem, sort, flat=1 (not grouped), page.
//
// Reads: /ws status (zones, instances, quarantines, agents with zone_state
// and group); GET /api/nodes (counts), GET /api/groups, GET
// /api/nodes/uptime?days=7, GET /api/nodes/availability, read again while
// the page is open; GET /api/history?node= for the open zone's events.
// Writes: the node actions of core/nodeactions.js (pause, unpause, force
// unload), each with a comment, signed by the default user.

import { h } from '../core/dom.js';
import { registerView, navigate, href } from '../core/router.js';
import { state, on } from '../core/store.js';
import * as api from '../core/api.js';
import { liveView, zoneStatus, sidePanel, skeleton, badge, empty, button, facts, dialog, toast, availStrip, availClass, availState, availLevel, dropButton, onDropChange, roleShare } from '../core/ui.js';
import { pager } from '../core/pager.js';
import { num, gb, gbNum, plural, durShort, dateTime, agoText, clock, DASH } from '../core/format.js';
import { viewNodes } from '../core/status.js';
import { agentsMode, agentOf, notPlaceable, sendNodeAction } from '../core/nodeactions.js';
import { onStartups } from '../core/startup.js';
import { defaultUser } from '../core/requests.js';
import { tile, settlePending } from './nodes.js';

const PAGE = 16; // zones a page, as drawn (232 zones on 15 pages)
const MON = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
const tms = t => { const v = typeof t === 'number' ? t : t ? Date.parse(t) : NaN; return Number.isNaN(v) ? null : v; };
const icon = (d, size = 15) => h('svg', { class: 'ic', style: `width:${size}px;height:${size}px`, viewBox: '0 0 20 20', 'aria-hidden': 'true' }, h('path', { d }));
const mono = t => h('span.mono', t);
const byWhom = actor => (!actor ? '' : actor === defaultUser() ? ' by you' : ' by ' + actor);

// The pills: the server's zone_state to the pill it counts in.
const PILL_OF = { in_use: 'use', free: 'free', loading: 'load', lost: 'down', set_aside: 'down', off: 'off' };
const PILLS = [['use', 'in use', 'in_use'], ['free', 'free', 'free'], ['load', 'loading', 'loading'], ['down', 'down', 'down'], ['off', 'off', 'off']];
const ASIDE = { preflight_failed: 'load failed', agent_unreachable: 'agent not reachable', worker_hung: 'worker stopped answering', node_lost: 'lost for a while' };

// member(st, id) -> {inst, role, share} of the instance on zone id, or null.
function member(st, id) {
  for (const inst of st.instances || []) {
    const m = (inst.nodes || []).find(x => x.id === id);
    if (m) return { inst, role: m.role || '', share: typeof m.share === 'number' ? m.share : null };
  }
  return null;
}

// chipOf(agent) -> the chip name the machine reports, else its first device's name.
export function chipOf(a) {
  if (!a) return '';
  if (a.chip) return String(a.chip);
  const d = Array.isArray(a.devices) ? a.devices[0] : null;
  return d && (d.description || d.name) ? String(d.description || d.name) : '';
}

// zoneOf(st, n) -> {pill, square, label, rest, rank, title}: one zone in the
// design's words ("Host 60% · alpha", "Offline · gone for 54 min", "Set
// aside · load failed, back at 16:17"). pill is the server's zone_state
// when it sends one (the one count), else the same reading made here.
export function zoneOf(st, n) {
  const a = agentOf(st, n) || {};
  const zs = a.zone_state || n.zone_state || '';
  const m = member(st, n.id);
  const inst = m ? m.inst : n.holder ? (st.instances || []).find(i => i.profile === n.holder) : null;
  const q = (st.quarantines || []).find(x => x.node === n.id);
  const sep = h('span.mut', ' · ');
  const nowMs = Date.now();
  const out = o => Object.assign({ title: null, rest: null }, o, { pill: PILL_OF[zs] || o.pill });
  if (n.excluded || a.excluded) {
    return out({ pill: 'off', square: 'off', label: 'Switched off' + byWhom(a.excluded_actor), rank: 6,
      title: 'Excluded in the configuration' + (n.excluded_by || a.excluded_by ? ' (exclude_nodes: ' + (n.excluded_by || a.excluded_by) + ')' : '') });
  }
  if (!n.online || zs === 'lost') {
    const since = tms(n.missing_since);
    return out({ pill: 'down', square: 'down', label: 'Offline', rank: 0,
      rest: since ? [sep, h('span.mut', { title: 'Gone since ' + dateTime(since) }, 'gone for ' + goneFor(nowMs - since))] : [sep, h('span.mut', 'not reporting')] });
  }
  if (n.health && n.health.state === 'hung') return out({ pill: 'down', square: 'down', label: 'Not answering', rank: 0, rest: [sep, h('span.mut', 'the worker stopped answering')] });
  if (q || zs === 'set_aside' || (n.quarantined && n.quarantined !== '0001-01-01T00:00:00Z')) {
    const until = tms(q && q.until) || tms(a.set_aside && a.set_aside.until);
    const reason = (q && q.reason) || (a.set_aside && a.set_aside.reason) || '';
    const msg = (q && q.message) || (a.set_aside && a.set_aside.message) || '';
    return out({ pill: 'down', square: 'down', label: 'Set aside', rank: 1, title: msg || null,
      rest: [sep, h('span.mut', (ASIDE[reason] || 'after a failed check') + (until ? ', back at ' + clock(until, false) : ''))] });
  }
  if (inst && inst.state === 'starting') {
    const frac = inst.load && inst.load.progress != null ? inst.load.progress : inst.load_progress;
    return out({ pill: 'load', square: 'load', label: frac != null ? `${num(frac * 100)}% loaded` : 'Loading', rank: 2, rest: [sep, mono(inst.profile)] });
  }
  if (inst || n.holder) {
    const prof = (inst && inst.profile) || n.holder;
    const role = m ? roleShare(m.role, m.share) : '';
    // the status square is the state's colour; how busy it was is the strip's shading
    return out({ pill: 'use', square: 'use4', label: role || 'In use', rank: 3,
      title: m && m.share > 0 ? 'Its share of the model: the split the load was given' : null,
      rest: [sep, mono(prof), a.paused ? h('span.mut', ' · not accepting models') : null] });
  }
  if (a.paused) {
    return out({ pill: 'off', square: 'off', label: 'Switched off' + byWhom(a.paused_actor), rank: 5,
      title: 'Not accepting models' + (a.paused_at ? ' since ' + dateTime(tms(a.paused_at)) : '') });
  }
  const np = notPlaceable(n, a);
  if (np && !np.paused && np.code !== 'excluded') {
    const down = np.code === 'silent' || np.code === 'agent_unreachable';
    return out({ pill: down ? 'down' : 'free', square: down ? 'down' : 'free', label: np.short, rank: down ? 0 : 4, title: np.text, rest: [sep, h('span.mut', 'takes no new model')] });
  }
  const freeAt = tms(n.free_at) || (n.settling ? tms(n.settle_until) : null);
  const next = n.claimed_by || a.next || '';
  const nextPart = next ? [sep, h('span.mut', 'next: '), mono(next)] : null;
  if (freeAt && freeAt > nowMs) {
    return out({ pill: 'free', square: 'free', label: `Free in ${num(Math.ceil((freeAt - nowMs) / 1000))} s`, rank: 4, title: 'Settling after an unload until ' + clock(freeAt), rest: nextPart });
  }
  return out({ pill: 'free', square: 'free', label: 'Free', rank: 4, rest: nextPart });
}

// memOf(n) -> {used, total, free} in MiB, or null when the machine reports nothing.
function memOf(n) {
  const total = n.total_mib || 0;
  if (!total || !n.online) return null;
  const free = Math.min(Math.max(0, n.free_mib || 0), total);
  return { used: total - free, total, free };
}
const memText = m => (m ? `${gbNum(m.used)} / ${gb(m.total)}` : DASH);
const tempOf = a => (a && a.stats && typeof a.stats.headline_temp_c === 'number' ? a.stats.headline_temp_c : null);
const pctText = u => (u == null ? DASH : (Math.round(u * 1000) / 10).toFixed(u >= 0.9995 ? 0 : 1) + '%');
// goneFor(ms) -> "54 min", "2 h 5 min", "3 d 4 h" (as drawn: "gone for 54 min")
function goneFor(ms) {
  const m = Math.max(0, Math.floor(ms / 60000));
  if (m < 60) return `${m} min`;
  if (m < 1440) return `${Math.floor(m / 60)} h${m % 60 ? ' ' + (m % 60) + ' min' : ''}`;
  return `${Math.floor(m / 1440)} d${Math.floor(m % 1440 / 60) ? ' ' + Math.floor(m % 1440 / 60) + ' h' : ''}`;
}
const dayText = ms => { const d = new Date(ms); return `${d.getDate()} ${MON[d.getMonth()]}`; };

// The strip of one zone: its last 24 hours (GET /api/nodes/availability).
const STATE_WORD = { free: 'Free', in_use: 'In use', loading: 'Loading', down: 'Down', off: 'Switched off' };
const SHADE_RULE = "how full the model's slots were during that use, not how much of the hour was used";
const hourText = t => { const d = new Date(t); const p = x => String(x).padStart(2, '0'); return `${p(d.getHours())}:00`; };
function cellWords(c) {
  const st = availState(c);
  const word = STATE_WORD[st];
  if (!word) return 'Nothing known';
  return st === 'in_use' ? `In use · busy ${availLevel(c)} of 4 (${SHADE_RULE})` : word;
}
function stripOf(series, z) {
  const ns = series && (series.nodes || []).find(x => x.id === z.id || x.name === z.name);
  const cells = ((ns && ns.cells) || []).slice(-24);
  const out = cells.map(c => ({ cls: availClass(c), title: (tms(c.start) ? hourText(tms(c.start)) + ' · ' : '') + cellWords(c) }));
  while (out.length < 24) out.unshift(null);
  return { cells: out, raw: cells, label: `${z.name}, last 24 hours` };
}
// groupStrip(strips, name) -> the group's 24 hours drawn from its zones' hours:
// each hour in the state most of its zones were in (in use shaded by their
// mean busy level); the hover counts the zones by state.
function groupStrip(strips, name) {
  const cells = [];
  for (let i = 0; i < 24; i++) {
    const by = {}; let lv = 0, nUse = 0, start = null;
    for (const s of strips) {
      const c = s.raw[s.raw.length - 24 + i];
      if (!c) continue;
      start = start || tms(c.start);
      const st = availState(c);
      if (!STATE_WORD[st]) continue;
      by[st] = (by[st] || 0) + 1;
      if (st === 'in_use') { lv += availLevel(c); nUse++; }
    }
    const keys = Object.keys(by);
    if (!keys.length) { cells.push(null); continue; }
    const top = keys.sort((a, b) => by[b] - by[a])[0];
    const cls = top === 'in_use' ? 'use' + Math.min(4, Math.max(1, Math.round(lv / nUse))) : availClass({ state: top });
    cells.push({ cls, title: (start ? hourText(start) + ' · ' : '') + Object.keys(STATE_WORD).filter(k => by[k]).map(k => `${by[k]} ${STATE_WORD[k].toLowerCase()}`).join(', ') });
  }
  return { cells, label: `${name}, last 24 hours, all zones` };
}

// The node actions on a selection: a pause on each zone, or the opposite.
const BULK = {
  pause: { label: 'Stop accepting models', node: 'pause', busy: 'Stopping', applies: a => !!a && !a.paused,
    hits: ['Each takes no new model until someone lets it accept models again.', 'What they hold now keeps serving requests.'] },
  unpause: { label: 'Accept models again', node: 'unpause', busy: 'Starting', applies: a => !!a && !!a.paused,
    hits: ['Each may take a new model again, as soon as nothing else holds it back.'] },
};

// askBulk(kind, ids, done) asks for the comment, then sends the action to each
// zone it changes, one after the other, and says which ones refused.
function askBulk(kind, ids, done) {
  const b = BULK[kind];
  const st = state.status;
  const targets = viewNodes(st).filter(n => ids.includes(n.id) && b.applies(agentOf(st, n)));
  if (!targets.length) return null;
  const names = targets.map(n => n.hostname || n.id);
  return dialog({
    title: `${b.label} on ${plural(targets.length, 'zone')}?`,
    body: close => {
      const comment = h('textarea.textarea', { rows: 2, maxlength: 400, placeholder: 'Why? It is kept in the audit log.', 'aria-label': 'Comment' });
      const err = h('span.err', { role: 'alert' });
      const go = button(b.label, null, 'lg pri');
      const cancel = button('Cancel', () => close(false), 'lg');
      go.addEventListener('click', async () => {
        const c = comment.value.trim();
        if (!c) { err.textContent = 'A comment is required. Say why, in a few words.'; comment.focus(); return; }
        err.textContent = '';
        go.disabled = cancel.disabled = comment.disabled = true;
        go.textContent = b.busy + '…';
        const failed = [];
        for (const n of targets) {
          try { await sendNodeAction(api, n.id, b.node, c); } catch (e) { failed.push(`${n.hostname || n.id}: ${(e && e.message) || 'the request failed'}`); }
        }
        const ok = targets.length - failed.length;
        if (ok) toast(`${b.label}: ${plural(ok, 'zone')} done`, 'ok');
        if (failed.length) { err.textContent = 'Refused: ' + failed.join('; '); go.textContent = b.label; cancel.disabled = false; cancel.textContent = 'Close'; }
        else close(true);
        done(targets.filter(n => !failed.some(f => f.startsWith((n.hostname || n.id) + ':'))).map(n => n.id));
      });
      return h('div.col', { style: 'gap:14px' },
        h('div', { style: 'display:flex;flex-direction:column;gap:8px' }, b.hits.map(t => h('div.hit', h('i', { class: kind === 'pause' ? 'a' : 'm' }), h('span', t)))),
        h('div.hit', h('i'), h('span', 'Zones: ', h('span.mono', names.join(', ')))),
        h('div.field', h('label', 'Comment (required)'), comment, err),
        h('div.dlg-foot', cancel, go));
    },
  });
}

// ---------- the zone panel (until the Zones-Fleet-Detail rebuild) ----------

const ZONE_EVENTS = ['loadStarted', 'loadFinished', 'loadStalled', 'loadCancelledLowMemory', 'preflightFailed', 'instanceUnloaded', 'childExited', 'eviction',
  'nodeLost', 'nodeQuarantined', 'quarantineEnded', 'workerHealth', 'recoveryRequest', 'recoveryResponse', 'recoveryOutcome', 'recoverySequenceStopped', 'benchStepFinished',
  'workerLaunchFailed', 'workerExited', 'nodeMembership', 'nodeAdmin'];
const EVENTS_SHOWN = 8;

function zoneFacts(n, agent) {
  const a = agent || {};
  const chip = chipOf(agent);
  const up = typeof a.host_uptime_s === 'number' ? durShort(a.host_uptime_s) : null;
  const seen = typeof a.last_seen_ago_s === 'number' ? (a.last_seen_ago_s < 2 ? 'just now' : durShort(a.last_seen_ago_s) + ' ago') : null;
  return facts([
    chip ? ['Device', h('span', chip)] : null,
    a.platform ? ['Platform', h('span.mono', a.platform)] : null,
    a.release ? ['Agent release', h('span.mono', a.release)] : null,
    a.install ? ['Install', h('span', a.install)] : null,
    up ? ['Up for', h('span', up), 'Since the machine last started, as its agent reports it'] : null,
    seen ? ['Last report', h('span', seen)] : null,
    ['Memory', h('span', memText(memOf(n)))],
    ['Temperature', h('span', tempOf(agent) == null ? DASH : `${num(tempOf(agent))} °C`)],
    ['Priority', h('span', n.priority ? String(n.priority) : 'no tier matches'), 'Lower numbers are used first when a model is placed'],
  ]);
}

function eventsPart(hist) {
  let body;
  if (!hist || hist.loading) body = h('span.mut', 'Reading the history…');
  else if (hist.error) body = h('span.err-t', 'The history could not be read: ' + hist.error);
  else if (hist.off) body = h('span.mut', 'The event history is off on this scheduler.');
  else if (!hist.events.length) body = h('span.mut', 'No load, unload or trouble recorded for this zone yet.');
  else body = h('div.zevents', hist.events.slice(0, EVENTS_SHOWN).map(e => h('div.zev', { key: e.seq + '-' + e.ts, class: 'zev sev-' + (e.severity || 'info') },
    h('time', { title: dateTime(e.ts), datetime: new Date(e.ts).toISOString() }, agoText(e.ts)),
    h('span', e.summary || e.type))));
  return h('section.zpart', { key: 'events' }, h('h3', 'Recent events'), body,
    hist && hist.events && hist.events.length ? h('a', { href: href('activity'), style: 'font-size:12px' }, 'All events on Activity') : null);
}

function zonePanel(st, n, hist, refresh) {
  const agent = agentOf(st, n);
  const z = zoneOf(st, n);
  return [
    h('section.zpart', { key: 'state' },
      h('div.row', { style: 'gap:10px;flex-wrap:wrap' }, zoneStatus(z.square, z.label, z.title),
        n.auto === false && z.pill !== 'off' ? badge('Named only', 'line', 'In no_auto_nodes: only a profile that names this zone in force_nodes puts a model here') : null,
        n.local ? badge('Runs Potluck', 'n') : null),
      z.rest ? h('div', z.rest) : null),
    h('section.zpart', { key: 'facts' }, zoneFacts(n, agent)),
    eventsPart(hist),
    h('div', { key: 'card' }, tile(st, n, refresh)),
  ];
}

// ---------- Create new zone (waits for the agent track) ----------

function createInfo() {
  return dialog({
    title: 'Create new zone',
    body: h('div.col', { style: 'gap:10px' },
      h('p', { style: 'margin:0' }, 'Making a zone from here is not built yet. It needs an install script Potluck serves with its own address filled in, an enrolment token the new machine carries when it first checks in, and a Windows and a USB variant of the script.'),
      h('p.mut', { style: 'margin:0' }, 'Until then a machine joins by itself: install the agent on it and start it with Potluck\'s address; it appears in this list when it checks in, under "No group", and can then be moved into a group.')),
    actions: [{ label: 'Close', kind: 'pri', value: true, autofocus: true }],
  });
}

// ---------- filters ----------

const SORTS = [{ id: 'attention', label: 'needs attention first' }, { id: 'name', label: 'name' }, { id: 'memory', label: 'memory used' }, { id: 'temp', label: 'hottest first' }];
const MEMS = [{ id: '', label: 'any' }, { id: 'low', label: 'under 25%' }, { id: 'mid', label: '25 to 50%' }, { id: 'high', label: 'over 50%' }];
const memBand = m => (!m ? '' : m.free / m.total < 0.25 ? 'low' : m.free / m.total <= 0.5 ? 'mid' : 'high');
const NONE_MODEL = '-';

registerView({
  id: 'zones', title: 'Zones',
  mount(el, route) {
    let r = route, side = null, hist = null, alive = true;
    let nodesDoc = null, groupsDoc = null, uptimeDoc = null, avail = null;
    const sel = new Set(); // the ticked zones (ids)
    const shut = new Set(); // collapsed groups ('' is No group)
    const offStatus = on('status', () => { settlePending(); readSoon(); });
    const offStartups = onStartups(() => { view.refresh(); if (side) side.redraw(); });
    const offDrop = onDropChange(() => view.refresh());
    const setQuery = (q, keepPage) => navigate('zones', [], Object.assign({}, r.query, keepPage ? {} : { page: '' }, q), true);
    const refresh = () => { view.refresh(); if (side) side.redraw(); };

    // the server's figures follow the live status: counts and groups at
    // most every 5 s, the strips at most every minute, uptime at most every
    // 5 min (no timer of their own: a status message asks for them)
    const get = async (path, q, put) => { try { const v = await api.get(path, q); if (alive) { put(v); view.refresh(); } } catch { /* the figure stays as it was */ } };
    const readCounts = () => Promise.all([get('/api/nodes', null, v => { nodesDoc = v; }), get('/api/groups', null, v => { groupsDoc = v; })]);
    const readUptime = () => get('/api/nodes/uptime', { days: 7 }, v => { uptimeDoc = v; });
    const readAvail = () => get('/api/nodes/availability', null, v => { avail = v; });
    let soon = null, lastRead = 0, lastAvail = 0, lastUptime = 0;
    function readSoon() {
      if (!alive) return;
      const t = Date.now();
      if (t - lastAvail >= 60000) { lastAvail = t; readAvail(); }
      if (t - lastUptime >= 300000) { lastUptime = t; readUptime(); }
      if (soon) return;
      const wait = Math.max(0, 5000 - (t - lastRead));
      soon = setTimeout(() => { soon = null; lastRead = Date.now(); readCounts(); }, wait);
    }
    lastRead = lastAvail = lastUptime = Date.now();
    readCounts(); readUptime(); readAvail();

    const loadEvents = async id => {
      hist = { id, loading: true, events: [] };
      const n = state.status && viewNodes(state.status).find(x => x.id === id);
      try {
        const res = await api.get('/api/history', { node: (n && n.hostname) || id, type: ZONE_EVENTS.join(','), limit: EVENTS_SHOWN });
        if (hist && hist.id === id) hist = { id, events: res.events || [], off: res.history === 'off' };
      } catch (e) {
        if (hist && hist.id === id) hist = { id, events: [], error: (e && e.message) || 'the request failed' };
      }
      if (alive && side && side.id === id) side.redraw();
    };

    // The side panel of the open zone (#/zones/<name>).
    const syncSide = () => {
      const id = r.query.zone || '';
      if (!id) { if (side) { const s = side; side = null; s.close(); } return; }
      if (side && side.id === id) { side.redraw(); return; }
      if (side) { const s = side; side = null; s.close(); }
      const card = () => {
        const st = state.status;
        const n = st && viewNodes(st).find(x => x.id === id);
        if (!n) return h('section.card', empty('This zone is not in the status', 'It may have left the pool. Close this panel to see the zones there are.'));
        return zonePanel(st, n, hist && hist.id === id ? hist : null, refresh);
      };
      loadEvents(id);
      const nameOf = () => { const n = state.status && viewNodes(state.status).find(x => x.id === id); return n ? h('span.mono', n.hostname || n.id) : 'Zone'; };
      const p = sidePanel({ label: 'Selected zone', title: nameOf, body: card, onClose: () => { if (side === p) { side = null; setQuery({ zone: '' }, true); } } });
      p.id = id;
      side = p;
    };

    const view = liveView(el, () => {
      const st = state.status;
      if (side) side.redraw();
      const q = r.query;
      const all = !st ? [] : viewNodes(st).filter(n => n.state !== 'removed' && (agentOf(st, n) || {}).zone_state !== 'removed').map(n => {
        const a = agentOf(st, n) || {};
        const z = zoneOf(st, n);
        return { n, a, z, id: n.id, name: n.hostname || a.name || n.id, chip: chipOf(a), mem: memOf(n), temp: tempOf(a), group: a.group || n.group || '',
          model: (member(st, n.id) || {}).inst ? member(st, n.id).inst.profile : n.holder || '', addr: String(a.addr || n.addr || '').replace(/:\d+$/, '') };
      });
      // the one set of counts; until it is read, the same reading of the live status
      const c = (nodesDoc && nodesDoc.counts) || (() => {
        const k = { total: all.length, in_use: 0, free: 0, loading: 0, down: 0, off: 0 };
        for (const x of all) k[{ use: 'in_use', free: 'free', load: 'loading', down: 'down', off: 'off' }[x.z.pill]]++;
        k.reachable = k.total - k.down;
        return k;
      })();
      const show = PILLS.some(([id]) => id === q.show) ? q.show : '';
      const pills = h('div.zpills', { role: 'group', 'aria-label': 'Show' },
        h('button', { class: 'fc all' + (!show ? ' on' : ''), type: 'button', 'aria-pressed': !show ? 'true' : 'false', 'data-show': '',
          title: `${num(c.reachable)} of ${num(c.total)} reachable` + (c.down ? ` · ${num(c.down)} down` : ''), onclick: () => setQuery({ show: '' }) },
        `ALL ${num(c.total)} ZONE${c.total === 1 ? '' : 'S'}`),
        PILLS.map(([id, word, key]) => h('button', { class: 'fc' + (show === id ? ' on' : ''), type: 'button', 'aria-pressed': show === id ? 'true' : 'false', 'data-show': id,
          onclick: () => setQuery({ show: show === id ? '' : id }) }, `${num(c[key] || 0)} ${word}`)),
        button([icon('M10 4v12M4 10h12'), 'Create new zone'], createInfo, 'pri', { 'data-act': 'create', style: 'margin-left:auto' }));
      if (!st) return [pills, h('section.w.zfleet', { 'aria-label': 'Zones' }, skeleton(6))];

      // filters
      const needle = String(q.q || '').trim().toLowerCase();
      const chips = [...new Set(all.map(x => x.chip).filter(Boolean))].sort();
      const models = [...new Set(all.map(x => x.model).filter(Boolean))].sort();
      const chipF = chips.includes(q.chip) ? q.chip : '';
      const modelF = q.model === NONE_MODEL || models.includes(q.model) ? q.model : '';
      const memF = MEMS.some(m => m.id && m.id === q.mem) ? q.mem : '';
      const sort = SORTS.some(s => s.id === q.sort) ? q.sort : 'attention';
      let rows = all.filter(x => (!show || x.z.pill === show) && (!chipF || x.chip === chipF) &&
        (!modelF || (modelF === NONE_MODEL ? !x.model : x.model === modelF)) && (!memF || memBand(x.mem) === memF) &&
        (!needle || [x.name, x.chip, x.model, x.addr, x.id].some(v => String(v || '').toLowerCase().includes(needle))));
      const byName = (a, b) => a.name.localeCompare(b.name, undefined, { numeric: true });
      const cmp = { name: byName, attention: (a, b) => (a.z.rank - b.z.rank) || byName(a, b),
        memory: (a, b) => ((b.mem ? b.mem.used / b.mem.total : -1) - (a.mem ? a.mem.used / a.mem.total : -1)) || byName(a, b),
        temp: (a, b) => ((b.temp ?? -1) - (a.temp ?? -1)) || byName(a, b) }[sort];
      rows.sort(cmp);

      // groups: as the server lists them, then No group
      // groups by name; under "needs attention first", those with a zone down first
      const downIn = g => (g.counts ? g.counts.down || 0 : (g.needs_attention || []).length);
      const gs = ((groupsDoc && groupsDoc.groups) || []).slice().sort((a, b) => (sort === 'attention' ? (downIn(b) > 0) - (downIn(a) > 0) : 0) ||
        String(a.name).localeCompare(String(b.name), undefined, { numeric: true }));
      const hasGroups = gs.length > 0 || all.some(x => x.group);
      const grouped = hasGroups && q.flat !== '1';
      const groupNames = [...new Set(gs.map(g => g.name).concat(all.map(x => x.group).filter(Boolean)))];
      const docOf = name => (name ? gs.find(g => g.name === name) : groupsDoc && groupsDoc.ungrouped) || null;

      // the uptime column: 7 days, or since the history began
      const win = uptimeDoc && uptimeDoc.window;
      const since = win && win.complete === false && tms(win.covered_from) ? dayText(tms(win.covered_from)) : '';
      const upOf = x => { const u = uptimeDoc && (uptimeDoc.zones || []).find(v => v.id === x.id || v.name === x.name); return u ? u.uptime : null; };
      const upTitle = since ? `Share of the time since ${since} the zone was reachable (the history began then; 7 days from ${dayText(tms(win.covered_from) + 7 * 86400000)})` : 'Share of the last 7 days the zone was reachable';

      // the page's items: [group row, its zones] or zones
      const items = []; // {kind: 'group'|'zone', ...}
      if (grouped) {
        for (const name of groupNames.concat([''])) {
          const zs = rows.filter(x => x.group === name);
          const doc = docOf(name);
          if (!zs.length && (show || needle || chipF || modelF || memF || !doc || !(doc.count || (doc.members || []).length))) continue;
          if (shut.has(name)) { items.push({ kind: 'group', name, zs, doc, shut: true, unit: 1 }); continue; }
          if (!zs.length) { items.push({ kind: 'group', name, zs, doc, unit: 1 }); continue; }
          zs.forEach((x, i) => items.push({ kind: 'zone', x, group: name, first: i === 0, zs, doc, unit: 1 }));
        }
      } else rows.forEach(x => items.push({ kind: 'zone', x, unit: 1 }));
      const pages = Math.max(1, Math.ceil(items.length / PAGE));
      const page = Math.min(pages, Math.max(1, Number(q.page) || 1));
      const onPage = items.slice((page - 1) * PAGE, page * PAGE);

      // selection
      // only a zone whose machine has an agent can be ticked: the actions are agent actions
      const canTick = x => !!(x.a && x.a.id);
      const shownZones = onPage.filter(i => i.kind === 'zone' && canTick(i.x)).map(i => i.x);
      for (const id of Array.from(sel)) if (!all.some(x => x.id === id && canTick(x))) sel.delete(id);
      const allTicked = shownZones.length > 0 && shownZones.every(x => sel.has(x.id));
      const toggle = id => { if (sel.has(id)) sel.delete(id); else sel.add(id); view.refresh(); };
      const tickMany = (xs, on_) => { xs.forEach(x => (on_ ? sel.add(x.id) : sel.delete(x.id))); view.refresh(); };

      const strips = new Map(all.map(x => [x.id, stripOf(avail, x)]));
      const pick = id => setQuery({ zone: id }, true);
      const zoneRow = (x, indent) => {
        const { z } = x;
        const off = z.pill === 'off';
        const u = upOf(x);
        const s = strips.get(x.id);
        return h('div', { key: 'z-' + x.id, class: 'fr v2' + (q.zone === x.id ? ' sel' : '') + (sel.has(x.id) ? ' chk' : ''), 'data-zone': x.id, role: 'row', tabindex: '0',
          'aria-label': x.name + ', ' + z.label, onclick: () => pick(x.id), onkeydown: e => { if (e.key === 'Enter') { e.preventDefault(); pick(x.id); } } },
        canTick(x) ? h('input', { class: 'cb', type: 'checkbox', checked: sel.has(x.id), 'aria-label': 'Select ' + x.name, 'data-sel': x.id,
          onclick: e => { e.stopPropagation(); toggle(x.id); }, onkeydown: e => e.stopPropagation() }) : h('span'),
        h('span.zn', { style: indent ? 'padding-left:26px' : null }, h('span.mono.znm', x.name)),
        h('span.sts', { title: z.title }, h('i', { class: 'sq ' + z.square, 'aria-hidden': 'true' }), h('span.stx', z.label, z.rest)),
        h('span', { class: 'r mono' + (!x.mem || off ? ' mut' : '') }, memText(x.mem)),
        h('span', { class: 'r mono' + (x.temp == null || off ? ' mut' : '') }, x.temp == null ? DASH : `${num(x.temp)} °C`),
        h('span', { class: 'r mono' + (u == null ? ' mut' : ''), title: u == null ? (off ? 'Switched off: its time is not counted' : 'Not known yet') : null }, pctText(u)),
        availStrip(s.cells, s.label));
      };
      const groupRow = (name, zs, doc, closed) => {
        const label = name || 'No group';
        const members = all.filter(x => x.group === name);
        const cs = (doc && doc.counts) || null;
        const chipsHere = [...new Set(members.map(x => x.chip).filter(Boolean))];
        const count = doc && doc.count != null ? doc.count : members.length;
        const memD = doc && doc.memory;
        const tempD = doc && doc.temp && typeof doc.temp.avg_c === 'number' ? doc.temp.avg_c : null;
        const up7 = doc && doc.uptime_7d ? doc.uptime_7d.uptime : null;
        const gcs = [['in_use', 'use4', 'in use'], ['free', 'free', 'free'], ['loading', 'load', 'loading'], ['down', 'down', 'down'], ['off', 'off', 'off']]
          .map(([k, sq, w]) => { const v = cs ? cs[k] : members.filter(x => PILL_OF[x.a.zone_state] === { in_use: 'use', free: 'free', loading: 'load', down: 'down', off: 'off' }[k]).length; return v ? h('span', { title: `${num(v)} ${w}` }, h('i', { class: 'sq ' + sq, 'aria-hidden': 'true' }), num(v)) : null; });
        const gstrip = groupStrip(members.map(x => strips.get(x.id)), label);
        const visible = zs.filter(x => canTick(x) && onPage.some(i => i.kind === 'zone' && i.x === x));
        const ticked = visible.length > 0 && visible.every(x => sel.has(x.id));
        return h('div', { key: 'g-' + label, class: 'fr v2 gh', 'data-group': name || '-', role: 'row' },
          h('input', { class: 'cb', type: 'checkbox', checked: ticked, 'aria-label': 'Select all in ' + label, 'data-sel': 'group:' + (name || '-'), disabled: !visible.length,
            onclick: e => { e.stopPropagation(); tickMany(visible, !ticked); } }),
          h('span.gn', h('button', { class: 'more', type: 'button', 'aria-expanded': closed ? 'false' : 'true', 'aria-label': (closed ? 'Expand ' : 'Collapse ') + label, 'data-fold': name || '-',
            onclick: e => { e.stopPropagation(); if (shut.has(name)) shut.delete(name); else shut.add(name); view.refresh(); } }, icon(closed ? 'M8 5l5 5-5 5' : 'M5 8l5 5 5-5', 14)),
          h(name ? 'span.mono.gname' : 'span.gname', label),
          h('span.mut.gmeta', num(count) + (chipsHere.length === 1 ? ' · ' + chipsHere[0] : count === 1 ? ' zone' : ' zones'))),
          h('span.gc', gcs),
          h('span.r', h('span.mono', memD && memD.total_mib ? `${num(Math.round(memD.used_mib / 1024))} / ${num(Math.round(memD.total_mib / 1024))} GB` : DASH)),
          h('span.r.mono', { title: 'Average across the group' }, tempD == null ? DASH : `${num(tempD)} °C`),
          h('span.r.mono', { title: up7 == null ? 'Not known yet' : 'Over the time of all its zones' + (since ? ' since ' + since : ', last 7 days') }, pctText(up7)),
          availStrip(gstrip.cells, gstrip.label));
      };

      const list = [];
      let lastGroup = null;
      for (const it of onPage) {
        if (it.kind === 'group') { list.push(groupRow(it.name, it.zs, it.doc, it.shut)); lastGroup = it.name; continue; }
        if (grouped && it.group !== lastGroup) { list.push(groupRow(it.group, it.zs, it.doc, false)); lastGroup = it.group; }
        list.push(zoneRow(it.x, grouped));
      }

      const fs = (label, opts, cur, key) => dropButton(label, opts, cur, v => setQuery({ [key]: v === opts[0].id ? '' : v }), 'zn-' + key);
      const findBox = h('label.zx', { 'data-keep': '', key: 'find' },
        icon('M13.5 13.5L17 17M14.5 9a5.5 5.5 0 1 1-11 0 5.5 5.5 0 0 1 11 0z', 14),
        (() => { const i = h('input', { type: 'text', 'data-search': 'zones', value: q.q || '', placeholder: 'Search name, chip, model or address', 'aria-label': 'Search zones' });
          i.addEventListener('input', () => setQuery({ q: i.value })); return i; })());
      const anyOpen = groupNames.concat(['']).some(n => !shut.has(n));
      const tools = h('div.ztools',
        findBox,
        fs('Chip', [{ id: '', label: 'any' }].concat(chips.map(c_ => ({ id: c_, label: c_ }))), chipF, 'chip'),
        fs('Model', [{ id: '', label: 'any' }].concat(models.map(m => ({ id: m, label: m })), [{ id: NONE_MODEL, label: 'nothing loaded' }]), modelF, 'model'),
        fs('Memory free', MEMS, memF, 'mem'),
        h('div.ztools-r',
          h('button', { class: 'fs2' + (grouped ? ' on' : ''), type: 'button', 'data-act': 'grouped', 'aria-pressed': grouped ? 'true' : 'false', disabled: !hasGroups,
            title: hasGroups ? (grouped ? 'Show every zone in one list' : 'Show the zones under their groups') : 'No groups yet: select zones and make one from the selection',
            onclick: () => setQuery({ flat: grouped ? '1' : '' }) }, 'Grouped'),
          grouped ? h('button', { class: 'fs2', type: 'button', 'data-act': 'collapse', onclick: () => { if (anyOpen) groupNames.concat(['']).forEach(n => shut.add(n)); else shut.clear(); view.refresh(); } },
            anyOpen ? 'Collapse all' : 'Expand all') : null,
          fs('Sort', SORTS, sort, 'sort')));

      const head = h('div.fr.v2.fhd', { key: 'head', role: 'row' },
        h('input', { class: 'cb', type: 'checkbox', checked: allTicked, 'aria-label': 'Select all', 'data-sel': 'all', disabled: !shownZones.length, onclick: () => tickMany(shownZones, !allTicked) }),
        h('span.lbl', 'Zone'), h('span.lbl', 'Model status'), h('span.lbl.r', 'Memory'), h('span.lbl.r', 'Temp'),
        h('span.lbl.r', { title: upTitle }, since ? h('span.uph', 'Uptime', h('small', 'since ' + since)) : 'Uptime'),
        h('span.lbl.strip-h', h('span', '24 h ago'), h('span', 'Availability, hour by hour'), h('span', { style: 'color:var(--ink)' }, 'now')));

      const chosen = all.filter(x => sel.has(x.id));
      const bulkCount = kind => chosen.filter(x => BULK[kind].applies(x.a && x.a.id ? x.a : null)).length;
      const bulkButton = kind => {
        const k = bulkCount(kind);
        return button(BULK[kind].label + (k && k < chosen.length ? ` (${num(k)})` : ''), () => askBulk(kind, Array.from(sel), done => { done.forEach(id => sel.delete(id)); view.refresh(); }), 'sm',
          { disabled: !k, 'data-bulk': kind, title: k ? `Applies to ${plural(k, 'zone')} of the selection` : kind === 'pause' ? 'Every selected zone is already not accepting models' : 'Every selected zone already accepts models' });
      };
      const keyPart = h('div.key2',
        h('span', h('i.sq.free'), 'Free'),
        h('span', h('i.sq.use1'), h('i.sq.use2'), h('i.sq.use3'), h('i.sq.use4'), 'In use, darker is busier'),
        h('span', h('i.sq.load'), 'Loading'), h('span', h('i.sq.down'), 'Down'), h('span', h('i.sq.off'), 'Switched off'));
      const selPart = h('div.zsel', { role: 'region', 'aria-label': 'Selection' }, h('b', `${num(chosen.length)} selected`), bulkButton('pause'), bulkButton('unpause'),
        button('Clear', () => { sel.clear(); view.refresh(); }, 'sm ghost', { 'data-act': 'clear' }));
      const foot = h('div.bulk', { key: 'foot' }, chosen.length ? selPart : keyPart,
        pager({ page, pages, onPage: n => setQuery({ page: n === 1 ? '' : String(n) }, true), label: 'Zone pages' }));

      const body = all.length
        ? (list.length ? list : h('div.zempty', 'No zone matches.'))
        : h('div.zempty', agentsMode(st)
          ? empty('No machines have joined yet', 'Machines join through their agent. None has registered so far; start the agent on a machine and it appears here.')
          : empty('No zones are known yet', 'Zones come from the fleet feed. None has been reported so far; About shows the connection.'));
      return [pills, h('section.w.zfleet', { 'aria-label': 'Zones' }, tools, head, h('div.zlist', body), foot)];
    }, ['status', 'tick', 'conn']);
    syncSide();
    return {
      update(nr) { r = nr; syncSide(); view.refresh(); },
      unmount() { alive = false; clearTimeout(soon); offStatus(); offStartups(); offDrop(); if (side) { const s = side; side = null; s.close(); } view.unmount(); },
    };
  },
});
