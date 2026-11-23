# Potluck web UI

Static files embedded in the scheduler binary and served below `/ui/`
(`internal/api/uifiles.go`). Plain JavaScript modules, one stylesheet, two
bundled fonts. No build step, no framework, nothing fetched from another host.

```
index.html          the shell markup (top bar, navigation pane, <main>)
app.css             the design system: tokens, shell, every shared component
app.js              entry: imports the views, starts shell, router and live channel
core/dom.js         h() element builder, morph() in-place update, icon()
core/format.js      numbers, units, durations, "38 min ago" with the time on hover
core/store.js       the live store: state, on(), listen()
core/live.js        the WebSocket client (reconnects by itself)
core/api.js         REST helper: get(), send(), post/put/patch/del (X-Source, X-Actor)
core/router.js      hash router and the view registry
core/shell.js       navigation, the connection warning, the name that signs changes
core/status.js      helpers that read the status document
core/reasons.js     the scheduler's reason strings in plain words (no DOM)
core/nodeactions.js agent machines: why one takes no new model, and the two operator actions (no DOM)
core/startup.js     start-up figures of agent machines: the words, and GET /api/startups on demand (no DOM)
core/reserve.js     the memory reservation rule: a node's reserved list and a plan's memory table
core/ui.js          the shared components (below)
core/charts.js      every chart and the chart tooltip: inline SVG and plain boxes ("Charts" below)
core/catalog.js     the model catalog, kept for the life of the page (fed by the live channel)
core/avatar.js      the picture of a model: the owner's icon, else a drawn tile
core/rate.js        the pass rate as dots (passDots)
views/<id>.js       one module per screen; it registers itself
views/models/       the instance card and the model page of the Models screen
views/benchmarks/   the parts of Benchmarks; pages.js is a suite's and a benchmark's own page
fonts/              Manrope (variable) and JetBrains Mono, with their licences (OFL)
```

## Two UIs: the new shell at /ui/, the classic UI at /ui/classic/

The UI rewrite (plan phase U1) adds a new shell without moving the current UI:

- `/ui/classic/<p>` serves the classic file `<p>` above, unchanged. It stays
  until the owner says it can go; every classic test reads these files.
- `/ui/<p>` serves `next/<p>` when that file exists, else the classic `<p>`
  (`uiResolve` in `internal/api/uifiles.go`). `next/` itself is never served.
  So the new shell reuses every classic view and data module, and a classic
  view loaded below `/ui/` imports the new `core/ui.js` and `core/shell.js`.
- A `next/` module that replaces a classic one keeps every export of it
  (`TestUINextKeepsTheClassicExports`); the benchmark views are mounted as
  they are.

```
next/index.html        rail, section menu and <main>; light only (the design defines no dark)
next/app.css           the design's tokens (light), the shell, every classic class restyled
next/app.js            entry of the new shell; imports the classic views and the new ones; an empty
                       hash opens #/overview (Q76)
next/core/shell.js     the design's rail: buttons with titles; Monitor, Zones, Models, Benchmarks;
                       Ask Potluck (a drawer that says it is not built yet), MCP, the System
                       health heart (dot = worst row), the gear Settings (Q69: Profiles, Tiers
                       and zones, Settings, Audit, About, Placement preview, Logs, Classic UI,
                       Q73); section menu (collapsible; closed on the Overview, Q9), fleet card,
                       phone drawer; ?health=open on any page opens the health popup
next/core/health.js    the health popup (Health board): Zones, Runs, Scheduler, MCP server,
                       Storage, Queue, Alerts, Last 2 hours; rows without data are drawn and
                       marked "waiting for data" (grey ring)
next/core/dom.js       the classic dom.js; morph() keeps attributes it did not set
next/core/period.js    Day / Week / Month / All time: bounds, step, stepper label, axis labels
next/core/live.js      the classic live.js; adds the opt-in "requests" topic when hello lists it
next/core/requests.js  requestFeed(): the "requests" topic (snapshot, queued/started/ended, tick);
                       a filtered or older page is read once and the frames laid over it; the 2 s
                       poll runs only while the channel is down; every() (the 100 ms run clock)
next/core/ui.js        the classic components plus panel, segmented, filterSelect, searchField,
                       zoneStatus, colourKey, availStrip, runStatus, sidePanel; for the Overview:
                       dateStepper, dropButton, tokensChart, tokensLegend, runsTable, runLegend,
                       runIcon, runWhy, tickRuns (100 ms run clock), availClass, groupClass,
                       roleShare and runMachines ("Host 50%": a role and a share of the model)
next/views/overview.js Overview, the home page (Main board): Tokens, Requests (the design's "Runs",
                       Q2), Zones with the hour-by-hour strip; Zones toggles outline a request's
                       zones; Customize (Q1, kept in localStorage potluck.overview.layout)
next/views/requests.js Requests: the paged request list behind "All N" (Q3; no mockup yet)
next/views/report.js   24-hour report behind "View 24-hour report" (no mockup yet)
next/views/zones.js    Zones: one row per machine (zone); selection with Stop accepting models /
                       Accept models again; the zone panel (state, facts, recent events, machine card)
next/views/models.js   Models: loaded instances, every model file with size and disk total, the
                       model panel (?model=<key>); #/models/<key> is still views/models/detail.js
```

Each UI links to the other: "Classic UI" at the foot of the new rail, "New UI"
in the classic header and pane. `GET /` on the scheduler redirects to `/ui/`.

Harness case fields for the new shell: `click` (selectors clicked after the
draw), `has` (selectors that must match; `[a^=v]` and `[a*=v]` work), `ws` (live-channel
messages delivered after the draw), `page` and `notOnPage` (texts anywhere on the page, side panels and
dialogs included), `links` (`{text, path}`).

Words of the new shell: a zone is one node; Off = excluded, "Not accepting"
= paused, Set aside = quarantined. Nothing is drawn without data behind it,
no control may reboot or restart a machine (`TestUINoRebootOrRestartControl`),
and there is no load or unload control.

The harness checks both: `scripts/ui-exec.mjs --base URL` runs the live
screens of `/ui/` and of `/ui/classic/`; a case file with `"shell":"next"`
runs the new shell over a merged copy of the files.

Run it: `scripts/ui-dev.sh` (the dev harness), then `http://127.0.0.1:8790/ui/`
(the new shell) or `http://127.0.0.1:8790/ui/classic/`.
One screenshot: `scripts/ui-screens.sh http://127.0.0.1:8790 models loading desktop out.png`.

## Rules

1. **Reality only.** A screen shows what the server sends. Read the field from
   the Go code or the contract before you draw it; if the data does not exist,
   the element does not exist. No sample data in a view.
2. **No load, unload, force load or cancel load control, anywhere.** Running a
   benchmark suite is the one action that causes loads, and it sits behind a
   confirm dialog that says so. `internal/api/ui_test.go` fails the build if a
   UI file names `/api/load`, `/api/unload` or `/api/models/unload`. (The
   catalog `GET /api/models` and `GET /api/models/<key>` are read-only and are
   what the Models screen reads.) The one exception, decided by the owner: in
   agents membership a machine card on Nodes has Pause / Unpause and Force
   unload, and nothing else ("Machines that join through agents" below).
   `internal/api/ui_nodes_test.go` fails the build if the UI gains a third
   node action or names `/api/nodes` outside `core/nodeactions.js`.
3. **WebSocket for live, REST for pull and push.** Live data comes from the
   store (`core/store.js`). REST is for lists and detail on demand and for
   writes. Never call REST on a timer. To refresh a list, react to a WebSocket
   message (`listen('event' | 'bench' | 'config', fn)`) or a store key.
4. **Writes go through `core/api.js`** (`send`, `post`, `put`, `patch`, `del`).
   It adds `X-Source: ui` and `X-Actor`. Never call `fetch` in a view (a test
   checks this). Configuration writes: dry run first (`{dryRun: true}`), show
   the diff with `diffView`, then save with `{ifMatch: revision, comment}`.
5. **Self-contained.** No `http://` or `https://` reference, no CDN, no
   `@import`. Only relative imports of files in this directory.
6. **One stylesheet.** Use the classes below. Do not add a `<style>` block or a
   second CSS file. A small one-off layout may use an inline `style`; a pattern
   used twice belongs in `app.css` with a line in this file.
7. **Phone width (390 px) must work.** Tables go inside `.scroll` (the `table()`
   helper does it) with a `minWidth`; rows of things use `.cols`, `.grid`,
   `.stats`, `.row`, which wrap.
8. **Copy.** Plain words, short sentences. Units always (`GB`, `tok/s`, `s`;
   see Units below). Times as "38 min ago" with the absolute time on hover: use `ago()`,
   `until()`, `at()`. Sentence case. No exclamation marks. Say why something
   waits or fails in plain words: pass the scheduler's `wait_reason` or
   `reason` through `plainReason()` and keep the raw text one click away (a
   tooltip, or `more()` with a `.raw` block). Never print a raw reason string,
   a setting key or a status field name as the sentence.
   Every list has an empty state (`empty()`), and a loading state (`skeleton()`).
9. **A model is a model file, not a profile.** Monitor screens name the
   model (`name` of the catalog) and show the profile id as the small mono
   "configuration". Profiles whose `generated` is true (benchmark matrices)
   are never listed as rows of their own outside Profiles and a model's page:
   they are counted ("24 generated") and folded behind a toggle.
10. **Units.** Anything read at a glance is GB with one decimal, and 1 GB is
   1,024 MiB (2^30 bytes) for memory and file sizes alike: `gb(mib)`,
   `gbNum(mib)`, `bytes(n)`. MiB (`mib()`) appears only in tooltips, in the
   configuration tables (where the setting itself is in MiB) and in the
   benchmark figure tables. Rates stay MB/s as the server measured them. A
   figure is never negative: when two sources disagree, clamp and say so in
   a note (see the Nodes screen).
11. **Identifiers and numbers are mono** (`.mono`): profile ids, node names,
   file names, MiB, counts in tables. Prose is Manrope.

## Tokens (`:root` in app.css)

| Token | Value | Use |
|---|---|---|
| `--bg` `--card` `--inset` | `#0b0d0c` `#111413` `#0e110f` | page, card, inputs and sub-headers |
| `--line` `--line-2` | `#1d2220` `#2c3431` | card and row borders; control borders |
| `--fill` `--hover` | `#1c2320` `#151917` | bar tracks, node chips, selected nav; hover |
| `--text` `--text-2` `--mut` | `#f0f3f1` `#d3d9d5` `#9fa8a3` | text, secondary, muted (all pass 4.5:1 on card) |
| `--mint` (+`-text`, `-bg`, `-2`, `-3`) | `#7ee0b0` | ready, healthy, primary action; model / KV / compute segments |
| `--amber` (+`-text`, `-bg`) | `#f0b849` | loading, in progress, changed, needs a look |
| `--coral` (+`-text`, `-bg`, `-line`) | `#ff5d4a` / `#ff9b8a` | failure, refusal, danger |
| `--lilac` (+`-text`, `-bg`) | `#b79ad8` / `#cdb4ea` | queued, waiting, edited |
| `--fs-xs … --fs-h1` | 11, 12, 13, 14, 16, 20, 26 px | type scale; body is 14 px / 500 |
| `--s1 … --s6` | 4, 8, 12, 16, 20, 26 px | spacing; cards are 16 px apart, card padding is 20 px |
| `--r-card` `--r-ctl` `--r-badge` `--r-chip` | 14, 9, 6, 5 px | radii |

Colour means state. Mint = fine, amber = in progress, coral = wrong, lilac =
waiting, grey = inactive. Do not use a colour for decoration. The one
exception is the avatar of a model (`core/avatar.js`): its hue is the identity
of a model family, muted (28% saturation on a dark tile), and used nowhere
else; an avatar never uses a state colour.

## Charts (`core/charts.js`)

Decided once, used everywhere. No chart library; inline SVG or plain boxes.

- **Colour is state, never a series.** Mint = it worked (loaded, passed),
  coral = failed or cancelled, amber = refused or partly passed, grey = no
  result or a neutral quantity (requests served, a duration line).
- **The number of machines is a shape, not a colour.** 1 PS5 = circle, 2 =
  square, 3 = triangle (`shapeOf(n)`); in the small "last loads" strip it is
  the count of pips under a bar. Loads on different node counts take very
  different times, so a load time is never drawn without its node count, and
  never averaged across node counts.
- **Points for events, a line for one thing measured again.** Loads over time
  are separate events: `pointChart` (no line). One benchmark over its runs is
  the same measurement repeated: `lineChart` (a grey line, the points coloured
  by the result of the run). Counts per hour, day or run: `columns` (stacked).
- **Axes.** The y axis starts at 0 (`zero`), has four grid lines and labels with
  the unit ("25 s", "200 MB/s"), thousands separated. The x axis names its first
  and last moment. The title of the tile says what is drawn and in which unit.
- **Size.** A chart is drawn for 560 px (340 px on a phone) and only shrinks, so
  its labels stay 11 px JetBrains Mono. Plot heights: 170 to 190 px for a main
  chart, 76 to 96 px for a strip of columns. Tiles sit in `.viz-grid` (two
  columns, one on a phone); a tile that needs the full width is `cls: 'wide'`.
- **Tooltip.** Every mark carries `data-tip`: lines of text, the first is the
  title (`tipText(head, lines)`). One floating box for the page shows it on
  hover and on keyboard focus; a mark that can be focused has `tabindex="0"`.
  Order of the lines: what happened, when (ago and the exact time), the
  configuration, the machines, the figures.
- **Empty state.** `noData('Not measured yet', why)`: a dashed frame with the
  words, the same everywhere. Never an empty axis.
- **Text alternative.** A tile is `viz(title, note, body, {legend, alt})`: `alt`
  is one sentence with the figures, read by a screen reader (`p.sr`) and set as
  the chart's `aria-label`. Write it from the data, not "a chart of".
- **Legend.** Under the chart, only what is in it: `legendItems([[shapeSwatch('square'), '2 PS5s'], [colorSwatch('coral'), 'Failed']])`.

| Chart | Call | Use |
|---|---|---|
| Last loads | `loadSpark(marks, {label})` | The strip of a model row: one bar per load (the newest 12), oldest left. Height = load time on the row's own scale, pips = machines, coral = failed, a hollow amber stub = a refused request. |
| Pass dots | `passDots(recent, rate, {label, large})` (`core/rate.js`), `dots(marks)` | One pill per run, oldest left: mint passed, amber partly, coral failed, grey cancelled or no result; the percentage beside it; each pill links to its run. Wherever a pass rate is shown. |
| Points over time | `pointChart({series: [{name, shape, points: [{x, y, color, tip}]}], unit})` | Load time and upload speed of every load of a model. |
| Line over runs | `lineChart({series, unit, time, zero})` | One benchmark or one suite over its runs. |
| Columns | `columns(buckets, {series: [{key, color}], label, tip})` | Loads per hour, benchmarks passed per run. |
| Comparison | `compareBars([{label, value, text, tip}])` | One figure on 1, 2 and 3 machines; a row without a value says "not measured yet". |
| Fit | `fitBar({need, budgets, label, budgetText})` | The memory a model needs against what one, two and three machines give: the most important fact about a model here. |
| Tile | `viz(title, note, body, {legend, alt, cls, key})`, `noData(text, why)` | The frame of every chart. |

## A view

```js
// views/example.js
import { h } from '../core/dom.js';
import { registerView, href, navigate } from '../core/router.js';
import { state, listen } from '../core/store.js';
import * as api from '../core/api.js';
import { liveView, pageHead, table, empty, skeleton } from '../core/ui.js';

registerView({
  id: 'example', title: 'Example',          // the id is the first route segment: #/example
  mount(el, route) {                        // route = {id, rest: [...], query: {...}}
    let rows = null, alive = true;
    const load = async () => { rows = (await api.get('/api/things')).things; if (alive) view.refresh(); };
    const off = listen('config', load);     // refresh on a pushed message, never on a timer
    const view = liveView(el, () => [       // re-drawn on every status message and once a second
      pageHead('Example', 'One line that says what this is'),
      h('section.card', h('div.ch', h('h2', 'Things')),
        rows ? table({ columns: [...], rows, empty: empty('Nothing yet', 'Why, and what to do') }) : skeleton()),
    ]);
    load();
    return { unmount() { alive = false; off(); view.unmount(); },
             update(r) { route = r; view.refresh(); } };   // optional: same view, new rest/query
  },
});
```

Add the import to `app.js` and, for a new screen, the entry in `NAV`
(`core/shell.js`). `views/models.js` is the reference view: read it first.
`liveView(el, render, keys)` morphs the DOM in place, so focus, scroll and
transitions survive. Give list items a `key` (`h('div', {key: id}, …)`). A
subtree the user is typing in must not be re-rendered: mark it `data-keep`
(the `schemaForm` node already is) or build it once outside `render` and
return the same node each time.

`h('tag.class.other', {attr, onclick, key}, ...children)`: children may be
nodes, strings, numbers, arrays, `null`/`false` (skipped).

## The store (`core/store.js`)

`state.conn` (`connecting`, `live`, `reconnecting`), `state.hello`,
`state.status` (the `/api/status` document, pushed), `state.events` (history
events as they happen, newest first), `state.bench` (`{state, run}`: the
`/api/bench/state` document and the progress object of the active run),
`state.configRevision`, `state.tick`.

- `on(keys, fn)` runs `fn` after a key changed (`status`, `events`, `bench`,
  `conn`, `hello`, `configRevision`, `tick`); returns the unsubscribe function.
- `listen(type, fn)` gets every raw frame's `data` of a WebSocket message type
  (`hello`, `status`, `event`, `bench`, `config`, `ping`).

## Shell (`core/shell.js`)

- `NAV`: the navigation groups and entries.
- `setShellSlot(name, node | null)`: puts a node into the foot of the pane.
  The compact benchmark-run indicator uses the slot `run`.
- There is no "Live" indicator. `connection()` is `null` while all is well,
  else `{title, note}`: the shell shows it as a slim bar above the screen
  (`.connbar`) when the live channel is connecting for more than a moment, was
  lost, or the fleet feed is stale or disconnected, and removes it by itself.
  Do not draw a second warning in a view (`staleBanner()` returns nothing now).
  `connectionFacts()` is the same in words for the About screen.

## Components

Functions are in `core/ui.js` unless noted. Each line gives the call and the
markup it produces, so the markup can also be written with `h()` directly.

| Component | Call | Markup and rules |
|---|---|---|
| Page header | `pageHead(title, sub, actions)` | `div.page-head > h1 + span.sub + div.actions`. One per screen, first child. `sub` is the count line ("2 ready · 1 loading"); wrap the numbers in `<b>`. |
| Card | `h('section.card', …)` | `.card` holds `.ch` (header: `h2`, optional `.note`, optional right-aligned item with `.right`; `.ch.split` pushes the last item right), then rows. `.cb` = padded body, `.cs` = padded section with a top rule, `.day` = a grey sub-header row. `.card.flag-warn` / `.flag-err` tint the border. |
| Layout | | `.cols > .wide + .side` (main column and a 320 to 420 px side column that wraps under on a phone); `.grid` (auto-fill tiles, 320 px min); `.row` (wrapping flex row); `.col` (16 px stack). |
| Badge | `badge(text, kind, title)` | `span.b.b-<kind>`, 22 px high. Kinds: `ready` (solid mint: ready, passed), `use` (mint tint: done, healthy, in use), `load` (amber: loading, running, changed), `q` (lilac: queued, edited), `n` (grey: not loaded, neutral), `settle` (light grey: settling, draining, needs restart), `aside` (coral tint: held back, quarantined, regression), `fail` (solid coral: failed), `line` (mint outline), `dash` (dashed: excluded, off). One badge per state; never two colours for one fact. |
| Node chip | `nodeChip(name)`, `nodeChips(names)` | `span.node` (mono, on `--fill`). Every node name in prose or a table is a chip. |
| Button | `button(label, onclick, kind)` | `button.btn` + `pri` (one per screen: the main action), `dng` (outline coral), `dng solid` (confirm of a dangerous action), `sm`, `lg`, `ghost`, `icon`. |
| Stat row | `h('div.stats', stat(label, value, unit, {title, small}))` | `div.stat > span + b (> small)`. Label above, 20 px figure, unit small and muted. |
| Memory bar | `memoryRow(name, {model_mib, kv_mib, compute_mib, total_mib}, budgetMiB, extra)` | `div.nr`: node name, segmented `.bar` (`m` model, `k` KV cache, `c` compute, `o` other), "used / usable GB", the split. One row per node. Show the legend once per screen: `legend([['m','Model'],['k','KV cache'],['c','Compute']])`. |
| Node memory | `memBar({model, other, total, limit}, ariaLabel)` | `div.membar > div.track > span.m + span.o` and one `i.lim`. One node's whole memory on one track: solid segments for models and other use, the rest of the track is free, and a single marker line where a new model must stop (free minus headroom). No outlined boxes; the marker is left out when it would sit at the end. |
| Facts (aligned) | `facts([[label, node, title]])` | `dl.facts`: small aligned label / value pairs with mono values (a card footer: address, priority, worker). |
| Copy | `copyButton(text, what)` | `button.copy`: next to a long identifier that is shown shortened (`shortId(s, 12)`, the full value in the `title`). |
| Fold | `more(open, onToggle, show, hide)` | `button.more`: a quiet text button that unfolds something long (the scheduler's own words in a `.raw` block, the generated profiles, the command line). Keep the open state in the view, not in the DOM. |
| Back link | `backLink(label, href)` | `a.back`: above the page header of a detail page. |
| Section title | `h('div.sec-title', h('h2', …), h('span.note', …))` | A heading between cards, when a group of cards needs a name. |
| Toolbar | `h('div.toolbar', searchInput, chips(...))` | Search first, then the filter chips, under a card header. Build the input once with `data-keep` so the caret survives a redraw. |
| Grouped table | `table.tbl.dense` with `tr.grp` (a group header row, `colspan`) and `tr.sub` (its rows) | A long list grouped by something (profiles by model file). |
| Clipped cell | column `cls: 'clip'` | The cell gives way and cuts its text with an ellipsis, so the figures beside it never clip in a narrow side table. |
| Bar | `bar([{cls, value, title}], total, ariaLabel, size)` | `div.bar > span.<cls>`. Classes: `m k c o` (memory), `a` amber, `x` coral, `l` lilac, `ld` striped (expected), `rsv` outline (reserved). Sizes: default 10 px, `lg` 14 px, `sm` 8 px. Always pass an aria label that says the figures. |
| Progress | `progress({fraction, cls, left, mid, right, foot: [l, r], striped, label})` | `div.prog`: a line above (percent as `span.pct`, time left, deadline), a 14 px bar, a mono line below (started, deadline). |
| Step tracker | `steps([{label, note, state}])` | `div.steps > div.step(.done|.now|.fail)`. The phases of one load or one benchmark, left to right; `note` is the live figure of the current step. |
| Table | `table({columns: [{label, cls, title, render}], rows, key, onRow, minWidth, empty, rowClass})` | `div.scroll > table.tbl`. `cls: 'r'` right-aligns and sets mono (numbers), `'tight'` stops wrapping. A second line in a cell is `span.sub`. `onRow` makes rows clickable (and Enter). Sort and filter outside the table. |
| Facts | `h('dl.kv', h('dt', …), h('dd', …))` | Two-column label / value list (command line, storage paths). |
| Reason list | `h('div.why', h('i.a'), h('span', text), badgeOrEmptySpan)` | Three cells per reason: a colour square (`a` amber, `x` coral, `l` lilac, `m` mint, `s` light, none = grey), the sentence, an optional badge. Used for "why is it waiting". |
| Side list row | `h('div.bl', h('span', what), h('span.mono', when), h('span.mut', detail))` | Title left, value right, detail on the next line. |
| Event row | `h('div.ev', time, typeBadge, h('div', h('p', sentence), h('p.d', detail)))` | `.ev.sev-warn` / `.sev-error` add a left edge. |
| Filter chips | `chips([{id, label, count}], current, onPick, ariaLabel)` | `div.chips > button.fc(.on)`. Single choice; the first is "All". |
| Tabs | `tabs([{id, label, count}], current, onPick)` | `div.tabs > button.tab(.on)`. Sub navigation inside one screen; keep the tab in the route (`navigate(id, rest, query, true)`). |
| Banner | `banner(kind, content, action)` | `div.banner(.info|.warn|.err)`. A condition of the whole screen. A lost connection or a stale fleet feed is not a view's business: the shell shows it above every screen. |
| Avatar | `avatar(model, 28 | 40 | 72)`, `named(model, node)` (`core/avatar.js`) | `span.av`: the owner's icon when `model.icon` is set, else a tile with the family's letters in the family's hue (`model.brand`), a prompt glyph for a command service. 40 px in the Models list, 28 px in a card header or a table of another screen, 72 px in a page header. Wherever a model is named prominently. |
| Panel | `aside.drawer` in `div.drawer-back` (see `views/benchmarks.js`) | A panel from the right over the page, for the configuration of the thing the page is about. It is in the route (`?edit=1`), so Back closes it; Escape and a click beside it too. |
| Snippet | `div.snip > div.snip-h + pre` | A block of text to copy (a command, a configuration), with a Copy button in its header. |
| Empty state | `empty(title, text, action)` | `div.empty > b + p (+ button)`. Says what is not there, why, and what would make it appear. |
| Loading state | `skeleton(lines)` | Pulsing lines; while the first REST answer is on its way. |
| Form from a schema | `schemaForm({fields, values, onChange, showDefaults})` | `div.form-grid > div.field`. `fields` are schema objects `{key, type, enum, default, min, max, unit, description, restart_required, label}`; types `duration`, `int`, `float`, `bool`, `enum`, `string`, `text`, `list`, anything else as JSON. Dotted keys reach into objects. Returns `{el, values(), validate(), setErrors({key: msg}), changed()}`. Shows "Changed from default", "Edited" and "Needs restart" marks by itself. |
| Form pieces | `toggle(checked, onChange, label)`, `tagInput(values, onChange, placeholder)` | `button.switch[role=switch]`; `div.tags`. Plain inputs: `input.input`, `select.select`, `textarea.textarea` inside `div.field > label + control + span.help + span.err`; a unit suffix: `div.unit > input.input + span`. Buttons of a form: `div.form-foot`. |
| Diff | `diffView([{path, old, new}])` | `div.diff > div.diff-row > span.path + span.old + span.new`. The answer of a dry run, an audit entry, a revision. `null` old = "not set", `null` new = "removed". |
| Dialog | `dialog({title, body, actions: [{label, kind, value, autofocus}], wide})` | Promise of the picked value; Escape or the backdrop gives `undefined`. Focus is trapped and returned. |
| Confirm | `confirm({title, intro, hits: [{kind, text}], confirmLabel, cancelLabel, danger, extra})` | Promise of a boolean. `hits` are the consequences, one line each (`kind: 'x'` coral = it interrupts or destroys, `'a'` amber, `''` grey = for information). The confirm button names the action ("Run 8 benchmarks"), never "OK". `danger` makes it solid coral and puts the focus on Cancel. |
| Prompt | `promptText({title, intro, label, value, placeholder})` | Promise of a string or `null`. |
| Toast | `toast(text, kind)` | `ok`, `err` or neutral; bottom right; for the result of an action ("Saved as revision 15"). Not for live events. |
| Sparkline | `sparkline(values, {width, height, color, flag, label})` (`core/charts.js`) | Inline SVG line, last point marked, `flag` = indexes drawn coral (regressions). |
| Charts | see "Charts" above | `loadSpark`, `passDots`, `pointChart`, `lineChart`, `columns`, `compareBars`, `fitBar`, in a `viz` tile. |
| Meter | `barMeter(value, max, cls)` (`core/charts.js`) | A thin bar for a table cell. |
| Step segments | `segments(index, total, ariaLabel)` | `div.bar.sm.seg`: equal steps, done mint, current amber, rest grey ("phase 2 of 5"). |
| Change mark | `changeMark(pct, {better: 'lower' or 'higher', regression})`, `signedPct(v)` | A signed percent next to a figure: mint better, amber worse, coral plus a "Regression" badge past the limit, grey under 1%. |
| Pick cards | `pickCards([{title, text, chips, onPick}])` | `div.grid > button.card.pick`: choose a starting point (templates). |
| Repeatable block | `subBlock(title, actions, ...body)`, `orderButtons({index, count, onMove, onRemove, what})` | `div.sub-block > div.sub-head + fields`: one item of an ordered list inside a form (a phase, a request, a turn, a tier) with its up, down and Remove buttons. Put a `schemaForm(...).el` in the body. |
| Pending changes | `pendingBar(o)`, `reviewChanges(o)`, `savedBanner(result, onClose)`, `noticeBanner(notice, o)`, `watchConfig(o)`, `restartKey(k)` (`core/changes.js`) | The save flow of every Configure screen: a bar that counts staged changes, the review dialog (dry run, diff, warnings, keys that need a restart, comment, save with `If-Match`, the 409 path that shows what changed and applies again), the "Saved as revision N" banner, and the notice when someone else changed the configuration while you edit. Read the header of `core/changes.js` for the options. |
| Config helpers | `sourceBadge(source)`, `searchBox(value, onInput, placeholder, label)`, `fieldOfMessage(msg, keys)`, `entrySentence(e)`, `sectionName`, `actionName` (`core/configdata.js`) | Source badges (web UI, REST, MCP, import, system), a search input, mapping a validation message to its field, one sentence for an audit entry. |
| Machine actions | `div.acts` in a `section.card.ncard` (see `views/nodes.js`) | The row of small buttons of a machine card, between the body and the foot: `button(label, fn, 'sm')` and `button('Force unload', fn, 'sm dng')`. While a request is on its way the row has `aria-busy` and both buttons are disabled; the one that was pressed reads "Pausing…". |
| Shell slot | `setShellSlot(name, node or null)` (`core/shell.js`) | Shows a node in the pane above the connection card, or under the top bar at phone width. The benchmark run indicator uses `run`. Update the node in place; the shell never re-creates it. |

Formatting (`core/format.js`): `num(v, digits)`, `mib(v)`, `gib(v)`,
`bytes(v)` (GB / MB), `pct(v)`, `tps(v)`, `ctx(v)` ("16k"), `dur(seconds)`
("5 min 48 s"), `durShort(seconds)`, `ago(t)`, `until(t)`, `at(t)` (all three
return a `<time>` with the absolute time as its title), `agoText(t)`,
`clock(t)`, `dateTime(t)`, `dayLabel(t)`, `plural(n, 'request')`,
`parseGoDuration('15m')`, `now()` (the server's clock), `DASH`.
Times may be unix milliseconds or RFC 3339 strings.

Reasons (`core/reasons.js`, re-exported by `core/status.js`): `plainReason(raw)`
(a plain sentence), `shortReason(raw)` (a few words for a table cell),
`plainBasis(raw)` (where a memory need comes from), `refusalWord(reason)`.
The patterns were written from the strings the real system produced
(`cmd/devcluster/realdata`); an unknown wording is only tidied, never guessed.
When the scheduler gains a reason, add its pattern there.

Status helpers (`core/status.js`): `machines(st, n)` ("3 PS5s", or "3 nodes"
when the pool is mixed), `nodeName(st, id)`, `findNode(st, idOrHost)`,
`nodeByAddr(st, addr)`, `instanceNodeNames(st, inst)`, `nodeMemory(placed)`,
`instanceBadge(inst)`, `healthBadge(node)`, `machineBadge(st, node)` (agents
membership: "Paused" or "Not placeable" with the reason on hover, else null),
`drift()`, `staleBanner()`.

## The configuration and audit contract (as built on the server)

For the Configure group. The exact shapes are in `internal/api/config.go`; the dev harness mocks them.

- `GET /api/config` -> `{revision, updated_at, configured, settings, settings_set,
  tiers[], profiles{}, exclude_nodes[], node_overrides{}, recovery{}, fleet{}}`.
  `configured: false` means nothing was imported yet (design that empty state:
  no profiles, no tiers). `settings` has every key with the value in force;
  `settings_set` lists the keys that were set (changed from default). A `null`
  in a settings PATCH resets a key to its default.
- `GET /api/config/schema` -> `{fields: [{key, scope, section, type, enum,
  default, min, max, unit, description, restart_required, required, nullable,
  applies_to}]}`. `scope` is `settings`, `recovery`, `fleet`, `profile` or
  `tier`. Sections of the settings scope: `timing`, `capacity`, `runtime`,
  `safety`, `workers`, `retention`, `api`, `bench`. Render forms from this with
  `schemaForm`; do not hard-code a field list.
  Profile fields: `kind`, `model`, `ctx`, `parallel`, `kv_quant`, `flash_attn`,
  `needs_mib`, `allow_tiers`, `force_nodes`, `tensor_split`, `extra_args`,
  `cmd`, `check_endpoint`, `aliases`, `ttl`, `load_timeout`, `stall_timeout`,
  `load_stall_timeout`. Tier fields: `match.hostname`, `match.role`,
  `match.source`, `match.local`, `priority`, `headroom_mib`, `shared_memory`,
  `link_mb_s`, `settle`.
- Section reads are wrapped: `GET /api/config/{profiles|tiers|settings|exclude-nodes|node-overrides|recovery|fleet}`
  -> `{revision, <section>: ...}`; `GET /api/config/profiles/{id}` -> `{revision, id, profile}`.
- Writes: `PATCH /api/config/settings` (only the keys to change), `POST /api/config/profiles`
  (`{id, profile}`), `PUT` and `DELETE /api/config/profiles/{id}`, `PUT /api/config/tiers`
  (`{tiers: [...]}`), `PUT /api/config/exclude-nodes`, `PUT /api/config/node-overrides`,
  `PUT` or `PATCH /api/config/recovery` and `/fleet`. With `core/api.js`:
  `api.patch(path, body, {dryRun: true, ifMatch: revision, comment})`.
  The answer: `{revision, audit_entry_ids, changed, dry_run, diff: [{path, old, new}],
  warnings: [...], restart_required: [keys that only take effect at the next start]}`.
  Show `warnings` and `restart_required` in the review step. A write that changes
  nothing creates no revision (`changed: false`).
- Errors (`ApiError`: `.status`, `.message`, `.body`): validation is `400` with
  `body.error.messages` (show every message); a stale `If-Match` is `409` with
  `body.error.type == "stale_revision"` and `body.error.current_revision` (show
  what changed since, from `/api/audit`, and let the user apply again); deleting a
  profile that benchmarks refer to is `409` with `body.error.warnings` unless
  `?force=1` (`{params: {force: 1}}`).
- Revisions: `GET /api/config/revisions` -> `{revisions: [...], current}`;
  `GET /api/config/revisions/{n}` -> `{revision, ts, source, actor, comment, config, document}`;
  `POST /api/config/revisions/{n}/rollback` (dry run, If-Match and comment as any write).
- Audit: `GET /api/audit?since=&until=&source=&actor=&section=&target=&action=&q=&limit=`
  -> `{entries: [{id, ts, source, actor, remote_addr, user_agent, action, section,
  target, before, after, diff, revision, comment}], total_matching}`;
  `GET /api/audit/{id}`. Sources: `ui`, `rest`, `mcp`, `import`, `system`.
- Live: `state.configRevision` (from `hello`, `status` and the `config` topic);
  `listen('config', fn)` gets `{revision, audit_entry_ids, section, target, source, actor}`
  on every change, whoever made it. Use it to refresh an open list and to warn
  someone who is editing that the configuration changed under them.
- The status has `configured` and, per instance, `stale_profile: true` when the
  profile was changed or deleted after the model loaded (Models shows "Older
  definition").

## The model catalog (as built on the server)

`GET /api/models` -> `{models: [...], models_dir, history}`: one entry per
model file (`internal/api/models.go`). An entry: `key`
(the file name, or `command:<profile id>`), `kind`, `file`, `exists`,
`size_bytes`, `name`, `publisher`, `family`, `parameters`, `quant`,
`context_length`, `meta_source`, `state` (`ready`, `loading`, `not_loaded`),
`profiles[]` (`id, ctx, parallel, placement, force_nodes, tensor_split,
needs_mib, needs_basis, needs_measured, aliases, generated, plan`),
`profile_count`, `generated_count`, `startable_count`, `instances[]`,
`measured[]` (`nodes, memory_mib, profile, ctx, at, source`), `fit`
(`need_mib, basis, min_nodes, ran_on_nodes, node_budget_mib,
cluster_budget_mib`), `loads` (`count, ready, failed, cancelled, unloads,
refusals, last, by_nodes[]`), `last_loaded`, `last_active`,
`requests_served`, `last_refusal`, `bench` (`benchmarks, runs, by_nodes[]`).
Also per entry: `raw_name` (the name as the file wrote it, when `name` was
tidied), `brand` (the model family: gemma, qwen, llama, mistral, deepseek,
glm, kimi, tinyllama, ..., `other`, `command`), `icon` (`{etag, scope:
"model" | "family"}` or null), `loads.recent[]` (the newest 12 marks: `at,
profile, nodes` (a count), `outcome` (`ready`, `failed`, `cancelled`,
`refused`), `load_s, mb_s`, and `count` for a run of refusals) and
`fit.budgets_mib[]` (what one, two, three machines give together).
`GET /api/models/<key>` adds `events[]`, `bench.detail[]` (each with
`recent[]`, the pass/fail history) and `charts`: `loads[]` (every load, oldest
first: `at, profile, nodes, outcome, load_s, mb_s, memory_mib, ctx, stayed_s,
requests, ended, bench_run`) and `activity.hours[24]` / `activity.days[7]`
(`at, loads, failed, refusals, requests`).

The catalog is kept in memory on the server and on the page. Use
`core/catalog.js`: `catalog.models`, `catalog.byKey`, `catalog.byProfile`,
`onCatalog(fn)`, `needCatalog()`, `modelFor({profile, model_file})`. The live
channel sends the whole catalog when it connects (topic `models`) and then
only the entries that changed, so a view never fetches it and never polls;
going back to Models draws at once. The instance cards are drawn from the
WebSocket status (they need the live load figures). A model's icon: `PUT`
(raw image body), `GET`, `DELETE /api/models/<key>/icon[?scope=family]`
(`api.upload`, `api.del`).

## Machines that join through agents (as built on the server)

With `defaults.membership: agents` the status (`GET /api/status` and the
WebSocket `status` message) has `membership: "agents"` and `agents[]`: the
facts of every agent node (`internal/pool` `NodeFacts`: `id, name, addr,
state` (`active`, `lost`, `left`, `removed`), `platform, programs_ready,
programs_error, model_source, paused, host_agent, role, launch_id,
launch_state, devices, mem_available_mib, stats, placeable, unplaced`). Each
`view.nodes[]` entry has `unplaced` (a reason word, or empty), `eligible`
and `agent` (its facts). In fleet-feed mode none of these exist. Not to be
confused with the AI-agent identities of the next section (`/api/agents`).

Everything about them is in `core/nodeactions.js` (no DOM, no import):

- `agentsMode(st)`: membership is agents and the status has `agents`.
  `agentOf(st, node)`: the facts of a view node, or null (fleet-feed mode, and
  the machine the scheduler runs on, which has no agent).
- `notPlaceable(node, agent)` -> null, or `{short, text, known, code,
  paused}`: why the machine takes no new model. The reason words
  (`no_tier_match`, `bad_floor`, `programs_not_ready`, `model_source_unreachable`,
  `orphan_launch`, `silent`, `agent_unreachable`, `host_machine`) are mapped to
  a few words and a sentence in `UNPLACED`; a word that is not in the table is
  shown as it is (`known: false`), never guessed at. A paused machine without
  another reason has `code: 'paused'`. When the scheduler gains a reason word,
  add it to `UNPLACED` and to the list in `internal/api/ui_nodes_test.go`.
- `sendNodeAction(api, id, action, comment)` with `action` `pause`, `unpause`
  or `force`: `POST /api/nodes/{id}/pause | unpause | force-unload`
  (`internal/api/nodes.go`). The comment is required; it goes in the body
  (`{comment}`, kept exactly) and in `X-Comment`. Each call writes an audit
  entry (section `nodes`, target `node:<id>`).

Wording: a text that both memberships show is written once, in its fleet
wording, and passed through `poolText(agents, text)` (`core/words.js`): with
agents, "absent from the fleet" reads "absent from the pool" and so on. Its
table is `PoolPhrases` in `internal/config/wording.go`, which the scheduler's
own messages use; `internal/api/ui_standalone_test.go` keeps the two equal.
A fact that exists in one mode only (the fleet feed's age, recovery requests)
is not reworded: in agents mode the connection facts and About say that the
nodes come from agent registrations, and Settings folds the recovery and
fleet sections under "Not used in this mode" (node overrides on Tiers get the
same badge).

Where it shows:

- **Nodes.** A card gets a "Paused" badge (`settle`) and, for any other
  reason, a "Not placeable" badge (`aside`) with the sentence as a note in the
  card (the raw word is the tooltip of the note). "Free" is not shown for a
  machine that takes nothing, its "Usable now" is a dash, and it is left out of
  "usable now" in the page header; machines that are not placeable for a reason
  other than a pause count as "need a look". The two controls sit in
  `div.acts`: **Pause / Unpause** (one button, by `agent.paused`) and **Force
  unload** (outline coral; enabled only when the machine has a launch or holds
  an instance: `holdsSomething`). Both open a dialog that says what will
  happen (the force unload: the model stops at once and requests waiting on it
  fail; its button is solid coral), with a required comment. The server's
  refusal is shown in the dialog in its own words. Nothing is drawn as done
  early: after the server said yes the card reads "Pausing…" until the next
  status message. A machine without agent facts has no controls, so there are
  none in fleet-feed mode.
- **Models.** A machine row of an instance card shows `machineBadge` when the
  machine has no health badge.
- **Queue.** "Why it waits" lists a free machine that is paused or not
  placeable, with the sentence.

Agent nodes that are no longer in `view.nodes` (left or removed) are not drawn
yet; re-admit and remove have no control in the UI.

## Agents and MCP (as built on the server)

`GET /api/agents` -> `{agents: [{id, name, description, key_prefix, created_at,
created_by: {source, actor}, last_used_at, rotated_at, revoked_at, revoked,
request_count, actor ("agent:<name>"), audit_entries}], note}`. `POST
/api/agents {name, description}` -> `{agent, key}`: the key is in this answer
and nowhere else, ever: show it once. `PATCH /api/agents/{id}`, `POST
/api/agents/{id}/rotate` (-> a new `key`), `DELETE /api/agents/{id}` (revoke;
the row stays). `GET /api/mcp/tools` -> `{server, tools: [{name, group,
summary, description, read_only, destructive}], groups: [{id, title, note}]}`:
draw the tool list from it, do not hard-code a tool. A key is identity for the
audit log, not access control: say so wherever keys are shown. Audit entries
made with a key have `actor: "agent:<name>"` and `agent_id`;
`GET /api/audit?agent=<id>` filters by it. The MCP screen builds the address it
shows from `location.origin` at run time: no UI file names a host. The web UI
still has no load or unload control; the MCP screen only says that agents do.

## The benchmark contract (as built on the server)

For the Benchmarks group. The exact shapes are in `internal/api/bench.go` and
`internal/benchspec`; the dev harness mocks them. Read the real code
before drawing a field.

- A benchmark is `{id, name, description, profile, phases: [{type, ...options}],
  timeout_s, expect, tags}`. A benchmark without `phases` is the simple form
  (load, warm-up, requests, unload). Phase types: `load` (cold load only, no
  chat request), `warmup`, `requests`, `conversation` (multi-turn), `hold`
  (idle N seconds, then one request), `concurrent_requests`, `unload`.
- `GET /api/bench/schema` -> the phase types, each with `title`, `description`,
  `records` and `options[]` (`name, type, default, min, max, unit, description`),
  plus `expect_keys` (`outcome, max_load_s, min_decode_tps, min_prefill_tps,
  max_ttft_ms, output_sha256, min_reused_tokens, max_concurrent_wall_s,
  max_errors, max_unload_s`), `templates` (`load-only`, `load-and-requests`,
  `load-and-conversation`, `requests-on-loaded`, `simultaneous-requests`),
  `request_options`, `turn_options`, `benchmark_options`, `limits`. Render the
  editor from it (map an option to a `schemaForm` field: `key` = its `name`);
  offer the templates as starting points.
- Progress: `GET /api/bench/state` -> `{enabled, current, queue, next_scheduled,
  run}` where `run` is the progress object or null, and WebSocket `bench`
  messages with `kind: "progress"` carry the same object as `run`:
  `{id, suite_id, suite_name, state, percent, started_at (unix ms), elapsed_s,
  eta_s (may be null), steps_total, steps_done, current: {index (0-based),
  benchmark_id, name, description, profile, model_file, nodes, phase: {index,
  type, label, phases_total}, percent, detail} | null (until the first benchmark
  starts), next: {benchmark_id, name, description, profile} | null,
  queue: [the benchmark ids still to run]}`. Mind the two queues: `state.queue`
  (top level of `GET /api/bench/state`) is the list of queued RUNS; `run.queue`
  is the benchmarks left in the active run.
  For a concurrent step `current.profile` is the members joined with " + " and
  `detail.members` lists them. `detail` during a load phase is `{text, load}`
  where `load` is the scheduler's load object (the one the Models screen draws:
  `progress`, `eta_s`, `stalled_s`, `workers: [{addr, bytes_total, bytes_sent,
  state}]`; map `addr` to a node with `nodeByAddr`); during a conversation it is
  `{text, turn, turns, tokens, max_tokens}`.
  The store keeps it as `state.bench.run` (null when no run is active);
  subscribe with `on('bench', fn)`.
- Other `bench` message kinds (`listen('bench', fn)`): `state` (on connect: the
  state document), `run` (a run changed state), `step_started`, `step_finished`,
  `request` (one request of the step in flight: `run_id, idx, benchmark_id,
  phase, request, seq, state` started or streaming, `tokens`, `elapsed_ms`),
  `load` (the load object of the step in flight) and `aggregate_updated`
  (`run_id, suite_id, state, benchmarks`: refresh aggregates and run lists on it).
- A run records its environment: `env` (`scheduler_version`, `config_revision`
  (a string), `llama_binary`, `workers[]` with `rpc_server_version`). Show it on
  the run detail. A run `Step` has no `nodes` field: use `node_count`,
  `load.nodes[].node` and `env.workers`.
- A failed validation answers with one text per problem, joined by newlines,
  each starting with where it is: `phases[1] (conversation): ...`. Show each
  next to its phase.
  Run detail is phase-aware: a load-only benchmark shows load metrics; a
  conversation shows per turn prefill, decode, time to first token and reused
  prompt tokens.
- A suite and a benchmark have their own page (`#/benchmarks/suite/<id>`,
  `#/benchmarks/benchmark/<id>`): run history first, then the trend, then what
  it contains or expects. Their configuration opens as a panel (`?edit=1`).
  `GET /api/bench/benchmarks/<id>/results` -> `{results: [{run_id, suite_id,
  suite_name, run_state, at, result, outcome, reason, profile, model_file,
  nodes, duration_s, load_s, load_mb_s, memory_mib, decode_tps, prefill_tps,
  ttft_ms, expect, regression}]}`, oldest first. Every suite and benchmark of
  the aggregates has `recent[]` (`{run_id, at, result: pass | partial | fail |
  cancelled | none, passed, failed, outcome}`, oldest first, at most 30): draw
  a pass rate with `passDots(recent, pass_rate)`, never as a bare percentage.
- External results (`views/benchmarks/external.js`): result sets measured
  outside the runner. The overview lists `GET /api/bench/external` ->
  `{external: [{id, title, description, note, created_at, updated_at, tables:
  [{title, columns, row_count}], attachments: [{name, bytes}], models}]}`;
  `#/benchmarks/external/<id>` shows `GET /api/bench/external/<id>` (`tables[]`
  with `columns[] {key, label, unit, higher_is_better}` and `rows[]` keyed by
  column, plus optional `model` and `profile`; `attachments[] {name, text}`).
  Read-only; headers sort, long text cells fold.
- Aggregates: `GET /api/bench/aggregates?suite=&benchmark=&profile=&since=&until=&env=`
  -> `{runs_considered, environment, benchmarks: [{benchmark_id, runs, steps,
  pass_rate (a fraction, 0 to 1; null when nothing was judged), judged, outcomes, metrics: {<metric>: {count, min, median, mean, max, stdev,
  last, best, change_pct, threshold_pct, regression, points: [{run_id, at, value}]}},
  turns: [...], last_run_id, last_at}], suites: [...], by_model: [...],
  by_model_nodes: [{model_file, nodes, runs, steps, metrics}]}`.
- The compact run indicator in the shell: `setShellSlot('run', node)` (`core/shell.js`).
- The one-click run: `confirm({...})` first, saying that it will load and unload
  models on the cluster, then `api.post('/api/bench/suites/<id>/run', {note})`.

## The dev harness

`scripts/ui-dev.sh [scenario]` runs `cmd/devcluster` on `127.0.0.1:8790`: the real
scheduler server, a scripted status per scenario, seeded history, and the mock
of the endpoints above. `GET /dev/scenarios` lists the scenarios;
`POST /dev/scenario?name=<name>` switches. `scripts/ui-dev.sh agents` shows
agents membership, scripted (`cmd/devcluster/agents_scenario.go`): a machine
with a model, a paused one, one that matches no tier and a free one; Pause,
Unpause and Force unload change that scripted status (no audit entry is
written there). `scripts/ui-dev.sh real` replays a
recording of the real cluster instead (254 profiles, real reason strings,
real history, benchmark runs and audit trail; read-only; `real-loading` is the
same at the moment of a load): look at every change with it too, because the
scripted states are small and tidy and the real one is not. Scenario data for a screen group
goes into its own file in `cmd/devcluster`. Name screenshots
`<screen>-<scenario>-<desktop|phone>.png`; they are not kept in the repository.

A fixture without a scheduler: `node scripts/ui-fixture-server.mjs [--port 3122]
[--state normal|empty|off|loading]` serves `/ui/` (the next overlay, as
`uiResolve` does) and fixed answers for `/api/status`, `/api/requests`,
`/api/requests/summary`, `/api/nodes/availability`, `/api/nodes` and
`/api/models`, plus a `/ws` that sends hello and the status. Its data mirrors
the Overview mockup's sample (generic names, 192.0.2.x, 02:00:00 MACs), so the
page can be compared with the artboard. It writes nothing (every write is 405).
