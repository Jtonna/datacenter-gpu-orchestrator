# CR-10: central logging through Potluck (proposal)

Status: **proposed, design only**. Nothing here is built or deployed.

## Goal

One place to read every log of the cluster, reached through the one address
everybody already knows: Potluck.

- Potluck starts and owns a log store on its host.
- Agents and clients push log lines to Potluck. Nobody else needs to know
  where the store is.
- Everything lands there: Potluck's own log, the output of the programs it
  starts, each agent's log, the output of each launch, and each machine's
  system log.
- Nothing is written to a console's USB stick. The journal in RAM stays as
  the local buffer.

## 1. Which store

Recommendation: **VictoriaLogs**.

| Criterion | VictoriaLogs | Grafana Loki | OpenObserve | Splunk |
|---|---|---|---|---|
| Containers | 1 | 2 (Loki has no UI of its own; it needs Grafana) | 1 | 1, large |
| Memory at this size | small | small to medium, plus Grafana | medium | large |
| Search UI built in | yes | no (Grafana) | yes | yes |
| Retention setting | one flag (time) and one flag (disk size) | configuration file | setting | setting |
| Runs offline | yes | yes | yes | yes, with licence limits |
| Login required | no | Grafana login by default | yes, always | yes |
| Licence | Apache 2.0 | AGPL | AGPL | proprietary |

Reasons:

- One process with its own search page, so one container and nothing to
  wire together.
- It indexes all fields of a line. Loki only indexes labels, and its rules
  about label count would force us to design around them (launch id and
  profile are exactly the labels we want).
- Potluck has no authentication today (accepted for this network). A store
  that insists on its own accounts (OpenObserve, Splunk, Grafana) adds a
  second login to a system that has none.
- Not Splunk: proprietary, the free licence caps the daily volume, and the
  image and memory use are several times larger than the whole of Potluck.

To confirm before building (a one-hour trial on the Potluck host, throwaway
container): memory in use with a day of our lines, the exact intake route
Potluck will forward to, and that the search page works behind a path
prefix (needed for section 5).

The store is an implementation detail behind Potluck. Agents and clients
never talk to it, so it can be replaced later without touching them.

## 2. How Potluck runs the store

- A second container in Potluck's compose file, like the share container:
  `potluck-logs`.
- Its port is not published. Only Potluck reaches it, over the compose
  network.
- Data in a folder on the Potluck host, set in the compose file.
- Settings (ordinary Potluck settings): `logs_enabled` (default on),
  `logs_retention_days` (default 14), `logs_max_disk_gib` (default 20).
- If the store is missing or down, Potluck works exactly as before. Logging
  is never a condition for scheduling.

## 3. Intake on Potluck

### 3.1 Agents: `POST /api/pool/nodes/{id}/logs`

- Same identification as the heartbeat: node id in the path,
  `agent_start_id` in a header. An unknown node or a stale start id gets
  410, like a heartbeat.
- Body: lines of JSON (one object per line), optionally gzip.

```json
{"ts":"2026-01-01T00:00:00.000Z","level":"warn","source":"agent","unit":"potluck-agent","msg":"...","fields":{"k":"v"}}
```

- `source` is one of `agent`, `launch`, `journal`.
- For `launch`: `launch_id` and `stream` (`stdout` or `stderr`).
- For `journal`: `unit` is the systemd unit, `fields` carries the priority.

Potluck adds the trusted labels itself and overwrites anything the sender
put there: `node_id`, `node_name`, `platform`, and, for a launch, `profile`
(looked up from the launch id). A sender cannot write under another node's
name.

### 3.2 Limits and back-pressure

A chatty or broken sender must not be able to hurt the scheduler.

| Limit | Value | When exceeded |
|---|---|---|
| Request body | 1 MiB after unpacking | 413 |
| One line | 8 KiB | cut, marked `truncated: true` |
| Per node | 256 KiB/s, burst 2 MiB | 429 with `Retry-After` |
| Potluck's queue to the store | 16 MiB in RAM, all senders together | 503 `store_busy` |

- The handler only checks, labels and appends to the queue. One separate
  worker sends the queue to the store. No lock is shared with the pool or
  the scheduler.
- Answers: 204 accepted, 429 slow down, 503 try later, 410 register again.
- Every refusal is counted per node and shown in `/api/status`, so a noisy
  node is visible.

### 3.3 Store down

- Potluck's queue fills, then Potluck answers 503.
- The agent keeps a ring buffer **in RAM only** (default 8 MiB, oldest lines
  dropped first). It never writes to disk on a console.
- When lines were dropped, the next batch starts with one line that says
  how many, so a gap is visible in the store.

### 3.4 Potluck's own lines

Potluck's log and the output of the programs it starts (`llama-server`) go
into the same queue directly, with `source: scheduler` or `source: host`,
plus `profile` and the instance id. The files it writes today stay.

## 4. Agent side

Rule above all others: **shipping logs must never block the agent.** The
agent's work (heartbeat, launch control, watchdog) does not wait on a log
line; lines go into the ring without blocking, and one background sender
drains it. (Finding of this week: a stalled system log on a console's USB
stick is the suspected cause of an agent standing still for seconds.)

| What | Bare install (consoles, root) | Docker install (laptops) |
|---|---|---|
| Agent's own lines | copied into the ring, still printed as today | same |
| Launch output | read from the launch unit's journal | read from the child process's pipes |
| Machine's system log | read from the journal in RAM by following it; position kept in RAM | **not shipped by default**: the container cannot see the host's journal. Optional: mount the journal folder read-only and pass `--journal-dir` |

System log filter, so the store is not filled with noise:

- Default: priority `notice` and above for all units; everything for the
  agent's and the launches' units.
- A deny list of units in the agent's registration answer (set in Potluck's
  settings), so it can be tuned without a new agent.
- The per-node limit of 3.2 applies to everything together.

After an agent restart the journal reader starts at "now": lines written
while the agent was down are not shipped. Accepted, and stated here.

Flags: `--logs off` turns shipping off on a node. `--log-buffer-mib`
(default 8).

## 5. How a reader gets to it

- Potluck serves the store's search page under `/logs/` (a reverse proxy).
  One address, and the store's port stays closed.
- A "Logs" entry in the Potluck UI's navigation.
- Links with the query filled in: from a node (that node's lines), from an
  instance or a failed load (that launch's lines on every node that took
  part), from a benchmark run (its time window).
- `GET /api/logs/query` passes a read-only query through, for scripts and
  for an MCP tool later.

## 6. Clients

- `POST /api/logs`, off by default (`logs_client_intake`, default false).
- Same line format. Labelled `source: client` with the caller's `X-Actor`
  name and address. A client cannot set `node_id` or `source`.
- Limits: 64 KiB/s per client address, 1 MiB/s for all clients together.
- Off by default because the API has no authentication: an open intake is
  an open way to fill the disk. The retention and disk settings of section
  2 bound the damage when it is on.

## 7. Labels on every line

`source`, `level`, `node_id`, `node_name`, `platform`, `unit`, `launch_id`,
`profile`, `stream`, and `release` (the agent's). Empty where they do not
apply.

## 8. Compatibility with release 0.1.0

Additive only.

- The registration answer gains `logs: {path, max_batch_bytes, every_s,
  deny_units}`. A 0.1.0 agent ignores the unknown field and ships nothing;
  it keeps working unchanged.
- A new agent that gets no `logs` field (older Potluck) ships nothing.
- The new routes do not change any existing route.
- Shipping needs a new agent binary, so it arrives with the next agent
  release, rolled out by the usual canary.

## 9. Which phase

With phase 5 (Potluck stands alone):

- Phase 5 already puts Potluck's containers under one compose file, which
  is where the store container belongs.
- The main source of journal noise on the consoles (a login every 5 s by
  the old dashboard backend) ends in phase 5.

## 10. Size of the work

| Part | Size |
|---|---|
| Trial of the store on the Potluck host, decision confirmed | 1 hour |
| Potluck: intake route, limits, queue, forwarder, own lines, settings, status counters | 1 builder batch |
| Potluck: compose entry, `/logs/` proxy, UI links, query pass-through | 1 builder batch |
| Agent: ring, non-blocking sender, own lines, launch output, journal reader, flags | 1 to 2 builder batches |
| Contract and runbook text, end-to-end test, canary on one console | 1 batch |

A batch here is the size of one of the phase-4 batches (built test-first,
full Linux and race runs, then a canary).

## 11. Open questions for Jacob

1. Store: VictoriaLogs as recommended?
2. Retention: 14 days and 20 GiB as defaults?
3. System log filter: `notice` and above by default, or everything?
4. Client intake off by default?
5. Laptops in Docker: accept "no system log from the host" by default?
