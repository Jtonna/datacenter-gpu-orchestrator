// Benchmarks · the editors (board bench-editors/editors.html, five states):
//   1  #/benchmarks/new                    New benchmark: what it starts as
//   2  #/benchmarks/new/<template>         the benchmark editor, from a template
//      #/benchmarks/new/copy?from=<id>     ... a copy of a stored benchmark
//      #/benchmarks/new/empty              ... no phases
//      #/benchmarks/benchmark/<id>/edit    ... a stored benchmark
//   3  the server refuses a save: "Not saved." and its sentences, verbatim;
//      the parts they name (problems[].path) are marked
//   4  #/benchmarks/group/new, #/benchmarks/group/<id>/edit  the group editor,
//      each entry with the dry read Run uses (Fits now / Not startable here)
//   5  "Leave without saving?" and Duplicate (the group's panel)
//
// Both editors are pages: one card, sections, a save bar that stays in view.
// What a phase kind takes comes from GET /api/bench/schema (the server's own
// list of kinds and options); the page adds no rule of its own: Save is off
// only until something changed and the comment has text, and every other
// check is the server's (422 with problems). Every save is one audited
// change carrying its comment (the body's "comment"). "Would it load now"
// is POST /api/plan for the chosen profile: information, never a block.
//
// Kept as stored, not drawn (the board leaves them out): a recipe's inputs,
// a parallel step, a group's matrix and inputs entries (shown, moved and
// removed, not edited), its schedule, baseline and planned/blocked status,
// a request's own sampling settings and a full chat (messages).

import { h, morph } from '../../core/dom.js';
import { href, navigate, setLeaveGuard } from '../../core/router.js';
import * as api from '../../core/api.js';
import { dialog, toast, diffView, skeleton, empty, profileLabel } from '../../core/ui.js';
import { num, plural, ctx as ctxWord } from '../../core/format.js';
import { bench, loadLists, groupById, benchmarkById, groupTitle, benchTitle, entryOf, matrixWords, phasesOf, notYet, WHY } from './common.js';
import { kindIcon } from './list.js';
import { andList, nodeName } from './run.js';

const enc = encodeURIComponent;
const clone = v => (v == null ? v : JSON.parse(JSON.stringify(v)));
const cap = s => (s ? s.charAt(0).toUpperCase() + s.slice(1) : '');

// what a benchmark and a group store (a GET adds source, times, last use ...;
// the strict parser refuses those, so only these go back)
const BENCH_KEYS = ['id', 'name', 'description', 'profile', 'cold', 'requests', 'repeats', 'warmup_requests', 'timeout_s', 'expect', 'unload_after', 'tags', 'phases', 'parallel', 'inputs'];
const GROUP_KEYS = ['id', 'name', 'description', 'benchmarks', 'stop_on_failure', 'cooldown_s', 'only_when_idle', 'idle_wait_s', 'schedule', 'baseline', 'status', 'reason', 'evict_others'];
const pick = (o, keys) => { const out = {}; for (const k of keys) if (o && o[k] !== undefined && o[k] !== null) out[k] = clone(o[k]); return out; };

// ---------- the server's documents, read once per page ----------

const docs = { schema: null, profiles: null, taskSets: null, err: '' };
let docsWait = null;
function loadDocs() {
  if (!docsWait) {
    docsWait = Promise.all([
      api.get('/api/bench/schema'),
      api.get('/api/profiles').catch(() => ({ profiles: [] })),
      api.get('/api/profiles/test').catch(() => ({ profiles: [] })),
      api.get('/api/bench/task-sets').catch(() => null),
    ]).then(([sc, pr, te, ts]) => {
      docs.schema = sc;
      const seen = new Set(), all = [];
      for (const p of (pr.profiles || []).concat(te.profiles || [])) if (p && p.id && !seen.has(p.id)) { seen.add(p.id); all.push(p); }
      docs.profiles = all;
      docs.taskSets = ts ? ts.task_sets || ts.sets || [] : null;
      docs.err = '';
    }, e => { docs.err = e.message; docsWait = null; });
  }
  return docsWait;
}
const phaseSchema = t => ((docs.schema && docs.schema.phase_types) || []).find(p => p.type === t) || null;
const kindTitle = t => (phaseSchema(t) || {}).title || cap(String(t || 'phase').replace(/_/g, ' '));

// ---------- the difference (the audit page's view) ----------

// diffOf(before, after): the changed leaf paths, as the server's audit
// diff writes them (cfgsvc.Diff): a.b, a[2].c; a list of another length is
// one change.
export function diffOf(a, b, path = '', out = []) {
  if (JSON.stringify(a) === JSON.stringify(b)) return out;
  const isObj = v => v && typeof v === 'object' && !Array.isArray(v);
  if (isObj(a) && isObj(b)) {
    for (const k of [...new Set(Object.keys(a).concat(Object.keys(b)))].sort()) diffOf(a[k], b[k], path ? path + '.' + k : k, out);
    return out;
  }
  if (Array.isArray(a) && Array.isArray(b) && a.length === b.length) {
    a.forEach((x, i) => diffOf(x, b[i], `${path}[${i}]`, out));
    return out;
  }
  out.push({ path, old: a === undefined ? null : a, new: b === undefined ? null : b });
  return out;
}

// changedWords(diff, kind) -> "the prompts of phase 3", "entry 4 added"
function changedWords(diff, before, after, kind) {
  const out = [];
  const add = w => { if (!out.includes(w)) out.push(w); };
  const FIELD = { name: 'the name', description: 'the description', id: 'the id', profile: 'the profile', timeout_s: 'the time limit', tags: 'the tags',
    cooldown_s: 'the pause between entries', only_when_idle: 'wait until idle', idle_wait_s: 'how long it waits for idle', stop_on_failure: 'stop at the first failure', evict_others: "unloading other people's idle models" };
  for (const d of diff) {
    let m;
    if ((m = /^phases\[(\d+)\]\.(requests|turns)/.exec(d.path))) add(`the ${m[2] === 'turns' ? 'turns' : 'prompts'} of phase ${+m[1] + 1}`);
    else if ((m = /^phases\[(\d+)\]/.exec(d.path))) add(`phase ${+m[1] + 1}`);
    else if (d.path === 'phases') add('the phases');
    else if (/^expect/.test(d.path)) add('what counts as passing');
    else if (d.path === 'benchmarks' && kind === 'group') {
      const a = (before && before.benchmarks) || [], b = (after && after.benchmarks) || [];
      if (b.length > a.length) add(b.length - a.length === 1 ? `entry ${b.length} added` : `${b.length - a.length} entries added`);
      else add(a.length - b.length === 1 ? 'an entry removed' : `${a.length - b.length} entries removed`);
    } else if (/^benchmarks\[/.test(d.path)) add('the order or the entries');
    else add(FIELD[d.path.split(/[.[]/)[0]] || d.path);
  }
  return out;
}

// ---------- shared pieces ----------

const field = (label, control, help, attrs = {}) => h('div', Object.assign({ class: 'field' }, attrs), h('label', label), control, help ? h('span.help', help) : null);
function check(on, label, note, onToggle, attrs = {}) {
  return h('label', Object.assign({ class: 'check ed-check' }, attrs),
    h('input', { type: 'checkbox', checked: !!on, disabled: !!attrs.disabled, onclick: e => { e.preventDefault(); if (!attrs.disabled) onToggle(!on); } }),
    h('span', h('b', label), note ? [h('br'), h('span.note-s', note)] : null));
}
// a number as typed: a number when it reads as one, else the text itself,
// so that the server's own sentence says what is wrong with it
const numOf = v => { const t = String(v).trim(); if (t === '') return undefined; const n = Number(t); return Number.isFinite(n) ? n : t; };

// fitBadge(kind, text, title)
const BADGE = { fits: 'b-use', waits: 'b-load', no: 'b-aside', unknown: 'b-n', off: 'b-aside' };
const badge = (kind, text, title) => h('span', { class: 'b ' + (BADGE[kind] || 'b-n'), title: title || null, 'data-fit': kind }, text);

// leaveDialog(name, what) -> true to leave
const leaveDialog = (name, what) => dialog({
  title: 'Leave without saving?',
  body: h('p', { 'data-dlg': 'leave' }, 'Your changes to ', h('span', { 'data-name': '' }, name), ' are not saved', what ? ': ' + what + '.' : '.'),
  actions: [{ label: 'Stay', value: false, autofocus: true }, { label: 'Leave', kind: 'dng', value: true }],
}).then(v => v === true);

// keptWords(kind, obj) -> the settings obj has that this page does not draw
// (kept as stored, the reviewer's condition of 26 Nov): ["parallel",
// "schedule", ...] in a fixed order, [] when none
const REQ_OWN = ['temperature', 'seed', 'cache_prompt', 'stream'];
export function keptWords(kind, o) {
  if (!o) return [];
  const out = [];
  if (kind === 'group') {
    const es = (o.benchmarks || []).filter(e => e && typeof e === 'object');
    if (es.some(e => e.matrix)) out.push('matrix entries');
    if (es.some(e => e.inputs)) out.push('entry inputs');
    if (o.schedule) out.push('schedule');
    if (o.baseline) out.push('baseline');
    if (o.status) out.push('status');
    return out;
  }
  if (o.inputs) out.push('inputs');
  if ((o.parallel || []).length) out.push('parallel');
  const reqs = [].concat(o.requests || [], ...(o.phases || []).map(p => [].concat(p.requests || [], p.turns || [])));
  // a phase's single request (a warm-up's): its own stream is not counted
  const single = (o.phases || []).map(p => p.request).filter(Boolean);
  if (reqs.some(q => q && REQ_OWN.some(k => q[k] != null)) || single.some(q => REQ_OWN.some(k => k !== 'stream' && q[k] != null))) out.push("a request's own sampling");
  if (reqs.concat(single).some(q => q && (q.messages || []).length)) out.push('messages');
  return out;
}
const keptLine = words => (words.length ? h('div.ed-kept', { 'data-kept': words.join(',') },
  `This one also has settings this page does not show (${words.join(', ')}): they are kept as they are.`) : null);

// firstLine(refusal): the sentence the bar shows of several: one that names
// no part first (no field shows it), else the first
const firstLine = r => { const i = (r.problems || []).findIndex(p => !p.path); return r.lines[i >= 0 ? i : 0] || ''; };
// saveBar(o): the bar at the foot of the card (sticky). o = {changed: words,
// fresh, refusal: {lines}, saving, onDiff, onCancel, onSave, comment input,
// kept: the settings not drawn (keptWords)}
function saveBar(o) {
  const can = !o.saving && (o.fresh || o.changed.length) && o.comment.value.trim();
  const left = o.refusal
    ? h('div.ed-refused', { role: 'alert', 'data-refused': '', title: o.refusal.lines.length > 1 ? o.refusal.lines.join('\n') : null }, h('b', 'Not saved.'), ' ', o.refusal.lines.length > 1
      // several: the count and one of them, at most two lines; the parts keep their own sentences
      ? h('span', { 'data-line': 0, 'data-count': o.refusal.lines.length }, `${num(o.refusal.lines.length)} problems; the first: `, firstLine(o.refusal))
      : h('span', { 'data-line': 0 }, o.refusal.lines[0] || ''))
    : h('span.note-s.ed-what', { 'data-changed': o.changed.length ? '' : null },
      o.fresh && !o.changed.length ? 'New: nothing is saved yet.' : o.changed.length ? ['Changed: ', andList(o.changed.slice(0, 4)), o.changed.length > 4 ? ` and ${num(o.changed.length - 4)} more` : '', '. ',
        o.fresh ? null : h('button.linkbtn', { type: 'button', 'data-act': 'diff', onclick: o.onDiff }, 'See what changed')] : 'No changes yet.');
  return h('div', { class: 'ed-savebar' + (o.refusal ? ' bad' : ''), key: 'savebar' },
    keptLine(o.kept || []),
    h('div.ed-left', left),
    h('div.ed-acts', o.comment,
      h('button.btn', { type: 'button', 'data-act': 'cancel', onclick: o.onCancel }, 'Cancel'),
      h('button.btn.pri', { type: 'button', 'data-act': 'save', disabled: !can, onclick: o.onSave }, o.saving ? 'Saving…' : 'Save')));
}

// refusalOf(error) -> {lines, problems}: the server's sentences, verbatim
function refusalOf(e) {
  const msg = (e && e.message) || 'the scheduler did not answer';
  const problems = (e && e.body && Array.isArray(e.body.problems)) ? e.body.problems : [];
  const lines = problems.length ? problems.map(placed) :String(msg).split('\n').filter(Boolean);
  return { lines, problems };
}
// placed(problem) -> the server's sentence with the place it names first, in
// the page's words and counted from 1 as the page counts ('phases[2].requests[0]
// (explain): messages or prompt is required' -> 'Phase 3, prompt "explain":
// messages or prompt is required'); the raw sentence when the path is not one
// the page can name
export function placed(p) {
  const path = (p && p.path) || '', msg = (p && p.message) || '';
  if (!path || !msg.startsWith(path)) return msg;
  const rest = /^(?: \(([^)]*)\))?: ?([\s\S]+)$/.exec(msg.slice(path.length));
  if (!rest) return msg;
  const paren = rest[1], tail = rest[2];
  let m, place = '';
  if ((m = /^phases\[(\d+)\]$/.exec(path))) place = `Phase ${+m[1] + 1}`;
  else if ((m = /^phases\[(\d+)\]\.requests\[(\d+)\]$/.exec(path))) place = `Phase ${+m[1] + 1}, prompt ${paren ? `"${paren}"` : +m[2] + 1}`;
  else if ((m = /^phases\[(\d+)\]\.request$/.exec(path))) place = `Phase ${+m[1] + 1}, its request`;
  else if ((m = /^benchmarks\[(\d+)\]$/.exec(path))) place = `Entry ${+m[1] + 1}` + (paren ? ` (${paren})` : '');
  return place ? `${place}: ${tail}` : msg;
}
// sentences(texts) -> one text, each sentence ending in its full stop
const sentences = xs => xs.map(x => String(x || '').trim()).filter(Boolean).map(x => (/[.!?]$/.test(x) ? x : x + '.')).join(' ');
// the sentences that name a part (path, or a part below it), each with its
// place in the page's words as the save bar says it (placed), never the raw
// path ("phases[2].requests[0] (explain): ..."; build-92 s6)
const about = (ref, path) => (ref ? ref.problems.filter(p => p.path === path || (p.path || '').startsWith(path + '.') || (p.path || '').startsWith(path + '[')).map(placed) : []);
const problemNote = lines => (lines.length ? h('div.ed-problem', { 'data-problem': '' }, lines.map((l, i) => h('div', { key: 'p' + i }, l))) : null);

// ---------- the editor shell (both editors) ----------

// editorShell(o): the state every editor shares. o = {kind, name(),
// current(), original, fresh, back, title}
function shell(o) {
  const comment = h('input', { class: 'input ed-comment', type: 'text', maxlength: 500, 'aria-label': 'Comment', placeholder: 'Comment (required): why?', oninput: () => o.redraw() });
  const s = { comment, refusal: null, saving: false };
  s.diff = () => diffOf(o.original || {}, o.current());
  s.changed = () => (o.fresh ? (JSON.stringify(o.original || {}) === JSON.stringify(o.current()) ? [] : changedWords(s.diff(), o.original, o.current(), o.kind)) : changedWords(s.diff(), o.original, o.current(), o.kind));
  s.dirty = () => s.changed().length > 0;
  // a saved page asks nothing on its way out
  s.done = false;
  s.guard = () => setLeaveGuard(!s.done && s.dirty() ? () => leaveDialog(o.name(), andList(s.changed().slice(0, 3))) : null);
  s.showDiff = () => dialog({ title: 'What changed', wide: true, body: h('div', { 'data-dlg': 'diff' }, diffView(s.diff())), actions: [{ label: 'Close', value: true }] });
  return s;
}

// ---------- 1 · New benchmark: what it starts as ----------

// the board's cards, then any other template the server has; a recipe
// template is not offered (its inputs are not on this board)
const BOARD_CARDS = [
  { id: 'cold-reload-loaded', title: 'Load and speed', note: 'Cold load, first generation, steady state, unload, reload, unload. The usual performance run.' },
  { id: 'load-only', title: 'Load only', note: 'Load and unload. For "does it fit" and "is it refused" checks.' },
  { id: 'context-fill-recipe', title: 'Context fill', note: 'Load, fill the context in marks while memory is watched, unload.' },
  { id: 'tasks', title: 'Coding tasks', note: 'Load, run a stored task set and score it, unload.' },
];
const isRecipeTpl = t => { try { return !!JSON.parse(JSON.stringify(t.benchmark)).inputs; } catch { return false; } };

export function startPage(r) {
  let chosen = r.query.start || 'cold-reload-loaded', from = '';
  const el = h('section', { class: 'card ed-card', key: 'ed-start', 'data-keep': '', 'aria-label': 'New benchmark' });
  const draw = () => {
    if (!docs.schema) { morph(el, docs.err ? h('div.banner.err', { role: 'alert' }, docs.err) : skeleton(5)); return; }
    const tpls = docs.schema.templates || [];
    const cards = [];
    for (const c of BOARD_CARDS) {
      const t = tpls.find(x => x.id === c.id);
      if (!t) continue;
      if (isRecipeTpl(t)) cards.push({ id: c.id, title: c.title, note: c.note, off: WHY.recipe });
      else cards.push(c);
    }
    for (const t of tpls) if (!BOARD_CARDS.some(c => c.id === t.id) && !isRecipeTpl(t)) cards.push({ id: t.id, title: t.name, note: t.description });
    cards.push({ id: 'copy', title: 'Copy of an existing one', note: 'Choose a benchmark; its phases and settings are copied under a new name.' });
    cards.push({ id: 'empty', title: 'Empty', note: 'No phases. Add them yourself.' });
    const benches = (bench.benchmarks || []).filter(b => !(b.parallel || []).length);
    if (chosen === 'copy' && !from && benches.length) from = benches[0].id;
    const go = () => {
      if (chosen === 'copy') { if (from) navigate('benchmarks', ['new', 'copy'], { from }); return; }
      navigate('benchmarks', ['new', chosen]);
    };
    morph(el, [
      h('div.ed-sec', h('h2.ed-h', 'New benchmark ', h('span', 'Pick what it starts as. Everything can be changed on the next page.')),
        h('div.ed-tpl', { role: 'radiogroup', 'aria-label': 'Start from' }, cards.map(c => c.off
          ? notYet(h('span', { class: 'ed-card-tpl off', 'data-tpl': c.id }, h('b', c.title), h('span.note-s', c.note)), c.off)
          : h('button', { type: 'button', role: 'radio', key: 't-' + c.id, class: 'ed-card-tpl' + (chosen === c.id ? ' on' : ''), 'aria-checked': chosen === c.id ? 'true' : 'false', 'data-tpl': c.id,
            onclick: () => { chosen = c.id; draw(); } }, h('b', c.title), h('span.note-s', c.note)))),
        chosen === 'copy' ? field('Copy of', h('select', { class: 'select', 'aria-label': 'Copy of', 'data-from': '', onchange: e => { from = e.currentTarget.value; draw(); } },
          benches.map(b => h('option', { value: b.id, selected: b.id === from }, benchTitle(b)))), benches.length ? null : 'There is no benchmark to copy yet.') : null),
      h('div.ed-savebar', h('div.ed-left', h('span.note-s', 'Nothing is saved until you press Save on the next page.')),
        h('div.ed-acts', h('a.btn', { href: href('benchmarks', ['all']), 'data-act': 'cancel' }, 'Cancel'),
          h('button.btn.pri', { type: 'button', 'data-act': 'continue', disabled: chosen === 'copy' && !from, onclick: go }, 'Continue'))),
    ]);
  };
  loadDocs().then(draw);
  loadLists().then(draw);
  draw();
  return { key: 'start', el, redraw: draw, close() {} };
}

// ---------- 2, 3 · the benchmark editor ----------

// a phase's one line when closed: "labelled "cold load"", "2 prompts × 3 repeats"
function phaseLine(p) {
  const bits = [];
  if (p.label) bits.push(`labelled "${p.label}"`);
  if (p.type === 'requests') { const n = (p.requests || []).length; bits.push(`${plural(n, 'prompt')}${p.repeats > 1 ? ` × ${num(p.repeats)} repeats` : ''}`); }
  if (p.type === 'warmup') bits.push(plural(p.count != null ? p.count : 1, 'request'));
  if (p.type === 'conversation') bits.push(plural((p.turns || []).length, 'turn'));
  if (p.type === 'hold') bits.push(p.seconds != null ? `${num(p.seconds)} s` : '');
  if (p.type === 'concurrent_requests') bits.push(p.n != null ? `${num(p.n)} at once` : '');
  if (p.type === 'context_fill') bits.push(p.fill_pct != null ? `to ${num(p.fill_pct)} %` : '');
  if (p.type === 'tasks') bits.push(p.task_set ? 'task set ' + p.task_set : p.tasks ? `${plural((p.tasks.tasks || []).length, 'task')} (inline)` : '');
  if (p.type === 'load' && p.cold) bits.push('cold');
  return bits.filter(Boolean).join(' · ');
}

// the board's words for some options; any other is the schema's name in words
const LABEL = { count: 'Requests', repeats: 'Repeats', stream: 'Stream the answers', cache_prompt: 'Reuse the cached prompt', max_tokens: 'Max tokens', temperature: 'Temperature', seed: 'Seed',
  cold: 'Cold: drop the file caches first', seconds: 'Idle for', n: 'Requests at once', system: 'System message', fill_pct: 'Fill to', marks_pct: 'Readings at', recall: 'Facts to recall',
  stop_available_mib: 'Stop below', refuse_shared_shape: "Refuse a shape a person's profile shares", settle_s: 'Hold after the load', task_set: 'Task set', task_set_version: 'Version', timeout_s: 'Time limit' };
const NOTE = { stream: '(measures time to the first token)' };
const optLabel = o => LABEL[o.name] || cap(o.name.replace(/_/g, ' '));

export function benchEditor(er) {
  const st = { b: null, orig: null, fresh: er.mode !== 'edit', open: -1, err: '', idTouched: er.mode === 'edit', plan: null, planFor: '', planErr: '', note: '' };
  const el = h('section', { class: 'card ed-card', key: 'ed-b-' + er.mode + '-' + (er.id || er.tpl || er.from || ''), 'data-keep': '', 'aria-label': er.mode === 'edit' ? 'Edit benchmark' : 'New benchmark' });
  const sh = shell({ kind: 'benchmark', name: () => (st.b && (st.b.name || st.b.id)) || 'the new benchmark', current: () => st.b, get original() { return st.orig; }, fresh: st.fresh, redraw: () => draw() });
  const back = () => href('benchmarks', ['all']);
  const set = (k, v) => { if (v === undefined || v === '') delete st.b[k]; else st.b[k] = v; draw(); };
  const phases = () => st.b.phases || (st.b.phases = []);

  async function init() {
    await loadDocs();
    if (!docs.schema) { st.err = docs.err || 'the schema could not be read'; draw(); return; }
    try {
      if (er.mode === 'edit') {
        st.b = pick(await api.get('/api/bench/benchmarks/' + enc(er.id)), BENCH_KEYS);
      } else if (er.tpl === 'copy') {
        const src = pick(await api.get('/api/bench/benchmarks/' + enc(er.from)), BENCH_KEYS);
        st.b = Object.assign(src, { id: (src.id + '-copy').slice(0, 64), name: (src.name || src.id) + ' (copy)' });
        st.idTouched = true;
      } else if (er.tpl === 'empty') {
        st.b = { id: '', phases: [] };
      } else {
        const t = (docs.schema.templates || []).find(x => x.id === er.tpl);
        if (!t) { st.err = `There is no template ${er.tpl}.`; draw(); return; }
        st.b = clone(t.benchmark);
        st.b.id = '';
        delete st.b.name;
        if (st.b.profile === 'your-profile') delete st.b.profile;
      }
    } catch (e) { st.err = e.status === 404 ? 'There is no such benchmark.' : e.message; draw(); return; }
    // the short form (cold, warm-up requests, requests, repeats) opens as its phases
    if (!(st.b.phases || []).length && (st.b.requests || []).length && !st.b.inputs && !(st.b.parallel || []).length) {
      st.note = 'Stored in the short form (cold, warm-up requests, requests, repeats); it opens as its phases and is saved in the phases form. The closing Unload then always unloads.';
      const ps = phasesOf(st.b).map(p => Object.assign({}, p));
      for (const p of ps) {
        if (p.type === 'load') { if (st.b.cold === false) p.cold = false; else delete p.cold; }
        if (p.type === 'warmup') { p.count = st.b.warmup_requests != null ? st.b.warmup_requests : 1; }
        if (p.type === 'requests') { p.requests = st.b.requests; if (st.b.repeats) p.repeats = st.b.repeats; }
      }
      if (st.b.cold === false) ps.shift();
      st.b.phases = ps.filter(p => !(p.type === 'warmup' && !p.count));
      for (const k of ['cold', 'requests', 'repeats', 'warmup_requests', 'unload_after']) delete st.b[k];
    }
    st.orig = clone(st.b);
    draw();
    readPlan();
  }

  // "Would it load now": the plan of the chosen profile, as it is now
  async function readPlan() {
    const pid = st.b && st.b.profile;
    if (!pid || pid === st.planFor) return;
    st.planFor = pid; st.plan = null; st.planErr = '';
    draw();
    try { const p = await api.post('/api/plan', { model: pid }); if (st.planFor === pid) st.plan = p; } catch (e) { if (st.planFor === pid) st.planErr = e.message; }
    draw();
  }
  function planBox() {
    const pid = st.b.profile;
    const kids = [h('span.lbl', 'Would it load now')];
    if (!pid) kids.push(h('span.mut', 'Choose a profile to see its plan.'));
    else if (st.planErr) kids.push(h('span.mut', 'The plan could not be read: ' + st.planErr));
    else if (!st.plan) kids.push(h('span.mut', 'Reading the plan…'));
    else {
      const p = st.plan, mem = p.memory || null;
      const nodes = (mem && mem.nodes) || [];
      const names = nodes.length ? nodes.map(n => n.node || nodeName(n.id)) : (p.placement || []).map(n => nodeName(n.id));
      const takes = nodes.map(n => n.will_take_mib).filter(v => v > 0);
      const each = takes.length ? (takes.every(v => v === takes[0]) ? `${num(takes[0])} MiB${takes.length > 1 ? ' each' : ''}` : `${num(Math.min(...takes))}–${num(Math.max(...takes))} MiB`) : '';
      const on = names.length ? [' on ', andList(names), each ? [' · ', h('span.mono', each)] : null] : null;
      let line;
      if (p.excluded_by) line = [badge('off', 'Not startable here'), ` it would load on ${p.excluded_by}, which is excluded as a zone`];
      else if (p.action === 'start') line = [badge('fits', 'Fits now'), on];
      else if (p.action === 'serve' || p.action === 'wait') line = [badge('fits', 'Loaded now'), on];
      else if (p.action === 'evict') line = [badge('waits', 'Fits after an unload'), on, (p.victims || []).length ? ` · unloads ${andList(p.victims.map(v => v.profile || v.id || ''))}` : ''];
      else if (p.action === 'queue') line = [badge('waits', 'Waits'), ' ', p.reason || 'no zone can take it now'];
      else if (p.action === 'backoff') line = [badge('no', 'Held back'), ' ', p.reason || 'its last load failed'];
      else line = [badge('no', 'Would not fit'), ' ', p.reason || 'no zones can hold it'];
      kids.push(h('span.ed-planline', { 'data-plan': p.action || '' }, line));
      const host = (p.placement || []).find(n => n.role === 'host');
      // the zone's name: the plan's own name for that node, else the zones' names; the id only when there is none
      const hostName = host ? (nodes.find(n => n.id === host.id && n.node) || {}).node || nodeName(host.id) : '';
      const need = p.fit_need_mib || p.need_mib;
      kids.push(h('span.note-s', need ? `Need ${num(need)} MiB, ${p.need_basis || (p.measured ? 'measured' : 'estimated')}.` : 'Need not known.', host ? ` Host on ${hostName}.` : ''));
    }
    return h('div.ed-what', { 'data-would-load': '' }, kids);
  }

  // one option of a phase, drawn from the schema
  function optField(p, o, ref) {
    const v = p[o.name];
    const setV = x => { if (x === undefined || x === '') delete p[o.name]; else p[o.name] = x; draw(); };
    const bad = about(ref, ref && ref.at ? ref.at + '.' + o.name : '').length > 0;
    if (o.type === 'boolean') return check(v != null ? v : !!o.default, optLabel(o), NOTE[o.name] || null, x => setV(x), { 'data-opt': o.name, title: o.description });
    if (o.type === 'enum') return field(optLabel(o), h('select', { class: 'select', 'data-opt': o.name, onchange: e => setV(e.currentTarget.value || undefined) },
      h('option', { value: '', selected: v == null }, o.default != null ? `Default (${o.default})` : 'Not set'), (o.values || []).map(x => h('option', { value: String(x), selected: v === x }, String(x)))));
    if (o.type === 'integer' || o.type === 'number') {
      return field(optLabel(o), h('div.ed-num', h('input', { class: 'input mono', type: 'text', inputmode: o.type === 'integer' ? 'numeric' : 'decimal', 'data-opt': o.name, 'aria-label': optLabel(o),
        'aria-invalid': bad ? 'true' : null, value: v == null ? '' : String(v), placeholder: o.default != null ? String(o.default) : '', title: o.description, oninput: e => setV(numOf(e.currentTarget.value)) }),
        o.unit ? h('span.mut', o.unit) : null));
    }
    if (o.type === 'list' && o.item_type === 'integer') {
      return field(optLabel(o), h('div.ed-num', h('input', { class: 'input mono', type: 'text', 'data-opt': o.name, 'aria-label': optLabel(o), value: (v || []).join(', '), placeholder: 'for example 25, 50, 75', title: o.description,
        oninput: e => { const xs = e.currentTarget.value.split(/[\s,]+/).filter(Boolean).map(numOf); setV(xs.length ? xs : undefined); } }), o.unit ? h('span.mut', o.unit) : null));
    }
    if (o.type === 'text') return field(optLabel(o), h('textarea', { class: 'textarea', rows: 2, 'data-opt': o.name, 'aria-label': optLabel(o), value: v || '', title: o.description, oninput: e => setV(e.currentTarget.value) }));
    if (o.name === 'task_set' && docs.taskSets && docs.taskSets.length) {
      return field(optLabel(o), h('select', { class: 'select', 'data-opt': o.name, onchange: e => setV(e.currentTarget.value || undefined) },
        h('option', { value: '', selected: !v }, p.tasks ? 'None: the set written in the benchmark' : 'Choose a task set'),
        docs.taskSets.map(t => h('option', { value: t.id, selected: v === t.id }, (t.title || t.id) + (t.version ? ` · version ${t.version}` : '')))));
    }
    return field(optLabel(o), h('input', { class: 'input', type: 'text', 'data-opt': o.name, 'aria-label': optLabel(o), value: v || '', title: o.description, oninput: e => setV(e.currentTarget.value) }));
  }

  // the prompts of a Requests phase (name, prompt, max tokens); a full chat
  // (messages) is kept as written
  function promptsTable(p, item) {
    const rows = p.requests || (p.requests = []);
    return h('div.ed-prompts', { 'data-prompts': '' },
      h('div.lbl', 'Prompts · ', num(rows.length)),
      h('div.ed-ptable', { role: 'table', 'aria-label': 'Prompts' },
        h('div.ed-prow.head', { role: 'row' }, h('span', { role: 'columnheader' }, 'Name'), h('span', { role: 'columnheader' }, 'Prompt'), h('span.r', { role: 'columnheader' }, 'Max tokens'), h('span')),
        rows.map((q, i) => h('div.ed-prow', { key: 'q' + i, role: 'row', 'data-prompt': i },
          h('input', { class: 'input', type: 'text', 'aria-label': 'Name', value: q.name || '', placeholder: `request-${i + 1}`, oninput: e => { q.name = e.currentTarget.value || undefined; if (!q.name) delete q.name; draw(); } }),
          q.messages ? h('span.mut.ed-chat', `A chat of ${plural(q.messages.length, 'message')}, kept as written`)
            : h('textarea', { class: 'textarea', rows: 2, 'aria-label': 'Prompt', value: q.prompt || '', oninput: e => { q.prompt = e.currentTarget.value; draw(); } }),
          h('input', { class: 'input mono r', type: 'text', inputmode: 'numeric', 'aria-label': 'Max tokens', value: q.max_tokens == null ? '' : String(q.max_tokens), placeholder: p.max_tokens != null ? String(p.max_tokens) : String((item || {}).defaultMax || ''),
            oninput: e => { const x = numOf(e.currentTarget.value); if (x === undefined) delete q.max_tokens; else q.max_tokens = x; draw(); } }),
          h('button.btn.sm.ghost.dngtext', { type: 'button', 'data-act': 'remove-prompt', onclick: () => { rows.splice(i, 1); draw(); } }, 'Remove')))),
      h('div.row', h('button.btn.sm', { type: 'button', 'data-act': 'add-prompt', onclick: () => { rows.push({ prompt: '' }); draw(); } }, 'Add a prompt'),
        h('span.note-s', `${plural(rows.length * (p.repeats || 1), 'request')} in all: ${plural(rows.length, 'prompt')} × ${plural(p.repeats || 1, 'repeat')}.`)));
  }
  function turnsTable(p) {
    const rows = p.turns || (p.turns = []);
    return h('div.ed-prompts', { 'data-turns': '' },
      h('div.lbl', 'Turns · ', num(rows.length)),
      h('div.ed-ptable', { role: 'table', 'aria-label': 'Turns' },
        h('div.ed-prow.head', { role: 'row' }, h('span', 'Name'), h('span', 'What the user says'), h('span.r', 'Max tokens'), h('span')),
        rows.map((q, i) => h('div.ed-prow', { key: 't' + i, role: 'row', 'data-turn': i },
          h('input', { class: 'input', type: 'text', 'aria-label': 'Name', value: q.name || '', placeholder: `turn-${i + 1}`, oninput: e => { q.name = e.currentTarget.value || undefined; if (!q.name) delete q.name; draw(); } }),
          h('textarea', { class: 'textarea', rows: 2, 'aria-label': 'What the user says', value: q.content || '', oninput: e => { q.content = e.currentTarget.value; draw(); } }),
          h('input', { class: 'input mono r', type: 'text', inputmode: 'numeric', 'aria-label': 'Max tokens', value: q.max_tokens == null ? '' : String(q.max_tokens), oninput: e => { const x = numOf(e.currentTarget.value); if (x === undefined) delete q.max_tokens; else q.max_tokens = x; draw(); } }),
          h('button.btn.sm.ghost.dngtext', { type: 'button', onclick: () => { rows.splice(i, 1); draw(); } }, 'Remove')))),
      h('div.row', h('button.btn.sm', { type: 'button', 'data-act': 'add-turn', onclick: () => { rows.push({ content: '' }); draw(); } }, 'Add a turn')));
  }
  // one request (warm-up, hold, simultaneous): its own, or the server's default
  function requestBlock(p) {
    const own = !!p.request;
    return h('div.ed-req', { 'data-request': '' },
      check(own, 'Its own request', own ? null : 'Off: it sends the first prompt of the Requests phase.', on => { if (on) p.request = { prompt: '' }; else delete p.request; draw(); }, { 'data-opt': 'request' }),
      own ? h('div.ed-grid3', field('Name', h('input', { class: 'input', type: 'text', value: p.request.name || '', oninput: e => { p.request.name = e.currentTarget.value || undefined; if (!p.request.name) delete p.request.name; draw(); } })),
        field('Prompt', h('textarea', { class: 'textarea', rows: 2, value: p.request.prompt || '', oninput: e => { p.request.prompt = e.currentTarget.value; draw(); } }), null, { class: 'field ed-wide' }),
        field('Max tokens', h('input', { class: 'input mono', type: 'text', inputmode: 'numeric', value: p.request.max_tokens == null ? '' : String(p.request.max_tokens),
          oninput: e => { const x = numOf(e.currentTarget.value); if (x === undefined) delete p.request.max_tokens; else p.request.max_tokens = x; draw(); } }))) : null);
  }

  function phaseRow(p, i, list, ref) {
    const probs = about(ref, `phases[${i}]`);
    const tools = h('div.tools',
      st.open === i ? null : h('button.btn.sm.ghost', { type: 'button', 'data-act': 'edit-phase', onclick: () => { st.open = i; draw(); } }, 'Edit'),
      h('button.btn.sm.ghost', { type: 'button', 'data-act': 'up', 'aria-label': 'Move up', title: 'Move up', disabled: i === 0, onclick: () => { list.splice(i - 1, 0, list.splice(i, 1)[0]); if (st.open === i) st.open = i - 1; draw(); } }, '↑'),
      h('button.btn.sm.ghost', { type: 'button', 'data-act': 'down', 'aria-label': 'Move down', title: 'Move down', disabled: i === list.length - 1, onclick: () => { list.splice(i + 1, 0, list.splice(i, 1)[0]); if (st.open === i) st.open = i + 1; draw(); } }, '↓'),
      h('button.btn.sm.ghost.dngtext', { type: 'button', 'data-act': 'remove-phase', onclick: () => { list.splice(i, 1); st.open = st.open === i ? -1 : st.open > i ? st.open - 1 : st.open; draw(); } }, 'Remove'));
    const cls = 'ed-phase' + (st.open === i ? ' open' : '') + (probs.length ? ' bad' : '');
    if (st.open !== i) {
      const line = phaseLine(p);
      return h('div', { class: cls, key: 'ph' + i, 'data-phase': i, 'data-kind': p.type },
        h('span.n', String(i + 1)),
        h('div.body', h('div.row', h('b', kindTitle(p.type)), line ? h('span.mut', line) : null), problemNote(probs)),
        tools);
    }
    const sc = phaseSchema(p.type);
    const opts = (sc ? sc.options : []).filter(o => o.name !== 'label');
    const headNum = opts.find(o => (o.type === 'integer') && ['repeats', 'count', 'seconds', 'n', 'fill_pct'].includes(o.name));
    const simple = opts.filter(o => o !== headNum && ['integer', 'number', 'string', 'enum', 'text'].includes(o.type) || (o.type === 'list' && o.item_type === 'integer'));
    const bools = opts.filter(o => o.type === 'boolean');
    const pref = { at: `phases[${i}]`, problems: ref ? ref.problems : [] };
    return h('div', { class: cls, key: 'ph' + i, 'data-phase': i, 'data-kind': p.type, 'data-open': '' },
      h('span.n', String(i + 1)),
      h('div.body',
        h('div.ed-grid3',
          field('Kind', h('select', { class: 'select', 'data-opt': 'type', 'aria-label': 'Kind', onchange: e => { const t = e.currentTarget.value; list[i] = p.label ? { type: t, label: p.label } : { type: t }; draw(); } },
            ((docs.schema && docs.schema.phase_types) || []).map(k => h('option', { value: k.type, selected: k.type === p.type }, k.title)))),
          field('Label', h('input', { class: 'input', type: 'text', 'data-opt': 'label', 'aria-label': 'Label', value: p.label || '', placeholder: sc ? sc.title : '', oninput: e => { if (e.currentTarget.value) p.label = e.currentTarget.value; else delete p.label; draw(); } })),
          headNum ? optField(p, headNum, pref) : null),
        sc && sc.description ? h('p.note-s.ed-kind-note', sc.description) : null,
        bools.length ? h('div.ed-checks', bools.map(o => optField(p, o, pref))) : null,
        simple.length ? h('div.ed-grid3', simple.map(o => optField(p, o, pref))) : null,
        opts.some(o => o.type === 'list' && o.item_type === 'request') ? promptsTable(p, { defaultMax: (opts.find(o => o.name === 'max_tokens') || {}).default }) : null,
        opts.some(o => o.type === 'list' && o.item_type === 'turn') ? turnsTable(p) : null,
        opts.some(o => o.type === 'request') ? requestBlock(p) : null,
        opts.some(o => o.name === 'tasks') && p.tasks ? h('p.note-s', { 'data-inline-tasks': '' }, `A task set written in the benchmark: ${p.tasks.title || p.tasks.id || 'no title'} · ${plural((p.tasks.tasks || []).length, 'task')}, kept as written.`) : null,
        problemNote(probs),
        h('div.row.ed-done', h('button.btn.sm', { type: 'button', 'data-act': 'done-phase', onclick: () => { st.open = -1; draw(); } }, 'Done'))),
      tools);
  }

  async function save() {
    if (!sh.comment.value.trim() || sh.saving) return;
    sh.saving = true; sh.refusal = null; draw();
    const body = Object.assign({}, st.b, { comment: sh.comment.value.trim() });
    try {
      if (st.fresh) await api.post('/api/bench/benchmarks', body);
      else await api.put('/api/bench/benchmarks/' + enc(er.id), body);
      sh.saving = false; sh.done = true;
      setLeaveGuard(null);
      toast(`Saved ${st.b.name || st.b.id}.`, 'ok');
      await loadLists();
      navigate('benchmarks', ['all'], { saved: st.b.id });
    } catch (e) {
      sh.saving = false;
      sh.refusal = refusalOf(e);
      draw();
    }
  }

  function draw() {
    if (st.err) { morph(el, h('div.ed-sec', empty('This benchmark cannot be edited', st.err, h('a.btn', { href: back() }, 'Back to the list')))); setLeaveGuard(null); return; }
    if (!st.b) { morph(el, h('div.ed-sec', skeleton(8))); return; }
    if ((st.b.parallel || []).length) {
      morph(el, h('div.ed-sec', empty('A parallel step', 'It starts the benchmarks ' + andList(st.b.parallel) + ' at the same moment. A parallel step has no editor on this board; it is kept as stored.', h('a.btn', { href: back() }, 'Back to the list'))));
      return;
    }
    const ref = sh.refusal;
    const list = phases();
    const sc = docs.schema;
    const limitMin = sc && sc.limits && sc.limits.timeout_s ? Math.round(sc.limits.timeout_s / 60) : null;
    const defTimeout = ((sc && sc.benchmark_options) || []).find(o => o.name === 'timeout_s');
    const exp = st.b.expect || {};
    const keys = ((sc && sc.expect_keys) || []).filter(k => k.name !== 'outcome' && (!k.needs || list.some(p => p.type === k.needs) || exp[k.name] != null));
    const fieldBad = path => about(ref, path);
    const recipe = !!st.b.inputs;
    const profiles = docs.profiles || [];
    const pOpt = p => h('option', { value: p.id, selected: p.id === st.b.profile },
      [profileLabel(p), p.model_name, p.ctx ? ctxWord(p.ctx) : '', (p.force_nodes || []).length ? `forced on ${plural(p.force_nodes.length, 'zone')}` : ''].filter(Boolean).join(' · '));
    sh.guard();
    morph(el, [
      st.note ? h('div.banner.warn.ed-note', { role: 'status', key: 'note' }, st.note) : null,
      h('div.ed-sec', { key: 's-what', 'data-sec': 'what' },
        h('h2.ed-h', er.mode === 'edit' ? ['Edit ', h('span', { 'data-name': '' }, st.orig.name || st.orig.id)] : 'New benchmark'),
        h('h3', 'What it is'),
        h('div.ed-grid2',
          field('Name', h('input', { class: 'input', type: 'text', name: 'name', 'aria-label': 'Name', 'aria-invalid': fieldBad('name').length ? 'true' : null, value: st.b.name || '',
            oninput: e => { const v = e.currentTarget.value; if (v) st.b.name = v; else delete st.b.name; if (!st.idTouched) st.b.id = slug(v); draw(); } }), null),
          field(['Id ', h('span.mut.ed-sub', '(used in addresses; cannot change after saving)')], h('input', { class: 'input mono', type: 'text', name: 'id', 'aria-label': 'Id', disabled: !st.fresh,
            'aria-invalid': fieldBad('id').length ? 'true' : null, value: st.b.id || '', oninput: e => { st.idTouched = true; st.b.id = e.currentTarget.value; draw(); } }), null)),
        problemNote(fieldBad('id').concat(fieldBad('name'))),
        field('Description', h('textarea', { class: 'textarea', name: 'description', rows: 2, 'aria-label': 'Description', value: st.b.description || '', oninput: e => set('description', e.currentTarget.value) })),
        field('Tags', h('input', { class: 'input', type: 'text', name: 'tags', 'aria-label': 'Tags', value: (st.b.tags || []).join(', '), placeholder: 'Free words for filtering, separated by commas',
          oninput: e => { const t = e.currentTarget.value.split(',').map(x => x.trim()).filter(Boolean); set('tags', t.length ? t : undefined); } }))),
      h('div.ed-sec', { key: 's-load', 'data-sec': 'profile' },
        h('h3', 'What it loads ', h('span', recipe ? 'A recipe: its inputs are kept as stored.' : 'One profile. The run uses it exactly as it is stored.')),
        recipe ? h('p.note-s', { 'data-recipe': '' }, 'A recipe takes its model, context and placement as inputs when it runs (stored: ', Object.entries(st.b.inputs).map(([k, v]) => `${k} ${v}`).join(', '), '). Editing the inputs waits for its own board.')
          : h('div.ed-grid2',
            field('Profile', h('select', { class: 'select', name: 'profile', 'aria-label': 'Profile', 'aria-invalid': fieldBad('profile').length ? 'true' : null,
              onchange: e => { set('profile', e.currentTarget.value || undefined); readPlan(); } },
              h('option', { value: '', selected: !st.b.profile }, 'Choose a profile'),
              st.b.profile && !profiles.some(p => p.id === st.b.profile) ? h('option', { value: st.b.profile, selected: true }, st.b.profile + ' · not a profile now') : null,
              profiles.map(pOpt)), 'Production and test profiles are both listed; test ones show as TEST.'),
            planBox()),
        problemNote(fieldBad('profile').concat(fieldBad('inputs')))),
      h('div.ed-sec', { key: 's-phases', 'data-sec': 'phases' },
        h('h3', 'Phases ', h('span', `${num(list.length)} · run in this order`)),
        list.length ? h('div.ed-list', { role: 'list', 'aria-label': 'Phases' }, list.map((p, i) => phaseRow(p, i, list, ref))) : h('p.mut', 'No phases yet.'),
        problemNote(about(ref, 'phases').filter(l => !list.some((_, i) => about(ref, `phases[${i}]`).includes(l)))),
        h('div.row', h('button.btn.sm', { type: 'button', 'data-act': 'add-phase', onclick: () => { list.push({ type: 'requests', requests: [{ prompt: '' }] }); st.open = list.length - 1; draw(); } }, 'Add a phase'),
          h('span.note-s', 'One phase is open at a time; the others show as one line.'))),
      h('div.ed-sec', { key: 's-pass', 'data-sec': 'expect', class: 'ed-sec' + (fieldBad('expect').length || fieldBad('timeout_s').length ? ' bad' : '') },
        h('h3', 'What counts as passing'),
        h('div.ed-grid2',
          field('Expected outcome', h('select', { class: 'select', 'data-opt': 'outcome', 'aria-label': 'Expected outcome', onchange: e => { const v = e.currentTarget.value; const x = Object.assign({}, st.b.expect || {}); if (v === 'ready') delete x.outcome; else x.outcome = v; set('expect', Object.keys(x).length ? x : undefined); } },
            h('option', { value: 'ready', selected: !exp.outcome || exp.outcome === 'ready' }, 'It runs to the end'),
            h('option', { value: 'refused', selected: exp.outcome === 'refused' }, 'The load is refused (a "must not fit" check)'),
            h('option', { value: 'failed', selected: exp.outcome === 'failed' }, 'The load fails'),
            exp.outcome && !['ready', 'refused', 'failed'].includes(exp.outcome) ? h('option', { value: exp.outcome, selected: true }, exp.outcome) : null)),
          field('Give up after', h('div.ed-num', h('input', { class: 'input mono', type: 'text', inputmode: 'decimal', 'data-opt': 'timeout_s', 'aria-label': 'Give up after, minutes', style: 'width:80px',
            value: st.b.timeout_s != null ? String(typeof st.b.timeout_s === 'number' ? Math.round(st.b.timeout_s / 6) / 10 : st.b.timeout_s) : '',
            placeholder: defTimeout && defTimeout.default ? String(Math.round(defTimeout.default / 60)) : '', oninput: e => { const x = numOf(e.currentTarget.value); set('timeout_s', typeof x === 'number' ? Math.round(x * 60) : x); } }),
            h('span.mut', 'minutes', limitMin ? ` · the cluster's limit is ${num(limitMin)}` : '')))),
        keys.length ? [h('div.lbl', 'Checks on the figures ', h('span.mut', '(optional)')), h('div.ed-grid3', keys.map(k => field(cap(k.description.replace(/\.$/, '')),
          h('div.ed-num', h('input', { class: 'input mono', type: 'text', 'data-expect': k.name, 'aria-label': k.name, value: exp[k.name] == null ? '' : String(exp[k.name]),
            oninput: e => { const x = Object.assign({}, st.b.expect || {}); const v = k.type === 'string' ? e.currentTarget.value.trim() || undefined : numOf(e.currentTarget.value); if (v === undefined) delete x[k.name]; else x[k.name] = v; set('expect', Object.keys(x).length ? x : undefined); } }),
            k.unit ? h('span.mut', k.unit) : null))))] : null,
        problemNote(fieldBad('expect').concat(fieldBad('timeout_s')))),
      saveBar({ kept: st.fresh ? [] : keptWords('benchmark', st.b), changed: sh.changed(), fresh: st.fresh, refusal: sh.refusal, saving: sh.saving, comment: sh.comment, onDiff: sh.showDiff, onSave: save,
        onCancel: () => navigate('benchmarks', ['all']) }),
    ]);
  }
  init();
  draw();
  return { key: el.__key, el, redraw: draw, close() { setLeaveGuard(null); } };
}

// an id from a name: lower-case letters, digits, '-' (the server's rule allows '.', '_' too)
const slug = s => String(s || '').toLowerCase().normalize('NFKD').replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 64);

// ---------- 4 · the group editor ----------

// fitOf(kind, id): the dry read Run uses, per entry (cached for the page)
const fitCache = new Map();
function fitOf(kind, id, after) {
  const k = kind + ':' + id;
  if (fitCache.has(k)) return fitCache.get(k);
  const v = { state: 'reading' };
  fitCache.set(k, v);
  api.send('POST', (kind === 'group' ? '/api/bench/groups/' : '/api/bench/benchmarks/') + enc(id) + '/run', {}, { params: { dry_run: 1 } })
    .then(plan => {
      const rs = plan.refusals || [];
      if (rs.length) { v.state = 'off'; v.why = sentences(rs.map(r => r.message || r.msg)); return; }
      const fits = (plan.loads || []).map(l => l.fits || 'unknown');
      v.state = !fits.length ? 'unknown' : fits.every(f => f === 'fits') ? 'fits' : fits.includes('no') ? 'no' : fits.includes('waits') ? 'waits' : 'unknown';
      v.why = (plan.loads || []).map(l => l.reason).filter(Boolean)[0] || '';
    }, e => {
      const rs = e.body && e.body.refusals;
      if (rs && rs.length) { v.state = 'off'; v.why = sentences(rs.map(r => r.message || r.msg)); } else { v.state = 'error'; v.why = e.message; }
    }).then(after);
  return v;
}
export const clearFits = () => fitCache.clear();
const FIT_WORD = { fits: 'Fits now', waits: 'Waits: in use', no: 'Would not fit', unknown: 'Need not known', off: 'Not startable here' };

export function groupEditor(er) {
  const st = { g: null, orig: null, fresh: er.mode !== 'edit', err: '', idTouched: er.mode === 'edit', q: '' };
  const el = h('section', { class: 'card ed-card', key: 'ed-g-' + er.mode + '-' + (er.id || ''), 'data-keep': '', 'aria-label': er.mode === 'edit' ? 'Edit group' : 'New group' });
  const sh = shell({ kind: 'group', name: () => (st.g && (st.g.name || st.g.id)) || 'the new group', current: () => st.g, get original() { return st.orig; }, fresh: st.fresh, redraw: () => draw() });
  const back = () => (st.fresh ? href('benchmarks', ['all']) : href('benchmarks', ['all', er.id]));
  const set = (k, v) => { if (v === undefined || v === '') delete st.g[k]; else st.g[k] = v; draw(); };

  async function init() {
    await Promise.all([loadDocs(), loadLists()]);
    try {
      st.g = er.mode === 'edit' ? pick(await api.get('/api/bench/groups/' + enc(er.id)), GROUP_KEYS) : { id: '', benchmarks: [] };
      if (!st.g.benchmarks) st.g.benchmarks = [];
    } catch (e) { st.err = e.status === 404 ? 'There is no such group.' : e.message; }
    st.orig = clone(st.g);
    draw();
  }
  const sOpt = name => ((docs.schema && docs.schema.suite_options) || []).find(o => o.name === name) || {};

  // what an entry is, in one line
  function entryRow(raw, i, list, ref) {
    const e = entryOf(raw);
    const probs = about(ref, `benchmarks[${i}]`);
    let title, sub = null, fit = null;
    if (e.kind === 'group') {
      const g = groupById(e.ref);
      title = [kindIcon(true), ' ', h('b', groupTitle(g) || e.ref)];
      sub = h('span.mut', g && g.expanded_count != null ? `group · ${plural(g.expanded_count, 'benchmark')}` : 'group');
      if (e.ref !== (st.g && st.g.id)) fit = fitOf('group', e.ref, draw);
    } else if (e.kind === 'inline') {
      title = [kindIcon(false), ' ', h('b', benchTitle(e.inline) || e.ref || 'a benchmark written in the group')];
      sub = h('span.mut', 'written in the group, kept as written');
    } else {
      const b = benchmarkById(e.ref);
      const mw = matrixWords(e);
      title = [kindIcon(false), ' ', h('b', benchTitle(b) || e.ref), mw.length ? [' ', mw.map(w => [h('span.mut', ' × '), w])] : null];
      if (e.inputs || e.matrix) sub = h('span.mut', 'a recipe with ' + (e.matrix ? 'a matrix' : 'inputs') + ', kept as written');
      else if (b && b.inputs) sub = h('span.mut', 'a recipe: its inputs are asked for when it runs');
      else if (b && b.profile) { const p = (docs.profiles || []).find(x => x.id === b.profile); sub = h('span.mono.mut', p ? profileLabel(p) : b.profile); }
      if (b && !b.inputs && !e.inputs && !e.matrix) fit = fitOf('benchmark', e.ref, draw);
      if (!b && bench.benchmarks) sub = h('span.mut', 'not a stored benchmark');
    }
    const fitEl = !fit ? null : fit.state === 'reading' ? h('span.mut.ed-fit', 'checking…') : fit.state === 'error' ? h('span.mut.ed-fit', { title: fit.why }, 'not checked')
      : badge(fit.state, FIT_WORD[fit.state], fit.why || null);
    return h('div', { class: 'ed-phase' + (probs.length ? ' bad' : ''), key: 'e' + i + ':' + JSON.stringify(raw), role: 'listitem', 'data-entry': i, 'data-fits': fit ? fit.state : null },
      h('span.n', String(i + 1)),
      h('div.body', h('div.row', h('span.ed-etitle', title), sub, fitEl),
        fit && fit.state === 'off' ? h('span.note-s.ed-off', { 'data-off': '' }, sentences([fit.why, 'A group with this entry cannot be started from Potluck.'])) : null,
        problemNote(probs)),
      h('div.tools',
        h('button.btn.sm.ghost', { type: 'button', 'data-act': 'up', 'aria-label': 'Move up', title: 'Move up', disabled: i === 0, onclick: () => { list.splice(i - 1, 0, list.splice(i, 1)[0]); draw(); } }, '↑'),
        h('button.btn.sm.ghost', { type: 'button', 'data-act': 'down', 'aria-label': 'Move down', title: 'Move down', disabled: i === list.length - 1, onclick: () => { list.splice(i + 1, 0, list.splice(i, 1)[0]); draw(); } }, '↓'),
        h('button.btn.sm.ghost.dngtext', { type: 'button', 'data-act': 'remove-entry', onclick: () => { list.splice(i, 1); draw(); } }, 'Remove')));
  }
  // counts: benchmarks and steps, a group within counted in place
  function counts(items, depth = 0, seen = new Set()) {
    let b = 0, s = 0;
    for (const raw of items || []) {
      const e = entryOf(raw);
      if (e.kind === 'group') {
        const g = groupById(e.ref);
        if (!g || depth > 6 || seen.has(e.ref)) continue;
        const c = counts(g.benchmarks, depth + 1, new Set(seen).add(e.ref));
        b += c.b; s += c.s;
      } else {
        const def = e.kind === 'inline' ? e.inline : benchmarkById(e.ref);
        const n = e.matrix ? Object.values(e.matrix).reduce((m, v) => m * (Array.isArray(v) && v.length ? v.length : 1), 1) : 1;
        b += n; s += n * phasesOf(def).length;
      }
    }
    return { b, s };
  }
  function adder(list) {
    const q = st.q.trim().toLowerCase();
    const hits = !q ? [] : [].concat(
      (bench.benchmarks || []).filter(b => [b.id, b.name].some(t => String(t || '').toLowerCase().includes(q))).map(b => ({ kind: 'benchmark', id: b.id, title: benchTitle(b) })),
      (bench.groups || []).filter(g => g.id !== st.g.id && [g.id, g.name].some(t => String(t || '').toLowerCase().includes(q))).map(g => ({ kind: 'group', id: g.id, title: groupTitle(g) }))).slice(0, 8);
    return h('div.ed-add',
      h('input', { class: 'input ed-search', type: 'search', 'aria-label': 'Add a benchmark or a group', placeholder: 'Add a benchmark or a group: type to search', value: st.q, 'data-add': '',
        oninput: e => { st.q = e.currentTarget.value; draw(); } }),
      hits.length ? h('div.ed-hits', { role: 'listbox', 'aria-label': 'Matches' }, hits.map(x => h('button', { type: 'button', role: 'option', class: 'ed-hit', key: x.kind + x.id, 'data-add-id': x.id,
        onclick: () => { list.push(x.kind === 'group' ? { group: x.id } : x.id); st.q = ''; draw(); } }, kindIcon(x.kind === 'group'), ' ', x.title, h('span.mut.mono', ' ' + x.id))))
        : q ? h('p.mut', 'Nothing matches.') : null);
  }

  async function save() {
    if (!sh.comment.value.trim() || sh.saving) return;
    sh.saving = true; sh.refusal = null; draw();
    const body = Object.assign({}, st.g, { comment: sh.comment.value.trim() });
    try {
      if (st.fresh) await api.post('/api/bench/groups', body);
      else await api.put('/api/bench/groups/' + enc(er.id), body);
      sh.saving = false; sh.done = true;
      setLeaveGuard(null);
      toast(`Saved ${st.g.name || st.g.id}.`, 'ok');
      clearFits();
      await loadLists();
      navigate('benchmarks', ['all', st.g.id], { saved: st.g.id });
    } catch (e) {
      sh.saving = false;
      sh.refusal = refusalOf(e);
      draw();
    }
  }

  function draw() {
    if (st.err) { morph(el, h('div.ed-sec', empty('This group cannot be edited', st.err, h('a.btn', { href: href('benchmarks', ['all']) }, 'Back to the list')))); setLeaveGuard(null); return; }
    if (!st.g) { morph(el, h('div.ed-sec', skeleton(8))); return; }
    const ref = sh.refusal;
    const list = st.g.benchmarks;
    const c = counts(list);
    const fieldBad = path => about(ref, path);
    const anyOff = list.some((raw, i) => { const e = entryOf(raw); const f = fitCache.get((e.kind === 'group' ? 'group:' : 'benchmark:') + e.ref); return f && f.state === 'off'; });
    const idle = st.g.only_when_idle != null ? st.g.only_when_idle : sOpt('only_when_idle').default !== false;
    sh.guard();
    morph(el, [
      h('div.ed-sec', { key: 's-what', 'data-sec': 'what' },
        h('h2.ed-h', er.mode === 'edit' ? ['Edit group ', h('span', { 'data-name': '' }, st.orig.name || st.orig.id)] : 'New group'),
        h('h3', 'What it is'),
        h('div.ed-grid2',
          field('Name', h('input', { class: 'input', type: 'text', name: 'name', 'aria-label': 'Name', value: st.g.name || '', 'aria-invalid': fieldBad('name').length ? 'true' : null,
            oninput: e => { const v = e.currentTarget.value; if (v) st.g.name = v; else delete st.g.name; if (!st.idTouched) st.g.id = slug(v); draw(); } })),
          field(['Id ', h('span.mut.ed-sub', '(cannot change after saving)')], h('input', { class: 'input mono', type: 'text', name: 'id', 'aria-label': 'Id', disabled: !st.fresh, value: st.g.id || '',
            'aria-invalid': fieldBad('id').length ? 'true' : null, oninput: e => { st.idTouched = true; st.g.id = e.currentTarget.value; draw(); } }))),
        problemNote(fieldBad('id').concat(fieldBad('name'))),
        field('Description', h('textarea', { class: 'textarea', name: 'description', rows: 2, 'aria-label': 'Description', value: st.g.description || '', oninput: e => set('description', e.currentTarget.value) })),
        st.g.status ? h('p.note-s', { 'data-status': '' }, `This group is ${st.g.status}: ${st.g.reason || 'no reason given'}. Kept as stored.`) : null),
      h('div.ed-sec', { key: 's-runs', 'data-sec': 'entries' },
        h('h3', 'What it runs ', h('span', `${plural(list.length, 'entry', 'entries')} · in this order`)),
        list.length ? h('div.ed-list', { role: 'list', 'aria-label': 'Entries' }, list.map((raw, i) => entryRow(raw, i, list, ref))) : h('p.mut', 'No entries yet.'),
        problemNote(fieldBad('benchmarks').filter(l => !list.some((_, i) => about(ref, `benchmarks[${i}]`).includes(l)))),
        h('div.row.ed-addrow', adder(list), h('span.note-s', 'A group inside a group runs its entries in place. ', `${plural(c.b, 'benchmark')}, ${plural(c.s, 'step')} in all.`)),
        anyOff ? h('p.note-s.ed-off', { 'data-cannot-start': '' }, 'An entry is not startable here: the group can be saved, but it cannot be started from Potluck while that entry is in it.') : null),
      h('div.ed-sec', { key: 's-how', 'data-sec': 'how' },
        h('h3', 'How it runs'),
        h('div.ed-grid2',
          check(idle, 'Wait until idle', 'Start only when nothing is loaded, no request runs and the queue is empty.', on => set('only_when_idle', on), { 'data-opt': 'only_when_idle' }),
          check(!!st.g.stop_on_failure, 'Stop at the first failure', 'Off: it carries on and the failed entry shows in the result.', on => set('stop_on_failure', on || undefined), { 'data-opt': 'stop_on_failure' }),
          check(!!st.g.evict_others, "Unload other people's idle models", 'Off: it only uses memory that is free. The run dialog names what would be unloaded.', on => set('evict_others', on || undefined), { 'data-opt': 'evict_others' }),
          field('Pause between entries', h('div.ed-num', h('input', { class: 'input mono', type: 'text', inputmode: 'numeric', 'data-opt': 'cooldown_s', 'aria-label': 'Pause between entries, seconds', style: 'width:80px',
            value: st.g.cooldown_s == null ? '' : String(st.g.cooldown_s), placeholder: sOpt('cooldown_s').default != null ? String(sOpt('cooldown_s').default) : '', oninput: e => set('cooldown_s', numOf(e.currentTarget.value)) }), h('span.mut', 'seconds'))),
          field('Wait for idle at most', h('div.ed-num', h('input', { class: 'input mono', type: 'text', inputmode: 'numeric', 'data-opt': 'idle_wait_s', 'aria-label': 'Wait for idle at most, seconds', style: 'width:80px', disabled: !idle,
            value: st.g.idle_wait_s == null ? '' : String(st.g.idle_wait_s), placeholder: sOpt('idle_wait_s').default != null ? String(sOpt('idle_wait_s').default) : '', oninput: e => set('idle_wait_s', numOf(e.currentTarget.value)) }),
            h('span.mut', 'seconds · then the run is skipped')))),
        notYet(h('span.check.ed-check.off', h('input', { type: 'checkbox', disabled: true }), h('span', h('b', 'Cold loads'), h('span.mut', ' · not available yet'))), WHY.b7),
        problemNote(['cooldown_s', 'idle_wait_s', 'only_when_idle', 'stop_on_failure', 'evict_others', 'schedule', 'baseline', 'status', 'reason'].flatMap(fieldBad))),
      saveBar({ kept: st.fresh ? [] : keptWords('group', st.g), changed: sh.changed(), fresh: st.fresh, refusal: sh.refusal, saving: sh.saving, comment: sh.comment, onDiff: sh.showDiff, onSave: save,
        onCancel: () => { location.hash = back(); } }),
    ]);
  }
  init();
  draw();
  return { key: el.__key, el, redraw: draw, close() { setLeaveGuard(null); } };
}

// ---------- 5 · Duplicate a group ----------

export async function duplicateGroup(g) {
  if (!g) return;
  const nameIn = h('input', { class: 'input', type: 'text', name: 'name', 'aria-label': 'Name of the copy', value: (g.name || g.id) + ' (copy)' });
  const idIn = h('input', { class: 'input mono', type: 'text', name: 'id', 'aria-label': 'Id', value: (g.id + '-copy').slice(0, 64) });
  const v = await dialog({
    title: h('span', 'Duplicate ', h('span', { 'data-name': '' }, g.name || g.id)),
    body: h('div.dcol', { 'data-dlg': 'duplicate' },
      h('div.field', h('label', 'Name of the copy'), nameIn),
      h('div.field', h('label', 'Id'), idIn, h('span.help', "Lower-case letters, digits, '.', '-' and '_'; up to 64 characters. It cannot change later."))),
    comment: { label: 'Comment', required: true, placeholder: 'Why? It is kept in the audit log.' },
    actions: [{ label: 'Cancel', value: undefined }, { label: 'Create the copy', kind: 'pri', value: () => ({ id: idIn.value.trim(), name: nameIn.value.trim() }) }],
  });
  if (!v || !v.value) return;
  const copy = Object.assign(pick(g, GROUP_KEYS), { id: v.value.id, name: v.value.name || undefined, comment: v.comment });
  if (!copy.name) delete copy.name;
  try {
    await api.post('/api/bench/groups', copy);
    toast('Created ' + (copy.name || copy.id) + '.', 'ok');
    await loadLists();
    navigate('benchmarks', ['all', copy.id]);
  } catch (e) { toast('Not created. ' + e.message, 'err'); }
}

// ---------- the routes ----------

// editorRoute(route) -> what to draw, or null (not an editor's address)
export function editorRoute(r) {
  const [a, b, c] = r.rest;
  if (a === 'new' && !b) return { kind: 'start', key: 'start' };
  if (a === 'new' && b) return { kind: 'bench', mode: 'new', tpl: b, from: r.query.from || '', key: 'bn:' + b + ':' + (r.query.from || '') };
  if (a === 'benchmark' && b && c === 'edit') return { kind: 'bench', mode: 'edit', id: b, key: 'be:' + b };
  if (a === 'group' && b === 'new') return { kind: 'group', mode: 'new', key: 'gn' };
  if (a === 'group' && b && c === 'edit') return { kind: 'group', mode: 'edit', id: b, key: 'ge:' + b };
  return null;
}
export function openEditor(er, r) {
  const ed = er.kind === 'start' ? startPage(r) : er.kind === 'bench' ? benchEditor(er) : groupEditor(er);
  ed.key = er.key;
  return ed;
}
export const editLink = (kind, id) => href('benchmarks', kind === 'group' ? ['group', id, 'edit'] : ['benchmark', id, 'edit']);
