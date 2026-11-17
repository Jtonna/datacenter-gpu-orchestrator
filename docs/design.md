# Cluster scheduler: design

Status: v1.0, approved to build from after three review passes. Owner: Jacob.
Date: 2026-11-17. Changes to behaviour go into this document first.

A drop-in replacement for llama-swap on the OMEN that decides at load time which
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
(`omen-runner-fleet-manager/apps/llama-swap/config.yaml`). Every added node
multiplies it.

Wanted: ask for `gemma-4-12b:64k` and have the system pick the machines, using
live availability, with the PS5s preferred and the laptops as fallback.

## 2. Decisions already made by the owner

These are requirements, not proposals. Reviewers should check that the design
meets them, not reopen them.

1. Custom scheduler that **replaces** llama-swap. Not a layer above it.
2. Written in Go, one Docker container on the OMEN.
3. Drop-in for existing apps: same port (3122), same OpenAI-style endpoints.
4. Nodes come from the fleet manager at run time. Nodes join and leave.
5. Priority tiers: PS5s first, Alienware second, OMEN (host) last.
6. One profile per model + context size. No pool count in the name.
   "If it fits, it fits": one node when it fits, several when it does not.
7. A profile may force specific nodes, for benchmarks.
8. Names follow `model:variant` (e.g. `gemma-4-12b:64k`). Old names are
   hand-configured aliases, logged on use, to be phased out.
9. When nothing is free, requests **queue**, as with llama-swap: a running
   generation is never interrupted by scheduling; a model is unloaded to make
   room only once nothing is running on it.
10. Built test-first (TDD).
11. Registration structure stays as it is: PCs register with the OMEN hub, PS5s
    push to the fleet manager, the hub relays PCs into the fleet manager, and
    other programs (this one included) only **pull** from the fleet manager.

## 3. Environment

| Node | GPU | Memory | RPC address | Fleet source |
|---|---|---|---|---|
| OMEN (host) | RTX 5060, CUDA | 8 GB VRAM | local | relayed by hub; read locally instead (5.1) |
| Alienware (`alienware-llm`) | RTX 3070 Ti, CUDA | 8 GB VRAM | `10.0.0.3:50052` | relayed by hub |
| PS5 (`ps5-062e1d84`) | AMD Oberon, Vulkan | about 14-15 GB shared | `10.0.0.2:50052` | pushes itself |
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
- **[verified by owner]** PS5 and Alienware firewalls admit port 50052 from the
  OMEN only. llama-swap reaches both today from a bridged container. Every head
  process must run on the OMEN.
- **[verified]** PS5 link is 100 Mb today, about 11.8 MB/s. Sending 5 GB to a
  PS5 takes about 7 minutes. The `rpc-server` tensor cache (`-c`) shortens
  repeat loads.
- **[verified by owner]** The old "wedges on client close" and "stalls over
  500 MiB" problems belonged to the PS5LM app, not PS5 Linux.
- **[verified]** The OMEN's host firewall admits only ports 22, 9443 and 5353
  by default. Reaching the fleet console on 8090 from a container needs a rule.
- **[assumed] H1.** `nvidia-smi` is available inside the container.
- **[assumed] H2.** Each worker's `rpc-server` exposes exactly one device.
- **[assumed] H3.** `--list-devices` runs cleanly with `CUDA_VISIBLE_DEVICES=""`
  (no local GPU touched). If not, the probe runs without that setting.

### 3.3 Fleet manager facts

All **[verified]**.

- Fleet console backend on `0.0.0.0:8090`, state in memory only (a restart
  empties it until nodes report again). `GET /api/fleet`,
  `GET /api/nodes/:id`, SSE at `GET /api/stream?topics=fleet,models` with a
  ping every 5 s. The stream sends a snapshot only when a meaningful field
  changes, not on every report. Offline after 15 s without a report.
- A member's key is `id` (its MAC when known). Two consoles can share a
  `hostname`.
- `FleetMember` fields used here: `id`, `hostname`, `role`, `source`, `online`,
  `rpc`, `ip`, `rpc_port`, `vram_total`, `vram_used`, `ram_total_gb`,
  `ram_used_gb`.
- `vram_total` and `vram_used` are in **GB**. For relayed nodes the hub
  truncates the total to an integer and derives used from a percentage,
  measured every 5 s. PS5s report amdgpu GTT figures.
- `rpc` is a TCP connect probe that also reads `true` on a connect timeout.
  It cannot tell a healthy worker from a busy or firewalled one.
- `ip` (IPv4 or null) and `rpc_port` (number or null) are on the fleet console
  branch `fleet-scheduler-fields` (going live). Relayed members show
  `ip: null` until the hub patch is deployed (branch `hub-member-ip` of
  `omen-runner-fleet-manager`, written, not deployed): it sends the Alienware
  as `10.0.0.3` / `50052` and the OMEN as `10.0.0.1` / `null`.

## 4. Goals and non-goals

Goals:

- One endpoint; placement decided per load from live state.
- Config holds profiles and policy only. Adding a PS5 needs no config edit.
- Existing clients and the OMEN hub's llama panel keep working.
- Correct under concurrency: many agents arriving at random times.

Non-goals for v1 (see section 14 for the planned follow-ups):

- Two models on one node. Every node is exclusive to one instance, because
  `rpc-server` is single-client.
- Exact memory estimates for sliding-window models. Those profiles take a
  measured `needs_mib` (7.2).
- Learning memory needs automatically.
- Moving a loaded model without a reload; several replicas of one profile.
- Using free nodes that an earlier request has claimed while it waits (no backfill).
- Budgeting host RAM on the OMEN. Nodes are exclusive, so there are never more
  instances than nodes, and each instance's host prompt cache is capped.
- Authentication (unchanged: LAN-only firewall) and a web UI.
- Starting or stopping `rpc-server` on nodes.

## 5. Architecture

One Go process in one container on the OMEN.

```
clients ──HTTP──▶ ┌──────────────── llm-scheduler ────────────────┐
(agents, IDE,     │ api/proxy ─▶ scheduler core ─▶ instance mgr ──┼─▶ llama-server-rpc (child)
 hub panel)       │              (one goroutine)        │         │      │ --rpc a,b
                  │                 │  ▲             prober ──────┼─▶ --list-devices (pre-flight)
                  │              placer │                         │      ▼
                  │                 ▼  │                          │   rpc-server on PS5s / Alienware
                  │   node view ◀─ fleet client ◀── SSE ── fleet console (:8090)
                  │       ▲─────── local GPU probe (nvidia-smi)   │
                  └───────────────────────────────────────────────┘
```

| Package | Job |
|---|---|
| `internal/config` | Load and validate the YAML file. |
| `internal/gguf` | Read the GGUF header fields needed for memory estimates. |
| `internal/estimate` | Memory need per profile (pure functions). |
| `internal/fleet` | Fleet console client (SSE, polling fallback, single-node fetch), local GPU probe, merged node view in MiB. |
| `internal/placer` | Pure functions: place, plan an eviction, or say why not. |
| `internal/instance` | Command builder, pre-flight prober, start / health-check / stop one child, port allocation. |
| `internal/scheduler` | The state machine: instances, claims, queue, timers. |
| `internal/proxy` | Extract the model name; stream the request to the instance. |
| `internal/api` | Routes: OpenAI paths, llama-swap compatible paths, admin paths. |
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

The OMEN's GPU is read with
`nvidia-smi --query-gpu=memory.total,memory.used --format=csv,noheader,nounits`
every 2 s (H1), not from the fleet manager: exact MiB, and `kind: command`
profiles keep working when the fleet console or the hub is down. If
`nvidia-smi` is missing, the scheduler logs a warning and falls back to the
fleet entry for `fleet.local_node`. The fleet entry for the local node is
otherwise ignored. The probe command is a config value (`local_probe_cmd`), so
tests point it at a fake.

## 6. Configuration

One YAML file. Nodes are not listed; only how to rank them.

**Node identity.** Internally nodes are keyed by the fleet `id`. Where config
names a node (`force_nodes`, `node_overrides`, `fleet.local_node`), the name
may be an `id` or a `hostname`. A hostname shared by two online members is
ambiguous: it matches neither, and the scheduler logs an error naming both ids.

```yaml
listen: ":8080"                    # mapped to 3122 by compose
fleet:
  url: http://host.docker.internal:8090
  local_node: omen-runner
  stale_after: 60s                 # view age beyond which remote nodes are not used
models_dir: /models

defaults:
  ttl: 300s                # unload after this long idle; 0 = never
  min_hold: 30s            # hold time floor (8.3)
  max_hold: 900s           # hold time ceiling
  drain_after: 60s         # wait before a busy model is asked to drain
  queue_timeout: 600s      # max time an entry may wait without a claim
  stall_timeout: 300s      # max gap between chunks once streaming has begun
  command_load_timeout: 900s   # for kind: command profiles
  shutdown_grace: 300s
  max_pool: 4
  device_overhead_mib: 600 # per-device cost added to estimated needs only
  min_share_mib: 1024      # least a node must be able to hold
  cache_ram_mib: 2048      # per-instance prompt cache limit on the host
  server_bin: llama-server-rpc
  local_device: CUDA0

tiers:                     # first match wins; lower number preferred;
                           # a node matching no tier is never used
  - match: { hostname: "ps5-*" }
    priority: 1
    headroom_mib: 1024
    shared_memory: true    # GPU memory is system RAM
    link_mb_s: 11          # used to size load timeouts
    settle: 3s
  - match: { hostname: "alienware*" }
    priority: 2
    headroom_mib: 256      # dedicated VRAM; pre-flight gives exact figures
    link_mb_s: 11
    settle: 8s             # relayed figures are measured every 5 s
  - match: { local: true }
    priority: 3
    headroom_mib: 256
    settle: 2s

node_overrides:            # optional; wins over the fleet's ip/rpc_port
  alienware-llm: { rpc: "10.0.0.3:50052" }

profiles:
  gemma-4-12b:             # bare name = the default variant
    model: gemma-4-12b-it-Q4_K_M.gguf
    ctx: 8192
    needs_mib: 7800        # measured; see 7.2
    aliases: [gemma-4-12b-1pool-8k, gemma-4-12b-2pool-8k]
  "devstral-24b:14k":
    model: Devstral-Small-2-24B-Instruct-2512-Q4_K_M.gguf
    ctx: 14336
    kv_quant: q8_0
    allow_tiers: [1, 2]              # never the OMEN's GPU
  gemma-bench-2pool:                  # benchmark: forced placement
    model: gemma-4-12b-it-Q4_K_M.gguf
    ctx: 65536
    kv_quant: q8_0
    flash_attn: true
    force_nodes: [ps5-062e1d84, alienware-llm]
    tensor_split: [60, 40]
  large-v3-turbo:                     # non-LLM: fixed command on the OMEN
    kind: command
    cmd: env -u LD_LIBRARY_PATH /opt/whisper-server/venv/bin/python /opt/whisper-server/server.py --host 127.0.0.1 --port ${PORT} --model large-v3-turbo
    check_endpoint: /health
    needs_mib: 5400
    stall_timeout: 0
```

`tier.match` keys: `hostname` (glob), `role` (exact), `source` (exact),
`local` (bool). Several keys in one match must all hold.

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
| `ttl`, `aliases`, `load_timeout`, `stall_timeout` | Per-profile overrides. |

Fields for `kind: command`: `cmd` (with `${PORT}`), `check_endpoint`,
`needs_mib` (required), `ttl`, `aliases`, timeouts. Always placed on the local
node alone.

Startup validation fails fast on: unknown fields; duplicate names or aliases; a
missing model file; `force_nodes` with `allow_tiers`; `tensor_split` without
`force_nodes` or of a different length; quantised KV without `flash_attn`;
`extra_args` containing a managed flag in any spelling (`--rpc`, `--device/-dev`,
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
    force_nodes: [alienware-llm, omen-runner]
    tensor_split: [50, 50]
```

## 7. Placement

### 7.1 Node view and budgets

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

A set of nodes fits when `Σ (budget_i − device_overhead) ≥ need × 1.05`. The
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

Worked checks, used as golden placer tests (OMEN 8151 MiB, Alienware 7885 MiB,
both empty, headroom 256):

| Case | Arithmetic | Result |
|---|---|---|
| `gemma-4-12b`, `needs_mib: 7800`, OMEN alone | 8151 − 256 = 7895 ≥ 7800 | fits |
| same, Alienware alone | 7885 − 256 = 7629 < 7800 | does not fit |
| Devstral 14K q8, estimated about 14828, both laptops | (7895 − 600) + (7629 − 600) = 14324 < 15570 | does not fit (see 6.1) |

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

So a pool of PS5s is chosen before anything that touches the Alienware, and the
OMEN is reached only when tiers 1 and 2 together cannot hold the model.

### 7.4 Command line

Built in this fixed order; `extra_args` always last:

```
llama-server-rpc --host 127.0.0.1 --port <p> --model /models/<file>
  --n-gpu-layers 999 --fit off --ctx-size <ctx × parallel> --parallel <n> --jinja
  --cache-ram <cache_ram_mib>
  [--rpc ip1:port,ip2:port]              # remote nodes, placement order
  --device <names>                       # exactly the chosen devices, always present
  [--tensor-split a,b,c]                 # same order as --device; omitted for one device
  [-ctk <q> -ctv <q>] [--flash-attn on] [extra_args...]
```

- Device names: the i-th remote node is `RPC<i>` (H2: one device per worker);
  the local node is `local_device`. `--device` lists them in placement order.
- `--tensor-split` values are each node's budget from the pre-flight figures,
  as integers, unless the profile gives `tensor_split`.
- Local only: no `--rpc`, `--device CUDA0`.

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

The local node is not probed; `nvidia-smi` already gives exact figures.
No child is ever started against a worker that would hang it.

### 7.6 Load timeout

Unless the profile sets `load_timeout`:
`120 s + 2 × Σ over remote nodes (node's share of the model file ÷ link_mb_s)`.
A share is the file size times that node's fraction of the tensor split.
Sizes are in bytes and `link_mb_s` is decimal megabytes per second.
Example: 10 GB (10^10 bytes) to a PS5 at 11 MB/s gives about 32 minutes.
`kind: command` profiles use `command_load_timeout`.

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
3. **Skip.** Otherwise the entry stays unclaimed and the pass moves on.

An instance is **evictable** for an entry when it is `ready`,
`now − ready_at ≥ hold`, and either `in_flight == 0`, or the entry has been
unclaimed for at least `drain_after`. `hold = clamp(load_duration, min_hold,
max_hold)`: a model is guaranteed to serve for as long as it took to load
(at most `max_hold`), so two alternating models do not spend all their time
loading.

**Claimed entry**

4. When every victim has exited and every claimed node has settled, run the
   placer over exactly the claimed nodes with live budgets. If it returns a
   placement, start it as in step 1 (nodes it does not use are released).
   If it does not (something else is using the memory): record each claimed
   node's observed free figure as its `empty` figure (7.1), drop the claim, and
   treat the entry as unclaimed in this same pass. The recorded figures stop
   the same plan from being chosen again.
5. If a claimed node stops being eligible (offline, quarantined, or the fleet
   view goes stale): drop the claim. Victims still `draining` return to
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
  cluster busy, forced nodes unavailable, or fleet view stale.
- **Idle unload:** `ready`, `in_flight == 0`, `now − last_used ≥ ttl` →
  `stopping`. Paused for instances on remote nodes while the fleet view is
  stale, since they could not be reloaded.
- Known cost: free nodes inside a claim sit unused while the claim's victims
  drain. Accepted for v1.
- Known behaviour: an idle LLM instance that includes the OMEN's GPU is evicted
  when Whisper or FLUX is requested, and may evict them back later. `hold`
  limits how often.

### 8.4 Failures

| Event | Response |
|---|---|
| Pre-flight fails for a node | Node quarantined 30 s, doubling per repeat up to 10 min, reset by a successful pre-flight. The instance is returned (7.5). |
| Child exits, or the load timeout passes, during `starting` | Instance → `stopping`. Waiters get `502` with the last lines of the child's output. The **profile** is backed off 60 s, doubling per repeat up to 10 min, reset when the profile next reaches `ready`. During backoff its requests fail fast with `503`, and any entry for it is answered `503` and deleted. Nodes are not quarantined: the pre-flight already cleared them. |
| Child exits while `ready` or `draining` | In-flight requests fail; instance → `absent`. |
| A remote node in use is offline in, or absent from, a live view for 30 s continuously | Node loss: instance → `stopping` with SIGKILL; in-flight requests fail. Needed because a blocked RPC socket does not make the child exit. The 30 s counter resets whenever the feed reconnects, so a fleet console restart is not read as every node vanishing. |
| A streaming response sends nothing for `stall_timeout` **after its first chunk** | That request is aborted with an error event. The instance is not stopped. Time before the first chunk (prompt processing) is never timed. |
| A non-streaming request hangs | Not detected in v1, other than by node loss. `POST /api/unload` with `force` clears it. |
| Fleet feed down | Reconnect SSE with backoff; poll `GET /api/fleet` every 5 s meanwhile. View age is the time since the last byte from the feed, pings included. Loaded instances keep serving. Past `stale_after`, remote nodes are not eligible; the local node still is. |

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
   again after another settle period.
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
| `GET /ui/` | A one-line page linking to `/api/status`. |
| `GET /health` | `200` whenever the scheduler is running. |
| `GET /api/status` | Instances, placements, per-device memory from the last pre-flight, claims, queue, quarantines, backoffs, node view and its age. |
| `POST /api/plan` `{model}` | Dry run: what would happen if this model were requested now (placement, plan with victims, or the reason for neither). Changes nothing. |
| `POST /api/load` `{model}` | Start loading ahead of time. Returns `202` at once. |
| `POST /api/unload` `{model, force}` | Stop one instance if idle; `force: true` stops it even if busy. |

`force: true` is the one way a running generation can be cut off, and only by
an explicit admin call.

Streaming: `httputil.ReverseProxy` with immediate flush; no response-header
timeout; client cancellation propagates upstream.

## 10. Packaging and rollout

- Image: `FROM llama-swap-whisper:rpc-330f018`, add the `llm-scheduler` binary,
  replace the entrypoint. Every binary the profiles need is already there.
- Compose: `container_name: llama-swap` (the hub finds the container by that
  name prefix), port `3122:8080`, `/models` read-only, NVIDIA device
  reservation, `init: true`, `stop_grace_period: 330s`,
  `extra_hosts: host.docker.internal:host-gateway`, and a pinned bridge subnet
  `172.30.0.0/24`. The `/run/avahi-daemon` mount is dropped: the scheduler uses
  IP addresses only.
- **Required host firewall rule:** allow `172.30.0.0/24` to port 8090, so the
  container can reach the fleet console. The runbook gives the command and
  verifies it with `curl http://host.docker.internal:8090/api/fleet` from
  inside the container. The scheduler logs a clear error if it cannot connect.
- **The scheduler and llama-swap must never run at the same time.** A worker
  serves one client; whichever connects second waits. Rollout is a cutover:
  1. Stop llama-swap.
  2. Start the scheduler on 3122.
  3. Run the runbook's smoke checks, starting with `POST /api/plan` for each
     profile, which changes nothing.
  4. Roll back by stopping the scheduler and starting llama-swap.
- The build deploys nothing. It produces the image recipe, compose file,
  example config and runbook. The owner deploys.

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
  `--tensor-split` behaviour on a PS5 pool; cold and warm load timing; measured
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
   and more time spent loading. To be tuned on the real cluster.
2. PS5 usable memory: sources disagree (12 GB, 14.1 GB, 14948 MiB). The
   scheduler uses the pre-flight figure at load time; the hardware checks
   compare it with what actually loads.
3. `qwen3.5-4b` runs at 76 tokens/s on the OMEN's GPU today. Under the tier
   order it moves to a PS5, which is probably slower. A `prefer_tiers` field
   could keep it on the OMEN if that matters.
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

Taken from the consumers' code in `omen-runner-fleet-manager`:
`host/omen-hub-nodes.py` (the hub's push loop), `apps/omen-hub/nginx.conf` and
`apps/omen-hub/web/src/lib/llama.ts` (the hub's llama panel). These are the
contract; M5 tests assert them. The only other caller of port 3122's
non-chat paths, `apps/imagine-dispatch`, uses `POST /v1/images/generations`.

### A.1 `GET /running`

Polled by the hub every 1 s with a 1 s timeout on a keep-alive connection.

```json
{ "running": [
  { "model": "gemma-4-12b:64k", "state": "ready", "name": "gemma-4-12b:64k",
    "cmd": "llama-server-rpc --host 127.0.0.1 --port 5801 --model /models/gemma-4-12b-it-Q4_K_M.gguf --n-gpu-layers 999 --fit off --ctx-size 65536 --parallel 1 --jinja --cache-ram 2048 --rpc 10.0.0.2:50052 --device RPC0 -ctk q8_0 -ctv q8_0 --flash-attn on" }
] }
```

- `model` and `name`: the profile name, never an alias.
- `state`: `starting`, `ready` or `stopping`. The hub drops `stopping` and
  `stopped`, shows `starting` as loading, and treats anything else as ready.
  A `draining` instance is reported as `ready`.
- `cmd`: the real command line as one string. The hub extracts workers with
  `--rpc[= ]+(\S+)` and context with `(?:--ctx-size|-c)[= ]+(\d+)`, so the
  scheduler always writes `--rpc a,b` and `--ctx-size n` in that form.

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
