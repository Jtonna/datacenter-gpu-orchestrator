# Potluck Stats Contract (DRAFT 2)

Status: DRAFT 2. Draft 1 was reviewed by the Potluck team; this draft applies their changes.
Contract version: `stats_version` = 1 (draft).
Measurements were taken on 2026-11-20. Anything not marked as measured is marked Proposed or Estimate.

## Changes since draft 1

1. Scrape interval is now self-adjusting per exporter: next interval = max(2 s, 3 x last scrape duration), capped at 10 s. The 4 s timeout and the no-overlap rule stay. The agent reports `scrape_ms` per source.
2. The four top-level status fields are replaced by one `sources` map (`node`, `nvidia`), each with `status`, `age_ms`, `scrape_ms`. A source that is not configured is absent from the map.
3. Report rate fixed at every 2 s, latest value plus age; no max-since-last is sent.
4. A missing or unparsable `stats` never rejects a report and never blocks loads. Stated as a rule.
5. Nvidia `gpus` entries get optional `index` and `mem_reserved_bytes`. CUDA order and Vulkan matching rules added.
6. The 5% disagreement threshold and the "Potluck flags a disagreement" wording are removed. Comparison is Potluck's own behaviour and outside this contract.
7. A missing or crashing exporter makes that source `absent`; it is a warning on the node and never a reason to refuse loads.
8. Headline temperature: `temps[].chip_name` added; storage, wireless and memory-module chips are excluded; every `gpus[].temp_c` is included.
9. `net[]` gets `primary: true` for the interface that carries the agent's route to Potluck; it is always sent and sent first.
10. `load5` and `load15` are optional; `load1` stays required.
11. The flag check is enough for the loopback rule; the agent builds the arguments itself.
12. Integrated GPUs on mixed machines are listed by default. `kind` is required on every entry. Pool totals are Potluck's business.
13. `mem_source` is required on every entry, with values `drm_vram_gtt`, `drm_vram`, `nvidia_smi`.
14. Version rule: `stats_version` must equal Potluck's, otherwise "stats version not supported" and no parsing. The "previous version" allowance is removed.
15. New section 1.1 states what is not taken from the exporters (launch memory check, watchdog and placement).
16. New section 8 "Docker installs".
17. Caps added: `gpus` at most 8, `fans` at most 8.
18. Section numbering fixed (5.5 and 5.6 swapped, cross-references updated).
19. Missing-data section states that a missing `link_mbps` on a PS5 is expected and not a fault.
20. One sentence on units: bytes here, MiB in the agent contract's `devices` list.
21. Open questions reduced to the items still open.

## 1. Purpose, scope, decisions

This document defines which exporter metrics the `potluck-agent` reads on each machine, how it turns them into rates and a small normalised set of readings, and the exact shape in which it sends them to Potluck.
Potluck receives only the normalised set. It never sees exporter output and contains no hardware-specific code.
Scope: Linux machines (PS5 consoles running Ubuntu with AMD integrated graphics, and Linux PCs with Nvidia cards). Windows and other GPU vendors are in section 9 only.
Units: byte units are used throughout this contract. The agent contract's `devices` list uses MiB because that is what llama prints; no conversion is defined here.

Decisions this document rests on (made by the project owner):

1. Every pool machine runs Prometheus `node_exporter` as a plain local process (no Docker on PS5s).
2. Machines with an Nvidia card also run `nvidia_gpu_exporter` (wraps `nvidia-smi`).
3. Both exporters listen on 127.0.0.1 only. The agent scrapes them (pull) and forwards a compact set in its regular report. Nothing else pushes.
4. Remote dashboards read Potluck's feed. A local text UI on a PS5 may read the local exporters directly.
5. Potluck contains no PS5-specific code. Hardware differences are expressed as "which exporter metrics exist", never as machine type names.
6. PS5 clocks are wrong. No field depends on a node's wall-clock time. Fields use ages and durations. Potluck stamps arrival time itself.
7. Exact graphics memory for model placement comes from what llama.cpp reports per device. The exporter-derived figure is a second source, shown in dashboards and available for cross-checking.

### 1.1 What is NOT taken from the exporters

This is a rule, not a hint.

1. The agent reads `MemAvailable` itself from `/proc/meminfo` for its launch memory check and its watchdog. It does this more often than the scrape interval, and it must keep working when the exporter is dead.
2. Device memory for placement comes from llama, per device.
3. `stats.memory` and `gpus[].mem_*` are for display and cross-checking only. No launch, placement or refusal decision may depend on them.
4. A stats problem of any kind (exporter missing, crashed, slow, unparsable, version mismatch) is never a reason to refuse or stop loads on a node.

## 2. Exporter set per platform

### 2.1 Versions tested

| Component | Version tested | Where |
|---|---|---|
| node_exporter | 1.13.1 | PS5 (Ubuntu), and a Linux laptop (Ubuntu 26.04, kernel 7.0) |
| nvidia_gpu_exporter (utkuozdemir) | 2.0.0 | Linux laptop with RTX 5060 8 GB, driver 595.99.02 |

### 2.2 Which exporters run where

| Platform | node_exporter | nvidia_gpu_exporter |
|---|---|---|
| PS5, Linux, AMD integrated GPU | required | not present |
| Linux PC with Nvidia card | required | required |
| Linux laptop with Nvidia card and AMD integrated GPU | required | required |

### 2.3 Command lines

`<NODE_ADDR>` and `<NV_ADDR>` are fixed by the agent contract as `127.0.0.1:50070` (node_exporter) and `127.0.0.1:50071` (nvidia_gpu_exporter). Both must be a 127.0.0.1 address with a port. (The tests in this document used other ports, 61211, 61221 and 61222; the ports do not affect the readings.)

node_exporter (measured on PS5 with port 61211; same flags used for the laptop samples):

```
node_exporter --web.listen-address=<NODE_ADDR> --collector.disable-defaults --collector.drm --collector.hwmon --collector.cpu --collector.meminfo --collector.netdev --collector.netclass --collector.loadavg
```

nvidia_gpu_exporter (measured on the laptop with port 61222; no other flags were used):

```
nvidia_gpu_exporter --web.listen-address=<NV_ADDR>
```

### 2.4 Required node_exporter collectors

| Collector | Why it is required |
|---|---|
| cpu | `node_cpu_seconds_total` for CPU utilisation |
| meminfo | `MemTotal`, `MemAvailable` |
| loadavg | `node_load1` (and load5/load15 where present) |
| netdev | rx/tx byte counters per interface |
| netclass | `node_network_up`, `node_network_speed_bytes`, interface link state |
| hwmon | temperatures, GPU clock (`sclk`), fan PWM/RPM, power, sensor labels, chip names |
| drm | AMD GPU memory (VRAM and GTT), GPU busy percent, card identity |

`--collector.disable-defaults` is required so that nothing else is enabled.

### 2.5 What must be absent

1. No collector that walks the process list (`processes`, `systemd`, `supervisord`, `textfile` with user data, `perf`, and similar). The seven above are the whole allowed set.
2. Nothing may listen on a non-loopback address. The agent builds the exporter arguments itself, and refuses (does not start the exporter) if the listen address does not start with `127.0.0.1:`. Checking the flag is enough; no outside connection test is needed.
3. Measured on the laptop with `ss -ltn`: each exporter listened on `127.0.0.1:<port>` only.
4. Proposed: the agent should not enable `--web.config`, TLS, or auth. Loopback is the boundary.

### 2.6 Cost evidence

| Measurement | node_exporter | nvidia_gpu_exporter | Machine |
|---|---|---|---|
| Binary size | 25 MB | not measured | PS5 |
| Resident memory | 21 MB | 17 MB (RSS 17352 kB) | PS5 / laptop |
| CPU | 0.33% of one core at 2 s interval | not measured | PS5 |
| Scrape time | 17-34 ms | 110-113 ms (3 runs) | PS5 / laptop |
| Output size | about 800 lines | 417 lines, 28 KB | PS5 / laptop |

Laptop node_exporter scrape (same flags) took 0.90-0.96 s and returned 2620 lines (154 KB). This is 30-50 times slower than the PS5. Per collector, measured with `?collect[]=<name>`: hwmon 1.27 s, netclass 29 ms, drm 5 ms, cpu 4 ms, netdev 3 ms, meminfo 1.4 ms, loadavg 0.9 ms. So one slow hwmon sensor on that laptop dominates. The cause was not diagnosed. The self-adjusting interval, the timeout and the no-overlap rule below exist because of this.

## 3. Scrape rules for the agent

| Rule | Value | Status |
|---|---|---|
| Scrape interval | Self-adjusting, per exporter: next interval = max(2 s, 3 x duration of the last scrape), capped at 10 s. Examples: 34 ms gives 2 s; 0.95 s gives 2.85 s; a scrape that timed out (4 s) gives 10 s. | Proposed. Evidence: 0.33% of one core at 2 s on a PS5 (measured). |
| Report rate to Potluck | The agent's report goes every 2 s and carries the latest value of each reading plus its age. The agent sends no maximum or other summary since the previous report. When the scrape interval is longer than 2 s, successive reports repeat the same reading with a growing `age_ms`. | Proposed |
| Overlap | Never run two scrapes of the same exporter at once. If a scrape is still running at the next tick, skip that tick. | Proposed |
| Timeout per scrape | 4 s (HTTP request, connect plus body) | Proposed. Laptop hwmon was 1.3 s (measured); 4 s leaves about 3x headroom. |
| Scrape each exporter independently | A failure of one must not drop the other's data | Proposed |
| `scrape_ms` | The duration in milliseconds of the most recent successful scrape of that exporter, as seen by the agent. Reported in `sources.<name>.scrape_ms`. | Proposed |
| Scrape failed or timed out | Keep the last good values, do not invent zeros. A failed scrape counts with its elapsed time for the interval rule, so a timeout leads to the 10 s interval. The first success restores the normal rule. | Proposed |
| Exporter missing, not startable or crashing | The agent still runs. The source is reported as `absent` (never scraped) or `stale` (scraped before, now failing). This is a warning on the node. It is never a reason to refuse loads. | Proposed |
| Staleness | Each source carries `age_ms`: monotonic milliseconds between the start of the scrape that produced its latest good data and the moment the report is built. A source is `stale` when `age_ms` > max(10000, 3 x current interval in ms). Stale data is still sent, with its real `age_ms`. If an exporter has never succeeded, its source is `absent`. | Proposed |
| Parsing | Prometheus text format. Ignore every metric, label and HELP/TYPE line not named in this document. A new unknown metric or label must never cause an error. | Proposed |
| Value parsing | Values appear in scientific notation (`1.5137001472e+10`). Parse as float64. `NaN`, `+Inf`, `-Inf` mean "unknown" for that reading. | Proposed |

### 3.1 Counters to rates

All time arithmetic uses the agent's monotonic clock (for example Go `time.Since` on a monotonic reading). The node's wall-clock time is never read for this.

Let `dt` be the monotonic seconds between two successive successful scrapes of the same exporter, and `prev` and `cur` the counter values.

| Derived value | Formula | Notes |
|---|---|---|
| CPU utilisation, percent | `100 * (1 - (d_idle + d_iowait) / d_total)` where `d_*` are deltas of `node_cpu_seconds_total` summed over all `cpu` labels, and `d_total` is the sum of deltas over all modes | Needs no clock at all. Modes: user, nice, system, idle, iowait, irq, softirq, steal. Clamp to 0-100. If `d_total` is 0, keep the previous value. Proposed. |
| Network rx rate, Mb/s | `(cur - prev) * 8 / dt / 1e6` from `node_network_receive_bytes_total` | Decimal megabits per second (1 Mb = 1,000,000 bits). Proposed. |
| Network tx rate, Mb/s | same, from `node_network_transmit_bytes_total` | Proposed |

Rules:

1. The first scrape after agent start (or after an exporter restart) produces no rate. The rate field is omitted until a second successful scrape.
2. If any `cur < prev` (counter reset, exporter restart, interface re-created), discard that sample and start again from `cur`. Do not report a negative or huge rate.
3. If `dt` is below 0.5 s or above 30 s, discard the sample (the interval is not trustworthy). Estimate, not measured.
4. The rate is for the interval between the last two successful scrapes. No smoothing is applied by the agent. Proposed. Dashboards may smooth.

## 4. The normalised report

The agent adds one object, `stats`, to its regular report to Potluck. The shape is below. Units are in the field names where practical.

Rule (Proposed): Potluck accepts a report whose `stats` is missing or cannot be parsed, and shows "no stats" for that node. A stats problem never rejects a report and never makes a node unusable for loads.

### 4.1 Top-level fields

| Field | Type | Unit | Required | Source metric(s) | Derivation |
|---|---|---|---|---|---|
| `stats_version` | integer | none | required | none | Constant. 1 for this draft. Must equal the version Potluck expects (section 7). |
| `sources` | object | | required | scrape state | Map from source name (`node`, `nvidia`) to a source object (4.1.1). A source not configured on the machine is not in the map. A PS5 has no `nvidia` key. |
| `cpu` | object | | required when `sources.node.status` is `ok` or `stale`; otherwise omitted | | See 4.2 |
| `memory` | object | | same as `cpu` | | See 4.3 |
| `load` | object | | same as `cpu` | | See 4.4 |
| `net` | array of objects | | same as `cpu` (may be empty) | | See 4.5 |
| `temps` | array of objects | | same as `cpu` (may be empty) | | See 4.6 |
| `headline_temp_c` | number | degrees Celsius | optional | derived from `temps` and `gpus` | Rule in 4.6. |
| `fans` | array of objects | | same as `cpu` (may be empty; at most 8) | | See 4.7 |
| `gpus` | array of objects | | required (may be empty; at most 8) | | See 5 |

#### 4.1.1 Source object, `sources.<name>`

| Field | Type | Unit | Required | Source | Derivation |
|---|---|---|---|---|---|
| `status` | string enum `ok` / `stale` / `absent` | none | required | scrape state | `ok`: fresh. `stale`: last success older than the staleness limit in section 3. `absent`: configured but never successfully scraped. |
| `age_ms` | integer | milliseconds | required if `status` is `ok` or `stale`; omitted if `absent` | monotonic clock | Section 3, staleness. |
| `scrape_ms` | integer | milliseconds | required if `status` is `ok` or `stale`; omitted if `absent` | monotonic clock | Duration of the last successful scrape. |

Drm-based (AMD) GPUs follow `sources.node`. Nvidia GPUs follow `sources.nvidia`. If `sources.nvidia` is `absent`, `gpus` contains no `nvidia` entries.

### 4.2 `cpu`

| Field | Type | Unit | Required | Source | Derivation |
|---|---|---|---|---|---|
| `util_percent` | number | percent, 0-100 | optional (omitted until second scrape) | `node_cpu_seconds_total{cpu,mode}` | Section 3.1 |
| `logical_cpus` | integer | count | optional | `node_cpu_seconds_total` | Number of distinct `cpu` label values. |

### 4.3 `memory`

For display and cross-checking only (section 1.1).

| Field | Type | Unit | Required | Source | Derivation |
|---|---|---|---|---|---|
| `total_bytes` | integer | bytes | required | `node_memory_MemTotal_bytes` | Value as is. |
| `available_bytes` | integer | bytes | required | `node_memory_MemAvailable_bytes` | Value as is. |

### 4.4 `load`

| Field | Type | Unit | Required | Source | Derivation |
|---|---|---|---|---|---|
| `load1` | number | runnable tasks (average over 1 min) | required | `node_load1` | As is. |
| `load5` | number | same, 5 min | optional | `node_load5` | As is. Seen on the laptop. Not confirmed on the PS5. |
| `load15` | number | same, 15 min | optional | `node_load15` | As is. Seen on the laptop. Not confirmed on the PS5. |

### 4.5 `net` (one element per reported interface)

| Field | Type | Unit | Required | Source | Derivation |
|---|---|---|---|---|---|
| `name` | string | none | required | `device` label | As is. |
| `primary` | boolean | none | optional; present only as `true` | agent's own knowledge | `true` on the one interface that carries the agent's route to Potluck. Omitted on all others. The method the agent uses to find it is not part of this contract. |
| `up` | boolean | none | required | `node_network_up{device}` | `value == 1`. |
| `rx_mbps` | number | megabits per second | optional | `node_network_receive_bytes_total` | Section 3.1. |
| `tx_mbps` | number | megabits per second | optional | `node_network_transmit_bytes_total` | Section 3.1. |
| `link_mbps` | number | megabits per second | optional | `node_network_speed_bytes{device}` | `value * 8 / 1e6`. Omit if absent or value <= 0. See section 6. |

Interface selection. Proposed: report every interface except `lo` and names starting with `veth`, `docker`, `br-`, `virbr`, `cilium`, `lxc`, `flannel`, `cni`, `tailscale`, `tun`, `tap`. Weakness: a denylist by name needs upkeep, and a VPN or bridge with an unlisted name would show up. Measured on the laptop: 15 or more interfaces including `cilium_*`, `lxc*`, `lo`, `tailscale0`, one wired USB adapter and a Wi-Fi interface.
Cap: at most 8 interfaces sent. The primary interface is always sent, even if its name matches the denylist, and is always the first element. The other slots go to the remaining interfaces with the highest `rx+tx` byte counter.

### 4.6 `temps` and the headline temperature

`temps` element:

| Field | Type | Unit | Required | Source | Derivation |
|---|---|---|---|---|---|
| `id` | string | none | required | `chip`, `sensor` labels | `<chip>/<sensor>`, opaque to Potluck. |
| `label` | string | none | optional | `node_hwmon_sensor_label{chip,sensor,label}` | Label whose `sensor` equals the temp sensor name. Omit if there is none. |
| `chip_name` | string | none | optional | `node_hwmon_chip_names{chip,chip_name}` | The `chip_name` label of the series whose `chip` label equals this sensor's `chip`. It is the Linux driver name (for example `nvme`, `k10temp`, `amdgpu`), not a machine name. Omit if there is no such series. |
| `celsius` | number | degrees Celsius | required | `node_hwmon_temp_celsius{chip,sensor}` | As is. |

Validity: drop a sensor if the value is <= 0 or >= 125. Measured: two memory-module sensors on the laptop reported exactly 0, which is not a temperature. Send at most 16 temperatures, highest first.

Headline rule (Proposed): `headline_temp_c` is the maximum of:

1. `celsius` over every valid `temps` entry whose `chip_name` is not in the excluded list below (an entry with no `chip_name` is included), and
2. `temp_c` of every entry in `gpus`.

If both sets are empty, omit `headline_temp_c`.

Why the second term exists: an Nvidia card is not in hwmon, so its temperature is not in `temps`. Without the `gpus` term it could never be the headline, even when it is the hottest part of the machine.

Excluded `chip_name` values (storage, wireless and memory-module drivers). Matching is exact or by prefix:

| Class | Excluded names | Seen in real output |
|---|---|---|
| Storage | `nvme`, `drivetemp` | `nvme` (laptop) |
| Wireless | `iwlwifi`, and prefixes `mt79`, `ath`, `rtw`, `brcm` | `mt7921_phy0` (laptop) |
| Memory module | `spd5118`, `jc42` | `spd5118` (laptop) |

Only `nvme`, `mt7921_phy0` and `spd5118` appeared in real output. The others are named from general knowledge of Linux drivers and were not seen on any machine tested. The list is extensible: a new name is added by a contract revision that does not change `stats_version` (it changes only which sensors can win the maximum). An unknown chip, or one with no `chip_name`, is included in the maximum.

Weaknesses:

1. The list names drivers, so it needs upkeep. A new storage or wireless driver not on the list will be included and can win the maximum.
2. Included but not excluded: battery, power-supply and ACPI thermal-zone sensors (on the laptop, `acpitz` read 62-64.5 C). These can win even though they are not the processor.
3. A broken sensor that reports a high but valid value (0 < value < 125) would dominate if its chip is not excluded.
4. On a PS5 the maximum measured was one of the MainSoC, Local1-3, Tctl, edge or Wi-Fi sensors; peaks of 72-87 C were seen briefly during a model load. The `chip_name` values of the PS5's chips were not recorded, so which entries the list would exclude there is not known. Estimate: the PS5's Wi-Fi sensor is excluded only if its driver name matches the list.
5. Dashboards that need a real CPU or GPU temperature should use `gpus[].temp_c` and the labelled entries in `temps`, not the headline.

### 4.7 `fans` (machine-level, at most 8)

| Field | Type | Unit | Required | Source | Derivation |
|---|---|---|---|---|---|
| `id` | string | none | required | `chip`, `sensor` labels | `<chip>/<sensor>` of the pwm sensor, for example `platform_ps5_fan/pwm1`. |
| `duty_percent` | number | percent, 0-100 | optional | `node_hwmon_pwm{chip,sensor="pwmN"}` | `value / 255 * 100`. Scale 0-255 is the Linux hwmon standard and was confirmed on the PS5 (observed 26 to 111). Clamp to 0-100. |
| `rpm` | number | revolutions per minute | optional | `node_hwmon_fan_rpm{chip,sensor="fanN"}` | Pair `pwmN` with `fanN` on the same chip. Omit if no such series. |

A fan with only RPM and no PWM series is also sent, with `duty_percent` omitted. If more than 8 fans exist, send the first 8 ordered by `id` ascending (Proposed).
Measured on the laptop: `platform_hp_wmi` has `pwm1` = 0 and two `fan_rpm` series that read 0. A value of 0 may mean "stopped" or "not supported". Reported as is. Weakness noted, not resolved.

## 5. GPUs and the memory rule

### 5.1 `gpus` element (at most 8 entries)

If more than 8 devices exist, send the first 8 ordered by `id` ascending (Proposed).

| Field | Type | Unit | Required | Source | Derivation |
|---|---|---|---|---|---|
| `id` | string | none | required | see 5.5 | Stable across reboots. `nvidia:<uuid>` or `pci:<address>`. |
| `vendor` | string enum `nvidia` / `amd` | none | required | exporter / `node_drm_card_info{vendor}` | Lower-case as reported. Other vendors are not specified (section 9). |
| `name` | string | none | optional | `nvidia_smi_gpu_info{name}` | Not available for drm devices; omit. |
| `index` | integer | none | optional; Nvidia only | `nvidia_smi_gpu_info{index}` | The `index` label as an integer. Omit for drm devices. See 5.6. |
| `pci_address` | string | none | optional | see 5.5 | Lower-case `dddd:bb:dd.f`. Used to detect duplicates and, if llama reports it, to match llama devices. |
| `kind` | string enum `integrated` / `discrete` | none | required | rule in 5.3 | Derived from metric shape only. Nvidia is always `discrete` here (Proposed; Nvidia integrated parts such as Jetson are not covered). |
| `util_percent` | number | percent, 0-100 | optional | AMD: `node_drm_gpu_busy_percent{card}`; Nvidia: `nvidia_smi_utilization_gpu_ratio{uuid}` | AMD: as is. Nvidia: `ratio * 100`. Clamp to 0-100. The Nvidia ratio was only observed at 0 (idle); that 1.0 means 100% is not yet confirmed under load (open item 2). |
| `mem_total_bytes` | integer | bytes | required | see 5.2 | See rule. |
| `mem_used_bytes` | integer | bytes | required | see 5.2 | See rule. |
| `mem_reserved_bytes` | integer | bytes | optional; Nvidia only | `nvidia_smi_memory_reserved_bytes{uuid}` | As is. Omit for drm devices. |
| `mem_source` | string enum | none | required | none | `drm_vram_gtt` (integrated formula), `drm_vram` (discrete AMD card, VRAM only, untested), `nvidia_smi` (Nvidia). Tells readers which formula produced the figures. |
| `temp_c` | number | degrees Celsius | optional | AMD: `node_hwmon_temp_celsius` on the GPU's chip; Nvidia: `nvidia_smi_temperature_gpu{uuid}` | AMD: sensor labelled `edge`, else the lowest-numbered temp sensor on the same `chip` label as the card. Nvidia: as is. |
| `clock_mhz` | number | megahertz | optional | AMD: `node_hwmon_freq_freq_mhz{chip,sensor="sclk"}`; Nvidia: `nvidia_smi_clocks_current_graphics_clock_hz{uuid}` | AMD: as is. Nvidia: `value / 1e6`. The Nvidia metric is in hertz (measured 1.8e+08 = 180 MHz at idle). |
| `fan_percent` | number | percent, 0-100 | optional | Nvidia: `nvidia_smi_fan_speed_ratio{uuid}` | `ratio * 100`, clamped to 0-100. Absent on the laptop tested. AMD GPUs: omit; the fan is reported in machine-level `fans`. This field is a percentage of maximum speed, not PWM duty. Scale not yet confirmed on a desktop card (open item 2). |
| `power_w` | number | watts | optional | Nvidia: `nvidia_smi_power_draw_watts{uuid}`; AMD: `node_hwmon_power_watt{chip}` on the GPU's chip | As is. Seen on the AMD integrated GPU of the laptop (label `PPT`, 19.8 W). Not seen on the PS5. |

### 5.2 The memory rule

For a drm device (AMD) classified `integrated` (5.3):

```
mem_total_bytes = node_drm_memory_vram_size_bytes + node_drm_memory_gtt_size_bytes
mem_used_bytes  = node_drm_memory_vram_used_bytes + node_drm_memory_gtt_used_bytes
mem_source      = drm_vram_gtt
```

For a drm device classified `discrete` (untested; Proposed):

```
mem_total_bytes = node_drm_memory_vram_size_bytes
mem_used_bytes  = node_drm_memory_vram_used_bytes
mem_source      = drm_vram
```

Justification (measured on a PS5): VRAM size is a fixed 536,870,912 bytes (512 MB) and GTT size is 15,137,001,472 bytes (about 14.1 GB). The real memory pool for an integrated GPU is the sum. A figure from VRAM alone is wrong by a factor of about 28 (GTT is 28.2 times VRAM; the sum is 29.2 times VRAM). During a Devstral 24B load split over three consoles, GTT used rose from 0.04 GB to 4.3-4.5 GB and VRAM used rose to 380-485 MB. A VRAM-only figure would have shown the VRAM almost full while most data sat in GTT. Do not use `vis_vram_*` (visible VRAM); on both machines tested it equals `vram_*` and adds nothing.

Measured on the laptop's AMD integrated GPU (Raphael): VRAM 536,870,912 bytes and GTT 16,049,745,920 bytes, the same shape as the PS5.

For an Nvidia device:

```
mem_total_bytes    = nvidia_smi_memory_total_bytes{uuid}
mem_used_bytes     = nvidia_smi_memory_used_bytes{uuid}
mem_reserved_bytes = nvidia_smi_memory_reserved_bytes{uuid}
mem_source         = nvidia_smi
```

Measured on the RTX 5060 laptop (idle): total 8,546,942,976; free 8,081,375,232; used 2,097,152; reserved 465,567,744. `used` excludes the driver-reserved part. The figure that should agree with llama's free memory is `free = total - used - reserved`. In this sample the exporter's own `memory_free_bytes` equals `total - reserved` exactly (8,081,375,232) and does not subtract `used`, so it is 2,097,152 bytes (2 MiB) higher than `total - used - reserved` (8,079,278,080). The agent must not send `total - free` as `used`, since that would count reserved memory as used. Proposed.

Integer conversion: values are floats in scientific notation; round to the nearest integer.

### 5.3 Deciding whether a drm device is integrated, without machine names

A card in `node_drm_card_info` is processed only if `vendor="amd"`. Other vendors are ignored in this version. For each such card (the `card` label), the agent reads the four memory series with the same `card` label.

Rule (Proposed): the card is `integrated` when both hold:

1. `node_drm_memory_vram_size_bytes` <= 2 GiB (2,147,483,648 bytes), and
2. `node_drm_memory_gtt_size_bytes` >= 8 x `node_drm_memory_vram_size_bytes`.

Otherwise it is `discrete`. Adding GTT to a discrete card's figure would over-count (GTT there is host RAM the card can borrow, not on-card memory), so a discrete card uses the VRAM-only formula in 5.2. This is a Proposed rule for the future AMD discrete case and is not tested.

Test against measured data: PS5 gives 512 MB VRAM and 14.1 GB GTT (ratio about 28), laptop iGPU gives 512 MB and 16.05 GB (ratio about 30). Both pass.

Weaknesses (false positives and negatives):

1. A discrete card with 2 GiB or less VRAM on a machine with at least 16 GiB of RAM would be called integrated. Such cards are rare and weak for LLMs, but a false positive would add GTT, which is host RAM, to its memory figure. Estimate; not tested.
2. An integrated GPU whose firmware assigns a large VRAM carve-out (for example 4 GiB or more) would fail rule 1 and be called discrete, which under-counts its memory by the GTT amount. Some PCs allow this in firmware. Estimate; not tested.
3. The drm collector does not expose a device name. Field `name` is omitted for these devices.
4. Alternative rejected: use `memory_vendor` or `power_performance_level` from `node_drm_card_info`. Both labels were empty or just "auto" on the two machines tested, so they carry no signal.

### 5.4 Machines with both an Nvidia card and an AMD integrated GPU

Measured on the laptop (RTX 5060 plus AMD Raphael iGPU): the drm collector listed only the AMD card (`card2`, vendor `amd`). It did not list the Nvidia card, even though `/sys/class/drm` has `card1` for it. The Nvidia card appears only through `nvidia_gpu_exporter`. The iGPU appears only through drm.

Rules:

1. Report every device either exporter lists. An integrated GPU on a mixed machine is listed by default, with `kind: "integrated"`. On this laptop that is two entries in `gpus`: one `vendor=nvidia`, `kind=discrete`; one `vendor=amd`, `kind=integrated`.
2. Duplicate avoidance (Proposed): compute `pci_address` for every device. If a drm card and an Nvidia entry have the same `pci_address`, keep the Nvidia entry and drop the drm one. Today this cannot happen, because drm only lists `vendor="amd"` here, but the rule is stated so that a future node_exporter that lists Nvidia cards will not double count.
3. Potluck does not decide which GPU a model uses from these entries. Placement uses llama's own per-device list (5.6). The stats entries describe the hardware.
4. How pool totals are computed from the `gpus` entries (for example whether an integrated GPU on a mixed machine counts) is Potluck's business and not part of this contract.

### 5.5 Stable ids and PCI addresses

| Source | `id` | `pci_address` |
|---|---|---|
| Nvidia | `nvidia:` + `uuid` label of `nvidia_smi_gpu_info` | `pci_bus_id` label, lower-case, with the 8-digit domain shortened to 4 digits: `00000000:01:00.0` becomes `0000:01:00.0` |
| drm | `pci:` + pci_address | Parse the `chip` label of `node_drm_card_info`. It is a bridge path with `.` and `/` replaced by `_`, for example `0000:00:08_1_0000:20:00_0`. Take the final group: regex `(\w{4}:\w{2}:\w{2})_(\d)$`, giving `0000:20:00.0`. If the regex fails, use `drm:` + `card` label as `id` and omit `pci_address`. |

Weakness: the `card` label (`card0`, `card2`) can change between boots, so it is not used when the PCI address is available. Measured: the PS5 card is `card0`; the laptop iGPU is `card2`.

### 5.6 Placement versus stats, and matching to llama devices

1. Model placement uses the per-device memory figures reported by the llama.cpp program itself. The stats `mem_*` fields are not an input to placement (section 1.1).
2. Potluck shows the exporter-derived figures in dashboards next to the llama figures.
3. Potluck may compare the two sources. The comparison, any threshold and what it shows are Potluck's own behaviour and are not part of this contract. The contract only guarantees that the stats figures are defined as in 5.2. For Nvidia, `total - used - reserved` (free) is the figure that should agree with llama's free figure.
4. Matching a llama device to a stats entry:
   - CUDA devices: the agent starts Nvidia launches with `CUDA_DEVICE_ORDER=PCI_BUS_ID`. Then llama's `CUDA<n>` equals the `index` field of the Nvidia entry with `index == n`. (The environment variable is a CUDA setting; its effect was not tested for this document.)
   - Vulkan devices: matched by vendor and position. Proposed: the n-th Vulkan device of a vendor corresponds to the n-th `gpus` entry of that vendor, ordered by `pci_address` ascending. The ordering assumption was not tested.
   - Open item 1: confirm in testing whether llama reports a PCI address per device; if it does, matching by `pci_address` replaces the position rule.
5. Measured nuance: the Nvidia total includes the 465 MB the driver reserves on the laptop tested; llama may report a different total. That is why `mem_reserved_bytes` is sent.

## 6. Missing data

Representation rule (Proposed): an unknown optional field is omitted from the JSON. It is never `null`, never `0`, never `-1`, never an empty string. A field that is present always has a real reading. Required fields are always present; if the source is missing for a required field, the whole containing object (for example a `gpus` element) is not sent, and `sources.<name>.status` carries the reason.

Consumers must treat "omitted" as "unknown". Unknown is different from zero: a fan at 0% duty is sent as `duty_percent: 0`.

Worked examples:

| Case | What the exporter shows | What the agent sends |
|---|---|---|
| PS5 wired link speed | `node_network_speed_bytes` is not reported for the PS5's wired interface (the driver reports nothing) | `net[]` element has `name`, `up`, `rx_mbps`, `tx_mbps`; `link_mbps` is omitted. This is expected on a PS5 and is not a fault. |
| Laptop interface that is down | `node_network_speed_bytes{device="eno1"}` = -125000 (negative, meaning unknown) | `link_mbps` omitted (value <= 0). Measured. |
| Laptop USB Ethernet | `node_network_speed_bytes` = 6.25e+08 | `link_mbps` = 5000 |
| PS5 fan RPM | The fan chip (`platform_ps5_fan`) has `node_hwmon_pwm` only, no `node_hwmon_fan_rpm` | `fans[]` element has `id` and `duty_percent`; `rpm` omitted |
| Nvidia fan | `nvidia_smi_fan_speed_ratio` absent on the laptop tested | `fan_percent` omitted for that GPU |
| First scrape | No previous counter | `cpu.util_percent`, `rx_mbps`, `tx_mbps` omitted |
| Exporter down after working | No fresh data | Last good data kept with growing `age_ms`; `sources.<name>.status` is `stale` |
| Exporter never worked | No data | `sources.<name>` is `{"status": "absent"}`; for `node`, the `cpu`, `memory`, `load`, `net`, `temps`, `fans` objects are omitted |
| Sensor reads 0 | Two laptop memory-module sensors read exactly 0 | Entry dropped from `temps` |
| No Nvidia exporter configured | Not running | No `nvidia` key in `sources`; no `nvidia` entries in `gpus` |
| Drm device | No name, index or reserved figure in drm | `name`, `index`, `mem_reserved_bytes` omitted |
| `stats` missing or unparsable | | Potluck shows "no stats"; the report is accepted (section 4) |

## 7. Compatibility and change rules

All of this section is Proposed.

1. `stats_version` is an integer. It changes only on a breaking change.
2. `stats_version` must equal the version Potluck expects. If it does not, Potluck records "stats version not supported" for that node and does not parse the `stats` object. The report itself is still accepted (section 4). There is no allowance for a previous version.
3. Adding a new optional field, a new optional array element field, or a new enum value for `kind` or `mem_source` is not breaking. The version stays the same. Readers must ignore unknown fields and must treat an unknown enum value as "unknown".
4. A breaking change (requires a version bump) is any of: removing or renaming a field; changing a field's unit, scale or meaning (for example percent to ratio); making an optional field required; changing the representation of unknown; changing the memory rule so that the same inputs produce different values; changing the headline-temperature rule in a way that changes its meaning (extending the excluded list in 4.6 is not breaking).
5. The agent sends exactly one `stats_version` per report.
6. Changes to the rules in sections 3 to 5 that alter the result for existing hardware are breaking even if no field changes, except the extensible list in 4.6.

### Pinning exporters

1. The agent's release names the exact exporter versions it was tested with: node_exporter 1.13.1 and nvidia_gpu_exporter 2.0.0 for this draft.
2. Binaries are pinned by version and checksum. They are never fetched as "latest".
3. When bumping a version, run the same command line on one PS5 and one Nvidia Linux machine and check:
   - every metric name and label in the appendix still exists with the same unit (watch for `_ratio` versus `_percent`, and for `_hz` versus `_mhz`);
   - HELP text did not hint at a unit change (the nvidia HELP lines say "[MHz]" and "[%]" while the metric names say `_hz` and `_ratio`; values matched the names);
   - the PS5's drm and hwmon series are unchanged: `node_drm_memory_gtt_*`, `node_drm_memory_vram_*`, `node_hwmon_freq_freq_mhz`, `node_hwmon_pwm`, `node_hwmon_chip_names`;
   - the `--collector.*` flags are still accepted, and `--collector.disable-defaults` still leaves only the seven collectors;
   - scrape time and resident memory are still close to section 2.6;
   - the memory rule still gives roughly the same totals on the PS5 (about 15.67 GB for the machine tested).
4. Record the result in the agent's release notes.

## 8. Docker installs

When the agent runs in a Docker container (not the PS5 case, which has no Docker):

1. The exporters run as child processes of the agent, inside its container. The listen addresses and flags are the same as in section 2.3.
2. With host networking, the network collectors (`netdev`, `netclass`) see the machine's interfaces. Without host networking they would see only the container's interfaces, which is not supported.
3. The `hwmon` and `drm` collectors need the machine's `/sys` visible inside the container. The container must also see the machine's CPU and memory figures; `/proc` inside a container may show container limits for some fields (not checked here).
4. `nvidia_gpu_exporter` needs `nvidia-smi` inside the container, with access to the Nvidia driver (for example through the Nvidia container runtime).
5. All samples in this document were taken on the host, not in a container.
6. Phase 4 must check that the same metric lines appear from inside the agent container (open item 3), and record the mounts and settings the setup script needs.

## 9. Future platforms (not specified)

This section is not part of the contract. Nothing here may be implemented against.

| Platform | Intended direction | Open points |
|---|---|---|
| Windows with Nvidia | `windows_exporter` (Prometheus community) in place of node_exporter, plus `nvidia_gpu_exporter` unchanged. Same loopback-only rule. The normalised report stays the same; only the agent's metric-to-field mapping changes. node_exporter is Linux-only. | Metric names, collector selection, how to get memory available, interface list and link speed are not mapped. |
| AMD or Intel graphics on ordinary Linux PCs | Mostly follows the PS5 path: node_exporter `drm` and `hwmon`. | Discrete AMD cards (see 5.3), Intel (the drm collector is AMD-oriented; whether Intel series exist was not checked), and cards with a large VRAM carve-out are not verified. |

## 10. Open items

Questions still open. Answered questions from draft 1 are removed.

1. (Testing) Does llama.cpp report a PCI address per device? If yes, matching in 5.6 uses `pci_address` instead of vendor and position. To be confirmed in testing.
2. (Owner: the PS5-side team, during phase 4 testing) Confirm under load that `nvidia_smi_utilization_gpu_ratio` reaches 1.0, and confirm the scale of `nvidia_smi_fan_speed_ratio` on a desktop card. The agent clamps utilisation and fan percentages to 0-100 meanwhile.
3. (Phase 4) Confirm that the same metric lines appear from inside the agent container (section 8) and record the mounts the setup script needs.

## Appendix A. Real sample lines

Machine labels: PS5 = PS5 console running Ubuntu, node_exporter 1.13.1 (earlier measurements, 2026-11-20). LAPTOP-NV = Linux laptop with RTX 5060 8 GB, nvidia_gpu_exporter 2.0.0 (earlier measurements, 2026-11-20). host machine = Linux laptop (Ubuntu 26.04, kernel 7.0.0-43) with RTX 5060 and an AMD Raphael integrated GPU, sampled again on 2026-11-20 with the command lines in 2.3. The Nvidia lines in A.2 are from the host machine run and match the LAPTOP-NV lines where both exist; the LAPTOP-NV measurements were not attributed to a host name, so they are not assumed to be the same unit.

### A.1 PS5 (node_exporter)

```
node_drm_card_info{card="card0",chip="0000:00:08_1_0000:20:00_0",memory_vendor="",power_performance_level="",unique_id="",vendor="amd"} 1
node_drm_gpu_busy_percent{card="card0"} 0
node_drm_memory_gtt_size_bytes{card="card0"} 1.5137001472e+10
node_drm_memory_gtt_used_bytes{card="card0"} 3.7711872e+07
node_drm_memory_vis_vram_size_bytes{card="card0"} 5.36870912e+08
node_drm_memory_vis_vram_used_bytes{card="card0"} 1.2120064e+07
node_drm_memory_vram_size_bytes{card="card0"} 5.36870912e+08
node_drm_memory_vram_used_bytes{card="card0"} 1.2120064e+07
node_hwmon_freq_freq_mhz{chip="0000:00:08_1_0000:20:00_0",sensor="sclk"} 1250
node_hwmon_pwm{chip="platform_ps5_fan",sensor="pwm1"} 26
node_hwmon_sensor_label{chip="0000:00:08_1_0000:20:00_0",label="edge",sensor="temp1"} 1
node_hwmon_sensor_label{chip="0000:00:08_1_0000:20:00_0",label="sclk",sensor="freq1"} 1
node_hwmon_sensor_label{chip="pci0000:00_0000:00:18_3",label="Tctl",sensor="temp1"} 1
node_hwmon_sensor_label{chip="platform_ps5_fan",label="Local1",sensor="temp2"} 1
node_network_up{device="enp64s0f2"} 1
```

Also present on the PS5 (names only, values not quoted): `node_hwmon_temp_celsius` (labels MainSoC, Local1, Local2, Local3 on `platform_ps5_fan`; Tctl; edge; one Wi-Fi sensor), `node_memory_MemAvailable_bytes`, `node_memory_MemTotal_bytes`, `node_network_receive_bytes_total`, `node_network_transmit_bytes_total`, `node_cpu_seconds_total`, `node_load1`. The PS5's `node_hwmon_chip_names` lines were not recorded.

### A.2 Nvidia exporter (LAPTOP-NV / host machine, RTX 5060)

```
# HELP nvidia_smi_clocks_current_graphics_clock_hz clocks.current.graphics [MHz]
# TYPE nvidia_smi_clocks_current_graphics_clock_hz gauge
nvidia_smi_clocks_current_graphics_clock_hz{uuid="11111111-2222-4333-8444-555555555555"} 1.8e+08
nvidia_smi_clocks_current_memory_clock_hz{uuid="11111111-2222-4333-8444-555555555555"} 4.05e+08
nvidia_smi_gpu_info{compute_cap="12.0",cuda_version="13.2",driver_model_current="[N/A]",driver_model_pending="[N/A]",driver_version="595.99.02",index="0",name="NVIDIA GeForce RTX 5060 Laptop GPU",pci_bus_id="00000000:01:00.0",pci_sub_device_id="0x8E35103C",serial="0",uuid="11111111-2222-4333-8444-555555555555",vbios_version="98.06.2A.40.D9"} 1
# HELP nvidia_smi_memory_free_bytes memory.free [MiB]
# TYPE nvidia_smi_memory_free_bytes gauge
nvidia_smi_memory_free_bytes{uuid="11111111-2222-4333-8444-555555555555"} 8.081375232e+09
# HELP nvidia_smi_memory_reserved_bytes memory.reserved [MiB]
# TYPE nvidia_smi_memory_reserved_bytes gauge
nvidia_smi_memory_reserved_bytes{uuid="11111111-2222-4333-8444-555555555555"} 4.65567744e+08
nvidia_smi_memory_total_bytes{uuid="11111111-2222-4333-8444-555555555555"} 8.546942976e+09
nvidia_smi_memory_used_bytes{uuid="11111111-2222-4333-8444-555555555555"} 2.097152e+06
# HELP nvidia_smi_power_draw_watts power.draw [W]
# TYPE nvidia_smi_power_draw_watts gauge
nvidia_smi_power_draw_watts{uuid="11111111-2222-4333-8444-555555555555"} 11.27
nvidia_smi_temperature_gpu{uuid="11111111-2222-4333-8444-555555555555"} 52
# HELP nvidia_smi_utilization_gpu_ratio utilization.gpu [%]
# TYPE nvidia_smi_utilization_gpu_ratio gauge
nvidia_smi_utilization_gpu_ratio{uuid="11111111-2222-4333-8444-555555555555"} 0
nvidia_gpu_exporter_build_info{branch="HEAD",goarch="amd64",goos="linux",goversion="go1.27.4",revision="e1f17c9567c2ee570e72e125fdd10dbcaf31b93e",tags="fips140v1.0",version="2.0.0"} 1
nvidia_smi_command_exit_code 0
nvidia_smi_failed_scrapes_total 0
```

`nvidia_smi_fan_speed_ratio` was absent. The exporter also emits `nvidia_smi_pstate`, `nvidia_smi_temperature_gpu_tlimit`, `nvidia_smi_utilization_memory_ratio` and many others that this contract does not use.

### A.3 host machine: node_exporter next to the Nvidia card (author's samples)

AMD iGPU through drm (the Nvidia card is not listed by drm):

```
# HELP node_drm_card_info Card information
# TYPE node_drm_card_info gauge
node_drm_card_info{card="card2",chip="0000:00:08_1_0000:06:00_0",memory_vendor="",power_performance_level="auto",unique_id="",vendor="amd"} 1
node_drm_gpu_busy_percent{card="card2"} 0
node_drm_memory_gtt_size_bytes{card="card2"} 1.604974592e+10
node_drm_memory_gtt_used_bytes{card="card2"} 1.4143488e+07
node_drm_memory_vram_size_bytes{card="card2"} 5.36870912e+08
node_drm_memory_vram_used_bytes{card="card2"} 2.4932352e+07
node_drm_memory_vis_vram_size_bytes{card="card2"} 5.36870912e+08
node_hwmon_freq_freq_mhz{chip="0000:00:08_1_0000:06:00_0",sensor="sclk"} 600
node_hwmon_power_watt{chip="0000:00:08_1_0000:06:00_0",sensor="power1"} 19.814
node_hwmon_sensor_label{chip="0000:00:08_1_0000:06:00_0",label="PPT",sensor="power1"} 1
node_hwmon_sensor_label{chip="0000:00:08_1_0000:06:00_0",label="edge",sensor="temp1"} 1
node_hwmon_temp_celsius{chip="0000:00:08_1_0000:06:00_0",sensor="temp1"} 58
```

Chip names (label layout: `chip` is the hwmon chip label used by every other `node_hwmon_*` series, `chip_name` is the Linux driver name, value is always 1):

```
# HELP node_hwmon_chip_names Annotation metric for human-readable chip names
# TYPE node_hwmon_chip_names gauge
node_hwmon_chip_names{chip="0000:00:08_1_0000:06:00_0",chip_name="amdgpu"} 1
node_hwmon_chip_names{chip="i2c_1_1_0050",chip_name="spd5118"} 1
node_hwmon_chip_names{chip="ieee80211_phy0",chip_name="mt7921_phy0"} 1
node_hwmon_chip_names{chip="nvme_nvme0",chip_name="nvme"} 1
node_hwmon_chip_names{chip="pci0000:00_0000:00:18_3",chip_name="k10temp"} 1
node_hwmon_chip_names{chip="platform_hp_wmi",chip_name="hp"} 1
node_hwmon_chip_names{chip="thermal_thermal_zone0",chip_name="acpitz"} 1
```

Other hwmon and system lines:

```
# HELP node_hwmon_temp_celsius Hardware monitor for temperature (input)
node_hwmon_temp_celsius{chip="i2c_1_1_0050",sensor="temp1"} 0
node_hwmon_temp_celsius{chip="ieee80211_phy0",sensor="temp1"} 46
node_hwmon_temp_celsius{chip="nvme_nvme0",sensor="temp2"} 47.85
node_hwmon_temp_celsius{chip="pci0000:00_0000:00:18_3",sensor="temp1"} 64.5
node_hwmon_temp_celsius{chip="thermal_thermal_zone0",sensor="temp2"} 64.5
node_hwmon_sensor_label{chip="pci0000:00_0000:00:18_3",label="Tctl",sensor="temp1"} 1
node_hwmon_pwm{chip="platform_hp_wmi",sensor="pwm1"} 0
node_hwmon_fan_rpm{chip="platform_hp_wmi",sensor="fan1"} 0
# HELP node_cpu_seconds_total Seconds the CPUs spent in each mode.
# TYPE node_cpu_seconds_total counter
node_cpu_seconds_total{cpu="0",mode="idle"} 19807.53
node_cpu_seconds_total{cpu="0",mode="iowait"} 71.9
node_cpu_seconds_total{cpu="0",mode="irq"} 0
node_cpu_seconds_total{cpu="0",mode="nice"} 1
node_cpu_seconds_total{cpu="0",mode="softirq"} 45.81
node_cpu_seconds_total{cpu="0",mode="steal"} 0
# HELP node_memory_MemTotal_bytes Memory information field MemTotal_bytes.
node_memory_MemTotal_bytes 3.2099495936e+10
node_memory_MemAvailable_bytes 2.7607855104e+10
# HELP node_load1 1m load average.
node_load1 1.69
node_load5 1.09
node_load15 1.21
# HELP node_network_up Value is 1 if operstate is 'up', 0 otherwise.
node_network_up{device="eno1"} 0
node_network_up{device="enx6c1ff7c349ea"} 1
# HELP node_network_receive_bytes_total Network device statistic receive_bytes.
# TYPE node_network_receive_bytes_total counter
node_network_receive_bytes_total{device="enx6c1ff7c349ea"} 2.2462712753e+10
node_network_transmit_bytes_total{device="enx6c1ff7c349ea"} 8.80881274607e+11
# HELP node_network_speed_bytes Network device property: speed_bytes
node_network_speed_bytes{device="eno1"} -125000
node_network_speed_bytes{device="enx6c1ff7c349ea"} 6.25e+08
```

The laptop has 256 `node_cpu_seconds_total` series (32 logical CPUs times 8 modes). The sample counter values above are from the first author run; the speed and counters were not re-checked in the second run.

### A.4 Scrape timing (host machine, author's samples, three runs each)

```
node_exporter     0.956 s, 0.945 s, 0.898 s   154 KB   (RSS 21244 kB)
nvidia_gpu_exporter 0.111 s, 0.113 s, 0.112 s   28 KB   (RSS 17352 kB)
per collector: hwmon 1.268 s, netclass 0.029 s, drm 0.005 s, cpu 0.004 s, netdev 0.003 s, meminfo 0.0014 s, loadavg 0.0009 s
listeners: 127.0.0.1:61221 (node_exporter), 127.0.0.1:61222 (nvidia_gpu_exporter)
```

## Appendix B. JSON examples (Proposed shape; values are illustrative, derived from the measured lines)

### B.1 PS5

Memory figures use the measured sizes: total = 536,870,912 + 15,137,001,472 = 15,673,872,384; used during the sample = 12,120,064 + 37,711,872 = 49,831,936. Values not in Appendix A (CPU, load, memory totals, network rates, `age_ms`, `scrape_ms`, temperature sensor numbers and values other than the GPU's, and every `chip_name` on the PS5) are illustrative, not measured.

```json
{
  "stats_version": 1,
  "sources": {
    "node": { "status": "ok", "age_ms": 640, "scrape_ms": 34 }
  },
  "cpu": { "util_percent": 3.1, "logical_cpus": 8 },
  "memory": { "total_bytes": 16000000000, "available_bytes": 12000000000 },
  "load": { "load1": 0.12 },
  "net": [
    { "name": "enp64s0f2", "primary": true, "up": true, "rx_mbps": 0.02, "tx_mbps": 0.01 }
  ],
  "temps": [
    { "id": "platform_ps5_fan/temp1", "label": "MainSoC", "celsius": 51.0 },
    { "id": "pci0000:00_0000:00:18_3/temp1", "label": "Tctl", "chip_name": "k10temp", "celsius": 50.5 },
    { "id": "0000:00:08_1_0000:20:00_0/temp1", "label": "edge", "chip_name": "amdgpu", "celsius": 47.0 }
  ],
  "headline_temp_c": 51.0,
  "fans": [
    { "id": "platform_ps5_fan/pwm1", "duty_percent": 10.2 }
  ],
  "gpus": [
    {
      "id": "pci:0000:20:00.0",
      "vendor": "amd",
      "pci_address": "0000:20:00.0",
      "kind": "integrated",
      "util_percent": 0,
      "mem_total_bytes": 15673872384,
      "mem_used_bytes": 49831936,
      "mem_source": "drm_vram_gtt",
      "temp_c": 47.0,
      "clock_mhz": 1250
    }
  ]
}
```

Note: there is no `nvidia` key in `sources`. `duty_percent` 10.2 is 26 / 255 * 100. `link_mbps`, `rpm`, `name`, `index`, `mem_reserved_bytes`, `fan_percent` and `power_w` are omitted because the PS5 does not provide them (section 6).

### B.2 Linux with Nvidia (and an AMD integrated GPU), from the host machine samples

Memory: Nvidia total 8,546,942,976, used 2,097,152, reserved 465,567,744. AMD: total 536,870,912 + 16,049,745,920 = 16,586,616,832; used 24,932,352 + 14,143,488 = 39,075,840. The headline is 64.5 (CPU `k10temp` and `acpitz` both 64.5); the Wi-Fi and nvme sensors are excluded by `chip_name`, and the GPUs at 52 and 58 are lower. `age_ms` and `scrape_ms` are illustrative (the `scrape_ms` values are the measured 0.95 s and 0.11 s rounded).

```json
{
  "stats_version": 1,
  "sources": {
    "node": { "status": "ok", "age_ms": 910, "scrape_ms": 950 },
    "nvidia": { "status": "ok", "age_ms": 1150, "scrape_ms": 112 }
  },
  "cpu": { "util_percent": 6.4, "logical_cpus": 32 },
  "memory": { "total_bytes": 32099495936, "available_bytes": 27607855104 },
  "load": { "load1": 1.69, "load5": 1.09, "load15": 1.21 },
  "net": [
    { "name": "enx6c1ff7c349ea", "primary": true, "up": true, "rx_mbps": 0.4, "tx_mbps": 1.2, "link_mbps": 5000 },
    { "name": "eno1", "up": false }
  ],
  "temps": [
    { "id": "pci0000:00_0000:00:18_3/temp1", "label": "Tctl", "chip_name": "k10temp", "celsius": 64.5 },
    { "id": "thermal_thermal_zone0/temp2", "chip_name": "acpitz", "celsius": 64.5 },
    { "id": "0000:00:08_1_0000:06:00_0/temp1", "label": "edge", "chip_name": "amdgpu", "celsius": 58.0 },
    { "id": "nvme_nvme0/temp2", "label": "Sensor 1", "chip_name": "nvme", "celsius": 47.85 },
    { "id": "ieee80211_phy0/temp1", "chip_name": "mt7921_phy0", "celsius": 46.0 }
  ],
  "headline_temp_c": 64.5,
  "fans": [
    { "id": "platform_hp_wmi/pwm1", "duty_percent": 0, "rpm": 0 }
  ],
  "gpus": [
    {
      "id": "nvidia:11111111-2222-4333-8444-555555555555",
      "vendor": "nvidia",
      "name": "NVIDIA GeForce RTX 5060 Laptop GPU",
      "index": 0,
      "pci_address": "0000:01:00.0",
      "kind": "discrete",
      "util_percent": 0,
      "mem_total_bytes": 8546942976,
      "mem_used_bytes": 2097152,
      "mem_reserved_bytes": 465567744,
      "mem_source": "nvidia_smi",
      "temp_c": 52,
      "clock_mhz": 180,
      "power_w": 11.27
    },
    {
      "id": "pci:0000:06:00.0",
      "vendor": "amd",
      "pci_address": "0000:06:00.0",
      "kind": "integrated",
      "util_percent": 0,
      "mem_total_bytes": 16586616832,
      "mem_used_bytes": 39075840,
      "mem_source": "drm_vram_gtt",
      "temp_c": 58,
      "clock_mhz": 600,
      "power_w": 19.814
    }
  ]
}
```

Note: the fan entry shows that rpm 0 and duty 0 are sent as real readings; `fan_percent` is omitted on the Nvidia GPU because the exporter had no fan series. The `i2c_*` memory-module sensors read 0 and were dropped (section 4.6). The `primary` flag on `enx6c1ff7c349ea` is illustrative: which interface carries the route to Potluck on that laptop is not known.
