// One model's page (#/models/<key>): what it is, whether it is loaded and
// where, how it has been used, how fast it loads and answers on one, two and
// three machines, the configurations (profiles) that load it, and the
// benchmarks that use it. Read-only: there is no load or unload control.
//
// Reads: REST GET /api/models/<key> (the catalog entry with its recent events
// and benchmarks) on open, and again when the placement picture in the
// WebSocket "status" changes, an event of this model arrives or the
// configuration changes; WebSocket "status" for the instance cards.

import { h } from '../../core/dom.js';
import { href } from '../../core/router.js';
import { state, on, listen } from '../../core/store.js';
import * as api from '../../core/api.js';
import { liveView, pageHead, badge, nodeChips, stat, table, empty, skeleton, backLink, more, legend } from '../../core/ui.js';
import { num, gb, gbNum, mib, bytes, ctx, dur, ago, plural, agoText, dateTime, pct, DASH } from '../../core/format.js';
import { staleBanner, machines, plainReason, plainBasis } from '../../core/status.js';
import { instanceCard, sizeLine, stateBadge, fitOf, planCell } from './common.js';

const picture = st => JSON.stringify([(st.instances || []).map(i => [i.profile, i.state]), (st.queue || []).map(q => [q.profile, q.claimed]),
  (st.backoffs || []).map(b => b.profile), ((st.view || {}).nodes || []).map(n => [n.online, n.eligible, n.holder]), (st.view || {}).stale, st.config_revision]);

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
    table({
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

// ---- performance on 1, 2, 3 machines ----

function perfCard(m, st) {
  const loads = Object.fromEntries(((m.loads || {}).by_nodes || []).map(x => [x.nodes, x]));
  const mem = Object.fromEntries((m.measured || []).map(x => [x.nodes, x]));
  const bench = Object.fromEntries((((m.bench || {}).by_nodes) || []).map(x => [x.nodes, x]));
  const pool = ((st.view || {}).nodes || []).filter(n => !n.excluded).length;
  const known = [].concat(Object.keys(loads), Object.keys(mem), Object.keys(bench)).map(Number);
  const max = Math.max(pool, 1, ...known);
  const cols = [];
  for (let n = 1; n <= max; n++) {
    const l = loads[n], me = mem[n], b = bench[n];
    const any = l || me || b;
    const row = (label, value, title) => h('div', { style: 'display:flex;justify-content:space-between;gap:12px;align-items:baseline', title },
      h('span.mut', { style: 'font-size:13px;font-weight:600' }, label), h('span.fig', { style: 'font-family:var(--mono);font-size:13px' }, value));
    cols.push(h('div', { key: 'perf-' + n },
      h('h3', machines(st, n), any ? null : badge('Not measured yet', 'n')),
      any ? [
        me ? row('Memory', gb(me.memory_mib), `${mib(me.memory_mib)} at ${ctx(me.ctx)} context, measured with ${me.profile}`) : row('Memory', DASH),
        l ? row('Load time', dur(l.median_s), `Median of ${plural(l.count, 'load')} in the history; the last took ${dur(l.last_s)}`) : b && b.load_s ? row('Load time', dur(b.load_s.median), 'Median of the benchmark runs') : row('Load time', DASH),
        l && l.avg_mb_s ? row('Upload speed', num(l.avg_mb_s) + ' MB/s', 'Average over the loads in the history') : row('Upload speed', DASH),
        b && b.decode_tps ? row('Answer speed', num(b.decode_tps.median, 1) + ' tok/s', `Decode speed, median of ${plural(b.decode_tps.count, 'benchmark step')}`) : row('Answer speed', h('span.mut', { style: 'font-family:var(--sans);font-weight:600' }, 'not measured yet')),
        b && b.prefill_tps ? row('Prompt speed', num(b.prefill_tps.median, 0) + ' tok/s', 'Prefill speed, median of the benchmark steps') : null,
        h('span.mut', { style: 'font-size:12px;font-weight:600' }, l ? plural(l.count, 'load') : 'no load', b ? ` · ${plural(b.runs, 'benchmark run')}` : ''),
      ] : h('span.none', n > pool ? 'More machines than the pool has.' : 'It has not been loaded on this many.')));
  }
  return h('section.card', { key: 'perf' },
    h('div.ch', h('h2', 'Performance'), h('span.note', 'From the loads in the event history and the benchmark runs')),
    h('div.perf', cols));
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
      rows, key: b => b.benchmark_id, minWidth: 760,
      columns: [
        { label: 'Benchmark', render: b => h('span', h('a.name', { href: href('benchmarks', ['edit', 'benchmark', b.benchmark_id]) }, b.name || b.benchmark_id), h('span.sub.mono', b.profile)) },
        { label: 'Runs', cls: 'r', render: b => num(b.runs) },
        { label: 'Passed', cls: 'r', render: b => (b.pass_rate != null ? pct(b.pass_rate * 100) : DASH) },
        { label: 'Load', cls: 'r', title: 'Load time of the last run', render: b => (b.load_s ? dur(b.load_s.last) : DASH) },
        { label: 'Upload', cls: 'r', render: b => (b.load_mb_s ? num(b.load_mb_s.last) + ' MB/s' : DASH) },
        { label: 'Answer speed', cls: 'r', title: 'Decode speed of the last run', render: b => (b.decode_tps ? num(b.decode_tps.last, 1) + ' tok/s' : DASH) },
        { label: 'Last run', cls: 'tight', render: b => (b.last_run_id ? h('a', { href: href('benchmarks', ['runs', b.last_run_id]) }, agoText(b.last_at)) : h('span.mut', 'Never')) },
      ],
      empty: empty('No benchmark uses this model', 'A benchmark is a test case under Benchmarks that loads one configuration of a model and measures it.'),
    }));
}

// ---- the page ----

export function mountDetail(el, key) {
  let m = null, error = '', missing = false, sig = '', timer = 0, alive = true, showGenerated = false, showAllUsage = false;
  const open = new Set();
  const toggle = k => { if (open.has(k)) open.delete(k); else open.add(k); view.refresh(); };

  const load = async () => {
    try { m = await api.get('/api/models/' + encodeURIComponent(key)); error = ''; missing = false; } catch (e) { error = e.message; missing = e.status === 404; }
    if (alive) view.refresh();
  };
  const later = ms => { clearTimeout(timer); timer = setTimeout(load, ms); };
  const offStatus = on('status', st => {
    const s = picture(st || {});
    if (s === sig) return;
    const first = sig === '';
    sig = s;
    if (first) load(); else later(500);
  });
  const offEvents = listen('event', e => { if (m && e && (e.model_file === m.file || (m.profiles || []).some(p => p.id === e.profile))) later(800); });
  const offConfig = listen('config', load);

  const render = () => {
    const st = state.status || {};
    const back = backLink('Models', href('models'));
    if (!m) {
      return [back, pageHead(key), staleBanner(),
        h('section.card', error ? empty(missing ? 'There is no such model' : 'This model could not be read', missing ? 'No model file and no profile has this name. It may have been removed from the configuration.' : error) : skeleton(5))];
    }
    const insts = (st.instances || []).filter(i => (m.profiles || []).some(p => p.id === i.profile) || (m.file && i.model_file === m.file));
    const f = fitOf(m, st);
    const refusal = m.last_refusal;
    const hand = (m.profile_count || 0) - (m.generated_count || 0);
    return [
      back,
      pageHead(m.name, [sizeLine(m), m.family ? [' · ', m.family] : null], stateBadge(m, st)),
      staleBanner(),
      m.kind === 'llama' && !m.exists ? h('div.banner.err', { role: 'status' }, h('span.grow', h('b', 'The model file is missing. '), 'A profile names ', h('span.mono', m.file), ', but it is not in the models directory, so it cannot be loaded.')) : null,
      h('section.card', { key: 'facts' },
        h('div.stats', { style: 'padding-top:16px' },
          stat('Needs', f.need ? gbNum(f.need) : DASH, f.need ? 'GB' : null, { title: f.title || (f.need ? mib(f.need) : null) }),
          stat(f.kind === 'one' || f.kind === 'several' ? (f.sub === 'measured' ? 'Runs on' : 'Should need') : 'Runs on',
            f.nodes ? machines(st, f.nodes) : f.kind === 'cmd' ? 'This machine' : f.kind === 'none' ? 'Too big' : DASH, f.sub && f.nodes ? f.sub : null),
          m.size_bytes ? stat('File size', bytes(m.size_bytes).replace(/ [A-Za-z]+$/, ''), bytes(m.size_bytes).split(' ')[1]) : null,
          m.context_length ? stat('Trained context', ctx(m.context_length), 'tokens') : null,
          stat('Configurations', num(m.profile_count || 0), m.generated_count ? `${num(hand)} by hand` : null),
          stat('Start now', num(m.startable_count || 0), `of ${num(m.profile_count || 0)}`, { title: 'How many of its configurations a request could start right now' })),
        m.file ? h('div.cs', h('dl.kv',
          h('dt', 'File'), h('dd', h('code.cmd', { style: 'font-size:13px;color:var(--text)' }, m.file)),
          m.publisher ? [h('dt', 'Published by'), h('dd', m.publisher)] : null,
          m.meta_source === 'file_name' ? [h('dt', 'Details from'), h('dd.mut', 'the file name (its own metadata was not read)')] : null)) : null),
      refusal ? h('div.banner', { key: 'refusal', role: 'status' },
        h('span.grow', h('b', 'Last refused ', agoText(refusal.at), ': '), h('span.mono', refusal.profile), ' · ', plainReason(refusal.message || refusal.reason),
          open.has('refusal') ? h('div.raw', refusal.message || refusal.reason) : null),
        more(open.has('refusal'), () => toggle('refusal'), 'The scheduler’s words', 'Hide')) : null,
      h('div.sec-title', { key: 't-now' }, h('h2', 'Loaded now'),
        insts.length ? h('span.note', plural(insts.length, 'instance')) : h('span.note', 'Not loaded. It loads when a request asks for one of its configurations.'),
        insts.some(i => i.state !== 'starting') ? h('span.right', legend([['m', 'Model'], ['k', 'KV cache'], ['c', 'Compute']])) : null),
      insts.map(inst => instanceCard(st, inst, m, open, toggle)),
      usageCard(m, st, showAllUsage, () => { showAllUsage = !showAllUsage; view.refresh(); }),
      m.kind === 'llama' ? perfCard(m, st) : null,
      configCard(m, st, showGenerated, () => { showGenerated = !showGenerated; view.refresh(); }),
      benchCard(m),
    ];
  };
  const view = liveView(el, render, ['status', 'tick', 'conn']);
  if (state.status) { sig = picture(state.status); load(); }
  return { unmount() { alive = false; clearTimeout(timer); offStatus(); offEvents(); offConfig(); view.unmount(); } };
}
