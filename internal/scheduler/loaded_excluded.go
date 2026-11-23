package scheduler

import (
	"context"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

// LoadedOnExcluded lists the instances running now on a node that cfg's
// exclude_nodes names (0.1.7 review, ES1). Excluding a node stops new
// placements only: such an instance is not drained and keeps serving until
// it is unloaded, so a configuration write that excludes its node reports
// it. cfg is the configuration being written, not necessarily the one in
// force. Nil when there is none (or the scheduler does not answer).
func (s *Scheduler) LoadedOnExcluded(ctx context.Context, cfg *config.Config) []config.LoadedOnExcluded {
	if cfg == nil || len(cfg.ExcludeNodes) == 0 {
		return nil
	}
	st, err := s.Status(ctx)
	if err != nil {
		return nil
	}
	return loadedOnExcluded(st, cfg)
}

func loadedOnExcluded(st Status, cfg *config.Config) []config.LoadedOnExcluded {
	host := map[string]string{}
	for _, n := range st.View.Nodes {
		host[string(n.ID)] = n.Hostname
	}
	var out []config.LoadedOnExcluded
	for _, in := range st.Instances {
		var l config.LoadedOnExcluded
		for _, n := range in.Nodes {
			h := host[string(n.ID)]
			e, ok := cfg.ExcludedBy(n.ID, h)
			if !ok {
				continue
			}
			if h == "" {
				h = string(n.ID)
			}
			l.Nodes, l.Entries = append(l.Nodes, h), append(l.Entries, e)
		}
		if len(l.Nodes) == 0 {
			continue
		}
		since := in.ReadySince
		if since.IsZero() {
			since = in.Created
		}
		l.Profile, l.State, l.LoadedSince = in.Profile, in.State, since.UnixMilli()
		out = append(out, l)
	}
	return out
}
