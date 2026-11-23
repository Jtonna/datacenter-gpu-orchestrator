// The fixture's components page (scripts/ui-fixture-server.mjs serves it at
// /ui/components.html, ui-exec loads it with "modules"): the shell's shared
// components shown with the fixture's answers, for the mockup comparison and
// the tests. It is not part of the product: the scheduler never serves it.
//
// #/components                    the page
// #/components?page=<n>           the pagers on page n (1 to 15)
// #/components/run/<id>           the side panel of one run (GET /api/requests/{id}).
//                                 No mockup draws a run's panel; it shows the
//                                 component with real data, nothing more.
//
// The pager, the popover, the dialog and the search field carry no data of
// their own: their labels here are only what the component draws.

import { registerView, navigate, href } from '../core/router.js';
import { h } from '../core/dom.js';
import * as api from '../core/api.js';
import { pageHead, panel, button, facts, runStatus, runLength, runMachines, dialog, searchField } from '../core/ui.js';
import { openPanel } from '../core/panel.js';
import { pager } from '../core/pager.js';
import { popover } from '../core/popover.js';
import { num, clock } from '../core/format.js';

const RUNS = [101, 102, 203];

registerView({
  id: 'components', title: 'Components',
  mount(el, route) {
    let r = route, side = null, alive = true;

    // the run panel follows the address: #/components/run/<id>
    const runId = () => (r.rest[0] === 'run' && r.rest[1]) || '';
    const syncSide = () => {
      const id = runId();
      if (!id) { if (side) { const s = side; side = null; s.close(); } return; }
      if (side && side.id === id) return;
      if (side) { const s = side; side = null; s.close(); }
      let run = null, error = '';
      const p = openPanel({
        label: 'Run',
        title: () => h('span.mono', '#' + id),
        sub: () => (run ? [run.profile || '', run.queued_at ? ' · queued ' + clock(run.queued_at, false) : ''] : error || 'Reading…'),
        body: () => (run ? facts([
          ['State', runStatus(run.status, run.status === 'queued' ? 'Not started' : runLength(run.duration_ms))],
          ['Model', h('span.mono', run.profile || '')],
          ['Waited', runLength(run.wait_ms)],
          ['Tokens in', run.tokens_in == null ? '–' : num(run.tokens_in)],
          ['Tokens out', run.tokens_out == null ? '–' : num(run.tokens_out)],
          ['Zones', run.nodes && run.nodes.length ? runMachines(run.nodes) : '–'],
        ]) : h('p.mut', error || 'Reading the run…')),
        foot: () => [h('a.btn', { href: href('requests') }, 'All runs'), button('Close', () => p.close(), '', { style: 'margin-left:auto' })],
        onClose: () => { if (side === p) { side = null; if (alive && runId()) navigate('components', []); } },
      });
      p.id = id;
      side = p;
      api.get('/api/requests/' + encodeURIComponent(id)).then(v => { run = v; }, e => { error = (e && e.message) || 'The run could not be read.'; })
        .then(() => { if (side === p) p.redraw(); });
    };

    const page = () => Math.min(15, Math.max(1, Number(r.query.page) || 1));
    const setPage = n => navigate('components', r.rest, Object.assign({}, r.query, { page: n === 1 ? '' : String(n) }), true);
    let said = '';
    const openPop = e => {
      const anchor = e.currentTarget || e.target;
      popover({ anchor, label: 'Anchored popover', content: close => [
        h('b', 'Anchored popover'),
        h('span.mut', 'Beside its anchor, the arrow at its middle. Escape or a click outside closes it.'),
        h('div', { style: 'display:flex;gap:8px' }, button('Close', close, 'sm pri')),
      ] });
    };
    const openDialog = withComment => () => dialog({
      title: 'Dialog title',
      body: h('p', 'The body of a 500 px dialog, with its close button in the title row.'),
      comment: withComment ? { label: 'Comment', required: true, placeholder: 'Why, for the audit' } : null,
      actions: [{ label: 'Cancel', value: false }, { label: 'Confirm', kind: 'dng solid', value: true }],
    }).then(v => { said = JSON.stringify(v === undefined ? null : v); draw(); });

    const draw = () => {
      el.replaceChildren(
        pageHead('Components', 'The shell\'s shared components with the fixture\'s answers'),
        panel('Side panel', null,
          h('p.mut', 'Beside the page at 1400 px and wider, over it below that. Esc closes it; the focus returns here.'),
          h('div', { style: 'display:flex;gap:8px' },
            RUNS.map(id => button('Run #' + id, () => navigate('components', ['run', String(id)]), '', { 'data-run': String(id) })))),
        panel('Numbered pager', null,
          h('div', { style: 'display:flex;flex-direction:column;gap:12px;align-items:flex-end' },
            pager({ page: page(), pages: 15, onPage: setPage, label: 'Zones' }),
            pager({ page: Math.min(page(), 5), pages: 5, total: 49, per: 12, range: true, onPage: setPage, label: 'Runs' }))),
        panel('Anchored popover', null,
          h('div', button('Open the popover', openPop, '', { 'data-pop': 'open', 'aria-expanded': 'false', 'aria-haspopup': 'dialog' }))),
        panel('Dialog', null,
          h('div', { style: 'display:flex;gap:8px;align-items:center' },
            button('With a comment', openDialog(true), '', { 'data-dlg': 'comment' }),
            button('Without', openDialog(false), '', { 'data-dlg': 'plain' }),
            said ? h('span.mut.mono', { 'data-said': '' }, said) : null)),
        panel('Search field', null,
          h('div', { style: 'display:flex;gap:12px;align-items:center' },
            searchField('', () => {}, 'Search models', 'demo-sm'),
            searchField('', () => {}, 'Search name, chip, model or address', 'demo-lg', { size: 'lg' }))));
      syncSide();
    };
    draw();
    return {
      update(next) { const p0 = r.query.page; r = next; if (next.query.page !== p0) draw(); else syncSide(); },
      unmount() { alive = false; if (side) { const s = side; side = null; s.close(); } },
    };
  },
});
