# Agent contract: Potluck and `potluck-agent`

Status: DRAFT 5, 2026-11-20 (drafts 1 to 3 reviewed by the PS5 side; change requests CR-1, CR-2 and CR-3 folded in; CR-5, accepted 2026-11-20, adds section 11.1 and the lines marked CR-5; CR-6, accepted 2026-11-21, from the first run on a real machine, adds section 6.12 and the lines marked CR-6). Nothing in this document is built. It is task C1 of the plan "Potluck Cleanup Tasks". It becomes binding when the scheduler side and the fleet console agent both accept it.

Authors: the scheduler agent (`ps5-ai-datacenter-load-balancer-32`) wrote it; the fleet console agent (`fix-fleet-console-tiles`) reviews it against the PS5 side (boot chain, RAM-only, payload delivery).

It replaces `docs/server-mode-interface.md` (DRAFT 3) once accepted. That document stays as the record of the measurements and of the share's container config (its sections 2 and 7.6).

Plan decisions this draft relies on: D1, D2 (the agent is a machine service and flips role per load), D4 (no PS5-specific code in Potluck), D8 (Potluck hands out programs), D9 (node_exporter), D11, D12 (two install paths, setup and uninstall scripts), D14 (agents are workers only in phase 4; hosting comes in phase 6), D15 (identity is hardware-derived).

## 1. Terms

Each term has one meaning in this document.

- **Potluck**: the scheduler. One instance, on the host machine, port 3122.
- **agent**: `potluck-agent`, one static Go binary, one per pool machine.
- **node**: a machine that runs an agent.
- **program**: a binary the agent runs but does not contain: `llama-server`, `rpc-server`, `node_exporter`, `nvidia_gpu_exporter`.
- **release**: one version string that names a matching set of Potluck, agent and programs.
- **launch**: one run of one llama program on one node, from start request to exit.
- **role**: what a node is doing: `idle`, `worker` (runs `rpc-server` for one load) or `host` (runs `llama-server` for one load).
- **share**: the read-only NFS export of the model files, served by the Potluck stack.
- **bare-metal install**: the agent runs as a systemd service on the machine. **Docker install**: the agent runs in a container.
- **starter**: the small script that fetches and starts the agent on a RAM-only node (section 5.4).

## 2. Rules that hold everywhere

1. **No wall-clock time from a node.** PS5 clocks are wrong. An agent reports ages and durations in seconds from a monotonic clock that keeps counting across agent restarts (`CLOCK_BOOTTIME`). Potluck turns them into its own timestamps. No field in this contract is a timestamp set by a node.
2. **No authentication**, as accepted for this cluster. The agent accepts requests only from the Potluck address it was configured with at install (source address check). Two exceptions, both explicit: the read-only local call of section 6.9, and extra source addresses given at install (section 9), used only on the machine that runs Potluck itself.
3. **One launch per node at a time.** A launch that has exited does not count: its last state stays readable, and the node is free.
4. **Nothing for programs, models or launch state on a RAM-only node's drive.** Programs and logs live under `/dev/shm/potluck` (executable). State files and the share mount point live under `/run/potluck` (`/run` is `noexec` on a PS5, so no program is ever placed there). The one file on the drive is the uninstall marker of section 10.
5. **Potluck holds no machine-type code.** What differs per machine is data the agent reports or an install option. `ram_only`, `install` and a device's `shares_system_memory` are used only for display and for the generic rules written in this document. No decision in Potluck may use them as a stand-in for "this is a PS5".
6. **No fallback.** A node that cannot do what a load needs is reported and the load is refused or retried. A load never changes shape silently.
7. **Membership is registration only.** From the first agent release (phase 4) a machine without a registered agent is not in the pool. Potluck reads no other membership source.

**Accepted risk, stated once.** With no firewall on the PS5 link and no authentication, any device on that link can take a worker's single client slot or call an agent's ports from a spoofed address. Potluck's own port 3122 is bound to all interfaces of the host machine, so pause, force unload and node removal are open to the home network too. The owner has accepted no authentication.

## 3. Ports

| Port | Program | Bind address | Who connects |
|---|---|---|---|
| 3122 | Potluck: API, UI, agent registration, program download | all interfaces of the host machine, as today | agents, clients |
| 2049 | share (NFSv4) | `192.0.2.1` | nodes that mount the share |
| 50060 | agent API | the node's address that reaches Potluck, and `127.0.0.1` | Potluck; local programs for `GET /status` only |
| 50052 | `rpc-server` during a worker launch | the node's address that reaches Potluck | the host of that load (Potluck's own host in phase 4, a host node from phase 6) |
| 50061 | `llama-server` during a host launch (phase 6) | same address | Potluck only |
| 50070 | `node_exporter` | `127.0.0.1` | the local agent only |
| 50071 | `nvidia_gpu_exporter` (Nvidia nodes) | `127.0.0.1` | the local agent only |

All ports are fixed. Ports 50060, 50070 and 50071 must be free when the agent starts; if one is taken, registration fails with a plain reason. Ports 50052 and 50061 are only checked when a launch needs them (section 6.2).

## 4. Registration, identity and heartbeat

### 4.1 Identity

- `id`: the lowercase MAC address of one physical network interface, for example `02:00:00:00:00:01`. It is computed at every agent start and never stored, so a cloned drive or a swapped network card cannot carry a wrong id.
- Which interface: the one the route to the configured Potluck address leaves by. When that address belongs to the machine itself (the host machine), the interface that owns the address. When the result is not a physical interface (loopback, bridge, VLAN, `docker0`, `veth`), the interface of the default route. If no physical interface is found, the agent does not register and reports why on `GET /status`.
- On a PS5 this is the Ethernet interface, which is the id the five consoles have in Potluck today.
- `name`: a display label. The agent sends the machine's hostname as the default. Potluck may hold a different label. Nothing is keyed by `name`.
- Consequence to accept: the host machine's id today is the string `host-1`. It becomes its MAC. No profile forces it, so nothing breaks.

### 4.2 `POST /api/pool/register` (agent to Potluck)

Sent at agent start, and again whenever Potluck answers a heartbeat with `410`.

```json
{
  "id": "02:00:00:00:00:01",
  "name": "console-5",
  "agent_release": "0.1.0",
  "agent_start_id": "k3f9a2",
  "os": "linux",
  "arch": "amd64",
  "install": "bare",
  "addr": "192.0.2.101",
  "gpus": [
    {"vendor": "amd", "model": "AMD Custom GPU 0405", "integrated": true}
  ],
  "backends": ["vulkan"],
  "mem_total_mib": 15258,
  "ram_only": true,
  "model_source": {"kind": "share", "target": "192.0.2.1:/"},
  "programs": [],
  "launch_id": null,
  "foreign_processes": []
}
```

- `agent_start_id` is random per agent process. A change tells Potluck the agent restarted.
- `install` is `bare` or `docker`.
- `gpus[].vendor` is `amd`, `nvidia`, `intel` or `other`. `backends` lists what the machine can run (`vulkan`, `cuda`), found by the agent from the drivers present.
- `ram_only` is true when the agent was started with the RAM-only option. It is an install option, not a hardware test.
- `model_source`: section 8.
- `programs`: what the agent already holds and has just re-hashed, as `[{"name", "sha256"}]`, so an agent restart does not fetch again.
- `launch_id`: a launch that exists on the node at registration (the agent restarted, or Potluck did).
- `foreign_processes`: any `rpc-server` or `llama-server` process on the node that the agent did not start, as `[{"pid", "exe"}]`. Not empty means the agent sends the registration, is refused (`foreign_process`), and starts nothing.

Answer `200`:

```json
{
  "accepted": true,
  "release": "0.1.0",
  "platform": "linux-amd64-vulkan",
  "heartbeat_every_s": 2,
  "programs": [
    {"name": "rpc-server", "path": "/api/pool/programs/0.1.0/linux-amd64-vulkan/rpc-server",
     "sha256": "…", "bytes": 81234567, "build": "a01665d30"},
    {"name": "llama-server", "path": "…", "sha256": "…", "bytes": 0, "build": "a01665d30"},
    {"name": "node_exporter", "path": "…", "sha256": "…", "bytes": 0, "build": "1.13.1"}
  ],
  "exporters": [
    {"name": "node_exporter", "args": ["--web.listen-address=127.0.0.1:50070", "…"]}
  ],
  "paused": false,
  "stop_launch": null
}
```

- **Addresses.** The agent talks to the Potluck address it was configured with (`192.0.2.1:3122` on the PS5 link; a home-network machine uses another address of the same Potluck). Every `path` Potluck returns is relative and the agent appends it to its configured address. Potluck never returns an address of its own.
- `exporters[].args` come from the stats contract (section 7). The agent adds nothing to them, and refuses a listen address that is not on `127.0.0.1`.
- `stop_launch`: when the registration carried a `launch_id` Potluck does not know (Potluck restarted and lost it), Potluck names it here and the agent stops it. Registering never touches a launch Potluck does know.

Refusals, all `200` with `"accepted": false`, a `reason` word and a `message`:

| `reason` | Meaning |
|---|---|
| `agent_too_old` | `agent_release` is lower than Potluck's release. The answer carries `required_release` (section 5.4). |
| `potluck_too_old` | `agent_release` is higher than Potluck's release. The node waits. |
| `unsupported_hardware` | No platform in the release matches the node's `os`, `arch` and `backends`. |
| `duplicate_id` | Another live agent with a different `addr` holds this `id`. |
| `foreign_process` | `foreign_processes` is not empty. |
| `removed` | The node was removed in Potluck and has not been re-admitted (section 10). |
| `release_unavailable` | Potluck serves no verified program release (its `programs/<release>` folder is missing or failed its check, section 5.2), so it has nothing to hand out. The `message` says why. (CR-5, accepted 2026-11-20) |

**Order of the checks** (CR-5, accepted 2026-11-20). The first that fails is the answer: a release is served (`release_unavailable`); the node is not removed (`removed`); the releases are equal (`agent_too_old`, then `potluck_too_old`); no foreign process (`foreign_process`); the id is not held by another live agent (`duplicate_id`); a platform matches (`unsupported_hardware`). A registration with an empty `id` is refused before all of these, with `unsupported_hardware`.

A refused agent starts no program and no exporter, keeps answering `GET /status` with the reason, and registers again every 60 s.

### 4.3 `POST /api/pool/nodes/{id}/heartbeat` (agent to Potluck)

Every `heartbeat_every_s`. It carries everything Potluck needs to know about an idle node, so Potluck never polls an idle agent.

```json
{
  "agent_start_id": "k3f9a2",
  "seq": 1841,
  "uptime_s": 3682.1,
  "role": "idle",
  "launch_id": null,
  "programs_ready": true,
  "programs_error": null,
  "model_source": {"kind": "share", "reachable": true, "checked_age_s": 1.2},
  "devices": [
    {"name": "Vulkan0", "backend": "vulkan", "total_mib": 14336, "free_mib": 14010,
     "shares_system_memory": true, "read_age_s": 40.0}
  ],
  "mem_available_mib": 14420,
  "stats": {}
}
```

- `devices` is what the node's own llama program reports (`--list-devices`), read after the programs are verified and after every launch exits. It is the figure placement uses (D15). The agent does not compute it. `shares_system_memory` is true for an integrated device. A `POST /launch` that arrives during the read (under 2 s) waits for it; it is not refused.
- `mem_available_mib` is read by the agent itself from `/proc/meminfo`, not from an exporter. It is read after the programs are in place, so on a RAM-only node it already excludes them.
- `stats` is the object defined by the stats contract (section 7). A missing or unreadable `stats` never makes a heartbeat invalid.
- `model_source.reachable`: section 8.

Answer `200`: `{"release": "0.1.0", "paused": false}`.

**The agent's three failure cases, kept apart:**

| Case | Agent does |
|---|---|
| Heartbeat cannot be sent (no connection, timeout, `5xx`) | Keeps its state, its launch and its exporters. Sends the next heartbeat on time. Nothing else. The lease decides the fate of a launch. |
| Heartbeat answered `410` (Potluck does not know this agent, or the heartbeat's `agent_start_id` is not the one of the agent's last registration: CR-5, accepted 2026-11-20) | Registers again at once. Does not touch its launch. |
| Heartbeat answer names a `release` different from the agent's | Registers again; the answer to that decides (`agent_too_old`, `potluck_too_old`). |

**Lost node.** Potluck marks a node lost after 10 s without a heartbeat. Section 11 says what follows.

**Silent node with a launch (CR-6, accepted 2026-11-21).** A node that runs a launch Potluck holds is not lost after 10 s. It is lost after `lease_ttl_s` without a heartbeat (section 6.2; 20 s): only then has the agent ended the launch by itself. This is one number, not two: Potluck takes the time from the lease TTL it sends with the launch, so the two cannot drift apart. From 10 s on the node takes no new load (`unplaced: "silent"`). Reason: an agent restart keeps its launch (section 6.1) and can take longer than 10 s; with the old rule Potluck gave the load up at 10 s and answered `stop_launch` to the agent that had just re-adopted it. A node without a launch is lost after 10 s as before.

## 5. Programs and versions

### 5.1 One release

A release is one string, for example `0.1.0`. Potluck, the agent and the program set each carry it. Potluck serves exactly one release at a time.

- Agent and Potluck must have the same release. Lower: `agent_too_old`. Higher: `potluck_too_old`. Neither gets a load.
- An agent with the right release but without all programs verified reports `programs_ready: false` and gets no load.
- **Same build on both ends of a load.** The llama RPC protocol needs host and workers from the same fork commit. In phase 4 the host is Potluck's own `llama-server`. Potluck checks at start that its own host program was built from the commit named in `release.json` (`builds.llama`), and refuses to serve the release, and so to start any load, when it was not.
- Each program also has a `build` (the fork commit for llama programs, the upstream version for exporters), for display and for benchmark records.

### 5.2 Folder layout (what task B4 delivers)

Potluck serves the folder `programs/` from a read-only volume. One sub-folder per release:

```
programs/
  0.1.0/
    release.json
    agent/
      potluck-agent-linux-amd64
      potluck-starter.sh
      SHA256SUMS
    linux-amd64-vulkan/
      llama-server
      rpc-server
      node_exporter
      SHA256SUMS
    linux-amd64-cuda/
      llama-server
      rpc-server
      node_exporter
      nvidia_gpu_exporter
      SHA256SUMS
```

- Platform name: `<os>-<arch>-<backend>`. Later: `windows-amd64-cuda`, with `.exe` file names.
- File names are exactly the program names above, with no version in the name.
- `SHA256SUMS` is the output of `sha256sum` run inside that folder: one line per file, `<hex>  <name>`.
- `release.json`:

```json
{
  "release": "0.1.0",
  "builds": {
    "llama": "a01665d30de2885879a64eae999067ef3ef5fd4b",
    "node_exporter": "1.13.1",
    "nvidia_gpu_exporter": "2.0.0"
  },
  "platforms": {
    "linux-amd64-vulkan": {"os": "linux", "arch": "amd64", "backend": "vulkan"},
    "linux-amd64-cuda": {"os": "linux", "arch": "amd64", "backend": "cuda"}
  }
}
```

- Requirements on the llama programs: `llama-server` and `rpc-server` in every platform folder come from the same fork commit, and are single files with no shared libraries of their own (`BUILD_SHARED_LIBS=OFF`). They may depend on what the machine already has: the C library, the GNU OpenMP runtime (`libgomp.so.1`), the Vulkan loader, and for `cuda` the Nvidia CUDA runtime libraries of the toolkit version the programs were built with (13.3 for release 0.1.0). The agent checks for these before it reports `programs_ready`, and names a missing one in `programs_error`; the setup script and the Docker node image (`nvidia/cuda:13.3.1-runtime-ubuntu24.04` plus `libgomp1`) provide them.
- Files at the release root that no `SHA256SUMS` names (licence texts, notes) are ignored by Potluck.
- The `agent/` folder is filled by the scheduler side, the platform folders by the fleet console agent (B4) until the builds are automated (B2).
- Potluck checks every file against `SHA256SUMS` when it starts and refuses to serve a release with a mismatch or a missing file.

Platform choice, done by Potluck from the registration: `cuda` in `backends` and an Nvidia GPU present gives `…-cuda`; otherwise `vulkan` in `backends` gives `…-vulkan`; otherwise `unsupported_hardware`.

### 5.3 `GET /api/pool/programs/{release}/{platform}/{file}` (agent from Potluck)

Plain file download with `Content-Length`. The agent:

1. at start, deletes every `*.part` file in its program folder (nothing is resumed) and re-hashes the files it holds;
2. fetches only while its role is `idle`;
3. checks free space first (`/dev/shm` on a RAM-only node) and reports `programs_error` when it is short;
4. downloads to `<name>.part`, checks the sha256 given at registration, renames into place, sets the executable bit;
5. starts the exporters;
6. reads `devices` with the llama program;
7. reports `programs_ready: true`.

Program folder: `/dev/shm/potluck/programs/<release>/` when `ram_only`, otherwise `/var/lib/potluck-agent/programs/<release>/` (bare) or the same path inside the container (Docker). An old release folder is deleted only after the new one is verified and when no launch exists. For that short time both releases are in RAM on a RAM-only node.

A failed check is retried three times, then reported in `programs_error`. The agent never runs a file whose check failed.

### 5.4 How a node gets and updates the agent

`GET /api/pool/release` (no parameters) answers:

```json
{"release": "0.1.0",
 "agents": {"linux-amd64": {"path": "/api/pool/programs/0.1.0/agent/potluck-agent-linux-amd64", "sha256": "…"}}}
```

| Install | First agent | On `agent_too_old` |
|---|---|---|
| Bare metal with a drive | The setup script (section 9). | With no launch: download the new binary, check it, replace itself, restart. |
| RAM-only (PS5) | The starter, at every boot. | With no launch: the same as the starter would do (download, check, restart of its unit). |
| Docker | The image, whose tag is the release. | The agent exits with code 75. A container cannot durably replace its own binary; the setup script (or a pull of the new tag) replaces the image. Until then the node is shown as `agent_too_old`. |

With a launch, an agent waits for the launch to end before it updates.

**The starter.** A shell script written by the scheduler side and shipped unchanged in the datacenter's payload, together with the Potluck address. So the boot chain stays: bare image, pull from the datacenter (starter and address), register with Potluck. A release of Potluck needs no payload change. The starter:

1. needs only a POSIX shell, `curl` and `sha256sum`;
2. does nothing and exits 0 when the marker file `/etc/potluck/disabled` exists (section 10);
3. does nothing and exits 0 when `potluck-agent.service` is active. It never stops a running agent or launch, so it is safe on every payload apply;
4. otherwise asks `GET /api/pool/release`, downloads the agent for its platform into `/dev/shm/potluck/`, checks the sha256, and starts it as the systemd transient unit `potluck-agent.service` with the RAM-only options;
5. retries forever with a growing wait (5 s up to 60 s) while Potluck is unreachable or a check fails.

**What the payload side guarantees** (PS5 side): a payload apply never restarts `potluck-agent.service`, never touches `/dev/shm/potluck` or `/run/potluck`, and never stops a `potluck-launch-*` unit, unless the starter itself changed.

### 5.5 Planned for phase 7, not part of release 0.1.0: llama releases that update by themselves (CR-1)

Requested by the owner on 2026-11-20. Nothing in release 0.1.0 is held to this section.

- **Two version numbers.** The Potluck release covers the scheduler, the agent, the starter and the exporters, and stays an equality check at registration. The llama release is a separate set of `llama-server` and `rpc-server` builds that moves on its own schedule.
- **Rule for a load.** Every member of a load, the host included, runs the same llama release. The launch request names it and the agent refuses one it does not hold.
- **Where releases come from.** The fork's automated build publishes a release with a manifest: release tag, commit, RPC protocol version, the minimum contract version it needs, and per platform the operating system, architecture, backend, what the programs need from the machine, and the files with sha256 and size. The repository address is a Potluck setting that defaults to the fork. Trust is TLS plus the manifest's checksums.
- **Potluck's own host program** is fetched from the staged release into Potluck's data volume, not built into its image. Potluck refuses a release that its own image, or a node's reported runtime, cannot run.
- **Rollout.** Stage: every idle node downloads and verifies the new release next to the current one, after a free-memory check on a RAM-only node. Trial: when every node holds it, the next load uses it; ready plus one served request promotes it. A start failure, a crash or a load timeout on the trial rolls back automatically, marks the release bad, and retries the request on the old release. After promotion the nodes delete the old set; Potluck keeps it for a manual one-step rollback.
- **Settings.** Automatic update on (default), off, or pinned to one release.

The manifest format is drafted by the scheduler side when this work starts.

## 6. Launches

### 6.1 How the agent runs a program

| | Bare-metal install | Docker install |
|---|---|---|
| Launch process | A systemd transient unit named `potluck-launch-<launch_id>`, independent of the agent's own unit. The prefix `potluck-launch-` is part of this contract: a local service may watch for an active unit with that prefix to know a launch is running (the PS5 GPU clock service does). | A child process of the agent inside the agent's container. The container has its own PID namespace and a reaping init (`--init`, never `--pid=host`), so a child cannot outlive it. |
| Exporters | systemd transient units `potluck-node-exporter`, `potluck-nvidia-exporter`. | Child processes of the agent. |
| Agent restart | The launch keeps running. The restarted agent re-hashes its programs, finds the `potluck-launch-*` unit and the existing share mount again, and continues the lease from its file (section 6.4). | The launch dies with the container. Potluck sees a new `agent_start_id` with no launch and fails the instance with reason `agent_restarted`. |
| Launch log | `/dev/shm/potluck/launch/<launch_id>.log` when `ram_only`, otherwise under `/var/lib/potluck-agent/launch/`. Capped at 8 MiB: when full, the oldest half is dropped. Kept for the last launch only. | Same path inside the container. |
| GPU access | The machine's drivers. For Nvidia this needs the CUDA runtime libraries installed on the machine: supported when they are present, not tested yet. Nvidia nodes use the Docker install for now. | Nvidia: the container is started with the GPU passed in (`--gpus all`); the image carries the CUDA runtime libraries; the programs are still handed out by Potluck. Other vendors, later: `--gpus` is Nvidia only, so they need `/dev/dri` passed in and a Vulkan driver in the image. |
| Network | The machine's. | Host networking, so the ports of section 3 are the machine's own. |
| Model files | Section 8. | Section 8. |

Nvidia launches are started with `CUDA_DEVICE_ORDER=PCI_BUS_ID`, so llama's `CUDA<n>` is the card with `nvidia-smi` index `n`.

Stopping a launch is always: SIGTERM, wait up to 10 s, SIGKILL. A forced stop skips the wait.

### 6.2 `POST /launch` (Potluck to agent): start

```json
{
  "launch_id": "ln-1795080000000-7",
  "role": "worker",
  "release": "0.1.0",
  "label": "Devstral-Small-2-24B-Instruct-2512-Q4_K_M.gguf",
  "lease_ttl_s": 20,
  "min_mem_available_mib": 9000,
  "worker": {
    "model_dir": true,
    "extra_args": []
  }
}
```

`role: "worker"` (phase 4). The agent starts `rpc-server` on port 50052, bound as in section 3. `model_dir: true` adds `--model-dir <the node's model folder>` so the worker can read model files itself (the direct-pull build); it is refused on a node whose model source is `push`. `label` is for display on the node's own screen only.

`role: "host"` (phase 6) replaces the `worker` object with:

```json
"host": {
  "model": "gemma-4-12b-it-Q4_K_M.gguf",
  "ctx": 8192,
  "parallel": 1,
  "workers": ["192.0.2.122:50052"],
  "tensor_split": [50, 50],
  "strict_file_load": true,
  "extra_args": []
}
```

- `ctx` is the context per slot. The agent passes `ctx * parallel` to `llama-server -c`.
- `model` is a file name relative to the node's model folder. The agent refuses a name that leaves that folder.
- `tensor_split` is required when `workers` is not empty, one entry per member, host first.
- `extra_args` are appended last. The agent refuses arguments it sets itself.
- The agent never starts `llama-server` with fewer workers than the request names.

Checks before anything starts. Each failure is a refusal with a `reason` word and a `message`:

| HTTP | `reason` | Check |
|---|---|---|
| `409` | `busy` | No launch is running or stopping. The body carries the existing `launch_id`. An exited launch does not count. |
| `409` | `paused` | The last heartbeat answer did not say `paused`. This closes the race between a pause and a load already on its way. |
| `409` | `release_mismatch` | `release` equals the agent's and `programs_ready` is true. |
| `503` | `model_source_unreachable` | For a host, or a worker with `model_dir`: the last share probe succeeded (section 8). |
| `404` | `model_not_found` | Host only: the model file exists and is readable. |
| `507` | `low_memory` | `MemAvailable` is at least `min_mem_available_mib`. |
| `502` | `worker_unreachable` | Host only: every worker accepts a TCP connection within 5 s. The agent closes each connection at once and sends no bytes (see below). The body lists the ones that failed. |
| `409` | `port_in_use` | The launch port is free. A bind failure is retried for 3 s before it is reported, because the previous launch's port can take a moment to free. |

Answer on success: `202` with the state object of 6.3. The start is asynchronous.

**Probing a worker.** `rpc-server` serves one client at a time. Any probe of a worker, by an agent or by Potluck, is a TCP connect that is closed at once with no bytes sent. A probe never holds the connection and is never counted as `client_connected`.

### 6.3 `GET /launch` (Potluck to agent): state

```json
{
  "launch_id": "ln-1795080000000-7",
  "role": "worker",
  "state": "ready",
  "state_age_s": 12.4,
  "port": 50052,
  "command": ["rpc-server", "…"],
  "build": "a01665d30",
  "uptime_s": 12.2,
  "client_connected": true,
  "load": {
    "fraction": null,
    "bytes_total": 4470000000,
    "bytes_loaded": 3100000000,
    "state": "receiving"
  },
  "mem_available_mib": 9120,
  "mem_low_water_mib": 9004,
  "exit": null,
  "lease_expires_in_s": 14
}
```

- `state`: `none`, `starting`, `ready`, `stopping`, `exited`.
  - Worker: `ready` when `rpc-server` has logged that it is listening and its port accepts a connect-and-close probe.
  - Host: `starting` covers the model load; `ready` when `llama-server`'s `/health` answers ok.
  - `stopping`: a stop was requested and the process is not yet gone. A process blocked on a dead share can stay here until SIGKILL takes effect. The node is busy while it lasts.
- `client_connected` (worker): whether a client holds the worker's connection, read from the worker's socket table, not by connecting.
- `load`: progress of this node's part of the load. `state` is `waiting`, `receiving` or `done`, the words Potluck uses today.
  - Host: `fraction` from the fork's `load_progress: fraction=N.NN` log line.
  - Worker reading from the share: `bytes_loaded` and `bytes_total` from the fork's `[file_load] progress bytes=N total=M` log line.
  - Worker receiving pushed weights: the agent has no count. `load` is `null` and Potluck keeps using the byte count it has on the sending side.
  - After the load, `load` stays with its final values.
- `exit`, when `state` is `exited`: `{"code": 137, "signal": "KILL", "reason": "…", "age_s": 3.1, "log_tail": ["…"]}`. `log_tail` is the last 40 log lines.
- The last `exited` state stays readable until the next launch starts. With no launch ever: `{"state": "none"}`.

Exit reasons, in the order the agent decides them. An earlier line wins over a later one, so a token in the log always wins over what the exit signal suggests:

| Order | `reason` | Decided from |
|---|---|---|
| 1 | `stopped`, `forced`, `lease_expired`, `watchdog`, `reset` | The agent sent the signal itself, for that cause. |
| 2 | `file_load_timeout` | The log holds `rpc_file_load_timeout: endpoint=… command=… timeout_s=N` (the fork aborts with exit 139). Host log only. |
| 3 | `file_load_required` | The log holds `rpc_file_load_required: endpoint=… reason=no_capability\|refused\|identity_mismatch` (exit 1). Host log only. The `reason=` word is passed on in `message`. |
| 4 | `start_failed` | The process exited before it was ready and before any load began, for example an invalid argument. |
| 5 | `oom_kill` | Killed by a signal the agent did not send, and the kernel log names the process. |
| 6 | `crashed` | Anything else. |

The two token strings are final (plan task B3). In phase 4 the host is Potluck's own child, so Potluck matches the same two tokens in its child's log; a worker-side agent never sees them.

Potluck reads `GET /launch` four times a second while a load is in progress on that node and once a second during the rest of a launch. An idle node is not polled.

### 6.4 `POST /launch/{launch_id}/lease` (Potluck to agent): renew

Every 5 s. Answer `204`, or `404` when the id is not the current launch.

- At every renewal the agent writes the lease deadline, as a `CLOCK_BOOTTIME` value, to `/run/potluck/launch/<launch_id>.lease`. A restarted agent continues from that file; a restart never grants a fresh lease. So an agent in a restart loop with Potluck gone still frees the node.
- When the deadline passes, the agent stops the launch with reason `lease_expired`.
- A network break shorter than `lease_ttl_s` is invisible. A longer one ends every member of the load with `lease_expired`, and Potluck fails the instance.

### 6.5 `DELETE /launch/{launch_id}` (Potluck to agent): stop

`?force=1` skips the 10 s wait. Idempotent for an already exited launch (`200` with its state). An id that is not the current launch answers `409` and stops nothing.

The agent reports `exited` only after the process is confirmed gone and its port is closed. Until then the state is `stopping`.

### 6.6 `GET /launch/log?since=<byte offset>` (Potluck to agent)

The launch's combined output from the offset, with the next offset in the header `X-Next-Offset`. Offsets count from the start of the launch, also after the log file has dropped its oldest half; a request for bytes already dropped gets what remains and the header `X-Dropped-Bytes`. Potluck copies the log into its own files as the launch runs, so nothing has to survive on the node.

### 6.7 Memory watchdog

While a launch exists, the agent reads `MemAvailable` at least four times a second and kills the launch with reason `watchdog` when it falls below a floor. Default floor 1000 MiB, an install option. This exists because a PS5 that runs out of memory freezes within seconds instead of killing the process.

### 6.8 `POST /reset` (Potluck to agent)

Kills every launch unit or child the agent started, whatever the launch id, and also any process whose executable lies in the agent's program folder and is not an exporter. Answers with what it killed. It is the last step of "force unload" (section 7.2). Exit reason of a launch ended this way: `reset`.

### 6.9 `GET /status` (local programs to agent)

Answered on `127.0.0.1:50060` only, read-only, for the machine's own screen. No other request is accepted from `127.0.0.1`.

```json
{
  "registered": true,
  "potluck": "192.0.2.1:3122",
  "release": "0.1.0",
  "paused": false,
  "role": "worker",
  "model": "Devstral-Small-2-24B-Instruct-2512-Q4_K_M.gguf",
  "state": "ready",
  "load": {"fraction": null, "bytes_total": 4470000000, "bytes_loaded": 4470000000, "state": "done"},
  "last_launch": null,
  "last_error": null,
  "last_error_age_s": null
}
```

`model` is the host's model file name, or the `label` of a worker launch. `last_error` is the reason and message of the last refused registration, failed program check or abnormal exit.

CR-6, accepted 2026-11-21:

- `role` and `model` describe a launch that exists (`starting`, `ready`, `stopping`). Once the launch has exited, `role` is `idle`, `model` is `null`, and the launch is named in `last_launch`: `{"id": "ln-...", "model": "<file name or label>", "reason": "<exit reason>", "ended_age_s": 12.5}`. `last_launch` is `null` in every other state.
- Right after an agent restart the answer waits for the re-adoption of section 6.11 (normally well under a second). When that takes longer than 2 s the answer is `503` with reason `agent_starting`, as on the node's address. A launch that runs is never shown as `idle`.
- `last_error` of an abnormal exit is kept across an agent restart (it is read from the launch's exit record, with its real age). Any other `last_error` is not stored and is gone after a restart.

### 6.10 Details fixed while building the agent (CR-3)

Accepted by both sides on 2026-11-20. They are part of the contract.

- **Malformed launch request**: a model name that leaves the model folder, `extra_args` the agent sets itself, a missing or wrong `tensor_split`, `model_dir` on a `push` node, a bad `launch_id` or `role`. Answer `400`, reason `invalid_request`.
- **Wrong launch id**: `DELETE /launch/{id}` answers `409` with reason `not_current`. A lease renewal for a wrong id, or for a launch that has exited, answers `404` with the same word.
- **Body fields**: a `worker_unreachable` refusal lists the workers in `failed_workers`. `POST /reset` answers `200` with `{"killed": [{"pid", "exe", "unit", "launch_id"}]}`.
- **Watchdog**: it kills with SIGKILL at once, with no SIGTERM and no 10 s wait.
- **Model source**: `--model-source dir:<path>` is reported as kind `share` with the path as target. A model folder with no file in it counts as unreachable, which also catches an empty mount point.
- **Programs**: after three failed downloads the error is reported and the whole fetch is tried again every 60 s.
- **Stopping**: the wait for the launch port to close is at most 10 s, so a foreign listener cannot hold a launch in `stopping` for ever.
- **No GPU access**: when `llama-server --list-devices` prints `(none)`, the node reports "the agent must run as root or in the render and video groups" in `programs_error`, stays not ready, and reads the list again every 60 s. The listing is always taken from `llama-server`; `rpc-server` has no such option.
- **Strict file load** (host launches, phase 6): `strict_file_load: true` sets the environment variable `LLAMA_RPC_FILE_LOAD=required` on the host program; false sets `LLAMA_RPC_FILE_LOAD=1`. Value `0` means the host never asks workers to read files and pushes the weights.
- **Starter settings**: the starter takes `POTLUCK_URL` (`http://<IPv4>:<port>`), and optionally `POTLUCK_MODEL_SOURCE` and `POTLUCK_WATCHDOG_FLOOR_MIB`, from its environment or from `/etc/potluck/starter.env`. Missing or malformed settings make it wait and read them again.

### 6.11 Details fixed after the agent code review

Found by the code review before the first run on a real machine. Accepted by both sides on 2026-11-21.

- **Agent start order**: a launch left by an earlier agent process is re-adopted first. Its lease and its memory watchdog run before the agent has an identity, a free port, a registration or a reachable Potluck. Exporter units of the earlier agent process (`potluck-node-exporter`, `potluck-nvidia-exporter`) are stopped before the port check of section 3. When such a unit cannot be stopped and still holds its port, the agent goes on and registers all the same: it never stays out of the pool over its own leftovers. It reads that exporter until it can replace it. A port held by anything else still blocks registration.
- **`503 agent_starting`**: until the re-adoption is done, every call on port 50060 except `POST /reset` answers `503` with reason `agent_starting` and `Retry-After: 1`. The agent never answers `GET /launch` with `none` for a launch it has not looked for yet, and it does not register before the re-adoption. Potluck treats this answer as "state not known yet", not as a lost launch.
- **Port 50060 taken**: the agent does not exit. It tries to bind again after 5 s, growing to 60 s, and does not register until both addresses are bound.
- **Kill**: an ordinary stop or a lease expiry is SIGTERM with `systemctl kill`, the 10 s wait, then the kill; a forced stop and the watchdog start with the kill at once. The kill is repeated until the unit is gone and is never given up. Its attempts escalate: first SIGKILL with `systemctl kill`, second `systemctl stop` of the unit, third SIGKILL sent directly to the processes whose cgroup is the launch's own `potluck-launch-<launch_id>` unit, and from the fourth on both SIGKILLs together, every time. The direct signal goes to no other process: the cgroup of each process is read again right before the signal, with the process pinned by a pidfd where the kernel has one (Linux 5.3), so a process id that was reused in between is not hit. The next attempt comes 1 s after one that was delivered and 250 ms after one that failed, so there are at most about four a second. Every attempt writes one line to the agent's log: its number, its method, its error. A restarted agent kills a `stopping` launch again; it does not trust a kill sent before the restart.
- **Start**: starting the unit is not tied to the `POST /launch` request (30 s limit of its own). When the start fails, the unit name is stopped before the launch is reported `start_failed`.
- **Launch unit**: it runs with `OOMScoreAdjust=900`, so the kernel's OOM killer takes the launch before the agent or sshd, and with the agent's `LD_LIBRARY_PATH`, the one the library check used.
- **Update (5.4)**: before a downloaded agent replaces the running one, it is asked for its release (`potluck-agent version`); a binary that reports another release is not installed. One release and sha256 is tried once: the attempt is kept in `<state-dir>/update-attempt`, so an update that changed nothing is not downloaded and started again every 60 s. A download that fails or has a wrong sha256 is tried again. The binary is never replaced while a launch exists.

## 7. Stats and controls

### 7.1 Stats

The agent scrapes its local exporters and puts the result in the heartbeat's `stats` object. Metric names, units, scrape rules, exporter versions and exporter arguments are defined in the stats contract (`docs/stats-contract.md`, plan task E1, written by the fleet console agent; copied from its draft 3, commit `07b7ac5` of the datacenter manager repo). This document defines nothing about stats beyond:

- the exporters listen on `127.0.0.1` only, the agent is the only network reader, and Potluck is the only place other systems read stats from;
- an exporter that is missing or keeps crashing is reported as absent in `stats` and shown as a warning on the node. It never stops the node from taking loads;
- nothing the agent needs for a launch comes from an exporter: `MemAvailable` is read from `/proc/meminfo` and device memory from llama.

### 7.2 Pause and force unload

Both are state and actions of Potluck, reachable from its web UI, its REST API and its MCP tools, and written to the audit log with a comment.

- **Pause** a node: Potluck places no new load on it. A load already using it continues. When that load ends, the node is shown as paused and idle. Unpause returns it to the pool. Pause is stored in Potluck's database and survives restarts of Potluck and of the agent. The heartbeat answer carries `paused`; the agent shows it on `GET /status` and refuses a `POST /launch` while it holds (section 6.2).
- **Force unload** a node: Potluck stops every model instance that has a launch on that node, because a split model cannot lose one member. On that node the stop is forced (`DELETE …?force=1`); if the agent does not report `exited` within 5 s Potluck calls `POST /reset`. The other nodes of the same model get a normal stop. Potluck shows the node idle when its agent reports no launch. A node that is lost (no heartbeat) is treated as holding nothing: force unload fails the instance, stops the other members and does not wait for the lost node.

REST shape on Potluck (proposal): `POST /api/nodes/{id}/pause`, `POST /api/nodes/{id}/unpause`, `POST /api/nodes/{id}/force-unload`, each with a required `comment`.

## 8. Where model files come from

A node has exactly one model source, set at install and reported at registration.

| `kind` | Meaning | Who |
|---|---|---|
| `share` | The node reads model files itself from a folder that holds the same files as the share. | PS5s and the Nvidia laptop (NFS mount of the share), the host machine (the model folder itself). |
| `push` | The node cannot read model files. As a worker it receives its weights from the host over the worker connection. It cannot be a host. | Windows, later. |

**Bare-metal install with `share`.** The agent mounts the share itself, read-only, at `/run/potluck/models`. It does not use `fstab`, so a missing share cannot block boot. The exact command, with `clientaddr` computed at mount time from the address that reaches Potluck:

```
mount -t nfs4 -o ro,hard,vers=4.2,addr=<potluck ip>,clientaddr=<node ip>,rsize=1048576 <potluck ip>:/ /run/potluck/models
```

- The options are explicit because these machines have no `mount.nfs` helper.
- `hard`: a read that waits on a dead share still honours SIGKILL, and every stop path of the agent ends in SIGKILL.
- At start the agent looks for an existing mount at that path and uses it. It never mounts twice.
- It never unmounts or remounts while a launch exists, and never uses a lazy unmount. A remount drops the page cache, which turns a 4.5 s warm reload into a 45 s cold one, so the agent remounts only after five failed probes in a row with no launch.

**Docker install with `share`.** The machine mounts the share, not the container. The setup script writes a systemd mount unit on the machine with the same options plus `nofail`, and the container gets that folder bind-mounted read-only at `/models`. The container needs no mount capability. On the host machine the folder is the model directory itself. The agent only probes the folder.

**The share probe.** A `stat` on a hard mount can block forever when the server is down, and can be answered from the attribute cache while it is down. So the probe is a separate short-lived process that reads the first 4 KiB of one model file with `O_DIRECT`, with a 2 s timeout, every 5 s. At most one probe process is outstanding; while one hangs the result is "unreachable". `model_source.reachable` is the result of the last finished probe.

**When the share is unreachable.** It is reported in the heartbeat. Potluck shows the node as "model source unreachable", gives it no load that needs it, and keeps a waiting request queued, trying again every 5 s until the load timeout.

**Mixed loads.** Today the requirement "workers must read from the share" is one switch for a whole load. A load that mixes `share` and `push` workers needs it per worker (plan task B5). Until then Potluck refuses such a mix with a plain reason.

## 9. Install

One setup script, `potluck-agent-setup`, with two paths. Common options: `--potluck <addr:port>`, `--model-source share:<target>|dir:<path>|push`, `--watchdog-floor-mib <n>`, `--allow-from <address or network>`.

- **Bare metal** (`--bare`): places the agent binary, writes one systemd unit `potluck-agent.service` that starts at boot, starts it. A RAM-only node does not use the setup script; it uses the starter (section 5.4), which passes `--ram-only`.
- **Docker** (`--docker`): starts one container `potluck-agent` from the image tagged with the release, with restart policy `unless-stopped`, `--init`, its own PID namespace, host networking, the GPU passed in when one exists, and the model folder mounted read-only.
- `--allow-from` adds source addresses the agent accepts requests from. It is needed on the machine that runs Potluck itself: Potluck runs in a container there, so its requests to the local agent arrive from Potluck's container network (`172.30.0.0/24` in the compose file) and not from the configured address. No other node uses it. That requests to the PS5s arrive from `192.0.2.1` is checked in the first phase 4 test.

After either path the node appears in Potluck with programs fetched and stats flowing, with no other step.

## 10. Leaving the pool

There are two different things, and they have different effects.

**Uninstall, from the node** (`potluck-agent-uninstall`). The script does the work; the agent cannot delete itself and finish.

1. Best effort: `POST /api/pool/nodes/{id}/deregister` on Potluck. If Potluck is unreachable the script goes on.
2. On a node with a starter: write the marker file `/etc/potluck/disabled` (one empty file; it must survive reboots, so it cannot be under `/run`). The starter does nothing while it exists.
3. Stop the launch, the exporters and the agent; unmount the share if the agent mounted it; delete the program folder, the launch log, the state files, the unit or container and image, and the agent binary.
4. Nothing of Potluck's keeps running on the machine.

On deregister, Potluck stops any instance with a launch on that node, as in force unload, and marks the node `left`. A `left` node that registers again is admitted like a new one, with the same `id`. Installing again removes the marker.

**Removal, from Potluck** (`DELETE /api/nodes/{id}`, audited, with a comment), for a machine that is gone or unwanted. Potluck stops any instance on it and marks the node `removed`. A `removed` node that registers is refused (`removed`) until someone re-admits it in Potluck (`POST /api/nodes/{id}/readmit`, audited).

In both cases Potluck keeps: history, benchmark runs and learned memory needs that name the node, and the audit entry. Potluck forgets: the node's pause state, its programs state and its place in the pool. A profile that forces such a node stays in the configuration and is refused at load with the reason "forced node is not in the pool".

### 6.12 Details fixed after the first run on a real machine (CR-6, accepted 2026-11-21)

CR-6, accepted 2026-11-21. They are part of the contract.

- **Exporters across a re-registration.** Exporter units left by an earlier agent process are stopped once, at process start (section 6.11). A later registration of the same agent process (heartbeat answered `410`, or another release named) does not stop the exporters: one whose entry in the answer's `exporters` is unchanged keeps running, one that is missing or changed is started. Before, the re-registration stopped the running exporter and did not start it again, so the stats went stale until the agent restarted.
- **Exporter found not running.** When an exporter the agent should run is not running and its scrape fails, the agent starts it again, at most once every 30 s per exporter, with one log line.
- **No failed unit records.** Launch units are started with `systemd-run --collect`, so a killed launch leaves no failed unit in systemd. The exit status still comes from the `ExecStopPost` file. Exporter units have no exit file; their status is read from the unit, then `systemctl reset-failed` clears it.
- **Stats warning.** A failed scrape is not warned about during the first 5 s after its exporter was started.
- **Kill ladder (addition to 6.11).** Before every kill attempt after the first, the agent looks for the unit again; when it is gone, no attempt is made. `systemctl` answering that the unit is not loaded or not found counts as done, not as a failed attempt. A stop request that arrives while a kill is under way (Potluck's ordinary stop after the watchdog's kill) does not start the next attempt early: it comes 1 s after a delivered one, as section 6.11 says.

## 11. What Potluck does with all this

- **Membership** is registrations and heartbeats only, from the first agent release (phase 4, plan task D1). In that release every pool machine has an agent: the five PS5s, the host machine and the Nvidia laptop. Potluck no longer reads the datacenter's fleet feed.
- **Placement memory.** Potluck places on the node's llama device figures. For a device with `shares_system_memory`, the usable figure is the smaller of the device's free memory and `mem_available_mib` minus the watchdog floor, so programs and logs held in RAM are accounted for without a special case.
- **Phase 4 loads.** Potluck's own `llama-server` child is the host, unchanged. For each worker node of a load Potluck sends `POST /launch` with `role: worker`, waits for `ready`, then starts its child with those workers. On unload it stops the worker launches after its child has exited.
- **Phase 6 loads.** Potluck sends the worker launches first, then one `role: host` launch, and forwards requests to the host node's port 50061. The scheduler-child host is removed only after an agent-hosted load on the host machine matches it.
- **Exit handling.** An `exited` launch at any time fails the instance with the agent's reason, and Potluck stops the other members. `watchdog` and `oom_kill` raise the learned memory need of that model. `start_failed` is recorded with its log lines and not retried.
- **Lost node with a launch.** A node that loses power cannot report or expire anything. When a node with a launch is marked lost (after 20 s without a heartbeat, section 4.3: CR-6, accepted 2026-11-21), Potluck fails the instance at once and stops the other members. It never tries to restart or recover the node. When the node's agent returns and reports a launch Potluck no longer holds, Potluck tells it to stop it (section 4.2).
- **Agent restart with a launch (CR-6, accepted 2026-11-21).** The launch keeps running (section 6.1). While the agent is away the node is silent, not lost, for up to 20 s. When the agent registers again within that time with the same `launch_id`, Potluck keeps the instance and sends no `stop_launch`.
- **Potluck restart.** Potluck holds no launches after a restart. Each agent's next heartbeat gets `410`, the agent registers with its `launch_id`, and Potluck answers `stop_launch`. Launches whose agent cannot reach Potluck end by their lease.
- **One feed** (plan task D4) for other systems is built from the heartbeats and Potluck's own instance state. It is a separate page of this repository, not part of this contract.

### 11.1 Details fixed while building the scheduler side (CR-5)

CR-5, accepted 2026-11-20. They are part of the contract. Each line says what Potluck does.

- **Register refusal `release_unavailable`** and **the order of the register checks**: section 4.2.
- **Heartbeat with a changed `agent_start_id`**: answered `410`, as for an agent Potluck does not know. The agent registers again, and the registration decides about its launch (`stop_launch`, or `agent_restarted` for a launch Potluck holds that the agent no longer reports).
- **Tiers.** A node gets its tier settings (priority, headroom, link speed, settle time) from the first tier of Potluck's configuration that matches it. For an agent node a tier can match on what the agent reports, so it does not depend on the hostname, which a console changes at each flash:
  - `platform`: exact, the platform Potluck chose at registration, for example `linux-amd64-vulkan`;
  - `device`: a case-insensitive glob (the syntax of the hostname matcher) that one of the node's device names must match. The device names are the `gpus[].model` of the registration and, once a heartbeat has arrived, its `devices[].name`.

  A node matches a tier when every matcher the tier sets holds. The older matchers still apply to an agent node when a tier sets them: `hostname` against the node's `name`, `source` against the word `agent`, `role` against the empty string, `local` against false. A node from the fleet feed reports no platform and no device, so a tier that sets one of them never matches it.
- **A node that matches no tier** is never silent. It stays registered, is listed in `GET /api/status` (in `view.nodes` and in `agents`) with `unplaced: "no_tier_match"`, is not eligible, is not counted in capacity and gets no load. One log line is written per registration, not per heartbeat.
- **A node that cannot take a load yet** is listed with the reason and gets no load while the reason holds; a request that needs it stays queued and the profile is not backed off:
  - `programs_not_ready`: the last heartbeat said `programs_ready: false`, or no heartbeat has arrived since the registration;
  - `model_source_unreachable`: worker launches ask for `model_dir` and the last heartbeat did not say `model_source.reachable: true`. When worker launches do not ask for `model_dir`, the model source is not looked at;
  - `orphan_launch`: next line;
  - `silent` (CR-6, accepted 2026-11-21): no heartbeat for 10 s from a node that runs a launch Potluck holds. It ends with the next heartbeat or registration, or with the node being lost at 20 s.
- **A heartbeat that names a launch Potluck does not hold** (the node was lost, its load was given up, and the node came back; or a start request got no answer). Potluck sends `DELETE /launch/{launch_id}` (section 6.5, not forced) once for that launch, and again only if the request could not be delivered. The node gets no load until a heartbeat reports no launch. This is in addition to `stop_launch` at registration.
- **The host machine's agent.** In phase 4 the local node comes from Potluck's own probe. An agent whose `id` or `name` equals Potluck's `fleet.local_node` setting (ignoring case) is the host machine's. It stays registered and is shown in status (`host_agent: true`), but is not published as a remote node and gets no worker launch.
- **One device per node.** A worker serves one device. Potluck uses the first device of the heartbeat's `devices` whose `backend` is the backend of the node's platform; when no device has that backend, the first device listed. Further devices of that backend are not used: the node's status counts them in `extra_devices_ignored` and the log warns once per registration.
- **Lost node with a launch.** The instance goes through Potluck's node-loss path at once (the time without a heartbeat is the wait: 20 s for a node with a launch, the lease TTL, instead of 10 s: CR-6, accepted 2026-11-21): it is killed and its other members are stopped. Requests that were waiting for an instance that was still loading are queued again and get no error; requests in flight on a ready instance are cut off. The profile is not backed off. **Force unload** differs: requests waiting for that instance fail at once with an error that names the force unload and the node.
- **Reading the launches when the host program ends (CR-6, accepted 2026-11-21).** Potluck's host program aborts within milliseconds of losing a worker, so Potluck often sees its exit before the worker's. When the host program of a load with agent workers ends without having been stopped, Potluck reads `GET /launch` on every member in parallel, again every 100 ms, for at most 1 s, before it handles the exit. A launch that ended by itself (`watchdog`, `oom_kill`, `lease_expired`, `start_failed`, `crashed` and the like; not `stopped`, `forced`, `reset`) is the failure reason, with its message, in the 502, the log and history (`workerExited`), and the memory-kill rule below applies. When no launch ended by itself, the host program's exit is the reason, as before.
- **`LLAMA_RPC_FILE_LOAD` on Potluck's host program (CR-6, accepted 2026-11-21).** For a load with agent workers Potluck sets it in the environment of the host `llama-server` it starts: `required` when `worker_model_dir` is on (the workers were launched with `model_dir` and must read the file themselves), `0` when it is off (the host sends the weights). It no longer passes on whatever its own environment holds. Profiles carry no environment of their own.
- **`min_mem_available_mib`** of a worker launch is Potluck's watchdog floor setting (`watchdog_floor_mib`, default 1000). For a node whose memory is shared (its device has `shares_system_memory`, or its tier says so) the node's budget for the load is added.
- **No forced-fit pre-flight with agents.** Potluck's check of a forced placement against freshly probed worker memory is not run: a probe would take the worker's single client slot. The host starts on the placement as made. A load that does not fit a node with shared memory is ended by that agent's watchdog: the load fails with the agent's reason and the launches of every member are stopped.
- **Learned need after a memory kill.** When a worker launch exits with `watchdog` or `oom_kill`, the model's learned memory need is raised by a tenth, at least 256 MiB. Like every learned need it holds until Potluck restarts.
- **Reading the launch state.** "While a load is in progress on that node" (section 6.3) is from the start request until Potluck's instance is ready or the agent reports `load.state: "done"`, whichever comes first. A worker is `ready` before its load starts, so `ready` does not slow the reads.

**Not on the wire: the watchdog floor.** The agent's floor is its install option (section 9, default 1000 MiB). Potluck's figure is its own setting (default 1000 MiB), used for placement memory and for `min_mem_available_mib`. Neither side tells the other, so the two must be set to the same value by hand. Open point for the next change request.

## 12. Open questions

1. **Worker per load, or always running.** This draft starts `rpc-server` per load (D2). Start cost measured for a host process on a PS5 is under 1.5 s. To confirm in phase 4: that a fresh worker per load does not slow warm loads, since the page cache belongs to the machine and not to the process.
2. **Source address of Potluck's requests** as seen by a PS5 (expected `192.0.2.1`) and by the host machine's own agent (expected Potluck's container network). First phase 4 test.
3. **What llama's device list prints** on the PS5 and CUDA builds (names, order, no PCI address expected), needed to match devices to the stats contract's GPU entries.

Settled in review, 2026-11-20:

- A PS5 gets its agent through the starter. The scheduler side writes the starter; the datacenter ships it unchanged and guarantees what section 5.4 lists.
- No second membership source in phase 4; the old PS5 worker container, its watchdog, recovery daemon and image loader are retired in the same payload that ships the starter (PS5 side).
- Nvidia nodes use the Docker install for now; bare-metal Nvidia stays documented as untested.
- The PS5 GPU clock stays a datacenter-side service. It watches for a unit with the prefix `potluck-launch-`.
- Local read access is `GET /status` on `127.0.0.1`.
- The Nvidia laptop can mount the share (192.0.2.3 reaches 192.0.2.1:2049, checked by the PS5 side).
- The strict-mode and timeout log tokens are final as quoted in section 6.3.
- The fork's `rpc-server` sets `SO_REUSEADDR`, so launches one after another can bind the same port.

## 13. Tests that close this contract's parts

1. Agent against fake programs on a development machine: registration and every refusal, both version directions, a foreign process, program download with a wrong checksum and with too little space, every launch refusal, the pause race, lease expiry, lease kept across an agent restart, stop with a wrong id, forced stop, a stop that hangs (`stopping`), reset, watchdog, the share probe against a hung mount, agent restart during a launch on both install paths, the starter run twice and with the marker present.
2. Potluck against a fake agent in its end-to-end tests: registration, lost node with and without a launch, Potluck restart with launches on nodes, worker launches around a load, exit reasons, pause, force unload on a live and on a lost node, deregister, removal and re-admission.
3. Phase 4 closing test, on the real machines: Devstral 24B on three PS5s through agent workers gives content hash `f892d1ebbf38fb1d`; one load with an Nvidia member logs its `load_mode` and gives the same content hash on two separate loads; a payload apply during a load does not disturb it; a node uninstalls and rejoins cleanly.
