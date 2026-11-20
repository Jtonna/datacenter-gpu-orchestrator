# Potluck agent deployment files

These files install, start and remove `potluck-agent` on a node. The rules they follow are in `docs/agent-contract.md` (sections 5.4, 9 and 10).

**`potluck-starter.sh`** is for RAM-only nodes (PS5). It is shipped unchanged in the datacenter payload, together with the Potluck address (argument, `POTLUCK_ADDR`, or a `potluck.addr` file next to the script). It needs only a POSIX shell, curl and sha256sum. It does nothing when `/etc/potluck/disabled` exists or when `potluck-agent.service` is active, activating, reloading or deactivating. Otherwise it downloads the agent from Potluck into `/dev/shm/potluck`, checks the sha256 and starts the transient unit `potluck-agent.service` with `--ram-only`. It retries forever (5 s growing to 60 s). Extra agent options go in `POTLUCK_AGENT_ARGS` (split into words).

**`potluck-agent-setup`** is for machines with a drive. Run as root with exactly one of `--bare` or `--docker`, plus `--potluck` and `--model-source`. `--bare` installs `/usr/local/bin/potluck-agent` and a systemd unit. `--docker` mounts the share on the machine (systemd mount unit, read-only, at `/var/lib/potluck-agent/models`) when the source is `share:`, gets the image (given `--image`, a pull, or a local build from the agent binary and the Dockerfile embedded in the script) and starts the `potluck-agent` container with `--init`, host networking and, when there is an Nvidia GPU, `--gpus all`. Installing again removes the uninstall marker.

**`potluck-agent-uninstall`** is run as root on the node. It deregisters from Potluck (best effort), writes the marker on nodes that have a starter (or with `--marker`), stops launches, exporters and the agent, removes the unit or container and image, the share mount, the program folder, state and the binary. It exits non-zero if something important is still running.

**`Dockerfile`** builds the node image `nvidia/cuda:13.3.1-runtime-ubuntu24.04` plus `libgomp1` plus the agent binary. Programs are not baked in. Build context must contain the agent binary. Keep it in sync with the copy inside `potluck-agent-setup`.

## The three install paths

1. RAM-only (PS5): starter, started by the payload at every boot or apply.
2. Bare metal with a drive: `potluck-agent-setup --bare ...`.
3. Docker (Nvidia laptop): `potluck-agent-setup --docker ...`.

## To try on a real machine

Nothing here has been executed. Only `sh -n` was run.

- Starter on a PS5: the JSON parsing against a real `/api/agents/release` answer, `systemd-run` with the transient unit, running it twice, with the marker, and while the unit is restarting.
- Setup `--bare` on a Linux box: the unit starts, the agent registers, re-running replaces the unit and keeps a running launch.
- Setup `--docker` with `share:`: the mount unit name and options, `clientaddr`, the container sees `/models`, the local-build fallback, `--gpus` handling.
- Uninstall on each path: the `Transient` detection, the launch unit listing, the umount of `/run/potluck/models`, a second run being harmless.
- Port checks via `ss -ltn` on a machine whose agent is already running.
