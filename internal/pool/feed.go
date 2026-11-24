package pool

import (
	"os"
	"strconv"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// LocalFacts is what the node feed (GET /api/nodes) shows about the
// scheduler's own machine: the fields of an agent entry that apply to it.
// The local node has no agent, so it has no programs, no heartbeat, no
// launch of its own and no pause.
type LocalFacts struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	State   string `json:"state"` // active
	Release string `json:"release"`
	Local   bool   `json:"local"` // always true
	// Capabilities: always an empty list (the local node has no agent).
	Capabilities []string `json:"capabilities"`
	Placeable    bool     `json:"placeable"`
	// Excluded, ExcludedBy, Unplaced and UnplacedMessage: as for an agent
	// node (NodeFacts), when exclude_nodes names the local node.
	Excluded   bool   `json:"excluded,omitempty"`
	ExcludedBy string `json:"excluded_by,omitempty"`
	// RunningWhileExcluded: as for an agent node (NodeFacts).
	RunningWhileExcluded *RunningInstance `json:"running_while_excluded,omitempty"`
	Unplaced             string           `json:"unplaced,omitempty"`
	UnplacedMessage      string           `json:"unplaced_message,omitempty"`
	HostUptimeS          *float64         `json:"host_uptime_s,omitempty"`
	BootID               string           `json:"boot_id,omitempty"`
	// Tier: the tier the local node was matched to (zonetier.go); the
	// local node is in the view only when it matches one.
	Tier *ZoneTier `json:"tier,omitempty"`
	// ZoneFacts: as for an agent node (zonefacts.go).
	ZoneFacts
}

// HostInfo reads the uptime (seconds since kernel boot) and boot id of the
// machine the scheduler runs on; ok is false when they cannot be read.
type HostInfo func() (uptimeS float64, bootID string, ok bool)

// ProcHostInfo reads /proc/uptime (its first figure counts from kernel boot,
// suspend included, as CLOCK_BOOTTIME does) and the kernel's boot id. In a
// container both are the host kernel's.
func ProcHostInfo() (float64, string, bool) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, "", false
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, "", false
	}
	up, err := strconv.ParseFloat(f[0], 64)
	if err != nil || up < 0 {
		return 0, "", false
	}
	id, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return up, strings.ReplaceAll(strings.TrimSpace(string(id)), "-", ""), true
}

// Local returns the scheduler's own node as the feed shows it; ok is false
// while the node view has no local node (no local probe, or it fails).
func (p *Pool) Local() (LocalFacts, bool) {
	cfg := p.o.Config()
	var f LocalFacts
	ok := false
	func() {
		p.mu.Lock()
		defer supervise.Unlocked("pool", p.unlock)
		for _, n := range p.nodes {
			if n.Local {
				f = LocalFacts{ID: string(n.ID), Name: n.Hostname, State: StateActive, Release: p.o.PotluckRelease, Local: true,
					Placeable: !n.Paused, Capabilities: []string{}, Tier: TierFacts(cfg, n.TierIndex)}
				if f.Name == "" {
					f.Name = f.ID
				}
				if e, ex := cfg.ExcludedBy(n.ID, n.Hostname); ex {
					f.Excluded, f.ExcludedBy, f.Placeable = true, e, false
					f.Unplaced, f.UnplacedMessage = UnplacedExcluded, ExcludedMessage(e)
				}
				ok = true
			}
		}
	}()
	if !ok {
		return f, false
	}
	hi := p.o.HostInfo
	if hi == nil {
		hi = ProcHostInfo
	}
	if up, id, ok := hi(); ok {
		f.HostUptimeS = &up
		if validBootID(id) {
			f.BootID = id
		}
	}
	return f, true
}
