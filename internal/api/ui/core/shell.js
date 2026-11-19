// The app shell: the navigation pane (a drawer under a top bar on a phone),
// the connection indicator and the name that signs changes.

import { h, morph, icon } from './dom.js';
import { state, on } from './store.js';
import { href, onRoute, parse } from './router.js';
import { getActor, setActor } from './api.js';
import { dur, durShort, plural } from './format.js';
import { promptText } from './ui.js';

// The navigation. A view module registers under the same id (views/<id>.js).
export const NAV = [
  { group: 'Monitor', items: [
    { id: 'models', label: 'Models', icon: 'M10 2l7 4-7 4-7-4zM3 10l7 4 7-4M3 14l7 4 7-4' },
    { id: 'nodes', label: 'Nodes', icon: 'M3 3h6v6H3zM11 3h6v6h-6zM3 11h6v6H3zM11 11h6v6h-6z' },
    { id: 'queue', label: 'Queue', icon: 'M3 5h14M3 10h14M3 15h9' },
    { id: 'activity', label: 'Activity', icon: 'M2 10h4l2-6 4 12 2-6h4' },
  ] },
  { group: 'Plan', items: [
    { id: 'placement', label: 'Placement preview', icon: [{ tag: 'circle', cx: 10, cy: 10, r: 7 }, { tag: 'circle', cx: 10, cy: 10, r: 2.5 }] },
    { id: 'benchmarks', label: 'Benchmarks', icon: 'M4 17V9M10 17V3M16 17v-6' },
  ] },
  { group: 'Configure', items: [
    { id: 'profiles', label: 'Profiles', icon: ['M3 6h8M15 6h2M3 14h2M9 14h8', { tag: 'circle', cx: 13, cy: 6, r: 2 }, { tag: 'circle', cx: 7, cy: 14, r: 2 }] },
    { id: 'tiers', label: 'Tiers and nodes', icon: [{ tag: 'rect', x: 3, y: 3, width: 14, height: 6, rx: 1.5 }, { tag: 'rect', x: 3, y: 11, width: 14, height: 6, rx: 1.5 }] },
    { id: 'settings', label: 'Settings', icon: [{ tag: 'circle', cx: 10, cy: 10, r: 7 }, 'M10 6v4l3 2'] },
    { id: 'audit', label: 'Audit', icon: 'M6 3h8v14H6zM9 7h2M9 10h2M9 13h2M3 6v8M17 6v8' },
  ] },
  { group: 'System', items: [
    { id: 'about', label: 'About', icon: [{ tag: 'circle', cx: 10, cy: 10, r: 7 }, 'M10 9v5M10 6v.5'] },
  ] },
];

const mark = size => h('svg', { width: size, height: size, viewBox: '0 0 32 32', role: 'img', 'aria-label': 'LLM Scheduler' },
  h('rect', { width: 32, height: 32, rx: 9, fill: '#7ee0b0' }),
  h('path', { d: 'M8 10.5h16M8 16h10M8 21.5h13', stroke: '#0b0d0c', 'stroke-width': 2.8, 'stroke-linecap': 'round', fill: 'none' }));

// What the badges in the navigation show, from the live status.
function counts() {
  const st = state.status;
  if (!st) return {};
  const inst = st.instances || [];
  const waiting = (st.queue || []).reduce((n, e) => n + Math.max(1, e.waiters || 0), 0);
  const bad = ((st.view && st.view.nodes) || []).filter(n => !n.excluded && (!n.online || n.quarantined || (n.health && n.health.state && n.health.state !== 'ok'))).length;
  return { models: inst.length || null, queue: waiting || null, nodesBad: bad || null };
}

// connection() -> {dot, title, note} for the indicator.
export function connection() {
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
  return { dot: '', title: 'Live', note: 'Fleet feed fresh' + (age != null ? ' · updated ' + durShort(age) + ' ago' : '') };
}

const slots = {};
const narrow = window.matchMedia('(max-width:860px)');
let strip = null; // the slot box under the top bar, used at phone width

// setShellSlot('run', node) shows a node in the shell: in the pane above the
// connection card, or at phone width (where the pane is a closed drawer) in a
// strip under the top bar. null removes it. The node is yours: update it in
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
  const box = narrow.matches ? strip : root.querySelector('.pane .slotbox');
  if (!box) return;
  const want = Object.keys(slots).map(k => slots[k]);
  for (const n of want) if (n.parentNode !== box) box.appendChild(n);
  for (const other of [strip, root.querySelector('.pane .slotbox')]) {
    if (!other) continue;
    for (const c of Array.from(other.children)) if (!want.includes(c) || other !== box) c.remove();
  }
}

let root = null, activeId = 'models';

function navNode() {
  const c = counts();
  return NAV.map(g => h('nav.nav-group', { 'aria-label': g.group },
    h('div.gl', g.group),
    g.items.map(it => {
      let nb = null;
      if (it.id === 'models' && c.models) nb = h('span.nb', { title: plural(c.models, 'model') + ' loaded or loading' }, c.models);
      if (it.id === 'queue' && c.queue) nb = h('span.nb', { title: plural(c.queue, 'request') + ' waiting' }, c.queue);
      if (it.id === 'nodes' && c.nodesBad) nb = h('span.nb.alert', { title: plural(c.nodesBad, 'node') + ' need a look' }, c.nodesBad);
      return h('a', { class: 'ni' + (it.id === activeId ? ' on' : ''), href: href(it.id), 'aria-current': it.id === activeId ? 'page' : null, onclick: closeMenu },
        icon(it.icon), it.label, nb);
    })));
}

function brandSub() {
  const nodes = (state.status && state.status.view && state.status.view.nodes) || null;
  if (!nodes) return 'Model placement';
  const usable = nodes.filter(n => !n.excluded);
  return `${plural(usable.length, 'node')} · ${usable.filter(n => n.online).length} online`;
}

function closeMenu() { root.classList.remove('menu-open'); }

function draw() {
  if (!root) return;
  const conn = connection();
  const topbar = root.querySelector('.topbar'), pane = root.querySelector('.pane');
  morph(topbar, [
    h('a.brand', { href: href('models') }, mark(28), h('div', h('b', 'LLM Scheduler'))),
    h('span.conn-mini', { title: conn.note }, h('span', { class: 'dot ' + conn.dot }), conn.title),
    h('button.btn.icon.menu', { type: 'button', 'aria-label': 'Menu', 'aria-expanded': root.classList.contains('menu-open') ? 'true' : 'false',
      onclick: () => root.classList.toggle('menu-open') }, icon('M3 5h14M3 10h14M3 15h14')),
  ]);
  morph(pane, [
    h('a.brand', { href: href('models') }, mark(34), h('div', h('b', 'LLM Scheduler'), h('span', brandSub()))),
    navNode(),
    h('div.pane-foot',
      h('div.slotbox', { key: 'slots', 'data-keep': '' }),
      h('div.conn', { role: 'status', 'aria-live': 'polite' },
        h('b', h('span', { class: 'dot ' + conn.dot }), conn.title),
        h('span', { class: conn.warn ? 'warn-t' : null }, conn.note)),
      h('div.actor', h('span', 'Changes are signed as ', h('b.txt2', getActor())),
        h('button', { type: 'button', onclick: changeActor }, 'Change'))),
  ]);
  placeSlots();
}

async function changeActor() {
  const v = await promptText({
    title: 'Your name for the audit trail',
    intro: 'Every change made from this page is recorded with this name, so it is clear later who changed what. It is kept in this browser only.',
    label: 'Name', value: getActor() === 'web' ? '' : getActor(), placeholder: 'for example jacob', maxlength: 64,
  });
  if (v == null) return;
  setActor(v);
  draw();
}

export function startShell(app) {
  root = app;
  strip = h('div.slotbox.topslots', { 'data-keep': '' });
  root.querySelector('.topbar').after(strip);
  narrow.addEventListener('change', placeSlots);
  activeId = parse(location.hash).id;
  onRoute(route => { activeId = route.id; closeMenu(); draw(); });
  on(['status', 'conn', 'hello', 'tick'], draw);
  draw();
}
