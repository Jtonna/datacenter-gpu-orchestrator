// fx-editors-refused.js: the editors' refused save, for a picture (the
// screenshot cannot click). Served at /ui/fx-editors-refused.html with
// --bench editors and the route #/benchmarks/new/cold-reload-loaded: it opens
// phase 3, empties its prompt, writes a comment and presses Save; the fixture
// refuses (no id, no profile, the prompt), and the save bar names each place.
const wait = (f, ms = 8000) => new Promise((ok, no) => {
  const t0 = Date.now();
  const tick = () => { const v = f(); if (v) ok(v); else if (Date.now() - t0 > ms) no(new Error('fx-editors-refused: timed out')); else setTimeout(tick, 50); };
  tick();
});
const put = (el, v) => { el.value = v; el.dispatchEvent(new Event('input', { bubbles: true })); el.dispatchEvent(new Event('change', { bubbles: true })); };
(async () => {
  (await wait(() => document.querySelector("[data-phase='2'] button[data-act=edit-phase]"))).click();
  put(await wait(() => document.querySelector("[data-phase='2'] [data-prompt='0'] textarea[aria-label=Prompt]")), '');
  put(await wait(() => document.querySelector('.ed-savebar input[aria-label=Comment]')), 'try it');
  (await wait(() => document.querySelector('.ed-savebar button[data-act=save]:not([disabled])'))).click();
  await wait(() => document.querySelector('.ed-savebar [data-refused]'));
  if (document.activeElement && document.activeElement.blur) document.activeElement.blur();
})();
