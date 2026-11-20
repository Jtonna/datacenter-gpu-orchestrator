package agentproto

import "encoding/json"

// StatsVersion is the stats contract version this package implements.
const StatsVersion = 1

// Source names in Stats.Sources.
const (
	SourceNode   = "node"   // node_exporter
	SourceNvidia = "nvidia" // nvidia_gpu_exporter
)

// Source statuses (stats contract 4.1.1).
const (
	SourceOK     = "ok"
	SourceStale  = "stale"
	SourceAbsent = "absent"
)

// GPU kinds and memory sources (stats contract 5.1).
const (
	GPUIntegrated = "integrated"
	GPUDiscrete   = "discrete"

	MemSourceDRMVRAMGTT = "drm_vram_gtt"
	MemSourceDRMVRAM    = "drm_vram"
	MemSourceNvidiaSMI  = "nvidia_smi"
)

// Caps (stats contract 4.5 to 5.1).
const (
	MaxNet   = 8
	MaxTemps = 16
	MaxFans  = 8
	MaxGPUs  = 8
)

// Stats is the normalised `stats` object (stats contract section 4).
//
// Missing data rule (section 6): an unknown optional field is omitted, never
// null, 0, -1 or "". Optional numbers are therefore pointers with omitempty.
//
// Encoding: cpu, memory, load, net, temps and fans are sent only when
// sources.node is ok or stale; then net, temps and fans are always arrays
// (possibly empty). gpus is always an array. See MarshalJSON.
type Stats struct {
	StatsVersion  int               `json:"stats_version"`
	Sources       map[string]Source `json:"sources"`
	CPU           *CPU              `json:"cpu,omitempty"`
	Memory        *Memory           `json:"memory,omitempty"`
	Load          *Load             `json:"load,omitempty"`
	Net           []NetIf           `json:"net,omitempty"`
	Temps         []Temp            `json:"temps,omitempty"`
	HeadlineTempC *float64          `json:"headline_temp_c,omitempty"`
	Fans          []Fan             `json:"fans,omitempty"`
	GPUs          []GPUStat         `json:"gpus"`
}

// Source is one entry of Stats.Sources. AgeMS and ScrapeMS are present when
// Status is ok or stale and omitted when absent.
type Source struct {
	Status   string `json:"status"`
	AgeMS    *int64 `json:"age_ms,omitempty"`
	ScrapeMS *int64 `json:"scrape_ms,omitempty"`
}

// CPU is stats.cpu (4.2).
type CPU struct {
	UtilPercent *float64 `json:"util_percent,omitempty"`
	LogicalCPUs *int     `json:"logical_cpus,omitempty"`
}

// Memory is stats.memory (4.3). For display only.
type Memory struct {
	TotalBytes     int64 `json:"total_bytes"`
	AvailableBytes int64 `json:"available_bytes"`
}

// Load is stats.load (4.4).
type Load struct {
	Load1  float64  `json:"load1"`
	Load5  *float64 `json:"load5,omitempty"`
	Load15 *float64 `json:"load15,omitempty"`
}

// NetIf is one element of stats.net (4.5).
type NetIf struct {
	Name     string   `json:"name"`
	Primary  bool     `json:"primary,omitempty"` // present only as true
	Up       bool     `json:"up"`
	RxMbps   *float64 `json:"rx_mbps,omitempty"`
	TxMbps   *float64 `json:"tx_mbps,omitempty"`
	LinkMbps *float64 `json:"link_mbps,omitempty"`
}

// Temp is one element of stats.temps (4.6).
type Temp struct {
	ID       string  `json:"id"`
	Label    string  `json:"label,omitempty"`
	ChipName string  `json:"chip_name,omitempty"`
	Celsius  float64 `json:"celsius"`
}

// Fan is one element of stats.fans (4.7).
type Fan struct {
	ID          string   `json:"id"`
	DutyPercent *float64 `json:"duty_percent,omitempty"`
	RPM         *float64 `json:"rpm,omitempty"`
}

// GPUStat is one element of stats.gpus (5.1).
type GPUStat struct {
	ID               string   `json:"id"`
	Vendor           string   `json:"vendor"`
	Name             string   `json:"name,omitempty"`
	Index            *int     `json:"index,omitempty"`
	PCIAddress       string   `json:"pci_address,omitempty"`
	Kind             string   `json:"kind"`
	UtilPercent      *float64 `json:"util_percent,omitempty"`
	MemTotalBytes    int64    `json:"mem_total_bytes"`
	MemUsedBytes     int64    `json:"mem_used_bytes"`
	MemReservedBytes *int64   `json:"mem_reserved_bytes,omitempty"`
	MemSource        string   `json:"mem_source"`
	TempC            *float64 `json:"temp_c,omitempty"`
	ClockMHz         *float64 `json:"clock_mhz,omitempty"`
	FanPercent       *float64 `json:"fan_percent,omitempty"`
	PowerW           *float64 `json:"power_w,omitempty"`
}

// NodeHasData reports whether sources.node is ok or stale, which is when the
// node-derived objects are sent.
func (s Stats) NodeHasData() bool {
	src, ok := s.Sources[SourceNode]
	return ok && (src.Status == SourceOK || src.Status == SourceStale)
}

// MarshalJSON applies the presence rules of stats contract 4.1.
func (s Stats) MarshalJSON() ([]byte, error) {
	type wire struct {
		StatsVersion  int               `json:"stats_version"`
		Sources       map[string]Source `json:"sources"`
		CPU           *CPU              `json:"cpu,omitempty"`
		Memory        *Memory           `json:"memory,omitempty"`
		Load          *Load             `json:"load,omitempty"`
		Net           *[]NetIf          `json:"net,omitempty"`
		Temps         *[]Temp           `json:"temps,omitempty"`
		HeadlineTempC *float64          `json:"headline_temp_c,omitempty"`
		Fans          *[]Fan            `json:"fans,omitempty"`
		GPUs          []GPUStat         `json:"gpus"`
	}
	w := wire{
		StatsVersion:  s.StatsVersion,
		Sources:       s.Sources,
		HeadlineTempC: s.HeadlineTempC,
		GPUs:          s.GPUs,
	}
	if w.Sources == nil {
		w.Sources = map[string]Source{}
	}
	if w.GPUs == nil {
		w.GPUs = []GPUStat{}
	}
	if s.NodeHasData() {
		w.CPU = s.CPU
		if w.CPU == nil {
			w.CPU = &CPU{}
		}
		// CONTRACT-QUESTION: memory and load are "required when node is ok or
		// stale", but section 6 also says an object whose required source
		// field is missing is not sent. When the meminfo or loadavg series are
		// missing from the scrape, the agent omits memory or load.
		w.Memory = s.Memory
		w.Load = s.Load
		net, temps, fans := s.Net, s.Temps, s.Fans
		if net == nil {
			net = []NetIf{}
		}
		if temps == nil {
			temps = []Temp{}
		}
		if fans == nil {
			fans = []Fan{}
		}
		w.Net, w.Temps, w.Fans = &net, &temps, &fans
	}
	return json.Marshal(w)
}

// MarshalJSON encodes a launch with no state as {"state":"none"} (6.3).
func (l LaunchState) MarshalJSON() ([]byte, error) {
	if l.State == StateNone || l.State == "" {
		return []byte(`{"state":"none"}`), nil
	}
	type plain LaunchState
	p := plain(l)
	if p.Command == nil {
		p.Command = []string{}
	}
	if p.Exit != nil && p.Exit.LogTail == nil {
		e := *p.Exit
		e.LogTail = []string{}
		p.Exit = &e
	}
	return json.Marshal(p)
}
