package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/benchspec"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
)

// DefaultDefaults returns the built-in policy defaults (design section 6).
func DefaultDefaults() Defaults {
	return Defaults{
		TTL: 300 * time.Second, MinHold: 30 * time.Second, MaxHold: 900 * time.Second,
		DrainAfter: 60 * time.Second, QueueTimeout: 600 * time.Second,
		StallTimeout: 300 * time.Second, CommandLoadTimeout: 900 * time.Second, LoadStallTimeout: 180 * time.Second,
		ShutdownGrace: 300 * time.Second, MaxPool: 4, DeviceOverheadMiB: 600,
		MinShareMiB: 1024, CacheRAMMiB: 2048, ServerBin: "llama-server-rpc", LocalDevice: "CUDA0",
		ForcedNodeGrace: 30 * time.Second, ForcedFitCheck: ForcedFitOff,
		WorkerSilence:   30 * time.Second,
		SplitSkewMargin: 0.15, EstimateMargin: 0.05, LoadMemoryFloorMiB: 768,
	}
}

// DefaultChildLogKeep is how many child log files are kept by default.
const DefaultChildLogKeep = 200

// Defaults of the history settings (see internal/history).
const (
	DefaultHistoryMaxFileMiB   = 16
	DefaultHistoryKeepFiles    = 20
	DefaultHistoryMemoryEvents = 5000
	// MaxHistoryMemoryEvents bounds history_memory_events (a sanity limit on memory use).
	MaxHistoryMemoryEvents = 200000
)

// DefaultLocalProbeCmd is the nvidia-smi query of design 5.1.
const DefaultLocalProbeCmd = "nvidia-smi --query-gpu=memory.total,memory.used --format=csv,noheader,nounits"

// Load reads and validates the config file at path.
func Load(path string) (*Config, error) { return LoadWithOptions(path, Options{}) }

// LoadWithOptions is Load with injectable behaviour.
func LoadWithOptions(path string, o Options) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return ParseWithOptions(data, o)
}

// Parse decodes and validates config YAML.
func Parse(data []byte) (*Config, error) { return ParseWithOptions(data, Options{}) }

// ParseWithOptions is Parse with injectable behaviour.
func ParseWithOptions(data []byte, o Options) (*Config, error) {
	raw := rawConfig{
		Listen:            ":8080",
		Fleet:             rawFleet{StaleAfter: duration(60 * time.Second)},
		ModelsDir:         "/models",
		ChildLogKeep:      DefaultChildLogKeep,
		HistoryMaxFileMiB: DefaultHistoryMaxFileMiB, HistoryKeepFiles: DefaultHistoryKeepFiles, HistoryMemoryEvents: DefaultHistoryMemoryEvents,
		LocalProbeCmd: DefaultLocalProbeCmd,
		WSMaxClients:  DefaultWSMaxClients,
		Bench:         rawBench{RegressLoadPct: DefaultRegressLoadPct, RegressDecodePct: DefaultRegressDecodePct, MaxRunMinutes: DefaultMaxRunMinutes},
		Defaults:      rawDefaultsFrom(DefaultDefaults()),
		Recovery:      rawRecoveryFrom(DefaultRecovery()),
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config: %w", err)
	}
	c := &Config{
		Listen:       raw.Listen,
		Fleet:        Fleet{URL: raw.Fleet.URL, LocalNode: raw.Fleet.LocalNode, StaleAfter: time.Duration(raw.Fleet.StaleAfter)},
		ModelsDir:    raw.ModelsDir,
		ChildLogDir:  strings.TrimSpace(raw.ChildLogDir),
		ChildLogKeep: raw.ChildLogKeep,
		HistoryDir:   strings.TrimSpace(raw.HistoryDir), HistoryMaxFileMiB: raw.HistoryMaxFileMiB, HistoryKeepFiles: raw.HistoryKeepFiles,
		HistoryMemoryEvents: raw.HistoryMemoryEvents,
		LocalProbeCmd:       raw.LocalProbeCmd,
		DataDir:             strings.TrimSpace(raw.DataDir),
		WSMaxClients:        raw.WSMaxClients,
		Bench: Bench{RegressLoadPct: raw.Bench.RegressLoadPct, RegressDecodePct: raw.Bench.RegressDecodePct,
			MaxRunMinutes: raw.Bench.MaxRunMinutes, Benchmarks: raw.Bench.Benchmarks, Suites: raw.Bench.Suites},
		Defaults:      raw.Defaults.value(),
		NodeOverrides: map[string]NodeOverride{},
		ExcludeNodes:  trimAll(raw.ExcludeNodes),
		Recovery:      raw.Recovery.value(),
		Profiles:      map[string]*core.Profile{},
	}
	for _, t := range raw.Tiers {
		c.Tiers = append(c.Tiers, TierRule{
			Match: Match{Hostname: t.Match.Hostname, Role: t.Match.Role, Source: t.Match.Source, Local: t.Match.Local},
			Tier: core.Tier{Priority: t.Priority, HeadroomMiB: t.HeadroomMiB, SharedMemory: t.SharedMemory,
				LinkMBs: t.LinkMBs, Settle: time.Duration(t.Settle)},
		})
	}
	for k, v := range raw.NodeOverrides {
		c.NodeOverrides[k] = NodeOverride{RPC: v.RPC}
	}
	rawProfiles := map[string]*rawProfile{}
	for name, rp := range raw.Profiles {
		if rp == nil {
			rp = &rawProfile{}
		}
		rawProfiles[name] = rp
		c.Profiles[name] = rp.toCore(name)
	}
	exists := o.FileExists
	if exists == nil {
		exists = func(p string) bool { _, err := os.Stat(p); return err == nil }
	}
	c.Bench.Enabled = c.DataDir != "" && (raw.Bench.Enabled == nil || *raw.Bench.Enabled)
	c.benchEnabledSet = raw.Bench.Enabled
	if err := c.validate(rawProfiles, exists); err != nil {
		return nil, err
	}
	return c, nil
}

func trimAll(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.TrimSpace(s)
	}
	return out
}

// modelPath is where a profile's model file is expected.
func (c *Config) modelPath(p *core.Profile) string { return filepath.Join(c.ModelsDir, p.Model) }

type rawConfig struct {
	Listen              string                 `yaml:"listen"`
	Fleet               rawFleet               `yaml:"fleet"`
	ModelsDir           string                 `yaml:"models_dir"`
	ChildLogDir         string                 `yaml:"child_log_dir"`
	ChildLogKeep        int                    `yaml:"child_log_keep"`
	HistoryDir          string                 `yaml:"history_dir"`
	HistoryMaxFileMiB   int                    `yaml:"history_max_file_mib"`
	HistoryKeepFiles    int                    `yaml:"history_keep_files"`
	HistoryMemoryEvents int                    `yaml:"history_memory_events"`
	LocalProbeCmd       string                 `yaml:"local_probe_cmd"`
	DataDir             string                 `yaml:"data_dir"`
	WSMaxClients        int                    `yaml:"ws_max_clients"`
	Bench               rawBench               `yaml:"bench"`
	Defaults            rawDefaults            `yaml:"defaults"`
	Tiers               []rawTier              `yaml:"tiers"`
	NodeOverrides       map[string]rawOverride `yaml:"node_overrides"`
	ExcludeNodes        []string               `yaml:"exclude_nodes"`
	Recovery            rawRecovery            `yaml:"recovery"`
	Profiles            map[string]*rawProfile `yaml:"profiles"`
}

type rawFleet struct {
	URL        string   `yaml:"url"`
	LocalNode  string   `yaml:"local_node"`
	StaleAfter duration `yaml:"stale_after"`
}

type rawOverride struct {
	RPC string `yaml:"rpc"`
}

type rawTier struct {
	Match        rawMatch `yaml:"match"`
	Priority     int      `yaml:"priority"`
	HeadroomMiB  int      `yaml:"headroom_mib"`
	SharedMemory bool     `yaml:"shared_memory"`
	LinkMBs      float64  `yaml:"link_mb_s"`
	Settle       duration `yaml:"settle"`
}

type rawMatch struct {
	Hostname string `yaml:"hostname"`
	Role     string `yaml:"role"`
	Source   string `yaml:"source"`
	Local    *bool  `yaml:"local"`
}

// rawProfile uses pointers wherever "absent" must be told from zero.
type rawProfile struct {
	Kind             string    `yaml:"kind"`
	Model            string    `yaml:"model"`
	Ctx              *int      `yaml:"ctx"`
	Parallel         *int      `yaml:"parallel"`
	KVQuant          string    `yaml:"kv_quant"`
	FlashAttn        *bool     `yaml:"flash_attn"`
	NeedsMiB         int       `yaml:"needs_mib"`
	AllowTiers       []int     `yaml:"allow_tiers"`
	ForceNodes       []string  `yaml:"force_nodes"`
	TensorSplit      []int     `yaml:"tensor_split"`
	ExtraArgs        []string  `yaml:"extra_args"`
	Cmd              string    `yaml:"cmd"`
	CheckEndpoint    string    `yaml:"check_endpoint"`
	Aliases          []string  `yaml:"aliases"`
	TTL              *duration `yaml:"ttl"`
	LoadTimeout      duration  `yaml:"load_timeout"`
	StallTimeout     *duration `yaml:"stall_timeout"`
	LoadStallTimeout *duration `yaml:"load_stall_timeout"`
}

func (r *rawProfile) toCore(name string) *core.Profile {
	p := &core.Profile{
		Name: name, Kind: core.Kind(r.Kind), Model: r.Model, KVQuant: r.KVQuant,
		NeedsMiB: r.NeedsMiB, AllowTiers: r.AllowTiers, ForceNodes: r.ForceNodes,
		TensorSplit: r.TensorSplit, ExtraArgs: r.ExtraArgs, Cmd: r.Cmd,
		CheckEndpoint: r.CheckEndpoint, Aliases: r.Aliases,
		TTL: r.TTL.ptr(), LoadTimeout: time.Duration(r.LoadTimeout), StallTimeout: r.StallTimeout.ptr(),
		LoadStallTimeout: r.LoadStallTimeout.ptr(),
	}
	if p.Kind == "" {
		p.Kind = core.KindLlama
	}
	if r.Ctx != nil {
		p.Ctx = *r.Ctx
	}
	if r.FlashAttn != nil {
		p.FlashAttn = *r.FlashAttn
	}
	if p.Kind == core.KindLlama {
		p.Parallel = 1
		if r.Parallel != nil {
			p.Parallel = *r.Parallel
		}
		if p.KVQuant == "" {
			p.KVQuant = "f16"
		}
	}
	return p
}

// duration is a time.Duration written as "300s"; a bare 0 is also accepted
// because the design writes `ttl: 0`. Other bare numbers are rejected as
// ambiguous.
type duration time.Duration

func (d *duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: duration must be a scalar like 300s", n.Line)
	}
	if n.Tag == "!!int" && n.Value == "0" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q (write it like 300s, or 0)", n.Line, n.Value)
	}
	*d = duration(v)
	return nil
}

func (d *duration) ptr() *time.Duration {
	if d == nil {
		return nil
	}
	v := time.Duration(*d)
	return &v
}

type rawDefaults struct {
	TTL                duration `yaml:"ttl"`
	MinHold            duration `yaml:"min_hold"`
	MaxHold            duration `yaml:"max_hold"`
	DrainAfter         duration `yaml:"drain_after"`
	QueueTimeout       duration `yaml:"queue_timeout"`
	StallTimeout       duration `yaml:"stall_timeout"`
	CommandLoadTimeout duration `yaml:"command_load_timeout"`
	LoadStallTimeout   duration `yaml:"load_stall_timeout"`
	ShutdownGrace      duration `yaml:"shutdown_grace"`
	MaxPool            int      `yaml:"max_pool"`
	DeviceOverheadMiB  int      `yaml:"device_overhead_mib"`
	MinShareMiB        int      `yaml:"min_share_mib"`
	CacheRAMMiB        int      `yaml:"cache_ram_mib"`
	ServerBin          string   `yaml:"server_bin"`
	LocalDevice        string   `yaml:"local_device"`
	ForcedNodeGrace    duration `yaml:"forced_node_grace"`
	ForcedFitCheck     string   `yaml:"forced_fit_check"`

	WorkerProbeInterval        duration `yaml:"worker_probe_interval"`
	WorkerSilence              duration `yaml:"worker_silence"`
	WorkerProbeDuringInference bool     `yaml:"worker_probe_during_inference"`

	SplitSkewMargin    float64 `yaml:"split_skew_margin"`
	EstimateMargin     float64 `yaml:"estimate_margin"`
	LoadMemoryFloorMiB int     `yaml:"load_memory_floor_mib"`
}

func rawDefaultsFrom(d Defaults) rawDefaults {
	return rawDefaults{duration(d.TTL), duration(d.MinHold), duration(d.MaxHold), duration(d.DrainAfter),
		duration(d.QueueTimeout), duration(d.StallTimeout), duration(d.CommandLoadTimeout),
		duration(d.LoadStallTimeout), duration(d.ShutdownGrace), d.MaxPool, d.DeviceOverheadMiB, d.MinShareMiB, d.CacheRAMMiB,
		d.ServerBin, d.LocalDevice, duration(d.ForcedNodeGrace), d.ForcedFitCheck,
		duration(d.WorkerProbeInterval), duration(d.WorkerSilence), d.WorkerProbeDuringInference,
		d.SplitSkewMargin, d.EstimateMargin, d.LoadMemoryFloorMiB}
}

func (r rawDefaults) value() Defaults {
	return Defaults{time.Duration(r.TTL), time.Duration(r.MinHold), time.Duration(r.MaxHold),
		time.Duration(r.DrainAfter), time.Duration(r.QueueTimeout), time.Duration(r.StallTimeout),
		time.Duration(r.CommandLoadTimeout), time.Duration(r.LoadStallTimeout), time.Duration(r.ShutdownGrace), r.MaxPool,
		r.DeviceOverheadMiB, r.MinShareMiB, r.CacheRAMMiB, r.ServerBin, r.LocalDevice,
		time.Duration(r.ForcedNodeGrace), r.ForcedFitCheck,
		time.Duration(r.WorkerProbeInterval), time.Duration(r.WorkerSilence), r.WorkerProbeDuringInference,
		r.SplitSkewMargin, r.EstimateMargin, r.LoadMemoryFloorMiB}
}

type rawRecovery struct {
	Enabled     bool     `yaml:"enabled"`
	Actions     []string `yaml:"actions"`
	MinInterval duration `yaml:"min_interval"`
	StepWait    duration `yaml:"step_wait"`
	RequestTTL  duration `yaml:"request_ttl"`
	HTTPTimeout duration `yaml:"http_timeout"`
}

func rawRecoveryFrom(r Recovery) rawRecovery {
	return rawRecovery{r.Enabled, append([]string(nil), r.Actions...), duration(r.MinInterval),
		duration(r.StepWait), duration(r.RequestTTL), duration(r.HTTPTimeout)}
}

func (r rawRecovery) value() Recovery {
	return Recovery{r.Enabled, trimAll(r.Actions), time.Duration(r.MinInterval),
		time.Duration(r.StepWait), time.Duration(r.RequestTTL), time.Duration(r.HTTPTimeout)}
}

type rawBench struct {
	Enabled          *bool                  `yaml:"enabled"`
	RegressLoadPct   float64                `yaml:"regress_load_pct"`
	RegressDecodePct float64                `yaml:"regress_decode_pct"`
	MaxRunMinutes    int                    `yaml:"max_run_minutes"`
	Benchmarks       []*benchspec.Benchmark `yaml:"benchmarks"`
	Suites           []*benchspec.Suite     `yaml:"suites"`
}
