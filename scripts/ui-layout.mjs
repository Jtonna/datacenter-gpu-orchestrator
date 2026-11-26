#!/usr/bin/env node
// Layout check of the web UI in a real headless Chrome (DevTools protocol):
// opens pages of a running server (usually scripts/ui-fixture-server.mjs) at
// several widths, measures them, and optionally saves a picture of each.
//
//   node scripts/ui-layout.mjs <cases.json> [<picture dir>]
//
//   cases.json: [{"base": "http://127.0.0.1:3122", "name": "models", "route": "#/models",
//                 "widths": [1536, 1280, 390], "menu": "open"|"closed", "js": "code run after the draw",
//                 "rows": ["css selector of a table row", ...], "viewport": true}]
//
// Chrome: UI_BROWSER, else the usual install paths. It prints one line per
// page and width ("<name>-<width>: ok" or what failed) and exits 1 when any
// failed, 3 when no Chrome is found.
//
// The measures (MEASURE below):
//   OVERLAP  in every row that "rows" names (and every table row, tr, on the
//            page), the text of one cell against the text of every other cell
//            of the same row. A cell is a direct child of the row (td/th for a
//            table). Its text is the rectangles of its visible text nodes
//            (Range.getClientRects over each text node, clipped to the
//            rectangle of every ancestor up to the cell whose overflow is not
//            visible: text cut off by an ellipsis is not drawn there). Two
//            rectangles of different cells that intersect, or that share a
//            line (vertical overlap) with less than 4 px between them, fail:
//            words painted over each other, or run together ("set by hand1 profile").
//   SIDEWAYS the page is wider than the window.
//   PAST     a cell's text reaches past the right edge of its row.
//   CUT      (720 px and wider) a cell's text reaches past the box the row is
//            seen through (the nearest ancestor of the row that clips: its card
//            or a scroll box): a column out of sight. On a phone a table may
//            scroll inside its card, so it is not counted there.
//   ERRORS   an exception was thrown on the page.
import { spawn } from 'node:child_process';
import { writeFileSync, readFileSync, mkdtempSync, mkdirSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const [list, out] = process.argv.slice(2);
if (!list) { console.error('usage: node scripts/ui-layout.mjs <cases.json> [<picture dir>]'); process.exit(2); }
if (out) mkdirSync(out, { recursive: true });
const cases = JSON.parse(readFileSync(list, 'utf8'));
const bin = [process.env.UI_BROWSER, 'C:/Program Files/Google/Chrome/Application/chrome.exe', 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
  '/usr/bin/google-chrome', '/usr/bin/chromium', '/usr/bin/chromium-browser', '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'].find(p => p && existsSync(p));
if (!bin) { console.error('no Chrome: set UI_BROWSER'); process.exit(3); }
const port = 9300 + Math.floor(Math.random() * 600);
const chrome = spawn(bin, ['--headless=new', '--disable-gpu', '--hide-scrollbars', '--no-first-run', `--remote-debugging-port=${port}`,
  `--user-data-dir=${mkdtempSync(join(tmpdir(), 'ui-layout-'))}`, '--window-size=1536,864', 'about:blank'], { stdio: 'ignore' });
const sleep = ms => new Promise(r => setTimeout(r, ms));
let target;
for (let i = 0; i < 100 && !target; i++) {
  try { target = (await (await fetch(`http://127.0.0.1:${port}/json`)).json()).find(t => t.type === 'page'); } catch { /* not up yet */ }
  if (!target) await sleep(200);
}
if (!target) { console.error('Chrome did not start'); chrome.kill(); process.exit(3); }
const ws = new WebSocket(target.webSocketDebuggerUrl);
await new Promise(r => { ws.onopen = r; });
let id = 0;
const pending = new Map();
const errors = [];
ws.onmessage = e => {
  const m = JSON.parse(e.data);
  if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); }
  if (m.method === 'Runtime.exceptionThrown') errors.push(JSON.stringify(m.params.exceptionDetails).slice(0, 200));
};
const send = (method, params = {}) => new Promise(r => { const i = ++id; pending.set(i, r); ws.send(JSON.stringify({ id: i, method, params })); });
const evalJS = async expr => {
  const r = await send('Runtime.evaluate', { expression: expr, awaitPromise: true, returnByValue: true });
  if (r.result && r.result.exceptionDetails) return { jsError: JSON.stringify(r.result.exceptionDetails).slice(0, 300) };
  return r.result && r.result.result && r.result.result.value;
};
await send('Runtime.enable');
await send('Page.enable');

const MEASURE = rows => `(() => {
  const GAP = 4;
  const d = document.documentElement;
  const visible = e => { const cs = getComputedStyle(e); return cs.display !== 'none' && cs.visibility !== 'hidden' && !e.closest('.sr,[aria-hidden=true]:not(.mr-h)'); };
  // the rectangles of a cell's drawn text, each clipped by its clipping ancestors up to the cell
  const textRects = cell => {
    const out = [];
    const walk = document.createTreeWalker(cell, NodeFilter.SHOW_TEXT);
    for (let n = walk.nextNode(); n; n = walk.nextNode()) {
      if (!n.textContent.trim() || !visible(n.parentElement)) continue;
      const r = document.createRange(); r.selectNodeContents(n);
      for (let b of r.getClientRects()) {
        b = { l: b.left, r: b.right, t: b.top, b: b.bottom };
        for (let a = n.parentElement; a && a !== cell.parentElement; a = a.parentElement) {
          const cs = getComputedStyle(a);
          if (cs.overflowX !== 'visible' || cs.overflowY !== 'visible') { const c = a.getBoundingClientRect(); b = { l: Math.max(b.l, c.left), r: Math.min(b.r, c.right), t: Math.max(b.t, c.top), b: Math.min(b.b, c.bottom) }; }
        }
        if (b.r - b.l > 0.5 && b.b - b.t > 0.5) out.push(Object.assign(b, { text: n.textContent.trim().slice(0, 40) }));
      }
    }
    return out;
  };
  const overlap = [], past = [], cut = [];
  // the box a row is seen through: the nearest ancestor that clips (a card, a scroll box)
  const viewOf = row => { for (let a = row.parentElement; a; a = a.parentElement) { const cs = getComputedStyle(a); if (cs.overflowX !== 'visible') return a.getBoundingClientRect(); } return { left: 0, right: d.clientWidth }; };
  const seen = new Set();
  for (const row of document.querySelectorAll(${JSON.stringify(['tr'].concat(rows || []).join(','))})) {
    if (seen.has(row) || !visible(row)) continue;
    seen.add(row);
    const rb = row.getBoundingClientRect();
    if (!rb.width) continue;
    const cells = [...row.children].filter(visible).map(c => ({ c, rects: textRects(c) }));
    const label = (row.getAttribute('data-model') || row.getAttribute('data-id') || row.className || row.tagName).toString().slice(0, 40);
    const view = viewOf(row);
    for (const x of cells) for (const a of x.rects) if (a.r > view.right + 1 || a.l < view.left - 1) cut.push(label + ': "' + a.text + '" reaches ' + Math.round(a.r) + ', its card ends at ' + Math.round(view.right));
    for (const x of cells) for (const a of x.rects) if (a.r > rb.right + 1) past.push(label + ': "' + a.text + '" ends at ' + Math.round(a.r) + ', the row at ' + Math.round(rb.right));
    for (let i = 0; i < cells.length; i++) for (let j = i + 1; j < cells.length; j++) {
      for (const a of cells[i].rects) for (const b of cells[j].rects) {
        const sameLine = Math.min(a.b, b.b) - Math.max(a.t, b.t) > 1;
        const gap = Math.max(b.l - a.r, a.l - b.r);
        if (sameLine && gap < GAP) overlap.push(label + ': "' + a.text + '" and "' + b.text + '" ' + (gap < 0 ? 'overlap by ' + Math.round(-gap) + ' px' : Math.round(gap * 10) / 10 + ' px apart'));
      }
    }
  }
  return { sw: d.scrollWidth, cw: d.clientWidth, sh: d.scrollHeight, rows: seen.size, overlap: [...new Set(overlap)].slice(0, 12), past: [...new Set(past)].slice(0, 8), cut: [...new Set(cut)].slice(0, 8) };
})()`;

let failed = 0;
for (const c of cases) {
  for (const w of c.widths || [1536, 1280, 390]) {
    errors.length = 0;
    await send('Emulation.setDeviceMetricsOverride', { width: w, height: c.height || 864, deviceScaleFactor: 1, mobile: false });
    await send('Page.navigate', { url: c.base + '/ui/' });
    await sleep(300);
    if (c.menu) await evalJS(`localStorage.setItem('llm-scheduler.menu-closed', ${JSON.stringify(c.menu === 'closed' ? '1' : '0')})`);
    await send('Page.navigate', { url: 'about:blank' });
    await sleep(100);
    await send('Page.navigate', { url: c.base + '/ui/' + (c.route || '') });
    await sleep(c.wait || 2500);
    if (c.js) { const r = await evalJS(`(async () => { ${c.js} })()`); if (r && r.jsError) errors.push('case js: ' + r.jsError); await sleep(c.after || 800); }
    const m = await evalJS(MEASURE(c.rows));
    const name = `${c.name}-${w}${c.menu ? '-menu-' + c.menu : ''}`;
    if (!m || m.jsError) { console.log(`${name}: the measure failed ${JSON.stringify(m)}`); failed++; continue; }
    if (out) {
      const cs = (await send('Page.getLayoutMetrics')).result.cssContentSize;
      const shot = await send('Page.captureScreenshot', { format: 'png', captureBeyondViewport: !c.viewport, clip: { x: 0, y: 0, width: w, height: c.viewport ? (c.height || 864) : Math.min(Math.max(864, cs.height), 4000), scale: 1 } });
      writeFileSync(join(out, name + '.png'), Buffer.from(shot.result.data, 'base64'));
    }
    const bad = (m.sw > m.cw ? ' SIDEWAYS ' + m.sw + '>' + m.cw : '') + (m.overlap.length ? ' OVERLAP ' + JSON.stringify(m.overlap) : '') +
      (m.past.length ? ' PAST ' + JSON.stringify(m.past) : '') + (m.cut.length && w >= 720 ? ' CUT ' + JSON.stringify(m.cut) : '') + (errors.length ? ' ERRORS ' + JSON.stringify(errors) : '') + (c.minRows && m.rows < c.minRows ? ` ROWS ${m.rows} < ${c.minRows}` : '');
    if (bad) failed++;
    console.log(`${name}: ${m.rows} rows${bad || ' ok'}`);
  }
}
ws.close();
chrome.kill();
process.exit(failed ? 1 : 0);
