// Package cfgsvc is the one implementation of configuration changes: REST
// handlers and MCP tools both call it. Every change is validated against the
// whole resulting configuration, committed as a new revision with audit
// entries in one transaction, then handed to the scheduler as a snapshot.
package cfgsvc

import (
	"context"
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
)

// RefError means a delete is refused because benchmarks refer to the profile.
type RefError struct {
	ID       string
	Warnings []string
}

func (e *RefError) Error() string {
	return fmt.Sprintf("profile %q is referenced; repeat with force to delete it anyway: %s", e.ID, strings.Join(e.Warnings, "; "))
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
	Logger      *slog.Logger
}

// Service holds the current configuration.
type Service struct {
	o   Options
	log *slog.Logger

	mu      sync.Mutex // serializes writes
	rev     int64
	updated int64
	doc     *config.Document
	cfgMu   sync.RWMutex
	cfg     *config.Config
	loadErr string
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
	cfg, err := config.Build(r.Doc, o.Boot, o.Parse)
	if err != nil {
		// A model file that went missing must not stop the scheduler: build without the file check.
		loose := o.Parse
		loose.FileExists = func(string) bool { return true }
		var err2 error
		if cfg, err2 = config.Build(r.Doc, o.Boot, loose); err2 != nil {
			s.loadErr = "the stored configuration (revision " + fmt.Sprint(r.Revision) + ") is invalid: " + err2.Error()
			s.log.Error("stored configuration is invalid; running with built-in defaults", "revision", r.Revision, "err", err2)
			s.setConfig(mustBuild(config.NewDocument(), o.Boot, o.Parse))
			return s
		}
		s.log.Warn("stored configuration has problems that only the file check finds", "err", err)
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
	s.cfgMu.Lock()
	s.cfg = c
	s.cfgMu.Unlock()
}

// Config is the current snapshot.
func (s *Service) Config() *config.Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg
}

// LoadError is non-empty when the stored configuration could not be used.
func (s *Service) LoadError() string { return s.loadErr }

// Revision is the current revision (0 when nothing is configured).
func (s *Service) Revision() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	s.mu.Lock()
	rev, upd, doc := s.rev, s.updated, s.doc.Clone()
	s.mu.Unlock()
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
	defer s.mu.Unlock()
	return s.doc.Clone()
}

type mutation struct {
	action       string // overrides the derived action (rollback, import)
	requireEmpty bool
	fn           func(d *config.Document) error
	refuse       func(before, after *config.Document, force bool) ([]string, error)
}

func (s *Service) mutate(ctx context.Context, o WriteOpts, m mutation) (*Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.o.Store == nil {
		return nil, ErrNoStore
	}
	before := s.doc.Clone()
	after := s.doc.Clone()
	if err := m.fn(after); err != nil {
		return nil, err
	}
	after.Normalize()
	var warnings []string
	if m.refuse != nil {
		w, err := m.refuse(before, after, o.Force)
		if err != nil {
			return nil, err
		}
		warnings = w
	}
	cfg, err := config.Build(after, s.o.Boot, s.o.Parse)
	if err != nil {
		return nil, validationFrom(err)
	}
	chs := sectionChanges(before, after)
	res := &Result{Revision: s.rev, Changed: len(chs) > 0, DryRun: o.DryRun, AuditEntryIDs: []int64{}, Warnings: append([]string{}, warnings...), RestartRequired: []string{}}
	es := entries(chs, m.action)
	for _, e := range es {
		res.Diff = append(res.Diff, e.Diff...)
	}
	if res.Diff == nil {
		res.Diff = []audit.DiffItem{}
	}
	res.RestartRequired = restartKeys(res.Diff)
	if o.IfMatch != nil && *o.IfMatch != s.rev {
		return nil, &cfgstore.ErrStale{Current: s.rev}
	}
	if o.DryRun || (!res.Changed && !m.requireEmpty) {
		return res, nil
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
			return nil, st
		}
		return nil, err
	}
	cfg.Revision = rev
	s.rev, s.doc = rev, after
	s.updated = time.Now().UnixMilli()
	s.setConfig(cfg)
	res.Revision, res.AuditEntryIDs = rev, ids
	if s.o.Apply != nil {
		s.o.Apply(cfg)
	}
	if s.o.OnChange != nil {
		ev := ChangeEvent{Revision: rev, EntryIDs: ids, Source: o.Origin.Source, Actor: o.Origin.Actor}
		if len(es) == 1 {
			ev.Section, ev.Target = es[0].Section, es[0].Target
		} else {
			ev.Section = "multiple"
		}
		s.o.OnChange(ev)
	}
	return res, nil
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
