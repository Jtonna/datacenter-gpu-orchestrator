package instance

import (
	"math"
	"testing"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
)

const (
	// link_mb_s is megabytes per second: 10^6 bytes, as network links are
	// quoted. A "10 GB" model file is 10^10 bytes.
	gb  = int64(1_000_000_000)
	gib = int64(1 << 30)
)

func TestLoadTimeoutFor(t *testing.T) {
	tests := []struct {
		name   string
		bytes  int64
		shares []NodeShare
		want   time.Duration
	}{
		{"no remote nodes", 10 * gb, nil, 120 * time.Second},
		// 10 GB = 10000 MB; 10000/11 = 909.09 s; x2 = 1818.18 s; +120 = 1938.18 s
		{"design 7.6 example", 10 * gb, []NodeShare{{1, 11}}, 1938*time.Second + 181818182*time.Nanosecond},
		{"split across two equal links costs the same", 10 * gb, []NodeShare{{0.6, 11}, {0.4, 11}}, 1938*time.Second + 181818182*time.Nanosecond},
		{"faster second link", 10 * gb, []NodeShare{{0.6, 11}, {0.4, 22}}, 1574*time.Second + 545454545*time.Nanosecond},
		{"unknown link speed contributes nothing", 10 * gb, []NodeShare{{1, 0}}, 120 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := LoadTimeoutFor(tc.bytes, tc.shares)
			if d := got - tc.want; d > time.Millisecond || d < -time.Millisecond {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

// The design says "10 GB to a PS5 at 11 MB/s gives about 32 minutes". That
// holds for decimal GB; a 10 GiB file would be about 34.5 minutes, so file
// sizes must be passed in bytes and never confused with MiB.
func TestLoadTimeoutWorkedExampleMBvsMiB(t *testing.T) {
	dec := LoadTimeoutFor(10*gb, []NodeShare{{1, 11}})
	if m := dec.Minutes(); m < 32 || m > 33 {
		t.Fatalf("10 GB at 11 MB/s = %.2f min, want about 32", m)
	}
	bin := LoadTimeoutFor(10*gib, []NodeShare{{1, 11}})
	if m := bin.Minutes(); m < 34 || m > 35 {
		t.Fatalf("10 GiB at 11 MB/s = %.2f min, want about 34.5", m)
	}
}

func TestEffectiveLoadTimeout(t *testing.T) {
	cmdDefault := 900 * time.Second
	shares := []NodeShare{{1, 11}}
	tests := []struct {
		name string
		prof core.Profile
		want time.Duration
	}{
		{"llama formula", core.Profile{Kind: core.KindLlama}, LoadTimeoutFor(10*gb, shares)},
		{"zero kind means llama", core.Profile{}, LoadTimeoutFor(10*gb, shares)},
		{"profile override wins", core.Profile{Kind: core.KindLlama, LoadTimeout: 5 * time.Minute}, 5 * time.Minute},
		{"command uses command_load_timeout", core.Profile{Kind: core.KindCommand}, cmdDefault},
		{"command override wins", core.Profile{Kind: core.KindCommand, LoadTimeout: time.Minute}, time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveLoadTimeout(tc.prof, cmdDefault, 10*gb, shares); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestRemoteShares(t *testing.T) {
	links := map[core.NodeID]float64{"a": 11, "b": 22}
	link := func(id core.NodeID) float64 { return links[id] }
	pl := core.Placement{Nodes: []core.PlacedNode{
		{ID: "a", Addr: "x:1", BudgetMiB: 14000},
		{ID: "omen", Local: true, BudgetMiB: 7000},
		{ID: "b", Addr: "y:1", BudgetMiB: 7000},
	}}
	got := RemoteShares(core.Profile{}, pl, link)
	want := []NodeShare{{0.5, 11}, {0.25, 22}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if math.Abs(got[i].Fraction-want[i].Fraction) > 1e-9 || got[i].LinkMBs != want[i].LinkMBs {
			t.Fatalf("share %d = %v want %v", i, got[i], want[i])
		}
	}

	// the profile's explicit split wins over budgets
	got = RemoteShares(core.Profile{TensorSplit: []int{60, 0, 40}}, pl, link)
	if math.Abs(got[0].Fraction-0.6) > 1e-9 || math.Abs(got[1].Fraction-0.4) > 1e-9 {
		t.Fatalf("explicit split shares = %v", got)
	}

	// single remote node holds everything
	got = RemoteShares(core.Profile{}, core.Placement{Nodes: []core.PlacedNode{{ID: "a", Addr: "x:1"}}}, link)
	if len(got) != 1 || got[0].Fraction != 1 {
		t.Fatalf("single node shares = %v", got)
	}
}
