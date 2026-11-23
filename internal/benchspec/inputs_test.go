package benchspec

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const recipeJSON = `{"id":"cold-load","name":"Cold load","inputs":{"ctx":8192,"placement":"any:2"},"phases":[{"type":"load","cold":true},{"type":"unload"}]}`

func TestPlacementForms(t *testing.T) {
	for in, want := range map[string]string{
		`"auto"`: "auto", `"all"`: "all", `"any:2"`: "any:2", `"nodes:ps5-1,ps5-4"`: "nodes:ps5-1,ps5-4",
		`{"any":3}`: "any:3", `{"all":true}`: "all", `{"auto":true}`: "auto", `{"nodes":["ps5-2"]}`: "nodes:ps5-2",
	} {
		var p Placement
		if err := json.Unmarshal([]byte(in), &p); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if err := p.Validate(); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if p.String() != want {
			t.Errorf("%s: %q, want %q", in, p.String(), want)
		}
		b, _ := json.Marshal(p)
		if string(b) != `"`+want+`"` {
			t.Errorf("%s marshals to %s", in, b)
		}
	}
	for _, in := range []string{`"any:0"`, `"any:x"`, `"some"`, `{"any":2,"all":true}`, `{}`, `"nodes:"`, `{"nodes":["bad name!"]}`, `"any:17"`} {
		var p Placement
		err := json.Unmarshal([]byte(in), &p)
		if err == nil {
			err = p.Validate()
		}
		if err == nil {
			t.Errorf("%s accepted", in)
		}
	}
}

func TestRecipeValidation(t *testing.T) {
	b := mustBench(t, recipeJSON)
	if err := b.Validate(profiles()); err != nil {
		t.Fatal(err)
	}
	if !b.IsRecipe() || b.Inputs.Model != "" {
		t.Fatal("recipe")
	}
	for name, c := range map[string]struct{ js, want string }{
		"both":        {`{"id":"a","profile":"p","inputs":{"ctx":8192},"phases":[{"type":"load"}]}`, "not both"},
		"neither":     {`{"id":"a","phases":[{"type":"load"}]}`, "profile or inputs is required"},
		"bad ctx":     {`{"id":"a","inputs":{"ctx":10},"phases":[{"type":"load"}]}`, "inputs.ctx"},
		"bad model":   {`{"id":"a","inputs":{"model":"../x.gguf"},"phases":[{"type":"load"}]}`, "inputs.model"},
		"bad kv":      {`{"id":"a","inputs":{"kv_quant":"q2"},"phases":[{"type":"load"}]}`, "inputs.kv_quant"},
		"runner only": {`{"id":"a","profile":"p","recipe":"x","phases":[{"type":"load"}]}`, "set by the runner"},
		"parallel":    {`{"id":"a","parallel":["x","y"],"inputs":{"ctx":8192}}`, "parallel step"},
	} {
		b := mustBench(t, c.js)
		err := b.Validate(profiles("p"))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

func TestProfileIDFromInputs(t *testing.T) {
	f := false
	for _, c := range []struct {
		in   Inputs
		want string
	}{
		{Inputs{Model: "gemma-3-12b-it-Q4_K_M.gguf", Ctx: 8192, Placement: &Placement{Mode: PlaceAny, N: 2}}, "bench-gemma-3-12b-it-q4-k-m-any2-8k"},
		{Inputs{Model: "qwen3-4b.gguf", Ctx: 40000, Placement: &Placement{Mode: PlaceAll}, Thinking: &f}, "bench-qwen3-4b-all-40000-nothink"},
		{Inputs{Model: "m.gguf", Ctx: 4096}, "bench-m-auto-4k"},
		{Inputs{Model: "m.gguf", Ctx: 4096, KVQuant: "q8_0", FlashAttn: &[]bool{true}[0]}, "bench-m-auto-4k-kvq8-0-fa"},
		{Inputs{Model: "m.gguf", Ctx: 4096, Placement: &Placement{Mode: PlaceNodes, Nodes: []string{"ps5-1", "ps5-4"}}}, "bench-m-on-ps5-1-ps5-4-4k"},
	} {
		if got := c.in.ProfileID(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.in, got, c.want)
		}
	}
	long := Inputs{Model: strings.Repeat("very-long-model-name-", 10) + ".gguf", Ctx: 8192}
	if id := long.ProfileID(); len(id) > MaxProfileIDLen || !strings.HasPrefix(id, "bench-") {
		t.Errorf("long: %q (%d)", id, len(id))
	}
	other := Inputs{Model: strings.Repeat("very-long-model-name-", 10) + "x.gguf", Ctx: 8192}
	if long.ProfileID() == other.ProfileID() {
		t.Error("two long names share a profile id")
	}
}

// A group: plain refs stay as they are (old suites run exactly as before),
// recipes are expanded with their inputs, matrix entries multiply, nested
// groups are spliced in.
func TestExpandGroup(t *testing.T) {
	recipe := mustBench(t, recipeJSON)
	if err := recipe.Validate(nil); err != nil {
		t.Fatal(err)
	}
	fixed := mustBench(t, `{"id":"fixed","profile":"p","phases":[{"type":"load"}]}`)
	look := func(id string) (*Benchmark, bool) {
		switch id {
		case "cold-load":
			return recipe, true
		case "fixed":
			return fixed, true
		}
		return nil, false
	}
	inner, err := ParseSuiteJSON([]byte(`{"id":"gemma","benchmarks":[{"benchmark":"cold-load","inputs":{"model":"gemma.gguf"}},"fixed"]}`))
	if err != nil {
		t.Fatal(err)
	}
	groups := func(id string) (*Suite, bool) {
		if id == "gemma" {
			return inner, true
		}
		return nil, false
	}
	if err := inner.ValidateGroup(profiles("p"), look, groups); err != nil {
		t.Fatal(err)
	}
	top, err := ParseSuiteJSON([]byte(`{"id":"full","benchmarks":[
		{"group":"gemma"},
		{"benchmark":"cold-load","matrix":{"model":["a.gguf","b.gguf"],"placement":["any:1","all"]},"inputs":{"ctx":4096}},
		"fixed"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := top.ValidateGroup(profiles("p"), look, groups); err != nil {
		t.Fatal(err)
	}
	flat, err := ExpandGroup(top, look, groups)
	if err != nil {
		t.Fatal(err)
	}
	if len(flat.Benchmarks) != 7 {
		t.Fatalf("%d entries: %+v", len(flat.Benchmarks), flat.Benchmarks)
	}
	if flat.Benchmarks[1].Ref != "fixed" || flat.Benchmarks[6].Ref != "fixed" || flat.Benchmarks[1].Inline != nil {
		t.Fatalf("plain refs: %+v", flat.Benchmarks)
	}
	g := flat.Benchmarks[0].Inline
	if g == nil || g.Recipe != "cold-load" || g.Applied.Model != "gemma.gguf" || g.Applied.Ctx != 8192 || g.Applied.Placement.String() != "any:2" ||
		g.Profile != "bench-gemma-any2-8k" || g.Inputs != nil || !strings.HasPrefix(g.ID, "cold-load.") || g.Name != "Cold load (gemma.gguf, 8k, any:2)" {
		t.Fatalf("gemma: %+v %+v", g, g.Applied)
	}
	var profs []string
	for _, it := range flat.Benchmarks[2:6] {
		profs = append(profs, it.Inline.Profile)
	}
	if strings.Join(profs, " ") != "bench-a-any1-4k bench-a-all-4k bench-b-any1-4k bench-b-all-4k" {
		t.Fatalf("matrix: %v", profs)
	}
	// The same inputs give the same expanded id (so runs line up over time); other inputs another.
	again, _ := ExpandGroup(top, look, groups)
	if again.Benchmarks[2].Inline.ID != flat.Benchmarks[2].Inline.ID || flat.Benchmarks[2].Inline.ID == flat.Benchmarks[3].Inline.ID {
		t.Fatal("expanded ids")
	}
	// The source group is not changed.
	if top.Benchmarks[0].Group != "gemma" || top.Benchmarks[1].Inline != nil {
		t.Fatal("source changed")
	}
	// The JSON form round-trips.
	b, _ := json.Marshal(top)
	if !strings.Contains(string(b), `{"group":"gemma"}`) || !strings.Contains(string(b), `"matrix":{"model":["a.gguf","b.gguf"],"placement":["any:1","all"]}`) {
		t.Fatalf("json: %s", b)
	}
	back, err := ParseSuiteJSON(b)
	if err != nil || back.Benchmarks[1].Matrix == nil || back.Benchmarks[0].Group != "gemma" {
		t.Fatalf("back: %v %+v", err, back)
	}
}

func TestGroupValidation(t *testing.T) {
	recipe := mustBench(t, recipeJSON)
	recipe.Validate(nil)
	fixed := mustBench(t, `{"id":"fixed","profile":"p","phases":[{"type":"load"}]}`)
	look := func(id string) (*Benchmark, bool) {
		switch id {
		case "cold-load":
			return recipe, true
		case "fixed":
			return fixed, true
		}
		return nil, false
	}
	stored := map[string]*Suite{}
	groups := func(id string) (*Suite, bool) { s, ok := stored[id]; return s, ok }
	mk := func(js string) *Suite {
		s, err := ParseSuiteJSON([]byte(js))
		if err != nil {
			t.Fatalf("%s: %v", js, err)
		}
		return s
	}
	stored["a"] = mk(`{"id":"a","benchmarks":[{"group":"b"}]}`)
	stored["b"] = mk(`{"id":"b","benchmarks":[{"group":"c"}]}`)
	stored["c"] = mk(`{"id":"c","benchmarks":["fixed"]}`)
	for name, c := range map[string]struct{ js, want string }{
		"missing inputs":  {`{"id":"x","benchmarks":["cold-load"]}`, "needs the input model"},
		"missing group":   {`{"id":"x","benchmarks":[{"group":"nope"}]}`, `"nope" is not a group`},
		"self":            {`{"id":"x","benchmarks":[{"group":"x"}]}`, "contains itself"},
		"cycle":           {`{"id":"c","benchmarks":[{"group":"a"}]}`, "contains itself"},
		"inputs on fixed": {`{"id":"x","benchmarks":[{"benchmark":"fixed","inputs":{"ctx":4096}}]}`, "fixed profile"},
		"group inputs":    {`{"id":"x","benchmarks":[{"group":"c","inputs":{"ctx":4096}}]}`, "a group entry takes no inputs"},
		"both kinds":      {`{"id":"x","benchmarks":[{"group":"c","benchmark":"fixed"}]}`, "benchmark or group, not both"},
		"empty matrix":    {`{"id":"x","benchmarks":[{"benchmark":"cold-load","matrix":{}}]}`, "matrix"},
		"matrix missing":  {`{"id":"x","benchmarks":[{"benchmark":"cold-load","matrix":{"ctx":[4096]}}]}`, "needs the input model"},
		"too big matrix":  {`{"id":"x","benchmarks":[{"benchmark":"cold-load","matrix":{"model":[` + strings.TrimSuffix(strings.Repeat(`"m.gguf",`, 40), ",") + `],"ctx":[` + strings.TrimSuffix(strings.Repeat(`4096,`, 20), ",") + `]}}]}`, "more than"},
		"inputs not json": {`{"id":"x","benchmarks":[{"benchmark":"cold-load","inputs":{"model":"m.gguf","ctx":1}}]}`, "inputs.ctx"},
	} {
		s := mk(c.js)
		err := s.ValidateGroup(profiles("p"), look, groups)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	// Validate (without a group lookup) refuses a group entry plainly.
	if err := mk(`{"id":"x","benchmarks":[{"group":"c"}]}`).Validate(profiles("p"), look); err == nil || !strings.Contains(err.Error(), "is not a group") {
		t.Errorf("plain validate: %v", err)
	}
	// Nesting deeper than MaxGroupDepth is refused.
	for i := 0; i < MaxGroupDepth+1; i++ {
		id := "d" + string(rune('a'+i))
		next := "d" + string(rune('a'+i+1))
		if i == MaxGroupDepth {
			stored[id] = mk(`{"id":"` + id + `","benchmarks":["fixed"]}`)
		} else {
			stored[id] = mk(`{"id":"` + id + `","benchmarks":[{"group":"` + next + `"}]}`)
		}
	}
	if err := mk(`{"id":"top","benchmarks":[{"group":"da"}]}`).ValidateGroup(profiles("p"), look, groups); err == nil || !strings.Contains(err.Error(), "deeper than") {
		t.Errorf("depth: %v", err)
	}
}

func TestGroupEntryYAML(t *testing.T) {
	var s Suite
	src := "id: y\nbenchmarks:\n  - fixed\n  - group: c\n  - benchmark: cold-load\n    inputs: {model: m.gguf, placement: any:2}\n    matrix:\n      ctx: [4096, 8192]\n"
	if err := yaml.Unmarshal([]byte(src), &s); err != nil {
		t.Fatal(err)
	}
	if s.Benchmarks[0].Ref != "fixed" || s.Benchmarks[1].Group != "c" || s.Benchmarks[2].Ref != "cold-load" || s.Benchmarks[2].Inputs.Placement.String() != "any:2" ||
		len(s.Benchmarks[2].Matrix.Ctx) != 2 {
		t.Fatalf("%+v", s.Benchmarks)
	}
}
