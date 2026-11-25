package config

import (
	"slices"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/modelpath"
)

// managedFlags are the llama-server flags the scheduler sets itself
// (design section 6); extra_args may not contain any spelling of them.
var managedFlags = map[string]bool{
	"--rpc": true, "--device": true, "-dev": true, "--tensor-split": true, "-ts": true,
	"--split-mode": true, "-sm": true, "--host": true, "--port": true, "--model": true, "-m": true,
	"--ctx-size": true, "-c": true, "--n-gpu-layers": true, "--gpu-layers": true, "-ngl": true,
	"--parallel": true, "-np": true, "--cache-type-k": true, "-ctk": true, "--cache-type-v": true,
	"-ctv": true, "--fit": true, "-fit": true, "--cache-ram": true, "-cram": true,
	"--flash-attn": true, "-fa": true, "--main-gpu": true, "-mg": true,
	// Log verbosity is always set by the scheduler (-lv 4, design 7.4); -v and
	// --log-disable would change what the load-log reader sees.
	"-lv": true, "--verbosity": true, "--log-verbosity": true,
	"-v": true, "--verbose": true, "--log-verbose": true, "--log-disable": true,
}

// lateManaged are flags the scheduler began to set after profiles could
// already carry them (--ctx-checkpoints, from the setting ctx_checkpoints).
// A write may not set them, as with managedFlags; a stored profile that
// already has them unchanged (Options.KeepArgs) stays loadable, and the
// launch drops them (StripLateManaged) so the setting is the one value.
var lateManaged = map[string]string{
	"--ctx-checkpoints": "ctx_checkpoints", "-ctxcp": "ctx_checkpoints", "--swa-checkpoints": "ctx_checkpoints",
}

// StripLateManaged is args without the lateManaged flags and their values
// (the next entry, for a flag written without "=").
func StripLateManaged(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		name, _, eq := strings.Cut(args[i], "=")
		if _, ok := lateManaged[name]; ok && strings.HasPrefix(args[i], "-") {
			if !eq && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
			continue
		}
		out = append(out, args[i])
	}
	return out
}

// LateManagedArgs lists, one sentence each, the stored profiles whose
// extra_args set a lateManaged flag that the launch drops.
func (c *Config) LateManagedArgs() []string { return c.lateArgs }

// validate checks every startup rule and returns all violations joined, in
// a deterministic order.
func (c *Config) validate(raw map[string]*ProfileSpec, modelOK func(*core.Profile) bool) error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	c.validateDefaults(add)
	c.validateRecovery(add)
	c.validateBench(add)
	c.validateLogs(add)
	if c.DefaultModelSource != "" && !ModelSourceRE.MatchString(c.DefaultModelSource) {
		add("default_model_source must be empty, share:<host>:/<path>, dir:/<path> or push, got %q", c.DefaultModelSource)
	}
	c.validateTiers(add)
	for _, l := range []struct {
		key  string
		list []string
	}{{"exclude_nodes", c.ExcludeNodes}, {"no_auto_nodes", c.NoAutoNodes}} {
		for i, e := range l.list {
			switch {
			case e == "":
				add("%s[%d]: entry must not be empty", l.key, i)
			default:
				if _, err := path.Match(e, ""); err != nil {
					add("%s[%d]: invalid glob %q", l.key, i, e)
				}
			}
		}
	}
	names := make([]string, 0, len(c.NodeOverrides))
	for n := range c.NodeOverrides {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		ap, err := netip.ParseAddrPort(c.NodeOverrides[n].RPC)
		if err != nil || !ap.Addr().Is4() || ap.Port() == 0 {
			add("node_overrides %q: rpc: %q is not a valid IPv4:port", n, c.NodeOverrides[n].RPC)
		}
	}

	pnames := make([]string, 0, len(c.Profiles))
	for n := range c.Profiles {
		pnames = append(pnames, n)
	}
	sort.Strings(pnames)
	aliasOwner := map[string]string{}
	for _, n := range pnames {
		p := c.Profiles[n]
		pf := func(field, format string, a ...any) {
			add("profile %q: %s: %s", n, field, fmt.Sprintf(format, a...))
		}
		for _, a := range p.Aliases {
			switch {
			case c.Profiles[a] != nil:
				pf("aliases", "%q is already a profile name", a)
			case aliasOwner[a] != "":
				pf("aliases", "%q is already used as an alias of profile %q", a, aliasOwner[a])
			default:
				aliasOwner[a] = n
			}
		}
		if p.TTL != nil && *p.TTL < 0 {
			pf("ttl", "must not be negative")
		}
		if p.StallTimeout != nil && *p.StallTimeout < 0 {
			pf("stall_timeout", "must not be negative")
		}
		if p.LoadStallTimeout != nil && *p.LoadStallTimeout < 0 {
			pf("load_stall_timeout", "must not be negative")
		}
		if p.LoadTimeout < 0 {
			pf("load_timeout", "must not be negative")
		}
		if p.NeedsMiB < 0 {
			pf("needs_mib", "must not be negative")
		}
		if d := p.Description; utf8.RuneCountInString(d) > MaxDescription {
			pf("description", "is %d characters; at most %d", utf8.RuneCountInString(d), MaxDescription)
		} else if strings.ContainsFunc(d, unicode.IsControl) {
			pf("description", "must be one line of text (no line breaks or control characters)")
		}
		c.validateStage(n, p, pf)
		switch p.Kind {
		case core.KindLlama:
			c.validateLlama(p, raw[n], pf, modelOK)
		case core.KindCommand:
			validateCommand(p, raw[n], pf)
		default:
			pf("kind", "unknown kind %q (want llama or command)", p.Kind)
		}
	}
	return errors.Join(errs...)
}

// MaxDescription is the longest profile description, in characters.
const MaxDescription = 500

func (c *Config) validateLlama(p *core.Profile, r *ProfileSpec, pf func(field, format string, a ...any), modelOK func(*core.Profile) bool) {
	if r.Cmd != "" {
		pf("cmd", "only valid with kind: command")
	}
	if r.CheckEndpoint != "" {
		pf("check_endpoint", "only valid with kind: command")
	}
	if p.Model == "" {
		pf("model", "is required")
	} else if err := modelpath.Check(p.Model); err != nil && !modelOK(p) {
		pf("model", "%v", err) // the path rule; a stored profile that keeps its path passes (modelCheck)
	} else if !modelOK(p) {
		note := ""
		if c.modelNote != nil {
			if n := c.modelNote(p.Model); n != "" {
				note = ": " + n
			}
		}
		pf("model", "file %q not found under models_dir %q%s", p.Model, c.ModelsDir, note)
	}
	switch {
	case r.Ctx == nil:
		pf("ctx", "is required and must be positive")
	case *r.Ctx <= 0:
		pf("ctx", "must be positive, got %d", *r.Ctx)
	}
	if p.Parallel <= 0 {
		pf("parallel", "must be positive, got %d", p.Parallel)
	}
	switch p.KVQuant {
	case "f16":
	case "q8_0", "q4_0":
		if !p.FlashAttn {
			pf("kv_quant", "%s requires flash_attn: true", p.KVQuant)
		}
	default:
		pf("kv_quant", "unknown value %q (want f16, q8_0 or q4_0)", p.KVQuant)
	}
	for _, t := range p.AllowTiers {
		if t < 1 {
			pf("allow_tiers", "tier priority %d is below 1", t)
		}
	}
	if len(p.ForceNodes) > 0 && len(p.AllowTiers) > 0 {
		pf("force_nodes", "cannot be combined with allow_tiers")
	}
	if len(p.TensorSplit) > 0 {
		if len(p.ForceNodes) == 0 {
			pf("tensor_split", "requires force_nodes")
		} else if len(p.TensorSplit) != len(p.ForceNodes) {
			pf("tensor_split", "length %d differs from force_nodes length %d", len(p.TensorSplit), len(p.ForceNodes))
		}
		for _, v := range p.TensorSplit {
			if v <= 0 {
				pf("tensor_split", "values must be positive, got %d", v)
			}
		}
	}
	switch r.Host {
	case "", core.HostServer:
	case core.HostNode:
		// DESIGN (phase 6): Potluck does not choose a console host by itself
		// yet; the profile names the nodes and the first one hosts.
		if len(p.ForceNodes) == 0 {
			pf("host", "node requires force_nodes; the first forced node is the host")
		}
		if !c.AgentsMode() {
			pf("host", "node requires membership: agents")
		}
	default:
		pf("host", "unknown value %q (want server or node)", r.Host)
	}
	// DESIGN: excluding wins over forcing, at run time. A profile that forces
	// an excluded node is valid configuration (0.1.7): it is kept, refused at
	// run time, and named by ExclusionBlocks (a warning, not an error).
	for i, a := range p.ExtraArgs {
		name := a
		if strings.HasPrefix(a, "-") {
			name, _, _ = strings.Cut(a, "=")
		}
		if managedFlags[name] {
			pf("extra_args", "entry %d %q is a managed flag (%s); the scheduler sets it from the profile", i, a, name)
		}
		if setting, ok := lateManaged[name]; ok && strings.HasPrefix(a, "-") {
			if kept, had := c.keepArgs[p.Name]; had && slices.Equal(kept, p.ExtraArgs) {
				val := a
				if _, v, eq := strings.Cut(a, "="); eq {
					val = name + " " + v
				} else if i+1 < len(p.ExtraArgs) {
					val = name + " " + p.ExtraArgs[i+1]
				}
				c.lateArgs = append(c.lateArgs, fmt.Sprintf("profile %q: extra_args %s is not passed: the scheduler passes --ctx-checkpoints from the setting %s on every launch; remove it from the profile", p.Name, val, setting))
				continue
			}
			pf("extra_args", "entry %d %q sets %s, which the scheduler passes on every launch from the setting %s; remove it here and change the setting instead", i, a, name, setting)
		}
	}
}

func validateCommand(p *core.Profile, r *ProfileSpec, pf func(field, format string, a ...any)) {
	if p.Cmd == "" {
		pf("cmd", "is required for kind: command")
	} else if !strings.Contains(p.Cmd, "${PORT}") {
		pf("cmd", "must contain ${PORT}")
	}
	if p.NeedsMiB <= 0 {
		pf("needs_mib", "is required for kind: command and must be positive")
	}
	llamaOnly := []struct {
		field string
		set   bool
	}{
		{"model", r.Model != ""}, {"ctx", r.Ctx != nil}, {"parallel", r.Parallel != nil},
		{"kv_quant", r.KVQuant != ""}, {"flash_attn", r.FlashAttn != nil},
		{"allow_tiers", r.AllowTiers != nil}, {"force_nodes", r.ForceNodes != nil},
		{"tensor_split", r.TensorSplit != nil}, {"extra_args", r.ExtraArgs != nil},
		{"host", r.Host != ""},
	}
	for _, f := range llamaOnly {
		if f.set {
			pf(f.field, "only valid with kind: llama")
		}
	}
}

func (c *Config) validateTiers(add func(string, ...any)) {
	for i, t := range c.Tiers {
		tf := func(field, format string, a ...any) {
			add("tiers[%d]: %s: %s", i, field, fmt.Sprintf(format, a...))
		}
		if t.Priority < 1 {
			tf("priority", "must be at least 1, got %d", t.Priority)
		}
		m := t.Match
		if m.Hostname == "" && m.Role == "" && m.Source == "" && m.Local == nil && m.Platform == "" && m.Device == "" {
			tf("match", "must set at least one of hostname, role, source, local, platform, device")
		}
		if _, err := path.Match(m.Hostname, ""); err != nil {
			tf("match.hostname", "invalid glob %q", m.Hostname)
		}
		if _, err := path.Match(m.Device, ""); err != nil {
			tf("match.device", "invalid glob %q", m.Device)
		}
		if t.HeadroomMiB < 0 {
			tf("headroom_mib", "must not be negative")
		}
		if t.LinkMBs < 0 {
			tf("link_mb_s", "must not be negative")
		}
		if t.Settle < 0 {
			tf("settle", "must not be negative")
		}
	}
}

func (c *Config) validateDefaults(add func(string, ...any)) {
	d := c.Defaults
	// ttl and stall_timeout may be 0 (never unload, check disabled).
	for _, x := range []struct {
		name string
		v    time.Duration
	}{{"ttl", d.TTL}, {"stall_timeout", d.StallTimeout}, {"load_stall_timeout", d.LoadStallTimeout},
		{"worker_probe_interval", d.WorkerProbeInterval}, {"worker_silence", d.WorkerSilence}} {
		if x.v < 0 {
			add("defaults: %s: must not be negative", x.name)
		}
	}
	for _, x := range []struct {
		name string
		v    time.Duration
	}{
		{"min_hold", d.MinHold}, {"max_hold", d.MaxHold}, {"drain_after", d.DrainAfter},
		{"queue_timeout", d.QueueTimeout}, {"command_load_timeout", d.CommandLoadTimeout},
		{"shutdown_grace", d.ShutdownGrace}, {"forced_node_grace", d.ForcedNodeGrace},
	} {
		if x.v <= 0 {
			add("defaults: %s: must be positive, got %v", x.name, x.v)
		}
	}
	if d.WorkerSilence <= 0 {
		add("defaults: worker_silence: must be positive, got %v", d.WorkerSilence)
	}
	if d.MinHold > d.MaxHold {
		add("defaults: max_hold: must be at least min_hold (%v), got %v", d.MinHold, d.MaxHold)
	}
	for _, x := range []struct {
		name string
		v    float64
	}{{"split_skew_margin", d.SplitSkewMargin}, {"estimate_margin", d.EstimateMargin}} {
		if !(x.v >= 0 && x.v <= 1) { // also rejects NaN
			add("defaults: %s: must be between 0 and 1, got %v", x.name, x.v)
		}
	}
	if d.MaxPool < 1 {
		add("defaults: max_pool: must be at least 1, got %d", d.MaxPool)
	}
	switch d.Membership {
	case MembershipFleet, MembershipAgents:
	default:
		add("defaults: membership: must be %s or %s, got %q", MembershipFleet, MembershipAgents, d.Membership)
	}
	if d.WorkerStartTimeout <= 0 {
		add("defaults: worker_start_timeout: must be positive, got %v", d.WorkerStartTimeout)
	}
	if d.WatchdogFloorMiB < 0 {
		add("defaults: watchdog_floor_mib: must not be negative, got %d", d.WatchdogFloorMiB)
	}
	for _, x := range []struct {
		name string
		v    int
	}{{"device_overhead_mib", d.DeviceOverheadMiB}, {"min_share_mib", d.MinShareMiB}, {"cache_ram_mib", d.CacheRAMMiB},
		{"load_memory_floor_mib", d.LoadMemoryFloorMiB}, {"fill_reserve_kib_per_token", d.FillReserveKiBPerToken},
		{"reserved_drift_warn_mib", d.ReservedDriftWarnMiB}, {"margin_measured_mib", d.MarginMeasuredMiB},
		{"margin_estimated_min_mib", d.MarginEstimatedMinMiB}, {"placement_tolerance_mib", d.PlacementToleranceMiB},
		{"node_host_cache_ram_mib", d.NodeHostCacheRAMMiB}, {"ctx_checkpoints", d.CtxCheckpoints}} {
		if x.v < 0 {
			add("defaults: %s: must not be negative", x.name)
		}
	}
	if d.MarginEstimatedPercent < 0 || d.MarginEstimatedPercent > 50 {
		add("defaults: margin_estimated_percent: must be between 0 and 50, got %d", d.MarginEstimatedPercent)
	}
	if d.FreeAverageWindow < 0 {
		add("defaults: free_average_window: must not be negative, got %v", d.FreeAverageWindow)
	}
	// Upper bounds (review N5): the reserve arithmetic stays far from
	// overflow, and the pool keeps a bounded number of samples per node.
	if d.FillReserveKiBPerToken > 1024 {
		add("defaults: fill_reserve_kib_per_token: must be at most 1024, got %d", d.FillReserveKiBPerToken)
	}
	if d.HostOverheadMiB < 0 || d.HostOverheadMiB > 16384 {
		add("defaults: host_overhead_mib: must be between 0 and 16384, got %d", d.HostOverheadMiB)
	}
	if d.FreeAverageWindow > 10*time.Minute {
		add("defaults: free_average_window: must be at most 10m, got %v", d.FreeAverageWindow)
	}
	switch d.ForcedFitCheck {
	case ForcedFitOff, ForcedFitWarn, ForcedFitReject:
	default:
		add("defaults: forced_fit_check: must be %s, %s or %s, got %q", ForcedFitOff, ForcedFitWarn, ForcedFitReject, d.ForcedFitCheck)
	}
	if d.ServerBin == "" {
		add("defaults: server_bin: must not be empty")
	}
	if d.LocalDevice == "" {
		add("defaults: local_device: must not be empty")
	}
	if c.ChildLogKeep < 1 {
		add("child_log_keep: must be at least 1, got %d", c.ChildLogKeep)
	}
	if c.HistoryMaxFileMiB < 1 {
		add("history_max_file_mib: must be at least 1, got %d", c.HistoryMaxFileMiB)
	}
	if c.HistoryKeepFiles < 1 {
		add("history_keep_files: must be at least 1, got %d", c.HistoryKeepFiles)
	}
	for _, u := range []struct {
		key string
		v   int
	}{{"usage_request_days", c.UsageRequestDays}, {"usage_hourly_days", c.UsageHourlyDays}} {
		if u.v < 1 || u.v > MaxUsageDays {
			add("%s: must be from 1 to %d, got %d", u.key, MaxUsageDays, u.v)
		}
	}
	if c.LoopStepWarnMs < 10 || c.LoopStepWarnMs > MaxLoopStepWarnMs {
		add("loop_step_warn_ms: must be from 10 to %d, got %d", MaxLoopStepWarnMs, c.LoopStepWarnMs)
	}
	if n := utf8.RuneCountInString(c.DefaultUserName); n < 1 || n > MaxDefaultUserName || strings.TrimSpace(c.DefaultUserName) != c.DefaultUserName ||
		strings.IndexFunc(c.DefaultUserName, unicode.IsControl) >= 0 {
		add("default_user_name: must be 1 to %d characters without control characters or surrounding spaces, got %q", MaxDefaultUserName, c.DefaultUserName)
	}
	if c.ModelTrashDays < 1 || c.ModelTrashDays > MaxModelTrashDays {
		add("model_trash_days: must be from 1 to %d, got %d", MaxModelTrashDays, c.ModelTrashDays)
	}
	if c.ModelsDiskBudgetGB < 0 {
		add("models_disk_budget_gb: must not be negative, got %d", c.ModelsDiskBudgetGB)
	}
	if c.HistoryMemoryEvents < 1 {
		add("history_memory_events: must be at least 1, got %d", c.HistoryMemoryEvents)
	} else if c.HistoryMemoryEvents > MaxHistoryMemoryEvents {
		add("history_memory_events: must be at most %d, got %d", MaxHistoryMemoryEvents, c.HistoryMemoryEvents)
	}
	if c.Fleet.StaleAfter <= 0 {
		add("fleet: stale_after: must be positive, got %v", c.Fleet.StaleAfter)
	}
}

// validateRecovery checks the recovery block whether or not it is enabled, so
// a mistake is found before the feature is switched on.
func (c *Config) validateRecovery(add func(string, ...any)) {
	r := c.Recovery
	if len(r.Actions) == 0 {
		add("recovery: actions: must list at least one of %s", strings.Join(RecoveryActions, ", "))
	}
	rank := map[string]int{}
	for i, a := range RecoveryActions {
		rank[a] = i
	}
	seen := map[string]bool{}
	last := -1
	for _, a := range r.Actions {
		i, ok := rank[a]
		switch {
		case !ok:
			add("recovery: actions: unknown action %q (allowed: %s)", a, strings.Join(RecoveryActions, ", "))
			continue
		case seen[a]:
			add("recovery: actions: %q is listed twice", a)
			continue
		case i < last:
			add("recovery: actions: %q must come before %q (order: %s)", a, RecoveryActions[last], strings.Join(RecoveryActions, ", "))
		}
		seen[a] = true
		last = max(last, i)
	}
	if r.MinInterval < 60*time.Second {
		add("recovery: min_interval: must be at least 60s, got %v", r.MinInterval)
	}
	if r.StepWait <= 0 {
		add("recovery: step_wait: must be positive, got %v", r.StepWait)
	}
	if r.RequestTTL <= 0 || r.RequestTTL > 60*time.Second {
		add("recovery: request_ttl: must be positive and at most 60s, got %v", r.RequestTTL)
	}
	if r.HTTPTimeout <= 0 {
		add("recovery: http_timeout: must be positive, got %v", r.HTTPTimeout)
	}
}

// validateBench checks data_dir, ws_max_clients and the bench block, including
// the seeded benchmarks and suites (profiles must exist, references must resolve).
func (c *Config) validateBench(add func(string, ...any)) {
	if c.WSMaxClients < 1 || c.WSMaxClients > MaxWSMaxClients {
		add("ws_max_clients must be 1 to %d, got %d", MaxWSMaxClients, c.WSMaxClients)
	}
	if c.benchEnabledSet != nil && *c.benchEnabledSet && c.DataDir == "" {
		add("bench.enabled is true but data_dir is empty: the benchmark feature stores its results under data_dir")
	}
	b := &c.Bench
	if b.RegressLoadPct <= 0 || b.RegressLoadPct > 1000 {
		add("bench.regress_load_pct must be above 0 and at most 1000, got %v", b.RegressLoadPct)
	}
	if b.RegressDecodePct <= 0 || b.RegressDecodePct > 100 {
		add("bench.regress_decode_pct must be above 0 and at most 100, got %v", b.RegressDecodePct)
	}
	if b.MaxRunMinutes < 1 || b.MaxRunMinutes > 10080 {
		add("bench.max_run_minutes must be 1 to 10080, got %d", b.MaxRunMinutes)
	}
	if b.RetentionDays < 0 || b.RetentionDays > MaxBenchRetentionDays {
		add("bench.retention_days must be 0 (off) to %d, got %d", MaxBenchRetentionDays, b.RetentionDays)
	}
	profileOK := func(id string) bool { _, _, ok := c.Resolve(id); return ok }
	byID := map[string]*benchspec.Benchmark{}
	for i, bm := range b.Benchmarks {
		if bm == nil {
			add("bench.benchmarks[%d]: empty entry", i)
			continue
		}
		if err := bm.Validate(profileOK); err != nil {
			add("bench.benchmarks[%d] (%s): %v", i, bm.ID, err)
		}
		if byID[bm.ID] != nil {
			add("bench.benchmarks[%d]: duplicate id %q", i, bm.ID)
		}
		byID[bm.ID] = bm
	}
	for _, bm := range b.Benchmarks {
		if bm == nil || !bm.IsParallel() {
			continue
		}
		for _, m := range bm.Parallel {
			if mb, ok := byID[m]; !ok || mb.IsParallel() {
				add("bench.benchmarks (%s): parallel member %q is not a plain benchmark of this block", bm.ID, m)
			}
		}
	}
	look := func(id string) (*benchspec.Benchmark, bool) { bm, ok := byID[id]; return bm, ok }
	groups := func(id string) (*benchspec.Suite, bool) {
		for _, s := range b.Suites {
			if s != nil && s.ID == id {
				return s, true
			}
		}
		return nil, false
	}
	seen := map[string]bool{}
	for i, s := range b.Suites {
		if s == nil {
			add("bench.suites[%d]: empty entry", i)
			continue
		}
		if err := s.ValidateGroup(profileOK, look, groups); err != nil {
			add("bench.suites[%d] (%s): %v", i, s.ID, err)
		}
		if seen[s.ID] {
			add("bench.suites[%d]: duplicate id %q", i, s.ID)
		}
		seen[s.ID] = true
	}
}

// Bounds of the logging settings (sanity limits).
const (
	MaxLogsRetentionDays = 3650
	MaxLogsMaxDiskGiB    = 100000
)

// validateLogs checks the central logging settings (CR-10).
func (c *Config) validateLogs(add func(string, ...any)) {
	l := c.Logs
	if l.RetentionDays < 1 || l.RetentionDays > MaxLogsRetentionDays {
		add("logs_retention_days must be 1 to %d, got %d", MaxLogsRetentionDays, l.RetentionDays)
	}
	if l.MaxDiskGiB < 1 || l.MaxDiskGiB > MaxLogsMaxDiskGiB {
		add("logs_max_disk_gib must be 1 to %d, got %d", MaxLogsMaxDiskGiB, l.MaxDiskGiB)
	}
	if l.PushURL != "" {
		u, err := url.Parse(l.PushURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" {
			add("logs_push_url must be empty or an http:// or https:// address without a user or a fragment, got %q", l.PushURL)
		}
	}
}
