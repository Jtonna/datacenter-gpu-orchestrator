// Settings, rebuilt for the new shell (the owner, 24 Nov: Settings, Placement
// and Profiles are rebuilt; there is no board for it, so it follows the
// lead's brief): a menu of the groups at the left, one group in view at a
// time, and one row per setting: its name and a one-line help (the whole
// description on hover), its value control, its default and unit, and a
// Reset when it is not the default. A search looks through every group and
// lists the hits under their group's name.
//
// The groups are the schema's sections (GET /api/config/schema); with
// membership agents Recovery and Fleet are marked "Not used in this mode".
// Edits collect in the pending bar and are saved as before (core/changes.js):
// a dry run, the diff, a comment, then PATCH with If-Match. Nothing is
// written by changing a control.
//
// Reads: GET /api/config, GET /api/config/schema; WebSocket "config".
// Writes: PATCH /api/config/settings, /api/config/recovery, /api/config/fleet
// (only the keys that changed; null resets a key to its default).
// Route: #/settings, #/settings?group=<section>, #/settings?q=<search>.

import { h } from '../core/dom.js';
import { registerView, navigate } from '../core/router.js';
import { liveView, pageHead, badge, banner, skeleton, button, toggle, tagInput, confirm, empty, fieldLabel } from '../core/ui.js';
import { num, plural, parseGoDuration } from '../core/format.js';
import { loadSchema, loadConfig, same, fieldOfMessage, searchBox } from '../core/configdata.js';
import { pendingBar, reviewChanges, savedBanner, watchConfig, noticeBanner } from '../core/changes.js';

// Unsaved edits survive a visit to another screen: "<scope>.<key>" -> value (null = reset).
const pending = new Map();

export const SECTIONS = [
  ['timing', 'Timing', 'How long models are held, how long requests wait, and when a stuck load or stream is given up on.'],
  ['capacity', 'Capacity', 'How far a model may be spread across zones and how much memory is set aside.'],
  ['runtime', 'Runtime', 'The llama.cpp binary, the local GPU and the name of the machine the scheduler runs on.'],
  ['safety', 'Safety', 'Checks that keep the scheduler from starting a model that will not fit. Loosening them trades safety for room.'],
  ['workers', 'Workers', 'How quickly a silent or hung worker is noticed.'],
  ['retention', 'Retention', 'How much history and how many log files are kept.'],
  ['api', 'API', 'Limits of the web socket and the API.'],
  ['health', 'Health', 'When the scheduler counts its own core loop as slow, and the disk budget of the models folder.'],
  ['bench', 'Benchmarks', 'The benchmark feature and when it flags a regression.'],
  ['agents', 'Agents', 'Where the zones come from, and how the machines that run potluck-agent are used.'],
  ['logs', 'Central logging', 'Whether log lines are collected, how long the log store keeps them and who may send them.'],
  ['recovery', 'Recovery', 'What the scheduler asks the fleet manager to do for a worker that stopped answering.'],
  ['fleet', 'Fleet', 'The connection to the fleet console that lists the zones.'],
];

const UNUSED_WITH_AGENTS = {
  recovery: 'Not used in this mode: with agents, the scheduler never sends recovery requests.',
  fleet: 'Not used in this mode: the zones come from agent registrations, not from a fleet console. Only local_node is still read here, while the local_node setting under Runtime is empty.',
};

// What loosening a safety setting risks, one plain line each.
const RISK = {
  forced_fit_check: 'Set to off or warn, a profile with forced zones can start even though its model does not fit, and the machine can run out of memory. Machines that join through agents are always checked.',
  split_skew_margin: 'A larger margin lets one zone of a split model take more than its share, which can push that zone over its memory.',
  estimate_margin: 'A smaller margin trusts the memory estimate more. If the estimate is low, a load can run a zone out of memory.',
  load_memory_floor_mib: 'Set lower, or to 0, a load that is eating a shared-memory zone (a PS5) is no longer stopped in time, and the console can crash.',
};

const SCOPE_PATH = { settings: '/api/config/settings', recovery: '/api/config/recovery', fleet: '/api/config/fleet' };
const ACTION_ORDER = ['restart_rpc_server', 'restart_docker', 'reboot'];

const pkey = (scope, key) => scope + '.' + key;
const norm = (f, v) => (f.type === 'duration' ? (parseGoDuration(v) ?? v) : v);
const sameValue = (f, a, b) => same(norm(f, a), norm(f, b));
// The server writes "15m0s"; a person writes "15m".
const tidyDur = v => (typeof v === 'string' && /\d[hms]/.test(v) ? v.replace(/(?<=[hm])0s$/, '').replace(/(?<=h)0m$/, '') : v);

// oneLine(text) -> its first sentence (the help line under a setting's name).
export function oneLine(text) {
  const s = String(text || '').trim();
  const m = s.match(/^.*?[.!?](?=\s|$)/);
  return m ? m[0] : s;
}

// defaultText(f) -> the default as the row shows it ("none" when there is none).
export function defaultText(f) {
  const d = f.default;
  if (d === null || d === undefined || d === '' || (Array.isArray(d) && !d.length)) return 'none';
  if (f.type === 'duration') return String(tidyDur(d));
  if (f.type === 'bool') return d ? 'on' : 'off';
  if (Array.isArray(d)) return d.join(', ');
  return typeof d === 'number' ? num(d, d % 1 ? 2 : 0) : String(d);
}

// check(f, v) -> what is wrong with a typed value, or ''.
function check(f, v) {
  if (v === undefined || v === '') return f.type === 'string' || f.type === 'list' ? '' : 'Enter a value, or reset it to the default.';
  if (f.type === 'int' || f.type === 'float') {
    if (typeof v !== 'number') return 'Enter a number.';
    if (f.type === 'int' && !Number.isInteger(v)) return 'Enter a whole number.';
    if (f.min != null && v < f.min) return `At least ${f.min}.`;
    if (f.max != null && v > f.max) return `At most ${f.max}.`;
  }
  if (f.type === 'duration' && parseGoDuration(v) == null) return 'Enter a duration such as 90s, 15m or 1h30m.';
  return '';
}

// control(f, value, onChange, label) -> {el, err}: the value control of one row.
function control(f, value, onChange, label) {
  const err = h('span.err', { role: 'alert' });
  let el;
  if (f.type === 'bool') {
    el = h('span.row', { style: 'gap:8px' }, toggle(!!value, v => onChange(v), label), h('span.mut', value ? 'On' : 'Off'));
    const sw = el.firstChild, word = el.lastChild;
    sw.addEventListener('click', () => { word.textContent = sw.getAttribute('aria-checked') === 'true' ? 'On' : 'Off'; });
  } else if (f.type === 'enum') {
    el = h('select.select', { 'aria-label': label }, (f.enum || []).map(e => h('option', { value: e, selected: String(value) === String(e) }, String(e))));
    el.addEventListener('change', () => onChange(el.value));
  } else if (f.type === 'list' && f.enum && f.enum.length) {
    const chosen = new Set(Array.isArray(value) ? value : []);
    const order = f.enum.length ? f.enum : ACTION_ORDER;
    el = h('div.col', { style: 'gap:6px' }, order.map(a => h('label.check', toggle(chosen.has(a), on => {
      if (on) chosen.add(a); else chosen.delete(a);
      onChange(order.filter(x => chosen.has(x)));
    }, a), h('span.mono', a))));
  } else if (f.type === 'list') {
    el = tagInput(Array.isArray(value) ? value : [], v => onChange(v), 'Type and press Enter');
  } else if (['int', 'float', 'duration', 'string'].includes(f.type)) {
    const numeric = f.type === 'int' || f.type === 'float';
    const inp = h('input', { class: 'input' + (f.type !== 'string' ? ' mono' : ''), type: 'text', inputmode: numeric ? 'decimal' : null, 'aria-label': label,
      value: value == null ? '' : String(f.type === 'duration' ? tidyDur(value) : value), placeholder: f.type === 'duration' ? 'for example 90s or 15m' : null, autocomplete: 'off', spellcheck: 'false' });
    inp.addEventListener('input', () => {
      const raw = inp.value.trim();
      let v = raw === '' ? undefined : raw;
      if (numeric && raw !== '') { const n = Number(raw); v = Number.isNaN(n) ? raw : n; }
      const msg = check(f, v);
      err.textContent = msg;
      inp.setAttribute('aria-invalid', msg ? 'true' : 'false');
      onChange(v === undefined && f.type === 'string' ? '' : v);
    });
    el = inp;
  } else {
    el = h('textarea.textarea.mono', { rows: 3, 'aria-label': label }, value === undefined ? '' : JSON.stringify(value, null, 2));
    el.addEventListener('input', () => {
      try { onChange(el.value.trim() ? JSON.parse(el.value) : undefined); err.textContent = ''; } catch { err.textContent = 'This is not valid JSON.'; }
    });
  }
  return { el, err };
}

registerView({
  id: 'settings', title: 'Settings',
  mount(el, route) {
    let cfg = null, fields = null, error = '', saved = null, alive = true;
    let group = (route && route.query && route.query.group) || '';
    let search = (route && route.query && route.query.q) || '';
    let pane = null, paneKey = '', builds = 0;
    const rows = new Map(); // "<scope>.<key>" -> {f, el, meta, err}
    const sbox = searchBox(search, v => { search = v; view.refresh(); }, 'Search every setting', 'Search settings');
    sbox.setAttribute('data-search', ''); sbox.setAttribute('data-keep', ''); sbox.__key = 'search';
    // live(n) -> n the first time; while n is on the page, a keyed placeholder
    // that morph() matches to it (moving a node out would lose the focus of
    // the control being typed in).
    const onPage = n => { for (let p = n; p; p = p.parentNode) if (p === el) return true; return false; };
    const live = n => {
      if (!onPage(n)) return n;
      const ph = document.createElement(n.nodeName.toLowerCase());
      ph.setAttribute('data-keep', ''); ph.__key = n.__key;
      return ph;
    };

    const rev = () => (cfg ? cfg.revision : 0);
    const scopeData = scope => (scope === 'settings' ? cfg.settings : cfg[scope]) || {};
    const savedValue = f => scopeData(f.scope)[f.key];
    const isSet = f => (f.scope === 'settings' ? (cfg.settings_set || []).includes(f.key) : scopeData(f.scope)[f.key] !== undefined && !same(scopeData(f.scope)[f.key], f.default));
    const savedFor = f => { const v = savedValue(f); return v === undefined ? f.default : v; };
    function shown(f) {
      const id = pkey(f.scope, f.key);
      if (pending.has(id)) { const v = pending.get(id); return v === null ? f.default : v; }
      return savedFor(f);
    }
    const agents = () => !!cfg && !!cfg.settings && cfg.settings.membership === 'agents';
    const sections = () => SECTIONS.filter(([sec]) => (fields || []).some(f => f.section === sec));
    const current = () => { const s = sections(); return s.find(x => x[0] === group) || s[0] || null; };

    async function load() {
      try {
        const [c, f] = await Promise.all([loadConfig(), loadSchema()]);
        cfg = c; fields = f.filter(x => SCOPE_PATH[x.scope]);
        for (const [id, v] of [...pending]) {
          const fd = fields.find(x => pkey(x.scope, x.key) === id);
          if (!fd) { pending.delete(id); continue; }
          if (v !== null && sameValue(fd, v, savedValue(fd))) pending.delete(id);
        }
        error = '';
        rows.clear(); pane = null;
      } catch (e) { error = e.message; }
      if (alive) view.refresh();
    }
    const watch = watchConfig({ revision: rev, pending: () => pending.size > 0, refresh: load, redraw: () => view.refresh() });

    function stage(f, v) {
      const id = pkey(f.scope, f.key);
      if (v === undefined && f.type !== 'string') return; // an empty number is not an edit; the row says what is wrong
      if (v === undefined) v = '';
      if (sameValue(f, v, savedFor(f))) pending.delete(id); else pending.set(id, v);
      saved = null;
      const r = rows.get(id);
      if (r) meta(r);
      view.refresh();
    }
    function reset(f) {
      const id = pkey(f.scope, f.key);
      if (!isSet(f)) pending.delete(id); else pending.set(id, null);
      rows.delete(id); pane = null; // rebuild the row with the default in its control
      view.refresh();
    }

    // meta(r): the default column of a row: the default and unit, the marks, Reset.
    function meta(r) {
      const f = r.f, id = pkey(f.scope, f.key);
      const edited = pending.has(id);
      const setNow = edited ? pending.get(id) !== null && !sameValue(f, pending.get(id), f.default) : isSet(f);
      const d = defaultText(f);
      r.meta.replaceChildren(
        h('span.sdef-v', { title: 'The built-in default' + (f.unit ? ', in ' + f.unit : '') }, 'Default ', h('span.mono', d.length > 28 ? d.slice(0, 26) + '…' : d),
          f.unit && f.type !== 'duration' && d !== 'none' ? ' ' + f.unit : ''),
        h('span.row', { style: 'gap:6px' },
          edited ? badge('Unsaved', 'q', 'Not saved yet: Review changes saves it') : setNow ? badge('Changed', 'load', 'Set on purpose: not the built-in default') : null,
          f.restart_required ? badge('Needs restart', 'settle', 'Saved now, in force the next time the scheduler starts') : null,
          setNow || edited ? button('Reset', () => reset(f), 'sm ghost', { 'aria-label': 'Reset ' + f.key + ' to its default', 'data-reset': f.key }) : null));
    }

    function buildRow(f) {
      const id = pkey(f.scope, f.key);
      if (rows.has(id)) return rows.get(id);
      const label = f.key === 'actions' && f.scope === 'recovery' ? 'Actions' : fieldLabel(f);
      const c = control(f, shown(f), v => stage(f, v), label);
      const range = f.min != null && f.max != null ? `Allowed: ${f.min} to ${f.max}.` : f.min != null ? `At least ${f.min}.` : f.max != null ? `At most ${f.max}.` : '';
      const risk = f.section === 'safety' && RISK[f.key];
      const r = { f, err: c.err, meta: h('div.sdef') };
      r.el = h('div.srow', { 'data-key': id, role: 'group', 'aria-label': label },
        h('div.sname',
          h('b', label), h('span.mono.skey', f.key),
          h('span.shelp', { title: [f.description, range].filter(Boolean).join(' ') }, oneLine(f.description) || ' '),
          risk ? h('span.srisk', { title: risk }, 'If you loosen it: ' + oneLine(risk)) : null),
        h('div.sctl', c.el, f.unit && f.type !== 'duration' && f.type !== 'bool' ? h('span.mut.sunit', f.unit) : null, c.err),
        r.meta);
      meta(r);
      rows.set(id, r);
      return r;
    }

    const hit = (f, q) => f.key.toLowerCase().includes(q) || (f.description || '').toLowerCase().includes(q) || f.key.replace(/_/g, ' ').includes(q);

    // buildPane() -> the right column: one group, or every hit of a search.
    function buildPane() {
      const q = search.trim().toLowerCase();
      const cur = current();
      if (!cur) return h('div', { 'data-keep': '', key: 'pane-none' }, empty('No settings', 'The server sent no settings schema.'));
      builds++;
      if (q) {
        const parts = sections().map(([sec, title]) => {
          const fs = fields.filter(f => f.section === sec && hit(f, q));
          return fs.length ? h('section.sgrp', { 'data-sec': sec }, h('h3.sgrp-t', title, h('span.note', plural(fs.length, 'setting'))), fs.map(f => buildRow(f).el)) : null;
        }).filter(Boolean);
        return h('div.spane', { 'data-keep': '', key: 'pane-q-' + builds, 'aria-label': 'Search results' },
          parts.length ? parts : empty('No setting matches', 'Try another word. The search looks at the key, the name and the description.'));
      }
      const [sec, title, blurb] = cur;
      const unused = agents() && UNUSED_WITH_AGENTS[sec];
      const fs = fields.filter(f => f.section === sec);
      return h('div.spane', { 'data-keep': '', key: 'pane-' + sec + '-' + builds, 'data-sec': sec },
        h('div.spane-h', h('h2', title), unused ? badge('Not used in this mode', 'dash', 'membership is agents') : null, h('span.note', plural(fs.length, 'setting'))),
        h('p.mut.spane-b', unused || blurb),
        h('div.srows', { class: unused ? 'srows quiet' : 'srows' }, fs.map(f => buildRow(f).el)));
    }

    const pick = sec => { search = ''; sbox.value = ''; navigate('settings', [], { group: sec }, true); };

    function menu() {
      const cur = current();
      const q = search.trim().toLowerCase();
      return h('nav.smenu', { 'aria-label': 'Setting groups' }, sections().map(([sec, title]) => {
        const fs = fields.filter(f => f.section === sec);
        const changed = fs.filter(isSet).length, unsaved = fs.filter(f => pending.has(pkey(f.scope, f.key))).length;
        const hits = q ? fs.filter(f => hit(f, q)).length : null;
        const on_ = !q && cur && cur[0] === sec;
        return h('button', { type: 'button', key: 'g-' + sec, class: 'smi' + (on_ ? ' on' : '') + (q && !hits ? ' dim' : ''), 'aria-current': on_ ? 'page' : null, 'data-group': sec,
          title: (agents() && UNUSED_WITH_AGENTS[sec]) || null, onclick: () => pick(sec) },
          h('span.grow', title),
          unsaved ? h('span.smi-n.warn-t', { title: plural(unsaved, 'unsaved change') }, '•') : null,
          h('span.smi-n', { title: q ? plural(hits, 'match', 'matches') : changed ? `${plural(fs.length, 'setting')}, ${num(changed)} changed from the default` : plural(fs.length, 'setting') },
            q ? num(hits) : changed ? `${num(changed)}/${num(fs.length)}` : num(fs.length)));
      }));
    }

    const buildOps = () => {
      const ops = [];
      for (const scope of Object.keys(SCOPE_PATH)) {
        const body = {}, keys = [];
        for (const [id, v] of pending) {
          if (!id.startsWith(scope + '.')) continue;
          const key = id.slice(scope.length + 1);
          body[key] = v; keys.push(key);
        }
        if (keys.length) ops.push({ label: `${scope === 'settings' ? 'Settings' : scope === 'recovery' ? 'Recovery' : 'Fleet'}: ${keys.join(', ')}`, method: 'PATCH', path: SCOPE_PATH[scope], body, count: keys.length });
      }
      return ops;
    };
    async function review() {
      if (!pending.size) return;
      const r = await reviewChanges({
        title: 'Review setting changes', getRevision: rev, buildOps, reload: async () => { await load(); },
        onInvalid: messages => {
          for (const [id] of pending) {
            const row = rows.get(id);
            if (!row) continue;
            const mine = messages.filter(m => fieldOfMessage(m, [row.f.key]) === row.f.key);
            if (mine.length) row.err.textContent = mine.join(' ').replace(/^[a-z]+: /, '');
          }
        },
      });
      if (r && r.saved) { pending.clear(); saved = r; watch.clear(); }
      await load();
    }
    const discard = async () => {
      if (await confirm({ title: 'Discard unsaved changes?', intro: `${plural(pending.size, 'setting')} will go back to what is saved. Nothing on the server changes.`, confirmLabel: 'Discard', danger: true })) {
        pending.clear(); rows.clear(); pane = null; view.refresh();
      }
    };

    const render = () => {
      const head = pageHead('Settings', cfg && fields ? [h('b', num(fields.length)), ' settings · ', h('b', num(fields.filter(isSet).length)), ' changed from the default',
        pending.size ? [' · ', h('b', num(pending.size)), ' unsaved'] : null, ' · revision ', h('span.mono', String(cfg.revision))] : null);
      if (error && !cfg) return [head, banner('err', ['The configuration could not be read: ', error])];
      if (!cfg || !fields) return [head, h('section.card', skeleton(6))];
      const key = (search.trim() ? 'q:' + search.trim().toLowerCase() : 'g:' + (current() || [''])[0]);
      if (!pane || paneKey !== key) { pane = buildPane(); paneKey = key; }
      return [
        head,
        noticeBanner(watch.notice, { onReload: () => { watch.clear(); load(); }, onDismiss: () => { watch.clear(); view.refresh(); } }),
        savedBanner(saved, () => { saved = null; view.refresh(); }),
        cfg.configured === false ? banner('info', [h('b', 'No configuration has been imported yet. '),
          'Every setting shows its built-in default. Changing one here saves it as the first configuration revision.']) : null,
        h('section.card.swrap', { key: 'settings' },
          h('div.sside',
            live(sbox),
            menu()),
          live(pane)),
        pendingBar({ count: pending.size, what: 'settings', onReview: review, onDiscard: discard }),
      ];
    };
    const view = liveView(el, render, ['conn']);
    load();
    return {
      update(r) { group = (r.query && r.query.group) || ''; pane = null; view.refresh(); },
      unmount() { alive = false; watch.off(); view.unmount(); },
    };
  },
});
