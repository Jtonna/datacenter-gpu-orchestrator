// Results per model (boards Bench-Overview "Latest result for each model"
// and Bench-Runs-Load, the result views of the Runs page): GET
// /api/bench/results/models. Each COLUMN shows the newest step that measured
// that figure (reviewer's ruling A, 26 Nov: a newer context fill does not
// blank the load columns); the figure links to that step's report and its
// hover gives the run number and date. A row whose figures come from more
// than one run carries a small "several runs" mark. A figure no step measured
// is a dash; when the newest step failed or was refused and no figure exists,
// the first figure column says so. A model file no longer in the models
// folder says "file no longer present" (or "in deleted models"), sorted last.
//
// resultsData(), loadResults(d): the page's copy, read on entry.
// latestTable(d): the Overview's table.
// resultsView(d, show): the Runs page's view (load-times, generation,
//   first-token, memory). The Coding view stays greyed (benchmarks slice 4).

import { h } from '../../core/dom.js';
import { href } from '../../core/router.js';
import * as api from '../../core/api.js';
import { skeleton, empty } from '../../core/ui.js';
import { num, dur, dateTime, DASH } from '../../core/format.js';
import { modelName, zonesWord, notYet, WHY } from './common.js';

export function resultsData() { return { rows: null, err: '', loading: false }; }
export async function loadResults(d) {
  if (d.loading) return false;
  d.loading = true;
  try { const r = await api.get('/api/bench/results/models'); d.rows = r.models || []; d.err = ''; } catch (e) { d.err = e.message; d.rows = d.rows || []; }
  d.loading = false;
  return true;
}

const NOT = 'not measured in this run';
const secs = v => (v < 120 ? num(v, 1) + ' s' : dur(v));
const FIG = {
  cold_load_s: secs, reload_s: secs,
  decode_tps: v => num(v, 1),
  ttft_ms: v => num(v / 1000, 2) + ' s',
  memory_mib: v => num(Math.round(v)) + ' MiB',
  lowest_available_mib: v => num(Math.round(v)) + ' MiB',
};
const OUTCOME = { failed: 'failed', error: 'failed', refused: 'refused', refused_by_exclusion: 'not judged', cancelled: 'cancelled' };

const reportOf = c => href('benchmarks', ['runs', c.run_id, String(c.step)]);
// the step a figure was read from (the server's from[key]; the cell itself before it said)
const srcOf = (cell, key) => (cell.from && cell.from[key]) || { run_id: cell.run_id, step: cell.step, at: cell.at, node_count: cell.node_count };
const runWord = f => (f.run_number ? `run ${num(f.run_number)}` : `run ${f.run_id}`) + (f.at ? `, ${dateTime(f.at)}` : '');

// fig(cell, key, first): one figure; first: the row's first figure column,
// where a step that did not pass says what happened
function fig(cell, key, first) {
  if (!cell) return h('span.mut', { title: 'not run on this many zones' }, DASH);
  const v = cell[key];
  if (v == null) {
    if (first && OUTCOME[cell.outcome]) {
      return h('a', { class: 'bp-out ' + (cell.outcome === 'refused' || cell.outcome === 'refused_by_exclusion' ? 'ref' : 'bad'), href: reportOf(cell), title: cell.reason || OUTCOME[cell.outcome],
        'data-outcome': cell.outcome }, OUTCOME[cell.outcome]);
    }
    return h('span.mut', { title: NOT, 'data-missing': key }, DASH);
  }
  const src = srcOf(cell, key);
  const cold = key === 'cold_load_s' && cell.cold ? (cell.cold_verified ? ', cold verified' : ', cold not verified') : '';
  const where = key === 'lowest_available_mib' && cell.lowest_available_node ? `${cell.lowest_available_node}, the lowest of the machines the step used; ` : '';
  return h('a.bp-fv', { href: reportOf(src), 'data-run': src.run_id, title: `${where}${runWord(src)}; ${zonesWord(src.node_count)}${cold}; open the report` }, FIG[key](v));
}

const atZones = (m, n) => (m.by_nodes || []).find(c => c.node_count === n) || null;
const atFit = m => (m.smallest_fit ? atZones(m, m.smallest_fit) : null) || m.latest;
const fitWord = m => (m.smallest_fit ? zonesWord(m.smallest_fit) : h('span.mut', { title: 'no load of this file has completed yet' }, DASH));
// a model whose file is no longer in the models folder: its results stay
// (they were measured), said so, and the server lists it last
function goneMark(m) {
  if (!m.file_gone) return null;
  if (m.trash_id) {
    return h('a.bp-sub.bp-gone', { 'data-gone': 'trash', href: href('models', ['deleted'], { item: m.trash_id }),
      title: 'the file is in Deleted models: restore it there to run it again; the figures are from when it was here' }, 'in deleted models');
  }
  return h('span.bp-sub.bp-gone', { 'data-gone': 'missing', title: 'the file is no longer in the models folder; the figures are from when it was' }, 'file no longer present');
}
// the row's mark when its figures come from more than one run: the runs on hover
function mixedMark(shown) {
  const runs = new Map();
  for (const [cell, key] of shown) if (cell && cell[key] != null) { const f = srcOf(cell, key); runs.set(f.run_id, f); }
  if (runs.size < 2) return null;
  const list = [...runs.values()].sort((a, b) => (b.at || 0) - (a.at || 0)).map(runWord).join('; ');
  return h('span.bp-sub.bp-mixed', { 'data-mixed': String(runs.size), title: 'the figures of this row come from several runs, each from the newest that measured it: ' + list }, 'several runs');
}
const nameCell = (m, shown) => h('td.bp-model', { title: m.model }, modelName(m.model), goneMark(m), mixedMark(shown || []));

function table(label, cols, rows, key) {
  return h('div.bp-tablewrap', h('table.bp-runs.bp-res-t', { 'aria-label': label, 'data-view': key || 'latest' },
    h('thead', h('tr', cols.map((c, i) => h('th', { class: i > 1 ? 'r' : null }, c)))), h('tbody', rows)));
}

// the most common context of the cells shown (the board's "context 8,192")
function ctxOf(rows) {
  const n = {};
  for (const m of rows) { const c = atFit(m); if (c && c.ctx) n[c.ctx] = (n[c.ctx] || 0) + 1; }
  const best = Object.keys(n).sort((a, b) => n[b] - n[a])[0];
  return best ? Number(best) : 0;
}

export function latestHead(d) {
  const c = d.rows && d.rows.length ? ctxOf(d.rows) : 0;
  return h('span.mut', { title: 'Each model at the fewest zones its load completed on, from its newest run there' }, 'Smallest fit' + (c ? ` · context ${num(c)}` : ''));
}

export function latestTable(d) {
  if (d.rows == null) return skeleton(4);
  if (d.err && !d.rows.length) return h('div.banner.err', { role: 'alert' }, d.err);
  if (!d.rows.length) return empty('No results yet', 'A model appears here once a benchmark step has run on it.');
  const cols = ['Model', 'Smallest fit', 'Cold load', 'Reload', 'Generation, tok/s', 'First token', 'Coding, of 102'];
  return table('Latest result for each model', cols, d.rows.map(m => {
    const c = atFit(m);
    const shown = c ? ['cold_load_s', 'reload_s', 'decode_tps', 'ttft_ms'].map(k => [c, k]) : [];
    return h('tr', { key: m.model, 'data-model': m.model, class: m.file_gone ? 'gone' : null },
      nameCell(m, shown), h('td', fitWord(m)),
      h('td.r', fig(c, 'cold_load_s', !m.file_gone)), h('td.r', fig(c, 'reload_s')), h('td.r', fig(c, 'decode_tps')), h('td.r', fig(c, 'ttft_ms')),
      h('td.r', notYet(DASH, WHY.coding)));
  }));
}

// the views of the Runs page: one figure at the smallest fit and on the
// most zones any model ran on (the board's "Cold, smallest" and "Cold, all 6")
export const RESULT_VIEWS = {
  'load-times': { label: 'Load times', key: 'cold_load_s', word: 'Cold' },
  generation: { label: 'Generation speed', key: 'decode_tps', word: 'tok/s' },
  'first-token': { label: 'First token', key: 'ttft_ms', word: 'First token' },
  memory: { label: 'Memory', key: 'memory_mib', word: 'Memory', extra: ['lowest_available_mib', 'Lowest available'] },
};

export function resultsView(d, show) {
  const v = RESULT_VIEWS[show];
  if (d.rows == null) return skeleton(6);
  if (d.err && !d.rows.length) return h('div.banner.err', { role: 'alert' }, d.err);
  if (!d.rows.length) return h('section.card', empty('No results yet', 'A model appears here once a benchmark step has run on it.'));
  const most = Math.max(0, ...d.rows.flatMap(m => (m.by_nodes || []).map(c => c.node_count)));
  const cols = ['Model', 'Smallest fit', `${v.word}, smallest`, most ? `${v.word}, all ${num(most)}` : `${v.word}, most zones`].concat(v.extra ? [v.extra[1]] : []);
  return table(v.label, cols, d.rows.map(m => {
    const small = m.smallest_fit ? atZones(m, m.smallest_fit) : null, all = most ? atZones(m, most) : null;
    const first = small || m.latest;
    const shown = [[first, v.key], [all, v.key]].concat(v.extra ? [[all || small || m.latest, v.extra[0]]] : []);
    return h('tr', { key: m.model, 'data-model': m.model, class: m.file_gone ? 'gone' : null },
      nameCell(m, shown), h('td', fitWord(m)),
      // a file no longer here: the row says so; a cell without a figure is a dash, not "failed"
      h('td.r', fig(first, v.key, !m.file_gone)),
      h('td.r', all ? fig(all, v.key, !m.file_gone) : h('span.mut', { title: `not run on ${zonesWord(most)}` }, 'not run')),
      v.extra ? h('td.r', fig(all || small || m.latest, v.extra[0])) : null);
  }), show);
}
