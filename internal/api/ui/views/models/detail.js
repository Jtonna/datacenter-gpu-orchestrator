// One model's page (#/models/<key>): what it is, the charts that say at a
// glance how it runs (the memory it needs against what one, two and three
// machines give, its load time and upload speed over time, one against two
// against three machines, its activity), whether it is loaded and where, how
// it has been used, the configurations (profiles) that load it, and the
// benchmarks that use it. Read-only apart from the icon: there is no load or
// unload control.
//
// Reads: REST GET /api/models/<key> (the catalog entry with its recent events,
// chart series and benchmarks) on open, and again when the catalog entry of
// this model changes (core/catalog.js, fed by the live channel); WebSocket
// "status" for the instance cards. Until the detail arrives the page draws
// from the catalog entry, which is usually in memory already.
// Writes: PUT and DELETE /api/models/<key>/icon (the owner's icon).

import { h } from '../../core/dom.js';
import { href } from '../../core/router.js';
import { state, listen } from '../../core/store.js';
import * as api from '../../core/api.js';
import { liveView, badge, nodeChips, stat, table, empty, skeleton, backLink, more, legend, dialog, toast } from '../../core/ui.js';
import { num, gb, gbNum, mib, bytes, ctx, dur, ago, plural, agoText, dateTime, clock, DASH } from '../../core/format.js';
import { staleBanner, machines, plainReason, plainBasis } from '../../core/status.js';
import { catalog, onCatalog, needCatalog } from '../../core/catalog.js';
import { avatar } from '../../core/avatar.js';
import { viz, pointChart, compareBars, fitBar, columns, shapeOf, shapeSwatch, colorSwatch, legendItems, tipText } from '../../core/charts.js';
import { passDots } from '../../core/rate.js';
import { instanceCard, sizeLine, stateBadge, fitOf, planCell } from './common.js';

// ---- charts: how it runs, at a glance ----

const OUTCOME_COLOR = { ready: 'mint', failed: 'coral', cancelled: 'coral' };

function loadPoint(l, st, value, text) {
  const head = l.outcome === 'ready' ? text : l.outcome === 'cancelled' ? 'Load cancelled' : 'Load failed';
  return { x: l.at, y: value, color: OUTCOME_COLOR[l.outcome] || 'mut',
    tip: tipText(head, [`${agoText(l.at)} · ${dateTime(l.at)}`, l.profile, 'On ' + machines(st, l.nodes), l.load_s != null && l.mb_s ? `${dur(l.load_s)} · ${num(l.mb_s)} MB/s` : null,
      l.bench_run ? 'A benchmark run' : null]) };
}

// Points by node count: a shape per count, so one chart holds them honestly.
function byNodes(loads, st, pick, text) {
  const counts = Array.from(new Set(loads.map(l => l.nodes))).sort((a, b) => a - b);
  return counts.map(n => ({ name: machines(st, n), shape: shapeOf(n), nodes: n,
    points: loads.filter(l => l.nodes === n && pick(l) != null).map(l => loadPoint(l, st, pick(l), text(l))) }));
}

const shapeLegend = (series, extra) => legendItems(series.map(s => [shapeSwatch(s.shape), s.name]).concat(extra || []));

function fitViz(m, st) {
  const f = m.fit || {};
  const fo = fitOf(m, st);
  const budgets = f.budgets_mib || [];
  const on = budgets.findIndex(b => b >= f.need_mib);
  const alt = !f.need_mib ? 'The memory need is not known yet.'
    : on < 0 ? `It needs ${gb(f.need_mib)}, more than the ${gb(budgets[budgets.length - 1])} the whole empty cluster gives.`
      : `It needs ${gb(f.need_mib)}. ` + budgets.map((b, i) => `${machines(st, i + 1)} give ${gb(b)}`).join(', ') + `. It fits on ${machines(st, on + 1)}.`;
  const verdict = !f.need_mib ? null
    : on < 0 ? h('span.err-t', 'Too big for this cluster')
      : h('span', h('b.ok-t', `Fits on ${machines(st, on + 1)}`), fo.ranOn ? h('span.mut', ` · measured on ${fo.ranOn}, not run on ${on + 1} yet`) : fo.sub === 'measured' ? h('span.mut', ' · measured') : h('span.mut', ` · ${fo.sub || 'estimated'}, not loaded yet`));
  return viz('Memory it needs', f.need_mib ? [h('b.fig', gb(f.need_mib)), ' against what the empty cluster gives'] : 'Against what the empty cluster gives',
    [fitBar({ need: f.need_mib, budgets, label: n => machines(st, n), budgetText: b => gb(b), alt,
      needTip: f.need_mib ? tipText(`Needs ${gb(f.need_mib)}`, [mib(f.need_mib), fo.title]) : null }),
    verdict ? h('p.viz-say', verdict) : null],
    { key: 'v-fit', cls: 'wide', alt });
}

function compareViz(m, st) {
  const loads = Object.fromEntries(((m.loads || {}).by_nodes || []).map(x => [x.nodes, x]));
  const mem = Object.fromEntries((m.measured || []).map(x => [x.nodes, x]));
  const bench = Object.fromEntries((((m.bench || {}).by_nodes) || []).map(x => [x.nodes, x]));
  const pool = ((st.view || {}).nodes || []).filter(n => !n.excluded).length;
  const max = Math.max(pool, 1, ...[].concat(Object.keys(loads), Object.keys(mem), Object.keys(bench)).map(Number));
  const ns = Array.from({ length: max }, (_, i) => i + 1);
  const block = (title, better, rows) => h('div.cmp', h('div.cmp-h', h('b', title), h('span', better)), compareBars(rows, { label: title + ': ' + rows.map(r => `${r.label} ${r.value != null ? r.text : 'not measured yet'}`).join(', ') }));
  return viz(`On ${ns.map(n => n).join(', ').replace(/, (\d+)$/, ' and $1')} ${machines(st, 2).replace(/^2 /, '')}`, 'Medians of the loads in the event history and of the benchmark runs',
    h('div.cmp-grid',
      block('Load time', 'lower is better', ns.map(n => {
        const l = loads[n], b = bench[n];
        const v = l ? l.median_s : b && b.load_s ? b.load_s.median : null;
        return { label: machines(st, n), value: v, text: dur(v), tip: l ? tipText(`${machines(st, n)}: ${dur(l.median_s)}`, [`Median of ${plural(l.count, 'load')}`, `The last took ${dur(l.last_s)}`]) : null };
      })),
      block('Upload speed', 'higher is better', ns.map(n => {
        const l = loads[n];
        const v = l && l.avg_mb_s ? l.avg_mb_s : null;
        return { label: machines(st, n), value: v, text: `${num(v)} MB/s`, tip: v ? tipText(`${machines(st, n)}: ${num(v)} MB/s`, [`Average over ${plural(l.count, 'load')}`]) : null };
      })),
      block('Memory', 'at the smallest context run', ns.map(n => {
        const me = mem[n];
        return { label: machines(st, n), value: me ? me.memory_mib : null, text: me ? gb(me.memory_mib) : '', tip: me ? tipText(`${machines(st, n)}: ${gb(me.memory_mib)}`, [mib(me.memory_mib), me.ctx ? `At ${ctx(me.ctx)} context` : null, me.profile]) : null };
      })),
      block('Answer speed', 'higher is better', ns.map(n => {
        const b = bench[n];
        const v = b && b.decode_tps ? b.decode_tps.median : null;
        return { label: machines(st, n), value: v, text: `${num(v, 1)} tok/s`, tip: v ? tipText(`${machines(st, n)}: ${num(v, 1)} tok/s`, [`Decode speed, median of ${plural(b.decode_tps.count, 'benchmark step')}`]) : null };
      }))),
    { key: 'v-cmp', cls: 'wide' });
}

function activityViz(m) {
  const act = ((m.charts || {}).activity) || {};
  const hours = act.hours || [], days = act.days || [];
  const any = b => b.loads || b.refusals || b.requests;
  // the last 24 hours when something happened in them, else the last 7 days
  const useDays = !hours.some(any) && days.some(any);
  const buckets = useDays ? days : hours;
  const label = b => (useDays ? new Date(b.at).toLocaleDateString('en-GB', { day: 'numeric', month: 'short' }) : clock(b.at, false));
  const span = b => (useDays ? label(b) : `${clock(b.at, false)} to ${clock(b.at + 3600e3, false)}`);
  const rows = buckets.map(b => Object.assign({}, b, { ok: (b.loads || 0) - (b.failed || 0) }));
  const tot = k => buckets.reduce((s, b) => s + (b[k] || 0), 0);
  const alt = `In the last ${useDays ? '7 days' : '24 hours'}: ${plural(tot('loads'), 'load')} (${num(tot('failed'))} failed), ${plural(tot('refusals'), 'refused request')}, ${plural(tot('requests'), 'request')} served.`;
  return viz('Activity', useDays ? 'Per day, the last 7 days' : 'Per hour, the last 24 hours', h('div.viz-pair',
    h('div.viz-row', h('span.viz-rl', 'Loads and refused requests'), columns(rows, { series: [{ key: 'ok', color: 'mint' }, { key: 'failed', color: 'coral' }, { key: 'refusals', color: 'amber' }], label, height: 92, alt,
      tip: b => tipText(span(b), [plural(b.loads || 0, 'load') + (b.failed ? `, ${num(b.failed)} failed` : ''), b.refusals ? plural(b.refusals, 'refused request') : null]), empty: 'No load in this time' })),
    h('div.viz-row', h('span.viz-rl', 'Requests served'), columns(rows, { series: [{ key: 'requests', color: 'mut' }], label, height: 92, alt: 'Requests served',
      tip: b => tipText(span(b), [plural(b.requests || 0, 'request') + ' served', 'Counted when the model unloaded']), empty: 'No request counted in this time' }))),
  { key: 'v-act', cls: 'wide', alt, legend: legendItems([[colorSwatch('mint'), 'Loaded'], [colorSwatch('coral'), 'Load failed'], [colorSwatch('amber'), 'Refused request'], [colorSwatch('mut'), 'Requests served, counted at unload']]) });
}

function chartsCard(m, st) {
  if (m.kind !== 'llama') return null;
  const loads = ((m.charts || {}).loads) || null;
  const body = [fitViz(m, st)];
  if (!loads) body.push(h('div.viz.wide', { key: 'v-wait' }, skeleton(4)));
  else {
    const time = byNodes(loads, st, l => (l.load_s > 0 ? l.load_s : null), l => `Loaded in ${dur(l.load_s)}`);
    const rate = byNodes(loads.filter(l => l.outcome === 'ready'), st, l => (l.mb_s > 0 ? l.mb_s : null), l => `Uploaded at ${num(l.mb_s)} MB/s`);
    const ok = loads.filter(l => l.outcome === 'ready' && l.load_s > 0);
    const failed = loads.some(l => l.outcome !== 'ready');
    const said = (list, f) => list.slice(-12).map(f).join(', ');
    const altTime = ok.length ? `Load time of the last ${Math.min(12, ok.length)} of ${plural(ok.length, 'load')}, oldest first: ` + said(ok, l => `${dur(l.load_s)} on ${l.nodes}`) + '.' : 'No load is in the kept history.';
    const altRate = ok.length ? 'Upload speed, oldest first: ' + said(ok.filter(l => l.mb_s), l => `${num(l.mb_s)} MB/s on ${l.nodes}`) + '.' : 'No load is in the kept history.';
    body.push(
      viz('Load time', 'Every load in the kept history, in seconds',
        pointChart({ series: time, unit: 's', label: altTime, empty: 'Not loaded yet', emptySub: 'A point appears here for every load.' }),
        { key: 'v-time', alt: altTime, legend: time.length ? shapeLegend(time, failed ? [[colorSwatch('coral'), 'Failed or cancelled']] : null) : null }),
      viz('Upload speed', 'To the nodes, in MB/s',
        pointChart({ series: rate, unit: 'MB/s', label: altRate, empty: 'Not measured yet', emptySub: 'The speed is known once a load has uploaded the model to a node.' }),
        { key: 'v-rate', alt: altRate, legend: rate.length ? shapeLegend(rate) : null }),
      compareViz(m, st),
      activityViz(m));
  }
  return h('section.card', { key: 'charts' },
    h('div.ch', h('h2', 'At a glance'), h('span.note', m.history === false ? 'The event history is off, so only the memory need is known' : 'From the event history and the benchmark runs. Hover a mark for its figures.')),
    h('div.viz-grid', body));
}

// ---- usage: loads, unloads and refusals, newest first ----

const UNLOAD_WHY = { ttl: 'idle too long', api: 'asked through the API', eviction: 'made room for another model', shutdown: 'the scheduler stopped' };

// One row per load: the load and the unload that ended it share a session id.
// Refusals and loads that failed are rows of their own.
function usageRows(events) {
  const out = [], bySession = {};
  for (const e of (events || []).slice().reverse()) { // oldest first, so a load comes before its unload
    const d = e.data || {};
    if (e.type === 'loadFinished') {
      const ok = d.outcome === 'ready';
      const r = { key: e.seq + '-' + e.ts, ts: e.ts, profile: e.profile, nodes: e.nodes || [], took: d.load_s, rate: d.avg_mb_s,
        what: ok ? ['Loaded', 'use'] : d.outcome === 'cancelled' ? ['Load cancelled', 'aside'] : ['Load failed', 'fail'],
        note: ok ? '' : plainReason(d.reason), raw: ok ? '' : d.reason };
      if (ok && e.session_id) bySession[e.session_id] = r;
      out.push(r);
    } else if (e.type === 'instanceUnloaded') {
      const r = e.session_id && bySession[e.session_id];
      const why = UNLOAD_WHY[d.reason] || d.reason || '';
      if (r) { r.stayed = d.loaded_for_s; r.served = d.requests_served; r.ended = why; r.bench = d.bench_run; }
      else out.push({ key: e.seq + '-' + e.ts, ts: e.ts, profile: e.profile, nodes: e.nodes || [], what: ['Unloaded', 'n'], stayed: d.loaded_for_s, served: d.requests_served, ended: why });
    } else if (e.type === 'requestRefused') {
      out.push({ key: e.seq + '-' + e.ts, ts: e.ts, profile: e.profile, nodes: e.nodes || [], what: ['Refused', 'aside'], note: plainReason(d.message || d.reason), raw: d.message });
    } else if (e.type === 'loadCancelledLowMemory') {
      out.push({ key: e.seq + '-' + e.ts, ts: e.ts, profile: e.profile, nodes: e.nodes || [], what: ['Low memory', 'fail'], note: e.summary });
    }
  }
  return out.reverse();
}

function usageCard(m, st, showAll, toggleAll) {
  const rows = usageRows(m.events);
  const l = m.loads || {};
  const shown = showAll ? rows : rows.slice(0, 6);
  const loadedNow = new Set((m.instances || []).map(i => i.profile));
  return h('section.card', { key: 'usage' },
    h('div.ch', h('h2', 'Usage'), h('span.note', m.history === false ? 'The event history is off, so past use is not known'
      : `${plural(l.count || 0, 'load')} and ${plural(l.refusals || 0, 'refusal')} in the kept history`)),
    h('div.stats',
      stat('Requests served', num(m.requests_served || 0), null, { title: 'By the instances that are loaded now and the ones the history has seen unload' }),
      stat('Last used', m.state !== 'not_loaded' ? 'now' : ago(m.last_active, 'never')),
      stat('Last loaded', ago(m.last_loaded, 'never')),
      stat('Loads', num(l.ready || 0), (l.failed || 0) + (l.cancelled || 0) ? `ok · ${num((l.failed || 0) + (l.cancelled || 0))} failed` : l.ready ? 'all ok' : null)),
    !m.events ? skeleton(3) : table({
      rows: shown, key: r => r.key, minWidth: 860,
      columns: [
        { label: 'When', cls: 'tight', render: r => h('time', { title: dateTime(r.ts) }, agoText(r.ts)) },
        { label: 'What', cls: 'tight', render: r => badge(r.what[0], r.what[1]) },
        { label: 'Configuration', render: r => h('span.mono.nowrap', r.profile) },
        { label: 'On', cls: 'tight', render: r => (r.nodes.length ? h('span', { title: r.nodes.join(', ') }, machines(st, r.nodes.length)) : h('span.mut', DASH)) },
        { label: 'Load time', cls: 'r', render: r => (r.took != null ? dur(r.took) : '') },
        { label: 'Upload', cls: 'r', title: 'Average upload speed to the nodes', render: r => (r.rate ? num(r.rate) + ' MB/s' : '') },
        { label: 'Stayed loaded', cls: 'r', render: r => (r.stayed != null ? dur(r.stayed) : r.what[0] === 'Loaded' && loadedNow.has(r.profile) ? 'still loaded' : '') },
        { label: 'Requests', cls: 'r', render: r => (r.served != null ? num(r.served) : '') },
        { label: 'Then', render: r => h('span.quiet', { title: r.raw ? 'The scheduler says: ' + r.raw : null, style: 'font-size:13px' },
          r.note || (r.ended ? 'Unloaded: ' + r.ended + (r.bench ? ' (a benchmark run)' : '') : '')) },
      ],
      empty: empty('It has not been loaded', m.history === false ? 'Loads are listed here when the event history is on.' : 'No load or refusal of this model is in the kept history.'),
    }),
    rows.length > 6 ? h('div.cs', more(showAll, toggleAll, `Show all ${num(rows.length)}`, 'Show the newest 6')) : null);
}

// ---- configurations ----

function configCard(m, st, showGenerated, toggleGenerated) {
  const all = m.profiles || [];
  const hand = all.filter(p => !p.generated), gen = all.filter(p => p.generated);
  const loaded = new Set((m.instances || []).map(i => i.profile));
  const rows = showGenerated || !hand.length ? all.slice().sort((a, b) => (a.generated - b.generated) || a.id.localeCompare(b.id)) : hand;
  return h('section.card', { key: 'configs' },
    h('div.ch.split',
      h('div.row', h('h2', 'Configurations'), h('span.note', `${plural(hand.length, 'hand-made profile')} · ${num(gen.length)} generated for benchmarks`)),
      h('a.btn.sm', { href: href('profiles', null, { q: m.file || m.name }) }, 'Open in Profiles')),
    table({
      rows, key: p => p.id, minWidth: 900,
      columns: [
        { label: 'Profile', render: p => h('span', h('span.mono.nowrap', { style: 'font-weight:500' }, p.id), loaded.has(p.id) ? [' ', badge('Loaded', 'ready')] : null,
          (p.aliases || []).length ? h('span.sub', 'also ' + p.aliases.join(', ')) : null) },
        { label: 'Made', cls: 'tight', render: p => (p.generated ? h('span.quiet', { title: 'An auto-generated benchmark configuration' }, 'Generated') : 'By hand') },
        { label: 'Context', cls: 'r', render: p => ctx(p.ctx) },
        { label: 'Placement', render: p => (p.placement === 'forced' ? h('span', nodeChips(p.force_nodes), (p.tensor_split || []).length ? h('span.sub', 'split ' + p.tensor_split.join(' / ')) : null)
          : h('span.quiet', 'Automatic')) },
        { label: 'Memory need', cls: 'r', render: p => (p.needs_mib ? h('span', { title: plainBasis(p.needs_basis) }, mib(p.needs_mib), h('span.sub', /^estimated/.test(p.needs_basis || '') ? 'estimated' : /needs_mib/.test(p.needs_basis || '') ? 'set by hand' : 'measured')) : h('span.mut', DASH)) },
        { label: 'Would it start now', render: p => planCell(p, st) },
        { label: '', cls: 'tight', render: p => h('span.row', { style: 'gap:6px 14px;flex-wrap:nowrap;justify-content:flex-end' },
          h('a', { href: href('placement', [p.id]) }, 'Preview'), h('a', { href: href('profiles', [p.id]) }, 'Edit')) },
      ],
      empty: empty('No configuration uses this file', 'The file is in the models directory, but no profile names it. Add a profile under Configure, Profiles to make it loadable.'),
    }),
    hand.length && gen.length ? h('div.cs', more(showGenerated, toggleGenerated, `Show the ${num(gen.length)} generated configurations`, 'Hide the generated configurations')) : null);
}

// ---- benchmarks ----

function benchCard(m) {
  const rows = ((m.bench || {}).detail) || [];
  return h('section.card', { key: 'bench' },
    h('div.ch', h('h2', 'Benchmarks'), h('span.note', rows.length ? 'Test cases that load this model, with their last result' : null)),
    table({
      rows, key: b => b.benchmark_id, minWidth: 820,
      columns: [
        { label: 'Benchmark', render: b => h('span', h('a.name', { href: href('benchmarks', ['benchmark', b.benchmark_id]) }, b.name || b.benchmark_id), h('span.sub.mono', b.profile)) },
        { label: 'Runs', cls: 'r', render: b => num(b.runs) },
        { label: 'Passed', cls: 'tight', title: 'One dot per run, oldest left', render: b => passDots(b.recent, b.pass_rate, { label: `Runs of ${b.name || b.benchmark_id}, oldest first` }) },
        { label: 'Load', cls: 'r', title: 'Load time of the last run', render: b => (b.load_s ? dur(b.load_s.last) : DASH) },
        { label: 'Upload', cls: 'r', render: b => (b.load_mb_s ? num(b.load_mb_s.last) + ' MB/s' : DASH) },
        { label: 'Answer speed', cls: 'r', title: 'Decode speed of the last run', render: b => (b.decode_tps ? num(b.decode_tps.last, 1) + ' tok/s' : DASH) },
        { label: 'Last run', cls: 'tight', render: b => (b.last_run_id ? h('a', { href: href('benchmarks', ['runs', b.last_run_id]) }, agoText(b.last_at)) : h('span.mut', 'Never')) },
      ],
      empty: empty('No benchmark uses this model', 'A benchmark is a test case under Benchmarks that loads one configuration of a model and measures it.'),
    }));
}

// ---- the icon ----

const FAMILY = { gemma: 'Gemma', qwen: 'Qwen', llama: 'Llama', mistral: 'Mistral', deepseek: 'DeepSeek', glm: 'GLM', kimi: 'Kimi', tinyllama: 'TinyLlama', phi: 'Phi', granite: 'Granite' };

// changeIcon(m) -> a dialog: pick an image, see it at the three sizes, save it
// for this model or for its whole family, or remove the one that is set.
function changeIcon(m, done) {
  const family = FAMILY[m.brand];
  let file = null, scope = 'model', busy = false, error = '';
  const path = '/api/models/' + encodeURIComponent(m.key) + '/icon';
  const box = h('div.col', { style: 'gap:16px' });
  const picker = h('input', { type: 'file', accept: 'image/png,image/jpeg,image/webp,image/svg+xml,.png,.jpg,.jpeg,.webp,.svg', 'aria-label': 'Image file', class: 'file' });
  let previewURL = '';
  picker.addEventListener('change', () => {
    file = picker.files && picker.files[0] ? picker.files[0] : null;
    error = file && file.size > 256 * 1024 ? `That file is ${num(file.size / 1024)} kB. An icon may be at most 256 kB.` : '';
    if (previewURL) URL.revokeObjectURL(previewURL);
    previewURL = file ? URL.createObjectURL(file) : '';
    draw();
  });
  let close = () => {};
  const finish = text => { toast(text, 'ok'); close(true); done(); };
  const save = async () => {
    if (!file || busy || error) return;
    busy = true; draw();
    try { await api.upload('PUT', path, file, { params: scope === 'family' ? { scope: 'family' } : null }); finish(scope === 'family' ? `Icon set for every ${family} model` : 'Icon set'); } catch (e) { error = e.message; busy = false; draw(); }
  };
  const remove = async sc => {
    if (busy) return;
    busy = true; draw();
    try { await api.del(path, { params: sc === 'family' ? { scope: 'family' } : null }); finish('Icon removed'); } catch (e) { error = e.message; busy = false; draw(); }
  };
  const tile = size => (previewURL
    ? h('span', { class: 'av img av-' + size }, h('img', { src: previewURL, alt: '', width: size, height: size }))
    : avatar(m, size));
  function draw() {
    const cur = m.icon;
    box.replaceChildren(...[
      h('p', 'The picture shown next to this model. Without one, the page draws a tile with the family’s letters. PNG, JPEG, WebP or SVG, at most 256 kB; a square image works best. An SVG that carries a script is refused.'),
      h('div.icon-prev', tile(72), tile(40), tile(28), h('span.mut', previewURL ? 'The new icon, at the sizes the page uses' : cur ? (cur.scope === 'family' ? `The icon of the ${family ? family + ' family' : 'family'}` : 'The icon set for this model') : 'The drawn avatar (no icon is set)')),
      h('div.field', h('label', 'Image file'), picker),
      family ? h('div.field', h('label', 'Use it for'),
        h('div.row', { style: 'gap:8px' },
          h('button', { type: 'button', class: 'fc' + (scope === 'model' ? ' on' : ''), onclick: () => { scope = 'model'; draw(); } }, 'This model only'),
          h('button', { type: 'button', class: 'fc' + (scope === 'family' ? ' on' : ''), onclick: () => { scope = 'family'; draw(); } }, `Every ${family} model`)),
        h('span.help', scope === 'family' ? `One upload serves every model of the ${family} family that has no icon of its own.` : 'Other models of the family keep theirs.')) : null,
      error ? h('div.banner.err', { role: 'alert' }, error) : null,
      h('div.dlg-foot',
        cur ? h('button', { type: 'button', class: 'btn lg dng', disabled: busy, style: 'margin-right:auto', onclick: () => remove(cur.scope) }, cur.scope === 'family' ? `Remove the ${family ? family + ' family' : 'family'} icon` : 'Remove icon') : null,
        h('button', { type: 'button', class: 'btn lg', onclick: () => close(undefined) }, 'Cancel'),
        h('button', { type: 'button', class: 'btn lg pri', disabled: !file || busy || !!error, onclick: save }, busy ? 'Saving…' : 'Save icon')),
    ].filter(Boolean));
  }
  draw();
  return dialog({ title: 'Icon of ' + m.name, body: c => { close = c; return box; } }).then(() => { if (previewURL) URL.revokeObjectURL(previewURL); });
}

// ---- the page ----

export function mountDetail(el, key) {
  let m = null, error = '', missing = false, timer = 0, alive = true, showGenerated = false, showAllUsage = false, seen = '';
  const open = new Set();
  const toggle = k => { if (open.has(k)) open.delete(k); else open.add(k); view.refresh(); };

  const load = async () => {
    try { m = await api.get('/api/models/' + encodeURIComponent(key)); error = ''; missing = false; } catch (e) { error = e.message; missing = e.status === 404; }
    if (alive) view.refresh();
  };
  const later = ms => { clearTimeout(timer); timer = setTimeout(load, ms); };
  // the catalog entry changes when anything about this model does (a load, a
  // refusal, the configuration, its icon): the detail is read again then
  const offCatalog = onCatalog(() => {
    const e = catalog.byKey[key];
    const sig = e ? JSON.stringify([e.state, e.loads && e.loads.count, e.loads && e.loads.refusals, e.requests_served, e.profile_count, e.startable_count, e.icon, (e.instances || []).length, e.last_active]) : '';
    if (sig !== seen) { const first = seen === ''; seen = sig; if (!first) later(300); }
    view.refresh();
  });
  const offEvents = listen('event', e => { if (m && e && (e.model_file === m.file || (m.profiles || []).some(p => p.id === e.profile))) later(800); });

  const render = () => {
    const st = state.status || {};
    const back = backLink('Models', href('models'));
    // the catalog entry is enough for the head, the facts and the fit; the rest follows
    const cur = m ? Object.assign({}, m, catalog.byKey[key] ? { icon: catalog.byKey[key].icon } : null) : catalog.byKey[key] || null;
    if (!cur) {
      return [back, h('div.mhead', avatar(null, 72), h('div', h('h1', key))), staleBanner(),
        h('section.card', error ? empty(missing ? 'There is no such model' : 'This model could not be read', missing ? 'No model file and no profile has this name. It may have been removed from the configuration.' : error) : skeleton(5))];
    }
    const insts = (st.instances || []).filter(i => (cur.profiles || []).some(p => p.id === i.profile) || (cur.file && i.model_file === cur.file));
    const f = fitOf(cur, st);
    const refusal = cur.last_refusal;
    const hand = (cur.profile_count || 0) - (cur.generated_count || 0);
    return [
      back,
      h('div.mhead', { key: 'head' },
        avatar(cur, 72),
        h('div.grow',
          h('div.row', { style: 'gap:10px 14px' }, h('h1', cur.name), stateBadge(cur, st)),
          h('span.sub', cur.publisher ? [cur.publisher, ' · '] : null, sizeLine(cur), cur.family ? [' · ', cur.family] : null),
          h('div.row', { style: 'gap:8px;margin-top:8px' },
            h('button', { type: 'button', class: 'btn sm', onclick: () => changeIcon(cur, load) }, 'Change icon'),
            cur.icon ? h('span.mut', { style: 'font-size:12px;font-weight:600' }, cur.icon.scope === 'family' ? 'Using the icon of its family' : 'Using its own icon') : null))),
      staleBanner(),
      error && m ? h('div.banner.warn', { role: 'status' }, 'The newest figures could not be read: ' + error) : null,
      cur.kind === 'llama' && !cur.exists ? h('div.banner.err', { role: 'status' }, h('span.grow', h('b', 'The model file is missing. '), 'A profile names ', h('span.mono', cur.file), ', but it is not in the models directory, so it cannot be loaded.')) : null,
      h('section.card', { key: 'facts' },
        h('div.stats', { style: 'padding-top:16px' },
          stat('Needs', f.need ? gbNum(f.need) : DASH, f.need ? 'GB' : null, { title: f.title || (f.need ? mib(f.need) : null) }),
          stat(f.kind === 'one' || f.kind === 'several' ? (f.estimate ? (f.ranOn ? 'Should fit on' : 'Should need') : 'Runs on') : 'Runs on',
            f.nodes ? machines(st, f.nodes) : f.kind === 'cmd' ? 'This machine' : f.kind === 'none' ? 'Too big' : DASH,
            f.nodes ? (f.ranOn ? `estimate · measured on ${f.ranOn}` : f.sub) : null, { title: f.title }),
          cur.size_bytes ? stat('File size', bytes(cur.size_bytes).replace(/ [A-Za-z]+$/, ''), bytes(cur.size_bytes).split(' ')[1]) : null,
          cur.context_length ? stat('Trained context', ctx(cur.context_length), 'tokens') : null,
          stat('Configurations', num(cur.profile_count || 0), cur.generated_count ? `${num(hand)} by hand` : null),
          stat('Could start now', num(cur.startable_count || 0), `of ${num(cur.profile_count || 0)}`, { title: 'How many of its configurations a request could start right now' })),
        cur.file ? h('div.cs', h('dl.kv',
          h('dt', 'File'), h('dd', h('code.cmd', { style: 'font-size:13px;color:var(--text)' }, cur.file)),
          cur.publisher ? [h('dt', 'Published by'), h('dd', cur.publisher)] : null,
          cur.raw_name ? [h('dt', 'Named in the file'), h('dd', h('code.cmd', cur.raw_name))] : null,
          cur.meta_source === 'file_name' ? [h('dt', 'Details from'), h('dd.mut', 'the file name (its own metadata was not read)')] : null)) : null),
      chartsCard(cur, st),
      refusal ? h('div.banner', { key: 'refusal', role: 'status' },
        h('span.grow', h('b', 'Last refused ', agoText(refusal.at), ': '), h('span.mono', refusal.profile), ' · ', plainReason(refusal.message || refusal.reason),
          open.has('refusal') ? h('div.raw', refusal.message || refusal.reason) : null),
        more(open.has('refusal'), () => toggle('refusal'), 'The scheduler’s words', 'Hide')) : null,
      h('div.sec-title', { key: 't-now' }, h('h2', 'Loaded now'),
        insts.length ? h('span.note', plural(insts.length, 'instance')) : h('span.note', 'Not loaded. It loads when a request asks for one of its configurations.'),
        insts.some(i => i.state !== 'starting') ? h('span.right', legend([['m', 'Model'], ['k', 'KV cache'], ['c', 'Compute']])) : null),
      insts.map(inst => instanceCard(st, inst, cur, open, toggle)),
      usageCard(cur, st, showAllUsage, () => { showAllUsage = !showAllUsage; view.refresh(); }),
      configCard(cur, st, showGenerated, () => { showGenerated = !showGenerated; view.refresh(); }),
      m ? benchCard(cur) : null,
    ];
  };
  const view = liveView(el, render, ['status', 'tick', 'conn']);
  needCatalog();
  if (catalog.byKey[key]) { const e = catalog.byKey[key]; seen = JSON.stringify([e.state, e.loads && e.loads.count, e.loads && e.loads.refusals, e.requests_served, e.profile_count, e.startable_count, e.icon, (e.instances || []).length, e.last_active]); }
  load();
  return { unmount() { alive = false; clearTimeout(timer); offCatalog(); offEvents(); view.unmount(); } };
}
