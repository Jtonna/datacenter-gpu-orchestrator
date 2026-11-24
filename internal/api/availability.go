package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/availability"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
)

// Per-machine availability (UI slice 1a, S3; internal/availability):
//
//	GET /api/nodes/availability?node=&group=&from=&to=&step=hour|day
//
// node is a machine id or name (empty: every machine); from and to are
// unix milliseconds, RFC 3339, or a span back from now (24h, 7d; to
// defaults to now; from defaults to 24 hour cells, or 7 day cells, the
// last being the current one); step is hour (default) or day (UTC days,
// rolled up at read time). group is accepted and answered
// 501 until zone groups exist. MCP: get_availability.

// maxAvailabilitySpan bounds one read (hours: 24 * 400 cells per machine).
const maxAvailabilitySpan = 400 * 24 * time.Hour

func (s *Server) availabilityRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/nodes/availability", s.handleAvailability)
	m.HandleFunc("GET /api/nodes/uptime", s.handleUptime) // uptime.go
}

// startAvailability starts the sampler when a usage store is recording. An
// off store (or none) starts nothing, so it costs the core loop no query;
// the route then answers an empty series with recording false. It stops
// with the server's background work (Close cancels s.bg and waits), writing
// what it counted last.
func (s *Server) startAvailability() {
	if !s.opts.Usage.On() || s.sched == nil {
		return
	}
	s.avail = availability.New(s.opts.Usage, s.availabilitySource, availability.Options{Now: s.opts.Now, Logger: s.log})
	s.bgWG.Add(1)
	s.avail.Start(s.bg, s.bgWG.Done)
}

// availabilitySource is one sample of every machine: the scheduler's
// status (one read-only query, as any status reader) and the pool's
// membership facts, both already in memory. No machine is called.
func (s *Server) availabilitySource(ctx context.Context) ([]availability.Sample, error) {
	st, err := s.sched.Status(ctx)
	if err != nil {
		return nil, err
	}
	var facts []pool.NodeFacts
	if s.opts.Pool != nil {
		facts = s.opts.Pool.Facts()
	}
	return availability.Classify(&st, facts), nil
}

// availTime reads from or to: unix ms, RFC 3339, or a span back from now.
func availTime(v string, now time.Time) (time.Time, bool) {
	if v == "" {
		return time.Time{}, true
	}
	if ms, ok := timeParam(v); ok {
		return time.UnixMilli(ms), true
	}
	if n, err := strconv.Atoi(strings.TrimSuffix(v, "d")); err == nil && strings.HasSuffix(v, "d") && n > 0 {
		return now.Add(-time.Duration(n) * 24 * time.Hour), true
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return now.Add(-d), true
	}
	return time.Time{}, false
}

func (s *Server) handleAvailability(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var members map[string]bool // group=: only the group's zones
	if name := q.Get("group"); name != "" {
		g, ok := s.groups.Get(name)
		if !ok {
			apiErr(w, http.StatusNotFound, "no zone group %q", name)
			return
		}
		members = map[string]bool{}
		for _, id := range g.Members {
			members[id] = true
		}
	}
	now := s.opts.Now()
	from, ok1 := availTime(q.Get("from"), now)
	to, ok2 := availTime(q.Get("to"), now)
	if !ok1 || !ok2 {
		apiErr(w, http.StatusBadRequest, "from and to are unix milliseconds, RFC 3339 times or a span back from now such as 24h or 7d")
		return
	}
	step := q.Get("step")
	if step != "" && step != availability.StepHour && step != availability.StepDay {
		apiErr(w, http.StatusBadRequest, "step is hour or day")
		return
	}
	if to.IsZero() || to.After(now) {
		to = now
	}
	if from.IsZero() { // 24 hour cells or 7 day cells, the last one now's
		from = to.Add(-23 * time.Hour)
		if step == availability.StepDay {
			from = to.Add(-6 * 24 * time.Hour)
		}
	}
	if !from.Before(to) {
		apiErr(w, http.StatusBadRequest, "from must be before to")
		return
	}
	if to.Sub(from) > maxAvailabilitySpan {
		apiErr(w, http.StatusBadRequest, "at most %d days in one read", int(maxAvailabilitySpan.Hours()/24))
		return
	}
	series, err := availability.Read(r.Context(), s.opts.Usage, s.avail, availability.Query{From: from, To: to, Step: step, Node: q.Get("node")}, now)
	if members != nil {
		kept := series.Nodes[:0]
		for _, n := range series.Nodes {
			if members[n.ID] {
				kept = append(kept, n)
			}
		}
		series.Nodes = kept
	}
	switch {
	case errors.Is(err, availability.ErrUnknownNode):
		apiErr(w, http.StatusNotFound, "no machine %q in the availability history", q.Get("node"))
		return
	case err != nil:
		// A store that cannot be read never fails the page: the series is
		// answered empty with the note.
		s.log.Warn("availability read failed; answering an empty series", "err", err)
		series.Nodes = []availability.NodeSeries{}
	}
	writeJSON(w, http.StatusOK, series)
}
