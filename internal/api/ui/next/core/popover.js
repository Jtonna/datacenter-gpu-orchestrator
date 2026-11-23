// The anchored popover (design: the System health popup on the Health
// board, .hp, and the "Answer ready" card on Ask Potluck's fourth board,
// .pop): a white box with a 1 px #c5cec9 border, a 12 px radius, a soft
// shadow and a small arrow on its left pointing at the rail button it
// belongs to. It opens beside its anchor, 10 px to its right, its arrow at
// the anchor's middle; it is kept inside the window.
//
//   const p = popover({anchor, content, label, width, role, onClose});
//   p.close(); p.redraw(); p.el
//
// content is a node or a function (close => node), drawn again by redraw().
// Escape, a click outside it (not on its anchor) or close() closes it; the
// focus then returns to the anchor when it was inside the popover. One
// popover at a time. role is "dialog" (the default; focus moves into it) or
// "status" (a notice like "Answer ready": the focus stays where it is).

import { h, morph } from './dom.js';

const GAP = 10, ARROW_AT = 30, EDGE = 10;
let current = null;

export function popover(o) {
  if (current) current.close();
  let closed = false;
  const role = o.role || 'dialog';
  const el = h('div.pop', { role, 'aria-label': o.label || null, style: 'width:' + (o.width || 300) + 'px' });
  const fill = () => {
    if (closed) return;
    const c = typeof o.content === 'function' ? o.content(close) : o.content;
    morph(el, c == null ? [] : c);
  };
  // beside the anchor, the arrow at its middle, inside the window
  const place = () => {
    const a = o.anchor && o.anchor.getBoundingClientRect ? o.anchor.getBoundingClientRect() : null;
    if (!a) return;
    const mid = a.top + a.height / 2;
    const hgt = el.offsetHeight || 0;
    const top = Math.max(EDGE, Math.min(mid - ARROW_AT, (window.innerHeight || 0) - hgt - EDGE));
    el.style.left = Math.round(a.right + GAP) + 'px';
    el.style.top = Math.round(top) + 'px';
    el.style.setProperty('--arrow-y', Math.round(mid - top - 5) + 'px');
  };
  const onKey = e => { if (e.key === 'Escape' && !document.querySelector('.backdrop')) { e.stopPropagation(); close(); } };
  const onDown = e => {
    const t = e.target;
    if (!t || (el.contains && el.contains(t)) || (o.anchor && o.anchor.contains && o.anchor.contains(t))) return;
    close();
  };
  function close() {
    if (closed) return;
    closed = true;
    if (current === api) current = null;
    document.removeEventListener('keydown', onKey, true);
    document.removeEventListener('mousedown', onDown, true);
    window.removeEventListener('resize', place);
    const hadFocus = el.contains && el.contains(document.activeElement);
    el.remove();
    if (o.anchor) o.anchor.setAttribute('aria-expanded', 'false');
    if (hadFocus && o.anchor && o.anchor.focus) o.anchor.focus();
    if (o.onClose) o.onClose();
  }
  const api = { close, redraw: () => { fill(); place(); }, el };
  fill();
  document.body.appendChild(el);
  place();
  if (o.anchor) o.anchor.setAttribute('aria-expanded', 'true');
  document.addEventListener('keydown', onKey, true);
  document.addEventListener('mousedown', onDown, true);
  window.addEventListener('resize', place);
  if (role === 'dialog') {
    const f = el.querySelector('button,a[href],input,select,textarea');
    if (f) f.focus();
  }
  current = api;
  return api;
}
