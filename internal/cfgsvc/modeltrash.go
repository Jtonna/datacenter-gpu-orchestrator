package cfgsvc

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

// Models slice 3: deleting a model file takes every profile that names it out
// of the configuration in one revision (their specs are kept with the trash
// record), and restoring it adds them back in one revision.

// ProfileNamesFile reports whether a profile (kind llama) names the model
// file name in the models folder.
func ProfileNamesFile(p *config.ProfileSpec, file string) bool {
	if p == nil || p.Kind == "command" || p.Model == "" {
		return false
	}
	return filepath.Clean(filepath.FromSlash(p.Model)) == file
}

// ProfilesOfFile are the profiles of d that name file, as copies.
func ProfilesOfFile(d *config.Document, file string) map[string]*config.ProfileSpec {
	out := map[string]*config.ProfileSpec{}
	for id, p := range d.Profiles {
		if ProfileNamesFile(p, file) {
			c := *p
			out[id] = &c
		}
	}
	return out
}

// RemoveProfiles takes the profiles ids out in one revision. o.IfMatch
// should be the revision the caller read them at, so nothing else changed
// meanwhile. Benchmarks that refer to them are warnings, not a refusal (the
// design: a model delete lists them).
func (s *Service) RemoveProfiles(ctx context.Context, o WriteOpts, ids []string) (*Result, error) {
	return s.mutate(ctx, o, mutation{
		fn: func(d *config.Document) error {
			for _, id := range ids {
				if _, ok := d.Profiles[id]; !ok {
					return fmt.Errorf("profile %q: %w", id, ErrNotFound)
				}
				delete(d.Profiles, id)
			}
			return nil
		},
		refuse: func(before, after *config.Document, force bool) ([]string, error) {
			var w []string
			if s.o.ProfileRefs != nil {
				for _, id := range sortedKeys(ids) {
					for _, r := range s.o.ProfileRefs(id) {
						w = append(w, fmt.Sprintf("benchmark %s refers to profile %s", r, id))
					}
				}
			}
			return w, nil
		},
	})
}

// AddProfiles adds the profiles in one revision; ErrExists when an id is
// taken (the caller checks ids and aliases first and renames on request).
func (s *Service) AddProfiles(ctx context.Context, o WriteOpts, ps map[string]*config.ProfileSpec) (*Result, error) {
	return s.addProfiles(ctx, o, ps, "")
}

// AddStoredProfiles adds back, in one revision, profiles exactly as they
// were stored (a model restored from the trash, a delete undone): what was
// valid as a stored profile when it was taken out is valid now (stored.go);
// what a new write would refuse comes back as Result.Warnings, naming them
// as what ("restored profiles"). Only the models trash calls it, with the
// specs its record kept; no API writes through it.
func (s *Service) AddStoredProfiles(ctx context.Context, o WriteOpts, ps map[string]*config.ProfileSpec, what string) (*Result, error) {
	if what == "" {
		what = "profiles put back"
	}
	return s.addProfiles(ctx, o, ps, what)
}

func (s *Service) addProfiles(ctx context.Context, o WriteOpts, ps map[string]*config.ProfileSpec, stored string) (*Result, error) {
	m := mutation{fn: func(d *config.Document) error {
		for _, id := range sortedKeys(keysOf(ps)) {
			if _, ok := d.Profiles[id]; ok {
				return fmt.Errorf("profile %q: %w", id, ErrExists)
			}
			c := *ps[id]
			d.Profiles[id] = &c
		}
		return nil
	}}
	if stored != "" {
		m.stored, m.storedWhat = ps, stored
	}
	return s.mutate(ctx, o, m)
}

func keysOf(m map[string]*config.ProfileSpec) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func sortedKeys(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

// trashNote names the model files in the trash for the refusal of a write
// that needs one ("file ... not found under models_dir ...: it is in the
// trash since ..."): a rollback to a revision from before a delete is
// refused with the file named, until it is restored.
func (s *Service) trashNote(ctx context.Context) func(string) string {
	if s.o.Store == nil {
		return nil
	}
	recs, err := s.o.Store.TrashList(ctx)
	if err != nil || len(recs) == 0 {
		return nil
	}
	by := map[string]*cfgstore.TrashRecord{}
	for _, r := range recs {
		if r.State == cfgstore.TrashTrashed || r.State == cfgstore.TrashPending || r.State == cfgstore.TrashInterrupted {
			if o := by[r.File]; o == nil || r.DeletedAt > o.DeletedAt {
				by[r.File] = r
			}
		}
	}
	return func(model string) string {
		r := by[filepath.Clean(filepath.FromSlash(model))]
		if r == nil {
			return ""
		}
		return fmt.Sprintf("it is in the trash since %s (trash id %s); restore it first (POST /api/models/trash/%s/restore)",
			time.UnixMilli(r.DeletedAt).UTC().Format("2 Jan 2006 15:04 UTC"), r.ID, r.ID)
	}
}
