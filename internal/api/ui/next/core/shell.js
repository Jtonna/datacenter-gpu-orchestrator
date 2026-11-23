// The shell of the new UI (design: Components board 2): a rail of sections,
// 60 wide, with the others at its foot; a section menu, 236 wide, that lists
// the pages of the current section and ends with the fleet status card and
// the name that signs changes; then the screen. On a phone (D37) the rail is
// a top bar and the section menu a drawer under it.
//
// There is no "Live" indicator: a healthy connection shows nothing. When the
// live channel is down or the fleet feed is stale, a slim bar says so above
// the screen, and goes away by itself.
//
// Left out until their backend ships (the plan, section 3): the Ask Potluck
// rail item (U9), the System health heart and its popup (U7) and Alerts and
// webhooks (U7). No item is drawn that leads nowhere.
//
// Overview is the home page (Q76): the mark and the Monitor item open it,
// and it opens with the section menu closed (Q9), as the design draws it;
// the Monitor item opens the menu again while on it, for that visit only.
//
// Every export of the classic core/shell.js is here with the same name:
// NAV, connectionFacts, connection, setShellSlot, startShell.

import { h, morph, icon } from './dom.js';
import { state, on } from './store.js';
import { href, onRoute, parse } from './router.js';
import { getActor, setActor } from './api.js';
import { dur, durShort, plural, num } from './format.js';
import { promptText } from './ui.js';
import { LOGS_PAGE, logsOn, logsURL } from './logs.js';
import { agentsMode, agentOf } from './nodeactions.js';

const I = {
  monitor: 'M2.5 10.5h3.2l2.1-5.5 4 10.5 2.2-5h3.5',
  zones: [{ tag: 'circle', cx: 10, cy: 10, r: 7 }, { tag: 'circle', cx: 10, cy: 10, r: 2.5 }],
  models: 'M10 2.5l6.5 3.5v8L10 17.5 3.5 14V6zM3.5 6L10 9.5 16.5 6M10 9.5v8',
  benchmarks: 'M4 17V9M10 17V3M16 17v-6',
  configure: ['M3 6h8M15 6h2M3 14h2M9 14h8', { tag: 'circle', cx: 13, cy: 6, r: 2 }, { tag: 'circle', cx: 7, cy: 14, r: 2 }],
  mcp: 'M7 3v4M13 3v4M5 7h10v3a5 5 0 01-10 0zM10 15v2.5',
  about: [{ tag: 'circle', cx: 10, cy: 10, r: 7 }, 'M10 9v5M10 6v.5'],
  menu: 'M3 5h14M3 10h14M3 15h14',
  close: 'M12 5l-5 5 5 5',
};

// The sections and their pages. A page is a view id (views/<id>.js) and
// optionally the first part of its rest ('runs' for #/benchmarks/runs).
// `also` lists other view ids that belong to the section (the classic
// Nodes page is reachable under Zones). `foot` sections sit at the foot of
// the rail.
export const NAV = [
  { id: 'monitor', label: 'Monitor', icon: I.monitor, also: ['report'], pages: [
    { id: 'overview', label: 'Overview' },
    // the full request list behind the Overview's "All 203" (no mockup yet)
    { id: 'requests', label: 'Requests' },
    { id: 'queue', label: 'Queue' },
    { id: 'activity', label: 'Activity' },
    { id: 'placement', label: 'Placement preview' },
  ] },
  { id: 'zones', label: 'Zones', icon: I.zones, also: ['nodes'], pages: [
    { id: 'zones', label: 'Zones', desc: 'Every machine, its state and what it is doing' },
    { id: 'nodes', label: 'Machine details', desc: 'One card per machine, with its memory and checks' },
  ] },
  { id: 'models', label: 'Models', icon: I.models, pages: [
    { id: 'models', label: 'Models', desc: 'Model files, what is loaded, and their profiles' },
  ] },
  { id: 'benchmarks', label: 'Benchmarks', icon: I.benchmarks, pages: [
    { id: 'benchmarks', label: 'Overview', match: r => !['runs', 'trends'].includes(r.rest[0]) },
    { id: 'benchmarks', rest: ['runs'], label: 'Runs', match: r => r.rest[0] === 'runs' },
    { id: 'benchmarks', rest: ['trends'], label: 'Aggregates', match: r => r.rest[0] === 'trends' },
  ] },
  { id: 'configure', label: 'Configure', icon: I.configure, foot: true, pages: [
    { id: 'profiles', label: 'Profiles', desc: 'Models, context size and memory need' },
    { id: 'tiers', label: 'Tiers and zones', desc: 'Priority, link speed, switch zones off' },
    { id: 'settings', label: 'Timing defaults', desc: 'Holds, timeouts and settle time' },
    { id: 'audit', label: 'Audit', desc: 'Every change, who made it and why' },
  ] },
  { id: 'mcp', label: 'MCP', icon: I.mcp, foot: true, pages: [
    { id: 'mcp', label: 'MCP', desc: 'The MCP server and the keys of AI agents' },
  ] },
  { id: 'about', label: 'About', icon: I.about, foot: true, pages: [
    { id: 'about', label: 'About', desc: 'Release, connection and data on disk' },
    // Logs is not a view of this page: it opens the log store's own search
    // page in a new tab (core/logs.js), and is there only while logging is on.
    { id: 'logs', label: 'Logs', external: true },
  ] },
];

// sectionOf(route) -> the section a route belongs to, or null.
export function sectionOf(route) {
  return NAV.find(s => s.pages.some(p => p.id === route.id) || (s.also || []).includes(route.id)) || null;
}

const pageOn = (p, route) => p.id === route.id && (!p.match || p.match(route));

// The mark: a pot, from the design's rail.
const mark = size => h('svg', { width: size, height: size, viewBox: '0 0 32 32', role: 'img', 'aria-label': 'Potluck, Datacenter GPU Orchestrator' },
  h('rect', { width: 32, height: 32, rx: 9, fill: '#0f6b4a' }),
  h('path', { d: 'M8 15h16a8 8 0 01-16 0z', fill: '#fff' }),
  h('path', { d: 'M11 10.5h10', stroke: '#fff', 'stroke-width': 2.6, 'stroke-linecap': 'round' }));

// What the counts in the menus show, from the live status.
function counts() {
  const st = state.status;
  if (!st) return {};
  const waiting = (st.queue || []).reduce((n, e) => n + Math.max(1, e.waiters || 0), 0);
  const bad = ((st.view && st.view.nodes) || []).filter(n => !n.excluded && (!n.online || n.quarantined || (n.health && n.health.state && n.health.state !== 'ok'))).length;
  return { queue: waiting || null, zonesBad: bad || null };
}

// connectionFacts() -> {dot, title, note}: the state of the connection in
// words, healthy or not (the About screen lists it among its facts).
export function connectionFacts() {
  const st = state.status, view = st && st.view;
  if (state.conn === 'connecting') return { dot: 'off pulse', title: 'Connecting…', note: 'Waiting for the scheduler' };
  if (state.conn === 'reconnecting') {
    const since = state.statusAt ? durShort((Date.now() - state.statusAt) / 1000) + ' ago' : 'never';
    return { dot: 'warn pulse', title: 'Reconnecting…', note: 'Last update ' + since };
  }
  if (!view) return { dot: 'pulse', title: 'Live', note: 'Waiting for the first status' };
  const age = view.age >= 0 ? view.age + (Date.now() - state.statusAt) / 1000 : null;
  if (view.stale) return { dot: 'warn', title: 'Live', note: 'Fleet feed stale' + (age != null ? ' · ' + dur(Math.round(age)) + ' old' : ''), warn: true };
  if (!view.connected) return { dot: 'warn', title: 'Live', note: 'Fleet feed disconnected', warn: true };
  // Membership agents has no feed (never stale, never disconnected): say where the nodes come from.
  if (agentsMode(st)) return { dot: '', title: 'Live', note: 'Nodes come from agent registrations' };
  return { dot: '', title: 'Live', note: 'Fleet feed fresh' + (age != null ? ' · updated ' + durShort(age) + ' ago' : '') };
}

// How long the first connection may take before the page says anything.
const CONNECT_GRACE_MS = 2500;

// connection() -> null when all is well, else {title, note}: what is wrong
// with the picture on the screen, in the order that matters.
export function connection() {
  const st = state.status, view = st && st.view;
  if (state.conn === 'connecting') {
    if (Date.now() - state.connSince < CONNECT_GRACE_MS) return null;
    return { title: 'Connecting to the scheduler…', note: 'Nothing is shown until it answers. Check that it is running and reachable from this device.' };
  }
  if (state.conn === 'reconnecting') {
    const since = state.statusAt ? durShort((Date.now() - state.statusAt) / 1000) + ' ago' : 'never';
    return { title: 'The connection to the scheduler was lost.', note: `What you see is the last known state (updated ${since}). It updates again once the connection is back.` };
  }
  if (st && st.shutting_down) return { title: 'The scheduler is shutting down.', note: 'It finishes the requests in flight and takes no new ones.' };
  if (!view) return null;
  const age = view.age >= 0 ? view.age + (Date.now() - state.statusAt) / 1000 : null;
  if (view.stale) return { title: 'The fleet feed is stale' + (age != null ? ` (${dur(Math.round(age))} old).` : '.'), note: 'Remote nodes are not used for new loads until it recovers; models that are already loaded keep serving.' };
  if (!view.connected) return { title: 'The fleet feed is disconnected.', note: 'The node figures may be old. The scheduler keeps trying to reconnect.' };
  return null;
}

// fleetFacts() -> {dot, title, note} of the fleet status card: how many zones
// (machines) are up, how many are switched off (excluded), and how fresh the
// picture is. While the connection is not live it says so instead.
export function fleetFacts() {
  const st = state.status;
  const nodes = (st && st.view && st.view.nodes) || null;
  if (state.conn !== 'live' || !nodes) {
    const c = connectionFacts();
    return { dot: c.dot, title: c.title, note: c.note };
  }
  const off = nodes.filter(n => n.excluded || agentExcluded(st, n));
  const on_ = nodes.filter(n => !off.includes(n));
  const up = on_.filter(n => n.online).length;
  const updated = state.statusAt ? 'Updated ' + durShort(Math.max(0, (Date.now() - state.statusAt) / 1000)) + ' ago' : 'Waiting for the first status';
  return {
    dot: on_.length && up < on_.length ? 'warn' : '',
    title: on_.length ? `${num(up)} of ${plural(on_.length, 'zone')} up` : 'No zones yet',
    note: (off.length ? `${num(off.length)} off · ` : '') + updated,
  };
}
// A machine whose agent facts report the exclusion although the view does not.
const agentExcluded = (st, n) => { const a = agentOf(st, n); return !!(a && a.excluded); };

const slots = {};
const narrow = window.matchMedia('(max-width:860px)');
let strip = null; // the slot box above the screen, used at phone width and while the menu is closed

// setShellSlot('run', node) shows a node in the shell: in the section menu
// above the fleet card, or (at phone width, or while the menu is closed) in a
// strip above the screen. null removes it. The node is yours: update it in
// place (morph into it); the shell only decides where it sits and never
// re-creates or re-wraps it.
export function setShellSlot(name, node) {
  if (node) slots[name] = node; else delete slots[name];
  placeSlots();
}

// placeSlots moves the slot nodes into the box of the current layout. The
// boxes are left alone by morph (data-keep), so a node stays where it is put.
function placeSlots() {
  if (!root) return;
  const inMenu = !narrow.matches && !root.classList.contains('menu-closed');
  const menuBox = root.querySelector('.menu .slotbox');
  const box = inMenu ? menuBox : strip;
  if (!box) return;
  const want = Object.keys(slots).map(k => slots[k]);
  for (const n of want) if (n.parentNode !== box) box.appendChild(n);
  for (const other of [strip, menuBox]) {
    if (!other) continue;
    for (const c of Array.from(other.children)) if (!want.includes(c) || other !== box) c.remove();
  }
}

const CLOSED_KEY = 'llm-scheduler.menu-closed';
function menuClosed() { try { return localStorage.getItem(CLOSED_KEY) === '1'; } catch { return false; } }
// The Overview opens with the menu closed (Q9); opening it there lasts until
// the page is left, and is not kept.
let overviewMenuOpen = false;
const OPENS_CLOSED = ['overview'];
const menuShut = () => (OPENS_CLOSED.includes(route.id) ? !overviewMenuOpen : menuClosed());
function setMenuClosed(v) {
  if (OPENS_CLOSED.includes(route.id)) overviewMenuOpen = !v;
  else try { localStorage.setItem(CLOSED_KEY, v ? '1' : '0'); } catch { /* private mode */ }
  root.classList.toggle('menu-closed', menuShut());
  draw();
}

let root = null, route = { id: 'overview', rest: [], query: {} }, connBar = null;

function railItem(s, current) {
  const on_ = current && current.id === s.id;
  const first = s.pages.find(p => !p.external);
  return h('a', { class: 'rb' + (on_ ? ' on' : ''), href: href(first.id, first.rest), 'aria-label': s.label, 'data-label': s.label,
    'aria-current': on_ ? 'page' : null, key: 'rb-' + s.id,
    // the current section's item opens its menu again when it was closed
    onclick: e => { if (on_ && root.classList.contains('menu-closed') && !narrow.matches) { e.preventDefault(); setMenuClosed(false); } closeDrawer(); } },
  icon(s.icon));
}

function railNode() {
  const cur = sectionOf(route);
  return [
    h('a.mark', { href: href('overview'), 'aria-label': 'Potluck, home', key: 'mark' }, mark(32)),
    NAV.filter(s => !s.foot).map(s => railItem(s, cur)),
    h('span.gap', { 'aria-hidden': 'true', key: 'gap' }),
    NAV.filter(s => s.foot).map(s => railItem(s, cur)),
    // the classic UI, which stays at /ui/classic/ until the owner retires it
    h('a.uiswitch', { href: 'classic/', key: 'classic', title: 'Open the classic UI (/ui/classic/)' }, 'Classic UI'),
    h('button', { class: 'rb menu-open-btn', type: 'button', key: 'menu-btn', 'aria-label': 'Menu', 'aria-expanded': root.classList.contains('menu-open') ? 'true' : 'false',
      onclick: () => { root.classList.toggle('menu-open'); draw(); } }, icon(I.menu)),
  ];
}

function menuItem(p, c) {
  if (p.external) {
    if (p.id !== 'logs' || !logsOn(state.status)) return null;
    return h('a.ni', { href: logsURL('*'), target: '_blank', rel: 'noopener', title: 'Search the central log in a new tab (' + LOGS_PAGE + ')', onclick: closeDrawer, key: 'logs' },
      p.label, h('span.ext', { 'aria-hidden': 'true' }, '↗'));
  }
  const on_ = pageOn(p, route);
  let nb = null;
  if (p.id === 'queue' && c.queue) nb = h('span.nb', { title: plural(c.queue, 'request') + ' waiting' }, num(c.queue));
  if (p.id === 'zones' && c.zonesBad) nb = h('span.nb.alert', { title: plural(c.zonesBad, 'zone') + ' need a look' }, num(c.zonesBad));
  const attrs = { href: href(p.id, p.rest), 'aria-current': on_ ? 'page' : null, onclick: closeDrawer, key: 'p-' + p.id + (p.rest ? '-' + p.rest.join('-') : '') };
  if (p.desc) return h('a', Object.assign({ class: 'nd' + (on_ ? ' on' : '') }, attrs), h('b', p.label, nb), h('span', p.desc));
  return h('a', Object.assign({ class: 'ni' + (on_ ? ' on' : '') }, attrs), p.label, nb);
}

function menuNode() {
  const s = sectionOf(route) || NAV[0];
  const c = counts();
  const f = fleetFacts();
  return [
    h('div.menu-head', { key: 'head' },
      h('div', h('b', s.label), h('span', 'Potluck')),
      h('button.menu-close', { type: 'button', 'aria-label': 'Close menu', title: 'Close menu', onclick: () => setMenuClosed(true) }, icon(I.close))),
    h('nav', { 'aria-label': s.label + ' pages', key: 'pages', style: 'display:flex;flex-direction:column;gap:4px' }, s.pages.map(p => menuItem(p, c))),
    h('div.menu-foot', { key: 'foot' },
      h('div.slotbox', { key: 'slots', 'data-keep': '' }),
      h('div.fleet', { role: 'status' }, h('b', h('span', { class: 'dot ' + (f.dot || '') }), f.title), h('span', f.note)),
      h('div.actor', h('span', 'Changes are signed as ', h('b.txt2', getActor())),
        h('button', { type: 'button', onclick: changeActor }, 'Change'))),
  ];
}

function closeDrawer() { if (root) root.classList.remove('menu-open'); }

function draw() {
  if (!root) return;
  const conn = connection();
  if (connBar) {
    connBar.hidden = !conn;
    if (conn) morph(connBar, [h('span.dot.pulse'), h('span', h('b', conn.title), ' ', conn.note)]);
  }
  morph(root.querySelector('.rail'), railNode());
  morph(root.querySelector('.menu'), menuNode());
  placeSlots();
}

async function changeActor() {
  const v = await promptText({
    title: 'Your name for the audit trail',
    intro: 'Every change made from this page is recorded with this name, so it is clear later who changed what. It is kept in this browser only.',
    label: 'Name', value: getActor() === 'web' ? '' : getActor(), placeholder: 'for example alex', maxlength: 64,
  });
  if (v == null) return;
  setActor(v);
  draw();
}

export function startShell(app) {
  root = app;
  // the content column: the connection warning, the slot strip, then the router's outlet
  const main = root.querySelector('.main');
  connBar = h('div.connbar', { role: 'status', 'aria-live': 'polite', hidden: true });
  strip = h('div.slotbox.topslots', { 'data-keep': '' });
  const stage = h('div.stage');
  main.replaceWith(stage);
  stage.append(connBar, strip, main);
  route = parse(location.hash);
  root.classList.toggle('menu-closed', menuShut());
  narrow.addEventListener('change', placeSlots);
  onRoute(r => {
    if (r.id !== route.id) overviewMenuOpen = false;
    route = r;
    root.classList.toggle('menu-closed', menuShut());
    closeDrawer(); draw();
  });
  on(['status', 'conn', 'hello', 'tick'], draw);
  draw();
}
