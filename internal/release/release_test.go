package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/progtree"
)

const llama = "a01665d30de2885879a64eae999067ef3ef5fd4b"

func tree() progtree.Tree {
	return progtree.Tree{Release: "0.1.0", Llama: llama, Builds: map[string]string{"node_exporter": "1.13.1", "nvidia_gpu_exporter": "2.0.0"},
		Folders: map[string]map[string][]byte{
			"agent":              {"potluck-agent-linux-amd64": []byte("AGENT"), "potluck-starter.sh": []byte("#!/bin/sh\n")},
			"linux-amd64-vulkan": {"llama-server": []byte("LS-V"), "rpc-server": []byte("RPC-V"), "node_exporter": []byte("NE")},
			"linux-amd64-cuda":   {"llama-server": []byte("LS-C"), "rpc-server": []byte("RPC-C"), "node_exporter": []byte("NE"), "nvidia_gpu_exporter": []byte("NV")},
		},
		RootFiles: map[string][]byte{"LICENSE.txt": []byte("licence"), "NOTES": []byte("notes")}}
}

func TestLoadVerifiesEveryFolder(t *testing.T) {
	root := progtree.Write(t, t.TempDir(), tree())
	r, err := Load(root, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Folders) != 3 || r.Builds["llama"] != llama {
		t.Fatalf("folders %v builds %v", r.Folders, r.Builds)
	}
	ps := r.Programs("linux-amd64-cuda")
	if len(ps) != 4 {
		t.Fatalf("programs %+v", ps)
	}
	for _, p := range ps {
		if !strings.HasPrefix(p.Path, "/api/pool/programs/0.1.0/linux-amd64-cuda/") || len(p.SHA256) != 64 || p.Bytes == 0 {
			t.Fatalf("entry %+v", p)
		}
		switch p.Name {
		case "llama-server", "rpc-server":
			if p.Build != llama {
				t.Fatalf("llama build %+v", p)
			}
		case "node_exporter":
			if p.Build != "1.13.1" {
				t.Fatalf("exporter build %+v", p)
			}
		}
	}
	ex := r.Exporters("linux-amd64-cuda")
	if len(ex) != 2 || ex[0].Name != "node_exporter" || ex[0].Args[0] != "--web.listen-address=127.0.0.1:50070" ||
		ex[1].Args[0] != "--web.listen-address=127.0.0.1:50071" {
		t.Fatalf("exporters %+v", ex)
	}
	if ex := r.Exporters("linux-amd64-vulkan"); len(ex) != 1 {
		t.Fatalf("vulkan exporters %+v", ex)
	}
	ag := r.Agents()
	if a, ok := ag["linux-amd64"]; !ok || a.Path != "/api/pool/programs/0.1.0/agent/potluck-agent-linux-amd64" || len(ag) != 1 {
		t.Fatalf("agents %+v", ag)
	}
	if _, ok := r.File("0.1.0", "linux-amd64-vulkan", "rpc-server"); !ok {
		t.Fatal("rpc-server not served")
	}
	if _, ok := r.File("0.1.0", "linux-amd64-vulkan", "SHA256SUMS"); ok {
		t.Fatal("a file no SHA256SUMS names is served")
	}
	if _, ok := r.File("0.2.0", "linux-amd64-vulkan", "rpc-server"); ok {
		t.Fatal("another release is served")
	}
}

func TestLoadRefusesMismatchAndMissingFile(t *testing.T) {
	root := progtree.Write(t, t.TempDir(), tree())
	p := filepath.Join(root, "0.1.0", "linux-amd64-vulkan", "rpc-server")
	os.WriteFile(p, []byte("tampered"), 0o755)
	if _, err := Load(root, "0.1.0"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatch: %v", err)
	}
	root = progtree.Write(t, t.TempDir(), tree())
	os.Remove(filepath.Join(root, "0.1.0", "agent", "potluck-starter.sh"))
	if _, err := Load(root, "0.1.0"); err == nil || !strings.Contains(err.Error(), "potluck-starter.sh") {
		t.Fatalf("missing: %v", err)
	}
}

func TestLoadRefusesBadManifest(t *testing.T) {
	if _, err := Load("", "0.1.0"); err == nil {
		t.Fatal("no folder accepted")
	}
	if _, err := Load(t.TempDir(), "0.1.0"); err == nil {
		t.Fatal("missing release accepted")
	}
	tr := tree()
	root := progtree.Write(t, t.TempDir(), tr)
	if _, err := Load(root, "0.2.0"); err == nil {
		t.Fatal("other release accepted")
	}
	// A platform named in release.json whose folder has no rpc-server.
	tr = tree()
	delete(tr.Folders["linux-amd64-vulkan"], "rpc-server")
	root = progtree.Write(t, t.TempDir(), tr)
	if _, err := Load(root, "0.1.0"); err == nil || !strings.Contains(err.Error(), "rpc-server") {
		t.Fatalf("platform without rpc-server: %v", err)
	}
	// release.json names another release.
	root = progtree.Write(t, t.TempDir(), tree())
	os.WriteFile(filepath.Join(root, "0.1.0", "release.json"), []byte(`{"release":"0.0.9","builds":{"llama":"abc1234"}}`), 0o644)
	if _, err := Load(root, "0.1.0"); err == nil {
		t.Fatal("manifest of another release accepted")
	}
}

func TestChoosePlatform(t *testing.T) {
	root := progtree.Write(t, t.TempDir(), tree())
	r, err := Load(root, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	nv := []agentproto.GPU{{Vendor: "nvidia", Model: "RTX 5060"}}
	amd := []agentproto.GPU{{Vendor: "amd", Integrated: true}}
	cases := []struct {
		os, arch string
		backends []string
		gpus     []agentproto.GPU
		want     string
	}{
		{"linux", "amd64", []string{"cuda", "vulkan"}, nv, "linux-amd64-cuda"},
		{"linux", "amd64", []string{"cuda", "vulkan"}, amd, "linux-amd64-vulkan"}, // cuda without an Nvidia GPU
		{"linux", "amd64", []string{"vulkan"}, amd, "linux-amd64-vulkan"},
		{"linux", "amd64", nil, amd, ""},
		{"linux", "arm64", []string{"vulkan"}, amd, ""},
		{"windows", "amd64", []string{"cuda"}, nv, ""},
	}
	for _, c := range cases {
		got, ok := r.ChoosePlatform(c.os, c.arch, c.backends, c.gpus)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("%v: got %q %v want %q", c, got, ok, c.want)
		}
	}
}

func TestParseLlamaVersion(t *testing.T) {
	cases := map[string]string{
		"version: 6123 (a01665d3)\nbuilt with cc (Ubuntu 13.3.0) 13.3.0 for x86_64-linux-gnu\n": "a01665d3",
		"ggml_vulkan: Found 1 Vulkan devices\nversion: 7001 (A01665D30DE2)\nbuilt with clang\n": "a01665d30de2",
		"llama-server build: 6123 (a01665d30)":                                                  "a01665d30",
		"commit: a01665d30de2885879a64eae999067ef3ef5fd4b":                                      "a01665d30de2885879a64eae999067ef3ef5fd4b",
		"some fork v1.2 (deadbee1)":                                                             "deadbee1",
	}
	for in, want := range cases {
		got, ok := ParseLlamaVersion(in)
		if !ok || got != want {
			t.Errorf("%q: got %q %v want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "error: unknown argument --version", "version: 6123 (unknown)", "(1234)"} {
		if got, ok := ParseLlamaVersion(in); ok {
			t.Errorf("%q parsed as %q", in, got)
		}
	}
}

func TestSameCommit(t *testing.T) {
	if !SameCommit("a01665d3", llama) || !SameCommit(llama, "A01665D30") || !SameCommit(llama, llama) {
		t.Fatal("prefix of the full commit is the same commit")
	}
	if SameCommit("a01665e3", llama) || SameCommit("a016", llama) || SameCommit("", "") {
		t.Fatal("different or too short commits match")
	}
}
