// Run history (list) and run detail.

import { h } from '../../core/dom.js';
import * as api from '../../core/api.js';
import { href, navigate } from '../../core/router.js';
import { badge, table, chips, empty, skeleton, stat, banner } from '../../core/ui.js';
import { num, mib, bytes, dur, ago, at, dateTime, plural, DASH } from '../../core/format.js';
import { cache, runBadge, passBadge, outcomeBadge, phaseTitle, changeMark, withUnit, fmtMetric, metricDef, errorBanner, reasonNode } from './common.js';
import { state } from '../../core/store.js';
import { logsLink, padded } from '../../core/logs.js';
import { loadModeNode, reloadAmount, cacheOccupied, occupiedText, rolesText, RELOAD_TIP } from '../../core/stepfacts.js';

// runLogs(run) -> a link to every log line written while the run ran (all
// sources and nodes: a benchmark touches the whole fleet), a minute either
// side; null before it started or while central logging is off.
function runLogs(run) {
  if (!run.started_at) return null;
  const [from, to] = padded(run.started_at, run.finished_at || null);
  return logsLink(state.status, 'Logs', '*', from, to, 'Open the log lines written while this run ran, in a new tab');
}

// ---------- the list ----------

export function runsList(suiteFilter, setFilter) {
  const runs = cache.runs;
  const suites = cache.suites || [];
  let body;
  if (cache.errors.runs && !runs) body = empty('The runs could not be read', cache.errors.runs.message);
  else if (!runs) body = skeleton(4);
  else if (!runs.length) body = empty(suiteFilter ? 'No runs of this suite yet' : 'No runs yet', 'Run a suite from the overview. Every run is kept here with its results.');
  else body = table({
    minWidth: 900, rows: runs, key: r => r.id, onRow: r => navigate('benchmarks', ['runs', r.id]),
    columns: [
      { label: 'Run', render: r => h('span', h('span.mono.nowrap', r.id), r.note ? h('span.sub', r.note) : null) },
      { label: 'Suite', render: r => r.suite_name || r.suite_id },
      { label: 'State', cls: 'tight', render: r => h('span', runBadge(r.state), r.trigger === 'schedule' ? h('span.sub', 'Scheduled') : null) },
      { label: 'Started', cls: 'tight', render: r => ago(r.started_at || r.queued_at) },
      { label: 'Duration', cls: 'r', render: r => (r.started_at && r.finished_at ? dur((r.finished_at - r.started_at) / 1000) : DASH) },
      { label: 'Passed', cls: 'r', render: r => (r.summary ? num(r.summary.passed) : DASH) },
      { label: 'Failed', cls: 'r', render: r => (r.summary ? h('span', { class: r.summary.failed ? 'err-t' : '' }, num(r.summary.failed)) : DASH) },
      { label: 'Not judged', cls: 'r', title: 'Steps refused because their profile forces a node that exclude_nodes names: neither passed nor failed',
        render: r => (r.summary ? h('span', { class: r.summary.refused_by_exclusion ? '' : 'mut' }, num(r.summary.refused_by_exclusion || 0)) : DASH) },
      { label: 'Regressions', cls: 'r', render: r => (r.summary ? h('span', { class: r.summary.regressions ? 'err-t' : '' }, num(r.summary.regressions)) : DASH) },
    ],
  });
  return h('section.card', { key: 'runs' },
    h('div.ch', h('h2', 'Runs'), h('span.note', 'Newest first. Open one for every figure.'),
      suites.length > 1 ? h('span.right', chips([{ id: '', label: 'All suites' }].concat(suites.map(s => ({ id: s.id, label: s.name || s.id }))), suiteFilter || '', setFilter, 'Filter by suite')) : null),
    body);
}

// ---------- the detail ----------

const exportLink = (id, label, params) => h('a.btn.sm', { href: api.url(`/api/bench/runs/${encodeURIComponent(id)}/export`, params), download: '' }, label);

function envCard(run) {
  const e = run.env;
  if (!e) return null;
  const workers = e.workers || [];
  return h('section.card', { key: 'env' },
    h('div.ch', h('h2', 'Environment'), h('span.note', 'What this run ran on. Runs on a different setup are not comparable one to one.')),
    h('div.cb', h('dl.kv',
      h('dt', 'Scheduler version'), h('dd.mono', e.scheduler_version || DASH),
      h('dt', 'Configuration revision'), h('dd.mono', e.config_revision || DASH),
      h('dt', 'llama binary'), h('dd', h('code.cmd', e.llama_binary || DASH)),
      h('dt', 'Workers'), h('dd', workers.length ? h('div.col', { style: 'gap:6px' }, workers.map(w => h('span', h('span.node', w.node), h('span.mono.mut', '  rpc-server ' + (w.rpc_server_version || DASH))))) : h('span.mut', 'None (the model ran on the scheduler host)')))));
}

// notJudged(step): refused because its profile forces an excluded node; the
// step was not judged, so nothing about it is a pass or a failure.
export const notJudged = step => !!step && (step.outcome === 'refused_by_exclusion' || !!(step.expect && step.expect.not_judged));

function checks(step) {
  const ex = step.expect;
  if (!ex || !(ex.checks || []).length) return null;
  const nj = notJudged(step);
  return h('div.cs', h('div', { style: 'font-weight:700;margin-bottom:8px' }, 'Expectations'),
    h('div', { style: 'display:flex;flex-direction:column;gap:8px' }, ex.checks.map(c =>
      h('div.why', h('i', { class: c.pass ? 'm' : nj ? '' : 'x' }), h('span', h('span.mono', c.name), c.detail ? h('span.mut', ' · ' + c.detail) : null),
        c.pass ? badge('Passed', 'use') : nj ? badge('Not judged', 'n', 'The profile forces a node that exclude_nodes names, so the step was refused and not judged') : badge('Failed', 'aside')))));
}

function comparison(step) {
  const c = step.comparison;
  if (!c) return null;
  if (!c.found) return h('div.cs', h('span.mut', c.baseline_run_id ? `The baseline run ${c.baseline_run_id} has no usable figures for this benchmark.` : 'No baseline to compare with.'));
  const rows = [['load_s', c.load_s_pct], ['decode_tps', c.decode_tps_pct], ['prefill_tps', c.prefill_tps_pct], ['ttft_ms', c.ttft_ms_pct]].filter(r => r[1] != null);
  return h('div.cs',
    h('div', { style: 'font-weight:700;margin-bottom:8px' }, 'Against the baseline ', h('a.mono', { href: href('benchmarks', ['runs', c.baseline_run_id]) }, c.baseline_run_id), c.regression ? [' ', badge('Regression', 'aside')] : null),
    rows.length ? h('div.stats', { style: 'padding:0 0 4px' }, rows.map(([k, v]) => h('div.stat', h('span', metricDef(k).label), h('b', changeMark(k, v, c.regression && regressed(k, v, c)))))) : h('span.mut', 'No figure to compare.'),
    c.output_hash_differs ? h('p.err-t', { style: 'margin-top:6px' }, 'The output of a repeatable request is different from the baseline.') : null,
    (c.reasons || []).length ? h('div', { style: 'display:flex;flex-direction:column;gap:6px;margin-top:8px' }, c.reasons.map(t => h('div.why', h('i.x'), h('span', t), h('span')))) : null);
}
// The server names the reasons; mark a figure when it is the worse direction by a lot.
function regressed(k, v, c) {
  const worse = metricDef(k).better === 'lower' ? v > 0 : v < 0;
  return worse && Math.abs(v) >= (k === 'load_s' ? 15 : 7);
}

const statsRows = s => [['decode_tps', 'Decode speed', s.decode_tps], ['prefill_tps', 'Prefill speed', s.prefill_tps], ['ttft_ms', 'Time to first token', s.ttft_ms], ['wall_ms', 'Wall time', s.wall_ms]]
  .filter(r => r[2]);
const unitOf = k => (k === 'wall_ms' ? 'ms' : metricDef(k).unit);
const f1 = (k, v) => (v == null ? DASH : num(v, k === 'ttft_ms' || k === 'wall_ms' ? 0 : 1));

function statsTable(s) {
  const rows = statsRows(s);
  if (!rows.length) return null;
  return table({
    minWidth: 620, rows, key: r => r[0],
    columns: [
      { label: 'Measured', render: r => r[1] },
      { label: 'Mean', cls: 'r', render: r => `${f1(r[0], r[2].mean)} ${unitOf(r[0])}` },
      { label: 'Median', cls: 'r', render: r => f1(r[0], r[2].median) },
      { label: 'Min', cls: 'r', render: r => f1(r[0], r[2].min) },
      { label: 'Max', cls: 'r', render: r => f1(r[0], r[2].max) },
      { label: 'Std dev', cls: 'r', render: r => f1(r[0], r[2].stdev) },
      { label: 'Requests', cls: 'r', render: r => num(r[2].n) },
    ],
  });
}

function loadBlock(l, withNodes = true) {
  if (!l) return null;
  return h('div',
    h('div.stats', { style: 'padding-top:4px' },
      stat('Load time', l.load_s != null ? num(l.load_s, 1) : DASH, 's'),
      stat('Average upload', l.avg_mb_s ? num(l.avg_mb_s, 0) : DASH, l.avg_mb_s ? 'MB/s' : null),
      stat('Uploaded', bytes(l.bytes || null)),
      stat('Memory', l.memory_mib ? num(Math.round(l.memory_mib)) : DASH, l.memory_mib ? 'MiB' : null),
      h('div.stat', { title: 'How the weights reached the nodes' }, h('span', 'Load mode'), h('b', { style: 'font-size:16px' }, loadModeNode(l.load_mode))),
      l.outcome && l.outcome !== 'ready' ? stat('Load result', l.outcome) : null),
    l.reason ? h('div.cb', banner('err', reasonNode(l.reason))) : null,
    withNodes && (l.nodes || []).length ? table({
      minWidth: 480, rows: l.nodes, key: n => n.node,
      columns: [
        { label: 'Node', render: n => h('span.node', n.node) },
        { label: 'Received', cls: 'r', render: n => (n.bytes != null ? bytes(n.bytes) : DASH) },
        { label: 'Memory', cls: 'r', render: n => (n.memory_mib != null ? mib(n.memory_mib) : DASH) },
      ],
    }) : null,
    l.source && l.source !== 'history' ? h('div.cb', h('span.mut', { style: 'font-size:12px' }, l.source === 'recent_loads' ? 'Figures from the scheduler\'s recent loads; the history was not available.' : 'Figures observed by the runner; the history was not available.')) : null);
}

function requestsTable(reqs) {
  if (!reqs.length) return null;
  const shown = reqs.slice(0, 30);
  return h('div',
    table({
      minWidth: 640, rows: shown, key: r => r.seq,
      columns: [
        { label: 'Request', render: r => h('span.mono', r.name || '#' + r.seq) },
        { label: 'Status', cls: 'r', render: r => (r.status === 200 ? h('span', '200') : h('span.err-t', r.status || 'error')) },
        { label: 'Wall', cls: 'r', render: r => `${num(r.wall_ms)} ms` },
        { label: 'First token', cls: 'r', render: r => (r.ttft_ms != null ? `${num(r.ttft_ms)} ms` : DASH) },
        { label: 'Prompt', cls: 'r', render: r => (r.prompt_n != null ? `${num(r.prompt_n)} tok` : DASH) },
        { label: 'Decode', cls: 'r', render: r => (r.decode_tps ? `${num(r.decode_tps, 1)} tok/s` : DASH) },
      ],
    }),
    reqs.length > shown.length ? h('div.cb', h('span.mut', `${reqs.length - shown.length} more requests are in the JSON export.`)) : null);
}

function turnsTable(turns) {
  return table({
    minWidth: 760, rows: turns, key: t => t.turn,
    columns: [
      { label: 'Turn', render: t => h('span', h('span.mono', t.turn), t.name ? h('span.sub', t.name) : null) },
      { label: 'Prompt', cls: 'r', title: 'Whole prompt: processed plus reused', render: t => `${num(t.prompt_tokens)} tok` },
      { label: 'Reused', cls: 'r', title: 'Prompt tokens taken from the cache', render: t => `${num(t.cache_n)} tok` },
      { label: 'Prefill', cls: 'r', render: t => (t.prefill_tps ? `${num(t.prefill_tps, 1)} tok/s` : DASH) },
      { label: 'Decode', cls: 'r', render: t => (t.decode_tps ? `${num(t.decode_tps, 1)} tok/s` : DASH) },
      { label: 'First token', cls: 'r', render: t => (t.ttft_ms != null ? `${num(t.ttft_ms)} ms` : DASH) },
      { label: 'Wall', cls: 'r', render: t => `${num(t.wall_ms)} ms` },
      { label: 'Status', cls: 'r', render: t => (t.status === 200 ? '200' : h('span.err-t', t.status || 'error')) },
    ],
  });
}

// ---------- the setup of a step: roles, lowest available memory, reload ----------

const ROLE_WORD = { host: 'Host', worker: 'Worker' };
const notRecorded = () => h('span.mut', 'Not recorded');

// "Host: host-1 · Workers: 2 (ps5-a, ps5-b)", or '' when not recorded.
const stepRoles = step => rolesText((step.roles || []).filter(r => r.role === 'host').map(r => r.node)[0],
  (step.roles || []).filter(r => r.role === 'worker').map(r => r.node).sort());

function setupBlock(step) {
  const roles = step.roles || [], low = step.mem_low || [], reload = step.reload_from_cache || [];
  const byNode = {};
  const row = (id, node) => (byNode[id || node] = byNode[id || node] || { key: id || node, node, role: null, share: null, low: null, reload: null });
  for (const r of roles) Object.assign(row(r.id, r.node), { role: r.role, share: r.memory_mib });
  for (const m of low) row(m.id, m.node).low = m;
  for (const r of reload) row(r.id, r.node).reload = r;
  const rows = Object.values(byNode).sort((a, b) => (a.role === 'host' ? -1 : 0) - (b.role === 'host' ? -1 : 0));
  if (!rows.length) {
    return h('div.cs', h('div', { style: 'font-weight:700;margin-bottom:4px' }, 'Nodes'),
      h('span.mut', 'Roles, lowest available memory and what can stay in the file cache: not recorded for this run.'));
  }
  // Which memory the figure is: the GPU's free memory on a node whose GPU has
  // memory of its own, else available system memory (rows without a kind).
  const kindWord = low => (low.kind === 'gpu' ? '(GPU)' : '(system)');
  const memCell = (v, low) => (v != null ? h('span', { title: low.note || null }, mib(v), h('span.sub', ' ' + kindWord(low))) : notRecorded());
  return h('div.cs', h('div', { style: 'font-weight:700;margin-bottom:8px' }, 'Nodes'),
    table({
      minWidth: 760, rows, key: r => r.key,
      columns: [
        { label: 'Node', render: r => h('span.node', r.node) },
        { label: 'Role', cls: 'tight', title: 'The host runs llama-server; a worker runs rpc-server with a share of the model', render: r => (r.role ? ROLE_WORD[r.role] || r.role : h('span.mut', DASH)) },
        { label: 'Model share', cls: 'r', render: r => (r.share != null ? mib(r.share) : h('span.mut', DASH)) },
        { label: 'Lowest available, load', cls: 'r', title: 'The lowest available memory while the model was loading, sampled about twice a second. (GPU): the free memory of a GPU with memory of its own; (system): available system memory', render: r => (r.low ? memCell(r.low.load_mib, r.low) : notRecorded()) },
        { label: 'Lowest available, step', cls: 'r', title: 'The lowest available memory during the whole benchmark. (GPU): the free memory of a GPU with memory of its own; (system): available system memory', render: r => (r.low ? memCell(r.low.step_mib, r.low) : notRecorded()) },
        { label: 'Can stay in the file cache', title: RELOAD_TIP, render: r => (r.reload ? h('span', { title: RELOAD_TIP }, reloadAmount(r.reload),
          cacheOccupied(r.reload) ? h('span.sub.warn-t', occupiedText(r.node, r.reload)) : null) : h('span.mut', DASH)) },
      ],
    }));
}

// ---- cold loads ----

// coldBadge: whether a cold load really started from empty file caches; the
// reason is on hover.
function coldBadge(c) {
  const why = c.verified ? 'Every machine ended with under 1 % of the model file in its file cache.' : c.reason || 'not guaranteed';
  return h('span', { title: why }, badge(c.verified ? 'cold: verified' : 'cold: not guaranteed', c.verified ? 'use' : 'aside'));
}

// coldMachines lists each machine's file cache before and after the drop.
function coldMachines(c) {
  const rows = [];
  if (c.local) rows.push({ node: 'Scheduler machine', ...c.local });
  for (const n of c.nodes || []) rows.push({ ...n, node: n.node || n.id });
  if (!rows.length) return null;
  const cached = (r, v) => (r.file_bytes ? `${bytes(v)} (${num((100 * v) / r.file_bytes, 1)} %)` : bytes(v));
  return table({
    minWidth: 520, rows, key: r => r.node,
    columns: [
      { label: 'File cache of', render: r => h('span.mono', r.node) },
      { label: 'File', cls: 'r', render: r => (r.error ? DASH : bytes(r.file_bytes)) },
      { label: 'Cached before', cls: 'r', render: r => (r.error ? DASH : cached(r, r.cached_before)) },
      { label: 'Cached after', cls: 'r', render: r => (r.error ? h('span.err-t', r.error) : cached(r, r.cached_after)) },
    ],
  });
}

// coldSummary is the result row of a step with a cold load phase. It follows
// the phases, whatever their order: each load phase under its own label
// (load time, the first token after it) and each requests phase's decode
// speed, so "cold load" and "reload" are whatever the benchmark called them.
function coldSummary(step) {
  const ph = step.phases || [];
  if (!ph.some(p => p.type === 'load' && p.cold)) return null;
  const loads = ph.filter(p => p.type === 'load');
  const nameOf = p => p.label || (p.cold ? 'cold load' : `load (phase ${p.idx + 1})`);
  const ttftAfter = l => {
    const n = loads.find(q => q.idx > l.idx);
    const end = n ? n.idx : Infinity;
    const r = (step.requests || []).find(q => q.phase_idx > l.idx && q.phase_idx < end && q.ttft_ms != null);
    return r ? r.ttft_ms : null;
  };
  const items = [];
  for (const l of loads) {
    const t = ttftAfter(l);
    items.push(stat(nameOf(l), l.load && l.load.load_s != null ? num(l.load.load_s, 1) : DASH, l.load && l.load.load_s != null ? 's' : null));
    items.push(stat('First token after ' + nameOf(l), t != null ? num(t) : DASH, t != null ? 'ms' : null));
  }
  for (const p of ph.filter(q => q.type === 'requests' && q.stats && q.stats.decode_tps)) {
    items.push(stat('Decode, ' + (p.label || `phase ${p.idx + 1}`), num(p.stats.decode_tps.mean, 1), 'tok/s'));
  }
  return h('div', { key: 'cold-sum' },
    h('div.day', { style: 'display:flex;flex-wrap:wrap;gap:6px 12px;align-items:center' }, h('b', { style: 'color:var(--text)' }, 'Loads'),
      loads.filter(l => l.cold).map(l => coldBadge(l.cold))),
    h('div.stats', { style: 'padding-top:4px' }, items));
}

const PH_STATE = { done: ['Done', 'use'], failed: ['Failed', 'fail'], cancelled: ['Cancelled', 'n'], skipped: ['Skipped', 'settle'], running: ['Running', 'load'] };

function phaseBlock(step, p) {
  const [sl, sk] = PH_STATE[p.state] || [p.state, 'n'];
  const reqs = (step.requests || []).filter(r => r.phase_idx === p.idx);
  const body = [];
  switch (p.type) {
    case 'load':
      body.push(loadBlock(p.load));
      if (p.cold) body.push(p.cold.reason ? h('div.cb', banner('warn', p.cold.reason)) : null, coldMachines(p.cold));
      break;
    case 'warmup': case 'requests':
      if (p.stats) body.push(statsTable(p.stats));
      if (p.stats && p.stats.failed) body.push(h('div.cb', h('span.err-t', `${plural(p.stats.failed, 'request')} did not return 200.`)));
      body.push(requestsTable(reqs));
      break;
    case 'conversation':
      body.push((p.turns || []).length ? turnsTable(p.turns) : null);
      break;
    case 'hold':
      body.push(h('div.stats', { style: 'padding-top:4px' }, stat('Held', p.held_s != null ? num(p.held_s, 0) : DASH, 's'),
        stat('Model after the hold', p.loaded_after_hold == null ? DASH : p.loaded_after_hold ? 'Still loaded' : 'Was unloaded')));
      if (p.stats) body.push(statsTable(p.stats));
      body.push(requestsTable(reqs));
      break;
    case 'concurrent_requests': {
      const c = p.concurrent;
      if (c) {
        body.push(h('div.stats', { style: 'padding-top:4px' }, stat('Requests', num(c.n)), stat('Wall time', num(c.wall_ms / 1000, 1), 's'), stat('Errors', num(c.errors))));
        body.push(table({
          minWidth: 520, rows: c.items || [], key: i => i.index,
          columns: [
            { label: 'Request', cls: 'r', render: i => i.index },
            { label: 'Status', cls: 'r', render: i => (i.status === 200 ? '200' : h('span.err-t', i.status || 'error')) },
            { label: 'Waited', cls: 'r', title: 'Time before the model started on it', render: i => (i.wait_ms != null ? `${num(i.wait_ms)} ms` : DASH) },
            { label: 'Wall', cls: 'r', render: i => `${num(i.wall_ms)} ms` },
            { label: 'Decode', cls: 'r', render: i => (i.decode_tps ? `${num(i.decode_tps, 1)} tok/s` : DASH) },
          ],
        }));
      }
      break;
    }
    case 'unload':
      if (p.unload) body.push(h('div.stats', { style: 'padding-top:4px' }, stat('Until the instance was gone', num(p.unload.unload_s, 1), 's'), stat('Was loaded', p.unload.was_loaded ? 'Yes' : 'No')));
      break;
  }
  return h('div', { key: 'ph-' + p.idx },
    h('div.day', { style: 'display:flex;flex-wrap:wrap;gap:6px 12px;align-items:center' },
      h('b', { style: 'color:var(--text)' }, `Phase ${p.idx + 1}: ${p.label || phaseTitle(p.type)}`), badge(sl, sk), p.cold ? coldBadge(p.cold) : null,
      p.duration_s ? h('span.mono', dur(p.duration_s)) : null),
    p.error ? h('div.cb', { style: 'padding-top:10px' }, banner('err', reasonNode(p.error))) : null,
    body.filter(Boolean));
}

function stepCard(step) {
  const ph = step.phases || [];
  const hasPhases = ph.length > 0;
  const bad = step.outcome && step.outcome !== 'ready';
  return h('section', { class: 'card ' + (!notJudged(step) && (bad || (step.expect && !step.expect.pass)) ? 'flag-err' : ''), key: 'step-' + step.idx },
    h('div.ch',
      h('h2', step.name || step.benchmark_id), passBadge(step), outcomeBadge(step.outcome), step.comparison && step.comparison.regression ? badge('Regression', 'aside') : null,
      h('span.mono.mut', step.profile), stepRoles(step) ? h('span.mut', stepRoles(step)) : step.node_count ? h('span.mut', plural(step.node_count, 'worker')) : null,
      step.model_file ? h('span.mono.mut.right.wrap-any', { title: 'Model file' }, step.model_file) : null),
    step.reason || step.error ? h('div.cb', banner(step.outcome === 'refused' || step.outcome === 'refused_by_exclusion' ? 'warn' : 'err',
      [step.outcome === 'refused' ? 'The scheduler refused it: ' : step.outcome === 'refused_by_exclusion' ? 'Refused because a forced node is excluded (not judged): ' : step.outcome === 'failed' ? 'It failed: ' : '', reasonNode(step.reason || step.error), step.http_status ? ` (HTTP ${step.http_status})` : ''])) : null,
    h('div.stats', { style: 'padding-top:2px' },
      stat('Started', step.started_at ? at(step.started_at) : DASH),
      stat('Took', step.started_at && step.finished_at ? dur((step.finished_at - step.started_at) / 1000) : DASH),
      step.stats && step.stats.requests ? stat('Measured requests', num(step.stats.requests - (step.stats.failed || 0)), `of ${num(step.stats.requests)}`) : null),
    checks(step), comparison(step), setupBlock(step), coldSummary(step),
    hasPhases ? ph.map(p => phaseBlock(step, p)) : [
      step.load ? h('div', h('div.day', h('b', { style: 'color:var(--text)' }, 'Load')), loadBlock(step.load)) : null,
      step.stats ? statsTable(step.stats) : null],
    step.log_file ? h('div.cs', h('span.mut', 'Child log '), h('code.cmd', step.log_file)) : null);
}

export function runDetail(run, error) {
  if (error) return [h('div.card', empty('The run could not be read', error)), ];
  if (!run) return [h('section.card', skeleton(4))];
  const steps = run.steps || [];
  const sum = run.summary || { steps: steps.length, passed: 0, failed: 0, regressions: 0 };
  const id = run.id;
  return [
    h('div.row', h('a', { href: href('benchmarks', ['runs']) }, '← All runs')),
    run.error ? errorBanner(reasonNode(run.error)) : null,
    h('section.card', { key: 'head' },
      h('div.ch', h('h2.mono', id), runBadge(run.state), run.trigger === 'schedule' ? badge('Scheduled', 'n') : null,
        h('span.right.row', { style: 'gap:8px' }, runLogs(run), exportLink(id, 'Markdown', { format: 'md' }), exportLink(id, 'CSV', { format: 'csv' }), exportLink(id, 'JSON', { format: 'json' }),
          h('details', { style: 'position:relative' }, h('summary.btn.sm', { style: 'list-style:none' }, 'More exports'),
            h('div', { style: 'position:absolute;right:0;top:36px;z-index:5;background:var(--card);border:1px solid var(--line-2);border-radius:10px;padding:8px;display:flex;flex-direction:column;gap:6px;min-width:200px' },
              exportLink(id, 'CSV, one row per phase', { format: 'csv', detail: 'phases' }), exportLink(id, 'CSV, one row per turn', { format: 'csv', detail: 'turns' }))))),
      h('div.stats',
        stat('Suite', run.suite_name || run.suite_id), stat('Started', run.started_at ? ago(run.started_at) : DASH),
        stat('Took', run.started_at && run.finished_at ? dur((run.finished_at - run.started_at) / 1000) : run.finished_at ? DASH : 'Not finished'),
        stat('Benchmarks', num(sum.steps)), stat('Passed', num(sum.passed)), stat('Failed', num(sum.failed), null, { cls: sum.failed ? 'err-t' : '' }),
        sum.refused_by_exclusion ? stat('Not judged (excluded node)', num(sum.refused_by_exclusion)) : null,
        stat('Regressions', num(sum.regressions), null, { cls: sum.regressions ? 'err-t' : '' })),
      run.note || run.baseline_run_id ? h('div.cb', h('dl.kv',
        run.note ? [h('dt', 'Note'), h('dd', run.note)] : null,
        run.baseline_run_id ? [h('dt', 'Compared with'), h('dd', h('a.mono', { href: href('benchmarks', ['runs', run.baseline_run_id]) }, run.baseline_run_id))] : null)) : null),
    envCard(run),
    steps.length ? steps.map(stepCard) : h('section.card', empty('No benchmark has finished yet', run.state === 'queued' ? 'The run is waiting to start.' : 'The results appear here as each benchmark finishes.')),
  ];
}
