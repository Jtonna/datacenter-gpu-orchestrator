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
import { bench, onBench, loadLists, seedState, activeRun, when, tookS, resultIcon, dots, inputsLine } from './bench/common.js';
import { listColumn, groupPanel, runningRow, kindIcon } from './bench/list.js';
import { runDialog } from './bench/run.js';
import { resultsData, loadResults, latestTable, latestHead } from './bench/results.js';
import { runsData, loadRuns, runsColumn, runPanel, reportPanel, directLine } from './bench/runs.js';
import { dur } from '../core/format.js';
import './bench/live.js';

// ---------- Overview ----------


function recentRow(rn) {
  const direct = rn.kind === 'benchmark';
  const name = rn.suite_name || rn.benchmark_id || rn.suite_id || rn.id;
  const inp = direct ? inputsLine(rn) : '';
  return h('a.bp-recent', { key: rn.id, href: href('benchmarks', ['runs', String(rn.number || rn.id)]), role: 'listitem', 'data-run': rn.number || rn.id },
    h('span.bp-ri', resultIcon(rn, 14)),
    h('span.bp-when', when(rn.started_at || rn.queued_at)),
    kindIcon(!direct),
    h('span.bp-name', { title: direct ? directLine(rn) : name }, name, inp ? h('span.bp-sub', ' · ' + inp) : null),
    h('span.bp-resw', dots(rn.counts, rn.result === 'running')),
    h('span.mut.bp-took', tookS(rn) != null ? dur(tookS(rn)) : DASH));
}

// "Run a benchmark": the run dialog, asking first what to run.
const runABenchmark = () => h('button.btn.pri', { type: 'button', 'data-act': 'run-a-benchmark', onclick: () => runDialog({ pick: true }) }, 'Run a benchmark');

function overview(ov, res) {
  const run = activeRun();
  return [
    h('h1.sr', 'Benchmarks'),
    run ? h('section.card.bp-runcard', { 'aria-label': 'Running now' }, runningRow(run, { inline: true, after: runABenchmark() }))
      : h('div.bp-tools', h('span.grow'), runABenchmark()),
    h('section.card.bp-latest', { 'aria-label': 'Latest result for each model' },
      h('div.ch', h('h2', 'Latest result for each model'), h('span.grow'), latestHead(res)),
      latestTable(res)),
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
    // the results per model (Overview table, the Runs page's views): read on
    // entry and again when a run ends
    const res = resultsData();
    const readResults = () => loadResults(res, { force: true }).then(ch => { if (ch && alive) view.refresh(); });
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
      if (pg === 'overview') return [staleBanner(), overview(ov, res)];
      if (pg === 'runs') {
        loadRuns(rd, r.query).then(ch => { if (ch && alive) view.refresh(); });
        // the result views' filters (From, Context) choose what the route answers
        if (r.query.show) loadResults(res, { ctx: r.query.ctx, run: r.query.from }).then(ch => { if (ch && alive) view.refresh(); });
        return [h('h1.sr', 'Runs'), staleBanner(), runsColumn(rd, r, res)];
      }
      return [h('h1.sr', 'Groups and benchmarks'), staleBanner(), listColumn(r, query, setQ, !!r.rest[1])];
    };
    const view = liveView(el, render, ['status', 'tick', 'conn', 'bench']);
    needCatalog({ live: false });
    seedState();
    loadLists();
    // the Overview's recent runs and results per model only where they are
    // drawn (the Runs page reads its own list; its result views read the
    // results in render, with their filters)
    if (pageOf(r) === 'overview') { loadOverview(); readResults(); }
    syncSide();
    return {
      update(nr) {
        const was = pageOf(r);
        r = nr;
        if (pageOf(r) === 'overview' && was !== 'overview') { loadOverview(); readResults(); }
        if (pageOf(r) === 'runs' && r.query.show && was !== 'runs') readResults();
        if (pageOf(r) === 'all') loadLists();
        syncSide();
        view.refresh();
      },
      unmount() { alive = false; offBench(); offCatalog(); closeSide(); view.unmount(); },
    };
  },
});
