package usage

import (
	"context"
	"database/sql"
)

// SessionTokens is what the request log says of one load session: the most
// tokens one finished request held (tokens_in + tokens_out: tokens_in is the
// whole prompt, reused tokens included, so tokens_reused is not added),
// how many of its requests had token counts, and whether the log still holds
// the cycle (Trimmed: its oldest row is newer than the ready time). Read-only;
// the profile and window use the client_requests_profile index.
type SessionTokens struct {
	MaxHeld int
	Counted int
	Trimmed bool
}

// SessionTokens reads the requests of profile's load session queued in
// [from, to] (unix ms); the log is trimmed for it when its oldest row is
// newer than need (the instance's ready time).
func (s *Store) SessionTokens(ctx context.Context, profile, session string, from, need, to int64) (SessionTokens, error) {
	var out SessionTokens
	if !s.On() {
		return out, nil
	}
	var held, n sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(COALESCE(tokens_in,0)+COALESCE(tokens_out,0)), COUNT(*)
	  FROM client_requests WHERE profile=? AND queued_at>=? AND queued_at<=? AND session_id=? AND tokens_in IS NOT NULL`, profile, from, to, session).Scan(&held, &n)
	if err != nil {
		return out, err
	}
	out.MaxHeld, out.Counted = int(held.Int64), int(n.Int64)
	var oldest sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MIN(queued_at) FROM client_requests`).Scan(&oldest); err != nil {
		return out, err
	}
	out.Trimmed = !oldest.Valid || oldest.Int64 > need
	return out, nil
}
