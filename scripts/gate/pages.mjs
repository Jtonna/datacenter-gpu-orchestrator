#!/usr/bin/env node
// pages.mjs: the every-page drawing harness of the release gate (README, "How
// a release is judged"). It draws every route of the web UI of a running
// scheduler at given widths, as full-page PNGs, and measures each page:
// sideways scroll, squeezed texts, console errors. It sends no write: every
// request that is not a GET is aborted in the browser (--allow-post lets a
// named dry-run POST through). The rules are in pages-lib.mjs, checked by
// `node --test scripts/gate/`. Run with --help for the flags.
//
// Playwright is a dev dependency of this folder only (scripts/gate/
// package.json, pinned): `npm install` here, then `npx playwright install
// chromium`, or run in the Playwright image of the same version. --help,
// --dry-run and the tests need no browser and no install.
import { mkdir, writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { USAGE, parseArgs, writeVerdict, consoleCounts, apiRoutes, RouteQueue, fileName, squeezeVerdict, measurePage, pageLine } from './pages-lib.mjs';

async function getJSON(base, path) {
  const r = await fetch(base + path, { headers: { Accept: 'application/json' } });
  if (!r.ok) throw new Error('HTTP ' + r.status);
  return r.json();
}

async function loadPlaywright() {
  const tries = ['playwright', process.env.PLAYWRIGHT_MODULE].filter(Boolean);
  for (const m of tries) {
    try { return await import(m.startsWith('playwright') ? m : 'file://' + resolve(m)); } catch { /* next */ }
  }
  throw new Error('playwright is not installed: run `npm install` in scripts/gate (or set PLAYWRIGHT_MODULE to its index.mjs)');
}

// drawOne opens a fresh page at route and width, waits, measures, draws.
async function drawOne(ctx, o, route, width) {
  const page = await ctx.newPage();
  await page.setViewportSize({ width, height: o.height });
  const aborted = new Set(), blocked = [], errors = [];
  await page.route('**/*', req => {
    const r = req.request();
    if (writeVerdict(r.method(), r.url(), o.allowPost) === 'pass') return req.continue();
    aborted.add(r.url());
    blocked.push(r.method() + ' ' + new URL(r.url()).pathname);
    return req.abort();
  });
  page.on('console', m => {
    if (m.type() !== 'error') return;
    const msg = { text: m.text(), url: (m.location() || {}).url || '' };
    if (consoleCounts(msg, aborted)) errors.push(msg.text.slice(0, 300));
  });
  page.on('pageerror', e => errors.push('uncaught: ' + String(e && e.message || e).slice(0, 300)));
  let links = [];
  try {
    await page.goto(o.base + '/ui/#/' + route, { waitUntil: 'load', timeout: 60000 });
    await page.waitForTimeout(o.wait);
    const m = await page.evaluate(measurePage, { minChars: o.minChars, verdict: squeezeVerdict.toString() });
    links = await page.evaluate(() => [...document.querySelectorAll('a[href*="#/"]')].map(a => a.getAttribute('href')));
    const png = join(o.out, fileName(route, width));
    await page.screenshot({ path: png, fullPage: true });
    return { page: { route, width, sideways: m.sideways, scroll_width: m.scrollWidth, squeezed: m.squeezed, titled: m.titled,
      squeezed_samples: m.samples, console_errors: errors, blocked_writes: blocked, png }, links };
  } catch (e) {
    errors.push('harness: ' + e.message);
    return { page: { route, width, sideways: false, squeezed: 0, titled: 0, squeezed_samples: [], console_errors: errors, blocked_writes: blocked, png: '', failed: true }, links };
  } finally {
    await page.close();
  }
}

async function main() {
  let o;
  try { o = parseArgs(process.argv.slice(2)); } catch (e) { console.error(e.message + '\n\n' + USAGE); return 2; }
  if (o.help) { console.log(USAGE); return 0; }
  const found = await apiRoutes(p => getJSON(o.base, p));
  const q = new RouteQueue(found.ids, o.maxPages);
  // the shell's views: what /ui/ redirects to and every link of its navigation, read in the browser
  q.add('overview');
  for (const r of found.routes) q.add(r);
  if (o.dryRun) {
    for (const n of found.notes) console.log('note: ' + n);
    for (const it of q.items) console.log(`ROUTE #/${it.route}  (shape ${it.shape})`);
    console.log(`dry run: ${q.items.length} routes from the API and the start page; the shell's links and the links of each page are added while drawing`);
    return 0;
  }
  const { chromium } = await loadPlaywright();
  await mkdir(o.out, { recursive: true });
  const browser = await chromium.launch();
  const ctx = await browser.newContext({ deviceScaleFactor: 1 });
  const pages = [];
  try {
    for (let i = 0; i < q.items.length; i++) {
      const { route } = q.items[i];
      for (const w of o.widths) {
        const { page, links } = await drawOne(ctx, o, route, w);
        pages.push(page);
        console.log(pageLine(page));
        if (w === o.widths[0]) for (const l of links) q.add(l);
      }
    }
  } finally {
    await browser.close();
  }
  const totals = {
    routes: q.items.length, pictures: pages.filter(p => p.png).length, failed: pages.filter(p => p.failed).length,
    sideways: pages.filter(p => p.sideways).length, squeezed: pages.reduce((a, p) => a + p.squeezed, 0),
    console_errors: pages.reduce((a, p) => a + p.console_errors.length, 0), blocked_writes: pages.reduce((a, p) => a + p.blocked_writes.length, 0),
    routes_over_max: q.skipped,
  };
  const summary = { base: o.base, widths: o.widths, min_chars: o.minChars, notes: found.notes, totals, pages };
  await writeFile(join(o.out, 'summary.json'), JSON.stringify(summary, null, 2) + '\n');
  console.log(`pages: ${totals.routes} routes, ${totals.pictures} pictures, failed ${totals.failed}, sideways ${totals.sideways}, squeezed ${totals.squeezed}, console errors ${totals.console_errors}, blocked writes ${totals.blocked_writes} -> ${join(o.out, 'summary.json')}`);
  if (totals.failed) return 2;
  if (o.strict && (totals.sideways || totals.squeezed || totals.console_errors)) return 1;
  return 0;
}

main().then(code => process.exit(code), e => { console.error(e && e.stack || e); process.exit(2); });
