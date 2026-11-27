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
// The rail as the design draws it (Components board 2, the rail of every
// page): the mark (not a link), Monitor, Zones, Models, Benchmarks; then at
// its foot Ask Potluck, MCP and the System health heart; and the gear
// "Settings" (Q69), which opens on Settings and holds Profiles, Test
// profiles, Tiers and zones, Placement preview, Audit, About (the link to the classic UI is gone with it, 24 Nov). Rail items are buttons with a
// title, as drawn. Ask Potluck opens a drawer at the right edge that says it
// is not built yet; the heart opens the health popup (core/health.js), and
// its dot is the worst row's colour.
//
// Overview is the home page (Q76): the Monitor item opens it, and it opens
// with the section menu closed (Q9), as the design draws it; the Monitor
// item opens the menu again while on it, for that visit only.
//
// Every export of the classic core/shell.js is here with the same name:
// NAV, connectionFacts, connection, setShellSlot, startShell.

import { h, morph, icon } from './dom.js';
import { state, on } from './store.js';
import { href, onRoute, parse, navigate } from './router.js';
import { healthPopup, heart, heartLabel, readHealth } from './health.js';
import { dur, durShort, plural, num } from './format.js';
import { LOGS_PAGE, logsOn, logsURL } from './logs.js';
import { agentsMode, agentOf } from './nodeactions.js';
import { onPanelHold, panelHoldsMenu } from './panel.js';

const I = {
  monitor: 'M2.5 10.5h3.2l2.1-5.5 4 10.5 2.2-5h3.5',
  zones: [{ tag: 'circle', cx: 10, cy: 10, r: 7 }, { tag: 'circle', cx: 10, cy: 10, r: 2.5 }],
  models: 'M10 2.5l6.5 3.5v8L10 17.5 3.5 14V6zM3.5 6L10 9.5 16.5 6M10 9.5v8',
  benchmarks: 'M4 17V9M10 17V3M16 17v-6',
  configure: ['M3 6h8M15 6h2M3 14h2M9 14h8', { tag: 'circle', cx: 13, cy: 6, r: 2 }, { tag: 'circle', cx: 7, cy: 14, r: 2 }],
  mcp: 'M7 3v4M13 3v4M5 7h10v3a5 5 0 01-10 0zM10 15v2.5',
  about: [{ tag: 'circle', cx: 10, cy: 10, r: 7 }, 'M10 9v5M10 6v.5'],
  ask: [{ tag: 'rect', x: 3, y: 3, width: 14, height: 14, rx: 4 }, 'M11.5 6.5l-3 7'],
  heart: 'M10 16.5S3.5 12.5 3.5 7.8A3.3 3.3 0 0110 6.3a3.3 3.3 0 016.5 1.5c0 4.7-6.5 8.7-6.5 8.7z',
  gear: [{ tag: 'circle', cx: 10, cy: 10, r: 2.4 }, 'M10 2.8v2M10 15.2v2M2.8 10h2M15.2 10h2M4.9 4.9l1.4 1.4M13.7 13.7l1.4 1.4M4.9 15.1l1.4-1.4M13.7 6.3l1.4-1.4', { tag: 'circle', cx: 10, cy: 10, r: 5 }],
  x: 'M5 5l10 10M15 5L5 15',
  menu: 'M3 5h14M3 10h14M3 15h14',
  close: 'M12 5l-5 5 5 5',
};

// The sections and their pages. A page is a view id (views/<id>.js) and
// optionally the first part of its rest ('runs' for #/benchmarks/runs).
// `also` lists other view ids that belong to the section (the classic
// Nodes page is reachable under Zones; the request list behind the
// Overview's "All N" and the 24-hour report under Monitor). `foot` sections
// sit at the foot of the rail. A page with `link` is another section's page
// listed here too (Models in the Monitor menu, as drawn); it does not make
// this section current. `href` is a plain link (the classic UI).
export const NAV = [
  { id: 'monitor', label: 'Monitor', icon: I.monitor, also: ['report', 'requests'], pages: [
    { id: 'overview', label: 'Overview' },
    { id: 'models', label: 'Models', link: true },
    { id: 'queue', label: 'Queue' },
    { id: 'activity', label: 'Activity' },
  ] },
  { id: 'zones', label: 'Zones', icon: I.zones, also: ['nodes'], pages: [
    { id: 'zones', label: 'Zones', desc: 'Every machine, its state and what it is doing' },
    { id: 'nodes', label: 'Machine details', desc: 'One card per machine, with its memory and checks' },
  ] },
  { id: 'models', label: 'Models', icon: I.models, pages: [
    { id: 'models', label: 'Models', desc: 'The models available to you' },
  ] },
  { id: 'benchmarks', label: 'Benchmarks', icon: I.benchmarks, pages: [
    // the boards' menu (Bench-Overview): Overview, Benchmarks N, Runs N,
    // Settings (the Settings page's Benchmarks group). The counts are the
    // benchmark pages' own copy (state.benchCounts), shown once read.
    { id: 'benchmarks', label: 'Overview', match: r => !['runs', 'all', 'new', 'benchmark', 'group'].includes(r.rest[0]) },
    // the editors (#/benchmarks/new..., /benchmark/<id>/edit, /group/...) are pages of the list
    { id: 'benchmarks', rest: ['all'], label: 'Benchmarks', match: r => ['all', 'new', 'benchmark', 'group'].includes(r.rest[0]), count: () => state.benchCounts && state.benchCounts.benchmarks, countWord: 'group or benchmark', countWords: 'groups and benchmarks' },
    { id: 'benchmarks', rest: ['runs'], label: 'Runs', match: r => r.rest[0] === 'runs', count: () => state.benchCounts && state.benchCounts.runs, countWord: 'run' },
    { id: 'settings', rest: ['bench'], label: 'Settings', link: true },
  ] },
  { id: 'mcp', label: 'MCP', icon: I.mcp, foot: true, pages: [
    { id: 'mcp', label: 'MCP', desc: 'The MCP server and the keys of AI agents' },
  ] },
  // the gear (Q69): everything that configures Potluck or describes it. The
  // gear opens the section's first page: Settings, the cluster's own
  // settings (the owner's walkthrough, build-92), not Profiles.
  { id: 'settings', label: 'Settings', icon: I.gear, foot: true, gear: true, pages: [
    { id: 'settings', label: 'Settings', desc: 'Holds, timeouts, settle time and the other defaults' },
    { id: 'profiles', label: 'Profiles', desc: 'Models, context size and memory need' },
    // under Profiles and out of the way (profiles plan, section 9)
    { id: 'test-profiles', label: 'Test profiles', desc: 'Profiles for trials and benchmarks, kept apart' },
    { id: 'tiers', label: 'Tiers and zones', desc: 'Priority, link speed, switch zones off' },
    // until it folds into the zone panel's "Load a model" (D8)
    { id: 'placement', label: 'Placement preview', desc: 'Where a profile would load now, and why' },
    { id: 'audit', label: 'Audit', desc: 'Every change, who made it and why' },
    { id: 'about', label: 'About', desc: 'Release, connection and data on disk' },
    // Logs is not a view of this page: it opens the log store's own search
    // page in a new tab (core/logs.js), and is there only while logging is on.
    { id: 'logs', label: 'Logs', external: true },
  ] },
];

// sectionOf(route) -> the section a route belongs to, or null.
export function sectionOf(route) {
  return NAV.find(s => s.pages.some(p => !p.link && !p.href && p.id === route.id) || (s.also || []).includes(route.id)) || null;
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
  // models: the loaded instances, as the classic rail counted them
  return { models: (st.instances || []).length || null, queue: waiting || null, zonesBad: bad || null };
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
  if (view.stale) return { title: 'The fleet feed is stale' + (age != null ? ` (${dur(Math.round(age))} old).` : '.'), note: 'Remote zones are not used for new loads until it recovers; models that are already loaded keep serving.' };
  if (!view.connected) return { title: 'The fleet feed is disconnected.', note: 'The zone figures may be old. The scheduler keeps trying to reconnect.' };
  return null;
}

// fleetFacts() -> {dot, title, note} of the fleet status card, in the one
// sentence of the zone counts (as the health popup and the Zones page's
// ALL pill say it): "221 of 232 reachable", then "11 down · 10 off". A
// zone's state is the server's zone_state (GET /api/nodes `counts` reads
// the same); without it (a fleet feed) the same reading made here: lost or
// set aside is down, excluded is off, and off zones are reachable. While
// the connection is not live it says so instead.
export function fleetFacts() {
  const st = state.status;
  const nodes = (st && st.view && st.view.nodes) || null;
  if (state.conn !== 'live' || !nodes) {
    const c = connectionFacts();
    return { dot: c.dot, title: c.title, note: c.note };
  }
  const stateOf = n => {
    const a = agentOf(st, n);
    if (a && a.zone_state) return a.zone_state;
    if (n.excluded || (a && a.excluded)) return 'off';
    if (!n.online) return 'lost';
    if ((st.quarantines || []).some(q => q.node === n.id)) return 'set_aside';
    return 'up';
  };
  const states = nodes.map(stateOf).filter(s => s !== 'removed');
  const down = states.filter(s => s === 'lost' || s === 'set_aside').length;
  const off = states.filter(s => s === 'off').length;
  const updated = state.statusAt ? 'Updated ' + durShort(Math.max(0, (Date.now() - state.statusAt) / 1000)) + ' ago' : 'Waiting for the first status';
  return {
    dot: down ? 'warn' : '',
    title: states.length ? `${num(states.length - down)} of ${num(states.length)} reachable` : 'No zones yet',
    note: (down ? `${num(down)} down · ` : '') + (off ? `${num(off)} off · ` : '') + updated,
  };
}

const slots = {};
const narrow = window.matchMedia('(max-width:860px)');
let strip = null; // the slot box above the screen, used at phone width and while the menu is closed

// setShellSlot('run', node) shows a node in the shell: in the section menu
// at its foot, or (at phone width, or while the menu is closed) in a
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
  // the Benchmarks pages draw the run themselves (the Overview's run card,
  // the list's "Running now" row; board Bench-Group has no strip): there the
  // strip stays empty
  const box = inMenu ? menuBox : route.id === 'benchmarks' ? null : strip;
  if (!box && (inMenu || route.id !== 'benchmarks')) return;
  const want = Object.keys(slots).map(k => slots[k]);
  if (box) for (const n of want) if (n.parentNode !== box) box.appendChild(n);
  for (const other of [strip, menuBox]) {
    if (!other) continue;
    for (const c of Array.from(other.children)) if (!want.includes(c) || other !== box) c.remove();
  }
}

const CLOSED_KEY = 'llm-scheduler.menu-closed';
function menuClosed() { try { return localStorage.getItem(CLOSED_KEY) === '1'; } catch { return false; } }
// The Overview (Q9) and Zones (the Zones boards draw no section menu) open
// with the menu closed; opening it there lasts until the page is left, and
// is not kept.
let overviewMenuOpen = false;
const OPENS_CLOSED = ['overview', 'zones'];
const menuShut = () => (OPENS_CLOSED.includes(route.id) ? !overviewMenuOpen : menuClosed());
// While the health popup is open no section menu stands beside the rail, as
// the Health board draws it (its script is empty: the heart only opens the
// popup); closing the popup brings the menu back as it was. A side panel
// opened with closesMenu (the MCP page's) does the same while it is docked.
function applyMenu() { root.classList.toggle('menu-closed', menuShut() || surface === 'health' || panelHoldsMenu()); }
onPanelHold(() => { if (root) { applyMenu(); draw(); } });
function setMenuClosed(v) {
  if (OPENS_CLOSED.includes(route.id)) overviewMenuOpen = !v;
  else try { localStorage.setItem(CLOSED_KEY, v ? '1' : '0'); } catch { /* private mode */ }
  applyMenu();
  draw();
}

let root = null, route = { id: 'overview', rest: [], query: {} }, connBar = null, surfaceBox = null;

// A rail item is a button, as drawn: it opens the section's first page, or
// (on the current section, while its menu is closed) opens the menu again.
function railItem(s, current, c = {}) {
  const on_ = current && current.id === s.id;
  // the Models button carries the number of loaded models
  const nb = s.id === 'models' && c.models ? h('span.rbn', { 'aria-hidden': 'true', 'data-rbn': 'models' }, num(c.models)) : null;
  const label = nb ? `${s.label}, ${plural(c.models, 'model')} loaded` : s.label;
  const first = s.pages.find(p => !p.external && !p.href && !p.link);
  return h('button', { class: 'rb' + (on_ ? ' on' : ''), type: 'button', 'aria-label': label, title: label, 'data-label': s.label,
    'aria-current': on_ ? 'page' : null, key: 'rb-' + s.id, 'data-section': s.id,
    onclick: () => {
      closeSurfaces(); closeDrawer();
      if (on_ && menuShut() && !panelHoldsMenu() && !narrow.matches) { setMenuClosed(false); return; }
      navigate(first.id, first.rest || []);
    } },
  icon(s.icon), nb);
}

// The surfaces the rail opens over the page: the health popup and the Ask
// Potluck drawer, one at a time.
let surface = ''; // '' | 'health' | 'ask'
function openSurface(which) {
  surface = surface === which ? '' : which;
  if (surface === 'health') readHealth(draw);
  applyMenu();
  draw();
}
function closeSurfaces() { if (surface) { surface = ''; applyMenu(); draw(); } }

function railNode() {
  const cur = sectionOf(route);
  const hb = heart();
  const foot = NAV.filter(s => s.foot);
  const c = counts();
  return [
    h('span.mark', { key: 'mark' }, mark(32)),
    NAV.filter(s => !s.foot).map(s => railItem(s, cur, c)),
    h('span.gap', { 'aria-hidden': 'true', key: 'gap' }),
    h('button', { class: 'rb' + (surface === 'ask' ? ' open' : ''), type: 'button', key: 'rb-ask', 'data-surface': 'ask', 'aria-label': 'Ask Potluck', title: 'Ask Potluck', 'data-label': 'Ask Potluck',
      'aria-expanded': surface === 'ask' ? 'true' : 'false', onclick: () => openSurface('ask') }, icon(I.ask)),
    foot.filter(s => !s.gear).map(s => railItem(s, cur)),
    // the heart: its dot and its count are the server's (GET /api/health, the health topic)
    h('button', { class: 'rb' + (surface === 'health' ? ' open' : ''), type: 'button', key: 'rb-health', 'data-surface': 'health', 'data-issues': String(hb.issues),
      'aria-label': heartLabel(), title: 'System health', 'data-label': 'System health',
      'aria-expanded': surface === 'health' ? 'true' : 'false', onclick: () => openSurface('health') },
      icon(I.heart), hb.level ? h('span', { class: 'dotmark ' + hb.level, 'aria-hidden': 'true' }) : null),
    foot.filter(s => s.gear).map(s => railItem(s, cur)),
    h('button', { class: 'rb menu-open-btn', type: 'button', key: 'menu-btn', 'aria-label': 'Menu', 'aria-expanded': root.classList.contains('menu-open') ? 'true' : 'false',
      onclick: () => { root.classList.toggle('menu-open'); draw(); } }, icon(I.menu)),
  ];
}

// The Ask Potluck drawer (design: Chat boards, .drw2, 440 wide at the right
// edge). The assistant is not built yet: the drawer says so in one line.
function askNode() {
  return h('aside.drw2', { 'aria-label': 'Ask Potluck', key: 'ask' },
    h('div.dh2',
      h('h2', 'Ask Potluck'),
      h('span', { style: 'margin-left:auto' }),
      h('button.ib', { type: 'button', 'aria-label': 'Close Ask Potluck', title: 'Close Ask Potluck', onclick: closeSurfaces }, icon(I.x))),
    h('p.drw2-note', 'Ask Potluck is not built yet: here you will ask about zones, models and runs in plain words.'));
}

function surfaceNode() {
  if (surface === 'health') return healthPopup(closeSurfaces, to => { surface = ''; applyMenu(); location.hash = to; draw(); });
  if (surface === 'ask') return askNode();
  return [];
}

function menuItem(p, c) {
  if (p.href) return h('a.ni', { href: p.href, key: 'p-' + p.id, title: p.desc }, p.label);
  if (p.external) {
    if (p.id !== 'logs' || !logsOn(state.status)) return null;
    return h('a.ni', { href: logsURL('*'), target: '_blank', rel: 'noopener', title: 'Search the central log in a new tab (' + LOGS_PAGE + ')', onclick: closeDrawer, key: 'logs' },
      p.label, h('span.ext', { 'aria-hidden': 'true' }, '↗'));
  }
  const on_ = pageOn(p, route);
  let nb = null;
  if (p.id === 'models' && !p.link && c.models) nb = h('span.nb', { title: plural(c.models, 'model') + ' loaded' }, num(c.models));
  if (p.id === 'queue' && c.queue) nb = h('span.nb', { title: plural(c.queue, 'request') + ' waiting' }, num(c.queue));
  if (p.count) { const n = p.count(); if (n) nb = h('span.nb.cnt', { title: n === 1 ? '1 ' + p.countWord : num(n) + ' ' + (p.countWords || p.countWord + 's') }, num(n)); }
  if (p.id === 'zones' && c.zonesBad) nb = h('span.nb.alert', { title: plural(c.zonesBad, 'zone') + ' need a look' }, num(c.zonesBad));
  const attrs = { href: href(p.id, p.rest), 'aria-current': on_ && !p.link ? 'page' : null, onclick: closeDrawer, key: 'p-' + p.id + (p.rest ? '-' + p.rest.join('-') : '') };
  if (p.desc) return h('a', Object.assign({ class: 'nd' + (on_ ? ' on' : '') }, attrs), h('b', p.label, nb), h('span', p.desc));
  return h('a', Object.assign({ class: 'ni' + (on_ ? ' on' : '') }, attrs), p.label, nb);
}

function menuNode() {
  const s = sectionOf(route) || NAV[0];
  const c = counts();
  return [
    h('div.menu-head', { key: 'head' },
      h('div', h('b', s.label), h('span', 'Potluck')),
      h('button.menu-close', { type: 'button', 'aria-label': 'Close menu', title: 'Close menu', onclick: () => setMenuClosed(true) }, icon(I.close))),
    h('nav', { 'aria-label': s.label + ' pages', key: 'pages', style: 'display:flex;flex-direction:column;gap:4px' }, s.pages.map(p => menuItem(p, c))),
    // the foot holds the slots (the Benchmarks run card); no health card: the
    // heart in the rail carries it (the owner's note 2, 25 Nov)
    h('div.menu-foot', { key: 'foot' },
      h('div.slotbox', { key: 'slots', 'data-keep': '' })),
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
  if (surfaceBox) morph(surfaceBox, surfaceNode());
  placeSlots();
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
  // the popup and the drawer the rail opens, over the page
  surfaceBox = h('div.surfaces');
  root.appendChild(surfaceBox);
  document.addEventListener('keydown', e => { if (e.key === 'Escape') closeSurfaces(); });
  document.addEventListener('mousedown', e => {
    if (!surface || !e.target || !e.target.closest) return;
    if (e.target.closest('.surfaces') || e.target.closest('.rail')) return;
    closeSurfaces();
  }, true);
  // the health document: read at the start; after a reconnect the topic sends
  // it again (a server without the topic is read once more instead)
  readHealth(draw);
  on(['hello'], d => { if (!(d && Array.isArray(d.topics) && d.topics.includes('health'))) readHealth(draw); });
  route = parse(location.hash);
  // ?health=open on any page opens the popup with it (a link to the popup)
  if (route.query.health === 'open') openSurface('health');
  applyMenu();
  narrow.addEventListener('change', placeSlots);
  onRoute(r => {
    if (r.id !== route.id) overviewMenuOpen = false;
    if (r.id !== route.id && surface === 'health') surface = '';
    if (r.query.health === 'open' && surface !== 'health') { surface = 'health'; readHealth(draw); }
    route = r;
    applyMenu();
    closeDrawer(); draw();
  });
  on(['status', 'conn', 'hello', 'tick', 'events', 'health'], draw);
  draw();
}
