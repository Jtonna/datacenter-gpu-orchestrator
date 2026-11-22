# datacenter-gpu-orchestrator

**Potluck** pools mixed machines, game consoles included, into one GPU cluster that runs large language models behind an OpenAI-compatible API. When a model is too big for one machine, Potluck splits it across several.

Potluck is the name of the application. This repository holds its scheduler, its web UI and its documentation.

## What it does

- **One API for the whole pool.** Clients send ordinary OpenAI-style requests (`/v1/chat/completions`). Potluck decides which machines run the model, loads it, and forwards the request.
- **Splits models across machines.** A model that needs more graphics memory than one machine has is spread over several, using llama.cpp's RPC workers.
- **Places by measured memory.** It reads each model file, estimates what it needs, checks what every machine has free, and refuses a load that would not fit instead of letting a machine run out of memory.
- **Queues and swaps.** When the pool is busy, requests wait in a queue. Idle models are unloaded to make room for the one that was asked for.
- **Watches loads.** Per-machine load progress, stall detection, hung-worker detection, and a memory floor that stops a load before a machine freezes.
- **Configuration in a database, with a full audit log.** Every change is a numbered revision that records who made it and from where (web UI, REST or an AI agent), applies without a restart, and can be rolled back.
- **Built-in benchmarks.** Define benchmarks and suites, run them through the scheduler itself, and compare every run with the last one: load time, tokens per second, regressions. Each step also records the lowest available memory per node, the load mode (file read or pushed), the host and workers, and whether a reload can come from the file cache.
- **A node feed.** `GET /api/nodes` lists every machine as status shows it, with its uptime and boot id and a change counter, for a dashboard that polls every few seconds.
- **A web UI and live updates.** Models, machines, activity, benchmarks and settings in the browser, updated over one WebSocket.
- **Central logging** (built, not deployed yet). Potluck's own log, the output of the programs it starts, and every node's agent log, launch output and system journal go to one log store (VictoriaLogs, its own container). Its search page is under `/logs/`, read only, with links from nodes, models and benchmark runs; scripts and AI agents query it through `GET /api/logs/query`. Logging never slows scheduling: senders drop the oldest lines instead of waiting.
- **Operable by AI agents.** An MCP server on the same port exposes the same operations as the REST API, with the same audit trail.

It is one Go binary in one container. It starts `llama-server` processes itself and needs no other service.

## Status

**Runs today:** the scheduler, the web UI, the REST and MCP APIs, benchmarks, and split loads across several machines. It has been serving a small cluster of consoles and laptops, with models from 4 to 72 billion parameters split over up to three machines.

**`potluck-agent`** is the small service that each machine runs to join the pool. It registers the machine, fetches the right llama.cpp programs for its hardware, starts them per load, and reports the machine's stats. It exists and has run on a console (bare metal, from RAM) and in Docker on two laptops. Its contract is in [docs/agent-contract.md](docs/agent-contract.md) and [docs/stats-contract.md](docs/stats-contract.md).

**In progress:** the changeover of the first deployment from a separate fleet manager to agents (until it is done, parts of the code, tests and documents still describe that deployment), and Windows.

**Supported hardware today:** Linux machines with AMD graphics through Vulkan (the consoles) and with Nvidia cards through CUDA. Windows with Nvidia is planned.

There is no authentication. Run it on a network you trust.

## Operating notes

- **Restarting or updating Potluck ends every loaded model.** Potluck keeps its launches in memory. After a restart it knows none, and each agent stops the launch Potluck no longer knows when it registers again. Plan a restart like an outage of every model.
- **Ports a machine must accept from Potluck** (TCP, inbound): `50060` (the agent API) and `50052` (the worker during a load); `50061` (a server on the machine) comes later. A machine whose firewall drops them registers fine but gets no load: its status says `agent_unreachable`. `potluck-agent-setup` opens them in ufw when ufw is active.
- **No authentication**, on Potluck's port or on the agents' ports. Anyone who can reach them can use and change the pool.

## Related projects

- Built on our llama.cpp fork: https://github.com/Jtonna/llama.cpp-for-datacenter-gpu-orchestrator. The fork carries the changes Potluck needs for loading a model across machines: parallel upload to workers, loader fixes, and workers that read model files themselves from a shared folder.
- Reference deployment: a rack of PlayStation 5 consoles managed by the PS5 AI Datacenter project (a separate, private repository).

## Run the tests

```
go vet ./...
go test ./...
```

Add `-race` where cgo is available (it needs gcc on Windows; the `golang:1.25` image on Linux has it). The tests use a fake `llama-server` and a fake fleet, so they need no GPU.

## Try the web UI without a cluster

```
go run ./cmd/devcluster --listen 127.0.0.1:8790 --scenario demo
```

This starts the real scheduler against simulated machines and opens the UI at `http://127.0.0.1:8790/ui/`.

## Configuration

All configuration (model profiles, hardware tiers, settings) is stored in a SQLite database in the data directory, not in a file. Read it at `GET /api/config`, change it over REST (`/api/config/...`; `GET /api/config/schema` describes every field), and read the change log at `GET /api/audit`. Only five paths are set outside the database, by flag or environment variable: `--listen`, `--data-dir`, `--models-dir`, `--history-dir`, `--child-log-dir`. A first install imports a YAML file once with `llm-scheduler import-config --data-dir DIR file.yaml`; [configs/example.yaml](configs/example.yaml) documents that format. Starting on an empty database works.

Two node lists take a node id or a hostname pattern: `exclude_nodes` (never used) and `no_auto_nodes` (used only by profiles that name the node in `force_nodes`; automatic placement never picks it or evicts from it). Excluding wins. See design section 6 and the runbook.

## Layout

- `cmd/llm-scheduler/`: the entry point.
- `cmd/devcluster/`: the development harness used above.
- `internal/`: the packages. `gguf` and `estimate` (reading model files and estimating memory), `fleet` and `placer` (which machines exist and where a model goes), `instance` and `scheduler` (starting, supervising and stopping models; the queue), `proxy` and `api` (the HTTP surface and the web UI), `cfgstore`, `cfgsvc` and `audit` (configuration, revisions, audit log), `bench` and `benchspec` (benchmarks), `history`, and `mcpserver`.
- `internal/testutil/`: the fake `llama-server`, the fake fleet and test helpers.
- `deploy/`: the container image, the compose file and the build recipe for the llama.cpp client.

## Documents

- [docs/design.md](docs/design.md): the design.
- [docs/runbook.md](docs/runbook.md): deploying and running it.
- [docs/benchmark-guide.md](docs/benchmark-guide.md): running benchmarks, by hand and from the scheduler.
- [docs/agent-contract.md](docs/agent-contract.md) and [docs/stats-contract.md](docs/stats-contract.md): the contract for `potluck-agent`.
- [docs/hardware-test/](docs/hardware-test): results of the tests on real hardware.

## Licence

MIT. See [LICENSE](LICENSE).
