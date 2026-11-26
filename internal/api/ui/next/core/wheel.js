// The mouse wheel over an overlay stays with the overlay (the reviewer's
// order, 26 Nov). CSS overscroll-behavior: contain (app.css) holds the wheel
// in a box that scrolls; a box that does not scroll cannot contain anything,
// so the wheel went on to the page behind (the health popup over the Models
// list: the list moved 116 px). Over these overlays a wheel step that no box
// inside them can take is dropped:
//   .hp         the System health popup
//   .pop        a popover (core/popover.js)
//   .cm         a context menu (Zones)
//   .drw2       the Ask Potluck drawer
//   .side       a side panel, docked or over the page (its head and foot)
//   .side-back  the shade behind a side panel over the page
//   .drawer-back the shade behind the drawer
//   .backdrop   the shade behind a dialog
// A box inside one that can still scroll that way takes the step as before.
export const HOLD = ['.hp', '.pop', '.cm', '.drw2', '.side', '.side-back', '.drawer-back', '.backdrop'];
const SEL = HOLD.join(', ');

// takes(el, dy): el scrolls up or down and has room left that way
function takes(el, dy) {
  if (el.scrollHeight <= el.clientHeight + 1) return false;
  const oy = getComputedStyle(el).overflowY;
  if (oy !== 'auto' && oy !== 'scroll') return false;
  return dy < 0 ? el.scrollTop > 0 : el.scrollTop + el.clientHeight < el.scrollHeight - 1;
}

export function holdWheel(e) {
  if (!e.deltaY || e.ctrlKey) return; // sideways (a wide table) and zoom pass
  const t = e.target && e.target.closest ? e.target : null;
  const root = t && t.closest(SEL);
  if (!root) return;
  for (let el = t; el; el = el.parentElement) {
    if (takes(el, e.deltaY)) return;
    if (el === root) break;
  }
  e.preventDefault();
}

export function startWheel() {
  document.addEventListener('wheel', holdWheel, { passive: false });
}
