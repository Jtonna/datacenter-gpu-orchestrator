package usage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ErrNotFound: no request with that id.
var ErrNotFound = errors.New("no such request")

func scanRows(rows *sql.Rows) ([]Request, error) {
	defer rows.Close()
	out := []Request{}
	for rows.Next() {
		var r Request
		var reason, message, path, requested, modelFile, session, host, source, bench sql.NullString
		var started, first, ended sql.NullInt64
		var tin, treused, tout sql.NullInt64
		var ptps, dtps, pms, dms sql.NullFloat64
		var stream int
		var schedID, instID int64
		var nodes string
		if err := rows.Scan(&r.ID, &r.Status, &reason, &message, &r.HTTPStatus, &r.Kind, &path, &stream, &r.QueuedAt, &started, &first, &ended,
			&r.Profile, &requested, &modelFile, &tin, &treused, &tout, &ptps, &dtps, &pms, &dms,
			&schedID, &instID, &session, &host, &nodes, &r.Caller, &r.CallerKind, &source, &bench); err != nil {
			return nil, err
		}
		r.Reason, r.Message, r.Path, r.Requested, r.ModelFile = reason.String, message.String, path.String, requested.String, modelFile.String
		r.Session, r.HostNode, r.SourceAddr, r.BenchRun = session.String, host.String, source.String, bench.String
		r.Stream = stream != 0
		r.StartedAt, r.FirstTokenAt, r.EndedAt = started.Int64, first.Int64, ended.Int64
		r.SchedID, r.Instance = uint64(schedID), uint64(instID)
		if tin.Valid {
			r.TokensIn = intOrNil(int(tin.Int64))
		}
		if treused.Valid {
			r.TokensReused = intOrNil(int(treused.Int64))
		}
		if tout.Valid {
			r.TokensOut = intOrNil(int(tout.Int64))
		}
		if ptps.Valid {
			r.PrefillTPS = floatOrNil(ptps.Float64)
		}
		if dtps.Valid {
			r.DecodeTPS = floatOrNil(dtps.Float64)
		}
		r.prefillMS, r.decodeMS = -1, -1
		if pms.Valid {
			r.prefillMS = pms.Float64
		}
		if dms.Valid {
			r.decodeMS = dms.Float64
		}
		if err := json.Unmarshal([]byte(nodes), &r.Nodes); err != nil || r.Nodes == nil {
			r.Nodes = []Node{}
		}
		r.WaitMS, r.DurationMS = spans(&r, r.EndedAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Filter selects requests. Times are unix ms on queued_at, From inclusive,
// To exclusive; zero means open.
type Filter struct {
	Status  []string // any of these statuses (queued, running, done, failed, refused, cancelled)
	Profile string
	Node    string // a machine id or name among the request's machines
	From    int64
	To      int64
	Bench   *bool // true: only benchmark requests; false: none
	Caller  string
	Limit   int   // default 50, at most 500
	Before  int64 // paging: only ids below this (the cursor)
}

// Page is a page of requests, newest first: the live ones (queued, then
// running) on the first page, then the finished ones.
type Page struct {
	Requests []Request `json:"requests"`
	// Next is the cursor of the next page (before=Next); 0 at the end.
	Next int64 `json:"next,omitempty"`
}

func (f Filter) wants(status string) bool {
	return len(f.Status) == 0 || slices.Contains(f.Status, status)
}

func (f Filter) matchesLive(r *Request) bool {
	if !f.wants(r.Status) || f.Profile != "" && r.Profile != f.Profile || f.Caller != "" && r.Caller != f.Caller {
		return false
	}
	if f.From > 0 && r.QueuedAt < f.From || f.To > 0 && r.QueuedAt >= f.To {
		return false
	}
	if f.Bench != nil && *f.Bench != (r.BenchRun != "") {
		return false
	}
	if f.Node != "" && !slices.ContainsFunc(r.Nodes, func(n Node) bool { return n.ID == f.Node || n.Name == f.Node }) {
		return false
	}
	return true
}

// List returns a page of requests: on the first page (no Before) the live
// requests that match, queued first, then running, then the stored
// finished ones, newest first.
func (r *Recorder) List(f Filter) (Page, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	f.Limit = min(f.Limit, 500)
	page := Page{Requests: []Request{}}
	if f.Before == 0 {
		var queued, running []Request
		for _, row := range r.Live() {
			if !f.matchesLive(&row) {
				continue
			}
			if row.Status == StatusQueued {
				queued = append(queued, row)
			} else {
				running = append(running, row)
			}
		}
		slices.Reverse(queued)
		slices.Reverse(running)
		page.Requests = append(append(page.Requests, queued...), running...)
	}
	var final []string
	for _, s := range []string{StatusDone, StatusFailed, StatusRefused, StatusCancelled} {
		if f.wants(s) {
			final = append(final, s)
		}
	}
	db := r.Store().DB()
	if db == nil || len(final) == 0 {
		return page, nil
	}
	where := []string{`status IN (` + strings.TrimSuffix(strings.Repeat("?,", len(final)), ",") + `)`}
	var args []any
	for _, s := range final {
		args = append(args, s)
	}
	add := func(cond string, a ...any) {
		where = append(where, cond)
		args = append(args, a...)
	}
	if f.Profile != "" {
		add(`profile = ?`, f.Profile)
	}
	if f.Caller != "" {
		add(`caller = ?`, f.Caller)
	}
	if f.From > 0 {
		add(`queued_at >= ?`, f.From)
	}
	if f.To > 0 {
		add(`queued_at < ?`, f.To)
	}
	if f.Before > 0 {
		add(`id < ?`, f.Before)
	}
	if f.Bench != nil {
		if *f.Bench {
			where = append(where, `bench_run IS NOT NULL`)
		} else {
			where = append(where, `bench_run IS NULL`)
		}
	}
	if f.Node != "" {
		add(`EXISTS (SELECT 1 FROM json_each(client_requests.nodes) WHERE json_extract(value,'$.id') = ? OR json_extract(value,'$.name') = ?)`, f.Node, f.Node)
	}
	args = append(args, f.Limit+1)
	rows, err := db.Query(`SELECT `+rowColumns+` FROM client_requests WHERE `+strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return page, err
	}
	stored, err := scanRows(rows)
	if err != nil {
		return page, err
	}
	if len(stored) > f.Limit {
		stored = stored[:f.Limit]
		page.Next = stored[len(stored)-1].ID
	}
	page.Requests = append(page.Requests, stored...)
	return page, nil
}

// Get returns one request, live or stored.
func (r *Recorder) Get(id int64) (Request, error) {
	if row, ok := r.LiveByID(id); ok {
		return row, nil
	}
	db := r.Store().DB()
	if db == nil {
		return Request{}, ErrNotFound
	}
	rows, err := db.Query(`SELECT `+rowColumns+` FROM client_requests WHERE id = ?`, id)
	if err != nil {
		return Request{}, err
	}
	got, err := scanRows(rows)
	if err != nil {
		return Request{}, err
	}
	if len(got) == 0 {
		return Request{}, ErrNotFound
	}
	return got[0], nil
}

// Steps of a period query.
const (
	StepHour = "hour"
	StepDay  = "day"
)

// Totals are the sums over a period or one bucket of it. Rates are nil when
// no request in it reported the figures.
type Totals struct {
	Requests     int64    `json:"requests"`
	Done         int64    `json:"done"`
	Failed       int64    `json:"failed"`
	Refused      int64    `json:"refused"`
	Cancelled    int64    `json:"cancelled"`
	TokensIn     int64    `json:"tokens_in"`
	TokensReused int64    `json:"tokens_reused"`
	TokensOut    int64    `json:"tokens_out"`
	WaitMS       int64    `json:"wait_ms"`
	DurationMS   int64    `json:"duration_ms"`
	PrefillTPS   *float64 `json:"prefill_tps"`
	DecodeTPS    *float64 `json:"decode_tps"`

	prefillTok, decodeTok int64
	prefillMS, decodeMS   float64
}

func (t *Totals) add(o Totals) {
	t.Requests += o.Requests
	t.Done += o.Done
	t.Failed += o.Failed
	t.Refused += o.Refused
	t.Cancelled += o.Cancelled
	t.TokensIn += o.TokensIn
	t.TokensReused += o.TokensReused
	t.TokensOut += o.TokensOut
	t.WaitMS += o.WaitMS
	t.DurationMS += o.DurationMS
	t.prefillTok += o.prefillTok
	t.decodeTok += o.decodeTok
	t.prefillMS += o.prefillMS
	t.decodeMS += o.decodeMS
}

func (t *Totals) rates() {
	if t.prefillMS > 0 {
		v := float64(t.prefillTok) / t.prefillMS * 1000
		t.PrefillTPS = &v
	}
	if t.decodeMS > 0 {
		v := float64(t.decodeTok) / t.decodeMS * 1000
		t.DecodeTPS = &v
	}
}

// Bucket is one step of a series: [Start, Start+step).
type Bucket struct {
	Start int64 `json:"t"`
	Totals
}

// Summary answers a period query.
type Summary struct {
	From int64  `json:"from"`
	To   int64  `json:"to"`
	Step string `json:"step"`
	// Source is the roll-up the figures come from: hourly, or daily (UTC
	// days) for periods older than the hourly retention.
	Source string   `json:"source"`
	Totals Totals   `json:"totals"`
	Series []Bucket `json:"series"`
	// Live counts the requests queued and running now (not in the totals).
	Live struct {
		Queued  int `json:"queued"`
		Running int `json:"running"`
	} `json:"live"`
	// OldestTS is the arrival of the oldest request on record (unix ms; 0
	// when there is none): the Overview disables "previous" before it.
	OldestTS int64 `json:"oldest_ts"`
	// HistoryBeginsAt is when the usage file began: before it, nothing was
	// recorded (unix ms; null when the store is off).
	HistoryBeginsAt *int64 `json:"history_begins_at"`
	On              bool   `json:"on"`
}

// MaxBuckets bounds a series.
const MaxBuckets = 2000

// Summarize returns the totals and the series of [from, to) in steps of an
// hour or a day, for one profile or all (""). Buckets start at from and are
// step long, so a caller's local midnight works as from with step=day; while
// the hourly roll-ups cover the period they are used, else the daily ones
// (UTC days).
func (r *Recorder) Summarize(from, to int64, step, profile string) (Summary, error) {
	if step == "" {
		step = StepHour
	}
	if step != StepHour && step != StepDay {
		return Summary{}, fmt.Errorf("step must be hour or day, not %q", step)
	}
	if to <= from {
		return Summary{}, errors.New("to must be after from")
	}
	width := int64(time.Hour / time.Millisecond)
	if step == StepDay {
		width *= 24
	}
	if (to-from)/width > MaxBuckets {
		return Summary{}, fmt.Errorf("at most %d buckets: use a longer step or a shorter period", MaxBuckets)
	}
	sum := Summary{From: from, To: to, Step: step, Source: "hourly", Series: []Bucket{}}
	for _, l := range r.Live() {
		if profile != "" && l.Profile != profile {
			continue
		}
		if l.Status == StatusQueued {
			sum.Live.Queued++
		} else {
			sum.Live.Running++
		}
	}
	for b := from; b < to; b += width {
		sum.Series = append(sum.Series, Bucket{Start: b})
	}
	st := r.Store()
	db := st.DB()
	if db == nil {
		return sum, nil
	}
	sum.On = true
	begins := st.HistoryBeginsAt().UnixMilli()
	sum.HistoryBeginsAt = &begins
	table := "request_hours"
	if step == StepDay && from < hourOf(st.Now().Add(-st.opt.Retention.KeepHourly).UnixMilli())+int64(time.Hour/time.Millisecond) {
		table, sum.Source = "request_days", "daily"
	}
	q := `SELECT bucket, SUM(requests), SUM(done), SUM(failed), SUM(refused), SUM(cancelled), SUM(tokens_in), SUM(tokens_reused), SUM(tokens_out),
  SUM(wait_ms), SUM(duration_ms), SUM(prefill_tokens), SUM(prefill_ms), SUM(decode_tokens), SUM(decode_ms)
  FROM ` + table + ` WHERE bucket >= ? AND bucket < ?`
	args := []any{from, to}
	if table == "request_days" { // a UTC day counts where it starts
		args = []any{dayOf(from), to}
	}
	if profile != "" {
		q += ` AND profile = ?`
		args = append(args, profile)
	}
	rows, err := db.Query(q+` GROUP BY bucket`, args...)
	if err != nil {
		return sum, err
	}
	defer rows.Close()
	for rows.Next() {
		var b int64
		var t Totals
		if err := rows.Scan(&b, &t.Requests, &t.Done, &t.Failed, &t.Refused, &t.Cancelled, &t.TokensIn, &t.TokensReused, &t.TokensOut,
			&t.WaitMS, &t.DurationMS, &t.prefillTok, &t.prefillMS, &t.decodeTok, &t.decodeMS); err != nil {
			return sum, err
		}
		i := int((max(b, from) - from) / width)
		if i < 0 || i >= len(sum.Series) {
			continue
		}
		sum.Series[i].add(t)
		sum.Totals.add(t)
	}
	if err := rows.Err(); err != nil {
		return sum, err
	}
	for i := range sum.Series {
		sum.Series[i].rates()
	}
	sum.Totals.rates()
	sum.OldestTS = oldestTS(db)
	return sum, nil
}

// oldestTS is the oldest request's arrival; once its row is trimmed, the
// start of the oldest daily roll-up.
func oldestTS(db *sql.DB) int64 {
	var row, day sql.NullInt64
	db.QueryRow(`SELECT MIN(queued_at) FROM client_requests`).Scan(&row)
	db.QueryRow(`SELECT MIN(bucket) FROM request_days`).Scan(&day)
	if day.Valid && (!row.Valid || day.Int64 < dayOf(row.Int64)) {
		return day.Int64
	}
	return row.Int64
}
