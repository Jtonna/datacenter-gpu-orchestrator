// The facts that tell one hardware setup from another, in plain words: how the
// weights reached the nodes (load mode), which node was the host and which were
// workers, and whether a reload of the model can come from the file cache.
// Used by the Benchmarks screens (a step, a benchmark's runs, the aggregates)
// and the Models page (the plan line of a configuration).
//
// Reads: a plan's load_mode and reload_from_cache (GET /api/models,
// /api/profiles), and a benchmark step's roles and reload_from_cache.

import { h } from './dom.js';
import { num, DASH } from './format.js';

export const LOAD_MODE = {
  file_read: ['File read', 'The workers read the model file from the shared folder'],
  pushed: ['Pushed', 'The host sent the weights to the workers over the network'],
  local: ['Local', 'No worker: the model ran on the scheduler machine only'],
  not_recorded: ['Not recorded', 'Stored before the load mode was recorded'],
};
// loadModeText(mode) -> "File read"; '' when there is no mode.
export const loadModeText = mode => (mode ? (LOAD_MODE[mode] || [mode])[0] : '');
export const loadModeNode = mode => (mode ? h('span', { title: (LOAD_MODE[mode] || [])[1] }, loadModeText(mode)) : h('span.mut', 'Not recorded'));

// The tooltip of the file cache label: what the figure says, and what it
// does not promise.
export const RELOAD_TIP = 'How much of the model file can stay in the node\'s file cache next to the loaded model, so a later load can read it from memory instead of the disk or the network. After other large models the first reloads still read part of the file again.';

// The verdict (reload_from_cache[].verdict) is about what CAN stay cached,
// not a promise that the next load is warm.
const RELOAD_PREFIX = 'Can stay in the file cache: ';
const RELOAD_WORD = { full: 'all of it', partial: 'part of it', none: 'none', unknown: 'not known' };

// reloadAmount(r) -> "all of it", "about 85%", "none" or "not known".
export function reloadAmount(r) {
  if (!r || !r.verdict) return '';
  if (r.verdict === 'partial' && r.cached_pct != null) return `about ${num(r.cached_pct, 0)}%`;
  return RELOAD_WORD[r.verdict] || r.verdict;
}

// reloadText(r) -> "Can stay in the file cache: about 85%".
export const reloadText = r => (r && r.verdict ? RELOAD_PREFIX + reloadAmount(r) : '');

// cacheOccupied(r) -> true when the node's cache holds so much of other files
// now that the file share does not fit next to it (cache_occupied). Shown on
// its own line, never folded into the verdict: the cache figure cannot tell
// whether the cached data is this model's own file.
export const cacheOccupied = r => !!(r && r.cache_occupied && (r.verdict === 'full' || r.verdict === 'partial'));
export const occupiedText = name => `The cache on ${name} is occupied by other files now; expect the first reloads to be slower`;

// reloadLine(list, nameOf) -> one label per node of a plan, or null. When all
// nodes say the same, one label is shown for them all. A node whose cache is
// occupied by other files now gets a second line.
export function reloadLine(list, nameOf) {
  const rs = (list || []).filter(r => r && r.verdict);
  if (!rs.length) return null;
  const same = rs.every(r => reloadText(r) === reloadText(rs[0]));
  const name = r => (nameOf ? nameOf(r) : r.node || r.id);
  const text = same ? reloadText(rs[0]) + (rs.length > 1 ? ' on every node' : '')
    : RELOAD_PREFIX + rs.map(r => `${name(r)}: ${reloadAmount(r)}`).join(' · ');
  const occ = rs.filter(cacheOccupied).map(r => h('span.sub.warn-t', { title: 'The node reports ' + num(r.cached_now_mib, 0) + ' MiB in its file cache now' }, occupiedText(name(r))));
  return [h('span.sub', { title: RELOAD_TIP }, text), ...occ];
}

// rolesText(host, workers) -> "Host: host-1 · Workers: 2 (ps5-a, ps5-b)".
export function rolesText(host, workers) {
  const ws = workers || [];
  if (!host && !ws.length) return '';
  const parts = [];
  if (host) parts.push(`Host: ${host}`);
  parts.push(ws.length ? `Workers: ${ws.length} (${ws.join(', ')})` : 'Workers: 0');
  return parts.join(' · ');
}
export const rolesNode = (host, workers) => {
  const t = rolesText(host, workers);
  return t ? h('span', t) : h('span.mut', DASH);
};
