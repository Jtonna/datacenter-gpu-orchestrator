package usage

import (
	"database/sql"
	"fmt"
	"testing"
	"time"
)

func dumpRollups(t *testing.T, db *sql.DB) string {
	t.Helper()
	out := ""
	for _, table := range []string{"request_hours", "request_days"} {
		rows, err := db.Query(`SELECT bucket, profile, requests, done, failed, refused, cancelled, tokens_in, tokens_reused, tokens_out,
  wait_ms, duration_ms, prefill_tokens, round(prefill_ms, 6), decode_tokens, round(decode_ms, 6) FROM ` + table + ` ORDER BY bucket, profile`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			v := make([]any, 16)
			p := make([]any, 16)
			for i := range v {
				p[i] = &v[i]
			}
			rows.Scan(p...)
			out += fmt.Sprintln(table, v)
		}
		rows.Close()
	}
	return out
}

// Migration 4 on a file that holds inference rows of both kinds and
// monitoring passthrough rows (/slots, /props, /health, a bare
// /upstream/<model>), across two hours and a UTC day: the monitoring rows
// are gone, everything else stays, and the hourly and daily totals equal
// those of a file that never recorded them.
func TestMigration4_DropsNonInferenceRowsAndFixesTheTotals(t *testing.T) {
	type rec struct {
		kind, path string
		end        End
		running    bool
	}
	keep := []rec{
		{KindV1, "/v1/chat/completions", done(2460, 2000, 318), false},
		{KindUpstream, "/upstream/alpha/v1/chat/completions", done(120, 0, 30), false},
		{KindUpstream, "/upstream/alpha/completion", End{Status: StatusFailed, Reason: "stall", TokensIn: -1, Reused: -1, TokensOut: -1, PrefillTPS: -1, DecodeTPS: -1}, false},
		{KindUpstream, "/upstream/alpha/embeddings", done(40, 0, 0), false},
		{KindV1, "/v1/completions", End{Status: StatusRefused, Reason: "unknown model", TokensIn: -1, Reused: -1, TokensOut: -1, PrefillTPS: -1, DecodeTPS: -1}, false},
	}
	junk := []rec{
		{KindUpstream, "/upstream/alpha/slots", done(-1, -1, -1), false},
		{KindUpstream, "/upstream/alpha/props", done(-1, -1, -1), false},
		{KindUpstream, "/upstream/alpha/health", End{Status: StatusFailed, Reason: "upstream_status", HTTPStatus: 503, TokensIn: -1, Reused: -1, TokensOut: -1, PrefillTPS: -1, DecodeTPS: -1}, false},
		{KindUpstream, "/upstream/alpha", done(-1, -1, -1), false},
		{KindUpstream, "/upstream/alpha/tokenize", done(-1, -1, -1), false},
		{KindUpstream, "/upstream/alpha/slots", End{}, true}, // left running by a stop
	}
	record := func(dir string, withJunk bool) {
		c := &clock{t: time.Date(2026, 11, 23, 22, 50, 0, 0, time.UTC)}
		st := Open(Options{Dir: dir, Now: c.now})
		r := NewRecorder(st)
		for round := 0; round < 3; round++ { // 22:50, 23:30, 00:10 the next day
			list := keep
			if withJunk {
				list = append(append([]rec{}, junk...), keep...)
			}
			for _, x := range list {
				b := begin("alpha")
				b.Kind, b.Path = x.kind, x.path
				tr := r.Queued(b)
				tr.Started(Placement{}, nil)
				c.add(time.Second)
				if !x.running {
					tr.Ended(x.end)
				}
			}
			c.add(40 * time.Minute)
		}
		if err := r.Flush(t.Context()); err != nil {
			t.Fatal(err)
		}
		st.Close()
	}
	clean, dirty := t.TempDir(), t.TempDir()
	record(clean, false)
	record(dirty, true)

	// The dirty file as the first image left it: at schema version 3.
	db, err := sql.Open("sqlite", "file:"+dirty+"/"+FileName)
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(`DELETE FROM usage_migrations WHERE version = 4`)
	var before int
	db.QueryRow(`SELECT COUNT(*) FROM client_requests`).Scan(&before)
	db.Close()
	if before != 3*(len(keep)+len(junk)) {
		t.Fatalf("%d rows before", before)
	}

	want := func(dir string) (string, int, *Store) {
		st := openT(t, Options{Dir: dir})
		var n int
		st.DB().QueryRow(`SELECT COUNT(*) FROM client_requests`).Scan(&n)
		return dumpRollups(t, st.DB()), n, st
	}
	cleanTotals, cleanN, _ := want(clean)
	dirtyTotals, dirtyN, st := want(dirty)
	if dirtyN != 3*len(keep) || dirtyN != cleanN {
		t.Fatalf("%d rows after, want %d", dirtyN, cleanN)
	}
	if dirtyTotals != cleanTotals {
		t.Fatalf("totals after the clean-up:\n%s\nwant:\n%s", dirtyTotals, cleanTotals)
	}
	var v int
	st.DB().QueryRow(`SELECT MAX(version) FROM usage_migrations`).Scan(&v)
	if v != 4 || SchemaVersion() != 4 {
		t.Fatalf("version %d", v)
	}
	var paths []string
	rows, _ := st.DB().Query(`SELECT DISTINCT path FROM client_requests ORDER BY path`)
	for rows.Next() {
		var p string
		rows.Scan(&p)
		paths = append(paths, p)
	}
	rows.Close()
	if fmt.Sprint(paths) != "[/upstream/alpha/completion /upstream/alpha/embeddings /upstream/alpha/v1/chat/completions /v1/chat/completions /v1/completions]" {
		t.Fatalf("%v", paths)
	}
}

func TestIsInference(t *testing.T) {
	for _, c := range []struct {
		method, path string
		want         bool
	}{
		{"POST", "/v1/chat/completions", true}, {"POST", "/chat/completions", true}, {"POST", "/completion", true},
		{"POST", "/completions", true}, {"POST", "/v1/completions", true}, {"POST", "/v1/embeddings", true},
		{"POST", "/embeddings", true}, {"POST", "/embedding", true}, {"POST", "/infill", true}, {"POST", "/v1/responses", true},
		{"POST", "/v1/messages", true}, {"POST", "/rerank", true}, {"POST", "/v1/rerank", true}, {"POST", "/v1/reranking", true},
		{"POST", "/v1/audio/transcriptions", true}, {"POST", "/v1/audio/speech", true},
		{"GET", "/v1/chat/completions", false}, {"GET", "/slots", false}, {"GET", "/props", false}, {"POST", "/props", false},
		{"GET", "/health", false}, {"GET", "/metrics", false}, {"GET", "/v1/models", false}, {"GET", "/models", false},
		{"POST", "/tokenize", false}, {"POST", "/detokenize", false}, {"POST", "/apply-template", false},
		{"GET", "/lora-adapters", false}, {"POST", "/lora-adapters", false}, {"POST", "/slots/0", false},
		{"POST", "/v1/chat/completions/input_tokens", false}, {"POST", "/v1/messages/count_tokens", false},
		{"POST", "/v1/chat/completions/control", false}, {"POST", "/v1/chat/completions/", false}, {"POST", "/", false},
	} {
		if got := IsInference(c.method, c.path); got != c.want {
			t.Errorf("%s %s: %v", c.method, c.path, got)
		}
	}
	if upstreamTail("/upstream/alpha/v1/chat/completions") != "/v1/chat/completions" || upstreamTail("/upstream/alpha") != "/" {
		t.Fatal("upstreamTail")
	}
}
