package scheduler

import (
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// SS2: a restored measurement is not trusted across a change of the llama
// build: until a load of this run measures it again, the need is
// max(estimate, stored). Without a build id on either side the release
// decides.
func TestReview2_SS2_AnotherBuildsMeasurementIsNotTrusted(t *testing.T) {
	cases := []struct {
		name           string
		build, release string // the stored row's
		total          int
		trusted        bool
	}{
		{"same build", "aaaaaaa1", "0.1.5", 7000, true},
		{"same build, abbreviated", "aaaaaaa1f00", "0.1.5", 7000, true},
		{"another build", "ccccccc3", "0.1.6", 7000, false},
		{"another build, higher than the estimate", "ccccccc3", "0.1.6", 10000, true}, // max(estimate, stored) is the stored
		{"no build, same release", "", "0.1.6", 7000, false},                          // trusted only once stamped through release.json (TS3)
		{"no build, another release", "", "0.1.5", 7000, false},
		{"nothing known", "", "", 7000, false},
	}
	for _, tc := range cases {
		h := oneAndThree(t)
		h.c.llamaBuild, h.c.buildRelease, h.c.servedBuild = "aaaaaaa1", "0.1.6", "aaaaaaa1"
		p := h.c.cfg.Profiles["one"]
		h.c.putLearned(&learnedNeed{TotalMiB: tc.total, key: needKeyOf(p), profile: "one", devices: 1, restored: true,
			at: h.clk.Now(), build: tc.build, release: tc.release})
		info, _ := h.c.needAt(p, 1)
		if used := info.Need == placer.Measured(tc.total); used != tc.trusted {
			t.Errorf("%s: need %+v, stored %d used %v, want %v", tc.name, info.Need, tc.total, used, tc.trusted)
		}
		if !tc.trusted && info.Source != NeedEstimated {
			t.Errorf("%s: source %s, want the estimate", tc.name, info.Source)
		}
	}
	// a load of this run measures it again: trusted
	h := oneAndThree(t)
	h.c.llamaBuild = "aaaaaaa1"
	p := h.c.cfg.Profiles["one"]
	h.c.putLearned(&learnedNeed{TotalMiB: 7000, key: needKeyOf(p), profile: "one", devices: 1, restored: true, at: h.clk.Now(), build: "ccccccc3"})
	h.request("one")
	setLoad(h, "one", rpcDev("192.0.2.2:50052", 6800, 400, 400)) // 7600
	h.healthy("one")
	if info, _ := h.c.needAt(p, 1); info.Need != placer.Measured(7600) {
		t.Fatalf("after a load of this run: %+v, want 7600", info.Need)
	}
	if l := h.c.learnedAt(p, 1); l.build != "aaaaaaa1" {
		t.Fatalf("the new measurement's build %q", l.build)
	}
}

// SS2: the listing of every stored measurement, and forgetting one shape.
func TestReview2_SS2_ListAndForgetByShape(t *testing.T) {
	h := oneAndThree(t)
	h.c.llamaBuild = "aaaaaaa1"
	one, three := h.c.cfg.Profiles["one"], h.c.cfg.Profiles["three"]
	h.c.putLearned(&learnedNeed{TotalMiB: 7600, key: needKeyOf(one), profile: "one", devices: 1, at: h.clk.Now(), build: "aaaaaaa1"})
	h.c.putLearned(&learnedNeed{TotalMiB: 11000, key: needKeyOf(three), profile: "three", devices: 3, restored: true, at: h.clk.Now(), build: "ccccccc3"})
	other := h.llama("other", 9000)
	h.c.putLearned(&learnedNeed{TotalMiB: 9100, key: needKeyOf(other), profile: "other", devices: 1, at: h.clk.Now()})
	rows := h.c.learnedRows()
	if len(rows) != 3 {
		t.Fatalf("rows %+v", rows)
	}
	var shared []LearnedRow
	for _, r := range rows {
		if r.Shape == shapeOf(one) {
			shared = append(shared, r)
		}
	}
	if len(shared) != 2 || shared[0].Devices != 1 || shared[1].Devices != 3 || shared[1].Trusted || !shared[0].Trusted ||
		shared[1].LlamaBuild != "ccccccc3" || len(shared[0].Profiles) != 2 || shared[0].Model != "shared.gguf" {
		t.Fatalf("rows of the shared shape %+v", shared)
	}
	if had := h.c.forgetShape(shapeOf(one)); !had {
		t.Fatal("forget by shape found nothing")
	}
	if rows := h.c.learnedRows(); len(rows) != 1 || rows[0].Profiles[0] != "other" {
		t.Fatalf("after forgetting the shape %+v", rows)
	}
	if h.c.forgetShape("absent") {
		t.Fatal("an absent shape was forgotten")
	}
}

// SS2: the llama build is stored with the measurement and restored.
func TestReview2_SS2_TheBuildIsStored(t *testing.T) {
	st := openNeedStore(t)
	h := tooBigEstimate(t, "off")
	h.c.llamaBuild = "aaaaaaa1"
	h.c.restoreNeeds(st)
	h.request("f")
	setLoad(h, "f", rpcDev(addrA, 10200, 500, 300))
	h.healthy("f")
	flushNeeds(h, st)
	recs, _, _ := st.All()
	if len(recs) != 1 || recs[0].LlamaBuild != "aaaaaaa1" || recs[0].Devices != 1 {
		t.Fatalf("stored %+v", recs)
	}
	h2 := restart(t, st)
	if l := h2.c.learnedAt(h2.c.cfg.Profiles["f"], 1); l == nil || l.build != "aaaaaaa1" {
		t.Fatalf("restored %+v", l)
	}
	_ = core.KindLlama
}
