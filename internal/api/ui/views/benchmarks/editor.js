// Editors for benchmarks and suites. The benchmark editor is drawn from
// GET /api/bench/schema (phase types, options, expect keys, templates) and
// GET /api/bench/profiles; writes are POST / PUT / DELETE through core/api.js.
//
// Both editors build one stable node (data-keep) and redraw it by hand when
// the structure changes (a phase added, moved or removed), so typing is never
// interrupted by the live re-render of the screen.

import { h } from '../../core/dom.js';
import * as api from '../../core/api.js';
import { schemaForm, toggle, tagInput, confirm, toast, banner, empty, skeleton, button, badge, pickCards } from '../../core/ui.js';
import { plural, dateTime, DASH } from '../../core/format.js';
import { cache, loadSchema, loadProfiles, loadModelProfiles, loadBenchmarks, loadSuites, phasesOf, phaseTitle, describe, modelOf, benchmarkById, suiteTitle } from './common.js';

const clone = v => JSON.parse(JSON.stringify(v));
const slug = s => String(s || '').toLowerCase().replace(/[^a-z0-9._-]+/g, '-').replace(/^[^a-z0-9]+/, '').replace(/-+$/, '').slice(0, 64);
const EXPECT_LABEL = { outcome: 'Expected outcome', max_load_s: 'Load time, at most', min_decode_tps: 'Decode speed, at least', min_prefill_tps: 'Prefill speed, at least',
  max_ttft_ms: 'Time to first token, at most', output_sha256: 'Output hash', min_reused_tokens: 'Reused prompt tokens, at least', max_concurrent_wall_s: 'Simultaneous requests, wall time at most',
  max_errors: 'Failed simultaneous requests, at most', max_unload_s: 'Unload time, at most' };
const OPT_TYPE = { integer: 'int', number: 'float', boolean: 'bool', string: 'string', text: 'text', enum: 'enum' };

// An option of the schema as a schemaForm field.
function toField(o) {
  return { key: o.name, type: OPT_TYPE[o.type] || o.type, enum: o.values, default: o.default, min: o.min, max: o.max, unit: o.unit, description: o.description, required: !!o.required,
    label: o.name === 'max_tokens' ? 'Most tokens per answer' : undefined };
}

// Defaults that must show as "on" when the option is not set.
const shownValues = (fields, obj) => {
  const v = clone(obj || {});
  for (const f of fields) if (f.type === 'bool' && v[f.key] === undefined && f.default === true) v[f.key] = true;
  return v;
};

function bindForm(target, fields, afterChange, errors) {
  const form = schemaForm({
    fields, values: shownValues(fields, target),
    onChange: (k, v) => { if (v === undefined || v === '' || (Array.isArray(v) && !v.length)) delete target[k]; else target[k] = v; afterChange(); },
  });
  if (errors) form.setErrors(errors);
  return form;
}

// "phases[2] (conversation): turns[1].content is empty" -> {phases: {2: {...}}}.
function parseErrors(msg) {
  const out = { general: [], top: {}, phases: {}, items: [] };
  const TOP = [['id', 'id'], ['name', 'name'], ['description', 'description'], ['profile', 'profile'], ['timeout_s', 'timeout_s'], ['tag', 'tags'], ['cooldown_s', 'cooldown_s'],
    ['idle_wait_s', 'idle_wait_s'], ['schedule', 'schedule'], ['baseline', 'baseline'], ['benchmarks', 'benchmarks'], ['parallel', 'parallel']];
  for (const line of String(msg || '').split('\n').map(s => s.trim()).filter(Boolean)) {
    let m = /^phases\[(\d+)\](?:\s*\([^)]*\))?[:.]?\s*(.*)$/.exec(line);
    const alt = /^benchmark \S+ phase (\d+) \([^)]*\):\s*(.*)$/.exec(line); // the dev mock's wording
    if (alt) m = [alt[0], String(alt[1] - 1), alt[2]];
    if (m) {
      const P = out.phases[m[1]] || (out.phases[m[1]] = { general: [], fields: {} });
      const rest = m[2], key = (/^([a-z_]+)/.exec(rest) || [])[1];
      if (key && !/^(requests|turns|request)$/.test(key) && !/^option\b/.test(rest)) P.fields[key] = rest; else P.general.push(rest);
      continue;
    }
    const hit = TOP.find(([p]) => line.startsWith(p));
    if (hit && !/^the /.test(line)) { out.top[hit[1]] = line; continue; }
    out.general.push(line);
  }
  return out;
}

function errMessages(e) {
  const body = e.body && e.body.error;
  return (body && (body.message || body.messages)) ? [].concat(body.message || body.messages).join('\n') : e.message;
}

function field(text, control, o = {}) {
  const err = h('span.err', { role: 'alert' });
  let id = null;
  if (!o.plain && control && /^(INPUT|SELECT|TEXTAREA)$/.test(control.nodeName)) { id = 'bf-' + Math.random().toString(36).slice(2, 8); control.id = id; }
  const node = h('div', { class: 'field' + (o.wide ? ' wide' : '') }, o.plain ? h('div.lab', text) : h('label', { for: id }, text), control, o.help ? h('span.help', o.help) : null, err);
  return { node, setErr: m => { err.textContent = m || ''; const inp = node.querySelector('input,select,textarea'); if (inp) inp.setAttribute('aria-invalid', m ? 'true' : 'false'); } };
}

const section = (title, note, ...body) => h('section.card', h('div.ch', h('h2', title), note ? h('span.note', note) : null), ...body);
const inset = (...c) => h('div.sub-block', ...c);
const smallBtn = (text, fn, kind = '', attrs = {}) => h('button', Object.assign({ class: 'btn sm ' + kind, type: 'button', onclick: fn }, attrs), text);

// ---------- the benchmark editor ----------

export function benchmarkEditor(o) {
  const el = h('div.col', { 'data-keep': '' }, h('section.card', skeleton(4)));
  let dead = false, bm = null, original = null, stage = 'loading', idTouched = false, errs = parseErrors(''), saving = false, loadError = '';
  const isNew = !o.id;
  const draw = () => { if (!dead) el.replaceChildren(...(stage === 'loading' ? [h('section.card', loadError ? empty('This benchmark could not be opened', loadError) : skeleton(4))] : stage === 'pick' ? pick() : edit()).filter(Boolean)); };
  let summaryEl = null;
  const updateSummary = () => {
    if (summaryEl) summaryEl.textContent = describe(bm, modelOf(bm.profile) || bm.profile) || 'Add a phase to say what this benchmark does.';
  };

  (async () => {
    try {
      await Promise.all([loadSchema(), loadProfiles(), loadModelProfiles(), loadBenchmarks()]);
      if (!cache.schema) throw new Error((cache.errors.schema && cache.errors.schema.message) || 'The schema could not be read.');
      if (!isNew) {
        bm = await api.get(`/api/bench/benchmarks/${encodeURIComponent(o.id)}`);
        for (const k of ['source', 'created_at', 'updated_at']) delete bm[k];
        original = clone(bm);
        stage = 'edit';
      } else if (o.template) {
        startFrom(o.template);
      } else stage = 'pick';
    } catch (e) { loadError = e.message; }
    draw();
  })();

  function startFrom(id) {
    const t = id === 'blank' ? null : (cache.schema.templates || []).find(x => x.id === id);
    bm = t ? clone(t.benchmark) : { id: '', name: '', description: '', profile: '', phases: [{ type: 'load' }, { type: 'unload' }] };
    bm.id = ''; bm.name = ''; bm.profile = '';
    if (t) bm.description = (t.benchmark.description || '');
    stage = 'edit'; idTouched = false;
  }

  // ---- step 1: pick a template ----
  function pick() {
    const tpls = (cache.schema.templates || []);
    return [section('Start from a template', 'You can change everything afterwards',
      h('div.cb', pickCards(tpls.map(t => ({ title: t.name, text: t.description, chips: phasesOf(t.benchmark).map(p => phaseTitle(p.type)), onPick: () => { startFrom(t.id); draw(); } }))
        .concat([{ title: 'Blank', text: 'Load, then unload. Add the phases you want.', onPick: () => { startFrom('blank'); draw(); } }]))))];
  }

  // ---- step 2: edit ----
  function edit() {
    const schema = cache.schema, limits = schema.limits || {};
    const parallel = !!bm.parallel;
    const simple = !parallel && !(bm.phases && bm.phases.length);
    const topErr = {};
    const mk = (key, text, control, opts) => { const f = field(text, control, opts); topErr[key] = f.setErr; return f.node; };

    const nameIn = h('input.input', { type: 'text', value: bm.name || '', maxlength: 120, placeholder: 'For example: Gemma 12B, cold load on 2 workers', autocomplete: 'off' });
    const idIn = h('input.input.mono', { type: 'text', value: bm.id || '', maxlength: 64, disabled: !isNew, placeholder: 'lower-case letters, digits, . - _', autocomplete: 'off', spellcheck: 'false' });
    nameIn.addEventListener('input', () => { bm.name = nameIn.value; if (isNew && !idTouched) { bm.id = slug(nameIn.value); idIn.value = bm.id; } });
    idIn.addEventListener('input', () => { idTouched = true; bm.id = idIn.value.trim(); });
    const descIn = h('textarea.textarea', { rows: 3, maxlength: 2000, placeholder: 'What this benchmark does and measures. It is shown while the benchmark runs.' }, bm.description || '');
    descIn.addEventListener('input', () => { bm.description = descIn.value; });
    const profiles = cache.profiles || [];
    const profSel = h('select.select', { 'aria-label': 'Profile' },
      h('option', { value: '', selected: !bm.profile }, 'Choose a profile'),
      profiles.map(p => h('option', { value: p.id, selected: bm.profile === p.id }, p.id + (p.kind && p.kind !== 'llama' ? ` (${p.kind})` : ''))),
      bm.profile && !profiles.some(p => p.id === bm.profile) ? h('option', { value: bm.profile, selected: true }, bm.profile + ' (not a profile now)') : null);
    profSel.addEventListener('change', () => { bm.profile = profSel.value; updateSummary(); });
    const tOpt = (schema.benchmark_options || []).find(x => x.name === 'timeout_s') || {};
    const timeoutForm = bindForm(bm, [Object.assign(toField(tOpt), { label: 'Time limit', description: 'The whole benchmark, load and holds included.' })], () => {}, null);
    const tagsCtl = tagInput(bm.tags || [], v => { if (v.length) bm.tags = v; else delete bm.tags; }, 'Add a tag');

    const about = section('About this benchmark', isNew ? null : 'The id cannot change',
      h('div.cb', h('div.form-grid',
        mk('name', 'Name', nameIn), mk('id', 'Id', idIn, { help: 'Used in suites and in the API.' }),
        mk('description', 'Description', descIn, { wide: true }),
        parallel ? null : mk('profile', 'Profile', profSel, { help: 'The model to benchmark.' }),
        parallel ? null : timeoutForm.el,
        mk('tags', 'Tags', tagsCtl, { plain: true, help: 'Optional, for filtering.' }))));

    summaryEl = h('p', { style: 'font-weight:600;font-size:15px' });
    const summary = section('What it will do', null, h('div.cb', summaryEl, bm.profile && modelOf(bm.profile) ? h('p.mut.mono', { style: 'margin-top:6px' }, modelOf(bm.profile)) : null));
    updateSummary();

    // phases
    let phasesCard;
    if (parallel) phasesCard = parallelCard(topErr);
    else if (simple) phasesCard = section('Phases', null, h('div.cb', banner('info', 'This benchmark uses the simple form (cold load, warm-up, requests, unload). Convert it to phases to edit each step.',
      smallBtn('Convert to phases', () => { convertSimple(); draw(); }))));
    else {
      const list = bm.phases.map((p, i) => phaseBlock(p, i, limits));
      const adders = (schema.phase_types || []).map(t => smallBtn('+ ' + t.title, () => { addPhase(t.type); draw(); }, '', { title: t.description, disabled: bm.phases.length >= (limits.phases || 32) }));
      phasesCard = section('Phases', 'They run in this order', h('div.cb', h('div.col', list,
        inset(h('div', { style: 'font-weight:700' }, 'Add a phase'), h('div.row', { style: 'gap:8px' }, adders))),
        errs.top.phases ? h('p.err-t', errs.top.phases) : null));
    }

    // expectations
    const phTypes = new Set(phasesOf(bm).map(p => p.type));
    const keys = (schema.expect_keys || []).filter(k => !k.needs || phTypes.has(k.needs) || (bm.expect && bm.expect[k.name] !== undefined));
    const exFields = keys.map(k => ({ key: k.name, type: k.name === 'outcome' ? 'enum' : k.type === 'integer' ? 'int' : k.type === 'number' ? 'float' : 'string',
      enum: k.name === 'outcome' ? ['ready', 'refused', 'failed'] : undefined, default: k.name === 'outcome' ? 'ready' : undefined, unit: k.unit, description: k.description,
      label: EXPECT_LABEL[k.name] || k.name }));
    if (!bm.expect) bm.expect = {};
    const exForm = parallel ? null : bindForm(bm.expect, exFields, () => {}, null);
    const expectCard = parallel ? null : section('Expectations', 'The benchmark fails when one of these does not hold. All are optional.', h('div.cb', exForm.el));

    // errors
    const gen = errs.general;
    const errBox = gen.length ? h('div.banner.err', { role: 'alert', style: 'flex-direction:column;align-items:flex-start' }, gen.map(t => h('span', t))) : null;

    const foot = h('div.form-foot', { style: 'justify-content:space-between' },
      h('span.row', { style: 'gap:8px' }, isNew ? null : smallBtn('Delete', onDelete, 'dng')),
      h('span.row', { style: 'gap:8px' }, h('a.btn', { href: '#/benchmarks' }, 'Cancel'),
        h('button', { class: 'btn pri', type: 'button', disabled: saving, onclick: () => save(timeoutForm, exForm, topErr) }, isNew ? 'Create benchmark' : 'Save changes')));

    queueMicrotask(() => {
      for (const k in errs.top) if (topErr[k]) topErr[k](errs.top[k]);
      updateSummary();
    });
    return [errBox, about, summary, phasesCard, expectCard, foot];
  }

  function parallelCard() {
    const members = bm.parallel;
    const all = (cache.benchmarks || []).filter(b => !b.parallel && b.id !== bm.id);
    const sel = h('select.select', { 'aria-label': 'Add a benchmark' }, all.filter(b => !members.includes(b.id)).map(b => h('option', { value: b.id }, `${b.name || b.id} (${b.profile})`)));
    return section('Benchmarks started together', 'Their loads are fired at the same moment (2 to 8, different profiles)',
      h('div.cb', h('div.col', { style: 'gap:8px' },
        members.map((id, i) => h('div.row', { style: 'justify-content:space-between;border:1px solid var(--line);border-radius:10px;padding:8px 12px' },
          h('span', h('b', (benchmarkById(id) || {}).name || id), h('span.mono.mut', '  ' + id)),
          smallBtn('Remove', () => { members.splice(i, 1); draw(); }, 'dng'))),
        h('div.row', { style: 'gap:8px' }, sel, smallBtn('Add', () => { if (sel.value) { members.push(sel.value); draw(); } })))));
  }

  function convertSimple() {
    const ph = [];
    if (bm.cold !== false) ph.push({ type: 'load' });
    const w = bm.warmup_requests == null ? 1 : bm.warmup_requests;
    if (w > 0) ph.push({ type: 'warmup', count: w });
    const req = { type: 'requests', requests: bm.requests || [{ name: 'request-1', prompt: '' }] };
    if (bm.repeats > 1) req.repeats = bm.repeats;
    ph.push(req);
    if (bm.unload_after !== false) ph.push({ type: 'unload' });
    for (const k of ['cold', 'requests', 'repeats', 'warmup_requests', 'unload_after']) delete bm[k];
    bm.phases = ph;
  }

  function addPhase(type) {
    const p = { type };
    if (type === 'requests') p.requests = [{ name: 'request-1', prompt: '' }];
    if (type === 'conversation') p.turns = [{ content: '' }];
    if (type === 'warmup') p.count = 1;
    if (type === 'hold') p.seconds = 60;
    if (type === 'concurrent_requests') p.n = 2;
    bm.phases.push(p);
  }

  // One phase: header with move and remove, the scalar options as a form, and
  // the request and turn lists as repeated blocks.
  function phaseBlock(p, i, limits) {
    const schema = cache.schema;
    const ps = (schema.phase_types || []).find(t => t.type === p.type) || { title: p.type, options: [] };
    const scalar = ps.options.filter(x => OPT_TYPE[x.type]).map(toField);
    const pe = errs.phases[i] || { general: [], fields: {} };
    const form = scalar.length ? bindForm(p, scalar, updateSummary, pe.fields) : null;
    const move = d => { const j = i + d; if (j < 0 || j >= bm.phases.length) return; [bm.phases[i], bm.phases[j]] = [bm.phases[j], bm.phases[i]]; errs = parseErrors(''); draw(); };
    const body = [];
    for (const opt of ps.options) {
      if (opt.type === 'list' && opt.item_type === 'request') body.push(requestList(p, opt, limits));
      else if (opt.type === 'list' && opt.item_type === 'turn') body.push(turnList(p, opt, limits));
      else if (opt.type === 'request') body.push(singleRequest(p, opt));
    }
    return h('div', { style: 'border:1px solid var(--line-2);border-radius:12px;overflow:hidden' },
      h('div.ch', { style: 'background:var(--inset);padding:10px 14px' },
        h('span', { class: 'mono', style: 'color:var(--mut)' }, i + 1), h('h2', { style: 'font-size:15px' }, ps.title),
        h('span.mut.grow', { style: 'font-size:12px;font-weight:600;min-width:200px' }, ps.description),
        h('span.row', { style: 'gap:6px;flex-wrap:nowrap' },
          smallBtn('↑', () => move(-1), '', { 'aria-label': 'Move up', disabled: i === 0 }), smallBtn('↓', () => move(1), '', { 'aria-label': 'Move down', disabled: i === bm.phases.length - 1 }),
          smallBtn('Remove', () => { bm.phases.splice(i, 1); errs = parseErrors(''); draw(); }, 'dng', { 'aria-label': 'Remove phase ' + (i + 1) }))),
      h('div', { style: 'padding:14px;display:flex;flex-direction:column;gap:14px' },
        pe.general.length ? h('div.banner.err', { role: 'alert', style: 'flex-direction:column;align-items:flex-start' }, pe.general.map(t => h('span', t))) : null,
        form ? form.el : null, body));
  }

  const reqFields = () => (cache.schema.request_options || []).filter(x => OPT_TYPE[x.type]).map(toField);

  function requestBox(r, title, remove) {
    const fields = reqFields();
    if (r.messages) fields.push({ key: 'messages', type: 'json', label: 'Messages', description: 'A full chat: a list of {role, content}. Use this or a prompt.' });
    const form = bindForm(r, fields, updateSummary, null);
    return inset(h('div.row', { style: 'justify-content:space-between' }, h('b', title), remove ? smallBtn('Remove', remove, 'dng') : null), form.el);
  }

  function requestList(p, opt, limits) {
    const list = p[opt.name] || (p[opt.name] = []);
    const max = opt.max || limits.requests || 64;
    return h('div.col', { style: 'gap:10px' }, h('div', { style: 'font-weight:700' }, 'Requests', h('span.mut', { style: 'font-weight:500' }, '  ' + (opt.description || ''))),
      list.map((r, k) => requestBox(r, `Request ${k + 1}`, list.length > 1 ? () => { list.splice(k, 1); draw(); } : null)),
      h('div.row', smallBtn('+ Add a request', () => { list.push({ name: `request-${list.length + 1}`, prompt: '' }); draw(); }, '', { disabled: list.length >= max })));
  }

  function turnList(p, opt, limits) {
    const list = p[opt.name] || (p[opt.name] = []);
    const max = opt.max || limits.turns || 64;
    const fields = (cache.schema.turn_options || []).filter(x => OPT_TYPE[x.type]).map(toField);
    return h('div.col', { style: 'gap:10px' }, h('div', { style: 'font-weight:700' }, 'Turns', h('span.mut', { style: 'font-weight:500' }, '  ' + (opt.description || ''))),
      list.map((t, k) => inset(h('div.row', { style: 'justify-content:space-between' }, h('b', `Turn ${k + 1}`), list.length > 1 ? smallBtn('Remove', () => { list.splice(k, 1); draw(); }, 'dng') : null),
        bindForm(t, fields, updateSummary, null).el)),
      h('div.row', smallBtn('+ Add a turn', () => { list.push({ content: '' }); draw(); }, '', { disabled: list.length >= max })));
  }

  function singleRequest(p, opt) {
    const on = !!p[opt.name];
    const sw = toggle(on, v => { if (v) p[opt.name] = { name: 'request-1', prompt: '', max_tokens: 32 }; else delete p[opt.name]; draw(); }, 'Use its own request');
    return h('div.col', { style: 'gap:10px' },
      h('label.check', sw, h('span', { style: 'font-weight:700' }, 'Use its own request'), h('span.mut', { style: 'font-weight:500' }, on ? '' : 'Off: it sends the first request of the requests phase.')),
      on ? requestBox(p[opt.name], 'Request', null) : null);
  }

  // ---- save and delete ----
  function clean() {
    const out = clone(bm);
    for (const k of ['name', 'description']) if (!out[k]) delete out[k];
    if (!out.name) delete out.name;
    if (out.expect && !Object.keys(out.expect).length) delete out.expect;
    const strip = v => {
      if (Array.isArray(v)) return v.map(strip);
      if (v && typeof v === 'object') { for (const k of Object.keys(v)) { if (v[k] === '' && k !== 'content') delete v[k]; else v[k] = strip(v[k]); } }
      return v;
    };
    return strip(out);
  }

  async function save(timeoutForm, exForm, topErr) {
    if (saving) return;
    errs = parseErrors('');
    const local = {};
    if (!bm.id) local.id = 'Give it an id. It is filled in from the name; you can change it.';
    if (!bm.parallel && !bm.profile) local.profile = 'Choose the profile to benchmark.';
    if (Object.keys(local).length) { errs.top = local; draw(); toast('Fix the marked fields first.', 'err'); return; }
    saving = true;
    try {
      const body = clean();
      const saved = isNew ? await api.post('/api/bench/benchmarks', body) : await api.put(`/api/bench/benchmarks/${encodeURIComponent(bm.id)}`, body);
      toast(`Saved ${saved.name || saved.id}.`, 'ok');
      await loadBenchmarks();
      if (o.onDone) o.onDone();
    } catch (e) {
      saving = false;
      errs = parseErrors(errMessages(e));
      if (e.status === 409 || (!errs.general.length && !Object.keys(errs.top).length && !Object.keys(errs.phases).length)) errs.general.push(errMessages(e));
      draw();
      el.scrollIntoView({ block: 'start' });
      toast('It was not saved. The reasons are shown on the page.', 'err');
    }
    saving = false;
  }

  async function onDelete() {
    const ok = await confirm({
      title: `Delete ${bm.name || bm.id}?`,
      intro: 'The benchmark definition is removed. Results of past runs are kept.',
      hits: [{ kind: 'x', text: 'A benchmark that a suite names cannot be deleted: remove it from the suite first.' }],
      confirmLabel: 'Delete benchmark', danger: true,
    });
    if (!ok) return;
    try { await api.del(`/api/bench/benchmarks/${encodeURIComponent(bm.id)}`); toast('Deleted.', 'ok'); await loadBenchmarks(); if (o.onDone) o.onDone(); } catch (e) {
      errs = parseErrors(''); errs.general.push(e.message); draw(); toast('Not deleted: ' + e.message, 'err');
    }
  }

  return { el, destroy() { dead = true; } };
}

// ---------- the suite editor ----------

export function suiteEditor(o) {
  const el = h('div.col', { 'data-keep': '' }, h('section.card', skeleton(4)));
  let dead = false, su = null, runs = [], stage = 'loading', loadError = '', idTouched = false, errs = parseErrors(''), saving = false;
  const isNew = !o.id;
  const draw = () => { if (!dead) el.replaceChildren(...(stage === 'loading' ? [h('section.card', loadError ? empty('This suite could not be opened', loadError) : skeleton(4))] : edit()).filter(Boolean)); };
  let summaryEl = null;

  (async () => {
    try {
      await Promise.all([loadBenchmarks(), loadModelProfiles()]);
      if (!isNew) {
        su = await api.get(`/api/bench/suites/${encodeURIComponent(o.id)}`);
        for (const k of ['source', 'created_at', 'updated_at', 'last_run']) delete su[k];
        try { runs = (await api.get('/api/bench/runs', { suite: o.id, limit: 50 })).runs || []; } catch { runs = []; }
      } else su = { id: '', name: '', benchmarks: [] };
      stage = 'edit';
    } catch (e) { loadError = e.message; }
    draw();
  })();

  const nameOf = it => { const b = typeof it === 'string' ? benchmarkById(it) : it; return b ? (b.name || b.id) : String(it); };
  const updateSummary = () => {
    if (!summaryEl) return;
    const n = su.benchmarks.length;
    const parts = [n ? `Runs ${plural(n, 'benchmark')} in order` : 'No benchmarks yet'];
    parts.push(su.only_when_idle === false ? 'does not wait for an idle scheduler' : `waits for an idle scheduler (up to ${su.idle_wait_s == null ? 600 : su.idle_wait_s} s)`);
    parts.push(`${su.cooldown_s == null ? 5 : su.cooldown_s} s pause between benchmarks`);
    if (su.stop_on_failure) parts.push('stops at the first failure');
    if (su.schedule) parts.push(su.schedule.at ? `runs every day at ${su.schedule.at}` : `runs every ${su.schedule.every}`);
    summaryEl.textContent = parts.join(', ') + '.';
  };

  function edit() {
    const topErr = {};
    const mk = (key, text, control, opts) => { const f = field(text, control, opts); topErr[key] = f.setErr; return f.node; };
    const nameIn = h('input.input', { type: 'text', value: su.name || '', maxlength: 120, placeholder: 'For example: Nightly', autocomplete: 'off' });
    const idIn = h('input.input.mono', { type: 'text', value: su.id || '', maxlength: 64, disabled: !isNew, autocomplete: 'off', spellcheck: 'false' });
    nameIn.addEventListener('input', () => { su.name = nameIn.value; if (isNew && !idTouched) { su.id = slug(nameIn.value); idIn.value = su.id; } });
    idIn.addEventListener('input', () => { idTouched = true; su.id = idIn.value.trim(); });
    const descIn = h('textarea.textarea', { rows: 2, maxlength: 2000, placeholder: 'What this suite is for.' }, su.description || '');
    descIn.addEventListener('input', () => { su.description = descIn.value; });

    const about = section('About this suite', isNew ? null : 'The id cannot change',
      h('div.cb', h('div.form-grid', mk('name', 'Name', nameIn), mk('id', 'Id', idIn, { help: 'Used in the API.' }), mk('description', 'Description', descIn, { wide: true }))));

    // ordered benchmarks
    const all = (cache.benchmarks || []);
    const addSel = h('select.select', { 'aria-label': 'Add a benchmark' }, all.map(b => h('option', { value: b.id }, `${b.name || b.id}${b.profile ? ' · ' + b.profile : ''}`)));
    const mv = (i, d) => { const j = i + d; if (j < 0 || j >= su.benchmarks.length) return; [su.benchmarks[i], su.benchmarks[j]] = [su.benchmarks[j], su.benchmarks[i]]; draw(); };
    const rows = su.benchmarks.map((it, i) => {
      const b = typeof it === 'string' ? benchmarkById(it) : it;
      return h('div', { key: 'sb-' + i, style: 'display:flex;flex-wrap:wrap;align-items:center;gap:8px 12px;border:1px solid var(--line);border-radius:10px;padding:8px 12px' },
        h('span.mono.mut', { style: 'width:22px' }, i + 1),
        h('span.grow', h('b', nameOf(it)), b && b.profile ? h('span.mono.mut', '  ' + b.profile) : null, !b ? h('span.err-t', '  not a saved benchmark') : typeof it !== 'string' ? h('span.mut', '  inline') : null),
        h('span.row', { style: 'gap:6px;flex-wrap:nowrap' }, smallBtn('↑', () => mv(i, -1), '', { 'aria-label': 'Move up', disabled: i === 0 }),
          smallBtn('↓', () => mv(i, 1), '', { 'aria-label': 'Move down', disabled: i === su.benchmarks.length - 1 }),
          smallBtn('Remove', () => { su.benchmarks.splice(i, 1); draw(); }, 'dng', { 'aria-label': 'Remove ' + nameOf(it) })));
    });
    const listCard = section('Benchmarks', 'They run in this order',
      h('div.cb', h('div.col', { style: 'gap:8px' },
        su.status ? h('p.mut', `This suite is ${su.status}: ${su.reason || ''} It is kept as it is when saved and cannot be run. It may list no benchmark yet.`) : null,
        rows.length ? rows : h('p.mut', 'No benchmark yet. Add one below.'),
        errs.top.benchmarks ? h('p.err-t', errs.top.benchmarks) : null,
        all.length ? h('div.row', { style: 'gap:8px' }, addSel, smallBtn('+ Add', () => { if (addSel.value) { su.benchmarks.push(addSel.value); draw(); } })) : h('p.mut', 'There is no benchmark to add. ', h('a', { href: '#/benchmarks/edit/benchmark/new' }, 'Create one first.')))));

    // options
    const vals = { stop_on_failure: !!su.stop_on_failure, only_when_idle: su.only_when_idle !== false, cooldown_s: su.cooldown_s, idle_wait_s: su.idle_wait_s };
    const optForm = bindForm(su, [
      { key: 'only_when_idle', type: 'bool', default: true, label: 'Only when idle', description: 'Wait while models are loaded, requests are running or the queue is not empty. If it stays busy too long the run is skipped.' },
      { key: 'idle_wait_s', type: 'int', unit: 's', min: 0, max: 86400, default: 600, label: 'Wait for idle at most', description: 'How long to wait before the run is skipped.' },
      { key: 'stop_on_failure', type: 'bool', label: 'Stop on failure', description: 'Skip the rest after a benchmark fails.' },
      { key: 'cooldown_s', type: 'int', unit: 's', min: 0, max: 3600, default: 5, label: 'Pause between benchmarks', description: 'Lets the nodes settle.' },
    ], () => { if (su.stop_on_failure === false) delete su.stop_on_failure; updateSummary(); }, null);
    void vals;
    // the form seeds its own values from su: make the bool default show as on
    const schedKind = su.schedule ? (su.schedule.at ? 'at' : 'every') : 'none';
    const schedSel = h('select.select', { 'aria-label': 'Schedule' }, [['none', 'Not scheduled'], ['every', 'Every so often'], ['at', 'Every day at a time']].map(([v, t]) => h('option', { value: v, selected: v === schedKind }, t)));
    const schedIn = h('input.input.mono', { type: 'text', value: su.schedule ? (su.schedule.at || su.schedule.every) : '', placeholder: schedKind === 'at' ? '03:00' : '24h', autocomplete: 'off', 'aria-label': 'Schedule value' });
    const applySched = () => {
      const k = schedSel.value, v = schedIn.value.trim();
      schedIn.disabled = k === 'none'; schedIn.placeholder = k === 'at' ? '03:00 (local time)' : '24h (at least 10m)';
      if (k === 'none') delete su.schedule; else su.schedule = k === 'at' ? { at: v } : { every: v };
      updateSummary();
    };
    schedSel.addEventListener('change', () => { schedIn.value = ''; applySched(); });
    schedIn.addEventListener('input', applySched);
    schedIn.disabled = schedKind === 'none';
    const baseSel = h('select.select', { 'aria-label': 'Baseline' }, h('option', { value: '', selected: !su.baseline }, 'The previous completed run'),
      runs.filter(r => r.state === 'completed').map(r => h('option', { value: r.id, selected: su.baseline === r.id }, `${r.id} · ${dateTime(r.started_at)}`)),
      su.baseline && !runs.some(r => r.id === su.baseline) ? h('option', { value: su.baseline, selected: true }, su.baseline) : null);
    baseSel.addEventListener('change', () => { if (baseSel.value) su.baseline = baseSel.value; else delete su.baseline; });

    const optCard = section('How it runs', null, h('div.cb', h('div.col', optForm.el,
      h('div.form-grid', mk('schedule', 'Schedule', h('div.row', { style: 'gap:8px;flex-wrap:nowrap' }, schedSel, schedIn), { plain: true, help: 'An interval of at least 10m, or a local time of day.' }),
        mk('baseline', 'Compare with', baseSel, { help: 'Each run is compared with this run.' })))));

    summaryEl = h('p', { style: 'font-weight:600;font-size:15px' });
    const summary = section('What it will do', null, h('div.cb', summaryEl));
    queueMicrotask(() => { updateSummary(); for (const k in errs.top) if (topErr[k]) topErr[k](errs.top[k]); });

    const gen = errs.general;
    const errBox = gen.length ? h('div.banner.err', { role: 'alert', style: 'flex-direction:column;align-items:flex-start' }, gen.map(t => h('span', t))) : null;
    const foot = h('div.form-foot', { style: 'justify-content:space-between' },
      h('span.row', { style: 'gap:8px' }, isNew ? null : smallBtn('Delete', onDelete, 'dng')),
      h('span.row', { style: 'gap:8px' }, h('a.btn', { href: '#/benchmarks' }, 'Cancel'),
        h('button', { class: 'btn pri', type: 'button', disabled: saving, onclick: save }, isNew ? 'Create suite' : 'Save changes')));
    return [errBox, about, listCard, optCard, summary, foot];
  }

  async function save() {
    if (saving) return;
    errs = parseErrors('');
    const local = {};
    if (!su.id) local.id = 'Give it an id. It is filled in from the name; you can change it.';
    if (!su.benchmarks.length && !su.status) local.benchmarks = 'Add at least one benchmark.';
    if (Object.keys(local).length) { errs.top = local; draw(); toast('Fix the marked fields first.', 'err'); return; }
    saving = true;
    try {
      const body = clone(su);
      if (!body.name) delete body.name;
      if (!body.description) delete body.description;
      const saved = isNew ? await api.post('/api/bench/suites', body) : await api.put(`/api/bench/suites/${encodeURIComponent(su.id)}`, body);
      toast(`Saved ${suiteTitle(saved)}.`, 'ok');
      await loadSuites();
      if (o.onDone) o.onDone();
    } catch (e) {
      saving = false;
      errs = parseErrors(errMessages(e));
      if (e.status === 409 || (!errs.general.length && !Object.keys(errs.top).length)) errs.general.push(errMessages(e));
      draw(); el.scrollIntoView({ block: 'start' });
      toast('It was not saved. The reasons are shown on the page.', 'err');
    }
    saving = false;
  }

  async function onDelete() {
    const ok = await confirm({
      title: `Delete ${suiteTitle(su)}?`,
      intro: 'The suite is removed together with its schedule. The benchmarks in it and the results of past runs are kept.',
      confirmLabel: 'Delete suite', danger: true,
    });
    if (!ok) return;
    try { await api.del(`/api/bench/suites/${encodeURIComponent(su.id)}`); toast('Deleted.', 'ok'); await loadSuites(); if (o.onDone) o.onDone(); } catch (e) {
      errs = parseErrors(''); errs.general.push(e.message); draw(); toast('Not deleted: ' + e.message, 'err');
    }
  }

  return { el, destroy() { dead = true; } };
}
