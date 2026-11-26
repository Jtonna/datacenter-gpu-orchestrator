// The benchmark pages (views/benchmarks.js, views/bench/*) in the server's
// shapes (internal/api/bench*.go, benchmodel.go; the slice 2 run facts), from
// the templates in bench-tpl.mjs (a real run's answers). --bench <state>:
//   long    one group and one benchmark with a 300-character description;
//           25 runs of the group, 24 within an hour and the newest 14 hours
//           later; every run with a long note and a long scheduler version;
//           the run at index 8 (oldest first) failed, its step with a
//           200-character reason that the step's error and its load's reason
//           repeat (the report prints it once).
//   boards  the boards' content (Bench-Overview, Bench, Bench-Group,
//           Bench-Runs, Bench-Runs-Group, Bench-Report): four groups (one of
//           two groups, one benchmark over 12 models and 2 placements), five
//           benchmarks, 49 runs served 12 at a time (offset, limit, total;
//           kind= and result= filter), the newest one running.
//   runcancel  the boards plus Run and Cancel (board bench-run/run-cancel):
//           the board's plain benchmarks and groups, a run on step 2 of 5
//           with two queued behind it, a cancelled run; the POSTs of a run
//           (its dry read and its start) and of a cancel answer (rcPost).
//   runwait  runcancel with the run waiting until idle.

import { TPL } from './bench-tpl.mjs';

export const BENCH_STATES = ['long', 'boards', 'runcancel', 'runwait'];

export const LONG = {
  suiteDesc: 'Measures load times and answer speed on the consoles with a cold page cache and again with a warm one, so the two can be compared run by run. Every step drops the caches first; the note of each run says which file was dropped from the share and on which consoles the drop was done before it.',
  benchDesc: 'Qwen3 4B on two consoles with equal shares, the file read from the share. Context 8192, temperature 0, seed 42. Cold load means the page caches were dropped on the consoles and the file dropped from the cache of the scheduler first; then a warm-up and six requests.',
  note: 'Programme blocks 1 and 2, cold: page caches dropped on all six consoles at 07:36:20 UTC and the model file dropped from the scheduler cache just before; the share was idle, no other load was running, and the consoles had been up for at least an hour before the first step of this run.',
  version: 'v0.1.8-rc.3+cand.v67.163b4fd.dirty.built-2026-11-25T06:55:04Z.go1.25.5.linux-amd64.sqlite-3.50.4.mcp-2025-06-18.llama-b6178',
  reason: 'child exited during load: exit code 1 (llama-server: failed to allocate the compute buffer of 1,204 MiB on the RPC worker console-03: out of memory; the other workers had loaded their shares; nothing was served)',
};
export const SUITE = 'cold-warm-consoles';
export const BENCH = 'qwen4b-2con';
export const FAILED_AT = 8;

const MIN = 60e3, HOUR = 3600e3;
const clone = o => JSON.parse(JSON.stringify(o));
const counts = (o = {}) => Object.assign({ steps: 1, done: 1, to_go: 0, completed: 0, passed: 0, with_issues: 0, failed: 0, refused: 0, cancelled: 0, skipped: 0 }, o);

// ---------- long ----------

function runAt(i, now) {
  // 24 runs within an hour, 2.5 min apart, ending 16 h ago; the newest 2 h ago
  return i < 24 ? now - 16 * HOUR - (23 - i) * 2.5 * MIN : now - 2 * HOUR;
}
const runId = i => `run-20261124T07${String(10 + i).padStart(2, '0')}00-${(0xa0c000 + i).toString(16)}`;
const durS = i => (i === FAILED_AT ? 4 : 70 + ((i * 37) % 120));

function summaryOf(i) {
  const failed = i === FAILED_AT ? 1 : 0;
  return { failed, passed: 1 - failed, ran: 1, refused_by_exclusion: 0, regressions: 0, steps: 1 };
}
function runDoc(i, now) {
  const at = runAt(i, now), failed = i === FAILED_AT;
  return { env: Object.assign({}, TPL.env, { scheduler_version: LONG.version, config_revision: '708' }), finished_at: at + durS(i) * 1000, id: runId(i), kind: 'suite', note: LONG.note,
    pinned: i === 3, queued_at: at - 1000, started_at: at, state: 'completed', suite_id: SUITE, suite_name: suiteDoc().name, summary: summaryOf(i), trigger: 'manual', version: LONG.version,
    baseline_run_id: i ? runId(i - 1) : undefined, number: i + 1, result: failed ? 'with_issues' : 'passed',
    counts: counts(failed ? { failed: 1 } : { completed: 1, passed: 1 }), cold: { loads: 1, cold: 1, verified: failed ? 0 : 1, state: failed ? 'some' : 'all' } };
}
function suiteDoc() {
  const s = Object.assign({}, TPL.suite, { id: SUITE, name: 'Load times on the consoles, cold and warm, every consoles pair, programme blocks 1 and 2', description: LONG.suiteDesc,
    benchmarks: [{ benchmark: BENCH, inputs: { placement: 'any:2', ctx: 8192 } }], expanded_count: 1 });
  delete s.last_run;
  return s;
}
const benchDoc = () => Object.assign({}, TPL.bench, { id: BENCH, name: 'Qwen3 4B, 2 consoles, cold page cache then warm, six requests each', description: LONG.benchDesc });

function stepDoc(i) {
  const at = runAt(i, Date.now()), s = clone(TPL.step);
  Object.assign(s, { benchmark_id: BENCH, name: benchDoc().name, started_at: at, finished_at: at + durS(i) * 1000, slug: 'qwen4b-2con-any-2', node_count: 2 });
  if (i === FAILED_AT) {
    // the same sentence three times: the step's reason, its error and its load's reason
    Object.assign(s, { outcome: 'failed', reason: LONG.reason, error: LONG.reason, expect: { pass: false, checks: [{ name: 'outcome', pass: false, detail: 'wanted ready, got failed (child exited during load: exit code 1)' }] } });
    s.load = Object.assign({}, s.load, { outcome: 'failed', reason: LONG.reason });
    s.phases = (s.phases || []).slice(0, 1).map(p => Object.assign(p, { state: 'failed', outcome: 'failed', error: LONG.reason }));
    delete s.stats;
  }
  return s;
}

function longRoute(p, q, now) {
  const runs = Array.from({ length: 25 }, (_, i) => runDoc(i, now));
  const newest = runs.slice().reverse();
  const group = () => Object.assign(suiteDoc(), { last_run: newest[0] });
  if (p === '/api/bench/state') return { current: null, enabled: true, next_scheduled: [], queue: [], run: null };
  if (p === '/api/bench/suites' || p === '/api/bench/groups') return { [p.endsWith('groups') ? 'groups' : 'suites']: [group()] };
  if (p === '/api/bench/suites/' + SUITE || p === '/api/bench/groups/' + SUITE) return group();
  if (p === '/api/bench/groups/' + SUITE + '/expand') return { id: SUITE, count: 1, entries: [{ benchmark_id: BENCH, name: benchDoc().name, profile: 'alpha' }] };
  if (p === '/api/bench/benchmarks') return { benchmarks: [Object.assign(benchDoc(), { last_use: { run_id: newest[0].id, run_number: 25, step: 0, at: newest[0].started_at, outcome: 'ready', duration_s: 150 } })] };
  if (p === '/api/bench/benchmarks/' + BENCH) return benchDoc();
  if (p === '/api/bench/runs') return paged(newest, q);
  const m = /^\/api\/bench\/runs\/([^/]+)$/.exec(p);
  if (m) {
    const i = runs.findIndex(r => r.id === m[1] || String(r.number) === m[1]);
    return i < 0 ? undefined : Object.assign(runDoc(i, now), { suite: suiteDoc(), steps: [stepDoc(i)] });
  }
  if (/^\/api\/bench\/runs\/[^/]+\/report$/.test(p)) return null;
  if (p === '/api/bench/aggregates') return { benchmarks: [] };
  if (p === '/api/bench/profiles') return TPL.profiles;
  if (p === '/api/bench/schema') return TPL.schema;
  return null;
}

// ---------- boards ----------

const MODELS = ['qwen3.5-4b-q4_k_m.gguf', 'gemma-4-12b-q4_k_m.gguf', 'deepseek-v2-lite-q4_k_m.gguf', 'devstral-24b-q4_k_m.gguf', 'qwen3-coder-30b-a3b-q4_k_m.gguf',
  'glm-4-32b-q4_k_m.gguf', 'kimi-dev-72b-iq2_xs.gguf', 'qwen2.5-72b-instruct-q2_k.gguf', 'mixtral-8x7b-q4_k_m.gguf', 'phi-4-14b-q4_k_m.gguf', 'llama-3.3-8b-q4_k_m.gguf', 'mistral-small-24b-q4_k_m.gguf'];
const ph = (type, o = {}) => Object.assign({ type }, o);
const BENCHES = [
  { id: 'cold-response-repeat', name: 'Cold load, response and repeat', phases: [ph('load', { cold: true }), ph('warmup', { count: 1 }), ph('requests', { requests: [{ name: 'a', prompt: 'x' }, { name: 'b', prompt: 'y' }], repeats: 3 }),
    ph('unload'), ph('load'), ph('requests', { requests: [{ name: 'a', prompt: 'x' }, { name: 'b', prompt: 'y' }], repeats: 3 }), ph('unload')], recipe: true, inputs: { ctx: 8192 } },
  { id: 'cold-load', name: 'Cold load', phases: [ph('load', { cold: true }), ph('unload')], recipe: true, inputs: { ctx: 8192 } },
  { id: 'fill-context', name: 'Fill the context', phases: [ph('load'), ph('conversation', { fill_pct: 90 }), ph('unload')], recipe: true, inputs: { ctx: 8192 } },
  { id: 'cold-first-token', name: 'Cold load and first token', phases: [ph('load', { cold: true }), ph('warmup', { count: 1 }), ph('unload')], recipe: true, inputs: { ctx: 8192 } },
  { id: 'eight-at-once', name: 'Eight at once', phases: [ph('load'), ph('concurrent_requests', { n: 8 }), ph('unload')], recipe: true, inputs: { ctx: 8192 } },
  { id: 'coding-tasks', name: 'Coding tasks', phases: [ph('load'), ph('tasks'), ph('unload')], recipe: true, inputs: { ctx: 8192 } },
];
const GROUPS = [
  { id: 'full-pass', name: 'Full pass', benchmarks: [{ group: 'all-load' }, { group: 'all-coding' }], expanded_count: 96, stop_on_failure: false, only_when_idle: true },
  { id: 'all-load', name: 'All models: load and response', description: 'Cold load, response and repeat for every model, on the smallest placement that fits and on all six zones.',
    benchmarks: [{ benchmark: 'cold-response-repeat', matrix: { model: MODELS, placement: ['auto', 'all'], ctx: [8192] } }], expanded_count: 24, stop_on_failure: false, only_when_idle: true },
  { id: 'all-coding', name: 'All models: coding', benchmarks: [{ benchmark: 'coding-tasks', matrix: { model: MODELS } }], expanded_count: 12, task_count: 72, only_when_idle: true },
  { id: 'gemma-everything', name: 'Gemma 4 12B: everything', benchmarks: ['cold-load', 'cold-response-repeat', 'fill-context', 'eight-at-once'].map(b => ({ benchmark: b, inputs: { model: MODELS[1], placement: 'any:2' } })),
    expanded_count: 4, only_when_idle: false, stop_on_failure: true },
  { id: 'planned-long-context', name: 'Long context on every model', benchmarks: [{ benchmark: 'fill-context', matrix: { model: MODELS.slice(0, 4) } }], expanded_count: 4,
    status: 'planned', reason: 'waits for the 128k profiles of the four smallest models' },
  // 24 entries, one benchmark each (live's longest group has 24): the panel scrolls inside
  { id: 'every-model', name: 'Every model, one at a time', benchmarks: MODELS.flatMap(m => [{ benchmark: 'cold-load', inputs: { model: m, placement: 'any:1' } },
    { benchmark: 'cold-first-token', inputs: { model: m, placement: 'all' } }]), expanded_count: 24, only_when_idle: true },
];

// 49 runs, newest first, the newest running
function boardRuns(now) {
  const out = [];
  const add = (o) => out.push(Object.assign({ trigger: 'manual', pinned: false, version: '0.1.9', kind: 'suite' }, o));
  const T = (h, m) => now - h * HOUR - m * MIN;
  add({ id: 'run-49', number: 49, suite_id: 'full-pass', suite_name: 'Full pass', state: 'running', result: 'running', queued_at: T(0, 42), started_at: T(0, 41),
    counts: counts({ steps: 96, done: 9, to_go: 87, completed: 9, passed: 9 }), cold: { loads: 9, cold: 9, verified: 9, state: 'all' } });
  add({ id: 'run-48', number: 48, suite_id: 'gemma-everything', suite_name: 'Gemma 4 12B: everything', state: 'completed', result: 'passed', queued_at: T(4, 55), started_at: T(4, 54), finished_at: T(4, 40),
    counts: counts({ steps: 4, done: 4, completed: 4, passed: 4 }), cold: { loads: 4, cold: 1, verified: 1, state: 'some' } });
  add({ id: 'run-47', number: 47, kind: 'benchmark', benchmark_id: 'cold-response-repeat', suite_id: '', suite_name: 'Cold load, response and repeat', state: 'completed', result: 'passed',
    queued_at: T(5, 34), started_at: T(5, 33), finished_at: T(5, 33) + 232e3, counts: counts({ completed: 1, passed: 1 }), cold: { loads: 2, cold: 1, verified: 1, state: 'some' },
    inputs: { recipe: 'cold-response-repeat', model: MODELS[7], placement: 'all', ctx: 8192, node_count: 6 } });
  add({ id: 'run-46', number: 46, suite_id: 'all-load', suite_name: 'All models: load and response', state: 'completed', result: 'with_issues', queued_at: T(8, 14), started_at: T(8, 13), finished_at: T(8, 13) + 124 * MIN,
    counts: counts({ steps: 24, done: 24, completed: 22, passed: 22, failed: 1, refused: 1 }), cold: { loads: 24, cold: 24, verified: 24, state: 'all' }, pinned: true });
  add({ id: 'run-45', number: 45, suite_id: 'all-coding', suite_name: 'All models: coding', state: 'completed', result: 'passed', queued_at: T(19, 5), started_at: T(19, 4), finished_at: T(19, 4) + 401 * MIN,
    counts: counts({ steps: 12, done: 12, completed: 12, passed: 12 }), cold: { loads: 12, cold: 0, verified: 0, state: 'none' } });
  add({ id: 'run-44', number: 44, kind: 'benchmark', benchmark_id: 'fill-context', suite_id: '', suite_name: 'Fill the context', state: 'completed', result: 'passed', queued_at: T(27, 13), started_at: T(27, 12),
    finished_at: T(27, 12) + 1450e3, counts: counts({ completed: 1, passed: 1 }), inputs: { recipe: 'fill-context', model: MODELS[1], placement: 'any:2', ctx: 131072, node_count: 2 } });
  add({ id: 'run-43', number: 43, kind: 'benchmark', benchmark_id: 'cold-load', suite_id: '', suite_name: 'Cold load', state: 'completed', result: 'failed', queued_at: T(27, 36), started_at: T(27, 35),
    finished_at: T(27, 35) + 2.2e3, counts: counts({ failed: 1 }), inputs: { recipe: 'cold-load', model: MODELS[8], placement: 'any:4', node_count: 4 } });
  add({ id: 'run-42', number: 42, kind: 'benchmark', benchmark_id: 'cold-load', suite_id: '', suite_name: 'Cold load', state: 'completed', result: 'refused', queued_at: T(27, 38), started_at: T(27, 37),
    finished_at: T(27, 37) + 0.4e3, counts: counts({ refused: 1 }), inputs: { recipe: 'cold-load', model: MODELS[3], placement: 'any:1', node_count: 1 } });
  add({ id: 'run-41', number: 41, suite_id: 'all-load', suite_name: 'All models: load and response', state: 'cancelled', result: 'cancelled', queued_at: T(80, 0), started_at: T(80, 0), finished_at: T(80, 0) + 58 * MIN,
    counts: counts({ steps: 24, done: 12, completed: 11, passed: 11, cancelled: 1 }), cold: { loads: 12, cold: 12, verified: 12, state: 'all' } });
  for (let n = 40; n >= 1; n--) {
    const at = T(90 + (40 - n) * 7, 0);
    add({ id: 'run-' + n, number: n, suite_id: n % 2 ? 'all-load' : 'all-coding', suite_name: n % 2 ? 'All models: load and response' : 'All models: coding', state: 'completed', result: n % 5 ? 'passed' : 'with_issues',
      queued_at: at, started_at: at, finished_at: at + (120 + n) * MIN, counts: counts(n % 2 ? { steps: 24, done: 24, completed: 24, passed: n % 5 ? 24 : 23, failed: n % 5 ? 0 : 1 } : { steps: 12, done: 12, completed: 12, passed: 12 }) });
  }
  return out;
}

const loadPh = (idx, s, o = {}) => Object.assign({ idx, type: 'load', state: 'done', outcome: 'ok', duration_s: s, load: { outcome: 'ready', load_s: s, memory_mib: 32643, load_mode: 'file_read' } }, o);
const sum = (mean, median, min, max) => ({ n: 6, mean, median, min, max, stdev: 0.1 });
const reqPh = (idx, o = {}) => Object.assign({ idx, type: 'requests', state: 'done', outcome: 'ok', duration_s: 75,
  stats: { requests: 6, failed: 0, decode_tps: sum(8.18, 8.2, 8.1, 8.21), prefill_tps: sum(36.9, 36.4, 33.6, 40.9), ttft_ms: sum(1510, 1300, 1220, 2340), wall_ms: sum(25200, 25100, 12000, 38800),
    output_hashes: { a: 'h1', b: 'h1', c: 'h1' } } }, o);
function reportStep(idx, model, nodes, o = {}) {
  const b = BENCHES[0];
  const gen = `${b.name} (${model}, 8k, ${nodes === 6 ? 'all' : 'auto'})`;
  return Object.assign({ idx, slug: `cold-response-repeat-${model.replace(/\.gguf$/, '')}-${nodes === 6 ? 'all' : 'auto'}`, benchmark_id: 'cold-response-repeat', name: gen, profile: 'bench-' + model.replace(/\.gguf$/, ''),
    model_file: model, node_count: nodes, state: 'done', outcome: 'ready', started_at: 1795400000000, finished_at: 1795400232000,
    spec: { id: 'cold-response-repeat.' + (idx + 100).toString(16), name: gen, recipe: 'cold-response-repeat', phases: b.phases, applied_inputs: { model, placement: nodes === 6 ? 'all' : 'auto', ctx: 8192 } },
    load: { outcome: 'ready', load_s: 26.8 + idx, memory_mib: 32643 }, stats: { requests: 6, failed: 0, decode_tps: sum(96.2 - idx * 4, 96, 95, 97) },
    phases: [loadPh(0, 60.5, { cold: { requested: true, verified: true, nodes: [] } }), Object.assign({ idx: 1, type: 'warmup', state: 'done', outcome: 'ok', duration_s: 3, stats: { requests: 1, failed: 0, ttft_ms: sum(2340, 2340, 2340, 2340) } }),
      reqPh(2), { idx: 3, type: 'unload', state: 'done', outcome: 'ok', duration_s: 2, unload: { was_loaded: true, gone: true, unload_s: 2 } },
      loadPh(4, 5.8, { load: { outcome: 'ready', load_s: 5.8, memory_mib: 32643, load_mode: 'cached' } }), reqPh(5, { stats: Object.assign(reqPh(5).stats, { decode_tps: sum(8.2, 8.2, 8.1, 8.3) }) }),
      { idx: 6, type: 'unload', state: 'done', outcome: 'ok', duration_s: 2, unload: { was_loaded: true, gone: true, unload_s: 2 } }] }, o);
}
function boardRunDetail(r) {
  const run = clone(r);
  if (r.id === 'run-46' || r.id === 'run-41') {
    const steps = [];
    let i = 0;
    for (const m of MODELS) for (const nodes of [0, 6]) {
      const n = nodes || 1 + (i % 4);
      steps.push(reportStep(i, m, n));
      i++;
    }
    steps[16] = reportStep(16, MODELS[8], 4, { outcome: 'failed', reason: 'child exited during load: exit code 1, after 2.2 s', error: 'child exited during load: exit code 1, after 2.2 s',
      phases: [loadPh(0, 2.2, { outcome: 'failed', error: 'child exited during load: exit code 1, after 2.2 s', load: { outcome: 'failed', load_s: 2.2, reason: 'child exited during load: exit code 1' } })] });
    // a refusal: the scheduler's long sentence and the server's short note (reason_short)
    steps[6] = reportStep(6, MODELS[3], 1, { outcome: 'refused', reason: 'profile bench-devstral-24b-q4_k_m does not fit ra-01 by the memory reservation rule: 15,400 MiB needed, 13,100 MiB free; nothing of ours is loaded there', reason_short: 'Does not fit on 1 zone', phases: [] });
    // a context fill step: its phase type has a word on the page, never the raw id
    steps[2].phases.push({ idx: 7, type: 'context_fill', state: 'done', outcome: 'ok', duration_s: 41, fill_pct: 90 });
    run.steps = r.id === 'run-41' ? steps.slice(0, 12) : steps;
  } else if (r.kind === 'benchmark') {
    run.steps = [reportStep(0, r.inputs.model, r.inputs.node_count, r.result === 'failed' ? { outcome: 'failed', reason: 'child exited during load: exit code 1, after 2.2 s', phases: [loadPh(0, 2.2, { outcome: 'failed' })] }
      : r.result === 'refused' ? { outcome: 'refused', reason: 'profile bench-devstral-24b-q4_k_m does not fit ra-01 by the memory reservation rule: 15,400 MiB needed, 13,100 MiB free; nothing of ours is loaded there', reason_short: 'Does not fit on 1 zone', phases: [] } : {})];
  } else run.steps = [reportStep(0, MODELS[1], 2)];
  return run;
}

function paged(all, q) {
  let list = all;
  const kind = q && q.get('kind'), result = q && q.get('result'), suite = q && q.get('suite'), model = q && q.get('model');
  if (kind) list = list.filter(r => r.kind === kind);
  if (suite) list = list.filter(r => r.suite_id === suite);
  if (model) list = list.filter(r => r.inputs && r.inputs.model === model);
  if (result === 'has_failed') list = list.filter(r => r.counts && r.counts.failed);
  else if (result === 'has_refused') list = list.filter(r => r.counts && r.counts.refused);
  else if (result === 'clean') list = list.filter(r => r.result === 'passed');
  else if (result) list = list.filter(r => r.result === result);
  const limit = Math.min(500, parseInt((q && q.get('limit')) || '50', 10) || 50), offset = parseInt((q && q.get('offset')) || '0', 10) || 0;
  return { runs: list.slice(offset, offset + limit), total: list.length, offset };
}

// the results per model (GET /api/bench/results/models): the board's
// Overview figures at the smallest fit and on all 6 zones; mixtral failed at
// its load; phi measured no first token; llama ran on 6 zones only
function boardResults(now, q) {
  const rows = [
    // model, fit, cold, reload, tok/s, ttft ms, mem, cold all 6, tok/s all 6
    [MODELS[0], 1, 26.8, 2.0, 96.2, 410, 3100, 11.3, 92.2], [MODELS[1], 2, 35.4, 3.0, 27.8, 800, 9100, 21.1, 24.3],
    [MODELS[2], 2, 48.4, 3.3, 80.5, 620, 11200, 24.8, 58.9], [MODELS[3], 2, 63.7, 23.8, 31.4, 950, 15400, 30.1, 28.7],
    [MODELS[4], 3, 61.0, 4.3, 59.3, 880, 19800, 43.7, 50.1], [MODELS[5], 3, 61.2, 10.0, 22.6, 1100, 21000, 41.4, 20.2],
    [MODELS[6], 4, 68.2, 24.1, 9.1, 1420, 27100, 56.9, 8.8], [MODELS[7], 4, 72.2, 40.4, 8.2, 1510, 29800, 60.5, 8.18],
  ];
  let step = 0;
  const cell = (file, n, cold, reload, tps, ttft, mem, at) => {
    const c = { run_id: 'run-46', step: step++, benchmark_id: 'cold-response-repeat', node_count: n, ctx: 8192, at, outcome: 'ready', cold_load_s: cold, cold: true, cold_verified: true,
      memory_mib: mem, lowest_available_mib: 900 + n * 10 };
    if (reload != null) c.reload_s = reload;
    if (tps != null) c.decode_tps = tps;
    if (ttft != null) c.ttft_ms = ttft;
    return c;
  };
  const at = now - 6 * HOUR;
  const out = rows.map(([file, fit, cold, reload, tps, ttft, mem, cold6, tps6]) => {
    const small = cell(file, fit, cold, reload, tps, ttft, mem, at), six = cell(file, 6, cold6, reload, tps6, ttft, mem, at);
    return { model: file, smallest_fit: fit, latest: six, by_nodes: [small, six] };
  });
  const failed = { run_id: 'run-46', step: 16, benchmark_id: 'cold-response-repeat', node_count: 4, ctx: 8192, at, outcome: 'failed', reason: 'child exited during load: exit code 1, after 2.2 s' };
  out.push({ model: MODELS[8], smallest_fit: 0, latest: failed, by_nodes: [failed] });
  const phi = cell(MODELS[9], 2, 30.2, 3.1, 40.0, null, 8800, at);
  const llama = cell(MODELS[10], 6, 15.5, null, 70.1, 350, 5200, at);
  out.push({ model: MODELS[10], smallest_fit: 6, latest: llama, by_nodes: [llama] });
  // a file no longer in the models folder: kept, marked, listed last
  // its only step failed at the load (no figure): the row says the file is
  // gone; its cells are dashes, never "failed" (the reviewer, 26 Nov)
  const old = { run_id: 'run-30', step: 3, benchmark_id: 'cold-response-repeat', node_count: 4, ctx: 8192, at: at - 30 * 24 * HOUR, outcome: 'failed', reason: 'child exited during load' };
  out.push({ model: 'mixtral-8x7b-instruct-v0.1.Q4_K_M.gguf', smallest_fit: 0, latest: old, by_nodes: [old], file_gone: true });
  // phi's file is in Deleted models (the trash fixture's a1b2c3d4e5f6): marked, linked there, last
  out.push({ model: MODELS[9], smallest_fit: 2, latest: phi, by_nodes: [phi], file_gone: true, trash_id: 'a1b2c3d4e5f6' });
  // every figure names its step (from); gemma-4-12b's newest step at its fit
  // was a context fill (run 47) that measured only tok/s: the other columns
  // come from run 46 and the row says "several runs"
  for (const m of out) for (const c of [m.latest, ...m.by_nodes]) {
    c.from = {};
    for (const k of ['cold_load_s', 'reload_s', 'decode_tps', 'ttft_ms', 'memory_mib', 'lowest_available_mib']) {
      if (c[k] != null) c.from[k] = { run_id: c.run_id, run_number: Number(String(c.run_id).replace('run-', '')) || 0, step: c.step, at: c.at, node_count: c.node_count };
    }
  }
  const q72 = out[1].by_nodes[0];
  q72.decode_tps = 29.4; q72.from.decode_tps = { run_id: 'run-47', run_number: 47, step: 0, at: at + 2 * HOUR, node_count: 2 }; q72.mixed = true;
  // the step's machine of the lowest available figure (the hover)
  for (const m of out) for (const c of m.by_nodes) if (c.lowest_available_mib != null) c.lowest_available_node = 'zone-' + c.node_count;
  // ?run=<number>: one run's results (here: run 47 holds only gemma's
  // context fill); ?ctx=: the cells at that context (all are 8,192 here)
  const run = q && q.get('run'), ctx = q && q.get('ctx');
  let models = out;
  if (run) models = run === '47' ? [{ model: MODELS[1], smallest_fit: 2, latest: Object.assign({}, out[1].by_nodes[0], { run_id: 'run-47', step: 0, cold_load_s: null, reload_s: null, ttft_ms: null, memory_mib: null, lowest_available_mib: null, mixed: false, from: { decode_tps: out[1].by_nodes[0].from.decode_tps } }), by_nodes: [] }] : run === '46' ? out : [];
  if (ctx && ctx !== '8192') models = [];
  return { ctx: ctx ? Number(ctx) : 0, run: run ? 'run-' + run : '', contexts: [4096, 8192], models };
}

function boardsRoute(p, q, now, rc = '') {
  const runs = rc ? rcRuns(now, rc === 'runwait') : boardRuns(now);
  const lastOf = id => runs.find(r => r.suite_id === id && !['running', 'queued', 'paused'].includes(r.state)) || null;
  const groups = (rc ? RC_GROUPS.concat(GROUPS) : GROUPS).map(g => Object.assign({ source: 'api', created_at: now - 40 * 24 * HOUR, updated_at: now - 3 * 24 * HOUR }, clone(g), lastOf(g.id) ? { last_run: lastOf(g.id) } : {}));
  const lastUse = { 'cold-response-repeat': { run_id: 'run-47', run_number: 47, step: 0, at: runs[2].started_at, outcome: 'ready', duration_s: 232 },
    'cold-load': { run_id: 'run-43', run_number: 43, step: 0, at: runs[6].started_at, outcome: 'failed', duration_s: 2.2 },
    'fill-context': { run_id: 'run-44', run_number: 44, step: 0, at: runs[5].started_at, outcome: 'ready', duration_s: 1450 } };
  const benches = (rc ? RC_BENCHES.concat(BENCHES) : BENCHES).map(b => Object.assign({ source: 'api', created_at: now - 40 * 24 * HOUR, updated_at: now - 3 * 24 * HOUR }, clone(b), lastUse[b.id] ? { last_use: lastUse[b.id] } : {}));
  const running = runs[0];
  const progress = { id: running.id, suite_id: running.suite_id, suite_name: running.suite_name, state: 'running', percent: 9.4, started_at: running.started_at, elapsed_s: 41 * 60, eta_s: 6 * 3600,
    steps_total: 96, steps_done: 9, counts: running.counts, next: null, queue: [],
    current: { index: 9, benchmark_id: 'cold-response-repeat', name: 'Cold load, response and repeat', profile: 'bench-devstral', model_file: MODELS[3], nodes: ['console-01', 'console-02', 'console-03', 'console-04', 'console-05', 'console-06'],
      percent: 40, placement: 'all', phase: { index: 2, type: 'requests', label: 'Requests', phases_total: 7 } } };
  if (p === '/api/bench/state') return rc ? { current: null, enabled: true, next_scheduled: [], queue: runs.filter(r => r.state === 'queued').reverse().map(brief), run: rcProgress(now, rc === 'runwait') }
    : { current: null, enabled: true, next_scheduled: [], queue: [], run: progress };
  if (p === '/api/bench/groups' || p === '/api/bench/suites') return { [p.endsWith('groups') ? 'groups' : 'suites']: groups };
  let m = /^\/api\/bench\/(?:groups|suites)\/([^/]+)(\/expand)?$/.exec(p);
  if (m) {
    const g = groups.find(x => x.id === m[1]);
    if (!g) return undefined;
    if (!m[2]) return g;
    return { id: g.id, count: g.expanded_count, entries: Array.from({ length: g.expanded_count }, (_, i) => ({ benchmark_id: 'cold-response-repeat~' + i, name: 'Cold load, response and repeat', profile: 'bench-' + i })) };
  }
  if (p === '/api/bench/benchmarks') return { benchmarks: benches };
  if (p === '/api/bench/results/models') return boardResults(now, q);
  if (p === '/api/bench/runs') return paged(runs, q);
  m = /^\/api\/bench\/runs\/([^/]+)$/.exec(p);
  if (m) {
    const r = runs.find(x => x.id === m[1] || String(x.number) === m[1]);
    if (r && rc && r.number >= 51) return rcRunDetail(r, now);
    return r ? boardRunDetail(r) : undefined;
  }
  m = /^\/api\/bench\/runs\/([^/]+)\/report$/.exec(p);
  if (m) {
    const step = parseInt((q && q.get('step')) || '0', 10);
    return { format: 'json', kind: 'benchmark', run: { id: m[1], kind: 'suite', name: 'x', state: 'completed', queued_at: 0 }, issues: [],
      benchmark: { step, benchmark_id: 'cold-response-repeat', name: 'Cold load, response and repeat', profile: 'p', outcome: 'ready', duration_s: 232, issues: [], step_figures: [],
        sections: [1, 2, 5].map(idx => ({ idx, type: idx === 1 ? 'warmup' : 'requests', title: 'x', state: 'done', duration_s: 1, figures: [], issues: [], transcript: `/api/bench/runs/${m[1]}/steps/${step}/transcript?phase=${idx}` })) } };
  }
  return null;
}

export function benchRoute(state, p, q, now = Date.now()) {
  if (!state) return null;
  if (state === 'runcancel' || state === 'runwait') return boardsRoute(p, q, now, state);
  return state === 'boards' ? boardsRoute(p, q, now) : longRoute(p, q, now);
}

// the runs of the runcancel states: run 52's first step passed (the 1-console
// load), its second is loading; the cancelled run 51's one step cancelled
function rcRunDetail(r, now) {
  const run = clone(r);
  const step = (idx, n, o) => Object.assign({ idx, slug: `qwen4b-${n}con`, benchmark_id: `qwen4b-${n}con`, name: RC_BENCHES[n - 1].name, profile: rcProfile(n), model_file: QWEN, node_count: n,
    spec: { id: `qwen4b-${n}con`, name: RC_BENCHES[n - 1].name, phases: RC_BENCHES[n - 1].phases } }, o);
  if (r.id === 'run-52' && r.state === 'running') {
    run.steps = [step(0, 1, { state: 'done', outcome: 'ready', started_at: now - 2 * MIN, finished_at: now - 65e3, load: { outcome: 'ready', load_s: 8.4, memory_mib: 3400 },
      stats: { requests: 3, failed: 0, decode_tps: { n: 3, mean: 48.2, median: 48, min: 47, max: 49 } }, phases: [] }),
    step(1, 2, { state: 'running', outcome: '', started_at: now - 18e3, phases: [] })];
  } else if (r.id === 'run-51') {
    run.steps = [step(0, 2, { state: 'done', outcome: 'cancelled', error: 'cancelled while the model was loading', started_at: r.started_at, finished_at: r.finished_at, phases: [] })];
  } else run.steps = [];
  return run;
}

// ---------- --bench runcancel: Run and Cancel (board bench-run/run-cancel) ----------
//
// The boards' answers, plus the board's plain benchmarks and groups: "Qwen 4B
// load and requests" on 1 to 5 consoles, the group of the five (S3a), a
// reference group the setting bench_not_startable names (S1), a smoke test.
// Run 52 (S3a) is on step 2 of 5, loading on ps5-01 and ps5-02; runs 53 and
// 54 are queued behind it; run 51 was cancelled. POST .../run answers the dry
// read (?dry_run=1) and the start; POST /api/bench/runs/<id>/cancel the cancel.
// "runwait": the same, run 52 waiting until idle.

const mac2 = n => '02:00:00:00:00:' + String(n).padStart(2, '0');
const CONSOLES = ['ps5-01', 'ps5-02', 'ps5-03', 'ps5-04', 'ps5-05'];
const QWEN = 'qwen3.5-4b-q4_k_m.gguf';
const rcProfile = n => (n === 1 ? 'qwen3.5-4b:16k' : `test-s3-qwen4b-${n}con`);
const rcShown = n => (n === 1 ? 'qwen3.5-4b:16k' : `TEST - s3-qwen4b-${n}con`);
const RC_BENCHES = [1, 2, 3, 4, 5].map(n => ({ id: `qwen4b-${n}con`, name: n === 1 ? 'Qwen 4B load and requests, 1 console' : `Qwen 4B, ${n} consoles`, profile: rcProfile(n),
  phases: [ph('load'), ph('warmup', { count: 1 }), ph('requests', { requests: [{ name: 'a', prompt: 'x' }], repeats: 3 }), ph('unload')] }))
  .concat([{ id: 'host-gpu-alone', name: 'Qwen 4B, host GPU alone', profile: 'test-s1-qwen4b-host', phases: [ph('load'), ph('requests', { requests: [{ name: 'a', prompt: 'x' }] }), ph('unload')] }]);
const RC_GROUPS = [
  { id: 's3a', name: 'S3a console workers only, 1 to 5 consoles, Qwen 4B', benchmarks: [1, 2, 3, 4, 5].map(n => `qwen4b-${n}con`), expanded_count: 5, cooldown_s: 10,
    only_when_idle: true, evict_others: true, stop_on_failure: false },
  { id: 's1-ref', name: 'S1 host GPU alone (reference)', benchmarks: ['host-gpu-alone'], expanded_count: 1, only_when_idle: true, not_startable: 'listed in bench_not_startable (Settings)' },
  { id: 'smoke', name: 'Smoke test', benchmarks: ['qwen4b-1con', 'qwen4b-2con', 'qwen4b-3con'], expanded_count: 3, only_when_idle: true },
];

function rcRuns(now, waiting) {
  const T = m => now - m * MIN;
  const base = boardRuns(now).map(r => (r.id === 'run-49' ? Object.assign({}, r, { state: 'completed', result: 'passed', finished_at: T(3),
    counts: counts({ steps: 96, done: 96, completed: 96, passed: 96 }) }) : r));
  const mk = o => Object.assign({ trigger: 'manual', pinned: false, version: '0.1.9', kind: 'suite' }, o);
  return [
    mk({ id: 'run-54', number: 54, kind: 'benchmark', benchmark_id: 'qwen4b-1con', suite_id: '', suite_name: 'Qwen 4B load and requests, 1 console', state: 'queued', result: 'queued', queued_at: T(0.5), started_at: 0 }),
    mk({ id: 'run-53', number: 53, suite_id: 'smoke', suite_name: 'Smoke test', state: 'queued', result: 'queued', queued_at: T(1), started_at: 0 }),
    mk({ id: 'run-52', number: 52, suite_id: 's3a', suite_name: 'S3a console workers only, 1 to 5 consoles, Qwen 4B', state: waiting ? 'paused' : 'running', result: waiting ? 'waiting' : 'running',
      queued_at: T(2.2), started_at: T(2.1), note: 'Re-run of S3a after the rename', started_by: 'anonymous', start_comment: 'Re-run of S3a after the rename',
      counts: counts(waiting ? { steps: 5, done: 0, to_go: 5 } : { steps: 5, done: 1, to_go: 4, completed: 1, passed: 1 }) }),
    mk({ id: 'run-51', number: 51, kind: 'benchmark', benchmark_id: 'qwen4b-2con', suite_id: '', suite_name: 'Qwen 4B, 2 consoles', state: 'cancelled', result: 'cancelled',
      queued_at: T(30), started_at: T(30), finished_at: T(30) + 21e3, error: 'cancelled at step 1 of 1 by anonymous: "wrong zones"', started_by: 'anonymous', counts: counts({ cancelled: 1 }) }),
  ].concat(base.filter(r => r.number < 51));
}
function rcProgress(now, waiting) {
  const r = { id: 'run-52', suite_id: 's3a', suite_name: 'S3a console workers only, 1 to 5 consoles, Qwen 4B', started_at: now - 2.1 * MIN, elapsed_s: 124, eta_s: null,
    steps_total: 5, steps_done: waiting ? 0 : 1, next: null, queue: [] };
  if (waiting) return Object.assign(r, { state: 'paused', percent: 0, counts: counts({ steps: 5, done: 0, to_go: 5 }), current: null,
    paused_because: 'gemma-4-12b:64k is ready (not loaded by this run)' });
  return Object.assign(r, { state: 'running', percent: 23, counts: counts({ steps: 5, done: 1, to_go: 4, completed: 1, passed: 1 }),
    current: { index: 1, benchmark_id: 'qwen4b-2con', name: 'Qwen 4B, 2 consoles', profile: rcProfile(2), profile_name: rcShown(2), model_file: QWEN, nodes: ['ps5-01', 'ps5-02'], percent: 40, placement: '',
      phase: { index: 0, type: 'load', label: 'Load', phases_total: 4 } } });
}
const brief = r => ({ id: r.id, suite_id: r.suite_id, suite_name: r.suite_name, state: r.state, trigger: 'manual', queued_at: r.queued_at, started_at: 0, finished_at: 0, kind: r.kind,
  benchmark_id: r.benchmark_id || undefined });

const GEMMA = { profile: 'gemma-4-12b:64k', profile_name: 'gemma-4-12b:64k', machines: ['ps5-01'], state: 'ready', in_flight: 0, idle_s: 180, by: 'chat-ui' };
function rcLoad(n, fits) {
  const ms = CONSOLES.slice(0, n).map((name, i) => ({ id: mac2(i + 1), name, takes_mib: Math.round(3400 / n) }));
  return { profile: rcProfile(n), profile_name: rcShown(n), model: QWEN, model_name: 'Qwen 3.5 4B', steps: 1, machines: ms, need_mib: 3400, need_known: true,
    action: fits ? 'start' : 'queue', reason: fits ? '' : 'gemma-4-12b:64k is loaded on ps5-01', fits: fits ? 'fits' : 'waits' };
}
const flag = (v, d) => (v === undefined || v === null ? d : !!v);
// rcPlan(kind, id, body): the dry read of a start
export function rcPlan(kind, id, body) {
  body = body || {};
  const queue = { queued: 0, max: 20 };
  const plan = { dry_run: true, startable: true, kind: kind === 'group' ? 'suite' : 'benchmark', id, refusals: [], warnings: [], loaded_now: [], would_unload: [], queue };
  if (kind === 'group') {
    const g = RC_GROUPS.find(x => x.id === id) || GROUPS.find(x => x.id === id);
    if (!g) return null;
    const wait = flag(body.only_when_idle, g.only_when_idle !== false), evict = flag(body.evict_others, !!g.evict_others);
    Object.assign(plan, { name: g.name, waits_for_idle: wait, evict_others: evict, max_run_minutes: 60 });
    if (g.not_startable) {
      plan.startable = false;
      plan.refusals = [{ code: 'not_startable', entry: g.id, message: `the group ${g.id} is listed in bench_not_startable (Settings)` }];
      plan.step_count = g.expanded_count; plan.steps = []; plan.loads = [];
      return plan;
    }
    const ns = id === 's3a' ? [1, 2, 3, 4, 5] : [1, 2, 3];
    plan.step_count = ns.length;
    plan.steps = ns.map(n => ({ benchmark_id: `qwen4b-${n}con`, name: RC_BENCHES[n - 1].name, profile: rcProfile(n), profile_name: rcShown(n) }));
    plan.loads = ns.map(n => rcLoad(n, false));
    plan.loaded_now = [GEMMA];
    if (!wait || evict) plan.would_unload = [Object.assign({}, GEMMA, { why: 'idle on ps5-01, a machine this run loads on' })];
    queue.running = 'run-52';
    queue.queued = 2;
    if (body.again_of) plan.warnings = [{ code: 'definition_changed', message: `changed since run ${body.again_of}: Qwen 4B, 2 consoles. This run uses the definitions as they are now` }];
    return plan;
  }
  const b = RC_BENCHES.find(x => x.id === id);
  if (!b) return null;
  const n = parseInt((/(\d)con$/.exec(id) || [0, 1])[1], 10);
  Object.assign(plan, { name: b.name, waits_for_idle: flag(body.only_when_idle, true), evict_others: flag(body.evict_others, false), max_run_minutes: 12, step_count: 1,
    steps: [{ benchmark_id: id, name: b.name, profile: b.profile, profile_name: id === 'host-gpu-alone' ? 'TEST - s1-qwen4b-host' : rcShown(n) }] });
  if (id === 'host-gpu-alone') {
    plan.startable = false; plan.loads = [];
    plan.refusals = [{ code: 'scheduler_gpu', entry: b.name, machine: 'scheduler', message: `${b.name} would load test-s1-qwen4b-host on scheduler, the scheduler's own machine; benchmarks never load there, so nothing of the run was started` }];
    return plan;
  }
  plan.loads = [rcLoad(n, true)];
  return plan;
}
// rcPost(method, path, body): {status, body} for the POSTs of the runcancel states, or null
export function rcPost(p, q, body) {
  body = body || {};
  let m = /^\/api\/bench\/(groups|suites|benchmarks)\/([^/]+)\/run$/.exec(p);
  if (m) {
    const kind = m[1] === 'benchmarks' ? 'benchmark' : 'group';
    const plan = rcPlan(kind, decodeURIComponent(m[2]), body);
    if (!plan) return { status: 404, body: { error: { message: 'not found' } } };
    const dry = q && ['1', 'true', 'yes'].includes(q.get('dry_run') || '');
    if (dry) return { status: 200, body: plan };
    if (!String(body.comment || '').trim()) return { status: 400, body: { error: { message: 'a comment is required (body {"comment": "..."} or the X-Comment header)', reason: 'comment_required' } } };
    if (plan.refusals.length) return { status: 409, body: { error: { message: plan.refusals[0].message, reason: plan.refusals[0].code }, refusals: plan.refusals } };
    const want = plan.would_unload.map(x => x.profile).sort().join(','), got = (body.evict || []).slice().sort().join(',');
    if (want !== got) return { status: 409, body: { error: { message: 'what this run would unload changed since it was shown: nothing was started; look at the new list and start again', reason: 'evict_set_changed' },
      refusals: [{ code: 'evict_set_changed', message: 'what this run would unload changed since it was shown: nothing was started; look at the new list and start again' }], would_unload: plan.would_unload } };
    return { status: 202, body: { id: 'run-55', number: 55, state: 'queued', suite_id: kind === 'group' ? plan.id : '', suite_name: plan.name, kind: plan.kind } };
  }
  m = /^\/api\/bench\/runs\/([^/]+)\/cancel$/.exec(p);
  if (m) {
    if (!String(body.comment || '').trim()) return { status: 400, body: { error: { message: 'a comment is required (body {"comment": "..."} or the X-Comment header)', reason: 'comment_required' } } };
    return { status: 202, body: { id: decodeURIComponent(m[1]), state: 'cancelled' } };
  }
  return null;
}

export const benchRunId = runId;
