// fx-mcp-created.js: the MCP-Created board for the mockup comparison (the
// compare tool cannot click, and it aborts every write). Served at
// /ui/fx-mcp-created.html with the route #/mcp?new=1. It answers the page's
// own requests for /api/agents in the browser from a local copy of the
// fixture's keys (keys.mjs), so nothing is sent to any server, then fills
// New key (grafana-reader), presses Create key and confirms the comment
// dialog: the panel shows the fixture key once.
// With ?rotate in the address it opens Manage for ops-bot and rotates instead.
import { Keys } from './keys.mjs';

const keys = new Keys('normal');
const realFetch = window.fetch.bind(window);
const answer = r => new Response(JSON.stringify(r.body), { status: r.status, headers: { 'Content-Type': 'application/json' } });
window.fetch = async (u, opts = {}) => {
  const url = new URL(String(u), location.href);
  const p = url.pathname, m = opts.method || 'GET';
  const body = () => { try { return JSON.parse(opts.body || '{}'); } catch { return {}; } };
  if (p === '/api/agents' && m === 'GET') return answer(keys.listAnswer(location.host));
  if (p === '/api/agents' && m === 'POST') return answer(keys.create(body(), location.host));
  const rot = /^\/api\/agents\/([^/]+)\/rotate$/.exec(p);
  if (rot && m === 'POST') return answer(keys.rotate(rot[1], location.host));
  return realFetch(u, opts);
};

const wait = (f, ms = 8000) => new Promise((ok, no) => {
  const t0 = Date.now();
  const tick = () => { const v = f(); if (v) ok(v); else if (Date.now() - t0 > ms) no(new Error('fx-mcp-created: timed out')); else setTimeout(tick, 50); };
  tick();
});
const type = (el, v) => { el.value = v; el.dispatchEvent(new Event('input', { bubbles: true })); };

(async () => {
  const rotate = location.search.includes('rotate');
  if (rotate) {
    location.hash = '#/mcp?key=ag_000000000001';
    (await wait(() => document.querySelector('aside.side button[data-act=rotate]'))).click();
  } else {
    if (!location.hash.includes('new=1')) location.hash = '#/mcp?new=1';
    type(await wait(() => document.querySelector('aside.side input#mcp-name')), 'grafana-reader');
    (await wait(() => document.querySelector('aside.side button[data-act=create]'))).click();
  }
  type(await wait(() => document.querySelector('textarea.dlg-comment')), rotate ? 'the old key was in a log' : 'for the dashboard');
  (await wait(() => document.querySelector('.dlg-foot button.pri:not([disabled])'))).click();
  await wait(() => document.querySelector('aside.side .mcp-key'));
  if (document.activeElement && document.activeElement.blur) document.activeElement.blur();
})();
