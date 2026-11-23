// The pass rate as a health monitor draws it: one pill per run, oldest left,
// newest right, with the percentage beside it. Used wherever a pass rate is
// shown: the Benchmarks overview, a suite's and a benchmark's page, the
// aggregates and the Benchmarks table of a model.
//
// The marks are the `recent` list the server sends with every aggregate
// (GET /api/bench/aggregates, and bench.detail of GET /api/models/<key>):
// [{run_id, at, result: pass | partial | fail | cancelled | none | not_judged, passed,
// failed, outcome}], oldest first. Each pill links to its run.

import { h } from './dom.js';
import { href } from './router.js';
import { num, agoText, dateTime } from './format.js';
import { dots, tipText } from './charts.js';

export const RESULT_WORD = { pass: 'Passed', partial: 'Partly passed', fail: 'Failed', cancelled: 'Cancelled', none: 'No result',
  not_judged: 'Not judged: refused because the profile forces an excluded node' };

// runMarks(recent) -> the marks of dots(): a tooltip and a link per run.
export function runMarks(recent) {
  return (recent || []).map(r => ({ result: r.result, href: href('benchmarks', ['runs', r.run_id]),
    tip: tipText(RESULT_WORD[r.result] || r.result, [`${agoText(r.at)} · ${dateTime(r.at)}`,
      r.passed != null || r.failed != null ? `${num(r.passed || 0)} passed · ${num(r.failed || 0)} failed` : r.outcome ? 'Outcome: ' + r.outcome : null,
      r.run_id, 'Click to open the run']) }));
}

// passDots(recent, rate, {label, empty, max, large}) -> the dots and the percentage.
export function passDots(recent, rate, o = {}) {
  const marks = runMarks(recent);
  if (!marks.length) return h('span.mut', o.empty || 'Not run yet');
  const d = dots(marks, { label: o.label || 'Runs, oldest first', max: o.max });
  if (o.large) d.classList.add('lg');
  // rate null (with runs): nothing was judged (every step refused by an
  // exclusion), so there is no rate to show, and certainly not 0 %.
  return h('span.rate', d, rate != null ? h('span.rate-n', { title: 'The share that passed, over the stored runs' }, num(rate * 100) + '%')
    : rate === null ? h('span.rate-n.mut', { title: 'Every run was refused because the profile forces a node that exclude_nodes names: nothing was judged' }, 'not judged') : null);
}
