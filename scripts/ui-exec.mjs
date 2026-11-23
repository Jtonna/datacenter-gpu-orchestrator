#!/usr/bin/env node
// ui-exec.mjs: runs screens of the web UI (internal/api/ui) under node, on a
// small fake DOM, and reports every runtime error. `node --check` only parses
// a module; this executes the rendering, so a reference to an undeclared name
// or a field read on undefined is found without a browser.
//
// usage: node scripts/ui-exec.mjs cases.json
//        node scripts/ui-exec.mjs --base http://HOST:PORT [--ui next|classic] [--tiles N] [--reserved] [--local-ui]
//
// A server serves two UIs: the new shell at /ui/ and the classic UI at
// /ui/classic/. --base checks both, each in its own node process, and fails
// when either fails; --ui checks one. Cases run the classic UI unless the
// cases file says "shell": "next".
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
//    "shell": "next",                              // the new shell (ui/next laid over the classic files); default classic
//    "modules": ["views/nodes.js", ...],          // imported before the cases (they register their views)
//    "fetch": {"/api/profiles": {...}, ...},       // answers of GET/POST by path (query ignored); others are 404
//    "base": "http://127.0.0.1:8790",              // or: GET requests go to this server (cmd/devcluster), writes are refused
//    "cases": [{"name": "...", "route": "#/nodes", "status": {...} or "fetch", "fetch": {...}, "expect": ["text", ...],
//               "absent": ["text that must not be drawn", ...], "none": ["css selector that must match nothing", ...],
//               "links": [{"text": "Classic UI", "path": "/ui/classic/"}, ...]}]}
//
// With "base", "status": "fetch" takes GET <base>/api/status.
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
// The page's address: /ui/ for the new shell, /ui/classic/ for the classic UI
// (set before any UI module is imported; core/api.js reads it once).
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

const argv = process.argv.slice(2);
const flag = k => { const i = argv.indexOf(k); return i < 0 ? null : (argv[i + 1] && !argv[i + 1].startsWith('--') ? argv[i + 1] : true); };
const live = flag('--base');
const here = path.dirname(fileURLToPath(import.meta.url));
// The two UIs a server serves: the new shell at /ui/ and the classic UI at
// /ui/classic/ (internal/api/uifiles.go). --base checks both, one after the
// other, each in its own node process (each loads its own modules); --ui
// next|classic checks one.
const UIS = { next: '/ui/', classic: '/ui/classic/' };
const LIVE_SCREENS = {
  classic: ['models', 'nodes', 'queue', 'activity', 'placement', 'benchmarks', 'benchmarks/runs', 'benchmarks/trends', 'profiles', 'tiers', 'settings', 'audit', 'mcp', 'about'],
  next: ['zones', 'models', 'nodes', 'queue', 'activity', 'placement', 'benchmarks', 'benchmarks/runs', 'benchmarks/trends', 'profiles', 'tiers', 'settings', 'audit', 'mcp', 'about'],
};
if (live && !flag('--ui')) {
  let bad = 0;
  for (const which of Object.keys(UIS)) {
    const r = spawnSync(process.execPath, [fileURLToPath(import.meta.url), ...argv, '--ui', which], { encoding: 'utf8' });
    process.stdout.write(`== ${which === 'next' ? 'the new shell' : 'the classic UI'} (${UIS[which]})\n` + (r.stdout || '') + (r.stderr || ''));
    if (r.status !== 0) bad++;
  }
  console.log(bad ? `FAILED: ${bad} of 2 UIs` : 'PASS: both UIs');
  process.exit(bad ? 1 : 0);
}
// Which UI this run executes: the cases' "shell" ("next" or "classic", the
// default), or --ui with --base.
const which = live ? String(flag('--ui')) : null;
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
if (live && !UIS[which]) { console.log(`FAIL --ui ${which}: give next or classic`); process.exit(1); }
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
const shell = spec.shell === 'next' ? 'next' : 'classic';
location.pathname = UIS[shell];
location.href = location.origin + UIS[shell];
// The checkout's UI runs from a copy in a temporary folder with its own
// package.json ({"type":"module"}): node takes a module's type from the
// nearest package.json above it, and one without "type" above the checkout
// (a user Temp folder, say) makes it warn on every module, which counts as
// an error here. The mirror of --base has its own package.json already.
// The new shell's copy is what the server serves below /ui/: the classic
// files with the files of ui/next/ laid over them (internal/api/uifiles.go).
function isolatedCopy(src, which) {
  const dir = mkdtempSync(path.join(os.tmpdir(), 'ui-exec-local-'));
  process.on('exit', () => { try { rmSync(dir, { recursive: true, force: true }); } catch { /* left in the temp folder */ } });
  const ui = path.join(dir, 'ui');
  cpSync(src, ui, { recursive: true });
  rmSync(path.join(ui, 'next'), { recursive: true, force: true });
  if (which === 'next') cpSync(path.join(src, 'next'), ui, { recursive: true });
  writeFileSync(path.join(dir, 'package.json'), '{"type":"module"}\n');
  return ui;
}
const ui = uiDir === spec.ui && live && !flag('--local-ui') ? path.resolve(spec.ui) : isolatedCopy(path.resolve(spec.ui), shell);
const mod = rel => import(pathToFileURL(path.join(ui, rel)).href);
const settle = async () => { for (let i = 0; i < 20; i++) await new Promise(r => setTimeout(r, 0)); };

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
  location.hash = '#/__ui_exec_blank';
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
  location.hash = '#/__ui_exec_blank';
  router.start(outlet);
}
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
  for (const bad of c.absent || []) if (text.includes(bad)) errors.push('text that must not be there: ' + JSON.stringify(bad) + ' in ' + JSON.stringify(text.slice(Math.max(0, text.indexOf(bad) - 80), text.indexOf(bad) + 80)));
  for (const sel of c.none || []) if (outlet.querySelector(sel)) errors.push('an element that must not be there: ' + sel);
  // links: [{text, path}]: somewhere on the page (shell included) a link with
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
  await go('#/__ui_exec_blank'); // unmounts the screen
  if (errors.length) {
    failed++;
    console.log(`FAIL ${c.name}${extra}\n  ` + errors.join('\n  ').split('\n').slice(0, 30).join('\n  '));
  } else console.log(`ok   ${c.name} (${text.length} characters drawn${fetched.length ? '; fetched ' + [...new Set(fetched)].join(', ') : ''})${extra}`);
}
console.log(failed ? `FAILED: ${failed} of ${spec.cases.length} screens` : `PASS: ${spec.cases.length} screens drew without a runtime error`);
process.exit(failed ? 1 : 0);
