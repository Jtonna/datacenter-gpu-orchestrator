# PS5-hosted server mode: interface between the scheduler and the PS5 side

Status: DRAFT 3, 2026-11-19. Nothing in this document is built. It needs the owner's approval before either side builds on it.

Draft 3 folds in four decisions the owner gave in the PS5-side session on 2026-11-19, relayed by the fleet console agent and not yet confirmed to the scheduler side first-hand: no fallback (sections 3, 6, 7.1), the share belongs to the scheduler stack (sections 4, 7.6, 8), model storage as a named volume (section 7.6), and real progress on short loads (sections 5.2, 7.4).

Authors: the scheduler agent (`ps5-ai-datacenter-load-balancer-32`) wrote it; the fleet console agent (`fix-fleet-console-tiles`) reviews the PS5 side.

## 1. What this is

Today every model is served by a `llama-server` process on the host machine, inside the scheduler's container. The PS5s only run `rpc-server` workers. For every token the host machine talks to the PS5s over the network.

In server mode the `llama-server` process runs on a PS5. The scheduler starts it through a small API on that PS5, then forwards requests to it. Other PS5s can still help as workers.

Terms used below, each with one meaning:

- **host machine-hosted**: `llama-server` runs on the host machine (today's mode).
- **PS5-hosted**: `llama-server` runs on a PS5.
- **server PS5**: the PS5 that runs `llama-server` for one load.
- **worker PS5**: a PS5 that only runs `rpc-server` for that load.
- **agent**: the node-agent service on a PS5. It gains the API in section 5.
- **launch**: one run of a PS5-hosted `llama-server`, from start request to exit.
- **share**: the read-only export of the host machine's model directory that PS5s read model files from.

## 2. Why (measured 2026-11-19 by the fleet console agent)

Same request, context 2048, one load per row. Not yet repeated by the scheduler side.

| Model | PS5s | Decode host machine-hosted (tok/s) | Decode PS5-hosted (tok/s) | Gain | Load host machine-hosted (s) | Load PS5-hosted (s) |
|---|---|---|---|---|---|---|
| Qwen 4B | 1 | 47.74 | 88.0 | 1.84x | 27.9 | 27 |
| DeepSeek V2 Lite | 1 | 81.11 | 152.1 | 1.88x | 100.4 | 93 |
| Gemma 12B | 1 | 28.86 | 41.8 | 1.45x | 74.5 | 72 |
| Qwen 4B | 3 | 40.2 | 76.5 | 1.90x | 28.0 | 16 (pull) / 47 (relay) |
| Devstral 24B | 2 | 18.22 | 21.0 | 1.15x | 82.5 | 66 (pull) / 183 (relay) |
| Qwen3 30B-A3B | 3 | 51.70 | 94.9 | 1.84x | 80.0 | 73 (pull) / 267 (relay) |
| Devstral 24B | 3 | 17.69 | 20.3 | 1.15x | 50.9 | 52 (pull) / 204 (relay) |
| Qwen 27B | 3 | 14.81 | 18.1 | 1.22x | 70.8 | 69 (pull) / 240 (relay) |
| Kimi-Dev 72B | 3 | 8.65 | 9.48 | 1.10x | 117.4 | 108 (pull) / 393 (relay) |

Load times include container start. The three-PS5 pull figures had uncontrolled page cache; controlled cold for Devstral on three is 58 to 59 s.

"relay": stock llama.cpp; the server PS5 reads the whole file and sends the workers their shares. "pull": the fork patch where each worker reads its own share from the share.

Other measured facts this design relies on:

- Output text is byte-identical between host machine-hosted and PS5-hosted for the same build, member count and split (Gemma 12B on one PS5; Devstral 24B on three).
- Reload with the share mounted permanently on the PS5 host: Gemma 12B 23 s warm against 79 s cold; Devstral on three PS5s 5 s warm against 58 s cold (one measurement).
- `--load-mode mmap` (the default) is the right choice: same cold time as `none`, much faster warm, no memory penalty seen.
- Parallel slots, Gemma 12B on one PS5, 2048 context per slot: about 0.5 GB per extra slot; 4 slots 77 tok/s total with 3.6 GB left; 8 slots 86 tok/s with 1.3 GB left.
- Three PS5s read the share at once at 108 to 118 MB/s each.
- A stock `llama-server` that cannot reach a worker carries on without it and loads everything locally. The local Vulkan device reports more free memory than the PS5 has. This hung a PS5 on 2026-11-19.
- PS5 wall clocks are wrong (one read 18 Sep 2026 on 19 Nov 2026). Nothing in this interface may depend on a PS5's wall clock.
- A PS5 under memory pressure freezes rather than being OOM-killed, because GPU memory is not charged to the process.
- An `rpc-server` accepts one client. A second connect waits in the backlog and times out; it is not refused.

## 3. Scope and order

**Step 1: one PS5, no workers.** A model that fits on one PS5 is served by a `llama-server` on that PS5. Needs: the agent API, the share mounted on the PS5 host, `llama-server` in the PS5 image, the GPU clock rule. Needs no firewall change between PS5s and no fork patch.

**Step 2: a server PS5 plus worker PS5s.** Only after the pull patch has what section 9 lists. Until then split models stay host machine-hosted.

Not in scope: PS5-hosted servers that use the host machine's or the Nvidia laptop's GPU as a member; more than one launch per PS5; any change to how PS5s register with the fleet manager.

host machine-hosted mode stays. It remains the mode for split models until step 2 and for profiles that use the host machine's or the Nvidia laptop's GPU.

**No fallback.** A load that the scheduler has decided is PS5-hosted never turns into a host machine-hosted load, and never into a relay upload over the socket. Every PS5 is expected to reach the share. A PS5 that cannot is reported as such and the load is retried (section 7.4). The agent always starts `llama-server` in the client's strict mode: a worker without the file, or a file identity mismatch, fails the load with a greppable token.

## 4. Who owns what

| Area | Owner |
|---|---|
| PS5 image with `llama-server` next to `rpc-server` | PS5 side |
| Agent API (section 5), memory watchdog, lease enforcement | PS5 side |
| Share on the host machine (the NFS export, part of the scheduler's compose stack) | Scheduler |
| Permanent mount of the share on each PS5 host | PS5 side |
| PS5 firewall, GPU clock script, recovery daemon's busy check, bootstrap | PS5 side |
| llama.cpp fork patch for workers pulling their share | PS5 side builds; the scheduler side validates before adopting |
| Fleet payload additions (section 6) | PS5 side, fleet manager unchanged in structure |
| Choice of host per load, placement, memory budget for the server PS5 | Scheduler |
| Starting, supervising, stopping launches through the agent API | Scheduler |
| Request routing to a PS5-hosted server, slots, queueing | Scheduler |
| Status, history, UI, MCP, benchmarks for both modes | Scheduler |

## 5. Agent API on each PS5

Plain HTTP with JSON on port 50060. `llama-server` listens on port 50061. Both are fixed and open to the host machine only (`192.0.2.1`). No authentication, consistent with the rest of this cluster.

One launch per PS5 at a time.

**Time.** The agent never reports wall-clock timestamps. It reports ages in seconds measured on the PS5's monotonic clock. The lease is counted on the same clock. The scheduler turns ages into its own timestamps.

**Agent restarts.** The agent is restarted by bundle updates. A launch survives an agent restart: `llama-server` runs as a host process in a systemd unit named by the launch id, with its binary and log in RAM (`/dev/shm`), and the agent adopts it again on start. No container per launch and nothing written to the PS5's USB stick: the sticks write at 6 to 10 MB/s and creating a container took 13 to 105 s. As a host process the start overhead is under 1.5 s (measured 2026-11-20). The lease clock restarts at adoption.

### 5.1 `POST /server`: start a launch

Request:

```json
{
  "launch_id": "ln-1795080000000-7",
  "model": "gemma-4-12b-it-Q4_K_M.gguf",
  "ctx": 8192,
  "parallel": 1,
  "workers": [],
  "tensor_split": [],
  "extra_args": [],
  "min_mem_available_mib": 9000,
  "lease_ttl_s": 20
}
```

- `launch_id` comes from the scheduler and is unique per launch.
- `ctx` is the context PER SLOT. The agent passes `ctx * parallel` to `llama-server -c`, which takes the total.
- `min_mem_available_mib` comes from the scheduler's budget for this load.
- `model` is a file name relative to the share. The agent refuses paths that leave the share.
- `workers` is a list of `host:port` of `rpc-server` workers. Empty in step 1.
- `tensor_split` is required whenever `workers` is not empty, with one entry per member, server first. The agent never lets llama.cpp choose a split.
- `extra_args` are appended last. The agent refuses arguments that it sets itself (model, port, host, rpc, tensor split, context, parallel).

The agent does these checks before it starts anything, and refuses with a reason if one fails:

1. No other launch exists on this PS5 (`409`, body carries the existing `launch_id`).
2. The model file exists on the share and is readable (`404`).
3. The share answers a `stat` within a short timeout (`503`). A look at the mount table is not enough.
4. Every worker accepts a connection and answers an RPC hello within 5 s (`502`, body lists the workers that failed). The agent closes these check connections before starting.
5. `MemAvailable` is at least `min_mem_available_mib` (`507`).
6. This PS5's own `rpc-server` has no client connected, meaning it is not a worker right now (`409`). The scheduler's placement already prevents this; the agent refuses anyway.

Answer on success: `202` with the state object of 5.2. The start is asynchronous.

The agent must never start `llama-server` with fewer workers than the request names.

### 5.2 `GET /server`: state

```json
{
  "launch_id": "ln-1795080000000-7",
  "state": "loading",
  "state_age_s": 12.4,
  "port": 50061,
  "command": ["llama-server", "..."],
  "build": "9088b8194",
  "uptime_s": 12.2,
  "load": {
    "fraction": 0.44,
    "members": [
      {"addr": "local", "fraction": 0.44, "bytes_total": 7121861440, "bytes_loaded": 3100000000, "state": "receiving"}
    ]
  },
  "mem_available_mib": 9120,
  "mem_low_water_mib": 9004,
  "exit": null,
  "lease_expires_in_s": 14
}
```

- `state` is one of `none`, `prechecking`, `loading`, `ready`, `exited`.
- `ready` means `llama-server`'s own `/health` answers ok.
- `build` is the fork commit the binary was built from, not the upstream base.
- `load.fraction` (0 to 1) is the primary progress figure, overall and per member. `bytes_loaded` is present only when the agent has a real count. `fraction` is OPTIONAL: `llama-server` at HEAD prints nothing during a load (two log lines in total), so without a patch the agent has no fraction. Planned: a few-line fork patch that logs the load callback value (`srv  load_progress: fraction=0.42`, at most once a second), parsed by the agent. Until then the agent reports the state, a `log_bytes` counter (size of the launch log), and on a cold load the bytes read from the share (NFS mount counters) as `bytes_loaded`. In step 2 the client tells each worker the total bytes it will ask it to read, so workers log `bytes=N total=M` and the agent reports a true `bytes_total` per worker.
- `load.members[].state` is one of `waiting`, `receiving`, `done`, the same words the scheduler uses today.
- `load` is updated about four times a second during a load, so a 4 s warm reload shows real intermediate values.
- After the load, `load` stays in the state object with every member at `done` and its final `bytes_loaded` and `bytes_total`. A load that finishes between two reads is still shown with its totals.
- `exit` when `state` is `exited`: `{"code": 137, "signal": "KILL", "reason": "...", "age_s": 3.1, "log_tail": ["..."]}`. Reasons: `watchdog`, `lease_expired`, `stopped`, `crashed`, `start_failed` (the process exited before loading, for example an invalid argument), and `oom_kill` (inferred: killed by a signal the agent did not send, plus a kernel log line). On a PS5 the reason expected in practice for memory trouble is `watchdog`. `log_tail` carries the last log lines.
- The last `exited` state stays readable until the next launch starts.
- With no launch ever: `{"state": "none"}`.

### 5.3 `POST /server/{launch_id}/lease`: renew

The scheduler calls this every 5 s. Body empty. Answer `204`, or `404` when the launch id is not the current one.

When the lease runs out (`lease_ttl_s` with no renewal) the agent stops the launch with exit reason `lease_expired`. This is what prevents a model staying in a PS5's memory after the scheduler restarts or loses the network.

### 5.4 `DELETE /server/{launch_id}`: stop

SIGTERM, then SIGKILL after 10 s. Idempotent: stopping an already exited launch answers `200` with its state. A launch id that is not the current one answers `409` and stops nothing, so an old scheduler process cannot stop a newer launch.

The agent reports `exited` only after the process is confirmed gone. Worker connections belong to `llama-server` and close with it; in step 2 the agent sends each worker an RPC hello again before reporting `exited`. Allow about 4 s. A worker serves one client, so a lingering connection would make it look unreachable to the next launch.

### 5.5 `GET /server/log?since=<byte offset>`: log

Returns the launch's combined output from the offset, with the next offset in a header. The scheduler copies it into its own child log files.

### 5.6 Memory watchdog

Permanent, not only for tests. If `MemAvailable` falls below 1000 MiB while a launch exists, the agent kills `llama-server` with exit reason `watchdog`. The floor is a setting of the agent.

## 6. Fleet payload additions

Each PS5 adds to what it already reports. All fields are optional, so mixed fleets work.

| Field | Meaning |
|---|---|
| `server_api_port` | Port of the agent API (50060). Absent means this PS5 cannot host a server. |
| `llama_server_build` | Fork commit the `llama-server` in the image was built from. |
| `share_mounted` | Result of a liveness check on the share (a `stat` with a timeout), not of the mount table. |
| `role` | `idle`, `worker` or `server`. `worker` is derived from an inbound connection on 50052, the same test the GPU clock script uses. |
| `launch_id` | Present when `role` is `server`. |

The scheduler treats a PS5 as able to host only when `server_api_port` is present, `share_mounted` is true and `llama_server_build` is on the scheduler's allowlist of validated builds. A build goes on the allowlist after the scheduler side has shown it gives the same output as the scheduler's own client. Equality of build ids is not required, because the pull-patch build is a different commit and still gave identical text.

`share_mounted: false` does not move a load to the host machine. The scheduler shows the PS5 as "share unreachable" in status, history and the UI, and retries (section 7.4).

## 7. What the scheduler does

### 7.1 Choosing the host

New profile field `host` with values `auto` (default), `host`, `ps5`.

`auto` in step 1: PS5-hosted when the profile runs on exactly one PS5 and does not force host machine or Nvidia laptop members. Otherwise host machine-hosted. The choice depends on the profile only, not on the PS5's state at that moment: a PS5 that cannot host right now (share unreachable, agent API absent, build not on the allowlist) gives a reported, retried load, not a host machine-hosted one.

`auto` in step 2 adds split models, decided from measured data per model: PS5-hosted when the expected time loaded is long enough for the decode gain to repay any extra load time.

`ps5` on a profile that cannot be PS5-hosted is refused at load with a plain reason. It never falls back silently.

### 7.2 Memory budget

The scheduler uses its own budget, never the Vulkan device's figure.

- The server PS5's budget is the worker budget minus a server allowance. Starting value 600 MiB, then learned per model from `mem_low_water_mib`.
- Each parallel slot beyond the first adds to the need. Starting value 500 MiB per slot at 2048 context, scaled with context, then learned.
- The existing fit check (`forced_fit_check`) applies unchanged, with these budgets.

### 7.3 Placement rules

- A PS5 has one role at a time: idle, worker for one host machine-hosted or PS5-hosted load, or server for one launch. The scheduler never places a worker load on a server PS5.
- The idle `rpc-server` stays running on a server PS5. The scheduler does not use it during a launch.

### 7.4 Starting and supervising

1. Decide placement and budgets as today.
2. `POST /server` on the server PS5. A refusal becomes a refused load with the agent's reason, recorded in history. When the reason is the share (`503`, or `share_mounted: false` before the call), the scheduler keeps the request queued and tries again every 5 s until the load timeout, then fails it with that reason. Proposal of the scheduler side; the interval is a setting.
3. Renew the lease every 5 s for the life of the instance.
4. Read `GET /server` four times a second while `state` is `loading`, and once a second otherwise, for progress and stall detection. The scheduler pushes each change to the UI over its websocket. It records the final `load` object (totals per member) in history even when the load ended between two reads. The existing stall rule applies to progress: a load is stalled when none of `fraction`, `bytes_loaded`, `state` and `log_bytes` has changed for the stall time. Without `fraction` (a warm load before the progress patch exists) the scheduler shows the load as in progress with no percentage and relies on the load timeout.
5. When `state` is `ready`, the instance is ready and requests are forwarded to `http://<ps5>:50061`.
6. On `exited` at any time, the instance is failed with the agent's exit reason. An `oom_kill` or `watchdog` exit raises the learned need for that model. A `start_failed` exit is recorded with its log lines and is not retried.
7. Unload is `DELETE /server/{launch_id}`.
8. If the agent API stops answering for longer than the lease, the scheduler marks the instance lost and the PS5 unusable until it reports `role: idle` again.

### 7.5 What stays the same for clients

The OpenAI-compatible endpoints, profile ids, queueing, TTL unload, `/running`, history events, benchmarks and MCP tools. A PS5-hosted instance reports the same fields, plus `host: "ps5"` and the server PS5's name.

### 7.6 The share

The share is a service in the scheduler's compose stack, next to the scheduler container, because the scheduler is what tracks which model is where and how far a load is.

- An NFS server container: read-only export of the model directory, NFSv4, bound to `192.0.2.1:2049` only. It replaces the hand-started test container `ps5-test-nfs`.
- Model storage: one named docker volume, defined with the local driver as a bind to the existing `/srv/llama-swap/models`. Nothing is copied, llama-swap keeps working, and there is no speed cost against a plain mounted directory. The scheduler container and the NFS container both mount it read-only.
- Host requirement outside compose: the kernel modules `nfs` and `nfsd` loaded on the host machine at boot. A container cannot load them. This is one file under `/etc/modules-load.d/` and needs sudo once.
- The scheduler checks the export itself and reports it in status: a dead share is shown as the cause before any PS5 reports `share_mounted: false`.

Container config as tested by the PS5 side in `ps5-test-nfs` (117 to 118 MB/s per PS5; three at once 108 to 118 MB/s each):

| Item | Value |
|---|---|
| Image | `erichough/nfs-server@sha256:784ef30907aa318b8324c4c49bd258b11a740ab0eea9f09b5ccf9df378fa77ca` (pinned by digest) |
| Privileges | `privileged: true`, `security_opt: label=disable`. A narrower set (SYS_ADMIN plus an apparmor exception) is documented by the image and untested here. |
| Port | `192.0.2.1:2049:2049/tcp` only. Never `0.0.0.0`. |
| Env | `NFS_EXPORT_0=/models 192.0.2.0/24(ro,no_subtree_check,fsid=0,insecure,all_squash)`, `NFS_DISABLE_VERSION_3=1` |
| Mounts | models volume at `/models` read-only; `/lib/modules` read-only |
| Restart | `unless-stopped` |
| PS5 mount as tested | nfs4, device `:/`, `addr=192.0.2.1,ro,nfsvers=4.2,rsize=1048576`. Production: a host mount, `soft`, `nofail`, short timeouts. |

`all_squash` with `ro` means model files must be world-readable on the host machine. They are today; the scheduler checks this per file and reports a file that is not.

Not built and not deployed. Adding it to the host machine is new infrastructure and waits for the owner's direct word to the scheduler side.

## 8. What the PS5 side changes outside the agent

- Share mounted on each PS5 host permanently and read-only, so the page cache survives between launches. Soft or hard is open (section 10). Either way the mount is `nofail`, a dead share must not block PS5 boot, and a launch must still be killable with the host machine away. PS5s have no `mount.nfs` helper, so the options are given to the kernel directly: `-t nfs4 -o ro,vers=4.2,addr=192.0.2.1,clientaddr=<ps5 ip>,rsize=1048576`.
- GPU clock script: also goes to performance while a local `llama-server` runs.
- Recovery daemon: treats a launch as busy, exactly like a worker with a client, and leaves memory to the watchdog. It acts on a server PS5 only when the agent API itself stops answering. Details after the PS5 side has read the daemon.
- Bootstrap: does not apply a bundle while a launch exists. Mandatory before step 1.
- Firewall: ports 50060 and 50061 open to `192.0.2.1`. Step 2 also opens the worker port to the other PS5s.
- Reboot: the agent starts on its own after a plain reboot. Today `node-agent.service` is disabled and bootstrap only starts it as part of an apply, so a reboot with an unchanged bundle leaves the PS5 out of the fleet.

## 9. Conditions for step 2 (split PS5-hosted loads)

In step 2 the patched `rpc-server` replaces the stock one in the same service on port 50052, started with `--model-dir`. It is backward compatible (a capability flag in the hello), so host machine-hosted loads keep working against it. Nobody starts a second `rpc-server`.

The pull patch is adopted only when:

| # | Condition | State |
|---|---|---|
| 1 | File identity between server and workers is checked by size plus a hash of the header and of sampled blocks. | Not met (size only). |
| 2 | The new set-tensor-from-file call has a timeout, and the share is a soft mount, so a stalled share fails a load instead of hanging it. Timeouts for all RPC calls are a separate, larger patch and not a condition. | Not met. |
| 3 | The directory a worker may read from is fixed when it starts and lookup is by file name inside it, with a path-traversal test. | Met in design; test owed. |
| 4 | Workers report progress for their share (`fraction`, and `bytes_loaded` when real). | Not met. |
| 5 | The scheduler side has validated it the same way as earlier patches: identical output, load time, no warnings, a rollback image kept. This includes host machine-hosted loads against the patched workers. | Not started. |

## 10. Open questions

1. Exact behaviour of the recovery daemon on a server PS5 (section 8).
2. The owner's direct word to the scheduler side for adding the share service to the host machine (section 7.6), and for the retry rule in section 7.4.
3. One wrong-text load, 2026-11-20, build `221f29bc`: the first cold Devstral 24B load on three PS5s answered with different text (200 tokens instead of 113), with no warning and exact worker byte totals. Four more cold loads were correct. Unexplained. Ruled out: a different split (explicit `-ts 1,1,1` in every run) and different byte counts per worker (identical to correct runs). Both requests on that instance gave the same wrong text and the warm reload right after was correct, so the fault was in that one load, not in the share's contents. Located on 2026-11-20 (build `40269e9ca`, host processes): the text is wrong when the SERVER PS5's page cache for the model is cold and RPC workers take part in the load. Cold on the server only: wrong. Cold on one or both workers only: correct. Warm everywhere: correct. It is not the pull patch: plain relay upload (`LLAMA_RPC_FILE_LOAD=0`) is wrong in the same way. Worker digests of the bytes handed to the backend were identical in good and bad loads. The wrong text is gross (one nonsense word, then nothing useful), with two different hashes seen; a 32-token canary catches it. Cause, confirmed on the PS5s on 2026-11-20: a race in the parallel-upload fork patch. On an integrated GPU ggml-vulkan reports no mmap support, so the load uses plain file reads (`load_mode = none`); the loader threads then share one file handle and position, and a tensor can silently receive another tensor's bytes. Forcing the collision gives the same wrong hash cold and warm, so the page cache only changed the timing. It does not need RPC workers, so single-PS5 hosting is not safe with the unfixed client either. Fixed in fork commit `2730b591f` (seek and read serialised per file); same load time. The scheduler's host machine-hosted client logs `load_mode = mmap` for PS5-only loads and is not on this path; profiles with a host machine or Nvidia laptop GPU member must be checked for their `load_mode` before they are trusted. A second, latent race on the mmap path exists in the scheduler's client and gets fixed with the same patch. The canary is required on every load path, single PS5 included, because an unfixed or regressed client fails silently.
4. Canary before ready (proposal of the PS5 side, accepted in principle): after a load the scheduler sends one short fixed request and compares the text hash with the expected one for that profile, member set, split and build; on mismatch the load fails and is retried. For both hosting modes.
   Request length: the full standard request for split loads until it is known where the wrong text diverged; a 32-token request for single-PS5 loads.
5. Share mount: hard, proposed by the PS5 side. A hard NFS wait honours SIGKILL, every stop path of the agent ends in SIGKILL, and the agent mounts the share itself (not from fstab), so a missing host machine cannot block boot. Stated from kernel documentation, not tested. Drill for section 11: the scheduler side stops the share during a load; the launch must die on stop and the PS5 must stay healthy.

Closed: the 91 s Gemma single-PS5 load was container creation on the USB stick. As a host process: 63.8 and 66.7 s cold, 22.5 s warm, 41.9 tok/s, text identical.

Settled: the progress source is the fork's `load_progress` log line (works cold and warm at commit `e43b711ab`).

Settled in review: ports 50060 and 50061; one Dockerfile builds `rpc-server` and `llama-server` from one fork commit and the image tag carries that commit; build allowlist instead of equality; no second `rpc-server` in step 2.

## 11. Test plan before anything replaces today's mode

1. Agent API against a fake `llama-server` on a development machine: every refusal, lease expiry, stop with a wrong id, watchdog, agent restart during a launch, start failure.
2. Scheduler against a fake agent in its end-to-end tests: start, progress, ready, routing, exit reasons, lost agent.
3. On one PS5: Qwen 4B and Gemma 12B PS5-hosted through the scheduler; output identical to host machine-hosted; decode and load time recorded as benchmark runs in the scheduler.
4. Failure drills on one PS5: stop the scheduler during a load (lease must free the PS5); a model that does not fit (must be refused before start); kill `llama-server` (instance must fail with the reason).
5. Only then enable `host: auto` for single-PS5 profiles.
