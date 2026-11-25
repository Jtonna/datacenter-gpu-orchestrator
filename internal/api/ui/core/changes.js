// The change flow shared by Profiles, Tiers and nodes, Settings and Audit:
//
//   pendingBar()      the bar that says "3 unsaved changes" with Review and Discard
//   reviewChanges()   dry run, show the diff, take a comment, save with If-Match,
//                     and handle 400 (messages) and 409 (someone else changed it)
//   savedBanner()     "Saved as revision 15" and the keys that need a restart
//   watchConfig()     the WebSocket "config" topic: refresh, or warn when editing
//
// REST: every write goes through core/api.js with ?dry_run=1 first, then
// If-Match: <revision> and the comment (X-Comment and the body field `comment`).
// On a 409 stale_revision: GET /api/audit (the entries after the revision the
// user started from, each with its diff).

import { h } from './dom.js';
import * as api from './api.js';
import { dialog, diffView, badge, banner, button } from './ui.js';
import { ago, plural } from './format.js';
import { listen } from './store.js';
import { href } from './router.js';
import { SOURCES, SECTION_NAMES, sourceBadge, sectionName } from './configdata.js';

let saving = 0;
export const isSaving = () => saving > 0;

// ---------- the pending bar ----------

// pendingBar({count, what, onReview, onDiscard}): sticks to the bottom of the screen.
export function pendingBar(o) {
  if (!o.count) return null;
  return h('div.banner.warn', { key: 'pending-bar', role: 'status', style: 'position:sticky;bottom:12px;z-index:8;box-shadow:0 8px 30px rgba(0,0,0,.5);background:var(--amber-bg)' },
    h('span.grow', h('b', plural(o.count, 'unsaved change')), o.what ? ' · ' + o.what : '', h('span.mut', { style: 'margin-left:8px;font-weight:600' }, 'Nothing is applied until you save.')),
    h('span.row', { style: 'gap:8px;margin-left:auto' },
      button('Discard', o.onDiscard, 'sm'),
      button('Review changes', o.onReview, 'pri sm')));
}

// ---------- saved, notice ----------

export function savedBanner(result, onClose) {
  if (!result) return null;
  const restart = result.restart_required || [];
  return h('div.banner.info', { key: 'saved-banner', role: 'status' },
    h('span.grow', h('b', result.revision ? `Saved as revision ${result.revision}. ` : 'Saved. '),
      result.changed === false ? 'Nothing changed.' : 'It is in force now; models that are already loaded keep the definition they started with.',
      restart.length ? [' ', h('b.warn-t', 'These keys take effect after the scheduler restarts: '), restart.map((k, i) => [i ? ', ' : '', h('span.mono', shortKey(k))]), '.'] : null,
      ' ', h('a', { href: href('audit') }, 'See it in Audit')),
    button('Dismiss', onClose, 'sm'));
}

// restartKey('settings.server_bin') -> 'server_bin'; 'fleet.url' stays. The one place that knows the prefix.
export const restartKey = k => String(k).replace(/^settings\./, '');
const shortKey = restartKey;
const who = ev => `${ev.actor || 'someone'} (${(SOURCES[ev.source] || [ev.source || 'unknown'])[0]})`;

// watchConfig({revision, pending, refresh, redraw}) listens for "config" messages.
// revision(): the revision the screen shows; pending(): the user has unsaved edits;
// refresh(): reload the screen's data; redraw(): draw again.
// notice: null or {ev, auto}; clear() removes it.
export function watchConfig(o) {
  let notice = null, timer = 0;
  const off = listen('config', ev => {
    if (isSaving()) return;
    if (ev && ev.revision != null && ev.revision <= (o.revision() || 0)) return;
    clearTimeout(timer);
    if (o.pending()) notice = { ev, auto: false };
    else {
      notice = { ev, auto: true };
      Promise.resolve(o.refresh()).catch(() => {});
      timer = setTimeout(() => { notice = null; o.redraw(); }, 12000);
    }
    o.redraw();
  });
  return {
    get notice() { return notice; },
    clear() { notice = null; clearTimeout(timer); },
    off() { off(); clearTimeout(timer); },
  };
}

export function noticeBanner(notice, o) {
  if (!notice) return null;
  const ev = notice.ev || {};
  const what = ev.section && SECTION_NAMES[ev.section] ? sectionName(ev.section).toLowerCase() + (ev.target ? ' ' + ev.target : '') : 'the configuration';
  if (notice.auto) {
    return h('div.banner.info', { key: 'notice', role: 'status' },
      h('span.grow', h('b', 'Updated. '), `${who(ev)} changed ${what}${ev.revision ? ' (revision ' + ev.revision + ')' : ''}; this screen shows the new state.`),
      button('Dismiss', o.onDismiss, 'sm'));
  }
  return h('div.banner.warn', { key: 'notice', role: 'status' },
    h('span.grow', h('b', 'The configuration changed while you were editing. '), `${who(ev)} changed ${what}${ev.revision ? ' (revision ' + ev.revision + ')' : ''}. Your edits are kept. If they overlap, saving will tell you.`),
    h('span.row', { style: 'gap:8px;margin-left:auto' }, button('Load the new state', o.onReload, 'sm'), button('Dismiss', o.onDismiss, 'sm')));
}

// ---------- review and save ----------

// o.requireComment: the save stays off until a comment is written (a profile
// saved from the Models panel or the profile editor: views/profiles.js saveProfile).
// reviewChanges({title, intro, saveLabel, getRevision, buildOps, reload, onInvalid, requireComment}) -> Promise of
//   {saved, partial, revision, restart_required, changed} or undefined when closed unsaved.
// An op is {label, method, path, body, params, forceable, count (how many changes it holds), summary (a node shown above the diff)}. A forceable op (delete of
// a profile) is sent with force=1 once the user ticked the box next to its warnings.
export function reviewChanges(o) {
  return dialog({ title: o.title || 'Review changes', wide: true, body: close => body(o, close) });
}

function body(o, close) {
  const root = h('div.col', { style: 'gap:14px' });
  const comment = h('textarea.textarea', { rows: 2, maxlength: 400, placeholder: 'Why are you making this change? It is kept in the audit log.', 'aria-label': 'Comment' });
  let ops = o.buildOps();
  const st = { phase: 'dry', res: [], messages: [], current: null, entries: null, error: '', force: false, done: [], result: null };

  let saveBtn = null, saveOff = false;
  const noComment = () => !!o.requireComment && !comment.value.trim();
  comment.addEventListener('input', () => { if (saveBtn) saveBtn.disabled = saveOff || noComment(); });
  const payload = op => (op.body && typeof op.body === 'object' && !Array.isArray(op.body) ? Object.assign({}, op.body, { comment: comment.value.trim() || undefined }) : op.body);

  async function dry() {
    st.phase = 'dry'; draw();
    try {
      const res = [];
      for (const op of ops) res.push(await api.send(op.method, op.path, payload(op), { dryRun: true, ifMatch: o.getRevision(), params: op.params }));
      st.res = res; st.phase = 'review'; st.force = false;
      draw();
      comment.focus();
    } catch (e) { fail(e); }
  }

  function fail(e) {
    const err = e && e.body && e.body.error;
    if (e.status === 400) {
      st.phase = 'invalid';
      st.messages = (err && err.messages && err.messages.length ? err.messages : [e.message]);
      if (o.onInvalid) o.onInvalid(st.messages);
    } else if (e.status === 409 && err && err.type === 'stale_revision') {
      st.phase = 'stale'; st.current = err.current_revision; st.entries = null;
      loadNewer();
    } else if (e.status === 409 && err && err.type === 'profile_referenced') {
      st.phase = 'error'; st.error = e.message;
    } else { st.phase = 'error'; st.error = e.message || 'The change failed.'; }
    draw();
  }

  async function loadNewer() {
    const base = o.getRevision();
    try {
      const r = await api.get('/api/audit', { limit: 200 });
      st.entries = (r.entries || []).filter(e => e.revision > base && (st.current == null || e.revision <= st.current)).reverse();
    } catch { st.entries = []; }
    draw();
  }

  async function save() {
    if (noComment()) { comment.focus(); return; }
    st.phase = 'saving'; st.done = []; draw();
    saving++;
    let rev = o.getRevision();
    let last = null;
    try {
      for (let i = 0; i < ops.length; i++) {
        if (!st.res[i] || !st.res[i].changed) continue;
        const op = ops[i];
        const params = Object.assign({}, op.params, op.forceable && (st.res[i].warnings || []).length ? { force: 1 } : null);
        last = await api.send(op.method, op.path, payload(op), { ifMatch: rev, params, comment: comment.value.trim() });
        st.done.push(op.label);
        if (last.revision) rev = last.revision;
      }
      const restart = [...new Set(st.res.flatMap(r => r.restart_required || []))];
      st.result = { saved: true, revision: rev, restart_required: restart, changed: st.done.length > 0 };
      close(st.result);
    } catch (e) {
      if (st.done.length) {
        st.result = { saved: true, partial: true, revision: rev, restart_required: [], changed: true };
        st.phase = 'error'; st.error = `${plural(st.done.length, 'change')} saved (${st.done.join(', ')}); the next one failed: ${e.message}`;
        draw();
      } else fail(e);
    } finally { setTimeout(() => { saving--; }, 1500); }
  }

  async function reapply() {
    st.phase = 'dry'; draw();
    try { await o.reload(st.current); } catch (e) { fail(e); return; }
    ops = o.buildOps();
    dry();
  }

  const diffBlock = (op, r) => h('div.col', { style: 'gap:6px' },
    h('div.row', { style: 'gap:8px' }, h('b', op.label), r.changed ? null : badge('No change', 'n')),
    r.changed ? [op.summary || null, diffView(r.diff)] : h('span.mut', 'This change would not alter the configuration.'));

  function draw() {
    const kids = [];
    const rev = o.getRevision();
    if (st.phase === 'dry') kids.push(h('p.mut', 'Checking the changes against the server…'), h('div.dlg-foot', button('Cancel', () => close(undefined), 'lg')));
    if (st.phase === 'review' || st.phase === 'saving') {
      const changed = st.res.reduce((n, r, i) => n + (r.changed ? (ops[i].count || 1) : 0), 0);
      const warnings = [...new Set(st.res.flatMap(r => r.warnings || []))];
      const restart = [...new Set(st.res.flatMap(r => r.restart_required || []))];
      const needForce = ops.some((op, i) => op.forceable && (st.res[i].warnings || []).length);
      kids.push(h('p', changed ? (o.intro || `This writes a new configuration revision on top of revision ${rev}. Nothing has been saved yet.`) : 'None of these edits would change the configuration.'));
      ops.forEach((op, i) => kids.push(diffBlock(op, st.res[i])));
      if (warnings.length) kids.push(banner('warn', h('span', h('b', 'Check this first. '), warnings.map((w, i) => [i ? ' ' : '', w, w.endsWith('.') ? '' : '.']))));
      if (restart.length) kids.push(banner('info', h('span', h('b', 'Needs a restart. '), 'These keys take effect after the scheduler restarts: ', restart.map((k, i) => [i ? ', ' : '', h('span.mono', shortKey(k))]), '. They are saved now.')));
      if (needForce) {
        const box = h('input', { type: 'checkbox', 'aria-label': 'Delete anyway' });
        box.addEventListener('change', () => { st.force = box.checked; draw(); });
        if (st.force) box.checked = true;
        kids.push(h('label.check', box, h('span', 'I understand. Delete it anyway.')));
      }
      kids.push(h('div.field', h('label', o.requireComment ? 'Comment (required)' : 'Comment'), comment));
      saveOff = st.phase === 'saving' || !changed || (needForce && !st.force);
      saveBtn = h('button', { class: 'btn lg pri', type: 'button', disabled: saveOff || noComment(), onclick: save },
        st.phase === 'saving' ? 'Saving…' : changed ? (o.saveLabel || `Save ${plural(changed, 'change')}`) : 'Nothing to save');
      kids.push(h('div.dlg-foot', button('Cancel', () => close(undefined), 'lg'), saveBtn));
    }
    if (st.phase === 'invalid') {
      kids.push(banner('err', h('span', h('b', 'The server did not accept this. '), 'Nothing was saved.')));
      kids.push(h('ul', { style: 'margin:0;padding-left:20px;display:flex;flex-direction:column;gap:6px' }, st.messages.map(m => h('li', h('span.err-t', m)))));
      kids.push(h('div.dlg-foot', button('Back to editing', () => close(undefined), 'lg pri')));
    }
    if (st.phase === 'stale') {
      kids.push(banner('err', h('span', h('b', 'Someone else changed the configuration first. '),
        `You started from revision ${rev}; it is now revision ${st.current}. Nothing of yours was saved.`)));
      if (st.entries == null) kids.push(h('p.mut', 'Loading what changed…'));
      else if (!st.entries.length) kids.push(h('p.mut', 'The audit log has no entries for the newer revisions.'));
      else {
        kids.push(h('b', 'What changed since revision ' + rev));
        const byRev = new Map();
        for (const e of st.entries) { if (!byRev.has(e.revision)) byRev.set(e.revision, []); byRev.get(e.revision).push(e); }
        for (const [r, es] of byRev) {
          kids.push(h('div.col', { style: 'gap:6px' },
            h('div.row', { style: 'gap:8px' }, h('span.mono', 'Revision ' + r), sourceBadge(es[0].source), h('span', es[0].actor || 'anonymous'), ago(es[0].ts),
              es[0].comment ? h('span.mut', '“' + es[0].comment + '”') : null),
            diffView(es.flatMap(e => (e.diff || []).map(d => Object.assign({}, d, { path: d.path })))),
          ));
        }
      }
      kids.push(h('p.mut', 'Apply my changes on top of the new revision to see how they differ now, then save again.'));
      kids.push(h('div.dlg-foot', button('Cancel', () => close(undefined), 'lg'), button(`Re-apply my changes on revision ${st.current}`, reapply, 'lg pri')));
    }
    if (st.phase === 'error') {
      kids.push(banner('err', st.error));
      kids.push(h('div.dlg-foot', button('Close', () => close(st.result || undefined), 'lg pri')));
    }
    root.replaceChildren(...kids);
  }
  dry();
  return root;
}

