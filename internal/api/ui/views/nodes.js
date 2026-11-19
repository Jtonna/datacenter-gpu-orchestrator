// Nodes: the machines, as a secondary view. One tile per node: its state, its
// memory, what is on it, and anything wrong with it (health, quarantine,
// recovery). Read-only.
//
// Reads: WebSocket "status" only (view.nodes, instances, quarantines).

import { h } from '../core/dom.js';
import { registerView, href } from '../core/router.js';
import { state } from '../core/store.js';
import { liveView, pageHead, badge, bar, stat, legend, empty, skeleton } from '../core/ui.js';
import { num, mib, dur, ago, until, at, plural, toMs, now, DASH } from '../core/format.js';
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
  const total = n.total_mib || 0, free = Math.max(0, n.free_mib || 0);
  const model = mem ? Math.min(total, mem.total_mib) : 0;
  const other = Math.max(0, total - free - model);
  const usable = Math.max(0, n.live_budget_mib || 0);
  const headroom = Math.max(0, free - usable);
  const facts = [];
  const fact = (cls, node) => facts.push(h('div.hit', h('i', { class: cls }), h('span', node)));

  if (n.excluded) fact('', 'Left out by the configuration (exclude_nodes). It gets no model and is not counted in capacity.');
  if (!n.online && !n.excluded) fact('x', ['Offline', n.missing_since ? [': missing from the fleet feed since ', at(n.missing_since), ' (', ago(n.missing_since), ')'] : '.']);
  else if (n.missing_since) fact('a', ['Missing from the fleet feed since ', at(n.missing_since), '.']);
  if (q || n.quarantined) {
    fact('x', ['Set aside ', q ? ['until ', at(q.until), ' (', until(q.until), ')'] : ['since ', at(n.quarantined)],
      q && q.level > 1 ? ` after ${plural(q.level, 'failed check')} in a row` : ' after a failed pre-load check or a hung worker', '. It gets no new model until then.']);
  }
  if (n.health && n.health.state && n.health.state !== 'ok') {
    const silent = n.health.silent_for_s || {};
    const parts = Object.keys(silent).map(k => `${k} silent for ${dur(Math.round(silent[k]))}`);
    fact(n.health.state === 'hung' ? 'x' : 'a', [n.health.state === 'hung' ? 'The worker stopped answering' : 'The worker is slow to answer',
      n.health.since ? [' since ', at(n.health.since)] : null, parts.length ? ': ' + parts.join(', ') : '', '.']);
  }
  const rec = n.recovery;
  if (rec && rec.last_request) {
    const r = rec.last_request;
    const word = { pending: 'waiting for the fleet manager', stored: 'accepted by the fleet manager', refused: 'refused by the fleet manager', error: 'could not be delivered' }[r.status] || r.status;
    fact(r.status === 'refused' || r.status === 'error' ? 'x' : 'a', ['Recovery: asked to ', ACTION[r.action] || r.action, ' ', ago(r.sent_at), ', ', word,
      rec.step ? ` (step ${rec.step})` : '', '.']);
  }
  if (rec && rec.last_outcome) {
    const o = rec.last_outcome;
    fact(o.outcome === 'ok' || o.outcome === 'done' || o.outcome === 'success' ? 'm' : 'a', ['The node reported: ', ACTION[o.action] || o.action, ' ', o.outcome, ' ', ago(o.at),
      o.duration_ms ? ` (took ${dur(o.duration_ms / 1000)})` : '', o.reason ? ': ' + o.reason : '', '.']);
  }
  if (n.settling) {
    const txt = { settling: 'Settling after an unload', fetching: 'Reading its free memory after an unload', 'fetch-failed': 'Its free memory could not be read after an unload' }[n.settling] || n.settling;
    fact(n.settling === 'fetch-failed' ? 'a' : '', [txt, n.settle_until ? [', ', until(n.settle_until)] : '', '.']);
  }
  if (toMs(n.override_until) && toMs(n.override_until) > now()) {
    fact('', ['The pre-load check measured ', h('span.mono', mib(n.override_free_mib)), ' free; that figure is used ', until(n.override_until), '.']);
  }

  const dim = n.excluded || !n.online;
  return h('section', { class: 'card ' + (kind === 'fail' ? 'flag-err' : kind === 'aside' ? 'flag-err' : kind === 'load' && !inst ? 'flag-warn' : ''), key: 'node-' + n.id, 'aria-label': name },
    h('div.ch', { style: 'padding-bottom:10px' },
      h('h2.mono', { class: dim ? 'mut' : null }, name), badge(label, kind),
      n.local ? badge('This machine', 'n', 'The local node: models are read from disk, nothing is uploaded') : null,
      h('span.mut.right', { style: 'font-size:12px;font-weight:600' }, n.tier ? String(n.tier) : n.priority ? `Priority ${n.priority}` : n.excluded ? '' : 'No tier')),
    h('div.cb', { style: 'display:flex;flex-direction:column;gap:12px' },
      // what is on it
      h('div', { style: 'min-height:22px' },
        n.holder ? h('span', inst && inst.state === 'starting' ? 'Loading ' : 'Holds ', h('a.mono', { href: href('models') }, n.holder),
          inst && inst.in_flight > 0 ? h('span.mut', ` · ${plural(inst.in_flight, 'request')} in flight`) : null)
          : n.claimed_by ? h('span', 'Claimed for ', h('a.mono', { href: href('queue') }, n.claimed_by))
            : h('span.mut', n.excluded ? 'Never used' : !n.online ? 'Nothing can be placed here' : 'Nothing loaded')),
      // memory
      total ? h('div', { style: 'display:flex;flex-direction:column;gap:7px' },
        bar([{ cls: 'm', value: model, title: 'This model ' + mib(model) }, { cls: 'o', value: other, title: 'Other use ' + mib(other) },
          { cls: 'rsv', value: n.online && !n.excluded ? usable : 0, title: 'Usable now ' + mib(usable) }], total,
        `${name}: ${mib(total - free)} in use of ${mib(total)}, ${mib(usable)} usable now`),
        h('div.mono.mut', { style: 'display:flex;justify-content:space-between;gap:10px;font-size:12px' },
          h('span', `${num(total - free)} in use`), h('span', `${num(total)} MiB total`))) : null,
      h('div.stats', { style: 'padding:0;gap:10px 28px' },
        stat('Usable now', n.online && !n.excluded ? num(usable) : DASH, n.online && !n.excluded ? 'MiB' : null, { small: true, title: 'Free memory minus the tier headroom: what a new model may take right now' }),
        stat('Free', num(free), 'MiB', { small: true, title: 'As the fleet reports it' + (headroom ? `; ${mib(headroom)} is kept as headroom` : '') }),
        stat('When empty', n.plan_budget_mib ? num(n.plan_budget_mib) : DASH, n.plan_budget_mib ? 'MiB' : null, { small: true, title: 'What the scheduler plans with once nothing is loaded here' })),
      n.health && n.health.channels ? h('div.row', { style: 'gap:6px 16px;font-size:13px;font-weight:600' },
        h('span.mut', 'Health'),
        ['upload', 'heartbeat', 'probe'].map(c => {
          const [txt, cls] = CHANNEL[n.health.channels[c]] || [n.health.channels[c] || 'not known', 'mut'];
          return h('span', { title: c === 'probe' && txt === 'not known' ? 'The connect probe is off by default' : null }, c.charAt(0).toUpperCase() + c.slice(1) + ' ', h('span', { class: cls }, txt));
        })) : null,
      facts.length ? h('div', { style: 'display:flex;flex-direction:column;gap:7px' }, facts) : null),
    h('div.cs.mono.mut', { style: 'font-size:12px;display:flex;flex-wrap:wrap;gap:2px 16px;padding-top:10px;padding-bottom:12px' },
      n.addr ? h('span', { title: 'Worker address' }, n.addr) : h('span', n.local ? 'local' : 'no address known'),
      h('span', { title: 'Node id' }, n.id),
      n.rpc_server_version ? h('span', { title: 'Worker version' }, 'rpc-server ' + n.rpc_server_version) : null,
      n.rpc_server_mem_mib ? h('span', { title: 'Memory the worker process holds' }, 'worker ' + mib(n.rpc_server_mem_mib)) : null));
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
          excluded ? [' · ', h('b', num(excluded)), ' excluded'] : null, ' · ', h('b', mib(usable)), ' usable now'],
          legend([['m', 'Model loaded here'], ['o', 'Other use'], ['rsv', 'Usable now']])),
        staleBanner(),
        nodes.length ? h('div.grid', nodes.map(n => tile(st, n)))
          : h('section.card', empty('No nodes are known yet', 'Nodes come from the fleet feed. None has been reported so far; check that the fleet manager is reachable (About shows the connection).')),
      ];
    }, ['status', 'tick', 'conn']);
    return view;
  },
});
