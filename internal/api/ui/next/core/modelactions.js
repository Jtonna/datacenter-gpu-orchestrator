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
