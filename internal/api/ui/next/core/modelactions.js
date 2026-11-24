// Model actions: the one place the web UI loads, drains or unloads a model.
// The owner's decision of 24 Nov (DECISIONS 05:29): the new UI loads and
// unloads models as the Zones mockups draw it (Load a model on chosen zones,
// Unload by waiting for the requests or by force, Force unload), each time
// behind a confirm dialog with a required comment; the audit entry keeps the
// comment (X-Comment) and is signed by the default user (no X-Actor). There
// is no setting that switches them off. No other UI file names these routes
// (TestUIModelActionsInOneModule).
//
//   POST /api/plan {model, nodes}     the plan for a load on those zones (changes nothing)
//   POST /api/load {model, nodes}     load on those zones (202), 409 / 400 with the server's sentence
//   POST /api/models/drain/<model>    admit nothing new, unload once the requests in flight end
//   POST /api/models/undrain/<model>  take the drain back: it admits again
//   POST /api/unload {model, force, comment}: without force a model that is
//   serving requests is refused (409 "busy"); with force they fail at once.

const need = comment => {
  const c = String(comment || '').trim();
  return c ? c : null;
};
const noComment = () => Promise.reject(new Error('A comment is required.'));
const path = (verb, model) => '/api/models/' + verb + '/' + String(model).split('/').map(encodeURIComponent).join('/');

export function unloadModel(api, model, force, comment) {
  const c = need(comment);
  if (!c) return noComment();
  return api.post('/api/unload', { model, force: !!force, comment: c }, { comment: c });
}

// planLoad(api, model, nodes) -> the plan (action start | evict | queue |
// reject, reason, need_mib, required_mib, placement[].budget_mib,
// tight_fit, warning, memory). Read only: no comment.
export function planLoad(api, model, nodes) {
  return api.post('/api/plan', { model, nodes: nodes.slice() });
}

// loadModel(api, model, nodes, comment) -> {msg: "loading", model, nodes}.
// The same body as the plan; the comment goes in X-Comment.
export function loadModel(api, model, nodes, comment) {
  const c = need(comment);
  if (!c) return noComment();
  return api.post('/api/load', { model, nodes: nodes.slice() }, { comment: c });
}

// drainModel(api, model, comment) -> {msg: "draining", model, eta_s?, next_load?}
// or {msg: "not loaded"}.
export function drainModel(api, model, comment) {
  const c = need(comment);
  if (!c) return noComment();
  return api.post(path('drain', model), undefined, { comment: c });
}

// undrainModel(api, model, comment) -> {msg: "ok"}; 409 not_draining.
export function undrainModel(api, model, comment) {
  const c = need(comment);
  if (!c) return noComment();
  return api.post(path('undrain', model), undefined, { comment: c });
}
