// Model actions: the one place the web UI unloads a model. The owner's rule
// of 24 Nov (DECISIONS "RULE CHANGE"): the Zones mockups draw Unload (and
// Load a model), so the UI may do it, each time behind a confirm dialog with
// a required comment; the audit entry keeps the comment and is signed by the
// default user (no X-Actor). No other UI file names these routes
// (TestUIModelActionsInOneModule).
//
//   POST /api/unload {model, force, comment}: without force a model that is
//   serving runs is refused (409 "busy"); with force its runs fail at once.

export function unloadModel(api, model, force, comment) {
  const c = String(comment || '').trim();
  if (!c) return Promise.reject(new Error('A comment is required.'));
  return api.post('/api/unload', { model, force: !!force, comment: c }, { comment: c });
}

// ---------- the page switch: setting ui_model_actions ----------
//
// GET /api/config/settings -> settings.ui_model_actions (boolean, default
// true). When it is false the page offers no model action at all: Load a
// model, Unload and Force unload (buttons, menu items and the unload dialog)
// are left out, not disabled. Read defensively: missing, unreadable or not a
// boolean means true. Read again when the configuration changes.
import * as rest from './api.js';
import { state, on } from './store.js';

let allowed = true, rev = -1, reading = false;
const watchers = new Set();
function readSwitch() {
  if (reading) return;
  reading = true;
  rest.get('/api/config/settings').then(c => {
    const s = (c && c.settings) || c || {};
    allowed = s.ui_model_actions !== false;
  }, () => { /* not readable: keep what is known (true at first) */ }).finally(() => {
    reading = false;
    for (const fn of Array.from(watchers)) fn();
  });
}
// modelActionsOn() -> false only when the setting says false.
export const modelActionsOn = () => allowed;
// onModelActions(fn) -> unsubscribe: fn() once the setting is read, and when it changes.
export function onModelActions(fn) {
  watchers.add(fn);
  if (rev !== state.configRevision) { rev = state.configRevision; readSwitch(); }
  return () => watchers.delete(fn);
}
on(['configRevision'], () => { if (watchers.size && rev !== state.configRevision) { rev = state.configRevision; readSwitch(); } });
