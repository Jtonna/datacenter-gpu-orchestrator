// Agent machines (defaults.membership: agents): why a machine takes no new
// model, in plain words, and the two operator actions on a machine. No DOM
// and no import, so the mapping and the requests can be run on their own.
//
// The data: GET /api/status has `membership: "agents"` and `agents[]` (the
// facts of every agent node, internal/pool NodeFacts); each view.nodes entry
// has `unplaced` (a reason word) and `agent` (its facts). The actions are
// POST /api/nodes/{id}/pause, /unpause and /force-unload (internal/api/nodes.go);
// each needs a comment and writes an audit entry.

// agentsMode(st) is true when the machines join through agents. In fleet-feed
// mode there are no agent facts and no machine controls.
export const agentsMode = st => !!st && st.membership === 'agents' && Array.isArray(st.agents);

// agentOf(st, node) -> the agent facts of a view node, or null.
export function agentOf(st, n) {
  if (!agentsMode(st) || !n) return null;
  return n.agent || st.agents.find(a => a.id === n.id) || null;
}

// The reason words of the scheduler (internal/scheduler/agents.go, Unplaced*)
// and of the pool. `short` is a few words, `text` one or two sentences.
export const UNPLACED = {
  no_tier_match: { short: 'No tier matches',
    text: 'No tier in the configuration matches this machine, so it is outside the pool and is not counted in the capacity. Add a tier that matches its platform or device.' },
  programs_not_ready: { short: 'Programs not ready',
    text: 'Its agent has not finished fetching and checking its programs, so no worker can start here yet.' },
  model_source_unreachable: { short: 'Cannot reach the models',
    text: 'It cannot reach the place the model files come from, so a worker here could not read a model.' },
  orphan_launch: { short: 'Runs an unknown worker',
    text: 'Its agent reports a running worker that Potluck does not hold. It takes no new model until that worker is gone.' },
  silent: { short: 'Agent is silent',
    text: 'Its agent has not reported for more than 10 s. What it holds is kept for now; it takes no new model until the agent reports again.' },
  agent_unreachable: { short: 'Agent not reachable',
    text: 'Potluck cannot reach its agent, so it cannot start a worker here.' },
  host_machine: { short: 'The machine Potluck runs on',
    text: 'This agent runs on the machine Potluck itself runs on. That machine is used directly, so nothing is placed through this agent.' },
};

// unplacedReason(code) -> {short, text, known}. A code this file does not
// know is shown as it is, never guessed at.
export function unplacedReason(code) {
  const c = String(code || '').trim();
  const r = UNPLACED[c];
  if (r) return { short: r.short, text: r.text, known: true, code: c };
  return { short: c, text: c, known: false, code: c };
}

// The states of an agent node that are not "active" (pool: lost, left, removed).
const STATE = {
  lost: { short: 'Agent lost', text: 'Its agent stopped reporting, so it is treated as gone until it registers again.' },
  left: { short: 'Left the pool', text: 'Its agent signed off. It comes back when the agent starts again.' },
  removed: { short: 'Removed', text: 'It was removed from the pool by an operator and stays out until it is re-admitted.' },
};

// notPlaceable(node, agent) -> null when the machine may take a new model,
// else {short, text, known, code, paused}. `agent` is the node's agent facts;
// the reason word may be on either. A paused machine has paused: true and no
// other reason unless the server gave one.
export function notPlaceable(n, a) {
  if (!a) return null;
  const code = a.unplaced || (n && n.unplaced) || '';
  if (code) return Object.assign(unplacedReason(code), { paused: !!a.paused });
  if (a.placeable !== false) return null;
  if (a.state && STATE[a.state]) return Object.assign({ known: true, code: a.state, paused: !!a.paused }, STATE[a.state]);
  if (a.paused) return { short: 'Paused', text: 'Paused by an operator: it keeps what it holds and takes no new model until it is unpaused.', known: true, code: 'paused', paused: true };
  if (a.host_agent) return Object.assign(unplacedReason('host_machine'), { paused: false });
  return { short: 'Not placeable', text: 'The scheduler reports that it takes no new model and gives no reason.', known: true, code: '', paused: false };
}

// holdsSomething(node, agent): the machine has a launch or an instance, so a
// force unload has something to stop.
export const holdsSomething = (n, a) => !!((a && a.launch_id) || (n && n.holder));

// The two actions. `verb` is the last path segment of the endpoint.
export const NODE_ACTIONS = {
  pause: { verb: 'pause', label: 'Pause', busy: 'Pausing', done: 'Paused' },
  unpause: { verb: 'unpause', label: 'Unpause', busy: 'Unpausing', done: 'Unpaused' },
  force: { verb: 'force-unload', label: 'Force unload', busy: 'Unloading', done: 'Force unload sent' },
};

// pauseAction(agent) -> 'unpause' for a paused machine, else 'pause'.
export const pauseAction = a => (a && a.paused ? 'unpause' : 'pause');

// nodeActionPath('02:00:00:00:01:01', 'pause') -> '/api/nodes/02%3A00.../pause'
export function nodeActionPath(id, action) {
  const a = NODE_ACTIONS[action];
  if (!a) throw new Error('unknown node action ' + action);
  return '/api/nodes/' + encodeURIComponent(id) + '/' + a.verb;
}

// sendNodeAction(api, id, action, comment) posts the action through the REST
// helper (core/api.js: X-Source, X-Actor). The comment is required; it goes in
// the body (kept exactly) and in X-Comment, as the configuration writes do.
export function sendNodeAction(api, id, action, comment) {
  const c = String(comment || '').trim();
  if (!c) return Promise.reject(new Error('A comment is required.'));
  return api.post(nodeActionPath(id, action), { comment: c }, { comment: c });
}
