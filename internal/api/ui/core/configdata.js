// Shared helpers of the Configure screens (Profiles, Tiers and nodes, Settings)
// and Audit: loading the configuration and its schema, small value helpers,
// source badges and the plain-words names of audit sections.
//
// REST: GET /api/config, GET /api/config/schema (cached for the page's life).

import { h } from './dom.js';
import * as api from './api.js';
import { badge } from './ui.js';

let schemaPromise = null;
// loadSchema() -> the schema fields ({key, scope, section, type, ...}), fetched once.
export function loadSchema() {
  if (!schemaPromise) schemaPromise = api.get('/api/config/schema').then(r => r.fields || []).catch(e => { schemaPromise = null; throw e; });
  return schemaPromise;
}
export const loadConfig = () => api.get('/api/config');
export const fieldsOf = (fields, scope) => (fields || []).filter(f => f.scope === scope);

export const clone = v => (v === undefined ? undefined : JSON.parse(JSON.stringify(v)));
export const same = (a, b) => JSON.stringify(a ?? null) === JSON.stringify(b ?? null);

// Which field does a server message talk about? The server's messages name the
// key ("profile "x": ctx: must be positive", "settings: ttl must be at least 0").
export function fieldOfMessage(msg, keys) {
  const m = String(msg);
  const lead = m.match(/^(?:profile "[^"]*"|profile|settings|recovery|fleet|tier \d+|defaults):\s*([a-z_][a-z0-9_.]*)/i);
  if (lead && keys.includes(lead[1])) return lead[1];
  for (const k of keys.slice().sort((a, b) => b.length - a.length)) {
    if (new RegExp('(^|[^a-z_.])' + k.replace(/\./g, '\\.') + '([^a-z_]|$)', 'i').test(m)) return k;
  }
  return null;
}

// Settings of a section are read from the server as the effective values; an
// input shows durations the way the server writes them ("5m0s").
export const SOURCES = {
  ui: ['Web UI', 'use'],
  rest: ['REST', 'q'],
  mcp: ['MCP', 'load'],
  import: ['Import', 'settle'],
  system: ['System', 'n'],
};
export function sourceBadge(source) {
  const [label, kind] = SOURCES[source] || [source || 'Unknown', 'n'];
  return badge(label, kind, source === 'ui' ? 'Made in this web page' : source === 'rest' ? 'Made with a REST call' : source === 'mcp' ? 'Made by an agent through MCP'
    : source === 'import' ? 'Made by import-config or a configuration import' : source === 'system' ? 'Made by the scheduler itself, for example a benchmark run' : '');
}

export const SECTION_NAMES = {
  profiles: 'Profiles', tiers: 'Tiers', settings: 'Settings', exclude_nodes: 'Excluded nodes', node_overrides: 'Node overrides',
  recovery: 'Recovery', fleet: 'Fleet', bench: 'Benchmarks', benchmarks: 'Benchmarks', suites: 'Suites', runtime: 'Runtime', config: 'Configuration',
};
export const sectionName = s => SECTION_NAMES[s] || s || 'Unknown';

export const ACTION_NAMES = {
  create: 'Created', update: 'Changed', delete: 'Deleted', import: 'Imported', rollback: 'Rolled back', load: 'Loaded', unload: 'Unloaded',
  run: 'Started a run', cancel: 'Cancelled a run', run_start: 'Started a run of', run_cancel: 'Cancelled a run of',
};
export const actionName = a => ACTION_NAMES[a] || (a ? a.charAt(0).toUpperCase() + a.slice(1) : 'Changed');

// A one-line plain sentence for an audit entry.
export function entrySentence(e) {
  const t = e.target;
  const sec = e.section;
  if (sec === 'runtime') return t === '*' || !t ? `${actionName(e.action)} every model` : `${actionName(e.action)} ${t}`;
  if (sec === 'bench' || sec === 'benchmarks' || sec === 'suites') {
    // the server writes the target as "suite:<id>", "benchmark:<id>" or "run:<id>"
    const m = /^(suite|benchmark|run):(.*)$/.exec(t || '');
    const kind = m ? m[1] : sec === 'suites' ? 'suite' : 'benchmark';
    const id = m ? m[2] : t;
    if (kind === 'run') return `${e.action === 'cancel' ? 'Cancelled' : e.action === 'create' || e.action === 'run' ? 'Started' : actionName(e.action)} benchmark run ${id}`;
    return `${actionName(e.action)} ${kind} ${id}`;
  }
  if (sec === 'profiles') return `${actionName(e.action)} profile ${t}`;
  if (sec === 'tiers') return `${actionName(e.action)} the tier list`;
  if (sec === 'exclude_nodes') return `${actionName(e.action)} the excluded nodes`;
  if (sec === 'node_overrides') return `${actionName(e.action)} the override for node ${t}`;
  if (sec === 'settings') return `${actionName(e.action)} setting ${t}`;
  if (sec === 'recovery') return `${actionName(e.action)} recovery setting ${t}`;
  if (sec === 'fleet') return `${actionName(e.action)} fleet setting ${t}`;
  if (e.action === 'rollback' || e.action === 'import') return `${actionName(e.action)} the configuration`;
  return `${actionName(e.action)} ${sectionName(sec).toLowerCase()}${t ? ' ' + t : ''}`;
}

// A search box: input.input with a clear label, no form.
export function searchBox(value, onInput, placeholder, label) {
  const input = h('input.input', { type: 'search', value, placeholder, 'aria-label': label || placeholder, autocomplete: 'off', spellcheck: 'false', style: 'max-width:340px' });
  input.addEventListener('input', () => onInput(input.value));
  return input;
}

// The text of a node name list, "none" when empty.
export const joinNames = l => ((l || []).length ? l.join(', ') : 'none');
