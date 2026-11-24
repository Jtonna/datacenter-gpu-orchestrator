// Profiles (the gear's Profiles; no mockup: inferred from the design
// language, phase 5). Replaced the classic page (the classic UI
// is removed). The owner's complaint: the page listed every
// placement of every profile and was hard to read. Here:
//
//   - one row per profile: name (with its description on one line), the
//     model's short name, the context, its zones (forced on N, or
//     automatic), the plan's verdict as a small pill;
//   - the placements only in the profile's docked side panel, opened from a
//     row (?profile=<id>): forced zones, tensor split, allowed tiers, the
//     host, where it would go now; Edit, Duplicate and Delete there;
//   - a search; the generated benchmark profiles HIDDEN by default (the
//     server's `generated` flag on GET /api/profiles: the profile's
//     generated marker, else an id starting bench-, b- or m-), counted and
//     shown with "Show generated (N)";
//   - a pager; the address carries the search, the filter, the page and the
//     open panel.
//
// Editing (carried over from the classic page): #/profiles/<id> edits a
// profile, #/profiles/new a new one, #/profiles/new?from=<id> a copy; every
// field of the schema's profile scope, grouped. Save runs a dry run, shows
// the diff, asks for a comment and writes with If-Match
// (core/changes.js reviewChanges). Delete from the panel goes through the
// same review (with "delete anyway" when benchmarks refer to it).
//
// Reads: REST GET /api/profiles (list, plans), GET /api/config/schema and
// GET /api/config (the editor), GET /api/config/profiles/<id> (a delete's
// revision); the catalog (short names); WebSocket status (loaded) and
// "config" (refresh).
// Writes: POST /api/config/profiles, PUT and DELETE /api/config/profiles/<id>.
// Routes: #/profiles?q=&show=all&page=&profile=<id>, #/profiles/<id>, #/profiles/new[?from=<id>].

import { h } from '../core/dom.js';
import { registerView, href, navigate } from '../core/router.js';
import { state, on } from '../core/store.js';
import * as api from '../core/api.js';
import { liveView, pageHead, badge, nodeChips, empty, banner, skeleton, button, schemaForm, searchField, facts, fileShort } from '../core/ui.js';
import { openPanel, openPanelNow } from '../core/panel.js';
import { pager } from '../core/pager.js';
import { num, mib, gb, ctx, plural } from '../core/format.js';
import { plainReason, plainBasis } from '../core/status.js';
import { loadSchema, loadConfig, fieldsOf, clone, fieldOfMessage } from '../core/configdata.js';
import { reviewChanges, savedBanner, watchConfig, noticeBanner } from '../core/changes.js';
import { catalog, onCatalog, needCatalog } from '../core/catalog.js';

const PER = 16; // rows a page (as the Zones list)

// the verdict pill of a profile's short plan (GET /api/profiles plan.action)
const VERDICT = {
  serve: ['Loaded', 'ready', 'A loaded copy answers requests now'],
  wait: ['Loading', 'load', 'It is loading now'],
  start: ['Would start', 'use', 'Free zones can take it now'],
  evict: ['Would make room', 'q', 'It would unload another model first'],
  queue: ['Would wait', 'n', 'No zone can take it now; a request would wait'],
  backoff: ['Held back', 'aside', 'Its last load failed; a request is refused for now'],
  reject: ['Cannot be placed', 'fail', 'A request would be refused'],
};
const verdict = plan => VERDICT[plan && plan.action] || ['Not known', 'n', plan && plan.reason ? plan.reason : ''];

// the model's short name: the catalog's short_name, else made from the file name
const shortModel = file => { const m = file && catalog.byKey[file]; return (m && (m.short_name || m.name)) || (file ? fileShort(file) : ''); };

// hostLine(e) -> where llama-server runs for a profile hosted on a zone
// (GET /api/profiles: host, host_node, host_overhead_mib, host_problem).
// Kept with the classic module's name and signature.
export function hostLine(e) {
  if (!e || e.host !== 'node') return null;
  return h('span.sub', { title: 'llama-server runs on this zone, started by its agent; the zone also holds the host process' },
    'hosted on ' + (e.host_node || 'the first forced zone'), e.host_overhead_mib ? ` · +${gb(e.host_overhead_mib)} for the host` : null,
    e.host_problem ? h('span.err-t', ' · ' + e.host_problem) : null);
}

// zonesText(e) -> the Zones cell: "Forced on 3" or "Automatic"
const zonesText = e => ((e.force_nodes || []).length ? 'Forced on ' + e.force_nodes.length : 'Automatic');

// ---------- the editor (carried over from the classic page) ----------

const LABELS = {
  kind: 'Kind', model: 'Model file', ctx: 'Context size', parallel: 'Parallel slots', kv_quant: 'KV cache precision', flash_attn: 'Flash attention',
  needs_mib: 'Memory need', allow_tiers: 'Allowed tiers', force_nodes: 'Forced zones', tensor_split: 'Tensor split', extra_args: 'Extra arguments',
  cmd: 'Command', check_endpoint: 'Readiness path', aliases: 'Aliases', ttl: 'Idle time before unload', load_timeout: 'Load timeout',
  stall_timeout: 'Stall timeout', load_stall_timeout: 'Load stall timeout', description: 'Description',
};
const SECTIONS = [['identity', 'Identity'], ['model', 'Model'], ['memory', 'Memory'], ['placement', 'Placement'], ['command', 'Command'], ['timing', 'Timing']];
const NUMERIC_LISTS = ['allow_tiers', 'tensor_split'];

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
  // keys the schema does not list (generated, for example) are kept as they were
  for (const k of Object.keys(orig || {})) if (!fields.some(f => f.key === k) && out[k] === undefined) out[k] = orig[k];
  if (kind === 'command' || (orig && orig.kind)) out.kind = kind; else delete out.kind;
  return out;
}

function makeEditor(o) {
  const { fields, mode, id } = o;
  const orig = o.initial || {};
  let current = toForm(orig);
  let forms = [];
  const idInput = h('input', { class: 'input mono', type: 'text', value: o.newId || '', placeholder: 'for example gemma-4-12b-48k', autocomplete: 'off', spellcheck: 'false', 'aria-label': 'Profile name' });
  const idErr = h('span.err', { role: 'alert' });
  const serverBox = h('div');
  const host = h('div.col', { style: 'gap:22px' });
  const applicable = (f, kind) => !f.applies_to || f.applies_to === (kind === 'command' ? 'command' : 'llama');
  const PLACEHOLDER = { extra_args: '--rope-scaling yarn', force_nodes: 'Zone name, then Enter', allow_tiers: 'Tier number, then Enter', tensor_split: 'One weight per forced zone, then Enter',
    ttl: 'Same as the ttl setting', stall_timeout: 'Same as the stall_timeout setting', load_stall_timeout: 'Same as the load_stall_timeout setting', load_timeout: 'Worked out from the model size',
    needs_mib: '0 = estimate from the model file', model: 'for example gemma-4-12b-it-Q4_K_M.gguf' };
  const fieldDef = f => Object.assign({}, f, { label: LABELS[f.key], type: f.key === 'extra_args' ? 'text' : f.type, placeholder: PLACEHOLDER[f.key],
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
    const known = new Set(SECTIONS.map(s => s[0]));
    const secs = SECTIONS.concat([...new Set(fields.map(f => f.section))].filter(s => !known.has(s)).map(s => [s, s.charAt(0).toUpperCase() + s.slice(1)]));
    for (const [sec, title] of secs) {
      const fs = fields.filter(f => f.section === sec && applicable(f, kind));
      if (!fs.length) continue;
      const form = schemaForm({ fields: fs.map(fieldDef), values: current, showDefaults: false, onChange: (k, v) => {
        current[k] = v;
        if (k === 'kind') { collect(); build(); const sel = host.querySelector('select'); if (sel) sel.focus(); }
      } });
      forms.push({ section: sec, form, fields: fs });
      blocks.push(h('section.prf-esec', { 'data-sec': sec }, h('h3', title), form.el));
    }
    host.replaceChildren(...blocks);
  }
  build();
  function setMessages(messages) {
    const left = [], byKey = {};
    const keys = fields.map(f => f.key);
    for (const m of messages || []) {
      const k = fieldOfMessage(m, keys);
      if (k) byKey[k] = (byKey[k] ? byKey[k] + ' ' : '') + m.replace(/^profile "[^"]*":\s*/, '').replace(new RegExp('^' + k + ':\\s*'), '');
      else left.push(m);
    }
    for (const { form } of forms) form.setErrors(byKey);
    serverBox.replaceChildren(...(left.length ? [banner('err', h('span', h('b', 'The server did not accept this profile. '), h('ul', { style: 'margin:6px 0 0;padding-left:18px' }, left.map(m => h('li', m)))))] : []));
  }
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
  function save() {
    if (!check()) { const bad = el.querySelector('[aria-invalid=true]'); if (bad) bad.focus(); return; }
    collect();
    o.onSave(mode === 'edit' ? id : idInput.value.trim(), fromForm(current, fields, orig), setMessages);
  }
  const el = h('section.panel.prf-editor', { 'data-keep': '', key: 'editor-' + (mode === 'edit' ? id : mode) + '-' + Math.random().toString(36).slice(2, 7), 'aria-label': mode === 'edit' ? 'Edit ' + id : 'New profile' },
    h('div.prf-ebar', h('h2', mode === 'edit' ? ['Edit profile ', h('span.mono', id)] : mode === 'dup' ? ['New profile, copied from ', h('span.mono', o.from)] : 'New profile'),
      h('a.btn.sm', { href: o.back }, 'Back to the list')),
    h('div.prf-ebody',
      serverBox,
      mode === 'edit' ? null : h('div.field', { style: 'max-width:460px' }, h('label', 'Name', h('span', { class: 'b b-n' }, 'Required')), idInput,
        h('span.help', 'The model name clients send. It cannot be changed later; to rename, copy the profile and delete the old one.'), idErr),
      host),
    h('div.prf-efoot', h('span.mut', 'Saving runs a check first, then asks for a comment.'), h('span.grow'),
      h('a.btn.lg', { href: o.back }, 'Cancel'), button('Save', save, 'lg pri', { 'data-save': '' })));
  return { el };
}

// ---------- the view ----------

registerView({
  id: 'profiles', title: 'Profiles',
  mount(el, route0) {
    let route = route0, list = null, error = '', fields = null, cfg = null, saved = null, alive = true;
    let editor = null, editorKey = '', panel = null, panelId = '';

    const q = () => route.query.q || '';
    const showAll = () => route.query.show === 'all';
    const page = () => Math.max(1, Number(route.query.page) || 1);
    const listHref = extra => href('profiles', [], Object.assign({ q: q() || null, show: showAll() ? 'all' : null, page: page() > 1 ? String(page()) : null }, extra));
    const go = (query, rest) => navigate('profiles', rest || [], Object.assign({ q: q() || null, show: showAll() ? 'all' : null, page: page() > 1 ? String(page()) : null, profile: route.query.profile || null }, query), true);

    async function load() {
      try { list = (await api.get('/api/profiles')).profiles || []; error = ''; } catch (e) { error = e.message; }
      if (alive) { syncPanel(); view.refresh(); }
    }
    async function loadEditorData() {
      try {
        const [c, f] = await Promise.all([loadConfig(), loadSchema()]);
        cfg = c; fields = fieldsOf(f, 'profile');
      } catch (e) { error = error || e.message; }
      if (alive) { syncEditor(); view.refresh(); }
    }
    const rev = () => (cfg ? cfg.revision : 0);
    const watch = watchConfig({ revision: () => (cfg ? cfg.revision : (state.status && state.status.config_revision) || 0), pending: () => !!editor, refresh: () => { load(); if (cfg) loadEditorData(); }, redraw: () => view.refresh() });
    const offCatalog = onCatalog(() => { view.refresh(); if (panel) panel.redraw(); });
    needCatalog();

    // ---- the editor (its own page) ----
    function syncEditor() {
      const key = route.rest.join('/') + '?' + (route.query.from || '');
      if (!route.rest.length) { editor = null; editorKey = ''; return; }
      if (!cfg || !fields) return;
      if (editor && editorKey === key) return;
      editorKey = key;
      const id = route.rest[0];
      const profiles = cfg.profiles || {};
      let mode = 'edit', initial = null, from = null, newId = '';
      if (id === 'new') {
        mode = route.query.from ? 'dup' : 'new'; from = route.query.from || null;
        initial = from && profiles[from] ? clone(profiles[from]) : { kind: 'llama' };
        if (initial.aliases) delete initial.aliases;
        if (initial.generated) delete initial.generated;
        if (from) newId = from + '-copy';
        if (mode === 'new') delete initial.kind;
      } else {
        initial = clone(profiles[id]);
        if (!initial) { editor = { missing: id }; return; }
      }
      editor = makeEditor({
        fields, mode, id, from, newId, initial, back: listHref({ profile: mode === 'edit' ? id : from }),
        exists: v => !!profiles[v],
        onSave: async (pid, prof, setMessages) => {
          const exists = !!profiles[pid];
          const op = exists ? { label: `Change profile ${pid}`, method: 'PUT', path: '/api/config/profiles/' + encodeURIComponent(pid), body: { profile: prof } }
            : { label: `Create profile ${pid}`, method: 'POST', path: '/api/config/profiles', body: { id: pid, profile: prof } };
          const r = await reviewChanges({ title: exists ? 'Save profile ' + pid : 'Create profile ' + pid, getRevision: rev, buildOps: () => [op], reload: loadEditorData, onInvalid: setMessages });
          if (r && r.saved) {
            saved = r; editor = null; editorKey = '';
            navigate('profiles', [], { q: q() || null, show: showAll() ? 'all' : null, profile: pid });
            load(); loadEditorData();
          }
        },
      });
    }

    // ---- delete (from the panel) ----
    async function remove(id) {
      let revision = 0;
      try { revision = (await api.get('/api/config/profiles/' + encodeURIComponent(id))).revision; } catch (e) { saved = null; error = e.message; view.refresh(); return; }
      const r = await reviewChanges({ title: 'Delete profile ' + id, saveLabel: 'Delete it', getRevision: () => revision,
        buildOps: () => [{ label: `Delete profile ${id}`, method: 'DELETE', path: '/api/config/profiles/' + encodeURIComponent(id), forceable: true }],
        reload: async () => { revision = (await api.get('/api/config/profiles/' + encodeURIComponent(id))).revision; } });
      if (r && r.saved) { saved = r; go({ profile: null }); load(); }
    }

    // ---- the side panel: one profile, its placements ----
    function panelBody(id) {
      const e = (list || []).find(x => x.id === id);
      if (!e) return [empty('There is no profile ' + id, 'It may have been deleted, or its name changed.')];
      const plan = e.plan || {};
      const [label, kind, why] = verdict(plan);
      const inst = ((state.status && state.status.instances) || []).find(i => i.profile === id);
      const force = e.force_nodes || [];
      return [
        e.description ? h('p.prf-desc', { key: 'desc' }, e.description) : null,
        h('section.prf-part', { key: 'facts' }, facts([
          ['Model', h('span', { title: e.model }, shortModel(e.model) || e.model || '–')],
          ['Model file', h('span', { title: e.model }, e.model || '–')],
          e.kind === 'command' ? ['Kind', 'command'] : ['Context', e.ctx ? ctx(e.ctx) : '–'],
          (e.parallel || 1) > 1 ? ['Parallel slots', String(e.parallel)] : null,
          (e.aliases || []).length ? ['Aliases', e.aliases.join(', ')] : null,
          e.needs_mib ? ['Memory need', h('span', { title: mib(e.needs_mib) + (e.needs_basis ? ' · ' + e.needs_basis : '') }, gb(e.needs_mib))] : null,
          e.needs_mib && e.needs_basis ? ['Worked out', h('span', { title: e.needs_basis }, plainBasis(e.needs_basis))] : null,
          ['Generated', e.generated ? 'yes, for benchmarks' : 'no, made by hand'],
        ])),
        h('section.prf-part', { key: 'placement' }, h('h3', 'Placement'),
          force.length ? [h('div', h('span.mut', 'Forced on ' + plural(force.length, 'zone') + ': '), nodeChips(force)),
            (e.tensor_split || []).length ? h('div', h('span.mut', 'Tensor split: '), h('span.mono', e.tensor_split.join(' / '))) : null]
            : h('div', 'Automatic: the scheduler picks the zones at each load.', (e.allow_tiers || []).length ? h('span.mut', ' Tiers ' + e.allow_tiers.join(', ') + ' only.') : null),
          e.host === 'node' ? h('div', hostLine(e)) : null,
          e.blocked_by_exclusion ? h('div.err-t', 'Blocked: a forced zone is switched off (' + e.blocked_by_exclusion + ').') : null),
        h('section.prf-part', { key: 'plan' }, h('h3', 'Where it would go now'),
          h('div.row', { style: 'gap:8px' }, badge(label, kind, why), (plan.nodes || []).length ? [h('span.mut', 'on'), nodeChips(plan.nodes)] : null, plan.tight_fit ? badge('Tight fit', 'load') : null),
          plan.reason ? h('p.mut', { title: plan.reason }, plainReason(plan.reason)) : null,
          plan.warning ? h('p.warn-t', { title: plan.warning }, plainReason(plan.warning)) : null,
          inst ? h('p', inst.stale_profile ? 'A model of this profile is loaded with an older definition.' : 'A model of this profile is loaded now.') : null,
          h('a', { href: href('placement', [id]) }, 'Open in Placement preview')),
      ];
    }
    function syncPanel() {
      const id = route.query.profile || '';
      if (!id || route.rest.length) { if (panel) { const p = panel; panel = null; panelId = ''; p.close(); } return; }
      if (panel && panelId === id && openPanelNow() === panel) { panel.redraw(); return; }
      if (!list) return;
      panelId = id;
      const e = list.find(x => x.id === id);
      panel = openPanel({
        label: 'Profile', name: 'Profile ' + id,
        title: () => h('span.mono', id),
        sub: () => (e ? [shortModel(e.model), e.ctx ? ctx(e.ctx) + ' context' : null].filter(Boolean).join(' · ') : null),
        body: () => panelBody(id),
        foot: () => (e ? [
          h('a.btn.pri', { href: href('profiles', [id]), 'data-act': 'edit' }, 'Edit'),
          h('a.btn', { href: href('profiles', ['new'], { from: id }), 'data-act': 'copy' }, 'Duplicate'),
          h('span.grow'),
          button('Delete', () => remove(id), 'dng', { 'data-act': 'delete' }),
        ] : null),
        onClose: () => { if (panelId === id) { panel = null; panelId = ''; if (route.query.profile === id) go({ profile: null }); } },
      });
    }

    // ---- the list ----
    function listPanel(st) {
      const all = list || [];
      const s = q().trim().toLowerCase();
      const nGen = all.filter(e => e.generated).length;
      const match = e => !s || e.id.toLowerCase().includes(s) || String(e.model || '').toLowerCase().includes(s) || shortModel(e.model).toLowerCase().includes(s)
        || (e.aliases || []).some(a => a.toLowerCase().includes(s)) || (e.description || '').toLowerCase().includes(s);
      const rows = all.filter(e => (showAll() || !e.generated) && match(e));
      const hiddenHits = showAll() ? 0 : all.filter(e => e.generated && match(e)).length;
      const pages = Math.max(1, Math.ceil(rows.length / PER));
      const pg = Math.min(page(), pages);
      const shown = rows.slice((pg - 1) * PER, pg * PER);
      const open = route.query.profile || '';
      const loaded = new Set(((st && st.instances) || []).map(i => i.profile));
      return h('section.panel.prf', { key: 'list' },
        h('div.prf-bar',
          searchField(q(), v => go({ q: v || null, page: null }), 'Search name, model, alias or description', 'prf-search', { size: 'lg' }),
          nGen ? h('button', { type: 'button', class: 'fc' + (showAll() ? ' on' : ''), 'aria-pressed': showAll() ? 'true' : 'false', 'data-gen': '',
            title: 'Benchmark profiles the scheduler generated (their generated flag, or an id that starts bench-, b- or m-)',
            onclick: () => go({ show: showAll() ? null : 'all', page: null }) }, showAll() ? 'Hide generated' : 'Show generated', h('span.n', num(nGen))) : null,
          h('span.prf-count.mut', plural(rows.length, 'profile') + (hiddenHits && s ? ' · ' + num(hiddenHits) + ' generated match too' : ''))),
        !rows.length ? empty(all.length ? 'No profile matches' : 'No profiles yet',
          all.length ? (hiddenHits ? hiddenHits + ' generated profiles match. Show the generated ones to see them.' : 'Change the search.') : 'A profile names a model file and how to load it. Add the first one to let the scheduler place a model.',
          all.length ? null : h('a.btn.pri.sm', { href: href('profiles', ['new']) }, 'Add the first profile'))
          : h('div.prf-scroll', h('table.tbl.prf-tbl',
            h('thead', h('tr', h('th', { scope: 'col' }, 'Profile'), h('th', { scope: 'col' }, 'Model'), h('th.r', { scope: 'col' }, 'Context'),
              h('th', { scope: 'col', title: 'Forced zones (force_nodes) or automatic placement; the zones themselves are in the panel' }, 'Zones'), h('th', { scope: 'col' }, 'Now'))),
            h('tbody', shown.map(e => {
              const [label, kind, why] = verdict(e.plan);
              const openRow = () => go({ profile: e.id });
              return h('tr', { key: 'p-' + e.id, class: 'click' + (open === e.id ? ' sel' : ''), tabindex: '0', 'data-profile': e.id, 'aria-selected': open === e.id ? 'true' : null,
                onclick: openRow, onkeydown: ev => { if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); openRow(); } } },
                h('td.prf-name',
                  h('div.prf-id', h('span.mono', { title: e.id }, e.id), e.generated ? h('span.plc-gen', 'generated') : null, loaded.has(e.id) && e.plan && e.plan.action !== 'serve' ? badge('Loaded', 'use') : null),
                  e.description ? h('div.prf-d', { title: e.description }, e.description) : null),
                // a command profile runs a program, not a model file: it has no model and no context size
                e.kind === 'command'
                  ? h('td.prf-model', { 'data-cmdprof': '' }, h('span.mut', { title: 'A command profile starts a program on a machine; it loads no model file and has no context size' }, 'command profile'))
                  : h('td.prf-model', h('span', { title: e.model }, shortModel(e.model) || '–')),
                h('td.r', e.kind === 'command' ? null : ctx(e.ctx)),
                h('td.prf-zones', zonesText(e)),
                h('td.tight', badge(label, kind, plainReason(e.plan && (e.plan.reason || e.plan.warning)) || why)));
            })))),
        rows.length > PER ? h('div.prf-foot', pager({ page: pg, pages, total: rows.length, per: PER, range: true, label: 'Profile pages', onPage: n => go({ page: n > 1 ? String(n) : null }) })) : null);
    }

    function editorPage() {
      if (!cfg || !fields) return h('section.panel', { key: 'ed-wait' }, skeleton(6));
      if (editor && editor.missing) return h('section.panel', { key: 'missing' }, empty('There is no profile ' + editor.missing, 'It may have been deleted. Go back to the list.', h('a.btn.sm', { href: listHref() }, 'Back to the list')));
      return editor ? editor.el : null;
    }

    const render = () => {
      const st = state.status;
      const all = list || [];
      const hand = all.filter(e => !e.generated).length;
      const head = pageHead('Profiles', list ? [h('b', num(hand)), ' made by hand · ', h('b', num(all.length - hand)), ' generated'] : null,
        list && !route.rest.length ? h('a.btn.pri', { href: href('profiles', ['new']) }, 'New profile') : null);
      if (error && !list) return [head, banner('err', ['The profiles could not be read: ', error])];
      return [
        head,
        noticeBanner(watch.notice, { onReload: () => { watch.clear(); load(); }, onDismiss: () => { watch.clear(); view.refresh(); } }),
        savedBanner(saved, () => { saved = null; view.refresh(); }),
        route.rest.length ? editorPage() : !list ? h('section.panel', { key: 'wait' }, skeleton(8)) : listPanel(st),
      ];
    };
    const view = liveView(el, render, ['status', 'conn']);
    const offStatus = on('status', () => { if (panel) panel.redraw(); });
    load();
    if (route.rest.length) loadEditorData();
    return {
      update(r) {
        route = r;
        if (route.rest.length && !cfg) loadEditorData();
        syncEditor(); syncPanel(); view.refresh();
      },
      unmount() { alive = false; watch.off(); offCatalog(); offStatus(); if (panel) { const p = panel; panel = null; p.close(); } view.unmount(); },
    };
  },
});
