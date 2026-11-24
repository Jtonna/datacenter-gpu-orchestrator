// fx-zones-selected.js: the Zones-Fleet-Selected board for the mockup
// comparison (the compare tool cannot click). Served at
// /ui/fx-zones-selected.html with --zones fleet: once the list is drawn it
// ticks ra-36, ra-35 and ra-45, opens the right-click menu where the board
// draws it and opens Move to group. It sends nothing to the server.
const want = ['ra-36', 'ra-35', 'ra-45'];
const rowOf = name => Array.from(document.querySelectorAll('div.fr[data-zone]')).find(r => (r.getAttribute('aria-label') || '').startsWith(name + ','));
const wait = (f, ms = 8000) => new Promise((ok, no) => {
  const t0 = Date.now();
  const tick = () => { const v = f(); if (v) ok(v); else if (Date.now() - t0 > ms) no(new Error('fx-zones-selected: timed out')); else setTimeout(tick, 50); };
  tick();
});
(async () => {
  await wait(() => want.every(rowOf));
  for (const n of want) rowOf(n).querySelector('input.cb').click();
  await new Promise(r => setTimeout(r, 50));
  rowOf('ra-36').dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: 262, clientY: 278 }));
  (await wait(() => document.querySelector('button[data-cm=move]'))).click();
  if (document.activeElement && document.activeElement.blur) document.activeElement.blur();
})();
