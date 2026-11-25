// The side panel (design: the panel of every artboard that has one: Zones
// "A zone clicked", Models, Benchmarks "A group clicked", MCP "Manage
// access", Alerts "A webhook clicked"; their .side). 620 wide, white, a
// border and a soft shadow on its left; a header with a small label
// ("Selected zone"), the title, one line under it, the panel's own controls
// (a switch) and the close button; then a scroll area; then, when given, a
// footer with the actions (.sf).
//
// Where it sits (Q72): at 1400 px and wider it sits BESIDE the page, as
// every artboard draws it: the page narrows and keeps working, there is no
// scrim, and the focus is not held in the panel. Below that width it sits
// OVER the page behind a scrim, as a modal dialog that keeps the focus
// inside. A panel open while the window is resized moves between the two.
//
// One panel at a time: opening one closes the one that is open. Escape
// closes it (unless a dialog or the health popup is open above it); the
// close button too; then the focus returns to what had it when the panel
// opened (the row or button that opened it).
//
//   const p = openPanel({label, title, sub, controls, body, foot, onClose, name});
//   p.redraw(); p.close(); p.el; p.docked()
//
// title, sub, controls, body and foot are nodes or functions (close => node),
// drawn again by redraw(). onClose is called once, after it closed. name is
// what a screen reader calls the panel (default: label and title).
// o.closesMenu: while this panel sits BESIDE the page, the section menu is
// closed (as the health popup does; the MCP boards draw no menu beside a
// docked panel); the shell brings it back as it was when the panel closes
// or moves over the page. onPanelHold(fn) / panelHoldsMenu() are the shell's.
//
// Its width (the owner's note 6, 25 Nov): while it sits beside the page its
// left edge is a handle (role separator, focusable): drag it, or use the
// arrow keys (left widens, right narrows; Shift for bigger steps; Home and
// End for the least and the most); a double click goes back to 620. The
// page beside it reflows. The width is remembered per panel kind (o.kind,
// else its label) in localStorage when the browser allows it; the panel
// works the same without. Between 420 px and what leaves the page 560 px
// beside the rail (at most 1000). Over the page there is no handle.

import { h, morph } from './dom.js';

export const DOCK_MIN = 1400;
const wide = window.matchMedia('(min-width:' + DOCK_MIN + 'px)');

export const SIDE_W = 620, SIDE_MIN = 420;
const SIDE_MAX = 1000, PAGE_KEEP = 560, RAIL = 60, STEP = 16, BIG_STEP = 64;
// sideMax() -> the widest a docked panel may be at this window width
const sideMax = () => Math.max(SIDE_MIN, Math.min(SIDE_MAX, (window.innerWidth || 0) - RAIL - PAGE_KEEP));
const clampW = w => Math.round(Math.min(sideMax(), Math.max(SIDE_MIN, w)));
const widthKey = kind => 'potluck.panel-width.' + kind;
function readWidth(kind) {
  try { const v = Number(localStorage.getItem(widthKey(kind))); return v > 0 ? v : null; } catch { return null; }
}
function saveWidth(kind, w) {
  try { if (w == null) localStorage.removeItem(widthKey(kind)); else localStorage.setItem(widthKey(kind), String(w)); } catch { /* not allowed here: the width lasts while the panel is open */ }
}

let current = null;
let held = false;
const holdFns = new Set();
export const panelHoldsMenu = () => held;
export const onPanelHold = fn => { holdFns.add(fn); };
function setHeld(v) { if (held !== v) { held = v; for (const fn of holdFns) fn(); } }
// openPanel(o) -> {close, redraw, el, docked}; o.focus: a selector focused on open
export function openPanel(o) {
  if (current) current.close();
  const prev = document.activeElement;
  let closed = false;
  const id = 'side-' + Date.now() + '-' + Math.floor(Math.random() * 1e6);
  const closeBtn = h('button.x', { type: 'button', 'aria-label': 'Close' + (o.label ? ' ' + String(o.label).toLowerCase() : ''), title: 'Close', onclick: () => close() },
    h('svg', { class: 'ic', viewBox: '0 0 20 20', 'aria-hidden': 'true' }, h('path', { d: 'M5 5l10 10M15 5L5 15' })));
  const label = h('div.lbl.side-label');
  const title = h('h2.side-title', { id });
  const sub = h('div.side-sub');
  const controls = h('div.side-ctl');
  const body = h('div.side-body');
  const foot = h('div.sf');
  const pane = h('aside.side', { 'aria-labelledby': id },
    h('div.side-head', h('div.side-names', label, title, sub), controls, closeBtn), body, foot);

  // the width handle (docked only): drag, arrow keys, a double click resets
  const kind = o.kind || (typeof o.label === 'string' && o.label) || 'panel';
  let width = readWidth(kind) || SIDE_W;
  const grip = h('div.side-grip', { role: 'separator', tabindex: '0', 'aria-orientation': 'vertical', 'aria-label': 'Panel width: drag, or use the arrow keys',
    title: 'Drag to resize · double click for the usual width' });
  const setWidth = (w, keep) => {
    width = clampW(w);
    if (!pane.classList.contains('docked')) return;
    pane.style.width = width + 'px';
    pane.setAttribute('data-w', String(width));
    grip.setAttribute('aria-valuenow', String(width));
    grip.setAttribute('aria-valuemin', String(SIDE_MIN));
    grip.setAttribute('aria-valuemax', String(sideMax()));
    if (keep) saveWidth(kind, width === SIDE_W ? null : width);
  };
  grip.addEventListener('keydown', e => {
    const step = e.shiftKey ? BIG_STEP : STEP;
    const to = { ArrowLeft: width + step, ArrowRight: width - step, Home: SIDE_MIN, End: sideMax() }[e.key];
    if (to == null) return;
    e.preventDefault(); e.stopPropagation();
    setWidth(to, true);
  });
  grip.addEventListener('dblclick', () => setWidth(SIDE_W, true));
  grip.addEventListener('pointerdown', e => {
    if (e.button != null && e.button !== 0) return;
    e.preventDefault();
    const x0 = e.clientX, w0 = width;
    pane.classList.add('resizing');
    const move = ev => setWidth(w0 + (x0 - ev.clientX), false);
    const up = () => {
      window.removeEventListener('pointermove', move); window.removeEventListener('pointerup', up); window.removeEventListener('pointercancel', up);
      pane.classList.remove('resizing');
      setWidth(width, true);
    };
    window.addEventListener('pointermove', move); window.addEventListener('pointerup', up); window.addEventListener('pointercancel', up);
  });
  const back = h('div.side-back', { onmousedown: e => { if (e.target === back) close(); } });

  const val = v => (typeof v === 'function' ? v(close) : v);
  const fill = () => {
    if (closed) return;
    const put = (el, v) => { const x = val(v); morph(el, x == null ? [] : x); if (x == null || (Array.isArray(x) && !x.length)) el.setAttribute('hidden', ''); else el.removeAttribute('hidden'); };
    put(label, o.label);
    const lt = val(o.label);
    closeBtn.setAttribute('aria-label', 'Close' + (typeof lt === 'string' && lt ? ' ' + lt.toLowerCase() : ''));
    morph(title, val(o.title) == null ? [] : val(o.title));
    put(sub, o.sub);
    put(controls, o.controls);
    const b = val(o.body);
    morph(body, b == null ? [] : b);
    put(foot, o.foot);
    if (o.name) pane.setAttribute('aria-label', o.name);
  };

  // beside the page (in the app's row, after the content) or over it
  const place = () => {
    if (closed) return;
    const app = document.getElementById('app');
    const docked = wide.matches && !!app;
    pane.classList.toggle('docked', docked);
    setHeld(docked && !!o.closesMenu);
    if (docked) { if (grip.parentNode !== pane) pane.insertBefore(grip, pane.firstChild); setWidth(width, false); }
    else { grip.remove(); pane.style.width = ''; pane.removeAttribute('data-w'); }
    if (docked) {
      pane.removeAttribute('role'); pane.removeAttribute('aria-modal');
      back.remove();
      if (pane.parentNode !== app) app.appendChild(pane);
    } else {
      pane.setAttribute('role', 'dialog'); pane.setAttribute('aria-modal', 'true');
      if (pane.parentNode !== back) back.appendChild(pane);
      if (back.parentNode !== document.body) document.body.appendChild(back);
    }
  };
  const onWidth = () => place();
  // a window resized while docked: the width keeps within the new maximum
  const onResize = () => { if (pane.classList.contains('docked')) setWidth(readWidth(kind) || width, false); };

  const onKey = e => {
    if (document.querySelector('.backdrop')) return; // a dialog above the panel handles its own keys
    if (e.key === 'Escape') {
      if (document.querySelector('.surfaces > *')) return; // the health popup or the Ask drawer closes first
      e.stopPropagation();
      close();
      return;
    }
    // over the page, the focus stays in the panel
    if (e.key === 'Tab' && !pane.classList.contains('docked')) {
      const f = pane.querySelectorAll('button,input,select,textarea,a[href]');
      if (!f.length) return;
      const first = f[0], last = f[f.length - 1];
      if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
    }
  };

  function close() {
    if (closed) return;
    closed = true;
    if (current === api) { current = null; setHeld(false); }
    document.removeEventListener('keydown', onKey, true);
    if (wide.removeEventListener) wide.removeEventListener('change', onWidth);
    window.removeEventListener('resize', onResize);
    pane.remove();
    back.remove();
    if (prev && prev.focus && prev !== document.body) try { prev.focus(); } catch { /* gone */ }
    if (o.onClose) o.onClose();
  }

  const api = { close, redraw: fill, el: pane, docked: () => pane.classList.contains('docked') };
  fill();
  place();
  if (wide.addEventListener) wide.addEventListener('change', onWidth);
  window.addEventListener('resize', onResize);
  document.addEventListener('keydown', onKey, true);
  // the first field when the panel asks for one (o.focus: a css selector), else the close button
  const first = o.focus ? pane.querySelector(o.focus) : null;
  (first || closeBtn).focus();
  current = api;
  return api;
}

// openPanelNow() -> the open panel's handle, or null.
export const openPanelNow = () => current;
