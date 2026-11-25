// The memory reservation rule in the UI: a node's reserved list (expected
// against measured) on the Nodes page, and the `memory` table of a plan on
// the Placement preview.
//
// Reads, from the WebSocket "status": each view node's reserved,
// reserved_mib, measured_used_mib, drift_mib, drift_warning,
// free_for_new_load_mib and free_avg_mib. From POST /api/plan: the plan's
// memory section. Server objects are read only through the names nd (a view
// node), res (one reservation), mem (the memory section), row (one node of
// it) and by (one reserved_by entry); ui_reserve_test.go checks every key
// read through those names against the keys the server writes.

import { h } from './dom.js';
import { badge, table } from './ui.js';
import { mib, gb, ctx, DASH } from './format.js';

// figuresLine(row) -> "available A − floor F − headroom H = placeable P;
// after this load about L left" for a shared-memory row (the server's own
// figures, nothing recomputed but the sign of the adjustment), else null.
function figuresLine(row) {
  if (typeof row.mem_available_mib !== 'number') return null;
  const avg = row.mem_available_reading === 'average';
  const a = avg ? row.mem_available_avg_mib : row.mem_available_mib;
  const adj = row.placeable_adjust_mib;
  return h('div.note-s', { key: 'fig-' + row.id },
    h('span.mono', row.node || row.id), ': available ', mib(a), avg ? ' (average)' : '', ' − floor ', mib(row.watchdog_floor_mib),
    ' − headroom ', mib(row.headroom_mib),
    typeof adj === 'number' ? [adj < 0 ? ' − ' : ' + ', mib(Math.abs(adj)), ' (', row.placeable_adjust_why, ')'] : null,
    ' = placeable ', mib(row.placeable_mib),
    typeof row.predicted_lowest_available_mib === 'number' ? ['; after this load about ', mib(row.predicted_lowest_available_mib), ' left (available now ', mib(row.mem_available_mib), ' less what it will take)'] : null);
}

const BASIS = { measured: 'measured on a load', peak: 'stored peak', estimated: 'estimate' };
const basisText = b => BASIS[b] || b || DASH;

// reservedPart(nd) -> the reservation lines of a node tile, or null when the
// server sends none (an older scheduler) and nothing is reserved or known.
export function reservedPart(nd) {
  if (!nd || !Array.isArray(nd.reserved)) return null;
  const lines = nd.reserved.map(res => h('div.memline', { key: 'res-' + res.profile },
    h('span', { title: `weights ${mib(res.weights_mib)} + context ${mib(res.context_mib)} at ${res.ctx} tokens + fill reserve ${mib(res.fill_reserve_mib)} (${basisText(res.basis)}; ${res.state})` },
      'Reserved for ', h('span.mono', res.profile), ': ', mib(res.weights_mib), ' weights + ', mib(res.context_mib), ' context at ', ctx(res.ctx),
      ' tokens', res.fill_reserve_mib ? [' + ', mib(res.fill_reserve_mib), ' fill reserve'] : null),
    h('span.mono', mib(res.total_mib))));
  const measured = typeof nd.measured_used_mib === 'number';
  const drift = typeof nd.drift_mib === 'number' ? nd.drift_mib : null;
  const freeNew = typeof nd.free_for_new_load_mib === 'number' ? nd.free_for_new_load_mib : null;
  const avg = typeof nd.free_avg_mib === 'number' ? nd.free_avg_mib : null;
  if (!lines.length && freeNew == null) return null;
  return h('div', { style: 'display:flex;flex-direction:column;gap:3px', key: 'reserve' },
    lines,
    lines.length ? h('div.memline', h('span', 'Expected ', h('b', mib(nd.reserved_mib)), ' · measured ', h('b', measured ? mib(nd.measured_used_mib) : 'not known yet')),
      drift != null ? h('span', { class: nd.drift_warning ? 'err-t' : 'mut', title: nd.drift_warning || 'measured less expected' },
        (drift > 0 ? '+' : '') + mib(drift), nd.drift_warning ? [' ', badge('Above its reservation', 'aside', nd.drift_warning)] : null) : null) : null,
    freeNew != null ? h('div.memline', h('span', 'Free for a new load'), h('span.mono', { title: avg != null ? `average free over the window: ${mib(avg)}` : null }, mib(Math.max(0, freeNew)))) : null);
}

// memoryTable(mem, name) -> the plan's memory section as a table: per node
// what the load will take with its context filled, what the node has, what
// is reserved there and what remains. name(id) gives a node's display name
// (short_node is already a label: shown as it is).
export function memoryTable(mem, name) {
  if (!mem || !Array.isArray(mem.nodes)) return null;
  const head = mem.fits ? badge('Fits', 'ready') : badge('Does not fit', 'fail');
  const short = !mem.fits && mem.short_node ? `${mem.short_node} is short by ${mib(mem.shortfall_mib)}.` : '';
  // A tight fit: the load fits a node without its margin only, so it starts
  // with less than the margin spare there (neutral: it is allowed).
  const tightRows = mem.nodes.filter(row => row.tight);
  const tight = mem.tight_fit && tightRows.length
    ? tightRows.map(row => `${row.node || name(row.id)}: ${mib(row.spare_mib)} spare, ${mib(row.margin_short_mib)} short of its margin of ${mib(row.margin_mib)}.`).join(' ')
    : '';
  return h('div', { style: 'display:flex;flex-direction:column;gap:8px', key: 'memory' },
    h('div.day', 'Memory, with the context filled'),
    h('div.cb', { style: 'display:flex;gap:8px;align-items:center;flex-wrap:wrap' }, head,
      tight ? badge('Tight fit', 'n', 'It fits without the margin only; it starts anyway (defaults.margin_refuses is off).') : null,
      tight ? h('span.note', tight) : null,
      mem.hypothetical ? badge('No placement now: the nodes it would be spread over', 'n') : null,
      mem.decides ? null : badge('Shown only: the default rules decide', 'n'),
      h('span.note', short, mem.placement_uses_reserved ? ' Available follows the reservation rule (the lower of free now and its average, less what is reserved and not measured yet).' : ' Available is free now less headroom.')),
    table({
      minWidth: 900,
      key: row => row.id,
      rowClass: row => (row.remains_mib < 0 && !row.tight ? 'flag-err' : ''),
      empty: h('div.cb.mut', 'No node can take a share of this profile now.'),
      rows: mem.nodes,
      columns: [
        { label: 'Node', render: row => h('span', h('span.name.mono', row.node || name(row.id)), row.shared ? h('span.sub', 'shared memory') : null) },
        { label: 'Weights', cls: 'r', render: row => mib(row.weights_mib) },
        { label: 'Context', cls: 'r', render: row => h('span', mib(row.context_mib),
          row.host_memory_mib ? h('span.sub', { 'data-hostmem': '' }, 'incl. ' + mib(row.host_memory_mib) + ' conversation memory of the host') : null) },
        { label: 'Fill reserve', cls: 'r', render: row => mib(row.fill_reserve_mib) },
        { label: 'Will take', cls: 'r', title: 'weights + context + fill reserve, or the stored peak', render: row => h('span', mib(row.will_take_mib), h('span.sub', basisText(row.basis))) },
        { label: 'Free now', cls: 'r', render: row => h('span', mib(row.free_now_mib), row.free_avg_known ? h('span.sub', 'avg ' + mib(row.free_avg_mib)) : null) },
        { label: 'Reserved there', render: row => (row.reserved_by || []).length
          ? h('span', (row.reserved_by || []).map((by, i) => [i ? ', ' : '', h('span.mono', by.profile), ' ', mib(by.mib), by.state ? h('span.quiet', ' ' + by.state) : null]))
          : h('span.quiet', 'nothing') },
        { label: 'Headroom', cls: 'r', render: row => mib(row.headroom_mib) },
        { label: 'Available', cls: 'r', render: row => mib(row.available_mib) },
        { label: 'Margin', cls: 'r', render: row => h('span', { title: row.margin_rule || null }, mib(row.margin_mib), row.margin_rule ? h('span.sub', row.margin_rule) : null) },
        { label: 'Remains', cls: 'r', render: row => h('span', { class: row.remains_mib < 0 && !row.tight ? 'err-t' : null }, mib(row.remains_mib),
          row.tight ? h('span.sub', `tight: ${mib(row.spare_mib)} spare`) : null) },
      ],
    }),
    mem.nodes.map(figuresLine),
    mem.note ? h('div.note-s', mem.note.replace(/^./, c => c.toUpperCase()) + '.') : null,
    h('div.note-s', { title: 'GB = 1,024 MiB' }, mem.nodes.length ? `In total it would take ${gb(mem.nodes.reduce((s, row) => s + (row.will_take_mib || 0), 0))}.` : ''));
}
