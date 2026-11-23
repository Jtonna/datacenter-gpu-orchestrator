// Nodes: the machines, as a secondary view. One tile per node: its state, its
// memory, what is on it, and anything wrong with it (health, quarantine,
// recovery).
//
// Reads: WebSocket "status" (view.nodes, instances, quarantines, and in
// agents mode `membership`, `agents` and each node's `unplaced` and `agent`,
// with its `last_startup`; the reservation figures of each view node,
// core/reserve.js). GET /api/startups only when someone opens the
// marks of a start (core/startup.js).
//
// Writes, in agents mode only, and only these two per machine (no load and
// no ordinary unload): Pause / Unpause and Force unload, each with a required
// comment (the requests are built in core/nodeactions.js).
// Nothing is shown as done before the next status says so; a card only shows
// that its request is on the way.

import { h } from '../core/dom.js';
import { registerView, href } from '../core/router.js';
import { state, on } from '../core/store.js';
import * as api from '../core/api.js';
import { liveView, pageHead, badge, button, dialog, toast, stat, empty, skeleton, memBar, facts, copyButton, more } from '../core/ui.js';
import { num, mib, gb, gbNum, dur, ago, until, at, plural, toMs, now, shortId, dateTime, DASH } from '../core/format.js';
import { viewNodes, nodeMemory, staleBanner } from '../core/status.js';
import { logsLink, nodeQuery } from '../core/logs.js';
import { agentsMode, agentOf, notPlaceable, holdsSomething, pauseAction, sendNodeAction, NODE_ACTIONS } from '../core/nodeactions.js';
import { poolText } from '../core/words.js';
import { reservedPart } from '../core/reserve.js';
import { startupText, markLabel, secs, requestedText, NOT_INCLUDED, startups, loadStartups, onStartups, lastRecord } from '../core/startup.js';

const CHANNEL = { ok: ['ok', 'ok-t'], silent: ['silent', 'err-t'], unknown: ['not known', 'mut'] };
const ACTION = { restart_rpc_server: 'restart the worker', restart_docker: 'restart Docker', reboot: 'reboot the machine' };

// cachedMiBOf(agent) -> the page cache in MiB its agent last reported, or null
// when unknown (no agent, no stats, or an agent too old to send it).
export function cachedMiBOf(agent) {
  const m = agent && agent.stats && agent.stats.memory;
  return m && typeof m.cached_bytes === 'number' ? Math.round(m.cached_bytes / 1048576) : null;
}

// excludedOf(node, agent): exclude_nodes names the machine (the view's
// excluded, or its agent facts' excluded).
const excludedOf = (n, a) => !!(n.excluded || (a && a.excluded));

// stateOf(node) -> [label, badge kind, sort rank]
function stateOf(st, n, inst, agent) {
  const q = (st.quarantines || []).find(x => x.node === n.id);
  if (excludedOf(n, agent)) return [n.holder || (agent && agent.running_while_excluded) ? 'Excluded, busy' : 'Excluded', 'dash', 9];
  if (!n.online) return ['Offline', 'n', 8];
  if (n.health && n.health.state === 'hung') return ['Not answering', 'fail', 0];
  if (q || n.quarantined) return ['Set aside', 'aside', 1];
  if (n.health && n.health.state === 'suspect') return ['Slow to answer', 'load', 2];
  if (inst && inst.state === 'starting') return ['Loading', 'load', 3];
  if (n.holder) return ['In use', 'use', 4];
  if (n.claimed_by) return ['Claimed', 'q', 5];
  if (n.settling) return ['Settling', 'settle', 6];
  return ['Free', 'line', 7];
}

// The requests on their way, by node id: {action, sent}. An entry stays after
// the server said yes (sent) until the next status arrives, so the card shows
// "Pausing…" until the page really knows the new state.
const pending = new Map();

// settlePending() forgets the requests the server accepted, once a new status
// is there to show their outcome. A page that draws tile() calls it on every
// status (the Nodes page below does the same inline).
export function settlePending() { for (const [id, p] of pending) if (p.sent) pending.delete(id); }

// askAction(id, action, refresh) opens the dialog of one action on one machine:
// what will happen, the required comment, then the request. A refusal of the
// server is shown in the dialog, in the server's words.
function askAction(id, action, refresh) {
  const st = state.status;
  const n = viewNodes(st).find(x => x.id === id);
  if (!n || pending.has(id)) return null;
  const name = n.hostname || n.id;
  const act = NODE_ACTIONS[action];
  const force = action === 'force';
  const inst = n.holder ? (st.instances || []).find(i => i.profile === n.holder) : null;
  const hits = force ? [
    { kind: 'x', text: n.holder ? ['It stops ', h('span.mono', n.holder), ' at once, on every machine that model is spread over.'] : 'It stops the worker that runs on this machine at once.' },
    { kind: 'x', text: inst && inst.in_flight > 0 ? `${plural(inst.in_flight, 'request')} in flight and every request waiting on that model will fail.` : 'Requests waiting on that model will fail.' },
    { kind: '', text: 'The machine itself stays in the pool. Pause it if it should not get a new model.' },
  ] : action === 'pause' ? [
    { kind: 'a', text: 'It takes no new model until someone unpauses it.' },
    { kind: '', text: n.holder ? ['It keeps ', h('span.mono', n.holder), ', which goes on serving requests.'] : 'Nothing is loaded on it, so nothing is interrupted.' },
  ] : [
    { kind: 'm', text: 'It may take a new model again, as soon as nothing else holds it back.' },
  ];
  return dialog({
    title: [act.label + ' ', h('span.mono', name), '?'],
    body: close => {
      const comment = h('textarea.textarea', { rows: 2, maxlength: 400, placeholder: 'Why? It is kept in the audit log.', 'aria-label': 'Comment' });
      const err = h('span.err', { role: 'alert' });
      const go = button(act.label, null, 'lg ' + (force ? 'dng solid' : 'pri'));
      const cancel = button('Cancel', () => close(false), 'lg');
      go.addEventListener('click', async () => {
        const c = comment.value.trim();
        if (!c) { err.textContent = 'A comment is required. Say why, in a few words.'; comment.focus(); return; }
        err.textContent = '';
        go.disabled = cancel.disabled = comment.disabled = true;
        go.textContent = act.busy + '…';
        pending.set(id, { action, sent: false });
        refresh();
        try {
          await sendNodeAction(api, id, action, c);
          pending.set(id, { action, sent: true });
          toast(`${act.done}: ${name}`, 'ok');
          close(true);
        } catch (e) {
          pending.delete(id);
          const msg = (e && e.message) || 'The request failed.';
          err.textContent = msg;
          if (!err.isConnected) toast(`${act.label} ${name}: ${msg}`, 'err'); // the dialog was closed meanwhile
          go.disabled = cancel.disabled = comment.disabled = false;
          go.textContent = act.label;
        }
        refresh();
      });
      return h('div.col', { style: 'gap:14px' },
        h('div', { style: 'display:flex;flex-direction:column;gap:8px' }, hits.map(x => h('div.hit', h('i', { class: x.kind }), h('span', x.text)))),
        h('div.field', h('label', 'Comment (required)'), comment, err),
        h('div.dlg-foot', cancel, go));
    },
  });
}

// controls(n, agent, refresh) -> the two buttons of an agent machine.
function controls(n, agent, refresh) {
  const p = pending.get(n.id);
  const pa = pauseAction(agent);
  const holds = holdsSomething(n, agent);
  const label = a => (p && p.action === a ? NODE_ACTIONS[a].busy + '…' : NODE_ACTIONS[a].label);
  return h('div.acts', { 'aria-busy': p ? 'true' : null },
    button(label(pa), () => askAction(n.id, pa, refresh), 'sm', { disabled: !!p,
      title: pa === 'pause' ? 'It keeps what it holds and takes no new model' : 'It may take a new model again' }),
    button(label('force'), () => askAction(n.id, 'force', refresh), 'sm dng', { disabled: !!p || !holds,
      title: holds ? 'Stops the model on this machine at once; requests waiting on it fail' : 'Nothing runs on this machine, so there is nothing to unload' }));
}

// The nodes whose start-up marks are open.
const marksOpen = new Set();

// startupPart(n, last, refresh) -> the line of a machine's last start, and
// its marks in time order once opened.
function startupPart(n, last, refresh) {
  const t = startupText(last);
  const open = marksOpen.has(n.id);
  const rec = open ? lastRecord(n.id, last.kind) : null;
  const dot = last.headline_s != null || last.kind === 'agent_restart' ? 'm' : last.open ? '' : 'a';
  const toggle = () => {
    if (open) marksOpen.delete(n.id); else { marksOpen.add(n.id); loadStartups(api); }
    refresh();
  };
  let marks = null;
  if (open) {
    if (startups.error && !rec) marks = h('span.err-t', 'The marks could not be read: ' + (startups.error.message || 'the request failed') + '.');
    else if (!rec) marks = h('span.mut', 'Reading the marks…');
    else {
      const r = rec.record || {};
      marks = h('div', { style: 'display:flex;flex-direction:column;gap:3px' },
        h('span.mut', 'Seconds since its kernel started' + (r.userspace_start_s != null ? `; userspace started at ${secs(r.userspace_start_s)}` : '') + '.'),
        (rec.marks_in_order || []).map(m => h('div.memline', { key: m.name },
          h('span', { title: m.name }, markLabel(m.name), m.external ? h('span.mut', ' · from the console side') : null),
          h('span.mono', secs(m.s)))),
        requestedText(r) ? h('span', requestedText(r)) : null);
    }
  }
  return h('div', { style: 'display:flex;flex-direction:column;gap:6px', key: 'startup' },
    h('div.hit', h('i', { class: dot }), h('span', h('b', t.head), t.kind ? ` (${t.kind})` : '', ', ', ago(last.at), '.', t.extra ? ' ' + t.extra : '',
      ' ', more(open, toggle, 'Marks', 'Hide marks'))),
    marks,
    open ? h('span.mut', NOT_INCLUDED) : null);
}

// tile(st, n, refresh) -> one machine's card. Exported for the new shell's
// Zones page, which shows it in the side panel of a selected zone.
export function tile(st, n, refresh) {
  const name = n.hostname || n.id;
  const agent = agentOf(st, n);
  const np = notPlaceable(n, agent);
  // Excluded (exclude_nodes): the view says so, and so do the agent facts, with the entry.
  const excl = excludedOf(n, agent);
  const exBy = n.excluded_by || (agent && agent.excluded_by) || '';
  const npOther = np && np.code !== 'paused' && np.code !== 'excluded'; // a reason other than a pause or the exclusion
  const inst = n.holder ? (st.instances || []).find(i => i.profile === n.holder) : null;
  const placed = inst ? (inst.nodes || []).find(p => p.id === n.id) : null;
  const mem = placed ? nodeMemory(placed) : null;
  const q = (st.quarantines || []).find(x => x.node === n.id);
  const [label, kind] = stateOf(st, n, inst, agent);
  const usableNode = n.online && !excl && !np; // a paused or not placeable machine takes nothing now
  // The figures come from two places (the fleet's total, a free figure that a
  // pre-load check may have measured later), so they can disagree. Nothing
  // negative is ever shown: "in use" is clamped at 0 and the note says why.
  const total = n.total_mib || 0;
  const freeRaw = Math.max(0, n.free_mib || 0);
  const free = Math.min(freeRaw, total);
  const disagree = total > 0 && freeRaw > total;
  // what the model placed here is known to hold counts as used even when the free figure says otherwise
  const model = mem ? Math.min(total, mem.total_mib) : 0;
  const used = Math.max(0, total - free, model);
  const other = Math.max(0, used - model);
  const usable = Math.min(Math.max(0, n.live_budget_mib || 0), free);
  const headroom = Math.max(0, free - usable);
  // Where the figures come from, in the words of the membership; the scheduler's
  // own machine measures itself (local_probe_cmd) in both.
  const pool = agentsMode(st);
  const reports = n.local && pool ? 'This machine reports ' : poolText(pool, 'The fleet reports ');
  const asReported = n.local && pool ? ', as this machine reports it' : poolText(pool, ', as the fleet reports it');
  const notes = [];
  const note = (cls, node) => notes.push(h('div.hit', h('i', { class: cls }), h('span', node)));

  // Excluded while a model runs on it (0.1.7): the exclusion stops new
  // placements only; the running instance is not drained.
  const runningX = excl ? ((agent && agent.running_while_excluded) || (n.holder ? { profile: n.holder, state: inst && inst.state } : null)) : null;
  if (excl) note('', ['Excluded in the configuration', exBy ? [' (exclude_nodes: ', h('span.mono', exBy), ')'] : '',
    runningX ? ': it gets no new model and is not counted in the capacity.' : ': it gets no model and is not counted in the capacity.']);
  if (runningX) note('a', [h('b', 'Excluded, but still busy. '), h('span.mono', runningX.profile), ' was placed here before the exclusion',
    runningX.since ? [' (', runningX.state === 'starting' ? 'loading since ' : 'loaded since ', dateTime(runningX.since), ')'] : '',
    ': it keeps running and serving until it is unloaded. Unload it to empty the machine.']);
  // What the agent can do beyond the basics (agent contract 6.13), in plain words.
  if (agent && Array.isArray(agent.capabilities)) {
    note('', agent.capabilities.length ? ['Its agent can also: ', h('span.mono', agent.capabilities.join(', ')), '.'] : 'Its agent advertises no extra capabilities.');
  }
  if (np && np.paused) note('', 'Paused by an operator: it keeps what it holds and takes no new model until it is unpaused.');
  if (npOther) {
    note('a', h('span', { title: np.code ? 'The scheduler says: ' + np.code : null }, h('b', 'Not placeable. '),
      np.known ? np.text : ['The scheduler gives this reason: ', h('span.mono', np.code)],
      agent.programs_error ? [' The agent says: ', agent.programs_error] : null));
  }
  const gone = agent ? 'its agent has not reported since ' : poolText(pool, 'missing from the fleet feed since ');
  if (!n.online && !excl) note('x', ['Offline', n.missing_since ? [': ', gone, at(n.missing_since), ' (', ago(n.missing_since), ')'] : '.']);
  else if (n.missing_since) note('a', [agent ? 'Its agent has not reported since ' : poolText(pool, 'Missing from the fleet feed since '), at(n.missing_since), '.']);
  if (q || n.quarantined) {
    note('x', ['Set aside ', q ? ['until ', at(q.until), ' (', until(q.until), ')'] : ['since ', at(n.quarantined)],
      q && q.level > 1 ? ` after ${plural(q.level, 'failed check')} in a row` : ' after a failed pre-load check or a worker that stopped answering', '. It gets no new model until then.']);
  }
  if (n.health && n.health.state && n.health.state !== 'ok') {
    const silent = n.health.silent_for_s || {};
    const parts = Object.keys(silent).map(k => `${k} silent for ${dur(Math.round(silent[k]))}`);
    note(n.health.state === 'hung' ? 'x' : 'a', [n.health.state === 'hung' ? 'The worker stopped answering' : 'The worker is slow to answer',
      n.health.since ? [' since ', at(n.health.since)] : null, parts.length ? ': ' + parts.join(', ') : '', '.']);
  }
  const rec = n.recovery;
  if (rec && rec.last_request) {
    const r = rec.last_request;
    const word = { pending: 'waiting for the fleet manager', stored: 'accepted by the fleet manager', refused: 'refused by the fleet manager', error: 'could not be delivered' }[r.status] || r.status;
    note(r.status === 'refused' || r.status === 'error' ? 'x' : 'a', ['Recovery: asked to ', ACTION[r.action] || r.action, ' ', ago(r.sent_at), ', ', word,
      rec.step ? ` (step ${rec.step})` : '', '.']);
  }
  if (rec && rec.last_outcome) {
    const o = rec.last_outcome;
    note(o.outcome === 'ok' || o.outcome === 'done' || o.outcome === 'success' ? 'm' : 'a', ['The node reported: ', ACTION[o.action] || o.action, ' ', o.outcome, ' ', ago(o.at),
      o.duration_ms ? ` (took ${dur(o.duration_ms / 1000)})` : '', o.reason ? ': ' + o.reason : '', '.']);
  }
  if (n.settling) {
    const txt = { settling: 'Settling after an unload', fetching: 'Reading its free memory after an unload', 'fetch-failed': 'Its free memory could not be read after an unload' }[n.settling] || n.settling;
    note(n.settling === 'fetch-failed' ? 'a' : '', [txt, n.settle_until ? [', ', until(n.settle_until)] : '', '.']);
  }
  const overridden = toMs(n.override_until) && toMs(n.override_until) > now();
  if (disagree) {
    note('', [overridden ? 'The last pre-load check measured ' : reports, h('b', gb(freeRaw)), ' free, more than the ', gb(total),
      ' it reports in total, so free is shown as the total', overridden ? ['; the measured figure is used ', until(n.override_until)] : '', '.']);
  } else if (overridden) {
    note('', ['The last pre-load check measured ', h('b', gb(n.override_free_mib)), ' free; that figure is used ', until(n.override_until), '.']);
  }

  // the page cache its agent reports (stats.memory.cached_bytes; an older agent sends none)
  const cachedMiB = cachedMiBOf(agent);
  const version = n.rpc_server_version || '';
  const onIt = n.holder ? h('span', inst && inst.state === 'starting' ? 'Loading ' : 'Holds ', h('a.mono', { href: href('models', inst && inst.model_file ? [inst.model_file] : null), title: n.holder }, n.holder),
    inst && inst.in_flight > 0 ? h('span.mut', ` · ${plural(inst.in_flight, 'request')} in flight`) : null)
    : n.claimed_by ? h('span', 'Claimed for ', h('a.mono', { href: href('queue') }, n.claimed_by))
      : h('span.mut', excl ? 'Never used' : !n.online ? 'Nothing can be placed here' : 'Nothing loaded');
  return h('section', { class: 'card ncard ' + (excl ? 'quiet ' : '') + (kind === 'fail' || kind === 'aside' ? 'flag-err' : kind === 'load' && !inst ? 'flag-warn' : npOther ? 'flag-warn' : ''), key: 'node-' + n.id, 'aria-label': name },
    h('div.ch', { style: 'padding-bottom:10px' },
      h('h2.mono', name), np && label === 'Free' ? null : badge(label, kind, label.startsWith('Excluded') && exBy ? 'excluded by exclude_nodes: ' + exBy : undefined),
      np && np.paused ? badge('Paused', 'settle', 'Paused by an operator: it takes no new model') : null,
      npOther ? badge('Not placeable', 'aside', np.known ? np.short : np.code) : null,
      n.auto === false && !excl ? badge('Forced only', 'line', 'In no_auto_nodes: only a profile that names this node in force_nodes puts a model here') : null,
      n.local ? badge('This machine', 'n', 'The scheduler runs here: models are read from disk, nothing is uploaded') : null),
    h('div.body',
      h('div.on-it', onIt),
      total ? h('div', { style: 'display:flex;flex-direction:column;gap:3px' },
        memBar({ model, other, total, limit: usableNode && headroom > 0 ? used + usable : null },
          `${name}: ${gb(used)} in use of ${gb(total)}` + (usableNode ? `, ${gb(usable)} usable for a new model` : '')),
        h('div.memline', h('span', h('b', gbNum(used)), ' GB in use', model > 0 && other > 0 ? ` · ${gbNum(model)} by the model` : ''), h('span', gbNum(total), ' GB total'))) : null,
      excl
        ? h('div.trio', stat('Free', gbNum(free), 'GB', { small: true, title: mib(free) + asReported }),
          stat('Total', gbNum(total), 'GB', { small: true, title: mib(total) }))
        : h('div.trio',
          stat('Usable now', usableNode ? gbNum(usable) : DASH, usableNode ? 'GB' : null, { small: true, title: usableNode ? `${mib(usable)}: the free memory minus the headroom of its tier; what a new model may take right now` : 'Nothing can be placed here now' }),
          stat('Free', gbNum(free), 'GB', { small: true, title: mib(free) + asReported + (headroom ? `; ${mib(headroom)} of it is kept as headroom` : '') }),
          stat('When empty', n.plan_budget_mib ? gbNum(n.plan_budget_mib) : DASH, n.plan_budget_mib ? 'GB' : null, { small: true, title: n.plan_budget_mib ? `${mib(n.plan_budget_mib)}: what the scheduler plans with once nothing is loaded here` : null })),
      n.excluded ? null : reservedPart(n),
      notes.length ? h('div', { style: 'display:flex;flex-direction:column;gap:8px' }, h('div.note-s', { style: 'display:flex;flex-direction:column;gap:8px' }, notes)) : null,
      agent && agent.last_startup ? startupPart(n, agent.last_startup, refresh) : null),
    agent ? controls(n, agent, refresh) : null,
    h('div.foot',
      facts([
        ['Address', h('span', n.addr || (n.local ? 'this machine' : 'not known')), n.addr ? 'The address of its worker' : null],
        ['Priority', h('span', n.priority ? String(n.priority) : 'no tier matches', n.priority ? h('span.mut', { style: 'font-family:var(--sans);font-weight:600' }, n.priority === 1 ? ' · used first' : '') : null), 'Lower numbers are used first when a model is placed'],
        n.health && n.health.channels && !excl ? ['Health', h('span', { style: 'font-family:var(--sans);font-weight:600' }, ['upload', 'heartbeat', 'probe'].filter(c => n.health.channels[c] && n.health.channels[c] !== 'unknown').map((c, i) => {
          const [txt, cls] = CHANNEL[n.health.channels[c]] || [n.health.channels[c], 'mut'];
          return [i ? ' · ' : '', c + ' ', h('span', { class: cls }, txt)];
        }), ['upload', 'heartbeat', 'probe'].every(c => !n.health.channels[c] || n.health.channels[c] === 'unknown') ? h('span.mut', 'no report yet') : null), 'The channels the scheduler watches the worker on'] : null,
        version ? ['Worker', [h('span', { title: version }, shortId(version, 12)), copyButton(version, 'the worker version')], 'The version of its rpc-server: ' + version] : null,
        cachedMiB != null ? ['File cache', h('span', gb(cachedMiB)), `${mib(cachedMiB)} of memory holds cached files, such as a model file it read. On a machine whose model shares that memory, a reload is fast only while the file stays cached`] : null,
        n.rpc_server_mem_mib ? ['Worker memory', h('span', mib(n.rpc_server_mem_mib)), 'Memory the worker process itself holds'] : null,
        ['Node id', h('span', n.id, logsLink(state.status, ' Logs', nodeQuery(n.id), null, null, 'Open the last hour of this node\'s log lines in a new tab'))],
      ])));
}

registerView({
  id: 'nodes', title: 'Nodes',
  mount(el) {
    // a request the server accepted stops being "on the way" with the next status
    const off = on('status', () => { for (const [id, p] of pending) if (p.sent) pending.delete(id); });
    const offStartups = onStartups(() => view.refresh());
    const refresh = () => view.refresh();
    const view = liveView(el, () => {
      const st = state.status;
      const agents = agentsMode(st);
      if (!st) return [pageHead('Nodes'), staleBanner(), h('section.card', skeleton(4))];
      const nodes = viewNodes(st).slice().sort((a, b) => (a.excluded - b.excluded) || ((a.priority || 99) - (b.priority || 99)) || String(a.hostname || a.id).localeCompare(String(b.hostname || b.id)));
      const pool = nodes.filter(n => !n.excluded);
      const online = pool.filter(n => n.online).length;
      // agents mode: a machine that takes no new model for a reason other than an operator's pause needs a look
      const unplaced = n => { const np = notPlaceable(n, agentOf(st, n)); return !!np && np.code !== 'paused' && np.code !== 'excluded'; };
      const paused = agents ? pool.filter(n => { const a = agentOf(st, n); return a && a.paused; }).length : 0;
      const bad = pool.filter(n => !n.online || n.quarantined || (st.quarantines || []).some(q => q.node === n.id) || (n.health && n.health.state && n.health.state !== 'ok') || unplaced(n)).length;
      const excluded = nodes.length - pool.length;
      const usable = pool.filter(n => n.online && !notPlaceable(n, agentOf(st, n))).reduce((s, n) => s + Math.max(0, n.live_budget_mib || 0), 0);
      return [
        pageHead('Nodes', [h('b', num(online)), ' of ', h('b', num(pool.length)), ' online', bad ? [' · ', h('b.err-t', num(bad)), ' need a look'] : null,
          excluded ? [' · ', h('b', num(excluded)), ' excluded'] : null, paused ? [' · ', h('b', num(paused)), ' paused'] : null, ' · ', h('b', gb(usable)), ' usable now'],
          h('div.legend', h('span', h('span.sw.m'), 'Model'), h('span', h('span.sw.o'), 'Other use'), h('span', h('span.sw.free'), 'Free'), h('span', h('span.sw.lim'), 'Limit for a new model'))),
        staleBanner(),
        nodes.length ? h('div.ngrid', nodes.map(n => tile(st, n, refresh)))
          : h('section.card', agents
            ? empty('No machines have joined yet', 'Machines join through their agent. None has registered so far; start the agent on a machine and it appears here.')
            : empty('No nodes are known yet', 'Nodes come from the fleet feed. None has been reported so far; check that the fleet manager is reachable (About shows the connection).')),
      ];
    }, ['status', 'tick', 'conn']);
    return { refresh, unmount() { off(); offStartups(); view.unmount(); } };
  },
});
