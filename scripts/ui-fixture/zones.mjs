// zones.mjs: the Zones pages' fixture states for ui-fixture-server.mjs
// (--zones none|groups|fleet), and what v43 adds to every state: each
// zone's zone_state and group, the one set of counts on GET /api/nodes,
// GET /api/groups and GET /api/nodes/uptime.
//
//   none    the Overview's eight machines, no group (the default)
//   groups  the same eight in two groups (rack-a, home) and one with no
//           group; one lost machine, one set aside
//   live    as the live pool is: eight zones and no group (six consoles,
//           laptop-01 named only, laptop-02 the scheduler's own machine
//           switched off) and one removed machine the page leaves out
//   one-group  live with one group (pair: ps5-01, ps5-02)
//   fleet   232 zones as the Zones-Fleet mockup draws them: rack-a (48
//           Oberon, its first rows as drawn), rack-b, rack-c, rack-d, a
//           paused group gpu-row-1 and 28 with no group; 15 pages
//
// Names and addresses are generic (192.0.2.x, 02:00:00:…).

const MIB = 1024;
const H = 3600000;
const iso = ms => new Date(ms).toISOString();
const hex = n => n.toString(16).padStart(2, '0');
const fmac = n => '02:00:00:00:' + hex(n >> 8) + ':' + hex(n & 255);

// zoneState(st, agent) -> the zone_state the server's one availability.Classify gives
export function zoneState(st, a) {
  const inst = (st.instances || []).find(i => (i.nodes || []).some(m => m.id === a.id));
  const node = ((st.view && st.view.nodes) || []).find(n => n.id === a.id) || {};
  if (a.state === 'removed') return 'removed';
  if (a.excluded) return 'off';
  if (a.state === 'lost' || node.online === false) return 'lost';
  if (a.set_aside || (st.quarantines || []).some(q => q.node === a.id)) return 'set_aside';
  if (inst && inst.state === 'starting') return 'loading';
  if (inst || node.holder) return 'in_use';
  if (a.paused) return 'off';
  return 'free';
}

// counts(st) -> GET /api/nodes `counts` (internal/api/counts.go)
export function counts(agents) {
  const c = { total: 0, reachable: 0, up: 0, down: 0, lost: 0, set_aside: 0, off: 0, in_use: 0, free: 0, loading: 0 };
  for (const a of agents) {
    if (a.zone_state === 'removed') continue;
    c.total++;
    c[a.zone_state]++;
  }
  c.down = c.lost + c.set_aside;
  c.reachable = c.total - c.down;
  c.up = c.free + c.in_use + c.loading;
  return c;
}

// ---------- the fleet (Zones-Fleet's sample) ----------

const P = { q4: 'qwen3-4b', q14: 'qwen3-14b', g12: 'gemma-4-12b:64k', g27: 'gemma-4-27b:32k', g27l: 'gemma-4-27b:64k' };
// a deterministic number in [0, 1) from a seed
const rnd = seed => { const x = Math.sin(seed * 12.9898 + 78.233) * 43758.5453; return x - Math.floor(x); };

// the fleet's zones: {i, name, group, chip, total, state, inst?, role?, share?, used, temp, up7, gone?, paused?}
function fleetZones() {
  const zs = [];
  let i = 0;
  const add = (name, group, chip, total, o) => { i++; zs.push(Object.assign({ i, name, group, chip, total }, o)); return zs[zs.length - 1]; };
  const insts = []; // {profile, parallel, in_flight, state, progress, members: [{z, role, share}]}
  const inst = (profile, busy, ...members) => { insts.push({ profile, parallel: 4, in_flight: busy, state: 'ready', members }); };

  // rack-a: its first rows as drawn; 30 in use, 14 free, 1 loading, 3 down
  const ra = {};
  for (let n = 1; n <= 48; n++) ra[n] = add('ra-' + String(n).padStart(2, '0'), 'rack-a', 'Oberon', 13.5, { state: 'free' });
  const z = n => ra[n];
  Object.assign(z(36), { state: 'lost', gone: 54, up7: 0.947 });
  Object.assign(z(48), { state: 'lost', gone: 13, up7: 0.942 });
  Object.assign(z(35), { state: 'set_aside', backIn: 3, up7: 0.994, used: 0.3, temp: 45 });
  Object.assign(z(45), { state: 'loading', up7: 0.988, used: 5.3, temp: 52 });
  insts.push({ profile: P.g27l, parallel: 1, in_flight: 0, state: 'starting', progress: 0.49, members: [{ z: z(45), role: 'host', share: 1 }] });
  const use = (n, used, temp, up7) => Object.assign(z(n), { state: 'in_use', used, temp, up7 });
  use(1, 11.8, 61, 0.995); use(2, 11.7, 62, 0.996); use(4, 8.6, 68, 0.999); use(7, 11.9, 65, 0.986); use(8, 9.6, 70, 0.993);
  use(9, 10.2, 62, 0.999); use(11, 8.9, 63, 0.999); use(12, 12.1, 67, 0.988); use(13, 9.9, 63, 0.993); use(14, 11.0, 61, 0.989);
  use(15, 12.1, 67, 0.994); use(17, 8.5, 68, 0.986);
  const rest = [18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32, 33, 34, 37, 38, 39, 40, 41, 42, 43, 44, 46, 47];
  const helpers = rest.slice(0, 18);
  helpers.forEach((n, k) => use(n, 9 + rnd(n) * 3, 60 + Math.round(rnd(n + 1) * 8), 0.985 + rnd(n + 2) * 0.015));
  const hp = () => z(helpers.shift());
  inst(P.q4, 3, { z: z(1), role: 'host' });
  inst(P.q4, 2, { z: z(2), role: 'host' });
  inst(P.g12, 2, { z: z(4), role: 'host', share: 0.6 }, { z: hp(), role: 'helper', share: 0.4 });
  inst(P.g27, 4, { z: hp(), role: 'host', share: 0.25 }, { z: z(7), role: 'helper', share: 0.25 }, { z: hp(), role: 'helper', share: 0.25 }, { z: hp(), role: 'helper', share: 0.25 });
  inst(P.q14, 1, { z: hp(), role: 'host', share: 0.6 }, { z: z(8), role: 'helper', share: 0.4 });
  inst(P.g27, 3, { z: z(9), role: 'host' });
  inst(P.g12, 2, { z: z(11), role: 'host' });
  inst(P.g12, 4, { z: z(12), role: 'host', share: 0.4 }, { z: hp(), role: 'helper', share: 0.3 }, { z: hp(), role: 'helper', share: 0.3 });
  inst(P.q4, 1, { z: hp(), role: 'host', share: 0.5 }, { z: z(13), role: 'helper', share: 0.25 }, { z: hp(), role: 'helper', share: 0.25 });
  inst(P.q14, 2, { z: hp(), role: 'host', share: 0.5 }, { z: z(14), role: 'helper', share: 0.25 }, { z: hp(), role: 'helper', share: 0.25 });
  inst(P.g12, 3, { z: z(15), role: 'host' });
  inst(P.g12, 2, { z: hp(), role: 'host', share: 0.5 }, { z: z(17), role: 'helper', share: 0.25 }, { z: hp(), role: 'helper', share: 0.25 });
  while (helpers.length) inst(P.q4, 1 + (helpers.length % 4), { z: hp(), role: 'host' });
  for (const n of [3, 5, 6, 10, 16].concat(rest.slice(18))) Object.assign(z(n), { state: 'free', used: 0.3, temp: 45 + Math.round(rnd(n) * 8), up7: 0.995 + rnd(n) * 0.005 });

  // rack-b, rack-c, rack-d (48 each) and 28 with no group: 122 in use, 35
  // free, 7 loading, 8 down among them
  const plan = [].concat(Array(122).fill('in_use'), Array(35).fill('free'), Array(7).fill('loading'), Array(8).fill('down'));
  for (let k = plan.length - 1; k > 0; k--) { const j = Math.floor(rnd(k + 7) * (k + 1)); [plan[k], plan[j]] = [plan[j], plan[k]]; }
  const generic = [];
  for (const [g, pre, n, chip, total] of [['rack-b', 'rb', 48, 'Oberon', 13.5], ['rack-c', 'rc', 48, 'Oberon', 13.5], ['rack-d', 'rd', 48, 'Oberon', 13.5], ['', 'node', 28, 'RTX 3070 Ti', 8]]) {
    for (let k = 1; k <= n; k++) generic.push(add(pre + '-' + String(k).padStart(2, '0'), g, chip, total, {}));
  }
  const profiles = [P.q4, P.q14, P.g12, P.g27];
  let pending = null, downs = 0;
  generic.forEach((x, k) => {
    const s = plan[k];
    const r = rnd(x.i);
    if (s === 'down') {
      downs++;
      Object.assign(x, downs % 2 ? { state: 'lost', gone: 5 + Math.round(r * 300), up7: 0.9 + r * 0.05 } : { state: 'set_aside', backIn: 2 + Math.round(r * 15), up7: 0.97 + r * 0.02, used: 0.3, temp: 46 });
    } else if (s === 'loading') {
      Object.assign(x, { state: 'loading', used: x.total * 0.4, temp: 52, up7: 0.99 + r * 0.01 });
      insts.push({ profile: profiles[x.i % 4], parallel: 1, in_flight: 0, state: 'starting', progress: 0.2 + r * 0.7, members: [{ z: x, role: 'host', share: 1 }] });
    } else if (s === 'free') {
      Object.assign(x, { state: 'free', used: 0.3, temp: 44 + Math.round(r * 9), up7: 0.993 + r * 0.007 });
    } else {
      Object.assign(x, { state: 'in_use', used: x.total * (0.6 + r * 0.3), temp: 58 + Math.round(r * 12), up7: 0.985 + r * 0.015 });
      if (pending && pending.group === x.group) { inst(profiles[x.i % 4], 1 + (x.i % 4), { z: pending, role: 'host', share: 0.6 }, { z: x, role: 'helper', share: 0.4 }); pending = null; }
      else { if (pending) inst(profiles[pending.i % 4], 2, { z: pending, role: 'host' }); pending = x; }
    }
  });
  if (pending) inst(profiles[pending.i % 4], 2, { z: pending, role: 'host' });

  // gpu-row-1: twelve laptops, the group paused: ten switched off, two still serving
  for (let k = 1; k <= 12; k++) {
    const x = add('gpu-' + String(k).padStart(2, '0'), 'gpu-row-1', 'RTX 5060', 8, { paused: true, up7: null });
    if (k <= 2) { Object.assign(x, { state: 'in_use', used: 6.4, temp: 66, up7: 0.998 }); inst(P.q4, 1, { z: x, role: 'host' }); }
    else Object.assign(x, { state: 'off', used: 0.2, temp: 40 });
  }
  return { zs, insts };
}

function fleetStatus(base, now) {
  const t = Math.round(now / 1000) * 1000;
  const { zs, insts } = fleetZones();
  const nodes = [], agents = [], quarantines = [];
  for (const x of zs) {
    const id = fmac(x.i);
    const online = x.state !== 'lost';
    const im = insts.find(s => s.members.some(m => m.z === x));
    nodes.push({ id, hostname: x.name, local: false, online, addr: '', priority: 1, eligible: true, auto: true, excluded: false, excluded_by: '', unplaced: '', unplaced_message: '',
      total_mib: Math.round(x.total * MIB), free_mib: online ? Math.round((x.total - (x.used || 0)) * MIB) : 0, holder: im && im.state === 'ready' ? im.profile : '',
      claimed_by: '', settling: '', settle_until: null, free_at: null, free_basis: '',
      quarantined: x.state === 'set_aside' ? iso(t - 600000) : null, missing_since: x.gone ? iso(t - x.gone * 60000) : null, reserved: [], reserved_mib: 0 });
    const aside = x.state === 'set_aside' ? { reason: 'preflight_failed', message: 'The pre-load check failed: the model did not fit the free memory.', until: iso(t + x.backIn * 60000), level: 1 } : undefined;
    if (aside) quarantines.push({ node: id, until: aside.until, level: 1, reason: aside.reason, message: aside.message, since: iso(t - 600000) });
    agents.push({ id, name: x.name, addr: '192.0.2.' + (10 + (x.i % 240)), state: online ? 'active' : 'lost', paused: !!x.paused,
      paused_actor: x.paused ? 'anonymous' : undefined, paused_at: x.paused ? iso(t - 3 * H) : undefined, placeable: online && !x.paused,
      excluded: false, unplaced: '', last_seen_ago_s: online ? 1 : x.gone * 60, platform: x.chip === 'Oberon' ? 'linux-amd64-vulkan' : 'linux-amd64-cuda', release: '0.1.9',
      chip: x.chip, devices: [{ name: x.chip, backend: x.chip === 'Oberon' ? 'vulkan' : 'cuda', total_mib: Math.round(x.total * MIB) }],
      stats: online && x.temp != null ? { headline_temp_c: x.temp } : {}, set_aside: aside, group: x.group || undefined });
  }
  const instances = insts.map(s => {
    const host = s.members.find(m => m.role === 'host') || s.members[0];
    const o = { profile: s.profile, state: s.state, in_flight: s.in_flight, waiters: 0, parallel: s.parallel, host: 'node', host_node: fmac(host.z.i), requests_served: 40,
      nodes: s.members.map(m => Object.assign({ id: fmac(m.z.i), role: m.role }, m.share != null && s.members.length > 1 ? { share: m.share } : {})) };
    if (s.state === 'starting') Object.assign(o, { load_progress: s.progress, load: { progress: s.progress } });
    return o;
  });
  const st = Object.assign({}, base, { instances, queue: [], quarantines, view: Object.assign({}, base.view, { nodes }), agents });
  Object.defineProperty(st, '_fleet', { value: zs, enumerable: false }); // the uptimes the fixture gives each zone (left out of the answers)
  return st;
}

// ---------- the states ----------

const GROUPS_OF_EIGHT = { 'ps5-01': 'rack-a', 'ps5-02': 'rack-a', 'ps5-03': 'rack-a', 'ps5-04': 'rack-a', 'laptop-01': 'home', 'ps5-05': 'home', 'ps5-06': 'home' };
const UP_OF_EIGHT = { 'ps5-01': 0.995, 'ps5-02': 0.996, 'ps5-03': 0.988, 'ps5-04': 0.999, 'laptop-01': 0.991, 'ps5-05': 0.994, 'ps5-06': 0.947, 'laptop-02': null };

// zonesStatus(base, now, mode) -> the status with zone_state and group on each agent
export function zonesStatus(base, now, mode) {
  const st = mode === 'fleet' ? fleetStatus(base, now) : Object.assign({}, base, { agents: base.agents.map(a => Object.assign({}, a)) });
  if (mode === 'live' || mode === 'one-group') liveLike(st, now, mode === 'one-group');
  for (const a of st.agents) {
    if (mode === 'groups' && GROUPS_OF_EIGHT[a.name]) a.group = GROUPS_OF_EIGHT[a.name];
    a.zone_state = zoneState(st, a);
  }
  return st;
}

// liveLike(st, now, oneGroup): the eight machines as the live pool has them
function liveLike(st, now, oneGroup) {
  const view = Object.assign({}, st.view, { nodes: ((st.view && st.view.nodes) || []).map(n => Object.assign({}, n)) });
  st.view = view;
  const node = name => view.nodes.find(n => n.hostname === name) || {};
  const agent = name => st.agents.find(a => a.name === name) || {};
  // all six consoles reachable
  Object.assign(node('ps5-06'), { online: true, missing_since: null });
  Object.assign(agent('ps5-06'), { state: 'active', placeable: true });
  // laptop-01: named only (no_auto_nodes)
  node('laptop-01').auto = false;
  // laptop-02: the scheduler's own machine, switched off (exclude_nodes)
  node('laptop-02').local = true;
  // a removed machine: in the agent list, left out of the page
  st.agents.push({ id: '02:00:00:00:00:99', name: 'ps5-old', addr: '192.0.2.199', state: 'removed', paused: false, placeable: false, last_seen_ago_s: 9 * 86400 });
  if (oneGroup) { agent('ps5-01').group = 'pair'; agent('ps5-02').group = 'pair'; }
}

const sum = (xs, f) => xs.reduce((s, x) => s + (f(x) || 0), 0);
function upOf(st, a) {
  if (st._fleet) { const x = st._fleet.find(z => z.name === a.name); return x && x.up7 != null ? x.up7 : null; }
  return a.name in UP_OF_EIGHT ? UP_OF_EIGHT[a.name] : 0.99;
}
// the 7-day figure of some zones: their reachable seconds summed (the
// fixture's per-zone shares stand for those seconds, a week each)
function upTotals(st, agents) {
  const counted = agents.filter(a => upOf(st, a) != null);
  const up = sum(counted, a => upOf(st, a) * 604800);
  return { counted_s: counted.length * 604800, up_s: Math.round(up), uptime: counted.length ? Math.round(up / (counted.length * 604800) * 1000) / 1000 : null };
}

function groupFigures(st, name, agents) {
  const nodes = (st.view && st.view.nodes) || [];
  const nodeOf = a => nodes.find(n => n.id === a.id) || {};
  const online = agents.filter(a => a.zone_state !== 'lost');
  const temps = online.map(a => ({ a, t: a.stats && a.stats.headline_temp_c })).filter(x => typeof x.t === 'number');
  const hot = temps.slice().sort((x, y) => y.t - x.t)[0];
  const models = {};
  for (const i of st.instances || []) for (const m of i.nodes || []) if (agents.some(a => a.id === m.id)) models[i.profile] = (models[i.profile] || 0) + 1;
  const paused = agents.filter(a => a.paused).length;
  return {
    name, members: agents.map(a => a.id), member_names: agents.map(a => a.name), count: agents.length, counts: counts(agents),
    memory: { used_mib: sum(online, a => nodeOf(a).total_mib - nodeOf(a).free_mib), total_mib: sum(agents, a => nodeOf(a).total_mib), each_total_mib: nodes.length ? nodeOf(agents[0]).total_mib : 0 },
    temp: temps.length ? { avg_c: Math.round(sum(temps, x => x.t) / temps.length), hottest_c: hot.t, hottest_id: hot.a.id, hottest_name: hot.a.name } : { avg_c: null, hottest_c: null },
    accepting: paused === 0 ? 'all' : paused === agents.length ? 'none' : 'mixed',
    needs_attention: agents.filter(a => a.zone_state === 'lost' || a.zone_state === 'set_aside').map(a => ({ id: a.id, name: a.name, state: a.zone_state })),
    models: Object.entries(models).map(([profile, zones]) => ({ profile, zones })),
    uptime_7d: upTotals(st, agents),
  };
}

// window(now, partial) -> the uptime window (from the start of the UTC hour 7 days ago)
function uptimeWindow(now, partial) {
  const from = Math.floor((now - 7 * 24 * H) / H) * H;
  const begins = partial ? now - 30 * H : now - 9 * 24 * H;
  return { days: 7, from: iso(from), to: iso(now), history_begins_at: iso(begins), covered_from: iso(Math.max(from, begins)), complete: !partial, recording: true };
}

export function groupsDoc(st, now, partial) {
  const names = [...new Set(st.agents.map(a => a.group).filter(Boolean))].sort();
  const live = st.agents.filter(a => a.zone_state !== 'removed');
  return { revision: 3, groups: names.map(g => groupFigures(st, g, live.filter(a => a.group === g))),
    ungrouped: groupFigures(st, '', live.filter(a => !a.group)), uptime_window: uptimeWindow(now, partial) };
}

export function uptimeDoc(st, now, partial) {
  const live = st.agents.filter(a => a.zone_state !== 'removed');
  const names = [...new Set(live.map(a => a.group).filter(Boolean))].sort();
  return { window: uptimeWindow(now, partial),
    zones: live.map(a => Object.assign({ id: a.id, name: a.name, group: a.group || '' }, upTotals(st, [a]))),
    groups: names.map(g => Object.assign({ name: g }, upTotals(st, live.filter(a => a.group === g)))),
    ungrouped: upTotals(st, live.filter(a => !a.group)), fleet: upTotals(st, live) };
}

// fleetStrip(st, agent) -> 24 cell codes of the fixture's availability for a fleet zone
export function fleetStrip(st, a) {
  const x = st._fleet && st._fleet.find(z => z.name === a.name);
  if (!x) return null;
  const out = [];
  for (let k = 0; k < 24; k++) {
    const r = rnd(x.i * 31 + k);
    const busy = r < 0.35 ? 'a' : r < 0.55 ? 'c1' : r < 0.75 ? 'c2' : r < 0.9 ? 'c3' : 'c4';
    out.push(x.state === 'free' && r < 0.6 ? 'a' : (k >= 5 && k <= 11 && r < 0.8) ? 'a' : busy);
  }
  if (x.state === 'lost') for (let k = 24 - Math.max(1, Math.ceil(x.gone / 60)); k < 24; k++) out[k] = 'dn';
  if (x.state === 'set_aside') out[23] = 'dn';
  if (x.state === 'loading') out[23] = 'ld';
  if (x.state === 'off') for (let k = 21; k < 24; k++) out[k] = 'x';
  return out;
}
