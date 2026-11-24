# Potluck runbook

What an operator does to install, run and repair a Potluck deployment. How the parts work is in [architecture.md](architecture.md); the first install from nothing is in the [README](../README.md).

Conventions:

- Commands run on the Potluck host unless a step says "on the node".
- `H=http://127.0.0.1:3122` in every example.
- There is no authentication. Add `-H 'X-Actor: your-name'` and `-H 'X-Comment: why'` to a write so the audit log says who and why.
- `jq` makes the JSON answers readable; it is optional.

| What | Where |
|---|---|
| Compose project | `/opt/potluck/` (project `potluck`) |
| Containers | `potluck` (scheduler), `potluck-share` (model share), `potluck-logs` (log store); optional `potluck-agent` |
| Data | `POTLUCK_ROOT`, default `/srv/potluck`: `data`, `history`, `child-logs`, `programs`, `logs-store` |
| Model files | The folder the `models` volume binds, at the bottom of the compose file |
| Ports | 3122 (API, UI, agents), 2049 (share, on the pool address only) |

---

## A. Before you start

1. Docker works and starts at boot: `docker ps` and `systemctl is-enabled docker`.
2. The NFS modules load at boot: `lsmod | grep -w nfsd`. If not, list `nfs` and `nfsd` in a file under `/etc/modules-load.d/`.
3. The Nvidia runtime works in containers: `docker run --rm --gpus all <your base image> nvidia-smi`.
4. The model files named by your profiles exist in the model folder and are readable by everyone.
5. A program release is ready for the scheduler's release (section E).

---

## B. Build the image and install

1. Build from the repository root:

```sh
docker build -f deploy/Dockerfile -t llm-scheduler:local \
  --build-arg BASE_IMAGE=<your base image> \
  --build-arg VERSION="$(git describe --always --dirty)" .
```

If the build stops with `base image has neither curl nor wget`, the base image cannot run the health check. Use a base image that has one of them.

2. Check the binary:

```sh
docker run --rm --entrypoint llm-scheduler llm-scheduler:local --version
```

3. Install the deployment files:

```sh
sudo mkdir -p /opt/potluck /srv/potluck
cp deploy/docker-compose.yml deploy/seed-config.yaml deploy/share-nfs.conf /opt/potluck/
```

Create `/opt/potluck/.env` with `POTLUCK_SHARE_BIND` (the host's address on the pool network) and `POTLUCK_SHARE_NET` (that network). Add `POTLUCK_ROOT=/path` to keep the data elsewhere. Set the model folder in the `models` volume of the compose file. Then check the file: `cd /opt/potluck && docker compose config -q` prints nothing when it is valid.

### First install and import

The configuration lives in the database. The seed file is read once, by `import-config`.

1. Edit `/opt/potluck/seed-config.yaml`: `local_node`, `tiers`, `profiles`. Every field is explained in `configs/example.yaml`.
2. Validate. Nothing is written:

```sh
cd /opt/potluck
docker compose run --rm --no-deps potluck import-config --check /seed/seed-config.yaml
```

Expected: `valid: N profiles, N tiers, ...`. An error lists every problem. `--no-model-check` skips the model file check on a machine without the files.

3. Import it as revision 1:

```sh
docker compose run --rm --no-deps potluck import-config --actor "$USER" /seed/seed-config.yaml
```

It refuses when the database already holds a configuration; `--replace` writes a new revision instead. Starting on an empty database also works: the scheduler logs `no configuration yet` and loads nothing until profiles exist.

---

## C. Start and check

```sh
cd /opt/potluck && docker compose up -d
docker compose ps                                        # potluck healthy, potluck-share and potluck-logs running
curl -fsS $H/health
curl -s $H/api/config | jq '{revision, configured}'      # 1, true
curl -s $H/api/system | jq '.programs'                   # served: true, no host_build_error
```

A dry smoke test that starts nothing, from the repository checkout:

```sh
CONFIG=/opt/potluck/seed-config.yaml sh deploy/scripts/smoke.sh $H
```

Expected: `PASS` lines, one plan line per profile, `SMOKE PASSED`. Add `--load <profile>` to send one real chat request, which loads the model and prints its memory per device. Unload everything idle afterwards with `curl -s -X POST $H/api/models/unload`.

---

## D. Nodes

### Add a node

**From the Zones page (Create new zone)**: make the zone first, then run the command it gives on the machine.

```sh
curl -s -X PATCH $H/api/config/settings -d '{"default_model_source":"share:<host>:/<path>"}'   # once, or give model_source per zone
curl -s -X POST $H/api/zones/pending -d '{"name":"zone-9","group":"rack-a"}'
# answer: {"id":"pz-…","model_source":"share:<host>:/<path>","command":"curl -sSL 'http://<potluck>/install.sh?zone=pz-…' | sudo sh", ...}
```

- `model_source` is the request's, else the setting `default_model_source` (section agents; empty by default). It is never guessed from the registered machines. With neither, the create answers 400 "no model source: set the default model source for new machines or give one for this zone" and no script is made; `GET /api/zones/pending` gives `default_model_source` ("" when unset) so the dialog asks for one. Forms: `share:<host>:/<path>` (an NFS export the machine mounts, as it reaches it), `dir:/<path>` or `push`; anything else is 400 (and refused as a setting). `mode` is `bare` (default) or `docker`. Extra arguments after `sudo sh -s --` go to `potluck-agent-setup` (for example `--allow-from` or `--no-firewall`).
- `GET /install.sh?zone=<id>` serves the script (`&download=1` as `potluck-zone-<name>.sh`); it fetches `potluck-agent-setup` from `GET /install/potluck-agent-setup` and runs it with `--potluck` (Potluck's address as the browser reached it), `--model-source` and `--name`. Nothing secret is in it: anyone on the LAN could fetch it, as they can the agent binary today. A person runs it on the machine; Potluck never connects out to install anything. It first refuses, changing nothing, on a console: any one of the fleet console's payload version file `/etc/node-payload-version`, its units `ps5-bootstrap.timer` or `node-agent.service` (in /etc, /run, /lib or /usr/lib systemd), `/etc/potluck/starter.env`, a root file system in memory, or the console GPU in `lspci` (consoles are set up by the fleet console). In doubt it refuses too: a machine whose host name starts with `ps5-` is refused unless the script is run with `--not-a-console` (for a machine that is truly not a console; the Create dialog never shows this flag, and it does not override any of the markers). It also refuses on the machine that runs the scheduler (Potluck's address, or the address its name resolves to, is one of the machine's own, or it is loopback). Then it prints what it will do (download, the setup command, the unit or container, the firewall rule, nothing rebooted and no other service restarted) before doing it; `sh -s -- --dry-run` (or the downloaded file with `--dry-run`) prints that and stops. Re-running it gives the same result (the setup replaces its own unit or container).
- The name must be new: a name (any case) that an active, lost or removed zone already has as its name or id, or that another waiting pending zone has, answers 409 ("the name … is already taken by the zone …").
- The first agent new to Potluck (an id Potluck has never listed) that registers with that name (any case) joins the pending zone: it reads `joined` with `joined_node`, and the machine is put in the group (audit entry `add_members`, comment "joined as pending zone …"). The name grants nothing else: no key, no other right. Registration is open on the LAN as before, from any address (the address that fetched the script is not checked), so a new machine that registers first under that name takes the group membership the owner chose, and nothing more. A zone Potluck already knows that re-registers under the name gets nothing from it, and the pending zone keeps waiting. `GET /api/zones/pending` lists them; `DELETE /api/zones/pending/<id>` cancels a waiting one. They expire after `ttl_min` (default 60, 5 to 60: at most an hour).
- Pending zones live in memory only, by design (they last an hour at most and hold nothing that must be kept; no table): a Potluck restart forgets them. The machine still joins under its name, without the group. A script fetched after the restart answers 404 with a script that prints "potluck: this zone was not found: create it again in Potluck" and exits 1; a cancelled or expired one answers 410 with "this zone was cancelled" (or "expired") and the same advice. The command is `curl -sSL … | sudo sh` (no `-f`) so that sentence reaches the person running it. `/api/groups` and its `/ws` topic carry them as `pending_zones`. MCP: `create_pending_zone`, `list_pending_zones`, `cancel_pending_zone`.
- Windows, the USB drive bundle and install over SSH are not served yet.

**By hand**:

On the node, as root, with the setup script from `deploy/agent/`:

```sh
sh potluck-agent-setup --docker --potluck 192.0.2.1:3122 --model-source share:192.0.2.1:/
```

- `--docker` runs the agent in a container (needed for Nvidia nodes today). `--bare` installs a systemd service.
- `--model-source`: `share:<host>:/` mounts the share; `dir:<path>` reads a folder on the node; `push` receives the weights over the worker connection.
- A RAM-only node runs `potluck-starter.sh` at every boot instead. It reads `POTLUCK_URL` and optional settings from `/etc/potluck/starter.env`.
- On the Potluck host itself, the agent needs `--allow-from 172.30.0.0/24`, because the scheduler's requests come from its container network. The compose file has an optional host agent for this (`docker compose --profile local-agent up -d`); it is not tested on real hardware.

The node must accept TCP `50060` and `50052` from Potluck. The script adds ufw rules when ufw is active, prints the rules for firewalld, and does nothing with `--no-firewall`.

Check: `curl -s $H/api/nodes | jq '.nodes[] | {name, state, placeable, unplaced}'`. A new node shows `state: active` within a few seconds.

### Which tier a zone is in

Every zone in `/api/nodes` and in `agents` of `/api/status` carries `tier`: the tier the scheduler matched it to. It has `index` (1-based position in the tier list), `priority`, `rule` (the match fields as stored, `{}` for none), `headroom_mib`, `shared_memory`, `link_mb_s` and `settle`. A zone that fits no tier has no `tier` and `tier_reason: "no tier matches"` (and `unplaced: no_tier_match`). A removed zone has neither. `GET /api/config/tiers` lists, next to `tiers`, `tier_zones`: one entry per tier with its `index`, its `zones` now (`id`, `name`; active and lost zones and the local node) and `never_reached: true` when a tier above it has an empty match. The configuration refuses to store an empty match (validation), so on a stored configuration `never_reached` is false; the field guards the page against one all the same. A catch-all such as `hostname: "*"` has the same effect but is not flagged.

Check: `curl -s $H/api/nodes | jq '.nodes[] | {name, tier: .tier.index, tier_reason}'`.

### Why a node gets no load

An active node that gets no load says why in `unplaced` (in `/api/nodes`, in `agents` and `view.nodes` of `/api/status`, and in the MCP status), with `unplaced_message`:

| `unplaced` | Meaning | Fix |
|---|---|---|
| `agent_unreachable` | Potluck cannot connect to the agent on 50060. The node can still register, because that goes the other way. | Open TCP 50060 and 50052 from Potluck on the node (`ufw status`). Check the agent listens: `ss -ltn | grep 50060`. From the host: `curl -m 3 http://<node>:50060/launch` must answer. Potluck retries every 15 s and logs `agent reachable`. |
| `programs_not_ready` | The agent has not fetched or verified its programs. | Read `programs_error` in the node's `agent` status. |
| `model_source_unreachable` | The node cannot read the model files. | Check the share mount on the node and the `potluck-share` container. |
| `no_tier_match` | No tier matches the node. | Add a tier, or fix its match (`platform`, `device`, `hostname`). |
| `paused` | The node is paused. With `unpause_pending: true`, the unpause reaches the agent with its next heartbeat. A plan that queues only because of such a node says "unpause pending: ... the load starts within about 2 s" (its action stays `queue`). | Unpause it. |
| `silent` | No heartbeat for 10 s from a node that runs a launch. | Wait: it is lost at 20 s or comes back. |
| `orphan_launch` | The node reports a launch Potluck does not hold. Potluck stops it. | Wait for the next heartbeat. |
| `bad_floor` | The agent's watchdog floor is larger than half its memory or 16384 MiB. | Reinstall the agent with a sensible `--watchdog-floor-mib`. |
| `host_machine` | The agent on the Potluck host itself. It is never a remote node. | Nothing. |

A refused agent logs `agent registration refused` with the reason: `agent_too_old`, `potluck_too_old`, `unsupported_hardware`, `duplicate_id`, `foreign_process`, `removed` or `release_unavailable` (agent contract 4.2).

### Pause, force unload, remove

Each action needs a comment and goes to the audit log (section `nodes`). The node id is its MAC address.

```sh
curl -s -X POST $H/api/nodes/02:00:00:00:00:01/pause        -d '{"comment":"why"}'
curl -s -X POST $H/api/nodes/02:00:00:00:00:01/unpause      -d '{"comment":"why"}'
curl -s -X POST $H/api/nodes/02:00:00:00:00:01/force-unload -d '{"comment":"why"}'
curl -s -X DELETE $H/api/nodes/02:00:00:00:00:01            -d '{"comment":"why"}'
curl -s -X POST $H/api/nodes/02:00:00:00:00:01/readmit      -d '{"comment":"why"}'
```

- **Pause**: no new load; a running load continues. Kept across restarts. A new request for a profile forced onto a paused node is refused at once (503 `forced nodes unavailable`, "paused by an operator", Retry-After 30 s); a request that was already waiting keeps waiting and is served after the unpause.
- **Force unload**: stops every model that uses the node now. Requests waiting for it fail with a 502 that names the force unload. Use pause when the node should only stop taking new loads.
- **Remove**: for a machine that is gone. It is refused at registration until it is re-admitted.
- **Uninstall** (on the node): `potluck-agent-uninstall` deregisters the node and removes everything the setup put there.

### Act on many zones at once

```sh
curl -s -X POST "$H/api/zone-actions/force-unload?dry_run=1" -d '{"nodes":["02:00:00:00:00:01","02:00:00:00:00:02"]}'
curl -s -X POST $H/api/zone-actions/pause -d '{"nodes":["02:00:00:00:00:01","02:00:00:00:00:02"],"comment":"rack work"}'
```

`nodes` takes zone ids or names (any case), resolved as `POST /api/load` resolves its nodes; the same zone given twice counts once; a name two zones share is refused 400 ("… is the name of 2 zones (…): name the zone by its id"), an unknown one 400 ("… is not a zone Potluck knows"). Actions: `pause`, `unpause`, `force-unload`, `drain`. With `dry_run` (body or query) nothing is done: the answer has, per zone, `effect` (plain words), `changes`, the `models` that unload because of it and `runs_cut`; for the whole selection `models` (each with `in_flight`, `host`, every zone it leaves in `zones` and those outside the selection in `outside`), `runs_cut` (each model's runs counted once) and a `summary` sentence. This is what the confirm dialog says. Without `dry_run` a `comment` is required and each zone is acted on as its own route does (one history event and one audit entry per zone); the answer has the plan worked out before acting and a result per zone. Each action is its own route: with an access key, `pause` and `unpause` need Switch zones, `force-unload` and `drain` need Load and unload models. MCP: `pause_zones`, `unpause_zones`, `drain_zones`, `force_unload_zones`.

`drain` is "wait for the runs, then unload": every model on the zones admits nothing new and unloads once its runs finish; nothing is cut. Its plan has `runs_wait` and `eta_s` (per model and the longest), from the profile's average run length (absent until a run of it has finished since Potluck started). A request arriving meanwhile queues and loads the model again once it has stopped (pause the zones first to have it placed elsewhere). The plan (and the drain answer) also says `next_load`: "The next load may start on another zone." The load that serves the requests queued meanwhile is placed afresh once the model has stopped: the drained zone may still be releasing its memory then, so another free zone can take it (seen on hardware). One model: `POST /api/models/drain/<model>` (answer `eta_s`, `next_load`), taken back with `POST /api/models/undrain/<model>`; the status shows `drain: true` and `drain_eta_s` while it drains; an instance not drained on request shows neither (its status is as before). A loading model is refused (409; force unload cancels a load). Audited as `unload` with `drain: true`, per zone as `drain`. MCP: `drain_model`, `undrain_model`, `drain_zones`.

### Load a model on chosen zones

```sh
curl -s -X POST $H/api/plan -d '{"model":"gemma","nodes":["ps5-02","ps5-03"]}'   # plan first: changes nothing
curl -s -X POST $H/api/load -d '{"model":"gemma","nodes":["ps5-02","ps5-03"]}'
```

`nodes` (ids or names the fleet view holds, in device order) places this one load as `force_nodes` would, without changing the profile: as `force_nodes`, the fit check is skipped and a load that can never fit there is refused. The instance shows `pinned_nodes` in `/api/status` (absent on any other instance) and is not reported stale for them; its next load follows the profile again. Refused with 409 (`already loaded or queued`) while the model is loaded, loading, stopping or queued: unload it first. Refused at once with 409 (`chosen zone unusable`) when a chosen zone is not in the fleet view, lost or offline, excluded, paused (also while an unpause is pending), set aside or outside the pool: a chosen-zones load never waits for such a zone, unlike a configured `force_nodes` profile. Audited as `load` with `nodes`. MCP: `plan_load` and `load_model` take `nodes`.

### A machine that is off across a restart

Potluck remembers the machines its previous run listed when it ended: active or lost in that run's list, not removed and not left. At start it reads them back from the event history (`schedulerStarted`, `schedulerStopping`, `nodeMembership` and the `nodeAdmin` removals; no table of its own). A run of an older image listed only the machines that registered in it, so a machine such a run had already dropped stays unlisted. A machine whose agent has not registered 65 s after the start is listed as `lost` in `/api/nodes` and in `agents` of `/api/status`, with `absent_since_start: true`, `last_seen_at` (when Potluck last heard from it) and `last_seen_ago_s`. It counts as down in `/api/health` ("7 of 8 reachable · 1 down") and its hours are lost in the availability series. It is never placed on: the scheduler's view does not hold it. Each start writes one info `knownMachines` event for the fleet at once ("Potluck knows 7 machines from its previous run ...", data `machines`: id, name, last_seen; it names no node, so it is not in any machine's recent events), and a `nodeMembership` warning "has not come back since Potluck started" for each machine that is listed as lost when the 65 s are over. The next start reads the machine and its last seen from them, so a machine off across several restarts keeps its first last seen, even when a run ends inside its 65 s.

- **Why 65 s**: an agent whose Potluck went away retries after 5 s, doubling up to 60 s; 60 s plus its 5 s request timeout covers the slowest agent that is up, so a running machine is not counted down for the length of a restart.
- **Last seen**: for a machine connected when Potluck stopped, the stop (`schedulerStopping`); after a crash, the last event of that run, which can be earlier than the crash.
- **How long**: listed as lost until its agent registers or the owner removes it.
- **Gone for good**: remove it (`DELETE /api/nodes/<id>`): it is then listed as `removed` under its name, not counted, and refused at registration until it is re-admitted; after a re-admit it is listed again only once its agent registers. A machine that deregistered (uninstall) is not listed after a restart.
- **Forgotten**: only if the history kept (20 files of 16 MiB by default) no longer holds the previous run's start and its events.

### Zone groups

A group is a name for a set of zones (for example one rack). Each zone is in one group or none. A group is a label and a way to act on many zones at once; it never changes where a model is placed (tiers do that).

```sh
curl -s $H/api/groups                                                   # every group with its figures, and the zones in none
curl -s -X POST $H/api/groups -d '{"name":"rack-a","members":["02:00:00:00:00:01"],"comment":"first rack"}'
curl -s -X POST $H/api/groups/rack-a/members -d '{"nodes":["02:00:00:00:00:02"]}'   # moves it out of its old group
curl -s -X DELETE $H/api/groups/rack-a/members -d '{"nodes":["02:00:00:00:00:02"]}'
curl -s -X PATCH $H/api/groups/rack-a -d '{"name":"rack-b"}'
curl -s -X POST $H/api/groups/rack-b/pause -d '{"comment":"maintenance"}'           # "Accepting models" off
curl -s -X POST $H/api/groups/rack-b/unpause -d '{"comment":"done"}'
curl -s -X DELETE $H/api/groups/rack-b                                  # its zones are then in no group
```

- **Names**: 1 to 64 letters, digits, `.`, `_` or `-`, starting with a letter or digit; unique without regard to case (`409` when taken). A member must be a zone Potluck lists (`400` otherwise).
- **Figures** per group (`GET /api/groups`): `members` and `member_names`, `count`, `counts` (total, reachable, down, lost, set_aside, off, in_use, free, loading; counted as the Zones row of `/api/health` counts them), `memory` (used_mib, total_mib, each_total_mib when all members have the same total), `temp` (avg_c, hottest_c and which zone, from the stats headline temperature), `accepting` (`all`, `none` or `mixed`: how many members are paused), `needs_attention` (members that are lost or set aside) and `models` (profiles placed on members, with how many). `ungrouped` has the same figures for the zones in no group. Each zone in `GET /api/nodes` and in `agents` of `/api/status` carries `group` (absent when in none).
- **Pause and unpause** act on each member as `POST /api/nodes/{id}/pause` does: one audit entry and one history event per member, the comment followed by "(group <name>)". A comment is required.
- **Audit**: every change is an entry in section `groups`, target `group:<name>`, signed by the caller (the default user unless an agent key or `X-Actor` names another).
- **Live**: the `/ws` topic `groups` sends the `GET /api/groups` document on subscribe and again only when a group changes.
- **MCP**: `list_groups`, `create_group`, `rename_group`, `delete_group`, `set_zone_group`, `remove_from_group`, `pause_group`, `unpause_group`.
- **Stored** in `scheduler.db`, tables `zone_groups` and `zone_group_members`, versioned by their own ledger `group_migrations`. An older image does not know these tables and leaves them alone, so rolling back keeps the groups for when you come forward again; the older image shows no groups.

### Keep a node out of the pool

Two node lists. Each entry is a node id or a hostname glob; matching ignores case; the whole list is replaced; add `?dry_run=1` to check first.

```sh
curl -s -X PUT $H/api/config/exclude-nodes -d '["console-test*", "02:00:00:00:00:01"]'
curl -s -X PUT $H/api/config/no-auto-nodes -d '["laptop-*"]'
```

- `exclude_nodes`: never used, not counted in capacity. Status and `GET /api/nodes` show `excluded: true`, `excluded_by` (the entry), `placeable: false` and `unplaced: excluded`; the agent keeps reporting. A profile that forces an excluded node is kept and refused at run time (503, reason `node excluded`, naming the entry) until the exclusion is lifted; the configuration write that creates the combination is accepted and lists each such profile in `warnings` and `blocked_by_exclusion`, and `GET /api/profiles` marks the profile with `blocked_by_exclusion: <entry>`. A benchmark step of such a profile ends `refused_by_exclusion` with the result `not_judged`: neither a pass nor a failure, and out of the pass rates. An automatic profile short of room because nodes are excluded gets the ordinary refusal and counts as a failure. Excluding a node does not drain it: a model already loaded there keeps serving until you unload it; the write lists it in `loaded_on_excluded_nodes`, and the node shows `running_while_excluded`. Unload the profile (`POST /api/unload`) to empty the machine.
- `no_auto_nodes`: used only by profiles that name the node in `force_nodes`; automatic placement never picks it or evicts from it. Status shows `auto: false`. A `command` profile runs on the local node alone by definition, so `no_auto_nodes` does not apply to it; an excluded local node refuses it (reason `node_excluded`).
- Excluding wins over both forcing and `no_auto_nodes`. Models already running keep running.

### Measure a soft restart

1. Pause the node and wait until nothing runs on it.
2. Announce the restart:

```sh
curl -s -X POST $H/api/nodes/<id>/expect-restart -H 'Content-Type: application/json' \
  -d '{"kind":"soft","comment":"measure a soft restart"}'
```

`409` says why it was not taken (not paused, or a launch is still on it). It holds for 10 minutes. Meanwhile the node's absence is logged as INFO, not as the ERROR "agent lost".

3. Restart the node: `sudo systemctl soft-reboot` on it (or `"kind":"full"` and a reboot).
4. Read the record: `curl -s "$H/api/nodes/<id>/startups?limit=1"` shows `requested: true`, `away_s` and, once ready, `headline_s` and `away_to_ready_s`.
5. Unpause the node.

---

## E. Program releases

The scheduler serves one release, the one it was built as (`programs.release` in `/api/system`). Its folder is `POTLUCK_ROOT/programs/<release>/`: `release.json`, `agent/`, one folder per platform, each folder with its `SHA256SUMS`. The rules are in agent contract 5.2.

Make the `SHA256SUMS` of a folder inside that folder:

```sh
cd /srv/potluck/programs/0.1.10/linux-amd64-cuda && sha256sum llama-server rpc-server node_exporter nvidia_gpu_exporter > SHA256SUMS
```

Check after a start: `curl -s $H/api/system | jq .programs`. `served: false` comes with `programs.error`, naming the missing or wrong file. `host_build_error` means the scheduler's own `llama-server-rpc` is not the release's llama build: every load is refused until it is.

### Move to a new release

1. Make `programs/<new>/`. Platform folders whose programs did not change can be copied. `agent/` holds the new agent; `release.json` names the new release; write new `SHA256SUMS`. Leave the previous release's `release.json` in place (section G): stored memory needs measured under it stay trusted only through it. A release that keeps the llama build (0.1.9 to 0.1.10) copies `builds.llama` unchanged: a need is trusted when its recorded llama build is the host program's and the served release's, so the stored needs stay trusted.
2. Build and start the scheduler of the new release (section F). This ends every loaded model.
3. Agents of the old release are refused with `agent_too_old` and update themselves. A Docker node exits and waits: run `potluck-agent-setup --docker ...` on it again, which builds the image for the new release.
4. Check that every node is back: `curl -s $H/api/nodes | jq '.nodes[] | {name, state, release}'`.

---

## F. Update or roll back the scheduler

Updating or restarting the scheduler ends every loaded model. Check nothing is mid-generation first: `curl -s $H/running`.

```sh
docker tag llm-scheduler:local llm-scheduler:previous          # the way back
docker build -f deploy/Dockerfile -t llm-scheduler:local --build-arg BASE_IMAGE=<your base image> \
  --build-arg VERSION="$(git describe --always --dirty)" .
cd /opt/potluck && docker compose up -d potluck
```

The agents register again by themselves. Models load again on the next request.

**Canary with a paused console.** When a roll-out pauses a console on the live scheduler (to point it at a throwaway scheduler for the canary), every new request for a profile forced onto that console is refused at once with Retry-After for as long as the pause lasts: for example, all the `*-6ps5` profiles. Requests that were already waiting keep waiting. So pause benchmark traffic for those profiles during the canary. The bench runner does not retry: it records such a step as `refused`, with the reason "paused by an operator", so a report shows why.

To roll back: `docker tag llm-scheduler:previous llm-scheduler:local && docker compose up -d potluck`.

What an older image does with what a newer one wrote (images after 0.1.9):

- **Configuration fields it does not know** (a new profile field, a new setting): it ignores them and serves everything else. It logs one WARN per field. `GET /api/system` lists them under `stored_config.unknown_fields`. While they are there, every configuration write answers `409 unknown_stored_fields`, because the write would drop them. Write with the newer image, or repeat with `?force=1` (`import-config --force`) to write anyway and drop them.
- **A database a newer image migrated** (a higher schema version), or a database or stored configuration it cannot use for any other reason: it does not start empty. It stays up, loads nothing and writes nothing. `/health` and every route answer `503` with the reason, for example `database is newer than this version: 6 > 5`. Start the image that wrote the database, or restore a backup this image can read (section H).
- **No database file yet** (a first start): it creates one and starts unconfigured, healthy.

Images up to 0.1.9 instead start healthy with built-in defaults and no profiles in both cases. Check `/api/system` and the profile count after any rollback to one of them.

---

## G. Measure `needs_mib`

The estimate from the model file is cautious, and several times too high for models with sliding-window attention (Gemma). Such a profile needs a measured `needs_mib`: the whole footprint (weights, KV cache, compute buffers), with no margin added. The scheduler warns at start about a sliding-window profile without one.

1. Create a measuring profile with the same `model`, `ctx`, `parallel`, `kv_quant` and `flash_attn` as the profile you size, and `force_nodes` set to the nodes it will really run on (the compute buffer is per device). The commented `gemma-measure` template at the end of `seed-config.yaml` shows the fields.

```sh
curl -s -X POST $H/api/config/profiles -H 'X-Comment: measure gemma 32k' \
  -d '{"id":"gemma-measure","model":"gemma-3-12b-it-Q4_K_M.gguf","ctx":32768,"kv_quant":"q8_0","flash_attn":true,"force_nodes":["console-1","laptop-1"]}'
```

2. Measure, from the repository checkout. The script loads the profile, waits for ready and prints the memory per device, the sum and a `needs_mib` line:

```sh
sh deploy/scripts/measure-needs.sh $H gemma-measure
```

3. Put the number in the real profile (`PUT /api/config/profiles/{id}`), unload, and delete the measuring profile.

A successful load also teaches the scheduler the measured size (`learned_needs` in `/api/status`). It is kept across restarts by the shape of the load (kind, model file, context, slots, KV cache type, flash attention, extra arguments) and the number of devices it ran on, not by profile name: profiles of the same shape share it, and deleting, renaming or changing a profile removes nothing. Every device holds its own compute buffer, so a measurement on one node is not used as is for three. The need on k devices is never below a measured total on fewer devices plus the estimate's per-device cost for each extra device, nor below a measured total on more devices less that cost for each missing one; with nothing measured on k or fewer devices it is the total measured on the closest count above, as it was taken. Above the largest measured count the estimate still counts. This applies with every reservation setting off too: a placement on more devices than were measured can queue where it used to start, and a count with its own measurement uses it even when another count measured more. After a restart, a stored figure is not lowered by the first load until that load ends normally (an unload, ttl, eviction or shutdown).

A measurement records the llama build it was made on. A stored measurement is trusted only when that build is both the scheduler's own `llama-server-rpc` build and the build the served release's `release.json` names (what the workers run). Otherwise it only ever raises the need, until a load measures it again. When either build cannot be read at start (a share that is slow right after a boot, a `release.json` briefly unreadable), nothing stored is trusted, and the start logs a warning saying which. Both are then read again in the background (after 5 s, doubling up to every 5 minutes) until they are known; the log line "llama builds known: stored memory needs of this build are trusted" says when trust was decided, and no restart is needed. `GET /api/needs` shows `host_build`, `release` and `served_build` ("" while not known), and for each row that is not trusted, `untrusted_reason`. Its `derivation_check` lists every pair of one model's measurements at two contexts with the error of predicting one from the other (`error_mib`, below 0 an underestimate); a profile is given a derived need in both directions ("derived from the measurement at ctx N") only when its model's pairs are all within `tolerance_mib`; otherwise only upward from a smaller context, with the estimate margin ("derived upward from …"), and never when an upward pair is an underestimate beyond the tolerance. Before any of that, a model with measurements of the same placement shape (device count included) at two or more contexts uses its own line ("own slope of this model on N devices: …") for a placement on N devices only, never carried to another count; a third row on the line within 128 MiB makes it count as measured. To see why a profile has no line: its rows on that count are fewer than two, one is off the line by more than 128 MiB, the line falls, or (a sliding-window model) its context lies between two rows with no third to confirm the line. To stop using a derived figure for one shape, load the profile once: its own measurement replaces it. Measurements made before builds were recorded (0.1.5 and earlier) carry only their release: at start they are stamped with the current build, and trusted, when their release's `release.json` in the programs folder names that same build (log line "stored memory needs stamped with the current llama build"); never by release name alone. Keep the previous release's `programs/<release>/release.json` in place at a rollout (the file alone is enough); without it those measurements count only above the estimate until each profile loads again.

List every stored measurement with `curl -s $H/api/needs` (shape, device count, total, who measured it, when, release, llama build, `trusted`, and the profiles that use it now). It is dropped when the model file's size changes; a model file replaced by one of the same size is not detected, so forget it by hand: for every profile of that shape with `curl -s -X POST $H/api/forget-need -d '{"model":"<profile>"}'` (the reply lists those profiles in `profiles_of_shape`), or by shape with `curl -s -X POST $H/api/needs/forget -d '{"shape":"<shape from /api/needs>"}'`.

### The host overhead of a host on a node, measured

A profile with `host: node` adds what its host process takes on the host node on top of that node's device share. A figure is kept per shape, node and device count, and never used across device counts: a host holding half the model (a split with workers) does other work than one holding all of it. The first hosted load of a shape on a node, on a given device count, plans with `host_overhead_mib` (default 1024). Once that load is ready and one full `free_average_window` has passed, Potluck reads what the node gave up for the load (its free figure when the load was placed less its free figure now, the average when known) less what the load log put on that node's own devices. Plans of that shape hosted on that node on that device count use the figure from then on, with the margin rules unchanged; every other node, and every other shape, keeps the setting. The plan says which it used: `host_overhead_source` (`setting` or `measured`) and `host_overhead_measured_at`, beside `host_overhead_mib`, in `POST /api/plan`, `GET /api/profiles` and on the host node's row of the plan's `memory` section. The history has a `learnedNeed` event with `host_overhead_mib`.

- Keep the higher: a new reading replaces the figure in force only when it is higher. It comes down only when the llama build or the model file changed (the next reading replaces it), or when three consecutive readings are all lower (then to the highest of the three). A load that ends abnormally (the watchdog, an OOM kill, a crash) gives the most the host took during that load as its reading. A reading below 0 or above 16384 MiB is not used (a warning event says so). The history event says the reading, the figure in force and the rule (`rule`: first, raised, kept, lowered, reset).
- A figure measured on another llama build, after a restart, only raises the setting; one measured on a model file of another size is not used.
- `curl -s $H/api/needs` lists them under `host_overheads` (shape, node, `devices`, `mib` (the figure in force) and `measured_at` (its date), `readings` (the last three, newest first, with dates), `log_host_mib` (the host buffers the log reported, shown only), who measured it, when, the llama build, `cause`, `used`, `profiles`), with `host_overhead_setting_mib` and `host_overhead_setting_only`. Forgetting the shape (`POST /api/needs/forget`, or `/api/forget-need` for a profile of it) forgets its host overheads too.
- With `host_no_mmap` off, weights mapped from the share are page cache, which the node's available memory counts as free: the reading leaves them out, as the agent's watchdog does.
- To plan every host with the setting again without losing the figures: `curl -s -X PATCH $H/api/config/settings -H 'X-Comment: host overhead from the setting' -d '{"host_overhead_setting_only":true}'`; false uses them again.
- They live in `needs_host_by_count` (ledger `needs_host_count_migrations` v1). The table of the release before device counts, `needs_host_overhead` (ledger `needs_host_migrations` v1), is left at version 1 and receives the one-device figure in force, so that release, after a rollback, opens the database and plans every host with the one-device figure, as it did. A reading it makes meanwhile is taken in at the next start of this release as a one-device reading, by the keep-the-higher rule (that release does not record the device count). Images before host overhead ignore both tables.
- The first start of this release copies each row of the release before as the one-device figure, and once its machine is known reads its readings back from the event history (the `learnedNeed` events with `host_overhead_mib` for that shape, machine and one device): the figure in force becomes the highest of them, with a history line "Kept the higher of the readings of the host overhead on <machine> …: <higher> MiB, not <latest> MiB." A split host measured by that release has no row of its own: it plans with the setting until its own split-host load measures it.

### The per-device overhead, measured

`device_overhead_mib` (default 600) is what one device of a placement holds beyond the estimator's share of the weights and KV cache and the compute buffer that grows with the context. It is added only to an estimated need; a measured need already holds it. A figure is kept per shape, node and device count, and never used across device counts.

- The reading: on every worker node of a ready load (never the node that hosts it), once per load, one full `free_average_window` after ready: what the node gave up for the load (its free figure when the load was placed less its average free figure now) less its share of the estimate and the compute buffer. It is never below the log's own part (what the load log put on that node less the same two), which no memory noise can lower. The history has a `learnedNeed` event with `device_overhead_mib`.
- A reading is taken only from a cold load on that node: the bytes the host sent there on the wire (measured, `loadFinished` `nodes_detail[].bytes`) reach at least half of what the load log put on its devices (`memory_mib`). After a cached reload the worker already holds the tensors (kilobytes cross the wire), the node gives up almost nothing, and the reading would be the log's part alone: no reading, and no warning. With `worker_model_dir` on, nothing crosses the wire, so no per-device reading is taken. No reading, host or per-device, is taken on a node that an instance that ran left less than one `free_average_window` before the new instance was created: the free figure taken as the base may still show the memory on its way back, and the reading would come out low (a quick unload and reload). Rows stored by the release before from cached loads are dropped once at start, decided from the event history: the `learnedNeed` event within 5 s of the reading that set the figure in force (same node by id or name, or the only such event when the view does not know the console's name yet; same device count and figure, from the row's own profile even when that profile is no longer configured, as a generated benchmark profile may not be), then the newest `loadFinished` of that profile at or before it on the same nodes. A dropped row gets a history line "Dropped the per-device overhead of …"; a row whose load is not in the history is kept. The check waits until every stored row's node is in the fleet view (events name consoles by hostname), at most two minutes after the first node is seen. Every start writes one log line ("stored per-device overheads checked against the event history") and one `learnedNeed` event "Checked N stored per-device overhead rows against the event history: D dropped …, C kept (a cold load), U kept (load not found in the history)" with `device_rows_checked`, `device_rows_dropped`, `device_rows_cold`, `device_rows_older_than_history` (read before the oldest event the history still holds: kept, nothing can be decided) and `device_rows_not_found`, also when nothing is dropped. The check waits because the scheduler's own machine is in the view at once and the consoles only with their first heartbeat: a check run before that knew the consoles by id only and found nothing (the release before this fix).
- No reading is taken from a configured `needs_mib`, a command profile, a model with a sliding window (its estimate is far off), a load whose log does not account for every node, or while a node's latest and average free figures differ by more than 128 MiB (it waits for the node to settle). A reading below 0 or above 4096 MiB is not used.
- Keep the higher, as for the host overhead. A figure from another llama build only raises the setting; one from a model file of another size is not used.
- Where it is used: where that shape's need on that device count is an estimate, on that node: the placer's per-node cost, the expected shares, and the plan's `memory` rows, which show `device_overhead_mib`, `device_overhead_source` (`setting` or `measured`) and `device_overhead_measured_at` for an estimated need.
- What the shape key leaves to replace: the same load stores the shape's own measured need, and a shape with a measured need plans from it (no per-device overhead at all). So a stored figure normally changes no plan; `used` in `GET /api/needs` is false while the shape plans from its own measurement. Moving a measured need to another device count keeps `device_overhead_mib` (a measurement is never used across counts).
- `curl -s $H/api/needs` lists them under `device_overheads` (the host overhead row's fields, with `log_part_mib` for the log's part), with `device_overhead_setting_mib` and `device_overhead_setting_only`. Forgetting the shape forgets them.
- To plan with the setting again without losing the figures: `curl -s -X PATCH $H/api/config/settings -H 'X-Comment: per-device overhead from the setting' -d '{"device_overhead_setting_only":true}'`; null uses them again.
- They live in `needs_device_overhead` (ledger `needs_device_migrations` v1). Earlier images never look at it and open the database as before.

Rolling back to a release before shape keys (0.1.5 and earlier) keeps the measurements: the older release reads its own table, which is left in place and also receives every new measurement under the profile that made it. It sees, for each profile, the highest figure of that profile's shape over every device count, written when that profile last measured; a profile whose shape was measured only by another profile falls back to its estimate until it loads once. Rolling forward again reads what the older release wrote; the higher figure wins.

---

## H. Configuration

All configuration is in `scheduler.db`. Every change is a numbered revision with an audit entry, and applies at once unless the schema marks it `restart_required`.

### Change a setting

```sh
curl -s $H/api/config/settings | jq .settings.ttl                      # the current value
curl -s $H/api/config/schema | jq '.fields[] | select(.key=="ttl")'    # type, default, bounds, description
curl -s -X PATCH "$H/api/config/settings?dry_run=1" -d '{"ttl":"10m"}'  # validate and show the diff
curl -s -X PATCH $H/api/config/settings -H 'If-Match: 3' -d '{"ttl":"10m"}'
curl -s -X PATCH $H/api/config/settings -d '{"ttl":null}'              # back to the default
```

`If-Match` makes the write fail with `409` when someone changed the configuration meanwhile. An invalid change is refused with `400` and nothing is written. The other sections: `POST /api/config/profiles`, `PUT|DELETE /api/config/profiles/{id}`, `PUT /api/config/tiers`, `PUT /api/config/exclude-nodes`, `PUT /api/config/no-auto-nodes`, `PUT /api/config/node-overrides`. A changed or deleted profile's loaded instance keeps serving, marked `stale_profile: true`, until it unloads.

### Add a model

1. Copy the GGUF file into the model folder.
2. Create the profile, after a dry run:

```sh
curl -s -X POST "$H/api/config/profiles?dry_run=1" -d '{"id":"qwen3-4b","model":"Qwen3-4B-Q4_K_M.gguf","ctx":16384}'
curl -s -X POST $H/api/config/profiles -d '{"id":"qwen3-4b","model":"Qwen3-4B-Q4_K_M.gguf","ctx":16384}'
```

3. It is usable at once. `curl -s -X POST $H/api/plan -d '{"model":"qwen3-4b"}'` shows where it would go.

### Delete a model file to the trash, and restore it

Needs the scheduler's models folder mounted read-write (see the note in `deploy/docker-compose.yml`); on a read-only folder both answer 409 `read_only`, "the models folder is read-only here", and change nothing.

```sh
curl -s -X POST $H/api/models/delete/Qwen3-4B-Q4_K_M.gguf -d '{"comment":"not used","dry_run":true}'   # size, profiles it takes out, purge_at
curl -s -X POST $H/api/models/delete/Qwen3-4B-Q4_K_M.gguf -d '{"comment":"not used"}'
curl -s $H/api/models/trash | jq '.items[] | {id, file, state, deleted_at, purge_at, profile_count, problem}'
curl -s -X POST $H/api/models/trash/<id>/restore -d '{"comment":"needed again"}'                      # rename_taken: true adds taken ids as <id>-restored
```

- The file is renamed into `<models>/.trash/<id>/` (same disk, one rename; it still uses its space). Every profile that names it leaves the configuration in one revision ("model deleted (...)") and is kept with the trash item; restore renames it back and adds them in one revision.
- **Interrupt** (`"interrupt": true`, after the owner confirmed): the model's instances are force unloaded as `POST /api/unload` with `force` does (requests in flight are cut; a load in progress is stopped and its waiting requests answered 503 `unloaded`), its queued requests are answered 503 `unloaded`, then the delete goes on. The answer has `interrupted: {instances, requests_cut, queued_cancelled}`; a dry run with interrupt shows `would_interrupt`. If an instance is still there after 5 s: 409 `interrupt_incomplete`, nothing moved, profiles unchanged. If something loads or queues the file between the interrupt and the revision: 409 `loaded_again`, the revision is undone, nothing moved. A benchmark run that touches the file still refuses.
- Refused (409, nothing changed): the model is loaded, loading, draining, stopping or has queued requests, without interrupt (`in_use`, with `instances`, `queued`); a benchmark run that touches it is queued, running or paused (`in_use`, `runs`); the file is a link or not a plain file (`not_regular`); it is already in the trash (`in_trash`); restore when a file of that name is back (`file_exists`) or a profile id or alias is taken now (`profiles_taken`).
- A configuration write or rollback that adds a profile on a trashed file is refused: "file ... not found under models_dir ...: it is in the trash since ... (trash id ...); restore it first".
- `model_trash_days` (default 30) sets each item's `purge_at`. Nothing removes files from the trash yet (`purge_on: false`): the trash only grows until the purge ships.
- A kill in the middle of a delete or restore is repaired at the next start from where the file is; what it cannot tell is marked `interrupted` with a `problem` in the trash list and a warning in the log. Files are never moved by the repair.

### Read the audit log

```sh
curl -s "$H/api/audit?limit=20" | jq '.entries[] | {id, ts, source, actor, action, section, target, revision}'
curl -s "$H/api/audit?section=profiles&target=qwen3-4b"
curl -s $H/api/audit/42 | jq '{before, after, diff}'
```

`source` is `ui`, `rest`, `mcp`, `import` or `system`. Loads and unloads asked for from the web UI (source `ui`), over REST or with MCP are in section `runtime`. The scheduler's own actions (benchmark loads and unloads) have source `system`.

### Roll back

```sh
curl -s "$H/api/config/revisions?limit=10"
curl -s $H/api/config/revisions/7 | jq .config
curl -s -X POST $H/api/config/revisions/7/rollback -H 'X-Comment: bad tiers'   # a new revision equal to 7
```

### Backup and restore

The configuration, revisions, audit log, benchmarks and learned needs are in `/srv/potluck/data/scheduler.db`, with its `-wal` and `-shm` files. The request log is in `usage.db` in the same folder; the copy below takes both.

```sh
cd /opt/potluck && docker compose stop potluck && sudo cp -a /srv/potluck/data /srv/potluck/data.bak-$(date -u +%Y%m%dT%H%M%SZ) && docker compose start potluck
curl -s $H/api/config/export > config-export.json                       # configuration only
```

Restore the database: stop `potluck`, replace `scheduler.db`, delete `scheduler.db-wal` and `scheduler.db-shm`, start it. Restore a configuration export into any scheduler: `curl -s -X POST "$H/api/config/import?replace=1" -d @config-export.json`.

---

## I. Day-2

### Read the status

```sh
curl -s $H/api/status
curl -s $H/api/nodes
curl -s $H/running
```

- `/api/status`: `instances` (each with `profile`, `state`, `nodes` with memory per device, `log_file`), `queue`, `backoffs`, `recent_loads` (loads that finished in the last 120 s), `learned_needs`, `agents`, `view.nodes`, `logs`.
- `/api/nodes`: the node feed for dashboards: `{time, seq, nodes}`. Each entry is an entry of `agents` of `/api/status`, plus Potluck's own machine with `"local": true`. A new `seq` means something changed besides ages and uptimes.
- `/running`: the loaded models, llama-swap compatible, with `recent_loads`.

### Unload everything idle

```sh
curl -s -X POST $H/api/models/unload   # JSON: {"msg":"ok","unloaded":[…]}
curl -s -X POST $H/unload              # plain "OK", llama-swap's shape
```

`GET /unload` does the same and still works for llama-swap clients, but it is deprecated because a GET should not change state: its answer carries `Deprecation: true` and a `Warning` header naming the POST routes, and every call logs one WARN line with the caller's address. Find the callers before it is removed:

```sh
docker logs potluck 2>&1 | grep 'GET /unload is deprecated' | tail -50
```

### Watch a load

```sh
curl -s $H/running | jq '.running[] | select(.state=="starting") | .load'
```

The `load` object has `elapsed_s`, `bytes_total`, `bytes_sent`, `progress`, `eta_s`, `source` (`measured` or `estimated`) and one entry per worker with its state (`waiting`, `receiving`, `done`). When bytes stop rising for more than 15 s, `eta_s` disappears and `stalled_s` appears. After `load_stall_timeout` (180 s by default) the load fails.

### Start-up figures

```sh
curl -s $H/api/startups
curl -s "$H/api/nodes/<id>/startups?limit=5"
```

The figure is the seconds from boot start to ready for model loads, grouped per node and kind of start (`full`, `soft`, `agent_restart`). Never compare across kinds. A record without a figure says which condition never came (`missing`) or what held the node back (`blocked_by`). A Docker agent reports full starts and agent restarts only.

### Fit check for forced profiles

A profile with `force_nodes` skips the automatic placer. On agent nodes Potluck still checks each node's share of the need against its usable memory, and the agent refuses a launch that does not fit (`would_not_fit`). A forced profile that can never fit its nodes, even with nothing loaded on them, is refused at once with `503` "forced nodes do not fit", and nothing loaded is stopped for it. The setting `forced_fit_check` (`off`, `warn`, `reject`) adds the same check on nodes without an agent.

### Memory safety on shared-memory nodes

A node whose GPU uses system memory freezes when a load takes all of it, so the scheduler plans conservatively. Three settings (defaults in `GET /api/config/schema`):

- `split_skew_margin` (0.15): a split model does not land evenly, so each shared-memory node's share must fit with this margin.
- `estimate_margin` (0.05): added to every estimated need.
- `load_memory_floor_mib` (768): a load is cancelled when a node's available memory falls below this. Waiting requests get `502` "load cancelled: <node> ran low on memory during the load". `0` turns it off.

The agent's own watchdog (`--watchdog-floor-mib`, default 1000) kills a launch whose node falls below its floor.

#### Stable placement among equal consoles

Free memory on an idle console moves by 2 to 25 MiB between reads. `placement_tolerance_mib` (128) keeps an automatic profile's plan from following that noise:

- **Equal consoles.** Consoles whose usable memory differs by less than the tolerance are equal. Among equal consoles that fit, a warm one comes first (the last load that became ready there used the profile's model file, so the file is likely in its page cache), then the one whose name sorts first. A console clearly smaller (by the tolerance or more) is still preferred for a single-console load, as before: the smallest that fits.
- **Held figure.** The figure a console is ordered by moves only when its usable memory has moved by more than twice the tolerance, so readings that jitter by up to the tolerance either way never reorder anything.
- **Even split.** A model split over equal consoles is split evenly (by the class figure) when every console holds its share that way on its exact figure; otherwise it is split by the exact figures, as before. The pre-flight keeps the even split only while its exact figures are within twice the tolerance of the held ones and still hold every share; the launch splits exactly as the plan said.
- **What it never changes.** Fit is always checked on the exact figures: a console that does not fit is never chosen because it sorts first, and every profile's action (start, queue, reject) is what it was. Forced profiles and loads on chosen zones are untouched: their plans are byte-identical to before, and their rows still follow the live figures. The tight-fit rule, the margins and the watchdog floor are unchanged.

`0` turns it off: the smallest budget that fits, then the id, and the split by exact budget.

#### Tightening or loosening the reservation

The memory reservation rule is off by default. It is changed at run time like any setting, and `GET /api/system` shows what is in force under `reserve`:

- `fill_reserve_kib_per_token` (0, recommended 8): every node of a placement keeps this x the profile's context for the growth while the conversation fills the context (180 to 460 MiB per node was measured). A stored peak (the most a node gave up for the profile in its latest load-to-unload cycle that ended normally) can raise that figure, never lower it. One spike holds until the next normal cycle ends; to clear it at once, forget the learned need (`POST /api/forget-need`). A peak above need + fill reserve means the fill reserve is too small for that profile: it is worth raising `fill_reserve_kib_per_token` rather than relying on the peak, since one short normal cycle (a force unload counts as normal) replaces the peak and the profile is back to need + fill reserve.
  - **Every cycle's peak, collected only.** The stored peak keeps only one cycle, so every cycle's peaks also go into its `instanceUnloaded` event (with the reserve on or off): `shape`, `ended_normally`, and `cycle_peaks`, one entry per node of the placement with `node_id`, `node`, `peak_mib` (null with `no_peak` saying why when the node had no free-memory reading in the cycle) and what the plan reserved there when the load started (`share_mib`, `reserved_mib` with the fill reserve, `basis`). `GET /api/needs` sums them up under `peak_cycles`: per shape and node set, the cycles with a peak on every node, their lowest, highest and latest peak (total and per node), the first and last date, and how many rows have at least 1, 3 and 5 cycles. It covers the history store's retention as of the start, plus every cycle since. Nothing reads these figures for placement; they are there to judge whether a profile has enough cycles to say how far its peaks spread.
- `placement_uses_reserved` (off): place on the smaller of the latest and the 30 s average free memory (`free_average_window`), and count what is placed on a node at the larger of its reserved figure and what the node measures (this lowers the figure shown for a held node; held nodes are not placed on). The average is not used for one window after a node's model changes. A budget is never above the one the default rules give. With any reservation setting on, every placement is held to the plan's `memory` rows, so the plan's `fits` is the decision (`decides: true`).
- `margin_measured_mib` (unset): per-node margin for a measured need, in place of `split_skew_margin`. `margin_estimated_percent` with `margin_estimated_min_mib` (unset): the same for an estimate.
- `margin_refuses` (off): with it off, the margin is not a reason to refuse. A placement whose nodes hold what the load will take (weights + context at the full configured window + fill reserve, or the stored peak) with the headroom kept starts even when the margin does not fit on top: a **tight fit**. The plan says `tight_fit: true` and "tight fit: N MiB spare on <node>, margin M MiB not met", each such node in `memory` has `tight: true`, `spare_mib` and `margin_short_mib`, the load logs a WARN line, and its `loadStarted` event carries `tight_fit`. The margin still orders placements: an automatic profile takes a placement that meets it everywhere when there is one, and nothing loaded is stopped only to meet it. A profile short even without the margin is refused ("short by N MiB on <node> even without the margin"). Turn it on to have the margin refuse again, as it did before tight fits.
- `reserved_drift_warn_mib` (500): a node that uses this much more than is reserved on it is flagged in the node view.

To tighten, raise the fill reserve or the margins; to loosen, lower them or set them back to 0. Check a profile before and after with `POST /api/plan`: its `memory` section shows per node what it will take, what the node has and what remains.

```sh
curl -s -X PATCH $H/api/config/settings -H 'X-Comment: reserve on' -d '{"fill_reserve_kib_per_token": 8, "placement_uses_reserved": true, "margin_measured_mib": 300}'
```

**Reading a plan row without taking anything off twice.** On a console, the row's `free_now_mib` already has the watchdog floor taken off, and `available_mib` has the floor and the headroom taken off; do not subtract either again. Each row also says what it is built from: `mem_available_mib` (the console's reported MemAvailable, the current reading), `mem_available_avg_mib` (its average, when known), `mem_available_reading` (`current` or `average`: the one the row is built from), `watchdog_floor_mib`, `headroom_mib` and `placeable_mib` (the same figure as `available_mib`, under a plain name). They add up: reading − floor − headroom = placeable; when another rule changes it (a device that reports less free, a pre-flight reading, memory reserved for a load not measured yet, or a loaded model counted as freed), `placeable_adjust_mib` is that signed term and `placeable_adjust_why` says why. `predicted_lowest_available_mib` is `mem_available_mib` − `will_take_mib`: the figure to compare with the console's own available memory while the load runs. Example: a console reporting 13,656 MiB with a floor of 1,000 and a headroom of 1,024 has `free_now_mib` 12,656 and `placeable_mib` 11,632; a load that will take 6,000 MiB leaves about 7,656 MiB available on it. The Placement preview prints the same line under the table: "available 13,656 MiB − floor 1,000 MiB − headroom 1,024 MiB = placeable 11,632 MiB; after this load about 7,656 MiB left". A node with memory of its own (a discrete GPU) has no such line: its figures are the device's.

### Event history

Loads and how they ended, stalls, low-memory cancels, lost nodes, evictions and unloads are kept in `POTLUCK_ROOT/history` as JSON Lines (rotated at 16 MiB, 20 files kept). The web UI shows them.

```sh
SINCE=$(( ($(date +%s) - 6*3600) * 1000 ))
curl -s "$H/api/history?severity=warn&since=$SINCE" | jq '.events[] | {ts, type, severity, summary}'
curl -s "$H/api/history?session_id=<load session id>&order=asc"
curl -s "$H/api/history/export?since=$SINCE" > history-last6h.jsonl
```

Filter by machine with `node=` (an id or a name, or a comma-separated list; an event naming the machine by either matches) or by zone group with `group=` (its members; 404 for an unknown group, nothing for an empty one). Both together match either.

### Request log

Every inference request through `/v1` and `/upstream` (a POST to one of llama-server's routes that runs the model: chat and text completions, responses, messages, infill, embeddings, rerank, audio), with its status, times, tokens, rates, machines and caller, is kept in `POTLUCK_ROOT/data/usage.db` (rows 90 days, hourly totals 13 months, daily totals for ever: settings `usage_request_days`, `usage_hourly_days`).

```sh
curl -s "$H/api/requests?status=failed,refused&limit=20" | jq '.requests[] | {id, status, reason, profile, queued_at}'
curl -s "$H/api/requests/summary?step=hour" | jq '{totals, live, oldest_ts, history_begins_at}'
curl -s $H/api/system | jq .storage.usage
```

`storage.usage.moved_aside` names a damaged file that was moved aside at the start; `dropped` counts updates lost to a full queue; `size_bytes` is the file and its WAL. A request row takes about 0.5 KB on disk (indexes and hourly and daily totals included), a machine-hour of availability about 0.13 KB; the WAL stays near 4 MiB and is cut back to 8 MiB at most after a checkpoint. On a full disk the writes fail and are counted in `failed`, with `last_error` "database or disk is full" and one WARN a minute at most; requests and the scheduler go on as before, the store stays on and writes again once space is free (the retention trim frees some itself). The file can be deleted while Potluck is stopped: it starts a new history.

Each row has `short_name`, the model file's short name, and `ctx`, the profile's ctx when the request came in, so a run whose profile was deleted later still reads `qwen3.5-4b:8k`. `/api/models` entries carry the same `short_name`, by one rule (`internal/modelname`): directory, `.gguf` and quantisation dropped, a leading vendor word the next word repeats dropped, lower case; then a publisher prefix dropped (one leading word of letters, no digit or hyphen, followed by an underscore: `moonshotai_Kimi-Dev-72B-IQ2_S.gguf` is `kimi-dev-72b`, `THUDM_GLM-4-32B-0414-Q4_K_M.gguf` is `glm-4-32b-0414`), then a trailing `-it` or `-instruct` dropped (`Qwen_Qwen3.5-4B-Q4_K_M.gguf` is `qwen3.5-4b`, `Gemma-4-27B-it-Q4_K_M.gguf` is `gemma-4-27b`). Each of the last two steps is skipped when two model files of the catalog would then share one name: those keep the prefix or the suffix. A row's `short_name` is not stored; it is worked out when the row is read, against the catalog as it is then, so it always matches `/api/models` (a row written before the base model `gemma-4-27b` arrived reads `gemma-4-27b-it` afterwards). The full file name stays in `model_file`. The `ctx` column has its own ledger in `usage.db`, `usage_ext_migrations`, so `usage_migrations` stays at 4 and the previous image keeps its request log on with the file; its rows have no `ctx` and show without one.

The list shows the queued and running requests first, from memory, then the finished ones; the last 64 finished are also kept in memory, so they show at once (before the writer stores them) and when `usage.db` is off.

**Live rows on `/ws`.** Subscribe to the `requests` topic (`{"type":"subscribe","topics":[..., "requests"]}`; not on by default). Frames are `{"type":"requests","data":{"kind": ...}}`: `snapshot` (the first page of `/api/requests`, sent when the client subscribes), `queued`, `started` and `ended` (`request`: the row, as `/api/requests/<id>` answers it) as they happen, and `tick` (`requests`: the running rows) once a second while any request runs. With no subscriber the topic does no work at all.

A running row counts `tokens_out` from the stream (an estimate, marked `live`). Tokens in, reused tokens, the prefill rate and `prefill_processed` (how many prompt tokens are read so far) are known while the request runs only when the request itself asked llama-server for them, with `"return_progress": true` (progress while the prompt is read) or `"timings_per_token": true` (from the first token on); otherwise they come with the end. The setting `requests_live_timings` (off by default) adds `"timings_per_token": true` to streamed chat and completion requests that do not set it, benchmark requests excepted, so every running row has them from its first token. `tokens_in` counts every prompt token, reused ones included, as llama-server's `usage.prompt_tokens` does; `tokens_reused` is its `prompt_tokens_details.cached_tokens` (`timings.cache_n`). `/upstream` rows name the instance and machines that served the model then (no lease is taken).

A client that closes its connection before the answer is complete is `cancelled`, reason `client_left` (`client_left_queued` while it waited); `failed` means the model server or the scheduler failed. A streamed request cut short (by its client, or by the model server dropping the stream) keeps what the stream said so far: `tokens_out` counted from the events (its message says "tokens_out counted from the stream"), `decode_tps` over the time since the first token, and the prompt figures when the request asked for them. A non-streamed request whose client leaves has nothing to count: its tokens are null. Rows written before this fix keep `failed` / `aborted`.

Monitoring calls (`/upstream/<model>/slots`, `/props`, `/health`, `/metrics`, `/models`, `/tokenize`, `/detokenize`, `/apply-template`, `/lora-adapters`, `GET /v1/models`) pass through unrecorded. A row's `nodes` list the host first, then the helpers, each with its `share` of the model's layers; for a model hosted on the scheduler's machine with no layers there, that machine is the host with no `share`.

`source_addr` is the address the TCP connection came from. Behind a published container port that is the Docker bridge gateway (for example `172.17.0.1`) for every caller, not the client's address: the scheduler does not read `X-Forwarded-For` or any other forwarding header. Use the caller name (agent key) to tell callers apart.

### Availability, hour by hour

Every 10 s Potluck notes what each machine is doing, from what it already holds in memory (its status and the agents' membership; no machine is asked), and keeps seconds per state per UTC hour in `POTLUCK_ROOT/data/usage.db`. The Overview strip, the Zones uptime and the 7-day grid read it.

```sh
curl -s "$H/api/nodes/availability" | jq '.nodes[] | {name, uptime, now: .cells[-1].state}'
curl -s "$H/api/nodes/availability?node=zone-a&step=day&from=7d"
```

- `node=` an id or name (default every machine); `from`/`to` unix ms, RFC 3339 or a span back such as `24h` or `7d` (default: 24 hour cells, or 7 day cells, the last one the current); `step=hour|day` (UTC days, summed when read); `group=` a zone group name: only its zones (404 for an unknown group). MCP: `get_availability`.
- Each cell: `seconds` per state (`free`, `in_use`, `loading`, `down`, `off`, `unknown`; they add up to `span_s`), `down_lost_s` and `down_set_aside_s`, `busy` (in flight over the instance's parallel slots, averaged over the in-use time) and `busy_level` 1 to 4, `reachable_s`, `uptime` (reachable over known time) and `state`, the strip colour: unknown when nothing is known, off when switched off half the known time or more, down from 5 minutes down (or half), loading from half, in use from a quarter, else free. Each machine also has its `uptime` over the whole range; the answer has `history_begins_at` and `recording`. With `usage.db` off (see "Request log") nothing is sampled and the answer is an empty list with `recording` false.
- How a machine is classed at each sample, first match wins: **off** removed; **down (lost)** membership state lost or left, even when paused or excluded (a machine the node list calls lost is always down); **off** paused or excluded; **down (lost)** offline in the view; **down (set aside)** quarantined; **loading** placed for an instance that is starting; **in use** placed for a ready, draining or stopping one; **free** otherwise. Time with no sample (Potluck not running, a node not known yet, the rest of a gap after a failed read) is **unknown**, never free.
- A removed machine (membership state removed) is left out of the list for a range it was only off in, which is every range after its removal; `node=` still answers it. Seconds and `uptime` are sent with 3 decimals at most.
- A missing or damaged `usage.db` never stops Potluck: a damaged one is moved aside, the history begins again and the route answers empty cells.

**Uptime over days** (the Zones page's "Uptime, 7 days"): `curl -s "$H/api/nodes/uptime?days=7"` (1 to 90, default 7; MCP `get_uptime`). Per zone (with its `group`), per group, for the zones in no group (`ungrouped`) and for the whole fleet (`fleet`):

- `uptime` = up / counted, where up = free + in use + loading and counted = up + lost + set aside. Switched-off time (paused, excluded, removed) is in neither and is given as `off_s`: the owner chose it. Set-aside time counts as down, as in `/api/health`; it is given as `set_aside_s` apart from `lost_s`. Unknown time (Potluck not running) is in neither. `null` when nothing was counted (for example a zone switched off the whole time).
- A group's figure is its members' seconds summed (weighted by time), not an average of their percentages.
- A removed machine is left out: it is not in `zones`, and its seconds are in no group, not in `ungrouped` and not in `fleet` (as the counts leave it out).
- `window`: `from` (the start of the UTC hour `days` ago) to `to` (now), `history_begins_at`, `covered_from` (the later of the two) and `complete` (false while the history is younger than the window: the figure is then over the history there is, from `covered_from`).
- This is not the `uptime` of `/api/nodes/availability` (reachable over known time, where a paused zone that answers counts up); that field keeps its meaning. `GET /api/groups` carries each group's 7-day figure as `uptime_7d` and the window as `uptime_window`.

### What a zone is doing

`/api/nodes` entries (and `agents` of `/api/status`) carry, when they apply:

- `member`: the instance on the node, its `role` (`host` runs `llama-server`, `helper` is an RPC worker), its `share` of the model (its tensor split weight over the sum: the split the launch was given, from the placement, the profile's `tensor_split` or the node budgets; the shares of an instance add up to 1), `host` (`server` or `node`) and `host_node`. `instances[].nodes[]` of `/api/status` have `role` and `share` too, and each instance `host`, `host_node` and `parallel`.
- `set_aside`: why the node is quarantined: `reason` (`preflight_failed`, `agent_unreachable`, `worker_hung`, `node_lost`), `message`, the load it happened in (`profile`), `since`, `until` and `level`. The same reason is in `quarantines[]` and in `view.nodes[].quarantine_reason` of `/api/status`.
- `paused_at`/`paused_actor` and `excluded_at`/`excluded_actor`: who switched the node off and when, read from the audit log (the newest pause; the newest configuration change that added the matching `exclude_nodes` entry). Absent when the audit log does not have it.
- `free_at`, `free_in_s`, `free_basis` and `next` for a node that is claimed or settling: `settling` (the settle end), `fetching` (now), `stopping` (the victim stops: now plus the node's settle), `draining` (no time: the victim's requests are still running) or `claimed` (now). `view.nodes[]` has `free_at` and `free_basis`.
- `group`: the zone group the node is in (section D, "Zone groups"); absent when in none.
- `zone_state`: the state the zone is counted in: `free`, `in_use`, `loading`, `lost`, `set_aside`, `off` (paused or excluded), or `removed` (not counted).
- `chip`: the GPU name the zone's stats report (`gpus[].name`, distinct names joined with " + "); absent when the agent reports none (a PS5 today). A group's `chip` in `/api/groups` is the common one, `mixed` when two members report different names, absent when a member reports none (a console, a fake agent) and the rest agree: a group of a console and a laptop has no chip.
- `last_load_link`: the zone's **last cold load**, the newest load that really moved the model there: ended ready, bytes measured on the wire (`bytes_measured` true; never the file size of an unmeasured event), at least 256 MiB sent to that zone and at least 2 s long. A warm load (the worker already holds the weights: kilobytes moved), a short or an unmeasured one leaves the previous figure, with its `at` and `model`. Fields: `mb_s` (bytes over the whole load time, which also holds the model's start: a lower bound of the link's rate, hence `kind: "lower_bound"`), `bytes`, `load_s`, `at` (when that load finished), `model`. The page can print "last cold load, <at>, <model>". Absent until such a load. It is not a link test. The network card's own speed is `stats.net[].link_mbps` where the driver gives one (not on a PS5).
- `attention_rank`: the "needs attention first" order: 0 lost or its worker not answering, 1 set aside or kept out for a fault (no tier, unreachable, ...), 2 loading, 3 in use, 4 paused, 5 free, 6 excluded, 7 removed. `GET /api/nodes?sort=attention` lists the zones in that order (then by name, then id); `sort=name` by name; without `sort` the order is as before. MCP `list_zones` takes the same `sort`.

`GET /api/nodes` also answers `counts`: `total` (removed zones left out), `reachable` (total less down; switched-off zones are reachable), `up` (free + in use + loading, that is reachable less off), `down` (lost + set aside), `lost`, `set_aside`, `off`, `in_use`, `free`, `loading`. The Zones row of `/api/health` has the same figures in its `facts`, from the same count, so a page heading and the health popup cannot disagree: "8 of 8 reachable" there is `reachable` of `total`; "7 up · 1 off" is `up` and `off`.

### System health

One judged summary of the System health popup, the same for the UI and the MCP server (`get_system_health`). It answers while the core loop is stalled: the status is asked for with a one-second timeout and the last good one is used when the loop does not answer.

```sh
curl -s "$H/api/health" | jq '{state, issues, sentence, rows: [.rows[] | {id, state, text}], recent: [.recent[].text]}'
```

- `state` (`ok`, `attention`, `problem`, `unknown`) is the worst row; `color` (`green`, `amber`, `red`, `grey`) the heart's dot; `issues` the rows in attention or problem; `sentence` ("2 things need attention", "All good"). Rows always come in this order, each with `state`, `text` (the sentence shown), `count` (the number of issues the row reports, 0 when it is ok or unknown: zones down, runs failed, scheduler issues, storage folders or disks in trouble, requests waiting past the threshold; the row's other figures, such as Storage's `used_percent` or MCP's `keys_active`, are in `facts`), `link` and `facts`:
  - **Zones**: "221 of 232 reachable · 11 down". Down is lost or set aside; paused and excluded zones are off (reachable, not down); removed ones are left out. Any zone down is a problem; no machines is unknown.
  - **Runs**: failed and refused requests of the last hour from the request log, plus benchmark runs that failed (history). Any is attention; five or more making more than half of the hour's requests is a problem. Unknown while the request log (`usage.db`) is off.
  - **Scheduler**: "Running · release <image tag> · up 12 d". `facts.release` is the bare image tag (for example `v36-311213b`); in the ok sentence it is exactly the text between "Running · release " and " · up ", so a page can set it apart without rewording the sentence. With no tag the sentence says "unknown" and `facts.release` is empty; in attention and problem the sentence names the issues, not the release. Problem when a task whose failure ends the process has failed, or the core loop is on one step for 5 s or does not answer. Attention for a background task failure in the last hour, a read panic, stored settings from a newer release, or a step over `loop_step_warn_ms` in the last 10 minutes.
  - **MCP server**: "Running · 3 keys active" (keys not revoked and not expired); attention with "· 1 expires soon" while a key expires within 7 days. The MCP server runs inside the scheduler.
  - **Storage**: the fullest disk among the data, history, child log and models folders, "50.3 of 240 GB used". Attention at 90 % used, under 10 GB free, or a folder whose disk does not answer or cannot be read; problem at 97 % or under 2 GB free. With `models_disk_budget_gb` set, the model files are also judged against it (attention at 90 %, problem over it).
  - **Queue**: "2 waiting · longest 5 min 48 s". Attention when the oldest wait reaches three quarters of `queue_timeout`, problem at nine tenths.
  - **Alerts**: unknown, "Not set up yet" (webhooks are not built).
- `recent`: warnings and errors of the last 2 hours in short sentences, newest first, at most 20, each with `at`, `type`, `severity`, `text`, and `node` and `until` (a set-aside's end) when they apply.
- Live: subscribe to the `/ws` topic `health` (not on by default). It sends the summary when a client subscribes (to that client), and again to every subscriber only when what a person reads changes: the heart (state, colour, issues, sentence), a row's state, text, count or link, or the recent lines. Facts that move by themselves (uptime, loop steps, `checked_at`, free bytes) send nothing; they ride along with the next frame. The uptime at the end of the Scheduler sentence ("up under 1 min", then minutes, hours, days) and the "longest" wait of the Queue sentence are left out of that comparison: they never send a frame by themselves, and are current in `GET /api/health` and in the next frame that is sent. While a client is subscribed the summary is evaluated every 2 s; with none, nothing runs.

**Disk figures.** One reader runs `statfs` on the data, history, child log and models folders every 30 s, off every request path, each read with a 5 s timeout. A read that does not come back (a network share that hangs) marks the folder `timeout` and keeps its last figures; no second read of that folder starts until it returns. `/api/system` `storage.folders` lists each folder: `state` (`ok`, `waiting`, `timeout`, `error`, `unknown` on a platform without `statfs`), `total_bytes`, `free_bytes` (what an unprivileged writer may still use), `used_bytes`, `used_percent`, `potluck_bytes` and `files` (the files directly in the folder), `same_disk_as`, `checked_at`, `late_since`. `GET /api/models` has `disk`: the models folder's figures, `model_bytes`, `budget_bytes` when `models_disk_budget_gb` is set, and `host` (the scheduler's node name: the machine the models folder is on).

**Pinned models.** `PUT /api/models/pin/<key>` pins a model (the catalog key: its file name, or `command:<profile id>`), `DELETE` takes the pin off; both need the permission models and a comment, and are audited in section models. `GET /api/models` entries say `pinned` and `pinned_at`. A pin only changes where the models page lists the model. Stored in `scheduler.db`, table `model_pins`, ledger `model_migrations` (v1): an older image never reads them and shows no pins; they come back with this image.

**Core loop.** `/api/system` `loop`: `busy_now_ms` and `current_event` (the step in progress), the longest step in the last minute, 10 minutes and hour and since the start (`max_last_minute_ms`, `max_10m_ms`, `max_last_hour_ms`, `max_ms`, each with its `_event`), `steps`, `slow_steps` and `warn_ms`. A step longer than `loop_step_warn_ms` (1000) is a `loopSlowStep` history event (warn, at most one a minute). The loop only stores a start time and compares the step's length; readers never ask the loop.

### Child output logs

Each `llama-server` writes its full output to `POTLUCK_ROOT/child-logs/<UTC time>-<profile>-<instance id>.log`. Worker launch logs are copied next to them as `worker-<node name>-<launch id>.log`. These hold the KV cache size, the memory breakdown and the exact out-of-memory error. The oldest files go past `child_log_keep` (200). `log_file` in `/api/status` names the current file.

### Central logging

Potluck, its `llama-server` children, every agent, each launch and each node's journal send log lines to `potluck-logs`. The files above stay; the store is a copy that may drop lines when it is slow.

- **Read**: the UI's "Logs" entry, or `http://<host>:3122/logs/`. Scripts: `curl -s "$H/api/logs/query?query=source:=host+_time:15m&limit=50"`. MCP: `query_logs`. Only reads pass; at most 8 at once.
- **Check**: `curl -s $H/api/status | jq .logs` shows `enabled` and `store`, and warnings.
- **Off for a while**: `curl -s -X PATCH $H/api/config/settings -d '{"logs_enabled":false}'`. Agents stop shipping within one heartbeat; `/logs/` answers 404.
- **Off for good**: remove the `LLM_SCHEDULER_LOGS_STORE_URL` line from the compose file, `docker compose up -d potluck`, then `docker compose stop potluck-logs`.
- **On a node**: `potluck-agent --logs off` ships nothing.
- **Retention and disk cap** are start flags of the store in the compose file (`-retentionPeriod=14d`, `-retention.maxDiskSpaceUsageBytes=20GiB`). Keep the settings `logs_retention_days` and `logs_max_disk_gib` equal to them; `/api/status` warns when they differ. After a change: `docker compose up -d potluck-logs`.
- **Wipe**: `docker compose stop potluck-logs && sudo rm -rf /srv/potluck/logs-store && docker compose up -d potluck-logs`.

### Benchmarks

The scheduler runs benchmarks through its own API and keeps every result in the database. Define benchmarks and suites, run them, read and compare the runs: [benchmark-guide.md](benchmark-guide.md). The web UI has a Benchmarks page.

### What a model server sees

The scheduler forwards a client's request to llama-server with most of its headers. What changes on the way (`internal/proxy/forward.go`, pinned by `TestForward_WhatTheModelServerSeesIsPinned`):

- **Dropped**: the client's credentials (`Authorization`, `Proxy-Authorization`, `X-Api-Key`, `Api-Key`, `X-Goog-Api-Key`, `X-Auth-Token`, `Cookie`, `X-Agent-Key`), Potluck's own `X-Actor`, `X-Comment` and `X-Source` (Potluck still reads them for its audit; only the copy sent on goes without them), the hop-by-hop headers (`Connection`, `Keep-Alive`, `Te`, `Upgrade`, `Proxy-Connection`), and the client's own `Forwarded` and `X-Forwarded-*`.
- **Set by the scheduler**: `X-Forwarded-For` (the address of the connection the request came from: behind a published Docker port that is the bridge gateway, not the client), `X-Forwarded-Host` (the host name the client used) and `X-Forwarded-Proto`; and, for a server hosted on a console, `Authorization: Bearer <launch key>`.
- **Passed on as sent**, including ones a person might take for identifying: `User-Agent`, `Origin`, `Referer`, `X-Real-Ip`, `Via`, `X-Client-Ip`, `True-Client-Ip`, `Cf-Connecting-Ip`, `X-Request-Id`, `Accept-Language`, and any other header the client adds. llama-server does not log request headers; a `command` profile's server might.

### AI agents (MCP)

The MCP server is at `http://<host>:3122/mcp` (Streamable HTTP). Each tool runs the REST request of the same name, with the same audit trail (source `mcp`). Give every agent its own access key (web UI, MCP page, "New key"); the key is shown once, with the settings to paste:

```sh
claude mcp add --transport http potluck http://<host>:3122/mcp --header "Authorization: Bearer ptl_..."
```

Useful tools: `get_status`, `plan_load`, `load_model`, `update_settings` (with `dry_run`), `query_audit`, `rollback_config`, `query_logs`.

### Access keys

Access is open: the web UI and the REST API need no key, and a request without one is the default user with full access. The default user's name is the setting `default_user_name` (`anonymous` unless set; 1 to 40 characters, no control characters; no restart): it signs, in the audit log and the request log, what is done without a key and without an `X-Actor` header. A client that sends `X-Actor` is recorded under that name, as before. An access key is the one restriction there is, so that what an agent does is limited and attributed:

- A request with a key (`Authorization: Bearer ptl_...` or `X-Agent-Key: ptl_...`) gets exactly its key's permissions, on REST and over MCP, and every change it makes is recorded under `agent:<name>` with the key's id. Beyond its permissions it is refused with 403 and a sentence that names the permission (`permission_denied`; `approval_required` for a permission set to Ask me, which is refused until approvals exist; `owner_only` for managing keys). Over MCP the tools a key has Off are not listed, and calling one answers a tool error with the same sentence.
- An unknown, expired or revoked key is refused with 401, never treated as no key.
- A value beginning `ptl_` or `lls_` (after `Bearer `, in any case, or in `X-Agent-Key`) is always taken as a Potluck key, so a wrong one is refused with 401 on every route, the model API included. Anything else in `Authorization` (an OpenAI-style key, `none`, an empty Bearer, Basic) is not a key: the request is keyless, and the value is not logged, stored or passed to the model server.
- Potluck never forwards a client's credential headers (`Authorization`, `X-Agent-Key` and the others listed under "What a model server sees") to a model server; a model server with a launch key gets that key.
- A new key may observe only and expires in 30 days unless set otherwise. Expiring means within 7 days. An expired key can be extended (Manage, new expiry) or rotated; a revoked key is final.
- Keys made before permissions existed (`lls_...`) keep working as before: every permission allowed, no expiry. Setting a permission or an expiry on one in Manage makes it a key like any other; its value does not change.
- Only the SHA-256 of a key is stored. The key appears once, in the answer that made or rotated it; never in the list, the audit log, an event or a log line.
- Expiry warnings: history events `agentKeyExpiring` once at 7 days and once at 1 day left, `agentKeyExpired` once when it has expired (checked hourly; a restart does not repeat them; a new expiry starts them over).
- Usage per key: each key's requests per UTC day, and of them the ones refused for want of a permission (`denied`, REST and MCP, owner-only routes included) and the ones refused because the permission is Ask me (`asked`). `GET /api/agents` gives each key `usage`: `days` (7), the sums `requests`, `denied`, `asked`, and `by_day`, the 7 UTC days with today last (`"day": "2026-11-17"`). The days are kept 90 days (`agent_usage` in `scheduler.db`, version 2 of `access_migrations`). Counts are written at most once a second per key, and on shutdown; the list adds the ones not written yet.
- The MCP URL in the settings to copy is built from the address the browser used (the `Host` header); no forwarding header is read.

What each permission covers (the table in `internal/api/scopes.go`; a route missing there needs observe for GET and settings otherwise):

<!-- access-table:begin (written by TestRouteScopes_DocsTable -update-docs) -->
| Permission | Levels | Routes |
|---|---|---|
| Observe (`observe`) | allow, off | `GET /api/about`, `GET /api/audit`, `GET /api/audit/{id}`, `GET /api/bench/aggregates`, `GET /api/bench/benchmarks`, `GET /api/bench/benchmarks/{id}`, `GET /api/bench/benchmarks/{id}/results`, `GET /api/bench/external`, `GET /api/bench/external/{id}`, `GET /api/bench/groups`, `GET /api/bench/groups/{id}`, `GET /api/bench/groups/{id}/expand`, `GET /api/bench/measurements`, `GET /api/bench/metrics`, `GET /api/bench/profiles`, `GET /api/bench/removals`, `GET /api/bench/removals/preview`, `GET /api/bench/runs`, `GET /api/bench/runs/{id}`, `GET /api/bench/runs/{id}/export`, `GET /api/bench/runs/{id}/measurements`, `GET /api/bench/runs/{id}/report`, `GET /api/bench/runs/{id}/scores`, `GET /api/bench/runs/{id}/steps/{step}/transcript`, `GET /api/bench/runs/{id}/tasks`, `GET /api/bench/runs/{id}/tasks/{result}`, `GET /api/bench/runs/{id}/transcripts`, `GET /api/bench/schema`, `GET /api/bench/state`, `GET /api/bench/suites`, `GET /api/bench/suites/{id}`, `GET /api/bench/suites/{id}/expand`, `GET /api/bench/task-sets`, `GET /api/bench/task-sets/{id}`, `GET /api/bench/trends`, `GET /api/config`, `GET /api/config/exclude-nodes`, `GET /api/config/export`, `GET /api/config/fleet`, `GET /api/config/no-auto-nodes`, `GET /api/config/node-overrides`, `GET /api/config/profiles`, `GET /api/config/profiles/{id...}`, `GET /api/config/recovery`, `GET /api/config/revisions`, `GET /api/config/revisions/{n}`, `GET /api/config/schema`, `GET /api/config/settings`, `GET /api/config/tiers`, `GET /api/events`, `GET /api/groups`, `GET /api/health`, `GET /api/history`, `GET /api/history/export`, `GET /api/history/summary`, `GET /api/logs/query`, `GET /api/mcp/tools`, `GET /api/metrics/activity`, `GET /api/models`, `GET /api/models/trash`, `GET /api/models/{key...}`, `GET /api/needs`, `GET /api/nodes`, `GET /api/nodes/availability`, `GET /api/nodes/uptime`, `GET /api/nodes/{id}/startups`, `POST /api/plan`, `GET /api/profiles`, `GET /api/requests`, `GET /api/requests/summary`, `GET /api/requests/{id}`, `GET /api/startups`, `GET /api/status`, `GET /api/system`, `GET /api/zones/pending`, `GET /api/zones/pending/{id}`, `/logs/`, `GET /running`, `GET /ws` |
| Switch zones on or off (`zones`) | allow, ask, off | `PUT /api/config/exclude-nodes`, `PUT /api/config/no-auto-nodes`, `POST /api/groups`, `DELETE /api/groups/{name}`, `PATCH /api/groups/{name}`, `DELETE /api/groups/{name}/members`, `POST /api/groups/{name}/members`, `POST /api/groups/{name}/pause`, `POST /api/groups/{name}/unpause`, `POST /api/nodes/{id}/expect-restart`, `POST /api/nodes/{id}/pause`, `POST /api/nodes/{id}/readmit`, `POST /api/nodes/{id}/unpause`, `POST /api/zone-actions/pause`, `POST /api/zone-actions/unpause`, `POST /api/zones/pending`, `DELETE /api/zones/pending/{id}` |
| Load and unload models (`models`) | allow, ask, off | `POST /api/bench/benchmarks/{id}/run`, `POST /api/bench/groups/{id}/run`, `POST /api/bench/runs/{id}/cancel`, `POST /api/bench/suites/{id}/run`, `POST /api/cache/evict`, `POST /api/load`, `POST /api/models/delete/{key...}`, `POST /api/models/drain/{model...}`, `DELETE /api/models/pin/{key...}`, `PUT /api/models/pin/{key...}`, `POST /api/models/trash/{id}/restore`, `POST /api/models/undrain/{model...}`, `POST /api/models/unload`, `POST /api/models/unload/{model...}`, `POST /api/nodes/{id}/force-unload`, `POST /api/unload`, `POST /api/zone-actions/drain`, `POST /api/zone-actions/force-unload`, `GET /unload`, `POST /unload` |
| Change settings (`settings`) | allow, ask, off | `POST /api/bench/benchmarks`, `DELETE /api/bench/benchmarks/{id}`, `PUT /api/bench/benchmarks/{id}`, `POST /api/bench/external`, `DELETE /api/bench/external/{id}`, `PUT /api/bench/external/{id}`, `POST /api/bench/groups`, `DELETE /api/bench/groups/{id}`, `PUT /api/bench/groups/{id}`, `DELETE /api/bench/runs/{id}/pin`, `PUT /api/bench/runs/{id}/pin`, `DELETE /api/bench/runs/{id}/scores/{scorer}`, `PUT /api/bench/runs/{id}/scores/{scorer}`, `POST /api/bench/suites`, `DELETE /api/bench/suites/{id}`, `PUT /api/bench/suites/{id}`, `POST /api/bench/task-sets`, `DELETE /api/bench/task-sets/{id}`, `PATCH /api/config/fleet`, `PUT /api/config/fleet`, `POST /api/config/import`, `PUT /api/config/node-overrides`, `POST /api/config/profiles`, `DELETE /api/config/profiles/{id...}`, `PUT /api/config/profiles/{id...}`, `PATCH /api/config/recovery`, `PUT /api/config/recovery`, `POST /api/config/revisions/{n}/rollback`, `PATCH /api/config/settings`, `PUT /api/config/tiers`, `POST /api/forget-need`, `DELETE /api/models/{key...}`, `PUT /api/models/{key...}`, `POST /api/needs/forget`, `DELETE /api/nodes/{id}` |
| Manage access keys (`owner`) | no key may (the default user only) | `GET /api/agents`, `POST /api/agents`, `DELETE /api/agents/{id}`, `PATCH /api/agents/{id}`, `POST /api/agents/{id}/rotate` |
| Console agents' protocol (`agents`) | not checked | `POST /api/logs`, `GET /api/logs/config`, `POST /api/pool/nodes/{id}/deregister`, `POST /api/pool/nodes/{id}/heartbeat`, `GET /api/pool/nodes/{id}/logs`, `POST /api/pool/nodes/{id}/logs`, `GET /api/pool/programs/{release}/{platform}/{file}`, `POST /api/pool/register`, `GET /api/pool/release`, `GET /install.sh`, `GET /install/potluck-agent-setup` |
| Model API (a key there is checked for validity only) (`model_api`) | not checked | `/upstream/{model}`, `/upstream/{model}/{rest...}`, `/v1/`, `GET /v1/models` |
| Always open (`open`) | not checked | `/`, `GET /health`, `GET /ui`, `GET /ui/`, `GET /ui/{$}`, `GET /{$}` |
<!-- access-table:end -->

### Phase out a model alias

Profiles can have `aliases`. Each use is logged at most once per alias and caller per minute:

```sh
docker logs potluck 2>&1 | grep 'deprecated model alias used' | tail -50
```

When an alias has had no hits for long enough, remove it from the profile.

### Tune hold and timeouts

All are settings; change them with `PATCH /api/config/settings` and watch.

| Symptom | Setting | Direction |
|---|---|---|
| Alternating models spend all their time loading | `min_hold`, `max_hold` | raise |
| Requests for another model wait too long behind a protected model | `max_hold` | lower |
| A busy model is never evicted for a waiting request | `drain_after` | lower |
| `503` after waiting too long | `queue_timeout` | raise |
| A stream is cut mid-answer | `stall_timeout` | raise (`0` = off) |
| Idle models stay loaded too long | `ttl` | lower |
| A hung load is given up too late, or a slow upload is cut | `load_stall_timeout` (also per profile) | lower or raise |
| Cold loads time out | profile `load_timeout`, tier `link_mb_s` | raise or correct |
| Workers of slow nodes do not start in time | `worker_start_timeout` | raise |

---

## J. Troubleshooting

First look: `curl -s $H/api/status` and `docker logs --tail 100 potluck`. For one profile, `curl -s -X POST $H/api/plan -d '{"model":"NAME"}'` says what would happen now and changes nothing.

| Symptom | Cause | Fix |
|---|---|---|
| `503` "cluster busy", `Retry-After: 30` | The request waited `queue_timeout` and no node could be freed: every usable node is held, claimed or settling, and held models are still protected. | Retry. Look at `queue` in `/api/status`. Free a node: `POST /api/models/unload/{model}` (idle only). Tune hold (I). |
| `503` "does not fit", with need and capacity | The need is larger than the best `max_pool` nodes could hold even if empty. | Is `needs_mib` right (G)? Is a node offline or unplaced? Compare the need with the node figures in `/api/status`. |
| `503` "forced nodes unavailable" after about 30 s | A node in `force_nodes` has been absent from the pool for `forced_node_grace`. | Bring the node back or fix the name in `force_nodes`. |
| `503` "forced nodes do not fit" | The forced nodes' usable memory is below the need. "even with nothing loaded on it" means it can never fit. Nothing was started. | Force more or other nodes, or measure the need (G). |
| `502` with `would_not_fit` | The agent refused the launch: the node's share of the need does not fit its memory. | Free memory on the node, or measure the need (G). |
| `502` "load cancelled: <node> ran low on memory during the load" | Available memory fell below `load_memory_floor_mib` during the load. The profile is backed off. | Find what uses the node's memory. Retry after the backoff. |
| `502` with `watchdog` or `oom_kill` | The worker ran out of memory. The learned need is raised. | Retry: the next placement uses the larger need. |
| `502` "load stalled: no bytes acknowledged by the workers" | A worker stopped taking bytes for `load_stall_timeout`. The profile is backed off. | Check the worker's node and its launch log in `child-logs`. Retry after the backoff. |
| `502` "load timeout" | The load took longer than `120 s + 2 x (each worker's share of the file / link_mb_s)`. The profile is backed off 60 s, doubling to 10 min. | Set `load_timeout` on the profile above the measured load time, or correct the tier's `link_mb_s`. |
| `502` `worker_start_timeout` | A worker launch was not ready within `worker_start_timeout`. | Look at the node's launch log; raise the setting for slow nodes. |
| `502` "force unload of node <id>" | Someone force-unloaded a node the model used. | Expected. |
| A node shows `unplaced` | See "Why a node gets no load" (D). | |
| Agents refused with `release_unavailable` | The scheduler serves no verified release. | `curl -s $H/api/system | jq .programs` names the problem (E). |
| Every load refused, `/api/system` shows `host_build_error` | The scheduler's `llama-server-rpc` is not the release's llama build. | Build the base image from the release's commit (B). |
| A model lands on the host GPU unexpectedly | The host tier is used only when better tiers cannot hold the model, or their nodes are not placeable. | `POST /api/plan` says why. Restrict the profile with `allow_tiers`. |
| `potluck-share` does not start | The `nfs` and `nfsd` kernel modules are not loaded, or `POTLUCK_SHARE_BIND` is not an address of the host. | `lsmod | grep -w nfsd`; check `.env`. |
| About shows "A background task failed in the last hour" | An internal error (a bug) in one of the scheduler's background tasks. It was handled: the task restarted alone, or the work it was doing failed with an error. | See "An internal failure" below. |
| The container restarted with exit code 3 | A failure Potluck must not continue from: the scheduler loop, the lost-node check or a configuration apply failed, or one task failed three times in a minute. | See "An internal failure" below. |

### An internal failure

Potluck isolates its background tasks (architecture, "What restarts when"). What you see:

- The log line `background task failed` at error level, with `task`, `panic`, `failures_last_min`, `restarts` and the Go `stack`.
- An `internalFailure` event on the Activity page (filter "Scheduler"), with the task and the message.
- The About page's "Background tasks" card: each task with what it does after a failure, its restarts, failures in the last minute and last failure. The same is in `curl -s $H/api/system | jq '.tasks, .internal_failures_total'`.

What to do:

1. Nothing is needed for the service: a task that restarts alone is running again (state `running` in the card). A benchmark run that was executing shows failed with "internal error"; start it again. A configuration change that answered 500 "the change is saved, but applying it ... failed" is saved, and the restart that follows applies it.
2. Keep the evidence: `docker logs potluck 2>&1 | grep -A40 'background task failed'` and the `internalFailure` events on the Activity page. Report it with the release (`version` in `/api/system`); it is a bug.
3. If Potluck stopped (the `schedulerStopping` event says "internal failure in ...", and `docker inspect -f '{{.State.ExitCode}}' potluck` shows 3 until the restart), Docker starts it again. Loaded models ended, as with any scheduler restart (L). If it stops again and again, roll back to the previous release (F).

---

## K. Move an existing deployment to the standing layout

The standing layout is `deploy/docker-compose.yml` as it is now: project `potluck`, containers `potluck`, `potluck-share` and `potluck-logs`, every host folder under `POTLUCK_ROOT`. These steps move an older deployment (project `llm-scheduler`, container `llama-swap`, folders next to the compose file in `/opt/llm-scheduler/`) with one short outage, from step 5 to step 7.

The old project must be down first: both use the subnet `172.30.0.0/24`, and two Docker networks cannot hold the same subnet. On that network the scheduler has the fixed address `172.30.0.2` and the optional host agent `172.30.0.10`; the share and the log store take addresses from `172.30.0.128/25`, so they never take a fixed one. Docker sets that range only when it creates the network. Check: `docker network inspect potluck_sched --format '{{(index .IPAM.Config 0).IPRange}}'` prints `172.30.0.128/25`; if not, run `docker compose down` and `docker compose up -d` once.

1. Write down what the old deployment uses:

```sh
docker inspect llama-swap --format '{{range .Mounts}}{{.Source}} -> {{.Destination}}{{"\n"}}{{end}}'
curl -s $H/api/config | jq '.revision, .settings.membership'
```

2. Put the new compose file, the seed and `share-nfs.conf` in `/opt/potluck/`, and copy the old `.env` there (`POTLUCK_SHARE_BIND`, `POTLUCK_SHARE_NET`). The seed is not imported again. Check: `cd /opt/potluck && docker compose config -q`.

3. Create the root folder, owned like the old data folder:

```sh
sudo mkdir -p /srv/potluck && sudo chown "$(stat -c '%u:%g' /opt/llm-scheduler/data)" /srv/potluck
```

4. Optional, while everything still runs: set the host's node name in its own setting, `curl -s -X PATCH $H/api/config/settings -d '{"local_node":"host-1"}'`. While it is empty, `fleet.local_node` is used.

5. Outage starts. Stop the old project: `cd /opt/llm-scheduler && docker compose down`. Then `docker network ls | grep -c sched` prints `0`. Nodes keep running; their share mounts wait.

6. Move the data, each folder whole (never `scheduler.db` without its `-wal` file):

```sh
sudo mv /opt/llm-scheduler/data       /srv/potluck/data
sudo mv /opt/llm-scheduler/history    /srv/potluck/history
sudo mv /opt/llm-scheduler/logs       /srv/potluck/child-logs
sudo mv /opt/llm-scheduler/logs-store /srv/potluck/logs-store
sudo mv <programs folder from step 1> /srv/potluck/programs
```

7. Start the new project: `cd /opt/potluck && docker compose up -d`. Outage ends.

8. Check: the checks of section C, the same revision as in step 1 (one more after step 4), and `curl -s $H/api/nodes | jq '[.nodes[] | {name, state}]'`. Agents register again within a minute.

9. Keep `/opt/llm-scheduler/` until you are sure. The way back: `docker compose down` in `/opt/potluck`, move the five folders back, `docker compose up -d` in `/opt/llm-scheduler`. If you did step 4 and go back to an image older than the `local_node` setting, set it to `null` first.

---

## L. What comes back after a restart of the host

Nothing has to be started by hand when these hold:

1. Docker starts at boot (`systemctl is-enabled docker`).
2. The three services are `restart: unless-stopped`. A service stopped by hand stays stopped; start it with `docker compose up -d`. The optional host agent comes back too once it was started with `--profile local-agent`.
3. The share needs the `nfs` and `nfsd` modules at boot.
4. The agents keep retrying and register again. A machine that has not registered 65 s after the start is listed as `lost` with its last seen (section D, "A machine that is off across a restart"). Their share mounts resume when `potluck-share` is up; a share restart costs clients about 16 s. Check the NFS times: `docker exec potluck-share cat /proc/fs/nfsd/nfsv4gracetime /proc/fs/nfsd/nfsv4leasetime` prints `10` twice.
5. No model is loaded at start. Launches left on the nodes are stopped by their agents.
6. Learned memory needs come back from the database. The queue, backoffs and quarantines start empty.

Check after a restart:

```sh
docker compose -f /opt/potluck/docker-compose.yml ps
curl -fsS $H/health
curl -s $H/api/nodes | jq '[.nodes[] | {name, state}]'
```

---

## Legacy: fleet membership

With the setting `membership: fleet` the scheduler reads its nodes from an external fleet console (`fleet.url`) instead of agent registrations, pre-flights each worker with `llama-server --list-devices`, and can send recovery requests for a hung worker (the `recovery` settings). `deploy/scripts/preflight-host.sh` and `deploy/scripts/hardware-checks.md` were written for that mode. It stays until a later release removes it; new deployments use agents.
