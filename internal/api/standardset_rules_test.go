package api

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// The reviewer's rulings A to G for the standard set (26 Nov), one test
// each, on ssEnv (standardset_test.go).

// A: the family's base name, on live's five families as they are on the
// fleet (ids from GET /api/config, revision 718).
func TestStandardSet_FamilyBase(t *testing.T) {
	for _, c := range []struct {
		ids  []string
		want string
	}{
		{[]string{"gemma-4-12b", "gemma-4-12b-thinking", "gemma-4-12b:48k", "gemma-4-12b:64k", "gemma-4-12b:96k"}, "gemma-4-12b"},
		{[]string{"qwen3.5-4b", "qwen3.5-4b-thinking"}, "qwen3.5-4b"},
		{[]string{"qwen3.8-27b", "qwen3.8-27b-thinking"}, "qwen3.8-27b"},
		{[]string{"devstral-24b", "devstral-24b:14k", "devstral-24b:24k"}, "devstral-24b"},
		{[]string{"tinyllama"}, "tinyllama"},
		{[]string{"x:16k", "x:32k"}, "x"}, // only ":" ids: the part before it
		{nil, ""},
	} {
		if got := familyBase(c.ids); got != c.want {
			t.Errorf("familyBase(%v) = %q, want %q", c.ids, got, c.want)
		}
	}
	// the file's own name keeps its dots (live writes qwen3.5-4b, not qwen3-5-4b)
	for in, want := range map[string]string{"Qwen2.5 72B Instruct": "qwen2.5-72b-instruct", "Llama 3.3 70B Instruct": "llama-3.3-70b-instruct",
		"Mixtral 8x7B Instruct v0.1": "mixtral-8x7b-instruct-v0.1", "Devstral-Small-2-24B-Instruct-2512": "devstral-small-2-24b-instruct-2512"} {
		if got := slugDots(in); got != want {
			t.Errorf("slugDots(%q) = %q, want %q", in, got, want)
		}
	}
}

// A: the name field. Prefilled from the family, else the file's name with
// its dots; editable (?short= on the GET, short in the POST); refused with
// a plain reason when it breaks the id rules or meets another file's
// profile or alias; a prefill that meets one writes nothing.
func TestStandardSet_Name(t *testing.T) {
	e, svc, _ := ssEnv(t)
	g := "gemma-3-12b-Q4_K_M.gguf"
	if s := ssGet(t, e, g); s.Short != "gemma-3-12b" || s.ShortSource != "family" || s.Family != "gemma-3-12b" {
		t.Fatalf("gemma prefill: %q %q %q", s.Short, s.ShortSource, s.Family)
	}
	if s := ssGet(t, e, "qwen-dots.gguf"); s.Short != "qwen2.5-72b-instruct" || s.ShortSource != "file" || s.ShortProblem != "" ||
		!reflect.DeepEqual(s.ToWrite, []string{"qwen2.5-72b-instruct:16k", "qwen2.5-72b-instruct"}) {
		t.Fatalf("qwen prefill: %+v", s)
	}
	refused := map[string]string{
		"older":     "the name older is taken: the profile older is of another file (nometa.gguf)",
		"nometa":    "the name nometa is taken: nometa:16k is an alias of the profile older, of another file (nometa.gguf)",
		"TEST - x":  `the name must not start with "TEST - "`,
		"a@b":       `must not contain "@"`,
		"a:b":       `must not contain ":"`,
		"my gemma":  "must be one word",
		"gemma/12b": "must be one word",
	}
	for short, want := range refused {
		resp := e.get("/api/models/" + g + "/standard-set?short=" + url.QueryEscape(short))
		b := body(t, resp)
		if resp.StatusCode != 400 || !strings.Contains(fmt.Sprint(jsonOf(t, b)), want) {
			t.Fatalf("?short=%q: %d %s", short, resp.StatusCode, b)
		}
	}
	rev := svc.Revision()
	if st, m := e.cdo("POST", "/api/models/"+g+"/standard-set", `{"comment":"x","short":"older"}`, nil); st != 400 || !strings.Contains(fmt.Sprint(m), "the profile older is of another file") || svc.Revision() != rev {
		t.Fatalf("POST short older: %d %v", st, m)
	}
	// a name of one's own
	resp := e.get("/api/models/" + g + "/standard-set?short=my-gemma")
	var s standardSet
	if err := json.Unmarshal([]byte(body(t, resp)), &s); err != nil || resp.StatusCode != 200 || s.Short != "my-gemma" || s.ShortSource != "asked" || s.DefaultID != "my-gemma" ||
		!slices.Contains(s.ToWrite, "my-gemma:16k") {
		t.Fatalf("my-gemma: %d %+v %v", resp.StatusCode, s, err)
	}
	st, m := e.cdo("POST", "/api/models/"+g+"/standard-set", `{"comment":"mine","short":"my-gemma"}`, nil)
	if st != 200 || fmt.Sprint(m["written"]) != fmt.Sprint(toAny(s.ToWrite)) || svc.Config().Profiles["my-gemma:16k"] == nil {
		t.Fatalf("POST my-gemma: %d %v", st, m)
	}
	// a prefill that meets another file's profile: shown, nothing can be written
	if st, m := e.cdo("POST", "/api/config/profiles", `{"id":"qwen2.5-72b-instruct","model":"huge.gguf","ctx":4096,"comment":"someone"}`, nil); st >= 300 {
		t.Fatalf("make the clash: %d %v", st, m)
	}
	q := ssGet(t, e, "qwen-dots.gguf")
	if !strings.Contains(q.ShortProblem, "the profile qwen2.5-72b-instruct is of another file (huge.gguf)") || len(q.ToWrite) != 0 {
		t.Fatalf("clash prefill: %+v", q)
	}
	for _, r := range q.Rows {
		if r.Tickable || r.Selected {
			t.Fatalf("clash row %+v", r)
		}
	}
	rev = svc.Revision()
	if st, m := e.cdo("POST", "/api/models/qwen-dots.gguf/standard-set", `{"comment":"x"}`, nil); st != 400 || !strings.Contains(fmt.Sprint(m), "is of another file") || svc.Revision() != rev {
		t.Fatalf("POST clash prefill: %d %v", st, m)
	}
}

// B and D: a family with its default id at another context, a context it
// has, what the existing rows carry, and the shape note that names a
// figure of another shape without taking it.
func TestStandardSet_ExistsAndCarries(t *testing.T) {
	e, _, _ := ssEnv(t)
	g := "gemma-3-12b-Q4_K_M.gguf"
	spec := `{"id":"gemma-3-12b","model":"` + g + `","ctx":8192,"kv_quant":"q8_0","flash_attn":true,"needs_mib":8600,` +
		`"extra_args":["--chat-template-kwargs","{\"enable_thinking\":false}"],"aliases":["gemma-chat"],"comment":"as on live"}`
	if st, m := e.cdo("POST", "/api/config/profiles", spec, nil); st >= 300 {
		t.Fatalf("make gemma-3-12b: %d %v", st, m)
	}
	s := ssGet(t, e, g)
	def := ssRow(t, s, 32768)
	if def.ID != "gemma-3-12b" || !def.Default || !def.Exists || def.Selected || def.Tickable ||
		def.ExistsNote != "gemma-3-12b exists at 8k, kept as it is; the standard default would be 32k" {
		t.Fatalf("default: %+v", def)
	}
	want := []string{"KV type q8_0", "flash attention", "thinking off (the chat-template argument)", "a hand-written need of 8,600 MiB", "aliases (gemma-chat)"}
	if !reflect.DeepEqual(def.Carries, want) {
		t.Fatalf("carries %q, want %q", def.Carries, want)
	}
	if slices.Contains(s.ToWrite, "gemma-3-12b") {
		t.Fatalf("the default would be written: %v", s.ToWrite)
	}
	// the 64k the family has (the person's f16 profile): exists, carries its need
	if r := ssRow(t, s, 65536); !r.Exists || r.ExistsAs != "gemma-3-12b:64k" || !reflect.DeepEqual(r.Carries, []string{"a hand-written need of 22,000 MiB"}) {
		t.Fatalf("64k: %+v", r)
	}
	// D: an estimate row names the q8_0 figure and does not take it; the
	// f16 64k profile is the standard shape, so it is not named
	r16 := ssRow(t, s, 16384)
	wantNote := "the existing gemma-3-12b (8k) runs at 8,600 MiB (written by hand) with KV q8_0, flash attention, extra arguments --chat-template-kwargs {\"enable_thinking\":false}; " +
		"this row would use the default KV type (f16), no flash attention, no extra arguments, so that figure is not its own"
	if r16.NeedSource != "estimate" || r16.ShapeNote != wantNote || r16.NeedMiB == nil || *r16.NeedMiB == 8600 {
		t.Fatalf("16k: %+v\nnote %q", r16, r16.ShapeNote)
	}
}

// C: the five states, the default selection, and the ids a POST may name.
func TestStandardSet_StatesAndTicks(t *testing.T) {
	e, svc, _ := ssEnv(t)
	small := ssGet(t, e, "small-measured.gguf")
	for _, r := range small.Rows {
		if r.State != "fits" || r.NeedSource != "measured for this shape" || !r.Selected || !r.Tickable || r.NeedNote != "" {
			t.Fatalf("small: %+v", r)
		}
	}
	big := ssGet(t, e, "big-measured.gguf")
	for _, r := range big.Rows {
		if r.State != "does-not-fit" || r.Tickable || r.Selected || !strings.HasPrefix(r.Reason, "by the measured need it does not fit even with nothing loaded") {
			t.Fatalf("big: %+v", r)
		}
	}
	h := ssGet(t, e, "huge.gguf")
	r16, r64 := ssRow(t, h, 16384), ssRow(t, h, 65536)
	if r16.ID != "huge" || !r16.Default {
		t.Fatalf("huge default: %+v", r16)
	}
	if r16.State != "fits-by-estimate" || !r16.Selected || r64.State != "estimate-does-not-fit" || r64.Selected || !r64.Tickable ||
		!strings.HasPrefix(r64.Reason, "not measured: by the file estimate it does not fit") || slices.Contains(h.ToWrite, "huge:64k") {
		t.Fatalf("huge: %+v / %+v / %v", r16, r64, h.ToWrite)
	}
	u := ssGet(t, e, "unknown.gguf")
	for _, r := range u.Rows {
		if r.State != "need-unknown" || r.Tickable || r.Kept || r.NeedMiB != nil {
			t.Fatalf("unknown: %+v", r)
		}
	}
	// the POST's ids: refused unless they are rows of the call that may be ticked
	rev := svc.Revision()
	for body, want := range map[string]string{
		`{"comment":"x","ids":["nope"]}`:             "nope is not one of this call's rows",
		`{"comment":"x","ids":["big-measured:16k"]}`: "big-measured:16k cannot be written: by the measured need",
		`{"comment":"x","ids":"huge:64k"}`:           "ids must be a list",
		`{"comment":"x","ids":["gemma-3-12b:64k"]}`:  "is not one of this call's rows",
	} {
		file := "big-measured.gguf"
		if strings.Contains(body, "nope") || strings.Contains(body, "huge") || strings.Contains(body, "gemma") {
			file = "huge.gguf"
		}
		if st, m := e.cdo("POST", "/api/models/"+file+"/standard-set", body, nil); st != 400 || !strings.Contains(fmt.Sprint(m), want) || svc.Revision() != rev {
			t.Fatalf("%s on %s: %d %v", body, file, st, m)
		}
	}
	// an existing row named: refused with its note
	if st, m := e.cdo("POST", "/api/models/gemma-3-12b-Q4_K_M.gguf/standard-set", `{"comment":"x","ids":["gemma-3-12b:64k"]}`, nil); st != 400 || !strings.Contains(fmt.Sprint(m), "gemma-3-12b:64k cannot be written: exists, kept as it is") {
		t.Fatalf("existing id: %d %v", st, m)
	}
	// a ticked estimate row: exactly it is written
	st, m := e.cdo("POST", "/api/models/huge.gguf/standard-set", `{"comment":"try it","ids":["huge:64k"]}`, nil)
	if st != 200 || fmt.Sprint(m["written"]) != "[huge:64k]" || svc.Revision() != rev+1 || svc.Config().Profiles["huge:64k"] == nil || svc.Config().Profiles["huge"] != nil {
		t.Fatalf("ticked: %d %v", st, m)
	}
	if !strings.Contains(fmt.Sprint(m["skipped"]), "id:huge reason:not chosen") {
		t.Fatalf("skipped: %v", m["skipped"])
	}
}

// E: no comment, no write: refused with or without rows to write.
func TestStandardSet_CommentAlways(t *testing.T) {
	e, svc, _ := ssEnv(t)
	g := "gemma-3-12b-Q4_K_M.gguf"
	if s := ssGet(t, e, g); len(s.ToWrite) == 0 {
		t.Fatal("the file must have rows to write for this test")
	}
	rev := svc.Revision()
	for _, b := range []string{``, `{}`, `{"comment":""}`, `{"comment":"   "}`, `{"ids":[]}`, `{"short":"gemma-3-12b"}`} {
		if st, m := e.cdo("POST", "/api/models/"+g+"/standard-set", b, nil); st != 400 || !strings.Contains(fmt.Sprint(m), "a comment is required") || svc.Revision() != rev {
			t.Fatalf("body %q: %d %v (revision %d, was %d)", b, st, m, svc.Revision(), rev)
		}
	}
	// the header counts as the comment
	if st, m := e.cdo("POST", "/api/models/"+g+"/standard-set", `{}`, map[string]string{"X-Comment": "by header"}); st != 200 || m["changed"] != true {
		t.Fatalf("X-Comment: %d %v", st, m)
	}
	// nothing left to write: still refused without one
	rev = svc.Revision()
	if st, m := e.cdo("POST", "/api/models/"+g+"/standard-set", `{}`, nil); st != 400 || svc.Revision() != rev {
		t.Fatalf("nothing left, no comment: %d %v", st, m)
	}
	if st, m := e.cdo("POST", "/api/models/"+g+"/standard-set", `{"comment":"again"}`, nil); st != 200 || m["changed"] != false || svc.Revision() != rev {
		t.Fatalf("nothing left, a comment: %d %v", st, m)
	}
}

// F: the row's need is the written profile's plan need; the margin figure
// is named apart. G: the every-zone sentence names max_pool when it set
// the number; the 160k rule.
func TestStandardSet_NeedAndWords(t *testing.T) {
	e, _, _ := ssEnv(t)
	g := "gemma-3-12b-Q4_K_M.gguf"
	s := ssGet(t, e, g)
	if st, m := e.cdo("POST", "/api/models/"+g+"/standard-set", `{"comment":"for the plan"}`, nil); st != 200 {
		t.Fatalf("write: %d %v", st, m)
	}
	for _, r := range s.Rows {
		if r.Exists || !r.Selected {
			continue
		}
		resp := e.post("/api/plan", `{"model":"`+r.ID+`"}`)
		p := jsonOf(t, body(t, resp))
		if p["need_mib"] != float64(*r.NeedMiB) || p["fit_need_mib"] != float64(*r.FitNeedMiB) {
			t.Fatalf("%s: row %d / %d, plan %v / %v", r.ID, *r.NeedMiB, *r.FitNeedMiB, p["need_mib"], p["fit_need_mib"])
		}
		if r.NeedSource == "estimate" && r.NeedNote != fmt.Sprintf("the fit uses %s MiB: the estimate with its 5%% margin", commas(*r.FitNeedMiB)) {
			t.Fatalf("%s need note %q", r.ID, r.NeedNote)
		}
	}
	// G
	if big := ssRow(t, s, 131072); big.EveryZoneNote != "this context fits only by using every zone it may use (3 of 3 reachable): nothing else can be placed beside it" || big.ByPool {
		t.Fatalf("128k: %q %v", big.EveryZoneNote, big.ByPool)
	}
	if got := everyZoneSentence(4, 4, true); got != "this context fits only by using every zone it may use (4: the setting max_pool): nothing else can be placed beside it" {
		t.Fatalf("by pool: %q", got)
	}
	if st, m := e.cdo("PATCH", "/api/config/settings", `{"max_pool":2,"comment":"two"}`, nil); st >= 300 {
		t.Fatalf("max_pool 2: %d %v", st, m)
	}
	two := ssGet(t, e, "huge.gguf")
	for _, r := range two.Rows {
		if r.MayUse != 2 || !r.ByPool || (r.UsesEveryZone && !strings.Contains(r.EveryZoneNote, "(2: the setting max_pool)")) {
			t.Fatalf("max_pool 2: %+v", r)
		}
	}
	// the trained-context rule: 160k at the trained context, once; equal is not above
	d := ssGet(t, e, "deep.gguf")
	var ctxs []int
	for _, r := range d.Rows {
		ctxs = append(ctxs, r.Ctx)
	}
	if !reflect.DeepEqual(ctxs, []int{16384, 32768, 65536, 131072, 163840}) || d.Rows[4].ID != "deep:160k" ||
		d.Note != "trained at 163,840 tokens: the standard contexts above it are replaced by the trained context itself (160k), tried once" {
		t.Fatalf("deep: %v %q %+v", ctxs, d.Note, d.Rows[4])
	}
	if n := trainedNote(131072); n != "trained at 131,072 tokens: the standard contexts above it are not tried; 128k, equal to it, is" {
		t.Fatalf("131072: %q", n)
	}
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// The labels (live's list, 26 Nov): a need derived from other contexts is
// never "measured for this shape" (an own-slope figure is placer.Measured
// too), and the state is that of the need on the zones the fit was decided
// on; when that figure is not the plan's, the row names both.
func TestStandardSet_DerivedLabelAndTwoFigures(t *testing.T) {
	slope := &scheduler.DerivedSlope{MiBPer1kCtx: 56, Devices: 6}
	own := scheduler.PlanReport{NeedMiB: 17627, Measured: true, DerivedSlope: slope, NeedBasis: "own slope of this model on 6 devices"}
	if got := needSource(own); got != "derived" {
		t.Fatalf("own slope: %q", got)
	}
	if got := needSource(scheduler.PlanReport{NeedMiB: 9000, Measured: true, DerivedFromCtx: 32768}); got != "derived" {
		t.Fatalf("formula step: %q", got)
	}
	if got := needSource(scheduler.PlanReport{NeedMiB: 9000, Measured: true}); got != "measured for this shape" {
		t.Fatalf("measured: %q", got)
	}
	for src, want := range map[scheduler.NeedSource]string{scheduler.NeedOwnSlope: "derived", scheduler.NeedDerived: "derived", scheduler.NeedRestored: "measured for this shape",
		scheduler.NeedLearned: "measured for this shape", scheduler.NeedEstimated: "estimate"} {
		if got := zonesSource(scheduler.CandidatePlan{ZonesNeedKnown: true, ZonesNeedSource: src}); got != want {
			t.Errorf("zones source %q: %q, want %q", src, got, want)
		}
	}
	// gemma-4-12b:256k on live: 18,499 on 6 devices in the plan, 105,440 on the 4 max_pool allows
	gem := scheduler.PlanReport{NeedMiB: 18499, Measured: true, DerivedSlope: slope, NeedBasis: "own slope of this model on 6 devices",
		Action: scheduler.PlanReject, Reason: "placer: need 105440 MiB (requires 113632 MiB) exceeds cluster capacity 55692 MiB"}
	cp := scheduler.CandidatePlan{Report: gem, MayUse: 4, ByPool: true, ZonesCount: 4, ZonesNeedMiB: 105440, ZonesNeedKnown: true, ZonesNeedSource: scheduler.NeedDerived}
	want := "/api/plan shows 18,499 MiB, the need on the count of devices its figure rests on (own slope of this model on 6 devices); " +
		"with nothing loaded it may use at most 4 zones (the setting max_pool), and on 4 its need is 105,440 MiB (derived): the figure the fit was decided on; the plan's message below quotes this one"
	if got := zonesNeedNote(gem, cp, false); got != want {
		t.Fatalf("note:\n%s\nwant\n%s", got, want)
	}
	gem.Reason = "placer: need 18499 MiB (requires 18499 MiB) does not fit the available nodes"
	if got := zonesNeedNote(gem, cp, false); !strings.HasSuffix(got, "; the plan's message below quotes 18,499 MiB, the need before a zone count is chosen") {
		t.Fatalf("note on a does-not-fit: %s", got)
	}
}
