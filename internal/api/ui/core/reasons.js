// The scheduler's reason strings in plain words. Pure text in, text out (no
// DOM), so the mapping can be run against recorded strings.
//
// explain(raw) -> {short, text}: `text` is one or two plain sentences, `short`
// a few words for a table cell. The scheduler's own wording always stays
// available to the reader: callers put `raw` in a tooltip or behind "Details".
// A wording this file does not know is only tidied (prefix removed, capital,
// full stop), never guessed at.
//
// Every pattern below was written from a string the real system produced
// (GET /api/profiles plan.reason, /api/history requestRefused data.message,
// status wait_reason and last_refusals) or from the format string in
// internal/scheduler (why.go, forcedfit.go, memfit.go) and internal/placer.

import { poolText } from './words.js';

const nf1 = new Intl.NumberFormat('en-US', { minimumFractionDigits: 1, maximumFractionDigits: 1 });
// Memory at a glance is GB with one decimal (1 GB = 1,024 MiB); see README.
const gb = mib => nf1.format(Number(mib) / 1024) + ' GB';
const cap = s => (s ? s.charAt(0).toUpperCase() + s.slice(1) : s);
const stop = s => (/[.!?]$/.test(s) ? s : s + '.');
const names = l => (l.length <= 1 ? l.join('') : l.slice(0, -1).join(', ') + ' and ' + l[l.length - 1]);
const splitNames = s => String(s).split(/,\s*/).map(x => x.trim()).filter(Boolean);
const isAre = l => (l.length === 1 ? 'is' : 'are');

// A forced-placement refusal: one or more clauses about the same profile.
//   profile P: need N MiB (basis), but the nodes it is forced onto have only F MiB usable in total (usable U MiB on X (how); ...)
//   profile P: need N MiB (basis) split over the forced nodes, node X: expected share S MiB (need N MiB x 50% of the split x 1.15 skew margin) but usable U MiB (how)
function forcedFit(s) {
  if (!/^profile \S+: need \d+ MiB/.test(s)) return null;
  const need = Number(s.match(/need (\d+) MiB/)[1]);
  const total = s.match(/forced onto have only (\d+) MiB usable in total/);
  const nodes = [];
  for (const m of s.matchAll(/usable (\d+) MiB on (\S+) \(/g)) if (!nodes.some(n => n.name === m[2])) nodes.push({ name: m[2], usable: Number(m[1]) });
  const shares = [];
  for (const m of s.matchAll(/node (\S+): expected share (\d+) MiB \([^)]*\) but usable (\d+) MiB/g)) shares.push({ name: m[1], share: Number(m[2]), usable: Number(m[3]) });
  if (total) {
    const free = Number(total[1]);
    const where = nodes.length === 1 ? nodes[0].name : `its ${nodes.length || ''} nodes together`.replace('  ', ' ');
    return { short: `Needs ${gb(need)}, has ${gb(free)}`,
      text: `Needs about ${gb(need)}; ${where} can give ${gb(free)}.` };
  }
  if (shares.length) {
    const worst = shares.reduce((a, b) => (b.share - b.usable > a.share - a.usable ? b : a));
    const who = shares.length === 1 ? shares[0].name : names(shares.map(x => x.name));
    return { short: `Its share does not fit on ${shares.length === 1 ? shares[0].name : shares.length + ' nodes'}`,
      text: `Needs about ${gb(need)} split over its nodes. The share for ${who} would be ${gb(worst.share)} with the safety margin, and ${shares.length === 1 ? 'it has' : 'the tightest has'} ${gb(worst.usable)} free.` };
  }
  return null;
}

// One clause of a reason (the scheduler joins several with "; ").
function clause(s) {
  let m;
  if ((m = s.match(/^need (\d+) MiB \(requires (\d+) MiB\) exceeds cluster capacity (\d+) MiB(?:; nodes excluded by config and not counted: (.+))?$/i))) {
    const ex = m[4] ? splitNames(m[4]) : [];
    if (Number(m[3]) === 0) {
      return { short: 'No node can run it',
        text: 'No node can run it' + (ex.length ? `: ${names(ex)} ${isAre(ex)} excluded from use.` : '.') };
    }
    const withMargin = m[2] !== m[1] ? ` (${gb(m[2])} with the safety margins)` : '';
    return { short: 'Too big for the cluster',
      text: `Needs ${gb(m[1])}${withMargin}; the whole cluster holds ${gb(m[3])}.` + (ex.length ? ` Not counted, because excluded: ${names(ex)}.` : '') };
  }
  if ((m = s.match(/^need (\d+) MiB \(requires (\d+) MiB\) does not fit the available nodes$/i))) {
    return { short: 'No free node is big enough', text: `Needs ${gb(m[1])}${m[2] !== m[1] ? ` (${gb(m[2])} with the safety margins)` : ''}; the nodes that are free right now do not have that much.` };
  }
  if ((m = s.match(/^forced nodes? in use: (.+)$/i))) {
    const l = splitNames(m[1]);
    return { short: 'Its nodes are busy', text: `It is tied to ${names(l)}, which ${isAre(l)} busy with another model right now.` };
  }
  if ((m = s.match(/^forced nodes? unavailable: (.+)$/i))) {
    const l = splitNames(m[1]);
    return { short: l.length === 1 ? `${l[0]} is offline` : 'Its nodes are offline', text: `It is tied to ${names(l)}, which ${isAre(l)} offline or cannot be used right now.` };
  }
  if ((m = s.match(/^forced node (\S+) is not in the (fleet view|pool)$/i))) return { short: `${m[1]} is not known`, text: poolText(m[2].toLowerCase() === 'pool', `It is tied to ${m[1]}, which the fleet does not report.`) };
  if ((m = s.match(/^forced node (\S+): node (\S+) is excluded by config$/i))) return { short: `${m[2]} is excluded`, text: `It is tied to ${m[2]}, which is excluded from use.` };
  if ((m = s.match(/^forced node (\S+) is quarantined until (\S+)$/i))) return { short: `${m[1]} is set aside`, text: `It is tied to ${m[1]}, which is set aside until ${m[2]} after a failure.` };
  if ((m = s.match(/^nodes that would fit are quarantined until (\S+)$/i))) return { short: 'Nodes are set aside', text: `The nodes that could take it are set aside until ${m[1]} after a failure.` };
  if ((m = s.match(/^node (\S+) has no known address$/i))) return { short: `${m[1]} has no address`, text: `The worker address of ${m[1]} is not known, so it cannot be used.` };
  if ((m = s.match(/^node (\S+) is excluded by config$/i))) return { short: `${m[1]} is excluded`, text: `The node that could take it (${m[1]}) is excluded from use.`, excluded: m[1] };
  if ((m = s.match(/^joins the queued entry \((\d+) waiting, claimed (true|false)\)$/i))) {
    return { short: 'Already waiting', text: m[2] === 'true' ? 'Requests for it are already waiting and have claimed their nodes; this one would join them.' : 'Requests for it are already waiting for room; this one would join them.' };
  }
  if (/^a placement is available/i.test(s)) return { short: 'Starts at the next pass', text: 'A node is free; it starts at the next pass.' };
  if ((m = s.match(/^memory need unknown: (.+)$/i))) return { short: 'Need not known', text: 'Its memory need could not be worked out: ' + stop(m[1]) };
  if ((m = s.match(/^profile (\S+) is backed off after a failed load$/i)) || /^the profile is backed off/i.test(s)) return { short: 'Held back after a failure', text: 'Its last load failed, so it is held back for a while.' };
  if ((m = s.match(/^load stalled: no new bytes acknowledged for (\d+) ?s(?: \(([^)]*)\))?/i))) {
    return { short: 'The load stalled', text: `The load stalled: nothing arrived for ${m[1]} s${m[2] ? ` (${m[2]})` : ''}, so it was stopped.` };
  }
  if ((m = s.match(/^load cancelled: (\S+) ran low on memory during the load \(available (\d+) MiB, floor (\d+) MiB\)$/i))) {
    return { short: `${m[1]} ran low on memory`, text: `The load was stopped because ${m[1]} had only ${gb(m[2])} left (the floor is ${gb(m[3])}).` };
  }
  return null;
}

export function explain(raw) {
  let s = String(raw || '').trim();
  if (!s) return { short: '', text: '' };
  let lead = '';
  if (/^fleet view stale;\s*/i.test(s)) { lead = 'The fleet feed is stale, so remote nodes are not used. '; s = s.replace(/^fleet view stale;\s*/i, ''); }
  let m;
  if ((m = s.match(/^waited (\d+)s without a placement: (.+)$/i))) {
    const mins = Math.round(m[1] / 60), inner = explain(m[2]);
    return { short: inner.short || 'Waited too long', text: lead + `Waited ${mins >= 1 ? mins + ' min' : m[1] + ' s'} without finding room. ` + inner.text };
  }
  s = s.replace(/^placer:\s*/i, '').replace(/\s*\(defaults\.forced_fit_check is reject\)\.?$/i, '');
  const ff = forcedFit(s);
  if (ff) return { short: ff.short, text: lead + ff.text };
  const whole = clause(s);
  if (whole) return { short: whole.short, text: lead + whole.text };
  // several clauses joined with "; "
  const parts = s.split(/;\s+/).map(clause);
  if (parts.length > 1 && parts.every(Boolean)) {
    const ex = parts.filter(p => p.excluded).map(p => p.excluded);
    if (ex.length === parts.length) {
      return { short: 'Its nodes are excluded', text: lead + `The nodes that could take it (${names(ex)}) are excluded from use.` };
    }
    return { short: parts[0].short, text: lead + parts.map(p => p.text).join(' ') };
  }
  if (!s) return { short: '', text: lead.trim() };
  return { short: '', text: lead + stop(cap(s)) };
}

// plainReason(text) -> the plain sentence of a reason string.
export const plainReason = raw => explain(raw).text;
// shortReason(text) -> a few words for a table cell ('' when not known).
export const shortReason = raw => explain(raw).short;

// plainBasis(text) says where a memory need comes from (needs_basis, need_basis).
export function plainBasis(raw) {
  const s = String(raw || '').trim();
  let m;
  if ((m = s.match(/^estimated, \+(\d+(?:\.\d+)?)% margin; the estimate is (\d+) MiB$/i))) return `Estimated from the model file (${gb(m[2])}), plus a ${m[1]}% margin`;
  if (/^estimated/i.test(s)) return 'Estimated from the model file';
  if (/^measured, needs_mib$/i.test(s)) return 'Set by hand in the profile';
  if ((m = s.match(/^measured this run; (\d+) MiB before$/i))) return `Measured on its last load (it was ${gb(m[1])} before)`;
  if (/^measured this run/i.test(s)) return 'Measured on its last load';
  if ((m = s.match(/^measured on (\d{4}-\d{2}-\d{2}), restored at start/i))) return `Measured on ${m[1]}, kept across a restart`;
  return s ? cap(s) : '';
}

// refusalWord(reason) -> the short reason of a refused request (the `reason`
// of a requestRefused event or of status.last_refusals) in plain words.
const REFUSAL = {
  'forced nodes do not fit': 'It does not fit on the nodes it is tied to',
  'does not fit': 'It is too big for the cluster',
  'cluster busy': 'It waited too long for room',
  'forced nodes unavailable': 'The nodes it is tied to were not available',
  'fleet view stale': 'The fleet feed was stale',
  'load failed': 'Its load failed',
  backoff: 'It was held back after a failed load',
  'backed off': 'It was held back after a failed load',
  'shutting down': 'The scheduler was shutting down',
  'nodes quarantined': 'The nodes that could take it were set aside',
  'node has no address': 'A node it needs has no known address',
  'node excluded': 'A node it needs is excluded from use',
  unloaded: 'It was unloaded while it loaded',
  'client gone': 'The caller went away',
  'unknown model': 'No profile has that name',
};
export const refusalWord = reason => REFUSAL[String(reason || '').trim().toLowerCase()] || (reason ? stop(cap(String(reason))).replace(/\.$/, '') : 'Refused');
