# Hardware checks (design 3.2 H1-H3, design 11 "On hardware")

Run on the host machine with the scheduler container up (`container_name: potluck`).
Rules: the PS5 or Nvidia laptop under test must be idle (a busy `rpc-server` makes
`--list-devices` block forever, so every command is wrapped in `timeout`).
Nothing here is destructive except check 8 (you power a worker off on purpose).
Record each result in the table at the bottom.

## H1. `nvidia-smi` works inside the container

```sh
docker exec potluck nvidia-smi --query-gpu=memory.total,memory.used --format=csv,noheader,nounits
```

Expect: one line, two integers, e.g. `8151, 412`. If it errors: the scheduler
logs a warning and falls back to the fleet's figure for `host-1` (design 5.1).
Check the log for no such warning:

```sh
docker logs potluck 2>&1 | grep -i nvidia-smi
```

Expect: no output.

## H2. Each worker exposes exactly one device

Run once per worker (PS5 `192.0.2.2:50052`, Nvidia laptop `192.0.2.3:50052`, any other PS5):

```sh
docker exec potluck timeout 10 llama-server-rpc --rpc 192.0.2.2:50052 --list-devices | grep -c '^ *RPC'
```

Expect: `1`. Any other number fails H2: the pre-flight will quarantine that
node (design 7.5) and the log says to pin `-d` on that worker. Exit status of
`timeout` 124 (printed as nothing) means the worker is busy or silent: stop and
find out why before going on.

## H3. `--list-devices` is clean with no local GPU

```sh
docker exec potluck sh -c 'CUDA_VISIBLE_DEVICES="" timeout 10 llama-server-rpc --rpc 192.0.2.2:50052 --list-devices; echo "exit=$?"'
```

Expect: exactly one line of the form `  RPC0: <description> (<total> MiB, <free> MiB free)`,
no `CUDA0` line, and `exit=0`. If it fails or prints a CUDA device, the probe
must run without `CUDA_VISIBLE_DEVICES` (design H3 fallback): this is a code
change, report it to the engineer; do not work around it on the box.
Compare with the same command without the variable to see the difference.

## 4. PS5 usable memory (design 13 item 2)

```sh
docker exec potluck timeout 10 llama-server-rpc --rpc 192.0.2.2:50052 --list-devices
```

Note the `<free> MiB free` figure. Sources disagree (12 GB, 14.1 GB, 14948 MiB).
Later, after a load, compare it with what the model really took (check 7).

## 5. `--device` and `--tensor-split` behaviour on a PS5 pool

Needs two PS5s (or one PS5 and the Nvidia laptop) online. Use a forced profile
(for example the `gemma-measure` template in `config.yaml`).

```sh
curl -s -H 'Content-Type: application/json' -d '{"model":"gemma-measure"}' http://127.0.0.1:3122/api/load
curl -s http://127.0.0.1:3122/running
docker logs potluck 2>&1 | grep -E 'RPC[0-9]+\[|CUDA0' | tail -20
```

Expect: `/running` shows `--rpc a,b ... --device RPC0,RPC1 --tensor-split 60,40`
in that order, and the per-device buffer lines in the log are in roughly the
same 60/40 proportion, each on the endpoint named in brackets. A mismatch in
order or proportion fails this check (design 3.1 says device order follows the
order written).

## 6. Cold and warm load timing

```sh
M=gemma-4-12b:64k   # any profile that lands on a PS5
time curl -s -H 'Content-Type: application/json' -d "{\"model\":\"$M\",\"max_tokens\":1,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}" http://127.0.0.1:3122/v1/chat/completions >/dev/null
curl -s -X POST -H 'Content-Type: application/json' -d "{\"model\":\"$M\"}" http://127.0.0.1:3122/api/unload
sleep 15
time curl -s -H 'Content-Type: application/json' -d "{\"model\":\"$M\",\"max_tokens\":1,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}" http://127.0.0.1:3122/v1/chat/completions >/dev/null
```

Expect: the first (cold) run is bounded by the link: file size / 11 MB/s
(about 7 min per 5 GB sent to a PS5); the second (warm) is shorter because of
the worker's `-c` tensor cache. Compare the cold time with the load timeout
the scheduler computed (design 7.6: it must be comfortably above it).
Whether the cache really shortens repeat loads is an open point: record it.

## 7. Measured `needs_mib` and estimate versus observed

Gemma profiles and FLUX: see runbook section G. For the profiles with no
`needs_mib` (Devstral, Qwen, TinyLlama): load each, run
`scripts/measure-needs.sh`, and compare the SUM with what `POST /api/plan`
reports as the need (the field name is not fixed by the design: read the
plan output). Expect the estimate to be higher than observed (it is
deliberately cautious, design 7.2). Record both numbers.

## 8. Worker power-off during a generation

Expect (design 8.4): the instance goes to `stopping` after the node has been
offline in the fleet view for 30 s, in-flight requests fail, no hang.

```sh
# 1. start a long generation on a model placed on the PS5 (note which node)
curl -N -H 'Content-Type: application/json' -d '{"model":"gemma-4-12b","stream":true,"max_tokens":2000,"messages":[{"role":"user","content":"Write a long story."}]}' http://127.0.0.1:3122/v1/chat/completions
# 2. in another shell, power the worker off (PS5 or Nvidia laptop), then watch:
watch -n 5 "curl -s http://127.0.0.1:3122/running; echo; docker logs --tail 5 potluck"
```

Expect: stream ends with an error within about 15 s (fleet offline) + 30 s
(node-loss rule); `/running` shows `stopping`, then empty; the next request
for that model queues or fails with a 503 reason, not a hang. The design infers
(does not observe) that a head blocks when its worker loses power: this check
is the observation.

## 9. 10-agent mixed-model soak

```sh
BASE=http://127.0.0.1:3122
for i in 1 2 3 4 5 6 7 8 9 10; do
  case $((i % 3)) in 0) M=gemma-4-12b;; 1) M=qwen3.5-4b;; 2) M=tinyllama;; esac
  ( for n in 1 2 3 4 5; do
      curl -s -o /dev/null -w "agent $i $M %{http_code} %{time_total}s\n" -H 'Content-Type: application/json' \
        -d "{\"model\":\"$M\",\"max_tokens\":64,\"messages\":[{\"role\":\"user\",\"content\":\"Count to ten.\"}]}" $BASE/v1/chat/completions
      sleep $((RANDOM % 20))
    done ) &
done
wait
curl -s $BASE/api/status | head -c 3000
```

Expect: every line `200` or a deliberate `503` (cluster busy, with
`Retry-After: 30`); no `502` and no hang; at the end `/api/status` shows no
leftover claims or queue entries once everything is idle. Count the 503s.

## Results table

| Check | Date | Result | Notes |
|---|---|---|---|
| H1 | | | |
| H2 (per worker) | | | |
| H3 | | | |
| PS5 free MiB | | | |
| Pool order and split | | | |
| Cold / warm load | | | |
| Estimate vs observed | | | |
| Power-off | | | |
| Soak | | | |
