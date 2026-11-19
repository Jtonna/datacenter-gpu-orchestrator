// Queue: the requests that wait for a model, with the reason in plain words,
// what frees up next (only what the status gives a time for), the profiles and
// nodes that are held back, and the requests that were turned away recently.
// Read-only.
//
// Reads: WebSocket "status" (queue, instances, view.nodes, backoffs,
// quarantines) and "event" (requestRefused, to keep the last table current);
// REST GET /api/history?type=requestRefused (once, on open) and
// GET /api/system (once: the queue time limit is not in the status).

import { h } from '../core/dom.js';
import { registerView, href } from '../core/router.js';
import { state, listen } from '../core/store.js';
import * as api from '../core/api.js';
import { liveView, pageHead, badge, nodeChip, nodeChips, bar, table, empty, skeleton } from '../core/ui.js';
import { num, dur, ago, at, until, plural, toMs, now, clock, parseGoDuration } from '../core/format.js';
import { viewNodes, nodeName, drift, staleBanner, plainReason, refusalWord } from '../core/status.js';

const sentence = s => { s = String(s || '').trim().replace(/^placer:\s*/i, ''); if (!s) return ''; s = s.charAt(0).toUpperCase() + s.slice(1); return /[.!?]$/.test(s) ? s : s + '.'; };

// The facts about the nodes that explain a wait, from the status alone.
function nodeFacts(st, entry) {
  const rows = [];
  const victims = new Set(entry.claimed ? entry.victims || [] : []);
  // nodes that hold the same model are one line
  const byHolder = new Map();
  for (const n of viewNodes(st)) {
    if (n.excluded || !n.online || !n.holder || n.holder === entry.profile || victims.has(n.holder)) continue;
    if ((st.quarantines || []).some(x => x.node === n.id) || n.settling) continue;
    if (!byHolder.has(n.holder)) byHolder.set(n.holder, []);
    byHolder.get(n.holder).push(n.hostname || n.id);
  }
  for (const [profile, names] of byHolder) {
    const holder = (st.instances || []).find(i => i.profile === profile) || { profile };
    rows.push(['', [nodeChips(names), names.length > 1 ? ' are ' : ' is ', holder.state === 'starting' ? 'loading ' : 'busy with ', h('span.mono', profile),
      holder.in_flight > 0 ? `, which has ${plural(holder.in_flight, 'request')} in flight` : holder.state === 'ready' ? ', which is idle but still inside its minimum hold time' : ''],
    holder.state === 'draining' ? badge('Draining', 'settle') : null]);
  }
  for (const n of viewNodes(st)) {
    if (n.excluded) continue;
    const chip = nodeChip(n.hostname || n.id);
    const q = (st.quarantines || []).find(x => x.node === n.id);
    const holder = n.holder;
    if (!n.online) rows.push(['', [chip, ' is offline'], null]);
    else if (q) rows.push(['x', [chip, ' is set aside until ', at(q.until), ' after a failed check'], badge(dur(Math.max(0, (toMs(q.until) - now()) / 1000)), 'aside')]);
    else if (n.settling) rows.push(['s', [chip, ' is settling after an unload', n.settle_until ? [' until ', at(n.settle_until)] : null], n.settle_until ? badge(dur(Math.max(0, (toMs(n.settle_until) - now()) / 1000)), 'settle') : null]);
    else if (!holder && n.claimed_by && n.claimed_by !== entry.profile) rows.push(['l', [chip, ' is claimed for ', h('span.mono', n.claimed_by)], null]);
  }
  return rows;
}

function entryCard(st, e, limitS) {
  const waited = (e.age || 0) + drift();
  const clockS = (e.clock || 0) + (e.clock_running ? drift() : 0);
  const claimNames = (e.claim || []).map(id => nodeName(st, id));
  const victims = (e.victims || []).map(v => (st.instances || []).find(i => i.profile === v) || { profile: v });
  let head, tag;
  if (e.claimed) {
    tag = badge('Nodes claimed', 'q');
    const busy = victims.filter(v => v.in_flight > 0);
    if (victims.length && !e.victims_stopped) {
      head = busy.length ? ['Its nodes are claimed. Waiting for ', h('span.mono', busy.map(v => v.profile).join(', ')), ` to finish ${plural(busy.reduce((s, v) => s + v.in_flight, 0), 'request')} and unload.`]
        : ['Its nodes are claimed. ', h('span.mono', victims.map(v => v.profile).join(', ')), ' is being unloaded to make room.'];
    } else head = 'Its nodes are claimed. The load starts as soon as they have settled.';
  } else {
    tag = badge('Waiting for room', 'n');
    head = plainReason(e.wait_reason) || 'No node can take it right now.';
  }
  const why = [];
  if (e.claimed) {
    why.push(['l', ['Claimed ', nodeChips(claimNames)], null]);
    for (const v of victims) {
      why.push([v.in_flight > 0 ? 'a' : 's', [h('span.mono', v.profile), v.state === 'draining' ? ' is finishing its requests, then it unloads' : v.state === 'stopping' ? ' is unloading' : e.victims_stopped ? ' was unloaded to make room' : ' will be unloaded to make room'],
        v.in_flight > 0 ? badge(plural(v.in_flight, 'request') + ' in flight', 'load') : null]);
    }
  }
  for (const r of nodeFacts(st, e)) why.push(r);
  const backoff = (st.backoffs || []).find(b => b.profile === e.profile);
  if (backoff) why.push(['x', ['A load of this profile failed; new requests for it are refused until ', at(backoff.until)], badge(dur(Math.max(0, (toMs(backoff.until) - now()) / 1000)), 'aside')]);
  return h('article', { key: 'q-' + e.profile + '-' + e.created, style: 'display:flex;flex-direction:column;gap:14px;padding:18px 20px;border-top:1px solid var(--line)' },
    h('div.row', { style: 'align-items:baseline;gap:6px 14px' },
      h('h3.mono', { style: 'font-size:16px;font-weight:500' }, e.profile),
      h('span.mono.mut', plural(Math.max(1, e.waiters || 0), 'request')),
      h('span.right', tag)),
    h('p', { style: 'font-size:16px;font-weight:600;max-width:72ch', title: !e.claimed && e.wait_reason ? 'The scheduler says: ' + e.wait_reason : null }, head),
    h('div', { style: 'display:flex;flex-direction:column;gap:6px' },
      h('div', { style: 'display:flex;flex-wrap:wrap;justify-content:space-between;gap:4px 16px;font-size:13px;font-weight:600' },
        h('span', 'Waited ', h('time', { title: 'Queued ' + clock(e.created) }, dur(Math.round(waited)))),
        limitS ? h('span.mut', e.claimed ? 'Claimed: the time limit is paused' : `Gives up after ${dur(limitS)} without a claim`)
          : null),
      limitS ? bar([{ cls: e.claimed ? 'l' : clockS / limitS > 0.75 ? 'x' : clockS / limitS > 0.4 ? 'a' : 'm', value: Math.min(clockS, limitS) }], limitS,
        `Waited ${dur(Math.round(clockS))} of a ${dur(limitS)} limit`, 'sm') : null),
    why.length ? h('div.why', why.map(([cls, text, tail]) => [h('i', { class: cls }), h('span', { class: cls ? null : 'mut' }, text), tail || h('span')])) : null);
}

function loadingCard(st, inst) {
  const load = inst.load || {};
  const frac = load.progress != null ? load.progress : inst.load_progress;
  const eta = load.eta_s != null ? load.eta_s : inst.load_eta_s;
  const names = (inst.nodes || []).map(n => nodeName(st, n.id));
  const stalled = load.stalled_s != null;
  return h('article', { key: 'l-' + inst.profile, style: 'display:flex;flex-direction:column;gap:14px;padding:18px 20px;border-top:1px solid var(--line)' },
    h('div.row', { style: 'align-items:baseline;gap:6px 14px' },
      h('h3.mono', { style: 'font-size:16px;font-weight:500' }, inst.profile),
      h('span.mono.mut', plural(inst.waiters, 'request')),
      h('span.right', stalled ? badge('Load stalled', 'fail') : badge('Model loading', 'load'))),
    h('p', { style: 'font-size:16px;font-weight:600;max-width:72ch' },
      stalled ? `The model is loading, but no new bytes have arrived for ${dur(Math.round(load.stalled_s))}.`
        : eta != null ? `The model is still loading. About ${dur(eta < 90 ? Math.round(eta) : Math.round(eta / 60) * 60)} left.` : 'The model is still loading.'),
    h('div.why',
      h('i', { class: stalled ? 'x' : 'a' }), h('span', 'Loading on ', nodeChips(names), ' · ', h('a', { href: href('models') }, 'see the load')),
      frac != null ? badge(Math.round(frac * 100) + '%', stalled ? 'aside' : 'load') : h('span')));
}

function freesNext(st) {
  const items = [];
  for (const n of viewNodes(st)) {
    const chip = nodeChip(n.hostname || n.id);
    if (n.settling && n.settle_until) items.push({ t: toMs(n.settle_until), what: [chip, ' finishes settling'], detail: n.claimed_by ? ['Goes to ', h('span.mono', n.claimed_by)] : 'Nothing has claimed it' });
  }
  for (const q of st.quarantines || []) items.push({ t: toMs(q.until), what: [nodeChip(nodeName(st, q.node)), ' returns to rotation'], detail: 'It was set aside after a failed check' });
  for (const b of st.backoffs || []) items.push({ t: toMs(b.until), what: [h('span.mono', b.profile), ' may be loaded again'], detail: 'Held back after a failed load' });
  for (const i of st.instances || []) {
    const eta = i.load && i.load.eta_s != null ? i.load.eta_s : i.load_eta_s;
    if (i.state === 'starting' && eta != null && !(i.load && i.load.stalled_s != null)) {
      items.push({ t: now() + eta * 1000, approx: true, what: [h('span.mono', i.profile), ' finishes loading'], detail: i.waiters ? `Then it serves ${plural(i.waiters, 'waiting request')}` : 'Nothing is waiting for it' });
    }
    if (i.state === 'draining') items.push({ t: null, what: [h('span.mono', i.profile), ' unloads'], when: `after ${plural(i.in_flight || 0, 'request')}`, detail: i.victim_of ? ['Its nodes go to ', h('span.mono', i.victim_of)] : 'It is draining' });
  }
  items.sort((a, b) => (a.t == null) - (b.t == null) || a.t - b.t);
  return items;
}

registerView({
  id: 'queue', title: 'Queue',
  mount(el) {
    let refused = null, limitS = null, historyOn = true, alive = true;
    const loadRefused = async () => {
      try {
        const r = await api.get('/api/history', { type: 'requestRefused', limit: 20 });
        refused = r.events || [];
        historyOn = r.history !== 'off';
      } catch { refused = []; }
      if (alive) view.refresh();
    };
    // The queue time limit is a setting. Until the configuration endpoint exists
    // on every server, it is read from where it is available and left out otherwise.
    const loadLimit = async () => {
      try {
        const c = await api.get('/api/config/settings');
        const v = parseGoDuration((c.settings || c).queue_timeout);
        if (v) limitS = v;
      } catch { /* not available: the bar and the limit are left out */ }
      if (alive) view.refresh();
    };
    const offEvent = listen('event', e => {
      if (e.type !== 'requestRefused' || !refused) return;
      refused.unshift(e);
      refused.length = Math.min(refused.length, 20);
    });
    const offConfig = listen('config', loadLimit);

    const view = liveView(el, () => {
      const st = state.status;
      if (!st) return [pageHead('Queue'), staleBanner(), h('section.card', skeleton(3))];
      const queue = st.queue || [];
      const loading = (st.instances || []).filter(i => i.state === 'starting' && i.waiters > 0);
      const waiting = queue.reduce((s, e) => s + Math.max(1, e.waiters || 0), 0) + loading.reduce((s, i) => s + i.waiters, 0);
      const oldest = queue.reduce((m, e) => Math.max(m, e.age || 0), 0);
      const next = freesNext(st);
      return [
        pageHead('Queue', waiting ? [h('b', num(waiting)), ' waiting', oldest ? [' · oldest ', h('b', dur(Math.round(oldest + drift())))] : null,
          limitS ? [' · a request gives up after ', h('b', dur(limitS)), ' without a claim'] : null] : 'Nothing is waiting'),
        staleBanner(),
        h('div.cols',
          h('div.wide',
            h('section.card',
              h('div.ch.split', h('h2', 'Waiting now'), waiting ? h('span.note', 'Oldest first') : null),
              queue.length || loading.length ? [queue.map(e => entryCard(st, e, limitS)), loading.map(i => loadingCard(st, i))]
                : empty('Nothing is waiting', 'A request waits here when its model is not loaded and no node can take it at once. It starts as soon as a node frees up, or is answered with an error when the time limit passes.')),
            h('section.card',
              h('div.ch.split', h('h2', 'Turned away recently'), h('a', { href: href('activity', null, { type: 'refusals' }), style: 'font-size:13px' }, 'All activity')),
              !refused ? skeleton(2) : table({
                minWidth: 640, rows: refused, key: e => e.seq + '-' + e.ts,
                columns: [
                  { label: 'When', cls: 'tight', render: e => ago(e.ts) },
                  { label: 'Profile', render: e => h('span.mono.nowrap', e.profile || '–') },
                  { label: 'Requests', cls: 'r', render: e => num(((e.data || {}).waiters || 1) + ((e.data || {}).suppressed_since_last || 0)) },
                  { label: 'Why', render: e => { const d = e.data || {}; return h('span', { title: d.message ? 'The scheduler says: ' + d.message : null }, h('span', { style: 'font-weight:700' }, refusalWord(d.reason) + '.'), d.message ? h('span.sub', plainReason(d.message)) : null); } },
                ],
                empty: empty(historyOn ? 'No request was turned away' : 'History is off',
                  historyOn ? 'Requests that cannot be served (the model does not fit, a node is missing, the time limit passed) are listed here.'
                    : 'Refused requests are kept in the event history, which is not switched on (the scheduler was started without --history-dir).'),
              }))),
          h('div.side',
            h('section.card',
              h('div.ch', h('h2', 'What frees up next')),
              next.length ? next.map(i => h('div.bl', h('span', i.what), i.t != null ? h('time', { title: clock(i.t) }, (i.approx ? 'in ≈ ' : 'in ') + dur(Math.max(0, Math.round((i.t - now()) / 1000)))) : h('span.when', i.when),
                h('span.mut', { style: 'font-size:13px' }, i.detail)))
                : empty('Nothing is pending', 'No node is settling or set aside, and no load is in progress.')),
            h('section.card',
              h('div.ch', h('h2', 'Held back after a failure')),
              (st.backoffs || []).length || (st.quarantines || []).length ? [
                (st.backoffs || []).map(b => h('div.bl', h('span.mono', { style: 'font-weight:500' }, b.profile), badge(plural(b.level, 'failure'), 'aside'),
                  h('span.mut', { style: 'font-size:13px' }, 'Its last load failed. New requests for it are refused until ', at(b.until), ' (', until(b.until), ').'))),
                (st.quarantines || []).map(q => h('div.bl', h('span', nodeChip(nodeName(st, q.node))), badge(plural(q.level, 'failed check'), 'aside'),
                  h('span.mut', { style: 'font-size:13px' }, 'Set aside until ', at(q.until), ' (', until(q.until), '). Other nodes are used meanwhile.'))),
              ] : empty('Nothing is held back', 'After a failed load the profile is refused for a while, and a node that fails its pre-load check is set aside. Both would show here.')))),
      ];
    }, ['status', 'tick', 'conn']);
    loadRefused();
    loadLimit();
    return { unmount() { alive = false; offEvent(); offConfig(); view.unmount(); } };
  },
});
