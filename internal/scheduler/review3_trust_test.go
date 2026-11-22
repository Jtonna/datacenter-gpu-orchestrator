package scheduler

import (
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/needstore"
)

// TS3 (third review): trust fails closed. A restored row is trusted only
// when the build recorded with it is both the host program's build and the
// build the served release's release.json names (the workers run the
// served release). A row without a build is stamped only when its own
// release's release.json names that same build; never by release name.
// When the host build or the served release's build is not known, nothing
// restored is trusted and nothing is stamped. The case: measured 11000,
// estimate 14000 (14700 with the margin): trusted means the 11000 is used.
func TestReview3_TS3_TrustFailsClosed(t *testing.T) {
	cases := []struct {
		name          string
		host          string            // the host program's build ("" : the check failed)
		manifests     map[string]string // release -> the build its release.json names
		build, rowRel string            // the stored row's
		trusted       bool
	}{
		{"same build everywhere", "aaaaaaa1", map[string]string{"0.1.6": "aaaaaaa1"}, "aaaaaaa1", "0.1.5", true},
		{"host check failed", "", map[string]string{"0.1.6": "aaaaaaa1"}, "aaaaaaa1", "0.1.5", false},
		{"served release.json missing", "aaaaaaa1", map[string]string{}, "aaaaaaa1", "0.1.5", false},
		{"workers run another build than the host", "aaaaaaa1", map[string]string{"0.1.6": "bbbbbbb2"}, "aaaaaaa1", "0.1.5", false},
		{"row of the workers' build only", "aaaaaaa1", map[string]string{"0.1.6": "bbbbbbb2"}, "bbbbbbb2", "0.1.5", false},
		{"no build, served release by name only", "aaaaaaa1", map[string]string{}, "", "0.1.6", false},
		{"no build, served release.json names it", "aaaaaaa1", map[string]string{"0.1.6": "aaaaaaa1"}, "", "0.1.6", true},
		{"no build, old release.json names it", "aaaaaaa1", map[string]string{"0.1.6": "aaaaaaa1", "0.1.4": "aaaaaaa1"}, "", "0.1.4", true},
		{"no build, old release.json missing", "aaaaaaa1", map[string]string{"0.1.6": "aaaaaaa1"}, "", "0.1.4", false},
		{"no build, host check failed", "", map[string]string{"0.1.6": "aaaaaaa1", "0.1.4": "aaaaaaa1"}, "", "0.1.4", false},
		{"no build, workers run another build", "aaaaaaa1", map[string]string{"0.1.6": "bbbbbbb2", "0.1.4": "aaaaaaa1"}, "", "0.1.4", false},
	}
	for _, tc := range cases {
		st := openNeedStore(t)
		h := tooBigEstimate(t, config.ForcedFitOff)
		p := h.cfg.Profiles["f"]
		if err := st.Put(needstore.Record{Schema: needstore.RecordSchema, Profile: "f", Kind: string(p.Kind), Model: p.Model, Ctx: p.Ctx,
			Parallel: 1, KVQuant: "f16", TotalMiB: 11000, Placement: []string{"a"}, Cause: needstore.CauseLoad, MeasuredAt: h.clk.Now(),
			Release: tc.rowRel, LlamaBuild: tc.build}); err != nil {
			t.Fatal(err)
		}
		h = restart(t, st, func(h *harness) {
			h.c.buildRelease, h.c.llamaBuild = "0.1.6", tc.host
			h.c.releaseBuild = func(rel string) string { return tc.manifests[rel] }
		})
		flushNeeds(h, st)
		if got := h.c.dryPlan("f").FitNeedMiB == 11000; got != tc.trusted {
			t.Errorf("%s: trusted %v, want %v", tc.name, got, tc.trusted)
		}
		recs, _, _ := st.All()
		if tc.build == "" {
			stamped := len(recs) == 1 && recs[0].LlamaBuild != ""
			if stamped != tc.trusted {
				t.Errorf("%s: stored %+v, stamped %v, want %v", tc.name, recs, stamped, tc.trusted)
			}
		}
	}
}
