// MCP (System): what to hand to an AI agent so that it can operate the
// scheduler, and so that what it does is recorded under its own name. The
// page lists the agents, makes a key for a new one (shown once, with
// ready-to-copy configuration), and shows what an agent can do: the MCP tool
// list, read from the server.
//
// A key here is identity for the audit log, not access control: the owner
// decided that this system has no login. Requests without a key keep working.
//
// This page only documents that agents can load and unload models. The web
// UI itself has no such control, and must not get one.
//
// Reads: REST GET /api/agents, GET /api/mcp/tools; WebSocket "config" (an
// agent was added, renamed or revoked somewhere else: read the list again).
// Writes: POST /api/agents, PATCH /api/agents/{id}, POST
// /api/agents/{id}/rotate, DELETE /api/agents/{id} (core/api.js).
// Route: #/mcp

import { h } from '../core/dom.js';
import { registerView, href } from '../core/router.js';
import { listen } from '../core/store.js';
import * as api from '../core/api.js';
import { liveView, pageHead, badge, table, empty, skeleton, button, more, dialog, confirm, toast, banner } from '../core/ui.js';
import { num, ago, plural, DASH } from '../core/format.js';

// The address of the MCP endpoint as this browser sees it. It is built from
// the page's own address at run time: nothing here names a host.
const mcpURL = () => location.origin + api.url('/mcp');
const PLACEHOLDER = 'lls_YOUR_AGENT_KEY';

async function copy(text, what) {
  try { await navigator.clipboard.writeText(text); toast('Copied ' + what, 'ok'); } catch { toast('Select the text and copy it by hand'); }
}

function snippet(title, note, text, what) {
  return h('div.snip', { key: 's-' + title },
    h('div.snip-h', h('b', title), note ? h('span', note) : null, button('Copy', () => copy(text, what), 'sm')),
    h('pre', { tabindex: 0, 'aria-label': title }, text));
}

// The three ways to hand the address and the key to an agent.
function snippets(key) {
  const url = mcpURL(), k = key || PLACEHOLDER;
  const json = JSON.stringify({ mcpServers: { 'llm-scheduler': { type: 'http', url, headers: { Authorization: 'Bearer ' + k } } } }, null, 2);
  const claude = `claude mcp add --transport http llm-scheduler ${url} --header "Authorization: Bearer ${k}"`;
  const curl = [`curl -s ${url} \\`, '  -H "Content-Type: application/json" \\', `  -H "Authorization: Bearer ${k}" \\`, `  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'`].join('\n');
  return [
    snippet('MCP client configuration', 'For a client that takes an address and headers (an mcpServers block)', json, 'the configuration'),
    snippet('Claude Code', 'Run it once in a terminal', claude, 'the command'),
    snippet('Try it with curl', 'Lists the tools the agent will see', curl, 'the command'),
  ];
}

// ---- dialogs ----

function agentForm(o) {
  const name = h('input.input', { type: 'text', value: o.name || '', maxlength: 64, placeholder: 'for example claude-code-laptop', 'aria-label': 'Name', autocomplete: 'off', spellcheck: 'false' });
  const desc = h('input.input', { type: 'text', value: o.description || '', maxlength: 500, placeholder: 'optional: what it is for', 'aria-label': 'Description', autocomplete: 'off' });
  const err = h('span.err', { role: 'alert' });
  let busy = false;
  return dialog({
    title: o.title,
    body: close => {
      const go = async () => {
        if (busy) return;
        if (!name.value.trim()) { err.textContent = 'Give the agent a name. It is what the audit log shows.'; name.focus(); return; }
        busy = true;
        try { close(await o.save({ name: name.value.trim(), description: desc.value.trim() })); } catch (e) { busy = false; err.textContent = e.message; }
      };
      for (const i of [name, desc]) i.addEventListener('keydown', e => { if (e.key === 'Enter') go(); });
      return h('div.col', { style: 'gap:14px' },
        h('p', o.intro),
        h('div.field', h('label', 'Name'), name, h('span.help', 'Shown in the audit log as the one who made a change. Name the agent and where it runs, so two agents can be told apart.'), err),
        h('div.field', h('label', 'Description'), desc),
        h('div.dlg-foot', h('button', { type: 'button', class: 'btn lg', onclick: () => close(undefined) }, 'Cancel'),
          h('button', { type: 'button', class: 'btn lg pri', onclick: go }, o.saveLabel)));
    },
  });
}

registerView({
  id: 'mcp', title: 'MCP',
  mount(el) {
    let agents = null, error = '', tools = null, toolsError = '', fresh = null, showRevoked = false, alive = true;
    const openGroups = new Set(['read', 'configuration', 'benchmarks', 'runtime']);

    const loadAgents = async () => {
      try { agents = (await api.get('/api/agents')).agents || []; error = ''; } catch (e) { error = e.message; }
      if (alive) view.refresh();
    };
    const loadTools = async () => {
      try { tools = await api.get('/api/mcp/tools'); toolsError = ''; } catch (e) { toolsError = e.message; }
      if (alive) view.refresh();
    };
    const off = listen('config', ev => { if (ev && (ev.section === 'agents' || (ev.actor || '').startsWith('agent:'))) loadAgents(); });

    const add = async () => {
      const r = await agentForm({ title: 'Add an agent', saveLabel: 'Add agent and make its key',
        intro: 'An agent is a name and a key. Give the key to one AI agent; whatever it changes is then recorded under this name. The key is shown once, right after this.',
        save: v => api.post('/api/agents', v) });
      if (!r) return;
      fresh = { id: r.agent.id, name: r.agent.name, key: r.key, what: 'made' };
      window.scrollTo({ top: 0 });
      loadAgents();
    };
    const rename = async a => {
      const r = await agentForm({ title: 'Rename ' + a.name, saveLabel: 'Save', name: a.name, description: a.description,
        intro: 'The key stays the same. Entries the agent made before keep the name they were recorded with; new ones carry the new name.',
        save: v => api.patch('/api/agents/' + encodeURIComponent(a.id), v) });
      if (r) { toast('Saved', 'ok'); loadAgents(); }
    };
    const rotate = async a => {
      const ok = await confirm({ title: `Make a new key for ${a.name}?`, intro: 'The agent keeps its name and its history.', confirmLabel: 'Make a new key',
        hits: [{ kind: 'x', text: 'The key it uses now stops working at once: its requests are refused until it has the new key.' }, { kind: '', text: 'The new key is shown once, right after this.' }] });
      if (!ok) return;
      try {
        const r = await api.post('/api/agents/' + encodeURIComponent(a.id) + '/rotate');
        fresh = { id: a.id, name: a.name, key: r.key, what: 'replaced' };
        window.scrollTo({ top: 0 });
        loadAgents();
      } catch (e) { toast(e.message, 'err'); }
    };
    const revoke = async a => {
      const ok = await confirm({ title: `Revoke ${a.name}?`, danger: true, confirmLabel: 'Revoke the agent',
        intro: 'Its key stops working: a request that still carries it is refused with a message that says so.',
        hits: [{ kind: 'x', text: 'This cannot be undone. To let the agent back in, add a new agent and give it the new key.' }, { kind: '', text: 'What it did stays in the audit log under its name.' }] });
      if (!ok) return;
      try { await api.del('/api/agents/' + encodeURIComponent(a.id)); toast(`${a.name} is revoked`, 'ok'); if (fresh && fresh.id === a.id) fresh = null; loadAgents(); } catch (e) { toast(e.message, 'err'); }
    };

    function connectCard() {
      const url = mcpURL();
      const local = /^(localhost|127\.|\[?::1)/.test(location.hostname);
      return h('section.card', { key: 'connect' },
        h('div.ch', h('h2', 'Connect an agent'), h('span.note', 'One address, one key per agent')),
        h('div.cb.col', { style: 'gap:14px' },
          h('p', { style: 'max-width:86ch;color:var(--text-2)' }, 'An AI agent that speaks MCP can read the cluster, change the configuration, run benchmarks, and load or unload models. Give it the address below and a key of its own: everything it does is then recorded under its name in the ',
            h('a', { href: href('audit') }, 'audit log'), '.'),
          h('div.banner', { role: 'note' }, h('span.grow', h('b', 'A key says who the agent is. It is not a lock. '),
            'This system has no login: requests without a key keep working and are recorded as an unnamed caller. A key that is unknown or revoked is refused, so a typo cannot quietly lose the attribution.')),
          h('div.snip',
            h('div.snip-h', h('b', 'Server address'), h('span', 'As this browser sees it'), button('Copy', () => copy(url, 'the address'), 'sm')),
            h('pre', { tabindex: 0, 'aria-label': 'Server address' }, url)),
          h('p.mut', { style: 'font-size:13px;font-weight:600' }, local
            ? 'This page was opened on the machine that runs the scheduler. An agent on another machine cannot use this address: replace the host with a name or address it can reach.'
            : 'An agent on another machine needs a host name it can reach. If this name only resolves on your computer, use the IP address of the scheduler instead.',
            ' The transport is HTTP (one request, one JSON answer); the REST API takes the same key in the same header.')));
    }

    function freshCard() {
      if (!fresh) return null;
      return h('section.card.flag-warn', { key: 'fresh-' + fresh.id, 'aria-label': 'The new key' },
        h('div.ch', h('h2', fresh.what === 'made' ? `The key of ${fresh.name}` : `The new key of ${fresh.name}`), badge('Shown once', 'load'),
          h('span.right', button('Done, I have copied it', () => { fresh = null; view.refresh(); }, 'sm'))),
        h('div.cb.col', { style: 'gap:14px' },
          h('p', { style: 'color:var(--text-2)' }, 'Copy it now. Only a fingerprint of it is stored, so it cannot be shown again; if it is lost, make a new key for the agent.'),
          h('div.keybox', h('code', fresh.key), button('Copy key', () => copy(fresh.key, 'the key'), 'sm pri')),
          h('div.col', { style: 'gap:12px' }, snippets(fresh.key))));
    }

    function agentsCard() {
      let body;
      const all = agents || [];
      const active = all.filter(a => !a.revoked), revoked = all.filter(a => a.revoked);
      const rows = showRevoked ? active.concat(revoked) : active;
      if (error && !agents) body = empty(/database|available/i.test(error) ? 'Agents are not available' : 'The agents could not be read', /database|available/i.test(error) ? 'The scheduler is running without its database, so keys cannot be kept. Agents can still connect without a key.' : error);
      else if (!agents) body = skeleton(3);
      else if (!all.length) body = empty('No agent yet', 'Add one to get a key. Until then an agent can still connect without a key; what it does is then recorded as an unnamed caller.', button('Add agent', add, 'sm'));
      else if (!rows.length) body = empty('No active agent', 'Every agent was revoked.', button('Add agent', add, 'sm'));
      else body = table({
        minWidth: 940, rows, key: a => a.id, rowClass: a => (a.revoked ? 'off' : ''),
        columns: [
          { label: 'Agent', render: a => h('span', h('span.name', a.name, a.revoked ? [' ', badge('Revoked', 'dash')] : null), a.description ? h('span.sub', a.description) : null) },
          { label: 'Key', cls: 'tight', title: 'The first characters of the key, to tell keys apart', render: a => h('span.mono', a.key_prefix + '…') },
          { label: 'Added', cls: 'tight', render: a => h('span', ago(a.created_at), h('span.sub', 'by ' + ((a.created_by || {}).actor || 'unknown'))) },
          { label: 'Last used', cls: 'tight', render: a => ago(a.last_used_at, 'Never') },
          { label: 'Requests', cls: 'r', title: 'Requests that carried its key, over MCP and REST', render: a => num(a.request_count || 0) },
          { label: 'In the audit log', cls: 'tight', render: a => (a.audit_entries
            ? h('a', { href: href('audit', null, { agent: a.id, range: 'all' }) }, plural(a.audit_entries, 'action'))
            : h('span.mut', 'Nothing yet')) },
          { label: '', cls: 'tight', render: a => (a.revoked ? h('span.mut', { style: 'font-size:13px' }, 'Revoked ', ago(a.revoked_at)) : h('span.row', { style: 'gap:6px;justify-content:flex-end;flex-wrap:nowrap' },
            button('Rename', () => rename(a), 'sm'), button('New key', () => rotate(a), 'sm'), button('Revoke', () => revoke(a), 'sm dng'))) },
        ],
      });
      return h('section.card', { key: 'agents' },
        h('div.ch', h('h2', 'Agents'), agents ? h('span.note', `${plural(active.length, 'agent')}` + (revoked.length ? ` · ${num(revoked.length)} revoked` : '')) : null,
          h('span.right', button('Add agent', add, 'sm pri'))),
        body,
        revoked.length ? h('div.cs', more(showRevoked, () => { showRevoked = !showRevoked; view.refresh(); }, `Show the ${plural(revoked.length, 'revoked agent')}`, 'Hide the revoked agents')) : null);
    }

    function snippetsCard() {
      if (fresh) return null;
      return h('section.card', { key: 'snips' },
        h('div.ch', h('h2', 'Configuration to hand over'), h('span.note', ['Replace ', h('span.mono', PLACEHOLDER), ' with the agent’s key. A new key comes with these filled in.'])),
        h('div.cb.col', { style: 'gap:12px' }, snippets('')));
    }

    function toolsCard() {
      let body;
      if (toolsError && !tools) body = empty('The tool list could not be read', toolsError);
      else if (!tools) body = skeleton(6);
      else {
        const list = tools.tools || [];
        body = (tools.groups || []).map(g => {
          const mine = list.filter(t => t.group === g.id);
          if (!mine.length) return null;
          const isOpen = openGroups.has(g.id);
          return h('div', { key: 'g-' + g.id },
            h('div.cs', { style: 'display:flex;flex-wrap:wrap;align-items:baseline;gap:4px 12px' },
              h('b', g.title), badge(num(mine.length), 'n'), h('span.mut', { style: 'font-size:13px;font-weight:600;flex:1 1 320px' }, g.note),
              more(isOpen, () => { if (isOpen) openGroups.delete(g.id); else openGroups.add(g.id); view.refresh(); }, 'Show the tools', 'Hide')),
            isOpen ? h('div.tool-list', mine.map(t => h('div', { key: t.name }, h('span', { title: t.description }, t.name), h('span', t.summary,
              t.destructive ? [' ', badge('Changes or removes', 'n', 'The tool replaces or deletes something. Configuration changes can be rolled back.')] : null)))) : null);
        });
      }
      const n = tools ? (tools.tools || []).length : 0;
      return h('section.card', { key: 'tools' },
        h('div.ch', h('h2', 'What an agent can do'), h('span.note', tools ? `${plural(n, 'tool')}, read from this scheduler` + (tools.server ? ` (version ${tools.server.version})` : '') : null)),
        tools ? h('div.cb', { style: 'padding-bottom:4px' }, banner('', ['Loading and unloading a model is something only an agent or the API can ask for. ', h('b', 'This web page has no load or unload control'), ', by design: here you watch, configure and run benchmarks.'])) : null,
        body);
    }

    const render = () => [
      pageHead('MCP', agents ? [h('b', num(agents.filter(a => !a.revoked).length)), agents.filter(a => !a.revoked).length === 1 ? ' agent' : ' agents', tools ? [' · ', h('b', num((tools.tools || []).length)), ' tools'] : null] : 'Connect an AI agent'),
      freshCard(),
      connectCard(),
      agentsCard(),
      snippetsCard(),
      toolsCard(),
    ];
    const view = liveView(el, render, ['tick', 'conn']);
    loadAgents();
    loadTools();
    return { unmount() { alive = false; off(); view.unmount(); } };
  },
});
