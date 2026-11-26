// Entry point of the new shell (served at /ui/). Importing a view module
// registers it; the order here does not matter (the navigation is NAV in
// core/shell.js). A view this folder does not have is the shared module of
// the same path in ui/, which then draws with this folder's core/ui.js and
// the rest of the shared core (internal/api/uifiles.go).

import { start as startRouter, onRoute } from './core/router.js';
import { releaseCatalog } from './core/catalog.js';
import { start as startLive } from './core/live.js';
import { startShell } from './core/shell.js';

import './views/overview.js';
import './views/requests.js';
import './views/report.js';
import './views/zones.js';
import './views/models.js';
import './views/nodes.js';
import './views/queue.js';
import './views/activity.js';
import './views/placement.js';
import './views/benchmarks.js';
import './views/profiles.js';
import './views/testprofiles.js';
import './views/tiers.js';
import './views/settings.js';
import './views/audit.js';
import './views/mcp.js';
import './views/about.js';

// The home page is the Overview (Q76): /ui/ with no screen named opens it.
if (!location.hash || location.hash === '#' || location.hash === '#/') history.replaceState(null, '', '#/overview');

// Another screen opens: the live catalog (and the plans the server computes
// for it) is let go unless the new screen asks for it again (needCatalog()
// in its mount, which runs right after this).
let screen = null;
onRoute(route => { if (route.id !== screen) { screen = route.id; releaseCatalog(); } });

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
