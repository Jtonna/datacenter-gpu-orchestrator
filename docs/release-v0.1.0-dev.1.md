# Potluck v0.1.0-dev.1

Potluck is a scheduler that places LLM models across a pool of machines: it decides which machines load a model, starts and stops the model server there, and queues requests until a model is ready. You use it through a web UI, a REST API and MCP tools for AI agents. This is the first dev release: it is meant for the owner's own cluster and for early users, it is not stable, and anything in it may still change.

It contains every build from build-74 to build-92 (the builds after build-72). The tag is on the commit of build-92.

## What you can do

### Models
- See every model file, what is loaded and where, and what each needs. A file under 1 GB is shown in MB.
- See a need that was written by hand, and why no configuration of a model can be placed, on the line under its name.
- See a test run's load apart from ordinary use ("test run e2 on <zone>").
- Read a model's conversation memory (restore points and prompt cache) as one line in its panel, with each profile's restore points in a column of its configurations. For Gemma 4 the size of one restore point is shown as a proven upper bound.
- Pin a model (with a comment). A pin changes nothing in where or when it loads.

### Profiles and the standard set
- See every profile with its shown name. A test profile is named "TEST - <name>"; a production profile has its plain name or a display name.
- Find test profiles on their own page (Settings > Test profiles): the model, the context it really runs at, why it is a test profile, which benchmarks refer to it, and when it was last used.
- Promote a test profile to production, or demote a production profile to test, with a comment. Both ask twice: nothing is written before the second press.
- Open a test profile's configuration by clicking its row: the same panel a production profile has, with Promote, Edit, Duplicate, Delete.
- Compare a profile with any other ("Compare with…"): the differences first, extra arguments flag by flag, and what each would need now. It reads only.
- Make a model's standard profiles: the page first shows which contexts would fit on an idle pool, where, and what each needs, then writes only the ticked ones that do not exist yet, with a comment. Existing profiles are never edited.
- Give a profile its own limits for conversation memory (`--ctx-checkpoints` and `--cache-ram` in its extra arguments). They are passed at launch instead of the cluster settings.
- Run a production profile with a benchmark's settings under its own name (`<profile>@<scope>`), beside production traffic, without writing a new profile.

### Zones and tiers
- See every zone, its state, memory and history, open its panel, and pause, unpause, unload or force unload with a comment.
- Load a model on chosen zones after reading its plan.
- Edit tiers, excluded zones and named-only zones in one save with a dry run first and a comment.

### Placement and memory
- Ask where a profile would load now, and why, on any zones (Settings > Placement preview). Nothing is loaded.
- See the profile's need beside a refusal, so a refused profile still says what it needs.
- See the hosting machine's conversation memory: restore points and prompt cache, where the figures come from, and whether they are counted.
- Have a profile's own conversation-memory limits counted for that profile against the machine that hosts it. On a zone it joins that zone's reservation; on the scheduler's own machine it must fit in the memory left there, or idle models are unloaded first, or the load waits. A profile that can never fit there is refused, and the plan names the figures and the flags that would make it fit. With the setting `count_host_memory` on, every profile is counted.
- See a profile planned at its model's trained context when its written context is higher, and the context the model server really runs at after a load.

### Benchmarks
- Browse groups, benchmarks, runs and reports on the Benchmarks pages: Overview, Benchmarks, Runs, Settings.
- Read the latest result for each model (load time, reload, generation speed, first token, memory, the fewest zones a load completed on) and the result views of the Runs page.
- Start a run (a benchmark, a group, or "Run again") from the page, the API or MCP. The run dialog first reads what would happen: the loads, the machines, what each takes, whether it fits now, what is loaded and what would be unloaded. A run starts only after a comment and a second confirm.
- Wait until the pool is idle, or let a run unload other people's idle models: the dialog names them, and the run is refused if that list changed.
- See a queued run's place, a running run's step and progress, and cancel a run with a comment. A cancelled run says where it stopped and who cancelled it.
- Create and edit benchmarks and groups in editors with a save bar. A refused save names the parts it is about.
- See which benchmarks a run used have changed or gone since it ran.
- Keep a group from being started from Potluck (setting `bench_not_startable`); its row then says "Not startable here" with the reason.
- Fill a model's context in measured steps (the `context_fill` phase) to collect memory growth per token.

### Deleted models and timed clearing
- Delete a model to the trash (dry run first, name typed, comment); restore it with its profiles exactly as they were stored.
- Remove a deleted model for good: dry run, the file name typed exactly, a comment. This cannot be undone.
- Turn on timed clearing (`model_trash_purge`): a file is removed after `model_trash_days` (default 30). Each deleted model says "Kept until <date>".
- See what was removed for good, when, by whom and why.
- When a restored profile's name was taken meanwhile, keep the taken ones back and add them later.

### Audit and comments
- Every write from the page asks a comment and is kept in the audit log with who made it.
- Every save or delete of a benchmark or group, every run start and every cancel needs a comment, from the page, the API and MCP alike.
- A run keeps who started it and why.

### MCP tools
- Run, cancel and dry-read benchmark runs (`run_benchmark`, `run_bench_group`, `run_suite`, `cancel_run`; each needs a comment; runs take `evict` and `dry_run`).
- Create, update and delete benchmarks, suites and groups (each needs a comment); group-named tools `list_bench_groups`, `get_bench_group`, `create_bench_group`, `update_bench_group`, `delete_bench_group`, `run_bench_group`.
- List runs by page and result; read a run by its number.
- List deleted models and remove one for good (`remove_deleted_model`).

### Health and logs
- Read the scheduler's own memory in one call (`GET /api/system` `runtime`).
- See the health popup from the rail; it no longer scrolls the page behind it.
- Pages ask the server for less: the live channel reads the status every 2 s while nothing moves, at once on a real change, and one page open costs at most one round of plans.

## Changes that can surprise you

- **Comments are required** for every save or delete of a benchmark, suite or group, every run start and every run cancel (body `comment` or the `X-Comment` header). Scripts that write without one get 400.
- **Pinning a benchmark run needs the permission `models`**, not `settings`.
- **A run start can be refused whole** before anything is queued: a listed group, an excluded zone, the scheduler's own machine as a target, a deleted model file, or 20 runs already queued.
- **`@` is refused in profile ids and aliases.** It is kept for run-scoped names. A configuration that holds one does not load: check before upgrading.
- **A context above the model's trained context is refused on a write** unless the profile says `ctx_above_trained: true`. Such a stored profile is planned and run at the trained context.
- **`--ctx-checkpoints` and `--cache-ram` in a profile's extra arguments are accepted and passed**, and that profile's conversation memory is counted against its hosting machine. The `--flag=value` form is refused.
- **Learned memory needs are keyed without those two flags.** A need stored under a key that carries one of them is skipped at start, with one warning; it is not deleted.
- **Every launch passes `--cache-ram` and `--ctx-checkpoints`** (default 32, the model server's own default). The worker agent starts model servers without any `LLAMA_ARG_*` variable.
- **Automatic placement's order among equal machines** (with `placement_tolerance_mib` above 0): warm first, then machines with stored measurements of the shape, then by name. Fit and every action are decided as before.
- **The KV part of an estimated need** leaves out an MTP block at the end of the file. Some estimates get smaller.
- **Removing a model for good erases the file.** It needs the models folder mounted read-write; on a read-only folder every removal is refused.
- **A restore never leaves a file in the trash because of a profile**: profiles come back as stored, with warnings instead of refusals.
- **The gear opens Settings** (the cluster's own settings), then Profiles, Test profiles, Tiers and zones, Placement preview, Audit, About. Old addresses still open their pages. (build-92)
- **The install script's rules.** Besides the scheduler's own machine, it refuses only a machine set up by the Potluck starter (it has `/etc/potluck/starter.env`) and a machine whose root file system is in memory. It no longer looks at host names, unit names or the kind of graphics chip, so machines it refused before may now install.
- **`--not-a-console` is accepted and ignored** in this release, and will be removed.
- **The default node source is `agents`** for a configuration that does not set `membership`. A stored `fleet` stays `fleet`. An install without the key that used the fleet feed starts in agents mode: set `membership: fleet` before upgrading.
- **The Docker build needs a base image.** `deploy/Dockerfile` and the rpc build files take `BASE_IMAGE` as a required build argument; a build without it stops with one sentence saying what to pass. The example compose file takes the models folder from `POTLUCK_MODELS_DIR`.
- **No word in the product names one kind of machine.** The pages, the API's sentences and the MCP tool descriptions say "zone" or "machine". A check in the tests and in the pre-commit hook keeps it so.
- **Text is lighter.** One rule sets font weights for the whole product: body text regular, one strong cell per row, figures and ids in the mono face at regular weight.

## Not in this release

Drawn greyed on the pages, with the reason on hover:
- Coding figures and the Coding view (needs task scores per model and a default scorer: benchmarks slice 4).
- Run and Run again for a recipe that asks for its inputs (that dialog is not designed yet).
- "Smallest that fits" placement and the "cold loads" option for a group (benchmarks slice 7).
- Compare two runs (needs a server route first; benchmarks slice 5).
- What stopped a run, such as a machine dropping out (benchmarks slice 5).
- One-click PDF of a report (benchmarks slice 5); open the full report and print it.
- Add a model from the page (put the .gguf file in the models folder instead).
- Restart a machine from the page (no server route).
- SSH from the pages, and installing a zone over SSH (left out on purpose).
- A Windows installer and a USB drive image for the agent.
- Ask Potluck (the assistant drawer is a placeholder).
- Approvals for MCP keys set to "Ask me": such requests are refused until approvals are built.

Planned after this release: run-state polish, a Benchmarks settings page, Compare two runs, phone-width touches on the new panels, and the rest of the generic wording in tests and docs.

## Builds in this release

- **build-74**: Remove a deleted model for good, and timed clearing of deleted models.
- **build-75**: Context fill reads fresh memory samples and waits for the model to settle.
- **build-76**: Run-scoped profiles, and the TEST / PRODUCTION marking (read only).
- **build-77**: A test run's instance counts only for itself.
- **build-78**: The live model list computes plans when something changes, not on every heartbeat.
- **build-79**: Profiles are planned and run at the model's real (trained) context.
- **build-80**: The context fill's stop check uses the measured memory growth.
- **build-81**: Host memory made explicit (restore points and prompt cache); the agent starts model servers without `LLAMA_ARG_*`.
- **build-82**: Gemma 4's restore-point size shown as a proven bound.
- **build-83**: The new Benchmarks pages, run numbers and run facts, "Remove for good" in the trash, and an estimate without the MTP block's KV.
- **build-84**: Benchmarks pages, second round; a calmer status loop; small files in MB.
- **build-85**: One round of plans per page open; heartbeats no longer wake the status loop.
- **build-86**: The Test profiles page, "TEST - …" shown names, Promote / Demote, and Make standard profiles.
- **build-87**: A restore keeps stored profiles.
- **build-88**: Request rows name their profile; the Needs cell holds the figure alone; the mouse wheel stays over overlays; the scheduler's memory in one read.
- **build-89**: Run and Cancel from the Benchmarks pages; benchmark and group editors; a comment on every benchmark write.
- **build-90**: A profile's own `--ctx-checkpoints` and `--cache-ram`, counted for that profile.
- **build-91**: Equal machines with stored figures first; the release gate programs; fake workers with a platform each.
- **build-92**: Fixes from the owner's walk-through (the gear opens Settings; Recent runs first on the Benchmarks Overview; one rule for font weights; the model's side panel toned down; Test profiles with a plain Promote that asks twice, the profile's panel and Compare with…; cancelled runs marked grey everywhere) and a generic product (the install script's rules, the default node source, the Docker base image argument, no machine-kind words).
