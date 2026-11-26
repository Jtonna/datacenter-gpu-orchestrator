package benchspec

import (
	"slices"
	"strings"
	"testing"
)

// The context_fill phase (B8): its options, defaults and limits.
func TestContextFill_OptionsDefaultsAndLimits(t *testing.T) {
	ok := `{"id":"f","profile":"p","phases":[{"type":"load"},{"type":"context_fill"},{"type":"unload"}]}`
	if err := validate(t, ok); err != nil {
		t.Fatalf("defaults: %v", err)
	}
	b := mustBench(t, ok)
	p := b.Phases[1]
	if p.FillTarget() != 90 || !slices.Equal(p.FillMarks(), []int{25, 50, 75, 90}) || p.RecallCount() != 8 || p.StopLine() != 2000 ||
		!p.RefusesSharedShape() || p.FillAnswerTokens() != 128 || p.FillSeed() != 1 {
		t.Fatalf("defaults: %d %v %d %d %v %d %d", p.FillTarget(), p.FillMarks(), p.RecallCount(), p.StopLine(), p.RefusesSharedShape(), p.FillAnswerTokens(), p.FillSeed())
	}
	set := mustBench(t, `{"id":"f","profile":"p","phases":[{"type":"load"},{"type":"context_fill","fill_pct":90,"marks_pct":[50,90],"recall":0,"seed":7,"max_tokens":64,"stop_available_mib":3000,"refuse_shared_shape":false},{"type":"unload"}]}`)
	if err := set.Validate(okAll); err != nil {
		t.Fatal(err)
	}
	q := set.Phases[1]
	if !slices.Equal(q.FillMarks(), []int{50, 90}) || q.RecallCount() != 0 || q.FillSeed() != 7 || q.FillAnswerTokens() != 64 || q.StopLine() != 3000 || q.RefusesSharedShape() {
		t.Fatalf("set: %+v", q)
	}
	// marks without fill_pct: the last mark is the target
	m := mustBench(t, `{"id":"f","profile":"p","phases":[{"type":"context_fill","marks_pct":[30,60]}]}`).Phases[0]
	if m.FillTarget() != 60 || !slices.Equal(m.FillMarks(), []int{30, 60}) {
		t.Fatalf("marks only: %d %v", m.FillTarget(), m.FillMarks())
	}
	// fill_pct below the default marks: the marks under it stay, the target ends them
	m = mustBench(t, `{"id":"f","profile":"p","phases":[{"type":"context_fill","fill_pct":60}]}`).Phases[0]
	if !slices.Equal(m.FillMarks(), []int{25, 50, 60}) {
		t.Fatalf("fill 60: %v", m.FillMarks())
	}

	for js, want := range map[string]string{
		`{"type":"context_fill","fill_pct":96}`:                           "fill_pct must be 1 to 95",
		`{"type":"context_fill","fill_pct":0}`:                            "fill_pct must be 1 to 95",
		`{"type":"context_fill","marks_pct":[50,40]}`:                     "marks_pct must rise",
		`{"type":"context_fill","marks_pct":[50,90],"fill_pct":80}`:       "the last of marks_pct must be fill_pct",
		`{"type":"context_fill","recall":33}`:                             "recall must be 0 to 32",
		`{"type":"context_fill","stop_available_mib":100}`:                "stop_available_mib must be 500 to",
		`{"type":"context_fill","turns":[{"content":"x"}]}`:               `option "turns" does not belong`,
		`{"type":"conversation","fill_pct":50,"turns":[{"content":"x"}]}`: `option "fill_pct" does not belong`,
		`{"type":"context_fill","max_tokens":5000}`:                       "max_tokens must be 1 to 1024",
	} {
		err := validate(t, `{"id":"f","profile":"p","phases":[`+js+`]}`)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", js, err, want)
		}
	}
	// The schema offers it, with its options, after conversation.
	var found *PhaseSchema
	for i, ps := range Schema().PhaseTypes {
		if ps.Type == PhaseContextFill {
			found = &Schema().PhaseTypes[i]
		}
	}
	if found == nil {
		t.Fatal("schema has no context_fill")
	}
	var names []string
	for _, o := range found.Options {
		names = append(names, o.Name)
	}
	for _, n := range []string{"fill_pct", "marks_pct", "recall", "seed", "max_tokens", "stop_available_mib", "refuse_shared_shape"} {
		if !slices.Contains(names, n) {
			t.Errorf("schema option %s missing: %v", n, names)
		}
	}
	if i := slices.Index(PhaseTypes, PhaseContextFill); i != slices.Index(PhaseTypes, PhaseConversation)+1 {
		t.Errorf("order: %v", PhaseTypes)
	}
}

// Nothing changes for a conversation phase: its options are exactly as before.
func TestContextFill_ConversationUnchanged(t *testing.T) {
	if got := allowed[PhaseConversation]; !slices.Equal(got, []string{"system", "turns", "max_tokens", "temperature", "seed", "cache_prompt", "stream"}) {
		t.Fatalf("conversation options changed: %v", got)
	}
}
