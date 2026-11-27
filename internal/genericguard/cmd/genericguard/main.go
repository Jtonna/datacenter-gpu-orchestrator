// Command genericguard checks the files a commit stages (their staged
// content) with the same rules as the whole-tree test in
// internal/genericguard. The pre-commit hook runs it:
//
//	git diff --cached --name-only --diff-filter=ACMR | go run ./internal/genericguard/cmd/genericguard
//
// It prints each hit and exits 1 when there is one, else 0.
package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/genericguard"
)

func staged(path string) ([]byte, error) {
	return exec.Command("git", "show", ":"+path).Output()
}

func main() {
	in, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "genericguard:", err)
		os.Exit(2)
	}
	const allowPath = "internal/genericguard/allow.txt"
	allowFile := filepath.FromSlash(allowPath)
	if b, err := staged(allowPath); err == nil {
		tmp, err := os.CreateTemp("", "genericguard-allow-*.txt")
		if err == nil {
			_, _ = tmp.Write(b)
			tmp.Close()
			defer os.Remove(tmp.Name())
			allowFile = tmp.Name()
		}
	}
	allow, err := genericguard.LoadAllow(allowFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "genericguard:", err)
		os.Exit(2)
	}
	var hits []genericguard.Hit
	for _, p := range genericguard.Lines(in) {
		checked, goStrings := genericguard.Checked(p)
		if !checked {
			continue
		}
		b, err := staged(p)
		if err != nil {
			continue // not in the index (deleted or renamed away)
		}
		hits = append(hits, genericguard.CheckFile(p, b, goStrings, allow)...)
	}
	if len(hits) == 0 {
		return
	}
	fmt.Fprintln(os.Stderr, "pre-commit: Potluck is generic: no brand word and no \"console\" as a machine word in the UI, Go product strings, deploy/ or configs/ (say zone, machine or the legacy fleet feed; a true exception goes in internal/genericguard/allow.txt):")
	for _, h := range hits {
		fmt.Fprintln(os.Stderr, "  "+h.String())
	}
	os.Exit(1)
}
