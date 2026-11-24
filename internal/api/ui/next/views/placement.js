// Placement preview, rebuilt for the new shell (the owner, 24 Nov): a form
// (the profile, and where: anywhere the scheduler would choose, or only on
// chosen zones) and a result card: the verdict in words, what it needs and
// where it would go, what it would unload first, why it would wait or be
// refused, and the memory table. A dry run: nothing is loaded from here.
//
// Reads: GET /api/profiles (the picker; generated benchmark profiles in their
// own group), POST /api/plan {model} or {model, nodes} (changes nothing),
// again when the placement picture in the WebSocket "status" changes or the
// configuration changes.
// Route: #/placement/<profile id>, #/placement/<profile id>?zones=a,b

import { h } from '../core/dom.js';
import { registerView, navigate, href } from '../core/router.js';
import { state, on, listen } from '../core/store.js';
import * as api from '../core/api.js';
import { liveView, pageHead, badge, bar, stat, empty, skeleton, banner, more } from '../core/ui.js';
import { num, mib, gb, gbNum, ctx, plural, DASH } from '../core/format.js';
import { memoryTable } from '../core/reserve.js';
import { nodeName, findNode, staleBanner, plainReason, plainBasis, machines } from '../core/status.js';

export const ACTION = {
  serve: ['Already loaded', 'ready', 'A loaded copy would answer the request at once.'],
  wait: ['Loading now', 'load', 'It is loading already; the request would wait for that load.'],
  start: ['Would start now', 'use', 'Free zones can take it, so the load would start at once.'],
  evict: ['Would make room', 'q', 'It would claim zones that hold another model, and unload that model first.'],
  queue: ['Would wait', 'n', 'No zone can take it now. The request would wait in the queue.'],
  backoff: ['Held back', 'aside', 'Its last load failed. A request would be refused for now.'],
  reject: ['Cannot be placed', 'fail', 'A request would be refused at once.'],
  unknown: ['Not known', 'n', ''],
};
const act = a => ACTION[a] || ACTION.unknown;
const isGen = x => (typeof x.generated === 'boolean' ? x.generated : /^(bench|b|m)-/.test(x.id));

// result(st, p, plan, error, chosen, rawOpen, toggleRaw) -> the result card.
function result(st, p, plan, error, chosen, rawOpen, toggleRaw) {
  if (error) return h('section.card.pl-res', { key: 'res-err' }, empty('The plan could not be calculated', error));
  if (!plan) return h('section.card.pl-res', { key: 'res-wait' }, skeleton(5));
  const [label, kind, text] = act(plan.action);
  const placement = plan.placement || [];
  const budget = placement.reduce((s, n) => s + (n.budget_mib || 0), 0);
  const need = plan.fit_need_mib || plan.need_mib || 0;
  const learned = (st.learned_needs || {})[plan.model] || null;
  const share = id => (learned && (learned.nodes || []).find(n => n.node === id)) || null;
  return h('section.card.pl-res', { key: 'res-' + plan.model, 'data-result': plan.action || 'unknown' },
    h('div.pl-verdict', badge(label, kind), h('span.pl-say', text),
      chosen.length ? h('span.note', { title: chosen.join(', ') }, 'only on ' + plural(chosen.length, 'chosen zone')) : h('span.note', 'anywhere the scheduler chooses'),
      plan.alias ? badge('Asked by an alias', 'n') : null),
    h('div.stats',
      stat('Needs', need ? gbNum(need) : DASH, need ? 'GB' : null, { title: need ? mib(need) : null }),
      p ? stat('Context', ctx(p.ctx)) : null,
      placement.length ? stat('On', machines(st, placement.length)) : null,
      placement.length ? stat('Usable there', gbNum(budget), 'GB', { title: mib(budget) }) : null,
      plan.capacity_mib ? stat('Fleet can hold', gbNum(plan.capacity_mib), 'GB', { title: mib(plan.capacity_mib) }) : null,
      plan.required_mib ? stat('It would need', gbNum(plan.required_mib), 'GB', { title: mib(plan.required_mib) + ' with the safety margins' }) : null),
    plan.need_basis ? h('div.pl-line.note-s', 'The need: ', plainBasis(plan.need_basis).replace(/^./, c => c.toLowerCase()), '.') : null,
    plan.warning ? h('div.pl-line', banner('warn', plan.warning)) : null,
    plan.reason ? h('div.pl-line', h('div.banner', { role: 'status', 'data-reason': '' },
      h('span.grow', h('b', plan.action === 'queue' ? 'Why it would wait: ' : 'Why: '), plainReason(plan.reason), rawOpen ? h('div.raw', plan.reason) : null),
      more(rawOpen, toggleRaw, 'The scheduler’s words', 'Hide'))) : null,
    plan.capacity_mib && plan.required_mib ? h('div.pl-line', bar([{ cls: 'o', value: plan.capacity_mib }], plan.required_mib,
      `The fleet can hold ${gb(plan.capacity_mib)} of the ${gb(plan.required_mib)} this profile would need`, 'lg'),
    h('div.note-s', { style: 'margin-top:6px' }, `The whole fleet holds ${gb(plan.capacity_mib)} of the ${gb(plan.required_mib)} it would need.`)) : null,
    (plan.victims || []).length ? h('div.pl-part', h('h3', 'To make room it would unload'),
      plan.victims.map(v => h('div.hit', h('i', { class: v.busy ? 'a' : '' }), h('span', h('a.mono', { href: href('models') }, v.profile),
        v.busy ? ', which is serving requests: it would wait for them to end (drain), then unload.' : ', which is idle: it would be unloaded at once.')))) : null,
    placement.length ? h('div.pl-part', h('h3', 'Where it would go'),
      placement.map(n => {
        const name = nodeName(st, n.id), node = findNode(st, n.id), s = share(n.id);
        const even = need / placement.length;
        const expect = s ? need * s.fraction : even;
        const over = n.budget_mib && expect > n.budget_mib;
        return h('div.nr', { key: 'p-' + n.id },
          h('span.mono', name, n.local ? [' ', badge('This machine', 'n')] : null),
          bar([{ cls: over ? 'x' : 'rsv', value: Math.min(expect, n.budget_mib || expect) }], n.budget_mib || expect, `${name}: about ${mib(expect)} of ${mib(n.budget_mib)} usable`),
          h('span.mono.fig', { title: `about ${mib(expect)} of ${mib(n.budget_mib)} usable` }, `≈ ${gbNum(expect)} / ${gb(n.budget_mib)}`),
          h('span.mut.split', s ? `${Math.round(s.fraction * 100)}% of the model, as measured on its last load` : placement.length > 1 ? 'An even share; the real split is known after a load' : node && node.holder ? 'After ' + node.holder + ' is unloaded' : 'The whole model'));
      })) : null,
    plan.memory ? h('div.pl-part', memoryTable(plan.memory, id => nodeName(st, id))) : null);
}

registerView({
  id: 'placement', title: 'Placement preview',
  mount(el, route) {
    let profiles = null, error = '', plan = null, planError = '', planFor = '', sig = '', timer = 0, alive = true, rawOpen = false, seq = 0;
    const picked = () => route.rest[0] || '';
    const chosen = () => String(route.query.zones || '').split(',').map(s => s.trim()).filter(Boolean);
    const where = () => (route.query.where === 'chosen' || chosen().length ? 'chosen' : 'any');
    const key = () => picked() + '|' + (where() === 'chosen' ? chosen().join(',') : '');
    const go = (id, zones, w) => {
      const q = {};
      if (w === 'chosen') { q.where = 'chosen'; if (zones.length) q.zones = zones.join(','); }
      navigate('placement', id ? [id] : [], q, true);
    };

    const loadPlan = async () => {
      const id = picked(), k = key(), zs = chosen();
      if (!id) { plan = null; planFor = ''; return; }
      if (where() === 'chosen' && !zs.length) { plan = null; planError = ''; planFor = k; if (alive) view.refresh(); return; }
      if (planFor !== k) { plan = null; planError = ''; planFor = k; if (alive) view.refresh(); }
      const n = ++seq;
      try {
        const r = await api.post('/api/plan', zs.length && where() === 'chosen' ? { model: id, nodes: zs } : { model: id });
        if (n === seq && key() === k) { plan = r; planError = ''; }
      } catch (e) { if (n === seq && key() === k) { planError = (e && e.body && e.body.error && e.body.error.message) || e.message; plan = null; } }
      if (alive) view.refresh();
    };
    const loadAll = async () => {
      try { profiles = (await api.get('/api/profiles')).profiles || []; error = ''; } catch (e) { error = e.message; }
      if (alive) view.refresh();
      if (!picked() && profiles && profiles.length) { go((profiles.find(x => !isGen(x)) || profiles[0]).id, [], 'any'); return; }
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

    // the form: built from plain controls; each change goes through the address
    function form(st) {
      const id = picked();
      const list = profiles || [];
      const sel = h('select.select', { 'aria-label': 'Profile', 'data-pl': 'profile', disabled: !list.length },
        !profiles ? h('option', { value: '' }, error ? 'The profiles could not be read' : 'Reading the profiles…')
          : !list.length ? h('option', { value: '' }, 'No profile yet')
            : [list.filter(x => !isGen(x)).map(x => h('option', { value: x.id, selected: x.id === id }, x.id)),
              list.some(isGen) ? h('optgroup', { label: 'Generated for benchmarks' }, list.filter(isGen).map(x => h('option', { value: x.id, selected: x.id === id }, x.id))) : null]);
      sel.addEventListener('change', () => go(sel.value, chosen(), where()));
      const p = list.find(x => x.id === id);
      const nodes = ((st.view || {}).nodes || []).filter(n => !n.removed)
        .map(n => ({ n, name: nodeName(st, n.id) })).sort((a, b) => a.name.localeCompare(b.name, undefined, { numeric: true }));
      const zs = new Set(chosen());
      const radio = (v, text, sub) => {
        const r = h('input', { type: 'radio', name: 'pl-where', value: v, checked: where() === v, 'data-where': v });
        r.addEventListener('change', () => { if (r.checked) go(id, v === 'chosen' ? [...zs] : [], v); });
        return h('label', { class: 'pl-opt' + (where() === v ? ' on' : '') }, r, h('span', h('b', text), h('span.mut', sub)));
      };
      return h('section.card.pl-form', { key: 'form' },
        h('div.pl-row',
          h('label.pl-lab', 'Profile'),
          h('div.pl-ctl', sel, p ? h('span.mut', { 'data-pl': 'facts' }, [p.model ? h('span.mono', p.model) : null, p.needs_mib ? ' · needs ' + gb(p.needs_mib) : ' · need not known', p.ctx ? ' · ' + ctx(p.ctx) + ' context' : ''].filter(Boolean)) : null)),
        h('div.pl-row',
          h('span.pl-lab', 'Where'),
          h('div.pl-ctl.col', { role: 'radiogroup', 'aria-label': 'Where' },
            radio('any', 'Anywhere the scheduler chooses', 'What an ordinary request for it would do now.'),
            radio('chosen', 'Only on chosen zones', 'What Load a model on these zones would do (the Zones page).'),
            where() === 'chosen' ? h('div.pl-zones', { 'data-pl': 'zones' }, nodes.length ? nodes.map(({ n, name }) => {
              const c = h('input', { type: 'checkbox', checked: zs.has(name), 'data-zone': name });
              c.addEventListener('change', () => { if (c.checked) zs.add(name); else zs.delete(name); go(id, [...zs], 'chosen'); });
              return h('label.pl-z', c, h('span.mono', name), n.auto === false ? badge('Named only', 'n', 'Takes a model only when a load names it (no_auto_nodes)') : null,
                n.online === false ? h('span.mut', ' · offline') : null);
            }) : h('span.mut', 'No zone is known yet.')) : null)),
        h('p.mut.pl-foot', 'A dry run of the scheduler’s own placement rules: nothing is loaded from here. It is worked out again whenever the fleet changes.'));
    }

    const view = liveView(el, () => {
      const st = state.status || {};
      const id = picked();
      const p = (profiles || []).find(x => x.id === id) || null;
      return [
        pageHead('Placement preview', 'What a request for a profile would do right now.'),
        staleBanner(),
        form(st),
        !profiles && !error ? h('section.card.pl-res', { key: 'res-load' }, skeleton(4))
          : error ? h('section.card.pl-res', { key: 'res-perr' }, empty('The profile list could not be read', error))
            : !profiles.length ? h('section.card.pl-res', { key: 'res-none' }, empty('No profiles yet', 'A profile says which model file to load and how. Add one under Settings, Profiles.'))
              : !id ? h('section.card.pl-res', { key: 'res-pick' }, empty('Choose a profile', 'Pick a profile above to see where it would be placed.'))
                : where() === 'chosen' && !chosen().length ? h('section.card.pl-res', { key: 'res-zones' }, empty('Choose the zones', 'Tick one zone or more to see what a load on them would do.'))
                  : result(st, p, plan, planError, where() === 'chosen' ? chosen() : [], rawOpen, () => { rawOpen = !rawOpen; view.refresh(); }),
      ];
    }, ['status', 'conn']);
    if (state.status) { sig = picture(state.status); loadAll(); }
    else loadAll();
    return {
      update(r) { route = r; rawOpen = false; loadPlan(); view.refresh(); },
      unmount() { alive = false; clearTimeout(timer); offStatus(); offConfig(); view.unmount(); },
    };
  },
});
