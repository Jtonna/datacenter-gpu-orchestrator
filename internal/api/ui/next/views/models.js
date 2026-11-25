// Models (the home screen of the new shell; design: board Models, "Models
// list and sidebar", phase 7: built from what the server feeds today). A
// model is a model FILE, not a profile (README rule 9).
//
// The page: what is loaded now (one card per instance), then the list as
// the board draws it: search, "Show" (all, loaded, fits on one, needs
// several, cannot run), "Sort: last used"; the sections Downloading, Pinned
// and All models ("N model files · X of Y GB used", from the catalog's disk
// block); one row per file (star, file name, size, "N profiles", last used:
// Now, 48 s ago or Never). A row opens the side panel (?model=<key>): size
// and last used, Benchmarks, what is loaded where, where it runs and what it
// needs, its facts, and its configurations (search, context, the memory
// each needs, a Now pill on a loaded one, Edit, Add profile), and the link
// to its full page (#/models/<key>, views/models/detail.js, unchanged).
//
// Drawn as on the board but not available yet, each with its reason on
// hover (p7-models-backend-STATUS.md, M1 to M6): Add a model. The groups
// Downloading and Pinned are drawn only while they have a row (the owner's
// note 1, 25 Nov).
// The trash (v65, models slice 3; views/models/trash.js): the footer row
// "Deleted models" with the disk's trash figures and the way into
// #/models/deleted, and the panel's Delete model (a dry run, the name typed,
// a comment); both unavailable on an older server. Pins (v61): the star, the
// Pinned group and the panel's Pin / Pinned are real (PUT / DELETE
// /api/models/pin/{key} with a required comment); unavailable before v61.
// From v60 (models slice 1): "added <date>" (added_at), "Benchmarks · N
// reports" (bench.reports), "Unload when idle" (ttl_s), the disk's host in
// the disk line's hover; an older server without them gets the earlier
// drawing (runs, no date, no column).
//
// Reads: the catalog (core/catalog.js; the live channel's "models" topic,
// REST GET /api/models as its fallback); GET /api/models every 30 s for its
// disk block (the live channel's copy has none); the WebSocket status for
// the instance cards. Read-only: models load when a request asks for one.
// Routes: #/models?q=&show=&sort=&model=<key>, #/models/<key> (page)

import { h } from '../core/dom.js';
import { registerView, href, navigate } from '../core/router.js';
import { state } from '../core/store.js';
import * as api from '../core/api.js';
import { liveView, legend, memoryRow, empty, skeleton, searchField, sidePanel, facts, badge, filterSelect, dialog, runStatus } from '../core/ui.js';
import { num, gb, tps, dur, ago, plural, agoText, dateTime, bytes, DASH } from '../core/format.js';
import { staleBanner, nodeName, nodeMemory, machines, shortReason, plainReason } from '../core/status.js';
import { loadConfig, loadSchema, fieldsOf, clone } from '../core/configdata.js';
import { makeEditor, saveProfile } from './profiles.js';
import { catalog, onCatalog, needCatalog, refreshCatalog } from '../core/catalog.js';
import { logsLink, profileQuery, padded } from '../core/logs.js';
import { tipText } from '../core/charts.js';
import { stateBadge, fitOf, cannotRun, runsOnCell } from './models/common.js';
import { mountDetail } from './models/detail.js';
import { splitProfiles, prodText, testText, prodNeeds, profName, needBasis, zonesFor } from './models/kinds.js';
import { trashFooter, hasTrash, deleteModel, deletedText, mountTrash } from './models/trash.js';

// the reasons of what is drawn but not built yet (p7-models-backend-STATUS.md)
export const NOT_YET = {
  add: 'Adding a model from this page is not built yet. Put the .gguf file in the models folder and it appears here.',
  pin: 'Pins are not available: this scheduler is older than v61 (models slice 2).',
  del: 'Deleting a model needs a scheduler of v65 or later (models slice 3): this one is older, or its models folder figures are not read yet.',
  trash: 'Deleted models need a scheduler of v65 or later (models slice 3): this one is older.',
};
// ---- pins (v61, models slice 2): PUT / DELETE /api/models/pin/{key} with a required comment ----
const STAR = 'M10 3l2.1 4.3 4.7.7-3.4 3.3.8 4.7L10 13.8 5.8 16l.8-4.7L3.2 8l4.7-.7z';
// pinsOn(models): the server feeds `pinned` (v61 and later)
const pinsOn = models => (models || []).some(m => typeof m.pinned === 'boolean');
let pinErr = '', pinRedraw = () => {};
// togglePin(m): the comment dialog the other writes use (its button stays off
// while the comment is empty), then the write; a refusal in the server's words
async function togglePin(m) {
  const on = !m.pinned, name = m.file || m.name;
  const r = await dialog({ title: (on ? 'Pin ' : 'Unpin ') + name + '?',
    body: h('p', on ? 'It is listed under Pinned at the top of the list. A pin changes nothing in where or when the model loads.' : 'It goes back to All models. Nothing else changes.'),
    comment: { label: 'Comment', required: true, placeholder: 'Why? It is kept in the audit log with the change.' },
    actions: [{ label: 'Cancel', value: undefined }, { label: on ? 'Pin' : 'Unpin', kind: 'pri', value: true }] });
  if (!r || !r.value || !r.comment) return;
  pinErr = '';
  try {
    await api.send(on ? 'PUT' : 'DELETE', '/api/models/pin/' + encodeURIComponent(m.key), { comment: r.comment }, { comment: r.comment });
    m.pinned = on; m.pinned_at = on ? Date.now() : null;
    refreshCatalog();
  } catch (e) { pinErr = (on ? 'Could not pin ' : 'Could not unpin ') + name + ': ' + e.message; }
  pinRedraw();
}
// starButton(m, models): the row's star; unavailable on an older server
const starButton = (m, models) => (pinsOn(models)
  ? h('button', { type: 'button', class: 'star' + (m.pinned ? ' on' : ''), 'aria-pressed': m.pinned ? 'true' : 'false', 'aria-label': (m.pinned ? 'Unpin ' : 'Pin ') + (m.file || m.name),
    title: m.pinned ? 'Pinned' + (m.pinned_at ? ' ' + agoText(m.pinned_at) : '') + ': click to unpin' : 'Pin: list it under Pinned', onclick: e => { e.stopPropagation(); togglePin(m); } }, icon(STAR))
  : off('button.star', NOT_YET.pin, { 'aria-label': 'Pin ' + (m.short_name || m.name) + ' (not available)', 'aria-pressed': 'false' }, icon(STAR)));

// off(tag, reason, attrs, ...kids) -> a control that is drawn but not available: it says why on hover and does nothing
const off = (tag, reason, attrs, ...kids) => h(tag, Object.assign({ type: 'button', 'aria-disabled': 'true', title: reason, 'data-off': '', onclick: e => { e.preventDefault(); e.stopPropagation(); } }, attrs), ...kids);
const icon = d => h('svg', { class: 'ic', viewBox: '0 0 20 20', 'aria-hidden': 'true' }, h('path', { d }));

// loadLogs(st, load) -> a link to the log lines of a load, or null while
// central logging is off (as on the classic page).
function loadLogs(st, l) {
  if (!l.finished_at) return null;
  const [from, to] = padded(l.finished_at - (l.load_s || 0) * 1000, l.finished_at);
  return logsLink(st, 'Logs', profileQuery(l.model), from, to, 'Open the log lines of this load in a new tab');
}

const FILTERS = [
  ['all', 'All', () => true],
  ['loaded', 'Loaded', m => m.state !== 'not_loaded'],
  ['one', 'Fits on one', (m, st) => fitOf(m, st).kind === 'one'],
  ['several', 'Needs several', (m, st) => fitOf(m, st).kind === 'several'],
  ['none', 'Cannot run', (m, st) => fitOf(m, st).kind === 'none' || !!cannotRun(m)],
];
const SORTS = [['used', 'last used'], ['name', 'name'], ['size', 'size']];
const usedKey = m => (m.state && m.state !== 'not_loaded' ? Infinity : m.last_active || -1);
const SORT = {
  used: (a, b) => usedKey(b) - usedKey(a) || (a.file || a.name).localeCompare(b.file || b.name),
  name: (a, b) => (a.file || a.name).localeCompare(b.file || b.name),
  size: (a, b) => (b.size_bytes || 0) - (a.size_bytes || 0) || (a.file || a.name).localeCompare(b.file || b.name),
};

// loadTip(mark, st) -> the tooltip of one mark of a "last loads" chart. Kept
// with the classic name (views/models.js exports it).
export function loadTip(l, st) {
  const when = `${agoText(l.at)} · ${dateTime(l.at)}`;
  if (l.outcome === 'refused') {
    return tipText(l.count > 1 ? `${num(l.count)} requests refused` : 'A request was refused', [when, l.profile, shortReason(l.reason) || null, 'Nothing was loaded']);
  }
  const head = l.outcome === 'ready' ? 'Loaded' : l.outcome === 'cancelled' ? 'Load cancelled' : 'Load failed';
  const figs = [l.load_s != null ? dur(l.load_s) : null, l.mb_s ? `${num(l.mb_s)} MB/s` : null].filter(Boolean).join(' · ');
  return tipText(head, [when, l.profile, l.nodes ? 'On ' + machines(st, l.nodes) : null, figs || null]);
}

// inUse(m) -> "in use on ps5-01, ps5-02" (or "loading on …") for a row
// whose model is loaded, each zone a link to Zones with that zone selected
// (the owner's note 3: the row says when and where, no banner); null when
// it is not loaded. The zones are the catalog instances' (names).
// zoneLinks(names) -> "ps5-01, ps5-02", each a link to the zone under Zones
// (a click on one does not open the row's panel)
const zoneLinks = list => list.flatMap((z, i) => [i ? ', ' : null, h('a.mdl-zone', { href: href('zones', [z]), title: 'Open ' + z + ' under Zones',
  onclick: e => e.stopPropagation(), onkeydown: e => e.stopPropagation() }, z)]);

function inUse(m) {
  const insts = m.instances || [];
  if (!insts.length) return null;
  const ready = [...new Set(insts.filter(i => i.state !== 'starting').flatMap(i => i.nodes || []))];
  const loading = [...new Set(insts.filter(i => i.state === 'starting').flatMap(i => i.nodes || []))].filter(z => !ready.includes(z));
  const zones = zoneLinks;
  const parts = [];
  if (ready.length) parts.push(['in use on ', ...zones(ready)]);
  else if (insts.some(i => i.state !== 'starting')) parts.push('in use');
  if (loading.length) parts.push(['loading on ', ...zones(loading)]);
  else if (!ready.length && insts.some(i => i.state === 'starting')) parts.push('loading');
  return h('span.mdl-inuse', { 'data-inuse': '' }, parts.flatMap((x, i) => [i ? ' · ' : null, x]));
}

// lastUsed(m) -> Now (loaded), Loading, "48 s ago" (exact time on hover) or Never
function lastUsed(m) {
  if (m.state === 'ready') return h('time.mdl-now', { title: m.last_active ? dateTime(m.last_active) : 'Loaded now' }, 'Now');
  if (m.state === 'loading') return h('span.mdl-now', 'Loading');
  return ago(m.last_active, m.history === false ? DASH : 'Never');
}
// dayOf(ms) -> "4 Nov 2026"
const dayOf = ms => { const d = new Date(ms); return `${d.getDate()} ${['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'][d.getMonth()]} ${d.getFullYear()}`; };
// prodCount(m) -> the model's production profiles (views/models/kinds.js;
// the test and benchmark configurations are never counted with them)
const prodCount = m => splitProfiles(m.profiles).prod.length;
// profCell(m) -> the row's Profiles cell: "2 profiles"; for a file with no
// production profile "No profiles yet", and smaller under it the real count
// of its test and benchmark configurations (nothing when there are none).
function profCell(m) {
  const { prod, test } = splitProfiles(m.profiles);
  if (prod.length) return h('span.mdl-pc', { 'data-prod': String(prod.length) }, prodText(prod.length));
  return h('span.mdl-pc.mut', { 'data-prod': '0' }, 'No profiles yet',
    test.length ? h('span.sub', { 'data-test': String(test.length) }, plural(test.length, 'test configuration')) : null);
}
// sizeText(m) -> "16.5 GB", or why there is none (decimal GB, as the board and the disk line)
const gbOf = v => (v / 1e9).toLocaleString('en-US', { maximumFractionDigits: 1, minimumFractionDigits: v >= 100e9 ? 0 : 1 });
const sizeText = m => (m.kind === 'command' ? 'A program on this machine' : m.size_bytes ? gbOf(m.size_bytes) + ' GB' : DASH);

// diskLine(models, disk) -> "5 model files · 50.3 of 240 GB used", amber at 90 %
function diskLine(models, disk) {
  const files = (models || []).filter(m => m.kind !== 'command' && m.exists !== false).length;
  const parts = [plural(files, 'model file')];
  let full = false, title = null;
  if (disk && disk.total_bytes) {
    const budget = disk.budget_bytes;
    const used = budget ? disk.model_bytes || 0 : disk.used_bytes;
    const of = budget || disk.total_bytes;
    parts.push(`${gbOf(used)} of ${gbOf(of)} GB used`);
    full = used / of >= 0.9;
    title = [budget ? `The model files take ${gbOf(used)} GB of their ${gbOf(budget)} GB budget (models_disk_budget_gb).`
      : `The disk of the models folder: ${gbOf(disk.used_bytes)} of ${gbOf(disk.total_bytes)} GB used.`,
    disk.free_bytes != null ? `${gbOf(disk.free_bytes)} GB free${disk.host ? ' on ' + disk.host : ''}.` : null,
    !budget && disk.model_bytes != null ? `The model files take ${gbOf(disk.model_bytes)} GB.` : null,
    disk.checked_at ? `Read ${agoText(disk.checked_at)}.` : null].filter(Boolean).join(' ');
  }
  return h('span', { class: 'lbl mdl-disk' + (full ? ' full' : ''), title, 'data-disk': '' }, parts.join(' · '), full ? ' · nearly full' : null);
}

// hostLine(p) -> "hosted on <node> · +1.5 GB for the host" for a profile whose llama-server runs on a node
const hostLine = p => (p.host === 'node'
  ? h('span.sub', { title: 'llama-server runs on this zone, started by its agent' }, 'hosted on ' + (p.host_node || 'its first forced zone'),
    p.host_overhead_mib ? ` · +${gb(p.host_overhead_mib)} for the host` : null, p.host_problem ? h('span.err-t', ' · ' + p.host_problem) : null)
  : null);

// needText(mib) -> "~18.5 GB": a tilde, as a need is approximate, and one
// decimal (the owner's note 7, 25 Nov)
export const needText = mib => '~' + (mib / 1024).toLocaleString('en-US', { minimumFractionDigits: 1, maximumFractionDigits: 1 }) + ' GB';
// needRange(r) -> "~3.3 to 5.1 GB", or one figure when the ends read the same
const needRange = r => (needText(r.lo.needs_mib) === needText(r.hi.needs_mib) ? needText(r.lo.needs_mib)
  : needText(r.lo.needs_mib).replace(/ GB$/, '') + ' to ' + needText(r.hi.needs_mib).slice(1));
// needEnd(p) -> "qwen3.5-4b at 16k ~3.3 GB (measured)"
const needEnd = p => `${profName(p)} ${needText(p.needs_mib)} (${needBasis(p)})`;
// needEnds(r) -> the profile at each end, or the one profile
const needEnds = r => (r.lo === r.hi ? needEnd(r.lo) : needEnd(r.lo) + ' … ' + needEnd(r.hi));
// noProd(m) -> true for a model file whose configurations are all tests
const noProd = m => m.kind !== 'command' && !prodCount(m) && (m.profiles || []).length > 0;

// needsCell(m, st) -> the Needs cell: the range over the model's production
// profiles' needs ("~3.3 to 5.1 GB", the reviewer, 25 Nov), the profile at
// each end on hover. A file with no production profile keeps the lightest
// figure (fit.need_mib), marked "smallest test configuration". How a figure
// is known is on hover and in the panel (the owner's note 7); a dash when
// unknown.
function needsCell(m, st) {
  const r = prodNeeds(m);
  if (r) {
    return h('span.mdl-needs', { 'data-col': 'needs' }, h('span', { title: (r.lo === r.hi ? 'From its production profile: ' : 'From its production profiles: ') + needEnds(r),
      'data-basis': r.basis, 'data-range': r.lo === r.hi ? null : '' }, needRange(r)));
  }
  const f = fitOf(m, st);
  let how = f.title || (f.sub ? f.sub[0].toUpperCase() + f.sub.slice(1) : null);
  if (noProd(m)) how = (how ? how + '. ' : '') + 'The smallest test configuration: no production profile yet';
  return h('span.mdl-needs', { 'data-col': 'needs' }, f.need ? h('span', { title: how, 'data-basis': f.sub || null }, needText(f.need)) : h('span.mut', DASH));
}

// runsOnRange(m, st, r) -> "1 to 2 PS5s": the fewest zones each end needs on
// the empty cluster (fit.budgets_mib); null when the sums are not served
function runsOnRange(m, st, r) {
  const sums = m.fit && m.fit.budgets_mib;
  const a = zonesFor(r.lo.needs_mib, sums), b = zonesFor(r.hi.needs_mib, sums);
  if (a == null) return null;
  const t = 'The fewest zones of the empty cluster that hold ' + needEnds(r);
  if (a === 0) return h('span', { title: t, 'data-zones': '0' }, 'Too big for this cluster');
  if (b === 0) return h('span', { title: t, 'data-zones': a + '-' }, machines(st, a), h('span.mut', ' (the largest is too big for this cluster)'));
  return h('span.nowrap', { title: t, 'data-zones': a === b ? String(a) : a + '-' + b }, a === b ? machines(st, a) : a + ' to ' + machines(st, b));
}

// deleteButton(m, disk, onDeleted) -> the panel's "Delete model": the delete dialog
// (views/models/trash.js), or unavailable with the reason
function deleteButton(m, disk, onDeleted) {
  if (!hasTrash(disk)) return off('button.btn.red', NOT_YET.del, { 'data-act': 'delete' }, 'Delete model');
  if (m.kind === 'command') return off('button.btn.red', 'A command profile has no model file to delete.', { 'data-act': 'delete' }, 'Delete model');
  if (m.exists === false) return off('button.btn.red', 'The file is not in the models folder: there is nothing to delete.', { 'data-act': 'delete' }, 'Delete model');
  return h('button.btn.red', { type: 'button', 'data-act': 'delete', onclick: () => deleteModel(m, onDeleted, disk ? disk.models_writable : undefined) }, 'Delete model');
}

// needsLine(m, st, f) -> the panel's "Runs on: 1 to 2 PS5s · needs ~3.3 to
// 5.1 GB" and under it the profile at each end, measured or estimated each;
// with no production profile, the lightest figure marked "smallest test
// configuration"
function needsLine(m, st, f) {
  const r = prodNeeds(m);
  if (r) {
    return h('div', { 'data-needs': '' }, h('span.mut', 'Runs on: '), runsOnRange(m, st, r) || runsOnCell(m, st),
      h('span.mut', ' · needs '), h('span', needRange(r)), h('span.mut.mdl-need-ends', needEnds(r)));
  }
  const tail = [f.sub, noProd(m) ? 'smallest test configuration' : ''].filter(Boolean).join(' · ');
  return h('div', { 'data-needs': '' }, h('span.mut', 'Runs on: '), runsOnCell(m, st),
    f.need ? [h('span.mut', ' · needs '), h('span', { title: f.title }, needText(f.need)), tail ? h('span.mut', ' (' + tail + ')') : null] : null);
}

// the side panel of one model
function modelPanel(st, m, ps) {
  const f = fitOf(m, st);
  const l = m.loads || {};
  const insts = m.instances || [];
  const loadedProfiles = new Set(insts.filter(i => i.state !== 'starting').map(i => i.profile));
  const all = m.profiles || [];
  // the production profiles are the model's profiles; the rest is one collapsed line (views/models/kinds.js)
  const { prod: hand, test: tests } = splitProfiles(all);
  const ctxs = [...new Set(hand.map(p => p.ctx).filter(Boolean))].sort((x, y) => x - y);
  const q = ps.q.trim().toLowerCase();
  // "Unload when idle" from ttl_s (v60; 0 = never); no column when the server has no ttl_s
  const ttlCol = hand.some(p => typeof p.ttl_s === 'number');
  const rows = hand.filter(p => (!q || p.id.toLowerCase().includes(q) || (p.description || '').toLowerCase().includes(q)) && (!ps.ctx || String(p.ctx) === ps.ctx));
  if (ps.view === 'bench') return benchView(m, ps);
  if (ps.view === 'edit') return editView(m, ps);
  return [
    ps.saved ? h('div.banner.info', { key: 'saved', role: 'status', 'data-saved': '' }, h('span.grow', ps.saved)) : null,
    h('div.mdl-acts', { key: 'acts' },
      // the count opens the model's runs in this panel (the owner's note 8a); it
      // is the count of that list, from the same query (runsOf), so the two
      // never differ; "report" is said per row only (the reviewer, 25 Nov)
      benchButton(m, ps),
      typeof m.pinned === 'boolean'
        ? h('button', { type: 'button', class: 'btn' + (m.pinned ? ' mdl-pinned' : ''), 'aria-pressed': m.pinned ? 'true' : 'false', 'data-act': 'pin',
          title: m.pinned ? 'Pinned: click to unpin' : 'Pin: list it under Pinned', onclick: () => togglePin(m) }, m.pinned ? [icon(STAR), ' Pinned'] : 'Pin')
        : off('button.btn', NOT_YET.pin, { 'aria-pressed': 'false', 'data-act': 'pin' }, 'Pinned'),
      deleteButton(m, ps.disk(), ps.onDeleted)),
    loadedBlock(st, insts),
    h('section.zpart', { key: 'runs' },
      h('div.row', { style: 'gap:10px;flex-wrap:wrap' }, h('b', m.short_name || m.name), m.publisher ? h('span.mut', m.publisher) : null,
        stateBadge(m, st), m.kind === 'llama' && !m.exists ? badge('File missing', 'fail') : null),
      needsLine(m, st, f),
      facts([
        m.family ? ['Family', h('span', m.family)] : null,
        m.parameters ? ['Parameters', h('span', m.parameters)] : null,
        m.quant ? ['Quantization', h('span', m.quant)] : null,
        m.context_length ? ['Trained context', h('span', num(m.context_length)), 'The context the model was trained with, from its file'] : null,
        m.requests_served != null ? ['Requests served', h('span', num(m.requests_served))] : null,
        l.count ? ['Loads', h('span', num(l.count), l.failed ? h('span.err-t', ` · ${num(l.failed)} failed`) : null)] : null,
      ].filter(Boolean))),
    h('section.mdl-cfg', { key: 'cfg' },
      h('div.dh', h('h3', 'Configurations ', h('span.mut', prodText(hand.length))),
        h('div.mdl-cfg-tools',
          hand.length > 1 ? searchField(ps.q, v => { ps.q = v; ps.redraw(); }, 'Search profiles', 'mdl-psearch') : null,
          ctxs.length > 1 ? filterSelect('Context', [{ id: '', label: 'any' }].concat(ctxs.map(c => ({ id: String(c), label: num(c) }))), ps.ctx, v => { ps.ctx = v; ps.redraw(); }) : null,
          h('a.btn.sm', { href: href('profiles', ['new']), title: 'Open the profile editor' }, 'Add profile'))),
      hand.length ? (rows.length ? h('div.scroll.mdl-pscroll', h('table.tbl.mdl-ptbl',
        h('thead', h('tr', h('th', { scope: 'col' }, 'Profile'), h('th.r', { scope: 'col' }, 'Context'),
          h('th.r', { scope: 'col', title: 'What a load of this profile needs: measured, or estimated from the file' }, 'Memory needed'),
          ttlCol ? h('th.r', { scope: 'col', title: 'How long the model stays loaded with no request (the ttl of the profile, else the default)' }, 'Unload when idle') : null,
          h('th', { scope: 'col' }, h('span.sr', 'Edit')))),
        h('tbody', rows.map(profileRow))))
        : h('p.mut.mdl-pad', 'No profile matches.'))
        : h('p.mut.mdl-pad', all.length ? 'No profile: only test and benchmark configurations.' : 'No configuration uses this file yet. Add a profile to load it.'),
      // the test and benchmark configurations: one collapsed line, never counted as profiles
      tests.length ? h('details.mdl-tests', { key: 'tests' }, h('summary', testText(tests.length)),
        h('table.tbl.mdl-ttbl', h('tbody', tests.map(profileRow)))) : null),
    h('div.mdl-pad.row', { key: 'page', style: 'gap:12px' }, h('a.btn', { href: href('models', [m.key]) }, 'Open the model page')),
  ];
  // one profile's row (production table and the test list)
  function profileRow(p) {
    return h('tr', { key: p.id, 'data-profile': p.id },
          h('td', h('span.mono', p.id), loadedProfiles.has(p.id) ? h('span.mdl-pill', { title: 'Loaded now' }, 'Now') : null, p.description ? h('span.sub', p.description) : null,
            p.parallel > 1 ? h('span.sub', `${num(p.parallel)} requests in parallel`) : null, hostLine(p)),
          h('td.r.mono', p.ctx ? num(p.ctx) : DASH),
          h('td.r', p.needs_mib ? [h('span.mono', num(p.needs_mib) + ' MiB'), h('span.sub', { title: p.needs_basis || null }, p.needs_measured ? 'measured' : 'estimated')] : h('span.mut', DASH)),
          ttlCol ? (typeof p.ttl_s === 'number' ? h('td.r.mono', { 'data-ttl': String(p.ttl_s) }, p.ttl_s > 0 ? dur(p.ttl_s) : 'Never') : h('td.r.mut', DASH)) : null,
          h('td.r', h('button.go', { type: 'button', 'data-act': 'edit', 'aria-label': 'Edit ' + p.id, title: 'Edit it here', onclick: () => ps.edit(p.id) }, h('span.tx', 'Edit'), icon('M8 5l5 5-5 5'))));
  }
}

// backBar(m, ps) -> the way back to the panel's main view
const backBar = (m, ps) => h('div.mdl-back', { key: 'back' }, h('button.btn.sm', { type: 'button', 'data-back': '', onclick: () => ps.show('') },
  icon('M12 5l-5 5 5 5'), 'Back to ' + (m.file || m.name)));

// planWords(plan) -> "would load on 1 zone", "loaded now on 2 zones", or why
// it would not load (a POST /api/plan answer, or a catalog profile's plan)
function planWords(r) {
  if (!r) return 'is not known';
  const n = (r.nodes || []).length;
  if (r.action === 'serve' || r.action === 'wait') return `is loaded now on ${plural(n, 'zone')}`;
  if (r.action === 'reject' || (r.memory && r.memory.fits === false)) return 'does not fit: ' + (plainReason(r.reason) || 'no zones can hold it');
  if (r.action === 'queue') return 'would wait: ' + (plainReason(r.reason) || 'no zone can take it now');
  if (r.action === 'backoff') return 'is held back: ' + (plainReason(r.reason) || 'its last load failed');
  if (r.would_start === false) return 'would not load now' + (r.reason ? ': ' + plainReason(r.reason) : '');
  return n ? `would load on ${plural(n, 'zone')}` + (r.action === 'evict' ? ', unloading another model first' : '') : 'is not known';
}

// benchView(m, ps): the model's benchmark runs and reports, in the panel
// (GET /api/bench/runs?model=<file>; the owner's note 8a); each opens its
// report page
// loadedBlock(st, insts) -> the panel's "Loaded now" block: per instance of
// the model its profile, state and zones (links), then what the instance
// card above the list used to show (the reviewer, 25 Nov): requests in
// flight and waiting, served, the last request's speed, its memory, and the
// memory per zone from the load log; while it loads, how far and how long.
// The catalog names the zones; the live status (matched by profile) has the
// figures. Nothing when the model is not loaded.
function loadedBlock(st, insts) {
  if (!insts.length) return null;
  const live = p => (st.instances || []).find(x => x.profile === p) || null;
  const anyMem = insts.some(i => { const x = live(i.profile); return x && (x.nodes || []).some(n => nodeMemory(n).total_mib > 0); });
  const allLoading = insts.every(i => i.state === 'starting');
  return h('section.zpart', { key: 'loaded', 'data-loaded': '' },
    h('div.mdl-loaded-h', h('h3', allLoading ? 'Loading now' : 'Loaded now'),
      anyMem ? h('div.mdl-key', legend([['m', 'Model'], ['k', 'KV cache'], ['c', 'Compute']])) : null),
    insts.map(i => {
      const x = live(i.profile) || {};
      const loading = i.state === 'starting' || x.state === 'starting';
      const names = (i.nodes || []).length ? i.nodes : (x.nodes || []).map(n => nodeName(st, n.id));
      const lr = x.last_request;
      const facts = [];
      if (loading) {
        const load = x.load || {};
        const frac = load.progress != null ? load.progress : x.load_progress;
        const eta = load.eta_s;
        if (frac != null) facts.push(Math.round(frac * 100) + '% loaded');
        if (eta != null) facts.push('about ' + dur(eta < 90 ? Math.round(eta) : Math.round(eta / 60) * 60) + ' left');
        if (x.waiters > 0) facts.push(plural(x.waiters, 'request') + ' waiting for it');
      } else {
        const inFlight = x.in_flight != null ? x.in_flight : i.in_flight || 0;
        facts.push(inFlight ? plural(inFlight, 'request') + ' in flight' : 'idle');
        if (x.waiters > 0) facts.push(num(x.waiters) + ' waiting');
        const served = x.requests_served != null ? x.requests_served : i.requests_served;
        if (served != null) facts.push(num(served) + ' served');
        if (lr && lr.tokens_per_second > 0) facts.push(h('span', { title: 'The last finished request: ' + num(lr.output_tokens) + ' tokens out in ' + dur((lr.duration_ms || 0) / 1000) }, tps(lr.tokens_per_second) + ' tok/s'));
        const memMib = x.device_total_mib || i.memory_mib;
        if (memMib) facts.push(h('span', { title: 'On the zones, from the load log' }, gb(memMib)));
      }
      return h('div.mdl-inst', { key: 'i-' + i.profile, 'data-profile': i.profile, 'data-state': loading ? 'starting' : 'ready' },
        h('div.mdl-inst-h', h('span.mono', i.profile), loading ? badge('Loading', 'load') : badge('Ready', 'ready'),
          names.length ? h('span.mut', 'on ', zoneLinks(names)) : null),
        facts.length ? h('div.mut.mdl-inst-f', facts.flatMap((v, k) => [k ? ' · ' : null, v])) : null,
        loading ? null : (x.nodes || []).filter(n => nodeMemory(n).total_mib > 0).map(n => memoryRow(nodeName(st, n.id), nodeMemory(n), n.budget_mib)));
    }));
}

// RUNS_MAX: the most runs the panel lists; a longer history says "200+".
const RUNS_MAX = 200;
// runsOf(m, ps) -> {file, list, error}: the model's benchmark runs (GET
// /api/bench/runs?model=<file>), asked once per panel. The Benchmarks button
// and the list it opens both read this one answer.
function runsOf(m, ps) {
  const file = m.file || m.key;
  if (!ps.runs || ps.runs.file !== file) {
    const r = ps.runs = { file, list: null, error: '' };
    api.get('/api/bench/runs?model=' + encodeURIComponent(file) + '&limit=' + RUNS_MAX)
      .then(d => { r.list = (d && d.runs) || []; if (ps.runs === r) ps.redraw(); }, e => { r.error = e.message; if (ps.runs === r) ps.redraw(); });
  }
  return ps.runs;
}
// runsCount(list) -> "2 runs", "No runs", "200+ runs"
const runsCount = list => (!list.length ? 'no runs' : list.length >= RUNS_MAX ? num(RUNS_MAX) + '+ runs' : plural(list.length, 'run'));

// benchButton(m, ps) -> "Benchmarks · 2 runs": opens the list in the panel
function benchButton(m, ps) {
  const list = runsOf(m, ps).list;
  return h('button.btn', { type: 'button', 'data-act': 'bench', 'data-runs': list ? String(list.length) : null, onclick: () => ps.show('bench'),
    title: !list ? 'The benchmark runs that used this model. Opens the list here.'
      : list.length ? 'The benchmark runs with a step on this model. Opens the list here.' : 'No benchmark run has used this model yet' },
  // one flex child: the count sits a space from the word, not the button's gap
  h('span', 'Benchmarks', list ? h('span.mut', ' · ' + runsCount(list)) : null));
}

function benchView(m, ps) {
  const r = runsOf(m, ps), list = r.list;
  const when = x => x.started_at || x.queued_at;
  return [
    backBar(m, ps),
    h('section.zpart', { key: 'bench', 'data-runs': list ? String(list.length) : null },
      h('h3', 'Benchmarks', list ? h('span.mut', ' · ' + runsCount(list)) : null),
      r.error ? h('p.err-t', 'The runs could not be read: ' + r.error)
        : !list ? skeleton(3)
          : !list.length ? h('p.mut', 'No benchmark run has used this model yet.')
            : h('div.mdl-runs', { role: 'list' }, list.map(x => {
              const sm = x.summary || {};
              const name = x.suite_name || x.benchmark_id || x.suite_id || x.id;
              const icon_ = x.state === 'completed' ? (sm.failed ? 'failed' : 'done') : x.state === 'paused' ? 'running' : x.state;
              return h('a.mdl-run', { key: x.id, role: 'listitem', href: href('benchmarks', ['runs', x.id]), title: 'Open the report of ' + x.id },
                h('span.mdl-run-n', h('b', name), h('span.mut.mono', x.id)),
                h('span.mdl-run-s', runStatus(icon_, x.state === 'completed' ? 'report' : x.state),
                  sm.steps ? h('span.mut', ` · ${num(sm.passed || 0)} of ${num(sm.steps)} passed`) : null),
                h('span.mut.mdl-run-t', when(x) ? h('time', { title: dateTime(when(x)) }, dayOf(when(x))) : DASH));
            })),
      h('p.mdl-more', h('a', { href: href('benchmarks') }, 'All benchmarks'))),
  ];
}

// editView(m, ps): one configuration edited in the panel (the owner's note
// 8b) with the profile editor of views/profiles.js (makeEditor: the same
// fields and checks), saved through the same review (saveProfile: a dry
// run, the diff, a required comment, If-Match; the server writes the audit
// entry). POST /api/plan reads saved profiles only: the preview gives the
// saved values' plan before, and the new plan after saving (status file,
// "Backend wished for": a what-if plan of edited values).
function editView(m, ps) {
  const id = ps.editId;
  const cur = (m.profiles || []).find(p => p.id === id) || null;
  if (!ps.editor || ps.editor.id !== id) {
    const ed = ps.editor = { id, el: null, error: '', cfg: null, preview: h('div.mdl-plan', { 'data-plan-preview': '' }) };
    Promise.all([loadConfig(), loadSchema()]).then(([cfg, schema]) => {
      if (ps.editor !== ed) return;
      const spec = cfg && cfg.profiles ? cfg.profiles[id] : null;
      if (!spec) { ed.error = 'There is no profile ' + id + ' in the configuration: it may have been deleted.'; ps.redraw(); return; }
      ed.cfg = cfg;
      ed.el = makeEditor({ fields: fieldsOf(schema, 'profile'), mode: 'edit', id, initial: clone(spec), inPanel: true, exists: () => true,
        onCancel: () => ps.show(''), above: ed.preview,
        onSave: async (pid, prof, setMessages) => {
          const r = await saveProfile({ pid, prof, exists: true, getRevision: () => (ed.cfg ? ed.cfg.revision : 0),
            reload: async () => { ed.cfg = await loadConfig(); }, onInvalid: setMessages });
          if (!(r && r.saved)) return;
          let words;
          try { words = planWords(await api.post('/api/plan', { model: pid })); } catch (er) { words = 'has a plan that could not be read (' + er.message + ')'; }
          ps.show('');
          ps.saved = `Saved ${pid}${r.revision ? ` (revision ${num(r.revision)})` : ''}. With the new values it ${words}.`;
          refreshCatalog();
          ps.redraw();
        } }).el;
      ps.redraw();
    }, er => { if (ps.editor === ed) { ed.error = er.message; ps.redraw(); } });
  }
  const ed = ps.editor;
  const plan = cur && cur.plan;
  ed.preview.replaceChildren(
    h('b', 'Where it would load: '),
    plan ? h('span', { 'data-would': String((plan.nodes || []).length) }, 'with the saved values it ', planWords(plan), '.') : h('span.mut', 'not known for the saved values.'),
    h('span.sub', 'The scheduler cannot show unsaved values yet: the plan for your changes is shown when you save.'));
  return [
    backBar(m, ps),
    h('section.zpart', { key: 'edit-' + id }, h('h3', 'Edit ', h('span.mono', id)),
      ed.error ? h('p.err-t', ed.error) : ed.el || skeleton(6)),
  ];
}

function listPanel(st, models, disk, error, r, query, setQuery, onFind, done) {
  const filter = FILTERS.some(f => f[0] === r.query.show) ? r.query.show : 'all';
  const sort = SORT[r.query.sort] ? r.query.sort : 'used';
  const counts = {};
  for (const [id, , test] of FILTERS) counts[id] = (models || []).filter(m => test(m, st)).length;
  const q = (query || '').trim().toLowerCase();
  const test = (FILTERS.find(f => f[0] === filter) || FILTERS[0])[2];
  const rows = (models || []).filter(m => test(m, st) && (!q || [m.name, m.raw_name, m.file, m.short_name, m.family, m.publisher, m.quant, m.parameters]
    .concat((m.profiles || []).map(p => p.id)).some(x => String(x || '').toLowerCase().includes(q)))).sort(SORT[sort]);
  const openKey = r.query.model || '';
  const open = key => setQuery({ model: key });
  let body;
  const pins = pinsOn(models);
  const rowOf = m => h('div', {
    key: m.key, role: 'listitem', class: 'mr' + (openKey === m.key ? ' sel' : ''), 'aria-current': openKey === m.key ? 'true' : null, 'data-model': m.key, tabindex: '0',
    onclick: e => { if (!e.target.closest('[data-off]') && !e.target.closest('a[href]')) open(m.key); },
    onkeydown: e => { if ((e.key === 'Enter' || e.key === ' ') && !e.target.closest('[data-off]') && !e.target.closest('button') && !e.target.closest('a[href]')) { e.preventDefault(); open(m.key); } } },
  starButton(m, models),
  h('span.mdl-name', h('span.mono.nm', { title: m.short_name && m.short_name !== m.file ? m.short_name : null }, m.file || m.name),
    h('span.mut', sizeText(m), m.kind !== 'command' && m.exists === false ? h('span.err-t', ' · the file is missing') : null),
    inUse(m)),
  needsCell(m, st),
  profCell(m),
  h('span.mut.mdl-used', lastUsed(m)));
  const pinnedRows = pins ? rows.filter(m => m.pinned) : [];
  const restRows = pins ? rows.filter(m => !m.pinned) : rows;
  const heads = () => h('div.mr.mr-h', { 'aria-hidden': 'true' }, h('span'), h('span'),
    h('span.lbl.mdl-needs', { title: 'The memory its production profiles need on the zones, smallest to largest' }, 'Needs'), h('span.lbl.mdl-pc', 'Profiles'), h('span.lbl', 'Last used'));
  if (error && !models) body = empty('The model list could not be read', error);
  else if (!models) body = skeleton(6);
  else if (!models.length) body = empty('No models yet', 'A model appears here once a .gguf file is in the models folder, or a profile names a model file.');
  else if (!rows.length) body = empty('No model matches', q ? `Nothing matches "${query}". Search looks at the file name, the name and the profile names.` : 'No model is in this group right now.');
  else body = restRows.length ? h('div.mdl-rows', { role: 'list', 'aria-label': 'All models' }, restRows.map(rowOf)) : h('p.mut.mdl-pad', 'Every model here is pinned.');
  const sec = (label, right, attrs) => h('div.sech', attrs || null, h('span.lbl', label), right || null);
  return h('section.panel.mdl', { key: 'catalog', 'aria-label': 'Models' },
    h('div.mdl-bar',
      searchField(query, onFind, 'Search models', 'model-search', { size: 'lg' }),
      models && models.length ? filterSelect('Show', FILTERS.map(([id, label]) => ({ id, label: label + (id === 'all' ? '' : ` (${counts[id]})`) })),
        filter, id => setQuery({ show: id === 'all' ? '' : id })) : null,
      filterSelect('Sort', SORTS.map(([id, label]) => ({ id, label })), sort, id => setQuery({ sort: id === 'used' ? '' : id })),
      off('button.btn.pri', NOT_YET.add, { 'data-act': 'add' }, icon('M10 4v12M4 10h12'), 'Add a model')),
    // Downloading: drawn only while something downloads (the owner's note 1);
    // no download is fed today (p7-models-backend-STATUS.md M2), so never yet
    pinErr ? h('div.banner.err', { role: 'alert', 'data-pin-err': '' }, h('span.grow', pinErr)) : null,
    done ? h('div.banner.info', { role: 'status', 'data-trash-done': '' }, h('span.grow', done), h('a', { href: href('models', ['deleted']) }, 'View deleted models')) : null,
    // Pinned: only while a model in the list is pinned (the owner's note 1)
    pinnedRows.length ? [sec('Pinned', null, { 'data-sec': 'pinned' }), heads(),
      h('div.mdl-rows', { role: 'list', 'aria-label': 'Pinned models', 'data-pinned-rows': '' }, pinnedRows.map(rowOf))] : null,
    sec('All models', models ? diskLine(models, disk) : null, { 'data-sec': 'all' }),
    restRows.length ? heads() : null,
    body,
    trashFooter(disk, NOT_YET.trash));
}

function mountHome(el, route) {
  let r = route, query = r.query.q || '', side = null, disk = null, alive = true, done = '';
  const offCatalog = onCatalog(() => { readDisk(); view.refresh(); if (side) side.redraw(); });
  pinRedraw = () => { if (!alive) return; view.refresh(); if (side) side.redraw(); };
  const setQuery = q => navigate('models', [], Object.assign({}, r.query, q), true);

  // the disk block: only the REST answer carries it; its figures move every
  // 30 s, so it is read again on a live message (status, catalog) at most
  // every 30 s, never on a timer of its own
  let diskAt = 0;
  const readDisk = () => {
    if (!alive || Date.now() - diskAt < 30000) return;
    diskAt = Date.now();
    api.get('/api/models').then(d => { disk = (d && d.disk) || null; if (alive) view.refresh(); }, () => { diskAt = 0; });
  };
  readDisk();

  // The side panel of the open model (?model=<key>).
  const syncSide = () => {
    const key = r.query.model || '';
    if (!key) { if (side) { const s = side; side = null; s.close(); } return; }
    if (side && side.key === key) { side.redraw(); return; }
    if (side) { const s = side; side = null; s.close(); }
    const find = () => (catalog.models || []).find(m => m.key === key) || null;
    const ps = { q: '', ctx: '', redraw: () => p.redraw(), disk: () => disk,
      // the panel's views: '' (main), 'bench' (its runs), 'edit' (a configuration in the editor)
      view: '', editId: '', editor: null, runs: null, saved: '',
      show: v => { ps.view = v; if (v !== 'edit') ps.editor = null; if (v === 'bench') ps.runs = null; if (v) ps.saved = ''; p.redraw(); },
      edit: id => { ps.editId = id; ps.saved = ''; ps.show('edit'); },
      // after a delete: the sentence on the list, the panel closed, the disk figures read again
      onDeleted: res => { done = deletedText(res); diskAt = 0; readDisk(); setQuery({ model: '' }); view.refresh(); } };
    const body = () => {
      const m = find();
      if (m) return modelPanel(state.status || {}, m, ps);
      return catalog.models ? h('section.card', empty('This model is not in the catalog', 'Its file may have gone. Close this panel to see the models there are.')) : skeleton(3);
    };
    const p = sidePanel({
      // docked beside the page it closes the section menu, as the board draws it (no menu beside the panel)
      closesMenu: true,
      label: 'Selected model', name: 'Selected model: ' + key,
      title: () => { const m = find(); return h('span.mono', m ? m.file || m.name : key); },
      sub: () => {
        const m = find();
        if (!m) return null;
        return [sizeText(m), ' · ', m.added_at ? [h('span', { title: 'The file arrived ' + dateTime(m.added_at) }, 'added ' + dayOf(m.added_at)), ' · '] : null, m.state === 'ready' ? 'loaded now' : m.state === 'loading' ? 'loading now' : m.last_active ? ['last used ', ago(m.last_active)] : 'never used'];
      },
      body, onClose: () => { if (side === p) { side = null; setQuery({ model: '' }); } },
    });
    p.key = key;
    side = p;
  };

  const render = () => {
    const st = state.status;
    const models = catalog.models;
    const view_ = st || {};
    const failed = (view_.recent_loads || []).filter(l => l.outcome && l.outcome !== 'ready');
    readDisk();
    if (side) side.redraw();
    return [
      // no header line: the page shows its content only (the owner's note 4,
      // 25 Nov); its name stays for a screen reader. Nothing about a loaded
      // model sits above the list: its row says where it is in use, its
      // panel's Loaded now block has the rest (the reviewer, 25 Nov).
      h('h1.sr', 'Models'),
      staleBanner(),
      view_.configured === false ? h('div.banner.info', { role: 'status' },
        h('span.grow', h('b', 'No configuration has been imported yet. '), 'The scheduler runs with its defaults and has no profiles and no tiers, so it cannot place a model. Import a configuration file with ',
          h('span.mono', 'llm-scheduler import-config'), ' or add tiers and profiles under Configure.'),
        h('a.btn.sm', { href: href('profiles') }, 'Open Profiles')) : null,
      failed.map(l => h('div.banner.err', { key: 'fail-' + l.session_id, role: 'status' },
        h('span.grow', h('span.mono', l.model), l.outcome === 'cancelled' ? ' stopped loading ' : ' failed to load ', ago(l.finished_at),
          l.load_s ? ` after ${dur(l.load_s)}.` : '.'),
        h('a', { href: href('activity', null, { session: l.session_id }) }, 'See what happened'),
        loadLogs(st, l))),
      view_.configured === false && models && !models.length ? null
        : listPanel(view_, models, disk, catalog.error, r, query, setQuery, v => { query = v; view.refresh(); }, done),
    ];
  };
  const view = liveView(el, render, ['status', 'tick', 'conn', 'hello']);
  needCatalog();
  syncSide();
  return {
    update(nr) { r = nr; syncSide(); view.refresh(); },
    unmount() { alive = false; offCatalog(); if (side) { const s = side; side = null; s.close(); } view.unmount(); },
  };
}

registerView({
  id: 'models', title: 'Models',
  mount(el, route) {
    // one view, two pages: the home (with its side panel), and a model's own page under #/models/<key>
    // #/models/deleted is the Deleted models view (no model key is "deleted": a key is a .gguf file or command:<id>)
    const pageOf = (k, rt) => (k === 'deleted' ? mountTrash(el, rt) : k ? mountDetail(el, k, rt) : mountHome(el, rt));
    let key = route.rest.join('/');
    let page = pageOf(key, route);
    return {
      update(r) {
        const k = r.rest.join('/');
        if (k === key) { if (page.update) page.update(r); return; }
        page.unmount();
        el.replaceChildren();
        window.scrollTo(0, 0);
        key = k;
        page = pageOf(key, r);
      },
      unmount() { page.unmount(); },
    };
  },
});
