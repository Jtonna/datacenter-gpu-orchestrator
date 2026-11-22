package scheduler

import (
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// The third review, TB1 and TS1, through the scheduler: needAt and the
// placer request's need for each device count.

// TB1: an untrusted row on exactly 3 devices (another llama build) does not
// switch off the raise of a trusted 1-device row, so it does not lower the
// need for 3 devices.
func TestReview3_TB1_AnUntrustedExactRowDoesNotLower(t *testing.T) {
	h := oneAndThree(t) // estimate 8000 (+5 %), 1000 MiB per device
	h.c.llamaBuild, h.c.buildRelease = "aaaaaaa1", "0.1.6"
	p := h.c.cfg.Profiles["three"]
	h.c.putLearned(&learnedNeed{TotalMiB: 11800, key: needKeyOf(p), profile: "one", devices: 1, at: h.clk.Now()})
	want := 11800 + 2*1000
	check := func(when string) {
		t.Helper()
		if info, _ := h.c.needAt(p, 3); info.Need != placer.Measured(want) {
			t.Fatalf("%s: needAt 3: %+v, want measured %d", when, info.Need, want)
		}
		req, _, err := h.c.request(p)
		if err != nil || len(req.ByCount) <= 3 || req.ByCount[3].Need != placer.Measured(want) {
			t.Fatalf("%s: the placer's need on 3 devices: %+v (%v), want measured %d", when, req.ByCount, err, want)
		}
	}
	check("trusted 1-device row only")
	h.c.putLearned(&learnedNeed{TotalMiB: 8000, key: needKeyOf(p), profile: "three", devices: 3, restored: true,
		at: h.clk.Now(), build: "ccccccc3", release: "0.1.5"})
	check("plus an untrusted 3-device row of 8000")
}

// TS1: a count between two measured counts is not below the larger one's
// total less the per-device cost of the missing devices.
func TestReview3_TS1_TheLargerCountBounds(t *testing.T) {
	h := oneAndThree(t)
	p := h.c.cfg.Profiles["three"]
	h.c.putLearned(&learnedNeed{TotalMiB: 9000, key: needKeyOf(p), profile: "one", devices: 1, at: h.clk.Now()})
	h.c.putLearned(&learnedNeed{TotalMiB: 12000, key: needKeyOf(p), profile: "three", devices: 3, at: h.clk.Now()})
	if info, _ := h.c.needAt(p, 2); info.Need != placer.Measured(12000-1000) {
		t.Fatalf("2 devices between 9000 on 1 and 12000 on 3: %+v, want 11000", info.Need)
	}
}
