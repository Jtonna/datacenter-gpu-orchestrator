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
		{OutcomeRefused, "any 3 consoles: only 2 can take a load now", 3, "Not enough zones free"},
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
