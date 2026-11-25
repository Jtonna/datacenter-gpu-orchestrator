package bench

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The context_fill phase (B8, slice F1) through the runner with the
// scripted API: a quarter of the characters is a token (Tokenize and the
// chat's prompt_n agree), so the marks can be checked exactly.

var plantedRe = regexp.MustCompile(`Record (\d+): the harbour code is (\d+)\.`)

// recallingFill answers OK, and the last turn with every planted code.
func recallingFill(body string) fakeReply {
	if !strings.Contains(body, "harbour codes") {
		return fakeReply{text: "OK"}
	}
	var parts []string
	for _, m := range plantedRe.FindAllStringSubmatch(body, -1) {
		parts = append(parts, m[1]+": "+m[2])
	}
	return fakeReply{text: strings.Join(parts, ", ")}
}

func fillMgr(t *testing.T, api *fakeAPI, mod ...func(*ManagerOptions)) *testMgr {
	t.Helper()
	api.planCtx = 4096
	shared := []SharedProfile{}
	api.shapeShared = &shared
	gen := true // the fixed profiles of these tests stand for generated ones
	api.planGenerated = &gen
	return newTestMgr(t, api, append([]func(*ManagerOptions){func(o *ManagerOptions) {
		o.MemSample = 5 * time.Millisecond
		o.FillSettle = func() time.Duration { return 0 } // no settle hold unless a test asks
		o.FillReportEvery = 20 * time.Millisecond
	}}, mod...)...)
}

func fillOf(t *testing.T, r *Run, step int) *FillResult {
	t.Helper()
	for _, p := range r.Steps[step].Phases {
		if p.Type == "context_fill" {
			if p.Fill == nil {
				t.Fatalf("no fill result: %+v", p)
			}
			return p.Fill
		}
	}
	t.Fatalf("no context_fill phase: %+v", r.Steps[step].Phases)
	return nil
}

func TestContextFill_FillsToTheMarksRecallsAndReadsGrowth(t *testing.T) {
	api := newFake().prof("small", fakeProf{chatFn: recallingFill})
	var chats atomic.Int64
	// n1 gives up 40 MiB per turn sent; the other nodes stay put
	api.mem = func(id string, loading bool, n int) *int64 {
		chats.Store(int64(n))
		v := int64(9000)
		if id == "n1" {
			v -= int64(n) * 40
		}
		return &v
	}
	tm := fillMgr(t, api)
	tm.bench(`{"id":"fill","profile":"small","phases":[{"type":"load"},{"type":"context_fill"},{"type":"unload"}]}`)
	tm.suite(`{"id":"s","benchmarks":["fill"],"cooldown_s":0,"only_when_idle":false}`)
	r := tm.finished(tm.run("s").ID)
	if r.State != RunCompleted || r.Steps[0].Outcome != OutcomeReady {
		t.Fatalf("%s %s %s %s", r.State, r.Steps[0].Outcome, r.Steps[0].Reason, r.Steps[0].Error)
	}
	f := fillOf(t, r, 0)
	if f.Ctx != 4096 || f.FillPct != 90 || len(f.Marks) != 4 || f.Recall != 8 || f.RecallCorrect == nil || *f.RecallCorrect != 8 {
		t.Fatalf("fill: %+v", f)
	}
	for i, pct := range []int{25, 50, 75, 90} {
		m := f.Marks[i]
		target := 4096 * pct / 100
		if m.Pct != pct || m.Target != target || m.TokensHeld > target || m.TokensHeld < target-150 || m.Status != 200 {
			t.Errorf("mark %d: %+v (target %d)", i, m, target)
		}
		var n1 *FillNode
		for j := range m.Nodes {
			if m.Nodes[j].ID == "n1" {
				n1 = &m.Nodes[j]
			}
		}
		if n1 == nil || n1.GrowthMiB == nil || *n1.GrowthMiB < int64(40*(i+1))-40 || n1.KiBPerToken == nil || *n1.KiBPerToken <= 0 {
			t.Errorf("mark %d n1: %+v", i, n1)
		}
	}
	if f.ReachedTokens != f.Marks[3].TokensHeld || f.TargetTokens != 3686 || f.FillerSHA256 == "" || f.FillerChars < 3000*4 || f.StoppedBecause != "" {
		t.Errorf("summary: %+v", f)
	}

	// The same seed gives the same filler; the transcript holds no filler text.
	r2 := tm.finished(tm.run("s").ID)
	if f2 := fillOf(t, r2, 0); f2.FillerSHA256 != f.FillerSHA256 || f2.FillerChars != f.FillerChars {
		t.Errorf("same seed, other filler: %s %s", f2.FillerSHA256, f.FillerSHA256)
	}
	dir, _ := tm.store.RunDir(r.ID)
	tr, err := os.ReadFile(filepath.Join(dir, "step-000.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(tr), "harbour code is") || !strings.Contains(string(tr), "filler: seed 1") {
		t.Errorf("transcript: %.600s", tr)
	}

	// The measurements carry the readings.
	ms, err := tm.store.RunMeasurements(r.ID, MeasureQuery{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, m := range ms {
		seen[m.Metric]++
		if m.Metric == "fill_kib_per_token" && (m.Node != "ps5-a" || m.Conditions["fill_pct"] == nil) {
			t.Errorf("row %+v", m)
		}
		if m.Metric == "recall_correct" && (m.Value != 8 || m.Of == nil || *m.Of != 8) {
			t.Errorf("recall row %+v", m)
		}
	}
	// The benchmark report carries the readings, one row per mark and machine.
	zero := 0
	rep, err := BuildReport(r, ms, ReportOptions{Step: &zero})
	if err != nil {
		t.Fatal(err)
	}
	md := ReportMarkdown(rep)
	for _, want := range []string{"Context fill", "| Mark | Tokens held |", "| 90 % |", "| ps5-a | true |", "Recall: 8 of 8 planted facts", "Context 4,096 tokens, filled to 90 % (3,686 tokens)"} {
		if !strings.Contains(md, want) {
			t.Errorf("report lacks %q:\n%s", want, md)
		}
	}
	if seen["fill_tokens"] != 4 || seen["fill_kib_per_token"] < 4 || seen["fill_growth_mib"] < 4 || seen["recall_correct"] != 1 || seen["prefill_tps"] < 4 {
		t.Errorf("metrics %v", seen)
	}
}

func TestContextFill_SeedChangesTheFiller(t *testing.T) {
	a := newFiller(1, 4, 4096*90/100)
	b := newFiller(1, 4, 4096*90/100)
	c := newFiller(2, 4, 4096*90/100)
	pa, pb, pc := a.paragraph(0), b.paragraph(0), c.paragraph(0)
	if pa != pb || pa == pc || len(pa) < 100 {
		t.Fatalf("%q / %q / %q", pa, pb, pc)
	}
	if a.code(3) != b.code(3) || a.code(3) == c.code(3) {
		t.Fatal("codes")
	}
}

func TestContextFill_RecallScoring(t *testing.T) {
	f := newFiller(5, 3, 1000)
	codes := []string{f.code(1), f.code(2), f.code(3)}
	ans := fmt.Sprintf("01: %s, 02: 0000, 03: %s", codes[0], codes[2])
	if got := f.score(ans); got != 2 {
		t.Fatalf("score %d", got)
	}
}

// Before a turn: a machine that would fall below the stop line with the
// next turn's expected growth stops the step; the model is unloaded and the
// run goes on with the next benchmark.
func TestContextFill_StopLineBeforeATurn(t *testing.T) {
	api := newFake().prof("small", fakeProf{chatFn: recallingFill}).prof("gemma", fakeProf{})
	api.mem = func(id string, loading bool, n int) *int64 {
		v := int64(9000)
		if id == "n1" {
			v = 2600 - int64(n)*350 // 2250 after the first turn: the next one would need about 350 more
		}
		return &v
	}
	tm := fillMgr(t, api)
	tm.bench(`{"id":"fill","profile":"small","phases":[{"type":"load"},{"type":"context_fill","marks_pct":[25,50,90]},{"type":"unload"}]}`)
	tm.bench(gemmaBench)
	tm.suite(`{"id":"s","benchmarks":["fill","gemma"],"cooldown_s":0,"only_when_idle":false}`)
	r := tm.finished(tm.run("s").ID)
	st := r.Steps[0]
	f := fillOf(t, r, 0)
	if st.Outcome != OutcomeFailed || !strings.Contains(st.Reason, "stop line") || !strings.Contains(f.StoppedBecause, "ps5-a") || len(f.Marks) != 1 {
		t.Fatalf("%s %q %+v", st.Outcome, st.Reason, f)
	}
	if !strings.Contains(strings.Join(api.callList(), " "), "unload:small") {
		t.Fatalf("not unloaded: %v", api.callList())
	}
	if r.Steps[1].Outcome != OutcomeReady {
		t.Fatalf("the run did not go on: %+v", r.Steps[1])
	}
}

// During a turn: a sample below the stop line cuts the turn off.
func TestContextFill_StopLineDuringATurn(t *testing.T) {
	api := newFake().prof("small", fakeProf{chatFn: func(body string) fakeReply {
		r := recallingFill(body)
		r.delay = 400 * time.Millisecond
		return r
	}})
	api.mem = func(id string, loading bool, n int) *int64 {
		v := int64(9000)
		if id == "n2" && n >= 2 { // another console drops while the second turn runs
			v = 1500
		}
		return &v
	}
	tm := fillMgr(t, api)
	tm.bench(`{"id":"fill","profile":"small","timeout_s":60,"phases":[{"type":"load"},{"type":"context_fill","marks_pct":[25,50]},{"type":"unload"}]}`)
	tm.suite(`{"id":"s","benchmarks":["fill"],"cooldown_s":0,"only_when_idle":false,"stop_on_failure":true}`)
	start := time.Now()
	r := tm.finished(tm.run("s").ID)
	f := fillOf(t, r, 0)
	if r.Steps[0].Outcome != OutcomeFailed || !strings.Contains(f.StoppedBecause, "ps5-b") || !strings.Contains(f.StoppedBecause, "1,500 MiB") || len(f.Marks) != 2 || f.Marks[1].Status == 200 {
		t.Fatalf("%s %q %+v", r.Steps[0].Outcome, r.Steps[0].Reason, f)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the turn was not cut off")
	}
}

// Preflight: a shape shared with a profile a person wrote refuses the step
// before anything loads; shared only with generated profiles, or with the
// check off, it runs; a scheduler that does not say refuses too.
func TestContextFill_RefusesASharedShape(t *testing.T) {
	t1, f0 := true, false
	yes, no := &t1, &f0
	for _, c := range []struct {
		name    string
		shared  *[]SharedProfile
		gen     *bool // the plan's generated for the step's own profile (nil: not said)
		opt     string
		outcome string
		why     string
	}{
		{"a person's profile", &[]SharedProfile{{ID: "small-8k", Generated: false}, {ID: "bench-x", Generated: true}}, yes, "", OutcomeRefused, "small-8k"},
		{"generated only", &[]SharedProfile{{ID: "bench-x", Generated: true}}, yes, "", OutcomeReady, ""},
		{"check off", &[]SharedProfile{{ID: "small-8k"}}, yes, `,"refuse_shared_shape":false`, OutcomeReady, ""},
		{"not said", nil, yes, "", OutcomeRefused, "does not say"},
		// The step's own profile: a written one is refused, with the check off
		// too; a generated one runs; a plan that does not say is refused.
		{"own profile written", &[]SharedProfile{}, no, "", OutcomeRefused, "profile small is a profile a person wrote"},
		{"own profile written, check off", &[]SharedProfile{}, no, `,"refuse_shared_shape":false`, OutcomeRefused, "use the recipe"},
		{"own profile generated", &[]SharedProfile{}, yes, "", OutcomeReady, ""},
		{"own profile not said", &[]SharedProfile{}, nil, "", OutcomeRefused, "does not say whether profile small was generated"},
	} {
		t.Run(c.name, func(t *testing.T) {
			api := newFake().prof("small", fakeProf{chatFn: recallingFill})
			tm := fillMgr(t, api)
			api.mu.Lock()
			api.shapeShared = c.shared
			api.planGenerated = c.gen
			api.mu.Unlock()
			tm.bench(`{"id":"fill","profile":"small","phases":[{"type":"load"},{"type":"context_fill","marks_pct":[25]` + c.opt + `},{"type":"unload"}]}`)
			tm.suite(`{"id":"s","benchmarks":["fill"],"cooldown_s":0,"only_when_idle":false}`)
			r := tm.finished(tm.run("s").ID)
			st := r.Steps[0]
			if st.Outcome != c.outcome || !strings.Contains(st.Reason, c.why) {
				t.Fatalf("%s %q", st.Outcome, st.Reason)
			}
			if c.outcome == OutcomeRefused && strings.Contains(strings.Join(api.callList(), " "), "load:small") {
				t.Fatalf("loaded anyway: %v", api.callList())
			}
		})
	}
}

// A conversation benchmark sends exactly what it sent before: the fill
// changes nothing for it (golden bodies).
func TestContextFill_ConversationBodiesUnchanged(t *testing.T) {
	api := newFake().prof("c", fakeProf{})
	tm := newTestMgr(t, api)
	tm.bench(`{"id":"conv","profile":"c","phases":[{"type":"load"},{"type":"conversation","max_tokens":8,"turns":[{"content":"first"},{"content":"second"}]},{"type":"unload"}]}`)
	tm.suite(`{"id":"s","benchmarks":["conv"],"cooldown_s":0,"only_when_idle":false}`)
	tm.finished(tm.run("s").ID)
	api.mu.Lock()
	bodies := append([]string(nil), api.bodies...)
	api.mu.Unlock()
	want := []string{
		`{"cache_prompt":true,"max_tokens":8,"messages":[{"role":"user","content":"first"}],"model":"c","seed":42,"stream":false,"temperature":0}`,
		`{"cache_prompt":true,"max_tokens":8,"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"hello from c"},{"role":"user","content":"second"}],"model":"c","seed":42,"stream":false,"temperature":0}`,
	}
	if len(bodies) != 2 || bodies[0] != want[0] || bodies[1] != want[1] {
		t.Fatalf("bodies:\n%s", strings.Join(bodies, "\n"))
	}
}
