# llm-scheduler runbook (host machine)

Owner: Jacob. Spec: `docs/design.md` (section numbers below refer to it).
Everything runs **on the host machine** as `admin`. Nothing here deploys itself.

Membership: the scheduler gets its machines either from the fleet feed
(`membership: fleet`, the default, which most sections below were written
for) or from `potluck-agent` on each machine (`membership: agents`, in use as
release 0.1.0 since 2026-11-21). For agents mode see section I, "Agent
membership", and `docs/agent-contract.md`; the steps below that read the fleet
console (`:8090/api/fleet`) do not apply to it.

Where things live on the box (same convention as llama-swap):

| What | Path |
|---|---|
| Repo checkout (build only) | `~/datacenter-gpu-orchestrator` (branch `design/scheduler-v1` or its merge) |
| Scheduler compose + config | `/opt/llm-scheduler/` |
| Old llama-swap (kept for rollback) | `/opt/llama-swap/` |
| Models | `/srv/llama-swap/models` |
| Container name (both) | `llama-swap` |

Total first-time cost: about 45 minutes, plus model loads.

---

## A. Before you start (5 min)

Tick every box. If one fails, stop: do not cut over.

1. [ ] Fleet manager runs the **fleet-scheduler-fields** version (members carry `ip` and `rpc_port`).
2. [ ] The hub patch `hub-member-ip` (branch of `host-1-fleet-manager`) is deployed. It went live on 2026-11-17 23:36 UTC, and the fleet list now reports `192.0.2.3:50052` for the Nvidia laptop, so no `node_overrides` entry is needed. (If you ever run against a hub without the patch, relayed members show `ip: null`; add a `node_overrides` entry for the Nvidia laptop, in the seed file before the import or later with `PUT /api/config/node-overrides`.)
3. [ ] Docker works: `docker ps`
4. [ ] Base image exists: `docker image inspect llama-swap-whisper:rpc-330f018 --format '{{.Id}}'`
5. [ ] Model files exist for every profile in the config (script below checks).

Next action: run the host pre-flight.

```sh
cd ~/datacenter-gpu-orchestrator
sh deploy/scripts/preflight-host.sh deploy/seed-config.yaml
```

Expected: lines of `OK`, ending `PRE-FLIGHT PASSED`. A `FAIL` on the **firewall** line is expected until section C; any other `FAIL`: fix it first. `WARN ... ip: null` means a member has no address; check the hub patch, or add a `node_overrides` entry for it.

---

## B. Build the image on the box (10 min, mostly Go download)

1. Get the code on the box and enter the repo root.

```sh
cd ~/datacenter-gpu-orchestrator && git fetch && git checkout design/scheduler-v1 && git pull --ff-only
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
cp deploy/docker-compose.yml deploy/seed-config.yaml /opt/llm-scheduler/
mkdir -p /opt/llm-scheduler/data /opt/llm-scheduler/history /opt/llm-scheduler/logs
```

6. Validate the seed configuration with the real binary (no hardware touched, nothing written).

```sh
cd /opt/llm-scheduler && docker compose run --rm --no-deps llm-scheduler import-config --check /seed/seed-config.yaml; echo "exit=$?"
```

Expected: `valid: ...` and `exit=0`. On `exit=1` read the messages (unknown field, missing model file, ...), fix `/opt/llm-scheduler/seed-config.yaml` (and `deploy/seed-config.yaml` in the repo), rerun. The PLACEHOLDER `needs_mib` values are valid; they are replaced in section G.

7. Import it once into the database (revision 1). This is the only time the seed file is read; after it the database is the configuration ("Configuration lives in the database" in section I).

```sh
docker compose run --rm --no-deps llm-scheduler import-config --actor "$USER" /seed/seed-config.yaml
```

Expected: `imported ... as revision 1`.

---

## C. Firewall check (1 min; add the rule only if the check fails)

**Measured 2026-11-17: on this box the container reached the fleet console with no extra rule.** Run the `curl` check below from inside the container first. Add the ufw rule only if it fails.

Why: the container is on `172.30.0.0/24`; the host machine's ufw only admits 22, 9443 and 5353 (host/README.md, Firewall). Without this rule the scheduler cannot read the fleet console on 8090 (design 10). This repo never adds the rule for you.

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
cd ~/datacenter-gpu-orchestrator
CONFIG=/opt/llm-scheduler/seed-config.yaml sh deploy/scripts/smoke.sh http://127.0.0.1:3122
```

Expected: `PASS` for `/health`, every profile in `/v1/models`, `/running` shape; then one line per profile from `POST /api/plan` showing its `action` (`serve`, `wait`, `start`, `evict`, `queue`, `backoff` or `reject`) with `placement` (the nodes) or `reason`; ends `SMOKE PASSED`. Read the plan lines: a profile that says no placement because a node is offline is normal if that node is off.

2. Check what the scheduler thinks of the fleet.

```sh
curl -s http://127.0.0.1:3122/api/status
```

Expected: the nodes (PS5s, `laptop-1`, `host-1`) with fresh view age (under 60 s). If a node is missing, see section J.

3. Check the hub panel in a browser (`http://host-1.local/`): the llama panel should render without errors (with nothing loaded it shows nothing loaded).

4. One real request on the cheapest model (loads it).

```sh
CONFIG=/opt/llm-scheduler/seed-config.yaml sh deploy/scripts/smoke.sh http://127.0.0.1:3122 --load tinyllama
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

1. Create the measuring profile (the commented `gemma-measure` template at the end of `/opt/llm-scheduler/seed-config.yaml` shows the fields). Set `ctx` to the variant (`49152`, `65536` or `98304`), keep `kv_quant: q8_0` and `flash_attn: true`, and set `force_nodes` / `tensor_split` to the pool you expect it to run on in practice (for example a PS5 and the Nvidia laptop). The compute buffer is per device, so the sum depends on the number of devices: measure on the pool size you will really use (the design does not say more; this is an assumption).

2. Create it through the API (no restart, nothing running is stopped):

```sh
curl -s -X POST http://127.0.0.1:3122/api/config/profiles -H "X-Actor: alex" -H "X-Comment: measure gemma 48k" -d '{"id":"gemma-measure","model":"gemma-4-12b-it-Q4_K_M.gguf","ctx":49152,"kv_quant":"q8_0","flash_attn":true,"force_nodes":["ps5-a","laptop-1"],"tensor_split":[50,50]}' | jq .
```

Expected: `exit` 0 then the container restarts.

3. Measure (waits for the load; a cold PS5 load can take many minutes).

```sh
cd ~/datacenter-gpu-orchestrator && sh deploy/scripts/measure-needs.sh http://127.0.0.1:3122 gemma-measure
```

Expected: a table of model/KV/compute MiB per device, a SUM, and a line `needs_mib: <number>`.

4. Paste that number into the real profile (e.g. `"gemma-4-12b:64k"`), replacing the `PLACEHOLDER` value and the comment. Re-comment `gemma-measure`. Validate and restart as in step 2.

5. Repeat for the next variant. Put the numbers in the hardware table too.

### G2. FLUX (`flux-1-schnell`, `flux-1-dev`)

These are `kind: command`, always on the host machine alone, and the design's measuring method reads llama.cpp load logs, which sd-server does not write. The design does **not** define how to measure them. Assumed method: GPU memory used by the loaded model, taken from `nvidia-smi` on the host machine.

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

4. Put the difference in `needs_mib` for that profile, repeat for `flux-1-dev`, validate and restart. Also note: a `kind: command` profile will evict an idle LLM on the host machine's GPU and may be evicted back (design 8.3).

---

## H. Hardware checks (about 60 min, in any order)

All commands and expected results: `deploy/scripts/hardware-checks.md`. Do H1, H2, H3 first: if H2 or H3 fails, placements on PS5s will be quarantined by the pre-flight. Since 2026-11-18 the PS5 tensor cache is off, so there are no fast cached reloads and every load is a full upload (design 3.2). Upload speed was about 80 MB/s in total on the old 1 GbE link. Since the host machine moved to a 2.5 GbE USB-C adapter (`enx020000000010`, 2026-11-18) it is about 100 to 108 MB/s per PS5, and loads to different PS5s no longer share one gigabit. Gemma 12B on one PS5 took 89 s on 1 GbE and 66 s on 2.5 GbE; three at once took 246 s each, then 77 s each. The PS5 tier `link_mb_s` is now 100. Open items the design leaves for hardware: what a head does when a worker loses power (inferred, not observed), PS5 usable memory (sources disagree).

---

## I. Day-2

### Configuration lives in the database (design 6.0)

There is no config file any more. All configuration (profiles, tiers, `exclude_nodes`, `node_overrides`, the `defaults` settings, `recovery`, fleet settings, retention, `ws_max_clients`, bench settings) is stored in `scheduler.db` in the data directory (`./data` on the box). Every change is a numbered revision and is written to the audit log. Changes take effect at once, without a restart (design 6.0.6), except the few settings the schema marks `restart_required`. Only five deployment paths stay outside: `--listen`, `--data-dir`, `--models-dir`, `--history-dir`, `--child-log-dir` (environment `LLM_SCHEDULER_LISTEN`, `_DATA_DIR`, `_MODELS_DIR`, `_HISTORY_DIR`, `_CHILD_LOG_DIR`; compose and the Dockerfile set them).

All examples use `H=http://127.0.0.1:3122`. There is no authentication (LAN tool). Add `-H 'X-Actor: your-name'` and `-H 'X-Comment: why'` to a write so the audit log says who and why; the web page sends `X-Source: ui`, everything else is recorded as `rest`.

#### First install and import

1. Put `deploy/docker-compose.yml` and `deploy/seed-config.yaml` in `/opt/llm-scheduler/` (section B step 5). `./data` must be on a volume.
2. Validate the seed with the real binary; nothing is written:

```sh
cd /opt/llm-scheduler && docker compose run --rm --no-deps llm-scheduler import-config --check /seed/seed-config.yaml
```

Expected: `valid: N profiles, N tiers, ...`. An error lists every violation (missing model file, bad field, ...). Add `--no-model-check` only to validate on a machine that does not have the model files.

3. Import it once, as revision 1:

```sh
docker compose run --rm --no-deps llm-scheduler import-config --actor "$USER" /seed/seed-config.yaml
```

Expected: `imported ... as revision 1 into /var/lib/llm-scheduler/data/scheduler.db`. It refuses if the database already has a configuration; `--replace` makes a new revision instead (the old one stays in the history). Starting the scheduler on an empty database also works: it logs `no configuration yet`, has no profiles and uses no node until you import or create them.

4. Start and check: `docker compose up -d`, then `curl -s $H/api/config | jq '{revision, configured}'` (expect `1`, `true`).

#### Change a setting (curl)

```sh
H=http://127.0.0.1:3122
curl -s $H/api/config/settings | jq .settings.ttl                      # read the effective value
curl -s $H/api/config/schema | jq '.fields[] | select(.key=="ttl")'    # type, default, bounds, description
# dry run: validate and show the diff, write nothing
curl -s -X PATCH "$H/api/config/settings?dry_run=1" -d '{"ttl":"10m"}' | jq .
# apply (If-Match makes it fail with 409 if someone else changed the configuration meanwhile)
curl -s -X PATCH $H/api/config/settings -H 'If-Match: 3' -H 'X-Actor: alex' -H 'X-Comment: longer idle time' -d '{"ttl":"10m"}' | jq .
curl -s -X PATCH $H/api/config/settings -d '{"ttl":null}'              # null resets a key to its default
```

The answer has `revision`, `audit_entry_ids`, `diff`, `warnings` and `restart_required` (keys that apply at the next start). A change that would make the configuration invalid is refused with `400` and the same messages as the old `--check-config`; nothing is written. The other write endpoints: `POST /api/config/profiles` (201, 409 if the id exists), `PUT/DELETE /api/config/profiles/{id}`, `PUT /api/config/tiers`, `PUT /api/config/exclude-nodes`, `PUT /api/config/node-overrides`, `PUT|PATCH /api/config/recovery`, `PUT|PATCH /api/config/fleet`. Deleting a profile that benchmarks refer to answers `409` with the warnings; repeat with `?force=1` to delete anyway. Requests queued for a deleted profile fail at once with `404`; a loaded instance of a changed or deleted profile keeps serving and shows `stale_profile: true` in `/api/status` until its next load.

#### Read the audit log

```sh
curl -s "$H/api/audit?limit=20" | jq '.entries[] | {id, ts, source, actor, action, section, target, revision, comment}'
curl -s "$H/api/audit?section=profiles&target=gemma-4-12b" | jq .          # one profile's history
curl -s "$H/api/audit?source=ui&since=1790000000000" | jq .total_matching     # filters: since until source actor section target action q limit order
curl -s $H/api/audit/42 | jq '{before, after, diff}'                       # what exactly changed
```

`source` is `ui`, `rest`, `mcp`, `import` or `system`, decided by the server (a client cannot claim `mcp` or `import`). Loads and unloads asked for over REST or MCP are in section `runtime` (no revision). Benchmark and suite changes, run starts and run cancels are in section `bench` (targets `benchmark:<id>`, `suite:<id>`, `run:<id>`). The loads and unloads a benchmark run makes by itself are in section `runtime` with source `system` and actor `bench run <run id>`: `curl -s "$H/api/audit?source=system"` lists exactly the scheduler's own actions, and everything a person or an agent did is the rest. The log is append-only and is kept forever unless you set `audit_retention_days`.

#### Roll back

```sh
curl -s "$H/api/config/revisions?limit=10" | jq .                  # revision, ts, source, actor, comment
curl -s $H/api/config/revisions/7 | jq .config                     # the whole configuration at revision 7
curl -s -X POST $H/api/config/revisions/7/rollback -H 'X-Comment: bad tiers' | jq .   # creates a NEW revision equal to 7
```

#### Back up and restore the database

The configuration, its revisions, the audit log and the benchmark results are all in `/opt/llm-scheduler/data/scheduler.db` (WAL mode: also `-wal` and `-shm`).

```sh
# backup, container stopped (simplest and always consistent)
cd /opt/llm-scheduler && docker compose stop && cp -a data data.bak-$(date -u +%Y%m%dT%H%M%SZ) && docker compose start
# backup while running (needs sqlite3 on the host)
sqlite3 data/scheduler.db ".backup 'scheduler-$(date -u +%Y%m%dT%H%M%SZ).db'"
# configuration only, as one JSON file (no audit log, no benchmark results)
curl -s $H/api/config/export > config-export.json
```

Restore the database: `docker compose stop`, replace `data/scheduler.db` with the backup and delete `data/scheduler.db-wal` and `data/scheduler.db-shm`, `docker compose start`. Restore a configuration export into any scheduler: `curl -s -X POST "$H/api/config/import?replace=1" -H 'X-Comment: restore' -d @config-export.json` (without `replace=1` it is refused unless the database is empty). An export from one box can set up another.

#### Migrate a Docker deployment that has `./config.yaml`

Assumes the old layout: `/opt/llm-scheduler/` with `docker-compose.yml`, `config.yaml` (mounted at `/etc/llm-scheduler/config.yaml`), `./data`, `./history`, `./logs`. Stopping the container stops every loaded model, so check `curl -s http://127.0.0.1:3122/running` first.

```sh
cd /opt/llm-scheduler
# 1. keep what is needed to go back
docker tag llm-scheduler:local llm-scheduler:previous
cp config.yaml config.yaml.pre-db-migration
cp docker-compose.yml docker-compose.yml.pre-db-migration
# 2. build the new image from the repo checkout (section B) and take the new compose file
cd ~/datacenter-gpu-orchestrator && docker build -f deploy/Dockerfile -t llm-scheduler:local --build-arg VERSION=$(git describe --always --dirty) .
cp deploy/docker-compose.yml /opt/llm-scheduler/docker-compose.yml
# 3. the old config file becomes the seed (the compose file mounts ./seed-config.yaml at /seed/seed-config.yaml)
cd /opt/llm-scheduler && cp config.yaml seed-config.yaml
# 4. stop the old container, validate, import once
docker compose down
docker compose run --rm --no-deps llm-scheduler import-config --check /seed/seed-config.yaml
docker compose run --rm --no-deps llm-scheduler import-config --actor "$USER" --comment "migration from config.yaml" /seed/seed-config.yaml
# 5. start and verify
docker compose up -d
sleep 5; curl -s http://127.0.0.1:3122/api/config | jq '{revision, configured, profiles: (.profiles|length)}'
curl -s "http://127.0.0.1:3122/api/audit?action=import" | jq '.entries[0] | {source, actor, revision}'
sh deploy/scripts/smoke.sh http://127.0.0.1:3122
```

The data volume is the same `./data`: the existing `scheduler.db` (benchmarks, runs) is migrated in place and keeps its contents; the configuration tables are added next to them. If `./data` was not mounted before, nothing is lost: the database is created. From now on edit configuration with the API, not the file; `config.yaml` is no longer read by anything.

To go back (the old image and file are untouched): `docker compose down && cp docker-compose.yml.pre-db-migration docker-compose.yml && docker tag llm-scheduler:previous llm-scheduler:local && docker compose up -d`. The old binary ignores the new tables in `scheduler.db`.

### Add a model profile (5 min)

1. Download the GGUF to `/srv/llama-swap/models` (as in the llama-swap README).
2. Create the profile (name `model:variant`; fields in design 6, `GET /api/config/schema` lists them with defaults). Validate first with `?dry_run=1`. Sliding-window model: set a measured `needs_mib` (section G); the scheduler only warns if you forget.

```sh
curl -s -X POST "http://127.0.0.1:3122/api/config/profiles?dry_run=1" -d '{"id":"qwen3.5-4b","model":"Qwen_Qwen3.5-4B-Q4_K_M.gguf","ctx":16384,"needs_mib":3400}' | jq .
curl -s -X POST http://127.0.0.1:3122/api/config/profiles -H "X-Actor: alex" -d '{"id":"qwen3.5-4b","model":"Qwen_Qwen3.5-4B-Q4_K_M.gguf","ctx":16384,"needs_mib":3400}' | jq .
```

3. No restart: it is usable at once and nothing running is stopped.
4. `sh deploy/scripts/smoke.sh http://127.0.0.1:3122` and look at the new profile's `/api/plan` line.

### Add a PS5 (no config change)

In agents mode a machine joins by running the agent ("Agent membership"
below; `docs/agent-contract.md` sections 5.4 and 9), not through the fleet
manager; check it in `agents` of `/api/status` instead of the steps here.

With fleet membership, nodes come from the fleet manager (design 4). Verify only:

1. The PS5 appears online in the fleet console: `curl -s http://127.0.0.1:8090/api/fleet` shows its `ip` and `rpc_port` (not null).
2. Its hostname matches `console-*` (the tier match). Otherwise it is **never used**.
3. Check it exposes one device (H2): `docker exec llama-swap timeout 10 llama-server-rpc --rpc <ip>:50052 --list-devices | grep -c '^ *RPC'` expects `1`.
4. `curl -s http://127.0.0.1:3122/api/status` lists it with a fresh view; `POST /api/plan` for a big profile now shows it as an option.

### Keeping a machine out of the pool

Use `exclude_nodes` when a machine must never receive a model (for example a PS5 under test) but you still want the `console-*` tier to find every other PS5. It is the `exclude_nodes` list of the configuration. Each entry is a node id (a MAC such as `02:00:00:00:00:01`) or a hostname glob. Matching ignores case.

```sh
curl -s -X PUT http://127.0.0.1:3122/api/config/exclude-nodes -H "X-Actor: alex" -d '["console-abc123", "console-test*", "02:00:00:00:00:01"]' | jq .
# one machine by hostname, every machine starting with ps5-test, one by id; the whole list is replaced
```

1. Send the new list as above (add `?dry_run=1` to check it first). It applies to the next placement decision, with no restart; models already running on that machine keep running.
2. Check `curl -s http://127.0.0.1:3122/api/status`: under `view.nodes` the machine shows `"excluded": true` and `"eligible": false`. The log has one INFO line at start listing the entries, and one `excluded node in fleet view` line the first time each excluded machine appears.
3. An excluded machine is never used, never counted in capacity checks and never pre-flighted. A plan or wait reason that involves it says `node <hostname> is excluded by config`.
4. Excluding wins over forcing. A profile whose `force_nodes` names an excluded node is refused when the configuration alone can tell. When it cannot (for example `force_nodes` uses the MAC and the entry is a hostname glob), the request fails at once with 503 `node excluded` and no `Retry-After`. Remove the entry, or the node from `force_nodes`.
5. You can exclude the local node (`host-1`). The host machine GPU is then never used, and `kind: command` profiles (Whisper, FLUX) fail with "does not fit" because they run on the local node only.
6. To bring the machine back, send the list without the entry.

### Keeping a machine for forced profiles only

Use `no_auto_nodes` when a machine may run a model only when a profile asks for it by name in `force_nodes` (for example a laptop someone works on). The entries and matching are those of `exclude_nodes`.

```sh
curl -s -X PUT http://127.0.0.1:3122/api/config/no-auto-nodes -H "X-Actor: alex" -d '["laptop-*"]' | jq .
# the whole list is replaced; add ?dry_run=1 to check it first
```

1. Automatic placement never picks the machine and never evicts what runs on it. It is not counted in capacity checks for profiles without `force_nodes`.
2. A profile that names it in `force_nodes` still starts there. The forced-fit checks still apply.
3. `exclude_nodes` wins: a machine on both lists is never used, and the configuration refuses a `force_nodes` entry that names an excluded machine.
4. Check `curl -s http://127.0.0.1:3122/api/status`: under `view.nodes` the machine shows `"auto": false`. The Nodes page shows "Forced only"; the list is edited on the Tiers and nodes page. The MCP tool is `set_no_auto_nodes`.
5. To make it automatic again, send the list without the entry.

### Benchmarking only the PS5s from a test deployment

A second copy of the scheduler can stand in on port 3122 while you benchmark the PS5s alone. Give it a config with `exclude_nodes` listing the host machine (`host-1`) and the Nvidia laptop (`laptop-1`), so only PS5s get models. Keep `forced_fit_check` off for the benchmark. The PS5 dashboards path keeps working: the hub polls `/running` on 3122, relays the `load` object and `recent_loads` to the fleet manager's `/api/models`, and shows them (verified 2026-11-18). Steps and the profile set are in `docs/benchmark-guide.md`. When done, put the production config and image back (section D, and F if you need llama-swap).

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

`load_progress_source` is `"measured"` when the scheduler counted the bytes the workers acknowledged (Linux; then `load_bytes_sent` is shown) and `"estimated"` when it worked from `link_mb_s` and the clock (the figure is then only a guide). `load_progress` stays at or below 0.99 until the model is ready. A load with no remote worker (host machine GPU only) shows `load_elapsed_s` only. `GET /running` has the same figure as `load_progress` on the entry. The scheduler log also carries a `load progress` line every 30 s per loading instance (`docker logs llama-swap | grep "load progress"`). Loads started at the same time share the host's one gigabit port (about 80 MB/s in total), so two loads each go at about half speed.

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

The profile's need (`needs_mib`, else the estimate) is compared with the forced nodes' usable memory from the pre-flight. On a `shared_memory` tier (the PS5: GPU memory is system RAM, and the pre-flight reports almost all of it as free) that is the SMALLER of the budget from the fleet-reported free figure (what the automatic placer uses) and the pre-flight free figure, each minus the tier `headroom_mib`, because the Vulkan pre-flight overstates what the machine can really give; if only one figure is known it is used, and `POST /api/plan` and a real request agree whenever both are known. On a dedicated-VRAM tier it is raw free with no headroom taken off, so a forced profile may still eat into headroom there. The message says per node which figure was used, for example `need 14691 MiB (estimated, +5% margin; the estimate is 13991 MiB), but the nodes it is forced onto have only 12380 MiB usable in total (usable 12380 MiB on console-1 (shared memory, headroom kept, from fleet figures))` (or `... from pre-flight`). Hardware fact: on llama.cpp upstream HEAD (2026-11-19) a forced load that does not fit took a PS5 down until it was power cycled (13991 MiB forced onto a PS5 with 14875 of 14947 MiB reported free), so `reject` is the recommended production setting. `warn` logs `forced profile may not fit` (with `need_mib`, `free_mib`, `nodes`) and starts anyway. `reject` fails the request at once with `503` "forced nodes do not fit", before anything is uploaded; it does not back the profile off or quarantine a node. `POST /api/plan` shows the same result (`action: reject`, or a `warning` field in `warn` mode). If the need is estimated, measure it (G) so the check is exact.

### Memory safety on shared-memory nodes (PS5s)

The PS5 workers have no container memory limit, and a load that exhausts a PS5 makes the machine unreachable for about 18 minutes, so the scheduler plans conservatively (design 7.2a). Three settings (`split_skew_margin`, `estimate_margin`, `load_memory_floor_mib`; change them with `PATCH /api/config/settings`, they apply from the next evaluation; defaults are in `GET /api/config/schema`):

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

Turn it on (the `recovery` section of the configuration, applied at once with `PUT /api/config/recovery`; shown as comments in `deploy/seed-config.yaml`). In YAML form the keys are:

```yaml
recovery:
  enabled: true
  actions: [restart_rpc_server]   # the default; escalation order
  # min_interval: 120s            # per node, at least 60s
  # step_wait: 90s                # time a step gets before the next one
  # request_ttl: 45s              # at most 60s
  # http_timeout: 3s
```

What it sends: one `POST <fleet.url>/api/recovery/<node id>` (the id percent-encoded, colons as `%3A`) with `{"v":1,"target_node_id","action","reason","issued_at","expires_at","nonce"}`, the first time a worker that backs a model turns `hung`, for any remote node an instance is using, never the host machine or an excluded node. With more actions it waits `step_wait` (at least `min_interval` in all) and sends the next one if the node is still hung; it sends at most one request per listed action per hung episode, and stops and resets when the node is healthy again. A `5xx` or timeout is retried once after 10 s with a new nonce; a `400/403/404/409/410/422/429` is not retried and **stops the escalation for that hung episode** (WARN `recovery sequence stopped`), so a refused restart is never followed by a reboot request.

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

### Event history and the dashboard

The scheduler keeps its safety and lifecycle events (loads and how they ended, stalls, low-memory cancels, lost and hung nodes, quarantines, recovery requests and answers, evictions, unloads with their reason, fleet view and feed trouble, per-minute memory of the PS5s in use) in `/opt/llm-scheduler/history` on the host machine (`--history-dir` / `LLM_SCHEDULER_HISTORY_DIR`, mounted by compose as `./history`). JSON Lines, files named `events-<UTC time>.jsonl`, rotated at 16 MiB, 20 files kept (`history_max_file_mib`, `history_keep_files`). It survives restarts and image swaps; the newest 5000 events are reloaded into memory at start. If startup logs a history directory problem the history is off until the directory is fixed and the scheduler restarted; scheduling is never affected.

Open the dashboard from any machine on the LAN: `http://<host>:3122/ui/`. It is read-only. Pick a time range at the top, read Incidents first, click a row for the full record and the other events of the same load (`session_id`).

Query it with curl:

```sh
B=http://127.0.0.1:3122
SINCE=$(( ($(date +%s) - 6*3600) * 1000 ))
curl -s "$B/api/history?severity=warn&since=$SINCE" | jq '.events[] | {ts, type, severity, summary}'
curl -s "$B/api/history?type=nodeLost,workerHealth&node=console-4&limit=50" | jq '.events[].summary'
curl -s "$B/api/history?session_id=ld-1795055840000-3&order=asc" | jq '.events[] | [.type, .summary]'
curl -s "$B/api/history/summary?since=$SINCE" | jq '{loads, profiles, nodes, incidents: (.incidents | length)}'
```

Export a range, or copy the files:

```sh
curl -s "$B/api/history/export?since=$SINCE" > history-last6h.jsonl     # the same link is on the dashboard
ls /opt/llm-scheduler/history                                              # the raw files, jq works on them too
```

`history` in the answer says `off` when no history directory is set. `truncated: true` means more events match than were returned: narrow the range or raise `limit` (at most 2000).

### Benchmarks run by the scheduler (design 9.3)

The scheduler can run benchmarks itself and keep the numbers: define test
cases ("benchmarks"), group them ("suites"), run a suite, read the results,
compare with the last run. It uses its own HTTP API on loopback, so a
benchmark loads and serves exactly like a client's request does, and it never
skips the fit checks.

1. **Turn it on.** It is on by default (the `bench_enabled` setting) and needs the database, which every deployment has (`./data:/var/lib/llm-scheduler/data` in compose). The log says `benchmark feature on database=...`. If the database cannot be opened the feature stays off (its endpoints answer `503`) and serving is not affected; fix the directory permissions and restart. Changing `bench_enabled` or the regression thresholds needs a restart (they are `restart_required`).
2. **Define.** Either put a `bench:` block in the YAML file you give to
   `import-config` (a commented example is at the end of `seed-config.yaml`;
   stored once, with an audit entry), or use the
   API:
```sh
B=http://localhost:3122
curl -s -X POST $B/api/bench/benchmarks -H 'Content-Type: application/json' -d '{
  "id": "gemma12b-1ps5-cold", "profile": "bench-gemma12b-1a-2048",
  "repeats": 3, "warmup_requests": 1,
  "requests": [{"name": "short", "prompt": "Explain what a load balancer does in two sentences.", "max_tokens": 96}]}'
curl -s -X POST $B/api/bench/suites -H 'Content-Type: application/json' -d '{
  "id": "gemma-scaling", "benchmarks": ["gemma12b-1ps5-cold"], "cooldown_s": 10}'
```
   That is the simple form. For anything else write `phases` (below).
   `curl -s $B/api/bench/schema | jq` lists the phase types, every option with
   its default, limit and unit, the `expect` keys and five templates ("Load
   only", "Load and requests", "Load and conversation", "Requests on a loaded
   model", "Simultaneous requests") to start from.

   *A load-only benchmark* (no chat request is sent; it measures the load and
   nothing else):
```sh
curl -s -X POST $B/api/bench/benchmarks -H 'Content-Type: application/json' -d '{
  "id": "gemma12b-load-1ps5", "name": "Gemma 12B load, 1 PS5",
  "description": "Cold load, then unload. Load time, upload rate, bytes and memory.",
  "profile": "bench-gemma12b-1a-2048",
  "phases": [{"type": "load"}, {"type": "unload"}],
  "expect": {"max_load_s": 120}}'
```
   *A conversation benchmark* (load, a multi-turn exchange, unload; each turn
   carries the earlier turns and replies; the result has prefill, decode, time
   to first token and reused prompt tokens per turn):
```sh
curl -s -X POST $B/api/bench/benchmarks -H 'Content-Type: application/json' -d '{
  "id": "gemma12b-conversation", "name": "Gemma 12B conversation",
  "description": "Cold load, then 3 turns with prompt caching.",
  "profile": "bench-gemma12b-1a-2048", "timeout_s": 900,
  "phases": [{"type": "load"},
    {"type": "conversation", "max_tokens": 96, "stream": true, "turns": [
      {"content": "Explain what a load balancer does."},
      {"content": "Give one example of a balancing rule."},
      {"content": "Now say both answers in one sentence."}]},
    {"type": "unload"}],
  "expect": {"min_reused_tokens": 20}}'
```
   The other phase types are `warmup` (requests left out of the statistics),
   `requests` (the measured ones), `hold` (stay loaded and idle for `seconds`,
   then one request) and `concurrent_requests` (`n` requests at the same
   moment). `phases` replaces `cold`, `requests`, `repeats`, `warmup_requests`
   and `unload_after`; the simple form keeps working (it is expanded to
   `load`, `warmup`, `requests`, `unload`). Hold seconds must fit in
   `timeout_s`.
   Unknown profiles, bad ids and unknown fields are rejected (`400` or `422`
   with the reason). A benchmark that is *supposed* to be refused (a model that
   does not fit) has `"expect": {"outcome": "refused"}`; it passes when the
   scheduler refuses.

   *A scenario you cannot run yet* (hardware not there, a link not set up) is a
   suite with `status` `planned` or `blocked` and a `reason`; it may list no
   benchmark:
```sh
curl -s -X POST $B/api/bench/suites -H 'Content-Type: application/json' -d '{
  "id": "five-consoles", "name": "Consoles only, 5", "status": "planned",
  "reason": "the fifth console is not set up yet", "benchmarks": []}'
```
   The Benchmarks page lists it with a badge and the reason, and its Run
   button is off. Running it answers `409` with the reason. Remove `status`
   and `reason` (and add benchmarks) when it can run. A planned or blocked
   suite cannot have a schedule.
3. **Run.** `curl -s -X POST $B/api/bench/suites/gemma-scaling/run -d '{"note":"after the PS5 firmware update"}'`
   returns the run (`queued`). Runs go one at a time; others queue. Cancel with
   `POST /api/bench/runs/<id>/cancel`: it stops after the current request (a
   load or a hold is abandoned at once) and unloads what the run loaded.

   *Progress.* While a run is active:
```sh
curl -s $B/api/bench/state | jq .run
# { "id": "run-...", "state": "running", "percent": 41.2, "elapsed_s": 130.5, "eta_s": 180.0,
#   "steps_total": 3, "steps_done": 1,
#   "current": { "index": 1, "benchmark_id": "gemma12b-load-1ps5", "name": "Gemma 12B load, 1 PS5",
#                "description": "...", "profile": "bench-gemma12b-1a-2048", "model_file": "...gguf",
#                "nodes": ["ps5-a"], "percent": 62.0,
#                "phase": { "index": 0, "type": "load", "label": "load", "phases_total": 2 },
#                "detail": { "text": "Loading ...: 4.1 of 6.7 GB sent", "load": { ...the scheduler load object... } } },
#   "next": { "benchmark_id": "...", "name": "...", "description": "...", "profile": "..." },
#   "queue": ["..."] }
```
   `run.percent` is the whole run, weighted by how long each benchmark took in
   earlier runs (equal weights for a benchmark never run); `current.percent` is
   this benchmark, and for a load-only benchmark it is essentially the model's
   load percent. `eta_s` is `null` until the benchmarks have run once. The same
   object arrives on `/ws` as `bench` messages with `kind: "progress"`, at most
   twice a second and on every phase change (`websocat ws://localhost:3122/ws`).
   Indexes start at 0.
4. **`only_when_idle` (default true).** Benchmarks load and unload models on the
   real cluster, so a run does not start, and pauses between benchmarks, while
   the scheduler has a model this run did not load, a request in flight that is
   not the run's, or anything queued. A model that someone used last stays loaded
   for its `ttl` (300 s by default), so a run may wait that long. After
   `idle_wait_s` (default 600) the run ends `skipped` and what it had measured
   is kept. Set `only_when_idle: false` only when you know nobody else is using
   the cluster: a cold benchmark then unloads the profile it tests even if
   someone else loaded it.
5. **Read.** `GET /api/bench/runs?suite=gemma-scaling` lists runs;
   `GET /api/bench/runs/<id>` has every step, its load numbers (load seconds,
   bytes, MB/s, per node bytes and memory, the child's log file), every request
   (phase `warmup`, `measured`, `conversation`, `hold` or `concurrent`,
   timings, output hash), the result of every phase (`steps[].phases[]`: load
   numbers, conversation turns, simultaneous requests, unload time) and the
   statistics. Warmup requests are never in the statistics. A step that was
   refused or failed is a result with the scheduler's reason, not an error.
   A load phase sends no chat request, so its history records are found by the
   load session: `GET /api/history?session_id=<steps[].load.session_id>`. The
   run also records its environment (`env`: scheduler version, configuration
   revision when the status has one, llama binary, each PS5's rpc-server
   version).

   Each step also records what tells one hardware setup from another (steps
   stored before this was added have none of it; the UI says "Not recorded"):
   `mem_low` (per node, the lowest available memory while loading and during
   the whole step, sampled about twice a second), `load.load_mode`
   (`file_read`, `pushed` or `local`), `roles` (the host and the workers, with
   each one's share of the model) and `reload_from_cache` (per node: `full`,
   `partial` with `cached_pct`, `none` or `unknown`; design 9.3, "Reload from
   cache"). `GET /api/bench/benchmarks/<id>/results` carries
   `lowest_available_mib`, `load_mode`, `host` and `workers` per run. The plan
   of a profile (`GET /api/profiles`, the Models page) shows the same reload
   figure before you load: "Reload from cache: partial, about 63% of the
   file" means the next load of that model reads about a third of the file
   from the disk or the network again.
6. **Compare.** Each run is compared with the previous completed run of the
   same suite (or the `baseline` you named when you ran it). `regression: true`
   means the load was more than 15% slower (`bench.regress_load_pct`), decode
   more than 7% slower (`bench.regress_decode_pct`), or the output of a
   temperature-0 request changed.
7. **Export.** `GET /api/bench/runs/<id>/export?format=md` is a table to paste
   into a document (plus a table of phases and of conversation turns);
   `format=csv` one row per step (`&detail=phases` one row per phase,
   `&detail=turns` one per conversation turn); `format=json` everything.
   `GET /api/bench/trends?benchmark=gemma12b-1ps5-cold&metric=load_s` gives the
   points over past runs for a chart (`metric` is `load_s`, `load_mb_s`,
   `memory_mib`, `decode_tps`, `prefill_tps`, `ttft_ms`, `unload_s`,
   `concurrent_wall_s` or `lowest_available_mib`).
   **Aggregates (the history added up):**
```sh
curl -s "$B/api/bench/aggregates?suite=gemma-scaling&since=30d" | jq '.benchmarks[] | {benchmark_id, runs, pass_rate, load_s: .metrics.load_s | {median, last, best, change_pct, regression}}'
curl -s "$B/api/bench/aggregates?benchmark=gemma12b-load-1ps5&env=same" | jq .by_model_nodes   # the same model on 1, 2, 3 workers
curl -s "$B/api/bench/aggregates" | jq .by_model_load_mode   # the same again, split by file read or pushed
```
   Filters: `suite`, `benchmark`, `profile`, `since`, `until` (unix ms, an RFC
   3339 time, or `24h`, `7d`), `limit_runs` (newest N, default 200) and
   `env=same` (only runs made in the same environment as the newest one). Per
   benchmark: runs, pass rate, outcome counts, and for `load_s`, `load_mb_s`,
   `decode_tps`, `prefill_tps`, `ttft_ms`, `memory_mib` the count, min, median,
   mean, max, standard deviation, last, best, percent change of the last run
   against the median of the earlier ones, a regression flag and the last 30
   points; per turn for conversations; per suite the state counts, pass rate,
   mean duration and last run; and the groupings `by_model` and
   `by_model_nodes`. A `bench` message `kind: "aggregate_updated"` arrives on
   `/ws` when a run ends.
8. **Schedule.** `"schedule": {"at": "03:00"}` (local time of the container,
   normally UTC) or `{"every": "24h"}` in a suite queues it by itself.
9. **Back up the database.** All results are in `/opt/llm-scheduler/data/scheduler.db`
   (plus `-wal` and `-shm` files while it runs). A consistent copy while the
   scheduler runs: `docker exec llama-swap sh -c 'command -v sqlite3'` may not
   exist in the image, so the safe ways are (a) stop the container and copy
   the three files, or (b) `curl -s $B/api/bench/runs/<id>/export?format=json`
   for the runs you care about. Deleting the file resets the feature (it is
   recreated empty at the next start; config seeds come back). The JSONL event
   history (`./history`) is separate and unchanged.
10. **A run died with the scheduler.** A run that was running when the
    container stopped is marked `failed` with `failed (interrupted)` at the next
    start; a queued one is `cancelled`. A model the run had loaded went away
    with the container.

Live channel: `ws://<host>:3122/ws` (design 9.2). From a browser it only
accepts the same origin or a LAN origin. `ws_max_clients` (32) limits clients.
Test it from a shell with any WebSocket client, for example
`websocat ws://localhost:3122/ws`.

### Let an AI agent operate the scheduler (MCP, design 9.5)

The scheduler is also an MCP server at `http://<host>:3122/mcp` (same port, no
authentication, LAN only). Every tool runs the same code as the REST endpoint
of the same name; nothing needs configuring.

Connect Claude Code:

```sh
claude mcp add --transport http llm-scheduler http://<host>:3122/mcp
```

Any other client that speaks Streamable HTTP takes a URL, for example:

```json
{
  "mcpServers": {
    "llm-scheduler": { "type": "http", "url": "http://<host>:3122/mcp" }
  }
}
```

Check it from a shell without a client (the session id comes back in the
`Mcp-Session-Id` response header; it is optional):

```sh
curl -s http://<host>:3122/mcp -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```

Three example calls an agent makes (arguments as JSON):

1. `get_status` with `{}` returns the model-centric summary: what is loaded or
   loading (state, nodes, memory, requests in flight, load progress and ETA),
   then nodes and queue.
2. `plan_load` with `{"model":"gemma-4-12b"}` says whether it fits and what it
   would evict; if the plan is good, `load_model` with
   `{"model":"gemma-4-12b","actor":"nightly-check"}` starts it (HTTP 202, the
   load takes minutes: poll `get_status`).
3. `update_settings` with `{"settings":{"queue_timeout":"2m"},"comment":"longer
   queue while benchmarking","dry_run":true}` validates a change without
   saving it; repeat without `dry_run` to apply it. `query_audit` then shows it
   with `source: mcp` and the client name, and `rollback_config` undoes it.

What the write tools send (each is one REST request; the answer is the REST
answer, errors included):

| Tool | Request |
|---|---|
| `create_profile` | `POST /api/config/profiles`, body = `profile` (with `id`) |
| `update_profile` | `PUT /api/config/profiles/{id}`, body = the whole `profile` |
| `delete_profile` | `DELETE /api/config/profiles/{id}` (`409` while a benchmark names it) |
| `update_settings` | `PATCH /api/config/settings`, body = `settings` (only the keys to change) |
| `set_tiers`, `set_exclude_nodes`, `set_no_auto_nodes`, `set_node_overrides`, `set_recovery_settings`, `set_fleet_settings` | `PUT /api/config/<section>`, body = `value` (a list for tiers, exclude_nodes and no_auto_nodes, an object for the others) |
| `rollback_config` | `POST /api/config/revisions/{revision}/rollback` |
| `create_benchmark`, `update_benchmark`, `create_suite`, `update_suite` | `POST` or `PUT /api/bench/benchmarks[/{id}]` and `/suites[/{id}]`, body = `benchmark` or `suite` |
| `run_suite`, `cancel_run` | `POST /api/bench/suites/{id}/run` `{baseline, note}`, `POST /api/bench/runs/{id}/cancel` |
| `load_model`, `unload_model` | `POST /api/load` `{model}`, `POST /api/unload` `{model, force}` |

The configuration tools also take `comment` (the audit comment), `dry_run`
(validate and report the diff, save nothing, audit nothing) and `if_revision`
(refuse with `409 stale_revision` if the configuration moved on; read it with
`get_config`, `list_config_revisions` or any write answer). Every write tool
takes `actor`, a label for the task, recorded as `<actor> (via <client>)`.
The read tools for the audit log and the results are `query_audit`,
`get_bench_aggregates` (since and until accept `24h` or `7d`; `env: "same"`
keeps only runs in the newest run's environment), `get_bench_trends`,
`get_bench_schema` and `export_run`. Benchmark tools need the database like the
REST API does, and answer `503` otherwise. An agent that should not change
anything can be given a client configuration that allows only the tools marked
read-only.

### Agent membership (potluck-agent nodes, design 7.7)

1. Put the release on the host: `programs/<release>/release.json`, `agent/`,
   one folder per platform, each with its `SHA256SUMS`. The release is the
   scheduler's own (`agentproto.Release`, 0.1.3 since fit before eviction
   and the benchmark step facts; 0.1.2 after CR-10's last batch, 0.1.1 after
   the CR-9 review, 0.1.0 before). Going to 0.1.3: make `programs/0.1.3/`
   (the platform folders can be copied unchanged; `agent/` holds the 0.1.3
   agent; `release.json` says `"release": "0.1.3"`; new `SHA256SUMS`).
   Agents of an older release are then refused with `agent_too_old` and
   update themselves; a Docker node needs the image `potluck-agent:0.1.3`
   (agent contract 5.4).
2. Start the scheduler with `--programs-dir /path/to/programs` (or
   `LLM_SCHEDULER_PROGRAMS_DIR`) and set `membership: agents` in the settings
   (restart required). The fleet feed is then not read.
3. Check `GET /api/system`: `programs.served` must be `true` and there must be
   no `host_build_error`. If `served` is false, `programs.error` names the
   missing or mismatched file. If `host_build_error` is set, `server_bin` is not
   the release's llama build and every load is refused until it is.
4. Install the agents. Each one shows up in `GET /api/status` under `agents`
   with `state: active`. A refused agent logs `agent registration refused`
   with the reason (`agent_too_old`, `potluck_too_old`, `unsupported_hardware`,
   `duplicate_id`, `foreign_process`, `removed`, `release_unavailable`).

Node actions (each needs a comment, each is in the audit log, section `nodes`):

```sh
curl -X POST localhost:3122/api/nodes/02:00:00:00:00:01/pause        -d '{"comment":"why"}'
curl -X POST localhost:3122/api/nodes/02:00:00:00:00:01/unpause      -d '{"comment":"why"}'
curl -X POST localhost:3122/api/nodes/02:00:00:00:00:01/force-unload -d '{"comment":"why"}'
curl -X DELETE localhost:3122/api/nodes/02:00:00:00:00:01            -d '{"comment":"why"}'
curl -X POST localhost:3122/api/nodes/02:00:00:00:00:01/readmit      -d '{"comment":"why"}'
```

A node that is `active` and still gets no load says why in `unplaced` (in
`agents[]` and `view.nodes[]` of `GET /api/status`, and in the MCP status):

- `agent_unreachable`: the node reaches Potluck, but Potluck cannot reach the
  node's agent. `unplaced_message` names the address and port it tried. Check,
  on the node: the firewall allows TCP from Potluck to `50060` and `50052`
  (`ufw status`; `potluck-agent-setup` adds the rules when ufw is active,
  firewalld you set yourself); the agent listens (`ss -ltn | grep 50060`).
  From the Potluck machine: `curl -m 3 http://<node>:50060/launch` must
  answer. When Potluck runs in a container on the same machine as an agent,
  that agent needs `--allow-from <container network>`. Potluck tries again
  every 15 s and logs `agent reachable` when it works; nothing has to be
  restarted. The log has one `agent unreachable` warning per failure.
- `programs_not_ready`, `model_source_unreachable`, `orphan_launch`, `silent`,
  `no_tier_match`: agent contract 11.1. `host_machine`: the agent of the
  machine Potluck runs on; it is never a remote node.

In `agents[]`, `launch_id` and `role` are what the agent reported;
`potluck_launch_id` is the launch Potluck holds for the node.

Restarting or updating Potluck ends every loaded model: the agents stop the
launches a restarted Potluck no longer knows.

Force unload fails the requests waiting for the instance on that node (the
502 says "force unload of node <id>"); use pause when you only want the node
to stop taking new loads. A node with more than one GPU of its backend shows
`extra_devices_ignored` in its `agent` status: only the first device is used.
A load whose workers are not ready within `worker_start_timeout` (default
15 s) fails with `worker_start_timeout`; raise the setting for slow nodes.

Worker launch logs are copied into `child_log_dir` as
`worker-<node name>-<launch id>.log`. A load that fails on an agent shows the
agent's reason in the 502 and in history (`workerLaunchFailed`,
`workerExited`); when the host program dies first (it aborts as soon as a
worker is gone), the scheduler reads the launches for up to 2 s and still
names the worker's reason (`watchdog`, `oom_kill`, ...). For a load with agent
workers the scheduler sets `LLAMA_RPC_FILE_LOAD` on the host `llama-server`
itself: `required` when `worker_model_dir` is on, `0` when it is off; a value
in the container's environment is not used for these loads.
`LLM_SCHEDULER_TEST_AGENT_PORT` and
`LLM_SCHEDULER_TEST_WORKER_PORT` exist for tests only; never set them.

### Central logging (CR-10; built, not deployed)

Potluck sends log lines to a log store (VictoriaLogs) and serves the store's
search page. Design: `docs/cr-10-central-logging.md`; routes, limits and
answers: agent contract 11.3.

**What is shipped, per install**

| Where | What | `source` |
|---|---|---|
| The Potluck host | Potluck's own log (what `docker logs` shows) | `scheduler` |
| The Potluck host | The output of every `llama-server` it starts, with `profile`, `instance_id` and `stream` | `host` |
| A node, bare install (also RAM-only) | The agent's log, each launch's output, the machine's journal | `agent`, `launch`, `journal` |
| A node, Docker install | The agent's log and each launch's output; the journal only with `--journal-dir` | `agent`, `launch`, `journal` |
| Clients (benchmark runner and the like) | What they push to `POST /api/logs` | `client` |

The files stay: `./logs` (child output), the launch logs on the nodes,
`docker logs`. Central logging is a copy, and the copy may drop lines when the
store is slow (a line says how many); the files never do.

**On and off**

- The compose file has the store (`potluck-logs`) and the scheduler's
  `LLM_SCHEDULER_LOGS_STORE_URL: http://potluck-logs:9428/logs`. With both,
  logging is on.
- Off for a while, no restart: `PATCH /api/config/settings` with
  `{"logs_enabled": false}`. Agents stop shipping within one heartbeat,
  Potluck stops sending its own lines, `/logs/` and `/api/logs/query`
  answer `404`, the "Logs" entry and links leave the UI. The store keeps
  what it has.
- Off for good: remove the `LLM_SCHEDULER_LOGS_STORE_URL` line, then
  `docker compose up -d llm-scheduler`, and `docker compose stop
  potluck-logs` (or remove the service). The start log says
  `central logging is off: no log store address`.
- On a node: `potluck-agent --logs off` builds no shipper at all.
- Check: `GET /api/status` has `logs` with `"enabled": true, "store": true`;
  each entry of `agents` has `logs` with the bytes Potluck accepted from the
  node and its refusals by reason.

**Reading**

- The UI's "Logs" entry opens the store's search page in a new tab
  (`/logs/select/vmui/`). Node tiles, model cards, failed loads and
  benchmark runs have "Logs" links with the query and time filled in.
- Scripts: `curl -s 'http://127.0.0.1:3122/api/logs/query?query=source:=host+_time:15m&limit=50'`.
  JSON lines; at most 10000; 30 s at most.
- The MCP tool `query_logs` does the same.
- Only reads pass: writing, deleting and the store's admin pages under
  `/logs/` answer `403`. At most 8 reads at once; one more gets `429`.

**Where the data is, retention, disk**

- The store's data is in `./logs-store` next to the compose file
  (`/opt/llm-scheduler/logs-store`). Its port is not published; only Potluck
  reaches it.
- Retention and the disk cap are **start flags of the store**, in the
  compose file: `-retentionPeriod=14d`, `-retention.maxDiskSpaceUsageBytes=20GiB`.
  The settings `logs_retention_days` and `logs_max_disk_gib` do not change
  them. Potluck reads the store's flags at start and every 10 minutes; when
  they differ from the settings, `/api/status` `logs.warnings` says so and
  the log has `central logging: logs_retention_days is ...`. To change the
  retention, edit the flag and the setting together, then
  `docker compose up -d potluck-logs`.
- The store has a memory limit of 512 MiB.

**Wipe it**

```sh
cd /opt/llm-scheduler
docker compose stop potluck-logs
sudo rm -rf ./logs-store
docker compose up -d potluck-logs
```

The store starts empty. Nothing else is touched: the files under `./logs`,
the history and the database stay.

**When the store is down or slow** scheduling does not change: agents and
clients get `503` after 5 s at most and keep their lines; Potluck's own
lines wait in an 8 MiB ring and the oldest go first. A child writing more
than 256 KiB/s has the rest skipped in the store (not in its file).

### Child output logs

The scheduler writes each child llama-server's complete stdout and stderr to a new file under `/opt/llm-scheduler/logs` on the host machine (`--child-log-dir` / `LLM_SCHEDULER_CHILD_LOG_DIR`, mounted by compose). One file per start, named `<UTC time>-<profile>-<instance id>.log`, so `ls` sorts by time. This is where the KV cache size, the memory breakdown and the exact out-of-memory error are, beyond the last 40 lines in a `502`.

```sh
curl -s http://127.0.0.1:3122/api/status | jq '.instances[] | {profile, log_file}'   # path inside the container
ls -t /opt/llm-scheduler/logs | head
```

The oldest files are deleted past `child_log_keep` (200). If startup logs a child log directory problem, the directory could not be created or written: fix its permissions or the mount, then restart; until then loads work but no files are kept. The container's own log (`docker logs`) rotates at 50 MB, 5 files.

### Phase out an alias

Alias hits are logged with the caller's address (design 8.2). Count them:

```sh
docker logs llama-swap 2>&1 | grep 'deprecated model alias used' | tail -50
```

Each hit is a WARN line `msg="deprecated model alias used"` with `alias=`, `profile=` and `caller=` (the caller's IP; `forwarded_for=` too when an `X-Forwarded-For` header is present). It is logged at most once per alias and caller per minute. When an alias has no hits for as long as you care to wait (log history is only as long as the container's, and a `docker compose down` resets it), remove it from `aliases:` in the config, validate, restart. Tell the caller first if the log shows one.

### Tune hold and timeouts

All are settings of the same names (design 6, 8.3, 13 item 6). Change one value with `PATCH /api/config/settings` (no restart), watch.

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
| `503` "forced nodes do not fit", message with need, free, the nodes and `measured` or `estimated` | `defaults.forced_fit_check` is `reject` and the profile's need is above the forced nodes' usable memory as the pre-flight saw it (PS5 and other shared-memory nodes: free minus headroom; dedicated-VRAM nodes: raw free). The message says per node which figure was used. Nothing was started; the profile is not backed off. When the message says "even with nothing loaded on it", the check ran before any eviction, on the memory the nodes would have if what is loaded there were gone: the profile can never fit those nodes, and nothing loaded was stopped for it. That answer comes when the request arrives, also while the model holding the node is younger than `min_hold` or busy; it does not come while a forced node is offline, re-registering or has no memory figure (the request waits) | Free memory on those nodes (stop what holds it), pick other nodes, or lower the need if it is only an estimate (measure it, G). `warn` would start anyway, `off` disables the check. |
| `502` "load cancelled: <node> ran low on memory during the load (available N MiB, floor M MiB)" | The memory watch (design 7.2a, 8.4) saw that PS5's available memory fall below `load_memory_floor_mib` during the load. The profile is backed off; the node is not quarantined | Find what takes the PS5's memory, check the model's size against the node (`learned_needs` in `/api/status` after a successful load), retry after the backoff. |
| A forced profile starts uploading, then the PS5 `rpc-server` crashes mid-upload (child log: "Remote RPC server crashed or returned malformed response"); the worker restarts by itself | The model does not fit the forced nodes. Measured 2026-11-18: Devstral 24B forced onto one PS5 needs about 13991 MiB against about 13.5 GB free. A `force_nodes` profile skips the fit check unless `forced_fit_check` is set | Force enough nodes (Devstral 24B fits on two PS5s: 14248 MiB measured at ctx 8192). Set `defaults.forced_fit_check: reject` or `warn` (see "Fit check for forced profiles"). The profile is backed off after the failed load; wait it out. |
| A PS5 shows `hung` (`GET /api/status`, `view.nodes[].health.state`, ERROR log `worker hung`, `workerHealth` event) | Two of its health channels are silent (design 8.4a): upload (a receiving worker got no new bytes for `worker_silence`, 30 s) and heartbeat (offline in the live fleet view for 30 s); or the heartbeat alone for 3 x `worker_silence` with no channel known ok (an idle loaded model whose node stopped reporting). The TCP connect probe (port 50052) is off by default and is not used on PS5s: an rpc-server accepts one client, so probes pile up in its listen backlog and then all time out, which made healthy PS5s read `probe: silent`; the same goes for any external TCP check of port 50052, do not add one. The incident this was built for: the PS5 ran out of memory, its `rpc-server` hung, the container stayed "running" and the machine stopped reporting. The scheduler only names and quarantines it; it does not touch the PS5 | Look at `health.channels` and `silent_for_s` to see which evidence is missing. Power cycle or restart the PS5 worker (it recovers by itself only if the process comes back), or turn on recovery requests so the fleet manager's PS5 service restarts the rpc-server for you (section I, "Recovery requests for a hung PS5"). The state returns to `ok` once no channel is silent and at least one is known ok; the log says `worker recovered`. A model loaded on it is ended by the existing rules (node loss at 30 s, load stall, load timeout), not by this one. `suspect` is one silent channel only: watch it, nothing is quarantined. `defaults.worker_probe_interval` is `0` (probe never runs, its channel is always `unknown`); do not set it above 0 for PS5 workers. |
| A load on an agent node fails with `agent_unreachable`, or the node shows `unplaced: agent_unreachable` | Potluck cannot connect to the node's agent on port 50060 (or got no answer to a start request or state read). The usual cause is a firewall on the node that drops inbound connections; the node still registers and sends heartbeats, because those go the other way | Section I, "Agent membership": open TCP 50060 and 50052 from Potluck on the node, then wait up to 15 s. `unplaced_message` in `/api/status` names the address. |
| A node stuck quarantined | Its pre-flight keeps failing: quarantine is 30 s, doubling to 10 min, reset by a successful pre-flight (design 8.4) | Run the H2/H3 commands against that node. Fix the node (worker down, busy, more than one device: pin `-d` on it, firewall). The design has no clear-quarantine command; wait up to 10 min after fixing, or restart the container (that the state is in memory only is assumed, not stated for the scheduler). |
| Pre-flight timeouts (10 s probe fails) | Another client holds that `rpc-server` (single-client; a blocked probe never returns), or the worker is silent | Look for a second head: old llama-swap still running (`docker ps -a`), a hand-run `llama-server-rpc`. Stop it. Never run two heads. |
| Fleet view stale | No byte from the fleet console for `stale_after` (60 s): console down, wrong URL, or the firewall rule is missing. Remote nodes become ineligible; the host machine still works (design 8.4) | `docker exec llama-swap curl -fsS http://host.docker.internal:8090/api/fleet`; check rule (C) and that the console runs on `:8090`. The console keeps state in memory: after its restart the view is empty until nodes report. |
| Hub panel shows nothing | Hub reads `/running`, `/upstream/{model}/props`, `/slots` and `/api/events` (design A); it finds the container by the name prefix `llama-swap` | `docker ps` name is `llama-swap`; `curl -s http://127.0.0.1:3122/running` is valid JSON with `running`. With several models loaded the panel shows only one (known, design 13 item 4). Beyond this the design is silent: look at the hub's logs. |
| A model loads on the host machine unexpectedly | Tier 3 (host machine) is used only when tiers 1 and 2 together cannot hold the model, or PS5/Nvidia laptop were ineligible (offline, quarantined, stale, `ip` null, no tier match). `kind: command` profiles (FLUX, Whisper) always run on the host machine. `qwen3.5-4b` was on the host machine before; now it goes to a PS5 first (design 13 item 3) | `POST /api/plan` for the profile and read why. Fix the node problem. To forbid the host machine: `allow_tiers: [1, 2]` on the profile. |
| A load sits at the same byte count (`load_bytes_sent` or `load.bytes_sent` does not rise; `load.stalled_s` appears after 15 s and `eta_s` disappears) | The child is hung, usually after a PS5 `rpc-server` dropped or restarted mid-upload. The scheduler gives up by itself: after `defaults.load_stall_timeout` (180 s) with no new acknowledged bytes it stops the child and answers `502` `load stalled: no bytes acknowledged by the workers for 3m0s (sent N of M)`; the profile is then backed off and `recent_loads` shows `failed` with that reason. Nodes are not quarantined. It applies only to a measured upload that is under 99% sent. | Check the worker (H2/H3 commands), then retry after the backoff. A slow but moving upload is not a stall. For a very slow link raise `load_stall_timeout` (default or per profile); `0` turns it off |
| Load timeout on a cold PS5 | Timeout is `120 s + 2 x (share of model file / link_mb_s)`; `link_mb_s` for the PS5 tier is 80, measured 2026-11-18 as the total out of the host machine (design 3.2, 7.6): 5 GB takes about 1 min, and loads that run at the same time share that rate (three Gemma 12B loads at once took 246 s each). Waiters get `502` with the child's last log lines; the profile is backed off 60 s, doubling to 10 min | Set `load_timeout` on the profile above the load time you measured (H check 6), or correct `link_mb_s`. Backoff clears when the profile next reaches ready; during it requests fail with `503`, so wait it out. |

---
