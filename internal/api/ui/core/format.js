// Formatting: numbers always carry their unit, times read "38 min ago" with
// the absolute time on hover. All functions tolerate null and return "–".

import { h } from './dom.js';

export const DASH = '–';

let skewMs = 0; // server clock minus browser clock, set from the hello message
export function setServerTime(serverMs) { skewMs = serverMs - Date.now(); }
export function now() { return Date.now() + skewMs; }

// toMs accepts unix milliseconds, an RFC 3339 string or a Date.
export function toMs(t) {
  if (t == null || t === '') return null;
  if (typeof t === 'number') return t;
  if (t instanceof Date) return t.getTime();
  const ms = Date.parse(t);
  return Number.isNaN(ms) ? null : ms;
}

const nf0 = new Intl.NumberFormat('en-US', { maximumFractionDigits: 0 });
const nf1 = new Intl.NumberFormat('en-US', { minimumFractionDigits: 1, maximumFractionDigits: 1 });
const nf2 = new Intl.NumberFormat('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 });

export function num(v, digits = 0) {
  if (v == null || Number.isNaN(v)) return DASH;
  return (digits === 0 ? nf0 : digits === 1 ? nf1 : nf2).format(v);
}
export const mib = v => (v == null ? DASH : nf0.format(Math.round(v)) + ' MiB');
export function gib(v) { return v == null ? DASH : nf1.format(v / 1024) + ' GiB'; }
// bytes as decimal GB or MB, the way upload sizes are spoken about.
export function bytes(v) {
  if (v == null) return DASH;
  if (v >= 1e9) return nf2.format(v / 1e9) + ' GB';
  if (v >= 1e6) return nf0.format(v / 1e6) + ' MB';
  if (v >= 1e3) return nf0.format(v / 1e3) + ' kB';
  return nf0.format(v) + ' B';
}
export const pct = (v, digits = 0) => (v == null ? DASH : num(v, digits) + '%');
export const tps = v => (v == null || v <= 0 ? DASH : nf1.format(v));
export function ctx(v) {
  if (!v) return DASH;
  return v % 1024 === 0 ? v / 1024 + 'k' : nf0.format(v);
}

// dur(seconds) -> "41 s", "5 min 48 s", "1 h 22 min", "2 d 4 h".
export function dur(s) {
  if (s == null || Number.isNaN(s)) return DASH;
  s = Math.max(0, s);
  if (s < 10 && s !== Math.floor(s)) return nf1.format(s) + ' s';
  s = Math.round(s);
  if (s < 60) return s + ' s';
  if (s < 3600) { const m = Math.floor(s / 60), r = s % 60; return r ? `${m} min ${r} s` : `${m} min`; }
  if (s < 86400) { const hh = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60); return m ? `${hh} h ${m} min` : `${hh} h`; }
  const d = Math.floor(s / 86400), hh = Math.floor((s % 86400) / 3600);
  return hh ? `${d} d ${hh} h` : `${d} d`;
}
// durShort keeps one unit for tight places: "41 s", "6 min", "1 h", "2 d".
export function durShort(s) {
  if (s == null || Number.isNaN(s)) return DASH;
  s = Math.max(0, Math.round(s));
  if (s < 60) return s + ' s';
  if (s < 3600) return Math.floor(s / 60) + ' min';
  if (s < 86400) { const hh = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60); return m && hh < 10 ? `${hh} h ${m} min` : `${hh} h`; }
  return Math.floor(s / 86400) + ' d';
}

const p2 = n => String(n).padStart(2, '0');
export function clock(t, withSeconds = true) {
  const ms = toMs(t);
  if (ms == null) return DASH;
  const d = new Date(ms);
  return `${p2(d.getHours())}:${p2(d.getMinutes())}` + (withSeconds ? `:${p2(d.getSeconds())}` : '');
}
const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
export function dateTime(t) {
  const ms = toMs(t);
  if (ms == null) return DASH;
  const d = new Date(ms);
  return `${d.getDate()} ${MONTHS[d.getMonth()]} ${d.getFullYear()}, ${clock(ms)}`;
}
export function dayLabel(t) {
  const ms = toMs(t);
  if (ms == null) return DASH;
  const d = new Date(ms), n = new Date(now());
  const day = x => Math.floor((x.getTime() - x.getTimezoneOffset() * 60000) / 86400000);
  const diff = day(n) - day(d);
  if (diff === 0) return 'Today';
  if (diff === 1) return 'Yesterday';
  return `${d.getDate()} ${MONTHS[d.getMonth()]} ${d.getFullYear()}`;
}

export function agoText(t) {
  const ms = toMs(t);
  if (ms == null) return DASH;
  const s = (now() - ms) / 1000;
  if (s < 0) return 'in ' + durShort(-s);
  if (s < 2) return 'just now';
  return durShort(s) + ' ago';
}
// ago(t) -> <time title="19 Nov 2026, 16:14:02">38 min ago</time>; never(…) for null.
export function ago(t, fallback = DASH) {
  const ms = toMs(t);
  if (ms == null) return h('span.mut', fallback);
  return h('time', { datetime: new Date(ms).toISOString(), title: dateTime(ms) }, agoText(ms));
}
// until(t) -> <time title="…">in 5 min 42 s</time>
export function until(t, fallback = DASH) {
  const ms = toMs(t);
  if (ms == null) return h('span.mut', fallback);
  const s = (ms - now()) / 1000;
  return h('time', { datetime: new Date(ms).toISOString(), title: dateTime(ms) }, s <= 0 ? 'now' : 'in ' + dur(s));
}
// at(t) -> <time title="19 Nov 2026, 16:14:02">16:14:02</time>
export function at(t, withSeconds = true) {
  const ms = toMs(t);
  if (ms == null) return h('span.mut', DASH);
  return h('time', { datetime: new Date(ms).toISOString(), title: dateTime(ms) }, clock(ms, withSeconds));
}

export const plural = (n, one, many) => `${num(n)} ${n === 1 ? one : many || one + 's'}`;
export const list = arr => (arr.length <= 1 ? arr.join('') : arr.slice(0, -1).join(', ') + ' and ' + arr[arr.length - 1]);

// Go duration strings ("90s", "15m", "1h30m") to seconds and back, for forms.
export function parseGoDuration(v) {
  if (typeof v === 'number') return v;
  if (typeof v !== 'string' || !v.trim()) return null;
  let total = 0, matched = false;
  const re = /(\d+(?:\.\d+)?)(ms|s|m|h|d)/g;
  let m, rest = v.trim();
  while ((m = re.exec(v))) {
    matched = true;
    rest = rest.replace(m[0], '');
    total += parseFloat(m[1]) * { ms: 0.001, s: 1, m: 60, h: 3600, d: 86400 }[m[2]];
  }
  return matched && !rest.trim() ? total : null;
}
