# ps5-ai-datacenter-load-balancer

`llm-scheduler` is a Go replacement for llama-swap on the OMEN host. When a
model is requested it decides, from live fleet state, which machines (PS5s,
Alienware, OMEN) run it, and queues requests when nothing is free. See
[docs/design.md](docs/design.md) for the full design. Current state: milestone
M0 (scaffold, test harness, `fakellama`); no scheduler logic yet.

## Tests

```
go vet ./...
go test ./...
```

Add `-race` where cgo is available (needs gcc on Windows).

## Layout

- `cmd/llm-scheduler/` entry point.
- `internal/` packages named in design section 5 (`config`, `gguf`, `estimate`,
  `fleet`, `placer`, `instance`, `scheduler`, `proxy`, `api`); each has a
  `doc.go` stating its job.
- `internal/testutil/fakellama/` fake `llama-server-rpc` for integration tests
  (see its README).
- `internal/testutil/harness/` test helpers: build `fakellama` once, free
  ports, wait for HTTP endpoints.

## Docs

- [docs/design.md](docs/design.md) the design.
- [docs/runbook.md](docs/runbook.md) deploying and running it on the OMEN.
- [docs/benchmark-guide.md](docs/benchmark-guide.md) running model benchmarks through the scheduler.
- [docs/hardware-test/](docs/hardware-test) results of the hardware tests: `RESULTS.md`, `RESULTS-run2.md`, `RESULTS-run3.md`, `RESULTS-run4.md` and the merged tables in `BENCHMARKS.md`.
