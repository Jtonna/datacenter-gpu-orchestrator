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
2. [ ] Either the hub patch `hub-member-ip` (branch of `omen-runner-fleet-manager`) is deployed, **or** you accept the Alienware override already in `deploy/config.yaml` (`alienware-llm: 10.0.0.3:50052`). Without the patch, relayed members show `ip: null`; the override covers the Alienware only.
3. [ ] Docker works: `docker ps`
4. [ ] Base image exists: `docker image inspect llama-swap-whisper:rpc-330f018 --format '{{.Id}}'`
5. [ ] Model files exist for every profile in the config (script below checks).

Next action: run the host pre-flight.

```sh
cd ~/ps5-ai-datacenter-load-balancer
sh deploy/scripts/preflight-host.sh deploy/config.yaml
```

Expected: lines of `OK`, ending `PRE-FLIGHT PASSED`. A `FAIL` on the **firewall** line is expected until section C; any other `FAIL`: fix it first. `WARN ... ip: null` is fine if you rely on the override.

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

Expected: a version line (note: the stamp only shows your git version if `cmd/llm-scheduler` declares `var version`, not `const version`; `0.0.0-m0` means not stamped, harmless), then a path such as `/usr/bin/curl`.

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

## C. One-time firewall rule (3 min)

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

Expected: `PASS` for `/health`, every profile in `/v1/models`, `/running` shape; then one line per profile from `POST /api/plan` showing a placement or a reason; ends `SMOKE PASSED`. Read the plan lines: a profile that says no placement because a node is offline is normal if that node is off.

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

Expected: a chat reply, wall time, `tokens/s`, and a per-device memory table. If the table is empty or an error says the jq expression failed, the `/api/status` field names differ from the guess in the script: see "VERIFY list" at the end of this file.

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

Why: Gemma has sliding-window layers, so the estimate is several times too high (design 7.2). The values in the config for `gemma-4-12b:48k`, `:64k`, `:96k` and for `flux-1-schnell`, `flux-1-dev` are **PLACEHOLDERS** until you do this. Until then the scheduler may say "does not fit" for a profile that does fit.

Rule: `needs_mib` is the whole footprint (weights + KV + compute buffers), no margin added (design 7.2).

### G1. Gemma variants (48k, 64k, 96k; the 8K `gemma-4-12b` already has 7800)

For each variant, one at a time. Nodes must be online and free.

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

All commands and expected results: `deploy/scripts/hardware-checks.md`. Do H1, H2, H3 first: if H2 or H3 fails, placements on PS5s will be quarantined by the pre-flight. Open items the design leaves for hardware: whether the cache shortens repeat loads, what a head does when a worker loses power (inferred, not observed), PS5 usable memory (sources disagree).

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

### Read `/api/status`

```sh
curl -s http://127.0.0.1:3122/api/status
```

Per design 9 it shows: instances, placements, per-device memory from the last pre-flight, claims, queue, quarantines, backoffs, the node view and its age. Pipe through `jq .` if installed. Exact field names are not fixed by the design.

### Phase out an alias

Alias hits are logged with the caller's address (design 8.2). Count them:

```sh
docker logs llama-swap 2>&1 | grep -i alias | tail -50
```

The exact log text is not specified in the design: look at the first lines it prints and refine the grep. When an alias has no hits for as long as you care to wait (log history is only as long as the container's, and a `docker compose down` resets it), remove it from `aliases:` in the config, validate, restart. Tell the caller first if the log shows one.

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
| Cold PS5 load times out | profile `load_timeout`, tier `link_mb_s` | raise / correct |

---

## J. Troubleshooting

First look, always: `curl -s http://127.0.0.1:3122/api/status` and `docker logs --tail 100 llama-swap`. For one profile, `curl -s -H 'Content-Type: application/json' -d '{"model":"NAME"}' http://127.0.0.1:3122/api/plan` says what would happen now, and changes nothing.

| Symptom | Cause | Fix |
|---|---|---|
| `503` "cluster busy" with `Retry-After: 30` | The profile's queue entry waited `queue_timeout` (600 s) with no claim: every node it could use is held, claimed, quarantined or settling, and nothing was evictable yet (held models are protected for `hold`) | Retry. See `/api/status` queue and holders. Free a node: `POST /api/models/unload/{model}` (idle only). Tune hold (I). |
| `503` "does not fit", message with need and capacity | The profile's need exceeds the combined plan budgets of the best `max_pool` nodes, even if all were empty (design 7.2) | Is `needs_mib` a stale PLACEHOLDER? Measure (G). Is a PS5/laptop offline or missing `ip` in the fleet? Raise nothing blindly: compare the need in the message with the node sizes in `/api/status`. Skipped while the fleet view is stale (the request then queues). |
| A node stuck quarantined | Its pre-flight keeps failing: quarantine is 30 s, doubling to 10 min, reset by a successful pre-flight (design 8.4) | Run the H2/H3 commands against that node. Fix the node (worker down, busy, more than one device: pin `-d` on it, firewall). The design has no clear-quarantine command; wait up to 10 min after fixing, or restart the container (that the state is in memory only is assumed, not stated for the scheduler). |
| Pre-flight timeouts (10 s probe fails) | Another client holds that `rpc-server` (single-client; a blocked probe never returns), or the worker is silent | Look for a second head: old llama-swap still running (`docker ps -a`), a hand-run `llama-server-rpc`. Stop it. Never run two heads. |
| Fleet view stale | No byte from the fleet console for `stale_after` (60 s): console down, wrong URL, or the firewall rule is missing. Remote nodes become ineligible; the OMEN still works (design 8.4) | `docker exec llama-swap curl -fsS http://host.docker.internal:8090/api/fleet`; check rule (C) and that the console runs on `:8090`. The console keeps state in memory: after its restart the view is empty until nodes report. |
| Hub panel shows nothing | Hub reads `/running`, `/upstream/{model}/props`, `/slots` and `/api/events` (design A); it finds the container by the name prefix `llama-swap` | `docker ps` name is `llama-swap`; `curl -s http://127.0.0.1:3122/running` is valid JSON with `running`. With several models loaded the panel shows only one (known, design 13 item 4). Beyond this the design is silent: look at the hub's logs. |
| A model loads on the OMEN unexpectedly | Tier 3 (OMEN) is used only when tiers 1 and 2 together cannot hold the model, or PS5/Alienware were ineligible (offline, quarantined, stale, `ip` null, no tier match). `kind: command` profiles (FLUX, Whisper) always run on the OMEN. `qwen3.5-4b` was on the OMEN before; now it goes to a PS5 first (design 13 item 3) | `POST /api/plan` for the profile and read why. Fix the node problem. To forbid the OMEN: `allow_tiers: [1, 2]` on the profile. |
| Load timeout on a cold PS5 | Timeout is `120 s + 2 x (share of model file / link_mb_s)`; the link is 100 Mb, about 11 MB/s (design 7.6): 5 GB takes about 7 min. Waiters get `502` with the child's last log lines; the profile is backed off 60 s, doubling to 10 min | Set `load_timeout` on the profile above the cold time you measured (H check 6), or correct `link_mb_s`. Backoff clears when the profile next reaches ready; during it requests fail with `503`, so wait it out. |

---

## VERIFY list (guesses to confirm against the real `/api/status`)

The design does not fix the field names inside `/api/status`. The scripts guess them, each in one variable marked `# VERIFY against /api/status output`:

| Script | Variable | Guess |
|---|---|---|
| `deploy/scripts/smoke.sh` | `STATUS_DEVICE_TSV` | `.instances[].profile`, `.devices[]` with `node`, `model_mib`, `kv_mib`, `compute_mib` |
| `deploy/scripts/measure-needs.sh` | `STATUS_DEVICE_TSV` | same as above |
| `deploy/scripts/measure-needs.sh` | `STATUS_SUM_MIB` | `.instances[].sum_mib` |

Check once, then fix the variables:

```sh
curl -s http://127.0.0.1:3122/api/status | jq .
```

Also unverified because other code is in progress: `/api/plan` output text, the alias log line, and whether `cmd/llm-scheduler` has `var version`.
