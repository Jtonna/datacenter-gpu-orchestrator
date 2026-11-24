// Shared by the Models home and the model detail: the card of one loaded or
// loading INSTANCE (two instances of one model are two cards, each naming its
// nodes), and the short phrases that describe a model of the catalog.
//
// An instance comes from the WebSocket "status" (instances); a model from
// REST GET /api/models (the catalog: one entry per model file).

import { h } from '../../core/dom.js';
import { href } from '../../core/router.js';
import { badge, nodeChips, stat, bar, memoryRow, progress, steps, banner, more } from '../../core/ui.js';
import { num, gb, gbNum, mib, bytes, tps, dur, ago, until, plural, clock, toMs, now, DASH } from '../../core/format.js';
import { avatar } from '../../core/avatar.js';
import { logsLink, profileQuery, padded } from '../../core/logs.js';
import { reloadLine } from '../../core/stepfacts.js';
import { nodeName, nodeByAddr, instanceNodeNames, nodeMemory, instanceBadge, healthBadge, machineBadge, findNode, drift, machines, shortReason, plainReason } from '../../core/status.js';

// ---- phrases about a catalog model ----

// sizeLine(m) -> "12B · Q4_K_M · 6.6 GB": what the file is, in one line.
export function sizeLine(m) {
  if (m.kind === 'command') return 'A program on this machine';
  return [m.parameters, m.quant, m.size_bytes ? bytes(m.size_bytes) : null].filter(Boolean).join(' · ') || 'Size not known';
}

// stateBadge(m) -> the one badge of a model row.
export function stateBadge(m, st) {
  const insts = m.instances || [];
  if (m.state === 'ready') {
    const nodes = new Set(insts.filter(i => i.state !== 'starting').flatMap(i => i.nodes || []));
    return badge(nodes.size ? `Ready on ${machines(st, nodes.size)}` : 'Ready', 'ready');
  }
  if (m.state === 'loading') return badge('Loading', 'load');
  return badge('Not loaded', 'n');
}

// fitOf(m, st) -> {text, sub, kind, title}: what the model needs and on how many
// machines it runs. A measured figure says "Runs on"; an estimate says so.
export function fitOf(m, st) {
  const f = m.fit;
  if (m.kind === 'command') {
    const p = (m.profiles || [])[0] || {};
    return { need: p.needs_mib || null, text: 'This machine', sub: 'set in the profile', kind: 'cmd' };
  }
  if (!f || !f.need_mib) return { need: null, text: 'Not known yet', sub: '', kind: 'unknown' };
  if (f.basis === 'measured') {
    const ran = f.ran_on_nodes || f.min_nodes || 1;
    // It was measured on `ran` machines, but that only says which
    // configurations happened to be run: when the measured need fits on
    // fewer, that is the answer, marked as an estimate.
    if (f.min_nodes > 0 && f.min_nodes < ran) {
      const n = f.min_nodes;
      return { need: f.need_mib, nodes: n, ranOn: ran, estimate: true, text: `Should fit on ${machines(st, n)}`, sub: 'measured', kind: n > 1 ? 'several' : 'one',
        title: `${mib(f.need_mib)} measured on a load across ${machines(st, ran)}. That fits on ${machines(st, n)} of the empty cluster, but it has not been loaded on so few yet, so the machine count is an estimate.` };
    }
    return { need: f.need_mib, nodes: ran, text: `Runs on ${machines(st, ran)}`, sub: 'measured', kind: ran > 1 ? 'several' : 'one',
      title: `${mib(f.need_mib)} measured on a load across ${machines(st, ran)}` };
  }
  const word = f.basis === 'configured' ? 'set in the profile' : 'estimated';
  if (f.min_nodes === 0) {
    return { need: f.need_mib, nodes: 0, text: 'Too big for this cluster', sub: word, kind: 'none',
      title: `${mib(f.need_mib)} ${word}; the empty cluster holds ${mib(f.cluster_budget_mib)}` };
  }
  if (f.min_nodes == null) return { need: f.need_mib, text: 'Not known yet', sub: word, kind: 'unknown' };
  return { need: f.need_mib, nodes: f.min_nodes, estimate: true, text: `Should need ${machines(st, f.min_nodes)}`, sub: word, kind: f.min_nodes > 1 ? 'several' : 'one',
    title: `${mib(f.need_mib)} ${word}; not loaded yet, so this is a guide` };
}

// runsOnCell(m, st) -> the "Runs on" cell of a model row: the fewest machines
// it fits on, "(estimate)" when it has not run on that few, and what it was
// measured on when that is more.
export function runsOnCell(m, st) {
  const f = fitOf(m, st), no = cannotRun(m);
  if (f.kind === 'none' || f.kind === 'unknown' || f.kind === 'cmd') return h('span', { title: f.title }, f.text, no && f.kind !== 'none' ? h('span.sub', no) : null);
  return h('span', { title: f.title }, h('span.nowrap', machines(st, f.nodes), f.estimate ? h('span.quiet', ' (estimate)') : null),
    f.ranOn ? h('span.sub', `measured on ${f.ranOn}`) : no ? h('span.sub', no) : null);
}

// runsLine(m, st) -> "Runs on 1 PS5 · 7.2 GB" for a header.
export function runsLine(m, st) {
  const f = fitOf(m, st);
  return f.need ? `${f.text} · ${gb(f.need)}` : f.text;
}

// cannotRun(m) -> a short plain sentence when no configuration of the model
// could start now and the reason is not just "busy", else ''.
export function cannotRun(m) {
  const ps = m.profiles || [];
  if (!ps.length) return m.kind === 'llama' ? 'No configuration uses this file yet' : '';
  // waiting for a busy node is not "cannot run": only a refusal of every configuration is
  if (m.state !== 'not_loaded' || ps.some(p => !p.plan || p.plan.would_start || (p.plan.action !== 'reject' && p.plan.action !== 'backoff'))) return '';
  const hand = ps.filter(p => !p.generated);
  const p = (hand.length ? hand : ps)[0];
  return shortReason(p.plan && p.plan.reason) || 'Cannot be placed right now';
}

// ---- one instance ----

const staleMark = inst => (inst.stale_profile
  ? badge('Older definition', 'settle', 'Its configuration was changed or deleted after it loaded. It keeps running as it was; the new definition is used at its next load.') : null);

function head(st, inst, model, extra) {
  const names = instanceNodeNames(st, inst);
  const key = model ? model.key : inst.model_file;
  return h('div.ch',
    avatar(model || { name: inst.model_file || inst.profile }, 28),
    h('h2.name', key ? h('a', { href: href('models', [key]), style: 'color:inherit' }, model ? model.name : inst.model_file) : inst.profile),
    instanceBadge(inst), extra, staleMark(inst),
    h('span.mut', { style: 'font-weight:600' }, 'on ', nodeChips(names),
      inst.waiters > 0 && inst.state === 'starting' ? ` · ${plural(inst.waiters, 'request')} waiting for it` : null),
    h('span.cfg.right', { title: 'The configuration (profile) it was loaded with' }, inst.profile),
    instanceLogs(st, inst));
}

// instanceLogs -> a link to this instance's log lines (its program's output
// and the nodes' launch lines carry its profile), from a minute before it
// was started until now; null while central logging is off.
function instanceLogs(st, inst) {
  const from = toMs(inst.created);
  return logsLink(st, 'Logs', profileQuery(inst.profile), from != null ? padded(from, null)[0] : null, null,
    'Open the log lines of ' + inst.profile + ' since it was started, in a new tab');
}

// The command line and the log file are long: they stay folded until asked for.
function technical(inst, open, toggle) {
  if (!inst.cmd && !inst.log_file) return null;
  const k = 'tech-' + inst.profile;
  return h('div.cs', { style: 'padding-top:10px;padding-bottom:12px' },
    more(open.has(k), () => toggle(k), 'Command and log file', 'Hide the command and log file'),
    open.has(k) ? h('dl.kv', { style: 'margin-top:10px' },
      inst.cmd ? [h('dt', 'Command'), h('dd', h('code.cmd', inst.cmd))] : null,
      inst.log_file ? [h('dt', 'Log file'), h('dd', h('code.cmd', inst.log_file))] : null) : null);
}

function readyCard(st, inst, model, open, toggle) {
  const lr = inst.last_request;
  const host = (inst.host_memory || []).reduce((s, d) => s + (d.total_mib || 0), 0);
  const serving = inst.in_flight > 0;
  return h('section.card', { key: 'inst-' + inst.profile, 'aria-label': inst.profile },
    head(st, inst, model),
    inst.victim_of ? h('div.cb', banner('warn', ['It is being unloaded to make room for ', h('span.mono', inst.victim_of), serving ? ', once its requests in flight finish.' : '.'])) : null,
    h('div.stats',
      stat('In flight', num(inst.in_flight || 0)),
      stat('Waiting', num(inst.waiters || 0)),
      stat('Speed', lr && lr.tokens_per_second > 0 ? tps(lr.tokens_per_second) : DASH, lr && lr.tokens_per_second > 0 ? 'tok/s' : null,
        { title: lr ? `The last finished request: ${num(lr.output_tokens)} tokens out in ${dur((lr.duration_ms || 0) / 1000)}` : 'No request has finished yet' }),
      stat('Ready', ago(inst.ready_since)),
      stat('Last used', serving ? 'now' : ago(inst.last_used, 'not yet')),
      stat('Load took', dur(inst.load_duration)),
      stat('Served', num(inst.requests_served || 0), inst.requests_served === 1 ? 'request' : 'requests'),
      stat('Memory', gbNum(inst.device_total_mib || 0), 'GB', { title: mib(inst.device_total_mib || 0) + ' on the nodes, from the load log' }),
      host > 0 ? stat('Host RAM', gbNum(host), 'GB', { title: mib(host) + ' on the machine that runs the scheduler' }) : null),
    (inst.nodes || []).map(n => {
      const name = nodeName(st, n.id), mem = nodeMemory(n);
      const hb = healthBadge(findNode(st, n.id)) || machineBadge(st, findNode(st, n.id));
      if (!mem.total_mib) {
        return h('div.nr', { key: 'mem-' + name }, h('span.mono', name, hb ? [' ', hb] : null), bar([], 1, 'No memory report'),
          h('span.mono.fig.mut', n.budget_mib ? `– / ${gb(n.budget_mib)}` : DASH), h('span.mut.split', 'No memory was reported for this node'));
      }
      return memoryRow(name, mem, n.budget_mib, hb);
    }),
    technical(inst, open, toggle));
}

const WORKER = { waiting: ['Waiting', 'n'], receiving: ['Receiving', 'load'], done: ['Done', 'use'] };

function loadingCard(st, inst, model, open, toggle) {
  const load = inst.load || {};
  const d = drift();
  const elapsed = (load.elapsed_s != null ? load.elapsed_s : inst.load_elapsed_s || 0) + d;
  const frac = load.progress != null ? load.progress : inst.load_progress;
  const eta = load.eta_s != null ? Math.max(0, load.eta_s - d) : null;
  const stalled = load.stalled_s != null ? load.stalled_s + d : null;
  const total = load.bytes_total, sent = load.bytes_sent;
  const workers = load.workers || [];
  const measured = load.source === 'measured';
  const startedAt = load.started_at || toMs(inst.created);
  const deadline = toMs(inst.load_deadline);
  const rate = eta > 0 && total != null && sent != null ? (total - sent) / eta / 1e6 : null;
  const phase = inst.phase || 'loading';
  const order = ['preflight', 'launching', 'loading'];
  const at_ = p => (order.indexOf(phase) > order.indexOf(p) ? 'done' : phase === p ? 'now' : '');
  const uploadNote = total == null ? (phase === 'loading' ? 'Loading on this machine' : 'No upload needed')
    : sent != null ? `${bytes(sent)} of ${bytes(total)}` + (rate ? ` · ${num(rate, 0)} MB/s` : '') : `${bytes(total)} to upload`;
  const stepItems = [
    { label: 'Pre-load check', state: at_('preflight'), note: phase === 'preflight' ? 'Checking free memory on the nodes' : 'Passed' },
    { label: 'Start the server', state: at_('launching'), note: phase === 'launching' ? 'Starting' : phase === 'loading' ? 'Running' : '' },
    { label: workers.length ? 'Upload to the nodes' : 'Load into memory', state: phase === 'loading' ? (stalled != null ? 'fail' : 'now') : '', note: phase === 'loading' ? uploadNote : '' },
    { label: 'Ready', state: '', note: eta != null && stalled == null ? ['Expected ≈ ', clock(now() + eta * 1000, false)] : 'Not known yet' },
  ];
  const left = stalled != null ? h('span.err-t', `Stalled: nothing new for ${dur(Math.round(stalled))}`)
    : eta != null ? h('span.mut', { style: 'font-weight:600' }, `About ${dur(eta < 90 ? Math.round(eta) : Math.round(eta / 60) * 60)} left`)
      : h('span.mut', { style: 'font-weight:600' }, 'Time left not known yet');
  return h('section', { class: 'card ' + (stalled != null ? 'flag-err' : ''), key: 'inst-' + inst.profile, 'aria-label': inst.profile },
    head(st, inst, model, stalled != null ? badge('Stalled', 'fail') : null),
    steps(stepItems),
    h('div.cb', progress({
      fraction: frac || 0, cls: stalled != null ? 'x' : 'a',
      label: frac != null ? `${Math.round(frac * 100)} percent loaded` : 'Load in progress',
      left: h('span', frac != null ? h('span.pct', Math.round(frac * 100) + '%') : null, h('span.mut', { style: 'font-weight:600' }, frac != null ? ' · ' : '', `${dur(Math.round(elapsed))} elapsed`)),
      mid: left,
      right: deadline ? h('span', 'Gives up ', until(deadline)) : null,
      foot: [startedAt ? 'Started ' + clock(startedAt) : '', frac != null ? (measured ? 'Measured from the nodes' : 'Estimated from the link speed') : ''],
    })),
    workers.map(w => {
      const node = nodeByAddr(st, w.addr);
      const name = node ? node.hostname || node.id : w.addr;
      const [label, kind] = WORKER[w.state] || [w.state, 'n'];
      const hb = healthBadge(node) || machineBadge(st, node);
      return h('div.nr', { key: 'w-' + w.addr },
        h('span.mono', name),
        bar([{ cls: w.state === 'done' ? 'm' : 'a', value: w.bytes_sent || 0 }], w.bytes_total, `${name}: ${label}`),
        h('span.mono.fig', w.bytes_sent != null ? `${bytes(Math.min(w.bytes_sent, w.bytes_total || w.bytes_sent))} / ${bytes(w.bytes_total)}` : bytes(w.bytes_total)),
        h('span.row', { style: 'gap:6px' }, badge(label, kind), hb));
    }),
    (inst.nodes || []).filter(n => n.local).map(n => h('div.nr', { key: 'local-' + n.id },
      h('span.mono', nodeName(st, n.id)), h('span.mut.split', 'This machine reads the model from its own disk; nothing is uploaded'), h('span'), h('span'))),
    technical(inst, open, toggle));
}

// instanceCard(st, inst, model, open, toggle): `open` is a Set of unfolded
// parts kept by the view, `toggle(key)` flips one and redraws.
export function instanceCard(st, inst, model, open, toggle) {
  return inst.state === 'starting' ? loadingCard(st, inst, model, open, toggle) : readyCard(st, inst, model, open, toggle);
}

// whyNot(plan) -> a plain sentence for a configuration that would not start, with the scheduler's words in the tooltip.
export function planCell(p, st) {
  const plan = p.plan || {};
  const raw = plan.reason ? 'The scheduler says: ' + plan.reason : null;
  // Names only where they are given (the page rule): a forced profile's own
  // nodes. An automatic profile's plan picks among equal nodes and can pick
  // others a moment later, so it says how many: "would load on 2 PS5s".
  // A loaded instance names its nodes (instance cards); a chosen zone, in
  // the Placement preview with its live memory rows.
  const forced = (p.force_nodes || []).length > 0 || p.placement === 'forced';
  const count = plan.nodes ? plan.nodes.length : 0;
  const nodes = !count ? null : forced ? nodeChips(plan.nodes) : h('span', { 'data-would': count }, 'would load on ' + machines(st, count));
  // Whether a reload of the model can come from the nodes' file cache.
  const reload = reloadLine(plan.reload_from_cache, r => r.node || nodeName(st, r.id));
  switch (plan.action) {
    case 'start': return h('span', h('span.ok-t', { style: 'font-weight:700' }, 'Yes'), nodes ? [h('span.mut', forced ? ' · on ' : ' · '), nodes] : null, plan.tight_fit ? [' ', badge('Tight fit', 'n', plan.reason)] : null, plan.warning ? h('span.sub.warn-t', plan.warning) : null, reload);
    case 'serve': return h('span', h('span.ok-t', { style: 'font-weight:700' }, 'Loaded now'), reload);
    case 'wait': return h('span.warn-t', { style: 'font-weight:700' }, 'Loading now');
    case 'evict': return h('span', h('span', { style: 'font-weight:700' }, 'Yes, after making room'), nodes ? [h('span.mut', forced ? ' · on ' : ' · '), nodes] : null, plan.tight_fit ? [' ', badge('Tight fit', 'n', plan.reason)] : null, reload);
    case 'queue': return h('span', { title: raw }, h('span', { style: 'font-weight:700' }, 'Would wait'), h('span.sub', plainReason(plan.reason)));
    case 'backoff': return h('span', { title: raw }, h('span.err-t', { style: 'font-weight:700' }, 'Held back'), h('span.sub', 'Its last load failed'));
    case 'reject': return h('span', { title: raw }, h('span', { style: 'font-weight:700' }, 'No'), h('span.sub', plainReason(plan.reason)));
  }
  return h('span.mut', plan.reason || 'Not known');
}
