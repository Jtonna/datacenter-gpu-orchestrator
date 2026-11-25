// Benchmarks · Runs (board Bench-Runs, #/benchmarks/runs), a run's panel
// (board Bench-Runs-Group, #/benchmarks/runs/<number>) and a benchmark's
// report (board Bench-Report, #/benchmarks/runs/<number>/<slug>).
//
// The runs table: the views RUNS, Load times, Generation speed, First token,
// Memory, Coding (only RUNS is served: the others are the result views of
// B5, drawn greyed); the filters Kind, Result, Model (GET /api/bench/runs
// kind=, result=, model=); the rows (started, the run: a group's name, or a
// direct run's benchmark with its model, zones and context on the line under
// the time; the result dots; how long it took); "1–12 of 49" and the pages
// (offset, limit, total).
//
// A run's panel ("Group run"): the started time, the release, the cold note;
// the counts (ran, passed, failed, refused) and the total time; Compare
// (B10), the pin toggle, Download PDF (B13: the full report's print page
// instead), Run again (slice 6); "Issues · N": each failed or refused step
// with its reason, ONCE; "Reports · N": one row per step, opening its report.
//
// A benchmark report: the model, the benchmark · the zones · the context;
// four headline figures (B9: not served yet); the outcome; "Steps · N" with
// "took"; each step with its key figure (the step's own measurements), the
// Transcript link, and the requests' table (Measured, Mean, Median, Min, Max)
// inside its card; Open full report (the server's print page), Download PDF
// (B13), Run again (slice 6). A failed or refused step prints its reason ONCE.

import { h } from '../../core/dom.js';
import { href, navigate } from '../../core/router.js';
import * as api from '../../core/api.js';
import { empty, skeleton, sidePanel, filterSelect } from '../../core/ui.js';
import { num, dur, plural, DASH, mib } from '../../core/format.js';
import { catalog } from '../../core/catalog.js';
import { state } from '../../core/store.js';
import { logsLink, padded } from '../../core/logs.js';
import { when, tookS, resultIcon, resultWord, dots, coldNote, inputsLine, notYet, WHY, modelName, placementWords, zonesWord, phaseWord } from './common.js';
import { pinButton } from './list.js';

export const PAGE = 12;

const VIEWS = [['runs', 'Runs'], ['load', 'Load times'], ['gen', 'Generation speed'], ['ttft', 'First token'], ['mem', 'Memory'], ['coding', 'Coding']];
const KINDS = [{ id: '', label: 'any' }, { id: 'suite', label: 'group runs' }, { id: 'benchmark', label: 'direct runs' }];
const RESULTS = [{ id: '', label: 'any' }, { id: 'clean', label: 'passed' }, { id: 'with_issues', label: 'passed with issues' }, { id: 'has_failed', label: 'something failed' },
  { id: 'has_refused', label: 'something refused' }, { id: 'cancelled', label: 'cancelled' }, { id: 'running', label: 'running' }];

// runsState(): the page's own copy, read on entry and when the filters move.
export function runsData() {
  return { key: '', runs: null, total: null, err: '' };
}
export async function loadRuns(d, q) {
  const page = Math.max(1, parseInt(q.page || '1', 10) || 1);
  const key = JSON.stringify([q.kind || '', q.result || '', q.model || '', q.suite || '', page]);
  if (d.key === key && d.runs) return false;
  d.key = key;
  try {
    const r = await api.get('/api/bench/runs', { limit: PAGE, offset: (page - 1) * PAGE, kind: q.kind || undefined, result: q.result || undefined,
      model: q.model || undefined, suite: q.suite || undefined });
    if (d.key !== key) return false;
    d.runs = r.runs || []; d.total = r.total != null ? r.total : null; d.err = '';
  } catch (e) { d.err = e.message; d.runs = d.runs || []; }
  return true;
}

// directLine(run): "Cold load, response and repeat · Qwen 2.5 72B Q2 · all 6 zones · 131,072"
export function directLine(rn) {
  const inp = inputsLine(rn);
  return [rn.suite_name || rn.benchmark_id || rn.id, inp].filter(Boolean).join(' · ');
}

function runRow(rn, q) {
  const direct = rn.kind === 'benchmark';
  const open = href('benchmarks', ['runs', String(rn.number || rn.id)], q);
  return h('tr', { key: rn.id, 'data-run': rn.number || rn.id, class: direct ? 'direct' : null },
    h('td.bp-start', direct ? h('div', when(rn.started_at || rn.queued_at), h('div.bp-sub', { title: directLine(rn) }, directLine(rn))) : when(rn.started_at || rn.queued_at)),
    h('td.bp-name', direct ? h('a', { href: open, 'aria-label': 'Open run ' + (rn.number || rn.id) }, h('span.sr', directLine(rn)))
      : h('a', { href: open, title: rn.suite_name || rn.suite_id }, rn.suite_name || rn.suite_id, rn.state === 'cancelled' ? h('span.mut', ' · cancelled') : null)),
    h('td.bp-res', h('span.bp-resw', resultIcon(rn, 14), dots(rn.counts, rn.result === 'running'))),
    h('td.bp-took.mut', tookS(rn) != null ? dur(tookS(rn)) : DASH),
    h('td.bp-pincell', pinButton(rn)));
}

function pager(q, total) {
  if (total == null) return null;
  const page = Math.max(1, parseInt(q.page || '1', 10) || 1);
  const pages = Math.max(1, Math.ceil(total / PAGE));
  const from = total ? (page - 1) * PAGE + 1 : 0, to = Math.min(total, page * PAGE);
  const go = p => href('benchmarks', ['runs'], Object.assign({}, q, { page: p > 1 ? String(p) : '' }));
  const nums = [];
  const lo = Math.max(1, Math.min(page - 2, pages - 4)), hi = Math.min(pages, lo + 4);
  for (let i = lo; i <= hi; i++) nums.push(h('a', { class: 'bp-pg' + (i === page ? ' on' : ''), href: go(i), 'aria-current': i === page ? 'page' : null }, String(i)));
  return h('div.bp-pager', h('span.mut', `${num(from)}–${num(to)} of ${num(total)}`), h('span.grow'), pages > 1 ? nums : null);
}

// runsTable(d, r): the page (route query kept: kind, result, model, suite, page)
export function runsColumn(d, r) {
  const q = r.query;
  const set = patch => navigate('benchmarks', r.rest.length > 1 ? ['runs'] : r.rest, Object.assign({}, q, patch, { page: '' }), true);
  const models = [{ id: '', label: 'any' }].concat((catalog.models || []).filter(m => m.file).map(m => ({ id: m.file, label: m.name || m.file })));
  if (q.model && !models.some(m => m.id === q.model)) models.push({ id: q.model, label: modelName(q.model) });
  return h('div.bp-runsp',
    h('div.bp-tools', { role: 'group', 'aria-label': 'Views and filters' },
      h('div.bp-views', { role: 'tablist', 'aria-label': 'Views' }, VIEWS.map(([id, label]) => id === 'runs'
        ? h('span.bp-view.on', { role: 'tab', 'aria-selected': 'true' }, label.toUpperCase())
        : notYet(h('span.bp-view', { role: 'tab', 'aria-selected': 'false' }, label), WHY.b5))),
      h('span.grow'),
      filterSelect('Kind', KINDS, q.kind || '', v => set({ kind: v })),
      filterSelect('Result', RESULTS, q.result || '', v => set({ result: v })),
      filterSelect('Model', models, q.model || '', v => set({ model: v }))),
    q.suite ? h('div.bp-chip', 'Group: ', h('b', q.suite), ' ', h('a', { href: href('benchmarks', ['runs'], Object.assign({}, q, { suite: '', page: '' })) }, 'Show all groups')) : null,
    d.err ? h('div.banner.err', { role: 'alert' }, d.err) : null,
    d.runs == null ? skeleton(6) : !d.runs.length ? h('section.card', empty(q.kind || q.result || q.model || q.suite ? 'No run matches these filters' : 'No runs yet', 'A run appears here when it is queued.'))
      : h('div.bp-tablewrap', h('table.bp-runs', { 'aria-label': 'Runs' },
        h('thead', h('tr', h('th', 'Started'), h('th', 'Run'), h('th', 'Result'), h('th', 'Took'), h('th', h('span.sr', 'Pinned')))),
        h('tbody', d.runs.map(rn => runRow(rn, q))))),
    pager(q, d.total));
}

// ---------- a run's panel ----------

// runLogs(run) -> a link to every log line written while the run ran (all
// sources and nodes: a benchmark touches the whole fleet), a minute either
// side; null before it started or while central logging is off. Not on the
// board: kept from the classic run page, beside the panel's own controls.
function runLogs(run) {
  if (!run.started_at) return null;
  const [from, to] = padded(run.started_at, run.finished_at || null);
  return logsLink(state.status, 'Logs', '*', from, to, 'Open the log lines written while this run ran, in a new tab');
}

const stepWhere = st => (st.node_count ? zonesWord(st.node_count) : '');
// reasonOnce(step): the one sentence of what went wrong: the scheduler's
// reason, else the step's error, else the load's; a sentence said by two of
// them is printed once (the old detail page printed it three times).
export function reasonOnce(st) {
  const seen = [];
  for (const t of [st.reason, st.error, st.load && st.load.reason]) {
    const s = String(t || '').trim();
    if (s && !seen.some(x => x.includes(s) || s.includes(x))) seen.push(s);
  }
  return seen.join(' · ');
}
const STEP_WORD = { ready: 'Passed', completed: 'Passed', failed: 'Failed', error: 'Failed', refused: 'Refused', refused_by_exclusion: 'Not judged', cancelled: 'Cancelled', skipped: 'Skipped' };
const stepResult = st => (st.outcome === 'ready' ? 'passed' : st.outcome === 'refused_by_exclusion' ? 'not_judged' : st.outcome === 'refused' ? 'refused' : st.outcome === 'cancelled' ? 'cancelled'
  : st.outcome === 'skipped' ? 'skipped' : st.outcome ? 'failed' : st.state === 'running' ? 'running' : 'queued');

function kpi(n, word, cls) {
  return h('div', { class: 'bp-kpi' + (cls ? ' ' + cls : '') }, h('b', n), h('span', word));
}

export function stepSlug(st) { return st.slug || String(st.idx); }

export function runPanel(ref, q, onClose) {
  const ps = { run: null, err: '', showAll: false };
  const load = async () => {
    try { ps.run = await api.get(`/api/bench/runs/${encodeURIComponent(ref)}`, { requests: 0 }); ps.err = ''; } catch (e) { ps.err = e.status === 404 ? 'not found' : e.message; }
    p.redraw();
  };
  const body = () => {
    const rn = ps.run;
    if (ps.err) return h('section.card', empty(ps.err === 'not found' ? 'There is no such run' : 'This run could not be read', ps.err === 'not found' ? 'It may have been trimmed by the retention.' : ps.err));
    if (!rn) return skeleton(5);
    const c = rn.counts || {};
    const steps = rn.steps || [];
    const issues = steps.filter(st => ['failed', 'refused', 'not_judged'].includes(stepResult(st)));
    const shown = ps.showAll ? steps : steps.slice(0, 8);
    return h('div.bp-panel',
      h('div.bp-kpis', kpi(num(c.done != null ? c.done : steps.length), 'ran'), kpi(num((c.passed || 0) + (c.with_issues || 0)), 'passed', 'ok'),
        kpi(num((c.failed || 0) + (c.cancelled || 0)), 'failed', (c.failed || c.cancelled) ? 'bad' : ''), kpi(num(c.refused || 0), 'refused', c.refused ? 'ref' : ''),
        kpi(tookS(rn) != null ? dur(tookS(rn)) : DASH, 'total')),
      rn.error ? h('div.banner.err', { role: 'status' }, rn.error) : null,
      h('h3.bp-h', 'Issues', [' · ', h('span.mut', num(issues.length))]),
      issues.length ? h('div.bp-issues', issues.map(st => h('div.bp-issue', { key: 'i-' + st.idx, 'data-step': st.idx },
        h('span', { class: 'bp-tag ' + (stepResult(st) === 'not_judged' ? 'refused' : stepResult(st)) }, STEP_WORD[st.outcome] || st.outcome),
        h('div.bp-main', h('a', { href: href('benchmarks', ['runs', String(rn.number || rn.id), stepSlug(st)]) }, [modelName(st.model_file) || st.profile, stepWhere(st)].filter(Boolean).join(' · ')),
          h('div.bp-reason', reasonOnce(st) || h('span.mut', 'no reason was recorded'))))))
        : h('p.mut', 'No step failed or was refused.'),
      h('h3.bp-h', 'Reports', [' · ', h('span.mut', num(steps.length))]),
      steps.length ? h('div.bp-reports', { role: 'list' }, shown.map(st => h('a.bp-rep', { key: 'r-' + st.idx, role: 'listitem', href: href('benchmarks', ['runs', String(rn.number || rn.id), stepSlug(st)]), 'data-step': st.idx },
        resultIcon({ result: stepResult(st) }, 14),
        h('span.bp-name', modelName(st.model_file) || st.profile), h('span.mut', stepWhere(st)),
        h('span.bp-fig', ['refused', 'not_judged'].includes(stepResult(st)) ? h('span.mut', reasonOnce(st)) : st.load && st.load.load_s ? `${secs(st.load.load_s)} load` + (st.stats && st.stats.decode_tps ? ` · ${num(st.stats.decode_tps.mean, 1)} tok/s` : '') : st.state === 'running' ? 'running' : ''))))
        : h('p.mut', 'This run has no steps yet.'),
      steps.length > shown.length ? h('button.bp-all', { type: 'button', onclick: () => { ps.showAll = true; p.redraw(); } }, `All ${num(steps.length)} reports`) : null);
  };
  const sub = () => {
    const rn = ps.run;
    if (!rn) return null;
    const parts = [when(rn.started_at || rn.queued_at)];
    if (rn.version) parts.push(' · release ', rn.version);
    const cold = coldNote(rn);
    if (cold) parts.push(' · ', cold);
    if (rn.kind === 'benchmark' && inputsLine(rn)) parts.push(' · ', inputsLine(rn));
    return parts;
  };
  const p = sidePanel({ closesMenu: true, kind: 'bench-run', label: 'Group run', name: 'Run ' + ref,
    title: () => (ps.run ? ps.run.suite_name || ps.run.suite_id || ps.run.id : 'Run ' + ref), sub,
    controls: () => h('div.bp-acts', notYet(h('span.btn.sm', 'Compare'), WHY.b10), ps.run ? pinButton(ps.run, () => p.redraw()) : null,
      notYet(h('span.btn.sm', 'Download PDF'), WHY.b13), notYet(h('span.btn.sm.pri', 'Run again'), WHY.run), ps.run ? runLogs(ps.run) : null),
    body, onClose });
  p.key = 'run:' + ref;
  load();
  return p;
}

// ---------- a benchmark's report ----------

const s1 = v => num(v, v < 10 ? 2 : 1);
// secs(26.6) -> "26.6 s" (the boards' load and unload times: one decimal up to two minutes, "72.2 s")
export const secs = v => (v == null ? DASH : v < 120 ? num(v, 1) + ' s' : dur(v));
function statsTable(stats) {
  if (!stats) return null;
  const rows = [['decode_tps', 'Generation, tok/s', 1], ['prefill_tps', 'Prompt, tok/s', 1], ['ttft_ms', 'First token, s', 1000], ['wall_ms', 'Total time, s', 1000]]
    .filter(([k]) => stats[k]).map(([k, label, div]) => {
      const sm = stats[k], f = v => (v == null ? DASH : s1(v / div));
      return h('tr', h('th', label), h('td', f(sm.mean)), h('td', f(sm.median)), h('td', f(sm.min)), h('td', f(sm.max)));
    });
  if (!rows.length) return null;
  return h('div.bp-tablewrap', h('table.bp-stats', { 'aria-label': 'Measured' },
    h('thead', h('tr', h('th', 'Measured'), h('th', 'Mean'), h('th', 'Median'), h('th', 'Min'), h('th', 'Max'))), h('tbody', rows)));
}

// phaseLine(phase, all): the key figure of one step of the benchmark.
export function phaseLine(ph, all) {
  const out = [];
  if (ph.load) {
    out.push(secs(ph.load.load_s));
    if (ph.load.memory_mib) out.push(mib(ph.load.memory_mib));
    if (ph.cold && ph.cold.requested) out.push(ph.cold.verified ? 'cold verified' : 'cold not verified' + (ph.cold.reason ? ': ' + ph.cold.reason : ''));
    if (ph.load.load_mode === 'cached' || ph.load.source === 'cache') out.push('file fully cached');
  } else if (ph.unload) out.push(secs(ph.unload.unload_s));
  else if (ph.tasks) {
    // a tasks phase: the set, the counts, each scorer's total (the per-task
    // table is in the server's full report)
    const t = ph.tasks;
    out.push(`${t.set_title || t.set_id} v${num(t.set_version)}`, `${num(t.answered)} of ${num(t.total)} answered`);
    if (t.timed_out) out.push(`${num(t.timed_out)} timed out`);
    if (t.not_sent) out.push(`${num(t.not_sent)} not sent`);
    if (t.phase_timed_out) out.push("the phase's time limit ran out");
    for (const sc of t.scores || []) out.push(`${sc.scorer} ${num(sc.points, 1)} / ${num(sc.max_points)}`);
  }
  else if (ph.stats) {
    const st = ph.stats;
    if (ph.type === 'warmup' && st.ttft_ms) out.push(`${s1(st.ttft_ms.mean / 1000)} s to first token`);
    else if (st.decode_tps) out.push(`${s1(st.decode_tps.mean)} tok/s`);
    const hs = st.output_hashes ? new Set(Object.values(st.output_hashes)) : null;
    if (hs && hs.size === 1 && Object.keys(st.output_hashes).length > 1) out.push('output identical');
    else if (hs && hs.size > 1) out.push(`${num(hs.size)} different outputs`);
    if (st.failed) out.push(`${num(st.failed)} of ${num(st.requests)} failed`);
  } else if (ph.held_s != null) out.push(dur(ph.held_s));
  else if (ph.duration_s) out.push(dur(ph.duration_s));
  return out.join(' · ');
}

function stepTitle(ph, i, phases, spec) {
  let w = phaseWord(ph, i, phases);
  const sp = spec && spec.phases && spec.phases[i];
  if (ph.type === 'requests' && sp) {
    const n = (sp.requests || []).length || (spec.requests || []).length, rep = sp.repeats || spec.repeats || 0;
    if (n && rep) w += `, ${num(n)} × ${num(rep)}`;
  }
  return w;
}

export function reportPanel(ref, slug, onClose) {
  const ps = { run: null, rep: null, err: '' };
  const findStep = () => {
    const steps = (ps.run && ps.run.steps) || [];
    return steps.find(s => s.slug === slug) || steps.find(s => String(s.idx) === slug) || null;
  };
  const load = async () => {
    try {
      ps.run = await api.get(`/api/bench/runs/${encodeURIComponent(ref)}`, { requests: 0 });
      const st = findStep();
      if (st) ps.rep = await api.get(`/api/bench/runs/${encodeURIComponent(ps.run.id)}/report`, { step: st.idx }).catch(() => null);
      ps.err = st ? '' : 'not found';
    } catch (e) { ps.err = e.status === 404 ? 'not found' : e.message; }
    p.redraw();
  };
  const body = () => {
    if (ps.err) return h('section.card', empty(ps.err === 'not found' ? 'There is no such report' : 'This report could not be read', ps.err === 'not found' ? 'The run has no step ' + slug + '.' : ps.err));
    const st = findStep();
    if (!st) return skeleton(5);
    const rn = ps.run, phases = st.phases || [], spec = st.spec || null;
    const res = stepResult(st);
    const reason = ['failed', 'refused', 'not_judged', 'cancelled'].includes(res) ? reasonOnce(st) : '';
    const repSections = (ps.rep && ps.rep.benchmark && ps.rep.benchmark.sections) || [];
    const full = api.url(`/api/bench/runs/${encodeURIComponent(rn.id)}/report`, { step: st.idx, format: 'html' });
    return h('div.bp-panel.bp-report',
      h('div.bp-heads', ['cold load', 'reload', 'tok/s steady', 'first token'].map(w => h('div.bp-head', notYet(h('b', DASH), WHY.b9), h('span', w)))),
      h('div.bp-outcome', resultIcon({ result: res }, 16), ' ', h('b', resultWord({ result: res }))),
      reason ? h('div', { class: 'banner bp-why ' + (res === 'failed' ? 'err' : 'warn'), role: 'status', 'data-reason': '' }, reason) : null,
      h('div.bp-h.row', h('h3.bp-h', 'Steps', [' · ', h('span.mut', num(phases.length))]), h('span.grow'),
        h('span.mut', 'took ', st.started_at && st.finished_at ? dur((st.finished_at - st.started_at) / 1000) : DASH)),
      phases.length ? h('ol.bp-steps', phases.map((ph, i) => {
        const sec = repSections.find(s => s.idx === ph.idx) || null;
        const failedHere = ph.outcome && !['ok', 'ready'].includes(ph.outcome);
        return h('li.bp-step', { key: 'p-' + ph.idx, 'data-phase': ph.idx, class: failedHere ? 'bad' : null },
          h('div.bp-steprow', h('span.bp-n', num(i + 1)), h('b', stepTitle(ph, i, phases, spec)), h('span.bp-fig', phaseLine(ph, phases)), h('span.grow'),
            sec && sec.transcript ? h('a', { href: sec.transcript, target: '_blank', rel: 'noopener' }, 'Transcript') : null),
          // the step's own error only when it is not the sentence printed above
          failedHere && ph.error && !reason.includes(ph.error.trim()) ? h('div.bp-reason', ph.error) : null,
          ph.type === 'requests' || ph.type === 'conversation' || ph.type === 'concurrent_requests' ? statsTable(ph.stats) : null);
      })) : h('p.mut', reason ? 'No step ran.' : 'The steps appear here as they run.'),
      h('div.bp-foot', h('a.btn', { href: full, target: '_blank', rel: 'noopener' }, 'Open full report'), notYet(h('span.btn', 'Download PDF'), WHY.b13), h('span.grow'),
        notYet(h('span.btn.pri', 'Run again'), WHY.run)));
  };
  const sub = () => {
    const st = findStep();
    if (!st) return null;
    const inp = (st.spec && st.spec.applied_inputs) || {};
    const parts = [st.name || st.benchmark_id];
    if (inp.placement) parts.push(placementWords(inp.placement));
    else if (st.node_count) parts.push(zonesWord(st.node_count));
    if (inp.ctx) parts.push('context ' + num(inp.ctx));
    return parts.join(' · ');
  };
  const p = sidePanel({ closesMenu: true, kind: 'bench-report', label: 'Benchmark report', name: 'Benchmark report ' + ref + ' ' + slug,
    title: () => { const st = findStep(); return st ? modelName(st.model_file) || st.profile : 'Report'; }, sub, body, onClose,
    controls: () => (ps.run ? h('a.btn.sm', { href: href('benchmarks', ['runs', String(ps.run.number || ps.run.id)]) }, '‹ The whole run') : null) });
  p.key = 'rep:' + ref + '/' + slug;
  load();
  return p;
}

