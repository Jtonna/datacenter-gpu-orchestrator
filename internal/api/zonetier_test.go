package api

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgsvc"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/release"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/progtree"
)

// Each zone's tier (the one the scheduler assigned) and each tier's zones.

func ptrBool(b bool) *bool { return &b }

// liveTiers is shaped like live's four tiers, stored exactly as there.
func liveTiers() []config.TierRule {
	return []config.TierRule{
		{Match: config.Match{Platform: "linux-amd64-vulkan"}, Tier: core.Tier{Priority: 1, HeadroomMiB: 1000, SharedMemory: true, LinkMBs: 110, Settle: 3 * time.Second}},
		{Match: config.Match{Hostname: "alienware*"}, Tier: core.Tier{Priority: 2, HeadroomMiB: 500, LinkMBs: 110, Settle: 8 * time.Second}},
		{Match: config.Match{Local: ptrBool(true)}, Tier: core.Tier{Priority: 3, HeadroomMiB: 200, Settle: 2 * time.Second}},
		{Match: config.Match{Hostname: "laptop-*", Platform: "linux-amd64-cuda"}, Tier: core.Tier{Priority: 4, HeadroomMiB: 300}},
	}
}

// tierEnv is an agents-mode API with a local node and a release that has a
// vulkan and a cuda platform.
func tierEnv(t *testing.T, tiers []config.TierRule) (*env, *pool.Pool) {
	t.Helper()
	return tierEnvStored(t, tiers, true)
}

// tierEnvStored is tierEnv; store false leaves the configuration service
// without the tiers (an empty match cannot be stored: validate.go).
func tierEnvStored(t *testing.T, tiers []config.TierRule, store bool) (*env, *pool.Pool) {
	t.Helper()
	root := progtree.Write(t, t.TempDir(), progtree.Tree{Release: "0.1.0", Llama: "a01665d30",
		Folders: map[string]map[string][]byte{
			"agent":              {"potluck-agent-linux-amd64": []byte("AGENT-BINARY")},
			"linux-amd64-vulkan": {"llama-server": []byte("L"), "rpc-server": []byte("RPC-SERVER")},
			"linux-amd64-cuda":   {"llama-server": []byte("LC"), "rpc-server": []byte("RPC-CUDA")},
		}})
	rel, err := release.Load(root, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	d := config.DefaultDefaults()
	d.Membership = config.MembershipAgents
	cfg := &config.Config{Defaults: d, Fleet: config.Fleet{LocalNode: "host-1"}, LocalProbeCmd: "probe", Tiers: tiers}
	clk := &feedClock{t: time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)}
	p, err := pool.New(pool.Options{Config: func() *config.Config { return cfg }, Programs: pool.Programs{Release: rel}, PotluckRelease: "0.1.0",
		Clock: clk.now, RunProbe: func(context.Context, string) ([]byte, error) { return []byte("24576, 1024\n"), nil },
		HostInfo: func() (float64, string, bool) { return 5000, "fedcba9876543210fedcba9876543210", true }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go p.Run(ctx)
	// The configuration service holds the same tiers (GET /api/config/tiers).
	st, err := bench.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cs, err := cfgstore.Open(st)
	if err != nil {
		t.Fatal(err)
	}
	svc := cfgsvc.New(context.Background(), cfgsvc.Options{Store: cs, Parse: config.Options{FileExists: func(string) bool { return true }}})
	e := newEnv(t, chatOK, func(o *Options) { o.Pool, o.Config, o.Audit = p, svc, cs })
	var specs []config.TierSpec
	for _, tr := range tiers {
		b, _ := json.Marshal(map[string]any{"match": pool.TierRuleOf(tr.Match), "priority": tr.Priority, "headroom_mib": tr.HeadroomMiB,
			"shared_memory": tr.SharedMemory, "link_mb_s": tr.LinkMBs, "settle": tr.Settle.String()})
		var sp config.TierSpec
		if err := json.Unmarshal(b, &sp); err != nil {
			t.Fatal(err)
		}
		specs = append(specs, sp)
	}
	if !store {
		return e, p
	}
	if _, err := svc.SetTiers(context.Background(), cfgsvc.WriteOpts{Origin: audit.Origin{Source: "rest", Actor: "test", Comment: "tiers"}}, specs); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := p.Local(); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no local node")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return e, p
}

func registerBackend(t *testing.T, e *env, id, name, backend string, n int) {
	t.Helper()
	req := agentproto.RegisterRequest{ID: id, Name: name, AgentRelease: "0.1.0", AgentStartID: "s-" + id, OS: "linux", Arch: "amd64",
		Install: "bare", Addr: fmt.Sprintf("192.0.2.%d", n), Backends: []string{backend}}
	if backend == "cuda" {
		req.GPUs = []agentproto.GPU{{Vendor: agentproto.VendorNvidia, Model: "GPU-model"}}
	}
	req.FillEmpty()
	b, _ := json.Marshal(req)
	resp := e.post(agentproto.PathRegister, string(b))
	out := body(t, resp)
	var a agentproto.RegisterAnswer
	json.Unmarshal([]byte(out), &a)
	if resp.StatusCode != 200 || !a.Accepted {
		t.Fatalf("register %s: %d %s", name, resp.StatusCode, out)
	}
}

type zoneTierEntry struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	State      string         `json:"state"`
	Local      bool           `json:"local"`
	Tier       map[string]any `json:"tier"`
	TierReason string         `json:"tier_reason"`
}

func zoneTiers(t *testing.T, e *env) (feed, status map[string]zoneTierEntry) {
	t.Helper()
	feed, status = map[string]zoneTierEntry{}, map[string]zoneTierEntry{}
	for _, raw := range readFeed(t, e).Nodes {
		var z zoneTierEntry
		json.Unmarshal(raw, &z)
		feed[z.Name] = z
	}
	var st struct{ Agents []zoneTierEntry }
	if err := json.Unmarshal([]byte(body(t, e.get("/api/status"))), &st); err != nil {
		t.Fatal(err)
	}
	for _, z := range st.Agents {
		status[z.Name] = z
	}
	return feed, status
}

type tierZonesAnswer struct {
	Tiers     []json.RawMessage `json:"tiers"`
	TierZones []struct {
		Index        int  `json:"index"`
		NeverReached bool `json:"never_reached"`
		Zones        []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"zones"`
	} `json:"tier_zones"`
}

func tierZones(t *testing.T, e *env) tierZonesAnswer {
	t.Helper()
	resp := e.get("/api/config/tiers")
	b := body(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/config/tiers: %d %s", resp.StatusCode, b)
	}
	var a tierZonesAnswer
	if err := json.Unmarshal([]byte(b), &a); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestZoneTier_LiveShapedTiers(t *testing.T) {
	e, _ := tierEnv(t, liveTiers())
	for i := 1; i <= 6; i++ {
		registerBackend(t, e, fmt.Sprintf("02:00:00:00:00:0%d", i), fmt.Sprintf("console-%d", i), "vulkan", 100+i)
	}
	registerBackend(t, e, "02:00:00:00:00:11", "alienware-1", "cuda", 111)
	registerBackend(t, e, "02:00:00:00:00:12", "server-9", "cuda", 112) // fits no tier
	feed, status := zoneTiers(t, e)

	want := map[string]int{"console-1": 1, "console-2": 1, "console-3": 1, "console-4": 1, "console-5": 1, "console-6": 1, "alienware-1": 2, "host-1": 3}
	for name, idx := range want {
		z, ok := feed[name]
		if !ok || z.Tier == nil || int(z.Tier["index"].(float64)) != idx || z.TierReason != "" {
			t.Fatalf("%s: %+v, want tier %d", name, z, idx)
		}
	}
	c := feed["console-1"].Tier
	rule, _ := c["rule"].(map[string]any)
	if len(rule) != 1 || rule["platform"] != "linux-amd64-vulkan" || c["priority"].(float64) != 1 || c["headroom_mib"].(float64) != 1000 ||
		c["shared_memory"] != true || c["link_mb_s"].(float64) != 110 || c["settle"] != "3s" {
		t.Fatalf("console-1 tier: %v", c)
	}
	if r, _ := feed["host-1"].Tier["rule"].(map[string]any); len(r) != 1 || r["local"] != true || !feed["host-1"].Local {
		t.Fatalf("host-1 tier: %v", feed["host-1"])
	}
	if r, _ := feed["alienware-1"].Tier["rule"].(map[string]any); len(r) != 1 || r["hostname"] != "alienware*" {
		t.Fatalf("alienware-1 tier: %v", feed["alienware-1"].Tier)
	}
	if z := feed["server-9"]; z.Tier != nil || z.TierReason != "no tier matches" {
		t.Fatalf("server-9: %+v", z)
	}
	// /api/status agents carry the same tier (one function for both).
	for name, z := range status {
		f, _ := json.Marshal(feed[name].Tier)
		s, _ := json.Marshal(z.Tier)
		if string(f) != string(s) || feed[name].TierReason != z.TierReason {
			t.Fatalf("%s: feed %s status %s", name, f, s)
		}
	}

	a := tierZones(t, e)
	if len(a.TierZones) != 4 || len(a.Tiers) != 4 {
		t.Fatalf("tier_zones %+v", a)
	}
	names := func(i int) []string {
		var out []string
		for _, z := range a.TierZones[i].Zones {
			out = append(out, z.Name)
		}
		return out
	}
	if got := names(0); fmt.Sprint(got) != "[console-1 console-2 console-3 console-4 console-5 console-6]" {
		t.Fatalf("tier 1 zones %v", got)
	}
	if got := names(1); fmt.Sprint(got) != "[alienware-1]" || names(2)[0] != "host-1" || len(names(2)) != 1 || len(names(3)) != 0 {
		t.Fatalf("tiers 2-4: %v %v %v", got, names(2), names(3))
	}
	for i, tz := range a.TierZones {
		if tz.Index != i+1 || tz.NeverReached || tz.Zones == nil && i != 3 {
			t.Fatalf("tier %d: %+v", i+1, tz)
		}
	}
	if string(a.Tiers[0]) == "" || a.TierZones[0].Zones[0].ID != "02:00:00:00:00:01" {
		t.Fatalf("ids: %+v", a.TierZones[0])
	}

	// A removed zone carries no tier.
	if code, out, _ := e.do("DELETE", "/api/nodes/02:00:00:00:00:06", `{"comment":"gone"}`); code != 200 {
		t.Fatalf("remove: %d %s", code, out)
	}
	feed, status = zoneTiers(t, e)
	var removed []zoneTierEntry
	for _, m := range []map[string]zoneTierEntry{feed, status} {
		for _, z := range m {
			if z.ID == "02:00:00:00:00:06" {
				removed = append(removed, z)
			}
		}
	}
	if len(removed) != 2 {
		t.Fatalf("removed zone listed %d times", len(removed))
	}
	for _, z := range removed {
		if z.State != "removed" || z.Tier != nil || z.TierReason != "" {
			t.Fatalf("removed: %+v", z)
		}
	}
	if got := names(0); len(tierZones(t, e).TierZones[0].Zones) != 5 {
		t.Fatalf("tier 1 after the removal: %v", got)
	}
}

// A tier with an empty match takes every zone; every tier below it is
// never reached. The configuration refuses to store an empty match
// (validate.go), so the tier list is handed to tierZones directly here; the
// zones' own tiers come from the node view as always.
func TestZoneTier_EmptyMatchFirst(t *testing.T) {
	tiers := append([]config.TierRule{{Match: config.Match{}, Tier: core.Tier{Priority: 1}}}, liveTiers()...)
	e, _ := tierEnvStored(t, tiers, false)
	registerBackend(t, e, "02:00:00:00:00:01", "console-1", "vulkan", 101)
	registerBackend(t, e, "02:00:00:00:00:11", "alienware-1", "cuda", 111)
	feed, _ := zoneTiers(t, e)
	for _, n := range []string{"console-1", "alienware-1", "host-1"} {
		if z := feed[n]; z.Tier == nil || z.Tier["index"].(float64) != 1 {
			t.Fatalf("%s: %+v", n, z)
		}
		if r, _ := feed[n].Tier["rule"].(map[string]any); len(r) != 0 {
			t.Fatalf("%s rule %v", n, r)
		}
	}
	specs := []config.TierSpec{{Priority: 1}, {Match: config.MatchSpec{Platform: "linux-amd64-vulkan"}, Priority: 1},
		{Match: config.MatchSpec{Hostname: "alienware*"}, Priority: 2}, {Match: config.MatchSpec{Local: ptrBool(true)}, Priority: 3},
		{Match: config.MatchSpec{Hostname: "laptop-*", Platform: "linux-amd64-cuda"}, Priority: 4}}
	var a tierZonesAnswer
	b, _ := json.Marshal(e.api.tierZones(context.Background(), specs))
	json.Unmarshal(b, &a.TierZones)
	if len(a.TierZones) != 5 || a.TierZones[0].NeverReached || len(a.TierZones[0].Zones) != 3 {
		t.Fatalf("tier 1: %+v", a.TierZones)
	}
	for _, tz := range a.TierZones[1:] {
		if !tz.NeverReached || len(tz.Zones) != 0 {
			t.Fatalf("tier %d: %+v", tz.Index, tz)
		}
	}
	// The live shape has no empty match: nothing is never reached.
	e2, _ := tierEnv(t, liveTiers())
	for _, tz := range tierZones(t, e2).TierZones {
		if tz.NeverReached {
			t.Fatalf("tier %d never reached", tz.Index)
		}
	}
}
