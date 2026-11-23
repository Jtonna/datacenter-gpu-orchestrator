# Releases

What each release of Potluck (the scheduler, `llm-scheduler`) and its node agent (`potluck-agent`) brings, newest first. The wire details are in the [agent contract](agent-contract.md), the day-to-day steps in the [runbook](runbook.md).

## 0.1.8

### What is new

- **Background tasks are isolated.** A failure (an internal error) in one of the scheduler's background tasks no longer takes the scheduler down. The task restarts alone, rebuilding its state where it needs to (the event stream reconnects browsers to a fresh snapshot, the history writer continues in a fresh file, the benchmark manager fails the run in hand and unloads its models), or the work it was doing fails with an error. Architecture, "What restarts when", lists every task.
- **Failures are visible.** Each one is logged at error level with its stack, recorded as an `internalFailure` event, and counted. `GET /api/system` lists the tasks (`tasks`: restarts, failures in the last minute, last failure time and message) and `internal_failures_total`. The About page has a "Background tasks" card and warns after a failure in the last hour.
- **Bounded stop with exit code 3.** Three places still stop Potluck, because their state cannot be rebuilt in place: the scheduler loop, the pool's lost-node check and applying a configuration change. The same task failing three times within 60 s stops it too. The stop logs the reason, records it, kills every model process Potluck started, and exits with code 3 within 5 s; Docker starts it again.
- **No lock is left held.** A failure inside a section that holds the lock of shared state (the pool, the scheduler, the learned needs, the history, the model processes, the configuration, the benchmarks) releases the lock and stops Potluck the bounded way, whatever the task: the state may be half-written, and a restart rebuilds it from the database. A failure outside such a section follows the task's own policy.
- **One bad input counts once.** The members of one fan-out (the probes of one pre-flight, the launches or stops of one load, the members of a benchmark group, the nodes of one cache eviction) count as one failure toward the three-in-a-minute stop.

### What changes without any setting

- A configuration change whose apply fails answers 500 ("the change is saved ... Potluck restarts now to apply it"), then Potluck restarts and applies it from the database.
- An HTTP handler panic is answered by net/http as before, and is now also logged with its stack and counted. Goroutines a request starts (a cache eviction, a prefetch) are isolated too: their failure becomes the request's error.
- A failure while Potluck is already stopping (a signal) does not cut the shutdown's grace short: the shutdown goes on and exits with code 3.

### What to do at the roll-out

- Nothing beyond the usual update (runbook, F). Check that the container's restart policy is `unless-stopped` (the standing layout has it), so an exit with code 3 starts Potluck again.

## 0.1.7

### What is new

- **Excluding a machine that profiles force.** Setting `exclude_nodes` while saved profiles force an excluded machine is accepted. The profiles stay as they are; the answer lists each one in `warnings` (`profile "<id>": force_nodes: node "<node>" is excluded by exclude_nodes entry "<entry>"; the profile is kept and is refused until the exclusion is lifted`) and in `blocked_by_exclusion` (`profile`, `node`, `entry`). The same holds the other way round, for a profile written to force an excluded machine, for `import-config` (one note per profile) and for the seed. `GET /api/profiles` marks such a profile with `blocked_by_exclusion: <entry>`. At run time it is refused as before, now naming the entry, and the plan says the same. Lifting the exclusion makes it work again without touching it.
- **A model still running on a machine being excluded.** Excluding a machine stops new placements only: a model placed there before keeps running and serving until it is unloaded (no automatic drain). Every configuration write lists such models in `loaded_on_excluded_nodes` (`profile`, `state`, `nodes`, `entries`, `loaded_since`) and in `warnings`. `GET /api/nodes` and the agents of `GET /api/status` give such a machine `running_while_excluded` (`profile`, `state`, `since`) next to `excluded`; the Nodes page says "Excluded, busy" and names the model.
- **Benchmark steps refused by an exclusion.** Only a profile that FORCES an excluded machine is refused this way. Such a step ends with the outcome `refused_by_exclusion` and the scheduler's reason, and is not judged: its result is `not_judged` (`expect.not_judged: true`), it does not stop a `stop_on_failure` suite, it is not a regression, and it stays out of the benchmark and suite pass rates and the trends. Exports say "not judged", the history event is `info` with `result: not_judged` and no `pass`, and the pages say "Not judged (excluded node)". The run summary has `ran` and `refused_by_exclusion` next to `passed` and `failed`; a run whose every step was refused this way has the result `not_judged`. The aggregates (`GET /api/bench/aggregates`) give each benchmark and suite `judged` (the steps or runs the rate is over) and a `pass_rate` of `null` when nothing was judged; the pages then say "not judged" instead of a percentage. The `benchRunFinished` event has `refused_by_exclusion` and its text counts the not-judged steps. An automatic profile that finds no room because machines are excluded gets the ordinary refusal (`cluster busy` after the queue wait, or `does not fit`), counted as a failure as before; its message names the excluded machines and how many there are. An automatic profile whose `allow_tiers` only excluded machines satisfy is refused at once (`does not fit`, no Retry-After), and the message says "allow_tiers [...] admits only nodes excluded by config: ...".
- **Naming.** The outcome `refused` stays what it was: an ordinary refusal, judged against the expectation. `refused_by_exclusion` is the outcome described above. `not_judged` is a step or run result (next to `pass`, `fail`, `partial`), never an outcome.
- **The plan names the entry.** `POST /api/plan` has `excluded_by` (the exclude_nodes entry) when it refuses a profile that forces an excluded machine; `GET /api/profiles` takes `blocked_by_exclusion` from it.
- **Capability words per machine.** `GET /api/nodes`, the agents of `GET /api/status` and the MCP status view show `capabilities` for every machine (an empty list when there are none); the Nodes page says it in words.

### Fixed

- The Nodes page used an undefined name when it read the entry that excludes a machine (release 0.1.6).

## 0.1.6

### What is new

- **Memory reservation rule.** Before a load, Potluck can work out what each machine will take once the conversation has filled the context, and keep that much free. It is controlled by `fill_reserve_kib_per_token`, `placement_uses_reserved`, `margin_measured_mib`, `margin_estimated_percent` with `margin_estimated_min_mib`, and `reserved_drift_warn_mib`; all are off by default, so placement stays as before until you turn them on (runbook, "Tightening or loosening the reservation").
- **Per-machine memory breakdown.** `POST /api/plan` has a memory section that shows for each machine what the load will take, what the machine has and what remains. The Nodes page shows what is reserved on each machine against what it measures.
- **Cold-load benchmarks.** A load phase with the `cold` option first drops the model file from the file cache of every machine involved, then loads. The result says "verified" when every machine really ended with the file out of its cache, and "not guaranteed" with the reason otherwise. A new template, "Cold, reload, loaded", is a ready starting point. A benchmark that does not end ready now always unloads its model before the next one starts, and with the suite option `evict_others` a run no longer waits for other models that are loaded but idle: its loads may push them out like any load.
- **Stored measurements you can see and clear.** `GET /api/needs` lists every stored memory measurement with its shape, device count, llama build and whether it is trusted. `POST /api/needs/forget` forgets one.
- **File cache eviction on the agent and through Potluck.** Agents that can do it say so with the capability word `file_cache_evict` and accept a request to drop one model file from their file cache. `POST /api/cache/evict` asks the named machines, and Potluck's own, to do so; the cold-load benchmark uses it.
- **The busy file.** On a bare Linux install the agent keeps `/run/potluck/busy` in place while a model is starting, loaded or stopping, so other software on the machine (maintenance scripts, updaters) can tell not to disturb it. The agent's local status page shows the same as `busy`, `launches` and `busy_since`.
- **Start-up time from the real start.** When the start-up marks file carries `boot_origin`, the start-up figure counts from it, so a soft restart now includes its shutdown. Each record says where its figure counts from. With the console's `boot_ready` mark as well, the figure ends at the later of `boot_ready` and the moment Potluck found the machine placeable, so it is the same figure the console shows (`placeable_s` and `headline_end` keep Potluck's own point and say which ended it).

### What changes without any setting

- **Measured needs are kept by the shape of a load and the number of devices.** A measurement belongs to the model file and its memory-relevant settings, not to the profile name, so profiles of the same shape share it and deleting or renaming a profile keeps it.
- **Measurements are trusted only for the llama build they were made with.** A measurement from another build only ever raises a need until a load measures it again.
- **The need per device count applies even with every reservation setting off.** A load spread over more devices than were measured is counted with the extra per-device cost, so it can now wait in the queue where it used to start.
- **Command profiles are no longer shut out by `no_auto_nodes`.** A command profile runs on the local machine alone, so listing that machine in `no_auto_nodes` no longer stops it; excluding the machine still does.
- **Excluded machines are shown as excluded.** A machine named in `exclude_nodes` says "excluded" on the status and node pages, with the entry that excludes it, instead of looking like a normal idle machine.
- **Clean shutdown.** Stopping Potluck now stops every part that uses the database (the pool, the catalog build, the audit queue) before the database is closed, so no write is lost on the way out.
- **Fixed data race.** A scheduler query whose caller gave up could still write the caller's result; it no longer does.

### What to do at the roll-out

- Put the 0.1.6 programs in `programs/0.1.6/` and start the 0.1.6 scheduler. Agents in the field run 0.1.4: they are refused as too old and update themselves; a Docker machine needs the image `potluck-agent:0.1.6`.
- Keep the previous release's `release.json` (`programs/0.1.4/release.json`) in the programs folder. Measurements stored before 0.1.6 stay trusted only through it; without it they count only above the estimate until each profile loads again.
- Leave the reservation settings off at first. Turn them on one at a time and check the plan's memory section before and after.
- Expect some loads over several devices to queue where they started before (see the device-count rule above).

## 0.1.5

A scheduler image only: it still served and required release 0.1.4 of the agent.

- The profile listing reads one consistent configuration, and a lease of a profile deleted since it was granted no longer crashes the scheduler.
- Lock fixes: start-up records, history and configuration callbacks are written after the locks are released, and cancelling a benchmark no longer waits on itself.
- The memory estimate counts each device's compute buffers and no longer estimates a KV cache for the recurrent layers of hybrid models.
- The load event says, per machine, whether the model came from the file cache.
- The Benchmarks page shows external results: result sets measured outside the benchmark runner.

## 0.1.4

The refusal at arrival of a forced profile that can never fit; the unpause delivered to the agent before the node takes load; lowest available memory by kind and the file cache figures; the node feed and start-up records; the `local_node` setting and the "pool" wording; learned memory needs kept across a Potluck restart; no remount of a share that is mounted but not answering; stale start-up marks ignored after a soft restart.

## 0.1.3

The forced-fit check before any eviction; paused and unplaced nodes never evicted and a pause winning after a claim; the benchmark step facts; planned and blocked suites; the agent's file cache figure.

## 0.1.2

The rest of central logging: the agent's log shipper, Potluck's own lines, the read side.

## 0.1.1

The memory check before a launch with its review, and the first part of central logging.
