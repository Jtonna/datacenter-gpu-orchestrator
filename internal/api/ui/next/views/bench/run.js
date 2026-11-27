// Run and Cancel from the Benchmarks pages (board bench-run/run-cancel.html,
// seven states). One run dialog, opened from "Run a benchmark", a row's Run,
// "Run group" and "Run again":
//   1, 2  what will run, the loads (machines by name, what each takes there,
//         whether it fits now), Wait until idle, Unload other people's idle
//         models with what that would unload now, a note, a required comment;
//   3     the server refuses the run: its own sentences, only Close;
//   4     the confirm (the write): the loads and machines, what it unloads.
// Every figure is the server's: POST .../run?dry_run=1 (the start's body)
// answers steps, loads, loaded_now, would_unload, refusals, warnings. The
// start carries the comment and the would_unload list it showed (evict); a
// changed list is refused (409 evict_set_changed) and the dialog shows the
// new one. Nothing is sent before Start but dry reads.
//
// The cancel dialog (5) says where the run is and what a cancel does, asks
// the comment and posts POST /api/bench/runs/<id>/cancel {comment}.
//
// The server counts a benchmark run of a group as one step ("step 7 of 30"
// in its progress and in a cancelled run's outcome); a benchmark's own load,
// warm-up, requests and unload are its steps too ("1 benchmark · 4 steps").

import { h } from '../../core/dom.js';
import { navigate } from '../../core/router.js';
import * as api from '../../core/api.js';
import { state } from '../../core/store.js';
import { dialog, toast, profileLabel, labelOf } from '../../core/ui.js';
import { num, dur, plural } from '../../core/format.js';
import { bench, groupById, benchmarkById, groupTitle, benchTitle, chain, phasesOf } from './common.js';

const enc = encodeURIComponent;
const runPath = (kind, id) => (kind === 'group' ? `/api/bench/groups/${enc(id)}/run` : `/api/bench/benchmarks/${enc(id)}/run`);
const cap = s => (s ? s.charAt(0).toUpperCase() + s.slice(1) : '');
// "a", "a and b", "a, b and c"
export const andList = xs => (xs.length < 2 ? xs.join('') : xs.slice(0, -1).join(', ') + ' and ' + xs[xs.length - 1]);
export const ordinal = n => n + (n % 100 >= 11 && n % 100 <= 13 ? 'th' : ['th', 'st', 'nd', 'rd'][n % 10] || 'th');
const plabel = (id, shown) => (shown ? profileLabel({ id, shown_name: shown }) : labelOf(id));

// isRecipe(benchmark): a recipe asks for its inputs before it runs; that
// dialog is not drawn yet, so its Run stays greyed (board: "Not on this board").
export const isRecipe = b => !!(b && b.inputs);

// nodeName(id): the machine's name from the status, else the id.
export function nodeName(id) {
  const n = ((state.status && state.status.view && state.status.view.nodes) || []).find(x => x.id === id || x.hostname === id);
  return (n && n.hostname) || id;
}

// The queue position of a queued run (1 = next), from the bench state.
export function queuePos(id) {
  const q = (state.bench.state && state.bench.state.queue) || [];
  const i = q.findIndex(r => r.id === id);
  return i >= 0 ? i + 1 : 0;
}
export const queuedCount = () => ((state.bench.state && state.bench.state.queue) || []).length;

// ---------- the run dialog ----------

const FITS = { fits: ['b-use', 'Fits now'], waits: ['b-load', 'Waits: in use'], no: ['b-aside', 'Would not fit'], unknown: ['b-n', 'Need not known'] };

function takesWords(l) {
  const ms = l.machines || [];
  const t = ms.map(m => m.takes_mib).filter(v => v > 0);
  if (!l.need_known && !t.length) return h('span.mut', 'need not known');
  if (t.length && t.length === ms.length) {
    if (t.length === 1) return num(t[0]) + ' MiB';
    if (t.every(v => v === t[0])) return num(t[0]) + ' MiB each';
    return num(Math.min(...t)) + '–' + num(Math.max(...t)) + ' MiB';
  }
  return l.need_mib ? num(l.need_mib) + ' MiB' : h('span.mut', 'need not known');
}
function onWords(l) {
  const names = (l.machines || []).map(m => m.name || m.id);
  if (!names.length) return h('span.mut', 'placed when it loads');
  if (names.length <= 2) return names.join(', ');
  return h('span', { title: names.join(', ') }, `${num(names.length)} machines`);
}
function loadRow(l) {
  const [cls, word] = FITS[l.fits] || FITS.unknown;
  return h('tr', { key: 'l-' + l.profile, 'data-load': l.profile, 'data-fits': l.fits || 'unknown' },
    h('td', h('span.mono', plabel(l.profile, l.profile_name)), l.model_name || l.model ? h('span.sub', l.model_name || l.model) : null),
    h('td', onWords(l)),
    h('td.r', takesWords(l)),
    h('td', h('span', { class: 'b ' + cls, title: l.reason || null }, word)));
}

// unloadItem(m): "gemma-4-12b on node-a (idle for 3 min)"
function unloadItem(m) {
  const where = (m.machines || []).length ? ' on ' + andList(m.machines) : '';
  const how = m.in_flight > 0 ? ' (answering a request now: unloaded when it ends)' : m.idle_s != null ? ` (idle for ${dur(m.idle_s)})` : '';
  return [h('b.mono', plabel(m.profile, m.profile_name)), where, how];
}
const unloadList = ms => ms.map((m, i) => [i ? (i === ms.length - 1 ? ' and ' : ', ') : null, unloadItem(m)]);

function loadedLine(ms) {
  return ms.map((m, i) => [i ? '; ' : null, h('span.mono', plabel(m.profile, m.profile_name)),
    (m.machines || []).length ? ' on ' + andList(m.machines) : '',
    m.in_flight > 0 ? `, answering ${plural(m.in_flight, 'request')}` : m.idle_s != null ? `, last used ${dur(m.idle_s)} ago` : '']);
}

// runDialog({kind: 'group'|'benchmark', id, againOf, pick}): the one dialog.
// pick: opened from "Run a benchmark", it asks what to run first.
export function runDialog(o = {}) {
  let st = o.pick ? 'pick' : 'checking';
  let kind = o.kind || 'group', id = o.id || '';
  let plan = null, msg = '', changed = '', showAll = false, closed = false, closeFn = null, wait = null, evict = null, refusals = [];
  const titleEl = h('span');
  const root = h('div.dcol.rd-body');
  const note = h('textarea', { class: 'input textarea rd-note', rows: 1, maxlength: 500, 'aria-label': 'Note', placeholder: 'What are you checking?' });
  const comment = h('textarea', { class: 'input textarea', rows: 2, maxlength: 500, 'aria-label': 'Comment', placeholder: 'Why? It is kept in the audit log.' });
  const review = h('button', { class: 'btn lg pri', type: 'button', 'data-go': 'review', onclick: () => { if (comment.value.trim()) { st = 'confirm'; draw(); } } }, 'Review and start');
  const gate = () => { if (comment.value.trim() && st === 'form') review.removeAttribute('disabled'); else review.setAttribute('disabled', ''); };
  comment.addEventListener('input', gate);
  const closeBtn = label => h('button', { class: 'btn lg', type: 'button', 'data-close': '', onclick: () => closeFn(undefined) }, label);
  const name = () => (kind === 'group' ? groupTitle(groupById(id)) : benchTitle(benchmarkById(id))) || (plan && plan.name) || id;
  const setTitle = () => {
    if (st === 'confirm' || st === 'sending') titleEl.replaceChildren('Start this run?');
    else if (st === 'pick') titleEl.replaceChildren('Run a benchmark');
    // a benchmark's or group's name is prose: the title's own face (mono is for ids and file names)
    else titleEl.replaceChildren(kind === 'group' ? 'Run group ' : 'Run ', h('span', { 'data-name': '' }, name()));
  };

  const body = () => {
    const b = { only_when_idle: wait == null ? undefined : wait, evict_others: evict == null ? undefined : evict };
    if (o.againOf) b.again_of = o.againOf;
    return b;
  };
  async function dry() {
    st = 'checking'; draw();
    try {
      plan = await api.send('POST', runPath(kind, id), body(), { params: { dry_run: 1 } });
      if (closed) return;
      wait = plan.waits_for_idle; evict = plan.evict_others;
      refusals = plan.refusals || [];
      st = refusals.length ? 'refused' : 'form';
    } catch (e) {
      if (closed) return;
      const rs = e.body && e.body.refusals;
      if (rs && rs.length) { refusals = rs; st = 'refused'; } else { msg = e.message; st = 'error'; }
    }
    draw();
  }
  async function start() {
    if (st !== 'confirm' || !comment.value.trim()) return;
    const b = Object.assign(body(), { only_when_idle: plan.waits_for_idle, evict_others: plan.evict_others, comment: comment.value.trim(),
      evict: (plan.would_unload || []).map(m => m.profile) });
    if (note.value.trim()) b.note = note.value.trim();
    st = 'sending'; msg = ''; draw();
    try {
      const run = await api.post(runPath(kind, id), b, { comment: b.comment });
      if (closed) return;
      closeFn(true);
      toast(run && run.state === 'queued' && queuedCount() ? 'Queued: ' + name() : 'Started: ' + name(), 'ok');
      if (run && (run.number || run.id)) navigate('benchmarks', ['runs', String(run.number || run.id)]);
    } catch (e) {
      if (closed) return;
      const err = (e.body && e.body.error) || {};
      if (err.reason === 'evict_set_changed') {
        changed = e.message;
        await dry();
        return;
      }
      const rs = e.body && e.body.refusals;
      if (rs && rs.length) { refusals = rs; st = 'refused'; draw(); return; }
      msg = e.message; st = 'confirm'; draw();
    }
  }

  // ---- the parts ----
  const what = () => {
    const parts = [];
    if (kind === 'benchmark') {
      const bm = benchmarkById(id), n = phasesOf(bm).length;
      parts.push(h('span', h('b', '1 benchmark', n ? ' · ' + plural(n, 'step') : ''), bm ? ' · ' + chain(bm) : ''));
    } else {
      const g = groupById(id);
      parts.push(h('span', h('b', plural(plan.step_count, 'benchmark')), plan.step_count > 1 ? ', one after the other' : '',
        g && g.cooldown_s && plan.step_count > 1 ? `, ${dur(g.cooldown_s)} between them` : ''));
    }
    const lim = plan.max_run_minutes ? ['At most ', h('span.mono', num(plan.max_run_minutes) + ' min'), ' (the setting ', h('span.mono', 'bench_max_run_minutes'), ').'] : [];
    const g = kind === 'group' ? groupById(id) : null;
    const fail = g && plan.step_count > 1 ? (g.stop_on_failure ? ' It stops at the first failure.' : ' It carries on after a failure.') : '';
    if (lim.length || fail) parts.push(h('span.note-s', lim, fail));
    return h('div.dbox.ok.rd-what', { 'data-what': '' }, h('span.lbl', 'What will run'), parts);
  };
  const loads = () => {
    const ls = plan.loads || [];
    if (!ls.length) return h('p.note-s', { 'data-loads': '0' }, 'This run loads nothing.');
    const shown = showAll || ls.length <= 4 ? ls : ls.slice(0, 3);
    return h('div', { 'data-loads': String(ls.length) },
      h('div.lbl.rd-lbl', 'Loads this run makes', ls.length > 1 ? ' · ' + num(ls.length) : ''),
      h('table.tbl.rd-loads', h('thead', h('tr', h('th', 'Profile'), h('th', 'On'), h('th.r', 'Takes there'), h('th', 'Now'))),
        h('tbody', shown.map(loadRow), shown.length < ls.length ? h('tr', { key: 'more' }, h('td', { colspan: 4 },
          h('button.rd-more', { type: 'button', 'data-go': 'all-loads', onclick: () => { showAll = true; draw(); } }, `Show the other ${num(ls.length - shown.length)}`))) : null)));
  };
  const options = () => {
    const now = plan.loaded_now || [], q = plan.queue || {}, wu = plan.would_unload || [];
    const waitHelp = !wait ? 'Off: it starts at once, even while other models are loaded or answering.'
      : !now.length && !q.queued && !q.running ? 'Nothing is loaded and nothing is queued: it starts at once.'
        : [now.length ? ['Loaded now: ', loadedLine(now), '. '] : null, 'The run waits until nothing is loaded, no request runs and the queue is empty.'];
    const box = (key, on, label, help) => h('label.check.rd-check', { 'data-opt': key },
      h('input', { type: 'checkbox', checked: on, 'aria-label': label, onclick() { if (key === 'wait') wait = !wait; else evict = !evict; changed = ''; dry(); } }),
      h('span', h('b', label), help ? [h('br'), h('span.note-s', help)] : null));
    return h('div.rd-opts',
      box('wait', !!wait, 'Wait until idle', waitHelp),
      box('evict', !!evict, "Unload other people's idle models", evict ? (wu.length ? null : 'Nothing would be unloaded now.') : (wu.length ? null : 'Off: this run only uses memory that is free.')),
      changed ? h('div.banner.warn.rd-changed', { role: 'status', 'data-changed': '' }, h('span', h('b', 'What is loaded changed. '), cap(changed))) : null,
      wu.length ? h('div.rd-sub', h('div.banner.warn', { 'data-would-unload': String(wu.length) },
        h('span', 'This would unload now: ', unloadList(wu), '. A model that is answering a request is never cut off.'))) : null,
      q.running || q.queued ? h('p.note-s', { 'data-queue': String(q.queued || 0) }, q.running ? 'Another run is going' : 'Runs are queued',
        q.queued ? `: this one waits behind ${plural(q.queued, 'queued run')}.` : ': this one waits until it ends.') : null);
  };
  const warnings = () => (plan.warnings || []).map(w => h('div.banner.warn', { 'data-warning': w.code }, h('span', cap(w.message), '.')));
  const field = (label, extra, el) => h('div.field', h('label.dlbl', label, extra ? h('span.mut', ' ', extra) : null), el);

  const draw = () => {
    if (closed) return;
    const d = root.parentNode;
    if (d && d.setAttribute) {
      d.setAttribute('data-dlg', 'run'); d.setAttribute('data-state', st);
      d.classList.toggle('wide', !['confirm', 'sending'].includes(st));
    }
    setTitle();
    let kids;
    if (st === 'pick') {
      const opts = [];
      for (const g of bench.groups || []) if (!g.not_startable) opts.push(h('option', { value: 'group:' + g.id }, 'Group · ' + groupTitle(g)));
      for (const b of bench.benchmarks || []) if (!b.not_startable && !isRecipe(b)) opts.push(h('option', { value: 'benchmark:' + b.id }, benchTitle(b)));
      const sel = h('select.select', { 'aria-label': 'What to run', 'data-pick': '' }, opts);
      kids = [opts.length ? field('What to run', '', sel) : h('p.mut', 'There is nothing that can be started from here.'),
        h('div.dlg-foot', closeBtn('Cancel'), opts.length ? h('button', { class: 'btn lg pri', type: 'button', 'data-go': 'pick', onclick: () => {
          const v = sel.value, i = v.indexOf(':');
          kind = v.slice(0, i); id = v.slice(i + 1); dry();
        } }, 'Continue') : null)];
    } else if (st === 'checking') {
      kids = [h('p.mut', { role: 'status' }, 'Asking the scheduler what this run would do now…'), h('div.dlg-foot', closeBtn('Cancel'))];
    } else if (st === 'error') {
      kids = [h('div.banner.err', { role: 'alert' }, h('span', h('b', 'The run could not be checked. '), msg)),
        h('div.dlg-foot', h('button', { class: 'btn lg', type: 'button', 'data-go': 'again', onclick: dry }, 'Check again'), closeBtn('Close'))];
    } else if (st === 'refused') {
      kids = [h('div.banner.err', { role: 'alert', 'data-refused': refusals.map(r => r.code).join(' ') },
        h('div.rd-refusals', refusals.map((r, i) => h('p', { key: 'r' + i, 'data-refusal': r.code },
          h('b', r.code === 'queue_full' ? 'Not started now.' : 'Not startable here.'), ' ', cap(r.message || r.msg || ''), /[.!?]$/.test(r.message || '') ? '' : '.')))),
      h('div.dlg-foot', closeBtn('Close'))];
    } else if (st === 'form') {
      kids = [what(), loads(), options(), warnings(),
        field('Note', '(optional, kept with the run)', note),
        field('Comment (required)', '', comment),
        h('div.dlg-foot', closeBtn('Cancel'), review)];
    } else {
      const ls = plan.loads || [], wu = plan.would_unload || [];
      const machines = [];
      for (const l of ls) for (const m of l.machines || []) { const n = m.name || m.id; if (!machines.includes(n)) machines.push(n); }
      const start = h('button', { class: 'btn lg pri', type: 'button', 'data-go': 'start', disabled: st === 'sending' ? '' : null, onclick: start_ }, st === 'sending' ? 'Starting…' : 'Start run');
      kids = [h('p', { 'data-confirm': String(ls.length) }, 'This starts ', h('b', plural(ls.length, 'load')),
        machines.length ? [' on ', h('b', andList(machines))] : null, ls.length > 1 ? ', one after the other' : '', '.',
        plan.max_run_minutes ? ` ${num(plan.max_run_minutes)} minutes at most.` : '',
        plan.waits_for_idle ? ' It waits until the fleet is idle first.' : ''),
      wu.length ? h('div.banner.warn', { 'data-unloads': String(wu.length) }, h('span', 'It unloads ', unloadList(wu), ' first.')) : null,
      h('p.note-s', 'If what is loaded changes before you press Start, the run is not started and this dialog shows the new list.'),
      msg ? h('div.banner.err', { role: 'alert' }, h('span', h('b', 'Not started. '), msg)) : null,
      h('div.dlg-foot', h('button', { class: 'btn lg', type: 'button', 'data-go': 'back', onclick: () => { st = 'form'; msg = ''; draw(); } }, 'Back'), start)];
    }
    root.replaceChildren(...kids.flat(3).filter(Boolean));
    gate();
  };
  const start_ = () => start();

  dialog({ title: titleEl, wide: true, body: close => { closeFn = close; return root; } }).then(() => { closed = true; });
  if (st === 'pick') draw(); else dry();
}

// runButton(kind, id, label, cls, againOf): the live Run control.
export function runButton(kind, id, label = 'Run', cls = 'btn sm', againOf = '') {
  return h('button', { class: cls, type: 'button', 'data-act': againOf ? 'run-again' : 'run', 'data-run-kind': kind, 'data-run-id': id,
    onclick: e => { e.preventDefault(); e.stopPropagation(); runDialog({ kind, id, againOf }); } }, label);
}

// notStartable(reason): the line said instead of Run (board state 3's note),
// on the entry's own line under its name: in the Run slot it widened the
// row's last column and pushed the row's figures out of line.
export const notStartable = why => h('span.bp-nostart', { title: 'Not startable here: ' + why, 'data-not-startable': '' }, 'Not startable here: ', why);

// ---------- the cancel dialog ----------

const PHASE_ING = { load: 'loading', warmup: 'warming up', requests: 'sending the requests of', unload: 'unloading', hold: 'holding', tasks: 'running the tasks of',
  conversation: 'holding a conversation with', concurrent_requests: 'sending requests at once to', context_fill: 'filling the context of' };

// stepNow(run) -> {at, total, doing}: "7", "30", "loading TEST - x on node-a
// and node-b" (the progress document's current benchmark and phase).
export function stepNow(run) {
  const cur = run.current || null;
  const total = run.steps_total || 0;
  const at = cur ? (cur.index || 0) + 1 : Math.min((run.steps_done || 0) + 1, total || Infinity);
  let doing = '';
  if (cur) {
    const ph = cur.phase || {};
    const verb = PHASE_ING[ph.type] || (ph.label ? ph.label.toLowerCase() : 'running');
    const nodes = (cur.nodes || []).map(nodeName);
    const prof = cur.profile ? plabel(cur.profile, cur.profile_name) : '';
    doing = verb + (prof ? ' ' + prof : '') + (nodes.length ? ' on ' + andList(nodes) : '');
  }
  return { at, total, doing };
}

// whereNow(run): "on step 7 of 30: loading TEST - x on node-a and node-b",
// "queued (2nd): it has not started", "waiting until idle: <reason>"
export function whereNow(run) {
  if (run.state === 'queued') { const p = queuePos(run.id); return p ? `queued (${ordinal(p)}): it has not started` : 'queued: it has not started'; }
  if (run.state === 'paused' || run.state === 'waiting') return 'waiting until idle' + (run.paused_because ? ': ' + run.paused_because : '');
  const { at, total, doing } = stepNow(run);
  const step = total ? `on step ${num(at)} of ${num(total)}` : '';
  return [step, doing].filter(Boolean).join(': ') || 'running';
}

// cancelDialog(run, after): run is the progress document or a run's brief.
export async function cancelDialog(run, after) {
  const live = state.bench.run && state.bench.run.id === run.id ? Object.assign({}, run, state.bench.run) : run;
  const name = live.suite_name || live.benchmark_id || live.suite_id || live.id;
  const queued = live.state === 'queued', waiting = live.state === 'paused' || live.state === 'waiting';
  const { at, total } = stepNow(live);
  const left = total ? Math.max(0, total - at) : 0;
  const bullets = queued ? ['It is taken out of the queue; nothing of it ran.']
    : waiting ? ['Nothing of it was loaded.', 'Its steps are skipped.']
      : ['The step in progress is stopped.', 'The model this run is loading or using is unloaded.',
        left ? `The ${plural(left, 'step')} not started ${left === 1 ? 'is' : 'are'} skipped.` : 'No step is left to skip.', 'The results so far are kept.'];
  const v = await dialog({ title: 'Cancel this run?',
    body: h('div.dcol', { style: 'gap:12px', 'data-dlg-cancel': live.state || '' },
      h('p', h('b', { 'data-name': '' }, name), ' is ', whereNow(live), '.'),
      h('ul.rd-list', bullets.map((b, i) => h('li', { key: 'b' + i }, b)))),
    comment: { label: 'Comment', required: true, placeholder: 'Why? It is kept in the audit log.' },
    actions: [{ label: 'Keep it running', value: undefined, autofocus: true }, { label: 'Cancel the run', kind: 'dng solid', value: true }] });
  if (!v || !v.value || !v.comment) return;
  try {
    await api.post(`/api/bench/runs/${enc(run.id)}/cancel`, { comment: v.comment }, { comment: v.comment });
    toast('Cancelled: ' + name, 'ok');
    if (after) after();
  } catch (e) { toast(e.message, 'err'); }
}

// cancelButton(run, label, cls): the live Cancel control.
export function cancelButton(run, label = 'Cancel', cls = 'btn sm dng', after) {
  return h('button', { class: cls, type: 'button', 'data-act': 'cancel', 'data-cancel': run.id,
    onclick: e => { e.preventDefault(); e.stopPropagation(); cancelDialog(run, after); } }, label);
}
