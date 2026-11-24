// The configuration pages' fixture (phase 5: Settings, Placement preview,
// Profiles), served by ui-fixture-server.mjs. Generic names and documentation
// addresses only (192.0.2.x, 02:00:00:… MACs).
//
// schema.json is GET /api/config/schema as the scheduler answers it (every
// field type: duration, int, float, bool, enum, string, list; a long help).
//
// --config normal:  settings changed from their defaults, membership agents;
// --config unknown: the same, and one field of the stored configuration this
//                   version does not know (GET /api/system stored_config), so
//                   the server refuses writes unless forced;
// --config fresh:   nothing configured yet (revision 0, every default).

import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
export const schema = JSON.parse(readFileSync(path.join(here, 'schema.json'), 'utf8'));

const field = (scope, key) => schema.fields.find(f => f.scope === scope && f.key === key);
const defaults = scope => Object.fromEntries(schema.fields.filter(f => f.scope === scope).map(f => [f.key, f.default]));

// the values set on purpose in the "normal" state
const SET = {
  ttl: '10m0s', max_hold: '30m0s', queue_timeout: '30m0s', max_pool: 6, cache_ram_mib: 4096,
  forced_fit_check: 'warn', estimate_margin: 0.08, placement_uses_reserved: true, margin_measured_mib: 256,
  membership: 'agents', host_overhead_mib: 1536, worker_silence: '45s',
  logs_push_url: 'http://192.0.2.10:9400/push', bench_retention_days: 183,
};
const RECOVERY = { enabled: true, actions: ['restart_rpc_server', 'restart_docker'] };

// configDoc(state) -> {revision, settings, settings_set, recovery, fleet}
export function configDoc(state = 'normal') {
  if (state === 'fresh') return { revision: 0, settings: defaults('settings'), settings_set: [], recovery: defaults('recovery'), fleet: defaults('fleet') };
  for (const k of Object.keys(SET)) if (!field('settings', k)) throw new Error('fixture: no setting ' + k);
  return {
    revision: 412,
    settings: Object.assign(defaults('settings'), SET),
    settings_set: Object.keys(SET).sort(),
    recovery: Object.assign(defaults('recovery'), RECOVERY),
    fleet: defaults('fleet'),
  };
}

// storedConfig(state) -> GET /api/system stored_config
export const storedConfig = (state = 'normal') => (state === 'unknown'
  ? { unknown_fields: ['settings.gpu_affinity_hint'], writes_need_force: true }
  : { unknown_fields: [], writes_need_force: false });

// dryRun(scope, body, doc) -> the answer of PATCH /api/config/<scope>?dry_run=1
// (internal/cfgsvc Result), or {status: 400, body} when a value is out of the
// schema's range, in the server's words ("settings: max_pool: must be at most 16").
export function dryRun(scope, body, doc) {
  const messages = [], diff = [], restart = [];
  const cur = scope === 'settings' ? doc.settings : doc[scope] || {};
  for (const [k, v] of Object.entries(body || {})) {
    if (k === 'comment') continue;
    const f = field(scope, k);
    if (!f) { messages.push(`${scope}: unknown key ${k}`); continue; }
    if (typeof v === 'number' && f.min != null && v < f.min) messages.push(`${scope}: ${k}: must be at least ${f.min}`);
    if (typeof v === 'number' && f.max != null && v > f.max) messages.push(`${scope}: ${k}: must be at most ${f.max}`);
    diff.push({ path: scope + '.' + k, old: cur[k] ?? null, new: v === null ? f.default : v });
    if (f.restart_required) restart.push(scope + '.' + k);
  }
  if (messages.length) return { status: 400, body: { error: { type: 'invalid_config', message: messages.join('; '), messages } } };
  return { status: 200, body: { revision: doc.revision, changed: diff.length > 0, dry_run: true, audit_entry_ids: [], diff, warnings: [], restart_required: restart } };
}
