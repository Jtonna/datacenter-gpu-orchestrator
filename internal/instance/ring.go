package instance

import (
	"sync"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// ring keeps the last n lines added. Safe for concurrent use.
type ring struct {
	mu    sync.Mutex
	lines []string
	start int // index of the oldest line once full
	n     int
}

func newRing(n int) *ring {
	if n < 1 {
		n = 1
	}
	return &ring{n: n}
}

// Add appends a line, dropping the oldest when full.
func (r *ring) Add(s string) {
	r.mu.Lock()
	defer supervise.Unlocked("instance", r.mu.Unlock)
	if len(r.lines) < r.n {
		r.lines = append(r.lines, s)
		return
	}
	r.lines[r.start] = s
	r.start = (r.start + 1) % r.n
}

// Lines returns all kept lines, oldest first.
func (r *ring) Lines() []string {
	r.mu.Lock()
	defer supervise.Unlocked("instance", r.mu.Unlock)
	out := make([]string, 0, len(r.lines))
	out = append(out, r.lines[r.start:]...)
	return append(out, r.lines[:r.start]...)
}

// Last returns at most k of the newest lines, oldest first.
func (r *ring) Last(k int) []string {
	all := r.Lines()
	if k < 0 {
		k = 0
	}
	if k < len(all) {
		all = all[len(all)-k:]
	}
	return all
}
