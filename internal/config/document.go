package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// Bootstrap holds the deployment facts that stay outside the database: they
// are set by flags or environment, not by scheduler policy.
type Bootstrap struct {
	Listen      string
	ModelsDir   string
	DataDir     string
	HistoryDir  string
	ChildLogDir string
	ProgramsDir string
}

// Document is the stored configuration (one per revision). Settings holds
// only the keys that were set explicitly; every other key has its default.
type Document struct {
	Settings      map[string]any              `json:"settings"`
	Tiers         []TierSpec                  `json:"tiers"`
	Profiles      map[string]*ProfileSpec     `json:"profiles"`
	ExcludeNodes  []string                    `json:"exclude_nodes"`
	NoAutoNodes   []string                    `json:"no_auto_nodes"`
	NodeOverrides map[string]NodeOverrideSpec `json:"node_overrides"`
	Recovery      map[string]any              `json:"recovery"`
	Fleet         map[string]any              `json:"fleet"`
}

// NewDocument is an empty configuration: the defaults, no profiles, no tiers.
func NewDocument() *Document {
	return &Document{Settings: map[string]any{}, Tiers: []TierSpec{}, Profiles: map[string]*ProfileSpec{},
		ExcludeNodes: []string{}, NoAutoNodes: []string{}, NodeOverrides: map[string]NodeOverrideSpec{}, Recovery: map[string]any{}, Fleet: map[string]any{}}
}

// Normalize replaces nil collections with empty ones (stable JSON).
func (d *Document) Normalize() {
	if d.Settings == nil {
		d.Settings = map[string]any{}
	}
	if d.Tiers == nil {
		d.Tiers = []TierSpec{}
	}
	if d.Profiles == nil {
		d.Profiles = map[string]*ProfileSpec{}
	}
	if d.ExcludeNodes == nil {
		d.ExcludeNodes = []string{}
	}
	if d.NoAutoNodes == nil {
		d.NoAutoNodes = []string{}
	}
	if d.NodeOverrides == nil {
		d.NodeOverrides = map[string]NodeOverrideSpec{}
	}
	if d.Recovery == nil {
		d.Recovery = map[string]any{}
	}
	if d.Fleet == nil {
		d.Fleet = map[string]any{}
	}
}

// Clone is a deep copy.
func (d *Document) Clone() *Document {
	b, _ := json.Marshal(d)
	out := NewDocument()
	_ = json.Unmarshal(b, out)
	out.Normalize()
	return out
}

// ParseDocument decodes a stored or imported JSON document, rejecting unknown fields.
func ParseDocument(b []byte) (*Document, error) {
	d := NewDocument()
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(d); err != nil {
		return nil, fmt.Errorf("configuration document: %w", err)
	}
	d.Normalize()
	return d, nil
}

func newBase(b Bootstrap) *Config {
	listen := b.Listen
	if listen == "" {
		listen = ":8080"
	}
	models := b.ModelsDir
	if models == "" {
		models = "/models"
	}
	return &Config{
		Listen: listen, ModelsDir: models, DataDir: b.DataDir, HistoryDir: b.HistoryDir, ChildLogDir: b.ChildLogDir, ProgramsDir: b.ProgramsDir,
		Fleet:        Fleet{StaleAfter: 60 * time.Second},
		ChildLogKeep: DefaultChildLogKeep, HistoryMaxFileMiB: DefaultHistoryMaxFileMiB, HistoryKeepFiles: DefaultHistoryKeepFiles,
		HistoryMemoryEvents: DefaultHistoryMemoryEvents, UsageRequestDays: DefaultUsageRequestDays, UsageHourlyDays: DefaultUsageHourlyDays, LocalProbeCmd: DefaultLocalProbeCmd, WSMaxClients: DefaultWSMaxClients, LoopStepWarnMs: DefaultLoopStepWarnMs, ModelTrashDays: DefaultModelTrashDays,
		DefaultUserName: DefaultDefaultUserName,
		Bench:           Bench{EnabledSetting: true, RegressLoadPct: DefaultRegressLoadPct, RegressDecodePct: DefaultRegressDecodePct, MaxRunMinutes: DefaultMaxRunMinutes, RetentionDays: DefaultBenchRetentionDays},
		Defaults:        DefaultDefaults(),
		Recovery:        DefaultRecovery(),
		Logs:            DefaultLogs(),
		NodeOverrides:   map[string]NodeOverride{},
		Profiles:        map[string]*core.Profile{},
	}
}

// Build turns a document into a validated Config. Validation is the same as
// for the old YAML file; every violation is reported, joined.
func Build(doc *Document, b Bootstrap, o Options) (*Config, error) {
	if doc == nil {
		doc = NewDocument()
	}
	c := newBase(b)
	var errs []error
	apply := func(scope string, vals map[string]any) {
		keys := make([]string, 0, len(vals))
		for k := range vals {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v, err := CoerceSetting(scope, k, vals[k])
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %v", scope, err))
				continue
			}
			f, _ := findField(scope, k)
			applyValue(f, c, v)
		}
	}
	apply(ScopeSettings, doc.Settings)
	apply(ScopeRecovery, doc.Recovery)
	apply(ScopeFleet, doc.Fleet)
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	c.Bench.Enabled = c.DataDir != "" && c.Bench.EnabledSetting
	for _, t := range doc.Tiers {
		c.Tiers = append(c.Tiers, TierRule{
			Match: Match{Hostname: t.Match.Hostname, Role: t.Match.Role, Source: t.Match.Source, Local: t.Match.Local,
				Platform: t.Match.Platform, Device: t.Match.Device},
			Tier: core.Tier{Priority: t.Priority, HeadroomMiB: t.HeadroomMiB, SharedMemory: t.SharedMemory,
				LinkMBs: t.LinkMBs, Settle: time.Duration(t.Settle)},
		})
	}
	for k, v := range doc.NodeOverrides {
		c.NodeOverrides[k] = NodeOverride{RPC: v.RPC}
	}
	c.ExcludeNodes = trimAll(doc.ExcludeNodes)
	c.NoAutoNodes = trimAll(doc.NoAutoNodes)
	c.Recovery.Actions = trimAll(c.Recovery.Actions)
	raws := map[string]*ProfileSpec{}
	for name, rp := range doc.Profiles {
		if rp == nil {
			rp = &ProfileSpec{}
		}
		raws[name] = rp
		c.Profiles[name] = rp.toCore(name)
	}
	exists := o.FileExists
	if exists == nil {
		exists = func(p string) bool { _, err := os.Stat(p); return err == nil }
	}
	c.modelNote = o.ModelNote
	c.trainedCtx, c.keepCtx = o.TrainedCtx, o.KeepCtx
	if err := c.validate(raws, c.modelCheck(o, exists)); err != nil {
		return nil, err
	}
	c.Configured = true
	return c, nil
}

// Import is the result of reading a YAML file for the import command.
type Import struct {
	Doc        *Document
	Config     *Config
	Benchmarks []*benchspec.Benchmark
	Suites     []*benchspec.Suite
	// Notes lists YAML keys that now come from flags or environment and were ignored.
	Notes []string
}

// ParseYAMLImport reads a file in the old YAML format into a Document. The
// bootstrap values win over the file's listen, models_dir, data_dir,
// history_dir and child_log_dir (which are not stored); a zero bootstrap
// value falls back to the file's. The result is validated with Build.
func ParseYAMLImport(data []byte, b Bootstrap, o Options) (*Import, error) {
	loose := o
	loose.FileExists = func(string) bool { return true }
	c, raw, rawProfiles, err := parseYAML(data, loose)
	if err != nil {
		return nil, err
	}
	imp := &Import{Benchmarks: raw.Bench.Benchmarks, Suites: raw.Bench.Suites}
	pick := func(flagVal, yamlVal, name, builtin string) string {
		if yamlVal != "" && yamlVal != builtin && flagVal != "" && flagVal != yamlVal {
			imp.Notes = append(imp.Notes, fmt.Sprintf("%s: the file says %q, using %q from the flags", name, yamlVal, flagVal))
		}
		if flagVal != "" {
			return flagVal
		}
		return yamlVal
	}
	b.ModelsDir = pick(b.ModelsDir, raw.ModelsDir, "models_dir", "/models")
	b.Listen = pick(b.Listen, raw.Listen, "listen", ":8080")
	b.DataDir = pick(b.DataDir, raw.DataDir, "data_dir", "")
	b.HistoryDir = pick(b.HistoryDir, raw.HistoryDir, "history_dir", "")
	b.ChildLogDir = pick(b.ChildLogDir, raw.ChildLogDir, "child_log_dir", "")

	doc := NewDocument()
	base := newBase(b)
	for _, f := range settingFields {
		cur, def := SettingValue(c, f.Scope, f.Key), SettingValue(base, f.Scope, f.Key)
		if reflect.DeepEqual(cur, def) {
			continue
		}
		switch f.Scope {
		case ScopeSettings:
			doc.Settings[f.Key] = cur
		case ScopeRecovery:
			doc.Recovery[f.Key] = cur
		case ScopeFleet:
			doc.Fleet[f.Key] = cur
		}
	}
	doc.Tiers = append(doc.Tiers, raw.Tiers...)
	for k, v := range raw.NodeOverrides {
		doc.NodeOverrides[k] = v
	}
	doc.ExcludeNodes = append(doc.ExcludeNodes, trimAll(raw.ExcludeNodes)...)
	doc.NoAutoNodes = append(doc.NoAutoNodes, trimAll(raw.NoAutoNodes)...)
	for k, v := range rawProfiles {
		doc.Profiles[k] = v
	}
	cfg, err := Build(doc, b, o)
	if err != nil {
		return nil, err
	}
	imp.Doc, imp.Config = doc, cfg
	for _, x := range cfg.ExclusionBlocks() {
		imp.Notes = append(imp.Notes, x.Warning())
	}
	return imp, nil
}
