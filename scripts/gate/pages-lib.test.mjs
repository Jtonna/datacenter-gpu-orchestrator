// node --test scripts/gate/ : the drawing harness's rules, without a browser.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readdirSync, readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { parseArgs, writeVerdict, consoleCounts, apiRoutes, RouteQueue, parseRoute, shapeOf, fileName, squeezeVerdict, overlapFindings, overlapLine, list, VIEWS } from './pages-lib.mjs';

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
  '/api/bench/runs?limit=200': { runs: [
    { number: 14, id: 'r-14', kind: 'benchmark', result: 'passed' },
    { number: 13, id: 'r-13', kind: 'group', result: 'failed' },
    { number: 12, id: 'r-12', kind: 'benchmark', result: 'passed', pinned: true }] },
  '/api/bench/groups': { groups: [{ id: 'g-quick' }, { id: 'g-long' }, { id: 'g-coding' }, { id: 'g-fourth' }] },
  '/api/nodes': { nodes: [{ id: '02:00:00:00:00:01', hostname: 'node-a' }] },
  '/api/config/profiles': { profiles: { alpha: {}, 'beta:16k': {}, 'made-by-a-sweep': {} }, revision: 4 },
};
const get = async p => { if (!(p in answers)) throw new Error('HTTP 404'); return answers[p]; };

test('routes: the detail routes come from the API answers, never a fixed id', async () => {
  const { routes, ids, notes } = await apiRoutes(get);
  assert.deepEqual(routes, [
    'profiles/alpha', 'placement/alpha', 'profiles?profile=alpha',
    'models/Alpha-7B-Q4_K_M.gguf', 'models?model=Alpha-7B-Q4_K_M.gguf',
    'models/deleted?item=t-1',
    'benchmarks/runs/14', 'benchmarks/runs/14/0', // the newest, with its report
    'benchmarks/runs/13', 'benchmarks/runs/13/0', // a failed one (here also the group's)
    'benchmarks/runs/13', 'benchmarks/runs/13/0', // a group's
    'benchmarks/runs/14', // a direct benchmark's
    'benchmarks/runs/12', // a pinned or noted one
    'benchmarks/all/g-quick', 'benchmarks/all/g-long', 'benchmarks/all/g-coding', 'benchmarks/suite/g-quick',
    'zones/node-a']);
  assert.deepEqual(notes, []);
  for (const id of ['alpha', 'beta:16k', 'Alpha-7B-Q4_K_M.gguf', 't-1', '12', 'r-13', 'g-long', 'node-a', 'made-by-a-sweep']) assert.ok(ids.has(id), id);
  assert.ok(!ids.has('new') && !ids.has('deleted'));
  const none = await apiRoutes(async p => { throw new Error('HTTP 503'); });
  assert.deepEqual(none.routes, []);
  assert.equal(none.notes.length, 7);
});

test('routes: seeds drawn once each; crawled links once per shape', async () => {
  const { ids } = await apiRoutes(get);
  assert.equal(shapeOf(parseRoute('#/profiles/beta%3A16k'), ids), 'profiles/*');
  assert.equal(shapeOf(parseRoute('#/profiles/new'), ids), 'profiles/new');
  assert.equal(shapeOf(parseRoute('#/benchmarks/runs/31/step-2'), ids), 'benchmarks/runs/*/*');
  assert.equal(shapeOf(parseRoute('#/models?model=beta.gguf&page=3&period=day'), ids), 'models?model=*&page=*');
  assert.equal(shapeOf(parseRoute('#/benchmarks/runs?show=load-times&kind=group'), ids), 'benchmarks/runs?show=load-times');
  const q = new RouteQueue(ids, 8);
  assert.ok(q.add('profiles/alpha', true));
  assert.ok(!q.add('#/profiles/beta%3A16k'), 'a crawled link of a shape drawn already');
  assert.ok(q.add('benchmarks/runs/14', true));
  assert.ok(q.add('benchmarks/runs/12', true), 'a seed of a shape drawn already: other data');
  assert.ok(!q.add('benchmarks/runs/14', true), 'the same route twice');
  assert.ok(q.add('#/benchmarks/runs?show=load-times'));
  assert.ok(q.add('#/benchmarks/runs?show=memory'), 'another view of the page');
  assert.ok(!q.add('#/benchmarks/runs?show=memory&kind=group'), 'a filter is dropped');
  assert.ok(q.add('#/settings/agents'));
  assert.ok(!q.add('#/settings/agents?q=x'));
  assert.ok(q.add('#/models?model=beta.gguf&period=day'));
  assert.equal(q.items.at(-1).route, 'models?model=beta.gguf');
  assert.ok(q.add('overview', true));
  assert.ok(!q.add('requests', true));
  assert.equal(q.skipped, 1);
  assert.equal(parseRoute('https://example.invalid/page'), null);
});

// VIEWS must name every view the new UI registers: a view added to the UI
// and not here is drawn only if some page links to it.
test('routes: every view the UI registers is in VIEWS', () => {
  const dir = join(dirname(fileURLToPath(import.meta.url)), '..', '..', 'internal', 'api', 'ui', 'next', 'views');
  const files = readdirSync(dir, { recursive: true }).filter(f => String(f).endsWith('.js'));
  const ids = new Set();
  for (const f of files) for (const m of readFileSync(join(dir, String(f)), 'utf8').matchAll(/registerView\(\{\s*id:\s*'([^']+)'/g)) ids.add(m[1]);
  assert.ok(ids.size >= 10, 'found ' + ids.size + ' views');
  const views = new Set(VIEWS.map(v => v.split(/[/?]/)[0]));
  assert.deepEqual([...ids].filter(id => !views.has(id)), []);
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
  assert.equal(squeezeVerdict({ ...col, height: 34, clientHeight: 34, scrollHeight: 34 }, 12), null, 'two lines are not a column');
  assert.equal(squeezeVerdict({ ...col, text: 'eighteen chars ok!' }, 12), null, 'a text under twice the threshold is not judged narrow');
  assert.equal(squeezeVerdict(col, 12), 'narrow');
  assert.equal(squeezeVerdict({ ...col, width: 84, clientWidth: 84, scrollWidth: 84 }, 12), null);
  assert.equal(squeezeVerdict({ ...col, height: 17, clientHeight: 17, scrollHeight: 17 }, 12), null, 'a short label on one line is not squeezed');
  assert.equal(squeezeVerdict({ ...box, width: 0 }, 12), null, 'not drawn');
  assert.equal(squeezeVerdict({ ...box, width: 1, height: 1, scrollWidth: 420, overflowX: 'hidden' }, 12), null, 'a screen-reader text (1 px box)');
  assert.equal(squeezeVerdict({ ...box, scrollWidth: 420, overflowX: 'hidden', visuallyHidden: true }, 12), null, 'a screen-reader text (clip)');
});

test('overlap: the rule, on rectangles', () => {
  const t = (id, text, left, top, right, bottom, clips) => ({ id, text, rect: { left, top, right, bottom }, clips });
  const row = (...cells) => [{ row: '1 (div.mr)', cells }];
  // the fault: the needs cell's second line (y 17..34) runs from x 100 to 330, over the next two cells
  const fault = row([t(1, '9.1 GiB', 100, 0, 150, 17), t(2, 'set by hand, measured on 2 of 3 nodes', 100, 17, 330, 34)],
    [t(3, '1 profile', 200, 9, 260, 26)], [t(4, '3 d ago', 280, 9, 330, 26)]);
  assert.deepEqual(overlapFindings(fault).map(f => f.a + ' | ' + f.b),
    ['set by hand, measured on 2 of 3 nodes | 1 profile', 'set by hand, measured on 2 of 3 nodes | 3 d ago']);
  assert.equal(overlapLine('models', overlapFindings(fault)[0]), "overlap: #/models row 1 (div.mr): 'set by hand, measured on 2 of 3 nodes' over '1 profile'");
  // touching (the edges meet) and a 1 px graze are not findings; 2 px is
  assert.deepEqual(overlapFindings(row([t(1, 'left', 0, 0, 100, 17)], [t(2, 'right', 100, 0, 200, 17)])), [], 'touching');
  assert.deepEqual(overlapFindings(row([t(1, 'left', 0, 0, 101, 17)], [t(2, 'right', 100, 0, 200, 17)])), [], '1 px');
  assert.equal(overlapFindings(row([t(1, 'left', 0, 0, 102, 17)], [t(2, 'right', 100, 0, 200, 17)])).length, 1, '2 px');
  assert.deepEqual(overlapFindings(row([t(1, 'above', 0, 0, 200, 17)], [t(2, 'below', 50, 16, 150, 33)])), [], 'lines that meet by 1 px');
  // clipped by an ancestor's overflow (the cell, x 0..100): the run-on part is not painted
  const cell = [{ left: 0, top: 0, right: 100, bottom: 48 }];
  assert.deepEqual(overlapFindings(row([t(1, 'a long name cut by its cell', 0, 0, 260, 17, cell)], [t(2, 'next', 110, 0, 160, 17)])), [], 'clipped');
  assert.equal(overlapFindings(row([t(1, 'a long name cut by its cell', 0, 0, 260, 17, [{ left: 0, top: 0, right: 140, bottom: 48 }])], [t(2, 'next', 110, 0, 160, 17)])).length, 1,
    'a clip wider than the cell lets the overlap show');
  assert.deepEqual(overlapFindings(row([t(1, 'gone', 0, 0, 260, 17, [{ left: 300, top: 0, right: 400, bottom: 48 }])], [t(2, 'next', 0, 0, 160, 17)])), [], 'clipped away entirely');
  // one cell's own texts may stack (an icon and its badge are one cell); only different cells are judged
  assert.deepEqual(overlapFindings(row([t(1, 'icon', 0, 0, 20, 20), t(2, '12', 10, 0, 30, 12)], [t(3, 'far', 300, 0, 340, 17)])), []);
  // a pair found in a nested row and in its outer row is reported once
  const twice = [...row([t(1, 'a', 0, 0, 100, 17)], [t(2, 'b', 50, 0, 150, 17)]), { row: '2 (div.inner)', cells: [[t(1, 'a', 0, 0, 100, 17)], [t(2, 'b', 50, 0, 150, 17)]] }];
  assert.equal(overlapFindings(twice).length, 1);
  assert.deepEqual(overlapFindings([{ row: '1', cells: [[], [t(1, 'x', 0, 0, 10, 10)]] }]), [], 'an empty cell');
});
