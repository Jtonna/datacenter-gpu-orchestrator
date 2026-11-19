package benchspec

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func profiles(ids ...string) func(string) bool {
	return func(id string) bool {
		for _, p := range ids {
			if p == id {
				return true
			}
		}
		return false
	}
}

func mustBench(t *testing.T, js string) *Benchmark {
	t.Helper()
	b, err := ParseBenchmarkJSON([]byte(js))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return b
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.ParseInLocation("2006-01-02T15:04:05", s, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

const okBench = `{"id":"gemma-cold","name":"Gemma cold","profile":"gemma-4-12b",
 "requests":[{"name":"short","prompt":"Say hi.","max_tokens":16}]}`

func TestBenchmarkDefaults(t *testing.T) {
	b := mustBench(t, okBench)
	if err := b.Validate(profiles("gemma-4-12b")); err != nil {
		t.Fatal(err)
	}
	if !b.IsCold() || !b.UnloadsAfter() || b.RepeatCount() != 1 || b.Warmups() != 1 || b.Timeout().Seconds() != 600 {
		t.Fatalf("defaults: cold=%v unload=%v repeats=%d warmup=%d timeout=%v", b.IsCold(), b.UnloadsAfter(), b.RepeatCount(), b.Warmups(), b.Timeout())
	}
	r := b.Requests[0]
	if r.Temp() != 0 || r.SeedVal() != 42 || r.CachesPrompt() || r.Streams() || !r.Deterministic() {
		t.Fatalf("request defaults: %+v", r)
	}
}

func TestBenchmarkRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"bad id":             `{"id":"Bad Id","profile":"p","requests":[{"prompt":"x","max_tokens":1}]}`,
		"no id":              `{"profile":"p","requests":[{"prompt":"x","max_tokens":1}]}`,
		"unknown profile":    `{"id":"a","profile":"nope","requests":[{"prompt":"x","max_tokens":1}]}`,
		"no profile":         `{"id":"a","requests":[{"prompt":"x","max_tokens":1}]}`,
		"no requests":        `{"id":"a","profile":"p"}`,
		"both prompt+msgs":   `{"id":"a","profile":"p","requests":[{"prompt":"x","messages":[{"role":"user","content":"y"}],"max_tokens":1}]}`,
		"neither":            `{"id":"a","profile":"p","requests":[{"max_tokens":1}]}`,
		"bad role":           `{"id":"a","profile":"p","requests":[{"messages":[{"role":"robot","content":"y"}],"max_tokens":1}]}`,
		"negative max":       `{"id":"a","profile":"p","requests":[{"prompt":"x","max_tokens":-1}]}`,
		"temperature":        `{"id":"a","profile":"p","requests":[{"prompt":"x","max_tokens":1,"temperature":9}]}`,
		"repeats":            `{"id":"a","profile":"p","repeats":1000,"requests":[{"prompt":"x","max_tokens":1}]}`,
		"warmup":             `{"id":"a","profile":"p","warmup_requests":-1,"requests":[{"prompt":"x","max_tokens":1}]}`,
		"outcome":            `{"id":"a","profile":"p","expect":{"outcome":"maybe"},"requests":[{"prompt":"x","max_tokens":1}]}`,
		"sha":                `{"id":"a","profile":"p","expect":{"output_sha256":"abc"},"requests":[{"prompt":"x","max_tokens":1}]}`,
		"parallel + profile": `{"id":"a","profile":"p","parallel":["x","y"]}`,
		"parallel one":       `{"id":"a","parallel":["x"]}`,
		"parallel dup":       `{"id":"a","parallel":["x","x"]}`,
		"parallel self":      `{"id":"a","parallel":["a","y"]}`,
	}
	for name, js := range cases {
		b, err := ParseBenchmarkJSON([]byte(js))
		if err == nil {
			err = b.Validate(profiles("p"))
		}
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ParseBenchmarkJSON([]byte(`{"id":"a","profile":"p","surprise":1}`)); err == nil || !strings.Contains(err.Error(), "surprise") {
		t.Errorf("unknown field not rejected: %v", err)
	}
}

func TestParallelBenchmarkIsValid(t *testing.T) {
	b := mustBench(t, `{"id":"two-at-once","parallel":["a","b"]}`)
	if err := b.Validate(profiles()); err != nil {
		t.Fatal(err)
	}
	if !b.IsParallel() {
		t.Fatal("not parallel")
	}
}

func TestExpectRefusedNeedsNoMeasurements(t *testing.T) {
	b := mustBench(t, `{"id":"too-big","profile":"p","expect":{"outcome":"refused"},"requests":[{"prompt":"x","max_tokens":1}]}`)
	if err := b.Validate(profiles("p")); err != nil {
		t.Fatal(err)
	}
	if b.Expect.Outcome != "refused" {
		t.Fatalf("%+v", b.Expect)
	}
	b2 := mustBench(t, `{"id":"too-big","profile":"p","expect":{"outcome":"refused","min_decode_tps":5},"requests":[{"prompt":"x","max_tokens":1}]}`)
	if err := b2.Validate(profiles("p")); err == nil {
		t.Fatal("min_decode_tps with outcome refused should be rejected")
	}
}

func TestSuiteJSONAndRefs(t *testing.T) {
	js := `{"id":"nightly","name":"Nightly","benchmarks":["a",{"profile":"p","requests":[{"prompt":"x","max_tokens":1}]}],
	  "cooldown_s":2,"schedule":{"at":"03:00"}}`
	s, err := ParseSuiteJSON([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]*Benchmark{"a": mustBench(t, `{"id":"a","profile":"p","requests":[{"prompt":"x","max_tokens":1}]}`)}
	if err := s.Validate(profiles("p"), func(id string) (*Benchmark, bool) { b, ok := known[id]; return b, ok }); err != nil {
		t.Fatal(err)
	}
	if s.Benchmarks[1].Inline == nil || s.Benchmarks[1].Inline.ID != "nightly-2" {
		t.Fatalf("inline benchmark id: %+v", s.Benchmarks[1].Inline)
	}
	if s.Cooldown().Seconds() != 2 || !s.WaitsForIdle() || s.IdleWait().Seconds() != 600 || s.StopOnFailure {
		t.Fatalf("options: %+v", s)
	}
	out, _ := MarshalSuite(s)
	if !strings.Contains(string(out), `"a"`) || !strings.Contains(string(out), `"nightly-2"`) {
		t.Fatalf("marshal: %s", out)
	}
}

func TestSuiteRejectsBadInput(t *testing.T) {
	look := func(id string) (*Benchmark, bool) {
		if id == "a" {
			b := mustBench(t, `{"id":"a","profile":"p","requests":[{"prompt":"x","max_tokens":1}]}`)
			return b, true
		}
		return nil, false
	}
	for name, js := range map[string]string{
		"empty":         `{"id":"s","benchmarks":[]}`,
		"unknown ref":   `{"id":"s","benchmarks":["zzz"]}`,
		"bad schedule":  `{"id":"s","benchmarks":["a"],"schedule":{"at":"25:99"}}`,
		"both schedule": `{"id":"s","benchmarks":["a"],"schedule":{"at":"03:00","every":"24h"}}`,
		"short every":   `{"id":"s","benchmarks":["a"],"schedule":{"every":"5s"}}`,
		"neg cooldown":  `{"id":"s","benchmarks":["a"],"cooldown_s":-1}`,
		"bad id":        `{"id":"S S","benchmarks":["a"]}`,
		"unknown field": `{"id":"s","benchmarks":["a"],"foo":1}`,
	} {
		s, err := ParseSuiteJSON([]byte(js))
		if err == nil {
			err = s.Validate(profiles("p"), look)
		}
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestYAMLShapeMatchesJSONShape(t *testing.T) {
	src := `
id: nightly
name: Nightly
benchmarks:
  - a
  - profile: p
    requests:
      - prompt: hello
        max_tokens: 8
        stream: true
stop_on_failure: true
schedule: {every: 24h}
`
	var s Suite
	if err := yaml.Unmarshal([]byte(src), &s); err != nil {
		t.Fatal(err)
	}
	if s.ID != "nightly" || len(s.Benchmarks) != 2 || s.Benchmarks[0].Ref != "a" || s.Benchmarks[1].Inline == nil ||
		!s.Benchmarks[1].Inline.Requests[0].Streams() || !s.StopOnFailure || s.Schedule.Every != "24h" {
		t.Fatalf("%+v", s)
	}
}

func TestScheduleNext(t *testing.T) {
	sc := &Schedule{At: "03:00"}
	from := mustTime(t, "2026-11-18T10:00:00")
	next, ok := sc.Next(from, time.Time{})
	if !ok || next.Format("2006-01-02T15:04") != "2026-11-19T03:00" {
		t.Fatalf("at next: %v", next)
	}
	from = mustTime(t, "2026-11-18T01:00:00")
	next, _ = sc.Next(from, time.Time{})
	if next.Format("2006-01-02T15:04") != "2026-11-18T03:00" {
		t.Fatalf("at same day: %v", next)
	}
	ev := &Schedule{Every: "24h"}
	last := mustTime(t, "2026-11-18T10:00:00")
	next, _ = ev.Next(mustTime(t, "2026-11-18T12:00:00"), last)
	if next.Format("2006-01-02T15:04") != "2026-11-19T10:00" {
		t.Fatalf("every next: %v", next)
	}
	next, _ = ev.Next(mustTime(t, "2026-11-18T12:00:00"), time.Time{})
	if next.Format("2006-01-02T15:04") != "2026-11-19T12:00" {
		t.Fatalf("every first: %v", next)
	}
}
