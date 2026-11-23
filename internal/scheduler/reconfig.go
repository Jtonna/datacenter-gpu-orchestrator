package scheduler

import (
	"fmt"
	"reflect"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// Reconfigurer is implemented by a NeedProvider that caches per-profile
// results and must forget them when the configuration changes.
type Reconfigurer interface {
	Reconfigure(cfg *config.Config)
}

// Reconfigure replaces the configuration and forgets the cached needs.
func (c *ConfigNeeds) Reconfigure(cfg *config.Config) {
	func() {
		c.mu.Lock()
		defer supervise.Unlocked("scheduler", c.mu.Unlock)
		c.Cfg = cfg
		c.cache = nil
	}()
}

// staleProfile reports that an instance runs with a profile definition that has since changed or been deleted.
func (c *schedCore) staleProfile(in *inst) bool {
	p := c.cfg.Profiles[in.prof.Name]
	if p == nil {
		return true
	}
	a, b := *p, *in.prof
	a.Description, b.Description = "", "" // words for people, not the load
	return !reflect.DeepEqual(a, b)
}

// applyConfig swaps in a new snapshot (live apply: a change takes effect without a restart).
//   - Loaded instances keep the profile they started with (inst.prof); they are
//     reported stale and are replaced at their next load.
//   - Queue entries take the new definition; those of a deleted profile fail.
//   - Tier, exclusion and timing settings are read from c.cfg at each decision,
//     so they apply from the next one.
func (c *schedCore) applyConfig(nc *config.Config) {
	old := c.cfg
	c.cfg = nc
	c.build = instance.OptionsFromConfig(nc)
	if r, ok := c.needs.(Reconfigurer); ok {
		r.Reconfigure(nc)
	}
	// Cached needs belong to the old definitions.
	for name := range c.needs_ {
		if np, op := nc.Profiles[name], old.Profiles[name]; np == nil || op == nil || !reflect.DeepEqual(*np, *op) {
			delete(c.needs_, name)
			delete(c.warnedLow, name)
		}
	}
	// Learned needs are kept by shape (needs.go): a deleted or changed
	// profile removes none; a changed profile uses the figure of its new
	// shape, if there is one.
	for name, e := range c.entries {
		np := nc.Profiles[name]
		if np == nil {
			ws := append([]*waiter(nil), e.waiters...)
			c.deleteEntry(e)
			for _, w := range ws {
				if in := c.insts[name]; in != nil {
					in.waiters = removeWaiter(in.waiters, w)
				}
			}
			err := errUnknown(name)
			err.Message = fmt.Sprintf("profile %q was deleted by a configuration change", name)
			c.failAll(ws, err)
			continue
		}
		e.prof = np
	}
	c.log.Info("configuration applied", "revision", nc.Revision, "profiles", len(nc.Profiles))
	c.changed = true
}

func removeWaiter(ws []*waiter, w *waiter) []*waiter {
	out := ws[:0]
	for _, x := range ws {
		if x != w {
			out = append(out, x)
		}
	}
	return out
}
