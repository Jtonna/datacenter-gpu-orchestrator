package bench

import "testing"

// A run's final environment (the llama binary it loaded, the workers) is
// stored before the run is marked finished, so whoever waits for the finished
// state reads the whole environment. The trigger makes the finishing write
// fail when the stored environment still lacks the llama binary, which is what
// the old order (state first, environment after) did; no timing is involved.
func TestRunEnvironmentIsStoredBeforeTheRunFinishes(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	tm := newTestMgr(t, api)
	if _, err := tm.store.db.Exec(`CREATE TRIGGER env_before_finish BEFORE UPDATE OF state ON runs
	  WHEN NEW.state IN ('completed','failed') AND (OLD.env IS NULL OR json_extract(OLD.env,'$.llama_binary') IS NULL)
	  BEGIN SELECT RAISE(ABORT, 'finished before its environment was stored'); END`); err != nil {
		t.Fatal(err)
	}
	tm.bench(`{"id":"b","profile":"gemma","phases":[{"type":"load"},{"type":"unload"}]}`)
	tm.suite(`{"id":"s","benchmarks":["b"],"only_when_idle":false}`)
	id := tm.run("s").ID
	r := tm.finished(id)
	if r.State != RunCompleted || r.Env == nil || r.Env.LlamaBinary != "llama-server-rpc" || len(r.Env.Workers) != 3 {
		t.Fatalf("%s %+v", r.State, r.Env)
	}
}
