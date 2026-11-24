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

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/modelname"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/proxy"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
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
	namer   *modelname.Namer // the short names of its files (request rows use it too)
}

type catalogCache struct {
	mu      sync.Mutex
	snap    *catalogSnap
	dirty   bool // an event, a configuration change or an icon change arrived since the build
	aggOld  bool // the benchmark aggregates must be computed again
	agg     map[string]any
	runs    []bench.RunStepModel // completed runs' steps, read with agg
	events  []history.Event      // newest first: the catalog's own copy of the events it reads
	seeded  bool
	pending []history.Event // arrived before the seed

	buildMu  sync.Mutex
	kick     chan struct{}
	quit     chan struct{}
	done     chan struct{}
	stop     context.CancelFunc // ends the rebuild loop's supervision (and its backoff)
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
			// This runs on the recording goroutine, often the scheduler's
			// event loop: a bad event costs one catalog rebuild, never the loop.
			supervise.Guard("api.catalog.event", func() { c.takeEvent(e) }, func(any) { s.catalogInvalidate() })
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
	// Rebuilds for the live channel, off the request path. A failure
	// drops the catalog's derived state (catalogInvalidate) and the loop
	// runs again; the last published catalog is served meanwhile.
	var ctx context.Context
	ctx, c.stop = context.WithCancel(context.Background())
	supervise.Go(ctx, supervise.Spec{Name: "api.catalog", Policy: supervise.Restart,
		Rebuild: func(any) { s.catalogInvalidate() }, Done: func() { close(c.done) }}, func(ctx context.Context) {
		for {
			select {
			case <-c.quit:
				return
			case <-ctx.Done():
				return
			case <-c.kick:
			}
			select { // let a burst of events settle
			case <-c.quit:
				return
			case <-ctx.Done():
				return
			case <-time.After(120 * time.Millisecond):
			}
			if s.ws == nil || s.ws.clientCount() == 0 {
				continue // nobody is watching: the next request builds it
			}
			bctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			s.catalogBuild(bctx, nil)
			cancel()
		}
	})
}

// takeEvent adds a history event the catalog reads (or keeps it until the
// seed) and marks the catalog out of date.
func (c *catalogCache) takeEvent(e history.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	supervise.Point("api.catalog.event", c)
	if c.seeded {
		c.addEvent(e)
	} else {
		c.pending = append(c.pending, e)
	}
	c.dirty = true
}

// catalogInvalidate is the catalog's rebuild after a failure: everything it
// derived may be corrupt, so its own copy of the events is dropped and asked
// of the history store again, the aggregates are computed again, and the
// next build starts at once. The last published snapshot is immutable and is
// served until that build replaces it.
func (s *Server) catalogInvalidate() {
	c := &s.catalog
	func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.dirty, c.aggOld = true, true
		c.seeded, c.events, c.pending = false, nil, nil
	}()
	if c.kick != nil {
		c.poke()
	}
}

// markDirty says the catalog must be built again (aggregates too when asked).
func (c *catalogCache) markDirty(aggregates bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dirty = true
	if aggregates {
		c.aggOld = true
	}
}

// current is the published snapshot and whether it is out of date.
func (c *catalogCache) current() (*catalogSnap, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snap, c.dirty
}

// publish makes snap the catalog.
func (c *catalogCache) publish(snap *catalogSnap) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snap = snap
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
		c.stop()
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
	c.markDirty(aggregates)
	if c.kick != nil {
		c.poke()
	}
}

// catalogStatus is called with every status the live channel looked at.
func (s *Server) catalogStatus(v any) {
	c := &s.catalog
	snap, dirty := c.current()
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
	if func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.seeded }() {
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
		snap, dirty := c.current()
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
	snap, dirty := c.current()
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
	prev, _, _ := c.take()
	built := false
	defer c.unlessBuilt(&built)
	c.builds.Add(1)
	if s.store != nil {
		in.Icons, _ = s.store.IconTags(ctx)
		in.Pins = s.modelPins(ctx)
	}
	in.Now = time.Now()
	snap := newCatalogSnap(in, prev, "/models")
	snap.sig, snap.builtAt = sig, time.Now()
	c.publish(snap)
	built = true
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
	prev, dirty, aggOld := c.take()
	built := false
	defer c.unlessBuilt(&built)
	if prev != nil && !dirty && prev.sig == sig { // another caller built it while this one waited
		built = true
		return prev, nil
	}
	supervise.Point("api.catalog.build", c)
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
			if s.opts.BenchStore != nil {
				if rm, err := s.opts.BenchStore.CompletedRunModels(); err == nil {
					c.runs = rm
				}
			}
		}
		in.Aggregates, in.RunModels = c.agg, c.runs
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
		in.Pins = s.modelPins(ctx)
	}
	in.Events = c.eventsCopy()

	snap := newCatalogSnap(in, prev, s.conf().ModelsDir)
	snap.sig, snap.builtAt = sig, time.Now()
	c.publish(snap)
	built = true
	if s.ws != nil && (prev == nil || snap.version != prev.version) {
		s.ws.publishModels(snap, prev)
	}
	return snap, nil
}

// take is the published snapshot and the dirty flags, which it clears: the
// caller builds now.
func (c *catalogCache) take() (prev *catalogSnap, dirty, aggOld bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev, dirty, aggOld = c.snap, c.dirty, c.aggOld
	c.dirty, c.aggOld = false, false
	return prev, dirty, aggOld
}

// unlessBuilt marks the catalog out of date again when a build that took the
// flags did not publish (it panicked: a request's build is recovered by
// net/http, the loop's by its supervision), so the next caller builds.
func (c *catalogCache) unlessBuilt(built *bool) {
	if !*built {
		c.markDirty(true)
	}
}

// eventsCopy is the catalog's events, newest first.
func (c *catalogCache) eventsCopy() []history.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]history.Event(nil), c.events...)
}

// newCatalogSnap builds the catalog of in. The version rises when an entry
// differs from prev (or there is no prev).
func newCatalogSnap(in CatalogInput, prev *catalogSnap, modelsDir string) *catalogSnap {
	entries := BuildModelCatalog(in, "")
	snap := &catalogSnap{in: in, raw: map[string]json.RawMessage{}, hash: map[string]string{}, namer: modelname.NewNamer(catalogFiles(in))}
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
	s.bgWG.Add(1)
	go supervise.Once("api.catalog.warm", func() { // a failure is reported; the first request builds it
		defer s.bgWG.Done()
		ctx, cancel := context.WithTimeout(s.bg, 30*time.Second)
		defer cancel()
		if _, err := s.catalogBuild(ctx, nil); err != nil {
			s.log.Debug("the model catalog could not be built at start; the first request builds it", "err", err)
		}
	}, nil)
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
	w.Write(s.withModelsDisk(snap.list))
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

// modelName is a model file's short name with the catalog as it is now (the
// request rows: usage.Recorder.SetNamer). Before the first catalog is built
// it chooses among the configured profiles' files.
func (s *Server) modelName(file string) string {
	if snap, _ := s.catalog.current(); snap != nil && snap.namer != nil {
		return snap.namer.Name(file)
	}
	var files []string
	if c := s.conf(); c != nil {
		for _, p := range c.Profiles {
			if p != nil && p.Model != "" {
				files = append(files, p.Model)
			}
		}
	}
	return modelname.NewNamer(files).Name(file)
}
