// The live run panel and the compact run indicator in the shell. All of it is
// drawn from the WebSocket "bench" topic through the store (state.bench.run is
// the progress object); the one REST call is the seed on entry (GET
// /api/bench/state), done by the view.

import { h, morph } from '../../core/dom.js';
import * as api from '../../core/api.js';
import { state, on, listen, emit } from '../../core/store.js';
import { href } from '../../core/router.js';
import { setShellSlot } from '../../core/shell.js';
import { badge, bar, progress, button, confirm, toast, nodeChips, banner } from '../../core/ui.js';
import { nodeByAddr, healthBadge } from '../../core/status.js';
import { num, bytes, dur, clock, now, DASH, plural } from '../../core/format.js';
import { cache, suiteById, benchmarkById, phaseTitle, reasonNode } from './common.js';

// ---------- what the live messages add to the progress object ----------

let lastReq = null;       // the newest "request" message of the active run
let lastRunMsg = null;    // the newest "run" message (it carries paused_because)
let seenRun = null, seenAt = 0;

export function trackLive() {
  listen('bench', d => {
    if (!d) return;
    if (d.kind === 'request') lastReq = Object.assign({ at: Date.now() }, d);
    else if (d.kind === 'run') lastRunMsg = d.run || null;
    else if (d.kind === 'step_started') lastReq = null;
  });
  // core/shell.js builds a fresh data-keep wrapper around the slot node on every
  // draw, which moves the node out of the wrapper that stays. Put it back (same task, no flicker).
  on(['tick', 'status', 'conn', 'hello', 'bench'], () => queueMicrotask(rehome));
  on('bench', () => {
    if (state.bench.run !== seenRun) { seenRun = state.bench.run; seenAt = Date.now(); }
    if (!state.bench.run) { lastReq = null; lastRunMsg = null; }
    updateIndicator();
  });
}

// Seed from GET /api/bench/state once on entry (the "state" message does the
// same on connect; this covers a page that opened between the two).
export async function seedState() {
  try {
    const doc = await api.get('/api/bench/state');
    state.bench.state = doc;
    if (doc.run && !state.bench.run) state.bench.run = doc.run;
    emit('bench');
  } catch (e) { if (e.status === 503) cache.off = e.message; }
}

const drift = () => (Date.now() - seenAt) / 1000;
const elapsedOf = run => Math.max(0, (now() - run.started_at) / 1000);
const etaOf = run => (run.eta_s == null ? null : Math.max(0, run.eta_s - drift()));
const pctOf = v => Math.max(0, Math.min(100, v || 0));

function pausedReason(run) {
  if (run.state !== 'paused') return null;
  const p = lastRunMsg && lastRunMsg.progress;
  return (p && p.paused_because) || null;
}

// ---------- the indicator in the shell ----------

const indicator = h('a.run-ind', { href: href('benchmarks'),
  style: 'display:flex;flex-direction:column;gap:6px;padding:11px 12px;border:1px solid var(--line-2);background:var(--card);border-radius:10px;color:var(--text);font-size:12px;font-weight:600' });
let indicatorOn = false;

function updateIndicator() {
  const run = state.bench.run;
  if (!run || ['completed', 'failed', 'cancelled', 'skipped'].includes(run.state)) {
    if (indicatorOn) { indicatorOn = false; setShellSlot('run', null); }
    return;
  }
  const cur = run.current;
  morph(indicator, [
    h('span', { style: 'display:flex;align-items:center;gap:8px;font-size:13px;font-weight:700' },
      h('span', { class: 'dot pulse ' + (run.state === 'paused' ? 'off' : 'warn') }),
      run.state === 'paused' ? 'Benchmark waiting' : 'Benchmark running'),
    h('span.txt2', { style: 'overflow-wrap:anywhere' }, run.suite_name || run.suite_id),
    bar([{ cls: 'a', value: pctOf(run.percent) }], 100, `${Math.round(pctOf(run.percent))} percent of the run is done`, 'sm'),
    h('span.mut', { style: 'display:flex;justify-content:space-between;gap:8px' },
      h('span.mono', `${Math.round(pctOf(run.percent))}%`),
      h('span', { style: 'overflow-wrap:anywhere;text-align:right' }, cur ? cur.name : run.state === 'paused' ? 'Waiting for idle' : 'Starting')),
  ]);
  indicator.setAttribute('aria-label', `Benchmark run ${run.suite_name}, ${Math.round(pctOf(run.percent))} percent. Open Benchmarks.`);
  if (!indicatorOn) { indicatorOn = true; setShellSlot('run', indicator); queueMicrotask(rehome); }
}

function rehome() {
  if (!indicatorOn) return;
  for (const w of document.querySelectorAll('.pane-foot > div')) {
    if (w.__key === 'slot-run' && indicator.parentNode !== w) w.replaceChildren(indicator);
  }
}

// ---------- the panel ----------

const WORKER = { waiting: ['Waiting', 'n'], receiving: ['Receiving', 'load'], done: ['Done', 'use'] };

function workerRows(st, load) {
  return (load.workers || []).map(w => {
    const node = nodeByAddr(st, w.addr);
    const name = node ? node.hostname || node.id : w.addr;
    const [label, kind] = WORKER[w.state] || [w.state, 'n'];
    const hb = healthBadge(node);
    return h('div.nr', { key: 'w-' + w.addr },
      h('span.mono', name),
      bar([{ cls: w.state === 'done' ? 'm' : 'a', value: w.bytes_sent || 0 }], w.bytes_total, `${name}: ${label}`),
      h('span.mono.fig', w.bytes_sent != null ? `${bytes(w.bytes_sent)} / ${bytes(w.bytes_total)}` : bytes(w.bytes_total)),
      h('span.row', { style: 'gap:6px' }, badge(label, kind), hb));
  });
}

// What is in flight inside the current benchmark.
function inFlight(st, run) {
  const cur = run.current, d = cur.detail || {}, type = cur.phase && cur.phase.type;
  const rows = [];
  if (type === 'load' || (d.load && d.load.workers)) {
    const load = d.load || {};
    const frac = load.progress;
    rows.push(h('div.cb', { key: 'load-line', style: 'padding-bottom:10px' },
      h('span.txt2', { style: 'font-weight:600' }, frac != null ? `Model ${Math.round(frac * 100)}% loaded` : 'Loading the model'),
      load.eta_s != null ? h('span.mut', ` · about ${dur(Math.round(load.eta_s))} left`) : null,
      load.stalled_s != null ? h('span.err-t', ` · stalled, no new bytes for ${dur(Math.round(load.stalled_s))}`) : null));
    rows.push(workerRows(st, load));
    return rows;
  }
  const req = lastReq && lastReq.run_id === run.id && lastReq.idx === cur.index ? lastReq : null;
  if (type === 'conversation' && d.turns) {
    const frac = d.max_tokens ? Math.min(1, (d.tokens || 0) / d.max_tokens) : 0;
    rows.push(h('div.cb', { key: 'turn' },
      h('div.prog-top', h('span', `Turn ${d.turn || 0} of ${d.turns}`), h('span.mut.mono', d.max_tokens ? `${num(d.tokens || 0)} of up to ${num(d.max_tokens)} tokens` : `${num(d.tokens || 0)} tokens`)),
      bar([{ cls: 'a', value: frac }], 1, `Turn ${d.turn} of ${d.turns}: ${d.tokens || 0} tokens so far`, 'sm')));
    return rows;
  }
  if (d.tokens != null || req) {
    const tokens = d.tokens != null ? d.tokens : req && req.tokens;
    const max = d.max_tokens;
    rows.push(h('div.cb', { key: 'req' },
      h('div.prog-top', h('span', req ? ['Request ', h('span.mono', req.request || ('#' + req.seq)), req.state === 'streaming' ? ', streaming' : ', sent'] : 'Request in flight'),
        h('span.mut.mono', tokens != null ? (max ? `${num(tokens)} of up to ${num(max)} tokens` : `${num(tokens)} tokens`) : '', req && req.elapsed_ms ? ` · ${dur(req.elapsed_ms / 1000)}` : '')),
      max && tokens != null ? bar([{ cls: 'a', value: Math.min(1, tokens / max) }], 1, `${tokens} of ${max} tokens`, 'sm') : null));
  }
  return rows;
}

function queueList(run) {
  const suite = suiteById(run.suite_id);
  const cur = run.current ? run.current.index : -1;
  const name = id => { const b = benchmarkById(id); return b ? b.name || b.id : id; };
  let items;
  if (suite && (suite.benchmarks || []).length) {
    items = suite.benchmarks.map((it, i) => {
      const id = typeof it === 'string' ? it : it.id;
      return { id, i, name: name(id), state: i < cur ? 'done' : i === cur ? 'now' : 'next' };
    });
  } else {
    items = (run.current ? [{ id: run.current.benchmark_id, name: run.current.name, state: 'now' }] : [])
      .concat((run.queue || []).map(id => ({ id, name: name(id), state: 'next' })));
  }
  const kind = { done: 'm', now: 'a', next: '' };
  const label = { done: 'Done', now: 'Running now', next: 'Waiting' };
  return h('div', { style: 'display:flex;flex-direction:column;gap:8px;padding:0 20px 16px;max-height:300px;overflow:auto' },
    items.map((it, k) => h('div.why', { key: 'q-' + k, style: it.state === 'done' ? 'opacity:.7' : null },
      h('i', { class: kind[it.state] }), h('span', { style: it.state === 'now' ? 'font-weight:700' : null }, it.name), h('span.mut', { style: 'font-size:12px' }, label[it.state]))));
}

async function cancelRun(run) {
  const ok = await confirm({
    title: 'Cancel this run?',
    intro: `${run.suite_name || run.suite_id} is at ${Math.round(pctOf(run.percent))}%.`,
    hits: [
      { kind: 'x', text: 'It stops after the current request. A load in progress or a hold is abandoned at once, and the model is unloaded again.' },
      { kind: '', text: 'Benchmarks that already finished keep their results. The run is kept as cancelled.' },
    ],
    confirmLabel: 'Cancel the run', cancelLabel: 'Keep running', danger: true,
  });
  if (!ok) return;
  try {
    await api.post(`/api/bench/runs/${encodeURIComponent(run.id)}/cancel`);
    toast('Cancelling. The run stops after its current request.', 'ok');
  } catch (e) { toast('Could not cancel: ' + e.message, 'err'); }
}

// livePanel() -> the node to show above everything while a run is active, else null.
export function livePanel() {
  const run = state.bench.run;
  const st = state.status || {};
  if (!run) return null;
  const cur = run.current, next = run.next;
  const pct = pctOf(run.percent);
  const eta = etaOf(run);
  const why = pausedReason(run);
  const nTotal = run.steps_total || 0;

  const overall = progress({
    fraction: pct / 100, cls: run.state === 'paused' ? 'l' : 'a',
    label: `${Math.round(pct)} percent of the run is done`,
    left: h('span', h('span.pct', `${Math.round(pct)}%`), h('span.mut', { style: 'font-weight:600' }, ` · ${num(run.steps_done || 0)} of ${num(nTotal)} benchmarks done`)),
    mid: h('span.mut', { style: 'font-weight:600' }, `${dur(Math.round(elapsedOf(run)))} elapsed`),
    right: h('span.mut', { style: 'font-weight:600' }, eta != null ? `About ${dur(eta < 90 ? Math.round(eta) : Math.round(eta / 60) * 60)} left` : 'Time left not known yet'),
    foot: ['Started ' + clock(run.started_at), run.id],
  });

  let curNode;
  if (!cur) {
    curNode = h('div.cb', h('span.mut', run.state === 'paused' ? 'Waiting for the scheduler to be idle before the first benchmark.' : 'Starting the first benchmark.'));
  } else {
    const ph = cur.phase || {};
    const total = ph.phases_total || ph.of || 0;
    const d = cur.detail || {};
    curNode = h('div', { key: 'cur-' + cur.index },
      h('div.cs', { style: 'display:flex;flex-direction:column;gap:10px' },
        h('div.row', { style: 'gap:6px 12px' },
          h('span.mut', { style: 'font-size:12px;font-weight:700;letter-spacing:.06em;text-transform:uppercase' }, `Benchmark ${(cur.index || 0) + 1} of ${nTotal}`)),
        h('div.row', { style: 'gap:6px 12px' }, h('b', { style: 'font-size:17px;font-weight:800' }, cur.name), h('span.mono.mut', cur.profile),
          cur.nodes && cur.nodes.length ? nodeChips(cur.nodes) : null),
        cur.description ? h('p.mut', { style: 'max-width:80ch' }, cur.description) : null,
        cur.model_file ? h('span.mono.mut.wrap-any', { title: 'Model file' }, cur.model_file) : null,
        h('div', { style: 'display:flex;flex-direction:column;gap:6px' },
          h('span.txt2', { style: 'font-weight:700' }, total ? `Phase ${ph.index + 1} of ${total}: ` : 'Phase: ', h('span', { style: 'color:var(--amber-text)' }, (ph.label && ph.label !== ph.type ? ph.label : phaseTitle(ph.type)))),
          total ? bar(Array.from({ length: total }, (_, i) => ({ cls: i < ph.index ? 'm' : i === ph.index ? 'a' : 'o', value: 1 })), total, `Phase ${ph.index + 1} of ${total}`, 'sm') : null),
        progress({
          fraction: pctOf(cur.percent) / 100, cls: 'a', label: `${Math.round(pctOf(cur.percent))} percent of this benchmark is done`,
          left: h('span', h('span.pct', `${Math.round(pctOf(cur.percent))}%`), h('span.mut', { style: 'font-weight:600' }, ' of this benchmark')),
          mid: d.text ? h('span.mut', { style: 'font-weight:600' }, d.text) : null,
        })),
      inFlight(st, run));
  }

  const nextNode = h('div.cs', { key: 'next' },
    h('span.mut', { style: 'font-size:12px;font-weight:700;letter-spacing:.06em;text-transform:uppercase' }, 'Next'),
    next ? h('div', { style: 'margin-top:6px' }, h('b', next.name), h('span.mono.mut', '  ' + (next.profile || '')),
      next.description ? h('p.mut', { style: 'margin-top:2px' }, next.description) : null)
      : h('p.mut', { style: 'margin-top:6px' }, 'This is the last benchmark.'));

  const queued = (state.bench.state && state.bench.state.queue) || [];
  return h('section', { class: 'card ' + (run.state === 'paused' ? 'flag-warn' : ''), key: 'live-panel', 'aria-label': 'Benchmark run in progress' },
    h('div.ch',
      h('h2', run.suite_name || run.suite_id), run.state === 'paused' ? badge('Waiting for idle', 'settle') : badge('Running', 'load'),
      queued.length ? h('span.note', `${plural(queued.length, 'more run')} queued after this one`) : null,
      h('span.right', button('Cancel run', () => cancelRun(run), 'dng sm'))),
    why ? h('div.cb', banner('warn', ['Waiting for the scheduler to be idle: ', reasonNode(why)])) : null,
    h('div.cb', overall),
    h('div.cols', { style: 'gap:0' },
      h('div.wide', { style: 'gap:0;flex-basis:520px' }, curNode, nextNode),
      h('div.side', { style: 'gap:0;max-width:420px;border-left:1px solid var(--line)' },
        h('div.cs', { style: 'border-top:0;padding-bottom:8px' }, h('span.mut', { style: 'font-size:12px;font-weight:700;letter-spacing:.06em;text-transform:uppercase' }, 'Queue')),
        queueList(run))));
}
