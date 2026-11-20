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
		d.Profiles[id] = p
		return nil
	}})
}

// UpdateProfile replaces a profile; ErrNotFound when it does not exist.
func (s *Service) UpdateProfile(ctx context.Context, o WriteOpts, id string, p *config.ProfileSpec) (*Result, error) {
	return s.mutate(ctx, o, mutation{fn: func(d *config.Document) error {
		if _, ok := d.Profiles[id]; !ok {
			return fmt.Errorf("profile %q: %w", id, ErrNotFound)
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
	return s.mutate(ctx, o, mutation{action: audit.ActionRollback, fn: func(d *config.Document) error {
		*d = *r.Doc.Clone()
		return nil
	}})
}

// Import replaces the configuration with doc. Without replace it is refused
// when the database already holds a configuration.
func (s *Service) Import(ctx context.Context, o WriteOpts, doc *config.Document, replace bool) (*Result, error) {
	return s.mutate(ctx, o, mutation{action: audit.ActionImport, requireEmpty: !replace, fn: func(d *config.Document) error {
		*d = *doc.Clone()
		return nil
	}})
}
