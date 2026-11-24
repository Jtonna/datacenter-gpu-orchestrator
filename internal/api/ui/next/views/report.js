// The 24-hour report: what the scheduler did in the last 24 hours, on one
// page that prints (the Overview's "View 24-hour report"). There is no
// mockup for it yet: it is built from the summary route and the Overview's
// own parts, and stays plain until one exists.
//
// Reads: GET /api/requests/summary (the last 24 hours by hour), GET
// /api/requests?status=failed,refused (what went wrong), GET
// /api/nodes/availability (each zone's 24 hours and its uptime).

import { h } from '../core/dom.js';
import { registerView, href } from '../core/router.js';
import { state } from '../core/store.js';
import * as api from '../core/api.js';
import { liveView, pageHead, panel, button, stat, tokensChart, tokensLegend, runsTable, availStrip, availClass, colourKey, skeleton, empty } from '../core/ui.js';
import { num, pct, dateTime } from '../core/format.js';
import { viewNodes } from '../core/status.js';
import { bucketTitle } from '../core/period.js';

const DAY = 24 * 3600000;

registerView({
  id: 'report', title: '24-hour report',
  mount(el) {
    let sum = null, bad = null, avail = null, err = '', dead = false;
    const to = Math.ceil(Date.now() / 3600000) * 3600000, from = to - DAY;
    (async () => {
      try {
        const [s, b, a] = await Promise.all([
          api.get('/api/requests/summary', { from, to, step: 'hour' }),
          api.get('/api/requests', { status: 'failed,refused', from, limit: 50 }),
          api.get('/api/nodes/availability', {}),
        ]);
        if (dead) return;
        sum = s; bad = (b && b.requests) || []; avail = a;
      } catch (e) { if (!dead) err = (e && e.message) || 'the request failed'; }
      view.refresh();
    })();

    const view = liveView(el, () => {
      const head = pageHead('24-hour report', [dateTime(from), ' to ', dateTime(to)],
        [h('a.btn.rp-noprint', { href: href('overview') }, 'Back to the Overview'), button('Print', () => window.print(), 'pri rp-noprint')]);
      if (err) return [head, h('section.card', empty('The report could not be read', err))];
      if (!sum) return [head, h('section.card', skeleton(5))];
      const t = sum.totals || {};
      const nowMs = Date.now();
      const byT = new Map((sum.series || []).map(b => [b.t, b]));
      const buckets = [];
      for (let x = from; x < to; x += 3600000) {
        const b = byT.get(x);
        const d = new Date(x);
        buckets.push({ in: b ? b.tokens_in : 0, out: b ? b.tokens_out : 0, label: d.getHours() % 6 === 0 ? String(d.getHours()).padStart(2, '0') : '',
          title: x > nowMs ? null : `${bucketTitle(x, 'hour')} · ${num(b ? b.tokens_in : 0)} in · ${num(b ? b.tokens_out : 0)} out` });
      }
      const st = state.status;
      const series = (avail && avail.nodes) || [];
      const nameOf = ns => { const n = st && viewNodes(st).find(v => v.id === ns.id); return (n && n.hostname) || ns.name || ns.id; };
      return [
        head,
        sum.on === false ? h('div.banner.warn', 'The request log is off on this scheduler: nothing was recorded, so the figures below are empty.') : null,
        panel('Requests', null, h('div.stats',
          stat('Requests', num(t.requests)), stat('Done', num(t.done)), stat('Failed', num(t.failed)), stat('Refused', num(t.refused)), stat('Cancelled', num(t.cancelled)),
          stat('Prefill', t.prefill_tps == null ? '–' : num(t.prefill_tps, 1), 'tok/s'), stat('Decode', t.decode_tps == null ? '–' : num(t.decode_tps, 1), 'tok/s'),
          stat('Queued now', num(sum.live ? sum.live.queued : 0)), stat('Running now', num(sum.live ? sum.live.running : 0)))),
        h('section.panel', { 'aria-label': 'Tokens' }, h('div.wh', h('h2', 'Tokens'), tokensLegend('per hour')),
          h('div', { style: 'height:270px;display:flex' }, tokensChart({ tin: t.tokens_in, tout: t.tokens_out, buckets, empty: t.requests ? null : 'No requests in these 24 hours.' }))),
        h('section.panel', { 'aria-label': 'What went wrong' }, h('div.wh', h('h2', 'Failed and refused requests'), h('a', { href: href('requests', [], { status: 'failed', from }) }, 'All failed')),
          runsTable({ rows: bad || [], zones: false, empty: [h('b', 'None'), 'Every request in these 24 hours got its answer, or was cancelled by the app that sent it.'] })),
        h('section.panel', { 'aria-label': 'Zones' }, h('div.wh', h('h2', 'Zones, hour by hour'), h('span.lbl', '24 h ago … now')),
          series.length ? h('div.rp-strips', series.map(ns => h('div.rp-strip', { key: ns.id },
            h('span.zname', nameOf(ns)),
            availStrip(ns.cells.slice(-24).map(availClass), nameOf(ns) + ', last 24 hours'),
            h('span.mut', ns.uptime == null ? 'Uptime not known' : 'Up ' + pct(ns.uptime * 100, 1) + ' of the known time')))) : h('div.zempty', 'No availability is recorded yet.'),
          colourKey()),
      ];
    }, ['status']);
    return { unmount() { dead = true; view.unmount(); } };
  },
});
