# Potluck runbook

What an operator does to install, run and repair a Potluck deployment. How the parts work is in [architecture.md](architecture.md); the first install from nothing is in the [README](../README.md).

Conventions:

- Commands run on the Potluck host unless a step says "on the node".
- `H=http://127.0.0.1:3122` in every example.
- There is no authentication. Add `-H 'X-Actor: your-name'` and `-H 'X-Comment: why'` to a write so the audit log says who and why.
- `jq` makes the JSON answers readable; it is optional.

| What | Where |
|---|---|
| Compose project | `/opt/potluck/` (project `potluck`) |
| Containers | `potluck` (scheduler), `potluck-share` (model share), `potluck-logs` (log store); optional `potluck-agent` |
| Data | `POTLUCK_ROOT`, default `/srv/potluck`: `data`, `history`, `child-logs`, `programs`, `logs-store` |
| Model files | The folder the `models` volume binds, at the bottom of the compose file |
| Ports | 3122 (API, UI, agents), 2049 (share, on the pool address only) |

---

## A. Before you start

1. Docker works and starts at boot: `docker ps` and `systemctl is-enabled docker`.
2. The NFS modules load at boot: `lsmod | grep -w nfsd`. If not, list `nfs` and `nfsd` in a file under `/etc/modules-load.d/`.
3. The Nvidia runtime works in containers: `docker run --rm --gpus all <your base image> nvidia-smi`.
4. The model files named by your profiles exist in the model folder and are readable by everyone.
5. A program release is ready for the scheduler's release (section E).

---

## B. Build the image and install

1. Build from the repository root:

```sh
docker build -f deploy/Dockerfile -t llm-scheduler:local \
  --build-arg BASE_IMAGE=<your base image> \
  --build-arg VERSION="$(git describe --always --dirty)" .
```

If the build stops with `base image has neither curl nor wget`, the base image cannot run the health check. Use a base image that has one of them.

2. Check the binary:

```sh
docker run --rm --entrypoint llm-scheduler llm-scheduler:local --version
```

3. Install the deployment files:

```sh
sudo mkdir -p /opt/potluck /srv/potluck
cp deploy/docker-compose.yml deploy/seed-config.yaml deploy/share-nfs.conf /opt/potluck/
```

Create `/opt/potluck/.env` with `POTLUCK_SHARE_BIND` (the host's address on the pool network) and `POTLUCK_SHARE_NET` (that network). Add `POTLUCK_ROOT=/path` to keep the data elsewhere. Set the model folder in the `models` volume of the compose file. Then check the file: `cd /opt/potluck && docker compose config -q` prints nothing when it is valid.

### First install and import

The configuration lives in the database. The seed file is read once, by `import-config`.

1. Edit `/opt/potluck/seed-config.yaml`: `local_node`, `tiers`, `profiles`. Every field is explained in `configs/example.yaml`.
2. Validate. Nothing is written:

```sh
cd /opt/potluck
docker compose run --rm --no-deps potluck import-config --check /seed/seed-config.yaml
```

Expected: `valid: N profiles, N tiers, ...`. An error lists every problem. `--no-model-check` skips the model file check on a machine without the files.

3. Import it as revision 1:

```sh
docker compose run --rm --no-deps potluck import-config --actor "$USER" /seed/seed-config.yaml
```

It refuses when the database already holds a configuration; `--replace` writes a new revision instead. Starting on an empty database also works: the scheduler logs `no configuration yet` and loads nothing until profiles exist.

---

## C. Start and check

```sh
cd /opt/potluck && docker compose up -d
docker compose ps                                        # potluck healthy, potluck-share and potluck-logs running
curl -fsS $H/health
curl -s $H/api/config | jq '{revision, configured}'      # 1, true
curl -s $H/api/system | jq '.programs'                   # served: true, no host_build_error
```

A dry smoke test that starts nothing, from the repository checkout:

```sh
CONFIG=/opt/potluck/seed-config.yaml sh deploy/scripts/smoke.sh $H
```

Expected: `PASS` lines, one plan line per profile, `SMOKE PASSED`. Add `--load <profile>` to send one real chat request, which loads the model and prints its memory per device. Unload everything idle afterwards with `curl -s -X POST $H/api/models/unload`.

---

## D. Nodes

### Add a node

On the node, as root, with the setup script from `deploy/agent/`:

```sh
sh potluck-agent-setup --docker --potluck 192.0.2.1:3122 --model-source share:192.0.2.1:/
```

- `--docker` runs the agent in a container (needed for Nvidia nodes today). `--bare` installs a systemd service.
- `--model-source`: `share:<host>:/` mounts the share; `dir:<path>` reads a folder on the node; `push` receives the weights over the worker connection.
- A RAM-only node runs `potluck-starter.sh` at every boot instead. It reads `POTLUCK_URL` and optional settings from `/etc/potluck/starter.env`.
- On the Potluck host itself, the agent needs `--allow-from 172.30.0.0/24`, because the scheduler's requests come from its container network. The compose file has an optional host agent for this (`docker compose --profile local-agent up -d`); it is not tested on real hardware.

The node must accept TCP `50060` and `50052` from Potluck. The script adds ufw rules when ufw is active, prints the rules for firewalld, and does nothing with `--no-firewall`.

Check: `curl -s $H/api/nodes | jq '.nodes[] | {name, state, placeable, unplaced}'`. A new node shows `state: active` within a few seconds.

### Why a node gets no load

An active node that gets no load says why in `unplaced` (in `/api/nodes`, in `agents` and `view.nodes` of `/api/status`, and in the MCP status), with `unplaced_message`:

| `unplaced` | Meaning | Fix |
|---|---|---|
| `agent_unreachable` | Potluck cannot connect to the agent on 50060. The node can still register, because that goes the other way. | Open TCP 50060 and 50052 from Potluck on the node (`ufw status`). Check the agent listens: `ss -ltn | grep 50060`. From the host: `curl -m 3 http://<node>:50060/launch` must answer. Potluck retries every 15 s and logs `agent reachable`. |
| `programs_not_ready` | The agent has not fetched or verified its programs. | Read `programs_error` in the node's `agent` status. |
| `model_source_unreachable` | The node cannot read the model files. | Check the share mount on the node and the `potluck-share` container. |
| `no_tier_match` | No tier matches the node. | Add a tier, or fix its match (`platform`, `device`, `hostname`). |
| `paused` | The node is paused. With `unpause_pending: true`, the unpause reaches the agent with its next heartbeat. | Unpause it. |
| `silent` | No heartbeat for 10 s from a node that runs a launch. | Wait: it is lost at 20 s or comes back. |
| `orphan_launch` | The node reports a launch Potluck does not hold. Potluck stops it. | Wait for the next heartbeat. |
| `bad_floor` | The agent's watchdog floor is larger than half its memory or 16384 MiB. | Reinstall the agent with a sensible `--watchdog-floor-mib`. |
| `host_machine` | The agent on the Potluck host itself. It is never a remote node. | Nothing. |

A refused agent logs `agent registration refused` with the reason: `agent_too_old`, `potluck_too_old`, `unsupported_hardware`, `duplicate_id`, `foreign_process`, `removed` or `release_unavailable` (agent contract 4.2).

### Pause, force unload, remove

Each action needs a comment and goes to the audit log (section `nodes`). The node id is its MAC address.

```sh
curl -s -X POST $H/api/nodes/02:00:00:00:00:01/pause        -d '{"comment":"why"}'
curl -s -X POST $H/api/nodes/02:00:00:00:00:01/unpause      -d '{"comment":"why"}'
curl -s -X POST $H/api/nodes/02:00:00:00:00:01/force-unload -d '{"comment":"why"}'
curl -s -X DELETE $H/api/nodes/02:00:00:00:00:01            -d '{"comment":"why"}'
curl -s -X POST $H/api/nodes/02:00:00:00:00:01/readmit      -d '{"comment":"why"}'
```

- **Pause**: no new load; a running load continues. Kept across restarts. A new request for a profile forced onto a paused node is refused at once (503 `forced nodes unavailable`, "paused by an operator", Retry-After 30 s); a request that was already waiting keeps waiting and is served after the unpause.
- **Force unload**: stops every model that uses the node now. Requests waiting for it fail with a 502 that names the force unload. Use pause when the node should only stop taking new loads.
- **Remove**: for a machine that is gone. It is refused at registration until it is re-admitted.
- **Uninstall** (on the node): `potluck-agent-uninstall` deregisters the node and removes everything the setup put there.

### Keep a node out of the pool

Two node lists. Each entry is a node id or a hostname glob; matching ignores case; the whole list is replaced; add `?dry_run=1` to check first.

```sh
curl -s -X PUT $H/api/config/exclude-nodes -d '["console-test*", "02:00:00:00:00:01"]'
curl -s -X PUT $H/api/config/no-auto-nodes -d '["laptop-*"]'
```

- `exclude_nodes`: never used, not counted in capacity. Status and `GET /api/nodes` show `excluded: true`, `excluded_by` (the entry), `placeable: false` and `unplaced: excluded`; the agent keeps reporting. A profile that forces an excluded node is kept and refused at run time (503, reason `node excluded`, naming the entry) until the exclusion is lifted; the configuration write that creates the combination is accepted and lists each such profile in `warnings` and `blocked_by_exclusion`, and `GET /api/profiles` marks the profile with `blocked_by_exclusion: <entry>`. A benchmark step of such a profile ends `refused_by_exclusion` with the result `not_judged`: neither a pass nor a failure, and out of the pass rates. An automatic profile short of room because nodes are excluded gets the ordinary refusal and counts as a failure. Excluding a node does not drain it: a model already loaded there keeps serving until you unload it; the write lists it in `loaded_on_excluded_nodes`, and the node shows `running_while_excluded`. Unload the profile (`POST /api/unload`) to empty the machine.
- `no_auto_nodes`: used only by profiles that name the node in `force_nodes`; automatic placement never picks it or evicts from it. Status shows `auto: false`. A `command` profile runs on the local node alone by definition, so `no_auto_nodes` does not apply to it; an excluded local node refuses it (reason `node_excluded`).
- Excluding wins over both forcing and `no_auto_nodes`. Models already running keep running.

### Measure a soft restart

1. Pause the node and wait until nothing runs on it.
2. Announce the restart:

```sh
curl -s -X POST $H/api/nodes/<id>/expect-restart -H 'Content-Type: application/json' \
  -d '{"kind":"soft","comment":"measure a soft restart"}'
```

`409` says why it was not taken (not paused, or a launch is still on it). It holds for 10 minutes. Meanwhile the node's absence is logged as INFO, not as the ERROR "agent lost".

3. Restart the node: `sudo systemctl soft-reboot` on it (or `"kind":"full"` and a reboot).
4. Read the record: `curl -s "$H/api/nodes/<id>/startups?limit=1"` shows `requested: true`, `away_s` and, once ready, `headline_s` and `away_to_ready_s`.
5. Unpause the node.

---

## E. Program releases

The scheduler serves one release, the one it was built as (`programs.release` in `/api/system`). Its folder is `POTLUCK_ROOT/programs/<release>/`: `release.json`, `agent/`, one folder per platform, each folder with its `SHA256SUMS`. The rules are in agent contract 5.2.

Make the `SHA256SUMS` of a folder inside that folder:

```sh
cd /srv/potluck/programs/0.1.9/linux-amd64-cuda && sha256sum llama-server rpc-server node_exporter nvidia_gpu_exporter > SHA256SUMS
```

Check after a start: `curl -s $H/api/system | jq .programs`. `served: false` comes with `programs.error`, naming the missing or wrong file. `host_build_error` means the scheduler's own `llama-server-rpc` is not the release's llama build: every load is refused until it is.

### Move to a new release

1. Make `programs/<new>/`. Platform folders whose programs did not change can be copied. `agent/` holds the new agent; `release.json` names the new release; write new `SHA256SUMS`. Leave the previous release's `release.json` in place (section G): stored memory needs measured under it stay trusted only through it.
2. Build and start the scheduler of the new release (section F). This ends every loaded model.
3. Agents of the old release are refused with `agent_too_old` and update themselves. A Docker node exits and waits: run `potluck-agent-setup --docker ...` on it again, which builds the image for the new release.
4. Check that every node is back: `curl -s $H/api/nodes | jq '.nodes[] | {name, state, release}'`.

---

## F. Update or roll back the scheduler

Updating or restarting the scheduler ends every loaded model. Check nothing is mid-generation first: `curl -s $H/running`.

```sh
docker tag llm-scheduler:local llm-scheduler:previous          # the way back
docker build -f deploy/Dockerfile -t llm-scheduler:local --build-arg BASE_IMAGE=<your base image> \
  --build-arg VERSION="$(git describe --always --dirty)" .
cd /opt/potluck && docker compose up -d potluck
```

The agents register again by themselves. Models load again on the next request.

**Canary with a paused console.** When a roll-out pauses a console on the live scheduler (to point it at a throwaway scheduler for the canary), every new request for a profile forced onto that console is refused at once with Retry-After for as long as the pause lasts: for example, all the `*-6ps5` profiles. Requests that were already waiting keep waiting. So pause benchmark traffic for those profiles during the canary. The bench runner does not retry: it records such a step as `refused`, with the reason "paused by an operator", so a report shows why.

To roll back: `docker tag llm-scheduler:previous llm-scheduler:local && docker compose up -d potluck`.

What an older image does with what a newer one wrote (images after 0.1.9):

- **Configuration fields it does not know** (a new profile field, a new setting): it ignores them and serves everything else. It logs one WARN per field. `GET /api/system` lists them under `stored_config.unknown_fields`. While they are there, every configuration write answers `409 unknown_stored_fields`, because the write would drop them. Write with the newer image, or repeat with `?force=1` (`import-config --force`) to write anyway and drop them.
- **A database a newer image migrated** (a higher schema version), or a database or stored configuration it cannot use for any other reason: it does not start empty. It stays up, loads nothing and writes nothing. `/health` and every route answer `503` with the reason, for example `database is newer than this version: 6 > 5`. Start the image that wrote the database, or restore a backup this image can read (section H).
- **No database file yet** (a first start): it creates one and starts unconfigured, healthy.

Images up to 0.1.9 instead start healthy with built-in defaults and no profiles in both cases. Check `/api/system` and the profile count after any rollback to one of them.

---

## G. Measure `needs_mib`

The estimate from the model file is cautious, and several times too high for models with sliding-window attention (Gemma). Such a profile needs a measured `needs_mib`: the whole footprint (weights, KV cache, compute buffers), with no margin added. The scheduler warns at start about a sliding-window profile without one.

1. Create a measuring profile with the same `model`, `ctx`, `parallel`, `kv_quant` and `flash_attn` as the profile you size, and `force_nodes` set to the nodes it will really run on (the compute buffer is per device). The commented `gemma-measure` template at the end of `seed-config.yaml` shows the fields.

```sh
curl -s -X POST $H/api/config/profiles -H 'X-Comment: measure gemma 32k' \
  -d '{"id":"gemma-measure","model":"gemma-3-12b-it-Q4_K_M.gguf","ctx":32768,"kv_quant":"q8_0","flash_attn":true,"force_nodes":["console-1","laptop-1"]}'
```

2. Measure, from the repository checkout. The script loads the profile, waits for ready and prints the memory per device, the sum and a `needs_mib` line:

```sh
sh deploy/scripts/measure-needs.sh $H gemma-measure
```

3. Put the number in the real profile (`PUT /api/config/profiles/{id}`), unload, and delete the measuring profile.

A successful load also teaches the scheduler the measured size (`learned_needs` in `/api/status`). It is kept across restarts by the shape of the load (kind, model file, context, slots, KV cache type, flash attention, extra arguments) and the number of devices it ran on, not by profile name: profiles of the same shape share it, and deleting, renaming or changing a profile removes nothing. Every device holds its own compute buffer, so a measurement on one node is not used as is for three. The need on k devices is never below a measured total on fewer devices plus the estimate's per-device cost for each extra device, nor below a measured total on more devices less that cost for each missing one; with nothing measured on k or fewer devices it is the total measured on the closest count above, as it was taken. Above the largest measured count the estimate still counts. This applies with every reservation setting off too: a placement on more devices than were measured can queue where it used to start, and a count with its own measurement uses it even when another count measured more. After a restart, a stored figure is not lowered by the first load until that load ends normally (an unload, ttl, eviction or shutdown).

A measurement records the llama build it was made on. A stored measurement is trusted only when that build is both the scheduler's own `llama-server-rpc` build and the build the served release's `release.json` names (what the workers run). Otherwise it only ever raises the need, until a load measures it again. When either build cannot be read at start (a share that is slow right after a boot, a `release.json` briefly unreadable), nothing stored is trusted, and the start logs a warning saying which. Both are then read again in the background (after 5 s, doubling up to every 5 minutes) until they are known; the log line "llama builds known: stored memory needs of this build are trusted" says when trust was decided, and no restart is needed. `GET /api/needs` shows `host_build`, `release` and `served_build` ("" while not known), and for each row that is not trusted, `untrusted_reason`. Measurements made before builds were recorded (0.1.5 and earlier) carry only their release: at start they are stamped with the current build, and trusted, when their release's `release.json` in the programs folder names that same build (log line "stored memory needs stamped with the current llama build"); never by release name alone. Keep the previous release's `programs/<release>/release.json` in place at a rollout (the file alone is enough); without it those measurements count only above the estimate until each profile loads again.

List every stored measurement with `curl -s $H/api/needs` (shape, device count, total, who measured it, when, release, llama build, `trusted`, and the profiles that use it now). It is dropped when the model file's size changes; a model file replaced by one of the same size is not detected, so forget it by hand: for every profile of that shape with `curl -s -X POST $H/api/forget-need -d '{"model":"<profile>"}'` (the reply lists those profiles in `profiles_of_shape`), or by shape with `curl -s -X POST $H/api/needs/forget -d '{"shape":"<shape from /api/needs>"}'`.

Rolling back to a release before shape keys (0.1.5 and earlier) keeps the measurements: the older release reads its own table, which is left in place and also receives every new measurement under the profile that made it. It sees, for each profile, the highest figure of that profile's shape over every device count, written when that profile last measured; a profile whose shape was measured only by another profile falls back to its estimate until it loads once. Rolling forward again reads what the older release wrote; the higher figure wins.

---

## H. Configuration

All configuration is in `scheduler.db`. Every change is a numbered revision with an audit entry, and applies at once unless the schema marks it `restart_required`.

### Change a setting

```sh
curl -s $H/api/config/settings | jq .settings.ttl                      # the current value
curl -s $H/api/config/schema | jq '.fields[] | select(.key=="ttl")'    # type, default, bounds, description
curl -s -X PATCH "$H/api/config/settings?dry_run=1" -d '{"ttl":"10m"}'  # validate and show the diff
curl -s -X PATCH $H/api/config/settings -H 'If-Match: 3' -d '{"ttl":"10m"}'
curl -s -X PATCH $H/api/config/settings -d '{"ttl":null}'              # back to the default
```

`If-Match` makes the write fail with `409` when someone changed the configuration meanwhile. An invalid change is refused with `400` and nothing is written. The other sections: `POST /api/config/profiles`, `PUT|DELETE /api/config/profiles/{id}`, `PUT /api/config/tiers`, `PUT /api/config/exclude-nodes`, `PUT /api/config/no-auto-nodes`, `PUT /api/config/node-overrides`. A changed or deleted profile's loaded instance keeps serving, marked `stale_profile: true`, until it unloads.

### Add a model

1. Copy the GGUF file into the model folder.
2. Create the profile, after a dry run:

```sh
curl -s -X POST "$H/api/config/profiles?dry_run=1" -d '{"id":"qwen3-4b","model":"Qwen3-4B-Q4_K_M.gguf","ctx":16384}'
curl -s -X POST $H/api/config/profiles -d '{"id":"qwen3-4b","model":"Qwen3-4B-Q4_K_M.gguf","ctx":16384}'
```

3. It is usable at once. `curl -s -X POST $H/api/plan -d '{"model":"qwen3-4b"}'` shows where it would go.

### Read the audit log

```sh
curl -s "$H/api/audit?limit=20" | jq '.entries[] | {id, ts, source, actor, action, section, target, revision}'
curl -s "$H/api/audit?section=profiles&target=qwen3-4b"
curl -s $H/api/audit/42 | jq '{before, after, diff}'
```

`source` is `ui`, `rest`, `mcp`, `import` or `system`. Loads and unloads asked for over REST or MCP are in section `runtime`. The scheduler's own actions (benchmark loads and unloads) have source `system`.

### Roll back

```sh
curl -s "$H/api/config/revisions?limit=10"
curl -s $H/api/config/revisions/7 | jq .config
curl -s -X POST $H/api/config/revisions/7/rollback -H 'X-Comment: bad tiers'   # a new revision equal to 7
```

### Backup and restore

The configuration, revisions, audit log, benchmarks and learned needs are in `/srv/potluck/data/scheduler.db`, with its `-wal` and `-shm` files.

```sh
cd /opt/potluck && docker compose stop potluck && sudo cp -a /srv/potluck/data /srv/potluck/data.bak-$(date -u +%Y%m%dT%H%M%SZ) && docker compose start potluck
curl -s $H/api/config/export > config-export.json                       # configuration only
```

Restore the database: stop `potluck`, replace `scheduler.db`, delete `scheduler.db-wal` and `scheduler.db-shm`, start it. Restore a configuration export into any scheduler: `curl -s -X POST "$H/api/config/import?replace=1" -d @config-export.json`.

---

## I. Day-2

### Read the status

```sh
curl -s $H/api/status
curl -s $H/api/nodes
curl -s $H/running
```

- `/api/status`: `instances` (each with `profile`, `state`, `nodes` with memory per device, `log_file`), `queue`, `backoffs`, `recent_loads` (loads that finished in the last 120 s), `learned_needs`, `agents`, `view.nodes`, `logs`.
- `/api/nodes`: the node feed for dashboards: `{time, seq, nodes}`. Each entry is an entry of `agents` of `/api/status`, plus Potluck's own machine with `"local": true`. A new `seq` means something changed besides ages and uptimes.
- `/running`: the loaded models, llama-swap compatible, with `recent_loads`.

### Watch a load

```sh
curl -s $H/running | jq '.running[] | select(.state=="starting") | .load'
```

The `load` object has `elapsed_s`, `bytes_total`, `bytes_sent`, `progress`, `eta_s`, `source` (`measured` or `estimated`) and one entry per worker with its state (`waiting`, `receiving`, `done`). When bytes stop rising for more than 15 s, `eta_s` disappears and `stalled_s` appears. After `load_stall_timeout` (180 s by default) the load fails.

### Start-up figures

```sh
curl -s $H/api/startups
curl -s "$H/api/nodes/<id>/startups?limit=5"
```

The figure is the seconds from boot start to ready for model loads, grouped per node and kind of start (`full`, `soft`, `agent_restart`). Never compare across kinds. A record without a figure says which condition never came (`missing`) or what held the node back (`blocked_by`). A Docker agent reports full starts and agent restarts only.

### Fit check for forced profiles

A profile with `force_nodes` skips the automatic placer. On agent nodes Potluck still checks each node's share of the need against its usable memory, and the agent refuses a launch that does not fit (`would_not_fit`). A forced profile that can never fit its nodes, even with nothing loaded on them, is refused at once with `503` "forced nodes do not fit", and nothing loaded is stopped for it. The setting `forced_fit_check` (`off`, `warn`, `reject`) adds the same check on nodes without an agent.

### Memory safety on shared-memory nodes

A node whose GPU uses system memory freezes when a load takes all of it, so the scheduler plans conservatively. Three settings (defaults in `GET /api/config/schema`):

- `split_skew_margin` (0.15): a split model does not land evenly, so each shared-memory node's share must fit with this margin.
- `estimate_margin` (0.05): added to every estimated need.
- `load_memory_floor_mib` (768): a load is cancelled when a node's available memory falls below this. Waiting requests get `502` "load cancelled: <node> ran low on memory during the load". `0` turns it off.

The agent's own watchdog (`--watchdog-floor-mib`, default 1000) kills a launch whose node falls below its floor.

#### Tightening or loosening the reservation

The memory reservation rule is off by default. It is changed at run time like any setting, and `GET /api/system` shows what is in force under `reserve`:

- `fill_reserve_kib_per_token` (0, recommended 8): every node of a placement keeps this x the profile's context for the growth while the conversation fills the context (180 to 460 MiB per node was measured). A stored peak (the most a node gave up for the profile in its latest load-to-unload cycle that ended normally) can raise that figure, never lower it. One spike holds until the next normal cycle ends; to clear it at once, forget the learned need (`POST /api/forget-need`). A peak above need + fill reserve means the fill reserve is too small for that profile: it is worth raising `fill_reserve_kib_per_token` rather than relying on the peak, since one short normal cycle (a force unload counts as normal) replaces the peak and the profile is back to need + fill reserve.
- `placement_uses_reserved` (off): place on the smaller of the latest and the 30 s average free memory (`free_average_window`), and count what is placed on a node at the larger of its reserved figure and what the node measures (this lowers the figure shown for a held node; held nodes are not placed on). The average is not used for one window after a node's model changes. A budget is never above the one the default rules give. With any reservation setting on, every placement is held to the plan's `memory` rows, so the plan's `fits` is the decision (`decides: true`).
- `margin_measured_mib` (unset): per-node margin for a measured need, in place of `split_skew_margin`. `margin_estimated_percent` with `margin_estimated_min_mib` (unset): the same for an estimate.
- `margin_refuses` (off): with it off, the margin is not a reason to refuse. A placement whose nodes hold what the load will take (weights + context at the full configured window + fill reserve, or the stored peak) with the headroom kept starts even when the margin does not fit on top: a **tight fit**. The plan says `tight_fit: true` and "tight fit: N MiB spare on <node>, margin M MiB not met", each such node in `memory` has `tight: true`, `spare_mib` and `margin_short_mib`, the load logs a WARN line, and its `loadStarted` event carries `tight_fit`. The margin still orders placements: an automatic profile takes a placement that meets it everywhere when there is one, and nothing loaded is stopped only to meet it. A profile short even without the margin is refused ("short by N MiB on <node> even without the margin"). Turn it on to have the margin refuse again, as it did before tight fits.
- `reserved_drift_warn_mib` (500): a node that uses this much more than is reserved on it is flagged in the node view.

To tighten, raise the fill reserve or the margins; to loosen, lower them or set them back to 0. Check a profile before and after with `POST /api/plan`: its `memory` section shows per node what it will take, what the node has and what remains.

```sh
curl -s -X PATCH $H/api/config/settings -H 'X-Comment: reserve on' -d '{"fill_reserve_kib_per_token": 8, "placement_uses_reserved": true, "margin_measured_mib": 300}'
```

### Event history

Loads and how they ended, stalls, low-memory cancels, lost nodes, evictions and unloads are kept in `POTLUCK_ROOT/history` as JSON Lines (rotated at 16 MiB, 20 files kept). The web UI shows them.

```sh
SINCE=$(( ($(date +%s) - 6*3600) * 1000 ))
curl -s "$H/api/history?severity=warn&since=$SINCE" | jq '.events[] | {ts, type, severity, summary}'
curl -s "$H/api/history?session_id=<load session id>&order=asc"
curl -s "$H/api/history/export?since=$SINCE" > history-last6h.jsonl
```

### Child output logs

Each `llama-server` writes its full output to `POTLUCK_ROOT/child-logs/<UTC time>-<profile>-<instance id>.log`. Worker launch logs are copied next to them as `worker-<node name>-<launch id>.log`. These hold the KV cache size, the memory breakdown and the exact out-of-memory error. The oldest files go past `child_log_keep` (200). `log_file` in `/api/status` names the current file.

### Central logging

Potluck, its `llama-server` children, every agent, each launch and each node's journal send log lines to `potluck-logs`. The files above stay; the store is a copy that may drop lines when it is slow.

- **Read**: the UI's "Logs" entry, or `http://<host>:3122/logs/`. Scripts: `curl -s "$H/api/logs/query?query=source:=host+_time:15m&limit=50"`. MCP: `query_logs`. Only reads pass; at most 8 at once.
- **Check**: `curl -s $H/api/status | jq .logs` shows `enabled` and `store`, and warnings.
- **Off for a while**: `curl -s -X PATCH $H/api/config/settings -d '{"logs_enabled":false}'`. Agents stop shipping within one heartbeat; `/logs/` answers 404.
- **Off for good**: remove the `LLM_SCHEDULER_LOGS_STORE_URL` line from the compose file, `docker compose up -d potluck`, then `docker compose stop potluck-logs`.
- **On a node**: `potluck-agent --logs off` ships nothing.
- **Retention and disk cap** are start flags of the store in the compose file (`-retentionPeriod=14d`, `-retention.maxDiskSpaceUsageBytes=20GiB`). Keep the settings `logs_retention_days` and `logs_max_disk_gib` equal to them; `/api/status` warns when they differ. After a change: `docker compose up -d potluck-logs`.
- **Wipe**: `docker compose stop potluck-logs && sudo rm -rf /srv/potluck/logs-store && docker compose up -d potluck-logs`.

### Benchmarks

The scheduler runs benchmarks through its own API and keeps every result in the database. Define benchmarks and suites, run them, read and compare the runs: [benchmark-guide.md](benchmark-guide.md). The web UI has a Benchmarks page.

### AI agents (MCP)

The MCP server is at `http://<host>:3122/mcp` (Streamable HTTP, no authentication). Each tool runs the REST request of the same name, with the same audit trail (source `mcp`).

```sh
claude mcp add --transport http potluck http://<host>:3122/mcp
```

Useful tools: `get_status`, `plan_load`, `load_model`, `update_settings` (with `dry_run`), `query_audit`, `rollback_config`, `query_logs`. Give an agent that must not change anything only the read-only tools.

### Phase out a model alias

Profiles can have `aliases`. Each use is logged at most once per alias and caller per minute:

```sh
docker logs potluck 2>&1 | grep 'deprecated model alias used' | tail -50
```

When an alias has had no hits for long enough, remove it from the profile.

### Tune hold and timeouts

All are settings; change them with `PATCH /api/config/settings` and watch.

| Symptom | Setting | Direction |
|---|---|---|
| Alternating models spend all their time loading | `min_hold`, `max_hold` | raise |
| Requests for another model wait too long behind a protected model | `max_hold` | lower |
| A busy model is never evicted for a waiting request | `drain_after` | lower |
| `503` after waiting too long | `queue_timeout` | raise |
| A stream is cut mid-answer | `stall_timeout` | raise (`0` = off) |
| Idle models stay loaded too long | `ttl` | lower |
| A hung load is given up too late, or a slow upload is cut | `load_stall_timeout` (also per profile) | lower or raise |
| Cold loads time out | profile `load_timeout`, tier `link_mb_s` | raise or correct |
| Workers of slow nodes do not start in time | `worker_start_timeout` | raise |

---

## J. Troubleshooting

First look: `curl -s $H/api/status` and `docker logs --tail 100 potluck`. For one profile, `curl -s -X POST $H/api/plan -d '{"model":"NAME"}'` says what would happen now and changes nothing.

| Symptom | Cause | Fix |
|---|---|---|
| `503` "cluster busy", `Retry-After: 30` | The request waited `queue_timeout` and no node could be freed: every usable node is held, claimed or settling, and held models are still protected. | Retry. Look at `queue` in `/api/status`. Free a node: `POST /api/models/unload/{model}` (idle only). Tune hold (I). |
| `503` "does not fit", with need and capacity | The need is larger than the best `max_pool` nodes could hold even if empty. | Is `needs_mib` right (G)? Is a node offline or unplaced? Compare the need with the node figures in `/api/status`. |
| `503` "forced nodes unavailable" after about 30 s | A node in `force_nodes` has been absent from the pool for `forced_node_grace`. | Bring the node back or fix the name in `force_nodes`. |
| `503` "forced nodes do not fit" | The forced nodes' usable memory is below the need. "even with nothing loaded on it" means it can never fit. Nothing was started. | Force more or other nodes, or measure the need (G). |
| `502` with `would_not_fit` | The agent refused the launch: the node's share of the need does not fit its memory. | Free memory on the node, or measure the need (G). |
| `502` "load cancelled: <node> ran low on memory during the load" | Available memory fell below `load_memory_floor_mib` during the load. The profile is backed off. | Find what uses the node's memory. Retry after the backoff. |
| `502` with `watchdog` or `oom_kill` | The worker ran out of memory. The learned need is raised. | Retry: the next placement uses the larger need. |
| `502` "load stalled: no bytes acknowledged by the workers" | A worker stopped taking bytes for `load_stall_timeout`. The profile is backed off. | Check the worker's node and its launch log in `child-logs`. Retry after the backoff. |
| `502` "load timeout" | The load took longer than `120 s + 2 x (each worker's share of the file / link_mb_s)`. The profile is backed off 60 s, doubling to 10 min. | Set `load_timeout` on the profile above the measured load time, or correct the tier's `link_mb_s`. |
| `502` `worker_start_timeout` | A worker launch was not ready within `worker_start_timeout`. | Look at the node's launch log; raise the setting for slow nodes. |
| `502` "force unload of node <id>" | Someone force-unloaded a node the model used. | Expected. |
| A node shows `unplaced` | See "Why a node gets no load" (D). | |
| Agents refused with `release_unavailable` | The scheduler serves no verified release. | `curl -s $H/api/system | jq .programs` names the problem (E). |
| Every load refused, `/api/system` shows `host_build_error` | The scheduler's `llama-server-rpc` is not the release's llama build. | Build the base image from the release's commit (B). |
| A model lands on the host GPU unexpectedly | The host tier is used only when better tiers cannot hold the model, or their nodes are not placeable. | `POST /api/plan` says why. Restrict the profile with `allow_tiers`. |
| `potluck-share` does not start | The `nfs` and `nfsd` kernel modules are not loaded, or `POTLUCK_SHARE_BIND` is not an address of the host. | `lsmod | grep -w nfsd`; check `.env`. |
| About shows "A background task failed in the last hour" | An internal error (a bug) in one of the scheduler's background tasks. It was handled: the task restarted alone, or the work it was doing failed with an error. | See "An internal failure" below. |
| The container restarted with exit code 3 | A failure Potluck must not continue from: the scheduler loop, the lost-node check or a configuration apply failed, or one task failed three times in a minute. | See "An internal failure" below. |

### An internal failure

Potluck isolates its background tasks (architecture, "What restarts when"). What you see:

- The log line `background task failed` at error level, with `task`, `panic`, `failures_last_min`, `restarts` and the Go `stack`.
- An `internalFailure` event on the Activity page (filter "Scheduler"), with the task and the message.
- The About page's "Background tasks" card: each task with what it does after a failure, its restarts, failures in the last minute and last failure. The same is in `curl -s $H/api/system | jq '.tasks, .internal_failures_total'`.

What to do:

1. Nothing is needed for the service: a task that restarts alone is running again (state `running` in the card). A benchmark run that was executing shows failed with "internal error"; start it again. A configuration change that answered 500 "the change is saved, but applying it ... failed" is saved, and the restart that follows applies it.
2. Keep the evidence: `docker logs potluck 2>&1 | grep -A40 'background task failed'` and the `internalFailure` events on the Activity page. Report it with the release (`version` in `/api/system`); it is a bug.
3. If Potluck stopped (the `schedulerStopping` event says "internal failure in ...", and `docker inspect -f '{{.State.ExitCode}}' potluck` shows 3 until the restart), Docker starts it again. Loaded models ended, as with any scheduler restart (L). If it stops again and again, roll back to the previous release (F).

---

## K. Move an existing deployment to the standing layout

The standing layout is `deploy/docker-compose.yml` as it is now: project `potluck`, containers `potluck`, `potluck-share` and `potluck-logs`, every host folder under `POTLUCK_ROOT`. These steps move an older deployment (project `llm-scheduler`, container `llama-swap`, folders next to the compose file in `/opt/llm-scheduler/`) with one short outage, from step 5 to step 7.

The old project must be down first: both use the subnet `172.30.0.0/24`, and two Docker networks cannot hold the same subnet. On that network the scheduler has the fixed address `172.30.0.2` and the optional host agent `172.30.0.10`; the share and the log store take addresses from `172.30.0.128/25`, so they never take a fixed one. Docker sets that range only when it creates the network. Check: `docker network inspect potluck_sched --format '{{(index .IPAM.Config 0).IPRange}}'` prints `172.30.0.128/25`; if not, run `docker compose down` and `docker compose up -d` once.

1. Write down what the old deployment uses:

```sh
docker inspect llama-swap --format '{{range .Mounts}}{{.Source}} -> {{.Destination}}{{"\n"}}{{end}}'
curl -s $H/api/config | jq '.revision, .settings.membership'
```

2. Put the new compose file, the seed and `share-nfs.conf` in `/opt/potluck/`, and copy the old `.env` there (`POTLUCK_SHARE_BIND`, `POTLUCK_SHARE_NET`). The seed is not imported again. Check: `cd /opt/potluck && docker compose config -q`.

3. Create the root folder, owned like the old data folder:

```sh
sudo mkdir -p /srv/potluck && sudo chown "$(stat -c '%u:%g' /opt/llm-scheduler/data)" /srv/potluck
```

4. Optional, while everything still runs: set the host's node name in its own setting, `curl -s -X PATCH $H/api/config/settings -d '{"local_node":"host-1"}'`. While it is empty, `fleet.local_node` is used.

5. Outage starts. Stop the old project: `cd /opt/llm-scheduler && docker compose down`. Then `docker network ls | grep -c sched` prints `0`. Nodes keep running; their share mounts wait.

6. Move the data, each folder whole (never `scheduler.db` without its `-wal` file):

```sh
sudo mv /opt/llm-scheduler/data       /srv/potluck/data
sudo mv /opt/llm-scheduler/history    /srv/potluck/history
sudo mv /opt/llm-scheduler/logs       /srv/potluck/child-logs
sudo mv /opt/llm-scheduler/logs-store /srv/potluck/logs-store
sudo mv <programs folder from step 1> /srv/potluck/programs
```

7. Start the new project: `cd /opt/potluck && docker compose up -d`. Outage ends.

8. Check: the checks of section C, the same revision as in step 1 (one more after step 4), and `curl -s $H/api/nodes | jq '[.nodes[] | {name, state}]'`. Agents register again within a minute.

9. Keep `/opt/llm-scheduler/` until you are sure. The way back: `docker compose down` in `/opt/potluck`, move the five folders back, `docker compose up -d` in `/opt/llm-scheduler`. If you did step 4 and go back to an image older than the `local_node` setting, set it to `null` first.

---

## L. What comes back after a restart of the host

Nothing has to be started by hand when these hold:

1. Docker starts at boot (`systemctl is-enabled docker`).
2. The three services are `restart: unless-stopped`. A service stopped by hand stays stopped; start it with `docker compose up -d`. The optional host agent comes back too once it was started with `--profile local-agent`.
3. The share needs the `nfs` and `nfsd` modules at boot.
4. The agents keep retrying and register again. Their share mounts resume when `potluck-share` is up; a share restart costs clients about 16 s. Check the NFS times: `docker exec potluck-share cat /proc/fs/nfsd/nfsv4gracetime /proc/fs/nfsd/nfsv4leasetime` prints `10` twice.
5. No model is loaded at start. Launches left on the nodes are stopped by their agents.
6. Learned memory needs come back from the database. The queue, backoffs and quarantines start empty.

Check after a restart:

```sh
docker compose -f /opt/potluck/docker-compose.yml ps
curl -fsS $H/health
curl -s $H/api/nodes | jq '[.nodes[] | {name, state}]'
```

---

## Legacy: fleet membership

With the setting `membership: fleet` the scheduler reads its nodes from an external fleet console (`fleet.url`) instead of agent registrations, pre-flights each worker with `llama-server --list-devices`, and can send recovery requests for a hung worker (the `recovery` settings). `deploy/scripts/preflight-host.sh` and `deploy/scripts/hardware-checks.md` were written for that mode. It stays until a later release removes it; new deployments use agents.
