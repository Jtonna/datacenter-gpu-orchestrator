// Settings (Configure): every key of the settings, recovery and fleet scopes,
// grouped by schema section, generated from GET /api/config/schema. Each shows
// its value, unit, default, whether it was changed from the default, and
// whether it only takes effect after a restart. Edits collect in the pending
// bar; Review changes does a dry run and saves with If-Match and a comment.
//
// Reads: REST GET /api/config (settings, settings_set, recovery, fleet),
// GET /api/config/schema; WebSocket "config" (refresh, or warn with unsaved edits).
// Writes: PATCH /api/config/settings, /api/config/recovery, /api/config/fleet
// (only the keys that changed; null resets a key to its default).
// Route: #/settings.

import { h, frag } from '../core/dom.js';
import { registerView } from '../core/router.js';
import * as api from '../core/api.js';
import { liveView, pageHead, badge, banner, skeleton, button, schemaForm, toggle, confirm, empty } from '../core/ui.js';
import { num, plural, parseGoDuration } from '../core/format.js';
import { loadSchema, loadConfig, same, fieldOfMessage, searchBox } from '../core/configdata.js';
import { pendingBar, reviewChanges, savedBanner, watchConfig, noticeBanner } from '../core/changes.js';

// Unsaved edits survive a visit to another screen: "<scope>.<key>" -> value (null = reset).
const pending = new Map();

const SECTIONS = [
  ['timing', 'Timing', 'How long models are held, how long requests wait, and when a stuck load or stream is given up on.'],
  ['capacity', 'Capacity', 'How far a model may be spread across nodes and how much memory is set aside.'],
  ['runtime', 'Runtime', 'The llama.cpp binary and the local GPU.'],
  ['safety', 'Safety', 'Checks that keep the scheduler from starting a model that will not fit. Loosening them trades safety for room.'],
  ['workers', 'Workers', 'How quickly a silent or hung worker is noticed.'],
  ['retention', 'Retention', 'How much history and how many log files are kept.'],
  ['api', 'API', 'Limits of the web socket and the API.'],
  ['bench', 'Benchmarks', 'The benchmark feature and when it flags a regression.'],
  ['recovery', 'Recovery', 'What the scheduler asks the fleet manager to do for a worker that stopped answering.'],
  ['fleet', 'Fleet', 'The connection to the fleet console that lists the nodes.'],
];

// What loosening a safety setting risks, one plain line each (the schema text says what it does).
const RISK = {
  forced_fit_check: 'Set to off or warn, a profile with forced nodes can start even though its model does not fit, and the node can run out of memory.',
  split_skew_margin: 'A larger margin lets one node of a split model take more than its share, which can push that node over its memory.',
  estimate_margin: 'A smaller margin trusts the memory estimate more. If the estimate is low, a load can run a node out of memory.',
  load_memory_floor_mib: 'Set lower, or to 0, a load that is eating a shared-memory node (a PS5) is no longer stopped in time, and the console can crash.',
};

const SCOPE_PATH = { settings: '/api/config/settings', recovery: '/api/config/recovery', fleet: '/api/config/fleet' };
const ACTION_ORDER = ['restart_rpc_server', 'restart_docker', 'reboot'];

const pkey = (scope, key) => scope + '.' + key;
// Two values are the same setting when they parse to the same thing ("5m" and "5m0s").
const norm = (f, v) => (f.type === 'duration' ? (parseGoDuration(v) ?? v) : v);
const sameValue = (f, a, b) => same(norm(f, a), norm(f, b));

// A checkbox list for recovery.actions: the enum values in escalation order.
function actionsControl(f, value, onChange) {
  const chosen = new Set(Array.isArray(value) ? value : []);
  const order = f.enum && f.enum.length ? f.enum : ACTION_ORDER;
  const el = h('div.col', { style: 'gap:8px', 'data-keep': '' }, order.map(a => h('label.check', toggle(chosen.has(a), on => {
    if (on) chosen.add(a); else chosen.delete(a);
    onChange(order.filter(x => chosen.has(x)));
  }, a), h('span.mono', a))));
  return { el, values: () => ({ [f.key]: order.filter(x => chosen.has(x)) }), validate: () => true, setErrors() {}, changed: () => [] };
}

registerView({
  id: 'settings', title: 'Settings',
  mount(el) {
    let cfg = null, fields = null, error = '', search = '', saved = null, alive = true;
    const rows = new Map(); // "<scope>.<key>" -> {f, el, host, meta, form}
    const sectionEls = new Map(); // section -> {card, count, listHost}
    let container = null;

    const rev = () => (cfg ? cfg.revision : 0);
    const scopeData = scope => (scope === 'settings' ? cfg.settings : cfg[scope]) || {};
    const savedValue = f => scopeData(f.scope)[f.key];
    const isSet = f => (f.scope === 'settings' ? (cfg.settings_set || []).includes(f.key) : scopeData(f.scope)[f.key] !== undefined && !same(scopeData(f.scope)[f.key], f.default));
    // value shown now: the pending edit, else the saved value
    function shown(f) {
      const id = pkey(f.scope, f.key);
      if (pending.has(id)) { const v = pending.get(id); return v === null ? f.default : v; }
      const v = savedValue(f);
      return v === undefined ? f.default : v;
    }

    async function load() {
      try {
        const [c, f] = await Promise.all([loadConfig(), loadSchema()]);
        cfg = c; fields = f.filter(x => SCOPE_PATH[x.scope]);
        // drop pending edits that the new state already holds
        for (const [id, v] of [...pending]) {
          const fd = fields.find(x => pkey(x.scope, x.key) === id);
          if (!fd) { pending.delete(id); continue; }
          if (v !== null && sameValue(fd, v, savedValue(fd))) pending.delete(id);
        }
        error = '';
        buildAll();
      } catch (e) { error = e.message; }
      if (alive) view.refresh();
    }

    const watch = watchConfig({ revision: rev, pending: () => pending.size > 0, refresh: load, redraw: () => view.refresh() });

    // ---- rows: built once per load, updated by hand (the inputs must keep focus) ----
    function stage(f, v) {
      const id = pkey(f.scope, f.key);
      if (v === undefined && f.type !== 'string') return; // an empty number is not an edit; the field shows what is wrong
      if (v === undefined) v = '';
      if (sameValue(f, v, savedFor(f))) pending.delete(id);
      else pending.set(id, v);
      saved = null;
      meta(rows.get(id));
      view.refresh();
    }
    const savedFor = f => { const v = savedValue(f); return v === undefined ? f.default : v; };

    function meta(r) {
      const f = r.f, id = pkey(f.scope, f.key);
      const edited = pending.has(id);
      const setNow = edited ? pending.get(id) !== null && !sameValue(f, pending.get(id), f.default) : isSet(f);
      const def0 = f.default === null || f.default === undefined || f.default === '' ? 'none' : String(f.default) + (f.type !== 'duration' && f.unit ? ' ' + f.unit : '');
      const def = def0.length > 36 ? def0.slice(0, 34) + '…' : def0;
      r.meta.replaceChildren(frag(
        h('span.mono.mut', { style: 'font-size:var(--fs-xs)' }, f.key),
        h('span.mut', { style: 'font-size:var(--fs-sm);font-weight:600' }, 'Default ', h('span.mono', def)),
        setNow ? badge('Changed from default', 'load', 'This value was set on purpose. It is not the built-in default.') : null,
        setNow || edited ? button('Reset to default', () => reset(f), 'sm ghost', { 'aria-label': 'Reset ' + f.key + ' to its default' }) : null));
      const risk = f.section === 'safety' && RISK[f.key];
      if (risk) r.risk.textContent = 'If you loosen it: ' + risk; else r.risk.textContent = '';
    }

    function buildRow(f) {
      const id = pkey(f.scope, f.key);
      const r = rows.get(id) || { f, host: h('div'), meta: h('div.row', { style: 'gap:4px 12px' }), risk: h('span', { class: 'warn-t', style: 'font-size:var(--fs-sm);font-weight:600' }) };
      r.f = f;
      const lab = f.key === 'actions' && f.scope === 'recovery' ? 'Actions' : undefined;
      const def = Object.assign({}, f, { label: lab, required: f.type !== 'string' && f.type !== 'list' && f.type !== 'bool' ? true : f.required });
      const values = { [f.key]: shown(f) };
      r.form = f.type === 'list' && f.enum ? actionsControl(f, values[f.key], v => stage(f, v))
        : schemaForm({ fields: [def], values, showDefaults: false, onChange: (k, v) => stage(f, v) });
      if (f.type === 'list' && f.enum) {
        // the list control has no label of its own
        r.host.replaceChildren(h('div.field', h('div.lab', 'Actions'), r.form.el, h('span.help', f.description)));
      } else r.host.replaceChildren(r.form.el);
      if (!r.el) r.el = h('div', { class: 'setting', style: 'display:flex;flex-direction:column;gap:6px;min-width:0' }, r.host, r.risk, r.meta);
      meta(r);
      rows.set(id, r);
      return r;
    }

    function reset(f) {
      const id = pkey(f.scope, f.key);
      const savedIsDefault = !isSet(f);
      if (savedIsDefault) pending.delete(id); else pending.set(id, null);
      buildRow(f);
      applySearch();
      view.refresh();
    }

    function buildAll() {
      if (!fields) return;
      rows.clear(); sectionEls.clear();
      const cards = [];
      for (const [sec, title, blurb] of SECTIONS) {
        const fs = fields.filter(f => f.section === sec);
        if (!fs.length) continue;
        const list = h('div.form-grid');
        const count = h('span.note');
        for (const f of fs) list.appendChild(buildRow(f).el);
        const card = h('section.card', { 'aria-label': title, 'data-sec': sec },
          h('div.ch', h('h2', title), count), h('div.cb', { style: 'padding-top:0;display:flex;flex-direction:column;gap:14px' }, h('p.mut', blurb), list));
        sectionEls.set(sec, { card, count, fs });
        cards.push(card);
      }
      const none = h('div', { key: 'none', hidden: true }, empty('No setting matches', 'Try another word. The search looks at the key, the name and the description.'));
      container = h('div.col', { 'data-keep': '', key: 'settings-sections', id: 'settings-sections' }, cards, none);
      container.__none = none;
      applySearch();
    }

    function applySearch() {
      if (!container) return;
      const q = search.trim().toLowerCase();
      let shownAny = false;
      for (const [sec, s] of sectionEls) {
        let n = 0;
        for (const f of s.fs) {
          const r = rows.get(pkey(f.scope, f.key));
          const hit = !q || f.key.toLowerCase().includes(q) || f.description.toLowerCase().includes(q) || f.key.replace(/_/g, ' ').includes(q);
          r.el.hidden = !hit;
          if (hit) n++;
        }
        s.card.hidden = n === 0;
        const changed = s.fs.filter(f => pending.has(pkey(f.scope, f.key))).length;
        s.count.textContent = (q ? `${n} of ${s.fs.length} shown` : plural(s.fs.length, 'setting')) + (changed ? ` · ${changed} unsaved` : '');
        if (n) shownAny = true;
      }
      container.__none.hidden = shownAny;
    }

    // ---- review and save ----
    const buildOps = () => {
      const ops = [];
      for (const scope of Object.keys(SCOPE_PATH)) {
        const body = {};
        const keys = [];
        for (const [id, v] of pending) {
          if (!id.startsWith(scope + '.')) continue;
          const key = id.slice(scope.length + 1);
          body[key] = v;
          keys.push(key);
        }
        if (keys.length) ops.push({ label: `${scope === 'settings' ? 'Settings' : scope === 'recovery' ? 'Recovery' : 'Fleet'}: ${keys.join(', ')}`, method: 'PATCH', path: SCOPE_PATH[scope], body, count: keys.length });
      }
      return ops;
    };
    async function review() {
      if (!pending.size) return;
      const r = await reviewChanges({
        title: 'Review setting changes', getRevision: rev, buildOps,
        reload: async () => { await load(); },
        onInvalid: messages => {
          for (const [id] of pending) {
            const row = rows.get(id);
            if (!row) continue;
            const mine = messages.filter(m => fieldOfMessage(m, [row.f.key]) === row.f.key);
            if (mine.length) row.form.setErrors({ [row.f.key]: mine.join(' ').replace(/^[a-z]+: /, '') });
          }
        },
      });
      if (r && r.saved) { pending.clear(); saved = r; watch.clear(); }
      await load();
    }
    const discard = async () => {
      if (await confirm({ title: 'Discard unsaved changes?', intro: `${plural(pending.size, 'setting')} will go back to what is saved. Nothing on the server changes.`, confirmLabel: 'Discard', danger: true })) {
        pending.clear(); buildAll(); view.refresh();
      }
    };

    const render = () => {
      const head = pageHead('Settings', cfg && fields ? [h('b', num(fields.length)), ' settings · ', h('b', num(fields.filter(isSet).length)), ' changed from default',
        pending.size ? [' · ', h('b', num(pending.size)), ' unsaved'] : null, ' · revision ', h('span.mono', String(cfg.revision))] : null);
      if (error && !cfg) return [head, banner('err', ['The configuration could not be read: ', error])];
      if (!cfg || !fields || !container) return [head, h('section.card', skeleton(6))];
      applySearch();
      return [
        head,
        noticeBanner(watch.notice, { onReload: () => { watch.clear(); load(); }, onDismiss: () => { watch.clear(); view.refresh(); } }),
        savedBanner(saved, () => { saved = null; view.refresh(); }),
        cfg.configured === false ? banner('info', [h('b', 'No configuration has been imported yet. '),
          'Every setting shows its built-in default. Changing one here saves it as the first configuration revision.']) : null,
        h('div.row', { key: 'search', style: 'gap:10px 16px' }, searchBox(search, v => { search = v; applySearch(); }, 'Search by key or description', 'Search settings'),
          h('span.mut', { style: 'font-weight:600' }, 'A setting marked "Needs restart" is saved now and takes effect the next time the scheduler starts.')),
        container,
        pendingBar({ count: pending.size, what: 'settings', onReview: review, onDiscard: discard }),
      ];
    };
    const view = liveView(el, render, ['conn']);
    load();
    return { unmount() { alive = false; watch.off(); view.unmount(); } };
  },
});
