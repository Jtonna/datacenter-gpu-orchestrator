package scheduler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/estimate"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/gguf"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// Clock is the time source of the scheduler. The core only calls Now; the
// runner uses AfterFunc to wake itself at the core's next deadline.
type Clock interface {
	Now() time.Time
	// AfterFunc calls f in its own goroutine once d has passed. The
	// returned function cancels the call if it has not happened yet.
	AfterFunc(d time.Duration, f func()) (stop func() bool)
}

// RealClock is the wall clock.
type RealClock struct{}

// Now implements Clock.
func (RealClock) Now() time.Time { return time.Now() }

// AfterFunc implements Clock.
func (RealClock) AfterFunc(d time.Duration, f func()) func() bool {
	return time.AfterFunc(d, f).Stop
}

// NeedInfo is a profile's memory need plus the model file size used for
// the load timeout (design 7.6).
type NeedInfo struct {
	Need      placer.Need
	FileBytes int64
	// DeviceMiB is, for an estimate, the compute buffer every device of the
	// placement holds on top of its share of Need, which grows with the
	// context (estimate.ComputeMiB); defaults.device_overhead_mib is the
	// fixed part. 0 for a configured need. It is counted once per device
	// (deviceCost), never spread over the split.
	DeviceMiB int

	// Set by the core (schedCore.need), not by a NeedProvider: where Need
	// comes from, and the figure the provider gave before the core applied
	// the estimate margin or a measurement learned this run.
	Source  NeedSource
	BaseMiB int
	// MeasuredAt is when a restored measurement was taken (NeedRestored).
	MeasuredAt time.Time
	// Devices is the device count Need is for, when a measurement of the
	// profile's shape is known (needcount.go); 0 otherwise. FromDevices is
	// the count the figure was measured on (0: not a measurement).
	Devices, FromDevices int
	// FromServer: Need is a measurement of the same shape hosted on the
	// scheduler's machine, used for a host: node profile that has none of
	// its own yet (the host overhead is added on the host node's share).
	FromServer bool
	// DerivedFromCtx: Need is derived (derive.go) from the measurement of the
	// same model at this other context; 0 otherwise.
	DerivedFromCtx int
	// Slope: Need is read from the model's own line on this device count
	// (ownslope.go); nil otherwise.
	Slope *DerivedSlope

	swa bool // the model has a sliding window (concave in the context: ownslope.go)

	// what at needs to give the need of another device count
	figs       []learnedFig
	serverFigs bool        // figs are the server-hosted shape's (planLearnedOf)
	derive     *deriveFrom // figs are derived from another context (derive.go)
	baseNeed   placer.Need
	baseSource NeedSource
	perDevice  int
	fromTotal  int // the total of the measurement Need was taken from (countWords)
}

// NeedSource says where a need comes from, in the words the messages and the
// plan use.
type NeedSource string

const (
	NeedConfigured NeedSource = "measured, needs_mib"
	NeedEstimated  NeedSource = "estimated"
	NeedLearned    NeedSource = "measured this run"
	// NeedRestored is a measurement of an earlier run read from the store at
	// start; the plan's basis says "measured on <date>, restored at start".
	NeedRestored NeedSource = "restored at start"
	// NeedDerived is derived from a measurement of the same model at another
	// context (derive.go).
	NeedDerived NeedSource = "derived"
)

// NeedProvider gives a profile's memory need (design 7.2). An error is a
// load failure for that profile (502 and backoff), never a crash. It is
// called from the core goroutine; results that succeed are cached by the
// core for the life of the scheduler.
type NeedProvider interface {
	Need(p *core.Profile) (NeedInfo, error)
}

// NeedFunc adapts a function to NeedProvider.
type NeedFunc func(p *core.Profile) (NeedInfo, error)

// Need implements NeedProvider.
func (f NeedFunc) Need(p *core.Profile) (NeedInfo, error) { return f(p) }

// ConfigNeeds is the production NeedProvider: needs_mib when set (measured),
// else weights + KV from the model's GGUF header (estimated). For kind:
// command the need is needs_mib and the file size is 0.
type ConfigNeeds struct {
	Cfg *config.Config

	mu    sync.Mutex
	cache map[string]NeedInfo
	metas map[string]metaEntry // GGUF headers by path, kept across configuration writes
	reads int                  // headers read (tests)
}

// metaEntry is a GGUF header read, valid while the file keeps its size and
// modification time.
type metaEntry struct {
	size int64
	mod  time.Time
	meta *gguf.Meta
}

// HeaderReady reports whether the GGUF header of model has been read (by a
// profile's need or by Warm). The derivation asks for the estimates of other
// contexts only then, so the core loop never waits on a header read for it.
func (c *ConfigNeeds) HeaderReady(model string) bool {
	c.mu.Lock()
	defer supervise.Unlocked("scheduler", c.mu.Unlock)
	_, ok := c.metas[filepath.Join(c.Cfg.ModelsDir, model)]
	return ok
}

// Warm reads the headers of models not read yet, off the core loop: the
// lock is not held during a read, so the core's own calls never wait on it.
func (c *ConfigNeeds) Warm(ctx context.Context, models []string) {
	for _, m := range models {
		if ctx.Err() != nil {
			return
		}
		var path string
		var ok bool
		func() {
			c.mu.Lock()
			defer supervise.Unlocked("scheduler", c.mu.Unlock)
			path = filepath.Join(c.Cfg.ModelsDir, m)
			_, ok = c.metas[path]
		}()
		if ok {
			continue
		}
		st, err := os.Stat(path)
		if err != nil {
			continue
		}
		meta, err := gguf.ReadMeta(path)
		if err != nil {
			continue
		}
		func() {
			c.mu.Lock()
			defer supervise.Unlocked("scheduler", c.mu.Unlock)
			c.reads++
			if c.metas == nil {
				c.metas = map[string]metaEntry{}
			}
			if _, ok := c.metas[path]; !ok {
				c.metas[path] = metaEntry{size: st.Size(), mod: st.ModTime(), meta: meta}
			}
		}()
	}
}

// metaOf is the header of the file at path, read once per version of the
// file: every profile and every derivation estimate of one model shares it.
func (c *ConfigNeeds) metaOf(path string) (*gguf.Meta, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if e, ok := c.metas[path]; ok && e.size == st.Size() && e.mod.Equal(st.ModTime()) {
		return e.meta, nil
	}
	c.reads++
	meta, err := gguf.ReadMeta(path)
	if err != nil {
		return nil, err
	}
	if c.metas == nil {
		c.metas = map[string]metaEntry{}
	}
	c.metas[path] = metaEntry{size: st.Size(), mod: st.ModTime(), meta: meta}
	return meta, nil
}

// Need implements NeedProvider.
func (c *ConfigNeeds) Need(p *core.Profile) (NeedInfo, error) {
	c.mu.Lock()
	defer supervise.Unlocked("scheduler", c.mu.Unlock)
	if info, ok := c.cache[p.Name]; ok {
		return info, nil
	}
	info, err := c.compute(p)
	if err != nil {
		return NeedInfo{}, err
	}
	if c.cache == nil {
		c.cache = map[string]NeedInfo{}
	}
	c.cache[p.Name] = info
	return info, nil
}

func (c *ConfigNeeds) compute(p *core.Profile) (NeedInfo, error) {
	if p.Kind == core.KindCommand {
		return NeedInfo{Need: placer.Measured(p.NeedsMiB)}, nil
	}
	path := filepath.Join(c.Cfg.ModelsDir, p.Model)
	if p.NeedsMiB > 0 {
		st, err := os.Stat(path)
		if err != nil {
			return NeedInfo{}, fmt.Errorf("model file: %w", err)
		}
		return NeedInfo{Need: placer.Measured(p.NeedsMiB), FileBytes: st.Size()}, nil
	}
	meta, err := c.metaOf(path)
	if err != nil {
		return NeedInfo{}, fmt.Errorf("model file: %w", err)
	}
	info, err := estimateFromMeta(meta, p)
	if err != nil {
		return NeedInfo{}, fmt.Errorf("model file %s: %w", path, err)
	}
	return info, nil
}

// estimateFromMeta is the estimated need of a kind: llama profile from its
// model's GGUF metadata and file size (meta.FileSize): the weights plus the
// KV cache for the profile's context and slots, and the per-device compute
// buffer that grows with the context (DeviceMiB).
func estimateFromMeta(meta *gguf.Meta, p *core.Profile) (NeedInfo, error) {
	elems, err := meta.KVElemsPerToken()
	if err != nil {
		return NeedInfo{}, err
	}
	parallel := max(p.Parallel, 1)
	quant := p.KVQuant
	if quant == "" {
		quant = "f16"
	}
	kv, err := estimate.KVMiB(1, p.Ctx*parallel, elems, 0, quant)
	if err != nil {
		return NeedInfo{}, err
	}
	return NeedInfo{Need: placer.Estimated(estimate.NeedMiB(meta.FileSize, kv)), FileBytes: meta.FileSize,
		DeviceMiB: estimate.ComputeMiB(p.Ctx * parallel), swa: meta.SlidingWindow}, nil
}
