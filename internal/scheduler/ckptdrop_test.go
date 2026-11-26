package scheduler

import (
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// A stored profile whose own extra_args set --ctx-checkpoints has that flag
// dropped at launch in favour of the setting ctx_checkpoints: the launch
// logs ONE line naming the profile, the flag and value dropped and the
// value used, and the instance carries the same note in its status.
func TestLaunch_DroppedCheckpointFlagIsLoggedOnce(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.cfg.Defaults.CtxCheckpoints = 32
	h.llama("s", 4000, func(p *core.Profile) { p.ExtraArgs = []string{"--jinja", "--ctx-checkpoints", "3"} })
	h.release(h.granted(func() *waiter { w := h.request("s"); h.healthy("s"); return w }()))
	recs := h.logs.find(msgCkptFlagDropped)
	if len(recs) != 1 {
		t.Fatalf("want one line, got %+v", recs)
	}
	a := recs[0].Attrs
	if a["profile"] != "s" || a["dropped"] != "--ctx-checkpoints 3" || a["used"] != "--ctx-checkpoints 32 (setting ctx_checkpoints)" {
		t.Fatalf("%+v", a)
	}
	var note string
	for _, s := range h.c.statusDoc().Instances {
		if s.Profile == "s" {
			note = s.LaunchNote
		}
	}
	if !strings.Contains(note, "--ctx-checkpoints 3") || !strings.Contains(note, "--ctx-checkpoints 32") || !strings.Contains(note, "ctx_checkpoints") {
		t.Fatalf("note %q", note)
	}
	// a profile without the flag: no line, no note
	h2 := newHarness(t).nodes(ps5("a", 2, 15000))
	h2.llama("plain", 4000)
	h2.release(h2.granted(func() *waiter { w := h2.request("plain"); h2.healthy("plain"); return w }()))
	if recs := h2.logs.find(msgCkptFlagDropped); len(recs) != 0 {
		t.Fatalf("%+v", recs)
	}
	for _, s := range h2.c.statusDoc().Instances {
		if s.Profile == "plain" && s.LaunchNote != "" {
			t.Fatalf("plain note %q", s.LaunchNote)
		}
	}
}

// v87: a stored --cache-ram is dropped too, and the line says the setting
// used on that path.
func TestLaunch_DroppedCacheFlagNamesTheCacheSetting(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.cfg.Defaults.CtxCheckpoints, h.cfg.Defaults.CacheRAMMiB = 32, 2048
	h.llama("s", 4000, func(p *core.Profile) { p.ExtraArgs = []string{"--ctx-checkpoints", "3", "-cram", "0"} })
	h.release(h.granted(func() *waiter { w := h.request("s"); h.healthy("s"); return w }()))
	recs := h.logs.find(msgCkptFlagDropped)
	if len(recs) != 1 || recs[0].Attrs["dropped"] != "--ctx-checkpoints 3, -cram 0" ||
		recs[0].Attrs["used"] != "--ctx-checkpoints 32 (setting ctx_checkpoints), --cache-ram 2048 (setting cache_ram_mib)" {
		t.Fatalf("%+v", recs)
	}
}
