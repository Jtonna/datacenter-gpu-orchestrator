// Audit (Configure): who changed what, when and from where. Two tabs: the
// trail (every audited action, newest first) and the configuration revisions
// (view one in full, roll back to it).
//
// Reads: REST GET /api/audit?since&source&actor&section&target&action&q&limit,
// GET /api/audit/{id} (a live entry), GET /api/config/revisions,
// GET /api/config/revisions/{n}; WebSocket "config" (prepend the new entries,
// refresh the revision list). Writes: POST /api/config/revisions/{n}/rollback
// (dry run first, then If-Match and a comment).
// Routes: #/audit[?section=&target=&source=&actor=&action=&q=&range=],
// #/audit/revisions.

import { h, frag } from '../core/dom.js';
import { registerView, navigate, href } from '../core/router.js';
import { listen } from '../core/store.js';
import * as api from '../core/api.js';
import { liveView, pageHead, badge, table, empty, banner, skeleton, chips, tabs, button, diffView, dialog, toast } from '../core/ui.js';
import { num, plural, ago, at, dayLabel, now, DASH } from '../core/format.js';
import { sourceBadge, SOURCES, SECTION_NAMES, sectionName, actionName, entrySentence, searchBox } from '../core/configdata.js';
import { reviewChanges } from '../core/changes.js';

const RANGES = [['1h', 'Last hour', 3600e3], ['24h', 'Last 24 hours', 86400e3], ['7d', 'Last 7 days', 7 * 86400e3], ['30d', 'Last 30 days', 30 * 86400e3], ['all', 'All time', 0]];
const SECTION_ORDER = ['profiles', 'tiers', 'settings', 'exclude_nodes', 'node_overrides', 'recovery', 'fleet', 'bench', 'runtime'];
const ACTIONS = ['create', 'update', 'delete', 'import', 'rollback', 'load', 'unload'];
const PAGE = 100;

const json = v => JSON.stringify(v, null, 2);

// ---- one entry ----

function details(e) {
  const hasBefore = e.before != null, hasAfter = e.after != null;
  return h('div.col', { style: 'gap:12px;margin-top:10px' },
    (e.diff || []).length ? diffView(e.diff) : null,
    hasBefore || hasAfter ? h('div.grid', { style: 'grid-template-columns:repeat(auto-fit,minmax(min(260px,100%),1fr));gap:12px' },
      hasBefore ? h('div.col', { style: 'gap:4px;min-width:0' }, h('span.mut', { style: 'font-weight:700;font-size:var(--fs-sm)' }, 'Before'), h('pre.mono', { style: 'margin:0;padding:10px;border:1px solid var(--line);border-radius:10px;background:var(--inset);overflow:auto;max-height:260px;font-size:var(--fs-sm)' }, json(e.before))) : null,
      hasAfter ? h('div.col', { style: 'gap:4px;min-width:0' }, h('span.mut', { style: 'font-weight:700;font-size:var(--fs-sm)' }, 'After'), h('pre.mono', { style: 'margin:0;padding:10px;border:1px solid var(--line);border-radius:10px;background:var(--inset);overflow:auto;max-height:260px;font-size:var(--fs-sm)' }, json(e.after))) : null) : null,
    h('dl.kv', { style: 'font-size:var(--fs-md)' },
      h('dt', 'Entry'), h('dd.mono', '#' + e.id),
      h('dt', 'Time'), h('dd', at(e.ts)),
      h('dt', 'From'), h('dd.mono', e.remote_addr || DASH),
      h('dt', 'Client'), h('dd.mono.wrap-any', e.user_agent || DASH)));
}

// ---- the revision viewer ----

async function viewRevision(n, current, onRollback) {
  let closeDlg = () => {};
  const body = h('div.col', { style: 'gap:14px;min-width:0' }, h('p.mut', 'Loading revision ' + n + '…'));
  const foot = h('div.dlg-foot', h('button', { class: 'btn lg', type: 'button', onclick: () => closeDlg(undefined) }, 'Close'));
  const dlgP = dialog({ title: 'Revision ' + n, wide: true, body: close => { closeDlg = close; return h('div.col', { style: 'gap:14px;min-width:0' }, body, foot); } });
  try {
    const [rv, dry] = await Promise.all([api.get('/api/config/revisions/' + n),
      n === current ? Promise.resolve(null) : api.send('POST', `/api/config/revisions/${n}/rollback`, {}, { dryRun: true })]);
    const cfg = rv.config || {};
    const profiles = Object.keys(cfg.profiles || {});
    body.replaceChildren(frag(
      h('div.row', { style: 'gap:8px 12px' }, sourceBadge(rv.source), h('span', rv.actor || 'anonymous'), ago(rv.ts), n === current ? badge('Current', 'ready') : null,
        rv.comment ? h('span.mut', '“' + rv.comment + '”') : null),
      n === current ? h('p.mut', 'This is the configuration in force now.') : [
        h('b', `Compared with the current configuration (revision ${current})`),
        dry && dry.changed ? [h('p.mut', 'Rolling back to this revision would change:'), diffView(dry.diff)] : h('p.mut', 'It is the same as the current configuration.')],
      h('b', 'The configuration at this revision'),
      h('div.row', { style: 'gap:6px 18px;font-size:var(--fs-md)' },
        h('span', plural(profiles.length, 'profile')), h('span', plural((cfg.tiers || []).length, 'tier')),
        h('span', plural((cfg.settings_set || []).length, 'setting'), ' changed from default'),
        h('span', plural((cfg.exclude_nodes || []).length, 'excluded node')), h('span', plural(Object.keys(cfg.node_overrides || {}).length, 'node override'))),
      h('pre.mono', { style: 'margin:0;padding:12px;border:1px solid var(--line);border-radius:10px;background:var(--inset);overflow:auto;max-height:340px;font-size:var(--fs-sm)', tabindex: '0', 'aria-label': 'The configuration document at revision ' + n }, json(rv.document || cfg))));
    foot.replaceChildren(frag(
      n === current ? null : h('button', { class: 'btn lg dng', type: 'button', onclick: () => { closeDlg(undefined); setTimeout(() => onRollback(n), 0); } }, 'Roll back to this revision…'),
      h('button', { class: 'btn lg', type: 'button', onclick: () => closeDlg(undefined) }, 'Close')));
  } catch (e) { body.replaceChildren(banner('err', 'Revision ' + n + ' could not be read: ' + e.message)); }
  return dlgP;
}

registerView({
  id: 'audit', title: 'Audit',
  mount(el, route0) {
    let route = route0, alive = true;
    // trail
    let entries = null, total = 0, error = '', limit = PAGE, knownActors = new Set(), open = new Set(), fresh = new Map(), reqId = 0, qTimer = 0, qText = null;
    // revisions
    let revs = null, current = 0, revError = '';

    const tab = () => (route.rest[0] === 'revisions' ? 'revisions' : 'trail');
    const filters = () => {
      const q = route.query;
      return { range: q.range || '7d', source: q.source || '', actor: q.actor || '', section: q.section || '', action: q.action || '', target: q.target || '', q: q.q || '' };
    };
    const setFilter = patch => {
      const f = Object.assign(filters(), patch);
      const query = {};
      for (const k in f) if (f[k] && !(k === 'range' && f[k] === '7d')) query[k] = f[k];
      limit = PAGE;
      navigate('audit', null, query, true);
    };
    const hasFilters = () => { const f = filters(); return !!(f.source || f.actor || f.section || f.action || f.target || f.q); };

    function params() {
      const f = filters();
      const r = RANGES.find(x => x[0] === f.range) || RANGES[2];
      return { since: r[2] ? Math.round(now() - r[2]) : '', source: f.source, actor: f.actor, section: f.section, action: f.action, target: f.target, q: f.q, limit };
    }
    const matches = e => {
      const f = filters();
      const r = RANGES.find(x => x[0] === f.range) || RANGES[2];
      return (!r[2] || e.ts >= now() - r[2]) && (!f.source || e.source === f.source) && (!f.actor || e.actor === f.actor) && (!f.section || e.section === f.section)
        && (!f.action || e.action === f.action) && (!f.target || e.target === f.target)
        && (!f.q || JSON.stringify(e).toLowerCase().includes(f.q.toLowerCase()));
    };

    async function loadTrail() {
      const id = ++reqId;
      try {
        const r = await api.get('/api/audit', params());
        if (id !== reqId || !alive) return;
        entries = r.entries || []; total = r.total_matching ?? entries.length; error = '';
        for (const e of entries) if (e.actor) knownActors.add(e.actor);
      } catch (e) { if (id !== reqId) return; error = e.message; entries = entries || []; }
      if (alive) view.refresh();
    }
    async function loadRevs() {
      try {
        const r = await api.get('/api/config/revisions', { limit: 200 });
        revs = r.revisions || []; current = r.current; revError = '';
      } catch (e) { revError = e.message; revs = revs || []; }
      if (alive) view.refresh();
    }

    // Audited actions the "config" topic does not announce (a load or unload
    // asked over REST or MCP, a benchmark or suite edit): the trail is read
    // again when the live channel hints at one (a load started, a model was
    // unloaded through the API, a benchmark run changed state), after a
    // reconnect, and when the page becomes visible again. Never on a timer.
    let softTimer = 0;
    const soft = () => { clearTimeout(softTimer); softTimer = setTimeout(() => { if (alive && tab() === 'trail') loadTrail(); }, 1200); };
    const offEvent = listen('event', e => { if (e && (e.type === 'loadStarted' || (e.type === 'instanceUnloaded' && e.data && e.data.reason === 'api'))) soft(); });
    const offBench = listen('bench', d => { if (d && (d.kind === 'run' || d.kind === 'aggregate_updated')) soft(); });
    const offHello = listen('hello', soft);
    const onVisible = () => { if (!document.hidden) soft(); };
    document.addEventListener('visibilitychange', onVisible);

    // a change announced on the "config" topic: put the new entries at the top
    const off = listen('config', async ev => {
      if (ev && ev.audit_entry_ids && ev.audit_entry_ids.length && entries && tab() === 'trail') {
        for (const id of ev.audit_entry_ids) {
          if (entries.some(e => e.id === id)) continue;
          try {
            const e = await api.get('/api/audit/' + id);
            if (!alive) return;
            if (entries.some(x => x.id === e.id)) continue;
            if (matches(e)) { entries.unshift(e); total++; fresh.set(e.id, Date.now()); if (e.actor) knownActors.add(e.actor); setTimeout(() => { fresh.delete(e.id); view.refresh(); }, 9000); }
          } catch { /* the entry will show on the next load */ }
        }
        view.refresh();
      }
      if (revs) loadRevs();
    });

    // ---- rollback ----
    async function rollback(n) {
      let cur = current;
      const r = await reviewChanges({
        title: `Roll back to revision ${n}?`, saveLabel: `Roll back to revision ${n}`,
        intro: `Rolling back does not delete anything. It writes a new revision with the configuration of revision ${n}, on top of revision ${cur}. The revisions in between stay in the history, so you can undo this by rolling back again.`,
        getRevision: () => cur,
        buildOps: () => [{ label: `Roll back to revision ${n}`, method: 'POST', path: `/api/config/revisions/${n}/rollback`, body: {} }],
        reload: async c => { await loadRevs(); cur = current; },
      });
      if (r && r.saved) toast(`Rolled back to revision ${n}. Saved as revision ${r.revision}.`, 'ok');
      await loadRevs();
      loadTrail();
    }
    const showRevision = async n => {
      if (revs == null) await loadRevs();
      viewRevision(n, current, rollback);
    };

    // ---- drawing: trail ----
    function filterBar() {
      const f = filters();
      const sel = (label, key, options) => h('label', { style: 'display:flex;flex-direction:column;gap:4px;font-size:var(--fs-sm);font-weight:700;color:var(--mut);min-width:0' }, label,
        h('select.select', { style: 'min-width:150px', 'aria-label': label, onchange: e => setFilter({ [key]: e.target.value }) },
          h('option', { value: '', selected: !f[key] }, 'Any'), options.map(([v, t]) => h('option', { value: v, selected: f[key] === v }, t))));
      const actors = [...knownActors];
      if (f.actor && !actors.includes(f.actor)) actors.push(f.actor);
      const secs = SECTION_ORDER.slice();
      if (f.section && !secs.includes(f.section)) secs.push(f.section);
      return h('div.cs', { style: 'display:flex;flex-direction:column;gap:12px;border-top:0;padding-top:0' },
        h('div.row', { style: 'gap:10px 16px' },
          chips(RANGES.map(r => ({ id: r[0], label: r[1] })), f.range, id => setFilter({ range: id }), 'Time range'),
          searchBox(qText != null ? qText : f.q, v => {
            qText = v; clearTimeout(qTimer);
            qTimer = setTimeout(() => { qText = null; setFilter({ q: v.trim() }); }, 350);
          }, 'Search the trail', 'Search the audit trail')),
        h('div.row', { style: 'gap:10px 16px;align-items:flex-end' },
          sel('Source', 'source', Object.entries(SOURCES).map(([k, v]) => [k, v[0]])),
          sel('Actor', 'actor', actors.sort().map(a => [a, a])),
          sel('Section', 'section', secs.map(s => [s, sectionName(s)])),
          sel('Action', 'action', ACTIONS.map(a => [a, actionName(a)])),
          f.target ? h('span.row', { style: 'gap:6px' }, h('span.mut', 'Target'), h('span.node', f.target), button('Clear', () => setFilter({ target: '' }), 'sm ghost')) : null,
          hasFilters() ? button('Clear filters', () => { limit = PAGE; navigate('audit', null, { range: filters().range === '7d' ? '' : filters().range }, true); }, 'sm') : null));
    }

    function entryRow(e) {
      const isOpen = open.has(e.id);
      return h('div', { key: 'e' + e.id, class: 'ev', style: fresh.has(e.id) ? 'box-shadow:inset 3px 0 0 var(--mint);background:var(--mint-bg)' : null },
        h('span.mut', { style: 'font-weight:600' }, ago(e.ts)),
        h('span', sourceBadge(e.source)),
        h('div', { style: 'min-width:0' },
          h('p', h('b', entrySentence(e)), ' ', e.section && !['profiles', 'tiers', 'settings'].includes(e.section) ? badge(sectionName(e.section), 'n') : null),
          h('p.d', 'by ', h('b.txt2', e.actor || 'anonymous'), ' · ', sectionName(e.section), e.target && e.section !== 'tiers' ? [' · ', h('span.mono', e.target)] : null,
            e.revision ? [' · ', h('a', { href: href('audit', ['revisions'], { rev: e.revision }), onclick: ev => { ev.preventDefault(); showRevision(e.revision); } }, 'Revision ' + e.revision)] : null,
            ' · ', h('a', { href: '#', 'aria-expanded': isOpen ? 'true' : 'false', onclick: ev => { ev.preventDefault(); if (isOpen) open.delete(e.id); else open.add(e.id); view.refresh(); } }, isOpen ? 'Hide details' : 'Show details')),
          e.comment ? h('p.d', { style: 'color:var(--text-2)' }, '“' + e.comment + '”') : null,

          isOpen ? details(e) : null));
    }

    function trail() {
      const sub = entries ? `Showing ${num(entries.length)} of ${num(total)}` : '';
      let body;
      if (error && !(entries || []).length) {
        body = /database|available|503/i.test(error) ? empty('The audit log is not available', 'The scheduler is running without its database, so nothing is recorded and nothing can be shown. Check the data directory the scheduler was started with.')
          : empty('The audit trail could not be read', error);
      } else if (!entries) body = skeleton(6);
      else if (!entries.length) {
        body = hasFilters() || filters().range !== 'all' ? empty('No entry matches', hasFilters() ? 'Change the filters or widen the time range.' : 'Nothing was changed in this time range. Choose All time to look further back. Every change to profiles, tiers, settings and benchmarks, and every load or unload asked for over REST or MCP, is recorded here.')
          : empty('Nothing has been changed yet', 'Every change to profiles, tiers, settings and benchmarks, and every load or unload asked for over REST or MCP, is recorded here with who made it and from where.');
      } else {
        const rows = [];
        let day = '';
        for (const e of entries) {
          const d = dayLabel(e.ts);
          if (d !== day) { day = d; rows.push(h('div.day', { key: 'd-' + d }, d)); }
          rows.push(entryRow(e));
        }
        body = h('div', rows,
          entries.length < total ? h('div.cs', h('div.row', { style: 'gap:12px' }, button('Show 100 more', () => { limit += PAGE; loadTrail(); }, 'sm'), h('span.mut', sub))) : null);
      }
      return h('section.card', { key: 'trail' }, h('div.ch', h('h2', 'Trail'), h('span.note', sub || 'Newest first. Times are shown as "5 min ago"; hover for the exact time.')), filterBar(), body);
    }

    // ---- drawing: revisions ----
    function revisions() {
      let body;
      if (revError && !(revs || []).length) body = empty('The revisions could not be read', revError);
      else if (!revs) body = skeleton(5);
      else if (!revs.length) body = empty('No revision yet', 'A revision is written every time the configuration changes, and the first one when a configuration is imported.');
      else body = table({
        minWidth: 820, rows: revs, key: r => r.revision, onRow: r => showRevision(r.revision),
        columns: [
          { label: 'Revision', cls: 'tight', render: r => h('span', h('span.mono', { style: 'font-weight:500' }, String(r.revision)), r.revision === current ? [' ', badge('Current', 'ready')] : null) },
          { label: 'When', cls: 'tight', render: r => ago(r.ts) },
          { label: 'Source', cls: 'tight', render: r => sourceBadge(r.source) },
          { label: 'Who', cls: 'tight', render: r => r.actor || 'anonymous' },
          { label: 'Comment', render: r => (r.comment ? h('span', r.comment) : h('span.mut', DASH)) },
          { label: '', cls: 'tight', render: r => h('span.row', { style: 'gap:6px;justify-content:flex-end;flex-wrap:nowrap' }, button('View', e => { e.stopPropagation(); showRevision(r.revision); }, 'sm'),
            r.revision === current ? null : button('Roll back', e => { e.stopPropagation(); rollback(r.revision); }, 'sm dng')) },
        ],
      });
      return h('section.card', { key: 'revs' }, h('div.ch', h('h2', 'Configuration revisions'),
        h('span.note', revs ? `${plural(revs.length, 'revision')} · the current one is ${current}` : 'Every saved change is a whole revision you can read back and return to.')), body);
    }

    const render = () => {
      const t = tab();
      return [
        pageHead('Audit', entries && t === 'trail' ? [h('b', num(total)), total === 1 ? ' entry' : ' entries', hasFilters() ? ' match' : ' in this range'] : 'Who changed what, when and from where'),
        tabs([{ id: 'trail', label: 'Trail' }, { id: 'revisions', label: 'Revisions', count: revs ? revs.length : null }], t,
          id => navigate('audit', id === 'revisions' ? ['revisions'] : null, id === 'trail' ? route.query : null, true)),
        error && entries && entries.length ? banner('err', error) : null,
        t === 'trail' ? trail() : revisions(),
      ];
    };
    const view = liveView(el, render, ['conn', 'tick']);
    loadTrail();
    if (tab() === 'revisions') loadRevs();
    return {
      unmount() { alive = false; off(); offEvent(); offBench(); offHello(); document.removeEventListener('visibilitychange', onVisible); clearTimeout(softTimer); clearTimeout(qTimer); view.unmount(); },
      update(r) {
        const prev = route; route = r;
        if (tab() === 'revisions' && !revs) loadRevs();
        if (JSON.stringify(prev.query) !== JSON.stringify(r.query)) loadTrail();
        view.refresh();
      },
    };
  },
});
