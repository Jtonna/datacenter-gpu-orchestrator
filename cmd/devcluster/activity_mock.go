package main

// GET /api/metrics/activity in the scripted scenarios: the shape of
// snapshot() in internal/api/activity.go, oldest first, for the profiles
// that are loaded (ready, draining) in the scenario.

import (
	"hash/fnv"
	"math"
	"math/rand"
	"net/http"
	"time"
)

func (s *scenarioState) handleActivity(w http.ResponseWriter, r *http.Request) bool {
	sc, _ := s.get()
	b := s.scripted()
	if b == nil {
		return false
	}
	s.mu.Lock()
	since := s.since
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"data": activityFor(sc.Name, b, since)})
	return true
}

// activityFor is stable while a scenario stays selected: it is anchored to
// the moment of selection and seeded by the scenario name.
func activityFor(name string, b *statusBuilder, since time.Time) []any {
	out := []any{}
	var loaded []string
	for _, in := range b.st.Instances {
		if in.State == "ready" || in.State == "draining" {
			loaded = append(loaded, in.Profile)
		}
	}
	if len(loaded) == 0 || name == "bare" {
		return out
	}
	h := fnv.New64a()
	h.Write([]byte(name))
	rnd := rand.New(rand.NewSource(int64(h.Sum64())))
	n := 12 + rnd.Intn(9)
	bad := rnd.Intn(n)
	end := since.Add(-20 * time.Second)
	t := end.Add(-29 * time.Minute)
	step := 29 * time.Minute / time.Duration(n)
	for i := 0; i < n; i++ {
		t = t.Add(step/2 + time.Duration(rnd.Int63n(int64(step))))
		if t.After(end) {
			t = end.Add(-time.Duration(n-i) * time.Second)
		}
		prof := loaded[rnd.Intn(len(loaded))]
		tps := perf[prof].decode
		if tps == 0 {
			tps = map[string]float64{"qwen3.8-27b": 6.4, "devstral-24b": 7.9, "kimi-dev-72b-iq2s": 3.1, "gemma-4-12b:48k": 13.1}[prof]
		}
		if tps == 0 {
			tps = 12
		}
		tps = math.Round(tps*(0.92+rnd.Float64()*0.16)*10) / 10
		in, outN := 200+rnd.Intn(2400), 60+rnd.Intn(700)
		dur := int64(float64(outN)/tps*1000) + int64(in/2) + int64(rnd.Intn(400))
		status, tok := 200, map[string]any{"input_tokens": in, "output_tokens": outN, "tokens_per_second": tps}
		if i == bad {
			status, dur = 502, int64(1500+rnd.Intn(3000))
			tok = map[string]any{"input_tokens": -1, "output_tokens": -1, "tokens_per_second": -1}
		}
		out = append(out, map[string]any{"id": i + 1, "timestamp": t.UTC().Format("2006-01-02T15:04:05.000Z07:00"), "model": prof,
			"req_path": "/v1/chat/completions", "resp_status_code": status, "duration_ms": dur, "tokens": tok})
	}
	return out
}
