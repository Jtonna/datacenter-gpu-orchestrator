// Placement preview: pick a profile and see what a request for it would do
// right now: where it would go, what it would unload, or why it cannot be
// placed. A dry run; nothing is loaded or changed.
//
// Reads: REST GET /api/profiles (the list with a short plan each) and
// POST /api/plan {"model": id} (the full dry-run plan of the picked profile),
// both again when the placement picture in the WebSocket "status" changes.
// Route: #/placement/<profile id>

import { h } from '../core/dom.js';
import { registerView, navigate, href } from '../core/router.js';
import { state, on, listen } from '../core/store.js';
import * as api from '../core/api.js';
import { liveView, pageHead, badge, bar, stat, empty, skeleton, banner } from '../core/ui.js';
import { num, mib, ctx, plural, DASH } from '../core/format.js';
import { nodeName, findNode, staleBanner, plainReason } from '../core/status.js';

const ACTION = {
  serve: ['Already loaded', 'ready', 'A loaded copy would answer the request at once.'],
  wait: ['Loading now', 'load', 'It is loading already; the request would wait for that load.'],
  start: ['Would start now', 'use', 'Free nodes can take it, so the load would start at once.'],
  evict: ['Would make room', 'q', 'It would claim nodes that hold another model, and unload that model first.'],
  queue: ['Would wait', 'n', 'No node can take it now. The request would wait in the queue.'],
  backoff: ['Held back', 'aside', 'Its last load failed. A request would be refused for now.'],
  reject: ['Cannot be placed', 'fail', 'A request would be refused at once.'],
  unknown: ['Not known', 'n', ''],
};
const act = a => ACTION[a] || ACTION.unknown;

function detail(st, p, plan, error) {
  if (error) return h('section.card', empty('The plan could not be calculated', error));
  if (!plan) return h('section.card', skeleton(5));
  const [label, kind, text] = act(plan.action);
  const placement = plan.placement || [];
  const budget = placement.reduce((s, n) => s + (n.budget_mib || 0), 0);
  const need = plan.fit_need_mib || plan.need_mib || 0;
  const learned = (st.learned_needs || {})[plan.model] || null;
  const share = id => (learned && (learned.nodes || []).find(n => n.node === id)) || null;
  return h('section.card', { key: 'plan-' + plan.model },
    h('div.ch', h('h2.mono', plan.model), badge(label, kind), h('span.note', text),
      plan.alias ? badge('Asked by an alias', 'n') : null),
    h('div.stats',
      stat('Needs', need ? num(need) : DASH, need ? 'MiB' : null, { title: plan.need_basis }),
      p ? stat('Context', ctx(p.ctx)) : null,
      placement.length ? stat('Nodes', num(placement.length)) : null,
      placement.length ? stat('Usable there', num(budget), 'MiB') : null,
      plan.capacity_mib ? stat('Cluster can hold', num(plan.capacity_mib), 'MiB') : null,
      plan.required_mib ? stat('It would need', num(plan.required_mib), 'MiB', { cls: 'err-t' }) : null),
    plan.need_basis ? h('div.cb.mut', { style: 'font-size:13px;font-weight:600;margin-top:-6px' }, 'The need is ', plan.need_basis, plan.fit_need_mib && plan.need_mib && plan.fit_need_mib !== plan.need_mib ? ` (${mib(plan.need_mib)} before the margin)` : '', '.') : null,
    plan.warning ? h('div.cb', banner('warn', plan.warning)) : null,
    plan.reason ? h('div.cb', banner(plan.action === 'reject' || plan.action === 'backoff' ? 'err' : '', [h('b', plan.action === 'queue' ? 'Why it would wait: ' : 'Why: '), plainReason(plan.reason),
      h('div.mono', { style: 'font-size:12px;margin-top:6px;opacity:.8;font-weight:400' }, 'The scheduler says: ', plan.reason)])) : null,
    plan.capacity_mib && plan.required_mib ? h('div.cb', bar([{ cls: 'x', value: plan.capacity_mib }], plan.required_mib,
      `The cluster can hold ${mib(plan.capacity_mib)} of the ${mib(plan.required_mib)} this profile would need`, 'lg'),
    h('div.mono.mut', { style: 'font-size:12px;margin-top:6px' }, `${num(plan.capacity_mib)} of ${num(plan.required_mib)} MiB: the whole cluster is too small for it`)) : null,
    (plan.victims || []).length ? h('div.cs', h('div', { style: 'display:flex;flex-direction:column;gap:8px' },
      h('b', 'To make room it would unload'),
      plan.victims.map(v => h('div.hit', h('i', { class: v.busy ? 'a' : '' }), h('span', h('a.mono', { href: href('models') }, v.profile),
        v.busy ? ', which is serving requests: it would finish them first (drain), then unload.' : ', which is idle: it would be unloaded at once.'))))) : null,
    placement.length ? [
      h('div.day', 'Where it would go'),
      placement.map(n => {
        const name = nodeName(st, n.id), node = findNode(st, n.id), s = share(n.id);
        const even = need / placement.length;
        const expect = s ? need * s.fraction : even;
        const over = n.budget_mib && expect > n.budget_mib;
        return h('div.nr', { key: 'p-' + n.id },
          h('span.mono', name, n.local ? [' ', badge('This machine', 'n')] : null),
          bar([{ cls: over ? 'x' : 'rsv', value: Math.min(expect, n.budget_mib || expect) }], n.budget_mib || expect, `${name}: about ${mib(expect)} of ${mib(n.budget_mib)} usable`),
          h('span.mono.fig', `≈ ${num(Math.round(expect))} / ${mib(n.budget_mib)}`),
          h('span.mono.mut', s ? `${Math.round(s.fraction * 100)}% of the model, as measured on the last load` : placement.length > 1 ? 'An even share; the real split is known after a load' : node && node.holder ? 'After ' + node.holder + ' is unloaded' : 'The whole model'));
      }),
    ] : null);
}

registerView({
  id: 'placement', title: 'Placement preview',
  mount(el, route) {
    let profiles = null, error = '', plan = null, planError = '', planFor = '', sig = '', timer = 0, alive = true;
    const picked = () => route.rest[0] || '';
    const loadPlan = async () => {
      const id = picked();
      if (!id) { plan = null; planFor = ''; return; }
      if (planFor !== id) { plan = null; planError = ''; planFor = id; if (alive) view.refresh(); }
      try { const r = await api.post('/api/plan', { model: id }); if (picked() === id) { plan = r; planError = ''; } } catch (e) { if (picked() === id) { planError = e.message; plan = null; } }
      if (alive) view.refresh();
    };
    const loadAll = async () => {
      try { profiles = (await api.get('/api/profiles')).profiles || []; error = ''; } catch (e) { error = e.message; }
      if (alive) view.refresh();
      if (!picked() && profiles && profiles.length) { navigate('placement', [profiles[0].id], null, true); return; }
      loadPlan();
    };
    const picture = st => JSON.stringify([(st.instances || []).map(i => [i.profile, i.state, i.in_flight > 0]), (st.queue || []).map(q => [q.profile, q.claimed]),
      (st.backoffs || []).map(b => b.profile), (st.quarantines || []).length, ((st.view || {}).nodes || []).map(n => [n.online, n.eligible, n.holder, n.live_budget_mib]), (st.view || {}).stale, st.config_revision]);
    const offStatus = on('status', st => {
      const s = picture(st || {});
      if (s === sig) return;
      const first = sig === '';
      sig = s;
      clearTimeout(timer);
      if (first) loadAll(); else timer = setTimeout(loadAll, 600);
    });
    const offConfig = listen('config', () => loadAll());

    const view = liveView(el, () => {
      const st = state.status || {};
      const id = picked();
      const p = (profiles || []).find(x => x.id === id) || null;
      return [
        pageHead('Placement preview', 'What a request for a profile would do right now. Nothing is loaded from here.'),
        staleBanner(),
        h('div.cols',
          h('section.card.side', { style: 'gap:0;max-width:440px;flex:1 1 340px' },
            h('div.ch.split', h('h2', 'Profiles'), profiles ? h('span.note', plural(profiles.length, 'profile')) : null),
            error ? empty('The profile list could not be read', error) : !profiles ? skeleton(6)
              : !profiles.length ? empty('No profiles yet', 'A profile says which model file to load and how. Add one under Configure, Profiles.')
                : profiles.map(x => {
                  const [label, kind] = act((x.plan || {}).action);
                  return h('a', { key: 'pp-' + x.id, href: href('placement', [x.id]), 'aria-current': x.id === id ? 'true' : null,
                    style: 'display:grid;grid-template-columns:minmax(0,1fr) auto;gap:2px 10px;padding:10px 20px;border-top:1px solid var(--line);color:var(--text);font-weight:500;align-items:center;' + (x.id === id ? 'background:var(--fill);box-shadow:inset 3px 0 0 var(--mint)' : '') },
                  h('span.mono', { style: 'overflow:hidden;text-overflow:ellipsis;white-space:nowrap', title: x.id }, x.id), badge(label, kind),
                  h('span.mono.mut', { style: 'font-size:12px;grid-column:1/-1' }, x.needs_mib ? mib(x.needs_mib) : 'need not known', ' · ', ctx(x.ctx), ' context'));
                })),
          h('div.wide',
            !id ? h('section.card', empty('Pick a profile', 'Choose a profile on the left to see where it would be placed.')) : detail(st, p, plan, planError),
            h('p.mut', { style: 'font-size:13px;font-weight:600' }, 'This is a dry run of the scheduler’s own placement rules. It is recalculated whenever the cluster changes, so it always describes this moment.'))),
      ];
    }, ['status', 'conn']);
    if (state.status) { sig = picture(state.status); loadAll(); }
    return {
      update(r) { route = r; loadPlan(); view.refresh(); },
      unmount() { alive = false; clearTimeout(timer); offStatus(); offConfig(); view.unmount(); },
    };
  },
});
