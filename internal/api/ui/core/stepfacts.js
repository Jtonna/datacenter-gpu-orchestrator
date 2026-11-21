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

// The one-sentence tooltip of the reload label.
export const RELOAD_TIP = 'Whether the model file stays in the node\'s file cache after loading, so the next load reads it from memory instead of the disk or the network.';

const RELOAD_WORD = { full: 'full', partial: 'partial', none: 'none', unknown: 'unknown' };

// reloadText(r) -> "Reload from cache: partial, about 85% of the file".
export function reloadText(r) {
  if (!r || !r.verdict) return '';
  const w = RELOAD_WORD[r.verdict] || r.verdict;
  if (r.verdict === 'partial' && r.cached_pct != null) return `Reload from cache: ${w}, about ${num(r.cached_pct, 0)}% of the file`;
  return `Reload from cache: ${w}`;
}

// reloadLine(list, nameOf) -> one label per node of a plan, or null. When all
// nodes say the same, one label is shown for them all.
export function reloadLine(list, nameOf) {
  const rs = (list || []).filter(r => r && r.verdict);
  if (!rs.length) return null;
  const same = rs.every(r => reloadText(r) === reloadText(rs[0]));
  const name = r => (nameOf ? nameOf(r) : r.node || r.id);
  const text = same ? reloadText(rs[0]) + (rs.length > 1 ? ' on every node' : '')
    : rs.map(r => `${name(r)}: ${reloadText(r).replace('Reload from cache: ', '')}`).join(' · ');
  return h('span.sub', { title: RELOAD_TIP }, same ? text : 'Reload from cache: ' + text);
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
