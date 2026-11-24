// The model catalog of the Models list (phase 7 UI) in the server's shape:
// GET /api/models = {disk, models, models_dir, history, version}
// (internal/api/models.go entries; internal/api/health.go withModelsDisk).
//
// States (--models <state>):
//   none   no model file, an empty models folder
//   five   five model files: one loaded (with a generated profile beside
//          two hand-written ones), one used 48 s ago, one 4 h ago, one
//          never used, one with no profiles; 50.3 of 240 GB used
//   full   the same files on a nearly full disk (95 %)
// File names and zones are generic fixture names.

const GB = 1e9;
const MIB = 2 ** 20;

function profile(id, ctx, needs, o = {}) {
  return Object.assign({ id, ctx, parallel: 1, aliases: [], force_nodes: [], tensor_split: [], generated: false, needs_mib: needs, needs_measured: true,
    needs_basis: 'measured on 2 zones', description: '', placement: 'auto', plan: { action: 'start', would_start: true, nodes: ['ps5-03'], reason: '' }, host: 'server', ttl_s: 600 }, o);
}

function entry(now, o) {
  const ps = o.profiles || [];
  const base = {
    key: o.file, kind: 'llama', file: o.file, exists: true, size_bytes: Math.round(o.gb * GB), name: o.name, short_name: o.name, raw_name: o.file.replace(/\.gguf$/, ''),
    publisher: o.publisher || '', family: o.family || '', brand: o.family || '', parameters: o.parameters || '', quant: o.quant || '', context_length: o.trained || 0,
    icon: null, state: 'not_loaded', instances: [], profiles: ps, profile_count: ps.length, generated_count: ps.filter(p => p.generated).length,
    startable_count: ps.filter(p => p.plan.would_start).length, last_loaded: null, last_active: null, requests_served: 0, history: true,
    added_at: o.added || Date.UTC(2026, 9, 20, 9, 30), bench: { benchmarks: 0, runs: 0, reports: 0 }, loads: { count: 0, failed: 0, recent: [] }, measured: [], fit: null,
  };
  return Object.assign(base, o.extra ? o.extra(now) : {});
}

export function modelsDoc(state, now = Date.now()) {
  const H = 3600e3;
  const files = state === 'none' ? [] : [
    { file: 'gemma-4-27b-it-Q4_K_M.gguf', gb: 16.5, added: Date.UTC(2026, 10, 4, 10, 0), name: 'gemma-4-27b', publisher: 'Google', family: 'Gemma', parameters: '27B', quant: 'Q4_K_M', trained: 131072,
      profiles: [profile('gemma-4-27b', 8192, 17480, { description: 'The default chat model.' }), profile('gemma-4-27b:long', 32768, 20890, { ttl_s: 0 }),
        profile('bench-gemma-4-27b-c16k-n2', 16384, 18620, { generated: true, needs_measured: false, needs_basis: 'estimated, +5% margin; the estimate is 17734 MiB' })],
      extra: n => ({ state: 'ready', last_loaded: n - 2 * H, last_active: n - 4e3, requests_served: 1840,
        instances: [{ profile: 'gemma-4-27b', state: 'ready', nodes: ['ps5-01', 'ps5-02'], memory_mib: 17480, in_flight: 2, waiters: 0, requests_served: 1840, last_used: n - 4e3, stale_profile: false }],
        bench: { benchmarks: 2, runs: 4, reports: 3 }, loads: { count: 9, failed: 1, recent: [] }, fit: { need_mib: 17480, basis: 'measured', ran_on_nodes: 2, min_nodes: 2 } }) },
    { file: 'qwen3-14b-Q5_K_M.gguf', gb: 10.1, name: 'qwen3-14b', publisher: 'Qwen', family: 'Qwen3', parameters: '14B', quant: 'Q5_K_M', trained: 40960,
      profiles: [profile('qwen3-14b', 16384, 11200, { force_nodes: ['ps5-04'], placement: 'forced', plan: { action: 'start', would_start: true, nodes: ['ps5-04'], reason: '' } })], extra: n => ({ last_active: n - 48e3, last_loaded: n - 5 * H, requests_served: 312, loads: { count: 3, failed: 0, recent: [] }, fit: { need_mib: 11200, basis: 'measured', ran_on_nodes: 1, min_nodes: 1 } }) },
    { file: 'gemma-4-12b-it-Q4_K_M.gguf', gb: 7.3, name: 'gemma-4-12b', publisher: 'Google', family: 'Gemma', parameters: '12B', quant: 'Q4_K_M', trained: 131072,
      profiles: [profile('gemma-4-12b', 8192, 8400), profile('gemma-4-12b:64k', 65536, 12100, { ttl_s: 1800,  needs_measured: false, needs_basis: 'estimated, +5% margin; the estimate is 11524 MiB' })],
      extra: n => ({ last_active: n - 4 * H, last_loaded: n - 6 * H, requests_served: 41, loads: { count: 2, failed: 0, recent: [] }, fit: { need_mib: 8400, basis: 'measured', ran_on_nodes: 1, min_nodes: 1 } }) },
    { file: 'qwen3-4b-Q8_0.gguf', gb: 4.3, name: 'qwen3-4b', publisher: 'Qwen', family: 'Qwen3', parameters: '4B', quant: 'Q8_0', trained: 40960,
      profiles: [profile('qwen3-4b', 8192, 5200, { needs_measured: false, needs_basis: 'estimated, +5% margin; the estimate is 4952 MiB' })],
      extra: () => ({ fit: { need_mib: 5200, basis: 'estimated', min_nodes: 1 } }) },
    { file: 'nomic-embed-text-v2.gguf', gb: 0.3, name: 'nomic-embed-text-v2', publisher: 'Nomic', family: 'Nomic BERT', parameters: '475M', quant: 'F16', trained: 8192, profiles: [] },
  ];
  const models = files.map(f => entry(now, f)).sort((a, b) => a.name.localeCompare(b.name));
  const modelBytes = models.reduce((s, m) => s + m.size_bytes, 0);
  const total = 240 * GB;
  const used = state === 'full' ? 228.4 * GB : state === 'none' ? 1.2 * GB : 50.3 * GB;
  const disk = { path: '/models', state: 'ok', total_bytes: total, free_bytes: total - used, used_bytes: used, used_percent: Math.round((used / total) * 1000) / 10,
    model_bytes: modelBytes, files: models.length, checked_at: now - 12e3, host: 'scheduler-host' };
  return { disk, models, models_dir: '/models', history: true, version: 7 };
}

// modelsInstances(status, state, mac) -> the status with the instances the
// catalog names: gemma-4-27b ready on the first two consoles, or none
export function modelsInstances(st, state, mac) {
  const insts = state === 'none' ? [] : [{ profile: 'gemma-4-27b', state: 'ready', in_flight: 2, waiters: 0, parallel: 1, host: 'server', requests_served: 1840,
    nodes: [{ id: mac(1), role: 'host', share: 0.5 }, { id: mac(2), role: 'helper', share: 0.5 }] }];
  return Object.assign({}, st, { instances: insts, queue: [], recent_loads: [] });
}

export const MIB_PER_GB = GB / MIB;
