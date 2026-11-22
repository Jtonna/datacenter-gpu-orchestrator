// External results: benchmark result sets produced outside the bench runner
// (a driver script, a hand-made table) and stored on the scheduler. The
// overview lists them; #/benchmarks/external/<id> shows one: its description,
// note, tables (sortable by column, units in the header, long text cut with
// the full text one click away) and its attachments, folded.
//
// Reads: REST GET /api/bench/external (summaries, no rows) and
// GET /api/bench/external/{id}. Read-only: sets are written through the API.

import { h } from '../../core/dom.js';
import * as api from '../../core/api.js';
import { href, navigate } from '../../core/router.js';
import { badge, table, empty, skeleton, stat, backLink, more } from '../../core/ui.js';
import { num, ago, dateTime, plural, bytes, DASH } from '../../core/format.js';
import { cache, loadProfiles } from './common.js';

export const extData = { list: null, sets: {}, errors: {} };
const ui = { sort: {}, open: new Set() }; // sort: table key -> {key, dir}; open: unfolded cells and attachments

export async function loadExternalList(redraw) {
  try { extData.list = (await api.get('/api/bench/external')).external || []; extData.errors.list = ''; } catch (e) { extData.errors.list = e.message; }
  redraw();
}
export async function loadExternalSet(id, redraw) {
  loadProfiles();
  try { extData.sets[id] = await api.get('/api/bench/external/' + encodeURIComponent(id)); extData.errors[id] = ''; } catch (e) { extData.errors[id] = e.message; }
  redraw();
}

const rowCount = x => (x.tables || []).reduce((n, t) => n + (t.row_count != null ? t.row_count : (t.rows || []).length), 0);

// ---------- overview card ----------

export function externalCard() {
  const list = extData.list;
  let body;
  if (extData.errors.list && !list) body = empty('The external results could not be read', extData.errors.list);
  else if (!list) body = skeleton(2);
  else if (!list.length) body = empty('No external results yet', 'Results measured outside the benchmark runner can be stored with POST /api/bench/external and are shown here. The benchmark guide says how.');
  else body = table({
    minWidth: 820, rows: list, key: x => x.id, onRow: x => navigate('benchmarks', ['external', x.id]),
    columns: [
      { label: 'Result set', render: x => h('span', h('a', { href: href('benchmarks', ['external', x.id]), style: 'font-weight:700', onclick: e => e.stopPropagation() }, x.title || x.id),
        x.description ? h('span.sub', x.description) : h('span.sub.mono', x.id)) },
      { label: 'Models', render: x => (x.models && x.models.length ? h('span', { title: x.models.join(', ') }, x.models.length > 3 ? x.models.slice(0, 3).join(', ') + ` and ${x.models.length - 3} more` : x.models.join(', ')) : h('span.mut', DASH)) },
      { label: 'Tables', cls: 'r', render: x => num((x.tables || []).length) },
      { label: 'Rows', cls: 'r', render: x => num(rowCount(x)) },
      { label: 'Stored', cls: 'tight', render: x => h('span', { title: dateTime(x.created_at) }, ago(x.created_at)) },
    ],
  });
  return h('section.card', { key: 'external' },
    h('div.ch', h('h2', 'External results'), h('span.note', 'Measured outside the benchmark runner and stored here. Open one for its tables.')),
    body);
}

// ---------- cells ----------

const CUT = 90; // characters of a text cell shown before it is cut

function digitsFor(v) {
  if (Number.isInteger(v)) return 0;
  const a = Math.abs(v);
  return a >= 100 ? 1 : a >= 1 ? 2 : 3;
}

function cell(v, cellKey, redraw) {
  if (v == null || v === '') return h('span.mut', DASH);
  if (typeof v === 'number') return num(v, digitsFor(v));
  const s = String(v);
  if (s.length <= CUT) return s;
  const open = ui.open.has(cellKey);
  return h('span', { style: 'white-space:normal;overflow-wrap:anywhere' },
    open ? s : h('span', { title: s }, s.slice(0, CUT).trimEnd() + '…'), ' ',
    more(open, () => { if (open) ui.open.delete(cellKey); else ui.open.add(cellKey); redraw(); }, 'Full text', 'Less'));
}

// Numbers before text, missing values last (the caller keeps them last whichever the direction).
function compare(a, b) {
  const na = a == null || a === '', nb = b == null || b === '';
  if (na || nb) return na && nb ? 0 : na ? 1 : -1;
  if (typeof a === 'number' && typeof b === 'number') return a - b;
  if (typeof a === 'number') return -1;
  if (typeof b === 'number') return 1;
  return String(a).localeCompare(String(b), undefined, { numeric: true });
}

function headLabel(c, tkey, redraw) {
  const s = ui.sort[tkey];
  const on = s && s.key === c.key;
  const text = (c.label || c.key) + (c.unit ? ` (${c.unit})` : '');
  const tip = [c.higher_is_better === true ? 'Higher is better.' : c.higher_is_better === false ? 'Lower is better.' : '', 'Click to sort.'].filter(Boolean).join(' ');
  return h('button', { type: 'button', title: tip, 'aria-label': `Sort by ${text}`, onclick: () => {
    ui.sort[tkey] = on && s.dir === 'asc' ? { key: c.key, dir: 'desc' } : on && s.dir === 'desc' ? null : { key: c.key, dir: 'asc' };
    redraw();
  } }, text, on ? (s.dir === 'asc' ? ' ▲' : ' ▼') : '');
}

function profileCell(p) {
  if (!p) return h('span.mut', DASH);
  const known = (cache.profiles || []).some(x => x.id === p);
  return known ? h('a.mono', { href: href('profiles', [p]), onclick: e => e.stopPropagation() }, p) : h('span.mono', { title: 'No profile of this name on this scheduler' }, p);
}

function tableCard(x, t, i, redraw) {
  const tkey = x.id + ':' + i;
  const cols = t.columns || [];
  const rows = (t.rows || []).slice();
  const idx = new Map(rows.map((r, ri) => [r, ri])); // a row keeps its place in the stored order for its key
  const s = ui.sort[tkey];
  if (s) {
    const blank = v => v == null || v === '';
    rows.sort((a, b) => (blank(a[s.key]) || blank(b[s.key]) ? compare(a[s.key], b[s.key]) : (s.dir === 'asc' ? 1 : -1) * compare(a[s.key], b[s.key])));
  }
  // a row that names a profile without a profile column gets one, so the UI can link it
  const hasProfile = rows.some(r => r.profile) && !cols.some(c => c.key === 'profile');
  const hasModel = rows.some(r => r.model) && !cols.some(c => c.key === 'model');
  const columns = [];
  if (hasModel) columns.push({ label: 'Model', render: r => r.model || h('span.mut', DASH) });
  for (const c of cols) {
    const numeric = rows.some(r => typeof r[c.key] === 'number');
    columns.push({ label: headLabel(c, tkey, redraw), cls: numeric ? 'r' : '', render: r => (c.key === 'profile' ? profileCell(r.profile) : cell(r[c.key], tkey + ':' + idx.get(r) + ':' + c.key, redraw)) });
  }
  if (hasProfile) columns.push({ label: 'Profile', cls: 'tight', render: r => profileCell(r.profile) });
  return h('section.card', { key: 'table-' + i },
    h('div.ch', h('h2', t.title), t.description ? h('span.note', t.description) : null, h('span.right.mut', plural(rows.length, 'row'))),
    table({ minWidth: Math.min(1600, 140 + 110 * columns.length), rows, key: r => idx.get(r), columns, empty: h('div.cb', h('span.mut', 'This table has no rows.')) }));
}

function attachmentsCard(x, redraw) {
  const list = x.attachments || [];
  if (!list.length) return null;
  return h('section.card', { key: 'attachments' },
    h('div.ch', h('h2', 'Attachments'), h('span.note', 'Texts stored with the results, such as task statements.')),
    h('div.cb', list.map(a => {
      const k = x.id + ':att:' + a.name, open = ui.open.has(k);
      return h('div', { key: k, style: 'padding:6px 0;border-top:1px solid var(--line)' },
        h('span.row', { style: 'gap:10px' }, h('b.mono', a.name), h('span.mut', bytes((a.text || '').length)),
          more(open, () => { if (open) ui.open.delete(k); else ui.open.add(k); redraw(); }, 'Show', 'Hide')),
        open ? h('pre.raw', a.text || '') : null);
    })));
}

// ---------- a set's own page ----------

export function externalPage(id, redraw) {
  const back = backLink('Benchmarks', href('benchmarks'));
  const x = extData.sets[id];
  if (!x) {
    const err = extData.errors[id];
    if (err) return [back, h('section.card', empty(/not found/i.test(err) ? `There is no external result set ${id}` : 'The result set could not be read', err))];
    return [back, h('section.card', skeleton(5))];
  }
  const head = h('div.page-head', { key: 'head' },
    h('h1', x.title || x.id, ' ', badge('External', 'n', 'Measured outside the benchmark runner')), h('span.sub', 'External results · ', h('span.mono', x.id)));
  const facts = h('section.card', { key: 'facts' },
    x.description ? h('div.cb', { style: 'padding-top:16px;padding-bottom:0' }, h('p', { style: 'color:var(--text-2);max-width:90ch' }, x.description)) : null,
    x.note ? h('div.cb', { style: 'padding-top:12px;padding-bottom:0' }, h('p', { style: 'max-width:90ch;white-space:pre-wrap' }, h('b', 'Note: '), x.note)) : null,
    h('div.stats', { style: 'padding-top:16px' },
      stat('Tables', num((x.tables || []).length)),
      stat('Rows', num(rowCount(x))),
      stat('Attachments', num((x.attachments || []).length)),
      stat('Stored', h('span', { title: dateTime(x.created_at) }, ago(x.created_at)), null, { small: true }),
      x.updated_at && x.updated_at !== x.created_at ? stat('Replaced', h('span', { title: dateTime(x.updated_at) }, ago(x.updated_at)), null, { small: true }) : null));
  return [back, head, facts, (x.tables || []).map((t, i) => tableCard(x, t, i, redraw)), attachmentsCard(x, redraw)];
}
