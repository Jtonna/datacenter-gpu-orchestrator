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
// unload), the group routes of core/groups.js and the model actions of
// core/modelactions.js (plan then load on chosen zones, wait for the
// requests then unload, take that back, unload or force unload), each with
// a comment, signed by the default user. There is no setting that hides the
// model actions (the owner's decision of 24 Nov).

import { h } from '../core/dom.js';
import { registerView, navigate, href } from '../core/router.js';
import { state, on, listen } from '../core/store.js';
import * as api from '../core/api.js';
import { liveView, zoneStatus, sidePanel, skeleton, badge, empty, button, facts, dialog, toast, availStrip, availClass, availState, availLevel, dropButton, onDropChange, roleShare, runModel } from '../core/ui.js';
import { pager } from '../core/pager.js';
import { num, gb, gbNum, plural, durShort, dateTime, agoText, clock, DASH } from '../core/format.js';
import { viewNodes } from '../core/status.js';
import { agentsMode, agentOf, notPlaceable, sendNodeAction } from '../core/nodeactions.js';
import { onStartups } from '../core/startup.js';
import { defaultUser } from '../core/requests.js';
import { createGroup, addMembers, removeMembers, renameGroup, pauseGroup, GROUP_NAME_RULE } from '../core/groups.js';
import { unloadModel, planLoad, loadModel, drainModel, undrainModel } from '../core/modelactions.js';
import { settlePending } from './nodes.js';

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
    return out({ pill: 'load', square: 'load', label: frac != null ? `${num(frac * 100)}% loaded` : 'Loading', rank: 2, rest: [sep, h('span.mono', { title: inst.profile }, modelName(inst.profile))] });
  }
  if (inst || n.holder) {
    const prof = (inst && inst.profile) || n.holder;
    const role = m ? roleShare(m.role, m.share) : '';
    // the status square is the state's colour; how busy it was is the strip's shading
    return out({ pill: 'use', square: 'use4', label: role || 'In use', rank: 3,
      title: m && m.share > 0 ? 'Its share of the model: the split the load was given' : null,
      rest: [sep, h('span.mono', { title: prof }, modelName(prof)), isDraining(inst) ? h('span.mut', { 'data-draining': '' }, ' · unloads when its requests end') : null,
        a.paused ? h('span.mut', ' · not accepting models') : null] });
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

// groupChip(doc, members) -> the group's chip: the server's `chip` (absent or
// "mixed" give none); until /api/groups is read, the one every member
// reports, when all report the same one.
function groupChip(doc, members) {
  if (doc && 'chip' in doc) return typeof doc.chip === 'string' && doc.chip && doc.chip !== 'mixed' ? doc.chip : '';
  if (doc || !members.length) return '';
  const r = [...new Set(members.map(x => x.reported || ''))];
  return r.length === 1 && r[0] ? r[0] : '';
}
// isDraining(inst): a drain was asked for (the server's state "draining" or drain true)
export const isDraining = inst => !!inst && (inst.drain === true || inst.state === 'draining');
// drainText(inst) -> "about 40 s left" or "no estimate yet"
const drainText = inst => (typeof inst.drain_eta_s === 'number' && inst.drain_eta_s >= 0 ? `about ${durShort(Math.max(1, Math.round(inst.drain_eta_s)))} left` : 'no estimate yet');
// pinnedOf(inst) -> the zones a load on chosen zones named, or []
const pinnedOf = inst => (inst && Array.isArray(inst.pinned_nodes) ? inst.pinned_nodes : []);
// modelName(profile) -> the design's short name ("gemma-4-27b:32k"), else the profile id
const modelName = profile => runModel({ profile }) || profile;

// planView(plan, err, sig): the plan for a load on chosen zones, in words:
// its verdict (the server's sentence for a refusal) and its memory line.
const VERDICT = {
  start: ['Starts now', 'ready', 'The chosen zones can take it: the load starts at once.'],
  serve: ['Already loaded', 'ready', 'It is loaded already.'],
  wait: ['Loading now', 'load', 'It is loading already.'],
  evict: ['Makes room first', 'q', 'A model on the chosen zones is unloaded first.'],
  queue: ['Waits', 'n', 'The chosen zones cannot take it now: the load waits in the queue.'],
  backoff: ['Held back', 'aside', 'Its last load failed: it is held back for now.'],
  reject: ['Refused', 'fail', ''],
};
function planView(plan, err, sig) {
  if (!sig) return h('span.mut', 'Choose at least one zone: the plan is read for the zones you choose.');
  if (err) return h('div.box.warn', { 'data-verdict': 'error' }, h('div.pt', h('span', h('b', 'The plan was refused: '), err)));
  if (!plan) return h('span.mut', { 'data-verdict': '' }, 'Asking the scheduler for its plan…');
  const [label, kind, text] = VERDICT[plan.action] || [String(plan.action || 'Not known'), 'n', ''];
  const need = plan.fit_need_mib || plan.need_mib || 0;
  const place = plan.placement || [];
  const usable = place.reduce((s, n) => s + (n.budget_mib || 0), 0);
  const victims = (plan.victims || []).map(v => v.profile).filter(Boolean);
  const mem = [];
  if (need) mem.push(h('span', { title: num(need) + ' MiB' }, 'Needs ', h('b', gb(need))));
  if (place.length) mem.push(h('span', { title: place.map(n => `${n.id}: ${num(n.budget_mib || 0)} MiB usable`).join('; ') }, 'usable on the chosen zones ', h('b', gb(usable))));
  if (plan.required_mib) mem.push(h('span', { title: num(plan.required_mib) + ' MiB' }, 'it would need ', h('b', gb(plan.required_mib)), ' with the margins'));
  const reason = plan.reason ? String(plan.reason) : '';
  return h('div.col', { style: 'gap:8px', 'data-verdict': plan.action || '' },
    h('div', { style: 'display:flex;gap:8px;align-items:center;flex-wrap:wrap' }, badge(label, kind),
      plan.tight_fit ? badge('Tight fit', 'n', 'It fits only without its safety margin; it starts anyway.') : null,
      h('span', plan.action === 'reject' ? reason || 'The scheduler refuses this load.' : text)),
    victims.length ? h('div.hit', h('i.a'), h('span', 'It unloads ', h('span.mono', victims.join(', ')), ' first.')) : null,
    plan.action !== 'reject' && reason ? h('div.hit', h('i'), h('span', { title: reason }, plan.action === 'queue' ? 'Why it waits: ' : '', reason)) : null,
    mem.length ? h('div.mut', { 'data-memory': '', style: 'font-weight:600' }, mem.map((x, i) => [i ? ' · ' : '', x])) : null,
    plan.warning ? h('div.box.warn', h('div.pt', h('span', String(plan.warning)))) : null);
}

// The node actions on a selection: a pause on each zone, or the opposite.
// Reboot and Open SSH: drawn as designed, not built
const REBOOT_WHY = 'Not available: Potluck cannot restart a machine yet (there is no server route for it), and a PS5 is never rebooted from here: a Linux reboot drops a console to the PlayStation system.';
const SSH_WHY = 'Not available yet: SSH from these pages is left out for now, so that nothing here depends on one particular setup (a user name, a key or a jump host).';

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
  const seen = typeof a.last_seen_ago_s === 'number' ? (a.last_seen_ago_s < 2 ? 'just now' : durShort(a.last_seen_ago_s) + ' ago') : null;
  return facts([
    chip ? ['Device', h('span', chip)] : null,
    a.platform ? ['Platform', h('span.mono', a.platform)] : null,
    a.release ? ['Agent release', h('span.mono', a.release)] : null,
    a.install ? ['Install', h('span', a.install)] : null,
    seen ? ['Last report', h('span', seen)] : null,
    ['Priority', h('span', n.priority ? String(n.priority) : 'no tier matches'), 'Lower numbers are used first when a model is placed'],
  ]);
}

function eventsPart(hist, none = 'No load, unload or trouble recorded for this zone yet.') {
  let body;
  if (!hist || hist.loading) body = h('span.mut', 'Reading the history…');
  else if (hist.error) body = h('span.err-t', 'The history could not be read: ' + hist.error);
  else if (hist.off) body = h('span.mut', 'The event history is off on this scheduler.');
  else if (!hist.events.length) body = h('span.mut', none);
  else body = h('div.zevents', hist.events.slice(0, EVENTS_SHOWN).map(e => h('div.zev', { key: e.seq + '-' + e.ts, class: 'zev sev-' + (e.severity || 'info') },
    h('time', { title: dateTime(e.ts), datetime: new Date(e.ts).toISOString() }, agoText(e.ts)),
    h('span', e.summary || e.type))));
  return h('section.zpart', { key: 'events' }, h('h3', 'Recent events'), body,
    hist && hist.events && hist.events.length ? h('a', { href: href('activity'), style: 'font-size:12px' }, 'All events on Activity') : null);
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
    let r = route, side = null, hist = null, alive = true, filtersOpen = false;
    let nodesDoc = null, groupsDoc = null, uptimeDoc = null, avail = null;
    const sel = new Set(); // the ticked zones (ids)
    const shut = new Set(); // collapsed groups ('' is No group)
    const offStatus = on('status', () => { settlePending(); readSoon(); });
    const offStartups = onStartups(() => { view.refresh(); if (side) side.redraw(); });
    const offDrop = onDropChange(() => view.refresh());
    const restNow = () => (r.rest && r.rest[0] === 'group' && r.rest[1] ? ['group', String(r.rest[1])] : []);
    const setQuery = (q, keepPage) => navigate('zones', restNow(), Object.assign({}, r.query, keepPage ? {} : { page: '' }, q), true);
    const pickZone = id => navigate('zones', [], Object.assign({}, r.query, { zone: id }), true);
    const refresh = () => { view.refresh(); if (side) side.redraw(); };

    // the server's figures follow the live status: counts and groups at
    // most every 5 s, the strips at most every minute, uptime at most every
    // 5 min (no timer of their own: a status message asks for them)
    const get = async (path, q, put) => { try { const v = await api.get(path, q); if (alive) { put(v); view.refresh(); } } catch { /* the figure stays as it was */ } };
    const readCounts = () => Promise.all([get('/api/nodes', { sort: 'attention' }, v => { nodesDoc = v; }), get('/api/groups', null, v => { groupsDoc = v; })]);
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

    // a group's recent events: GET /api/history?group=<name> (404 for a group the server does not know)
    let ghist = null;
    const loadGroupEvents = async name => {
      ghist = { name, loading: true, events: [] };
      try {
        const res = await api.get('/api/history', { group: name, type: ZONE_EVENTS.join(','), limit: EVENTS_SHOWN });
        if (ghist && ghist.name === name) ghist = { name, events: res.events || [], off: res.history === 'off' };
      } catch (e) {
        if (ghist && ghist.name === name) ghist = { name, events: [], error: (e && e.message) || 'the request failed' };
      }
      if (alive && side && side.key === 'g:' + name) side.redraw();
    };

    // The side panel: a group's (#/zones/group/<name>) or a zone's (#/zones/<name>).
    const syncSide = () => {
      const g = groupOpen();
      const id = g ? '' : r.query.zone || '';
      const key = g ? 'g:' + g : id ? 'z:' + id : '';
      if (!key) { if (side) { const s = side; side = null; s.close(); } return; }
      if (side && side.key === key) { side.redraw(); return; }
      if (side) { const s = side; side = null; s.close(); }
      if (g) { loadGroupEvents(g); const p = groupPanel(g); p.key = key; side = p; return; }
      loadEvents(id);
      loadWeek(id);
      const p = zonePanelOf(id);
      p.id = id;
      p.key = key;
      side = p;
    };

    // ---------- actions on the selection (Zones-Fleet-Selected) ----------
    let cur = { all: [], groups: [] }; // the last drawing's zones and groups
    let menu = null; // the open right-click menu: {el, sub, off, back}
    const chosenNow = () => cur.all.filter(x => sel.has(x.id));
    const afterWrite = () => { sel.clear(); readCounts(); view.refresh(); };
    const holding = x => { const m = member(state.status || {}, x.id); return m ? m.inst.profile : x.n.holder || ''; };
    const names = xs => h('span.mono', xs.map(x => x.name).join(', '));

    // ask({title, lines, name, go, red, run(comment, name)}): the confirm
    // dialog of a write, with its required comment (kept in the audit log);
    // a refusal is shown in place and the dialog stays open.
    function ask(o) {
      return dialog({ title: o.title, body: close => {
        const nameIn = o.name ? h('input.input', { type: 'text', 'data-field': 'name', maxlength: 64, placeholder: 'rack-b', 'aria-label': 'Group name' }) : null;
        const comment = h('textarea.textarea', { rows: 2, maxlength: 400, 'data-field': 'comment', placeholder: 'Why? It is kept in the audit log.', 'aria-label': 'Comment' });
        const err = h('span.err', { role: 'alert' });
        const go = button(o.go, null, 'lg ' + (o.red ? 'dng' : 'pri'), { 'data-go': '' });
        const cancel = button('Cancel', () => close(false), 'lg');
        go.addEventListener('click', async () => {
          const c = comment.value.trim();
          if (nameIn && !nameIn.value.trim()) { err.textContent = 'Give the group a name.'; nameIn.focus(); return; }
          const bad = o.check ? o.check() : '';
          if (bad) { err.textContent = bad; return; }
          if (!c) { err.textContent = 'A comment is required. Say why, in a few words.'; comment.focus(); return; }
          err.textContent = '';
          go.disabled = cancel.disabled = true;
          try { await o.run(c, nameIn ? nameIn.value.trim() : ''); close(true); afterWrite(); } catch (e) {
            err.textContent = (e && e.message) || 'The request failed.';
            go.disabled = cancel.disabled = false;
            readCounts();
          }
        });
        return h('div.col', { style: 'gap:14px' },
          h('div', { style: 'display:flex;flex-direction:column;gap:8px' }, o.lines.map(t => h('div.hit', h('i', { class: o.red ? 'a' : 'm' }), h('span', t)))),
          nameIn ? h('div.field', h('label', 'Group name'), nameIn, h('span.mut', { style: 'font-size:12px' }, GROUP_NAME_RULE)) : null,
          o.extra || null,
          h('div.field', h('label', 'Comment (required)'), comment, err),
          h('div.dlg-foot', cancel, go));
      } });
    }
    // each(items, send): every write in turn; the refused ones are named, the rest stay done
    async function each(items, send) {
      const failed = [];
      for (const [label, args] of items) { try { await send(...args); } catch (e) { failed.push(label + ': ' + ((e && e.message) || 'the request failed')); } }
      if (failed.length) throw new Error('Refused: ' + failed.join('; '));
    }
    function newGroup(xs) {
      return ask({ title: `New group from ${plural(xs.length, 'zone')}`, name: true, go: 'Create group',
        lines: [['Zones: ', names(xs)], 'A zone is in one group at most: one that is in another group moves into the new one.'],
        run: (c, name) => createGroup(api, name, xs.map(x => x.id), c).then(() => toast(`Group ${name} created`, 'ok')) });
    }
    function moveTo(xs, g) {
      return ask({ title: `Move ${plural(xs.length, 'zone')} to ${g}?`, go: 'Move',
        lines: [['Zones: ', names(xs)], 'A zone is in one group at most: one that is in another group leaves it. Nothing they hold is touched.'],
        run: c => addMembers(api, g, xs.map(x => x.id), c).then(() => toast(`${plural(xs.length, 'zone')} moved to ${g}`, 'ok')) });
    }
    function removeFrom(xs) {
      const by = {};
      xs.filter(x => x.group).forEach(x => (by[x.group] = by[x.group] || []).push(x));
      const n = Object.values(by).reduce((s, ys) => s + ys.length, 0);
      const gs_ = Object.keys(by);
      return ask({ title: `Remove ${plural(n, 'zone')} from ${gs_.length === 1 ? gs_[0] : 'their groups'}?`, go: 'Remove from group',
        lines: gs_.map(g => [h('span.mono', g), ': ', names(by[g])]).concat(['They stay in the pool, under "No group". Nothing they hold is touched.']),
        run: c => each(gs_.map(g => [g, [api, g, by[g].map(y => y.id), c]]), removeMembers).then(() => toast(`${plural(n, 'zone')} taken out of their groups`, 'ok')) });
    }
    function forceUnload(xs) {
      const hold = xs.filter(holding);
      // one zone: the Zones-Confirm dialog for the model it holds
      if (hold.length === 1) return askUnload(holding(hold[0]), { zone: hold[0].id });
      const models = {};
      hold.forEach(x => (models[holding(x)] = models[holding(x)] || []).push(x));
      return ask({ title: `Force unload ${plural(hold.length, 'zone')}?`, go: 'Force unload', red: true,
        lines: Object.keys(models).map(p => [h('span.mono', p), ' ends, on ', names(models[p])])
          .concat(['Requests on them are cut off: the apps that sent them get an error. A model spread over other zones ends there as well.']),
        run: c => each(hold.map(x => [x.name, [api, x.id, 'force', c]]), sendNodeAction).then(() => toast(`Force unload sent to ${plural(hold.length, 'zone')}`, 'ok')) });
    }
    const pauseSome = xs => askBulk('pause', xs.map(x => x.id), done => { done.forEach(id => sel.delete(id)); view.refresh(); });

    function closeMenu() {
      if (!menu) return;
      const m = menu;
      menu = null;
      m.el.remove();
      if (m.sub) m.sub.remove();
      m.off();
      if (m.back && m.back.isConnected !== false && m.back.focus) m.back.focus();
    }
    const item = (label, attrs, onclick) => h('button', Object.assign({ type: 'button', role: 'menuitem', onclick: e => { e.stopPropagation(); if (onclick) onclick(e); } }, attrs), label);
    // the groups the selection can move to: every group that does not hold all of it
    function moveList(xs, x, y) {
      const gs_ = cur.groups.map(g => g.name).filter(g => !xs.every(z => z.group === g));
      return h('div.cm.sub', { role: 'menu', 'aria-label': 'Move to group', style: `left:${x}px;top:${y}px;width:190px` },
        gs_.map(g => item(h('span.mono', g), { 'data-move': g }, () => { closeMenu(); moveTo(xs, g); })),
        gs_.length ? h('hr') : null,
        item('New group…', { 'data-cm': 'newgroup-sub' }, () => { closeMenu(); newGroup(xs); }));
    }
    // openMenu(x, y, back, onlyMove): the menu for the selection at x, y
    // (the right click), or only its group list (the bar's Move to group)
    function openMenu(x, y, back, onlyMove) {
      closeMenu();
      const xs = chosenNow();
      if (!xs.length) return;
      const W = window.innerWidth || 1536, H_ = window.innerHeight || 730;
      const left = Math.max(8, Math.min(x, W - 232)), top = Math.max(8, Math.min(y, H_ - 300));
      let el;
      if (onlyMove) el = moveList(xs, left, top);
      else {
        const pausable = xs.filter(z => BULK.pause.applies(z.a && z.a.id ? z.a : null));
        const grouped_ = xs.some(z => z.group);
        const holds = xs.some(holding);
        const moveBtn = item([h('span', 'Move to group'), icon('M8 5l5 5-5 5', 15)], { 'data-cm': 'move', 'aria-haspopup': 'true', 'aria-expanded': 'false' }, () => {
          if (menu.sub) { menu.sub.remove(); menu.sub = null; moveBtn.setAttribute('aria-expanded', 'false'); moveBtn.classList.remove('on'); return; }
          menu.sub = moveList(xs, left + 224 > W - 200 ? left - 194 : left + 224, top + 26);
          document.body.appendChild(menu.sub);
          moveBtn.setAttribute('aria-expanded', 'true');
          moveBtn.classList.add('on');
          const f = menu.sub.querySelector('button');
          if (f) f.focus();
        });
        el = h('div.cm', { role: 'menu', 'aria-label': `Actions for ${plural(xs.length, 'selected zone')}`, style: `left:${left}px;top:${top}px` },
          h('span.lbl', xs.length === 1 ? '1 zone selected' : `${num(xs.length)} zones selected`),
          moveBtn,
          item('New group from selection', { 'data-cm': 'newgroup' }, () => { closeMenu(); newGroup(xs); }),
          item('Remove from group', { 'data-cm': 'remove', disabled: !grouped_, title: grouped_ ? null : 'None of them is in a group' }, () => { closeMenu(); removeFrom(xs); }),
          h('hr'),
          item('Open SSH', { 'data-cm': 'ssh', disabled: true, title: SSH_WHY }),
          item('Reboot', { 'data-cm': 'reboot', disabled: true, title: REBOOT_WHY }),
          item('Stop accepting models', { 'data-cm': 'pause', disabled: !pausable.length, title: pausable.length ? null : 'Every one of them already takes no new model' }, () => { closeMenu(); pauseSome(xs); }),
          h('hr'),
          item('Force unload', { class: 'red', 'data-cm': 'force', disabled: !holds, title: holds ? null : 'None of them holds a model' }, () => { closeMenu(); forceUnload(xs); }));
      }
      document.body.appendChild(el);
      const onDoc = e => {
        if (e.type === 'keydown') {
          if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); closeMenu(); return; }
          if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
            const box = menu.sub && menu.sub.contains(document.activeElement) ? menu.sub : menu.el;
            const bs = Array.from(box.querySelectorAll('button')).filter(b => !b.disabled);
            const i = bs.indexOf(document.activeElement);
            const next = bs[(i + (e.key === 'ArrowDown' ? 1 : bs.length - 1)) % bs.length];
            if (next) { e.preventDefault(); next.focus(); }
          }
          return;
        }
        if (menu && (menu.el.contains(e.target) || (menu.sub && menu.sub.contains(e.target)))) return;
        closeMenu();
      };
      document.addEventListener('keydown', onDoc, true);
      document.addEventListener('mousedown', onDoc, true);
      window.addEventListener('resize', closeMenu);
      window.addEventListener('scroll', closeMenu, true);
      menu = { el, sub: null, back, off: () => {
        document.removeEventListener('keydown', onDoc, true); document.removeEventListener('mousedown', onDoc, true);
        window.removeEventListener('resize', closeMenu); window.removeEventListener('scroll', closeMenu, true);
      } };
      const first = Array.from(el.querySelectorAll('button')).find(b => !b.disabled);
      if (first) first.focus();
    }
    // a right click (or the menu key) on a zone: on a ticked zone the menu acts
    // on the whole selection; on another it selects that zone alone
    function rowMenu(x, e, rowEl) {
      if (!(x.a && x.a.id)) return;
      e.preventDefault();
      if (!sel.has(x.id)) { sel.clear(); sel.add(x.id); view.refresh(); }
      const r = rowEl && rowEl.getBoundingClientRect ? rowEl.getBoundingClientRect() : null;
      openMenu(e.clientX != null ? e.clientX : r ? r.left + 260 : 300, e.clientY != null ? e.clientY : r ? r.bottom : 300, rowEl);
    }

    // ---------- the group panel (Zones-Fleet-Group, #/zones/group/<name>) ----------
    const groupOpen = () => (r.rest && r.rest[0] === 'group' && r.rest[1] ? String(r.rest[1]) : '');
    const groupDoc = name => ((groupsDoc && groupsDoc.groups) || []).find(g => g.name === name) || null;
    const openGroup = name => navigate('zones', ['group', name], Object.assign({}, r.query, { zone: '' }));
    const zoneOfId = id => cur.all.find(x => x.id === id) || null;

    // the unload dialog (Zones-Confirm): what it ends; wait for the requests
    // (a drain: new requests wait in the queue meanwhile), unload now, or
    // force; a comment. A drain's answer (its estimate and the server's
    // sentence about the next load) is shown in the dialog before it closes.
    function askUnload(model, o = {}) {
      const st = state.status || {};
      const inst = (st.instances || []).find(i => i.profile === model && (!o.zone || (i.nodes || []).some(m => m.id === o.zone))) ||
        (st.instances || []).find(i => i.profile === model) || null;
      const runs = inst ? inst.in_flight || 0 : 0;
      const loading = inst && inst.state === 'starting';
      const draining = isDraining(inst);
      const nameOfId = id => { const x = zoneOfId(id); return x ? x.name : id; };
      const ms = inst ? (inst.nodes || []) : [];
      const host = ms.find(m => m.role === 'host');
      const helpers = ms.filter(m => m !== host);
      const list = ids => h('span.mono', ids.map(nameOfId).join(', '));
      let where = null;
      if (o.zone && host && host.id === o.zone && helpers.length) where = [' ', h('span.mono', nameOfId(o.zone)), ' is the host, so the model also unloads from its ', helpers.length === 1 ? 'helper ' : 'helpers ', list(helpers.map(m => m.id)), '.'];
      else if (o.zone && ms.length > 1) where = [' The model unloads from all its zones: ', list(ms.map(m => m.id)), '.'];
      else if (!o.zone && ms.length) {
        const out = o.group ? ms.filter(m => { const x = zoneOfId(m.id); return !x || x.group !== o.group; }) : [];
        where = [' It runs on ', list(ms.map(m => m.id)), host ? [' (', h('span.mono', nameOfId(host.id)), ' is the host)'] : null, '; it unloads from all of them',
          out.length ? [', ', list(out.map(m => m.id)), ' outside ', h('span.mono', o.group), ' included'] : null, '.'];
      }
      const runsWord = runs === 1 ? '1 request is in flight.' : `${num(runs)} requests are in flight.`;
      const lead = loading ? 'It is still loading: the load is cancelled.' : draining ? ['It is already waiting for its requests. ', runsWord] : runs ? runsWord : 'No request is in flight.';
      const failWord = runs === 0 ? 'Nothing is running on it.' : runs === 1 ? 'The request fails straight away.' : runs === 2 ? 'Both requests fail straight away.' : `All ${num(runs)} requests fail straight away.`;
      // the option's sentence (the owner's words): new requests wait, the next load may go elsewhere
      const WAIT_SUB = 'New requests for this model wait in the queue meanwhile. The next load may start on another free zone.';
      let when = loading ? 'force' : runs && !draining ? 'wait' : runs ? 'force' : 'now';
      const title = o.zone ? ['Unload ', h('span.mono', model), ' from ', h('span.mono', nameOfId(o.zone)), '?'] : ['Unload ', h('span.mono', model), '?'];
      return dialog({ title, body: close => {
        const comment = h('textarea.textarea', { rows: 2, maxlength: 400, 'data-field': 'comment', placeholder: 'Why? It is kept in the audit log.', 'aria-label': 'Comment' });
        const err = h('span.err', { role: 'alert' });
        const go = button('Unload', null, 'lg dng solid', { 'data-go': '' });
        const cancel = button('Cancel', () => close(false), 'lg');
        const opt = (id, label, sub, extra = {}) => {
          const input = h('input', Object.assign({ type: 'radio', name: 'unload-when', 'data-when': id, checked: when === id, onclick: () => { when = id; box.querySelectorAll('.opt').forEach(l => l.classList.toggle('on', l.contains(input))); } }, extra));
          return h('label', { class: 'opt' + (when === id ? ' on' : '') + (extra.disabled ? ' off' : ''), title: extra.title || null }, input, h('span', label, h('span', sub)));
        };
        const waitLabel = `Wait for the ${runs === 1 ? 'request' : num(runs) + ' requests'}, then unload`;
        const box = h('div', { role: 'radiogroup', 'aria-label': 'When to unload', style: 'display:flex;flex-direction:column;gap:8px' },
          loading ? opt('wait', 'Wait for the load, then unload', 'Not possible while it loads: Potluck waits for requests, not for a load. Force unload cancels the load.', { disabled: true, title: 'The server refuses to wait while the model is loading (409).' })
            : draining ? opt('wait', waitLabel, 'Already asked: it unloads when they end. Cancel that from the zone panel.', { disabled: true, title: 'It is draining already.' })
            : runs ? opt('wait', waitLabel, WAIT_SUB)
            : opt('now', 'Unload now', 'Nothing is running on it. A new request for this model loads it again.'),
          opt('force', 'Force unload now', loading ? 'The load stops straight away.' : failWord));
        const body = h('div.col', { style: 'gap:14px' },
          h('div', { class: 'box' + (runs || loading ? ' warn' : '') },
            h('div.pt', icon(runs || loading ? 'M10 3l8 14H2zM10 8.5v4M10 14.8v.01' : 'M10 3a7 7 0 1 0 0 14 7 7 0 0 0 0-14zM10 9v5M10 6v.01', 15), h('span', h('b', lead), where))),
          box,
          h('div.field', h('label', 'Comment (required)'), comment, err),
          h('div.dlg-foot', cancel, go));
        // after a drain: what the server answered, then Close
        const drained = res => {
          const eta = res && typeof res.eta_s === 'number' && res.eta_s >= 0 ? res.eta_s : null;
          body.replaceChildren(
            h('div.box', { 'data-drained': '' }, h('div.pt', icon('M10 3a7 7 0 1 0 0 14 7 7 0 0 0 0-14zM10 6v4l3 2', 15),
              h('span', h('b', model, ' unloads when its requests end. '),
                eta != null ? h('span', { 'data-eta': String(eta) }, 'About ', durShort(Math.max(1, Math.round(eta))), '.') : h('span', { 'data-eta': '' }, 'No estimate yet: no request of this model has finished since Potluck started.'),
                ' New requests for it wait in the queue meanwhile.'))),
            h('p.mut', { style: 'margin:0', 'data-next-load': '' }, (res && typeof res.next_load === 'string' && res.next_load) || 'The next load may start on another free zone.'),
            h('div.dlg-foot', button('Close', () => close(true), 'lg pri', { 'data-close': '' })));
        };
        go.addEventListener('click', async () => {
          const c = comment.value.trim();
          if (!c) { err.textContent = 'A comment is required. Say why, in a few words.'; comment.focus(); return; }
          err.textContent = '';
          go.disabled = cancel.disabled = true;
          try {
            if (when === 'wait') {
              const res = await drainModel(api, model, c);
              if (res && res.msg === 'not loaded') { toast(`${model} was not loaded`, 'ok'); close(true); }
              else { toast(`${model} unloads when its requests end`, 'ok'); drained(res); }
            } else {
              const res = await unloadModel(api, model, when === 'force', c);
              toast(res && res.msg === 'not loaded' ? `${model} was not loaded` : `${model} unloaded`, 'ok');
              close(true);
            }
            readCounts(); view.refresh();
          } catch (e) { err.textContent = (e && e.message) || 'The request failed.'; go.disabled = cancel.disabled = false; }
        });
        return body;
      } });
    }

    // undrainAsk(model): take a drain back; the model admits requests again
    function undrainAsk(model) {
      return ask({ title: ['Cancel the unload of ', h('span.mono', model), '?'], go: 'Cancel the unload',
        lines: ['It stops waiting to unload and takes requests again, the ones queued meanwhile first.'],
        run: c => undrainModel(api, model, c).then(() => toast(`${model} stays loaded`, 'ok')) });
    }

    // ---------- Load a model on chosen zones (Zones-Fleet-Detail) ----------
    // The plan comes first (POST /api/plan {model, nodes}): its verdict and
    // memory line; only then Load (loadModel, the same body). The zone
    // picker lists every zone; one that takes a model only when named (the
    // laptop, no_auto_nodes) carries its "Named only" badge.
    function askLoad(model, ids) {
      const st = state.status || {};
      const zones = viewNodes(st).filter(n => n.state !== 'removed' && (agentOf(st, n) || {}).zone_state !== 'removed')
        .map(n => ({ n, id: n.id, name: n.hostname || (agentOf(st, n) || {}).name || n.id, z: zoneOf(st, n) }))
        .sort((a, b) => a.name.localeCompare(b.name, undefined, { numeric: true }));
      const picked = new Set(ids.filter(id => zones.some(x => x.id === id)));
      const nodesNow = () => zones.filter(x => picked.has(x.id)).map(x => x.name);
      let plan = null, planErr = '', planFor = '', seq = 0;
      return dialog({ title: ['Load ', h('span.mono', model), ' on chosen zones'], wide: true, body: close => {
        const comment = h('textarea.textarea', { rows: 2, maxlength: 400, 'data-field': 'comment', placeholder: 'Why? It is kept in the audit log.', 'aria-label': 'Comment' });
        const err = h('span.err', { role: 'alert' });
        const go = button('Load', null, 'lg pri', { 'data-go': '', disabled: true });
        const cancel = button('Cancel', () => close(false), 'lg');
        const verdict = h('div', { 'data-plan': '' });
        const draw = () => {
          const sig = nodesNow().join(',');
          go.disabled = !(plan && planFor === sig && plan.action !== 'reject' && sig);
          verdict.replaceChildren(planView(plan && planFor === sig ? plan : null, planErr, sig));
        };
        const check = async () => {
          const nodes = nodesNow(), sig = nodes.join(','), my = ++seq;
          plan = null; planErr = ''; planFor = sig;
          draw();
          if (!nodes.length) return;
          try { const p = await planLoad(api, model, nodes); if (my === seq) plan = p; } catch (e) { if (my === seq) planErr = (e && e.message) || 'The plan could not be read.'; }
          if (my === seq) draw();
        };
        const picker = h('div.addz', { 'data-picker': '' }, zones.map(x => {
          const named = x.n.auto === false;
          return h('label.addrow', { 'data-pick-row': x.name },
            h('input', { type: 'checkbox', class: 'cb', 'data-pick': x.name, checked: picked.has(x.id), onclick: () => { if (picked.has(x.id)) picked.delete(x.id); else picked.add(x.id); check(); } }),
            h('span.mono', x.name),
            h('span.mut', x.z.label, named ? [' ', badge('Named only', 'n', 'It takes a model only when a profile or a load names it (no_auto_nodes); naming it here does.')] : null));
        }));
        go.addEventListener('click', async () => {
          const c = comment.value.trim();
          if (!c) { err.textContent = 'A comment is required. Say why, in a few words.'; comment.focus(); return; }
          const nodes = nodesNow();
          if (!nodes.length) { err.textContent = 'Choose at least one zone.'; return; }
          // the plan for exactly these zones first; a refused plan loads nothing
          if (!plan || planFor !== nodes.join(',')) { err.textContent = 'Wait for the plan of these zones.'; return; }
          if (plan.action === 'reject') { err.textContent = 'The plan refuses this load: nothing is sent.'; return; }
          err.textContent = '';
          go.disabled = cancel.disabled = true;
          try {
            await loadModel(api, model, nodes, c);
            toast(`Loading ${model} on ${nodes.join(', ')}`, 'ok');
            close(true); readCounts(); view.refresh();
          } catch (e) { err.textContent = (e && e.message) || 'The request failed.'; cancel.disabled = false; draw(); }
        });
        check();
        return h('div.col', { style: 'gap:14px' },
          h('div.field', h('label', 'Zones'), picker,
            h('span.mut', { style: 'font-size:12px' }, 'The load goes on these zones only, for this one load; the profile itself is not changed.')),
          verdict,
          h('div.field', h('label', 'Comment (required)'), comment, err),
          h('div.dlg-foot', cancel, go));
      } });
    }

    function renameAsk(name) {
      return ask({ title: ['Rename ', h('span.mono', name)], name: true, go: 'Rename',
        lines: ['Its zones stay in it; only the name changes.'],
        run: (c, to) => renameGroup(api, name, to, c).then(() => {
          const g = groupDoc(name);
          if (g) g.name = to;
          toast(`${name} is now ${to}`, 'ok');
          navigate('zones', ['group', to], r.query, true);
        }) });
    }
    function addAsk(name) {
      const outside = cur.all.filter(x => x.group !== name && x.a && x.a.id).sort((a, b) => a.name.localeCompare(b.name, undefined, { numeric: true }));
      const picked = new Set();
      const listBox = h('div.addz', outside.length ? outside.map(x => h('label.addrow', h('input', { type: 'checkbox', class: 'cb', 'data-add': x.id, onclick: () => { if (picked.has(x.id)) picked.delete(x.id); else picked.add(x.id); } }),
        h('span.mono', x.name), h('span.mut', x.group ? h('span.mono', x.group) : 'No group'))) : h('span.mut', 'Every zone is in this group already.'));
      return ask({ title: ['Add zones to ', h('span.mono', name)], go: 'Add zones', extra: h('div.field', h('label', 'Zones'), listBox),
        lines: ['A zone is in one group at most: one that is in another group leaves it. Nothing they hold is touched.'],
        check: () => (picked.size ? '' : 'Tick at least one zone.'),
        run: c => addMembers(api, name, outside.filter(x => picked.has(x.id)).map(x => x.id), c).then(() => toast(`${plural(picked.size, 'zone')} added to ${name}`, 'ok')) });
    }
    function switchAsk(name, doc) {
      const pause = doc.accepting === 'all';
      const n = doc.count != null ? doc.count : (doc.members || []).length;
      return ask({ title: [pause ? 'Stop accepting models on ' : 'Accept models again on ', h('span.mono', name), '?'], go: pause ? 'Stop accepting models' : 'Accept models again',
        lines: pause ? [`Its ${plural(n, 'zone')} take no new model until the group accepts models again.`, 'What they hold now keeps serving requests.']
          : [`Each of its ${plural(n, 'zone')} may take a new model again, as soon as nothing else holds it back.`],
        run: c => pauseGroup(api, name, pause, c).then(() => toast(pause ? `${name} takes no new model` : `${name} accepts models again`, 'ok')) });
    }

    function groupBody(name) {
      const doc = groupDoc(name);
      if (!doc) return h('div.zempty', groupsDoc ? empty(`No group named ${name}`, 'It may have been renamed or deleted. Close this panel to see the groups there are.') : skeleton(4));
      const members = cur.all.filter(x => x.group === name);
      const count = doc.count != null ? doc.count : members.length;
      const mem = doc.memory || {};
      const used = Math.round((mem.used_mib || 0) / 1024), total = Math.round((mem.total_mib || 0) / 1024);
      const pct = mem.total_mib ? Math.round((mem.used_mib || 0) / mem.total_mib * 100) : 0;
      const t = doc.temp || {};
      const up = doc.uptime_7d ? doc.uptime_7d.uptime : null;
      const win = (groupsDoc && groupsDoc.uptime_window) || (uptimeDoc && uptimeDoc.window);
      const since = win && win.complete === false && tms(win.covered_from) ? dayText(tms(win.covered_from)) : '';
      const fact = (label, value, more, attrs = {}) => h('div.fact', attrs, h('span.lbl', label), value, more || null);
      // lost first, then set aside, each by name (the list's order)
      const attn = (doc.needs_attention || []).slice().sort((a, b) => ((a.state === 'lost' ? 0 : 1) - (b.state === 'lost' ? 0 : 1)) ||
        String(a.name || '').localeCompare(String(b.name || ''), undefined, { numeric: true }));
      const models = (doc.models || []).slice().sort((a, b) => (b.zones || 0) - (a.zones || 0));
      return [
        h('div.gp-acts', { key: 'acts' },
          button('Rename', () => renameAsk(name), '', { 'data-gp': 'rename' }),
          button('Add zones', () => addAsk(name), '', { 'data-gp': 'add' }),
          button([icon('M16 10a6 6 0 1 1-2-4.5M16 4v4h-4', 15), 'Reboot all'], null, 'red', { 'data-gp': 'reboot', disabled: true, title: REBOOT_WHY, style: 'margin-left:auto' })),
        h('div.f2', { key: 'facts' },
          fact('Memory', h('b', num(used), h('span.mut.of', ` of ${num(total)} GB`)), h('div.mb', { role: 'img', 'aria-label': `${pct}% of memory in use` }, h('span', { style: `width:${pct}%` }))),
          fact('Temperature, average', h('b', typeof t.avg_c === 'number' ? `${num(Math.round(t.avg_c))} °C` : DASH)),
          fact('Hottest', h('b', typeof t.hottest_c === 'number' && t.hottest_c ? `${num(Math.round(t.hottest_c))} °C` : DASH), t.hottest_name ? h('span.mono.mut', { style: 'font-size:12px' }, t.hottest_name) : null),
          fact('In use', h('b', num((doc.counts && doc.counts.in_use) || 0), h('span.mut.of', ` of ${plural(count, 'zone')}`))),
          fact('Ping, average', h('b.mut', { title: 'Ping is not measured yet: the agents do not time their heartbeat to Potluck (it needs the agent track).' }, 'not measured')),
          fact(since ? 'Uptime, since ' + since : 'Uptime, 7 days', h('b', pctText(up)))),
        h('div.dh', { key: 'attn-h' }, h('h3', 'Needs attention ', h('span.mut', { style: 'font-size:12px;font-weight:600' }, num(attn.length)))),
        attn.length ? attn.map(a => {
          const x = zoneOfId(a.id);
          const z = x ? x.z : { square: 'down', label: a.state === 'lost' ? 'Offline' : 'Set aside', rest: null };
          return h('div.pf.attn', { key: 'attn-' + a.id, 'data-attn': a.id, role: 'button', tabindex: '0', onclick: () => pickZone(a.id), onkeydown: e => { if (e.key === 'Enter') pickZone(a.id); } },
            h('span.mono', { style: 'font-weight:500' }, a.name || (x && x.name) || a.id),
            h('span.sts', h('i', { class: 'sq ' + z.square, 'aria-hidden': 'true' }), h('span.stx', z.label, z.rest)),
            h('span.chev', icon('M8 5l5 5-5 5', 15)));
        }) : h('div.pf.none', { key: 'attn-none' }, h('span.mut', 'Nothing needs a look: every zone of it is reachable.')),
        h('div.dh', { key: 'models-h', style: 'margin-top:8px' }, h('h3', 'Models here')),
        models.length ? models.map(m => {
          const inst = ((state.status || {}).instances || []).find(i => i.profile === m.profile);
          const dr = isDraining(inst);
          return h('div.pf', { key: 'm-' + m.profile, 'data-model': m.profile },
            h('span', h('span.mono', { style: 'font-weight:500', title: m.profile }, modelName(m.profile)),
              dr ? h('span.mut', { style: 'font-size:12px;font-weight:600', 'data-draining': '' }, ' · unloads when its requests end, ' + drainText(inst)) : null),
            h('span', { style: 'text-align:right;font-weight:600' }, plural(m.zones || 0, 'zone')),
            dr ? button('Cancel unload', () => undrainAsk(m.profile), 'sm', { 'data-undrain': m.profile, style: 'justify-content:center', title: 'Take the unload back: it takes requests again' })
              : button('Unload', () => askUnload(m.profile, { group: name }), 'sm', { 'data-unload': m.profile, style: 'justify-content:center' }));
        })
          : h('div.pf.none', { key: 'models-none' }, h('span.mut', 'No model is loaded on its zones.')),
        h('div.zmore', { key: 'gevents' }, eventsPart(ghist && ghist.name === name ? ghist : null, 'No load, unload or trouble recorded for its zones yet.')),
      ];
    }
    function groupPanel(name) {
      const doc = () => groupDoc(name);
      const sub = () => {
        const d = doc();
        if (!d) return null;
        const members = cur.all.filter(x => x.group === name);
        const each = (d.memory && d.memory.each_total_mib) || (members.length && members.every(x => x.n.total_mib === members[0].n.total_mib) ? members[0].n.total_mib : 0);
        const count = d.count != null ? d.count : members.length;
        return [plural(count, 'zone'), groupChip(d, members) || null, each ? `${gb(each)} each` : null].filter(Boolean).join(' · ');
      };
      const controls = () => {
        const d = doc();
        if (!d) return null;
        const v = d.accepting === 'all' ? 'true' : d.accepting === 'none' ? 'false' : 'mixed';
        return h('label.gsw', v === 'mixed' ? h('span.mut', { style: 'font-weight:600' }, 'Some zones') : null, 'Accepting models',
          h('button.switch', { type: 'button', role: 'switch', 'aria-checked': v, 'aria-label': 'Accepting models', title: v === 'mixed' ? 'Some of its zones are not accepting models: switch it on to let them all accept again' : null,
            onclick: () => switchAsk(name, d) }, h('span')));
      };
      const foot = () => h('a.gp-act', { href: href('activity'), 'data-gp': 'activity', title: 'Its recent events are listed above (the history filtered by this group). Activity itself shows every zone: it has no group filter yet.' }, 'Activity for this group', icon('M8 5l5 5-5 5', 15));
      return sidePanel({ label: 'Selected group', title: h('span.mono', name), sub, controls, body: () => groupBody(name), foot,
        name: 'Selected group: ' + name,
        onClose: () => { if (side && side.key === 'g:' + name) { side = null; navigate('zones', [], r.query, true); } } });
    }

    // ---------- the zone panel (Zones-Fleet-Detail, #/zones/<name>) ----------
    let week = null; // {id, doc} the open zone's last 7 days by hour
    const loadWeek = async id => {
      week = { id, doc: null };
      try {
        const doc = await api.get('/api/nodes/availability', { node: id, from: '7d', step: 'hour' });
        if (week && week.id === id) week = { id, doc };
      } catch (e) { if (week && week.id === id) week = { id, doc: null, error: (e && e.message) || 'the request failed' }; }
      if (alive && side && side.id === id) side.redraw();
    };
    // the profiles "Load a model" offers (GET /api/profiles, read when a zone
    // panel first opens and again after a configuration change); the
    // generated ones (benchmark matrices) are folded into their own group
    let profs = null; // {list, error}
    const loadPick = new Map(); // zone id -> the chosen profile
    const readProfiles = async () => {
      try { const d = await api.get('/api/profiles'); profs = { list: (d && d.profiles) || [] }; } catch (e) { profs = { list: [], error: (e && e.message) || 'the request failed' }; }
      if (alive && side) side.redraw();
    };
    const offConfig = listen('config', () => { if (profs) readProfiles(); });
    function loadPart(id) {
      if (!profs) { readProfiles(); profs = { list: null }; }
      const list = profs.list;
      const isGen = x => (typeof x.generated === 'boolean' ? x.generated : false);
      const label = x => { const s = modelName(x.id); return s && s !== x.id ? `${s} · ${x.id}` : x.id; };
      const cur_ = loadPick.get(id) || '';
      const sel_ = h('select.select', { 'data-zp': 'profile', 'aria-label': 'Choose a profile', disabled: !list || !list.length, style: 'flex:1;font-weight:600',
        onchange: e => { loadPick.set(id, e.target.value); if (side) side.redraw(); } },
        h('option', { value: '', selected: !cur_ }, !list ? 'Reading the profiles…' : profs.error ? 'The profiles could not be read' : list.length ? 'Choose a profile' : 'No profile to load'),
        list ? [list.filter(x => !isGen(x)).map(x => h('option', { value: x.id, selected: x.id === cur_ }, label(x))),
          list.some(isGen) ? h('optgroup', { label: 'Generated for benchmarks' }, list.filter(isGen).map(x => h('option', { value: x.id, selected: x.id === cur_ }, label(x)))) : null] : null);
      return h('div.gp-acts', { key: 'load' }, sel_,
        button('Load', () => { const p = loadPick.get(id); if (p) askLoad(p, [id]); }, '', { 'data-zp': 'load', disabled: !cur_,
          title: profs.error ? 'The profiles could not be read: ' + profs.error : cur_ ? 'Shows the plan for the zones you choose, then loads' : 'Choose a profile first' }));
    }
    const STATE_SHORT = { use: 'In use', free: 'Free', load: 'Loading', off: 'Switched off' };
    function zoneHead(st, n) {
      const a = agentOf(st, n) || {};
      const z = zoneOf(st, n);
      const g = a.group || n.group || '';
      return h('div.sts', { title: z.title },
        h('i', { class: 'sq ' + z.square, 'aria-hidden': 'true' }),
        z.pill === 'down' ? z.label : STATE_SHORT[z.pill] || z.label,
        h('span.mut', { style: 'font-weight:600' }, ' · ' + (g || 'No group') + (n.priority ? ' · tier ' + n.priority : '')));
    }
    function zoneSwitch(st, n) {
      const a = agentOf(st, n);
      if (!a) return null;
      const name = n.hostname || a.name || n.id;
      const excluded = !!(n.excluded || a.excluded);
      const on_ = !a.paused && !excluded;
      const z = zoneOf(st, n);
      return h('label.gsw', 'Accepting models',
        h('button.switch', { type: 'button', role: 'switch', 'aria-checked': on_ ? 'true' : 'false', 'aria-label': 'Accepting models', disabled: excluded,
          title: excluded ? z.title || 'Excluded in the configuration (exclude_nodes)' : null,
          onclick: () => ask({ title: [on_ ? 'Stop accepting models on ' : 'Accept models again on ', h('span.mono', name), '?'], go: on_ ? 'Stop accepting models' : 'Accept models again',
            lines: on_ ? ['It takes no new model until someone lets it accept models again.', 'What it holds now keeps serving requests.'] : ['It may take a new model again, as soon as nothing else holds it back.'],
            run: c => sendNodeAction(api, n.id, on_ ? 'pause' : 'unpause', c).then(() => toast(on_ ? `${name} takes no new model` : `${name} accepts models again`, 'ok')) }) }, h('span')));
    }
    // weekGrid(doc, now) -> 7 rows (oldest day first) of 24 hours, local time
    function weekGrid(doc) {
      const cells = new Map();
      const ns = doc && (doc.nodes || [])[0];
      for (const c of (ns && ns.cells) || []) { const t = tms(c.start); if (t != null) cells.set(Math.floor(t / 3600000), c); }
      const nowMs = Date.now();
      const today = new Date(nowMs); today.setHours(0, 0, 0, 0);
      const DAY = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
      const out = [h('b'), h('span', '00'), h('span', '06'), h('span', '12'), h('span', '18')];
      for (let d = 6; d >= 0; d--) {
        const day = new Date(today); day.setDate(today.getDate() - d);
        out.push(h('b', DAY[day.getDay()]));
        for (let hr = 0; hr < 24; hr++) {
          const at_ = new Date(day); at_.setHours(hr);
          const t = at_.getTime();
          if (t > nowMs) { out.push(h('i.fu', { title: hourText(t) + ' · not yet' })); continue; }
          const c = cells.get(Math.floor(t / 3600000));
          out.push(c ? h('i', { class: 'sq ' + availClass(c), title: `${DAY[day.getDay()]} ${hourText(t)} · ${cellWords(c)}` }) : h('i.nd', { title: `${DAY[day.getDay()]} ${hourText(t)} · nothing known` }));
        }
      }
      return h('div.wg', { role: 'img', 'aria-label': 'Last 7 days by hour, one row per day' }, out);
    }
    function zoneBody(id) {
      const st = state.status;
      const n = st && viewNodes(st).find(x => x.id === id);
      if (!n) return h('div.zempty', empty('This zone is not in the status', 'It may have left the pool. Close this panel to see the zones there are.'));
      const a = agentOf(st, n) || {};
      const name = n.hostname || a.name || n.id;
      const mem = memOf(n);
      const temp = tempOf(a);
      // the last cold load (last_load_link): a lower bound of the link's rate; absent: none yet, never 0
      const link = a.last_load_link && a.last_load_link.mb_s > 0 ? a.last_load_link : null;
      const u = uptimeDoc && (uptimeDoc.zones || []).find(v => v.id === id || v.name === name);
      const win = uptimeDoc && uptimeDoc.window;
      const since = win && win.complete === false && tms(win.covered_from) ? dayText(tms(win.covered_from)) : '';
      const fact = (label, value, more) => h('div.fact', h('span.lbl', label), value, more || null);
      const m = member(st, id);
      const inst = m ? m.inst : n.holder ? (st.instances || []).find(i => i.profile === n.holder) : null;
      const nameOfId = x => { const y = viewNodes(st).find(v => v.id === x || v.hostname === x); return y ? y.hostname || y.id : x; };
      let loaded;
      if (inst) {
        const others = (inst.nodes || []).filter(x => x.id !== id);
        const hostM = (inst.nodes || []).find(x => x.role === 'host');
        const role = m ? roleShare(m.role, m.share) : '';
        const runs = inst.in_flight || 0;
        const since_ = tms(inst.ready_since);
        const parts = [];
        if (inst.state === 'starting') { const f = inst.load && inst.load.progress != null ? inst.load.progress : inst.load_progress; parts.push(f != null ? `${num(f * 100)}% loaded` : 'Loading'); }
        else if (role) parts.push(role);
        if (m && m.role === 'host' && others.length) parts.push([others.length === 1 ? 'helper ' : 'helpers ', others.map((x, i) => [i ? ', ' : '', h('span.mono', { style: 'font-size:12px' }, nameOfId(x.id))])]);
        else if (hostM && hostM.id !== id) parts.push(['host ', h('span.mono', { style: 'font-size:12px' }, nameOfId(hostM.id))]);
        if (inst.state !== 'starting') parts.push(plural(runs, 'request'));
        if (since_) parts.push(['loaded ', h('time', { title: dateTime(since_), datetime: new Date(since_).toISOString() }, agoText(since_))]);
        const pins = pinnedOf(inst);
        if (pins.length) parts.push(h('span', { 'data-pinned': '', title: 'Loaded on chosen zones: ' + pins.map(nameOfId).join(', ') + '. Its next ordinary load is placed as its profile says.' }, 'on chosen zones'));
        const dr = isDraining(inst);
        loaded = h('div.zl', { key: 'loaded' },
          h('div', { style: 'flex:1;min-width:0' }, h('div.mono', { style: 'font-size:14px;font-weight:500', title: inst.profile }, modelName(inst.profile)),
            h('div.mut', { style: 'font-weight:600' }, parts.map((p, i) => [i ? ' · ' : '', p])),
            dr ? h('div', { 'data-draining': '', style: 'font-weight:600;font-size:12px' }, 'Unloads when its requests end, ' + drainText(inst) + '. New requests wait in the queue.') : null),
          dr ? button('Cancel unload', () => undrainAsk(inst.profile), '', { 'data-zp': 'undrain', title: 'Take the unload back: it takes requests again' }) : null,
          button('Force unload', () => askUnload(inst.profile, { zone: id }), 'red', { 'data-zp': 'force' }));
      } else {
        const z = zoneOf(st, n);
        loaded = h('div.zl', { key: 'loaded' }, h('span.mut', { style: 'font-weight:600' }, 'Nothing is loaded.', z.pill === 'free' && z.label !== 'Free' ? [' ', z.label, z.rest] : null));
      }
      const w = week && week.id === id ? week : null;
      return [
        h('div.gp-acts', { key: 'ssh' },
          h('div.cmd', { 'data-zp': 'ssh', title: SSH_WHY }, h('span.mut', 'SSH: not available yet')),
          button('Copy', null, '', { 'data-zp': 'copy', disabled: true, title: SSH_WHY }),
          button([icon('M3 5h14v10H3zM6 9l2 1.5L6 12M10 12h4', 15), 'Open terminal'], null, '', { 'data-zp': 'terminal', disabled: true, title: SSH_WHY }),
          button([icon('M16 10a6 6 0 1 1-2-4.5M16 4v4h-4', 15), 'Reboot'], null, 'red', { 'data-zp': 'reboot', disabled: true, title: REBOOT_WHY })),
        h('div.f2', { key: 'facts' },
          fact('Memory', mem ? h('b', gbNum(mem.used), h('span.mut.of', ` of ${gb(mem.total)}`)) : h('b.mut', DASH),
            mem ? h('div.mb', { role: 'img', 'aria-label': `${Math.round(mem.used / mem.total * 100)}% of memory in use` }, h('span', { style: `width:${Math.round(mem.used / mem.total * 100)}%` })) : null),
          fact('Temperature', h('b', temp == null ? DASH : `${num(temp)} °C`)),
          fact('Ping', h('b.mut', { title: 'Ping is not measured yet: the agent does not time its heartbeat to Potluck (it needs the agent track).' }, 'not measured')),
          fact('Uptime', typeof a.host_uptime_s === 'number' ? h('b', { title: 'Since the machine last started, as its agent reports it' }, goneFor(a.host_uptime_s * 1000)) : h('b.mut', { title: 'The agent reports no uptime' }, DASH)),
          fact('Link speed', link
            ? h('b', { 'data-link': '', title: `At least this fast: ${num(link.bytes || 0)} bytes in ${num(Math.round((link.load_s || 0) * 10) / 10)} s, and that time also holds the model's start. Not a link test.` }, `≥ ${num(Math.round(link.mb_s))} MB/s`)
            : h('b.mut', { 'data-link': '', title: 'No load has sent this zone a model over the network yet (a cached or a short load does not count).' }, 'no cold load yet'),
          link ? h('span.mut', { style: 'font-size:12px;font-weight:600' }, 'last cold load, ', tms(link.at) ? `${dayText(tms(link.at))} ${clock(tms(link.at), false)}` : DASH,
            link.model ? [' · ', h('span.mono', { title: link.model }, modelName(link.model))] : null) : null),
          fact(since ? 'Up, since ' + since : 'Up, last 7 days', h('b', pctText(u ? u.uptime : null)))),
        h('div.dh', { key: 'loaded-h' }, h('h3', 'Loaded now')),
        loaded,
        h('div.dh', { key: 'load-h' }, h('h3', 'Load a model')),
        loadPart(id),
        h('div.dh', { key: 'week-h' }, h('h3', 'Last 7 days'),
          h('span.mut.wkey', 'Idle', h('i.sq.free'), h('i.sq.use2'), h('i.sq.use4'), 'Busy', h('i.sq.down', { style: 'margin-left:8px' }), 'Down')),
        h('div.wgbox', { key: 'week' }, !w || (!w.doc && !w.error) ? h('span.mut', 'Reading the last 7 days…') : w.error ? h('span.err-t', 'The last 7 days could not be read: ' + w.error) : weekGrid(w.doc)),
        h('div.zmore', { key: 'more' }, eventsPart(hist && hist.id === id ? hist : null), h('section.zpart', h('h3', 'Machine'), zoneFacts(n, agentOf(st, n)))),
      ];
    }
    function zonePanelOf(id) {
      const nodeNow = () => state.status && viewNodes(state.status).find(x => x.id === id);
      const title = () => { const n = nodeNow(); if (!n) return 'Zone'; const a = agentOf(state.status, n); const c = chipOf(a); return [h('span.mono', n.hostname || n.id), c ? h('span.mut.chipt', ' | ' + c) : null]; };
      const sub = () => { const n = nodeNow(); return n ? zoneHead(state.status, n) : null; };
      const controls = () => { const n = nodeNow(); return n ? zoneSwitch(state.status, n) : null; };
      const foot = () => { const n = nodeNow(); return h('a.gp-act', { href: href('activity', [], { q: (n && n.hostname) || id }), 'data-zp': 'activity', title: 'Activity, searched for this zone\'s name' }, 'Activity for this zone', icon('M8 5l5 5-5 5', 15)); };
      const p = sidePanel({ label: 'Selected zone', title, sub, controls, body: () => zoneBody(id), foot,
        onClose: () => { if (side === p) { side = null; setQuery({ zone: '' }, true); } } });
      return p;
    }

    const view = liveView(el, () => {
      const st = state.status;
      if (side) side.redraw();
      const q = r.query;
      const all = !st ? [] : viewNodes(st).filter(n => n.state !== 'removed' && (agentOf(st, n) || {}).zone_state !== 'removed').map(n => {
        const a = agentOf(st, n) || {};
        const z = zoneOf(st, n);
        // needs attention first: the server's attention_rank (GET /api/nodes, the status agents), else the page's own
        const nd = nodesDoc && (nodesDoc.nodes || []).find(v => v.id === n.id);
        const rank = typeof a.attention_rank === 'number' ? a.attention_rank : nd && typeof nd.attention_rank === 'number' ? nd.attention_rank : z.rank;
        return { n, a, z, rank, id: n.id, name: n.hostname || a.name || n.id, chip: chipOf(a), reported: typeof a.chip === 'string' ? a.chip : '', mem: memOf(n), temp: tempOf(a), group: a.group || n.group || '',
          model: (member(st, n.id) || {}).inst ? member(st, n.id).inst.profile : n.holder || '', addr: String(a.addr || n.addr || '').replace(/:\d+$/, '') };
      });
      // the one set of counts; until it is read, the same reading of the live status
      const c = (nodesDoc && nodesDoc.counts) || (() => {
        const k = { total: all.length, in_use: 0, free: 0, loading: 0, down: 0, off: 0 };
        for (const x of all) k[{ use: 'in_use', free: 'free', load: 'loading', down: 'down', off: 'off' }[x.z.pill]]++;
        k.reachable = k.total - k.down;
        return k;
      })();
      // with a panel open the list narrows (Zone, Model status, Memory) and
      // the pills keep all, in use, free and down
      const narrow = !!(groupOpen() || q.zone);
      const pillsHere = narrow ? PILLS.filter(([id]) => ['use', 'free', 'down'].includes(id)) : PILLS;
      const show = PILLS.some(([id]) => id === q.show) ? q.show : '';
      const pills = h('div.zpills', { role: 'group', 'aria-label': 'Show' },
        h('button', { class: 'fc all' + (!show ? ' on' : ''), type: 'button', 'aria-pressed': !show ? 'true' : 'false', 'data-show': '',
          title: `${num(c.reachable)} of ${num(c.total)} reachable` + (c.down ? ` · ${num(c.down)} down` : ''), onclick: () => setQuery({ show: '' }) },
        `ALL ${num(c.total)} ZONE${c.total === 1 ? '' : 'S'}`),
        pillsHere.map(([id, word, key]) => h('button', { class: 'fc' + (show === id ? ' on' : ''), type: 'button', 'aria-pressed': show === id ? 'true' : 'false', 'data-show': id,
          onclick: () => setQuery({ show: show === id ? '' : id }) }, `${num(c[key] || 0)} ${word}`)),
        narrow ? null : button([icon('M10 4v12M4 10h12'), 'Create new zone'], createInfo, 'pri', { 'data-act': 'create', style: 'margin-left:auto' }));
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
      const cmp = { name: byName, attention: (a, b) => (a.rank - b.rank) || byName(a, b),
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
      // with no group at all the zones are listed flat: no lone "No group" row
      const headed = grouped && groupNames.length > 0;
      if (headed) {
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
      // groups whose zones do not share one chip: each row names its own
      const mixedChip = new Set();
      for (const g of new Set(all.map(x => x.group))) if (new Set(all.filter(x => x.group === g && x.chip).map(x => x.chip)).size > 1) mixedChip.add(g);
      const pick = pickZone;
      const grid = narrow ? 'fr v2n' : 'fr v2';
      const zoneRow = (x, indent) => {
        const { z } = x;
        const off = z.pill === 'off';
        const u = upOf(x);
        const s = strips.get(x.id);
        return h('div', { key: 'z-' + x.id, class: grid + (q.zone === x.id ? ' sel' : '') + (sel.has(x.id) ? ' chk' : ''), 'data-zone': x.id, role: 'row', tabindex: '0',
          'aria-label': x.name + ', ' + z.label, onclick: () => pick(x.id), oncontextmenu: e => rowMenu(x, e, e.currentTarget),
          onkeydown: e => { if (e.key === 'Enter') { e.preventDefault(); pick(x.id); } else if (e.key === 'ContextMenu' || (e.shiftKey && e.key === 'F10')) rowMenu(x, { preventDefault: () => e.preventDefault() }, e.currentTarget); } },
        canTick(x) ? h('input', { class: 'cb', type: 'checkbox', checked: sel.has(x.id), 'aria-label': 'Select ' + x.name, 'data-sel': x.id,
          onclick: e => { e.stopPropagation(); toggle(x.id); }, onkeydown: e => e.stopPropagation() }) : h('span'),
        h('span.zn', { style: indent ? 'padding-left:26px' : null }, h('span.mono.znm', x.name), x.chip && mixedChip.has(x.group) ? h('span.mut', { style: 'font-weight:600' }, ' | ' + x.chip) : null),
        h('span.sts', { title: z.title }, h('i', { class: 'sq ' + z.square, 'aria-hidden': 'true' }), h('span.stx', z.label, z.rest)),
        h('span', { class: 'r mono' + (!x.mem || off ? ' mut' : '') }, memText(x.mem)),
        narrow ? null : [
          h('span', { class: 'r mono' + (x.temp == null || off ? ' mut' : '') }, x.temp == null ? DASH : `${num(x.temp)} °C`),
          h('span', { class: 'r mono' + (u == null ? ' mut' : ''), title: u == null ? (off ? 'Switched off: its time is not counted' : 'Not known yet') : null }, pctText(u)),
          availStrip(s.cells, s.label)]);
      };
      const groupRow = (name, zs, doc, closed) => {
        const label = name || 'No group';
        const members = all.filter(x => x.group === name);
        const cs = (doc && doc.counts) || null;
        const chipsHere = [...new Set(members.map(x => x.chip).filter(Boolean))];
        const gchip = groupChip(doc, members);
        const count = doc && doc.count != null ? doc.count : members.length;
        const memD = doc && doc.memory;
        const tempD = doc && doc.temp && typeof doc.temp.avg_c === 'number' ? doc.temp.avg_c : null;
        const up7 = doc && doc.uptime_7d ? doc.uptime_7d.uptime : null;
        const gcs = [['in_use', 'use4', 'in use'], ['free', 'free', 'free'], ['loading', 'load', 'loading'], ['down', 'down', 'down'], ['off', 'off', 'off']]
          .map(([k, sq, w]) => { const v = cs ? cs[k] : members.filter(x => PILL_OF[x.a.zone_state] === { in_use: 'use', free: 'free', loading: 'load', down: 'down', off: 'off' }[k]).length; return v ? h('span', { title: `${num(v)} ${w}` }, h('i', { class: 'sq ' + sq, 'aria-hidden': 'true' }), num(v)) : null; });
        const gstrip = groupStrip(members.map(x => strips.get(x.id)), label);
        const visible = zs.filter(x => canTick(x) && onPage.some(i => i.kind === 'zone' && i.x === x));
        const ticked = visible.length > 0 && visible.every(x => sel.has(x.id));
        const open_ = name && groupOpen() === name;
        return h('div', { key: 'g-' + label, class: grid + ' gh' + (open_ ? ' sel' : '') + (name ? ' gopen' : ''), 'data-group': name || '-', role: 'row', tabindex: name ? '0' : null,
          'aria-label': name ? label + ', open the group' : null, onclick: name ? () => openGroup(name) : null, onkeydown: name ? e => { if (e.key === 'Enter') { e.preventDefault(); openGroup(name); } } : null },
          h('input', { class: 'cb', type: 'checkbox', checked: ticked, 'aria-label': 'Select all in ' + label, 'data-sel': 'group:' + (name || '-'), disabled: !visible.length,
            onclick: e => { e.stopPropagation(); tickMany(visible, !ticked); } }),
          h('span.gn', h('button', { class: 'more', type: 'button', 'aria-expanded': closed ? 'false' : 'true', 'aria-label': (closed ? 'Expand ' : 'Collapse ') + label, 'data-fold': name || '-',
            onclick: e => { e.stopPropagation(); if (shut.has(name)) shut.delete(name); else shut.add(name); view.refresh(); } }, icon(closed ? 'M8 5l5 5-5 5' : 'M5 8l5 5 5-5', 14)),
          h(name ? 'span.mono.gname' : 'span.gname', label),
          // the group's chip (GET /api/groups `chip`): "N · chip" only when every
          // member reports that one; otherwise the count, "mixed" and the names on hover
          h('span.mut.gmeta', { title: chipsHere.length > 1 ? 'mixed: ' + chipsHere.join(', ') : null }, num(count) + (gchip ? ' · ' + gchip : count === 1 ? ' zone' : ' zones'))),
          h('span.gc', gcs),
          h('span.r', h('span.mono', memD && memD.total_mib ? `${num(Math.round(memD.used_mib / 1024))} / ${num(Math.round(memD.total_mib / 1024))} GB` : DASH)),
          narrow ? null : [
            h('span.r.mono', { title: 'Average across the group' }, tempD == null ? DASH : `${num(tempD)} °C`),
            h('span.r.mono', { title: up7 == null ? 'Not known yet' : 'Over the time of all its zones' + (since ? ' since ' + since : ', last 7 days') }, pctText(up7)),
            availStrip(gstrip.cells, gstrip.label)]);
      };

      const list = [];
      let lastGroup = null;
      for (const it of onPage) {
        if (it.kind === 'group') { list.push(groupRow(it.name, it.zs, it.doc, it.shut)); lastGroup = it.name; continue; }
        if (headed && it.group !== lastGroup) { list.push(groupRow(it.group, it.zs, it.doc, false)); lastGroup = it.group; }
        list.push(zoneRow(it.x, headed));
      }

      const fs = (label, opts, cur, key) => dropButton(label, opts, cur, v => setQuery({ [key]: v === opts[0].id ? '' : v }), 'zn-' + key);
      const findBox = h('label.zx', { 'data-keep': '', key: 'find' },
        icon('M13.5 13.5L17 17M14.5 9a5.5 5.5 0 1 1-11 0 5.5 5.5 0 0 1 11 0z', 14),
        (() => { const i = h('input', { type: 'text', 'data-search': 'zones', value: q.q || '', placeholder: 'Search name, chip, model or address', 'aria-label': 'Search zones' });
          i.addEventListener('input', () => setQuery({ q: i.value })); return i; })());
      const anyOpen = groupNames.concat(['']).some(n => !shut.has(n));
      const filterButtons = () => [
        fs('Chip', [{ id: '', label: 'any' }].concat(chips.map(c_ => ({ id: c_, label: c_ }))), chipF, 'chip'),
        fs('Model', [{ id: '', label: 'any' }].concat(models.map(m => ({ id: m, label: m })), [{ id: NONE_MODEL, label: 'nothing loaded' }]), modelF, 'model'),
        fs('Memory free', MEMS, memF, 'mem')];
      const nOn = [chipF, modelF, memF].filter(Boolean).length + (sort !== 'attention' ? 1 : 0);
      const tools = h('div.ztools',
        findBox,
        narrow ? null : filterButtons(),
        h('div.ztools-r',
          narrow ? h('button', { class: 'fs2' + (filtersOpen ? ' open' : ''), type: 'button', 'data-act': 'filters', 'aria-expanded': filtersOpen ? 'true' : 'false',
            onclick: () => { filtersOpen = !filtersOpen; view.refresh(); } }, 'Filters' + (nOn ? ` · ${num(nOn)}` : '')) : null,
          h('button', { class: 'fs2' + (grouped ? ' on' : ''), type: 'button', 'data-act': 'grouped', 'aria-pressed': grouped ? 'true' : 'false', disabled: !hasGroups,
            title: hasGroups ? (grouped ? 'Show every zone in one list' : 'Show the zones under their groups') : 'No groups yet: select zones and make one from the selection',
            onclick: () => setQuery({ flat: grouped ? '1' : '' }) }, 'Grouped'),
          grouped && !narrow ? h('button', { class: 'fs2', type: 'button', 'data-act': 'collapse', onclick: () => { if (anyOpen) groupNames.concat(['']).forEach(n => shut.add(n)); else shut.clear(); view.refresh(); } },
            anyOpen ? 'Collapse all' : 'Expand all') : null,
          narrow ? null : fs('Sort', SORTS, sort, 'sort')));
      const tools2 = narrow && filtersOpen ? h('div.ztools.ztools2', filterButtons(), fs('Sort', SORTS, sort, 'sort')) : null;

      const head = h('div', { class: grid + ' fhd', key: 'head', role: 'row' },
        h('input', { class: 'cb', type: 'checkbox', checked: allTicked, 'aria-label': 'Select all', 'data-sel': 'all', disabled: !shownZones.length, onclick: () => tickMany(shownZones, !allTicked) }),
        h('span.lbl', 'Zone'), h('span.lbl', 'Model status'), h('span.lbl.r', 'Memory'),
        narrow ? null : [h('span.lbl.r', 'Temp'),
          h('span.lbl.r', { title: upTitle }, since ? h('span.uph', 'Uptime', h('small', 'since ' + since)) : 'Uptime'),
          h('span.lbl.strip-h', h('span', '24 h ago'), h('span', 'Availability, hour by hour'), h('span', { style: 'color:var(--ink)' }, 'now'))]);

      cur = { all, groups: gs };
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
      const holds = chosen.some(holding);
      const selPart = h('div.zsel', { role: 'region', 'aria-label': 'Selection' }, h('span', `${num(chosen.length)} selected`), h('span', { style: 'width:4px' }),
        button('New group', () => newGroup(chosenNow()), 'sm', { 'data-bulk': 'newgroup' }),
        button('Move to group', e => { const b = e.currentTarget, rc = b.getBoundingClientRect(); openMenu(rc.left, rc.top - 30 * (cur.groups.length + 1) - 24, b, true); }, 'sm',
          { 'data-bulk': 'move', 'aria-haspopup': 'true' }),
        button('Reboot', null, 'sm', { 'data-bulk': 'reboot', disabled: true, title: REBOOT_WHY }),
        bulkButton('pause'),
        bulkCount('unpause') ? bulkButton('unpause') : null,
        button('Force unload', () => forceUnload(chosenNow()), 'sm red', { 'data-bulk': 'force', disabled: !holds, title: holds ? null : 'None of the selected zones holds a model' }),
        button('Clear', () => { sel.clear(); view.refresh(); }, 'sm ghost', { 'data-act': 'clear' }));
      const totals = narrow && pages === 1 ? h('span.mut', { style: 'font-weight:600' }, (gs.length ? plural(gs.length, 'group') + ' · ' : '') + plural(all.length, 'zone')) : null;
      const foot = h('div.bulk', { key: 'foot' }, chosen.length ? selPart : narrow ? totals : keyPart,
        // no pager (no "Page 1 of 1") when everything fits on one page
        pages > 1 ? pager({ page, pages, onPage: n => setQuery({ page: n === 1 ? '' : String(n) }, true), label: 'Zone pages' }) : null);

      const body = all.length
        ? (list.length ? list : h('div.zempty', 'No zone matches.'))
        : h('div.zempty', agentsMode(st)
          ? empty('No machines have joined yet', 'Machines join through their agent. None has registered so far; start the agent on a machine and it appears here.')
          : empty('No zones are known yet', 'Zones come from the fleet feed. None has been reported so far; About shows the connection.'));
      return [pills, h('section', { class: 'w zfleet' + (narrow ? ' narrow' : ''), 'aria-label': 'Zones' }, tools, tools2, head, h('div.zlist', body), foot)];
    }, ['status', 'tick', 'conn']);
    syncSide();
    return {
      update(nr) { r = nr; syncSide(); view.refresh(); },
      unmount() { alive = false; closeMenu(); clearTimeout(soon); offStatus(); offStartups(); offDrop(); offConfig(); if (side) { const s = side; side = null; s.close(); } view.unmount(); },
    };
  },
});
