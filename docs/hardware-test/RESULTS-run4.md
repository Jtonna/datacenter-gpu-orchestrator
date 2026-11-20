# llm-scheduler: fourth on-hardware test (stopped early, then re-measured after the PS5 fixes)

Date: 2026-11-18. Three PS5s as rpc workers, host machine as host, one gigabit port (the numbers marked 1 GbE were measured on that port; see "After the network upgrade" at the end).

**Run 4 was stopped early by the owner.** The part before the stop was our own tester's partial run. The run's logs folder (raw logs are not kept in the repository) is partial: it was copied part way through the run and it only covers that part. The after-fix numbers on this page do not come from that folder. They come from the scheduler's log lines and from the separate benchmark agent, as marked below.

## Verdict

**No scheduler defect was found in run 4 or in the benchmark work that followed.** The upload bottleneck was on the PS5 side, and it was fixed there (not in this repo). One bug in new scheduler code (per-worker load state) was found and fixed the same day.

## Part 1: before the PS5 fixes (our tester, partial run)

Source: the run's logs, not kept in the repository (the run was stopped when the upload bottleneck was found). Loads here ran with the PS5 tensor cache on.

| Test | Placement | Load | Cache |
|---|---|---|---|
| Qwen 4B | one PS5 | 340 s | cold |
| Gemma 12B | one PS5 | 57 to 59 s | cached |
| Gemma 12B | two PS5s | 21 to 37 s | cached |
| Gemma 12B | to the third PS5 | 1645 s | cold |
| Devstral 24B | two PS5s | 4963 s (83 minutes) | cold |
| Devstral 24B | two PS5s | 298 s | cached |

- The `exclude_nodes` check passed in this run (its exclude check log and status snapshot; raw logs are not kept in the repository).
- These numbers describe a PS5 that wrote every tensor to USB storage. They are not valid for the current setup. See `BENCHMARKS.md` for the older rows, marked as measured before the fixes.

## The cause and the two PS5-side fixes

Both fixes were made on the PS5 side and are not in this repo.

1. The PS5 `rpc-server` tensor cache was turned off. It wrote every tensor to USB storage at about 3 MB/s, so a cold 14.3 GB load took 83 minutes. Now every load is a full upload and there are no fast cached reloads.
2. The PS5 GPU clock was pinned at 2230 MHz. It sat at 500 MHz. This raised decode speed about 2.4x on a two-PS5 split.

## Part 2: after the fixes

### Upload speed (scheduler log and tester)

- 75 to 84 MB/s in total out of the host machine, the same for 1, 2 or 3 PS5s in one load.
- A split model uploads to one PS5 at a time, in an order llama.cpp chooses (seen: first, third, second of the `--rpc` list).
- Simultaneous loads share the rate:

| Loads at once | Time per load | Rate per load |
|---|---|---|
| One Gemma 12B | 89 s | not stated |
| Two | 167 s each | 40 MB/s each |
| Three | 246 s each | 27 MB/s each |

- Load progress was verified live. It is measured from TCP acknowledged bytes and matches about 80 MB/s (measured on the old 1 GbE link).

### Load times through the scheduler (ctx 2048; scheduler log)

| Model | PS5s | Load |
|---|---|---|
| tinyllama | 1 | 8 to 9 s |
| Qwen 4B | 1 | 35 to 38 s |
| Gemma 12B | 1 | 85 to 90 s |
| Devstral 24B | 2 | 183 s |
| Devstral 24B | 3 | 184 s |
| Qwen 27B | 2 | 215 s |
| Qwen 27B | 3 | 216 s |
| Qwen3 Coder 30B | 3 | 240 s |
| Kimi-Dev 72B IQ2_S (ctx 8192) | 3 | 356 s |

### Memory (MiB, from the scheduler's "instance ready" log lines)

| Model | Context | PS5s | MiB |
|---|---|---|---|
| tinyllama | 2048 | 1 | 696 |
| tinyllama | 8192 | 1 | 834 |
| Qwen 4B | 2048 | 1 | 2881 |
| Qwen 4B | 8192 | 1 | 3079 |
| Qwen 4B | not stated | 2 | 3034 |
| Qwen 4B | not stated | 3 | 3126 |
| Gemma 12B | 2048 | 1 | 7412 |
| Gemma 12B | 8192 | 1 | 7514 |
| Gemma 12B | 2048 | 2 | 7687 |
| Gemma 12B | 8192 | 2 | 7831 |
| Gemma 12B | not stated | 3 | 7852 |
| Gemma 12B | 48k | 2 | 8206 |
| Gemma 12B | 96k | 2 | 8998 |
| Devstral 24B | 8192 | 2 | 14248 |
| Devstral 24B | 2048 | 2 | 14248 |
| Devstral 24B | 2048 | 3 | 14517 |
| Qwen 27B | 2048 | 2 | 16272 |
| Qwen 27B | 2048 | 3 | 16442 |
| Qwen 27B | 8192 | 3 | 16898 |
| Qwen3 Coder 30B | 2048 | 3 | 18077 |
| Kimi-Dev 72B IQ2_S | 8192 | 3 | 29785 |

Devstral 24B forced onto one PS5 does not fit: it needs about 13991 MiB against about 13.5 GB free. The upload started and the PS5 `rpc-server` crashed mid-upload ("Remote RPC server crashed or returned malformed response"), then restarted by itself. This is why `defaults.forced_fit_check` was added (default off).

### Scheduler behaviour (separate benchmark agent: five phases, about 60 loads, no scheduler defect)

- Automatic placement was 3.8 to 7.5% faster in decode than forcing three PS5s, for models that fit on fewer.
- Adding PS5s to a model that already fits slightly lowers decode (Qwen 4B 47.9 / 45.5 / 44.8 tok/s on 1 / 2 / 3 PS5s) but raises prefill (Devstral 104.5 to 137.6 tok/s from 2 to 3 PS5s).
- Eviction right after a load waited 87 s (the hold time), then swapped: 307 s in total. An idle victim was evicted at once: 225 s in total.
- A child killed mid-decode was removed in 100 ms with no backoff, and the next request reloaded.
- A child killed during load gave 502, a 60 s backoff, 503 inside the window, then a normal load.
- 8 simultaneous requests to a one-slot profile were served in order with no errors.
- Fail-fast for a forced node missing from the fleet returned 503 in live use.
- A fleet manager restart during a 27.9 GB load (three times) did not interrupt the load, and the node re-registered in about 10 s.
- Scheduler overhead against talking to the child directly was not measurable (3.71 s against 3.75 s per request).

### Decode speeds on three PS5s (benchmark agent's figures)

| Model | Decode tok/s |
|---|---|
| DeepSeek V2 Lite | 78.4 |
| Qwen3 Coder 30B-A3B | 62.3 |
| Qwen3 30B-A3B | 59 to 61 at 2k, 8k and 32k context |
| GLM-4 32B | 15.1 |
| Kimi-Dev 72B IQ2_S | 8.9 |
| Llama 3.3 70B Q2_K | 8.5 |
| Qwen2.5 72B Q2_K | 8.5 |
| Devstral 24B (two PS5s) | 17.8 |

Mixtral 8x7B (TheBloke Q4_K_M file) failed to load for a file-format reason (`tensor 'blk.0.ffn_down_exps.weight' not found`), not memory. The full tables are outside this repo, in a results file kept outside this repository.

### Bug found and fixed (new code)

The per-worker load `state` assumed upload order equals `--rpc` order and marked an untouched worker `done`. It now uses each worker's own bytes (design 7.6a).

### PS5 dashboards path

The scheduler can stand in for llama-swap on port 3122. The hub polls `/running`, relays the `load` object and `recent_loads` to the fleet manager's `/api/models`. Verified end to end.

## Not tested or open

- Run 4 itself stopped early, so its planned tests after the stop were not run.
- The before-fix rows above are partial and used the PS5 cache.
- Mixtral 8x7B needs a different GGUF file.

## After the network upgrade (2026-11-18, about 14:00 to 14:31 UTC)

The host machine's link to the PS5 switch moved from its built-in 1 GbE port to a USB-C adapter (interface `enx020000000010`) negotiated at 2500 Mb/s. The PS5s still have 1 GbE ports. Load times through the scheduler at ctx 2048. The "1 GbE" column is the old link, the "2.5 GbE" column is the new one.

| Load | 1 GbE | 2.5 GbE |
|---|---|---|
| Gemma 12B on one PS5 | 89 s (single stream about 78 MB/s) | 66 s (about 100 to 108 MB/s) |
| Qwen 27B across three PS5s | 216 s | 170 s |
| Kimi-Dev 72B IQ2_S across three PS5s | 363 s | 286 s |
| Two Gemma 12B at once, different PS5s | 167 s each | 66 s each |
| Three Gemma 12B at once | 246 s each | 77 s each (about 280 MB/s in total) |
| Devstral 24B on two PS5s, with Gemma 12B on the third | no earlier run of this pair (Devstral alone: 183 s) | 149 s and 67 s |

Reading: a single stream to one PS5 is now limited by that PS5's own 1 GbE port (about 100 to 125 MB/s). Loads to different PS5s no longer share one gigabit. Two at once run at full speed. Three at once slow slightly because the total nears the 2.5 GbE link. A split model still uploads to one PS5 at a time. The PS5 tier `link_mb_s` is now 100.

## Files

The run produced (partial; raw logs are not kept in the repository): `config-run4*.yaml`, `scheduler-run4-all.log`, per-test `D-*`, `L-*`, `M-*` files, `status-*.json`, `fleet-sample.jsonl`, `ps5-checks.txt`, the exclude check files.
