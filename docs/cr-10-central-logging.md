# CR-10: central logging through Potluck (proposal, revision 3)

Status: **accepted** (approved by Jacob and by the project manager). To be
built as part of phase 5. Nothing here is built or deployed yet.

## Decisions so far

| Question | Decision |
|---|---|
| Which store | VictoriaLogs |
| Machine's system log | Shipped in full. A deny list stays as a tool, empty by default |
| Who tells a machine where to push | Potluck, at registration, and it can change later without touching the machine |
| What stands in front of the store | **Potluck itself** is the proxy. No separate gate (no vmauth) |
| Retention | 14 days, 20 GiB |
| Laptops in Docker | No system log from the host by default |
| Client intake | **On by default**, with the limits of section 6; a setting switches it off |

The principle: a machine knows only Potluck's address. Role, programs and
now the logging endpoint are handed out by Potluck.

History: revision 1 put a queue inside Potluck; revision 2 let senders push
straight to the store behind a separate gate. Revision 3 keeps the dynamic
hand-out of revision 2 and puts a thin pass-through in Potluck instead of
the gate.

## Goal

- Potluck starts and owns the log store on its host.
- Everything lands there: Potluck's own log, the output of the programs it
  starts, each agent's log, the output of each launch, each machine's
  system log.
- Nothing is written to a console's USB stick. The journal in RAM stays as
  the local buffer.

## 1. The store

VictoriaLogs: one container with its own search page, indexes every field,
flags for retention by time and by disk size, runs offline, Apache 2.0.

Considered and not chosen: Grafana Loki (needs Grafana as a second
container for any UI; limits the number of labels), OpenObserve (always
requires its own accounts; heavier), Splunk (proprietary; the free licence
caps the daily volume; several times the size of Potluck).

Checked in its documentation: VictoriaLogs has no API keys and no
per-sender rules of its own, only one optional Basic-Auth user for the whole
server. With Potluck in front, that does not matter: the store is never
reachable from the network.

## 2. How Potluck runs the store

- Two containers in all: Potluck and `potluck-logs` (the store), in one
  compose file, like the share container today.
- **The store's port is not published.** Only Potluck reaches it, over the
  compose network. No new firewall rule: every machine already reaches
  Potluck's port.
- Data in a folder on the Potluck host, set in the compose file.
- Settings (ordinary Potluck settings):

| Setting | Default | Meaning |
|---|---|---|
| `logs_enabled` | on | Off: no `logs` object is handed out, the routes answer 404 |
| `logs_retention_days` | 14 | Passed to the store |
| `logs_max_disk_gib` | 20 | Passed to the store |
| `logs_push_url` | empty | Empty: Potluck's own route (section 3). Set: senders are told this address instead |
| `logs_client_intake` | on | Section 6 |

- If the store is missing or down, Potluck schedules exactly as before.

## 3. Where to push: the `logs` object

The registration answer carries a `logs` object:

```json
{
  "revision": 7,
  "url": "/api/pool/nodes/02:00:00:00:00:01/logs",
  "format": "jsonline",
  "auth": null,
  "limits": {"max_batch_bytes": 1048576, "max_line_bytes": 8192, "max_bytes_per_s": 262144, "every_s": 2},
  "deny_units": []
}
```

- `url` without a host means "the Potluck you registered with". If
  `logs_push_url` is set, `url` is a full address and `auth` may carry a
  credential. That is how the store can move, or a real gate can be added
  later, by changing one setting.
- `format` is the store's own JSON-lines intake: one JSON object per line.
- The heartbeat answer carries `logs_revision`. When it differs from the
  agent's, the agent reads the new object from
  `GET /api/pool/nodes/{id}/logs`. A changed setting reaches every agent
  within one heartbeat (2 s), without a restart.
- `logs` absent or `null`: the sender ships nothing.
- No credential is handed to any machine in the default setup.

## 4. Potluck's pass-through

### 4.1 Route for agents

`POST /api/pool/nodes/{id}/logs?source=<agent|launch|journal>[&launch_id=…]`

- Identification like the heartbeat: node id in the path, `agent_start_id`
  in a header. Unknown node or stale start id: 410.
- Body: JSON lines, optionally gzip. One request carries lines of **one
  source**, and for `launch` of one launch. The agent groups its batches
  that way.
- Per line the sender sets only what belongs to the line: time, message,
  `level`, `unit`, `stream`.

### 4.2 What Potluck does with a request

1. Checks the sender and the limits (4.3).
2. Adds the **trusted fields** for the whole request: `node_id`,
   `node_name`, `platform`, `release`, `source`, and for a launch
   `launch_id` and `profile` (looked up from the launch id). They are passed
   to the store as fixed fields of the request, so the **body is streamed
   through untouched**: not decoded, not re-encoded, not held in memory.
3. Forwards to the store and returns the store's answer.

A sender cannot write under another node's name: the fields that say who
sent a line come from Potluck. (Fallback if the trial shows the store does
not let fixed fields win over fields inside a line: Potluck rewrites each
line. More CPU, same behaviour. See section 9.)

No queue in Potluck. The agent's ring (section 5) is the only buffer.

### 4.3 Limits

| Limit | Value | When exceeded |
|---|---|---|
| Request body | 1 MiB (counted while streaming) | 413 |
| One line | 8 KiB | the agent cuts its lines to this itself. The store skips (does not cut) a line above its own limit of 256 KiB and logs a warning |
| Per node | 256 KiB/s, burst 2 MiB | 429 with `Retry-After` |
| Per node, at the same time | 1 request | 429 |
| All log requests at the same time | 8 | 429 at once, no waiting |
| Store does not answer | 5 s for the whole forward | 503 |

Answers: 204 accepted, 413, 429, 503, 410. Every refusal is counted per
node and shown in `/api/status` and on the Nodes page, so a noisy node is
visible.

### 4.4 Keeping it off the scheduler's paths

A log flood must not be able to slow a heartbeat, a load or a request.

- **No lock shared with the pool or the scheduler.** The log handler reads
  a snapshot (node id to start id, name, platform; launch id to profile)
  that the pool publishes whenever it changes. Reading it takes no lock.
- **A hard cap** of 8 log requests in flight. The ninth is refused at once;
  it never waits and never holds a connection open.
- **Its own client to the store**, with its own connection pool and the 5 s
  limit, so a slow store cannot use up anything the scheduler needs.
- **Bounded memory:** streaming with small fixed buffers; at most 8 requests
  times one buffer.
- **Same port.** It stays on Potluck's one address. The cap, not a second
  port, is what protects the API.
- A test in the build proves it: heartbeats and a chat request stay within
  their normal times while log senders flood the route.

### 4.5 Potluck's own lines

Potluck's log and the output of the programs it starts (`llama-server`) go
to the store with the same non-blocking sender the agent uses (section 5),
`source: scheduler` and `source: host`, with `profile` and the instance id.
The files it writes today stay.

## 5. Agent side

- **Shipping never blocks the agent.** Lines go into a ring without
  waiting; one background sender drains it. Heartbeat, launch control and
  watchdog never wait on a log line.
- The ring is **in RAM only**, 8 MiB by default, oldest lines dropped
  first. Never disk on a console.
- When lines were dropped, the next batch starts with one line that says
  how many.
- On 429 or 503 the sender waits (`Retry-After`, else a growing pause up to
  30 s). The ring absorbs what it can.

| What | Bare install (consoles, root) | Docker install (laptops) |
|---|---|---|
| Agent's own lines | copied into the ring, still printed as today | same |
| Launch output | read from the launch unit's journal | read from the child process's pipes |
| Machine's system log | **all of it**, read by following the journal in RAM; position kept in RAM | **not shipped by default**: the container cannot see the host's journal. Optional: mount the journal folder read-only and pass `--journal-dir` |

- `deny_units` from the `logs` object drops named units. Empty by default.
- After an agent restart the journal reader starts at "now": lines written
  while the agent was down are not shipped. Accepted, and stated here.
- The agent reports in its heartbeat: lines sent, lines dropped, last push
  error. Shown per node next to Potluck's refusal counters.
- Flags: `--logs off`, `--log-buffer-mib` (default 8).

## 6. Clients

- `POST /api/logs`, the same pass-through with its own limits. **On by
  default**; `logs_client_intake: false` switches it off (the route then
  answers 404).
- Potluck sets `source: client` and the caller's address itself; `actor`
  comes from the `X-Actor` header. A client cannot set `node_id` or
  another `source`.
- Limits: 64 KiB/s per client address, 1 MiB/s for all clients together,
  inside the same cap of 8 requests.
- The API has no authentication, so this intake is open to every machine
  that can reach Potluck. What bounds it: the limits above, and retention
  by time and disk size (the oldest logs of everybody go first when the
  disk cap is reached).

## 7. How a reader gets to it

- Potluck serves the store's search page under `/logs/` (a reverse proxy).
  One address, no login, like the rest of Potluck.
- A "Logs" entry in the Potluck UI's navigation.
- Links with the query filled in: from a node, from an instance or a failed
  load (that launch's lines on every node that took part), from a benchmark
  run (its time window).
- `GET /api/logs/query` passes a read-only query through, for scripts and
  for an MCP tool later.

## 8. Fields on every line

| Set by | Fields |
|---|---|
| Potluck (trusted) | `node_id`, `node_name`, `platform`, `release`, `source`, `launch_id`, `profile`; for clients `actor`, `client_addr` |
| Sender, per line | time, message, `level`, `unit`, `stream` |

`source` is one of `scheduler`, `host`, `agent`, `launch`, `journal`,
`client`.

## 9. Trial results

Run once on the Potluck host with a throwaway container
(`victoriametrics/victoria-logs:v1.54.0`), removed afterwards.

| Question | Result |
|---|---|
| Fixed fields for a whole request, set by the proxy | Works, as a query argument (`extra_fields=`) and as a request header (`VL-Extra-Fields`). **They win over fields of the same name inside a line**: a line that carried another `node_id` and `source` was stored with the proxy's values. The streaming design of 4.2 holds; the fallback is not needed |
| Stream fields per request | Works the same way (`_stream_fields=` or `VL-Stream-Fields`) |
| Line-size limit | Flag `-insert.maxLineSizeBytes`, default 256 KiB. A longer line is skipped with a warning, not cut; the lines after it are stored |
| Search page behind a path prefix | Works with `-http.pathPrefix=/logs` (page at `/logs/select/vmui/`) |
| Memory | 4 MiB at rest. A push of 400 000 real journal lines (32 MB of message text) in 1.5 s ran without a failed request inside a 512 MiB memory limit; without a limit it took about 1 GiB and kept it. The compose entry sets a limit of 512 MiB |
| Disk | 5 MB on disk for those 400 000 lines: about 0.15 MB per MB of journal text |
| Search speed | A word search over the 400 000 lines answered in 0.01 s |
| Version to pin | v1.54.0 (image 14 MB) |

## 10. Compatibility with release 0.1.0

Additive only.

- The registration answer gains `logs`; the heartbeat answer gains
  `logs_revision`; the heartbeat gains the shipping figures. A 0.1.0 agent
  ignores the unknown fields, ships nothing and keeps working unchanged.
- A new agent that gets no `logs` object (older Potluck) ships nothing.
- New routes only. No existing route changes. The launch request does not
  change (Potluck looks the profile up itself).
- Shipping needs a new agent binary, so it arrives with the next agent
  release, rolled out by the usual canary.

## 11. Which phase, and size of the work

Phase 5 (Potluck stands alone): Potluck's containers come under one compose
file there, and the main source of journal noise on the consoles (a login
every 5 s by the old dashboard backend) ends there. Until CR-10 is live,
the journal in RAM on each console (48 MB) is the only copy of its system
log and is lost on a restart.

| Part | Size |
|---|---|
| Trial (section 9) | done |
| Potluck: settings, `logs` object, pass-through with limits and counters, snapshot from the pool, compose entry, own lines, the flood test | 1 builder batch |
| Potluck: `/logs/` proxy, UI entry and links, query pass-through | half a batch |
| Agent: ring, non-blocking sender, own lines, launch output, journal reader, flags, heartbeat figures | 1 to 2 batches |
| Contract and runbook text, end-to-end test, canary on one console | half a batch |

Total 3 to 4 batches. A batch is the size of one of the phase-4 batches. No
firewall change and no third container.

## 12. Open questions

None. All questions of the earlier revisions are decided (table at the top).

## Sources

- VictoriaLogs security and authentication:
  https://docs.victoriametrics.com/victorialogs/security-and-lb/
- VictoriaLogs data ingestion (JSON lines, extra fields):
  https://docs.victoriametrics.com/victorialogs/data-ingestion/
