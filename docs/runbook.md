# llm-scheduler runbook (OMEN)

Owner: Jacob. Spec: `docs/design.md` (section numbers below refer to it).
Everything runs **on the OMEN** as `admin`. Nothing here deploys itself.

Where things live on the box (same convention as llama-swap):

| What | Path |
|---|---|
| Repo checkout (build only) | `~/ps5-ai-datacenter-load-balancer` (branch `design/scheduler-v1` or its merge) |
| Scheduler compose + config | `/opt/llm-scheduler/` |
| Old llama-swap (kept for rollback) | `/opt/llama-swap/` |
| Models | `/srv/llama-swap/models` |
| Container name (both) | `llama-swap` |

Total first-time cost: about 45 minutes, plus model loads.

---

## A. Before you start (5 min)

Tick every box. If one fails, stop: do not cut over.

1. [ ] Fleet manager runs the **fleet-scheduler-fields** version (members carry `ip` and `rpc_port`).
2. [ ] The hub patch `hub-member-ip` (branch of `omen-runner-fleet-manager`) is deployed. It went live on 2026-11-17 23:36 UTC, and the fleet list now reports `10.0.0.3:50052` for the Alienware, so no `node_overrides` entry is needed. (If you ever run against a hub without the patch, relayed members show `ip: null`; uncomment the example `node_overrides` entry in `deploy/config.yaml` for the Alienware.)
3. [ ] Docker works: `docker ps`
4. [ ] Base image exists: `docker image inspect llama-swap-whisper:rpc-330f018 --format '{{.Id}}'`
5. [ ] Model files exist for every profile in the config (script below checks).

Next action: run the host pre-flight.

```sh
cd ~/ps5-ai-datacenter-load-balancer
sh deploy/scripts/preflight-host.sh deploy/config.yaml
```

Expected: lines of `OK`, ending `PRE-FLIGHT PASSED`. A `FAIL` on the **firewall** line is expected until section C; any other `FAIL`: fix it first. `WARN ... ip: null` means a member has no address; check the hub patch, or add a `node_overrides` entry for it.

---

## B. Build the image on the box (10 min, mostly Go download)

1. Get the code on the box and enter the repo root.

```sh
cd ~/ps5-ai-datacenter-load-balancer && git fetch && git checkout design/scheduler-v1 && git pull --ff-only
```

2. Build. Run from the **repo root** (the Dockerfile copies the whole repo).

```sh
docker build -f deploy/Dockerfile -t llm-scheduler:local --build-arg VERSION="$(git describe --always --dirty)" .
```

Expected: ends with `naming to docker.io/library/llm-scheduler:local`. The base is not reinstalled: only the Go binary layer is new.

3. If the build stops with `base image has neither curl nor wget`: the HEALTHCHECK has no tool. Tell the engineer; do not install packages ad hoc. (The llama-swap folder does not record whether the base has `curl`; the build check exists because of that.)

4. Check the binary and the healthcheck tool.

```sh
docker run --rm --entrypoint llm-scheduler llm-scheduler:local --version
docker run --rm --entrypoint sh llm-scheduler:local -c 'command -v curl || command -v wget'
```

Expected: a version line (the stamp replaces the default `0.1.0` with your git version; `0.1.0` means the build was not stamped, harmless), then a path such as `/usr/bin/curl`.

5. Install the deployment files.

```sh
sudo mkdir -p /opt/llm-scheduler && sudo chown admin:admin /opt/llm-scheduler
cp deploy/docker-compose.yml deploy/config.yaml /opt/llm-scheduler/
```

6. Validate the config with the real binary (no hardware touched).

```sh
docker run --rm -v /opt/llm-scheduler/config.yaml:/etc/llm-scheduler/config.yaml:ro -v /srv/llama-swap/models:/models:ro --entrypoint llm-scheduler llm-scheduler:local --check-config --config /etc/llm-scheduler/config.yaml; echo "exit=$?"
```

Expected: `exit=0`. On `exit=1` read the message (unknown field, missing model file, ...), fix `/opt/llm-scheduler/config.yaml` (and `deploy/config.yaml` in the repo), rerun. The PLACEHOLDER `needs_mib` values are valid; they are replaced in section G.

---

## C. Firewall check (1 min; add the rule only if the check fails)

**Measured 2026-11-17: on this box the container reached the fleet console with no extra rule.** Run the `curl` check below from inside the container first. Add the ufw rule only if it fails.

Why: the container is on `172.30.0.0/24`; the OMEN's ufw only admits 22, 9443 and 5353 (host/README.md, Firewall). Without this rule the scheduler cannot read the fleet console on 8090 (design 10). This repo never adds the rule for you.

1. Add the rule (host convention: ufw, with a comment).

```sh
sudo ufw allow from 172.30.0.0/24 to any port 8090 proto tcp comment 'llm-scheduler to fleet console'
```

Expected: `Rule added`.

2. Confirm it is listed.

```sh
sudo ufw status | grep 8090
```

Expected: a line `8090/tcp ALLOW 172.30.0.0/24`.

The end-to-end proof (container to console) comes in section D step 5, because the `172.30.0.0/24` network only exists once the compose project is up.

---

## D. Cutover (10 min)

**Never run the scheduler and llama-swap at the same time:** an `rpc-server` serves one client, so whichever head connects second hangs on it (design 3.1, 10).

1. Check nothing is mid-generation (a stop cuts it off).

```sh
curl -s http://127.0.0.1:3122/running
```

Expected: `{"running":[]}`, or wait until it is.

2. Stop llama-swap (frees container name and port 3122).

```sh
cd /opt/llama-swap && docker compose down
```

Expected: `Container llama-swap Removed`.

3. Start the scheduler.

```sh
cd /opt/llm-scheduler && docker compose up -d
```

Expected: `Container llama-swap Started`. (Yes, the same container name, on purpose: the hub finds the container by that name prefix.)

4. Watch the log for 10 seconds.

```sh
docker logs --tail 30 llama-swap
```

Expected: no panic; no line about being unable to reach the fleet console. Wait for health:

```sh
sleep 20; docker inspect -f '{{.State.Health.Status}}' llama-swap
```

Expected: `healthy`. (If `unhealthy` but `curl http://127.0.0.1:3122/health` works, the healthcheck tool is the problem, not the scheduler.)

5. Prove the firewall rule from inside the container (design 10).

```sh
docker exec llama-swap curl -fsS http://host.docker.internal:8090/api/fleet | head -c 300
```

Expected: JSON starting `{`. If `curl` is not in the image use `wget -qO- ...` instead. A timeout means the rule from section C is missing or wrong subnet; check `docker network inspect llm-scheduler_sched --format '{{(index .IPAM.Config 0).Subnet}}'` prints `172.30.0.0/24`.

If any step fails and you cannot fix it in 5 minutes: section F.

---

## E. Smoke checks (10 min; the first four steps change nothing)

1. Run the dry smoke.

```sh
cd ~/ps5-ai-datacenter-load-balancer
CONFIG=/opt/llm-scheduler/config.yaml sh deploy/scripts/smoke.sh http://127.0.0.1:3122
```

Expected: `PASS` for `/health`, every profile in `/v1/models`, `/running` shape; then one line per profile from `POST /api/plan` showing its `action` (`serve`, `wait`, `start`, `evict`, `queue`, `backoff` or `reject`) with `placement` (the nodes) or `reason`; ends `SMOKE PASSED`. Read the plan lines: a profile that says no placement because a node is offline is normal if that node is off.

2. Check what the scheduler thinks of the fleet.

```sh
curl -s http://127.0.0.1:3122/api/status
```

Expected: the nodes (PS5s, `alienware-llm`, `omen-runner`) with fresh view age (under 60 s). If a node is missing, see section J.

3. Check the hub panel in a browser (`http://omen-runner.local/`): the llama panel should render without errors (with nothing loaded it shows nothing loaded).

4. One real request on the cheapest model (loads it).

```sh
CONFIG=/opt/llm-scheduler/config.yaml sh deploy/scripts/smoke.sh http://127.0.0.1:3122 --load tinyllama
```

Expected: a chat reply, wall time, `tokens/s`, and a per-device memory table. If the table is empty, the instance has no `nodes[].memory` rows: run `curl -s http://127.0.0.1:3122/api/status | jq '.instances[] | {profile, state, nodes}'` to see what the scheduler reports.

5. Unload it.

```sh
curl -s -X POST http://127.0.0.1:3122/api/models/unload
```

---

## F. Rollback (under 2 minutes)

Run these two commands; nothing else is needed.

```sh
cd /opt/llm-scheduler && docker compose down
cd /opt/llama-swap && docker compose up -d
```

Expected: `docker ps` shows `llama-swap` using image `llama-swap-whisper:rpc-330f018`; `curl -s http://127.0.0.1:3122/running` returns `{"running":[]}`.

Verify:

```sh
curl -fsS http://127.0.0.1:3122/v1/models | head -c 200
```

The ufw rule from section C can stay.

---

## G. Measure `needs_mib` (about 20 min per Gemma variant, plus load time)

Why: Gemma has sliding-window layers, so the estimate is several times too high (design 7.2). The values in the config for `flux-1-schnell` and `flux-1-dev` are **PLACEHOLDERS** until you do this. (`gemma-4-12b` 8K is measured at 7900, `:48k` at 8250, `:64k` at 8600 and `:96k` at 9050; see `docs/hardware-test/BENCHMARKS.md`.) Until then the scheduler may say "does not fit" for a profile that does fit.

Rule: `needs_mib` is the whole footprint (weights + KV + compute buffers), no margin added (design 7.2).

### G1. Gemma variants (all four are measured now; use these steps to re-check one or to size a new variant)

For each variant you want to measure, one at a time. Nodes must be online and free. To re-check 64k, use the same steps with `ctx: 65536`.

1. Uncomment the `gemma-measure` template at the end of `/opt/llm-scheduler/config.yaml`. Set `ctx` to the variant (`49152`, `65536` or `98304`), keep `kv_quant: q8_0` and `flash_attn: true`, and set `force_nodes` / `tensor_split` to the pool you expect it to run on in practice (for example a PS5 and the Alienware). The compute buffer is per device, so the sum depends on the number of devices: measure on the pool size you will really use (the design does not say more; this is an assumption).

2. Validate and restart (restart stops anything running).

```sh
docker run --rm -v /opt/llm-scheduler/config.yaml:/etc/llm-scheduler/config.yaml:ro -v /srv/llama-swap/models:/models:ro --entrypoint llm-scheduler llm-scheduler:local --check-config --config /etc/llm-scheduler/config.yaml && cd /opt/llm-scheduler && docker compose restart
```

Expected: `exit` 0 then the container restarts.

3. Measure (waits for the load; a cold PS5 load can take many minutes).

```sh
cd ~/ps5-ai-datacenter-load-balancer && sh deploy/scripts/measure-needs.sh http://127.0.0.1:3122 gemma-measure
```

Expected: a table of model/KV/compute MiB per device, a SUM, and a line `needs_mib: <number>`.

4. Paste that number into the real profile (e.g. `"gemma-4-12b:64k"`), replacing the `PLACEHOLDER` value and the comment. Re-comment `gemma-measure`. Validate and restart as in step 2.

5. Repeat for the next variant. Put the numbers in the hardware table too.

### G2. FLUX (`flux-1-schnell`, `flux-1-dev`)

These are `kind: command`, always on the OMEN alone, and the design's measuring method reads llama.cpp load logs, which sd-server does not write. The design does **not** define how to measure them. Assumed method: GPU memory used by the loaded model, taken from `nvidia-smi` on the OMEN.

1. Idle baseline (nothing loaded: `curl -s -X POST http://127.0.0.1:3122/api/models/unload`).

```sh
nvidia-smi --query-gpu=memory.used --format=csv,noheader,nounits
```

2. Load it with one real request (this also checks that the command works).

```sh
curl -s -o /dev/null -w '%{http_code}\n' -H 'Content-Type: application/json' -d '{"model":"flux-1-schnell","prompt":"a red cube","size":"512x512"}' http://127.0.0.1:3122/v1/images/generations
```

Expected: `200`. (The image request itself can push memory a little above idle load; take the reading right after the request.)

3. Loaded reading, then subtract the baseline.

```sh
nvidia-smi --query-gpu=memory.used --format=csv,noheader,nounits
```

4. Put the difference in `needs_mib` for that profile, repeat for `flux-1-dev`, validate and restart. Also note: a `kind: command` profile will evict an idle LLM on the OMEN's GPU and may be evicted back (design 8.3).

---

## H. Hardware checks (about 60 min, in any order)

All commands and expected results: `deploy/scripts/hardware-checks.md`. Do H1, H2, H3 first: if H2 or H3 fails, placements on PS5s will be quarantined by the pre-flight. Since 2026-11-18 the PS5 tensor cache is off, so there are no fast cached reloads and every load is a full upload (design 3.2). Upload speed was about 80 MB/s in total on the old 1 GbE link. Since the OMEN moved to a 2.5 GbE USB-C adapter (`enx6c1ff7c349ea`, 2026-11-18) it is about 100 to 108 MB/s per PS5, and loads to different PS5s no longer share one gigabit. Gemma 12B on one PS5 took 89 s on 1 GbE and 66 s on 2.5 GbE; three at once took 246 s each, then 77 s each. The PS5 tier `link_mb_s` is now 100. Open items the design leaves for hardware: what a head does when a worker loses power (inferred, not observed), PS5 usable memory (sources disagree).

---

## I. Day-2

### Add a model profile (10 min)

1. Download the GGUF to `/srv/llama-swap/models` (as in the llama-swap README).
2. Add a profile to `/opt/llm-scheduler/config.yaml` (name `model:variant`; see design 6 for fields). Sliding-window model: set a measured `needs_mib` (section G); the scheduler only warns if you forget.
3. Validate with `--check-config` (section B step 6), then restart: `cd /opt/llm-scheduler && docker compose restart`. The design does not mention hot config reload, so assume restart is required, and it stops anything running.
4. `sh deploy/scripts/smoke.sh http://127.0.0.1:3122` and look at the new profile's `/api/plan` line.
5. Copy the file back into the repo's `deploy/config.yaml`.

### Add a PS5 (no config change)

Nodes come from the fleet manager (design 4). Verify only:

1. The PS5 appears online in the fleet console: `curl -s http://127.0.0.1:8090/api/fleet` shows its `ip` and `rpc_port` (not null).
2. Its hostname matches `ps5-*` (the tier match). Otherwise it is **never used**.
3. Check it exposes one device (H2): `docker exec llama-swap timeout 10 llama-server-rpc --rpc <ip>:50052 --list-devices | grep -c '^ *RPC'` expects `1`.
4. `curl -s http://127.0.0.1:3122/api/status` lists it with a fresh view; `POST /api/plan` for a big profile now shows it as an option.

### Keeping a machine out of the pool

Use `exclude_nodes` when a machine must never receive a model (for example a PS5 under test) but you still want the `ps5-*` tier to find every other PS5. It is a top-level list in `/opt/llm-scheduler/config.yaml`. Each entry is a node id (a MAC such as `70:66:2a:a9:a7:65`) or a hostname glob. Matching ignores case.

```yaml
exclude_nodes:
  - ps5-abc123        # one machine, by hostname
  - ps5-test*         # every machine whose hostname starts with ps5-test
  - 70:66:2a:a9:a7:65 # by id
```

1. Add the entry, validate with `--check-config`, restart (`cd /opt/llm-scheduler && docker compose restart`). Restart stops anything running.
2. Check `curl -s http://127.0.0.1:3122/api/status`: under `view.nodes` the machine shows `"excluded": true` and `"eligible": false`. The log has one INFO line at start listing the entries, and one `excluded node in fleet view` line the first time each excluded machine appears.
3. An excluded machine is never used, never counted in capacity checks and never pre-flighted. A plan or wait reason that involves it says `node <hostname> is excluded by config`.
4. Excluding wins over forcing. A profile whose `force_nodes` names an excluded node fails `--check-config` when config alone can tell. When it cannot (for example `force_nodes` uses the MAC and the entry is a hostname glob), the request fails at once with 503 `node excluded` and no `Retry-After`. Remove the entry, or the node from `force_nodes`.
5. You can exclude the local node (`omen-runner`). The OMEN GPU is then never used, and `kind: command` profiles (Whisper, FLUX) fail with "does not fit" because they run on the local node only.
6. To bring the machine back, delete the entry and restart.

### Benchmarking only the PS5s from a test deployment

A second copy of the scheduler can stand in on port 3122 while you benchmark the PS5s alone. Give it a config with `exclude_nodes` listing the OMEN (`omen-runner`) and the Alienware (`alienware-llm`), so only PS5s get models. Keep `forced_fit_check` off for the benchmark. The PS5 dashboards path keeps working: the hub polls `/running` on 3122, relays the `load` object and `recent_loads` to the fleet manager's `/api/models`, and shows them (verified 2026-11-18). Steps and the profile set are in `docs/benchmark-guide.md`. When done, put the production config and image back (section D, and F if you need llama-swap).

### Read `/api/status`

```sh
curl -s http://127.0.0.1:3122/api/status
```

It returns one JSON object with the keys `time`, `shutting_down`, `instances`, `queue`, `quarantines`, `backoffs`, `recent_loads` and `view`. Each instance has `profile`, `state`, `log_file` (the file holding that child's full output; absent or empty when `child_log_dir` is off, see "Child output logs"), `nodes` (placed nodes, each with `id` and its device `memory` rows: `name`, `model_mib`, `kv_mib`, `compute_mib`, `total_mib`), `host_memory`, `unmatched` and `device_total_mib`. The node view is `view.nodes`, with `eligible`, `excluded` (see "Keeping a machine out of the pool"), `live_budget_mib` and related budgets. Pipe through `jq .` if installed.

A starting instance whose child is loading also has a `load` object (below). `recent_loads` (also on `GET /running`, next to `running`) is the outcome of every load attempt that started a child and finished in the last 120 s, newest first, always present and `[]` when there is none: `{"session_id", "model", "workers", "outcome": "ready"|"failed"|"cancelled", "reason", "finished_at" (unix ms), "load_s"}`. `reason` is empty for `ready`; a failed load says why (child exit, load timeout, load stalled), a cancelled one says it was unloaded or the scheduler shut down. The `/api/events` stream sends `loadStarted` (`session_id`, `model`, `started_at`, `workers`) and `loadFinished` (the same object as the `recent_loads` entry).

### Watch a load (progress and ETA)

A load onto the PS5s prints nothing in the child log while the model uploads, so look at the scheduler instead. While the instance is `starting`:

```sh
curl -s http://127.0.0.1:3122/api/status | jq '.instances[] | select(.state=="starting") | {profile, load_elapsed_s, load_bytes_total, load_bytes_sent, load_progress, load_eta_s, load_progress_source}'
```

`load_progress_source` is `"measured"` when the scheduler counted the bytes the workers acknowledged (Linux; then `load_bytes_sent` is shown) and `"estimated"` when it worked from `link_mb_s` and the clock (the figure is then only a guide). `load_progress` stays at or below 0.99 until the model is ready. A load with no remote worker (OMEN GPU only) shows `load_elapsed_s` only. `GET /running` has the same figure as `load_progress` on the entry. The scheduler log also carries a `load progress` line every 30 s per loading instance (`docker logs llama-swap | grep "load progress"`). Loads started at the same time share the host's one gigabit port (about 80 MB/s in total), so two loads each go at about half speed.

For a program, the same instance (and its entry in `GET /running`) carries a `load` object: `session_id` (unique per load attempt, also in `recent_loads`), `started_at` (unix ms), `elapsed_s`, `bytes_total`, `bytes_sent`, `progress`, `eta_s` (left out while a measured load has had no new bytes for over 15 s), `stalled_s` (seconds since the bytes last rose; present only when above 15), `source`, and `workers`, one `{addr, bytes_total, bytes_sent, state}` per remote worker in `--rpc` order. A worker is `waiting` (under 1 MiB received), `receiving` (at least 1 MiB but under 99% of its bytes), or `done` (99% or more of its bytes), judged from that worker's own bytes only; workers receive one at a time, in an order llama.cpp decides and that is not guaranteed. Per-worker `bytes_sent` is only present when `source` is `measured`; on an estimate every worker shows `waiting`. Per-worker `bytes_total` starts as the worker's share of the file by the tensor split and is replaced by the real size once the child prints its `model buffer size` line for that worker. The `load` object is absent while the instance is in pre-flight and once it is ready. Example:

```sh
curl -s http://127.0.0.1:3122/running | jq '.running[] | select(.state=="starting") | .load'
```

### Fit check for forced profiles

A profile with `force_nodes` is not fit-checked by default. To guard it, set in `defaults:` (restart to apply):

```yaml
defaults:
  forced_fit_check: reject   # off (default) | warn | reject
```

The profile's need (`needs_mib`, else the estimate) is compared with the forced nodes' usable memory from the pre-flight. On a `shared_memory` tier (the PS5: GPU memory is system RAM, and the pre-flight reports almost all of it as free) that is the SMALLER of the budget from the fleet-reported free figure (what the automatic placer uses) and the pre-flight free figure, each minus the tier `headroom_mib`, because the Vulkan pre-flight overstates what the machine can really give; if only one figure is known it is used, and `POST /api/plan` and a real request agree whenever both are known. On a dedicated-VRAM tier it is raw free with no headroom taken off, so a forced profile may still eat into headroom there. The message says per node which figure was used, for example `need 14691 MiB (estimated, +5% margin; the estimate is 13991 MiB), but the nodes it is forced onto have only 12380 MiB usable in total (usable 12380 MiB on ps5-9032a330 (shared memory, headroom kept, from fleet figures))` (or `... from pre-flight`). Hardware fact: on llama.cpp upstream HEAD (2026-11-19) a forced load that does not fit took a PS5 down until it was power cycled (13991 MiB forced onto a PS5 with 14875 of 14947 MiB reported free), so `reject` is the recommended production setting. `warn` logs `forced profile may not fit` (with `need_mib`, `free_mib`, `nodes`) and starts anyway. `reject` fails the request at once with `503` "forced nodes do not fit", before anything is uploaded; it does not back the profile off or quarantine a node. `POST /api/plan` shows the same result (`action: reject`, or a `warning` field in `warn` mode). If the need is estimated, measure it (G) so the check is exact.

### Memory safety on shared-memory nodes (PS5s)

The PS5 workers have no container memory limit, and a load that exhausts a PS5 makes the machine unreachable for about 18 minutes, so the scheduler plans conservatively (design 7.2a). Three settings, all in `defaults:` and shown as commented defaults in `deploy/config.yaml`:

```yaml
defaults:
  split_skew_margin: 0.15     # 0 to 1
  estimate_margin: 0.05       # 0 to 1
  load_memory_floor_mib: 768  # 0 = off
```

- What is refused and why. A split model does not land evenly (one node 11% over an even share was measured), so every placement of two or more nodes that includes a PS5 needs each PS5's share, `need x split fraction x 1.15`, to fit in that PS5's usable memory (fleet free minus 1024 MiB headroom, or the smaller pre-flight figure). Single-node placements take no margin. Real figures at about 12400 MiB usable per PS5: a 27793 MiB model on three PS5s split 33/33/34 puts 10548, 10548 and 10868 MiB on them and is allowed; 16442 or 14248 MiB on two PS5s split 50/50 is allowed (9455 and 8193 MiB per PS5). The largest need that fits is about 31700 MiB on three (33/33/34), about 21500 MiB on two (50/50), and 12400 MiB on one. A profile without `needs_mib` is raised 5% (`estimated, +5% margin` in `/api/plan`).
- Refusal texts. Forced profile with `forced_fit_check: reject`: `503` "forced nodes do not fit" with `node <name>: expected share N MiB (need ... x P% of the split x 1.15 skew margin) but usable M MiB (...)`. Automatic profile: the placer takes another set or adds a node, and only when none fits answers `503` "does not fit" (the `required` figure includes the margin) or queues.
- Learned sizes. After a load succeeds the measured memory is used for later fit decisions of that profile in this run (`GET /api/status`, `learned_needs`). If it is above a configured `needs_mib`, the log has one WARN `configured needs_mib is too low: the load measured more memory; using the measured value` with both numbers: correct `needs_mib`.
- Load cancelled for low memory. While a load runs, a PS5 whose available memory falls below 768 MiB cancels it: waiting requests get `502` `load cancelled: ps5-xxxx ran low on memory during the load (available 700 MiB, floor 768 MiB)`, the child is killed, the profile is backed off, the PS5 is not quarantined. The log line is ERROR `load cancelled: a node ran low on memory` with every watched node's figure (`node_memory_mib`). Something else is using the PS5's memory: free it and retry after the backoff. `load_memory_floor_mib: 0` turns the watch off.

### Recovery requests for a hung PS5 (off by default)

When a PS5 is `hung` (see the troubleshooting row for `health.state`), the scheduler can ask the fleet manager to relay a recovery request; a small service on the PS5 polls the fleet manager, acts, and reports the outcome. The scheduler never talks to the PS5 itself, and never sends while the fleet view is stale. Design 8.4b.

Turn it on (a top-level block in the config, shown as comments in `deploy/config.yaml`; restart the scheduler, it reads the file at start):

```yaml
recovery:
  enabled: true
  actions: [restart_rpc_server]   # the default; escalation order
  # min_interval: 120s            # per node, at least 60s
  # step_wait: 90s                # time a step gets before the next one
  # request_ttl: 45s              # at most 60s
  # http_timeout: 3s
```

What it sends: one `POST <fleet.url>/api/recovery/<node id>` (the id percent-encoded, colons as `%3A`) with `{"v":1,"target_node_id","action","reason","issued_at","expires_at","nonce"}`, the first time a worker that backs a model turns `hung`, for any remote node an instance is using, never the OMEN or an excluded node. With more actions it waits `step_wait` (at least `min_interval` in all) and sends the next one if the node is still hung; it sends at most one request per listed action per hung episode, and stops and resets when the node is healthy again. A `5xx` or timeout is retried once after 10 s with a new nonce; a `400/403/404/409/410/422/429` is not retried.

**WARNING: `restart_docker` and `reboot` are off by default and interrupt everything on that machine** (every model it serves, every container, every other job). Add them to `actions` only if you accept that, and only in the order `restart_rpc_server`, `restart_docker`, `reboot`.

Read it in `GET /api/status`: each node that was sent a request, or whose outcome was seen, has `recovery`:

```json
"recovery": {"last_request": {"action": "restart_rpc_server", "nonce": "529a...0810", "sent_at": 1795058743515, "status": "stored", "http_status": 200},
             "last_outcome": {"action": "restart_rpc_server", "at": 1795058743518, "outcome": "ok", "reason": "rpc-server restarted", "nonce": "529a...0810", "duration_ms": 1700},
             "step": 1, "episode_started": 1795058743515}
```

- `last_request.status`: `pending` (no answer yet), `stored` (the fleet manager has it), `refused` (a 4xx, see `http_status` and the WARN log), `error` (5xx or timeout, after the one retry).
- `last_outcome` is what the PS5 reported (`ok` or `failed`); `last_outcome.nonce` equal to `last_request.nonce` means it answers that request. `failed` makes the next step go as soon as `min_interval` allows.
- `step` is how many steps this hung episode has sent; `episode_started` is there only while the node is hung. After recovery it goes back to `0` and is omitted, while `last_request` and `last_outcome` stay.
- Log lines (INFO unless noted): `recovery request sent`, `recovery request stored`, WARN `recovery request refused` / `recovery request failed` (with `status` and the response `body`), `recovery outcome`, `recovery sequence reset`, WARN `recovery sequence finished and worker still hung`. The `/api/events` stream sends `recoveryRequest` when a request is sent and when an outcome is seen.

### Child output logs

The scheduler writes each child llama-server's complete stdout and stderr to a new file under `/opt/llm-scheduler/logs` on the OMEN (`child_log_dir` in `config.yaml`, mounted by compose). One file per start, named `<UTC time>-<profile>-<instance id>.log`, so `ls` sorts by time. This is where the KV cache size, the memory breakdown and the exact out-of-memory error are, beyond the last 40 lines in a `502`.

```sh
curl -s http://127.0.0.1:3122/api/status | jq '.instances[] | {profile, log_file}'   # path inside the container
ls -t /opt/llm-scheduler/logs | head
```

The oldest files are deleted past `child_log_keep` (200). If startup logs `child_log_dir is not usable`, the directory could not be created or written: fix its permissions or the mount, then restart; until then loads work but no files are kept. The container's own log (`docker logs`) rotates at 50 MB, 5 files.

### Phase out an alias

Alias hits are logged with the caller's address (design 8.2). Count them:

```sh
docker logs llama-swap 2>&1 | grep 'deprecated model alias used' | tail -50
```

Each hit is a WARN line `msg="deprecated model alias used"` with `alias=`, `profile=` and `caller=` (the caller's IP; `forwarded_for=` too when an `X-Forwarded-For` header is present). It is logged at most once per alias and caller per minute. When an alias has no hits for as long as you care to wait (log history is only as long as the container's, and a `docker compose down` resets it), remove it from `aliases:` in the config, validate, restart. Tell the caller first if the log shows one.

### Tune hold and timeouts

All in `defaults:` (design 6, 8.3, 13 item 6). Change one value, restart, watch.

| Symptom | Value | Direction |
|---|---|---|
| Alternating models spend all time loading | `min_hold`, `max_hold` | raise |
| A different-model request gets 503 too often while a slow-loading model is protected | `max_hold` | lower (more loading, faster turns). To be tuned on the real cluster. |
| Busy model never gets evicted for a waiter | `drain_after` | lower |
| 503 after waiting too long | `queue_timeout` | raise |
| Streaming aborted mid-generation | `stall_timeout` | raise (`0` = off, used for image/audio) |
| Idle models linger | `ttl` | lower |
| A hung load is given up too late, or a slow upload is cut as stalled | `load_stall_timeout` (default 180 s, `0` = off, also per profile) | lower / raise |
| Cold PS5 load times out | profile `load_timeout`, tier `link_mb_s` (100 per PS5 on the 2.5 GbE link; it was 80 in total on 1 GbE) | raise / correct |

---

## J. Troubleshooting

First look, always: `curl -s http://127.0.0.1:3122/api/status` and `docker logs --tail 100 llama-swap`. For one profile, `curl -s -H 'Content-Type: application/json' -d '{"model":"NAME"}' http://127.0.0.1:3122/api/plan` says what would happen now, and changes nothing.

| Symptom | Cause | Fix |
|---|---|---|
| `503` "cluster busy" with `Retry-After: 30` | The profile's queue entry waited `queue_timeout` (600 s) with no claim: every node it could use is held, claimed, quarantined or settling, and nothing was evictable yet (held models are protected for `hold`) | Retry. See `/api/status` queue and holders. Free a node: `POST /api/models/unload/{model}` (idle only). Tune hold (I). |
| `503` "does not fit", message with need and capacity | The profile's need exceeds the combined plan budgets of the best `max_pool` nodes, even if all were empty (design 7.2) | Is `needs_mib` a stale PLACEHOLDER? Measure (G). Is a PS5/laptop offline or missing `ip` in the fleet? Raise nothing blindly: compare the need in the message with the node sizes in `/api/status`. Skipped while the fleet view is stale (the request then queues). |
| `503` "forced nodes unavailable" within about 30 s, message `forced node X is absent from the fleet` or `... offline in the fleet` | A node named in the profile's `force_nodes` has been missing from the live fleet view for `forced_node_grace` (30 s). Absent means the fleet console does not list it (worker or console down, wrong name); offline means it is listed but not online | Bring the machine back (start the worker, check `GET /api/fleet`), or fix the name in `force_nodes`. Then retry. If the fleet view itself is stale, this does not fire: see "Fleet view stale". |
| `503` "forced nodes do not fit", message with need, free, the nodes and `measured` or `estimated` | `defaults.forced_fit_check` is `reject` and the profile's need is above the forced nodes' usable memory as the pre-flight saw it (PS5 and other shared-memory nodes: free minus headroom; dedicated-VRAM nodes: raw free). The message says per node which figure was used. Nothing was started; the profile is not backed off | Free memory on those nodes (stop what holds it), pick other nodes, or lower the need if it is only an estimate (measure it, G). `warn` would start anyway, `off` disables the check. |
| `502` "load cancelled: <node> ran low on memory during the load (available N MiB, floor M MiB)" | The memory watch (design 7.2a, 8.4) saw that PS5's available memory fall below `load_memory_floor_mib` during the load. The profile is backed off; the node is not quarantined | Find what takes the PS5's memory, check the model's size against the node (`learned_needs` in `/api/status` after a successful load), retry after the backoff. |
| A forced profile starts uploading, then the PS5 `rpc-server` crashes mid-upload (child log: "Remote RPC server crashed or returned malformed response"); the worker restarts by itself | The model does not fit the forced nodes. Measured 2026-11-18: Devstral 24B forced onto one PS5 needs about 13991 MiB against about 13.5 GB free. A `force_nodes` profile skips the fit check unless `forced_fit_check` is set | Force enough nodes (Devstral 24B fits on two PS5s: 14248 MiB measured at ctx 8192). Set `defaults.forced_fit_check: reject` or `warn` (see "Fit check for forced profiles"). The profile is backed off after the failed load; wait it out. |
| A PS5 shows `hung` (`GET /api/status`, `view.nodes[].health.state`, ERROR log `worker hung`, `workerHealth` event) | Two of its health channels are silent (design 8.4a): upload (a receiving worker got no new bytes for `worker_silence`, 30 s) and heartbeat (offline in the live fleet view for 30 s); or the heartbeat alone for 3 x `worker_silence` with no channel known ok (an idle loaded model whose node stopped reporting). The TCP connect probe (port 50052) is off by default and is not used on PS5s: an rpc-server accepts one client, so probes pile up in its listen backlog and then all time out, which made healthy PS5s read `probe: silent`; the same goes for any external TCP check of port 50052, do not add one. The incident this was built for: the PS5 ran out of memory, its `rpc-server` hung, the container stayed "running" and the machine stopped reporting. The scheduler only names and quarantines it; it does not touch the PS5 | Look at `health.channels` and `silent_for_s` to see which evidence is missing. Power cycle or restart the PS5 worker (it recovers by itself only if the process comes back), or turn on recovery requests so the fleet manager's PS5 service restarts the rpc-server for you (section I, "Recovery requests for a hung PS5"). The state returns to `ok` once no channel is silent and at least one is known ok; the log says `worker recovered`. A model loaded on it is ended by the existing rules (node loss at 30 s, load stall, load timeout), not by this one. `suspect` is one silent channel only: watch it, nothing is quarantined. `defaults.worker_probe_interval` is `0` (probe never runs, its channel is always `unknown`); do not set it above 0 for PS5 workers. |
| A node stuck quarantined | Its pre-flight keeps failing: quarantine is 30 s, doubling to 10 min, reset by a successful pre-flight (design 8.4) | Run the H2/H3 commands against that node. Fix the node (worker down, busy, more than one device: pin `-d` on it, firewall). The design has no clear-quarantine command; wait up to 10 min after fixing, or restart the container (that the state is in memory only is assumed, not stated for the scheduler). |
| Pre-flight timeouts (10 s probe fails) | Another client holds that `rpc-server` (single-client; a blocked probe never returns), or the worker is silent | Look for a second head: old llama-swap still running (`docker ps -a`), a hand-run `llama-server-rpc`. Stop it. Never run two heads. |
| Fleet view stale | No byte from the fleet console for `stale_after` (60 s): console down, wrong URL, or the firewall rule is missing. Remote nodes become ineligible; the OMEN still works (design 8.4) | `docker exec llama-swap curl -fsS http://host.docker.internal:8090/api/fleet`; check rule (C) and that the console runs on `:8090`. The console keeps state in memory: after its restart the view is empty until nodes report. |
| Hub panel shows nothing | Hub reads `/running`, `/upstream/{model}/props`, `/slots` and `/api/events` (design A); it finds the container by the name prefix `llama-swap` | `docker ps` name is `llama-swap`; `curl -s http://127.0.0.1:3122/running` is valid JSON with `running`. With several models loaded the panel shows only one (known, design 13 item 4). Beyond this the design is silent: look at the hub's logs. |
| A model loads on the OMEN unexpectedly | Tier 3 (OMEN) is used only when tiers 1 and 2 together cannot hold the model, or PS5/Alienware were ineligible (offline, quarantined, stale, `ip` null, no tier match). `kind: command` profiles (FLUX, Whisper) always run on the OMEN. `qwen3.5-4b` was on the OMEN before; now it goes to a PS5 first (design 13 item 3) | `POST /api/plan` for the profile and read why. Fix the node problem. To forbid the OMEN: `allow_tiers: [1, 2]` on the profile. |
| A load sits at the same byte count (`load_bytes_sent` or `load.bytes_sent` does not rise; `load.stalled_s` appears after 15 s and `eta_s` disappears) | The child is hung, usually after a PS5 `rpc-server` dropped or restarted mid-upload. The scheduler gives up by itself: after `defaults.load_stall_timeout` (180 s) with no new acknowledged bytes it stops the child and answers `502` `load stalled: no bytes acknowledged by the workers for 3m0s (sent N of M)`; the profile is then backed off and `recent_loads` shows `failed` with that reason. Nodes are not quarantined. It applies only to a measured upload that is under 99% sent. | Check the worker (H2/H3 commands), then retry after the backoff. A slow but moving upload is not a stall. For a very slow link raise `load_stall_timeout` (default or per profile); `0` turns it off |
| Load timeout on a cold PS5 | Timeout is `120 s + 2 x (share of model file / link_mb_s)`; `link_mb_s` for the PS5 tier is 80, measured 2026-11-18 as the total out of the OMEN (design 3.2, 7.6): 5 GB takes about 1 min, and loads that run at the same time share that rate (three Gemma 12B loads at once took 246 s each). Waiters get `502` with the child's last log lines; the profile is backed off 60 s, doubling to 10 min | Set `load_timeout` on the profile above the load time you measured (H check 6), or correct `link_mb_s`. Backoff clears when the profile next reaches ready; during it requests fail with `503`, so wait it out. |

---
