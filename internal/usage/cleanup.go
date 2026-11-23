package usage

import (
	"database/sql"
	"strings"
)

// upstreamTail is the path a passthrough row sent to the model server:
// what follows /upstream/<model> in its stored path ("/" when nothing).
func upstreamTail(path string) string {
	rest, ok := strings.CutPrefix(path, "/upstream/")
	if !ok {
		return path
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[i:]
	}
	return "/"
}

// dropNonInferenceRows (migration 4) deletes the passthrough rows that did
// not run the model (the image that shipped first recorded every
// /upstream call: /slots polls, /props, /health), and takes each deleted
// finished row's share back out of its hourly and daily totals: the exact
// inverse of addRollups, so the totals equal what they would have been had
// the rows never been recorded (also for hours whose other rows were
// trimmed already). A total left with no request is deleted. The rows
// were all POST or GET passthroughs; the method was not stored, so a row
// is kept exactly when its path is an inference route.
func dropNonInferenceRows(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT `+rowColumns+` FROM client_requests WHERE kind = ?`, KindUpstream)
	if err != nil {
		return err
	}
	all, err := scanRows(rows)
	if err != nil {
		return err
	}
	for i := range all {
		r := &all[i]
		if inferenceRoutes[upstreamTail(r.Path)] {
			continue
		}
		if r.Status != StatusQueued && r.Status != StatusRunning { // finished: it is in the totals
			if err := subRollups(tx, r); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`DELETE FROM client_requests WHERE id = ?`, r.ID); err != nil {
			return err
		}
	}
	for _, t := range []string{"request_hours", "request_days"} {
		if _, err := tx.Exec(`DELETE FROM ` + t + ` WHERE requests <= 0`); err != nil {
			return err
		}
	}
	return nil
}

// subRollups takes one finished request back out of its buckets.
func subRollups(tx *sql.Tx, r *Request) error {
	a := rollupOf(r)
	for _, b := range []struct {
		table  string
		bucket int64
	}{{"request_hours", hourOf(r.QueuedAt)}, {"request_days", dayOf(r.QueuedAt)}} {
		_, err := tx.Exec(`UPDATE `+b.table+` SET requests=requests-1, done=done-?, failed=failed-?, refused=refused-?, cancelled=cancelled-?,
  tokens_in=tokens_in-?, tokens_reused=tokens_reused-?, tokens_out=tokens_out-?, wait_ms=wait_ms-?, duration_ms=duration_ms-?,
  prefill_tokens=prefill_tokens-?, prefill_ms=prefill_ms-?, decode_tokens=decode_tokens-?, decode_ms=decode_ms-?
WHERE bucket = ? AND profile = ?`,
			a.done, a.failed, a.refused, a.cancelled, a.in, a.reused, a.out, a.wait, a.dur,
			a.prefillTok, a.prefillMS, a.decodeTok, a.decodeMS, b.bucket, r.Profile)
		if err != nil {
			return err
		}
	}
	return nil
}
