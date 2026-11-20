package stats

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// Input is everything Normalize needs. Node and Nvidia are the latest good
// parsed scrapes of each exporter, or nil when the source is not configured
// or has never succeeded (absent). Stale sources still pass their last good
// data.
type Input struct {
	Sources map[string]agentproto.Source
	Node    Metrics
	Rates   Rates
	Nvidia  Metrics
	Primary string // name of the primary interface, may be ""
}

// Normalize builds the stats object (contract sections 4 to 6). It is a pure
// function of its input.
func Normalize(in Input) agentproto.Stats {
	s := agentproto.Stats{
		StatsVersion: agentproto.StatsVersion,
		Sources:      in.Sources,
		GPUs:         []agentproto.GPUStat{},
	}
	if s.Sources == nil {
		s.Sources = map[string]agentproto.Source{}
	}
	var gpus []agentproto.GPUStat
	var nvPCI map[string]bool
	var drmChips map[string]string
	var node *nodeView
	if in.Nvidia != nil {
		var nv []agentproto.GPUStat
		nv, nvPCI = nvidiaGPUs(in.Nvidia)
		gpus = append(gpus, nv...)
	}
	if in.Node != nil {
		n := newNodeView(in.Node)
		node = n
		s.CPU = n.cpu(in.Rates)
		s.Memory = n.memory()
		s.Load = n.load()
		s.Net = n.net(in.Rates, in.Primary)
		s.Fans = n.fans()
		var drm []agentproto.GPUStat
		drm, drmChips = n.drmGPUs()
		for _, g := range drm {
			// 5.4 rule 2: a drm card at the PCI address of an Nvidia entry
			// is dropped; the Nvidia entry is kept.
			if g.PCIAddress != "" && nvPCI[g.PCIAddress] {
				continue
			}
			gpus = append(gpus, g)
		}
	}
	sort.SliceStable(gpus, func(i, j int) bool { return gpus[i].ID < gpus[j].ID })
	if len(gpus) > agentproto.MaxGPUs {
		gpus = gpus[:agentproto.MaxGPUs]
	}
	if gpus != nil {
		s.GPUs = gpus
	}
	if node != nil {
		// 4.6: a GPU chip is the hwmon chip of a drm card actually in gpus.
		gpuChips := map[string]bool{}
		for _, g := range s.GPUs {
			if c, ok := drmChips[g.ID]; ok && c != "" {
				gpuChips[c] = true
			}
		}
		s.Temps = node.temps(gpuChips)
	}
	s.HeadlineTempC = headline(s.Temps, s.GPUs)
	return s
}

func f64(v float64) *float64 { return &v }

func roundInt(v float64) int64 { return int64(math.Round(v)) }

// nodeView indexes one node_exporter scrape.
type nodeView struct {
	m          Metrics
	sensorLbl  map[string]string // chip\x00sensor -> label
	chipNames  map[string]string // chip -> chip_name
	tempByChip map[string][]Sample
}

func chipSensor(chip, sensor string) string { return chip + "\x00" + sensor }

func newNodeView(m Metrics) *nodeView {
	n := &nodeView{m: m, sensorLbl: map[string]string{}, chipNames: map[string]string{}, tempByChip: map[string][]Sample{}}
	for _, s := range m["node_hwmon_sensor_label"] {
		k := chipSensor(s.Labels["chip"], s.Labels["sensor"])
		if _, dup := n.sensorLbl[k]; !dup {
			n.sensorLbl[k] = s.Labels["label"]
		}
	}
	for _, s := range m["node_hwmon_chip_names"] {
		c := s.Labels["chip"]
		if _, dup := n.chipNames[c]; !dup {
			n.chipNames[c] = s.Labels["chip_name"]
		}
	}
	for _, s := range m["node_hwmon_temp_celsius"] {
		c := s.Labels["chip"]
		n.tempByChip[c] = append(n.tempByChip[c], s)
	}
	return n
}

// single returns the value of an unlabelled gauge.
func (n *nodeView) single(name string) (float64, bool) {
	ss := n.m[name]
	if len(ss) == 0 {
		return 0, false
	}
	return ss[0].Value, true
}

// byLabel returns the first sample of name whose label key equals val.
func byLabel(m Metrics, name, key, val string) (float64, bool) {
	for _, s := range m[name] {
		if s.Labels[key] == val {
			return s.Value, true
		}
	}
	return 0, false
}

// 4.2
func (n *nodeView) cpu(r Rates) *agentproto.CPU {
	c := &agentproto.CPU{UtilPercent: r.CPUUtil}
	cpus := map[string]bool{}
	for _, s := range n.m["node_cpu_seconds_total"] {
		if v, ok := s.Labels["cpu"]; ok {
			cpus[v] = true
		}
	}
	if len(cpus) > 0 {
		k := len(cpus)
		c.LogicalCPUs = &k
	}
	return c
}

// 4.3: both fields required, otherwise the object is not sent.
func (n *nodeView) memory() *agentproto.Memory {
	total, ok1 := n.single("node_memory_MemTotal_bytes")
	avail, ok2 := n.single("node_memory_MemAvailable_bytes")
	if !ok1 || !ok2 {
		return nil
	}
	return &agentproto.Memory{TotalBytes: roundInt(total), AvailableBytes: roundInt(avail)}
}

// 4.4: load1 required; load5/load15 optional.
func (n *nodeView) load() *agentproto.Load {
	l1, ok := n.single("node_load1")
	if !ok {
		return nil
	}
	l := &agentproto.Load{Load1: l1}
	if v, ok := n.single("node_load5"); ok {
		l.Load5 = f64(v)
	}
	if v, ok := n.single("node_load15"); ok {
		l.Load15 = f64(v)
	}
	return l
}

var netDenyPrefixes = []string{"veth", "docker", "br-", "virbr", "cilium", "lxc", "flannel", "cni", "tailscale", "tun", "tap"}

func netDenied(dev string) bool {
	if dev == "lo" {
		return true
	}
	for _, p := range netDenyPrefixes {
		if strings.HasPrefix(dev, p) {
			return true
		}
	}
	return false
}

// 4.5
func (n *nodeView) net(r Rates, primary string) []agentproto.NetIf {
	up := map[string]bool{}
	var devs []string
	for _, s := range n.m["node_network_up"] {
		d, ok := s.Labels["device"]
		if !ok {
			continue
		}
		if _, dup := up[d]; !dup {
			devs = append(devs, d)
		}
		up[d] = s.Value == 1
	}
	rx := readByDevice(n.m, "node_network_receive_bytes_total")
	tx := readByDevice(n.m, "node_network_transmit_bytes_total")
	speed := readByDevice(n.m, "node_network_speed_bytes")

	build := func(d string) agentproto.NetIf {
		e := agentproto.NetIf{Name: d, Up: up[d], Primary: d == primary}
		if nr, ok := r.Net[d]; ok {
			e.RxMbps, e.TxMbps = nr.Rx, nr.Tx
		}
		if v, ok := speed[d]; ok && v > 0 {
			e.LinkMbps = f64(v * 8 / 1e6)
		}
		return e
	}

	out := []agentproto.NetIf{}
	// 4.5 (draft 3): an interface with no node_network_up series is not
	// sent, even the primary one; then no entry is marked primary (devs and
	// rest come from node_network_up only, and rest never holds primary).
	if _, ok := up[primary]; ok && primary != "" {
		out = append(out, build(primary))
	}
	var rest []string
	for _, d := range devs {
		if d != primary && !netDenied(d) {
			rest = append(rest, d)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool {
		a, b := rx[rest[i]]+tx[rest[i]], rx[rest[j]]+tx[rest[j]]
		if a != b {
			return a > b
		}
		return rest[i] < rest[j]
	})
	for _, d := range rest {
		if len(out) >= agentproto.MaxNet {
			break
		}
		out = append(out, build(d))
	}
	return out
}

func validTemp(v float64) bool { return v > 0 && v < 125 }

// cpuChipNames and cpuLabelPrefixes recognise a CPU chip (4.6).
var (
	cpuChipNames     = map[string]bool{"k10temp": true, "coretemp": true, "zenpower": true}
	cpuLabelPrefixes = []string{"Tctl", "Tdie", "Package"}
)

func isCPUSensor(chipName, label string) bool {
	if cpuChipNames[chipName] {
		return true
	}
	for _, p := range cpuLabelPrefixes {
		if strings.HasPrefix(label, p) {
			return true
		}
	}
	return false
}

// temps builds temps[] (4.6, draft 3). Valid sensors only (0 < v < 125).
// The cap of 16 keeps, in this order: the sensors on GPU chips (the hwmon
// chip of a drm card in gpus, given as gpuChips) and on CPU chips (chip_name
// k10temp, coretemp or zenpower, or a label starting with Tctl, Tdie or
// Package), then every other sensor; each group keeps the exporter's own
// output order. The sent list is in that same order.
func (n *nodeView) temps(gpuChips map[string]bool) []agentproto.Temp {
	var first, rest []agentproto.Temp
	for _, s := range n.m["node_hwmon_temp_celsius"] {
		if !validTemp(s.Value) {
			continue
		}
		chip, sensor := s.Labels["chip"], s.Labels["sensor"]
		t := agentproto.Temp{
			ID:       chip + "/" + sensor,
			Label:    n.sensorLbl[chipSensor(chip, sensor)],
			ChipName: n.chipNames[chip],
			Celsius:  s.Value,
		}
		if gpuChips[chip] || isCPUSensor(t.ChipName, t.Label) {
			first = append(first, t)
		} else {
			rest = append(rest, t)
		}
	}
	out := append(append([]agentproto.Temp{}, first...), rest...)
	if len(out) > agentproto.MaxTemps {
		out = out[:agentproto.MaxTemps]
	}
	return out
}

// Headline exclusion list (4.6). The table names nvme, drivetemp, iwlwifi,
// spd5118 and jc42 as names and only mt79, ath, rtw and brcm as prefixes.
// CONTRACT-QUESTION: the iwlwifi hwmon chip is usually named "iwlwifi_1" by
// the kernel, which an exact match does not catch. The table says only the
// four listed entries are prefixes, so iwlwifi is matched exactly.
var (
	headlineExcludeExact  = map[string]bool{"nvme": true, "drivetemp": true, "iwlwifi": true, "spd5118": true, "jc42": true}
	headlineExcludePrefix = []string{"mt79", "ath", "rtw", "brcm"}
)

func headlineExcluded(chipName string) bool {
	if chipName == "" {
		return false
	}
	if headlineExcludeExact[chipName] {
		return true
	}
	for _, p := range headlineExcludePrefix {
		if strings.HasPrefix(chipName, p) {
			return true
		}
	}
	return false
}

// headline is the maximum over the sent temps entries not excluded by
// chip_name plus every sent gpus[].temp_c; nil when both sets are empty.
// 4.6 (draft 3): temps is the list actually sent (after the cap of 16), so a
// sensor dropped by the cap can never be the headline.
func headline(temps []agentproto.Temp, gpus []agentproto.GPUStat) *float64 {
	var best *float64
	consider := func(v float64) {
		if best == nil || v > *best {
			best = f64(v)
		}
	}
	for _, t := range temps {
		if !headlineExcluded(t.ChipName) {
			consider(t.Celsius)
		}
	}
	for _, g := range gpus {
		if g.TempC != nil {
			consider(*g.TempC)
		}
	}
	return best
}

// sensorNum returns the trailing number of a sensor name ("temp12" -> 12).
func sensorNum(sensor, prefix string) (int, bool) {
	if !strings.HasPrefix(sensor, prefix) {
		return 0, false
	}
	v, err := strconv.Atoi(sensor[len(prefix):])
	return v, err == nil
}

// lowestNumbered picks the sample whose sensor label has the lowest number
// after prefix; sensors without a number sort after, by name.
func lowestNumbered(ss []Sample, prefix string) (Sample, bool) {
	if len(ss) == 0 {
		return Sample{}, false
	}
	best := ss[0]
	for _, s := range ss[1:] {
		a, aok := sensorNum(s.Labels["sensor"], prefix)
		b, bok := sensorNum(best.Labels["sensor"], prefix)
		switch {
		case aok && !bok:
			best = s
		case aok && bok && a < b:
			best = s
		case !aok && !bok && s.Labels["sensor"] < best.Labels["sensor"]:
			best = s
		}
	}
	return best, true
}

// 4.7
func (n *nodeView) fans() []agentproto.Fan {
	rpm := map[string]float64{} // chip\x00fanN
	for _, s := range n.m["node_hwmon_fan_rpm"] {
		k := chipSensor(s.Labels["chip"], s.Labels["sensor"])
		if _, dup := rpm[k]; !dup {
			rpm[k] = s.Value
		}
	}
	paired := map[string]bool{}
	seen := map[string]bool{}
	out := []agentproto.Fan{}
	for _, s := range n.m["node_hwmon_pwm"] {
		chip, sensor := s.Labels["chip"], s.Labels["sensor"]
		id := chip + "/" + sensor
		if seen[id] {
			continue
		}
		seen[id] = true
		f := agentproto.Fan{ID: id, DutyPercent: f64(clamp(s.Value/255*100, 0, 100))}
		if num, ok := sensorNum(sensor, "pwm"); ok {
			k := chipSensor(chip, "fan"+strconv.Itoa(num))
			if v, ok := rpm[k]; ok {
				f.RPM = f64(v)
				paired[k] = true
			}
		}
		out = append(out, f)
	}
	for _, s := range n.m["node_hwmon_fan_rpm"] {
		chip, sensor := s.Labels["chip"], s.Labels["sensor"]
		k := chipSensor(chip, sensor)
		id := chip + "/" + sensor
		if paired[k] || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, agentproto.Fan{ID: id, RPM: f64(s.Value)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > agentproto.MaxFans {
		out = out[:agentproto.MaxFans]
	}
	return out
}

// 5.5: the final group of the drm chip label.
var drmPCIRe = regexp.MustCompile(`(\w{4}:\w{2}:\w{2})_(\d)$`)

func drmPCIAddress(chip string) (string, bool) {
	m := drmPCIRe.FindStringSubmatch(chip)
	if m == nil {
		return "", false
	}
	return strings.ToLower(m[1]) + "." + m[2], true
}

const twoGiB = 2 << 30

// 5.1 to 5.5 for drm (AMD) cards. chips maps each returned GPU id to the
// card's hwmon chip label (for the 4.6 cap order).
func (n *nodeView) drmGPUs() (out []agentproto.GPUStat, chips map[string]string) {
	chips = map[string]string{}
	seen := map[string]bool{}
	for _, info := range n.m["node_drm_card_info"] {
		// 5.3: only vendor="amd" is processed (exact, lower-case as reported).
		if info.Labels["vendor"] != "amd" {
			continue
		}
		card, chip := info.Labels["card"], info.Labels["chip"]
		if seen[card] {
			continue
		}
		seen[card] = true
		vramSize, okVS := byLabel(n.m, "node_drm_memory_vram_size_bytes", "card", card)
		vramUsed, okVU := byLabel(n.m, "node_drm_memory_vram_used_bytes", "card", card)
		gttSize, okGS := byLabel(n.m, "node_drm_memory_gtt_size_bytes", "card", card)
		gttUsed, okGU := byLabel(n.m, "node_drm_memory_gtt_used_bytes", "card", card)
		if !okVS || !okVU {
			continue // required memory fields missing: element not sent
		}
		g := agentproto.GPUStat{Vendor: "amd"}
		// 5.3 (draft 3): a card with no gtt_size series is discrete, with
		// mem_source drm_vram and the VRAM-only formula (the else branch).
		// CONTRACT-QUESTION: if the card qualifies as integrated but
		// gtt_used is missing, the required mem_used_bytes cannot be
		// computed by the integrated formula, so the element is not sent.
		if okGS && vramSize <= twoGiB && gttSize >= 8*vramSize {
			if !okGU {
				continue
			}
			g.Kind = agentproto.GPUIntegrated
			g.MemTotalBytes = roundInt(vramSize + gttSize)
			g.MemUsedBytes = roundInt(vramUsed + gttUsed)
			g.MemSource = agentproto.MemSourceDRMVRAMGTT
		} else {
			g.Kind = agentproto.GPUDiscrete
			g.MemTotalBytes = roundInt(vramSize)
			g.MemUsedBytes = roundInt(vramUsed)
			g.MemSource = agentproto.MemSourceDRMVRAM
		}
		if addr, ok := drmPCIAddress(chip); ok {
			g.PCIAddress = addr
			g.ID = "pci:" + addr
		} else {
			g.ID = "drm:" + card
		}
		if v, ok := byLabel(n.m, "node_drm_gpu_busy_percent", "card", card); ok {
			g.UtilPercent = f64(clamp(v, 0, 100))
		}
		g.TempC = n.drmTemp(chip)
		for _, s := range n.m["node_hwmon_freq_freq_mhz"] {
			if s.Labels["chip"] == chip && s.Labels["sensor"] == "sclk" {
				g.ClockMHz = f64(s.Value)
				break
			}
		}
		// CONTRACT-QUESTION: the contract names node_hwmon_power_watt{chip}
		// without a sensor. With several power sensors on the chip the agent
		// takes the lowest-numbered one (power1 on the hardware tested).
		var pw []Sample
		for _, s := range n.m["node_hwmon_power_watt"] {
			if s.Labels["chip"] == chip {
				pw = append(pw, s)
			}
		}
		if s, ok := lowestNumbered(pw, "power"); ok {
			g.PowerW = f64(s.Value)
		}
		chips[g.ID] = chip
		out = append(out, g)
	}
	return out, chips
}

// drmTemp: the sensor labelled "edge" on the card's chip, else the
// lowest-numbered temp sensor on that chip.
// CONTRACT-QUESTION: the 4.6 validity rule (drop <= 0 or >= 125) is written
// for temps[]; the agent also applies it to the chosen GPU sensor, since a
// present field must be a real reading (section 6). An invalid edge reading
// leaves temp_c omitted rather than falling back to another sensor.
func (n *nodeView) drmTemp(chip string) *float64 {
	ss := n.tempByChip[chip]
	for _, s := range ss {
		if n.sensorLbl[chipSensor(chip, s.Labels["sensor"])] == "edge" {
			if validTemp(s.Value) {
				return f64(s.Value)
			}
			return nil
		}
	}
	if s, ok := lowestNumbered(ss, "temp"); ok && validTemp(s.Value) {
		return f64(s.Value)
	}
	return nil
}

var pciRestRe = regexp.MustCompile(`^[0-9a-f]{2}:[0-9a-f]{2}\.[0-9a-f]$`)

// nvidiaPCI lower-cases pci_bus_id and shortens the 8-digit domain to 4:
// "00000000:01:00.0" -> "0000:01:00.0".
func nvidiaPCI(id string) (string, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	dom, rest, ok := strings.Cut(id, ":")
	if !ok || !pciRestRe.MatchString(rest) || len(dom) == 0 || len(dom) > 8 {
		return "", false
	}
	d, err := strconv.ParseUint(dom, 16, 32)
	// CONTRACT-QUESTION: a domain above 0xffff (for example Intel VMD
	// domains such as 10000) cannot be written in 4 digits; pci_address is
	// omitted for such a device rather than truncated.
	if err != nil || d > 0xffff {
		return "", false
	}
	return fmt.Sprintf("%04x:%s", d, rest), true
}

// 5.1, 5.2, 5.5 for nvidia_gpu_exporter. Returns the entries and the set of
// their PCI addresses.
func nvidiaGPUs(m Metrics) ([]agentproto.GPUStat, map[string]bool) {
	var out []agentproto.GPUStat
	pcis := map[string]bool{}
	seen := map[string]bool{}
	for _, info := range m["nvidia_smi_gpu_info"] {
		uuid := info.Labels["uuid"]
		if uuid == "" || seen[uuid] {
			continue
		}
		seen[uuid] = true
		total, ok1 := byLabel(m, "nvidia_smi_memory_total_bytes", "uuid", uuid)
		used, ok2 := byLabel(m, "nvidia_smi_memory_used_bytes", "uuid", uuid)
		if !ok1 || !ok2 {
			continue // required fields missing: element not sent
		}
		g := agentproto.GPUStat{
			ID:            "nvidia:" + uuid,
			Vendor:        "nvidia",
			Name:          info.Labels["name"],
			Kind:          agentproto.GPUDiscrete,
			MemTotalBytes: roundInt(total),
			MemUsedBytes:  roundInt(used),
			MemSource:     agentproto.MemSourceNvidiaSMI,
		}
		if v, err := strconv.Atoi(info.Labels["index"]); err == nil {
			g.Index = &v
		}
		if addr, ok := nvidiaPCI(info.Labels["pci_bus_id"]); ok {
			g.PCIAddress = addr
			pcis[addr] = true
		}
		if v, ok := byLabel(m, "nvidia_smi_memory_reserved_bytes", "uuid", uuid); ok {
			r := roundInt(v)
			g.MemReservedBytes = &r
		}
		if v, ok := byLabel(m, "nvidia_smi_utilization_gpu_ratio", "uuid", uuid); ok {
			g.UtilPercent = f64(clamp(v*100, 0, 100))
		}
		if v, ok := byLabel(m, "nvidia_smi_temperature_gpu", "uuid", uuid); ok {
			g.TempC = f64(v)
		}
		if v, ok := byLabel(m, "nvidia_smi_clocks_current_graphics_clock_hz", "uuid", uuid); ok {
			g.ClockMHz = f64(v / 1e6)
		}
		if v, ok := byLabel(m, "nvidia_smi_fan_speed_ratio", "uuid", uuid); ok {
			g.FanPercent = f64(clamp(v*100, 0, 100))
		}
		if v, ok := byLabel(m, "nvidia_smi_power_draw_watts", "uuid", uuid); ok {
			g.PowerW = f64(v)
		}
		out = append(out, g)
	}
	return out, pcis
}
