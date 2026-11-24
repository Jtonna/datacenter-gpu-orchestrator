// Create new zone (board Zones-Create): make a zone before its machine joins.
//
// 1. The form: the zone name, its group, where the machine reads its model
//    files (the setting default_model_source, which the person may change
//    for this zone), how long the command stays valid, and a comment.
//    POST /api/zones/pending {name, group?, model_source?, ttl_min, comment}
//    (X-Comment too; the audit entry is signed by the default user).
// 2. The answer: the command line the server wrote ("curl -sSL
//    '…/install.sh?zone=pz-…' | sudo sh") with Copy, the script to download,
//    and the state, read again from GET /api/zones/pending/{id} every few
//    seconds: waiting (with its expiry), joined, expired, cancelled; a 404
//    (the scheduler restarted: pending zones live in memory) says "This zone
//    was not found: create it again". Cancel = DELETE /api/zones/pending/{id}
//    with its own comment.
//
// Drawn but not available, with the reason: Install over SSH (Potluck never
// connects out and holds no SSH credentials), the Windows command and
// script, and the USB drive (those installers do not exist yet).
// The server's sentences are shown as they come: 400 (no model source, a bad
// name or source), 409 (a taken name), 404 (no such group).

import { h } from './dom.js';
import { href } from './router.js';
import * as api from './api.js';
import { dialog, button, copyButton, badge } from './ui.js';
import { clock, dateTime } from './format.js';

const SSH_WHY = 'Not available: Potluck never connects out to a machine and holds no SSH credentials. Run the command on the machine instead.';
const WIN_WHY = 'Not available yet: there is no Windows installer. The agent runs on Linux machines for now.';
const USB_WHY = 'Not available yet: there is no USB drive image.';
const SOURCE_HINT = 'share:<host>:/<path>, dir:/<path> or push';
const TTLS = [15, 30, 60];

const errText = e => (e && e.body && e.body.error && e.body.error.message) || (e && e.message) || 'The request failed.';

// stateLine(z, gone) -> the state of a pending zone in words.
function stateLine(z, gone) {
  if (gone) return h('div.zc-state.bad', { 'data-cz': 'state', 'data-state': 'gone' }, h('b', 'This zone was not found: create it again.'),
    h('span.mut', ' Pending zones are kept in memory, so a restart of the scheduler forgets them; the command above no longer works.'));
  const st = z.state || 'waiting';
  const text = z.state_text || ({ waiting: 'Waiting for the machine to check in', joined: 'Joined', expired: 'Expired: the machine did not check in in time', cancelled: 'Cancelled' })[st] || st;
  return h('div', { class: 'zc-state ' + st, 'data-cz': 'state', 'data-state': st },
    st === 'waiting' ? h('span.zc-spin', { 'aria-hidden': 'true' }) : null,
    h('b', text),
    st === 'waiting' && z.expires_at ? h('span.mut', { title: dateTime(z.expires_at) }, ' · the command works until ' + clock(z.expires_at, false)) : null,
    st === 'joined' ? h('span.mut', ' as ', h('span.mono', z.name), z.group ? [' in ', h('span.mono', z.group)] : null, z.joined_at ? ' at ' + clock(z.joined_at, false) : null) : null,
    st === 'expired' || st === 'cancelled' ? h('span.mut', ' The command no longer works; create the zone again to get a new one.') : null);
}

// createZone({groups, group}) opens the dialog; groups are the group names to
// offer, group the one to preselect. Resolves when it closes.
export function createZone(o = {}) {
  let timer = 0;
  return dialog({ title: 'Create new zone', wide: true, body: close => {
    const root = h('div.zc', { 'data-create-zone': '' });
    const st = { phase: 'form', def: null, defErr: '', z: null, gone: false, tab: 'cmd', err: '', errSource: false, busy: false, cancelOpen: false };
    const name = h('input.input.mono', { 'data-cz': 'name', 'aria-label': 'Zone name', placeholder: 'for example zone-9', autocomplete: 'off', spellcheck: 'false', maxlength: 63 });
    const group = h('select.select', { 'data-cz': 'group', 'aria-label': 'Group' },
      h('option', { value: '' }, 'No group'), (o.groups || []).map(g => h('option', { value: g, selected: g === o.group }, g)));
    const source = h('input.input.mono', { 'data-cz': 'source', 'aria-label': 'Model source', placeholder: SOURCE_HINT, autocomplete: 'off', spellcheck: 'false' });
    const ttl = h('select.select', { 'data-cz': 'ttl', 'aria-label': 'The command works for' }, TTLS.map(m => h('option', { value: String(m), selected: m === 60 }, m + ' min')));
    const comment = h('textarea.textarea', { rows: 2, maxlength: 400, 'data-cz': 'comment', placeholder: 'Why? It is kept in the audit log.', 'aria-label': 'Comment' });
    const cancelNote = h('textarea.textarea', { rows: 2, maxlength: 400, 'data-cz': 'cancel-comment', placeholder: 'Why cancel? It is kept in the audit log.', 'aria-label': 'Comment for the cancel' });
    const stop = () => { clearInterval(timer); timer = 0; };
    const done = v => { stop(); close(v); };

    const tab = (id, label, why) => h('button', { type: 'button', role: 'tab', class: 'zc-tab' + (st.tab === id ? ' on' : ''), 'aria-selected': st.tab === id ? 'true' : 'false',
      'data-cz-tab': id, disabled: !!why, title: why || null, onclick: () => { st.tab = id; draw(); } }, label);
    const tile = (label, why) => h('div.zc-tile.off', { title: why, 'data-cz-off': label }, h('b', label), h('span.mut', why));

    async function readDefault() {
      try {
        const r = await api.get('/api/config/settings');
        const s = (r && r.settings) || r || {};
        st.def = typeof s.default_model_source === 'string' ? s.default_model_source : '';
      } catch (e) {
        try { const p = await api.get('/api/zones/pending'); st.def = (p && p.default_model_source) || ''; } catch { st.defErr = errText(e); st.def = ''; }
      }
      if (!source.value && st.def) source.value = st.def;
      draw();
    }
    async function poll() {
      if (!st.z) return;
      try { const z = await api.get('/api/zones/pending/' + encodeURIComponent(st.z.id)); st.z = Object.assign({}, st.z, z); st.gone = false; }
      catch (e) { if (e && e.status === 404) { st.gone = true; stop(); } }
      if (st.z && st.z.state !== 'waiting') stop();
      draw();
    }
    async function create() {
      const c = comment.value.trim();
      if (!name.value.trim() || !c || st.busy) return;
      st.busy = true; st.err = ''; st.errSource = false; draw();
      const body = { name: name.value.trim(), ttl_min: Number(ttl.value) || 60, comment: c };
      if (group.value) body.group = group.value;
      const src = source.value.trim();
      if (src && src !== st.def) body.model_source = src; // the default is the server's own; send only a change
      try {
        st.z = await api.post('/api/zones/pending', body, { comment: c });
        st.phase = 'made';
        timer = setInterval(poll, 3000);
        poll();
      } catch (e) {
        st.err = errText(e);
        st.errSource = e && e.status === 400 && /model source/i.test(st.err);
      }
      st.busy = false; draw();
    }
    async function cancel() {
      const c = cancelNote.value.trim();
      if (!c || !st.z || st.busy) return;
      st.busy = true; st.err = ''; draw();
      try { st.z = Object.assign({}, st.z, await api.del('/api/zones/pending/' + encodeURIComponent(st.z.id), { comment: c })); stop(); st.cancelOpen = false; }
      catch (e) { if (e && e.status === 404) { st.gone = true; stop(); } else st.err = errText(e); }
      st.busy = false; draw();
    }

    function formPart() {
      const go = button('Create zone', create, 'lg pri', { 'data-go': '', disabled: !name.value.trim() || !comment.value.trim() || st.busy });
      const srcNote = st.def == null ? h('span.help', 'Reading the default model source…')
        : st.def ? h('span.help', { 'data-cz': 'default' }, 'The default for new machines is ', h('span.mono', st.def), '. Change it here for this zone only.')
          : h('span.help', { 'data-cz': 'default' }, 'No default is set for new machines: give one for this zone, or set it under ',
            h('a', { href: href('settings', ['agents']), 'data-cz': 'settings-link', onclick: () => done(false) }, 'Settings, Agents'), '.');
      return [
        h('div.zc-grid',
          h('label.field', h('span.lab', 'Zone name'), name),
          h('label.field', h('span.lab', 'Group'), group)),
        h('div.zc-tabs', { role: 'tablist', 'aria-label': 'How to install' }, tab('cmd', 'Run a command'), tab('dl', 'Download a script'), tab('ssh', 'Install over SSH', SSH_WHY)),
        h('div.zc-grid',
          h('label.field', h('span.lab', 'Model source'), source, srcNote),
          h('label.field', h('span.lab', 'The command works for'), ttl, h('span.help', 'At most an hour. A restart of Potluck also ends it.'))),
        h('p.zc-safe', { 'data-cz': 'safe' }, 'The script installs the Potluck agent and nothing else. It refuses to run on a PlayStation console and on the scheduler\'s own machine, prints what it will do first, and restarts no machine.'),
        h('label.field', h('span.lab', 'Comment (required)'), comment),
        st.err ? h('div.err', { role: 'alert', 'data-cz': 'error' }, st.err,
          st.errSource ? [' ', h('a', { href: href('settings', ['agents']), 'data-cz': 'settings-link', onclick: () => done(false) }, 'Set the default model source')] : null) : null,
        h('div.dlg-foot', button('Cancel', () => done(false), 'lg'), go),
      ];
    }

    function madePart() {
      const z = st.z;
      const live = z.state === 'waiting' && !st.gone;
      const cmdTab = h('div.zc-pane', { 'data-cz-pane': 'cmd' },
        h('div.zc-lab', 'Run this on the machine'),
        h('div.zc-os', h('b', 'Linux'), h('div.zc-cmd', h('code.mono', { 'data-cz': 'command' }, z.command || ''), live ? copyButton(z.command || '', 'the command') : null)),
        tile('Windows', WIN_WHY));
      const dlTab = h('div.zc-pane', { 'data-cz-pane': 'dl' },
        h('div.zc-os', h('b', 'Linux'), h('span.mono', z.download_name || 'potluck-zone.sh'),
          live && z.download_path ? h('a.btn', { href: z.download_path, download: z.download_name || '', 'data-cz': 'download' }, 'Download') : null),
        tile('Windows', WIN_WHY), tile('USB drive', USB_WHY));
      return [
        h('div.zc-grid',
          h('div.field', h('span.lab', 'Zone name'), h('b.mono', { 'data-cz': 'made-name' }, z.name)),
          h('div.field', h('span.lab', 'Group'), z.group ? h('b.mono', z.group) : h('span.mut', 'No group'))),
        h('div.zc-tabs', { role: 'tablist', 'aria-label': 'How to install' }, tab('cmd', 'Run a command'), tab('dl', 'Download a script'), tab('ssh', 'Install over SSH', SSH_WHY)),
        st.tab === 'dl' ? dlTab : cmdTab,
        h('div.mut.zc-src', 'Model files from ', h('span.mono', z.model_source || '?'), ' · installs the agent as ', z.mode === 'docker' ? 'a container' : 'a service', '.'),
        stateLine(z, st.gone),
        st.err ? h('div.err', { role: 'alert', 'data-cz': 'error' }, st.err) : null,
        st.cancelOpen ? h('div.zc-cancel', h('label.field', h('span.lab', 'Comment (required)'), cancelNote),
          button('Cancel this zone', cancel, 'dng', { 'data-cz': 'cancel-go', disabled: !cancelNote.value.trim() || st.busy })) : null,
        h('div.dlg-foot',
          live && !st.cancelOpen ? button('Cancel this zone…', () => { st.cancelOpen = true; draw(); }, 'lg', { 'data-cz': 'cancel' }) : null,
          button('Close', () => done(true), 'lg pri', { 'data-cz': 'close' })),
      ];
    }

    function draw() { root.replaceChildren(...(st.phase === 'form' ? formPart() : madePart()).filter(Boolean).flat()); }
    for (const el of [name, comment]) el.addEventListener('input', () => { const b = root.querySelector('[data-go]'); if (b) { if (name.value.trim() && comment.value.trim() && !st.busy) b.removeAttribute('disabled'); else b.setAttribute('disabled', ''); } });
    cancelNote.addEventListener('input', () => { const b = root.querySelector('[data-cz=cancel-go]'); if (b) { if (cancelNote.value.trim()) b.removeAttribute('disabled'); else b.setAttribute('disabled', ''); } });
    draw();
    readDefault();
    return root;
  } }).finally(() => { clearInterval(timer); });
}
