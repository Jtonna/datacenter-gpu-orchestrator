// Nodes: the machines, as a secondary view. One tile per node: its state, its
// memory, what is on it, and anything wrong with it (health, quarantine,
// recovery). Read-only.
//
// Reads: WebSocket "status" only (view.nodes, instances, quarantines).

import { h } from '../core/dom.js';
import { registerView, href } from '../core/router.js';
import { state } from '../core/store.js';
import { liveView, pageHead, badge, stat, empty, skeleton, memBar, facts, copyButton } from '../core/ui.js';
import { num, mib, gb, gbNum, dur, ago, until, at, plural, toMs, now, shortId, DASH } from '../core/format.js';
import { viewNodes, nodeMemory, staleBanner } from '../core/status.js';

const CHANNEL = { ok: ['ok', 'ok-t'], silent: ['silent', 'err-t'], unknown: ['not known', 'mut'] };
const ACTION = { restart_rpc_server: 'restart the worker', restart_docker: 'restart Docker', reboot: 'reboot the machine' };

// stateOf(node) -> [label, badge kind, sort rank]
function stateOf(st, n, inst) {
  const q = (st.quarantines || []).find(x => x.node === n.id);
  if (n.excluded) return ['Excluded', 'dash', 9];
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

function tile(st, n) {
  const name = n.hostname || n.id;
  const inst = n.holder ? (st.instances || []).find(i => i.profile === n.holder) : null;
  const placed = inst ? (inst.nodes || []).find(p => p.id === n.id) : null;
  const mem = placed ? nodeMemory(placed) : null;
  const q = (st.quarantines || []).find(x => x.node === n.id);
  const [label, kind] = stateOf(st, n, inst);
  const usableNode = n.online && !n.excluded;
  // The figures come from two places (the fleet's total, a free figure that a
  // pre-load check may have measured later), so they can disagree. Nothing
  // negative is ever shown: "in use" is clamped at 0 and the note says why.
  const total = n.total_mib || 0;
  const freeRaw = Math.max(0, n.free_mib || 0);
  const free = Math.min(freeRaw, total);
  const disagree = total > 0 && freeRaw > total;
  const used = Math.max(0, total - free);
  const model = mem ? Math.min(used, mem.total_mib) : 0;
  const other = Math.max(0, used - model);
  const usable = Math.min(Math.max(0, n.live_budget_mib || 0), free);
  const headroom = Math.max(0, free - usable);
  const notes = [];
  const note = (cls, node) => notes.push(h('div.hit', h('i', { class: cls }), h('span', node)));

  if (n.excluded) note('', 'Excluded in the configuration: it gets no model and is not counted in the capacity.');
  if (!n.online && !n.excluded) note('x', ['Offline', n.missing_since ? [': missing from the fleet feed since ', at(n.missing_since), ' (', ago(n.missing_since), ')'] : '.']);
  else if (n.missing_since) note('a', ['Missing from the fleet feed since ', at(n.missing_since), '.']);
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
    note('', [overridden ? 'The last pre-load check measured ' : 'The fleet reports ', h('b', gb(freeRaw)), ' free, more than the ', gb(total),
      ' it reports in total. Free is shown as the total and in use as 0', overridden ? ['; the measured figure is used ', until(n.override_until)] : '', '.']);
  } else if (overridden) {
    note('', ['The last pre-load check measured ', h('b', gb(n.override_free_mib)), ' free; that figure is used ', until(n.override_until), '.']);
  }

  const version = n.rpc_server_version || '';
  const onIt = n.holder ? h('span', inst && inst.state === 'starting' ? 'Loading ' : 'Holds ', h('a.mono', { href: href('models', inst && inst.model_file ? [inst.model_file] : null), title: n.holder }, n.holder),
    inst && inst.in_flight > 0 ? h('span.mut', ` · ${plural(inst.in_flight, 'request')} in flight`) : null)
    : n.claimed_by ? h('span', 'Claimed for ', h('a.mono', { href: href('queue') }, n.claimed_by))
      : h('span.mut', n.excluded ? 'Never used' : !n.online ? 'Nothing can be placed here' : 'Nothing loaded');
  return h('section', { class: 'card ncard ' + (n.excluded ? 'quiet ' : '') + (kind === 'fail' || kind === 'aside' ? 'flag-err' : kind === 'load' && !inst ? 'flag-warn' : ''), key: 'node-' + n.id, 'aria-label': name },
    h('div.ch', { style: 'padding-bottom:10px' },
      h('h2.mono', name), badge(label, kind),
      n.local ? badge('This machine', 'n', 'The scheduler runs here: models are read from disk, nothing is uploaded') : null),
    h('div.body',
      h('div.on-it', onIt),
      total ? h('div', { style: 'display:flex;flex-direction:column;gap:3px' },
        memBar({ model, other, total, limit: usableNode && headroom > 0 ? used + usable : null },
          `${name}: ${gb(used)} in use of ${gb(total)}` + (usableNode ? `, ${gb(usable)} usable for a new model` : '')),
        h('div.memline', h('span', h('b', gbNum(used)), ' GB in use', model > 0 && other > 0 ? ` · ${gbNum(model)} by the model` : ''), h('span', gbNum(total), ' GB total'))) : null,
      n.excluded
        ? h('div.trio', stat('Free', gbNum(free), 'GB', { small: true, title: mib(free) + ', as the fleet reports it' }),
          stat('Total', gbNum(total), 'GB', { small: true, title: mib(total) }))
        : h('div.trio',
          stat('Usable now', usableNode ? gbNum(usable) : DASH, usableNode ? 'GB' : null, { small: true, title: usableNode ? `${mib(usable)}: the free memory minus the headroom of its tier; what a new model may take right now` : 'Nothing can be placed here now' }),
          stat('Free', gbNum(free), 'GB', { small: true, title: `${mib(free)}, as the fleet reports it` + (headroom ? `; ${mib(headroom)} of it is kept as headroom` : '') }),
          stat('When empty', n.plan_budget_mib ? gbNum(n.plan_budget_mib) : DASH, n.plan_budget_mib ? 'GB' : null, { small: true, title: n.plan_budget_mib ? `${mib(n.plan_budget_mib)}: what the scheduler plans with once nothing is loaded here` : null })),
      notes.length ? h('div', { style: 'display:flex;flex-direction:column;gap:8px' }, h('div.note-s', { style: 'display:flex;flex-direction:column;gap:8px' }, notes)) : null),
    h('div.foot',
      facts([
        ['Address', h('span', n.addr || (n.local ? 'this machine' : 'not known')), n.addr ? 'The address of its worker' : null],
        ['Priority', h('span', n.priority ? String(n.priority) : 'no tier matches', n.priority ? h('span.mut', { style: 'font-family:var(--sans);font-weight:600' }, n.priority === 1 ? ' · used first' : '') : null), 'Lower numbers are used first when a model is placed'],
        n.health && n.health.channels && !n.excluded ? ['Health', h('span', { style: 'font-family:var(--sans);font-weight:600' }, ['upload', 'heartbeat', 'probe'].filter(c => n.health.channels[c] && n.health.channels[c] !== 'unknown').map((c, i) => {
          const [txt, cls] = CHANNEL[n.health.channels[c]] || [n.health.channels[c], 'mut'];
          return [i ? ' · ' : '', c + ' ', h('span', { class: cls }, txt)];
        }), ['upload', 'heartbeat', 'probe'].every(c => !n.health.channels[c] || n.health.channels[c] === 'unknown') ? h('span.mut', 'no report yet') : null), 'The channels the scheduler watches the worker on'] : null,
        version ? ['Worker', [h('span', { title: version }, shortId(version, 12)), copyButton(version, 'the worker version')], 'The version of its rpc-server: ' + version] : null,
        n.rpc_server_mem_mib ? ['Worker memory', h('span', mib(n.rpc_server_mem_mib)), 'Memory the worker process itself holds'] : null,
        ['Node id', h('span', n.id)],
      ])));
}

registerView({
  id: 'nodes', title: 'Nodes',
  mount(el) {
    const view = liveView(el, () => {
      const st = state.status;
      if (!st) return [pageHead('Nodes'), staleBanner(), h('section.card', skeleton(4))];
      const nodes = viewNodes(st).slice().sort((a, b) => (a.excluded - b.excluded) || ((a.priority || 99) - (b.priority || 99)) || String(a.hostname || a.id).localeCompare(String(b.hostname || b.id)));
      const pool = nodes.filter(n => !n.excluded);
      const online = pool.filter(n => n.online).length;
      const bad = pool.filter(n => !n.online || n.quarantined || (st.quarantines || []).some(q => q.node === n.id) || (n.health && n.health.state && n.health.state !== 'ok')).length;
      const excluded = nodes.length - pool.length;
      const usable = pool.filter(n => n.online).reduce((s, n) => s + Math.max(0, n.live_budget_mib || 0), 0);
      return [
        pageHead('Nodes', [h('b', num(online)), ' of ', h('b', num(pool.length)), ' online', bad ? [' · ', h('b.err-t', num(bad)), ' need a look'] : null,
          excluded ? [' · ', h('b', num(excluded)), ' excluded'] : null, ' · ', h('b', gb(usable)), ' usable now'],
          h('div.legend', h('span', h('span.sw.m'), 'Model'), h('span', h('span.sw.o'), 'Other use'), h('span', h('span.sw.free'), 'Free'), h('span', h('span.sw.lim'), 'Limit for a new model'))),
        staleBanner(),
        nodes.length ? h('div.ngrid', nodes.map(n => tile(st, n)))
          : h('section.card', empty('No nodes are known yet', 'Nodes come from the fleet feed. None has been reported so far; check that the fleet manager is reachable (About shows the connection).')),
      ];
    }, ['status', 'tick', 'conn']);
    return view;
  },
});
