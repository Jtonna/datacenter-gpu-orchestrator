// The benchmark pages (the carried-over views/benchmarks/*) in the server's
// shapes (internal/api/bench*.go), from the templates in bench-tpl.mjs (a
// real run's answers). --bench <state>:
//   long   one suite and one benchmark with a 300-character description;
//          25 runs of the suite, 24 within an hour and the newest 14 hours
//          later (the duration chart's x scale); every run with a long note
//          and a long scheduler version; the run at index 8 (oldest first)
//          failed, its step with a 200-character reason. The benchmark's
//          results name a long suite and a host with five workers.

import { TPL } from './bench-tpl.mjs';

export const BENCH_STATES = ['long'];

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

const MIN = 60e3;

function runAt(i, now) {
  // 24 runs within an hour, 2.5 min apart, ending 16 h ago; the newest 2 h ago
  return i < 24 ? now - 16 * 3600e3 - (23 - i) * 2.5 * MIN : now - 2 * 3600e3;
}
const runId = i => `run-20261124T07${String(10 + i).padStart(2, '0')}00-${(0xa0c000 + i).toString(16)}`;
const durS = i => (i === FAILED_AT ? 4 : 70 + ((i * 37) % 120));

function summaryOf(i) {
  const failed = i === FAILED_AT ? 1 : 0;
  return { failed, passed: 1 - failed, ran: 1, refused_by_exclusion: 0, regressions: 0, steps: 1 };
}
function runDoc(i, now) {
  const at = runAt(i, now);
  return { env: Object.assign({}, TPL.env, { scheduler_version: LONG.version, config_revision: '708' }), finished_at: at + durS(i) * 1000, id: runId(i), kind: 'suite', note: LONG.note,
    pinned: false, queued_at: at - 1000, started_at: at, state: 'completed', suite_id: SUITE, suite_name: suiteDoc(now).name, summary: summaryOf(i), trigger: 'manual', version: LONG.version,
    baseline_run_id: i ? runId(i - 1) : undefined };
}
function suiteDoc(now) {
  const s = Object.assign({}, TPL.suite, { id: SUITE, name: 'Load times on the consoles, cold and warm', description: LONG.suiteDesc, benchmarks: [BENCH] });
  delete s.last_run;
  return s;
}
const benchDoc = () => Object.assign({}, TPL.bench, { id: BENCH, name: 'Qwen3 4B, 2 consoles', description: LONG.benchDesc });

function stepDoc(i, now) {
  const at = runAt(i, now), s = JSON.parse(JSON.stringify(TPL.step));
  Object.assign(s, { benchmark_id: BENCH, name: benchDoc().name, started_at: at, finished_at: at + durS(i) * 1000 });
  if (i === FAILED_AT) {
    Object.assign(s, { outcome: 'failed', reason: LONG.reason, expect: { pass: false, checks: [{ name: 'outcome', pass: false, detail: 'wanted ready, got failed (child exited during load: exit code 1)' }] } });
    s.phases = (s.phases || []).slice(0, 1).map(p => Object.assign(p, { state: 'failed', outcome: 'failed' }));
    delete s.stats;
  }
  return s;
}

export function benchRoute(state, p, q, now = Date.now()) {
  if (!state) return null;
  const runs = Array.from({ length: 25 }, (_, i) => runDoc(i, now));
  const newest = runs.slice().reverse();
  if (p === '/api/bench/state') return { current: null, enabled: true, next_scheduled: [], queue: [], run: null };
  if (p === '/api/bench/suites') return { suites: [Object.assign(suiteDoc(now), { last_run: newest[0] })] };
  if (p === '/api/bench/suites/' + SUITE) return Object.assign(suiteDoc(now), { last_run: newest[0] });
  if (p === '/api/bench/benchmarks') return { benchmarks: [benchDoc()] };
  if (p === '/api/bench/benchmarks/' + BENCH) return benchDoc();
  if (p === '/api/bench/benchmarks/' + BENCH + '/results') {
    return { benchmark: BENCH, results: runs.map((r, i) => Object.assign({}, TPL.result, { at: r.started_at, run_id: r.id, suite_id: SUITE, suite_name: r.suite_name,
      host: 'console-01', workers: ['console-02', 'console-03', 'console-04', 'console-05', 'console-06'], result: i === FAILED_AT ? 'fail' : 'pass', outcome: i === FAILED_AT ? 'failed' : 'ready',
      load_s: 30 + (i % 7), load_mb_s: 410 + i, decode_tps: 27.8, memory_mib: 7800, lowest_available_mib: 9100, profile: BENCH })) };
  }
  if (p === '/api/bench/runs') return { runs: newest };
  const m = /^\/api\/bench\/runs\/([^/]+)$/.exec(p);
  if (m) {
    const i = runs.findIndex(r => r.id === m[1]);
    return i < 0 ? undefined : Object.assign(runDoc(i, now), { suite: suiteDoc(now), steps: [stepDoc(i, now)] });
  }
  if (p === '/api/bench/aggregates') return { benchmarks: [] };
  if (p === '/api/bench/profiles') return TPL.profiles;
  if (p === '/api/bench/schema') return TPL.schema;
  if (p === '/api/bench/external') return { external: [] };
  return null;
}
export const benchRunId = runId;
