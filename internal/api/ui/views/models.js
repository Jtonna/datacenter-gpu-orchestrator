// Models (the home screen): one card per loaded or loading instance, then the
// profiles that are not loaded with what a request for each would do now.
// Read-only: models load when a request asks for them.
//
// Reads: WebSocket "status" (instances, view.nodes, queue, backoffs,
// recent_loads, learned_needs) and "event" (to keep "last loaded" current);
// REST GET /api/profiles (on open and when the placement picture changes) and
// GET /api/history?type=loadFinished,instanceUnloaded (once, on open).

import { h } from '../core/dom.js';
import { registerView, href } from '../core/router.js';
import { state, on, listen } from '../core/store.js';
import * as api from '../core/api.js';
import { liveView, pageHead, badge, nodeChips, stat, bar, legend, memoryRow, progress, steps, table, empty, banner, skeleton } from '../core/ui.js';
import { num, mib, bytes, tps, dur, ago, at, until, ctx, plural, clock, dateTime, agoText, toMs, now, DASH } from '../core/format.js';
import { nodeName, nodeByAddr, instanceNodeNames, nodeMemory, instanceBadge, healthBadge, findNode, drift, staleBanner } from '../core/status.js';

// ---- a loaded instance ----

function placedLine(st, inst, sameModel) {
  const names = instanceNodeNames(st, inst);
  return h('span.mut', { style: 'font-weight:600' },
    'On ', nodeChips(names),
    names.length > 1 ? ` · split across ${names.length} nodes` : null,
    inst.waiters > 0 && inst.state === 'starting' ? ` · ${plural(inst.waiters, 'request')} waiting for it` : null,
    sameModel.length ? [' · same model file as ', sameModel.map((p, i) => [i ? ', ' : '', h('span.mono.txt2', p)])] : null);
}

// A model keeps the profile definition it was loaded with (design 6.0.6).
const staleMark = inst => (inst.stale_profile
  ? badge('Older definition', 'settle', 'Its profile was changed or deleted after it loaded. It keeps running as it was; the new definition is used at its next load.') : null);

function readyCard(st, inst, sameModel) {
  const lr = inst.last_request;
  const host = (inst.host_memory || []).reduce((s, d) => s + (d.total_mib || 0), 0);
  const serving = inst.in_flight > 0;
  return h('section.card', { key: 'inst-' + inst.profile, 'aria-label': inst.profile },
    h('div.ch',
      h('h2.mono', inst.profile), instanceBadge(inst), staleMark(inst), placedLine(st, inst, sameModel),
      inst.model_file ? h('span.mono.mut.right.wrap-any', { title: 'Model file' }, inst.model_file) : null),
    inst.victim_of ? h('div.cb', banner('warn', ['It is being unloaded to make room for ', h('span.mono', inst.victim_of), serving ? ', once its requests in flight finish.' : '.'])) : null,
    h('div.stats',
      stat('In flight', num(inst.in_flight || 0)),
      stat('Waiting', num(inst.waiters || 0)),
      stat('Speed', lr && lr.tokens_per_second > 0 ? tps(lr.tokens_per_second) : DASH, lr && lr.tokens_per_second > 0 ? 'tok/s' : null,
        { title: lr ? `The last finished request: ${num(lr.output_tokens)} tokens out in ${dur((lr.duration_ms || 0) / 1000)}` : 'No request has finished yet' }),
      stat('Ready', ago(inst.ready_since)),
      stat('Last used', serving ? 'now' : ago(inst.last_used, 'not yet')),
      stat('Load took', dur(inst.load_duration)),
      stat('Served', num(inst.requests_served || 0), inst.requests_served === 1 ? 'request' : 'requests'),
      stat('Memory', num(Math.round(inst.device_total_mib || 0)), 'MiB', { title: 'Sum over the nodes, from the load log' }),
      host > 0 ? stat('Host RAM', num(Math.round(host)), 'MiB') : null),
    (inst.nodes || []).map(n => {
      const name = nodeName(st, n.id), mem = nodeMemory(n);
      const hb = healthBadge(findNode(st, n.id));
      if (!mem.total_mib) {
        return h('div.nr', { key: 'mem-' + name }, h('span.mono', name, hb ? [' ', hb] : null), bar([], 1, 'No memory report'),
          h('span.mono.fig.mut', n.budget_mib ? `– / ${mib(n.budget_mib)}` : DASH), h('span.mut', 'The load log reported no memory for this node'));
      }
      return memoryRow(name, mem, n.budget_mib, hb);
    }),
    inst.cmd || inst.log_file ? h('div.cs',
      h('dl.kv',
        inst.cmd ? [h('dt', 'Command'), h('dd', h('code.cmd', inst.cmd))] : null,
        inst.log_file ? [h('dt', 'Log file'), h('dd', h('code.cmd', inst.log_file))] : null)) : null);
}

// ---- a loading instance ----

const WORKER = { waiting: ['Waiting', 'n'], receiving: ['Receiving', 'load'], done: ['Done', 'use'] };

function loadingCard(st, inst, sameModel) {
  const load = inst.load || {};
  const d = drift();
  const elapsed = (load.elapsed_s != null ? load.elapsed_s : inst.load_elapsed_s || 0) + d;
  const frac = load.progress != null ? load.progress : inst.load_progress;
  const eta = load.eta_s != null ? Math.max(0, load.eta_s - d) : null;
  const stalled = load.stalled_s != null ? load.stalled_s + d : null;
  const total = load.bytes_total, sent = load.bytes_sent;
  const workers = load.workers || [];
  const measured = load.source === 'measured';
  const startedAt = load.started_at || toMs(inst.created);
  const deadline = toMs(inst.load_deadline);
  const rate = eta > 0 && total != null && sent != null ? (total - sent) / eta / 1e6 : null;
  const phase = inst.phase || 'loading';
  const order = ['preflight', 'launching', 'loading'];
  const at_ = p => (order.indexOf(phase) > order.indexOf(p) ? 'done' : phase === p ? 'now' : '');
  const uploadNote = total == null ? (phase === 'loading' ? 'Loading on the local node' : 'No upload needed')
    : sent != null ? `${bytes(sent)} of ${bytes(total)}` + (rate ? ` · ${num(rate, 0)} MB/s` : '') : `${bytes(total)} to upload`;
  const stepItems = [
    { label: 'Pre-load check', state: at_('preflight'), note: phase === 'preflight' ? 'Checking free memory on the nodes' : 'Passed' },
    { label: 'Start llama-server', state: at_('launching'), note: phase === 'launching' ? 'Starting' : phase === 'loading' ? 'Running' : '' },
    { label: workers.length ? 'Upload to the workers' : 'Load into memory', state: phase === 'loading' ? (stalled != null ? 'fail' : 'now') : '', note: phase === 'loading' ? uploadNote : '' },
    { label: 'Ready', state: '', note: eta != null && stalled == null ? ['Expected ≈ ', clock(now() + eta * 1000, false)] : 'Not known yet' },
  ];
  const left = stalled != null ? h('span.err-t', `Stalled: no new bytes for ${dur(Math.round(stalled))}`)
    : eta != null ? h('span.mut', { style: 'font-weight:600' }, `About ${dur(eta < 90 ? Math.round(eta) : Math.round(eta / 60) * 60)} left`)
      : h('span.mut', { style: 'font-weight:600' }, 'Time left not known yet');
  return h('section', { class: 'card ' + (stalled != null ? 'flag-err' : ''), key: 'inst-' + inst.profile, 'aria-label': inst.profile },
    h('div.ch',
      h('h2.mono', inst.profile), instanceBadge(inst), stalled != null ? badge('Stalled', 'fail') : null, staleMark(inst), placedLine(st, inst, sameModel),
      inst.model_file ? h('span.mono.mut.right.wrap-any', { title: 'Model file' }, inst.model_file) : null),
    steps(stepItems),
    h('div.cb', progress({
      fraction: frac || 0, cls: stalled != null ? 'x' : 'a',
      label: frac != null ? `${Math.round(frac * 100)} percent loaded` : 'Load in progress',
      left: h('span', frac != null ? h('span.pct', Math.round(frac * 100) + '%') : null, h('span.mut', { style: 'font-weight:600' }, frac != null ? ' · ' : '', `${dur(Math.round(elapsed))} elapsed`)),
      mid: left,
      right: deadline ? h('span', 'Deadline ', until(deadline)) : null,
      foot: [startedAt ? 'Started ' + clock(startedAt) : '', (deadline ? 'Deadline ' + clock(deadline) + ' · ' : '') + (frac != null ? (measured ? 'measured from the workers' : 'estimated from the link speed') : '')],
    })),
    workers.map(w => {
      const node = nodeByAddr(st, w.addr);
      const name = node ? node.hostname || node.id : w.addr;
      const [label, kind] = WORKER[w.state] || [w.state, 'n'];
      const hb = healthBadge(node);
      return h('div.nr', { key: 'w-' + w.addr },
        h('span.mono', name),
        bar([{ cls: w.state === 'done' ? 'm' : 'a', value: w.bytes_sent || 0 }], w.bytes_total, `${name}: ${label}`),
        h('span.mono.fig', w.bytes_sent != null ? `${bytes(w.bytes_sent)} / ${bytes(w.bytes_total)}` : bytes(w.bytes_total)),
        h('span.row', { style: 'gap:6px' }, badge(label, kind), hb));
    }),
    (inst.nodes || []).filter(n => n.local).map(n => h('div.nr', { key: 'local-' + n.id },
      h('span.mono', nodeName(st, n.id)), h('span.mut', 'Local node: the model is read from disk, nothing is uploaded'), h('span'), h('span'))),
    inst.cmd || inst.log_file ? h('div.cs', h('dl.kv',
      inst.cmd ? [h('dt', 'Command'), h('dd', h('code.cmd', inst.cmd))] : null,
      inst.log_file ? [h('dt', 'Log file'), h('dd', h('code.cmd', inst.log_file))] : null)) : null);
}

// ---- not loaded ----

function whereCell(p, st) {
  const plan = p.plan || {};
  const nodes = plan.nodes && plan.nodes.length ? nodeChips(plan.nodes) : null;
  const backoff = (st.backoffs || []).find(b => b.profile === p.id);
  switch (plan.action) {
    case 'start': return h('span', nodes, h('span.mut', nodes ? ' · would start now' : 'Would start now'), plan.warning ? h('span.sub.warn-t', plan.warning) : null);
    case 'serve': case 'wait': return h('span.mut', 'Already loaded or loading');
    case 'evict': return h('span', nodes, h('span.mut', nodes ? ' · after making room by unloading another model' : 'After unloading another model'));
    case 'queue': return h('span', h('span.mut', 'Would wait in the queue'), plan.reason ? h('span.sub', plan.reason) : null);
    case 'backoff': return h('span.err-t', 'Held back after a failed load', backoff ? [', ', until(backoff.until)] : null, plan.reason ? h('span.sub', plan.reason) : null);
    case 'reject': return h('span.err-t', 'Cannot be placed', plan.reason ? h('span.sub', { style: 'color:inherit' }, plan.reason) : null);
  }
  return h('span.mut', plan.reason || 'Not known');
}

function notLoaded(st, profiles, lastSeen, historyOn, error) {
  const loaded = new Set((st.instances || []).map(i => i.profile));
  const queued = new Set((st.queue || []).map(e => e.profile));
  const backed = new Set((st.backoffs || []).map(b => b.profile));
  const rows = (profiles || []).filter(p => !loaded.has(p.id));
  let body;
  if (error) body = empty('The profile list could not be read', error);
  else if (!profiles) body = skeleton(4);
  else if (!profiles.length) body = empty('No profiles yet', 'A profile names a model file and how to load it. Add one under Configure, Profiles.');
  else if (!rows.length) body = empty('Every profile is loaded', 'There is nothing left to place.');
  else {
    body = table({
      minWidth: 900, rows, key: p => p.id,
      columns: [
        { label: 'Profile', render: p => h('span', h('span.mono.nowrap', { style: 'font-weight:500' }, p.id),
          (p.aliases || []).length ? h('span.sub', 'also ' + p.aliases.join(', ')) : null) },
        { label: 'Status', cls: 'tight', render: p => (queued.has(p.id) ? h('a', { href: href('queue') }, badge('Queued', 'q'))
          : backed.has(p.id) ? badge('Held back', 'aside', 'A load failed; new requests are refused for a while')
            : badge('Not loaded', 'n')) },
        { label: 'Needs', cls: 'r', title: 'Memory the model needs on the nodes', render: p => (p.needs_mib
          ? h('span', { title: p.needs_basis || '' }, mib(p.needs_mib), h('span.sub', p.needs_measured ? 'measured' : 'estimated'))
          : h('span.mut', DASH)) },
        { label: 'Context', cls: 'r', render: p => ctx(p.ctx) },
        { label: 'Where it would go', render: p => whereCell(p, st) },
        { label: 'Last loaded', cls: 'tight', title: historyOn ? 'From the event history' : 'History is off, so past loads are not known',
          render: p => (lastSeen[p.id] ? h('time', { title: (lastSeen[p.id].unloaded ? 'Unloaded ' : 'Loaded ') + dateTime(lastSeen[p.id].ts) }, agoText(lastSeen[p.id].ts))
            : h('span.mut', historyOn ? 'Not in the history' : DASH)) },
      ],
    });
  }
  return h('section.card', { key: 'not-loaded' },
    h('div.ch', h('h2', 'Not loaded'), h('span.note', rows.length ? 'What a request for each profile would do now' : null)),
    body);
}

// ---- the view ----

registerView({
  id: 'models', title: 'Models',
  mount(el) {
    let profiles = null, profilesError = '', lastSeen = {}, sig = '', timer = 0, alive = true;

    const loadProfiles = async () => {
      try {
        const r = await api.get('/api/profiles');
        profiles = r.profiles || [];
        profilesError = '';
      } catch (e) { profilesError = e.message; }
      if (alive) view.refresh();
    };
    const note = e => {
      if (!e || !e.profile) return;
      const unloaded = e.type === 'instanceUnloaded';
      if (!unloaded && !(e.type === 'loadFinished' && e.data && e.data.outcome === 'ready')) return;
      if (!lastSeen[e.profile] || lastSeen[e.profile].ts < e.ts) lastSeen[e.profile] = { ts: e.ts, unloaded };
    };
    const loadHistory = async () => {
      try {
        const r = await api.get('/api/history', { type: 'loadFinished,instanceUnloaded', limit: 1000 });
        for (const e of r.events || []) note(e);
        if (alive) view.refresh();
      } catch { /* the column stays empty */ }
    };
    // The plans in /api/profiles depend on what is loaded, queued and online:
    // ask again when that picture changes (pushed by the WebSocket, not a timer).
    const picture = st => JSON.stringify([(st.instances || []).map(i => [i.profile, i.state]), (st.queue || []).map(q => [q.profile, q.claimed]),
      (st.backoffs || []).map(b => b.profile), (st.quarantines || []).length, ((st.view || {}).nodes || []).map(n => [n.online, n.eligible, n.holder]),
      (st.view || {}).stale, st.config_revision]);
    const offStatus = on('status', st => {
      const s = picture(st || {});
      if (s === sig) return;
      const first = sig === '';
      sig = s;
      clearTimeout(timer);
      if (first) loadProfiles(); else timer = setTimeout(loadProfiles, 500);
    });
    const offEvents = listen('event', e => { note(e); });
    const offConfig = listen('config', () => loadProfiles());

    const render = () => {
      const st = state.status;
      if (!st) return [pageHead('Models'), staleBanner(), h('section.card', skeleton(4))];
      const insts = st.instances || [];
      const ready = insts.filter(i => i.state !== 'starting').length, loading = insts.length - ready;
      const notN = profiles ? profiles.filter(p => !insts.some(i => i.profile === p.id)).length : null;
      const failed = (st.recent_loads || []).filter(l => l.outcome && l.outcome !== 'ready');
      return [
        pageHead('Models',
          [h('b', num(ready)), ' ready · ', h('b', num(loading)), ' loading', notN != null ? [' · ', h('b', num(notN)), ' not loaded'] : null],
          insts.some(i => i.state !== 'starting') ? legend([['m', 'Model'], ['k', 'KV cache'], ['c', 'Compute']]) : null),
        staleBanner(),
        st.configured === false ? h('div.banner.info', { role: 'status' },
          h('span.grow', h('b', 'No configuration yet. '), 'The scheduler has no profiles and no tiers, so it cannot place a model. Import a configuration file with ',
            h('span.mono', 'llm-scheduler import-config'), ' or add tiers and profiles under Configure.'),
          h('a.btn.sm', { href: href('profiles') }, 'Open Profiles')) : null,
        failed.map(l => h('div.banner.err', { key: 'fail-' + l.session_id, role: 'status' },
          h('span.grow', h('span.mono', l.model), l.outcome === 'cancelled' ? ' stopped loading ' : ' failed to load ', ago(l.finished_at),
            l.load_s ? ` after ${dur(l.load_s)}` : '', l.reason ? ': ' + l.reason : '.'),
          h('a', { href: href('activity', null, { session: l.session_id }) }, 'See what happened'))),
        insts.length ? insts.map(inst => {
          const same = insts.filter(o => o !== inst && inst.model_file && o.model_file === inst.model_file).map(o => o.profile);
          return inst.state === 'starting' ? loadingCard(st, inst, same) : readyCard(st, inst, same);
        }) : st.configured === false ? null : h('section.card', { key: 'none' }, empty('No models are loaded',
          'A model loads when a request asks for it, and unloads after it has been idle for a while. The profiles below can be placed now.')),
        notLoaded(st, profiles, lastSeen, !state.hello || state.hello.history !== 'off', profilesError),
      ];
    };
    const view = liveView(el, render, ['status', 'tick', 'conn', 'hello']);
    if (state.status) { sig = picture(state.status); loadProfiles(); }
    loadHistory();
    return { unmount() { alive = false; clearTimeout(timer); offStatus(); offEvents(); offConfig(); view.unmount(); } };
  },
});
