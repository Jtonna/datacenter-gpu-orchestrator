// Placement preview (the gear's "Placement preview"; no mockup: inferred
// from the design language, phase 5). Replaced the classic page
// (the classic UI is removed). Ask what a request for a profile
// would do right now, optionally on zones you choose, and read the answer as
// one result card: the verdict with the scheduler's sentence, the memory
// line, where it would go (zone by zone: usable, free, what it takes, what
// is left), what it would unload, and where llama-server would run. The raw
// answer is one click away ("Show the full answer"), never a wall of JSON.
//
// A dry run: POST /api/plan writes nothing, loads nothing.
//
// Reads: REST GET /api/profiles (the picker), POST /api/plan {model, nodes?}
// (the result), again when the placement picture of the WebSocket status
// changes; the zones from the status (view.nodes); the catalog for the
// model's short name.
// Route: #/placement/<profile>?zones=<id,id>&q=<search>&gen=1

import { h } from '../core/dom.js';
import { registerView, navigate, href } from '../core/router.js';
import { state, on, listen } from '../core/store.js';
import * as api from '../core/api.js';
import { liveView, pageHead, badge, banner, empty, skeleton, searchField, segmented, fileShort, profileLabel, profileStage } from '../core/ui.js';
import { num, gb, mib, ctx, plural } from '../core/format.js';
import { nodeName, viewNodes, plainReason, plainBasis } from '../core/status.js';
import { catalog, onCatalog, needCatalog } from '../core/catalog.js';
import { memoryTable } from '../core/reserve.js';

// the verdicts: label, badge kind, the plain sentence
export const VERDICT = {
  serve: ['Already loaded', 'ready', 'A loaded copy would answer the request at once.'],
  wait: ['Loading now', 'load', 'It is loading already; the request would wait for that load.'],
  start: ['Would start now', 'use', 'Free zones can take it, so the load would start at once.'],
  evict: ['Would make room', 'q', 'It would claim zones that hold other models, and unload those first.'],
  queue: ['Would wait', 'n', 'No zone can take it now. The request would wait in the queue.'],
  backoff: ['Held back', 'aside', 'Its last load failed. A request would be refused for now.'],
  reject: ['Cannot be placed', 'fail', 'A request would be refused at once.'],
  unknown: ['Not known', 'n', ''],
};
export const verdictOf = action => VERDICT[action] || VERDICT.unknown;
const LIST_MAX = 60; // rows drawn in the picker; the search narrows the rest

// shortModel(file) -> the model's short name ("gemma-4-27b"): the catalog's short_name, else made from the file name
export function shortModel(file) {
  const m = file && catalog.byKey[file];
  return (m && (m.short_name || m.name)) || (file ? fileShort(file) : '');
}

// tightText(reason) -> the tight-fit sentence without its own "tight fit:"
// lead-in, which the banner's bold "Tight fit." already says.
export function tightText(reason) {
  const t = String(plainReason(reason) || '').replace(/^\s*tight fit\s*[:.,-]\s*/i, '');
  return t ? t.charAt(0).toUpperCase() + t.slice(1) : '';
}

registerView({
  id: 'placement', title: 'Placement preview',
  mount(el, route) {
    let profiles = null, error = '', plan = null, planError = '', planKey = '', alive = true, raw = false, sig = '', timer = 0;
    const picked = () => route.rest[0] || '';
    const chosen = () => (route.query.zones ? route.query.zones.split(',').filter(Boolean) : []);
    const only = () => route.query.only === '1' || chosen().length > 0;
    const search = () => route.query.q || '';
    const showGen = () => route.query.gen === '1';
    const go = (rest, q) => navigate('placement', rest, Object.assign({ zones: chosen().join(',') || null, q: search() || null, gen: showGen() ? '1' : null, only: route.query.only || null }, q), true);

    async function loadProfiles() {
      try { profiles = (await api.get('/api/profiles')).profiles || []; error = ''; } catch (e) { error = e.message; }
      if (alive) view.refresh();
    }
    async function loadPlan(force) {
      const id = picked();
      const key = id + '|' + chosen().join(',');
      if (!id) { plan = null; planKey = ''; planError = ''; return; }
      if (planKey !== key) { plan = null; planError = ''; planKey = key; raw = false; if (alive) view.refresh(); }
      else if (!force) return;
      try {
        const body = { model: id };
        if (chosen().length) body.nodes = chosen();
        const r = await api.post('/api/plan', body);
        if (planKey === key) { plan = r; planError = ''; }
      } catch (e) { if (planKey === key) { planError = e.message; plan = null; } }
      if (alive) view.refresh();
    }
    // the placement picture: a change re-asks (debounced)
    const picture = st => JSON.stringify([(st.instances || []).map(i => [i.profile, i.state, i.in_flight > 0]), (st.queue || []).map(q => q.profile),
      (st.backoffs || []).map(b => b.profile), viewNodes(st).map(n => [n.id, n.online, n.eligible, n.holder, n.live_budget_mib]), st.config_revision]);
    const offStatus = on('status', st => {
      const s = picture(st || {});
      if (s === sig) return;
      const first = sig === '';
      sig = s;
      clearTimeout(timer);
      if (!first) timer = setTimeout(() => loadPlan(true), 600);
    });
    const offConfig = listen('config', () => { loadProfiles(); loadPlan(true); });
    const offCatalog = onCatalog(() => view.refresh());
    needCatalog();

    // ---- the form ----
    function picker() {
      const all = profiles || [];
      const q = search().trim().toLowerCase();
      // production first, the test profiles under their shown names (profiles plan, section 9)
      const isTest = p => profileStage(p) === 'test';
      const nGen = all.filter(isTest).length;
      const hit = p => !q || p.id.toLowerCase().includes(q) || profileLabel(p).toLowerCase().includes(q) || String(p.model || '').toLowerCase().includes(q) || (p.aliases || []).some(a => a.toLowerCase().includes(q)) || (p.description || '').toLowerCase().includes(q);
      // test profiles stay out until asked for or searched (README rule 9), and come after the production ones
      const shown = all.filter(p => hit(p) && (q || showGen() || !isTest(p) || p.id === picked()))
        .sort((a, b) => Number(isTest(a)) - Number(isTest(b)));
      const list = shown.slice(0, LIST_MAX);
      return h('div.plc-sec', { key: 'pick' },
        h('div.plc-lab', h('label', { for: 'plc-search' }, 'Profile'), profiles ? h('span.mut', plural(all.length - nGen, 'profile') + (nGen ? ' · ' + num(nGen) + ' test' : '')) : null),
        searchField(search(), v => go(route.rest, { q: v || null }), 'Search profiles and models', 'plc-search', { size: 'lg', width: 9999 }),
        error ? h('div.err-t', 'The profiles could not be read: ' + error)
          : !profiles ? skeleton(4)
            : h('div.plc-list', { role: 'listbox', 'aria-label': 'Profiles' },
              !shown.length ? h('div.plc-none', all.length ? 'No profile matches.' : 'No profiles yet. Add one under Profiles.') : null,
              list.map(p => {
                const [label, kind] = verdictOf(p.plan && p.plan.action);
                const on = p.id === picked();
                return h('button', { class: 'plc-opt' + (on ? ' on' : ''), type: 'button', role: 'option', 'aria-selected': on ? 'true' : 'false', key: 'o-' + p.id, 'data-profile': p.id,
                  onclick: () => go([p.id], {}) },
                  h('span.plc-oname', h('span', { title: p.id, 'data-shown': '' }, profileLabel(p)), isTest(p) ? h('span.plc-gen', 'test') : null),
                  h('span.plc-osub', [shortModel(p.model), p.ctx ? ctx(p.ctx) + ' context' : null].filter(Boolean).join(' · ')),
                  h('span', { class: 'plc-dot k-' + kind, title: label, 'aria-label': label }));
              }),
              shown.length > list.length ? h('div.plc-none', { key: 'more' }, num(shown.length - list.length) + ' more: search to narrow the list.') : null),
        profiles && nGen && !q ? h('label.check.plc-gentoggle', h('input.cb', { type: 'checkbox', checked: showGen(), 'data-gen': '', onchange() { go(route.rest, { gen: this.checked ? '1' : null }); } }),
          'Show test profiles too (' + num(nGen) + ')') : null);
    }

    function zonesPart(st) {
      const nodes = viewNodes(st);
      const pick = chosen();
      const toggleZone = (id, onNow) => {
        const next = onNow ? pick.concat(id) : pick.filter(x => x !== id);
        go(route.rest, { zones: next.join(',') || null, only: '1' });
      };
      return h('div.plc-sec', { key: 'zones' },
        h('div.plc-lab', h('span', 'Zones'), pick.length ? h('span.mut', plural(pick.length, 'zone') + ' chosen') : null),
        segmented([{ id: 'any', label: 'Any zone' }, { id: 'only', label: 'Only these' }], only() ? 'only' : 'any',
          v => go(route.rest, v === 'any' ? { zones: null, only: null } : { only: '1' }), 'Where'),
        only() ? [
          h('p.plc-note', 'The scheduler places it on the zones you tick, as force_nodes would, without changing the profile.'),
          !nodes.length ? h('div.plc-none', 'No zone is known yet.') : h('div.plc-zones', { role: 'group', 'aria-label': 'Zones to place it on' }, nodes.map(n => h('label', { class: 'plc-zone check', key: 'z-' + n.id },
            h('input.cb', { type: 'checkbox', checked: pick.includes(n.id), 'data-zone': n.hostname || n.id, 'aria-label': nodeName(st, n.id), onchange() { toggleZone(n.id, this.checked); } }),
            h('span.mono', nodeName(st, n.id)),
            n.auto === false ? badge('Named only', 'line', 'In no_auto_nodes: automatic placement never picks it; a profile that names it, or this preview, may') : null,
            n.excluded ? badge('Off', 'dash', 'Switched off (exclude_nodes)') : !n.online ? badge('Offline', 'aside') : null))),
        ] : null);
    }

    // ---- the result ----
    function result(st) {
      const id = picked();
      if (!id) return h('section.panel.plc-result', { key: 'none' }, empty('Pick a profile', 'Choose a profile on the left to see what a request for it would do now.'));
      if (planError) return h('section.panel.plc-result', { key: 'err' }, empty('The plan could not be worked out', planError));
      if (!plan) return h('section.panel.plc-result', { key: 'wait' }, skeleton(6));
      const p = (profiles || []).find(x => x.id === plan.model) || null;
      const [label, kind, sentence] = verdictOf(plan.action);
      const placement = plan.placement || [];
      const memNodes = new Map(((plan.memory && plan.memory.nodes) || []).map(n => [n.id, n]));
      const room = placement.reduce((s, n) => s + (n.budget_mib || 0), 0);
      const need = plan.need_mib || 0, fitNeed = plan.fit_need_mib || need;
      const basis = plan.need_basis ? plainBasis(plan.need_basis) : '';
      const zoneName = zid => nodeName(st, zid);
      const why = plan.reason && !plan.tight_fit ? plainReason(plan.reason) : '';
      return h('section.panel.plc-result', { key: 'r-' + planKey, 'aria-label': 'Result' },
        h('div.plc-rhead',
          h('div.grow',
            h('div.lbl', chosen().length ? 'Result on ' + plural(chosen().length, 'chosen zone') : 'Result'),
            h('div.plc-rtitle', h('h2', { title: plan.model, 'data-shown': '' }, p ? profileLabel(p) : plan.model), p && profileLabel(p) !== plan.model ? h('span.mono.mut', plan.model) : null, badge(label, kind), plan.tight_fit ? badge('Tight fit', 'load', 'It fits only without its margin on at least one zone') : null,
              plan.alias ? badge('Asked by an alias', 'n') : null),
            h('div.mut', [p ? shortModel(p.model) : null, p && p.ctx ? ctx(p.ctx) + ' context' : null, p && p.parallel > 1 ? p.parallel + ' slots' : null].filter(Boolean).join(' · '))),
          h('a.btn.sm', { href: href('profiles', [], { profile: plan.model }) }, 'Open the profile')),
        h('div.plc-say', h('p', h('b', sentence)), why ? h('p', { title: plan.reason }, why) : null),
        plan.warning ? h('div.plc-pad', banner('warn', h('span', h('b', 'It would start anyway. '), h('span', { title: plan.warning }, plainReason(plan.warning))))) : null,
        plan.tight_fit && plan.reason ? h('div.plc-pad', banner('warn', h('span', h('b', 'Tight fit. '), h('span', { title: plan.reason }, tightText(plan.reason))))) : null,
        h('div.plc-mem', { 'aria-label': 'Memory' },
          fig('Needs', need ? gb(need) : 'not known', need ? mib(need) : null, basis || (plan.measured ? 'Measured' : 'Estimated')),
          fitNeed && fitNeed !== need ? fig('With its margin', gb(fitNeed), mib(fitNeed), 'What the fit rules use') : null,
          placement.length ? fig('Usable on its zones', gb(room), mib(room), plural(placement.length, 'zone')) : null,
          plan.required_mib ? fig('It would need', gb(plan.required_mib), mib(plan.required_mib), 'With the safety margins') : null,
          plan.capacity_mib ? fig('The fleet can hold', gb(plan.capacity_mib), mib(plan.capacity_mib), 'Every zone together') : null),
        placement.length ? h('div.plc-zt', h('table.tbl.dense',
          h('thead', h('tr', h('th', { scope: 'col' }, 'Zone'), h('th', { scope: 'col' }, 'Role'), h('th.r', { scope: 'col', title: 'What the scheduler may place there now' }, 'Usable'),
            h('th.r', { scope: 'col' }, 'Free now'), h('th.r', { scope: 'col', title: 'What this load would take there, its context filled' }, 'Takes'), h('th.r', { scope: 'col' }, 'Left'), h('th', { scope: 'col' }, h('span.sr', 'Fits')))),
          h('tbody', placement.map(n => {
            const m = memNodes.get(n.id);
            const left = m ? m.remains_mib : null;
            const mark = !m ? null : left < 0 ? badge('Short ' + gb(-left), 'fail') : m.tight ? badge('Tight', 'load', 'Less than its margin (' + mib(m.margin_mib) + ') would stay free') : badge('Fits', 'use');
            return h('tr', { key: 'z-' + n.id },
              h('td', h('span.mono', zoneName(n.id)), n.local ? h('span.sub', 'Runs Potluck') : null),
              h('td', n.role ? (n.role === 'host' ? 'Host' : 'Helper') + (n.share ? ' · ' + Math.round(n.share * 100) + '%' : '') : 'All of it'),
              h('td.r', { title: mib(n.budget_mib) }, gb(n.budget_mib)),
              h('td.r', m ? { title: mib(m.free_now_mib) } : null, m ? gb(m.free_now_mib) : '–'),
              h('td.r', m ? { title: mib(m.will_take_mib) + (m.basis ? ', ' + m.basis : '') } : null, m ? gb(m.will_take_mib) : '–'),
              h('td.r', m ? { title: mib(left) } : null, m ? (left < 0 ? '−' + gb(-left) : gb(left)) : '–'),
              h('td.tight', mark));
          })))) : null,
        (plan.victims || []).length ? h('div.plc-block', h('h3', 'To make room it would unload'),
          h('ul.plc-victims', plan.victims.map(v => h('li', { key: 'v-' + v.profile }, h('span.mono', v.profile),
            v.busy ? ' is serving requests: it finishes them first, then unloads.' : ' is idle: it unloads at once.')))) : null,
        hostLine(plan, zoneName),
        // the server's memory figures per zone, as the classic page showed them
        // (weights, context, reserve, headroom, margin; for shared memory
        // "available − floor − headroom = placeable" and what would be left)
        plan.memory && (plan.memory.nodes || []).length ? h('details.plc-raw', { 'data-memory-table': '' },
          h('summary', 'How the memory was worked out'), h('div.scroll', memoryTable(plan.memory, zoneName))) : null,
        h('details.plc-raw', { open: raw ? true : null, ontoggle() { raw = this.open; } },
          h('summary', 'Show the full answer'),
          h('pre.raw', JSON.stringify(plan, null, 2))));
    }

    const view = liveView(el, () => {
      const st = state.status || {};
      return [
        pageHead('Placement preview', 'What a request for a profile would do right now. A dry run: nothing is loaded or changed.'),
        h('div.plc', { key: 'plc' },
          h('section.panel.plc-ask', { key: 'ask', 'aria-label': 'Ask' }, h('div.plc-bar', h('h2', 'Ask the scheduler')), picker(), zonesPart(st)),
          result(st)),
      ];
    }, ['status', 'conn']);
    loadProfiles();
    loadPlan();
    if (state.status) sig = picture(state.status);
    return {
      update(r) { route = r; loadPlan(); view.refresh(); },
      unmount() { alive = false; clearTimeout(timer); offStatus(); offConfig(); offCatalog(); view.unmount(); },
    };
  },
});

// fig(label, value, hover, note) -> one figure of the memory line
function fig(label, value, hover, note) {
  return h('div.plc-fig', h('span.lbl', label), h('b', { title: hover || null }, value), note ? h('span.mut', note) : null);
}

// hostLine(plan, name) -> where llama-server would run (phase 6 hosting)
function hostLine(plan, name) {
  if (!plan.host || !(plan.placement || []).length) return null;
  const text = plan.host === 'node'
    ? ['llama-server would run on ', h('span.mono', name(plan.host_node)), plan.host_overhead_mib ? ', which also holds ' + gb(plan.host_overhead_mib) + ' for the host process' : '', '.']
    : ["llama-server would run on the scheduler's machine; the zones hold the model."];
  const hm = plan.host_memory;
  const conv = hm && (hm.size_unknown || hm.checkpoints_mib + hm.prompt_cache_mib > 0)
    ? h('p', { 'data-hostmem': '' }, 'Conversation memory of the host process: ', hm.size_unknown ? 'at least ' : 'up to ', gb(hm.checkpoints_mib + hm.prompt_cache_mib),
      hm.size_unknown ? ` (prompt cache ${gb(hm.prompt_cache_mib)}; restore points NOT included: size not yet known, read at the first load), `
        : ` (restore points ${gb(hm.checkpoints_mib)}${hm.from_log ? `, size as the server logged it at a load of ${hm.logged_file} at ctx ${hm.logged_ctx}` : ''}, prompt cache ${gb(hm.prompt_cache_mib)}), `,
      hm.on_node ? (hm.counted ? 'counted in what it needs' : 'not yet counted') : 'not placed', hm.set_note ? '; ' + hm.set_note + '.' : '.')
    : null;
  return h('div.plc-block', h('h3', 'Host'), h('p', text), conv, plan.host_problem ? h('p.err-t', 'It cannot host now: ' + plainReason(plan.host_problem)) : null);
}
