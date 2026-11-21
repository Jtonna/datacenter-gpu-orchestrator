# CR-10: central logging through Potluck (proposal, revision 3)

Status: **accepted** (approved by Jacob and by the project manager). To be
built as part of phase 5. **Built, not deployed.** All three batches are
built on branches: batch 1, the Potluck side; the agent batch, the shipper
in `potluck-agent`; the last batch, the read side, Potluck's own lines and
the store's container (section 13 says what they hold and where they differ
from this text). Release 0.1.2 carries the last two. Nothing is deployed
yet.

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

## 13. As built, and where it differs from the text above

### 13.1 Batch 1 (Potluck side)

Built on a branch, not deployed: settings, the `logs` object, the
pass-through with limits and counters, the snapshot from the pool, the flood
test. Not in this batch: the compose entry, Potluck's own lines (4.5), the
agent (5), the `/logs/` page and query pass-through (7), the Nodes page. The
wire text is agent contract 11.3.

Where it differs from sections 2 to 10, and what those sections left open:

| Section | As built | Why |
|---|---|---|
| 2 | The store's address is the environment variable `LLM_SCHEDULER_LOGS_STORE_URL`, not a setting. Without it logging is off (no `logs` object, routes `404`), unless `logs_push_url` is set: then the object is handed out with that address and only Potluck's own `POST` routes stay `404` | The address differs per deployment. One rule for "no store" |
| 2 | `logs_retention_days` and `logs_max_disk_gib` exist as settings; nothing passes them to the store yet | The compose entry is a later batch |
| 3 | `revision` is a number computed from the logging settings and from whether there is a store address. It is not a counter, and it is the same again after a restart | Needs no stored state. Compare for equality only |
| 3 | When logging is off the heartbeat answer has no `logs_revision`; the agent takes that as "stop shipping" | The `GET` would answer `404` |
| 3 | `auth`, when a later change sets it, is `{"header", "value"}`. Always `null` now. `deny_units` is always empty: there is no setting for it | The text gave no shape and no setting |
| 4.1 | The header is `X-Agent-Start-Id` | The text named no header |
| 4.2 | The trusted fields go to the store as `extra_fields=name=value,name=value`. The store splits that argument at every comma and has no escape, so a comma in a value is stored as `%2C` and a percent sign as `%25`; a control character becomes a space; a value is cut at about 256 bytes; a field with an empty value is left out (the store refuses an empty value for the whole request) | Read from the store's source, v1.54.0: `app/vlinsert/insertutil/common_params.go` (`getExtraFields`, `getArray`) and `lib/httputil/array.go` (`GetArray`) |
| 4.2, 6, 8 | Every trusted field a request does not set is passed as `ignore_fields`, so a line cannot set it either (a client's line cannot name a `node_id`; a node's line cannot name a `profile`) | `extra_fields` only wins for the names it sets. **Not checked against the real store yet**; the trial of section 9 did not cover `ignore_fields` |
| 4.2 | `profile` is set when Potluck holds the launch for that node, or held it lately (the last 256 ended launches). An unknown `launch_id` is passed on as given, without `profile` | The last lines of a launch arrive after Potluck dropped it |
| 4.2 | The store's `2xx` becomes `204`; its `400` becomes `400` (`store_rejected`); every other answer, no answer and no connection become `503` | "Returns the store's answer" would hand the store's own texts to senders |
| 4.3 | More answers than the five listed: `400` (`bad_request`, `store_rejected`, `body_read`), `404` (logging off), `415` (an encoding other than gzip) | A wrong request needs an answer |
| 4.3 | The sender's body must arrive within 4.5 s (`400` `body_read`), inside the 5 s of the forward | A sender that stalls must not hold a place, and must not look like a silent store |
| 4.3 | Bytes are counted on the wire, so compressed bytes for a gzip body | The body is never decoded |
| 4.3 | A body with a declared length is checked against the rate before it is read. A chunked body is let in while the node has any budget left and charged after it streamed. A chunked body that passes 1 MiB is cut (`413`); lines already passed may be stored | The length is not known before |
| 4.3, 4.4 | A refused request is not read to its end. With a declared body of at most 64 KiB the connection is kept; otherwise it is closed with the answer | Closing on every refusal would make a busy sender open a connection per request |
| 4.3 | Refusals are counted per node in `/api/status`. The Nodes page does not show them yet | UI is not in this batch |
| 6 | Of the 8 places at most 2 are for clients at the same time | Clients are not identified. Without this, eight slow client requests would shut every node out |
| 6 | Bursts: 1 MiB per client address, 2 MiB for all clients together. At most 1024 client addresses are remembered; idle ones are forgotten | The text gave rates only |

Measured by the flood test on a development machine: 80 senders, about
240 000 log requests in 2 s against a store that never answers and about
280 000 against a store that takes 60 ms; the slowest heartbeat was 6 ms
and the slowest chat request 6 ms; never more than 8 forwards in flight.

Open for the agent batch (settled in 13.2): rule 1 of the agent contract's
section 2 (no time set by a node) against "time" in the per-line fields of
4.1 and 8.

### 13.2 Agent batch (the shipper in `potluck-agent`)

Built on a branch, not deployed, not in a release yet (the release string
was 0.1.1 then; release 0.1.2 carries it, see 13.3). Code:
`internal/agent/logship`, wired in `internal/agent/agent`, flags in
`cmd/potluck-agent`. The wire text is agent contract 11.3, items 11 to 15.

**Time.** Rule 1 of the contract's section 2 stays: a line's time is
Potluck's, never the node's. Potluck puts its own clock into the
registration answer and every heartbeat answer (`server_time_unix_ms`, only
while it hands out a `logs` object). The agent keeps an anchor: Potluck's
time at the middle of that request's round trip, on the agent's monotonic
clock (`CLOCK_BOOTTIME`). A line is stamped when it is produced: anchor plus
the monotonic time since. Every answer sets the anchor again. A line from
before the first anchor keeps its monotonic stamp and gets its time when it
is sent. `_time` is RFC3339 with nanoseconds in UTC, the store's default
time field (VictoriaLogs data-ingestion documentation: `_time` in ISO8601 /
RFC3339, fractional seconds allowed, nanoseconds since v1.9.0). Without an
anchor (a Potluck that sends no server time) a line has no `_time` and the
store uses the time it took it.

**Never blocks.** A producer puts a line into the ring and returns. The
ring's lock is held only for memory work; nothing does I/O under it, and no
lock of the heartbeat, the launch manager or the watchdog is held while a
log line is handled. The heartbeat only reads atomics and hands values over
(the anchor, the revision); a new object is read by the shipper's own
goroutine. One goroutine sends, one reads the object, one follows launch
output, one follows the journal. The sender has its own HTTP client.

| Section | As built | Why |
|---|---|---|
| 5 | Launch output is read from the launch's log file (the one `GET /launch/log` serves) on **both** installs, not from the child's pipes nor from the unit's journal | The launch's stdout and stderr already go to that file: systemd writes it (`StandardOutput=append:`), so the unit's journal has no output; the Docker child has the file as its stdout. Reading a file by offset can never hold the launch up; a pipe could. The file mixes both streams, so a line has no `stream` field |
| 5 | Launch output over `max_bytes_per_s` is skipped, not read; one line says how many bytes, and it counts as one dropped line | Reading a flood to drop it would cost CPU for nothing |
| 5 | A launch that exists when the agent starts (re-adopted) is followed from its end | Its earlier lines were shipped by the agent before the restart; the same rule as the journal |
| 5 | The journal is followed only while a `logs` object is held, from "now" at that moment, by `journalctl --follow --lines=0 --output=json` (needs systemd 236 or later for `--output-fields`). Missing `journalctl`: one warning, no journal | Without an object nothing may change on the node (section 10). No cgo in the agent |
| 5 | The agent's own unit (`potluck-agent.service`) is not shipped from the journal | Its lines are shipped as `source: agent` already; shipping both doubles them and lets a sender error come back as a journal line |
| 5 | A journal entry's time is its `__MONOTONIC_TIMESTAMP` (`CLOCK_MONOTONIC`) taken as an age off the agent's clock, through the same anchor. An entry of another boot gets the time it arrived and `time_source: arrival` | The realtime field is the node's wall clock |
| 5 | The journal reader keeps to `max_bytes_per_s` with a bucket of one batch; a line over it is dropped before it is parsed. A journal line longer than 64 KiB is dropped unread | Drop, never buffer |
| 5 | The agent's own lines from its start are kept until the first registration answer; an answer without `logs` throws them away | So the start of the log is not lost, and a node without logging keeps nothing |
| 5 | `every_s` is one round: each source with lines gets one batch in it, one request at a time, within the byte rate (a burst of one batch) | Otherwise three sources would share one request per 2 s |
| 5 | Answers: `429`, `503`, `404`, any other answer and a broken connection make the sender wait (`Retry-After`, else 1 s doubling to 30 s) and send the same batch again. `410` stops sending until the next registration, or for at most 60 s of the agent's clock (a `410` from the log route alone). `413` halves the batch; a single line answered `413` is dropped and counted. `400` and `415` drop the batch and count it | The contract's answers, and one rule for those it does not name |
| 5 | Lines dropped count: ring overflow, readers over the rate, too-long journal lines, rejected batches. Each source's next batch starts with one line saying how many of its lines were lost | Per source, because a request carries one source |
| 5 | `max_line_bytes` bounds the whole JSON line with its newline; `_msg` is cut on a character boundary to fit and the line gets `truncated: true`. Lines are also cut when they enter the ring | The store skips, it does not cut, an over-long line |
| 5 | The heartbeat carries `logs` (figures) only while a `logs` object is held | Without logging the heartbeat stays that of release 0.1.0 |
| 5 | Flags: `--logs on\|off` (default on), `--log-buffer-mib` (default 8, 1 to 4096), `--journal-dir` | As the text, plus the Docker journal folder |
| 3, 10 | `server_time_unix_ms` is added to both answers, set by Potluck, only while it hands out a `logs` object | The time rule above. With logging off the answers stay byte for byte those of release 0.1.0 |

Known limits:

- After a pause in which the monotonic clock did not count (a suspended
  virtual machine, for example), the lines produced between the end of the
  pause and the next answer are early by the pause. The next answer, within
  one heartbeat (2 s), corrects every later line.
- The Docker image has no `journalctl`; `--journal-dir` logs "journalctl not
  found" there until the image installs it (the `systemd` package).
- The starter (`deploy/agent/potluck-starter.sh`) is unchanged: shipping is
  on by default, so it needs no new option.

### 13.3 Last batch: the read side, Potluck's own lines, the store's container

Built on a branch, not deployed. Release 0.1.2 carries it with the agent
batch. Code: `internal/logrelay` (`reader.go`, `storeflags.go`),
`internal/selflog`, the tap in `internal/instance`, the routes in
`internal/api/logs.go`, the UI in `internal/api/ui/core/logs.js`, the MCP
tool `query_logs`, the service `potluck-logs` in `deploy/docker-compose.yml`.
The wire text is agent contract 11.3, items 16 to 19; the runbook section
"Central logging" says how to run it.

**Never blocks.** Potluck's log handler and a child's output writer put a
line into one RAM ring (8 MiB) and return; the ring's lock is held for
memory work only; when it is full the oldest lines go first. One goroutine
sends, with its own HTTP client. Nothing on a request path, in the pool or
in the scheduler waits on the store. Two tests prove it with a store that
never answers and a full ring: a chat request keeps its normal time while
four goroutines log as fast as they can, and a real child writing 16 MiB as
fast as it can runs to its end with every byte in its log file.

| Section | As built | Why |
|---|---|---|
| 2 | The store runs with `-http.pathPrefix=/logs`, so `LLM_SCHEDULER_LOGS_STORE_URL` is `http://potluck-logs:9428/logs` | Then `/logs/...` on Potluck is the same path on the store: no rewriting of the search page's links or of its API calls |
| 2 | Retention and the disk cap stay start flags of the store, in the compose file. Potluck reads the store's set flags back (`/flags`, at start and every 10 minutes) and warns in `/api/status` (`logs.warnings`) and once in its log when they differ from `logs_retention_days` and `logs_max_disk_gib` | The store has no API to change them. A flag the store was started without is its default (7 days, no cap; `app/vlstorage/main.go`) |
| 2 | Image pinned by digest: `victoriametrics/victoria-logs:v1.54.0@sha256:ee4e820c...` (the multi-platform index). Data in `./logs-store` next to the compose file, a bind folder like `./logs`, `./history` and `./data` | Digest read from Docker Hub on 2026-11-21 three ways: the registry's `Docker-Content-Digest`, the SHA-256 of the index it served, and the Hub tag API. A bind folder is easy to find and to wipe |
| 4.5 | Potluck's own lines and child output go straight to the store's `/insert/jsonline`, with the trusted fields of contract 11.3 item 6 set by Potluck; `node_id` and `node_name` are the local node's (`fleet.local_node`, else `local`) | They do not come from a node; the agent route needs a node and a start id |
| 4.5 | Child output is taken at the line writer that already feeds the last-40-lines ring and the load parser, in the copy goroutine of `os/exec`, just before the child's log file. Not by tailing the file | It works without a child log folder, it knows stdout from stderr (`stream`), and the work is a token-bucket check and a ring insert under a short lock: no I/O, nothing that waits. The flood test shows the child is not held up |
| 4.5 | Per child at most 256 KiB/s; the rest is skipped and one line says how many bytes, before the next kept line and when the child ends. The file keeps everything | One model's flood must not push every other line out of the ring |
| 4.5 | Lines from Potluck's start are kept until the sender first checks whether logging is on, then sent or dropped | The start of the log matters most and comes before the configuration is read |
| 7 | `/logs/` is a read allowlist, not a deny list: `GET`/`HEAD` on `/`, `/select/...`, `/flags`, `/metrics`, `/health`, `/robots.txt`, `/favicon.ico`; `POST` only on `/select/...`. `/select/vmalert/...` is refused too. Everything else `403` `read_only`. A path with `.`, `..`, empty segments, a backslash or NUL is refused, because the store folds `//` into `/` | The store has `GET` routes that change things (`/internal/force_merge`, `/delete/...`); a deny list would miss the next one. Routes read from the store's source, v1.54.0: `app/vlselect/main.go`, `app/victoria-logs/main.go`, `lib/httpserver/httpserver.go` |
| 7 | At most 8 reads at once for `/logs/` and `/api/logs/query` together, apart from the 8 places of the write side; the 9th is `429` `too_many_reads` at once. Own transport, no compression, answers stream | A slow query must not take a place a node needs to push |
| 7 | `GET /api/logs/query` answers the store's JSON lines unchanged; limit default 1000, at most 10000 (a larger one is cut; `X-Potluck-Limit` says which); 30 s timeout; the store's `400` is `400` `store_rejected` with its words | The store's own format is what LogsQL users know |
| 7 | The "Logs" entry and the links open the search page **in a new tab**, not in a frame | It is a whole single-page application with its own hash routes and layout; in a tab all of it works and the address can be shared. A frame would need sizing, a second scroll bar and care with its routes, for nothing |
| 7 | The links: a node, `node_id:="<id>"`, the last hour; an instance, `profile:="<profile>"` from a minute before it started; a failed load, `profile:="<profile>"` over the load's time plus a minute either side; a benchmark run, `*` over the run plus a minute either side | Launch ids are not in the status the UI has; the profile covers the host's output and every node's launch lines (Potluck sets `profile` on them) |
| 7 | The link format is vmui's own: `/logs/select/vmui/#/?query=<LogsQL>&g0.end_input=<ISO time>&g0.range_input=<N>s&g0.relative_time=none`, or `g0.relative_time=last_1_hour` | Read from the vmui source of v1.54.0 (`app/vmui/packages/vmui/src`): `constants/logs.ts` and `pages/QueryPage/hooks/useQueryController.ts` (query), `pages/QueryPage/hooks/useTimePeriod.ts` (the three `g0.*` keys), `utils/time/relative.ts` (preset ids), `utils/time/duration.ts` (the range format) |
| 7 | All of it is hidden while `status.logs` is missing or says `enabled: false` or `store: false` | Nothing to open then; the routes answer `404` |
| 7 | MCP tool `query_logs`, limit 100 unless given | An MCP answer is held whole |

Tests:

- The proxy rules (`internal/logrelay/reader_test.go`):
  `TestReadProxyPassesReads` (allowed), `TestReadProxyRefusesWritesAndAdmin`
  (forbidden, also with `..` and `//`), `TestReadSideOff` (no store,
  `logs_enabled` off), `TestReadProxyCap` (8 at once, `429` at once),
  `TestReadProxyStreams` (streaming).
- The query: `TestQueryPassThrough` (default limit, cap, bad parameters, the
  store's `400` and `500`), `TestQueryTimeout`.
- The store's flags: `TestStoreSettingsWarning`, `TestParseStoreFlagValues`.
- Through the API: `TestLogPagesThroughTheAPI`, and the off cases in
  `TestLogsOffChangesNothing`.
- The UI: `TestUILogsEntryAndLinks` (`internal/api/ui_logs_test.go`).
- Own lines and child output (`internal/selflog/selflog_test.go`):
  `TestOwnLinesReachTheStore`, `TestChildOutputReachesTheStore`,
  `TestDroppedLinesNotice`, `TestChildRateLimit`, `TestOffShipsNothing`.
- Never blocking: `TestOwnLoggingDoesNotSlowRequests` (`internal/api`) and
  `TestHungStoreSlowsNeitherChildNorLogging` (`internal/selflog`).
- The MCP tool: the `query_logs` row of the tool table test
  (`internal/mcpserver`).
- The compose file: `TestComposeLogStore` (`deploy/compose_test.go`).
- The release: `TestRelease012AndOlderAgents` (`internal/pool`).

Not checked yet; needs a real store or a browser: the search page through
the proxy (its assets, its `POST` queries and the live tail); the links
opening the right query and range; the `/flags` text of a running store;
Potluck's own lines and child output arriving with the right fields in a
real store; `ignore_fields` (as in 13.1).

## Sources

- VictoriaLogs security and authentication:
  https://docs.victoriametrics.com/victorialogs/security-and-lb/
- VictoriaLogs data ingestion (JSON lines, extra fields, the `_time`
  formats): https://docs.victoriametrics.com/victorialogs/data-ingestion/
- VictoriaLogs source, tag v1.54.0 (github.com/VictoriaMetrics/VictoriaLogs):
  the read routes (`app/vlselect/main.go`), the path prefix and the built-in
  pages such as `/flags` (`lib/httpserver/httpserver.go`), the retention and
  disk flags and their defaults (`app/vlstorage/main.go`), and the search
  page's link format (`app/vmui/packages/vmui/src`, files named in 13.3).
- VictoriaLogs querying (LogsQL, `/select/logsql/query` and its `limit`,
  `start`, `end`): https://docs.victoriametrics.com/victorialogs/querying/
- VictoriaLogs retention and disk flags (the sections on retention and on
  the disk usage limit): https://docs.victoriametrics.com/victorialogs/
