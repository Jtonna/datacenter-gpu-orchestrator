package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const benchProfiles = `profiles:
  tiny:
    model: tiny.gguf
    ctx: 2048
    needs_mib: 1000
`

func TestBenchConfigDefaults(t *testing.T) {
	c := mustParse(t, "profiles: {}\n")
	if c.DataDir != "" || c.WSMaxClients != 32 || c.Bench.Enabled || c.Bench.RegressLoadPct != 15 ||
		c.Bench.RegressDecodePct != 7 || c.Bench.MaxRunMinutes != 240 || c.Bench.RetentionDays != 0 { // retention is off by default
		t.Fatalf("defaults: %+v", c)
	}
	c = mustParse(t, "data_dir: /var/lib/llm-scheduler/data\nprofiles: {}\n")
	if c.DataDir != "/var/lib/llm-scheduler/data" || !c.Bench.Enabled {
		t.Fatalf("data_dir set: bench should default to on: %+v", c.Bench)
	}
	c = mustParse(t, "data_dir: /d\nbench: {enabled: false}\nprofiles: {}\n")
	if c.Bench.Enabled {
		t.Fatal("bench.enabled false ignored")
	}
	if c = mustParse(t, "data_dir: /d\nbench: {retention_days: 183}\nprofiles: {}\n"); c.Bench.RetentionDays != 183 {
		t.Fatalf("retention_days 183 read as %d", c.Bench.RetentionDays)
	}
}

func TestBenchConfigValidation(t *testing.T) {
	bad := map[string]string{
		"enabled without data_dir": "bench: {enabled: true}\nprofiles: {}\n",
		"ws clients":               "ws_max_clients: 0\nprofiles: {}\n",
		"regress load":             "data_dir: /d\nbench: {regress_load_pct: 0}\nprofiles: {}\n",
		"regress decode":           "data_dir: /d\nbench: {regress_decode_pct: -1}\nprofiles: {}\n",
		"max run":                  "data_dir: /d\nbench: {max_run_minutes: 0}\nprofiles: {}\n",
		"retention":                "data_dir: /d\nbench: {retention_days: -1}\nprofiles: {}\n",
		"unknown key":              "data_dir: /d\nbench: {nope: 1}\nprofiles: {}\n",
		"unknown profile":          "data_dir: /d\nbench:\n  benchmarks:\n    - {id: a, profile: ghost, requests: [{prompt: x, max_tokens: 4}]}\n" + benchProfiles,
		"duplicate id":             "data_dir: /d\nbench:\n  benchmarks:\n    - {id: a, profile: tiny, requests: [{prompt: x, max_tokens: 4}]}\n    - {id: a, profile: tiny, requests: [{prompt: x, max_tokens: 4}]}\n" + benchProfiles,
		"suite ref":                "data_dir: /d\nbench:\n  suites:\n    - {id: s, benchmarks: [missing]}\n" + benchProfiles,
		"unknown benchmark field":  "data_dir: /d\nbench:\n  benchmarks:\n    - {id: a, profile: tiny, colour: red, requests: [{prompt: x, max_tokens: 4}]}\n" + benchProfiles,
	}
	for name, y := range bad {
		if _, err := ParseWithOptions([]byte(y), Options{FileExists: existsAll}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestBenchSeedsParse(t *testing.T) {
	c := mustParse(t, `data_dir: /d
bench:
  regress_load_pct: 20
  benchmarks:
    - id: tiny-cold
      profile: tiny
      requests:
        - {name: hi, prompt: hello, max_tokens: 8}
    - id: both
      parallel: [tiny-cold, tiny-warm]
    - id: tiny-warm
      profile: tiny
      cold: false
      requests: [{prompt: hello, max_tokens: 8}]
  suites:
    - id: nightly
      benchmarks: [tiny-cold, both]
      cooldown_s: 1
`+benchProfiles)
	if len(c.Bench.Benchmarks) != 3 || len(c.Bench.Suites) != 1 || c.Bench.RegressLoadPct != 20 ||
		c.Bench.Suites[0].ID != "nightly" || c.Bench.Suites[0].Benchmarks[1].Ref != "both" {
		t.Fatalf("%+v", c.Bench)
	}
}

// Both shipped config files carry the bench block as a commented example,
// with the same keys the parser knows.
func TestExampleConfigsDocumentTheBenchBlock(t *testing.T) {
	for _, rel := range []string{"configs/example.yaml", "deploy/seed-config.yaml"} {
		b, err := os.ReadFile(filepath.Join("..", "..", rel))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"data_dir:", "# ws_max_clients: 32", "# bench:", "#   regress_load_pct: 15",
			"#   regress_decode_pct: 7", "#   max_run_minutes: 240", "#   suites:"} {
			if !strings.Contains(string(b), want) {
				t.Errorf("%s lacks %q", rel, want)
			}
		}
	}
}

// The commented example block is real: uncommenting it gives a valid config.
func TestCommentedBenchExampleParsesWhenUncommented(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "configs", "example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	i := strings.Index(src, "# bench:")
	if i < 0 {
		t.Fatal("no commented bench block")
	}
	var block, plain []string
	for _, l := range strings.Split(src[i:], "\n") {
		if !strings.HasPrefix(l, "#") {
			break
		}
		block = append(block, l)
		plain = append(plain, strings.TrimPrefix(strings.TrimPrefix(l, "#"), " "))
	}
	spliced := "data_dir: /var/lib/llm-scheduler/data\n" + src[:i] + strings.Join(plain, "\n") + src[i+len(strings.Join(block, "\n")):]
	c, err := ParseWithOptions([]byte(spliced), Options{FileExists: existsAll})
	if err != nil {
		t.Fatalf("the uncommented example does not validate: %v\n%s", err, strings.Join(plain, "\n"))
	}
	if len(c.Bench.Suites) == 0 || len(c.Bench.Benchmarks) == 0 || !c.Bench.Enabled {
		t.Fatalf("the example seeds nothing: %+v", c.Bench)
	}
}

// A planned or blocked scenario imports from the config with no benchmark
// and its reason; a normal suite without one is still refused.
func TestBenchPlannedSuiteImports(t *testing.T) {
	c := mustParse(t, `data_dir: /d
bench:
  suites:
    - id: five-consoles
      status: planned
      reason: the fifth console is not set up yet
      benchmarks: []
    - id: gbe
      status: blocked
      reason: waiting for the switch
`+benchProfiles)
	if len(c.Bench.Suites) != 2 || c.Bench.Suites[0].Status != "planned" || c.Bench.Suites[1].Reason != "waiting for the switch" {
		t.Fatalf("%+v", c.Bench.Suites)
	}
	y := "data_dir: /d\nbench:\n  suites:\n    - {id: s, status: blocked}\n" + benchProfiles
	if _, err := ParseWithOptions([]byte(y), Options{FileExists: existsAll}); err == nil {
		t.Fatal("a blocked suite without a reason was accepted")
	}
}
