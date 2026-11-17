package instance

import (
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
)

// BaseLoadTimeout is the fixed part of the load allowance (design 7.6).
const BaseLoadTimeout = 120 * time.Second

// NodeShare is one remote node's part of a load: the fraction of the model
// file it receives and the speed of its link.
type NodeShare struct {
	Fraction float64 // 0..1, the node's fraction of the tensor split
	LinkMBs  float64 // megabytes (10^6 bytes) per second; 0 or less means unknown
}

// LoadTimeoutFor computes the design 7.6 allowance for a llama load:
// 120 s + 2 x sum over remote nodes of (file bytes x fraction / link bytes
// per second). fileBytes is in bytes and link speeds are in MB/s with
// MB = 10^6 bytes, so "10 GB to a PS5 at 11 MB/s" is 1938 s (about 32
// minutes). A node with an unknown link speed contributes nothing.
func LoadTimeoutFor(fileBytes int64, shares []NodeShare) time.Duration {
	var secs float64
	for _, s := range shares {
		if s.LinkMBs <= 0 || s.Fraction <= 0 {
			continue
		}
		secs += float64(fileBytes) * s.Fraction / (s.LinkMBs * 1e6)
	}
	return BaseLoadTimeout + time.Duration(2*secs*float64(time.Second))
}

// EffectiveLoadTimeout resolves the timeout for one instance: the profile's
// load_timeout when set; else commandDefault (defaults.command_load_timeout)
// for kind: command; else the design 7.6 formula.
func EffectiveLoadTimeout(p core.Profile, commandDefault time.Duration, fileBytes int64, shares []NodeShare) time.Duration {
	switch {
	case p.LoadTimeout > 0:
		return p.LoadTimeout
	case p.Kind == core.KindCommand:
		return commandDefault
	default:
		return LoadTimeoutFor(fileBytes, shares)
	}
}

// RemoteShares returns one NodeShare per remote node in pl, in placement
// order. A node's fraction is its weight over the sum of all weights, using
// the same weights as the command's --tensor-split (placement split, else
// profile split, else budgets). linkMBs looks up a node's tier link speed.
// If the weights are unusable, the file is assumed to be spread evenly.
func RemoteShares(p core.Profile, pl core.Placement, linkMBs func(core.NodeID) float64) []NodeShare {
	n := len(pl.Nodes)
	if n == 0 {
		return nil
	}
	w, err := splitWeights(p, pl)
	if err != nil {
		w = make([]int, n)
		for i := range w {
			w[i] = 1
		}
	}
	sum := 0
	for _, v := range w {
		sum += v
	}
	var out []NodeShare
	for i, node := range pl.Nodes {
		if node.Local {
			continue
		}
		out = append(out, NodeShare{Fraction: float64(w[i]) / float64(sum), LinkMBs: linkMBs(node.ID)})
	}
	return out
}
