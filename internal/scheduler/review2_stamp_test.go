package scheduler

import (
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/needstore"
)

// A row written before llama builds were recorded (0.1.5 and earlier) has
// only the release it was measured with. At restore it is stamped with the
// current llama build, and trusted, when that release shipped the same
// llama build: the release served now, or an older release whose
// release.json says so (the release bump to 0.1.6 keeps the build). Any
// other row stays untrusted (the estimate where it is higher). The case:
// measured 11000 MiB, estimate 14000 (14700 with the margin) on a 12000 MiB
// node.
func TestReview2_SS2_RowsWithoutABuildAreStampedWhenTheReleaseHadThisBuild(t *testing.T) {
	builds := map[string]string{"0.1.4": "aaaaaaa1f00d", "0.1.3": "ccccccc3"} // what each older release's release.json says
	cases := []struct {
		release string
		trusted bool
	}{
		{"0.1.6", true},  // the release served now
		{"0.1.4", true},  // an older release with the same llama build (the 0.1.6 rollout)
		{"0.1.3", false}, // an older release with another build
		{"0.1.2", false}, // a release whose build is not known
		{"", false},      // no release recorded
	}
	for _, tc := range cases {
		st := openNeedStore(t)
		h := tooBigEstimate(t, config.ForcedFitOff)
		p := h.cfg.Profiles["f"]
		if err := st.Put(needstore.Record{Schema: needstore.RecordSchema, Profile: "f", Kind: string(p.Kind), Model: p.Model, Ctx: p.Ctx,
			Parallel: 1, KVQuant: "f16", TotalMiB: 11000, Placement: []string{"a"}, Cause: needstore.CauseLoad, MeasuredAt: h.clk.Now(),
			Release: tc.release}); err != nil {
			t.Fatal(err)
		}
		h = restart(t, st, func(h *harness) {
			h.c.buildRelease, h.c.llamaBuild = "0.1.6", "aaaaaaa1"
			h.c.releaseBuild = func(rel string) string { return builds[rel] }
		})
		flushNeeds(h, st)
		r := h.c.dryPlan("f")
		if got := r.FitNeedMiB == 11000; got != tc.trusted {
			t.Errorf("release %q: need %d, trusted %v, want %v", tc.release, r.FitNeedMiB, got, tc.trusted)
		}
		recs, _, _ := st.All()
		stamped := len(recs) == 1 && recs[0].LlamaBuild == "aaaaaaa1"
		if stamped != tc.trusted {
			t.Errorf("release %q: stored %+v, stamped %v, want %v", tc.release, recs, stamped, tc.trusted)
		}
		if tc.trusted {
			h.mustLog("stored memory needs stamped with the current llama build", "stamped", 1)
		}
	}
}
