// Zones: every machine in one table (design: Zones board 1 and the zones
// table of Components board 3). "Zone" is the UI's word for a node: one
// registered machine (plan, D1). The states are Potluck's, mapped one to one
// (D2): in use, loading, free, set aside, offline, off (excluded in the
// configuration), not accepting models (paused). Both laptops are excluded
// and show as Off (D3); one that still runs a model says so.
//
// A row opens the zone's side panel (Zones board 4, the parts that exist):
// its state, its facts, its recent events, then the same card as the Machine
// details page (views/nodes.js, tile()) with its two controls (Pause or
// Unpause, Force unload).
//
// Zones whose machine has an agent can be ticked (board 2). The selection bar
// offers the two actions that exist for several machines: Stop accepting
// models (a pause on each) and Accept models again, each with a required
// comment, sent one zone after the other. Force unload stays on one zone at a
// time: it ends the model on every machine it spans, not only the selected.
//
// Left out until their data exists (the plan): the availability strip and
// uptime % (U4, D14), ping (U6, D12), groups and their menu items (U6, D4),
// the chip, memory and temperature filters, Create new zone (U6, D15),
// Restart services and Reboot (U6b, D9), Open SSH (no browser can), the 7-day
// grid and "Load a model" (U6, D8), measured link speed.
//
// Reads: WebSocket "status" (view.nodes, instances, quarantines, agents with
// their stats); GET /api/history?node= for the open zone's events. Writes:
// only the node actions of core/nodeactions.js (pause, unpause, force unload).

import { h } from '../core/dom.js';
import { registerView, navigate, href } from '../core/router.js';
import { state, on } from '../core/store.js';
import * as api from '../core/api.js';
import { liveView, pageHead, chips, filterSelect, searchField, zoneStatus, colourKey, sidePanel, skeleton, badge, empty, button, facts, dialog, toast } from '../core/ui.js';
import { num, gb, gbNum, ago, at, plural, durShort, dateTime, agoText, DASH } from '../core/format.js';
import { viewNodes } from '../core/status.js';
import { agentsMode, agentOf, notPlaceable, sendNodeAction } from '../core/nodeactions.js';
import { onStartups } from '../core/startup.js';
import { tile, settlePending } from './nodes.js';

const PAGE = 50; // rows per page; with a handful of machines there is one page and no pager

// zoneOf(st, n) -> {group, square, label, doing, rank, title}: one machine's
// state in the Zones words. group is the filter it belongs to (use, load,
// free, down, off); rank orders "needs attention first".
export function zoneOf(st, n) {
  const agent = agentOf(st, n);
  const inst = n.holder ? (st.instances || []).find(i => i.profile === n.holder) : null;
  const q = (st.quarantines || []).find(x => x.node === n.id);
  const np = notPlaceable(n, agent);
  const mono = t => h('span.mono', t);
  if (n.excluded || (agent && agent.excluded)) {
    const running = (agent && agent.running_while_excluded) || (n.holder ? { profile: n.holder } : null);
    const by = n.excluded_by || (agent && agent.excluded_by) || '';
    // why it is off, in view: the exclude_nodes entry that matches it
    const why = by ? h('span.mut', 'Off by exclude_nodes: ', mono(by)) : h('span.mut', 'Off by exclude_nodes');
    return { group: 'off', square: 'off', label: 'Off', rank: 6,
      title: by ? 'Excluded in the configuration (exclude_nodes: ' + by + ')' : 'Excluded in the configuration',
      doing: running ? [h('span.warn-t', 'Excluded, still running '), mono(running.profile), h('br'), why] : why };
  }
  if (!n.online) {
    return { group: 'down', square: 'down', label: 'Offline', rank: 0,
      doing: h('span.mut', n.missing_since ? ['Gone since ', at(n.missing_since, false), ' (', ago(n.missing_since), ')'] : 'Not reporting') };
  }
  if (n.health && n.health.state === 'hung') return { group: 'down', square: 'down', label: 'Not answering', rank: 0, doing: h('span.mut', 'The worker stopped answering') };
  if (q || n.quarantined) {
    return { group: 'down', square: 'down', label: 'Set aside', rank: 1,
      doing: h('span.mut', q ? ['Back at ', at(q.until, false)] : 'After a failed check') };
  }
  const paused = !!(np && np.paused);
  const other = np && !np.paused && np.code !== 'excluded' ? np : null;
  const notAccepting = paused ? h('span.mut', ' · not accepting models') : null;
  if (inst && inst.state === 'starting') {
    const frac = inst.load && inst.load.progress != null ? inst.load.progress : inst.load_progress;
    return { group: 'load', square: 'load', label: 'Loading', rank: 2,
      doing: [frac != null ? `${num(frac * 100)}% loaded · ` : 'Loading · ', mono(n.holder), notAccepting] };
  }
  if (n.holder) {
    const busy = inst && inst.in_flight > 0 ? h('span.mut', ` · ${plural(inst.in_flight, 'request')}`) : null;
    return { group: 'use', square: 'use4', label: 'In use', rank: 3, doing: [mono(n.holder), busy, notAccepting] };
  }
  if (other) {
    return { group: other.code === 'silent' || other.code === 'agent_unreachable' ? 'down' : 'free', square: other.code === 'silent' || other.code === 'agent_unreachable' ? 'down' : 'free',
      label: other.short, rank: 1, title: other.text, doing: h('span.mut', 'Takes no new model') };
  }
  if (paused) return { group: 'free', square: 'free', label: 'Not accepting', rank: 4, title: np.text, doing: h('span.mut', 'Paused: takes no new model') };
  if (n.claimed_by) return { group: 'free', square: 'free', label: 'Claimed', rank: 4, doing: [h('span.mut', 'Next: '), mono(n.claimed_by)] };
  if (n.settling) return { group: 'free', square: 'free', label: 'Settling', rank: 5, doing: h('span.mut', 'After an unload') };
  return { group: 'free', square: 'free', label: 'Free', rank: 5, doing: h('span.mut', 'Nothing loaded') };
}

// memoryOf(n) -> "10.7 / 13.5 GB", or a dash when the machine reports nothing.
function memoryOf(n) {
  const total = n.total_mib || 0;
  if (!total || !n.online) return DASH;
  const free = Math.min(Math.max(0, n.free_mib || 0), total);
  return `${gbNum(total - free)} / ${gb(total)}`;
}

// tempOf(agent) -> its headline temperature, as its agent last reported it.
function tempOf(agent) {
  const t = agent && agent.stats && typeof agent.stats === 'object' ? agent.stats.headline_temp_c : null;
  return typeof t === 'number' ? `${num(t)} °C` : DASH;
}

// deviceOf(agent) -> the name of its first device, when its agent reports one.
function deviceOf(agent) {
  const d = agent && Array.isArray(agent.devices) ? agent.devices[0] : null;
  return d && (d.description || d.name) ? String(d.description || d.name) : '';
}

// The events of one zone the panel lists (GET /api/history?node=): loads,
// unloads and the machine's own trouble; not the minute memory samples.
const ZONE_EVENTS = ['loadStarted', 'loadFinished', 'loadStalled', 'loadCancelledLowMemory', 'preflightFailed', 'instanceUnloaded', 'childExited', 'eviction',
  'nodeLost', 'nodeQuarantined', 'quarantineEnded', 'workerHealth', 'recoveryRequest', 'recoveryResponse', 'recoveryOutcome', 'recoverySequenceStopped', 'benchStepFinished',
  'workerLaunchFailed', 'workerExited', 'nodeMembership', 'nodeAdmin'];
const EVENTS_SHOWN = 8;

// zoneFacts(n, agent) -> the facts of one zone that the table has no room for,
// each only when its source reports it.
function zoneFacts(n, agent) {
  const a = agent || {};
  const dev = deviceOf(agent);
  const up = typeof a.host_uptime_s === 'number' ? durShort(a.host_uptime_s) : null;
  const seen = typeof a.last_seen_ago_s === 'number' ? (a.last_seen_ago_s < 2 ? 'just now' : durShort(a.last_seen_ago_s) + ' ago') : null;
  return facts([
    dev ? ['Device', h('span', dev)] : null,
    a.platform ? ['Platform', h('span.mono', a.platform)] : null,
    a.release ? ['Agent release', h('span.mono', a.release)] : null,
    a.install ? ['Install', h('span', a.install)] : null,
    up ? ['Up for', h('span', up), 'Since the machine last started, as its agent reports it'] : null,
    seen ? ['Last report', h('span', seen)] : null,
    ['Memory', h('span', memoryOf(n))],
    ['Temperature', h('span', tempOf(agent))],
    ['Priority', h('span', n.priority ? String(n.priority) : 'no tier matches'), 'Lower numbers are used first when a model is placed'],
  ]);
}

// eventsPart(hist) -> the last events that name this zone, newest first.
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

// zonePanel(st, n, hist, refresh) -> the side panel of one zone: its state,
// its facts, its recent events, then its machine card with the controls.
function zonePanel(st, n, hist, refresh) {
  const agent = agentOf(st, n);
  const z = zoneOf(st, n);
  return [
    h('section.zpart', { key: 'state' },
      h('div.row', { style: 'gap:10px;flex-wrap:wrap' }, zoneStatus(z.square, z.label, z.title),
        n.auto === false && z.group !== 'off' ? badge('Named only', 'line', 'In no_auto_nodes: only a profile that names this zone in force_nodes puts a model here') : null,
        n.local ? badge('Runs Potluck', 'n') : null),
      h('div', z.doing)),
    h('section.zpart', { key: 'facts' }, zoneFacts(n, agent)),
    eventsPart(hist),
    h('div', { key: 'card' }, tile(st, n, refresh)),
  ];
}

// The two actions on a selection: a pause on each zone, or the opposite.
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

const FILTERS = [['use', 'in use'], ['load', 'loading'], ['free', 'free'], ['down', 'down'], ['off', 'off']];
const SORTS = [{ id: 'attention', label: 'needs attention first' }, { id: 'name', label: 'name' }];

registerView({
  id: 'zones', title: 'Zones',
  mount(el, route) {
    let r = route, find = '', side = null, page = 0, hist = null, alive = true;
    const sel = new Set(); // the ticked zones (ids)
    const offStatus = on('status', () => settlePending());
    const offStartups = onStartups(() => { view.refresh(); if (side) side.redraw(); });
    const setQuery = q => navigate('zones', [], Object.assign({}, r.query, q), true);
    const refresh = () => { view.refresh(); if (side) side.redraw(); };

    // the open zone's recent events, read once when its panel opens
    const loadEvents = async id => {
      hist = { id, loading: true, events: [] };
      try {
        const res = await api.get('/api/history', { node: id, type: ZONE_EVENTS.join(','), limit: EVENTS_SHOWN });
        if (hist && hist.id === id) hist = { id, events: res.events || [], off: res.history === 'off' };
      } catch (e) {
        if (hist && hist.id === id) hist = { id, events: [], error: (e && e.message) || 'the request failed' };
      }
      if (alive && side && side.id === id) side.redraw();
    };

    // The side panel of the open zone (?zone=<id>).
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
      const p = sidePanel({ title: nameOf, body: card, onClose: () => { if (side === p) { side = null; setQuery({ zone: '' }); } } });
      p.id = id;
      side = p;
    };

    const view = liveView(el, () => {
      const st = state.status;
      if (side) side.redraw();
      if (!st) return [pageHead('Zones'), h('section.card', skeleton(4))];
      const all = viewNodes(st).map(n => ({ n, agent: agentOf(st, n), z: zoneOf(st, n) }));
      const count = g => all.filter(x => x.z.group === g).length;
      const group = FILTERS.some(([g]) => g === r.query.show) ? r.query.show : '';
      const sort = r.query.sort === 'name' ? 'name' : 'attention';
      const needle = find.trim().toLowerCase();
      let rows = all.filter(x => (!group || x.z.group === group) &&
        (!needle || String(x.n.hostname || '').toLowerCase().includes(needle) || String(x.n.id).toLowerCase().includes(needle)));
      const byName = (a, b) => String(a.n.hostname || a.n.id).localeCompare(String(b.n.hostname || b.n.id));
      rows.sort(sort === 'name' ? byName : (a, b) => (a.z.rank - b.z.rank) || byName(a, b));
      const pages = Math.max(1, Math.ceil(rows.length / PAGE));
      page = Math.min(page, pages - 1);
      const shown = rows.slice(page * PAGE, page * PAGE + PAGE);
      const up = all.filter(x => x.z.group !== 'off' && x.n.online).length;
      const counted = all.filter(x => x.z.group !== 'off').length;
      const pick = id => setQuery({ zone: id });
      // the selection: only zones whose machine has an agent (the actions are agent actions)
      const selectable = x => !!x.agent;
      for (const id of Array.from(sel)) if (!all.some(x => x.n.id === id && selectable(x))) sel.delete(id);
      const canSelect = shown.filter(selectable);
      const allTicked = canSelect.length > 0 && canSelect.every(x => sel.has(x.n.id));
      const toggle = id => { if (sel.has(id)) sel.delete(id); else sel.add(id); view.refresh(); };
      const tickAll = () => { if (allTicked) canSelect.forEach(x => sel.delete(x.n.id)); else canSelect.forEach(x => sel.add(x.n.id)); view.refresh(); };
      const chosen = all.filter(x => sel.has(x.n.id));
      const bulkCount = kind => chosen.filter(x => BULK[kind].applies(x.agent)).length;
      const bulkButton = kind => {
        const c = bulkCount(kind);
        return button(BULK[kind].label + (c && c < chosen.length ? ` (${num(c)})` : ''), () => askBulk(kind, Array.from(sel), done => { done.forEach(id => sel.delete(id)); view.refresh(); }), 'sm',
          { disabled: !c, 'data-bulk': kind, title: c ? `Applies to ${plural(c, 'zone')} of the selection` : kind === 'pause' ? 'Every selected zone is already not accepting models' : 'Every selected zone already accepts models' });
      };
      const selBar = chosen.length ? h('div.selbar', { role: 'region', 'aria-label': 'Selection' },
        h('b', `${num(chosen.length)} selected`), bulkButton('pause'), bulkButton('unpause'),
        button('Clear selection', () => { sel.clear(); view.refresh(); }, 'sm ghost')) : null;
      const tick = x => selectable(x)
        ? h('input', { type: 'checkbox', checked: sel.has(x.n.id), 'aria-label': 'Select ' + (x.n.hostname || x.n.id), 'data-sel': x.n.id,
          onclick: e => { e.stopPropagation(); toggle(x.n.id); }, onkeydown: e => e.stopPropagation() })
        : null;

      const filterBar = h('div.zfilters',
        h('div', chips([{ id: '', label: 'All zones' }].concat(FILTERS.map(([g, word]) => ({ id: g, label: `${num(count(g))} ${word}` }))), group,
          id => { page = 0; setQuery({ show: id }); }, 'Show')),
        h('div', filterSelect('Sort', SORTS, sort, id => setQuery({ sort: id === 'attention' ? '' : id })),
          searchField(find, v => { find = v; page = 0; view.refresh(); }, 'Find a zone', 'zone-search')));

      const table = shown.length ? h('div.scroll', h('table.tbl.zones', { style: 'min-width:720px' },
        h('thead', h('tr', h('th.tick', { scope: 'col' }, canSelect.length ? h('input', { type: 'checkbox', checked: allTicked, 'aria-label': allTicked ? 'Clear the selection of the shown zones' : 'Select every shown zone', 'data-sel': 'all', onclick: tickAll }) : null),
          h('th', { scope: 'col' }, 'Status'), h('th', { scope: 'col' }, 'Zone'), h('th', { scope: 'col' }, 'Doing'),
          h('th.r', { scope: 'col' }, 'Memory'), h('th.r', { scope: 'col' }, 'Temp'))),
        h('tbody', shown.map(x => {
          const { n, agent, z } = x;
          const dev = deviceOf(agent);
          return h('tr', { key: n.id, class: 'click' + (r.query.zone === n.id ? ' sel' : '') + (sel.has(n.id) ? ' ticked' : ''), tabindex: '0', 'aria-label': (n.hostname || n.id) + ', ' + z.label,
            onclick: () => pick(n.id), onkeydown: e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); pick(n.id); } } },
          h('td.tick', tick(x)),
          h('td', zoneStatus(z.square, z.label, z.title)),
          h('td', h('span.zname', n.hostname || n.id), dev ? h('span.zchip', ' | ' + dev) : null, n.local ? h('span.zchip', ' · runs Potluck') : null,
            // no_auto_nodes: automatic placement never picks it (status view: auto false)
            n.auto === false && z.group !== 'off' ? [' ', badge('Named only', 'line', 'In no_auto_nodes: only a profile that names this zone in force_nodes puts a model here')] : null),
          h('td', z.doing),
          h('td.r', { class: z.group === 'off' ? 'r mut' : 'r' }, memoryOf(n)),
          h('td.r', { class: z.group === 'off' ? 'r mut' : 'r' }, tempOf(agent)));
        }))))
        : all.length ? h('div.zempty', 'No zone matches.') : null;

      const pager = pages > 1 ? h('div.row', { style: 'padding:8px 16px 0' },
        button('Previous', () => { page = Math.max(0, page - 1); view.refresh(); }, 'sm', { disabled: page === 0 }),
        h('span.mut', `Page ${page + 1} of ${pages}`),
        button('Next', () => { page = Math.min(pages - 1, page + 1); view.refresh(); }, 'sm', { disabled: page >= pages - 1 })) : null;

      return [
        pageHead('Zones', all.length ? [h('b', num(up)), ' of ', h('b', num(counted)), ' up',
          count('off') ? [' · ', h('b', num(count('off'))), ' off'] : null,
          count('down') ? [' · ', h('b.err-t', num(count('down'))), ' down'] : null] : null),
        all.length
          ? h('section.panel', { 'aria-label': 'Zones' }, filterBar, selBar, table, pager,
            colourKey(`Showing ${num(shown.length)} of ${num(all.length)}`))
          : h('section.card', agentsMode(st)
            ? empty('No machines have joined yet', 'Machines join through their agent. None has registered so far; start the agent on a machine and it appears here.')
            : empty('No zones are known yet', 'Zones come from the fleet feed. None has been reported so far; About shows the connection.')),
      ];
    }, ['status', 'tick', 'conn']);
    syncSide();
    return {
      update(nr) { r = nr; syncSide(); view.refresh(); },
      unmount() { alive = false; offStatus(); offStartups(); if (side) { const s = side; side = null; s.close(); } view.unmount(); },
    };
  },
});
