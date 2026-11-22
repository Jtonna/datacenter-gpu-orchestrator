# Releases

What each release of Potluck (the scheduler, `llm-scheduler`) and its node agent (`potluck-agent`) brings, newest first. The wire details are in the [agent contract](agent-contract.md), the day-to-day steps in the [runbook](runbook.md).

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
