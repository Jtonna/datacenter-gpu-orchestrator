// fx-zones-confirm.js: the Zones-Confirm board for the mockup comparison
// (the compare tool cannot click). Served at /ui/fx-zones-confirm.html with
// --zones fleet and the route #/zones/ra-04: once the zone panel is drawn it
// presses Force unload, which opens the unload dialog. It sends nothing to
// the server.
const wait = (f, ms = 8000) => new Promise((ok, no) => {
  const t0 = Date.now();
  const tick = () => { const v = f(); if (v) ok(v); else if (Date.now() - t0 > ms) no(new Error('fx-zones-confirm: timed out')); else setTimeout(tick, 50); };
  tick();
});
(async () => {
  (await wait(() => document.querySelector('button[data-zp=force]'))).click();
  await wait(() => document.querySelector('[role=dialog] [data-go]'));
  if (document.activeElement && document.activeElement.blur) document.activeElement.blur();
})();
