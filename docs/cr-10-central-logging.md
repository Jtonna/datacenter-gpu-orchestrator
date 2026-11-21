# CR-10: central logging through Potluck (proposal, revision 2)

Status: **proposed, design only**. Nothing here is built or deployed.

Revision 2 follows two decisions and one change of direction:

- Decided: the store is **VictoriaLogs**.
- Decided: a machine's system log is shipped **in full**. A deny list stays
  as a tool, empty by default.
- Changed: senders push **straight to the store**. Potluck no longer carries
  the lines. Potluck tells every sender where to push and with which
  credential, so the store can move without touching a machine.

## Goal

One place to read every log of the cluster. Everybody still needs to know
one address only: Potluck. Potluck answers "where do logs go".

- Potluck starts and owns the log store on its host.
- Agents and clients ask Potluck for the push address and credential, then
  push to the store themselves.
- Everything lands there: Potluck's own log, the output of the programs it
  starts, each agent's log, the output of each launch, each machine's
  system log.
- Nothing is written to a console's USB stick. The journal in RAM stays as
  the local buffer.

## 1. Which store (decided: VictoriaLogs)

| Criterion | VictoriaLogs | Grafana Loki | OpenObserve | Splunk |
|---|---|---|---|---|
| Containers | 1 (2 with its auth proxy, section 3.2) | 2 (needs Grafana for a UI) | 1 | 1, large |
| Search UI built in | yes | no | yes | yes |
| Retention | flags for time and disk size | configuration file | setting | setting |
| Runs offline | yes | yes | yes | yes, with licence limits |
| Licence | Apache 2.0 | AGPL | AGPL | proprietary |

- It indexes every field of a line; Loki indexes labels only and limits how
  many we may use.
- Not Splunk: proprietary, the free licence caps the daily volume, and it is
  several times the size of Potluck.

## 2. How Potluck runs the store

- Containers in Potluck's compose file, like the share container:
  `potluck-logs` (the store) and `potluck-logs-gate` (its auth proxy, see
  3.2).
- Only the gate's port is published on the cluster network. The store's own
  port is not published.
- Data in a folder on the Potluck host, set in the compose file.
- Settings (ordinary Potluck settings): `logs_enabled` (default on),
  `logs_retention_days` (default 14), `logs_max_disk_gib` (default 20),
  `logs_push_url` (default: this host, the gate's port),
  `logs_client_intake` (default off).
- If the store is missing or down, Potluck works exactly as before. Logging
  is never a condition for scheduling.

## 3. Where to push: the `logs` object

### 3.1 What Potluck hands out

The registration answer carries a `logs` object:

```json
{
  "revision": 7,
  "url": "http://192.0.2.1:9428/insert/jsonline",
  "format": "jsonline",
  "auth": {"type": "basic", "username": "agents", "password": "..."},
  "stream_fields": ["source", "node_id", "unit"],
  "labels": {"node_id": "02:00:00:00:00:01", "node_name": "node-a", "platform": "linux-amd64-vulkan", "release": "0.2.0"},
  "limits": {"max_batch_bytes": 1048576, "max_line_bytes": 8192, "max_bytes_per_s": 262144, "every_s": 2},
  "deny_units": []
}
```

- `format` is the store's own JSON-lines intake. A line is one JSON object;
  the sender sets the fields in `labels` on every line, unchanged.
- `limits` are limits the sender keeps itself (see section 5 for what that
  means).
- The heartbeat answer carries `logs_revision`. When it differs from the
  agent's, the agent reads the new object from
  `GET /api/pool/nodes/{id}/logs`. A changed setting in Potluck therefore
  reaches every agent within one heartbeat (2 s), without a restart.
- `logs` absent or `null`: the sender ships nothing.

Clients do not register. They read the same kind of object from
`GET /api/logs/config`. It answers 404 while `logs_client_intake` is off.
The object carries the clients' credential and `labels: {"source":
"client"}`.

### 3.2 Authentication: what the store offers (checked in its documentation)

- VictoriaLogs itself has **no API keys** and no per-sender rules. Its HTTP
  server has one optional Basic-Auth user (`-httpAuth.username`,
  `-httpAuth.password`) for the whole server: writing, reading and the
  search page alike.
- Per-sender credentials and rules come from **vmauth**, a separate small
  proxy by the same makers. Per user it can limit which paths are allowed
  (for example `/insert/` only) and set fixed request headers, including the
  tenant (`AccountID`, `ProjectID`).

Recommendation: run vmauth as the gate from the start (one more small
container). Reason: with the store's own Basic Auth, the one credential that
lets a console write would also let it read and delete everything, and the
whole server would be open on the network. With the gate:

| Credential | May do | Tenant |
|---|---|---|
| `agents` | `/insert/` only | 0 |
| `clients` (only if client intake is on) | `/insert/` only | 1 |
| none | nothing | |

- Reading stays closed on the network. Readers go through Potluck
  (section 6).
- Client lines land in their own tenant, so a client cannot write into the
  cluster's own logs at all.
- Potluck generates the gate's configuration file and the passwords, and
  can rotate them: new password in the file, new `revision`, agents pick it
  up at the next heartbeat.

One shared credential per class is enough for a start. A credential per
node is possible later (Potluck writes one gate user per node) and would
buy back the trusted node label; see section 5.

Stated plainly: Potluck's API has no authentication, so any machine that can
reach Potluck can read the push credential. The credential keeps strangers'
lines and accidents out and lets us move or rotate; it does not stop a
hostile machine inside the network.

To confirm in the trial (one hour, throwaway containers on the Potluck
host, versions pinned there): the gate's path rule and tenant header work as
documented; whether the gate can also force a fixed field on each request
(needed for per-node credentials); the store's line-size limit flag; memory
in use with a day of our lines; the search page behind a path prefix.

## 4. Agent side

Kept from revision 1:

- **Shipping never blocks the agent.** Lines go into a ring without waiting;
  one background sender drains it. Heartbeat, launch control and watchdog
  never wait on a log line.
- The ring is **in RAM only**, 8 MiB by default, oldest lines dropped first.
  Never disk on a console.
- When lines were dropped, the next batch starts with one line that says how
  many.
- Store unreachable: the sender retries with a growing pause (up to 30 s);
  the ring absorbs what it can.

| What | Bare install (consoles, root) | Docker install (laptops) |
|---|---|---|
| Agent's own lines | copied into the ring, still printed as today | same |
| Launch output | read from the launch unit's journal | read from the child process's pipes |
| Machine's system log | **all of it**, read by following the journal in RAM; position kept in RAM | **not shipped by default**: the container cannot see the host's journal. Optional: mount the journal folder read-only and pass `--journal-dir` |

- `deny_units` from the `logs` object drops named units. Empty by default.
- After an agent restart the journal reader starts at "now": lines written
  while the agent was down are not shipped. Accepted, and stated here.
- The agent reports in every heartbeat: lines sent, lines dropped, last push
  error and its age. Potluck shows them per node in `/api/status` and on the
  Nodes page. This replaces the refusal counters of revision 1.
- Flags: `--logs off`, `--log-buffer-mib` (default 8).

Potluck ships its own lines and the output of the programs it starts
(`llama-server`) with the same non-blocking sender, `source: scheduler` and
`source: host`. The files it writes today stay.

## 5. What this direction loses and gains

### Lost against revision 1 (Potluck in the data path)

| Lost | Covered by | Still open |
|---|---|---|
| Potluck wrote the node labels itself. Now a sender sets them, so a machine can write under another node's name | Clients are kept apart for certain (own tenant). Per-node credentials later would fix it for agents, if the trial shows the gate can force a field | With one shared `agents` credential: open. Acceptable on a network where every machine is ours |
| Per-node byte limit enforced by Potluck | The agent keeps `limits` itself; the store's line-size limit; the gate's limit on parallel requests per user; retention by time and disk size bounds the total | A broken or hostile sender can ignore `limits`. The disk cap is then the only bound, and it works by deleting the oldest logs of everybody |
| Store port closed to the network | Only the gate is open, and only for `/insert/` with a credential | The gate's port must be reachable by every machine, and by clients if they push: **one firewall rule on the Potluck host** (needs root, so Jacob) |
| Refusal counters in Potluck | Heartbeat figures from each agent (section 4) | Nothing comparable for clients |

### Gained

- Potluck's process is out of the data path: no queue, no back-pressure
  code, and a log flood cannot touch scheduling at all.
- The store can move to another machine by changing one setting.
- Standard shippers can push to the same address later (for a machine that
  runs no agent).
- Less to build (section 10).

## 6. How a reader gets to it

- Potluck serves the store's search page under `/logs/` (a reverse proxy
  over the internal network).
- The proxy is **still worth it**: the gate opens writing only. Without the
  proxy we would have to open reading on the network too, with a second
  credential for people. With it, readers keep one address and no login,
  like the rest of Potluck.
- A "Logs" entry in the Potluck UI's navigation.
- Links with the query filled in: from a node, from an instance or a failed
  load (that launch's lines on every node that took part), from a benchmark
  run (its time window).
- `GET /api/logs/query` passes a read-only query through, for scripts and
  for an MCP tool later.

## 7. Clients

- Off by default (`logs_client_intake`).
- When on: the client reads `GET /api/logs/config` and pushes to the store
  with the `clients` credential. Its lines land in the clients' tenant.
- The client is asked to set `actor` (its name) on each line. Nothing
  enforces it.
- Limits are the ones in the object, kept by the client itself, plus the
  disk cap. See "still open" in section 5.

## 8. Fields on every line

Set by the sender from the `logs` object: `node_id`, `node_name`,
`platform`, `release`. Set per line: `source` (`scheduler`, `host`, `agent`,
`launch`, `journal`, `client`), `level`, `unit`, `launch_id`, `profile`,
`stream`. The store's own fields: `_time`, `_msg`.

`profile` on launch output: Potluck sends the profile name with the launch
request, so the agent can set it. (In revision 1 Potluck looked it up
itself.)

## 9. Compatibility with release 0.1.0

Additive only.

- The registration answer gains `logs`; the heartbeat answer gains
  `logs_revision`; the heartbeat gains the shipping figures; the launch
  request gains `profile`. A 0.1.0 agent ignores the unknown fields, ships
  nothing and keeps working unchanged.
- A new agent that gets no `logs` object (older Potluck) ships nothing.
- New routes only: `GET /api/pool/nodes/{id}/logs`, `GET /api/logs/config`,
  `GET /api/logs/query`, `/logs/`. No existing route changes.
- Shipping needs a new agent binary, so it arrives with the next agent
  release, rolled out by the usual canary.

## 10. Which phase, and size of the work

Phase 5 (Potluck stands alone): Potluck's containers come under one compose
file there, and the main source of journal noise on the consoles (a login
every 5 s by the old dashboard backend) ends there. Until CR-10 is live, the
journal in RAM on each console (48 MB) is the only copy of its system log
and is lost on a restart.

| Part | Size |
|---|---|
| Trial of the store and the gate on the Potluck host | 1 hour |
| Potluck: settings, `logs` object and routes, gate configuration and passwords, compose entries, own lines, heartbeat figures in status | 1 builder batch |
| Potluck: `/logs/` proxy, UI entry and links, query pass-through | half a batch |
| Agent: ring, non-blocking sender, own lines, launch output, journal reader, flags, heartbeat figures | 1 to 2 batches |
| Contract and runbook text, end-to-end test, canary on one console | half a batch |

Total 3 to 4 batches (revision 1: 4 to 5). A batch is the size of one of the
phase-4 batches. Outside the batches: one firewall rule on the Potluck host
for the gate's port.

## 11. Open questions for Jacob

1. The gate (vmauth, a third container) from the start, as recommended, or
   the store's single Basic-Auth user for a start (then reading is open to
   every machine that can write)?
2. One shared credential for all agents for a start (a machine could write
   under another node's name), or a credential per node from the start?
3. Retention: 14 days and 20 GiB as defaults?
4. Laptops in Docker: accept "no system log from the host" by default?
5. Client intake off by default?

## Sources

- VictoriaLogs security and authentication:
  https://docs.victoriametrics.com/victorialogs/security-and-lb/
- vmauth: https://docs.victoriametrics.com/victoriametrics/vmauth/
- VictoriaLogs data ingestion (JSON lines, stream fields):
  https://docs.victoriametrics.com/victorialogs/data-ingestion/
