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
//   - "Zones and their tier": which tier each known zone is in now, and
//     with the unsaved tier list;
//   - ONE save bar ("N changes · Save"), and ONE save dialog: a dry run of
//     every part first (a refusal shows the server's sentence and saves
//     nothing), then the list of changes and the required comment, then
//     the writes with If-Match.
//
// Reads: REST GET /api/config, GET /api/config/schema (scope "tier");
// WebSocket "status" (view.nodes: hostname, priority, eligible, excluded,
// auto, online, local) and "config".
// Writes: PUT /api/config/tiers, /api/config/exclude-nodes,
// /api/config/no-auto-nodes, /api/config/node-overrides (whole lists, only
// the ones that changed), each with a dry run first, If-Match and the
// comment (X-Comment). Signed by the default user.
// Route: #/tiers?tier=<n>.

import { h } from '../core/dom.js';
import { registerView, navigate } from '../core/router.js';
import { state } from '../core/store.js';
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

// A glob like the server's: * ? and [..], case-insensitive.
export function globTest(pattern, s) {
  const re = '^' + String(pattern).replace(/[.+^${}()|\\]/g, '\\$&').replace(/\*/g, '.*').replace(/\?/g, '.') + '$';
  try { return new RegExp(re, 'i').test(s); } catch { return false; }
}

// Does a zone (from the status view) fit a tier's rule? true, false, or null
// when the rule uses what the status does not carry (role, source, platform,
// device).
function fits(node, tier) {
  const m = tier.match || {};
  if (m.hostname && !globTest(m.hostname, node.hostname || node.id)) return false;
  if (m.local != null && !!node.local !== !!m.local) return false;
  if (m.role || m.source || m.platform || m.device) return null;
  return true;
}
export function matchTier(node, tiers) {
  for (let i = 0; i < tiers.length; i++) {
    const f = fits(node, tiers[i]);
    if (f === true) return { index: i, certain: true };
    if (f === null) return { index: i, certain: false };
  }
  return { index: -1, certain: true };
}
const zname = n => n.hostname || n.id;

// The rule in plain words.
export function ruleText(t) {
  const m = (t && t.match) || {};
  const bits = [];
  if (m.hostname) bits.push('host name matches ' + m.hostname);
  if (m.role) bits.push('role is ' + m.role);
  if (m.source) bits.push('source is ' + m.source);
  if (m.platform) bits.push('platform is ' + m.platform);
  if (m.device) bits.push('a device matches ' + m.device);
  if (m.local === true) bits.push('only the local zone');
  if (m.local === false) bits.push('only remote zones');
  if (!bits.length) return 'Every zone (no rule)';
  const s = bits.join(' and ');
  return s[0].toUpperCase() + s.slice(1);
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
      h('p.mut', 'Use this address for the rpc-server of the zone instead of the one the fleet console reports.'),
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

    async function load() {
      try {
        const [c, f] = await Promise.all([loadConfig(), loadSchema()]);
        cfg = c; fields = fieldsOf(f, 'tier'); error = '';
        if (draft && !dirtyAny()) draft = null;
      } catch (e) { error = e.message; }
      if (alive) { view.refresh(); syncPanel(); }
    }
    const watch = watchConfig({ revision: rev, pending: () => !!draft, refresh: load, redraw: () => view.refresh() });

    const ops = () => [
      dirty.tiers() ? { what: 'the tier list', path: '/api/config/tiers', body: { tiers: draft.tiers } } : null,
      dirty.exclude() ? { what: 'the excluded zones', path: '/api/config/exclude-nodes', body: { exclude_nodes: draft.exclude } } : null,
      dirty.noauto() ? { what: 'the named-only zones', path: '/api/config/no-auto-nodes', body: { no_auto_nodes: draft.noauto } } : null,
      dirty.overrides() ? { what: 'the node overrides', path: '/api/config/node-overrides', body: { node_overrides: draft.overrides } } : null,
    ].filter(Boolean);

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
          warnings.length ? banner('warn', h('span', h('b', 'Check this first. '), warnings.join(' '))) : null),
        comment: { label: 'Comment', required: true, placeholder: 'Why are you changing this? It is kept in the audit log.' },
        actions: [{ label: 'Cancel', value: false }, { label: 'Save ' + plural(lines.length, 'change'), kind: 'pri', value: true }],
      });
      if (!ans || ans.value !== true) return;
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
          { kind: mine.length ? 'x' : '', text: mine.length ? ['These zones are in this tier now and would join the next tier that fits, or none: ', nodeChips(mine.map(zname))] : 'No known zone is in this tier now.' }],
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

    // the zones in tier i now: by the rule when it can be checked here, else
    // by the priority the server gives the zone
    function zonesOf(i) {
      const tiers = work().tiers, t = tiers[i];
      return viewNodes(state.status).filter(n => {
        const m = matchTier(n, tiers);
        if (m.certain) return m.index === i;
        return m.index <= i && t && n.priority === t.priority;
      });
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
      panel = openPanel({
        label: 'Tier', name: 'Tier ' + want,
        title: () => 'Tier ' + panelFor + ' · ' + plural(zonesOf(idx()).length, 'zone'),
        sub: () => ruleText(tier()),
        body: () => {
          const t = tier(), f = facts(t), mine = zonesOf(idx()), n = work().tiers.length;
          return [
            h('div.trs-facts', { key: 'facts' },
              [['Checked', ordinal(idx()) + ' of ' + n], ['Priority', f.priority], ['Headroom', f.headroom], ['Memory', f.memory === 'Shared' ? 'Shared with the system' : 'Dedicated'], ['Link speed', f.link], ['Settle time', f.settle]]
                .map(([k, v]) => h('div', h('span.lbl', k), h('b', v)))),
            h('h3.trs-sub', { key: 'zh' }, 'Zones in this tier now'),
            mine.length ? h('div.trs-zones', { key: 'zones' }, nodeChips(mine.map(zname))) : h('p.mut', { key: 'none' }, 'No known zone is in this tier now.'),
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
              const f = facts(t), mine = zonesOf(i), n = String(i + 1);
              const edited = dirty.tiers() && !t0.some(o => same(o, t));
              const openRow = () => go({ tier: n });
              return h('tr', { key: 'tier-' + i, class: 'click' + (open === n ? ' sel' : ''), tabindex: '0', 'data-tier': n, 'aria-selected': open === n ? 'true' : null,
                onclick: e => { if (!e.target.closest('button')) openRow(); }, onkeydown: e => { if (e.target === e.currentTarget && (e.key === 'Enter' || e.key === ' ')) { e.preventDefault(); openRow(); } } },
                h('td.r', n),
                h('td.trs-rule', h('span', { class: (t.match && Object.keys(t.match).length) ? null : 'mut' }, ruleText(t)), edited ? [' ', badge('Edited', 'q')] : null),
                h('td.r', f.priority), h('td.r', f.headroom), h('td', f.memory), h('td.r', f.link), h('td.r', f.settle),
                h('td.r', { 'data-zones': '', title: mine.length ? mine.map(zname).join(', ') : 'No known zone is in this tier now' }, num(mine.length)),
                h('td.trs-mv',
                  h('button.trs-ib', { type: 'button', 'aria-label': 'Move tier ' + n + ' up', title: 'Move up', disabled: i === 0, 'data-mv': 'up', onclick: () => move(i, -1) }, '↑'),
                  h('button.trs-ib', { type: 'button', 'aria-label': 'Move tier ' + n + ' down', title: 'Move down', disabled: i === tiers.length - 1, 'data-mv': 'down', onclick: () => move(i, 1) }, '↓')));
            })))));
    }

    function addTo(which) {
      const L = LISTS[which], v = (addText[which] || '').trim();
      if (!v) { addErr[which] = 'Write a zone id, a host name or a pattern such as ps5-test*.'; view.refresh(); return; }
      if (work()[which].includes(v)) { addErr[which] = v + ' ' + L.already; view.refresh(); return; }
      addErr[which] = ''; addText[which] = '';
      edit(w => { w[which].push(v); });
    }
    function listPanel(which) {
      const L = LISTS[which], list = work()[which], e0 = cfg[L.key] || [];
      const known = viewNodes(state.status).map(zname).filter(n => !list.includes(n));
      return h('section.panel.trs', { key: which, 'data-list': which },
        h('div.ch', h('h2', L.title), h('span.note', plural(list.length, 'zone'))),
        h('p.trs-help', L.help),
        list.length ? h('ul.trs-list', list.map(x => h('li', { key: which + '-' + x },
          h('span.mono.grow', x), e0.includes(x) ? null : badge('Unsaved', 'q'),
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
          : 'Use a fixed rpc-server address for a zone instead of the one the fleet console reports.'),
        names.length ? h('ul.trs-list', names.map(n => h('li', { key: 'ov-' + n },
          h('span.mono', n), h('span.mut', '→'), h('span.mono.grow', (o[n] || {}).rpc || DASH), same(o[n], o0[n]) ? null : badge('Unsaved', 'q'),
          agents ? null : button('Edit', () => editOverride(n), 'sm'),
          button('Remove', () => edit(w => { delete w.overrides[n]; }), 'sm', { 'aria-label': 'Remove the override of ' + n }))))
          : agents ? null : h('p.trs-none.mut', 'No overrides: every zone uses the address the fleet console reports.'));
    }

    function zonesPanel() {
      const nodes = viewNodes(state.status), tiers = work().tiers, t0 = cfg.tiers || [];
      const now = n => { const p = n.priority; const i = p ? t0.findIndex(t => t.priority === p) : -1; return i >= 0 ? 'Tier ' + (i + 1) : null; };
      return h('section.panel.trs', { key: 'zones' },
        h('div.ch', h('h2', 'Zones and their tier'), h('span.note', plural(nodes.length, 'zone'))),
        h('p.trs-help', 'From the live fleet view. Rules on role, source, platform or device cannot be checked here: those zones show the tier the scheduler reports.'),
        !nodes.length ? h('p.trs-none.mut', 'No zone is known yet. Machines appear here when their agent registers.')
          : h('div.trs-scroll', h('table.tbl.trs-ztbl',
            h('thead', h('tr', h('th', { scope: 'col' }, 'Zone'), h('th', { scope: 'col' }, 'Tier now'),
              dirty.tiers() ? h('th', { scope: 'col' }, 'With your unsaved tiers') : null,
              h('th', { scope: 'col' }, 'Takes models'), h('th', { scope: 'col' }, 'Online'))),
            h('tbody', nodes.map(n => {
              const m = matchTier(n, tiers);
              return h('tr', { key: 'z-' + n.id },
                h('td.mono', zname(n)),
                h('td', now(n) || h('span.mut', 'No tier')),
                dirty.tiers() ? h('td', m.index < 0 ? h('span.err-t', 'No tier: never used') : ['Tier ' + (m.index + 1), m.certain ? null : h('span.sub', 'depends on a rule not checked here')]) : null,
                h('td', n.excluded ? badge('Excluded', 'dash', 'exclude_nodes') : n.auto === false ? badge('Named only', 'n', 'no_auto_nodes') : n.eligible ? 'Yes' : h('span.mut', 'Not now')),
                h('td', n.online ? 'Online' : h('span.err-t', 'Offline')));
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
      unmount() { alive = false; watch.off(); if (panel) { const p = panel; panel = null; p.close(); } view.unmount(); },
    };
  },
});
