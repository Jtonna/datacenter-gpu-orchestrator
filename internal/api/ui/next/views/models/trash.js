// The models trash (v65, models slice 3; boards Models-Trash, Models-Delete
// and the Models list's footer row). A deleted model FILE waits in the trash
// with its profiles until it is restored; nothing removes it for good (the
// server has no purge: purge_on is false, and this page has no control that
// erases a file).
//
//   trashFooter(disk)     the Models list's footer row "Deleted models": the
//                         catalog disk's trash_bytes / trash_count /
//                         next_purge_at / trash_purge_on, and the way in
//   mountTrash(el, r)     #/models/deleted?q=&sort=&item=<id>: the Deleted
//                         models view (GET /api/models/trash) and an item's
//                         side panel with Restore (POST
//                         /api/models/trash/{id}/restore, a required comment;
//                         a 409 profiles_taken offers rename_taken)
//   deleteModel(m, done)  the panel's "Delete model": a dry run first (POST
//                         /api/models/delete/{file} with dry_run, and again
//                         with interrupt when something holds the file, to
//                         read would_interrupt), then the name typed and a
//                         comment; interrupt is sent only when the dialog
//                         showed what it interrupts
//
// Every refusal is the server's sentence under a plain headline, never its
// JSON. read_only (the models folder mounted read-only) is its own state;
// the server gives no way to know it before the real call (the dry run does
// not write), so the page says it up front only once this page has seen
// that answer.

import { h } from '../../core/dom.js';
import { href, navigate } from '../../core/router.js';
import * as api from '../../core/api.js';
import { liveView, empty, skeleton, searchField, sidePanel, filterSelect, dialog } from '../../core/ui.js';
import { num, plural, dateTime, agoText, parseGoDuration, list, dur, DASH, now } from '../../core/format.js';
import { onCatalog, needCatalog, refreshCatalog } from '../../core/catalog.js';

const DAY = 864e5;
const TRASH = 'M4 6h12M8 6V4h4v2M6 6l.7 10h6.6L14 6M8.5 9v4.5M11.5 9v4.5';
const CHEV_R = 'M8 5l5 5-5 5', CHEV_L = 'M12 5l-5 5 5 5';
const WARN = 'M10 3l8 14H2zM10 8.5v4M10 14.8v.01', CLOCK = 'M10 6v4l2.5 1.5', BACK = 'M4 10a6 6 0 102-4.5M4 4v4h4', CHECK = 'M4 10.5l4 4 8-9';
const icon = (d, extra) => h('svg', { class: 'ic', viewBox: '0 0 20 20', 'aria-hidden': 'true' }, extra || null, h('path', { d }));
const clockIcon = () => icon(CLOCK, h('circle', { cx: 10, cy: 10, r: 7 }));
const off = (tag, reason, attrs, ...kids) => h(tag, Object.assign({ type: 'button', 'aria-disabled': 'true', title: reason, 'data-off': '', onclick: e => { e.preventDefault(); e.stopPropagation(); } }, attrs), ...kids);

// sizes in decimal GB, as the Models list and the boards
const gbOf = v => (v / 1e9).toLocaleString('en-US', { maximumFractionDigits: 1, minimumFractionDigits: v >= 100e9 ? 0 : 1 });
const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
const dayOf = ms => { const d = new Date(ms); return `${d.getDate()} ${MONTHS[d.getMonth()]} ${d.getFullYear()}`; };
// daysAgo(ms) -> "3 days ago" (the board's words), "5 h ago" under a day; the exact time on hover
const daysAgo = ms => {
  const d = Math.floor((now() - ms) / DAY);
  return h('time', { datetime: new Date(ms).toISOString(), title: dateTime(ms) }, d >= 1 ? plural(d, 'day') + ' ago' : agoText(ms));
};
const daysLeft = ms => Math.max(0, Math.ceil((ms - now()) / DAY));
// monoList(['a', 'b', 'c']) -> "a, b and c" with each name in mono
const monoList = xs => xs.flatMap((x, k) => [k ? (k === xs.length - 1 ? ' and ' : ', ') : null, h('span.mono', x)]);
const was = (n, one, many) => `${plural(n, one, many)} ${n === 1 ? 'was' : 'were'}`;

const PURGE_OFF = 'Nothing removes a deleted model on this scheduler (purge is off): it stays in Deleted models, and keeps using the disk, until it is restored.';
// keptCell(item, purgeOn) -> "24 Dec 2026" (kept until) or "27 days" (clears in, when purge is on)
const keptCell = (it, purgeOn) => (purgeOn
  ? h('time', { title: 'It is removed for good on ' + dateTime(it.purge_at), class: daysLeft(it.purge_at) <= 3 ? 'del-soon' : null }, plural(daysLeft(it.purge_at), 'day'))
  : h('time', { title: `Kept until ${dateTime(it.purge_at)}, the date set when it was deleted (model_trash_days). ${PURGE_OFF}` }, dayOf(it.purge_at)));

// hasTrash(disk): the server serves the trash (v65 and later)
export const hasTrash = disk => !!disk && typeof disk.trash_count === 'number';

// ---- what this page has learned from the server ----
// readOnlySeen: the last read_only refusal of a delete or restore on this page
let readOnlySeen = '';
const watchers = new Set();
// onTrashChange(fn) -> off: a delete or restore went through
export const onTrashChange = fn => { watchers.add(fn); return () => watchers.delete(fn); };
const changed = () => { refreshCatalog(); for (const fn of [...watchers]) fn(); };

// errOf(e) -> the error object of a refusal ({message, reason, ...}) or {}
const errOf = e => (e && e.body && e.body.error && typeof e.body.error === 'object' ? e.body.error : {});

// box(kind, iconNode, ...text) -> one of the dialog's boxes (warn: red, ok: green, ro: amber)
const box = (kind, attrs, ...rows) => h('div', Object.assign({ class: 'dbox ' + kind }, attrs), ...rows);
const pt = (ic, color, ...text) => h('div.pt', h('span', { class: 'pt-ic ' + color }, ic), h('span', ...text));

const roNote = () => (readOnlySeen
  ? box('ro', { 'data-ro-note': '' }, pt(icon(WARN), 'amber', h('b', 'This scheduler answered that its models folder is read-only'),
    ' the last time a delete or restore was tried from this page, so this one will most likely be refused too, with nothing changed.'), h('p.dmsg', readOnlySeen))
  : null);
// roFlag(...figures) -> the read-only state when the server says
// models_writable false: said at once, with the file's figures above it,
// and nothing to type or send
const RO_FLAG = 'The models folder is read-only on this scheduler: deleting and restoring are not possible here. Nothing can be changed.';
const roFlag = (...figures) => [h('p.del-figs', { 'data-figures': '' }, ...figures),
  box('ro', { 'data-ro-flag': '' }, pt(icon(WARN), 'amber', h('b', RO_FLAG)))];
const profilesOf = ids => (ids.length ? [plural(ids.length, 'profile'), ': ', monoList(ids)] : 'no profiles');
const roState = (what, msg) => box('warn', { 'data-ro': '' }, pt(icon(WARN), 'red', h('b', 'The models folder is read-only on this scheduler: nothing was changed. '), what),
  h('p.dmsg', msg));

// ---------- the Models list's footer row ----------

// trashFooter(disk, notYet) -> the footer row: "34.6 GB in deleted models ·
// 2 files · nothing is removed automatically yet · View" (the board: "· next
// clears in 2 days": said only when the server purges); unavailable on a
// server older than v65
export function trashFooter(disk, notYet) {
  const ic = h('span.tbar-ic', icon(TRASH));
  if (disk && !hasTrash(disk)) return off('button.tbar', notYet, { 'data-act': 'trash' }, ic, 'Deleted models', h('span.mut', ' · not available'));
  const go = h('span.tbar-go', 'View', icon(CHEV_R));
  if (!disk) return h('a.tbar', { href: href('models', ['deleted']), 'data-act': 'trash' }, ic, 'Deleted models', go);
  const n = disk.trash_count || 0;
  if (!n) return h('a.tbar', { href: href('models', ['deleted']), 'data-act': 'trash', title: 'A model you delete waits in Deleted models, with its profiles, until you restore it.' },
    ic, 'No deleted models', go);
  const purge = disk.trash_purge_on && disk.next_purge_at ? ` · next clears in ${plural(daysLeft(disk.next_purge_at), 'day')}` : ' · nothing is removed automatically yet';
  return h('a.tbar', { href: href('models', ['deleted']), 'data-act': 'trash',
    title: `${gbOf(disk.trash_bytes || 0)} GB in ${plural(n, 'deleted model file')}: they still use the disk. ` + (disk.trash_purge_on ? 'The next is removed for good on ' + dateTime(disk.next_purge_at) + '.' : PURGE_OFF) },
  ic, `${gbOf(disk.trash_bytes || 0)} GB in deleted models`, h('span.mut', ` · ${plural(n, 'file')}${purge}`), go);
}

// ---------- the delete dialog ----------

const DRY_NOTE = 'dry run: what deleting this file would change';
const HEADS = {
  interrupt_incomplete: 'Not deleted: the interrupt did not finish in time.',
  loaded_again: 'Not deleted: it was loaded again meanwhile.',
  in_use: 'Not deleted: it is in use now.',
  in_trash: 'It is in Deleted models already.',
  not_found: 'This file is not in the models folder.',
  not_regular: 'Not deleted: it is not a plain file.',
  config_refused: 'Not deleted: its profiles could not be taken out of the configuration.',
  move_failed: 'Not deleted: the file could not be moved to Deleted models.',
  bad_name: 'Not deleted: this is not a model file name.',
  not_a_file: 'Not deleted: a command profile has no model file.',
};
const AGAIN = new Set(['interrupt_incomplete', 'loaded_again', 'in_use', 'move_failed', 'config_refused']);

// interrupts(would, m) -> "gemma-4-27b is loaded on ps5-01 and ps5-02 with 2
// requests in flight; 1 request for gemma-4-27b:long is queued", and the
// number of requests that fail. The zones are the answer's own (nodes, v67);
// a server older than that names none, and the catalog's instance is joined
function interrupts(would, m) {
  const parts = [];
  let n = 0;
  for (const i of would.instances || []) {
    const cat = Array.isArray(i.nodes) ? null : (m.instances || []).find(x => x.profile === i.profile);
    const own = Array.isArray(i.nodes) ? i.nodes : cat && cat.nodes;
    const nodes = own && own.length ? own : null;
    n += (i.in_flight || 0) + (i.waiters || 0);
    parts.push([h('span.mono', i.profile), ' is ', i.state === 'ready' ? 'loaded' : i.state, nodes ? [' on ', monoList(nodes)] : null,
      i.in_flight ? ` with ${plural(i.in_flight, 'request')} in flight` : null, i.waiters ? ` and ${plural(i.waiters, 'request')} waiting for it` : null]);
  }
  for (const q of would.queued || []) {
    n += q.waiters || 0;
    parts.push([plural(q.waiters || 0, 'request'), ' for ', h('span.mono', q.profile), q.waiters === 1 ? ' is queued' : ' are queued']);
  }
  return { n, parts: parts.flatMap((p, i) => (i ? ['; ', p] : [p])) };
}

// deleteModel(m, onDone, writable): the dialog; onDone(answer) after the
// file moved. writable is the catalog disk's models_writable: false opens
// the read-only state at once (no dry run, nothing typed, nothing sent);
// true or undefined: the dry run first
export function deleteModel(m, onDone, writable) {
  const file = m.file || m.key;
  const name = m.short_name || m.name || file;
  const path = '/api/models/delete/' + encodeURIComponent(m.key);
  let state = 'checking', figures = null, refusal = null, closed = false, closeFn = () => {};
  const typed = h('input.input.dinp', { type: 'text', placeholder: name, 'data-confirm': '', 'aria-label': `Type ${name} to confirm`, autocomplete: 'off', spellcheck: 'false' });
  const note = h('textarea.input.dlg-comment', { rows: 2, maxlength: 500, placeholder: 'Why? It is kept in the audit log with the delete.', 'aria-label': 'Comment' });
  const go = h('button', { class: 'btn lg dng solid', type: 'button', 'data-go': '', onclick: () => send() }, 'Delete model');
  const gate = () => {
    const ok = state === 'ready' && typed.value.trim() === name && !!note.value.trim();
    if (ok) go.removeAttribute('disabled'); else go.setAttribute('disabled', '');
    typed.classList.toggle('ok', typed.value.trim() === name);
  };
  typed.addEventListener('input', gate);
  note.addEventListener('input', gate);
  const root = h('div.dcol');
  const closeBtn = label => h('button', { class: 'btn lg', type: 'button', 'data-close': '', onclick: () => closeFn(undefined) }, label);

  const draw = () => {
    if (closed) return;
    const d = root.parentNode;
    if (d && d.setAttribute) { d.setAttribute('data-dlg', 'delete'); d.setAttribute('data-state', state); }
    let kids;
    if (state === 'checking') {
      kids = [h('p.mut', { role: 'status' }, 'Checking what deleting it would change…'), h('div.dlg-foot', closeBtn('Cancel'))];
    } else if (state === 'blocked') {
      const er = refusal.err, runs = er.runs || [];
      kids = [box('warn', null, pt(icon(WARN), 'red', h('b', runs.length === 1 ? 'A benchmark run uses this model: ' : 'Benchmark runs use this model: '),
        runs.flatMap((r, i) => [i ? ', ' : null, h('a', { href: href('benchmarks', ['runs', r]) }, r)]), '. ',
        'Interrupt does not stop benchmark runs. Cancel the run or wait for it to finish, then delete the model.'), h('p.dmsg', refusal.msg)),
      h('div.dlg-foot', closeBtn('Close'))];
    } else if (state === 'read-only' && !refusal) {
      const ids = (m.profiles || []).map(p => p.id).filter(Boolean);
      kids = [...roFlag(h('span.mono', file), ' · ', m.size_bytes ? gbOf(m.size_bytes) + ' GB' : null, m.size_bytes ? ' · ' : null, profilesOf(ids)), h('div.dlg-foot', closeBtn('Close'))];
    } else if (state === 'read-only') {
      kids = [roState(h('span', h('span.mono', file), ' stays in the models folder with its profiles.'), refusal.msg), h('div.dlg-foot', closeBtn('Close'))];
    } else if (state === 'refused') {
      const r = refusal.err.reason;
      kids = [box('warn', null, pt(icon(WARN), 'red', h('b', HEADS[r] || 'Not deleted.'), ' ', r === 'in_trash' ? 'Restore it from Deleted models to use it again.' : 'Nothing was moved; its profiles are as they were.'),
        h('p.dmsg', refusal.msg)),
      h('div.dlg-foot', AGAIN.has(r) ? h('button', { class: 'btn lg', type: 'button', 'data-go': 'again', onclick: () => check() }, 'Check again') : null, closeBtn('Close'))];
    } else {
      const f = figures, would = f.would_interrupt;
      const it = would ? interrupts(would, m) : null;
      const np = (f.profiles || []).length;
      const size = gbOf(f.size_bytes || m.size_bytes || 0) + ' GB';
      const kept = f.purge_on
        ? [h('b', `Kept for ${plural(f.trash_days, 'day')}.`), ' The file moves to Deleted models and stays there until ', h('time', { title: dateTime(f.purge_at) }, dayOf(f.purge_at)), `. It still uses ${size} until then.`]
        : [h('b', 'Kept in Deleted models.'), ` The file moves there and still uses ${size}. Nothing removes it automatically yet: it stays until it is restored (kept until `,
          h('time', { title: dateTime(f.purge_at) }, dayOf(f.purge_at)), `, ${plural(f.trash_days, 'day')} from now).`];
      kids = [
        would ? box('warn', { 'data-interrupts': '' }, pt(icon(WARN), 'red', h('b', it.n ? `This interrupts ${plural(it.n, 'request')}.` : 'This unloads it.'), ' ', it.parts, '. ',
          it.n ? 'It is unloaded now and these requests fail.' : 'It is unloaded now.'))
          : box('ok', { 'data-free': '' }, pt(icon(CHECK), 'green', h('b', 'Nothing is interrupted.'), ' Nothing is loaded or queued for it now.')),
        box('ok', null, pt(clockIcon(), 'green', ...kept),
          pt(icon(BACK), 'green', h('b', 'Restorable.'), np ? (np === 1 ? ' Restore it and its 1 profile comes back as it is now.' : ` Restore it and all ${np} profiles come back exactly as they are now.`) : ' Restore it any time. It has no profiles.')),
        roNote(),
        h('div.field', h('label.dlbl', 'Type ', h('span.mono', name), ' to confirm'), typed),
        h('div.field.dlg-note', h('label.dlbl', 'Comment (required)'), note),
        h('div.dlg-foot', closeBtn('Cancel'), go)];
      if (state === 'sending') { go.textContent = 'Deleting…'; go.setAttribute('disabled', ''); } else go.textContent = 'Delete model';
    }
    root.replaceChildren(...kids.filter(Boolean).map(k => (Array.isArray(k) ? h('div', k) : k)));
    if (state === 'ready') gate();
  };
  const set = (s, r) => { state = s; if (r !== undefined) refusal = r; draw(); };
  const refuse = e => {
    const er = errOf(e);
    if (er.reason === 'read_only') { readOnlySeen = e.message; return set('read-only', { err: er, msg: e.message }); }
    if (er.reason === 'in_use' && (er.runs || []).length) return set('blocked', { err: er, msg: e.message });
    return set('refused', { err: er, msg: e.message });
  };
  const dry = interrupt => api.send('POST', path, interrupt ? { comment: DRY_NOTE, dry_run: true, interrupt: true } : { comment: DRY_NOTE, dry_run: true }, { dryRun: true, comment: DRY_NOTE });
  // check: the dry run; when something holds the file (in_use, no benchmark run), again with interrupt to read would_interrupt
  async function check() {
    set('checking');
    try {
      let f;
      try { f = await dry(false); } catch (e) {
        const er = errOf(e);
        if (e.status !== 409 || er.reason !== 'in_use' || (er.runs || []).length) throw e;
        if (closed) return;
        f = await dry(true);
      }
      if (closed) return;
      figures = f;
      set('ready');
      typed.focus();
    } catch (e) { if (!closed) refuse(e); }
  }
  async function send() {
    if (state !== 'ready' || typed.value.trim() !== name || !note.value.trim()) return;
    const comment = note.value.trim();
    const body = { comment };
    if (figures.would_interrupt) body.interrupt = true; // shown in the dialog and confirmed by typing the name
    set('sending');
    try {
      const res = await api.send('POST', path, body, { comment });
      if (closed) return;
      closeFn(undefined);
      changed();
      if (onDone) onDone(res || {});
    } catch (e) { if (!closed) refuse(e); }
  }
  dialog({ title: h('span', 'Delete ', h('span.mono', file), '?'), body: close => { closeFn = close; return root; } }).then(() => { closed = true; });
  if (writable === false) { set('read-only', null); return; }
  draw();
  check();
}

// deletedText(res) -> the sentence after a delete
export function deletedText(res) {
  const n = (res.profiles_removed || []).length, i = res.interrupted;
  return [`Deleted ${res.file}. It is in Deleted models; `,
    n ? `${n === 1 ? '1 profile' : plural(n, 'profile')} left the configuration${res.revision ? ` (revision ${num(res.revision)})` : ''}.` : 'it had no profiles.',
    i ? ` ${was(i.requests_cut || 0, 'request')} cut${i.queued_cancelled ? ` and ${was(i.queued_cancelled, 'queued request')} cancelled` : ''}.` : null].filter(Boolean).join('');
}

// ---------- restore ----------

// restoreItem(it, onDone, writable): the restore dialog (a required
// comment); a 409 profiles_taken offers the server's rename_taken. writable
// false (the list's models_writable) opens the read-only state at once
function restoreItem(it, onDone, writable) {
  let state = 'ask', refusal = null, closed = false, closeFn = () => {}, comment = '';
  const ids = (it.profiles || []).map(p => p.id);
  const note = h('textarea.input.dlg-comment', { rows: 2, maxlength: 500, placeholder: 'Why? It is kept in the audit log with the restore.', 'aria-label': 'Comment' });
  const go = h('button', { class: 'btn lg pri', type: 'button', 'data-go': '', onclick: () => send(false) }, 'Restore');
  const gate = () => { if (state === 'ask' && note.value.trim()) go.removeAttribute('disabled'); else go.setAttribute('disabled', ''); };
  note.addEventListener('input', gate);
  const root = h('div.dcol');
  const closeBtn = label => h('button', { class: 'btn lg', type: 'button', 'data-close': '', onclick: () => closeFn(undefined) }, label);
  const draw = () => {
    if (closed) return;
    const d = root.parentNode;
    if (d && d.setAttribute) { d.setAttribute('data-dlg', 'restore'); d.setAttribute('data-state', state); }
    let kids;
    if (state === 'ask' || state === 'sending') {
      kids = [box('ok', null, pt(icon(BACK), 'green', h('b', 'Restorable.'), ' The file moves back to the models folder',
        ids.length ? [ids.length === 1 ? ' and its 1 profile comes back as it was: ' : ` and its ${ids.length} profiles come back as they were: `, monoList(ids), '.'] : '. It had no profiles.')),
      roNote(),
      h('div.field.dlg-note', h('label.dlbl', 'Comment (required)'), note),
      h('div.dlg-foot', closeBtn('Cancel'), go)];
      go.textContent = state === 'sending' ? 'Restoring…' : 'Restore';
    } else if (state === 'taken') {
      const taken = refusal.err.taken || [];
      kids = [box('ro', null, pt(icon(WARN), 'amber', h('b', 'Some names of its profiles are taken now: '), list(taken), '. Nothing was restored.'), h('p.dmsg', refusal.msg)),
        h('p', 'Restore with new names: a taken profile comes back as ', h('span.mono', '<id>-restored'), ' and a taken alias is left off. The other profiles keep their names.'),
        h('div.dlg-foot', closeBtn('Cancel'), h('button', { class: 'btn lg pri', type: 'button', 'data-go': 'rename', onclick: () => send(true) }, 'Restore with new names'))];
    } else if (state === 'read-only' && !refusal) {
      kids = [...roFlag(h('span.mono', it.file), ' · ', gbOf(it.size_bytes || 0) + ' GB · ', profilesOf(ids)), h('div.dlg-foot', closeBtn('Close'))];
    } else if (state === 'read-only') {
      kids = [roState(h('span', h('span.mono', it.file), ' stays in Deleted models.'), refusal.msg), h('div.dlg-foot', closeBtn('Close'))];
    } else {
      kids = [box('warn', null, pt(icon(WARN), 'red', h('b', 'Not restored.'), ' It stays in Deleted models.'), h('p.dmsg', refusal.msg)), h('div.dlg-foot', closeBtn('Close'))];
    }
    root.replaceChildren(...kids.filter(Boolean));
    gate();
  };
  const set = (s, r) => { state = s; if (r !== undefined) refusal = r; draw(); };
  async function send(rename) {
    if (!rename) { comment = note.value.trim(); if (state !== 'ask' || !comment) return; }
    set('sending');
    try {
      const res = await api.send('POST', '/api/models/trash/' + encodeURIComponent(it.id) + '/restore', rename ? { comment, rename_taken: true } : { comment }, { comment });
      if (closed) return;
      closeFn(undefined);
      changed();
      if (onDone) onDone(res || {});
    } catch (e) {
      if (closed) return;
      const er = errOf(e);
      if (er.reason === 'read_only') { readOnlySeen = e.message; set('read-only', { err: er, msg: e.message }); } else if (er.reason === 'profiles_taken' && !rename) set('taken', { err: er, msg: e.message });
      else set('refused', { err: er, msg: e.message });
    }
  }
  dialog({ title: h('span', 'Restore ', h('span.mono', it.file), '?'), body: close => { closeFn = close; return root; } }).then(() => { closed = true; });
  if (writable === false) { set('read-only', null); return; }
  draw();
}

// restoredText(res) -> the sentence after a restore
function restoredText(res) {
  const n = (res.profiles_restored || []).length;
  const ren = Object.entries(res.renamed || {}).map(([a, b]) => `${a} is back as ${b}`);
  const dropped = res.aliases_dropped || [];
  return [`Restored ${res.file}: `, n ? (n === 1 ? 'its 1 profile is back' : `its ${n} profiles are back`) : 'it had no profiles', ren.length ? ` (${list(ren)})` : '',
    dropped.length ? `; the taken ${dropped.length === 1 ? 'alias' : 'aliases'} ${list(dropped)} ${dropped.length === 1 ? 'was' : 'were'} left off` : '', '.'].join('');
}

// ---------- the Deleted models view ----------

const SORTS = [['until', 'kept until'], ['deleted', 'deleted, newest'], ['name', 'name'], ['size', 'size']];
const SORT = {
  until: (a, b) => a.purge_at - b.purge_at || a.file.localeCompare(b.file),
  deleted: (a, b) => b.deleted_at - a.deleted_at || a.file.localeCompare(b.file),
  name: (a, b) => a.file.localeCompare(b.file),
  size: (a, b) => b.size_bytes - a.size_bytes || a.file.localeCompare(b.file),
};
const ttlText = t => { const s = t == null ? null : parseGoDuration(t); return s == null ? null : s > 0 ? dur(s) : 'Never'; };

function itemPanel(it, purgeOn, onRestore) {
  const bad = it.state !== 'trashed' || it.in_trash_folder === false;
  const profs = it.profiles || [];
  return [
    h('div.mdl-acts.del-acts', { key: 'acts' },
      bad ? off('button.btn.pri', it.problem ? 'It cannot be restored: ' + it.problem : `It is ${it.state}, not in the trash: it cannot be restored.`, { 'data-act': 'restore' }, icon(BACK), 'Restore model')
        : h('button.btn.pri', { type: 'button', 'data-act': 'restore', onclick: () => onRestore(it) }, icon(BACK), 'Restore model'),
      off('button.btn.red', 'Removing a model for good is not built: nothing on this scheduler erases a model file. It stays here until it is restored.', { 'data-act': 'remove', style: 'margin-left:auto' }, 'Remove now')),
    bad ? h('div.banner.err', { key: 'prob', role: 'status' }, h('span.grow', h('b', 'Needs a look. '), it.problem || `It is ${it.state}.`)) : null,
    it.comment ? h('p.del-why', { key: 'why' }, h('span.mut', 'Comment: '), it.comment) : null,
    h('section.mdl-cfg', { key: 'cfg' },
      h('div.dh', h('h3', 'Configurations ', h('span.mut', profs.length ? plural(profs.length, 'profile') : 'No profiles'))),
      profs.length ? h('table.tbl.mdl-ptbl.del-ptbl',
        h('thead', h('tr', h('th', { scope: 'col' }, 'Profile'), h('th.r', { scope: 'col' }, 'Context'), h('th.r', { scope: 'col' }, 'Memory needed'), h('th.r', { scope: 'col' }, 'Unload when idle'))),
        h('tbody.ro', profs.map(p => {
          const s = p.spec || {}, ttl = ttlText(s.ttl);
          return h('tr', { key: p.id, 'data-profile': p.id }, h('td', h('span.mono', p.id)), h('td.r.mono', s.ctx ? num(s.ctx) : DASH),
            h('td.r.mono', s.needs_mib ? num(s.needs_mib) + ' MiB' : DASH), ttl ? h('td.r.mono', ttl) : h('td.r.mut', { title: 'The default of the scheduler' }, DASH));
        })))
        : h('p.mut.mdl-pad', 'It had no profiles.'),
      profs.length ? h('p.mut.mdl-pad', 'These profiles come back with a restore, as they were when it was deleted. They cannot be edited here.') : null),
    h('p.mut.del-note', { key: 'purge' }, purgeOn ? `It is removed for good on ${dateTime(it.purge_at)}.` : PURGE_OFF),
  ];
}

export function mountTrash(el, route) {
  let r = route, query = r.query.q || '', data = null, error = null, alive = true, side = null, note = '';
  const setQuery = q => navigate('models', ['deleted'], Object.assign({}, r.query, q), true);
  const read = () => api.get('/api/models/trash').then(d => { data = d; error = null; if (alive) { view.refresh(); syncSide(); } }, e => { error = e; if (alive) view.refresh(); });
  const offChange = onTrashChange(read);
  const offCatalog = onCatalog(() => { if (alive) view.refresh(); });
  const onRestore = it => restoreItem(it, res => { note = restoredText(res); if (r.query.item === it.id) setQuery({ item: '' }); read(); }, data ? data.models_writable : undefined);

  const syncSide = () => {
    const id = r.query.item || '';
    if (!id) { if (side) { const s = side; side = null; s.close(); } return; }
    if (side && side.key === id) { side.redraw(); return; }
    if (side) { const s = side; side = null; s.close(); }
    const find = () => ((data && data.items) || []).find(x => x.id === id) || null;
    const p = sidePanel({
      closesMenu: true, label: 'Deleted model', name: 'Deleted model: ' + id,
      title: () => { const it = find(); return h('span.mono.del-title', it ? it.file : id); },
      sub: () => {
        const it = find();
        if (!it) return null;
        return [gbOf(it.size_bytes) + ' GB · deleted ', daysAgo(it.deleted_at), it.actor ? ' by ' + it.actor : null, ' · ',
          data.purge_on ? ['clears in ', keptCell(it, true)] : ['kept until ', keptCell(it, false)]];
      },
      body: () => {
        const it = find();
        if (it) return itemPanel(it, !!data.purge_on, onRestore);
        return data ? h('section.card', empty('This item is not in Deleted models', 'It may have been restored. Close this panel to see what is there.')) : skeleton(3);
      },
      onClose: () => { if (side === p) { side = null; setQuery({ item: '' }); } },
    });
    p.key = id;
    side = p;
  };

  const render = () => {
    const items = (data && data.items) || [];
    const purgeOn = !!(data && data.purge_on);
    const sort = SORT[r.query.sort] ? r.query.sort : 'until';
    const q = query.trim().toLowerCase();
    const rows = items.filter(it => !q || [it.file, it.actor, it.comment].concat((it.profiles || []).map(p => p.id)).some(x => String(x || '').toLowerCase().includes(q))).sort(SORT[sort]);
    const sel = r.query.item || '';
    const open = id => setQuery({ item: id });
    const row = it => h('div', {
      key: it.id, role: 'listitem', class: 'mr del' + (sel === it.id ? ' sel' : ''), 'aria-current': sel === it.id ? 'true' : null, 'data-trash': it.id, 'data-state': it.state, tabindex: '0',
      onclick: () => open(it.id), onkeydown: e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); open(it.id); } } },
    h('span.del-ic', icon(TRASH)),
    h('span.mdl-name', h('span.mono.nm', it.file), h('span.mut', gbOf(it.size_bytes) + ' GB'),
      it.state !== 'trashed' || it.problem ? h('span.err-t.del-prob', { title: it.problem || null }, 'Needs a look: ', it.problem || `it is ${it.state}`) : null),
    h('span.del-pc', plural(it.profile_count != null ? it.profile_count : (it.profiles || []).length, 'profile')),
    h('span.mut.del-at', daysAgo(it.deleted_at)),
    h('span.del-until', keptCell(it, purgeOn)));
    let body;
    if (error && !data) body = error.status === 404 ? empty('Deleted models are not available', 'This scheduler is older than v65 (models slice 3): it has no trash. ' + error.message) : empty('Deleted models could not be read', error.message);
    else if (!data) body = skeleton(3);
    else if (!items.length) body = empty('Nothing in deleted models', 'A model you delete waits here, with its profiles, until you restore it. Nothing is removed automatically.');
    else if (!rows.length) body = empty('No deleted model matches', `Nothing matches "${query}". Search looks at the file name, the profiles, who deleted it and the comment.`);
    else body = [h('div.mr.del.mr-h', { 'aria-hidden': 'true' }, h('span'), h('span.lbl', 'Model'), h('span.lbl', 'Profiles'), h('span.lbl', 'Deleted'), h('span.lbl', purgeOn ? 'Clears in' : 'Kept until')),
      h('div.mdl-rows', { role: 'list', 'aria-label': 'Deleted models' }, rows.map(row))];
    const repairs = (data && data.repairs_at_start) || [];
    return [
      h('h1.sr', 'Deleted models'),
      repairs.length ? h('div.banner.warn', { key: 'repairs', role: 'status', 'data-repairs': '' },
        h('span.grow', h('b', 'At the last start the scheduler found deletes or restores that had stopped half way: '),
          repairs.length === 1 ? repairs[0] : h('ul.del-list', repairs.map(x => h('li', x))))) : null,
      note ? h('div.banner.info', { key: 'note', role: 'status', 'data-trash-note': '' }, h('span.grow', note)) : null,
      h('section.panel.mdl.del-view', { key: 'trash', 'aria-label': 'Deleted models' },
        h('div.mdl-bar',
          h('a.btn', { href: href('models'), 'data-act': 'back' }, icon(CHEV_L), 'All models'),
          searchField(query, v => { query = v; view.refresh(); }, 'Search deleted models', 'trash-search', { size: 'lg' }),
          filterSelect('Sort', SORTS.map(([id, label]) => ({ id, label: id === 'until' && purgeOn ? 'clears soonest' : label })), sort, id => setQuery({ sort: id === 'until' ? '' : id }))),
        h('div.sech', { title: data ? (purgeOn ? null : PURGE_OFF) : null }, h('span.lbl', 'Deleted models'),
          data && items.length ? h('span.lbl.mut', `${plural(data.count != null ? data.count : items.length, 'file')} · ${gbOf(data.total_bytes || 0)} GB`, purgeOn ? null : ' · nothing is removed automatically yet') : null),
        body),
    ];
  };
  const view = liveView(el, render, ['conn', 'hello']);
  needCatalog();
  read();
  syncSide();
  return {
    update(nr) { r = nr; syncSide(); view.refresh(); },
    unmount() { alive = false; offChange(); offCatalog(); if (side) { const s = side; side = null; s.close(); } view.unmount(); },
  };
}
