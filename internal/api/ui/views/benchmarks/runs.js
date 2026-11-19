// Run history (list) and run detail.

import { h } from '../../core/dom.js';
import * as api from '../../core/api.js';
import { href, navigate } from '../../core/router.js';
import { badge, table, chips, empty, skeleton, stat, banner } from '../../core/ui.js';
import { num, mib, bytes, dur, ago, at, dateTime, plural, DASH } from '../../core/format.js';
import { cache, runBadge, passBadge, outcomeBadge, phaseTitle, changeMark, withUnit, fmtMetric, metricDef, errorBanner } from './common.js';

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

function checks(step) {
  const ex = step.expect;
  if (!ex || !(ex.checks || []).length) return null;
  return h('div.cs', h('div', { style: 'font-weight:700;margin-bottom:8px' }, 'Expectations'),
    h('div', { style: 'display:flex;flex-direction:column;gap:8px' }, ex.checks.map(c =>
      h('div.why', h('i', { class: c.pass ? 'm' : 'x' }), h('span', h('span.mono', c.name), c.detail ? h('span.mut', ' · ' + c.detail) : null), badge(c.pass ? 'Passed' : 'Failed', c.pass ? 'use' : 'aside')))));
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
      l.outcome && l.outcome !== 'ready' ? stat('Load result', l.outcome) : null),
    l.reason ? h('div.cb', banner('err', l.reason)) : null,
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

const PH_STATE = { done: ['Done', 'use'], failed: ['Failed', 'fail'], cancelled: ['Cancelled', 'n'], skipped: ['Skipped', 'settle'], running: ['Running', 'load'] };

function phaseBlock(step, p) {
  const [sl, sk] = PH_STATE[p.state] || [p.state, 'n'];
  const reqs = (step.requests || []).filter(r => r.phase_idx === p.idx);
  const body = [];
  switch (p.type) {
    case 'load': body.push(loadBlock(p.load)); break;
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
      h('b', { style: 'color:var(--text)' }, `Phase ${p.idx + 1}: ${p.label || phaseTitle(p.type)}`), badge(sl, sk),
      p.duration_s ? h('span.mono', dur(p.duration_s)) : null),
    p.error ? h('div.cb', { style: 'padding-top:10px' }, banner('err', p.error)) : null,
    body.filter(Boolean));
}

function stepCard(step) {
  const ph = step.phases || [];
  const hasPhases = ph.length > 0;
  const bad = step.outcome && step.outcome !== 'ready';
  return h('section', { class: 'card ' + (bad || (step.expect && !step.expect.pass) ? 'flag-err' : ''), key: 'step-' + step.idx },
    h('div.ch',
      h('h2', step.name || step.benchmark_id), passBadge(step), outcomeBadge(step.outcome), step.comparison && step.comparison.regression ? badge('Regression', 'aside') : null,
      h('span.mono.mut', step.profile), step.node_count ? h('span.mut', plural(step.node_count, 'worker')) : null,
      step.model_file ? h('span.mono.mut.right.wrap-any', { title: 'Model file' }, step.model_file) : null),
    step.reason || step.error ? h('div.cb', banner(step.outcome === 'refused' ? 'warn' : 'err',
      [step.outcome === 'refused' ? 'The scheduler refused it: ' : step.outcome === 'failed' ? 'It failed: ' : '', step.reason || step.error, step.http_status ? ` (HTTP ${step.http_status})` : ''])) : null,
    h('div.stats', { style: 'padding-top:2px' },
      stat('Started', step.started_at ? at(step.started_at) : DASH),
      stat('Took', step.started_at && step.finished_at ? dur((step.finished_at - step.started_at) / 1000) : DASH),
      step.stats && step.stats.requests ? stat('Measured requests', num(step.stats.requests - (step.stats.failed || 0)), `of ${num(step.stats.requests)}`) : null),
    checks(step), comparison(step),
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
    run.error ? errorBanner(run.error) : null,
    h('section.card', { key: 'head' },
      h('div.ch', h('h2.mono', id), runBadge(run.state), run.trigger === 'schedule' ? badge('Scheduled', 'n') : null,
        h('span.right.row', { style: 'gap:8px' }, exportLink(id, 'Markdown', { format: 'md' }), exportLink(id, 'CSV', { format: 'csv' }), exportLink(id, 'JSON', { format: 'json' }),
          h('details', { style: 'position:relative' }, h('summary.btn.sm', { style: 'list-style:none' }, 'More exports'),
            h('div', { style: 'position:absolute;right:0;top:36px;z-index:5;background:var(--card);border:1px solid var(--line-2);border-radius:10px;padding:8px;display:flex;flex-direction:column;gap:6px;min-width:200px' },
              exportLink(id, 'CSV, one row per phase', { format: 'csv', detail: 'phases' }), exportLink(id, 'CSV, one row per turn', { format: 'csv', detail: 'turns' }))))),
      h('div.stats',
        stat('Suite', run.suite_name || run.suite_id), stat('Started', run.started_at ? ago(run.started_at) : DASH),
        stat('Took', run.started_at && run.finished_at ? dur((run.finished_at - run.started_at) / 1000) : run.finished_at ? DASH : 'Not finished'),
        stat('Benchmarks', num(sum.steps)), stat('Passed', num(sum.passed)), stat('Failed', num(sum.failed), null, { cls: sum.failed ? 'err-t' : '' }),
        stat('Regressions', num(sum.regressions), null, { cls: sum.regressions ? 'err-t' : '' })),
      run.note || run.baseline_run_id ? h('div.cb', h('dl.kv',
        run.note ? [h('dt', 'Note'), h('dd', run.note)] : null,
        run.baseline_run_id ? [h('dt', 'Compared with'), h('dd', h('a.mono', { href: href('benchmarks', ['runs', run.baseline_run_id]) }, run.baseline_run_id))] : null)) : null),
    envCard(run),
    steps.length ? steps.map(stepCard) : h('section.card', empty('No benchmark has finished yet', run.state === 'queued' ? 'The run is waiting to start.' : 'The results appear here as each benchmark finishes.')),
  ];
}
