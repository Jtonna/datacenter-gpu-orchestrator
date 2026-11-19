# ps5-ai-datacenter-load-balancer

`llm-scheduler` is a Go replacement for llama-swap on the OMEN host. When a
model is requested it decides, from live fleet state, which machines (PS5s,
Alienware, OMEN) run it, and queues requests when nothing is free. See
[docs/design.md](docs/design.md) for the full design.

The scheduler keeps its safety and lifecycle events in a file-backed history
(`history_dir`) and serves a read-only dashboard at `/ui/` and the history at
`/api/history` (design 9.1, runbook "Event history and the dashboard").

Everything live goes over one WebSocket, `GET /ws` (status, history events,
benchmark progress; design 9.2). It can also run benchmarks itself: define
benchmarks and suites (config or `/api/bench/...`), run them through its own
API, and keep every result and the comparison with the last run in a SQLite
database under `data_dir` (design 9.3, runbook "Benchmarks run by the
scheduler", benchmark guide section 12). All of it is in the one binary and
container; with no `data_dir` the benchmark feature is off and nothing else
changes.

AI agents can operate it over MCP at `/mcp` on the same port (46 tools that run the same code as the REST API; design 9.5, runbook "Let an AI agent operate the scheduler").

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
  `benchspec` and `bench` (benchmarks: store, runner, statistics, export); each
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
