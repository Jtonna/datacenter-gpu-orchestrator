# LLM Scheduler web UI

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
core/shell.js       navigation, connection indicator, the name that signs changes
core/status.js      helpers that read the status document
core/ui.js          the shared components (below)
core/charts.js      sparkline(), lineChart(), barMeter(): inline SVG
views/<id>.js       one module per screen; it registers itself
fonts/              Manrope (variable) and JetBrains Mono, with their licences (OFL)
```

Run it: `scripts/ui-dev.sh` (the dev harness), then `http://127.0.0.1:8790/ui/`.
One screenshot: `scripts/ui-screens.sh http://127.0.0.1:8790 models loading desktop out.png`.

## Rules

1. **Reality only.** A screen shows what the server sends. Read the field from
   the Go code or the contract before you draw it; if the data does not exist,
   the element does not exist. No sample data in a view.
2. **No load, unload, force load or cancel load control, anywhere.** Running a
   benchmark suite is the one action that causes loads, and it sits behind a
   confirm dialog that says so. `internal/api/ui_test.go` fails the build if a
   UI file names `/api/load`, `/api/unload` or `/api/models/unload`.
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
8. **Copy.** Plain words, short sentences. Units always (`MiB`, `GB`, `tok/s`,
   `s`). Times as "38 min ago" with the absolute time on hover: use `ago()`,
   `until()`, `at()`. Sentence case. No exclamation marks. Say why something
   waits or fails in the scheduler's own words (`wait_reason`, `reason`).
   Every list has an empty state (`empty()`), and a loading state (`skeleton()`).
9. **Identifiers and numbers are mono** (`.mono`): profile ids, node names,
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
waiting, grey = inactive. Do not use a colour for decoration.

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
- `setShellSlot(name, node | null)`: puts a node into the pane above the
  connection card. The compact benchmark-run indicator uses the slot `run`.
- `connection()`: `{dot, title, note}` of the indicator.

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
| Memory bar | `memoryRow(name, {model_mib, kv_mib, compute_mib, total_mib}, budgetMiB, extra)` | `div.nr`: node name, segmented `.bar` (`m` model, `k` KV cache, `c` compute, `o` other), "used / usable MiB", the split. One row per node. Show the legend once per screen: `legend([['m','Model'],['k','KV cache'],['c','Compute']])`. |
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
| Banner | `banner(kind, content, action)` | `div.banner(.info|.warn|.err)`. A condition of the whole screen. `staleBanner()` (`core/status.js`) is the shared one for a lost connection or a stale fleet feed: put it right under the page header of every Monitor screen. |
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
| Line chart | `lineChart({series: [{name, color, points: [{x, y, flag, title}]}], unit, time, zero, height, label})` (`core/charts.js`) | Inline SVG with a y axis and four grid lines; each point has a hover title. Colours: `mint`, `amber`, `coral`, `lilac`, `mut`. |
| Meter | `barMeter(value, max, cls)` (`core/charts.js`) | A thin bar for a table cell. |

Formatting (`core/format.js`): `num(v, digits)`, `mib(v)`, `gib(v)`,
`bytes(v)` (GB / MB), `pct(v)`, `tps(v)`, `ctx(v)` ("16k"), `dur(seconds)`
("5 min 48 s"), `durShort(seconds)`, `ago(t)`, `until(t)`, `at(t)` (all three
return a `<time>` with the absolute time as its title), `agoText(t)`,
`clock(t)`, `dateTime(t)`, `dayLabel(t)`, `plural(n, 'request')`,
`parseGoDuration('15m')`, `now()` (the server's clock), `DASH`.
Times may be unix milliseconds or RFC 3339 strings.

Status helpers (`core/status.js`): `nodeName(st, id)`, `findNode(st, idOrHost)`,
`nodeByAddr(st, addr)`, `instanceNodeNames(st, inst)`, `nodeMemory(placed)`,
`instanceBadge(inst)`, `healthBadge(node)`, `drift()`, `staleBanner()`.

## The configuration and audit contract (as built on the server)

For the Configure group. The exact shapes are in `internal/api/config.go` and
`docs/design.md` chapter 6.0 of the backend branch; the dev harness mocks them.

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

## The benchmark contract

For the Benchmarks group. Existing endpoints: design 9.3 and `internal/api/bench.go`.
Added by the owner (the dev harness mocks all of it until the server has it):

- A benchmark is `{id, name, description, profile, phases: [{type, ...options}], timeout_s,
  expect, tags}`. Phase types and their options come from `GET /api/bench/schema` ->
  `{phase_types: [{type, label, description, options: [{key, type, default, min, max,
  unit, description, enum}]}], templates: [{id, name, description, phases}],
  benchmark_fields, suite_fields}`; option types are the `schemaForm` types plus
  `requests` (a list of `{name, prompt, max_tokens, stream}`) and `turns` (a list of
  user messages). Types: `load`, `warmup`, `requests`, `conversation`, `hold`,
  `concurrent_requests`, `unload`. A benchmark without `phases` is the simple form.
- Progress: WebSocket `bench` messages with `kind: "progress"` and `GET /api/bench/state`
  carry `run: {id, suite_id, suite_name, state, percent, started_at, elapsed_s, eta_s,
  steps_total, steps_done, current: {index, benchmark_id, name, description, profile,
  model_file, nodes, phase: {index, type, label, of}, percent, detail}, next: {benchmark_id,
  name, description, profile} | null, queue: [benchmark ids]}`. It is in the store as
  `state.bench.run` (null when no run is active); subscribe with `on('bench', fn)`.
  During a load phase `current.detail` is the scheduler's load object (the same
  `load` the Models screen draws: `progress`, `eta_s`, `stalled_s`, `workers: [{addr,
  bytes_total, bytes_sent, state}]`; map `addr` to a node with `nodeByAddr`); during
  request phases it is `{requests_done, requests_total, request, state, tokens,
  elapsed_ms, tokens_per_second, turn, turns_total, in_flight}`.
- Run detail: each step may have `phases: [{index, type, label, state, started_at,
  finished_at, wall_ms}]`; each request has `phase` (a phase type; `measured` in the
  simple form), `phase_index` and, for a conversation, `turn` (with `cache_n` = reused
  prompt tokens).
- Aggregates: `GET /api/bench/aggregates?suite=&benchmark=&profile=&since=&until=&env=`
  -> `{benchmarks: [{benchmark_id, name, profile, model_file, workers, phase_types, runs,
  pass_rate, outcomes, metrics: {load_s | load_mb_s | decode_tps | prefill_tps | ttft_ms |
  memory_mib: {count, min, median, mean, max, stdev, last, best, change_pct, regressions,
  points: [{ts, run_id, value}]}}}], suites: [{suite_id, name, runs, pass_rate,
  mean_duration_s, last_run}], by_model: [{model_file, runs, by_workers: [{workers, runs,
  metrics}]}], by_workers: [...]}`.
- The compact run indicator in the shell: `setShellSlot('run', node)` (`core/shell.js`).
- The one-click run: `confirm({...})` first, saying that it will load and unload
  models on the cluster, then `api.post('/api/bench/suites/<id>/run', {note})`.

## The dev harness

`scripts/ui-dev.sh [scenario]` runs `cmd/devcluster` on `127.0.0.1:8790`: the real
scheduler server, a scripted status per scenario, seeded history, and the mock
of the endpoints above. `GET /dev/scenarios` lists the scenarios;
`POST /dev/scenario?name=<name>` switches. Scenario data for a screen group
goes into its own file in `cmd/devcluster`. Screenshots go to `docs/ui-screens/`
(`<screen>-<scenario>-<desktop|phone>.png`).
