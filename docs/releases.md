# Releases

What each release of Potluck (the scheduler, `llm-scheduler`) and its node agent (`potluck-agent`) brings, newest first. The wire details are in the [agent contract](agent-contract.md), the day-to-day steps in the [runbook](runbook.md).

## Scheduler image: Zones backend, second slice (phase 4)

No agent change. No new listening port.

- **Uptime leaves removed machines out.** `GET /api/nodes/uptime`: a removed machine is not in `zones` and its seconds count in no group and not in the fleet.
- **History by zone group or list.** `GET /api/history?group=<name>` and `node=a,b,c` (ids or names); MCP `query_history` takes `group`.
- **Actions on a selection of zones.** `POST /api/zone-actions/{pause|unpause|force-unload}` with `nodes` and `comment`; `dry_run` answers what would happen (per zone effect, models that unload and every zone they leave, runs cut) for the confirm dialog. MCP `act_on_zones`.
- **Last load transfer rate per zone.** `last_load_link` {mb_s, bytes, load_s, at, model} from the newest ready load that sent the zone data (history `loadFinished`); a lower bound, not a link test.

- **Chip name.** `chip` per zone from its stats' GPU name (absent on a PS5, which reports none); `chip` per group (common name or `mixed`).
- **Needs attention first, defined by the server.** Each zone in `/api/nodes` (and `/api/status` agents) has `attention_rank`; `GET /api/nodes?sort=attention|name`; MCP `list_zones`. A group's `needs_attention` lists lost before set aside.

## Scheduler image: MCP access keys (phase 3)

No agent change, no new port. **`scheduler.db`: a new table `agent_access` with its own ledger `access_migrations` (version 1); `cfg_migrations` stays at 3**, so the previous image still opens the database (it ignores the two tables). See the runbook, "Access keys".

- **Access keys with expiry and permissions.** New keys start with `ptl_` and show as `ptl_••••7c2e`; each has an expiry (default 30 days, or never) and four permissions: Observe (allow, off), Switch zones, Load and unload models, Change settings (allow, ask, off). A new key may observe only unless set otherwise. Status: active, expiring (within 7 days), expired, revoked. Create and Manage take name, description, `expires_at` and `permissions`; the create and rotate answers carry the settings to paste into an MCP client (`mcp_settings`, URL from the `Host` header).
- **Enforced for requests that carry a key, on REST and MCP.** One route table (`internal/api/scopes.go`) decides what each route needs; beyond its permissions a keyed request gets 403 with a sentence naming the permission; over MCP the tools a key has Off are not listed. Ask me is refused until approvals exist. Unknown, expired and revoked keys get 401. A request without a key is the default user with full access, as before.
- **Existing keys keep working unchanged**: every key made before (`lls_...`) has every permission and no expiry until its permissions or expiry are set in Manage.
- **Expiry warnings.** History events `agentKeyExpiring` (once at 7 days, once at 1 day left) and `agentKeyExpired` (once), checked hourly, kept with the key so a restart does not repeat them. The health summary's MCP row says "· N expire soon" and needs attention while a key expires within 7 days.
- **Client credentials stay with the client.** A credential that is not a Potluck key (`Authorization: Bearer sk-...`, Basic, anything not beginning `ptl_` or `lls_`) is ignored and written nowhere (log, audit, events, request log). The header fix of v40 (below) keeps every client credential header away from the model servers.
- **Setting `default_user_name`** (`anonymous` unless set, no restart): the name of the default user in the audit log and the request log.
- **Going back to the previous image:** it starts on the migrated database and loses nothing. There, `lls_` keys work as before; `ptl_` keys work as identity when sent as `X-Agent-Key`, while a `ptl_` Bearer token is not read as a key (the request runs as the default user); expiry and permissions are not enforced; revoked keys stay revoked. A key rotated there keeps its permissions and expiry when this image returns, with the stored start as its hint.

## Scheduler image: client credentials stay with the client

Fix on top of v36. No agent change, no schema change, no new port.

- **No client credential header is passed on to a model server**, on every proxied route (`/v1/...`, `/upstream/...`), streamed or not, for a llama-server on the scheduler's machine and one hosted on a console alike. Dropped: `Authorization`, `Proxy-Authorization`, `X-Api-Key`, `Api-Key`, `X-Goog-Api-Key`, `X-Auth-Token`, `Cookie`, `X-Agent-Key` (one list, `proxy.ClientCredentialHeaders`). Before, a llama-server on the scheduler's machine received the client's `Authorization` header (often another provider's real key, sent by habit), and every model server received the client's other credential headers. A console-hosted server still gets exactly Potluck's own launch key (`Authorization: Bearer <launch key>`), never the client's value instead of it or beside it. The log store proxy (`/logs/`) drops the same list.
- A `command` profile whose own server checks a key from the client's header would now be refused by that server; none is known (llama-server launches never take the client's key).
- **What a model server still sees** (unchanged by this fix; see the runbook, "What a model server sees"): the client's other headers as sent, among them `User-Agent`, `Origin`, `Referer`, `X-Real-Ip`, `Via`, `X-Client-Ip`, `True-Client-Ip`, `Cf-Connecting-Ip`, `X-Request-Id`, `Accept-Language` and any custom header; and `X-Forwarded-For`, `X-Forwarded-Host` and `X-Forwarded-Proto` set by the scheduler itself (the connection's address, which behind a published Docker port is the bridge gateway, the host name the client used, and the scheme). The client's own `Forwarded` and `X-Forwarded-*` are not passed on.
- **Potluck's own headers stay with Potluck.** `X-Actor`, `X-Comment` and `X-Source` (who made a change, why, from where) are no longer passed on to a model server; Potluck still reads them for its audit and request log.

## Scheduler image: Zones backend (phase 4)

No agent change. No new listening port.

- **One event per start for the known machines.** The start writes one `knownMachines` event for the fleet (data `machines`: id, name, last_seen) in place of one `nodeMembership` event per carried machine, which filled each machine's recent events at every start. The per-machine warning stays, written only for a machine that ends up listed as lost. The events of v39 to v41 are still read.
- **Zone groups.** A zone is in one group or none. `GET/POST /api/groups`, `PATCH/DELETE /api/groups/{name}`, `POST/DELETE /api/groups/{name}/members`, `POST /api/groups/{name}/pause|unpause` (each member paused or unpaused, one audit entry each), MCP tools to match, `/ws` topic `groups` (sent only on change), `group` on each zone in `/api/nodes` and `/api/status` agents. New tables `zone_groups` and `zone_group_members` in `scheduler.db` with their own ledger `group_migrations`; an older image ignores them (runbook D, "Zone groups").
- **Uptime over days.** `GET /api/nodes/uptime?days=7` (MCP `get_uptime`): per zone, group, ungrouped and fleet, up (free, in use, loading) over counted (up, lost, set aside); switched-off time left out and given as `off_s`; `window.complete` false while the history is younger than the window. `GET /api/groups` carries `uptime_7d` per group. `/api/nodes/availability?group=` now filters to a group's zones (was 501). No schema change (reads `node_hours`).
- **One set of zone counts.** `GET /api/nodes` answers `counts` (total, reachable, up, down, lost, set_aside, off, in_use, free, loading) and each zone its `zone_state`; the Zones row of `/api/health` takes the same count and gains `up`, `in_use`, `free` and `loading` in its facts (its other facts and its text keep their meaning).

## Scheduler image: known machines across a restart (phase 4)

No agent change, no `scheduler.db` or `usage.db` schema change, no new port.

- **A machine that is off across a restart is listed.** It was not listed at all when its agent did not register again. The machines the previous run listed when it ended (active or lost, not removed or left) are read back from the event history; a machine an older image had already dropped stays unlisted. One that has not registered 65 s after the start is `lost` in `/api/nodes` and `/api/status` `agents`, with the new fields `absent_since_start` and `last_seen_at`, counts as down in `/api/health`, and its hours are lost in the availability series. Placement and plans do not see it. A removed machine stays removed, now under its name. It stays listed until its agent registers or the owner removes it: each start writes an info `nodeMembership` event for every machine it carries. `last_seen_at` is also set on any node that is lost or left, and lost `nodeMembership` events carry `last_seen`. See the runbook, "A machine that is off across a restart".
- **A lost machine is down, even when paused or excluded.** The Zones row of `/api/health` and the availability hours counted a lost machine that was also paused or excluded as switched off (and reachable in the row), while `/api/nodes` called it lost. Membership lost or left now wins: it is down; removed stays off.

## Scheduler image: system health (phase 2)

Ships after slice 1b. No agent change, no `scheduler.db` or `usage.db` schema change, no new port.

- **One judged health summary.** `GET /api/health` (MCP `get_system_health`, `/ws` topic `health` asked for by name, sent again only when a row's state, text, count or link, the heart or the recent lines change; evaluated every 2 s while subscribed, not at all without a subscriber) answers the System health popup's rows (Zones, Runs, Scheduler, MCP server, Storage, Queue, Alerts), each with a state, its sentence, a count and a link, the warnings of the last 2 hours in short sentences, and the heart's state, colour and issue count; it answers while the core loop is stalled. See the runbook, "System health".
- **Disk capacity and free space.** One reader runs `statfs` on the data, history, child log and models folders every 30 s with a 5 s timeout per read (a hung network share keeps its last figures and is marked `timeout`); `/api/system` `storage.folders` and `GET /api/models` `disk` read its cache.
- **Core-loop health in production** (B9): `/api/system` `loop` has the step in progress (`busy_now_ms`) and the longest step in the last minute, 10 minutes, hour and since the start; a step over the new setting `loop_step_warn_ms` (1000) is a `loopSlowStep` history event, at most one a minute.
- **Setting `models_disk_budget_gb`** (0, off): the Storage row also judges the model files against it.
- **`nodeQuarantined` events** add `reason` and `until` to their data.

## Scheduler image: live requests (slice 1b)

Ships with or after slice 1a. No agent change, no `scheduler.db` or `usage.db` schema change, no new port.

- **Fix after v32: a client that leaves mid-stream is cancelled.** It was `failed`, reason `aborted`, with every token null. Now it is `cancelled`, reason `client_left`, on `/v1/chat/completions`, `/v1/completions` and `/upstream` inference alike, and keeps what the stream said so far: `tokens_out` (counted from the stream; the message says "tokens_out counted from the stream"), `decode_tps`, `duration_ms`, and `tokens_in`, `tokens_reused`, `prefill_tps` when the request had asked for them (`timings_per_token`, `return_progress`). It counts as cancelled in the hourly and daily totals, with its tokens. `failed` stays for the model server or the scheduler failing; a model server that drops the stream is still `failed` (`aborted`), and now keeps the tokens counted so far as well. A non-streamed request whose client leaves is `cancelled` with its tokens null. Also fixed: a client that left before the answer began (non-streamed) was recorded `failed` (`upstream_error`); and a row's `stream` flag learnt at the end was not stored. Rows stored before the fix are left as they are (one such failed row is on live, 18:33 UTC on 23 Nov 2026).

- **Live rows on `/ws`.** A `requests` topic, asked for by name: a snapshot when a client subscribes, then each request as it is queued, starts and ends, and once a second the running rows with live figures. The snapshot always comes before the changes made after the subscription. With no subscriber nothing is built and the tick loop sleeps; nothing runs in the core loop.
- **Prompt figures while a request runs**, when the request asked llama-server for them (`return_progress` or `timings_per_token`): tokens in, reused tokens, prefill rate and `prefill_processed`. Progress events are not counted as output tokens, nor as the first token. A plain stream still reports the prompt only at its end.
- **Setting `requests_live_timings`** (off by default, no restart): adds `"timings_per_token": true` after the opening brace of streamed `/v1/chat/completions` and `/v1/completions` bodies that do not set the key themselves; the rest of the body is unchanged byte for byte and Content-Length follows. Never `return_progress`. Benchmark requests (`X-Bench-Run`) are left as sent. The client then sees a `timings` key on each event; first-token time and the events themselves are otherwise unchanged (to be measured on a console before it is turned on).
- **Machines for `/upstream` rows.** A passthrough row names the instance, load session and machines (with roles) that served the model, taken from the scheduler's snapshot without a lease.
- **Just-finished rows from memory.** The last 64 finished requests are kept in memory: `/api/requests` and `/api/requests/<id>` show a request the moment it ends (before the writer stores it), and with `usage.db` off. `history_begins_at` is `null` (not a negative number) when `usage.db` is off.
- **Token counting checked against the pinned llama-server** (`a01665d30de2`): `usage.prompt_tokens` counts every prompt token, reused ones included, and `prompt_tokens_details.cached_tokens` the reused ones; `timings.prompt_n` counts only the tokens read. Tests on the response shapes of that build.

## Scheduler image: usage store (slice 1a)

Ships after the own-slope image. No agent change, no `scheduler.db` migration: everything new is kept in `usage.db`, a second file in the data folder that an older image ignores.

- **Request log in its own file.** Every `/v1` and `/upstream` request is a row in `usage.db`, beside `scheduler.db` in the data folder (not in `scheduler.db`: no migration there, and an older image starts on the same folder and ignores the file). Status `queued`, `running`, `done`, `failed` (reason `stall`, `cut`, `upstream_error`, `upstream_status`, `aborted`, `scheduler_restart`), `refused` (the scheduler's reason) or `cancelled` (`client_left`, `client_left_queued`: a client that leaves while queued is now recorded); times (queued, started, first token, ended); profile and model; tokens in (reused ones counted, and also stored apart), out, prefill and decode rates; the machines with their role (host or helper); the caller (agent key name, else the default user) and source address; the benchmark run and the load session. Writes never run on the request path or in the core loop: one supervised writer drains a bounded queue in batches; a full queue drops non-final updates first and counts them.
- **Request routes and MCP tools.** `GET /api/requests` (paged, newest first, queued and running ones first with live token counts; filters `status`, `profile`, `node`, `from`, `to`, `bench`, `caller`; MCP `list_requests`), `GET /api/requests/<id>` (`get_request`), `GET /api/requests/summary?from&to&step=hour|day&profile` (totals, series, live counts, `oldest_ts`, `history_begins_at`; `get_request_summary`). With `usage.db` off, `history_begins_at` is `null` in both. `/api/system` shows the file under `storage.usage`.
- **Hourly and daily totals** of the request log, kept 13 months and for ever. Settings `usage_request_days` (90) and `usage_hourly_days` (396), restart required.
- **A full disk** never stops the scheduler or a request: the writes fail and are counted (`failed`, `last_error`, one WARN a minute at most), the store stays on and writes again once there is space. Each write runs in its own savepoint, so a failed one is undone alone; when SQLite drops the whole transaction (as it does on a full disk) the batch is abandoned and counted, never half written. About 0.5 KB on disk per request and 0.13 KB per machine-hour; the WAL is cut back to 8 MiB at most after a checkpoint.
- **A damaged `usage.db`** is moved aside as `usage.db.damaged-<UTC time>` with one WARN, and the history begins again; it never makes the scheduler unhealthy. `GET /api/metrics/activity` (the last 200 requests, in memory) is unchanged.
- **Availability, hour by hour.** A sampler notes each machine's state every 10 s (free, in use with how busy, loading, down, switched off) from what Potluck already holds, and keeps seconds per state per UTC hour in `usage.db`; hours it did not see read unknown. With `usage.db` off no sampler runs (no read of the core loop's state) and the route answers an empty series with `recording` false. `GET /api/nodes/availability` (MCP `get_availability`) answers hour or day cells with uptime and `history_begins_at`. See the runbook, "Availability, hour by hour".
- **Fix after the first day live: inference only, host first.** The first image recorded every `/upstream` call, monitoring polls included (`/slots` about once a second while a model is loaded). Now only a POST to one of the pinned llama-server's inference routes is a row, for `/v1` and `/upstream` alike; anything else passes through with no recorder work. The allow-list (one table, used by `/v1`, `/upstream` and the clean-up) also holds the routes a command profile serves: audio transcription, translation and speech, image generation. Usage migration 4 deletes the stored rows whose path is not on it, then rebuilds the hourly totals of each hour and the daily totals of each UTC day that held one from the rows that remain, in one transaction, with one INFO line: rows before, deleted, after, hours and days rebuilt. A row's `nodes` now list the host first: for a model hosted on the scheduler's machine, that machine (the local node) is the host, then the consoles as helpers; each machine has its `share`, absent for a host with no layers. `/api/nodes/availability` leaves out a removed machine for a range it was only off in, and sends seconds and `uptime` with 3 decimals at most.
- **Role and share per machine.** Each member of an instance has `role` (host or helper) and `share` of the model in `/api/status` and as `member` in `/api/nodes`; instances gain `host`, `host_node` and `parallel`.
- **Why a zone is set aside or off.** Quarantines say their `reason`, `message`, load and `since`; `/api/nodes` adds `set_aside`, and `paused_at`/`paused_actor`, `excluded_at`/`excluded_actor` from the audit log.
- **Free in N s.** A claimed or settling node has `free_at`, `free_in_s`, `free_basis` and `next` in `/api/nodes` (`free_at`, `free_basis` in `view.nodes`). Each of these facts is left out (not sent as `""`, `0` or `null`) where it does not apply, so an idle fleet's `/api/status` and `/api/nodes` read as before.
- **One writer per file, and a gate figure.** A second `usage.db` store in the same process is refused (off, one WARN). `/api/system` `storage.usage` reports `queued`, `queue_high_water`, `queue_size`, `dropped`, `dropped_terminal`, `failed`, `last_error`, `written` and `size_bytes` (the file and its WAL): after a day of recording, `dropped` and `dropped_terminal` at 0 and `queue_high_water` well under `queue_size` mean nothing was lost.

## Scheduler image after Phase 0: own slope

- **The model's own slope.** Where a model has measurements of one placement shape (same model file, slots, cache type, flash attention, extra arguments, host kind and device count) at two or more contexts, a profile of that shape at another context takes the straight line through the two rows nearest its context, in both directions, instead of the formula's step. A third row on the line within 128 MiB confirms it, and so does the formula's step when it predicts one of two rows from the other within 128 MiB: the figure then counts as a measurement; otherwise, with two rows only, it gets the estimate margin (+5%). A row off the line by more than 128 MiB, or a falling line, means no line on that count. A sliding-window model (the header's sliding window) is concave in the context: its rows below the window are not used, nothing is derived below the window, and between two rows its line is used only when confirmed. Lines never join rows of two device counts, and a line decides only for a placement on its own device count: it is never raised, lowered or kept for another count (a profile placed on a count without a line takes what it took before: its shape's measurements under the count rule, else the formula step). Order of sources for a placement on k devices: a measurement of the shape on k; the server-hosted twin (host: node); a confirmed line on k, which joins the shape's measurements on other counts (one on more devices, moved down by the per-device cost, still counts where it is higher); a measurement of the shape on another count, under the count rule; a two-point line on k; the formula-step derivation; the estimate. The basis reads "own slope of this model on N devices: S MiB per 1k context, from ctx A and B (latest DATE); confirmed by ctx C within 128 MiB"; `/api/profiles` adds `derived_slope_mib_per_1k_ctx` and `derived_slope_from_ctx`, a plan adds `derived_slope` (devices, `mib_per1k_ctx`, `from_ctx`, `confirmed_by`, `confirmed`). The formula step's pair check, as before, only pairs rows on the same device count. Validated on the live store: 294 leave-one-out predictions over seven models, worst error 0 MiB.
- **Model headers read in the background at start** (not in v26): the headers of every model the profiles and the stored measurements name are read by a background task (scheduler.header_reads), never in the scheduler's loop, so the first `GET /api/needs` after a start no longer waits on them (about 220 ms on live with v26); until the read is done a profile keeps its estimate and `derivation_check` lists no pair.

## Next scheduler image (after v26; no agent change)

- **History `node` filter takes an id or a name.** `GET /api/history?node=` (and MCP `query_history`) matches events that name the node by its id or its hostname, so the zone panel's "Recent events" (which asks by id) lists the zone's events.
- **A missing model file no longer blocks every configuration write.** A write is refused for a missing file only when it adds a profile or points one at a new file; profiles that already pointed at the missing file stay, other writes (settings, other profiles, deleting those profiles) go through. Loading such a profile is refused at plan time: "memory need unknown: model file "x.gguf" is missing from models_dir …".
- **`bench_max_run_minutes` applies without a restart.** The benchmark manager reads it when each run starts (it was read once at start, so a raised limit kept cutting runs at the old one until a restart). Its range is still 1 to 10080 minutes (720 allowed); the default stays 240: set 720 for a 6 to 7 hour Full pass.
- **`GET /unload` is deprecated; `POST /unload` added.** `POST /unload` unloads everything idle and answers `OK` like the GET. `GET /unload` still works but answers with `Deprecation: true` and a `Warning` header naming `POST /unload` and `POST /api/models/unload`, and logs one WARN per call with the caller's address (runbook, "Unload everything idle"). Nothing in this repository calls the GET.

## Next scheduler image (after v25; agent stays 0.1.9)

- **Derived needs.** A profile with no measurement of its own shape takes its need from the same model measured at another context (same slots, cache type, flash attention, extra arguments and host kind; the nearest context): the measured total with the part that grows with the window (KV cache, and the compute buffer on each device) recomputed by the estimate's formula. The direction matters: a model that uses less than the formula (sliding-window or MLA caches) is over-predicted going up and under-predicted going down. When the model has pairs of measurements at two contexts, each predicted from the other, and all are within 512 MiB, it derives in both directions as measured ("derived from the measurement at ctx N (date); …"). When its pairs disagree, or the source is more than 512 MiB below its own window's estimate, it derives upward only, with the estimate margin (+5%), and only where the figure is below the estimate path ("derived upward from ctx N (conservative: this model uses less than the formula)"). With one measurement and no pair: upward only, +5%. Downward otherwise, or when an upward pair under-predicts by more than 512 MiB (a model above the formula), the estimate stays. A derived figure is never below both the truth and the estimate without margin less 512 MiB; going up, never below the truth for a model at or below the formula. The basis says which; `/api/profiles` and `/api/models` add `derived_from_ctx`, and `GET /api/needs` adds `derivation_check` (the tolerance and every pair with its error). The first load records the shape's own measurement, which replaces it. The derivation's estimates and pair checks are computed once per state of the measurement store (a model's pairs again only when its own rows change), never on a configuration write, and a model's GGUF header is read once per version of the file, not again after each configuration write. At start the headers of every model the profiles and the stored measurements name are read in the background, never in the scheduler's loop; until that read is done a profile keeps its estimate (and `derivation_check` lists no pair). Order of sources: own measurement, then (host: node) the server-hosted one, then derived, then the estimate.

Shipped already in v25 (picked onto it), listed here for the record:

- **Plan rows say what they are built from.** Per node of a plan's `memory`, additive: `mem_available_mib`, `mem_available_avg_mib`, `mem_available_reading`, `watchdog_floor_mib`, `placeable_mib`, `placeable_adjust_mib`/`placeable_adjust_why` and `predicted_lowest_available_mib` (shared-memory nodes; `placeable_mib` on every node). `free_now_mib` (after the floor) and `available_mib` (after floor and headroom) are unchanged. The Placement preview prints "available A − floor F − headroom H = placeable P; after this load about L left". See the runbook's memory section.
- **`gpu_util_percent`** in `/api/nodes`: the busiest GPU's utilisation from the agent's stats.

## Next release: benchmark data model

The heading takes the release number when it ships.

### What is new

- **One benchmark on its own.** `POST /api/bench/benchmarks/<id>/run` (MCP `run_benchmark`) runs a benchmark without a suite. The run has `kind: "benchmark"` and `benchmark_id`; every other run has `kind: "suite"`. `GET /api/bench/runs` filters by `kind` and `benchmark`. See the benchmark guide, section 4.
- **The measurement record.** Every number a step measures is also a row of one shape: metric, value (of), unit, where it came from, what it describes, and its conditions (cold verified, first or steady generation, thinking, memory kind, scorer). `GET /api/bench/runs/<id>/measurements` (MCP `get_run_measurements`), the history across runs at `GET /api/bench/measurements` (MCP `query_measurements`), the metric list at `GET /api/bench/metrics`. See the benchmark guide, section 5.

- **Recipes and groups.** A benchmark with `inputs` (model, ctx, placement, thinking) instead of a profile is a recipe; the profile is made from the inputs before each step (`generated: true`, never over a profile a person wrote). Placements offer consoles only: `auto`, `all`, `any:N`, `nodes:a,b`; one that cannot be met is a refused step. Suites are also groups (`/api/bench/groups`): entries can use a recipe with inputs, a matrix of models, placements, contexts and thinking, or another group. `GET /api/bench/groups/<id>/expand` (MCP `expand_group`) previews the steps; `run_benchmark` takes `inputs`. A recipe's measurements are under the recipe with `variant`, `ctx`, `placement` and the condition `thinking_input`, and `query_measurements` filters by them. See the benchmark guide, sections "Recipes" and "Groups".
- **Editor and schema.** `GET /api/bench/schema` gains `input_options`, `entry_options`, `placement_forms`, the new limits and two templates: "Tasks" (an inline task set) and "Cold load recipe". The benchmark editor can make a benchmark a recipe and edits a tasks phase's inline set.

- **Transcripts.** Every step that sends prompts saves its full exchange (messages sent, answers, reasoning, timings) as `<data_dir>/bench-runs/<run id>/step-NNN.json`. `GET /api/bench/runs/<id>/transcripts`, `.../steps/<n>/transcript` (`?format=md`); MCP `list_run_transcripts`, `get_step_transcript`.
- **Reports, generated when opened.** `GET /api/bench/runs/<id>/report` (MCP `get_run_report`): the summary of a group run (headline counts, issues first, a table per metric, an index), the benchmark report of a step (`?step=`), one section (`&phase=`). Formats json, md and html (a print layout with Markdown and PDF downloads). Issues are facts only; nothing is judged.
- **Retention, off by default.** Setting `bench_retention_days` (default 0: off, nothing is removed). Set to N, it trims finished runs older than N days: requests and transcripts go, the run, its steps and its measurements stay. `GET /api/bench/removals/preview?days=N` lists what N would trim without changing anything; pinned runs (`PUT`/`DELETE /api/bench/runs/<id>/pin`, MCP `pin_run`, `unpin_run`) stay; every trim is logged (`GET /api/bench/removals`, MCP `list_run_removals`).
- **Task figures in exports and trends.** The markdown and phases CSV exports carry task counts and scorer totals, `detail=tasks` gives one CSV row per task with each scorer's points, and the trends gain `tasks_answered`, `task_decode_tps`, `task_points` and `task_points_pct` (`&scorer=`).

### What changes without any setting

- Nothing is removed: retention is off until `bench_retention_days` is set.
- Migration 8 adds `runs.pinned`, `runs.trimmed_at` and the table `run_removals`. Steps that send prompts write files under `<data_dir>/bench-runs/`.
- Migration 7 adds `measurements.variant`, `ctx` and `placement`; the measurements are mapped again once at the first start. Old suites run exactly as before.
- The database gains the columns `runs.kind` and `runs.target_id` and the tables `measurements` and `bench_meta` (migration 6). At the first start every stored run is mapped into measurements once; old runs read as before, as kind `suite`.
- The suite aggregates leave direct benchmark runs out; the benchmark aggregates and the history count them.
- Profiles made by recipes never set `host`: they run on the scheduler's machine (`host: server`, the default).
- Going back after this release has migrated the database (schema 8): an image with the unhealthy mode (the next scheduler image below and later) answers 503 on `/health` and every route with "database is newer than this version: 8 > 5" and changes nothing; 0.1.9 and older start with no profiles and no runs. Either way the data is untouched: start this release again, or restore the copy of `scheduler.db` taken before the upgrade.

## Next scheduler image (agent stays 0.1.9)

- **A `host: node` profile is planned from the server-hosted measurement** of the same shape (model, context, slots, cache, arguments) until a load hosted on a node measured its own; the host overhead is still added on the host node, and the basis says "hosted on the scheduler's machine (no measurement hosted on a node yet), plus N MiB host overhead". Stored peaks stay per shape.
- **The host node's own device share** (its `Vulkan0`, ... rows) shows under that node in an instance's `nodes`, like a worker's row; it used to be listed as unmatched. Host-side buffers stay in `host_memory`.
- **Profile `description`**: optional free text, one line, at most 500 characters. Shown in `/api/config`, `/api/profiles`, `/api/models` and both UIs' profile lists. Changing only the description does not make a loaded model stale.

## 0.1.10

An agent release on the same llama build as 0.1.9 (`03c9c28`): the scheduler code changes only to serve it, name its sha256 and refuse older agents.

### What is new

- **A discrete Nvidia GPU's `free_mib` follows the GPU**: taken from the Nvidia exporter at every heartbeat while that source is ok, instead of the `--list-devices` read from before the last launch (the Docker laptop showed 7,679 MiB free throughout).
- **The setup script opens 50061** (host launches) from Potluck's address with 50060 and 50052; a setup run again keeps an existing 50061 rule.
- **Same-release re-fetch**: the registration answer names the served agent's `agent_sha256`; a bare or RAM-only agent of the right release with another build fetches it once per (release, sha256) while it has no launch. A Docker agent ignores it (its binary is the image's). So a rebuilt agent of the same release reaches the consoles without a release bump.
- **Who calls the agent**: `--log-level debug` (default `info`) names the source address of each allowed API call, once per source per minute. Behind Docker Desktop every caller appears as the gateway.

### What changes without any setting

- Every agent must be 0.1.10: 0.1.9 agents are refused with `agent_too_old` and update themselves; a Docker agent exits (75) and needs the image `potluck-agent:0.1.10`.
- A 0.1.10 agent that meets a 0.1.9 scheduler is refused with `potluck_too_old` and waits, asking again, until the scheduler serves 0.1.10. Agents never downgrade.
- Stored memory needs stay trusted: a need is trusted when the llama build it was measured on is the host program's and the served release's build, and 0.1.10 names the same build as 0.1.9.

### What to do at the roll-out

- Make `programs/0.1.10/` on the scheduler's host:
  - `release.json`: `"release": "0.1.10"`, `builds` and `platforms` exactly as in `programs/0.1.9/release.json` (`builds.llama` stays `03c9c28...`, the full commit as written there).
  - `agent/`: the 0.1.10 `potluck-agent-linux-amd64`, `potluck-starter.sh` (unchanged, copy it), and `SHA256SUMS` over both.
  - The platform folders (`linux-amd64-vulkan`, `linux-amd64-cuda`, ...) copied unchanged from `programs/0.1.9/`, each with its `SHA256SUMS` (copied too: the files did not change).
- Keep `programs/0.1.9/release.json` (and the older ones) in place: only stored needs without a recorded build are matched through their release's `release.json`.
- Build `potluck-agent:0.1.10` for the Docker laptop (deploy/agent/Dockerfile with the served binary) before the switch.
- Start the 0.1.10 scheduler. The consoles are refused with `agent_too_old`, download 0.1.10 from the scheduler and come back; on the Docker node run `potluck-agent-setup --docker ...` again (or start the new image).
- Check: `curl -s $H/api/nodes | jq '.nodes[] | {name, state, release}'` shows 0.1.10 everywhere, and `GET /api/needs` shows the same trusted count as before.

## 0.1.9

### What is new

- **A console as host.** A profile with `host: node` runs its `llama-server` on the first of its `force_nodes`, started by that node's agent (agent contract 6.15), instead of on the scheduler's machine. Potluck never picks a console host by itself: only a profile asks for it, and it must force its nodes. The other forced nodes are its workers, as before. Requests go to the host node's port 50061 with a key made for that launch; the port is open only while the launch exists, and every route but `/health` refuses a request without the key.
- **The host's memory is counted.** The host node's reservation carries `host_overhead_mib` (default 1024) on top of its device share, and learned needs of node-hosted loads are kept under a shape of their own, never mixed with loads hosted on the scheduler's machine.
- **The plan says where it runs.** `POST /api/plan` and `GET /api/profiles` have `host` (`server` or `node`); for a node, `host_node`, `host_overhead_mib` and, when it cannot host now, `host_problem`. The host node's row in the plan's memory section has `host_overhead_mib`.
- **`host_no_mmap`** (default off): the host reads the model with `--no-mmap`, so its read progress counts. Leave it off until measured.
- **Agent: `--advertise-addr`** for a Docker agent behind NAT, and a WARN for an `--allow-from` that covers every address.
- **Agent: an Nvidia GPU under WSL2 or Docker Desktop** (`/dev/dxg`) is found and reported as `linux-amd64-cuda`.

### What changes without any setting

- Nothing for profiles without `host: node`: they run on the scheduler's machine as before.
- Every agent must be 0.1.9: 0.1.6 agents are refused with `agent_too_old` and update themselves.

### Fixed before release

- **A host launch asked the node for its whole budget.** `min_mem_available_mib` of a launch on a shared-memory node was the watchdog floor plus the node's whole budget (plus the host overhead on a host node), not this load's share; the agent then refused a console-hosted load that fit (`low_memory`, found on the canary). It is now the floor plus the node's `expect_mib`, for host and worker launches alike. Scheduler only: the 0.1.9 agent is unchanged.

### Known limits

- **No stall detection for a node-hosted load.** The stall rule needs Potluck's own byte count of an upload, and a host on a node is not Potluck's process, so a stuck node-hosted load ends only at its load timeout: the profile's `load_timeout`, else 120 s + 2 x (file size x share / link speed) over every node, the host node included. Set `load_timeout` on a `host: node` profile to bound it.
- **A console-hosted model is stopped at a scheduler restart**, not kept.
- **The tensor split is not reduced by the overhead** on the host node; give the host node a smaller split by hand if it is tight.

### What to do at the roll-out

- Make `programs/0.1.9/` on the scheduler's host: `release.json` with `"release": "0.1.9"`, the same `builds` (`llama` must be a fork build whose `llama-server` takes `--api-key-file`) and `platforms` as 0.1.6; `agent/` with the 0.1.9 `potluck-agent-linux-amd64` and its `SHA256SUMS`; the platform folders copied from `programs/0.1.6/` when the llama build did not change (each with its `SHA256SUMS`).
- Keep `programs/0.1.6/release.json` in place: stored needs measured under it stay trusted only through it.
- Start the 0.1.9 scheduler. The consoles are refused with `agent_too_old`, download the 0.1.9 agent from the scheduler and come back; a Docker node needs the image `potluck-agent:0.1.9`.
- Before the first `host: node` load, open the host node's port 50061 to the scheduler's address only; open 50052 between consoles only for a node-hosted load that also uses other consoles as workers.

## 0.1.8

### What is new

- **Tasks phase.** A benchmark phase `tasks` sends each task of a fixed task set once, in order, to the loaded profile (temperature 0, the set's seed, streamed; thinking follows the profile) and keeps every answer and reasoning text with its finish reason, prompt and completion tokens, time to first token, tok/s, wall time and error. A failed or timed-out task is recorded and the phase goes on; the phase has its own `timeout_s`, and what it did not send is recorded as `not_sent`. Potluck never runs an answer and never scores one. See the benchmark guide, section "Tasks".
- **Task sets** (format `potluck-tasks/1`) are stored through `POST /api/bench/task-sets` (MCP `create_task_set`), fixed per id and version, and identified by a content hash; a phase can also carry a set inline. A benchmark that names a set that is not stored is refused (422).
- **Scores.** `PUT /api/bench/runs/<id>/scores/<scorer>` (MCP `put_task_scores`) attaches a scorer's points to a run's tasks, replacing that scorer's earlier post; the run shows the total per scorer. `GET /api/bench/runs/<id>/tasks` (MCP `get_run_tasks`) and `.../tasks/<result_id>` (MCP `get_task_answer`) read the answers. External result sets stay for results measured outside Potluck.
- **Run page.** A tasks phase shows the set, tasks sent, answered, failed and not sent, the median decode speed, each scorer's total, and a row per task with a link to its answer.
- **Kit script.** `scripts/bench-tasks.py` builds a task set from a kit folder, pulls a run's answers in the kit's layout, and posts the kit's test scores or a ready scores file.
- **Background tasks are isolated.** A failure (an internal error) in one of the scheduler's background tasks no longer takes the scheduler down. The task restarts alone, rebuilding its state where it needs to (the event stream reconnects browsers to a fresh snapshot, the history writer continues in a fresh file, the benchmark manager fails the run in hand and unloads its models), or the work it was doing fails with an error. Architecture, "What restarts when", lists every task.
- **Failures are visible.** Each one is logged at error level with its stack, recorded as an `internalFailure` event, and counted. `GET /api/system` lists the tasks (`tasks`: restarts, failures in the last minute, last failure time and message) and `internal_failures_total`. The About page has a "Background tasks" card and warns after a failure in the last hour.
- **Bounded stop with exit code 3.** Three places still stop Potluck, because their state cannot be rebuilt in place: the scheduler loop, the pool's lost-node check and applying a configuration change. The same task failing three times within 60 s stops it too. The stop logs the reason, records it, kills every model process Potluck started, and exits with code 3 within 5 s; Docker starts it again.
- **No lock is left held.** A failure inside a section that holds the lock of shared state (the pool, the scheduler, the learned needs, the history, the model processes, the configuration, the benchmarks) releases the lock and stops Potluck the bounded way, whatever the task: the state may be half-written, and a restart rebuilds it from the database. A failure outside such a section follows the task's own policy.
- **One bad input counts once.** The members of one fan-out (the probes of one pre-flight, the launches or stops of one load, the members of a benchmark group, the nodes of one cache eviction) count as one failure toward the three-in-a-minute stop.

### What changes without any setting

- The database gains the tables `task_sets`, `task_results` and `task_scores` at the first start (migration 5); older runs read as before. The run JSON gains `phases[].tasks` only on tasks phases.
- The benchmark client keeps `finish_reason`, `usage` and the reasoning text of an answer, and the partial answer of a stream that was cut off.
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
