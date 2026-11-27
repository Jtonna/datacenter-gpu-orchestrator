// Test profiles (profiles plan, section 9; the owner's note: "test profiles
// get their own page … all visible but moved out of the way visually …
// named TEST - <name> … when promoted it loses the TEST - part
// automatically"). Under Profiles in the gear menu.
//
// One row per test profile: its shown name (core/ui.js profileLabel: "TEST -
// " + display name or id) with the id under it, the model, the context it
// REALLY runs at (a profile written above the model's trained context runs at
// the trained context: the 16k bench-tinyllama profiles run at 2k, and say
// so), why it is test (marked, generated, forced zones), who refers to it
// (benchmarks and benchmark groups), its last use in the history kept, and
// Promote (a plain button; it asks twice). Filters by model and by reason,
// a search, a pager (299 rows). A row opens the profile's side panel, the
// same one Settings > Profiles opens (views/profiles.js openProfilePanel:
// its config, Promote, Edit, Duplicate, Compare with…, Delete; build-92 W2).
//
// Reads: GET /api/profiles/test; the catalog (model names); GET /api/profiles
// when a panel opens (the panel's rows). Writes: POST
// /api/config/profiles/<id>/promote (views/profiles.js changeStage).
// Route: #/test-profiles?model=<file>&reason=<why>&q=&page=&profile=<id>

import { h } from '../core/dom.js';
import { registerView, href, navigate } from '../core/router.js';
import * as api from '../core/api.js';
import { liveView, pageHead, badge, empty, banner, skeleton, button, searchField, filterSelect, profileLabel, toast } from '../core/ui.js';
import { openPanelNow } from '../core/panel.js';
import { pager } from '../core/pager.js';
import { num, plural, ago } from '../core/format.js';
import { watchConfig, noticeBanner } from '../core/changes.js';
import { catalog, onCatalog, needCatalog } from '../core/catalog.js';
import { changeStage, openProfilePanel } from './profiles.js';

const PER = 25;
const REASONS = { marked: 'Marked test', generated: 'Generated', 'forced zones': 'Forced zones' };
const REASON_HELP = {
  marked: 'Its stage is written as test',
  generated: 'Made by a tool: its generated flag, else an id that starts bench-, b- or m-',
  'forced zones': 'Pinned to chosen zones (force_nodes)',
};
const ctxText = c => (!c ? '–' : c % 1024 === 0 ? c / 1024 + 'k' : num(c));
const modelName = (file, row) => { const m = file && catalog.byKey[file]; return (m && (m.short_name || m.name)) || (row && row.model_name) || file || '–'; };

registerView({
  id: 'test-profiles', title: 'Test profiles',
  mount(el, route0) {
    let route = route0, data = null, error = '', alive = true;
    let full = null, fullReading = false, panel = null, panelId = '';
    const qv = k => route.query[k] || '';
    const page = () => Math.max(1, Number(route.query.page) || 1);
    const go = query => navigate('test-profiles', [], Object.assign({ model: qv('model') || null, reason: qv('reason') || null, q: qv('q') || null, page: null }, query), true);

    async function load() {
      try { data = await api.get('/api/profiles/test'); error = ''; } catch (e) { error = e.message; }
      if (alive) view.refresh();
    }
    // the panel's rows: GET /api/profiles (every profile, as Settings > Profiles reads them)
    async function loadFull() {
      if (fullReading) return;
      fullReading = true;
      try { full = (await api.get('/api/profiles')).profiles || []; } catch (e) { full = full || []; toast('The profiles could not be read: ' + e.message, 'err'); }
      fullReading = false;
      if (alive) syncPanel();
    }
    const reloadAll = () => { load(); loadFull(); };
    const openRow = id => navigate('test-profiles', [], { model: qv('model') || null, reason: qv('reason') || null, q: qv('q') || null, page: page() > 1 ? String(page()) : null, profile: id }, true);
    function syncPanel() {
      const id = qv('profile');
      if (!id) { if (panel) { const p = panel; panel = null; panelId = ''; p.close(); } return; }
      if (panel && panelId === id && openPanelNow() === panel) { panel.redraw(); return; }
      if (!full) { loadFull(); return; }
      panelId = id;
      panel = openProfilePanel({ id, label: 'Test profile', cur: () => (full || []).find(x => x.id === id), all: () => full || [], reload: reloadAll,
        onDeleted: () => { toast('Deleted ' + id + '.', 'ok'); openRow(null); reloadAll(); },
        onClose: () => { if (panelId === id) { panel = null; panelId = ''; if (qv('profile') === id) openRow(null); } } });
    }
    const watch = watchConfig({ revision: () => 0, pending: () => false, refresh: load, redraw: () => view.refresh() });
    const offCatalog = onCatalog(() => view.refresh());
    needCatalog();

    function refsCell(r) {
      const b = (r.refs && r.refs.benchmarks) || [], g = (r.refs && r.refs.groups) || [];
      if (!b.length && !g.length) return h('span.mut', 'nothing');
      return h('span', { 'data-refs': String(b.length + g.length) },
        b.length ? h('span', { title: b.join(', ') }, plural(b.length, 'benchmark')) : null,
        b.length && g.length ? ', ' : null,
        g.length ? h('span', { title: g.join(', ') }, plural(g.length, 'group')) : null);
    }
    function ctxCell(r) {
      const real = r.ctx_real || r.ctx;
      return h('td.r', { 'data-ctx-real': String(real) },
        h('span.mono', ctxText(real)),
        real !== r.ctx ? h('span.sub', { title: r.ctx_note || '' }, 'written ' + ctxText(r.ctx) + ', runs at ' + ctxText(real)) : null);
    }

    function listPanel() {
      const all = data.profiles || [];
      const s = qv('q').trim().toLowerCase();
      const rows = all.filter(r => (!qv('model') || r.model === qv('model')) && (!qv('reason') || r.reason === qv('reason'))
        && (!s || r.id.toLowerCase().includes(s) || profileLabel(r).toLowerCase().includes(s) || String(r.model || '').toLowerCase().includes(s)));
      const pages = Math.max(1, Math.ceil(rows.length / PER));
      const pg = Math.min(page(), pages);
      const shown = rows.slice((pg - 1) * PER, pg * PER);
      const models = Object.keys(data.models || {}).sort((a, b) => modelName(a).localeCompare(modelName(b)));
      return h('section.panel.prf', { key: 'list' },
        h('div.prf-bar',
          searchField(qv('q'), v => go({ q: v || null }), 'Search name, id or model', 'tp-search', { size: 'lg' }),
          filterSelect('Model', [{ id: '', label: 'any' }].concat(models.map(m => ({ id: m, label: modelName(m) + ' (' + num(data.models[m]) + ')' }))), qv('model'), v => go({ model: v || null })),
          filterSelect('Why', [{ id: '', label: 'any' }].concat(Object.keys(REASONS).filter(k => (data.reasons || {})[k]).map(k => ({ id: k, label: REASONS[k] + ' (' + num(data.reasons[k]) + ')' }))), qv('reason'), v => go({ reason: v || null })),
          h('span.prf-count.mut', plural(rows.length, 'test profile'))),
        !rows.length ? empty(all.length ? 'No test profile matches' : 'No test profiles', all.length ? 'Change the filters or the search.' : 'Every profile is a production profile.')
          : h('div.prf-scroll', h('table.tbl.prf-tbl.tp-tbl',
            h('thead', h('tr', h('th', { scope: 'col' }, 'Test profile'), h('th', { scope: 'col' }, 'Model'),
              h('th.r', { scope: 'col', title: 'The context it really runs at: the trained context when the written one is above it' }, 'Context'),
              h('th', { scope: 'col' }, 'Why test'), h('th', { scope: 'col', title: 'Benchmarks and benchmark groups that name it' }, 'Referred to by'),
              h('th', { scope: 'col', title: 'Its last load, unload or benchmark step in the history kept' }, 'Last used'), h('th', { scope: 'col' }, h('span.sr', 'Promote')))),
            h('tbody', shown.map(r => h('tr', { key: 'tp-' + r.id, 'data-profile': r.id, 'data-reason': r.reason, class: 'click' + (qv('profile') === r.id ? ' sel' : ''), tabindex: '0',
              'aria-selected': qv('profile') === r.id ? 'true' : null, onclick: () => openRow(r.id),
              onkeydown: ev => { if (ev.target === ev.currentTarget && (ev.key === 'Enter' || ev.key === ' ')) { ev.preventDefault(); openRow(r.id); } } },
              h('td.prf-name.tp-name', h('div', { 'data-shown': '', title: 'The id clients send: ' + r.id }, profileLabel(r)), r.display_name ? h('div.mono.mut.sub', { title: 'The id clients send' }, r.id) : null),
              h('td.prf-model', h('span', { title: r.model }, modelName(r.model, r))),
              ctxCell(r),
              h('td', badge(REASONS[r.reason] || r.reason, 'n', REASON_HELP[r.reason]), (r.reasons || []).length > 1 ? h('span.sub', 'also ' + r.reasons.slice(1).join(', ')) : null),
              h('td', refsCell(r)),
              h('td', r.last_used ? ago(r.last_used) : h('span.mut', { title: data.history ? 'No load in the history kept' : 'The history is off' }, 'not in the history')),
              // a small plain button at the body weight, not a primary one (build-92 W2); it does not open the row's panel
              h('td.r', button('Promote', async ev => { if (ev && ev.stopPropagation) ev.stopPropagation(); if (await changeStage(r, 'production')) reloadAll(); }, 'sm plain', { 'data-act': 'promote', 'aria-label': 'Promote ' + r.id }))))))),
        rows.length > PER ? h('div.prf-foot', pager({ page: pg, pages, total: rows.length, per: PER, range: true, label: 'Test profile pages', onPage: n => go({ page: n > 1 ? String(n) : null }) })) : null);
    }

    const render = () => {
      const head = pageHead('Test profiles', data ? [h('b', num(data.count)), ' test · ', h('a', { href: href('profiles') }, h('b', num(data.production_count)), ' production'),
        ' · kept out of the way: they are shown as TEST - <name> everywhere, and lose that when promoted'] : null);
      if (error && !data) return [head, banner('err', ['The test profiles could not be read: ', error])];
      return [head, noticeBanner(watch.notice, { onReload: () => { watch.clear(); load(); }, onDismiss: () => { watch.clear(); view.refresh(); } }),
        !data ? h('section.panel', { key: 'wait' }, skeleton(8)) : listPanel()];
    };
    const view = liveView(el, render, ['conn']);
    load();
    syncPanel();
    return {
      update(r) { route = r; syncPanel(); view.refresh(); },
      unmount() { alive = false; watch.off(); offCatalog(); if (panel) { const p = panel; panel = null; p.close(); } view.unmount(); },
    };
  },
});
