// Tiers and nodes (Configure): the ordered tier list (first match wins), which
// tier each known node matches right now, the excluded nodes, the forced-only
// nodes (no_auto_nodes) and the node overrides. Edits collect in the pending bar; Review changes does a dry run
// and saves with If-Match and a comment.
//
// Reads: REST GET /api/config, GET /api/config/schema (scope "tier");
// WebSocket "status" (view.nodes: hostname, priority, eligible, excluded,
// auto, online) and "config". Writes: PUT /api/config/tiers, /api/config/exclude-nodes,
// /api/config/no-auto-nodes, /api/config/node-overrides (whole lists, only the ones that changed).
// Route: #/tiers.

import { h } from '../core/dom.js';
import { registerView } from '../core/router.js';
import { state } from '../core/store.js';
import { liveView, pageHead, badge, nodeChips, nodeChip, table, empty, banner, skeleton, button, dialog, schemaForm, confirm } from '../core/ui.js';
import { num, mib, plural, DASH } from '../core/format.js';
import { viewNodes } from '../core/status.js';
import { loadSchema, loadConfig, fieldsOf, clone, same } from '../core/configdata.js';
import { agentsMode } from '../core/nodeactions.js';
import { poolText } from '../core/words.js';
import { pendingBar, reviewChanges, savedBanner, watchConfig, noticeBanner } from '../core/changes.js';

// Unsaved edits survive a visit to another screen: {tiers, exclude, noauto, overrides} or null.
let draft = null;

const ORDINAL = ['1st', '2nd', '3rd'];
const ord = i => ORDINAL[i] || (i + 1) + 'th';

// A glob like the server's: * ? and [..], case-insensitive.
function globTest(pattern, s) {
  const re = '^' + String(pattern).replace(/[.+^${}()|\\]/g, '\\$&').replace(/\*/g, '.*').replace(/\?/g, '.') + '$';
  try { return new RegExp(re, 'i').test(s); } catch { return false; }
}

// Does a node (from the status view) fit a tier's rule? true, false, or null when
// the rule uses the role or the source, which the status does not carry.
function fits(node, tier) {
  const m = tier.match || {};
  if (m.hostname && !globTest(m.hostname, node.hostname || node.id)) return false;
  if (m.local != null && !!node.local !== !!m.local) return false;
  if (m.role || m.source) return null;
  return true;
}
function matchTier(node, tiers) {
  for (let i = 0; i < tiers.length; i++) {
    const f = fits(node, tiers[i]);
    if (f === true) return { index: i, certain: true };
    if (f === null) return { index: i, certain: false };
  }
  return { index: -1, certain: true };
}

function ruleSentence(t) {
  const m = t.match || {};
  const bits = [];
  if (m.hostname) bits.push(['Host name matches ', h('span.mono', m.hostname)]);
  if (m.role) bits.push(['Role is ', h('span.mono', m.role)]);
  if (m.source) bits.push(['Source is ', h('span.mono', m.source)]);
  if (m.local === true) bits.push('Only the local node');
  if (m.local === false) bits.push('Only remote nodes');
  if (!bits.length) return h('span.warn-t', 'No rule: every node that reaches this tier matches');
  return h('span', bits.map((b, i) => [i ? h('span.mut', ' and ') : null, b]));
}

// ---- the tier dialog ----

const LOCAL_CHOICES = ['any node', 'only the local node', 'only remote nodes'];
function tierDialog(fields, initial, index, isNew) {
  const fs = fields.filter(f => f.key !== 'match.local').map(f => Object.assign({}, f, {
    label: { 'match.hostname': 'Host name pattern', 'match.role': 'Role', 'match.source': 'Source', priority: 'Priority', headroom_mib: 'Headroom', shared_memory: 'Shared memory', link_mb_s: 'Link speed', settle: 'Settle time' }[f.key],
  }));
  const loc = fields.find(f => f.key === 'match.local');
  const localField = { key: 'where', type: 'enum', enum: LOCAL_CHOICES, label: 'Which nodes', description: loc ? loc.description : '', default: 'any node' };
  const order = ['match.hostname', 'match.role', 'match.source', 'where', 'priority', 'headroom_mib', 'shared_memory', 'link_mb_s', 'settle'];
  const all = [...fs, localField].sort((a, b) => order.indexOf(a.key) - order.indexOf(b.key));
  const values = clone(initial) || {};
  values.where = LOCAL_CHOICES[values.match && values.match.local === true ? 1 : values.match && values.match.local === false ? 2 : 0];
  const form = schemaForm({ fields: all, values });
  return dialog({
    title: isNew ? 'Add a tier' : `Edit tier ${index + 1}`,
    wide: true,
    body: close => h('div.col', { style: 'gap:16px' },
      h('p', 'A node joins the first tier in the list whose rule it fits. Order decides which tier a node belongs to; priority decides which tier is tried first when a model is placed (lower first).'),
      form.el,
      h('div.dlg-foot', button('Cancel', () => close(undefined), 'lg'), button(isNew ? 'Add the tier' : 'Apply to the tier', () => {
        if (!form.validate()) return;
        const v = form.values();
        const out = { match: {}, priority: v.priority };
        for (const k of ['hostname', 'role', 'source']) if (v.match && v.match[k]) out.match[k] = v.match[k];
        if (v.where === LOCAL_CHOICES[1]) out.match.local = true; else if (v.where === LOCAL_CHOICES[2]) out.match.local = false;
        if (v.headroom_mib) out.headroom_mib = v.headroom_mib;
        if (v.shared_memory) out.shared_memory = true;
        if (v.link_mb_s) out.link_mb_s = v.link_mb_s;
        if (v.settle && v.settle !== '0s') out.settle = v.settle;
        close(out);
      }, 'lg pri'))),
  });
}

function overrideDialog(nodes, initial) {
  const name = h('input', { class: 'input mono', type: 'text', value: initial ? initial.node : '', placeholder: 'node id or host name', list: 'known-nodes', autocomplete: 'off', spellcheck: 'false', 'aria-label': 'Node' });
  const rpc = h('input', { class: 'input mono', type: 'text', value: initial ? initial.rpc : '', placeholder: '192.0.2.3:50052', autocomplete: 'off', spellcheck: 'false', 'aria-label': 'RPC address' });
  const err = h('span.err', { role: 'alert' });
  return dialog({
    title: initial ? 'Edit the override' : 'Add a node override',
    body: close => h('div.col', { style: 'gap:14px' },
      h('p', 'Use this address for the rpc-server of the node instead of the one the fleet console reports.'),
      h('datalist', { id: 'known-nodes' }, nodes.map(n => h('option', { value: n.hostname || n.id }))),
      h('div.field', h('label', 'Node'), name, h('span.help', 'The node id or its host name.')),
      h('div.field', h('label', 'RPC address'), rpc, h('span.help', 'host:port of the worker, for example 192.0.2.3:50052.'), err),
      h('div.dlg-foot', button('Cancel', () => close(undefined), 'lg'), button('Apply', () => {
        const n = name.value.trim(), r = rpc.value.trim();
        if (!n) { err.textContent = 'Name the node.'; name.focus(); return; }
        if (!/^[^\s:]+:\d{1,5}$/.test(r)) { err.textContent = 'Write the address as host:port, for example 192.0.2.3:50052.'; rpc.focus(); return; }
        close({ node: n, rpc: r });
      }, 'lg pri'))),
  });
}

registerView({
  id: 'tiers', title: 'Tiers and nodes',
  mount(el) {
    let cfg = null, fields = null, error = '', saved = null, alive = true;
    const addText = { exclude: '', noauto: '' }, addErr = { exclude: '', noauto: '' };

    const rev = () => (cfg ? cfg.revision : 0);
    const savedState = () => ({ tiers: clone(cfg.tiers || []), exclude: clone(cfg.exclude_nodes || []), noauto: clone(cfg.no_auto_nodes || []), overrides: clone(cfg.node_overrides || {}) });
    const work = () => draft || savedState();
    const edit = fn => { if (!draft) draft = savedState(); fn(draft); if (!dirtyAny()) draft = null; saved = null; view.refresh(); };
    const dirtyTiers = () => !!draft && !same(draft.tiers, cfg.tiers || []);
    const dirtyExclude = () => !!draft && !same(draft.exclude, cfg.exclude_nodes || []);
    const dirtyNoAuto = () => !!draft && !same(draft.noauto, cfg.no_auto_nodes || []);
    const dirtyOverrides = () => !!draft && !same(draft.overrides, cfg.node_overrides || {});
    const dirtyAny = () => dirtyTiers() || dirtyExclude() || dirtyNoAuto() || dirtyOverrides();
    const pendingCount = () => {
      if (!cfg || !draft) return 0;
      const e0 = new Set(cfg.exclude_nodes || []), e1 = new Set(draft.exclude);
      const exN = [...e1].filter(x => !e0.has(x)).length + [...e0].filter(x => !e1.has(x)).length;
      const n0 = new Set(cfg.no_auto_nodes || []), n1 = new Set(draft.noauto);
      const naN = [...n1].filter(x => !n0.has(x)).length + [...n0].filter(x => !n1.has(x)).length;
      const o0 = cfg.node_overrides || {}, o1 = draft.overrides;
      const ovN = new Set([...Object.keys(o0), ...Object.keys(o1)]);
      let ov = 0; for (const k of ovN) if (!same(o0[k], o1[k])) ov++;
      return (dirtyTiers() ? 1 : 0) + exN + naN + ov;
    };

    async function load() {
      try {
        const [c, f] = await Promise.all([loadConfig(), loadSchema()]);
        cfg = c; fields = fieldsOf(f, 'tier'); error = '';
        if (draft && !dirtyAny()) draft = null;
      } catch (e) { error = e.message; }
      if (alive) view.refresh();
    }
    const watch = watchConfig({ revision: rev, pending: () => !!draft, refresh: load, redraw: () => view.refresh() });

    const buildOps = () => {
      const ops = [];
      if (dirtyTiers()) ops.push({ label: 'Tier list', method: 'PUT', path: '/api/config/tiers', body: { tiers: draft.tiers },
        summary: h('div.col', { style: 'gap:4px' }, h('span.mut', 'The tier list would be, in the order it is checked:'),
          draft.tiers.map((t, i) => h('span', h('span.mono', (i + 1) + '. '), ruleSentence(t), h('span.mut', ' · priority ' + (t.priority ?? DASH))))) });
      if (dirtyExclude()) ops.push({ label: 'Excluded nodes', method: 'PUT', path: '/api/config/exclude-nodes', body: { exclude_nodes: draft.exclude }, count: Math.max(1, Math.abs(draft.exclude.length - (cfg.exclude_nodes || []).length)) });
      if (dirtyNoAuto()) ops.push({ label: 'Forced-only nodes', method: 'PUT', path: '/api/config/no-auto-nodes', body: { no_auto_nodes: draft.noauto }, count: Math.max(1, Math.abs(draft.noauto.length - (cfg.no_auto_nodes || []).length)) });
      if (dirtyOverrides()) ops.push({ label: 'Node overrides', method: 'PUT', path: '/api/config/node-overrides', body: { node_overrides: draft.overrides } });
      return ops;
    };
    async function review() {
      if (!draft) return;
      const r = await reviewChanges({ title: 'Review tier and node changes', getRevision: rev, buildOps, reload: async () => { await load(); } });
      if (r && r.saved) { draft = null; saved = r; watch.clear(); }
      await load();
    }
    const discard = async () => {
      if (await confirm({ title: 'Discard unsaved changes?', intro: 'The tier list, excluded nodes, forced-only nodes and overrides go back to what is saved. Nothing on the server changes.', confirmLabel: 'Discard', danger: true })) {
        draft = null; view.refresh();
      }
    };

    // ---- tier actions ----
    const move = (i, d) => edit(w => { const t = w.tiers; const j = i + d; if (j < 0 || j >= t.length) return; [t[i], t[j]] = [t[j], t[i]]; });
    async function editTier(i) {
      const isNew = i == null;
      const v = await tierDialog(fields, isNew ? { priority: Math.max(0, ...work().tiers.map(t => t.priority || 0)) + 1 } : work().tiers[i], i, isNew);
      if (!v) return;
      edit(w => { if (isNew) w.tiers.push(v); else w.tiers[i] = v; });
    }
    async function removeTier(i) {
      const t = work().tiers[i];
      const live = viewNodes(state.status).filter(n => matchTier(n, work().tiers).index === i);
      const ok = await confirm({
        title: `Remove tier ${i + 1}?`, intro: 'The tier leaves the list. It is not deleted from the server until you save.',
        hits: [{ kind: live.length ? 'x' : '', text: live.length ? ['These nodes match only this tier and would never be used: ', nodeChips(live.map(n => n.hostname || n.id))] : 'No known node matches this tier now.' },
          { kind: '', text: ['Rule: ', ruleSentence(t)] }],
        confirmLabel: 'Remove from the list', danger: true,
      });
      if (ok) edit(w => { w.tiers.splice(i, 1); });
    }

    // ---- overrides ----
    async function editOverride(node) {
      const cur = node ? { node, rpc: (work().overrides[node] || {}).rpc || '' } : null;
      const v = await overrideDialog(viewNodes(state.status), cur);
      if (!v) return;
      edit(w => { if (node && node !== v.node) delete w.overrides[node]; w.overrides[v.node] = { rpc: v.rpc }; });
    }

    // ---- drawing ----
    function tierCard(t, i, total, nodes) {
      const mine = nodes.filter(n => matchTier(n, work().tiers).index === i);
      const changed = dirtyTiers() && !(cfg.tiers || []).some(o => same(o, t));
      const facts = [
        ['Headroom', t.headroom_mib ? mib(t.headroom_mib) : DASH, 'Memory kept free on a node of this tier'],
        ['Memory', t.shared_memory ? 'shared with the system' : 'dedicated', 'Whether the GPU memory is system RAM', false],
        ['Link', t.link_mb_s ? num(t.link_mb_s, 0) + ' MB/s' : DASH, 'Used to size load timeouts'],
        ['Settle', t.settle || DASH, 'How long a node is unusable after an instance releases it'],
      ];
      return h('div', { key: 'tier-' + i + '-' + JSON.stringify(t), style: 'display:grid;grid-template-columns:56px minmax(0,1fr);gap:8px 16px;padding:16px 20px;border-top:1px solid var(--line)' },
        h('div', { style: 'display:flex;flex-direction:column;align-items:center;gap:2px', title: 'Tiers are checked in this order' },
          h('b.mono', { style: 'font-size:var(--fs-xl);font-weight:500;line-height:1.1' }, String(i + 1)), h('span.mut', { style: 'font-size:var(--fs-xs);font-weight:700' }, ord(i) + ' check')),
        h('div.col', { style: 'gap:8px;min-width:0' },
          h('div.row', { style: 'gap:8px 12px' }, h('span', { style: 'font-weight:700' }, ruleSentence(t)),
            badge('Priority ' + (t.priority ?? DASH), 'line', 'Models are placed on a tier with a lower priority number first'), changed ? badge('Edited', 'q') : null),
          h('div.row', { style: 'gap:4px 22px;font-size:var(--fs-md)' }, facts.map(([k, v, tip, mono]) => h('span', { title: tip }, h('span.mut', { style: 'margin-right:6px' }, k), mono === false ? v : h('span.mono', v)))),
          h('div.row', { style: 'gap:6px 10px' }, h('span.mut', { style: 'font-weight:600;font-size:var(--fs-md)' }, mine.length ? 'Nodes in this tier now:' : 'No known node is in this tier now'),
            mine.length ? nodeChips(mine.map(n => n.hostname || n.id)) : null),
          h('div.row', { style: 'gap:8px' },
            button('↑ Move up', () => move(i, -1), 'sm', { disabled: i === 0, 'aria-label': `Move tier ${i + 1} up` }),
            button('↓ Move down', () => move(i, 1), 'sm', { disabled: i === total - 1, 'aria-label': `Move tier ${i + 1} down` }),
            button('Edit', () => editTier(i), 'sm'), button('Remove', () => removeTier(i), 'sm dng'))));
    }

    function tiersCard(nodes) {
      const tiers = work().tiers;
      return h('section.card', { key: 'tiers' },
        h('div.ch', h('h2', 'Tiers'), h('span.note', 'Checked from the top. A node joins the first tier it fits; a node that fits none is never used.'),
          h('span.right', button('Add a tier', () => editTier(null), 'sm'))),
        h('div.cb', { style: 'padding-bottom:12px' }, h('span.mut', 'Order decides which tier a node belongs to. Priority decides which tier a model is placed on first: a lower number is preferred.')),
        tiers.length ? tiers.map((t, i) => tierCard(t, i, tiers.length, nodes)) : empty('No tiers', 'With no tier, no node is used and no model can be placed. Add a tier to bring nodes in.'));
    }

    function nodesCard(nodes) {
      const tiers = work().tiers, savedTiers = cfg.tiers || [];
      const dirty = dirtyTiers();
      const nodeTier = n => {
        // what the live status says: the tier whose priority the node carries
        const p = n.priority;
        const idx = p ? savedTiers.findIndex(t => t.priority === p) : -1;
        return idx >= 0 ? h('span', h('span.mono', 'Tier ' + (idx + 1)), h('span.sub', 'priority ' + p)) : h('span.mut', 'No tier');
      };
      const columns = [
        { label: 'Node', render: n => nodeChip(n.hostname || n.id) },
        { label: 'Tier now', render: nodeTier },
        dirty ? { label: 'With your unsaved tiers', render: n => { const m = matchTier(n, tiers); return m.index < 0 ? h('span.err-t', 'No tier: never used') : h('span', h('span.mono', 'Tier ' + (m.index + 1)), m.certain ? null : h('span.sub', 'depends on role or source')); } } : null,
        { label: 'Eligible', cls: 'tight', title: 'Eligible nodes can receive a model now', render: n => (n.eligible ? badge('Eligible', 'use') : badge('Not eligible', 'n', n.excluded ? 'Excluded by configuration' : !n.online ? 'Offline' : 'No tier matches, or the node is held back')) },
        { label: 'Excluded', cls: 'tight', render: n => (n.excluded ? badge('Excluded', 'dash', 'An excluded node never receives a model') : h('span.mut', 'No')) },
        { label: 'Automatic', cls: 'tight', render: n => (n.auto === false ? badge('Forced only', 'line', 'Only a profile that forces this node uses it') : h('span.mut', 'Yes')) },
        { label: 'Online', cls: 'tight', render: n => (n.online ? h('span.ok-t', 'Online') : h('span.err-t', 'Offline')) },
      ].filter(Boolean);
      return h('section.card', { key: 'nodes' },
        h('div.ch', h('h2', 'Which tier each node matches'), h('span.note', poolText(agentsMode(state.status), 'From the live fleet view. Role and source rules cannot be checked here.'))),
        nodes.length ? table({ columns, rows: nodes, key: n => n.id, minWidth: 720 })
          : empty('No nodes known yet', agentsMode(state.status) ? 'The scheduler has not heard of any node. Machines appear here when their agent registers.'
            : 'The scheduler has not heard of any node. When the fleet feed connects, they appear here.'));
    }

    // The two node lists: exclude_nodes and no_auto_nodes. Same entries
    // (node id, host name or pattern); only what they do differs.
    const LISTS = {
      exclude: { key: 'exclude_nodes', title: 'Excluded nodes', note: 'An excluded node never receives a model. Excluding wins over a profile that forces the node.',
        none: 'No node is excluded.', verb: 'Exclude', already: 'is already excluded.', remove: 'Stop excluding ', input: 'Node to exclude' },
      noauto: { key: 'no_auto_nodes', title: 'Forced only', note: 'A node here gets a model only from a profile that names it in force_nodes. Automatic placement never picks it and never evicts what runs on it.',
        none: 'Every node can be picked automatically.', verb: 'Forced only', already: 'is already forced only.', remove: 'Make automatic again: ', input: 'Node to keep for forced profiles' },
    };
    function addTo(which) {
      const L = LISTS[which], v = (addText[which] || '').trim();
      if (!v) { addErr[which] = 'Write a node id, a host name or a pattern such as ps5-test*.'; view.refresh(); return; }
      if (work()[which].includes(v)) { addErr[which] = v + ' ' + L.already; view.refresh(); return; }
      addErr[which] = ''; addText[which] = '';
      edit(w => { w[which].push(v); });
    }
    function listCard(nodes, which) {
      const L = LISTS[which], list = work()[which];
      const known = nodes.map(n => n.hostname || n.id).filter(n => !list.includes(n));
      const e0 = cfg[L.key] || [];
      return h('section.card', { key: which },
        h('div.ch', h('h2', L.title), h('span.note', L.note)),
        h('div.cb', { style: 'display:flex;flex-direction:column;gap:12px' },
          list.length ? h('div.tags', { style: 'border:0;background:none;padding:0' }, list.map(x => h('span.node', { key: which + '-' + x }, x, e0.includes(x) ? null : h('span', { class: 'b b-q', style: 'margin-left:6px;height:18px' }, 'new'),
            h('button', { type: 'button', 'aria-label': L.remove + x, onclick: () => edit(w => { w[which] = w[which].filter(y => y !== x); }) }, '×'))))
            : h('span.mut', L.none),
          h('div.row', { style: 'gap:8px' },
            h('input', { class: 'input mono', style: 'max-width:340px', type: 'text', value: addText[which] || '', placeholder: 'Node id, name or pattern', 'aria-label': L.input, autocomplete: 'off', spellcheck: 'false',
              oninput: e => { addText[which] = e.target.value; }, onkeydown: e => { if (e.key === 'Enter') addTo(which); } }),
            button(L.verb, () => addTo(which), 'sm')),
          addErr[which] ? h('span.err-t', { role: 'alert' }, addErr[which]) : null,
          known.length ? h('div.row', { style: 'gap:6px 8px' }, h('span.mut', { style: 'font-size:var(--fs-sm);font-weight:600' }, 'Known nodes:'),
            known.map(k => h('button.fc', { key: which + '-k-' + k, type: 'button', style: 'height:26px;font-family:var(--mono);font-size:var(--fs-sm)', onclick: () => edit(w => { w[which].push(k); }) }, '+ ' + k))) : null));
    }

    function overridesCard() {
      const o = work().overrides, names = Object.keys(o).sort();
      const o0 = cfg.node_overrides || {};
      return h('section.card', { key: 'overrides' },
        h('div.ch', h('h2', 'Node overrides'), agentsMode(state.status) ? badge('Not used in this mode', 'dash', 'Agents report their own addresses; node overrides apply only with membership fleet.') : null,
          h('span.note', 'Use a fixed rpc-server address for a node instead of the one the fleet console reports.'),
          h('span.right', button('Add an override', () => editOverride(null), 'sm'))),
        names.length ? table({
          minWidth: 560, rows: names, key: n => n,
          columns: [
            { label: 'Node', render: n => h('span', nodeChip(n), same(o[n], o0[n]) ? null : [' ', badge('Edited', 'q')]) },
            { label: 'RPC address', render: n => h('span.mono', (o[n] || {}).rpc || DASH) },
            { label: '', cls: 'tight', render: n => h('span.row', { style: 'gap:6px;justify-content:flex-end;flex-wrap:nowrap' }, button('Edit', () => editOverride(n), 'sm'),
              button('Remove', () => edit(w => { delete w.overrides[n]; }), 'sm dng')) },
          ],
        }) : empty('No overrides', 'Every node uses the address the fleet console reports.'));
    }

    const render = () => {
      const nodes = viewNodes(state.status);
      const head = pageHead('Tiers and nodes', cfg ? [h('b', num(work().tiers.length)), work().tiers.length === 1 ? ' tier' : ' tiers', ' · ', h('b', num(work().exclude.length)), ' excluded',
        pendingCount() ? [' · ', h('b', num(pendingCount())), ' unsaved'] : null] : null);
      if (error && !cfg) return [head, banner('err', ['The configuration could not be read: ', error])];
      if (!cfg || !fields) return [head, h('section.card', skeleton(5))];
      return [
        head,
        noticeBanner(watch.notice, { onReload: () => { watch.clear(); load(); }, onDismiss: () => { watch.clear(); view.refresh(); } }),
        savedBanner(saved, () => { saved = null; view.refresh(); }),
        cfg.configured === false ? banner('info', [h('b', 'No configuration has been imported yet. '),
          'There are no tiers, so no node is used. Import a configuration file with ', h('span.mono', 'llm-scheduler import-config'), ' or add the first tier below.']) : null,
        h('div.cols', { key: 'cols' }, h('div.wide', tiersCard(nodes), nodesCard(nodes)), h('div.side', listCard(nodes, 'exclude'), listCard(nodes, 'noauto'), overridesCard())),
        pendingBar({ count: pendingCount(), what: 'tiers and nodes', onReview: review, onDiscard: discard }),
      ];
    };
    const view = liveView(el, render, ['status', 'conn']);
    load();
    return { unmount() { alive = false; watch.off(); view.unmount(); } };
  },
});
