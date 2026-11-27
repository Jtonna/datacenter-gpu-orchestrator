// fx-editors-entry-off.js: the group editor with an entry not startable
// here, for a picture (the screenshot cannot click). Served at
// /ui/fx-editors-entry-off.html with --bench editors and the route
// #/benchmarks/group/s3a/edit: it adds host-gpu-alone, whose dry read the
// fixture refuses, so the entry says why and what that means.
const wait = (f, ms = 8000) => new Promise((ok, no) => {
  const t0 = Date.now();
  const tick = () => { const v = f(); if (v) ok(v); else if (Date.now() - t0 > ms) no(new Error('fx-editors-entry-off: timed out')); else setTimeout(tick, 50); };
  tick();
});
(async () => {
  const add = await wait(() => document.querySelector('input[data-add]'));
  add.value = 'host'; add.dispatchEvent(new Event('input', { bubbles: true }));
  (await wait(() => document.querySelector('button[data-add-id=host-gpu-alone]'))).click();
  await wait(() => document.querySelector('[data-off]'));
  if (document.activeElement && document.activeElement.blur) document.activeElement.blur();
})();
