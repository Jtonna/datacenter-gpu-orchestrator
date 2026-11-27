// MCP access keys (design: boards MCP, MCP-New, MCP-Created, MCP-Manage;
// the mockup is the requirement). Replaced the classic page (the classic UI
// is removed; its MCP tool list, GET /api/mcp/tools, is the Tools section
// below the keys, which the boards do not draw).
//
//   - the list: Access keys · N active, New key; Name, Key, Can, Expires,
//     Last used, Status, Manage (Key and Last used drop out when the docked
//     panel leaves the list narrow, as the three panel boards draw it);
//   - New key in the side panel: name, expiry (30 days / 90 days / Custom
//     date / Never), the four permissions with their levels; Create key;
//   - Created: the key "shown once" with Copy, the settings snippet with
//     the key in it, Copy; Manage access / Done;
//   - Manage in the docked panel: rename, expiry, permissions, the snippet
//     with the placeholder (only a hash of the key is stored), Rotate key,
//     Revoke key, Save; status, last used and the usage of the last 7 days.
//
// Every write asks for a comment (sent as X-Comment; the audit log keeps it)
// and is signed by the default user: the page sends no key and no actor.
// The server's sentence is shown on a 400 or 409.
//
// The full key: only in the Created / Rotated panel, held in that panel's
// closure (never in the address, the view's state, a toast or the developer tools),
// and dropped when the panel closes or turns into Manage.
//
// Reads: REST GET /api/agents; WebSocket "config" (another tab changed a
// key: read the list again).
// Writes: POST /api/agents, PATCH /api/agents/{id}, POST
// /api/agents/{id}/rotate, DELETE /api/agents/{id}.
// Route: #/mcp?key=<id> (Manage) or ?new=1 (New key)

import { h } from '../core/dom.js';
import { registerView, navigate } from '../core/router.js';
import { listen } from '../core/store.js';
import * as api from '../core/api.js';
import { liveView, empty, skeleton, banner, segmented, dialog, toast } from '../core/ui.js';
import { openPanel, openPanelNow } from '../core/panel.js';
import { num, plural } from '../core/format.js';

const DAY = 24 * 3600 * 1000;
const PERMS = ['observe', 'zones', 'models', 'settings'];
const LABEL = { observe: 'Observe', zones: 'Switch zones on or off', models: 'Load and unload models', settings: 'Change settings' };
const HINT = { observe: 'zones, runs, tokens' };
const LEVEL = { allow: 'Allow', ask: 'Ask me', off: 'Off' };
const EXPIRY = [{ id: '30', label: '30 days' }, { id: '90', label: '90 days' }, { id: 'custom', label: 'Custom date' }, { id: 'never', label: 'Never' }];

// dates as the boards write them: "17 Dec 2026", "17 Nov, 14:02", "9 Nov"
const dateLong = ms => new Date(ms).toLocaleDateString('en-GB', { day: 'numeric', month: 'short', year: 'numeric' });
const dateShort = ms => new Date(ms).toLocaleDateString('en-GB', { day: 'numeric', month: 'short' });
// stamp: the time too when it is within a week ("17 Nov, 14:02"), else the day ("28 Oct")
const stamp = ms => (Date.now() - ms < 7 * DAY ? dateShort(ms) + ', ' + new Date(ms).toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit' }) : dateShort(ms));
const full = ms => new Date(ms).toLocaleString('en-GB', { day: 'numeric', month: 'short', year: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit', timeZoneName: 'short' });
const isoDay = ms => { const d = new Date(ms); return d.getFullYear() + '-' + String(d.getMonth() + 1).padStart(2, '0') + '-' + String(d.getDate()).padStart(2, '0'); };

// statusTag(a) -> the Status pill: Active, Expires in N days, Expired, Revoked 9 Nov
export function statusTag(a) {
  if (a.status === 'revoked') return h('span.ktag.t-gone', a.revoked_at ? 'Revoked ' + dateShort(a.revoked_at) : 'Revoked');
  if (a.status === 'expired') return h('span.ktag.t-gone', 'Expired');
  if (a.status === 'expiring') return h('span.ktag.t-soon', a.expires_in_days === 1 ? 'Expires tomorrow' : 'Expires in ' + a.expires_in_days + ' days');
  return h('span.ktag.t-ok', 'Active');
}
const gone = a => a.status === 'expired' || a.status === 'revoked';
const expiresText = a => (a.status === 'revoked' ? '–' : a.expires_at ? dateLong(a.expires_at) : 'Never');

// usageLine(a) -> "1,284 requests in 7 days, 3 refused" (Ask me refusals are counted in refused)
export function usageLine(a) {
  const u = a.usage || {};
  if (!u.requests && !u.denied) return 'not used in the last ' + (u.days || 7) + ' days';
  return [plural(u.requests || 0, 'request') + ' in ' + (u.days || 7) + ' days', u.denied ? num(u.denied) + ' refused' : null].filter(Boolean).join(', ');
}

// snippetText(s) -> the settings as the boards write them: the server entry's
// fields one per line, the rest closed on the first and last lines (valid JSON)
export function snippetText(s) {
  const servers = s && s.mcpServers;
  const names = servers ? Object.keys(servers) : [];
  if (names.length !== 1 || typeof servers[names[0]] !== 'object') return JSON.stringify(s || {}, null, 2);
  const one = servers[names[0]];
  const lines = Object.keys(one).map(k => '    ' + JSON.stringify(k) + ': ' + JSON.stringify(one[k]).replace(/^\{"/, '{ "').replace(/"\}$/, '" }').replace(/":"/g, '": "').replace(/","/g, '", "'));
  return '{ "mcpServers": { ' + JSON.stringify(names[0]) + ': {\n' + lines.join(',\n') + '\n} } }';
}

// copy(text, what): to the clipboard; never echoes the text (it may be a key)
async function copy(text, what) {
  try { await navigator.clipboard.writeText(text); toast('Copied ' + what, 'ok'); } catch { toast('The browser did not allow copying: select the text and copy it by hand.', 'err'); }
}

// expiryAt(choice, custom) -> unix ms, 0 for never, null when nothing is chosen
function expiryAt(choice, custom) {
  if (choice === '30') return Date.now() + 30 * DAY;
  if (choice === '90') return Date.now() + 90 * DAY;
  if (choice === 'never') return 0;
  if (choice === 'custom' && custom) { const [y, m, d] = custom.split('-').map(Number); return new Date(y, m - 1, d, 23, 59, 0).getTime(); }
  return null;
}

// askComment(title, lines, label) -> the comment, or undefined when closed
async function askComment(title, lines, label, danger) {
  const r = await dialog({ title, body: h('div.col', { style: 'gap:10px' }, lines.map(l => (typeof l === 'string' ? h('p', l) : l))),
    comment: { label: 'Comment', required: true, placeholder: 'Why? It is kept in the audit log with the change.' },
    actions: [{ label: 'Cancel', value: undefined }, { label, kind: danger ? 'dng solid' : 'pri', value: true }] });
  return r && r.value ? r.comment : undefined;
}

// toolsPart(doc, error) -> "Tools": every MCP tool an agent can call, by
// group, with what it does and the permission it needs. Read-only. Not on
// the boards: it keeps what the classic MCP page listed.
function toolsPart(doc, error) {
  const head = h('div.mcp-head', h('h2', 'Tools'), doc ? h('span.mut', `${plural((doc.tools || []).length, 'tool')} an agent can call` + (doc.server && doc.server.version ? ` · version ${doc.server.version}` : '')) : null);
  if (!doc) return h('section.w.mcp.mcp-tools', { key: 'tools' }, head, error ? h('p.mut.mcp-pad', 'The tool list could not be read: ' + error) : skeleton(3));
  const all = doc.tools || [];
  return h('section.w.mcp.mcp-tools', { key: 'tools', 'aria-label': 'Tools' }, head,
    (doc.groups || []).map(g => {
      const mine = all.filter(t => t.group === g.id);
      if (!mine.length) return null;
      return h('details.mcp-tg', { key: 'g-' + g.id, 'data-group': g.id },
        h('summary', h('b', g.title), h('span.ktag.t-gone', num(mine.length)), h('span.mut', g.note)),
        h('table.mcp-tbl.mcp-ttbl', h('thead', h('tr', h('th', { scope: 'col' }, 'Tool'), h('th', { scope: 'col' }, 'What it does'), h('th', { scope: 'col' }, 'Needs'))),
          h('tbody', mine.map(t => h('tr', { key: t.name, 'data-tool': t.name },
            h('td.mono', t.name),
            h('td', { title: t.description || null }, t.summary || t.description || '', t.destructive ? h('span.ktag.t-soon.mcp-dz', { title: 'The tool replaces or deletes something. Configuration changes can be rolled back.' }, 'Changes or removes') : null),
            h('td', LABEL[t.scope] || t.scope || '–'))))));
    }));
}

registerView({
  id: 'mcp', title: 'MCP',
  mount(el, route0) {
    let route = route0, data = null, error = '', alive = true, fresh = '';
    let tools = null, toolsError = ''; // GET /api/mcp/tools: what an agent can do (read-only; not on the boards)
    let panel = null, panelFor = '', shown = null; // shown: {id, key, settings, rotated} while the Created/Rotated panel is open

    async function load() {
      try { data = await api.get('/api/agents'); error = ''; } catch (e) { error = e.message; }
      if (!alive) return;
      view.refresh(); syncPanel(true);
    }
    const offConfig = listen('config', ev => { if (!ev || !ev.section || ev.section === 'agents') load(); });
    api.get('/api/mcp/tools').then(t => { tools = t; toolsError = ''; }, e => { toolsError = e.message; }).then(() => { if (alive) view.refresh(); });
    const agentOf = id => ((data && data.agents) || []).find(a => a.id === id);
    const go = q => navigate('mcp', [], q, true);

    // ---- the side panel ----
    function closePanel() { if (panel) { const p = panel; panel = null; panelFor = ''; shown = null; p.close(); } }
    function syncPanel(redraw) {
      const want = route.query.new === '1' ? 'new' : route.query.key ? 'key:' + route.query.key : '';
      if (!want) { closePanel(); return; }
      if (!data) return;
      if (shown && want === 'key:' + shown.id) return; // the Created / Rotated panel stays until it is closed
      if (panel && panelFor === want && openPanelNow() === panel) { if (redraw) panel.redraw(); return; }
      shown = null;
      if (want === 'new') openNew();
      else openKey(route.query.key);
    }
    const onClosed = which => () => { if (panelFor === which) { panel = null; panelFor = ''; shown = null; if (route.query.new || route.query.key) go({}); } };

    // New key
    function openNew() {
      const f = { name: '', exp: '30', custom: '', perms: { observe: 'allow', zones: 'off', models: 'off', settings: 'off' }, err: '', busy: false };
      const redraw = () => panel && panel.redraw();
      async function create() {
        const at = expiryAt(f.exp, f.custom);
        if (!f.name.trim()) { f.err = 'The key needs a name: it is what the audit log shows.'; redraw(); return; }
        if (at === null) { f.err = 'Pick the date the key stops working.'; redraw(); return; }
        const comment = await askComment('Create the key ' + f.name.trim() + '?', [permSummary(f.perms) + '. ' + (at ? 'It stops working on ' + dateLong(at) + '.' : 'It never expires.'),
          'The key is shown once, in the next panel.'], 'Create key');
        if (comment === undefined) return;
        f.busy = true; f.err = ''; redraw();
        try {
          const r = await api.post('/api/agents', { name: f.name.trim(), expires_at: at, permissions: f.perms }, { comment });
          fresh = r.agent.id;
          data = data || { agents: [] };
          data.agents = [r.agent].concat((data.agents || []).filter(a => a.id !== r.agent.id));
          data.active = (data.active || 0) + 1;
          showKey(r.agent, r.key, r.mcp_settings, false);
          load();
        } catch (e) { f.busy = false; f.err = e.message; redraw(); }
      }
      panelFor = 'new';
      panel = openPanel({ closesMenu: true,
        label: 'New key', name: 'New key', title: 'New key', focus: 'input',
        body: () => [
          f.err ? h('div.mcp-err', { key: 'err' }, banner('err', f.err)) : null,
          h('div.mcp-form', { key: 'form' },
            h('div.fld', h('label.lbl', { for: 'mcp-name' }, 'Name'),
              h('input.inp', { id: 'mcp-name', type: 'text', value: f.name, maxlength: 64, autocomplete: 'off', spellcheck: 'false', 'data-keep': '', placeholder: 'for example grafana-reader',
                oninput() { f.name = this.value; } })),
            expiryField(f, redraw, null),
            permsField(f.perms, redraw)),
        ],
        foot: () => [h('span.grow'), h('button.btn', { type: 'button', onclick: () => go({}) }, 'Cancel'),
          h('button.btn.pri', { type: 'button', disabled: f.busy, 'data-act': 'create', onclick: create }, f.busy ? 'Creating…' : 'Create key')],
        onClose: onClosed('new'),
      });
    }

    // Created / Rotated: the key shown once; it lives only in this closure
    function showKey(agent, key, settings, rotated) {
      const which = 'key:' + agent.id;
      let secret = { key, settings: snippetText(settings) };
      if (panel) { const p = panel; panel = null; panelFor = ''; p.close(); }
      shown = { id: agent.id };
      panelFor = which;
      const a = () => agentOf(agent.id) || agent;
      panel = openPanel({ closesMenu: true,
        label: rotated ? 'Key rotated' : 'New key', name: (rotated ? 'Key rotated: ' : 'New key: ') + agent.name,
        title: () => a().name,
        sub: () => [h('span.ktag.t-ok', rotated ? 'Rotated' : 'Created'), ' ', a().can + (a().expires_at ? ' · expires ' + dateLong(a().expires_at) : ' · never expires')],
        body: () => (secret ? [
          h('div.mcp-form', { key: 'shown' },
            h('div.fld', h('div.fld-h', h('span.lbl', 'Key ', h('span.once', '· shown once')), h('button.btn.sm.pri', { type: 'button', 'data-copy': 'key', onclick: () => copy(secret.key, 'the key') }, 'Copy')),
              h('div.inp.mono.mcp-key', { 'aria-label': 'The new key' }, secret.key)),
            h('div.fld', h('div.fld-h', h('span.lbl', 'MCP settings for the agent'), h('button.btn.sm', { type: 'button', 'data-copy': 'settings', onclick: () => copy(secret.settings, 'the settings') }, 'Copy')),
              h('pre.mcp-snip', secret.settings)),
            h('p.mut.mcp-note', rotated ? 'The old key stopped working. Copy the new one now: only its hash is kept, so it cannot be shown again.'
              : 'Copy it now: only its hash is kept, so it cannot be shown again. Rotate key makes a new one if it is lost.')),
        ] : []),
        foot: () => [h('button.btn', { type: 'button', 'data-act': 'manage', onclick: () => { secret = null; shown = null; const id = agent.id; closePanel(); panelFor = ''; openKey(id); } }, 'Manage access'),
          h('span.grow'), h('button.btn.pri', { type: 'button', 'data-act': 'done', onclick: () => go({}) }, 'Done')],
        onClose: () => { secret = null; onClosed(which)(); },
      });
      go({ key: agent.id }); // after the panel is open: the guard in syncPanel keeps it
    }

    // Manage
    function openKey(id) {
      const a0 = agentOf(id);
      const which = 'key:' + id;
      const f = { name: a0 ? a0.name : '', exp: '', custom: '', perms: a0 ? Object.assign({}, a0.permissions) : {}, err: '', busy: false };
      const redraw = () => panel && panel.redraw();
      const changes = a => {
        const out = {};
        if (f.name.trim() !== a.name) out.name = f.name.trim();
        const at = expiryAt(f.exp, f.custom);
        if (f.exp && at !== null && at !== (a.expires_at || 0)) out.expires_at = at;
        const p = {};
        for (const k of PERMS) if (f.perms[k] !== a.permissions[k]) p[k] = f.perms[k];
        if (Object.keys(p).length) out.permissions = p;
        return out;
      };
      async function save() {
        const a = agentOf(id);
        const ch = changes(a);
        if (!f.name.trim()) { f.err = 'The key needs a name: it is what the audit log shows.'; redraw(); return; }
        if (f.exp === 'custom' && !f.custom) { f.err = 'Pick the date the key stops working.'; redraw(); return; }
        if (!Object.keys(ch).length) { toast('Nothing to save'); return; }
        const lines = [];
        if (ch.name) lines.push('Name: ' + a.name + ' → ' + ch.name);
        if ('expires_at' in ch) lines.push('Expires: ' + expiresText(a) + ' → ' + (ch.expires_at ? dateLong(ch.expires_at) : 'Never'));
        for (const k of Object.keys(ch.permissions || {})) lines.push(LABEL[k] + ': ' + LEVEL[a.permissions[k]] + ' → ' + LEVEL[ch.permissions[k]]);
        const comment = await askComment('Save ' + plural(lines.length, 'change') + ' to ' + a.name + '?', [h('ul.mcp-list', lines.map(l => h('li', l)))], 'Save');
        if (comment === undefined) return;
        f.busy = true; f.err = ''; redraw();
        try {
          const r = await api.patch('/api/agents/' + encodeURIComponent(id), ch, { comment });
          toast(r.changed === false ? 'Nothing changed' : 'Saved ' + r.agent.name, 'ok');
          replace(r.agent); f.exp = ''; f.custom = ''; f.name = r.agent.name; f.perms = Object.assign({}, r.agent.permissions); f.busy = false;
          view.refresh(); redraw(); load();
        } catch (e) { f.busy = false; f.err = e.message; redraw(); }
      }
      async function rotate() {
        const a = agentOf(id);
        const comment = await askComment('Rotate the key of ' + a.name + '?', ['A new key is made and shown once. The old key (' + a.key_hint + ') stops working at once: every agent that uses it needs the new one.',
          'Its permissions and expiry stay as they are.'], 'Rotate key');
        if (comment === undefined) return;
        try {
          const r = await api.post('/api/agents/' + encodeURIComponent(id) + '/rotate', undefined, { comment });
          replace(r.agent); view.refresh();
          showKey(r.agent, r.key, r.mcp_settings, true);
          load();
        } catch (e) { f.err = e.message; redraw(); }
      }
      async function revoke() {
        const a = agentOf(id);
        const comment = await askComment('Revoke the key of ' + a.name + '?', ['The key (' + a.key_hint + ') stops working at once, for good: a revoked key cannot be turned back on. It stays in the list as Revoked.'], 'Revoke key', true);
        if (comment === undefined) return;
        try {
          const r = await api.del('/api/agents/' + encodeURIComponent(id), { comment });
          replace(r.agent); toast('Revoked ' + r.agent.name, 'ok'); view.refresh(); redraw(); load();
        } catch (e) { f.err = e.message; redraw(); }
      }
      panelFor = which;
      panel = openPanel({ closesMenu: true,
        label: 'Manage access', name: 'Manage access: ' + (a0 ? a0.name : id),
        title: () => (agentOf(id) || { name: id }).name,
        sub: () => {
          const a = agentOf(id);
          if (!a) return null;
          return [statusTag(a), a.last_used_at ? [' last used ', h('time', { title: full(a.last_used_at) }, stamp(a.last_used_at))] : ' never used',
            a.status === 'revoked' ? null : h('span.mcp-use', { 'data-usage': '' }, ' · ' + usageLine(a))];
        },
        body: () => {
          const a = agentOf(id);
          if (!a) return [empty('There is no key ' + id, 'It may have been removed. Close this panel.')];
          if (a.status === 'revoked') return [h('div.mcp-form', { key: 'rev' }, banner('info', 'This key was revoked on ' + dateLong(a.revoked_at) + ' and no longer works. A revoked key cannot be changed: make a new key instead.'),
            facts(a), permsField(a.permissions, null, true))];
          return [
            f.err ? h('div.mcp-err', { key: 'err' }, banner('err', f.err)) : null,
            h('div.mcp-form', { key: 'form-' + id },
              a.legacy ? banner('info', 'Made before permissions existed: it may do everything and never expires. Setting an expiry or a permission here gives it its own; the key itself stays the same.') : null,
              a.status === 'expired' ? banner('warn', 'This key expired on ' + dateLong(a.expires_at) + '. Pick a new expiry and save to turn it back on, or rotate it.') : null,
              h('div.fld', h('label.lbl', { for: 'mcp-name' }, 'Name'),
                h('input.inp', { id: 'mcp-name', type: 'text', value: f.name, maxlength: 64, autocomplete: 'off', spellcheck: 'false', 'data-keep': '', oninput() { f.name = this.value; } })),
              expiryField(f, redraw, a),
              permsField(f.perms, redraw),
              h('div.fld', h('div.fld-h', h('span.lbl', 'MCP settings for the agent'), h('button.btn.sm', { type: 'button', 'data-copy': 'settings', onclick: () => copy(snippetText(a.mcp_settings), 'the settings') }, 'Copy')),
                h('pre.mcp-snip', snippetText(a.mcp_settings)),
                h('p.mut.mcp-note', 'Put this key (' + a.key_hint + ') where it says <paste your key>. Only its hash is kept, so the page cannot show it again; Rotate key makes a new one.'))),
          ];
        },
        foot: () => {
          const a = agentOf(id);
          if (!a || a.status === 'revoked') return [h('span.grow'), h('button.btn', { type: 'button', onclick: () => go({}) }, 'Close')];
          return [h('button.btn', { type: 'button', 'data-act': 'rotate', onclick: rotate }, 'Rotate key'), h('button.btn.red', { type: 'button', 'data-act': 'revoke', onclick: revoke }, 'Revoke key'),
            h('span.grow'), h('button.btn.pri', { type: 'button', 'data-act': 'save', disabled: f.busy, onclick: save }, f.busy ? 'Saving…' : 'Save')];
        },
        onClose: onClosed(which),
      });
    }
    function replace(agent) {
      if (!data) return;
      data.agents = (data.agents || []).map(a => (a.id === agent.id ? agent : a));
      data.active = data.agents.filter(a => a.status === 'active' || a.status === 'expiring').length;
    }

    // the panel's parts
    function expiryField(f, redraw, a) {
      const at = expiryAt(f.exp, f.custom);
      const now = a && !f.exp ? (a.expires_at ? dateLong(a.expires_at) : 'never') : at === null ? 'pick a date' : at ? dateLong(at) : 'never';
      return h('div.fld', { key: 'exp' },
        h('span.lbl', 'Key expires ', h('span.mcp-when', '· ' + now)),
        (() => {
          const seg = segmented(EXPIRY, f.exp || (a && !a.expires_at && a.status !== 'expired' ? 'never' : ''), v => { f.exp = v; if (v === 'custom' && !f.custom) f.custom = isoDay(Date.now() + 60 * DAY); redraw(); }, 'Key expires');
          [...seg.children].forEach((b, i) => b.setAttribute('data-exp', EXPIRY[i].id));
          return seg;
        })(),
        f.exp === 'custom' ? h('input.inp.mcp-date', { type: 'date', value: f.custom, min: isoDay(Date.now() + DAY), 'aria-label': 'Expiry date', 'data-keep': '', onchange() { f.custom = this.value; redraw(); } }) : null);
    }
    function permsField(perms, redraw, readOnly) {
      return h('div.fld', { key: 'perms' }, h('span.lbl', 'Permissions'),
        h('div.mcp-perms', PERMS.map(p => h('div.pr', { key: 'p-' + p, 'data-perm': p },
          h('div.pr-name', LABEL[p], HINT[p] ? h('span.mut', ' ' + HINT[p]) : null),
          readOnly ? h('span.mcp-level', LEVEL[perms[p]] || '–')
            : levels(p, perms, redraw)))),
        !readOnly && PERMS.some(p => perms[p] === 'ask') ? h('p.mut.mcp-note', { 'data-ask': '' }, 'Ask me is kept, but approvals are not built yet: until they are, a request that needs one is refused.') : null);
    }
    // levels(p) -> the level buttons of one permission, each marked data-level
    function levels(p, perms, redraw) {
      const ls = p === 'observe' ? ['allow', 'off'] : ['allow', 'ask', 'off'];
      const seg = segmented(ls.map(l => ({ id: l, label: LEVEL[l] })), perms[p], v => { perms[p] = v; redraw(); }, LABEL[p]);
      [...seg.children].forEach((b, i) => b.setAttribute('data-level', ls[i]));
      return seg;
    }
    function facts(a) {
      return h('dl.facts', h('dt', 'Key'), h('dd', a.key_hint), h('dt', 'Last used'), h('dd', a.last_used_at ? stamp(a.last_used_at) : 'never'), h('dt', 'Requests'), h('dd', num(a.request_count || 0)));
    }
    const permSummary = p => { const on = PERMS.filter(k => p[k] === 'allow').map(k => LABEL[k].toLowerCase()); const ask = PERMS.filter(k => p[k] === 'ask').map(k => LABEL[k].toLowerCase());
      return 'It may ' + (on.length ? on.join(', ') : 'do nothing') + (ask.length ? '; ask me: ' + ask.join(', ') : ''); };

    // ---- the list ----
    function list() {
      const agents = data.agents || [];
      const open = route.query.key || '';
      return h('section.w.mcp', { key: 'list' },
        h('div.mcp-head', h('h2', 'Access keys ', h('span.mut', num(data.active || 0) + ' active')),
          h('button.btn.pri', { type: 'button', 'data-act': 'new', onclick: () => go({ new: '1' }) }, h('svg', { class: 'ic', viewBox: '0 0 20 20', 'aria-hidden': 'true' }, h('path', { d: 'M10 4v12M4 10h12' })), 'New key')),
        !agents.length ? empty('No access keys yet', 'A key lets an AI agent use Potluck over MCP, with only the permissions you give it, and records what it does under its name. Requests without a key keep working as the default user.')
          : h('div.mcp-scroll', h('table.mcp-tbl',
            h('thead', h('tr', h('th', { scope: 'col' }, 'Name'), h('th.c-key', { scope: 'col' }, 'Key'), h('th.c-can', { scope: 'col' }, 'Can'), h('th', { scope: 'col' }, 'Expires'),
              h('th.c-used', { scope: 'col' }, 'Last used'), h('th', { scope: 'col' }, 'Status'), h('th.c-go', { scope: 'col' }, h('span.sr', 'Manage')))),
            h('tbody', agents.map(a => h('tr', { key: 'k-' + a.id, 'data-key': a.id, class: [gone(a) ? 'gone' : '', a.id === open ? 'sel' : '', a.id === fresh ? 'fresh' : ''].filter(Boolean).join(' ') || null,
              onclick: e => { if (!e.target.closest('a')) go({ key: a.id }); } },
              h('td.nm', h('span', { title: a.description || null }, a.name), a.legacy ? h('span.ktag.t-line', { title: 'Made before permissions existed' }, 'legacy') : null),
              h('td.mono.c-key', a.key_hint),
              h('td.c-can', { title: (a.asks || []).length ? 'Ask me: ' + a.asks.map(k => LABEL[k]).join(', ') : null }, a.can),
              h('td', expiresText(a)),
              h('td.c-used', a.last_used_at ? h('time', { title: full(a.last_used_at) }, stamp(a.last_used_at)) : h('span.mut', 'never')),
              h('td', statusTag(a)),
              h('td.num.c-go', h('a.go', { href: '#/mcp?key=' + encodeURIComponent(a.id), 'aria-label': 'Manage ' + a.name, onclick: e => { e.preventDefault(); go({ key: a.id }); } },
                h('span.tx', 'Manage'), h('svg', { class: 'ic', viewBox: '0 0 20 20', 'aria-hidden': 'true' }, h('path', { d: 'M8 5l5 5-5 5' }))))))))));
    }

    const view = liveView(el, () => {
      if (error && !data) return [h('section.w.mcp', { key: 'err' }, h('div.mcp-head', h('h2', 'Access keys')), banner('err', ['The keys could not be read: ', error]))];
      if (!data) return [h('section.w.mcp', { key: 'wait' }, skeleton(6))];
      return [list(), toolsPart(tools, toolsError)];
    }, ['conn']);
    load();
    return {
      update(r) { route = r; syncPanel(); view.refresh(); },
      unmount() { alive = false; offConfig(); closePanel(); view.unmount(); },
    };
  },
});
