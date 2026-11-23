package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
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
		Membership: MembershipFleet, WatchdogFloorMiB: 1000, WorkerStartTimeout: 15 * time.Second,
		ReservedDriftWarnMiB: 500, FreeAverageWindow: 30 * time.Second,
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
	c, _, _, err := parseYAML(data, o)
	return c, err
}

func parseYAML(data []byte, o Options) (*Config, *rawConfig, map[string]*ProfileSpec, error) {
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
		Logs:          rawLogsFrom(DefaultLogs()),
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, nil, fmt.Errorf("config: %w", err)
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
		LocalNode:           strings.TrimSpace(raw.LocalNode),
		DataDir:             strings.TrimSpace(raw.DataDir),
		WSMaxClients:        raw.WSMaxClients,
		Bench: Bench{RegressLoadPct: raw.Bench.RegressLoadPct, RegressDecodePct: raw.Bench.RegressDecodePct,
			MaxRunMinutes: raw.Bench.MaxRunMinutes, RetentionDays: DefaultBenchRetentionDays, Benchmarks: raw.Bench.Benchmarks, Suites: raw.Bench.Suites},
		Defaults:      raw.Defaults.value(),
		NodeOverrides: map[string]NodeOverride{},
		ExcludeNodes:  trimAll(raw.ExcludeNodes),
		NoAutoNodes:   trimAll(raw.NoAutoNodes),
		Recovery:      raw.Recovery.value(),
		Logs:          raw.Logs.value(),
		Profiles:      map[string]*core.Profile{},
	}
	if raw.Bench.RetentionDays != nil {
		c.Bench.RetentionDays = *raw.Bench.RetentionDays
	}
	for _, t := range raw.Tiers {
		c.Tiers = append(c.Tiers, TierRule{
			Match: Match{Hostname: t.Match.Hostname, Role: t.Match.Role, Source: t.Match.Source, Local: t.Match.Local,
				Platform: t.Match.Platform, Device: t.Match.Device},
			Tier: core.Tier{Priority: t.Priority, HeadroomMiB: t.HeadroomMiB, SharedMemory: t.SharedMemory,
				LinkMBs: t.LinkMBs, Settle: time.Duration(t.Settle)},
		})
	}
	for k, v := range raw.NodeOverrides {
		c.NodeOverrides[k] = NodeOverride{RPC: v.RPC}
	}
	rawProfiles := map[string]*ProfileSpec{}
	for name, rp := range raw.Profiles {
		if rp == nil {
			rp = &ProfileSpec{}
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
	c.Bench.EnabledSetting = raw.Bench.Enabled == nil || *raw.Bench.Enabled
	if err := c.validate(rawProfiles, exists); err != nil {
		return nil, nil, nil, err
	}
	return c, &raw, rawProfiles, nil
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
	Listen              string                      `yaml:"listen" json:"listen,omitempty"`
	Fleet               rawFleet                    `yaml:"fleet" json:"fleet,omitempty"`
	ModelsDir           string                      `yaml:"models_dir" json:"models_dir,omitempty"`
	ChildLogDir         string                      `yaml:"child_log_dir" json:"child_log_dir,omitempty"`
	ChildLogKeep        int                         `yaml:"child_log_keep" json:"child_log_keep,omitempty"`
	HistoryDir          string                      `yaml:"history_dir" json:"history_dir,omitempty"`
	HistoryMaxFileMiB   int                         `yaml:"history_max_file_mib" json:"history_max_file_mib,omitempty"`
	HistoryKeepFiles    int                         `yaml:"history_keep_files" json:"history_keep_files,omitempty"`
	HistoryMemoryEvents int                         `yaml:"history_memory_events" json:"history_memory_events,omitempty"`
	LocalProbeCmd       string                      `yaml:"local_probe_cmd" json:"local_probe_cmd,omitempty"`
	LocalNode           string                      `yaml:"local_node" json:"local_node,omitempty"`
	DataDir             string                      `yaml:"data_dir" json:"data_dir,omitempty"`
	WSMaxClients        int                         `yaml:"ws_max_clients" json:"ws_max_clients,omitempty"`
	Bench               rawBench                    `yaml:"bench" json:"bench,omitempty"`
	Defaults            rawDefaults                 `yaml:"defaults" json:"defaults,omitempty"`
	Tiers               []TierSpec                  `yaml:"tiers" json:"tiers,omitempty"`
	NodeOverrides       map[string]NodeOverrideSpec `yaml:"node_overrides" json:"node_overrides,omitempty"`
	ExcludeNodes        []string                    `yaml:"exclude_nodes" json:"exclude_nodes,omitempty"`
	NoAutoNodes         []string                    `yaml:"no_auto_nodes" json:"no_auto_nodes,omitempty"`
	Recovery            rawRecovery                 `yaml:"recovery" json:"recovery,omitempty"`
	Logs                rawLogs                     `yaml:"logs" json:"logs,omitempty"`
	Profiles            map[string]*ProfileSpec     `yaml:"profiles" json:"profiles,omitempty"`
}

type rawFleet struct {
	URL        string   `yaml:"url" json:"url,omitempty"`
	LocalNode  string   `yaml:"local_node" json:"local_node,omitempty"`
	StaleAfter duration `yaml:"stale_after" json:"stale_after,omitempty"`
}

type NodeOverrideSpec struct {
	RPC string `yaml:"rpc" json:"rpc,omitempty"`
}

type TierSpec struct {
	Match        MatchSpec `yaml:"match" json:"match,omitempty"`
	Priority     int       `yaml:"priority" json:"priority,omitempty"`
	HeadroomMiB  int       `yaml:"headroom_mib" json:"headroom_mib,omitempty"`
	SharedMemory bool      `yaml:"shared_memory" json:"shared_memory,omitempty"`
	LinkMBs      float64   `yaml:"link_mb_s" json:"link_mb_s,omitempty"`
	Settle       duration  `yaml:"settle" json:"settle,omitempty"`
}

type MatchSpec struct {
	Hostname string `yaml:"hostname" json:"hostname,omitempty"`
	Role     string `yaml:"role" json:"role,omitempty"`
	Source   string `yaml:"source" json:"source,omitempty"`
	Local    *bool  `yaml:"local" json:"local,omitempty"`
	Platform string `yaml:"platform" json:"platform,omitempty"`
	Device   string `yaml:"device" json:"device,omitempty"`
}

// ProfileSpec uses pointers wherever "absent" must be told from zero.
type ProfileSpec struct {
	Kind             string    `yaml:"kind" json:"kind,omitempty"`
	Model            string    `yaml:"model" json:"model,omitempty"`
	Ctx              *int      `yaml:"ctx" json:"ctx,omitempty"`
	Parallel         *int      `yaml:"parallel" json:"parallel,omitempty"`
	KVQuant          string    `yaml:"kv_quant" json:"kv_quant,omitempty"`
	FlashAttn        *bool     `yaml:"flash_attn" json:"flash_attn,omitempty"`
	NeedsMiB         int       `yaml:"needs_mib" json:"needs_mib,omitempty"`
	AllowTiers       []int     `yaml:"allow_tiers" json:"allow_tiers,omitempty"`
	ForceNodes       []string  `yaml:"force_nodes" json:"force_nodes,omitempty"`
	TensorSplit      []int     `yaml:"tensor_split" json:"tensor_split,omitempty"`
	ExtraArgs        []string  `yaml:"extra_args" json:"extra_args,omitempty"`
	Cmd              string    `yaml:"cmd" json:"cmd,omitempty"`
	CheckEndpoint    string    `yaml:"check_endpoint" json:"check_endpoint,omitempty"`
	Aliases          []string  `yaml:"aliases" json:"aliases,omitempty"`
	Generated        *bool     `yaml:"generated" json:"generated,omitempty"`
	TTL              *duration `yaml:"ttl" json:"ttl,omitempty"`
	LoadTimeout      duration  `yaml:"load_timeout" json:"load_timeout,omitempty"`
	StallTimeout     *duration `yaml:"stall_timeout" json:"stall_timeout,omitempty"`
	LoadStallTimeout *duration `yaml:"load_stall_timeout" json:"load_stall_timeout,omitempty"`
}

func (r *ProfileSpec) toCore(name string) *core.Profile {
	p := &core.Profile{
		Name: name, Kind: core.Kind(r.Kind), Model: r.Model, KVQuant: r.KVQuant,
		NeedsMiB: r.NeedsMiB, AllowTiers: r.AllowTiers, ForceNodes: r.ForceNodes,
		TensorSplit: r.TensorSplit, ExtraArgs: r.ExtraArgs, Cmd: r.Cmd,
		CheckEndpoint: r.CheckEndpoint, Aliases: r.Aliases, Generated: r.Generated,
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
	TTL                duration `yaml:"ttl" json:"ttl,omitempty"`
	MinHold            duration `yaml:"min_hold" json:"min_hold,omitempty"`
	MaxHold            duration `yaml:"max_hold" json:"max_hold,omitempty"`
	DrainAfter         duration `yaml:"drain_after" json:"drain_after,omitempty"`
	QueueTimeout       duration `yaml:"queue_timeout" json:"queue_timeout,omitempty"`
	StallTimeout       duration `yaml:"stall_timeout" json:"stall_timeout,omitempty"`
	CommandLoadTimeout duration `yaml:"command_load_timeout" json:"command_load_timeout,omitempty"`
	LoadStallTimeout   duration `yaml:"load_stall_timeout" json:"load_stall_timeout,omitempty"`
	ShutdownGrace      duration `yaml:"shutdown_grace" json:"shutdown_grace,omitempty"`
	MaxPool            int      `yaml:"max_pool" json:"max_pool,omitempty"`
	DeviceOverheadMiB  int      `yaml:"device_overhead_mib" json:"device_overhead_mib,omitempty"`
	MinShareMiB        int      `yaml:"min_share_mib" json:"min_share_mib,omitempty"`
	CacheRAMMiB        int      `yaml:"cache_ram_mib" json:"cache_ram_mib,omitempty"`
	ServerBin          string   `yaml:"server_bin" json:"server_bin,omitempty"`
	LocalDevice        string   `yaml:"local_device" json:"local_device,omitempty"`
	ForcedNodeGrace    duration `yaml:"forced_node_grace" json:"forced_node_grace,omitempty"`
	ForcedFitCheck     string   `yaml:"forced_fit_check" json:"forced_fit_check,omitempty"`

	WorkerProbeInterval        duration `yaml:"worker_probe_interval" json:"worker_probe_interval,omitempty"`
	WorkerSilence              duration `yaml:"worker_silence" json:"worker_silence,omitempty"`
	WorkerProbeDuringInference bool     `yaml:"worker_probe_during_inference" json:"worker_probe_during_inference,omitempty"`

	SplitSkewMargin    float64 `yaml:"split_skew_margin" json:"split_skew_margin,omitempty"`
	EstimateMargin     float64 `yaml:"estimate_margin" json:"estimate_margin,omitempty"`
	LoadMemoryFloorMiB int     `yaml:"load_memory_floor_mib" json:"load_memory_floor_mib,omitempty"`

	Membership       string `yaml:"membership" json:"membership,omitempty"`
	WorkerModelDir   bool   `yaml:"worker_model_dir" json:"worker_model_dir,omitempty"`
	WatchdogFloorMiB int    `yaml:"watchdog_floor_mib" json:"watchdog_floor_mib,omitempty"`

	WorkerStartTimeout duration `yaml:"worker_start_timeout" json:"worker_start_timeout,omitempty"`

	FillReserveKiBPerToken int      `yaml:"fill_reserve_kib_per_token" json:"fill_reserve_kib_per_token,omitempty"`
	PlacementUsesReserved  bool     `yaml:"placement_uses_reserved" json:"placement_uses_reserved,omitempty"`
	ReservedDriftWarnMiB   int      `yaml:"reserved_drift_warn_mib" json:"reserved_drift_warn_mib,omitempty"`
	MarginMeasuredMiB      int      `yaml:"margin_measured_mib" json:"margin_measured_mib,omitempty"`
	MarginEstimatedPercent int      `yaml:"margin_estimated_percent" json:"margin_estimated_percent,omitempty"`
	MarginEstimatedMinMiB  int      `yaml:"margin_estimated_min_mib" json:"margin_estimated_min_mib,omitempty"`
	FreeAverageWindow      duration `yaml:"free_average_window" json:"free_average_window,omitempty"`
}

func rawDefaultsFrom(d Defaults) rawDefaults {
	return rawDefaults{duration(d.TTL), duration(d.MinHold), duration(d.MaxHold), duration(d.DrainAfter),
		duration(d.QueueTimeout), duration(d.StallTimeout), duration(d.CommandLoadTimeout),
		duration(d.LoadStallTimeout), duration(d.ShutdownGrace), d.MaxPool, d.DeviceOverheadMiB, d.MinShareMiB, d.CacheRAMMiB,
		d.ServerBin, d.LocalDevice, duration(d.ForcedNodeGrace), d.ForcedFitCheck,
		duration(d.WorkerProbeInterval), duration(d.WorkerSilence), d.WorkerProbeDuringInference,
		d.SplitSkewMargin, d.EstimateMargin, d.LoadMemoryFloorMiB,
		d.Membership, d.WorkerModelDir, d.WatchdogFloorMiB, duration(d.WorkerStartTimeout),
		d.FillReserveKiBPerToken, d.PlacementUsesReserved, d.ReservedDriftWarnMiB, d.MarginMeasuredMiB,
		d.MarginEstimatedPercent, d.MarginEstimatedMinMiB, duration(d.FreeAverageWindow)}
}

func (r rawDefaults) value() Defaults {
	return Defaults{time.Duration(r.TTL), time.Duration(r.MinHold), time.Duration(r.MaxHold),
		time.Duration(r.DrainAfter), time.Duration(r.QueueTimeout), time.Duration(r.StallTimeout),
		time.Duration(r.CommandLoadTimeout), time.Duration(r.LoadStallTimeout), time.Duration(r.ShutdownGrace), r.MaxPool,
		r.DeviceOverheadMiB, r.MinShareMiB, r.CacheRAMMiB, r.ServerBin, r.LocalDevice,
		time.Duration(r.ForcedNodeGrace), r.ForcedFitCheck,
		time.Duration(r.WorkerProbeInterval), time.Duration(r.WorkerSilence), r.WorkerProbeDuringInference,
		r.SplitSkewMargin, r.EstimateMargin, r.LoadMemoryFloorMiB,
		r.Membership, r.WorkerModelDir, r.WatchdogFloorMiB, time.Duration(r.WorkerStartTimeout),
		r.FillReserveKiBPerToken, r.PlacementUsesReserved, r.ReservedDriftWarnMiB, r.MarginMeasuredMiB,
		r.MarginEstimatedPercent, r.MarginEstimatedMinMiB, time.Duration(r.FreeAverageWindow)}
}

type rawRecovery struct {
	Enabled     bool     `yaml:"enabled" json:"enabled,omitempty"`
	Actions     []string `yaml:"actions" json:"actions,omitempty"`
	MinInterval duration `yaml:"min_interval" json:"min_interval,omitempty"`
	StepWait    duration `yaml:"step_wait" json:"step_wait,omitempty"`
	RequestTTL  duration `yaml:"request_ttl" json:"request_ttl,omitempty"`
	HTTPTimeout duration `yaml:"http_timeout" json:"http_timeout,omitempty"`
}

func rawRecoveryFrom(r Recovery) rawRecovery {
	return rawRecovery{r.Enabled, append([]string(nil), r.Actions...), duration(r.MinInterval),
		duration(r.StepWait), duration(r.RequestTTL), duration(r.HTTPTimeout)}
}

func (r rawRecovery) value() Recovery {
	return Recovery{r.Enabled, trimAll(r.Actions), time.Duration(r.MinInterval),
		time.Duration(r.StepWait), time.Duration(r.RequestTTL), time.Duration(r.HTTPTimeout)}
}

// rawLogs is the logs block of the YAML file: the logs_* settings.
type rawLogs struct {
	Enabled       bool   `yaml:"enabled" json:"enabled,omitempty"`
	RetentionDays int    `yaml:"retention_days" json:"retention_days,omitempty"`
	MaxDiskGiB    int    `yaml:"max_disk_gib" json:"max_disk_gib,omitempty"`
	PushURL       string `yaml:"push_url" json:"push_url,omitempty"`
	ClientIntake  bool   `yaml:"client_intake" json:"client_intake,omitempty"`
}

func rawLogsFrom(l Logs) rawLogs {
	return rawLogs{l.Enabled, l.RetentionDays, l.MaxDiskGiB, l.PushURL, l.ClientIntake}
}

func (r rawLogs) value() Logs {
	return Logs{r.Enabled, r.RetentionDays, r.MaxDiskGiB, strings.TrimSpace(r.PushURL), r.ClientIntake}
}

type rawBench struct {
	Enabled          *bool                  `yaml:"enabled" json:"enabled,omitempty"`
	RegressLoadPct   float64                `yaml:"regress_load_pct" json:"regress_load_pct,omitempty"`
	RegressDecodePct float64                `yaml:"regress_decode_pct" json:"regress_decode_pct,omitempty"`
	MaxRunMinutes    int                    `yaml:"max_run_minutes" json:"max_run_minutes,omitempty"`
	RetentionDays    *int                   `yaml:"retention_days" json:"retention_days,omitempty"`
	Benchmarks       []*benchspec.Benchmark `yaml:"benchmarks" json:"benchmarks,omitempty"`
	Suites           []*benchspec.Suite     `yaml:"suites" json:"suites,omitempty"`
}

// MarshalJSON writes the duration as a string such as "5m0s".
func (d duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON accepts "300s" or a bare 0.
func (d *duration) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	x, err := ParseDurationValue(v)
	if err != nil {
		return err
	}
	*d = duration(x)
	return nil
}

// ParseDurationValue turns a decoded JSON value (a duration string like "300s" or the number 0) into a duration.
func ParseDurationValue(v any) (time.Duration, error) {
	switch t := v.(type) {
	case string:
		x, err := time.ParseDuration(strings.TrimSpace(t))
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q (write it like 300s, or 0)", t)
		}
		return x, nil
	case float64:
		if t == 0 {
			return 0, nil
		}
	}
	return 0, fmt.Errorf("invalid duration %v (write it like 300s, or 0)", v)
}
