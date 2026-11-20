// Package progtree writes a programs folder (agent contract 5.2) for tests:
// programs/<release>/release.json, platform folders and the agent folder,
// each with a SHA256SUMS file.
package progtree

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Tree describes one release.
type Tree struct {
	Release string
	Llama   string                       // builds.llama
	Builds  map[string]string            // other builds (node_exporter, ...)
	Folders map[string]map[string][]byte // folder -> file -> content; a platform folder is "<os>-<arch>-<backend>"
	// Extra files at the release root (ignored by Potluck).
	RootFiles map[string][]byte
}

// Write creates root/<release>/ and returns root.
func Write(tb testing.TB, root string, t Tree) string {
	tb.Helper()
	dir := filepath.Join(root, t.Release)
	must(tb, os.MkdirAll(dir, 0o755))
	builds := map[string]string{"llama": t.Llama}
	for k, v := range t.Builds {
		builds[k] = v
	}
	plats := map[string]map[string]string{}
	for folder, files := range t.Folders {
		fdir := filepath.Join(dir, folder)
		must(tb, os.MkdirAll(fdir, 0o755))
		names := make([]string, 0, len(files))
		for n := range files {
			names = append(names, n)
		}
		sort.Strings(names)
		var sums strings.Builder
		for _, n := range names {
			must(tb, os.WriteFile(filepath.Join(fdir, n), files[n], 0o755))
			s := sha256.Sum256(files[n])
			sums.WriteString(hex.EncodeToString(s[:]) + "  " + n + "\n")
		}
		must(tb, os.WriteFile(filepath.Join(fdir, "SHA256SUMS"), []byte(sums.String()), 0o644))
		if parts := strings.Split(folder, "-"); len(parts) == 3 {
			plats[folder] = map[string]string{"os": parts[0], "arch": parts[1], "backend": parts[2]}
		}
	}
	for n, b := range t.RootFiles {
		must(tb, os.WriteFile(filepath.Join(dir, n), b, 0o644))
	}
	m := map[string]any{"release": t.Release, "builds": builds, "platforms": plats}
	b, _ := json.MarshalIndent(m, "", "  ")
	must(tb, os.WriteFile(filepath.Join(dir, "release.json"), b, 0o644))
	return root
}

func must(tb testing.TB, err error) {
	tb.Helper()
	if err != nil {
		tb.Fatal(err)
	}
}
