package api

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// peakCycles is GET /api/needs peak_cycles: how many load-to-unload cycles
// of each shape and node set (as the stored peaks are keyed) have a peak on
// every node in the history, with the lowest, highest and latest peak and
// the dates of the first and last. Collected only: nothing reads it for
// placement. The cycles come from the instanceUnloaded events' cycle_peaks
// (scheduler/peakcycle.go): a tally rebuilt at start from the events the
// history store still holds (its retention), then counted as new events are
// recorded, as occupied_only is, so the polled /api/needs never reads the
// history files. A cycle that ends after the start counts until the next
// start even when the store has since pruned it.
type peakCycles struct {
	mu      sync.Mutex
	seeded  bool
	pending []history.Event
	rows    map[string]*peakCycleRow
	since   int64
	cancel  func()
}

// peakCycleRow is the tally of one shape and node set.
type peakCycleRow struct {
	shape     string
	nodeIDs   []string
	names     []string // the nodes' names in the latest cycle
	profiles  []string
	cycles    int // cycles with a peak on every node
	normal    int // of them, ended normally
	noPeak    int // cycles with no peak on at least one node
	first     int64
	last      int64
	total     peakSpread
	nodes     []peakSpread
	hasCycles bool
}

// peakSpread is the lowest, highest and latest of one figure.
type peakSpread struct{ low, high, latest int }

func (p *peakSpread) add(v int, first bool) {
	if first {
		p.low, p.high = v, v
	}
	p.low, p.high, p.latest = min(p.low, v), max(p.high, v), v
}

func (p peakSpread) view() map[string]any {
	return map[string]any{"low_mib": p.low, "high_mib": p.high, "latest_mib": p.latest}
}

// cyclePeakData is the part of an instanceUnloaded event's data the tally
// reads. Decoded through JSON so an event of this run (Go values) and one
// read back from a file (JSON values) read alike.
type cyclePeakData struct {
	Shape         string `json:"shape"`
	EndedNormally bool   `json:"ended_normally"`
	CyclePeaks    []struct {
		NodeID  string `json:"node_id"`
		Node    string `json:"node"`
		PeakMiB *int   `json:"peak_mib"`
	} `json:"cycle_peaks"`
}

// startPeakCycles subscribes to new instanceUnloaded events and adds the
// ones the store already holds; an event recorded while the store is read
// is counted once (by time and sequence).
func startPeakCycles(h *history.Store) *peakCycles {
	pc := &peakCycles{rows: map[string]*peakCycleRow{}}
	pc.cancel = h.Subscribe(func(e history.Event) {
		if e.Type != history.TInstanceUnloaded {
			return
		}
		pc.mu.Lock()
		defer supervise.Unlocked("peakcycles", pc.mu.Unlock)
		if !pc.seeded {
			pc.pending = append(pc.pending, e)
			return
		}
		pc.addLocked(e)
	})
	seen := map[evKey]bool{}
	var past []history.Event
	h.ScanType(history.TInstanceUnloaded, func(e history.Event) {
		seen[evKey{e.Ts, e.Seq}] = true
		past = append(past, e)
	})
	since := h.OldestTs()
	pc.mu.Lock()
	defer supervise.Unlocked("peakcycles", pc.mu.Unlock)
	pc.since = since
	for _, e := range past {
		pc.addLocked(e)
	}
	for _, e := range pc.pending {
		if !seen[evKey{e.Ts, e.Seq}] {
			pc.addLocked(e)
		}
	}
	pc.pending, pc.seeded = nil, true
	return pc
}

// addLocked counts one instanceUnloaded event; one of an older build (no
// cycle_peaks) is skipped.
func (pc *peakCycles) addLocked(e history.Event) {
	b, err := json.Marshal(e.Data)
	if err != nil {
		return
	}
	var d cyclePeakData
	if json.Unmarshal(b, &d) != nil || d.Shape == "" || len(d.CyclePeaks) == 0 {
		return
	}
	ids := make([]string, len(d.CyclePeaks))
	for i, n := range d.CyclePeaks {
		ids[i] = n.NodeID
	}
	key := d.Shape + "\x00" + strings.Join(ids, "\x00")
	r := pc.rows[key]
	if r == nil {
		r = &peakCycleRow{shape: d.Shape, nodeIDs: ids, nodes: make([]peakSpread, len(ids))}
		pc.rows[key] = r
	}
	r.names = make([]string, len(ids))
	complete, sum := true, 0
	for i, n := range d.CyclePeaks {
		r.names[i] = n.Node
		if n.PeakMiB == nil {
			complete = false
			continue
		}
		sum += *n.PeakMiB
	}
	if e.Profile != "" && !slices.Contains(r.profiles, e.Profile) {
		r.profiles = append(r.profiles, e.Profile)
		slices.Sort(r.profiles)
	}
	if !complete {
		r.noPeak++
		return
	}
	first := !r.hasCycles
	for i, n := range d.CyclePeaks {
		r.nodes[i].add(*n.PeakMiB, first)
	}
	r.total.add(sum, first)
	r.cycles++
	if d.EndedNormally {
		r.normal++
	}
	if first || e.Ts < r.first {
		r.first = e.Ts
	}
	r.last = max(r.last, e.Ts)
	r.hasCycles = true
}

// view is the JSON of peak_cycles: a row per shape and node set (sorted),
// and how many of them have at least 1, 3 and 5 cycles with a peak.
func (pc *peakCycles) view() map[string]any {
	pc.mu.Lock()
	defer supervise.Unlocked("peakcycles", pc.mu.Unlock)
	keys := make([]string, 0, len(pc.rows))
	for k := range pc.rows {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	rows := make([]map[string]any, 0, len(keys))
	var at1, at3, at5 int
	for _, k := range keys {
		r := pc.rows[k]
		at1, at3, at5 = at1+b2i(r.cycles >= 1), at3+b2i(r.cycles >= 3), at5+b2i(r.cycles >= 5)
		row := map[string]any{"shape": r.shape, "placement": r.nodeIDs, "node_names": r.names, "profiles": r.profiles,
			"cycles": r.cycles, "normal_cycles": r.normal, "cycles_without_peak": r.noPeak,
			"first_at": nil, "last_at": nil, "total": nil}
		if r.hasCycles {
			row["first_at"], row["last_at"] = isoTime(time.UnixMilli(r.first)), isoTime(time.UnixMilli(r.last))
			row["total"] = r.total.view()
			nodes := make([]map[string]any, len(r.nodes))
			for i, s := range r.nodes {
				nodes[i] = s.view()
				nodes[i]["node"] = r.nodeIDs[i]
			}
			row["nodes"] = nodes
		} else {
			row["nodes"] = []map[string]any{}
		}
		rows = append(rows, row)
	}
	var since any
	if pc.since > 0 {
		since = isoTime(time.UnixMilli(pc.since))
	}
	return map[string]any{"rows": rows, "since": since,
		"totals": map[string]any{"rows": len(rows), "at_least_1": at1, "at_least_3": at3, "at_least_5": at5}}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
