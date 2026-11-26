package llamaflags

import (
	"slices"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		args       []string
		ck, cache  int
		hasCk, has bool
		problem    string
	}{
		{args: nil},
		{args: []string{"--temp", "0.7"}},
		{args: []string{"--ctx-checkpoints", "3"}, ck: 3, hasCk: true},
		{args: []string{"-ctxcp", "0"}, hasCk: true},
		{args: []string{"--swa-checkpoints", "1"}, ck: 1, hasCk: true},
		{args: []string{"--ctx_checkpoints", "2"}, ck: 2, hasCk: true},
		{args: []string{"--cache-ram", "0"}, has: true},
		{args: []string{"-cram", "512", "--jinja"}, cache: 512, has: true},
		{args: []string{"--cache_ram", "256"}, cache: 256, has: true},
		{args: []string{"--ctx-checkpoints", "3", "--cache-ram", "0"}, ck: 3, hasCk: true, has: true},
		{args: []string{"--ctx-checkpoints=3"}, problem: "does not read --ctx-checkpoints=value"},
		{args: []string{"--cache-ram"}, problem: "has no value after it"},
		{args: []string{"--cache-ram", "lots"}, problem: "must be a whole number"},
		{args: []string{"--cache-ram", "-1"}, problem: "must not be negative"},
		{args: []string{"-ctxcp", "2000"}, problem: "at most 1024"},
		{args: []string{"--ctx-checkpoints", "3", "-ctxcp", "1"}, problem: "is given twice (--ctx-checkpoints and -ctxcp)"},
		{args: []string{"--cache-ram", "3.5"}, problem: "must be a whole number"},
	}
	for _, c := range cases {
		h, p := Parse(c.args)
		if h.CtxCheckpoints != c.ck || h.HasCtxCheckpoints != c.hasCk || h.CacheRAMMiB != c.cache || h.HasCacheRAM != c.has {
			t.Errorf("%q: %+v", c.args, h)
		}
		if c.problem == "" && len(p) != 0 || c.problem != "" && (len(p) != 1 || !strings.Contains(p[0], c.problem)) {
			t.Errorf("%q: problems %q, want %q", c.args, p, c.problem)
		}
	}
}

func TestStrip(t *testing.T) {
	for in, want := range map[string]string{
		"--jinja --ctx-checkpoints 3 --temp 0.7": "--jinja --temp 0.7",
		"-ctxcp 1 -cram 0":                       "",
		"--cache-ram=0 --top-k 5":                "--top-k 5",
		"--cache-ram -1 --top-k 5":               "--top-k 5",
		"--cache_ram 9":                          "",
		"--checkpoint-min-step 64":               "--checkpoint-min-step 64",
		"--ctx-size-draft 64":                    "--ctx-size-draft 64",
	} {
		if got := strings.Join(Strip(strings.Fields(in)), " "); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	if !Has([]string{"-cram", "0"}) || Has([]string{"--jinja"}) {
		t.Fatal("Has")
	}
	if !slices.Equal(Spellings(CtxCheckpoints), []string{"-ctxcp", "--ctx-checkpoints", "--swa-checkpoints"}) || !slices.Equal(Spellings(CacheRAM), []string{"-cram", "--cache-ram"}) {
		t.Fatal("spellings")
	}
}
