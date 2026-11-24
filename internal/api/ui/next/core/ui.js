// Shared components of the new shell. Each returns a DOM node built with h();
// the markup and the rules are documented in ../../README.md. Views compose
// these and re-render with liveView() (or morph()) when the store changes.
//
// Every export of the classic core/ui.js is here with the same name and
// signature (the views both UIs share import them; a Go test checks the list),
// drawn with the design's classes; the design's own components follow at the
// end of the file.

import { h, morph, icon } from './dom.js';
import { on } from './store.js';
import { catalog } from './catalog.js';
import { openPanel } from './panel.js';
import { num, mib, gb, gbNum, parseGoDuration, clock, dur, DASH } from './format.js';

// ---------- live rendering ----------

// liveView(el, render, keys) draws render() into el now and again after every
// change of the store keys (default: status and the one-second tick), coalesced so
// that several changes in one turn draw once. Returns {refresh(), unmount()}: give it back from mount().
export function liveView(el, render, keys = ['status', 'tick']) {
  let queued = false, dead = false;
  const draw = () => {
    queued = false;
    if (dead) return;
    try { morph(el, render()); } catch (e) {
      console.error(e);
      morph(el, banner('err', 'This screen could not be drawn: ' + (e && e.message ? e.message : e)));
    }
  };
  // Coalesced with a microtask, not a timer: a background tab throttles timers,
  // and a monitoring page must not fall behind because of that.
  const refresh = () => { if (!queued && !dead) { queued = true; queueMicrotask(draw); } };
  const off = on(keys, refresh);
  draw();
  return { refresh, unmount() { dead = true; off(); } };
}

// ---------- small pieces ----------

export function pageHead(title, sub, actions) {
  return h('div.page-head', h('h1', title), sub ? h('span.sub', sub) : null, actions ? h('div.actions', actions) : null);
}

// badge('Ready', 'ready'): kinds ready, use, load, q, n, settle, aside, fail, line, dash.
export const badge = (text, kind = 'n', title) => h('span', { class: 'b b-' + kind, title }, text);

export const nodeChip = name => h('span.node', name);
export const nodeChips = names => h('span.nodes', (names || []).map(nodeChip));

// stat('Speed', '14.2', 'tok/s') -> label over a large figure with a small unit.
export function stat(label, value, unit, opts = {}) {
  return h('div', { class: 'stat' + (opts.small ? ' sm' : ''), title: opts.title },
    h('span', label),
    h('b', { class: opts.cls }, value, unit ? [' ', h('small', unit)] : null));
}

// bar([{cls:'m', value: 8410}, {cls:'k', value: 1560}], 13824, 'aria text')
// Segment classes: m model, k KV cache, c compute, o other, a amber, x coral,
// l lilac, ld loading stripes, rsv reserved outline.
export function bar(segments, total, label, size) {
  const t = total > 0 ? total : segments.reduce((s, x) => s + (x.value || 0), 0) || 1;
  return h('div', { class: 'bar' + (size ? ' ' + size : ''), role: 'img', 'aria-label': label || '' },
    segments.filter(s => s.value > 0).map(s =>
      h('span', { class: s.cls, style: `width:${Math.min(100, (s.value / t) * 100).toFixed(2)}%`, title: s.title })));
}

export function legend(items) {
  return h('div.legend', items.map(([cls, text]) => h('span', h('span', { class: 'sw ' + cls }), text)));
}

// memoryRow: one node's memory as name | segmented bar | used / usable | split.
// mem = {model_mib, kv_mib, compute_mib, total_mib}; budget = usable MiB.
export function memoryRow(name, mem, budget, extra) {
  const other = Math.max(0, (mem.total_mib || 0) - (mem.model_mib || 0) - (mem.kv_mib || 0) - (mem.compute_mib || 0));
  const label = `${gb(mem.total_mib)} for this model on ${name}` + (budget ? `, of ${gb(budget)} usable` : '');
  // a part under 0.05 GB would read "0.0": it is given in MiB instead
  const part = (v, what) => (v >= 51 ? `${gbNum(v)} GB ${what}` : v > 0 ? `${num(v)} MiB ${what}` : null);
  const split = [part(mem.model_mib, 'model'), part(mem.kv_mib, 'KV cache'), part(mem.compute_mib, 'compute')].filter(Boolean);
  return h('div.nr', { key: 'mem-' + name },
    h('span.mono', name, extra ? [' ', extra] : null),
    bar([{ cls: 'm', value: mem.model_mib, title: 'Model ' + mib(mem.model_mib) }, { cls: 'k', value: mem.kv_mib, title: 'KV cache ' + mib(mem.kv_mib) },
      { cls: 'c', value: mem.compute_mib, title: 'Compute ' + mib(mem.compute_mib) }, { cls: 'o', value: other, title: 'Other ' + mib(other) }], budget, label),
    h('span.mono.fig', { title: budget ? `${mib(mem.total_mib)} of ${mib(budget)} usable` : mib(mem.total_mib) }, budget ? `${gbNum(mem.total_mib)} / ${gb(budget)}` : gb(mem.total_mib)),
    h('span.mut.split', { title: `${mib(mem.model_mib)} model, ${mib(mem.kv_mib)} KV cache, ${mib(mem.compute_mib)} compute` }, split.join(' · ')));
}

// copyButton(text, what) -> a small "Copy" button that puts text on the clipboard.
export function copyButton(text, what) {
  return h('button.copy', { type: 'button', title: 'Copy ' + (what || 'it'), 'aria-label': 'Copy ' + (what || text), onclick: async e => {
    e.stopPropagation();
    try { await navigator.clipboard.writeText(text); toast('Copied ' + (what || text), 'ok'); } catch { toast('Copy it by hand: ' + text); }
  } }, 'Copy');
}

// facts([['Address', node], ['Priority', '1']]) -> aligned label / value pairs.
export function facts(pairs) {
  return h('dl.facts', pairs.filter(Boolean).map(([k, v, title]) => [h('dt', k), h('dd', { title }, v)]));
}

// backLink('Models', href) -> the way back from a detail page, above its title.
export const backLink = (label, to) => h('a.back', { href: to }, '\u2190 ' + label);

// more(open, onToggle, show, hide) -> a quiet "show more" button for a part
// that is folded away (the scheduler's own wording, a long list).
export function more(open, onToggle, show, hide) {
  return h('button.more', { type: 'button', 'aria-expanded': open ? 'true' : 'false', onclick: e => { e.stopPropagation(); onToggle(); } }, open ? (hide || 'Hide') : (show || 'Show'));
}

// memBar({model, other, total, limit}, label): one node's memory on one track.
// Solid segments: models (mint), other use (grey); the rest of the track is
// free. `limit` is where a new model must stop (free minus the headroom): one
// marker line, drawn only when it is inside the track.
export function memBar(o, label) {
  const t = o.total > 0 ? o.total : 1;
  const pct = v => Math.max(0, Math.min(100, (v / t) * 100)).toFixed(2) + '%';
  return h('div.membar', { role: 'img', 'aria-label': label || '' },
    h('div.track',
      o.model > 0 ? h('span.m', { style: 'width:' + pct(o.model), title: 'Models ' + mib(o.model) }) : null,
      o.other > 0 ? h('span.o', { style: 'width:' + pct(o.other), title: 'Other use ' + mib(o.other) }) : null),
    o.limit != null && o.limit > 0 && o.limit < t ? h('i.lim', { style: 'left:' + pct(o.limit), title: 'New models may fill the node up to here; the rest is kept free' }) : null);
}

// progress({fraction: 0.41, cls: 'a', left: node, mid: node, right: node, foot: [l, r], striped: 0.2})
export function progress(o) {
  const f = Math.max(0, Math.min(1, o.fraction || 0));
  const segs = [{ cls: o.cls || 'a', value: f }];
  if (o.striped) segs.push({ cls: 'ld', value: Math.min(1 - f, o.striped) });
  return h('div.prog',
    (o.left || o.mid || o.right) ? h('div.prog-top', o.left || h('span'), o.mid || null, o.right || null) : null,
    bar(segs, 1, o.label, o.size || 'lg'),
    o.foot ? h('div.prog-foot', h('span', o.foot[0]), h('span', o.foot[1])) : null);
}

// steps([{label:'Upload model', note:'4.1 of 7.3 GB', state:'now'}]) state: done | now | fail | ''.
export function steps(items) {
  return h('div.steps', items.map(s => h('div', { class: 'step ' + (s.state || ''), 'aria-current': s.state === 'now' ? 'step' : null },
    h('b', s.label), h('span', s.note || ' '))));
}

// table({columns:[{label, cls:'r', render: row => node, title}], rows, key: row => id,
//        onRow: row => {}, minWidth: 860, empty: node})
export function table(o) {
  if (!o.rows || !o.rows.length) return o.empty || null;
  return h('div.scroll', h('table.tbl', { style: o.minWidth ? `min-width:${o.minWidth}px` : null },
    h('thead', h('tr', o.columns.map(c => h('th', { scope: 'col', class: c.cls, title: c.title }, c.label)))),
    h('tbody', o.rows.map((r, i) => h('tr', { key: o.key ? o.key(r, i) : i, class: (o.onRow ? 'click ' : '') + (o.rowClass ? o.rowClass(r) || '' : ''),
      onclick: o.onRow ? () => o.onRow(r) : null, tabindex: o.onRow ? '0' : null,
      onkeydown: o.onRow ? e => { if (e.key === 'Enter') o.onRow(r); } : null },
    o.columns.map(c => h('td', { class: c.cls }, c.render(r, i))))))));
}

// chips([{id:'all', label:'All', count: 12}], 'all', id => {}) -> filter chips.
// An option may carry cls (extra classes: 'all' for the design's "ALL ZONES")
// and title.
export function chips(options, current, onPick, label) {
  return h('div.chips', { role: 'group', 'aria-label': label || 'Filter' }, options.map(o =>
    h('button', { class: 'fc' + (o.cls ? ' ' + o.cls : '') + (o.id === current ? ' on' : ''), type: 'button', title: o.title, 'aria-pressed': o.id === current ? 'true' : 'false', onclick: () => onPick(o.id) },
      o.label, o.count != null ? h('span.n', num(o.count)) : null)));
}

// tabs([{id, label, count}], current, id => {}) -> sub navigation inside a view.
export function tabs(items, current, onPick) {
  return h('div.tabs', { role: 'tablist' }, items.map(t =>
    h('button', { class: 'tab' + (t.id === current ? ' on' : ''), type: 'button', role: 'tab', 'aria-selected': t.id === current ? 'true' : 'false', onclick: () => onPick(t.id) },
      t.label, t.count != null ? badge(num(t.count), 'n') : null)));
}

export function empty(title, text, action) {
  return h('div.empty', h('b', title), text ? h('p', text) : null, action || null);
}

// banner('warn', 'text', actionNode): kinds info, warn, err, or '' for neutral.
export function banner(kind, content, action) {
  return h('div', { class: 'banner ' + (kind || ''), role: kind === 'err' ? 'alert' : 'status' }, h('span.grow', content), action || null);
}

export function skeleton(lines = 3) {
  return h('div.cb', { style: 'display:flex;flex-direction:column;gap:10px;padding-top:4px' },
    Array.from({ length: lines }, (_, i) => h('div.skel', { style: `width:${[72, 54, 63, 40][i % 4]}%` })));
}

export function button(label, onclick, kind = '', attrs = {}) {
  return h('button', Object.assign({ class: 'btn ' + kind, type: 'button', onclick }, attrs), label);
}

// ---------- diff ----------

function show(v) {
  if (v === undefined || v === null) return null;
  if (typeof v === 'object') return JSON.stringify(v, null, 1).replace(/\n\s*/g, ' ');
  return String(v);
}
// diffView([{path:'settings.queue_timeout', old:'600s', new:'900s'}])
export function diffView(diff) {
  if (!diff || !diff.length) return h('div.diff', h('div.diff-row', h('span.none', 'No changes.')));
  return h('div.diff', diff.map(d => {
    const o = show(d.old), n = show(d.new);
    return h('div.diff-row',
      h('span.path', d.path),
      o == null ? h('span.none', 'not set') : h('span.old', o),
      n == null ? h('span.none', 'removed') : h('span.new', n));
  }));
}

// ---------- dialog, confirm, toast ----------

let openDialogs = 0;
// dialog({title, body, actions:[{label, kind, value, autofocus}], wide, comment})
// resolves with the picked action's value (undefined on Escape, the close
// button or a backdrop click). body may be a node or a function (close =>
// node) for a body with its own controls.
//
// The design's dialog (Zones "Confirm": "Unload … from …?", "Reboot 3
// zones?"): 500 wide, a 14 px radius, 20 by 22 px of padding, the title
// (18 px, 800) and a close button in one row, the actions on the right of
// the last row, 34 px high. wide makes it 820 (a diff to review).
//
// comment (optional, the comment field the audit needs, 00-combined S27):
// {label, required, placeholder, value}. A text box above the actions; the
// dialog then resolves with {value, comment} for an action, and a required
// comment keeps the main action (kind pri or dng) disabled while it is empty.
export function dialog(o) {
  return new Promise(resolve => {
    const prev = document.activeElement;
    const close = v => {
      document.removeEventListener('keydown', onKey, true);
      back.remove();
      openDialogs--;
      if (prev && prev.focus) prev.focus();
      resolve(v);
    };
    const note = o.comment ? h('textarea.input.dlg-comment', { rows: 3, maxlength: 500, placeholder: o.comment.placeholder || '', 'aria-label': o.comment.label || 'Comment' }) : null;
    if (note && o.comment.value) note.value = o.comment.value;
    const pick = a => () => {
      const v = typeof a.value === 'function' ? a.value() : a.value;
      close(note ? (v === undefined ? undefined : { value: v, comment: note.value.trim() }) : v);
    };
    const onKey = e => {
      if (e.key === 'Escape') { e.stopPropagation(); close(undefined); }
      if (e.key === 'Tab') {
        const f = dlg.querySelectorAll('button,input,select,textarea,a[href]');
        if (!f.length) return;
        const first = f[0], last = f[f.length - 1];
        if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
        else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
      }
    };
    const id = 'dlg-' + Date.now();
    const main = [];
    const dlg = h('div', { class: 'dlg' + (o.wide ? ' wide' : ''), role: 'dialog', 'aria-modal': 'true', 'aria-labelledby': id },
      h('div.dlg-head', h('h2', { id }, o.title),
        h('button.x', { type: 'button', 'aria-label': 'Close', title: 'Close', onclick: () => close(undefined) },
          h('svg', { class: 'ic', viewBox: '0 0 20 20', 'aria-hidden': 'true' }, h('path', { d: 'M5 5l10 10M15 5L5 15' })))),
      typeof o.body === 'function' ? o.body(close) : o.body,
      note ? h('div.field.dlg-note', h('label', o.comment.label || 'Comment', o.comment.required ? ' (required)' : ''), note) : null,
      o.actions ? h('div.dlg-foot', o.actions.map(a => {
        const b = h('button', { class: 'btn lg ' + (a.kind || ''), type: 'button', onclick: pick(a) }, a.label);
        if (a.autofocus) b.dataset.autofocus = '1';
        if (/\b(pri|dng)\b/.test(a.kind || '')) main.push(b);
        return b;
      })) : null);
    if (note && o.comment.required) {
      const gate = () => { for (const b of main) { if (note.value.trim()) b.removeAttribute('disabled'); else b.setAttribute('disabled', ''); } };
      note.addEventListener('input', gate);
      gate();
    }
    const back = h('div.backdrop', { onmousedown: e => { if (e.target === back) close(undefined); } }, dlg);
    document.body.appendChild(back);
    openDialogs++;
    document.addEventListener('keydown', onKey, true);
    (dlg.querySelector('[data-autofocus]') || dlg.querySelector('input,textarea,select') || dlg.querySelector('button')).focus();
  });
}

// confirm({title, intro, hits:[{kind:'x'|'a'|'m'|'', text: node}], confirmLabel,
//          cancelLabel, danger, extra: node}) -> Promise<boolean>
// The dialog says what will happen in plain words; the confirm button names the action.
export function confirm(o) {
  return dialog({
    title: o.title,
    body: h('div.col', { style: 'gap:14px' },
      o.intro ? h('p', o.intro) : null,
      o.hits && o.hits.length ? h('div', { style: 'display:flex;flex-direction:column;gap:8px' },
        o.hits.map(x => h('div.hit', h('i', { class: x.kind || '' }), h('span', x.text)))) : null,
      o.extra || null),
    actions: [
      { label: o.cancelLabel || 'Cancel', value: false, autofocus: !!o.danger },
      { label: o.confirmLabel || 'Confirm', kind: o.danger ? 'dng solid' : 'pri', value: true, autofocus: !o.danger },
    ],
  }).then(v => v === true);
}

// promptText({title, intro, label, value, placeholder, confirmLabel}) -> Promise<string|null>
export function promptText(o) {
  const input = h('input.input', { type: 'text', value: o.value || '', placeholder: o.placeholder, maxlength: o.maxlength || 200, 'aria-label': o.label });
  return dialog({
    title: o.title,
    body: close => {
      input.addEventListener('keydown', e => { if (e.key === 'Enter') close(input.value); });
      return h('div.col', { style: 'gap:12px' }, o.intro ? h('p', o.intro) : null, h('div.field', h('label', o.label), input));
    },
    actions: [{ label: 'Cancel', value: null }, { label: o.confirmLabel || 'Save', kind: 'pri', value: () => input.value }],
  }).then(v => (v == null ? null : v));
}

let toastBox = null;
// toast('Saved as revision 15', 'ok'): kinds ok, err, or '' for neutral. Errors stay longer.
export function toast(text, kind = '') {
  if (!toastBox) { toastBox = h('div.toasts', { role: 'status', 'aria-live': 'polite' }); document.body.appendChild(toastBox); }
  const t = h('div', { class: 'toast ' + kind }, text);
  toastBox.appendChild(t);
  setTimeout(() => t.remove(), kind === 'err' ? 9000 : 4500);
}

// ---------- forms from a schema ----------

export function getPath(obj, key) {
  let v = obj;
  for (const p of key.split('.')) { if (v == null) return undefined; v = v[p]; }
  return v;
}
export function setPath(obj, key, value) {
  const parts = key.split('.');
  let o = obj;
  for (let i = 0; i < parts.length - 1; i++) { if (o[parts[i]] == null || typeof o[parts[i]] !== 'object') o[parts[i]] = {}; o = o[parts[i]]; }
  if (value === undefined) delete o[parts[parts.length - 1]]; else o[parts[parts.length - 1]] = value;
}
export const humanize = key => {
  const s = key.split('.').pop().replace(/_/g, ' ').replace(/\bmib\b/i, 'MiB').replace(/\bmb s\b/i, 'MB/s').replace(/\bttl\b/i, 'TTL').replace(/\bctx\b/i, 'context').replace(/\btps\b/i, 'tok/s');
  return s.charAt(0).toUpperCase() + s.slice(1);
};
const same = (a, b) => JSON.stringify(a ?? null) === JSON.stringify(b ?? null);

// fieldLabel(field) -> its label; a unit that the control already shows is not repeated ("Min share", not "Min share MiB").
export function fieldLabel(f) {
  if (f.label) return f.label;
  const l = humanize(f.key);
  return f.unit && l.toLowerCase().endsWith(' ' + String(f.unit).toLowerCase()) ? l.slice(0, -String(f.unit).length - 1) : l;
}

// tagInput(values, onChange, placeholder) -> a list-of-strings editor.
export function tagInput(values, onChange, placeholder) {
  let vals = (values || []).map(String);
  const box = h('div.tags');
  const input = h('input', { type: 'text', placeholder: placeholder || 'Type and press Enter', 'aria-label': placeholder || 'Add a value' });
  const add = () => {
    const parts = input.value.split(',').map(s => s.trim()).filter(Boolean);
    if (!parts.length) return;
    vals = vals.concat(parts.filter(p => !vals.includes(p)));
    input.value = '';
    draw(); onChange(vals.slice());
  };
  input.addEventListener('keydown', e => {
    if (e.key === 'Enter' || e.key === ',') { e.preventDefault(); add(); }
    else if (e.key === 'Backspace' && !input.value && vals.length) { vals.pop(); draw(); onChange(vals.slice()); }
  });
  input.addEventListener('blur', add);
  function draw() {
    box.replaceChildren(...vals.map((v, i) => h('span.node', v, h('button', { type: 'button', 'aria-label': 'Remove ' + v, onclick: () => { vals.splice(i, 1); draw(); onChange(vals.slice()); } }, '×'))), input);
  }
  draw();
  return box;
}

// toggle(checked, onChange, label) -> an accessible switch.
export function toggle(checked, onChange, label) {
  const b = h('button.switch', { type: 'button', role: 'switch', 'aria-checked': checked ? 'true' : 'false', 'aria-label': label }, h('span'));
  b.addEventListener('click', () => {
    const next = b.getAttribute('aria-checked') !== 'true';
    b.setAttribute('aria-checked', next ? 'true' : 'false');
    onChange(next);
  });
  return b;
}

// schemaForm({fields, values, onChange, showDefaults}) builds a form from
// schema field objects {key, type, enum, default, min, max, unit, description,
// restart_required, label}. Types: duration, int, float, bool, enum, string,
// text, list; anything else is edited as JSON. Keys may be dotted ("match.hostname").
// Returns {el, values(), validate() -> bool, setErrors({key: message}), changed() -> [keys]}.
// The node is stable (data-keep): morph() leaves it alone while the user types.
export function schemaForm(o) {
  const vals = JSON.parse(JSON.stringify(o.values || {}));
  const initial = JSON.parse(JSON.stringify(vals));
  const errEls = {}, marks = {}, inputs = {};
  const fire = k => { mark(k); if (o.onChange) o.onChange(k, getPath(vals, k), vals); };
  function mark(k) {
    const f = o.fields.find(x => x.key === k);
    if (!marks[k] || !f) return;
    const cur = getPath(vals, k);
    const offDefault = o.showDefaults !== false && f.default != null && cur !== undefined && !same(cur, f.default);
    marks[k].replaceChildren(
      offDefault ? h('span', { class: 'b b-load', title: 'The default is ' + show(f.default) + (f.unit ? ' ' + f.unit : '') }, 'Changed from default') : '',
      !same(cur, getPath(initial, k)) ? h('span', { class: 'b b-q' }, 'Edited') : '',
      f.restart_required ? h('span', { class: 'b b-settle', title: 'Takes effect after the scheduler restarts' }, 'Needs restart') : '');
  }
  function control(f) {
    const cur = getPath(vals, f.key);
    const id = 'f-' + f.key.replace(/[^a-z0-9]+/gi, '-') + '-' + Math.random().toString(36).slice(2, 7);
    let c;
    switch (f.type) {
      case 'bool':
        c = h('label.check', toggle(!!cur, v => { setPath(vals, f.key, v); fire(f.key); }, f.label || humanize(f.key)), h('span.mut', f.unit || ''));
        break;
      case 'enum':
        c = h('select.select', { id }, (f.enum || []).map(e => h('option', { value: e, selected: String(cur) === String(e) }, String(e))));
        c.addEventListener('change', () => { setPath(vals, f.key, c.value); fire(f.key); });
        break;
      case 'list':
        c = tagInput(Array.isArray(cur) ? cur : [], v => { setPath(vals, f.key, v); fire(f.key); }, f.placeholder);
        break;
      case 'text':
        c = h('textarea.textarea', { id, rows: 4, placeholder: f.placeholder }, cur == null ? '' : String(cur));
        c.addEventListener('input', () => { setPath(vals, f.key, c.value); fire(f.key); });
        break;
      case 'int': case 'float': case 'duration': case 'string': {
        const numeric = f.type === 'int' || f.type === 'float';
        const inp = h('input', { id, class: 'input' + (f.type !== 'string' ? ' mono' : ''), type: 'text', inputmode: numeric ? 'decimal' : null,
          value: cur == null ? '' : String(cur), placeholder: f.placeholder || (f.type === 'duration' ? 'for example 90s or 15m' : f.default != null ? String(f.default) : null), autocomplete: 'off', spellcheck: 'false' });
        inp.addEventListener('input', () => {
          const raw = inp.value.trim();
          let v = raw;
          if (raw === '') v = undefined;
          else if (numeric) { const n = Number(raw); v = Number.isNaN(n) ? raw : n; }
          setPath(vals, f.key, v);
          setErr(f.key, check(f, v));
          fire(f.key);
        });
        inputs[f.key] = inp;
        const unit = f.unit && f.type !== 'duration' ? f.unit : null;
        c = unit ? h('div.unit', inp, h('span', unit)) : inp;
        break;
      }
      default: {
        c = h('textarea.textarea.mono', { id, rows: 4 }, cur === undefined ? '' : JSON.stringify(cur, null, 2));
        c.addEventListener('input', () => {
          try { setPath(vals, f.key, c.value.trim() ? JSON.parse(c.value) : undefined); setErr(f.key, ''); } catch { setErr(f.key, 'This is not valid JSON.'); }
          fire(f.key);
        });
      }
    }
    const m = marks[f.key] = h('span.row', { style: 'gap:6px' });
    const e = errEls[f.key] = h('span.err', { role: 'alert' });
    const range = f.min != null && f.max != null ? `${f.min} to ${f.max}` : f.min != null ? `at least ${f.min}` : f.max != null ? `at most ${f.max}` : '';
    const help = [f.description, range ? `Allowed: ${range}.` : ''].filter(Boolean).join(' ');
    return h('div', { class: 'field' + (f.type === 'text' || f.wide || !['bool', 'enum', 'int', 'float', 'duration', 'string', 'list', 'text'].includes(f.type) ? ' wide' : '') },
      h(f.type === 'bool' || f.type === 'list' ? 'div.lab' : 'label', { for: f.type === 'bool' || f.type === 'list' ? null : id }, fieldLabel(f), m),
      c, help ? h('span.help', help) : null, e);
  }
  function check(f, v) {
    if (v === undefined || v === '') return f.required ? 'This is required.' : '';
    if (f.type === 'int' || f.type === 'float') {
      if (typeof v !== 'number') return 'Enter a number.';
      if (f.type === 'int' && !Number.isInteger(v)) return 'Enter a whole number.';
      if (f.min != null && v < f.min) return `At least ${f.min}.`;
      if (f.max != null && v > f.max) return `At most ${f.max}.`;
    }
    if (f.type === 'duration' && parseGoDuration(v) == null) return 'Enter a duration such as 90s, 15m or 1h30m.';
    return '';
  }
  function setErr(k, msg) {
    if (errEls[k]) errEls[k].textContent = msg || '';
    if (inputs[k]) inputs[k].setAttribute('aria-invalid', msg ? 'true' : 'false');
  }
  const el = h('div.form-grid', { 'data-keep': '' }, o.fields.map(control));
  o.fields.forEach(f => mark(f.key));
  return {
    el,
    values: () => JSON.parse(JSON.stringify(vals)),
    validate() {
      let ok = true;
      for (const f of o.fields) { const m = check(f, getPath(vals, f.key)); setErr(f.key, m); if (m) ok = false; }
      return ok;
    },
    setErrors(map) { for (const k in map) setErr(k, map[k]); },
    changed: () => o.fields.map(f => f.key).filter(k => !same(getPath(vals, k), getPath(initial, k))),
  };
}

// ---------- promoted from the Benchmarks views ----------

// signedPct(12) -> "+12%", signedPct(-3.4) -> "−3.4%".
export function signedPct(v) {
  if (v == null || Number.isNaN(v)) return '–';
  const s = Math.abs(v) < 10 ? num(Math.abs(v), 1) : num(Math.abs(v), 0);
  return (v > 0 ? '+' : v < 0 ? '−' : '') + s + '%';
}

// changeMark(pct, {better: 'lower' | 'higher', regression}) -> a signed percent
// next to a figure: coral with a Regression badge when it crossed its limit,
// amber when worse, mint when better, grey when under 1%.
export function changeMark(pct, o = {}) {
  if (pct == null) return h('span.mut', '–');
  const worse = (o.better || 'lower') === 'lower' ? pct > 0 : pct < 0;
  const cls = o.regression ? 'err-t' : Math.abs(pct) < 1 ? 'mut' : worse ? 'warn-t' : 'ok-t';
  return h('span.nowrap', { title: o.regression ? 'More than the allowed change for this figure' : worse ? 'Worse than before, within the allowed change' : 'Not worse' },
    h('span', { class: 'mono ' + cls }, signedPct(pct)), o.regression ? [' ', badge('Regression', 'aside')] : null);
}

// segments(index, total, label) -> a bar of equal steps: done mint, the current
// one amber, the rest grey. For "phase 2 of 5" and the like.
export function segments(index, total, label) {
  return h('div.bar.sm.seg', { role: 'img', 'aria-label': label || `Step ${index + 1} of ${total}` },
    Array.from({ length: total }, (_, i) => h('span', { class: i < index ? 'm' : i === index ? 'a' : 'o' })));
}

// pickCards([{title, text, chips: ['Load', 'Requests'], onPick}]) -> a grid of
// cards to choose a starting point from (templates).
export function pickCards(items) {
  return h('div.grid', items.map(it => h('button', { class: 'card pick', type: 'button', onclick: it.onPick },
    h('b', it.title), it.text ? h('span', it.text) : null,
    it.chips && it.chips.length ? h('span.nodes', it.chips.map(c => h('span.node', c))) : null)));
}

// subBlock(title, actions, ...body) -> one repeatable block of a form (a phase,
// a request, a turn, a tier): a title row with its buttons, then its fields.
export function subBlock(title, actions, ...body) {
  return h('div.sub-block', h('div.sub-head', typeof title === 'string' ? h('b', title) : title, actions ? h('span.row', { style: 'gap:6px;flex-wrap:nowrap' }, actions) : null), ...body);
}

// orderButtons({index, count, onMove: delta => {}, onRemove, what}) -> the
// up / down / Remove buttons of a block in an ordered list.
export function orderButtons(o) {
  return [
    o.onMove ? button('↑', () => o.onMove(-1), 'sm', { 'aria-label': 'Move ' + (o.what || 'it') + ' up', disabled: o.index === 0 }) : null,
    o.onMove ? button('↓', () => o.onMove(1), 'sm', { 'aria-label': 'Move ' + (o.what || 'it') + ' down', disabled: o.index === o.count - 1 }) : null,
    o.onRemove ? button('Remove', o.onRemove, 'sm dng', { 'aria-label': 'Remove ' + (o.what || 'it') }) : null,
  ];
}

// ---------- the design's components (Components boards 1 to 3) ----------

// panel(title, action, ...body) -> a panel: a 40 px header with the title on
// the left and one action (a link or a button) on the right, then the body.
export function panel(title, action, ...body) {
  return h('section.panel', { 'aria-label': typeof title === 'string' ? title : null },
    title || action ? h('div.ch.split', typeof title === 'string' ? h('h2', title) : title, action || null) : null, ...body);
}

// segmented([{id, label}], current, id => {}, label) -> a one-of-few switch
// (the design's period switch). The current one is pressed.
export function segmented(options, current, onPick, label) {
  return h('div.seg', { role: 'group', 'aria-label': label || 'Choose' }, options.map(o =>
    h('button', { type: 'button', class: o.id === current ? 'on' : '', 'aria-pressed': o.id === current ? 'true' : 'false', onclick: () => onPick(o.id) }, o.label)));
}

// filterSelect(label, [{id, label}], current, id => {}) -> the design's filter
// dropdown ("Sort: needs attention first"): a native select, so it works
// with the keyboard and on a phone.
// opts.bare draws it as the Overview's flat button (no arrow), as drawn there.
export function filterSelect(label, options, current, onPick, opts = {}) {
  return h('select', { class: 'fs' + (opts.bare ? ' bare' : ''), 'aria-label': label, key: 'fs-' + label, onchange() { onPick(this.value); } },
    options.map(o => h('option', { value: o.id, selected: o.id === current }, label + ': ' + o.label)));
}

// searchField(value, onInput, placeholder, key) -> a small search box. The
// node is kept by morph (data-keep with a key: give each box on a screen its
// own), so typing is never interrupted by a redraw. onInput is the one given
// at the first draw: pass a function that reads the view's current state.
//
// Drawn as the design's search field (Models, Zones, Benchmarks): a
// magnifier inside the box, then the text. opts.size 'lg' is the Zones
// page's (28 px high, 290 wide, 12.5 px text); the default is the small one
// of the panels and tables (24 px, 150 wide, 12 px). opts.width sets the
// width. The box is kept by morph, the input inside it with it.
export function searchField(value, onInput, placeholder, key = 'search', opts = {}) {
  const i = h('input', { type: 'search', value: value || '', placeholder: placeholder || 'Search', 'aria-label': placeholder || 'Search' });
  i.addEventListener('input', () => onInput(i.value));
  return h('label', { class: 'sx' + (opts.size === 'lg' ? ' lg' : ''), 'data-keep': '', key, style: opts.width ? 'width:' + opts.width + 'px' : null },
    h('svg', { class: 'ic', viewBox: '0 0 20 20', 'aria-hidden': 'true' }, h('circle', { cx: 9, cy: 9, r: 5.5 }), h('path', { d: 'M13.5 13.5L17 17' })), i);
}

// The zone state colours: free, in use 1 to 4 (darker is busier), loading,
// down (offline or failed), switched off. One class per square.
export const ZONE_SQUARE = { free: 'free', use1: 'use1', use2: 'use2', use3: 'use3', use4: 'use4', load: 'load', down: 'down', off: 'off' };

// zoneStatus('use4', 'In use', title) -> the design's zone status: a 14 px
// square in the state's colour and its name.
export function zoneStatus(square, label, title) {
  return h('span', { class: 'zs' + (square === 'off' ? ' off' : ''), title },
    h('i', { class: 'sq ' + (ZONE_SQUARE[square] || 'free'), 'aria-hidden': 'true' }), label);
}

// colourKey(extra) -> the key shared by zone status and availability.
export function colourKey(extra) {
  const sq = c => h('i', { class: 'sq ' + c, 'aria-hidden': 'true' });
  return h('div.key',
    h('b', 'Colour key'),
    h('span', sq('free'), 'Free'),
    h('span', sq('use1'), sq('use2'), sq('use3'), sq('use4'), 'In use, darker is busier'),
    h('span', sq('load'), 'Loading'),
    h('span', sq('down'), 'Down: offline or failed'),
    h('span', sq('off'), 'Switched off'),
    extra ? h('span', { style: 'margin-left:auto' }, extra) : null);
}

// availStrip(cells, label) -> 24 squares, oldest on the left, current hour on
// the right. cells: 24 square classes (ZONE_SQUARE), or null for an hour
// nothing is known about (an empty outlined square), or {cls, title}. Draw
// it only from a real series (GET /api/nodes/availability, availClass()).
export function availStrip(cells, label) {
  return h('div.avail', { role: 'img', 'aria-label': label || 'Availability, hour by hour' },
    (cells || []).slice(-24).map(c => {
      const cls = c && typeof c === 'object' ? c.cls : c;
      return h('i', { class: cls ? ZONE_SQUARE[cls] || 'none' : 'none', title: c && typeof c === 'object' ? c.title : null });
    }));
}

// availState(cell) -> the state a cell is drawn in. The server colours an
// hour in_use only from a quarter of its known time in use; an hour with any
// use is never drawn Free, so a free cell with seconds.in_use > 0 is in use.
export function availState(cell) {
  const st = cell && cell.state;
  return st === 'free' && cell.seconds && cell.seconds.in_use > 0 ? 'in_use' : st;
}

// availLevel(cell) -> its busy level 1 to 4 (darker is busier): busy_level,
// the server's quarter of busy (the mean of in flight over parallel across
// the in-use time): under 25 % 1, under 50 % 2, under 75 % 3, else 4.
export function availLevel(cell) {
  let l = cell && cell.busy_level;
  if (!l && cell && typeof cell.busy === 'number') l = cell.busy < 0.25 ? 1 : cell.busy < 0.5 ? 2 : cell.busy < 0.75 ? 3 : 4;
  return Math.min(4, Math.max(1, l || 1));
}

// availClass(cell) -> the square of one cell of GET /api/nodes/availability:
// its state (free, in_use, loading, down, off, unknown; availState) and, in
// use, its busy level 1 to 4 (availLevel). Unknown is null (an empty square).
export function availClass(cell) {
  switch (availState(cell)) {
    case 'free': return 'free';
    case 'in_use': return 'use' + availLevel(cell);
    case 'loading': return 'load';
    case 'down': return 'down';
    case 'off': return 'off';
    default: return null;
  }
}

// The run states and their icons (Components board 2): queued, running,
// done, failed, cancelled. Each entry: [name, () => shapes, colour].
const RUN_ICON = {
  queued: ['Queued', () => [h('circle', { cx: 8, cy: 8, r: 6, fill: 'none', stroke: 'currentColor', 'stroke-width': 2 }), h('path', { d: 'M8 4.8V8l2 1.4', fill: 'none', stroke: 'currentColor', 'stroke-width': 1.6, 'stroke-linecap': 'round' })], 'var(--queued)'],
  running: ['Running', () => [h('circle', { cx: 8, cy: 8, r: 7, fill: 'var(--accent)' }), h('path', { d: 'M6.4 5v6l4.8-3z', fill: '#fff' })], null],
  done: ['Done', () => [h('path', { d: 'M3.5 8.5l3 3 6-7', fill: 'none', stroke: 'currentColor', 'stroke-width': 2, 'stroke-linecap': 'round', 'stroke-linejoin': 'round' })], 'var(--quiet)'],
  failed: ['Failed', () => [h('circle', { cx: 8, cy: 8, r: 7, fill: 'var(--failed)' }), h('path', { d: 'M5.5 5.5l5 5M10.5 5.5l-5 5', stroke: '#fff', 'stroke-width': 1.8, 'stroke-linecap': 'round' })], null],
  cancelled: ['Cancelled', () => [h('circle', { cx: 8, cy: 8, r: 6, fill: 'none', stroke: 'currentColor', 'stroke-width': 2 }), h('path', { d: 'M4 12l8-8', stroke: 'currentColor', 'stroke-width': 2, 'stroke-linecap': 'round' })], 'var(--cancelled)'],
};
// A request turned away before it ran is drawn with the Failed icon (it did
// not get an answer); its name for a screen reader says what happened.
RUN_ICON.refused = ['Refused', RUN_ICON.failed[1], null];

// runIcon('done', 14) -> the state's icon alone (the legend of a runs table).
export function runIcon(stateName, size = 16, labelled = false) {
  const r = RUN_ICON[stateName];
  if (!r) return null;
  return h('svg', { viewBox: '0 0 16 16', width: size, height: size, role: labelled ? 'img' : null, 'aria-label': labelled ? r[0] : null,
    'aria-hidden': labelled ? null : 'true', style: r[2] ? 'color:' + r[2] : null }, r[1]());
}

// runStatus('running', '22.6 s', title) -> the design's run status: the
// state's icon (named for a screen reader) and a length or a word. A state
// this file does not know is shown as its word, never guessed at.
export function runStatus(stateName, text, title) {
  const r = RUN_ICON[stateName];
  if (!r) return h('span.rs', { title }, String(stateName || ''), text ? [' ', text] : null);
  return h('span.rs', { title },
    h('svg', { viewBox: '0 0 16 16', role: 'img', 'aria-label': r[0], style: r[2] ? 'color:' + r[2] : null }, r[1]()),
    text != null ? h('span', { class: stateName === 'queued' ? 'mut' : 'mono' }, text) : h('span', r[0]));
}

// sidePanel({title, body, onClose, label, sub, controls, foot}) opens the
// side panel (core/panel.js): beside the page at 1400 px and wider, over it
// below that; one at a time; Escape or the close button closes it and the
// focus goes back to what opened it. body is a node, or a function (close =>
// node) called again by redraw(). onClose is called once when it closes.
// Returns {close(), redraw(), el}.
export function sidePanel(o) {
  return openPanel(o);
}

// ---------- the Overview's components (Main board; Components board 3) ----------

const CHEVRON_L = 'M12 5l-5 5 5 5', CHEVRON_R = 'M8 5l5 5-5 5';
const CALENDAR = [{ tag: 'rect', x: 3, y: 4.5, width: 14, height: 12.5, rx: 2 }, 'M3 8.5h14M7 3v3M13 3v3'];

// dateStepper({label, value, min, max, onPick, onPrev, onNext, prevDisabled,
// nextDisabled, prevLabel, nextLabel}) -> the design's date stepper: a
// previous arrow, a calendar button with the period's name ("Tue 17 Nov 2026
// · today"), a next arrow. The calendar button opens the browser's own date
// picker (value, min and max are YYYY-MM-DD); onPick gets the date chosen.
export function dateStepper(o) {
  const pick = h('input.stepper-pick', { type: 'date', value: o.value || '', min: o.min || null, max: o.max || null, tabindex: '-1', 'aria-hidden': 'true',
    onchange() { if (this.value && o.onPick) o.onPick(this.value); } });
  const open = () => {
    try { if (pick.showPicker) { pick.showPicker(); return; } } catch { /* not allowed here: fall back to focus */ }
    pick.focus();
  };
  return h('div.stepper', { role: 'group', 'aria-label': 'Date' },
    h('button.arrow', { type: 'button', 'aria-label': o.prevLabel || 'Previous', title: o.prevLabel || 'Previous', disabled: !!o.prevDisabled, onclick: o.onPrev }, icon(CHEVRON_L)),
    h('button.when', { type: 'button', title: 'Pick a date', onclick: open }, icon(CALENDAR), o.label),
    h('button.arrow', { type: 'button', 'aria-label': o.nextLabel || 'Next', title: o.nextLabel || 'Next', disabled: !!o.nextDisabled, onclick: o.onNext }, icon(CHEVRON_R)),
    pick);
}

// zonesToggle(on, attrs) -> the "Zones" toggle of a runs row: pressed, it
// marks the request's zones in the zones panel.
export function zonesToggle(on, attrs = {}) {
  return h('button', Object.assign({ class: 'zb' + (on ? ' on' : ''), type: 'button', 'aria-pressed': on ? 'true' : 'false' }, attrs), 'Zones');
}

// runLegend(extra) -> the key of a runs table: the five states with their
// icons, then extra (the "All 203" link).
export function runLegend(extra) {
  return h('div.runkey', ['queued', 'running', 'done', 'failed', 'cancelled'].map(s => h('span', runIcon(s, 14), RUN_ICON[s][0])), extra || null);
}

// tokensLegend(unit) -> "in, out, per hour": the tokens panel's key.
export function tokensLegend(unit) {
  return h('span.tokkey', h('span.sw.tin'), 'in', h('span.sw.tout'), 'out · ' + unit);
}

// tokensChart({tin, tout, buckets: [{in, out, label, title}], ariaIn, ariaOut,
// empty}) -> the two big totals beside mirrored bars, tokens in above the
// line and tokens out below, one bar of each per bucket, with the axis
// labels under them. Each side is scaled to its own largest bucket (the
// design: in 130 px, out 64 px). A bucket with nothing draws no bar.
export function tokensChart(o) {
  const b = o.buckets || [];
  const maxIn = Math.max(0, ...b.map(x => x.in || 0)), maxOut = Math.max(0, ...b.map(x => x.out || 0));
  const bar = (v, max, full) => (v > 0 && max > 0 ? Math.max(2, Math.round((v / max) * full)) : 0);
  const total = v => (v == null ? DASH : num(v));
  return h('div.tokens',
    h('div.tokens-tot',
      h('div', h('div.bign', total(o.tin)), h('div.lbl', 'tokens in')),
      h('div', h('div.bign', total(o.tout)), h('div.lbl', 'tokens out'))),
    h('div.tokens-plot',
      o.empty ? h('div.tokens-empty', o.empty) : null,
      h('div.hbars.in', { role: 'img', 'aria-label': o.ariaIn || 'Tokens in' },
        b.map((x, i) => { const v = bar(x.in, maxIn, 130); return h('i', { key: 'i' + i, title: x.title || null, style: v ? `height:${v}px` : null }); })),
      h('div.hbars-axis'),
      h('div.hbars.out', { role: 'img', 'aria-label': o.ariaOut || 'Tokens out' },
        b.map((x, i) => { const v = bar(x.out, maxOut, 64); return h('i', { key: 'o' + i, title: x.title || null, style: v ? `height:${v}px` : null }); })),
      h('div.hbars-x', { 'aria-hidden': 'true' }, b.map((x, i) => h('span', { key: 'x' + i }, x.label || '')))));
}

// runLength(ms) -> "22.6 s" under a minute, else "1 min 12 s".
export function runLength(ms) {
  if (ms == null || ms < 0) return DASH;
  const s = ms / 1000;
  return s < 60 ? num(s, 1) + ' s' : dur(s);
}

// A rate of tokens per second: one decimal below 100, whole above (as drawn:
// "412" in, "14.1" out).
const rateText = v => (v == null || !(v > 0) ? DASH : v >= 100 ? num(v) : num(v, 1));

// The quantisation in a model file name, as the server reads it (api
// ModelFileFromName): Q4_K_M, IQ3_XS, BF16, F16, F32.
const QUANT = /(^|[-_.])((?:IQ|Q)\d+(?:_[A-Z0-9]+)*|BF16|F16|F32)(?=$|[-_.])/i;

// fileName('/models/Qwen_Qwen3.5-9B-Q4_K_M.gguf') -> 'Qwen_Qwen3.5-9B-Q4_K_M.gguf'.
const fileName = file => String(file || '').split(/[\\/]/).pop();
const TUNE = /[-_.](it|instruct)(?=$|[-_.])/i;

// fileShort('/models/Gemma-4-27B-it-Q4_K_M.gguf') -> 'gemma-4-27b': the
// file name without its directory, the .gguf extension and the quantisation,
// in lower case; "-it" and "-instruct" are dropped unless another file of the
// catalog would then get the same short name (the server's rule for
// short_name, used when the server sends none).
export function fileShort(file) {
  const keep = fileShortKeep(file);
  const dropped = keep.replace(TUNE, '').replace(/[-_.]{2,}/g, m => m[0]);
  if (dropped === keep) return keep;
  const me = fileName(file);
  const files = (catalog.models || []).map(m => m && m.file).filter(f => f && fileName(f) !== me);
  // another file that keeps the same name (another quantisation) is no clash
  return files.some(f => { const k = fileShortKeep(f); return k !== keep && (k === dropped || k.replace(TUNE, '').replace(/[-_.]{2,}/g, m => m[0]) === dropped); }) ? keep : dropped;
}
function fileShortKeep(file) {
  let s = String(file || '').split(/[\\/]/).pop().replace(/\.gguf$/i, '');
  s = s.replace(QUANT, '$1').replace(/[-_.]{2,}/g, m => m[0]).replace(/^[-_.]+|[-_.]+$/g, '');
  // a leading vendor word the next word repeats: "Qwen_Qwen3.5-9B" -> "Qwen3.5-9B" (any case)
  const v = /^([a-z]+)[-_]/i.exec(s);
  if (v && s.slice(v[0].length).toLowerCase().startsWith(v[1].toLowerCase())) s = s.slice(v[0].length);
  return s.toLowerCase();
}


// runModelTitle(r) -> the Model cell's hover: the profile id and the full
// model file name (and the name the client sent, when it was an alias).
export function runModelTitle(r) {
  const entry = r && r.profile ? catalog.byProfile[r.profile] : null;
  const file = fileName((entry && entry.file) || (r && r.model_file));
  const parts = [r && r.profile, file].filter(Boolean);
  if (r && r.requested && r.requested !== r.profile) parts.push('sent as ' + r.requested);
  return parts.join(' · ') || null;
}

// runModel(r) -> the design's model name of a run, "gemma-4-27b:32k": the
// short name of its model file (the catalog entry of its profile, else the
// row's model_file) and its profile's ctx in k (8192 -> 8k). The server's
// short_name (on the row, else on the catalog entry) and the row's ctx win
// when present; a run whose file is not known keeps its profile id.
export function runModel(r) {
  const entry = r && r.profile ? catalog.byProfile[r.profile] : null;
  const file = (entry && entry.file) || (r && r.model_file) || '';
  const sent = (r && typeof r.short_name === 'string' && r.short_name) || (entry && typeof entry.short_name === 'string' && entry.short_name) || '';
  const short = sent || fileShort(file);
  if (!short) return (r && (r.profile || r.requested)) || '';
  const prof = entry && (entry.profiles || []).find(p => p.id === r.profile);
  const c = r && r.ctx > 0 ? r.ctx : prof && prof.ctx > 0 ? prof.ctx : 0;
  const ctx = c ? Math.round(c / 1024) + 'k' : '';
  return ctx ? short + ':' + ctx : short;
}

// promptRate(r) -> {rate, title} for a run's "In tok/s": its prefill_tps,
// or null with a hover when fewer than READ_MIN prompt tokens were read
// (tokens_in minus tokens_reused): a prompt almost all reused from the cache
// is read too fast to say anything about the machine. Used by the run list
// and the run detail.
export const READ_MIN = 16;
export function promptRate(r) {
  const tin = r && r.tokens_in, reused = r && r.tokens_reused, tps = r && r.prefill_tps;
  if (tps != null && tin != null && reused > 0 && tin - reused < READ_MIN) {
    return { rate: null, title: `Almost all of the prompt was reused from the cache (${num(reused)} of ${num(tin)} tokens); measured ${rateText(tps)} tok/s` };
  }
  return { rate: tps == null ? null : tps, title: null };
}
const firstUp = t => (t ? t.charAt(0).toUpperCase() + t.slice(1) : t);

// roleShare(role, share) -> "Host 50%", "Helper 100%", or "Host" alone when
// the machine holds no share of the model (share absent or 0: the
// scheduler's own machine hosting while the others hold the whole model).
// share is a fraction of the model (0.5 is 50 %), as /api/status and the
// request log carry it. An unknown role gives ''.
const ROLES = { host: 'Host', helper: 'Helper' };
export function roleShare(role, share) {
  const r = ROLES[role];
  if (!r) return '';
  return typeof share === 'number' && share > 0 ? `${r} ${Math.round(share * 100)}%` : r;
}

// runMachines(nodes) -> "node-a Host 50%, node-b Helper 50%": the machines
// that served a request (its nodes, host first), each with its role and
// share; '' when there are none.
export function runMachines(nodes) {
  return (nodes || []).map(n => [n.name || n.id, roleShare(n.role, n.share)].filter(Boolean).join(' ')).join(', ');
}

// runWhy(row) -> the tooltip of a row's status: what happened, in words.
export function runWhy(r) {
  const msg = r.message || '';
  switch (r.status) {
    case 'failed': return 'Failed' + (msg ? ': ' + msg : r.reason ? ': ' + r.reason : '');
    case 'refused': return 'Refused' + (msg ? ': ' + msg : r.reason ? ': ' + r.reason : '');
    case 'cancelled': return msg ? firstUp(msg) : 'Cancelled';
    case 'queued': return 'Waiting since ' + clock(r.queued_at);
    case 'running': return r.live ? 'Running; tokens out are counted from the stream until it ends' : 'Running';
    default: return null;
  }
}

// runsTable({rows, now, pin, onPick, onOver, onOut, zones, zonesTitle, extra,
// empty}) -> the design's runs table, 30 px rows: Status and length ·
// Started at · Model · Tokens in · In tok/s · Tokens out · Out tok/s · the
// Zones toggle. rows are request log rows (GET /api/requests). Row states:
// queued ("Not started", "queued 16:08:30"), running (a live length: its
// span carries data-run-start, which tickRuns() advances), done, failed (red
// row, its reason on hover), refused (the Failed icon and row, its reason on
// hover), cancelled (greyed). zones: false leaves the toggle column out;
// extra adds columns [{label, cls, render}] before it.
export function runsTable(o) {
  const rows = o.rows || [];
  const extra = o.extra || [];
  const withZones = o.zones !== false;
  const now = o.now || Date.now();
  // dashTitle: why a running row has no figure yet (it shows one as soon as its row carries it)
  const fig = (v, title, isRate, dashTitle) => {
    const t = v == null || v < 0 ? DASH : isRate ? rateText(v) : num(v);
    return h('td', { class: 'num mono' + (t === DASH ? ' mut' : ''), title: t === DASH ? dashTitle || null : title || null }, t);
  };
  // o.liveTimings: the setting requests_live_timings (false: off, true: on, null: not known)
  const PROMPT_LATER = o.liveTimings === false ? 'Live prompt figures are off: they arrive when the request ends (setting requests_live_timings)'
    : 'The prompt figures arrive from the first token on, or when the request ends';
  return h('table.runs',
    h('thead', h('tr',
      h('th', { scope: 'col' }, 'Status and length'), h('th', { scope: 'col' }, 'Started at'), h('th', { scope: 'col' }, 'Model'),
      h('th.num', { scope: 'col' }, 'Tokens in'), h('th.num', { scope: 'col' }, 'In tok/s'), h('th.num', { scope: 'col' }, 'Tokens out'), h('th.num', { scope: 'col' }, 'Out tok/s'),
      extra.map(c => h('th', { scope: 'col', class: c.cls || null }, c.label)),
      withZones ? h('th', { scope: 'col' }, h('span.sr', 'Zones')) : null)),
    h('tbody', rows.length ? rows.map(r => {
      const st = r.status;
      const notStarted = st === 'queued' || !r.started_at;
      let text;
      if (st === 'queued') text = 'Not started';
      else if (st === 'running') text = runLength(now - (r.started_at || r.queued_at));
      else if (notStarted) text = runLength(r.wait_ms);
      else text = runLength(r.duration_ms);
      const status = runStatus(st, text, runWhy(r));
      if (st === 'running' && status.lastChild) {
        status.lastChild.setAttribute('data-run-start', String(r.started_at || r.queued_at));
        // drawn by us: morph() removes it when the row has ended (core/dom.js keeps only foreign attributes)
        if (status.lastChild.__own) status.lastChild.__own.add('data-run-start');
      }
      const waiting = st === 'queued';
      const id = String(r.id);
      return h('tr', { key: 'r' + id, 'data-req': id, class: [st === 'failed' || st === 'refused' ? 'fail' : st === 'cancelled' ? 'canc' : '', o.pin === id ? 'pinned' : ''].filter(Boolean).join(' ') || null },
        h('td', status),
        notStarted ? h('td.mut.qd', 'queued ', h('span.mono', clock(r.queued_at))) : h('td.mono', clock(r.started_at)),
        // o.modelClass: a page that lets the Model cell wrap (Requests: rq-model)
        h('td', { class: 'mono' + (o.modelClass ? ' ' + o.modelClass : ''), title: runModelTitle(r) }, runModel(r) || DASH),
        fig(waiting ? null : r.tokens_in, r.tokens_reused > 0 ? `${num(r.tokens_reused)} of them reused from the cache` : null, false, st === 'running' ? PROMPT_LATER : null),
        (pr => fig(waiting ? null : pr.rate, null, true, pr.title || (st === 'running' ? PROMPT_LATER : null)))(promptRate(r)),
        fig(waiting ? null : r.tokens_out, r.live ? 'Counted from the stream so far' : null),
        fig(waiting ? null : r.decode_tps, null, true),
        extra.map(c => h('td', { class: c.cls || null }, c.render(r))),
        withZones ? h('td.num', zonesToggle(o.pin === id, {
          'data-zones': id, title: o.zonesTitle ? o.zonesTitle(r) : null,
          onclick: () => o.onPick && o.onPick(id), onmouseenter: () => o.onOver && o.onOver(id), onmouseleave: () => o.onOut && o.onOut(),
          onfocus: () => o.onOver && o.onOver(id), onblur: () => o.onOut && o.onOut() })) : null);
    }) : o.empty ? h('tr.none', { key: 'none' }, h('td', { colspan: String(7 + extra.length + (withZones ? 1 : 0)) }, o.empty)) : null));
}

// tickRuns(root) sets the live length of every running row under root from
// its start (data-run-start), without a redraw. The Overview calls it every
// 100 ms.
export function tickRuns(root, nowMs) {
  const t = nowMs || Date.now();
  for (const el of root.querySelectorAll('[data-run-start]')) {
    const v = runLength(t - Number(el.getAttribute('data-run-start')));
    if (el.textContent !== v) el.textContent = v;
  }
}

// groupClass(ids, order) -> {id: 'hi g-top' | 'hi g-mid' | 'hi g-bot' |
// 'hi g-solo'}: how each highlighted row joins its neighbours into one
// outlined box (the design's zone highlight). order is the drawn order.
export function groupClass(ids, order) {
  const on = new Set(ids), out = {};
  order.forEach((z, i) => {
    if (!on.has(z)) return;
    const up = i > 0 && on.has(order[i - 1]);
    const dn = i < order.length - 1 && on.has(order[i + 1]);
    out[z] = 'hi ' + (up && dn ? 'g-mid' : up ? 'g-bot' : dn ? 'g-top' : 'g-solo');
  });
  return out;
}

// dropButton(label, [{id, label}], current, id => {}, key) -> the design's
// filter dropdown as drawn on the Overview: a flat button that reads
// "Sort: needs attention first" and opens a short menu of the choices under
// it. One menu is open at a time; Escape, a choice or a click elsewhere
// closes it. The open menu survives a redraw (it is drawn from the module's
// own state), so a live page does not close it under the pointer.
let openDrop = null;
const dropRedraws = new Set();
function closeDrop() {
  if (!openDrop) return;
  openDrop = null;
  for (const fn of Array.from(dropRedraws)) fn();
}
let dropWired = false;
function wireDrop() {
  if (dropWired || typeof document === 'undefined') return;
  dropWired = true;
  document.addEventListener('mousedown', e => { if (openDrop && !(e.target && e.target.closest && e.target.closest('.dd'))) closeDrop(); }, true);
  document.addEventListener('keydown', e => { if (e.key === 'Escape' && openDrop) closeDrop(); }, true);
}
// onDropChange(fn) -> unsubscribe: fn() after a menu opens or closes (a view redraws).
// The unsubscribe also closes an open menu: the view that drew it is gone.
export function onDropChange(fn) { dropRedraws.add(fn); return () => { dropRedraws.delete(fn); openDrop = null; }; }
export function dropButton(label, options, current, onPick, key) {
  wireDrop();
  const id = key || 'dd-' + label;
  const cur = options.find(o => o.id === current) || options[0];
  const open = openDrop === id;
  return h('span.dd', { key: id },
    h('button', { class: 'fs bare' + (open ? ' open' : ''), type: 'button', 'aria-haspopup': 'menu', 'aria-expanded': open ? 'true' : 'false', 'data-dd': id,
      onclick: () => { openDrop = open ? null : id; for (const fn of Array.from(dropRedraws)) fn(); } }, label + ': ' + (cur ? cur.label : '')),
    open ? h('span.dd-menu', { role: 'menu', 'aria-label': label }, options.map(o =>
      h('button', { type: 'button', role: 'menuitemradio', 'aria-checked': o.id === (cur && cur.id) ? 'true' : 'false', 'data-dd-opt': o.id,
        onclick: () => { openDrop = null; onPick(o.id); for (const fn of Array.from(dropRedraws)) fn(); } }, o.label))) : null);
}
