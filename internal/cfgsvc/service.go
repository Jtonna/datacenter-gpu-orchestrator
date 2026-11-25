// Package cfgsvc is the one implementation of configuration changes: REST
// handlers and MCP tools both call it. Every change is validated against the
// whole resulting configuration, committed as a new revision with audit
// entries in one transaction, then handed to the scheduler as a snapshot.
package cfgsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// ValidationError is a rejected change; Messages are the validation messages.
type ValidationError struct{ Messages []string }

func (e *ValidationError) Error() string { return strings.Join(e.Messages, "; ") }

func validationFrom(err error) *ValidationError {
	var msgs []string
	for _, m := range strings.Split(err.Error(), "\n") {
		if m = strings.TrimSpace(m); m != "" {
			msgs = append(msgs, m)
		}
	}
	return &ValidationError{Messages: msgs}
}

// Errors the transports map to status codes.
var (
	ErrExists   = errors.New("already exists")
	ErrNotFound = errors.New("not found")
	ErrNoStore  = errors.New("the configuration database is not available")
	// ErrApplyFailed: the change is committed, but applying it to the
	// running pieces failed, so Potluck restarts to apply it from the store.
	ErrApplyFailed = errors.New("the change is saved, but applying it to the running scheduler failed; Potluck restarts now to apply it")
)

// RefError means a delete is refused because benchmarks refer to the profile.
type RefError struct {
	ID       string
	Warnings []string
}

func (e *RefError) Error() string {
	return fmt.Sprintf("profile %q is referenced; repeat with force to delete it anyway: %s", e.ID, strings.Join(e.Warnings, "; "))
}

// UnknownFieldsError refuses a write while the stored configuration has
// fields this version does not know (a newer version wrote them): the write
// would store the document without them, so they would be lost. Force
// writes anyway.
type UnknownFieldsError struct{ Fields []string }

func (e *UnknownFieldsError) Error() string {
	return "the stored configuration has fields this version does not know (" + strings.Join(e.Fields, ", ") +
		"), written by a newer version; a write now would drop them. Write with the newer version, or repeat with force to write anyway and drop them"
}

// WriteOpts are the options every write takes.
type WriteOpts struct {
	Origin  audit.Origin
	DryRun  bool
	IfMatch *int64
	Force   bool
}

// Result describes a write.
type Result struct {
	Revision        int64            `json:"revision"`
	AuditEntryIDs   []int64          `json:"audit_entry_ids"`
	Changed         bool             `json:"changed"`
	DryRun          bool             `json:"dry_run"`
	Diff            []audit.DiffItem `json:"diff"`
	Warnings        []string         `json:"warnings"`
	RestartRequired []string         `json:"restart_required"`
	// BlockedByExclusion lists the profiles that force a node exclude_nodes
	// names (0.1.7): valid configuration, kept, refused at run time until
	// the exclusion is lifted. Each is also one entry of Warnings. It
	// describes the configuration after the write, whatever the write was.
	BlockedByExclusion []config.ExclusionBlock `json:"blocked_by_exclusion"`
	// LoadedOnExcludedNodes lists the instances that run now on a node the
	// configuration after the write excludes (0.1.7 review): they were
	// placed before the exclusion, are not drained, and keep running and
	// serving until they are unloaded. Each is also one entry of Warnings.
	LoadedOnExcludedNodes []config.LoadedOnExcluded `json:"loaded_on_excluded_nodes"`
}

// ChangeEvent is announced after every committed change.
type ChangeEvent struct {
	Revision int64
	EntryIDs []int64
	Section  string
	Target   string
	Source   string
	Actor    string
}

// Options configure the service.
type Options struct {
	Store       *cfgstore.Store // nil: configuration cannot be changed (database unavailable)
	Boot        config.Bootstrap
	Parse       config.Options
	Apply       func(*config.Config) // delivers a new snapshot to the scheduler; must not block
	OnChange    func(ChangeEvent)
	ProfileRefs func(profile string) []string // benchmarks that use the profile
	// LoadedOnExcluded lists the instances running now on nodes the given
	// configuration excludes (the scheduler's answer); nil: none known.
	LoadedOnExcluded func(*config.Config) []config.LoadedOnExcluded
	Logger           *slog.Logger
}

// Service holds the current configuration.
type Service struct {
	o   Options
	log *slog.Logger

	mu sync.Mutex // serializes writes
	// The Apply and OnChange callbacks run after mu is released, one change
	// at a time in revision order: a change takes ticket cbNext under mu and
	// runs its callbacks once cbDone reaches it (under cbMu).
	cbNext  uint64
	cbMu    sync.Mutex
	cbCond  *sync.Cond
	cbDone  uint64
	rev     int64
	updated int64
	doc     *config.Document
	cfgMu   sync.RWMutex
	cfg     *config.Config
	loadErr string
	// unknown names the fields of the stored configuration this version
	// does not know (config.ParseStoredDocument); while there are any,
	// writes are refused unless forced. Guarded by mu.
	unknown []string
}

// New loads the latest revision. A database failure or an invalid stored
// document does not stop the scheduler: it starts from built-in defaults and
// LoadError says why.
func New(ctx context.Context, o Options) *Service {
	s := &Service{o: o, log: o.Logger, doc: config.NewDocument()}
	if s.log == nil {
		s.log = slog.Default()
	}
	s.setConfig(mustBuild(config.NewDocument(), o.Boot, o.Parse))
	if o.Store == nil {
		s.loadErr = ErrNoStore.Error()
		return s
	}
	r, err := o.Store.Latest(ctx)
	if err != nil {
		s.loadErr = "cannot read the stored configuration: " + err.Error()
		s.log.Error("configuration database", "err", err)
		return s
	}
	if r.Revision == 0 {
		s.log.Info("no configuration yet: starting with built-in defaults and no profiles; import one with `llm-scheduler import-config` or the REST API")
		return s
	}
	s.rev, s.updated, s.doc = r.Revision, r.TS, r.Doc
	if len(r.Unknown) > 0 {
		s.unknown = append([]string(nil), r.Unknown...)
		for _, f := range r.Unknown {
			s.log.Warn("the stored configuration has a field this version does not know (a newer version wrote it); it is ignored, everything else is used, and configuration writes are refused unless forced because they would drop it",
				"revision", r.Revision, "field", f)
		}
	}
	boot := o.Parse
	boot.KeepCtx = keptCtx(r.Doc)   // stored profiles load whatever their ctx
	boot.KeepArgs = keptArgs(r.Doc) // a load is never a new write: a flag managed later stays loadable
	cfg, err := config.Build(r.Doc, o.Boot, boot)
	if err != nil {
		// A model file that went missing must not stop the scheduler: build without the file check.
		// A stored profile whose model path the path rule refuses (internal/modelpath) is kept the
		// same way: its load is refused at plan time, and only writes that point a profile at such a
		// path are refused.
		loose := o.Parse
		loose.FileExists = func(string) bool { return true }
		loose.KeepModels, loose.KeepArgs = keptModels(r.Doc), keptArgs(r.Doc)
		loose.TrainedCtx = nil
		var err2 error
		if cfg, err2 = config.Build(r.Doc, o.Boot, loose); err2 != nil {
			s.loadErr = "the stored configuration (revision " + fmt.Sprint(r.Revision) + ") is invalid: " + err2.Error()
			s.log.Error("stored configuration is invalid; running with built-in defaults", "revision", r.Revision, "err", err2)
			s.setConfig(mustBuild(config.NewDocument(), o.Boot, o.Parse))
			return s
		}
		s.log.Warn("stored configuration has problems that only the file check finds", "err", err)
	}
	for _, w := range cfg.LateManagedArgs() {
		s.log.Warn("stored profile sets a flag the scheduler now manages; the launch drops it", "revision", r.Revision, "detail", w)
	}
	cfg.Revision = r.Revision
	s.setConfig(cfg)
	return s
}

func mustBuild(d *config.Document, b config.Bootstrap, o config.Options) *config.Config {
	c, err := config.Build(d, b, o)
	if err != nil { // an empty document is always valid
		panic(err)
	}
	c.Configured = false
	return c
}

func (s *Service) setConfig(c *config.Config) {
	func() {
		s.cfgMu.Lock()
		defer supervise.Unlocked("cfgsvc", s.cfgMu.Unlock)
		s.cfg = c
	}()
}

// Config is the current snapshot.
func (s *Service) Config() *config.Config {
	s.cfgMu.RLock()
	defer supervise.Unlocked("cfgsvc", s.cfgMu.RUnlock)
	return s.cfg
}

// UnknownFields names the fields of the stored configuration this version
// does not know; nil when there are none (GET /api/system).
func (s *Service) UnknownFields() []string {
	s.mu.Lock()
	defer supervise.Unlocked("cfgsvc", s.mu.Unlock)
	return append([]string(nil), s.unknown...)
}

// LoadError is non-empty when the stored configuration could not be used.
func (s *Service) LoadError() string { return s.loadErr }

// Revision is the current revision (0 when nothing is configured).
func (s *Service) Revision() int64 {
	s.mu.Lock()
	defer supervise.Unlocked("cfgsvc", s.mu.Unlock)
	return s.rev
}

// Store exposes the underlying store (nil when unavailable).
func (s *Service) Store() *cfgstore.Store { return s.o.Store }

// View is GET /api/config.
type View struct {
	Revision      int64                              `json:"revision"`
	UpdatedAt     int64                              `json:"updated_at"`
	Configured    bool                               `json:"configured"`
	Error         string                             `json:"error,omitempty"`
	Settings      map[string]any                     `json:"settings"`
	SettingsSet   []string                           `json:"settings_set"`
	Tiers         []config.TierSpec                  `json:"tiers"`
	Profiles      map[string]*config.ProfileSpec     `json:"profiles"`
	ExcludeNodes  []string                           `json:"exclude_nodes"`
	NoAutoNodes   []string                           `json:"no_auto_nodes"`
	NodeOverrides map[string]config.NodeOverrideSpec `json:"node_overrides"`
	Recovery      map[string]any                     `json:"recovery"`
	Fleet         map[string]any                     `json:"fleet"`
}

// View returns the effective configuration (defaults filled in).
func (s *Service) View() View {
	var rev int64
	var upd int64
	var doc *config.Document
	func() {
		s.mu.Lock()
		defer supervise.Unlocked("cfgsvc", s.mu.Unlock)
		rev, upd, doc = s.rev, s.updated, s.doc.Clone()
	}()
	return s.viewOf(rev, upd, doc, s.Config(), s.loadErr)
}

func (s *Service) viewOf(rev, upd int64, doc *config.Document, cfg *config.Config, errText string) View {
	v := View{Revision: rev, UpdatedAt: upd, Configured: rev > 0 && errText == "", Error: errText,
		Settings: map[string]any{}, Recovery: map[string]any{}, Fleet: map[string]any{},
		Tiers: doc.Tiers, Profiles: doc.Profiles, ExcludeNodes: doc.ExcludeNodes, NoAutoNodes: doc.NoAutoNodes,
		NodeOverrides: doc.NodeOverrides, SettingsSet: []string{}}
	for _, f := range config.SettingFields(config.ScopeSettings) {
		v.Settings[f.Key] = config.SettingValue(cfg, f.Scope, f.Key)
	}
	for _, f := range config.SettingFields(config.ScopeRecovery) {
		v.Recovery[f.Key] = config.SettingValue(cfg, f.Scope, f.Key)
	}
	for _, f := range config.SettingFields(config.ScopeFleet) {
		v.Fleet[f.Key] = config.SettingValue(cfg, f.Scope, f.Key)
	}
	for k := range doc.Settings {
		v.SettingsSet = append(v.SettingsSet, k)
	}
	sort.Strings(v.SettingsSet)
	return v
}

// RevisionView is the full configuration at a revision.
func (s *Service) RevisionView(ctx context.Context, n int64) (*View, *cfgstore.Revision, error) {
	if s.o.Store == nil {
		return nil, nil, ErrNoStore
	}
	r, err := s.o.Store.Get(ctx, n)
	if errors.Is(err, cfgstore.ErrNotFound) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	loose := s.o.Parse
	loose.FileExists = func(string) bool { return true }
	loose.KeepModels = keptModels(r.Doc) // an older revision may name a path the path rule now refuses
	loose.KeepArgs = keptArgs(r.Doc)
	loose.TrainedCtx = nil
	cfg, err := config.Build(r.Doc, s.o.Boot, loose)
	if err != nil {
		return nil, nil, err
	}
	v := s.viewOf(r.Revision, r.TS, r.Doc, cfg, "")
	return &v, r, nil
}

// Document returns a copy of the current stored document.
func (s *Service) Document() *config.Document {
	s.mu.Lock()
	defer supervise.Unlocked("cfgsvc", s.mu.Unlock)
	return s.doc.Clone()
}

type mutation struct {
	action       string // overrides the derived action (rollback, import)
	requireEmpty bool
	fn           func(d *config.Document) error
	refuse       func(before, after *config.Document, force bool) ([]string, error)
}

func (s *Service) mutate(ctx context.Context, o WriteOpts, m mutation) (*Result, error) {
	res, c, err := s.commit(ctx, o, m)
	if err != nil || c == nil {
		return res, err
	}
	// The callbacks run after the lock is released (callbacksInOrder).
	if err := s.callbacksInOrder(c.ticket, c.cfg, c.ev); err != nil {
		return nil, err
	}
	return res, nil
}

// committed is a change commit made, for its callbacks.
type committed struct {
	ticket uint64
	cfg    *config.Config
	ev     ChangeEvent
}

// commit validates and commits one change under the lock; c is nil when
// nothing was committed (a dry run, or no change).
func (s *Service) commit(ctx context.Context, o WriteOpts, m mutation) (*Result, *committed, error) {
	s.mu.Lock()
	defer supervise.Unlocked("cfgsvc", s.mu.Unlock)
	if s.o.Store == nil {
		return nil, nil, ErrNoStore
	}
	if len(s.unknown) > 0 && !o.Force {
		return nil, nil, &UnknownFieldsError{Fields: append([]string(nil), s.unknown...)}
	}
	before := s.doc.Clone()
	after := s.doc.Clone()
	if err := m.fn(after); err != nil {
		return nil, nil, err
	}
	after.Normalize()
	var warnings []string
	if m.refuse != nil {
		w, err := m.refuse(before, after, o.Force)
		if err != nil {
			return nil, nil, err
		}
		warnings = w
	}
	parse := s.o.Parse
	parse.KeepModels, parse.KeepArgs = keptModels(before), keptArgs(before)
	parse.KeepCtx = keptCtx(before) // a stored ctx above the trained context still loads; a new one is refused
	parse.ModelNote = s.trashNote(ctx) // a file in the trash is named as such (models slice 3)
	cfg, err := config.Build(after, s.o.Boot, parse)
	if err != nil {
		return nil, nil, validationFrom(err)
	}
	chs := sectionChanges(before, after)
	// A forced write while the stored configuration has fields this
	// version does not know writes a new revision even when nothing else
	// changes: dropping them is what forcing is for. Its audit entry names
	// them (section unknown_fields).
	dropUnknown := o.Force && len(s.unknown) > 0
	res := &Result{Revision: s.rev, Changed: len(chs) > 0 || dropUnknown, DryRun: o.DryRun, AuditEntryIDs: []int64{}, Warnings: append([]string{}, warnings...), RestartRequired: []string{}}
	es := entries(chs, m.action)
	if dropUnknown {
		es = append(es, unknownDropEntry(s.unknown))
	}
	for _, e := range es {
		res.Diff = append(res.Diff, e.Diff...)
	}
	if res.Diff == nil {
		res.Diff = []audit.DiffItem{}
	}
	res.RestartRequired = restartKeys(res.Diff)
	res.BlockedByExclusion = append([]config.ExclusionBlock{}, cfg.ExclusionBlocks()...)
	for _, b := range res.BlockedByExclusion {
		res.Warnings = append(res.Warnings, b.Warning())
	}
	res.LoadedOnExcludedNodes = []config.LoadedOnExcluded{}
	if s.o.LoadedOnExcluded != nil {
		res.LoadedOnExcludedNodes = append(res.LoadedOnExcludedNodes, s.o.LoadedOnExcluded(cfg)...)
	}
	for _, l := range res.LoadedOnExcludedNodes {
		res.Warnings = append(res.Warnings, l.Warning())
	}
	if o.IfMatch != nil && *o.IfMatch != s.rev {
		return nil, nil, &cfgstore.ErrStale{Current: s.rev}
	}
	if o.DryRun || (!res.Changed && !m.requireEmpty) { // res.Changed holds dropUnknown
		return res, nil, nil
	}
	if o.Origin.Source == "" {
		o.Origin.Source = audit.SourceREST
	}
	if o.Origin.Actor == "" {
		o.Origin.Actor = audit.DefaultActor
	}
	rev, ids, err := s.o.Store.Commit(ctx, cfgstore.Change{Doc: after, Origin: o.Origin, Entries: es, IfMatch: o.IfMatch, RequireEmpty: m.requireEmpty})
	if err != nil {
		var st *cfgstore.ErrStale
		if errors.As(err, &st) {
			return nil, nil, st
		}
		return nil, nil, err
	}
	cfg.Revision = rev
	s.rev, s.doc = rev, after
	s.unknown = nil                     // the new revision holds only what this version knows
	supervise.Point("config.commit", s) // test hook, under s.mu: the revision is taken, the config not set
	s.updated = time.Now().UnixMilli()
	s.setConfig(cfg)
	res.Revision, res.AuditEntryIDs = rev, ids
	ev := ChangeEvent{Revision: rev, EntryIDs: ids, Source: o.Origin.Source, Actor: o.Origin.Actor}
	if len(es) == 1 {
		ev.Section, ev.Target = es[0].Section, es[0].Target
	} else {
		ev.Section = "multiple"
	}
	ticket := s.cbNext
	s.cbNext++
	return res, &committed{ticket: ticket, cfg: cfg, ev: ev}, nil
}

// callbacksInOrder runs Apply and OnChange for one committed change, after
// those of every earlier change. The caller does not hold mu, so a callback
// may read the service (a callback that writes to it would wait for itself).
//
// A failure there is NOT recovered and continued: the change is already the
// store's truth and only partly applied to the running pieces (fleet client,
// pool, scheduler, API), and the only rebuild that reaches all of them
// together is a start from the store. It ends the process through the fatal
// path (supervise.GuardExit) and the caller answers ErrApplyFailed first.
func (s *Service) callbacksInOrder(ticket uint64, cfg *config.Config, ev ChangeEvent) error {
	func() {
		s.cbMu.Lock()
		defer supervise.Unlocked("cfgsvc", s.cbMu.Unlock)
		if s.cbCond == nil {
			s.cbCond = sync.NewCond(&s.cbMu)
		}
		for s.cbDone != ticket {
			s.cbCond.Wait()
		}
	}()
	defer func() {
		s.cbMu.Lock()
		defer supervise.Unlocked("cfgsvc", s.cbMu.Unlock)
		s.cbDone++
		s.cbCond.Broadcast()
	}()
	if supervise.GuardExit("config.apply", func() {
		supervise.Point("config.apply", s)
		if s.o.Apply != nil {
			s.o.Apply(cfg)
		}
		if s.o.OnChange != nil {
			s.o.OnChange(ev)
		}
	}) {
		return ErrApplyFailed
	}
	return nil
}

func restartKeys(diff []audit.DiffItem) []string {
	need := map[string]bool{}
	for _, d := range diff {
		parts := strings.Split(d.Path, ".")
		if len(parts) < 2 {
			continue
		}
		scope := parts[0]
		if scope != config.ScopeSettings && scope != config.ScopeFleet && scope != config.ScopeRecovery {
			continue
		}
		for _, f := range config.SettingFields(scope) {
			if f.Key == parts[1] && f.RestartRequired {
				need[scope+"."+f.Key] = true
			}
		}
	}
	out := make([]string, 0, len(need))
	for k := range need {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// unknownDropEntry is the audit entry of a forced write that drops the
// fields of the stored configuration this version does not know.
func unknownDropEntry(fields []string) audit.Entry {
	before, _ := json.Marshal(fields)
	e := audit.Entry{Action: audit.ActionUpdate, Section: "unknown_fields", Before: before, After: json.RawMessage("[]")}
	for _, f := range fields {
		e.Diff = append(e.Diff, audit.DiffItem{Path: "unknown_fields." + f, Old: "written by a newer version", New: nil})
	}
	return e
}

// keptModels is the model file each profile of before points at: a write that
// leaves a profile on its file is not refused when that file is missing; one
// that adds a profile or points one at another file is (B4).
// keptArgs maps each profile of before to its extra_args (config
// Options.KeepArgs).
func keptArgs(before *config.Document) map[string][]string {
	m := make(map[string][]string, len(before.Profiles))
	for id, p := range before.Profiles {
		if p != nil && len(p.ExtraArgs) > 0 {
			m[id] = p.ExtraArgs
		}
	}
	return m
}

func keptModels(before *config.Document) map[string]string {
	m := make(map[string]string, len(before.Profiles))
	for id, p := range before.Profiles {
		if p != nil && p.Model != "" {
			m[id] = p.Model
		}
	}
	return m
}

// keptCtx is every stored profile's model and ctx: a write that keeps them
// is not refused for a ctx above the model's trained context.
func keptCtx(before *config.Document) map[string]config.KeptCtx {
	m := make(map[string]config.KeptCtx, len(before.Profiles))
	for id, p := range before.Profiles {
		if p != nil && p.Ctx != nil {
			m[id] = config.KeptCtx{Model: p.Model, Ctx: *p.Ctx}
		}
	}
	return m
}
