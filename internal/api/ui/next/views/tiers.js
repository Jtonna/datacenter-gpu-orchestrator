// Tiers and zones (the gear's Tiers and zones; no mockup: inferred from the
// design language, as Settings and Profiles). Replaced the carried-over
// screen, whose help text was squeezed into a narrow column over the tier
// numbers and whose side cards overlapped. Here, top to bottom:
//
//   - "Tiers": one line of help, then a table with one row per tier in the
//     order the tiers are checked: order, what it matches (in words),
//     priority, headroom, memory kind, link speed, settle time and the
//     zones in it now (count; the names on hover and in the panel); ↑ ↓ in
//     the row move it; the row opens the tier in the docked side panel
//     (?tier=<n>): its zones, Move up / Move down / Edit / Remove;
//     "Add a tier" opens the tier dialog (every field of the schema's tier
//     scope, platform and device included);
//   - "Excluded zones" (exclude_nodes: never receive a model) and "Named
//     only" (no_auto_nodes: a model only from a profile or a load that
//     names them), each a simple list with Remove, and Add with the known
//     zones offered;
//   - "Node overrides" (node_overrides) with membership fleet; with agents
//     one line says they are not used (any stored ones are still listed, so
//     they can be removed);
//   - "Zones and their tier": which tier each known zone is in now;
//   - ONE save bar ("N changes · Save"), and ONE save dialog: a dry run of
//     every part first (a refusal shows the server's sentence and saves
//     nothing), then the list of changes and the required comment, then
//     the writes with If-Match.
//
// The page NEVER works out which tier a zone is in: "Zones now", the
// panel's zones and "Tier now" come only from the server (tier_zones on GET
// /api/config/tiers; tier / tier_reason on GET /api/nodes). A tier's
// "Never reached" warning is the server's never_reached; the save dialog
// says the same when the unsaved order puts a tier below an empty match.
//
// Reads: REST GET /api/config, GET /api/config/schema (scope "tier"), GET
// /api/config/tiers and GET /api/nodes (again on status messages, at most
// every 5 s); WebSocket "status" (view.nodes: excluded, auto, eligible) and
// "config".
// Writes: PUT /api/config/tiers, /api/config/exclude-nodes,
// /api/config/no-auto-nodes, /api/config/node-overrides (whole lists, only
// the ones that changed), each with a dry run first, If-Match and the
// comment (X-Comment). Signed by the default user.
// Route: #/tiers?tier=<n>.

import { h } from '../core/dom.js';
import { registerView, navigate } from '../core/router.js';
import { state, on } from '../core/store.js';
import * as api from '../core/api.js';
import { liveView, pageHead, badge, nodeChips, empty, banner, skeleton, button, dialog, schemaForm, confirm, toast } from '../core/ui.js';
import { openPanel, openPanelNow } from '../core/panel.js';
import { num, mib, plural, DASH } from '../core/format.js';
import { viewNodes } from '../core/status.js';
import { loadSchema, loadConfig, fieldsOf, clone, same } from '../core/configdata.js';
import { agentsMode } from '../core/nodeactions.js';
import { savedBanner, watchConfig, noticeBanner } from '../core/changes.js';

// Unsaved edits survive a visit to another screen: {tiers, exclude, noauto, overrides} or null.
let draft = null;

const zname = n => n.hostname || n.name || n.id;

// The rule in plain words: every field of the match, joined with " and ".
// A key the page does not know is written as "<key> is <value>", never
// dropped. "Every zone (no rule)" only for a truly empty match.
const RULE_WORDS = {
  hostname: v => 'host name matches ' + v,
  role: v => 'role is ' + v,
  source: v => 'source is ' + v,
  platform: v => 'platform is ' + v,
  device: v => 'a device name matches ' + v,
  local: v => (v === true ? 'only the local zone' : v === false ? 'only remote zones' : 'local is ' + v),
};
const RULE_ORDER = ['hostname', 'role', 'source', 'platform', 'device', 'local'];
const isEmptyMatch = m => !m || !Object.keys(m).length;
export function ruleText(t) {
  const m = (t && t.match) || {};
  const keys = Object.keys(m).sort((a, b) => {
    const x = RULE_ORDER.indexOf(a), y = RULE_ORDER.indexOf(b);
    return (x < 0 ? 99 : x) - (y < 0 ? 99 : y) || (a < b ? -1 : a > b ? 1 : 0);
  });
  const bits = keys.map(k => (RULE_WORDS[k] ? RULE_WORDS[k](m[k]) : k + ' is ' + (typeof m[k] === 'string' ? m[k] : JSON.stringify(m[k]))));
  if (!bits.length) return 'Every zone (no rule)';
  const s = bits.join(' and ');
  return s[0].toUpperCase() + s.slice(1);
}
// The sentence of a tier below an empty match (the server's never_reached).
export const neverText = above => 'Never reached: tier ' + above + ' above matches every zone';
// neverReached(tiers) -> [{index, above}] for each tier below an empty match
// (1-based); this reads the order of the list only, not which zone is where.
export function neverReached(tiers) {
  const out = [];
  let above = 0;
  tiers.forEach((t, i) => { if (above) out.push({ index: i + 1, above }); else if (isEmptyMatch(t.match)) above = i + 1; });
  return out;
}
const facts = t => ({
  priority: t.priority != null ? String(t.priority) : DASH,
  headroom: t.headroom_mib ? mib(t.headroom_mib) : DASH,
  memory: t.shared_memory ? 'Shared' : 'Dedicated',
  link: t.link_mb_s ? num(t.link_mb_s, 0) + ' MB/s' : DASH,
  settle: t.settle || DASH,
});

// ---- the tier dialog ----

const LOCAL_CHOICES = ['any zone', 'only the local zone', 'only remote zones'];
const LABELS = { 'match.hostname': 'Host name pattern', 'match.role': 'Role', 'match.source': 'Source', 'match.platform': 'Platform', 'match.device': 'Device name pattern',
  priority: 'Priority', headroom_mib: 'Headroom', shared_memory: 'Shared memory', link_mb_s: 'Link speed', settle: 'Settle time' };
const ORDER = ['match.hostname', 'match.role', 'match.source', 'match.platform', 'match.device', 'where', 'priority', 'headroom_mib', 'shared_memory', 'link_mb_s', 'settle'];
const MATCH_KEYS = ['hostname', 'role', 'source', 'platform', 'device'];
function tierDialog(fields, initial, index, isNew) {
  const fs = fields.filter(f => f.key !== 'match.local').map(f => Object.assign({}, f, { label: LABELS[f.key] || f.label }));
  const loc = fields.find(f => f.key === 'match.local');
  const localField = { key: 'where', type: 'enum', enum: LOCAL_CHOICES, label: 'Which zones', description: loc ? loc.description : '', default: 'any zone' };
  const all = [...fs, localField].sort((a, b) => ORDER.indexOf(a.key) - ORDER.indexOf(b.key));
  const values = clone(initial) || {};
  values.where = LOCAL_CHOICES[values.match && values.match.local === true ? 1 : values.match && values.match.local === false ? 2 : 0];
  const form = schemaForm({ fields: all, values, showDefaults: false });
  return dialog({
    title: isNew ? 'Add a tier' : 'Edit tier ' + (index + 1), wide: true,
    body: close => h('div.col', { style: 'gap:16px' },
      h('p.mut', 'A zone joins the first tier in the list whose rule it fits. Leave the rule empty to match every zone that reaches this tier.'),
      form.el,
      h('div.dlg-foot', button('Cancel', () => close(undefined), 'lg'), button(isNew ? 'Add the tier' : 'Apply to the tier', () => {
        if (!form.validate()) return;
        const v = form.values();
        const out = { match: {} };
        // a key the form does not offer stays as it was
        for (const [k, x] of Object.entries((initial && initial.match) || {})) if (!MATCH_KEYS.includes(k) && k !== 'local') out.match[k] = x;
        for (const k of MATCH_KEYS) if (v.match && v.match[k]) out.match[k] = v.match[k];
        if (v.where === LOCAL_CHOICES[1]) out.match.local = true; else if (v.where === LOCAL_CHOICES[2]) out.match.local = false;
        if (v.priority != null && v.priority !== '') out.priority = v.priority;
        if (v.headroom_mib) out.headroom_mib = v.headroom_mib;
        if (v.shared_memory) out.shared_memory = true;
        if (v.link_mb_s) out.link_mb_s = v.link_mb_s;
        if (v.settle && v.settle !== '0s') out.settle = v.settle;
        close(out);
      }, 'lg pri', { 'data-act': 'apply' }))),
  });
}

function overrideDialog(nodes, initial) {
  const name = h('input', { class: 'input mono', type: 'text', value: initial ? initial.node : '', placeholder: 'zone id or host name', list: 'trs-known', autocomplete: 'off', spellcheck: 'false', 'aria-label': 'Zone', id: 'trs-ov-node' });
  const rpc = h('input', { class: 'input mono', type: 'text', value: initial ? initial.rpc : '', placeholder: '192.0.2.3:50052', autocomplete: 'off', spellcheck: 'false', 'aria-label': 'RPC address', id: 'trs-ov-rpc' });
  const err = h('span.err-t', { role: 'alert' });
  return dialog({
    title: initial ? 'Edit the override' : 'Add a node override',
    body: close => h('div.col', { style: 'gap:14px' },
      h('p.mut', 'Use this address for the rpc-server of the zone instead of the one the legacy fleet feed reports.'),
      h('datalist', { id: 'trs-known' }, nodes.map(n => h('option', { value: zname(n) }))),
      h('div.field', h('label', 'Zone'), name, h('span.help', 'The zone id or its host name.')),
      h('div.field', h('label', 'RPC address'), rpc, h('span.help', 'host:port of the worker, for example 192.0.2.3:50052.'), err),
      h('div.dlg-foot', button('Cancel', () => close(undefined), 'lg'), button('Apply', () => {
        const n = name.value.trim(), r = rpc.value.trim();
        if (!n) { err.textContent = 'Name the zone.'; name.focus(); return; }
        if (!/^[^\s:]+:\d{1,5}$/.test(r)) { err.textContent = 'Write the address as host:port, for example 192.0.2.3:50052.'; rpc.focus(); return; }
        close({ node: n, rpc: r });
      }, 'lg pri', { 'data-act': 'apply' }))),
  });
}

// The two zone lists: exclude_nodes and no_auto_nodes. Same entries (zone
// id, host name or pattern); only what they do differs.
const LISTS = {
  exclude: { key: 'exclude_nodes', path: '/api/config/exclude-nodes', title: 'Excluded zones', help: 'An excluded zone never receives a model, even from a profile that names it.',
    none: 'No zone is excluded.', add: 'Exclude', already: 'is already excluded.', removeLabel: 'Stop excluding ', input: 'Zone to exclude', on: 'Exclude ', off: 'Stop excluding ' },
  noauto: { key: 'no_auto_nodes', path: '/api/config/no-auto-nodes', title: 'Named only', help: 'A zone here gets a model only from a profile or a load that names it. Automatic placement never picks it and never evicts what runs on it.',
    none: 'Every zone can be picked automatically.', add: 'Add', already: 'is already named only.', removeLabel: 'Make automatic again: ', input: 'Zone to keep for profiles that name it', on: 'Named only: ', off: 'Automatic again: ' },
};

registerView({
  id: 'tiers', title: 'Tiers and zones',
  mount(el, r0) {
    let route = r0 || { query: {} };
    let cfg = null, fields = null, error = '', saved = null, refusal = null, alive = true, saving = false;
    let panel = null, panelFor = '';
    let tierDoc = null, nodesDoc = null;
    const addText = { exclude: '', noauto: '' }, addErr = { exclude: '', noauto: '' };

    const rev = () => (cfg ? cfg.revision : 0);
    const savedState = () => ({ tiers: clone(cfg.tiers || []), exclude: clone(cfg.exclude_nodes || []), noauto: clone(cfg.no_auto_nodes || []), overrides: clone(cfg.node_overrides || {}) });
    const work = () => draft || savedState();
    const dirty = {
      tiers: () => !!draft && !same(draft.tiers, cfg.tiers || []),
      exclude: () => !!draft && !same(draft.exclude, cfg.exclude_nodes || []),
      noauto: () => !!draft && !same(draft.noauto, cfg.no_auto_nodes || []),
      overrides: () => !!draft && !same(draft.overrides, cfg.node_overrides || {}),
    };
    const dirtyAny = () => dirty.tiers() || dirty.exclude() || dirty.noauto() || dirty.overrides();
    const edit = fn => { if (!draft) draft = savedState(); fn(draft); if (!dirtyAny()) draft = null; saved = null; refusal = null; view.refresh(); if (panel) panel.redraw(); };

    // the changes in words, one line each (the save bar counts them)
    function changeLines() {
      if (!cfg || !draft) return [];
      const out = [];
      if (dirty.tiers()) {
        const t0 = cfg.tiers || [], t1 = draft.tiers;
        const key = t => JSON.stringify(t);
        const k0 = t0.map(key), k1 = t1.map(key);
        t1.forEach((t, i) => { if (!k0.includes(key(t))) out.push({ kind: 'tier', text: [h('b', 'Tier ' + (i + 1) + ': '), ruleText(t), h('span.mut', ' · priority ' + facts(t).priority + ', headroom ' + facts(t).headroom + ', ' + facts(t).memory.toLowerCase() + ' memory, link ' + facts(t).link + ', settle ' + facts(t).settle)], mark: 'new or edited' }); });
        t0.forEach(t => { if (!k1.includes(key(t))) out.push({ kind: 'tier', text: [h('b', 'Removed: '), ruleText(t)], mark: 'removed' }); });
        const kept0 = k0.filter(k => k1.includes(k)), kept1 = k1.filter(k => k0.includes(k));
        if (!same(kept0, kept1)) out.push({ kind: 'tier', text: [h('b', 'New order: '), t1.map((t, i) => (i + 1) + '. ' + ruleText(t)).join(' · ')], mark: 'moved' });
      }
      for (const which of ['exclude', 'noauto']) {
        const L = LISTS[which], a = new Set(cfg[L.key] || []), b = new Set(draft[which]);
        for (const x of b) if (!a.has(x)) out.push({ kind: which, text: [L.on, h('span.mono', x)] });
        for (const x of a) if (!b.has(x)) out.push({ kind: which, text: [L.off, h('span.mono', x)] });
      }
      const o0 = cfg.node_overrides || {}, o1 = draft.overrides;
      for (const k of [...new Set([...Object.keys(o0), ...Object.keys(o1)])].sort()) {
        if (same(o0[k], o1[k])) continue;
        out.push({ kind: 'overrides', text: o1[k] ? ['Override ', h('span.mono', k), ' → ', h('span.mono', o1[k].rpc)] : ['Remove the override of ', h('span.mono', k)] });
      }
      return out;
    }

    // The server's membership: GET /api/config/tiers (tier_zones, each tier's
    // zones now, and never_reached) and GET /api/nodes (each zone's tier or
    // tier_reason; the local node too). Read with the configuration and again
    // on the live channel's status messages, at most every 5 s (no timer of
    // their own). A failed read keeps what was there (the page then says
    // "not known").
    const readFacts = async () => {
      await Promise.all([
        api.get('/api/config/tiers').then(v => { tierDoc = v; }, () => {}),
        api.get('/api/nodes').then(v => { nodesDoc = v; }, () => { nodesDoc = null; }),
      ]);
      if (alive) { view.refresh(); if (panel) panel.redraw(); }
    };
    let soon = null, lastRead = 0;
    const offStatus = on('status', () => {
      if (!alive || soon) return;
      soon = setTimeout(() => { soon = null; lastRead = Date.now(); readFacts(); }, Math.max(0, 5000 - (Date.now() - lastRead)));
    });

    async function load() {
      try {
        const [c, f] = await Promise.all([loadConfig(), loadSchema()]);
        cfg = c; fields = fieldsOf(f, 'tier'); error = '';
        if (draft && !dirtyAny()) draft = null;
      } catch (e) { error = e.message; }
      lastRead = Date.now();
      await readFacts();
      if (alive) { view.refresh(); syncPanel(); }
    }
    const watch = watchConfig({ revision: rev, pending: () => !!draft, refresh: load, redraw: () => view.refresh() });

    const ops = () => [
      dirty.tiers() ? { what: 'the tier list', path: '/api/config/tiers', body: { tiers: draft.tiers } } : null,
      dirty.exclude() ? { what: 'the excluded zones', path: '/api/config/exclude-nodes', body: { exclude_nodes: draft.exclude } } : null,
      dirty.noauto() ? { what: 'the named-only zones', path: '/api/config/no-auto-nodes', body: { no_auto_nodes: draft.noauto } } : null,
      dirty.overrides() ? { what: 'the node overrides', path: '/api/config/node-overrides', body: { node_overrides: draft.overrides } } : null,
    ].filter(Boolean);

    // tiers the unsaved order leaves below an empty match that were reached before
    function newlyNever() {
      if (!dirty.tiers()) return [];
      const before = neverReached(cfg.tiers || []);
      return neverReached(draft.tiers).filter(x => !before.some(b => same(cfg.tiers[b.index - 1], draft.tiers[x.index - 1])));
    }

    function refused(e, op) {
      const err = e && e.body && e.body.error;
      if (e && e.status === 409 && err && err.type === 'stale_revision') {
        refusal = { title: 'Someone else changed the configuration first (it is now revision ' + err.current_revision + '). Nothing of yours was saved.', messages: [], reload: true };
      } else {
        const msgs = (err && err.messages && err.messages.length ? err.messages : [e.message || 'The change failed.']);
        refusal = { title: 'The server did not accept ' + op.what + '. Nothing was saved.', messages: msgs };
      }
      view.refresh();
    }

    async function save() {
      const list = ops();
      if (!list.length || saving) return;
      refusal = null;
      saving = true; view.refresh();
      // 1. dry run: the server checks every part before anything is written
      const warnings = [];
      let at = list[0];
      try {
        for (const op of list) {
          at = op;
          const r = await api.put(op.path, op.body, { dryRun: true, ifMatch: rev() });
          for (const w of (r && r.warnings) || []) if (!warnings.includes(w)) warnings.push(w);
        }
      } catch (e) { saving = false; refused(e, at); return; }
      saving = false; view.refresh();
      const lines = changeLines();
      // 2. one comment, then the writes
      const ans = await dialog({
        title: 'Save ' + plural(lines.length, 'change') + '?', wide: true,
        body: h('div.col', { style: 'gap:12px' },
          h('p', 'This writes a new configuration revision on top of revision ' + rev() + '. The scheduler uses it at once.'),
          h('ul.trs-chgs', lines.map(l => h('li', { 'data-chg': l.kind }, l.text, l.mark ? h('span.mut', ' (' + l.mark + ')') : null))),
          warnings.length ? banner('warn', h('span', h('b', 'Check this first. '), warnings.join(' '))) : null,
          newlyNever().map(x => h('div.banner.warn', { role: 'status', 'data-never': String(x.index) }, h('span.grow', h('b', 'Tier ' + x.index + ': '), neverText(x.above))))),
        comment: { label: 'Comment', required: true, placeholder: 'Why are you changing this? It is kept in the audit log.' },
        actions: [{ label: 'Cancel', value: false }, { label: 'Save ' + plural(lines.length, 'change'), kind: 'pri', value: true }],
      });
      // nothing is saved without the comment (the dialog keeps Save disabled while it is empty)
      if (!ans || ans.value !== true || !ans.comment) return;
      let revision = rev(), done = 0;
      for (const op of list) {
        try {
          const r = await api.put(op.path, Object.assign({}, op.body, { comment: ans.comment }), { ifMatch: revision, comment: ans.comment });
          if (r && r.revision) revision = r.revision;
          done++;
        } catch (e) {
          if (done) toast(done + ' of ' + list.length + ' parts were saved before the server refused the rest.', 'err');
          refused(e, op);
          await load();
          return;
        }
      }
      draft = null;
      saved = { revision, changed: true };
      toast('Saved as revision ' + revision, 'ok');
      watch.clear();
      await load();
    }
    async function discard() {
      if (await confirm({ title: 'Discard unsaved changes?', intro: 'The tiers, the excluded and named-only zones and the overrides go back to what is saved. Nothing on the server changes.', confirmLabel: 'Discard', danger: true })) {
        draft = null; refusal = null; view.refresh(); syncPanel();
      }
    }

    // ---- tier actions ----
    const go = q => navigate('tiers', [], Object.assign({}, route.query, q), true);
    const move = (i, d) => {
      const j = i + d;
      edit(w => { const t = w.tiers; if (j < 0 || j >= t.length) return; [t[i], t[j]] = [t[j], t[i]]; });
      if (panelFor === String(i + 1)) go({ tier: String(j + 1) });
    };
    async function editTier(i) {
      const isNew = i == null;
      const v = await tierDialog(fields, isNew ? { priority: Math.max(0, ...work().tiers.map(t => t.priority || 0)) + 1 } : work().tiers[i], i, isNew);
      if (!v) return;
      edit(w => { if (isNew) w.tiers.push(v); else w.tiers[i] = v; });
      if (isNew) go({ tier: String(work().tiers.length) });
    }
    async function removeTier(i) {
      const t = work().tiers[i];
      const mine = zonesOf(i);
      const ok = await confirm({
        title: 'Remove tier ' + (i + 1) + '?', intro: 'The tier leaves the list. Nothing changes on the server until you save.',
        hits: [{ kind: '', text: ['Rule: ', ruleText(t)] },
          mine == null ? { kind: '', text: zonesUnknown(i) }
            : { kind: mine.length ? 'x' : '', text: mine.length ? ['These zones are in this tier now and would join the next tier that fits, or none: ', nodeChips(mine.map(zname))] : 'No zone is in this tier now.' }],
        confirmLabel: 'Remove from the list', danger: true,
      });
      if (!ok) return;
      edit(w => { w.tiers.splice(i, 1); });
      go({ tier: null });
    }
    async function editOverride(node) {
      const cur = node ? { node, rpc: (work().overrides[node] || {}).rpc || '' } : null;
      const v = await overrideDialog(viewNodes(state.status), cur);
      if (!v) return;
      edit(w => { if (node && node !== v.node) delete w.overrides[node]; w.overrides[v.node] = { rpc: v.rpc }; });
    }

    // The zones in (draft) tier i now, ONLY from the server's tier_zones (GET
    // /api/config/tiers): the row is matched to the stored tier it is (same
    // fields), and that tier's entry is read. A new or edited tier, or no
    // answer yet: null (not known; drawn as a dash). The table, the panel and
    // the remove dialog all read this one function.
    function savedIndexOf(i) {
      const tiers = work().tiers, stored = (tierDoc && tierDoc.tiers) || [];
      const used = new Set();
      for (let j = 0; j <= i; j++) {
        const k = stored.findIndex((s, x) => !used.has(x) && same(s, tiers[j]));
        if (k < 0) { if (j === i) return -1; continue; }
        used.add(k);
        if (j === i) return k;
      }
      return -1;
    }
    function tierEntry(i) {
      if (!tierDoc || !Array.isArray(tierDoc.tier_zones)) return null;
      const k = savedIndexOf(i);
      return k < 0 ? null : tierDoc.tier_zones.find(z => z.index === k + 1) || null;
    }
    function zonesOf(i) {
      const e = tierEntry(i);
      return e ? (e.zones || []) : null;
    }
    const zonesUnknown = i => (!tierDoc ? 'Not known yet: the tier list with its zones has not been read.'
      : !Array.isArray(tierDoc.tier_zones) ? 'Not known in this run: the server does not report the zones of each tier.'
        : 'Not known until you save: this tier is new or edited.');
    // the server's never_reached for a row of the saved list (null while the list is edited)
    function neverOf(i) {
      if (dirty.tiers()) return null;
      const e = tierEntry(i);
      if (!e || !e.never_reached) return null;
      const above = work().tiers.findIndex(t => !t.match || !Object.keys(t.match).length);
      return neverText(above >= 0 ? above + 1 : '?');
    }

    // ---- the side panel (?tier=<n>) ----
    function syncPanel() {
      const want = route.query.tier || '';
      const i = Number(want) - 1;
      if (!want || !cfg || !(i >= 0 && i < work().tiers.length)) {
        if (panel) { const p = panel; panel = null; panelFor = ''; p.close(); }
        return;
      }
      if (panel && panelFor === want && openPanelNow() === panel) { panel.redraw(); return; }
      if (panel) { const p = panel; panel = null; p.close(); }
      panelFor = want;
      const idx = () => Number(panelFor) - 1;
      const tier = () => work().tiers[idx()] || {};
      // closesMenu: beside the page the section menu steps aside, so the tiers table keeps its columns
      panel = openPanel({ closesMenu: true,
        label: 'Tier', name: 'Tier ' + want,
        title: () => { const m = zonesOf(idx()); return 'Tier ' + panelFor + ' · ' + (m == null ? DASH + ' zones' : plural(m.length, 'zone')); },
        sub: () => ruleText(tier()),
        body: () => {
          const t = tier(), f = facts(t), mine = zonesOf(idx()), n = work().tiers.length, never = neverOf(idx());
          return [
            never ? h('div.banner.warn', { key: 'never', role: 'status', 'data-never': 'panel' }, h('span.grow', never)) : null,
            h('div.trs-facts', { key: 'facts' },
              [['Checked', ordinal(idx()) + ' of ' + n], ['Priority', f.priority], ['Headroom', f.headroom], ['Memory', f.memory === 'Shared' ? 'Shared with the system' : 'Dedicated'], ['Link speed', f.link], ['Settle time', f.settle]]
                .map(([k, v]) => h('div', h('span.lbl', k), h('b', v)))),
            h('h3.trs-sub', { key: 'zh' }, 'Zones in this tier now'),
            mine == null ? h('p.mut', { key: 'unknown', title: zonesUnknown(idx()) }, DASH + ' ' + zonesUnknown(idx()))
              : mine.length ? h('div.trs-zones', { key: 'zones', 'data-panel-zones': String(mine.length) }, nodeChips(mine.map(zname))) : h('p.mut', { key: 'none', 'data-panel-zones': '0' }, 'No zone is in this tier now.'),
            dirty.tiers() ? h('p.mut', { key: 'unsaved' }, 'With the unsaved tier list. Nothing changes until you save.') : null,
            h('p.mut', { key: 'help' }, 'Order decides which tier a zone joins: the first that fits. Priority decides which tier a model is placed on first: a lower number is tried first.'),
          ];
        },
        foot: () => {
          const i = idx(), n = work().tiers.length;
          return [
            button('Edit', () => editTier(i), 'pri', { 'data-act': 'edit' }),
            button('Move up', () => move(i, -1), '', { disabled: i === 0, 'data-act': 'up' }),
            button('Move down', () => move(i, 1), '', { disabled: i === n - 1, 'data-act': 'down' }),
            h('span.grow'),
            button('Remove', () => removeTier(i), 'dng', { 'data-act': 'remove' }),
          ];
        },
        onClose: () => { if (panelFor === want || panel === null) { panel = null; panelFor = ''; if (route.query.tier) go({ tier: null }); } },
      });
    }
    const ordinal = i => (['1st', '2nd', '3rd'][i] || (i + 1) + 'th');

    // ---- drawing ----
    function tiersPanel() {
      const tiers = work().tiers, open = route.query.tier || '';
      const t0 = cfg.tiers || [];
      return h('section.panel.trs', { key: 'tiers' },
        h('div.ch', h('h2', 'Tiers'), h('span.note', plural(tiers.length, 'tier')), h('span.grow'), button('Add a tier', () => editTier(null), 'sm', { 'data-act': 'add-tier' })),
        h('p.trs-help', 'Checked from the top: a zone joins the first tier it fits, and a zone that fits none is never used. Priority decides where a model is placed first (lower first).'),
        !tiers.length ? empty('No tiers', 'With no tier, no zone is used and no model can be placed. Add a tier to bring zones in.')
          : h('div.trs-scroll', h('table.tbl.trs-tbl',
            h('thead', h('tr',
              h('th.r', { scope: 'col', title: 'The order the tiers are checked in' }, '#'), h('th', { scope: 'col' }, 'Matches'),
              h('th.r', { scope: 'col', title: 'A model is placed on a lower number first' }, 'Priority'), h('th.r', { scope: 'col', title: 'Memory kept free on each zone of this tier' }, 'Headroom'),
              h('th', { scope: 'col', title: 'Whether the GPU memory is the system\'s RAM' }, 'Memory'), h('th.r', { scope: 'col', title: 'Used to size load timeouts' }, 'Link'),
              h('th.r', { scope: 'col', title: 'How long a zone rests after a model leaves it' }, 'Settle'), h('th.r', { scope: 'col' }, 'Zones now'),
              h('th.trs-mv', { scope: 'col' }, h('span.sr', 'Move')))),
            h('tbody', tiers.map((t, i) => {
              const f = facts(t), mine = zonesOf(i), n = String(i + 1), never = neverOf(i);
              const edited = dirty.tiers() && !t0.some(o => same(o, t));
              const openRow = () => go({ tier: n });
              return h('tr', { key: 'tier-' + i, class: 'click' + (open === n ? ' sel' : ''), tabindex: '0', 'data-tier': n, 'aria-selected': open === n ? 'true' : null,
                onclick: e => { if (!e.target.closest('button')) openRow(); }, onkeydown: e => { if (e.target === e.currentTarget && (e.key === 'Enter' || e.key === ' ')) { e.preventDefault(); openRow(); } } },
                h('td.r', n),
                h('td.trs-rule', h('span', { class: (t.match && Object.keys(t.match).length) ? null : 'mut' }, ruleText(t)), edited ? [' ', badge('Edited', 'q')] : null,
                  never ? h('span.trs-never', { 'data-never': n }, never) : null),
                h('td.r', f.priority), h('td.r', f.headroom), h('td', f.memory), h('td.r', f.link), h('td.r', f.settle),
                mine == null ? h('td.r.mut', { 'data-zones': '-', title: zonesUnknown(i) }, DASH)
                  : h('td.r', { 'data-zones': String(mine.length), title: mine.length ? mine.map(zname).join(', ') : 'No zone is in this tier now' }, num(mine.length)),
                h('td.trs-mv',
                  h('button.trs-ib', { type: 'button', 'aria-label': 'Move tier ' + n + ' up', title: 'Move up', disabled: i === 0, 'data-mv': 'up', onclick: () => move(i, -1) }, '↑'),
                  h('button.trs-ib', { type: 'button', 'aria-label': 'Move tier ' + n + ' down', title: 'Move down', disabled: i === tiers.length - 1, 'data-mv': 'down', onclick: () => move(i, 1) }, '↓')));
            })))));
    }

    function addTo(which) {
      const L = LISTS[which], v = (addText[which] || '').trim();
      if (!v) { addErr[which] = 'Write a zone id, a host name or a pattern such as test-*.'; view.refresh(); return; }
      if (work()[which].includes(v)) { addErr[which] = v + ' ' + L.already; view.refresh(); return; }
      addErr[which] = ''; addText[which] = '';
      edit(w => { w[which].push(v); });
    }
    // A stored entry that is the id of a known zone shows the zone's name
    // beside it ("node-03 · 02:00:00:…"); a name, a pattern or an id no zone
    // has stays as stored. Names from GET /api/nodes (else the status agents).
    // Only the text changes: the list, Remove and the save use the stored value.
    function zoneNames() {
      const src = nodesDoc && Array.isArray(nodesDoc.nodes) ? nodesDoc.nodes : state.status && Array.isArray(state.status.agents) ? state.status.agents : [];
      const m = new Map();
      for (const z of src) if (z && z.id && z.name && z.name !== z.id) m.set(z.id, z.name);
      return m;
    }
    function entryText(x) {
      const name = zoneNames().get(x);
      return name ? h('span.grow', { title: 'Stored as the zone id ' + x }, h('span', { 'data-name': name }, name), h('span.mut', ' · '), h('span.mono', x))
        : h('span.mono.grow', x);
    }
    function listPanel(which) {
      const L = LISTS[which], list = work()[which], e0 = cfg[L.key] || [];
      const known = viewNodes(state.status).map(zname).filter(n => !list.includes(n));
      return h('section.panel.trs', { key: which, 'data-list': which },
        h('div.ch', h('h2', L.title), h('span.note', plural(list.length, 'zone'))),
        h('p.trs-help', L.help),
        list.length ? h('ul.trs-list', list.map(x => h('li', { key: which + '-' + x, 'data-entry': x },
          entryText(x), e0.includes(x) ? null : badge('Unsaved', 'q'),
          button('Remove', () => edit(w => { w[which] = w[which].filter(y => y !== x); }), 'sm', { 'aria-label': L.removeLabel + x, 'data-rm': x }))))
          : h('p.trs-none.mut', L.none),
        h('div.trs-add',
          h('datalist', { id: 'trs-known-' + which }, known.map(k => h('option', { value: k }))),
          h('input', { class: 'input mono', type: 'text', value: addText[which] || '', placeholder: 'Zone id, host name or pattern', list: 'trs-known-' + which, 'aria-label': L.input, autocomplete: 'off', spellcheck: 'false', 'data-add': which,
            oninput: e => { addText[which] = e.target.value; }, onkeydown: e => { if (e.key === 'Enter') addTo(which); } }),
          button(L.add, () => addTo(which), 'sm', { 'data-add-btn': which }),
          addErr[which] ? h('span.err-t', { role: 'alert' }, addErr[which]) : null));
    }

    function overridesPanel() {
      const o = work().overrides, names = Object.keys(o).sort(), o0 = cfg.node_overrides || {};
      const agents = agentsMode(state.status);
      return h('section.panel.trs', { key: 'overrides', 'data-list': 'overrides' },
        h('div.ch', h('h2', 'Node overrides'), agents ? badge('Not used in this mode', 'dash', 'membership is agents') : h('span.note', plural(names.length, 'override')), h('span.grow'),
          agents ? null : button('Add an override', () => editOverride(null), 'sm', { 'data-act': 'add-override' })),
        h('p.trs-help', agents ? 'Zones run potluck-agent and report their own addresses, so overrides are not used. They apply only with membership fleet.'
          : 'Use a fixed rpc-server address for a zone instead of the one the legacy fleet feed reports.'),
        names.length ? h('ul.trs-list', names.map(n => h('li', { key: 'ov-' + n },
          h('span.mono', n), h('span.mut', '→'), h('span.mono.grow', (o[n] || {}).rpc || DASH), same(o[n], o0[n]) ? null : badge('Unsaved', 'q'),
          agents ? null : button('Edit', () => editOverride(n), 'sm'),
          button('Remove', () => edit(w => { delete w.overrides[n]; }), 'sm', { 'aria-label': 'Remove the override of ' + n }))))
          : agents ? null : h('p.trs-none.mut', 'No overrides: every zone uses the address the legacy fleet feed reports.'));
    }

    // "Zones and their tier": the tier each zone is in now, ONLY from the
    // server (GET /api/nodes, else the status agents): `tier` or, when it
    // fits none, `tier_reason`. Neither (removed, or not seen since start):
    // a dash, "not known in this run". An excluded zone keeps the tier the
    // server gives and reads "Excluded" under Takes models.
    function zonesPanel() {
      const view0 = viewNodes(state.status), byId = new Map(view0.map(n => [n.id, n]));
      const src = nodesDoc && Array.isArray(nodesDoc.nodes) ? nodesDoc.nodes
        : state.status && Array.isArray(state.status.agents) && state.status.agents.length ? state.status.agents : view0;
      const zones = src.filter(z => z.state !== 'removed' && z.zone_state !== 'removed');
      const tierCell = z => (z.tier && z.tier.index ? h('td', { 'data-tier-now': String(z.tier.index) }, 'Tier ' + z.tier.index)
        : h('td.mut', { 'data-tier-now': '-', title: z.tier_reason || 'not known in this run' }, DASH, h('span.sr', ' ' + (z.tier_reason || 'not known in this run'))));
      const takes = (z, v) => ((z.excluded || (v && v.excluded)) ? badge('Excluded', 'dash', 'exclude_nodes: never receives a model')
        : (z.auto === false || (v && v.auto === false)) ? badge('Named only', 'n', 'no_auto_nodes') : (v ? v.eligible : z.placeable) ? 'Yes' : h('span.mut', 'Not now'));
      const online = (z, v) => (z.state ? (z.state === 'active' ? 'Online' : z.state === 'lost' ? h('span.err-t', 'Offline') : h('span.mut', z.state))
        : v && v.online ? 'Online' : h('span.err-t', 'Offline'));
      return h('section.panel.trs', { key: 'zones' },
        h('div.ch', h('h2', 'Zones and their tier'), h('span.note', plural(zones.length, 'zone'))),
        h('p.trs-help', 'The tier each zone is in now, as the scheduler reports it.' + (dirty.tiers() ? ' Your unsaved tier changes move zones only after you save.' : '')),
        !zones.length ? h('p.trs-none.mut', 'No zone is known yet. Machines appear here when their agent registers.')
          : h('div.trs-scroll', h('table.tbl.trs-ztbl',
            h('thead', h('tr', h('th', { scope: 'col' }, 'Zone'), h('th', { scope: 'col' }, 'Tier now'),
              h('th', { scope: 'col' }, 'Takes models'), h('th', { scope: 'col' }, 'Online'))),
            h('tbody', zones.map(z => {
              const v = byId.get(z.id);
              return h('tr', { key: 'z-' + z.id, 'data-zone': zname(z) },
                h('td.mono', zname(z), z.local ? h('span.mut', ' (local)') : null), tierCell(z), h('td', takes(z, v)), h('td', online(z, v)));
            })))));
    }

    function saveBar() {
      const n = changeLines().length;
      if (!n) return null;
      return h('div.setp-save.trs-save', { key: 'save', role: 'status' },
        h('span', h('b', plural(n, 'change'))), h('span.mut', 'Nothing is applied until you save.'),
        h('span.setp-save-btns', button('Discard', discard, 'sm', { 'data-act': 'discard' }), button(saving ? 'Checking…' : 'Save', save, 'pri sm', { disabled: saving, 'data-save': '' })));
    }

    const render = () => {
      const w = cfg ? work() : null;
      const head = pageHead('Tiers and zones', w ? [h('b', num(w.tiers.length)), w.tiers.length === 1 ? ' tier' : ' tiers', ' · ', h('b', num(w.exclude.length)), ' excluded',
        ' · ', h('b', num(w.noauto.length)), ' named only'] : null);
      if (error && !cfg) return [head, banner('err', ['The configuration could not be read: ', error])];
      if (!cfg || !fields) return [head, h('section.panel', { key: 'wait' }, skeleton(6))];
      return [
        head,
        noticeBanner(watch.notice, { onReload: () => { watch.clear(); load(); }, onDismiss: () => { watch.clear(); view.refresh(); } }),
        savedBanner(saved, () => { saved = null; view.refresh(); }),
        refusal ? h('div.banner.err', { key: 'refusal', role: 'alert' },
          h('span.grow', h('b', refusal.title), refusal.messages.length ? h('ul.setp-msgs', refusal.messages.map(m => h('li', m))) : null),
          refusal.reload ? button('Load the new state', () => { refusal = null; draft = null; load(); }, 'sm') : null,
          button('Dismiss', () => { refusal = null; view.refresh(); }, 'sm')) : null,
        cfg.configured === false ? banner('info', [h('b', 'No configuration has been imported yet. '),
          'There are no tiers, so no zone is used. Import a configuration file with ', h('span.mono', 'llm-scheduler import-config'), ' or add the first tier below.']) : null,
        h('div.trs-page', { key: 'page' }, tiersPanel(), listPanel('exclude'), listPanel('noauto'), overridesPanel(), zonesPanel(), saveBar()),
      ];
    };
    const view = liveView(el, render, ['status', 'conn']);
    load();
    return {
      update(r) { route = r; view.refresh(); syncPanel(); },
      unmount() { alive = false; offStatus(); if (soon) clearTimeout(soon); watch.off(); if (panel) { const p = panel; panel = null; p.close(); } view.unmount(); },
    };
  },
});
