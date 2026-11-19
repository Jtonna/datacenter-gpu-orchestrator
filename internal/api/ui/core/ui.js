// Shared components. Each returns a DOM node built with h(); the markup and
// the rules are documented in ../README.md. Views compose these and re-render
// with liveView() (or morph()) when the store changes.

import { h, morph } from './dom.js';
import { on } from './store.js';
import { num, mib, parseGoDuration } from './format.js';

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
  const label = `${mib(mem.total_mib)} for this model on ${name}` + (budget ? `, of ${mib(budget)} usable` : '');
  return h('div.nr', { key: 'mem-' + name },
    h('span.mono', name, extra ? [' ', extra] : null),
    bar([{ cls: 'm', value: mem.model_mib, title: 'Model ' + mib(mem.model_mib) }, { cls: 'k', value: mem.kv_mib, title: 'KV cache ' + mib(mem.kv_mib) },
      { cls: 'c', value: mem.compute_mib, title: 'Compute ' + mib(mem.compute_mib) }, { cls: 'o', value: other, title: 'Other ' + mib(other) }], budget, label),
    h('span.mono.fig', budget ? `${num(mem.total_mib)} / ${mib(budget)}` : mib(mem.total_mib)),
    h('span.mono.mut', `${num(mem.model_mib)} model · ${num(mem.kv_mib)} KV · ${num(mem.compute_mib)} compute`));
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
export function chips(options, current, onPick, label) {
  return h('div.chips', { role: 'group', 'aria-label': label || 'Filter' }, options.map(o =>
    h('button', { class: 'fc' + (o.id === current ? ' on' : ''), type: 'button', 'aria-pressed': o.id === current ? 'true' : 'false', onclick: () => onPick(o.id) },
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
// dialog({title, body, actions:[{label, kind, value, autofocus}], wide}) resolves
// with the picked action's value (undefined on Escape or a backdrop click).
// body may be a node or a function (close => node) for a body with its own controls.
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
    const dlg = h('div', { class: 'dlg' + (o.wide ? ' wide' : ''), role: 'dialog', 'aria-modal': 'true', 'aria-labelledby': id },
      h('h2', { id }, o.title),
      typeof o.body === 'function' ? o.body(close) : o.body,
      o.actions ? h('div.dlg-foot', o.actions.map(a => {
        const b = h('button', { class: 'btn lg ' + (a.kind || ''), type: 'button', onclick: () => close(typeof a.value === 'function' ? a.value() : a.value) }, a.label);
        if (a.autofocus) b.dataset.autofocus = '1';
        return b;
      })) : null);
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
      h(f.type === 'bool' || f.type === 'list' ? 'div.lab' : 'label', { for: f.type === 'bool' || f.type === 'list' ? null : id }, f.label || humanize(f.key), m),
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
