// node --test scripts/gate/ : the drawing harness's rules, without a browser.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { parseArgs, writeVerdict, consoleCounts, apiRoutes, RouteQueue, parseRoute, shapeOf, fileName, squeezeVerdict, list } from './pages-lib.mjs';

test('arguments: defaults, flags and refusals', () => {
  const o = parseArgs(['--base', 'http://192.0.2.10:8080/']);
  assert.equal(o.base, 'http://192.0.2.10:8080');
  assert.deepEqual(o.widths, [1536, 1280]);
  assert.equal(o.minChars, 12);
  const p = parseArgs(['--base', 'http://192.0.2.10:8080', '--widths', '1440,390', '--allow-post', '/api/plan', '--strict', '--dry-run']);
  assert.deepEqual(p.widths, [1440, 390]);
  assert.deepEqual(p.allowPost, ['/api/plan']);
  assert.ok(p.strict && p.dryRun);
  assert.ok(parseArgs(['--help']).help);
  assert.throws(() => parseArgs([]), /--base/);
  assert.throws(() => parseArgs(['--base', 'http://192.0.2.10:8080/ui/']), /--base/);
  assert.throws(() => parseArgs(['--base', 'http://192.0.2.10:8080', '--widths', '1536,abc']), /--widths/);
  assert.throws(() => parseArgs(['--base', 'http://192.0.2.10:8080', '--bogus']), /unknown/);
});

test('writes: only GETs pass, and the POSTs the caller names', () => {
  const allow = ['/api/plan'];
  assert.equal(writeVerdict('GET', 'http://192.0.2.10/api/profiles', allow), 'pass');
  assert.equal(writeVerdict('post', 'http://192.0.2.10/api/plan', allow), 'pass');
  assert.equal(writeVerdict('POST', 'http://192.0.2.10/api/plan', []), 'abort');
  for (const m of ['POST', 'PUT', 'PATCH', 'DELETE']) assert.equal(writeVerdict(m, 'http://192.0.2.10/api/config/profiles/x', allow), 'abort');
  assert.equal(writeVerdict('PUT', 'http://192.0.2.10/api/plan', allow), 'abort');
});

test('console: the reports of the harness own aborts are not errors', () => {
  const aborted = new Set(['http://192.0.2.10/api/config/settings']);
  assert.equal(consoleCounts({ text: 'Failed to load resource: net::ERR_FAILED', url: 'http://192.0.2.10/api/config/settings' }, aborted), false);
  assert.equal(consoleCounts({ text: 'TypeError: Failed to fetch', url: '' }, aborted), false);
  assert.equal(consoleCounts({ text: 'TypeError: Failed to fetch', url: '' }, new Set()), true);
  assert.equal(consoleCounts({ text: 'ReferenceError: x is not defined', url: 'http://192.0.2.10/ui/app.js' }, aborted), true);
});

const answers = {
  '/api/profiles': { profiles: [{ id: 'made-by-a-sweep', generated: true }, { id: 'alpha' }, { id: 'beta:16k' }] },
  '/api/models': { models: [{ key: 'Alpha-7B-Q4_K_M.gguf', file: 'Alpha-7B-Q4_K_M.gguf' }, { key: 'beta.gguf' }] },
  '/api/models/trash': { items: [{ trash_id: 't-1', file: 'old.gguf' }] },
  '/api/bench/runs': { runs: [{ number: 12, id: 'r-12' }] },
  '/api/nodes': { nodes: [{ id: '02:00:00:00:00:01', hostname: 'node-a' }] },
  '/api/config/profiles': { profiles: { alpha: {}, 'beta:16k': {}, 'made-by-a-sweep': {} }, revision: 4 },
};
const get = async p => { if (!(p in answers)) throw new Error('HTTP 404'); return answers[p]; };

test('routes: the detail routes come from the first item of each listing, never a fixed id', async () => {
  const { routes, ids, notes } = await apiRoutes(get);
  assert.deepEqual(routes, ['profiles/alpha', 'placement/alpha', 'profiles/new', 'models/Alpha-7B-Q4_K_M.gguf', 'models?model=Alpha-7B-Q4_K_M.gguf',
    'models/deleted', 'models/deleted?item=t-1', 'benchmarks/runs', 'benchmarks/runs/12', 'zones/node-a']);
  assert.deepEqual(notes, ['/api/bench/groups: HTTP 404']);
  for (const id of ['alpha', 'beta:16k', 'Alpha-7B-Q4_K_M.gguf', 't-1', '12', 'node-a', 'made-by-a-sweep']) assert.ok(ids.has(id), id);
  assert.ok(!ids.has('new') && !ids.has('deleted'));
});

test('routes: one per shape; ids and numbers are the shape\'s stars, words are not', async () => {
  const { ids } = await apiRoutes(get);
  assert.equal(shapeOf(parseRoute('#/profiles/beta%3A16k'), ids), 'profiles/*');
  assert.equal(shapeOf(parseRoute('#/profiles/new'), ids), 'profiles/new');
  assert.equal(shapeOf(parseRoute('#/benchmarks/runs/31/step-2'), ids), 'benchmarks/runs/*/*');
  assert.equal(shapeOf(parseRoute('#/models?model=beta.gguf&page=3&period=day'), ids), 'models?model=*');
  const q = new RouteQueue(ids, 5);
  assert.ok(q.add('profiles/alpha'));
  assert.ok(!q.add('#/profiles/beta%3A16k'));
  assert.ok(q.add('profiles/new'));
  assert.ok(q.add('#/settings/agents'));
  assert.ok(!q.add('#/settings/agents?q=x'));
  assert.ok(q.add('#/models?model=beta.gguf&page=2'));
  assert.equal(q.items.at(-1).route, 'models?model=beta.gguf');
  assert.ok(q.add('overview'));
  assert.ok(!q.add('requests'));
  assert.equal(q.skipped, 1);
  assert.equal(parseRoute('https://example.invalid/page'), null);
});

test('list and file names', () => {
  assert.deepEqual(list([{ a: 1 }]), [{ a: 1 }]);
  assert.deepEqual(list({ total: 3, rows: [{ a: 1 }] }), [{ a: 1 }]);
  assert.deepEqual(list({ rows: [] }), []);
  assert.equal(fileName('models?model=Alpha-7B.gguf', 1536), 'models_model_Alpha-7B.gguf-1536.png');
});

test('squeezed text: the rule', () => {
  const box = { text: 'a model name of some length', width: 300, height: 17, clientWidth: 300, scrollWidth: 300, clientHeight: 17, scrollHeight: 17,
    overflowX: 'visible', overflowY: 'visible', textOverflow: 'clip', fontSizePx: 14, lineHeightPx: 17, hasTitle: false, visuallyHidden: false };
  assert.equal(squeezeVerdict(box, 12), null, 'fits');
  assert.equal(squeezeVerdict({ ...box, text: 'short' }, 12), null, 'short texts are not judged');
  assert.equal(squeezeVerdict({ ...box, scrollWidth: 420, overflowX: 'hidden' }, 12), 'clipped');
  assert.equal(squeezeVerdict({ ...box, scrollWidth: 420, textOverflow: 'ellipsis' }, 12), 'clipped');
  assert.equal(squeezeVerdict({ ...box, scrollWidth: 420, textOverflow: 'ellipsis', hasTitle: true }, 12), 'titled');
  assert.equal(squeezeVerdict({ ...box, scrollWidth: 420 }, 12), null, 'overflow visible: it spills, not clipped (the sideways check sees a page scroll)');
  assert.equal(squeezeVerdict({ ...box, scrollHeight: 60, overflowY: 'hidden' }, 12), 'clipped', 'a line clamp');
  // 12 characters at 14 px are 84 px: a box of 80 px whose text wraps is a column too narrow
  const col = { ...box, width: 80, clientWidth: 80, scrollWidth: 80, height: 85, clientHeight: 85, scrollHeight: 85 };
  assert.equal(squeezeVerdict(col, 12), 'narrow');
  assert.equal(squeezeVerdict({ ...col, width: 84, clientWidth: 84, scrollWidth: 84 }, 12), null);
  assert.equal(squeezeVerdict({ ...col, height: 17, clientHeight: 17, scrollHeight: 17 }, 12), null, 'a short label on one line is not squeezed');
  assert.equal(squeezeVerdict({ ...box, width: 0 }, 12), null, 'not drawn');
  assert.equal(squeezeVerdict({ ...box, width: 1, height: 1, scrollWidth: 420, overflowX: 'hidden' }, 12), null, 'a screen-reader text (1 px box)');
  assert.equal(squeezeVerdict({ ...box, scrollWidth: 420, overflowX: 'hidden', visuallyHidden: true }, 12), null, 'a screen-reader text (clip)');
});
