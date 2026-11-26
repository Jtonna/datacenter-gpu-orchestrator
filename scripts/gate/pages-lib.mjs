// pages-lib.mjs: the rules of the every-page drawing harness (pages.mjs),
// kept apart from the browser so that `node --test scripts/gate/` checks
// them in a second without one.

// ---------- arguments ----------

export const USAGE = `pages.mjs: draw every page of the scheduler's web UI, read-only.

usage: node scripts/gate/pages.mjs --base URL [--out DIR] [--widths 1536,1280]
         [--height 900] [--wait MS] [--max-pages N] [--min-chars N]
         [--allow-post PATH ...] [--strict] [--dry-run]

--base URL       a running scheduler, http://HOST:PORT. Point it at a COPY:
                 the harness does not check that the server is not live.
--out DIR        the PNGs and summary.json (default: pages-out)
--widths LIST    viewport widths in CSS pixels (default 1536,1280)
--height N       the viewport height; each picture is the full page (900)
--wait MS        how long a page may draw before it is measured (2500)
--max-pages N    the most routes drawn (default 120)
--min-chars N    the squeezed-text rule's threshold in characters (12)
--allow-post P   let a POST to the path P through (repeatable), for a dry-run
                 endpoint such as /api/plan. Every other request that is not
                 a GET is aborted in the browser, so nothing is written.
--strict         exit 1 when a page scrolls sideways, has a squeezed text
                 or logs a console error (default: report only, exit 0)
--dry-run        list the routes found from the API and the shell; no pictures

Routes: the shell's own links (its navigation), the detail routes computed from
the API's answers (the first profile, model, trash item, run, group, zone),
and every link found on a drawn page whose shape is new. A shape is the route
with each data id replaced by *: #/profiles/<id> is drawn once, for the first
id found, not once per profile.

Per page and width it measures:
  sideways        the page scrolls sideways (documentElement.scrollWidth is
                  wider than the viewport)
  squeezed        texts of at least --min-chars characters that are clipped
                  (scrollWidth or scrollHeight beyond the box while the
                  overflow is hidden, clipped or ellipsed; a clip whose full
                  text is in a title attribute is counted apart, as titled)
                  or whose box is narrower than --min-chars characters (at
                  half the font size per character) while its text wraps onto
                  two lines or more; a screen-reader-only text (a box of
                  2 px or less, or a CSS clip) is not judged
  console_errors  console errors and uncaught exceptions, less the "failed to
                  load" errors of the writes the harness aborted itself
  blocked_writes  the non-GET requests the harness aborted

Output: one line per page and width, and <out>/summary.json.
Exit 0; 1 with --strict and a finding; 2 when the run could not be made.
`;

export function parseArgs(argv) {
  const o = { base: '', out: 'pages-out', widths: [1536, 1280], height: 900, wait: 2500, maxPages: 120, minChars: 12,
    allowPost: [], strict: false, dryRun: false, help: false };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    const val = () => {
      if (i + 1 >= argv.length) throw new Error(a + ' needs a value');
      return argv[++i];
    };
    const num = () => {
      const n = Number(val());
      if (!Number.isInteger(n) || n <= 0) throw new Error(a + ' needs a positive whole number');
      return n;
    };
    switch (a) {
      case '-h': case '--help': o.help = true; break;
      case '--base': o.base = val().replace(/\/+$/, ''); break;
      case '--out': o.out = val(); break;
      case '--widths':
        o.widths = val().split(',').map(s => Number(s.trim()));
        if (!o.widths.length || o.widths.some(w => !Number.isInteger(w) || w < 200)) throw new Error('--widths: whole numbers of at least 200');
        break;
      case '--height': o.height = num(); break;
      case '--wait': o.wait = num(); break;
      case '--max-pages': o.maxPages = num(); break;
      case '--min-chars': o.minChars = num(); break;
      case '--allow-post': o.allowPost.push(val()); break;
      case '--strict': o.strict = true; break;
      case '--dry-run': o.dryRun = true; break;
      default: throw new Error('unknown argument ' + a);
    }
  }
  if (!o.help && !/^https?:\/\/[^/]+$/.test(o.base)) throw new Error('--base must be http://HOST:PORT');
  return o;
}

// ---------- writes ----------

// writeVerdict: 'pass' for a GET (or HEAD, OPTIONS), and for a POST to a
// path the caller allowed; 'abort' for every other request.
export function writeVerdict(method, url, allowPost) {
  const m = String(method).toUpperCase();
  if (m === 'GET' || m === 'HEAD' || m === 'OPTIONS') return 'pass';
  let path;
  try { path = new URL(url).pathname; } catch { return 'abort'; }
  if (m === 'POST' && allowPost.includes(path)) return 'pass';
  return 'abort';
}

// consoleCounts: a console error ({text, url}: url is the message's source)
// counts, unless it is the browser's report of a request the harness aborted
// (its url is that request's), or a "Failed to fetch" on a page where the
// harness aborted a write (the page's own report of the refused write).
export function consoleCounts(msg, abortedUrls) {
  if (msg.url && abortedUrls.has(msg.url)) return false;
  if (abortedUrls.size && /Failed to fetch/i.test(msg.text || '')) return false;
  return true;
}

// ---------- routes ----------

// PANEL_PARAMS: query parts that open a panel on a datum (a model, a trash
// item, a zone, a profile); kept in a route. Other query parts (filters,
// period, page) are dropped.
export const PANEL_PARAMS = ['model', 'item', 'zone', 'profile', 'session', 'suite'];

// parseRoute: '#/models/a.gguf?model=x&page=2' -> {view, rest, query}
export function parseRoute(href) {
  const i = href.indexOf('#/');
  if (i < 0) return null;
  const raw = href.slice(i + 2);
  const [path, qs] = raw.split('?');
  const parts = path.split('/').filter(Boolean).map(s => { try { return decodeURIComponent(s); } catch { return s; } });
  if (!parts.length) return null;
  const query = {};
  for (const [k, v] of new URLSearchParams(qs || '')) if (PANEL_PARAMS.includes(k) && v) query[k] = v;
  return { view: parts[0], rest: parts.slice(1), query };
}

export function routeString(r) {
  const path = [r.view, ...r.rest].map(encodeURIComponent).join('/');
  const q = Object.keys(r.query).sort().map(k => k + '=' + encodeURIComponent(r.query[k])).join('&');
  return path + (q ? '?' + q : '');
}

// shapeOf: the route with each id replaced by *: a segment is an id when the
// API listed it (ids) or when it holds a digit.
export function shapeOf(r, ids) {
  const seg = s => (ids.has(s) || /\d/.test(s) ? '*' : s);
  const q = Object.keys(r.query).sort().map(k => k + '=*').join('&');
  return [r.view, ...r.rest.map(seg)].join('/') + (q ? '?' + q : '');
}

// list: the first array of a document: the document itself, or the first
// of its values that is an array of objects.
export function list(doc) {
  if (Array.isArray(doc)) return doc;
  if (!doc || typeof doc !== 'object') return [];
  for (const v of Object.values(doc)) if (Array.isArray(v) && v.length && typeof v[0] === 'object') return v;
  return [];
}

const pick = (o, keys) => {
  for (const k of keys) if (o && o[k] != null && o[k] !== '') return String(o[k]);
  return null;
};

// SOURCES: the detail routes computed from the API. Each reads one endpoint
// and makes routes of its FIRST item; every item's id joins the ids that
// shapes are made with. An endpoint that fails is skipped (noted).
export const SOURCES = [
  { path: '/api/profiles', ids: ['id'], routes: (first, all) => {
    const p = all.find(x => !x.generated) || first;
    const id = pick(p, ['id']);
    return id ? ['profiles/' + encodeURIComponent(id), 'placement/' + encodeURIComponent(id), 'profiles/new'] : ['profiles/new'];
  } },
  { path: '/api/models', ids: ['key', 'file'], routes: first => {
    const k = pick(first, ['key', 'file']);
    return k ? ['models/' + encodeURIComponent(k), 'models?model=' + encodeURIComponent(k)] : [];
  } },
  { path: '/api/models/trash', ids: ['trash_id', 'id'], routes: first => {
    const id = pick(first, ['trash_id', 'id']);
    return ['models/deleted'].concat(id ? ['models/deleted?item=' + encodeURIComponent(id)] : []);
  } },
  { path: '/api/bench/runs', ids: ['number', 'id', 'run_id'], routes: first => {
    const n = pick(first, ['number', 'id', 'run_id']);
    return ['benchmarks/runs'].concat(n ? ['benchmarks/runs/' + encodeURIComponent(n)] : []);
  } },
  { path: '/api/bench/groups', ids: ['id', 'suite_id'], routes: first => {
    const g = pick(first, ['id', 'suite_id']);
    return g ? ['benchmarks/all/' + encodeURIComponent(g)] : [];
  } },
  { path: '/api/nodes', ids: ['id', 'hostname', 'name'], routes: first => {
    const z = pick(first, ['hostname', 'name', 'id']);
    return z ? ['zones/' + encodeURIComponent(z)] : [];
  } },
  { path: '/api/config/profiles', ids: [], routes: () => [] },
];

// apiRoutes: the seeds and the ids, from get(path) -> JSON or throws.
export async function apiRoutes(get) {
  const routes = [], ids = new Set(), notes = [];
  for (const s of SOURCES) {
    let doc;
    try { doc = await get(s.path); } catch (e) { notes.push(s.path + ': ' + e.message); continue; }
    let items = list(doc);
    if (s.path === '/api/config/profiles' && doc && doc.profiles && !Array.isArray(doc.profiles)) items = Object.keys(doc.profiles).map(id => ({ id }));
    for (const it of items) for (const k of ['id', 'key', 'file', 'trash_id', 'number', 'run_id', 'suite_id', 'hostname', 'name']) {
      if (it && it[k] != null && it[k] !== '' && (s.ids.includes(k) || s.path === '/api/config/profiles')) ids.add(String(it[k]));
    }
    routes.push(...s.routes(items[0] || null, items));
  }
  return { routes, ids, notes };
}

// RouteQueue keeps the routes to draw: one per shape, in the order found.
export class RouteQueue {
  constructor(ids, max) { this.ids = ids; this.max = max; this.shapes = new Set(); this.items = []; this.skipped = 0; }
  add(href) {
    const r = parseRoute(href.startsWith('#') ? href : '#/' + href);
    if (!r) return false;
    const shape = shapeOf(r, this.ids);
    if (this.shapes.has(shape)) return false;
    if (this.items.length >= this.max) { this.skipped++; return false; }
    this.shapes.add(shape);
    this.items.push({ route: routeString(r), shape });
    return true;
  }
}

// fileName: a route as a file name.
export function fileName(route, width) {
  const s = route.replace(/[^A-Za-z0-9._-]+/g, '_').replace(/^_+|_+$/g, '').slice(0, 120) || 'root';
  return `${s}-${width}.png`;
}

// ---------- the squeezed-text rule ----------

// squeezeVerdict: m is one element's measures, as the browser reads them:
// {text, width, height, clientWidth, scrollWidth, clientHeight, scrollHeight,
//  overflowX, overflowY, textOverflow, fontSizePx, lineHeightPx, hasTitle,
//  visuallyHidden}.
// Only texts of at least minChars characters are judged, and not a text
// hidden on purpose for screen readers (a box of 2 px or less, or a clip).
// Returns null (fine), 'clipped', 'titled' (clipped, with the full text in a
// title) or 'narrow': a box narrower than minChars characters (half the font
// size each) whose text wraps onto two lines or more.
export function squeezeVerdict(m, minChars) {
  const text = (m.text || '').replace(/\s+/g, ' ').trim();
  if (text.length < minChars || m.width <= 2 || m.height <= 2 || m.visuallyHidden) return null;
  const hides = v => v === 'hidden' || v === 'clip';
  const clippedX = m.scrollWidth > m.clientWidth + 1 && (hides(m.overflowX) || m.textOverflow === 'ellipsis');
  const clippedY = m.scrollHeight > m.clientHeight + 1 && hides(m.overflowY);
  if (clippedX || clippedY) return m.hasTitle ? 'titled' : 'clipped';
  const font = m.fontSizePx || 16, charPx = font * 0.5, line = m.lineHeightPx || font * 1.2;
  if (m.width < minChars * charPx && m.height >= line * 1.8) return 'narrow';
  return null;
}

// measurePage runs IN THE PAGE (page.evaluate(measurePage, {minChars,
// verdict: squeezeVerdict.toString()})): the sideways scroll and the squeezed
// texts. A text is an element's own text nodes (not its children's).
export function measurePage(arg) {
  const verdict = (0, eval)('(' + arg.verdict + ')');
  const de = document.documentElement;
  const out = { sideways: de.scrollWidth > de.clientWidth + 1, scrollWidth: de.scrollWidth, clientWidth: de.clientWidth,
    squeezed: 0, titled: 0, samples: [] };
  for (const el of document.body.querySelectorAll('*')) {
    let own = '';
    for (const n of el.childNodes) if (n.nodeType === 3) own += n.nodeValue;
    own = own.replace(/\s+/g, ' ').trim();
    if (own.length < arg.minChars) continue;
    const cs = getComputedStyle(el);
    if (cs.display === 'none' || cs.visibility === 'hidden') continue;
    const r = el.getBoundingClientRect();
    const v = verdict({ text: own, width: r.width, height: r.height, lineHeightPx: parseFloat(cs.lineHeight) || 0,
      visuallyHidden: (cs.clip && cs.clip !== 'auto') || (cs.clipPath && cs.clipPath !== 'none'), clientWidth: el.clientWidth || r.width, scrollWidth: el.scrollWidth || r.width,
      clientHeight: el.clientHeight || r.height, scrollHeight: el.scrollHeight || r.height, overflowX: cs.overflowX, overflowY: cs.overflowY,
      textOverflow: cs.textOverflow, fontSizePx: parseFloat(cs.fontSize), hasTitle: (el.getAttribute('title') || '').length >= own.length }, arg.minChars);
    if (!v) continue;
    if (v === 'titled') { out.titled++; continue; }
    out.squeezed++;
    if (out.samples.length < 8) out.samples.push({ rule: v, tag: el.tagName.toLowerCase() + (el.className && typeof el.className === 'string' ? '.' + el.className.trim().split(/\s+/).join('.') : ''),
      width: Math.round(r.width), text: own.slice(0, 80) });
  }
  return out;
}

// pageLine: the one line of a page.
export function pageLine(p) {
  return `PAGE ${p.width} #/${p.route} sideways=${p.sideways ? 'YES' : 'no'} squeezed=${p.squeezed} titled=${p.titled} console_errors=${p.console_errors.length} blocked_writes=${p.blocked_writes.length} -> ${p.png}`;
}
