package api

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/bench"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/history"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/proxy"
)

// The model catalog is kept in memory (design 9.4). It is built once, in the
// background at start (Warm) or on the first request, and built again only
// when something it is joined from changed:
//
//   - a history event of a type the catalog reads arrives (the server keeps
//     its own list of those events, fed by the history subscription, so the
//     history store is queried once per process and never per request);
//   - the configuration changes, or an icon is set;
//   - a benchmark run changes the aggregates (they are computed again only then);
//   - the status differs from the one the catalog was built from.
//
// File metadata comes from the modelFileCache (by path, size and modification
// time, persisted in the database), so a GGUF header is parsed once per file
// version, not once per start. GET /api/models writes the stored bytes; the
// live channel sends the same document on the "models" topic when a client
// subscribes and then only the entries that changed.

// catalogSnap is one built catalog. It is immutable once published.
type catalogSnap struct {
	version int64
	in      CatalogInput
	keys    []string
	raw     map[string]json.RawMessage // key -> the entry as JSON
	hash    map[string]string
	list    []byte // the GET /api/models document
	sig     string // the status it was built from
	builtAt time.Time
}

type catalogCache struct {
	mu      sync.Mutex
	snap    *catalogSnap
	dirty   bool // an event, a configuration change or an icon change arrived since the build
	aggOld  bool // the benchmark aggregates must be computed again
	agg     map[string]any
	events  []history.Event // newest first: the catalog's own copy of the events it reads
	seeded  bool
	pending []history.Event // arrived before the seed
	version int64

	buildMu  sync.Mutex
	kick     chan struct{}
	quit     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	cancels  []func()

	histQueries atomic.Int64 // how often the history store was asked (tests)
	builds      atomic.Int64
}

var catalogTypes = func() map[string]bool {
	m := map[string]bool{}
	for _, t := range catalogEventTypes {
		m[t] = true
	}
	return m
}()

// catalogMinGap is the least time between two rebuilds that only a changed
// status asked for (node memory figures move every few seconds).
const catalogMinGap = 2 * time.Second

func (s *Server) startCatalog() {
	c := &s.catalog
	c.kick, c.quit, c.done = make(chan struct{}, 1), make(chan struct{}), make(chan struct{})
	c.aggOld = true
	if st, ok := s.opts.Audit.(fileMetaStore); ok {
		s.files.store = st
	}
	if s.hist.On() {
		c.cancels = append(c.cancels, s.hist.Subscribe(func(e history.Event) {
			if !catalogTypes[e.Type] {
				return
			}
			c.mu.Lock()
			if c.seeded {
				c.addEvent(e)
			} else {
				c.pending = append(c.pending, e)
			}
			c.dirty = true
			c.mu.Unlock()
			c.poke()
		}))
	}
	if s.benchOn() {
		c.cancels = append(c.cancels, s.opts.Bench.Subscribe(func(ev map[string]any) {
			if k := ev["kind"]; k == "aggregate_updated" || k == "run" {
				s.catalogDirty(true)
			}
		}))
	}
	go func() { // rebuilds for the live channel, off the request path
		defer close(c.done)
		for {
			select {
			case <-c.quit:
				return
			case <-c.kick:
			}
			select { // let a burst of events settle
			case <-c.quit:
				return
			case <-time.After(120 * time.Millisecond):
			}
			if s.ws == nil || s.ws.clientCount() == 0 {
				continue // nobody is watching: the next request builds it
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			s.catalogBuild(ctx, nil)
			cancel()
		}
	}()
}

func (s *Server) stopCatalog() {
	c := &s.catalog
	c.stopOnce.Do(func() {
		if c.quit == nil {
			return
		}
		for _, f := range c.cancels {
			f()
		}
		close(c.quit)
		<-c.done
	})
}

func (c *catalogCache) poke() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// addEvent puts e in front of the list (the caller holds mu).
func (c *catalogCache) addEvent(e history.Event) {
	c.events = append(c.events, history.Event{})
	copy(c.events[1:], c.events)
	c.events[0] = e
	if len(c.events) > history.MaxLimit {
		c.events = c.events[:history.MaxLimit]
	}
}

// catalogDirty says that something the catalog is joined from changed.
func (s *Server) catalogDirty(aggregates bool) {
	c := &s.catalog
	c.mu.Lock()
	c.dirty = true
	if aggregates {
		c.aggOld = true
	}
	c.mu.Unlock()
	if c.kick != nil {
		c.poke()
	}
}

// catalogStatus is called with every status the live channel looked at.
func (s *Server) catalogStatus(v any) {
	c := &s.catalog
	c.mu.Lock()
	snap, dirty := c.snap, c.dirty
	c.mu.Unlock()
	if dirty {
		c.poke()
		return
	}
	if snap == nil || time.Since(snap.builtAt) < catalogMinGap {
		return
	}
	if s.opts.catalogSource != nil {
		if _, sig, ok := s.opts.catalogSource(context.Background(), snap.sig); ok {
			if sig != snap.sig {
				c.poke()
			}
			return
		}
	}
	if catalogSig(v) != snap.sig {
		c.poke()
	}
}

// catalogSig is what of a status document the catalog depends on: everything
// but the clock fields and the progress of a load in flight (the instance
// cards read that from the status itself).
func catalogSig(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	cp := make(map[string]any, len(m))
	for k, val := range m {
		cp[k] = val
	}
	if insts, ok := cp["instances"].([]any); ok {
		out := make([]any, 0, len(insts))
		for _, i := range insts {
			im, ok := i.(map[string]any)
			if !ok {
				continue
			}
			ic := make(map[string]any, len(im))
			for k, val := range im {
				switch k {
				case "load", "load_progress", "load_eta_s", "load_elapsed_s":
				default:
					ic[k] = val
				}
			}
			out = append(out, ic)
		}
		cp["instances"] = out
	}
	delete(cp, "recent_loads")
	return statusSignature(cp)
}

// seedEvents asks the history store once for the events the catalog reads.
func (s *Server) seedEvents() {
	c := &s.catalog
	c.mu.Lock()
	seeded := c.seeded
	c.mu.Unlock()
	if seeded {
		return
	}
	c.histQueries.Add(1)
	ev := s.hist.Query(history.Query{Types: catalogEventTypes, Limit: history.MaxLimit}).Events
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seeded {
		return
	}
	var top uint64
	have := map[uint64]bool{}
	for _, e := range ev {
		have[e.Seq] = true
		if e.Seq > top {
			top = e.Seq
		}
	}
	c.events = ev
	for _, e := range c.pending { // arrived while the query ran
		if !have[e.Seq] {
			c.addEvent(e)
		}
	}
	c.pending, c.seeded = nil, true
}

// catalogGet returns the catalog, building it when there is none yet or it is
// out of date. A warm call asks the scheduler for its status (one message to
// its loop) and nothing else: no file is read and the history is not queried.
func (s *Server) catalogGet(ctx context.Context) (*catalogSnap, error) {
	c := &s.catalog
	if s.opts.catalogSource != nil {
		c.mu.Lock()
		snap, dirty := c.snap, c.dirty
		c.mu.Unlock()
		have := ""
		if snap != nil && !dirty {
			have = snap.sig
		}
		if in, sig, ok := s.opts.catalogSource(ctx, have); ok {
			if have != "" && sig == have {
				return snap, nil
			}
			return s.catalogBuildFrom(ctx, in, sig), nil
		}
	}
	st, err := s.statusView(ctx)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	snap, dirty := c.snap, c.dirty
	c.mu.Unlock()
	if snap != nil && !dirty && catalogSig(st) == snap.sig {
		return snap, nil
	}
	return s.catalogBuild(ctx, st)
}

// CatalogStatusSig is what of a status document the catalog depends on (for
// the dev harness, which says with it when its scripted cluster changed).
func CatalogStatusSig(status any) string { return catalogSig(status) }

// catalogBuildFrom publishes the catalog of an input the dev harness gave.
func (s *Server) catalogBuildFrom(ctx context.Context, in CatalogInput, sig string) *catalogSnap {
	c := &s.catalog
	c.buildMu.Lock()
	defer c.buildMu.Unlock()
	c.mu.Lock()
	prev := c.snap
	c.dirty, c.aggOld = false, false
	c.mu.Unlock()
	c.builds.Add(1)
	if s.store != nil {
		in.Icons, _ = s.store.IconTags(ctx)
	}
	in.Now = time.Now()
	snap := newCatalogSnap(in, prev, "/models")
	snap.sig, snap.builtAt = sig, time.Now()
	c.mu.Lock()
	c.snap = snap
	c.mu.Unlock()
	if s.ws != nil && prev != nil && snap.version != prev.version {
		s.ws.publishModels(snap, prev)
	}
	return snap
}

// catalogBuild joins the catalog again and publishes it (st may be nil: the
// status is then asked for).
func (s *Server) catalogBuild(ctx context.Context, st any) (*catalogSnap, error) {
	c := &s.catalog
	if s.opts.catalogSource != nil {
		if in, sig, ok := s.opts.catalogSource(ctx, ""); ok {
			return s.catalogBuildFrom(ctx, in, sig), nil
		}
	}
	c.buildMu.Lock()
	defer c.buildMu.Unlock()
	if st == nil {
		var err error
		if st, err = s.statusView(ctx); err != nil {
			return nil, err
		}
	}
	sig := catalogSig(st)
	c.mu.Lock()
	prev, dirty, aggOld := c.snap, c.dirty, c.aggOld
	c.dirty, c.aggOld = false, false
	c.mu.Unlock()
	if prev != nil && !dirty && prev.sig == sig { // another caller built it while this one waited
		return prev, nil
	}
	c.builds.Add(1)
	s.seedEvents()

	in := CatalogInput{HistoryOn: s.hist.On(), Now: time.Now()}
	in.Status, _ = st.(map[string]any)
	in.Profiles = s.profileEntries(ctx)
	if s.opts.Bench != nil {
		if aggOld || c.agg == nil {
			if doc, err := s.opts.Bench.Aggregates(bench.AggFilter{}); err == nil {
				c.agg = viaJSON(doc)
			}
		}
		in.Aggregates = c.agg
	}
	if s.opts.modelFiles != nil {
		in.Files = s.opts.modelFiles()
	} else {
		var named []string
		for _, p := range in.Profiles {
			if jstr(p["kind"]) != "command" {
				named = append(named, jstr(p["model"]))
			}
		}
		in.Files = s.files.list(s.conf().ModelsDir, named)
	}
	if s.store != nil {
		in.Icons, _ = s.store.IconTags(ctx)
	}
	c.mu.Lock()
	in.Events = append([]history.Event(nil), c.events...)
	c.mu.Unlock()

	snap := newCatalogSnap(in, prev, s.conf().ModelsDir)
	snap.sig, snap.builtAt = sig, time.Now()
	c.mu.Lock()
	c.snap = snap
	c.mu.Unlock()
	if s.ws != nil && (prev == nil || snap.version != prev.version) {
		s.ws.publishModels(snap, prev)
	}
	return snap, nil
}

// newCatalogSnap builds the catalog of in. The version rises when an entry
// differs from prev (or there is no prev).
func newCatalogSnap(in CatalogInput, prev *catalogSnap, modelsDir string) *catalogSnap {
	entries := BuildModelCatalog(in, "")
	snap := &catalogSnap{in: in, raw: map[string]json.RawMessage{}, hash: map[string]string{}}
	changed := prev == nil || len(prev.keys) != len(entries)
	list := make([]json.RawMessage, 0, len(entries))
	for i, e := range entries {
		key := jstr(e["key"])
		b, _ := json.Marshal(e)
		sum := sha1.Sum(b)
		snap.keys = append(snap.keys, key)
		snap.raw[key], snap.hash[key] = b, hex.EncodeToString(sum[:])
		list = append(list, b)
		if !changed && (prev.keys[i] != key || prev.hash[key] != snap.hash[key]) {
			changed = true
		}
	}
	snap.version = 1
	if prev != nil {
		snap.version = prev.version
		if changed {
			snap.version++
		}
	}
	snap.list, _ = json.Marshal(map[string]any{"models": list, "models_dir": modelsDir, "history": in.HistoryOn, "version": snap.version})
	snap.list = append(snap.list, '\n')
	return snap
}

// delta is the "models" message that takes a client from prev to snap: the
// entries that changed, the keys that went away and the order of all keys.
func (snap *catalogSnap) delta(prev *catalogSnap) map[string]any {
	changed, removed := []json.RawMessage{}, []string{}
	for _, k := range snap.keys {
		if prev.hash[k] != snap.hash[k] {
			changed = append(changed, snap.raw[k])
		}
	}
	for _, k := range prev.keys {
		if _, ok := snap.raw[k]; !ok {
			removed = append(removed, k)
		}
	}
	return map[string]any{"version": snap.version, "base": prev.version, "changed": changed, "removed": removed, "order": snap.keys}
}

// Warm builds the catalog in the background, so that the first request and
// the first live-channel client find it ready. Call it once after New.
func (s *Server) Warm() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := s.catalogBuild(ctx, nil); err != nil {
			s.log.Debug("the model catalog could not be built at start; the first request builds it", "err", err)
		}
	}()
}

// handleModelCatalog is GET /api/models: one entry per model file, from memory.
func (s *Server) handleModelCatalog(w http.ResponseWriter, r *http.Request) {
	snap, err := s.catalogGet(r.Context())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-Catalog-Version", strconv.FormatInt(snap.version, 10))
	w.Write(snap.list)
}

// handleModelDetail is GET /api/models/{key}: one model with its recent
// events, its chart series and the benchmarks that use it. The key is the file
// name, or "command:<profile id>" for a profile of kind command.
// GET /api/models/{key}/icon is the model's icon (icons.go).
func (s *Server) handleModelDetail(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if k, ok := iconPath(key); ok {
		s.handleIconGet(w, r, k)
		return
	}
	snap, err := s.catalogGet(r.Context())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	in := snap.in
	in.Now = time.Now()
	out := BuildModelCatalog(in, key)
	if len(out) == 0 {
		proxy.WriteError(w, http.StatusNotFound, "no model file "+key+": see GET /api/models", "invalid_request_error", "not found", nil)
		return
	}
	writeJSON(w, http.StatusOK, out[0])
}
