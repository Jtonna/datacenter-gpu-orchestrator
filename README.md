# ps5-ai-datacenter-load-balancer

`llm-scheduler` is a Go replacement for llama-swap on the OMEN host. When a
model is requested it decides, from live fleet state, which machines (PS5s,
Alienware, OMEN) run it, and queues requests when nothing is free. See
[docs/design.md](docs/design.md) for the full design.

The scheduler keeps its safety and lifecycle events in a file-backed history
(`--history-dir`) and serves a read-only dashboard at `/ui/` and the history at
`/api/history` (design 9.1, runbook "Event history and the dashboard").

Everything live goes over one WebSocket, `GET /ws` (status, history events,
benchmark progress; design 9.2). It can also run benchmarks itself: define
benchmarks and suites (`import-config`, `/api/bench/...` or MCP), run them through its own
API, and keep every result and the comparison with the last run in a SQLite
database under the data directory (design 9.3, runbook "Benchmarks run by the
scheduler", benchmark guide section 12). All of it is in the one binary and
container.

## Configuration

All scheduler configuration (profiles, tiers, settings, exclusions, recovery,
fleet) is stored in the SQLite database in the data directory, not in a file.
Every change is a numbered revision with an audit entry (who, when, from the web
UI, REST or MCP, what changed), applies at once without a restart, and can be
rolled back. Read it at `GET /api/config`, change it over REST (`/api/config/...`,
`GET /api/config/schema` describes every field), read the log at `GET /api/audit`
(design 6.0). Only five deployment paths are set outside the database, by flag or
environment: `--listen`, `--data-dir`, `--models-dir`, `--history-dir`,
`--child-log-dir` (`LLM_SCHEDULER_LISTEN`, `_DATA_DIR`, `_MODELS_DIR`,
`_HISTORY_DIR`, `_CHILD_LOG_DIR`). The `--config file.yaml` start is gone: a first
install (or a migration) imports a YAML file once with
`llm-scheduler import-config --data-dir DIR file.yaml`; `configs/example.yaml`
documents that input format and `deploy/seed-config.yaml` is the production seed
(runbook "Configuration lives in the database"). Starting on an empty database works:
no profiles, "no configuration yet".

AI agents can operate it over MCP at `/mcp` on the same port (46 tools that run the same code as the REST API; design 9.5, runbook "Let an AI agent operate the scheduler"). Everything an agent changes lands in the same audit log with source `mcp` and the client's name; benchmark and suite changes and runs are audited in section `bench`, and the loads a benchmark run makes by itself are tagged in the history and logged as source `system`.

## Tests

```
go vet ./...
go test ./...
```

Add `-race` where cgo is available (needs gcc on Windows; the Linux run in the
`golang:1.25` image has it).

## Layout

- `cmd/llm-scheduler/` entry point.
- `internal/` packages named in design section 5 (`config`, `gguf`, `estimate`,
  `fleet`, `placer`, `instance`, `scheduler`, `proxy`, `api`) plus `history`,
  `audit`, `cfgstore`, `cfgsvc` (audit log, revisions, the one configuration service),
  `benchspec` and `bench` (benchmarks: store, runner, progress model, statistics,
  aggregates, export), `mcpserver` (the MCP server, tools as REST recipes); each
  has a `doc.go` or package comment stating its job.
- `internal/testutil/fakellama/` fake `llama-server-rpc` for integration tests
  (see its README).
- `internal/testutil/harness/` test helpers: build `fakellama` once, free
  ports, wait for HTTP endpoints.

## Docs

- [docs/design.md](docs/design.md) the design.
- [docs/runbook.md](docs/runbook.md) deploying and running it on the OMEN.
- [docs/benchmark-guide.md](docs/benchmark-guide.md) running model benchmarks through the scheduler, by hand and (section 12) from the scheduler itself.
- [docs/hardware-test/](docs/hardware-test) results of the hardware tests: `RESULTS.md`, `RESULTS-run2.md`, `RESULTS-run3.md`, `RESULTS-run4.md` and the merged tables in `BENCHMARKS.md`.
