// "Make standard profiles" (profiles plan, section 6): the dialog of a
// model's panel. It reads first what would be written and shows it, then
// asks for a comment, then writes.
//
//   checking  GET /api/models/{file}/standard-set (nothing changes)
//   ready     the table: each candidate context with its id, kept or not
//             (kept = it fits on an idle fleet; the plan of now beside it)
//             (and the plan's reason), the zones it would use, its need and
//             where that figure comes from, the every-zone sentence, the
//             memory on the hosting machine, and "exists, kept as it is";
//             a required comment; "Write N profiles"
//   nothing   the same table, nothing left to write: Close only
//   error     the read failed: its message, Check again
//   sending   the write
//   written   what was written and what was skipped
//   refused   the write failed: its message, nothing was written
//
// Writes: POST /api/models/{file}/standard-set {comment}. The ids in the
// table are the profile ids themselves (never a shown name).

import { h } from '../../core/dom.js';
import * as api from '../../core/api.js';
import { dialog, badge } from '../../core/ui.js';
import { num, plural } from '../../core/format.js';
import { plainReason } from '../../core/status.js';
import { refreshCatalog } from '../../core/catalog.js';

const ctxText = c => (c % 1024 === 0 ? c / 1024 + 'k' : num(c));
const SOURCE = { 'measured for this shape': 'measured for this shape', derived: 'derived from another context', estimate: 'estimate from the file', unknown: 'not known' };

// rowsTable(set) -> the candidates' table
function rowsTable(set) {
  return h('div.scroll', h('table.tbl.std-tbl', { 'data-std-rows': String(set.rows.length) },
    h('thead', h('tr', h('th', { scope: 'col' }, 'Profile'), h('th.r', { scope: 'col' }, 'Context'), h('th', { scope: 'col' }, 'Kept'),
      h('th', { scope: 'col' }, 'Zones'), h('th.r', { scope: 'col' }, 'Needs'), h('th', { scope: 'col' }, 'Already there'))),
    // the long sentences (every zone, the hosting machine's memory, why not
    // kept) go on a full-width line under their row, so the columns stay narrow
    h('tbody', set.rows.map(r => {
      const notes = [
        !r.kept && r.reason ? h('div', { 'data-why': '' }, h('b', 'Not kept: '), plainReason(r.reason)) : null,
        r.uses_every_zone ? h('div.warn-t', { 'data-every-zone': '' }, r.every_zone_note) : null,
        r.now_note ? h('div', { 'data-now': r.action }, h('b', 'Now: '), r.now_note) : null,
        r.host_memory_note ? h('div', { 'data-host-memory': r.host_memory_counted ? 'counted' : 'not-counted' }, r.host_memory_note) : null,
      ].filter(Boolean);
      return [h('tr', { key: r.id, 'data-std': r.id, 'data-kept': String(!!r.kept), 'data-exists': String(!!r.exists), class: notes.length ? 'has-notes' : null },
      h('td.std-id', h('span.mono', r.id), r.default ? h('span.sub', 'the default context') : null),
      h('td.r.mono', ctxText(r.ctx)),
      h('td', r.kept ? badge('Kept', 'ready', 'It fits with nothing else loaded') : badge('Not kept', 'fail')),
      h('td', r.zones.length ? plural(r.zones.length, 'zone') : h('span.mut', '–'), r.uses_every_zone ? h('span.sub.warn-t', 'every zone') : null),
      h('td.r', r.need_mib != null ? h('span.mono', num(r.need_mib) + ' MiB') : h('span.mut', 'not known'),
        h('span.sub', { 'data-source': r.need_source, title: r.need_basis || null }, r.need_mib != null ? SOURCE[r.need_source] || r.need_source : null)),
      h('td', r.exists ? h('span', { 'data-exists-note': '', title: r.exists_as || null }, 'exists, kept as it is') : h('span.mut', 'no'))),
      notes.length ? h('tr.std-notes', { key: r.id + ':notes', 'data-std-notes': r.id }, h('td', { colspan: '6' }, notes)) : null];
    }).flat().filter(Boolean))));
}

// standardDialog(m, onDone): the dialog for the catalog model m; onDone(text)
// after a write that changed something.
export function standardDialog(m, onDone) {
  const file = m.file || m.key;
  const path = '/api/models/' + encodeURIComponent(file) + '/standard-set';
  let state = 'checking', set = null, msg = '', answer = null, closed = false, closeFn = () => {};
  const note = h('textarea.input.dlg-comment', { rows: 2, maxlength: 500, placeholder: 'Why? It is kept in the audit log with the profiles.', 'aria-label': 'Comment' });
  const go = h('button', { class: 'btn lg pri', type: 'button', 'data-go': '', onclick: () => send() }, 'Write');
  const gate = () => { if (state === 'ready' && note.value.trim()) go.removeAttribute('disabled'); else go.setAttribute('disabled', ''); };
  note.addEventListener('input', gate);
  const root = h('div.dcol');
  const closeBtn = label => h('button', { class: 'btn lg', type: 'button', 'data-close': '', onclick: () => closeFn(undefined) }, label);
  const head = () => h('p', { 'data-std-note': '' }, h('span.mono', file), ': ', set.note, '. ',
    set.default_id ? ['The default ', h('span.mono', set.default_id), ' is ', ctxText(set.default_ctx), ' (the largest kept context up to 32k). '] : 'No context up to 32k is kept, so there is no default. ',
    'Each context was planned on the zones as they are now; nothing was loaded.');

  const draw = () => {
    if (closed) return;
    const d = root.parentNode;
    if (d && d.setAttribute) { d.setAttribute('data-dlg', 'standard'); d.setAttribute('data-state', state); }
    let kids;
    if (state === 'checking') {
      kids = [h('p.mut', { role: 'status' }, 'Planning each context on the zones as they are now…'), h('div.dlg-foot', closeBtn('Cancel'))];
    } else if (state === 'error') {
      kids = [h('div.banner.err', h('span', h('b', 'The standard profiles could not be worked out. '), msg)),
        h('div.dlg-foot', h('button', { class: 'btn lg', type: 'button', 'data-go': 'again', onclick: () => check() }, 'Check again'), closeBtn('Close'))];
    } else if (state === 'refused') {
      kids = [head(), rowsTable(set), h('div.banner.err', { 'data-refused': '' }, h('span', h('b', 'Not written: nothing changed. '), msg)),
        h('div.dlg-foot', h('button', { class: 'btn lg', type: 'button', 'data-go': 'again', onclick: () => check() }, 'Check again'), closeBtn('Close'))];
    } else if (state === 'written') {
      const w = answer.written || [], sk = answer.skipped || [];
      kids = [h('div.banner.info', { role: 'status', 'data-written': String(w.length) },
        h('span', w.length ? [h('b', `Wrote ${plural(w.length, 'profile')}`), answer.revision ? ` in revision ${num(answer.revision)}: ` : ': ', w.join(', '), '.'] : h('b', 'Nothing was written: every kept profile exists.'))),
      sk.length ? h('ul.std-skip', sk.map(s => h('li', h('span.mono', s.id), ': ', s.reason))) : null,
      h('div.dlg-foot', closeBtn('Close'))];
    } else {
      const n = set.to_write.length;
      go.textContent = state === 'sending' ? 'Writing…' : 'Write ' + plural(n, 'profile');
      kids = [head(), rowsTable(set),
        n ? [h('p', { 'data-to-write': String(n) }, 'Writes ', h('b', plural(n, 'profile')), ': ', set.to_write.map((id, i) => [i ? ', ' : null, h('span.mono', id)]),
          '. Placed automatically, marked production, described "standard set". A profile that exists is never changed.'),
        h('div.field.dlg-note', h('label.dlbl', 'Comment (required)'), note),
        h('div.dlg-foot', closeBtn('Cancel'), go)]
          : [h('p', { 'data-to-write': '0' }, 'Nothing to write: every kept context has its profile already.'), h('div.dlg-foot', closeBtn('Close'))]];
    }
    root.replaceChildren(...kids.flat().filter(Boolean));
    if (state === 'ready') gate();
    if (state === 'sending') go.setAttribute('disabled', '');
  };
  const setState = s => { state = s; draw(); };
  async function check() {
    setState('checking');
    try {
      set = await api.get(path);
      if (closed) return;
      setState(set.to_write.length ? 'ready' : 'nothing');
    } catch (e) { if (!closed) { msg = e.message; setState('error'); } }
  }
  async function send() {
    const comment = note.value.trim();
    if (state !== 'ready' || !comment) return;
    setState('sending');
    try {
      answer = await api.send('POST', path, { comment }, { comment });
      if (closed) return;
      setState('written');
      refreshCatalog();
      if (onDone && (answer.written || []).length) onDone(`Wrote ${plural(answer.written.length, 'standard profile')} for ${file}.`);
    } catch (e) { if (!closed) { msg = e.message; setState('refused'); } }
  }
  dialog({ title: h('span', 'Standard profiles for ', h('span.mono', m.short_name || m.name || file)), wide: true, body: close => { closeFn = close; return root; } }).then(() => { closed = true; });
  draw();
  check();
}
