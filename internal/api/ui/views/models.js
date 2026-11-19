// Models (the home screen). A model is a model FILE the cluster has, not a
// profile: a profile is one configuration of a model, and a benchmark matrix
// makes dozens of them. The home shows what is loaded now, one card per
// instance, then the catalog: one row per model with its picture and its last
// loads as a small chart. Clicking a model opens its page
// (views/models/detail.js). Read-only: models load when a request asks.
//
// Reads: WebSocket "status" (instances, view.nodes, recent_loads) for the
// cards; the catalog from core/catalog.js, which the live channel fills
// (topic "models") and keeps current, with REST GET /api/models as the
// fallback. The page draws its shell at once and never waits for either.
// Routes: #/models, #/models/<model key>

import { h } from '../core/dom.js';
import { registerView, href, navigate } from '../core/router.js';
import { state } from '../core/store.js';
import { liveView, pageHead, legend, table, chips, empty, skeleton } from '../core/ui.js';
import { num, gb, dur, ago, plural, agoText, dateTime, DASH } from '../core/format.js';
import { staleBanner, machines, shortReason } from '../core/status.js';
import { catalog, onCatalog, needCatalog } from '../core/catalog.js';
import { avatar } from '../core/avatar.js';
import { loadSpark, tipText } from '../core/charts.js';
import { instanceCard, sizeLine, stateBadge, fitOf, cannotRun, runsOnCell } from './models/common.js';
import { mountDetail } from './models/detail.js';

const FILTERS = [
  ['all', 'All', () => true],
  ['loaded', 'Loaded', m => m.state !== 'not_loaded'],
  ['one', 'Fits on one', (m, st) => fitOf(m, st).kind === 'one'],
  ['several', 'Needs several', (m, st) => fitOf(m, st).kind === 'several'],
  ['none', 'Cannot run', (m, st) => fitOf(m, st).kind === 'none' || !!cannotRun(m)],
];

// loadTip(mark, st) -> the tooltip of one mark of the "last loads" chart.
export function loadTip(l, st) {
  const when = `${agoText(l.at)} · ${dateTime(l.at)}`;
  if (l.outcome === 'refused') {
    return tipText(l.count > 1 ? `${num(l.count)} requests refused` : 'A request was refused', [when, l.profile, shortReason(l.reason) || null, 'Nothing was loaded']);
  }
  const head = l.outcome === 'ready' ? 'Loaded' : l.outcome === 'cancelled' ? 'Load cancelled' : 'Load failed';
  const figs = [l.load_s != null ? dur(l.load_s) : null, l.mb_s ? `${num(l.mb_s)} MB/s` : null].filter(Boolean).join(' · ');
  return tipText(head, [when, l.profile, l.nodes ? 'On ' + machines(st, l.nodes) : null, figs || null]);
}

// The last loads of a model: the chart, and the newest figure beside it.
function lastLoads(m, st) {
  const recent = (m.loads && m.loads.recent) || [];
  if (!recent.length) return h('span.quiet', m.history === false ? DASH : 'Never loaded');
  const marks = recent.map(l => Object.assign({}, l, { tip: loadTip(l, st) }));
  const loads = recent.filter(l => l.outcome !== 'refused');
  const l = recent[0];
  let latest;
  if (l.outcome === 'refused') latest = h('span.ll-fig', h('span.warn-t', 'Refused'), h('span.sub', agoText(l.at)));
  else if (l.outcome !== 'ready') latest = h('span.ll-fig', h('span.err-t', l.outcome === 'cancelled' ? 'Cancelled' : 'Failed'), h('span.sub', agoText(l.at)));
  else {
    latest = h('span.ll-fig', { title: `The newest load: ${l.profile}, ${dateTime(l.at)}` },
      h('span', l.load_s != null ? dur(l.load_s) : DASH),
      h('span.sub', [l.mb_s ? `${num(l.mb_s)} MB/s` : null, machines(st, l.nodes)].filter(Boolean).join(' · ')));
  }
  const said = `Last ${plural(loads.length, 'load')}, oldest first: ` + loads.slice().reverse().map(x => (x.outcome === 'ready' ? `${dur(x.load_s)} on ${x.nodes}` : x.outcome)).join(', ');
  return h('span.ll', loadSpark(marks, { label: said }), latest);
}

function catalogCard(st, models, error, filter, query, setFilter, searchBox) {
  let body;
  const counts = {};
  for (const [id, , test] of FILTERS) counts[id] = (models || []).filter(m => test(m, st)).length;
  const q = (query || '').trim().toLowerCase();
  const test = (FILTERS.find(f => f[0] === filter) || FILTERS[0])[2];
  const rows = (models || []).filter(m => test(m, st) && (!q || [m.name, m.raw_name, m.file, m.family, m.publisher, m.quant, m.parameters].concat((m.profiles || []).map(p => p.id)).some(x => String(x || '').toLowerCase().includes(q))));
  if (error && !models) body = empty('The model list could not be read', error);
  else if (!models) body = skeleton(6);
  else if (!models.length) body = empty('No models yet', 'A model appears here once a profile names a model file, or a GGUF file is put in the models directory. Add a profile under Configure, Profiles.');
  else if (!rows.length) body = empty('No model matches', q ? `Nothing matches "${query}". Search looks at the name, the file name and the configuration names.` : 'No model is in this group right now.');
  else {
    body = table({
      minWidth: 1080, rows, key: m => m.key, onRow: m => navigate('models', [m.key]),
      columns: [
        { label: 'Model', cls: 'mname', render: m => h('span.named', avatar(m, 40),
          h('span.grow', h('a.name', { href: href('models', [m.key]), title: m.file || m.name }, m.name),
            h('span.sub', m.publisher ? [m.publisher, ' · '] : null, sizeLine(m), m.kind === 'llama' && !m.exists ? h('span.err-t', ' · file missing') : null))) },
        { label: 'State', cls: 'tight', render: m => stateBadge(m, st) },
        { label: 'Needs', cls: 'r', title: 'The memory its lightest configuration needs on the nodes', render: m => {
          const f = fitOf(m, st);
          return f.need ? h('span', { title: f.title }, gb(f.need), h('span.sub', f.sub)) : h('span.mut', DASH);
        } },
        { label: 'Runs on', title: 'The fewest machines it fits on', render: m => runsOnCell(m, st) },
        { label: 'Configurations', cls: 'r', title: 'Profiles that load this model; the generated ones were made for benchmarks', render: m => {
          const hand = m.profile_count - m.generated_count;
          return h('span', num(m.profile_count), m.profile_count ? h('span.sub', !m.generated_count ? 'all by hand' : hand ? `${num(hand)} by hand` : 'all generated') : null);
        } },
        { label: 'Last loads', title: 'One bar per load, oldest left, newest right. The height is the load time on this row’s own scale; the pips under a bar count the machines it loaded onto. Coral: failed. A hollow amber stub: a refused request.',
          render: m => lastLoads(m, st) },
        { label: 'Last used', cls: 'tight', render: m => (m.state !== 'not_loaded' ? h('span.ok-t', { style: 'font-weight:700' }, 'Now') : ago(m.last_active, m.history === false ? DASH : 'Never')) },
      ],
    });
  }
  const profiles = (models || []).reduce((s, m) => s + m.profile_count, 0);
  return h('section.card', { key: 'catalog' },
    h('div.ch', h('h2', 'All models'), models ? h('span.note', `${plural(models.length, 'model')} · ${plural(profiles, 'configuration')}`) : h('span.note', 'Reading the catalog…')),
    models && models.length ? h('div.toolbar', searchBox,
      chips(FILTERS.map(([id, label]) => ({ id, label: id === 'one' ? 'Fits on ' + machines(st, 1).replace(/^1 /, 'one ') : label, count: id === 'all' ? null : counts[id] })),
        filter, setFilter, 'Filter the models')) : null,
    body,
    models && rows && rows.some(m => ((m.loads || {}).recent || []).length) ? h('div.cs.viz-legend', { key: 'll-legend' },
      h('span', h('i.sw.sw-mint'), 'Loaded: the bar is the load time'),
      h('span', h('i.sw.sw-coral'), 'Failed or cancelled'),
      h('span', h('i.sw.sw-amber.hollow'), 'Refused request'),
      h('span', h('i.pips'), `Pips: ${machines(st, 1).replace(/^1 /, '')}s it loaded onto`)) : null);
}

function mountHome(el) {
  let filter = 'all', query = '';
  const open = new Set();
  const toggle = k => { if (open.has(k)) open.delete(k); else open.add(k); view.refresh(); };
  const offCatalog = onCatalog(() => view.refresh());

  // built once: morph leaves it alone, so the caret stays while the list redraws
  const searchBox = h('input.input', { type: 'search', placeholder: 'Search models', 'aria-label': 'Search models', autocomplete: 'off', spellcheck: 'false', 'data-keep': '', key: 'search' });
  searchBox.addEventListener('input', () => { query = searchBox.value; view.refresh(); });

  const render = () => {
    const st = state.status;
    const models = catalog.models;
    const view_ = st || {};
    const insts = view_.instances || [];
    const ready = insts.filter(i => i.state !== 'starting').length, loading = insts.length - ready;
    const failed = (view_.recent_loads || []).filter(l => l.outcome && l.outcome !== 'ready');
    const sub = st
      ? [h('b', num(ready)), ' ready · ', h('b', num(loading)), ' loading', models ? [' · ', h('b', num(models.length)), ' in the catalog'] : null]
      : models ? [h('b', num(models.length)), ' in the catalog'] : null;
    return [
      pageHead('Models', sub, ready ? legend([['m', 'Model'], ['k', 'KV cache'], ['c', 'Compute']]) : null),
      staleBanner(),
      view_.configured === false ? h('div.banner.info', { role: 'status' },
        h('span.grow', h('b', 'No configuration has been imported yet. '), 'The scheduler runs with its defaults and has no profiles and no tiers, so it cannot place a model. Import a configuration file with ',
          h('span.mono', 'llm-scheduler import-config'), ' or add tiers and profiles under Configure.'),
        h('a.btn.sm', { href: href('profiles') }, 'Open Profiles')) : null,
      failed.map(l => h('div.banner.err', { key: 'fail-' + l.session_id, role: 'status' },
        h('span.grow', h('span.mono', l.model), l.outcome === 'cancelled' ? ' stopped loading ' : ' failed to load ', ago(l.finished_at),
          l.load_s ? ` after ${dur(l.load_s)}.` : '.'),
        h('a', { href: href('activity', null, { session: l.session_id }) }, 'See what happened'))),
      !st ? h('section.card', { key: 'none' }, h('div.strip', skeleton(1)))
        : insts.length ? insts.map(inst => instanceCard(st, inst, catalog.byProfile[inst.profile] || catalog.byKey[inst.model_file] || null, open, toggle))
          : view_.configured === false ? null : h('section.card', { key: 'none' }, h('div.strip', h('b', 'Nothing is loaded right now.'),
            h('span', 'A model loads when a request asks for it, and unloads after it has been idle for a while.'))),
      view_.configured === false && models && !models.length ? null
        : catalogCard(view_, models, catalog.error, filter, query, id => { filter = id; view.refresh(); }, searchBox),
    ];
  };
  const view = liveView(el, render, ['status', 'tick', 'conn', 'hello']);
  needCatalog();
  return { unmount() { offCatalog(); view.unmount(); } };
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
