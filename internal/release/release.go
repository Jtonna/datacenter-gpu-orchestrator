// Package release loads and verifies the program set Potluck hands out to
// agents (agent contract 5.1 to 5.4): programs/<release>/release.json, one
// folder per platform plus the agent folder, each with a SHA256SUMS file.
//
// Every file named in a folder's SHA256SUMS is hashed when the release is
// loaded; a missing file or a mismatch makes the whole release unusable (it is
// not served). Files that no SHA256SUMS names are ignored.
package release

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// AgentFolder is the folder that holds the agent binaries and the starter.
const AgentFolder = "agent"

// File is one verified file of a folder.
type File struct {
	Name   string
	SHA256 string
	Bytes  int64
	Path   string // on disk
}

// PlatformSpec is one entry of release.json "platforms".
type PlatformSpec struct {
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Backend string `json:"backend"`
}

// Manifest is release.json.
type Manifest struct {
	Release   string                  `json:"release"`
	Builds    map[string]string       `json:"builds"`
	Platforms map[string]PlatformSpec `json:"platforms"`
}

// Release is a loaded and verified release.
type Release struct {
	Manifest
	Dir     string
	Folders map[string]map[string]File // folder name -> file name -> file
}

// Load reads root/<want>/release.json and verifies every folder of the
// release that has a SHA256SUMS. want is the release Potluck serves; the
// manifest must name the same release. Any error means the release must not
// be served.
func Load(root, want string) (*Release, error) {
	if root == "" {
		return nil, errors.New("no programs folder is configured (--programs-dir)")
	}
	if want == "" || strings.ContainsAny(want, `/\`) || want == "." || want == ".." {
		return nil, fmt.Errorf("release %q is not a valid folder name", want)
	}
	dir := filepath.Join(root, want)
	b, err := os.ReadFile(filepath.Join(dir, "release.json"))
	if err != nil {
		return nil, fmt.Errorf("release %s: %w", want, err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("release %s: release.json: %w", want, err)
	}
	if m.Release != want {
		return nil, fmt.Errorf("release %s: release.json names release %q", want, m.Release)
	}
	if strings.TrimSpace(m.Builds["llama"]) == "" {
		return nil, fmt.Errorf("release %s: release.json has no builds.llama", want)
	}
	r := &Release{Manifest: m, Dir: dir, Folders: map[string]map[string]File{}}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("release %s: %w", want, err)
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue // files at the release root are ignored
		}
		sums := filepath.Join(dir, e.Name(), "SHA256SUMS")
		if _, err := os.Stat(sums); err != nil {
			continue
		}
		files, err := verifyFolder(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("release %s: %s: %w", want, e.Name(), err)
		}
		r.Folders[e.Name()] = files
	}
	for name, p := range m.Platforms {
		if p.OS == "" || p.Arch == "" || p.Backend == "" {
			return nil, fmt.Errorf("release %s: platform %s needs os, arch and backend", want, name)
		}
		if name != p.OS+"-"+p.Arch+"-"+p.Backend {
			return nil, fmt.Errorf("release %s: platform %s does not match its os, arch and backend (%s-%s-%s)", want, name, p.OS, p.Arch, p.Backend)
		}
		files, ok := r.Folders[name]
		if !ok {
			return nil, fmt.Errorf("release %s: platform folder %s or its SHA256SUMS is missing", want, name)
		}
		for _, prog := range []string{agentproto.ProgramLlamaServer, agentproto.ProgramRPCServer} {
			if _, ok := files[programFile(name, prog)]; !ok {
				return nil, fmt.Errorf("release %s: platform %s has no %s", want, name, prog)
			}
		}
	}
	return r, nil
}

// programFile is the file name of a program on a platform (".exe" on windows).
func programFile(platform, prog string) string {
	if strings.HasPrefix(platform, "windows-") {
		return prog + ".exe"
	}
	return prog
}

// verifyFolder hashes every file named in dir/SHA256SUMS.
func verifyFolder(dir string) (map[string]File, error) {
	f, err := os.Open(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]File{}
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		sum, name, ok := strings.Cut(text, " ")
		name = strings.TrimLeft(name, " ")
		name = strings.TrimPrefix(name, "*") // sha256sum binary mode marker
		sum = strings.ToLower(sum)
		if !ok || len(sum) != 64 || name == "" {
			return nil, fmt.Errorf("SHA256SUMS line %d is not \"<hex>  <name>\"", line)
		}
		if _, err := hex.DecodeString(sum); err != nil {
			return nil, fmt.Errorf("SHA256SUMS line %d: %q is not hex", line, sum)
		}
		if name != path.Base(name) || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
			return nil, fmt.Errorf("SHA256SUMS line %d: %q is not a plain file name", line, name)
		}
		p := filepath.Join(dir, name)
		got, n, err := hashFile(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if got != sum {
			return nil, fmt.Errorf("%s: sha256 %s does not match SHA256SUMS (%s)", name, got, sum)
		}
		out[name] = File{Name: name, SHA256: sum, Bytes: n, Path: p}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// ChoosePlatform is section 5.2's platform choice: cuda in backends and an
// Nvidia GPU present gives <os>-<arch>-cuda; otherwise vulkan in backends
// gives <os>-<arch>-vulkan; otherwise none. The chosen platform must be in
// the release; there is no fallback to another backend (rule 2.6).
func (r *Release) ChoosePlatform(goos, arch string, backends []string, gpus []agentproto.GPU) (string, bool) {
	has := func(b string) bool {
		for _, x := range backends {
			if x == b {
				return true
			}
		}
		return false
	}
	nvidia := false
	for _, g := range gpus {
		if g.Vendor == agentproto.VendorNvidia {
			nvidia = true
		}
	}
	backend := ""
	switch {
	case has(agentproto.BackendCUDA) && nvidia:
		backend = agentproto.BackendCUDA
	case has(agentproto.BackendVulkan):
		backend = agentproto.BackendVulkan
	default:
		return "", false
	}
	for name, p := range r.Platforms {
		if p.OS == goos && p.Arch == arch && p.Backend == backend {
			return name, true
		}
	}
	return "", false
}

// exporterPrograms are the programs that are exporters, in the order the
// registration answer lists them.
var exporterPrograms = []string{agentproto.ProgramNodeExporter, agentproto.ProgramNvidiaGPUExporter}

// Programs is the program list of a platform for the registration answer:
// every file of the platform folder, with its relative download path and its
// build (builds.llama for the llama programs, builds[<name>] otherwise).
func (r *Release) Programs(platform string) []agentproto.ProgramEntry {
	files := r.Folders[platform]
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	out := []agentproto.ProgramEntry{}
	for _, n := range names {
		f := files[n]
		prog := strings.TrimSuffix(n, ".exe")
		build := r.Builds[prog]
		if prog == agentproto.ProgramLlamaServer || prog == agentproto.ProgramRPCServer {
			build = r.Builds["llama"]
		}
		out = append(out, agentproto.ProgramEntry{Name: prog, Path: ProgramPath(r.Release, platform, n),
			SHA256: f.SHA256, Bytes: f.Bytes, Build: build})
	}
	return out
}

// Exporters is the exporter list of a platform: the exporters the platform
// folder holds, with the arguments of the stats contract (section 2.3).
func (r *Release) Exporters(platform string) []agentproto.ExporterSpec {
	files := r.Folders[platform]
	out := []agentproto.ExporterSpec{}
	for _, prog := range exporterPrograms {
		if _, ok := files[programFile(platform, prog)]; !ok {
			continue
		}
		switch prog {
		case agentproto.ProgramNodeExporter:
			out = append(out, agentproto.ExporterSpec{Name: prog, Args: []string{
				"--web.listen-address=" + agentproto.NodeExporterAddr, "--collector.disable-defaults",
				"--collector.drm", "--collector.hwmon", "--collector.cpu", "--collector.meminfo",
				"--collector.netdev", "--collector.netclass", "--collector.loadavg"}})
		case agentproto.ProgramNvidiaGPUExporter:
			out = append(out, agentproto.ExporterSpec{Name: prog, Args: []string{"--web.listen-address=" + agentproto.NvidiaExporterAddr}})
		}
	}
	return out
}

// ProgramPath is the download path of a file (5.3), relative to Potluck's address.
func ProgramPath(release, folder, file string) string {
	return agentproto.PathProgramsPrefix + release + "/" + folder + "/" + file
}

// Agents is GET /api/agents/release's "agents": every agent binary in the
// agent folder, keyed by "<os>-<arch>" (from potluck-agent-<os>-<arch>[.exe]).
func (r *Release) Agents() map[string]agentproto.AgentBinary {
	out := map[string]agentproto.AgentBinary{}
	for n, f := range r.Folders[AgentFolder] {
		base := strings.TrimSuffix(n, ".exe")
		key, ok := strings.CutPrefix(base, "potluck-agent-")
		if !ok || key == "" {
			continue
		}
		out[key] = agentproto.AgentBinary{Path: ProgramPath(r.Release, AgentFolder, n), SHA256: f.SHA256}
	}
	return out
}

// File returns a verified file for download, or false. Only files named in a
// folder's SHA256SUMS are served.
func (r *Release) File(release, folder, name string) (File, bool) {
	if r == nil || release != r.Release {
		return File{}, false
	}
	f, ok := r.Folders[folder][name]
	return f, ok
}
