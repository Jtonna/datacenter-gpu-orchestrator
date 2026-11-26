// The models trash (models slice 3, v65) in the server's shapes
// (internal/api/trash.go): GET /api/models/trash, POST
// /api/models/delete/{file} (dry run, interrupt), POST
// /api/models/trash/{id}/restore, POST /api/models/trash/{id}/remove
// ("Remove for good": dry run, then the real remove; the list's removed
// tombstones), and the trash figures of the catalog's disk block
// (trash_bytes, trash_count, next_purge_at, trash_purge_on). Purge is off,
// as on the server: nothing is removed automatically.
//
// States (--trash <state>, with --models five):
//   empty       nothing in the trash; every delete works
//   two         two items: phi-4-14b (in the trash, 3 profiles, deleted 3
//               days ago) and llama-3.3-70b (interrupted: a delete stopped and
//               the file is in neither place), one repair at the last start;
//               deletes and restores work
//   in-use      as two; gemma-4-27b (loaded, 2 requests in flight) also has
//               1 request queued for gemma-4-27b:long: refused in_use
//               without interrupt, the dry run with interrupt says what it
//               would interrupt, the delete with interrupt works
//   run         as two; benchmark run r-12 uses qwen3-14b: refused in_use,
//               with interrupt too
//   read-only   as two; the models folder is read-only: models_writable is
//               false, every real delete and restore answers 409 read_only
//               (dry runs answer 200). Every other state: models_writable true
//   taken       as two; phi-4-14b's restore answers 409 profiles_taken,
//               and works with rename_taken
//   incomplete  as in-use; the delete with interrupt answers 409
//               interrupt_incomplete
//   again       as two; a delete of a file nothing holds answers 409
//               loaded_again
//   not-regular as two; the remove of phi-4-14b answers 409 not_regular
//               (dry run and real), the server's sentence
// The states two and later carry two tombstones under removed: one removed
// for good by the owner (with a comment), one by the timed clearing. A
// remove that works takes the item out of the list into removed; under
// read-only its dry run says read_only true and the real one answers 409.
// A delete that works moves the file out of the catalog into the list; a
// restore takes it out of the list (and back into the catalog).

const GB = 1e9, DAY = 24 * 3600e3, DAYS = 30;
export const TRASH_STATES = ['empty', 'two', 'in-use', 'run', 'read-only', 'taken', 'incomplete', 'again', 'not-regular'];

// goDur(600) -> "10m0s", as Go marshals a profile's ttl
const goDur = s => { if (!s) return '0s'; const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), x = s % 60; return (h ? h + 'h' : '') + (h || m ? m + 'm' : '') + x + 's'; };
const spec = (file, ctx, needs, ttl) => Object.assign({ model: file, ctx }, needs ? { needs_mib: needs } : null, ttl != null ? { ttl: goDur(ttl) } : null);

let items = [];
let repairs = [];
let removed = []; // tombstones: removed for good
const deleted = new Map(); // catalog file -> its catalog entry, while it is in the trash
let seq = 0;
let writable = true; // models_writable in the disk block and the list

export function seedTrash(state, now = Date.now()) {
  items = []; repairs = []; removed = []; deleted.clear(); seq = 0;
  writable = state !== 'read-only';
  if (!state || state === 'empty') return;
  const phi = 'phi-4-14b-q4_k_m.gguf', llama = 'llama-3.3-70b-q3_k_s.gguf';
  const at1 = now - 3 * DAY - 4 * 3600e3, at2 = now - 28 * DAY;
  items.push({ id: 'a1b2c3d4e5f6', file: phi, size_bytes: Math.round(8.4 * GB), deleted_at: at1, purge_at: at1 + DAYS * DAY, state: 'trashed', profile_count: 3,
    profiles: [{ id: 'phi-4-14b', spec: spec(phi, 8192, 9480, 600) }, { id: 'phi-4-14b:16k', spec: spec(phi, 16384, 10350, 600) }, { id: 'phi-4-14b:json', spec: spec(phi, 8192, 9480, 300) }],
    revision: 41, actor: 'owner', comment: 'not used since qwen3-14b arrived', problem: null, in_trash_folder: true });
  const why = `a delete stopped and the file is not in exactly one place (in the folder: false, in .trash/f6e5d4c3b2a1: false); nothing was moved`;
  items.push({ id: 'f6e5d4c3b2a1', file: llama, size_bytes: Math.round(26.2 * GB), deleted_at: at2, purge_at: at2 + DAYS * DAY, state: 'interrupted', profile_count: 1,
    profiles: [{ id: 'llama-3.3-70b', spec: spec(llama, 8192, 31200, 900) }], revision: 37, actor: 'owner', comment: 'too slow on two zones', problem: why, in_trash_folder: false });
  repairs.push(`${llama} (trash id f6e5d4c3b2a1): needs a look: ${why}`);
  const tomb = (id, file, gb, delAgo, remAgo, reason, by, comment, ids) => ({ id, file, size_bytes: Math.round(gb * GB), deleted_at: now - delAgo * DAY, deleted_by: 'owner',
    deleted_comment: 'not used', purge_at: now - delAgo * DAY + DAYS * DAY, removed_at: now - remAgo * DAY, state: 'removed', reason, removed_by: by, comment, profile_ids: ids, revision: 30 });
  removed.push(tomb('0a0b0c0d0e0f', 'mistral-7b-v0.3-q4_k_m.gguf', 4.4, 12, 2, 'owner', 'owner', 'the disk was full before the 72B arrived', ['mistral-7b', 'mistral-7b:32k']));
  removed.push(tomb('1a1b1c1d1e1f', 'tinyllama-1.1b-q8_0.gguf', 1.2, 45, 15, 'clearing', 'potluck', '', ['tinyllama']));
}

// summary: the records whose file is in the trash (trashed or interrupted), as summarizeTrash
function summary() {
  let bytes = 0, count = 0, next = 0;
  for (const r of items) {
    if (r.state !== 'trashed' && r.state !== 'interrupted') continue;
    bytes += r.size_bytes; count++;
    if (r.state === 'trashed' && (!next || r.purge_at < next)) next = r.purge_at;
  }
  return { bytes, count, next: next || null };
}

export const trashList = () => {
  const s = summary();
  return { items: items.map(r => Object.assign({}, r)), count: s.count, total_bytes: s.bytes, next_purge_at: s.next, trash_days: DAYS, purge_on: false, repairs_at_start: repairs.slice(), removed: removed.map(t => Object.assign({}, t)), models_writable: writable };
};

// withTrash(doc) -> the catalog without the files in the trash, its disk block with the trash figures
export function withTrash(doc) {
  const s = summary();
  const models = doc.models.filter(m => !deleted.has(m.key));
  const disk = Object.assign({}, doc.disk, { trash_bytes: s.bytes, trash_count: s.count, next_purge_at: s.next, trash_purge_on: false, models_writable: writable });
  disk.model_bytes = models.reduce((t, m) => t + (m.size_bytes || 0), 0);
  disk.files = models.length;
  return Object.assign({}, doc, { models, disk });
}

const refuse = (status, reason, message, extra) => ({ status, body: { error: Object.assign({ message, type: status === 404 ? 'not_found_error' : 'invalid_request_error', param: null, code: status }, reason ? { reason } : null, extra || null) } });
const READ_ONLY = op => `the models folder is read-only here: nothing was changed (${op}: read-only file system). Deleting and restoring model files need the scheduler's models folder mounted read-write.`;

// busy(state, entry) -> what holds the file now: {instances, queued, runs}, as trashBusy
function busy(state, m) {
  const out = {};
  if (m.instances && m.instances.length) out.instances = m.instances.map(i => ({ profile: i.profile, state: i.state, in_flight: i.in_flight || 0, nodes: (i.nodes || []).slice() }));
  if ((state === 'in-use' || state === 'incomplete') && m.key === 'gemma-4-27b-it-Q4_K_M.gguf') out.queued = [{ profile: 'gemma-4-27b:long', waiters: 1 }];
  if (state === 'run' && m.key === 'qwen3-14b-Q5_K_M.gguf') out.runs = ['r-12'];
  return out;
}
function whyOf(b) {
  const w = [];
  for (const i of b.instances || []) w.push(`${i.profile} is ${i.state}`);
  for (const q of b.queued || []) w.push(`${q.waiters} request(s) for ${q.profile} are queued`);
  if (b.runs) w.push(`benchmark run(s) ${b.runs.join(', ')} use it`);
  return w;
}

// deleteRoute(state, file, body, query, catalog, now) -> {status, body}, as handleModelDelete
export function deleteRoute(state, file, body, query, catalog, now = Date.now()) {
  body = body || {};
  const comment = String(body.comment || '').trim();
  if (!comment) return refuse(400, null, 'a comment is required (body {"comment": "..."} or the X-Comment header)');
  const dry = body.dry_run === true || /^(1|true)$/.test(String(query.get('dry_run') || ''));
  const interrupt = body.interrupt === true;
  if (file.startsWith('command:')) return refuse(400, 'not_a_file', `${file} is a command profile: it has no model file to delete`);
  const m = (catalog.models || []).find(x => x.key === file && x.kind !== 'command');
  if (!m) {
    const rec = items.find(r => r.file === file);
    if (rec) return refuse(409, 'in_trash', `${file} is in the trash since ${new Date(rec.deleted_at).toUTCString().slice(5, 22)} UTC (trash id ${rec.id})`, { trash_id: rec.id });
    return refuse(404, 'not_found', `no model file ${file} in the models folder`);
  }
  const b = busy(state, m), why = whyOf(b);
  if (why.length && interrupt && b.runs) return refuse(409, 'in_use', `${file} is in use and was not deleted: ${why.join('; ')}. interrupt does not stop benchmark runs: cancel them or wait for them first.`, b);
  if (why.length && !interrupt) return refuse(409, 'in_use', `${file} is in use and was not deleted: ${why.join('; ')}. Unload it (or wait for the runs) first, or delete with interrupt.`, b);
  const ids = (m.profiles || []).map(p => p.id).sort();
  const purgeAt = now + DAYS * DAY;
  const figures = { file, size_bytes: m.size_bytes, profiles: ids, trash_days: DAYS, purge_at: purgeAt, purge_on: false };
  if (why.length) figures.would_interrupt = b;
  if (dry) return { status: 200, body: Object.assign(figures, { dry_run: true }) };
  const id = 'c0ffee' + String(100000 + ++seq).slice(-6);
  if (state === 'read-only') return refuse(409, 'read_only', READ_ONLY(`mkdir /models/.trash/${id}`));
  let interrupted = null;
  if (why.length) {
    const insts = (b.instances || []).map(i => Object.assign({ waiters: 0 }, i));
    interrupted = { instances: insts, requests_cut: insts.reduce((t, i) => t + i.in_flight + i.waiters, 0), queued_cancelled: (b.queued || []).reduce((t, q) => t + q.waiters, 0) };
    if (state === 'incomplete') {
      const still = [`${(b.instances || [{ profile: m.key }])[0].profile} is stopping`];
      return refuse(409, 'interrupt_incomplete', `${file} was not deleted: after the interrupt, within 5s, still there: ${still.join('; ')}. Nothing was moved; the profiles are unchanged.`, { interrupted, still });
    }
  }
  if (state === 'again') return refuse(409, 'loaded_again', `${file} was not deleted: it was loaded again meanwhile (${(m.profiles[0] || { id: m.key }).id} is starting); the profiles were put back and nothing was moved`);
  deleted.set(file, m);
  items.unshift({ id, file, size_bytes: m.size_bytes, deleted_at: now, purge_at: purgeAt, state: 'trashed', profile_count: ids.length,
    profiles: (m.profiles || []).map(p => ({ id: p.id, spec: spec(file, p.ctx, p.needs_mib, p.ttl_s) })), revision: 52 + seq, actor: 'owner', comment, problem: null, in_trash_folder: true });
  const out = { trash_id: id, file, size_bytes: m.size_bytes, deleted_at: now, purge_at: purgeAt, trash_days: DAYS, purge_on: false, profiles_removed: ids,
    revision: ids.length ? 52 + seq : 0, warnings: [], audit_id: 300 + seq };
  if (interrupted) out.interrupted = interrupted;
  return { status: 200, body: out };
}

// restoreRoute(state, id, body) -> {status, body}, as handleModelRestore
export function restoreRoute(state, id, body) {
  body = body || {};
  const comment = String(body.comment || '').trim();
  if (!comment) return refuse(400, null, 'a comment is required (body {"comment": "..."} or the X-Comment header)');
  const rec = items.find(r => r.id === id);
  if (!rec) return refuse(404, 'not_found', `no trash item ${id}: see GET /api/models/trash`);
  if (rec.state !== 'trashed') return refuse(409, 'not_restorable', `trash item ${id} (${rec.file}) is ${rec.state}, not in the trash: ${rec.problem || ''}`);
  const ids = rec.profiles.map(p => p.id);
  const renameTaken = body.rename_taken === true;
  if (state === 'taken' && rec.file.startsWith('phi-4-14b') && !renameTaken) {
    const taken = ['profile phi-4-14b'];
    return refuse(409, 'profiles_taken', `these names of ${rec.file}'s profiles are taken now: ${taken.join(', ')}. Restore with rename_taken: true to add them as <id>-restored (taken aliases are left off)`, { taken });
  }
  if (state === 'read-only') return refuse(409, 'read_only', READ_ONLY(`rename /models/.trash/${id}/${rec.file} /models/${rec.file}`));
  const renamed = state === 'taken' && renameTaken && rec.file.startsWith('phi-4-14b') ? { 'phi-4-14b': 'phi-4-14b-restored' } : {};
  items = items.filter(r => r !== rec);
  deleted.delete(rec.file);
  const restored = ids.map(p => renamed[p] || p).sort();
  return { status: 200, body: { file: rec.file, trash_id: id, profiles_restored: restored, renamed, aliases_dropped: [], revision: restored.length ? 60 + seq : 0, audit_id: 400 + seq } };
}

// removeRoute(state, id, body, query, now) -> {status, body}, as handleTrashRemove
export function removeRoute(state, id, body, query, now = Date.now()) {
  body = body || {};
  const comment = String(body.comment || '').trim();
  if (!comment) return refuse(400, null, 'a comment is required (body {"comment": "..."} or the X-Comment header)');
  const dry = body.dry_run === true || /^(1|true)$/.test(String(query.get('dry_run') || ''));
  const tomb = removed.find(t => t.id === id);
  if (tomb) return refuse(410, 'already_removed', `${tomb.file} (trash id ${id}) was removed for good on ${new Date(tomb.removed_at).toISOString()} by ${tomb.removed_by}`);
  const rec = items.find(r => r.id === id);
  if (!rec) return refuse(404, 'not_found', `no trash item ${id}: see GET /api/models/trash`);
  if (rec.state !== 'trashed') return refuse(409, 'needs_a_look', `${rec.file} (trash id ${id}) was not removed: its record needs a look (${rec.problem}). Restore or settle it first. Nothing was changed.`);
  if (state === 'not-regular') return refuse(409, 'not_regular', `${rec.file} was not removed: .trash/${id}/${rec.file} is a symbolic link. Potluck erases only a plain file in a plain trash folder; nothing was changed`);
  const ids = rec.profiles.map(p => p.id).sort();
  const daysEarly = Math.max(0, Math.ceil((rec.purge_at - now) / DAY));
  const figures = { file: rec.file, trash_id: id, size_bytes: rec.size_bytes, profiles: ids, deleted_at: rec.deleted_at, purge_at: rec.purge_at, days_early: daysEarly, cannot_undo: true, purge_on: false,
    message: `This cannot be undone. ${rec.file} and its ${ids.length} profile${ids.length === 1 ? '' : 's'} are erased now, ${daysEarly} days before they would have been. ${rec.size_bytes >= 100e6 ? (rec.size_bytes / 1e9).toFixed(1) + ' GB' : rec.size_bytes >= 1e6 ? Math.round(rec.size_bytes / 1e6) + ' MB' : 'under 1 MB'} is freed.` };
  if (dry) return { status: 200, body: Object.assign(figures, { dry_run: true, read_only: state === 'read-only' }) };
  if (state === 'read-only') return refuse(409, 'read_only', READ_ONLY(`unlink /models/.trash/${id}/${rec.file}`));
  items = items.filter(r => r !== rec);
  deleted.delete(rec.file);
  removed.unshift({ id, file: rec.file, size_bytes: rec.size_bytes, deleted_at: rec.deleted_at, deleted_by: rec.actor, deleted_comment: rec.comment, purge_at: rec.purge_at, removed_at: now,
    state: 'removed', reason: 'owner', removed_by: 'owner', comment, profile_ids: ids, revision: rec.revision });
  return { status: 200, body: { file: rec.file, trash_id: id, removed_at: now, freed_bytes: rec.size_bytes, profiles_removed: ids, reason: 'owner', cannot_undo: true, audit_id: 500 + ++seq } };
}
