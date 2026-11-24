// Models (the home screen of the new shell; design: Models board 1, "Models
// list and sidebar"). A model is a model FILE, not a profile (README rule 9).
// The page shows what is loaded now (one card per instance, as before), then
// every model file in one list with its size, its state, its configurations
// and when it was last used, and the disk the files take together. A row
// opens the model's side panel (?model=<key>): its facts, its benchmarks, its
// hand-written configurations (the generated ones counted), and a link to its
// full page (#/models/<key>, views/models/detail.js, unchanged).
//
// Left out until their backend exists (plan, U4 and U5): Pin and pinned
// models, Delete, Deleted models, Add a model, downloads in progress, free
// disk space, the "reports" count of the new benchmark model, and a
// profile's unload-when-idle in the side panel (not in the catalog).
//
// Reads: the catalog (core/catalog.js; the live channel's "models" topic,
// REST GET /api/models as its fallback) and the WebSocket status for the
// instance cards. Read-only: models load when a request asks for one.
// Routes: #/models, #/models?model=<key> (side panel), #/models/<key> (page)

import { h } from '../core/dom.js';
import { registerView, href, navigate } from '../core/router.js';
import { state } from '../core/store.js';
import { liveView, pageHead, legend, chips, empty, skeleton, searchField, sidePanel, facts, badge } from '../core/ui.js';
import { num, gb, dur, ago, plural, agoText, dateTime, bytes, DASH } from '../core/format.js';
import { staleBanner, machines, shortReason } from '../core/status.js';
import { catalog, onCatalog, needCatalog } from '../core/catalog.js';
import { logsLink, profileQuery, padded } from '../core/logs.js';
import { avatar } from '../core/avatar.js';
import { tipText } from '../core/charts.js';
import { instanceCard, sizeLine, stateBadge, fitOf, cannotRun, runsOnCell } from './models/common.js';
import { mountDetail } from './models/detail.js';

// loadLogs(st, load) -> a link to the log lines of a load, or null while
// central logging is off (as on the classic page).
function loadLogs(st, l) {
  if (!l.finished_at) return null;
  const [from, to] = padded(l.finished_at - (l.load_s || 0) * 1000, l.finished_at);
  return logsLink(st, 'Logs', profileQuery(l.model), from, to, 'Open the log lines of this load in a new tab');
}

const FILTERS = [
  ['all', 'All', () => true],
  ['loaded', 'Loaded', m => m.state !== 'not_loaded'],
  ['one', 'Fits on one', (m, st) => fitOf(m, st).kind === 'one'],
  ['several', 'Needs several', (m, st) => fitOf(m, st).kind === 'several'],
  ['none', 'Cannot run', (m, st) => fitOf(m, st).kind === 'none' || !!cannotRun(m)],
];

// loadTip(mark, st) -> the tooltip of one mark of a "last loads" chart. Kept
// with the classic name (views/models.js exports it).
export function loadTip(l, st) {
  const when = `${agoText(l.at)} · ${dateTime(l.at)}`;
  if (l.outcome === 'refused') {
    return tipText(l.count > 1 ? `${num(l.count)} requests refused` : 'A request was refused', [when, l.profile, shortReason(l.reason) || null, 'Nothing was loaded']);
  }
  const head = l.outcome === 'ready' ? 'Loaded' : l.outcome === 'cancelled' ? 'Load cancelled' : 'Load failed';
  const figs = [l.load_s != null ? dur(l.load_s) : null, l.mb_s ? `${num(l.mb_s)} MB/s` : null].filter(Boolean).join(' · ');
  return tipText(head, [when, l.profile, l.nodes ? 'On ' + machines(st, l.nodes) : null, figs || null]);
}

const lastUsed = m => (m.state !== 'not_loaded' ? h('span.ok-t', { style: 'font-weight:700' }, 'Now') : ago(m.last_active, m.history === false ? DASH : 'Never'));

// configsCell(m) -> "5 · 2 by hand"
function configsCell(m) {
  const hand = m.profile_count - m.generated_count;
  return h('span', num(m.profile_count), m.profile_count ? h('span.sub', !m.generated_count ? 'all by hand' : hand ? `${num(hand)} by hand` : 'all generated') : null);
}

// profileRows(m) -> the hand-written configurations of a model, the generated
// ones counted (README rule 9).
function profilesPart(m) {
  const ps = m.profiles || [];
  const hand = ps.filter(p => !p.generated);
  const gen = ps.length - hand.length;
  const need = p => (p.needs_mib ? h('span', gb(p.needs_mib), h('span.mut', p.needs_measured ? ' measured' : ' estimated')) : h('span.mut', DASH));
  return h('section.zpart', { key: 'profiles' },
    h('h3', 'Configurations'),
    hand.length ? h('div.scroll', h('table.tbl', { style: 'min-width:420px' },
      h('thead', h('tr', h('th', { scope: 'col' }, 'Profile'), h('th.r', { scope: 'col' }, 'Context'), h('th.r', { scope: 'col' }, 'Needs'), h('th.r', { scope: 'col' }, 'Parallel'))),
      h('tbody', hand.map(p => h('tr', { key: p.id },
        h('td', h('span.mono', p.id), p.description ? h('span.sub', p.description) : null, p.host === 'node'
          ? h('span.sub', { title: 'llama-server runs on this node, started by its agent' }, 'hosted on ' + (p.host_node || 'its first forced node'),
            p.host_overhead_mib ? ` · +${gb(p.host_overhead_mib)} for the host` : null, p.host_problem ? h('span.err-t', ' · ' + p.host_problem) : null)
          : null),
        h('td.r', p.ctx ? num(p.ctx) : DASH),
        h('td.r', need(p)),
        h('td.r', p.parallel ? num(p.parallel) : DASH))))))
      : h('span.mut', ps.length ? 'Every configuration of this model was generated.' : 'No configuration uses this file yet.'),
    gen ? h('span.mut', `${plural(gen, 'generated configuration')} (benchmark matrices), listed on the model's page and under Profiles.`) : null,
    h('div.row', { style: 'gap:12px' }, h('a', { href: href('profiles') }, 'Edit under Profiles')));
}

// modelPanel(st, m) -> the side panel of one model.
function modelPanel(st, m) {
  const f = fitOf(m, st);
  const b = m.bench || {};
  const l = m.loads || {};
  return [
    h('section.zpart', { key: 'head' },
      h('span.named', avatar(m, 48), h('span.grow', h('b', m.name), h('span.sub', m.publisher ? [m.publisher, ' · '] : null, sizeLine(m)))),
      h('div.row', { style: 'gap:10px;flex-wrap:wrap' }, stateBadge(m, st), m.kind === 'llama' && !m.exists ? badge('File missing', 'fail') : null),
      h('div', h('span.mut', 'Runs on: '), runsOnCell(m, st), f.need ? [h('span.mut', ' · needs '), h('span', { title: f.title }, gb(f.need))] : null)),
    h('section.zpart', { key: 'facts' }, facts([
      m.file ? ['File', h('span.mono', m.file)] : null,
      m.kind === 'llama' ? ['On disk', h('span', m.exists ? (m.size_bytes ? bytes(m.size_bytes) : DASH) : 'missing')] : null,
      m.family ? ['Family', h('span', m.family)] : null,
      m.parameters ? ['Parameters', h('span', m.parameters)] : null,
      m.quant ? ['Quantization', h('span', m.quant)] : null,
      m.context_length ? ['Trained context', h('span', num(m.context_length)), 'The context the model was trained with, from its file'] : null,
      ['Last used', lastUsed(m)],
      m.requests_served != null ? ['Requests served', h('span', num(m.requests_served))] : null,
      l.count ? ['Loads', h('span', num(l.count), l.failed ? h('span.err-t', ` · ${num(l.failed)} failed`) : null)] : null,
    ])),
    b.benchmarks || b.runs ? h('section.zpart', { key: 'bench' }, h('h3', 'Benchmarks'),
      h('span', plural(b.benchmarks || 0, 'benchmark'), ' · ', plural(b.runs || 0, 'run')),
      h('a', { href: href('benchmarks') }, 'Open Benchmarks')) : null,
    profilesPart(m),
    h('div', { key: 'page' }, h('a.btn', { href: href('models', [m.key]) }, 'Open the model page')),
  ];
}

function listPanel(st, models, error, r, query, setQuery, onFind) {
  const filter = FILTERS.some(f => f[0] === r.query.show) ? r.query.show : 'all';
  const counts = {};
  for (const [id, , test] of FILTERS) counts[id] = (models || []).filter(m => test(m, st)).length;
  const q = (query || '').trim().toLowerCase();
  const test = (FILTERS.find(f => f[0] === filter) || FILTERS[0])[2];
  const rows = (models || []).filter(m => test(m, st) && (!q || [m.name, m.raw_name, m.file, m.family, m.publisher, m.quant, m.parameters]
    .concat((m.profiles || []).map(p => p.id)).some(x => String(x || '').toLowerCase().includes(q))));
  let body;
  if (error && !models) body = empty('The model list could not be read', error);
  else if (!models) body = skeleton(6);
  else if (!models.length) body = empty('No models yet', 'A model appears here once a profile names a model file, or a GGUF file is put in the models directory. Add a profile under Configure, Profiles.');
  else if (!rows.length) body = empty('No model matches', q ? `Nothing matches "${query}". Search looks at the name, the file name and the configuration names.` : 'No model is in this group right now.');
  else {
    const open = key => setQuery({ model: key });
    body = h('div.scroll', h('table.tbl.models', { style: 'min-width:940px' },
      h('thead', h('tr', h('th', { scope: 'col' }, 'Model'), h('th', { scope: 'col' }, 'State'), h('th.r', { scope: 'col' }, 'Size'),
        h('th.r', { scope: 'col', title: 'The memory its lightest configuration needs on the nodes' }, 'Needs'),
        h('th', { scope: 'col' }, 'Runs on'), h('th.r', { scope: 'col', title: 'Profiles that load this model; the generated ones were made for benchmarks' }, 'Configurations'),
        h('th', { scope: 'col' }, 'Last used'))),
      h('tbody', rows.map(m => h('tr', { key: m.key, class: 'click' + (r.query.model === m.key ? ' sel' : ''), tabindex: '0', 'aria-label': m.name,
        onclick: () => open(m.key), onkeydown: e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); open(m.key); } } },
      h('td', h('span.named', avatar(m, 32), h('span.grow', h('b', { title: m.file || m.name }, m.name),
        h('span.sub', m.publisher ? [m.publisher, ' · '] : null, [m.parameters, m.quant].filter(Boolean).join(' · ') || (m.kind === 'command' ? 'A program on this machine' : ''),
          m.kind === 'llama' && !m.exists ? h('span.err-t', ' · file missing') : null)))),
      h('td', stateBadge(m, st)),
      h('td.r', m.kind === 'llama' && m.size_bytes ? bytes(m.size_bytes) : DASH),
      h('td.r', { 'data-col': 'needs' }, needsCell(m, st)),
      h('td', runsOnCell(m, st)),
      h('td.r', configsCell(m)),
      h('td', lastUsed(m)))))));
  }
  const disk = (models || []).filter(m => m.kind === 'llama' && m.exists).reduce((s, m) => s + (m.size_bytes || 0), 0);
  return h('section.panel', { key: 'catalog', 'aria-label': 'All models' },
    h('div.ch.split', h('h2', 'All models'),
      models ? h('span.mut', `${plural(models.length, 'model')}` + (disk ? ` · ${bytes(disk)} on disk` : '')) : h('span.mut', 'Reading the catalog…')),
    models && models.length ? h('div.zfilters',
      h('div', chips(FILTERS.map(([id, label]) => ({ id, label: id === 'one' ? 'Fits on ' + machines(st, 1).replace(/^1 /, 'one ') : label, count: id === 'all' ? null : counts[id] })),
        filter, id => setQuery({ show: id === 'all' ? '' : id }), 'Filter the models')),
      h('div', searchField(query, onFind, 'Search models', 'model-search'))) : null,
    body);
}

// The Needs cell, as the classic list draws it: the memory of the lightest
// configuration (fit.need_mib) and how it is known (measured, estimated, set
// in the profile); a dash when it is not known yet.
function needsCell(m, st) {
  const f = fitOf(m, st);
  return f.need ? h('span', { title: f.title || null }, gb(f.need), h('span.sub', f.sub)) : h('span.mut', DASH);
}

function mountHome(el, route) {
  let r = route, query = '', side = null;
  const open = new Set();
  const toggle = k => { if (open.has(k)) open.delete(k); else open.add(k); view.refresh(); };
  const offCatalog = onCatalog(() => { view.refresh(); if (side) side.redraw(); });
  const setQuery = q => navigate('models', [], Object.assign({}, r.query, q), true);

  // The side panel of the open model (?model=<key>).
  const syncSide = () => {
    const key = r.query.model || '';
    if (!key) { if (side) { const s = side; side = null; s.close(); } return; }
    if (side && side.key === key) { side.redraw(); return; }
    if (side) { const s = side; side = null; s.close(); }
    const find = () => (catalog.models || []).find(m => m.key === key) || null;
    const body = () => {
      const m = find();
      if (m) return modelPanel(state.status || {}, m);
      return catalog.models ? h('section.card', empty('This model is not in the catalog', 'Its file may have gone. Close this panel to see the models there are.')) : skeleton(3);
    };
    const p = sidePanel({ title: () => { const m = find(); return m ? m.name : 'Model'; }, body, onClose: () => { if (side === p) { side = null; setQuery({ model: '' }); } } });
    p.key = key;
    side = p;
  };

  const render = () => {
    const st = state.status;
    const models = catalog.models;
    const view_ = st || {};
    const insts = view_.instances || [];
    const ready = insts.filter(i => i.state !== 'starting').length, loading = insts.length - ready;
    const failed = (view_.recent_loads || []).filter(l => l.outcome && l.outcome !== 'ready');
    if (side) side.redraw();
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
        h('a', { href: href('activity', null, { session: l.session_id }) }, 'See what happened'),
        loadLogs(st, l))),
      !st ? h('section.card', { key: 'none' }, h('div.strip', skeleton(1)))
        : insts.length ? insts.map(inst => instanceCard(st, inst, catalog.byProfile[inst.profile] || catalog.byKey[inst.model_file] || null, open, toggle))
          : view_.configured === false ? null : h('section.card', { key: 'none' }, h('div.strip', h('b', 'Nothing is loaded right now.'),
            h('span', 'A model loads when a request asks for it, and unloads after it has been idle for a while.'))),
      view_.configured === false && models && !models.length ? null
        : listPanel(view_, models, catalog.error, r, query, setQuery, v => { query = v; view.refresh(); }),
    ];
  };
  const view = liveView(el, render, ['status', 'tick', 'conn', 'hello']);
  needCatalog();
  syncSide();
  return {
    update(nr) { r = nr; syncSide(); view.refresh(); },
    unmount() { offCatalog(); if (side) { const s = side; side = null; s.close(); } view.unmount(); },
  };
}

registerView({
  id: 'models', title: 'Models',
  mount(el, route) {
    // one view, two pages: the home (with its side panel), and a model's own page under #/models/<key>
    let key = route.rest.join('/');
    let page = key ? mountDetail(el, key, route) : mountHome(el, route);
    return {
      update(r) {
        const k = r.rest.join('/');
        if (k === key) { if (page.update) page.update(r); return; }
        page.unmount();
        el.replaceChildren();
        window.scrollTo(0, 0);
        key = k;
        page = key ? mountDetail(el, key, r) : mountHome(el, r);
      },
      unmount() { page.unmount(); },
    };
  },
});
