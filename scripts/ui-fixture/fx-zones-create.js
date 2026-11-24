// fx-zones-create.js: the Zones-Create board for the mockup comparison (the
// compare tool cannot click or type, and it refuses every write). Served at
// /ui/fx-zones-create.html with --zones groups --pending waiting and the
// route #/zones: it presses Create new zone, types the name zone-9, picks
// rack-a, writes a comment and presses Create zone. The POST is answered
// here, in the page, from the fixture's GET /api/zones/pending/<id> (status
// 201, as the server answers it): nothing is sent as a write. ?tab=dl then
// opens the Download a script tab; ?form=1 stops at the filled form.
const q = new URLSearchParams(location.search);
const wait = (f, ms = 8000) => new Promise((ok, no) => {
  const t0 = Date.now();
  const tick = () => { const v = f(); if (v) ok(v); else if (Date.now() - t0 > ms) no(new Error('fx-zones-create: timed out')); else setTimeout(tick, 50); };
  tick();
});
const realFetch = window.fetch.bind(window);
window.fetch = async (u, o = {}) => {
  const p = new URL(String(u), location.href).pathname;
  if (p === '/api/zones/pending' && (o.method || 'GET') === 'POST') {
    const r = await realFetch('/api/zones/pending/pz-0000000000000001');
    return new Response(await r.text(), { status: 201, headers: { 'Content-Type': 'application/json' } });
  }
  return realFetch(u, o);
};
const type = (el, v) => { el.value = v; el.dispatchEvent(new Event('input', { bubbles: true })); el.dispatchEvent(new Event('change', { bubbles: true })); };
(async () => {
  (await wait(() => document.querySelector('button[data-act=create]'))).click();
  type(await wait(() => document.querySelector('[data-cz=name]')), 'zone-9');
  type(document.querySelector('[data-cz=group]'), 'rack-a');
  type(document.querySelector('[data-cz=comment]'), 'a new machine for rack-a');
  await wait(() => document.querySelector('[data-cz=default]'));
  if (!q.get('form')) {
    (await wait(() => document.querySelector('[data-go]:not([disabled])'))).click();
    await wait(() => document.querySelector('[data-cz=state]'));
    if (q.get('tab') === 'dl') (await wait(() => document.querySelector('[data-cz-tab=dl]'))).click();
  }
  if (document.activeElement && document.activeElement.blur) document.activeElement.blur();
})();
