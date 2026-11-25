// Benchmarks (boards Bench-Overview, Bench, Bench-Group, Bench-Runs,
// Bench-Runs-Group, Bench-Report). Replaces the classic benchmark pages
// (dropped: the owner's word, 25 Nov); the API routes and the data stay.
//
// Routes:
//   #/benchmarks                        Overview
//   #/benchmarks/all                    the groups and benchmarks
//   #/benchmarks/all/<group>            the same, narrow, with the group's panel
//   #/benchmarks/runs[?kind&result&model&suite&page]   the runs
//   #/benchmarks/runs/<number or id>    the runs with a run's panel
//   #/benchmarks/runs/<number>/<slug>   the runs with a benchmark's report
// The classic addresses still open something sensible: suite/<id> is the
// group's panel; benchmark/<id>, trends, external/..., edit/... the list.
//
// Reads: GET /api/bench/state (seed; then the "bench" topic through the
// store), /groups, /benchmarks, /runs, /runs/<ref>, /runs/<id>/report,
// /groups/<id>/expand. Model names from the catalog's REST copy
// (needCatalog({live: false})): these pages show no plans.

import { h } from '../core/dom.js';
import { registerView, href, navigate } from '../core/router.js';
import { state } from '../core/store.js';
import * as api from '../core/api.js';
import { liveView, empty, skeleton } from '../core/ui.js';
import { num, DASH } from '../core/format.js';
import { staleBanner } from '../core/status.js';
import { needCatalog, onCatalog } from '../core/catalog.js';
import { bench, onBench, loadLists, seedState, activeRun, when, tookS, resultIcon, dots, inputsLine, notYet, WHY } from './bench/common.js';
import { listColumn, groupPanel, runningRow } from './bench/list.js';
import { runsData, loadRuns, runsColumn, runPanel, reportPanel, directLine } from './bench/runs.js';
import { dur } from '../core/format.js';
import './bench/live.js';

// ---------- Overview ----------

const LATEST_COLS = ['Model', 'Smallest fit', 'Cold load', 'Reload', 'Generation, tok/s', 'First token', 'Coding, of 102'];

function recentRow(rn) {
  const direct = rn.kind === 'benchmark';
  return h('a.bp-recent', { key: rn.id, href: href('benchmarks', ['runs', String(rn.number || rn.id)]), role: 'listitem', 'data-run': rn.number || rn.id },
    h('span.bp-when', when(rn.started_at || rn.queued_at)),
    h('span.bp-name', { title: direct ? directLine(rn) : rn.suite_name }, direct ? directLine(rn) : rn.suite_name || rn.suite_id),
    h('span.bp-resw', resultIcon(rn, 14), dots(rn.counts, rn.result === 'running')),
    h('span.mut.bp-took', tookS(rn) != null ? dur(tookS(rn)) : DASH));
}

function overview(ov) {
  const run = activeRun();
  return [
    h('h1.sr', 'Benchmarks'),
    run ? h('section.card.bp-runcard', { 'aria-label': 'Running now' }, runningRow(run)) : null,
    h('div.bp-tools', h('span.grow'), notYet(h('span.btn.pri', 'Run a benchmark'), WHY.run)),
    h('section.card.bp-latest', { 'aria-label': 'Latest result for each model' },
      h('div.ch', h('h2', 'Latest result for each model'), h('span.grow'), notYet(h('span.mut', 'Smallest fit · context 8,192'), WHY.b5)),
      h('div.bp-tablewrap', h('table.bp-runs', h('thead', h('tr', LATEST_COLS.map(c => h('th', c)))),
        h('tbody', h('tr', h('td', { colspan: LATEST_COLS.length }, notYet('Not available yet: the latest result per model is not served yet.', WHY.b5, 'p')))))),
      h('p.mut.bp-cardnote', 'Until then each run\'s figures are in its report: ', h('a', { href: href('benchmarks', ['runs']) }, 'Runs'), '.')),
    h('section.card.bp-recentcard', { 'aria-label': 'Recent runs' },
      h('div.ch', h('h2', 'Recent runs'), h('span.grow'), h('a', { href: href('benchmarks', ['runs']) }, ov.total != null ? `All ${num(ov.total)}` : 'All runs')),
      ov.err ? h('div.banner.err', { role: 'alert' }, ov.err) : null,
      ov.runs == null ? skeleton(4) : ov.runs.length ? h('div.bp-rows', { role: 'list', 'aria-label': 'Recent runs' }, ov.runs.map(recentRow))
        : empty('No runs yet', 'A run appears here when it is queued.')),
  ];
}

// ---------- the view ----------

const pageOf = r => (r.rest[0] === 'runs' ? 'runs' : ['all', 'suite', 'benchmark', 'edit', 'external', 'trends'].includes(r.rest[0]) ? 'all' : 'overview');

registerView({
  id: 'benchmarks', title: 'Benchmarks',
  mount(el, route) {
    let r = route, alive = true, side = null, query = route.query.q || '';
    // old addresses: suite/<id> -> all/<id>; the other classic pages -> the list
    if (r.rest[0] === 'suite' && r.rest[1]) { navigate('benchmarks', ['all', r.rest[1]], {}, true); }
    else if (['benchmark', 'edit', 'external', 'trends'].includes(r.rest[0])) { navigate('benchmarks', ['all'], {}, true); }
    const ov = { runs: null, total: null, err: '' };
    const rd = runsData();
    const loadOverview = async () => {
      try { const d = await api.get('/api/bench/runs', { limit: 4 }); ov.runs = d.runs || []; ov.total = d.total != null ? d.total : null; ov.err = ''; } catch (e) { ov.err = e.message; ov.runs = ov.runs || []; }
      if (alive) view.refresh();
    };
    const closeSide = () => { if (side) { const s = side; side = null; s.close(); } };
    const syncSide = () => {
      const pg = pageOf(r);
      let want = '', make = null;
      if (pg === 'all' && r.rest[0] === 'all' && r.rest[1]) {
        const id = r.rest[1];
        want = id; make = () => groupPanel(id, () => { if (side && side.key === id) { side = null; navigate('benchmarks', ['all'], r.query.q ? { q: r.query.q } : {}, true); } });
      } else if (pg === 'runs' && r.rest[1]) {
        const ref = r.rest[1], slug = r.rest[2] || '';
        const back = () => navigate('benchmarks', ['runs'], Object.assign({}, r.query), true);
        if (slug) { want = 'rep:' + ref + '/' + slug; make = () => reportPanel(ref, slug, () => { if (side && side.key === want) { side = null; back(); } }); }
        else { want = 'run:' + ref; make = () => runPanel(ref, r.query, () => { if (side && side.key === want) { side = null; back(); } }); }
      }
      if (!want) { closeSide(); return; }
      if (side && side.key === want) { side.redraw(); return; }
      closeSide();
      side = make();
    };
    const offBench = onBench(() => { if (alive) { view.refresh(); if (side) side.redraw(); } });
    const offCatalog = onCatalog(() => { if (alive) { view.refresh(); if (side) side.redraw(); } });
    const setQ = v => { query = v; view.refresh(); };
    const render = () => {
      const pg = pageOf(r);
      if (bench.off) return [h('h1.sr', 'Benchmarks'), h('section.card', empty('Benchmarks are off', bench.off))];
      if (pg === 'overview') return [staleBanner(), overview(ov)];
      if (pg === 'runs') {
        loadRuns(rd, r.query).then(ch => { if (ch && alive) view.refresh(); });
        return [h('h1.sr', 'Runs'), staleBanner(), runsColumn(rd, r)];
      }
      return [h('h1.sr', 'Groups and benchmarks'), staleBanner(), listColumn(r, query, setQ, !!r.rest[1])];
    };
    const view = liveView(el, render, ['status', 'tick', 'conn', 'bench']);
    needCatalog({ live: false });
    seedState();
    loadLists();
    loadOverview();
    syncSide();
    return {
      update(nr) {
        const was = pageOf(r);
        r = nr;
        if (pageOf(r) === 'overview' && was !== 'overview') loadOverview();
        if (pageOf(r) === 'all') loadLists();
        syncSide();
        view.refresh();
      },
      unmount() { alive = false; offBench(); offCatalog(); closeSide(); view.unmount(); },
    };
  },
});
