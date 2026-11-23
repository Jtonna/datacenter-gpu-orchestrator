// Shared pieces of the Benchmarks screens: the data cache (REST, pulled on
// entry and again when a "bench" message says something changed), metric
// definitions, state badges and the plain-language description of a benchmark.

import { h } from '../../core/dom.js';
import * as api from '../../core/api.js';
import { badge, signedPct as sharedSignedPct, changeMark as sharedChangeMark } from '../../core/ui.js';
import { plainReason } from '../../core/status.js';
import { num, dur, plural, DASH } from '../../core/format.js';

// ---------- data cache ----------

export const cache = {
  benchmarks: null, suites: null, runs: null, schema: null, profiles: null, modelProfiles: null,
  agg: { all: null, same: null }, errors: {}, off: false,
};
const subs = new Set();
export const onData = fn => { subs.add(fn); return () => subs.delete(fn); };
const notify = () => { for (const fn of Array.from(subs)) { try { fn(); } catch (e) { console.error(e); } } };

async function pull(key, fn, set) {
  try { set(await fn()); cache.errors[key] = null; cache.off = false; } catch (e) {
    cache.errors[key] = e;
    if (e && e.status === 503) cache.off = e.message;
  }
  notify();
}
export const loadBenchmarks = () => pull('benchmarks', () => api.get('/api/bench/benchmarks'), r => { cache.benchmarks = r.benchmarks || []; });
export const loadSuites = () => pull('suites', () => api.get('/api/bench/suites'), r => { cache.suites = r.suites || []; });
export const loadRuns = suite => pull('runs', () => api.get('/api/bench/runs', { limit: 200, suite }), r => { cache.runs = r.runs || []; });
export const loadAggregates = same => pull('agg', () => api.get('/api/bench/aggregates', { env: same ? 'same' : undefined }), r => { cache.agg[same ? 'same' : 'all'] = r; });
export const loadSchema = () => (cache.schema ? Promise.resolve() : pull('schema', () => api.get('/api/bench/schema'), r => { cache.schema = r; }));
export const loadProfiles = () => (cache.profiles ? Promise.resolve() : pull('profiles', () => api.get('/api/bench/profiles'), r => { cache.profiles = r.profiles || []; }));
// The model file of each profile, to say what a benchmark loads. Optional.
export const loadModelProfiles = () => (cache.modelProfiles ? Promise.resolve() : pull('modelProfiles', () => api.get('/api/profiles'), r => {
  cache.modelProfiles = {};
  for (const p of r.profiles || []) cache.modelProfiles[p.id] = p;
}));
export const refreshLists = () => Promise.all([loadBenchmarks(), loadSuites()]);

export const benchmarkById = id => (cache.benchmarks || []).find(b => b.id === id) || null;
export const suiteById = id => (cache.suites || []).find(s => s.id === id) || null;
export const suiteTitle = s => s.name || s.id;

// A suite with a status of planned or blocked is a scenario that cannot run
// yet; it carries a reason and may list no benchmark.
const SUITE_STATUS = { planned: ['Planned', 'n'], blocked: ['Blocked', 'aside'] };
export const suiteRunnable = s => !s || !s.status;
export const suiteStatusBadge = s => {
  if (suiteRunnable(s)) return null;
  const [t, k] = SUITE_STATUS[s.status] || [s.status, 'n'];
  return badge(t, k, s.reason || null);
};
// Why the Run button of a suite is off, or null.
export const suiteNotRunnable = s => (suiteRunnable(s) ? null : `This suite is ${s.status} and cannot be run yet: ${s.reason || 'no reason given'}`);

// ---------- metrics ----------

export const METRICS = [
  { key: 'load_s', label: 'Load time', unit: 's', better: 'lower', digits: 1 },
  { key: 'load_mb_s', label: 'Upload speed', unit: 'MB/s', better: 'higher', digits: 0 },
  { key: 'decode_tps', label: 'Decode speed', unit: 'tok/s', better: 'higher', digits: 1 },
  { key: 'prefill_tps', label: 'Prefill speed', unit: 'tok/s', better: 'higher', digits: 1 },
  { key: 'ttft_ms', label: 'Time to first token', unit: 'ms', better: 'lower', digits: 0 },
  { key: 'memory_mib', label: 'Memory', unit: 'MiB', better: 'lower', digits: 0 },
  { key: 'lowest_available_mib', label: 'Lowest available memory (system)', unit: 'MiB', better: 'higher', digits: 0 },
  { key: 'lowest_available_gpu_mib', label: 'Lowest available memory (GPU)', unit: 'MiB', better: 'higher', digits: 0 },
  { key: 'unload_s', label: 'Unload time', unit: 's', better: 'lower', digits: 1 },
  { key: 'concurrent_wall_s', label: 'Simultaneous requests, wall time', unit: 's', better: 'lower', digits: 1 },
];
export const metricDef = key => METRICS.find(m => m.key === key) || { key, label: key, unit: '', better: 'lower', digits: 1 };
export const fmtMetric = (key, v) => (v == null || Number.isNaN(v) ? DASH : num(v, metricDef(key).digits));
export const withUnit = (key, v) => (v == null ? DASH : fmtMetric(key, v) + ' ' + metricDef(key).unit);

// A signed percent: "+12%" / "−3.4%".
export const signedPct = sharedSignedPct;

// A change next to a figure: coral when it is a regression, amber when it is
// worse, mint when better.
export const changeMark = (key, pct, regression) => sharedChangeMark(pct, { better: metricDef(key).better, regression });

// ---------- states ----------

const RUN_STATE = {
  queued: ['Queued', 'q', 'Waiting for the run in front of it to finish'],
  running: ['Running', 'load'],
  paused: ['Waiting for idle', 'settle', 'The scheduler is busy; the run starts when it is idle'],
  completed: ['Completed', 'use'],
  failed: ['Failed', 'fail'],
  cancelled: ['Cancelled', 'n'],
  skipped: ['Skipped', 'settle', 'The scheduler was never idle, so nothing ran'],
};
export const runBadge = st => { const [t, k, title] = RUN_STATE[st] || [st || 'Unknown', 'n']; return badge(t, k, title); };

const OUTCOME = {
  ready: ['Loaded and served', 'use'], refused: ['Refused', 'aside'], refused_by_exclusion: ['Refused: node excluded', 'aside'], failed: ['Failed', 'fail'], error: ['Runner error', 'fail'],
  cancelled: ['Cancelled', 'n'], skipped: ['Skipped', 'settle'],
};
export const outcomeBadge = o => { if (!o) return null; const [t, k] = OUTCOME[o] || [o, 'n']; return badge(t, k); };

export function passBadge(step) {
  if (!step || step.state === 'pending') return badge('Pending', 'n');
  if (step.state === 'running') return badge('Running', 'load');
  if (step.outcome === 'skipped' || step.state === 'skipped') return badge('Skipped', 'settle');
  if (step.outcome === 'cancelled') return badge('Cancelled', 'n');
  if (step.outcome === 'refused_by_exclusion') return badge('Refused, not judged', 'aside');
  if (step.expect) return step.expect.pass ? badge('Passed', 'ready') : badge('Failed', 'fail');
  return step.outcome === 'ready' ? badge('Passed', 'ready') : badge('Failed', 'fail');
}

export const pctRate = v => (v == null ? DASH : num(v * 100, 0) + '%');
export function rateBadge(v) {
  if (v == null) return h('span.mut', DASH);
  return badge(pctRate(v), v >= 1 ? 'use' : v >= 0.5 ? 'load' : 'aside', 'Share of runs whose expectations held');
}

// ---------- phases and descriptions ----------

export const PHASE_TITLE = {
  load: 'Load', warmup: 'Warm-up', requests: 'Requests', conversation: 'Conversation', hold: 'Hold',
  concurrent_requests: 'Simultaneous requests', unload: 'Unload',
};
export const phaseTitle = t => {
  const p = cache.schema && (cache.schema.phase_types || []).find(x => x.type === t);
  return (p && p.title) || PHASE_TITLE[t] || t;
};

// The phases a benchmark runs: its own list, or the simple form expanded.
export function phasesOf(bm) {
  if (!bm || bm.parallel) return [];
  if (bm.phases && bm.phases.length) return bm.phases;
  const out = [];
  if (bm.cold !== false) out.push({ type: 'load' });
  const w = bm.warmup_requests == null ? 1 : bm.warmup_requests;
  if (w > 0) out.push({ type: 'warmup', count: w });
  out.push({ type: 'requests', requests: bm.requests || [], repeats: bm.repeats || 1 });
  if (bm.unload_after !== false) out.push({ type: 'unload' });
  return out;
}

export function phaseShort(p) {
  switch (p.type) {
    case 'load': return 'Load';
    case 'warmup': return `Warm-up × ${p.count == null ? 1 : p.count}`;
    case 'requests': return plural((p.requests || []).length * (p.repeats || 1), 'request');
    case 'conversation': return `${plural((p.turns || []).length, 'turn')} chat`;
    case 'hold': return `Hold ${p.seconds == null ? 60 : p.seconds} s`;
    case 'concurrent_requests': return `${p.n || 2} at once`;
    case 'unload': return 'Unload';
  }
  return p.type;
}

export const phaseChips = bm => {
  if (bm && bm.parallel) return h('span.mut', `Starts ${bm.parallel.length} benchmarks together`);
  return h('span.nodes', phasesOf(bm).map((p, i) => h('span.node', { key: i, title: p.label || phaseTitle(p.type) }, phaseShort(p))));
};

// One sentence of what a benchmark does, from its definition.
export function describe(bm, modelFile) {
  if (!bm) return '';
  if (bm.parallel) return `Starts ${bm.parallel.length} benchmarks at the same moment: ${bm.parallel.join(', ')}.`;
  const ph = phasesOf(bm);
  if (!ph.length) return '';
  const what = modelFile || bm.profile || 'the profile';
  const parts = [];
  for (const p of ph) {
    let s = '';
    switch (p.type) {
      case 'load': s = `cold-loads ${what}`; break;
      case 'warmup': { const n = p.count == null ? 1 : p.count; s = n === 0 ? '' : plural(n, 'warm-up request'); break; }
      case 'requests': {
        const per = (p.requests || []).length, total = per * (p.repeats || 1);
        s = plural(total, 'measured request') + ((p.repeats || 1) > 1 && per ? ` (${plural(per, 'prompt')}, ${p.repeats} times each)` : '');
        break;
      }
      case 'conversation': s = `a ${(p.turns || []).length}-turn conversation${p.cache_prompt === false ? ' without prompt caching' : ''}`; break;
      case 'hold': s = `holds the model idle for ${dur(p.seconds == null ? 60 : p.seconds)}, then one request`; break;
      case 'concurrent_requests': s = `${p.n || 2} requests at the same moment`; break;
      case 'unload': s = 'unloads'; break;
    }
    if (s) parts.push(s);
  }
  if (!ph.some(p => p.type === 'load') && parts.length) parts[0] = `uses ${what} as it is (loads it first if needed): ` + parts[0];
  const text = parts.join(', ');
  return text.charAt(0).toUpperCase() + text.slice(1) + '.';
}

export function scheduleText(sc) {
  if (!sc) return 'Not scheduled';
  if (sc.at) return `Every day at ${sc.at}`;
  if (sc.every) return `Every ${sc.every.replace(/(\d+)h/, '$1 h ').replace(/(\d+)m/, '$1 min ').trim()}`;
  return 'Not scheduled';
}

export const modelOf = profileId => (cache.modelProfiles && cache.modelProfiles[profileId] && cache.modelProfiles[profileId].model) || '';

export const errorBanner = msg => h('div.banner.err', { role: 'alert' }, h('span.grow', msg));

// A scheduler reason in plain words; the raw text stays in the tooltip.
export const reasonNode = raw => h('span', { title: raw }, plainReason(raw) || raw);
