// Benchmarks · the list (board Bench, #/benchmarks/all) and a group's panel
// (board Bench-Group, #/benchmarks/all/<group>).
//
// The list: search, New group, New benchmark; "Running now · N" with the run
// in progress (its name, "10 of 96", the model and where, the step, a bar,
// the time, Cancel); "Groups" (icon, name, the line under it, the last run:
// icon and time or "Running now", the result dots, the duration); and
// "Benchmarks" (name, the chain of steps, the last use or "Never run"). With a
// group open the list narrows beside the panel and keeps only New benchmark.
//
// The group's panel: label "Group", the title, Run group, Edit, Duplicate;
// "Contains · N runs" (the expand count) with each entry (the benchmark's
// title × the matrix) and its options; "Runs · N" (B1 total) with Compare
// two and up to five runs (icon, date, dots, duration, the cold note, the pin
// toggle) and "All N runs ›".
//
// Run, Run group and Cancel open the run and cancel dialogs (run.js, board
// bench-run/run-cancel); an entry the setting bench_not_startable keeps from
// starting shows "Not startable here: <reason>" in place of Run.
//
// Not available yet (drawn, greyed, the reason on hover; p15 plan): New
// group, New benchmark and Edit (no editor mockup), Run on a recipe (its
// inputs dialog has no mockup), "Smallest that fits" (B6), "cold loads"
// (B7), Compare two (B10), the drop-out note (B11).

import { h } from '../../core/dom.js';
import { href, navigate } from '../../core/router.js';
import { state } from '../../core/store.js';
import * as api from '../../core/api.js';
import { empty, skeleton, searchField, sidePanel, dialog, toast, bar, runIcon } from '../../core/ui.js';
import { num, dur, plural, DASH } from '../../core/format.js';
import { bench, groupById, benchmarkById, groupTitle, benchTitle, activeRun, groupSub, chain, entryOf, matrixWords, optionWords,
  when, tookS, resultIcon, dots, coldNote, notYet, WHY, placementWords, modelName } from './common.js';
import { runButton, cancelButton, notStartable, isRecipe, queuedCount } from './run.js';

const GRID = 'M3 3h6v6H3zM11 3h6v6h-6zM3 11h6v6H3zM11 11h6v6h-6z';
const LINES = 'M4 5h12M4 10h12M4 15h8';
export const kindIcon = group => h('svg.bp-kind', { viewBox: '0 0 20 20', 'aria-hidden': 'true' }, h('path', { d: group ? GRID : LINES }));

// ---------- the run in progress ----------

// runningLine(run): "10 of 96 · Devstral 24B on all 6 zones · Requests, step 3 of 7"
export function runningLine(run) {
  const cur = run.current || null;
  const parts = [];
  if (run.steps_total) parts.push(`${num(cur ? (cur.index || 0) + 1 : run.steps_done || 0)} of ${num(run.steps_total)}`);
  if (cur) {
    const where = cur.placement ? placementWords(cur.placement) : cur.nodes && cur.nodes.length ? plural(cur.nodes.length, 'zone') : '';
    parts.push([modelName(cur.model_file) || cur.name, where ? ' on ' + where : ''].join(''));
    if (cur.phase && cur.phase.phases_total) parts.push(`${cur.phase.label || cur.phase.type || 'Step'}, step ${num((cur.phase.index || 0) + 1)} of ${num(cur.phase.phases_total || 0)}`);
  } else if (run.state === 'paused') parts.push(run.paused_because ? 'waiting until idle: ' + run.paused_because : 'waiting until the zones are idle');
  return parts.join(' · ');
}
export function timeLeft(run) {
  const el = run.elapsed_s != null ? run.elapsed_s : run.started_at ? (Date.now() - run.started_at) / 1000 : null;
  const out = [];
  if (el != null) out.push(dur(el));
  if (run.eta_s != null) out.push('about ' + dur(run.eta_s) + ' left');
  return out.join(' · ');
}
export function runningRow(run, opts = {}) {
  const pct = Math.max(0, Math.min(100, run.percent || 0));
  return h('div', { key: 'running', role: 'listitem', 'data-running': run.id || '', class: 'bp-running' + (opts.inline ? ' inline' : '') },
    h('span.bp-ri', runIcon('running', 16, true)),
    h('div.bp-main',
      opts.inline ? h('span.bp-word', run.state === 'paused' ? 'Waiting until idle' : 'Running') : null,
      h('a.bp-name', { href: run.suite_id ? href('benchmarks', ['all', run.suite_id]) : href('benchmarks', ['runs']) }, run.suite_name || run.suite_id),
      h('div.bp-sub', { title: runningLine(run) }, opts.inline ? ' · ' : null, runningLine(run))),
    // waiting until idle: nothing is done yet, the bar would say nothing
    h('div.bp-bar', run.state === 'paused' ? null : bar([{ cls: 'a', value: pct }], 100, `${Math.round(pct)} percent of the run is done`, 'sm'), h('span.mut.bp-left', timeLeft(run))),
    h('span.bp-runacts', queuedCount() ? h('span.b.b-q', { 'data-queued': String(queuedCount()), title: 'Runs waiting in the queue behind this one' }, `${num(queuedCount())} more queued`) : null,
      opts.noCancel ? null : cancelButton(run, 'Cancel', 'btn sm dng')),
    opts.after || null);
}

// ---------- rows ----------

function groupRow(g, run, selected) {
  const last = g.last_run || null;
  const isRunning = run && run.suite_id === g.id;
  return h('a.bp-row.bp-grow', { key: 'g-' + g.id, href: href('benchmarks', ['all', g.id]), role: 'listitem', class: selected ? 'sel' : null,
    'aria-current': selected ? 'true' : null, 'data-group': g.id },
    kindIcon(true),
    h('div.bp-main', h('span.bp-name', groupTitle(g)),
      h('div.bp-sub', groupSub(g), g.status ? [h('span.bp-sep', ' · '), h('span.bp-warn', { title: g.reason || '' }, g.status === 'planned' ? 'planned: ' + (g.reason || 'not ready') : 'blocked: ' + (g.reason || 'cannot run'))] : null,
        g.not_startable ? [h('span.bp-sep', ' · '), notStartable(g.not_startable)] : null)),
    h('div.bp-last', isRunning ? h('span.bp-now', runIcon('running', 14), 'Running now')
      : last ? [resultIcon(last, 14), ' ', when(last.started_at || last.queued_at)] : h('span.mut', 'Never run')),
    h('div.bp-res', isRunning ? dots(run.counts || null, true) : last ? dots(last.counts, false) : null),
    h('div.bp-took.mut', isRunning ? dur(tookS(Object.assign({ state: 'running' }, run))) : last && tookS(last) != null ? dur(tookS(last)) : ''),
    g.not_startable ? null : runButton('group', g.id, 'Run', 'btn sm bp-hover'));
}

function benchRow(b) {
  const u = b.last_use || null;
  return h('div.bp-row', { key: 'b-' + b.id, role: 'listitem', 'data-bench': b.id },
    kindIcon(false),
    h('div.bp-main', h('span.bp-name', benchTitle(b)), h('div.bp-sub', chain(b), b.not_startable ? [h('span.bp-sep', ' · '), notStartable(b.not_startable)] : null)),
    h('div.bp-last', u ? [resultIcon({ result: u.outcome === 'ready' ? 'passed' : u.outcome === 'running' ? 'running' : u.outcome === 'refused' ? 'refused' : u.outcome === 'refused_by_exclusion' ? 'not_judged' : 'failed' }, 14), ' ',
      h('a', { href: href('benchmarks', ['runs', String(u.run_number || u.run_id), String(u.step)]), title: 'Open this use: run ' + (u.run_number || u.run_id) }, when(u.at))]
      : h('span.mut', 'Never run')),
    h('div.bp-res', u ? h('span.bp-dots', h('span', { class: 'bp-dot ' + (u.outcome === 'ready' ? 'ok' : u.outcome === 'running' || u.outcome === 'refused_by_exclusion' ? '' : u.outcome === 'refused' ? 'ref' : 'bad'),
      title: u.outcome === 'ready' ? '1 passed' : u.outcome === 'refused_by_exclusion' ? 'not judged: the profile forces an excluded zone' : u.outcome }, '1')) : null),
    h('div.bp-took.mut', u && u.duration_s != null ? dur(u.duration_s) : ''),
    b.not_startable ? null : isRecipe(b) ? notYet(h('span.btn.sm.bp-hover', 'Run'), WHY.recipe) : runButton('benchmark', b.id, 'Run', 'btn sm bp-hover'));
}

function match(q, ...texts) {
  if (!q) return true;
  const s = q.toLowerCase();
  return texts.some(t => String(t || '').toLowerCase().includes(s));
}

// listPage(route, query, narrow): the list column.
export function listColumn(r, q, setQ, narrow) {
  const run = activeRun();
  const groups = bench.groups, benches = bench.benchmarks;
  if (bench.off) return h('section.card', empty('Benchmarks are off', bench.off));
  if (!groups || !benches) return bench.err ? h('div.banner.err', { role: 'alert' }, bench.err) : skeleton(6);
  const gs = groups.filter(g => match(q, g.id, g.name, g.description));
  const bs = benches.filter(b => match(q, b.id, b.name, b.description));
  const open = r.rest[1] || '';
  return h('div', { class: 'bp-list' + (narrow ? ' narrow' : '') },
    h('section.bp-card', { 'aria-label': 'Groups and benchmarks' },
      h('div.bp-tools.bp-cardtools',
        searchField(q, setQ, 'Search benchmarks and groups', 'bench-search'),
        narrow ? null : notYet(h('span.btn', 'New group'), WHY.edit),
        notYet(h('span.btn.pri', 'New benchmark'), WHY.edit)),
      run ? [h('h2.bp-sec.now', 'Running now · 1'), h('div.bp-rows', { role: 'list', 'aria-label': 'Running now' }, runningRow(run))] : null,
      h('h2.bp-sec', 'Groups'),
      gs.length ? h('div.bp-rows', { role: 'list', 'aria-label': 'Groups' }, gs.map(g => groupRow(g, run, g.id === open)))
        : h('p.mut.bp-none', groups.length ? 'No group matches the search.' : 'No groups yet. A group runs several benchmarks, or one benchmark over several models and placements, in one go.'),
      h('h2.bp-sec', 'Benchmarks'),
      bs.length ? h('div.bp-rows', { role: 'list', 'aria-label': 'Benchmarks' }, bs.map(benchRow))
        : h('p.mut.bp-none', benches.length ? 'No benchmark matches the search.' : 'No benchmarks yet.')));
}

// ---------- a group's panel ----------

function entryRow(g, e, i, expand) {
  let title, sub;
  if (e.kind === 'group') {
    const inner = groupById(e.ref);
    title = h('span', kindIcon(true), ' ', h('a', { href: href('benchmarks', ['all', e.ref]) }, groupTitle(inner) || e.ref));
    sub = inner ? groupSub(inner) : '';
  } else {
    const b = e.kind === 'inline' ? e.inline : benchmarkById(e.ref);
    const mw = matrixWords(e);
    title = h('span', kindIcon(false), ' ', h('b', benchTitle(b) || e.ref), mw.length ? [' ', mw.map(w => [h('span.mut', ' × '), w])] : null);
    sub = b ? chain(b) : '';
  }
  const opts = optionWords(g, e);
  const model = e.inputs && e.inputs.model ? modelName(e.inputs.model) : '';
  return h('div.bp-entry', { key: 'e-' + i, 'data-entry': i },
    h('div.bp-etitle', title),
    sub ? h('div.bp-sub', sub) : null,
    h('div.bp-opts', model ? [model, ' · '] : null,
      // the board's "Smallest that fits" placement: the runner has no such form yet (B6)
      e.kind === 'group' ? null : [notYet('Smallest that fits', WHY.b6), ' · '],
      opts.join(' · '),
      // the board's "cold loads" group option: no field yet (B7)
      [' · ', notYet('cold loads', WHY.b7)]));
}

function runRow(rn) {
  const note = coldNote(rn);
  return h('div.bp-run', { key: 'r-' + rn.id, role: 'listitem', 'data-run': rn.number || rn.id },
    resultIcon(rn),
    h('a.bp-when', { href: href('benchmarks', ['runs', String(rn.number || rn.id)]) }, when(rn.started_at || rn.queued_at)),
    dots(rn.counts, rn.result === 'running'),
    h('span.mut.bp-took', tookS(rn) != null ? dur(tookS(rn)) : DASH),
    pinButton(rn),
    note || rn.state === 'cancelled' ? h('div.bp-note.mut', note || '', rn.state === 'cancelled' ? [note ? ' · ' : '', 'cancelled', ' ', notYet('(what stopped it)', WHY.b11)] : null) : null);
}

// pinButton(run): the pin toggle (PUT / DELETE /api/bench/runs/{id}/pin,
// permission models, a comment asked for).
export function pinButton(rn, after) {
  const on = !!rn.pinned;
  const label = on ? 'Pinned: kept by the retention trim. Click to unpin.' : 'Pin: keep this run when old runs are trimmed';
  return h('button', { class: 'bp-pin' + (on ? ' on' : ''), type: 'button', title: label, 'aria-label': label, 'aria-pressed': on ? 'true' : 'false',
    onclick: async e => {
      e.preventDefault(); e.stopPropagation();
      const v = await dialog({ title: on ? 'Unpin this run' : 'Pin this run',
        body: h('p', on ? 'An unpinned run is trimmed like any other once it is older than the retention.' : 'A pinned run is never trimmed by the retention.'),
        comment: { label: 'Why', required: true }, actions: [{ label: 'Cancel', value: undefined }, { label: on ? 'Unpin' : 'Pin', kind: 'pri', value: true }] });
      if (!v || !v.value) return;
      try {
        await api.send(on ? 'DELETE' : 'PUT', `/api/bench/runs/${encodeURIComponent(rn.id)}/pin`, undefined, { comment: v.comment });
        rn.pinned = !on;
        toast(on ? 'Unpinned' : 'Pinned', 'ok');
        if (after) after();
      } catch (err) { toast(err.message, 'err'); }
    } },
  h('svg', { viewBox: '0 0 20 20', 'aria-hidden': 'true' }, h('path', { d: 'M7 3h6l-1 5 3 3H5l3-3zM10 11v6' })));
}

// openGroup(id, ctx): the panel. ctx = {close(), redraw()}
export function groupPanel(id, onClose) {
  const ps = { expand: null, runs: null, total: null, err: '' };
  const load = async () => {
    try {
      const [ex, rs] = await Promise.all([
        api.get(`/api/bench/groups/${encodeURIComponent(id)}/expand`).catch(e => ({ error: e.message })),
        api.get('/api/bench/runs', { suite: id, limit: 5 })]);
      ps.expand = ex; ps.runs = rs.runs || []; ps.total = rs.total != null ? rs.total : null;
    } catch (e) { ps.err = e.message; }
    p.redraw();
  };
  const body = () => {
    const g = groupById(id);
    if (!g) return bench.groups ? h('section.card', empty('There is no such group', 'It may have been removed. Close this panel to see the groups there are.')) : skeleton(4);
    const items = (g.benchmarks || []).map(entryOf);
    const count = ps.expand && ps.expand.count != null ? ps.expand.count : g.expanded_count;
    const total = ps.total != null ? ps.total : ps.runs ? ps.runs.length : null;
    return h('div.bp-panel',
      g.description ? h('p.txt2', g.description) : null,
      g.status ? h('div.banner.warn', { role: 'status' }, `This group is ${g.status}: ${g.reason || 'no reason given'}. It cannot run until that is settled.`) : null,
      h('h3.bp-h', 'Contains', count != null ? [' · ', h('span.mut', plural(count, 'run'))] : null),
      ps.expand && ps.expand.error ? h('div.banner.err', { role: 'alert' }, ps.expand.error) : null,
      h('div.bp-entries', items.map((e, i) => entryRow(g, e, i))),
      h('div.bp-h.row', h('h3.bp-h', 'Runs', total != null ? [' · ', h('span.mut', num(total))] : null), h('span.grow'), notYet(h('span.btn.sm', 'Compare two'), WHY.b10)),
      ps.err ? h('div.banner.err', { role: 'alert' }, ps.err) : null,
      ps.runs == null ? skeleton(3) : ps.runs.length ? h('div.bp-runs', { role: 'list', 'aria-label': 'Runs of this group' }, ps.runs.map(runRow))
        : h('p.mut', 'This group has not run yet.'),
      ps.runs && ps.runs.length ? h('a.bp-all', { href: href('benchmarks', ['runs'], { suite: id }) }, `All ${total != null ? num(total) + ' ' : ''}runs ›`) : null);
  };
  const duplicate = async () => {
    const g = groupById(id);
    if (!g) return;
    let idInput, nameInput;
    const v = await dialog({ title: 'Duplicate this group',
      body: h('div.col', { style: 'gap:12px' }, h('p', 'A copy with the same entries and options, under a new id. Nothing runs.'),
        h('div.field', h('label', 'New id'), idInput = h('input.input', { type: 'text', name: 'id', value: g.id + '-copy', 'aria-label': 'New id' })),
        h('div.field', h('label', 'Name'), nameInput = h('input.input', { type: 'text', name: 'name', value: (g.name || g.id) + ' (copy)', 'aria-label': 'Name' }))),
      comment: { label: 'Why', required: true }, actions: [{ label: 'Cancel', value: undefined }, { label: 'Duplicate', kind: 'pri', value: () => ({ id: idInput.value.trim(), name: nameInput.value.trim() }) }] });
    if (!v || !v.value || !v.value.id) return;
    const copy = { id: v.value.id, name: v.value.name || v.value.id };
    for (const k of ['description', 'benchmarks', 'stop_on_failure', 'cooldown_s', 'only_when_idle', 'idle_wait_s', 'evict_others', 'baseline']) if (g[k] !== undefined) copy[k] = g[k];
    try {
      await api.post('/api/bench/groups', copy, { comment: v.comment });
      toast('Duplicated as ' + copy.id, 'ok');
      navigate('benchmarks', ['all', copy.id]);
    } catch (e) { toast(e.message, 'err'); }
  };
  const p = sidePanel({ closesMenu: true, label: 'Group', kind: 'bench-group', name: 'Group ' + id,
    title: () => groupTitle(groupById(id)) || id,
    controls: () => h('div.bp-acts', groupById(id) && groupById(id).not_startable ? notStartable(groupById(id).not_startable) : runButton('group', id, 'Run group', 'btn pri sm'),
      notYet(h('span.btn.sm', 'Edit'), WHY.edit),
      h('button.btn.sm', { type: 'button', 'data-act': 'duplicate', onclick: duplicate, disabled: groupById(id) ? null : '' }, 'Duplicate')),
    body, onClose });
  p.key = id;
  load();
  return p;
}
