// Profiles and plans for the fixture (phase 5: Placement preview, Profiles),
// served by ui-fixture-server.mjs. Generic names and documentation addresses
// only (192.0.2.x, 02:00:00:… MACs).
//
//   profileEntries()  GET /api/profiles: 12 hand-written profiles and 300
//                     generated benchmark profiles (generated: true)
//   profileSpecs()    GET /api/config profiles{} (the editor's source)
//   testProfiles()    GET /api/profiles/test (the Test profiles page)
//   standardSet(file, state)   GET and POST /api/models/{file}/standard-set (--stdset)
//
// The marking (profiles plan, section 5): two hand-written profiles pinned to
// zones are marked production, one is left to the rule (test: forced
// zones), qwen3-14b-128k is marked test with a display name, and every
// generated one is test by the rule; gemma-4-27b has a display name.
//   catalog()         GET /api/models (with --catalog): one entry per model file, short_name
//   planAnswer(body)  POST /api/plan: the full dry-run plan (scheduler.PlanReport),
//                     on chosen zones when the body names them
//
// The hand-written profiles cover every verdict: serve, wait (loading),
// start, start with a tight fit, start with a forced-fit warning, evict (two
// victims, one busy), queue, backoff and reject (capacity).

const mac = n => '02:00:00:00:00:' + String(n).padStart(2, '0');
// the fixture server's machines (ui-fixture-server.mjs MACHINES): usable and free MiB now
export const ZONES = [
  { n: 1, name: 'ps5-01', budget: 2900, free: 3100 }, { n: 2, name: 'ps5-02', budget: 3200, free: 3400 },
  { n: 3, name: 'ps5-03', budget: 9300, free: 9500 }, { n: 4, name: 'ps5-04', budget: 9700, free: 9900 },
  { n: 5, name: 'laptop-01', budget: 1100, free: 1300 }, { n: 6, name: 'ps5-05', budget: 13300, free: 13500 },
  { n: 7, name: 'ps5-06', budget: 0, free: 0 }, { n: 8, name: 'laptop-02', budget: 6900, free: 7000 },
].map(z => Object.assign(z, { id: mac(z.n) }));
const zone = name => ZONES.find(z => z.name === name);

// the model files: key (file), the catalog's name and short_name
export const MODEL_FILES = [
  { file: 'gemma-4-27b-it-Q4_K_M.gguf', name: 'Gemma 4 27B', short_name: 'gemma-4-27b', size: 16.5, publisher: 'google', family: 'gemma' },
  { file: 'gemma-4-12b-it-Q4_K_M.gguf', name: 'Gemma 4 12B', short_name: 'gemma-4-12b', size: 7.3, publisher: 'google', family: 'gemma' },
  { file: 'Qwen3-14B-Q4_K_M.gguf', name: 'Qwen3 14B', short_name: 'qwen3-14b', size: 8.4, publisher: 'qwen', family: 'qwen' },
  { file: 'Qwen3-32B-Q4_K_M.gguf', name: 'Qwen3 32B', short_name: 'qwen3-32b', size: 18.4, publisher: 'qwen', family: 'qwen' },
  { file: 'Llama-3.3-70B-Instruct-Q3_K_M.gguf', name: 'Llama 3.3 70B Instruct', short_name: 'llama-3.3-70b', size: 31.9, publisher: 'meta', family: 'llama' },
  { file: 'GLM-4.5-Air-Q2_K.gguf', name: 'GLM 4.5 Air', short_name: 'glm-4.5-air', size: 41.2, publisher: 'zai', family: 'glm' },
];
const fileOf = s => MODEL_FILES.find(m => m.short_name === s).file;

// hand-written profiles: [id, model short name, ctx, forced zones, need MiB, verdict, extra]
const HAND = [
  ['gemma-4-27b', 'gemma-4-27b', 32768, [], 17900, 'serve', { description: 'The default chat model: two consoles, host on the first.', aliases: ['chat'], display_name: 'Gemma chat' }],
  ['gemma-4-27b-long', 'gemma-4-27b', 131072, [], 25400, 'evict', { description: 'Long documents. Takes three consoles; unloads the small models first.' }],
  ['gemma-4-12b', 'gemma-4-12b', 65536, [], 9100, 'wait', {}],
  ['gemma-4-12b-fast', 'gemma-4-12b', 8192, ['ps5-05'], 13120, 'tight', { description: 'One console, short context, for quick answers.', stage: 'production' }],
  ['qwen3-14b', 'qwen3-14b', 40960, [], 9800, 'start', { aliases: ['coder'] }],
  ['qwen3-14b-laptop', 'qwen3-14b', 16384, ['laptop-02'], 9200, 'warn', { description: 'Pinned to the named-only laptop: it is the only profile that may use it.' }],
  ['qwen3-32b', 'qwen3-32b', 32768, [], 20500, 'queue', {}],
  ['qwen3-32b-split', 'qwen3-32b', 32768, ['ps5-03', 'ps5-04', 'ps5-05'], 21000, 'start', { stage: 'production', tensor_split: [3, 3, 4], description: 'A hand-made split over three consoles, measured at 9.8 tok/s; the even split ran out of memory on the first console, so the third takes the largest share and hosts nothing.' }],
  ['llama-3.3-70b', 'llama-3.3-70b', 16384, [], 36800, 'reject', {}],
  ['glm-4.5-air', 'glm-4.5-air', 32768, [], 46100, 'reject', { description: 'Too big for this fleet today; kept for when the second rack joins.' }],
  ['qwen3-14b-128k', 'qwen3-14b', 131072, [], 14800, 'backoff', { stage: 'test', display_name: 'Qwen 128k trial' }],
  ['gemma-4-12b-vision', 'gemma-4-12b', 32768, [], 8600, 'start', { parallel: 2 }],
];
const RACK = ['ps5-03', 'ps5-04', 'ps5-05'];
// each file's trained context (GGUF context_length)
const TRAINED = { 'gemma-4-27b': 131072, 'gemma-4-12b': 131072, 'qwen3-14b': 40960, 'qwen3-32b': 40960, 'llama-3.3-70b': 131072, 'glm-4.5-air': 131072 };

// marks(e, x) -> the server's marking fields (config.EffectiveStage, ShownName)
function marks(e, x) {
  const set = x.stage === 'test' || x.stage === 'production';
  const stage = set ? x.stage : (e.generated || e.force_nodes.length ? 'test' : 'production');
  const name = x.display_name || e.id;
  Object.assign(e, { stage, stage_set: set, shown_name: (stage === 'test' ? 'TEST - ' : '') + name });
  if (x.display_name) e.display_name = x.display_name;
  return e;
}

// the short plan of GET /api/profiles (internal/api/profiles.go)
function planOf(action, need, force) {
  const base = { action, would_start: ['start', 'serve', 'tight', 'warn'].includes(action), nodes: [], reason: '', instance: '' };
  switch (action) {
    case 'serve': return Object.assign(base, { nodes: ['ps5-03', 'ps5-04'], instance: 'ready' });
    case 'wait': return Object.assign(base, { nodes: ['ps5-06'], instance: 'starting' });
    case 'start': return Object.assign(base, { nodes: force.length ? force : ['ps5-05'] });
    case 'tight': return Object.assign(base, { action: 'start', nodes: force, tight_fit: true, reason: 'tight fit: 180 MiB spare on ps5-05, margin 512 MiB not met' });
    case 'warn': return Object.assign(base, { action: 'start', nodes: force, warning: `forced_fit_check is warn: the forced zones have 6900 MiB usable, the profile needs ${need} MiB; it starts anyway` });
    case 'evict': return Object.assign(base, { nodes: RACK });
    case 'queue': return Object.assign(base, { reason: `no placement now: needs ${need} MiB; the largest free pool of 3 zones has 18300 MiB` });
    case 'backoff': return Object.assign(base, { reason: 'backoff: the last load failed 2 min ago (out of memory on ps5-04); retry in 3 min' });
    case 'reject': return Object.assign(base, { reason: `does not fit: needs ${Math.round(need * 1.05)} MiB with margins; the cluster can hold 31200 MiB` });
  }
  return base;
}

let entriesCache = null;
export function profileEntries() {
  if (entriesCache) return entriesCache;
  const out = [];
  for (const [id, short, ctx, force, need, action, x] of HAND) {
    const e = { id, kind: 'llama', model: fileOf(short), ctx, parallel: x.parallel || 1, aliases: x.aliases || [], force_nodes: force, tensor_split: x.tensor_split || [],
      generated: false, needs_mib: need, needs_basis: action === 'reject' ? 'estimated, +5% margin; the estimate is ' + need + ' MiB' : 'measured, needs_mib', needs_measured: action !== 'reject',
      plan: planOf(action, need, force), host: force.length > 1 ? 'node' : 'server' };
    if (x.description) e.description = x.description;
    if (force.length > 1) Object.assign(e, { host_node: force[0], host_overhead_mib: 1536 });
    marks(e, x);
    if (x.stage) e.spec_stage = x.stage;
    out.push(e);
  }
  // 300 generated: six models × five contexts × ten zone counts
  const ctxs = [4096, 8192, 16384, 32768, 65536];
  let i = 0;
  for (const m of MODEL_FILES) {
    for (const c of ctxs) {
      for (let k = 1; k <= 10; k++) {
        const id = (i % 3 === 2 ? 'm-' : 'bench-') + m.short_name + '-c' + c / 1024 + 'k-n' + k;
        const force = ZONES.filter(z => z.budget > 0).slice(0, Math.min(k, 6)).map(z => z.name);
        const need = Math.round(m.size * 1024 * 1.1 + c / 16);
        const action = need > 30000 ? 'reject' : k % 4 === 0 ? 'queue' : 'start';
        out.push(marks({ id, kind: 'llama', model: m.file, ctx: c, parallel: 1, aliases: [], force_nodes: force, tensor_split: [], generated: true,
          needs_mib: need, needs_basis: 'estimated, +5% margin; the estimate is ' + need + ' MiB', needs_measured: false, plan: planOf(action, need, force), host: 'server' }, {}));
        i++;
      }
    }
  }
  entriesCache = out.sort((a, b) => a.id.localeCompare(b.id));
  return entriesCache;
}

export function profileSpecs() {
  const out = {};
  for (const e of profileEntries()) {
    const s = { model: e.model, ctx: e.ctx };
    if (e.parallel > 1) s.parallel = e.parallel;
    if (e.force_nodes.length) s.force_nodes = e.force_nodes;
    if (e.tensor_split.length) s.tensor_split = e.tensor_split;
    if (e.aliases.length) s.aliases = e.aliases;
    if (e.description) s.description = e.description;
    if (e.generated) s.generated = true;
    if (e.spec_stage) s.stage = e.spec_stage;
    if (e.display_name) s.display_name = e.display_name;
    out[e.id] = s;
  }
  return out;
}

export function catalog() {
  const ps = profileEntries();
  return MODEL_FILES.map(m => {
    const mine = ps.filter(p => p.model === m.file);
    return { key: m.file, kind: 'llama', file: m.file, exists: true, size_bytes: Math.round(m.size * 2 ** 30), name: m.name, short_name: m.short_name, publisher: m.publisher, family: m.family,
      brand: m.family, quant: (m.file.match(/(Q\d_K(?:_M)?)/) || [])[1] || '', state: 'not_loaded', icon: null,
      profiles: mine.map(p => ({ id: p.id, ctx: p.ctx, parallel: p.parallel, force_nodes: p.force_nodes, generated: p.generated, needs_mib: p.needs_mib, plan: p.plan,
        stage: p.stage, stage_set: p.stage_set, shown_name: p.shown_name, display_name: p.display_name })),
      profile_count: mine.length, generated_count: mine.filter(p => p.generated).length, startable_count: mine.filter(p => p.plan.would_start).length,
      instances: [], measured: [], loads: { count: 0, recent: [] } };
  });
}

const EMPTY_ACTIONS = ['queue', 'reject', 'backoff'];

export function planAnswer(body) {
  const asked = body && body.model;
  const e = profileEntries().find(p => p.id === asked || (p.aliases || []).includes(asked));
  if (!e) return { status: 404, body: { error: { message: `unknown model "${asked}"`, type: 'invalid_request_error', code: 'model_not_found' } } };
  const chosen = (body.nodes || []).map(n => ZONES.find(z => z.id === n || z.name === n)).filter(Boolean);
  const need = e.needs_mib;
  const fit = Math.round(need * (e.needs_measured ? 1 : 1.05));
  let action = e.plan.action, reason = e.plan.reason || '', warning = e.plan.warning || '', tight = !!e.plan.tight_fit;
  let names = e.plan.nodes;
  if (chosen.length) {
    names = chosen.map(z => z.name);
    const room = chosen.reduce((s, z) => s + z.budget, 0);
    action = 'start'; reason = ''; warning = ''; tight = false;
    if (room < fit) warning = `forced_fit_check is warn: the chosen zones have ${room} MiB usable, the profile needs ${fit} MiB; it starts anyway`;
    else if (room - fit < 512) { tight = true; reason = `tight fit: ${room - fit} MiB spare on ${names[names.length - 1]}, margin 512 MiB not met`; }
  }
  const placed = EMPTY_ACTIONS.includes(action) ? [] : names.map(zone);
  const share = placed.length ? 1 / placed.length : 0;
  let memory = null;
  if (placed.length) {
    const nodes = placed.map((z, i) => {
      const take = Math.round(fit * share);
      const remains = z.budget - take;
      return { id: z.id, node: z.name, shared: z.name.startsWith('ps5'), total_mib: z.name.startsWith('ps5') ? 13824 : 8192, weights_mib: Math.round(take * 0.8), context_mib: take - Math.round(take * 0.8),
        fill_reserve_mib: 0, will_take_mib: take, basis: e.needs_measured ? 'measured' : 'estimated', free_now_mib: z.free, free_avg_mib: z.free - 40, free_avg_known: true, reserved_by: [],
        headroom_mib: 200, available_mib: z.budget, margin_mib: 512, margin_rule: 'margin_measured_mib', remains_mib: remains, host_overhead_mib: i === 0 && e.host === 'node' ? 1536 : 0,
        spare_mib: remains, tight: remains >= 0 && remains < 512, margin_short_mib: remains >= 0 && remains < 512 ? 512 - remains : 0, placeable_mib: z.budget };
    });
    const fits = nodes.every(n => n.remains_mib >= 0);
    const worst = nodes.slice().sort((a, b) => a.remains_mib - b.remains_mib)[0];
    memory = { nodes, fits, short_node: fits ? '' : worst.node, shortfall_mib: fits ? 0 : -worst.remains_mib, hypothetical: false, placement_uses_reserved: true, tight_fit: tight, decides: true, note: '' };
  }
  return { status: 200, body: {
    model: e.id, alias: e.id !== asked, action, instance: e.plan.instance || '',
    placement: placed.map((z, i) => ({ id: z.id, addr: '192.0.2.' + (100 + z.n) + ':50052', local: false, budget_mib: z.budget, memory: [], role: placed.length > 1 ? (i === 0 ? 'host' : 'helper') : '', share: placed.length > 1 ? share : 0 })),
    victims: action === 'evict' ? [{ profile: 'gemma-4-12b-vision', busy: false }, { profile: 'qwen3-14b', busy: true }] : [],
    reason, excluded_by: '', need_mib: need, measured: e.needs_measured, fit_need_mib: fit, need_basis: e.needs_basis, derived_from_ctx: 0, derived_slope: null, warning,
    required_mib: action === 'reject' ? fit : 0, capacity_mib: action === 'reject' ? 31200 : 0, load_mode: action === 'start' ? 'file_read' : '', reload_from_cache: [],
    memory, tight_fit: tight,
    host: e.host, host_node: e.host === 'node' ? zone(e.host_node).id : '', host_overhead_mib: e.host === 'node' ? 1536 : 0, host_problem: '',
  } };
}

// testProfiles(now) -> GET /api/profiles/test (internal/api/testprofiles.go):
// the test profiles, each with the context it really runs at (a Qwen3 file
// is trained at 40,960: its 64k bench profiles run at 40k), why it is test,
// who refers to it and its last use
export function testProfiles(now = Date.now()) {
  const rows = [], models = {}, reasons = {};
  let i = 0, production = 0;
  for (const e of profileEntries()) {
    if (e.stage !== 'test') { production++; continue; }
    const why = [];
    if (e.stage_set) why.push('marked');
    if (e.generated) why.push('generated');
    if (e.force_nodes.length) why.push('forced zones');
    const short = MODEL_FILES.find(m => m.file === e.model).short_name;
    const trained = TRAINED[short] || 0;
    const real = trained && e.ctx > trained ? trained : e.ctx;
    const row = { id: e.id, shown_name: e.shown_name, model: e.model, model_name: short, ctx: e.ctx, ctx_real: real, reason: why[0], reasons: why, stage_set: e.stage_set,
      aliases: e.aliases, force_nodes: e.force_nodes,
      refs: { benchmarks: i % 7 === 0 ? ['cold-load-' + short] : [], groups: i % 21 === 0 ? ['nightly'] : [] }, last_used: i % 5 === 0 ? now - (i + 1) * 3600e3 : null };
    if (real !== e.ctx) row.ctx_note = 'ctx ' + e.ctx.toLocaleString('en-US') + ' is above this model\'s trained context of ' + trained.toLocaleString('en-US') + '; the server runs it at ' + trained.toLocaleString('en-US');
    if (e.display_name) row.display_name = e.display_name;
    rows.push(row);
    models[e.model] = (models[e.model] || 0) + 1;
    reasons[why[0]] = (reasons[why[0]] || 0) + 1;
    i++;
  }
  return { profiles: rows, count: rows.length, production_count: production, models, reasons, history: true };
}

// standardSet(file, state) -> GET /api/models/{file}/standard-set
// (internal/api/standardset.go) for any file: 16k and 32k kept on one zone,
// 64k kept on two, 128k only on every zone with the host memory not counted;
// the default id exists; kept means it fits on an idle fleet, and 64k would
// unload another model now. state: ready (the default), nothing (every kept one
// exists), error (the read fails), refused (the write fails).
export function standardSet(file, state) {
  if (state === 'error') return { status: 503, body: { error: { message: 'the scheduler is shutting down', type: 'server_error', code: 'shutting_down' } } };
  const short = file.replace(/\.gguf$/i, '').replace(/-?(it-)?Q\d_K(_M)?|-?Q\d_\d$/i, '').toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '');
  const all = state === 'nothing';
  const row = (ctx, id, x) => Object.assign({ id, ctx, kept: true, action: 'start', reason: '', zones: ['ps5-05'], zones_may_use: 6, need_mib: 9000 + ctx / 16, need_source: 'estimate',
    need_basis: 'estimated, +5% margin; the estimate is ' + (9000 + ctx / 16) + ' MiB', uses_every_zone: false, host: 'server', host_memory_mib: 4096, host_memory_counted: false,
    host_memory_note: "restore points and prompt cache on the scheduler's machine: up to 4,096 MiB, not counted in what it needs (restore points 0 MiB, prompt cache 4,096 MiB)",
    exists: all, default: false }, all ? { exists_as: 'the profile ' + id } : {}, x);
  const rows = [
    row(16384, short + ':16k', { need_source: 'derived', need_basis: 'derived from the measurement at ctx 32768' }),
    row(32768, short, { default: true, exists: true, exists_as: 'the profile ' + short, need_source: 'measured for this shape', need_basis: 'measured this run' }),
    row(65536, short + ':64k', { zones: ['ps5-03', 'ps5-04'], action: 'evict', now_note: 'fits only when other models are unloaded: now it would first unload gemma-4-12b' }),
    row(131072, short + ':128k', { zones: ['ps5-01', 'ps5-02', 'ps5-03', 'ps5-04', 'ps5-05', 'ps5-06'], uses_every_zone: true,
      every_zone_note: 'this context fits only by using every zone it may use (6 of 6): nothing else can be placed beside it', host_memory_mib: 10240,
      host_memory_note: "restore points and prompt cache on the scheduler's machine: up to 10,240 MiB, not counted in what it needs (restore points 6,144 MiB, prompt cache 4,096 MiB)" }),
    // 256k = the trained context (not above it): planned as written, its need known; it
    // does not fit the zones even with nothing loaded, and the reason is the plan's own
    Object.assign(row(262144, short + ':256k'), { kept: false, action: 'reject', exists: false, zones: [], need_mib: 63488, need_source: 'estimate',
      need_basis: 'estimated, +5% margin; the estimate is 60465 MiB', host_memory_note: '',
      reason: 'it does not fit even with nothing loaded; the plan says: placer: need 63488 MiB (requires 66662 MiB) does not fit the available nodes' }),
  ];
  const toWrite = rows.filter(r => r.kept && !r.exists).map(r => r.id);
  return { status: 200, body: { file, short, trained_ctx: 262144, trained_ctx_known: true, note: 'trained at 262,144 tokens: no candidate above it', default_id: short, default_ctx: 32768, rows, to_write: toWrite } };
}

// standardWrite(file, state, body) -> POST /api/models/{file}/standard-set
export function standardWrite(file, state, body) {
  if (!body || !String(body.comment || '').trim()) return { status: 400, body: { error: { message: 'a comment is required (body {"comment": "..."} or the X-Comment header)', type: 'invalid_request_error' } } };
  if (state === 'refused') return { status: 409, body: { error: { message: 'configuration changed since revision 412 (now 413): read it again', type: 'invalid_request_error', code: 'stale' } } };
  const set = standardSet(file, state).body;
  const written = set.to_write;
  const skipped = set.rows.filter(r => r.exists).map(r => ({ id: r.id, reason: 'exists, kept as it is' }))
    .concat(set.rows.filter(r => !r.kept).map(r => ({ id: r.id, reason: 'not kept: ' + r.reason })));
  return { status: 200, body: { revision: 413, changed: written.length > 0, written, skipped, rows: set.rows, diff: [], warnings: [], audit_entry_ids: [] } };
}
