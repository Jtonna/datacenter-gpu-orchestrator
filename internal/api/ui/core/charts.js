// Charts are inline SVG drawn here; there is no chart library.
// Colours are class names from app.css: mint, amber, coral, lilac, mut.

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

// lineChart({series:[{name, color:'mint', points:[{x: unixMs | number, y}]}],
//   width: 720, height: 220, unit: 's', time: true (x is a time), zero: true (y starts at 0), label})
// A small line chart with a labelled y axis, four grid lines and a dot per point
// (each dot has a <title> with the exact value). It is drawn for `width` CSS
// pixels and never grows beyond that, so its 11 px labels stay 11 px in a wide
// card (in a narrower place it shrinks as a whole). The left margin is sized
// from the longest y label, unit included, so labels are never cut.
export function lineChart(o) {
  const W = o.width || 720, H = o.height || 220, R = 12, T = 10, B = 26;
  const svg = h('svg', { class: 'chart', viewBox: `0 0 ${W} ${H}`, role: 'img', 'aria-label': o.label || 'Chart', preserveAspectRatio: 'xMinYMid meet', style: `max-width:${W}px` });
  const all = o.series.flatMap(s => s.points);
  let L = 40;
  if (!all.length) {
    svg.appendChild(h('text', { x: W / 2, y: H / 2, 'text-anchor': 'middle' }, 'No data yet'));
    return svg;
  }
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
  svg.appendChild(h('text', { x: L, y: H - 6, 'text-anchor': 'start' }, o.time ? dateTime(xlo).replace(/:\d\d$/, '') : xl(xlo)));
  svg.appendChild(h('text', { x: W - R, y: H - 6, 'text-anchor': 'end' }, o.time ? dateTime(xhi).replace(/:\d\d$/, '') : xl(xhi)));
  for (const s of o.series) {
    const pts = s.points.slice().sort((a, b) => a.x - b.x);
    if (pts.length > 1) svg.appendChild(h('polyline', { fill: 'none', class: 's-' + (s.color || 'mint'), 'stroke-width': 2, 'stroke-linejoin': 'round', 'stroke-linecap': 'round',
      points: pts.map(p => `${X(p.x).toFixed(1)},${Y(p.y).toFixed(1)}`).join(' ') }));
    for (const p of pts) {
      svg.appendChild(h('circle', { cx: X(p.x), cy: Y(p.y), r: p.flag ? 4 : 3, class: 'f-' + (p.flag ? 'coral' : s.color || 'mint') },
        h('title', `${s.name ? s.name + ': ' : ''}${num(p.y, 1)}${o.unit ? ' ' + o.unit : ''}${o.time ? ' at ' + dateTime(p.x) : ''}${p.title ? ' · ' + p.title : ''}`)));
    }
  }
  return svg;
}

// barMeter(value, max, 'mint') -> a thin horizontal meter for a table cell.
export function barMeter(value, max, color = 'm') {
  const f = max > 0 ? Math.max(0, Math.min(1, value / max)) : 0;
  return h('div.bar.sm', { role: 'img', 'aria-label': `${num(value, 1)} of ${num(max, 1)}`, style: 'min-width:80px' }, h('span', { class: color, style: `width:${(f * 100).toFixed(1)}%` }));
}
