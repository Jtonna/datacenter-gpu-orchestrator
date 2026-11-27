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
import { liveView, pageHead, badge, nodeChips, empty, banner, skeleton, button, schemaForm, searchField, facts, fileShort, dialog, toast, toggle, profileLabel, profileStage } from '../core/ui.js';
import { openPanel, openPanelNow } from '../core/panel.js';
import { pager } from '../core/pager.js';
import { num, mib, gb, ctx, plural, dur } from '../core/format.js';
import { plainReason, plainBasis, machines, nodeName } from '../core/status.js';
import { loadSchema, loadConfig, fieldsOf, clone, fieldOfMessage } from '../core/configdata.js';
import { reviewChanges, savedBanner, watchConfig, noticeBanner } from '../core/changes.js';
import { catalog, onCatalog, needCatalog } from '../core/catalog.js';
import { restorePoints } from './models/kinds.js';
import { profileDiff } from '../core/profilediff.js';

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
  stall_timeout: 'Stall timeout', load_stall_timeout: 'Load stall timeout', description: 'Description', display_name: 'Display name',
};
// stage is set by Promote and Demote (their own audited calls), not in the
// editor: the editor leaves it out and the save keeps the stored one
const NOT_EDITED = ['stage'];
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

// makeEditor(o) -> {el}: the profile editor. Used here (its own page) and in
// the Models side panel (views/models.js, the owner's note 8): there o.inPanel
// is set, o.onCancel replaces the links back to the list, and o.above (a node)
// sits above the fields (the plan preview).
export function makeEditor(o) {
  const { mode, id } = o;
  const fields = o.fields.filter(f => !NOT_EDITED.includes(f.key));
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
    needs_mib: '0 = estimate from the model file', model: 'for example gemma-4-12b-it-Q4_K_M.gguf', display_name: 'Shown in place of the id; leave empty to show the id' };
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
  const cancel = o.onCancel ? h('button.btn.lg', { type: 'button', 'data-cancel': '', onclick: () => o.onCancel() }, 'Cancel') : h('a.btn.lg', { href: o.back }, 'Cancel');
  const el = h('section', { class: 'panel prf-editor' + (o.inPanel ? ' prf-inpanel' : ''), 'data-keep': '', key: 'editor-' + (mode === 'edit' ? id : mode) + '-' + Math.random().toString(36).slice(2, 7), 'aria-label': mode === 'edit' ? 'Edit ' + id : 'New profile' },
    o.inPanel ? null : h('div.prf-ebar', h('h2', mode === 'edit' ? ['Edit profile ', h('span.mono', id)] : mode === 'dup' ? ['New profile, copied from ', h('span.mono', o.from)] : 'New profile'),
      h('a.btn.sm', { href: o.back }, 'Back to the list')),
    h('div.prf-ebody',
      o.above || null,
      serverBox,
      mode === 'edit' ? null : h('div.field', { style: 'max-width:460px' }, h('label', 'Name', h('span', { class: 'b b-n' }, 'Required')), idInput,
        h('span.help', 'The model name clients send. It cannot be changed later; to rename, copy the profile and delete the old one.'), idErr),
      host),
    h('div.prf-efoot', h('span.mut', 'Saving runs a check first, then asks for a comment.'), h('span.grow'),
      cancel, button('Save', save, 'lg pri', { 'data-save': '' })));
  return { el };
}

// saveProfile({pid, prof, exists, getRevision, reload, onInvalid}) -> the
// review and the write of one profile (a dry run, the diff, a comment the
// save waits for, If-Match; the audit entry is the server's): POST for a new one,
// PUT for an existing one. Resolves to reviewChanges' answer. Used by the
// editor page here and by the Models side panel.
export function saveProfile(o) {
  const { pid, prof, exists } = o;
  const op = exists ? { label: `Change profile ${pid}`, method: 'PUT', path: '/api/config/profiles/' + encodeURIComponent(pid), body: { profile: prof } }
    : { label: `Create profile ${pid}`, method: 'POST', path: '/api/config/profiles', body: { id: pid, profile: prof } };
  return reviewChanges({ title: exists ? 'Save profile ' + pid : 'Create profile ' + pid, getRevision: o.getRevision, buildOps: () => [op], reload: o.reload, onInvalid: o.onInvalid, requireComment: true });
}

// ---------- promote and demote ----------

// changeStage(e, to) -> the dialogs and the write that mark a profile
// production (promote) or test (demote): POST /api/config/profiles/<id>/promote
// or /demote with a required comment (audited; nothing is written when it
// is already so). It asks TWICE (the owner's walkthrough, build-92 W2): step
// 1 says what it does and takes the comment ("Review"); step 2 asks
// "Promote <name>?" with Back and Promote. Nothing is sent before the second
// press. Resolves to the answer, or null when cancelled or refused (a toast
// says why). e is a GET /api/profiles (or /api/profiles/test) row.
export async function changeStage(e, to) {
  const promote = to === 'production';
  const shown = profileLabel(e);
  const name = e.display_name || e.id;
  const after = promote ? name : 'TEST - ' + name;
  const verb = promote ? 'Promote' : 'Demote';
  let comment = '';
  for (;;) {
    const r = await dialog({
      title: h('span', verb + ' ', h('span.mono', e.id)),
      body: h('div.col', { style: 'gap:10px', 'data-dlg': promote ? 'promote' : 'demote' },
        h('p', promote ? "It becomes a production profile: listed with the model's profiles." : 'It becomes a test profile: moved to the Test profiles page, out of the way.'),
        h('p', 'Shown as ', h('b', shown), ' now, ', h('b', { 'data-after': '' }, after), ' after. The id clients send stays ', h('span.mono', e.id),
          '. Nothing about where or how it loads changes.')),
      comment: { label: 'Comment', required: true, placeholder: 'Why? It is kept in the audit log.', value: comment },
      actions: [{ label: 'Cancel', value: undefined }, { label: 'Review', kind: 'pri', value: true }],
    });
    if (!r || r.value !== true || !r.comment) return null;
    comment = r.comment;
    const c = await dialog({
      title: verb + ' ' + name + '?',
      body: h('div.col', { style: 'gap:10px', 'data-dlg': (promote ? 'promote' : 'demote') + '-confirm' },
        h('p', promote ? 'It will be a production profile, shown as ' : 'It will be a test profile, shown as ', h('b', after), '.'),
        h('p', h('span.mut', 'Comment: '), h('span', { 'data-comment': '' }, comment))),
      actions: [{ label: 'Back', value: 'back' }, { label: verb, kind: 'pri', value: true, autofocus: true }],
    });
    if (c === 'back') continue;
    if (c !== true) return null;
    break;
  }
  try {
    const res = await api.send('POST', '/api/config/profiles/' + encodeURIComponent(e.id) + (promote ? '/promote' : '/demote'), { comment }, { comment });
    toast(res && res.changed ? (promote ? 'Promoted ' : 'Demoted ') + e.id + (res.revision ? ' (revision ' + num(res.revision) + ')' : '') + '.'
      : e.id + ' was already ' + (promote ? 'production' : 'test') + ': nothing written.', 'ok');
    return res || {};
  } catch (err) { toast(verb + ' of ' + e.id + ' refused: ' + err.message, 'err'); return null; }
}

// testWhy(e) -> why a profile is test, in words: marked, generated, forced zones
export function testWhy(e) {
  if (e.stage_set && e.stage === 'test') return 'marked test';
  if (e.generated) return 'generated';
  if ((e.force_nodes || []).length) return 'forced zones';
  return '';
}

// ---------- the side panel (Profiles and Test profiles) ----------

// readSpec(id) -> the profile as written (GET /api/config/profiles/<id>):
// its extra arguments, and the settings "Compare with…" compares
const readSpec = async id => ((await api.get('/api/config/profiles/' + encodeURIComponent(id))) || {}).profile || {};

// whereText(profile, plan, instance): names only where they are given
// (the page rule): a forced profile's own zones, or the machines a loaded
// instance is on. An automatic profile's plan picks among equal machines
// and can pick others a moment later, so it says how many.
function whereText(e, plan, inst) {
  const st = state.status || {};
  if ((e.force_nodes || []).length || e.placement === 'forced') return (plan.nodes || []).length ? [h('span.mut', 'on'), nodeChips(plan.nodes)] : null;
  const on = inst ? (inst.nodes || []).map(n => nodeName(st, n && typeof n === 'object' ? n.id : n)) : [];
  if (on.length) return h('span.row', { style: 'gap:8px', 'data-loaded-on': String(on.length) }, h('span.mut', 'Loaded on'), nodeChips(on));
  const n = (plan.nodes || []).length;
  // loaded or loading but its instance not in the status yet: a count, not "would load"
  const now = plan.action === 'serve' || plan.action === 'wait';
  return n ? h('span', { 'data-would': String(n) }, (now ? 'on ' : 'would load on ') + machines(st, n)) : null;
}

// prfFacts(pairs) -> core facts() with prose values: the panel's values are
// sans at the body weight, a figure or an id says span.mono itself
function prfFacts(pairs) {
  const dl = facts(pairs);
  dl.classList.add('prf-facts');
  return dl;
}

// panelBody(id, e, spec): the profile's config (a GET /api/profiles row and,
// once read, its spec: undefined while reading, null when it could not be read)
function panelBody(id, e, spec) {
  if (!e) return [empty('There is no profile ' + id, 'It may have been deleted, or its name changed.')];
  const plan = e.plan || {};
  const [label, kind, why] = verdict(plan);
  const inst = ((state.status && state.status.instances) || []).find(i => i.profile === id);
  const force = e.force_nodes || [];
  const rp = restorePoints(e);
  const xa = spec && spec.extra_args;
  return [
    e.description ? h('p.prf-desc', { key: 'desc' }, e.description) : null,
    // the values: sans at the body weight (dl.prf-facts); mono only for ids,
    // file names, flags and figures (the reviewer, build-92)
    h('section.prf-part', { key: 'facts' }, prfFacts([
      ['Model', h('span', { title: e.model }, shortModel(e.model) || e.model || '–')],
      ['Model file', h('span.mono', { title: e.model, 'data-fact': 'file' }, e.model || '–')],
      e.kind === 'command' ? ['Kind', 'command'] : ['Context', e.ctx ? h('span.mono', { 'data-fact': 'ctx' }, ctx(e.ctx)) : '–'],
      (e.parallel || 1) > 1 ? ['Parallel slots', h('span.mono', String(e.parallel))] : null,
      (e.aliases || []).length ? ['Aliases', h('span.mono', e.aliases.join(', '))] : null,
      e.needs_mib ? ['Memory need', h('span.mono', { title: mib(e.needs_mib) + (e.needs_basis ? ' · ' + e.needs_basis : ''), 'data-fact': 'need' }, gb(e.needs_mib))] : null,
      e.needs_mib && e.needs_basis ? ['Worked out', h('span', { title: e.needs_basis }, plainBasis(e.needs_basis))] : null,
      // the restore points it keeps on the hosting machine, and where the number comes from (build-92 W1)
      rp ? ['Restore points', h('span', { 'data-restore-points': String(rp.n), title: e.host_memory.checkpoints_from || null },
        h('span.mono', num(rp.n) + ' (' + num(Math.round(rp.mib || 0)) + ' MB)'), rp.own ? ' · its own, from its extra arguments' : ' · the cluster default')] : null,
      // its extra arguments as written, and when it unloads (build-92 W2)
      e.kind === 'command' ? null : ['Extra arguments', spec === undefined ? h('span.mut', 'reading…') : spec === null ? h('span.mut', 'could not be read')
        : (xa || []).length ? h('span.mono', { 'data-extra-args': '' }, xa.join(' ')) : h('span.mut', { 'data-extra-args': '' }, 'none')],
      typeof e.ttl_s === 'number' ? ['Unload when idle', h('span', { 'data-ttl': String(e.ttl_s) }, e.ttl_s > 0 ? 'after ' + dur(e.ttl_s) : 'never')] : null,
      ['Stage', h('span', { 'data-stage': profileStage(e) }, profileStage(e) === 'test' ? 'Test (' + testWhy(e) + ')' : 'Production' + (e.stage_set ? '' : ' (not marked: the rule)'))],
      e.display_name ? ['Display name', e.display_name] : null,
      ['Id', h('span.mono', { 'data-fact': 'id' }, e.id)],
      ['Generated', h('span', { 'data-fact': 'generated' }, e.generated ? 'yes, for benchmarks' : 'no, made by hand')],
    ])),
    h('section.prf-part', { key: 'placement' }, h('h3', 'Placement'),
      force.length ? [h('div', h('span.mut', 'Forced on ' + plural(force.length, 'zone') + ': '), nodeChips(force)),
        (e.tensor_split || []).length ? h('div', h('span.mut', 'Tensor split: '), h('span.mono', e.tensor_split.join(' / '))) : null]
        : h('div', 'Automatic: the scheduler picks the zones at each load.', (e.allow_tiers || []).length ? h('span.mut', ' Tiers ' + e.allow_tiers.join(', ') + ' only.') : null),
      e.host === 'node' ? h('div', hostLine(e)) : null,
      e.blocked_by_exclusion ? h('div.err-t', 'Blocked: a forced zone is switched off (' + e.blocked_by_exclusion + ').') : null),
    h('section.prf-part', { key: 'plan' }, h('h3', 'Where it would go now'),
      h('div.row', { style: 'gap:8px' }, badge(label, kind, why), whereText(e, plan, inst), plan.tight_fit ? badge('Tight fit', 'load') : null),
      plan.reason ? h('p.mut', { title: plan.reason }, plainReason(plan.reason)) : null,
      plan.warning ? h('p.warn-t', { title: plan.warning }, plainReason(plan.warning)) : null,
      inst ? h('p', inst.stale_profile ? 'A model of this profile is loaded with an older definition.' : 'A model of this profile is loaded now.') : null,
      h('a', { href: href('placement', [id]) }, 'Open in Placement preview')),
  ];
}

// deleteProfile(id) -> the review and the DELETE of one profile; resolves to
// reviewChanges' answer, or null (a toast says why)
export async function deleteProfile(id) {
  let revision = 0;
  try { revision = (await api.get('/api/config/profiles/' + encodeURIComponent(id))).revision; } catch (e) { toast('The profile ' + id + ' could not be read: ' + e.message, 'err'); return null; }
  return reviewChanges({ title: 'Delete profile ' + id, saveLabel: 'Delete it', getRevision: () => revision,
    buildOps: () => [{ label: `Delete profile ${id}`, method: 'DELETE', path: '/api/config/profiles/' + encodeURIComponent(id), forceable: true }],
    reload: async () => { revision = (await api.get('/api/config/profiles/' + encodeURIComponent(id))).revision; } });
}

// openProfilePanel({id, label, cur, all, reload, onDeleted, onClose}) -> the
// side panel of one profile, the same on Profiles and on Test profiles (the
// owner's walkthrough, build-92 W2): its config, placement and plan; Edit
// (the one primary button), Duplicate, Promote or Demote, Compare with…,
// Delete. cur() is its GET /api/profiles row NOW (after a promote the list
// is read again and the title, sub and buttons follow); all() every row.
export function openProfilePanel(o) {
  const { id } = o;
  let spec, p = null;
  readSpec(id).then(x => { spec = x; }, () => { spec = null; }).then(() => { if (p && openPanelNow() === p) p.redraw(); });
  p = openPanel({
    label: o.label || 'Profile', name: (o.label || 'Profile') + ' ' + id,
    title: () => { const e = o.cur(); return e ? h('span', { 'data-shown': '' }, profileLabel(e)) : h('span.mono', id); },
    sub: () => { const e = o.cur(); return e ? [profileLabel(e) !== id ? id : null, shortModel(e.model), e.ctx ? ctx(e.ctx) + ' context' : null].filter(Boolean).join(' · ') : null; },
    body: () => panelBody(id, o.cur(), spec),
    foot: () => { const e = o.cur(); return e ? [
      h('a.btn.pri', { href: href('profiles', [id]), 'data-act': 'edit' }, 'Edit'),
      h('a.btn', { href: href('profiles', ['new'], { from: id }), 'data-act': 'copy' }, 'Duplicate'),
      profileStage(e) === 'test'
        ? button('Promote', async () => { if (await changeStage(e, 'production')) o.reload(); }, '', { 'data-act': 'promote', title: 'Make it a production profile (a comment is required)' })
        : button('Demote', async () => { if (await changeStage(e, 'test')) o.reload(); }, '', { 'data-act': 'demote', title: 'Make it a test profile (a comment is required)' }),
      button('Compare with…', () => compareDialog(e, o.all()), '', { 'data-act': 'compare', title: 'Its settings beside any other profile: read only' }),
      h('span.grow'),
      button('Delete', async () => { const r = await deleteProfile(id); if (r && r.saved) o.onDeleted(r); }, 'dng', { 'data-act': 'delete' }),
    ] : null; },
    onClose: o.onClose,
  });
  return p;
}

// ---------- Compare with… (build-92 W2) ----------

// show a setting's value in the compare table
function cmpValue(v) {
  if (v === undefined) return h('span.mut', 'not set');
  if (v === true) return h('span', 'set');
  if (Array.isArray(v)) return v.length ? h('span.mono', v.join(', ')) : h('span.mut', 'empty');
  if (v && typeof v === 'object') return h('span.mono', JSON.stringify(v));
  return h('span.mono', String(v));
}
// the need a dry plan gives (POST /api/plan): the figure and how it was worked out
function cmpNeed(r) {
  if (!r || r.status !== 'fulfilled') return h('span.mut', { title: r && r.reason ? r.reason.message : '' }, 'no plan');
  const v = r.value || {};
  if (!v.need_mib) return h('span.mut', v.reason ? plainReason(v.reason) : 'not known');
  return h('span', { title: mib(v.need_mib) + (v.need_basis ? ' · ' + v.need_basis : '') }, h('span.mono', gb(v.need_mib)), v.need_basis ? h('span.mut', ' · ' + plainBasis(v.need_basis)) : null);
}
const cmpLabel = key => (key.startsWith('extra_args ') ? h('span', 'Extra argument ', h('span.mono', key.slice(11))) : LABELS[key] || key);

// compareDialog(e, all): pick ANY profile (search; production and test) and
// see "Setting | this profile | the other": the differences only, or every
// setting with "Show all settings"; extra arguments flag by flag
// (core/profilediff.js); the two dry plans' need side by side. Read only:
// two profile reads and two dry plans, no write and no route of its own.
export function compareDialog(e, all) {
  let q = '', other = null, showAll = false, got = null, err = '', seq = 0;
  const pool = () => (all || []).filter(x => x.id !== e.id);
  const list = h('div.cmp-picks', { 'data-cmp-picks': '' });
  const result = h('div.cmp-result');
  const search = h('input.input', { type: 'search', placeholder: 'Search any profile: name, id or model', 'aria-label': 'Profile to compare with', 'data-cmp-search': '',
    oninput: ev => { q = ev.target.value; drawList(); } });
  function drawList() {
    const s = q.trim().toLowerCase();
    // the exact id first, then ids that start with the search, then the rest
    const rank = x => (x.id.toLowerCase() === s ? 0 : x.id.toLowerCase().startsWith(s) ? 1 : 2);
    const hits = pool().filter(x => !s || x.id.toLowerCase().includes(s) || profileLabel(x).toLowerCase().includes(s) || String(x.model || '').toLowerCase().includes(s))
      .map((x, i) => [rank(x), i, x]).sort((a, b) => a[0] - b[0] || a[1] - b[1]).map(r => r[2]);
    list.replaceChildren(...hits.slice(0, 8).map(x => h('button', { type: 'button', class: 'cmp-pick' + (other && other.id === x.id ? ' on' : ''), 'data-pick': x.id, onclick: () => pick(x) },
      h('span', profileLabel(x)), h('span.mut', ' · ' + (profileStage(x) === 'test' ? 'test' : 'production'))),
    ), hits.length > 8 ? h('span.mut', '+ ' + num(hits.length - 8) + ' more: type to narrow') : null, !hits.length ? h('span.mut', 'No profile matches') : null);
  }
  function drawResult() {
    if (!other) { result.replaceChildren(h('p.mut', 'Pick a profile to compare with.')); return; }
    if (err) { result.replaceChildren(banner('err', ['The profiles could not be read: ', err])); return; }
    if (!got) { result.replaceChildren(skeleton(4)); return; }
    const rows = profileDiff(got.sa, got.sb, Object.keys(LABELS));
    const diff = rows.filter(r => !r.same);
    const shown = showAll ? rows : diff;
    result.replaceChildren(
      h('div.row', { style: 'gap:10px' }, toggle(showAll, v => { showAll = v; drawResult(); }, 'Show all settings'), h('span', 'Show all settings'),
        h('span.mut', { 'data-diff-count': String(diff.length) }, diff.length ? plural(diff.length, 'setting') + ' differ' : 'The settings are the same')),
      h('table.tbl.cmp-tbl', { 'data-compare': other.id },
        h('thead', h('tr', h('th', { scope: 'col' }, 'Setting'), h('th', { scope: 'col' }, profileLabel(e)), h('th', { scope: 'col' }, profileLabel(other)))),
        h('tbody',
          h('tr', { 'data-need': '' }, h('td', 'What it needs (the plan now)'), h('td', cmpNeed(got.pa)), h('td', cmpNeed(got.pb))),
          shown.map(r => h('tr', { 'data-key': r.key, 'data-flag': r.key.startsWith('extra_args ') ? r.key.slice(11) : null, 'data-diff': r.same ? 'same' : 'diff', 'data-only': r.only || null },
            h('td', cmpLabel(r.key)), h('td', cmpValue(r.a)), h('td', cmpValue(r.b)))))));
  }
  async function pick(x) {
    other = x; got = null; err = ''; const my = ++seq;
    drawList(); drawResult();
    try {
      const [sa, sb] = await Promise.all([readSpec(e.id), readSpec(x.id)]);
      const [pa, pb] = await Promise.allSettled([api.send('POST', '/api/plan', { model: e.id }), api.send('POST', '/api/plan', { model: x.id })]);
      if (my === seq) got = { sa, sb, pa, pb };
    } catch (er) { if (my === seq) err = er.message; }
    if (my === seq) drawResult();
  }
  drawList(); drawResult();
  return dialog({ title: h('span', 'Compare ', h('span', { 'data-shown': '' }, profileLabel(e)), ' with…'), wide: true,
    body: h('div.col', { style: 'gap:12px', 'data-dlg': 'compare' }, search, list, result),
    actions: [{ label: 'Close', value: undefined }] });
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
          const r = await saveProfile({ pid, prof, exists: !!profiles[pid], getRevision: rev, reload: loadEditorData, onInvalid: setMessages });
          if (r && r.saved) {
            saved = r; editor = null; editorKey = '';
            navigate('profiles', [], { q: q() || null, show: showAll() ? 'all' : null, profile: pid });
            load(); loadEditorData();
          }
        },
      });
    }

    function syncPanel() {
      const id = route.query.profile || '';
      if (!id || route.rest.length) { if (panel) { const p = panel; panel = null; panelId = ''; p.close(); } return; }
      if (panel && panelId === id && openPanelNow() === panel) { panel.redraw(); return; }
      if (!list) return;
      panelId = id;
      panel = openProfilePanel({ id, label: 'Profile', cur: () => (list || []).find(x => x.id === id), all: () => list || [], reload: load,
        onDeleted: r => { saved = r; go({ profile: null }); load(); },
        onClose: () => { if (panelId === id) { panel = null; panelId = ''; if (route.query.profile === id) go({ profile: null }); } } });
    }

    // ---- the list ----
    function listPanel(st) {
      const all = list || [];
      const s = q().trim().toLowerCase();
      const isTest = e => profileStage(e) === 'test';
      const nGen = all.filter(isTest).length;
      const match = e => !s || e.id.toLowerCase().includes(s) || profileLabel(e).toLowerCase().includes(s) || String(e.model || '').toLowerCase().includes(s) || shortModel(e.model).toLowerCase().includes(s)
        || (e.aliases || []).some(a => a.toLowerCase().includes(s)) || (e.description || '').toLowerCase().includes(s);
      const rows = all.filter(e => (showAll() || !isTest(e)) && match(e));
      const hiddenHits = showAll() ? 0 : all.filter(e => isTest(e) && match(e)).length;
      const pages = Math.max(1, Math.ceil(rows.length / PER));
      const pg = Math.min(page(), pages);
      const shown = rows.slice((pg - 1) * PER, pg * PER);
      const open = route.query.profile || '';
      const loaded = new Set(((st && st.instances) || []).map(i => i.profile));
      return h('section.panel.prf', { key: 'list' },
        h('div.prf-bar',
          searchField(q(), v => go({ q: v || null, page: null }), 'Search name, model, alias or description', 'prf-search', { size: 'lg' }),
          nGen ? h('button', { type: 'button', class: 'fc' + (showAll() ? ' on' : ''), 'aria-pressed': showAll() ? 'true' : 'false', 'data-gen': '',
            title: 'Test profiles: marked test, generated for benchmarks, or pinned to zones. They also have their own page.',
            onclick: () => go({ show: showAll() ? null : 'all', page: null }) }, showAll() ? 'Hide test profiles' : 'Show test profiles too', h('span.n', num(nGen))) : null,
          h('span.prf-count.mut', plural(rows.length, 'profile') + (hiddenHits && s ? ' · ' + num(hiddenHits) + ' test profiles match too' : '')),
          nGen ? h('a', { href: href('test-profiles'), 'data-test-page': '' }, 'Test profiles page') : null),
        !rows.length ? empty(all.length ? 'No profile matches' : 'No profiles yet',
          all.length ? (hiddenHits ? hiddenHits + ' test profiles match. Show the test profiles too to see them.' : 'Change the search.') : 'A profile names a model file and how to load it. Add the first one to let the scheduler place a model.',
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
                  h('div.prf-id', h('span', { 'data-shown': '', title: e.id }, profileLabel(e)), e.display_name ? h('span.mono.mut', e.id) : null,
                    isTest(e) ? h('span.plc-gen', { title: testWhy(e) }, 'test') : null, loaded.has(e.id) && e.plan && e.plan.action !== 'serve' ? badge('Loaded', 'use') : null),
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
      const hand = all.filter(e => profileStage(e) === 'production').length;
      const head = pageHead('Profiles', list ? [h('b', num(hand)), ' production · ', h('a', { href: href('test-profiles') }, h('b', num(all.length - hand)), ' test')] : null,
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
