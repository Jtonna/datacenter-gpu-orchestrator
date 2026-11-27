package bench

import (
	"testing"
	"time"
)

// The progress document names the profile in flight as people see it
// (profile_name: the shown name, "TEST - ..." for a test profile), so the run
// banner and the cancel dialog need not look it up.
func TestProgressNamesTheProfileInFlight(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	tm := newTestMgr(t, api)
	tm.m.SetProfileNamer(func(p string) string { return "TEST - " + p })
	tm.bench(gemmaBench)
	tm.suite(`{"id":"s","benchmarks":["gemma"],"cooldown_s":0,"only_when_idle":false}`)
	run := tm.run("s")
	saw := ""
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && saw == "" {
		if d := tm.m.State(); d.Run != nil {
			if c, ok := d.Run["current"].(map[string]any); ok {
				saw, _ = c["profile_name"].(string)
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	tm.finished(run.ID)
	if saw != "TEST - gemma" {
		t.Fatalf("current.profile_name %q", saw)
	}
}
