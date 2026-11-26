// "Make standard profiles" (profiles plan, section 6, with the reviewer's
// rulings A to G): the dialog of a model's panel. It reads first what would
// be written and shows it, then asks for a comment, then writes the ticked
// rows.
//
//   checking  GET /api/models/{file}/standard-set[?short=] (nothing changes)
//   ready     the name field (prefilled from the file's production family,
//             else the file's name; editable, "Use this name" reads again
//             with ?short=); the table: a tick box per row that may be
//             written (ticked: the read's default selection), its id and
//             context, whether it fits with nothing loaded (measured,
//             derived, or only by the file estimate), the zones, its need
//             and where that figure comes from, "exists, kept as it is";
//             under each row its sentences (why not, the 5% margin, a figure
//             of another shape, every zone, the plan of now, the hosting
//             machine's memory, what an existing profile carries); a
//             required comment; "Write N profiles" counts the ticked rows
//   named     the name was refused (?short= answered 400): the reason, the
//             field to change it; nothing can be written
//   nothing   the table, no row may be written: Close only
//   error     the read failed: its message, Check again
//   sending   the write
//   written   what was written and what was skipped
//   refused   the write failed: its message, nothing was written
//
// Writes: POST /api/models/{file}/standard-set {comment, short, ids}. The ids
// are the profile ids themselves (never a shown name).

import { h } from '../../core/dom.js';
import * as api from '../../core/api.js';
import { dialog, badge } from '../../core/ui.js';
import { num, plural } from '../../core/format.js';
import { refreshCatalog } from '../../core/catalog.js';

const ctxText = c => (c % 1024 === 0 ? c / 1024 + 'k' : num(c));
const SOURCE = { 'measured for this shape': 'measured for this shape', derived: 'derived from another context', estimate: 'estimate from the file', unknown: 'not known' };
// the row's state as a badge: label, tone, title
const FIT = {
  fits: ['Fits', 'ready', 'It fits with nothing else loaded, by a measured or derived need'],
  'fits-by-estimate': ['Fits, estimated', 'ready', 'Not measured: by the file estimate it fits with nothing else loaded'],
  'estimate-does-not-fit': ['Not measured', 'load', 'Not measured: by the file estimate it does not fit. Not written unless ticked'],
  'does-not-fit': ['Not kept', 'fail', 'By a measured or derived need it does not fit even with nothing loaded'],
  'need-unknown': ['Not kept', 'fail', 'Its need is not known: nothing can be planned'],
};
const SRC_LABEL = { 'family': 'from the existing family', file: "from the file's name", asked: 'the name you gave' };

// rowsTable(set, ticks, onTick) -> the candidates' table
function rowsTable(set, ticks, onTick) {
  return h('div.scroll', h('table.tbl.std-tbl', { 'data-std-rows': String(set.rows.length) },
    h('thead', h('tr', h('th', { scope: 'col' }, h('span.sr', 'Write')), h('th', { scope: 'col' }, 'Profile'), h('th.r', { scope: 'col' }, 'Context'), h('th', { scope: 'col' }, 'Fits'),
      h('th', { scope: 'col' }, 'Zones'), h('th.r', { scope: 'col' }, 'Needs'), h('th', { scope: 'col' }, 'Already there'))),
    // the long sentences go on a full-width line under their row, so the
    // columns stay narrow
    h('tbody', set.rows.map(r => {
      const fit = FIT[r.state] || [r.kept ? 'Fits' : 'Not kept', r.kept ? 'ready' : 'fail', ''];
      const why = r.state === 'estimate-does-not-fit' ? 'Estimate only: ' : 'Not kept: ';
      const notes = [
        r.reason ? h('div', { 'data-why': r.state }, h('b', why), r.reason) : null,
        r.need_note ? h('div', { 'data-need-note': '' }, r.need_note) : null,
        r.shape_note ? h('div', { 'data-shape-note': '' }, r.shape_note) : null,
        r.uses_every_zone ? h('div.warn-t', { 'data-every-zone': r.zones_may_use_by_max_pool ? 'max-pool' : 'reachable' }, r.every_zone_note) : null,
        r.now_note ? h('div', { 'data-now': r.action }, h('b', 'Now: '), r.now_note) : null,
        r.host_memory_note ? h('div', { 'data-host-memory': r.host_memory_counted ? 'counted' : 'not-counted' }, r.host_memory_note) : null,
        r.exists ? h('div', { 'data-exists-note': '' }, h('b', 'Exists: '), r.exists_note || 'exists, kept as it is',
          (r.exists_carries || []).length ? ['. It has what a standard row would not: ', r.exists_carries.join(', ')] : null, '.') : null,
      ].filter(Boolean);
      const on = ticks.has(r.id);
      const box = r.tickable
        ? h('input', { type: 'checkbox', class: 'cb', checked: on, 'data-tick': r.id, 'aria-label': 'Write ' + r.id, onclick: () => onTick(r.id, !on) })
        : h('span.mut', { 'aria-hidden': 'true' }, '–');
      return [h('tr', { key: r.id, 'data-std': r.id, 'data-state': r.state || '', 'data-kept': String(!!r.kept), 'data-exists': String(!!r.exists), 'data-ticked': String(on), class: notes.length ? 'has-notes' : null },
        h('td.std-tick', box),
        h('td.std-id', h('span.mono', r.id), r.default ? h('span.sub', 'the default context') : null),
        h('td.r.mono', ctxText(r.ctx)),
        h('td', badge(fit[0], fit[1], fit[2])),
        h('td', r.zones.length ? plural(r.zones.length, 'zone') : h('span.mut', '–'), r.uses_every_zone ? h('span.sub.warn-t', 'every zone') : null),
        h('td.r', r.need_mib != null ? h('span.mono', num(r.need_mib) + ' MiB') : h('span.mut', 'not known'),
          h('span.sub', { 'data-source': r.need_source, title: r.need_basis || null }, r.need_mib != null ? SOURCE[r.need_source] || r.need_source : null)),
        h('td', r.exists ? h('span', { title: r.exists_as || null }, 'exists, kept as it is') : h('span.mut', 'no'))),
      notes.length ? h('tr.std-notes', { key: r.id + ':notes', 'data-std-notes': r.id }, h('td', { colspan: '7' }, notes)) : null];
    }).flat().filter(Boolean))));
}

// standardDialog(m, onDone): the dialog for the catalog model m; onDone(text)
// after a write that changed something.
export function standardDialog(m, onDone) {
  const file = m.file || m.key;
  const path = '/api/models/' + encodeURIComponent(file) + '/standard-set';
  let state = 'checking', set = null, msg = '', nameMsg = '', answer = null, closed = false, closeFn = () => {};
  let ticks = new Set();
  const note = h('textarea.input.dlg-comment', { rows: 2, maxlength: 500, placeholder: 'Why? It is kept in the audit log with the profiles.', 'aria-label': 'Comment' });
  const name = h('input.input.mono.std-name', { type: 'text', maxlength: 80, spellcheck: 'false', 'aria-label': 'Short name',
    onkeydown: e => { if (e.key === 'Enter') { e.preventDefault(); check(name.value.trim()); } } });
  const go = h('button', { class: 'btn lg pri', type: 'button', 'data-go': '', onclick: () => send() }, 'Write');
  const ids = () => set.rows.filter(r => ticks.has(r.id)).map(r => r.id);
  const gate = () => {
    go.textContent = state === 'sending' ? 'Writing…' : 'Write ' + plural(ids().length, 'profile');
    if (state === 'ready' && note.value.trim() && ids().length) go.removeAttribute('disabled'); else go.setAttribute('disabled', '');
  };
  note.addEventListener('input', gate);
  const root = h('div.dcol');
  const closeBtn = label => h('button', { class: 'btn lg', type: 'button', 'data-close': '', onclick: () => closeFn(undefined) }, label);
  const nameBtn = () => h('button', { class: 'btn', type: 'button', 'data-go': 'name', onclick: () => check(name.value.trim()) }, 'Use this name');
  // the name field: what it is prefilled from, the refusal under it
  const nameField = () => h('div.field.std-namef', { 'data-name-source': set ? set.short_source : 'asked' },
    h('label.dlbl', 'Short name'),
    h('div.row', name, nameBtn()),
    set && state !== 'named' ? h('p.sub', { 'data-name-from': set.short_source },
      set.short_source === 'family' ? [SRC_LABEL.family, ' (', (set.family_ids || []).map((id, i) => [i ? ', ' : null, h('span.mono', id)]), '). The rows are ', h('span.mono', set.short), ' and ', h('span.mono', set.short + ':<N>k'), '.']
        : [SRC_LABEL[set.short_source] || '', '. The rows are ', h('span.mono', set.short), ' and ', h('span.mono', set.short + ':<N>k'), '.']) : null,
    nameMsg ? h('div.banner.err', { 'data-name-refused': '' }, h('span', h('b', 'This name cannot be used. '), nameMsg.charAt(0).toUpperCase() + nameMsg.slice(1))) : null);
  const head = () => h('p', { 'data-std-note': '' }, h('span.mono', file), ': ', set.note, '. ',
    set.default_id ? ['The default ', h('span.mono', set.default_id), ' is ', ctxText(set.default_ctx), ' (the largest context up to 32k that fits). '] : 'No context up to 32k fits, so there is no default. ',
    'Each context was planned with nothing loaded; nothing was loaded. What would happen now is said under a row when it differs.');

  const draw = () => {
    if (closed) return;
    const d = root.parentNode;
    if (d && d.setAttribute) { d.setAttribute('data-dlg', 'standard'); d.setAttribute('data-state', state); }
    let kids;
    if (state === 'checking') {
      kids = [h('p.mut', { role: 'status' }, 'Planning each context with nothing loaded…'), h('div.dlg-foot', closeBtn('Cancel'))];
    } else if (state === 'error') {
      kids = [h('div.banner.err', h('span', h('b', 'The standard profiles could not be worked out. '), msg)),
        h('div.dlg-foot', h('button', { class: 'btn lg', type: 'button', 'data-go': 'again', onclick: () => check(set ? set.short : '') }, 'Check again'), closeBtn('Close'))];
    } else if (state === 'named') {
      go.textContent = 'Write'; go.setAttribute('disabled', '');
      kids = [nameField(), h('div.dlg-foot', closeBtn('Cancel'), go)];
    } else if (state === 'refused') {
      kids = [head(), rowsTable(set, ticks, () => {}), h('div.banner.err', { 'data-refused': '' }, h('span', h('b', 'Not written: nothing changed. '), msg)),
        h('div.dlg-foot', h('button', { class: 'btn lg', type: 'button', 'data-go': 'again', onclick: () => check(set.short_source === 'asked' ? set.short : '') }, 'Check again'), closeBtn('Close'))];
    } else if (state === 'written') {
      const w = answer.written || [], sk = answer.skipped || [];
      kids = [h('div.banner.info', { role: 'status', 'data-written': String(w.length) },
        h('span', w.length ? [h('b', `Wrote ${plural(w.length, 'profile')}`), answer.revision ? ` in revision ${num(answer.revision)}: ` : ': ', w.join(', '), '.'] : h('b', 'Nothing was written.'))),
      sk.length ? h('ul.std-skip', sk.map(s => h('li', h('span.mono', s.id), ': ', s.reason))) : null,
      h('div.dlg-foot', closeBtn('Close'))];
    } else if (state === 'nothing') {
      kids = [nameField(), head(), rowsTable(set, ticks, () => {}),
        set.short_problem ? h('div.banner.err', { 'data-name-refused': '' }, h('span', h('b', 'The name cannot be used. '), set.short_problem, ' Give another name above.')) : null,
        h('p', { 'data-to-write': '0' }, set.short_problem ? 'Nothing can be written under this name.' : 'Nothing to write: every context that fits has its profile already.'),
        h('div.dlg-foot', closeBtn('Close'))];
    } else {
      const n = ids().length;
      kids = [nameField(), head(), rowsTable(set, ticks, (id, on) => { if (on) ticks.add(id); else ticks.delete(id); draw(); }),
        h('p', { 'data-to-write': String(n) }, n ? ['Writes ', h('b', plural(n, 'profile')), ': ', ids().map((id, i) => [i ? ', ' : null, h('span.mono', id)]),
          '. Placed automatically, marked production, described "standard set". A profile that exists is never changed.'] : 'No row is ticked: tick the rows to write.'),
        h('div.field.dlg-note', h('label.dlbl', 'Comment (required)'), note),
        h('div.dlg-foot', closeBtn('Cancel'), go)];
    }
    root.replaceChildren(...kids.flat().filter(Boolean));
    if (state === 'ready' || state === 'sending') gate();
  };
  const setState = s => { state = s; draw(); };
  async function check(short) {
    nameMsg = '';
    setState('checking');
    try {
      set = await api.get(path + (short ? '?short=' + encodeURIComponent(short) : ''));
      if (closed) return;
      name.value = set.short;
      ticks = new Set(set.to_write);
      setState(set.rows.some(r => r.tickable) ? 'ready' : 'nothing');
    } catch (e) {
      if (closed) return;
      if (short && e.status === 400) { set = null; name.value = short; nameMsg = e.message; setState('named'); return; }
      msg = e.message; setState('error');
    }
  }
  async function send() {
    const comment = note.value.trim();
    if (state !== 'ready' || !comment || !ids().length) return;
    const body = { comment, short: set.short, ids: ids() };
    setState('sending');
    try {
      answer = await api.send('POST', path, body, { comment });
      if (closed) return;
      setState('written');
      refreshCatalog();
      if (onDone && (answer.written || []).length) onDone(`Wrote ${plural(answer.written.length, 'standard profile')} for ${file}.`);
    } catch (e) { if (!closed) { msg = e.message; setState('refused'); } }
  }
  dialog({ title: h('span', 'Standard profiles for ', h('span.mono', m.short_name || m.name || file)), wide: true, body: close => { closeFn = close; return root; } }).then(() => { closed = true; });
  draw();
  check('');
}
