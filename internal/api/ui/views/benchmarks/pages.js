// A suite's own page (#/benchmarks/suite/<id>) and a benchmark's own page
// (#/benchmarks/benchmark/<id>): the run history first, a trend over the
// runs, what it contains or expects, and an "Open configuration" action that
// opens the editor as a panel over the page (?edit=1).
//
// Reads: REST GET /api/bench/runs?suite=<id> (a suite's runs), GET
// /api/bench/benchmarks/<id>/results (a benchmark in each stored run), the
// aggregates (pass/fail history) and the definitions from the shared cache.

import { h } from '../../core/dom.js';
import * as api from '../../core/api.js';
import { href, navigate } from '../../core/router.js';
import { badge, button, table, empty, skeleton, stat, backLink, banner } from '../../core/ui.js';
import { num, dur, ago, agoText, dateTime, clock, plural, gb, DASH } from '../../core/format.js';
import { machines } from '../../core/status.js';
import { state } from '../../core/store.js';
import { viz, lineChart, columns, tipText, legendItems, colorSwatch } from '../../core/charts.js';
import { passDots, RESULT_WORD } from '../../core/rate.js';
import { catalog } from '../../core/catalog.js';
import { avatar } from '../../core/avatar.js';
import { cache, suiteById, benchmarkById, suiteTitle, suiteStatusBadge, suiteNotRunnable, runBadge, phaseChips, scheduleText, describe, modelOf, outcomeBadge, signedPct } from './common.js';
import { runSuite, suiteBenchmarks } from './runconfirm.js';
import { loadModeNode, rolesText, reloadAmount, cacheOccupied, RELOAD_TIP } from '../../core/stepfacts.js';

// The page's own data, kept while the screen is open.
export const pageData = { suiteRuns: {}, results: {}, errors: {} };

export async function loadSuiteRuns(id, redraw) {
  try { pageData.suiteRuns[id] = (await api.get('/api/bench/runs', { suite: id, limit: 200 })).runs || []; pageData.errors['s:' + id] = ''; } catch (e) { pageData.errors['s:' + id] = e.message; }
  redraw();
}
export async function loadResults(id, redraw) {
  try { pageData.results[id] = (await api.get('/api/bench/benchmarks/' + encodeURIComponent(id) + '/results')).results || []; pageData.errors['b:' + id] = ''; } catch (e) { pageData.errors['b:' + id] = e.message; }
  redraw();
}

const RESULT_BADGE = { pass: ['Passed', 'ready'], partial: ['Partly passed', 'load'], fail: ['Failed', 'fail'], cancelled: ['Cancelled', 'n'], none: ['No result', 'n'], not_judged: ['Not judged (excluded node)', 'n'] };
const RESULT_COLOR = { pass: 'mint', partial: 'amber', fail: 'coral', cancelled: 'mut', none: 'mut', not_judged: 'mut' };
const resultBadge = r => { const [t, k] = RESULT_BADGE[r] || [r, 'n']; return badge(t, k); };

// The result of a run of a suite, as the server judges it for the dots.
function runResult(r) {
  if (r.state === 'cancelled') return 'cancelled';
  if (!r.summary || ['queued', 'running', 'paused', 'skipped'].includes(r.state)) return r.state === 'failed' ? 'fail' : 'none';
  if (!r.summary.ran && r.summary.refused_by_exclusion > 0 && r.state === 'completed') return 'not_judged';
  if (r.summary.failed === 0 && r.state === 'completed') return 'pass';
  return r.summary.passed > 0 && r.summary.failed > 0 ? 'partial' : 'fail';
}
const runSeconds = r => (r.started_at && r.finished_at ? (r.finished_at - r.started_at) / 1000 : null);
const modelOfBench = bm => (bm && !bm.parallel ? catalog.byProfile[bm.profile] || catalog.byKey[modelOf(bm.profile)] || null : null);

// A change against the run before: "+12%" in the colour of its direction.
function versus(now, before, better) {
  if (now == null || before == null || !before) return h('span.mut', DASH);
  const pct = ((now - before) / before) * 100;
  if (Math.abs(pct) < 1) return h('span.mut', 'same');
  const worse = better === 'lower' ? pct > 0 : pct < 0;
  return h('span', { class: worse ? 'warn-t' : 'ok-t', style: 'font-weight:700' }, signedPct(pct));
}

// The node with the lowest available memory of a result among the nodes whose
// figure is of the given kind ('system', also rows without a kind, or 'gpu'), or ''.
function lowestNode(r, kind) {
  const m = (r.mem_low || []).filter(x => x.step_mib != null && (x.kind || 'system') === kind).sort((a, b) => a.step_mib - b.step_mib)[0];
  return m ? 'on ' + m.node : '';
}
// The reload figure of a result's workers: the worst one, as a short phrase.
function reloadCell(r) {
  const rs = (r.reload_from_cache || []).filter(x => x.verdict);
  if (!rs.length) return h('span.mut', DASH);
  const rank = { none: 0, partial: 1, unknown: 2, full: 3 };
  const w = rs.slice().sort((a, b) => (rank[a.verdict] ?? 2) - (rank[b.verdict] ?? 2))[0];
  return h('span', { title: RELOAD_TIP + (rs.length > 1 ? ' The node that keeps the least is shown.' : '') }, reloadAmount(w), rs.length > 1 ? h('span.sub', 'on ' + w.node) : null,
    rs.some(cacheOccupied) ? h('span.sub.warn-t', 'File cache nearly full: first reloads may be slower') : null);
}

const notFound = (what, id) => [backLink('Benchmarks', href('benchmarks')), h('section.card', empty(`There is no ${what} ${id}`, `It may have been deleted. Go back to Benchmarks to see the ones that exist.`))];

// ---------- a suite ----------

export function suitePage(id) {
  const back = backLink('Benchmarks', href('benchmarks'));
  if (!cache.suites) return [back, h('section.card', cache.errors.suites ? empty('The suite could not be read', cache.errors.suites.message) : skeleton(5))];
  const s = suiteById(id);
  if (!s) return notFound('suite', id);
  const st = state.status || {};
  const agg = ((cache.agg.all && cache.agg.all.suites) || []).find(x => x.suite_id === id);
  const benchAggs = (cache.agg.all && cache.agg.all.benchmarks) || [];
  const runs = pageData.suiteRuns[id]; // newest first
  const list = suiteBenchmarks(s);
  const done = (runs || []).filter(r => runSeconds(r) != null).slice().reverse(); // oldest first
  const running = state.bench.run && state.bench.run.suite_id === id;

  const head = h('div.page-head', { key: 'head' },
    h('h1', suiteTitle(s), suiteStatusBadge(s) ? [' ', suiteStatusBadge(s)] : null), h('span.sub', 'Suite · ', h('span.mono', s.id)),
    h('div.actions',
      h('a.btn', { href: href('benchmarks', ['suite', id], { edit: 1 }) }, 'Open configuration'),
      h('button', { class: 'btn pri', type: 'button', disabled: !!cache.off || !!suiteNotRunnable(s), title: suiteNotRunnable(s) || (state.bench.run ? 'A run is active; this one is queued behind it' : 'Run the suite now'), onclick: () => runSuite(s) }, running ? 'Run again' : 'Run')));

  const facts = h('section.card', { key: 'facts' },
    s.status ? h('div.cb', { style: 'padding-top:16px;padding-bottom:0' }, banner(s.status === 'blocked' ? 'warn' : 'info',
      [h('b', s.status === 'blocked' ? 'Blocked: ' : 'Planned: '), s.reason || '', ' It cannot be run until its status is removed.'])) : null,
    s.description ? h('div.cb', { style: 'padding-top:16px;padding-bottom:0' }, h('p', { style: 'color:var(--text-2);max-width:90ch' }, s.description)) : null,
    h('div.stats', { style: 'padding-top:16px' },
      stat('Benchmarks', num(list.length)),
      stat('Schedule', scheduleText(s.schedule), null, { small: true }),
      stat('Runs', agg ? num(agg.runs) : runs ? num(runs.length) : DASH),
      h('div.stat', h('span', 'Pass rate'), h('b', { style: 'display:flex;align-items:center;min-height:30px' }, passDots(agg && agg.recent, agg && agg.pass_rate, { large: true, label: 'Runs of this suite, oldest first', empty: 'Not run yet' }))),
      stat('Mean duration', agg && agg.mean_duration_s ? dur(agg.mean_duration_s) : DASH),
      stat('Last run', s.last_run ? ago(s.last_run.finished_at || s.last_run.started_at || s.last_run.queued_at) : 'never')));

  // run history
  let history;
  if (pageData.errors['s:' + id] && !runs) history = empty('The runs could not be read', pageData.errors['s:' + id]);
  else if (!runs) history = skeleton(4);
  else if (!runs.length) history = empty('This suite has not run yet', 'Press Run to start it. Every run is kept here with its results.');
  else {
    history = table({
      minWidth: 900, rows: runs, key: r => r.id, onRow: r => navigate('benchmarks', ['runs', r.id]),
      columns: [
        { label: 'When', cls: 'tight', render: r => h('span', ago(r.started_at || r.queued_at), r.trigger === 'schedule' ? h('span.sub', 'Scheduled') : null) },
        { label: 'State', cls: 'tight', render: r => runBadge(r.state) },
        { label: 'Result', cls: 'tight', render: r => (['queued', 'running', 'paused'].includes(r.state) ? h('span.mut', DASH) : resultBadge(runResult(r))) },
        { label: 'Passed', cls: 'r', render: r => (r.summary ? `${num(r.summary.passed)} of ${num(r.summary.steps)}` : DASH) },
        { label: 'Duration', cls: 'r', render: r => (runSeconds(r) != null ? dur(runSeconds(r)) : DASH) },
        { label: 'Against the run before', title: 'The duration and the number of passed benchmarks, compared with the run before this one', render: r => {
          const i = runs.indexOf(r), prev = runs.slice(i + 1).find(x => runSeconds(x) != null && x.summary);
          if (!prev || runSeconds(r) == null || !r.summary) return h('span.mut', prev ? DASH : 'The first run');
          const dp = r.summary.passed - prev.summary.passed;
          return h('span.row', { style: 'gap:6px 12px;flex-wrap:nowrap' }, h('span', 'Duration ', versus(runSeconds(r), runSeconds(prev), 'lower')),
            h('span.mut', '·'), dp === 0 ? h('span.mut', 'same passes') : h('span', { class: dp > 0 ? 'ok-t' : 'err-t', style: 'font-weight:700' }, `${dp > 0 ? '+' : '−'}${Math.abs(dp)} passed`),
            r.summary.regressions ? badge(plural(r.summary.regressions, 'regression'), 'aside') : null);
        } },
        { label: 'Note', render: r => (r.note ? h('span.quiet', { style: 'font-size:13px' }, r.note) : '') },
        { label: '', cls: 'tight', render: r => h('a', { href: href('benchmarks', ['runs', r.id]), onclick: e => e.stopPropagation() }, 'Open') },
      ],
    });
  }

  // trend over runs
  const durPts = done.map(r => ({ x: r.started_at, y: runSeconds(r), color: RESULT_COLOR[runResult(r)],
    tip: tipText(dur(runSeconds(r)), [RESULT_WORD[runResult(r)], `${agoText(r.started_at)} · ${dateTime(r.started_at)}`, r.summary ? `${num(r.summary.passed)} passed · ${num(r.summary.failed)} failed` : null, r.id]) }));
  const cols = done.slice(-30).map(r => ({ at: r.started_at, passed: (r.summary || {}).passed || 0, failed: (r.summary || {}).failed || 0, id: r.id }));
  const altDur = done.length ? 'Duration of the runs, oldest first: ' + done.map(r => dur(runSeconds(r))).join(', ') + '.' : 'No finished run yet.';
  const altCols = done.length ? 'Benchmarks passed per run, oldest first: ' + cols.map(c => `${c.passed} of ${c.passed + c.failed}`).join(', ') + '.' : 'No finished run yet.';
  const trend = h('section.card', { key: 'trend' },
    h('div.ch', h('h2', 'Over its runs'), h('span.note', done.length ? `${plural(done.length, 'finished run')}` : null)),
    h('div.viz-grid',
      viz('Duration', 'How long the whole run took, in seconds',
        lineChart({ series: [{ name: 'Duration', color: 'mut', points: durPts }], time: true, zero: true, unit: 's', width: 560, height: 180, label: altDur, empty: 'No finished run yet' }),
        { key: 'v-dur', alt: altDur, legend: done.length ? legendItems([[colorSwatch('mint'), 'Passed'], [colorSwatch('amber'), 'Partly passed'], [colorSwatch('coral'), 'Failed']]) : null }),
      viz('Benchmarks passed', 'Per run, oldest left',
        columns(cols, { series: [{ key: 'passed', color: 'mint' }, { key: 'failed', color: 'coral' }], height: 180, label: c => clock(c.at, false), alt: altCols, empty: 'No finished run yet',
          tip: c => tipText(`${num(c.passed)} of ${num(c.passed + c.failed)} passed`, [`${agoText(c.at)} · ${dateTime(c.at)}`, c.id]) }),
        { key: 'v-pass', alt: altCols, legend: done.length ? legendItems([[colorSwatch('mint'), 'Passed'], [colorSwatch('coral'), 'Failed']]) : null })));

  const contains = h('section.card', { key: 'contains' },
    h('div.ch', h('h2', 'What it runs'), h('span.note', `${plural(list.length, 'benchmark')}, in this order`)),
    list.length ? table({
      minWidth: 820, rows: list, key: (b, i) => (b.id || '') + i, onRow: b => navigate('benchmarks', ['benchmark', b.id]),
      columns: [
        { label: 'Benchmark', render: b => h('span.named', avatar(modelOfBench(b), 28), h('span.grow', h('a.name', { href: href('benchmarks', ['benchmark', b.id]), onclick: e => e.stopPropagation() }, b.name || b.id),
          b.description ? h('span.sub', b.description) : null)) },
        { label: 'Configuration', cls: 'tight', render: b => (b.parallel ? h('span.mut', 'Several') : h('span.mono.nowrap', b.profile)) },
        { label: 'Phases', render: b => phaseChips(b) },
        { label: 'Passed', cls: 'tight', render: b => { const a = benchAggs.find(x => x.benchmark_id === b.id); return passDots(a && a.recent, a && a.pass_rate, { label: `Runs of ${b.name || b.id}, oldest first` }); } },
      ],
    }) : empty('No benchmark in this suite', s.status ? 'A planned or blocked suite may list none yet.' : 'Open the configuration to add some.'));

  return [back, head, facts,
    h('section.card', { key: 'history' }, h('div.ch', h('h2', 'Run history'), h('span.note', 'Newest first. Open a run for every figure.')), history),
    trend, contains, st ? null : null];
}

// ---------- a benchmark ----------

const TREND = [
  ['load_s', 'Load time', 's', 'lower', v => dur(v)],
  ['load_mb_s', 'Upload speed', 'MB/s', 'higher', v => num(v) + ' MB/s'],
  ['decode_tps', 'Answer speed', 'tok/s', 'higher', v => num(v, 1) + ' tok/s'],
  ['memory_mib', 'Memory', 'GB', 'lower', v => gb(v)],
];

const EXPECT_WORD = {
  outcome: v => `The scheduler answers "${v}"`, max_load_s: v => `Loads within ${dur(v)}`, min_decode_tps: v => `Answers at ${num(v, 1)} tok/s or faster`,
  min_prefill_tps: v => `Reads the prompt at ${num(v)} tok/s or faster`, max_ttft_ms: v => `First token within ${num(v)} ms`, output_sha256: () => 'The output is the expected one',
  min_reused_tokens: v => `Reuses at least ${num(v)} prompt tokens`, max_concurrent_wall_s: v => `Simultaneous requests finish within ${dur(v)}`, max_errors: v => `At most ${plural(v, 'failed request')}`,
  max_unload_s: v => `Unloads within ${dur(v)}`,
};

export function benchmarkPage(id) {
  const back = backLink('Benchmarks', href('benchmarks'));
  if (!cache.benchmarks) return [back, h('section.card', cache.errors.benchmarks ? empty('The benchmark could not be read', cache.errors.benchmarks.message) : skeleton(5))];
  const bm = benchmarkById(id);
  if (!bm) return notFound('benchmark', id);
  const st = state.status || {};
  const model = modelOfBench(bm);
  const agg = ((cache.agg.all && cache.agg.all.benchmarks) || []).find(x => x.benchmark_id === id);
  const results = pageData.results[id]; // oldest first
  const last = results && results.length ? results[results.length - 1] : null;
  const suites = (cache.suites || []).filter(s => (s.benchmarks || []).some(b => (typeof b === 'string' ? b : b.id) === id));

  const head = h('div.mhead', { key: 'head' },
    avatar(model, 72),
    h('div.grow',
      h('div.row', { style: 'gap:10px 14px' }, h('h1', bm.name || bm.id), last ? resultBadge(last.result) : badge('Not run yet', 'n')),
      h('span.sub', 'Benchmark · ', h('span.mono', bm.id)),
      h('div.row', { style: 'gap:8px;margin-top:8px' }, h('a.btn.sm', { href: href('benchmarks', ['benchmark', id], { edit: 1 }) }, 'Open configuration'))));

  const facts = h('section.card', { key: 'facts' },
    h('div.cb', { style: 'padding-top:16px;padding-bottom:0' }, h('p', { style: 'color:var(--text-2);max-width:90ch' }, bm.description ? bm.description + ' ' : '', h('span.mut', describe(bm, model ? model.name : modelOf(bm.profile))))),
    h('div.stats', { style: 'padding-top:16px' },
      h('div.stat', h('span', 'Model'), h('b', { style: 'font-size:16px' }, model ? h('a', { href: href('models', [model.key]), style: 'color:inherit' }, model.name) : bm.parallel ? 'Several' : modelOf(bm.profile) || DASH)),
      h('div.stat', h('span', 'Configuration'), h('b.mono', { style: 'font-size:13px;font-weight:500' }, bm.parallel ? bm.parallel.join(' + ') : bm.profile)),
      stat('Runs', agg ? num(agg.runs) : results ? num(results.length) : DASH),
      h('div.stat', h('span', 'Pass rate'), h('b', { style: 'display:flex;align-items:center;min-height:30px' }, passDots(agg && agg.recent, agg && agg.pass_rate, { large: true, label: 'Runs of this benchmark, oldest first' }))),
      stat('Last run', last ? ago(last.at) : 'never')),
    h('div.cs', h('div.row', { style: 'gap:8px 14px' }, h('span.mut', { style: 'font-weight:700;font-size:13px' }, 'Phases'), phaseChips(bm),
      suites.length ? [h('span.mut', { style: 'font-weight:700;font-size:13px;margin-left:8px' }, 'Runs in'), suites.map(s => h('a', { key: s.id, href: href('benchmarks', ['suite', s.id]), style: 'font-weight:700' }, suiteTitle(s)))]
        : h('span.mut', { style: 'font-size:13px' }, 'It is in no suite, so it does not run. Add it to a suite to run it.'))));

  // the expectation, and whether the last run met it
  const keys = Object.keys(bm.expect || {});
  const checks = Object.fromEntries((((last || {}).expect || {}).checks || []).map(c => [c.name, c]));
  const expect = h('section.card', { key: 'expect' },
    h('div.ch', h('h2', 'What it expects'), h('span.note', keys.length ? (last ? 'With the result of the last run' : 'Not run yet, so nothing is checked yet') : null)),
    h('div.cb.col', { style: 'gap:8px' }, keys.length ? keys.map(k => {
      const c = checks[k];
      const text = (EXPECT_WORD[k] || (v => `${k}: ${v}`))(bm.expect[k]);
      return h('div.why', { key: k }, h('i', { class: c ? (c.pass ? 'm' : 'x') : '' }), h('span', text, c && c.detail ? h('span.mut', ' · ' + c.detail) : null),
        c ? badge(c.pass ? 'Met' : 'Not met', c.pass ? 'use' : 'aside') : h('span'));
    }) : h('p.mut', { style: 'font-weight:600' }, 'No expectation is set: a run passes when the model loads and answers. Open the configuration to add a limit, such as a longest load time.')));

  // trends
  let trends;
  if (!results) trends = h('div.cb', skeleton(3));
  else {
    const tiles = TREND.map(([key, title, unit, better, fmt]) => {
      const pts = results.filter(r => r[key] != null).map(r => ({ x: r.at, y: key === 'memory_mib' ? r[key] / 1024 : r[key], color: RESULT_COLOR[r.result],
        tip: tipText(fmt(r[key]), [RESULT_WORD[r.result], `${agoText(r.at)} · ${dateTime(r.at)}`, r.nodes ? 'On ' + machines(st, r.nodes) : null, r.run_id]) }));
      if (!pts.length) return null;
      const alt = `${title}, oldest first: ` + results.filter(r => r[key] != null).map(r => fmt(r[key])).join(', ') + '.';
      return viz(title, `In ${unit}; ${better} is better`, lineChart({ series: [{ name: title, color: 'mut', points: pts }], time: true, zero: true, unit, width: 560, height: 170, label: alt }), { key: 'v-' + key, alt });
    }).filter(Boolean);
    trends = tiles.length ? h('div.viz-grid', tiles) : h('div.cb', h('div.viz-empty', h('span', results.length ? 'No figure was measured' : 'Not run yet'),
      h('small', results.length ? 'This benchmark expects a refusal, or never loaded, so there is no load time or speed to chart.' : 'A point appears for every run of a suite that contains this benchmark.')));
  }

  // results across runs
  let rows;
  if (pageData.errors['b:' + id] && !results) rows = empty('The results could not be read', pageData.errors['b:' + id]);
  else if (!results) rows = skeleton(4);
  else if (!results.length) rows = empty('This benchmark has not run yet', suites.length ? 'Run a suite that contains it.' : 'Add it to a suite and run the suite.');
  else {
    const list = results.slice().reverse();
    rows = table({
      minWidth: 1320, rows: list, key: r => r.run_id, onRow: r => navigate('benchmarks', ['runs', r.run_id]),
      columns: [
        { label: 'When', cls: 'tight', render: r => ago(r.at) },
        { label: 'Result', cls: 'tight', render: r => h('span', resultBadge(r.result), r.regression ? [' ', badge('Regression', 'aside')] : null) },
        { label: 'Outcome', cls: 'tight', render: r => outcomeBadge(r.outcome) || h('span.mut', DASH) },
        { label: 'Suite', render: r => h('a', { href: href('benchmarks', ['suite', r.suite_id]), onclick: e => e.stopPropagation(), style: 'color:inherit' }, r.suite_name || r.suite_id) },
        { label: 'On', render: r => (r.host || (r.workers || []).length ? h('span', { style: 'font-size:13px' }, rolesText(r.host, r.workers))
          : r.nodes ? machines(st, r.nodes) : h('span.mut', DASH)) },
        { label: 'Load mode', cls: 'tight', title: 'How the weights reached the nodes', render: r => loadModeNode(r.load_mode) },
        { label: 'Load time', cls: 'r', render: r => (r.load_s != null ? dur(r.load_s) : DASH) },
        { label: 'Upload', cls: 'r', render: r => (r.load_mb_s != null ? num(r.load_mb_s) + ' MB/s' : DASH) },
        { label: 'Answer speed', cls: 'r', render: r => (r.decode_tps != null ? num(r.decode_tps, 1) + ' tok/s' : DASH) },
        { label: 'Memory', cls: 'r', render: r => (r.memory_mib != null ? gb(r.memory_mib) : DASH) },
        { label: 'Lowest available (system)', cls: 'r', title: 'The lowest available system memory of the nodes that use system memory for the model (consoles), during the benchmark, sampled about twice a second', render: r => (r.lowest_available_mib != null
          ? h('span', gb(r.lowest_available_mib), lowestNode(r, 'system') ? h('span.sub', lowestNode(r, 'system')) : null) : h('span.mut', 'Not recorded')) },
        { label: 'Lowest available (GPU)', cls: 'r', title: 'The lowest free memory of the nodes whose GPU has memory of its own, during the benchmark', render: r => (r.lowest_available_gpu_mib != null
          ? h('span', gb(r.lowest_available_gpu_mib), lowestNode(r, 'gpu') ? h('span.sub', lowestNode(r, 'gpu')) : null) : h('span.mut', DASH)) },
        { label: 'Can stay in the file cache', title: RELOAD_TIP, render: r => reloadCell(r) },
        { label: '', cls: 'tight', render: r => h('a', { href: href('benchmarks', ['runs', r.run_id]), onclick: e => e.stopPropagation() }, 'Open run') },
      ],
    });
  }

  return [back, head, facts,
    h('section.card', { key: 'results' }, h('div.ch', h('h2', 'Results, run by run'), h('span.note', 'Newest first')), rows),
    h('section.card', { key: 'trends' }, h('div.ch', h('h2', 'Over its runs'), h('span.note', 'One point per run; the colour is the result of that run')), trends),
    expect];
}
