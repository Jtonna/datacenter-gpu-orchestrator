// Activity: the event history as decisions in plain language, with a type
// filter and a time range, incidents (warnings and errors) marked, the loads
// that finished, and the last requests the scheduler served. Read-only.
//
// Reads: REST GET /api/history (the list; again when a filter changes or
// "Show earlier" is pressed), GET /api/history/summary (counts and incidents),
// GET /api/history?type=loadFinished (the loads table), GET /api/metrics/activity
// (recent requests; again when the served count in the status changes).
// WebSocket "event" adds new events to the top as they happen.
// Route: #/activity?range=6h&type=loads&sev=warn&q=text&session=<load session id>

import { h } from '../core/dom.js';
import { registerView, navigate } from '../core/router.js';
import { state, on, listen } from '../core/store.js';
import * as api from '../core/api.js';
import { liveView, pageHead, badge, nodeChips, chips, stat, table, empty, skeleton, button } from '../core/ui.js';
import { num, gb, bytes, tps, dur, plural, now, dayLabel, dateTime, agoText } from '../core/format.js';
import { plainReason } from '../core/status.js';

const RANGES = [['1h', '1 h', 3600], ['6h', '6 h', 21600], ['24h', '24 h', 86400], ['7d', '7 d', 604800]];
const GROUPS = {
  loads: ['Loads', ['loadStarted', 'loadFinished', 'loadStalled', 'loadCancelledLowMemory', 'preflightFailed', 'learnedNeed', 'needsMibTooLow']],
  unloads: ['Unloads', ['instanceUnloaded', 'childExited', 'eviction']],
  refusals: ['Refusals', ['requestRefused', 'profileBackedOff']],
  nodes: ['Nodes', ['nodeLost', 'nodeQuarantined', 'quarantineEnded', 'workerHealth', 'recoveryRequest', 'recoveryResponse', 'recoveryOutcome', 'recoverySequenceStopped',
    'fleetViewStale', 'fleetViewRecovered', 'fleetFeedDisconnected', 'fleetFeedRecovered']],
  benchmarks: ['Benchmarks', ['benchRunStarted', 'benchStepFinished', 'benchRunFinished']],
  system: ['Scheduler', ['schedulerStarted', 'schedulerStopping', 'internalFailure']],
  memory: ['Memory samples', ['nodeMemory']],
};
// "All" leaves out the periodic memory samples (one a minute per node); they have their own chip.
const ALL_TYPES = Object.keys(GROUPS).filter(g => g !== 'memory').flatMap(g => GROUPS[g][1]);

// tagOf(event) -> [label, badge kind]
function tagOf(e) {
  const d = e.data || {};
  switch (e.type) {
    case 'loadStarted': return ['Load', 'load'];
    case 'loadFinished': return d.outcome === 'ready' ? ['Ready', 'ready'] : d.outcome === 'cancelled' ? ['Load cancelled', 'aside'] : ['Load failed', 'fail'];
    case 'loadStalled': return ['Load stalled', 'fail'];
    case 'loadCancelledLowMemory': return ['Low memory', 'fail'];
    case 'requestRefused': return ['Refused', 'aside'];
    case 'profileBackedOff': return ['Held back', 'aside'];
    case 'nodeLost': return ['Node lost', 'fail'];
    case 'eviction': return ['Eviction', 'q'];
    case 'instanceUnloaded': return ['Unload', 'settle'];
    case 'childExited': return ['Crashed', 'fail'];
    case 'workerHealth': return d.state === 'hung' ? ['Not answering', 'fail'] : d.state === 'suspect' ? ['Slow to answer', 'load'] : ['Healthy again', 'use'];
    case 'nodeQuarantined': return ['Set aside', 'aside'];
    case 'quarantineEnded': return ['Back in use', 'use'];
    case 'recoveryRequest': case 'recoveryResponse': case 'recoveryOutcome': return ['Recovery', 'n'];
    case 'recoverySequenceStopped': return ['Recovery stopped', 'aside'];
    case 'fleetViewStale': case 'fleetFeedDisconnected': return ['Fleet feed', 'load'];
    case 'fleetViewRecovered': case 'fleetFeedRecovered': return ['Fleet feed', 'use'];
    case 'preflightFailed': return ['Check failed', 'aside'];
    case 'learnedNeed': return ['Measured', 'line'];
    case 'needsMibTooLow': return ['Needs too low', 'load'];
    case 'nodeMemory': return ['Memory', 'n'];
    case 'schedulerStarted': case 'schedulerStopping': return ['Scheduler', 'n'];
    case 'internalFailure': return d.escalating ? ['Internal failure, stopping', 'fail'] : ['Internal failure', 'fail'];
    case 'benchRunStarted': case 'benchStepFinished': case 'benchRunFinished': return ['Benchmark', 'line'];
  }
  return [e.type, 'n'];
}

// detailOf(event) -> a second, muted line built from the event's data keys.
function detailOf(e) {
  const d = e.data || {};
  const parts = [];
  switch (e.type) {
    case 'loadStarted':
      if (d.bytes_total) parts.push(bytes(d.bytes_total) + ' to upload');
      if (d.need_mib) parts.push('needs ' + gb(d.need_mib) + (/^estimated/.test(d.need_basis || '') ? ' (estimated)' : ''));
      break;
    case 'loadFinished':
      if (d.bytes) parts.push(bytes(d.bytes) + ' uploaded');
      if (d.outcome !== 'ready' && d.load_s != null) parts.push('after ' + dur(d.load_s));
      break;
    case 'loadStalled':
      parts.push(`no new bytes for ${dur(d.stalled_s)} (the limit is ${dur(d.limit_s)})`);
      if (d.bytes_total) parts.push(`${bytes(d.bytes_sent || 0)} of ${bytes(d.bytes_total)} sent`);
      break;
    case 'loadCancelledLowMemory':
      parts.push(`${gb(d.available_mib)} available, the floor is ${gb(d.floor_mib)}`);
      break;
    case 'requestRefused':
      if (d.waiters > 1) parts.push(plural(d.waiters, 'request'));
      if (d.suppressed_since_last) parts.push(`${num(d.suppressed_since_last)} more since the last record`);
      if (d.retry_after_s) parts.push('retry after ' + dur(d.retry_after_s));
      break;
    case 'instanceUnloaded':
      if (d.reason) parts.push({ ttl: 'idle too long', api: 'asked through the API', eviction: 'evicted to make room', shutdown: 'the scheduler stopped' }[d.reason] || d.reason);
      break;
    case 'eviction':
      if (d.for_profile) parts.push('to make room for ' + d.for_profile);
      if (d.waited_s != null) parts.push('which had waited ' + dur(d.waited_s));
      if (d.victim_busy) parts.push('it was busy, so it drains first');
      break;
    case 'nodeQuarantined': case 'profileBackedOff':
      if (d.for_s != null) parts.push('for ' + dur(d.for_s));
      if (d.level > 1) parts.push(`failure ${d.level} in a row`);
      if (d.cause) parts.push(d.cause);
      break;
    case 'nodeLost':
      if (d.missing_for_s != null) parts.push('missing for ' + dur(d.missing_for_s));
      if (d.in_flight) parts.push(plural(d.in_flight, 'request') + ' were in flight');
      break;
    case 'preflightFailed':
      if (d.err) parts.push(d.err);
      break;
    case 'needsMibTooLow':
      break;
    case 'nodeMemory':
      if (d.budget_mib != null) parts.push(`${gb(d.budget_mib)} usable for a model`);
      break;
    case 'recoveryRequest':
      if (d.step) parts.push(`step ${d.step} of ${d.of}`);
      if (d.reason) parts.push(d.reason);
      break;
    case 'recoveryOutcome':
      if (d.duration_ms) parts.push('took ' + dur(d.duration_ms / 1000));
      if (d.reason) parts.push(d.reason);
      break;
  }
  if (d.bench_run && !e.type.startsWith('bench')) parts.push('part of a benchmark run');
  return parts.join(' · ');
}

// mbps(x) -> an upload speed in words, or '' when it was not measured (absent,
// zero, not a number): never "0".
const mbps = x => (typeof x === 'number' && isFinite(x) && x > 0 ? (x < 10 ? num(x, 1) : num(x)) : '');

// learnedSentence: the learnedNeed events in plain words. The scheduler
// records several kinds under this one type: a model's measured memory need,
// a measured host or per-device overhead (in MiB, small), a reading out of
// range (not used), the higher of two host readings kept, and a need raised
// after a worker was ended. Each is told from its data; anything else: the
// scheduler's own summary.
function learnedSentence(e, d, p) {
  const on = d.node ? [' on ', h('span.mono', d.node)] : [];
  const devs = d.devices ? ` with ${plural(d.devices, 'device')}` : '';
  const mib = x => num(x) + ' MiB';
  if (d.measured_mib != null) return ['Measured ', p, ': it needs ', gb(d.measured_mib), '.'];
  if (d.host_overhead_mib != null) return ['Measured how much memory ', p, ' takes on its host', on, devs, ' beyond the model: ', mib(d.host_overhead_mib),
    d.in_force_mib != null && d.in_force_mib !== d.host_overhead_mib ? `. ${mib(d.in_force_mib)} is used from now on` : '', '.'];
  if (d.device_overhead_mib != null) return ['Measured how much extra memory ', p, ' takes on each device', on, devs, ': ', mib(d.device_overhead_mib),
    d.in_force_mib != null && d.in_force_mib !== d.device_overhead_mib ? `. ${mib(d.in_force_mib)} is used from now on` : '', '.'];
  if (d.host_overhead_reading_mib != null) return ['A host memory reading of ', p, on, ' was ', mib(d.host_overhead_reading_mib), ', which cannot be right: it was not used.'];
  if (d.device_overhead_reading_mib != null) return ['A per-device memory reading of ', p, on, ' was ', mib(d.device_overhead_reading_mib), ', which cannot be right: it was not used.'];
  if (d.kept_mib != null) return ['Kept the higher of the host memory readings', on, devs, ': ', mib(d.kept_mib), d.was_mib != null ? `, not ${mib(d.was_mib)}` : '', '.'];
  if (d.to_mib != null) return ['The memory need of ', p, ' was raised', d.from_mib != null ? ` from ${gb(d.from_mib)}` : '', ` to ${gb(d.to_mib)}`,
    ' after its worker', on, ' was ended', d.exit ? ` (${String(d.exit).replace(/_/g, ' ')})` : '', '.'];
  return [e.summary || 'Measured ' + (e.profile || 'a model') + '.'];
}

// sentenceOf(event) -> the event as one short plain sentence. The scheduler's
// own summary (kept in the record, behind "Details") is used for the types that
// have no shorter wording here.
function sentenceOf(e) {
  const d = e.data || {};
  const p = e.profile ? h('span.mono', e.profile) : null;
  switch (e.type) {
    case 'loadStarted': return ['Loading ', p, '.'];
    case 'loadFinished':
      if (d.outcome === 'ready') return [p, ' is ready: loaded in ', dur(d.load_s), mbps(d.avg_mb_s) ? ` at ${mbps(d.avg_mb_s)} MB/s` : '', '.'];
      return [p, d.outcome === 'cancelled' ? ' stopped loading' : ' failed to load', d.load_s != null ? ` after ${dur(d.load_s)}` : '', d.reason ? ['. ', plainReason(d.reason)] : '.'];
    case 'requestRefused': return ['A request for ', p, ' was refused. ', plainReason(d.message || d.reason)];
    case 'instanceUnloaded': return [p, ' was unloaded', d.loaded_for_s != null ? ` after ${dur(d.loaded_for_s)}` : '', d.requests_served != null ? ` and ${plural(d.requests_served, 'request')}` : '', '.'];
    case 'learnedNeed': return learnedSentence(e, d, p);
    case 'needsMibTooLow': return ['The memory need set for ', p, ' is too low: ', gb(d.configured_mib), ' set, ', gb(d.measured_mib), ' measured. The measured figure is used.'];
    case 'nodeMemory': return [gb(d.available_mib), ' free of ', gb(d.total_mib), d.reason ? ` (${String(d.reason).replace(/_/g, ' ')})` : '', '.'];
    case 'benchStepFinished': return ['Benchmark ', h('span.mono', d.benchmark || ''),
      d.result === 'not_judged' || d.outcome === 'refused_by_exclusion' ? ' was not judged (the profile forces a node that exclude_nodes names)'
        : d.pass ? ' passed' : d.outcome === 'refused' ? ' was refused' : ' did not pass',
      d.load_s != null ? `: load ${dur(d.load_s)}` : '', d.decode_tps ? `, ${num(d.decode_tps, 1)} tok/s` : '', d.regression ? ', slower than before' : '', '.'];
    case 'benchRunStarted': return [`A benchmark run of suite ${d.suite || ''} started (${plural(d.steps || 0, 'step')}).`];
    case 'benchRunFinished': return [`The benchmark run of suite ${d.suite || ''} ${d.state || 'finished'}: ${num(d.passed || 0)} passed, ${num(d.failed || 0)} failed`,
      d.refused_by_exclusion ? `, ${num(d.refused_by_exclusion)} not judged` : '',
      d.regressions ? `, ${plural(d.regressions, 'regression')}` : '', '.'];
  }
  return [e.summary || e.type];
}

function eventRow(e, open, toggle) {
  const [label, kind] = tagOf(e);
  const key = e.seq + '-' + e.ts;
  const detail = detailOf(e);
  return h('div', { class: 'ev sev-' + (e.severity || 'info'), key: 'ev-' + key },
    h('time', { title: dateTime(e.ts), datetime: new Date(e.ts).toISOString() }, agoText(e.ts)),
    h('span', badge(label, kind)),
    h('div', { style: 'min-width:0' },
      h('p', sentenceOf(e), (e.nodes || []).length ? [' ', nodeChips(e.nodes)] : null),
      detail ? h('p.d', detail) : null),
    h('button.more', { type: 'button', 'aria-expanded': open ? 'true' : 'false', title: 'The full record as the scheduler stored it', onclick: () => toggle(key) }, open ? 'Hide' : 'Details'),
    open ? h('pre.mono', (e.summary ? e.summary + '\n\n' : '') + JSON.stringify(e, null, 2)) : null);
}

registerView({
  id: 'activity', title: 'Activity',
  mount(el, route) {
    let events = null, truncated = false, historyOn = true, error = '', summary = null, loads = null, requests = null, alive = true;
    let busy = false, sumTimer = 0, reqTimer = 0, servedSig = '';
    const open = new Set();
    const q = () => route.query;
    const range = () => RANGES.find(r => r[0] === q().range) || RANGES[1];
    const since = () => Math.round(now() - range()[2] * 1000);
    const types = () => (GROUPS[q().type] ? GROUPS[q().type][1] : ALL_TYPES);
    const setQuery = patch => navigate('activity', null, Object.assign({}, q(), patch), true);
    const params = extra => Object.assign({ since: q().session ? null : since(), type: q().session ? null : types().join(','), severity: q().sev === 'warn' ? 'warn' : null,
      q: q().q || null, session_id: q().session || null, limit: 200 }, extra);
    const matches = e => {
      if (q().session) return e.session_id === q().session;
      if (!types().includes(e.type)) return false;
      if (q().sev === 'warn' && e.severity !== 'warn' && e.severity !== 'error') return false;
      if (q().q && !String(e.summary || '').toLowerCase().includes(q().q.toLowerCase())) return false;
      return true;
    };

    const loadEvents = async () => {
      busy = true;
      try {
        const r = await api.get('/api/history', params());
        events = r.events || []; truncated = !!r.truncated || events.length >= 200; historyOn = r.history !== 'off'; error = '';
      } catch (e) { error = e.message; events = events || []; }
      busy = false;
      if (alive) view.refresh();
    };
    const loadEarlier = async () => {
      if (busy || !events || !events.length) return;
      busy = true; view.refresh();
      try {
        const r = await api.get('/api/history', params({ until: events[events.length - 1].ts - 1 }));
        const more = r.events || [];
        events = events.concat(more); truncated = more.length >= 200;
      } catch (e) { error = e.message; }
      busy = false;
      if (alive) view.refresh();
    };
    const loadSide = async () => {
      try {
        const [s, l] = await Promise.all([
          api.get('/api/history/summary', { since: since() }),
          api.get('/api/history', { since: since(), type: 'loadFinished', limit: 50 }),
        ]);
        summary = s; loads = l.events || [];
      } catch { summary = summary || {}; loads = loads || []; }
      if (alive) view.refresh();
    };
    const loadRequests = async () => {
      try { requests = ((await api.get('/api/metrics/activity')).data || []).slice().reverse(); } catch { requests = requests || []; }
      if (alive) view.refresh();
    };
    const offEvent = listen('event', e => {
      if (events && matches(e)) events.unshift(e);
      if (loads && e.type === 'loadFinished') loads.unshift(e);
      clearTimeout(sumTimer);
      sumTimer = setTimeout(loadSide, 3000); // counts follow the pushed events
    });
    // Recent requests come from REST; ask again when the status says more were served.
    const offStatus = on('status', st => {
      const sig = ((st && st.instances) || []).map(i => i.profile + ':' + i.requests_served + ':' + i.in_flight).join(',');
      if (sig === servedSig) return;
      servedSig = sig;
      clearTimeout(reqTimer);
      reqTimer = setTimeout(loadRequests, 1200);
    });

    const render = () => {
      const r = range();
      const head = pageHead('Activity', 'What the scheduler decided, and the requests it served',
        [chips(RANGES.map(x => ({ id: x[0], label: x[1] })), r[0], id => setQuery({ range: id }), 'Time range'),
          h('a.btn', { href: api.url('/api/history/export', { since: since() }), download: '', title: 'Every event of this time range as JSON Lines' }, 'Download')]);
      if (events && !historyOn) {
        return [pageHead('Activity', 'What the scheduler decided, and the requests it served'),
          h('section.card', empty('History is off', 'The scheduler keeps its decisions only when it is started with a history directory (--history-dir). Nothing is recorded now, so there is nothing to show here. The Models, Nodes and Queue screens still show the live state.')),
          requestsCard()];
      }
      const typeChips = [{ id: 'all', label: 'All' }].concat(Object.keys(GROUPS).map(g => ({ id: g, label: GROUPS[g][0] })));
      let lastDay = '';
      const rows = [];
      for (const e of events || []) {
        const day = dayLabel(e.ts);
        if (day !== lastDay) { rows.push(h('div.day', { key: 'day-' + day }, day, rows.length ? '' : ' · newest first')); lastDay = day; }
        rows.push(eventRow(e, open.has(e.seq + '-' + e.ts), k => { if (open.has(k)) open.delete(k); else open.add(k); view.refresh(); }));
      }
      const bySev = (summary && summary.by_severity) || {};
      const incidents = ((summary && summary.incidents) || []).slice(0, 6);
      return [
        head,
        q().session ? h('div.banner.info', h('span.grow', 'Showing every event of one load (', h('span.mono', q().session), ').'), button('Show all activity', () => setQuery({ session: null }), 'sm')) : null,
        error ? h('div.banner.err', 'The history could not be read: ' + error) : null,
        h('div.cols',
          h('section.card.wide', { style: 'gap:0' },
            h('div.ch.split',
              h('h2', 'Decisions'),
              h('div.row', { style: 'gap:8px' },
                h('input.input', { type: 'search', placeholder: 'Search the sentences', 'aria-label': 'Search', value: q().q || '', style: 'width:220px;height:32px',
                  onchange: ev => setQuery({ q: ev.target.value.trim() || null }) }),
                h('button', { class: 'fc' + (q().sev === 'warn' ? ' on' : ''), type: 'button', 'aria-pressed': q().sev === 'warn' ? 'true' : 'false',
                  onclick: () => setQuery({ sev: q().sev === 'warn' ? null : 'warn' }) }, 'Incidents only'))),
            h('div', { style: 'padding:0 20px 14px' }, chips(typeChips, GROUPS[q().type] ? q().type : 'all', id => setQuery({ type: id === 'all' ? null : id }), 'Filter by type')),
            !events ? skeleton(5)
              : rows.length ? rows
                : empty('Nothing happened that matches', `No event of this kind in the last ${r[1]}. Try a longer time range or another filter.`),
            events && events.length && truncated ? h('div.cs', button(busy ? 'Reading…' : 'Show earlier', loadEarlier, 'sm', { disabled: busy })) : null),
          h('div.side', { style: 'max-width:500px;flex-basis:420px' },
            h('section.card',
              h('div.ch.split', h('h2', 'Incidents'), h('span.note', 'last ' + r[1])),
              !summary ? skeleton(2) : [
                h('div.stats', stat('Events', num(summary.events || 0), null, { small: true }), stat('Warnings', num(bySev.warn || 0), null, { small: true, cls: bySev.warn ? 'warn-t' : null }),
                  stat('Errors', num(bySev.error || 0), null, { small: true, cls: bySev.error ? 'err-t' : null })),
                incidents.length ? incidents.map(e => h('div.bl', { key: 'inc-' + e.seq + '-' + e.ts }, h('span', e.summary), h('time', { title: dateTime(e.ts) }, agoText(e.ts))))
                  : h('div.cs.mut', 'No error in this time range.'),
              ]),
            h('section.card',
              h('div.ch.split', h('h2', 'Loads'), summary && summary.loads ? h('span.note', `${num(summary.loads.ready || 0)} ready · ${num((summary.loads.failed || 0) + (summary.loads.cancelled || 0))} failed or cancelled`) : null),
              !loads ? skeleton(3) : table({
                rows: loads.slice(0, 10), key: e => e.seq + '-' + e.ts,
                columns: [
                  { label: 'Finished', cls: 'tight', render: e => h('time', { title: dateTime(e.ts) }, agoText(e.ts)) },
                  { label: 'Model', cls: 'clip', render: e => h('span', { title: e.profile + ' on ' + (e.nodes || []).join(', ') }, h('span.mono', e.profile),
                    h('span.sub', (e.data || {}).outcome !== 'ready' ? h('span.err-t', (e.data || {}).outcome === 'cancelled' ? 'Cancelled · ' : 'Failed · ') : null, plural((e.nodes || []).length, 'node'))) },
                  { label: 'Took', cls: 'r', title: 'Load time in seconds', render: e => ((e.data || {}).load_s != null ? num(e.data.load_s) + ' s' : '–') },
                  { label: 'MB/s', cls: 'r', title: 'Average upload speed; a dash when it was not measured', render: e => mbps((e.data || {}).avg_mb_s) || h('span.mut', { title: 'Not measured' }, '–') },
                ],
                empty: empty('No load finished', `No model finished loading in the last ${r[1]}.`),
              })),
            requestsCard())),
      ];
    };
    function requestsCard() {
      return h('section.card',
        h('div.ch.split', h('h2', 'Recent requests'), h('span.note', requests && requests.length ? `the last ${num(requests.length)} since the scheduler started` : null)),
        !requests ? skeleton(3) : table({
          rows: requests.slice(0, 10), key: r => r.id,
          columns: [
            { label: 'Finished', cls: 'tight', render: r => h('time', { title: dateTime(r.timestamp) }, agoText(r.timestamp)) },
            { label: 'Model', cls: 'clip', render: r => h('span', { title: r.model }, h('span.mono', r.model), r.resp_status_code !== 200 ? h('span.sub.err-t', 'HTTP ' + r.resp_status_code) : null) },
            { label: 'Tokens', cls: 'r', title: 'Tokens generated', render: r => (r.tokens && r.tokens.output_tokens >= 0 ? num(r.tokens.output_tokens) : '–') },
            { label: 'tok/s', cls: 'r', render: r => (r.tokens ? tps(r.tokens.tokens_per_second) : '–') },
            { label: 'Took', cls: 'r', render: r => dur((r.duration_ms || 0) / 1000) },
          ],
          empty: empty('No request yet', 'Requests show here once the scheduler has served one. Their text is never kept, only the counts and the speed.'),
        }));
    }

    const view = liveView(el, render, ['tick', 'conn']);
    loadEvents(); loadSide(); loadRequests();
    return {
      update(r) {
        const changed = JSON.stringify(r.query) !== JSON.stringify(route.query);
        const rangeChanged = r.query.range !== route.query.range;
        route = r;
        if (changed) { events = null; loadEvents(); }
        if (rangeChanged) { summary = null; loads = null; loadSide(); }
        view.refresh();
      },
      unmount() { alive = false; clearTimeout(sumTimer); clearTimeout(reqTimer); offEvent(); offStatus(); view.unmount(); },
    };
  },
});
