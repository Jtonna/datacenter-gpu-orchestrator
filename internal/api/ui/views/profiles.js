// Profiles (Configure): the model profiles the scheduler can load. List with
// search and filters; create, edit, duplicate and delete. Edits collect in a
// pending-changes bar; "Review changes" does a dry run, shows the diff, takes a
// comment and saves with If-Match (views/configure/changes.js).
//
// Reads: REST GET /api/config, GET /api/config/schema, GET /api/profiles (the
// plan of each saved profile); WebSocket "status" (loaded instances,
// stale_profile) and "config" (refresh, or warn when there are unsaved edits).
// Writes: POST /api/config/profiles, PUT and DELETE /api/config/profiles/{id}.
// Routes: #/profiles, #/profiles/new[?from=<id>], #/profiles/<id>.

import { h } from '../core/dom.js';
import { registerView, href, navigate } from '../core/router.js';
import { state } from '../core/store.js';
import * as api from '../core/api.js';
import { liveView, pageHead, badge, nodeChips, table, empty, banner, skeleton, chips, button, schemaForm, confirm } from '../core/ui.js';
import { num, mib, ctx, plural, DASH } from '../core/format.js';
import { plainReason } from '../core/status.js';
import { loadSchema, loadConfig, fieldsOf, clone, fieldOfMessage, searchBox } from '../core/configdata.js';
import { pendingBar, reviewChanges, savedBanner, watchConfig, noticeBanner } from '../core/changes.js';

// Unsaved profile changes live outside the view so they survive a visit to another screen.
// id -> {type: 'create' | 'update' | 'delete', profile, errors: [messages]}
const pending = new Map();

const LABELS = {
  kind: 'Kind', model: 'Model file', ctx: 'Context size', parallel: 'Parallel slots', kv_quant: 'KV cache precision', flash_attn: 'Flash attention',
  needs_mib: 'Memory need', allow_tiers: 'Allowed tiers', force_nodes: 'Forced nodes', tensor_split: 'Tensor split', extra_args: 'Extra arguments',
  cmd: 'Command', check_endpoint: 'Readiness path', aliases: 'Aliases', ttl: 'Idle time before unload', load_timeout: 'Load timeout',
  stall_timeout: 'Stall timeout', load_stall_timeout: 'Load stall timeout',
};
const SECTIONS = [['identity', 'Identity'], ['model', 'Model'], ['memory', 'Memory'], ['placement', 'Placement'], ['command', 'Command'], ['timing', 'Timing']];
const NUMERIC_LISTS = ['allow_tiers', 'tensor_split'];

// ---- server profile <-> form values ----

function toForm(p) {
  const v = clone(p || {});
  for (const k of NUMERIC_LISTS) if (Array.isArray(v[k])) v[k] = v[k].map(String);
  if (Array.isArray(v.extra_args)) v.extra_args = v.extra_args.join('\n');
  return v;
}

function fromForm(vals, fields, orig) {
  const kind = vals.kind || 'llama';
  const out = {};
  for (const f of fields) {
    if (f.applies_to && f.applies_to !== (kind === 'command' ? 'command' : 'llama')) continue;
    let v = vals[f.key];
    if (v === undefined || v === '' || v === null) continue;
    if (NUMERIC_LISTS.includes(f.key)) v = (v || []).map(Number).filter(n => !Number.isNaN(n));
    if (f.key === 'extra_args') v = String(v).split('\n').map(s => s.trim()).filter(Boolean);
    if (Array.isArray(v) && !v.length) continue;
    out[f.key] = v;
  }
  if (kind === 'command' || (orig && orig.kind)) out.kind = kind; else delete out.kind;
  return out;
}

// ---- the editor ----

// makeEditor builds the form once; it is returned as a stable node (data-keep).
function makeEditor(o) {
  const { fields, mode, id, onStage, onReview, onBack } = o;
  const orig = o.initial || {};
  let current = toForm(orig);
  let forms = [];
  const idInput = h('input', { class: 'input mono', type: 'text', value: o.newId || '', placeholder: 'for example gemma-4-12b:48k', autocomplete: 'off', spellcheck: 'false', 'aria-label': 'Profile name' });
  const idErr = h('span.err', { role: 'alert' });
  const serverBox = h('div');
  const host = h('div.col', { style: 'gap:22px' });

  const applicable = (f, kind) => !f.applies_to || f.applies_to === (kind === 'command' ? 'command' : 'llama');
  const PLACEHOLDER = { extra_args: '--rope-scaling yarn', force_nodes: 'Node name, then Enter', allow_tiers: 'Tier number, then Enter', tensor_split: 'One weight per forced node, then Enter',
    ttl: 'Same as the ttl setting', stall_timeout: 'Same as the stall_timeout setting', load_stall_timeout: 'Same as the load_stall_timeout setting', load_timeout: 'Worked out from the model size',
    needs_mib: '0 = estimate from the model file', model: 'for example gemma-4-12b-it-Q4_K_M.gguf' };
  const fieldDef = f => Object.assign({}, f, { label: LABELS[f.key], type: f.key === 'extra_args' ? 'text' : f.type,
    placeholder: PLACEHOLDER[f.key],
    description: f.key === 'extra_args' ? f.description + ' One argument per line.' : f.description });

  function collect() {
    for (const { form, fields: fs } of forms) {
      const v = form.values();
      for (const f of fs) { if (v[f.key] === undefined) delete current[f.key]; else current[f.key] = v[f.key]; }
    }
    return current;
  }

  function build() {
    const kind = current.kind || 'llama';
    forms = [];
    const blocks = [];
    for (const [sec, title] of SECTIONS) {
      const fs = fields.filter(f => f.section === sec && applicable(f, kind));
      if (!fs.length) continue;
      const form = schemaForm({ fields: fs.map(fieldDef), values: current, showDefaults: true, onChange: (k, v) => {
        current[k] = v;
        if (k === 'kind') { collect(); build(); const sel = host.querySelector('select'); if (sel) sel.focus(); }
      } });
      forms.push({ section: sec, form, fields: fs });
      blocks.push(h('section.col', { style: 'gap:10px' }, h('h3', { style: 'font-size:var(--fs);font-weight:800;color:var(--text-2)' }, title), form.el));
    }
    host.replaceChildren(...blocks);
  }
  build();

  const allKeys = () => fields.map(f => f.key);

  function setMessages(messages) {
    const left = [];
    const byKey = {};
    for (const m of messages || []) {
      const k = fieldOfMessage(m, allKeys());
      if (k) byKey[k] = (byKey[k] ? byKey[k] + ' ' : '') + m.replace(/^profile "[^"]*":\s*/, '').replace(new RegExp('^' + k + ':\\s*'), '');
      else left.push(m);
    }
    for (const { form } of forms) form.setErrors(byKey);
    serverBox.replaceChildren(...(left.length ? [banner('err', h('span', h('b', 'The server did not accept this profile. '), h('ul', { style: 'margin:6px 0 0;padding-left:18px' }, left.map(m => h('li', m)))))] : []));
  }

  function profile() { collect(); return fromForm(current, fields, orig); }

  function check() {
    let ok = true;
    for (const { form } of forms) if (!form.validate()) ok = false;
    if (mode !== 'edit') {
      const v = idInput.value.trim();
      const msg = !v ? 'A profile needs a name.' : /\s/.test(v) ? 'A name cannot contain spaces.' : o.exists(v) ? `A profile named ${v} already exists.` : '';
      idErr.textContent = msg;
      idInput.setAttribute('aria-invalid', msg ? 'true' : 'false');
      if (msg) ok = false;
    }
    return ok;
  }

  function stage(review) {
    if (!check()) { const bad = el.querySelector('[aria-invalid=true]'); if (bad) bad.focus(); return; }
    const pid = mode === 'edit' ? id : idInput.value.trim();
    onStage(pid, profile(), mode);
    if (review) onReview(); else onBack();
  }

  const el = h('section.card', { 'data-keep': '', key: 'editor-' + (mode === 'edit' ? id : mode) + '-' + Math.random().toString(36).slice(2, 7), 'aria-label': mode === 'edit' ? 'Edit ' + id : 'New profile' },
    h('div.ch', h('h2', mode === 'edit' ? [o.unsaved ? 'New profile ' : 'Edit profile ', h('span.mono', id)] : mode === 'dup' ? ['New profile, copied from ', h('span.mono', o.from)] : 'New profile'),
      h('span.right', h('a', { href: href('profiles') }, 'Back to the list'))),
    h('div.cb', { style: 'display:flex;flex-direction:column;gap:22px' },
      serverBox,
      mode === 'edit' ? null : h('div.field', { style: 'max-width:460px' }, h('label', 'Name', h('span', { class: 'b b-n' }, 'Required')), idInput,
        h('span.help', 'The model name clients send. It cannot be changed later; to rename, copy the profile and delete the old one.'), idErr),
      host,
      h('div.form-foot', button('Cancel', onBack, 'lg'), button('Add to unsaved changes', () => stage(false), 'lg'), button('Review changes', () => stage(true), 'lg pri'))));
  return { el, setMessages };
}

// ---- cells ----

function placementCell(p) {
  const bits = [];
  if ((p.force_nodes || []).length) {
    bits.push(nodeChips(p.force_nodes));
    if ((p.tensor_split || []).length) bits.push(h('span.sub', 'split ' + p.tensor_split.join(' / ')));
    else bits.push(h('span.sub', 'forced, in this order'));
  } else {
    bits.push(h('span.mut', 'Automatic'));
    if ((p.allow_tiers || []).length) bits.push(h('span.sub', 'tiers ' + p.allow_tiers.join(', ') + ' only'));
  }
  return h('span', bits);
}

function memoryCell(p, plan) {
  if (p.kind === 'command' || !plan) {
    return p.needs_mib ? h('span', mib(p.needs_mib), h('span.sub', 'configured')) : h('span.mut', DASH);
  }
  if (!plan.needs_mib && !p.needs_mib) return h('span.mut', DASH);
  const configured = plan.needs_mib_configured > 0;
  const need = plan.needs_mib || p.needs_mib;
  return h('span', { title: plan.needs_basis || '' }, mib(need), h('span.sub', configured ? 'configured' : plan.needs_measured ? 'learned' : 'estimated'));
}

function planLine(plan) {
  if (!plan) return h('span.mut', 'Not known yet');
  const nodes = plan.nodes && plan.nodes.length ? nodeChips(plan.nodes) : null;
  switch (plan.action) {
    case 'start': return h('span', nodes, ' ', h('span.mut', 'would start now'));
    case 'serve': case 'wait': return h('span.mut', 'Already loaded or loading', nodes ? [' on ', nodes] : null);
    case 'evict': return h('span', nodes, ' ', h('span.mut', 'after making room by unloading another model'));
    case 'queue': return h('span', { title: plan.reason ? 'The scheduler says: ' + plan.reason : null }, h('span.mut', 'Would wait in the queue'), plan.reason ? h('span.sub', plainReason(plan.reason)) : null);
    case 'backoff': return h('span.err-t', { title: plan.reason ? 'The scheduler says: ' + plan.reason : null }, 'Held back after a failed load');
    case 'reject': return h('span.err-t', { title: plan.reason ? 'The scheduler says: ' + plan.reason : null }, 'Cannot be placed', plan.reason ? h('span.sub', { style: 'color:inherit' }, plainReason(plan.reason)) : null);
  }
  return h('span.mut', { title: plan.reason || null }, plainReason(plan.reason) || 'Not known');
}

// ---- the view ----

registerView({
  id: 'profiles', title: 'Profiles',
  mount(el, route0) {
    let route = route0, cfg = null, fields = null, plans = {}, error = '', search = '', filter = 'all', saved = null, alive = true;
    let editor = null, editorKey = '';

    const rev = () => (cfg ? cfg.revision : 0);
    const profiles = () => (cfg && cfg.profiles) || {};

    async function load() {
      try {
        const [c, f, pl] = await Promise.all([loadConfig(), loadSchema(), api.get('/api/profiles').catch(() => null)]);
        cfg = c; fields = fieldsOf(f, 'profile');
        plans = {};
        for (const p of (pl && pl.profiles) || []) plans[p.id] = p;
        error = '';
      } catch (e) { error = e.message; }
      if (alive) { syncEditor(); view.refresh(); }
    }

    const watch = watchConfig({ revision: rev, pending: () => pending.size > 0, refresh: load, redraw: () => view.refresh() });

    // ---- editor lifecycle: one editor node per route ----
    function routeKey() { return route.rest.join('/') + '?' + (route.query.from || ''); }
    function syncEditor() {
      const key = routeKey();
      if (!route.rest.length) { editor = null; editorKey = ''; return; }
      if (!cfg || !fields) return;
      if (editor && editorKey === key) return;
      editorKey = key;
      const id = route.rest[0];
      let mode = 'edit', initial = null, from = null, newId = '';
      if (id === 'new') {
        mode = route.query.from ? 'dup' : 'new'; from = route.query.from || null;
        const src = from && (pending.get(from) && pending.get(from).profile || profiles()[from]);
        initial = src ? clone(src) : { kind: 'llama' };
        if (initial.aliases) delete initial.aliases;
        if (from) newId = from + '-copy';
        if (mode === 'new') delete initial.kind;
      } else {
        const p = pending.get(id);
        initial = p && p.type !== 'delete' ? clone(p.profile) : clone(profiles()[id]);
        if (!initial) { editor = { missing: id }; return; }
      }
      editor = makeEditor({
        fields, mode, id, unsaved: mode === 'edit' && !profiles()[id], from, newId, initial, exists: v => !!profiles()[v] || (pending.has(v) && pending.get(v).type !== 'delete'),
        onStage: (pid, prof, m) => {
          const existing = !!profiles()[pid];
          pending.set(pid, { type: existing ? 'update' : 'create', profile: prof, errors: [] });
          saved = null;
        },
        onReview: () => { navigate('profiles', null, null, true); review(); },
        onBack: () => navigate('profiles', null, null, true),
      });
      const p = pending.get(id);
      if (p && p.errors && p.errors.length) setTimeout(() => editor && editor.setMessages && editor.setMessages(p.errors), 0);
    }

    // ---- pending ----
    const stageDelete = id => { pending.set(id, { type: 'delete', errors: [] }); saved = null; view.refresh(); };
    const undo = id => { pending.delete(id); view.refresh(); };
    const discard = async () => {
      if (await confirm({ title: 'Discard unsaved changes?', intro: `${plural(pending.size, 'profile change')} will be thrown away. Nothing on the server changes.`, confirmLabel: 'Discard', danger: true })) {
        pending.clear(); view.refresh();
      }
    };
    const buildOps = () => [...pending].map(([id, p]) => {
      const path = '/api/config/profiles/' + encodeURIComponent(id);
      if (p.type === 'create') return { label: `Create profile ${id}`, method: 'POST', path: '/api/config/profiles', body: { id, profile: p.profile } };
      if (p.type === 'update') return { label: `Change profile ${id}`, method: 'PUT', path, body: { profile: p.profile } };
      return { label: `Delete profile ${id}`, method: 'DELETE', path, forceable: true };
    });
    async function review() {
      if (!pending.size) return;
      const r = await reviewChanges({
        title: 'Review profile changes', getRevision: rev, buildOps,
        reload: async () => { await load(); },
        onInvalid: messages => {
          for (const [id, p] of pending) {
            const own = messages.filter(m => m.includes(`profile "${id}"`));
            p.errors = own.length ? own : messages.filter(m => !/profile "/.test(m));
          }
          const first = [...pending].find(([, p]) => p.errors.length);
          if (first && !route.rest.length) setTimeout(() => navigate('profiles', [first[0]]), 0);
          else if (editor && editor.setMessages) editor.setMessages((pending.get(route.rest[0]) || {}).errors || messages);
        },
      });
      if (r && r.saved) {
        for (const id of [...pending.keys()]) pending.delete(id);
        saved = r; watch.clear(); editor = null; editorKey = '';
        if (route.rest.length) navigate('profiles', null, null, true);
      }
      await load();
    }

    // ---- drawing ----
    function listCard(st) {
      const loaded = new Map((st && st.instances || []).map(i => [i.profile, i]));
      const all = Object.keys(profiles());
      for (const [id, p] of pending) if (p.type === 'create' && !all.includes(id)) all.push(id);
      const get = id => (pending.has(id) && pending.get(id).type !== 'delete' ? pending.get(id).profile : profiles()[id]) || {};
      const q = search.trim().toLowerCase();
      const match = id => {
        const p = get(id);
        return !q || id.toLowerCase().includes(q) || (p.model || '').toLowerCase().includes(q) || (p.aliases || []).some(a => a.toLowerCase().includes(q));
      };
      const counts = {
        all: all.length, loaded: all.filter(id => loaded.has(id)).length, notloaded: all.filter(id => !loaded.has(id)).length,
        forced: all.filter(id => (get(id).force_nodes || []).length).length, auto: all.filter(id => !(get(id).force_nodes || []).length).length,
        changed: all.filter(id => pending.has(id)).length,
      };
      const inFilter = id => filter === 'all' || (filter === 'loaded' && loaded.has(id)) || (filter === 'notloaded' && !loaded.has(id))
        || (filter === 'forced' && (get(id).force_nodes || []).length) || (filter === 'auto' && !(get(id).force_nodes || []).length) || (filter === 'changed' && pending.has(id));
      const rows = all.filter(id => match(id) && inFilter(id)).sort();
      const tools = h('div.cs', { style: 'display:flex;flex-wrap:wrap;gap:10px 16px;align-items:center;border-top:0;padding-top:0' },
        searchBox(search, v => { search = v; view.refresh(); }, 'Search by name, alias or model file', 'Search profiles'),
        chips([{ id: 'all', label: 'All', count: counts.all }, { id: 'loaded', label: 'Loaded now', count: counts.loaded }, { id: 'notloaded', label: 'Not loaded', count: counts.notloaded },
          { id: 'forced', label: 'Forced nodes', count: counts.forced }, { id: 'auto', label: 'Automatic', count: counts.auto }, ...(counts.changed ? [{ id: 'changed', label: 'Unsaved', count: counts.changed }] : [])],
        filter, id => { filter = id; view.refresh(); }, 'Filter profiles'));
      const status = id => {
        const inst = loaded.get(id), p = pending.get(id);
        const b = [];
        if (p) b.push(p.type === 'delete' ? badge('Will be deleted', 'aside') : p.type === 'create' ? badge('New, unsaved', 'q') : badge('Edited, unsaved', 'q'));
        if (p && p.errors && p.errors.length) b.push(badge('Not accepted', 'fail', 'The server refused this profile. Open it to see why.'));
        if (inst) b.push(inst.state === 'starting' ? badge('Loading', 'load') : badge('Loaded', 'use', 'A model of this profile is loaded now'));
        if (inst && inst.stale_profile) b.push(badge('Older definition', 'settle', 'Its profile was changed or deleted after it loaded. It keeps running as it was; the new definition is used at its next load.'));
        if (!b.length) b.push(badge('Not loaded', 'n'));
        return h('span', { style: 'display:flex;flex-wrap:wrap;gap:4px' }, b);
      };
      const body = !rows.length ? empty(all.length ? 'No profile matches' : 'No profiles yet',
        all.length ? 'Change the search or the filter.' : 'A profile names a model file and how to load it. Add the first one to let the scheduler place a model.',
        all.length ? null : button('Add the first profile', () => navigate('profiles', ['new']), 'pri sm'))
        : table({
          minWidth: 1020, rows, key: id => id, onRow: id => navigate('profiles', [id]),
          rowClass: id => (pending.has(id) && pending.get(id).type === 'delete' ? 'mut' : ''),
          columns: [
            { label: 'Profile', render: id => h('span', h('span.mono.nowrap', { style: 'font-weight:500' }, id), get(id).aliases && get(id).aliases.length ? h('span.sub', 'also ' + get(id).aliases.join(', ')) : null,
              pending.has(id) && pending.get(id).errors && pending.get(id).errors.length ? h('span.sub.err-t', pending.get(id).errors[0]) : null) },
            { label: 'Model file', render: id => { const p = get(id); return p.kind === 'command' ? h('span', badge('Command', 'n'), h('span.sub.mono.wrap-any', p.cmd || ''))
              : h('span', h('span.mono.wrap-any', p.model || DASH), (p.extra_args || []).length ? h('span.sub', plural(p.extra_args.length, 'extra argument')) : null); } },
            { label: 'Context', cls: 'r', render: id => { const p = get(id); return p.kind === 'command' ? h('span.mut', DASH) : h('span', ctx(p.ctx), (p.parallel || 1) > 1 ? h('span.sub', `${p.parallel} slots`) : null); } },
            { label: 'Placement', render: id => placementCell(get(id)) },
            { label: 'Memory need', cls: 'r', title: 'Configured, learned from a finished load, or estimated from the model file', render: id => memoryCell(get(id), plans[id]) },
            { label: 'State', cls: 'tight', render: status },
            { label: '', cls: 'tight', render: id => h('span.row', { style: 'gap:6px;justify-content:flex-end;flex-wrap:nowrap' },
              pending.has(id) && pending.get(id).type === 'delete' ? button('Undo', e => { e.stopPropagation(); undo(id); }, 'sm')
                : [button('Edit', e => { e.stopPropagation(); navigate('profiles', [id]); }, 'sm'),
                  button('Copy', e => { e.stopPropagation(); navigate('profiles', ['new'], { from: id }); }, 'sm'),
                  button('Delete', e => { e.stopPropagation(); stageDelete(id); }, 'sm dng')]) },
          ],
        });
      return h('section.card', { key: 'list' }, h('div.ch', h('h2', 'Profiles'), h('span.note', 'Click a profile to edit it. Models of a changed profile keep their old definition until they load again.')), tools, body);
    }

    function editorCard(st) {
      if (editor && editor.missing) return h('section.card', { key: 'missing' }, empty('There is no profile ' + editor.missing, 'It may have been deleted. Go back to the list.', h('a.btn.sm', { href: href('profiles') }, 'Back to the list')));
      const id = route.rest[0];
      const nodes = [editor.el];
      if (id && id !== 'new' && profiles()[id]) {
        const inst = (st && st.instances || []).find(i => i.profile === id);
        nodes.push(h('section.card', { key: 'plan' },
          h('div.ch', h('h2', 'Where it would go now'), h('span.note', 'For the saved definition. It updates after you save.')),
          h('div.cb', h('div.col', { style: 'gap:8px' }, planLine((plans[id] || {}).plan),
            inst ? h('span.mut', inst.stale_profile ? 'A model of this profile is loaded with an older definition.' : 'A model of this profile is loaded now.') : null))));
      }
      return nodes;
    }

    const render = () => {
      const st = state.status;
      const n = cfg ? Object.keys(profiles()).length : null;
      const loadedN = st && cfg ? (st.instances || []).filter(i => profiles()[i.profile]).length : null;
      const head = pageHead('Profiles',
        n == null ? null : [h('b', num(n)), n === 1 ? ' profile' : ' profiles', loadedN != null ? [' · ', h('b', num(loadedN)), ' loaded now'] : null, pending.size ? [' · ', h('b', num(pending.size)), ' unsaved'] : null],
        cfg && !route.rest.length ? button('New profile', () => navigate('profiles', ['new']), 'pri') : null);
      if (error && !cfg) return [head, banner('err', ['The configuration could not be read: ', error]), ];
      if (!cfg || !fields) return [head, h('section.card', skeleton(5))];
      const pbar = pendingBar({ count: pending.size, what: 'profiles', onReview: review, onDiscard: discard });
      return [
        head,
        noticeBanner(watch.notice, { onReload: () => { watch.clear(); load(); }, onDismiss: () => { watch.clear(); view.refresh(); } }),
        savedBanner(saved, () => { saved = null; view.refresh(); }),
        cfg.configured === false ? banner('info', [h('b', 'No configuration has been imported yet. '),
          'The scheduler runs with its defaults and has no profiles and no tiers, so it cannot place a model. Import a configuration file with ',
          h('span.mono', 'llm-scheduler import-config'), ', or add the first profile here and the tiers under ', h('a', { href: href('tiers') }, 'Tiers and nodes'), '.']) : null,
        route.rest.length ? editorCard(st) : listCard(st),
        pbar,
      ];
    };
    const view = liveView(el, render, ['status', 'conn']);
    load();
    return {
      unmount() { alive = false; watch.off(); view.unmount(); },
      update(r) { route = r; syncEditor(); view.refresh(); },
    };
  },
});
