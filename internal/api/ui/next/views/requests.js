// Runs: every client request in the request log (a run, in the design's
// word), newest first, live ones on the first page, 50 a page (the
// Overview's "All 203" opens it). The route stays #/requests.
// There is no mockup for this page yet (owner's answer to Q3): it is built
// from the Overview's own parts (the runs table, the filter pills) and
// stays plain until one exists.
//
// Query: ?status=<one state>&profile=<name>&from=<ms>&to=<ms>; the pages are
// a stack of cursors (GET /api/requests?before=).

import { h } from '../core/dom.js';
import { registerView, navigate, href } from '../core/router.js';
import { onCatalog, needCatalog } from '../core/catalog.js';
import { liveView, pageHead, chips, button, runsTable, runLegend, tickRuns, skeleton, filterSelect, roleShare, runMachines } from '../core/ui.js';
import { num, dateTime, clock } from '../core/format.js';
import { requestFeed, every, onLiveTimings, liveTimings } from '../core/requests.js';

const STATES = [['', 'All'], ['queued', 'Queued'], ['running', 'Running'], ['done', 'Done'], ['failed', 'Failed'], ['refused', 'Refused'], ['cancelled', 'Cancelled']];
const PAGE = 50;

// machineList(nodes) -> the Zones cell (no mockup draws it). Up to three
// machines: the same words as runMachines ("node-a Host 50%, node-b Helper
// 50%"), each machine in its own span.mc that never breaks, so the list
// wraps between machines inside the column (td.rq-zones) instead of pushing
// the table out of its card. More than three: "node-a Host + 5 helpers",
// the full list on hover.
const MACHINES_LISTED = 3;
function machineList(nodes) {
  if (nodes.length > MACHINES_LISTED) {
    const host = nodes[0];
    return h('span.mono', { 'data-machines': String(nodes.length), title: runMachines(nodes) },
      h('span.mc', `${host.name || host.id} ${roleShare(host.role) || 'Host'} + ${nodes.length - 1} helpers`));
  }
  return h('span.mono', { 'data-machines': String(nodes.length) }, nodes.map((n, i) => [
    i ? ' ' : null,
    h('span.mc', [n.name || n.id, roleShare(n.role, n.share)].filter(Boolean).join(' ') + (i < nodes.length - 1 ? ',' : '')),
  ]));
}

registerView({
  id: 'requests', title: 'Requests',
  mount(el, route) {
    let r = route;
    let cursors = [0]; // the cursor of each page seen; the last one is shown
    const params = () => {
      const q = { limit: PAGE, status: r.query.status || '', profile: r.query.profile || '', from: r.query.from || '', to: r.query.to || '' };
      const before = cursors[cursors.length - 1];
      if (before) q.before = before;
      // only the first page changes by itself
      q.poll = !before;
      return q;
    };
    const feed = requestFeed(params(), () => view.refresh());
    const setQuery = q => { cursors = [0]; navigate('requests', [], Object.assign({}, r.query, q), true); };
    const ticker = every(100, () => tickRuns(el));
    const offTimings = onLiveTimings(() => view.refresh());

    // the Model column names runs from the catalog (runModel)
    const offCat = onCatalog(() => view.refresh());
    needCatalog();
    const view = liveView(el, () => {
      const d = feed.data;
      const rows = d.rows || [];
      const from = Number(r.query.from) || 0, to = Number(r.query.to) || 0;
      const range = from || to ? [from ? dateTime(from) : 'the start', ' to ', to ? dateTime(to) : 'now'] : 'All requests on record';
      const models = [...new Set(rows.map(x => x.profile).filter(Boolean).concat(r.query.profile ? [r.query.profile] : []))].sort();
      const page = cursors.length;
      return [
        pageHead('Requests', range, (from || to) ? h('a.btn', { href: href('requests') }, 'Show every request') : null),
        h('section.panel', { 'aria-label': 'Requests' },
          h('div.rq-bar',
            h('div', chips(STATES.map(([id, label]) => ({ id, label })), r.query.status || '', id => setQuery({ status: id }), 'State')),
            h('div', filterSelect('Model', [{ id: '', label: 'any' }].concat(models.map(m => ({ id: m, label: m }))), r.query.profile || '', v => setQuery({ profile: v }), { bare: true }),
              runLegend())),
          d.loading && !rows.length ? skeleton(6) : runsTable({ rows, now: Date.now(), zones: false, liveTimings: liveTimings(), modelClass: 'rq-model',
            extra: [{ label: 'Zones', cls: 'rq-zones', render: x => (x.nodes || []).length ? machineList(x.nodes) : h('span.mut', '–') },
              { label: 'Sent by', cls: 'rq-sent', render: x => h('span', { title: x.caller ? x.caller + (x.source_addr ? ' · from ' + x.source_addr : '') : x.source_addr || null }, x.caller || '–') }],
            empty: [h('b', 'No requests here'), d.on === false ? 'The request log is off on this scheduler: only live requests show.' : 'Nothing matches these filters.'] }),
          h('div.rq-foot',
            h('span.grow', d.error ? `The requests could not be read (${d.error}).` : d.at ? `Page ${num(page)} · ${num(rows.length)} shown · updated ${clock(d.at)}` : ''),
            button('Newer', () => { cursors.pop(); feed.set(params()); }, 'sm', { disabled: page <= 1 }),
            button('Older', () => { cursors.push(d.next); feed.set(params()); }, 'sm', { disabled: !d.next }))),
      ];
    }, ['tick']);

    return {
      update(nr) { r = nr; feed.set(params()); view.refresh(); },
      unmount() { offCat(); feed.stop(); offTimings(); ticker(); view.unmount(); },
    };
  },
});
