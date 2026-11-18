package config

import (
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
)

// Config is the parsed, validated configuration file.
type Config struct {
	Listen    string
	Fleet     Fleet
	ModelsDir string
	// ChildLogDir, when set, receives one file per child start holding its
	// complete stdout and stderr; ChildLogKeep is how many such files to keep.
	ChildLogDir   string
	ChildLogKeep  int
	LocalProbeCmd string
	Defaults      Defaults
	Tiers         []TierRule
	NodeOverrides map[string]NodeOverride
	// ExcludeNodes lists nodes that are never used: each entry is a node id or a
	// hostname glob (case-insensitive). See Excluded.
	ExcludeNodes []string
	Profiles     map[string]*core.Profile
}

// Fleet is the fleet console connection.
type Fleet struct {
	URL        string
	LocalNode  string
	StaleAfter time.Duration
}

// Defaults are the policy values used when a profile does not override them.
type Defaults struct {
	TTL                time.Duration
	MinHold            time.Duration
	MaxHold            time.Duration
	DrainAfter         time.Duration
	QueueTimeout       time.Duration
	StallTimeout       time.Duration
	CommandLoadTimeout time.Duration
	ShutdownGrace      time.Duration
	MaxPool            int
	DeviceOverheadMiB  int
	MinShareMiB        int
	CacheRAMMiB        int
	ServerBin          string
	LocalDevice        string
	// ForcedNodeGrace is how long a forced node may be absent or offline in a
	// live fleet view before requests for the profile fail fast.
	ForcedNodeGrace time.Duration
	// ForcedFitCheck is what a profile with force_nodes does when its memory
	// need exceeds the forced nodes' free memory at pre-flight: ForcedFitOff
	// (no check, the default), ForcedFitWarn (log and start) or
	// ForcedFitReject (fail the request).
	ForcedFitCheck string
}

// Values of defaults.forced_fit_check.
const (
	ForcedFitOff    = "off"
	ForcedFitWarn   = "warn"
	ForcedFitReject = "reject"
)

// Match is a tier's match clause. Empty fields are not checked.
type Match struct {
	Hostname string
	Role     string
	Source   string
	Local    *bool
}

// TierRule is one tiers[] entry: a match clause and the settings it grants.
type TierRule struct {
	Match Match
	core.Tier
}

// NodeOverride replaces the fleet's ip/rpc_port for a node.
type NodeOverride struct {
	RPC string
}

// Options tune parsing; the zero value uses the real filesystem.
type Options struct {
	// FileExists reports whether a model file exists. Nil uses os.Stat.
	FileExists func(path string) bool
}
