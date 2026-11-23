// About: the version, how long it has been up, the connection, where the data
// lives and how large it is, the safety settings in force, and the addresses
// other programs (and agents, through MCP) use. Read-only.
//
// Reads: REST GET /api/system and GET /api/about (on open, after a
// reconnect and with a status message every 30 s); WebSocket "hello" and "status" for the connection facts.

import { h } from '../core/dom.js';
import { registerView, href } from '../core/router.js';
import { state, on } from '../core/store.js';
import * as api from '../core/api.js';
import { liveView, pageHead, badge, stat, empty, skeleton, button, toast, humanize } from '../core/ui.js';
import { num, bytes, dur, ago, at, plural, now, DASH } from '../core/format.js';
import { connectionFacts } from '../core/shell.js';
import { agentsMode } from '../core/nodeactions.js';

const origin = () => location.origin + api.url('');
const wsOrigin = () => origin().replace(/^http/, 'ws');

function copyRow(label, value, note) {
  const copy = async () => {
    try { await navigator.clipboard.writeText(value); toast('Copied ' + value, 'ok'); } catch { toast('Copy it by hand: ' + value); }
  };
  return [h('dt', label), h('dd', h('div.row', { style: 'gap:6px 10px' }, h('code.cmd', { style: 'font-size:13px;color:var(--text)' }, value), button('Copy', copy, 'sm')),
    note ? h('div.mut', { style: 'font-size:13px;margin-top:3px' }, note) : null)];
}

function settingValue(k, v) {
  if (v === true) return 'on';
  if (v === false) return 'off';
  if (Array.isArray(v)) return v.length ? v.join(', ') : 'none';
  if (typeof v === 'number') {
    if (/_s$/.test(k)) return v === 0 ? 'off' : dur(v);
    if (/_mib$/.test(k)) return num(v) + ' MiB';
    if (/margin$/.test(k)) return num(v * 100) + '%';
    if (/_pct$/.test(k)) return num(v) + '%';
    if (/_minutes$/.test(k)) return num(v) + ' min';
    return num(v, v % 1 ? 2 : 0);
  }
  return v == null || v === '' ? DASH : String(v);
}
const settingLabel = k => humanize(k.replace(/_(s|pct|minutes|mib)$/, ''));

// The background tasks the scheduler supervises (GET /api/system "tasks"):
// a task that fails is restarted alone, or turns its failure into its
// result; a failure that must not continue stops Potluck so that it is
// started again. Failures are also on the Activity page (internalFailure).
const POLICY = { restart: 'Restarts alone', exit: 'Stops Potluck', once: 'One-shot', guard: 'Guarded call', read: 'Read-only query', report: 'Reported' };
function tasksCard(sys) {
  const tasks = (sys && sys.tasks) || [];
  const hour = now() - 3600 * 1000;
  const recent = tasks.some(t => t.last_failure_at && t.last_failure_at >= hour);
  return h('section.card',
    h('div.ch.split', h('h2', 'Background tasks'), h('a', { href: href('activity'), style: 'font-size:13px' }, 'Activity')),
    !sys ? skeleton(3) : [
      recent ? h('div.banner.err', { role: 'status' }, 'A background task failed in the last hour and was handled (restarted, or its result became an error); see Activity.') : null,
      h('div.cs', { style: 'border-top:0;padding-top:0' }, h('div.mut', { style: 'font-size:13px;margin-bottom:6px' },
        sys.internal_failures_total ? plural(sys.internal_failures_total, 'failure') + ' recovered since start.' : 'No failure since start.')),
      tasks.length ? h('div.scroll', h('table.tbl.dense',
        h('thead', h('tr', h('th', 'Task'), h('th', 'After a failure'), h('th', 'State'), h('th', 'Restarts'), h('th', 'Last minute'), h('th', 'Last failure'))),
        h('tbody', tasks.map(t => h('tr',
          h('td.mono', t.name),
          h('td', POLICY[t.policy] || t.policy),
          h('td', t.state),
          h('td.mono', num(t.restarts || 0)),
          h('td.mono', num(t.failures_last_min || 0)),
          h('td', t.last_failure_at ? [ago(t.last_failure_at), h('div.mut.wrap-any', { style: 'font-size:12px' }, t.last_failure || '')] : DASH)))))) : null,
    ]);
}

function storageRow(title, s, extra) {
  if (!s) return null;
  const on_ = s.on !== false && (s.dir || s.path);
  return h('div.bl',
    h('span', h('b', title), ' ', on_ ? badge('On', 'use') : badge('Off', 'n')),
    h('span.mono', s.bytes != null ? bytes(s.bytes) : DASH),
    h('span.mono.mut.wrap-any', { style: 'font-size:12px' }, s.path || s.dir || 'No directory is configured'),
    h('span.mut', { style: 'font-size:13px' }, [s.files != null ? plural(s.files, 'file') : null, s.schema_version != null ? 'schema version ' + s.schema_version : null, extra].filter(Boolean).join(' · ')));
}

registerView({
  id: 'about', title: 'About',
  mount(el) {
    let sys = null, about = null, error = '', alive = true;
    const load = async () => {
      loadedAt = now();
      try { [sys, about] = await Promise.all([api.get('/api/system'), api.get('/api/about')]); error = ''; } catch (e) { error = e.message; }
      if (alive) view.refresh();
    };
    const offHello = on('hello', load); // a reconnect may mean a restart: read again
    // The background tasks' figures move without a reconnect: read again
    // with a status message at most every 30 s.
    let loadedAt = 0;
    const offStatus = on('status', () => { if (now() - loadedAt >= 30000) load(); });

    const view = liveView(el, () => {
      const hello = state.hello || {};
      const st = state.status;
      const conn = connectionFacts();
      const started = (sys && sys.started_at) || hello.started_at;
      const version = (sys && sys.version) || hello.version;
      const storage = (sys && sys.storage) || {};
      const safety = (sys && sys.safety) || null;
      const bench = (sys && sys.bench) || null;
      return [
        pageHead('About', version ? ['Version ', h('span.mono.txt2', version)] : 'Potluck'),
        error ? h('div.banner.err', 'The system details could not be read: ' + error) : null,
        h('div.cols',
          h('div.wide',
            h('section.card',
              h('div.ch', h('h2', 'Scheduler')),
              h('div.stats',
                stat('Version', version || DASH),
                stat('Started', started ? ago(started) : DASH),
                stat('Profiles', sys ? num(sys.profiles) : DASH),
                stat('Nodes', st ? num(((st.view || {}).nodes || []).length) : DASH),
                stat('Live clients', sys ? num(sys.live_clients) : DASH, sys ? 'of ' + num(sys.live_clients_max) : null, { title: 'Pages and programs connected to the live channel right now' })),
              h('div.cs', h('dl.kv',
                h('dt', 'Started at'), h('dd', started ? at(started) : DASH, started ? h('span.mut', ' · up ', dur(Math.max(0, Math.round((now() - started) / 1000)))) : null),
                h('dt', 'Listens on'), h('dd.mono', (sys && sys.listen) || DASH),
                h('dt', 'Models directory'), h('dd.mono', (sys && sys.models_dir) || DASH)))),
            tasksCard(sys),
            h('section.card',
              h('div.ch', h('h2', 'Connection')),
              h('div.cs', { style: 'border-top:0;padding-top:0' }, h('dl.kv',
                h('dt', 'This page'), h('dd', h('span.row', { style: 'gap:8px' }, h('span', { class: 'dot ' + conn.dot }), h('b', conn.title), h('span.mut', conn.note))),
                agentsMode(st) ? [h('dt', 'Nodes'), h('dd', 'From agent registrations: each worker runs potluck-agent and registers itself.')] :
                [h('dt', 'Fleet feed'), h('dd', !st || !st.view ? DASH : st.view.stale ? h('span.warn-t', `Stale: the last update is ${dur(Math.round(st.view.age))} old. Remote nodes are not used for new loads.`)
                  : !st.view.connected ? h('span.warn-t', 'Disconnected. The scheduler keeps retrying.') : 'Fresh: the fleet manager reports the nodes as they change.')],
                h('dt', 'Event history'), h('dd', hello.history === 'off' ? 'Off (started without --history-dir)' : hello.history === 'on' ? ['On', about && about.oldest_ts ? [' · oldest event ', ago(about.oldest_ts)] : null] : DASH),
                h('dt', 'Benchmarks'), h('dd', hello.bench === 'off' ? 'Off (no data directory, or the database could not be opened)' : hello.bench === 'on' ? 'On' : DASH),
                h('dt', 'Who may connect'), h('dd.mut', (sys && sys.origin_policy) || DASH),
                h('dt', 'Signing in'), h('dd.mut', 'There is none. Anyone who can reach this address on the network can read this page and change the configuration; every change is recorded in the ', h('a', { href: href('audit') }, 'audit trail'), '.')))),
            h('section.card',
              h('div.ch', h('h2', 'For programs and agents'), h('span.note', 'Addresses on this network')),
              h('div.cs', { style: 'border-top:0;padding-top:0' }, h('dl.kv',
                copyRow('MCP server', origin() + '/mcp', 'For agents that speak the Model Context Protocol. What an agent changes through it is recorded in the audit trail with the source MCP.'),
                copyRow('OpenAI-compatible API', origin() + '/v1', 'Send chat requests here; the "model" field is a profile id. The model is loaded when a request needs it.'),
                copyRow('REST', origin() + '/api/status', 'The same status this page shows, as JSON.'),
                copyRow('Live channel', wsOrigin() + '/ws', 'WebSocket: status, events and benchmark progress as they happen.'))))),
          h('div.side',
            h('section.card',
              h('div.ch', h('h2', 'Storage')),
              !sys ? skeleton(3) : [
                storageRow('Event history', storage.history),
                storageRow('Database', storage.database, 'benchmarks, runs and their results'),
                storage.child_logs ? storageRow('Model server logs', storage.child_logs, 'one file per load') : null,
              ]),
            h('section.card',
              h('div.ch.split', h('h2', 'Safety settings in force'), h('a', { href: href('settings'), style: 'font-size:13px' }, 'Settings')),
              !sys ? skeleton(4) : safety && Object.keys(safety).length
                ? h('div.cs', { style: 'border-top:0;padding-top:0' }, h('dl.kv', Object.keys(safety).sort().map(k => [h('dt', settingLabel(k)), h('dd', settingValue(k, safety[k]))])))
                : empty('Not reported', 'This server does not list its safety settings.')),
            bench ? h('section.card',
              h('div.ch.split', h('h2', 'Benchmark settings'), h('a', { href: href('benchmarks'), style: 'font-size:13px' }, 'Benchmarks')),
              h('div.cs', { style: 'border-top:0;padding-top:0' }, h('dl.kv', Object.keys(bench).sort().map(k => [h('dt', settingLabel(k)), h('dd', settingValue(k, bench[k]))])))) : null)),
      ];
    }, ['status', 'conn', 'hello', 'tick']);
    load();
    return { unmount() { alive = false; offHello(); offStatus(); view.unmount(); } };
  },
});
