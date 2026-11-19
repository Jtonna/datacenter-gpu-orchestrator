// Helpers that read the status document (GET /api/status, the "status"
// WebSocket message). Field names are the server's; nothing is invented here.

import { h } from './dom.js';
import { badge } from './ui.js';
import { state } from './store.js';

export const viewNodes = st => (st && st.view && st.view.nodes) || [];

// nodeName(st, id) -> the hostname of a node id, else the id.
export function nodeName(st, id) {
  const n = viewNodes(st).find(x => x.id === id);
  return (n && n.hostname) || id;
}
export const findNode = (st, idOrHost) => viewNodes(st).find(x => x.id === idOrHost || x.hostname === idOrHost) || null;
// nodeByAddr(st, '10.0.0.21:50052') -> the view node with that rpc address.
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

// drift() is how many seconds passed since the newest status arrived, for
// counters that should keep moving between two status messages (at most 5 s,
// and 0 while disconnected so stale data does not pretend to be live).
export function drift() {
  if (state.conn !== 'live' || !state.statusAt) return 0;
  return Math.min(5, Math.max(0, (Date.now() - state.statusAt) / 1000));
}

// staleBanner() -> a banner when the page shows old or degraded data, else null.
export function staleBanner() {
  const st = state.status;
  if (state.conn === 'reconnecting') {
    return h('div.banner.warn', { role: 'status' }, 'The connection to the scheduler was lost. What you see is the last known state; it updates again once the connection is back.');
  }
  if (st && st.shutting_down) return h('div.banner.warn', { role: 'status' }, 'The scheduler is shutting down. It finishes the requests in flight and takes no new ones.');
  if (st && st.view && st.view.stale) {
    return h('div.banner.warn', { role: 'status' }, 'The fleet feed is stale. Remote nodes are not used for new loads until it recovers; models that are already loaded keep serving.');
  }
  return null;
}

// plainReason(text) turns the scheduler's reason strings (wait_reason, a
// plan's reason) into a plain sentence. The scheduler's own words stay
// available: callers put the raw text in a tooltip. An unknown wording is
// only tidied (prefix removed, capital, full stop), never guessed at.
export function plainReason(raw) {
  let s = String(raw || '').trim();
  if (!s) return '';
  let stale = '';
  if (/^fleet view stale;\s*/i.test(s)) { stale = 'The fleet feed is stale, so remote nodes are not used. '; s = s.replace(/^fleet view stale;\s*/i, ''); }
  s = s.replace(/^placer:\s*/i, '');
  const n = v => Number(v).toLocaleString('en-US');
  let m;
  if ((m = s.match(/^need (\d+) MiB \(requires (\d+) MiB\) exceeds cluster capacity (\d+) MiB(?:; nodes excluded by config and not counted: (.+))?$/i))) {
    return stale + `Needs ${n(m[1])} MiB` + (m[2] !== m[1] ? ` (${n(m[2])} MiB with the safety margins)` : '') + `, more than the whole cluster can hold (${n(m[3])} MiB)` +
      (m[4] ? `. Not counted because they are excluded: ${m[4]}.` : '.');
  }
  if ((m = s.match(/^need (\d+) MiB \(requires (\d+) MiB\) does not fit the available nodes$/i))) {
    return stale + `Needs ${n(m[1])} MiB` + (m[2] !== m[1] ? ` (${n(m[2])} MiB with the safety margins)` : '') + '; the nodes that are free now do not have that much.';
  }
  if ((m = s.match(/^forced nodes? in use: (.+)$/i))) return stale + `It is tied to ${m[1]}, which ${m[1].includes(',') ? 'are' : 'is'} in use by another model.`;
  if ((m = s.match(/^joins the queued entry \((\d+) waiting, claimed (true|false)\)$/i))) {
    return stale + (m[2] === 'true' ? 'Requests for it are already waiting and have claimed their nodes; this one would join them.' : 'Requests for it are already waiting for room; this one would join them.');
  }
  if ((m = s.match(/^a placement is available/i))) return stale + 'A node is free; it starts at the next pass.';
  if (!s) return stale.trim();
  s = s.charAt(0).toUpperCase() + s.slice(1);
  return stale + (/[.!?]$/.test(s) ? s : s + '.');
}
