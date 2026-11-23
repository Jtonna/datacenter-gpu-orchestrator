package scheduler

import (
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// A profile whose model file is missing stays in the configuration (B4); its
// load is refused, and the reason names the file and the directory.
func TestMissingModelFileReasonNamesTheFile(t *testing.T) {
	dir := t.TempDir()
	c := &ConfigNeeds{Cfg: &config.Config{ModelsDir: dir}}
	var err error
	for _, needs := range []int{0, 4000} { // estimated and measured
		c = &ConfigNeeds{Cfg: c.Cfg}
		_, err = c.Need(&core.Profile{Name: "m", Kind: core.KindLlama, Model: "gone.gguf", Ctx: 1024, Parallel: 1, NeedsMiB: needs})
		if err == nil || !strings.Contains(err.Error(), `model file "gone.gguf" is missing from models_dir`) {
			t.Fatalf("needs_mib %d: %v", needs, err)
		}
	}
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 0)
	h.needErr["m"] = err // the provider's answer, as ConfigNeeds gives it
	r := h.c.dryPlan("m")
	if r.Action != PlanReject || !strings.Contains(r.Reason, `model file "gone.gguf" is missing`) {
		t.Fatalf("plan %s: %s", r.Action, r.Reason)
	}
}
