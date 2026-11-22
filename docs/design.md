# Cluster scheduler: design

Status: v1.1. Approved after three review passes; built; first hardware test
run 2026-11-17 (results in `docs/hardware-test/RESULTS.md`). Owner: Jacob.
Changes to behaviour go into this document first.

Two sources of membership. This document was written for the fleet feed: the
nodes come from the fleet console (`defaults.membership: fleet`, still the
default). Since 2026-11-21 the cluster runs in agents mode
(`defaults.membership: agents`, release 0.1.0): each machine runs
`potluck-agent`, registers with the scheduler and starts its worker per load.
Section 7.7 says what changes in the scheduler; the wire between the two is
`docs/agent-contract.md`. Where a section below says the nodes come from the
fleet feed (3.3, 5, 7.1, 7.5), read it as the fleet mode.

A drop-in replacement for llama-swap on the host machine that decides at load time which
machines a model runs on. Working name of the binary: `llm-scheduler`.

Tags used below:

- **[verified]** confirmed from this workspace's repos, upstream source at the
  pinned commit, or the owner's reports
- **[assumed]** believed true; a named hardware check must prove it

---

## 1. Problem

llama-swap maps one model name to one fixed command. In this cluster the command
depends on which machines are free, so the config has become a table of
model x context size x pool: 10 Gemma and Devstral entries for 2 model files
(`host-1-fleet-manager/apps/llama-swap/config.yaml`). Every added node
multiplies it.

Wanted: ask for `gemma-4-12b:64k` and have the system pick the machines, using
live availability, with the PS5s preferred and the laptops as fallback.

## 2. Decisions already made by the owner

These are requirements, not proposals. Reviewers should check that the design
meets them, not reopen them.

1. Custom scheduler that **replaces** llama-swap. Not a layer above it.
2. Written in Go, one Docker container on the host machine.
3. Drop-in for existing apps: same port (3122), same OpenAI-style endpoints.
4. Nodes come from the fleet manager at run time. Nodes join and leave.
5. Priority tiers: PS5s first, Nvidia laptop second, host machine (host) last.
6. One profile per model + context size. No pool count in the name.
   "If it fits, it fits": one node when it fits, several when it does not.
7. A profile may force specific nodes, for benchmarks.
8. Names follow `model:variant` (e.g. `gemma-4-12b:64k`). Old names are
   hand-configured aliases, logged on use, to be phased out.
9. When nothing is free, requests **queue**, as with llama-swap: a running
   generation is never interrupted by scheduling; a model is unloaded to make
   room only once nothing is running on it.
10. Built test-first (TDD).
11. Registration structure stays as it is: PCs register with the host machine hub, PS5s
    push to the fleet manager, the hub relays PCs into the fleet manager, and
    other programs (this one included) only **pull** from the fleet manager.

## 3. Environment

| Node | GPU | Memory | RPC address | Fleet source |
|---|---|---|---|---|
| host machine (host) | RTX 5060, CUDA | 8 GB VRAM | local | relayed by hub; read locally instead (5.1) |
| Nvidia laptop (`laptop-1`) | RTX 3070 Ti, CUDA | 8 GB VRAM | `192.0.2.3:50052` | relayed by hub |
| PS5 (`console-4`) | AMD Oberon, Vulkan | about 14-15 GB shared | `192.0.2.2:50052` | pushes itself |
| more PS5s | same | same | from fleet manager | push themselves |

### 3.1 llama.cpp facts (commit `330f0189`)

All **[verified]** against source unless tagged otherwise.

- Client and `rpc-server` must be built from the same commit.
- `rpc-server` serves **one client at a time** (`ggml-rpc.cpp:2985-2994`):
  it serves each accepted connection inline. A second client's TCP connect
  succeeds, then blocks until the first disconnects.
- RPC sockets have no timeouts. `--rpc` contacts every server while arguments
  are parsed, so a head pointed at a busy or silent worker hangs. A head whose
  worker loses power most likely blocks while its `/health` keeps answering
  (inferred from the code, not observed).
- `--device` takes device names and only those devices are used. RPC devices
  are named `RPC0, RPC1, ...`, numbered **per device** across the `--rpc` list,
  not per server. `--rpc` must come before `--device`. With `--device`, device
  order (and `--tensor-split` order) is the order written.
- With no `--tensor-split`, layers are split in proportion to each device's own
  free memory. `--fit` is on by default and conflicts with `-ngl`. The
  scheduler always passes `--fit off` and an explicit `--tensor-split`.
- `--list-devices` needs no model, prints one stdout line per device in the
  form `  RPC0: <description> (<total> MiB, <free> MiB free)` and exits 0
  (`common/arg.cpp:1140-1150, 2697-2703`). It lists local devices too. The free
  figure for an RPC device is live. Against a closed port the process aborts at
  once (SIGABRT); against a busy worker it blocks forever.
- Per-slot context is `--ctx-size / --parallel` (`llama-context.cpp:428`).
- KV cache: per layer, `(n_embd_k_gqa + n_embd_v_gqa) × cache_size`, with 2
  bytes per element for f16, 1.0625 for `q8_0`, 0.5625 for `q4_0`. For ordinary
  layers `cache_size` is the context. Models with sliding-window layers
  (Gemma) give those layers a much smaller cache, and some layers share one.
- Each head keeps a prompt cache in host RAM, limit `--cache-ram` (default
  8192 MiB), and keeps the input layer on the host CPU.

### 3.2 Deployment facts

- **[verified]** The current image (`llama-swap-whisper:rpc-330f018`) contains
  `llama-server` (CUDA, no RPC), `llama-server-rpc` (CUDA + RPC), `sd-server`
  (FLUX) and a Python Whisper server.
- **[verified by owner]** PS5 and Nvidia laptop firewalls admit port 50052 from the
  host machine only. llama-swap reaches both today from a bridged container. Every head
  process must run on the host machine.
- **[measured 2026-11-17, superseded]** Before the PS5-side fixes, PS5 uploads
  ran at about 13.9 MB/s for one upload and under 5.6 MB/s each with two at
  once. The cause was the PS5 `rpc-server` tensor cache, which wrote every
  tensor to USB storage at about 3 MB/s (a cold 14.3 GB load took 83 minutes).
  The Nvidia laptop took 77 MB/s.
- **[measured 2026-11-18]** Two PS5-side fixes landed (they are not in this
  repo): the tensor cache was turned off, and the PS5 GPU clock was pinned at
  2230 MHz (it sat at 500 MHz), which raised decode speed about 2.4x on a
  two-PS5 split. With the cache off, every load is a full upload and there are
  no fast cached reloads. On the old link (the host machine's built-in 1 GbE port)
  upload speed was 75 to 84 MB/s in total out of the host machine, the same for 1, 2 or
  3 PS5s in one load. A split model uploads to one PS5 at a time, in an order
  llama.cpp chooses. Loads that ran at the same time shared that rate: one
  Gemma 12B load took 89 s, two at once 167 s each (40 MB/s each), three at
  once 246 s each (27 MB/s each).
- **[measured 2026-11-18, about 14:00 to 14:31 UTC]** The host machine's link to the
  PS5 switch moved from its built-in 1 GbE port to a USB-C adapter (interface
  `enx020000000010`) negotiated at 2500 Mb/s. The PS5s still have 1 GbE ports.
  A single stream to one PS5 is now limited by that PS5's own port (about 100
  to 125 MB/s). Loads to different PS5s no longer share one gigabit. Load times
  at ctx 2048, before (1 GbE) and after (2.5 GbE): Gemma 12B on one PS5 89 s,
  then 66 s (single stream about 78 MB/s, then about 100 to 108 MB/s); Qwen
  27B across three PS5s 216 s, then 170 s; Kimi-Dev 72B IQ2_S across three PS5s
  363 s, then 286 s; two Gemma 12B loads at once 167 s each, then 66 s each;
  three Gemma 12B loads at once 246 s each, then 77 s each (about 280 MB/s in
  total, close to the 2.5 GbE link). Devstral 24B on two PS5s together with
  Gemma 12B on the third took 149 s and 67 s (no earlier run of this pair;
  Devstral alone was 183 s on 1 GbE). A split model still uploads to one PS5 at
  a time. Config uses `link_mb_s` 100 per PS5 and 60 for the Nvidia laptop.
- **[measured 2026-11-17]** H1, H2 and H3 below all hold on this cluster, and
  the container reached the fleet console on 8090 without an extra firewall rule.
- **[verified by owner]** The old "wedges on client close" and "stalls over
  500 MiB" problems belonged to the PS5LM app, not PS5 Linux.
- **[verified]** The host machine's host firewall admits only ports 22, 9443 and 5353
  by default. Reaching the fleet console on 8090 from a container needs a rule.
- **[assumed] H1.** `nvidia-smi` is available inside the container.
- **[assumed] H2.** Each worker's `rpc-server` exposes exactly one device.
- **[assumed] H3.** `--list-devices` runs cleanly with `CUDA_VISIBLE_DEVICES=""`
  (no local GPU touched). If not, the probe runs without that setting.

### 3.3 Fleet manager facts

All **[verified]**. They apply to fleet membership; in agents mode the fleet
feed is not read (section 7.7, `docs/agent-contract.md`).

- Fleet console backend on `0.0.0.0:8090`, state in memory only (a restart
  empties it until nodes report again). `GET /api/fleet`,
  `GET /api/nodes/:id`, SSE at `GET /api/stream?topics=fleet,models` with a
  ping every 5 s. The stream sends a snapshot only when a meaningful field
  changes, not on every report. Offline after 15 s without a report.
- A member's key is `id` (its MAC when known). Two consoles can share a
  `hostname`.
- `FleetMember` fields used here: `id`, `hostname`, `role`, `source`, `online`,
  `rpc`, `ip`, `rpc_port`, `vram_total`, `vram_used`, `ram_total_gb`,
  `ram_used_gb`; newer optional fields: `mem_available_mib` (any number, rounded down), `rpc_server_mem_mib` and `rpc_server_version` (informational, shown in `/api/status`). Numbers are read as int or float; a field of the wrong type is ignored with a rate-limited WARN naming it, and a bad field never discards its member, nor a bad member the payload.
- `vram_total` and `vram_used` are in **GB**. For relayed nodes the hub
  truncates the total to an integer and derives used from a percentage,
  measured every 5 s. PS5s report amdgpu GTT figures.
- `rpc` is a TCP connect probe that also reads `true` on a connect timeout.
  It cannot tell a healthy worker from a busy or firewalled one.
- `ip` (IPv4 or null) and `rpc_port` (number or null) are on the fleet console
  branch `fleet-scheduler-fields` (going live). Relayed members show
  `ip: null` until the hub patch is deployed (branch `hub-member-ip` of
  `host-1-fleet-manager`, written, not deployed): it sends the Nvidia laptop
  as `192.0.2.3` / `50052` and the host machine as `192.0.2.1` / `null`.

## 4. Goals and non-goals

Goals:

- One endpoint; placement decided per load from live state.
- Config holds profiles and policy only. Adding a PS5 needs no config edit.
- Existing clients and the host machine hub's llama panel keep working.
- Correct under concurrency: many agents arriving at random times.

Non-goals for v1 (see section 14 for the planned follow-ups):

- Two models on one node. Every node is exclusive to one instance, because
  `rpc-server` is single-client.
- Exact memory estimates for sliding-window models. Those profiles take a
  measured `needs_mib` (7.2).
- Learning memory needs automatically.
- Moving a loaded model without a reload; several replicas of one profile.
- Using free nodes that an earlier request has claimed while it waits (no backfill).
- Budgeting host RAM on the host machine. Nodes are exclusive, so there are never more
  instances than nodes, and each instance's host prompt cache is capped.
- Authentication (unchanged: LAN-only firewall). The dashboard at /ui/ is read-only (9.1); the live channel (9.2) and benchmarks (9.3) are the only additions.
- Starting or stopping `rpc-server` on nodes.

## 5. Architecture

One Go process in one container on the host machine.

```
clients ──HTTP──▶ ┌──────────────── llm-scheduler ────────────────┐
(agents, IDE,     │ api/proxy ─▶ scheduler core ─▶ instance mgr ──┼─▶ llama-server-rpc (child)
 hub panel)       │              (one goroutine)        │         │      │ --rpc a,b
                  │                 │  ▲             prober ──────┼─▶ --list-devices (pre-flight)
                  │              placer │                         │      ▼
                  │                 ▼  │                          │   rpc-server on PS5s / Nvidia laptop
                  │   node view ◀─ fleet client ◀── SSE ── fleet console (:8090)
                  │       ▲─────── local GPU probe (nvidia-smi)   │
                  └───────────────────────────────────────────────┘
```

| Package | Job |
|---|---|
| `internal/config` | The configuration document, settings schema, validation, and the YAML parser used only by `import-config` and tests. |
| `internal/audit`, `internal/cfgstore`, `internal/cfgsvc` | Audit entry and origin; revisions and audit log in SQLite; the one service every configuration change goes through. |
| `internal/gguf` | Read the GGUF header fields needed for memory estimates. |
| `internal/estimate` | Memory need per profile (pure functions). |
| `internal/fleet` | Fleet console client (SSE, polling fallback, single-node fetch), local GPU probe, merged node view in MiB. |
| `internal/placer` | Pure functions: place, plan an eviction, or say why not. |
| `internal/instance` | Command builder, pre-flight prober, start / health-check / stop one child, port allocation. |
| `internal/scheduler` | The state machine: instances, claims, queue, timers. |
| `internal/proxy` | Extract the model name; stream the request to the instance. |
| `internal/api` | Routes: OpenAI paths, llama-swap compatible paths, admin paths. |
| `internal/benchspec`, `internal/bench` | Benchmark and suite shapes and validation; the SQLite store, runner, statistics and exports (9.3). |
| `cmd/llm-scheduler` | Wiring, flags, signals. |

**Concurrency model.** All scheduler state is owned by one goroutine that
processes events from a channel: request arrived, request finished, client
gone, pre-flight result, child healthy, child exited, node view changed, timer
fired, admin command. HTTP handlers only send events and wait for replies.
Slow work (pre-flight, starting and stopping children, fetching a node) runs
outside the core and reports back as events. The core takes interfaces for the
clock, the node view, the prober and the launcher, so every rule in section 8
is testable with fakes.

### 5.1 The local node

The host machine's GPU is read with
`nvidia-smi --query-gpu=memory.total,memory.used --format=csv,noheader,nounits`
every 2 s (H1), not from the fleet manager: exact MiB, and `kind: command`
profiles keep working when the fleet console or the hub is down. If
`nvidia-smi` is missing, the scheduler logs a warning and falls back to the
fleet entry for `fleet.local_node`. The fleet entry for the local node is
otherwise ignored. The probe command is a config value (`local_probe_cmd`), so
tests point it at a fake.

The local tier's `headroom_mib` is 450. `nvidia-smi` reports 8151 MiB on the
host machine's GPU but llama.cpp can use 7707, and each child that uses the GPU adds
about 120 MiB of CUDA context that the load log does not show.

The local node has one fixed id for the life of the process: the value of
`fleet.local_node`. Its fleet member (matched by id or hostname) is never
published as a remote node, and both of the member's names resolve to that id.

## 6. Configuration

Configuration lives in the scheduler database, not in a file. A deployment
has five bootstrap values (paths, 6.0.1) and everything else is stored in
`scheduler.db` under the data directory: profiles, tiers, `exclude_nodes`,
`node_overrides`, the `defaults` timing, safety, hold and worker-health
settings, `recovery`, fleet settings, retention numbers, `ws_max_clients` and
the bench settings. There is no backwards compatibility with the old
`--config file.yaml` start: the server never reads YAML. The YAML parser is
kept only for the one-time `import-config` command (6.0.5) and for tests.

### 6.0 The database configuration

**6.0.1 Bootstrap (the only things outside the database).** Deployment facts,
not scheduler policy, set by flag or environment variable (a flag wins):

| Flag | Environment | Default | Meaning |
|---|---|---|---|
| `--listen` | `LLM_SCHEDULER_LISTEN` | `:8080` | address to listen on |
| `--data-dir` | `LLM_SCHEDULER_DATA_DIR` | `/var/lib/llm-scheduler/data` | directory of `scheduler.db` (configuration, revisions, audit log, benchmarks) |
| `--models-dir` | `LLM_SCHEDULER_MODELS_DIR` | `/models` | where the model files are |
| `--history-dir` | `LLM_SCHEDULER_HISTORY_DIR` | empty (history off) | event history files (9.1) |
| `--child-log-dir` | `LLM_SCHEDULER_CHILD_LOG_DIR` | empty (none kept) | full output of each child (7.3a) |

The Dockerfile sets the five variables to the container layout.

**6.0.2 Document, revisions.** The configuration is one JSON document with the
sections `settings`, `tiers`, `profiles`, `exclude_nodes`, `node_overrides`,
`recovery`, `fleet`. `settings` stores only the keys that were set; every
other key has its built-in default, and `null` in a PATCH resets a key. Every
committed change writes a new, whole revision (`config_revisions`: monotonic
integer, time, source, actor, comment, document), so revision *n* can be read
back in full and rolled back to (a rollback creates a new revision equal to
*n*). A write that changes nothing creates no revision.

**6.0.3 Validation.** The existing validation (the same messages as with the
YAML file) runs on the whole resulting configuration before anything is
written. An invalid change is rejected with `400` and every message; nothing is
written, nothing is audited. Model files are checked against the models
directory at change time. At start, a stored revision whose model file went
missing still loads (a warning is logged): a missing file must not stop
serving.

**6.0.4 Audit log.** Table `audit_log`, append-only (a trigger refuses
`UPDATE`; the application never deletes): `id, ts (unix ms), source, actor,
remote_addr, user_agent, action, section, target, before, after, diff, revision,
comment`. One change writes one entry per touched `(section, target)`: a
profile id, a settings key, `tiers`, `exclude_nodes`, a node override, a
recovery or fleet key. `diff` is a list of `{path, old, new}`. The config
revision and its entries are committed in one transaction.

- **source** is decided by the server: `ui` only when the request carries
  `X-Source: ui`; `mcp` only for calls an in-process MCP layer marks with
  `audit.WithSource`; `import` for the import command; `system`; anything else
  is `rest`. An outside client cannot claim `mcp` or `import` with a header.
- **actor** is the `X-Actor` header (free text) or the MCP client name;
  default `anonymous`. `X-Comment` (or a `comment` field in the body) is the
  comment. `remote_addr` and `user_agent` are recorded.
- `load` and `unload` asked for over REST or MCP are audited as section
  `runtime`, without a revision. They go through a bounded queue to one
  writer goroutine: a database problem never delays or fails the action.
  The loads and unloads of a benchmark run (the runner calls the API on
  loopback with `X-Bench-Run: <run id>`) are audited too, but as source
  `system`, actor `bench run <id>` and `after.bench_run`; they are never
  mistaken for a person's action, and the header does not hide a call (it only
  changes its label). The history record of such a load carries
  `data.bench_run` (9.3).
- Benchmark and suite changes, run starts and run cancels are audited in
  section `bench` (9.3), with the same source and actor rules.
- `audit_retention_days` (default 0 = keep everything): when above 0, entries
  older than that are removed at start. That is the only delete in the
  application and is off by default.

**6.0.5 First start and migration.** An empty database starts: built-in
defaults, no profiles, no tier (so no node is used), a WARN line "no
configuration yet", `configured: false` in `GET /api/config` and
`GET /api/status`. `llm-scheduler import-config --data-dir DIR [--models-dir
DIR] [--replace] [--check] [--no-model-check] file.yaml` reads a YAML file in
the old format (`configs/example.yaml` documents it), validates it, and writes
it as revision 1 with source `import`. It refuses when the database already
has a configuration unless `--replace` (then a new revision). The `listen`,
`models_dir`, `data_dir`, `history_dir` and `child_log_dir` keys of the file
are not stored. Benchmarks and suites of its `bench` block go into the
benchmark tables. If the database cannot be opened at start the scheduler
still starts with the defaults and no profiles; models already loaded are
unaffected by any later database failure because the running configuration is
a snapshot in memory.

**6.0.6 Live apply.** A committed change reaches the scheduler core as an
event carrying a new immutable `Config` snapshot, swapped between two steps
(no I/O, nothing blocks the loop); request goroutines see the snapshot at
once. Rules as implemented:

- A loaded or loading instance keeps the profile definition it started with.
  When its profile has since changed or been deleted, `GET /api/status` shows
  `stale_profile: true` on it; the new definition is used at its next load.
- Queued requests are re-evaluated: their queue entries take the new profile
  definition and the new timing; the requests of a deleted profile fail at
  once with `404 unknown model` and the message `profile "x" was deleted by a
  configuration change`. New requests for a deleted name get `404`.
- Tier, `exclude_nodes` and `node_overrides` changes: the fleet client
  rebuilds its node view at once, and the scheduler reads exclusions at each
  decision, so they govern the next placement decision; running instances are
  not moved.
- Timing, safety, hold, worker-health and recovery settings are read from
  the snapshot at each evaluation: they apply from the next one.
- Cached memory needs and learned needs of a changed or deleted profile are
  dropped.
- `restart_required` settings are stored and audited at once, listed in the
  response (`restart_required`) and take effect at the next start, because a
  long-lived component reads them once: `server_bin` (the pre-flight prober),
  `fleet.url` and `fleet.local_node` (the fleet client base address and node
  identity), `child_log_keep` (the child launcher), `history_max_file_mib`,
  `history_keep_files`, `history_memory_events` (the history store is opened
  at start), `ws_max_clients` (the live hub is sized at start), and
  `bench_enabled`, `bench_regress_load_pct`, `bench_regress_decode_pct`,
  `bench_max_run_minutes` (the benchmark manager is built at start).

**6.0.7 REST.** `GET /api/config`, `GET /api/config/schema` (every field of
settings, recovery, fleet, profile and tier with key, scope, section, type,
enum, default, min, max, unit, description, `restart_required`),
`/api/config/profiles[/{id}]`, `/api/config/tiers`, `/api/config/settings`
(`PATCH`), `/api/config/exclude-nodes`, `/api/config/no-auto-nodes`, `/api/config/node-overrides`,
`/api/config/recovery`, `/api/config/fleet`, `/api/config/revisions`,
`/api/config/revisions/{n}[/rollback]`, `/api/config/export`,
`/api/config/import`, `/api/audit`, `/api/audit/{id}`. Every write accepts
`?dry_run=1`, `If-Match: <revision>` (`409` with `current_revision` when stale)
and a comment; the answer has `revision`, `audit_entry_ids`, `diff`,
`warnings` and `restart_required`. Deleting a profile that benchmarks refer to
is refused with `409` and the warnings unless `?force=1`. There is no
authentication anywhere (LAN tool). Loading and unloading models stays out of
the web UI but is available over REST (and MCP).

The WebSocket `hello` carries `config_revision`; the `config` topic announces
`{revision, audit_entry_ids, section, target, source, actor}` on every change;
`status` carries `config_revision` and `configured`.

**6.0.8 The keys.** The text below describes each key. It is written in YAML
because that is the format of the import file and of `configs/example.yaml`;
the same keys are the database settings (`defaults.*` are the plain setting
names `ttl`, `min_hold`, ...; `recovery.*` and `fleet.*` are their own
sections; the top-level retention and bench keys are settings named
`child_log_keep`, `history_*`, `ws_max_clients`, `bench_enabled`,
`bench_regress_load_pct`, `bench_regress_decode_pct`, `bench_max_run_minutes`,
plus `local_probe_cmd` and `audit_retention_days`).
`GET /api/config/schema` is the authoritative list with defaults.

Nodes are not listed; only how to rank them.

**Node identity.** Internally nodes are keyed by the fleet `id`. Where config
names a node (`force_nodes`, `node_overrides`, `fleet.local_node`), the name
may be an `id` or a `hostname`. A hostname shared by two online members is
ambiguous: it matches neither, and the scheduler logs an error naming both ids.

The block below is the format of the import file (6.0.5); `listen`, `models_dir`,
`data_dir`, `history_dir` and `child_log_dir` are bootstrap flags (6.0.1) and are
not stored.

```yaml
listen: ":8080"                    # mapped to 3122 by compose
fleet:
  url: http://host.docker.internal:8090
  local_node: host-1
  stale_after: 60s                 # view age beyond which remote nodes are not used
models_dir: /models
child_log_dir: ""                  # optional; see "Child output on disk" below
child_log_keep: 200                # files kept in child_log_dir
history_dir: ""                    # optional; the event history (9.1). Empty = off
history_max_file_mib: 16           # a history file is rotated at this size
history_keep_files: 20             # files kept in history_dir; the oldest are deleted
history_memory_events: 5000        # newest events held in memory for queries (1 to 200000)
data_dir: ""                       # bootstrap flag --data-dir (6.0.1); not stored
ws_max_clients: 32                 # concurrent /ws clients (9.2); 1 to 1000
bench:                             # 9.3; every key optional
  enabled: true                    # setting bench_enabled (default true)
  regress_load_pct: 15             # a load more than this much slower than the baseline's is a regression
  regress_decode_pct: 7            # decode tokens/s more than this much lower is a regression
  max_run_minutes: 240             # a run that takes longer is stopped and marked failed
  benchmarks: []                   # stored by import-config (source "config"); later edits use the API
  suites: []

defaults:
  ttl: 300s                # unload after this long idle; 0 = never
  min_hold: 30s            # hold time floor (8.3)
  max_hold: 900s           # hold time ceiling
  drain_after: 60s         # wait before a busy model is asked to drain
  queue_timeout: 600s      # max time an entry may wait without a claim
  stall_timeout: 300s      # max gap between chunks once streaming has begun
  command_load_timeout: 900s   # for kind: command profiles
  # load_stall_timeout: 180s  # fail a load whose measured upload gets no new acknowledged bytes for this long (a hung child); 0 = off; per-profile override of the same name
  shutdown_grace: 300s
  max_pool: 4
  device_overhead_mib: 600 # per-device cost added to estimated needs only
  min_share_mib: 1024      # least a node must be able to hold
  cache_ram_mib: 2048      # per-instance prompt cache limit on the host
  server_bin: llama-server-rpc
  local_device: CUDA0
  forced_node_grace: 30s   # forced node absent/offline this long: fail fast (8.4)
  forced_fit_check: off    # off | warn | reject: fit check for force_nodes profiles (7.2)
  # split_skew_margin: 0.15     # per-node fit on shared-memory nodes: share = need x fraction x (1 + this) (7.2a)
  # estimate_margin: 0.05       # added to a need that is an estimate (7.2, 7.2a)
  # load_memory_floor_mib: 768  # cancel a load when a shared-memory node's available memory falls below this; 0 = off (7.2a, 8.4)
  # worker_silence: 30s         # a worker's upload or heartbeat channel is silent after this long (8.4a)
  # worker_probe_interval: 0    # TCP connect probe (8.4a); 0 = never runs (default; harmful for rpc-server workers)
  # worker_probe_during_inference: false  # only with the probe on: probe while a request is in flight

tiers:                     # first match wins; lower number preferred;
                           # a node matching no tier is never used
  - match: { hostname: "console-*" }
    priority: 1
    headroom_mib: 1024
    shared_memory: true    # GPU memory is system RAM
    link_mb_s: 8           # measured; used to size load timeouts
    settle: 3s
  - match: { hostname: "laptop-*" }
    priority: 2
    headroom_mib: 256      # dedicated VRAM; pre-flight gives exact figures
    link_mb_s: 60
    settle: 8s             # relayed figures are measured every 5 s
  - match: { local: true }
    priority: 3
    headroom_mib: 450      # see 5.1
    settle: 2s

node_overrides:            # optional; wins over the fleet's ip/rpc_port
  laptop-1: { rpc: "192.0.2.3:50052" }

exclude_nodes:             # optional; nodes that are never used (see below)
  - ps5-abc123             # a hostname
  - ps5-test*              # a hostname glob
  - 02:00:00:00:00:01      # a node id

no_auto_nodes:             # optional; nodes used only by profiles that force them
  - laptop-*

profiles:
  gemma-4-12b:             # bare name = the default variant
    model: gemma-4-12b-it-Q4_K_M.gguf
    ctx: 8192
    needs_mib: 7900        # measured 2026-11-18; see 7.2
    aliases: [gemma-4-12b-1pool-8k, gemma-4-12b-2pool-8k]
  "devstral-24b:14k":
    model: Devstral-Small-2-24B-Instruct-2512-Q4_K_M.gguf
    ctx: 14336
    kv_quant: q8_0
    allow_tiers: [1, 2]              # never the host machine's GPU
  gemma-bench-2pool:                  # benchmark: forced placement
    model: gemma-4-12b-it-Q4_K_M.gguf
    ctx: 65536
    kv_quant: q8_0
    flash_attn: true
    force_nodes: [console-4, laptop-1]
    tensor_split: [60, 40]
  large-v3-turbo:                     # non-LLM: fixed command on the host machine
    kind: command
    cmd: env -u LD_LIBRARY_PATH /opt/whisper-server/venv/bin/python /opt/whisper-server/server.py --host 127.0.0.1 --port ${PORT} --model large-v3-turbo
    check_endpoint: /health
    needs_mib: 5400
    stall_timeout: 0
```

`tier.match` keys: `hostname` (glob), `role` (exact), `source` (exact),
`local` (bool). Several keys in one match must all hold.

**Excluding nodes.** `exclude_nodes` is an optional list of strings. An entry
matches a node when it equals the node id, or matches the node's hostname as a
glob (the same matching as a tier's `hostname`); both are case-insensitive. It
lets the owner keep a machine (a PS5 under test, say) from ever receiving a
model without giving up the `console-*` tier for the others.

- An excluded node is not *reachable* (7.1), so it is never eligible, never
  placed, never claimed, never counted in the capacity check (7.2) or in a
  plan, and never pre-flighted (7.5).
- It still appears in `/api/status` `view.nodes` with `excluded: true` and
  `eligible: false`. Wait and plan reasons that involve it say
  `node <hostname> is excluded by config`. When the capacity check fails, the
  message lists the excluded nodes that were left out.
- The local node can be excluded (by `fleet.local_node` or its hostname). The
  host machine GPU is then never used, and `kind: command` profiles, which run only on
  the local node, fail as "does not fit".
- Excluding wins over forcing. A profile whose `force_nodes` names an excluded
  node fails config validation when config alone can tell (the name equals an
  entry, or is a hostname the glob matches). When only the fleet can tell (a
  hostname glob against an id), the request fails at once with `503`, reason
  `node excluded` and no `Retry-After`, like `does not fit`; an entry already
  queued is failed the same way when the node is seen. A forced node that is
  absent from the fleet, or offline, queues for `forced_node_grace` (30 s) in
  case it is only re-registering, then fails fast (8.4).
- The scheduler logs the entries once at startup and, at INFO, the first time
  each excluded node appears in the fleet view.

**Nodes for forced profiles only** (CR-9 review). `no_auto_nodes` is an
optional list with the entries and matching of `exclude_nodes`. It keeps a
machine (a laptop someone works on, say) for the profiles that name it in
`force_nodes`:

- Automatic placement (a profile without `force_nodes`) never picks the node:
  it is not a candidate in Place or Plan (7.3), is not counted in the
  capacity check (7.2), and is never pre-flighted for such a profile.
- An instance that runs on the node is never a victim of automatic eviction:
  a profile without `force_nodes` does not evict it, even when the instance
  also uses other nodes.
- A profile that names the node in `force_nodes` uses it as before, and every
  forced-placement check still applies (`forced_fit_check`; with agents the
  forced-fit refusal of the agent contract 11.2).
- `exclude_nodes` wins: a node on both lists is excluded, and a forced profile
  that names it fails as for any excluded node. Config validation accepts
  `force_nodes` on a `no_auto_nodes` node and refuses it on an excluded one.
- The node stays eligible. `/api/status` `view.nodes` shows `auto: false` for
  it (`true` for every other node); the Nodes page shows a "Forced only" badge.
  A wait reason that involves it says `node <hostname> is in no_auto_nodes`;
  a failed capacity check lists the `no_auto_nodes` nodes that were left out.
- `GET`/`PUT /api/config/no-auto-nodes` and the MCP tool `set_no_auto_nodes`
  read and replace the list, with no restart; it is part of export and import
  and of the audit (section `no_auto_nodes`).

Fields for `kind: llama` (the default):

| Field | Meaning |
|---|---|
| `model` | File under `models_dir`. |
| `ctx` | Context **per slot**. The scheduler passes `--ctx-size ctx × parallel`. |
| `parallel` | Slots, default 1. |
| `kv_quant` | `f16` (default), `q8_0`, `q4_0`; applied to keys and values. Not `f16` requires `flash_attn: true`. |
| `flash_attn` | Adds `--flash-attn on`. |
| `needs_mib` | Measured memory need. Overrides the estimate. |
| `allow_tiers` | Tier priorities this profile may use. |
| `force_nodes` | Exact nodes, in device order. May include the local node. |
| `tensor_split` | Only with `force_nodes`; same length. |
| `extra_args` | Appended last. |
| `ttl`, `aliases`, `load_timeout`, `stall_timeout`, `load_stall_timeout` | Per-profile overrides. |
| `generated` | Optional marker: a tool made this profile (9.4). No effect on placement. |

Worker health (8.4a) is configured in `defaults` only:

| Key | Default | Meaning |
|---|---|---|
| `worker_probe_interval` | `0` | How often each worker that backs an instance gets a TCP connect probe (2 s timeout, nothing sent). `0` (default) means the probe never runs and its channel is always `unknown`; the rest of worker health is always on. Opt-in only, and not for `rpc-server` workers (8.4a). Must not be negative. |
| `worker_silence` | `30s` | How long a worker's upload progress or fleet heartbeat may be quiet before that channel is silent. Must be positive. |
| `worker_probe_during_inference` | `false` | Only with the probe on: also probe while the instance has a request in flight. Off by default. |

Recovery requests (8.4b) are configured in the top-level `recovery:` block
(`enabled`, `actions`, `min_interval`, `step_wait`, `request_ttl`,
`http_timeout`; defaults and rules in 8.4b). It is off by default.

Fields for `kind: command`: `cmd` (with `${PORT}`), `check_endpoint`,
`needs_mib` (required), `ttl`, `aliases`, timeouts. Always placed on the local
node alone.

Startup validation fails fast on: unknown fields; duplicate names or aliases; a
missing model file; `force_nodes` with `allow_tiers`; `tensor_split` without
`force_nodes` or of a different length; quantised KV without `flash_attn`;
an empty or invalid-glob `exclude_nodes` entry; a `force_nodes` name that
`exclude_nodes` excludes; `extra_args` containing a managed flag in any spelling (`--rpc`, `--device/-dev`,
`--tensor-split/-ts`, `--split-mode/-sm`, `--host`, `--port`, `--model/-m`,
`--ctx-size/-c`, `--n-gpu-layers/--gpu-layers/-ngl`, `--parallel/-np`,
`--cache-type-k/-ctk`, `--cache-type-v/-ctv`, `--fit/-fit`,
`--cache-ram/-cram`, `--flash-attn/-fa`, `--main-gpu/-mg`).
It **warns** when a `kind: llama` profile has no `needs_mib` and its model's
GGUF metadata declares a sliding window (7.2). Children are started with every
`LLAMA_ARG_*` environment variable removed.

### 6.1 Profiles replacing today's config

| Profile | Context | Old names kept as aliases |
|---|---|---|
| `qwen3.5-4b` | 16K | (same name) |
| `gemma-4-12b` | 8K | `gemma-4-12b-1pool-8k`, `gemma-4-12b-2pool-8k` |
| `gemma-4-12b:48k` | 48K, q8 KV, flash-attn | `gemma-4-12b-2pool-48k` |
| `gemma-4-12b:64k` | 64K, q8 KV, flash-attn | `gemma-4-12b-2pool-64k`, `gemma-4-12b-3pool-64k` |
| `gemma-4-12b:96k` | 96K, q8 KV, flash-attn | `gemma-4-12b-3pool-96k` |
| `devstral-24b` | 8K | `devstral-24b-f16` |
| `devstral-24b:14k` | 14K, q8 KV, flash-attn | `devstral-24b-q8`, `devstral-24b-3way` |
| `devstral-24b:24k` | 24K, q4 KV, flash-attn (untested) | `devstral-24b-q4` |
| `tinyllama` | 2K | `tinyllama-ps5` |
| `flux-1-schnell`, `flux-1-dev`, `large-v3-turbo` | `kind: command` | (same names) |

`gemma-4-12b` was an alias of the 1-pool 8K entry and is now that profile's
name. Differences from today: the old 2-pool 64K entry and the Devstral q8/q4
entries ran without flash-attn and now run with it. The Gemma profiles need a
measured `needs_mib` each (7.2); the runbook has the steps.

**Devstral without a PS5.** Today Devstral runs across the two 8 GB laptops
with 163 MiB to spare. That is too tight for any general fit rule to accept,
so the automatic placer puts Devstral on a PS5 plus one more node. To keep the
laptops-only arrangement available when no PS5 is online, the example config
includes one forced profile:

```yaml
  "devstral-24b:14k-laptops":
    model: Devstral-Small-2-24B-Instruct-2512-Q4_K_M.gguf
    ctx: 14336
    kv_quant: q8_0
    flash_attn: true
    force_nodes: [laptop-1, host-1]
    tensor_split: [50, 50]
```

## 7. Placement

### 7.1 Node view and budgets

In agents mode the node view comes from the agents' registrations and
heartbeats instead of the fleet client; the figures below then come from the
heartbeat (section 7.7 "Nodes", agent contract section 11). The
budgets and everything after them are the same in both modes.

The fleet client and local probe produce, per node, in MiB:

- `total`, `used`: fleet GB x 1024, or `nvidia-smi` for the local node.
- `free = total − used`. For tiers with `shared_memory: true`:
  `free = min(total − used, ram_total − ram_used)`.
- **Live budget** `= free − headroom`. Used for nodes nobody holds.
- **Plan budget** `= empty − headroom`. Used for a node an instance holds, when
  working out what would fit once that instance has gone. `empty` is the best
  known figure for that node with nothing of ours on it: the smallest of
  `total`, the last pre-flight free figure seen for it, and the free figure
  seen when a claim on it last failed to fit (8.3 step 4). The last two are
  forgotten when a load on that node succeeds.
- **Pre-flight override.** When a pre-flight has measured a node (7.5), that
  free figure replaces the fleet figure in the live budget until an instance
  is next released from the node, the node is next probed, or 10 minutes pass.
  This stops a coarse fleet figure from proposing the same failing placement
  again.

A node is **eligible** for a profile when all hold:

- it is not excluded by `exclude_nodes` (6)
- it is the local node, or it is online in a view no older than `stale_after`
- remote: an address is known (`node_overrides`, else `ip` + `rpc_port`)
- it matches a tier, and the profile's `allow_tiers` permits that tier
- it is not quarantined (8.4)
- its budget `≥ min_share`

An eligible node is **free** when no instance holds it, no entry claims it, and
it is not *settling*. Settling: after an instance whose child ran releases a
node, the node is unusable for its tier's `settle` time, then the scheduler
fetches it once (`GET /api/nodes/:id`, or the local probe) before using its
figures. If that fetch fails, the node is treated as offline.

The fleet's `rpc` flag is not used for eligibility; the pre-flight (7.5) is.

### 7.2 Memory need

There are two kinds of need, with two fit rules.

**Measured** (`needs_mib` set). The figure is the whole footprint: weights, KV
and compute buffers, as actually allocated. A set of nodes fits when
`Σ budget_i ≥ needs_mib`. No overhead and no margin are added, because the
measurement already contains them.

**Estimated** (no `needs_mib`). `need = weights + kv`:

- `weights` = model file size.
- `kv = n_layer × (ctx × parallel) × (n_embd_k_gqa + n_embd_v_gqa) × b`, from
  GGUF metadata, with `b` as in 3.1.

A set of nodes fits when `Σ (budget_i − device_overhead) ≥ need × (1 +
estimate_margin)`, the margin being `defaults.estimate_margin` (default 0.05,
the 5% of earlier versions; hardware showed an estimate 2% low: 13991 MiB
estimated, 14248 MiB measured). Plans and messages call such a need
`estimated, +5% margin`. The
overhead and margin stand in for compute buffers, which the estimate cannot
see. This is deliberately cautious, and it is far too high for sliding-window
models (several times too high for Gemma at 64K), so those profiles must carry
a measured `needs_mib`.

**Measuring.** When a child loads, the scheduler reads the model, KV and
compute buffer sizes that llama.cpp logs per device and shows them, with their
sum, in `/api/status` for as long as the instance lives. In those log lines an
RPC device is named `RPC0[<ip:port>]` with a per-server index, so devices are
matched to nodes by the endpoint in brackets, not by the `RPC<n>` name used on
the command line. Host-side buffers (CPU) are shown separately and are not
part of the sum. To measure a profile,
load it once through a `force_nodes` profile (which skips the fit check), read
the sum, and write it into `needs_mib`.

If a profile's need does not fit the plan budgets of all the nodes it could
ever use (best `max_pool` of them), the request fails at once with `503` and a
message giving the need and the capacity. It does not queue. For this check
only, quarantined and settling nodes still count, because they will be back
shortly. The check is skipped while the fleet view is stale: the request
queues, and if nothing changes it times out with the "fleet view stale" reason.

Worked checks, used as golden placer tests (host machine 8151 MiB, Nvidia laptop 7885 MiB,
both empty, headroom 256):

| Case | Arithmetic | Result |
|---|---|---|
| `gemma-4-12b`, `needs_mib: 7800`, host machine alone | 8151 − 256 = 7895 ≥ 7800 | fits |
| same, Nvidia laptop alone | 7885 − 256 = 7629 < 7800 | does not fit |
| Devstral 14K q8, estimated about 14828, both laptops | (7895 − 600) + (7629 − 600) = 14324 < 15570 | does not fit (see 6.1) |

**Optional fit check for forced profiles (`defaults.forced_fit_check`).** A
profile with `force_nodes` skips the placer's fit check by design (7.3), so a
forced load whose model needs 14.0 GB on workers with 13.5 GB free simply
starts uploading and can crash the worker. This happened on hardware on
2026-11-18: Devstral 24B forced onto one PS5 needed about 13991 MiB against
about 13.5 GB free; the upload started, the PS5 `rpc-server` crashed mid-upload
("Remote RPC server crashed or returned malformed response") and restarted by
itself. `forced_fit_check` adds a guard
with three values: `off` (the default: today's behaviour, no check), `warn`
and `reject`. The comparison is the profile's need (`needs_mib` when measured,
else the estimate of this section) against the sum of the forced nodes' usable
memory as the scheduler holds it after the pre-flight (7.5). On a dedicated-VRAM
tier that is raw FREE memory with no tier headroom taken off (forced profiles
may eat into headroom there). On a tier with `shared_memory: true` (the PS5:
GPU memory is system RAM and the pre-flight reports nearly all of it as free)
it is the SMALLER of the fleet-based budget (the figure the automatic placer
uses: fleet-reported free minus the tier headroom) and the pre-flight free minus
the tier headroom, because the Vulkan pre-flight overstates what shared memory
can give (live 2026-11-19: fleet budget 12472 MiB against 13856 MiB from the
pre-flight; the smaller is used, so the plan and a real request agree). Real case, 2026-11-19: a model needing about 13991 MiB forced
onto one PS5 whose pre-flight said 14875 MiB free of 14947 would have passed
against raw free, but the load exhausted the machine's memory and the PS5 was
unreachable until a physical power cycle; its budget was 12380 MiB, which
correctly says it does not fit. On llama.cpp upstream HEAD (2026-11-19) a
forced load that does not fit takes a PS5 down until it is power cycled, so
`reject` is the recommended production setting. Messages say per node which
figure was used, e.g. `need 14691 MiB (estimated, +5% margin; the estimate is 13991 MiB), but the nodes it is forced onto have only 12380 MiB usable in total (usable 12380 MiB on
console-1 (shared memory, headroom kept, from fleet figures))`. `warn` logs one WARN line, `forced profile
may not fit`, with `need_mib`, `free_mib`, `nodes` and `measured`, and starts
anyway. `reject` fails the request before a child is started, with `503`,
reason `forced nodes do not fit` and a `Retry-After` like the other placement
refusals (free memory changes), and a plain-words message giving the need, the
free figure, the forced nodes and whether the need is `measured` or
`estimated`. It is not a load failure: the profile is not backed off and no
node is quarantined, and the nodes are released at once. `POST /api/plan`
reports the same outcome from the free figures it holds (no probe is run): in
`reject` mode `action: reject` with the message in `reason`, in `warn` mode
the usual `start` plus a `warning` field.

**Checked before any eviction.** When the forced nodes are held by other
instances, the placement is a plan with victims (8.3 step 2). Before any victim
is stopped, the forced fit check (`reject` only; and in agents membership the
always-on check of agent contract 11.2) runs on the memory each node would have
with nothing of ours loaded on it: the free figure the scheduler holds now plus
what the instance holding the node is expected to take there (its share as
`expect_mib` gives it: measured this run, else its need with the estimate
margin times its split fraction). A pre-flight figure still in force on the
node is used as is, because it was taken while the node was free. A profile
that cannot fit even then is refused at once, with the same `503` "forced nodes
do not fit" and a message that says "even with nothing loaded on it", and no
model is stopped. Before this, the victim was stopped, the node was waited for
(about 30 s on the cluster) and only then was the request refused (seen twice on
hardware: Qwen 4B unloaded for a Devstral 24B request that needed 14691 MiB on
a console with 12484 MiB usable). When the profile could fit, the eviction goes
ahead and the check runs again on the real figures after it, because the "nothing
loaded" figure is an estimate. With `warn` or `off` (fleet membership) nothing
changes. `POST /api/plan` shows the same refusal in place of `evict`.

### 7.2a Memory safety on shared-memory nodes

The PS5 workers have no container memory limit, and on the current llama.cpp a
load that exhausts a PS5's memory makes the whole machine unreachable for about
18 minutes. So the scheduler plans loads that fit, and watches while they run.
Usable memory per PS5 is about 12.4 to 12.5 GB (fleet free minus the 1024 MiB
tier headroom); the Vulkan pre-flight (about 14.9 GB free) overstates it, so
for a `shared_memory` node every fit decision, automatic or forced, uses the
smaller of the fleet-based and the pre-flight budget.

**Per-node fit (`defaults.split_skew_margin`, default 0.15).** A split model
does not land evenly: `--tensor-split 33,33,34` of a 7.1 GB model gave buffers
of 2210, 2048 and 2519 MiB (the largest 11% over an even share). For every
placement of two or more nodes that includes a shared-memory node, each such
node's expected share must fit its own budget:

    share_i = need x fraction_i x (1 + split_skew_margin)  <=  budget_i

`fraction_i` comes from the split the child is given (`instance.SplitFractions`:
the profile's `tensor_split` for a forced placement, the node budgets for an
automatic one). A single-node placement takes no margin, and dedicated-VRAM
nodes are not checked this way.

- Automatic placements: the placer applies the rule in its search
  (`placer.Request.SkewPermille`, `Candidate.Shared`). Before this rule the
  split followed the budgets, so every node's share equalled its budget share
  of the need and only the total was checked, which left no room for skew. Now
  a set whose total fits but whose split would overfill a shared-memory node is
  not chosen: the placer takes another set, or adds a node (up to `max_pool`),
  and refuses (503 "does not fit", with `required` including the margin) only
  when nothing fits. The pre-flight re-check applies the same rule to the exact
  figures.
- Forced placements: checked with `forced_fit_check` `reject` (503 "forced
  nodes do not fit", message naming the node, its expected share and its usable
  figure, for example `node c: expected share 10868 MiB (need 27793 MiB x 34% of
  the split x 1.15 skew margin) but usable 10000 MiB (shared memory, headroom
  kept, from fleet figures)`) or `warn` (the same text in the WARN log and
  `POST /api/plan` `warning`); `off` is unchanged.

**Measured sizes are learned.** After a load succeeds, the memory the child's
log reported (the `memory_mib` total and each node's figure) is kept for the
life of the process, keyed by profile. A later fit decision for the same profile
uses it as `measured this run` instead of the estimate (no estimate margin); the
per-node figures replace the skew margin when the node set and the split are the
same. A learned figure never lowers a configured `needs_mib`; if it is higher,
one WARN per profile (`configured needs_mib is too low`, both numbers) is logged
and the higher figure is used. `GET /api/status` shows them in `learned_needs`.

`GET /api/status` also carries `last_refusals`: per profile, the newest answer that refused it without serving it (`profile`, `at`, `status`, `reason`, `message`, `bench_run`). `POST /api/load` answers 202 before the fit and pre-flight checks have run, so a client that started a load compares `at` with the time of its call to learn why no child started. The benchmark runner's `load` phase does this and records outcome `refused` with that message.
Nothing is written to the config.

**Memory watch during a load (`defaults.load_memory_floor_mib`, default 768, 0
= off).** See 8.4. On these machines the worker allocates its whole buffer
within the first seconds of a load, so the fleet's available figure during the
load already contains the model: the rule is simply "available now is below the
floor". A stale fleet view disables it.

**Concurrent placements.** A node held by an instance (from its placement
through its load and for as long as it runs), or claimed by a queue entry, is
not offered to any other placement; after the instance goes the node settles and
is re-read before it is used again (7.1). So two loads cannot both count the same
free memory, whatever the fleet figures say in the meantime. Tests:
`TestConcurrent_*`.

### 7.3 Choosing nodes

The placer is given a set of usable nodes with budgets, and returns a
placement or nothing.

1. `force_nodes` set: the placement is exactly those nodes, in that order, with
   no fit check. If any is not in the usable set, there is no placement.
2. Otherwise, for each tier level `t` in priority order, using the nodes in
   tiers up to and including `t`:
   a. If any single node fits: take the one in the best tier; among equals, the
      smallest budget that still fits.
   b. Else, if a pool of at most `max_pool` nodes fits: take the fewest nodes;
      among equal sizes, the best worst-tier, then the largest total budget.
   c. Else go to the next tier level.

So a pool of PS5s is chosen before anything that touches the Nvidia laptop, and the
host machine is reached only when tiers 1 and 2 together cannot hold the model.

### 7.3a Child output on disk

The scheduler keeps only the last 200 lines of each child's output in memory
(used for the 502 body). Benchmarks need all of it: the KV cache size, the
memory breakdown and the exact out-of-memory error. When `child_log_dir` is set,
every child (`kind: llama` and `kind: command`) writes its complete stdout and
stderr to a new file there, one file per start, named
`<UTC time>-<profile>-<instance id>.log`, for example
`20261118T064512Z-gemma-4-12b_48k-7.log` (characters other than letters,
digits, `-` and `.` in the profile name become `_`). The in-memory tail is
unchanged. The `child started` log line and `GET /api/status` (`log_file` on
each instance) give the path of the current or last file.

- Empty or unset `child_log_dir`: off, nothing changes.
- `child_log_keep` (default 200): when a new file is created, the oldest of the
  scheduler's own files beyond that count are deleted. Other files in the
  directory are never touched.
- Never in the way of a load: at startup the directory is created if needed and
  test-written; if that fails the scheduler warns and runs with the feature off.
  A later failure to create or write a file logs one warning and the child runs
  on without a file.
- Compose mounts `./logs` on the host at `/var/log/llm-children`.

### 7.4 Command line

Built in this fixed order; `extra_args` always last:

```
llama-server-rpc --host 127.0.0.1 --port <p> --model /models/<file>
  --n-gpu-layers 999 --fit off --ctx-size <ctx × parallel> --parallel <n> --jinja
  --cache-ram <cache_ram_mib> -lv 4
  [--rpc ip1:port,ip2:port]              # remote nodes, placement order
  --device <names>                       # exactly the chosen devices, always present
  [--tensor-split a,b,c]                 # same order as --device; omitted for one device
  [-ctk <q> -ctv <q>] [--flash-attn on] [extra_args...]
```

- Device names: the i-th remote node is `RPC<i>` (H2: one device per worker);
  the local node is `local_device`. `--device` lists them in placement order.
- `--tensor-split` values are each node's budget from the pre-flight figures,
  as integers of at least 1, unless the profile gives `tensor_split`.
- Local only: no `--rpc`, `--device CUDA0`.
- `-lv 4`: llama.cpp prints the per-device buffer sizes only at verbosity 4
  or higher (the default is 3), so every llama child runs with it. Child
  output is kept in a bounded buffer and is not copied to the scheduler's log.
  All spellings of the verbosity flags are managed flags.
- A llama child whose placement has no local node is started with
  `CUDA_VISIBLE_DEVICES=""`. Without it llama.cpp initialises CUDA whatever
  `--device` says and takes about 100 MiB of the host machine's VRAM per child.

### 7.5 Pre-flight

The first thing a `starting` instance does, before any child is started, is
probe every remote node in its placement, in parallel, one process per node:

```
CUDA_VISIBLE_DEVICES="" llama-server-rpc --rpc <ip:port> --list-devices
```

with every `LLAMA_ARG_*` variable removed and a 10 s timeout (H3). A probe
**succeeds** when the process exits 0 and stdout has exactly one line starting
with `RPC` in the form shown in 3.1. Lines for other devices are ignored. Any
other result (non-zero exit, death by signal, timeout, no `RPC` line, more
than one) is a **failure**.

- **A node fails:** it is quarantined (8.4). A worker with more than one `RPC`
  line is logged with a message telling the owner to pin `-d` on that worker.
- **All succeed:** each node's free MiB becomes its pre-flight override (7.1).
  The placer then re-checks the fit on exactly these nodes with the exact
  figures. If it fits, the child is started with a tensor split from them.

If any node failed, or the exact figures do not fit, the instance is
**returned**: it goes straight to `absent`, its nodes are released without
settling (no child ran), and its waiters go back into the profile's queue
entry. A returned entry keeps its original creation time, and so its place in
the queue, and its accumulated unclaimed time. The next pass sees the
quarantine or the override, so it does not repeat the same placement.

A failed probe's reason leads with the worker's own message (for example
`Failed to connect to <addr>`, or `no answer within 10s`, or `no RPC device
line in output`). The probe hides CUDA on purpose, so llama.cpp's
`ggml_cuda_init` failure, backtrace frames and blank lines are dropped from
that summary; the raw stderr is logged at debug level.

The local node is not probed; `nvidia-smi` already gives exact figures.
No child is ever started against a worker that would hang it.

### 7.6 Load timeout

Unless the profile sets `load_timeout`:
`120 s + 2 × Σ over remote nodes (node's share of the model file ÷ link_mb_s)`.
A share is the file size times that node's fraction of the tensor split.
Sizes are in bytes and `link_mb_s` is decimal megabytes per second.
Example: 10 GB (10^10 bytes) to a PS5 at 100 MB/s gives about 5 minutes (320 s).
(On the old 1 GbE link the figure was 80 MB/s, which gave 370 s.)
`kind: command` profiles use `command_load_timeout`.

**Load stall.** A hung child (seen on real hardware: a remote worker dropped
during a load, the child stayed alive doing nothing, and the acknowledged
bytes stayed at 549 for 732 s) would otherwise hold the load until this
timeout, an hour on the benchmark profiles. So a load also fails when its
upload stops moving: `defaults.load_stall_timeout` (default `180s`; `0`
disables; profile override of the same name, where an explicit `0` disables it
for that profile). It trips when all of these hold: the progress source is
`measured` (7.6a); the total acknowledged bytes have not risen for the whole
timeout; and the upload is not finished (bytes sent below 99% of
`bytes_total`). The clock starts at the first measured reading and restarts
every time the byte count rises, so the quiet start of a large model (0 bytes
for 10 to 15 s while the file is mapped) is covered by the default, and a load
that never sends anything is stalled too. It never applies to an estimate, to
a local-only load, before the first measured reading, or after the upload is
complete (context creation and warmup are quiet by nature). A stalled load is
a load failure (8.4): the child is stopped as for a load timeout, waiting
requests get `502` `load failed` with `load stalled: no bytes acknowledged by
the workers for 3m0s (sent 549 of 14334446752)`, the profile is backed off,
nodes are not quarantined, and `recent_loads` / `loadFinished` record
`outcome: "failed"` with that reason. An ERROR line `load stalled` carries the
profile, bytes, the quiet time and each worker's bytes.

### 7.6a Load progress

The child prints nothing while it uploads the model to the remote workers (the
log stops at `load_tensors: loading model tensors, this can take a while...`),
and a PS5 load takes minutes. While an instance is `starting`, `GET /api/status`
shows how far it is. All fields are in the instance object and are left out
once the instance is ready:

| Field | Meaning |
|---|---|
| `load_elapsed_s` | Seconds since the instance was created. Always present while starting. |
| `load_bytes_total` | Bytes the remote workers must receive: the model file size times the remote nodes' share of the tensor split. Absent for a placement with no remote worker (local GPU only: nothing is uploaded, so only `load_elapsed_s` is shown). |
| `load_bytes_sent` | Bytes the workers have acknowledged. Only when measured. |
| `load_progress` | 0 to 1, never above 0.99 until the instance is ready. Absent when it cannot be known (not measured and no `link_mb_s` figure in the nodes' tiers). |
| `load_eta_s` | Seconds left at the current rate. Absent while a measured load has had no new acknowledged bytes for more than 15 s (a stale figure would only mislead). |
| `load_progress_source` | `"measured"` or `"estimated"`. |

`GET /running` gains one number, `load_progress`, on an entry that is loading a
remote model, and the structured `load` object and `recent_loads` described
below; its other fields are unchanged (Appendix A.1). The scheduler also
logs one INFO line, `load progress`, every 30 s per loading instance with
`elapsed`, `percent`, `eta`, `source` and the byte counts; when the bytes have not moved for over 15 s it says `stalled=<duration>` instead of `eta`.

*Measured.* On Linux the scheduler reads the kernel's TCP statistics for the
child's connections to the workers (netlink `sock_diag`, the source of
`ss -ti`) and adds up `bytes_acked`: what the workers have acknowledged. The
child shares the scheduler's network namespace, and only the sockets of the
child's own process group whose peer is a worker's rpc address are counted.
`/proc/<pid>/io` was tried first and does not work: its `wchar` counts
`write(2)` but not `send(2)`, which llama.cpp's RPC client uses (a test proves
it: 64 MiB sent with `sendto` leaves `wchar` at a few dozen bytes). The count is read at
most once a second, in its own goroutine and never on the scheduler's event
loop. A read error, or no connection to a worker yet, falls back to the
estimate until a read succeeds again; the figure never goes backwards.

*Estimated* (also on any platform but Linux). `progress = elapsed × rate ÷
bytes_total`, where `elapsed` is the time since the child started, `rate` is
the slowest `link_mb_s` among the remote nodes divided by the number of loads
uploading at that moment (they share the host's one network port: measured
2026-11-18, about 75 to 84 MB/s in total, split equally), capped at 0.99.
That split was measured on the old 1 GbE link. On the 2.5 GbE link, loads to
different PS5s no longer share the rate, so the estimate is pessimistic for
simultaneous loads (it shows too little progress and too long an ETA). The
measured figure, the normal case on Linux, is not affected.

#### The `load` report and `recent_loads`

A consumer that wants more than one number reads the structured report. A
**load attempt** (a session) starts when its child starts and ends when the
instance is ready, the load fails, or it is cancelled. Pre-flight is not part
of it: an instance that never starts a child has no session, shows no `load`
and leaves nothing in `recent_loads`.

**`load`**, on each `GET /running` entry and on the same instance of
`GET /api/status`, while the child loads (absent otherwise). The flat fields
above stay exactly as they are.

```json
"load": {
  "session_id": "ld-1794995021087-3",
  "started_at": 1794995021087,
  "elapsed_s": 42.1,
  "bytes_total": 11628266644,
  "bytes_sent": 5580799366,
  "progress": 0.4799,
  "eta_s": 605,
  "source": "measured",
  "workers": [
    {"addr": "192.0.2.101:50052", "bytes_total": 5814133322, "bytes_sent": 5580799366, "state": "receiving"},
    {"addr": "192.0.2.122:50052", "bytes_total": 5814133322, "bytes_sent": 0, "state": "waiting"}
  ]
}
```

| Field | Meaning |
|---|---|
| `session_id` | Unique per load attempt and the same for its whole life; it is the `session_id` later seen in `recent_loads` and in the load events (A.3). Built as `ld-<start time in unix ms>-<instance id>`: the instance id restarts from 1 with the scheduler, the start time makes it unique across restarts. |
| `started_at` | Unix ms at which the child started. |
| `elapsed_s` | Seconds since `started_at`. (`load_elapsed_s` counts from the instance's creation, so it also includes pre-flight.) |
| `bytes_total`, `bytes_sent`, `progress`, `eta_s`, `source` | The same figures as `load_bytes_total`, `load_bytes_sent`, `load_progress`, `load_eta_s`, `load_progress_source`, with the same omissions: `bytes_total` is absent for a load with no remote worker, `bytes_sent` unless measured, `progress` (0 to 0.99) and `eta_s` when unknown or when the bytes have not moved for more than 15 s. |
| `stalled_s` | Seconds since the acknowledged bytes last rose. Present only on a measured, unfinished upload when that is above 15; `eta_s` is absent then. At `load_stall_timeout` the load fails (7.6). |
| `workers` | The remote workers in `--rpc` list order. Workers receive one at a time, but llama.cpp decides the order, so it is not guaranteed to match the array. Always present, empty for a local-GPU-only load. |

A worker is `{addr, bytes_total, bytes_sent?, state}`:

- `bytes_sent` is that worker's own TCP acknowledged-byte count (the netlink
  read of 7.6a returns one figure per worker address as well as their sum). It
  never goes backwards, even if a socket closes. It is absent when `source` is
  `estimated`, or when a reading carried no per-worker figures.
- `bytes_total` starts as the worker's tensor-split share of the model file
  (the split the scheduler passes to the child: the placement's, else the
  profile's, else the node budgets, so equal shares for equal budgets). When
  the child's output prints the real size, it replaces the estimate: the
  scheduler reads the `model buffer size` line of each remote device from the
  same load log that fills `memory_mib` (`load_tensors: RPC0[192.0.2.101:50052]
  model buffer size = 6686.84 MiB`; the device label is `RPC0[...]` for every
  worker, so the address inside the brackets is the key; MiB is 1048576
  bytes). The totals sum to roughly `bytes_total`; they are not forced to.
- `state`: the real upload is one worker at a time, in an order llama.cpp decides (not guaranteed to be the `--rpc` order). Each state comes from that worker's own `bytes_sent` only.
  `waiting`: less than 1 MiB acknowledged (the handshake). `receiving`: at
  least 1 MiB and not done. `done`: `bytes_sent` reached `bytes_total` within
  1%. When `source` is
  `estimated` nothing is known per worker, so every worker is `waiting`
  (a guess would be wrong); the overall `progress` is still given.

`/running` is served from a snapshot taken after each scheduler step, so
`elapsed_s` and the figures can be up to one step old (about a second while
the bytes are measured, up to 30 s on an estimate); `/api/status` is computed
on request.

**`recent_loads`**, a top-level key of `GET /running` (next to `running`,
whose entries are unchanged) and of `GET /api/status`. It is always present,
an empty array when there is nothing to show. It holds the outcomes of load
attempts that started a child, newest first, each kept for
`RecentLoadRetention` (120 s) after it finished:

```json
{"session_id": "ld-1794995021087-3", "model": "big", "workers": ["192.0.2.101:50052", "192.0.2.122:50052"],
 "outcome": "ready", "reason": "", "finished_at": 1794995221087, "load_s": 215.1}
```

- `outcome`: `ready`; `failed` (the child exited during the load, the load
  timeout passed, or a worker node was lost); `cancelled` (an unload or
  shutdown during the load, or an eviction).
- `reason`: plain words, empty for `ready`. For `failed` it is the load error
  (`child exited during load: exit status 1`, `load timeout (2m0s) passed`);
  for `cancelled` it is `unloaded while loading` or `scheduler shutting down`.
- `workers`: the remote workers' addresses (empty for a local load).
- `finished_at`: unix ms. `load_s`: seconds from `started_at` to the end.
- A load refused before a child starts (no fit, backoff, failed pre-flight,
  child that could not be started) does not appear.

### 7.7 Agent membership and worker launches

With `defaults.membership: agents` (read at start; default `fleet`) the pool
comes from `potluck-agent` (docs/agent-contract.md) and the fleet feed is not
read at all. `internal/pool` holds the agent registry, implements the node
view the core reads, and runs the worker launches.

- **Programs.** `--programs-dir` (`LLM_SCHEDULER_PROGRAMS_DIR`) holds
  `<release>/release.json` and its folders. At start every file named in each
  folder's `SHA256SUMS` is hashed; one missing or wrong file means the release
  is not served: the scheduler still starts, `/api/system` `programs.error`
  says why, agents are refused (`release_unavailable`) and loads are refused.
  Potluck's release is the build variable `main.potluckRelease` (default
  `agentproto.Release`, `0.1.3`), separate from `version`.
- **Same build.** `server_bin --version` must report the commit of
  `builds.llama` (a prefix match of at least 7 hex digits). Otherwise every
  load is refused with the reason, and `/api/system` shows `host_build_error`.
- **Nodes.** Node id = agent id, address = the agent's `addr` (worker
  `addr:50052`, agent `addr:50060`). Tiers match an agent node on what its
  agent reports: `match.platform` (exact) and `match.device` (a glob on a GPU
  model of the registration or a device name of the heartbeat); `hostname`
  (the agent's `name`) and `source: agent` still work. A node that matches no
  tier is listed with `unplaced: no_tier_match` and takes no load; a node
  whose programs are not ready, whose model source a worker launch needs and
  cannot reach, or that reports a launch Potluck does not hold is listed with
  `programs_not_ready`, `model_source_unreachable`, `orphan_launch` or `silent` and
  takes no load until that passes. Placement memory is the first device of the
  node's platform backend; for a device that shares system memory the usable
  figure is `min(device free, mem_available_mib - watchdog_floor_mib)`
  (`watchdog_floor_mib`, default 1000). One device per node: a node that
  lists more than one device of its backend uses the first; its status shows
  `extra_devices_ignored` and the log warns once per registration. The local
  node still comes from
  `local_probe_cmd`; an agent whose id or name is `fleet.local_node` is the
  host machine's and is not published as a remote node.
- **States.** A node is lost after 10 s without a heartbeat (shown offline;
  the load on it is killed at once, no recovery request is ever sent),
  `left` after its deregister, `removed` after `DELETE /api/nodes/{id}` until
  `POST /api/nodes/{id}/readmit`. Pause and removal are stored in the
  database (`node_admin`). A paused node takes no new load; a load on it
  continues. A pause also breaks a claim on the node for a waiting request
  (step 5 of the queue), so the request is planned again.
- **Loads.** The worker launches replace the pre-flight: no `--list-devices`
  probe runs, so nothing holds a worker's single client slot before the child
  connects. Each remote node gets `POST /launch` (role worker, the release,
  a 20 s lease, `min_mem_available_mib` = the watchdog floor, plus the node's
  budget on a shared-memory device, `model_dir` from `worker_model_dir`).
  The child starts once every launch is `ready`; a launch not `ready`
  within `worker_start_timeout` (default 15 s) fails the load with
  `worker_start_timeout`. Leases are renewed every
  5 s; the state is read 4 times a second while starting, then each second;
  the launch log is copied to `child_log_dir` as
  `worker-<name>-<launch id>.log`. After the instance is gone (its child has
  exited, or none ran) the launches are stopped.
- **Failures.** An agent refusal fails the load with the agent's reason and
  message (history `workerLaunchFailed`); `model_source_unreachable` keeps
  the request queued and leaves the node out for 5 s, until the queue timeout.
  A launch that exits fails the instance (or stops a ready one) and stops the
  other members (history `workerExited`); `watchdog` and `oom_kill` raise the
  profile's learned need by a tenth (at least 256 MiB). A Docker agent that
  restarts during a launch ends it with `agent_restarted`. After a scheduler
  restart no launch is held: the next heartbeat gets `410` and the agent's
  re-registration is answered `stop_launch`.
- **Force unload** (`POST /api/nodes/{id}/force-unload`, MCP
  `force_unload_node`): the instance on the node is killed; requests waiting
  for it fail at once with a 502 naming the force unload and the node
  (unlike node loss, they are not put back in the queue), and requests in
  flight are cut off as with any kill. The profile is not backed off. That
  node's launch gets `DELETE ?force=1` and, if it is
  not `exited` within 5 s, `POST /reset`. The other members get a normal
  stop; a lost node is not waited for.
- **Status.** In agents membership each node of `/api/status` `view.nodes`
  has an `agent` object (state, install, release, platform, programs, model
  source, paused, launch id and state, devices, the last stats object as raw
  JSON) and `agents` lists every agent node, lost, left and removed ones too.
  A stats object whose `stats_version` is not 1 is not kept and shows
  `stats_note: "stats version not supported"`; a missing or bad `stats` never
  rejects a heartbeat.

## 8. Runtime behaviour

### 8.1 Instance states

```
absent ─▶ starting ─▶ ready ◀─▶ draining
              │          │          │
              └──────────┴──────────┴──▶ stopping ─▶ absent
```

| State | Meaning | `/running` shows |
|---|---|---|
| `starting` | Nodes held; pre-flight, then child running and polled for health. | `starting` |
| `ready` | Serving. Tracks `in_flight`, `ready_at`, `last_used`, `load_duration`. | `ready` |
| `draining` | Finishing in-flight requests; admits no new ones. Returns to `ready` if the claim it drains for is dropped. | `ready` |
| `stopping` | SIGTERM to the child's process group; SIGKILL after 10 s. Its nodes are released when the process has exited, then settle. | `stopping` |

At most one instance per profile. Stopping a child on Linux is SIGTERM to its
process group, then SIGKILL; on Windows (development only) it is an immediate
kill of the process tree.

### 8.2 Request path

1. Read the model name: JSON `model`, or the `model` form field for multipart
   requests. Resolve aliases; log alias hits with the caller's address.
   Unknown model → `404` in OpenAI error format.
2. If the profile is in backoff (8.4) → `503` at once, whatever else is true.
3. By instance state:
   - `ready` → proxy; `in_flight++` until the response ends or the client goes.
   - `starting` → wait for it, then proxy.
   - `draining`, `stopping`, or no instance → join that profile's queue entry,
     creating it (with the current time) if there is none.
4. All waiting requests for one profile share one entry.
5. A client that disconnects while waiting is removed, whether it was waiting
   in an entry or on a `starting` instance. A `starting` instance carries on
   loading with no waiters; it becomes `ready` and later unloads by `ttl`.
6. `POST /api/load` counts as a waiter that never disconnects. It is removed
   when the instance becomes `ready` or the entry is answered with an error.

### 8.3 Queue, claims, eviction

The queue holds at most one **entry** per profile, ordered by creation time.
An entry is in one of two conditions:

- **Unclaimed.** It has waiters and nothing set aside for it. Its
  `queue_timeout` clock runs, except while any instance anywhere is
  `starting` (a load in progress is the cluster working, not the cluster
  stuck). When its last waiter leaves, the entry is deleted.
- **Claimed.** A set of nodes (its **claim**) is set aside for it, and zero or
  more instances (its **victims**) are stopping or draining to free them. Its
  clock is stopped.

After **every** event the core runs one pass over the entries, oldest first.
An entry whose profile currently has an instance in any state is skipped: the
old instance must be gone before a new one is considered. When an instance
becomes `ready`, or returns to `ready` from `draining`, its profile's entry
(if there is one) is deleted and its waiters are admitted to the instance.

For each remaining entry:

**Unclaimed entry**

1. **Place.** Run the placer over the free nodes with live budgets. If it
   returns a placement: hold those nodes, create the instance in `starting`,
   move the waiters to it, delete the entry.
2. **Plan.** Otherwise run the placer over the free nodes (live budgets) plus
   the nodes of every *evictable* instance (plan budgets). If it returns a
   placement: its nodes become the entry's claim, and every instance holding
   one of them becomes a victim. Idle victims go to `stopping`; busy victims go
   to `draining`. The entry is now claimed.
   If several plans are possible, the placer's tier rules pick the nodes;
   ties go to the plan with the fewest victims, then idle victims over busy,
   then the shortest `load_duration`, then the longest idle.
   A forced profile's plan is first checked on the nodes' memory with nothing
   of ours loaded (7.2, "Checked before any eviction"): one that cannot fit
   even then is refused before any victim is stopped. A node that takes no new
   load (paused, or listed with an unplaced reason) is not offered as an
   evictable node, so its instance is never stopped for a load the node may
   not take.
3. **Skip.** Otherwise the entry stays unclaimed and the pass moves on.

An instance is **evictable** for an entry when it is `ready`,
`now − ready_at ≥ hold`, and either `in_flight == 0`, or the entry has been
unclaimed for at least `drain_after`. `hold = clamp(load_duration, min_hold,
max_hold)`: a model is guaranteed to serve for as long as it took to load
(at most `max_hold`), so two alternating models do not spend all their time
loading. With no cached reloads on the PS5s (3.2), a reload always costs the
full upload, so the hold is worth keeping.

**Claimed entry**

4. When every victim has exited and every claimed node has settled, run the
   placer over exactly the claimed nodes with live budgets. If it returns a
   placement, start it as in step 1 (nodes it does not use are released).
   If it does not (something else is using the memory): record each claimed
   node's observed free figure as its `empty` figure (7.1), drop the claim, and
   treat the entry as unclaimed in this same pass. The recorded figures stop
   the same plan from being chosen again.
5. If a claimed node stops being eligible (offline, quarantined, or the fleet
   view goes stale), or stops taking new load (paused, or listed with an
   unplaced reason, after the claim was made): drop the claim. Pause wins
   over a claim: the paused node does not get the load. Victims still `draining` return to
   `ready`. Victims already `stopping` or gone stay so. The entry is unclaimed
   again and is evaluated in this same pass.
6. A claimed entry with no waiters:
   - If none of its victims has been stopped yet (all are still `draining`),
     it is deleted and its victims return to `ready`.
   - Otherwise it stays and loads when its nodes are ready, because a model
     was already unloaded for it.
   - If its claim is dropped (steps 4 or 5) while it has no waiters, it is deleted.

Rules that hold throughout:

- Claimed nodes are invisible to every other entry, so a later entry can never
  take what an earlier one is waiting for. A later entry that fits on unclaimed
  free nodes still goes ahead of an earlier one that does not.
- A request admitted to an instance is never cut off by scheduling. A victim
  with a long generation drains for as long as that generation takes, and the
  claimed entry waits for it, as llama-swap does.
- An unclaimed entry whose clock passes `queue_timeout` answers
  its waiters `503` with `Retry-After: 30` and a JSON error naming the reason:
  cluster busy, forced nodes unavailable, or fleet view stale. A forced node
  that is gone is failed sooner, see 8.4.
- **Idle unload:** `ready`, `in_flight == 0`, `now − last_used ≥ ttl` →
  `stopping`. Paused for instances on remote nodes while the fleet view is
  stale, since they could not be reloaded.
- Known cost: free nodes inside a claim sit unused while the claim's victims
  drain. Accepted for v1.
- Known behaviour: an idle LLM instance that includes the host machine's GPU is evicted
  when Whisper or FLUX is requested, and may evict them back later. `hold`
  limits how often.

### 8.4 Failures

| Event | Response |
|---|---|
| Pre-flight fails for a node | Node quarantined 30 s, doubling per repeat up to 10 min, reset by a successful pre-flight. The instance is returned (7.5). |
| A node a load runs on (shared-memory tier) reports available memory below `load_memory_floor_mib` (default 768) at any time during the load, in a live fleet view (7.2a) | Cancelled at once: the child is killed, waiters get `502` (as for any load failure) with `load cancelled: <node> ran low on memory during the load (available N MiB, floor M MiB)`, the outcome in `recent_loads` / `loadFinished` is `failed`, an ERROR line gives every watched node's figure, and the profile is backed off as below. The node is not quarantined. A stale view, an unreported figure, an offline node and a dedicated-VRAM node are not watched. The fleet figure is `mem_available_mib` when the fleet sends it, else the existing RAM figure. |
| Child exits, the load timeout passes, or the load stalls (7.6: measured upload, no new acknowledged bytes for `load_stall_timeout`, default 180 s), during `starting` | Instance → `stopping`. Waiters get `502` with the last lines of the child's output (a stall says `load stalled: no bytes acknowledged by the workers for 3m0s (sent 549 of 14334446752)`). The **profile** is backed off 60 s, doubling per repeat up to 10 min, reset when the profile next reaches `ready`. During backoff its requests fail fast with `503`, and any entry for it is answered `503` and deleted. Nodes are not quarantined: the pre-flight already cleared them. |
| Child exits while `ready` or `draining` | In-flight requests fail; instance → `absent`. |
| A remote node in use is offline in, or absent from, a live view for 30 s continuously | Node loss: instance → `stopping` with SIGKILL; in-flight requests fail. Needed because a blocked RPC socket does not make the child exit. The 30 s counter resets whenever the feed reconnects, so a fleet console restart is not read as every node vanishing. The node is then kept as `hung` (8.4a) until a live view shows it online again, so a recovery request (8.4b) can be sent for it. |
| A forced node (`force_nodes`) is absent from, or offline in, a live fleet view for `forced_node_grace` (default 30 s) continuously | Fail fast: the profile's waiting and new requests get `503`, reason `forced nodes unavailable`, `Retry-After: 30` (the same as the queue timeout), with a message naming the node and saying whether it is `absent from the fleet` or `offline in the fleet`, instead of waiting out `queue_timeout`. The grace lets a node that is re-registering (fleet console or PS5 worker restart) come back. The clock restarts when the feed reconnects and does not count while the view is stale (then today's behaviour applies: the request queues and the "fleet view stale" reason can follow). A forced node that is present and online but busy is not a fail-fast case; it queues. Excluded nodes fail as before (6). |
| A streaming response sends nothing for `stall_timeout` **after its first chunk** | That request is aborted with an error event. The instance is not stopped. Time before the first chunk (prompt processing) is never timed. |
| A non-streaming request hangs | Not detected in v1, other than by node loss. `POST /api/unload` with `force` clears it. |
| A remote worker in use is `hung` (8.4a: two health channels silent, or one channel alone silent for 3 x `worker_silence` with nothing known ok: the heartbeat, or the connect probe when enabled) | Log ERROR `worker hung`, the node is quarantined, `workerHealth` event and `health` in `/api/status`. Detection only: instances are not stopped by this rule; node loss, load stall and the load timeout do that, unchanged. A node that node loss killed an instance on stays `hung` after the instance is gone (8.4a). |
| Fleet feed down | Reconnect SSE with backoff; poll `GET /api/fleet` every 5 s meanwhile. View age is the time since the last byte from the feed, pings included. Loaded instances keep serving. Past `stale_after`, remote nodes are not eligible; the local node still is. |

### 8.4a Worker health

A remote worker that hangs does not always take its container with it. Seen on
real hardware: a PS5 ran out of memory, its `rpc-server` stopped answering,
its container stayed "running", the machine stopped reporting to the fleet
manager after about 10 s, and it was unreachable for 18 minutes. The rules of
8.4 and 7.6 act on that (node loss at 30 s, load stall, load timeout), but
nothing said in one place "this worker is hung", nothing looked at the
worker's own port, and nothing covered a model that was already loaded and idle.
This feature is **detection and reporting only**: it sends nothing to a worker,
kills nothing and starts nothing (an optional, separate feature, 8.4b, can ask
the fleet manager to relay a recovery request for a hung worker). Node loss, load stall and the load timeout
keep ending instances exactly as before.

*What is watched.* Every remote worker address that currently backs an instance
that is loading (child running) or ready or draining. A worker no instance uses
is never probed and has no health.

*Three channels per worker.*

| Channel | Source | `silent` when | `unknown` when |
|---|---|---|---|
| `upload` (A) | The measured acknowledged-byte count of that worker (7.6a). | The worker is `receiving` and has acknowledged no new bytes for `worker_silence`. | The worker is not receiving (waiting, done, no load), or the count is only estimated. |
| `heartbeat` (B) | The fleet view (the same counter as node loss, 8.4). | The node has been offline in, or absent from, a **live** view for `worker_silence`. | The view is stale. A stale view never makes this channel silent. |
| `probe` (C) | **Off by default.** With `worker_probe_interval` above 0: a plain TCP connect to the worker's rpc address every interval, 2 s timeout, closed at once, nothing sent. It runs off the event loop and its result is an event, like every slow job. | Three consecutive connect failures. | Always, when the probe is off (the default). Otherwise: it has not run yet, or may not run now (below). |

Otherwise a channel is `ok`. Each channel's `silent_for_s` counts from the
moment it became silent (for the heartbeat that is `worker_silence` after the
node went missing; for the probe it is the third failure).

*Why the probe is off (hardware finding).* A TCP connect is not a valid health
signal for these workers and is harmful. An `rpc-server` accepts one client and
does not call `accept()` again until that client leaves, so extra connections
are completed by the kernel's listen backlog: the first few probes "succeed"
whatever state the server is in, each opened-and-closed probe stays in the
backlog, and after about a minute of 10 s probes the backlog was full and every
connect timed out. Healthy PS5s serving a model then read `probe: silent`, state
`suspect`. The same applies to any external TCP check of port 50052 (monitoring,
load balancer health checks, `nc -z`): do not point one at a worker. Health
tracking (upload, heartbeat, state, quarantine, status, event) is therefore
always on, and `worker_probe_interval` only controls the probe channel, default
`0` = never runs. The probe code stays for workers that accept parallel
connections.

*When the probe does not run (even when enabled).* These workers serve one client at a time, and it
is not known whether an extra connection that is opened and closed without
sending anything disturbs a live session. So the probe is skipped, and its
channel is `unknown`: (a) for a worker that is `receiving` an upload (7.6a), and
also while an upload to it cannot be ruled out because the byte count is only
estimated; (b) while an instance using the worker has a request in flight, unless
`defaults.worker_probe_during_inference` is `true` (default `false`). The
price of (b): a worker that hangs during a generation shows only the heartbeat
channel until the request ends.

*State.* `ok`; `suspect` when exactly one channel is silent (unknown does not
count as silent); `hung` when at least two channels are silent, or when one
channel alone has been silent for 3 x `worker_silence` and nothing else is known
ok: the heartbeat alone (the normal case with the probe off: an idle loaded
model, so no upload is going on, whose node stopped reporting; with the probe
enabled, a probe that is `ok` keeps it `suspect`), or the probe alone while the
heartbeat is `unknown` (a stale view: the only evidence left is the worker's own
port). Node loss (8.4) kills the instance after 30 s of missing node; with the
default `worker_silence` of 30 s the heartbeat channel only turns silent at that
same moment, so the heartbeat-alone escalation is never reached first. That is
what the *lost node* rule below is for. Once
hung, a worker is `ok` again only when no channel is silent and at least one is
known to be healthy; unknown channels do not block that, but they do not prove
recovery either.

*Effects.* Entering `hung` logs one ERROR line `worker hung` (`node`, `addr`,
`silent`: each silent channel with how long, `instances`: the profiles on it)
and quarantines the node with the ordinary quarantine of 8.4 (30 s doubling to
10 min; the release rules, including the reset by a successful pre-flight, are
unchanged), so nothing new is placed on it. Leaving `hung` logs one INFO line
`worker recovered` with `hung_for`. The check runs before the node-loss rule
in the same step, so a worker that is hung when node loss kills its instance is
still named. If the last instance on a hung worker ends, its health is dropped
and an INFO line `worker health no longer tracked` says so, with one exception.

*A lost node stays hung.* With the default settings a node that hangs (it stops
reporting to the fleet) is killed off by the node-loss rule at 30 s, which stops
its instance; no instance uses the node any more, so the tracking above would
forget it before it was ever `hung`, and no recovery request (8.4b) would be
sent, in the very case that feature exists for. So when the node-loss rule fires
for a node (it was in use and offline or absent in a live view), the node is
kept as `hung` with the heartbeat channel `silent` (the other two `unknown`),
its episode starting when the node went missing, and is quarantined (the
ordinary quarantine of 8.4). It stays tracked with no instance using it, shown in
`/api/status` with its `health` (a node that has left the fleet view altogether
is listed with only `id`, `addr`, `health` and `recovery`), until a **live**
view shows it online again. A stale view never decides the node is back. Then
it logs INFO `worker recovered` with `hung_for`, sends a `workerHealth` event
for `ok`, and the health is cleared on the next step. A node that was already
`hung` keeps its own episode start. Recovery requests, when enabled, run on this
episode exactly as for any hung node and end with it.

*Reporting.* Each node of `GET /api/status` `view.nodes` that an instance uses
has `health`; nodes no instance uses have none, except a lost node kept as hung
(above).

```json
"health": {"state": "hung", "since": 1795055906526,
  "channels": {"upload": "unknown", "heartbeat": "silent", "probe": "silent"},
  "silent_for_s": {"heartbeat": 12.4, "probe": 31.0}}
```

`since` is unix ms, when the state was entered. `silent_for_s` has only the
silent channels. `GET /api/events` sends a `workerHealth` event whenever a
worker's state changes (A.3).

*Configuration* (`defaults`): `worker_probe_interval` (default `0`: the probe
never runs; the rest of the feature is always on), `worker_silence` (default
`30s`; must be positive), `worker_probe_during_inference` (default `false`;
only with the probe on). Negative values fail validation.

### 8.4b Recovery requests

Optional, **off by default**, and separate from 8.4a: nothing is ever sent
unless `recovery.enabled` is `true`. When worker health marks a remote worker
`hung`, the scheduler asks the fleet manager to relay a *recovery request* to
that node. A small service on the node polls the fleet manager, performs the
action and reports the outcome back through the fleet data. **The scheduler
never contacts the node itself**: its only new outbound call is one HTTP POST
to the fleet manager it already talks to (`fleet.url`). There is no signing key;
the fleet manager decides which sources it accepts.

*Which nodes.* Any remote node that worker health tracks, that is, a remote
worker that backs a loading, ready or draining instance, whatever its tier
(`shared_memory` or not). Reason: `restart_rpc_server` is harmless on any
worker, and `hung` is already restricted to workers an instance was using; the
tier says nothing about whether the node can be asked to restart. Never the
local node, never an excluded node, never a node no instance uses (a node whose
last instance ends stops being tracked, which ends its episode).

*Request.* `POST <fleet.url>/api/recovery/<node_id>`, `<node_id>` being the
node's fleet `id` exactly as `/api/fleet` returns it (a MAC-style id such as
`02:00:00:00:00:01`), percent-encoded as one path segment (colons become `%3A`).
Body (JSON):

```json
{"v":1,"target_node_id":"02:00:00:00:00:01","action":"restart_rpc_server",
 "reason":"hung: heartbeat silent 95s, upload silent 40s",
 "issued_at":1795058743515,"expires_at":1795058788515,
 "nonce":"529a35e343727f1c3f403fc090490810"}
```

`issued_at` and `expires_at` are unix ms (`expires_at` is `request_ttl`, at most
60 s, after `issued_at`); `nonce` is 32 lowercase hex characters from 16 random
bytes (`crypto/rand`), new for every request. `reason` lists the silent
channels with how long each has been silent (`hung: probe silent 70s`), plus a
note on later steps (`; escalating after restart_rpc_server`) and on a retry.

*Answers.* `200` stored. `400` malformed, `403` source not allowed, `404`
unknown node, `409` duplicate nonce, `410` already expired, `422` invalid
values, `429` rate limited: **not retried** (the record is dropped; the status
shows `refused`). A `5xx` or a timeout (`recovery.http_timeout`) is retried
**exactly once**, 10 s later, with a new nonce and fresh `issued_at` and
`expires_at` (and only if the node is still hung and the view is live); if that
fails too the status stays `error`. A failed send only logs.

*Sequence.* `recovery.actions` is the ordered list (default
`[restart_rpc_server]`; at most one each of `restart_rpc_server`,
`restart_docker`, `reboot`, always in that order). Per node and hung episode:

1. On entering `hung` (view live, no request for the node within
   `min_interval`) send the first action.
2. After step N, wait `step_wait`. If the node is still `hung`, the view is live
   and `min_interval` has passed since the last request, send step N+1. A step
   whose request failed (5xx, timeout) still counts as sent and the sequence
   goes on. A step whose request was **refused** (a non-retry status: `400`,
   `403`, `404`, `409`, `410`, `422`, `429`, any 4xx) **stops the sequence for
   that hung episode**: the record never reached the node, so a later step (a
   reboot after a refused restart) must not follow on a timer. WARN `recovery
   sequence stopped` (`node`, `action`, `status`, `reason`) says so, and a
   `recoverySequenceStopped` history event is written. The next hung episode
   starts the sequence again.
3. If the node reported the nonce of the request in flight with outcome
   `failed`, do not wait out `step_wait`: send the next step as soon as
   `min_interval` allows.
4. If the node's health returns to `ok` (or no instance uses it any more) the
   episode ends and the step counter resets. `min_interval` still counts from
   the last request sent, across episodes, so a node that hangs again at once
   is not hammered.
5. At most `len(actions)` steps per episode (a retry repeats the same step).
   When the last step has had its `step_wait` and the node is still hung, one
   WARN `recovery sequence finished and worker still hung` says so, and
   nothing more is sent.

*Never on a stale view.* Every send, including the retry and each escalation,
waits for a live fleet view. A send never blocks or delays the scheduler: the
POST runs off the event loop (`effRecoveryPost`) and its answer comes back as
an event (`evRecoveryPosted`), like every other slow job.

*Outcomes.* The node's report arrives as `last_recovery_action` on its member
in `/api/fleet`: `{"action","at" (unix ms),"outcome":"ok"|"failed","reason",
"nonce","duration_ms"}`, or `null`; an older fleet manager has no such field
(parsed as absent). A new outcome (a new nonce or `at`) is logged and sent as
an event, for any remote node while `recovery.enabled` is on, including one
the scheduler did not ask for (`ours: false` in the log).

*Logging.* INFO `recovery request sent` (`node`, `action`, `nonce`, `reason`,
`step`, `of`); INFO `recovery request stored` for a `200`; WARN `recovery
request refused` (4xx) or `recovery request failed` (5xx, timeout) with
`status` and the last 200 bytes of the response `body`; WARN `recovery sequence
stopped` after a refusal; INFO `recovery outcome`
(`node`, `action`, `outcome`, `nonce`, `reason`, `duration_ms`, `ours`); INFO
`recovery episode started` and `recovery sequence reset`.

*Reporting.* Each node of `GET /api/status` `view.nodes` that was sent a
request, or whose outcome was seen, has `recovery` (omitted otherwise):

```json
"recovery": {
  "last_request": {"action": "restart_rpc_server", "nonce": "529a35e343727f1c3f403fc090490810",
                   "sent_at": 1795058743515, "status": "stored", "http_status": 200},
  "last_outcome": {"action": "restart_rpc_server", "at": 1795058743518, "outcome": "ok",
                   "reason": "rpc-server restarted", "nonce": "529a35e343727f1c3f403fc090490810",
                   "duration_ms": 1700},
  "step": 1, "episode_started": 1795058743515}
```

`last_request.status` is `pending` (posted, no answer yet), `stored`,
`refused` or `error`; `http_status` is `0` until an answer arrives (and after a
timeout). `last_outcome` is the fleet's object, as is. `step` is how many steps
this episode has sent; `episode_started` (unix ms) is present only while the
node is in a hung episode. `GET /api/events` sends a `recoveryRequest` event
when a request is sent (`phase: "sent"`) and when an outcome is seen
(`phase: "outcome"`) (A.3).

*Configuration* (top-level `recovery:` block; validated even when disabled):

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `false` | Nothing is ever sent unless `true`. |
| `actions` | `[restart_rpc_server]` | Escalation order. Allowed: `restart_rpc_server`, `restart_docker`, `reboot`; no duplicates; this order when more than one; not empty. |
| `min_interval` | `120s` | Least time between two requests for one node (across episodes). At least `60s`. |
| `step_wait` | `90s` | How long a step gets to take effect before the next one. Positive. |
| `request_ttl` | `45s` | `expires_at - issued_at`. Positive, at most `60s`. |
| `http_timeout` | `3s` | Timeout of the POST to the fleet manager. Positive. |

**`restart_docker` and `reboot` interrupt everything on that machine** (every
model it serves, every container, every other job). They are not in the default
`actions`.

### 8.5 Clarifications settled during the build

These resolve cases the rules above left open. Each is marked `// DESIGN:` in
`internal/scheduler`.

1. A returned instance (7.5) does not clear its nodes' pre-flight overrides
   when it releases them; only a release after a child actually ran does.
2. The queue pass repeats until nothing changes, so an older entry sees
   capacity that a younger entry gave back in the same event.
3. For step 5, "eligible" does not include the `min_share` test: a node held by
   a victim has almost no live budget and would otherwise void every claim.
4. `drain_after` is measured on the same clock as `queue_timeout`, including
   its pause while any instance is `starting`.
5. The 30 s node-loss counter is cleared while the fleet view is stale.
6. Node loss while an instance is `starting`: its waiters go back into the
   queue (keeping their place); the profile is not backed off.
7. If the fetch after settling fails, the node is offline, and it is fetched
   again after another settle period. The retries stop once the node is absent
   from a fresh view and nothing holds or claims it: its state is dropped, and
   the failure is logged once per outage.
8. `POST /api/unload` on a `starting` instance is refused without `force`;
   with `force` its waiters get `503`.

### 8.6 Shutdown

On SIGTERM: stop accepting; answer every queued waiter `503`; stop `starting`
instances and answer their waiters `503`; wait for in-flight requests up to
`shutdown_grace`; then stop all children. Compose sets `stop_grace_period` to
`shutdown_grace + 30 s`. The container runs with `init: true` and each child
gets its own process group, so nothing is left behind.

## 9. HTTP surface

| Path | Behaviour |
|---|---|
| `POST /v1/*` | Any path under `/v1/` whose JSON or multipart body carries `model` is routed by it and streamed through. Covers chat, completions, embeddings, rerank, responses, messages, images, audio. |
| `GET /v1/models` | Profiles (not aliases), OpenAI list format. |
| `GET /running` | llama-swap compatible. From memory; never starts anything. Appendix A. |
| `POST /api/models/unload`, `GET /unload` | llama-swap compatible: stop every instance with `in_flight == 0`. |
| `POST /api/models/unload/{model}` | llama-swap compatible: stop that instance if idle. |
| `ANY /upstream/{model}/...` | llama-swap compatible passthrough to a `ready` or `draining` instance. Never starts a load and never waits: otherwise `503` at once. |
| `GET /api/events`, `GET /api/metrics/activity` | llama-swap compatible, for the hub's llama panel. Appendix A. |
| `GET /ui/` | The read-only dashboard (9.1): one self-contained page embedded in the binary. |
| `GET /api/history`, `GET /api/history/summary`, `GET /api/history/export`, `GET /api/about` | The event history and what the dashboard header shows (9.1). Read-only. |
| `GET /ws` | The live channel, a WebSocket (9.2): hello, status, event, bench, ping. |
| `GET /api/config`, `GET /api/config/schema`, `GET`/`POST /api/config/profiles`, `GET`/`PUT`/`DELETE /api/config/profiles/{id}` | The configuration document with its revision, the settings schema `{"fields": [...]}`, and profiles (6.0.2). Section GETs are wrapped: `{"revision": N, "<section>": ...}`. Writes take `?dry_run=1`, `If-Match: <revision>`, `X-Comment`, `X-Actor`, `X-Source: ui`, and answer with the new revision, the diff and `restart_required`. |
| `GET /api/config/{tiers,settings,exclude-nodes,no-auto-nodes,node-overrides,recovery,fleet}`; `PUT` the same except settings; `PATCH` settings, recovery and fleet | The other sections (6.0.2). |
| `GET /api/config/revisions`, `GET /api/config/revisions/{n}`, `POST /api/config/revisions/{n}/rollback`, `GET /api/config/export`, `POST /api/config/import` | Revisions, rollback (a new revision), export and import (6.0.3). |
| `GET /api/audit`, `GET /api/audit/{id}` | The audit log (6.0.4): filters `since`, `until`, `source`, `actor`, `section`, `target`, `action`, `q`, `limit`, `order`. |
| `GET`/`POST /api/bench/benchmarks`, `GET`/`PUT`/`DELETE /api/bench/benchmarks/{id}`, `GET`/`POST /api/bench/suites`, `GET`/`PUT`/`DELETE /api/bench/suites/{id}`, `POST /api/bench/suites/{id}/run` | Benchmark and suite definitions (with phases) and starting a run (9.3). `503` when the `bench_enabled` setting is off or the database is not open. |
| `GET /api/bench/runs`, `GET /api/bench/runs/{id}`, `POST /api/bench/runs/{id}/cancel`, `GET /api/bench/runs/{id}/export`, `GET /api/bench/trends`, `GET /api/bench/aggregates`, `GET /api/bench/schema`, `GET /api/bench/state`, `GET /api/bench/profiles` | Runs, progress, exports, trends, aggregates, the editor schema, the engine state and the profile ids a benchmark may name (9.3). |
| `POST /mcp` | The MCP server for AI agents (9.5): the same operations as the REST API, as tools. |
| `GET /api/profiles`, `GET /api/system` | Every profile with its need and dry-run plan; configuration and storage summary (9.4). Read-only. |
| `GET /api/models`, `GET /api/models/{key}` | The model catalog: one entry per model file, with its profiles, instances, history and benchmark figures (9.4). Read-only. |
| `GET /health` | `200` whenever the scheduler is running. |
| `GET /api/status` | Instances, placements, per-device memory from the last pre-flight, claims, queue, quarantines, backoffs, node view and its age. A starting instance also shows its load progress and its `load` report (7.6a). The top-level `recent_loads` lists the outcomes of load attempts finished in the last 120 s. Each node in `view.nodes` that an instance uses also has `health` (8.4a), and a node that was sent a recovery request or reported an outcome also has `recovery` (8.4b). |
| `POST /api/plan` `{model}` | Dry run: what would happen if this model were requested now (placement, plan with victims, or the reason for neither). Changes nothing. |
| `POST /api/load` `{model}` | Start loading ahead of time. Returns `202` at once. Audited (section `runtime`). |
| `POST /api/unload` `{model, force}` | Stop one instance if idle; `force: true` stops it even if busy. Audited. |

`force: true` is the one way a running generation can be cut off, and only by
an explicit admin call.

Streaming: `httputil.ReverseProxy` with immediate flush; no response-header
timeout; client cancellation propagates upstream.

### 9.1 Event history and dashboard

The scheduler's safety and lifecycle events are kept so they can be found
later: a file-backed history inside the scheduler, queried over HTTP, and one
read-only page. There is no new service and no database. The package is
`internal/history`; the core only appends events at the places where it already
logs a fact (`internal/scheduler/history.go`).

*Never in the way of scheduling.* The core never writes a file. After each step
the runner hands the step's events to the store with `Record`, which only
appends to a bounded in-memory ring (under a mutex) and tries to put the event
on a buffered channel; if the channel is full the event is counted as dropped
(it is still in memory). A separate writer goroutine does all file I/O. A bad
`history_dir` warns once at start and history stays off; a write error warns
once and the run continues with memory only. Neither can reach the scheduler.

*Format.* `history_dir` (empty = off: nothing written, the endpoints answer with
an empty list and `"history":"off"`) holds JSON Lines files named
`events-<UTC yyyymmddThhmmssZ>.jsonl`, so a listing sorts by time. A file is
rotated when the next line would take it past `history_max_file_mib` (default
16); files beyond `history_keep_files` (default 20) are deleted, oldest first;
only files matching that name are ever touched. At start the newest events (up
to `history_memory_events`, default 5000) are loaded back into memory, and the
newest file is reused when it has room, so history survives restarts and image
swaps. A torn last line (a crash) is skipped. Compose mounts
`./history:/var/lib/llm-scheduler/history`.

*Record.* One event per line:

```json
{"ts": 1795055906526, "seq": 41, "type": "loadFinished", "severity": "info",
 "profile": "gemma-4-12b", "model_file": "gemma-4-12b.gguf",
 "nodes": ["console-4", "console-6"], "session_id": "ld-1795055840000-3",
 "summary": "gemma-4-12b finished loading in 66.1 s (101.4 MB/s).",
 "data": {"outcome": "ready", "load_s": 66.1, "bytes": 6700000000, "avg_mb_s": 101.4}}
```

`ts` is unix ms; `seq` is monotonic per process (events reloaded from an earlier
run keep theirs); `severity` is `info`, `warn` or `error`; `nodes` are hostnames
(else ids); `summary` is one plain sentence and is what the dashboard shows.
Per-request chat traffic is never recorded (only a count, in `instanceUnloaded`).

*Types and their `data`.*

| `type` | Sev. | When, and the `data` keys |
|---|---|---|
| `schedulerStarted` | info | At start. `version`, `profiles`, `listen`, `safety` (the margins, floor, stall and silence settings, recovery on/off), `history`. |
| `schedulerStopping` | info | When shutdown begins. `reason` (the signal). |
| `loadStarted` | info | A child started. `workers`, `bytes_total`, `need_mib`, `need_basis`. |
| `loadFinished` | info / warn / error | Ready, cancelled or failed. `outcome`, `reason`, `load_s`, `bytes`, `bytes_measured`, `avg_mb_s`, `nodes_detail` (per node: `node`, `bytes` for a remote worker, `memory_mib` from the load log). |
| `requestRefused` | warn | Requests answered 503 before any start: does not fit, forced node missing or offline, forced nodes do not fit, node excluded, queue timeout, backoff, fleet stale, nodes quarantined, no address. `reason`, `status`, `message`, `waiters`, `retry_after_s`; `need_mib`, `required_mib`, `capacity_mib` for does-not-fit. The same profile and reason is recorded at most every 30 s; `suppressed_since_last` counts the others. A bad model name, a failed load (it has `loadFinished`), a shutdown and a client that left are not recorded. |
| `loadStalled` | error | The load stall rule (7.6). `stalled_s`, `limit_s`, `bytes_sent`, `bytes_total`, `per_worker_bytes`. |
| `loadCancelledLowMemory` | error | The memory watch (7.2a). `node`, `available_mib`, `floor_mib`, `elapsed_s`. |
| `nodeLost` | error | Node loss (8.4). `missing_for_s`, `in_flight`, `waiters`, `nodes_in_instance`. |
| `eviction` | info | A claim stops or drains a victim (8.3), one event per victim. `victim`, `for_profile`, `waited_s`, `waiters`, `victim_busy`. |
| `instanceUnloaded` | info / warn | A loaded instance ended. `reason` (`ttl`, `api`, `eviction`, `shutdown`, `node loss`, `child exited`), `loaded_for_s`, `requests_served`, `exit`. |
| `childExited` | error | The child exited while ready or draining. `exit`, `state`, `in_flight`, `requests_served`. |
| `workerHealth` | info / warn / error | A watched worker changed state (8.4a). `state`, `previous`, `channels`, `silent`, `instances`, `addr`, `node_lost`; `hung_for_s` on recovery. |
| `nodeQuarantined`, `quarantineEnded` | warn, info | 8.4. `for_s`, `level`, `cause`. |
| `profileBackedOff` | warn | 8.4. `for_s`, `level`. |
| `recoveryRequest` | info | A recovery POST was sent (8.4b). `action`, `nonce`, `step`, `of`, `reason`. |
| `recoveryResponse` | info / warn | The fleet manager's answer. `action`, `nonce`, `outcome` (`stored`, `refused`, `error`), `http_status`, `body` or `err`. |
| `recoveryOutcome` | info / warn | The node's reported outcome. `action`, `outcome`, `nonce`, `reason`, `duration_ms`, `ours`. |
| `recoverySequenceStopped` | warn | A refusal stopped the escalation (8.4b), or every action was sent and the worker is still hung. `reason`, `action`, `http_status`. |
| `fleetViewStale`, `fleetViewRecovered` | warn, info | The fleet view crossed `stale_after`. `age_s`, `stale_after_s`; `stale_for_s`. |
| `fleetFeedDisconnected`, `fleetFeedRecovered` | warn, info | The fleet SSE stream. `down_for_s`. |
| `preflightFailed` | warn | A pre-flight failed (7.5). `kind`, `addr`, `duration_s`, `err`. |
| `learnedNeed` | info | A load measured its memory need (7.2a). `measured_mib`, `highest_mib`, `per_node`. |
| `needsMibTooLow` | warn | A measurement exceeded the configured `needs_mib`. `configured_mib`, `measured_mib`. |
| `nodeMemory` | info | A compact sample for one shared-memory node: every 60 s while it backs an instance, and at load start and load end. `reason` (`periodic`, `load_start`, `load_end`), `available_mib`, `total_mib`, `used_mib`, `budget_mib`. |

*Endpoints* (read-only; a bad parameter is `400` in the usual error shape):

- `GET /api/history`: `since`, `until` (unix ms), `type` (comma list),
  `severity` (minimum), `node`, `profile`, `session_id`, `q` (substring of the
  summary, case-insensitive), `limit` (default 200, at most 2000), `order`
  (`desc` default, or `asc`). Answers `{"events": [...], "total_matching": N,
  "oldest_ts": ..., "history": "on"|"off", "truncated": bool}`. The newest
  events come from memory. A query that reaches back past the memory window
  reads the files, newest file first (oldest first for `asc`) and stops as soon
  as the limit is reached, so it never loads everything. `total_matching`
  counts matches in the memory window plus the file parts actually read;
  `truncated` is true when more may exist.
- `GET /api/history/summary?since=&until=`: `events`, `by_type`, `by_severity`,
  `loads` (`ready`, `failed`, `cancelled`), `profiles` (per profile: `loads`,
  `ready`, `failed`, `cancelled`, and over the ready loads `min_s`,
  `median_s`, `max_s`, `avg_mb_s`), `nodes` (per node: `hung`, `lost`,
  `low_memory`) and `incidents` (every error-severity event, newest first, at
  most 500, `incidents_truncated`). It streams the range, so a long range is
  slower.
- `GET /api/history/export?since=&until=`: the matching events as JSON Lines
  (`application/x-ndjson`), oldest first, streamed.
- `GET /api/about`: `version`, `started_at`, `now`, `uptime_s`, `history`,
  `oldest_ts`; the dashboard header.

*Dashboard.* `GET /ui/` is one HTML file embedded in the binary (`embed`): no
external script, font or stylesheet, no build step, plain JavaScript, and it
works from another machine on the LAN (`http://<host>:3122/ui/`). It only
reads: it offers no load, unload or any other action. Sections, top to bottom:
header (version, uptime, history on or off, oldest event, live feed state); Now
(a tile per node from `/api/status` every 2 s, redrawn at once on live
`/api/events`: hostname, tier, online, excluded, health, usable memory, what is
loaded or loading with a progress bar and this node's bytes, plus the queue);
a time range bar (1 h, 6 h, 24 h, 7 d, custom) for the rest, with a download
link to the export; Incidents (warn and error events, newest first, each
expandable to the full record, a copy-as-JSON button and the other events of the
same `session_id`); Loads (finished loads, sortable, profile and outcome
filter); Timeline (all events, filtered by type, severity, node, profile and
text); Summary (counts and per-profile load statistics).

### 9.2 The live channel (`GET /ws`)

One WebSocket for everything that is live; REST is for what a client pulls or
pushes. It is in the same process and port as the rest (`http://<host>:3122` ->
`ws://<host>:3122/ws`). The SSE stream `/api/events` (A.3) is unchanged and
stays for the hub's llama panel. Library: `github.com/coder/websocket` (pure
Go). Package `internal/api` (`ws.go`).

*Frames.* JSON text frames, `{"type": "...", "ts": <unix ms>, "data": {...}}`.

| `type` | Direction | When, and `data` |
|---|---|---|
| `hello` | server | First message. `version`, `started_at`, `server_time` (unix ms), `history` (`on`/`off`), `bench` (`on`/`off`), `topics`, `ws_max_clients`. |
| `status` | server | The object `GET /api/status` returns (with the model-centric additions of 9.4). A full snapshot on connect, then whenever it changed, looked at every 250 ms so at most 4 a second; while a load is in progress at least one a second (so progress moves); at least one every 5 s. A change in only the clock, the fleet view age or a queue entry's age is not a change. |
| `event` | server | Every history event as recorded: the record of `GET /api/history`, including `seq` and `data.bench_run`. Only when `history_dir` is set (history off records nothing). |
| `bench` | server | Benchmark feature events (9.3). `data.kind` is `state` (on connect: the `GET /api/bench/state` document), `run`, `progress` (the run's progress model), `step_started`, `step_finished`, `request`, `load` or `aggregate_updated`; see 9.3. |
| `ping` | server | Every 15 s, `data: {}`. |
| `subscribe` | client | `{"type":"subscribe","topics":["status","event","bench"]}`. Default is all three. Replaces the set; `hello` and `ping` always arrive. Newly subscribing to `status` or `bench` sends a fresh snapshot at once. |
| `pong` | client | `{"type":"pong"}`; accepted and ignored (any traffic keeps the connection alive). Unknown messages are ignored. |

*Never in the way of serving.* The status look is an ordinary `Status` query
on the scheduler loop (the same as `GET /api/status`), taken at most 4 times a
second and only while a client is connected. Each client has a bounded send
queue (64 frames); a frame that does not fit means the client is too slow, and
it is closed with code 1008 (policy violation) at once, never waited for; a
write that takes over 10 s does the same. A producer (history, benchmarks) only
does non-blocking sends. At most `ws_max_clients` (default 32) clients; the next
gets `503` with `Retry-After`. On shutdown every client is closed with 1001.

*Origin check.* The tool has no authentication (LAN only). A browser sends an
`Origin` header on a WebSocket request, so the handshake is refused (`403`)
unless there is no `Origin` (not a browser), it is the same origin as the
request, or its host is a loopback or private IP (`127.0.0.0/8`, `::1`,
`10/8`, `172.16/12`, `192.168/16`, `fc00::/7`, link-local), `localhost`, a name
ending in `.local`, `.lan`, `.home.arpa` or `.internal`, or a single-label name
such as `host-1`. This stops a page on the internet from using the browser of
someone on the LAN to reach the scheduler; it is not authentication.

### 9.3 Benchmarks

Defining benchmarks, grouping them into suites, running them through the
scheduler itself and keeping every result. Packages: `internal/benchspec`
(shapes, phases, validation, the editor schema), `internal/bench` (store,
runner, progress model, statistics, aggregates, export), `internal/api/bench.go`
(REST). All of it is off when the `bench_enabled` setting is false or the
database cannot be opened: the endpoints answer `503`;
everything else is unaffected.

**Concepts.**

*Benchmark* (a test case): `id` (slug), `name`, `description` (free text, shown
while the benchmark runs), `profile` (a profile id or alias; unknown ones are
rejected when it is saved), `timeout_s` (default 600, the whole benchmark
including the load and the holds), `expect`, `tags`, and either **`phases`** or
the **simple form**.

*Phases.* `phases` is an ordered list of `{type, label?, ...options}`. The
options belong to the type; an option that does not belong is rejected with a
message that names what the type takes. `GET /api/bench/schema` lists all of
it for an editor (below).

| `type` | What it does | Options (default) | What it records |
|---|---|---|---|
| `load` | Cold load only. If the profile has an instance it is unloaded first (not forced; a busy profile fails the step), then `POST /api/load` starts the load, **no chat request is sent**, and the runner waits until the instance is ready or the load fails. | none | load seconds, bytes, average MB/s, per node bytes and measured memory (`load`), the scheduler's plan (on the step) |
| `warmup` | `count` requests, recorded but left out of the statistics. | `count` (1, 0 to 10), `request` (the first request of the `requests` phase) | the requests |
| `requests` | The measured requests: each is sent `repeats` times. | `requests` (1 to 64 of `{name, messages \| prompt, max_tokens (128), temperature (0), seed (42), cache_prompt (false), stream (false)}`), `repeats` (1), and `max_tokens`, `temperature`, `seed`, `cache_prompt`, `stream` as defaults for requests that leave them out | decode and prefill tokens per second, time to first token (streaming), wall time, output hash; statistics |
| `conversation` | A multi-turn exchange. Each turn is sent with the optional `system` message, every earlier user turn and the model's reply to it. | `system`, `turns` (1 to 64 of `{name?, content, max_tokens?}`), `max_tokens`, `temperature`, `seed`, `stream`, `cache_prompt` (**true**) | per turn: prompt tokens (`prompt_n` plus `cache_n`), reused prompt tokens (`cache_n`), prefill and decode speed, time to first token, wall time |
| `hold` | Keeps the model loaded and idle for `seconds`, then sends one request, to see what happens after idle time. | `seconds` (60, 1 to 3600), `request` (as for warmup) | idle seconds, whether the model was still loaded afterwards, the request |
| `concurrent_requests` | `n` requests to the same profile at the same moment. A failed request is counted, not fatal. | `n` (2, 2 to 16), `request` (as for warmup) | per request: status, wait (time before the model started on it), decode speed, wall time; total wall time; errors |
| `unload` | `POST /api/unload` (forced only when the step already failed or was cancelled) and the wait until the instance is gone. | none | seconds until the instance is gone |

*The simple form* (no `phases`) stays valid and is expanded internally:
`cold` (default true) gives a `load` phase, `warmup_requests` (default 1) a
`warmup` phase, `requests` and `repeats` a `requests` phase, and `unload_after`
(default true) an `unload` phase that only unloads what the run itself loaded.
So `cold: false` with `unload_after: false` is a single `requests` phase. "Load
only" is `phases: [{"type":"load"},{"type":"unload"}]`. `phases` replaces
`cold`, `requests`, `repeats`, `warmup_requests` and `unload_after`: mixing the
two forms is a validation error. When a phase that sends requests (`warmup`,
`requests`, `conversation`, `hold`, `concurrent_requests`) finds the profile
not loaded, it loads it first, the way the `load` phase does but without making
it cold, and records that load on the step. The sum of the `hold` seconds plus 30
must fit in `timeout_s`.

*`expect`*: `outcome` (`ready` default, `refused`, `failed`), `max_load_s`,
`min_decode_tps`, `min_prefill_tps`, `max_ttft_ms` (needs a phase that streams),
`output_sha256` (pins the output of the first request of the first `requests`
phase), `min_reused_tokens` (every conversation turn after the first reuses at
least this many prompt tokens), `max_unload_s`, `max_concurrent_wall_s` and
`max_errors` (for `concurrent_requests`). A key whose phase the benchmark does
not have is rejected when it is saved. Speed and time to first token are the
means over the benchmark's `requests` phases, or, for a benchmark without one,
over its conversation turns, hold request or simultaneous requests (in that
order).

*Concurrent step*: `{"id": ..., "parallel": [<benchmark ids>]}` (2 to 8 plain
benchmarks with different profiles): their loads are fired at the same moment
(each member gets as far as the point where it would start its load, then all
go).

*`GET /api/bench/schema`*: `phase_types` (each with `type`, `title`,
`description`, `records` and `options`: `name`, `type` (`string`, `text`,
`integer`, `number`, `boolean`, `enum`, `list`, `request`), `default`, `min`,
`max`, `unit`, `description`), `request_options`, `turn_options`,
`benchmark_options`, `expect_keys` (`name`, `type`, `unit`, `needs`,
`description`), `limits` and five `templates` ("Load only", "Load and
requests", "Load and conversation", "Requests on a loaded model",
"Simultaneous requests"; each `benchmark` is a valid body whose profile is the
placeholder `your-profile`), so an editor can be drawn from it.

*Suite*: `id`, `name`, `benchmarks` (ordered list of benchmark ids, or inline
benchmarks, whose id defaults to `<suite>-<position>`), `stop_on_failure`
(default false), `cooldown_s` (default 5, between benchmarks), `only_when_idle`
(default true), `idle_wait_s` (default 600), `schedule` (`{"every":"24h"}` at
least 10 minutes, or `{"at":"03:00"}` local time; default none), `baseline` (a
run id; default the previous completed run of the suite).

*Planned or blocked suite*: a scenario that cannot be run yet is a suite with
`status` set to `planned` or `blocked` and a `reason` (required then, at most
500 characters). It may list no benchmark (0 to 100); a normal suite still
needs 1 to 100, and has neither field. It cannot have a schedule. It is listed
with a badge and its reason, and running it is refused with `409` and the
reason (`POST /api/bench/suites/{id}/run`, the MCP `run_suite` tool). The REST
routes, the MCP `create_suite` tool and the configuration import and export
accept it as they accept any suite. To run it, remove the status.

*Run*: one execution of a suite; states `queued`, `running`, `paused` (waiting
for idle), `completed`, `failed`, `cancelled`, `skipped`. One run at a time,
others queue (at most 20). A run stores the suite and every benchmark it named
as they were when it was queued, and the environment it ran in (below).

**Execution.** The runner is a goroutine of the same process that drives the
scheduler only through its public HTTP API on its own loopback address
(`/api/plan`, `/api/status`, `/api/load`, `/api/unload`,
`/v1/chat/completions`, `/api/history`), so a benchmark takes exactly the path a
client takes and the scheduler's fit checks cannot be bypassed. Every chat
request carries `X-Bench-Run: <run id>`; the scheduler copies it into the
history records of the load such a request causes and of a refusal
(`data.bench_run`). A `load` phase sends no chat request, so its
`POST /api/load` (and the unloads) carry the same `X-Bench-Run` header: the
scheduler tags the load's history records with `data.bench_run` too, and the
audit log records the call as source `system`, actor `bench run <id>` (6.0.4),
never as a person's. The load session id (`load.session_id`; `GET
/api/history?session_id=...`) stays the other join key. Per benchmark:

1. `POST /api/plan` is recorded (`plan`, and the nodes it would use).
2. The phases run in order, each stored as a row of `run_phases` when it starts
   and again when it ends, so the run detail fills in while the run goes.
3. A `load` phase unloads, fires `POST /api/load` and then reads `/api/status`
   every second (`LoadPoll`) until the instance is `ready`. While it waits it
   feeds the instance's `load` object to the progress model and to the live
   channel and remembers the child's `log_file`. A refusal answered by
   `/api/load` itself (`503` or 4xx) is outcome `refused`; an instance that
   disappears, or a `recent_loads` entry that did not end `ready`, is outcome
   `failed` with the scheduler's reason. Afterwards the load is read from the
   scheduler: the `recent_loads` entry (session id, outcome, reason), the
   history `loadFinished` record of that session (bytes, MB/s, per node bytes
   and memory) and the instance's `device_total_mib`. Without history only
   `recent_loads`, the last observed progress and the measured wall time are
   available and `load.source` says so.
4. A request that does not return 200 ends the step (outcome `failed`), except
   in `concurrent_requests`, where it is counted. A failed or refused step marks
   its remaining phases `skipped`, except `unload`, which still cleans up (with
   force, for a failed or cancelled step).
5. `refused` is a refusal before a load started; `failed` is a load or request
   that failed after; `error` means the runner itself could not do its job;
   `cancelled`; `skipped`. At the end of the run (however it ended) everything
   the run loaded and did not unload is unloaded.

The step also records `model_file` and `node_count` (the worker nodes the
instance was placed on), which the aggregates group by, and since this was
added (a step stored before has none of these; the UI says "Not recorded"):

- `mem_low`: per node that took part (the scheduler host, the plan's nodes and
  the instance's nodes), the lowest available memory during the load
  (`load_mib`) and during the whole step (`step_mib`), with the number of
  `samples`. The runner samples `/api/status` about twice a second
  (`ManagerOptions.MemSample`, 500 ms) and also uses every status it reads for
  the load and at the end of each phase. The figure is the agent's
  `mem_available_mib`, or the node's free figure when it has no agent. A
  benchmark result carries the lowest of them as `lowest_available_mib`.
- `load.load_mode` (and each `load` phase's): `file_read` (the workers read
  the model file from the shared folder), `pushed` (the host sent the weights)
  or `local` (no remote worker). The scheduler reports it on the instance and
  in the plan. The aggregates group by it in `by_model_load_mode` (model file,
  workers, load mode; `not_recorded` for older steps).
- `roles`: the `host` (the scheduler machine, which runs llama-server) and the
  `worker`s (the remote nodes of the instance, or of the plan when it never
  started), each with its share of the model's memory when known. A result
  carries `host` and `workers` (names) for the compact "Host: x · Workers: n
  (names)".
- `reload_from_cache`: the plan's reload figure per node (below).

*Reload from cache.* Whether a reload of a model can come from a node's file
cache instead of the disk or the network. One pure function
(`scheduler.ReloadFromCache`) decides it per node of a placement:

- `unknown` when the node does not read the model file (`pushed` weights), or
  a figure it needs is not known (system memory, file share, model share);
- otherwise `room` = the node's system memory, minus the margin (the watchdog
  floor, plus the tier headroom on a node whose GPU shares system memory),
  minus the node's share of the loaded model when the GPU shares system memory;
- `full` when `room` is at least the node's share of the file, `none` when
  `room` is 0 or less, else `partial` with `cached_pct` = room / file share.

A node's file share and model share are the file size and the model's memory
times the node's split fraction. The scheduler host always reads the file; with
`pushed` weights it reads all of it to send it, so its share is the whole file.
A dedicated GPU (the scheduler host's own card) holds the model outside system
memory, so its file share only has to fit in system memory for `full`. The two
measured cases on a 14 GB console: 7.0 GB file and 7.5 GB model gives `partial`,
about 63%, and reloads slowly; 2.7 GB and 2.9 GB gives `full` and reloads fast.
The verdict is in the plan (`POST /api/plan`, `GET /api/profiles` plan
`load_mode` and `reload_from_cache`) and stored with the benchmark step. The UI
shows it as "Reload from cache: partial, about 63% of the file".

*Safety.* `only_when_idle` (default true): the run does not start, and pauses
between benchmarks, while the scheduler has an instance this run did not load,
a request in flight or waiting that is not this run's, or a queue entry. It
waits up to `idle_wait_s`, then the run ends `skipped` (benchmarks already done
keep their results; the rest are recorded as skipped). Cancel
(`POST /api/bench/runs/{id}/cancel`) stops after the current request (a load in
progress or a hold is abandoned at once and the load force-unloaded, since
nobody wants it), marks the run `cancelled` and unloads what the run loaded. On
shutdown the running run is cancelled the same way with the reason "scheduler
shutting down", before the HTTP server stops. At start, a run found `running`
or `paused` is marked `failed` with `failed (interrupted)` and a `queued` one
`cancelled`. A run that exceeds `bench.max_run_minutes` is stopped and marked
`failed`. Scheduled runs are queued by a check every 30 s and not queued twice
for one suite.

*Statistics and comparison.* Per step: mean, median, min, max and sample
standard deviation of decode tokens/s, prefill tokens/s, time to first token
and wall time over the measured requests that returned 200; warmup requests are
never included. Each phase has its own statistics too. A step passes when its
`expect` holds (default: the outcome is `ready`). Against the baseline run:
percent change of load time (positive is slower), decode, prefill and time to
first token; `regression` is true when the load is more than
`bench.regress_load_pct` (15) slower, decode more than `bench.regress_decode_pct`
(7) slower, the output hash of a deterministic (temperature 0) request differs,
or a benchmark that loaded in the baseline no longer does. A request whose
repeats produced different outputs is marked `varies` and is not compared.

**Progress model.** While a run is active the manager keeps a progress object,
returned as `run` by `GET /api/bench/state` (`null` when no run is active) and
published on the `bench` topic as `{"kind": "progress", "run": {...}}`: once
on every phase or step change and otherwise at most twice a second.

```
run: { id, suite_id, suite_name, state, percent (0-100, whole run), started_at (unix ms),
       elapsed_s, eta_s (null when unknown), steps_total, steps_done,
       current: { index, benchmark_id, name, description, profile, model_file, nodes,
                  phase: { index, type, label, phases_total },
                  percent (0-100, this benchmark), detail },
       next: { benchmark_id, name, description, profile } | null,
       queue: [ids of the benchmarks still to run, in order] }
```

Indexes are 0-based. `current` is `null` before the first benchmark starts.
For a concurrent step `current` is the step: its `profile` joins the members'
with ` + `, `percent` is the mean of the members', `phase` is the least
advanced member's and `detail.members` lists them.

*Whole-run percent* is weighted by each suite entry's expected duration: the
median duration of that benchmark in the (newest 20) earlier completed runs.
An entry with no history gets the median of the known ones; when nothing is
known all entries weigh the same. It includes the part of the current benchmark
that is done and is capped at 99.9 until the run is `completed`, and it never
decreases.

*The current benchmark's percent* is computed from its phases, each weighted
by its expected duration (the median of that phase, by position and type, in
earlier runs; else a default: load 60 s, 5 s per warm-up or measured request,
8 s per conversation turn, the hold seconds plus 5, 15 s for simultaneous
requests, 3 s for an unload). Within a phase: `load` follows the scheduler's
real `load.progress` (0 to 0.99, from the instance's `load` object; without it
an estimate that never reaches the end), so a load-only benchmark's percent is
essentially the model's load percent; request phases follow requests completed
out of planned plus, when streaming, the tokens generated out of `max_tokens`
for the request in flight; `hold` follows elapsed time; `concurrent_requests`
the requests finished. It is 100 only when the benchmark has ended.

`detail` has a sentence in `text` and, depending on the phase, `load` (the
scheduler's own `load` object, passed through: bytes total and sent, ETA and
per worker bytes and state, so a UI can show each worker), `tokens` and
`max_tokens`, `turn` and `turns`, `held_s` and `hold_s`. `eta_s` is the sum of
the expected remaining durations of the current benchmark's phases and of the
entries after it; it is `null` when no benchmark involved has a history.

**Aggregates (run history that aggregates).** `GET /api/bench/aggregates` adds
the stored runs up. Filters: `suite`, `benchmark`, `profile`, `since` and
`until` (unix ms or seconds, RFC 3339, or a span back from now: `24h`, `7d`),
`limit_runs` (the newest this many runs; default 200, at most 1000) and
`env=same` (only runs whose environment equals the newest matching run's).
Only runs that ended (`completed`, `failed`, `cancelled`) count.

```
{ generated_at, filter, runs_considered, environment (the newest run's),
  benchmarks: [ { benchmark_id, name, profile, runs, steps, pass_rate,
                  outcomes: {ready, refused, failed, (error, cancelled)},
                  metrics: { load_s, load_mb_s, decode_tps, prefill_tps, ttft_ms,
                             memory_mib, unload_s, concurrent_wall_s:
                     { count, min, median, mean, max, stdev, last, best,
                       change_pct, threshold_pct, regression, points: [{run_id, at, value} x 30] } },
                  turns: [ { phase, turn, metrics: { decode_tps, prefill_tps, ttft_ms,
                             wall_ms, prompt_tokens, reused_tokens } } ],
                  last_run_id, last_at } ],
  suites: [ { suite_id, name, runs, states, pass_rate, mean_duration_s, last_run } ],
  by_model: [ { model_file, runs, steps, benchmarks, profiles, metrics } ],
  by_model_nodes: [ { model_file, nodes, runs, steps, benchmarks, profiles, metrics } ] }
```

Only a step that loaded and served (`ready`) contributes numbers. `best` is
the lowest value for times, memory and wall time and the highest for speeds and
reused tokens. `change_pct` is the last value against the median of the earlier
ones (`null` with fewer than two values) and `regression` is set when the last
value is worse than that median by more than the threshold of the metric
(`bench.regress_load_pct` for times and memory, `bench.regress_decode_pct` for
speeds; prompt tokens are never flagged). `pass_rate` of a benchmark is the
share of its steps whose `expect` held; of a suite, the share of its runs that
completed with no failed step. `by_model` and `by_model_nodes` group the ready
steps by the step's `model_file`, and by `model_file` and the number of worker
nodes (`node_count`: placed nodes that are not the scheduler's own host), so
the same model's behaviour on 1, 2 and 3 workers sits side by side. When a run
ends the manager publishes `{"kind": "aggregate_updated", "run_id", "suite_id",
"state", "benchmarks": [...]}` on the `bench` topic.

*Environment.* A run records, in `runs.env`, what it ran in: `scheduler_version`,
`config_revision` (when `/api/status` exposes `config_revision`, or
`config.revision`; absent otherwise), `llama_binary` (the program of the first
child's `cmd` seen during the run) and `workers` (`node`, `rpc_server_version`
from `view.nodes[]`, remote nodes only). It is read at the start and merged as
instances appear. `env=same` compares these.

**Storage.** `data_dir/scheduler.db`, SQLite via `modernc.org/sqlite` (pure Go,
no cgo; the Docker build is the plain `go build`), WAL mode, one writer
goroutine (one transaction per write), reads through a pool. Schema versions
are rows of `migrations (version, name, applied_at)`; a database written by a
newer build is refused. JSON columns hold the open shapes. Migration 2
("benchmark phases, run environment, aggregates") upgrades a database made by
an earlier build in place: it adds `runs.env`, `run_steps.model_file`,
`run_steps.node_count`, the `run_phases` table, and rebuilds `requests` with
the wider `phase` check and three new columns, keeping every row.

| Table | Columns |
|---|---|
| `migrations` | `version` PK, `name`, `applied_at` |
| `benchmarks` | `id` PK, `name`, `source` (`config`/`api`), `spec` JSON, `created_at`, `updated_at` |
| `suites` | same as benchmarks |
| `runs` | `id` PK, `suite_id`, `suite_name`, `state`, `note`, `triggered_by` (`manual`/`schedule`), `baseline_run_id`, `queued_at`, `started_at`, `finished_at`, `error`, `version`, `suite` JSON (suite + benchmarks snapshot), `summary` JSON, `env` JSON |
| `run_steps` | `id` PK, `run_id` FK, `idx`, `grp` (parallel step id), `benchmark_id`, `name`, `profile`, `state`, `outcome`, `reason`, `http_status`, `log_file`, `started_at`, `finished_at`, `error`, `model_file`, `node_count`, JSON: `spec`, `plan`, `load`, `stats`, `expect`, `comparison` |
| `run_phases` | `id` PK, `step_id` FK, `run_id`, `idx` (position in the benchmark's phases), `type`, `label`, `state` (`running`/`done`/`failed`/`cancelled`/`skipped`), `outcome` (`ok`, `refused`, ...), `error`, `started_at`, `finished_at`, `result` JSON (`duration_s`, `load`, `stats`, `turns`, `concurrent`, `held_s`, `loaded_after_hold`, `unload`) |
| `requests` | `id` PK, `step_id` FK, `run_id`, `seq`, `name`, `phase` (`warmup`/`measured`/`conversation`/`hold`/`concurrent`; `load` in rows written by an earlier build), `phase_idx`, `turn`, `wait_ms`, `status`, `wall_ms`, `ttft_ms`, `prompt_n`, `prompt_ms`, `prompt_tps`, `predicted_n`, `predicted_ms`, `decode_tps`, `cache_n`, `timings` JSON, `output_sha256`, `output_head`, `deterministic`, `error`, `started_at` |

*Seeding.* The `bench.benchmarks` and `bench.suites` of a YAML file given to
`import-config` (same shapes as the API) are stored once, with `source: "config"`
(and an audit entry, section `bench`, action `import`). Nothing is seeded at
start any more: the database is the source of truth. Validation checks every
seed against the profiles at import time.

*History.* The JSONL history is unchanged and not migrated. A run writes
`benchRunStarted`, `benchStepFinished` and `benchRunFinished` (all with
`data.bench_run`); the load a chat request causes and its refusals carry
`data.bench_run` too.

*Audit.* Creating, updating and deleting a benchmark or a suite, starting a run
and cancelling a run each call an `AuditHook` (`internal/api/bench.go`) with
action (`create`, `update`, `delete`, `run_start`, `run_cancel`), section
`bench`, target (`benchmark:<id>`, `suite:<id>`, `run:<id>`) and the state
before and after. `api.New` plugs the audit log's recorder in whenever
`Options.Audit` is set (`internal/api/benchaudit.go`; `Server.SetAuditHook`
replaces it), so the entries carry the source and actor of the request: `ui`
for `X-Source: ui`, `mcp` through the MCP server, else `rest`. The entry holds
`before`, `after` and a `diff`; a failure to record is logged and never fails
the change.

**REST** (pull and push only; a bad body is `400`, a failed validation `422`,
an unknown id `404`, a conflict `409`, off `503`):

| Endpoint | Behaviour |
|---|---|
| `GET /api/bench/benchmarks`, `POST` | List `{"benchmarks": [...]}`; create (`201`, `409` if the id exists). |
| `GET /api/bench/benchmarks/{id}`, `PUT`, `DELETE` | One benchmark (the spec fields plus `source`, `created_at`, `updated_at`); replace; delete (`409` while a suite or parallel step names it). |
| `GET /api/bench/suites`, `POST`, `GET/PUT/DELETE /api/bench/suites/{id}` | The same for suites; the list adds `last_run`. |
| `POST /api/bench/suites/{id}/run` | Body optional `{"baseline": "<run id>", "note": "..."}`. `202` with the run (`queued`); `429` when 20 runs are queued. |
| `GET /api/bench/runs?suite=&state=&limit=` | `{"runs": [...]}` newest first, without steps. |
| `GET /api/bench/runs/{id}` | The run with `env`, `steps`, their `phases` (results per phase), `requests`, `stats`, `expect`, `comparison`, `load`, `plan`, `model_file`, `node_count`. |
| `POST /api/bench/runs/{id}/cancel` | `202`; `409` for a finished run. |
| `GET /api/bench/runs/{id}/export?format=json\|csv\|md` | Download. `md` is a table, one row per benchmark (profile, nodes, load s, MB/s, prefill and decode tokens/s, decode standard deviation, memory MiB, outcome, change against the baseline), followed by a table of phases for each step and of the turns of each conversation. `csv` is one row per step (with `model_file`, `worker_nodes`, `phases`); `format=csv&detail=phases` one row per phase; `detail=turns` one row per conversation turn. |
| `GET /api/bench/trends?benchmark=&metric=&limit=` | `metric` is `load_s`, `load_mb_s`, `memory_mib`, `decode_tps`, `prefill_tps`, `ttft_ms`, `unload_s` or `concurrent_wall_s`. `{"benchmark","metric","points":[{run_id,started_at,value}]}` oldest first (the newest `limit`), ready steps of completed or failed runs. |
| `GET /api/bench/aggregates?suite=&benchmark=&profile=&since=&until=&limit_runs=&env=` | The aggregates above. |
| `GET /api/bench/schema` | The editor schema above. |
| `GET /api/bench/state` | `{"enabled","run","current","queue","next_scheduled"}`: `run` is the progress object, `current` the run in the older brief form. |
| `GET /api/bench/profiles` | Valid profile ids (and aliases) for an editor. |

**`bench` WebSocket messages** (`data.kind`): `state` (on connect; the
`/api/bench/state` document), `run` (`data.run`: id, suite, state, trigger,
times, note, baseline, error, summary, `progress` with `steps_total`,
`steps_done`, the step in flight and `paused_because`), `progress` (`run`: the
progress object above), `step_started` (`run_id`, `step`), `step_finished`
(`run_id`, `step`: outcome, reason, load, stats, expect, comparison, no
requests), `request` (`run_id`, `idx`, `benchmark_id`, `phase`, `request`,
`seq`, `state` `started`/`streaming`, `tokens` and `elapsed_ms` while
streaming; this was called `progress` in the first version of the feature),
`load` (`run_id`, `idx`, `benchmark_id`, `load`: the status `load` object of the
step in flight), `aggregate_updated` (`run_id`, `suite_id`, `state`,
`benchmarks`).

### 9.4 Model-centric status, `/api/profiles`, `/api/system`

`GET /api/status` (and the `status` WebSocket message) describes each instance
with: `profile`, `model_file`, `state`, `nodes` (id, address, budget, and the
per-device `memory` from the load log), `device_total_mib`, `in_flight`,
`requests_served` (admitted since it started), `ready_since`, `last_used`,
`load_duration`, and the load progress fields while starting. When the
upstream reported a token rate for the newest finished request of that model,
the instance also has `last_request`: `tokens_per_second`, `output_tokens`,
`input_tokens`, `status`, `duration_ms`, `finished_at`. That figure comes from
the activity log (A.4) and is the rate of the last *finished* request: there is
no live tokens-per-second figure for a generation in progress (the proxy does
not parse the stream), so a client can only show the live rate of a benchmark's
streamed request (`bench` `progress` messages). All additions are new keys;
existing keys are unchanged.

`GET /api/profiles` lists every profile: `id`, `kind`, `model`, `ctx`,
`parallel`, `aliases`, `force_nodes`, `tensor_split`, `needs_mib_configured`
(when set), `needs_mib` with `needs_basis` (`measured, needs_mib`, `estimated,
+5% margin; ...` or `measured this run`) and `needs_measured`, and `plan`
(the dry-run of `POST /api/plan`: `action`, `would_start`, `nodes` as hostnames,
`reason`, `instance`, `warning`), and `generated` (below). Read-only; it runs
one plan per profile.

`GET /api/models` is the model catalog: one entry per model FILE, not per
profile (a model usually has several profiles, and a benchmark matrix adds
dozens). It joins the profiles and their plans, the status, the event history
and the benchmark aggregates with the file's own metadata. Each entry: `key`
(the file name, or `command:<profile id>` for a profile of kind command),
`kind`, `file`, `exists`, `size_bytes`, `name`, `publisher`, `family` (the GGUF
architecture), `parameters` ("12B"), `quant`, `context_length`, `meta_source`
(`gguf` when the header was read, else `file_name`), `state` (`ready`,
`loading`, `not_loaded`), `profiles` (each: `id`, `ctx`, `parallel`,
`placement` `automatic` or `forced`, `force_nodes`, `tensor_split`,
`needs_mib`, `needs_basis`, `needs_measured`, `aliases`, `generated`, `plan`),
`profile_count`, `generated_count`, `startable_count`, `instances` (each
loaded or loading instance of any of its profiles: `profile`, `state`, `nodes`
as hostnames, `memory_mib`, `since`, `in_flight`, `waiters`,
`requests_served`, `last_used`, `last_request_tps`, `load_progress`,
`load_eta_s`, `stale_profile`), `measured` (memory per node count, from the
loads in the history and this run's learned needs; the lowest figure per node
count), `fit` (`need_mib`, `basis` `measured`, `configured` or `estimated`,
`min_nodes`: on how few nodes of the EMPTY cluster the need fits by their
plan budgets, 0 when it is more than the whole cluster; it is a guide, the
placer's own rules decide a real placement), `loads` (`count`, `ready`,
`failed`, `cancelled`, `unloads`, `refusals`, `last`, `by_nodes` with
`median_s`, `avg_mb_s`), `last_loaded`, `last_active`, `requests_served`,
`last_refusal` (the newer of the status's and the history's) and `bench`
(`benchmarks`, `runs`, `by_nodes` with the medians of the benchmark
aggregates), or null. `GET /api/models/{key}` is one entry plus `events` (its
newest 200 history events) and `bench.detail`. File metadata is read on the
request's goroutine and cached by name, size and modification time; the
history part covers the newest 2000 events of the types it reads. Read-only.

A profile is **generated** when a tool made it (a benchmark matrix, a sweep)
rather than a person. The optional profile field `generated` (true or false)
says so explicitly; when it is not set, the id decides: `bench-*`, `b-*` and
`m-*` count as generated. It changes nothing about placement; clients use it to
keep generated configurations out of model lists.

`GET /api/system`: version, uptime, `listen`, the safety settings (the same
summary as the `schedulerStarted` event), the benchmark settings, live client
count and the storage locations with sizes (history directory, database file and
schema version, child log directory).

### 9.5 MCP server (`/mcp`)

AI agents operate the scheduler through the Model Context Protocol, in the same
binary and on the same port: `http://<host>:3122/mcp`. Package
`internal/mcpserver`. No authentication (LAN tool, like the rest).

*Transport.* Streamable HTTP in its stateless form: every `POST /mcp` carries
one JSON-RPC 2.0 message (a batch is accepted too) and is answered with one
`application/json` body; notifications get `202`. `GET /mcp` answers `405`
(there is no server-initiated stream). `initialize` negotiates the protocol
version (2025-11-25, 2025-06-18, 2025-03-26 or 2024-11-05; the client's if
supported, else the newest), returns `serverInfo` `llm-scheduler` with the
scheduler's version, capabilities `tools` and `resources`, and a short
`instructions` text (check `plan_load` before `load_model`; loads take minutes;
never unload a busy model unless asked; configuration changes are audited and
reversible). It also returns an `Mcp-Session-Id`; the server remembers only the
client name and version behind it (a bounded in-memory map), so the audit log
can name the client. A client that sends no session id still works.
`DELETE /mcp` forgets a session. Because a browser page can send a plain POST to
a LAN host, `/mcp` requires `Content-Type: application/json` and applies the
same origin rule as `/ws` (9.2). The protocol is hand-rolled (about 400 lines):
the official Go SDK was not used because it pulls an OAuth, JWT and
JSON-Schema dependency tree into a binary that otherwise has three direct
dependencies, and this server needs only `initialize`, `ping`, `tools/*` and
`resources/*`.

*One implementation, two transports.* A tool is a table entry: name, schema, and
a recipe (method, path, which arguments are path parts, query parameters or
body). A call builds an HTTP request and runs it **in-process** through the same
`http.Handler` that serves REST (a small recorder instead of a socket), then
returns the answer body as the tool result. So a tool cannot differ from its
REST endpoint, validation, status codes and error text included. A 2xx answer
is the result; any other status is a tool error (`isError: true`) carrying
`HTTP <status> from <method> <path>: <body>`. When the route does not exist in
the build (the mux's `404 no such path`) the error says "this server build does
not have that endpoint" (a safety net for an older scheduler; every route the
tools use exists in this build). The only tools that reshape an answer are the
status views (`get_status`, `list_nodes`, `get_queue`, below); `get_profile`
returns the real `{revision, id, profile}` answer. Arguments are validated against the tool's own schema before anything
is sent (unknown or missing arguments, wrong types, enum values) and fail as
tool errors.

*Tools* (46; read-only tools carry `readOnlyHint`; replacing and deleting tools,
`unload_model` and `rollback_config` carry `destructiveHint`):

| Tool | REST it calls |
|---|---|
| `get_status` | `GET /api/status`, summarised model-centric: per instance the profile, model file, state, nodes (hostname, budget, memory), `in_flight`, `requests_served`, load progress, ETA and `last_request`; then nodes, queue, quarantines, backoffs, the five newest `recent_loads` and the fleet view state. `verbose: true` returns the full object. |
| `list_nodes`, `get_queue` | The nodes (full) or the queue with quarantines and backoffs, from `GET /api/status`. |
| `list_models`, `list_profiles`, `get_profile` | `GET /v1/models`, `GET /api/profiles`, `GET /api/config/profiles/{id}` |
| `list_model_files`, `get_model_file` | `GET /api/models`, `GET /api/models/{key}` |
| `plan_load` | `POST /api/plan` |
| `query_history`, `get_history_summary` | `GET /api/history` (all its filters), `GET /api/history/summary` |
| `query_logs` | `GET /api/logs/query?query=&limit=&start=&end=` (CR-10; the tool sets `limit` 100 when not given; `404` while central logging is off) |
| `get_system_info` | `GET /api/system` |
| `get_config`, `get_config_schema` | `GET /api/config`, `GET /api/config/schema` (`{"fields": [...]}`, the settings) |
| `list_config_revisions`, `get_config_revision` | `GET /api/config/revisions?limit=` (`{revisions, current}`), `.../{revision}` |
| `query_audit` | `GET /api/audit` (since, until, source, actor, section, target, action, q, limit, order); answers `{entries, total_matching}`. `source` is one of ui, rest, mcp, import, system; `section` a configuration section, `bench` or `runtime`. |
| `list_benchmarks`, `get_benchmark`, `list_suites`, `get_suite` | `GET /api/bench/benchmarks`, `.../{id}`, `/suites`, `/suites/{id}` |
| `get_bench_state`, `list_runs`, `get_run`, `export_run` | `GET /api/bench/state`, `/runs`, `/runs/{id}`, `/runs/{id}/export?format=md\|csv\|json[&detail=phases\|turns]` (the tool also accepts `markdown` for `md`) |
| `get_bench_aggregates`, `get_bench_trends`, `get_bench_schema` | `GET /api/bench/aggregates` (suite, benchmark, profile, since and until as a time or a span such as `7d`, limit_runs, `env=same`), `/trends` (benchmark, metric: load_s, load_mb_s, memory_mib, decode_tps, prefill_tps, ttft_ms, unload_s, concurrent_wall_s; limit), `/schema` |
| `load_model`, `unload_model` | `POST /api/load` `{model}`, `POST /api/unload` `{model, force}` |
| `update_settings` | `PATCH /api/config/settings`; the body is the `settings` object itself (only the keys to change; an unknown key is `400`) |
| `create_profile`, `update_profile`, `delete_profile` | `POST /api/config/profiles` (body = the `profile` object with its `id`), `PUT /api/config/profiles/{id}` (body = the whole `profile`, not a patch), `DELETE /api/config/profiles/{id}` |
| `set_tiers`, `set_exclude_nodes`, `set_no_auto_nodes`, `set_node_overrides`, `set_recovery_settings`, `set_fleet_settings` | `PUT /api/config/tiers` (body = `value`: a list of tiers), `/exclude-nodes` and `/no-auto-nodes` (a list of node patterns), `/node-overrides` (an object of node id to override), `/recovery`, `/fleet` (an object). The body is `value` itself, a bare JSON array or object, which the handlers accept. |
| `rollback_config` | `POST /api/config/revisions/{revision}/rollback` (no body; the reason goes in `comment`, sent as `X-Comment`) |
| `create_benchmark`, `update_benchmark`, `delete_benchmark`, `create_suite`, `update_suite`, `delete_suite` | `POST /api/bench/benchmarks`, `PUT`, `DELETE .../{id}`, and the same for `/suites` |
| `run_suite`, `cancel_run` | `POST /api/bench/suites/{id}/run` `{baseline, note}`, `POST /api/bench/runs/{id}/cancel` |

Tools that change configuration (the `/api/config` ones) take `comment`
(header `X-Comment`), `dry_run` (`?dry_run=1`) and `if_revision` (header
`If-Match: <revision number>`; a stale one is `409` `stale_revision` with
`current_revision`). Every write tool takes an optional `actor` label. The
answer of a configuration write carries `revision`, `diff`, `restart_required`
and `audit_entry_ids`; with `dry_run` the revision is unchanged and nothing is
saved or audited.

*Resources.* `llm-scheduler://status` (`GET /api/status`),
`llm-scheduler://config` (`GET /api/config`) and
`llm-scheduler://docs/benchmark-guide` (the embedded guide, `docs/embed.go`).

*Audit source rule.* A request that reaches a handler through MCP is marked in
its context: `mcpserver.SourceFromContext(ctx)` returns `(true, actor)`. The
marker is an unexported context key set only by the in-process dispatcher; no
header is consulted and a context cannot cross the network, so a REST client
cannot claim to be MCP. The audit log records such a change with
`source: "mcp"`, and the actor is the client name and version from the
`initialize` handshake, or `<actor argument> (via <client>)` when the write
tool was given `actor`. `Options.Bridge` is the single hook that copies the
marker into the audit package's own context value; `cmd/llm-scheduler`
sets it to `audit.WithSource(ctx, "mcp", actor)`. Through MCP, benchmark and
suite changes and run start and cancel (section `bench`) and `load_model` and
`unload_model` (section `runtime`) are audited with source `mcp` as well.

## 10. Packaging and rollout

- Image: `FROM llama-swap-whisper:rpc-330f018`, add the `llm-scheduler` binary,
  replace the entrypoint. Every binary the profiles need is already there.
- Compose: `container_name: llama-swap` (the hub finds the container by that
  name prefix), port `3122:8080`, `/models` read-only, NVIDIA device
  reservation, `init: true`, `stop_grace_period: 330s`,
  `extra_hosts: host.docker.internal:host-gateway`, and a pinned bridge subnet
  `172.30.0.0/24`. The `/run/avahi-daemon` mount is dropped: the scheduler uses
  IP addresses only.
- **Host firewall:** the container must reach the fleet console on host port
  8090. On this box it did so with no extra rule. The runbook checks it with
  `curl http://host.docker.internal:8090/api/fleet` from inside the container;
  only if that fails, allow `172.30.0.0/24` to port 8090. The scheduler logs a
  clear error if it cannot connect.
- **The scheduler and llama-swap must never run at the same time.** A worker
  serves one client; whichever connects second waits. Rollout is a cutover:
  1. Stop llama-swap.
  2. Start the scheduler on 3122.
  3. Run the runbook's smoke checks, starting with `POST /api/plan` for each
     profile, which changes nothing.
  4. Roll back by stopping the scheduler and starting llama-swap.
- The configuration is in the database under the mounted data directory (no config
  file is mounted). A first install imports `deploy/seed-config.yaml` once (6.0.5).
- The build deploys nothing. It produces the image recipe, compose file,
  seed config and runbook. The owner deploys.

## 11. Testing

TDD: for every task, tests are written and seen failing before the code.

- **Unit:** config validation, GGUF parsing (fixture files generated in tests),
  estimate maths, command building (exact argument order), `--list-devices`
  output parsing, placer (table-driven: tiers, pools, forced nodes, plans).
- **Scheduler core, rule tests:** fake clock, node view, prober and launcher;
  events fed in a fixed order. One test per rule in 8.2-8.6.
- **Scheduler core, random tests:** generated event sequences (requests,
  disconnects, load results, node changes, clock ticks) checked after every
  event against invariants: a node has at most one holder and at most one
  claim, and if it has both, the holder is a victim of that claim; every claim
  belongs to a live entry; every victim belongs to a live claimed entry; at
  most one instance and one entry per profile; no instance has negative
  `in_flight`; `draining` instances admit nothing; and when the sequence is
  drained to quiescence every waiter has received exactly one answer.
- **Integration:** `fakellama`, a small Go binary built by the tests, stands in
  for `llama-server-rpc`: `--list-devices` output, health after a delay,
  load-log lines, streaming chat completions, `/props`, `/slots`, scripted
  failures and hangs. A fake fleet console serves the list, single-node lookup
  and SSE, and a fake stands in for `nvidia-smi`. The real scheduler binary
  runs against all three, with `-race`.
- **Platform split:** SIGTERM-then-SIGKILL escalation is tested on Linux only;
  on Windows the tests assert only that the child is gone.
- **On hardware (owner, with runbook):** H1, H2, H3; `--device` and
  `--tensor-split` behaviour on a PS5 pool; load timing (there is no warm reload since 2026-11-18); measured
  `needs_mib` for each Gemma profile; estimate versus observed memory for the
  others; worker power-off during a generation; a 10-agent mixed-model soak.

Code builds and tests on Windows (development) and Linux (container). Process
groups and signals sit behind one small OS-specific file.

## 12. Build plan

| # | Milestone | Proves |
|---|---|---|
| M0 | Repo scaffold, test task, `fakellama` | Test harness works |
| M1 | Config, command builder, prober, load-log reader, instance manager, proxy | Start a child on a fixed placement, stream through it, stop it. (Scheduling rules, including idle unload, arrive in M4.) |
| M2 | Fleet client, local probe, node view | Live nodes in MiB |
| M3 | GGUF reader, estimate, placer (single node, pools, forced nodes, plans) | "If it fits, it fits", as pure functions |
| M4 | Scheduler core: queue, claims, eviction, draining, failures, shutdown | Busy-cluster behaviour |
| M5 | `kind: command`, llama-swap compatible paths, aliases, admin paths | Drop-in |
| M6 | Dockerfile, compose, example config, runbook, hardware check scripts | Deployable |

## 13. Open questions

1. H1, H2 and H3.
6. Hold time. A model that took 15 minutes to load is protected from eviction
   for 15 minutes, so a request for a different model that needs the same
   nodes can get `503` and must retry. Shorter `max_hold` means faster turns
   and more time spent loading. Since there are no cached reloads (3.2), every
   reload is a full upload at about 100 MB/s per PS5 (80 MB/s in total on the old 1 GbE link), so a hold of one load time is a
   real protection. To be tuned on the real cluster.
2. PS5 usable memory: sources disagree (12 GB, 14.1 GB, 14948 MiB). The
   scheduler uses the pre-flight figure at load time; the hardware checks
   compare it with what actually loads.
3. `qwen3.5-4b` runs at 76 tokens/s on the host machine's GPU today. Under the tier
   order it moves to a PS5, which is probably slower. A `prefer_tiers` field
   could keep it on the host machine if that matters.
4. The hub's llama panel shows one loaded model and assumes a 300 s TTL. With
   several models loaded at once it will show only one until the panel is updated.
5. Token counts in A.4 are filled on a best-effort basis from the tail of each
   response; `-1` when the upstream does not report them.

## 14. Later

- **Slots: several models per PS5.** Run two or three `rpc-server` containers
  per PS5 on fixed ports, always on. The PS5's node agent reports the port
  list, the fleet manager passes it through, and the scheduler pulls it as
  today. A node becomes a memory budget with slots instead of an exclusive
  unit. No commands are sent to PS5s, so the registration structure (decision
  11) is unchanged. Needs trustworthy memory figures first, since nothing but
  the scheduler's bookkeeping stops two models overfilling one PS5.
- Exact KV estimates for sliding-window models, removing the need for measured
  `needs_mib`.
- Learned memory needs; backfill of claimed-but-idle nodes; host RAM budgeting;
  `prefer_tiers`; pushing model state to the fleet manager directly.

## Appendix A. llama-swap compatible shapes

Taken from the consumers' code in the host machine's hub (a separate repository):
the hub's push loop, its nginx configuration and its llama panel. These are the
contract; M5 tests assert them. The only other caller of port 3122's
non-chat paths, `apps/imagine-dispatch`, uses `POST /v1/images/generations`.

### A.1 `GET /running`

Polled by the hub every 1 s with a 1 s timeout on a keep-alive connection.

```json
{ "running": [
  { "model": "gemma-4-12b:64k", "state": "ready", "name": "gemma-4-12b:64k",
    "cmd": "llama-server-rpc --host 127.0.0.1 --port 5801 --model /models/gemma-4-12b-it-Q4_K_M.gguf --n-gpu-layers 999 --fit off --ctx-size 65536 --parallel 1 --jinja --cache-ram 2048 -lv 4 --rpc 192.0.2.2:50052 --device RPC0 -ctk q8_0 -ctv q8_0 --flash-attn on" }
] }
```

- `model` and `name`: the profile name, never an alias.
- `state`: `starting`, `ready` or `stopping`. The hub drops `stopping` and
  `stopped`, shows `starting` as loading, and treats anything else as ready.
  A `draining` instance is reported as `ready`. While an instance is still in
  pre-flight it has no port yet: `cmd` then omits `--port` (llama) or is empty
  (command).
- `cmd`: the real command line as one string. The hub extracts workers with
  `--rpc[= ]+(\S+)` and context with `(?:--ctx-size|-c)[= ]+(\d+)`, so the
  scheduler always writes `--rpc a,b` and `--ctx-size n` in that form.
- `load_progress` (number, 0 to 0.99) is an addition on an entry whose
  instance is `starting` and loads onto a remote worker (7.6a).
- `load` (object) is an addition on an entry whose child is loading: session,
  progress and per-worker upload state (7.6a). It is absent once the instance
  is ready.
- `recent_loads` (array) is a new top-level key next to `running`, always
  present: the outcomes of the load attempts finished in the last 120 s,
  newest first (7.6a).
  No other field changed; consumers that ignore unknown keys are unaffected.

### A.2 `GET /upstream/{model}/props` and `/slots`

The hub calls these every second for each entry `/running` lists as not
starting. They pass through unchanged to the instance's own `/props` and
`/slots` when it is `ready` or `draining`; otherwise `503` at once. `{model}`
is URL-encoded (profile names contain `:`); names and aliases both resolve.

### A.3 `GET /api/events` (Server-Sent Events)

Read by a browser `EventSource` using `onmessage` only, so events are sent
**without** an `event:` line. Each `data:` line is a JSON envelope whose `data`
field is itself a JSON **string**:

```
data: {"type":"modelStatus","data":"[{\"id\":\"gemma-4-12b:64k\",\"state\":\"ready\",\"readySince\":\"2026-11-17T10:00:00Z\"}]"}

data: {"type":"inflight","data":"{\"operation\":\"snapshot\",\"requests\":[]}"}
```

- `modelStatus`: every profile, each `{id, state, readySince?}`. `state` is
  `ready`, `starting`, `stopping`, or `stopped` for unloaded profiles. The list
  replaces the client's whole view. Sent on connect and on every instance
  state change.
- `inflight`: `{operation:"snapshot", requests:[...]}` on connect, then
  `{operation:"upsert", request:{...}}` when a request is admitted and
  `{operation:"remove", id}` when it ends. A request is
  `{id: string, model: string, timestamp: ISO-8601}`.
- `activity`: `{}`; sent when a request finishes, telling the client to refetch A.4.
- `loadStarted`: sent when a load attempt starts, that is when its child has
  started. `data` is `{session_id, model, started_at, workers}`: `model` is
  the profile name, `started_at` unix ms, `workers` the remote workers'
  addresses in `--rpc` order (empty for a local load).
- `loadFinished`: sent when the attempt ends. `data` is the same object as the
  `recent_loads` entry: `{session_id, model, workers, outcome, reason,
  finished_at, load_s}` (7.6a). It is sent before the `modelStatus` that
  reports the instance `ready`. Neither event replaces `modelStatus`, which is
  still sent on every instance state change.

```
data: {"type":"loadStarted","data":"{\"model\":\"big\",\"session_id\":\"ld-1794995021087-3\",\"started_at\":1794995021087,\"workers\":[\"192.0.2.101:50052\",\"192.0.2.122:50052\"]}"}

data: {"type":"loadFinished","data":"{\"finished_at\":1794995221087,\"load_s\":215.1,\"model\":\"big\",\"outcome\":\"ready\",\"reason\":\"\",\"session_id\":\"ld-1794995021087-3\",\"workers\":[\"192.0.2.101:50052\",\"192.0.2.122:50052\"]}"}
```
- `workerHealth`: sent when a watched worker's health state changes (8.4a).
  `data` is `{node, addr, health}`, `health` being the object of `/api/status`
  (`{state, since, channels, silent_for_s}`). When the last instance on a
  non-`ok` worker ends, one more event is sent with no `health`.

```
data: {"type":"workerHealth","data":"{\"addr\":\"192.0.2.101:50052\",\"health\":{\"channels\":{\"heartbeat\":\"silent\",\"probe\":\"silent\",\"upload\":\"unknown\"},\"silent_for_s\":{\"heartbeat\":0,\"probe\":0.1},\"since\":1795055906526,\"state\":\"hung\"},\"node\":\"ps5-a\"}"}
```
- `recoveryRequest`: sent only with `recovery.enabled` (8.4b), when a recovery
  request is sent (`phase` `sent`) and when a node's outcome is seen (`phase`
  `outcome`). `data` is `{node, phase, recovery}`, `recovery` being the object
  of `/api/status` (`{last_request, last_outcome, step, episode_started}`).

```
data: {"type":"recoveryRequest","data":"{\"node\":\"ps5-a\",\"phase\":\"sent\",\"recovery\":{\"episode_started\":1795058743515,\"last_request\":{\"action\":\"restart_rpc_server\",\"http_status\":0,\"nonce\":\"529a35e343727f1c3f403fc090490810\",\"sent_at\":1795058743515,\"status\":\"pending\"},\"step\":1}}"}
```
- Each event is flushed at once. The connection may stay open for an hour.
  A comment line (`: ping`) is sent every 15 s.

### A.4 `GET /api/metrics/activity`

The last 200 finished requests, kept in memory:

```json
{ "data": [
  { "id": 1, "timestamp": "2026-11-17T10:01:00Z", "model": "gemma-4-12b:64k",
    "req_path": "/v1/chat/completions", "resp_status_code": 200, "duration_ms": 1200,
    "tokens": { "input_tokens": 50, "output_tokens": 100, "tokens_per_second": 42.0 } }
] }
```

`id` is a number, increasing. Token fields come from `usage` and `timings` in
the last 16 KB of the upstream response when present, else `-1`.

### A.5 `GET /health`

`200` whenever the scheduler is running, including with nothing loaded.
