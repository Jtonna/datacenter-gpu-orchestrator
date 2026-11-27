// node --test scripts/gate/ : the overlap rule in a real browser, on a fixture
// page that reproduces a fault the old picture run let through (a needs
// cell's second line painted over "1 profile" and "3 d ago" on the models
// list). Skipped when Playwright or its Chromium is not installed: the rule
// itself is checked without a browser in pages-lib.test.mjs.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { loadPlaywright, measurePage, squeezeVerdict, overlapFindings, overlapLine } from './pages-lib.mjs';

const here = dirname(fileURLToPath(import.meta.url));

test('overlap: the fixture page, in Chromium', async t => {
  let browser;
  try {
    const { chromium } = await loadPlaywright();
    browser = await chromium.launch();
  } catch (e) {
    t.skip('no Playwright or no Chromium: ' + String(e.message).split('\n')[0]);
    return;
  }
  try {
    const page = await browser.newPage({ viewport: { width: 1000, height: 700 }, deviceScaleFactor: 1 });
    await page.setContent(readFileSync(join(here, 'fixtures', 'overlap.html'), 'utf8'));
    const m = await page.evaluate(measurePage, { minChars: 12, verdict: squeezeVerdict.toString(), overlaps: overlapFindings.toString() });
    const lines = m.overlapSamples.map(f => overlapLine('fixture', f));
    const pairs = m.overlapSamples.map(f => f.a + ' | ' + f.b).sort();
    assert.deepEqual(pairs, [
      'a very long name that does not wrap | next cell',
      'set by hand, measured on 2 of 3 nodes | 1 profile',
      'set by hand, measured on 2 of 3 nodes | 3 d ago',
    ], lines.join('\n'));
    assert.equal(m.overlaps, 3);
    // the cases left out are left out by the rule, not by the geometry: each
    // set-apart text's box does run over its neighbour's text
    const crossing = await page.evaluate(() => {
      const box = el => { const r = document.createRange(); r.selectNodeContents(el.firstChild); return r.getBoundingClientRect(); };
      const meet = (a, b) => Math.min(a.right, b.right) - Math.max(a.left, b.left) > 1 && Math.min(a.bottom, b.bottom) - Math.max(a.top, b.top) > 1;
      const rows = document.querySelectorAll('.mr');
      const out = {};
      out.clipped = meet(box(rows[2].children[1]), box(rows[2].children[2]));
      for (const c of ['badge', 'tip', 'sr']) out[c] = meet(box(rows[3].querySelector('.' + c)), box(rows[3].children[2]));
      return out;
    });
    assert.deepEqual(crossing, { clipped: true, badge: true, tip: true, sr: true });
    assert.match(lines.find(l => l.includes("'1 profile'")), /^overlap: #\/fixture row \d+ \(div\.mr\): 'set by hand, measured on 2 of 3 nodes' over '1 profile'$/);
  } finally {
    await browser.close();
  }
});
