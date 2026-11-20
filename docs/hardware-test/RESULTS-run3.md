# llm-scheduler: third on-hardware test (PS5s resumed)

Date: 2026-11-17/18. Build `eb268e1` (image `llm-scheduler:test`, version `test-eb268e1`, same image as run 2).
Test window (llama-swap stopped): 23:37:14 to 00:23:06 UTC (46 min). Scheduler on host port 3199.
Logs and configs: raw logs are not kept in the repository. Everything is MEASURED unless marked "inferred".

## Verdict

**Pass. Every planned test (B to G) ran on the real PS5s, and the scheduler behaved as the design says. No new scheduler defect. The remaining problems are on the worker or PS5 side (slow, bursty uploads).**

- The scheduler found the Nvidia laptop from the fleet list alone (no `node_overrides` in the config).
- The PS5 `payload_version` (`e728c4d3dc1f-4ff2ab52`) and `rpc_port` (50052) were identical at every pre-test check (13 checks, `ps5-checks.txt`). No node dropped. New rule 7 never fired.
- Run 2 defect 4 (low first budget after a node returns) did not reappear, but no node was lost in this run, so it was not really re-tested.
- Upload speed is the one real finding: PS5 uploads are slower and burstier than `link_mb_s: 8` suggests (see "Upload rate"). The computed load timeout (1920 s for Gemma) was adequate: the slowest load took 669 s.

## Config checks

- Nvidia laptop from the fleet only: `config-run3.yaml` has no `node_overrides`. The fleet reports `ip: 192.0.2.3`, `rpc_port: 50052`. Log lines: `pre-flight ok profile=gemma-4-12b:ps5-aw node=02:00:00:00:00:11 addr=192.0.2.3:50052 free_mib=7509 total_mib=7842` and `placement chosen profile=tinyllama nodes=[02:00:00:00:00:11]`. `/api/status` showed `addr 192.0.2.3:50052` at start.
- Coordinator's tier change: from 00:00:28 UTC the config (`config-run3b.yaml`, later `config-run3c.yaml`) has two exact-hostname PS5 tiers (`console-1`, `console-2`, both priority 1, same settings). `--check-config` passed. After the restart `/api/status` listed exactly four usable nodes: the two PS5s (priority 1), the Nvidia laptop (2) and the host machine (3).
- Third PS5: none appeared. The fleet sampler (every 3 s, 909 samples, `fleet-sample.jsonl`) shows only `console-1` and `console-2`, and the fleet list at 00:00 and 00:05 had four members. Nothing could have been placed on a third node.
- The first F and G auto tests (23:53 to 23:57) ran under the old `console-*` tier because the tier message reached me after they had finished. I repeated F and G under the new tiers (files `F-*` and `G-*`; the earlier runs are `F-oldtier-*` and `G-oldtier-*`). Both gave consistent outcomes.

## Numbers

"Load" is the scheduler's `load_duration`. The PS5 workers keep a tensor cache, so most loads were partly or fully cached. The only all-cold load was Gemma on .122 (11 min).

| Test | Node(s) | Load | Tokens/s (3 x 200 tokens) | Device MiB (`/api/status`) |
|---|---|---|---|---|
| B: gemma 8K, first load after the payload rollout | PS5 .101 | 79.9 s (mostly cached) | 29.2 | RPC0 7514 (model 6777, KV 608, compute 130) |
| B: gemma 8K, reload after unload | PS5 .101 | **278.9 s** once; later reloads at 00:20 took 32.0 s and 27.5 s | 29.1 to 29.2 | 7514 |
| B2: gemma 8K, cold | PS5 .122 | **669 s (11 m 9 s)** | 29.1 to 29.2 | 7514 |
| B2: same, reload after unload | PS5 .122 | 141.9 s | 29.1 to 29.2 | 7514 |
| C: qwen 4B (cached from run 1, so warm) | PS5 .122 | 8.2 s; reload 7.6 s | 39.6 to 39.9 | RPC0 3343 (model 2741, KV 512, compute 90) |
| D: gemma pooled, budget split `13895,13897` | PS5 .122 + .101 | 26.7 s; reload 21.0 s | 12.3 to 12.7 | .122: 3811, .101: 4019, total 7830.5 |
| D: gemma pooled, forced 50/50 | PS5 .122 + .101 | 21.0 s | 12.7 to 13.0 | 3811 + 4019 (identical to the budget split) |
| E: gemma pooled | PS5 .122 + Nvidia laptop | 62.6 s | 18.4 to 19.2 | .122: 4931, Nvidia laptop: 2900, total 7830.5 |

Notes:

- For two equal PS5s the budget-derived split is 13895:13897, effectively 50/50, so the two D rows have the same memory. The load times differ (26.7 s vs 21.0 s) because of cache state, not the split.
- Pooling two PS5s gives lower generation speed (12.5 tok/s) than one PS5 (29.2) and than PS5 + Nvidia laptop (18.8). One PS5 is the right place for Gemma 8K, and that is where the placer puts it.
- Gemma pooled sums to 7830.5 MiB, the same as run 1 and 2, so `needs_mib: 7850` holds on PS5s.
- host machine GPU stayed at 10 MiB after every PS5 and PS5 + Nvidia laptop load (`gpu_loaded 10 MiB` in each test file).

## Upload rate

Measured from the scheduler container's `eth0` tx bytes (5 s samples, `tx-sample.txt`), because the fleet's `vram_used` jumps to the full buffer size at the start of a load and does not show progress. The counter covers all uploads of the container: single-load figures are exact for B and B2; D and E figures are combined across the two workers.

| Window | Bytes sent | Duration | Average |
|---|---|---|---|
| Gemma cold on PS5 .122 (00:06:04 to 00:17:13) | 3424 MB | 669 s | **5.1 MB/s** |
| Same, minute by minute | | | 38.8 MB/s in minute 1 (about 2.3 GB), then 2.2, 2.4, 2.4, 3.9, 2.4, 2.5, 1.9, **0, 0, 0**, then 1.2 MB/s |
| Gemma reload, PS5 .122 | 1119 MB | 142 s | 7.9 MB/s |
| Gemma reload, PS5 .101 (23:39 to 23:44) | 1126 MB | 283 s | 4.0 MB/s, **no data for about 4 minutes** (23:39:34 to 23:43:44), then about 1 GB at 40 to 60 MB/s |
| Both PS5s, pooled loads (D, F) | about 1.1 to 1.2 GB per load | 20 to 25 s | 49 to 56 MB/s combined while sending |
| PS5 + Nvidia laptop (E) | 4286 MB | 60 s | 71 MB/s (the Nvidia laptop share dominates) |

The pattern is not a steady 14 MB/s as in run 1. It is a fast initial burst (40 to 60 MB/s), a slow trickle (2 to 4 MB/s), and sometimes stalls of 3 to 4 minutes with nothing on the wire. Every Gemma load on a PS5 sent about 1.1 GB even when the rest was cached.

Two concurrent cold uploads: **not measured** (every pooled test found at least one half already cached). The single-PS5 cold rate is 5.1 MB/s average over the whole load; `link_mb_s: 8` gave a 1920 s deadline and the real load took 669 s, so the timeout was adequate. I am not recommending a change.

## F: PS5-only auto placement (allow_tiers PS5), new tiers

| Step | Observed | Design 7.3 / 8.3 says | Result |
|---|---|---|---|
| gemma:t1 | placed on .101 (budget about 12.5 GB, need 7850), ready 00:01:19 (39.5 s) | best tier, smallest budget that fits | pass |
| qwen:t1 | placed on .122 (the other PS5), ready 00:01:28.7 (7.6 s) | nodes are exclusive, so the free PS5 | pass |
| tinyllama:t1 | both PS5s held, so it waited; claim at 00:01:58.684 with victim qwen only (qwen hold = clamp(7.6 s, 30, 900) = 30 s, ended 00:01:58.7; gemma's hold of 39.5 s ended 00:01:58.8). tinyllama loaded on .122 in 7.6 s | wait until a hold expires, then evict the evictable instance | pass |
| gemma:2ps5 (forced, both PS5s) | claim at 00:02:39.405 = tinyllama ready + 30 s; victims gemma:t1 and tinyllama:t1 stopped together; pooled model ready 00:03:03 (21.0 s) | every holder on claimed nodes is a victim | pass |

In the first F run (old tier, 23:53 to 23:55) the order was gemma .101, qwen .122, and tinyllama evicted **gemma**, because Gemma's 38.5 s hold ended at 23:54:36.7 before qwen's 30 s hold ended at 23:54:37.5. Both runs follow the hold times exactly. Because nodes are exclusive, a tiny model evicts a large idle one; this is by design (design section 4, "Two models on one node" is a non-goal).

## G: burst of 8 mixed requests, auto placement, all tiers, PS5s available (new tiers, 00:03:26)

Requests: gemma x2, qwen x3, tinyllama x3, 0.5 s apart.

| Model | Placement | Evidence | Request times |
|---|---|---|---|
| gemma-4-12b | **PS5 .101** | `placement chosen nodes=[02:00:00:00:00:01] budgets=[12523]`, ready after 40.8 s | 42.6 s and 43.1 s, 29.1 tok/s |
| qwen3.5-4b | **PS5 .122** | `nodes=[02:00:00:00:00:02]`, ready after 13.9 s | 15.6 to 19.6 s, 33.5 to 40.3 tok/s |
| tinyllama | **Nvidia laptop** (tier 2) | `nodes=[02:00:00:00:00:11]`, ready after 9.9 s | 8.4 to 12.3 s, 46 to 74 tok/s |

All 8 requests got 200. The order matches the design: PS5s first, then the Nvidia laptop, the host machine untouched. The old-tier run (23:56) gave the same placements (Gemma 38 s). No eviction was needed.

## Lifecycle logs seen in this run

`queue entry created`, `placement chosen`, `pre-flight ok`, `child started`, `instance ready ... load_duration=...`, `instance stopping/stopped` (reasons admin and evicted), `claim made ... victims=[...]`. No `pre-flight failed`, `node lost`, `quarantined` or `queue timeout` lines occurred, because those conditions did not arise (they were verified in run 2).

## Defects

No new scheduler defects. Open items:

1. **PS5 uploads are slow, bursty and sometimes stall (worker or PS5 side, not the scheduler).**
   - Did: a cold Gemma load on PS5 .122, and a reload of Gemma on PS5 .101.
   - Saw: 669 s for the cold load, with three consecutive minutes of zero bytes on the wire and 2 to 4 MB/s otherwise. On .101 a reload took 279 s with no data for about 4 minutes, then a burst of about 1 GB. The same reload repeated at 00:20 took 32 s and 27 s, so the stall is intermittent. The fleet showed `rpc_port: 50052` and `vram_used` 6.63 GB throughout, so the worker was alive but waiting. The scheduler cannot see why.
   - Expected: steady throughput (14 MB/s in run 1) with no long idle gaps.
   - Log lines: `child started ... 23:39:23` then `instance ready ... load_duration=4m38.858609983s`; and `load_duration=11m9.512837018s` for 00:06:04 to 00:17:13 (`scheduler-run3-all.log`).
2. **Low (carried from run 2): `devices=3` / `devices=4` in the `instance ready` log** for one RPC device and for a two-device pool. Same cause as before (host rows counted). Seen in every ready line here.
3. **Low (carried from run 2): a forced node that is not in the fleet waits the whole `queue_timeout`.** Not re-tested here.
4. **Run 2 defect 4 (low first budget after a node returns): not reproduced.** No node dropped, so there is no `vram_used` capture for it. The lowest budget seen at placement was 12462 MiB (PS5).

## Not tested

- Two concurrent cold uploads, and a second all-cold PS5 load (PS5 caches cannot be cleared; I may not touch the PS5s). The Nvidia laptop cache was also warm.
- Failure paths (node lost, quarantine, queue timeout): not repeated; verified in run 2.
- Qwen cold on a PS5: it was already cached on .122 (cold was 216 s in run 1).
- Inferred, not measured: that the 3 to 4 minute stalls are on the worker side. The data only shows an idle wire while the worker stayed alive.

## End state

- llama-swap: started 00:23:06 UTC; `/running` answers `{"running":[]}`, `/v1/models` lists 15 models.
- Compose project `llm-scheduler-test` removed (container and network). `~/llm-scheduler-test` and the image `llm-scheduler:test` (eb268e1) left in place. Run-2 logs are in `~/llm-scheduler-test/logs-run2`.
- host machine GPU: 10 MiB. No `llama-server` processes.
- PS5s from the fleet list at the end: .101 and .122 both online, `rpc_port: 50052`, `payload_version: e728c4d3dc1f-4ff2ab52`, `vram_used` 0.02 GB.
- No sudo, no firewall change, no source change, no commit.

## Files

The run produced (raw logs are not kept in the repository): `config-run3.yaml`, `config-run3b.yaml` (exact-hostname PS5 tiers), `config-run3c.yaml` (adds `gemma-4-12b:ps5b`), `scheduler-run3-all.log`, `ps5-checks.txt`, `fleet-sample.jsonl`, `tx-sample.txt`, per-test `*.txt`, `*-chat.jsonl`, `*-timeline.jsonl`, `status-*.json`, and the driver scripts.
