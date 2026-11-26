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
	// tokens_in is the whole prompt (reused included), as usage.prompt_tokens says.
	exec(t, s, ins, 1000, "m", 100, 0, 8, "s1")
	exec(t, s, ins, 2000, "m", 2900, 2000, 8, "s1")
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

// A four-turn conversation with cache_prompt, as llama-server logs it: each
// row's tokens_in is the whole prompt (prompt_n + cache_n, usage.prompt_tokens)
// and tokens_reused is cache_n, a part of it. The most the session held is the
// last turn's prompt_n + cache_n + predicted_n, never a sum over the turns and
// never tokens_in + tokens_reused (that counts the cached part twice).
func TestSessionTokens_FourTurnsHeldIsTheLastTurn(t *testing.T) {
	s := openT(t, Options{Dir: t.TempDir(), Now: func() time.Time { return time.UnixMilli(5_000_000) }})
	ins := `INSERT INTO client_requests (status, http_status, kind, stream, queued_at, profile, tokens_in, tokens_reused, tokens_out, session_id, caller, caller_kind)
	  VALUES ('done', 200, 'chat', 1, ?, 'm', ?, ?, ?, 's1', 'c', 'key')`
	// turn: prompt_n, cache_n, predicted_n (the next turn reuses the whole earlier conversation)
	turns := [][3]int{{1984, 0, 30}, {1954, 2014, 30}, {1954, 3998, 30}, {1144, 5982, 30}}
	for i, tn := range turns {
		exec(t, s, ins, 1000+i*100, tn[0]+tn[1], tn[1], tn[2])
	}
	got, err := s.SessionTokens(ctxT(t), "m", "s1", 500, 1000, 3000)
	last := turns[3][0] + turns[3][1] + turns[3][2] // 7,156
	if err != nil || got.MaxHeld != last || got.Counted != 4 {
		t.Fatalf("held %d, want the last turn's %d (prompt_n + cache_n + predicted_n): %+v %v", got.MaxHeld, last, got, err)
	}
}
