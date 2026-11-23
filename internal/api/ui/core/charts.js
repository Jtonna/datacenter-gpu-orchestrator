// Charts are drawn here, as inline SVG or plain boxes; there is no chart
// library. The vocabulary is in ../README.md ("Charts"): colour is state (mint
// fine, coral failed, amber refused), never a series; the number of machines
// is a SHAPE or a count of pips; every mark has a tooltip (data-tip) and every
// chart a text alternative. Colours are class names from app.css: mint,
// amber, coral, lilac, mut.

import { h } from './dom.js';
import { num, clock, dateTime } from './format.js';

function extent(vals, pad = 0.08) {
  let lo = Math.min(...vals), hi = Math.max(...vals);
  if (!Number.isFinite(lo)) { lo = 0; hi = 1; }
  if (lo === hi) { lo -= Math.abs(lo) * 0.1 || 1; hi += Math.abs(hi) * 0.1 || 1; }
  const d = (hi - lo) * pad;
  return [lo - d, hi + d];
}

// sparkline([14.1, 14.4, 13.9], {width: 120, height: 34, color: 'mint',
//   flag: [indexes drawn as coral dots], label: 'aria text'})
// A line with the last point marked. Fewer than two values draws a flat rule.
export function sparkline(values, o = {}) {
  const w = o.width || 120, ht = o.height || 34, color = o.color || 'mint';
  const vals = (values || []).filter(v => v != null && !Number.isNaN(v));
  const svg = h('svg', { class: 'spark', width: w, height: ht, viewBox: `0 0 ${w} ${ht}`, role: 'img', 'aria-label': o.label || 'Trend' });
  if (vals.length < 2) {
    svg.appendChild(h('line', { x1: 1, y1: ht / 2, x2: w - 1, y2: ht / 2, class: 's-mut', 'stroke-width': 1.5, 'stroke-dasharray': '2 4' }));
    if (vals.length === 1) svg.appendChild(h('circle', { cx: w - 4, cy: ht / 2, r: 3, class: 'f-' + color }));
    return svg;
  }
  const [lo, hi] = extent(vals, 0.12);
  const x = i => 3 + (i * (w - 6)) / (vals.length - 1);
  const y = v => ht - 3 - ((v - lo) / (hi - lo)) * (ht - 6);
  svg.appendChild(h('polyline', { fill: 'none', class: 's-' + color, 'stroke-width': 2, 'stroke-linejoin': 'round', 'stroke-linecap': 'round',
    points: vals.map((v, i) => `${x(i).toFixed(1)},${y(v).toFixed(1)}`).join(' ') }));
  for (const i of o.flag || []) if (i >= 0 && i < vals.length) svg.appendChild(h('circle', { cx: x(i), cy: y(vals[i]), r: 3, class: 'f-coral' }));
  const last = vals.length - 1;
  svg.appendChild(h('circle', { cx: x(last), cy: y(vals[last]), r: 3, class: (o.flag || []).includes(last) ? 'f-coral' : 'f-' + color }));
  return svg;
}

function niceTicks(lo, hi, n = 4) {
  const span = hi - lo, step0 = span / n, mag = 10 ** Math.floor(Math.log10(step0));
  const step = [1, 2, 2.5, 5, 10].map(m => m * mag).find(s => s >= step0) || step0;
  const out = [];
  for (let v = Math.ceil(lo / step) * step; v <= hi + 1e-9; v += step) out.push(+v.toFixed(10));
  return out;
}

// A chart is drawn for a width in CSS pixels and only shrinks from there. On a
// phone it is drawn for the phone, so that its 11 px labels stay readable.
const phone = window.matchMedia('(max-width:700px)');
const fitW = w => (phone.matches ? Math.min(w, 340) : w);

const shortDate = v => dateTime(v).replace(/ \d{4},/, ',').replace(/:\d\d$/, '');

// lineChart({series:[{name, color:'mint', points:[{x: unixMs | number, y, tip, flag}]}],
//   width: 720, height: 220, unit: 's', time: true (x is a time), zero: true (y starts at 0), label})
// A small line chart with a labelled y axis, four grid lines and a dot per point
// (each dot has a tooltip with the exact value). It is drawn for `width` CSS
// pixels and never grows beyond that, so its 11 px labels stay 11 px in a wide
// card (in a narrower place it shrinks as a whole). The left margin is sized
// from the longest y label, unit included, so labels are never cut. Use a line
// only for one thing measured again and again (a benchmark over its runs).
export function lineChart(o) {
  const W = fitW(o.width || 720), H = o.height || 220, R = 12, T = 10, B = 26;
  const all = o.series.flatMap(s => s.points);
  if (!all.length) return noData(o.empty || 'No data yet');
  const svg = h('svg', { class: 'chart', viewBox: `0 0 ${W} ${H}`, role: 'img', 'aria-label': o.label || 'Chart', preserveAspectRatio: 'xMinYMid meet', style: `max-width:${W}px` });
  let L = 40;
  let [ylo, yhi] = extent(all.map(p => p.y));
  if (o.zero) ylo = 0;
  let xlo = Math.min(...all.map(p => p.x)), xhi = Math.max(...all.map(p => p.x));
  if (xlo === xhi) { xlo -= 1; xhi += 1; }
  const ticks = niceTicks(ylo, yhi);
  const tickLabel = t => num(t, Math.abs(t) < 10 && t % 1 ? 1 : 0) + (o.unit ? ' ' + o.unit : '');
  L = Math.max(L, Math.ceil(Math.max(...ticks.map(t => tickLabel(t).length)) * 6.7) + 14);
  const X = v => L + ((v - xlo) / (xhi - xlo)) * (W - L - R);
  const Y = v => H - B - ((v - ylo) / (yhi - ylo)) * (H - T - B);
  for (const t of ticks) {
    svg.appendChild(h('line', { class: 'gridline', x1: L, x2: W - R, y1: Y(t), y2: Y(t) }));
    svg.appendChild(h('text', { x: L - 8, y: Y(t) + 4, 'text-anchor': 'end' }, tickLabel(t)));
  }
  const xl = v => (o.time ? clock(v, false) : num(v));
  svg.appendChild(h('text', { x: L, y: H - 6, 'text-anchor': 'start' }, o.time ? shortDate(xlo) : xl(xlo)));
  svg.appendChild(h('text', { x: W - R, y: H - 6, 'text-anchor': 'end' }, o.time ? shortDate(xhi) : xl(xhi)));
  for (const s of o.series) {
    const pts = s.points.slice().sort((a, b) => a.x - b.x);
    if (pts.length > 1) svg.appendChild(h('polyline', { fill: 'none', class: 's-' + (s.color || 'mint'), 'stroke-width': 2, 'stroke-linejoin': 'round', 'stroke-linecap': 'round',
      points: pts.map(p => `${X(p.x).toFixed(1)},${Y(p.y).toFixed(1)}`).join(' ') }));
    for (const p of pts) {
      const tip = p.tip || tipText(`${num(p.y, 1)}${o.unit ? ' ' + o.unit : ''}`, [s.name, o.time ? dateTime(p.x) : null, p.title]);
      svg.appendChild(h('circle', { cx: X(p.x), cy: Y(p.y), r: p.flag ? 4.5 : 3.5, class: 'pt f-' + (p.flag ? 'coral' : p.color || s.color || 'mint'), 'data-tip': tip, tabindex: 0 }));
    }
  }
  return svg;
}

// barMeter(value, max, 'mint') -> a thin horizontal meter for a table cell.
export function barMeter(value, max, color = 'm') {
  const f = max > 0 ? Math.max(0, Math.min(1, value / max)) : 0;
  return h('div.bar.sm', { role: 'img', 'aria-label': `${num(value, 1)} of ${num(max, 1)}`, style: 'min-width:80px' }, h('span', { class: color, style: `width:${(f * 100).toFixed(1)}%` }));
}

// ---- tooltip: any element with data-tip shows it on hover and on focus ----
// The text is lines; the first is the title. One floating box for the page,
// found by delegation, so a redraw of the chart does not lose it.
let tipBox = null, tipFor = null;
function hideTip() { if (tipBox) tipBox.hidden = true; tipFor = null; }
function showTip(el) {
  const text = el.getAttribute('data-tip');
  if (!text) { hideTip(); return; }
  if (!tipBox) { tipBox = h('div.tip', { role: 'tooltip' }); document.body.appendChild(tipBox); }
  const lines = text.split('\n');
  tipBox.replaceChildren(h('b', lines[0]), ...lines.slice(1).filter(Boolean).map(l => h('span', l)));
  tipBox.hidden = false;
  tipFor = el;
  const r = el.getBoundingClientRect(), b = tipBox.getBoundingClientRect();
  let x = r.left + r.width / 2 - b.width / 2, y = r.top - b.height - 8;
  x = Math.max(8, Math.min(x, document.documentElement.clientWidth - b.width - 8));
  if (y < 8) y = r.bottom + 8;
  tipBox.style.left = Math.round(x + window.scrollX) + 'px';
  tipBox.style.top = Math.round(y + window.scrollY) + 'px';
}
const tipTarget = e => (e.target && e.target.closest ? e.target.closest('[data-tip]') : null);
document.addEventListener('mouseover', e => { const t = tipTarget(e); if (t) { if (t !== tipFor) showTip(t); } else hideTip(); });
document.addEventListener('focusin', e => { const t = tipTarget(e); if (t) showTip(t); });
document.addEventListener('focusout', hideTip);
document.addEventListener('scroll', hideTip, true);
document.addEventListener('keydown', e => { if (e.key === 'Escape') hideTip(); });
window.addEventListener('hashchange', hideTip);

// tipText('Loaded 12 min ago', ['On 3 PS5s', '28 s · 251 MB/s']) -> the data-tip value
export const tipText = (head, lines) => [head].concat((lines || []).filter(Boolean)).join('\n');

// viz(title, note, body, {legend, alt, key}) -> one chart tile: a heading, the
// chart, an optional legend line and the text alternative for a screen reader.
export function viz(title, note, body, o = {}) {
  return h('figure', { class: 'viz' + (o.cls ? ' ' + o.cls : ''), key: o.key },
    h('figcaption', h('b', title), note ? h('span', note) : null),
    h('div.viz-body', body),
    o.legend ? h('div.viz-legend', o.legend) : null,
    o.alt ? h('p.sr', o.alt) : null);
}

// noData('Not measured yet') -> the empty state of a chart, the same everywhere.
export const noData = (text = 'Not measured yet', sub) => h('div.viz-empty', h('span', text), sub ? h('small', sub) : null);

// A node count is a shape: 1 circle, 2 square, 3 triangle, more a diamond.
const SHAPES = ['circle', 'circle', 'square', 'tri', 'diamond'];
export const shapeOf = n => SHAPES[Math.max(0, Math.min(n || 0, 4))];
function shapeNode(shape, cx, cy, r, attrs) {
  if (shape === 'square') return h('rect', Object.assign({ x: cx - r, y: cy - r, width: 2 * r, height: 2 * r, rx: 1 }, attrs));
  if (shape === 'tri') return h('polygon', Object.assign({ points: `${cx},${cy - r - 1} ${cx + r + 1},${cy + r} ${cx - r - 1},${cy + r}` }, attrs));
  if (shape === 'diamond') return h('polygon', Object.assign({ points: `${cx},${cy - r - 1} ${cx + r + 1},${cy} ${cx},${cy + r + 1} ${cx - r - 1},${cy}` }, attrs));
  return h('circle', Object.assign({ cx, cy, r }, attrs));
}
export function shapeSwatch(shape, color = 'mint') {
  const s = h('svg', { class: 'swatch', width: 12, height: 12, viewBox: '0 0 12 12', 'aria-hidden': 'true' });
  s.appendChild(shapeNode(shape, 6, 6, 4, { class: 'f-' + color }));
  return s;
}
// legendItems([[swatchNode, 'label'], ...]) -> the spans of viz({legend})
export const legendItems = items => items.map(([sw, label]) => h('span', sw, label));
export const colorSwatch = (color, hollow) => h('i', { class: 'sw sw-' + color + (hollow ? ' hollow' : '') });

// pointChart({series: [{name, shape, points: [{x, y, color, tip}]}], unit,
//   zero: true, width, height, label, empty})
// Points over time with a labelled y axis. A series is a SHAPE (a node count),
// a point's colour is its outcome. No line joins the points: loads are
// separate events, not one quantity that moves.
export function pointChart(o) {
  const W = fitW(o.width || 560), H = o.height || 190, R = 14, T = 12, B = 26;
  const all = o.series.flatMap(s => s.points).filter(p => p.y != null && !Number.isNaN(p.y));
  if (!all.length) return noData(o.empty, o.emptySub);
  const svg = h('svg', { class: 'chart', viewBox: `0 0 ${W} ${H}`, role: 'img', 'aria-label': o.label || 'Chart', preserveAspectRatio: 'xMinYMid meet', style: `max-width:${W}px` });
  let [ylo, yhi] = extent(all.map(p => p.y), 0.12);
  if (o.zero !== false) ylo = 0;
  let xlo = Math.min(...all.map(p => p.x)), xhi = Math.max(...all.map(p => p.x));
  if (xlo === xhi) { xlo -= 60000; xhi += 60000; }
  const pad = (xhi - xlo) * 0.03;
  const ticks = niceTicks(ylo, yhi);
  const tickLabel = t => num(t, Math.abs(t) < 10 && t % 1 ? 1 : 0) + (o.unit ? ' ' + o.unit : '');
  const L = Math.max(40, Math.ceil(Math.max(...ticks.map(t => tickLabel(t).length)) * 6.7) + 14);
  const X = v => L + ((v - xlo + pad) / (xhi - xlo + 2 * pad)) * (W - L - R);
  const Y = v => H - B - ((v - ylo) / (yhi - ylo)) * (H - T - B);
  for (const t of ticks) {
    svg.appendChild(h('line', { class: 'gridline', x1: L, x2: W - R, y1: Y(t), y2: Y(t) }));
    svg.appendChild(h('text', { x: L - 8, y: Y(t) + 4, 'text-anchor': 'end' }, tickLabel(t)));
  }
  svg.appendChild(h('text', { x: L, y: H - 6, 'text-anchor': 'start' }, shortDate(xlo)));
  svg.appendChild(h('text', { x: W - R, y: H - 6, 'text-anchor': 'end' }, shortDate(xhi)));
  for (const s of o.series) {
    for (const p of s.points) {
      if (p.y == null || Number.isNaN(p.y)) continue;
      svg.appendChild(shapeNode(s.shape || 'circle', X(p.x), Y(p.y), 4, { class: 'pt f-' + (p.color || 'mint'), 'data-tip': p.tip || null, tabindex: p.tip ? 0 : null }));
    }
  }
  return svg;
}

// loadSpark(marks, {label}) -> the "last loads" strip of a model row: one bar
// per load, oldest left, newest right. The height is the load time, on the
// row's own scale. The pips under a bar count the machines it loaded onto (a
// load across three PS5s is a different thing from a load onto one, so a bar
// is never shown without them). Mint: loaded. Coral: failed or cancelled. A
// hollow amber stub: a request that was refused (there was no load).
// marks: [{outcome, load_s, nodes, tip}] newest first, as the server sends them.
export function loadSpark(marks, o = {}) {
  const list = (marks || []).slice().reverse();
  const slots = o.slots || 12, bw = 6, gap = 3, ht = 26, pipH = 10;
  const W = slots * (bw + gap) - gap, H = ht + pipH + 2;
  const svg = h('svg', { class: 'lspark', width: W, height: H, viewBox: `0 0 ${W} ${H}`, role: 'img', 'aria-label': o.label || 'Last loads' });
  const max = Math.max(1, ...list.map(m => (m.outcome === 'ready' ? m.load_s || 0 : 0)));
  const start = slots - list.length;
  svg.appendChild(h('line', { x1: 0, x2: W, y1: ht + 0.5, y2: ht + 0.5, class: 'base' }));
  list.forEach((m, i) => {
    const x = (start + i) * (bw + gap);
    const g = h('g', { 'data-tip': m.tip || null, class: 'mk' });
    if (m.outcome === 'refused') {
      g.appendChild(h('rect', { x: x + 0.75, y: ht - 6.25, width: bw - 1.5, height: 5.5, rx: 1.5, class: 'refused' }));
    } else {
      const ok = m.outcome === 'ready';
      const v = m.load_s > 0 ? Math.min(m.load_s, max) : max * 0.3;
      const bh = Math.max(3, (v / max) * (ht - 2));
      g.appendChild(h('rect', { x, y: ht - bh, width: bw, height: bh, rx: 1.5, class: ok ? 'f-mint' : 'f-coral' }));
      for (let k = 0; k < Math.min(m.nodes || 0, 3); k++) g.appendChild(h('rect', { x: x + 2, y: ht + 3 + k * 3, width: 2, height: 2, class: 'pip' }));
    }
    g.appendChild(h('rect', { x: x - gap / 2, y: 0, width: bw + gap, height: H, class: 'hit' }));
    svg.appendChild(g);
  });
  return svg;
}

// dots(marks, {max, label, empty}) -> the pass/fail history as a health
// monitor draws it: one pill per run, oldest left, newest right. marks:
// [{result: 'pass' | 'partial' | 'fail' | 'cancelled' | 'none', tip, href}].
// A pill with an href is a link to its run.
const DOT_WORD = { pass: 'passed', partial: 'partly passed', fail: 'failed', cancelled: 'cancelled', none: 'no result', not_judged: 'not judged' };
export function dots(marks, o = {}) {
  const list = (marks || []).slice(-(o.max || 30));
  if (!list.length) return h('span.mut', o.empty || 'Not run yet');
  const said = list.map(m => DOT_WORD[m.result] || m.result).join(', ');
  return h('span.dots', { role: 'group', 'aria-label': (o.label || 'Runs, oldest first') + ': ' + said },
    list.map((m, i) => (m.href
      ? h('a', { key: i, class: 'pd pd-' + (m.result || 'none'), href: m.href, 'data-tip': m.tip || null, 'aria-label': m.tip ? m.tip.split('\n').join(', ') : DOT_WORD[m.result], onclick: e => e.stopPropagation() })
      : h('i', { key: i, class: 'pd pd-' + (m.result || 'none'), 'data-tip': m.tip || null }))));
}

// compareBars([{label, value, text, tip}], {color, label}) -> a few horizontal
// bars on one scale (the largest value fills the track): 1 PS5, 2 PS5s, 3 PS5s.
// A row without a value says "not measured yet".
export function compareBars(rows, o = {}) {
  const max = Math.max(0, ...rows.map(r => r.value || 0));
  return h('div.cbars', { role: 'img', 'aria-label': o.label || '' }, rows.map((r, i) => h('div.cbar', { key: i, 'data-tip': r.tip || null },
    h('span.cl', r.label),
    r.value != null && max > 0
      ? h('span.ct', h('i', { class: o.color || 'm', style: `width:${Math.max(1.5, (r.value / max) * 100).toFixed(1)}%` }))
      : h('span.ct.none'),
    r.value != null ? h('span.cv', r.text) : h('span.cv.none', 'not measured yet'))));
}

// fitBar({need, budgets: [one, two, three machines together], label(n),
//   budgetText(mib), needTip, alt}) -> the memory a model needs against what
// one, two and three machines give. The track is the whole cluster; a tick
// marks where each machine count ends and the zone the need ends in is lit.
export function fitBar(o) {
  const budgets = (o.budgets || []).filter(b => b > 0);
  if (!o.need || !budgets.length) return noData('Not known yet', 'The memory need is known after the first load, or once a profile sets it.');
  const total = budgets[budgets.length - 1];
  const scale = Math.max(total, o.need) * 1.02;
  const fits = budgets.findIndex(b => b >= o.need);
  const pct = v => ((v / scale) * 100).toFixed(2) + '%';
  const span = i => `left:${pct(i ? budgets[i - 1] : 0)};width:${pct(budgets[i] - (i ? budgets[i - 1] : 0))}`;
  return h('div.fit', { role: 'img', 'aria-label': o.alt || '' },
    h('div.fit-track',
      budgets.map((b, i) => h('span', { key: 'z' + i, class: 'zone' + (i === fits ? ' on' : ''), style: span(i) })),
      h('span', { class: 'need' + (fits < 0 ? ' over' : ''), style: `width:${pct(o.need)}`, 'data-tip': o.needTip || null }),
      budgets.map((b, i) => h('i.tick', { key: 't' + i, style: `left:${pct(b)}` }))),
    h('div.fit-scale', budgets.map((b, i) => h('span', { key: i, class: i === fits ? 'on' : null, style: span(i) }, h('b', o.label(i + 1)), h('small', o.budgetText(b))))));
}

// columns(buckets, {series: [{key, color}], label(bucket), tip(bucket), height, alt, empty})
// -> small columns over time (one per hour or day), stacked from the series.
// The y scale starts at 0 and its top value is written; the first, the middle
// and the last bucket are labelled.
export function columns(buckets, o) {
  const H = o.height || 96, W = fitW(o.width || 560), L = 30, R = 6, T = 8, B = 20;
  const total = b => o.series.reduce((s, x) => s + (b[x.key] || 0), 0);
  const max = Math.max(0, ...buckets.map(total));
  if (!buckets.length || max <= 0) return noData(o.empty || 'Nothing in this time');
  const top = Math.max(niceTicks(0, max, 2).pop() || max, max);
  const svg = h('svg', { class: 'chart', viewBox: `0 0 ${W} ${H}`, role: 'img', 'aria-label': o.alt || 'Chart', preserveAspectRatio: 'xMinYMid meet', style: `max-width:${W}px` });
  const slot = (W - L - R) / buckets.length, bw = Math.max(3, Math.min(18, slot - 3));
  const Y = v => H - B - (v / top) * (H - T - B);
  for (const t of [0, top]) {
    svg.appendChild(h('line', { class: 'gridline', x1: L, x2: W - R, y1: Y(t), y2: Y(t) }));
    svg.appendChild(h('text', { x: L - 6, y: Y(t) + 4, 'text-anchor': 'end' }, num(t)));
  }
  buckets.forEach((b, i) => {
    const x = L + i * slot + (slot - bw) / 2;
    const g = h('g', { class: 'mk', 'data-tip': o.tip ? o.tip(b) : null });
    let acc = 0;
    for (const s of o.series) {
      const v = b[s.key] || 0;
      if (v <= 0) continue;
      const y1 = Y(acc + v), y0 = Y(acc);
      g.appendChild(h('rect', { x, y: y1, width: bw, height: Math.max(1.5, y0 - y1), rx: 1.5, class: 'f-' + s.color }));
      acc += v;
    }
    g.appendChild(h('rect', { x: L + i * slot, y: T, width: slot, height: H - T - B, class: 'hit' }));
    svg.appendChild(g);
    if (i === 0 || i === buckets.length - 1 || i === Math.floor(buckets.length / 2)) {
      svg.appendChild(h('text', { x: L + i * slot + slot / 2, y: H - 5, 'text-anchor': i === 0 ? 'start' : i === buckets.length - 1 ? 'end' : 'middle' }, o.label(b)));
    }
  });
  return svg;
}
