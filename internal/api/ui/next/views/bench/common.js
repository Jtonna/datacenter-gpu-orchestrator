// The Benchmarks pages' shared data and words (boards Bench-Overview, Bench,
// Bench-Group, Bench-Runs, Bench-Runs-Group, Bench-Runs-Load, Bench-Report).
//
// Reads: GET /api/bench/state (seed; the "bench" topic keeps it), /groups,
// /benchmarks, /runs (offset, limit, total, result, kind, model, suite),
// /runs/<id or number>, /runs/<id>/report, /groups/<id>, /groups/<id>/expand.
// Model names come from the catalog's copy (needCatalog({live: false}): names
// only; the Benchmarks pages show no plans, so they never hold the live
// models topic).
//
// The boards' words: "group" (the API's suite), "run" for a whole run AND for
// each expanded entry ("24 runs", "10 of 96"), "step" for a phase, "zones" for
// nodes. Nothing is renamed in the API.

import { h } from '../../core/dom.js';
import * as api from '../../core/api.js';
import { state, emit } from '../../core/store.js';
import { catalog } from '../../core/catalog.js';
import { runIcon } from '../../core/ui.js';
import { num, dur, dayLabel, clock, dateTime, plural, DASH } from '../../core/format.js';

// ---------- the cache, read once per page and on bench messages ----------

export const bench = { groups: null, benchmarks: null, err: '', off: '' };
const subs = new Set();
export const onBench = fn => { subs.add(fn); return () => subs.delete(fn); };
const tell = () => { for (const fn of Array.from(subs)) { try { fn(); } catch (e) { console.error(e); } } };
const busy = {};
function pull(key, fn) {
  if (busy[key]) return busy[key];
  busy[key] = fn().then(() => { bench.err = ''; }, e => {
    if (e && e.status === 503) bench.off = e.message; else bench.err = (e && e.message) || String(e);
  }).finally(() => { busy[key] = null; tell(); });
  return busy[key];
}
export const loadGroups = () => pull('groups', async () => { const d = await api.get('/api/bench/groups'); bench.groups = d.groups || d.suites || []; });
export const loadBenchmarks = () => pull('benchmarks', async () => { const d = await api.get('/api/bench/benchmarks'); bench.benchmarks = d.benchmarks || []; });
// The menu's counts (Benchmarks N, Runs N): groups and benchmarks, and the
// runs' total (B1), kept in state.benchCounts for the shell.
export const loadRunTotal = () => pull('total', async () => {
  const d = await api.get('/api/bench/runs', { limit: 1 });
  if (d.total != null) state.benchCounts = Object.assign({}, state.benchCounts, { runs: d.total });
});
const countLists = () => {
  if (bench.groups && bench.benchmarks) state.benchCounts = Object.assign({}, state.benchCounts, { benchmarks: bench.groups.length + bench.benchmarks.length });
};
export const loadLists = () => Promise.all([loadGroups(), loadBenchmarks(), loadRunTotal()]).then(countLists);
export async function seedState() {
  try {
    const doc = await api.get('/api/bench/state');
    state.bench.state = doc;
    if (doc.run && !state.bench.run) state.bench.run = doc.run;
    emit('bench');
  } catch (e) { if (e.status === 503) { bench.off = e.message; tell(); } }
}
export const groupById = id => (bench.groups || []).find(g => g.id === id) || null;
export const benchmarkById = id => (bench.benchmarks || []).find(b => b.id === id) || null;
export const groupTitle = g => (g && (g.name || g.id)) || '';
export const benchTitle = b => (b && (b.name || b.id)) || '';

// recipeTitle(step): the title of the benchmark a step ran. A step of a
// recipe carries the runner's generated name, "<title> (<model>, 8k,
// nodes:...)" (benchspec.Use): the page says the model, the placement and
// the context in its own words, so the bracket is never printed.
export function recipeTitle(st) {
  const sp = (st && st.spec) || {};
  const own = sp.recipe ? benchTitle(benchmarkById(sp.recipe)) : '';
  if (own) return own;
  const n = sp.name || (st && st.name) || '';
  const i = n.lastIndexOf(' (');
  if ((sp.recipe || sp.applied_inputs) && i > 0 && n.endsWith(')')) return n.slice(0, i);
  return n || (st && st.benchmark_id) || sp.recipe || '';
}

// The run in progress (the progress document of the "bench" topic), or null.
export function activeRun() {
  const run = state.bench.run;
  return run && !['completed', 'failed', 'cancelled', 'skipped'].includes(run.state) ? run : null;
}

// ---------- words ----------

// The zones the cluster has now (for "all 6 zones").
export const zoneCount = () => ((state.status && state.status.view && state.status.view.nodes) || []).length;

// placementWords('all') -> "all 6 zones"; 'any:2' -> "any 2 zones"; 'auto' ->
// "placed by the scheduler"; 'nodes:a,b' -> "on a, b"; 'smallest' -> "the
// smallest that fits".
export function placementWords(p) {
  if (p == null || p === '') return '';
  const s = typeof p === 'string' ? p : String(p);
  if (s === 'all') { const n = zoneCount(); return n ? `all ${n} zones` : 'all zones'; }
  if (s === 'auto') return 'placed by the scheduler';
  if (s === 'smallest') return 'the smallest that fits';
  const any = /^any:(\d+)$/.exec(s);
  if (any) return `any ${any[1]} ${any[1] === '1' ? 'zone' : 'zones'}`;
  if (s.startsWith('nodes:')) return 'on ' + s.slice(6).split(',').join(', ');
  return s;
}

// modelName('qwen2.5-72b-instruct-q2_k.gguf') -> the catalog's tidy name, else the file.
export function modelName(file) {
  if (!file) return '';
  const m = catalog.byKey[file];
  return (m && m.name) || String(file).replace(/\.gguf$/i, '');
}

export const zonesWord = n => (n == null ? '' : n === 1 ? '1 zone' : `${num(n)} zones`);

// when(t): "Today 08:01", "Yesterday 21:10", "14 Nov 09:12" (the boards).
export function when(t) {
  if (!t) return DASH;
  const d = dayLabel(t);
  const short = /^\d/.test(d) ? d.replace(/ \d{4}$/, '') : d;
  return h('time', { datetime: new Date(t).toISOString(), title: dateTime(t) }, `${short} ${clock(t, false)}`);
}

// took(run): the run's length, or "41 min" so far while it runs.
export function tookS(r) {
  if (!r || !r.started_at) return null;
  const end = r.finished_at || (['running', 'paused'].includes(r.state) ? Date.now() : 0);
  return end ? Math.max(0, (end - r.started_at) / 1000) : null;
}

// ---------- results ----------

// The icon of a run's result (B1 `result`): Passed, Failed, Refused,
// Running, Queued. A cancelled run or step is a neutral grey dash, not the
// red cross (the owner's walkthrough, build-92: nothing failed); failed and
// timed out keep the red cross.
const RESULT_ICON = { passed: 'done', with_issues: 'done', failed: 'failed', cancelled: 'stopped', refused: 'refused', running: 'running',
  queued: 'queued', waiting: 'queued', skipped: 'cancelled', not_judged: 'cancelled' };
const RESULT_WORD = { passed: 'Passed', with_issues: 'Passed with issues', failed: 'Failed', cancelled: 'Cancelled', refused: 'Refused', running: 'Running',
  queued: 'Queued', waiting: 'Waiting until the zones are idle', skipped: 'Skipped',
  // refused because the profile forces an excluded zone: neither a pass nor a failure (0.1.7 review)
  not_judged: 'Not judged' };
export const resultOf = r => (r && (r.result || (r.state === 'completed' ? 'passed' : r.state === 'paused' ? 'waiting' : r.state))) || '';
// the boards' own marks: a passed run is a green tick, a refused one an
// amber circle with a bar (the Requests screens keep their icons)
const BOARD_ICON = {
  done: () => [h('path', { d: 'M3.5 8.5l3 3 6-7', fill: 'none', stroke: 'var(--up)', 'stroke-width': 2, 'stroke-linecap': 'round', 'stroke-linejoin': 'round' })],
  refused: () => [h('circle', { cx: 8, cy: 8, r: 6.2, fill: 'none', stroke: 'var(--queued)', 'stroke-width': 1.8 }), h('path', { d: 'M5 8h6', stroke: 'var(--queued)', 'stroke-width': 1.8, 'stroke-linecap': 'round' })],
  stopped: () => [h('path', { d: 'M4 8h8', stroke: 'var(--cancelled)', 'stroke-width': 2.2, 'stroke-linecap': 'round' })],
};
export function resultIcon(r, size = 16) {
  const res = resultOf(r);
  const kind = RESULT_ICON[res] || 'queued';
  const icon = BOARD_ICON[kind] ? h('svg', { viewBox: '0 0 16 16', width: size, height: size, role: 'img', 'aria-label': RESULT_WORD[res] || res }, BOARD_ICON[kind]()) : runIcon(kind, size, true);
  return h('span.bp-ri', { title: RESULT_WORD[res] || res, 'data-result': res }, icon);
}
export const resultWord = r => RESULT_WORD[resultOf(r)] || resultOf(r);
// outcomeResult(outcome, state) -> the result of one step or one use of a
// benchmark (B1 step `outcome`) for resultIcon: cancelled and skipped stay
// themselves (grey, nothing failed); any other outcome that is not a pass or
// a refusal is a failure (failed, error, a time-out)
export const outcomeResult = (o, state) => (o === 'ready' || o === 'ok' || o === 'completed' ? 'passed' : o === 'refused_by_exclusion' ? 'not_judged'
  : o === 'refused' ? 'refused' : o === 'cancelled' ? 'cancelled' : o === 'skipped' ? 'skipped' : o === 'running' ? 'running'
    : o ? 'failed' : state === 'running' ? 'running' : 'queued');
// resultTone(result) -> the class of a result's dot or word: ok (passed),
// bad (failed and timed out only), ref (refused), notrun (cancelled,
// skipped: the neutral grey), '' (running, not judged, queued)
const RESULT_TONE = { passed: 'ok', with_issues: 'ok', failed: 'bad', refused: 'ref', cancelled: 'notrun', skipped: 'notrun' };
export const resultTone = res => RESULT_TONE[res] || '';

// dots(counts, running): "22 · 1 · 1" passed, failed, refused with their
// hovers; "9 passed · 87 to go" while a run runs (B1 counts).
export function dots(c, running) {
  if (!c) return h('span.mut', { title: 'This scheduler does not count the results of a run (older than benchmarks slice 2).' }, DASH);
  const passed = (c.passed || 0) + (c.with_issues || 0);
  // red is what failed; cancelled and skipped steps are a neutral "not run"
  const failed = c.failed || 0;
  const notRun = (c.cancelled || 0) + (c.skipped || 0);
  const out = [];
  if (running) {
    out.push(h('span.bp-dot.ok', { title: plural(passed, 'run') + ' passed' }, `${num(passed)} passed`));
    if (failed) out.push(h('span.bp-dot.bad', { title: plural(failed, 'run') + ' failed' }, num(failed)));
    if (c.refused) out.push(h('span.bp-dot.ref', { title: plural(c.refused, 'run') + ' refused' }, num(c.refused)));
    if (notRun) out.push(h('span.bp-dot.notrun', { title: `${num(notRun)} not run` }, num(notRun)));
    out.push(h('span.mut', { title: plural(c.to_go || 0, 'run') + ' to go' }, `${num(c.to_go || 0)} to go`));
  } else {
    out.push(h('span.bp-dot.ok', { title: `${num(passed)} passed` }, num(passed)));
    if (failed) out.push(h('span.bp-dot.bad', { title: `${num(failed)} failed` }, num(failed)));
    if (c.refused) out.push(h('span.bp-dot.ref', { title: `${num(c.refused)} refused` }, num(c.refused)));
    if (notRun) out.push(h('span.bp-dot.notrun', { title: `${num(notRun)} not run` }, num(notRun)));
  }
  return h('span.bp-dots', out);
}

// coldNote(run): "cold: caches dropped on all six zones" (B1 cold).
export function coldNote(r) {
  const c = r && r.cold;
  if (!c || !c.loads) return null;
  if (c.state === 'all') return `cold: caches dropped and verified on every load (${plural(c.loads, 'load')})`;
  if (c.state === 'some') return `cold on ${num(c.verified)} of ${plural(c.loads, 'load')}`;
  return null;
}

// inputsLine(run): "Qwen 2.5 72B Q2 · all 6 zones · 131,072" for a direct run.
export function inputsLine(r) {
  const i = r && r.inputs;
  if (!i) return null;
  const parts = [];
  if (i.model) parts.push(modelName(i.model));
  if (i.placement) parts.push(placementWords(i.placement));
  else if (i.node_count) parts.push(zonesWord(i.node_count));
  if (i.ctx) parts.push(num(i.ctx));
  return parts.length ? parts.join(' · ') : null;
}

// ---------- groups and benchmarks ----------

// phaseWord(phase): the step chain's words ("Load cold", "Warm-up",
// "Requests", "Unload", "Reload", "Conversation to 90%", "8 at once", "Tasks").
export function phaseWord(p, i, phases) {
  const t = p.type;
  if (t === 'load') {
    const after = (phases || []).slice(0, i).some(q => q.type === 'unload');
    if (p.label) return p.label;
    return after ? 'Reload' : p.cold ? 'Load cold' : 'Load';
  }
  if (p.label) return p.label;
  if (t === 'warmup') return 'Warm-up';
  if (t === 'requests') return 'Requests';
  if (t === 'unload') return 'Unload';
  if (t === 'hold') return 'Hold';
  if (t === 'tasks') return 'Tasks';
  if (t === 'concurrent_requests') return `${num(p.n || p.count || 0)} at once`;
  if (t === 'conversation') return p.fill_pct ? `Conversation to ${num(p.fill_pct)}%` : 'Conversation';
  if (t === 'context_fill') return 'Context fill';
  // a type this page does not know yet: its id in words, never raw ("some_type" -> "Some type")
  const w = String(t || 'step').replace(/_/g, ' ');
  return w.charAt(0).toUpperCase() + w.slice(1);
}
export function phasesOf(b) {
  if (!b) return [];
  if (Array.isArray(b.phases) && b.phases.length) return b.phases;
  // the short form: a load (cold or not), warm-up, requests, unload
  const out = [{ type: 'load', cold: !!b.cold }];
  if (b.warmup_requests) out.push({ type: 'warmup' });
  if (b.requests && b.requests.length) out.push({ type: 'requests' });
  if (b.unload_after !== false) out.push({ type: 'unload' });
  return out;
}
export const chain = b => { const ps = phasesOf(b); return ps.map((p, i) => phaseWord(p, i, ps)).join(' · '); };

// entryOf(item): a group's entry in one shape: {kind: 'benchmark'|'group'|'inline', ref, inputs, matrix, inline}
export function entryOf(it) {
  if (typeof it === 'string') return { kind: 'benchmark', ref: it };
  if (!it || typeof it !== 'object') return { kind: 'benchmark', ref: String(it) };
  if (it.group) return { kind: 'group', ref: it.group };
  if (it.benchmark) return { kind: 'benchmark', ref: it.benchmark, inputs: it.inputs || null, matrix: it.matrix || null };
  return { kind: 'inline', ref: it.id || '', inline: it };
}

// matrixWords(entry): ["12 models", "2 placements"] for a matrix entry.
export function matrixWords(e) {
  const m = e.matrix || {};
  const out = [];
  if (m.model && m.model.length) out.push(plural(m.model.length, 'model'));
  if (m.placement && m.placement.length) out.push(plural(m.placement.length, 'placement'));
  // one context is an option of the entry ("context 8,192"), not a dimension
  if (m.ctx && m.ctx.length > 1) out.push(plural(m.ctx.length, 'context'));
  if (m.thinking && m.thinking.length) out.push(m.thinking.length === 2 ? 'thinking on and off' : 'thinking ' + (m.thinking[0] ? 'on' : 'off'));
  return out;
}

// groupSub(group): the list's line under a group's name: "2 groups · 96
// runs", "1 benchmark × 12 models × 2 placements · 24 runs", "4 benchmarks ·
// any 2 zones" (Bench board).
export function groupSub(g) {
  const items = (g.benchmarks || []).map(entryOf);
  const groups = items.filter(e => e.kind === 'group').length;
  const benches = items.length - groups;
  const parts = [];
  if (groups && !benches) parts.push(plural(groups, 'group'));
  else if (benches === 1 && items.length === 1 && items[0].matrix) parts.push(['1 benchmark'].concat(matrixWords(items[0])).join(' × '));
  else if (items.length) parts.push(groups ? `${plural(benches, 'benchmark')} and ${plural(groups, 'group')}` : plural(benches, 'benchmark'));
  const places = new Set(items.filter(e => e.inputs && e.inputs.placement).map(e => String(e.inputs.placement)));
  if (places.size === 1 && !items.some(e => e.matrix && e.matrix.placement)) parts.push(placementWords([...places][0]));
  // a coding group says its tasks (board Bench: "1 benchmark × 12 models · 72 tasks")
  if (g.task_count) parts.push(plural(g.task_count, 'task'));
  else if (g.expanded_count != null && (groups || items.some(e => e.matrix) || g.expanded_count !== items.length)) parts.push(plural(g.expanded_count, 'run'));
  return parts.join(' · ');
}

// optionWords(group, entry): the board's options line: "Smallest that fits,
// and all 6 zones · context 8,192 · cold loads · carries on after a failure
// · waits until idle". Options the server has no field for (cold loads:
// B7) are drawn by optionChips with their reason.
export function optionWords(g, e) {
  const parts = [];
  const places = (e && e.matrix && e.matrix.placement) || (e && e.inputs && e.inputs.placement ? [e.inputs.placement] : []);
  if (places.length) {
    const ws = places.map(placementWords);
    parts.push(ws.length > 1 ? ws.slice(0, -1).join(', ') + ', and ' + ws[ws.length - 1] : ws[0]);
  }
  const ctxs = (e && e.matrix && e.matrix.ctx) || (e && e.inputs && e.inputs.ctx ? [e.inputs.ctx] : []);
  if (ctxs.length) parts.push('context ' + ctxs.map(c => num(c)).join(', '));
  if (g.stop_on_failure) parts.push('stops at the first failure'); else parts.push('carries on after a failure');
  if (g.only_when_idle === false) parts.push('runs at once, even while zones are busy'); else parts.push('waits until idle');
  if (g.evict_others) parts.push('may unload other models');
  if (g.cooldown_s) parts.push(`${dur(g.cooldown_s)} between runs`);
  return parts;
}

// ---------- the "not available yet" mark ----------

// notYet(text, why): the element as drawn, greyed, with the reason on hover
// and for a screen reader. Never dropped (the mockup is the requirement).
// data-notyet is the reason's key in WHY (b5, b7, edit, run, ...).
export function notYet(text, why, tag = 'span') {
  const key = Object.keys(WHY).find(k => WHY[k] === why) || 'other';
  return h(tag + '.bp-notyet', { title: 'Not available yet: ' + why, 'aria-disabled': 'true', 'data-notyet': key }, text);
}

// The reasons, one per missing backend package (p15 plan, B-numbers).
// Each names the thing that is missing and the slice of the benchmarks plan
// (p15) that builds it.
export const WHY = {
  coding: 'the Coding figures need task scores per model and a default scorer, which no route serves yet (benchmarks slice 4: B5 Coding, B15)',
  b6: 'a group cannot ask for "the smallest that fits" yet: the placements are auto, all, any N or named zones (benchmarks slice 7: B6)',
  b7: 'a group has no "cold loads" option yet; a benchmark asks for a cold load in its own load step (benchmarks slice 7: B7)',
  b10: 'comparing two runs is not built yet; it needs a server route first (benchmarks slice 5: B10)',
  b11: 'what stopped a run (a machine dropping out) is not read from the history yet (benchmarks slice 5: B11)',
  b13: 'one-click PDF is not served yet: open the full report and print it (benchmarks slice 5: B13)',
  recipe: 'a recipe asks for its inputs (model, placement, context) before it runs, and that dialog has no mockup yet (benchmarks slice 6 drew the run dialog without inputs)',
};
