// Models (the home screen). A model is a model FILE the cluster has, not a
// profile: a profile is one configuration of a model, and a benchmark matrix
// makes dozens of them. The home shows what is loaded now, one card per
// instance, then the catalog: one row per model. Clicking a model opens its
// page (views/models/detail.js). Read-only: models load when a request asks.
//
// Reads: WebSocket "status" (instances, view.nodes, recent_loads) for the
// cards; REST GET /api/models (the catalog) on open and again when the
// placement picture in the status changes or the configuration changes.
// Routes: #/models, #/models/<model key>

import { h } from '../core/dom.js';
import { registerView, href, navigate } from '../core/router.js';
import { state, on, listen } from '../core/store.js';
import * as api from '../core/api.js';
import { liveView, pageHead, legend, table, chips, empty, skeleton } from '../core/ui.js';
import { num, gb, dur, ago, plural, agoText, dateTime, DASH } from '../core/format.js';
import { staleBanner, machines } from '../core/status.js';
import { instanceCard, sizeLine, stateBadge, fitOf, cannotRun } from './models/common.js';
import { mountDetail } from './models/detail.js';

// picture(st) changes when something that the catalog depends on changes:
// what is loaded, queued or backed off, and which nodes are usable.
export const picture = st => JSON.stringify([(st.instances || []).map(i => [i.profile, i.state]), (st.queue || []).map(q => [q.profile, q.claimed]),
  (st.backoffs || []).map(b => b.profile), (st.quarantines || []).length, ((st.view || {}).nodes || []).map(n => [n.online, n.eligible, n.holder]),
  (st.view || {}).stale, st.config_revision]);

const FILTERS = [
  ['all', 'All', () => true],
  ['loaded', 'Loaded', m => m.state !== 'not_loaded'],
  ['one', 'Fits on one', (m, st) => fitOf(m, st).kind === 'one'],
  ['several', 'Needs several', (m, st) => fitOf(m, st).kind === 'several'],
  ['none', 'Cannot run', (m, st) => fitOf(m, st).kind === 'none' || !!cannotRun(m)],
];

function lastLoad(m, st) {
  const l = m.loads && m.loads.last;
  if (!l) return h('span.quiet', m.history === false ? DASH : 'Never');
  if (l.outcome !== 'ready') return h('span', h('span.err-t', l.outcome === 'cancelled' ? 'Cancelled' : 'Failed'), h('span.sub', agoText(l.at)));
  return h('span', { title: `${l.profile} on ${(l.nodes || []).join(', ')}, ${dateTime(l.at)}` },
    h('span', { style: 'font-variant-numeric:tabular-nums;white-space:nowrap' }, l.load_s != null ? dur(l.load_s) : DASH, l.mb_s ? h('span.quiet', ` · ${num(l.mb_s)} MB/s`) : null),
    h('span.sub', `on ${machines(st, (l.nodes || []).length)}`));
}

function catalog(st, models, error, filter, query, setFilter, setQuery, searchBox) {
  let body;
  const counts = {};
  for (const [id, , test] of FILTERS) counts[id] = (models || []).filter(m => test(m, st)).length;
  const q = (query || '').trim().toLowerCase();
  const test = (FILTERS.find(f => f[0] === filter) || FILTERS[0])[2];
  const rows = (models || []).filter(m => test(m, st) && (!q || [m.name, m.file, m.family, m.publisher, m.quant, m.parameters].concat((m.profiles || []).map(p => p.id)).some(x => String(x || '').toLowerCase().includes(q))));
  if (error && !models) body = empty('The model list could not be read', error);
  else if (!models) body = skeleton(5);
  else if (!models.length) body = empty('No models yet', 'A model appears here once a profile names a model file, or a GGUF file is put in the models directory. Add a profile under Configure, Profiles.');
  else if (!rows.length) body = empty('No model matches', q ? `Nothing matches "${query}". Search looks at the name, the file name and the configuration names.` : 'No model is in this group right now.');
  else {
    body = table({
      minWidth: 920, rows, key: m => m.key, onRow: m => navigate('models', [m.key]),
      columns: [
        { label: 'Model', render: m => h('span', h('a.name', { href: href('models', [m.key]), title: m.file || m.name }, m.name),
          h('span.sub', sizeLine(m), m.kind === 'llama' && !m.exists ? h('span.err-t', ' · file missing') : null)) },
        { label: 'State', cls: 'tight', render: m => stateBadge(m, st) },
        { label: 'Needs', cls: 'r', title: 'The memory its lightest configuration needs on the nodes', render: m => {
          const f = fitOf(m, st);
          return f.need ? h('span', { title: f.title }, gb(f.need), h('span.sub', f.sub)) : h('span.mut', DASH);
        } },
        { label: 'Runs on', render: m => {
          const f = fitOf(m, st), no = cannotRun(m);
          if (f.kind === 'none') return h('span', { title: f.title }, f.text);
          return h('span', { title: f.title }, f.text.replace(/^Runs on |^Should need /, ''), f.kind !== 'cmd' && f.sub === 'estimated' ? h('span.quiet', ' (estimate)') : null,
            no ? h('span.sub', no) : null);
        } },
        { label: 'Configurations', cls: 'r', title: 'Profiles that load this model; the generated ones were made for benchmarks', render: m => {
          const hand = m.profile_count - m.generated_count;
          return h('span', num(m.profile_count), m.profile_count ? h('span.sub', !m.generated_count ? 'all by hand' : hand ? `${num(hand)} by hand` : 'all generated') : null);
        } },
        { label: 'Last load', title: 'How long the newest load took, from the event history', render: m => lastLoad(m, st) },
        { label: 'Last used', cls: 'tight', render: m => (m.state !== 'not_loaded' ? h('span.ok-t', { style: 'font-weight:700' }, 'Now') : ago(m.last_active, m.history === false ? DASH : 'Never')) },
      ],
    });
  }
  const profiles = (models || []).reduce((s, m) => s + m.profile_count, 0);
  return h('section.card', { key: 'catalog' },
    h('div.ch', h('h2', 'All models'), models ? h('span.note', `${plural(models.length, 'model')} · ${plural(profiles, 'configuration')}`) : null),
    models && models.length ? h('div.toolbar', searchBox,
      chips(FILTERS.map(([id, label]) => ({ id, label: id === 'one' ? 'Fits on ' + machines(st, 1).replace(/^1 /, 'one ') : label, count: id === 'all' ? null : counts[id] })),
        filter, setFilter, 'Filter the models')) : null,
    body);
}

function mountHome(el) {
  let models = null, error = '', sig = '', timer = 0, alive = true, filter = 'all', query = '';
  const open = new Set();
  const toggle = k => { if (open.has(k)) open.delete(k); else open.add(k); view.refresh(); };

  const load = async () => {
    try { models = (await api.get('/api/models')).models || []; error = ''; } catch (e) { error = e.message; }
    if (alive) view.refresh();
  };
  const offStatus = on('status', st => {
    const s = picture(st || {});
    if (s === sig) return;
    const first = sig === '';
    sig = s;
    clearTimeout(timer);
    if (first) load(); else timer = setTimeout(load, 500);
  });
  // a finished load or an unload changes "last load" and "last used"
  const offEvents = listen('event', e => {
    if (e && (e.type === 'loadFinished' || e.type === 'instanceUnloaded' || e.type === 'requestRefused')) { clearTimeout(timer); timer = setTimeout(load, 800); }
  });
  const offConfig = listen('config', load);

  // built once: morph leaves it alone, so the caret stays while the list redraws
  const searchBox = h('input.input', { type: 'search', placeholder: 'Search models', 'aria-label': 'Search models', autocomplete: 'off', spellcheck: 'false', 'data-keep': '', key: 'search' });
  searchBox.addEventListener('input', () => { query = searchBox.value; view.refresh(); });

  const render = () => {
    const st = state.status;
    if (!st) return [pageHead('Models'), staleBanner(), h('section.card', skeleton(4))];
    const insts = st.instances || [];
    const ready = insts.filter(i => i.state !== 'starting').length, loading = insts.length - ready;
    const failed = (st.recent_loads || []).filter(l => l.outcome && l.outcome !== 'ready');
    const byFile = {};
    for (const m of models || []) { byFile[m.key] = m; for (const p of m.profiles || []) byFile['profile:' + p.id] = m; }
    return [
      pageHead('Models',
        [h('b', num(ready)), ' ready · ', h('b', num(loading)), ' loading', models ? [' · ', h('b', num(models.length)), ' in the catalog'] : null],
        ready ? legend([['m', 'Model'], ['k', 'KV cache'], ['c', 'Compute']]) : null),
      staleBanner(),
      st.configured === false ? h('div.banner.info', { role: 'status' },
        h('span.grow', h('b', 'No configuration has been imported yet. '), 'The scheduler runs with its defaults and has no profiles and no tiers, so it cannot place a model. Import a configuration file with ',
          h('span.mono', 'llm-scheduler import-config'), ' or add tiers and profiles under Configure.'),
        h('a.btn.sm', { href: href('profiles') }, 'Open Profiles')) : null,
      failed.map(l => h('div.banner.err', { key: 'fail-' + l.session_id, role: 'status' },
        h('span.grow', h('span.mono', l.model), l.outcome === 'cancelled' ? ' stopped loading ' : ' failed to load ', ago(l.finished_at),
          l.load_s ? ` after ${dur(l.load_s)}.` : '.'),
        h('a', { href: href('activity', null, { session: l.session_id }) }, 'See what happened'))),
      insts.length ? insts.map(inst => instanceCard(st, inst, byFile['profile:' + inst.profile] || byFile[inst.model_file] || null, open, toggle))
        : st.configured === false ? null : h('section.card', { key: 'none' }, h('div.strip', h('b', 'Nothing is loaded right now.'),
          h('span', 'A model loads when a request asks for it, and unloads after it has been idle for a while.'))),
      st.configured === false && models && !models.length ? null
        : catalog(st, models, error, filter, query, id => { filter = id; view.refresh(); }, null, searchBox),
    ];
  };
  const view = liveView(el, render, ['status', 'tick', 'conn', 'hello']);
  if (state.status) { sig = picture(state.status); load(); }
  return { unmount() { alive = false; clearTimeout(timer); offStatus(); offEvents(); offConfig(); view.unmount(); } };
}

registerView({
  id: 'models', title: 'Models',
  mount(el, route) {
    // one view, two pages: the home, and a model's own page under #/models/<key>
    let key = route.rest.join('/');
    let page = key ? mountDetail(el, key, route) : mountHome(el);
    return {
      update(r) {
        const k = r.rest.join('/');
        if (k === key) { if (page.update) page.update(r); return; }
        page.unmount();
        el.replaceChildren();
        window.scrollTo(0, 0);
        key = k;
        page = key ? mountDetail(el, key, r) : mountHome(el);
      },
      unmount() { page.unmount(); },
    };
  },
});
