package usage

import (
	"database/sql"
	"time"
)

// requestsSQL is migration 2: the request log and its roll-ups.
var requestsSQL = `
CREATE TABLE client_requests (
  id             INTEGER PRIMARY KEY,
  status         TEXT NOT NULL,
  reason         TEXT,
  message        TEXT,
  http_status    INTEGER NOT NULL DEFAULT 0,
  kind           TEXT NOT NULL,
  path           TEXT,
  stream         INTEGER NOT NULL DEFAULT 0,
  queued_at      INTEGER NOT NULL,
  started_at     INTEGER,
  first_token_at INTEGER,
  ended_at       INTEGER,
  profile        TEXT NOT NULL,
  requested      TEXT,
  model_file     TEXT,
  tokens_in      INTEGER,
  tokens_reused  INTEGER,
  tokens_out     INTEGER,
  prefill_tps    REAL,
  decode_tps     REAL,
  prefill_ms     REAL,
  decode_ms      REAL,
  sched_id       INTEGER NOT NULL DEFAULT 0,
  instance_id    INTEGER NOT NULL DEFAULT 0,
  session_id     TEXT,
  host_node      TEXT,
  nodes          TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(nodes)),
  caller         TEXT NOT NULL,
  caller_kind    TEXT NOT NULL,
  source_addr    TEXT,
  bench_run      TEXT
);
CREATE INDEX client_requests_queued ON client_requests (queued_at);
CREATE INDEX client_requests_open ON client_requests (status) WHERE status IN ('queued','running');
CREATE INDEX client_requests_profile ON client_requests (profile, queued_at);
CREATE INDEX client_requests_bench ON client_requests (bench_run) WHERE bench_run IS NOT NULL;
` + rollupTable("request_hours") + rollupTable("request_days")

// rollupTable is one roll-up: per bucket (UTC hour or day start, unix ms)
// and profile, the finished requests by status and their sums. A request
// counts in the bucket of its arrival (queued_at). Rates are sums: tokens
// over milliseconds, for the requests whose figures are known.
func rollupTable(name string) string {
	return `
CREATE TABLE ` + name + ` (
  bucket         INTEGER NOT NULL,
  profile        TEXT NOT NULL,
  requests       INTEGER NOT NULL DEFAULT 0,
  done           INTEGER NOT NULL DEFAULT 0,
  failed         INTEGER NOT NULL DEFAULT 0,
  refused        INTEGER NOT NULL DEFAULT 0,
  cancelled      INTEGER NOT NULL DEFAULT 0,
  tokens_in      INTEGER NOT NULL DEFAULT 0,
  tokens_reused  INTEGER NOT NULL DEFAULT 0,
  tokens_out     INTEGER NOT NULL DEFAULT 0,
  wait_ms        INTEGER NOT NULL DEFAULT 0,
  duration_ms    INTEGER NOT NULL DEFAULT 0,
  prefill_tokens INTEGER NOT NULL DEFAULT 0,
  prefill_ms     REAL NOT NULL DEFAULT 0,
  decode_tokens  INTEGER NOT NULL DEFAULT 0,
  decode_ms      REAL NOT NULL DEFAULT 0,
  PRIMARY KEY (bucket, profile)
);`
}

// Bucket starts (UTC).
func hourOf(ms int64) int64 { return ms - ms%int64(time.Hour/time.Millisecond) }
func dayOf(ms int64) int64  { return ms - ms%int64(24*time.Hour/time.Millisecond) }

// rollupAdd is what one finished request adds to its buckets.
type rollupAdd struct {
	done, failed, refused, cancelled int
	in, reused, out                  int64
	wait, dur                        int64
	prefillTok, decodeTok            int64
	prefillMS, decodeMS              float64
}

func rollupOf(r *Request) rollupAdd {
	var a rollupAdd
	switch r.Status {
	case StatusDone:
		a.done = 1
	case StatusRefused:
		a.refused = 1
	case StatusCancelled:
		a.cancelled = 1
	default:
		a.failed = 1
	}
	if r.TokensIn != nil {
		a.in = int64(*r.TokensIn)
	}
	if r.TokensReused != nil {
		a.reused = int64(*r.TokensReused)
	}
	if r.TokensOut != nil {
		a.out = int64(*r.TokensOut)
	}
	a.wait, a.dur = r.WaitMS, r.DurationMS
	// Prefill: the prompt tokens actually processed (reused ones are not).
	if r.TokensIn != nil {
		proc := a.in - a.reused
		ms := r.prefillMS
		if ms <= 0 && r.PrefillTPS != nil && *r.PrefillTPS > 0 {
			ms = float64(proc) / *r.PrefillTPS * 1000
		}
		if proc > 0 && ms > 0 {
			a.prefillTok, a.prefillMS = proc, ms
		}
	}
	if r.TokensOut != nil {
		ms := r.decodeMS
		if ms <= 0 && r.DecodeTPS != nil && *r.DecodeTPS > 0 {
			ms = float64(a.out) / *r.DecodeTPS * 1000
		}
		if a.out > 0 && ms > 0 {
			a.decodeTok, a.decodeMS = a.out, ms
		}
	}
	return a
}

// addRollups adds a finished request to its hour and its day.
func addRollups(tx *sql.Tx, r *Request) error {
	a := rollupOf(r)
	for _, b := range []struct {
		table  string
		bucket int64
	}{{"request_hours", hourOf(r.QueuedAt)}, {"request_days", dayOf(r.QueuedAt)}} {
		_, err := tx.Exec(`INSERT INTO `+b.table+` (bucket, profile, requests, done, failed, refused, cancelled, tokens_in, tokens_reused, tokens_out,
  wait_ms, duration_ms, prefill_tokens, prefill_ms, decode_tokens, decode_ms) VALUES (?,?,1,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(bucket, profile) DO UPDATE SET requests=requests+1, done=done+excluded.done, failed=failed+excluded.failed,
  refused=refused+excluded.refused, cancelled=cancelled+excluded.cancelled, tokens_in=tokens_in+excluded.tokens_in,
  tokens_reused=tokens_reused+excluded.tokens_reused, tokens_out=tokens_out+excluded.tokens_out, wait_ms=wait_ms+excluded.wait_ms,
  duration_ms=duration_ms+excluded.duration_ms, prefill_tokens=prefill_tokens+excluded.prefill_tokens, prefill_ms=prefill_ms+excluded.prefill_ms,
  decode_tokens=decode_tokens+excluded.decode_tokens, decode_ms=decode_ms+excluded.decode_ms`,
			b.bucket, r.Profile, a.done, a.failed, a.refused, a.cancelled, a.in, a.reused, a.out, a.wait, a.dur,
			a.prefillTok, a.prefillMS, a.decodeTok, a.decodeMS)
		if err != nil {
			return err
		}
	}
	return nil
}
