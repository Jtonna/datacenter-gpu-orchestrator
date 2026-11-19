# UI screenshot review

Screenshots are taken with `scripts/ui-screens.sh` against the dev harness
(`scripts/ui-dev.sh`). File names: `<screen>-<scenario>-<desktop|phone>.png`
(desktop 1440 px wide, phone 390 px wide; the height is chosen to show the
whole page). Each pass looked at every picture and compared it with the intent
of the reference design ("Roost screens"): alignment, spacing, overflow,
truncated text, contrast, empty and broken states, phone layout, numbers that
line up.

## Method notes (found while reviewing)

- Desktop Chrome does not lay a window out narrower than about 500 px, so the
  first phone pictures were a 504 px layout cropped to 390 px (text cut off at
  the right edge, which looked like an overflow bug and was not). Phone
  pictures now go through the harness page `/dev/frame`, which shows the UI in
  a frame of the exact width.
- A picture taken through `--timeout` sometimes showed only the loading
  placeholders. The script now uses `--virtual-time-budget`, and the views draw
  on a microtask instead of a timer (a background tab throttles timers; a
  monitoring page must not fall behind because of that).

## Pass 1 (Monitor group, Placement preview, About)

| Screen | Found | Changed |
|---|---|---|
| Shell | Renders as designed. Mono text inside bold runs was drawn with a faked bold (only weights 400 and 500 are bundled). | `.mono` is 400, at most 500 inside headings and links. |
| Models, loading | Step tracker, overall percent, time left, deadline and per-worker rows read well. | None. |
| Models, split / twins | Memory rows line up; the model file sits at the right of the header. | None. |
| Models, phone | Command and log file took a 120 px label column; profile ids wrapped inside the scrolling table. | Fact lists stack on a phone; profile ids do not wrap; tighter card padding under 700 px. |
| Models, not loaded | "Where it would go" repeated internal strings ("placer: need 12100 MiB (requires 12100 MiB) does not fit the available nodes", "forced nodes in use: ...", "Joins the queued entry (1 waiting, claimed false)"). | `plainReason()` rewrites the known wordings in plain words ("Needs 12,100 MiB; the nodes that are free now do not have that much."); the scheduler's own text stays in the tooltip. An unknown wording is only tidied, never guessed at. |
| Nodes | The priority label wrapped to a second header line on a tile with two badges. | Priority moved to the tile footer with the address and the id. |
| Queue | Three identical lines for three nodes holding one model; durations in bold mono looked heavy; the badge column stretched; a claimed entry repeated its victim as "node is busy". | Nodes holding the same model are one line; durations are in the text face; badges align right; a victim is listed once. |
| Queue | "Placer: need ..." as the headline of a waiting request. | Headline uses `plainReason()`; raw text in the tooltip. |
| Activity | "1 h 33 min ago" wrapped in the time column; a "Show the record" link under every row was noisy; the second line repeated the first ("took 6.1 s"); the loads table was cut off in the side column. | Wider time column in the text face; a small "Record" toggle at the right of the row; the second line only adds what the sentence lacks; loads table shows seconds, the node count as a second line, and fits. |
| Placement preview | Profile ids wrapped in the list. | List is wider; ids are cut with an ellipsis and a tooltip. A refusal shows the plain reason and, under it, what the scheduler said. |
| About | "Load memory floor MiB 768 MiB"; values in mono looked like code. | The unit is dropped from the label; values are in the text face. |

## Pass 2 (same screens, after the fixes; plus empty, stale, refused, bare)

| Screen | Found | Changed |
|---|---|---|
| Models, bare (no configuration) | The "no configuration yet" banner and "No profiles yet" read well. | None. |
| Models, refused | The failed-load banner states what failed, when, and why, with a link to the events of that load. | None. |
| Models, twins | Two cards for one model file, each naming its node and the other profile. `stale_profile` shows "Older definition". | None. |
| Activity, history off | The time range and Download were offered although there is nothing to read; the text named a YAML key that is now a start flag. | The header drops the range and the download; the text names `--history-dir`. |
| Queue, phone | The "why" column of the refused table is reached by scrolling the table sideways; acceptable, the first three columns identify the row. | None. |
| Reconnect | Killing the dev server shows "Reconnecting… Last update 8 s ago" and the warning banner within 4 s; after a restart the page is live again in 2 s and re-reads the profile list. | None. |

## Pass 3: the whole product on the real backend

The three builders' branches and the backend branch are merged, and the dev
harness now runs the UI against the real server code: the real configuration
database (seeded through the real import path), the real audit log (entries
made through the real handlers from the web UI, REST and MCP), the real
benchmark store, manager and progress model. Only the fleet and the llama
processes are fakes. The scripted status scenarios and the frozen benchmark
progress frames are dev-only injections, because the fakes cannot reach those
states on this machine. The builders' own review logs are
`REVIEW-benchmarks.md` and `REVIEW-configure.md` in this folder.

Every screen was taken again at 1440 px and 390 px and looked at. Behaviour
was driven in a headless Chrome through the DevTools protocol.

### Mismatches between a view and the real responses, and what was fixed

| Where | Found | Fix |
|---|---|---|
| Benchmark editor (new, edit) | Stayed on its loading placeholder for ever. The editor is a hand-managed node (`data-keep`); `morph()` merged it into a look-alike `div` that was already on screen instead of inserting it. The old mock harness hid it by timing. | `core/dom.js`: a `data-keep` node only matches itself, or a placeholder with the same explicit key. |
| Shell run indicator | The shell wrapped the slot node in a new wrapper on every draw, so the indicator vanished; the builder had a workaround. On a phone it was hidden in the closed drawer. | `core/shell.js` keeps one slot box per layout and never re-creates the node; at phone width the box sits under the top bar. Workaround removed. |
| Audit | The real server writes benchmark entries with section `bench` and targets `suite:<id>`, `benchmark:<id>`; the sentence read "Changed benchmark suite:side-by-side". Audit ids are numbers, not strings. | `entrySentence()` reads the target prefix ("Changed suite side-by-side"); nothing depended on the id type. |
| Audit, live | Loads, unloads and benchmark edits are audited but not announced on the `config` topic. | The trail is read again when the live channel hints at one (a load started, an unload asked through the API, a benchmark run changed), after a reconnect and when the page becomes visible. Not on a timer. |
| Aggregates trend chart | Text scaled with the card; y labels with a unit were cut. | `lineChart` is drawn for a fixed pixel width; the left margin fits the longest label. |
| Forms from the schema | "Changed from default" on every field whose default is null; "at least 1 tokens"; "Min share MiB [MiB]". | `schemaForm`: a null default is no default; ranges carry no unit; a label does not repeat the unit the control shows. |
| Profiles | Raw scheduler reasons in "where it would go". | `plainReason()`, raw text in the tooltip; the "changed from default" mark is off for profile fields (a profile has no default). |
| Suites table | "Not scheduled" wrapped onto two lines. | The column does not wrap. |
| Wording | Models said "No configuration yet", Profiles "No configuration has been imported yet". | One sentence on both. |

Nothing was found wrong in the backend. Two things in the seed data of the
harness were wrong and reported to its builder (a conversation benchmark whose
expectation failed in every seeded run, giving 0% pass rates; frozen benchmark
scenarios that changed the suites list).

### States nobody had seen rendered

| State | Picture | Result |
|---|---|---|
| Live panel, conversation phase | `benchmarks-live-conversation-desktop.png` | Turn 3 of 5, tokens of the turn, phase 2 of 3. Good. |
| Live panel, hold phase | `benchmarks-live-hold-desktop.png` | "Holding the model idle: 8 of 900 s", phase 5 of 7. Good. |
| Live panel, simultaneous requests | `benchmarks-live-concurrent-desktop.png`, `-phone.png` | "1 of 4 done". Good. |
| Live panel, parallel step | `benchmarks-live-parallel-desktop.png` | Both profiles joined with "+", both model files, the load of the member in flight. Good. |
| Live panel, load-only at 40% | `benchmarks-live-load-desktop.png`, `-phone.png` | Overall percent, benchmark percent derived from the model load, per-worker done / receiving / waiting, next benchmark, queue. Good. |
| A real run, started with one click | `benchmarks-run-confirm-desktop.png`, `benchmarks-live-real-1-desktop.png`, `-2-` | The confirm dialog names the benchmarks, profiles and nodes and says it loads and unloads models; the run played through load, requests and unload on the real manager and showed up in Runs as completed. |
| Run detail, every phase type | `benchmarks-run-every-phase-desktop.png` | Load, warm-up, requests, conversation turns with reused prompt tokens, hold, simultaneous requests, unload, and the environment of the run. Good. |
| Run detail, parallel step and load-only | `benchmarks-run-parallel-desktop.png`, `benchmarks-run-load-only-desktop.png` | Good. |
| Editing a simple-form benchmark | `benchmarks-editor-simple-form-desktop.png` | Shows what it will do and offers "Convert to phases". Good. |
| Editing a parallel step | `benchmarks-editor-parallel-desktop.png` | Member list with add and remove. Good. |
| Configure dialogs on a phone | `settings-review-phone.png`, `settings-pending-phone.png` | The dialog is 358 px wide inside a 390 px page; no sideways scroll. |
| Settings, Profiles, Tiers, Audit on a phone | `settings-demo-phone.png`, `profiles-edit-phone.png`, `tiers-demo-phone.png`, `audit-demo-phone.png` | One column, no sideways scroll. |
| Audit live prepend | `audit-live-desktop.png` | A REST change by "curl" appeared at the top within 2 s while the page was open. |
| Rollback | `audit-rollback-confirm-desktop.png`, `audit-revisions-after-rollback-desktop.png` | The dialog shows the dry-run diff and the keys that need a restart; it wrote revision 15 on top of 14. |
| Changed by someone else while editing, and the 409 | `settings-changed-elsewhere-desktop.png`, `settings-conflict-desktop.png` | The notice names who changed what; saving shows what changed since and offers to apply again. |
| Save while disconnected | `settings-save-disconnected-desktop.png` | "The scheduler did not answer. Nothing was changed." The staged change is kept. |
| No configuration yet, every screen | `*-bare-desktop.png`, `models-bare-phone.png`, `profiles-bare-phone.png` | Each screen says what is missing and what to do; nothing is broken or blank. |
| History off | `activity-bare-desktop.png` | Explained; the time range and the download are not offered. |
| Reconnect | `models-reconnecting-desktop.png` | "Reconnecting…" and a banner within 4 s of the server dying; live again 2 s after it is back. |

### One product

Checked across the three builders' screens: page header (title, count line,
one primary button at most), cards with the same header and padding, badges
(mint = fine, amber = in progress or changed, coral = wrong, lilac = waiting,
grey = inactive), node names as chips, numbers and ids in mono and never
bolder than 500, tables inside a scrolling card, the same empty-state pattern
(what is missing, why, what to do), one confirm dialog (consequences listed,
the button names the action), plain reasons with the scheduler's words in a
tooltip. The local helpers the builders wrote were moved into the shared set
(`segments`, `changeMark`, `pickCards`, `subBlock`, `orderButtons`,
`core/changes.js`, `core/configdata.js`).

Left as is, on purpose:
- On a phone the Run button of a suite is reached by scrolling the suites
  table sideways.
- Durations in right-aligned table cells are set in mono like the other
  figures ("2 min 58 s").
- The seeded audit entries of the harness are all a few seconds old, because
  they are real writes made when the harness starts.

### The owner's requirements, one by one

| Requirement | Result |
|---|---|
| Model-centric home: one card per loaded or loading instance, stating its nodes | Yes. `models-demo-desktop.png`, `models-split-desktop.png`. |
| Two instances of one model are two cards | Yes. `models-twins-desktop.png`: `devstral-24b` on `ps5-9032a330` and `bench-devstral-24b` on `ps5-31bd9dc4`, each naming the other. |
| No load or unload control anywhere | Yes. No such button exists; `TestUIHasNoLoadOrUnloadControl` fails the build if a UI file names `/api/load`, `/api/unload` or `/api/models/unload`, and it passes. Running a suite is the one action that causes loads; it is behind a confirm that says so. |
| Benchmarks configurable in the UI, one click to run | Yes. Created "Qwen 4B load only" from the "Load only" template and ran "Smoke test" with one click plus the confirm, on the real backend. |
| The running-benchmark panel: overall percent, the current benchmark with its description and its own percent, the next one, load-derived progress for a load benchmark | Yes. `benchmarks-live-load-desktop.png`; the compact indicator shows on every screen (`models-during-benchmark-desktop.png`). |
| Run history that aggregates | Yes. Runs, run detail with baseline comparison and regression marks, exports, and Aggregates computed by the real server (`benchmarks-aggregates-desktop.png`). |
| Every configuration item editable, with a dry-run review | Yes. Profiles, tiers, exclusions, node overrides, settings, recovery and fleet; each save goes through the review dialog (dry run, diff, comment, `If-Match`). |
| Audit trail with source and actor | Yes. Web UI, REST, MCP and import entries with the actor, a diff per entry, filters, revisions and rollback. Every write from the page carries `X-Source: ui` and `X-Actor`. |
| Live data over the WebSocket only | Yes, by code search: the only two intervals are the one-second clock tick of the store and the watchdog of the live channel; every REST call is made on open, on a user action, or after a pushed message. `TestUIHasNoPollingTimer` enforces it. |
| Self-contained | Yes. Fonts are bundled; `TestUIHasNoExternalReference` and `TestUIImportsResolve` pass. |
| Usable at phone width | Yes. Navigation is a drawer, tables scroll inside their card, dialogs fit. |
