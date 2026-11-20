package fleet

import (
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

func gbToMiB(gb float64) int { return int(math.Round(gb * 1024)) }

func clampZero(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// nodeFromMember converts a fleet member to a core.Node in MiB (design 7.1).
// local says whether the member is the scheduler's own host; it selects tiers
// with `local: true` and clears the address. ok is false when the member
// matches no tier.
//
// Unknown figures are treated as "not free": a null vram_total gives a total
// of 0; a null vram_used (or, for shared-memory tiers, a null ram figure)
// gives a free figure of 0.
func nodeFromMember(cfg *config.Config, m Member, local bool) (core.Node, bool) {
	tier, ok := cfg.MatchTier(m.Hostname, m.Role, m.Source, local)
	if !ok {
		return core.Node{}, false
	}
	n := core.Node{
		ID: core.NodeID(m.ID), Hostname: m.Hostname, Local: local, Online: m.Online,
		Priority: tier.Priority, HeadroomMiB: tier.HeadroomMiB, SharedMemory: tier.SharedMemory,
		LinkMBs: tier.LinkMBs, Settle: tier.Settle,
	}
	if m.RPCServerMemMiB != nil {
		n.RPCServerMemMiB, n.RPCServerMemKnown = *m.RPCServerMemMiB, true
	}
	if m.RPCServerVersion != nil {
		n.RPCServerVersion = *m.RPCServerVersion
	}
	if m.VRAMTotal != nil {
		n.TotalMiB = gbToMiB(*m.VRAMTotal)
	}
	if m.VRAMTotal != nil && m.VRAMUsed != nil {
		n.FreeMiB = clampZero(n.TotalMiB - gbToMiB(*m.VRAMUsed))
	}
	vramKnown := m.VRAMTotal != nil && m.VRAMUsed != nil
	n.FreeUnknown = !vramKnown
	if tier.SharedMemory {
		ramFree, ramKnown := 0, false
		switch {
		case m.MemAvailableMiB != nil:
			ramFree, ramKnown = clampZero(*m.MemAvailableMiB), true
		case m.RAMTotalGB != nil && m.RAMUsedGB != nil:
			ramFree, ramKnown = clampZero(gbToMiB(*m.RAMTotalGB)-gbToMiB(*m.RAMUsedGB)), true
		}
		switch {
		case !ramKnown:
			n.FreeUnknown = true
		case !vramKnown && m.MemAvailableMiB != nil:
			n.FreeMiB, n.FreeUnknown = ramFree, false // the newer figure stands alone
		}
		if ramFree < n.FreeMiB {
			n.FreeMiB = ramFree
		}
	}
	if !local {
		n.Addr = addrFor(cfg, m)
		if r := m.LastRecoveryAction; r != nil {
			n.Recovery = core.RecoveryOutcome{Present: true, Action: r.Action, At: r.At, Outcome: r.Outcome,
				Reason: r.Reason, Nonce: r.Nonce, DurationMs: r.DurationMs}
		}
	}
	return n, true
}

// addrFor is the rpc address: node_overrides (by id, then hostname) wins,
// else ip:rpc_port, else empty.
func addrFor(cfg *config.Config, m Member) string {
	if o, ok := cfg.NodeOverrides[m.ID]; ok && o.RPC != "" {
		return o.RPC
	}
	if o, ok := cfg.NodeOverrides[m.Hostname]; ok && o.RPC != "" {
		return o.RPC
	}
	if m.IP != nil && *m.IP != "" && m.RPCPort != nil {
		return net.JoinHostPort(*m.IP, strconv.Itoa(*m.RPCPort))
	}
	return ""
}

// parseSMI parses the output of
// `nvidia-smi --query-gpu=memory.total,memory.used --format=csv,noheader,nounits`:
// the first non-empty line, "total, used", in MiB.
func parseSMI(out []byte) (total, used int, err error) {
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) != 2 {
			return 0, 0, fmt.Errorf("probe output %q: want \"total, used\"", line)
		}
		total, err = strconv.Atoi(strings.TrimSpace(parts[0]))
		if err == nil {
			used, err = strconv.Atoi(strings.TrimSpace(parts[1]))
		}
		if err != nil || total < 0 || used < 0 {
			return 0, 0, fmt.Errorf("probe output %q: not two non-negative integers", line)
		}
		return total, used, nil
	}
	return 0, 0, fmt.Errorf("probe output is empty")
}

// ParseSMI parses the local probe output ("total, used" in MiB), for a node
// view other than Client that runs the same local_probe_cmd.
func ParseSMI(out []byte) (total, used int, err error) { return parseSMI(out) }
