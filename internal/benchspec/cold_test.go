package benchspec

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestColdIsALoadOptionOnly(t *testing.T) {
	b, err := ParseBenchmarkJSON([]byte(`{"id":"a","profile":"p","phases":[{"type":"load","cold":true},{"type":"load"},{"type":"unload"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Validate(okAll); err != nil {
		t.Fatal(err)
	}
	if !b.Phases[0].EvictsCache() || b.Phases[1].EvictsCache() {
		t.Fatalf("%+v", b.Phases)
	}
	err = validate(t, `{"id":"a","profile":"p","phases":[{"type":"unload","cold":true}]}`)
	if err == nil || !strings.Contains(err.Error(), `option "cold" does not belong to this phase type`) {
		t.Fatalf("%v", err)
	}
}

func TestSchemaDescribesColdEvictOthersAndTheTemplate(t *testing.T) {
	d := Schema()
	var cold *Option
	for _, p := range d.PhaseTypes {
		if p.Type == PhaseLoad {
			for i := range p.Options {
				if p.Options[i].Name == "cold" {
					cold = &p.Options[i]
				}
			}
		}
	}
	if cold == nil || cold.Type != "boolean" || cold.Default != false || !strings.Contains(cold.Description, "file cache") {
		t.Fatalf("%+v", cold)
	}
	names := []string{}
	for _, o := range d.Suite {
		names = append(names, o.Name)
		if o.Description == "" {
			t.Errorf("%+v", o)
		}
	}
	if strings.Join(names, ",") != "stop_on_failure,cooldown_s,only_when_idle,idle_wait_s,evict_others" {
		t.Fatalf("%v", names)
	}
	tp, ok := TemplateByID(TemplateColdReload)
	if !ok || tp.Name != "Cold, reload, loaded" {
		t.Fatalf("%+v", tp)
	}
	b, err := ParseBenchmarkJSON(tp.Benchmark)
	if err != nil {
		t.Fatal(err)
	}
	var shape []string
	for _, p := range b.Phases {
		s := p.Type
		if p.Label != "" {
			s += "(" + p.Label + ")"
		}
		if p.EvictsCache() {
			s += "+cold"
		}
		shape = append(shape, s)
	}
	if got := strings.Join(shape, " "); got != "load(cold load)+cold warmup(first generation) requests(loaded) unload load(reload) requests(after the reload) unload" {
		t.Fatalf("%s", got)
	}
}

func TestEvictOthersSuiteOption(t *testing.T) {
	s, err := ParseSuiteJSON([]byte(`{"id":"s","benchmarks":["a"],"evict_others":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !s.EvictsOthers() || !s.WaitsForIdle() {
		t.Fatal("evict_others true, only_when_idle default true")
	}
	s2, _ := ParseSuiteJSON([]byte(`{"id":"s","benchmarks":["a"]}`))
	if s2.EvictsOthers() {
		t.Fatal("default false")
	}
	b, _ := json.Marshal(s2)
	if strings.Contains(string(b), "evict_others") {
		t.Fatalf("%s", b)
	}
}
