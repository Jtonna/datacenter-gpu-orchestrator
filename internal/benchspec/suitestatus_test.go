package benchspec

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A scenario that cannot be run yet is a suite with status planned or blocked
// and a reason; it may list no benchmark at all. A normal suite still needs at
// least one.
func TestSuiteStatusPlannedOrBlocked(t *testing.T) {
	look := func(id string) (*Benchmark, bool) {
		if id == "a" {
			return mustBench(t, `{"id":"a","profile":"p","requests":[{"prompt":"x","max_tokens":1}]}`), true
		}
		return nil, false
	}
	good := map[string]string{
		"planned, no benchmark":   `{"id":"s","status":"planned","reason":"needs a fifth console","benchmarks":[]}`,
		"blocked, no benchmarks":  `{"id":"s","status":"blocked","reason":"the 2.5 GbE switch is not here yet"}`,
		"planned with benchmarks": `{"id":"s","status":"planned","reason":"later","benchmarks":["a"]}`,
		"normal":                  `{"id":"s","benchmarks":["a"]}`,
	}
	for name, js := range good {
		s, err := ParseSuiteJSON([]byte(js))
		if err == nil {
			err = s.Validate(profiles("p"), look)
		}
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := map[string]string{
		"normal, no benchmark":   `{"id":"s","benchmarks":[]}`,
		"normal, no list":        `{"id":"s"}`,
		"planned without reason": `{"id":"s","status":"planned","benchmarks":[]}`,
		"blank reason":           `{"id":"s","status":"blocked","reason":"   "}`,
		"unknown status":         `{"id":"s","status":"later","reason":"x","benchmarks":["a"]}`,
		"reason without status":  `{"id":"s","reason":"x","benchmarks":["a"]}`,
		"planned with schedule":  `{"id":"s","status":"planned","reason":"x","schedule":{"at":"03:00"}}`,
		"reason too long":        `{"id":"s","status":"planned","reason":"` + strings.Repeat("x", 501) + `"}`,
	}
	for name, js := range bad {
		s, err := ParseSuiteJSON([]byte(js))
		if err == nil {
			err = s.Validate(profiles("p"), look)
		}
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	s, _ := ParseSuiteJSON([]byte(good["planned, no benchmark"]))
	if s.Runnable() {
		t.Fatal("a planned suite is runnable")
	}
	n, _ := ParseSuiteJSON([]byte(good["normal"]))
	if !n.Runnable() {
		t.Fatal("a normal suite is not runnable")
	}
}

// Status and reason survive JSON and YAML (import and export of a config).
func TestSuiteStatusRoundTrips(t *testing.T) {
	s := &Suite{ID: "s", Status: SuiteBlocked, Reason: "no switch", Benchmarks: []SuiteItem{}}
	b, _ := json.Marshal(s)
	if !strings.Contains(string(b), `"status":"blocked"`) || !strings.Contains(string(b), `"reason":"no switch"`) {
		t.Fatalf("%s", b)
	}
	back, err := ParseSuiteJSON(b)
	if err != nil || back.Status != SuiteBlocked || back.Reason != "no switch" {
		t.Fatalf("%+v %v", back, err)
	}
	y, _ := yaml.Marshal(s)
	var fromY Suite
	if err := yaml.Unmarshal(y, &fromY); err != nil || fromY.Status != SuiteBlocked || fromY.Reason != "no switch" {
		t.Fatalf("%s: %+v %v", y, fromY, err)
	}
	// A normal suite has neither field on the wire.
	nb, _ := json.Marshal(&Suite{ID: "n", Benchmarks: []SuiteItem{{Ref: "a"}}})
	if strings.Contains(string(nb), "status") || strings.Contains(string(nb), "reason") {
		t.Fatalf("%s", nb)
	}
}
