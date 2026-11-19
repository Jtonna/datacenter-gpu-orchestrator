// The one-click run: a confirm dialog that says plainly that a run loads and
// unloads models on the cluster, then POST /api/bench/suites/{id}/run.

import { h } from '../../core/dom.js';
import * as api from '../../core/api.js';
import { state } from '../../core/store.js';
import { confirm, toast, nodeChips } from '../../core/ui.js';
import { plural, dur, DASH } from '../../core/format.js';
import { benchmarkById, suiteTitle, cache } from './common.js';

// The benchmarks of a suite, in order, with parallel members expanded.
export function suiteBenchmarks(suite) {
  const out = [];
  for (const it of suite.benchmarks || []) {
    const bm = typeof it === 'string' ? benchmarkById(it) : it;
    if (!bm) { out.push({ id: it, name: String(it), profile: '' }); continue; }
    out.push(bm);
  }
  return out;
}

export function profilesOf(list) {
  const set = new Set();
  for (const bm of list) {
    if (bm.parallel) for (const id of bm.parallel) { const m = benchmarkById(id); if (m && m.profile) set.add(m.profile); }
    else if (bm.profile) set.add(bm.profile);
  }
  return Array.from(set);
}

export async function runSuite(suite) {
  const st = state.status || {};
  const list = suiteBenchmarks(suite);
  const profiles = profilesOf(list);
  const nodes = ((st.view && st.view.nodes) || []).filter(n => !n.excluded && n.online);
  const insts = st.instances || [];
  const inFlight = insts.reduce((s, i) => s + (i.in_flight || 0), 0);
  const waiting = (st.queue || []).length;
  const idleOnly = suite.only_when_idle !== false;
  const wait = suite.idle_wait_s == null ? 600 : suite.idle_wait_s;

  const hits = [
    { kind: 'x', text: 'This loads and unloads models on the cluster. While it runs, the nodes are busy with the benchmarks and the models they load.' },
    { kind: '', text: [h('b', plural(list.length, 'benchmark')), ' in this order: ', list.map(b => b.name || b.id).join(', '), '.'] },
    { kind: '', text: ['Profiles used: ', profiles.length ? h('span.mono', profiles.join(', ')) : DASH, '.'] },
    { kind: '', text: nodes.length ? ['Nodes that may be used: ', nodeChips(nodes.map(n => n.hostname || n.id)), '.'] : 'No node is online right now, so the benchmarks will be refused or wait.' },
  ];
  if (insts.length || inFlight || waiting) {
    const names = insts.map(i => i.profile).join(', ');
    const busy = [];
    if (insts.length) busy.push(`${plural(insts.length, 'model')} loaded now (${names})`);
    if (inFlight) busy.push(`${plural(inFlight, 'request')} in flight`);
    if (waiting) busy.push(`${plural(waiting, 'request')} waiting in the queue`);
    hits.push({ kind: 'a', text: 'The scheduler is not idle: ' + busy.join(', ') + '. ' + (idleOnly
      ? `This suite only runs when the scheduler is idle, so it will wait, up to ${dur(wait)}, and be skipped if that does not happen.`
      : 'This suite does not wait for idle, so loaded models can be unloaded to make room for the benchmarks.') });
  } else if (idleOnly) {
    hits.push({ kind: 'm', text: 'The scheduler is idle now, so the run starts at once.' });
  }
  if (suite.stop_on_failure) hits.push({ kind: '', text: 'It stops at the first benchmark that fails.' });

  const note = h('input.input', { type: 'text', maxlength: 200, placeholder: 'For example: after the link change', 'aria-label': 'Note for this run' });
  const ok = await confirm({
    title: `Run ${suiteTitle(suite)}?`,
    intro: 'One click starts the whole suite. Everything is measured and kept as a run you can open later.',
    hits,
    extra: h('div.field', h('label', 'Note (optional)'), note, h('span.help', 'Shown with the run in the history.')),
    confirmLabel: `Run ${plural(list.length, 'benchmark')}`,
    cancelLabel: 'Not now',
  });
  if (!ok) return null;
  try {
    const body = note.value.trim() ? { note: note.value.trim() } : {};
    const run = await api.post(`/api/bench/suites/${encodeURIComponent(suite.id)}/run`, body);
    toast(run && run.state === 'queued' ? 'The run is queued and starts shortly.' : 'The run was started.', 'ok');
    return run;
  } catch (e) {
    toast(e.status === 429 ? 'Too many runs are queued. Wait for one to finish or cancel one. ' + e.message : 'The run did not start: ' + e.message, 'err');
    return null;
  }
}

export const runDisabledReason = () => (cache.off ? 'Benchmarks are off on this scheduler' : null);
