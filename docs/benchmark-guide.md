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

Ask `GET $B/api/bench/schema` (MCP `get_bench_schema`) first. It lists every phase type, every option with its default, limit and unit, the `expect` keys, and five templates: "Load only", "Load and requests", "Load and conversation", "Requests on a loaded model", "Simultaneous requests".

### Phases

| Phase | What it does | Measures |
|---|---|---|
| `load` | Unloads the profile first, then loads it with `POST /api/load`. No chat request is sent. | Load time, bytes and MB/s per node, memory, lowest available memory, load mode |
| `warmup` | Requests left out of the statistics. | Recorded, not counted |
| `requests` | The measured requests, each repeated `repeats` times. | Prefill and decode tokens/s, time to first token, output hash |
| `conversation` | A multi-turn exchange; each turn carries the earlier turns and replies. | Per turn: prefill, decode, time to first token, reused prompt tokens |
| `hold` | Stays loaded and idle for `seconds`, then sends one request. | Whether the model answers as fast after idling |
| `concurrent_requests` | `n` requests at the same moment. | Each request's wait and decode speed, total time, errors |
| `unload` | Unloads the profile. | Time until the instance is gone |

A phase that sends requests with no `load` before it loads the model if needed and leaves it loaded for the next benchmark; the end of the run unloads it. So a benchmark with only a `requests` phase means "requests on a model that is already loaded". Hold seconds must fit in `timeout_s`.

The simple form (`cold`, `warmup_requests`, `repeats`, `requests`, `unload_after`) still works: it is expanded to `load`, `warmup`, `requests`, `unload`. Write `phases` for anything else.

A benchmark with `parallel: [id, id]` starts the listed benchmarks at the same moment, to measure loads that compete.

### Expectations

`expect` turns a benchmark into a pass or fail check: `outcome` (`ready`, or `refused` for a model that is supposed not to fit), `max_load_s`, `max_unload_s`, `min_decode_tps`, `min_prefill_tps`, `max_ttft_ms`, `min_reused_tokens`, `max_errors`, `max_concurrent_wall_s`. A step that was refused or failed is a result with the scheduler's reason, not an error.

### Choosing profiles

A benchmark names a profile. A profile with `force_nodes` pins the benchmark to chosen nodes, which makes runs comparable. A forced profile is still checked against the nodes' memory, so a test of a model that must not fit uses `expect: {"outcome": "refused"}`. For good results on automatic profiles, give the profile a measured `needs_mib` (runbook section G).

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

Suite options: `cooldown_s` (pause between benchmarks), `stop_on_failure`, `only_when_idle` (default true), `idle_wait_s` (default 600), `schedule` (`{"at":"03:00"}` in the container's local time, normally UTC, or `{"every":"24h"}`).

A scenario that cannot run yet is a suite with `status` `planned` or `blocked` and a `reason`. It may list no benchmark, its Run button is off, and running it answers `409` with the reason. Remove `status` and `reason` when it can run.

Unknown profiles, bad ids and unknown fields are refused with `400` or `422` and the reason. Deleting a profile that a benchmark names answers `409` unless forced.

## 4. Run

```sh
curl -s -X POST $B/api/bench/suites/gemma-scaling/run -d '{"note":"after the firmware update"}'
```

MCP: `run_suite` with `id`, optional `baseline` and `note`. The answer is the queued run. Runs go one at a time; others queue.

- **Idle first.** Benchmarks load and unload real models. With `only_when_idle` a run does not start, and pauses between benchmarks, while another model is loaded, a request that is not the run's is in flight, or anything is queued. A model someone used stays loaded for its `ttl` (300 s by default), so a run may wait that long. After `idle_wait_s` the run ends `skipped` and keeps what it measured. Set `only_when_idle: false` only when nobody else uses the pool.
- **Cancel.** `POST $B/api/bench/runs/<id>/cancel` (MCP `cancel_run`) stops after the current request; a load or a hold is abandoned at once. It unloads what the run loaded.
- **Progress.** `GET $B/api/bench/state` (MCP `get_bench_state`): the run's percent, elapsed and estimated seconds left (`null` until the benchmarks have run once), the benchmark and phase in flight with its own percent, the next benchmark and the queue. The same object arrives on `/ws` as `bench` messages with `kind: "progress"`.
- **Scheduler restart.** A run that was running is marked `failed (interrupted)` at the next start; a queued one is `cancelled`.

The runner tags its requests with `X-Bench-Run: <run id>`. Its loads and unloads appear in the audit log with source `system` and actor `bench run <run id>`, and in the history with `data.bench_run`, so they are not confused with a person's.

## 5. Read the results

| Ask | REST | MCP |
|---|---|---|
| Runs of a suite | `GET /api/bench/runs?suite=<id>` | `list_runs` |
| One run, every step, phase and request | `GET /api/bench/runs/<id>` | `get_run` |
| A table for a document | `GET /api/bench/runs/<id>/export?format=md` (also `csv`, `json`; `&detail=phases` or `&detail=turns` for CSV) | `export_run` |
| Points over past runs for a chart | `GET /api/bench/trends?benchmark=<id>&metric=load_s` | `get_bench_trends` |
| The history added up | `GET /api/bench/aggregates?suite=<id>&since=30d` | `get_bench_aggregates` |
| A benchmark's results per run | `GET /api/bench/benchmarks/<id>/results` | |

What a step records:

- **Load**: load seconds, bytes, MB/s, bytes and memory per node, the child's log file, `load_mode` (`file_read`, `pushed` or `local`), and the load session id. A load phase sends no chat request, so its history records are found with `GET /api/history?session_id=<steps[].load.session_id>`.
- **Requests**: phase (`warmup`, `measured`, `conversation`, `hold`, `concurrent`), timings, output hash, reused prompt tokens.
- **Statistics**: mean, median, minimum, maximum and standard deviation of the measured requests. Warmup requests are never counted.
- **Hardware facts**: `mem_low` (per node, the lowest available memory while loading and over the whole step; `kind` says system memory or GPU memory), `roles` (the host and the workers with each one's share of the model), and `reload_from_cache` (per node: `full`, `partial` with `cached_pct`, `none` or `unknown`).
- **Environment** (`env`): scheduler version, configuration revision, llama build, the nodes' program versions.

Trend metrics: `load_s`, `load_mb_s`, `memory_mib`, `decode_tps`, `prefill_tps`, `ttft_ms`, `unload_s`, `concurrent_wall_s`, `lowest_available_mib`, `lowest_available_gpu_mib`.

Aggregate filters: `suite`, `benchmark`, `profile`, `since` and `until` (Unix ms, RFC 3339, or `24h`, `7d`), `limit_runs` (default 200), `env=same` (only runs made in the same environment as the newest). Per benchmark the answer has the run count, pass rate, outcome counts, and for each metric the count, minimum, median, mean, maximum, standard deviation, last, best, the change of the last run against the median of the earlier ones, a regression flag and the last 30 points. `by_model_nodes` puts the same model on 1, 2 or 3 nodes side by side; `by_model_load_mode` splits by file read or pushed.

## 6. Compare and regressions

Each run is compared with its baseline. `regression: true` means the load was more than 15% slower (`regress_load_pct`), decode more than 7% slower (`regress_decode_pct`), or the output of a temperature-0 request changed. Both thresholds are settings that need a restart.

Before you compare numbers, check `env` with `env=same`: a different llama build, configuration or program version changes results. Label every number you report with the profile, the nodes and the link it was measured on.

## 7. Good practice

- Measure the load on its own (a `load` phase), then requests on the loaded model. Mixing them hides where time goes.
- Use `warmup` before measured requests: the first request after a load is slow.
- Keep the request text the same across benchmarks you want to compare.
- Run when the pool is otherwise idle, and say so in the run's `note`.
- Read a failed step's reason and the child log before retrying. A failed load backs the profile off for a while.
