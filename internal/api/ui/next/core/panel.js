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

import { h, morph } from './dom.js';

export const DOCK_MIN = 1400;
const wide = window.matchMedia('(min-width:' + DOCK_MIN + 'px)');

let current = null;
// openPanel(o) -> {close, redraw, el, docked}
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
  const back = h('div.side-back', { onmousedown: e => { if (e.target === back) close(); } });

  const val = v => (typeof v === 'function' ? v(close) : v);
  const fill = () => {
    if (closed) return;
    const put = (el, v) => { const x = val(v); morph(el, x == null ? [] : x); if (x == null || (Array.isArray(x) && !x.length)) el.setAttribute('hidden', ''); else el.removeAttribute('hidden'); };
    put(label, o.label);
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
    if (current === api) current = null;
    document.removeEventListener('keydown', onKey, true);
    if (wide.removeEventListener) wide.removeEventListener('change', onWidth);
    pane.remove();
    back.remove();
    if (prev && prev.focus && prev !== document.body) try { prev.focus(); } catch { /* gone */ }
    if (o.onClose) o.onClose();
  }

  const api = { close, redraw: fill, el: pane, docked: () => pane.classList.contains('docked') };
  fill();
  place();
  if (wide.addEventListener) wide.addEventListener('change', onWidth);
  document.addEventListener('keydown', onKey, true);
  closeBtn.focus();
  current = api;
  return api;
}

// openPanelNow() -> the open panel's handle, or null.
export const openPanelNow = () => current;
