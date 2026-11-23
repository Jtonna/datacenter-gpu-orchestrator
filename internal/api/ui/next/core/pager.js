// The numbered pager (design: Zones "Fleet", the foot of the zones table:
// "Page 1 of 15 ‹ 1 2 3 … 15 ›"; Benchmarks "Runs", the foot of the runs
// table: "1–12 of 49 ‹ 1 2 3 4 5 ›"). Square 26 px buttons with a 7 px
// radius; the current page dark (#131a17) and marked aria-current; the
// previous arrow disabled on the first page and the next one on the last.
// The count sits on the left of the buttons in muted text.
//
//   pager({page, pages, onPage, total, per, range, label})
//
// page is 1-based. With range (and total, per) the count reads "1–12 of 49"
// as on Benchmarks; without it "Page 1 of 15" as on Zones. onPage(n) is
// called with the page picked; the view keeps it in its address (?page=n,
// core/router.js) so it survives a reload. A single page draws the count
// alone.

import { h } from './dom.js';
import { num } from './format.js';

const ARROW = { prev: 'M12 5l-5 5 5 5', next: 'M8 5l5 5-5 5' };
const arrow = d => h('svg', { class: 'ic', viewBox: '0 0 20 20', 'aria-hidden': 'true' }, h('path', { d }));

// pageList(page, pages) -> the numbers to draw, with null for a gap ("…"):
// every page up to 7; else the first, the last, the current one and its
// neighbours, and the first three or last three at either end.
export function pageList(page, pages) {
  if (pages <= 7) return Array.from({ length: pages }, (_, i) => i + 1);
  const want = new Set([1, pages, page - 1, page, page + 1]);
  if (page <= 3) [1, 2, 3].forEach(n => want.add(n));
  if (page >= pages - 2) [pages - 2, pages - 1, pages].forEach(n => want.add(n));
  const ns = [...want].filter(n => n >= 1 && n <= pages).sort((a, b) => a - b);
  const out = [];
  ns.forEach((n, i) => { if (i && n - ns[i - 1] > 1) out.push(null); out.push(n); });
  return out;
}

export function pager(o) {
  const pages = Math.max(1, o.pages || 1);
  const page = Math.min(pages, Math.max(1, o.page || 1));
  const count = o.range && o.total != null && o.per
    ? (o.total ? `${num((page - 1) * o.per + 1)}–${num(Math.min(o.total, page * o.per))} of ${num(o.total)}` : 'None')
    : `Page ${num(page)} of ${num(pages)}`;
  const go = n => () => { if (n !== page && n >= 1 && n <= pages) o.onPage(n); };
  const btn = (label, n, attrs) => h('button', Object.assign({ class: 'pg', type: 'button', onclick: go(n) }, attrs), label);
  return h('nav.pager', { 'aria-label': o.label || 'Pages' },
    h('span.pager-count', count),
    pages > 1 ? [
      btn(arrow(ARROW.prev), page - 1, { 'aria-label': 'Previous page', disabled: page <= 1, 'data-page': 'prev' }),
      pageList(page, pages).map((n, i) => (n == null
        ? h('span.pager-gap', { key: 'gap' + i, 'aria-hidden': 'true' }, '…')
        : btn(num(n), n, { key: 'p' + n, 'aria-current': n === page ? 'page' : null, 'aria-label': 'Page ' + n, 'data-page': String(n) }))),
      btn(arrow(ARROW.next), page + 1, { 'aria-label': 'Next page', disabled: page >= pages, 'data-page': 'next' }),
    ] : null);
}
