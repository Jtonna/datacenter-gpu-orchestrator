package cfgsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

// ParseProfile decodes a profile body strictly (unknown fields are errors).
func ParseProfile(b []byte) (*config.ProfileSpec, error) {
	var p config.ProfileSpec
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, &ValidationError{Messages: []string{"profile: " + err.Error()}}
	}
	return &p, nil
}

// CreateProfile adds a profile; ErrExists when the id is taken.
func (s *Service) CreateProfile(ctx context.Context, o WriteOpts, id string, p *config.ProfileSpec) (*Result, error) {
	if id == "" {
		return nil, &ValidationError{Messages: []string{"profile id must not be empty"}}
	}
	return s.mutate(ctx, o, mutation{fn: func(d *config.Document) error {
		if _, ok := d.Profiles[id]; ok {
			return fmt.Errorf("profile %q: %w", id, ErrExists)
		}
		if err := keepMarks(id, nil, p); err != nil {
			return err
		}
		d.Profiles[id] = p
		return nil
	}})
}

// keepMarks: stage and display_name are written through create and update
// (profiles plan, slice B2). A write that leaves both out keeps the stored
// ones, so a client that does not know them never wipes them; one that
// sets either writes both (validated with the configuration).
func keepMarks(id string, old, p *config.ProfileSpec) error {
	if old != nil && p.Stage == "" && p.DisplayName == "" {
		p.Stage, p.DisplayName = old.Stage, old.DisplayName
	}
	return nil
}

// SetStage is promote (stage production) and demote (stage test): one
// revision audited as promote or demote; nothing is written when the
// profile already has that stage, by mark or by rule (config.EffectiveStage).
func (s *Service) SetStage(ctx context.Context, o WriteOpts, id, stage string) (*Result, error) {
	action := audit.ActionPromote
	switch stage {
	case config.StageProduction:
	case config.StageTest:
		action = audit.ActionDemote
	default:
		return nil, &ValidationError{Messages: []string{fmt.Sprintf("stage must be %s or %s, got %q", config.StageProduction, config.StageTest, stage)}}
	}
	if p := s.Config().Profiles[id]; p != nil {
		if st, _ := config.EffectiveStage(p); st == stage {
			return &Result{Revision: s.Revision(), AuditEntryIDs: []int64{}, Diff: []audit.DiffItem{}, Warnings: []string{}, RestartRequired: []string{}}, nil
		}
	}
	return s.mutate(ctx, o, mutation{action: action, fn: func(d *config.Document) error {
		p, ok := d.Profiles[id]
		if !ok {
			return fmt.Errorf("profile %q: %w", id, ErrNotFound)
		}
		cp := *p
		cp.Stage = stage
		d.Profiles[id] = &cp
		return nil
	}})
}

// UpdateProfile replaces a profile; ErrNotFound when it does not exist.
func (s *Service) UpdateProfile(ctx context.Context, o WriteOpts, id string, p *config.ProfileSpec) (*Result, error) {
	return s.mutate(ctx, o, mutation{fn: func(d *config.Document) error {
		old, ok := d.Profiles[id]
		if !ok {
			return fmt.Errorf("profile %q: %w", id, ErrNotFound)
		}
		if err := keepMarks(id, old, p); err != nil {
			return err
		}
		d.Profiles[id] = p
		return nil
	}})
}

// DeleteProfile removes a profile. Benchmarks that still refer to it are
// reported as warnings and the delete is refused unless o.Force.
func (s *Service) DeleteProfile(ctx context.Context, o WriteOpts, id string) (*Result, error) {
	return s.mutate(ctx, o, mutation{
		fn: func(d *config.Document) error {
			if _, ok := d.Profiles[id]; !ok {
				return fmt.Errorf("profile %q: %w", id, ErrNotFound)
			}
			delete(d.Profiles, id)
			return nil
		},
		refuse: func(before, after *config.Document, force bool) ([]string, error) {
			if s.o.ProfileRefs == nil {
				return nil, nil
			}
			refs := s.o.ProfileRefs(id)
			if len(refs) == 0 {
				return nil, nil
			}
			var w []string
			for _, r := range refs {
				w = append(w, fmt.Sprintf("benchmark %s refers to profile %s", r, id))
			}
			if !force && !o.DryRun {
				return nil, &RefError{ID: id, Warnings: w}
			}
			return w, nil
		},
	})
}

// SetTiers replaces the ordered tier list.
func (s *Service) SetTiers(ctx context.Context, o WriteOpts, tiers []config.TierSpec) (*Result, error) {
	return s.mutate(ctx, o, mutation{fn: func(d *config.Document) error {
		if tiers == nil {
			tiers = []config.TierSpec{}
		}
		d.Tiers = tiers
		return nil
	}})
}

// SetExcludeNodes replaces exclude_nodes.
func (s *Service) SetExcludeNodes(ctx context.Context, o WriteOpts, list []string) (*Result, error) {
	return s.mutate(ctx, o, mutation{fn: func(d *config.Document) error {
		if list == nil {
			list = []string{}
		}
		d.ExcludeNodes = list
		return nil
	}})
}

// SetNoAutoNodes replaces no_auto_nodes (CR-9 review).
func (s *Service) SetNoAutoNodes(ctx context.Context, o WriteOpts, list []string) (*Result, error) {
	return s.mutate(ctx, o, mutation{fn: func(d *config.Document) error {
		if list == nil {
			list = []string{}
		}
		d.NoAutoNodes = list
		return nil
	}})
}

// SetNodeOverrides replaces node_overrides.
func (s *Service) SetNodeOverrides(ctx context.Context, o WriteOpts, m map[string]config.NodeOverrideSpec) (*Result, error) {
	return s.mutate(ctx, o, mutation{fn: func(d *config.Document) error {
		if m == nil {
			m = map[string]config.NodeOverrideSpec{}
		}
		d.NodeOverrides = m
		return nil
	}})
}

// PatchSettings sets keys of the settings scope; a nil value resets a key to its default.
func (s *Service) PatchSettings(ctx context.Context, o WriteOpts, patch map[string]any) (*Result, error) {
	return s.patchScope(ctx, o, config.ScopeSettings, patch)
}

// PatchRecovery changes keys of the recovery section (nil resets).
func (s *Service) PatchRecovery(ctx context.Context, o WriteOpts, patch map[string]any) (*Result, error) {
	return s.patchScope(ctx, o, config.ScopeRecovery, patch)
}

// PatchFleet changes keys of the fleet section (nil resets).
func (s *Service) PatchFleet(ctx context.Context, o WriteOpts, patch map[string]any) (*Result, error) {
	return s.patchScope(ctx, o, config.ScopeFleet, patch)
}

// ReplaceRecovery is the PUT form: keys absent from the object return to their defaults.
func (s *Service) ReplaceRecovery(ctx context.Context, o WriteOpts, obj map[string]any) (*Result, error) {
	return s.replaceScope(ctx, o, config.ScopeRecovery, obj)
}

// ReplaceFleet is the PUT form of the fleet section.
func (s *Service) ReplaceFleet(ctx context.Context, o WriteOpts, obj map[string]any) (*Result, error) {
	return s.replaceScope(ctx, o, config.ScopeFleet, obj)
}

func scopeMap(d *config.Document, scope string) map[string]any {
	switch scope {
	case config.ScopeRecovery:
		return d.Recovery
	case config.ScopeFleet:
		return d.Fleet
	}
	return d.Settings
}

func (s *Service) patchScope(ctx context.Context, o WriteOpts, scope string, patch map[string]any) (*Result, error) {
	return s.mutate(ctx, o, mutation{fn: func(d *config.Document) error {
		m := scopeMap(d, scope)
		var msgs []string
		keys := make([]string, 0, len(patch))
		for k := range patch {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if patch[k] == nil {
				if _, ok := config.SettingFieldByKey(scope, k); !ok {
					msgs = append(msgs, fmt.Sprintf("unknown %s key %q", scope, k))
				}
				delete(m, k)
				continue
			}
			v, err := config.CoerceSetting(scope, k, patch[k])
			if err != nil {
				msgs = append(msgs, err.Error())
				continue
			}
			m[k] = v
		}
		if len(msgs) > 0 {
			return &ValidationError{Messages: msgs}
		}
		return nil
	}})
}

func (s *Service) replaceScope(ctx context.Context, o WriteOpts, scope string, obj map[string]any) (*Result, error) {
	return s.mutate(ctx, o, mutation{fn: func(d *config.Document) error {
		m := map[string]any{}
		var msgs []string
		for k, raw := range obj {
			if raw == nil {
				continue
			}
			v, err := config.CoerceSetting(scope, k, raw)
			if err != nil {
				msgs = append(msgs, err.Error())
				continue
			}
			m[k] = v
		}
		if len(msgs) > 0 {
			sort.Strings(msgs)
			return &ValidationError{Messages: msgs}
		}
		switch scope {
		case config.ScopeRecovery:
			d.Recovery = m
		case config.ScopeFleet:
			d.Fleet = m
		}
		return nil
	}})
}

// Rollback creates a new revision equal to revision n.
func (s *Service) Rollback(ctx context.Context, o WriteOpts, n int64) (*Result, error) {
	if s.o.Store == nil {
		return nil, ErrNoStore
	}
	r, err := s.o.Store.Get(ctx, n)
	if err != nil {
		return nil, ErrNotFound
	}
	// a stored revision is restorable: its profiles are checked as stored
	// ones (stored.go); a file it names must be there (a rollback to a
	// revision from before a model delete is refused until it is restored)
	return s.mutate(ctx, o, mutation{action: audit.ActionRollback, fn: func(d *config.Document) error {
		*d = *r.Doc.Clone()
		return nil
	}, stored: r.Doc.Clone().Profiles, storedWhat: fmt.Sprintf("profiles of revision %d", n)})
}

// Import replaces the configuration with doc. Without replace it is refused
// when the database already holds a configuration.
func (s *Service) Import(ctx context.Context, o WriteOpts, doc *config.Document, replace bool) (*Result, error) {
	return s.mutate(ctx, o, mutation{action: audit.ActionImport, requireEmpty: !replace, fn: func(d *config.Document) error {
		*d = *doc.Clone()
		return nil
	}})
}

// NewProfile is one profile CreateMissing writes when its id is free.
type NewProfile struct {
	ID   string
	Spec *config.ProfileSpec
}

// CreateMissing writes, in ONE revision, each profile whose id is neither a
// profile id nor an alias; the others are skipped and never edited (the
// standard set, profiles plan section 6). written and skipped are the ids,
// in the order given; nothing is committed when every id is taken.
func (s *Service) CreateMissing(ctx context.Context, o WriteOpts, ps []NewProfile) (res *Result, written, skipped []string, err error) {
	res, err = s.mutate(ctx, o, mutation{fn: func(d *config.Document) error {
		written, skipped = nil, nil
		taken := map[string]bool{}
		for id, p := range d.Profiles {
			taken[id] = true
			if p != nil {
				for _, a := range p.Aliases {
					taken[a] = true
				}
			}
		}
		for _, np := range ps {
			if np.ID == "" || np.Spec == nil {
				return &ValidationError{Messages: []string{"profile id must not be empty"}}
			}
			if taken[np.ID] {
				skipped = append(skipped, np.ID)
				continue
			}
			taken[np.ID] = true
			d.Profiles[np.ID] = np.Spec
			written = append(written, np.ID)
		}
		return nil
	}})
	if err != nil {
		return nil, nil, nil, err
	}
	return res, written, skipped, nil
}
