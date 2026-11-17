# llm-scheduler: first on-hardware test

Date: 2026-11-17. Build: `ae8eccb` (branch `design/scheduler-v1`), image `llm-scheduler:test` (version `test-ae8eccb`).
Test window (llama-swap stopped): 15:45:32 to 16:33:45 UTC (48 min). Scheduler ran on host port 3199 as `llm-scheduler-test`.
Raw evidence: `logs/` (file names below). Configs: `logs/config-v1.yaml` to `logs/config-v4.yaml`.

## Verdict

**The scheduling logic works on real hardware. Fix 2 defects before cutover.**

- Placement, tiers, parallel loads, queueing, hold, drain, eviction, idle unload, pre-flight quarantine, backoff, clean stop and the llama-swap-compatible endpoints all behaved as the design says.
- **Defect 1 (high):** per-device memory figures are always empty. llama.cpp at this commit hides the buffer-size lines unless it runs with `-lv 4`. Workaround used: `extra_args: ["-lv", "4"]`.
- **Defect 2 (medium):** every child initialises CUDA on the OMEN, even when it uses only RPC workers. Each one takes about 102 MiB of OMEN VRAM. This made a Gemma load on the OMEN fail with out of memory.
- **Not tested on PS5s beyond the first loads.** At about 16:09 the fleet console pushed payload `f2cee5fc5f66` to both PS5s. Their `rpc-server`s died in the middle of our Gemma uploads and had not come back by the end of the window (`rpc_port: null`). The PS5 pool tests (E) and the PS5 warm reloads (C) could not be run. Later tests used the Alienware and the OMEN.

## Headline numbers

| Model (file) | Node(s) | Load (cold) | Load (warm) | Gen tok/s | Device MiB (load log) | Scheduler need |
|---|---|---|---|---|---|---|
| tinyllama (0.67 GB) | PS5 .101 | 8.4 s | 7.4 s | 142 | (not captured: defect 1) | est. 682 |
| tinyllama | Alienware | 9.1 s | 6-7 s | 100-157 | 696 | est. 682 |
| tinyllama | OMEN (CUDA0) | 0.5 s | - | 341 | 696 (nvidia-smi +819) | est. 682 |
| qwen3.5-4b (3.0 GB) | PS5 .122 | 216 s | not run (PS5 down) | 38.9 | 3343 | est. 4986 |
| qwen3.5-4b | Alienware | 39.0 s | 7.6 s | 19-20 | 3343 | est. 4986 |
| qwen3.5-4b | OMEN (F7) | ~2 s | - | 74-84 | - | est. 4986 |
| gemma-4-12b 8K (7.1 GB) | PS5 .101 | did not finish: worker died at 20:58 | - | - | 6777 model buffer allocated | 7800 |
| gemma-4-12b 8K | OMEN alone | 1.9 s | - | 33.3-38.6 | 7514 (nvidia-smi 7638) | 7800 |
| gemma-4-12b 8K | Alienware + OMEN, budget split 7301:7885 | 10.7 s | - | 18.8 | 7831 (3679 + 4152) | forced |
| gemma-4-12b 8K | Alienware + OMEN, 50/50 | 11.2 s | - | 17.0 | 7831 (3811 + 4019) | forced |
| gemma-4-12b:64k q8 + FA | Alienware + OMEN | 10.7 s | - | 17.9 | **8578** (4053 + 4525) | 14000 placeholder |

Effective transfer rates:

- PS5, cold: qwen sent 3.0 GB in 216 s, which is **13.9 MB/s**. That is above `link_mb_s: 11`.
- PS5 under contention: Gemma to .101 managed less than 5.6 MB/s on average, in bursts. The socket send queue sat still at 1.6 to 3.7 MB for 10 s or more at a time.
- Alienware: qwen cold load gives **77 MB/s**.
- tinyllama on PS5 .101 loaded at 80 MB/s, almost certainly from the worker's tensor cache.
- Tensor cache effect, measured on the Alienware: qwen loaded in 39.0 s cold and **7.6 s warm** (5x faster).

## A. Bring-up

| Check | Expected | Observed | Result |
|---|---|---|---|
| Image build | builds | 15.7 s (cached Go layers); 26.7 GB on disk, 11.1 GB content (`build.log`) | pass |
| `--check-config` | exit 0 | `config ok: 17 profiles` (24 in v3/v4) | pass |
| Container starts | healthy | healthy; start log = warnings + `llm-scheduler started` | pass |
| Fleet console reachable | design says a ufw rule is needed | reachable from 172.30.0.0/24 **without** a rule (curl inside the container returned the JSON). **No ufw rule added.** | pass (design 3.2/10 claim is wrong for this box) |
| `/health`, `/v1/models`, `/running` | 200, profiles, `{"running":[]}` | as expected | pass |
| `/api/status` nodes | 2 PS5s, Alienware (override), OMEN | all 4; tiers 1/1/2/3; view age 0.1 s (`status-A-startup.json`) | pass |
| PS5 budget vs `--list-devices` | similar | fleet `total` 14438, `free` 13650/13680, live budget 12626/12656; probe 14947 total / 14927 free | ok (fleet is about 1.3 GB more cautious) |
| Alienware budget | close to the probe | fleet `total` **8192** (the hub sends `8`, rounded up, not truncated), live budget 7649; probe 7842 total / 7557 free, so 7301 is the real budget | over by 348 MiB until the pre-flight corrects it |
| OMEN budget | nvidia-smi | total 8151, budget 7885; llama.cpp can use only **7707** | over by 444 MiB (see D) |

## B. `POST /api/plan` before any load (`plan-B.txt`)

| Profile | Plan | Matches tier rules? |
|---|---|---|
| tinyllama, qwen3.5-4b, gemma-4-12b, the `:t1` profiles | start on ps5-9032a330 (budget 12626, the smaller of two equal PS5s) | yes (PS5 first; smallest budget that fits) |
| gemma-4-12b:64k (14000 placeholder) | pool of both PS5s | yes (no single node fits) |
| forced profiles | exactly the forced nodes, in the written order | yes |
| tinyllama:huge (99999) | `reject`: "exceeds cluster capacity 42659" | yes |
| tinyllama:nonode | `queue`: "forced nodes unavailable: no-such-node" | as designed (see defect 4) |
| unknown model | 404 `model_not_found` | yes |
| Estimates | tinyllama 682, qwen 4986, gemma 8K 12040, gemma 64K 29096 | Gemma estimates are 1.5x (8K) and 3.4x (64K) the measured figure, as the design warns |

## C. First loads

| Item | Observed | Result |
|---|---|---|
| tinyllama | went to PS5 .101; `cmd` as in design 7.4 (`--rpc 10.0.0.101:50052 --device RPC0`) | pass |
| qwen + gemma requested at the same moment | started in the same millisecond on **different** PS5s (qwen on .122, gemma on .101) | pass (F1) |
| qwen on PS5 | ready after 216 s; the client's time to first token was 217.9 s | pass |
| gemma on PS5 | still loading after 20 min. Then `Remote RPC server crashed or returned malformed response` (the worker went away, see verdict). The client got a 502 carrying the child's tail; the profile was backed off for 60 s | worker event; the scheduler handled it correctly |
| Warm reload | Alienware qwen 39.0 to 7.6 s; PS5 tinyllama 8.4 to 7.4 s (probably already cached). PS5 qwen/gemma warm: not run | partial |
| Load timeout | gemma deadline = 120 + 2 x 7.1 GB / 11 MB/s = 1411 s. The PS5 upload had not finished after 1258 s | timeout is too tight under contention (see tuning) |

## D. Memory: truth vs estimate

| Profile | Scheduler need | Load-log sum | Fleet change | nvidia-smi (OMEN) |
|---|---|---|---|---|
| tinyllama | est. 682 (fit rule: 682 x 1.05 + 600/device) | 696 | Alienware +0.68 GB | process 814 MiB |
| qwen3.5-4b | est. 4986 | 3343 | PS5 .122 +3.18 GB | - |
| gemma 8K, 1 device | 7800 | 7514 (model 6777, KV 608, compute 130) | PS5 .101 +6.63 GB while loading | process 7638 MiB (load log + 124 MiB CUDA context) |
| gemma 8K, 2 devices | 7800 | 7831 | Alienware +3.6 GB | - |
| gemma 64K q8, 2 devices | 14000 placeholder | 8578 (KV 799, compute 535 + 467) | Alienware +3.97 GB | 4660 MiB |

Findings:

- **Does 7800 hold for gemma 8K?** On one device, yes: 7514 + 124 MiB context = 7638. On two devices it is 31 MiB short (7831). **Use 7850.**
- **The OMEN cannot actually take a 7800 need.** llama.cpp can use 7707 MiB, not 8151. Gemma 8K on the OMEN alone has only about 69 MiB to spare. With one other child's 102 MiB context on the GPU it fails with OOM (`D-gemma-local-oom.json`: `cudaMalloc failed: out of memory` on the 129.5 MiB compute buffer). The scheduler's budget, 7885, says it fits.
- **The OMEN tier's `headroom_mib` must cover the 444 MiB gap** (8151 vs 7707), so 450. Each child that uses the OMEN GPU also adds about 120 MiB of CUDA context that the load log does not show.
- **gemma 64K:** 8578 measured on 2 devices. One device would be about 8100 (one compute buffer). **Use `needs_mib: 8600`.** With that, the plan puts 64K on a single PS5 (`plan-v4.txt`), not a pool.

## E. Pools

| Test | Observed | Result |
|---|---|---|
| Both PS5s (budget split and 50/50) | not run: PS5 `rpc-server`s down from 16:09 | not tested |
| PS5 + Alienware (forced, `--rpc 10.0.0.122:50052,10.0.0.3:50052 --device RPC0,RPC1 --tensor-split 13895,7301`) | command correct; it evicted idle qwen first; model buffers RPC0[.122] 4258 + RPC0[.3] 2519 MiB (63/37, matching the split, each on the right endpoint); then the PS5 worker died | command/split pass, load not finished |
| RPC-only placement uses no CUDA0 buffers | the load log has only `CPU_Mapped` and `RPC0[...]` buffers. But each child holds a 102 MiB CUDA context on the OMEN | pass, with defect 2 |
| Alienware + OMEN (`--device RPC0,CUDA0`) | layers on both, in proportion; 18.8 tok/s (budget split), 17.0 (50/50), vs 33-38 on the OMEN alone | pass |
| tinyllama forced local | `--device CUDA0`, no `--rpc`; 0.5 s load; 341 tok/s | pass |

## F. Scheduling behaviour

| # | Test | Observed | Result |
|---|---|---|---|
| 1 | Two models at once, two PS5s free | qwen and gemma loaded in parallel on different PS5s | pass |
| 2 | Both PS5s full, then a third model | tinyllama went to the Alienware (tier 2) in 9.1 s (`status-F2-...json`) | pass |
| 3 | Tier-restricted: third model must queue, then evict after hold (run on tier 2, because the PS5s were down) | `qwen3.5-4b:t2` ready 16:18:07.5, hold 39 s. `tinyllama:t2` unclaimed from 16:18:30, claimed at **16:18:46.9** (= ready + 39 s), victim stopped, 8 s settle, loading at 16:18:54.9 (`F3-timeline.jsonl`) | pass |
| 3b | Same with design default timers (v4) | tinyllama past its hold (30 s): evicted at once, qwen loaded warm in 7.6 s (`F3-defaults-timeline.jsonl`) | pass |
| 4 | Drain: 800-token stream on gemma (OMEN), then a request needing the OMEN | entry unclaimed 16:23:49; gemma **draining** at 16:24:05 (hold 20 s + drain_after 15 s); the stream finished all **800 tokens** (23.9 s); gemma stopped 16:24:09; tinyllama ready 16:24:12 (`F4-timeline.jsonl`, `F4-long.json`) | pass |
| 5 | Idle unload after ttl | tinyllama (ttl 120 s) on the Alienware: last used 15:57:03.8, gone by 15:59:07; log line `idle unload ... idle=2m0.0s`; Alienware VRAM back to 0.28 GB | pass |
| 6 | Client leaves while queued (`curl -m 5`) | queue empty afterwards; the protected model was not evicted; the queued model was not loaded (`status-F6-after-disconnect.json`). Same for `tinyllama:nonode` | pass |
| 7 | Burst: 10 parallel requests over tinyllama, qwen and tinyllama:local | **10/10 got 200**, none hung. qwen 4.1-5.6 s, tinyllama 7.9-9.0 s, tinyllama:local 26.5 s (`F7-burst.jsonl`). The idle tinyllama:local was evicted for qwen (by design: idle and past its hold), then had to wait 20 s to win the OMEN back | pass (note: churn) |

## G. Failure handling

| # | Test | Observed | Result |
|---|---|---|---|
| 1 | Forced to a node name that does not exist | request queues (it would wait the full `queue_timeout`); plan reason "forced nodes unavailable: no-such-node" | not fast (defect 4) |
| 2 | Unknown model | 404 in 0.6 ms, OpenAI error shape | pass |
| 3 | Too big (`needs_mib: 99999`) | 503 at once: "needs 99999 MiB ... cluster can hold at most 42659 MiB" | pass (has `Retry-After: 30`, defect 7) |
| 4 | Dead worker address (v4 override `ps5-9032a330 -> 10.0.0.250:50052`) | pre-flight failed in 2.3 s ("Failed to connect"); node quarantined 30 s (shown in `/api/status`); instance returned and placed on the Alienware; **the client still got 200** in 10.6 s (`G4-timeline.jsonl`, `status-G4-quarantine.json`) | pass |
| extra | Child crashes during load (PS5 worker death) | 502 with the child's last lines, including the crash line; backoff 60 s, doubling to 120 s on the repeat OOM | pass |

## H. llama-swap compatibility

| Item | Observed | Result |
|---|---|---|
| `/running` shape | `{running:[{cmd,model,name,state}]}`; `starting`/`ready`; profile names only (`running-H.json`) | pass |
| Hub regexes on real `cmd` | `--rpc 10.0.0.101:50052`, `--rpc 10.0.0.122:50052`, `--ctx-size 8192/16384` extracted; `-lv 4` and `--cache-ram` do not confuse `-c` | pass |
| `/upstream/<model>/props`, `/slots` | 200 passthrough for loaded qwen and for `tinyllama%3At2` (encoded) and `tinyllama:t2` | pass |
| `/upstream` for an unloaded or starting model | 503 in under 1 ms, nothing loaded | pass |
| `/api/events` | `modelStatus` (every profile, stopped/starting/ready) on connect and on each change; `inflight` snapshot, then upsert `{id,model,timestamp}` and remove `{id}`; `activity` after the request (`events-F2.txt`) | pass |
| `/api/metrics/activity` | real token counts and tok/s from the stream (`activity-H1.json`, `activity-final.json`) | pass |

## I. Clean stop

`docker compose stop` with tinyllama:local loaded and idle: 0.59 s, **exit code 0**, log `shutting down` then `stopped`. No children left; OMEN GPU back to 10 MiB. The Alienware answered `--list-devices` straight after. The same happened on each `restart` (v2, v3).

## Defects

1. **High: per-device memory rows are always empty.**
   - Reproduce: load any profile with no `extra_args`. `/api/status` shows `memory: []`, `device_total_mib: 0`, `host_memory: []` (`status-C-tinyllama.json`).
   - Evidence: a hand-run child prints `verbosity = 3 (adjust with the -lv N CLI arg)` and only 8 lines. With `-lv 4` it prints 163 lines, including `load_tensors: CUDA0 model buffer size = 601.02 MiB`, `llama_kv_cache: CUDA0 KV buffer size`, `sched_reserve: CUDA0 compute buffer size`. Those match `bufferLineRE` as written.
   - Diagnosis: `internal/instance/command.go` `BuildArgs` never raises the verbosity. The comment in `loadlog.go` assumes these lines print by default. Fix: always add `-lv 4` (and treat `-lv`/`--log-verbosity` as a managed flag). `-lv 4` printed no debug (`D`) lines during the load. Generation was not checked by hand, but no log growth was noticed during the later runs.
   - Note: the extra lines push the useful error out of the 502 tail. It still fits, but the tail is mostly `print_info` noise (`chat.jsonl`, status 502 rows).
2. **Medium: children that use only RPC workers still initialise CUDA on the OMEN.**
   - Effect: 102 MiB of OMEN VRAM per child (`nvidia-smi --query-compute-apps` lists 3 x 102 MiB with 3 RPC-only children).
   - Reproduce: load a model on the Alienware (`tinyllama:t2`), then force gemma 8K onto the OMEN. The result is a 502 with `cudaMalloc failed: out of memory` (`D-gemma-local-oom.json`). The same load succeeds when no other child runs (`status-D-gemma-local.json`).
   - Diagnosis: `internal/instance/proc.go` `Start` passes the scrubbed environment unchanged. When the placement has no local node, set `CUDA_VISIBLE_DEVICES=` as the pre-flight probe already does (`probe.go`).
3. **Low-medium: no log lines for the instance lifecycle.**
   - Only start, stop, `idle unload` and quarantine are logged. Nothing is logged for: load start, ready (with load_duration), load failure (502), backoff, claim, draining, eviction.
   - Reproduce: `docker logs` during any test (`scheduler-run*.log`). The timelines in this report came from polling `/api/status` every 1 to 3 s.
4. **Low (design gap): a forced node that is not in the fleet queues instead of failing.**
   - Reproduce: `tinyllama:nonode`. The request waits until `queue_timeout` (600 to 900 s). Fail fast with 503 when a forced name matches no member of a fresh view.
5. **Low: false sliding-window warning.** It fires for `force_nodes` profiles, which never use the estimate. `cmd/llm-scheduler/main.go` `warnSlidingWindow` / `config.ProfilesWithoutNeedsMiB`: skip forced profiles. There were 7 to 10 such warnings per start.
6. **Low: misleading plan/queue reason.** With one PS5 quarantined and the other without an address, the `tinyllama:t1` plan says "need 682 MiB ... does not fit the available nodes". It should say quarantined / no address.
7. **Low: wrong `Retry-After` on a permanent error.** The 503 "does not fit" sends `Retry-After: 30`, but retrying cannot help.

Not scheduler defects, but the owner should know:

- **The PS5 `rpc-server`s died mid-upload at 16:09:18 (.122) and 16:09:39 (.101).** The fleet console log shows it serving bundle `f2cee5fc5f66-4ff2ab52` to both PS5s, and their `payload_version` changed to it between 16:10:39 and 16:11:24. After that, `rpc: null` and `rpc_port: null`, and the probe aborts (port closed). The timing points to the payload push, but I cannot prove it: I was not allowed to look at the PS5s.
- **The Alienware's fleet `vram_total` is 8 GB, rounded up from 7842 MiB.** The design says the hub truncates. The pre-flight override corrects it, but plans made before a pre-flight are optimistic.
- **The design's firewall claim is wrong for this box.** The container reached `host.docker.internal:8090` with no ufw rule.

## Tuning recommendations

```yaml
tiers:
  - match: { hostname: "ps5-*" }
    link_mb_s: 8          # measured 13.9 MB/s alone, <5.6 MB/s while two PS5 uploads ran; 11 timed gemma at 1411 s, the upload was unfinished at 1258 s
  - match: { hostname: "alienware*" }
    link_mb_s: 60         # measured 77 MB/s (qwen cold 3.0 GB / 39 s)
  - match: { local: true }
    headroom_mib: 450     # nvidia-smi 8151 vs llama.cpp-usable 7707 MiB (444 gap); keep defect 2 fixed too
profiles:
  gemma-4-12b:      { needs_mib: 7850 }   # measured 7514 (1 device, +124 CUDA ctx) / 7831 (2 devices)
  "gemma-4-12b:64k": { needs_mib: 8600 }  # measured 8578 on 2 devices (KV 799 q8)
  qwen3.5-4b:       { needs_mib: 3400 }   # measured 3343; estimate 4986 (fit rule asks 5835)
  tinyllama:        { needs_mib: 700 }    # measured 696
  # until defect 1 is fixed, on every kind: llama profile:
  #   extra_args: ["-lv", "4"]
```

- `min_hold 20s / max_hold 60s / drain_after 15s` made the tests quick and caused no interrupted request. Keep the design defaults for production: the default run (v4) also behaved correctly.
- `qwen3.5-4b` runs at 74-84 tok/s on the OMEN, 38.9 on a PS5 and 19-20 on the Alienware. That makes design open question 3 (`prefer_tiers`) worth doing.

## State of the box afterwards

- llama-swap: `docker start llama-swap` at 16:33:45 UTC. `/running` returns `{"running":[]}` (200); `/v1/models` lists the usual 15 models (`after-state.txt`).
- Scheduler: compose project `llm-scheduler-test` is down; its container and network `llm-scheduler-test_sched` are removed. There are no `llama-server`/`sd-server` processes on the host; OMEN GPU at 10 MiB.
- Workers: the Alienware answers `--list-devices` (7842 / 7557 MiB free): no client attached. **Both PS5s refuse the connection** (`rpc_port: null` in the fleet) since the 16:09 payload push. That was not caused by this test, and I did not touch them.
- Left on the box: `~/llm-scheduler-test/` (source, config, compose, logs) and the image `llm-scheduler:test` (26.7 GB on disk, mostly shared base layers). Delete with `rm -rf ~/llm-scheduler-test && docker image rm llm-scheduler:test`.
- No firewall rule was added. No sudo was used. Nothing under /opt, /srv or /etc was changed.
