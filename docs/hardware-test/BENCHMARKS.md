# Benchmarks: what the hardware runs measured

This page merges the numbers from `RESULTS.md` (run 1), `RESULTS-run2.md` (run 2), `RESULTS-run3.md` (run 3) and `RESULTS-run4.md` (run 4, partial). Runs 1 to 3 ran on 2026-11-17 and early 2026-11-18 UTC.

**The first part of this page (the main table, upload rates and memory needs) was measured before the PS5 fixes of 2026-11-18** (tensor cache turned off, GPU clock pinned at 2230 MHz). Those PS5 rows used the worker's tensor cache, so the cold and warm loads and upload rates there do not apply any more. The numbers measured after the fixes are in the last part, "After the PS5 fixes (2026-11-18)".

How to read it:

- "Load" is the scheduler's `load_duration`, in seconds.
- Cold means the worker had nothing cached. Warm means the model was already in the worker's tensor cache. The results files say which runs were cached. Where they do not say, the cell says "cache state not stated".
- "Not stated" means the results file never gives that number.
- Tokens/s come from the test requests. Runs 2 and 3 used 3 requests of 200 tokens. Run 1 does not say.
- Where runs disagree, there is one row per run. The numbers are not averaged.
- .101 and .122 are the two PS5s, as the results files name them.

## Main table

| Model | Context | Placement (machines) | Cold load s | Warm load s | Tokens/s | Memory per device MiB | Run |
|---|---|---|---|---|---|---|---|
| tinyllama | not stated | 1 PS5 (.101) | 8.4 (about 80 MB/s, almost certainly cached) | 7.4 | 142 | not captured | 1 |
| tinyllama | not stated | Nvidia laptop | 9.1 | 6 to 7 | 100 to 157 | 696 | 1 |
| tinyllama | not stated | host machine | 0.5 | - | 341 | 696 (nvidia-smi +819) | 1 |
| tinyllama | not stated | Nvidia laptop | - | 7.15 (cached from run 1) | 83 to 92 | RPC0 696.0 (model 601.0, KV 44, compute 51.0) | 2 |
| tinyllama | not stated | Nvidia laptop (auto placement, burst of 8) | 9.9 (cache state not stated) | - | 46 to 74 | not stated | 3 |
| qwen3.5-4b | 16K (inferred from `--ctx-size 16384` next to the .122 command) | 1 PS5 (.122) | 216 | not run (PS5 down) | 38.9 | 3343 | 1 |
| qwen3.5-4b | as above | 1 PS5 (.122) | - | 8.2 (cached from run 1); 7.6 on a reload | 39.6 to 39.9 | RPC0 3343 (model 2741, KV 512, compute 90) | 3 |
| qwen3.5-4b | as above | 1 PS5 (.122), auto placement, burst of 8 | 13.9 (cache state not stated) | - | 33.5 to 40.3 | not stated | 3 |
| qwen3.5-4b | as above | Nvidia laptop | 39.0 | 7.6 | 19 to 20 | 3343 | 1 |
| qwen3.5-4b | as above | Nvidia laptop | - | 7.65 (cached); 7.66 after an unload | 18.9 to 19.1 | RPC0 3342.8 (model 2740.8, KV 512, compute 90.0) | 2 |
| qwen3.5-4b | as above | host machine | about 2 (cache state not stated) | - | 74 to 84 | not stated | 1 |
| qwen3.5-4b | as above | host machine (auto placement, burst of 8; the PS5 was dead-addressed) | 1.1 (cache state not stated) | - | 68 to 84 | not stated | 2 |
| gemma-4-12b | 8K | 1 PS5 (.101) | did not finish: worker died, still loading after 20 min | - | - | 6777 model buffer allocated | 1 |
| gemma-4-12b | 8K | 1 PS5 (.101) | never ready (load killed when the node dropped) | - | - | 6776.8 model buffer, read mid-load | 2 |
| gemma-4-12b | 8K | 1 PS5 (.101) | 79.9 (mostly cached; first load after the payload rollout) | 278.9 once, after a 4 minute stall; later reloads 32.0 and 27.5 | 29.1 to 29.2 | 7514 (model 6777, KV 608, compute 130) | 3 |
| gemma-4-12b | 8K | 1 PS5 (.122) | 669 (the only all-cold load in run 3) | 141.9 | 29.1 to 29.2 | 7514 | 3 |
| gemma-4-12b | 8K | 1 PS5 (.101), auto placement | 39.5 in the F test; 40.8 in the burst (cache state not stated) | - | 29.1 (burst) | not stated | 3 |
| gemma-4-12b | 8K | host machine alone | 1.9 | - | 33.3 to 38.6 | 7514 (nvidia-smi 7638, which adds 124 MiB of CUDA context) | 1 |
| gemma-4-12b | 8K | Nvidia laptop + host machine, budget split 7301:7885 | 10.7 | - | 18.8 | 7831 (3679 + 4152) | 1 |
| gemma-4-12b | 8K | Nvidia laptop + host machine, budget split 7253,7691 | - | 11.2 (cached) | 16.9 to 17.0 | RPC0 3679.0 + CUDA0 4151.5 = 7830.6 | 2 |
| gemma-4-12b | 8K | Nvidia laptop + host machine, forced 50/50 | 11.2 | - | 17.0 | 7831 (3811 + 4019) | 1 |
| gemma-4-12b | 8K | Nvidia laptop + host machine, forced 50/50 | - | 11.4 (cached) | 16.8 to 17.2 | RPC0 3811.3 + CUDA0 4019.2 = 7830.6 | 2 |
| gemma-4-12b | 8K | 2 PS5s (.122 + .101), budget split 13895,13897 | 26.7 (at least one half was already cached) | 21.0 on a reload | 12.3 to 12.7 | .122 3811, .101 4019, total 7830.5 | 3 |
| gemma-4-12b | 8K | 2 PS5s (.122 + .101), forced 50/50 | 21.0 (at least one half was already cached) | - | 12.7 to 13.0 | 3811 + 4019 (same as the budget split) | 3 |
| gemma-4-12b | 8K | PS5 (.122) + Nvidia laptop | 62.6 (partly cached) | - | 18.4 to 19.2 | .122 4931, Nvidia laptop 2900, total 7830.5 | 3 |
| gemma-4-12b | 8K | PS5 (.122) + Nvidia laptop, forced | load not finished (the PS5 worker died) | - | - | model buffers RPC0[.122] 4258 + RPC0[.3] 2519, split 63/37 | 1 |
| gemma-4-12b | 64K, q8 KV, flash-attn | Nvidia laptop + host machine | 10.7 | - | 17.9 | 8578 (4053 + 4525) | 1 |

Things the table shows:

- One PS5 is the best home for Gemma 8K: 29.2 tok/s on one PS5, 12.5 on two PS5s, 18.8 on a PS5 plus the Nvidia laptop (run 3).
- Pooling the Nvidia laptop and the host machine gives 17 to 18.8 tok/s. The host machine alone gives 33.3 to 38.6 (run 1).
- Where two runs give different tokens/s for the same placement, the gap is small. Run 1 has 18.8 for the Nvidia laptop + host machine budget split and run 2 has 16.9 to 17.0. Run 1 has 100 to 157 for tinyllama on the Nvidia laptop, run 2 has 83 to 92, and run 3 has 46 to 74 inside a burst of 8 mixed requests. The results files do not explain these gaps.
- Gemma pooled always sums to about 7831 MiB, however it is split.

## Upload rates per link

| Link | Rate | Where it comes from |
|---|---|---|
| PS5, one upload alone | 13.9 MB/s | Run 1. qwen sent 3.0 GB in 216 s. |
| PS5, two uploads at once | under 5.6 MB/s on average, in bursts | Run 1. Gemma to .101. The socket send queue sat still at 1.6 to 3.7 MB for 10 s or more at a time. |
| PS5, cold Gemma (run 3) | 5.1 MB/s average | Run 3. 3424 MB in 669 s to .122. About 39 MB/s in the first minute (about 2.3 GB). Then 2.2, 2.4, 2.4, 3.9, 2.4, 2.5, 1.9 MB/s, then three minutes of 0, then 1.2 MB/s. |
| PS5, Gemma reload on .122 | 7.9 MB/s | Run 3. 1119 MB in 142 s. |
| PS5, Gemma reload on .101 | 4.0 MB/s | Run 3. 1126 MB in 283 s. No data for about 4 minutes (23:39:34 to 23:43:44), then about 1 GB at 40 to 60 MB/s. The same reload at 00:20 took 32 s and 27 s. |
| Two PS5s, pooled loads | 49 to 56 MB/s combined while sending | Run 3. About 1.1 to 1.2 GB per load, in 20 to 25 s. Partly cached. |
| PS5, tinyllama | about 80 MB/s | Run 1. Almost certainly the worker's tensor cache, not the wire. |
| Nvidia laptop, cold | 77 MB/s | Run 1. qwen sent 3.0 GB in 39 s. |
| PS5 + Nvidia laptop pooled | 71 MB/s | Run 3. 4286 MB in 60 s. The Nvidia laptop share dominates. |

Other facts about uploads:

- Every Gemma load on a PS5 in run 3 sent about 1.1 GB, even when the rest was cached.
- The tensor cache is a big effect on the Nvidia laptop: qwen loaded in 39.0 s cold and 7.6 s warm, 5 times faster (run 1).
- `link_mb_s: 8` is the setting for PS5s now. Run 1 used 11 and the Gemma load timed out. That deadline was 120 + 2 x 7.1 GB / 11 MB/s = 1411 s, and the upload had not finished after 1258 s. Run 3 had a 1920 s deadline. The slowest load took 669 s, so the deadline was adequate (run 3 recommends no change).
- The stalls of 3 to 4 minutes are on the worker or PS5 side. That is inferred. The data only shows an idle wire while the worker stayed alive (run 3).

## Memory needs per profile

`needs_mib` is the whole footprint: weights, KV and compute buffers. The "measured from" column says what the number came from.

| Profile | `needs_mib` | Measured from | The old estimate or placeholder |
|---|---|---|---|
| tinyllama | 700 | 696 MiB on the Nvidia laptop (RPC) and on the host machine (runs 1 and 2). Model 601.0, KV 44, compute 51.0. | estimate 682 |
| qwen3.5-4b | 3400 | 3343 MiB on one PS5 (.122) and on the Nvidia laptop (runs 1, 2, 3). Model 2741, KV 512, compute 90. | estimate 4986 (the fit rule asks 5835) |
| gemma-4-12b, 8K | 7850 | 7514 MiB on one device (model 6777, KV 608, compute 130), plus 124 MiB of CUDA context on the host machine, which is 7638 in nvidia-smi. 7831 MiB on two devices (7830.6 in run 2, 7830.5 in run 3, on PS5 and Nvidia laptop pairs too). The old 7800 was 31 MiB short on two devices. | old value 7800; estimate 12040 |
| gemma-4-12b:64k, q8 KV, flash-attn | 8600 | 8578 MiB on Nvidia laptop + host machine (2 devices; KV 799, compute 535 + 467). One device would be about 8100 (one compute buffer; inferred, not measured). With 8600 the plan puts 64K on a single PS5. | old placeholder 14000; estimate 29096 |

Two related numbers:

- The host machine tier's `headroom_mib` is 450. nvidia-smi reports 8151 MiB but llama.cpp can use only 7707 MiB, a gap of 444 MiB (run 1).
- Each child that uses the host machine GPU adds about 120 MiB of CUDA context that the load log does not show. Run 1 measured about 102 MiB per RPC-only child before the fix. After the fix, the host machine stayed at 10 MiB with remote-only models loaded (run 2, and again after every PS5 load in run 3).

## Not yet measured (as of runs 1 to 3)

- **Gemma 48k and 96k.** They were not run in any of the three runs, so there was no measured `needs_mib` for them. They were measured after the fixes (last part).
- **Gemma 64K on one device.** About 8100 MiB is inferred, not measured.
- **Two cold uploads at once.** Every pooled test in run 3 found at least one half already cached.
- **A second all-cold PS5 load.** The PS5 caches cannot be cleared. The Nvidia laptop cache was also warm.
- **Gemma on both PS5s pooled, cold.** Not measured for the same reason.
- **PS5 + Nvidia laptop, cold.** Run 3 had it partly cached. Run 1 did not finish.
- **Qwen cold on a PS5 after run 1.** It was already cached in run 3. Run 1 has the only cold number (216 s).
- **PS5 qwen and gemma warm reloads in run 1.** Not run then. Run 3 covers some of them.
- **Failure paths on the PS5s** (node lost, quarantine, queue timeout). Run 2 checked them on other hardware. Run 3 did not repeat them.
- **A load failure (502) log line.** Not exercised in run 2 or 3.
- **Whether a node that comes back has a very low first budget.** Run 2 saw one placement at 5724 MiB. Run 3 could not reproduce it, because no node dropped.
- **Why the PS5 uploads stall.** Inferred to be worker or PS5 side. Not measured.
- **FLUX and Whisper.** These runs did not cover them.

## After the PS5 fixes (2026-11-18)

Two fixes were made on the PS5 side (not in this repo): the `rpc-server` tensor cache was turned off, and the PS5 GPU clock was pinned at 2230 MHz (it sat at 500 MHz; decode about 2.4x faster on a two-PS5 split). Every load is now a full upload and there are no cached reloads. Sources: load times and memory come from the scheduler's log lines (`load_duration`, "instance ready"); decode speeds come from the separate benchmark agent (full tables in `C:\Users\J\documents\ps5llm\benchmarks.md`, outside this repo). Details and context: `RESULTS-run4.md`.

### Upload rates after the fixes

| Case | Rate | Notes |
|---|---|---|
| Total out of the host machine, 1, 2 or 3 PS5s in one load | 75 to 84 MB/s | A split model uploads to one PS5 at a time, in an order llama.cpp chooses. |
| One Gemma 12B load | 89 s | |
| Two Gemma 12B loads at once | 167 s each, 40 MB/s each | The rate is shared. |
| Three Gemma 12B loads at once | 246 s each, 27 MB/s each | The rate is shared. |

The rows above were measured on the old 1 GbE link (the host machine's built-in port).

`link_mb_s` for the PS5 tier was 80 on that link. It is now 100 (see below).

#### After the network upgrade (2026-11-18, about 14:00 to 14:31 UTC)

The host machine's link to the PS5 switch moved to a USB-C adapter (interface `enx6c1ff7c349ea`) negotiated at 2500 Mb/s. The PS5s still have 1 GbE ports.

| Case | 1 GbE (old link) | 2.5 GbE |
|---|---|---|
| One Gemma 12B load, one PS5 | 89 s, about 78 MB/s | 66 s, about 100 to 108 MB/s |
| Qwen 27B, three PS5s | 216 s | 170 s |
| Kimi-Dev 72B IQ2_S, three PS5s | 363 s | 286 s |
| Two Gemma 12B at once, different PS5s | 167 s each | 66 s each |
| Three Gemma 12B at once | 246 s each | 77 s each, about 280 MB/s in total |
| Devstral 24B on two PS5s plus Gemma 12B on the third | no earlier run of this pair (Devstral alone 183 s) | 149 s and 67 s |

A single stream to one PS5 is now limited by that PS5's own 1 GbE port (about 100 to 125 MB/s). Loads to different PS5s no longer share one gigabit. Three at once slow slightly because the total nears the 2.5 GbE link. A split model still uploads to one PS5 at a time.

### Load times through the scheduler (ctx 2048 unless stated)

| Model | PS5s | Load s |
|---|---|---|
| tinyllama | 1 | 8 to 9 |
| qwen 4B | 1 | 35 to 38 |
| gemma 12B | 1 | 85 to 90 |
| devstral 24B | 2 | 183 |
| devstral 24B | 3 | 184 |
| qwen 27B | 2 | 215 |
| qwen 27B | 3 | 216 |
| qwen3 coder 30B | 3 | 240 |
| kimi-dev 72B IQ2_S, ctx 8192 | 3 | 356 |

### Memory per instance (MiB, "instance ready" log lines)

| Model | Context | PS5s | MiB |
|---|---|---|---|
| tinyllama | 2048 | 1 | 696 |
| tinyllama | 8192 | 1 | 834 |
| qwen 4B | 2048 | 1 | 2881 |
| qwen 4B | 8192 | 1 | 3079 |
| qwen 4B | not stated | 2 | 3034 |
| qwen 4B | not stated | 3 | 3126 |
| gemma 12B | 2048 | 1 | 7412 |
| gemma 12B | 8192 | 1 | 7514 |
| gemma 12B | 2048 | 2 | 7687 |
| gemma 12B | 8192 | 2 | 7831 |
| gemma 12B | not stated | 3 | 7852 |
| gemma 12B | 48k | 2 | 8206 |
| gemma 12B | 96k | 2 | 8998 |
| devstral 24B | 8192 | 2 | 14248 |
| devstral 24B | 2048 | 2 | 14248 |
| devstral 24B | 2048 | 3 | 14517 |
| qwen 27B | 2048 | 2 | 16272 |
| qwen 27B | 2048 | 3 | 16442 |
| qwen 27B | 8192 | 3 | 16898 |
| qwen3 coder 30B | 2048 | 3 | 18077 |
| kimi-dev 72B IQ2_S | 8192 | 3 | 29785 |

Config `needs_mib` now: `gemma-4-12b` 7900 (above the 7852 seen on three PS5s), `gemma-4-12b:48k` 8250 (from 8206), `gemma-4-12b:96k` 9050 (from 8998), `devstral-24b` 14300 (from 14248 at ctx 8192); `tinyllama` 700 and `gemma-4-12b:64k` 8600 are unchanged. Devstral 24B does not fit one PS5 (needs about 13991 MiB, about 13.5 GB free); forced onto one PS5 the upload starts and the PS5 `rpc-server` crashes mid-upload.

### Decode speed on three PS5s (benchmark agent)

| Model | Decode tok/s |
|---|---|
| deepseek v2 lite | 78.4 |
| qwen3 coder 30B-A3B | 62.3 |
| qwen3 30B-A3B | 59 to 61 (2k, 8k and 32k context) |
| glm-4 32B | 15.1 |
| kimi-dev 72B IQ2_S | 8.9 |
| llama 3.3 70B Q2_K | 8.5 |
| qwen2.5 72B Q2_K | 8.5 |
| devstral 24B (two PS5s) | 17.8 |

Other benchmark-agent findings: automatic placement was 3.8 to 7.5% faster in decode than forcing three PS5s for models that fit on fewer. Adding PS5s to a model that already fits slightly lowers decode (qwen 4B 47.9 / 45.5 / 44.8 tok/s on 1 / 2 / 3 PS5s) but raises prefill (devstral 24B 104.5 to 137.6 tok/s from 2 to 3 PS5s). Mixtral 8x7B (TheBloke Q4_K_M file) failed to load for a file-format reason (`tensor 'blk.0.ffn_down_exps.weight' not found`), not memory.
