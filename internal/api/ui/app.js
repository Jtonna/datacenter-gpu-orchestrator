// Entry point. Importing a view module registers it; the order here does not
// matter (the navigation order is in core/shell.js).

import { start as startRouter } from './core/router.js';
import { start as startLive } from './core/live.js';
import { startShell } from './core/shell.js';

import './views/models.js';
import './views/nodes.js';
import './views/queue.js';
import './views/activity.js';
import './views/placement.js';
import './views/benchmarks.js';
import './views/profiles.js';
import './views/tiers.js';
import './views/settings.js';
import './views/audit.js';
import './views/about.js';

startShell(document.getElementById('app'));
startRouter(document.getElementById('main'));
startLive();

// A script error is shown, not swallowed: the page is a monitoring tool and a
// silently frozen screen would be worse than a visible fault.
import { toast } from './core/ui.js';
let lastFault = '';
function fault(msg) {
  if (!msg || msg === lastFault) return;
  lastFault = msg;
  toast('The page hit an error: ' + msg, 'err');
}
window.addEventListener('error', e => fault(e.message));
window.addEventListener('unhandledrejection', e => fault(e.reason && e.reason.message ? e.reason.message : String(e.reason)));
