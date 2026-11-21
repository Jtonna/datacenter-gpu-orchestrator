# Potluck agent deployment files

These files install, start and remove `potluck-agent` on a node. The rules they follow are in `docs/agent-contract.md` (sections 5.4, 9 and 10).

**`potluck-starter.sh`** is for RAM-only nodes (PS5). It takes no arguments. It reads `POTLUCK_URL` (for example `http://192.0.2.1:3122`) and the optional `POTLUCK_MODEL_SOURCE` (default `share:<host of POTLUCK_URL>:/`) and `POTLUCK_WATCHDOG_FLOOR_MIB` (default 1000) from its environment, else from `/etc/potluck/starter.env` (`NAME=value` lines). It runs as root and needs only a POSIX shell, curl, sha256sum, sed and coreutils; the release answer is read with sed (layout rule on `agentproto.ReleaseInfo`). It exits 0 when `/etc/potluck/disabled` exists, when `potluck-agent.service` is active (or activating, reloading, deactivating), or after starting it. Otherwise it downloads the agent into `/dev/shm/potluck`, checks the sha256 and starts the system-level transient unit `potluck-agent.service` (Restart=always, RestartSec=5) with `--ram-only --potluck --model-source --watchdog-floor-mib`. While Potluck is unreachable, the settings are missing or a checksum fails it waits 5 s growing to 60 s and never exits. It exits 1 only for a local fault: curl, sha256sum or systemd-run missing, unsupported architecture, `/dev/shm/potluck` not writable, systemd-run failed.

**`potluck-agent-setup`** is for machines with a drive. Run as root with exactly one of `--bare` or `--docker`, plus `--potluck` and `--model-source`. `--bare` installs `/usr/local/bin/potluck-agent` and a systemd unit. `--docker` mounts the share on the machine (systemd mount unit, read-only, at `/var/lib/potluck-agent/models`) when the source is `share:`, gets the image (given `--image`, a pull, or a local build from the agent binary and the Dockerfile embedded in the script) and starts the `potluck-agent` container with `--init`, host networking and, when there is an Nvidia GPU, `--gpus all`. Installing again removes the uninstall marker. Potluck connects to the machine on TCP 50060, 50052 and (later) 50061: when ufw is active the script allows these from Potluck's address and from every `--allow-from` entry (comment `potluck-agent`, listed in `/etc/potluck/firewall-rules`); when firewalld is active it only prints what to allow; `--no-firewall` skips both.

**`potluck-agent-uninstall`** is run as root on the node. It deregisters from Potluck (best effort), writes the marker on nodes that have a starter (or with `--marker`), stops launches, exporters and the agent, removes the unit or container and image, the ufw rules the setup listed, the share mount, the program folder, state and the binary. It exits non-zero if something important is still running.

**`Dockerfile`** builds the node image `nvidia/cuda:13.3.1-runtime-ubuntu24.04` plus `libgomp1` plus the agent binary. Programs are not baked in. Build context must contain the agent binary. Keep it in sync with the copy inside `potluck-agent-setup`.

## The three install paths

1. RAM-only (PS5): starter, started by the payload at every boot or apply.
2. Bare metal with a drive: `potluck-agent-setup --bare ...`.
3. Docker (Nvidia laptop): `potluck-agent-setup --docker ...`.

## To try on a real machine

Nothing here has been executed. Only `sh -n` was run.

- Starter on a PS5: the JSON parsing against a real `/api/pool/release` answer, `systemd-run` with the transient unit, running it twice, with the marker, and while the unit is restarting.
- Setup `--bare` on a Linux box: the unit starts, the agent registers, re-running replaces the unit and keeps a running launch.
- Setup `--docker` with `share:`: the mount unit name and options, `clientaddr`, the container sees `/models`, the local-build fallback, `--gpus` handling.
- Uninstall on each path: the `Transient` detection, the launch unit listing, the umount of `/run/potluck/models`, a second run being harmless.
- Port checks via `ss -ltn` on a machine whose agent is already running.
- The ufw rules on a machine with ufw active: `ufw allow ... comment potluck-agent` is accepted, and `ufw delete allow proto tcp from <src> to any port <port>` removes a rule that was added with a comment. Tested with a fake `ufw` only.
