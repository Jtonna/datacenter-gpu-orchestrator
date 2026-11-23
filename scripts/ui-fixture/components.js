// The fixture's components page (scripts/ui-fixture-server.mjs serves it at
// /ui/components.html, ui-exec loads it with "modules"): the shell's shared
// components shown with the fixture's answers, for the mockup comparison and
// the tests. It is not part of the product: the scheduler never serves it.
//
// #/components                    the page
// #/components/run/<id>           the side panel of one run (GET /api/requests/{id}).
//                                 No mockup draws a run's panel; it shows the
//                                 component with real data, nothing more.

import { registerView, navigate, href } from '../core/router.js';
import { h } from '../core/dom.js';
import * as api from '../core/api.js';
import { pageHead, panel, button, facts, runStatus, runLength, runMachines } from '../core/ui.js';
import { openPanel } from '../core/panel.js';
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

    const draw = () => {
      el.replaceChildren(
        pageHead('Components', 'The shell\'s shared components with the fixture\'s answers'),
        panel('Side panel', null,
          h('p.mut', 'Beside the page at 1400 px and wider, over it below that. Esc closes it; the focus returns here.'),
          h('div', { style: 'display:flex;gap:8px' },
            RUNS.map(id => button('Run #' + id, () => navigate('components', ['run', String(id)]), '', { 'data-run': String(id) })))));
      syncSide();
    };
    draw();
    return {
      update(next) { r = next; syncSide(); },
      unmount() { alive = false; if (side) { const s = side; side = null; s.close(); } },
    };
  },
});
