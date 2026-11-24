// Hash router and view registry. A route is "#/<view>/<rest...>?<query>".
// A view module registers itself:
//
//   registerView({ id: 'models', title: 'Models', mount(el, route) { ... } });
//
// mount draws into el and returns {unmount()} and optionally {update(route)}
// (called instead of a fresh mount when only the rest or the query changed).
// route = {id, rest: ['runs', 'r-12'], query: {range: '6h'}, hash}.
//
// The new shell's copy of core/router.js (same exports). What it adds: the
// design's addresses (SH-5, Q71), inside the hash under /ui/, so that an open
// panel, a selected row, a period and a page number survive a reload and can
// be shared, and so that Back and Forward close and open a panel.
//
// - A panel named in the path: #/zones/<zone name> is the Zones page with
//   that zone's panel open (the view reads ?zone=<id>; the name is the
//   machine's host name, the id its MAC on an agent cluster). The old form
//   #/zones?zone=<id> still opens it; every link this page writes uses the
//   path. A name the status does not know yet (a reload, before the first
//   status) is looked up again when the status arrives.
// - #/models?model=<key> stays: #/models/<key> is the classic full model
//   page, which the design's address for the panel would take over. Which
//   one keeps that address is not decided (09 SH-5, section 7 item 16).
// - Opening or closing a panel, or selecting a row, adds a history entry
//   even when the view asks to replace the address (PUSHED); the other
//   query parts (filters, sort, period, page) replace it, as before.
// - Addresses the server writes (GET /api/health's links): #/monitor/<page>
//   is <page> (the Monitor section's pages have no prefix here), and the
//   Zones page's ?filter= is its ?show=.
// - The period (?period=, ?date=), a page number (?page=) and the other
//   filters are plain query parts, kept as the views write them.

import { h } from './dom.js';
import { empty } from './ui.js';
import { state, on } from './store.js';

const views = new Map();
let outlet = null;
let current = null; // {id, handle}
const onChange = new Set();

export function registerView(v) { views.set(v.id, v); }
export const getView = id => views.get(id);

// ---------- the design's addresses ----------

const nodesOf = () => (state.status && state.status.view && state.status.view.nodes) || [];
// a zone's name in an address: its host name when no other zone has it
function zoneName(id) {
  const n = nodesOf().find(x => x.id === id);
  if (!n || !n.hostname) return id;
  return nodesOf().filter(x => x.hostname === n.hostname).length === 1 ? n.hostname : id;
}
// the zone an address names: by id, or by host name
function zoneId(name) {
  const ns = nodesOf();
  if (ns.some(x => x.id === name)) return name;
  const byName = ns.filter(x => x.hostname === name);
  return byName.length === 1 ? byName[0].id : name;
}
const zoneKnown = name => nodesOf().some(x => x.id === name || x.hostname === name);

// SHAPES[view]: the query part a one-part path stands for: #/<view>/<name>
// is #/<view>?<param>=<id>.
export const SHAPES = {
  zones: { param: 'zone', toPath: zoneName, fromPath: zoneId, known: zoneKnown },
};
// PUSHED[view]: query parts whose change adds a history entry (a panel, a
// selected row), even through navigate(..., replace).
export const PUSHED = {
  zones: ['zone'],
  models: ['model', 'item'],
};
// ALIASES: the Zones page's filter written by the server
const QUERY_ALIASES = { zones: { filter: 'show' } };

export function parse(hash) {
  const raw = (hash || '').replace(/^#\/?/, '');
  const [path, qs] = raw.split('?');
  let parts = path.split('/').filter(Boolean).map(safeDecode);
  const query = {};
  if (qs) for (const [k, v] of new URLSearchParams(qs)) query[k] = v;
  // #/monitor/<page>: the page (the server's links name the section)
  if (parts[0] === 'monitor' && parts[1]) parts = parts.slice(1);
  const id = parts[0] || 'models';
  let rest = parts.slice(1);
  for (const [from, to] of Object.entries(QUERY_ALIASES[id] || {})) {
    if (query[from] != null && query[to] == null) query[to] = query[from];
    delete query[from];
  }
  const shape = SHAPES[id];
  if (shape && rest.length === 1 && query[shape.param] == null) {
    query[shape.param] = shape.fromPath(rest[0]);
    rest = [];
  }
  return { id, rest, query, hash: '#/' + raw };
}

function safeDecode(s) { try { return decodeURIComponent(s); } catch { return s; } }

// href('benchmarks', ['runs', id], {tab: 'steps'}) -> "#/benchmarks/runs/<id>?tab=steps"
// href('zones', [], {zone: id}) -> "#/zones/<zone name>"
export function href(id, rest, query) {
  rest = rest || [];
  const q0 = Object.assign({}, query || {});
  const shape = SHAPES[id];
  if (shape && !rest.length && q0[shape.param]) {
    rest = [shape.toPath(q0[shape.param])];
    delete q0[shape.param];
  }
  const s = '#/' + [id].concat(rest).map(encodeURIComponent).join('/');
  const q = new URLSearchParams();
  for (const k in q0) if (q0[k] != null && q0[k] !== '') q.set(k, q0[k]);
  const qs = q.toString();
  return qs ? s + '?' + qs : s;
}

// navigate(...) changes the route; with replace it does not add a history
// entry (use that for filters and tabs), unless it opens or closes a panel
// or changes the selected row (PUSHED): Back then closes it again.
export function navigate(id, rest, query, replace) {
  const to = href(id, rest, query);
  if (replace) {
    const now = parse(location.hash);
    const next = parse(to);
    const pushes = now.id === next.id && (PUSHED[id] || []).some(k => (now.query[k] || '') !== (next.query[k] || ''));
    if (!pushes) { history.replaceState(null, '', to); render(); return; }
  }
  location.hash = to;
}

export function onRoute(fn) { onChange.add(fn); return () => onChange.delete(fn); }

let unresolved = null; // a panel name the status did not know when the route was drawn
function render() {
  const route = parse(location.hash);
  const shape = SHAPES[route.id];
  unresolved = shape && route.query[shape.param] != null && !shape.known(route.query[shape.param]) ? route.hash : null;
  const view = views.get(route.id);
  for (const fn of onChange) fn(route, view);
  document.title = (view ? view.title + ' · ' : '') + 'Potluck';
  if (current && current.id === route.id && current.handle && current.handle.update) {
    current.handle.update(route);
    return;
  }
  if (current && current.handle && current.handle.unmount) {
    try { current.handle.unmount(); } catch (e) { console.error(e); }
  }
  outlet.replaceChildren();
  window.scrollTo(0, 0);
  if (!view) {
    outlet.appendChild(h('div.card', empty('There is no such screen', 'The address does not match a screen of this page. Pick one from the navigation.')));
    current = null;
    return;
  }
  let handle = null;
  try { handle = view.mount(outlet, route) || null; } catch (e) {
    console.error(e);
    outlet.replaceChildren(h('div.banner.err', 'This screen failed to start: ' + (e && e.message ? e.message : e)));
  }
  current = { id: route.id, handle };
}

export function start(el) {
  outlet = el;
  window.addEventListener('hashchange', render);
  if (!location.hash || location.hash === '#') history.replaceState(null, '', '#/models');
  // a name in the address the first status makes known: draw the route again
  on('status', () => {
    if (!unresolved || unresolved !== parse(location.hash).hash) return;
    const r = parse(location.hash), shape = SHAPES[r.id];
    if (shape && shape.known(r.query[shape.param])) render();
  });
  render();
}
