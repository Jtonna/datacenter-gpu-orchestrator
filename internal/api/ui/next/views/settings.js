// Settings (the gear's Settings page; no mockup: inferred from the design
// language, phase 5). Replaces the classic page below /ui/ (the classic file
// stays at /ui/classic/). The owner's complaint was a page that scrolled far
// with jumbled text; this one shows ONE group of settings at a time:
//
//   - the groups (the schema's sections) in a menu at the left of the page,
//     the section-menu pattern; the address carries the group
//     (#/settings/<group>) and the search (?q=);
//   - each setting is ONE row: its name and key, a one-line help (the rest
//     behind "More"), the control that fits its type (switch, number with
//     its unit, select, short text, a set of checkboxes), the default, a
//     dot when the value differs from the default, "Reset to default", and
//     "Needs restart" when it only takes effect at the next start;
//   - the search filters across every group;
//   - changed values collect in a save bar ("3 changes · Save"); Save runs a
//     dry run first (a refusal is shown on the rows it names, in the
//     server's words), then asks for one comment and PATCHes each scope
//     with If-Match;
//   - fields of the stored configuration this version does not know
//     (GET /api/system stored_config.unknown_fields) are listed read-only in
//     a group of their own.
//
// Reads: REST GET /api/config/schema, /api/config/settings, /api/config/recovery,
// /api/config/fleet, /api/system; WebSocket "config" (refresh, or warn while
// there are unsaved edits).
// Writes: PATCH /api/config/settings, /api/config/recovery, /api/config/fleet
// (only the keys that changed; null resets a key to its default), each with
// a dry run first, If-Match and the comment. Signed by the default user.
// Route: #/settings/<group>?q=<search>.

import { h } from '../core/dom.js';
import { registerView, navigate } from '../core/router.js';
import * as api from '../core/api.js';
import { liveView, pageHead, badge, banner, skeleton, button, toggle, dialog, confirm, toast, searchField, tagInput, fieldLabel, humanize } from '../core/ui.js';
import { num, plural, parseGoDuration } from '../core/format.js';
import { loadSchema, same, fieldOfMessage } from '../core/configdata.js';
import { savedBanner, watchConfig, noticeBanner } from '../core/changes.js';

// Unsaved edits survive a visit to another screen: "<scope>.<key>" -> value (null = reset).
const pending = new Map();

// The groups, in this order, with one plain line each. A section the schema
// has and this list lacks is still shown (its name made from the key).
export const GROUPS = [
  ['timing', 'Timing', 'How long models are held, how long requests wait, and when a stuck load or stream is given up on.'],
  ['capacity', 'Capacity', 'How far a model may be spread across zones and how much memory is set aside.'],
  ['safety', 'Safety', 'Checks that keep the scheduler from starting a model that will not fit. Loosening them trades safety for room.'],
  ['agents', 'Agents', 'Where the zones come from, and how the machines that run potluck-agent are used.'],
  ['workers', 'Workers', 'How quickly a silent or hung worker is noticed.'],
  ['runtime', 'Runtime', 'The llama.cpp binary, the local GPU and the name of the machine the scheduler runs on.'],
  ['api', 'API', 'The web socket, the API and the page itself.'],
  ['health', 'Health', 'When the scheduler counts its own core loop as slow, and the disk budget of the models folder.'],
  ['retention', 'Retention', 'How much history and how many log files are kept.'],
  ['logs', 'Central logging', 'Whether log lines are collected, how long the log store keeps them and who may send them.'],
  ['bench', 'Benchmarks', 'The benchmark feature and when it flags a regression.'],
  ['recovery', 'Recovery', 'What the scheduler asks the fleet manager to do for a worker that stopped answering.'],
  ['fleet', 'Fleet', 'The connection to the fleet console that lists the zones.'],
];
const UNKNOWN = 'unknown';

// The groups that membership agents does not read: marked, with the reason.
const UNUSED_WITH_AGENTS = {
  recovery: 'Not used in this mode: with agents, the scheduler never sends recovery requests.',
  fleet: 'Not used in this mode: the zones come from agent registrations, not from a fleet console. Only local_node is still read here, while the local_node setting under Runtime is empty.',
};

// What loosening a safety setting risks, one plain line each (shown with the full help).
const RISK = {
  forced_fit_check: 'Set to off or warn, a profile with forced zones can start even though its model does not fit, and the machine can run out of memory. Machines that join through agents are always checked.',
  split_skew_margin: 'A larger margin lets one zone of a split model take more than its share, which can push that zone over its memory.',
  estimate_margin: 'A smaller margin trusts the memory estimate more. If the estimate is low, a load can run a zone out of memory.',
  load_memory_floor_mib: 'Set lower, or to 0, a load that is eating a shared-memory machine (a console) is no longer stopped in time, and the console can crash.',
};

const SCOPE_PATH = { settings: '/api/config/settings', recovery: '/api/config/recovery', fleet: '/api/config/fleet' };
const SCOPE_NAME = { settings: 'Settings', recovery: 'Recovery', fleet: 'Fleet' };
const ACTION_ORDER = ['restart_rpc_server', 'restart_docker', 'reboot'];
const UNIT_WORD = { percent: '%' };
const HELP_FOLD = 110; // a help longer than this is cut to one line, the rest behind "More"

// label(f) -> the setting's name: the schema's key in words, with the units spelled as units
const WORDS = { kib: 'KiB', gib: 'GiB', gb: 'GB', ws: 'WebSocket', ui: 'UI', api: 'API', url: 'URL', pct: 'percent', rpc: 'RPC' };
export const label = f => fieldLabel(f).replace(/\b(kib|gib|gb|ws|ui|api|url|pct|rpc)\b/gi, w => WORDS[w.toLowerCase()]);

const pkey = (scope, key) => scope + '.' + key;
const pid = f => pkey(f.scope, f.key);
// Two values are the same setting when they parse to the same thing ("5m" and "5m0s").
const norm = (f, v) => (f.type === 'duration' ? (parseGoDuration(v) ?? v) : v);
const sameValue = (f, a, b) => same(norm(f, a), norm(f, b));
// The server writes a duration as "15m0s"; a person writes "15m". Zero parts are dropped for display.
export const tidyDur = v => (typeof v === 'string' && /\d[hms]/.test(v) ? v.replace(/(?<=[hm])0s$/, '').replace(/(?<=h)0m$/, '') : v);

// valueText(f, v) -> a value in words: "5m", "On", "1,024 MiB", "not set".
export function valueText(f, v) {
  if (v === null || v === undefined || v === '') return 'not set';
  if (f.type === 'bool') return v ? 'On' : 'Off';
  if (f.type === 'duration') return String(tidyDur(v));
  if (Array.isArray(v)) return v.length ? v.join(', ') : 'none';
  if (typeof v === 'number') return num(v, v % 1 ? 2 : 0) + (f.unit ? ' ' + (UNIT_WORD[f.unit] || f.unit) : '');
  return String(v);
}

// groupsOf(fields, unknown) -> [{id, title, blurb, fields}] in menu order.
export function groupsOf(fields, unknown) {
  const out = [];
  const known = new Set(GROUPS.map(g => g[0]));
  for (const [id, title, blurb] of GROUPS) {
    const fs = fields.filter(f => f.section === id);
    if (fs.length) out.push({ id, title, blurb, fields: fs });
  }
  for (const sec of [...new Set(fields.map(f => f.section))]) {
    if (!known.has(sec)) out.push({ id: sec, title: humanize(sec), blurb: '', fields: fields.filter(f => f.section === sec) });
  }
  if (unknown && unknown.length) out.push({ id: UNKNOWN, title: 'Not known to this version', blurb: '', fields: [], unknown });
  return out;
}

// matches(f, q) -> does a setting match the search (key, name, help)?
export function matches(f, q) {
  if (!q) return true;
  const s = q.trim().toLowerCase();
  return f.key.toLowerCase().includes(s) || f.key.replace(/_/g, ' ').includes(s) || fieldLabel(f).toLowerCase().includes(s) || (f.description || '').toLowerCase().includes(s);
}

registerView({
  id: 'settings', title: 'Settings',
  mount(el, route) {
    let doc = null; // {revision, settings, settings_set, recovery, fleet}
    let fields = null, unknown = [], needForce = false, error = '', alive = true;
    let group = route.rest[0] || '', search = route.query.q || '';
    let saved = null, refusal = null, searchGen = 0; // refusal: {title, messages: [unplaced], text}
    const errors = new Map(); // id -> message (a typed value that does not parse, or the server's refusal)
    const drafts = new Map(); // id -> the text in a number or duration box while it does not parse
    const open = new Set(); // ids whose full help is unfolded

    const rev = () => (doc ? doc.revision : 0);
    const scopeData = scope => (doc && (scope === 'settings' ? doc.settings : doc[scope])) || {};
    const savedValue = f => { const v = scopeData(f.scope)[f.key]; return v === undefined ? f.default : v; };
    const isSet = f => (f.scope === 'settings' ? (doc.settings_set || []).includes(f.key) : !sameValue(f, savedValue(f), f.default));
    const shown = f => { const id = pid(f); if (pending.has(id)) { const v = pending.get(id); return v === null ? f.default : v; } return savedValue(f); };
    const offDefault = f => !sameValue(f, shown(f), f.default);
    const agents = () => scopeData('settings').membership === 'agents';

    async function load() {
      try {
        const [schema, s, r, fl, sys] = await Promise.all([
          loadSchema(),
          api.get('/api/config/settings'),
          api.get('/api/config/recovery').catch(() => ({})),
          api.get('/api/config/fleet').catch(() => ({})),
          api.get('/api/system').catch(() => ({})),
        ]);
        fields = schema.filter(x => SCOPE_PATH[x.scope]);
        doc = { revision: s.revision || 0, settings: s.settings || {}, settings_set: s.settings_set || [], recovery: r.recovery || {}, fleet: fl.fleet || {} };
        const sc = sys && sys.stored_config;
        unknown = (sc && sc.unknown_fields) || [];
        needForce = !!(sc && sc.writes_need_force);
        // drop pending edits that the new state already holds
        for (const [id, v] of [...pending]) {
          const f = fields.find(x => pid(x) === id);
          if (!f) { pending.delete(id); continue; }
          if (v === null ? !isSet(f) : sameValue(f, v, savedValue(f))) pending.delete(id);
        }
        error = '';
      } catch (e) { error = e.message; }
      if (alive) view.refresh();
    }

    const watch = watchConfig({ revision: rev, pending: () => pending.size > 0, refresh: load, redraw: () => view.refresh() });

    // ---- editing ----
    function stage(f, v) {
      const id = pid(f);
      errors.delete(id); drafts.delete(id);
      if (sameValue(f, v, savedValue(f))) pending.delete(id);
      else pending.set(id, v);
      saved = null;
      view.refresh();
    }
    function reset(f) {
      const id = pid(f);
      errors.delete(id); drafts.delete(id);
      if (isSet(f)) pending.set(id, null); else pending.delete(id);
      saved = null;
      view.refresh();
    }
    // a number or duration box: stage what parses, else keep the text and say what is wrong
    function typed(f, text) {
      const id = pid(f);
      const t = String(text).trim();
      let v, bad = '';
      if (f.type === 'int' || f.type === 'float') {
        v = t === '' ? NaN : Number(t);
        if (!Number.isFinite(v)) bad = 'Type a number.';
        else if (f.type === 'int' && !Number.isInteger(v)) bad = 'Type a whole number.';
        else if (f.min != null && v < f.min) bad = 'At least ' + num(f.min, f.min % 1 ? 2 : 0) + '.';
        else if (f.max != null && v > f.max) bad = 'At most ' + num(f.max, f.max % 1 ? 2 : 0) + '.';
      } else if (f.type === 'duration') {
        v = t;
        if (parseGoDuration(t) == null) bad = 'Type a duration such as 90s, 5m or 1h30m.';
      }
      if (bad) { drafts.set(id, text); errors.set(id, bad); pending.delete(id); saved = null; view.refresh(); return; }
      stage(f, v);
    }

    function control(f) {
      const id = pid(f), v = shown(f), name = label(f);
      const bad = errors.has(id);
      if (f.type === 'bool') {
        return h('div.setp-bool', { key: 'c-' + id },
          toggle(!!v, on => stage(f, on), name),
          h('span', { 'aria-hidden': 'true' }, v ? 'On' : 'Off'));
      }
      if (f.type === 'enum') {
        return h('select.select', { key: 'c-' + id, 'aria-label': name, onchange() { stage(f, this.value); } },
          (f.enum || []).map(o => h('option', { value: o, selected: o === v }, o)));
      }
      if (f.type === 'list' && f.enum) {
        const chosen = new Set(Array.isArray(v) ? v : []);
        const order = f.enum.length ? f.enum : ACTION_ORDER;
        return h('div.setp-checks', { key: 'c-' + id, role: 'group', 'aria-label': name }, order.map(a => h('label.check',
          h('input.cb', { type: 'checkbox', checked: chosen.has(a), 'aria-label': a, onchange() {
            if (this.checked) chosen.add(a); else chosen.delete(a);
            stage(f, order.filter(x => chosen.has(x)));
          } }),
          h('span.mono', a))));
      }
      if (f.type === 'list') {
        return h('div', { key: 'c-' + id + '-' + JSON.stringify(v), 'data-keep': '' }, tagInput(Array.isArray(v) ? v : [], vals => stage(f, vals), 'Type and press Enter'));
      }
      const text = drafts.has(id) ? drafts.get(id) : f.type === 'duration' ? String(tidyDur(v ?? '')) : v == null ? '' : String(v);
      if (f.type === 'int' || f.type === 'float') {
        const unit = f.unit ? (UNIT_WORD[f.unit] || f.unit) : '';
        const input = h('input', { class: 'input mono', type: 'text', inputmode: 'decimal', value: text, 'aria-label': name + (unit ? ' (' + unit + ')' : ''), 'aria-invalid': bad ? 'true' : null,
          oninput() { typed(f, this.value); } });
        return unit ? h('div.unit', { key: 'c-' + id }, input, h('span', unit)) : h('div', { key: 'c-' + id }, input);
      }
      if (f.type === 'duration') {
        return h('input', { key: 'c-' + id, class: 'input mono', type: 'text', value: text, placeholder: 'e.g. 90s, 5m', 'aria-label': name, 'aria-invalid': bad ? 'true' : null,
          oninput() { typed(f, this.value); } });
      }
      // string
      return h('input', { key: 'c-' + id, class: 'input mono', type: 'text', value: text, spellcheck: 'false', autocomplete: 'off', 'aria-label': name,
        placeholder: f.default ? String(f.default) : 'not set', oninput() { stage(f, this.value); } });
    }

    function row(f) {
      const id = pid(f);
      const help = f.description || '';
      const risk = RISK[f.key] && f.section === 'safety' ? 'If you loosen it: ' + RISK[f.key] : '';
      const long = help.length > HELP_FOLD || !!risk;
      const unfolded = open.has(id);
      const differs = offDefault(f);
      const edited = pending.has(id);
      return h('div', { class: 'setp-row' + (edited ? ' edited' : '') + (errors.has(id) ? ' bad' : ''), key: 'r-' + id, 'data-key': f.key, 'data-scope': f.scope },
        h('div.setp-name',
          h('div.setp-title',
            differs ? h('span.changed', { title: 'Differs from the default (' + valueText(f, f.default) + ')', 'aria-label': 'Differs from the default' }) : null,
            h('b', label(f)),
            edited ? badge('Unsaved', 'q') : null),
          h('span.mono.setp-key', f.scope === 'settings' ? f.key : f.scope + '.' + f.key)),
        h('div.setp-ctl', control(f)),
        h('div.setp-def',
          h('span.mut', 'Default ', h('span.mono', valueText(f, f.default))),
          f.restart_required ? badge('Needs restart', 'settle', 'Saved now; takes effect the next time the scheduler starts') : null,
          differs ? button('Reset to default', () => reset(f), 'sm ghost', { 'aria-label': 'Reset ' + f.key + ' to its default', 'data-reset': f.key })
            : edited ? button('Undo', () => { pending.delete(id); errors.delete(id); drafts.delete(id); view.refresh(); }, 'sm ghost', { 'aria-label': 'Undo the change of ' + f.key }) : null),
        help || errors.has(id) ? h('div', { class: 'setp-helpline' + (unfolded ? ' open' : '') },
          help ? h('div', { class: 'setp-help' + (unfolded ? ' open' : ''), title: unfolded ? null : help }, help, unfolded && risk ? h('span.warn-t', ' ' + risk) : null) : null,
          long ? h('button.setp-more', { type: 'button', 'aria-expanded': unfolded ? 'true' : 'false', onclick: () => { if (unfolded) open.delete(id); else open.add(id); view.refresh(); } }, unfolded ? 'Less' : 'More') : null,
          errors.has(id) ? h('div.setp-err', { role: 'alert' }, errors.get(id)) : null) : null);
    }

    function unknownRows(g) {
      return [
        h('div.setp-note', { key: 'unknown-note' },
          'The stored configuration has these fields, written by a newer version of Potluck. This version does not read them and cannot change them here.',
          needForce ? h('b', ' While they are stored, the server refuses every change on this page: saving would drop them.') : null),
        g.unknown.map(k => h('div.setp-row.ro', { key: 'u-' + k },
          h('div.setp-name', h('div.setp-title', h('b.mono', k)), h('div.setp-help', 'Kept as it is. Read-only.')),
          h('div.setp-ctl', h('span.mut', 'Not editable')),
          h('div.setp-def'))),
      ];
    }

    // ---- save ----
    const buildOps = () => {
      const ops = [];
      for (const scope of Object.keys(SCOPE_PATH)) {
        const body = {}, keys = [];
        for (const [id, v] of pending) {
          if (!id.startsWith(scope + '.')) continue;
          const key = id.slice(scope.length + 1);
          body[key] = v; keys.push(key);
        }
        if (keys.length) ops.push({ scope, path: SCOPE_PATH[scope], body, keys });
      }
      return ops;
    };

    // place the server's messages on the rows they name; the rest go to the banner
    function placeMessages(ops, messages) {
      const left = [];
      for (const m of messages) {
        let placed = false;
        for (const op of ops) {
          const k = fieldOfMessage(m, op.keys);
          if (k) { errors.set(pkey(op.scope, k), String(m).replace(/^[a-z]+: /, '')); placed = true; break; }
        }
        if (!placed) left.push(m);
      }
      return left;
    }

    function refused(e, ops) {
      const err = e && e.body && e.body.error;
      if (e.status === 400) {
        const messages = err && err.messages && err.messages.length ? err.messages : [e.message];
        const left = placeMessages(ops, messages);
        refusal = { title: 'The server did not accept ' + (messages.length === 1 ? 'this change' : 'these changes') + '. Nothing was saved.', messages: left };
        // show the first refused row
        const first = fields.find(f => errors.has(pid(f)) && pending.has(pid(f)));
        if (first && first.section !== group && !search) navigate('settings', [first.section], {}, true);
      } else if (e.status === 409 && err && err.type === 'stale_revision') {
        refusal = { title: 'Someone else changed the configuration first (it is now revision ' + err.current_revision + '). Nothing of yours was saved.', messages: [], reload: true };
      } else {
        refusal = { title: 'Nothing was saved.', messages: [e.message || 'The change failed.'] };
      }
      view.refresh();
    }

    async function save() {
      const ops = buildOps();
      if (!ops.length) return;
      refusal = null;
      for (const [id] of pending) errors.delete(id);
      // 1. dry run: the server checks every change before anything is written
      let res = [];
      try {
        for (const op of ops) res.push(await api.patch(op.path, op.body, { dryRun: true, ifMatch: rev() }));
      } catch (e) { refused(e, ops); return; }
      const count = ops.reduce((n, op) => n + op.keys.length, 0);
      const warnings = [...new Set(res.flatMap(r => (r && r.warnings) || []))];
      const restart = [...new Set(res.flatMap(r => (r && r.restart_required) || []))].map(k => String(k).replace(/^settings\./, ''));
      const lines = ops.flatMap(op => op.keys.map(k => {
        const f = fields.find(x => x.scope === op.scope && x.key === k);
        const nv = op.body[k] === null ? f.default : op.body[k];
        return h('li.setp-chg', h('b', label(f)), ' ', h('span.mono.mut', op.scope === 'settings' ? k : op.scope + '.' + k), h('br'),
          h('span.mono.err-t', valueText(f, savedValue(f))), ' → ', h('span.mono.ok-t', valueText(f, nv)), op.body[k] === null ? h('span.mut', ' (the default)') : null);
      }));
      // 2. one comment, then the writes
      const ans = await dialog({
        title: 'Save ' + plural(count, 'change') + '?', wide: true,
        body: h('div.col', { style: 'gap:12px' },
          h('p', 'This writes a new configuration revision on top of revision ' + rev() + '. The scheduler uses it at once.'),
          h('ul.setp-chgs', lines),
          warnings.length ? banner('warn', h('span', h('b', 'Check this first. '), warnings.join(' '))) : null,
          restart.length ? banner('info', h('span', h('b', 'Needs a restart. '), 'These take effect the next time the scheduler starts: ', restart.map((k, i) => [i ? ', ' : '', h('span.mono', k)]), '.')) : null),
        comment: { label: 'Comment', required: true, placeholder: 'Why are you changing this? It is kept in the audit log.' },
        actions: [{ label: 'Cancel', value: false }, { label: 'Save ' + plural(count, 'change'), kind: 'pri', value: true }],
      });
      if (!ans || ans.value !== true) return;
      let revision = rev(), done = 0;
      try {
        for (const op of ops) {
          const r = await api.patch(op.path, Object.assign({}, op.body, { comment: ans.comment }), { ifMatch: revision, comment: ans.comment });
          if (r && r.revision) revision = r.revision;
          for (const k of op.keys) pending.delete(pkey(op.scope, k));
          done++;
        }
      } catch (e) {
        if (done) toast(done + ' of ' + ops.length + ' parts were saved before the server refused the rest.', 'err');
        refused(e, ops);
        await load();
        return;
      }
      saved = { revision, restart_required: restart, changed: true };
      toast('Saved as revision ' + revision, 'ok');
      watch.clear();
      await load();
    }

    async function discard() {
      if (await confirm({ title: 'Discard unsaved changes?', intro: plural(pending.size, 'setting') + ' will go back to what is saved. Nothing on the server changes.', confirmLabel: 'Discard', danger: true })) {
        pending.clear(); errors.clear(); drafts.clear(); refusal = null; view.refresh();
      }
    }

    // ---- drawing ----
    const setSearch = v => { search = v; navigate('settings', group ? [group] : [], { q: v || null }, true); };

    function menu(groups, hits) {
      return h('nav.setp-menu', { 'aria-label': 'Setting groups' }, groups.map(g => {
        const n = g.unknown ? g.unknown.length : g.fields.length;
        const p = g.fields.filter(f => pending.has(pid(f))).length;
        const changed = g.fields.filter(f => doc && !sameValue(f, savedValue(f), f.default)).length;
        const hitN = search ? (hits.get(g.id) || 0) : null;
        const on = !search && g.id === group;
        return h('a', { class: 'ni setp-gi' + (on ? ' on' : '') + (search && !hitN ? ' dim' : ''), key: 'g-' + g.id, href: '#/settings/' + encodeURIComponent(g.id), 'aria-current': on ? 'page' : null, 'data-group': g.id,
          onclick: e => { e.preventDefault(); if (search) searchGen++; search = ''; navigate('settings', [g.id], {}, true); } },
          h('span.grow', g.title),
          changed ? h('span.changed', { title: plural(changed, 'setting') + ' differ from the default' }) : null,
          p ? h('span.nb', { title: plural(p, 'unsaved change') }, num(p)) : null,
          h('span.setp-n', search ? num(hitN) : num(n)));
      }));
    }

    function pane(groups) {
      if (search) {
        const found = groups.filter(g => !g.unknown).map(g => ({ g, fs: g.fields.filter(f => matches(f, search)) })).filter(x => x.fs.length);
        const total = found.reduce((n, x) => n + x.fs.length, 0);
        return h('div.setp-pane', { key: 'pane-search' },
          h('div.setp-head', h('h2', total ? plural(total, 'setting') + ' match “' + search + '”' : 'No setting matches “' + search + '”'),
            h('p.mut', total ? 'Across ' + plural(found.length, 'group') + '. Clear the search to see one group at a time.' : 'The search looks at the name, the key and the help text.')),
          found.map(x => [h('h3.setp-sub', { key: 'sh-' + x.g.id }, x.g.title), x.fs.map(row)]));
      }
      const g = groups.find(x => x.id === group) || groups[0];
      const unused = agents() && UNUSED_WITH_AGENTS[g.id];
      return h('div.setp-pane', { key: 'pane-' + g.id },
        h('div.setp-head',
          h('div.row', { style: 'gap:8px 10px' }, h('h2', g.title), unused ? badge('Not used in this mode', 'dash', 'membership is agents') : null,
            g.unknown ? null : h('span.mut.setp-count', plural(g.fields.length, 'setting'))),
          unused ? h('p.mut', unused) : g.blurb ? h('p.mut', g.blurb) : null),
        g.unknown ? unknownRows(g) : g.fields.map(row));
    }

    function saveBar() {
      const n = pending.size, bad = [...errors.keys()].filter(id => drafts.has(id)).length;
      if (!n && !bad) return null;
      const restart = [...pending.keys()].filter(id => { const f = fields.find(x => pid(x) === id); return f && f.restart_required; }).length;
      return h('div.setp-save', { key: 'save', role: 'status' },
        h('span', h('b', plural(n, 'change')), restart ? ' · ' + restart + ' need' + (restart === 1 ? 's' : '') + ' a restart' : '',
          bad ? h('span.err-t', ' · ' + plural(bad, 'value') + ' to fix') : null),
        h('span.mut', 'Nothing is applied until you save.'),
        h('span.setp-save-btns', button('Discard', discard, 'sm'), button('Save', save, 'pri sm', { disabled: !n || bad > 0, 'data-save': '' })));
    }

    const render = () => {
      const head = pageHead('Settings', doc && fields ? [h('b', num(fields.length)), ' settings · ', h('b', num(fields.filter(isSet).length)), ' changed from default',
        ' · revision ', h('span.mono', String(rev()))] : null);
      if (error && !doc) return [head, banner('err', ['The settings could not be read: ', error])];
      if (!doc || !fields) return [head, h('section.panel', skeleton(6))];
      const groups = groupsOf(fields, unknown);
      if (!groups.some(g => g.id === group) && !search) group = groups.length ? groups[0].id : '';
      const hits = new Map(groups.map(g => [g.id, g.fields.filter(f => matches(f, search)).length]));
      return [
        head,
        noticeBanner(watch.notice, { onReload: () => { watch.clear(); load(); }, onDismiss: () => { watch.clear(); view.refresh(); } }),
        savedBanner(saved, () => { saved = null; view.refresh(); }),
        refusal ? h('div.banner.err', { key: 'refusal', role: 'alert' },
          h('span.grow', h('b', refusal.title), refusal.messages.length ? h('ul.setp-msgs', refusal.messages.map(m => h('li', m))) : null,
            errors.size && !refusal.reload ? h('span', ' The rows it named are marked.') : null),
          refusal.reload ? button('Load the new state', () => { refusal = null; load(); }, 'sm') : null,
          button('Dismiss', () => { refusal = null; view.refresh(); }, 'sm')) : null,
        rev() === 0 ? banner('info', [h('b', 'No configuration has been saved yet. '), 'Every setting shows its built-in default. Saving a change here makes the first revision.']) : null,
        h('section.panel.setp', { key: 'setp' },
          h('div.setp-bar',
            searchField(search, v => setSearch(v), 'Search every setting', 'settings-search-' + searchGen, { size: 'lg' }),
            h('span.mut.setp-legend', h('span.changed'), ' differs from the default')),
          h('div.setp-body', menu(groups, hits), pane(groups)),
          saveBar()),
      ];
    };

    const view = liveView(el, render, ['conn']);
    load();
    return {
      update(r) {
        group = r.rest[0] || group;
        const q = r.query.q || '';
        if (q !== search) search = q;
        view.refresh();
      },
      unmount() { alive = false; watch.off(); view.unmount(); },
    };
  },
});
