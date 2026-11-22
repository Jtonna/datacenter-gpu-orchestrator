// Start-up figures of agent machines: the words, and the records of
// GET /api/startups pulled on demand (no DOM).
//
// The figure: seconds from boot start to ready for model loads. Boot start
// is the start of the Linux kernel for a full start and the start of
// userspace for a soft restart; an agent restart (the machine kept running)
// has no figure. Kinds are never put into one figure.

import { unplacedReason } from './nodeactions.js';

// The sentence shown wherever a figure is shown (the server sends the same
// one as not_included).
export const NOT_INCLUDED = 'On a console the time before the Linux kernel starts cannot be seen from inside Linux and is not included; none of these is a time from power-on.';

export const KIND = {
  full: 'full start',
  soft: 'soft restart',
  agent_restart: 'agent restart',
};
export const kindLabel = k => KIND[k] || k;

// The conditions of "ready for model loads", in plain words.
const MISSING = {
  registered: 'the agent has registered',
  programs_ready: 'its programs are ready',
  share_mounted: 'the model share is reachable',
  userspace_start: 'systemd says when userspace started',
};

// missingText('registered, share_mounted') -> 'the agent has registered and the model share is reachable'
export function missingText(list) {
  const parts = String(list || '').split(',').map(s => s.trim()).filter(Boolean).map(s => MISSING[s] || s);
  return parts.length <= 1 ? parts.join('') : parts.slice(0, -1).join(', ') + ' and ' + parts[parts.length - 1];
}

// The marks, in plain words; a name this file does not know is shown as it is.
const MARK = {
  agent_start: 'Agent started',
  registered: 'Registered with Potluck',
  share_mounted: 'Model share reachable',
  programs_ready: 'Programs ready',
  exporter_up: 'Exporters running',
  first_stats: 'First stats read',
  boot_progress_start: 'Boot progress started',
  network_ok: 'Network up',
  payload_ok: 'Payload loaded',
  boot_ready: 'Boot finished',
  dashboard_up: 'Dashboard up',
};
export const markLabel = name => MARK[name] || name;

// secs(41.25) -> '41.3 s'; whole seconds above 100.
export function secs(v) {
  if (v == null || Number.isNaN(v)) return '';
  return (v >= 100 ? Math.round(v) : Math.round(v * 10) / 10) + ' s';
}

// startupText(last) -> {head, kind, extra} for a node's last_startup: the
// Nodes page shows "<head> (<kind>), <when>. <extra>". Never a partial figure:
// without one it names what is missing or what holds it back.
export function startupText(last) {
  if (!last) return null;
  const guessed = last.inferred ? 'The kind is guessed: there was no earlier start to compare with.' : '';
  const join = (...xs) => xs.filter(Boolean).join(' ');
  if (last.kind === 'agent_restart') {
    return { head: 'Agent restarted', kind: '', extra: join('The machine kept running, so there is no start-up figure.', guessed) };
  }
  const kind = kindLabel(last.kind);
  if (last.headline_s != null) {
    return { head: `Ready for model loads ${secs(last.headline_s)} after boot start`, kind, extra: guessed };
  }
  const head = last.open ? 'Not ready for model loads yet' : 'No start-up figure';
  if (last.blocked_by) {
    const r = unplacedReason(last.blocked_by);
    return { head, kind, extra: join(`Held back: ${r.short.toLowerCase()}.`, guessed) };
  }
  return { head, kind, extra: join(`${last.open ? 'Waiting until' : 'Never ready: the record closed while waiting until'} ${missingText(last.missing)}.`, guessed) };
}

// requestedText(record) -> the note of a start announced with expect-restart.
export function requestedText(r) {
  if (!r || !r.requested) return '';
  const away = r.away_s != null ? `; away ${secs(r.away_s)}` : '';
  const ready = r.away_to_ready_s != null ? `, ready ${secs(r.away_to_ready_s)} after the call` : '';
  return `Requested with expect-restart (${kindLabel(r.requested_kind)})${away}${ready}.`;
}

// ---------- records of GET /api/startups, pulled when a view asks ----------

export const startups = { groups: null, error: null, off: false };
const subs = new Set();
export const onStartups = fn => { subs.add(fn); return () => subs.delete(fn); };

// loadStartups(api) pulls the figures once more. A 503 means the scheduler
// takes its nodes from the fleet feed: there are no agents, so no figures.
export async function loadStartups(api) {
  try {
    const r = await api.get('/api/startups');
    startups.groups = r.groups || [];
    startups.error = null;
    startups.off = false;
  } catch (e) {
    if (e && e.status === 503) startups.off = true;
    else startups.error = e;
  }
  for (const fn of Array.from(subs)) { try { fn(); } catch (e) { console.error(e); } }
}

// lastRecord(nodeId, kind) -> the newest record of that node and kind, or null.
export function lastRecord(nodeId, kind) {
  const g = (startups.groups || []).find(x => x.node_id === nodeId && x.kind === kind);
  return g && g.last ? g.last : null;
}
