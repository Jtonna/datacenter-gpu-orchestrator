// profilediff.js: "Compare with…" (the owner's walkthrough, build-92 W2).
// Pure functions, no DOM and no fetch: the test-profile panel's compare
// dialog (views/profiles.js compareDialog) reads two profiles
// (GET /api/config/profiles/<id>) and hands their specs here.

// splitFlags(args) -> [[flag, value], ...] in the order written: "--x=1" and
// "--x 1" give ["--x", "1"], a flag with no value gives ["--x", true]; a word
// that is not a flag and follows none is kept as its own entry [word, true].
// A repeated flag keeps every value, joined with a space.
export function splitFlags(args) {
  const out = new Map();
  const list = Array.isArray(args) ? args.map(String) : [];
  const isFlag = s => /^--?[A-Za-z]/.test(s);
  const put = (k, v) => { out.set(k, out.has(k) && out.get(k) !== true && v !== true ? out.get(k) + ' ' + v : v); };
  for (let i = 0; i < list.length; i++) {
    const t = list[i];
    if (!isFlag(t)) { put(t, true); continue; }
    const eq = t.indexOf('=');
    if (eq > 0) { put(t.slice(0, eq), t.slice(eq + 1)); continue; }
    if (i + 1 < list.length && !isFlag(list[i + 1])) { put(t, list[i + 1]); i++; continue; }
    put(t, true);
  }
  return [...out.entries()];
}

const same = (x, y) => JSON.stringify(x === undefined ? null : x) === JSON.stringify(y === undefined ? null : y);

// profileDiff(a, b, order) -> one row a setting: {key, a, b, same, only}.
// a and b are profile specs (objects); order lists the keys to put first
// (the editor's order), the rest follow by name. extra_args is compared
// flag by flag: one row a flag, key "extra_args <flag>". A setting written
// on one side only has only: 'a' or 'b' (its value on the other side is
// undefined); written on neither side, it is left out. same: equal values.
// The caller shows the rows with same false, or all of them.
export function profileDiff(a, b, order = []) {
  a = a || {}; b = b || {};
  const keys = [...new Set([...Object.keys(a), ...Object.keys(b)])].filter(k => k !== 'extra_args' && (a[k] !== undefined || b[k] !== undefined));
  const rank = k => { const i = order.indexOf(k); return i < 0 ? order.length : i; };
  keys.sort((x, y) => rank(x) - rank(y) || x.localeCompare(y));
  const row = (key, va, vb) => ({ key, a: va, b: vb, same: same(va, vb), only: va === undefined ? 'b' : vb === undefined ? 'a' : null });
  const rows = keys.map(k => row(k, a[k], b[k]));
  const fa = new Map(splitFlags(a.extra_args)), fb = new Map(splitFlags(b.extra_args));
  const flags = [...new Set([...fa.keys(), ...fb.keys()])];
  for (const f of flags) rows.push(row('extra_args ' + f, fa.get(f), fb.get(f)));
  return rows;
}
