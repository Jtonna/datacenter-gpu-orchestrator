# UI rework: review against the real system and the reference design

The UI was built against a small scripted cluster. On the real one (three
PS5s, 254 profiles of 13 model files, 241 of them generated benchmark
configurations) it did not hold up. This is the record of the rework: what was
wrong in the real screenshots, what the owner's reference design ("Roost
screens") does, what changed, and the pictures.

How the pictures were made:

- `*-before-live.png`: the live UI on the real system, before the rework.
- `reference-*.png`: the owner's reference design, rendered from its source
  files at 1440 px (`reference-models`, `-queue`, `-activity`,
  `-overview-light`, `-confirm`).
- `*-after-real*.png`: the reworked UI on the dev harness replaying a
  recording of that same real system (`scripts/ui-dev.sh real`, GET answers
  only; `real-loading` is the recording at the moment a benchmark loaded
  Gemma 12B). `-phone` is 390 px wide.

The live system was only read (GET). The after pictures are therefore of the
harness, not of the live system itself: see "Not verified" at the end.

## What a model is now

A model is a model **file**. A profile is one configuration of it. The server
has a catalog, `GET /api/models`, with one entry per file; the UI's Models
screen reads it. A profile is **generated** when its `generated` field says so
or, when the field is not set, when its id starts with `bench-`, `b-` or `m-`.
Generated profiles are counted and folded away everywhere except where they
are asked for.

Units: anything read at a glance is GB with one decimal (1 GB = 1,024 MiB),
for memory and file sizes alike. MiB remains in tooltips, in the profile and
settings tables (the setting is in MiB) and in the benchmark figure tables.

## Screen by screen

### Models

- Before: `models-before-live.png`. Reference: `reference-models.png`.
  After: `models-after-real.png`, `models-after-real-loading.png`,
  `models-after-real-phone.png`.
- Wrong: 254 rows of profile ids under the title "Models"; red paragraphs of
  raw refusal text in the table; MiB everywhere; "Not in the history" 250
  times.
- Reference: a card per loaded instance with a stat row and one memory bar
  per node; then a short, calm table.
- Changed: the top is one card per loaded or loading INSTANCE (model name as
  the title, its nodes as chips, the configuration id small on the right;
  the command line folded). Below, "All models": 16 rows (13 files and 3
  command services) with size and quantization, state ("Ready on 2 PS5s"),
  what it needs in GB and whether that is measured, on how many PS5s it runs,
  how many configurations it has ("28 · 4 by hand"), the last load time and
  speed, last used. Search and four filters (loaded, fits on one PS5, needs
  several, cannot run). A model that cannot be placed says so in a few words
  ("No node can run it"); the detail is one click away.

### A model's page (new)

- After: `model-detail-after-real.png`, `model-detail-refused-after-real.png`,
  `model-detail-after-real-phone.png`.
- Clicking a model opens `#/models/<file>`: header with name, size,
  quantization; what it needs and on how many PS5s; the file; the last
  refusal in one plain sentence (the scheduler's words behind a button);
  "Loaded now" with the instance cards; Usage (requests served, last used,
  one row per load with its load time, upload speed, how long it stayed and
  how it ended); Performance on 1, 2 and 3 PS5s (memory, load time, upload
  speed, answer speed, "not measured yet" where there is nothing);
  Configurations (the hand-made ones, the generated ones behind a toggle; for
  each whether it would start now, in plain words, with links to Placement
  preview and the editor); Benchmarks that use it. No load or unload control.

### Nodes

- Before: `nodes-before-live.png`. Reference: the node rows of
  `reference-models.png` (solid segments on one track).
  After: `nodes-after-real.png`, `nodes-after-real-loading.png`,
  `nodes-after-real-phone.png`.
- Wrong: an outlined box floating over a grey stub; "-434 in use"; the
  64-character worker hash running out of the card and under its neighbour;
  cards of different heights; a footer of loose mono words.
- Changed: one track per node: models (mint), other use (grey), the rest is
  free, and one white marker line where a new model must stop. "In use" is
  never negative: when the free figure is above the reported total it is
  clamped and a note says which two figures disagree. The worker version is
  12 characters with the full value in the tooltip and a Copy button. The
  three figures sit on an aligned grid, the footer is aligned label / value
  pairs, cards in a row have one height, excluded machines are quieter and
  show only what applies to them.

### Queue

- Before: `queue-before-live.png` (taken before the first status arrived).
  Reference: `reference-queue.png`. After: `queue-after-real.png`.
- Wrong with real data: "Forced nodes do not fit." and the refusal paragraph.
- Changed: each refusal is "It does not fit on the nodes it is tied to." with
  one plain line under it ("Needs about 14.3 GB; ps5-9032a330 can give
  12.1 GB."); the raw text is the tooltip.

### Activity

- Before: `activity-before-live.png`. Reference: `reference-activity.png`.
  After: `activity-after-real.png`, `activity-after-real-phone.png`.
- Wrong: refusal events of fourteen lines; every sentence repeated the node
  names that follow as chips; MiB; "(api)"; the Took and MB/s columns of the
  side table were cut off by the card.
- Changed: every common event has one short sentence ("bench-qwen4b-1a-2048
  is ready: loaded in 25 s at 113 MB/s."); the stored record is behind
  "Details"; the side tables clip the name, never the figures.

### Placement preview

- Before: `placement-before-live.png`. After: `placement-after-real.png`.
- Wrong: 254 profiles in the list; the raw reason printed in full.
- Changed: search first; the 241 generated profiles behind a toggle; figures
  in GB; "Why: Needs about 28.5 GB; its 2 nodes together can give 24.2 GB."
  with the scheduler's words behind a button.

### Profiles

- Before: `profiles-before-live.png`. After: `profiles-after-real.png`,
  `profiles-search-after-real.png`, `profiles-after-real-phone.png`.
- Wrong: 254 rows, each three lines high, three buttons per row.
- Changed: grouped by model file (the file links to the model's page); the
  hand-made profiles show, the generated ones are a count with a toggle per
  model; search and filters (made by hand, generated, loaded now) open the
  groups; one Edit button, Copy and Delete as quiet text.

### Benchmarks

- Before: `benchmarks-before-live.png`. After: `benchmarks-after-real.png`,
  `benchmarks-run-after-real.png`, `benchmarks-aggregates-after-real.png`.
- These screens held up. Changed: the column that showed a pass rate was
  called "Last result"; it is "Passed" now.

### Tiers and nodes, Settings, Audit, About

- Before: `tiers-before-live.png`, `settings-before-live.png`,
  `audit-before-live.png`, `about-before-live.png`. After: the same names
  with `-after-real`.
- Settings: durations read "15m" instead of "15m0s"; numbers in the
  "Default" line have thousands separators.
- Audit: "Run_start suite smoke" is "Started a run of suite smoke";
  "Unloaded model *" is "Unloaded every model"; the target is no longer
  printed twice.
- Tiers and About: unchanged; they read correctly with the real data.

## Reason strings

`core/reasons.js` maps every reason the recording contains (the plans of 254
profiles and the refused requests of the history) and the formats in
`internal/scheduler/why.go`, `forcedfit.go` and `internal/placer`. Examples:

| The scheduler says | The page says |
|---|---|
| `profile …: need 19452 MiB (estimated, +5% margin; the estimate is 18525 MiB), but the nodes it is forced onto have only 12370 MiB usable in total (…) (defaults.forced_fit_check is reject)` | Needs about 19.0 GB; ps5-9032a330 can give 12.1 GB. |
| `… split over the forced nodes, node ps5-407c8925: expected share 12724 MiB (need 32541 MiB x 34% of the split x 1.15 skew margin) but usable 12234 MiB (…)` | Needs about 31.8 GB split over its nodes. The share for ps5-407c8925 would be 12.4 GB with the safety margin, and it has 11.9 GB free. |
| `placer: need 36885 MiB (requires 44540 MiB) exceeds cluster capacity 38442 MiB; nodes excluded by config and not counted: alienware-llm, omen-runner` | Needs 36.0 GB (43.5 GB with the safety margins); the whole cluster holds 37.5 GB. Not counted, because excluded: alienware-llm and omen-runner. |
| `node alienware-llm is excluded by config; node omen-runner is excluded by config` | The nodes that could take it (alienware-llm and omen-runner) are excluded from use. |
| `placer: need 7500 MiB (requires 7500 MiB) exceeds cluster capacity 0 MiB; nodes excluded by config and not counted: omen-runner` | No node can run it: omen-runner is excluded from use. |

## The three passes

1. Looked at the eleven live screenshots and the five reference renders;
   built the catalog, the Models home, the model page and the Nodes card;
   compared on the recording.
2. Went through every other screen on the recording: Profiles, Placement,
   Activity, Queue, Audit, Settings, Benchmarks; then the scripted states
   (demo, twins, loading, stalled) and 390 px.
3. Read the final pictures as the owner would. Found and fixed: a node that
   is loading showed "0.0 GB in use" (the free figure had been clamped
   without counting the model's own memory); "No free node is big enough"
   was shown for models that were only waiting for a busy node; the catalog's
   name column collapsed at phone width.

## Still weak

- Model names come from the file name on the recording ("gemma-4-12b-it"),
  because the recorder cannot read the files. On the real server the catalog
  reads `general.name` from the GGUF header; that path is tested with a
  written file, not seen with the real models.
- "Runs on N PS5s" is the smallest node count a load was measured on. For a
  model that was only ever loaded on three, it says three even if two might
  do; the estimate ("should need") is only used when nothing was measured.
- The Performance block has no answer speed for most models: only one
  benchmark measures decoding so far.
- Settings is still a long page of fields; it was tidied, not redesigned.
- The older pictures in `docs/ui-screens/*.png` (the scripted scenarios) were
  not retaken; Models, Nodes, Profiles, Placement, Activity and Queue look
  different now.

## Not verified

- The reworked UI was not seen on the live system: it is not deployed there
  (this branch was not pushed or installed), and the live system was only
  read. `GET /api/models` therefore ran against the recording and the test
  fixtures, never against the real models directory.
- The recording is one moment (idle) plus one status during a load. A real
  ready instance with requests in flight was seen only in the scripted
  scenarios.
