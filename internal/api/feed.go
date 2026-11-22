package api

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
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
func (s *Server) agentFacts() []pool.NodeFacts {
	facts := s.opts.Pool.Facts()
	s.logFacts(facts) // CR-10: what the log route counted per node
	return facts
}

func (s *Server) handleNodeFeed(w http.ResponseWriter, r *http.Request) {
	if !s.agentsOn(w) {
		return
	}
	now := s.opts.Now()
	nodes := []any{}
	for _, f := range s.agentFacts() {
		nodes = append(nodes, f)
	}
	if l, ok := s.opts.Pool.Local(); ok {
		nodes = append(nodes, l)
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
	seq := s.feed.next(sha256.Sum256(canon), s.start.UnixMilli())
	writeJSON(w, http.StatusOK, map[string]any{"time": isoTime(now), "seq": seq, "nodes": json.RawMessage(b)})
}

// ageKey: a figure that changes with the passing of time alone.
func ageKey(k string) bool {
	return strings.HasSuffix(k, "_age_s") || strings.HasSuffix(k, "_ago_s") || k == "age_ms" || strings.HasSuffix(k, "_age_ms") ||
		k == "uptime_s" || k == "host_uptime_s"
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
