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

import { h, icon } from './dom.js';
import { href } from './router.js';
import * as api from './api.js';
import { dialog, button, copyButton, badge } from './ui.js';
import { clock, dateTime } from './format.js';

const SSH_WHY = 'Not available: Potluck never connects out to a machine and holds no SSH credentials. Run the command on the machine instead.';
const WIN_WHY = 'Not available yet: there is no Windows installer. The agent runs on Linux machines for now.';
const USB_WHY = 'Not available yet: there is no USB drive image.';
const SOURCE_HINT = 'share:<host>:/<path>, dir:/<path> or push';
const TTLS = [15, 30, 60];
const IC = {
  cmd: ['M3 4h14v12H3z', 'M6 8l2.5 2L6 12M10 12h4'],
  dl: ['M10 3v9M6 8.5l4 4 4-4M4 16h12'],
  ssh: ['M11 4h5v12h-5M3 10h9M9 7l3 3-3 3'],
  file: ['M5 2.5h6.5l3.5 3.5v11.5H5z', 'M11.5 2.5V6H15'],
  usb: ['M7 9h6v8.5H7zM8.5 2.5h3V9h-3z'],
};

const errText = e => (e && e.body && e.body.error && e.body.error.message) || (e && e.message) || 'The request failed.';

// stateLine(z, gone) -> the state of a pending zone in words.
function stateLine(z, gone) {
  if (gone) return h('div.zc-state.bad', { 'data-cz': 'state', 'data-state': 'gone' }, h('span.zc-dot', { 'aria-hidden': 'true' }),
    h('span', h('b', 'This zone was not found: create it again.'),
      h('span.mut', ' Pending zones are kept in memory, so a restart of the scheduler forgets them; the command above no longer works.')));
  const st = z.state || 'waiting';
  const text = z.state_text || ({ waiting: 'Waiting for the machine to check in', joined: 'Joined', expired: 'Expired: the machine did not check in in time', cancelled: 'Cancelled' })[st] || st;
  return h('div', { class: 'zc-state ' + st, 'data-cz': 'state', 'data-state': st },
    h('span.zc-dot', { 'aria-hidden': 'true' }),
    h('span', h('b', text),
    st === 'waiting' && z.expires_at ? h('span.mut', { title: dateTime(z.expires_at) }, ' · the command works until ' + clock(z.expires_at, false)) : null,
    st === 'joined' ? h('span.mut', ' as ', h('span.mono', z.name), z.group ? [' in ', h('span.mono', z.group)] : null, z.joined_at ? ' at ' + clock(z.joined_at, false) : null) : null,
    st === 'expired' || st === 'cancelled' ? h('span.mut', ' The command no longer works; create the zone again to get a new one.') : null));
}

// createZone({groups}) opens the dialog; groups are the group names the page
// has (the dialog also reads GET /api/groups). Resolves true when a zone was
// made, else false or undefined.
export function createZone(o = {}) {
  let timer = 0;
  return dialog({ title: 'Create new zone', body: close => {
    const root = h('div.zc', { 'data-create-zone': '' });
    const st = { phase: 'form', def: null, defErr: '', z: null, gone: false, tab: 'cmd', err: '', errSource: false, busy: false, cancelOpen: false };
    const name = h('input.input.mono', { 'data-cz': 'name', 'aria-label': 'Zone name', placeholder: 'Machine\'s hostname', autocomplete: 'off', spellcheck: 'false', maxlength: 63 });
    const group = h('select.select', { 'data-cz': 'group', 'aria-label': 'Group' },
      h('option', { value: '' }, 'No group'), (o.groups || []).map(g => h('option', { value: g }, g)));
    const source = h('input.input.mono', { 'data-cz': 'source', 'aria-label': 'Model source', placeholder: SOURCE_HINT, autocomplete: 'off', spellcheck: 'false' });
    const ttl = h('select.select', { 'data-cz': 'ttl', 'aria-label': 'The command works for' }, TTLS.map(m => h('option', { value: String(m), selected: m === 60 }, m + ' min')));
    const comment = h('textarea.textarea', { rows: 2, maxlength: 400, 'data-cz': 'comment', placeholder: 'Why? It is kept in the audit log.', 'aria-label': 'Comment' });
    const cancelNote = h('textarea.textarea', { rows: 2, maxlength: 400, 'data-cz': 'cancel-comment', placeholder: 'Why cancel? It is kept in the audit log.', 'aria-label': 'Comment for the cancel' });
    const stop = () => { clearInterval(timer); timer = 0; };
    const done = v => { stop(); close(v); };

    const tab = (id, label, why) => h('button', { type: 'button', role: 'tab', class: 'zc-tab' + (st.tab === id ? ' on' : ''), 'aria-selected': st.tab === id ? 'true' : 'false',
      'data-cz-tab': id, disabled: !!why, title: why || null, onclick: () => { st.tab = id; draw(); } }, icon(IC[id]), h('span', label));
    const tabs = () => h('div.zc-tabs', { role: 'tablist', 'aria-label': 'How to install' }, tab('cmd', 'Run a command'), tab('dl', 'Download a script'), tab('ssh', 'Install over SSH', SSH_WHY));
    const head = () => h('div.zc-grid',
      h('label.field', h('span.lab', 'Zone name'), name),
      h('label.field', h('span.lab', 'Group'), group));
    // a file row of Download a script: an unavailable one says why instead of a file name
    const fileRow = (ic, os, file, link, why) => h('div.zc-file', { 'data-cz-file': os },
      icon(IC[ic]), h('div.zc-file-t', h('b', os), why ? h('span.mut', why) : h('span.mono', file)),
      link ? h('a.btn', { href: link, download: file, 'data-cz': 'download' }, 'Download') : h('button.btn', { type: 'button', disabled: true, title: why || null }, 'Download'));

    // the groups to offer: what the page had, then GET /api/groups (the page may
    // not have read it yet when the dialog opens)
    const fillGroups = names => {
      const keep = group.value;
      group.replaceChildren(h('option', { value: '' }, 'No group'), ...names.map(g => h('option', { value: g }, g)));
      group.value = names.includes(keep) ? keep : '';
    };
    async function readGroups() {
      try { const d = await api.get('/api/groups'); const names = ((d && d.groups) || []).map(g => g.name).filter(Boolean); if (names.length && st.phase === 'form') fillGroups(names); } catch { /* keep what the page had */ }
    }
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
        head(),
        tabs(),
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
        h('div.zc-row', h('span.zc-lab', 'Run this on the machine'),
          h('div.zc-seg', { role: 'group', 'aria-label': 'System' }, h('button', { type: 'button', class: 'on', 'aria-pressed': 'true' }, 'Linux'),
            h('button', { type: 'button', disabled: true, title: WIN_WHY, 'data-cz-off': 'Windows' }, 'Windows'))),
        h('pre.zc-cmd', h('code.mono', { 'data-cz': 'command' }, z.command || '')),
        live ? h('div.zc-copy', copyButton(z.command || '', 'the command')) : null,
        h('div.zc-note', 'Windows: ' + WIN_WHY.replace(/^Not available yet: /, 'not available yet, ')));
      const dlTab = h('div.zc-pane.zc-files', { 'data-cz-pane': 'dl' },
        fileRow('file', 'Linux', z.download_name || 'potluck-zone.sh', live ? z.download_path : '', live ? '' : 'The script no longer works.'),
        fileRow('file', 'Windows', '', '', WIN_WHY),
        fileRow('usb', 'USB drive', '', '', USB_WHY));
      name.value = z.name; name.disabled = true; name.setAttribute('disabled', '');
      group.value = z.group || ''; group.disabled = true; group.setAttribute('disabled', '');
      return [
        head(),
        tabs(),
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
    readGroups();
    return root;
  } }).finally(() => { clearInterval(timer); });
}
