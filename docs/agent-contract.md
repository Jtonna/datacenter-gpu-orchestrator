# Agent contract: Potluck and `potluck-agent`

This is the contract between Potluck (the scheduler, `llm-scheduler`) and `potluck-agent` (the node agent), as of release 0.1.9. A field added after release 0.1.0 is optional and says from which release it is sent. Code comments cite change requests by label (CR-5, CR-9, ...); the Changelog at the end says what each one changed and where.

A change to this document becomes binding when both sides accept it: the scheduler side and the console side (the PS5 boot chain, RAM-only setup and payload delivery). Until then nothing may rely on it.

The agent is a machine service that flips role per load. Agents are workers only in phase 4; hosting comes in phase 6.

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
| 3122 | Potluck: API, UI, agent registration, program download | all interfaces of the host machine | agents, clients |
| 2049 | share (NFSv4) | `192.0.2.1` | nodes that mount the share |
| 50060 | agent API | the node's address that reaches Potluck, and `127.0.0.1` | Potluck; local programs for `GET /status` only |
| 50052 | `rpc-server` during a worker launch | the node's address that reaches Potluck | the host of that load (Potluck's own host in phase 4, a host node from phase 6) |
| 50061 | `llama-server` during a host launch (phase 6, release 0.1.9) | the node's address that reaches Potluck | Potluck only: every route but `/health` wants the launch's per-launch key (6.15); open only while the host launch exists |
| 50070 | `node_exporter` | `127.0.0.1` | the local agent only |
| 50071 | `nvidia_gpu_exporter` (Nvidia nodes) | `127.0.0.1` | the local agent only |

All ports are fixed. Ports 50060, 50070 and 50071 must be free when the agent starts; if one is taken, registration fails with a plain reason. Ports 50052 and 50061 are only checked when a launch needs them (section 6.2).

## 4. Registration, identity and heartbeat

### 4.1 Identity

- `id`: the lowercase MAC address of one physical network interface (for a Docker install in a container, see below), for example `02:00:00:00:00:01`. It is computed at every agent start and never stored, so a cloned drive or a swapped network card cannot carry a wrong id.
- Which interface: the one the route to the configured Potluck address leaves by. When that address belongs to the machine itself (the host machine), the interface that owns the address. When the result is not a physical interface (loopback, bridge, VLAN, `docker0`, `veth`), the interface of the default route. If no physical interface is found, the agent does not register and reports why on `GET /status`.
- Docker install (`--install docker`) in a container on a bridge network: there is no physical interface, only the container's `eth0` (a `veth`). When neither the route interface nor the default route's interface is physical, the id is the MAC of the interface that carries the route to Potluck, and the agent logs one INFO line saying the id comes from a virtual interface. That id is only stable when the container's MAC address is fixed (compose `mac_address`); without it every new container is a new node. A Docker install on the host network still takes the physical interface's MAC, and a bare install keeps the rule above. An empty or all-zero MAC is never an id. `potluck-agent id` takes the same `--install` flag (default `bare`).
- On a PS5 this is the Ethernet interface.
- `name`: a display label. The agent sends the machine's hostname as the default. Potluck may hold a different label. Nothing is keyed by `name`.
- The host machine's id is its MAC too, not a name such as `host-1`. No profile forces it, so nothing breaks.

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
- `addr` is the address Potluck reaches the agent and its launches at: the agent's own address that reaches Potluck, or, from release 0.1.9, the agent's `--advertise-addr` when set (a Docker container behind NAT, section 9). The agent itself always binds its own address.
- `gpus[].vendor` is `amd`, `nvidia`, `intel` or `other`. `backends` lists what the machine can run (`vulkan`, `cuda`), found by the agent from the drivers present.
- GPUs are read from `/sys/class/drm/card*`. From release 0.1.9, a machine with no drm card at all that has `/dev/dxg` and finds `libcuda.so.1` (WSL2 or Docker Desktop on Windows, GPU paravirtualization; `/usr/lib/wsl/lib` is searched) reports one GPU `{"vendor": "nvidia", "model": "NVIDIA GPU (WSL2 GPU-PV)"}`; with `cuda` in `backends` its platform is `linux-amd64-cuda`. A machine with drm cards is read as before.
- `ram_only` is true when the agent was started with the RAM-only option. It is an install option, not a hardware test.
- `model_source`: section 8.
- `programs`: what the agent already holds and has just re-hashed, as `[{"name", "sha256"}]`, so an agent restart does not fetch again.
- `launch_id`: a launch that exists on the node at registration (the agent restarted, or Potluck did).
- `foreign_processes`: any `rpc-server` or `llama-server` process on the node that the agent did not start, as `[{"pid", "exe"}]`. Not empty means the agent sends the registration, is refused (`foreign_process`), and starts nothing.
- `watchdog_floor_mib` (CR-9; optional, sent from release 0.1.1): the agent's memory watchdog floor in MiB (section 6.7), for example `"watchdog_floor_mib": 1000`. Section 11.2, item 2.
- `capabilities` (optional, sent from release 0.1.6): optional requests this agent answers, as a list of words. Missing or empty means none. Known words: `file_cache_evict` (section 6.13), sent by a Linux agent whose model source has a model folder; `host_launch` (section 6.15, from release 0.1.9), sent by an agent whose model source is not `push`. Potluck ignores words it does not know, and never sends an optional request to an agent that does not list it.

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
- `logs` (CR-10; optional, sent from release 0.1.1): where and how the agent pushes its log lines. Absent or `null`: the agent ships nothing. Section 11.3, item 2.
- `server_time_unix_ms` (CR-10; optional, sent from release 0.1.2): Potluck's own clock when it answered, in Unix milliseconds, sent only with `logs`. The agent stamps its log lines from it. Section 11.3, item 11.
- `startup_ack` (optional, sent from release 0.1.4; late builds of 0.1.3 already send it): how many start-up marks of the registration's `startup` object Potluck took. Absent: Potluck takes no start-up reports. Section 11.4. The registration itself may carry `startup` (same section); a Potluck built before it ignores it, and one that cannot read it drops it and takes the rest.

Refusals, all `200` with `"accepted": false`, a `reason` word and a `message`:

| `reason` | Meaning |
|---|---|
| `agent_too_old` | `agent_release` is lower than Potluck's release. The answer carries `required_release` (section 5.4). |
| `potluck_too_old` | `agent_release` is higher than Potluck's release. The node waits. |
| `unsupported_hardware` | No platform in the release matches the node's `os`, `arch` and `backends`. |
| `duplicate_id` | Another live agent with a different `addr` holds this `id`. |
| `foreign_process` | `foreign_processes` is not empty. |
| `removed` | The node was removed in Potluck and has not been re-admitted (section 10). |
| `release_unavailable` | Potluck serves no verified program release (its `programs/<release>` folder is missing or failed its check, section 5.2), so it has nothing to hand out. The `message` says why. (CR-5) |

**Order of the checks** (CR-5). The first that fails is the answer: a release is served (`release_unavailable`); the node is not removed (`removed`); the releases are equal (`agent_too_old`, then `potluck_too_old`); no foreign process (`foreign_process`); the id is not held by another live agent (`duplicate_id`); a platform matches (`unsupported_hardware`). A registration with an empty `id` is refused before all of these, with `unsupported_hardware`.

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

- `devices` is what the node's own llama program reports (`--list-devices`), read after the programs are verified and after every launch exits. It is the figure placement uses. The agent does not compute it. `shares_system_memory` is true for an integrated device. A `POST /launch` that arrives during the read (under 2 s) waits for it; it is not refused.
- From release 0.1.1 (CR-9): `shares_system_memory` is reported as the agent's launch check uses it (section 11.2, item 1): true for every device the agent holds to `MemAvailable`. That is every device except one known to have memory of its own: a CUDA device with `--shared-memory auto`, or any device with `--shared-memory false`. So a Vulkan device that the agent cannot tell to be integrated is reported `true`. The agent option `--shared-memory true|false|auto` (default `auto`, section 9) overrides the rule; the starter does not set it.
- `mem_available_mib` is read by the agent itself from `/proc/meminfo`, not from an exporter. It is read after the programs are in place, so on a RAM-only node it already excludes them.
- `stats` is the object defined by the stats contract (section 7). A missing or unreadable `stats` never makes a heartbeat invalid.
- `model_source.reachable`: section 8.
- CR-9; all optional, sent from release 0.1.1 (section 11.2):

  | Field | Meaning |
  |---|---|
  | `watchdog_floor_mib` | The agent's memory watchdog floor in MiB, as in the registration. |
  | `stop_reason` | Only while the node's launch is `stopping`: why the agent is stopping it (`watchdog`, `lease_expired`, `forced`, `stopped`, `reset`). |
  | `stop_message` | Only with `stop_reason`, when there is more to say (the watchdog's figures). |

- CR-10; optional, sent by agents from release 0.1.2 (Potluck reads it from 0.1.1): `logs`, the agent's log shipping figures (section 11.3, item 4).
- Optional, sent from release 0.1.4; late builds of 0.1.3 already send them (an agent built before them sends neither, and a Potluck built before them ignores them): `host_uptime_s`, the machine's own uptime in seconds since its kernel booted, read from the agent's monotonic clock (`CLOCK_BOOTTIME`) when it builds the heartbeat, with decimals; and `boot_id`, the kernel's boot id (`/proc/sys/kernel/random/boot_id`, 32 hex digits without dashes), read once at agent start and left out when it cannot be read. For example `"host_uptime_s": 4123.25, "boot_id": "0123456789abcdef0123456789abcdef"`. A Docker install sends the host kernel's: a container shares both with its host. Neither is a wall-clock time (rule 2.1). A value of the wrong type, a negative uptime or a boot id that is not hex is dropped by Potluck; the heartbeat is still taken.

- Optional, sent from release 0.1.4 (late builds of 0.1.3 already send it): `startup`, the agent's start-up report, sent until Potluck has acknowledged all of it (section 11.4). A report that cannot be read is dropped; the heartbeat is still taken.

Answer `200`: `{"release": "0.1.0", "paused": false}`. When the heartbeat carried `startup`, the answer has `startup_ack` (section 11.4; absent from a Potluck built before it). With central logging on (CR-10) the answer also has `logs_revision` (from release 0.1.1, section 11.3, item 3) and `server_time_unix_ms` (from release 0.1.2, item 11); release 0.1.0 ignores both.

**The agent's three failure cases, kept apart:**

| Case | Agent does |
|---|---|
| Heartbeat cannot be sent (no connection, timeout, `5xx`) | Keeps its state, its launch and its exporters. Sends the next heartbeat on time. Nothing else. The lease decides the fate of a launch. |
| Heartbeat answered `410` (Potluck does not know this agent, or the heartbeat's `agent_start_id` is not the one of the agent's last registration: CR-5) | Registers again at once. Does not touch its launch. |
| Heartbeat answer names a `release` different from the agent's | Registers again; the answer to that decides (`agent_too_old`, `potluck_too_old`). |

**Lost node.** Potluck marks a node lost after 10 s without a heartbeat. Section 11 says what follows.

**Silent node with a launch (CR-6).** A node that runs a launch Potluck holds is not lost after 10 s. It is lost after `lease_ttl_s` without a heartbeat (section 6.2; 20 s): only then has the agent ended the launch by itself. This is one number, not two: Potluck takes the time from the lease TTL it sends with the launch, so the two cannot drift apart. From 10 s on the node takes no new load (`unplaced: "silent"`). Reason: an agent restart keeps its launch (section 6.1) and can take longer than 10 s; giving the load up at 10 s would answer `stop_launch` to an agent that had just re-adopted it. A node without a launch is lost after 10 s.

## 5. Programs and versions

### 5.1 One release

A release is one string, for example `0.1.0`. Potluck, the agent and the program set each carry it. Potluck serves exactly one release at a time.

- Agent and Potluck must have the same release. Lower: `agent_too_old`. Higher: `potluck_too_old`. Neither gets a load.
- An agent of an older release is refused with `agent_too_old` and `required_release` set to Potluck's release, and updates itself as section 5.4 says: bare and RAM-only nodes download the new agent; a Docker node exits 75 and waits for the image `potluck-agent:<release>`. It gets no load until it runs the new release. Potluck of release `<r>` serves the folder `programs/<r>/`, which must exist.
- An agent with the right release but without all programs verified reports `programs_ready: false` and gets no load.
- **Same build on both ends of a load.** The llama RPC protocol needs host and workers from the same fork commit. In phase 4 the host is Potluck's own `llama-server`. Potluck checks at start that its own host program was built from the commit named in `release.json` (`builds.llama`), and refuses to serve the release, and so to start any load, when it was not.
- Each program also has a `build` (the fork commit for llama programs, the upstream version for exporters), for display and for benchmark records.

### 5.2 Folder layout

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
- The `agent/` folder is filled by the scheduler side, the platform folders by the console side until the builds are automated.
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

### 5.5 Planned for phase 7, not part of any release yet: llama releases that update by themselves (CR-1)

No release up to 0.1.6 implements this section, and nothing is held to it.

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

**Restarting the agent, in words.** On a Docker install a launch is a child process inside the agent's container, so stopping or restarting the container (or the agent in it) ends the launch. Potluck then ends the instance with reason `agent_restarted`, sends no stop for a launch it knows has ended this way, and loads the model again on the next request. On a bare install the launch is its own systemd unit and survives an agent restart (CR-6, section 6.12). The agent's last log line says which of the two applies.

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

`worker.expect_mib` (CR-9; optional, sent from release 0.1.1): the memory in MiB that Potluck expects this launch to take on the node, for example `"expect_mib": 6200`. With it the agent makes the `would_not_fit` check below; without it the agent makes no such check. Section 11.2, item 1.

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
- From release 0.1.9 a host launch also takes `expect_mib`, `rpc_file_load` and `api_key`, and is sent only to an agent that advertises `host_launch`. Section 6.15.

Checks before anything starts. Each failure is a refusal with a `reason` word and a `message`:

| HTTP | `reason` | Check |
|---|---|---|
| `409` | `busy` | No launch is running or stopping. The body carries the existing `launch_id`. An exited launch does not count. |
| `409` | `paused` | The last heartbeat answer did not say `paused`. This closes the race between a pause and a load already on its way. |
| `409` | `release_mismatch` | `release` equals the agent's and `programs_ready` is true. |
| `503` | `model_source_unreachable` | For a host, or a worker with `model_dir`: the last share probe succeeded (section 8). |
| `404` | `model_not_found` | Host only: the model file exists and is readable. |
| `507` | `low_memory` | `MemAvailable` is at least `min_mem_available_mib`. |
| `409` | `would_not_fit` | CR-9, from release 0.1.1. Worker with `expect_mib` only (from release 0.1.9 also a host with `expect_mib`, 6.15). Two tests; every one that applies must pass. The device test: the device's `free_mib` is at least `expect_mib`. The `MemAvailable` test: `MemAvailable - expect_mib` is at least the watchdog floor; it applies unless the device is known to have memory of its own, and always when no device figure is known. When `MemAvailable` cannot be read and that test applies, the launch is refused with a message that says no memory figure could be read. The message names the numbers. Section 11.2, item 1. |
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
- `load`: progress of this node's part of the load. `state` is `waiting`, `receiving` or `done`.
  - Host: `fraction` from the fork's `load_progress: fraction=N.NN` log line.
  - Worker reading from the share: `bytes_loaded` and `bytes_total` from the fork's `[file_load] progress bytes=N total=M` log line.
  - Worker receiving pushed weights: the agent has no count. `load` is `null` and Potluck keeps using the byte count it has on the sending side.
  - After the load, `load` stays with its final values.
- `exit`, when `state` is `exited`: `{"code": 137, "signal": "KILL", "reason": "…", "age_s": 3.1, "log_tail": ["…"]}`. `log_tail` is the last 40 log lines.
- `stop_reason`, `stop_message` (CR-9; optional, sent from release 0.1.1): present only while `state` is `stopping`, from the moment the agent has decided to stop the launch: `"stop_reason": "watchdog", "stop_message": "MemAvailable 912 MiB fell below the watchdog floor of 1000 MiB"`. The words are those of line 1 of the table below. Once the launch is `exited`, `exit.reason` counts. Section 11.2, item 4.
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

The two token strings are final. In phase 4 the host is Potluck's own child, so Potluck matches the same two tokens in its child's log; a worker-side agent never sees them.

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
  "last_error_age_s": null,
  "busy": true,
  "launches": 1,
  "busy_since": 1795400000
}
```

`model` is the host's model file name, or the `label` of a worker launch. `last_error` is the reason and message of the last refused registration, failed program check or abnormal exit.

CR-6:

- `role` and `model` describe a launch that exists (`starting`, `ready`, `stopping`). Once the launch has exited, `role` is `idle`, `model` is `null`, and the launch is named in `last_launch`: `{"id": "ln-...", "model": "<file name or label>", "reason": "<exit reason>", "ended_age_s": 12.5, "load": {...}}`. `load` at the top level is `null` then too: the launch's last load progress is `last_launch.load` (null when it had none). `last_launch` is `null` in every other state.
- Right after an agent restart the answer waits for the re-adoption of section 6.11 (normally well under a second). When that takes longer than 2 s the answer is `503` with reason `agent_starting`, as on the node's address. A launch that runs is never shown as `idle`.
- `last_error` of an abnormal exit is kept across an agent restart (it is read from the launch's exit record, with its real age). Any other `last_error` is not stored and is gone after a restart.

### 6.10 Details fixed while building the agent (CR-3)

They are part of the contract.

- **Malformed launch request**: a model name that leaves the model folder, `extra_args` the agent sets itself, a missing or wrong `tensor_split`, `model_dir` on a `push` node, a bad `launch_id` or `role`. Answer `400`, reason `invalid_request`.
- **Wrong launch id**: `DELETE /launch/{id}` answers `409` with reason `not_current`. A lease renewal for a wrong id, or for a launch that has exited, answers `404` with the same word.
- **Body fields**: a `worker_unreachable` refusal lists the workers in `failed_workers`. `POST /reset` answers `200` with `{"killed": [{"pid", "exe", "unit", "launch_id"}]}`.
- **Watchdog**: it kills with SIGKILL at once, with no SIGTERM and no 10 s wait.
- **Model source**: `--model-source dir:<path>` is reported as kind `share` with the path as target. A model folder with no file in it counts as unreachable, which also catches an empty mount point. The agent's probe log lines for a `dir:` source say `model folder probe ok` and `model folder probe failed` (with `dir`, no `target`), for a share `share probe ok` and `share probe failed`. Potluck's node feed keeps the registration's `target` in `model_source` after the first heartbeat (from release 0.1.4), so a model folder (an absolute path) can be told from a share (`host:export`).
- **Programs**: after three failed downloads the error is reported and the whole fetch is tried again every 60 s.
- **Stopping**: the wait for the launch port to close is at most 10 s, so a foreign listener cannot hold a launch in `stopping` for ever.
- **No GPU access**: when `llama-server --list-devices` prints `(none)`, the node reports "the agent must run as root or in the render and video groups" in `programs_error`, stays not ready, and reads the list again every 60 s. The listing is always taken from `llama-server`; `rpc-server` has no such option.
- **Strict file load** (host launches, phase 6): `strict_file_load: true` sets the environment variable `LLAMA_RPC_FILE_LOAD=required` on the host program; false sets `LLAMA_RPC_FILE_LOAD=1`. Value `0` means the host never asks workers to read files and pushes the weights.
- **Starter settings**: the starter takes `POTLUCK_URL` (`http://<IPv4>:<port>`), and optionally `POTLUCK_MODEL_SOURCE` and `POTLUCK_WATCHDOG_FLOOR_MIB`, from its environment or from `/etc/potluck/starter.env`. Missing or malformed settings make it wait and read them again.

### 6.11 Details fixed after the agent code review

They are part of the contract.

- **Agent start order**: a launch left by an earlier agent process is re-adopted first. Its lease and its memory watchdog run before the agent has an identity, a free port, a registration or a reachable Potluck. Exporter units of the earlier agent process (`potluck-node-exporter`, `potluck-nvidia-exporter`) are stopped before the port check of section 3. When such a unit cannot be stopped and still holds its port, the agent goes on and registers all the same: it never stays out of the pool over its own leftovers. It reads that exporter until it can replace it. A port held by anything else still blocks registration.
- **`503 agent_starting`**: until the re-adoption is done, every call on port 50060 except `POST /reset` answers `503` with reason `agent_starting` and `Retry-After: 1`. The agent never answers `GET /launch` with `none` for a launch it has not looked for yet, and it does not register before the re-adoption. Potluck treats this answer as "state not known yet", not as a lost launch.
- **Port 50060 taken**: the agent does not exit. It tries to bind again after 5 s, growing to 60 s, and does not register until both addresses are bound.
- **Kill**: an ordinary stop or a lease expiry is SIGTERM with `systemctl kill`, the 10 s wait, then the kill; a forced stop and the watchdog start with the kill at once. The kill is repeated until the unit is gone and is never given up. Its attempts escalate: first SIGKILL with `systemctl kill`, second `systemctl stop` of the unit, third SIGKILL sent directly to the processes whose cgroup is the launch's own `potluck-launch-<launch_id>` unit, and from the fourth on both SIGKILLs together, every time. The direct signal goes to no other process: the cgroup of each process is read again right before the signal, with the process pinned by a pidfd where the kernel has one (Linux 5.3), so a process id that was reused in between is not hit. The next attempt comes 1 s after one that was delivered and 250 ms after one that failed, so there are at most about four a second. Every attempt writes one line to the agent's log: its number, its method, its error. A restarted agent kills a `stopping` launch again; it does not trust a kill sent before the restart.
- **Start**: starting the unit is not tied to the `POST /launch` request (30 s limit of its own). When the start fails, the unit name is stopped before the launch is reported `start_failed`.
- **Launch unit**: it runs with `OOMScoreAdjust=900`, so the kernel's OOM killer takes the launch before the agent or sshd, and with the agent's `LD_LIBRARY_PATH`, the one the library check used.
- **Update (5.4)**: before a downloaded agent replaces the running one, it is asked for its release (`potluck-agent version`); a binary that reports another release is not installed. One release and sha256 is tried once: the attempt is kept in `<state-dir>/update-attempt`, so an update that changed nothing is not downloaded and started again every 60 s. A download that fails or has a wrong sha256 is tried again. The binary is never replaced while a launch exists.

### 6.12 Details fixed after the first run on a real machine (CR-6)

They are part of the contract.

- **Exporters across a re-registration.** Exporter units left by an earlier agent process are stopped once, at process start (section 6.11). A later registration of the same agent process (heartbeat answered `410`, or another release named) does not stop the exporters: one whose entry in the answer's `exporters` is unchanged keeps running, one that is missing or changed is started.
- **Exporter found not running.** When an exporter the agent should run is not running and its scrape fails, the agent starts it again, at most once every 30 s per exporter, with one log line.
- **No failed unit records.** Launch units are started with `systemd-run --collect`, so a killed launch leaves no failed unit in systemd. The exit status still comes from the `ExecStopPost` file. Exporter units have no exit file; their status is read from the unit, then `systemctl reset-failed` clears it.
- **Last load progress.** At exit the agent reads the rest of the launch log, a last line without a line end included, and takes the figures of any `[file_load]` line that has `bytes=` and `total=`, whatever its word, so the final figure is not one progress step short.
- **Stats warning.** A failed scrape is not warned about during the first 5 s after its exporter was started.
- **Kill ladder (addition to 6.11).** Before every kill attempt after the first, the agent looks for the unit again; when it is gone, no attempt is made. `systemctl` answering that the unit is not loaded or not found counts as done, not as a failed attempt. A stop request that arrives while a kill is under way (Potluck's ordinary stop after the watchdog's kill) does not start the next attempt early: it comes 1 s after a delivered one, as section 6.11 says.

### 6.13 `POST /cache/evict` (Potluck to agent): drop a model file from the page cache

Added in release 0.1.6 for the benchmark's cold load (benchmark guide, section 2). Additive: an agent that does not advertise the capability `file_cache_evict` (section 4.2) is never sent this request, and Potluck works as before without it.

```json
{"file": "gemma-3-12b/gemma-3-12b-Q4_K_M.gguf"}
```

`file` is the profile's model file, relative to the node's model folder (section 8), exactly as Potluck names it in a launch. The agent resolves it inside the model folder, opens it, counts the pages of the file that are in the page cache (`mmap` + `mincore`), asks the kernel to drop them (`posix_fadvise(POSIX_FADV_DONTNEED)`), and counts again. It never writes to the file and never drops anything but this one file. Answer `200`:

```json
{"file": "gemma-3-12b/gemma-3-12b-Q4_K_M.gguf", "file_bytes": 7300000000, "cached_before": 7290000000, "cached_after": 0, "duration_s": 0.41}
```

`cached_before` and `cached_after` are bytes of the file in the page cache, counted in whole pages and never more than `file_bytes`. The kernel may keep pages that another process holds mapped or dirty, so `cached_after` can be above 0; the caller decides what is cold enough (Potluck's benchmark: under 1 % of `file_bytes`).

Refusals use the launch refusal body `{"reason", "message"}`:

| Status | `reason` | When |
|---|---|---|
| 400 | `invalid_request` | The body is not JSON, `file` is empty, absolute or holds a NUL byte, leaves the model folder (after `..` parts are resolved, so `a/../b.gguf` names `b.gguf`), resolves (symlinks followed) outside the model folder, or is not a regular file. |
| 404 | `model_not_found` | The file does not exist in the model folder. |
| 409 | `busy` | A launch exists on the node (running or stopping). Dropping the cache under a running worker would change its load, so the caller waits until the launch is gone. |
| 429 | `too_soon` | The last eviction that reached a file was less than 10 s ago, or another eviction is under way. The answer carries `Retry-After: 10`. Refused requests do not count. |
| 500 | `evict_failed` | `mincore` or `posix_fadvise` failed; `message` holds the error. |
| 501 | `unsupported` | This agent cannot do it (not Linux, or a node with the `push` model source, which has no model folder). |
| 503 | `model_source_unreachable` | The model folder could not be reached (a share that does not answer). |

- One eviction runs at a time; a request that arrives meanwhile is refused with `too_soon`. Every eviction and every refusal is logged at info level with the file and the counts.
- The same source-address rule as every Potluck-to-agent request holds (section 2).
- Potluck's own machine is not asked through its agent: the scheduler drops the file from its own page cache with the same code, because that machine serves the file to the nodes.

### 6.14 The busy file (for other software on the node)

Added in release 0.1.6. Additive. Software on the node that must not disturb it while a model is loading or loaded (maintenance scripts, updaters) may rely on this file. It needs no network call and no permission beyond reading a file.

- **Path:** `/run/potluck/busy` on a bare Linux install (agent option `--busy-file`; `auto` is the default and means this path on a bare Linux install and no file otherwise; `off` means no file). A Docker install writes none by default: its `/run/potluck` is not the machine's. If the folder is missing or cannot be written, the agent logs this once and carries on.
- **When it exists:** while a launch exists on the node: starting, ready, or stopping (until the program has gone). A file cache eviction (6.13) is not a launch and does not create it.
- **Order:** the busy file and the busy fields of the status route (`busy`, `launches`, `busy_since`) agree within one second, and the file is never removed before the launch has fully stopped.
  - The file is written when the agent accepts a launch request, before the launch's unit (or, on Docker, its child process) is started.
  - It is removed only after the launch has fully stopped: the unit is inactive or the child process has been reaped. This holds for a normal stop, a stop that takes a while, a reset (6.8), a lease expiry, the watchdog, and a start that fails. A failed start is answered only after the agent has stopped whatever the start left.
  - A request that is refused can make the file appear for the moment the refusal takes; that error is in the safe direction.
- **Format:** plain text, mode `0644`, four `key value` lines, each ending with a newline:

  ```
  launches 1
  since 1795400000
  updated 1795400123
  agent_pid 812
  ```

  `launches` is the number of launches (0 never appears: the file is gone instead; today at most 1, rule 2.3). `since` is when the node became busy and `updated` the last write, both in Unix seconds of the node's clock. `agent_pid` is the agent process that wrote it. The file is replaced atomically (a temporary file in the same folder, then a rename), so a reader never sees part of it. `updated` is refreshed at least every 15 s while busy.
- **Removal:** the file is removed when the last launch has gone, and when the agent stops cleanly with no launch.
- **Agent start:** a left-over file is removed before launches are re-adopted (6.11). It is written again the moment a launch is adopted. In between there is a gap with no file: one re-adoption attempt, normally well under a second and at most 15 s. When the attempt fails (for example, systemd does not answer), it is retried every second and the gap lasts until one succeeds. On a bare install a reader covers that gap with the unit rule below.
- **Staleness:** a reader treats the file as stale, and the node as not known to be busy, when no process with `agent_pid` exists (the agent died without cleaning up). A reader that wants a second check can also treat an `updated` older than 60 s as stale.
- **Edge case:** a bare launch can outlive its agent (6.11). On a clean agent stop with a launch still running, the file is kept, because the node is still busy. Its `agent_pid` is then gone until the next agent adopts the launch and rewrites the file, a few seconds at most under systemd.

- **Units (bare install).** On a bare (systemd) install, every launch runs as a systemd transient unit named `potluck-launch-<launch_id>.service`, whatever its role (`worker` or `host`). The unit is active while the launch runs, and it survives an agent restart. This is the rule of section 6.1, and other software may rely on it as well: an active unit with the prefix `potluck-launch-` means the node is busy, whatever the busy file says.
- **Docker install.** There are no systemd units for launches. A launch is a child process of the agent inside the agent's container (6.1), and it ends with the container. On that install the busy file (if a path is given) and `GET /status` are the signals.

The same fact is in `GET /status` (6.9): `busy` (true or false), `launches` and `busy_since` (Unix seconds, `null` when idle).

### 6.15 Host launches: a console as host (release 0.1.9)

Added in release 0.1.9 (phase 6). Additive and versioned:

- An agent that can run a host launch says so with the capability word `host_launch` (4.2). Potluck sends `role: "host"` only to such an agent. An agent that does not send the word is never asked: the load fails on Potluck's side with `host_not_capable` before anything is launched. (A 0.1.9 Potluck refuses older agents with `agent_too_old` anyway, and they update, 5.4; the rule covers a `push` node and an agent built without the word.) A `push` node never advertises it (the host reads the model from the node's model folder).
- Every new field is optional. A 0.1.9 agent takes a host request in the 0.1.6 form (no `expect_mib`, no `rpc_file_load`, no `api_key`) as before: `strict_file_load` decides `LLAMA_RPC_FILE_LOAD`, no memory test, no key.
- Potluck chooses a node to host only when a profile asks for it (`host: node`, which needs `force_nodes`): the first forced node hosts. It never picks a host by itself.

New fields of the `host` object:

```json
"host": {
  "model": "gemma-4-12b-it-Q4_K_M.gguf",
  "ctx": 8192,
  "parallel": 1,
  "workers": ["192.0.2.122:50052"],
  "tensor_split": [0.5, 0.5],
  "strict_file_load": false,
  "rpc_file_load": "0",
  "expect_mib": 7400,
  "api_key": "<32 random characters>",
  "extra_args": ["--n-gpu-layers", "999", "--fit", "off", "--jinja", "--cache-ram", "0", "-lv", "4"]
}
```

- `expect_mib`: the memory Potluck expects the host to take on the node: its device share of the weights, its context (KV cache and compute buffers) and the host process itself (`host_overhead_mib`, default 1024). The agent makes the same `would_not_fit` test as for a worker (6.2). `min_mem_available_mib` of a host is the watchdog floor plus that `expect_mib`, so it carries the host process too; it is never the node's whole budget.
- `rpc_file_load`: the host's `LLAMA_RPC_FILE_LOAD`, `"required"` (the workers read the file themselves; set when Potluck's `worker_model_dir` is on) or `"0"` (the weights go over the worker connection). Any other value is refused with `invalid_request`. When it is missing, `strict_file_load` decides as before: true is `required`, false is `1`.
- `api_key`: a per-launch key, 16 to 256 characters of letters, digits, `.`, `_` and `-`; anything else is refused with `invalid_request`. With it the agent writes the key to a file readable by the launch only (mode `0600`, in the agent's state folder under `/run`, which is RAM), starts `llama-server --api-key-file <that file>`, and removes the file when the launch ends and when the start fails. The key is never on the command line, in the unit's environment, in the state object, in the launch log or in the agent's own log. `llama-server` then answers every route but `/health` only with `Authorization: Bearer <key>`. Potluck makes a new key for every launch and sends it on every request it forwards; the client's own `Authorization` header is not passed on.
- `extra_args` may not name `--api-key`, `--api-key-file`, `--device` or `-dev` (the agent sets them).

What the agent does differently for a host from 0.1.9:

- **Its own device first.** With workers, the agent passes `--device <its own device>,RPC0,RPC1,...` so that the first weight of `tensor_split` is the host's own device. When the agent knows no device of its own and the request names workers, it refuses with `invalid_request`.
- **Read progress.** While a host starts, `load.bytes_loaded` is the bytes the host process has read (`rchar` of `/proc/<pid>/io`), capped at the model file's size, with `bytes_total` the file's size. This counts `read()` calls only: with memory-mapped loading (the default) it stays near 0 and the agent reports no progress; it counts with `--no-mmap` (Potluck's `host_no_mmap`, default off until measured). Potluck shows these figures as the host node's load progress but does not feed them to its stall rule in 0.1.9: the stall rule needs Potluck's own byte count of the upload, and a host on a node is not Potluck's process. A stuck node-hosted load is ended by its load timeout alone: the profile's `load_timeout` when set, else the formula of design 7.6, 120 s + 2 x (file size x share / link speed) summed over every node of the placement, the host node included (it reads its own share from the model share over its link). For a 2.6 GB model on one console at 11 MB/s that is about 593 s; set `load_timeout` on the profile to make it shorter.
- **Ports.** The host listens on 50061 on the node's address that reaches Potluck (3). The port is opened by the launch and closed when it ends. The host connects out from the node to the workers' 50052.
- **Nothing on the USB drives.** The model is read from the read-only model folder; the key file and the launch log live in RAM (`/run`). The rules of 2 hold as for a worker. Nothing in a host launch reboots or power-cycles the node.
- **Busy file.** A host launch makes the node busy exactly as a worker launch does (6.14); the unit is `potluck-launch-<launch_id>.service` whatever the role.

What Potluck does with a host launch:

- The worker launches of the load are started first; the host launch is sent once they are ready. The host node is not also a worker.
- The host's state is read, its lease renewed and its log copied as for a worker (6.3, 6.4, 6.6). When the agent reports `ready`, Potluck checks `http://<node>:50061/health` itself (5 tries, 1 s apart) before the instance takes requests; when that fails the launch is stopped with force and the load fails with `host_unreachable`.
- A plain unload is `DELETE /launch/<id>`, a forced one `DELETE ...?force=1`, then `/reset` after 5 s (7.2). A lost node is not called; its launch ends by its lease on the node.
- A host that ends by itself (watchdog, OOM kill, crash) ends the instance, its worker launches are stopped, and a watchdog or OOM kill raises the learned need as a worker's does (11.1).
- **Potluck restart.** A console-hosted model is not kept across a Potluck restart: the new run does not hold the launch, and it is stopped as any orphan launch is (4.2 `stop_launch`, 11).
- **Memory accounting.** Learned needs of a load hosted on a node are stored under a shape of their own (the shape key gains `host=node`), so they never mix with loads hosted on Potluck's machine. The host node's reservation carries `host_overhead_mib` on top of its device share.
- Requests in flight to a host whose instance is removed (its node lost, the host ended) are ended by Potluck: a console that loses power sends no reset.

## 7. Stats and controls

### 7.1 Stats

The agent scrapes its local exporters and puts the result in the heartbeat's `stats` object. Metric names, units, scrape rules, exporter versions and exporter arguments are defined in the stats contract (`docs/stats-contract.md`). This document defines nothing about stats beyond:

- the exporters listen on `127.0.0.1` only, the agent is the only network reader, and Potluck is the only place other systems read stats from;
- an exporter that is missing or keeps crashing is reported as absent in `stats` and shown as a warning on the node. It never stops the node from taking loads;
- nothing the agent needs for a launch comes from an exporter: `MemAvailable` is read from `/proc/meminfo` and device memory from llama.

### 7.2 Pause and force unload

Both are state and actions of Potluck, reachable from its web UI, its REST API and its MCP tools, and written to the audit log with a comment.

- **Pause** a node: Potluck places no new load on it. A load already using it continues. A node paused after it was claimed for a waiting request, while other models are still stopping to make room, loses that claim: the request is planned again. When that load ends, the node is shown as paused and idle. Unpause returns it to the pool. Pause is stored in Potluck's database and survives restarts of Potluck and of the agent. The heartbeat answer carries `paused`; the agent shows it on `GET /status` and refuses a `POST /launch` while it holds (section 6.2).

  **Unpause delivery** (from release 0.1.4; Potluck only, nothing on the wire changes). The agent learns that it is unpaused only from the next heartbeat or registration answer, up to one heartbeat (2 s) later. Until Potluck has sent the agent an answer with `paused: false`, the node stays out of placement: in `agents` of `GET /api/status` it keeps `unplaced: "paused"`, with `unpause_pending: true` (optional, absent when false) and an `unplaced_message` saying the unpause is being delivered, while `paused` already shows `false`. The same holds for a node that is lost when it is unpaused (it takes load only after it is back and its heartbeat answer told it) and after a restart of Potluck (the registration answer tells the agent the stored state; a node that was told `paused: true` there waits for the next answer after an unpause). A node unpaused before any answer told its agent of the pause takes load at once. A pause takes effect in placement at once; a load placed before the pause continues, even if its `POST /launch` reaches the agent before the heartbeat answer that carries the pause. Should a launch still be refused as `paused` (it overtook the answer), Potluck does not treat it as a failed load: no `502`, no backoff; the node takes no new load for 3 s and the request is planned again.
- **Force unload** a node: Potluck stops every model instance that has a launch on that node, because a split model cannot lose one member. On that node the stop is forced (`DELETE …?force=1`); if the agent does not report `exited` within 5 s Potluck calls `POST /reset`. The other nodes of the same model get a normal stop. Potluck shows the node idle when its agent reports no launch. A node that is lost (no heartbeat) is treated as holding nothing: force unload fails the instance, stops the other members and does not wait for the lost node.

REST routes on Potluck: `POST /api/nodes/{id}/pause`, `POST /api/nodes/{id}/unpause`, `POST /api/nodes/{id}/force-unload`, each with a required `comment`.

- **Expect a restart** (from release 0.1.4; late builds of 0.1.3 already have it; Potluck only): `POST /api/nodes/{id}/expect-restart` with `{"kind": "soft"|"full", "comment": "…"}` says the operator is about to restart the node (a soft restart with `systemctl soft-reboot`, or a full one). It is taken only while the node is paused and holds no launch (Potluck holds none on it and its agent reports none); otherwise `409` with the reason; `404` for an unknown node; `400` without a comment or with another kind. It is valid for 10 minutes. While it is pending, the node's absence is logged at INFO ("agent away for an expected restart") and written to history as info, not as the ERROR "agent lost". The next registration of another agent process closes it: that start's record (section 11.4) is marked `requested`, with `away_s` (from the call to the registration) and, once it has its figure, `away_to_ready_s` (from the call to ready for model loads), both on Potluck's clock. A restart that does not come within the 10 minutes is logged once as a warning and dropped. The call is written to the audit log and to history like a pause. The web UI does not offer it: it is an operator route for measuring a restart (runbook "Measure a soft restart").

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
- It never unmounts or remounts while a launch exists, and never uses a lazy unmount. A remount drops the page cache, which turns a 4.5 s warm reload into a 45 s cold one, so the agent remounts only after five failed probes in a row with no launch, and only when the mount point is not a mount of the configured share (it is not a mount, or another share is mounted there, which is logged).

**Docker install with `share`.** The machine mounts the share, not the container. The setup script writes a systemd mount unit on the machine with the same options plus `nofail`, and the container gets that folder bind-mounted read-only at `/models`. The container needs no mount capability. On the host machine the folder is the model directory itself. The agent only probes the folder.

**The share probe.** A `stat` on a hard mount can block forever when the server is down, and can be answered from the attribute cache while it is down. So the probe is a separate short-lived process that reads the first 4 KiB of one model file with `O_DIRECT`, with a 2 s timeout, every 5 s. At most one probe process is outstanding; while one hangs the result is "unreachable". `model_source.reachable` is the result of the last finished probe. When probes fail while the mount table shows the configured share mounted (for example during the NFS server's grace period after its restart), the agent does not mount again: it logs once per failing episode `share is mounted but not answering yet; waiting for the server`, and `share probe ok` with `unreachable_s` when the share answers.

**When the share is unreachable.** It is reported in the heartbeat. Potluck shows the node as "model source unreachable", gives it no load that needs it, and keeps a waiting request queued, trying again every 5 s until the load timeout.

**Mixed loads.** The requirement "workers must read from the share" is one switch for a whole load. A load that mixes `share` and `push` workers needs it per worker. Until that exists Potluck refuses such a mix with a plain reason.

## 9. Install

One setup script, `potluck-agent-setup`, with two paths. Common options: `--potluck <addr:port>`, `--model-source share:<target>|dir:<path>|push`, `--watchdog-floor-mib <n>`, `--allow-from <address or network>`.

The agent itself also takes `--shared-memory true|false|auto` (CR-9, from release 0.1.1; default `auto`): whether a launch on the node's device must also fit `MemAvailable` (section 11.2, item 1). Leave it at `auto` unless the rule picks wrong for a machine: `false` for a GPU with memory of its own that is not CUDA, `true` for one that uses system memory but is reported otherwise. Neither the setup script nor the starter sets it.

Log shipping options (CR-10, from release 0.1.2; section 11.3, item 15): `--logs on|off` (default `on`), `--log-buffer-mib N` (default 8), `--journal-dir PATH` (Docker install: a journal folder of the host, mounted read-only). Neither the setup script nor the starter sets them.

Busy file option (from release 0.1.6; section 6.14): `--busy-file auto|off|PATH` (default `auto`: `/run/potluck/busy` on a bare Linux install, no file otherwise). Neither the setup script nor the starter sets it.

- **Bare metal** (`--bare`): places the agent binary, writes one systemd unit `potluck-agent.service` that starts at boot, starts it. A RAM-only node does not use the setup script; it uses the starter (section 5.4), which passes `--ram-only`.
- **Docker** (`--docker`): starts one container `potluck-agent` from the image tagged with the release, with restart policy `unless-stopped`, `--init`, its own PID namespace, host networking, the GPU passed in when one exists, and the model folder mounted read-only.
  - From release 0.1.1 (CR-9 review): the agent's data folder (programs, launch logs) is the named volume `potluck-agent-data`, mounted at `/var/lib/potluck-agent` in the container, so a new container does not fetch the programs again. The models stay at `/models`; nothing else is mounted there.
  - From release 0.1.1 (CR-9 review): with `share:` the share is a systemd mount unit at `/var/lib/potluck-agent/models` on the machine, bound into the container as `/models` with `ro,rslave`, so a later mount of the share also reaches a running container. The drop-in `/etc/systemd/system/docker.service.d/potluck-models.conf` (`Wants=` and `After=` the mount unit) makes Docker start after the share is mounted at boot; `Wants=`, not `Requires=`, so Docker still starts when the share cannot be mounted. A setup without a share removes that drop-in.
- `--allow-from` adds source addresses the agent accepts requests from. It is needed on the machine that runs Potluck itself: Potluck runs in a container there, so its requests to the local agent arrive from Potluck's container network (`172.30.0.0/24` in the compose file) and not from the configured address. A Docker container behind NAT needs it too (below). It must name a network as small as the need: never `0.0.0.0/0` or `::/0`, which would let anyone who reaches the machine start and stop launches; the agent logs a WARN line at start when an `--allow-from` covers every address. That requests to the PS5s arrive from `192.0.2.1` is checked in the first phase 4 test.

**Docker behind NAT (Docker Desktop on Windows, WSL2; from release 0.1.9).** When the container cannot use host networking, its agent port `50060` and worker port `50052` are published on the machine (`-p 50060:50060 -p 50052:50052`, plus `-p 50061:50061` for a node that hosts), and Potluck must be told the machine's address, not the container's. Three options together:

- `--advertise-addr <ip>`: the machine's address on Potluck's network, where the ports are published. It is sent as the registration's `addr` (4.2) and nothing else: the agent API and the worker still bind the container's own address and `127.0.0.1`. It must be an IP address (not a name, not loopback, not `0.0.0.0`); `GET /status` shows it as `advertise_addr`.
- `--mac-address` pinning (`docker run --mac-address`, compose `mac_address`): the node's id is the MAC of the container's interface (4.1), so without a fixed MAC every new container is a new node.
- `--allow-from <the Docker gateway network>`: requests published through NAT arrive from the gateway of the container's network (for example the bridge network `172.17.0.0/16`), not from Potluck's address, so the agent must accept that network. Name that network only. Never `0.0.0.0/0`.

On such a machine the Nvidia GPU is found by `/dev/dxg` (4.2), and the setup script's firewall handling does not apply (the machine's firewall is the Windows one).

**Inbound ports and the firewall.** The machine must accept TCP connections from Potluck on `50060` (agent API) and `50052` (worker), and on `50061` from phase 6 (section 3). The setup script opens `50060` and `50052` only (CR-9 review): no Potluck release sends a host launch yet, so `50061` is not needed; the setup of the phase 6 release will open it. The setup script handles the machine's firewall as follows:

- **ufw active** (`ufw status` says `Status: active`): it allows TCP to the two ports from Potluck's address (the host part of `--potluck`) and from every `--allow-from` entry, each rule with the comment `potluck-agent`, and lists the rules it added in `/etc/potluck/firewall-rules`. `potluck-agent-uninstall` removes exactly the listed rules, whatever their port, so the `50061` rules of an older setup go too. A setup run again removes the rules it no longer asks for, the old `50061` rules included.
- **firewalld active**: it changes nothing and prints the rules to add.
- **`--no-firewall`**: it does not look at the firewall at all.
- On the machine that runs Potluck in a container, the source of Potluck's requests is the container network. The same `--allow-from <network>` that makes the agent accept them also opens the ports for that network.

A machine with another firewall, or with none of these, is left alone; the call-back check (section 11.1, CR-8) shows a machine Potluck cannot reach.

After either path the node appears in Potluck with programs fetched and stats flowing, with no other step.

## 10. Leaving the pool

There are two different things, and they have different effects.

**Uninstall, from the node** (`potluck-agent-uninstall`). The script does the work; the agent cannot delete itself and finish.

1. Best effort: `POST /api/pool/nodes/{id}/deregister` on Potluck. If Potluck is unreachable the script goes on.
2. On a node with a starter: write the marker file `/etc/potluck/disabled` (one empty file; it must survive reboots, so it cannot be under `/run`). The starter does nothing while it exists.
3. Stop the launch, the exporters and the agent; unmount the share if the agent mounted it; delete the program folder, the launch log, the state files, the unit or container and image, and the agent binary. A Docker install also loses the volume `potluck-agent-data` and the drop-in `docker.service.d/potluck-models.conf` (only that file; the folder only when it is then empty).
4. Nothing of Potluck's keeps running on the machine.

From release 0.1.1 (CR-9, section 11.2, item 3): an agent that is stopped while the marker file exists sends the deregister call itself before it exits, so a node that was disabled by hand, without the uninstall script, also shows `left`. It does not stop a running launch on the way out; on a bare install that launch keeps running until the machine restarts or the uninstall script stops it (section 11.2, item 3).

On deregister, Potluck stops any instance with a launch on that node, as in force unload, and marks the node `left`. A `left` node that registers again is admitted like a new one, with the same `id`. Installing again removes the marker.

**Removal, from Potluck** (`DELETE /api/nodes/{id}`, audited, with a comment), for a machine that is gone or unwanted. Potluck stops any instance on it and marks the node `removed`. A `removed` node that registers is refused (`removed`) until someone re-admits it in Potluck (`POST /api/nodes/{id}/readmit`, audited).

In both cases Potluck keeps: history, benchmark runs and learned memory needs that name the node, and the audit entry. Potluck forgets: the node's pause state, its programs state and its place in the pool. A profile that forces such a node stays in the configuration and is refused at load with the reason "forced node is not in the pool".

## 11. What Potluck does with all this

- **Membership** is registrations and heartbeats only, from the first agent release (phase 4). In that release every pool machine has an agent: the five PS5s, the host machine and the Nvidia laptop. Potluck no longer reads the datacenter's fleet feed.
- **Placement memory.** Potluck places on the node's llama device figures. For a device with `shares_system_memory`, or (from release 0.1.1, CR-9) a node whose tier sets `shared_memory`, the usable figure is the smaller of the device's free memory and `mem_available_mib` minus the watchdog floor, so programs and logs held in RAM are accounted for without a special case.
- **Phase 4 loads.** Potluck's own `llama-server` child is the host, unchanged. For each worker node of a load Potluck sends `POST /launch` with `role: worker`, waits for `ready`, then starts its child with those workers. On unload it stops the worker launches after its child has exited.
- **Phase 6 loads.** Potluck sends the worker launches first, then one `role: host` launch, and forwards requests to the host node's port 50061. The scheduler-child host is removed only after an agent-hosted load on the host machine matches it.
- **Exit handling.** An `exited` launch at any time fails the instance with the agent's reason, and Potluck stops the other members. `watchdog` and `oom_kill` raise the learned memory need of that model. `start_failed` is recorded with its log lines and not retried.
- **Lost node with a launch.** A node that loses power cannot report or expire anything. When a node with a launch is marked lost (after 20 s without a heartbeat, section 4.3, CR-6), Potluck fails the instance at once and stops the other members. It never tries to restart or recover the node. When the node's agent returns and reports a launch Potluck no longer holds, Potluck tells it to stop it (section 4.2).
- **Agent restart with a launch (CR-6).** The launch keeps running (section 6.1). While the agent is away the node is silent, not lost, for up to 20 s. When the agent registers again within that time with the same `launch_id`, Potluck keeps the instance and sends no `stop_launch`.
- **Potluck restart.** Potluck holds no launches after a restart. Each agent's next heartbeat gets `410`, the agent registers with its `launch_id`, and Potluck answers `stop_launch`. Launches whose agent cannot reach Potluck end by their lease.
- **One feed** for other systems is built from the heartbeats and Potluck's own instance state: the node feed, `GET /api/nodes`. Its entries are exactly the entries of `agents` in `GET /api/status`, plus Potluck's own machine marked `"local": true`. It is not part of this contract.

### 11.1 Details fixed while building the scheduler side (CR-5)

They are part of the contract. Each line says what Potluck does.

- **Register refusal `release_unavailable`** and **the order of the register checks**: section 4.2.
- **Heartbeat with a changed `agent_start_id`**: answered `410`, as for an agent Potluck does not know. The agent registers again, and the registration decides about its launch (`stop_launch`, or `agent_restarted` for a launch Potluck holds that the agent no longer reports).
- **Tiers.** A node gets its tier settings (priority, headroom, link speed, settle time) from the first tier of Potluck's configuration that matches it. For an agent node a tier can match on what the agent reports, so it does not depend on the hostname, which a console changes at each flash:
  - `platform`: exact, the platform Potluck chose at registration, for example `linux-amd64-vulkan`;
  - `device`: a case-insensitive glob (the syntax of the hostname matcher) that one of the node's device names must match. The device names are the `gpus[].model` of the registration and, once a heartbeat has arrived, its `devices[].name`.

  A node matches a tier when every matcher the tier sets holds. The older matchers still apply to an agent node when a tier sets them: `hostname` against the node's `name`, `source` against the word `agent`, `role` against the empty string, `local` against false. A node from the fleet feed reports no platform and no device, so a tier that sets one of them never matches it.
- **A watchdog floor that cannot be right** (CR-9, from release 0.1.1). A node whose agent reports a `watchdog_floor_mib` larger than half the node's `mem_total_mib`, or larger than 16384 MiB, would have no placement memory left. It is listed with `unplaced: "bad_floor"` and an `unplaced_message` that names the floor and the bound. Like `no_tier_match` it is outside the pool (not eligible, not counted in capacity) until its agent registers with a sensible floor. One warning is logged per registration.
- **A node that matches no tier** is never silent. It stays registered, is listed in `GET /api/status` (in `view.nodes` and in `agents`) with `unplaced: "no_tier_match"`, is not eligible, is not counted in capacity and gets no load. One log line is written per registration, not per heartbeat.
- **A node that cannot take a load yet** is listed with the reason and gets no load while the reason holds; a request that needs it stays queued and the profile is not backed off:
  - `programs_not_ready`: the last heartbeat said `programs_ready: false`, or no heartbeat has arrived since the registration;
  - `model_source_unreachable`: worker launches ask for `model_dir` and the last heartbeat did not say `model_source.reachable: true`. When worker launches do not ask for `model_dir`, the model source is not looked at;
  - `orphan_launch`: next line;
  - `silent` (CR-6): no heartbeat for 10 s from a node that runs a launch Potluck holds. It ends with the next heartbeat or registration, or with the node being lost at 20 s.
- **A heartbeat that names a launch Potluck does not hold** (the node was lost, its load was given up, and the node came back; or a start request got no answer). Potluck sends `DELETE /launch/{launch_id}` (section 6.5, not forced) once for that launch, and again only if the request could not be delivered. The node gets no load until a heartbeat reports no launch. This is in addition to `stop_launch` at registration.
- **The host machine's agent.** In phase 4 the local node comes from Potluck's own probe. An agent whose `id` or `name` equals Potluck's local node name (ignoring case) is the host machine's: the `local_node` setting, or `fleet.local_node` while `local_node` is empty. No fleet console is involved. It stays registered and is shown in status (`host_agent: true`, `placeable: false`, `unplaced: "host_machine"`), but is not published as a remote node and gets no worker launch.
- **Launch fields in status.** In an entry of `agents` in `GET /api/status`, `role` and `launch_id` are what the agent last reported in its heartbeat. The launch Potluck holds for the node, from the moment it decides to send it, is `potluck_launch_id`, with `launch_state` as Potluck last read it.
- **Call-back check (CR-8).** A registration and heartbeats only prove that the node reaches Potluck. After each accepted registration Potluck calls the agent back once: `GET /launch` on port 50060, timeout 3 s. Until that has answered `200` the node gets no load: `unplaced: "agent_unreachable"`, with `unplaced_message` naming the address and port and saying that an inbound firewall rule is the usual cause (in `view.nodes`, in `agents` and in the MCP status). While it fails Potluck tries again every 15 s. It logs one warning when the check first fails and one info line when it then answers. A later start request or state read (section 6.2, 6.3) that gets no answer puts the node back into this state and starts the check again. Like the other reasons that pass by themselves, a request that needs the node stays queued and the profile is not backed off. Potluck gives up a connection to an agent that is not established within 5 s; the 30 s of a start request are for a slow answer only.
- **One device per node.** A worker serves one device. Potluck uses the first device of the heartbeat's `devices` whose `backend` is the backend of the node's platform; when no device has that backend, the first device listed. Further devices of that backend are not used: the node's status counts them in `extra_devices_ignored` and the log warns once per registration.
- **Lost node with a launch.** The instance goes through Potluck's node-loss path at once (the time without a heartbeat is the wait: 20 s for a node with a launch, the lease TTL, instead of 10 s; CR-6): it is killed and its other members are stopped. Requests that were waiting for an instance that was still loading are queued again and get no error; requests in flight on a ready instance are cut off. The profile is not backed off. **Force unload** differs: requests waiting for that instance fail at once with an error that names the force unload and the node.
- **Reading the launches when the host program ends (CR-6).** Potluck's host program aborts within milliseconds of losing a worker, so Potluck often sees its exit before the worker's. When the host program of a load with agent workers ends without having been stopped, Potluck reads `GET /launch` on every member in parallel, again every 100 ms, for at most 2 s, before it handles the exit. A launch that ended by itself (`watchdog`, `oom_kill`, `lease_expired`, `start_failed`, `crashed` and the like; not `stopped`, `forced`, `reset`) is the failure reason, with its message, in the 502, the log and history (`workerExited`), and the memory-kill rule below applies. When no launch ended by itself, the host program's exit is the reason.
- **`LLAMA_RPC_FILE_LOAD` on Potluck's host program (CR-6).** For a load with agent workers Potluck sets it in the environment of the host `llama-server` it starts: `required` when `worker_model_dir` is on (the workers were launched with `model_dir` and must read the file themselves), `0` when it is off (the host sends the weights). It does not pass on whatever its own environment holds. Profiles carry no environment of their own.
- **`min_mem_available_mib`** of a worker launch is Potluck's watchdog floor setting (`watchdog_floor_mib`, default 1000). For a node whose memory is shared (its device has `shares_system_memory`, or its tier says so) the node's share of this load is added: its `expect_mib` (from release 0.1.9; before, the node's whole budget, which asks a forced node for more than it has once the floor exceeds its headroom). Only when the share is unknown is the budget added. From release 0.1.1 (CR-9) the node's own reported floor is used in place of the setting when its agent reports one (section 11.2, item 2).
- **No forced-fit pre-flight with agents.** Potluck's check of a forced placement against freshly probed worker memory is not run: a probe would take the worker's single client slot. From release 0.1.1 (CR-9) Potluck checks each forced member's `expect_mib` against its placement memory instead, before any launch is sent (section 11.2, item 1). The watchdog alone was measured to be too slow for a load that does not fit.
- **Learned need after a memory kill.** When a worker launch exits with `watchdog` or `oom_kill`, the model's learned memory need is raised by a tenth, at least 256 MiB. Like every learned need it is kept across a Potluck restart, by the shape of the load (the need-relevant settings, not the profile name) and the number of devices it ran on, until it is forgotten (`POST /api/forget-need` or `POST /api/needs/forget`).
- **Reading the launch state.** "While a load is in progress on that node" (section 6.3) is from the start request until Potluck's instance is ready or the agent reports `load.state: "done"`, whichever comes first. A worker is `ready` before its load starts, so `ready` does not slow the reads.

**Known limits.**

- **Nodes for forced profiles only** (CR-9 review, from release 0.1.1). Potluck's `no_auto_nodes` list keeps a node out of automatic placement; only a profile that names it in `force_nodes` uses it (a `command` profile, which runs on the local node alone, is not automatic placement: the local node in the list still runs it), and the forced-fit refusal of section 11.2 item 1 applies as for any forced node. It is Potluck configuration only: nothing on the wire changes and the agent does not know about it.
- **The lease is enforced by the agent.** While the agent process is dead, nothing ends its launch: the worker runs on until the agent is back and finds the lease run out (section 6.4).
- **`worker_model_dir` is one setting for the whole pool.** While it is on, a node whose model source is `push` is not placeable (`model_source_unreachable`). A choice per worker is planned (CR-7).
- **Potluck in a container on a pool machine.** The machine's own firewall must let the container network reach the agent and worker ports (section 9, `--allow-from`).
- **Reaching the agent is checked, not assumed** (the call-back check above; CR-8). Port 50052 is not checked before a load: a firewall that lets 50060 through and drops 50052 shows up only as a failed load.

**The watchdog floor.** The agent's floor is its install option (section 9, default 1000 MiB). Potluck's figure is its own setting `watchdog_floor_mib` (default 1000 MiB), used for placement memory and for `min_mem_available_mib`. In release 0.1.0 neither side tells the other, so the two must be set to the same value by hand. From release 0.1.1 the agent sends its floor and Potluck uses it for that node (section 11.2, item 2).

### 11.2 CR-9: memory check before a launch, the watchdog floor on the wire, leaving on purpose, the cause while stopping

CR-9 ships from release 0.1.1. The release rule of section 5.1 applies; what each release carries is in the Changelog.

Every field it adds is optional, so either side reads a body without them: an agent that gets no `expect_mib` makes no `would_not_fit` check; a Potluck that gets no `watchdog_floor_mib`, `stop_reason` or `stop_message` from an agent uses its own floor setting and waits for `exited`.

1. **Memory check before a launch.** Measured: a worker takes the model's whole device memory in one step, 4 to 6 GB within one second. The watchdog (section 6.7: four reads a second, floor 1000 MiB) cannot stop a load that does not fit; the machine is out of memory before the kill takes effect.
   - Potluck sends `worker.expect_mib` with every worker launch (section 6.2): the memory it expects this launch to take on that node. It is the whole need of the model when the node is the only member. Otherwise it is the node's share of the split: the figure a load of this run measured on that node for the same members and split when there is one, else need x the node's fraction of the tensor split, raised by `split_skew_margin` on a node with shared memory (the figure Potluck's fit rules already hold such a node to). For an estimated need (no `needs_mib`, nothing measured yet) every node's figure, and the single node's, also includes what each device holds beyond its share: `device_overhead_mib` plus the compute buffer that grows with the context.
   - The agent refuses the launch before it starts anything: `409`, reason `would_not_fit` (section 6.2). It makes every test that applies, and the stricter one decides:
     - the device test, when it knows the device: `free_mib - expect_mib < 0` refuses, with the device's `free_mib` of the last device read. The device is the one Potluck uses for the node (section 11.1, one device per node). The figure is read at registration and after a launch exits, so it can be old; it is used all the same;
     - the `MemAvailable` test: `MemAvailable - expect_mib < watchdog floor` refuses, and the message names the three numbers. It applies to every device that is not known to have memory of its own, and when no device figure is known at all (no device was read, or the read failed). Known to have memory of its own means: a CUDA device that does not report `shares_system_memory`, with `--shared-memory auto` (the default); or any device with `--shared-memory false`. With `--shared-memory true` it applies to every device. In words: dedicated-memory devices (for example CUDA) use the device figure; everything else, and any doubt, also uses `MemAvailable`. Reason: `shares_system_memory` comes from a heuristic, and a console's Vulkan device reports nearly all the RAM as free;
     - when the `MemAvailable` test applies and `MemAvailable` cannot be read, the launch is refused: no memory figure could be read.

     Without `expect_mib` the agent makes no such check; that is the only case in which a worker launch starts without one. The agent reports `shares_system_memory` as this rule uses it (section 4.3), so Potluck's placement figure for such a device is `min(device free, mem_available_mib - floor)`, the same figure the agent tests. "Potluck places it, the agent refuses it" is therefore not the normal case; it happens when memory is taken between the last heartbeat and the launch.
   - **A gate, not a guarantee.** The check runs when `POST /launch` arrives. The memory is taken seconds later, when the host connects and the weights arrive; nothing is reserved in between. What covers that gap is Potluck's placement accounting: a node runs one launch at a time (section 2, rule 3) and Potluck does not place a second load on memory it has already handed out. The watchdog (section 6.7) does not reliably cover it: a console can take 4 to 6 GB in one step, faster than a watchdog that reads four times a second with a 1000 MiB floor can react. Memory taken on the node by something other than Potluck in that window is not covered by anything.
   - Potluck treats `would_not_fit` as a failed load: the 502 and history (`workerLaunchFailed`) carry the agent's reason and message, the launches of the other members are stopped, the profile is backed off as after any failed load. The node is not quarantined and stays placeable, and the model's learned need is not raised: nothing ran, so nothing was measured.
   - Potluck does not place tighter than the agent's test. Placement memory of a node with shared memory is already `min(device free, mem_available_mib - watchdog floor)` and the fit rules keep each such node's share inside it. A forced placement has no fit rule, so this replaces the line "No forced-fit pre-flight with agents" of section 11.1: before the launches are sent, Potluck compares each remote member's `expect_mib` with that node's placement memory (no tier headroom taken off). When one is larger, or when a member's memory figure is unknown (its agent reports no usable device), the request is refused at once with `503`, reason "forced nodes do not fit", and a message that names the node and both figures, or says that its memory figure is unknown; no launch is sent, the profile is not backed off, and `POST /api/plan` shows the same refusal. A missing figure never means "start unchecked". The check first runs before any loaded model is evicted for the placement, on the memory each forced node would have with nothing of Potluck's loaded on it (its placement memory now plus what the instance holding it is expected to take there); a profile that cannot fit even then, or a member with an unknown figure, is refused at once and nothing loaded is stopped. That "even with nothing loaded" check also runs when the request arrives, before it is queued, and again each time its queue entry is looked at while it waits, so the refusal does not wait for the holder's `min_hold`, for a request in flight on it, or for another request's claim on the node. A forced node that is absent, lost, re-registering (listed with an unplaced reason) or whose memory figure is unknown keeps the request waiting instead, because it may come back with other figures. When it could fit, the eviction goes ahead and the check runs again on the real figures before the launches are sent.
   - **Not switchable.** This refusal does not depend on `forced_fit_check`: in agents membership it runs whatever that setting says, `off` included, and no setting turns it off. The machines it protects cannot be recovered remotely when they run out of memory. When `forced_fit_check` is `warn` or `reject` its own check (with headroom) runs here as well, from the agents' figures. The description of the setting and the settings page say so.
2. **The watchdog floor on the wire.** This replaces the hand-set floor of release 0.1.0 ("The watchdog floor", section 11.1). The agent sends its floor as `watchdog_floor_mib` in the registration (section 4.2) and in every heartbeat (section 4.3). Potluck uses the node's own floor, when it has one, for that node's placement memory, for item 1 and for `min_mem_available_mib`; else its setting `watchdog_floor_mib`. When the two differ, the node's entry in `agents` of `GET /api/status` has `floor_mismatch` (a sentence with both figures) beside the reported `watchdog_floor_mib`, and Potluck logs one warning per registration. The two no longer have to be set to the same value by hand.
3. **Leaving on purpose.** When the agent is stopped (SIGTERM or SIGINT) and the marker file `/etc/potluck/disabled` exists (section 10), it calls `POST /api/pool/nodes/{id}/deregister` once before it exits, with a limit of 3 s. The node then shows `left`, not `lost` after 10 s. Without the marker the agent says nothing and its launch is treated as section 6.1 says. A Docker install has no marker inside its container, so nothing changes there. Deregistering twice (the uninstall script already did) is harmless: the second call is answered `404`, which the agent logs as "already deregistered". Any other answer, or none within the 3 s, is logged as a warning with the status or the error; the node then shows `lost`, not `left`.

   **A leave with a launch running** (CR-9 review). The agent does not stop its launch on the way out; it deregisters and exits. Potluck ends the load at once on its side: the deregister stops the node's instance as a force unload does (section 7.2). Its stop request to this node is not answered, because the agent has already closed its API, so the node is treated as holding nothing. What happens to the launch process itself depends on the install:
   - Docker: the launch is a child of the agent's container and ends with it (section 6.1).
   - Bare metal: the launch unit outlives the agent (section 6.1) and nothing ends it by its lease, because only a running agent enforces the lease (section 11.1, known limits). It ends when the machine restarts, when `potluck-agent-uninstall` stops it (section 10, step 3), or when an agent starts again on the node and finds its lease run out. A node that was disabled by hand with a model loaded therefore keeps the worker process, and its memory, until one of these. Use the uninstall script, or force unload the node in Potluck before stopping the agent, when that matters.

   Stopping the launch inside the agent before it deregisters was not chosen: once the agent's run has ended, its kill ladder (section 6.11) no longer runs, so the stop could not be confirmed within the 3 s.
4. **The cause while stopping.** The launch state (section 6.3) has `stop_reason`, and `stop_message` when there is more to say, from the moment the agent has decided to stop a launch, while `state` is still `stopping`: `watchdog`, `lease_expired`, `forced`, `stopped` or `reset`. The heartbeat carries the same two fields while its launch is stopping (section 4.3); Potluck shows them in the node's entry in `agents`. They are absent in every other state; once the launch is `exited`, `exit.reason` is the word that counts (it can differ: a launch that was being stopped can still end as `oom_kill`). When Potluck reads the launches of a load whose host program ended (section 11.1), a `stopping` launch with `stop_reason` `watchdog`, `oom_kill` or `lease_expired` is taken as the cause at once, with its message, without waiting for `exited` within the 2 s.
5. **A paused node says why.** In an entry of `agents` in `GET /api/status`, a paused node that has no other reason has `unplaced: "paused"` (and `placeable: false`). Another reason of the node itself (`programs_not_ready` and the like) comes first; `paused: true` is unchanged.
6. **An excluded node says so** (from release 0.1.6). When `exclude_nodes` names a node, its entry in `agents` of `GET /api/status` and in `GET /api/nodes` (the local node's entry too) has `excluded: true`, `excluded_by` (the matching entry), `placeable: false`, `unplaced: "excluded"` and `unplaced_message: "excluded by exclude_nodes: <entry>"`. The exclusion comes before every other reason and before a pause. Nothing else in the entry changes: the agent keeps registering and heartbeating, and its release, devices and stats are shown as for any node. The node of `view.nodes` in `GET /api/status` has `excluded` and `excluded_by` as before. Neither field is present for a node that is not excluded. From release 0.1.7, an excluded node that still runs an instance placed before the exclusion also has `running_while_excluded: {"profile", "state", "since"}` (since: unix ms); the instance is not drained.
7. **Capability words are shown** (from release 0.1.7). Every entry in `agents` of `GET /api/status` and in `GET /api/nodes` has `capabilities`: the words the agent sent in its registration (section 4.2, for example `file_cache_evict`, section 6.13), sorted, or an empty list when it sent none; the local node's entry always has an empty list.

### 11.3 CR-10: central logging through Potluck

Batch 1, the Potluck side (the settings, the `logs` object, the pass-through with its limits and counters), ships from release 0.1.1. The agent batch, the shipper in `potluck-agent` (items 11 to 15), and the last batch, the read side under `/logs/`, the query route, Potluck's own lines and the compose entry of the store (items 16 to 19), ship from release 0.1.2.

Every field it adds is optional and every route is new: a release 0.1.0 agent ignores `logs` and `logs_revision` and ships nothing; an agent that gets no `logs` object ships nothing.

1. **When logging is on.** The setting `logs_enabled` is on (the default) and Potluck has an address for the log store. That address is a deployment fact, not a setting: the environment variable `LLM_SCHEDULER_LOGS_STORE_URL` of Potluck, for example `http://potluck-logs:9428/logs` (the store runs with `-http.pathPrefix=/logs`). Without it logging is off: no `logs` object is handed out, the heartbeat answer has no `logs_revision`, status has no `logs`, and the routes below answer `404`. One exception: when the setting `logs_push_url` names another address, the `logs` object is handed out with that address even without a store address; Potluck's own two `POST` routes still answer `404` then, because it has nothing to forward to. An address that is not `http://` or `https://` is logged as an error and taken as unset. Potluck schedules the same with or without logging.

2. **The `logs` object.** In the registration answer (section 4.2), and the answer to `GET /api/pool/nodes/{id}/logs`:

   ```json
   {
     "revision": 1830412345,
     "url": "/api/pool/nodes/02:00:00:00:00:01/logs",
     "format": "jsonline",
     "auth": null,
     "limits": {"max_batch_bytes": 1048576, "max_line_bytes": 8192, "max_bytes_per_s": 262144, "every_s": 2},
     "deny_units": []
   }
   ```

   - `url` without a host is a path on the Potluck the agent registered with (the address rule of section 4.2). With `logs_push_url` set it is that address, as written in the setting.
   - `format` is always `jsonline`: one JSON object per line, in the store's own field names (`_msg` for the message; `level`, `unit`, `stream` as the sender sets them).
   - `auth` is `null`: no credential is handed out. When a later change adds one it is `{"header": "<name>", "value": "<value>"}`, a request header to send with every push.
   - `limits`: the largest body, the length the sender cuts its lines to, the node's byte rate, and how often at most the sender pushes.
   - `deny_units`: system log units the sender drops. Always empty; there is no setting for it yet.
   - `revision` is a number computed from all logging settings and from whether Potluck has a store address. It changes when one of them changes, and it is the same after a Potluck restart with the same settings. It is not a counter: compare it for equality only.
   - `logs` absent or `null`: the agent ships nothing.

3. **`logs_revision` in the heartbeat answer** (section 4.3) is the revision of the node's `logs` object. When it differs from the one the agent holds, the agent reads the object again with `GET /api/pool/nodes/{id}/logs`. When it is absent, logging is off: the agent stops shipping and need not ask (the `GET` would answer `404`). A changed setting so reaches every agent within one heartbeat.

4. **Shipping figures in the heartbeat** (section 4.3), optional: `"logs": {"lines_sent": 120, "lines_dropped": 3, "last_error": "429 rate_limited", "last_error_age_s": 4.5}`. The counts are since the agent started. `last_error` and `last_error_age_s` are left out when there was no error. An unreadable `logs` never makes a heartbeat invalid; it is taken as not reported.

5. **The routes.** All on Potluck's one port.

   | Route | Who | Answer |
   |---|---|---|
   | `GET /api/pool/nodes/{id}/logs` | agent | `200` the `logs` object; `410`; `404` when logging is off |
   | `POST /api/pool/nodes/{id}/logs?source=<agent\|launch\|journal>[&launch_id=<id>]` | agent | `204` accepted; else the table of item 7 |
   | `GET /api/logs/config` | client | `200` the clients' `logs` object (`url` `/api/logs`, `max_bytes_per_s` 65536); `404` when logging or `logs_client_intake` is off |
   | `POST /api/logs` | client | `204` accepted; else the table of item 7; `404` when logging or `logs_client_intake` is off |
   | `GET /api/logs/query?query=<LogsQL>[&limit=&start=&end=]` | reader | `200` JSON lines from the store; item 16 |
   | `GET`, `HEAD` `/logs/...`; `POST /logs/select/...` | reader (a browser) | the store's own search page and query API, read only; item 16 |

   - **Who sends** is checked like the heartbeat: the node id in the path, and the `agent_start_id` of the node's last registration in the header `X-Agent-Start-Id` (the body is not a JSON object, so it cannot carry it). A node Potluck does not know, one that left or was removed, or another `agent_start_id`: `410`, and the agent registers again as after a heartbeat `410`.
   - **Body**: JSON lines, at most 1 MiB, with `Content-Encoding: gzip` or without one. One request carries lines of one `source`, and for `launch` of one launch. `launch_id` is required for `source=launch` (1 to 128 letters, digits, `.`, `-`, `_`, `:`) and ignored otherwise. An empty body is answered `204` and not forwarded.
   - Potluck streams the body to the store as it is. It never decodes it, re-encodes it or holds it in memory, so it does not look at the lines and a line that is not JSON is the store's to skip.

6. **Trusted fields.** Potluck sets them for the whole request; the store lets them win over fields of the same name inside a line (checked against the store). A sender cannot write under another node's name.

   | Request | Fields Potluck sets |
   |---|---|
   | from a node | `node_id`, `node_name`, `platform`, `release` (the agent's, from its registration), `source` |
   | from a node, `source=launch` | the same, and `launch_id`, and `profile` when Potluck holds that launch for that node or held it lately (the last 256 ended launches are remembered, so the last lines of a launch still get their profile) |
   | from a client | `source` = `client`, `client_addr` (the address the request came from), `actor` (the `X-Actor` header, when sent) |

   - Every trusted field a request does not set is dropped from its lines (the store's `ignore_fields`), so a client's line cannot name a `node_id` and a node's line cannot name a `profile` or an `actor`.
   - The store has no way to write a comma inside such a value. A comma in a value is stored as `%2C` and a percent sign as `%25`; a control character becomes a space; the value is cut at about 256 bytes. A field whose value is empty is not set. Node ids, with their colons, are stored unchanged.
   - Stream fields of every request: `source`, `node_id`, `unit`.

7. **Limits and answers.** Every refusal has the usual error body with a `reason` word.

   | Limit | Value | Answer | `reason` |
   |---|---|---|---|
   | Request body | 1 MiB, counted while it streams | `413` | `too_large` |
   | Per node | 256 KiB/s, burst 2 MiB | `429` with `Retry-After` (seconds) | `rate_limited` |
   | Per node, at the same time | 1 request | `429` with `Retry-After: 1` | `node_busy` |
   | All log requests at the same time | 8, of which at most 2 from clients | `429` with `Retry-After: 1`, at once, no waiting | `too_many_requests` |
   | Per client address | 64 KiB/s, burst 1 MiB | `429` with `Retry-After` | `rate_limited` |
   | All clients together | 1 MiB/s, burst 2 MiB | `429` with `Retry-After` | `rate_limited` |
   | The whole forward | 5 s | `503` with `Retry-After: 5` | `store_unavailable` |
   | The sender's body | must arrive within 4.5 s | `400` | `body_read` |

   Other answers: `204` accepted (whatever `2xx` the store answered); `410` (`unknown_sender`); `404` (`logs_off`); `400` `bad_request` (no or a wrong `source`, `source=launch` without a usable `launch_id`); `415` `unsupported_encoding` (a `Content-Encoding` other than gzip); `400` `store_rejected` (the store answered `400`, for example because no line of the body was JSON); `503` `store_unavailable` also when the store is down or gives any other answer.

   - A sender waits on `429` and `503` (`Retry-After`) and sends the same batch again. On `413` and on `400` sending the same batch again does not help.
   - Bytes are counted as they are on the wire: for a gzip body, the compressed bytes.
   - A body with a declared length is checked against the rate before it is read. A body without one (chunked) is let in while the sender has any budget left and charged after it streamed; the sender then waits that much longer. When such a body passes 1 MiB the forward is cut and answered `413`; lines that had already passed may be stored.
   - A refused request is never read to its end. When its declared body is at most 64 KiB the connection is kept; otherwise it is closed with the answer, and a sender in the middle of writing a large body may see a broken connection instead of the status. It treats that like `429`.
   - There is no queue in Potluck. What is refused stays in the sender's buffer.

8. **Off the scheduler's paths.** The log routes take no lock of the pool or of the scheduler. They read a snapshot the pool publishes at each registration, each node that leaves or is removed and each launch that starts or ends (a heartbeat publishes nothing), and the configuration snapshot. Potluck's requests to the store use their own connections. A test in the build floods the routes with a slow store and with one that never answers and checks that heartbeats and a chat request keep their normal time and that never more than 8 forwards are in flight.

9. **Status.** In an entry of `agents` in `GET /api/status`, `logs` holds what Potluck counted for the node since Potluck started and what the agent reports: `{"accepted_bytes", "accepted_requests", "refused": {"<reason>": <count>}, "agent": {the figures of item 4}}`. `agent` is absent when the agent reports none. The top level has `logs`: `enabled`, `store` (Potluck has a store address), `revision`, `in_flight`, `peak_in_flight`, `max_in_flight`, `client_intake`, `clients` (the same counters for all clients together), `unknown_sender` (requests answered `410`), `reads_in_flight` and `max_reads` (item 16), and, once Potluck has read them from the store, `store_settings` (`retention_days`, `max_disk_gib` with `0` for no cap, `read_error`, `read_at`) and `warnings` (plain sentences, item 10). Without a store address and without `logs_push_url` neither is there. With a store address and `logs_enabled` off, the top-level one stays and says `"enabled": false`.

10. **Settings**, all live (no restart): `logs_enabled` (default on), `logs_retention_days` (14), `logs_max_disk_gib` (20), `logs_push_url` (empty), `logs_client_intake` (default on). `logs_retention_days` and `logs_max_disk_gib` describe the store, but the store takes them as start flags (`-retentionPeriod`, `-retention.maxDiskSpaceUsageBytes`, set in the compose file), which Potluck cannot change. Potluck reads the store's set flags from its `/flags` page at start and every 10 minutes; when they differ from the two settings, or cannot be read, the top-level `logs` of status has a sentence in `warnings` and the log has one warning per change. A flag the store was started without counts as its default (7 days, no disk cap).

11. **Time of a line** (agent batch). Rule 1 of section 2 stays: no time set by a node. A line's `_time` is Potluck's time, worked out by the agent:
    - Potluck sends `server_time_unix_ms`, its own clock in Unix milliseconds, in the registration answer (section 4.2) and in every heartbeat answer (section 4.3), only while it hands out a `logs` object. With logging off neither answer has it.
    - The agent keeps an anchor: that time at the middle of the request's round trip, on its monotonic clock (`CLOCK_BOOTTIME`). Every such answer sets it again. A line's time is the anchor plus the monotonic time from the anchor to the moment the line was produced. A line produced before the first anchor keeps its monotonic stamp and gets its time when it is sent.
    - A journal entry's time is its `__MONOTONIC_TIMESTAMP` (`CLOCK_MONOTONIC`), taken as an age off the agent's clock. An entry of an earlier boot gets the time it arrived, with `time_source: arrival`. The journal's realtime field is never used.
    - `_time` is RFC3339 with nanoseconds, in UTC (`2026-11-21T10:00:00.123456789Z`). With no anchor (a Potluck without `server_time_unix_ms`) a line has no `_time`, and the store uses the time at which it took it.

12. **Fields of a line** set by the agent: `_msg`; `_time` (item 11); `level` (`debug`, `info`, `warn`, `error`; from the journal also `emerg`, `alert`, `crit`, `notice`); `unit` (journal: the systemd unit, else the syslog identifier); `stream` (not set yet: launch output is read from the launch log, which mixes stdout and stderr); `truncated: true` when the message was cut; `time_source: arrival` (item 11). The agent never sets a trusted field (item 6). Each line, with its newline, is at most `max_line_bytes`: the agent cuts `_msg` on a character boundary to fit.

13. **What the agent ships.** Its own log lines (`source=agent`), which it also still prints. Launch output (`source=launch&launch_id=<id>`), read from the launch log file (section 6.6) on both installs. The machine's journal (`source=journal`): all of it on a bare install, less the units of `deny_units` and less the agent's own unit (already shipped as `agent`); on a Docker install only with `--journal-dir`, a mounted journal folder. The journal and a launch that exists at agent start are followed from "now". The ring is in RAM, `--log-buffer-mib` (8 MiB); when it is full the oldest lines go first, and the next batch of that source starts with one line that says how many were lost.

14. **The sender on each answer.** One request at a time, lines of one source per request, at most `max_batch_bytes` per body, gzip when that makes the body smaller; per round of `every_s` one batch per source; within `max_bytes_per_s` on the wire.

    | Answer | The sender |
    |---|---|
    | `2xx` | counts the lines as sent |
    | `429`, `503` | waits `Retry-After`, else 1 s doubling to 30 s, and sends the same batch again |
    | `410` | stops sending until the agent registers again, or for at most 60 s of its own clock (a `410` from the log route alone); keeps the batch |
    | `413` | sends the batch again in halves; a single line answered `413` is dropped and counted |
    | `400`, `415` | drops the batch and counts its lines as dropped; never sends it again |
    | `404`, any other answer, no answer, a broken connection | as `429` |

    A new `logs_revision` in a heartbeat answer makes the agent read `GET /api/pool/nodes/{id}/logs` in its own goroutine and use the new object (limits, url, `auth`, `deny_units`) at once; a `404` there means logging is off. A heartbeat answer without `logs_revision`, or a registration answer without `logs`, stops shipping: the ring is emptied, the journal reader stops, the heartbeat no longer has `logs`.

15. **Never blocking.** Shipping never makes the heartbeat, a launch call or the watchdog wait: a producer puts a line into the ring and returns, the ring's lock is held for memory work only, the readers drop by the byte rate instead of buffering, a launch writes into its log file and never into a pipe the agent must drain, and only the sender's own goroutine (with its own HTTP client) waits on Potluck. Agent options (section 9): `--logs on|off` (default on; `off` builds no shipper at all), `--log-buffer-mib N` (default 8), `--journal-dir PATH`. The figures of item 4 are sent only while a `logs` object is held.

16. **The read side** (last batch). Potluck passes reads through to the store; nothing else.
    - `/logs/...` is the store's own address space under its path prefix: Potluck forwards the path unchanged. Allowed: `GET` and `HEAD` on `/logs/`, `/logs/select/...` (the search page is `/logs/select/vmui/`, the query API `/logs/select/logsql/...`), `/logs/flags`, `/logs/metrics`, `/logs/health`, `/logs/robots.txt`, `/logs/favicon.ico`; `POST` only on `/logs/select/...` (the search page sends its queries as `POST`). Everything else is `403` `read_only` and never reaches the store: `/logs/insert/...`, `/logs/delete/...`, `/logs/internal/...`, `/logs/debug/...`, `/logs/select/vmalert/...`, any other method, and a path with an empty, `.` or `..` segment, a backslash or a NUL.
    - `Cookie` and `Authorization` are not passed on. Answers stream: the live tail of the search page arrives as the store writes it. A read ends when its reader goes away or Potluck shuts down; a store that takes a read and does not begin its answer within 60 s gets `503` `store_unavailable`.
    - At most 8 reads at once (`/logs/` and the query route together), on their own connections, apart from the 8 places of item 7. One more is answered `429` `too_many_reads` with `Retry-After: 1` at once, without waiting.
    - `GET /api/logs/query`: `query` (LogsQL, required, at most 64 KiB), `limit` (default 1000, at most 10000; a larger one is cut to 10000, the header `X-Potluck-Limit` says the limit used), `start` and `end` (passed as given: RFC 3339, Unix seconds or a span such as `1h`; at most 64 characters each). Potluck sends it to the store's `/select/logsql/query`. It answers the store's JSON lines (`application/stream+json`) as they come. A query the store refuses is `400` `store_rejected` with the store's words (at most 1 KiB). A query still running after 30 s, a store that is down or any other answer: `503` `store_unavailable`. A wrong parameter: `400` `bad_request`.
    - With logging off, or without a store address, both answer `404` `logs_off`, also when `logs_push_url` is set.
    - The MCP tool `query_logs` calls `GET /api/logs/query`, with a limit of 100 unless it is given one.

17. **Potluck's own lines** (`source=scheduler`) and **the output of the programs it starts** (`source=host`, with `profile` and `instance_id`, and `stream` `stdout` or `stderr` per line) go straight to the store's `/insert/jsonline`, not through the routes of item 5, with the trusted fields of item 6: `node_id` and `node_name` are the local node's (`local_node`, else `fleet.local_node`, else `local`), `platform` is Potluck's, `release` is Potluck's release. They are sent while logging is on and Potluck has a store address; `logs_push_url` does not change where they go. The files and the printed log stay as they are.

18. **Never blocking, on Potluck's side.** A log call and a child's output put a line into one RAM ring of 8 MiB and return. The ring's lock is held for memory work only; when it is full the oldest lines go first and the next batch of that source starts with one line that says how many were lost. One goroutine sends, with its own HTTP client (10 s per push; a store that never answers costs one push, which is tried again later; a push in flight is given up when logging goes off or Potluck stops), one batch per source per second, at most 1 MiB per body and 4 MiB/s, lines cut to 8 KiB. A line's `_time` is Potluck's clock when it was produced. Child output is taken at the writer that already writes the child's log file, before the file: a program writing more than 256 KiB/s has the rest skipped (the file keeps all of it), and one line says how many bytes were skipped. Lines from Potluck's start are kept until the sender first checks whether logging is on; then they are sent, or dropped when it is off.

19. **The store's container** (compose service `potluck-logs`): `victoriametrics/victoria-logs:v1.54.0` pinned by digest, no published port, on the scheduler's network, `-http.pathPrefix=/logs -retentionPeriod=14d -retention.maxDiskSpaceUsageBytes=20GiB -storageDataPath=/storage`, memory limit 512 MiB, data in `./logs-store` next to the compose file. Runbook "Central logging" says how to turn it on and off and how to wipe it.

**Not checked against the real store:** `ignore_fields` (item 6). `extra_fields` and `_stream_fields` were checked in a trial against the store. The syntax of all three was read from the store's source (VictoriaLogs v1.54.0).

### 11.4 Start-up reporting (from release 0.1.4)

Each start of an agent reports, on the machine's own time axis, how its start went, so that Potluck can say how many seconds after boot start the node was ready for model loads. It is sent from release 0.1.4; late builds of 0.1.3 already send it. Everything here is optional on the wire: an agent built before it sends no `startup` object and gets no record, and a new agent against a Potluck built before it gets no `startup_ack` and stops sending the report in heartbeats until it registers again.

**The report.** The registration always carries it; heartbeats carry it while Potluck has not acknowledged all of it.

```json
"startup": {
  "boot_id": "0123456789abcdef0123456789abcdef",
  "agent_start_s": 21.403,
  "userspace_start_s": 3.118,
  "soft_reboots": 0,
  "marks": {"agent_start": 21.403, "registered": 23.512, "share_mounted": 24.007,
            "programs_ready": 41.25, "exporter_up": 41.31, "first_stats": 43.32},
  "external_marks": {"boot_progress_start": 1.95, "network_ok": 9.5, "payload_ok": 18.2}
}
```

- Every figure is seconds since the machine's kernel booted, from the agent's monotonic clock (`CLOCK_BOOTTIME`), rounded to milliseconds. None is a wall-clock time (rule 2.1).
- `boot_id`: as in the heartbeat (section 4.3). `agent_start_s`: when the agent process started.
- `userspace_start_s` and `soft_reboots`: systemd's `UserspaceTimestampMonotonic` (in seconds) and `SoftRebootsCount`, read once at agent start with `systemctl show --property=UserspaceTimestampMonotonic --property=SoftRebootsCount`. Each is left out when systemd does not give it (no systemd; systemd older than 254 has no `SoftRebootsCount`; an unset timestamp is 0). Never guessed. systemd's figure is `CLOCK_MONOTONIC`, the agent's `CLOCK_BOOTTIME`; they are equal unless the machine was suspended, which a pool machine is not.
- `marks`, the agent's own, each recorded once per agent process, the first time it happens: `agent_start`; `registered` (the answer of the first accepted registration arrived); `share_mounted` (the first probe that found the model source reachable; a push node never has it); `programs_ready` (the programs are verified and ready for a launch); `exporter_up` (the exporters were started with at least one source); `first_stats` (the first successful scrape).
- `external_marks`: the marks of the start-up marks file (below), passed through as they are, names Potluck does not know included.
- **Acknowledgement.** Potluck answers `startup_ack`: the number of marks (`marks` plus `external_marks`) of the report it took. When that is the number the agent sent and nothing changed since, the agent stops sending the report; a new mark, or a changed value in the marks file, sends it again. `startup_ack` absent: Potluck takes no reports; the agent stops sending them in heartbeats until its next registration.

**The start-up marks file** is a contract with the console side (the component that boots the machine and starts the payload). Path `/run/potluck/startup-marks`. One mark per line, `<name> <seconds>`, the seconds since the kernel booted (`CLOCK_BOOTTIME`, the first field of `/proc/uptime`), for example `network_ok 9.50`. Names are 1 to 64 letters, digits, `_`, `-` or `.`; the known names are `boot_progress_start`, `network_ok`, `payload_ok`, `boot_ready` and `dashboard_up`, and any other name is passed through. The writer replaces the file whole (writes a temporary file in the same folder, then renames it over), so a reader never sees half of it. The agent:

- reads it at start, then every 2 s, and stops after 5 minutes or after 60 s without a change, whichever comes first;
- skips a line that is not exactly a valid name and a finite number of 0 or more (a half-written last line included); a name given twice takes the last value; at most 32 names;
- ignores a file larger than 64 KiB, with one warning, and keeps the marks it had;
- takes a missing file as normal (no marks), and never creates or deletes the file or its folder;
- after a soft restart (systemd gave the userspace start and a soft-restart count that is not 0) drops every mark earlier than the userspace start, and ignores the whole file while its `boot_progress_start` is earlier (`/run` survives a soft restart, so the file is the previous start's until the writer starts a fresh one), with one INFO line.

**The start origin lines** (from release 0.1.6; an older agent passes them through unchanged as unknown names, but drops a `boot_origin` earlier than the cut-off). The file may carry, anywhere in it, one of two lines on the same clock as the marks:

- `boot_origin <seconds>`: where this start began. `0.000` on a full start; after a soft restart the moment the restart began (systemd's `ShutdownStartTimestampMonotonic`), which is before the new userspace start.
- `boot_origin_partial <seconds>`: the node could not tell that moment; the value is the userspace start, and the figure counts from there. A file with neither line is read as before.

The agent passes either through in `external_marks`. After a soft restart (when the cut-off above applies):

- both lines are exempt from the cut-off;
- the file is this start's when its `boot_progress_start` is at or after the userspace start, or, without `boot_progress_start`, when at least one mark other than an origin line is at or after it. The origin's own value cannot tell: this start's `boot_origin` and a left-over file's are both before the userspace start. A file that is not this start's is ignored whole, origin included (one INFO line; a file with only an origin line so far waits silently for the writer's next mark);
- a `boot_origin` later than the userspace start is dropped;
- an origin line counts only when it ends with a newline (a cut value would still parse, and an origin is exempt from the cut-off).

Potluck: when a record has `boot_origin` (at or before ready, and for a soft restart at or before the userspace start when known), the figure is ready minus `boot_origin` for every kind of start: unchanged on a full start (`0`), and on a soft restart it now includes the shutdown (ready 25.8 with `boot_origin` 10.0 gives 15.8, where the userspace start 15.8 gave 10.0). With `boot_origin_partial` or neither line it is as before. The record (and `last_startup`) says where its figure counts from in `origin`: `boot_origin`, `kernel` (a full start without `boot_origin`) or `userspace` (a soft restart without it). Records stored before have no `origin` and read unchanged. A group of `GET /api/startups` carries the newest figure's `origin` and `mixed_origins: true` when its figures do not all count from one origin.

When the record has both `boot_origin` and `boot_ready` (the console's boot screen writes `boot_ready` when all its steps have passed; it is ignored when earlier than `boot_origin`), the figure ends at the later of `boot_ready` and Potluck's own readiness point (`ready_s`, the moment the node was placeable): `headline_s` = max(`boot_ready`, `ready_s`) minus `boot_origin`. So it equals the console's own figure whenever the boot screen finishes last, and never claims ready before the node was placeable. The record and `last_startup` then also carry `placeable_s` (`ready_s` minus `boot_origin`) and `headline_end`: `boot_ready` or `placeable`, whichever ended the figure. A `boot_ready` that arrives after the figure was taken (it is written late) updates the figure, the stored record and the group figures of `GET /api/startups`; `away_to_ready_s` follows the end. Without `boot_ready` or without `boot_origin` nothing changes, and neither field is present. Records stored before read unchanged.

**Docker install.** It reports `boot_id`, `agent_start_s` and its own marks: the container shares the host's kernel, so its boot id and `CLOCK_BOOTTIME` are the host's. It reports no `userspace_start_s`, no `soft_reboots` and no external marks: inside the container `systemctl` would show the container's own systemd or none, and the host's `/run/potluck` is not mounted. So Potluck can tell a full start (another boot id) from the rest, but not a soft restart from an agent restart.

**What Potluck does with it**: one record per agent process, of one kind (full start, soft restart or agent restart), with its figure once the node is ready for model loads; `last_startup` in the node's entry of `agents` in `GET /api/status` and of `GET /api/nodes`; `GET /api/nodes/{id}/startups` and `GET /api/startups`.

## 12. Open questions

1. **Worker per load, or always running.** The agent starts `rpc-server` per load. Start cost measured for a host process on a PS5 is under 1.5 s. To confirm in phase 4: that a fresh worker per load does not slow warm loads, since the page cache belongs to the machine and not to the process.
2. **Source address of Potluck's requests** as seen by a PS5 (expected `192.0.2.1`) and by the host machine's own agent (expected Potluck's container network). First phase 4 test.
3. **What llama's device list prints** on the PS5 and CUDA builds (names, order, no PCI address expected), needed to match devices to the stats contract's GPU entries.

Settled in review:

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

## Changelog

History only: this section adds nothing to the contract. Code comments cite these labels.

| Label | What it changed | Where |
|---|---|---|
| CR-1 | Planned only: llama releases that update by themselves (phase 7). Not implemented. | 5.5 |
| CR-2 | Review changes to the early drafts by the console side, folded into the text before release 0.1.0. | throughout |
| CR-3 | Details fixed while building the agent: `invalid_request`, `not_current`, `failed_workers`, the reset answer, the watchdog's SIGKILL, the `dir:` model source, program fetch retries, the stopping wait, no GPU access, strict file load, starter settings. | 6.10 |
| CR-5 | Details fixed while building the scheduler side: `release_unavailable` and the order of the register checks; `410` for a changed `agent_start_id`; tier matchers `platform` and `device`; `no_tier_match` and the other unplaced reasons; the orphan launch stop; the host machine's agent; launch fields in status; one device per node; the lost-node path; `min_mem_available_mib`; the learned need after a memory kill; when the launch state is read fast. | 4.2, 4.3, 11.1 |
| CR-6 | From the first run on a real machine: a silent node with a launch is lost after `lease_ttl_s` (20 s), not 10 s (`unplaced: "silent"`); a bare launch survives an agent restart; `GET /status` with `last_launch` and `503 agent_starting`; exporters kept across a re-registration and restarted when found not running; `systemd-run --collect`; the last load progress; the stats warning grace; kill ladder additions; reading every launch when the host program ends; `LLAMA_RPC_FILE_LOAD` on Potluck's host program. | 4.3, 6.1, 6.9, 6.12, 11, 11.1 |
| CR-7 | Planned only: `model_dir` chosen per worker. Not implemented. | 11.1, known limits |
| CR-8 | The call-back check after each registration (`unplaced: "agent_unreachable"`) and the setup script's firewall handling. | 9, 11.1 |
| CR-9 | `worker.expect_mib` and the `would_not_fit` refusal; `watchdog_floor_mib` in registration and heartbeat, `floor_mismatch`, `bad_floor`; `--shared-memory` and the `shares_system_memory` rule; tier `shared_memory`; deregister on stop when the marker exists; `stop_reason` and `stop_message`; `unplaced: "paused"`. | 4.2, 4.3, 6.2, 6.3, 9, 10, 11, 11.1, 11.2 |
| CR-9 review | Release 0.1.1 as the release that carries CR-9; the Docker data volume `potluck-agent-data`, the share mount unit bound in with `ro,rslave` and the Docker drop-in; the setup script opens 50060 and 50052 only; a leave with a launch running; `no_auto_nodes`; the bounds of `bad_floor`. | 9, 10, 11.1, 11.2 |
| CR-10 | Central logging through Potluck. Batch 1, the Potluck side (settings, the `logs` object, `logs_revision`, the pass-through with its limits and counters, items 1 to 10). The agent batch: the shipper, `server_time_unix_ms`, the `--logs`, `--log-buffer-mib` and `--journal-dir` options (items 11 to 15). The last batch: the read side under `/logs/`, `GET /api/logs/query`, Potluck's own lines and the store's container (items 16 to 19). | 4.2, 4.3, 9, 11.3 |
| Cold load | The `capabilities` list in registration and `file_cache_evict`; `POST /cache/evict` (drop one model file from the node's page cache, with counts before and after). Additive: an agent without it is never asked. | 4.2, 6.13 |
| Busy file | `/run/potluck/busy` while a launch exists, for other software on the node, and `busy`, `launches`, `busy_since` in `GET /status`. Additive. | 6.9, 6.14 |
| Host on node | Phase 6: the capability `host_launch`; `host.expect_mib`, `host.rpc_file_load`, `host.api_key` (key file, `--api-key-file`); the host names its own device first; read progress from `rchar`; Potluck sends a host launch only to an agent that advertises it, never picks a host by itself, and stops a console-hosted model at a restart. Additive. | 3, 4.2, 6.2, 6.15 |

| Release | What it carries |
|---|---|
| 0.1.0 | The first release on the real machines: this contract with CR-2, CR-3, CR-5, CR-6 and CR-8. |
| 0.1.1 | CR-9 with its review, and CR-10 batch 1. Potluck serves `programs/0.1.1/`; the 0.1.0 platform folders could be copied unchanged, with the 0.1.1 agent in `agent/`. |
| 0.1.2 | The rest of CR-10: the agent's log shipper, Potluck's own lines, the read side. |
| 0.1.3 | The forced-fit check before any eviction; paused and unplaced nodes never evicted and a pause winning after a claim; the benchmark step facts (lowest available memory, load mode, roles, the reload-from-cache verdict); planned and blocked suites; the agent's optional `cached_bytes` figure (stats contract 4.3). Start-up reporting, `host_uptime_s`, `boot_id` and expect-restart were first built in late 0.1.3 builds and ship as part of 0.1.4. |
| 0.1.4 | The refusal at arrival of a forced profile that can never fit; the unpause delivered to the agent before the node takes load (`unpause_pending`, 7.2); lowest available memory by kind (GPU or system) and the file cache figures; the node feed and start-up records (11.4, `GET /api/nodes`, `GET /api/startups`, expect-restart, `host_uptime_s`, `boot_id`, `startup_ack`); the node feed keeps the model source's `target` (6.10); the `local_node` setting and the "pool" wording in agents mode (compose project `potluck`); learned memory needs kept across a Potluck restart, with `POST /api/forget-need`; an agent that does not remount a share that is mounted but not answering (8); stale start-up marks ignored after a soft restart (11.4). |
| 0.1.5 | Not a program release: a scheduler image only (its version said 0.1.5) that still served and required 0.1.4. Nothing on the wire changed. |
| 0.1.6 | For the agent: the `boot_origin` and `boot_origin_partial` lines of the start-up marks file, passed through in `external_marks`, with Potluck's figure counted from `boot_origin` and the record's `origin` (11.4); the `capabilities` list with `file_cache_evict` and `POST /cache/evict` (4.2, 6.13); the busy file `/run/potluck/busy`, the `--busy-file` option and `busy`, `launches`, `busy_since` in `GET /status` (6.9, 6.14). For Potluck only: an excluded node says so (11.2, item 6); a `command` profile is not shut out by `no_auto_nodes` (11.1, known limits); a raised need is kept by the shape of the load and its device count (11.1), listed by `GET /api/needs` and forgotten by `POST /api/needs/forget` as well as `POST /api/forget-need`; `POST /api/cache/evict` for the benchmark's cold load. |
| 0.1.7, 0.1.8 | Not program releases: scheduler images only, which served and required 0.1.6. |
| 0.1.9 | Agent: `--advertise-addr` and the WARN for an `--allow-from` that covers everything (9); the Nvidia GPU under WSL2 / Docker Desktop (`/dev/dxg`, `/usr/lib/wsl/lib`, 4.2). A console as host (6.15): `host_launch`, the new `host` fields, the key file, the host's own device first, read progress. Potluck: `host: node` profiles, `host_overhead_mib`, `host_no_mmap`, separate need shapes for a host on a node; the plan and `GET /api/profiles` show `host`, `host_node`, `host_overhead_mib` and `host_problem`. A 0.1.9 Potluck refuses agents of 0.1.0 to 0.1.8 with `agent_too_old` and they update (5.4); a 0.1.9 agent takes a 0.1.6-style host request. |
