#!/usr/bin/env node
// ui-fixture-server.mjs: serves the web UI with fixed answers, no scheduler
// needed. For looking at a screen in a browser, and for the mockup
// comparison (the ui-compare tool): the answers mirror the sample data of
// the Overview mockup (design-canvas Main.dc.html), with generic names and
// documentation addresses, so a screenshot of the page and one of the
// artboard can be compared like for like. Node only, no dependencies.
//
// usage: node scripts/ui-fixture-server.mjs [--port 3122] [--ui internal/api/ui] [--host 0.0.0.0]
//                                           [--state normal|empty|off|loading] [--sequence] [--live-timings] [--wide-zones]
//                                           [--health two|good|unknown] [--release 0.1.5]
//                                           [--zones none|groups|live|one-group|fleet] [--uptime-partial]
//                                           [--load start|reject|evict|loaded|unknown] [--drain eta|noeta] [--pinned]
//                                           [--pending waiting|joined|expired|cancelled|gone|nosource|taken] [--no-host-uptime]
//
//   /ui/components.html  the shell's shared components (scripts/ui-fixture/components.js)
//   /ui/fx-<name>.html   the app driven by scripts/ui-fixture/fx-<name>.js (a board
//                that needs clicks, e.g. fx-zones-selected with --zones fleet)
//   /ui/...      the UI (ui/next laid over the shared files of ui/, as the
//                scheduler serves it)
//   /api/status, /api/requests, /api/requests/summary, /api/requests/{id},
//   /api/nodes/availability, /api/nodes, /api/models, and for the health popup
//   /api/system, /api/about, /api/agents, /api/config/settings, /api/history:
//   fixed answers (times relative to now)
//   /ws          a WebSocket: hello, the status every 5 s, the requests topic
//   the model actions of the Zones pages (POST /api/load, /api/unload,
//   /api/models/drain/<model>, /api/models/undrain/<model>, and POST
//   /api/plan when --load is given): fixed answers as the server gives them
//   (--load, --drain below); nothing changes
//   /api/config, /api/config/schema|settings|recovery|fleet, /api/profiles,
//   POST /api/plan and settings dry runs (PATCH ...?dry_run=1): the
//   configuration pages' fixture (scripts/ui-fixture/config.mjs, profiles.mjs)
//   any other /api/... answers 404 in the API's error shape; other writes 405.
//
// --config normal|unknown|fresh: the settings state (config.mjs). --catalog:
// /api/models lists the profile fixture's model files. --named-only <zone>:
// that zone is in no_auto_nodes (auto: false), drawn "Named only".
// --tiers live|catchall (with --zones live): the Tiers and zones page's
// states (scripts/ui-fixture/tiers.mjs): GET /api/config carries the tier
// lists, GET /api/config/tiers adds tier_zones, the zones on GET /api/nodes
// and /api/status agents carry tier / tier_reason as the server gives them,
// and GET /api/nodes lists the local node.
//
// --state empty: no request in the period; off: the request log is off
// (usage.db not open); loading: the request routes never answer.
//
// The fixture's machines name their first device after its chip ("Oberon",
// "RTX 3070 Ti"); the Zone column shows that name until agents report a
// chip field.
//
// --live-timings: the setting requests_live_timings on, so running rows carry
// their prompt figures (tokens in, reused, in tok/s) as the mockup draws them.
// Without it, as on a scheduler with the setting off (its default), those
// three are null until the run ends and the table shows a dash.
//
// --health: what GET /api/health and the health topic answer (the Health
// board's states): two (the default: Zones a problem, Runs needing
// attention, as the mockup draws it), good (no issue), unknown (no status
// read yet: Zones and Queue unknown, as is Alerts in every state).
//
// --zones: the Zones pages' states (scripts/ui-fixture/zones.mjs): none (the
// eight machines, no group; the default), groups (the eight in rack-a and
// home, one with no group), fleet (232 zones as Zones-Fleet draws them, a
// paused group, 15 pages). Every state has zone_state and group on the
// zones, the counts on GET /api/nodes, GET /api/groups and GET
// /api/nodes/uptime. --uptime-partial: the uptime window is not complete
// yet (the history began 30 hours ago).
//
// --load: what POST /api/plan and POST /api/load answer for a load on chosen
// zones (internal/api/api.go, pin.go): start (the plan starts it, the load
// is accepted: 202), reject (the plan says reject with the server's
// sentence for a paused zone; the load 409 "chosen zone unusable"), evict
// (the plan unloads a model first), loaded (the plan starts, the load 409
// "already loaded or queued"), unknown (400: a zone the fleet view does not
// hold).
//
// --drain: the first instance (gemma-4-27b:32k, two requests in flight) is
// draining: eta (drain_eta_s 40) or noeta (no estimate yet). POST
// /api/models/drain answers eta_s 40 unless --drain noeta. --pinned: that
// instance was loaded on chosen zones (pinned_nodes).
//
// --no-host-uptime: no agent reports host_uptime_s (an older agent, a console
// fed by the fleet console); otherwise every reachable agent does.
//
// --pending: Create new zone (internal/api/pending.go). GET /api/zones/pending
// lists the one pending zone zone-9 (rack-a) in that state, GET
// /api/zones/pending/pz-0000000000000001 answers it (gone: 404, as after a
// restart of the scheduler), POST answers 201 waiting (nosource: the
// server's 400 sentence; taken: its 409), DELETE answers it cancelled. The
// setting default_model_source reads share:192.0.2.10:/models (nosource:
// empty). The command names Potluck as 192.0.2.1:8080. Nothing is kept.
// --pending mixed: the list holds zone-9 waiting and zone-8
// (pz-0000000000000002) cancelled, as the server keeps a cancelled zone
// listed with its state until its expiry; each answers by its id.
//
// --wide-zones: the request list also holds rows served by one, three and
// six machines (ids 190 to 192), for the Requests page's Zones column.
//
// --sequence: a scripted change for screenshots: 4 s after a client
// subscribes to the requests topic, the queued run 202 starts (a "started"
// frame), so a queued row can be seen turning into a running one.

import http from 'node:http';
import { createHash } from 'node:crypto';
import { readFileSync, existsSync, statSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { zonesStatus, counts, groupsDoc, uptimeDoc, fleetStrip } from './ui-fixture/zones.mjs';
import { schema as cfgSchema, configDoc, storedConfig, dryRun } from './ui-fixture/config.mjs';
import { profileEntries, profileSpecs, catalog as cfgCatalog, planAnswer } from './ui-fixture/profiles.mjs';
import { Keys, toolsAnswer } from './ui-fixture/keys.mjs';
import { modelsDoc, modelsInstances } from './ui-fixture/models.mjs';
import { tierLists, applyTiers, localEntry, tiersDoc } from './ui-fixture/tiers.mjs';

const argv = process.argv.slice(2);
const opt = (k, d) => { const i = argv.indexOf(k); return i >= 0 && argv[i + 1] ? argv[i + 1] : d; };
const here = path.dirname(fileURLToPath(import.meta.url));
const UI = path.resolve(opt('--ui', path.join(here, '..', 'internal', 'api', 'ui')));
const PORT = Number(opt('--port', '3122'));
const HOST = opt('--host', '0.0.0.0');
const STATE = opt('--state', 'normal');
const SEQUENCE = argv.includes('--sequence');
const LIVE_TIMINGS = argv.includes('--live-timings');
const WIDE_ZONES = argv.includes('--wide-zones');
const HEALTH = opt('--health', 'two');
// --release: the scheduler's release the Scheduler row names (facts.release)
const RELEASE = opt('--release', '0.1.5');
const ZONES = opt('--zones', 'none');
const UPTIME_PARTIAL = argv.includes('--uptime-partial');
const LOAD = opt('--load', 'start');
const LOAD_SET = argv.includes('--load');
const DRAIN = opt('--drain', '');
const PINNED = argv.includes('--pinned');
// --config: the configuration pages' state (scripts/ui-fixture/config.mjs)
const CONFIG = opt('--config', 'normal');
// --catalog: GET /api/models answers the profile fixture's six model files (default: none)
const CATALOG = argv.includes('--catalog');
// --named-only <name>: that zone is in no_auto_nodes (auto: false)
const NAMED_ONLY = opt('--named-only', '');
// --named-only-id <name>: that zone's ID (not its name) is added to no_auto_nodes (auto: false), as a stored id the Tiers page names
const NAMED_ONLY_ID = opt('--named-only-id', '');
// --loads: GET /api/history also holds two loadFinished events (a cached load that sent 414 kB, a full upload), for Activity's Loads
const LOADS = argv.includes('--loads');
// --no-host-uptime: no agent reports host_uptime_s (the zone panel leaves Uptime out)
const NO_HOST_UPTIME = argv.includes('--no-host-uptime');
// --pending: the Create new zone states (see the header)
const PENDING = opt('--pending', '');
const PZ_SOURCE = PENDING && PENDING !== 'nosource' ? 'share:192.0.2.10:/models' : '';
const PZ_ID = 'pz-0000000000000001', PZ_ID2 = 'pz-0000000000000002';
const PZ_TEXT = { waiting: 'Waiting for the machine to check in', joined: 'Joined', expired: 'Expired: the machine did not check in in time', cancelled: 'Cancelled' };
function pendingZone(state, b = {}, id = PZ_ID) {
  const t = Date.now(), name = b.name || 'zone-9', script = '/install.sh?zone=' + id;
  const z = { id, name, group: b.group === undefined ? 'rack-a' : b.group || undefined, model_source: b.model_source || PZ_SOURCE || 'push', mode: 'bare',
    state, state_text: PZ_TEXT[state], created_at: new Date(t - 4 * 60000).toISOString(), expires_at: new Date(t + 56 * 60000).toISOString(),
    command: "curl -sSL 'http://192.0.2.1:8080" + script + "' | sudo sh", script_path: script, download_path: script + '&download=1', download_name: 'potluck-zone-' + name + '.sh' };
  if (state === 'joined') Object.assign(z, { joined_node: '02:00:00:00:01:09', joined_at: new Date(t - 60000).toISOString() });
  return z;
}
function pendingRoute(req, res, p) {
  if (p === '/api/zones/pending' && req.method === 'POST') return readBody(req, b => {
    b = b && typeof b === 'object' ? b : {};
    if (PENDING === 'nosource' && !b.model_source) return json(res, 400, { error: { code: 400, message: 'no model source: set the default model source for new machines or give one for this zone', type: 'invalid_request_error' } });
    if (PENDING === 'taken') return json(res, 409, { error: { code: 409, message: 'the name ' + (b.name || 'zone-9') + ' is already taken by the zone 02:00:00:00:01:01 (active, lost or removed): choose another name', type: 'invalid_request_error' } });
    json(res, 201, pendingZone('waiting', b));
  });
  if (p === '/api/zones/pending' && req.method === 'GET') {
    const list = PENDING === 'mixed' ? [pendingZone('waiting'), pendingZone('cancelled', { name: 'zone-8' }, PZ_ID2)]
      : PENDING && PENDING !== 'gone' && PZ_TEXT[PENDING] ? [pendingZone(PENDING)] : [];
    return json(res, 200, { pending: list, default_model_source: PZ_SOURCE });
  }
  if (PENDING === 'mixed' && p === '/api/zones/pending/' + PZ_ID2) {
    if (req.method === 'GET' || req.method === 'DELETE') return json(res, 200, pendingZone('cancelled', { name: 'zone-8' }, PZ_ID2));
  }
  if (p === '/api/zones/pending/' + PZ_ID) {
    if (PENDING === 'gone' || !PENDING) return json(res, 404, { error: { code: 404, message: 'no pending zone ' + PZ_ID, type: 'invalid_request_error' } });
    if (req.method === 'DELETE') return json(res, 200, pendingZone('cancelled'));
    if (req.method === 'GET') return json(res, 200, pendingZone(PZ_TEXT[PENDING] ? PENDING : 'waiting'));
  }
  return json(res, 404, { error: { code: 404, message: 'no pending zone ' + p.split('/').pop(), type: 'invalid_request_error' } });
}
// --keys none|normal: GET/POST /api/agents, PATCH/DELETE /api/agents/{id}, POST .../rotate in the access-key shapes
// (scripts/ui-fixture/keys.mjs; kept in memory, so the MCP pages can be driven end to end). Without it the old
// names-only /api/agents answer stays.
const KEYS_STATE = opt('--keys', '');
// --models none|five|full: GET /api/models as the Models list reads it, with the disk block (scripts/ui-fixture/models.mjs)
const MODELS_STATE = opt('--models', '');
const KEYS = KEYS_STATE ? new Keys(KEYS_STATE) : null;
// --tiers live|catchall: the Tiers and zones page (tiers.mjs)
const TIERS = opt('--tiers', '');
const TIER_LISTS = tierLists(TIERS);
const cfgDoc = () => { const d = configDoc(CONFIG); d.settings.requests_live_timings = LIVE_TIMINGS; return d; };

// ---------- the fixture (the Overview mockup's sample data) ----------

const MIB = 1024;
const H = 3600000;
const mac = n => '02:00:00:00:00:' + String(n).padStart(2, '0');
// name, chip, total GB, used GB, temp, what it does
const MACHINES = [
  { n: 1, name: 'ps5-01', chip: 'Oberon', total: 13.5, used: 10.7, temp: 61 },
  { n: 2, name: 'ps5-02', chip: 'Oberon', total: 13.5, used: 10.4, temp: 63 },
  { n: 3, name: 'ps5-03', chip: 'Oberon', total: 13.5, used: 4.2, temp: 58 },
  { n: 4, name: 'ps5-04', chip: 'Oberon', total: 13.5, used: 3.8, temp: 55 },
  { n: 5, name: 'laptop-01', chip: 'RTX 3070 Ti', total: 8.0, used: 6.9, temp: 67 },
  { n: 6, name: 'ps5-05', chip: 'Oberon', total: 13.5, used: 0.3, temp: 49 },
  { n: 7, name: 'ps5-06', chip: 'Oberon', total: 13.5, used: 0, temp: null, offline: true },
  { n: 8, name: 'laptop-02', chip: 'RTX 5060', total: 8.0, used: 1.2, temp: 44, excluded: true },
];
const BIG = 'gemma-4-27b:32k', SMALL = 'gemma-4-12b:64k', QWEN = 'qwen3-14b';
const iso = ms => new Date(ms).toISOString();
// the scheduler's own machine, when it hosts llama-server for consoles that hold the whole model
const SCHED = { id: 'scheduler', name: 'scheduler', role: 'host' };

// status(now) -> the status of the --zones state
// with --models the instances are the catalog's (one loaded model, or none)
const status = now => applyTiers(named(zonesStatus(MODELS_STATE ? modelsInstances(baseStatus(now), MODELS_STATE, mac) : baseStatus(now), now, ZONES, { drain: DRAIN, pinned: PINNED, hostUptime: !NO_HOST_UPTIME })), TIERS);
const named = st => { for (const n of (st.view && st.view.nodes) || []) if ((NAMED_ONLY && n.hostname === NAMED_ONLY) || (NAMED_ONLY_ID && n.hostname === NAMED_ONLY_ID)) n.auto = false; return st; };
// withNamedId(doc, now): --named-only-id appends the zone's id to the stored no_auto_nodes
const withNamedId = (d, now) => { if (!NAMED_ONLY_ID) return d; const z = (status(now).agents || []).find(a => a.name === NAMED_ONLY_ID); if (z) d.no_auto_nodes = [...(d.no_auto_nodes || []), z.id]; return d; };

function baseStatus(now) {
  const t = Math.round(now / 1000) * 1000;
  const nodes = MACHINES.map(m => ({
    id: mac(m.n), hostname: m.name, local: false, online: !m.offline, addr: '', priority: 1, eligible: !m.excluded, auto: true,
    excluded: !!m.excluded, excluded_by: m.excluded ? mac(m.n) : '', unplaced: '', unplaced_message: '',
    total_mib: Math.round(m.total * MIB), free_mib: Math.round((m.total - m.used) * MIB),
    holder: [1, 2, 5].includes(m.n) ? BIG : m.n === 3 ? SMALL : '',
    claimed_by: m.n === 4 ? QWEN : '', settling: m.n === 4 ? 'settling' : '', settle_until: m.n === 4 ? iso(t + 12000) : null,
    free_at: m.n === 4 ? iso(t + 12000) : null, free_basis: m.n === 4 ? 'settle' : '',
    quarantined: m.n === 6 ? iso(t - 600000) : null, missing_since: m.offline ? iso(t - 70 * 60000) : null,
    reserved: [], reserved_mib: 0,
  }));
  const agents = MACHINES.map(m => ({
    id: mac(m.n), name: m.name, addr: '192.0.2.' + (100 + m.n), state: m.offline ? 'lost' : 'active', paused: false, placeable: !m.excluded && !m.offline,
    excluded: !!m.excluded, excluded_by: m.excluded ? mac(m.n) : undefined, unplaced: m.excluded ? 'excluded' : '',
    excluded_at: m.excluded ? iso(t - 26 * H) : undefined, excluded_actor: m.excluded ? 'anonymous' : undefined,
    last_seen_ago_s: m.offline ? 4440 : 1, platform: m.chip === 'Oberon' ? 'linux-amd64-vulkan' : 'linux-amd64-cuda', release: '0.1.9',
    devices: [{ name: m.chip, backend: m.chip === 'Oberon' ? 'vulkan' : 'cuda', total_mib: Math.round(m.total * MIB) }],
    stats: m.temp == null ? {} : { headline_temp_c: m.temp },
    set_aside: m.n === 6 ? { reason: 'preflight_failed', message: 'The pre-load check failed: the model did not fit the free memory.', until: iso(t + 6 * 60000), level: 1 } : undefined,
  }));
  return {
    time: iso(t), shutting_down: false, membership: 'agents', configured: true, config_revision: 12,
    instances: [
      { profile: BIG, state: 'ready', in_flight: 2, waiters: 0, parallel: 4, host: 'node', host_node: mac(1), requests_served: 190,
        nodes: [{ id: mac(1), role: 'host', share: 0.4 }, { id: mac(2), role: 'helper', share: 0.35 }, { id: mac(5), role: 'helper', share: 0.25 }] },
      { profile: SMALL, state: 'starting', in_flight: 0, waiters: 1, load_progress: 0.56, load: { progress: 0.56 }, parallel: 1, host: 'node', host_node: mac(3),
        nodes: [{ id: mac(3), role: 'host', share: 1 }] },
    ],
    queue: [{ profile: SMALL, waiters: 1, age: 348, claimed: true, claim: [mac(3)] }, { profile: QWEN, waiters: 1, age: 37, claimed: true, claim: [mac(4)] }],
    quarantines: [{ node: mac(6), until: iso(t + 6 * 60000), level: 1, reason: 'preflight_failed', message: 'The pre-load check failed: the model did not fit the free memory.', since: iso(t - 600000) }],
    backoffs: [], last_refusals: [], recent_loads: [], learned_needs: {},
    view: { age: 1, connected: true, stale: false, nodes },
    agents,
  };
}

// the request rows of the mockup's table, the times relative to now
function requests(now) {
  // the machines that served a row, host first, each with its share of the
  // model (a fraction); the scheduler's own machine hosting holds no share
  const nodes = (...ns) => ns.map(([n, share], i) => Object.assign({ id: mac(n), name: MACHINES.find(m => m.n === n).name, role: i === 0 ? 'host' : 'helper' }, share ? { share } : {}));
  const big = nodes([1, 0.4], [2, 0.35], [5, 0.25]);
  const onSched = (...ns) => [SCHED].concat(nodes(...ns).map(n => Object.assign(n, { role: 'helper' })));
  const row = (id, o) => Object.assign({ id, kind: 'v1', stream: true, nodes: [], caller: 'anonymous', caller_kind: 'default',
    tokens_in: null, tokens_reused: null, tokens_out: null, prefill_tps: null, decode_tps: null, wait_ms: 0, duration_ms: 0 }, o,
    // a running row's prompt figures come when it ends unless requests_live_timings is on
    o.status === 'running' && !LIVE_TIMINGS ? { tokens_in: null, tokens_reused: null, prefill_tps: null } : {});
  return [
    row(203, { status: 'queued', queued_at: now - 344000, profile: SMALL, live: true, wait_ms: 344000 }),
    row(202, { status: 'queued', queued_at: now - 37000, profile: QWEN, live: true, wait_ms: 37000 }),
    row(201, { status: 'running', queued_at: now - 22900, started_at: now - 22600, profile: BIG, live: true, tokens_in: 2460, prefill_tps: 412, tokens_out: 318, decode_tps: 14.1, nodes: big, duration_ms: 22600 }),
    row(200, { status: 'running', queued_at: now - 3900, started_at: now - 3800, profile: BIG, live: true, tokens_in: 705, prefill_tps: 405, tokens_out: 54, decode_tps: 14.3, nodes: big, duration_ms: 3800 }),
    row(199, { status: 'done', queued_at: now - 51500, started_at: now - 51000, ended_at: now - 8500, profile: BIG, tokens_in: 1840, prefill_tps: 418, tokens_out: 612, decode_tps: 14.4, nodes: big, duration_ms: 42500, wait_ms: 500 }),
    row(198, { status: 'failed', reason: 'cut', message: 'the zone dropped the connection', queued_at: now - 83500, started_at: now - 83000, ended_at: now - 74800, profile: QWEN, tokens_in: 1120, prefill_tps: 598, tokens_out: 37, decode_tps: 20.8, nodes: onSched([4, 1]), duration_ms: 8200, wait_ms: 500 }),
    row(197, { status: 'done', queued_at: now - 77300, started_at: now - 77000, ended_at: now - 59900, profile: BIG, tokens_in: 920, prefill_tps: 409, tokens_out: 238, decode_tps: 13.9, nodes: nodes([1, 0.5], [2, 0.5]), duration_ms: 17100, wait_ms: 300 }),
    row(196, { status: 'cancelled', reason: 'client_left', message: 'cancelled by the app that sent it', queued_at: now - 124200, started_at: now - 124000, ended_at: now - 120000, profile: BIG, tokens_in: 640, prefill_tps: 415, tokens_out: 21, decode_tps: 14.0, nodes: big, duration_ms: 4000, wait_ms: 200 }),
    row(195, { status: 'refused', reason: 'queue_timeout', message: 'it waited longer than the queue time limit', queued_at: now - 700000, ended_at: now - 100000, profile: QWEN, wait_ms: 600000 }),
    // a non-streamed request whose app left: cancelled, no token counted
    row(194, { status: 'cancelled', reason: 'client_left', message: 'cancelled by the app that sent it', stream: false, queued_at: now - 760000, started_at: now - 759800, ended_at: now - 751800, profile: QWEN, nodes: onSched([4, 1]), duration_ms: 8000, wait_ms: 200 }),
  ].concat(WIDE_ZONES ? [
    // --wide-zones: one, three and six machines
    row(192, { status: 'done', queued_at: now - 800000, started_at: now - 799800, ended_at: now - 795900, profile: QWEN, tokens_in: 32, prefill_tps: 53.3, tokens_out: 160, decode_tps: 48.0, nodes: nodes([3, 1]), duration_ms: 3900, wait_ms: 200 }),
    row(191, { status: 'done', queued_at: now - 820000, started_at: now - 819800, ended_at: now - 817400, profile: BIG, tokens_in: 28, prefill_tps: 120, tokens_out: 134, decode_tps: 62.1, nodes: onSched([3, 0.5], [4, 0.5]), duration_ms: 2400, wait_ms: 200 }),
    row(190, { status: 'done', queued_at: now - 840000, started_at: now - 839800, ended_at: now - 834500, profile: BIG, tokens_in: 39, prefill_tps: 377, tokens_out: 160, decode_tps: 47.8, nodes: nodes([1, 0.17], [2, 0.17], [3, 0.17], [4, 0.17], [5, 0.16], [6, 0.16]), duration_ms: 5300, wait_ms: 200 }),
  ] : []);
}

// tokens in and out per hour of the mockup's day (00:00 to 23:00)
const IN = [6000, 3000, 0, 0, 0, 1800, 4000, 9000, 31000, 30000, 48000, 21000, 12000, 40000, 46000, 28000, 11500, 0, 0, 0, 0, 0, 0, 0];
const OUT = [1700, 870, 0, 0, 0, 650, 1300, 2600, 8900, 8700, 13900, 6100, 3500, 11500, 13300, 8000, 3300, 0, 0, 0, 0, 0, 0, 0];
function summary(q, now) {
  const from = Number(q.get('from')) || now - 24 * H, to = Number(q.get('to')) || now;
  const step = q.get('step') || 'hour';
  const width = step === 'day' ? 24 * H : H;
  const series = [];
  const tot = { requests: 0, done: 0, failed: 0, refused: 0, cancelled: 0, tokens_in: 0, tokens_reused: 0, tokens_out: 0, wait_ms: 0, duration_ms: 0, prefill_tps: 412.3, decode_tps: 14.2 };
  for (let b = from; b < to; b += width) {
    const hr = new Date(b).getHours();
    const past = b <= now;
    const k = STATE === 'normal' && past ? (step === 'day' ? 1 : 0) : -1;
    const tin = k < 0 ? 0 : step === 'day' ? IN.reduce((a, x) => a + x, 0) : IN[hr];
    const tout = k < 0 ? 0 : step === 'day' ? OUT.reduce((a, x) => a + x, 0) : OUT[hr];
    const reqs = tin ? Math.max(1, Math.round(tin / 1450)) : 0;
    series.push({ t: b, requests: reqs, done: reqs, failed: 0, refused: 0, cancelled: 0, tokens_in: tin, tokens_reused: 0, tokens_out: tout, wait_ms: 0, duration_ms: 0, prefill_tps: null, decode_tps: null });
    tot.requests += reqs; tot.done += reqs; tot.tokens_in += tin; tot.tokens_out += tout;
  }
  if (STATE === 'normal' && step === 'hour') { tot.requests = 199; tot.done = 196; tot.failed = 1; tot.refused = 1; tot.cancelled = 1; }
  return { from, to, step, source: 'hourly', totals: tot, series, live: { queued: STATE === 'normal' ? 2 : 0, running: STATE === 'normal' ? 2 : 0 },
    oldest_ts: STATE === 'normal' ? now - 9 * 24 * H : 0, history_begins_at: STATE === 'off' ? 0 : now - 9 * 24 * H, on: STATE !== 'off' };
}

// each machine's last 24 hours, as the mockup draws them (oldest first)
const STRIPS = {
  1: 'c2 c3 c3 c2 c1 a a a a a a a a c1 c2 c3 c3 c4 c2 c1 c3 c3 c2 c4',
  2: 'c1 c2 c2 c1 a a a a a a a a a a c1 c2 c2 c3 c1 a c1 c2 c2 c4',
  3: 'c3 c3 c2 a a a a a a a a a a a a c1 a a a c3 c2 a a ld',
  4: 'a c1 c2 c2 a a a a a a a a a a a c2 c1 a a a c3 c4 c4 c3',
  5: 'c2 c2 c1 a a a a a a a a a a a a c1 c1 c2 c3 c1 c1 c2 a c4',
  6: 'a a c1 c1 a a a a a a a a a a a a c1 a a c2 c2 a a dn',
  7: 'dn dn a a a a a a a a a a a a a a a c2 c2 a a a dn dn',
  8: 'x x x x x x x x x x x x x x x x x x x x x x x x',
};
const CELL = { a: ['free', 0], c1: ['in_use', 1], c2: ['in_use', 2], c3: ['in_use', 3], c4: ['in_use', 4], ld: ['loading', 0], dn: ['down', 0], x: ['off', 0] };
function availability(q, now) {
  const st = status(now);
  const group = q.get('group');
  const hourNow = Math.floor(now / H) * H;
  const to = Number(q.get('to')) || now;
  const last = Math.floor((to - 1) / H) * H;
  const first = Number(q.get('from')) ? Math.floor(Number(q.get('from')) / H) * H : last - 23 * H;
  // a removed machine is left out, as the server does
  const nodes = st.agents.filter(a => a.zone_state !== 'removed' && (!group || a.group === group)).map(a => {
    const m = MACHINES.find(x => mac(x.n) === a.id && x.name === a.name) || { offline: a.zone_state === 'lost' };
    const codes = (m.n ? STRIPS[m.n].split(' ') : fleetStrip(st, a)) || ['?'];
    const cells = [];
    for (let s = first, i = 0; s <= last; s += H, i++) {
      const [state, busy] = STATE === 'off' ? ['unknown', 0] : CELL[codes[i % codes.length]] || ['unknown', 0];
      cells.push({ start: iso(s), span_s: s === hourNow ? (now - s) / 1000 : 3600, partial: s === hourNow, busy_level: busy, state,
        seconds: { free: 0, in_use: 0, loading: 0, down: 0, off: 0, unknown: 0 }, down_lost_s: 0, down_set_aside_s: 0, busy: null, reachable_s: 0, uptime: null });
    }
    return { id: a.id, name: a.name, known_s: 86400, reachable_s: 86400, uptime: m.offline ? 0.83 : 1, cells };
  });
  return { from: iso(first), to: iso(to), step: 'hour', history_begins_at: STATE === 'off' ? null : iso(now - 9 * 24 * H), recording: STATE !== 'off', nodes };
}

// ---------- serving ----------

const TYPES = { '.html': 'text/html; charset=utf-8', '.css': 'text/css; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.json': 'application/json',
  '.svg': 'image/svg+xml', '.png': 'image/png', '.woff2': 'font/woff2', '.txt': 'text/plain; charset=utf-8', '.md': 'text/markdown; charset=utf-8' };
const isFile = p => existsSync(p) && statSync(p).isFile();
// as internal/api/uifiles.go: /ui/<p> is next/<p> when it exists, else <p>; /ui/classic/ is gone
function uiFile(rel) {
  rel = path.posix.normalize('/' + rel).slice(1);
  if (rel === '' ) return path.join(UI, 'next', 'index.html');
  if (rel.startsWith('next/') || rel === 'classic' || rel.startsWith('classic/')) return null;
  if (isFile(path.join(UI, 'next', rel))) return path.join(UI, 'next', rel);
  return isFile(path.join(UI, rel)) ? path.join(UI, rel) : null;
}

const readBody = (req, fn) => { let s = ''; req.on('data', c => { s += c; }); req.on('end', () => { let b = null; try { b = JSON.parse(s || 'null'); } catch { /* not JSON */ } fn(b); }); };
const json = (res, code, body) => { res.writeHead(code, { 'Content-Type': 'application/json', 'Cache-Control': 'no-cache' }); res.end(JSON.stringify(body)); };
const notFound = (res, what) => json(res, 404, { error: { message: 'not in the fixture: ' + what, type: 'invalid_request_error' } });

const server = http.createServer((req, res) => {
  const u = new URL(req.url, 'http://fixture');
  const p = u.pathname;
  const now = Date.now();
  if (p === '/') { res.writeHead(302, { Location: '/ui/' }); return res.end(); }
  if (p === '/ui') { res.writeHead(301, { Location: '/ui/' }); return res.end(); }
  // the components page (scripts/ui-fixture/components.js): the app with the
  // fixture's demonstration view registered before it starts
  if (p === '/ui/components.html') {
    const html = readFileSync(uiFile('index.html'), 'utf8').replace('<script type="module" src="app.js"></script>',
      '<script type="module" src="_fixture/components.js"></script>' + String.fromCharCode(10) + '<script type="module" src="app.js"></script>');
    res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8', 'Cache-Control': 'no-cache' });
    return res.end(html);
  }
  // /ui/fx-<name>.html: the app with scripts/ui-fixture/fx-<name>.js run after
  // it, a board the compare tool cannot reach by clicking (it drives the page
  // itself and sends nothing)
  const fx = /^\/ui\/fx-([a-z0-9-]+)\.html$/.exec(p);
  if (fx && existsSync(path.join(here, 'ui-fixture', 'fx-' + fx[1] + '.js'))) {
    const html = readFileSync(uiFile('index.html'), 'utf8').replace('<script type="module" src="app.js"></script>',
      '<script type="module" src="app.js"></script>' + String.fromCharCode(10) + '<script type="module" src="_fixture/fx-' + fx[1] + '.js"></script>');
    res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8', 'Cache-Control': 'no-cache' });
    return res.end(html);
  }
  if (p.startsWith('/ui/_fixture/')) {
    const f = path.join(here, 'ui-fixture', path.basename(p));
    if (!existsSync(f)) return notFound(res, p);
    res.writeHead(200, { 'Content-Type': 'text/javascript; charset=utf-8', 'Cache-Control': 'no-cache' });
    return res.end(readFileSync(f));
  }
  if (p.startsWith('/ui/')) {
    const f = uiFile(decodeURIComponent(p.slice(4)));
    const type = f && TYPES[path.extname(f)];
    if (!type) { res.writeHead(404); return res.end('not found'); }
    res.writeHead(200, { 'Content-Type': type, 'Cache-Control': 'no-cache' });
    return res.end(readFileSync(f));
  }
  if (!p.startsWith('/api/')) return notFound(res, p);
  // a dry run of a settings write writes nothing: it answers what the server would
  const cfgW = /^\/api\/config\/(settings|recovery|fleet)$/.exec(p);
  if (cfgW && req.method === 'PATCH' && u.searchParams.get('dry_run')) return readBody(req, b => { const r = dryRun(cfgW[1], b, cfgDoc()); json(res, r.status, r.body); });
  // the placement preview's dry run writes nothing
  // POST /api/plan: the Zones fixture's answers when --load names a load state (the Load dialog's states),
  // else the configuration pages' (profiles.mjs, Placement preview); the other model actions are the Zones fixture's
  if (KEYS && p === '/api/mcp/tools') return json(res, 200, toolsAnswer());
  if (KEYS && (p === '/api/agents' || p.startsWith('/api/agents/'))) {
    const host = req.headers.host || 'fixture';
    const rot = /^\/api\/agents\/([^/]+)\/rotate$/.exec(p), one = /^\/api\/agents\/([^/]+)$/.exec(p);
    const send = r => json(res, r.status, r.body);
    if (p === '/api/agents' && req.method === 'GET') return send(KEYS.listAnswer(host));
    if (p === '/api/agents' && req.method === 'POST') return readBody(req, b => send(KEYS.create(b || {}, host)));
    if (rot && req.method === 'POST') return send(KEYS.rotate(rot[1], host));
    if (one && req.method === 'PATCH') return readBody(req, b => send(KEYS.patch(one[1], b || {}, host)));
    if (one && req.method === 'DELETE') return send(KEYS.revoke(one[1], host));
  }
  if (p === '/api/plan' && req.method === 'POST' && !LOAD_SET) return readBody(req, b => { const r = planAnswer(b); json(res, r.status, r.body); });
  if (req.method === 'POST' && MODEL_ROUTE.test(p)) return modelAction(req, res, p);
  if (p.startsWith('/api/zones/pending')) return pendingRoute(req, res, p);
  if (req.method !== 'GET') return json(res, 405, { error: { message: 'the fixture is read-only' } });
  if (STATE === 'loading' && p.startsWith('/api/requests')) return; // never answers
  if (p === '/api/status') return json(res, 200, status(now));
  if (p === '/api/models') return json(res, 200, MODELS_STATE ? modelsDoc(MODELS_STATE, now) : { version: 1, models: CATALOG ? cfgCatalog() : [] });
  if (p === '/api/profiles') return json(res, 200, { profiles: profileEntries() });
  if (p === '/api/config') { const d = cfgDoc(); return json(res, 200, withNamedId(Object.assign({ updated_at: now - 3 * H, configured: d.revision > 0, tiers: [], exclude_nodes: [], no_auto_nodes: NAMED_ONLY ? [NAMED_ONLY] : [], node_overrides: {} }, d, TIER_LISTS, { profiles: profileSpecs() }), now)); }
  // what the health popup reads (the Health board's sample, generic names)
  if (p === '/api/about') return json(res, 200, { version: '0.1.5', started_at: now - 12 * 24 * H, now, uptime_s: 12 * 24 * 3600, history: 'on' });
  if (p === '/api/system') return json(res, 200, { version: '0.1.5', started_at: now - 12 * 24 * H, now, uptime_s: 12 * 24 * 3600, read_panics: 0, internal_failures_total: 0,
    tasks: [{ name: 'core loop', policy: 'exit', state: 'running', running: 1, restarts: 0, failures: 0, failures_last_min: 0 }],
    stored_config: storedConfig(CONFIG),
    storage: { database: { dir: '/data', on: true, bytes: 37600000 }, history: { dir: '/data/history', on: true, bytes: 3000000, files: 4 }, usage: { size_bytes: 9400000 }, child_logs: { dir: '/data/logs', bytes: 23700000, files: 12 } } });
  if (p === '/api/agents') return json(res, 200, { agents: ['ops-bot', 'coding agent', 'report writer'].map((name, i) => ({ id: 'agent-' + i, name, key_prefix: 'ptk_' + i, created_at: now - (i + 2) * 24 * H, request_count: 10 * i })) });
  if (p === '/api/config/schema') return json(res, 200, cfgSchema);
  if (p === '/api/config/settings') { const d = cfgDoc(); return json(res, 200, { revision: d.revision, settings: PENDING ? Object.assign({}, d.settings, { default_model_source: PZ_SOURCE }) : d.settings, settings_set: d.settings_set }); }
  if (p === '/api/config/recovery') { const d = cfgDoc(); return json(res, 200, { revision: d.revision, recovery: d.recovery }); }
  if (p === '/api/config/fleet') { const d = cfgDoc(); return json(res, 200, { revision: d.revision, fleet: d.fleet }); }
  if (p === '/api/history/summary' && LOADS) return json(res, 200, { events: 3, by_severity: { info: 2, warn: 1 }, loads: { ready: 2, failed: 0, cancelled: 0 }, incidents: [] });
  if (p === '/api/history' && LOADS) {
    const ld = [
      { ts: now - 3 * 60000, seq: 43, type: 'loadFinished', severity: 'info', profile: 'qwen3-14b:16k', nodes: ['ps5-01'], summary: 'qwen3-14b:16k is ready.', data: { outcome: 'ready', load_s: 6.3, bytes: 414000, avg_mb_s: 0.066 } },
      { ts: now - 9 * 60000, seq: 42, type: 'loadFinished', severity: 'info', profile: 'gemma-4-27b:32k', nodes: ['ps5-02', 'ps5-03'], summary: 'gemma-4-27b:32k is ready.', data: { outcome: 'ready', load_s: 21, bytes: 16500000000, avg_mb_s: 104.6 } },
    ];
    if (u.searchParams.get('type') === 'loadFinished') return json(res, 200, { events: ld, total_matching: ld.length });
    return json(res, 200, { events: [...ld,
      { ts: now - 12 * 60000, seq: 41, type: 'nodeQuarantined', severity: 'warn', nodes: ['ps5-05'], summary: 'Node ps5-05 was quarantined for 18m0s (level 1): preflight failed.', data: { for_s: 1080, level: 1 } }], total_matching: 3 });
  }
  if (p === '/api/history') return json(res, 200, { events: [
    { ts: now - 12 * 60000, seq: 41, type: 'nodeQuarantined', severity: 'warn', nodes: ['ps5-05'], summary: 'Node ps5-05 was quarantined for 18m0s (level 1): preflight failed.', data: { for_s: 1080, level: 1 } },
    { ts: now - 54 * 60000, seq: 40, type: 'nodeLost', severity: 'error', nodes: ['ps5-07'], summary: 'Node ps5-07 was lost.', data: {} },
    { ts: now - 70 * 60000, seq: 39, type: 'nodeLost', severity: 'error', nodes: ['ps5-06'], summary: 'Node ps5-06 was lost.', data: {} },
  ], total_matching: 3 });
  if (p === '/api/health') return json(res, 200, health(now));
  if (p === '/api/nodes') { const st = status(now), l = localEntry(TIERS); return json(res, 200, { nodes: l ? [...st.agents, l] : st.agents, counts: counts(st.agents) }); }
  if (p === '/api/config/tiers') return json(res, 200, tiersDoc(TIERS, status(now), cfgDoc().revision));
  if (p === '/api/groups') return json(res, 200, groupsDoc(status(now), now, UPTIME_PARTIAL));
  if (p === '/api/nodes/uptime') return json(res, 200, uptimeDoc(status(now), now, UPTIME_PARTIAL));
  if (p === '/api/nodes/availability') return json(res, 200, availability(u.searchParams, now));
  if (p === '/api/requests/summary') return json(res, 200, summary(u.searchParams, now));
  if (p === '/api/requests') {
    let rows = STATE === 'normal' ? requests(now) : [];
    const st = (u.searchParams.get('status') || '').split(',').filter(Boolean);
    if (st.length) rows = rows.filter(r => st.includes(r.status));
    const prof = u.searchParams.get('profile');
    if (prof) rows = rows.filter(r => r.profile === prof);
    const limit = Number(u.searchParams.get('limit')) || 50;
    return json(res, 200, { requests: rows.slice(0, limit), on: STATE !== 'off', history_begins_at: STATE === 'off' ? 0 : now - 9 * 24 * H });
  }
  const m = /^\/api\/requests\/(\d+)$/.exec(p);
  if (m) { const r = requests(now).find(x => String(x.id) === m[1]); return r ? json(res, 200, r) : notFound(res, p); }
  return notFound(res, p);
});

// ---------- the model actions (fixed answers, nothing changes) ----------

const PROFILES = [
  { id: BIG, model: 'gemma-4-27b', ctx: 32768, needs_mib: 18900, generated: false },
  { id: SMALL, model: 'gemma-4-12b', ctx: 65536, needs_mib: 9800, generated: false },
  { id: QWEN, model: 'qwen3-14b', ctx: 16384, needs_mib: 11200, generated: false },
  { id: 'qwen3-4b', model: 'qwen3-4b', ctx: 8192, needs_mib: 3900, generated: false },
  { id: 'bench-qwen4b-auto-2048', model: 'qwen3-4b', ctx: 2048, needs_mib: 3300, generated: true },
];
const MODEL_ROUTE = /^\/api\/(plan|load|unload|models\/(drain|undrain)\/.+)$/;
const apiError = (res, code, reason, message) => json(res, code, { error: { code, message, reason, type: 'invalid_request_error', param: null } });
async function modelAction(req, res, p) {
  const body = await new Promise(ok => readBody(req, b => ok(b && typeof b === 'object' ? b : {})));
  const model = body.model || decodeURIComponent(p.replace(/^\/api\/models\/(drain|undrain)\//, ''));
  const nodes = Array.isArray(body.nodes) ? body.nodes : [];
  const first = nodes[0] || 'ps5-04';
  if ((p === '/api/plan' || p === '/api/load') && nodes.length) {
    if (LOAD === 'unknown') return apiError(res, 400, 'bad request', `"${first}" is not a node in the fleet view`);
    if (new Set(nodes).size !== nodes.length) return apiError(res, 400, 'bad request', `"${first}" is named twice`);
  }
  const unusable = `${model} cannot be loaded on the chosen zones: ${first} is paused`;
  if (p === '/api/plan') {
    const prof = PROFILES.find(x => x.id === model) || { needs_mib: 9800 };
    const placement = nodes.map(n => ({ id: n, addr: '', budget_mib: 13100, local: false }));
    const base = { model, action: 'start', placement, need_mib: prof.needs_mib, fit_need_mib: Math.round(prof.needs_mib * 1.05), measured: false, reason: '', warning: '', tight_fit: false };
    if (LOAD === 'reject') return json(res, 200, Object.assign(base, { action: 'reject', placement: [], reason: unusable }));
    if (LOAD === 'evict') return json(res, 200, Object.assign(base, { action: 'evict', victims: [{ profile: QWEN, busy: false }] }));
    return json(res, 200, base);
  }
  if (p === '/api/load') {
    if (LOAD === 'reject') return apiError(res, 409, 'chosen zone unusable', unusable);
    if (LOAD === 'loaded') return apiError(res, 409, 'already loaded or queued', `${model} is ready: unload it (or let the queued load start) before loading it on chosen nodes`);
    return json(res, 202, nodes.length ? { model, msg: 'loading', nodes } : { model, msg: 'loading' });
  }
  if (p === '/api/unload') return json(res, 200, { model, msg: 'ok' });
  if (p.startsWith('/api/models/drain/')) return json(res, 200, Object.assign({ model, msg: 'draining', next_load: 'The next load may start on another zone.' }, DRAIN === 'noeta' ? {} : { eta_s: 40 }));
  if (DRAIN) return json(res, 200, { model, msg: 'ok' });
  return apiError(res, 409, 'not_draining', model + ' is not draining');
}

// GET /api/health (internal/api/health.go): the server's judged rows and
// sentences, as the Health board draws them, in the state --health names
function health(now) {
  const iso = t => new Date(t).toISOString();
  const row = (id, label, state, text, link, count = 0) => ({ id, label, state, text, count, link, facts: {} });
  const two = HEALTH === 'two', unknown = HEALTH === 'unknown';
  const rows = [
    unknown ? row('zones', 'Zones', 'unknown', 'Status not read yet', '#/zones?filter=down')
      : two ? row('zones', 'Zones', 'problem', '7 of 8 reachable · 1 down', '#/zones?filter=down', 1) : row('zones', 'Zones', 'ok', '8 of 8 reachable', '#/zones'),
    two ? row('runs', 'Runs', 'attention', '1 failed in the last hour', '#/overview?runs=failed', 1) : row('runs', 'Runs', 'ok', 'None failed in the last hour', '#/overview'),
    Object.assign(row('scheduler', 'Scheduler', 'ok', `Running · release ${RELEASE} · up 12 d`, '#/about'), { facts: { release: RELEASE, uptime_s: 12 * 86400 } }),
    row('mcp', 'MCP server', 'ok', 'Running · 3 keys active', '#/mcp', 3),
    row('storage', 'Storage', 'ok', '50.3 of 240 GB used', '#/about', 21),
    unknown ? row('queue', 'Queue', 'unknown', 'Status not read yet', '#/monitor/queue') : row('queue', 'Queue', 'ok', '2 waiting · longest 5 min 48 s', '#/monitor/queue', 2),
    row('alerts', 'Alerts', 'unknown', 'Not set up yet', '#/health/alerts'),
  ];
  const issues = rows.filter(r => r.state === 'attention' || r.state === 'problem').length;
  const worst = rows.some(r => r.state === 'problem') ? 'problem' : issues ? 'attention' : 'ok';
  return {
    state: worst, color: { ok: 'green', attention: 'amber', problem: 'red' }[worst], issues,
    sentence: issues === 0 ? 'All good' : issues === 1 ? '1 thing needs attention' : issues + ' things need attention',
    updated_at: iso(now), rows,
    recent: two ? [
      { at: iso(now - 12 * 60000), seq: 41, type: 'nodeQuarantined', severity: 'warn', text: 'ps5-05 failed a load and was set aside', node: 'ps5-05', until: iso(now + 6 * 60000) },
      { at: iso(now - 54 * 60000), seq: 40, type: 'nodeLost', severity: 'error', text: 'ps5-07 went offline', node: 'ps5-07' },
      { at: iso(now - 70 * 60000), seq: 39, type: 'nodeLost', severity: 'error', text: 'ps5-06 went offline', node: 'ps5-06' },
    ] : [],
    recent_link: '#/activity?range=2h&sev=warn', alerts_link: '#/health/alerts',
  };
}

// /ws: a minimal WebSocket (RFC 6455): hello (with the topics, requests among
// them), the status every 5 s, and the requests topic once subscribed
function frame(text) {
  const b = Buffer.from(text);
  const head = b.length < 126 ? Buffer.from([0x81, b.length]) : b.length < 65536 ? Buffer.from([0x81, 126, b.length >> 8, b.length & 255]) : null;
  if (head) return Buffer.concat([head, b]);
  const h8 = Buffer.alloc(10); h8[0] = 0x81; h8[1] = 127; h8.writeBigUInt64BE(BigInt(b.length), 2);
  return Buffer.concat([h8, b]);
}
// the text frames a client sent (masked, as clients must); one chunk may hold several
function readFrames(buf) {
  const out = [];
  let i = 0;
  while (i + 2 <= buf.length) {
    const op = buf[i] & 15, masked = buf[i + 1] & 128;
    let len = buf[i + 1] & 127, at = i + 2;
    if (len === 126) { len = buf.readUInt16BE(at); at += 2; } else if (len === 127) { len = Number(buf.readBigUInt64BE(at)); at += 8; }
    const key = masked ? buf.subarray(at, at + 4) : null;
    if (masked) at += 4;
    if (at + len > buf.length) break;
    const body = Buffer.from(buf.subarray(at, at + len));
    if (key) for (let k = 0; k < body.length; k++) body[k] ^= key[k & 3];
    if (op === 1) out.push(body.toString('utf8'));
    i = at + len;
  }
  return out;
}
server.on('upgrade', (req, sock) => {
  if (new URL(req.url, 'http://fixture').pathname !== '/ws') { sock.destroy(); return; }
  const accept = createHash('sha1').update(req.headers['sec-websocket-key'] + '258EAFA5-E914-47DA-95CA-C5AB0DC85B11').digest('base64');
  sock.write('HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ' + accept + '\r\n\r\n');
  const send = (type, data) => { try { sock.write(frame(JSON.stringify({ type, ts: Date.now(), data }))); } catch { /* closed */ } };
  send('hello', { server_time: Date.now(), config_revision: 12, topics: ['status', 'event', 'bench', 'config', 'ping', 'models', 'requests', 'health'] });
  send('status', status(Date.now()));
  let reqs = false, seq = 0, hl = false;
  const t = setInterval(() => {
    send('status', status(Date.now()));
  }, 5000);
  // the requests topic: a snapshot on subscribe, then a tick a second with the running rows
  const tick = setInterval(() => {
    if (!reqs || STATE !== 'normal') return;
    send('requests', { kind: 'tick', requests: requests(Date.now()).filter(r => r.status === 'running') });
  }, 1000);
  sock.on('data', buf => {
    for (const msg of readFrames(buf)) {
      let m; try { m = JSON.parse(msg); } catch { continue; }
      if (m && m.type === 'subscribe') {
        const want = Array.isArray(m.topics) && m.topics.includes('requests');
        if (want && !reqs) {
          const now = Date.now();
          send('requests', { kind: 'snapshot', requests: STATE === 'normal' ? requests(now) : [], next: 0, on: STATE !== 'off', history_begins_at: STATE === 'off' ? null : now - 9 * 24 * H });
        }
        if (want && !reqs && SEQUENCE && STATE === 'normal') {
          seq = setTimeout(() => {
            const now = Date.now();
            const r = requests(now).find(x => x.id === 202);
            const served = [SCHED, { id: mac(4), name: 'ps5-04', role: 'helper', share: 1 }];
            send('requests', { kind: 'started', request: Object.assign({}, r, { status: 'running', started_at: now, wait_ms: now - r.queued_at, duration_ms: 0, nodes: served }) });
          }, 4000);
        }
        reqs = want;
        // the health topic: the whole document on subscribe (it does not change here)
        const wantH = Array.isArray(m.topics) && m.topics.includes('health');
        if (wantH && !hl) send('health', health(Date.now()));
        hl = wantH;
      }
    }
  });
  const stop = () => { clearInterval(t); clearInterval(tick); clearTimeout(seq); };
  sock.on('close', stop);
  sock.on('error', stop);
});

server.listen(PORT, HOST, () => console.log(`ui fixture (${STATE}) at http://${HOST}:${PORT}/ui/ serving ${UI}`));
