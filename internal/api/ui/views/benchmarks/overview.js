// Overview: the suites (with a Run button), the benchmarks, the external
// result sets and the start-up figures of the machines.

import { h } from '../../core/dom.js';
import { href, navigate } from '../../core/router.js';
import { state } from '../../core/store.js';
import { badge, button, table, empty, skeleton } from '../../core/ui.js';
import { num, plural, ago, dur, DASH } from '../../core/format.js';
import { cache, suiteTitle, suiteStatusBadge, suiteNotRunnable, runBadge, rateBadge, phaseChips, scheduleText, describe, modelOf, errorBanner } from './common.js';
import { runSuite } from './runconfirm.js';
import { passDots } from '../../core/rate.js';
import { catalog } from '../../core/catalog.js';
import { avatar } from '../../core/avatar.js';
import { startupsCard } from './startups.js';
import { externalCard } from './external.js';

const play = 'M6 4l10 6-10 6z';

function lastRunCell(s, agg) {
  const r = s.last_run;
  if (!r) return h('span.mut', 'Never run');
  const sum = r.summary;
  return h('a', { href: href('benchmarks', ['runs', r.id]), style: 'display:block;font-weight:600;color:inherit' },
    h('span.row', { style: 'gap:8px' }, runBadge(r.state), r.finished_at || r.started_at ? ago(r.finished_at || r.started_at) : null),
    sum ? h('span.sub', `${sum.passed} passed · ${sum.failed} failed` + (sum.regressions ? ` · ${plural(sum.regressions, 'regression')}` : '')) : null);
}

function suitesCard(running) {
  const suites = cache.suites;
  const aggs = (cache.agg.all && cache.agg.all.suites) || [];
  let body;
  if (cache.errors.suites && !suites) body = empty('The suites could not be read', cache.errors.suites.message);
  else if (!suites) body = skeleton(3);
  else if (!suites.length) body = empty('No suites yet', 'A suite is an ordered list of benchmarks that run together, on a schedule or with one click. Create a benchmark first, then group it into a suite.',
    h('a.btn.sm', { href: href('benchmarks', ['edit', 'suite', 'new']) }, 'New suite'));
  else body = table({
    minWidth: 960, rows: suites, key: s => s.id, onRow: s => navigate('benchmarks', ['suite', s.id]),
    columns: [
      { label: 'Suite', render: s => h('span', h('span.row', { style: 'gap:8px' }, h('a', { href: href('benchmarks', ['suite', s.id]), style: 'font-weight:700', onclick: e => e.stopPropagation() }, suiteTitle(s)), suiteStatusBadge(s)),
        s.reason ? h('span.sub', s.reason) : s.description ? h('span.sub', s.description) : h('span.sub.mono', s.id)) },
      { label: 'Benchmarks', cls: 'r', render: s => num((s.benchmarks || []).length) },
      { label: 'Schedule', cls: 'tight', render: s => h('span', scheduleText(s.schedule), s.only_when_idle === false ? h('span.sub', 'Does not wait for idle') : null) },
      { label: 'Last run', render: s => (s.status && !s.last_run ? h('span.mut', 'Cannot run yet') : lastRunCell(s)) },
      { label: 'Pass rate', cls: 'tight', title: 'One dot per run, oldest left. Mint: every benchmark passed. Amber: some failed. Coral: failed.', render: s => { const a = aggs.find(x => x.suite_id === s.id); return passDots(a && a.recent, a && a.pass_rate, { label: `Runs of ${suiteTitle(s)}, oldest first` }); } },
      { label: '', cls: 'tight', render: s => h('span.row', { style: 'gap:8px;justify-content:flex-end;flex-wrap:nowrap' },
        h('a', { class: 'btn sm', href: href('benchmarks', ['suite', s.id], { edit: 1 }), title: 'Open the configuration of the suite', onclick: e => e.stopPropagation() }, 'Configure'),
        h('button', { class: 'btn sm', type: 'button', disabled: !!cache.off || !!suiteNotRunnable(s), title: suiteNotRunnable(s) || (running ? 'A run is active; this one is queued behind it' : 'Run the suite now'),
          onclick: e => { e.stopPropagation(); runSuite(s); } }, 'Run')) },
    ],
  });
  return h('section.card', { key: 'suites' },
    h('div.ch', h('h2', 'Suites'), h('span.note', 'Open one for its runs. Run a whole suite with one click.'), h('span.right', h('a.btn.sm', { href: href('benchmarks', ['edit', 'suite', 'new']) }, 'New suite'))),
    body);
}

function benchmarksCard() {
  const list = cache.benchmarks;
  const aggs = (cache.agg.all && cache.agg.all.benchmarks) || [];
  let body;
  if (cache.errors.benchmarks && !list) body = empty('The benchmarks could not be read', cache.errors.benchmarks.message);
  else if (!list) body = skeleton(4);
  else if (!list.length) body = empty('No benchmarks yet', 'A benchmark is one test case: which model to use and what to do with it, such as only loading it, or loading it and sending a conversation. Start from a template.',
    h('a.btn.sm', { href: href('benchmarks', ['edit', 'benchmark', 'new']) }, 'New benchmark'));
  else body = table({
    minWidth: 1040, rows: list, key: b => b.id, onRow: b => navigate('benchmarks', ['benchmark', b.id]),
    columns: [
      { label: 'Benchmark', render: b => h('span.named', { style: 'max-width:460px' }, avatar(b.parallel ? null : catalog.byProfile[b.profile] || catalog.byKey[modelOf(b.profile)] || null, 28),
        h('span.grow', h('a', { href: href('benchmarks', ['benchmark', b.id]), style: 'font-weight:700', onclick: e => e.stopPropagation() }, b.name || b.id),
          b.description ? h('span.sub', { style: 'font-weight:500' }, b.description) : null)) },
      { label: 'Profile', cls: 'tight', render: b => (b.parallel ? h('span.mut', 'Several') : h('span', h('span.mono.nowrap', b.profile), modelOf(b.profile) ? h('span.sub.mono', modelOf(b.profile)) : null)) },
      { label: 'Phases', render: b => phaseChips(b) },
      { label: 'Passed', cls: 'tight', title: 'One dot per run, oldest left, with the share that passed', render: b => {
        const a = aggs.find(x => x.benchmark_id === b.id);
        if (!a || !a.steps) return h('span.mut', 'Not run yet');
        return h('span', passDots(a.recent, a.pass_rate, { label: `Runs of ${b.name || b.id}, oldest first` }), h('span.sub', a.last_at ? ['last run ', ago(a.last_at)] : null));
      } },
      { label: '', cls: 'tight', render: b => h('a', { class: 'btn sm', href: href('benchmarks', ['benchmark', b.id], { edit: 1 }), title: 'Open the configuration of the benchmark', onclick: e => e.stopPropagation() }, 'Configure') },
    ],
  });
  return h('section.card', { key: 'benchmarks' },
    h('div.ch', h('h2', 'Benchmarks'), h('span.note', 'Test cases: what to load and what to do with it. Open one for its results.'), h('span.right', h('a.btn.sm', { href: href('benchmarks', ['edit', 'benchmark', 'new']) }, 'New benchmark'))),
    body);
}

export function overview() {
  const running = state.bench.run;
  return [
    cache.off ? errorBanner('Benchmarks are off on this scheduler. ' + cache.off) : null,
    suitesCard(running),
    benchmarksCard(),
    externalCard(),
    startupsCard(),
  ];
}
