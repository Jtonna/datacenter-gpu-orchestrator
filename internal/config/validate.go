package config

import (
	"errors"
	"fmt"
	"net/netip"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
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
}

// validate checks every startup rule and returns all violations joined, in
// a deterministic order.
func (c *Config) validate(raw map[string]*rawProfile, exists func(string) bool) error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	c.validateDefaults(add)
	c.validateTiers(add)
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
		if p.LoadTimeout < 0 {
			pf("load_timeout", "must not be negative")
		}
		if p.NeedsMiB < 0 {
			pf("needs_mib", "must not be negative")
		}
		switch p.Kind {
		case core.KindLlama:
			c.validateLlama(p, raw[n], pf, exists)
		case core.KindCommand:
			validateCommand(p, raw[n], pf)
		default:
			pf("kind", "unknown kind %q (want llama or command)", p.Kind)
		}
	}
	return errors.Join(errs...)
}

func (c *Config) validateLlama(p *core.Profile, r *rawProfile, pf func(field, format string, a ...any), exists func(string) bool) {
	if r.Cmd != "" {
		pf("cmd", "only valid with kind: command")
	}
	if r.CheckEndpoint != "" {
		pf("check_endpoint", "only valid with kind: command")
	}
	if p.Model == "" {
		pf("model", "is required")
	} else if !exists(c.modelPath(p)) {
		pf("model", "file %q not found under models_dir %q", p.Model, c.ModelsDir)
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
	for i, a := range p.ExtraArgs {
		name := a
		if strings.HasPrefix(a, "-") {
			name, _, _ = strings.Cut(a, "=")
		}
		if managedFlags[name] {
			pf("extra_args", "entry %d %q is a managed flag (%s); the scheduler sets it from the profile", i, a, name)
		}
	}
}

func validateCommand(p *core.Profile, r *rawProfile, pf func(field, format string, a ...any)) {
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
		if m.Hostname == "" && m.Role == "" && m.Source == "" && m.Local == nil {
			tf("match", "must set at least one of hostname, role, source, local")
		}
		if _, err := path.Match(m.Hostname, ""); err != nil {
			tf("match.hostname", "invalid glob %q", m.Hostname)
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
	for _, x := range []struct {
		name string
		v    time.Duration
	}{
		{"ttl", d.TTL}, {"min_hold", d.MinHold}, {"max_hold", d.MaxHold}, {"drain_after", d.DrainAfter},
		{"queue_timeout", d.QueueTimeout}, {"stall_timeout", d.StallTimeout},
		{"command_load_timeout", d.CommandLoadTimeout}, {"shutdown_grace", d.ShutdownGrace},
	} {
		if x.v < 0 {
			add("defaults: %s: must not be negative", x.name)
		}
	}
	if d.MinHold > d.MaxHold {
		add("defaults: min_hold: must not exceed max_hold")
	}
	if d.MaxPool < 1 {
		add("defaults: max_pool: must be at least 1, got %d", d.MaxPool)
	}
	for _, x := range []struct {
		name string
		v    int
	}{{"device_overhead_mib", d.DeviceOverheadMiB}, {"min_share_mib", d.MinShareMiB}, {"cache_ram_mib", d.CacheRAMMiB}} {
		if x.v < 0 {
			add("defaults: %s: must not be negative", x.name)
		}
	}
	if d.ServerBin == "" {
		add("defaults: server_bin: must not be empty")
	}
	if d.LocalDevice == "" {
		add("defaults: local_device: must not be empty")
	}
	if c.Fleet.StaleAfter < 0 {
		add("fleet: stale_after: must not be negative")
	}
}
