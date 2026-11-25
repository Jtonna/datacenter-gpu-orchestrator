// The model catalog on the client. It arrives over the live channel (topic
// "models": the whole document of GET /api/models when the connection opens,
// then only the entries that changed) and is kept here for the life of the
// page, so every screen that names a model has it at once and going back to
// Models draws it without a request. REST (GET /api/models) is only the
// fallback: while the channel is not connected yet, on a server without the
// topic, or when a change message does not fit the copy that is held.
//
//   catalog.models   the entries in display order, or null before the first copy
//   catalog.byKey    key -> entry;  catalog.byProfile  profile id -> entry
//   catalog.error    the message of a failed REST read, '' otherwise
//   onCatalog(fn)    fn() after every change; returns the unsubscribe function
//   needCatalog()    makes sure a copy is on its way and keeps it current over
//                    the live channel while this screen is open (call it when
//                    a screen opens); needCatalog({live: false}): a copy is
//                    enough (model names and avatars), no live updates
//   releaseCatalog() the open screen changed: no live updates until the next
//                    screen asks (the router hook in app.js calls it)
//   modelFor({profile, model_file}) -> the entry an instance or a benchmark belongs to

import { on, listen, set, state } from './store.js';
import * as api from './api.js';

export const catalog = { models: null, byKey: {}, byProfile: {}, version: 0, dir: '', history: null, error: '', at: 0 };
const subs = new Set();
export const onCatalog = fn => { subs.add(fn); return () => subs.delete(fn); };

function index() {
  catalog.byKey = {};
  catalog.byProfile = {};
  for (const m of catalog.models || []) {
    catalog.byKey[m.key] = m;
    for (const p of m.profiles || []) catalog.byProfile[p.id] = m;
  }
  catalog.at = Date.now();
  for (const fn of Array.from(subs)) { try { fn(); } catch (e) { console.error(e); } }
}

function setAll(doc) {
  if (!doc || !Array.isArray(doc.models)) return;
  if (catalog.models && doc.version != null && doc.version < catalog.version) return; // an older copy that arrived late
  catalog.models = doc.models;
  catalog.version = doc.version || 0;
  catalog.dir = doc.models_dir || '';
  catalog.history = doc.history;
  catalog.error = '';
  index();
}

let pulling = null;
function pull() {
  if (pulling) return pulling;
  pulling = api.get('/api/models').then(setAll, e => { catalog.error = e.message; index(); }).finally(() => { pulling = null; });
  return pulling;
}

listen('models', d => {
  if (!d) return;
  if (Array.isArray(d.models)) { setAll(d); return; }
  // a change applies to the version it names; if it does not fit, read the whole copy again
  if (!catalog.models || d.base !== catalog.version) { pull(); return; }
  const by = Object.assign({}, catalog.byKey);
  for (const m of d.changed || []) by[m.key] = m;
  for (const k of d.removed || []) delete by[k];
  catalog.models = (d.order || Object.keys(by)).map(k => by[k]).filter(Boolean);
  catalog.version = d.version;
  index();
});

// needCatalog(): a screen that draws models calls it when it opens. It
// subscribes the live channel to "models" (state.catalogLive, which
// core/live.js sends), so the catalog's changes arrive while the screen is
// open; the server computes every profile's plan for that topic, so a screen
// that needs only names passes {live: false} and a screen that needs nothing
// does not call it. It asks over REST as well when there is no copy yet, so
// the first paint never depends on which one is faster.
let asked = false;
export function needCatalog(opts) {
  if (!(opts && opts.live === false) && !state.catalogLive) set('catalogLive', true);
  if (catalog.models || asked) return;
  asked = true;
  pull();
}
// a server without the models topic (an older build): REST after every reconnect
on('hello', hello => { if (hello && !(hello.topics || []).includes('models')) pull(); });
// a configuration change always ends in a new catalog over the channel; when
// the channel is down the next open asks again
on('conn', c => { if (c !== 'live') asked = false; });

export function releaseCatalog() { if (state.catalogLive) set('catalogLive', false); }

// refreshCatalog(): read the catalog again now (after a write the page made, such as a pin)
export const refreshCatalog = () => pull();

export const modelFor = x => (x && (catalog.byProfile[x.profile] || catalog.byKey[x.model_file])) || null;
