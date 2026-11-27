package api

import (
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The wheel over an overlay that does not scroll (the health popup at a
// desktop width, a popover, a context menu, the Ask drawer, a shade) is
// dropped: CSS overscroll-behavior cannot hold it in a box that does not
// scroll, and the page behind moved (live, 26 Nov: the Models list 116 px
// under the health popup). A box inside an overlay that can still scroll
// that way takes the step; the wheel over the page itself is untouched.
func TestUINextOverlaysSwallowTheWheel(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	_, here, _, _ := runtime.Caller(0)
	mod := (&url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(filepath.Dir(here), "ui", "next", "core", "wheel.js"))}).String()
	if !strings.HasPrefix(mod, "file:///") {
		mod = strings.Replace(mod, "file://", "file:///", 1)
	}
	script := `
const styles = new Map();
globalThis.getComputedStyle = (el) => ({ overflowY: styles.get(el) || 'visible' });
const listeners = [];
globalThis.document = { addEventListener: (type, fn, opt) => listeners.push([type, fn, opt]) };
function el(cls, parent, box) {
  const e = { cls, parentElement: parent || null, scrollTop: 0, scrollHeight: 100, clientHeight: 100, ...box };
  e.closest = (sel) => { for (let x = e; x; x = x.parentElement) if (sel.split(', ').some((s) => x.cls === s.slice(1))) return x; return null; };
  return e;
}
const { holdWheel, startWheel, HOLD } = await import(process.argv[1]);
startWheel();
const [type, , opt] = listeners[0] || [];
if (type !== 'wheel' || !opt || opt.passive !== false) throw new Error('startWheel must listen to wheel with passive:false, got ' + JSON.stringify([type, opt]));
const wheel = (target, deltaY, extra) => { let dropped = false; holdWheel({ target, deltaY, ...extra, preventDefault() { dropped = true; } }); return dropped; };
const fails = [];
const want = (name, got, exp) => { if (got !== exp) fails.push(name + ': dropped ' + got + ', want ' + exp); };
const page = el('main');
// every overlay the design draws over the page is listed
for (const s of ['.hp', '.pop', '.cm', '.drw2', '.side', '.side-back', '.drawer-back', '.backdrop']) if (!HOLD.includes(s)) fails.push(s + ' is not held');
// the health popup at a desktop width: it does not scroll (overflow hidden)
const hp = el('hp'); styles.set(hp, 'hidden');
const row = el('hp-row', el('hp-body', hp));
want('wheel down over the health popup', wheel(row, 500), true);
want('wheel up over the health popup', wheel(row, -500), true);
// a list inside an overlay that has room to scroll takes the step; at its end the step is dropped
const pop = el('pop'); const list = el('list', pop, { scrollHeight: 400, clientHeight: 100, scrollTop: 0 }); styles.set(list, 'auto');
want('wheel down over a list with room below', wheel(el('li', list), 300), false);
want('wheel up over the same list at its top', wheel(el('li', list), -300), true);
list.scrollTop = 300;
want('wheel down over the list at its end', wheel(el('li', list), 300), true);
// the page itself, sideways steps and pinch zoom are untouched
want('wheel over the page', wheel(el('td', page), 300), false);
want('sideways over the popup', wheel(row, 0, { deltaX: 200 }), false);
want('pinch zoom over the popup', wheel(row, 300, { ctrlKey: true }), false);
want('a text node target', wheel({}, 300), false);
if (fails.length) { console.log(fails.join('\n')); process.exit(1); }
console.log('ok');
`
	cmd := exec.Command(node, "--input-type=module", "-e", script, mod)
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("wheel guard: %v\n%s", err, out)
	}
}
