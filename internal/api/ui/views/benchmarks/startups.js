// Start-up: per machine and kind of start, how many starts, the last, the
// median and the worst figure (seconds from boot start to ready for model
// loads). One row per machine and kind: a full start, a soft restart and an
// agent restart are never put into one figure.
//
// Reads: GET /api/startups (core/startup.js), pulled with the overview.

import { h } from '../../core/dom.js';
import { table, empty, skeleton } from '../../core/ui.js';
import { num, ago, DASH } from '../../core/format.js';
import { startups, kindLabel, secs, originText, NOT_INCLUDED } from '../../core/startup.js';

const fig = v => (v == null ? h('span.mut', DASH) : h('span.mono', secs(v)));

export function startupsCard() {
  if (startups.off) return null; // fleet-feed membership: no agents, no figures
  const groups = startups.groups;
  let body;
  if (startups.error && !groups) body = empty('The start-up figures could not be read', startups.error.message);
  else if (!groups) body = skeleton(2);
  else if (!groups.length) body = empty('No starts recorded yet', 'A machine reports its start-up when its agent starts. Restart one and its figure appears here.');
  else body = table({
    minWidth: 820, rows: groups, key: g => g.node_id + ':' + g.kind,
    columns: [
      { label: 'Machine', render: g => h('span', h('span.mono', { style: 'font-weight:700' }, g.node_name || g.node_id), h('span.sub.mono', g.node_id)) },
      { label: 'Kind', cls: 'tight', render: g => kindLabel(g.kind) },
      { label: 'Starts', cls: 'r', title: 'Starts of this kind; those with a figure in brackets', render: g => [num(g.count), g.kind !== 'agent_restart' && g.with_figure !== g.count ? h('span.mut', ` (${num(g.with_figure)})`) : null] },
      { label: 'Last', cls: 'r', render: g => (g.kind === 'agent_restart' ? h('span.mut', 'no figure')
        : h('span', { title: originText(g.origin) || null }, fig(g.last_s), g.mixed_origins ? h('span.sub', 'counted from different origins') : null)) },
      { label: 'Median', cls: 'r', render: g => (g.kind === 'agent_restart' ? null : fig(g.median_s)) },
      { label: 'Worst', cls: 'r', render: g => (g.kind === 'agent_restart' ? null : fig(g.worst_s)) },
      { label: 'Latest start', cls: 'tight', render: g => {
        const r = g.last && g.last.record;
        if (!r) return h('span.mut', DASH);
        return h('span', ago(r.registered_at), r.requested ? h('span.sub', 'Requested with expect-restart') : null,
          !r.headline_s && g.last.condition ? h('span.sub', g.last.condition.replace(/_/g, ' ')) : null);
      } },
    ],
  });
  const requested = (groups || []).reduce((s, g) => s + (g.requested || 0), 0);
  return h('section.card', { key: 'startups' },
    h('div.ch', h('h2', 'Start-up'), h('span.note', 'Seconds from boot start to ready for model loads: from the kernel start for a full start; for a soft restart from the moment the restart began when the console side reports it, else from the userspace start.')),
    body,
    h('div.note-s', { style: 'padding-top:10px' }, NOT_INCLUDED,
      requested ? ` ${num(requested)} of these figures come from restarts announced with expect-restart (the node was paused and nothing ran on it).` : ''));
}
