package usage

import (
	"testing"
	"time"
)

// SessionTokens: the most one request of a load session held, the rows
// counted, and whether the log still reaches back to the window's start.
func TestSessionTokens(t *testing.T) {
	s := openT(t, Options{Dir: t.TempDir(), Now: func() time.Time { return time.UnixMilli(5_000_000) }})
	ins := `INSERT INTO client_requests (status, http_status, kind, stream, queued_at, profile, tokens_in, tokens_reused, tokens_out, session_id, caller, caller_kind)
	  VALUES ('done', 200, 'chat', 0, ?, ?, ?, ?, ?, ?, 'c', 'key')`
	exec(t, s, ins, 1000, "m", 100, 0, 8, "s1")
	exec(t, s, ins, 2000, "m", 900, 2000, 8, "s1")
	exec(t, s, ins, 2500, "m", 9000, 0, 8, "s2") // another session
	exec(t, s, ins, 2600, "x", 9000, 0, 8, "s1") // another profile
	got, err := s.SessionTokens(ctxT(t), "m", "s1", 500, 1000, 3000)
	if err != nil || got.MaxHeld != 2908 || got.Counted != 2 || got.Trimmed {
		t.Fatalf("%+v %v", got, err)
	}
	if got, _ := s.SessionTokens(ctxT(t), "m", "s1", 0, 1500, 3000); got.Trimmed {
		t.Fatalf("not trimmed: %+v", got)
	}
	if got, _ := s.SessionTokens(ctxT(t), "m", "s1", 0, 100, 3000); !got.Trimmed {
		t.Fatalf("the log starts at 1000, the cycle at 100: %+v", got)
	}
}
