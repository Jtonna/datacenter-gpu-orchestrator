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
import { num, mib, bytes, tps, dur, ago, plural, now, dayLabel, dateTime, agoText } from '../core/format.js';

const RANGES = [['1h', '1 h', 3600], ['6h', '6 h', 21600], ['24h', '24 h', 86400], ['7d', '7 d', 604800]];
const GROUPS = {
  loads: ['Loads', ['loadStarted', 'loadFinished', 'loadStalled', 'loadCancelledLowMemory', 'preflightFailed', 'learnedNeed', 'needsMibTooLow']],
  unloads: ['Unloads', ['instanceUnloaded', 'childExited', 'eviction']],
  refusals: ['Refusals', ['requestRefused', 'profileBackedOff']],
  nodes: ['Nodes', ['nodeLost', 'nodeQuarantined', 'quarantineEnded', 'workerHealth', 'recoveryRequest', 'recoveryResponse', 'recoveryOutcome', 'recoverySequenceStopped',
    'fleetViewStale', 'fleetViewRecovered', 'fleetFeedDisconnected', 'fleetFeedRecovered']],
  benchmarks: ['Benchmarks', ['benchRunStarted', 'benchStepFinished', 'benchRunFinished']],
  system: ['Scheduler', ['schedulerStarted', 'schedulerStopping']],
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
      if (d.need_mib) parts.push('needs ' + mib(d.need_mib) + (d.need_basis ? ` (${d.need_basis})` : ''));
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
      parts.push(`${mib(d.available_mib)} available, the floor is ${mib(d.floor_mib)}`);
      break;
    case 'requestRefused':
      if (d.message) parts.push(d.message);
      if (d.waiters > 1) parts.push(plural(d.waiters, 'request'));
      if (d.suppressed_since_last) parts.push(`${num(d.suppressed_since_last)} more since the last record`);
      if (d.retry_after_s) parts.push('retry after ' + dur(d.retry_after_s));
      break;
    case 'instanceUnloaded':
      if (d.reason) parts.push({ ttl: 'idle too long', api: 'asked through the API', eviction: 'evicted to make room', shutdown: 'the scheduler stopped' }[d.reason] || d.reason);
      if (d.loaded_for_s != null) parts.push('was loaded for ' + dur(d.loaded_for_s));
      if (d.requests_served != null) parts.push(plural(d.requests_served, 'request') + ' served');
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
      parts.push(`configured ${mib(d.configured_mib)}, measured ${mib(d.measured_mib)}`);
      break;
    case 'nodeMemory':
      parts.push(`${mib(d.available_mib)} available of ${mib(d.total_mib)}`);
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
  if (d.bench_run && !e.type.startsWith('bench')) parts.push('caused by benchmark run ' + d.bench_run);
  return parts.join(' · ');
}

function eventRow(e, open, toggle) {
  const [label, kind] = tagOf(e);
  const key = e.seq + '-' + e.ts;
  const detail = detailOf(e);
  return h('div', { class: 'ev sev-' + (e.severity || 'info'), key: 'ev-' + key },
    h('time', { title: dateTime(e.ts), datetime: new Date(e.ts).toISOString() }, agoText(e.ts)),
    h('span', badge(label, kind)),
    h('div', { style: 'min-width:0' },
      h('p', e.summary || e.type, (e.nodes || []).length ? [' ', nodeChips(e.nodes)] : null),
      detail ? h('p.d', detail) : null),
    h('button.more', { type: 'button', 'aria-expanded': open ? 'true' : 'false', title: 'The full record as the scheduler stored it', onclick: () => toggle(key) }, open ? 'Hide record' : 'Record'),
    open ? h('pre.mono', JSON.stringify(e, null, 2)) : null);
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
                rows: loads.slice(0, 12), key: e => e.seq + '-' + e.ts, minWidth: 360,
                columns: [
                  { label: 'Finished', cls: 'tight', render: e => h('time', { title: dateTime(e.ts) }, agoText(e.ts)) },
                  { label: 'Model', render: e => h('span', { title: (e.nodes || []).join(', ') }, h('span.mono.nowrap', e.profile),
                    h('span.sub', (e.data || {}).outcome !== 'ready' ? h('span.err-t', (e.data || {}).outcome === 'cancelled' ? 'Cancelled · ' : 'Failed · ') : null, plural((e.nodes || []).length, 'node'))) },
                  { label: 'Took', cls: 'r', render: e => ((e.data || {}).load_s != null ? num(e.data.load_s, 1) + ' s' : '–') },
                  { label: 'MB/s', cls: 'r', render: e => ((e.data || {}).avg_mb_s ? num(e.data.avg_mb_s, 1) : '–') },
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
          rows: requests.slice(0, 10), key: r => r.id, minWidth: 400,
          columns: [
            { label: 'Finished', cls: 'tight', render: r => h('time', { title: dateTime(r.timestamp) }, agoText(r.timestamp)) },
            { label: 'Model', render: r => h('span', h('span.mono.nowrap', r.model), r.resp_status_code !== 200 ? h('span.sub.err-t', 'HTTP ' + r.resp_status_code) : null) },
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
