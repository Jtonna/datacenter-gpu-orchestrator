// Results per model (boards Bench-Overview "Latest result for each model"
// and Bench-Runs-Load, the result views of the Runs page): GET
// /api/bench/results/models, per model file its newest finished step, the
// newest per zone count and the smallest fit (the fewest zones a load of the
// file completed on). Every figure links to the report it comes from; a
// figure the step did not measure is a dash with "not measured in this run";
// a step that failed or was refused says so in the first figure column.
//
// resultsData(), loadResults(d): the page's copy, read on entry.
// latestTable(d): the Overview's table.
// resultsView(d, show): the Runs page's view (load-times, generation,
//   first-token, memory). The Coding view stays greyed (benchmarks slice 4).

import { h } from '../../core/dom.js';
import { href } from '../../core/router.js';
import * as api from '../../core/api.js';
import { skeleton, empty } from '../../core/ui.js';
import { num, dur, DASH } from '../../core/format.js';
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
  const cold = key === 'cold_load_s' && cell.cold ? (cell.cold_verified ? ', cold verified' : ', cold not verified') : '';
  const where = key === 'lowest_available_mib' && cell.lowest_available_node ? `${cell.lowest_available_node}, the lowest of the machines the step used; ` : '';
  return h('a.bp-fv', { href: reportOf(cell), title: `${where}${zonesWord(cell.node_count)}${cold}; open the report` }, FIG[key](v));
}

const atZones = (m, n) => (m.by_nodes || []).find(c => c.node_count === n) || null;
const atFit = m => (m.smallest_fit ? atZones(m, m.smallest_fit) : null) || m.latest;
const fitWord = m => (m.smallest_fit ? zonesWord(m.smallest_fit) : h('span.mut', { title: 'no load of this file has completed yet' }, DASH));
// a model whose file is no longer in the models folder: its results stay
// (they were measured), said so, and the server lists it last
const nameCell = m => h('td.bp-model', { title: m.file_gone ? `${m.model}: this file is no longer in the models folder; the figures are from when it was` : m.model },
  modelName(m.model), m.file_gone ? h('span.bp-sub.bp-gone', { 'data-gone': '' }, 'file no longer here') : null);

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
    return h('tr', { key: m.model, 'data-model': m.model, class: m.file_gone ? 'gone' : null },
      nameCell(m), h('td', fitWord(m)),
      h('td.r', fig(c, 'cold_load_s', true)), h('td.r', fig(c, 'reload_s')), h('td.r', fig(c, 'decode_tps')), h('td.r', fig(c, 'ttft_ms')),
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
    return h('tr', { key: m.model, 'data-model': m.model, class: m.file_gone ? 'gone' : null },
      nameCell(m), h('td', fitWord(m)),
      h('td.r', small ? fig(small, v.key, true) : fig(m.latest, v.key, true)),
      h('td.r', all ? fig(all, v.key, true) : h('span.mut', { title: `not run on ${zonesWord(most)}` }, 'not run')),
      v.extra ? h('td.r', fig(all || small || m.latest, v.extra[0])) : null);
  }), show);
}
