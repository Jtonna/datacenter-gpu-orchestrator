# Benchmark guide

How to design, run and read benchmarks in Potluck. The scheduler runs them itself, through its own HTTP API, so a benchmark loads and serves exactly like a client request and never skips a fit check. Every result is kept in the scheduler database.

The same operations are available three ways: the Benchmarks page of the web UI, the REST API under `/api/bench/`, and MCP tools. Examples use `B=http://<host>:3122`.

## 1. Words

- **Benchmark**: one test case on one profile: a list of phases, for example load, a few requests, unload.
- **Suite**: an ordered list of benchmarks, run together. Only suites are run.
- **Run**: one execution of a suite. Each benchmark in it becomes a **step** with its results.
- **Baseline**: the run a new run is compared with. By default, the previous completed run of the same suite.

Benchmarks need the database and the `bench_enabled` setting (on by default). Without them `/api/bench/...` answers `503`.

## 2. Design a benchmark

Ask `GET $B/api/bench/schema` (MCP `get_bench_schema`) first. It lists every phase type, every option with its default, limit and unit, the `expect` keys, the suite options, and six templates: "Load only", "Load and requests", "Load and conversation", "Requests on a loaded model", "Simultaneous requests", "Cold, reload, loaded".

### Phases

| Phase | What it does | Measures |
|---|---|---|
| `load` | Unloads the profile first, then loads it with `POST /api/load`. No chat request is sent. With `cold: true` it drops the model file from the file caches first (see Cold loads). | Load time, bytes and MB/s per node, memory, lowest available memory, load mode, the `cold` block |
| `warmup` | Requests left out of the statistics. | Recorded, not counted |
| `requests` | The measured requests, each repeated `repeats` times. | Prefill and decode tokens/s, time to first token, output hash |
| `conversation` | A multi-turn exchange; each turn carries the earlier turns and replies. | Per turn: prefill, decode, time to first token, reused prompt tokens |
| `hold` | Stays loaded and idle for `seconds`, then sends one request. | Whether the model answers as fast after idling |
| `concurrent_requests` | `n` requests at the same moment. | Each request's wait and decode speed, total time, errors |
| `tasks` | Sends each task of a fixed task set once, in order, and keeps every answer (see Tasks). Never runs or scores an answer. | Per task: answer, finish reason, tokens, time to first token, tok/s, wall time, error; scores posted later |
| `unload` | Unloads the profile. | Time until the instance is gone |

A phase that sends requests with no `load` before it loads the model if needed and leaves it loaded for the next benchmark; the end of the run unloads it. So a benchmark with only a `requests` phase means "requests on a model that is already loaded". Hold seconds must fit in `timeout_s`.

The simple form (`cold`, `warmup_requests`, `repeats`, `requests`, `unload_after`) still works: it is expanded to `load`, `warmup`, `requests`, `unload`. Write `phases` for anything else.

A benchmark with `parallel: [id, id]` starts the listed benchmarks at the same moment, to measure loads that compete.

### Cold loads

A `load` phase normally finds the model file partly in the nodes' file caches, left there by an earlier load, so a reload from the cache and a load from disk or the share look alike. With `"cold": true` the phase first drops the model file from the file cache of every node the plan names and of the scheduler's machine (which serves the file to the nodes), then loads. The phase result keeps a `cold` block:

- `verified: true` only when every one of those machines ended with under 1 % of the file cached, and the load then used only the nodes whose cache was dropped.
- Otherwise `verified: false` and `reason` starts with "not guaranteed:" and names each cause: a node whose agent cannot drop files from the cache (an older release, or the push model source), a node that refused or was still busy, a machine that kept more than 1 %, or a load placed on a node the plan did not name. The load still runs; only the label changes.
- `local` and `nodes[]` give each machine's `file_bytes`, `cached_before`, `cached_after`, or its `error`.

The Benchmarks page shows a "cold: verified" or "cold: not guaranteed" badge on the phase, with the reason on hover, and a table of each machine's cache before and after.

`cold` is an option of a load phase like any other, so any benchmark can use it. A step with a cold load phase also shows a row with each load phase under its own label (its seconds and the time to first token after it) and each requests phase's decode speed.

The template "Cold, reload, loaded" is a starting point, an ordinary benchmark you can edit: a cold load, the first answer, three measured requests on the loaded model, an unload, a reload from the file cache, and three requests after it. Copy it from the schema, put your profile in, and save it:

```sh
curl -s $B/api/bench/schema | jq '.templates[] | select(.id=="cold-reload-loaded") | .benchmark | .id="gemma12b-cold" | .profile="gemma-3-12b"' | curl -s -X POST $B/api/bench/benchmarks -d @-
```

### Tasks (from release 0.1.8)

A `tasks` phase measures what a model answers, not only how fast: it sends a fixed set of tasks (first use: coding tasks) to the loaded profile and stores every answer so that a scorer can judge them later. **Potluck never runs what a model wrote and never scores it.** Scoring is done outside (tests in a sandbox, a rubric, a person) and posted back.

**Task sets are data.** A task set is a small versioned document:

```json
{"format": "potluck-tasks/1", "id": "coding", "version": 1, "title": "Coding tasks",
 "system": "You are an expert Python programmer.", "max_tokens": 4096, "seed": 1, "temperature": 0,
 "tasks": [{"id": "t1", "title": "parse_duration", "prompt": "...", "max_tokens": 8192, "timeout_s": 1200},
           {"id": "t2", "prompt": "...", "system": "a system prompt for this task only"}]}
```

`id` follows the benchmark id rule, `version` starts at 1, at most 64 tasks with unique ids, a prompt of at most 1 MiB. Sampling is fixed: temperature 0 (the only value accepted), the set's `seed` (default 1), streamed, no prompt cache. Thinking is not set: it follows the profile, and the phase result says whether a reasoning text was seen. A task's `timeout_s` ends that task only.

Store a set with `POST $B/api/bench/task-sets` (MCP `create_task_set`). A stored version never changes: the same content again answers 200 (`created: false`), other prompts under the same version answer 409, so a changed set gets a new version. The answer carries `sha256`, a hash of what the model is asked (format, seed, and per task the id, effective system prompt, prompt and max_tokens; titles and timeouts are left out). Results with the same hash were asked the same thing and can be compared.

**The phase** names a stored set, `{"type": "tasks", "task_set": "coding"}` (the newest version) or with `"task_set_version": 2`, or carries the set inline as `"tasks": {...}` (exactly one of the two). `timeout_s` is the phase's own limit; it must leave 30 s of the benchmark's `timeout_s`. A benchmark that names a set that is not stored is refused (422). Example:

```sh
curl -s -X POST $B/api/bench/task-sets -d @coding-tasks.json
curl -s -X POST $B/api/bench/benchmarks -d '{"id":"gemma12b-coding","profile":"gemma-3-12b","timeout_s":10800,
  "phases":[{"type":"load"},{"type":"tasks","task_set":"coding","timeout_s":9000},{"type":"unload"}]}'
```

Each task is sent once, in order. A task that fails (not 200) or runs out of time is recorded (`failed`, `timed_out` with whatever had arrived) and the next one is sent; when the phase's time or the step's runs out, or the run is cancelled, the rest are recorded as `not_sent`. A failed task does not fail the step. The phase result (`phases[].tasks`) has the set's id, version and hash, `total`, `sent`, `answered`, `failed` (timed-out tasks included), `timed_out`, `not_sent`, `phase_timed_out`, `thinking` (`seen` or `not_seen`), `decode_tps` and `ttft_ms` summaries over the answered tasks, and `items[]` per task: `result_id`, `task_id`, `state`, `status`, `finish_reason` (`length` means it hit max_tokens), `prompt_tokens`, `completion_tokens`, `ttft_ms`, `decode_tps`, `wall_ms`, `answer_chars`, `reasoning_chars`, `answer_sha256`, `error`. The answer texts are not in the run: they are stored apart (up to 4 MiB each) and kept as long as the run.

| Ask | REST | MCP |
|---|---|---|
| The stored sets (no prompts) | `GET /api/bench/task-sets` | `list_task_sets` |
| One set with its prompts | `GET /api/bench/task-sets/<id>?version=N` | `get_task_set` |
| Remove a version (409 while answers use it) | `DELETE /api/bench/task-sets/<id>?version=N` | |
| A run's task results, with scores | `GET /api/bench/runs/<id>/tasks` (`?step=N`, `?answers=1` for the texts) | `get_run_tasks` |
| One answer | `GET /api/bench/runs/<id>/tasks/<result_id>` (`?format=text` or `reasoning` for the bare text) | `get_task_answer` |
| Each scorer's total | `GET /api/bench/runs/<id>/scores` | |
| Post a scorer's scores | `PUT /api/bench/runs/<id>/scores/<scorer>` | `put_task_scores` |
| Remove a scorer's scores | `DELETE /api/bench/runs/<id>/scores/<scorer>` | |

**Scores.** A scorer posts every score it has for a run at once; the post replaces whatever that scorer posted on the run before, so posting again is safe. Several scorers live side by side (for example `kit-tests` and `t5-rubric`). The body:

```json
{"scorer_version": "score.py strict", "scores": [
  {"step": 0, "task": "t1", "points": 17, "max_points": 17},
  {"result": 42, "points": 9, "max_points": 17, "failed_tests": ["test_edge"], "notes": "missed the empty input"}]}
```

A score names its task by `result` (the `result_id`), or by `step` and `task` (and `phase` when the step has two tasks phases). Points are 0 to `max_points`, and `max_points` must be more than 0. A score that names nothing in the run, or a task scored twice, refuses the whole post (422). The run then shows, per tasks phase, each scorer's `points`, `max_points`, `tasks_scored` of `tasks`, and each item's `scores` by scorer. Changes are in the audit log (target `scores:<run>/<scorer>`, and `task_set:<id>@<version>`).

**With the coding kit.** `scripts/bench-tasks.py` (Python 3, standard library) links a run to a scoring kit: `taskset KIT --post $B` builds and stores a set from the kit's `system_prompt.txt` and `tasks/<id>/{meta.json,prompt.md}`; `pull $B <run> --out DIR` writes the answers in the kit's layout (`DIR/<profile>-step<N>/<task>.json` and `<task>.answer.md`, with the result ids); after the kit's `score.py --out DIR`, `scores DIR --scorer kit-tests [--rule lenient] [--max-points 17] --post $B` posts the tests passed; `post $B <run> <scorer> scores.json` sends a ready body, such as a rubric's.

### Expectations

`expect` turns a benchmark into a pass or fail check: `outcome` (`ready`, or `refused` for a model that is supposed not to fit), `max_load_s`, `max_unload_s`, `min_decode_tps`, `min_prefill_tps`, `max_ttft_ms`, `min_reused_tokens`, `max_errors`, `max_concurrent_wall_s`. A step that was refused or failed is a result with the scheduler's reason, not an error. One outcome is never judged: `refused_by_exclusion`, for a profile that forces a node `exclude_nodes` names. Its result is `not_judged`; it is neither a pass nor a failure and stays out of the pass rates (from release 0.1.7). An ordinary `refused` is judged like any outcome. So a benchmark that expects `outcome: refused` on a profile that forces an excluded node is not judged either, rather than passing. A benchmark or suite with nothing judged has no pass rate (`pass_rate: null`, `judged: 0`).

### Choosing profiles

A benchmark names a profile. A profile with `force_nodes` pins the benchmark to chosen nodes, which makes runs comparable. A forced profile is still checked against the nodes' memory, so a test of a model that must not fit uses `expect: {"outcome": "refused"}`. For good results on automatic profiles, give the profile a measured `needs_mib` (runbook section G).

### Recipes: one benchmark for every model

A benchmark with `inputs` instead of `profile` is a recipe. It says what to do (its phases) and leaves open which model, how much context and where. The profile is made from the inputs just before each step, so one recipe serves every model and every placement.

| Input | Meaning |
|---|---|
| `model` | The model file under the models directory |
| `ctx` | Context size per slot (256 to 1048576) |
| `placement` | Consoles only: `auto` (Potluck chooses, the default), `all` (every console that can take a load), `any:N` (N consoles, those with the most available memory, ties by name), `nodes:a,b` (these consoles, by name or id) |
| `thinking` | `false` turns reasoning off (`--reasoning-budget 0`); leave it out for the model's default |
| `kv_quant`, `flash_attn` | As in a profile |

The recipe's own `inputs` are defaults; a group entry or a direct run fills in or overrides them. `model` and `ctx` must be known by then. A console is an agent node that is active, not excluded and not the scheduler's own machine. A placement that cannot be met when the step starts (a named console paused, fewer than N consoles free) is a refused step with the reason, not an error, and no profile is made for it.

The profile is named after the inputs, for example `bench-gemma-3-12b-q4-any2-8k-nothink`, and is marked `generated: true`. It is created when missing, rewritten when the placement now picks other consoles, and left alone when it is already right. A profile of that id that a person wrote (`generated: false`) is never replaced; the step fails and says so. Each use of a recipe is a step with its own id, `<recipe>.<6 hex digits of the inputs>`, the same in every run, and a name such as `Cold load (gemma.gguf, 8k, any:2, thinking off)`. The step's `spec` keeps `recipe` and `applied_inputs`.

Members of a parallel benchmark still name fixed profiles.

### Groups

A suite is also called a group; `/api/bench/groups` takes the same requests as `/api/bench/suites` and old suites read unchanged. An entry of a group is one of:

- a benchmark id, or an inline benchmark, as before;
- `{"benchmark": "<recipe>", "inputs": {...}}`: one use of a recipe;
- `{"benchmark": "<recipe>", "inputs": {...}, "matrix": {"model": [...], "placement": [...], "ctx": [...], "thinking": [...]}}`: one use per combination, model outermost (up to 64 values per list and 500 combinations);
- `{"group": "<id>"}`: another group's entries, run in its place. Its own run options (cooldown, idle wait) are not used; the outer group's are. A group that contains itself, directly or through others, is refused, and so is nesting deeper than 8.

A run expands the group when it is queued (at most 1000 steps) and keeps the expanded list. `GET /api/bench/groups/<id>/expand` (MCP `expand_group`) shows that list without running anything: each entry's id, name, profile, recipe and inputs. A group that another group names cannot be deleted (`409`, naming the groups).

```sh
curl -s -X POST $B/api/bench/groups -d '{"id":"cold-loads","benchmarks":[{"benchmark":"cold-load","matrix":{"model":["gemma-3-12b-q4.gguf","qwen3-8b-q4.gguf"],"placement":["any:1","any:2","all"]}}]}'
```

## 3. Create benchmarks and suites

REST: `POST $B/api/bench/benchmarks` and `POST $B/api/bench/suites` with the JSON object; `PUT` and `DELETE` on `/api/bench/benchmarks/{id}` and `/api/bench/suites/{id}`. MCP: `create_benchmark`, `update_benchmark`, `delete_benchmark`, `create_suite`, `update_suite`, `delete_suite`. A first install can also seed them from the `bench:` block of the import file (see `deploy/seed-config.yaml`).

A load-only benchmark, the cleanest way to compare nodes or links:

```sh
curl -s -X POST $B/api/bench/benchmarks -d '{"id":"gemma12b-load-1","profile":"gemma-3-12b","phases":[{"type":"load"},{"type":"unload"}],"expect":{"max_load_s":120}}'
```

A suite of benchmarks, 10 s apart:

```sh
curl -s -X POST $B/api/bench/suites -d '{"id":"gemma-scaling","benchmarks":["gemma12b-load-1"],"cooldown_s":10}'
```

Suite options: `cooldown_s` (pause between benchmarks), `stop_on_failure`, `only_when_idle` (default true), `idle_wait_s` (default 600), `evict_others` (default false: see Run), `schedule` (`{"at":"03:00"}` in the container's local time, normally UTC, or `{"every":"24h"}`).

A scenario that cannot run yet is a suite with `status` `planned` or `blocked` and a `reason`. It may list no benchmark, its Run button is off, and running it answers `409` with the reason. Remove `status` and `reason` when it can run.

Unknown profiles, bad ids and unknown fields are refused with `400` or `422` and the reason. Deleting a profile that a benchmark names answers `409` unless forced.

## 4. Run

```sh
curl -s -X POST $B/api/bench/suites/gemma-scaling/run -d '{"note":"after the firmware update"}'
```

MCP: `run_suite` with `id`, optional `baseline`, `note` and `evict_others`. The answer is the queued run. Runs go one at a time; others queue.

**One benchmark on its own.** A benchmark can also be run without a suite:

```sh
curl -s -X POST $B/api/bench/benchmarks/gemma12b-load-1/run -d '{"note":"after the firmware update"}'
```

MCP: `run_benchmark` with `id`, optional `baseline`, `note`, `only_when_idle` (default true), `idle_wait_s` (default 600), `evict_others` (default false) and, for a recipe, `inputs` (for example `{"inputs":{"model":"gemma-3-12b-q4.gguf","placement":"any:2"}}`). A recipe without a model, or inputs on a benchmark with a fixed profile, is refused with `422`. It is an ordinary run with one step and `kind: "benchmark"`, `benchmark_id` naming the benchmark, an empty `suite_id` and the benchmark's title as `suite_name`. Its default baseline is the benchmark's previous completed direct run. Every other run has `kind: "suite"` (older runs read the same way). `GET /api/bench/runs?kind=benchmark&benchmark=<id>` lists the direct runs; `?benchmark=<id>` alone lists every run with a step of it. The suite aggregates leave direct runs out; the benchmark aggregates count them.

- **Idle first.** Benchmarks load and unload real models. With `only_when_idle` a run does not start, and pauses between benchmarks, while another model is loaded, a request that is not the run's is in flight, or anything is queued. A model someone used stays loaded for its `ttl` (300 s by default), so a run may wait that long. After `idle_wait_s` the run ends `skipped` and keeps what it measured. Set `only_when_idle: false` only when nobody else uses the pool. With `evict_others: true` (a suite option, or `{"evict_others": true}` in the body of `POST /api/bench/suites/<id>/run` for that run only) the run does not wait for other people's models that are loaded but idle; its loads may make the scheduler unload them to make room, as any load does. Requests in flight and the queue are still waited for.
- **Clean-up.** A benchmark that does not end ready (a failed phase, its `timeout_s`, a cancel) ends with a forced unload of its profile before the next benchmark starts, after up to 3 s for its last request to be counted as ended. An `unload` phase (or the unload before a load) that the scheduler refuses because a request is still in flight is tried again for those 3 s: the scheduler counts a request's end a moment after the answer has arrived. A request that really stays in flight still makes it fail. A benchmark that ends ready keeps what it left loaded on purpose until the end of the run.
- **Cancel.** `POST $B/api/bench/runs/<id>/cancel` (MCP `cancel_run`) stops after the current request; a load or a hold is abandoned at once. It unloads what the run loaded.
- **Progress.** `GET $B/api/bench/state` (MCP `get_bench_state`): the run's percent, elapsed and estimated seconds left (`null` until the benchmarks have run once), the benchmark and phase in flight with its own percent, the next benchmark and the queue. The same object arrives on `/ws` as `bench` messages with `kind: "progress"`.
- **Run limit.** A run that takes longer than `bench_max_run_minutes` (YAML `bench: {max_run_minutes: ...}`, default 720, 12 hours; a Full pass takes 6 to 7) is stopped and marked failed. The setting is read when each run starts; a stored value is kept.
- **Scheduler restart.** A run that was running is marked `failed (interrupted)` at the next start; a queued one is `cancelled`.

The runner tags its requests with `X-Bench-Run: <run id>`. Its loads and unloads appear in the audit log with source `system` and actor `bench run <run id>`, and in the history with `data.bench_run`, so they are not confused with a person's.

## 5. Read the results

| Ask | REST | MCP |
|---|---|---|
| Runs of a suite | `GET /api/bench/runs?suite=<id>` | `list_runs` |
| Runs on a model file | `GET /api/bench/runs?model=<file>` | `list_runs` (`model`) |
| One run, every step, phase and request | `GET /api/bench/runs/<id>` | `get_run` |
| A table for a document | `GET /api/bench/runs/<id>/export?format=md` (also `csv`, `json`; `&detail=phases`, `&detail=turns` or `&detail=tasks` for CSV) | `export_run` |
| Points over past runs for a chart | `GET /api/bench/trends?benchmark=<id>&metric=load_s` | `get_bench_trends` |
| The history added up | `GET /api/bench/aggregates?suite=<id>&since=30d` | `get_bench_aggregates` |
| A benchmark's results per run | `GET /api/bench/benchmarks/<id>/results` | |

What a step records:

- **Load**: load seconds, bytes, MB/s, bytes and memory per node, the child's log file, `load_mode` (`file_read`, `pushed` or `local`), and the load session id. A load phase sends no chat request, so its history records are found with `GET /api/history?session_id=<steps[].load.session_id>`.
- **Requests**: phase (`warmup`, `measured`, `conversation`, `hold`, `concurrent`), timings, output hash, reused prompt tokens.
- **Statistics**: mean, median, minimum, maximum and standard deviation of the measured requests. Warmup requests are never counted.
- **Hardware facts**: `mem_low` (per node, the lowest available memory while loading and over the whole step; `kind` says system memory or GPU memory), `roles` (the host and the workers with each one's share of the model), and `reload_from_cache` (per node: `full`, `partial` with `cached_pct`, `none` or `unknown`).
- **Environment** (`env`): scheduler version, configuration revision, llama build, the nodes' program versions.

Trend metrics: `load_s`, `load_mb_s`, `memory_mib`, `decode_tps`, `prefill_tps`, `ttft_ms`, `unload_s`, `concurrent_wall_s`, `lowest_available_mib`, `lowest_available_gpu_mib`, and for tasks phases `tasks_answered`, `task_decode_tps` (median), `task_points` and `task_points_pct` (with `&scorer=<name>`; default the step's first scorer by name). The markdown export and the phases CSV give a tasks phase's counts and each scorer's total; the markdown has a table per tasks phase; `detail=tasks` is one CSV row per task with `points_<scorer>` and `max_points_<scorer>` columns.

### The measurement record

Every number a step measures is also stored as one row of one shape, so that comparisons, trends and reports work for any benchmark:

| Field | Meaning |
|---|---|
| `metric`, `value`, `of`, `unit` | What and how much, for example `load_s` 35.4 `s`, or `task_points` 12 of 17 `points` |
| `run_id`, `step`, `phase`, `phase_type`, `phase_label` | Where it came from (`phase` is absent for a figure of the whole step) |
| `benchmark_id`, `profile`, `model`, `node_count`, `node` | What it describes; `node` for per-machine figures. For a recipe, `benchmark_id` is the recipe |
| `variant`, `ctx`, `placement` | For a recipe only: the use (the step's id) and its context and placement inputs |
| `subject`, `seq` | The request (by name), turn (`turn 2`), simultaneous request or task, and which repeat |
| `conditions` | `thinking_input` (`on`, `off`, for a recipe that sets it), `cold` and `cold_verified` (of the load, and of the last load before a request), `load_mode`, `generation` (`first` for warm-up, `steady`, `after_hold`), `thinking` (`seen`, `not_seen`), `memory` (`system`, `gpu`) and `window` (`load`, `step`), `scorer` |
| `at` | When the request started, else when the phase or step started (unix ms) |

`GET /api/bench/metrics` (MCP `list_bench_metrics`) lists the metrics with unit, description, the phase types that record them, whether they are per machine and whether higher is better. A failed request has no figures (its error stays in the step); a simultaneous-requests phase records `errors` of `n`. Task points are rows of `task_points`, written when a scorer posts and removed with its scores.

| Ask | REST | MCP |
|---|---|---|
| A run's numbers | `GET /api/bench/runs/<id>/measurements` (`?step=N`, `?metric=load_s,ttft_ms`) | `get_run_measurements` |
| The history of a metric | `GET /api/bench/measurements?benchmark=<id>&metric=ttft_ms&since=30d` (also `profile`, `model`, `node`, `variant`, `ctx`, `placement`, `kind`, `until`, `limit` up to 10000) | `query_measurements` |

History rows, oldest first, carry their run's `run_state`, `run_kind`, `release` and `config_revision`. The rows are derived from the step results when a step ends; runs stored before this release are mapped once when the scheduler starts, so their history is there too.

Aggregate filters: `suite`, `benchmark`, `profile`, `since` and `until` (Unix ms, RFC 3339, or `24h`, `7d`), `limit_runs` (default 200), `env=same` (only runs made in the same environment as the newest). Per benchmark the answer has the run count, pass rate, outcome counts, and for each metric the count, minimum, median, mean, maximum, standard deviation, last, best, the change of the last run against the median of the earlier ones, a regression flag and the last 30 points. `by_model_nodes` puts the same model on 1, 2 or 3 nodes side by side; `by_model_load_mode` splits by file read or pushed.

### Transcripts

Every step that sends prompts saves the whole exchange as a file of its own: `<data_dir>/bench-runs/<run id>/step-NNN.json` (format `potluck-transcript/1`). Each entry is one request, conversation turn or task, in order: the messages sent (a later conversation turn holds only its new message; the earlier ones are the previous entries), the sampling, the answer and the reasoning, the status or error, tokens and timings. A step that only loads and unloads has none.

| Ask | REST | MCP |
|---|---|---|
| Which steps have one | `GET /api/bench/runs/<id>/transcripts` | `list_run_transcripts` |
| One step's exchange | `GET /api/bench/runs/<id>/steps/<n>/transcript` (`?format=md` for a readable chat) | `get_step_transcript` |

### Reports

Reports are made from the recorded data every time one is opened, by fixed templates: the same run always gives the same report, nothing is stored and nothing is judged (no baselines, no pass marks).

- **Summary** (the run of a group): headline counts (steps, ran, completed, failed, refused, cancelled, skipped), the issues first, one table per metric with the average, the lowest and the highest for each benchmark use and phase, and an index linking every benchmark report.
- **Benchmark report** (one benchmark in a run, and the report of a direct run): the inputs (recipe, profile, model, context, placement, thinking), the outcome, then one section per phase with its figures, conditions, issues and a link to the transcript.
- **Issues** are facts only: a step failed or timed out, a placement or the scheduler refused it, a machine was excluded, the scheduler stopped during the run, requests or tasks failed.

`GET /api/bench/runs/<id>/report` (MCP `get_run_report`), `?step=<n>` for a benchmark report, `&phase=<p>` for one section; `format=json` (default), `md`, or `html`: a page with a print layout, "Download as Markdown", and "Download as PDF", which uses the browser's own save as PDF. The run page (Benchmarks, Runs) has a Report button.

### Keeping runs

Nothing is removed unless you turn retention on. The setting `bench_retention_days` (YAML `bench: {retention_days: ...}`) is `0` by default, which is off. Set to N days, a finished run older than that is trimmed: its request rows and its transcript folder are removed; the run itself, its steps and phases, its task answers and scores and its measurements (the compact record that history, trends and reports read) stay, and the run carries `trimmed_at`. The check runs every hour and reads the setting each time. A run that has not finished is never trimmed. Before turning it on, `GET /api/bench/removals/preview?days=N` lists the runs that N days would trim, and changes nothing. Pin a run to keep it: `PUT /api/bench/runs/<id>/pin` (MCP `pin_run`), `DELETE` the same path to unpin (`unpin_run`); runs carry `pinned`, and the run page has a Pin button. Every trim is logged: `GET /api/bench/removals` (MCP `list_run_removals`) lists the run, its kind, name, state, when it finished and was trimmed, why, how many steps and measurements it keeps and how many file bytes went; the history also gets a `benchRunRemoved` event.

## 6. Compare and regressions

Each run is compared with its baseline. `regression: true` means the load was more than 15% slower (`regress_load_pct`), decode more than 7% slower (`regress_decode_pct`), or the output of a temperature-0 request changed. Both thresholds are settings that need a restart.

Before you compare numbers, check `env` with `env=same`: a different llama build, configuration or program version changes results. Label every number you report with the profile, the nodes and the link it was measured on.

## 7. Results measured elsewhere

A driver script that measures outside the runner (coding tasks scored by tests, a context-size ladder) can store its results on the scheduler so that they show on the Benchmarks page under "External results", beside the suites. An external result set is a document: an `id` (the benchmark id rule), a `title`, an optional `description` and `note`, `tables` (each with a `title`, ordered `columns` of `key`, `label`, `unit` and optional `higher_is_better`, and `rows` that map column keys to a number, a string or null; a row may also name a `model` and a `profile`, and the page links the profile when it exists), and optional `attachments` (`name` and `text`, such as a task statement). Nothing is run and no profile has to exist.

| Ask | REST | MCP |
|---|---|---|
| Store a set (409 when the id exists) | `POST /api/bench/external` | |
| Replace a set (keeps `created_at`) | `PUT /api/bench/external/<id>` | |
| List the sets, without rows or attachment texts | `GET /api/bench/external` | `list_external_results` |
| One set with everything | `GET /api/bench/external/<id>` | `get_external_results` |
| Remove a set | `DELETE /api/bench/external/<id>` | |

Limits: a body of at most 2 MiB (413 above it), 50 tables, 2,000 rows and 60 columns per table, 20 attachments of at most 512 KiB each (422 above them). Changes are in the audit log with section `bench` and target `external:<id>`, with the caller's `X-Source` and `X-Actor`.

Two converters in `scripts/` (Python 3, standard library) turn the summaries of the coding and context drivers into a set; without `--post` they print the body, `--replace` sends a PUT, `--attach` adds a text file:

    scripts/bench-external-context.py summary.json --ladder ladder.json --post $B

External results and task scores do not overlap. An external set holds results measured outside Potluck, such as the hand-run coding kit before release 0.1.8 or the context ladder; Potluck only shows it. Task scores belong to answers a Potluck run stored in a `tasks` phase, and are posted against that run (see Tasks). Do not also post a scored Potluck run as an external set: its scores already show on the run, next to the speed figures and the set's hash.

## 8. Good practice

- Measure the load on its own (a `load` phase), then requests on the loaded model. Mixing them hides where time goes.
- Use `warmup` before measured requests: the first request after a load is slow.
- Keep the request text the same across benchmarks you want to compare.
- Run when the pool is otherwise idle, and say so in the run's `note`.
- Read a failed step's reason and the child log before retrying. A failed load backs the profile off for a while.
