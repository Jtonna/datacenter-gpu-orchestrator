package bench

import "testing"

// ShortReason: a refusal in a few words (board Bench-Runs-Group: "Does not
// fit on 1 zone"); a failure and a step without a reason have none.
func TestShortReason(t *testing.T) {
	for _, c := range []struct {
		outcome, reason string
		n               int
		want            string
	}{
		{OutcomeRefused, "profile bench-x does not fit ra-01 by the memory reservation rule: 9,100 MiB needed, 7,800 MiB free; nothing of ours is loaded", 1, "Does not fit on 1 zone"},
		{OutcomeRefused, "profile bench-x does not fit the nodes it is forced onto: ra-01 short by 1,200 MiB", 0, "Does not fit"},
		// a run stored before build-92 keeps its old sentence: still matched
		{OutcomeRefused, "any 3 consoles: only 2 can take a load now", 3, "Not enough zones free"},
		{OutcomeRefused, "any 3 zones: only 2 can take a load now", 3, "Not enough zones free"},
		{OutcomeRefused, "placement refused: all zones: no zone can take a load now", 0, "Not enough zones free"},
		{OutcomeRefused, `placement refused: zone "node-3" cannot take a load now (paused)`, 1, "Not enough zones free"},
		{OutcomeRefused, `placement refused: "node-9" is not a placeable zone (an agent zone that is not excluded and not the scheduler's machine)`, 1, "Not a placeable zone"},
		{OutcomeRefused, `placement refused: "node-9" is not a console (an agent node that is not excluded and not the scheduler's machine)`, 1, "Not a placeable zone"},
		{OutcomeRefusedByExclusion, "node ra-02 is excluded from scheduling", 2, "A zone is excluded"},
		{OutcomeRefused, "the model server answered 400: context too large for this build of the server; see the log", 1, "The model server answered 400"},
		{OutcomeFailed, "child exited during load: exit code 1", 4, ""},
		{OutcomeRefused, "", 1, ""},
	} {
		if got := ShortReason(c.outcome, c.reason, c.n); got != c.want {
			t.Errorf("ShortReason(%s, %q, %d) = %q, want %q", c.outcome, c.reason, c.n, got, c.want)
		}
	}
}
