package api

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

// A profile is refused a model path inside a dot-folder of the models folder
// (internal/modelpath), but a profile stored before that rule may still name
// one, or name a model file by a link. These say which profiles reach a file
// whatever the path they use, so a delete and a removal for good never miss
// one.

// profileOnFile reports whether profile p (kind llama) names a file that
// resolves to target, the file in the models folder at root.
func profileOnFile(root string, p *config.ProfileSpec, target os.FileInfo) bool {
	if root == "" || target == nil || p == nil || p.Kind == "command" || p.Model == "" {
		return false
	}
	st, err := os.Stat(filepath.Join(root, filepath.FromSlash(p.Model)))
	return err == nil && os.SameFile(st, target)
}

// withProfilesOnFile is profiles plus every other profile of the current
// configuration whose model resolves to file (slice 3's delete names those
// by name; a link or another path to the same file counts too). profiles is
// not changed.
func (s *Server) withProfilesOnFile(file string, profiles map[string]*config.ProfileSpec) map[string]*config.ProfileSpec {
	root := s.modelDir().Root
	if root == "" || s.opts.Config == nil {
		return profiles
	}
	target, err := os.Stat(filepath.Join(root, file))
	if err != nil {
		return profiles
	}
	doc, _ := s.docAt()
	out := profiles
	for id, p := range doc.Profiles {
		if _, ok := profiles[id]; ok || !profileOnFile(root, p, target) {
			continue
		}
		if len(out) == len(profiles) { // copy on the first addition
			out = make(map[string]*config.ProfileSpec, len(profiles)+1)
			for k, v := range profiles {
				out[k] = v
			}
		}
		out[id] = p
	}
	return out
}

// profilesInTrashFolder are the profiles of the current configuration whose
// model, cleaned, is inside .trash/<id>/, or resolves to the trashed file
// there: while there is one, the file is not removed for good (models slice
// 4, 409 in_use_by_profile), by the owner or by the clearing. Sorted.
func (s *Server) profilesInTrashFolder(id, file string) []string {
	if s.opts.Config == nil {
		return nil
	}
	root := s.modelDir().Root
	var target os.FileInfo
	if root != "" {
		target, _ = os.Stat(filepath.Join(root, ".trash", id, file))
	}
	folder := ".trash/" + id
	doc, _ := s.docAt()
	var out []string
	for pid, p := range doc.Profiles {
		if p == nil || p.Kind == "command" || p.Model == "" {
			continue
		}
		clean := path.Clean(strings.ReplaceAll(p.Model, `\`, "/"))
		if clean == folder || strings.HasPrefix(clean, folder+"/") || profileOnFile(root, p, target) {
			out = append(out, pid)
		}
	}
	sort.Strings(out)
	return out
}
