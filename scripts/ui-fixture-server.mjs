#!/usr/bin/env node
// ui-fixture-server.mjs: serves the web UI with fixed answers, no scheduler
// needed. For looking at a screen in a browser, and for the mockup
// comparison (the ui-compare tool): the answers mirror the sample data of
// the Overview mockup (design-canvas Main.dc.html), with generic names and
// documentation addresses, so a screenshot of the page and one of the
// artboard can be compared like for like. Node only, no dependencies.
//
// usage: node scripts/ui-fixture-server.mjs [--port 3122] [--ui internal/api/ui] [--host 0.0.0.0]
//                                           [--state normal|empty|off|loading] [--sequence] [--live-timings]
//                                           [--health two|good|unknown]
//
//   /ui/...      the new shell (ui/next laid over the classic files, as the
//                scheduler serves it), /ui/classic/... the classic UI
//   /api/status, /api/requests, /api/requests/summary, /api/requests/{id},
//   /api/nodes/availability, /api/nodes, /api/models, and for the health popup
//   /api/system, /api/about, /api/agents, /api/config/settings, /api/history:
//   fixed answers (times relative to now)
//   /ws          a WebSocket: hello, the status every 5 s, the requests topic
//   any other /api/... answers 404 in the API's error shape; writes 405.
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
// --sequence: a scripted change for screenshots: 4 s after a client
// subscribes to the requests topic, the queued run 202 starts (a "started"
// frame), so a queued row can be seen turning into a running one.

import http from 'node:http';
import { createHash } from 'node:crypto';
import { readFileSync, existsSync, statSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const argv = process.argv.slice(2);
const opt = (k, d) => { const i = argv.indexOf(k); return i >= 0 && argv[i + 1] ? argv[i + 1] : d; };
const here = path.dirname(fileURLToPath(import.meta.url));
const UI = path.resolve(opt('--ui', path.join(here, '..', 'internal', 'api', 'ui')));
const PORT = Number(opt('--port', '3122'));
const HOST = opt('--host', '0.0.0.0');
const STATE = opt('--state', 'normal');
const SEQUENCE = argv.includes('--sequence');
const LIVE_TIMINGS = argv.includes('--live-timings');
const HEALTH = opt('--health', 'two');

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

function status(now) {
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
  ];
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
  const hourNow = Math.floor(now / H) * H;
  const to = Number(q.get('to')) || now;
  const last = Math.floor((to - 1) / H) * H;
  const first = Number(q.get('from')) ? Math.floor(Number(q.get('from')) / H) * H : last - 23 * H;
  const nodes = MACHINES.map(m => {
    const codes = STRIPS[m.n].split(' ');
    const cells = [];
    for (let s = first, i = 0; s <= last; s += H, i++) {
      const [state, busy] = STATE === 'off' ? ['unknown', 0] : CELL[codes[i % codes.length]] || ['unknown', 0];
      cells.push({ start: iso(s), span_s: s === hourNow ? (now - s) / 1000 : 3600, partial: s === hourNow, busy_level: busy, state,
        seconds: { free: 0, in_use: 0, loading: 0, down: 0, off: 0, unknown: 0 }, down_lost_s: 0, down_set_aside_s: 0, busy: null, reachable_s: 0, uptime: null });
    }
    return { id: mac(m.n), name: m.name, known_s: 86400, reachable_s: 86400, uptime: m.offline ? 0.83 : 1, cells };
  });
  return { from: iso(first), to: iso(to), step: 'hour', history_begins_at: STATE === 'off' ? null : iso(now - 9 * 24 * H), recording: STATE !== 'off', nodes };
}

// ---------- serving ----------

const TYPES = { '.html': 'text/html; charset=utf-8', '.css': 'text/css; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.json': 'application/json',
  '.svg': 'image/svg+xml', '.png': 'image/png', '.woff2': 'font/woff2', '.txt': 'text/plain; charset=utf-8', '.md': 'text/markdown; charset=utf-8' };
const isFile = p => existsSync(p) && statSync(p).isFile();
// as internal/api/uifiles.go: /ui/<p> is next/<p> when it exists, else <p>; /ui/classic/<p> is <p>
function uiFile(rel) {
  rel = path.posix.normalize('/' + rel).slice(1);
  if (rel === '' ) return path.join(UI, 'next', 'index.html');
  if (rel === 'classic' || rel === 'classic/') return path.join(UI, 'index.html');
  if (rel.startsWith('next/')) return null;
  if (rel.startsWith('classic/')) { const r = rel.slice(8); return r.startsWith('next/') || !isFile(path.join(UI, r)) ? null : path.join(UI, r); }
  if (isFile(path.join(UI, 'next', rel))) return path.join(UI, 'next', rel);
  return isFile(path.join(UI, rel)) ? path.join(UI, rel) : null;
}

const json = (res, code, body) => { res.writeHead(code, { 'Content-Type': 'application/json', 'Cache-Control': 'no-cache' }); res.end(JSON.stringify(body)); };
const notFound = (res, what) => json(res, 404, { error: { message: 'not in the fixture: ' + what, type: 'invalid_request_error' } });

const server = http.createServer((req, res) => {
  const u = new URL(req.url, 'http://fixture');
  const p = u.pathname;
  const now = Date.now();
  if (p === '/') { res.writeHead(302, { Location: '/ui/' }); return res.end(); }
  if (p === '/ui') { res.writeHead(301, { Location: '/ui/' }); return res.end(); }
  if (p.startsWith('/ui/')) {
    const f = uiFile(decodeURIComponent(p.slice(4)));
    const type = f && TYPES[path.extname(f)];
    if (!type) { res.writeHead(404); return res.end('not found'); }
    res.writeHead(200, { 'Content-Type': type, 'Cache-Control': 'no-cache' });
    return res.end(readFileSync(f));
  }
  if (!p.startsWith('/api/')) return notFound(res, p);
  if (req.method !== 'GET') return json(res, 405, { error: { message: 'the fixture is read-only' } });
  if (STATE === 'loading' && p.startsWith('/api/requests')) return; // never answers
  if (p === '/api/status') return json(res, 200, status(now));
  if (p === '/api/models') return json(res, 200, { version: 1, models: [] });
  // what the health popup reads (the Health board's sample, generic names)
  if (p === '/api/about') return json(res, 200, { version: '0.1.5', started_at: now - 12 * 24 * H, now, uptime_s: 12 * 24 * 3600, history: 'on' });
  if (p === '/api/system') return json(res, 200, { version: '0.1.5', started_at: now - 12 * 24 * H, now, uptime_s: 12 * 24 * 3600, read_panics: 0, internal_failures_total: 0,
    tasks: [{ name: 'core loop', policy: 'exit', state: 'running', running: 1, restarts: 0, failures: 0, failures_last_min: 0 }],
    stored_config: { unknown_fields: [], writes_need_force: false },
    storage: { database: { dir: '/data', on: true, bytes: 37600000 }, history: { dir: '/data/history', on: true, bytes: 3000000, files: 4 }, usage: { size_bytes: 9400000 }, child_logs: { dir: '/data/logs', bytes: 23700000, files: 12 } } });
  if (p === '/api/agents') return json(res, 200, { agents: ['ops-bot', 'coding agent', 'report writer'].map((name, i) => ({ id: 'agent-' + i, name, key_prefix: 'ptk_' + i, created_at: now - (i + 2) * 24 * H, request_count: 10 * i })) });
  if (p === '/api/config/settings') return json(res, 200, { settings: { queue_timeout: '30m0s', requests_live_timings: LIVE_TIMINGS } });
  if (p === '/api/history') return json(res, 200, { events: [
    { ts: now - 12 * 60000, seq: 41, type: 'nodeQuarantined', severity: 'warn', nodes: ['ps5-05'], summary: 'Node ps5-05 was quarantined for 18m0s (level 1): preflight failed.', data: { for_s: 1080, level: 1 } },
    { ts: now - 54 * 60000, seq: 40, type: 'nodeLost', severity: 'error', nodes: ['ps5-07'], summary: 'Node ps5-07 was lost.', data: {} },
    { ts: now - 70 * 60000, seq: 39, type: 'nodeLost', severity: 'error', nodes: ['ps5-06'], summary: 'Node ps5-06 was lost.', data: {} },
  ], total_matching: 3 });
  if (p === '/api/health') return json(res, 200, health(now));
  if (p === '/api/nodes') return json(res, 200, { nodes: status(now).agents });
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
    row('scheduler', 'Scheduler', 'ok', 'Running · release 0.1.5 · up 12 d', '#/about'),
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
