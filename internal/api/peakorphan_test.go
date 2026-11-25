package api

import (
	"io"
	"log/slog"
	"sort"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// A peak_cycles row whose shape no profile has any more (deleted or
// changed) is kept, marked no_profile with the note "no profile has this
// shape now"; a row of a profile in force is marked no_profile false.
func TestNeedsPeakCyclesMarkShapesWithoutAProfile(t *testing.T) {
	st := history.Open(history.Options{Dir: t.TempDir(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	t.Cleanup(st.Close)
	e := histEnv(t, st)
	cfg := e.api.conf()
	var names []string
	for n := range cfg.Profiles {
		names = append(names, n)
	}
	if len(names) == 0 {
		t.Fatal("the test configuration has no profile")
	}
	sort.Strings(names)
	live := scheduler.ShapeOf(cfg.Profiles[names[0]])
	const gone = `["llama","deleted.gguf",4096,1,"f16",false,[]]`
	now := time.Now().UnixMilli()
	st.Record(unloadEvent(now-2000, names[0], live, true, map[string]*int{"a": mib(4000)}, "a"))
	st.Record(unloadEvent(now-1000, "deleted", gone, true, map[string]*int{"a": mib(3000)}, "a"))
	m := peakView(t, e)
	if r := peakRow(t, m, gone, 1); r["no_profile"] != true || r["note"] != "no profile has this shape now" || r["cycles"] != 1.0 {
		t.Fatalf("the row of the deleted profile: %v", r)
	}
	if r := peakRow(t, m, live, 1); r["no_profile"] != false || r["note"] != nil {
		t.Fatalf("the row of %s: %v", names[0], r)
	}
}
