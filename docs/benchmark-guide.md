# Benchmark guide: running model benchmarks through the scheduler

Written 2026-11-18 from the repo code, the repo docs and a read-only look at the OMEN. Updated the same day after the new build went live (version string below), and again after the load progress and load report build.
Anything not confirmed is marked "unverified". "Scheduler" and "load balancer" mean the same thing here.

## 1. What this is and where it runs

- `llm-scheduler` decides which machines run a model, loads it, queues requests and unloads it later.
- It runs on the OMEN as container `llm-scheduler-test` (image `llm-scheduler:test`), host port 3122. llama-swap is stopped and must stay stopped.
- Scheduler version string: `test-da67241+failfast+childlog+progress+loadreport2`.
- Config: `~/llm-scheduler-test/config.yaml`. It is read once at start. A change needs a container restart.
- Models are GGUF files in `/srv/llama-swap/models` (mounted at `/models`). Machines come from the fleet manager at `localhost:8090`, not from the config.
- Machines: three PS5s (each serves one client at a time), the Alienware laptop, and the OMEN's own GPU. For these benchmarks only the three PS5s receive models. The OMEN and the Alienware are excluded (section 4). Do not remove that exclusion.
- PS5 fleet, all rpc port 50052:

| Hostname | Address |
|---|---|
| ps5-9032a330 | 10.0.0.101 |
| ps5-31bd9dc4 | 10.0.0.122 |
| ps5-407c8925 | 10.0.0.114 |

  There is no PS5 at 10.0.0.100 or 10.0.0.102. Addresses can change after a PS5 restart, so always read the current list from `curl -s localhost:8090/api/fleet` (a JSON object with a `members` array). Profiles force machines by hostname, not by IP.
- As of 2026-11-18 the PS5 tensor cache is off, so every PS5 load is a full upload and there are no fast cached reloads. PS5 GPU clock is pinned at 2230 MHz. Upload speed is 75 to 84 MB/s in total out of the OMEN, the same for 1, 2 or 3 PS5s, and simultaneous loads share it. A split model uploads to one PS5 at a time, in an order llama.cpp chooses.

Run all commands below on the OMEN (`ssh omen-runner`). Examples use `B=http://localhost:3122`.

## 2. Quick start

1. Check it is healthy and idle. Expect `healthy`, `instances` and `queue` empty, the three PS5s `online: true`. The OMEN and the Alienware show `excluded: true`, `eligible: false`.
```sh
B=http://localhost:3122
docker inspect -f '{{.State.Health.Status}}' llm-scheduler-test
curl -s $B/health                      # prints OK
curl -s $B/running                     # "running":[] when idle (and a "recent_loads" key next to it)
curl -s $B/api/status | jq '{instances, queue, backoffs, quarantines, nodes: [.view.nodes[] | {hostname, online, excluded, free_mib}]}'
```
2. List the models (profiles). There are 254 in total, plus whatever you add. 225 of them are generated benchmark profiles named `bench-<model>-<place>-<ctx>` (section 5a). Names starting `b-` are older benchmark profiles pinned to named machines.
```sh
curl -s $B/v1/models | jq -r '.data[].id'
```
3. Ask what a request would do. This changes nothing.
```sh
curl -s -H 'Content-Type: application/json' -d '{"model":"b-qwen4b-1ps5"}' $B/api/plan | jq .
```
4. Send one request. It loads the model first, so it can take minutes. Use a long client timeout.
```sh
curl -s --max-time 3600 -H 'Content-Type: application/json' \
  -d '{"model":"b-qwen4b-1ps5","max_tokens":64,"messages":[{"role":"user","content":"Say hello in five words."}]}' \
  $B/v1/chat/completions | jq '{text: .choices[0].message.content, usage, timings}'
```
5. While it loads, watch: `curl -s $B/api/status | jq '.instances[] | {profile,state,phase,load_deadline,load}'`. The `load` object shows bytes sent and progress (section 9).
6. Unload when done. Without this the model stays until the idle timer (`ttl`, 900 s in the live config) ends it.
```sh
curl -s -X POST -H 'Content-Type: application/json' -d '{"model":"b-qwen4b-1ps5"}' $B/api/unload
```

## 3. Every HTTP endpoint

Errors use this shape: `{"error":{"message":"...","type":"...","param":null,"code":<status>,"reason":"..."}}`. Every 503 except "does not fit" and "node excluded" carries a `Retry-After` header.

| Method | Path | What it does | Example |
|---|---|---|---|
| POST | `/v1/*` (chat/completions, completions, embeddings, ...) | Routes by the `model` field in the JSON body. Loads the model if needed, waits in the queue, streams the answer back. | see section 2 step 4 |
| GET | `/v1/models` | Lists profile names (aliases are not listed). | `curl -s $B/v1/models` |
| GET | `/health` | Returns `OK`. | `curl -s $B/health` |
| GET | `/running` | Loaded or loading models: `{"running":[{"model","state","name","cmd"}], "recent_loads":[...]}`. States: starting, ready, stopping. A loading entry also has `load_progress` and a `load` object. `recent_loads` is a sibling key, always present (section 9). To test for idle, check that `running` is an empty array; do not compare the whole body. | `curl -s $B/running` |
| GET | `/api/status` | Full state: `instances`, `queue`, `quarantines`, `backoffs`, `view.nodes`, `time`, `shutting_down`. Each instance has `log_file` (section 7) and `load_duration`; a starting instance has `load_*` fields and the `load` object. The top level also has `recent_loads`. | `curl -s $B/api/status` |
| POST | `/api/plan` | Dry run. Body `{"model":"NAME"}`. Reply keys: `model`, `alias`, `action` (serve, wait, start, evict, queue, backoff, reject), `instance`, `placement`, `victims`, `reason`, `need_mib`, `measured`, `required_mib`, `capacity_mib`. A profile forced onto an absent or offline machine is rejected with the fail-fast text (section 9). | `curl -s -H 'Content-Type: application/json' -d '{"model":"b-gemma-2ps5"}' $B/api/plan` |
| POST | `/api/load` | Starts a load without sending a request. Body `{"model":"NAME"}`. Answers at once: `202 {"msg":"loading","model":"NAME"}`. It does not wait for ready; poll `/running` or `/api/status`. | `curl -s -H 'Content-Type: application/json' -d '{"model":"b-gemma-2ps5"}' $B/api/load` |
| POST | `/api/unload` | Body `{"model":"NAME","force":false}`. `200 {"msg":"ok","model":..}`, or `200 {"msg":"not loaded",..}`. A busy model gives `409`. `force:true` stops it anyway (also a model that is still loading). | `curl -s -H 'Content-Type: application/json' -d '{"model":"b-gemma-2ps5","force":true}' $B/api/unload` |
| POST | `/api/models/unload` | Stops every model with no request in flight. `{"msg":"ok","unloaded":[...]}`. | `curl -s -X POST $B/api/models/unload` |
| GET | `/unload` | Same as above, replies plain `OK`. | `curl -s $B/unload` |
| POST | `/api/models/unload/{model}` | Stops that model if idle (409 if busy). | `curl -s -X POST $B/api/models/unload/b-gemma-2ps5` |
| ANY | `/upstream/{model}/...` | Passes straight to a model that is already ready. Never loads. `503 not ready` otherwise. | `curl -s $B/upstream/b-gemma-2ps5/props` |
| GET | `/api/events` | Server-sent events stream for the hub panel. | `curl -sN $B/api/events` |
| GET | `/api/metrics/activity` | Last 200 finished requests: model, path, status, duration, token counts, tokens per second. | `curl -s $B/api/metrics/activity` |
| GET | `/ui/` | One line linking to `/api/status`. | |

Other paths give 404. `/v1/*` with a method other than POST gives 405. A body without `model` on the admin paths gives 400 (`body must be JSON with a "model" field`).

## 4. How placement works

- Machines sit in tiers: PS5s (tier 1), Alienware (tier 2), OMEN GPU (tier 3). Lower number is preferred. A machine in no tier is never used.
- Auto profile: the scheduler picks the best tier that can hold the model, one machine first, then a pool of up to `max_pool` (4). It uses `needs_mib` if set, else an estimate from the GGUF file.
- Forced profile (`force_nodes`): exactly those machines, in that order, with no fit check. `tensor_split` sets each machine's share. All `b-*`, `m-*` and `bench-*` profiles are forced except the `auto` ones.
- `defaults.forced_fit_check` is off in the benchmark config, so a forced profile is never fit-checked. A model that does not fit fails during the upload (the PS5 `rpc-server` can crash and restart by itself). Do not turn it on for the benchmark. Outside the benchmark, `reject` is the recommended setting: on llama.cpp upstream HEAD (2026-11-19) a forced load that does not fit took a PS5 down until it was power cycled. On PS5 (shared memory) nodes the check uses free minus tier headroom, not raw free.
- PS5 memory safety (design 7.2a): a load that exhausts a PS5 makes it unreachable for about 18 minutes, so a placement of two or more nodes with a PS5 must fit each PS5's share, `need x split fraction x 1.15` (`defaults.split_skew_margin`), in its usable memory (about 12.4 GB). With `forced_fit_check: reject` a forced profile that fails this gets `503` "forced nodes do not fit" naming the node, its expected share and its usable figure; with the check off it starts anyway, so keep the benchmark `tensor_split` shares within each PS5's usable memory. A need without `needs_mib` is raised 5% (`estimate_margin`). While a load runs, a PS5 whose available memory falls below `load_memory_floor_mib` (768) cancels it: `502` `load cancelled: <node> ran low on memory during the load (available N MiB, floor M MiB)`, `recent_loads` outcome `failed`, node not quarantined.
- Each machine serves one model at a time. A request for a model that cannot start now waits in a queue (one entry per profile). Waiting longer than `queue_timeout` (1800 s live) without a claim gives `503 cluster busy`. A forced machine that is present but busy is not an error: the request queues.
- Eviction: to make room, the scheduler stops idle models, or drains busy ones after `drain_after` (60 s). A model is protected for its hold time: its load time, clamped between `min_hold` (30 s) and `max_hold` (900 s).
- After a model leaves a machine, the machine is unusable for a short settle time (PS5 3 s) while the scheduler re-reads its memory.
- `excluded` (in `view.nodes`) means the config's `exclude_nodes` keeps that machine out of the pool. The live config has `exclude_nodes: [omen-runner, "alienware*"]` (owner request, for the benchmarks). `/api/status` shows both with `excluded: true, eligible: false`. Only the three PS5s receive models. Do not remove the exclusion.
- Because of that, the profile `devstral-24b:14k-laptops` is commented out in the live config (lines start with `# BENCH-OFF`). The config check rejects a profile forced onto an excluded machine. Do not add a profile that forces `omen-runner` or an Alienware node.
- A model that never gets a request unloads after `ttl` (900 s live). Benchmark profiles set `load_timeout: 10800s` so slow uploads do not time out.

## 5. Adding a benchmark profile for a new model

### 5a. The generated profiles

The config already has 225 generated profiles named `bench-<model>-<place>-<ctx>` (254 profiles in total).

- Models: `tinyllama`, `qwen4b`, `gemma12b`, `devstral24b`, `qwen27b`, `deepseek-v2-lite`, `qwen30b-a3b`, `mixtral-8x7b`, `llama33-70b-q2`, `qwen25-72b-q2`, `glm4-32b`, `qwen3-coder-30b`, `kimi-dev-72b-iq2s`.
- Place: `1a`, `1b`, `1c` (one PS5 each), `2` (two PS5s), `3` (three PS5s), `auto` (the scheduler picks). `kimi-dev-72b-iq2s` has only `2`, `3` and `auto`.
- Context: `2048`, `8192`, `32768`.
- Example: `bench-gemma12b-3-8192`. Which PS5 is `1a`, `1b` or `1c` is set in the config (read `~/llm-scheduler-test/config.yaml`; not checked for this guide).

Count: 13 models x 6 places x 3 contexts is 234, less 9 for the three places kimi does not have, gives 225.

### 5b. Adding a profile for a model that has none

1. Check the file size and what fits. One PS5 has about 14438 MiB total and a plan budget of about 13414 MiB. Weights plus KV cache plus compute buffers must fit across the machines you force. A forced profile skips the fit check, so a too-big model fails during load instead.
```sh
ls -l /srv/llama-swap/models/
```
2. Back up the config, then append the profiles. A backup before each edit is required. The `profiles:` block is the last block in the file, so appending a correctly indented profile to the end works (confirmed live with `printf ... >> config.yaml`). Append (do not replace the file) so the file stays the one mounted into the container. Names must be unique. `model` must be the exact file name. Copy the shape of the existing `b-*` profiles.
```sh
cd ~/llm-scheduler-test && cp config.yaml config.yaml.bak-$(date -u +%Y%m%dT%H%M%SZ)
cat >> config.yaml <<'EOF'

  # auto-placed: the scheduler picks machines. Needs a measured needs_mib for good results (step 6).
  qwen3-30b-auto-8k:
    model: Qwen_Qwen3-30B-A3B-Instruct-2507-Q4_K_M.gguf
    ctx: 8192

  # forced onto two PS5s, split 50/50, ctx 2048
  b-qwen3-30b-2ps5:
    model: Qwen_Qwen3-30B-A3B-Instruct-2507-Q4_K_M.gguf
    ctx: 2048
    load_timeout: 10800s
    force_nodes: [ps5-9032a330, ps5-31bd9dc4]
    tensor_split: [50, 50]

  # forced onto three PS5s, split 33/33/34, bigger context (quantised KV needs flash_attn)
  b-qwen3-30b-3ps5-8k:
    model: Qwen_Qwen3-30B-A3B-Instruct-2507-Q4_K_M.gguf
    ctx: 8192
    load_timeout: 10800s
    force_nodes: [ps5-9032a330, ps5-31bd9dc4, ps5-407c8925]
    tensor_split: [33, 33, 34]
    kv_quant: q8_0
    flash_attn: true
EOF
```
The new text must be indented under the existing `profiles:` block: two spaces before the profile name, four before its keys. If the end of the file has changed, put the lines where the last profile ends and check the indentation.
3. Rules the loader enforces (from `docs/design.md`): `tensor_split` needs `force_nodes` and the same length; `kv_quant` other than f16 needs `flash_attn: true`; `force_nodes` and `allow_tiers` cannot both be set; the model file must exist; unknown fields are errors; a forced node must not be excluded. `ctx` is per slot. `force_nodes` accepts a hostname or a node id.
4. Check the config without touching hardware. Unverified on this box (command is from the runbook with the image tag changed to `:test`). Expect `exit=0`.
```sh
docker run --rm -v ~/llm-scheduler-test/config.yaml:/etc/llm-scheduler/config.yaml:ro -v /srv/llama-swap/models:/models:ro \
  --entrypoint llm-scheduler llm-scheduler:test --check-config --config /etc/llm-scheduler/config.yaml; echo "exit=$?"
```
5. Apply. Only restart when nothing is loaded (`/running | jq .running` is `[]`) and no request is in flight. Tell the orchestrator before each restart. Save logs first (section 7). A restart stops any loaded model.
```sh
docker restart llm-scheduler-test
sleep 15
docker ps --filter name=llm-scheduler-test      # expect (healthy)
curl -s localhost:3122/running | jq .running      # []
curl -s localhost:3122/v1/models | jq -r '.data[].id' | grep qwen3-30b
```
If the config has an error the container exits. The reason is in `docker logs --tail 20 llm-scheduler-test`. Fix the file (or copy the `.bak` file back over `config.yaml`) and run `docker start llm-scheduler-test`.
6. After the first successful load, read `device_total_mib` from `/api/status` for that instance. That sum is the figure for `needs_mib` on an auto profile (design 7.2). Write it in only if the orchestrator asks.

## 6. What to measure and where each number comes from

| Number | Where it comes from |
|---|---|
| Load time | `load_duration` (seconds) on the instance in `/api/status` `instances[]`, and the scheduler log line that contains `load_duration=` and `memory_mib=`. It runs from child start to ready. Live example: tinyllama loaded onto one PS5 in 9.15 s. |
| Upload MB/s | `load.bytes_sent / load.elapsed_s` from `/running` or `/api/status` while loading (bytes the workers acknowledged, measured from TCP counters; about 80 MB/s in total live on the old 1 GbE link, about 100 to 108 MB/s per PS5 since the 2.5 GbE link of 2026-11-18). For a finished load, model file size in MB divided by load time is an average only. Per worker: `load.workers[].bytes_sent`. |
| Prefill tokens/s | `timings.prompt_per_second` in the completion response (confirmed live). |
| Decode tokens/s | `timings.predicted_per_second` in the completion response (confirmed live). |
| Token counts | `usage.prompt_tokens`, `usage.completion_tokens`; `timings.prompt_n`, `timings.predicted_n`. Streaming: the numbers are in the last chunks (unverified for `timings` in streamed replies). |
| Memory per machine | `/api/status` `instances[].nodes[].memory[]` (`model_mib`, `kv_mib`, `compute_mib`, `total_mib`), `device_total_mib` for the sum, `host_memory` for CPU buffers (not in the sum). Machine free memory: `view.nodes[]` (`free_mib`, `total_mib`). |
| KV cache size | `kv_mib` above. The full text (for example the KV size and any "does not fit" or out-of-memory message) is in the child log file (section 7). |
| Requests as the scheduler saw them | `/api/metrics/activity` (duration, in/out tokens, tokens per second). |
| Cross-check on the PS5s | The fleet manager's `GET localhost:8090/api/models` shows the loaded model (name from the model file, quant, state, slots, context used, tokens per second, PS5 worker addresses). While a model is still loading it shows the profile id as the name. Useful as a second opinion; no action needed. |

The `timings` object in a completion reply has these keys (confirmed live): `cache_n`, `prompt_n`, `prompt_ms`, `prompt_per_token_ms`, `prompt_per_second`, `predicted_n`, `predicted_ms`, `predicted_per_token_ms`, `predicted_per_second`.

Prompt cache: llama-server reuses the previous prompt, which makes a second identical request look faster. For clean prefill numbers add `"cache_prompt": false` to the request body, and check that `timings.cache_n` is 0.

Loop rule: a benchmark loop checks for a stop file between profiles (for example `~/llm-scheduler-test/STOP`), so a `PAUSE` stops cleanly after the current profile and nothing is cut mid-load.

Method: for each profile, `plan`, then one request (this includes the load), then a second identical request (no load) for clean prefill and decode numbers. Record the UTC time, profile, ctx, split and the `/api/status` snapshot after ready. Unload before the next profile.

## 7. Logs

- Scheduler log: `docker logs llm-scheduler-test` (stdout, key=value lines). Key events: `queue entry created`, `placement chosen` (nodes, budgets, `need_mib`), `pre-flight ok`, `child started` (the full llama-server command line), `instance ready`, `instance stopping/stopped`, `fleet console unreachable` / `fleet feed recovered`.
- The container's own log now rotates at 50 MB x 5 files. It survives `docker restart` but is lost if the container is recreated (`docker compose up` after a compose change). A saved copy from before the redeploy is at `~/llm-scheduler-test/scheduler-log-before-redeploy-*.log`.
- llama-server child logs (confirmed live): config keys `child_log_dir: /var/log/llm-children` and `child_log_keep: 500` (top level). On the OMEN the files are in `~/llm-scheduler-test/child-logs/`, one file per model start, named like `20261118T094330Z-tinyllama-5.log` (UTC start time, profile with unsafe characters replaced by `_`, instance id). Each file has the complete llama-server output (188 lines for tinyllama). Each instance in `GET /api/status` has `log_file` with the in-container path, for example `/var/log/llm-children/20261118T094330Z-tinyllama-5.log`. Map `/var/log/llm-children/` to `~/llm-scheduler-test/child-logs/` on the host. Files are owned by root but world-readable. The oldest files beyond 500 are deleted automatically. These files survive container restarts and recreation.
- A failed load also shows the last 40 child lines inside the `502 load failed` reply.
- Fleet manager: `localhost:8090/api/fleet` (read only). Do not restart it.
- `~/llm-scheduler-test/logs`, `logs-run1` to `logs-run3` exist from earlier tests. Their contents are unverified. Do not delete them; use new names.
- Save a copy of the scheduler log before any restart:
```sh
mkdir -p ~/llm-scheduler-test/logs-bench
docker logs llm-scheduler-test > ~/llm-scheduler-test/logs-bench/sched-$(date -u +%Y%m%dT%H%M%SZ).log 2>&1
curl -s localhost:3122/api/status > ~/llm-scheduler-test/logs-bench/status-$(date -u +%Y%m%dT%H%M%SZ).json
```

## 8. Errors you may see

All from the code. Statuses are HTTP codes; `reason` is in the JSON error.

| Status / reason | Meaning and what to do |
|---|---|
| 404 `unknown model` (`code: model_not_found`) | Profile name not in config. Check `/v1/models`. New profiles need a restart. |
| 400 `bad request` | Admin body has no `model`, or the `/v1` body cannot be read. |
| 405 | Wrong method on `/v1/*` (only POST). |
| 409 `busy` | Unload refused: a request is in flight (or the model is still loading). Wait, or use `force:true`. |
| 502 `load failed` | Child exited, the load timed out, or the load stalled (message `load stalled: no bytes acknowledged by the workers for 3m0s (sent N of M)`: a measured upload got no new acknowledged bytes for `defaults.load_stall_timeout`, 180 s, `0` = off, also a profile key; the hung child is stopped, nodes are not quarantined, `recent_loads` shows `failed` with that reason). The message ends with the last 40 child output lines. The full output is in the child log file. The profile is then backed off. |
| 503 `backoff` | Profile failed a load recently. Waits 60 s, doubling to 10 min; clears when the profile next reaches ready. See `backoffs` in `/api/status`. |
| 503 `cluster busy` | Waited `queue_timeout` (1800 s) with no machine free. Message "too many request bodies are waiting in the queue" is the same reason (queue memory cap, 256 MiB). Retry after 30 s. |
| 503 `forced nodes unavailable` | Fail fast (section 9). A machine named in `force_nodes` is absent from the fleet or offline for longer than `defaults.forced_node_grace` (30 s). The message names the node and says "absent from the fleet" or "offline in the fleet". `Retry-After: 30`. `/api/plan` gives the same text. A machine that is present but busy is not this error: the request queues. |
| 503 `fleet view stale` | No data from the fleet manager for 60 s. Remote machines are not used until it recovers. The fail-fast clock does not run while the view is stale. |
| 503 `nodes quarantined` | A pre-flight check failed on a machine; it is blocked 30 s, doubling to 10 min. Usually another client holds the PS5 worker. |
| 503 `node has no address` | The fleet reports no IP for a machine the model needs. |
| 503 `does not fit` | Need is larger than all usable machines together, even empty. No `Retry-After`. Message has need and capacity. Does not apply to forced profiles. |
| 503 `node excluded` | Config excludes a machine this profile needs. No `Retry-After`. |
| 503 `unloaded` | A model that was loading was force-unloaded. |
| 503 `shutting down` | Container is stopping. |
| 503 `not ready` | `/upstream/...` was used and the model is not loaded. |
| 499 | Your client left while waiting. Nothing is sent back. |
| Stream cut mid-answer | No chunk for `stall_timeout` (300 s) after the first chunk. The model stays loaded. |

`/api/plan` actions `reject` and `backoff` give the same reasons without trying.

## 9. New in this build (live since 2026-11-18)

Items 1 and 2 were checked live on the OMEN on 2026-11-18 at 09:43 UTC (version `test-da67241+failfast+childlog`). Items 3 and 4 came with `+progress+loadreport2`; load progress was checked live (measured from TCP acknowledged bytes, matching about 80 MB/s on the old 1 GbE link).

Network note (2026-11-18, about 14:00 to 14:31 UTC): the OMEN's link to the PS5 switch moved from its built-in 1 GbE port to a USB-C adapter, interface `enx6c1ff7c349ea`, at 2500 Mb/s. The PS5s still have 1 GbE ports. To read tx bytes on the host, use `enx6c1ff7c349ea` (older logs used `eno1`). Load times at ctx 2048, 1 GbE then 2.5 GbE: Gemma 12B on one PS5 89 s then 66 s; Qwen 27B on three PS5s 216 s then 170 s; Kimi-Dev 72B IQ2_S on three PS5s 363 s then 286 s; two Gemma 12B at once 167 s each then 66 s each; three at once 246 s each then 77 s each (about 280 MB/s in total). A single stream is now limited by the PS5's own 1 GbE port (about 100 to 125 MB/s). A split model still uploads to one PS5 at a time. Label any new number with the link it was measured on.

1. Fail fast. A request for a profile forced onto a machine that is absent from the fleet or offline gets HTTP 503 after the grace period, instead of waiting the whole queue timeout (1800 s). Live reply for an absent machine:
```json
{"error":{"code":503,"message":"forced nodes unavailable: forced node ps5-doesnotexist is absent from the fleet (for 30s); profile x-failfast cannot start until it is back, so this fails now instead of waiting for queue_timeout","param":null,"reason":"forced nodes unavailable","type":"server_error"}}
```
   - Header: `Retry-After: 30`. For an offline node the text says "offline in the fleet".
   - Config key: `defaults.forced_node_grace`, default 30 s.
   - The 30 s counts from when the scheduler first sees the node missing. A request that arrives later can fail sooner than 30 s (16 s in the live check), or at once.
   - `POST /api/plan` rejects with the same text.
   - A machine that is present but busy is not a fail-fast case. The request queues.
   - While the fleet view is stale the clock does not run.
2. Full child logs. Each llama-server child writes its complete output to one file per start. Keys, paths, file names and retention are in section 7.
3. Load progress. While an instance is `starting`, `/running` entries and `/api/status` instances have a `load` object (shapes: `docs/design.md` section 7.6a):
```json
"load": {"session_id":"ld-1794995021087-3","started_at":1794995021087,"elapsed_s":42.1,
  "bytes_total":11628266644,"bytes_sent":5580799366,"progress":0.4799,"eta_s":605,"source":"measured",
  "workers":[{"addr":"10.0.0.101:50052","bytes_total":5814133322,"bytes_sent":5580799366,"state":"receiving"}]}
```
   - `source` is `measured` (TCP acknowledged bytes) or `estimated`. Worker `state` is `waiting`, `receiving` or `done`, taken from each worker's own bytes. Workers receive one at a time, in an order llama.cpp chooses, which may differ from the `--rpc` order.
   - Load MB/s: `load.bytes_sent / load.elapsed_s`.
   - If `bytes_sent` stops rising for more than 15 s, `eta_s` is left out and `stalled_s` (seconds since the last rise) appears. At `defaults.load_stall_timeout` (180 s) the load fails (see the errors table). Only a measured upload under 99% sent is judged.
   - The flat fields `load_elapsed_s`, `load_bytes_total`, `load_bytes_sent`, `load_progress`, `load_eta_s`, `load_progress_source` are still on the `/api/status` instance.
   - The scheduler also logs a `load progress` line every 30 s.
4. `recent_loads`. A top-level key of `/running` and `/api/status`, next to `running`. It lists load attempts that finished in the last 120 s, newest first, and is an empty array when there are none:
```json
{"session_id":"ld-1794995021087-3","model":"big","workers":["10.0.0.101:50052","10.0.0.122:50052"],
 "outcome":"ready","reason":"","finished_at":1794995221087,"load_s":215.1}
```
   `outcome` is `ready`, `failed` or `cancelled` (see design 7.6a). The same object arrives in the `loadFinished` event on `/api/events`; `loadStarted` is sent when the child starts. Poll often enough (under 120 s) not to miss a load.

Worker health (design 8.4a). Every PS5 that backs a loaded or loading model is watched on its upload progress and the fleet heartbeat (a third channel, a TCP connect to port 50052, exists but is off by default), and `/api/status` shows `view.nodes[].health` with `state` `ok`, `suspect` or `hung`. `hung` logs an ERROR `worker hung`, quarantines the node and sends a `workerHealth` event on `/api/events`; it does not stop anything, so for a benchmark watch the state and power cycle a hung PS5 yourself. Leave `defaults.worker_probe_interval` at its default `0`: an rpc-server accepts one client and does not accept() again until it leaves, so connect probes fill its listen backlog and then all time out (healthy PS5s read `probe: silent`). The same applies to any TCP check of port 50052 you run by hand during a benchmark.

Recovery requests (design 8.4b) are off by default (`recovery.enabled: false`), so a hung PS5 is not touched; leave it off for a benchmark, because a restart ends the run on that PS5 and `restart_docker` and `reboot` would interrupt everything on it.
If you turn it on, `/api/status` shows `view.nodes[].recovery` (last request, last outcome, step) for each PS5 that was sent one.

## 10. Known gaps

- Load progress is no longer a gap (section 9).
- Config is read once. No hot reload (the runbook states the design has none).
- `timings` in streamed replies: unverified.
- Whether every key in `timings` is present for every model: unverified. The keys were seen in one live reply.
- Out-of-memory or "does not fit" text from llama.cpp: exact wording unverified. Look in the 502 tail or the child log.

## 11. Rules, protocol and how to report an issue

Rules:
- Do not SSH into the PS5s or the Alienware.
- Do not touch the fleet console, the hub or any other container.
- Do not change PS5 or OMEN system settings.
- Do not start llama-swap.
- Do not remove `exclude_nodes` from the config.
- Do not run a `llama-server-rpc` directly against a PS5 while the scheduler has a model loaded on it. The workers take one client at a time. If a direct run is needed, unload first, and say so in that benchmark row.
- Change only `~/llm-scheduler-test/config.yaml` (with a backup before each edit) and restart only `llm-scheduler-test`. Tell the orchestrator before each restart.

Results file: `C:\Users\J\documents\ps5llm\benchmarks.md`. You own its layout (phase tables are fine). Two requirements:
- Every row says whether it was measured direct or via the load balancer.
- Rows measured before the two PS5 fixes (tensor cache off, GPU clock pinned) are kept apart from rows measured after.

Protocol with the orchestrator (session `ps5-ai-datacenter-load-balancer-32`). It watches the scheduler during each pass.
- `START <pass>` before each pass. Say which models, which PS5s and which context sizes. Wait for "go" on the first pass only. After that, start without waiting.
- `STOP <pass>` after each pass.
- `ISSUE` on any scheduler problem. Stop sending requests first, then send the report below.
- On `PAUSE`: finish the request that is in flight, start nothing new, reply `PAUSED`. Resume only when `RESUME` arrives.

An ISSUE report includes:
1. UTC time (`date -u`).
2. Profile name.
3. The request you sent (the exact `curl` command, body included).
4. HTTP status and response body.
5. A snapshot of `curl -s localhost:3122/api/status`.
6. The last 100 lines: `docker logs --tail 100 llm-scheduler-test`.
7. The child log file path (from `/api/status` `log_file`, mapped to the host path in section 7).
