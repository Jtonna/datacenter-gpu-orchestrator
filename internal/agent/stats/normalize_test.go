package stats

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// Appendix B.1: the PS5.
func TestNormalizePS5AppendixB1(t *testing.T) {
	node, _ := parseFile(t, "ps5_node.prom", keepNode)
	s := Normalize(Input{Sources: okSources("node"), Node: node, Primary: "enp64s0f2"})

	if s.StatsVersion != agentproto.StatsVersion {
		t.Fatalf("stats_version = %d", s.StatsVersion)
	}
	if _, ok := s.Sources["nvidia"]; ok {
		t.Fatal("nvidia key present")
	}
	if len(s.GPUs) != 1 {
		t.Fatalf("gpus = %+v", s.GPUs)
	}
	g := s.GPUs[0]
	if g.ID != "pci:0000:20:00.0" || g.Vendor != "amd" || g.PCIAddress != "0000:20:00.0" || g.Kind != "integrated" {
		t.Fatalf("gpu identity = %+v", g)
	}
	if g.MemTotalBytes != 15673872384 || g.MemUsedBytes != 49831936 || g.MemSource != "drm_vram_gtt" {
		t.Fatalf("gpu memory = %d / %d %s", g.MemTotalBytes, g.MemUsedBytes, g.MemSource)
	}
	wantF(t, "util", g.UtilPercent, 0)
	wantF(t, "temp_c", g.TempC, 47)
	wantF(t, "clock", g.ClockMHz, 1250)
	wantNil(t, "power", g.PowerW)
	wantNil(t, "index", g.Index)
	wantNil(t, "reserved", g.MemReservedBytes)
	wantNil(t, "fan_percent", g.FanPercent)
	if g.Name != "" {
		t.Fatalf("name = %q", g.Name)
	}

	if len(s.Fans) != 1 || s.Fans[0].ID != "platform_ps5_fan/pwm1" {
		t.Fatalf("fans = %+v", s.Fans)
	}
	wantF(t, "duty", s.Fans[0].DutyPercent, 26.0/255*100)
	wantNil(t, "rpm", s.Fans[0].RPM)

	if len(s.Temps) != 3 {
		t.Fatalf("temps = %+v", s.Temps)
	}
	wantTemps := []agentproto.Temp{
		{ID: "platform_ps5_fan/temp1", Label: "MainSoC", Celsius: 51},
		{ID: "pci0000:00_0000:00:18_3/temp1", Label: "Tctl", Celsius: 50.5},
		{ID: "0000:00:08_1_0000:20:00_0/temp1", Label: "edge", Celsius: 47},
	}
	for i, w := range wantTemps {
		if s.Temps[i] != w {
			t.Fatalf("temps[%d] = %+v, want %+v", i, s.Temps[i], w)
		}
	}
	wantF(t, "headline", s.HeadlineTempC, 51)

	if len(s.Net) != 1 || s.Net[0].Name != "enp64s0f2" || !s.Net[0].Primary || !s.Net[0].Up {
		t.Fatalf("net = %+v", s.Net)
	}
	wantNil(t, "link", s.Net[0].LinkMbps)
	wantNil(t, "rx on first scrape", s.Net[0].RxMbps)

	if s.CPU == nil || s.CPU.LogicalCPUs == nil || *s.CPU.LogicalCPUs != 8 {
		t.Fatalf("cpu = %+v", s.CPU)
	}
	wantNil(t, "cpu util on first scrape", s.CPU.UtilPercent)
	if s.Memory == nil || s.Memory.TotalBytes != 16000000000 || s.Memory.AvailableBytes != 12000000000 {
		t.Fatalf("memory = %+v", s.Memory)
	}
	if s.Load == nil || s.Load.Load1 != 0.12 || s.Load.Load5 != nil || s.Load.Load15 != nil {
		t.Fatalf("load = %+v", s.Load)
	}
}

func normalizeB2(t *testing.T) agentproto.Stats {
	node, _ := parseFile(t, "host_node.prom", keepNode)
	nv, _ := parseFile(t, "host_nvidia.prom", keepNvidia)
	return Normalize(Input{Sources: okSources("node", "nvidia"), Node: node, Nvidia: nv, Primary: "enx020000000010"})
}

// Appendix B.2: Linux laptop with an Nvidia card and an AMD iGPU.
func TestNormalizeHostAppendixB2(t *testing.T) {
	s := normalizeB2(t)

	if len(s.GPUs) != 2 {
		t.Fatalf("gpus = %+v", s.GPUs)
	}
	n := s.GPUs[0]
	if n.ID != "nvidia:11111111-2222-4333-8444-555555555555" || n.Vendor != "nvidia" ||
		n.Name != "NVIDIA GeForce RTX 5060 Laptop GPU" || n.PCIAddress != "0000:01:00.0" || n.Kind != "discrete" {
		t.Fatalf("nvidia identity = %+v", n)
	}
	if n.Index == nil || *n.Index != 0 {
		t.Fatalf("index = %v", n.Index)
	}
	if n.MemTotalBytes != 8546942976 || n.MemUsedBytes != 2097152 || n.MemSource != "nvidia_smi" {
		t.Fatalf("nvidia memory = %+v", n)
	}
	if n.MemReservedBytes == nil || *n.MemReservedBytes != 465567744 {
		t.Fatalf("reserved = %v", n.MemReservedBytes)
	}
	wantF(t, "nv util", n.UtilPercent, 0)
	wantF(t, "nv temp", n.TempC, 52)
	wantF(t, "nv clock", n.ClockMHz, 180)
	wantF(t, "nv power", n.PowerW, 11.27)
	wantNil(t, "nv fan", n.FanPercent)

	a := s.GPUs[1]
	if a.ID != "pci:0000:06:00.0" || a.Vendor != "amd" || a.PCIAddress != "0000:06:00.0" || a.Kind != "integrated" {
		t.Fatalf("amd identity = %+v", a)
	}
	if a.MemTotalBytes != 16586616832 || a.MemUsedBytes != 39075840 || a.MemSource != "drm_vram_gtt" {
		t.Fatalf("amd memory = %+v", a)
	}
	wantF(t, "amd util", a.UtilPercent, 0)
	wantF(t, "amd temp", a.TempC, 58)
	wantF(t, "amd clock", a.ClockMHz, 600)
	wantF(t, "amd power", a.PowerW, 19.814)
	wantNil(t, "amd index", a.Index)

	// Headline 64.5: nvme (47.85) and mt7921_phy0 (46) excluded, spd5118 (0) dropped.
	wantF(t, "headline", s.HeadlineTempC, 64.5)
	wantIDs := []string{
		"pci0000:00_0000:00:18_3/temp1",
		"thermal_thermal_zone0/temp2",
		"0000:00:08_1_0000:06:00_0/temp1",
		"nvme_nvme0/temp2",
		"ieee80211_phy0/temp1",
	}
	if len(s.Temps) != len(wantIDs) {
		t.Fatalf("temps = %+v", s.Temps)
	}
	for i, id := range wantIDs {
		if s.Temps[i].ID != id {
			t.Fatalf("temps[%d] = %+v, want id %s", i, s.Temps[i], id)
		}
	}
	if s.Temps[0].Label != "Tctl" || s.Temps[0].ChipName != "k10temp" || s.Temps[1].Label != "" || s.Temps[1].ChipName != "acpitz" {
		t.Fatalf("temp labels = %+v", s.Temps[:2])
	}

	// Net: primary first with link 5000; eno1 down with link omitted (-125000).
	if len(s.Net) != 2 || s.Net[0].Name != "enx020000000010" || !s.Net[0].Primary || !s.Net[0].Up {
		t.Fatalf("net = %+v", s.Net)
	}
	wantF(t, "link", s.Net[0].LinkMbps, 5000)
	if s.Net[1].Name != "eno1" || s.Net[1].Primary || s.Net[1].Up {
		t.Fatalf("net[1] = %+v", s.Net[1])
	}
	wantNil(t, "eno1 link", s.Net[1].LinkMbps)

	if len(s.Fans) != 1 || s.Fans[0].ID != "platform_hp_wmi/pwm1" {
		t.Fatalf("fans = %+v", s.Fans)
	}
	wantF(t, "duty 0", s.Fans[0].DutyPercent, 0)
	wantF(t, "rpm 0", s.Fans[0].RPM, 0)

	if s.Load == nil || s.Load.Load1 != 1.69 {
		t.Fatalf("load = %+v", s.Load)
	}
	wantF(t, "load5", s.Load.Load5, 1.09)
	wantF(t, "load15", s.Load.Load15, 1.21)
	if s.Memory == nil || s.Memory.TotalBytes != 32099495936 || s.Memory.AvailableBytes != 27607855104 {
		t.Fatalf("memory = %+v", s.Memory)
	}
	if s.CPU == nil || s.CPU.LogicalCPUs == nil || *s.CPU.LogicalCPUs != 1 {
		t.Fatalf("cpu = %+v", s.CPU)
	}
}

func TestJSONEncodingB2(t *testing.T) {
	s := normalizeB2(t)
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	top := assertNoNull(t, raw)
	gpus := top["gpus"].([]any)
	nv := gpus[0].(map[string]any)
	if _, ok := nv["fan_percent"]; ok {
		t.Fatal("fan_percent sent")
	}
	if v, ok := nv["util_percent"]; !ok || v.(float64) != 0 {
		t.Fatalf("util_percent 0 must be present: %v", nv)
	}
	if v := nv["index"]; v != float64(0) {
		t.Fatalf("index = %v", v)
	}
	amd := gpus[1].(map[string]any)
	for _, k := range []string{"name", "index", "mem_reserved_bytes", "fan_percent"} {
		if _, ok := amd[k]; ok {
			t.Fatalf("amd entry has %s", k)
		}
	}
	net := top["net"].([]any)
	eno := net[1].(map[string]any)
	for _, k := range []string{"primary", "link_mbps", "rx_mbps", "tx_mbps"} {
		if _, ok := eno[k]; ok {
			t.Fatalf("eno1 has %s", k)
		}
	}
	if eno["up"] != false {
		t.Fatalf("eno1 up = %v", eno["up"])
	}
	fan := top["fans"].([]any)[0].(map[string]any)
	if fan["duty_percent"] != float64(0) || fan["rpm"] != float64(0) {
		t.Fatalf("fan = %v", fan)
	}
	cpu := top["cpu"].(map[string]any)
	if _, ok := cpu["util_percent"]; ok {
		t.Fatal("cpu util sent on first scrape")
	}
	if !strings.Contains(string(raw), `"mem_total_bytes":16586616832`) {
		t.Fatalf("integer encoding: %s", raw)
	}
}

func TestJSONAbsentSource(t *testing.T) {
	s := Normalize(Input{Sources: map[string]agentproto.Source{"node": {Status: agentproto.SourceAbsent}}})
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	top := assertNoNull(t, raw)
	for _, k := range []string{"cpu", "memory", "load", "net", "temps", "fans", "headline_temp_c"} {
		if _, ok := top[k]; ok {
			t.Fatalf("%s present for absent node: %s", k, raw)
		}
	}
	src := top["sources"].(map[string]any)["node"].(map[string]any)
	if len(src) != 1 || src["status"] != "absent" {
		t.Fatalf("absent source = %v", src)
	}
	if g, ok := top["gpus"].([]any); !ok || len(g) != 0 {
		t.Fatalf("gpus = %v", top["gpus"])
	}
}

func TestHeadlineExclusionList(t *testing.T) {
	chips := []struct {
		chip, name string
		c          float64
	}{
		{"c1", "nvme", 99}, {"c2", "drivetemp", 98}, {"c3", "iwlwifi", 97},
		{"c4", "mt7921_phy0", 96}, {"c5", "ath11k_hwmon", 95}, {"c6", "rtw89", 94},
		{"c7", "brcmfmac", 93}, {"c8", "spd5118", 92}, {"c9", "jc42", 91},
		{"c10", "k10temp", 60},
	}
	var b strings.Builder
	for _, c := range chips {
		fmt.Fprintf(&b, "node_hwmon_chip_names{chip=%q,chip_name=%q} 1\n", c.chip, c.name)
		fmt.Fprintf(&b, "node_hwmon_temp_celsius{chip=%q,sensor=\"temp1\"} %v\n", c.chip, c.c)
	}
	m := parseText(t, b.String(), keepNode)
	s := Normalize(Input{Sources: okSources("node"), Node: m})
	wantF(t, "headline", s.HeadlineTempC, 60)
	if len(s.Temps) != 10 {
		t.Fatalf("excluded chips must still be in temps: %d", len(s.Temps))
	}

	// Exact names are exact: "nvme2" and "iwlwifi_1" are not on the list.
	m = parseText(t, b.String()+
		"node_hwmon_chip_names{chip=\"x\",chip_name=\"nvme2\"} 1\nnode_hwmon_temp_celsius{chip=\"x\",sensor=\"temp1\"} 70\n", keepNode)
	s = Normalize(Input{Sources: okSources("node"), Node: m})
	wantF(t, "headline with unlisted name", s.HeadlineTempC, 70)

	// No chip_name: included.
	m = parseText(t, b.String()+"node_hwmon_temp_celsius{chip=\"nochip\",sensor=\"temp1\"} 61\n", keepNode)
	s = Normalize(Input{Sources: okSources("node"), Node: m})
	wantF(t, "headline with no chip_name", s.HeadlineTempC, 61)

	// Only excluded temps and no GPUs: headline omitted.
	m = parseText(t, "node_hwmon_chip_names{chip=\"c1\",chip_name=\"nvme\"} 1\nnode_hwmon_temp_celsius{chip=\"c1\",sensor=\"temp1\"} 40\n", keepNode)
	s = Normalize(Input{Sources: okSources("node"), Node: m})
	wantNil(t, "headline", s.HeadlineTempC)

	// GPU temp alone sets the headline.
	nv, _ := parseFile(t, "host_nvidia.prom", keepNvidia)
	s = Normalize(Input{Sources: okSources("node", "nvidia"), Node: m, Nvidia: nv})
	wantF(t, "headline from gpu", s.HeadlineTempC, 52)
}

func TestTempsValidityAndCap(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&b, "node_hwmon_temp_celsius{chip=\"c\",sensor=\"temp%d\"} %d\n", i, 30+i)
	}
	b.WriteString("node_hwmon_temp_celsius{chip=\"z\",sensor=\"temp1\"} 0\n")
	b.WriteString("node_hwmon_temp_celsius{chip=\"z\",sensor=\"temp2\"} -5\n")
	b.WriteString("node_hwmon_temp_celsius{chip=\"z\",sensor=\"temp3\"} 125\n")
	b.WriteString("node_hwmon_temp_celsius{chip=\"z\",sensor=\"temp4\"} 124.9\n")
	s := Normalize(Input{Sources: okSources("node"), Node: parseText(t, b.String(), keepNode)})
	if len(s.Temps) != agentproto.MaxTemps {
		t.Fatalf("temps len = %d", len(s.Temps))
	}
	if s.Temps[0].ID != "z/temp4" || s.Temps[0].Celsius != 124.9 {
		t.Fatalf("temps[0] = %+v", s.Temps[0])
	}
	for i := 1; i < len(s.Temps); i++ {
		if s.Temps[i].Celsius > s.Temps[i-1].Celsius {
			t.Fatal("not highest first")
		}
		if s.Temps[i].Celsius <= 0 || s.Temps[i].Celsius >= 125 {
			t.Fatalf("invalid temp kept: %+v", s.Temps[i])
		}
	}
	// lowest kept: 124.9 then 49..35 (15 entries)
	if s.Temps[15].Celsius != 35 {
		t.Fatalf("last = %+v", s.Temps[15])
	}
}

func TestFansPairingRPMOnlyAndCap(t *testing.T) {
	in := `node_hwmon_pwm{chip="a",sensor="pwm1"} 300
node_hwmon_pwm{chip="a",sensor="pwm2"} 127.5
node_hwmon_fan_rpm{chip="a",sensor="fan1"} 2000
node_hwmon_fan_rpm{chip="b",sensor="fan1"} 1500
node_hwmon_fan_rpm{chip="a",sensor="fan2"} 900
`
	s := Normalize(Input{Sources: okSources("node"), Node: parseText(t, in, keepNode)})
	if len(s.Fans) != 3 {
		t.Fatalf("fans = %+v", s.Fans)
	}
	// ordered by id ascending
	if s.Fans[0].ID != "a/pwm1" || s.Fans[1].ID != "a/pwm2" || s.Fans[2].ID != "b/fan1" {
		t.Fatalf("fan ids = %+v", s.Fans)
	}
	wantF(t, "clamped duty", s.Fans[0].DutyPercent, 100)
	wantF(t, "rpm pair 1", s.Fans[0].RPM, 2000)
	wantF(t, "duty 2", s.Fans[1].DutyPercent, 50)
	wantF(t, "rpm pair 2", s.Fans[1].RPM, 900)
	wantNil(t, "rpm-only duty", s.Fans[2].DutyPercent)
	wantF(t, "rpm-only rpm", s.Fans[2].RPM, 1500)

	var b strings.Builder
	for i := 9; i >= 0; i-- {
		fmt.Fprintf(&b, "node_hwmon_pwm{chip=\"c\",sensor=\"pwm%d\"} 10\n", i)
	}
	s = Normalize(Input{Sources: okSources("node"), Node: parseText(t, b.String(), keepNode)})
	if len(s.Fans) != agentproto.MaxFans || s.Fans[0].ID != "c/pwm0" || s.Fans[7].ID != "c/pwm7" {
		t.Fatalf("fan cap = %+v", s.Fans)
	}
}

func TestNetCapPrimaryAndDenylist(t *testing.T) {
	var b strings.Builder
	// 12 ordinary interfaces with growing traffic, plus denylisted ones.
	for i := 0; i < 12; i++ {
		fmt.Fprintf(&b, "node_network_up{device=\"eth%02d\"} 1\n", i)
		fmt.Fprintf(&b, "node_network_receive_bytes_total{device=\"eth%02d\"} %d\n", i, i*1000)
		fmt.Fprintf(&b, "node_network_transmit_bytes_total{device=\"eth%02d\"} %d\n", i, i*10)
	}
	for _, d := range []string{"lo", "veth1", "docker0", "br-abc", "virbr0", "cilium_host", "lxc1", "flannel.1", "cni0", "tailscale0", "tun0", "tap0"} {
		fmt.Fprintf(&b, "node_network_up{device=%q} 1\n", d)
		fmt.Fprintf(&b, "node_network_receive_bytes_total{device=%q} 9e12\n", d)
	}
	m := parseText(t, b.String(), keepNode)

	s := Normalize(Input{Sources: okSources("node"), Node: m, Primary: "tailscale0"})
	if len(s.Net) != agentproto.MaxNet {
		t.Fatalf("net len = %d", len(s.Net))
	}
	if s.Net[0].Name != "tailscale0" || !s.Net[0].Primary {
		t.Fatalf("primary must be first even if denylisted: %+v", s.Net[0])
	}
	for i, want := range []string{"eth11", "eth10", "eth09", "eth08", "eth07", "eth06", "eth05"} {
		if s.Net[i+1].Name != want || s.Net[i+1].Primary {
			t.Fatalf("net[%d] = %+v, want %s", i+1, s.Net[i+1], want)
		}
	}

	// Primary that is an ordinary low-traffic interface still comes first.
	s = Normalize(Input{Sources: okSources("node"), Node: m, Primary: "eth00"})
	if s.Net[0].Name != "eth00" || !s.Net[0].Primary || s.Net[1].Name != "eth11" {
		t.Fatalf("net = %+v", s.Net[:2])
	}
	for _, n := range s.Net[1:] {
		if n.Name == "eth00" {
			t.Fatal("primary listed twice")
		}
	}

	// No primary known.
	s = Normalize(Input{Sources: okSources("node"), Node: m})
	for _, n := range s.Net {
		if n.Primary || !strings.HasPrefix(n.Name, "eth") {
			t.Fatalf("net = %+v", s.Net)
		}
	}
}

func drmCard(card, chip, vendor string, vram, gtt float64) string {
	return fmt.Sprintf(`node_drm_card_info{card=%q,chip=%q,vendor=%q} 1
node_drm_memory_vram_size_bytes{card=%q} %g
node_drm_memory_vram_used_bytes{card=%q} 1000
node_drm_memory_gtt_size_bytes{card=%q} %g
node_drm_memory_gtt_used_bytes{card=%q} 2000
`, card, chip, vendor, card, vram, card, card, gtt, card)
}

func TestDRMRulesKindMemoryAndFallback(t *testing.T) {
	in := drmCard("card0", "0000:00:08_1_0000:20:00_0", "amd", 512<<20, 14<<30) + // integrated
		drmCard("card1", "0000:00:01_1_0000:03:00_0", "amd", 16<<30, 32<<30) + // discrete (big VRAM)
		drmCard("card3", "0000:00:01_1_0000:04:00_0", "amd", 2<<30, 15<<30) + // discrete (gtt < 8x)
		drmCard("card4", "weird-chip", "amd", 512<<20, 14<<30) + // regex fails
		drmCard("card5", "0000:00:02_0_0000:05:00_0", "intel", 512<<20, 14<<30) + // ignored vendor
		"node_drm_card_info{card=\"card6\",chip=\"0000:00:03_0_0000:07:00_0\",vendor=\"amd\"} 1\n" + // no memory: not sent
		`node_hwmon_temp_celsius{chip="0000:00:01_1_0000:03:00_0",sensor="temp3"} 70
node_hwmon_temp_celsius{chip="0000:00:01_1_0000:03:00_0",sensor="temp2"} 65
node_hwmon_temp_celsius{chip="0000:00:01_1_0000:03:00_0",sensor="temp10"} 99
node_drm_gpu_busy_percent{card="card1"} 140
node_hwmon_power_watt{chip="0000:00:01_1_0000:03:00_0",sensor="power2"} 50
node_hwmon_power_watt{chip="0000:00:01_1_0000:03:00_0",sensor="power1"} 40
`
	s := Normalize(Input{Sources: okSources("node"), Node: parseText(t, in, keepNode)})
	ids := []string{}
	for _, g := range s.GPUs {
		ids = append(ids, g.ID)
	}
	want := []string{"drm:card4", "pci:0000:03:00.0", "pci:0000:04:00.0", "pci:0000:20:00.0"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	byID := map[string]agentproto.GPUStat{}
	for _, g := range s.GPUs {
		byID[g.ID] = g
	}
	if g := byID["drm:card4"]; g.PCIAddress != "" || g.Kind != "integrated" {
		t.Fatalf("fallback = %+v", g)
	}
	if g := byID["pci:0000:20:00.0"]; g.MemTotalBytes != 512<<20+14<<30 || g.MemUsedBytes != 3000 || g.MemSource != "drm_vram_gtt" {
		t.Fatalf("integrated = %+v", g)
	}
	g := byID["pci:0000:03:00.0"]
	if g.Kind != "discrete" || g.MemTotalBytes != 16<<30 || g.MemUsedBytes != 1000 || g.MemSource != "drm_vram" {
		t.Fatalf("discrete = %+v", g)
	}
	wantF(t, "lowest-numbered temp sensor", g.TempC, 65)
	wantF(t, "util clamped", g.UtilPercent, 100)
	wantF(t, "power from lowest-numbered sensor", g.PowerW, 40)
	wantNil(t, "util missing", byID["pci:0000:20:00.0"].UtilPercent)
	if byID["pci:0000:04:00.0"].Kind != "discrete" {
		t.Fatal("gtt < 8x vram must be discrete")
	}
}

func TestGPUCapAndDuplicateRule(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 10; i++ {
		b.WriteString(drmCard(fmt.Sprintf("card%d", i), fmt.Sprintf("0000:00:08_1_0000:%02x:00_0", 0x10+i), "amd", 512<<20, 14<<30))
	}
	// A drm card at the Nvidia card's PCI address (01:00.0).
	b.WriteString(drmCard("card99", "0000:00:01_1_0000:01:00_0", "amd", 512<<20, 14<<30))
	node := parseText(t, b.String(), keepNode)
	nv, _ := parseFile(t, "host_nvidia.prom", keepNvidia)

	s := Normalize(Input{Sources: okSources("node", "nvidia"), Node: node, Nvidia: nv})
	if len(s.GPUs) != agentproto.MaxGPUs {
		t.Fatalf("gpus = %d", len(s.GPUs))
	}
	if s.GPUs[0].Vendor != "nvidia" {
		t.Fatalf("gpus[0] = %+v", s.GPUs[0])
	}
	for i, g := range s.GPUs {
		if g.ID == "pci:0000:01:00.0" {
			t.Fatal("duplicate drm entry kept")
		}
		if i > 0 && s.GPUs[i-1].ID >= g.ID {
			t.Fatal("not ordered by id")
		}
	}
	if s.GPUs[7].ID != "pci:0000:16:00.0" {
		t.Fatalf("last = %s", s.GPUs[7].ID)
	}

	// Without nvidia data the drm card at 01:00.0 stays.
	s = Normalize(Input{Sources: map[string]agentproto.Source{"node": okSources("node")["node"], "nvidia": {Status: "absent"}}, Node: node})
	if s.GPUs[0].ID != "pci:0000:01:00.0" {
		t.Fatalf("gpus[0] = %+v", s.GPUs[0])
	}
	for _, g := range s.GPUs {
		if g.Vendor == "nvidia" {
			t.Fatal("nvidia entry while absent")
		}
	}
}

func TestNvidiaFieldRules(t *testing.T) {
	in := `nvidia_smi_gpu_info{index="1",name="X",pci_bus_id="00000000:0A:00.0",uuid="GPU-b"} 1
nvidia_smi_gpu_info{index="0",name="Y",pci_bus_id="garbage",uuid="GPU-a"} 1
nvidia_smi_gpu_info{index="z",uuid="GPU-c"} 1
nvidia_smi_memory_total_bytes{uuid="GPU-a"} 1.0e+09
nvidia_smi_memory_used_bytes{uuid="GPU-a"} 1.5
nvidia_smi_memory_total_bytes{uuid="GPU-b"} 2e9
nvidia_smi_memory_used_bytes{uuid="GPU-b"} 3e8
nvidia_smi_utilization_gpu_ratio{uuid="GPU-b"} 1.3
nvidia_smi_fan_speed_ratio{uuid="GPU-b"} 0.42
nvidia_smi_memory_total_bytes{uuid="GPU-c"} 2e9
`
	s := Normalize(Input{Sources: okSources("nvidia"), Nvidia: parseText(t, in, keepNvidia)})
	if len(s.GPUs) != 2 {
		t.Fatalf("gpus = %+v (GPU-c lacks used, must be dropped)", s.GPUs)
	}
	a, b := s.GPUs[0], s.GPUs[1]
	if a.ID != "nvidia:GPU-a" || a.PCIAddress != "" || *a.Index != 0 || a.MemUsedBytes != 2 {
		t.Fatalf("a = %+v", a)
	}
	if b.PCIAddress != "0000:0a:00.0" || *b.Index != 1 {
		t.Fatalf("b = %+v", b)
	}
	wantF(t, "util clamp", b.UtilPercent, 100)
	wantF(t, "fan", b.FanPercent, 42)
	wantNil(t, "reserved", b.MemReservedBytes)
	if s.Temps != nil || s.CPU != nil {
		t.Fatal("node objects without node data")
	}
}
