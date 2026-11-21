// Package logship is the agent's log shipper (CR-10, design section 5,
// agent contract 11.3): a ring in RAM, one background sender, and the
// producers that feed the ring (the agent's own lines, launch output, the
// machine's journal).
//
// The one rule above all: shipping never blocks the agent. A producer puts a
// line into the ring and returns; it never waits for room, for the sender or
// for Potluck. The ring's lock is held only for memory work (append, drop,
// take a batch out); nothing holds it during I/O. The sender is one
// goroutine that does all the network work.
package logship

import (
	"sync"
	"sync/atomic"
	"time"
)

// DefaultBufferBytes is the ring's size when nothing else is set
// (--log-buffer-mib 8).
const DefaultBufferBytes = 8 << 20

// lineOverhead is what one line costs in the ring beyond its strings: the
// Line struct and its slot in the queue. It is counted so that many short
// lines cannot grow the ring past its bound.
const lineOverhead = 128

// Key names one stream of batches. A request to Potluck carries lines of one
// source, and for launch output of one launch.
type Key struct {
	Source   string // agent, launch or journal
	LaunchID string // only for launch
}

// Line is one log line in the ring.
type Line struct {
	// Mono is the agent's clock (CLOCK_BOOTTIME) when the line was produced.
	Mono time.Duration
	// At is Potluck's time for the line, Unix nanoseconds; 0 until a time
	// anchor is known. Never the node's wall clock.
	At         int64
	Msg        string
	Level      string
	Unit       string
	Stream     string
	Truncated  bool
	TimeSource string // "arrival" when the line's own time could not be used

	seq uint64
}

func (l *Line) size() int {
	return lineOverhead + len(l.Msg) + len(l.Level) + len(l.Unit) + len(l.Stream) + len(l.TimeSource)
}

type queue struct {
	lines   []Line // lines[head:] are kept, oldest first
	head    int
	bytes   int
	dropped uint64 // lines of this key dropped since its last batch
}

func (q *queue) len() int { return len(q.lines) - q.head }

func (q *queue) popFront() Line {
	l := q.lines[q.head]
	q.lines[q.head] = Line{} // let the strings go
	q.head++
	q.bytes -= l.size()
	if q.head > 64 && q.head*2 > len(q.lines) {
		n := copy(q.lines, q.lines[q.head:])
		for i := n; i < len(q.lines); i++ {
			q.lines[i] = Line{}
		}
		q.lines, q.head = q.lines[:n], 0
	}
	return l
}

// Ring holds lines in RAM, at most max bytes (counted with lineOverhead per
// line). When a line does not fit, the oldest lines of all keys go first.
// The ring never touches a disk.
type Ring struct {
	max int

	mu     sync.Mutex
	bytes  int
	seq    uint64
	queues map[Key]*queue

	dropped atomic.Uint64 // every line lost, since the start
}

// NewRing returns a ring of max bytes (DefaultBufferBytes when max <= 0).
func NewRing(max int) *Ring {
	if max <= 0 {
		max = DefaultBufferBytes
	}
	return &Ring{max: max, queues: map[Key]*queue{}}
}

// Max is the ring's bound in bytes.
func (r *Ring) Max() int { return r.max }

// Add puts a line into the ring. It never waits for room: older lines are
// dropped to make it. A line larger than the whole ring is dropped.
func (r *Ring) Add(k Key, l Line) {
	n := l.size()
	r.mu.Lock()
	defer r.mu.Unlock()
	q := r.queues[k]
	if q == nil {
		q = &queue{}
		r.queues[k] = q
	}
	if n > r.max {
		q.dropped++
		r.dropped.Add(1)
		return
	}
	for r.bytes+n > r.max && r.dropOldestLocked() {
	}
	r.seq++
	l.seq = r.seq
	q.lines = append(q.lines, l)
	q.bytes += n
	r.bytes += n
}

// dropOldestLocked drops the oldest line of all keys and counts it against
// its key.
func (r *Ring) dropOldestLocked() bool {
	var oldest *queue
	for _, q := range r.queues {
		if q.len() > 0 && (oldest == nil || q.lines[q.head].seq < oldest.lines[oldest.head].seq) {
			oldest = q
		}
	}
	if oldest == nil {
		return false
	}
	l := oldest.popFront()
	r.bytes -= l.size()
	oldest.dropped++
	r.dropped.Add(1)
	return true
}

// Dropped counts n lines of key k that were lost before they reached the
// ring (over the byte rate, too long to read, refused by Potluck). The next
// batch of k says so.
func (r *Ring) Dropped(k Key, n uint64) {
	if n == 0 {
		return
	}
	r.mu.Lock()
	q := r.queues[k]
	if q == nil {
		q = &queue{}
		r.queues[k] = q
	}
	q.dropped += n
	r.mu.Unlock()
	r.dropped.Add(n)
}

// DroppedTotal is every line lost since the start.
func (r *Ring) DroppedTotal() uint64 { return r.dropped.Load() }

// Bytes is what the ring holds now, in the bytes it counts.
func (r *Ring) Bytes() int { r.mu.Lock(); defer r.mu.Unlock(); return r.bytes }

// Len is the number of lines held.
func (r *Ring) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, q := range r.queues {
		n += q.len()
	}
	return n
}

// Empty reports whether there is nothing to send: no line and no drop to
// report.
func (r *Ring) Empty() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, q := range r.queues {
		if q.len() > 0 || q.dropped > 0 {
			return false
		}
	}
	return true
}

// Clear drops everything without counting it (shipping was switched off).
func (r *Ring) Clear() {
	r.mu.Lock()
	r.queues = map[Key]*queue{}
	r.bytes = 0
	r.mu.Unlock()
}

// Take takes the next batch out of the ring: lines of one key, the key whose
// oldest line is the oldest of all (or a key with only drops to report),
// while their estimated encoded size stays within budget. dropped is how
// many lines of that key were lost since its last batch; the caller puts a
// line saying so first. ok is false when there is nothing to send.
func (r *Ring) Take(budget int, estimate func(*Line) int) (k Key, lines []Line, dropped uint64, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var best *queue
	for key, q := range r.queues {
		switch {
		case q.len() == 0 && q.dropped == 0:
			delete(r.queues, key) // an ended launch's queue goes
			continue
		case best == nil:
		case q.len() == 0: // drops only: after keys with lines
			continue
		case best.len() == 0 || q.lines[q.head].seq < best.lines[best.head].seq:
		default:
			continue
		}
		best, k = q, key
	}
	if best == nil {
		return Key{}, nil, 0, false
	}
	dropped, best.dropped = best.dropped, 0
	used := 0
	for best.len() > 0 {
		e := estimate(&best.lines[best.head])
		if len(lines) > 0 && used+e > budget {
			break
		}
		used += e
		l := best.popFront()
		r.bytes -= l.size()
		lines = append(lines, l)
	}
	return k, lines, dropped, true
}
