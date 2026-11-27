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

// the figures as core/format.js writes them (this module imports nothing:
// node runs it alone in TestUINextProfileDiff)
const nf0 = new Intl.NumberFormat('en-US', { maximumFractionDigits: 0 });
const nf1 = new Intl.NumberFormat('en-US', { minimumFractionDigits: 1, maximumFractionDigits: 1 });
const num = v => nf0.format(v);
const gb = v => (Math.abs(v) < 1023.5 ? nf0.format(Math.round(v)) + ' MB' : nf1.format(v / 1024) + ' GB');
const ctxShort = v => (v % 1024 === 0 ? v / 1024 + 'k' : nf0.format(v));

const same = (x, y) => JSON.stringify(x === undefined ? null : x) === JSON.stringify(y === undefined ? null : y);

// SETTINGS: THE one table of the settings "Compare with…" knows, in the
// profile panel's words (the reviewer, build-92): label; show(v, x) -> the
// written value as text (x.zone(id) -> a zone's name); def(spec) -> what an
// unwritten setting means, in words, when it has a default (else "not set");
// dv(spec) -> the default as a value, so a written default and an unwritten
// one are the same; mono: the written value is an id, a file, a flag or a
// figure. A key not in the table is shown as its raw key, in mono.
const list = v => (Array.isArray(v) ? v : v == null ? [] : [v]);
const ctxText = v => (v % 1024 === 0 ? `${ctxShort(v)} (${num(v)})` : num(v));
const stageOf = sp => (sp.generated || list(sp.force_nodes).length ? 'test' : 'production');
const STAGE = { test: 'Test', production: 'Production' };
export const SETTINGS = {
  kind: { label: 'Kind', show: v => (v === 'command' ? 'A program (command)' : 'A model file (llama-server)'), def: () => 'A model file (llama-server)', dv: () => 'llama' },
  model: { label: 'Model file', mono: true },
  ctx: { label: 'Context size', show: ctxText, mono: true },
  ctx_above_trained: { label: 'Context above the trained size', show: v => (v ? 'allowed' : 'not allowed'), def: () => 'not allowed', dv: () => false },
  parallel: { label: 'Parallel slots', show: v => num(v), mono: true, def: () => '1 (the default)', dv: () => 1 },
  kv_quant: { label: 'KV cache precision', mono: true },
  flash_attn: { label: 'Flash attention', show: v => (v ? 'on' : 'off'), def: () => 'off (the default)', dv: () => false },
  needs_mib: { label: 'Memory need', show: v => (v ? `${gb(v)} (${num(v)} MiB), set by hand` : 'worked out by the plan'), def: () => 'worked out by the plan', dv: () => 0 },
  allow_tiers: { label: 'Allowed tiers', show: v => (list(v).length ? list(v).join(', ') : 'any tier'), def: () => 'any tier', dv: () => [] },
  force_nodes: { label: 'Forced zones', show: (v, x) => (list(v).length ? list(v).map(id => x.zone(id)).join(', ') : 'none: the scheduler picks'), def: () => 'none: the scheduler picks', dv: () => [] },
  host: { label: 'Hosted on', show: v => (v === 'node' ? 'a zone (its first forced zone)' : "the scheduler's machine"), def: () => "the scheduler's machine", dv: () => 'server' },
  tensor_split: { label: 'Tensor split', show: v => list(v).join(' / '), mono: true, def: () => 'by memory (the default)', dv: () => [] },
  cmd: { label: 'Command', show: v => list(v).join(' '), mono: true },
  check_endpoint: { label: 'Readiness path', mono: true },
  aliases: { label: 'Aliases', show: v => (list(v).length ? list(v).join(', ') : 'none'), mono: true, def: () => 'none', dv: () => [] },
  description: { label: 'Description' },
  display_name: { label: 'Display name' },
  generated: { label: 'Generated', show: v => (v ? 'yes, for benchmarks' : 'no, made by hand'), def: () => 'no, made by hand', dv: () => false },
  stage: { label: 'Stage', show: v => STAGE[v] || String(v), def: sp => STAGE[stageOf(sp)] + ' (not marked: the rule)', dv: sp => stageOf(sp) },
  ttl: { label: 'Idle time before unload', mono: true, def: () => 'the default' },
  load_timeout: { label: 'Load timeout', mono: true, def: () => 'the default' },
  stall_timeout: { label: 'Stall timeout', mono: true, def: () => 'the default' },
  load_stall_timeout: { label: 'Load stall timeout', mono: true, def: () => 'the default' },
};
// effective(k, v, spec) -> v, or the setting's default when v is not written
const effective = (k, v, spec) => (v === undefined && SETTINGS[k] && SETTINGS[k].dv ? SETTINGS[k].dv(spec || {}) : v);

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
  const row = (key, va, vb, ea = va, eb = vb) => ({ key, a: va, b: vb, same: same(ea, eb), only: va === undefined ? 'b' : vb === undefined ? 'a' : null });
  // a written default and an unwritten setting are the same (parallel 1 and no parallel)
  const rows = keys.map(k => row(k, a[k], b[k], effective(k, a[k], a), effective(k, b[k], b)));
  const fa = new Map(splitFlags(a.extra_args)), fb = new Map(splitFlags(b.extra_args));
  const flags = [...new Set([...fa.keys(), ...fb.keys()])];
  for (const f of flags) rows.push(row('extra_args ' + f, fa.get(f), fb.get(f)));
  return rows;
}

// describe(r, specA, specB, x) -> {label, a, b, keyRaw, monoA, monoB}: one
// row of profileDiff in the page's words through SETTINGS. A flag of the
// extra arguments is "Extra argument <flag>" with its value ("set" for a
// flag with no value; "not set" is the truth there). A key SETTINGS does not
// know keeps its raw key (keyRaw: shown in mono) and its raw value.
// x.zone(id) -> a zone's name (the caller's lookup).
export function describe(r, specA, specB, x = {}) {
  x = { zone: id => id, ...x };
  if (r.key.startsWith('extra_args ')) {
    const f = v => (v === undefined ? 'not set' : v === true ? 'set' : String(v));
    return { label: 'Extra argument', flag: r.key.slice(11), a: f(r.a), b: f(r.b), monoA: r.a !== undefined && r.a !== true, monoB: r.b !== undefined && r.b !== true };
  }
  const s = SETTINGS[r.key];
  const raw = v => (Array.isArray(v) ? v.join(', ') : v && typeof v === 'object' ? JSON.stringify(v) : String(v));
  const text = (v, sp) => (v === undefined ? (s && s.def ? s.def(sp || {}) : 'not set') : s && s.show ? s.show(v, x) : raw(v));
  const mono = v => v !== undefined && (!s || !!s.mono);
  return { label: s ? s.label : r.key, keyRaw: !s, a: text(r.a, specA), b: text(r.b, specB), monoA: mono(r.a), monoB: mono(r.b) };
}
