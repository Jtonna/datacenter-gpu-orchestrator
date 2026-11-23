package availability

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/usage"
)

// Steps of a series.
const (
	StepHour = "hour"
	StepDay  = "day"
)

// DownMarks is the down time that makes a cell down (see CellState).
const DownMarks = 5 * time.Minute

// ErrUnknownNode: the node asked for is neither stored nor known now.
var ErrUnknownNode = errors.New("no such machine in the availability history")

// Query is one read of the series.
type Query struct {
	From, To time.Time // [From, To); cells cover whole steps, never past now
	Step     string    // hour (default) or day (UTC days, rolled up at read time)
	Node     string    // a machine id or name; empty for every machine
}

// Stamp is a time written as the API writes times (UTC, milliseconds).
type Stamp time.Time

// MarshalJSON writes the time, or null for the zero time.
func (s Stamp) MarshalJSON() ([]byte, error) {
	t := time.Time(s)
	if t.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(t.UTC().Format("2006-01-02T15:04:05.000Z07:00"))
}

// UnmarshalJSON reads what MarshalJSON writes (clients in Go, tests).
func (s *Stamp) UnmarshalJSON(b []byte) error {
	var v *string
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	if v == nil {
		*s = Stamp{}
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, *v)
	*s = Stamp(t)
	return err
}

// Seconds is a cell's time in each state, in seconds. They add up to the
// cell's span (up to now for the current cell).
type Seconds struct {
	Free    float64 `json:"free"`
	InUse   float64 `json:"in_use"`
	Loading float64 `json:"loading"`
	Down    float64 `json:"down"`
	Off     float64 `json:"off"`
	Unknown float64 `json:"unknown"`
}

// Cell is one hour (or day) of one machine.
type Cell struct {
	Start Stamp   `json:"start"`
	SpanS float64 `json:"span_s"` // the cell's length, up to now for the current cell
	// Partial: the current cell, counted up to now.
	Partial bool    `json:"partial,omitempty"`
	Seconds Seconds `json:"seconds"`
	// Down split: lost (offline or failed membership) and set aside.
	DownLostS     float64 `json:"down_lost_s"`
	DownSetAsideS float64 `json:"down_set_aside_s"`
	// Busy is the mean of in flight over parallel across the in-use time
	// (0 to 1); BusyLevel is its quarter 1 to 4 ("darker is busier"), 0
	// when the cell has no in-use time.
	Busy      *float64 `json:"busy"`
	BusyLevel int      `json:"busy_level"`
	// ReachableS and Uptime (reachable over known time; null when nothing is
	// known).
	ReachableS float64  `json:"reachable_s"`
	Uptime     *float64 `json:"uptime"`
	// State is the cell's colour by CellState.
	State State `json:"state"`
}

// NodeSeries is one machine's cells, oldest first, with its uptime over the
// whole range.
type NodeSeries struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	KnownS     float64  `json:"known_s"`
	ReachableS float64  `json:"reachable_s"`
	Uptime     *float64 `json:"uptime"`
	Cells      []Cell   `json:"cells"`
}

// Series is the answer of the availability route.
type Series struct {
	From Stamp  `json:"from"`
	To   Stamp  `json:"to"`
	Step string `json:"step"`
	// HistoryBeginsAt is when usage.db's history begins (null when the
	// store is off); cells before it read unknown.
	HistoryBeginsAt Stamp `json:"history_begins_at"`
	// Recording: the store is on and the sampler runs.
	Recording bool         `json:"recording"`
	Nodes     []NodeSeries `json:"nodes"`
}

// CellState is a cell's colour: unknown when nothing is known; off when
// switched off at least half the known time; down when down at least
// DownMarks or half the known time; loading when loading at least half the
// known time; in use when in use at least a quarter of the known time; else
// free.
func CellState(s Seconds) State {
	known := s.Free + s.InUse + s.Loading + s.Down + s.Off
	switch {
	case known <= 0:
		return Unknown
	case s.Off >= known/2:
		return Off
	case s.Down >= DownMarks.Seconds() || s.Down >= known/2:
		return Down
	case s.Loading >= known/2:
		return Loading
	case s.InUse >= known/4:
		return InUse
	}
	return Free
}

// BusyLevel is the quarter (1 to 4) of a busy figure.
func BusyLevel(b float64) int {
	switch {
	case b < .25:
		return 1
	case b < .5:
		return 2
	case b < .75:
		return 3
	}
	return 4
}

// Read answers a query from the stored hours plus what the sampler (nil
// when it does not run) counted and has not written yet. now bounds the
// cells. An off store answers machines the sampler knows with unknown
// cells, and no history beginning.
func Read(ctx context.Context, store *usage.Store, smp *Sampler, q Query, now time.Time) (Series, error) {
	step := time.Hour
	if q.Step == "" {
		q.Step = StepHour
	}
	if q.Step == StepDay {
		step = 24 * time.Hour
	}
	from := q.From.UTC().Truncate(step)
	to := q.To.UTC()
	if to.After(now) {
		to = now.UTC()
	}
	if t := to.Truncate(step); t.Before(to) {
		to = t.Add(step)
	}
	out := Series{From: Stamp(from), To: Stamp(to), Step: q.Step, Recording: store.On() && smp != nil, Nodes: []NodeSeries{}}
	if store.On() {
		out.HistoryBeginsAt = Stamp(store.HistoryBeginsAt())
	}
	if !from.Before(to) {
		return out, nil
	}
	rows, err := store.NodeHours(ctx, from, to)
	if err != nil {
		return out, err
	}
	for _, p := range smp.Pending() {
		if store.On() && !p.Hour.Before(from) && p.Hour.Before(to) { // an off store records nothing: all unknown
			rows = append(rows, p)
		}
	}
	type acc struct {
		name  string
		cells map[int64]*usage.NodeHour
	}
	byNode := map[string]*acc{}
	node := func(id, name string) *acc {
		a := byNode[id]
		if a == nil {
			a = &acc{cells: map[int64]*usage.NodeHour{}}
			byNode[id] = a
		}
		if name != "" {
			a.name = name
		}
		return a
	}
	for _, r := range rows {
		a := node(r.Node, r.Name)
		k := r.Hour.UTC().Truncate(step).UnixMilli()
		c := a.cells[k]
		if c == nil {
			c = &usage.NodeHour{}
			a.cells[k] = c
		}
		c.Add(r)
	}
	for _, k := range smp.Known() {
		node(k.Node, k.Name) // the newest name wins
	}
	if q.Node != "" {
		want := ""
		for id, a := range byNode {
			if id == q.Node || strings.EqualFold(a.name, q.Node) {
				want = id
			}
		}
		if want == "" {
			return out, ErrUnknownNode
		}
		byNode = map[string]*acc{want: byNode[want]}
	}
	for id, a := range byNode {
		ns := NodeSeries{ID: id, Name: a.name, Cells: []Cell{}}
		if ns.Name == "" {
			ns.Name = id
		}
		for t := from; t.Before(to); t = t.Add(step) {
			end := t.Add(step)
			c := Cell{Start: Stamp(t)}
			if end.After(now) {
				end, c.Partial = now, true
			}
			c.SpanS = end.Sub(t).Seconds()
			var h usage.NodeHour
			if x := a.cells[t.UnixMilli()]; x != nil {
				h = *x
			}
			c.fill(h)
			ns.KnownS += c.SpanS - c.Seconds.Unknown
			ns.ReachableS += c.ReachableS
			ns.Cells = append(ns.Cells, c)
		}
		if ns.KnownS > 0 {
			u := min(1, ns.ReachableS/ns.KnownS)
			ns.Uptime = &u
		}
		out.Nodes = append(out.Nodes, ns)
	}
	sort.Slice(out.Nodes, func(i, j int) bool {
		if out.Nodes[i].Name != out.Nodes[j].Name {
			return out.Nodes[i].Name < out.Nodes[j].Name
		}
		return out.Nodes[i].ID < out.Nodes[j].ID
	})
	return out, nil
}

// fill sets the cell's figures from its stored milliseconds.
func (c *Cell) fill(h usage.NodeHour) {
	s := func(ms int64) float64 { return float64(ms) / 1000 }
	c.Seconds = Seconds{Free: s(h.FreeMs), InUse: s(h.InUseMs), Loading: s(h.LoadingMs), Down: s(h.LostMs + h.SetAsideMs), Off: s(h.OffMs)}
	c.DownLostS, c.DownSetAsideS = s(h.LostMs), s(h.SetAsideMs)
	known := s(h.KnownMs())
	c.Seconds.Unknown = max(0, c.SpanS-known)
	c.ReachableS = s(h.ReachableMs)
	if known > 0 {
		u := min(1, c.ReachableS/known)
		c.Uptime = &u
	}
	if h.InUseMs > 0 {
		b := min(1, float64(h.BusyMs)/float64(h.InUseMs))
		c.Busy, c.BusyLevel = &b, BusyLevel(b)
	}
	c.State = CellState(c.Seconds)
}
