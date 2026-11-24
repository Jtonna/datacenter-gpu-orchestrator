// The access keys of the MCP pages (phase 6 UI) in the server's shapes
// (internal/api/agents.go): GET /api/agents, POST /api/agents, PATCH
// /api/agents/{id}, POST /api/agents/{id}/rotate, DELETE /api/agents/{id}.
// Kept in memory for one fixture server, so the pages can be driven end to
// end (create, then manage, rotate, revoke) in a browser.
//
// States (--keys <state>):
//   none     no key yet
//   normal   three active keys, one expiring in 3 days, one expired, one
//            revoked, one legacy (made before permissions: lls_, all allowed,
//            never expires)
// The "just created" and "just rotated" states are the answers of POST
// /api/agents and POST .../rotate (createAnswer, rotateAnswer): the full key
// is in those answers only.
//
// The keys are fixture text, not keys of any system.

const DAY = 24 * 3600 * 1000;
const PERMS = ['observe', 'zones', 'models', 'settings'];
const LABEL = { observe: 'Observe', zones: 'Switch zones on or off', models: 'Load and unload models', settings: 'Change settings' };
const SHORT = { observe: 'observe', zones: 'switch zones', models: 'load and unload', settings: 'change settings' };
const ROUTES = {
  observe: ['GET /api/*', 'POST /api/plan', '/ws', '/api/events', '/api/mcp/tools'],
  zones: ['POST /api/nodes/{id}/pause', 'POST /api/nodes/{id}/unpause', 'PUT /api/config/exclude-nodes', 'PUT /api/config/no-auto-nodes'],
  models: ['POST /api/load', 'POST /api/unload', 'POST /api/nodes/{id}/force-unload', 'POST /api/cache/evict'],
  settings: ['PATCH /api/config/settings', 'POST /api/config/profiles', 'PUT /api/config/profiles/{id}', 'DELETE /api/models/{key...}'],
};
export const CREATED_KEY = 'ptl_fixture0example0key0not0real0001';
export const ROTATED_KEY = 'ptl_fixture0example0key0rotated00002';
const PASTE = '<paste your key>';

const can = g => {
  const parts = PERMS.filter(p => g[p] === 'allow').map(p => SHORT[p]);
  if (!parts.length) return 'Nothing';
  parts[0] = parts[0][0].toUpperCase() + parts[0].slice(1);
  return parts.join(' · ');
};
const settings = (host, key) => ({ mcpServers: { potluck: { type: 'http', url: 'http://' + host + '/mcp', headers: { Authorization: 'Bearer ' + key } } } });
const err = (status, message) => ({ status, body: { error: { message, type: status === 409 ? 'conflict' : 'invalid_request_error' } } });

// usage(now, perDay) -> the last 7 UTC days, today last
function usage(now, perDay) {
  const today = now - (now % DAY);
  const by = [];
  let r = 0, d = 0, a = 0;
  for (let i = 0; i < 7; i++) {
    const x = perDay ? perDay(i) : [0, 0, 0];
    r += x[0]; d += x[1]; a += x[2];
    by.push({ day: new Date(today - (6 - i) * DAY).toISOString().slice(0, 10), requests: x[0], denied: x[1], asked: x[2] });
  }
  return { days: 7, requests: r, denied: d, asked: a, by_day: by };
}

function seed(state, now) {
  if (state === 'none') return [];
  const g = (o, z, m, s) => ({ observe: o, zones: z, models: m, settings: s });
  const k = (id, name, o) => Object.assign({ id, name, description: '', created_at: now - 40 * DAY, created_by: { source: 'ui', actor: 'anonymous' }, rotated_at: null,
    last_used_at: null, revoked_at: null, request_count: 0, audit_entries: 1, expires_at: null, perms: g('allow', 'off', 'off', 'off'), legacy: false, use: null }, o);
  return [
    k('ag_000000000001', 'ops-bot', { hint: 'ptl_••••7c2e', expires_at: now + 23 * DAY + 3600e3, perms: g('allow', 'allow', 'ask', 'off'), last_used_at: now - 2 * 3600e3,
      request_count: 1284, audit_entries: 9, description: 'Pauses zones at night and wakes them in the morning.', use: i => [120 + i * 9, i === 5 ? 3 : 0, i === 6 ? 2 : 0] }),
    k('ag_000000000002', 'Desktop assistant', { hint: 'ptl_••••a91f', last_used_at: now - 15 * 60e3, request_count: 342, use: i => [30 + i, 0, 0] }),
    k('ag_000000000003', 'ci-runner', { hint: 'ptl_••••4b10', expires_at: now + 80 * DAY, perms: g('allow', 'off', 'allow', 'off'), last_used_at: now - 26 * 3600e3, request_count: 57, use: i => [i % 2 ? 6 : 0, 0, 0] }),
    k('ag_000000000004', 'Home dashboard', { hint: 'ptl_••••03bd', expires_at: now + 3 * DAY - 3600e3, last_used_at: now - 19 * 3600e3, request_count: 9120, use: i => [1200 + i * 3, 0, 0] }),
    k('ag_000000000005', 'test-script', { hint: 'ptl_••••e2a4', expires_at: now - 16 * DAY, last_used_at: now - 20 * DAY, request_count: 12 }),
    k('ag_000000000006', 'old-laptop', { hint: 'ptl_••••55d0', expires_at: now + 10 * DAY, perms: g('allow', 'allow', 'off', 'off'), last_used_at: now - 15 * DAY, revoked_at: now - 8 * DAY, request_count: 410, audit_entries: 4 }),
    k('ag_000000000007', 'report writer', { hint: 'lls_3f9a01••••', legacy: true, perms: g('allow', 'allow', 'allow', 'allow'), created_at: now - 200 * DAY, last_used_at: now - 4 * DAY, request_count: 77, use: i => [i === 2 ? 4 : 0, 0, 0] }),
  ];
}

export class Keys {
  constructor(state, now = Date.now()) { this.now = () => now; this.list = seed(state, now); this.n = 100; }

  view(k, host) {
    const now = this.now();
    let status = 'active', days = 0;
    if (k.revoked_at) status = 'revoked';
    else if (k.expires_at) {
      const left = k.expires_at - now;
      days = Math.ceil(left / DAY);
      status = left <= 0 ? 'expired' : left <= 7 * DAY ? 'expiring' : 'active';
    }
    const v = { id: k.id, name: k.name, description: k.description, key_prefix: k.hint.slice(0, 4), key_hint: k.hint, created_at: k.created_at, created_by: k.created_by,
      rotated_at: k.rotated_at, last_used_at: k.last_used_at, revoked_at: k.revoked_at, revoked: !!k.revoked_at, request_count: k.request_count,
      actor: 'agent:' + k.name, audit_entries: k.audit_entries, expires_at: k.expires_at, status, permissions: Object.assign({}, k.perms), can: can(k.perms),
      asks: PERMS.filter(p => k.perms[p] === 'ask'), legacy: k.legacy, mcp_settings: settings(host, PASTE), usage: usage(now, k.use) };
    if (k.expires_at && days > 0) v.expires_in_days = days;
    return v;
  }

  listAnswer(host) {
    const agents = this.list.map(k => this.view(k, host));
    return { status: 200, body: { agents, active: agents.filter(a => a.status === 'active' || a.status === 'expiring').length, key_prefix: 'ptl_',
      headers: ['Authorization: Bearer <key>', 'X-Agent-Key: <key>'], mcp_url: 'http://' + host + '/mcp', mcp_settings: settings(host, PASTE),
      permissions: PERMS.map(p => ({ id: p, label: LABEL[p], levels: p === 'observe' ? ['allow', 'off'] : ['allow', 'ask', 'off'], routes: ROUTES[p] })),
      note: 'A key limits and names an agent: a request with a key gets exactly its permissions and is recorded under its name; a request without a key is the default user with full access.' } };
  }

  // the checks of agentName, expiryArg and Grants.Merge, with their sentences
  name(v, except) {
    const n = String(v == null ? '' : v).trim();
    if (!n) return [null, err(400, 'the agent needs a name (it is what the audit log shows)')];
    if (n.length > 64) return [null, err(400, 'the name may be at most 64 characters')];
    const other = this.list.find(k => k.id !== except && !k.revoked_at && k.name.toLowerCase() === n.toLowerCase());
    if (other) return [null, err(409, 'an agent named ' + other.name + ' already exists: two agents with one name could not be told apart in the audit log')];
    return [n, null];
  }
  expiry(obj) {
    if (!('expires_at' in obj) || obj.expires_at == null) return [0, null];
    const f = obj.expires_at;
    if (typeof f !== 'number' || f < 0 || f !== Math.trunc(f)) return [0, err(400, 'expires_at must be a time in unix milliseconds, or 0 (or null) for never')];
    if (f > 0 && f <= this.now()) return [0, err(400, 'expires_at must be in the future (or 0 for never)')];
    return [f, null];
  }
  perms(obj, base) {
    const g = Object.assign({}, base);
    for (const [p, l] of Object.entries(obj.permissions || {})) {
      if (!PERMS.includes(p)) return [null, err(400, `no permission "${p}": the permissions are observe, zones, models and settings`)];
      g[p] = String(l).toLowerCase();
    }
    for (const p of PERMS) {
      const ok = p === 'observe' ? ['allow', 'off'] : ['allow', 'ask', 'off'];
      if (!ok.includes(g[p])) return [null, err(400, p === 'observe' ? `permission observe must be allow or off, not "${g[p]}"` : `permission ${p} must be allow, ask or off, not "${g[p]}"`)];
    }
    return [g, null];
  }

  create(body, host, key = CREATED_KEY) {
    const [name, e1] = this.name(body.name);
    if (e1) return e1;
    let [exp, e2] = this.expiry(body);
    if (e2) return e2;
    if (!('expires_at' in body)) exp = this.now() + 30 * DAY;
    const [g, e3] = this.perms(body, { observe: 'allow', zones: 'off', models: 'off', settings: 'off' });
    if (e3) return e3;
    const k = { id: 'ag_' + String(++this.n).padStart(12, '0'), name, description: String(body.description || '').slice(0, 500), hint: 'ptl_••••' + key.slice(-4),
      created_at: this.now(), created_by: { source: 'ui', actor: 'anonymous' }, rotated_at: null, last_used_at: null, revoked_at: null, request_count: 0, audit_entries: 1,
      expires_at: exp || null, perms: g, legacy: false, use: null };
    this.list.unshift(k);
    return { status: 201, body: { agent: this.view(k, host), key, mcp_settings: settings(host, key), audit_id: 500 + this.n, note: 'This is the only time the key is shown. Only its SHA-256 is stored.' } };
  }

  patch(id, body, host) {
    const k = this.list.find(x => x.id === id);
    if (!k) return err(404, 'no such agent (or it is revoked)');
    if (k.revoked_at) return err(409, 'this key is revoked: a revoked key cannot be changed (create a new key)');
    let name = k.name;
    if ('name' in body) { const [n, e] = this.name(body.name, id); if (e) return e; name = n; }
    let exp = k.expires_at;
    if ('expires_at' in body) { const [x, e] = this.expiry(body); if (e) return e; exp = x || null; }
    const [g, e3] = this.perms(body, k.perms);
    if (e3) return e3;
    const changed = name !== k.name || exp !== k.expires_at || JSON.stringify(g) !== JSON.stringify(k.perms) || ('description' in body && body.description !== k.description);
    if (changed) {
      Object.assign(k, { name, expires_at: exp, perms: g, audit_entries: k.audit_entries + 1 });
      if ('description' in body) k.description = String(body.description).slice(0, 500);
      if (k.legacy && ('expires_at' in body || 'permissions' in body)) k.legacy = false; // its access row is written; the hint stays (no last 4 known)
    }
    return { status: 200, body: { agent: this.view(k, host), changed } };
  }

  rotate(id, host, key = ROTATED_KEY) {
    const k = this.list.find(x => x.id === id);
    if (!k) return err(404, 'no such agent (or it is revoked)');
    if (k.revoked_at) return err(404, 'no such agent (or it is revoked)');
    Object.assign(k, { hint: 'ptl_••••' + key.slice(-4), rotated_at: this.now(), legacy: false, audit_entries: k.audit_entries + 1 });
    return { status: 200, body: { agent: this.view(k, host), key, mcp_settings: settings(host, key), note: 'This is the only time the key is shown. The old key no longer works.' } };
  }

  revoke(id, host) {
    const k = this.list.find(x => x.id === id);
    if (!k || k.revoked_at) return err(404, 'no such agent (or it is revoked)');
    k.revoked_at = this.now();
    k.audit_entries++;
    return { status: 200, body: { agent: this.view(k, host), revoked: true } };
  }
}

// GET /api/mcp/tools in the shape of mcpserver.ToolInfo: a few tools of each
// group, for the MCP page's Tools section.
export function toolsAnswer() {
  const t = (name, group, scope, summary, o = {}) => Object.assign({ name, group, scope, summary, description: summary + ' (fixture text)', read_only: group === 'read', destructive: false, input_schema: { type: 'object' } }, o);
  return {
    server: { name: 'llm-scheduler', version: 'fixture' }, path: '/mcp', transport: 'http', protocol_versions: ['2025-06-18'], instructions: '',
    tools: [
      t('get_status', 'read', 'observe', 'What is loaded, what is waiting and the state of every zone.'),
      t('list_models', 'read', 'observe', 'The model files and the profiles that load them.'),
      t('plan_load', 'read', 'observe', 'Where a profile would load now, without loading it.'),
      t('update_settings', 'configuration', 'settings', 'Change scheduler settings.', { destructive: true }),
      t('save_profile', 'configuration', 'settings', 'Add or replace a profile.', { destructive: true }),
      t('pause_node', 'configuration', 'zones', 'Stop new loads on a zone.'),
      t('start_benchmark', 'benchmarks', 'models', 'Start a benchmark run.'),
      t('load_model', 'runtime', 'models', 'Load a profile ahead of the first request.'),
      t('unload_model', 'runtime', 'models', 'Stop a loaded model.', { destructive: true }),
    ],
    groups: [
      { id: 'read', title: 'Read', note: 'Look at the cluster, the configuration, the history and benchmark results. Nothing changes.' },
      { id: 'configuration', title: 'Configuration', note: 'Change settings, profiles, tiers and nodes. Every change is a revision that can be rolled back.' },
      { id: 'benchmarks', title: 'Benchmarks', note: 'Define benchmarks and suites, start and cancel runs. A run loads and unloads real models.' },
      { id: 'runtime', title: 'Load and unload', note: 'Load a model ahead of time or stop a loaded one. Only agents and the API can do this; the web UI has no such control.' },
    ],
  };
}
