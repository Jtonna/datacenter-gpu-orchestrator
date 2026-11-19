// DOM helpers: h() builds real elements, morph() updates a rendered tree in
// place so focus, scroll position and CSS transitions survive a re-render.

const SVG_NS = 'http://www.w3.org/2000/svg';
const SVG_TAGS = new Set(['svg', 'path', 'circle', 'rect', 'line', 'polyline', 'polygon', 'g', 'text', 'title', 'defs', 'linearGradient', 'stop']);

function dispatch(e) {
  const fn = this.__h && this.__h[e.type];
  if (fn) fn.call(this, e);
}

function append(el, c) {
  if (c == null || c === false || c === true) return;
  if (Array.isArray(c)) { for (const x of c) append(el, x); return; }
  el.appendChild(c instanceof Node ? c : document.createTextNode(String(c)));
}

// h('div.card', {onclick: fn, title: 'x'}, child, [children], 'text')
// The tag may carry classes ("span.b.b-ready"). Attributes: on<event> sets a
// handler; class/className; style as a string; a false or null value is left
// out; true sets an empty attribute; "key" marks the node for morph().
export function h(tag, attrs, ...children) {
  const parts = tag.split('.');
  const name = parts[0] || 'div';
  const el = SVG_TAGS.has(name) ? document.createElementNS(SVG_NS, name) : document.createElement(name);
  let cls = parts.slice(1).join(' ');
  if (attrs && (typeof attrs !== 'object' || attrs instanceof Node || Array.isArray(attrs))) {
    children.unshift(attrs);
    attrs = null;
  }
  if (attrs) {
    for (const k in attrs) {
      const v = attrs[k];
      if (v == null || v === false) continue;
      if (k === 'class' || k === 'className') cls = cls ? cls + ' ' + v : v;
      else if (k.startsWith('on') && typeof v === 'function') {
        const type = k.slice(2).toLowerCase();
        (el.__h || (el.__h = {}))[type] = v;
        el.addEventListener(type, dispatch);
      } else if (k === 'key') el.__key = v;
      else if (k === 'value' && 'value' in el && name !== 'option') { el.value = v; el.setAttribute('value', v); }
      else if (k === 'checked' || k === 'selected' || k === 'disabled') { if (v) { el[k] = true; el.setAttribute(k, ''); } }
      else el.setAttribute(k, v === true ? '' : v);
    }
  }
  if (cls) el.setAttribute('class', cls);
  append(el, children);
  return el;
}

export const frag = (...children) => { const f = document.createDocumentFragment(); append(f, children); return f; };

function sameKind(a, b) {
  return a.nodeType === b.nodeType && a.nodeName === b.nodeName && a.__key === b.__key;
}

function syncAttrs(from, to) {
  for (const a of Array.from(from.attributes)) if (!to.hasAttribute(a.name)) from.removeAttribute(a.name);
  for (const a of Array.from(to.attributes)) if (from.getAttribute(a.name) !== a.value) from.setAttribute(a.name, a.value);
  const oldH = from.__h || {}, newH = to.__h || {};
  for (const t in newH) if (!oldH[t]) from.addEventListener(t, dispatch);
  from.__h = newH;
  const editing = document.activeElement === from;
  if ('value' in to && from.nodeName !== 'OPTION' && !editing && from.value !== to.value) from.value = to.value;
  if ('checked' in to && from.checked !== to.checked) from.checked = to.checked;
  if (from.nodeName === 'OPTION' && from.selected !== to.selected) from.selected = to.selected;
}

// morph(parent, next) makes parent's children equal to next (a node, an
// array of nodes or a fragment), reusing the nodes that are already there.
// A node with data-keep is never touched after it was first inserted (use it
// for a subtree the view manages by hand, such as a form being edited).
export function morph(parent, next) {
  const want = [];
  append({ appendChild: n => { if (n.nodeType === 11) want.push(...n.childNodes); else want.push(n); } }, next);
  let cur = parent.firstChild;
  for (const n of want) {
    if (cur && sameKind(cur, n)) {
      if (cur.nodeType === 3) { if (cur.nodeValue !== n.nodeValue) cur.nodeValue = n.nodeValue; }
      else if (cur.nodeType === 1 && !cur.hasAttribute('data-keep')) { syncAttrs(cur, n); morph(cur, Array.from(n.childNodes)); }
      cur = cur.nextSibling;
    } else {
      // look ahead for a keyed match before replacing
      let found = null;
      if (n.__key != null) for (let s = cur; s; s = s.nextSibling) if (sameKind(s, n)) { found = s; break; }
      if (found) {
        parent.insertBefore(found, cur);
        if (!found.hasAttribute('data-keep')) { syncAttrs(found, n); morph(found, Array.from(n.childNodes)); }
      } else parent.insertBefore(n, cur);
    }
  }
  while (cur) { const nx = cur.nextSibling; parent.removeChild(cur); cur = nx; }
}

// icon('M3 5h14...') draws an 18 px stroked icon on a 20 px grid.
export function icon(d, extra) {
  const svg = h('svg', { class: 'ic', viewBox: '0 0 20 20', 'aria-hidden': 'true' });
  for (const p of [].concat(d)) {
    if (typeof p === 'string') svg.appendChild(h('path', { d: p }));
    else svg.appendChild(h(p.tag, p));
  }
  if (extra) for (const k in extra) svg.setAttribute(k, extra[k]);
  return svg;
}
