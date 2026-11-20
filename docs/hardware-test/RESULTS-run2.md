# llm-scheduler: second on-hardware test (cut short)

Date: 2026-11-17. Build: `eb268e1`, image `llm-scheduler:test` (version `test-eb268e1`).
Test window (llama-swap stopped): 23:16:24 to 23:33:53 UTC (17.5 min). Scheduler on host port 3199, project `llm-scheduler-test`.
Logs and configs: raw logs are not kept in the repository. Everything below is MEASURED unless it says "inferred".

## Verdict

**The two fixes work on hardware. The PS5 tests (B, C, D, E-with-PS5, F) were NOT run: hard rule 7 stopped them.**

- Fix 1 (per-device memory at `-lv 4`): confirmed. Every load shows per-device MiB in `/api/status`, and the ready log line carries `memory_mib=`.
- Fix 2 (no host machine CUDA context for remote-only children): confirmed. host machine GPU stayed at 10 MiB with remote-only models loaded and in use. Run 1 showed about 102 MiB per child.
- Lifecycle logs: confirmed. Quoted below.
- **PS5 stop (rule 7):** at 23:22:22 UTC I read the fleet list. Both PS5s now report `payload_version` `e728c4d3dc1f-4ff2ab52`, not `f2cee5fc5f66-4ff2ab52`. `rpc_port` is 50052 on both, so this is not the run-1 failure, but the rule says to stop on a version change. I stopped all PS5 loads, recorded the time, and did not try to fix anything. The first Gemma load on PS5 .101 (started 23:16:52) was killed by the scheduler as "node lost" at 23:19:57, so the worker or the PS5 dropped off the fleet from about 23:19:27 to about 23:21:30, which is when the new payload must have gone in. The fleet console shows a new bundle again, as in run 1. I did not look at the PS5s or the console container.
- Everything after that ran with the two PS5s deliberately pointed at dead addresses (`node_overrides` 192.0.2.250 and 192.0.2.251), so the scheduler could not use them. That also tested quarantine. Auto-placement tests therefore ran over Nvidia laptop and host machine only.

## Numbers

| Test | Node(s) | Load (first in run) | Load (after unload) | Gen tok/s | Device MiB (from `/api/status`) | host machine GPU (nvidia-smi) |
|---|---|---|---|---|---|---|
| A: tinyllama | Nvidia laptop only (remote-only) | 7.15 s | 7.15 s | 83-92 (3 reqs, 200 tokens) | RPC0 696.0 (model 601.0, KV 44, compute 51.0) | 10 MiB before, 10 MiB loaded |
| A: qwen3.5-4b | Nvidia laptop only (remote-only) | 7.65 s | 7.66 s | 18.9-19.1 | RPC0 3342.8 (model 2740.8, KV 512, compute 90.0) | 10 MiB before, 10 MiB loaded |
| A: gemma-4-12b 8K, pooled, budget split `7253,7691` | Nvidia laptop + host machine | 11.2 s | 11.2 s | 16.9-17.0 | RPC0 3679.0, CUDA0 4151.5, total 7830.6 | 10 before, 4292 loaded (one process, 4274) |
| D/E: gemma 8K, pooled, forced 50/50 | Nvidia laptop + host machine | 11.4 s | not run | 16.8-17.2 | RPC0 3811.3, CUDA0 4019.2, total 7830.6 | 10 before, 4160 loaded (one process, 4142) |
| A: gemma 8K, single PS5 .101 (killed run) | PS5 .101 | never ready (see defect 1) | - | - | RPC0 6776.8 model buffer, read from `/api/status` at 23:17:12 while loading | 10 MiB while it loaded |
| Burst G (8 mixed requests, auto, PS5s dead-addressed) | Nvidia laptop + host machine | see below | - | qwen 68-84, tinyllama 83-136, gemma 19.8 | - | - |
| F analog on tier 2 only (Nvidia laptop) | Nvidia laptop | qwen 7.7 s; tinyllama 7.2 s after the eviction | - | - | - | - |

Notes on the numbers:

- The Nvidia laptop loads are fast (7 s) because that rpc-server holds the tensors in its cache from run 1. This is a warm number, not a cold one.
- Gemma pooled sums to 7830.6 MiB in both splits, matching run 1 (7831). The tuned `needs_mib: 7850` holds.
- Pooled host machine GPU use (4292 MiB total, 4274 in the one process) is the model share plus its CUDA context. No extra idle child contexts appeared.
- Not measured at all: Gemma cold and warm on a PS5, Qwen on a PS5, both PS5s pooled (either split), PS5 + Nvidia laptop, PS5-only auto placement and eviction. PS5 transfer rates are therefore not updated.

## A. The fixes

1. **Memory readings.** `/api/status` per-device rows are filled for single and pooled loads, plus `host_memory` rows (CPU_Mapped, CPU). Fully measured for Nvidia laptop single, Nvidia laptop + host machine pooled (two splits). For a single PS5 I only have a mid-load reading (6776.8 MiB model buffer for Gemma on .101), because the PS5 load was stopped. Pooled PS5 readings: not available.
2. **host machine GPU memory with remote-only models.** nvidia-smi on the host machine: 10 MiB before, 10 MiB with tinyllama on the Nvidia laptop loaded and answering four requests, 10 MiB with qwen on the Nvidia laptop the same way. Also 10 MiB at 23:22 while the Gemma child for PS5 .101 was running. The `CUDA_VISIBLE_DEVICES=` override is in the code (`ChildEnvOverrides`) and visibly takes effect.
3. **Lifecycle logs.** All present. Lines from the run's scheduler log (raw logs are not kept in the repository):

```
level=INFO msg="queue entry created" profile=tinyllama:aw alias=false waiters=1
level=INFO msg="placement chosen" profile=tinyllama:aw nodes=[02:00:00:00:00:11] budgets=[7649] need_mib=682 measured=false waiters=1
level=INFO msg="pre-flight ok" profile=tinyllama:aw node=02:00:00:00:00:11 addr=192.0.2.3:50052 free_mib=7557 total_mib=7842 duration=120.516965ms
level=INFO msg="child started" profile=tinyllama:aw pid=99 port=5800 cmd="llama-server-rpc ... -lv 4 --rpc 192.0.2.3:50052 --device RPC0"
level=INFO msg="instance ready" profile=tinyllama:aw load_duration=7.148998847s memory_mib=696.03 devices=3 waiters=1
level=INFO msg="instance stopping" profile=tinyllama:aw reason=admin mode=graceful in_flight=0
level=INFO msg="instance stopped" profile=tinyllama:aw reason=admin loaded_for=7.638465329s requests_served=4 exit="exit code 0, stop requested"
level=WARN msg="pre-flight failed" profile=tinyllama node=02:00:00:00:00:01 addr=192.0.2.250:50052 kind=exit duration=3.27s ...
level=WARN msg="node quarantined" node=02:00:00:00:00:01 for=30s until=2026-11-17T23:29:55.124Z level=1 ...
level=INFO msg="instance returned" profile=tinyllama reason="pre-flight failed" nodes=[02:00:00:00:00:01] budgets=[] waiters=1
level=INFO msg="node released from quarantine" node=02:00:00:00:00:01
level=WARN msg="queue timeout" profile=tinyllama:nonode waited=30.000609512s reason="forced nodes unavailable" detail="forced node no-such-node is not in the fleet view" waiters=1
level=INFO msg="claim made" profile=tinyllama:t2 nodes=[02:00:00:00:00:11] victims=[qwen3.5-4b:t2] waiters=1
level=INFO msg="instance stopping" profile=qwen3.5-4b:t2 reason="evicted for tinyllama:t2" mode=graceful in_flight=0
level=INFO msg="claimed nodes no longer fit; claim dropped" profile=gemma-4-12b nodes=[02:00:00:00:00:11]
level=INFO msg="instance stopping" profile=tinyllama:t2 reason=ttl mode=graceful in_flight=0
level=ERROR msg="node lost" profile=gemma-4-12b:ps5a node=02:00:00:00:00:01 missing_for=30.000978481s in_flight=0 waiters=1
```

A load-failure (502) log line was not exercised, so it is untested. The unload reasons seen: admin, ttl, evicted, node loss, shutdown.

## Scheduling checks that ran (non-PS5)

| Test | Observed | Result |
|---|---|---|
| Quarantine on dead worker address (both PS5s) | `tinyllama` tried PS5 .101, pre-flight failed in 3.3 s, quarantined 30 s; tried PS5 .122, same; landed on the Nvidia laptop; client got 200 in 14.0 s. Both released after 30 s | pass |
| Forced node not in fleet (`tinyllama:nonode`, `queue_timeout 30s`) | 503 after 30.0 s, reason "forced nodes unavailable ... not in the fleet view" | pass, but not fail-fast (run-1 defect 4 still open, see defect 3) |
| Unknown model | 404 `model_not_found` in under a second | pass |
| Eviction after hold on tier 2 (F analog): qwen ready 23:32:07.75, tinyllama (needs inflated to 3000) requested 23:32:09.8 | claim made at 23:32:37.752 = ready + 30.0 s (hold = clamp(7.7 s, 30, 900) = 30 s); victim stopped; tinyllama placed 8 s later (Nvidia laptop settle); ready 23:32:52.98; ttl 20 s unload at 23:33:13 | pass, as design 8.3 |
| Burst of 8 (gemma x2, qwen x3, tinyllama x3) at 23:30:23 over auto placement, PS5s dead | qwen: first tried PS5 .122 (dead), fell to host machine, ready 1.1 s; tinyllama on the Nvidia laptop, ready 7.1 s. All qwen and tinyllama answered in 3.4-8.2 s. Gemma could not fit one node: it waited for the holds (30 s), evicted tinyllama (claim on Nvidia laptop alone did not fit, dropped at 23:31:11), then claimed Nvidia laptop + host machine and evicted qwen; pooled, ready 23:31:24.9 (load 11.2 s, 7830.6 MiB). Both gemma requests got 200 after 63.9 and 64.7 s. No hang, no 5xx | pass |

## Defects

1. **PS5 payload changes under the test (not a scheduler bug).**
   - What I did: started `gemma-4-12b:ps5a` (forced to PS5 .101) at 23:16:52, then read the fleet list after the scheduler logged "node lost".
   - What I saw: both PS5s switched to `payload_version` `e728c4d3dc1f-4ff2ab52` (was `f2cee5fc5f66-4ff2ab52`) in the window 23:16 to 23:22. The scheduler log: `23:19:57 ERROR node lost ... missing_for=30.0s`, the child was killed, and the load restarted at 23:21:33 when the node came back (`budgets=[5724]` in that placement, a transient low value). The rpc-server ports are back (`rpc_port: 50052`).
   - Expected: stable payload during the test. This is the same kind of event as in run 1. Someone or something is pushing payloads to the PS5s while tests run. Fix on the owner's side: stop the auto-push, or schedule tests around it.
2. **Low: `devices=3` in the "instance ready" log is wrong.**
   - Seen: `instance ready ... memory_mib=696.03 devices=3` for a single RPC device; `devices=4` for the two-device pool (RPC0 + CUDA0). The count appears to include the host rows (CPU_Mapped, CPU). Expected 1 and 2.
3. **Low (open from run 1, defect 4): a forced node that is not in the fleet still waits the whole `queue_timeout`.**
   - Seen: `tinyllama:nonode` 503 at exactly 30.0 s (the test timeout). With the default 600 s this is a 10 minute wait. The message is now clear: `forced node no-such-node is not in the fleet view`. Expected: fail at once when the name is not in a fresh view.
4. **Low: the first placement after a node returns uses a very low budget.**
   - Seen: `placement chosen ... nodes=[02:00:00:00:00:01] budgets=[5724] need_mib=12040` at 23:21:33, against about 12.5 GB normal. Probably the fleet's `vram_used` still included the previous child's upload at that moment. It did not stop the load (forced profile). Could not repeat it (PS5 tests stopped). Inferred, not confirmed.
5. **Cosmetic: a dead worker address pre-flight prints CUDA init noise.**
   - The quarantine log contains `ggml_cuda_init: failed to initialize CUDA: no CUDA-capable device is detected` plus an abort, because the probe runs with `CUDA_VISIBLE_DEVICES=`. The cause of the failure is `failed to connect` further down the long line; the first thing a reader sees is misleading.

I did not see any new fault in the scheduler itself on non-PS5 hardware.

## Not tested, and why

- B (Gemma 8K single PS5, cold and warm), C (Qwen 4B single PS5 warm), D (Gemma pooled on both PS5s, two splits), E with a PS5 (PS5 + Nvidia laptop), and F as specified (PS5-only tier, Gemma then Qwen then tinyllama): all stopped by hard rule 7 at 23:22:22 UTC because both PS5 `payload_version` values changed. A tier-2 analog of F ran instead (table above).
- G was run with auto placement over Nvidia laptop and host machine only (the PS5s were dead-addressed), not over all three tiers.
- Single-PS5 per-device MiB: only the mid-load figure (6776.8) from the killed load. Pooled-PS5 MiB: none. PS5 transfer-rate tuning (`link_mb_s: 8`) is unchecked. Load timeout on the PS5s unchecked (the profile deadline shown for the Gemma PS5 load was 1920 s at `link_mb_s: 8`, from the status, not exercised).
- A load-failure (502) lifecycle log was not triggered.
- No sudo was needed. No firewall change.

## End state

- llama-swap: started 23:33:53 UTC; `/running` answers `{"running":[]}`, `/v1/models` lists 15 models.
- Compose project `llm-scheduler-test` removed (container and network gone). `~/llm-scheduler-test` and image `llm-scheduler:test` (eb268e1) left in place. Run-1 logs moved to `~/llm-scheduler-test/logs-run1`.
- host machine GPU: 10 MiB. No `llama-server` processes.
- PS5s at the end (fleet list): .101 and .122 both `online`, `rpc: true`, `rpc_port: 50052`, `payload_version: e728c4d3dc1f-4ff2ab52`, `vram_used` 0.01 GB.

## Files

The run produced (raw logs are not kept in the repository): `config-run2.yaml` (base test config), `config-run2b.yaml` (dead PS5 addresses, Nvidia laptop profiles, 30 s queue timeout), `config-run2c.yaml` (900 s queue timeout), `config-run2d.yaml` (inflated needs for the eviction test, ttl 20 s), `scheduler-run2-all.log` (all scheduler logs, deduplicated), per-test `*.txt`, `*-chat.jsonl`, `*-timeline.jsonl`, `status-*.json`, `fleet-rule7.json`, `rule7-stop-time.txt`, and the driver scripts `lt.sh`, `chat.py`, `fastpoll.sh`, `poller.sh`.
