// Benchmarks: suites and benchmarks (overview), the live panel of a run in
// progress, the editors, run history and detail, and aggregates.
//
// Routes: #/benchmarks (overview), #/benchmarks/suite/<id>[?edit=1],
// #/benchmarks/benchmark/<id>[?edit=1] (a suite's or a benchmark's own page;
// ?edit=1 opens its configuration as a panel over the page),
// #/benchmarks/runs, #/benchmarks/runs/<id>,
// #/benchmarks/trends, #/benchmarks/edit/benchmark/<id|new>[?template=<id>],
// #/benchmarks/edit/suite/<id|new>.
//
// Reads: WebSocket "bench" (state, run, progress, step_started, step_finished,
// request, aggregate_updated) through the store; REST GET /api/bench/state
// (once on entry), /benchmarks, /suites, /runs, /runs/{id}, /aggregates,
// /trends, /schema, /profiles, GET /api/profiles for model file names, and
// GET /api/startups for the start-up figures of the overview.
// Writes: POST/PUT/DELETE /api/bench/benchmarks and /suites, POST
// /api/bench/suites/{id}/run and /api/bench/runs/{id}/cancel (core/api.js).
// The compact run indicator is registered with setShellSlot('run', node) as
// soon as this module loads, so it shows on every screen.

import { h } from '../core/dom.js';
import { registerView, navigate } from '../core/router.js';
import { state, listen } from '../core/store.js';
import * as api from '../core/api.js';
import { liveView, pageHead, tabs, banner } from '../core/ui.js';
import { num } from '../core/format.js';
import { staleBanner } from '../core/status.js';
import { cache, onData, loadRuns, loadAggregates, refreshLists, loadModelProfiles } from './benchmarks/common.js';
import { trackLive, seedState, livePanel } from './benchmarks/live.js';
import { overview } from './benchmarks/overview.js';
import { runsList, runDetail } from './benchmarks/runs.js';
import { aggregatesView, aggState } from './benchmarks/aggregates.js';
import { benchmarkEditor, suiteEditor } from './benchmarks/editor.js';
import { suitePage, benchmarkPage, loadSuiteRuns, loadResults } from './benchmarks/pages.js';
import { needCatalog, onCatalog } from '../core/catalog.js';
import { loadStartups, onStartups } from '../core/startup.js';
import { h as el_ } from '../core/dom.js';

trackLive();

registerView({
  id: 'benchmarks', title: 'Benchmarks',
  mount(el, route) {
    let r = route, alive = true, editor = null, editorKey = '';
    let run = null, runErr = '', runId = '';
    const asked = {};
    const timers = {};
    const later = (k, fn, ms = 400) => { clearTimeout(timers[k]); timers[k] = setTimeout(() => { if (alive) fn(); }, ms); };

    const tabOf = rest => (rest[0] === 'runs' ? 'runs' : rest[0] === 'trends' ? 'trends' : rest[0] === 'edit' ? 'edit'
      : (rest[0] === 'suite' || rest[0] === 'benchmark') && rest[1] ? 'page' : 'overview');
    const suiteFilter = () => r.query.suite || '';

    const loadRun = async id => {
      try { const d = await api.get(`/api/bench/runs/${encodeURIComponent(id)}`); if (id === runId) { run = d; runErr = ''; } } catch (e) { if (id === runId) runErr = e.message; }
      if (alive) view.refresh();
    };

    // What the current route needs, asked for once; refreshed on a "bench" message.
    const need = (force = false) => {
      const tab = tabOf(r.rest);
      if (force || !asked.lists) { asked.lists = true; refreshLists(); }
      if ((tab === 'overview' || tab === 'page') && (force || !asked.aggAll)) { asked.aggAll = true; loadAggregates(false); }
      if (tab === 'overview' && (force || !asked.startups)) { asked.startups = true; loadStartups(api); }
      if (tab === 'page') {
        const k = r.rest[0] + ':' + r.rest[1];
        if (force || asked.page !== k) {
          asked.page = k;
          if (r.rest[0] === 'suite') loadSuiteRuns(r.rest[1], () => { if (alive) view.refresh(); }); else loadResults(r.rest[1], () => { if (alive) view.refresh(); });
          loadModelProfiles();
        }
      }
      if (tab === 'trends') { const k = aggState.same ? 'same' : 'all'; if (force || !asked['agg' + k]) { asked['agg' + k] = true; loadAggregates(aggState.same); } }
      if (tab === 'runs') {
        if (r.rest[1]) { if (force || runId !== r.rest[1]) { if (runId !== r.rest[1]) { run = null; runErr = ''; } runId = r.rest[1]; loadRun(runId); } }
        else if (force || asked.runs !== suiteFilter()) { asked.runs = suiteFilter(); loadRuns(suiteFilter() || undefined); }
      }
    };

    // The editor is a full page for something new (#/benchmarks/edit/...), and a
    // panel over a suite's or a benchmark's page when that page asks for its
    // configuration (?edit=1). The panel is closed with its button, Escape, a
    // click beside it, or the browser's back button.
    let drawer = null;
    const closeDrawer = () => navigate('benchmarks', [r.rest[0], r.rest[1]], null, true);
    const onDrawerKey = e => { if (e.key === 'Escape' && drawer && !document.querySelector('.backdrop')) { e.stopPropagation(); closeDrawer(); } };
    const dropDrawer = () => { if (drawer) { drawer.remove(); drawer = null; document.removeEventListener('keydown', onDrawerKey, true); document.body.style.overflow = ''; } };
    const syncEditor = () => {
      const tab = tabOf(r.rest);
      const inPanel = tab === 'page' && !!r.query.edit;
      if (tab !== 'edit' && !inPanel) { dropDrawer(); if (editor) { editor.destroy(); editor = null; editorKey = ''; } return; }
      const kind = (inPanel ? r.rest[0] : r.rest[1]) === 'suite' ? 'suite' : 'benchmark';
      const id = inPanel ? r.rest[1] : r.rest[2] && r.rest[2] !== 'new' ? r.rest[2] : '';
      const key = [kind, id, r.query.template || '', inPanel ? 'panel' : 'page'].join('|');
      if (key === editorKey) return;
      dropDrawer();
      if (editor) editor.destroy();
      editorKey = key;
      const onDone = () => { if (inPanel) { need(true); closeDrawer(); } else navigate('benchmarks'); };
      editor = kind === 'suite' ? suiteEditor({ id, onDone }) : benchmarkEditor({ id, template: r.query.template, onDone });
      if (inPanel) {
        const panel = el_('aside.drawer', { role: 'dialog', 'aria-modal': 'true', 'aria-label': 'Configuration' },
          el_('div.drawer-head', el_('h2', kind === 'suite' ? 'Configuration of the suite' : 'Configuration of the benchmark'),
            el_('button', { type: 'button', class: 'btn sm', onclick: closeDrawer }, 'Close')),
          editor.el);
        drawer = el_('div.drawer-back', { onmousedown: e => { if (e.target === drawer) closeDrawer(); } }, panel);
        document.body.appendChild(drawer);
        document.body.style.overflow = 'hidden';
        document.addEventListener('keydown', onDrawerKey, true);
        const first = panel.querySelector('button');
        if (first) first.focus();
      }
    };

    const offData = onData(() => view.refresh());
    const offStartups = onStartups(() => view.refresh());
    const offCatalog = onCatalog(() => view.refresh());
    needCatalog();
    const offBench = listen('bench', d => {
      if (!d) return;
      if (d.kind === 'aggregate_updated') { aggState.trendFor = ''; need(true); }
      else if (d.kind === 'run') { later('lists', () => { refreshLists(); if (tabOf(r.rest) === 'page') { asked.page = ''; need(); } if (tabOf(r.rest) === 'runs' && !r.rest[1]) loadRuns(suiteFilter() || undefined); if (runId && d.run && d.run.id === runId) loadRun(runId); }); }
      else if (d.kind === 'step_finished' || d.kind === 'step_started') { if (runId && d.run_id === runId && tabOf(r.rest) === 'runs') later('run', () => loadRun(runId), 600); }
    });

    const render = () => {
      const tab = tabOf(r.rest);
      need();
      const nb = cache.benchmarks, ns = cache.suites;
      const sub = nb && ns ? [h('b', num(ns.length)), ` ${ns.length === 1 ? 'suite' : 'suites'} · `, h('b', num(nb.length)), ` ${nb.length === 1 ? 'benchmark' : 'benchmarks'}`] : 'Suites, benchmarks and their runs';
      const live = livePanel();
      const tabbar = tab === 'edit' || tab === 'page' ? null : tabs([{ id: 'overview', label: 'Overview' }, { id: 'runs', label: 'Runs' }, { id: 'trends', label: 'Aggregates' }], tab,
        id => navigate('benchmarks', id === 'overview' ? [] : [id], null, true));
      let body;
      if (tab === 'page') {
        const page = r.rest[0] === 'suite' ? suitePage(r.rest[1]) : benchmarkPage(r.rest[1]);
        return [page[0], staleBanner(), live, page.slice(1)];
      }
      if (tab === 'edit') body = editor ? editor.el : null;
      else if (tab === 'runs') body = r.rest[1] ? runDetail(run, runErr) : runsList(suiteFilter(), v => navigate('benchmarks', ['runs'], v ? { suite: v } : null, true));
      else if (tab === 'trends') body = aggregatesView(() => { need(); view.refresh(); });
      else body = overview();
      const stateNote = state.bench.state && state.bench.state.enabled === false ? banner('warn', 'Benchmarks are off on this scheduler.') : null;
      return [pageHead('Benchmarks', sub), staleBanner(), stateNote, live, tabbar, body];
    };
    const view = liveView(el, render, ['status', 'tick', 'bench', 'conn']);
    syncEditor();
    seedState();
    return {
      unmount() { alive = false; Object.values(timers).forEach(clearTimeout); offData(); offStartups(); offCatalog(); offBench(); dropDrawer(); if (editor) editor.destroy(); view.unmount(); },
      update(nr) { r = nr; syncEditor(); view.refresh(); },
    };
  },
});
