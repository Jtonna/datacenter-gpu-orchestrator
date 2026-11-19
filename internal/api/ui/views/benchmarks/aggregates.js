// Aggregates: what the stored runs add up to, per benchmark, per suite and per
// model on 1, 2 and 3 workers, and a trend chart for one benchmark and metric.

import { h } from '../../core/dom.js';
import * as api from '../../core/api.js';
import { href } from '../../core/router.js';
import { badge, table, chips, empty, skeleton, stat, toggle } from '../../core/ui.js';
import { sparkline, lineChart } from '../../core/charts.js';
import { num, dur, ago, plural, DASH } from '../../core/format.js';
import { cache, METRICS, metricDef, fmtMetric, changeMark, rateBadge, runBadge, withUnit } from './common.js';

// The view's own state: it survives re-drawing and tab changes within the screen.
export const aggState = { same: false, benchmark: '', metric: 'decode_tps', trend: null, trendFor: '', trendErr: null, loadingTrend: false, modelMetric: 'load_s' };

export function loadTrend(redraw) {
  const key = aggState.benchmark + '|' + aggState.metric;
  if (!aggState.benchmark || aggState.trendFor === key) return;
  aggState.trendFor = key;
  aggState.loadingTrend = true;
  api.get('/api/bench/trends', { benchmark: aggState.benchmark, metric: aggState.metric, limit: 60 })
    .then(r => { if (aggState.trendFor === key) { aggState.trend = r; aggState.trendErr = null; } })
    .catch(e => { if (aggState.trendFor === key) { aggState.trend = null; aggState.trendErr = e.message; } })
    .finally(() => { if (aggState.trendFor === key) { aggState.loadingTrend = false; redraw(); } });
}

export const currentAgg = () => cache.agg[aggState.same ? 'same' : 'all'];

const METRIC_ORDER = METRICS.map(m => m.key);

function trendCard(agg, redraw) {
  const benches = (agg && agg.benchmarks) || [];
  if (!aggState.benchmark && benches.length) aggState.benchmark = benches[0].benchmark_id;
  const b = benches.find(x => x.benchmark_id === aggState.benchmark);
  const have = b ? METRIC_ORDER.filter(k => b.metrics && b.metrics[k]) : [];
  if (b && have.length && !have.includes(aggState.metric)) aggState.metric = have[0];
  loadTrend(redraw);
  const def = metricDef(aggState.metric);
  let chart;
  if (aggState.trendErr) chart = empty('The trend could not be read', aggState.trendErr);
  else if (!aggState.trend) chart = h('div', skeleton(3));
  else if (!aggState.trend.points.length) chart = empty('No figures yet', 'Only runs where the benchmark loaded and served count.');
  else {
    const m = b && b.metrics && b.metrics[aggState.metric];
    const pts = aggState.trend.points.map((p, i, arr) => ({ x: p.started_at, y: p.value, title: p.run_id, flag: !!(m && m.regression && i === arr.length - 1) }));
    chart = h('div.cb', lineChart({ series: [{ name: def.label, color: 'mint', points: pts }], time: true, height: 240, unit: def.unit, width: 760, label: `${def.label} of ${aggState.benchmark} over time, in ${def.unit}` }));
  }
  return h('section.card', { key: 'trend' },
    h('div.ch', h('h2', 'Trend'), h('span.note', `One benchmark, one figure, over its runs. The axis is in ${def.unit}.`),
      h('span.right.row', { style: 'gap:8px' },
        h('select.select', { 'aria-label': 'Benchmark', style: 'width:auto;min-width:180px', onchange: e => { aggState.benchmark = e.target.value; redraw(); } },
          benches.map(x => h('option', { value: x.benchmark_id, selected: x.benchmark_id === aggState.benchmark }, x.name || x.benchmark_id))),
        h('select.select', { 'aria-label': 'Figure', style: 'width:auto;min-width:180px', onchange: e => { aggState.metric = e.target.value; redraw(); } },
          (have.length ? have : [aggState.metric]).map(k => h('option', { value: k, selected: k === aggState.metric }, `${metricDef(k).label} (${metricDef(k).unit})`))))),
    benches.length ? chart : empty('Nothing to chart yet', 'Run a suite; each finished run adds a point.'));
}

function suitesCard(agg) {
  const suites = (agg && agg.suites) || [];
  return h('section.card', { key: 'suites-agg' },
    h('div.ch', h('h2', 'Suites'), h('span.note', 'Share of runs that completed with no failed benchmark')),
    suites.length ? table({
      minWidth: 640, rows: suites, key: s => s.suite_id,
      columns: [
        { label: 'Suite', render: s => s.name || s.suite_id },
        { label: 'Runs', cls: 'r', render: s => num(s.runs) },
        { label: 'Pass rate', cls: 'tight', render: s => rateBadge(s.pass_rate) },
        { label: 'Mean duration', cls: 'r', render: s => dur(s.mean_duration_s) },
        { label: 'Last run', cls: 'tight', render: s => (s.last_run ? h('a', { href: href('benchmarks', ['runs', s.last_run.id]), style: 'color:inherit' },
          h('span.row', { style: 'gap:8px' }, runBadge(s.last_run.state), s.last_run.regression ? badge('Regression', 'aside') : null), h('span.sub', ago(s.last_run.started_at))) : DASH) },
      ],
    }) : empty('No suite has finished a run yet', 'The summary appears after the first run ends.'));
}

function metricRows(b) {
  return METRIC_ORDER.filter(k => b.metrics && b.metrics[k]).map(k => ({ key: k, m: b.metrics[k], bench: b }));
}

function benchCard(b, redraw) {
  const rows = metricRows(b);
  const out = b.outcomes || {};
  const odd = Object.keys(out).filter(k => k !== 'ready' && out[k]).map(k => `${out[k]} ${k}`);
  return h('section.card', { key: 'bagg-' + b.benchmark_id },
    h('div.ch', h('h2', b.name || b.benchmark_id), rateBadge(b.pass_rate), h('span.mono.mut', b.profile),
      h('span.note', `${plural(b.runs, 'run')}` + (odd.length ? ' · ' + odd.join(', ') : '')),
      b.last_at ? h('span.right.mut', { style: 'font-size:12px' }, 'Last ', ago(b.last_at)) : null),
    rows.length ? table({
      minWidth: 860, rows, key: r => r.key,
      onRow: r => { aggState.benchmark = b.benchmark_id; aggState.metric = r.key; redraw(); window.scrollTo({ top: 0, behavior: 'smooth' }); },
      rowClass: r => (r.key === aggState.metric && b.benchmark_id === aggState.benchmark ? 'sel' : ''),
      columns: [
        { label: 'Figure', render: r => h('span', { style: 'font-weight:600' }, metricDef(r.key).label, h('span.sub', metricDef(r.key).better === 'lower' ? 'Lower is better' : 'Higher is better')) },
        { label: 'Trend', cls: 'tight', render: r => {
          const pts = (r.m.points || []).map(p => p.value);
          return sparkline(pts, { width: 120, height: 30, color: r.m.regression ? 'coral' : 'mint', flag: r.m.regression ? [pts.length - 1] : [], label: `${metricDef(r.key).label}, last ${pts.length} runs` });
        } },
        { label: 'Last', cls: 'r', render: r => `${fmtMetric(r.key, r.m.last)} ${metricDef(r.key).unit}` },
        { label: 'Median', cls: 'r', render: r => fmtMetric(r.key, r.m.median) },
        { label: 'Min', cls: 'r', render: r => fmtMetric(r.key, r.m.min) },
        { label: 'Max', cls: 'r', render: r => fmtMetric(r.key, r.m.max) },
        { label: 'Best', cls: 'r', render: r => fmtMetric(r.key, r.m.best) },
        { label: 'Last vs median', cls: 'tight', title: 'The last run against the median of the earlier ones', render: r => changeMark(r.key, r.m.change_pct, r.m.regression) },
        { label: 'Runs', cls: 'r', render: r => num(r.m.count) },
      ],
    }) : empty('No figures yet', 'This benchmark has no run in which it loaded and served.'));
}

function modelNodesCard(agg, redraw) {
  const groups = (agg && agg.by_model_nodes) || [];
  const keys = ['load_s', 'load_mb_s', 'decode_tps', 'prefill_tps', 'ttft_ms', 'memory_mib'];
  const mk = aggState.modelMetric;
  const counts = Array.from(new Set(groups.map(g => g.nodes == null ? 0 : g.nodes))).sort((a, b) => a - b);
  const models = Array.from(new Set(groups.map(g => g.model_file)));
  const cell = (model, n) => {
    const g = groups.find(x => x.model_file === model && (x.nodes == null ? 0 : x.nodes) === n);
    const m = g && g.metrics && g.metrics[mk];
    if (!m) return h('span.mut', DASH);
    return h('span', fmtMetric(mk, m.median), h('span.sub', `${plural(m.count, 'run')}`));
  };
  const label = n => (n === 0 ? 'Local' : plural(n, 'worker'));
  return h('section.card', { key: 'bymodel' },
    h('div.ch', h('h2', 'Same model on 1, 2 and 3 workers'), h('span.note', `Median ${metricDef(mk).label.toLowerCase()} in ${metricDef(mk).unit}`),
      h('span.right', chips(keys.map(k => ({ id: k, label: metricDef(k).label })), mk, id => { aggState.modelMetric = id; redraw(); }, 'Figure to compare'))),
    models.length ? table({
      minWidth: 560, rows: models, key: m => m,
      columns: [{ label: 'Model file', render: m => h('span.mono.wrap-any', m) }].concat(counts.map(n => ({ label: label(n), cls: 'r', render: m => cell(m, n) }))),
    }) : empty('No model has a run yet', 'Run the same model on different numbers of workers to compare them here.'));
}

export function aggregatesView(redraw) {
  const agg = currentAgg();
  const err = cache.errors.agg;
  const controls = h('section.card', { key: 'agg-controls' },
    h('div.ch',
      h('label.check', toggle(aggState.same, v => { aggState.same = v; aggState.trendFor = ''; redraw(); }, 'Only runs on the current setup'), h('span', { style: 'font-weight:700' }, 'Only runs on the current setup')),
      h('span.note', agg ? `${plural(agg.runs_considered, 'run')} counted` + (aggState.same ? ': same scheduler, configuration, llama binary and workers as the newest run' : '') : ''),
      ));
  if (err && !agg) return [controls, h('section.card', empty('The aggregates could not be read', err.message))];
  if (!agg) return [controls, h('section.card', skeleton(5))];
  if (!agg.runs_considered) return [controls, h('section.card', empty('No finished runs yet', aggState.same ? 'No run matches the current setup. Turn the switch off to see all runs.' : 'Run a suite. Once a run has ended, its figures are added up here.'))];
  return [controls, trendCard(agg, redraw), suitesCard(agg), (agg.benchmarks || []).map(b => benchCard(b, redraw)), modelNodesCard(agg, redraw)];
}
