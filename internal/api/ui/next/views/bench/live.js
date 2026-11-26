// The run in progress, shown on every screen: the menu's run card ("Full
// pass · 10 of 96 · about 6 h left", board Bench-Overview), drawn from the
// "bench" topic through the store (state.bench.run is the progress
// document). When a run starts or ends the benchmark lists are read again,
// so the pages' last runs and counts follow.

import { h, morph } from '../../core/dom.js';
import { state, on } from '../../core/store.js';
import { href } from '../../core/router.js';
import { setShellSlot } from '../../core/shell.js';
import { bar } from '../../core/ui.js';
import { num, dur } from '../../core/format.js';
import { activeRun, loadLists } from './common.js';

const card = h('a.run-ind', { href: href('benchmarks') });
let shown = false, lastId = '', lastState = '';

function update() {
  const run = activeRun();
  const id = (state.bench.run && state.bench.run.id) || '', st = (state.bench.run && state.bench.run.state) || '';
  if (id !== lastId || st !== lastState) {
    const ended = lastId && (!run || id !== lastId);
    lastId = id; lastState = st;
    if (ended || run) loadLists();
  }
  if (!run) { if (shown) { shown = false; setShellSlot('run', null); } return; }
  const pct = Math.max(0, Math.min(100, run.percent || 0));
  const of = run.steps_total ? `${num(run.current ? (run.current.index || 0) + 1 : run.steps_done || 0)} of ${num(run.steps_total)}` : '';
  morph(card, [
    h('span.run-ind-t', h('span', { class: 'dot pulse ' + (run.state === 'paused' ? 'off' : 'warn') }), h('b', run.suite_name || run.suite_id), of ? h('span.mono', of) : null),
    bar([{ cls: 'a', value: pct }], 100, `${Math.round(pct)} percent of the run is done`, 'sm'),
    h('span.mut', run.state === 'paused' ? 'waiting until the zones are idle' : run.eta_s != null ? `about ${dur(run.eta_s)} left` : 'running'),
  ]);
  card.setAttribute('aria-label', `Benchmark run ${run.suite_name || run.suite_id}${of ? ', ' + of : ''}. Open Benchmarks.`);
  if (!shown) { shown = true; setShellSlot('run', card); }
}

on('bench', update);
