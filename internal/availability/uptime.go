package availability

import (
	"context"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/usage"
)

// Uptime over a window of days (the Zones page's "Uptime, 7 days"), per
// machine, from the availability hours in usage.db (node_hours) and the
// sampler's unsent minute:
//
//	up      = free + in use + loading
//	counted = up + lost + set aside      (known time less switched-off time)
//	uptime  = up / counted               (null when counted is 0)
//
// Switched-off time (paused, excluded, removed) is left out of both: the
// owner chose it, so it neither raises nor lowers the figure; it is given
// as off_s. Set-aside time counts as down, as the health row counts a
// set-aside zone down; it is given as set_aside_s, apart from lost_s.
// Unknown time (Potluck not running, before the history begins) is in
// neither: the figure is over what was seen.
//
// The window is the last Days*24 hours, starting at the start of the UTC
// hour that holds now-Days*24h (whole stored hours), up to now. When the
// history begins later than that, Complete is false and CoveredFrom says
// from when the figure is.

// Totals are seconds per state over a window, and the uptime of them.
type Totals struct {
	FreeS     float64  `json:"free_s"`
	InUseS    float64  `json:"in_use_s"`
	LoadingS  float64  `json:"loading_s"`
	LostS     float64  `json:"lost_s"`
	SetAsideS float64  `json:"set_aside_s"`
	OffS      float64  `json:"off_s"`
	KnownS    float64  `json:"known_s"`   // all six
	UpS       float64  `json:"up_s"`      // free + in use + loading
	CountedS  float64  `json:"counted_s"` // known less off
	Uptime    *float64 `json:"uptime"`    // up over counted, 0 to 1, 3 decimals
}

func (t *Totals) addHour(h usage.NodeHour) {
	s := func(ms int64) float64 { return float64(ms) / 1000 }
	t.FreeS += s(h.FreeMs)
	t.InUseS += s(h.InUseMs)
	t.LoadingS += s(h.LoadingMs)
	t.LostS += s(h.LostMs)
	t.SetAsideS += s(h.SetAsideMs)
	t.OffS += s(h.OffMs)
}

// Add adds another machine's seconds (a group's uptime is weighted by
// time, not an average of its members' figures). Call Finish after.
func (t *Totals) Add(o Totals) {
	t.FreeS += o.FreeS
	t.InUseS += o.InUseS
	t.LoadingS += o.LoadingS
	t.LostS += o.LostS
	t.SetAsideS += o.SetAsideS
	t.OffS += o.OffS
}

// Finish works out the sums and the uptime, rounded as the series rounds.
func (t *Totals) Finish() {
	t.UpS = t.FreeS + t.InUseS + t.LoadingS
	t.CountedS = t.UpS + t.LostS + t.SetAsideS
	t.KnownS = t.CountedS + t.OffS
	t.Uptime = nil
	if t.CountedS > 0 {
		u := round3(min(1, t.UpS/t.CountedS))
		t.Uptime = &u
	}
	for _, p := range []*float64{&t.FreeS, &t.InUseS, &t.LoadingS, &t.LostS, &t.SetAsideS, &t.OffS, &t.KnownS, &t.UpS, &t.CountedS} {
		*p = round3(*p)
	}
}

// Window is the span an uptime figure covers.
type Window struct {
	Days int   `json:"days"`
	From Stamp `json:"from"` // the window's start
	To   Stamp `json:"to"`   // now
	// HistoryBeginsAt is when usage.db's history begins (null when the
	// store is off); CoveredFrom the later of it and From.
	HistoryBeginsAt *Stamp `json:"history_begins_at"`
	CoveredFrom     *Stamp `json:"covered_from"`
	// Complete: the history covers the whole window.
	Complete bool `json:"complete"`
	// Recording: the store is on (otherwise every figure is null).
	Recording bool `json:"recording"`
}

// MachineTotals is one machine's totals over the window.
type MachineTotals struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Totals
}

// Uptimes reads every machine's totals over the last days days. Totals
// are not finished (call Finish, or Add them into a group's first).
func Uptimes(ctx context.Context, store *usage.Store, smp *Sampler, days int, now time.Time) (Window, map[string]*MachineTotals, error) {
	now = now.UTC()
	from := now.Add(-time.Duration(days) * 24 * time.Hour).Truncate(time.Hour)
	w := Window{Days: days, From: Stamp(from), To: Stamp(now), Recording: store.On()}
	out := map[string]*MachineTotals{}
	if !store.On() {
		return w, out, nil
	}
	begins := store.HistoryBeginsAt().UTC()
	b, cov := Stamp(begins), Stamp(from)
	if begins.After(from) {
		cov = b
	}
	w.HistoryBeginsAt, w.CoveredFrom, w.Complete = &b, &cov, !begins.After(from)
	rows, err := store.NodeHours(ctx, from, now.Add(time.Nanosecond))
	if err != nil {
		return w, out, err
	}
	for _, p := range smp.Pending() {
		if !p.Hour.Before(from) && !p.Hour.After(now) {
			rows = append(rows, p)
		}
	}
	for _, r := range rows {
		m := out[r.Node]
		if m == nil {
			m = &MachineTotals{ID: r.Node, Name: r.Node}
			out[r.Node] = m
		}
		if r.Name != "" {
			m.Name = r.Name
		}
		m.addHour(r)
	}
	return w, out, nil
}
