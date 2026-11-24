// Models (the home screen of the new shell; design: board Models, "Models
// list and sidebar", phase 7: built from what the server feeds today). A
// model is a model FILE, not a profile (README rule 9).
//
// The page: what is loaded now (one card per instance), then the list as
// the board draws it: search, "Show" (all, loaded, fits on one, needs
// several, cannot run), "Sort: last used"; the sections Downloading, Pinned
// and All models ("N model files · X of Y GB used", from the catalog's disk
// block); one row per file (star, file name, size, "N profiles", last used:
// Now, 48 s ago or Never). A row opens the side panel (?model=<key>): size
// and last used, Benchmarks, what is loaded where, where it runs and what it
// needs, its facts, and its configurations (search, context, the memory
// each needs, a Now pill on a loaded one, Edit, Add profile), and the link
// to its full page (#/models/<key>, views/models/detail.js, unchanged).
//
// Drawn as on the board but not available yet, each with its reason on
// hover (p7-models-backend-STATUS.md, M1 to M6): Add a model, Downloading,
// Pinned and the star, the panel's Pinned and Delete model, deleted models.
// Left out: "added <date>", the benchmark "reports" count, "Unload when
// idle" (no data yet), the Needs column and the loaded-models badge (p4-ui).
//
// Reads: the catalog (core/catalog.js; the live channel's "models" topic,
// REST GET /api/models as its fallback); GET /api/models every 30 s for its
// disk block (the live channel's copy has none); the WebSocket status for
// the instance cards. Read-only: models load when a request asks for one.
// Routes: #/models?q=&show=&sort=&model=<key>, #/models/<key> (page)

import { h } from '../core/dom.js';
import { registerView, href, navigate } from '../core/router.js';
import { state } from '../core/store.js';
import * as api from '../core/api.js';
import { liveView, pageHead, legend, empty, skeleton, searchField, sidePanel, facts, badge, filterSelect } from '../core/ui.js';
import { num, gb, dur, ago, plural, agoText, dateTime, bytes, DASH } from '../core/format.js';
import { staleBanner, machines, shortReason } from '../core/status.js';
import { catalog, onCatalog, needCatalog } from '../core/catalog.js';
import { logsLink, profileQuery, padded } from '../core/logs.js';
import { tipText } from '../core/charts.js';
import { instanceCard, stateBadge, fitOf, cannotRun, runsOnCell } from './models/common.js';
import { mountDetail } from './models/detail.js';

// the reasons of what is drawn but not built yet (p7-models-backend-STATUS.md)
export const NOT_YET = {
  add: 'Adding a model from this page is not built yet. Put the .gguf file in the models folder and it appears here.',
  downloads: 'Downloads are not built yet: a model added from this page would show its progress here.',
  pin: 'Pins are not built yet.',
  del: 'Deleting a model is not built yet: the file stays in the models folder.',
  trash: 'Deleted models are not built yet.',
};
// off(tag, reason, attrs, ...kids) -> a control that is drawn but not available: it says why on hover and does nothing
const off = (tag, reason, attrs, ...kids) => h(tag, Object.assign({ type: 'button', 'aria-disabled': 'true', title: reason, 'data-off': '', onclick: e => { e.preventDefault(); e.stopPropagation(); } }, attrs), ...kids);
const icon = d => h('svg', { class: 'ic', viewBox: '0 0 20 20', 'aria-hidden': 'true' }, h('path', { d }));

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
const SORTS = [['used', 'last used'], ['name', 'name'], ['size', 'size']];
const usedKey = m => (m.state && m.state !== 'not_loaded' ? Infinity : m.last_active || -1);
const SORT = {
  used: (a, b) => usedKey(b) - usedKey(a) || (a.file || a.name).localeCompare(b.file || b.name),
  name: (a, b) => (a.file || a.name).localeCompare(b.file || b.name),
  size: (a, b) => (b.size_bytes || 0) - (a.size_bytes || 0) || (a.file || a.name).localeCompare(b.file || b.name),
};

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

// lastUsed(m) -> Now (loaded), Loading, "48 s ago" (exact time on hover) or Never
function lastUsed(m) {
  if (m.state === 'ready') return h('time.mdl-now', { title: m.last_active ? dateTime(m.last_active) : 'Loaded now' }, 'Now');
  if (m.state === 'loading') return h('span.mdl-now', 'Loading');
  return ago(m.last_active, m.history === false ? DASH : 'Never');
}
// profilesText(m) -> "11 profiles", "1 profile", "No profiles"
const profilesText = m => (m.profile_count ? plural(m.profile_count, 'profile') : 'No profiles');
// sizeText(m) -> "16.5 GB", or why there is none (decimal GB, as the board and the disk line)
const gbOf = v => (v / 1e9).toLocaleString('en-US', { maximumFractionDigits: 1, minimumFractionDigits: v >= 100e9 ? 0 : 1 });
const sizeText = m => (m.kind === 'command' ? 'A program on this machine' : m.size_bytes ? gbOf(m.size_bytes) + ' GB' : DASH);

// diskLine(models, disk) -> "5 model files · 50.3 of 240 GB used", amber at 90 %
function diskLine(models, disk) {
  const files = (models || []).filter(m => m.kind !== 'command' && m.exists !== false).length;
  const parts = [plural(files, 'model file')];
  let full = false, title = null;
  if (disk && disk.total_bytes) {
    const budget = disk.budget_bytes;
    const used = budget ? disk.model_bytes || 0 : disk.used_bytes;
    const of = budget || disk.total_bytes;
    parts.push(`${gbOf(used)} of ${gbOf(of)} GB used`);
    full = used / of >= 0.9;
    title = [budget ? `The model files take ${gbOf(used)} GB of their ${gbOf(budget)} GB budget (models_disk_budget_gb).`
      : `The disk of the models folder: ${gbOf(disk.used_bytes)} of ${gbOf(disk.total_bytes)} GB used.`,
    disk.free_bytes != null ? `${gbOf(disk.free_bytes)} GB free.` : null,
    !budget && disk.model_bytes != null ? `The model files take ${gbOf(disk.model_bytes)} GB.` : null,
    disk.checked_at ? `Read ${agoText(disk.checked_at)}.` : null].filter(Boolean).join(' ');
  }
  return h('span', { class: 'lbl mdl-disk' + (full ? ' full' : ''), title, 'data-disk': '' }, parts.join(' · '), full ? ' · nearly full' : null);
}

// hostLine(p) -> "hosted on <node> · +1.5 GB for the host" for a profile whose llama-server runs on a node
const hostLine = p => (p.host === 'node'
  ? h('span.sub', { title: 'llama-server runs on this node, started by its agent' }, 'hosted on ' + (p.host_node || 'its first forced node'),
    p.host_overhead_mib ? ` · +${gb(p.host_overhead_mib)} for the host` : null, p.host_problem ? h('span.err-t', ' · ' + p.host_problem) : null)
  : null);

// the side panel of one model
function modelPanel(st, m, ps) {
  const f = fitOf(m, st);
  const b = m.bench || {};
  const l = m.loads || {};
  const insts = m.instances || [];
  const loadedProfiles = new Set(insts.filter(i => i.state !== 'starting').map(i => i.profile));
  const all = m.profiles || [];
  const hand = all.filter(p => !p.generated);
  const gen = all.length - hand.length;
  const ctxs = [...new Set(hand.map(p => p.ctx).filter(Boolean))].sort((x, y) => x - y);
  const q = ps.q.trim().toLowerCase();
  const rows = hand.filter(p => (!q || p.id.toLowerCase().includes(q) || (p.description || '').toLowerCase().includes(q)) && (!ps.ctx || String(p.ctx) === ps.ctx));
  return [
    h('div.mdl-acts', { key: 'acts' },
      h('a.btn', { href: href('benchmarks'), title: b.runs ? 'Benchmarks that used a profile of this model' : 'No benchmark has used this model yet' }, 'Benchmarks ',
        h('span.mut', b.runs ? plural(b.runs, 'run') : 'none yet')),
      off('button.btn', NOT_YET.pin, { 'aria-pressed': 'false', 'data-act': 'pin' }, 'Pinned'),
      off('button.btn.red', NOT_YET.del, { 'data-act': 'delete' }, 'Delete model')),
    insts.length ? h('section.zpart', { key: 'loaded' }, h('h3', 'Loaded now'),
      insts.map(i => h('div.mdl-inst', { key: 'i-' + i.profile },
        h('span.mono', i.profile), i.state === 'starting' ? badge('Loading', 'load') : badge('Ready', 'ready'),
        (i.nodes || []).length ? h('span.mut', 'on ' + i.nodes.join(', ')) : null,
        i.in_flight ? h('span.mut', plural(i.in_flight, 'request') + ' in flight') : null))) : null,
    h('section.zpart', { key: 'runs' },
      h('div.row', { style: 'gap:10px;flex-wrap:wrap' }, h('b', m.short_name || m.name), m.publisher ? h('span.mut', m.publisher) : null,
        stateBadge(m, st), m.kind === 'llama' && !m.exists ? badge('File missing', 'fail') : null),
      h('div', h('span.mut', 'Runs on: '), runsOnCell(m, st), f.need ? [h('span.mut', ' · needs '), h('span', { title: f.title }, gb(f.need))] : null),
      facts([
        m.family ? ['Family', h('span', m.family)] : null,
        m.parameters ? ['Parameters', h('span', m.parameters)] : null,
        m.quant ? ['Quantization', h('span', m.quant)] : null,
        m.context_length ? ['Trained context', h('span', num(m.context_length)), 'The context the model was trained with, from its file'] : null,
        m.requests_served != null ? ['Requests served', h('span', num(m.requests_served))] : null,
        l.count ? ['Loads', h('span', num(l.count), l.failed ? h('span.err-t', ` · ${num(l.failed)} failed`) : null)] : null,
      ].filter(Boolean))),
    h('section.mdl-cfg', { key: 'cfg' },
      h('div.dh', h('h3', 'Configurations ', h('span.mut', profilesText(m))),
        h('div.mdl-cfg-tools',
          hand.length > 1 ? searchField(ps.q, v => { ps.q = v; ps.redraw(); }, 'Search profiles', 'mdl-psearch') : null,
          ctxs.length > 1 ? filterSelect('Context', [{ id: '', label: 'any' }].concat(ctxs.map(c => ({ id: String(c), label: num(c) }))), ps.ctx, v => { ps.ctx = v; ps.redraw(); }) : null,
          h('a.btn.sm', { href: href('profiles', ['new']), title: 'Open the profile editor' }, 'Add profile'))),
      hand.length ? (rows.length ? h('table.tbl.mdl-ptbl',
        h('thead', h('tr', h('th', { scope: 'col' }, 'Profile'), h('th.r', { scope: 'col' }, 'Context'),
          h('th.r', { scope: 'col', title: 'What a load of this profile needs: measured, or estimated from the file' }, 'Memory needed'),
          h('th', { scope: 'col' }, h('span.sr', 'Edit')))),
        h('tbody', rows.map(p => h('tr', { key: p.id, 'data-profile': p.id },
          h('td', h('span.mono', p.id), loadedProfiles.has(p.id) ? h('span.mdl-pill', { title: 'Loaded now' }, 'Now') : null, p.description ? h('span.sub', p.description) : null,
            p.parallel > 1 ? h('span.sub', `${num(p.parallel)} requests in parallel`) : null, hostLine(p)),
          h('td.r.mono', p.ctx ? num(p.ctx) : DASH),
          h('td.r', p.needs_mib ? [h('span.mono', num(p.needs_mib) + ' MiB'), h('span.sub', { title: p.needs_basis || null }, p.needs_measured ? 'measured' : 'estimated')] : h('span.mut', DASH)),
          h('td.r', h('a.go', { href: href('profiles', [p.id]), 'aria-label': 'Edit ' + p.id }, h('span.tx', 'Edit'), icon('M8 5l5 5-5 5')))))))
        : h('p.mut.mdl-pad', 'No profile matches.'))
        : h('p.mut.mdl-pad', all.length ? 'Every configuration of this model was generated.' : 'No configuration uses this file yet. Add a profile to load it.'),
      gen ? h('p.mut.mdl-pad', `${plural(gen, 'generated configuration')} (benchmark matrices), listed on the model's page and under Profiles.`) : null),
    h('div.mdl-pad.row', { key: 'page', style: 'gap:12px' }, h('a.btn', { href: href('models', [m.key]) }, 'Open the model page'), h('a', { href: href('profiles') }, 'Edit under Profiles')),
  ];
}

function listPanel(st, models, disk, error, r, query, setQuery, onFind) {
  const filter = FILTERS.some(f => f[0] === r.query.show) ? r.query.show : 'all';
  const sort = SORT[r.query.sort] ? r.query.sort : 'used';
  const counts = {};
  for (const [id, , test] of FILTERS) counts[id] = (models || []).filter(m => test(m, st)).length;
  const q = (query || '').trim().toLowerCase();
  const test = (FILTERS.find(f => f[0] === filter) || FILTERS[0])[2];
  const rows = (models || []).filter(m => test(m, st) && (!q || [m.name, m.raw_name, m.file, m.short_name, m.family, m.publisher, m.quant, m.parameters]
    .concat((m.profiles || []).map(p => p.id)).some(x => String(x || '').toLowerCase().includes(q)))).sort(SORT[sort]);
  const openKey = r.query.model || '';
  const open = key => setQuery({ model: key });
  let body;
  if (error && !models) body = empty('The model list could not be read', error);
  else if (!models) body = skeleton(6);
  else if (!models.length) body = empty('No models yet', 'A model appears here once a .gguf file is in the models folder, or a profile names a model file.');
  else if (!rows.length) body = empty('No model matches', q ? `Nothing matches "${query}". Search looks at the file name, the name and the profile names.` : 'No model is in this group right now.');
  else body = h('div.mdl-rows', { role: 'list', 'aria-label': 'All models' }, rows.map(m => h('div', {
    key: m.key, role: 'listitem', class: 'mr' + (openKey === m.key ? ' sel' : ''), 'aria-current': openKey === m.key ? 'true' : null, 'data-model': m.key, tabindex: '0',
    onclick: e => { if (!e.target.closest('[data-off]')) open(m.key); },
    onkeydown: e => { if ((e.key === 'Enter' || e.key === ' ') && !e.target.closest('[data-off]')) { e.preventDefault(); open(m.key); } } },
  off('button.star', NOT_YET.pin, { 'aria-label': 'Pin ' + (m.short_name || m.name) + ' (not available)', 'aria-pressed': 'false' }, icon('M10 3l2.1 4.3 4.7.7-3.4 3.3.8 4.7L10 13.8 5.8 16l.8-4.7L3.2 8l4.7-.7z')),
  h('span.mdl-name', h('span.mono.nm', { title: m.short_name && m.short_name !== m.file ? m.short_name : null }, m.file || m.name),
    h('span.mut', sizeText(m), m.kind !== 'command' && m.exists === false ? h('span.err-t', ' · the file is missing') : null)),
  h('span', { class: m.profile_count ? 'mdl-pc' : 'mdl-pc mut' }, profilesText(m)),
  h('span.mut.mdl-used', lastUsed(m)))));
  const sec = (label, right, attrs) => h('div.sech', attrs || null, h('span.lbl', label), right || null);
  return h('section.panel.mdl', { key: 'catalog', 'aria-label': 'Models' },
    h('div.mdl-bar',
      searchField(query, onFind, 'Search models', 'model-search', { size: 'lg' }),
      models && models.length ? filterSelect('Show', FILTERS.map(([id, label]) => ({ id, label: label + (id === 'all' ? '' : ` (${counts[id]})`) })),
        filter, id => setQuery({ show: id === 'all' ? '' : id })) : null,
      filterSelect('Sort', SORTS.map(([id, label]) => ({ id, label })), sort, id => setQuery({ sort: id === 'used' ? '' : id })),
      off('button.btn.pri', NOT_YET.add, { 'data-act': 'add' }, icon('M10 4v12M4 10h12'), 'Add a model')),
    sec('Downloading', h('span.lbl.mdl-off', 'not built yet'), { 'data-sec': 'downloading', title: NOT_YET.downloads }),
    sec('Pinned', h('span.lbl.mdl-off', 'not built yet'), { 'data-sec': 'pinned', title: NOT_YET.pin }),
    sec('All models', models ? diskLine(models, disk) : null, { 'data-sec': 'all' }),
    body,
    off('button.tbar', NOT_YET.trash, { 'data-act': 'trash' }, icon('M4 6h12M8 6V4h4v2M6 6l1 10h6l1-10'), 'Deleted models', h('span.mut', ' · not built yet')));
}

function mountHome(el, route) {
  let r = route, query = r.query.q || '', side = null, disk = null, alive = true;
  const open = new Set();
  const toggle = k => { if (open.has(k)) open.delete(k); else open.add(k); view.refresh(); };
  const offCatalog = onCatalog(() => { view.refresh(); if (side) side.redraw(); });
  const setQuery = q => navigate('models', [], Object.assign({}, r.query, q), true);

  // the disk block: only the REST answer carries it; its figures move every 30 s
  const readDisk = () => api.get('/api/models').then(d => { disk = (d && d.disk) || null; if (alive) view.refresh(); }, () => {});
  readDisk();
  const diskTimer = setInterval(readDisk, 30000);

  // The side panel of the open model (?model=<key>).
  const syncSide = () => {
    const key = r.query.model || '';
    if (!key) { if (side) { const s = side; side = null; s.close(); } return; }
    if (side && side.key === key) { side.redraw(); return; }
    if (side) { const s = side; side = null; s.close(); }
    const find = () => (catalog.models || []).find(m => m.key === key) || null;
    const ps = { q: '', ctx: '', redraw: () => p.redraw() };
    const body = () => {
      const m = find();
      if (m) return modelPanel(state.status || {}, m, ps);
      return catalog.models ? h('section.card', empty('This model is not in the catalog', 'Its file may have gone. Close this panel to see the models there are.')) : skeleton(3);
    };
    const p = sidePanel({
      label: 'Selected model', name: 'Selected model: ' + key,
      title: () => { const m = find(); return h('span.mono', m ? m.file || m.name : key); },
      sub: () => {
        const m = find();
        if (!m) return null;
        return [sizeText(m), ' · ', m.state === 'ready' ? 'loaded now' : m.state === 'loading' ? 'loading now' : m.last_active ? ['last used ', ago(m.last_active)] : 'never used'];
      },
      body, onClose: () => { if (side === p) { side = null; setQuery({ model: '' }); } },
    });
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
        : listPanel(view_, models, disk, catalog.error, r, query, setQuery, v => { query = v; view.refresh(); }),
    ];
  };
  const view = liveView(el, render, ['status', 'tick', 'conn', 'hello']);
  needCatalog();
  syncSide();
  return {
    update(nr) { r = nr; syncSide(); view.refresh(); },
    unmount() { alive = false; clearInterval(diskTimer); offCatalog(); if (side) { const s = side; side = null; s.close(); } view.unmount(); },
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
