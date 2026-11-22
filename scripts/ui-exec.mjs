#!/usr/bin/env node
// ui-exec.mjs: runs screens of the web UI (internal/api/ui) under node, on a
// small fake DOM, and reports every runtime error. `node --check` only parses
// a module; this executes the rendering, so a reference to an undeclared name
// or a field read on undefined is found without a browser.
//
// usage: node scripts/ui-exec.mjs cases.json
//
// cases.json:
//   {"ui": "<path of internal/api/ui>",
//    "modules": ["views/nodes.js", ...],          // imported before the cases (they register their views)
//    "fetch": {"/api/profiles": {...}, ...},       // answers of GET/POST by path (query ignored); others are 404
//    "base": "http://127.0.0.1:8790",              // or: GET requests go to this server (cmd/devcluster), writes are refused
//    "cases": [{"name": "...", "route": "#/nodes", "status": {...} or "fetch", "fetch": {...}, "expect": ["text", ...]}]}
//
// With "base", "status": "fetch" takes GET <base>/api/status.
//
// Each case sets the live status, mounts the route's view, lets fetches and
// re-draws settle, and fails on any console.error, unhandled rejection, a
// "could not be drawn" / "failed to start" banner, or an expected text that
// is missing. Output: one line per case; exit status 1 when any case fails.

import { readFileSync } from 'node:fs';
import { pathToFileURL } from 'node:url';
import path from 'node:path';

// ---------- fake DOM ----------

class FakeEvent {
  constructor(type, init = {}) { Object.assign(this, init); this.type = type; this.defaultPrevented = false; }
  preventDefault() { this.defaultPrevented = true; }
  stopPropagation() {}
  stopImmediatePropagation() {}
}

class Node {
  constructor(type, name) {
    this.nodeType = type; this.nodeName = name; this.childNodes = []; this.parentNode = null; this.__l = {};
  }
  get firstChild() { return this.childNodes[0] || null; }
  get lastChild() { return this.childNodes[this.childNodes.length - 1] || null; }
  get parentElement() { return this.parentNode && this.parentNode.nodeType === 1 ? this.parentNode : null; }
  get nextSibling() { const p = this.parentNode; if (!p) return null; return p.childNodes[p.childNodes.indexOf(this) + 1] || null; }
  get previousSibling() { const p = this.parentNode; if (!p) return null; return p.childNodes[p.childNodes.indexOf(this) - 1] || null; }
  get children() { return this.childNodes.filter(c => c.nodeType === 1); }
  get firstElementChild() { return this.children[0] || null; }
  get isConnected() { let n = this; while (n.parentNode) n = n.parentNode; return n === document || n === document.body; }
  get textContent() { return this.nodeType === 3 ? this.nodeValue : this.childNodes.map(c => c.textContent).join(''); }
  set textContent(v) { if (this.nodeType === 3) this.nodeValue = String(v); else { this.replaceChildren(); if (v != null && v !== '') this.appendChild(new Text(String(v))); } }
  get innerText() { return this.textContent; }
  set innerText(v) { this.textContent = v; }
  get innerHTML() { return this.textContent; }
  set innerHTML(v) { this.textContent = String(v).replace(/<[^>]*>/g, ''); }
  _take(n) {
    const list = n.nodeType === 11 ? n.childNodes.slice() : [n];
    for (const c of list) if (c.parentNode) c.parentNode.removeChild(c);
    return list;
  }
  appendChild(n) { for (const c of this._take(n)) { c.parentNode = this; this.childNodes.push(c); } return n; }
  insertBefore(n, ref) {
    if (!ref) return this.appendChild(n);
    const list = this._take(n);
    const i = this.childNodes.indexOf(ref);
    if (i < 0) throw new Error('insertBefore: the reference node is not a child');
    for (const c of list) c.parentNode = this;
    this.childNodes.splice(i, 0, ...list);
    return n;
  }
  removeChild(n) {
    const i = this.childNodes.indexOf(n);
    if (i < 0) throw new Error('removeChild: not a child');
    this.childNodes.splice(i, 1); n.parentNode = null; return n;
  }
  replaceChild(n, old) { this.insertBefore(n, old); return this.removeChild(old); }
  replaceChildren(...ns) { for (const c of this.childNodes) c.parentNode = null; this.childNodes = []; this.append(...ns); }
  append(...ns) { for (const n of ns) this.appendChild(n instanceof Node ? n : new Text(String(n))); }
  prepend(...ns) { const f = this.firstChild; for (const n of ns) this.insertBefore(n instanceof Node ? n : new Text(String(n)), f); }
  remove() { if (this.parentNode) this.parentNode.removeChild(this); }
  before(...ns) { const p = this.parentNode; if (p) for (const n of ns) p.insertBefore(n instanceof Node ? n : new Text(String(n)), this); }
  after(...ns) { const p = this.parentNode; if (!p) return; let ref = this.nextSibling; for (const n of ns) p.insertBefore(n instanceof Node ? n : new Text(String(n)), ref); }
  replaceWith(...ns) { const p = this.parentNode; if (!p) return; this.before(...ns); this.remove(); }
  contains(n) { for (; n; n = n.parentNode) if (n === this) return true; return false; }
  hasChildNodes() { return this.childNodes.length > 0; }
  cloneNode(deep) {
    let c;
    if (this.nodeType === 3) c = new Text(this.nodeValue);
    else if (this.nodeType === 11) c = new DocumentFragment();
    else { c = new Element(this.nodeName.toLowerCase(), this.namespaceURI); for (const [k, v] of this._a) c._a.set(k, v); }
    if (deep) for (const k of this.childNodes) c.appendChild(k.cloneNode(true));
    return c;
  }
  addEventListener(t, fn) { (this.__l[t] || (this.__l[t] = new Set())).add(fn); }
  removeEventListener(t, fn) { this.__l[t] && this.__l[t].delete(fn); }
  dispatchEvent(e) { e.target = e.target || this; e.currentTarget = this; for (const fn of Array.from(this.__l[e.type] || [])) (typeof fn === 'function' ? fn.call(this, e) : fn.handleEvent(e)); return !e.defaultPrevented; }
}

class Text extends Node {
  constructor(v) { super(3, '#text'); this.nodeValue = v; }
  get data() { return this.nodeValue; }
  set data(v) { this.nodeValue = v; }
}

class DocumentFragment extends Node { constructor() { super(11, '#document-fragment'); } }

// Simple selectors only: tag, #id, .class, [attr] and [attr=value], combined
// in one compound (div.card[data-x]), and lists with commas. Descendant
// combinators match any ancestor.
function compile(sel) {
  return sel.split(',').map(s => s.trim()).filter(Boolean).map(s => s.split(/\s+/).map(part => {
    const m = { tag: null, id: null, cls: [], attrs: [] };
    const re = /([a-zA-Z][\w-]*)|#([\w-]+)|\.([\w-]+)|\[([\w-]+)(?:([~^$*|]?=)["']?([^"'\]]*)["']?)?\]|:([\w-]+)(\([^)]*\))?/g;
    let x;
    while ((x = re.exec(part))) {
      if (x[1]) m.tag = x[1].toUpperCase();
      else if (x[2]) m.id = x[2];
      else if (x[3]) m.cls.push(x[3]);
      else if (x[4]) m.attrs.push([x[4], x[5], x[6]]);
    }
    return m;
  }));
}
function one(el, m) {
  if (el.nodeType !== 1) return false;
  if (m.tag && el.nodeName !== m.tag) return false;
  if (m.id && el.getAttribute('id') !== m.id) return false;
  const cl = (el.getAttribute('class') || '').split(/\s+/);
  for (const c of m.cls) if (!cl.includes(c)) return false;
  for (const [k, op, v] of m.attrs) {
    if (!el.hasAttribute(k)) return false;
    if (op === '=' && el.getAttribute(k) !== v) return false;
  }
  return true;
}
function matchesChain(el, chain) {
  if (!one(el, chain[chain.length - 1])) return false;
  let i = chain.length - 2;
  for (let p = el.parentNode; p && i >= 0; p = p.parentNode) if (one(p, chain[i])) i--;
  return i < 0;
}

class Element extends Node {
  constructor(tag, ns) {
    super(1, ns ? tag : tag.toUpperCase());
    this.namespaceURI = ns || 'http://www.w3.org/1999/xhtml';
    this.tagName = this.nodeName; this._a = new Map(); this.__h = undefined;
    this.value = ''; this.checked = false; this.disabled = false; this.selected = false;
    const self = this;
    this.style = new Proxy({ setProperty(k, v) { this[k] = v; }, removeProperty(k) { delete this[k]; }, getPropertyValue(k) { return this[k] || ''; } }, {
      set(t, k, v) { t[k] = v; return true; },
    });
    this.dataset = new Proxy({}, {
      get: (t, k) => self.getAttribute('data-' + String(k).replace(/[A-Z]/g, c => '-' + c.toLowerCase())) ?? undefined,
      set: (t, k, v) => { self.setAttribute('data-' + String(k).replace(/[A-Z]/g, c => '-' + c.toLowerCase()), v); return true; },
    });
    this.classList = {
      _get: () => (self.getAttribute('class') || '').split(/\s+/).filter(Boolean),
      add(...c) { const s = new Set(this._get()); c.forEach(x => s.add(x)); self.setAttribute('class', [...s].join(' ')); },
      remove(...c) { self.setAttribute('class', this._get().filter(x => !c.includes(x)).join(' ')); },
      toggle(c, force) { const has = this._get().includes(c); const on = force === undefined ? !has : force; if (on) this.add(c); else this.remove(c); return on; },
      contains(c) { return this._get().includes(c); },
    };
  }
  get attributes() { return Array.from(this._a, ([name, value]) => ({ name, value })); }
  get id() { return this.getAttribute('id') || ''; }
  set id(v) { this.setAttribute('id', v); }
  get className() { return this.getAttribute('class') || ''; }
  set className(v) { this.setAttribute('class', v); }
  setAttribute(k, v) { this._a.set(String(k), String(v)); }
  getAttribute(k) { return this._a.has(k) ? this._a.get(k) : null; }
  hasAttribute(k) { return this._a.has(k); }
  removeAttribute(k) { this._a.delete(k); }
  toggleAttribute(k, force) { const on = force === undefined ? !this.hasAttribute(k) : force; if (on) this.setAttribute(k, ''); else this.removeAttribute(k); return on; }
  querySelectorAll(sel) {
    const chains = compile(sel), out = [];
    const walk = n => { for (const c of n.childNodes) { if (c.nodeType === 1 && chains.some(ch => matchesChain(c, ch))) out.push(c); walk(c); } };
    walk(this);
    return out;
  }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
  getElementsByTagName(t) { return this.querySelectorAll(t); }
  matches(sel) { return compile(sel).some(ch => matchesChain(this, ch)); }
  closest(sel) { for (let n = this; n && n.nodeType === 1; n = n.parentNode) if (n.matches(sel)) return n; return null; }
  focus() { document.activeElement = this; }
  blur() { if (document.activeElement === this) document.activeElement = null; }
  select() {}
  click() { this.dispatchEvent(new FakeEvent('click')); }
  scrollIntoView() {}
  scrollTo() {}
  showModal() { this.open = true; }
  close() { this.open = false; }
  getBoundingClientRect() { return { x: 0, y: 0, top: 0, left: 0, right: 0, bottom: 0, width: 0, height: 0 }; }
  getContext() { return null; }
  get offsetWidth() { return 0; }
  get offsetHeight() { return 0; }
  get clientWidth() { return 0; }
  get clientHeight() { return 0; }
  get scrollHeight() { return 0; }
}

class Document extends Element {
  constructor() {
    super('#document');
    this.nodeType = 9; this.activeElement = null; this.title = ''; this.hidden = false; this.visibilityState = 'visible';
    this.documentElement = new Element('html'); this.body = new Element('body'); this.head = new Element('head');
    this.documentElement.appendChild(this.head); this.documentElement.appendChild(this.body); this.appendChild(this.documentElement);
  }
  createElement(t) { return new Element(t); }
  createElementNS(ns, t) { return new Element(t, ns); }
  createTextNode(v) { return new Text(String(v)); }
  createDocumentFragment() { return new DocumentFragment(); }
  createComment() { return new Text(''); }
  getElementById(id) { return this.querySelector('#' + id); }
}

// ---------- globals ----------

const errors = [];
const origError = console.error;
console.error = (...a) => { errors.push(a.map(x => (x && x.stack) || (typeof x === 'object' ? JSON.stringify(x) : String(x))).join(' ')); };
process.on('unhandledRejection', e => { errors.push('unhandled rejection: ' + ((e && e.stack) || e)); });

const document = new Document();
const def = (k, v) => Object.defineProperty(globalThis, k, { value: v, writable: true, configurable: true });
def('Node', Node); def('Element', Element); def('HTMLElement', Element); def('SVGElement', Element); def('Text', Text);
def('DocumentFragment', DocumentFragment); def('Event', FakeEvent); def('CustomEvent', FakeEvent); def('KeyboardEvent', FakeEvent);
def('document', document);
const winListeners = new Node(0, '#window');
def('window', globalThis);
globalThis.addEventListener = (t, f) => winListeners.addEventListener(t, f);
globalThis.removeEventListener = (t, f) => winListeners.removeEventListener(t, f);
globalThis.dispatchEvent = e => winListeners.dispatchEvent(e);
def('matchMedia', () => ({ matches: false, media: '', addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {} }));
def('scrollTo', () => {}); def('scrollX', 0); def('scrollY', 0); def('innerWidth', 1280); def('innerHeight', 900); def('devicePixelRatio', 1);
def('getComputedStyle', () => new Proxy({}, { get: (t, k) => (k === 'getPropertyValue' ? () => '' : '') }));
def('requestAnimationFrame', fn => setTimeout(() => fn(Date.now()), 0)); def('cancelAnimationFrame', id => clearTimeout(id));
const location = { href: 'http://potluck.test/ui/', origin: 'http://potluck.test', protocol: 'http:', host: 'potluck.test', pathname: '/ui/', search: '', hash: '#/' };
def('location', location);
def('history', { replaceState(_, __, to) { location.hash = to; }, pushState(_, __, to) { location.hash = to; }, back() {} });
const ls = new Map();
def('localStorage', { getItem: k => (ls.has(k) ? ls.get(k) : null), setItem: (k, v) => ls.set(k, String(v)), removeItem: k => ls.delete(k), clear: () => ls.clear() });
def('sessionStorage', globalThis.localStorage);
def('navigator', { userAgent: 'ui-exec', language: 'en', clipboard: { writeText: async () => {} } });
def('WebSocket', class { constructor() { this.readyState = 0; } send() {} close() {} addEventListener() {} });
for (const k of ['ResizeObserver', 'IntersectionObserver', 'MutationObserver']) def(k, class { observe() {} unobserve() {} disconnect() {} });
def('alert', () => {}); def('confirm', () => false); def('prompt', () => null);

let fetchAnswers = {};
const fetched = [];
const realFetch = globalThis.fetch;
let base = null;
def('fetch', async (u, opts = {}) => {
  const full = new URL(String(u), location.href);
  const p = full.pathname;
  if (base && !Object.prototype.hasOwnProperty.call(fetchAnswers, p)) {
    fetched.push((opts.method || 'GET') + ' ' + p);
    // Writes are refused; POST /api/plan only reads (a dry run), so it goes through.
    if ((opts.method || 'GET') !== 'GET' && p !== '/api/plan') return { ok: false, status: 405, statusText: 'refused by ui-exec', text: async () => '{"error":{"message":"writes are refused"}}' };
    return realFetch(base + p + full.search, { method: opts.method || 'GET', headers: opts.headers, body: opts.body });
  }
  fetched.push((opts.method || 'GET') + ' ' + p);
  const ans = Object.prototype.hasOwnProperty.call(fetchAnswers, p) ? fetchAnswers[p] : undefined;
  const ok = ans !== undefined;
  const text = ok ? JSON.stringify(ans) : JSON.stringify({ error: { message: 'not in the fixture: ' + p } });
  return { ok, status: ok ? 200 : 404, statusText: ok ? 'OK' : 'Not Found', headers: new Map([['content-type', 'application/json']]),
    text: async () => text, json: async () => JSON.parse(text), blob: async () => text };
});

// ---------- run ----------

const spec = JSON.parse(readFileSync(process.argv[2], 'utf8'));
base = spec.base ? spec.base.replace(/\/$/, '') : null;
const ui = path.resolve(spec.ui);
const mod = rel => import(pathToFileURL(path.join(ui, rel)).href);
const settle = async () => { for (let i = 0; i < 20; i++) await new Promise(r => setTimeout(r, 0)); };

const router = await mod('core/router.js');
const store = await mod('core/store.js');
for (const m of spec.modules || []) await mod(m);
await settle();
if (errors.length) { origError('module import errors:\n' + errors.join('\n')); process.exit(1); }

// The screens are mounted the way the page does it: through the router, on
// a hash change, so a view that navigates (placement picks a profile) works.
const outlet = new Element('main');
document.body.replaceChildren(outlet);
location.hash = '#/__ui_exec_blank';
router.start(outlet);
const go = async hash => { location.hash = hash; globalThis.dispatchEvent(new FakeEvent('hashchange')); await settle(); };

let failed = 0;
for (const c of spec.cases) {
  errors.length = 0; fetched.length = 0;
  fetchAnswers = Object.assign({}, spec.fetch || {}, c.fetch || {});
  const route = router.parse(c.route);
  if (!router.getView(route.id)) errors.push('no view registered for ' + route.id);
  else {
    if (c.status === 'fetch') c.status = await (await realFetch(base + '/api/status')).json();
    store.set('status', c.status === undefined ? null : c.status);
    await go(route.hash);
    if (c.status !== undefined) store.set('status', c.status); // a second draw over a mounted tree (morph)
    store.set('tick', (store.state.tick || 0) + 1);
    await settle();
  }
  const text = outlet.textContent;
  for (const bad of ['could not be drawn', 'failed to start']) if (text.includes(bad)) errors.push('banner: ' + text.slice(Math.max(0, text.indexOf(bad) - 40), text.indexOf(bad) + 200));
  for (const want of c.expect || []) if (!text.includes(want)) errors.push('missing text: ' + JSON.stringify(want));
  await go('#/__ui_exec_blank'); // unmounts the screen
  if (errors.length) {
    failed++;
    console.log(`FAIL ${c.name}\n  ` + errors.join('\n  ').split('\n').slice(0, 30).join('\n  '));
  } else console.log(`ok   ${c.name} (${text.length} characters drawn${fetched.length ? '; fetched ' + [...new Set(fetched)].join(', ') : ''})`);
}
process.exit(failed ? 1 : 0);
