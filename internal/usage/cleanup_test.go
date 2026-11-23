package usage

import (
	"database/sql"
	"fmt"
	"strings"
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

// freshRollups is the hourly and daily totals rebuilt from scratch from the
// stored finished rows (a reference, in a transaction rolled back).
func freshRollups(t *testing.T, db *sql.DB) string {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	tx.Exec(`DELETE FROM request_hours`)
	tx.Exec(`DELETE FROM request_days`)
	rows, err := tx.Query(`SELECT `+rowColumns+` FROM client_requests WHERE status NOT IN (?, ?)`, StatusQueued, StatusRunning)
	if err != nil {
		t.Fatal(err)
	}
	all, err := scanRows(rows)
	if err != nil {
		t.Fatal(err)
	}
	for i := range all {
		if err := addRollups(tx, &all[i]); err != nil {
			t.Fatal(err)
		}
	}
	out := ""
	for _, table := range []string{"request_hours", "request_days"} {
		rows, err := tx.Query(`SELECT bucket, profile, requests, done, failed, refused, cancelled, tokens_in, tokens_reused, tokens_out,
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

// Migration 4 on a file written by the first image: inference rows (/v1
// and /upstream) and junk rows (monitoring passthrough: /slots, /props,
// /health, a bare /upstream/<model>, /tokenize; and a /v1 POST that is not
// inference) in four hours across a UTC day: both kinds in the same hour
// (22:50, 23:30), inference only (00:10 the next day), junk only (01:30).
// After it: only the inference rows remain, the hourly and daily totals
// equal a fresh roll-up of the kept rows and those of a file that never
// recorded the junk, the junk-only hour has no totals, and one INFO line
// gives the counts.
func TestMigration4_DropsNonInferenceRowsAndRebuildsTheTotals(t *testing.T) {
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
		{KindV1, "/v1/audio/translations", done(-1, -1, -1), false},
		{KindV1, "/v1/completions", End{Status: StatusRefused, Reason: "unknown model", TokensIn: -1, Reused: -1, TokensOut: -1, PrefillTPS: -1, DecodeTPS: -1}, false},
	}
	junk := []rec{
		{KindUpstream, "/upstream/alpha/slots", done(-1, -1, -1), false},
		{KindUpstream, "/upstream/alpha/props", done(-1, -1, -1), false},
		{KindUpstream, "/upstream/alpha/health", End{Status: StatusFailed, Reason: "upstream_status", HTTPStatus: 503, TokensIn: -1, Reused: -1, TokensOut: -1, PrefillTPS: -1, DecodeTPS: -1}, false},
		{KindUpstream, "/upstream/alpha", done(-1, -1, -1), false},
		{KindUpstream, "/upstream/alpha/tokenize", done(-1, -1, -1), false},
		{KindV1, "/v1/tokenize", done(-1, -1, -1), false},
		{KindUpstream, "/upstream/alpha/slots", End{}, true}, // left running by a stop
	}
	day := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	rounds := []struct {
		at   time.Time
		list []rec
	}{
		{day.Add(22*time.Hour + 50*time.Minute), append(append([]rec{}, junk...), keep...)},
		{day.Add(23*time.Hour + 30*time.Minute), append(append([]rec{}, keep...), junk...)},
		{day.Add(24*time.Hour + 10*time.Minute), keep},
		{day.Add(25*time.Hour + 30*time.Minute), junk},
	}
	record := func(dir string, withJunk bool) {
		c := &clock{t: rounds[0].at}
		lg, _ := newLog()
		st := Open(Options{Dir: dir, Now: c.now, Logger: lg})
		r := NewRecorder(st)
		for _, rd := range rounds {
			c.t = rd.at
			for _, x := range rd.list {
				if !withJunk && !inferenceRoutes[sentPath(&Request{Kind: x.kind, Path: x.path})] {
					continue
				}
				b := begin("alpha")
				b.Kind, b.Path = x.kind, x.path
				tr := r.Queued(b)
				tr.Started(Placement{}, nil)
				c.add(time.Second)
				if !x.running {
					tr.Ended(x.end)
				}
			}
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
	var before, junkHourTotals int
	db.QueryRow(`SELECT COUNT(*) FROM client_requests`).Scan(&before)
	db.Close()
	if want := 3*len(junk) + 3*len(keep); before != want { // junk in rounds 1, 2, 4; inference in 1, 2, 3
		t.Fatalf("%d rows before, want %d", before, want)
	}

	lg, logs := newLog()
	st := openT(t, Options{Dir: dirty, Logger: lg})
	cst := openT(t, Options{Dir: clean})
	var after, cleanN int
	st.DB().QueryRow(`SELECT COUNT(*) FROM client_requests`).Scan(&after)
	cst.DB().QueryRow(`SELECT COUNT(*) FROM client_requests`).Scan(&cleanN)
	if after != 3*len(keep) || after != cleanN {
		t.Fatalf("%d rows after, want %d", after, cleanN)
	}
	got := dumpRollups(t, st.DB())
	if fresh := freshRollups(t, st.DB()); got != fresh {
		t.Fatalf("totals after the clean-up:\n%s\nfresh roll-up of the kept rows:\n%s", got, fresh)
	}
	if want := dumpRollups(t, cst.DB()); got != want {
		t.Fatalf("totals after the clean-up:\n%s\nnever recorded:\n%s", got, want)
	}
	// The junk-only hour has no totals left.
	st.DB().QueryRow(`SELECT COUNT(*) FROM request_hours WHERE bucket = ?`, hourOf(rounds[3].at.UnixMilli())).Scan(&junkHourTotals)
	if junkHourTotals != 0 {
		t.Fatalf("the junk-only hour keeps %d totals", junkHourTotals)
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
	if fmt.Sprint(paths) != "[/upstream/alpha/completion /upstream/alpha/embeddings /upstream/alpha/v1/chat/completions /v1/audio/translations /v1/chat/completions /v1/completions]" {
		t.Fatalf("%v", paths)
	}
	line := ""
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "usage migration 4") {
			line = l
		}
	}
	want := fmt.Sprintf("rows_before=%d rows_deleted=%d rows_after=%d hours_rebuilt=3 days_rebuilt=2", before, before-after, after)
	if !strings.Contains(line, "level=INFO") || !strings.Contains(line, want) {
		t.Fatalf("log line %q, want %q", line, want)
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
		{"POST", "/v1/audio/translations", true}, {"POST", "/v1/images/generations", true}, {"GET", "/v1/images/generations", false},
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
