# fakellama

A stand-in for `llama-server-rpc` (design 7.4, 11). Tests build it with
`harness.BuildFakeLlama(t)` and run it as a subprocess.

It accepts any command line. `--host`, `--port`, `--model/-m`, `--rpc`,
`--device/-dev`, `--tensor-split/-ts`, `--ctx-size/-c`, `--parallel/-np`, `-lv/--verbosity/--log-verbosity` and
`--list-devices` are understood; every other flag is tolerated, with or
without a value.

`--list-devices` prints the same text as llama.cpp's
`common_print_available_devices()`:

```
Available devices:
  RPC0: 192.0.2.2:50052 (15000 MiB, 14000 MiB free)
  CUDA0: NVIDIA GeForce RTX 5060 (8151 MiB, 7000 MiB free)
```

Otherwise it serves `/health`, `/v1/chat/completions` (JSON or SSE),
`/v1/completions`, `/v1/embeddings`, `/props`, `/slots`, `/v1/models`. Until
loaded, every endpoint answers 503 `{"error":{"message":"Loading model",...}}`.
`n_ctx` per slot is `--ctx-size / --parallel`.

## Environment variables

| Variable | Effect |
|---|---|
| `FAKELLAMA_START_LOG` | File to which one JSON line is appended per start: time, pid, mode (`serve` or `list-devices`), full `argv`, and every `FAKELLAMA_*` and `LLAMA_ARG_*` env var, plus `CUDA_VISIBLE_DEVICES` when set (even empty). |
| `FAKELLAMA_LOAD_DELAY` | Duration (e.g. `500ms`) before `/health` turns 200. Default 0. |
| `FAKELLAMA_NEVER_HEALTHY` | Non-empty: stay in "loading" forever. |
| `FAKELLAMA_EXIT_CODE` | Exit with this code, after `FAKELLAMA_EXIT_AFTER` (default 0s), printing a line on stderr. Simulates a load failure. |
| `FAKELLAMA_EXIT_AFTER` | Delay for the above. |
| `FAKELLAMA_DEVICES` | `--list-devices` list as `NAME:totalMiB:freeMiB,...`, or `none`. Default: `RPC<i>` per `--rpc` endpoint plus `CUDA0`. |
| `FAKELLAMA_LIST_DEVICES_HANG` | Non-empty: `--list-devices` blocks forever, printing nothing (busy rpc-server). |
| `FAKELLAMA_LIST_DEVICES_EXIT` | Non-zero: `--list-devices` prints an error on stderr and exits with this code. |
| `FAKELLAMA_STREAM_CHUNKS` | Content chunks per reply. Default 5. |
| `FAKELLAMA_STREAM_DELAY` | Pause between streamed chunks. Default 0. |
| `FAKELLAMA_STALL_AFTER` | Streaming: after N content chunks send nothing more and hold the connection until the client leaves. Default off. |
| `FAKELLAMA_LOAD_LOG` | On start (before listening), and only when the command line has `-lv N` with N >= 4 (`--verbosity`, `--log-verbosity`; llama.cpp hides these INFO lines at its default 3), print llama.cpp style `model` / `KV` / `compute buffer size` lines on stderr: `auto` (one set per `--device` name, 1000/200/100 MiB) or `NAME:modelMiB:kvMiB:computeMiB,...`. RPC devices are named `RPC0[<endpoint>]` as in llama.cpp. A `CPU_Mapped` model line and unrelated lines are interleaved. Default: nothing. |
| `FAKELLAMA_IGNORE_SIGTERM` | Non-empty: log "ignoring" on stderr and keep running on SIGTERM/interrupt. |

By default SIGTERM or interrupt exits 0 at once. The SIGTERM tests are skipped
on Windows, where Go cannot deliver that signal to a child.

## Additions for the end-to-end tests

| Item | Effect |
|---|---|
| `FAKELLAMA_LIST_DEVICES_HANG_ADDR` | Comma-separated `--rpc` endpoints. `--list-devices` blocks forever only when one of its `--rpc` endpoints is listed (one busy worker among several). |
| `fakellama --nvidia-smi [totalMiB [usedMiB]]` | Prints `total, used` (default `8151, 0`) and exits: a stand-in for the `nvidia-smi` command in `local_probe_cmd`. |
