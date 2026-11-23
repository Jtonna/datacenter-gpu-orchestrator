package scheduler

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/ggufgen"
)

// benchStore: a store shaped like a live one (83 measurements of 12 models
// at three contexts and one to three devices) and 311 profiles at other
// contexts, with the production need provider reading GGUF files.
func benchStore(b *testing.B) *harness {
	dir := b.TempDir()
	h := deriveHarness(b)
	h.cfg.ModelsDir = dir
	h.c.needs = &ConfigNeeds{Cfg: h.cfg}
	rows := 0
	for m := 0; rows < 83; m++ {
		model := fmt.Sprintf("model%02d.gguf", m)
		path := filepath.Join(dir, model)
		if err := ggufgen.Write(path, ggufgen.Spec{Blocks: 32, Embd: 4096, Heads: 32, HeadsKV: 8}); err != nil {
			b.Fatal(err)
		}
		fi, _ := os.Stat(path)
		for _, ctx := range []int{2048, 8192, 32768} {
			for d := 1; d <= 3 && rows < 83; d++ {
				h.measure(model, ctx, d, 1000+ctx/8+d*700, func(*core.Profile) {})
				for _, ls := range h.c.learned {
					for _, l := range ls {
						l.modelBytes = fi.Size()
					}
				}
				rows++
			}
		}
	}
	models := len(h.c.learned) / 3
	for i := range 311 {
		ctx := []int{4096, 16384, 65536, 1024, 24576}[i%5]
		h.llama(fmt.Sprintf("p%03d", i), 0, func(p *core.Profile) { p.Model, p.Ctx = fmt.Sprintf("model%02d.gguf", i%models), ctx })
	}
	for _, p := range h.cfg.Profiles {
		h.c.need(p)
	}
	return h
}

// The first listing after a configuration write: the provider's cache is
// dropped (as at every write), then every profile's need and the pair list
// of GET /api/needs.
func BenchmarkDerive_ListingAfterConfigWrite(b *testing.B) {
	h := benchStore(b)
	b.ResetTimer()
	for range b.N {
		nc := cloneCfg(h.cfg)
		nc.Revision++
		h.c.applyConfig(nc)
		h.cfg = nc
		for _, p := range h.cfg.Profiles {
			h.c.need(p)
		}
		_ = h.c.derivePairs(nil)
	}
}

// A load stores a measurement: one row changes, then every profile's need
// and the pair list.
func BenchmarkDerive_ListingAfterAMeasurement(b *testing.B) {
	h := benchStore(b)
	var rows []*learnedNeed
	for _, ls := range h.c.learned {
		for _, l := range ls {
			rows = append(rows, l)
		}
	}
	b.ResetTimer()
	for i := range b.N {
		rows[i%len(rows)].TotalMiB++
		for _, p := range h.cfg.Profiles {
			h.c.need(p)
		}
		_ = h.c.derivePairs(nil)
	}
}
