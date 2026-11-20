package release

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Same build on both ends of a load (5.1): the scheduler's own host program
// must be built from builds.llama. Its commit is read from `<bin> --version`.

var (
	// llama.cpp prints "version: 6123 (a01665d3)" (stderr), forks may add text
	// around it. The commit is the hex word in the parentheses.
	reVersionParen = regexp.MustCompile(`(?i)version:?\s*[^\s(]*\s*\(\s*([0-9a-f]{7,40})\s*\)`)
	// Some builds print "commit: <hex>" or "build: <n> (<hex>)".
	reCommitWord = regexp.MustCompile(`(?i)\b(?:commit|build)\s*[:=]?\s*(?:\d+\s*)?\(?\s*([0-9a-f]{7,40})\b`)
	// Last resort: a hex word of 7 to 40 characters with at least one letter
	// and one digit, alone in parentheses.
	reParenHex = regexp.MustCompile(`\(\s*([0-9a-f]{7,40})\s*\)`)
)

// ParseLlamaVersion finds the commit in the output of `llama-server --version`.
// It is tolerant: the first line that matches one of the known shapes wins.
func ParseLlamaVersion(out string) (string, bool) {
	out = strings.ToLower(out)
	for i, re := range []*regexp.Regexp{reVersionParen, reCommitWord, reParenHex} {
		for _, line := range strings.Split(out, "\n") {
			if m := re.FindStringSubmatch(line); m != nil && (i < 2 || hasDigitAndLetter(m[1])) {
				return m[1], true
			}
		}
	}
	return "", false
}

func hasDigitAndLetter(s string) bool {
	return strings.ContainsAny(s, "0123456789") && strings.ContainsAny(s, "abcdef")
}

// SameCommit compares two commit ids, full or abbreviated (at least 7 hex
// characters): one must be a prefix of the other.
func SameCommit(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	if len(a) < 7 || len(b) < 7 {
		return false
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	return strings.HasPrefix(b, a)
}

// HostBuild runs bin --version and returns the commit it reports.
func HostBuild(ctx context.Context, bin string) (string, error) {
	f := strings.Fields(bin)
	if len(f) == 0 {
		return "", errors.New("no host program (server_bin) is configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, f[0], append(f[1:], "--version")...)
	out, err := cmd.CombinedOutput()
	if c, ok := ParseLlamaVersion(string(out)); ok {
		return c, nil // some builds exit non-zero after printing the version
	}
	if err != nil {
		return "", fmt.Errorf("%s --version: %v", bin, err)
	}
	return "", fmt.Errorf("%s --version printed no commit: %q", bin, clip(string(out), 200))
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
