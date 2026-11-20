// Hash router and view registry. A route is "#/<view>/<rest...>?<query>".
// A view module registers itself:
//
//   registerView({ id: 'models', title: 'Models', mount(el, route) { ... } });
//
// mount draws into el and returns {unmount()} and optionally {update(route)}
// (called instead of a fresh mount when only the rest or the query changed).
// route = {id, rest: ['runs', 'r-12'], query: {range: '6h'}, hash}.

import { h } from './dom.js';
import { empty } from './ui.js';

const views = new Map();
let outlet = null;
let current = null; // {id, handle}
const onChange = new Set();

export function registerView(v) { views.set(v.id, v); }
export const getView = id => views.get(id);

export function parse(hash) {
  const raw = (hash || '').replace(/^#\/?/, '');
  const [path, qs] = raw.split('?');
  const parts = path.split('/').filter(Boolean).map(decodeURIComponent);
  const query = {};
  if (qs) for (const [k, v] of new URLSearchParams(qs)) query[k] = v;
  return { id: parts[0] || 'models', rest: parts.slice(1), query, hash: '#/' + raw };
}

// href('benchmarks', ['runs', id], {tab: 'steps'}) -> "#/benchmarks/runs/<id>?tab=steps"
export function href(id, rest, query) {
  let s = '#/' + [id].concat(rest || []).map(encodeURIComponent).join('/');
  const q = new URLSearchParams();
  for (const k in query || {}) if (query[k] != null && query[k] !== '') q.set(k, query[k]);
  const qs = q.toString();
  return qs ? s + '?' + qs : s;
}

// navigate(...) changes the route; with replace it does not add a history entry
// (use that for filters and tabs).
export function navigate(id, rest, query, replace) {
  const to = href(id, rest, query);
  if (replace) { history.replaceState(null, '', to); render(); } else location.hash = to;
}

export function onRoute(fn) { onChange.add(fn); return () => onChange.delete(fn); }

function render() {
  const route = parse(location.hash);
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
  render();
}
