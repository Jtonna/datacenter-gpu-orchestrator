#!/usr/bin/env node
// ui-exec.mjs: runs screens of the web UI (internal/api/ui) under node, on a
// small fake DOM, and reports every runtime error. `node --check` only parses
// a module; this executes the rendering, so a reference to an undeclared name
// or a field read on undefined is found without a browser.
//
// usage: node scripts/ui-exec.mjs cases.json
//        node scripts/ui-exec.mjs --base http://HOST:PORT [--tiles N] [--reserved] [--local-ui]
//
// A server serves one UI, at /ui/: the files of ui/next/ laid over the
// shared modules of ui/ (internal/api/uifiles.go). The classic UI that was
// served at /ui/classic/ is removed; a cases file that asks for "shell":
// "classic" fails.
//
// --base: the live check. It downloads the page FROM THAT SERVER: <base>/ui/
// index.html, its module script and every module it imports, statically or
// dynamically, followed recursively, into a temporary folder; a module the
// server does not serve, or that does not parse as an ES module, fails the
// run with its name. Then it loads app.js, the page's start module, from
// those files, the way the browser does; reads GET /api/status of that
// server; and mounts every screen in turn, with the server's real
// answers (GET only; the dry-run POST /api/plan passes, every other write is
// refused, so nothing changes on the server). The Nodes page line lists each
// machine tile and whether its reserved-against-measured block is drawn.
// --tiles N fails unless the Nodes page draws exactly N tiles; --reserved
// fails unless every tile of a machine that is not excluded has the block.
// Exit status 0 when every screen drew without a runtime error, else 1.
// --local-ui (development only): take the page from this checkout
// (internal/api/ui) instead of the server; only the answers come from it.
//
// cases.json:
//   {"ui": "<path of internal/api/ui>",
//    "shell": "next",                              // optional: the only UI (ui/next laid over the shared files)
//    "modules": ["views/nodes.js", ...],          // imported before the cases (they register their views)
//    "fetch": {"/api/profiles": {...}, ...},       // answers of GET/POST by path (query ignored); others are 404
//    "base": "http://127.0.0.1:8790",              // or: GET requests go to this server (cmd/devcluster), writes are refused
//    "cases": [{"name": "...", "route": "#/nodes", "status": {...} or "fetch", "fetch": {...}, "expect": ["text", ...],
//               "absent": ["text that must not be drawn", ...], "none": ["css selector that must match nothing", ...],
//               "order": ["text", "text drawn after it", ...],
//               "has": ["css selector that must match something on the screen", ...],
//               "pageHas" / "pageNone": the same over the whole page (shell, popups)
//               "count": {"css selector": n} on the screen; "pageCount": the same over the whole page
//               "text": {"css selector": "whole text"}: each element it matches reads exactly that
//               "hash": "#/zones": the address after the clicks
//               "ws": [{"type": "hello"|"requests"|..., "data": {...}}, ...]: live-channel
//               messages delivered after the screen is drawn (hello also sets the
//               connection live), before the clicks
//               "wsAfter": [...]: the same, delivered after the clicks
//               "keys": ["Escape", {"key": "Tab", "shiftKey": true}]: key presses after that
//               "back": 1: history.back() that many times after that
//               "width": 1440: the window's width for the case (default 1280);
//               "widthAfter": a width set at the end (a resize)
//               "focused": "css selector the focused element must match"
//               "modules" (top level, with "app"): modules loaded before app.js
//               "links": [{"text": "Overview", "path": "/ui/"}, ...],
//               "click": ["css selector clicked after the draw", {"type": "css selector", "value": "typed"}, ...],
//               "page": ["text anywhere on the page: shell, side panel, dialog", ...],
//               "notOnPage": ["text nowhere on the page", ...],
//               "notOnPageRe": ["regexp no text of the page may match", ...],
//               "brandFree": true: no brand word (PlayStation, PS5, OMEN, ...) and no "console(s)" in the page's text or its titles, tips, placeholders, labels,
//               "titles": ["text some title attribute on the page contains", ...],
//               "fetchedQuery": ["regexp a request (GET /path?query body) must match", ...],
//               "notFetched": ["regexp no request of the case may match", ...],
//               "fetchedCount": {"regexp": n}: exactly n requests of the case match it,
//               "noHeader": ["X-Actor", ...]: no request of the case carried it,
//               "sentHeader": ["^X-Comment: why$"]: a header line "Name: value" matched each,
//               "noHeaderLine": ["ptl_"]: no header line matched any,
//               "subscribed": ["models"] / "notSubscribed": ["models"]: topics the
//               live channel's last subscribe names, or not (with top-level "wsOpen")}],
//    (fetch answers: "POST /api/agents" before "/api/agents"; {"$status": 409, "$body": {...}} for an error;
//     {"$match": [["regexp", answer], ...]}: the first regexp that matches the request line;
//     {"$hang": true}: the request never answers, to draw a page while it waits;
//     {"$after": "regexp", "$then": answer, "$before": answer}: $then once an earlier request line matched)
//     "METHOD /path?query body" picks the answer, none answers 404: one route, answers by query or body)
//    "wsOpen": true,                               // the live channel's socket opens (with "app")
//    "now": "2026-11-24T06:30:00Z",               // the clock starts there (Date.now, new Date())
//    "tz": "America/Los_Angeles"}                 // the zone: uiExec sets TZ for node
//
// With "base", "status": "fetch" takes GET <base>/api/status, and a request
// with no answer in "fetch" goes to the base server: GETs, POST /api/plan and
// writes with ?dry_run=1 (they only read); any other write is refused (405)
// unless "passWrites": ["regexp on METHOD /path?query body", ...] names it.
//
// Each case sets the live status, mounts the route's view, lets fetches and
// re-draws settle, and fails on any console.error, unhandled rejection, a
// "could not be drawn" / "failed to start" banner, an expected text that is
// missing, an absent text that is drawn, a "none" selector that matches, or a
// "links" entry with no link of that text to that path anywhere on the page.
// Output: one line per case; exit status 1 when any case fails.

import { readFileSync, writeFileSync, mkdirSync, mkdtempSync, rmSync, cpSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import os from 'node:os';
import { pathToFileURL, fileURLToPath } from 'node:url';
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
    if (op === '^=' && !el.getAttribute(k).startsWith(v)) return false;
    if (op === '*=' && !el.getAttribute(k).includes(v)) return false;
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
// matchMedia answers width queries ((min-width:Npx), (max-width:Npx)) against
// innerWidth; a case's "width" changes it and tells the queries that flip.
// Any other query does not match.
const mqls = new Set();
const evalMedia = q => /width/.test(q) && Array.from(q.matchAll(/\((min|max)-width:\s*(\d+)px\)/g)).every(m => (m[1] === 'min' ? globalThis.innerWidth >= +m[2] : globalThis.innerWidth <= +m[2]));
def('matchMedia', q => {
  const l = new Set();
  const mql = { media: q, get matches() { return evalMedia(q); }, addEventListener(t, f) { l.add(f); }, removeEventListener(t, f) { l.delete(f); },
    addListener(f) { l.add(f); }, removeListener(f) { l.delete(f); }, _l: l, _last: false };
  mql._last = mql.matches;
  mqls.add(mql);
  return mql;
});
function setWidth(w) {
  globalThis.innerWidth = w;
  for (const m of mqls) {
    const now = m.matches;
    if (now === m._last) continue;
    m._last = now;
    for (const f of Array.from(m._l)) f({ matches: now, media: m.media });
  }
}
def('scrollTo', () => {}); def('scrollX', 0); def('scrollY', 0); def('innerWidth', 1280); def('innerHeight', 900); def('devicePixelRatio', 1);
def('getComputedStyle', () => new Proxy({}, { get: (t, k) => (k === 'getPropertyValue' ? () => '' : '') }));
def('requestAnimationFrame', fn => setTimeout(() => fn(Date.now()), 0)); def('cancelAnimationFrame', id => clearTimeout(id));
// The page's address: /ui/ (set before any UI module is imported; core/api.js
// reads it once).
// The address keeps a history of entries, as a browser's: a page that sets
// location.hash adds an entry and gets a hashchange event (on the next
// turn); pushState adds one and replaceState changes the current one, both
// without an event; back() and forward() move between entries with an event.
const location = { href: 'http://potluck.test/ui/', origin: 'http://potluck.test', protocol: 'http:', host: 'potluck.test', pathname: '/ui/', search: '' };
const entries = ['#/'];
let entry = 0;
const hashOf = to => { const i = String(to).indexOf('#'); return i >= 0 ? String(to).slice(i) : String(to); };
const fireHash = () => setTimeout(() => globalThis.dispatchEvent(new FakeEvent('hashchange')), 0);
const pushEntry = h => { entries.length = entry + 1; entries.push(h); entry = entries.length - 1; };
Object.defineProperty(location, 'hash', {
  get: () => entries[entry],
  set: v => { const h = hashOf(v); if (h === entries[entry]) return; pushEntry(h); fireHash(); },
  enumerable: true,
});
// setHashQuietly: the harness's own moves (mounting a case's route); it sends the event itself
const setHashQuietly = v => pushEntry(hashOf(v));
def('location', location);
def('history', {
  replaceState(_, __, to) { entries[entry] = hashOf(to); },
  pushState(_, __, to) { pushEntry(hashOf(to)); },
  back() { if (entry > 0) { entry--; fireHash(); } },
  forward() { if (entry < entries.length - 1) { entry++; fireHash(); } },
  get length() { return entries.length; },
});
const ls = new Map();
def('localStorage', { getItem: k => (ls.has(k) ? ls.get(k) : null), setItem: (k, v) => ls.set(k, String(v)), removeItem: k => ls.delete(k), clear: () => ls.clear() });
def('sessionStorage', globalThis.localStorage);
def('navigator', { userAgent: 'ui-exec', language: 'en', clipboard: { writeText: async () => {} } });
// The live channel never opens, unless the cases file says "wsOpen": true:
// then a socket opens at once and every subscribe it sends is kept, for a
// case's "subscribed" / "notSubscribed" (the topics of the last subscribe).
const wsSubscribes = [];
def('WebSocket', class {
  static OPEN = 1;
  constructor() {
    this.readyState = 0;
    if (spec.wsOpen) setTimeout(() => { this.readyState = 1; if (this.onopen) this.onopen(); }, 0);
  }
  send(b) { try { const m = JSON.parse(b); if (m.type === 'subscribe') wsSubscribes.push(m.topics || []); } catch { /* not JSON */ } }
  close() {} addEventListener() {}
});
for (const k of ['ResizeObserver', 'IntersectionObserver', 'MutationObserver']) def(k, class { observe() {} unobserve() {} disconnect() {} });
def('alert', () => {}); def('confirm', () => false); def('prompt', () => null);

let fetchAnswers = {};
const fetched = [];
const fetchedFull = []; // with the query and the body, for "fetchedQuery"
const sentHeaders = new Set(); // every header name a request carried, for "noHeader"
const sentHeaderLines = []; // "Name: value" of every header sent, for "sentHeader"
const realFetch = globalThis.fetch;
// requests to a real server (base) still in flight: settle() waits for them,
// since a real answer (a write that syncs to disk, a slow race build) can
// take longer than a few timer turns; realLog keeps each write's answer for
// the failure report
let inflight = 0;
const realLog = [];
let base = null;
let passWrites = [];
def('fetch', async (u, opts = {}) => {
  const full = new URL(String(u), location.href);
  const p = full.pathname;
  if (base && !Object.prototype.hasOwnProperty.call(fetchAnswers, p)) {
    const m = opts.method || 'GET';
    fetched.push(m + ' ' + p);
    const line = m + ' ' + p + full.search + (opts.body != null ? ' ' + String(opts.body) : '');
    fetchedFull.push(line);
    for (const k of Object.keys(opts.headers || {})) { sentHeaders.add(k); sentHeaderLines.push(k + ': ' + opts.headers[k]); }
    // Writes are refused. What only reads goes through: POST /api/plan, a
    // write with ?dry_run=1, and a write the spec lets through by name
    // ("passWrites": regexps on the request line, for a test that owns its server).
    const reads = p === '/api/plan' || full.searchParams.get('dry_run') === '1';
    if (m !== 'GET' && !reads && !passWrites.some(re => new RegExp(re).test(line))) return { ok: false, status: 405, statusText: 'refused by ui-exec', text: async () => '{"error":{"message":"writes are refused"}}' };
    inflight++;
    try {
      const r = await realFetch(base + p + full.search, { method: m, headers: opts.headers, body: opts.body });
      const body = await r.text();
      if (m !== 'GET') realLog.push(`${line} -> ${r.status} ${body.slice(0, 300)}`);
      return new Response(body, { status: r.status, statusText: r.statusText, headers: r.headers });
    } finally { inflight--; }
  }
  const method = opts.method || 'GET';
  fetched.push(method + ' ' + p);
  fetchedFull.push(method + ' ' + p + full.search + (opts.body != null ? ' ' + String(opts.body) : ''));
  for (const k of Object.keys(opts.headers || {})) { sentHeaders.add(k); sentHeaderLines.push(k + ': ' + opts.headers[k]); }
  // an answer by "METHOD /path" first, then by path; {"$status": 409, "$body": {...}} answers an error
  const has = k => Object.prototype.hasOwnProperty.call(fetchAnswers, k);
  let ans = has(method + ' ' + p) ? fetchAnswers[method + ' ' + p] : has(p) ? fetchAnswers[p] : undefined;
  // {"$after": "regexp", "$then": answer, "$before": answer}: $then once an earlier request of the case matched (a page that reads again after a write)
  if (ans && typeof ans === 'object' && typeof ans.$after === 'string') ans = fetchedFull.slice(0, -1).some(l => new RegExp(ans.$after).test(l)) ? ans.$then : ans.$before;
  if (ans && typeof ans === 'object' && Array.isArray(ans.$match)) {
    const line = method + ' ' + p + full.search + (opts.body != null ? ' ' + String(opts.body) : '');
    const hit = ans.$match.find(([re]) => new RegExp(re).test(line));
    ans = hit ? hit[1] : undefined;
  }
  if (ans && typeof ans === 'object' && ans.$hang) return new Promise(() => {}); // still waiting when the case is read
  let status = ans !== undefined ? 200 : 404;
  if (ans && typeof ans === 'object' && ans.$status) { status = ans.$status; ans = ans.$body; }
  const ok = status >= 200 && status < 300;
  const text = ans !== undefined ? JSON.stringify(ans) : JSON.stringify({ error: { message: 'not in the fixture: ' + method + ' ' + p } });
  return { ok, status, statusText: ok ? 'OK' : 'Not Found', headers: new Map([['content-type', 'application/json']]),
    text: async () => text, json: async () => JSON.parse(text), blob: async () => text };
});

// ---------- run ----------

const argv = process.argv.slice(2);
const flag = k => { const i = argv.indexOf(k); return i < 0 ? null : (argv[i + 1] && !argv[i + 1].startsWith('--') ? argv[i + 1] : true); };
const live = flag('--base');
const here = path.dirname(fileURLToPath(import.meta.url));
// The one UI a server serves, at /ui/ (internal/api/uifiles.go).
const UIS = { next: '/ui/' };
const LIVE_SCREENS = {
  next: ['overview', 'requests', 'report', 'zones', 'models', 'nodes', 'queue', 'activity', 'placement', 'benchmarks', 'benchmarks/runs', 'benchmarks/trends', 'profiles', 'tiers', 'settings', 'audit', 'mcp', 'about'],
};
const which = live ? 'next' : null;
// mirrorUI downloads the page from the server, as a browser would load it,
// into a temporary folder: index.html, its module scripts and modulepreload
// links, and every relative import of every module (static, re-export and
// dynamic), recursively. Each module must be served (200) and parse as an
// ES module (node --check on an .mjs copy; a .js file is not checked as a
// module).
async function mirrorUI(root, prefix) {
  const dir = mkdtempSync(path.join(os.tmpdir(), 'ui-exec-'));
  process.on('exit', () => { try { rmSync(dir, { recursive: true, force: true }); } catch { /* left in the temp folder */ } });
  writeFileSync(path.join(dir, 'package.json'), '{"type":"module"}\n'); // the .js files are ES modules, as the browser loads them
  const uiURL = root + prefix;
  const problems = [];
  const get = async rel => {
    let r;
    try { r = await realFetch(uiURL + rel); } catch (e) { problems.push(`${rel}: could not be fetched: ${e.message}`); return null; }
    if (r.status !== 200) { problems.push(`${rel}: the server answered ${r.status}`); return null; }
    const text = await r.text();
    mkdirSync(path.dirname(path.join(dir, rel)), { recursive: true });
    writeFileSync(path.join(dir, rel), text);
    return text;
  };
  const html = await get('index.html');
  if (html == null) return { dir, count: 0, problems };
  const queue = [];
  for (const m of html.matchAll(/<script[^>]*type="module"[^>]*src="([^"]+)"/g)) queue.push(m[1]);
  for (const m of html.matchAll(/<link[^>]*rel="modulepreload"[^>]*href="([^"]+)"/g)) queue.push(m[1]);
  const seen = new Set();
  const norm = rel => path.posix.normalize(rel).replace(/^\.\//, '');
  while (queue.length) {
    const rel = norm(queue.shift());
    if (seen.has(rel)) continue;
    seen.add(rel);
    const src = await get(rel);
    if (src == null) continue;
    const check = spawnSync(process.execPath, ['--check', '--input-type=module'], { input: src, encoding: 'utf8' });
    if (check.status !== 0) {
      const why = (check.stderr || '').split('\n').filter(l => /Error/.test(l))[0] || 'does not parse';
      const at = ((check.stderr || '').match(/\[stdin\]:(\d+)/) || [])[1];
      problems.push(`${rel}${at ? ':' + at : ''}: does not parse as an ES module: ${why.trim()}`);
    }
    const from = path.posix.dirname(rel);
    for (const m of src.matchAll(/(?:^|[\s;}])(?:import|export)\s[^'"`;]*?from\s*['"](\.{1,2}\/[^'"]+)['"]|(?:^|[\s;])import\s*['"](\.{1,2}\/[^'"]+)['"]|import\(\s*['"](\.{1,2}\/[^'"]+)['"]\s*\)/gm)) {
      queue.push(path.posix.join(from, m[1] || m[2] || m[3]));
    }
  }
  return { dir, count: seen.size, problems };
}

let uiDir = path.join(here, '..', 'internal', 'api', 'ui');
if (live && !flag('--local-ui')) {
  const mirror = await mirrorUI(String(live).replace(/\/$/, ''), UIS[which]);
  console.log(`${mirror.problems.length ? 'FAIL' : 'ok  '} fetched ${mirror.count} modules from ${String(live).replace(/\/$/, '')}${UIS[which]}` +
    (mirror.problems.length ? '\n  ' + mirror.problems.join('\n  ') : ''));
  if (mirror.problems.length) { console.log('FAILED: the page the server serves cannot load'); process.exit(1); }
  uiDir = mirror.dir;
} else if (live) console.log('note: --local-ui: the page comes from this checkout, not from the server');
const spec = live
  ? { ui: uiDir, base: String(live), app: true, shell: which,
      cases: LIVE_SCREENS[which].map(r => ({ name: r, route: '#/' + r, status: 'fetch' })) }
  : JSON.parse(readFileSync(argv[0], 'utf8'));
base = spec.base ? spec.base.replace(/\/$/, '') : null;
passWrites = spec.passWrites || [];
// "now": "2026-11-24T06:30:00Z" fixes the clock there (it runs on from it):
// new Date(), Date.now(). The zone is the process's (TZ, set by the caller).
if (spec.now) {
  const RealDate = Date, shift = RealDate.parse(spec.now) - RealDate.now();
  class FixedDate extends RealDate {
    constructor(...a) { if (a.length) super(...a); else super(RealDate.now() + shift); }
    static now() { return RealDate.now() + shift; }
  }
  globalThis.Date = FixedDate;
}
if (spec.shell && spec.shell !== 'next') { console.log(`FAIL "shell": "${spec.shell}": the classic UI is removed; /ui/ is the only UI`); process.exit(1); }
const shell = 'next';
location.pathname = UIS[shell];
location.href = location.origin + UIS[shell];
// The checkout's UI runs from a copy in a temporary folder with its own
// package.json ({"type":"module"}): node takes a module's type from the
// nearest package.json above it, and one without "type" above the checkout
// (a user Temp folder, say) makes it warn on every module, which counts as
// an error here. The mirror of --base has its own package.json already.
// The copy is what the server serves below /ui/: the shared files of ui/
// with the files of ui/next/ laid over them (internal/api/uifiles.go).
function isolatedCopy(src) {
  const dir = mkdtempSync(path.join(os.tmpdir(), 'ui-exec-local-'));
  process.on('exit', () => { try { rmSync(dir, { recursive: true, force: true }); } catch { /* left in the temp folder */ } });
  const ui = path.join(dir, 'ui');
  cpSync(src, ui, { recursive: true });
  rmSync(path.join(ui, 'next'), { recursive: true, force: true });
  cpSync(path.join(src, 'next'), ui, { recursive: true });
  // the fixture's demonstration modules (scripts/ui-fixture), served by
  // ui-fixture-server.mjs at /ui/_fixture/
  cpSync(path.join(here, 'ui-fixture'), path.join(ui, '_fixture'), { recursive: true });
  writeFileSync(path.join(dir, 'package.json'), '{"type":"module"}\n');
  return ui;
}
const ui = uiDir === spec.ui && live && !flag('--local-ui') ? path.resolve(spec.ui) : isolatedCopy(path.resolve(spec.ui));
const mod = rel => import(pathToFileURL(path.join(ui, rel)).href);
const settle = async () => {
  const until = Date.now() + 20000;
  for (;;) {
    for (let i = 0; i < 20; i++) await new Promise(r => setTimeout(r, 0));
    if (!inflight || Date.now() > until) return;
    while (inflight && Date.now() <= until) await new Promise(r => setTimeout(r, 5));
  }
};

let router, store;
try { router = await mod('core/router.js'); store = await mod('core/store.js'); } catch (e) {
  console.log('FAIL core/router.js or core/store.js did not load: ' + (e.stack || e));
  process.exit(1);
}
let outlet;
if (spec.app) {
  // The page as index.html builds it: its body's elements (up to the
  // module script), then app.js starts the shell in #app and the router in #main.
  const html = readFileSync(path.join(ui, 'index.html'), 'utf8');
  const bodyHTML = html.slice(html.indexOf('<body>') + 6, html.indexOf('<noscript>'));
  const stack = [document.body];
  document.body.replaceChildren();
  for (const m of bodyHTML.matchAll(/<(\/?)([a-z]+)([^>]*)>/g)) {
    if (m[1]) { stack.pop(); continue; }
    const el = new Element(m[2]);
    for (const a of m[3].matchAll(/([a-z-]+)="([^"]*)"/g)) el.setAttribute(a[1], a[2]);
    stack[stack.length - 1].appendChild(el);
    stack.push(el);
  }
  outlet = document.getElementById('main');
  setHashQuietly('#/__ui_exec_blank');
  // modules loaded before app.js, as the fixture's components page does
  for (const m of spec.modules || []) { try { await mod(m); } catch (e) { errors.push(m + ' did not load: ' + (e.stack || e)); } }
  try { await mod('app.js'); } catch (e) { errors.push('app.js did not load: ' + (e.stack || e)); }
  await settle();
  console.log((errors.length ? 'FAIL' : 'ok  ') + ' start module app.js' + (errors.length ? '\n  ' + errors.join('\n  ') : ''));
} else for (const m of spec.modules || []) { try { await mod(m); } catch (e) { errors.push(m + ' did not load: ' + (e.stack || e)); } }
await settle();
if (errors.length) { origError('module import errors:\n' + errors.join('\n')); process.exit(1); }

// The screens are mounted the way the page does it: through the router, on
// a hash change, so a view that navigates (placement picks a profile) works.
if (!spec.app) {
  outlet = new Element('main');
  document.body.replaceChildren(outlet);
  setHashQuietly('#/__ui_exec_blank');
  router.start(outlet);
}
const go = async hash => { setHashQuietly(hash); globalThis.dispatchEvent(new FakeEvent('hashchange')); await settle(); };

let failed = 0;
for (const c of spec.cases) {
  errors.length = 0; realLog.length = 0; fetched.length = 0; fetchedFull.length = 0; sentHeaders.clear(); sentHeaderLines.length = 0;
  fetchAnswers = Object.assign({}, spec.fetch || {}, c.fetch || {});
  const route = router.parse(c.route);
  if (!router.getView(route.id)) errors.push('no view registered for ' + route.id);
  else {
    setWidth(c.width || 1280);
    if (c.status === 'fetch') c.status = await (await realFetch(base + '/api/status')).json();
    store.set('status', c.status === undefined ? null : c.status);
    await go(route.hash);
    if (c.status !== undefined) store.set('status', c.status); // a second draw over a mounted tree (morph)
    store.set('tick', (store.state.tick || 0) + 1);
    await settle();
    for (const m of c.ws || []) {
      if (m.type === 'hello') { store.set('conn', 'live'); store.set('hello', m.data); }
      if (m.type === 'status') { store.state.statusAt = Date.now(); store.set('status', m.data); }
      store.publish(m.type, m.data, Date.now());
      await settle();
    }
    // click: css selectors, clicked in order (the first match on the page each)
    for (const sel of c.click || []) {
      // {"type": "css selector", "value": "text"}: the text typed into that field (an input event)
      // {"context": "css selector"}: a right click on it (a contextmenu event)
      // {"dbl": "css selector"}: a double click on it (a dblclick event)
      if (sel && typeof sel === 'object' && sel.dbl) {
        const el = document.body.querySelector(sel.dbl);
        if (!el) { errors.push('nothing to double-click: ' + sel.dbl); break; }
        el.dispatchEvent(new FakeEvent('dblclick'));
        await settle();
        continue;
      }
      if (sel && typeof sel === 'object' && sel.context) {
        const el = document.body.querySelector(sel.context);
        if (!el) { errors.push('nothing to right-click: ' + sel.context); break; }
        el.dispatchEvent(new FakeEvent('contextmenu', { clientX: 300, clientY: 300 }));
        await settle();
        continue;
      }
      if (sel && typeof sel === 'object') {
        const f = document.body.querySelector(sel.type);
        if (!f) { errors.push('nothing to type into: ' + sel.type); break; }
        f.focus(); f.value = sel.value; f.dispatchEvent(new FakeEvent('input')); f.dispatchEvent(new FakeEvent('change'));
        await settle();
        continue;
      }
      const el = document.body.querySelector(sel);
      if (!el) { errors.push('nothing to click: ' + sel); break; }
      // a click focuses what can take the focus, as in a browser
      if (/^(BUTTON|A|INPUT|SELECT|TEXTAREA)$/.test(el.nodeName) || el.getAttribute('tabindex') != null) el.focus();
      el.click();
      await settle();
    }
    // wsAfter: live-channel messages delivered after the clicks
    for (const m of c.wsAfter || []) {
      store.publish(m.type, m.data, Date.now());
      await settle();
    }
    // benchRunAfter: the run in progress after that (a progress document, or
    // null when it ended), set as core/live.js sets it from the "bench"
    // topic, then the "bench" key emitted
    if ('benchRunAfter' in c) {
      store.state.bench.run = c.benchRunAfter;
      store.emit('bench');
      await settle();
    }
    // keys: key presses after that, sent to the focused element (or the body)
    // and up through its ancestors to the document, as a browser does
    for (const k of c.keys || []) {
      const ev = new FakeEvent('keydown', typeof k === 'string' ? { key: k } : k);
      const at = document.activeElement || document.body;
      for (let n = at; n; n = n.parentNode) { ev.target = at; n.dispatchEvent(ev); }
      await settle();
    }
    // back: history.back() that many times (a hash change each)
    for (let i = 0; i < (c.back || 0); i++) { globalThis.history.back(); await settle(); }
    // widthAfter: the window resized after all that
    if (c.widthAfter) { setWidth(c.widthAfter); await settle(); }
    // against a real server (base): the case is judged once its condition
    // holds, not after a fixed number of turns: every expected text on the
    // screen and on the page, polled (with the requests in flight answered,
    // settle) for up to 30 s, so a loaded machine cannot fail it early
    if (base && ((c.expect || []).length || (c.page || []).length)) {
      const until = Date.now() + 30000;
      const ready = () => (c.expect || []).every(w => outlet.textContent.includes(w)) && (c.page || []).every(w => document.body.textContent.includes(w));
      while (!ready() && Date.now() < until) { await new Promise(r => setTimeout(r, 50)); await settle(); }
    }
  }
  const text = outlet.textContent;
  for (const bad of ['could not be drawn', 'failed to start']) if (text.includes(bad)) errors.push('banner: ' + text.slice(Math.max(0, text.indexOf(bad) - 40), text.indexOf(bad) + 200));
  for (const want of c.expect || []) if (!text.includes(want)) errors.push('missing text: ' + JSON.stringify(want));
  // what the page showed instead: the open dialog, the banners, the writes' answers
  if ((c.expect || []).some(w => !text.includes(w)) || (c.page || []).some(w => !document.body.textContent.includes(w))) {
    const dlg = document.body.querySelector('div.dlg');
    if (dlg) errors.push(`  the dialog shows [${dlg.getAttribute('data-dlg') || ''} ${dlg.getAttribute('data-state') || ''}]: ` + JSON.stringify(dlg.textContent.slice(0, 600)));
    for (const b of document.body.querySelectorAll('.banner')) errors.push('  a banner says: ' + JSON.stringify(b.textContent.slice(0, 300)));
    for (const l of realLog.slice(-6)) errors.push('  a write answered: ' + l);
    if (inflight) errors.push(`  ${inflight} request(s) to the server still unanswered after 20 s`);
  }
  // order: texts that must appear in this order (a sorted list)
  { let at = -1; for (const want of c.order || []) { const i = text.indexOf(want, at + 1); if (i < 0) { errors.push('out of order or missing: ' + JSON.stringify(want)); break; } at = i; } }
  for (const bad of c.absent || []) if (text.includes(bad)) errors.push('text that must not be there: ' + JSON.stringify(bad) + ' in ' + JSON.stringify(text.slice(Math.max(0, text.indexOf(bad) - 80), text.indexOf(bad) + 80)));
  for (const sel of c.none || []) if (outlet.querySelector(sel)) errors.push('an element that must not be there: ' + sel);
  for (const sel of c.has || []) if (!outlet.querySelector(sel)) errors.push('no element matches: ' + sel);
  // count: {selector: n}: exactly n elements in the view match
  for (const [sel, n] of Object.entries(c.count || {})) { const k = outlet.querySelectorAll(sel).length; if (k !== n) errors.push(`${k} elements match ${sel}, want ${n}`); }
  // text: {"css selector": "the whole text of each element it matches"}
  for (const [sel, want] of Object.entries(c.text || {})) {
    const els = outlet.querySelectorAll(sel);
    if (!els.length) errors.push(`no element matches: ${sel}`);
    for (const e of els) { const got = String(e.textContent).replace(/\s+/g, ' ').trim(); if (got !== want) errors.push(`${sel} reads "${got}", want "${want}"`); }
  }
  // pageSeq: {"css selector": ["text", ...]}: the trimmed texts of the
  // elements on the page that selector matches, in document order, are
  // exactly these (a menu's order)
  for (const [sel, want] of Object.entries(c.pageSeq || {})) {
    const got = document.body.querySelectorAll(sel).map(e => String(e.textContent).replace(/\s+/g, ' ').trim());
    if (JSON.stringify(got) !== JSON.stringify(want)) errors.push(`${sel} reads ${JSON.stringify(got)}, want ${JSON.stringify(want)}`);
  }
  for (const [sel, n] of Object.entries(c.pageCount || {})) { const k = document.body.querySelectorAll(sel).length; if (k !== n) errors.push(`${k} elements on the page match ${sel}, want ${n}`); }
  // evenX: selectors of chart points (cx) that must be spread evenly within
  // each chart (their parent svg): the widest gap between neighbouring x
  // values at most twice the narrowest
  for (const sel of c.evenX || []) {
    const charts = new Map();
    for (const e of outlet.querySelectorAll(sel)) { const k = e.parentNode; if (!charts.has(k)) charts.set(k, new Set()); charts.get(k).add(Number(e.getAttribute('cx'))); }
    if (!charts.size) errors.push(`evenX: no point matches ${sel}`);
    for (const set of charts.values()) {
      const xs = [...set].sort((x, y) => x - y), gaps = xs.slice(1).map((x, i) => x - xs[i]);
      if (xs.length < 3) errors.push(`evenX: a chart with ${xs.length} points (${sel})`);
      else if (Math.max(...gaps) > 2 * Math.min(...gaps)) errors.push(`evenX: ${sel} gaps from ${Math.min(...gaps).toFixed(1)} to ${Math.max(...gaps).toFixed(1)}`);
    }
  }
  if (c.hash && location.hash !== c.hash) errors.push('the address is ' + location.hash + ', want ' + c.hash);
  // titles: texts some element's title attribute on the page contains
  if (c.titles) {
    const all = [];
    const walk = n => { if (n.getAttribute) { const t = n.getAttribute('title'); if (t) all.push(t); } for (const k of n.childNodes || []) walk(k); };
    walk(document.body);
    for (const want of c.titles) if (!all.some(t => t.includes(want))) errors.push('no title contains ' + JSON.stringify(want) + ' (titles: ' + JSON.stringify(all.filter(t => /\d/.test(t)).slice(0, 12)) + ')');
  }
  // fetchedQuery: regular expressions some request (method, path and query) must match
  for (const re of c.fetchedQuery || []) if (!fetchedFull.some(f => new RegExp(re).test(f))) errors.push('no request matches /' + re + '/ (requests: ' + JSON.stringify([...new Set(fetchedFull)]) + ')');
  // notFetched: regular expressions no request may match (a write that must not be sent)
  for (const re of c.notFetched || []) { const hit = fetchedFull.find(f => new RegExp(re).test(f)); if (hit) errors.push('a request that must not be sent: ' + hit); }
  // fetchedCount: how many requests match (a read sent once, not once per draw)
  for (const [re, n] of Object.entries(c.fetchedCount || {})) { const hits = fetchedFull.filter(f => new RegExp(re).test(f)); if (hits.length !== n) errors.push(hits.length + ' requests match /' + re + '/, want ' + n + ': ' + JSON.stringify(hits)); }
  for (const hd of c.noHeader || []) if (sentHeaders.has(hd)) errors.push('a request carried the header ' + hd);
  for (const re of c.sentHeader || []) if (!sentHeaderLines.some(l => new RegExp(re).test(l))) errors.push('no header matches /' + re + '/ (headers: ' + JSON.stringify([...new Set(sentHeaderLines)]) + ')');
  // subscribed / notSubscribed: topics the live channel's last subscribe names, or not
  {
    const last = wsSubscribes.length ? wsSubscribes[wsSubscribes.length - 1] : [];
    for (const t of c.subscribed || []) if (!last.includes(t)) errors.push('the live channel is not subscribed to ' + t + ' (last subscribe: ' + JSON.stringify(last) + ')');
    for (const t of c.notSubscribed || []) if (last.includes(t)) errors.push('the live channel is subscribed to ' + t + ' (last subscribe: ' + JSON.stringify(last) + ')');
  }
  for (const re of c.noHeaderLine || []) { const hit = sentHeaderLines.find(l => new RegExp(re).test(l)); if (hit) errors.push('a header that must not be sent: ' + hit); }
  for (const sel of c.pageHas || []) if (!document.body.querySelector(sel)) errors.push('no element on the page matches: ' + sel);
  for (const sel of c.pageNone || []) if (document.body.querySelector(sel)) errors.push('an element on the page that must not be there: ' + sel);
  if (c.focused && !(document.activeElement && document.activeElement.matches && document.activeElement.matches(c.focused)))
    errors.push('the focus is on ' + (document.activeElement ? document.activeElement.nodeName + ' ' + JSON.stringify(document.activeElement.getAttribute ? document.activeElement.getAttribute('class') : '') : 'nothing') + ', want ' + c.focused);
  // page: texts that must be anywhere on the page, the shell, side panels and dialogs included
  const pageText = document.body.textContent;
  for (const want of c.page || []) if (!pageText.includes(want)) errors.push('missing on the page: ' + JSON.stringify(want));
  for (const bad of c.notOnPage || []) if (pageText.includes(bad)) errors.push('on the page, but must not be: ' + JSON.stringify(bad));
  for (const re of c.notOnPageRe || []) { const m = pageText.match(new RegExp(re)); if (m) errors.push('on the page, matches ' + re + ': ' + JSON.stringify(m[0])); }
  // brandFree: no hardware brand and no "console" as a machine word in the
  // page's text or in a title, data-tip, placeholder or aria-label (Potluck
  // is generic). Fixture names such as console-1 or ps5-01 are data, not
  // words of the product, so a word followed by "-" is let through.
  if (c.brandFree) {
    const brand = /playstation|\bps[45]s?\b(?!-)|\borbis\b|\boberon\b|\bomen\b|alienware(?!-)|\bconsoles?\b(?!-)/i;
    const texts = [pageText];
    const walk = n => { if (n.getAttribute) for (const a of ['title', 'data-tip', 'placeholder', 'aria-label']) { const v = n.getAttribute(a); if (v) texts.push(v); } for (const k of n.childNodes || []) walk(k); };
    walk(document.body);
    for (const t of texts) { const m = t.match(brand); if (m) { errors.push('a brand or machine-type word on the page: ' + JSON.stringify(m[0]) + ' in ' + JSON.stringify(t.slice(Math.max(0, m.index - 60), m.index + 60))); break; } }
  }
  // links:[{text, path}]: somewhere on the page (shell included) a link with
  // that text whose href, resolved against the page's address, has that path.
  for (const l of c.links || []) {
    const found = document.body.querySelectorAll('a').filter(a => a.textContent.trim() === l.text);
    const paths = found.map(a => new URL(a.getAttribute('href') || '', location.href).pathname);
    if (!paths.includes(l.path)) errors.push(`no link ${JSON.stringify(l.text)} to ${l.path} (found ${found.length ? 'to ' + paths.join(', ') : 'none'})`);
  }
  let extra = '';
  if (live && route.id === 'nodes' && route.rest.length === 0) {
    const grid = outlet.querySelector('div.ngrid');
    const tiles = grid ? grid.children : [];
    const vn = ((store.state.status || {}).view || {}).nodes || [];
    const rows = tiles.map((t, i) => {
      const txt = t.textContent;
      const n = vn.find(x => txt.includes(x.hostname || x.id)) || {};
      return { name: n.hostname || n.id || '#' + i, local: !!n.local, excluded: !!n.excluded,
        reserved: txt.includes('Free for a new load') || txt.includes('Reserved for '), measured: txt.includes(' · measured ') };
    });
    extra = '\n     tiles ' + tiles.length + ': ' + rows.map(r => `${r.name}${r.local ? ' (local)' : ''}${r.excluded ? ' (excluded)' : ''} reserved-block=${r.reserved ? 'yes' : 'no'}${r.measured ? ' measured=yes' : ''}`).join('; ');
    const want = flag('--tiles');
    if (want && want !== true && tiles.length !== Number(want)) errors.push(`the Nodes page drew ${tiles.length} tiles, want ${want}`);
    if (flag('--reserved')) for (const r of rows) if (!r.excluded && !r.reserved) errors.push(`no reserved-against-measured block on ${r.name}`);
  }
  // a page left with unsaved changes would ask; the next case starts clean
  if (router.setLeaveGuard) router.setLeaveGuard(null);
  await go('#/__ui_exec_blank'); // unmounts the screen
  // a dialog or a popover left open by a case is closed the way a person would
  for (let i = 0; i < 3 && document.body.querySelector('div.backdrop, div.pop'); i++) {
    const ev = new FakeEvent('keydown', { key: 'Escape' });
    ev.target = document.body;
    document.dispatchEvent(ev);
    await settle();
  }
  if (errors.length) {
    failed++;
    console.log(`FAIL ${c.name}${extra}\n  ` + errors.join('\n  ').split('\n').slice(0, 30).join('\n  '));
  } else console.log(`ok   ${c.name} (${text.length} characters drawn${fetched.length ? '; fetched ' + [...new Set(fetched)].join(', ') : ''})${extra}`);
}
console.log(failed ? `FAILED: ${failed} of ${spec.cases.length} screens` : `PASS: ${spec.cases.length} screens drew without a runtime error`);
process.exit(failed ? 1 : 0);
