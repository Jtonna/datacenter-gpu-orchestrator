// tiers.mjs: the Tiers and zones page's fixture states for
// ui-fixture-server.mjs (--tiers live|catchall, with --zones live).
//
//   live      shaped like the real instance: tier 1 platform
//             linux-amd64-vulkan (the six consoles), tier 2 host name
//             alienware* (one cuda laptop, alienware-01, named only), tier 3
//             the local zone (scheduler-host), tier 4 host name laptop-* and
//             platform linux-amd64-cuda (no zone). devbox-02 (the other cuda
//             laptop, excluded) fits no tier: tier_reason "no tier matches".
//   catchall  a truly empty match first: every zone is in tier 1 and the
//             tiers below it are never reached (never_reached).
//
// What the server feeds (feat/p9-zone-tier): per zone on GET /api/nodes and
// /api/status agents `tier` {index, priority, rule, headroom_mib,
// shared_memory, link_mb_s, settle} or `tier_reason`; per tier on GET
// /api/config/tiers `tier_zones` [{index, zones: [{id, name}],
// never_reached}]. The membership here is written out as the server gives
// it, never worked out from the rules. Names are generic.

const LIVE_TIERS = [
  { match: { platform: 'linux-amd64-vulkan' }, priority: 1, headroom_mib: 1024, shared_memory: true, link_mb_s: 100, settle: '3s' },
  { match: { hostname: 'alienware*' }, priority: 2, headroom_mib: 256, link_mb_s: 60, settle: '8s' },
  { match: { local: true }, priority: 3, headroom_mib: 450, settle: '2s' },
  { match: { hostname: 'laptop-*', platform: 'linux-amd64-cuda' }, priority: 4, headroom_mib: 500, link_mb_s: 10 },
];
const CATCHALL_TIERS = [{ match: {}, priority: 1, headroom_mib: 512, link_mb_s: 80, settle: '3s' }, ...LIVE_TIERS.map((t, i) => Object.assign({}, t, { priority: i + 2 }))];

// the names the live pool's two cuda laptops carry in these states
export const RENAME = { 'laptop-01': 'alienware-01', 'laptop-02': 'devbox-02' };
export const LOCAL = { id: 'local', name: 'scheduler-host' };

// tierLists(mode) -> the stored lists GET /api/config carries, or null
export function tierLists(mode) {
  if (mode !== 'live' && mode !== 'catchall') return null;
  return { tiers: mode === 'live' ? LIVE_TIERS : CATCHALL_TIERS, exclude_nodes: ['devbox-02', '02:00:00:00:00:77'], no_auto_nodes: ['alienware-01'], node_overrides: {} };
}

const settleOf = t => t.settle || '0s';
const facts = (tiers, index) => {
  const t = tiers[index - 1];
  return { index, priority: t.priority || 0, rule: Object.assign({}, t.match), headroom_mib: t.headroom_mib || 0, shared_memory: !!t.shared_memory, link_mb_s: t.link_mb_s || 0, settle: settleOf(t) };
};
// the tier the server put each zone in (by name), 0 for none
function indexOf(mode, name) {
  if (mode === 'catchall') return 1;
  if (/^ps5-0[1-6]$/.test(name)) return 1;
  if (name === 'alienware-01') return 2;
  if (name === LOCAL.name) return 3;
  return 0;
}

// applyTiers(st, mode): renames the laptops and stamps tier / tier_reason on
// the status agents (removed ones carry neither) and priorities on the view
export function applyTiers(st, mode) {
  const lists = tierLists(mode);
  if (!lists) return st;
  for (const a of st.agents || []) {
    if (RENAME[a.name]) a.name = RENAME[a.name];
    delete a.tier; delete a.tier_reason;
    if (a.state === 'removed') continue;
    const i = indexOf(mode, a.name);
    if (i) a.tier = facts(lists.tiers, i); else a.tier_reason = 'no tier matches';
  }
  for (const n of (st.view && st.view.nodes) || []) {
    if (RENAME[n.hostname]) n.hostname = RENAME[n.hostname];
    const i = indexOf(mode, n.hostname);
    n.priority = i ? lists.tiers[i - 1].priority : 0;
    if (n.local) n.local = false; // the scheduler's own machine is the separate local entry
  }
  return st;
}

// localEntry(mode) -> the local node's entry on GET /api/nodes
export function localEntry(mode) {
  const lists = tierLists(mode);
  if (!lists) return null;
  return { id: LOCAL.id, name: LOCAL.name, state: 'active', local: true, placeable: true, release: '0.1.9', capabilities: [], zone_state: 'free', attention_rank: 50, tier: facts(lists.tiers, indexOf(mode, LOCAL.name)) };
}

// tiersDoc(mode, st, revision) -> GET /api/config/tiers: the stored tiers and tier_zones
export function tiersDoc(mode, st, revision) {
  const lists = tierLists(mode);
  const tiers = lists ? lists.tiers : [];
  let blocked = false;
  const out = tiers.map((t, k) => {
    const z = { index: k + 1, zones: [] };
    if (blocked) z.never_reached = true;
    if (!Object.keys(t.match || {}).length) blocked = true;
    return z;
  });
  const members = (st.agents || []).filter(a => a.tier && (a.state === 'active' || a.state === 'lost')).map(a => ({ index: a.tier.index, id: a.id, name: a.name }));
  const l = localEntry(mode);
  if (l && l.tier) members.push({ index: l.tier.index, id: l.id, name: l.name });
  for (const m of members) if (out[m.index - 1]) out[m.index - 1].zones.push({ id: m.id, name: m.name });
  for (const z of out) z.zones.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  return { revision, tiers, tier_zones: out };
}
