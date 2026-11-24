package api

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// The node feed: GET /api/nodes, for a reader outside Potluck (a dashboard
// backend) that polls every few seconds and should not read the whole
// /api/status for it.
//
//	{"time": "...", "seq": N, "nodes": [...]}
//
// Each entry of nodes is exactly an entry of agents in /api/status (both are
// built by agentFacts), plus the scheduler's own machine as an entry with
// "local": true and only the fields that apply to it (pool.LocalFacts).
//
// seq rises whenever anything in the answer changed other than the ages
// (keys ending in _age_s, _ago_s or age_ms, and the uptimes uptime_s and
// host_uptime_s) and the time, so a reader can skip an answer whose seq it
// has seen. Compare it for equality only. It starts at Potluck's start time
// in Unix milliseconds, so a restart of Potluck does not repeat an earlier
// number (as long as Potluck's clock goes forward). No CORS headers: the
// reader is a backend, not a browser on another origin.

// feedSeq is the node feed's change counter.
type feedSeq struct {
	mu   sync.Mutex
	seq  int64
	last [32]byte
}

// next returns the seq for an answer whose age-free form hashes to h.
func (f *feedSeq) next(h [32]byte, start int64) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seq == 0 {
		f.seq, f.last = start, h
		return f.seq
	}
	if h != f.last {
		f.seq++
		f.last = h
	}
	return f.seq
}

// agentFacts is the agents list of /api/status and of the node feed: the
// pool's facts with what the log route counted per node. One function, so
// the two cannot drift apart.
func (s *Server) agentFacts(st *scheduler.Status) []pool.NodeFacts {
	facts := s.opts.Pool.Facts()
	s.logFacts(facts)           // CR-10: what the log route counted per node
	reservationFacts(facts, st) // reserve.go: what is reserved on each node
	s.zoneFacts(facts, st)      // zonefacts.go: role, share, set-aside, who switched it off, free at
	_, states, _ := zoneCountsOf(st, facts)
	setZoneStates(st, facts, states) // counts.go: the state each zone is counted in, and its attention rank
	return facts
}

func (s *Server) handleNodeFeed(w http.ResponseWriter, r *http.Request) {
	if !s.agentsOn(w) {
		return
	}
	now := s.opts.Now()
	order := r.URL.Query().Get("sort")
	if order != "" && order != "attention" && order != "name" {
		apiErr(w, http.StatusBadRequest, "sort is attention or name")
		return
	}
	nodes := []any{}
	var zs []rankedZone
	var stp *scheduler.Status
	if st, err := s.sched.Status(r.Context()); err == nil {
		stp = &st
	}
	facts := s.agentFacts(stp)
	for _, f := range facts {
		nodes = append(nodes, f)
		zs = append(zs, rankedZone{rank: f.AttentionRank, name: f.Name, id: f.ID, v: f})
	}
	counts, states, _ := zoneCountsOf(stp, facts) // counts.go: the same counts as /api/health's Zones row
	if l, ok := s.opts.Pool.Local(); ok {
		if l.Excluded && stp != nil {
			if on := instancesOnNodes(stp)[l.ID]; len(on) > 0 {
				l.RunningWhileExcluded = &on[0]
			}
		}
		l.ZoneFacts = s.zoneFactsOf(l.ID, stp, false, l.ExcludedBy) // zonefacts.go
		l.ZoneState = states[l.ID]
		l.AttentionRank = attentionRank(l.ZoneState, l.Unplaced, l.Excluded, hungNodes(stp)[l.ID])
		nodes = append(nodes, l)
		zs = append(zs, rankedZone{rank: l.AttentionRank, name: l.Name, id: l.ID, v: l})
	}
	b, err := json.Marshal(nodes)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	var plain any
	json.Unmarshal(b, &plain)
	stripAges(plain)
	canon, _ := json.Marshal(plain) // map keys are sorted: a stable form
	seq := s.feed.next(sha256.Sum256(canon), s.start.UnixMilli()) // over the unsorted list: the order asked for is not a change
	if order != "" {
		if order == "attention" {
			sortByAttention(zs)
		} else {
			sortByName(zs)
		}
		sorted := make([]any, len(zs))
		for i, z := range zs {
			sorted[i] = z.v
		}
		b, _ = json.Marshal(sorted)
	}
	writeJSON(w, http.StatusOK, map[string]any{"time": isoTime(now), "seq": seq, "nodes": json.RawMessage(b), "counts": counts})
}

// ageKey: a figure that changes with the passing of time alone.
func ageKey(k string) bool {
	return strings.HasSuffix(k, "_age_s") || strings.HasSuffix(k, "_ago_s") || k == "age_ms" || strings.HasSuffix(k, "_age_ms") ||
		k == "uptime_s" || k == "host_uptime_s" || k == "free_in_s"
}

// stripAges removes the age figures from a decoded JSON value, in place.
func stripAges(v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if ageKey(k) {
				delete(x, k)
				continue
			}
			stripAges(e)
		}
	case []any:
		for _, e := range x {
			stripAges(e)
		}
	}
}
