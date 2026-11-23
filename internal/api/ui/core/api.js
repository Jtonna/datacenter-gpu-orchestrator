// REST helper. REST is only for what the page pulls on demand or pushes;
// nothing here runs on a timer (live data comes over the WebSocket).
// Every write carries X-Source: ui and X-Actor (the name set in the shell).

const ACTOR_KEY = 'llm-scheduler.actor';

export function getActor() {
  try { return localStorage.getItem(ACTOR_KEY) || 'web'; } catch { return 'web'; }
}
export function setActor(name) {
  const v = String(name || '').trim().slice(0, 64) || 'web';
  try { localStorage.setItem(ACTOR_KEY, v); } catch { /* private mode */ }
  return v;
}

// The API root is what comes before /ui/ in the page's path, so the page also
// works behind a path prefix, and at /ui/classic/ as well as at /ui/.
const ROOT = (p => { const i = p.lastIndexOf('/ui/'); return i >= 0 ? p.slice(0, i) : new URL('..', new URL('.', location.href)).pathname.replace(/\/$/, ''); })(location.pathname);
export const url = (path, params) => {
  let u = ROOT + path;
  if (params) {
    const q = new URLSearchParams();
    for (const k in params) {
      const v = params[k];
      if (v != null && v !== '' && v !== false) q.set(k, v === true ? '1' : v);
    }
    const s = q.toString();
    if (s) u += (u.includes('?') ? '&' : '?') + s;
  }
  return u;
};

export class ApiError extends Error {
  constructor(status, message, body) {
    super(message);
    this.status = status;
    this.body = body; // the parsed JSON answer, when there was one
  }
}

// Header values must be plain printable ASCII.
const hdr = v => String(v).replace(/\s+/g, ' ').replace(/[^\x20-\x7e]/g, '?');

async function parse(resp) {
  const text = await resp.text();
  let body = null;
  if (text) { try { body = JSON.parse(text); } catch { body = text; } }
  if (!resp.ok) {
    const msg = (body && body.error && (body.error.message || body.error)) || (typeof body === 'string' && body) || resp.statusText || 'request failed';
    throw new ApiError(resp.status, String(msg), body);
  }
  return body;
}

// get('/api/profiles') or get('/api/history', {limit: 50})
export async function get(path, params, opts = {}) {
  let resp;
  try { resp = await fetch(url(path, params), { headers: { Accept: 'application/json' }, signal: opts.signal }); } catch (e) {
    if (e.name === 'AbortError') throw e;
    throw new ApiError(0, 'The scheduler did not answer. Check that it is running and reachable.', null);
  }
  return parse(resp);
}

// send('PATCH', '/api/config/settings', {queue_timeout: '900s'},
//      {dryRun: true, ifMatch: 14, comment: 'why'})
// opts.dryRun adds ?dry_run=1, opts.ifMatch sets If-Match, opts.comment sets
// X-Comment. A 409 or 422 throws ApiError with .status and .body.
export async function send(method, path, body, opts = {}) {
  const headers = { Accept: 'application/json', 'X-Source': 'ui', 'X-Actor': hdr(getActor()) };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (opts.ifMatch != null) headers['If-Match'] = String(opts.ifMatch);
  if (opts.comment) headers['X-Comment'] = hdr(opts.comment);
  const params = Object.assign({}, opts.params, opts.dryRun ? { dry_run: 1 } : null);
  let resp;
  try {
    resp = await fetch(url(path, params), { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  } catch {
    throw new ApiError(0, 'The scheduler did not answer. Nothing was changed.', null);
  }
  return parse(resp);
}

// upload('PUT', '/api/models/<key>/icon', file, {params: {scope: 'family'}})
// sends a file (or a Blob) as the raw body with its own content type.
export async function upload(method, path, blob, opts = {}) {
  const headers = { Accept: 'application/json', 'X-Source': 'ui', 'X-Actor': hdr(getActor()), 'Content-Type': blob.type || 'application/octet-stream' };
  let resp;
  try { resp = await fetch(url(path, opts.params), { method, headers, body: blob }); } catch {
    throw new ApiError(0, 'The scheduler did not answer. Nothing was changed.', null);
  }
  return parse(resp);
}

export const post = (path, body, opts) => send('POST', path, body, opts);
export const put = (path, body, opts) => send('PUT', path, body, opts);
export const patch = (path, body, opts) => send('PATCH', path, body, opts);
export const del = (path, opts) => send('DELETE', path, undefined, opts);
