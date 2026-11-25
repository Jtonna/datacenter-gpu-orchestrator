package bench

import (
	"encoding/json"
	"strings"
	"testing"
)

// floorRun runs a one-mark fill with the given growth history and setting
// on the fake scheduler, with memory from mem (nil: plenty).
func floorRun(t *testing.T, hist *GrowthHistory, setting *int, mem func(id string, loading bool, n int) *int64) (*Run, *FillResult) {
	t.Helper()
	api := newFake().prof("small", fakeProf{chatFn: recallingFill})
	api.growth, api.reserveKiB = hist, setting
	if mem != nil {
		api.mem = mem
	}
	tm := fillMgr(t, api)
	tm.bench(`{"id":"fill","profile":"small","phases":[{"type":"load"},{"type":"context_fill","marks_pct":[25],"stop_available_mib":3000},{"type":"unload"}]}`)
	tm.suite(`{"id":"s","benchmarks":["fill"],"cooldown_s":0,"only_when_idle":false}`)
	r := tm.finished(tm.run("s").ID)
	return r, fillOf(t, r, 0)
}

func intp(v int) *int { return &v }

// The stop check's growth per token: the highest stored reading for the
// shape, 12 with no history, never below the setting; the figure and where
// it came from are in the fill result.
func TestContextFill_GrowthFloor(t *testing.T) {
	for _, c := range []struct {
		name    string
		hist    *GrowthHistory
		setting *int
		floor   float64
		basis   string
	}{
		{"no history", &GrowthHistory{}, intp(8), 12, "no history: 12 KiB per token"},
		{"history below the setting", &GrowthHistory{KiBPerToken: 5.78, Readings: 2}, intp(8), 8,
			"the setting fill_reserve_kib_per_token: 8 KiB per token (highest of 2 readings for this shape: 5.78 KiB per token)"},
		{"history above the setting", &GrowthHistory{KiBPerToken: 9.73, Readings: 4}, intp(8), 9.73, "highest of 4 readings for this shape: 9.73 KiB per token"},
		{"no history, setting above 12", &GrowthHistory{}, intp(16), 16, "the setting fill_reserve_kib_per_token: 16 KiB per token (no history: 12 KiB per token)"},
		{"an older scheduler", nil, nil, 12, "no history (the scheduler does not report it): 12 KiB per token"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, f := floorRun(t, c.hist, c.setting, nil)
			if r.Steps[0].Outcome != OutcomeReady {
				t.Fatalf("%s %q", r.Steps[0].Outcome, r.Steps[0].Reason)
			}
			if f.GrowthFloorKiBPerToken != c.floor || f.GrowthFloorBasis != c.basis {
				t.Fatalf("floor %v %q, want %v %q", f.GrowthFloorKiBPerToken, f.GrowthFloorBasis, c.floor, c.basis)
			}
		})
	}
}

// A refusal before a turn names the figure it used and where it came from.
func TestContextFill_StopRefusalNamesTheGrowthFigure(t *testing.T) {
	tight := func(id string, loading bool, n int) *int64 {
		v := int64(9000)
		if id == "n1" {
			v = 3005 // 5 MiB above the stop line: any turn of about 1,000 tokens at 9.73 KiB is too much
		}
		return &v
	}
	r, f := floorRun(t, &GrowthHistory{KiBPerToken: 9.73, Readings: 4}, intp(8), tight)
	st := r.Steps[0]
	if st.Outcome != OutcomeFailed || len(f.Marks) != 0 {
		t.Fatalf("%s %q, %d marks", st.Outcome, st.Reason, len(f.Marks))
	}
	for _, want := range []string{"stopped at the stop line before the turn to 25 %", "at 9.73 KiB per token (highest of 4 readings for this shape: 9.73 KiB per token)", "below the stop line of 3,000 MiB"} {
		if !strings.Contains(f.StoppedBecause, want) || !strings.Contains(st.Reason, want) {
			t.Fatalf("refusal %q lacks %q", f.StoppedBecause, want)
		}
	}
}

// reasoning_chars and recall_note are always in the fill result: 0 and ""
// when there is nothing, so a reader can tell them from absent.
func TestContextFill_ReasoningAndRecallNoteAlwaysWritten(t *testing.T) {
	_, f := floorRun(t, &GrowthHistory{}, intp(8), nil)
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"reasoning_chars":0`, `"recall_note":""`, `"growth_floor_kib_per_token":12`, `"growth_floor_basis":"no history: 12 KiB per token"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("fill result lacks %s: %s", want, b)
		}
	}
}
