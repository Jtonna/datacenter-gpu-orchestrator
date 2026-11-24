// The periods of the Overview (Day, Week, Month, All time) in the viewer's
// local time: where one starts and ends, its step, its name in the date
// stepper, the labels of its chart axis. A week starts on Monday. No DOM.

const WD = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
const MON = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
const MONTH = ['January', 'February', 'March', 'April', 'May', 'June', 'July', 'August', 'September', 'October', 'November', 'December'];

export const PERIODS = [{ id: 'day', label: 'Day' }, { id: 'week', label: 'Week' }, { id: 'month', label: 'Month' }, { id: 'all', label: 'All time' }];
export const isPeriod = p => PERIODS.some(x => x.id === p);

const p2 = n => String(n).padStart(2, '0');
// ymd(ms) -> "2026-11-17" (local).
export function ymd(ms) { const d = new Date(ms); return `${d.getFullYear()}-${p2(d.getMonth() + 1)}-${p2(d.getDate())}`; }
// parseYmd("2026-11-17") -> local midnight (ms), or null.
export function parseYmd(s) {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(String(s || ''));
  if (!m) return null;
  const d = new Date(+m[1], +m[2] - 1, +m[3]);
  return Number.isNaN(d.getTime()) ? null : d.getTime();
}
export function startOfDay(ms) { const d = new Date(ms); d.setHours(0, 0, 0, 0); return d.getTime(); }
export function addDays(ms, n) { const d = new Date(ms); d.setDate(d.getDate() + n); return d.getTime(); }
function addMonths(ms, n) { const d = new Date(ms); d.setDate(1); d.setMonth(d.getMonth() + n); return d.getTime(); }
function startOfWeek(ms) { const d = new Date(startOfDay(ms)); return addDays(d.getTime(), -((d.getDay() + 6) % 7)); }
function startOfMonth(ms) { const d = new Date(startOfDay(ms)); d.setDate(1); return d.getTime(); }
export const dayName = ms => { const d = new Date(ms); return `${WD[d.getDay()]} ${d.getDate()} ${MON[d.getMonth()]} ${d.getFullYear()}`; };
export const shortDay = ms => { const d = new Date(ms); return `${WD[d.getDay()]} ${d.getDate()} ${MON[d.getMonth()]}`; };

// periodOf(period, dateStr, now, oldest) -> {period, from, to, step, label,
// current (it holds now), unit ('per hour' | 'per day'), labels (one per
// bucket), buckets (their starts), prev, next (the date of the period before
// and after, YYYY-MM-DD), date (YYYY-MM-DD), stripDay (the local day the
// availability strip shows: the period's last day, today when current)}.
// oldest is the first time anything was recorded (All time starts on its day).
export function periodOf(period, dateStr, now, oldest) {
  const p = isPeriod(period) ? period : 'day';
  const anchor = Math.min(parseYmd(dateStr) ?? startOfDay(now), startOfDay(now));
  let from, to, step, label, unit, prev = null, next = null;
  if (p === 'day') {
    from = anchor; to = addDays(from, 1); step = 'hour'; unit = 'per hour';
    label = dayName(from); prev = addDays(from, -1); next = to;
  } else if (p === 'week') {
    from = startOfWeek(anchor); to = addDays(from, 7); step = 'day'; unit = 'per day';
    const a = new Date(from), b = new Date(addDays(to, -1));
    label = a.getMonth() === b.getMonth() ? `${a.getDate()}–${b.getDate()} ${MON[b.getMonth()]} ${b.getFullYear()}`
      : `${a.getDate()} ${MON[a.getMonth()]} – ${b.getDate()} ${MON[b.getMonth()]} ${b.getFullYear()}`;
    prev = addDays(from, -7); next = to;
  } else if (p === 'month') {
    from = startOfMonth(anchor); to = addMonths(from, 1); step = 'day'; unit = 'per day';
    const a = new Date(from);
    label = `${MONTH[a.getMonth()]} ${a.getFullYear()}`;
    prev = addMonths(from, -1); next = to;
  } else {
    from = startOfDay(oldest && oldest < now ? oldest : now); to = addDays(startOfDay(now), 1); step = 'day'; unit = 'per day';
    label = 'All time';
  }
  const current = from <= now && now < to;
  if (current) label += p === 'day' ? ' · today' : p === 'week' ? ' · this week' : p === 'month' ? ' · this month' : '';
  // the bucket starts and their axis labels
  const buckets = [], labels = [];
  for (let t = from; t < to; t = step === 'hour' ? t + 3600000 : addDays(t, 1)) {
    const d = new Date(t);
    buckets.push(t);
    if (step === 'hour') labels.push(d.getHours() % 6 === 0 ? p2(d.getHours()) : '');
    else if (p === 'week') labels.push(WD[d.getDay()]);
    else if (p === 'month') labels.push((d.getDate() - 1) % 7 === 0 ? String(d.getDate()) : '');
    else labels.push(d.getDate() === 1 || buckets.length === 1 ? `${d.getDate()} ${MON[d.getMonth()]}` : '');
    if (buckets.length > 2000) break;
  }
  // All time with many days: label only the first of a quarter's months, so the labels never crowd
  if (p === 'all' && buckets.length > 120) {
    buckets.forEach((t, i) => { const d = new Date(t); labels[i] = d.getDate() === 1 && d.getMonth() % 3 === 0 ? `${MON[d.getMonth()]} ${String(d.getFullYear()).slice(2)}` : ''; });
  }
  const stripDay = current ? startOfDay(now) : addDays(to, -1);
  return { period: p, from, to, step, label, unit, current, buckets, labels, date: ymd(anchor),
    prev: prev == null ? null : ymd(prev), next: next == null ? null : ymd(next), stripDay };
}

// bucketTitle(t, step) -> "10:00–11:00" or "Tue 17 Nov".
export function bucketTitle(t, step) {
  const d = new Date(t);
  if (step === 'hour') { const e = new Date(t + 3600000); return `${p2(d.getHours())}:00–${p2(e.getHours())}:00`; }
  return shortDay(t);
}

// The counts of a summary bucket (GET /api/requests/summary) that add up.
const COUNTS = ['requests', 'done', 'failed', 'refused', 'cancelled', 'tokens_in', 'tokens_reused', 'tokens_out', 'wait_ms', 'duration_ms'];

// foldLocal(buckets, to, answers) -> one summary whose series has a bucket per
// local day (buckets: their starts, from periodOf) from hourly answers of
// GET /api/requests/summary (step=hour; several when the period is longer
// than one answer may be). Each hour goes to the local day it starts in, so a
// 23 or 25 hour day (daylight saving) gets exactly its hours. The server's
// day step makes fixed 24-hour buckets, and older than the hourly roll-ups it
// answers UTC days; so the page asks for hours and folds them here. Rates
// (prefill_tps, decode_tps) cannot be summed: they stay only when there is
// one answer.
export function foldLocal(buckets, to, answers) {
  const zero = t => { const b = { t, prefill_tps: null, decode_tps: null }; for (const k of COUNTS) b[k] = 0; return b; };
  const series = buckets.map(zero);
  const totals = zero(0);
  delete totals.t;
  for (const a of answers) {
    for (const h of (a && a.series) || []) {
      if (!(h.t >= buckets[0] && h.t < to)) continue;
      let i = buckets.length - 1;
      while (i > 0 && buckets[i] > h.t) i--;
      for (const k of COUNTS) series[i][k] += h[k] || 0;
    }
    for (const k of COUNTS) totals[k] += (a && a.totals && a.totals[k]) || 0;
  }
  const last = answers[answers.length - 1] || {};
  if (answers.length === 1 && last.totals) { totals.prefill_tps = last.totals.prefill_tps ?? null; totals.decode_tps = last.totals.decode_tps ?? null; }
  return Object.assign({}, last, { from: buckets[0], to, step: 'day', totals, series });
}
