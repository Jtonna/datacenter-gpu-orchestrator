// Helpers that read the status document (GET /api/status, the "status"
// WebSocket message). Field names are the server's; nothing is invented here.

import { h } from './dom.js';
import { badge } from './ui.js';
import { state } from './store.js';
import { agentOf, notPlaceable } from './nodeactions.js';

export const viewNodes = st => (st && st.view && st.view.nodes) || [];

// nodeName(st, id) -> the hostname of a node id, else the id.
export function nodeName(st, id) {
  const n = viewNodes(st).find(x => x.id === id);
  return (n && n.hostname) || id;
}
// zoneLabel(st, id, plan) -> a zone's name for a page, by the profile
// editor's rule (build-89): the plan's own name for that node
// (plan.memory.nodes[].node), else the zones' names (nodeName), else the id
// itself (a zone with no name)
export function zoneLabel(st, id, plan) {
  const own = ((plan && plan.memory && plan.memory.nodes) || []).find(n => n && n.id === id && n.node);
  return own ? own.node : nodeName(st, id);
}
// zoneChips(st, ids, plan) -> the chips of forced zones by their names, the
// id on hover only; a zone with no name shows its id, marked as such
// (the reviewer, build-92: a hardware address is never a column's text
// when the zone has a name)
export function zoneChips(st, ids, plan) {
  return h('span.nodes', (ids || []).map(id => {
    const name = zoneLabel(st, id, plan);
    return name !== id ? h('span.node', { title: id, 'data-zone': id }, name)
      : h('span.node.mono', { title: 'This zone has no name: its id', 'data-zone': id, 'data-noname': '' }, id);
  }));
}
export const findNode = (st, idOrHost) => viewNodes(st).find(x => x.id === idOrHost || x.hostname === idOrHost) || null;
// nodeByAddr(st, '192.0.2.21:50052') -> the view node with that rpc address.
export const nodeByAddr = (st, addr) => viewNodes(st).find(x => x.addr && x.addr === addr) || null;

// instanceNodeNames(st, inst) -> the hostnames an instance is placed on.
export const instanceNodeNames = (st, inst) => (inst.nodes || []).map(n => nodeName(st, n.id));

// nodeMemory(node) sums the load-log buffers of one placed node.
export function nodeMemory(placed) {
  const m = { model_mib: 0, kv_mib: 0, compute_mib: 0, total_mib: 0 };
  for (const d of placed.memory || []) {
    m.model_mib += d.model_mib || 0; m.kv_mib += d.kv_mib || 0; m.compute_mib += d.compute_mib || 0; m.total_mib += d.total_mib || 0;
  }
  return m;
}

// machines(st, n) -> "1 zone", "3 zones": always the pages' word for a
// machine, whatever the machines' host names are (the API's fields keep the
// word node). st stays in the signature so no caller changes.
export function machines(st, n) {
  return `${n} zone${n === 1 ? '' : 's'}`;
}

// instanceBadge(inst) -> the state badge of an instance.
export function instanceBadge(inst) {
  switch (inst.state) {
    case 'starting': return badge('Loading', 'load');
    case 'ready': return inst.in_flight > 0 ? badge('Ready · serving', 'ready') : badge('Ready', 'ready');
    case 'draining': return badge('Draining', 'settle', 'Finishing its requests, then it unloads');
    case 'stopping': return badge('Stopping', 'settle');
  }
  return badge(inst.state || 'Unknown', 'n');
}

// healthBadge(node) -> a badge for a worker that is not healthy, else null.
export function healthBadge(node) {
  const s = node && node.health && node.health.state;
  if (s === 'hung') return badge('Not answering', 'fail', 'The worker stopped answering (hung)');
  if (s === 'suspect') return badge('Slow to answer', 'load', 'One health channel is silent (suspect)');
  return null;
}

// machineBadge(st, node) -> in agents mode, a badge for a machine that takes
// no new model: "Paused", or "Not placeable" with the reason on hover. Null
// for a machine that may take one, and always null in fleet-feed mode.
export function machineBadge(st, node) {
  const np = notPlaceable(node, agentOf(st, node));
  if (!np) return null;
  if (np.paused && np.code === 'paused') return badge('Paused', 'settle', np.text);
  if (np.code === 'excluded') {
    const by = node.excluded_by || (agentOf(st, node) || {}).excluded_by;
    return badge('Excluded', 'dash', by ? 'excluded by exclude_nodes: ' + by : np.text);
  }
  return badge(np.paused ? 'Paused · not placeable' : 'Not placeable', 'aside', np.known ? np.text : 'The scheduler gives this reason: ' + np.code);
}

// drift() is how many seconds passed since the newest status arrived, for
// counters that should keep moving between two status messages (at most 5 s,
// and 0 while disconnected so stale data does not pretend to be live).
export function drift() {
  if (state.conn !== 'live' || !state.statusAt) return 0;
  return Math.min(5, Math.max(0, (Date.now() - state.statusAt) / 1000));
}

// staleBanner() used to draw the warning for a lost connection or a stale
// fleet feed inside each Monitor screen. The shell now shows it once, above
// every screen (core/shell.js, connection()), so this returns nothing; the
// calls stay where the warning used to sit.
export function staleBanner() { return null; }

// The reason strings in plain words live in reasons.js (no DOM, testable).
export { plainReason, shortReason, explain, plainBasis, refusalWord } from './reasons.js';
// Agent machines: the reason words and the two operator actions (no DOM).
export { agentsMode, agentOf, notPlaceable, unplacedReason } from './nodeactions.js';
