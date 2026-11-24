package api

import (
	"math"
	"sync"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// The last load's transfer rate per zone (Zones-Fleet-Detail "Link
// speed"): from the newest loadFinished event that ended ready and sent
// the zone at least 1 MiB, the bytes Potluck's host sent to that zone's
// worker over the load's time. It is a lower bound of the link's rate (the
// load time also holds the host's own work); it is not a link test, which
// needs the agent (R1). Kept in memory: seeded from the history in the
// background at start, then from each new event.

// LoadLink is one zone's last load transfer (pool.LoadLink, a zone fact).
type LoadLink = pool.LoadLink

type loadLinks struct {
	mu     sync.Mutex
	byName map[string]loadLinkAt
	cancel func()
}

type loadLinkAt struct {
	ts int64
	l  LoadLink
}

const minLinkBytes = 1 << 20 // less than this is the handshake only

func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	case int:
		return float64(x), true
	}
	return 0, false
}

func asList(v any) []map[string]any {
	switch x := v.(type) {
	case []map[string]any:
		return x
	case []any:
		var out []map[string]any
		for _, e := range x {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// add takes one loadFinished event; an older one never replaces a newer.
func (ll *loadLinks) add(e history.Event) {
	if e.Type != history.TLoadFinished || e.Data == nil || e.Data["outcome"] != "ready" {
		return
	}
	loadS, ok := asFloat(e.Data["load_s"])
	if !ok || loadS <= 0 {
		return
	}
	at := time.UnixMilli(e.Ts).UTC().Format("2006-01-02T15:04:05.000Z")
	ll.mu.Lock()
	defer supervise.Unlocked("loadlinks", ll.mu.Unlock)
	for _, n := range asList(e.Data["nodes_detail"]) {
		name, _ := n["node"].(string)
		b, ok := asFloat(n["bytes"])
		if name == "" || !ok || b < minLinkBytes {
			continue
		}
		if old, ok := ll.byName[name]; ok && old.ts > e.Ts {
			continue
		}
		ll.byName[name] = loadLinkAt{e.Ts, LoadLink{MBs: math.Round(b/1e6/loadS*10) / 10, Bytes: int64(b), LoadS: loadS, At: at, Model: e.Profile}}
	}
}

func (ll *loadLinks) get(name string) *LoadLink {
	if ll == nil {
		return nil
	}
	ll.mu.Lock()
	defer supervise.Unlocked("loadlinks", ll.mu.Unlock)
	if x, ok := ll.byName[name]; ok {
		l := x.l
		return &l
	}
	return nil
}

// startLoadLinks subscribes to new events and reads the kept ones in the
// background (the start does not wait for the scan).
func (s *Server) startLoadLinks() {
	ll := &loadLinks{byName: map[string]loadLinkAt{}}
	s.links = ll
	if !s.hist.On() {
		return
	}
	ll.cancel = s.hist.Subscribe(ll.add)
	s.bgWG.Add(1)
	go supervise.Once("api.loadlinks", func() {
		defer s.bgWG.Done()
		s.hist.ScanType(history.TLoadFinished, func(e history.Event) {
			if s.bg.Err() == nil {
				ll.add(e)
			}
		})
	}, func(p any) { s.log.Warn("load link scan stopped", "panic", p) })
}

func (s *Server) stopLoadLinks() {
	if s.links != nil && s.links.cancel != nil {
		s.links.cancel()
	}
}

// setLoadLinks writes last_load_link on each fact.
func (s *Server) setLoadLinks(facts []pool.NodeFacts) {
	for i := range facts {
		facts[i].LastLoadLink = s.linkOf(facts[i].Name)
	}
}

func (s *Server) linkOf(name string) *pool.LoadLink { return s.links.get(name) }
