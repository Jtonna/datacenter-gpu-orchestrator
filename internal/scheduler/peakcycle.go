package scheduler

import "github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"

// Per-cycle peaks, collected only. The scheduler keeps one stored peak per
// placement (reserve.go): a normal cycle replaces it, so the spread of peaks
// across cycles is lost there. Every load-to-unload cycle's peak on each
// node therefore goes into its instanceUnloaded event, beside what the plan
// reserved on that node when the instance was created, so the spread can be
// read from the history later (GET /api/needs peak_cycles). Nothing here is
// read for a decision.

// Why a node of a cycle has no peak figure.
const (
	noPeakAtStart = "no free-memory reading when the load started"
	noPeakLoaded  = "no free-memory reading while loaded"

	// Why a node of a cycle has no settle_use_mib.
	noSettleEnded   = "unloaded before one free_average_window after ready"
	noSettleMoving  = "the free figure was still moving at settle time"
	noSettleReading = "no free-memory reading at settle time"
	noSettleBase    = "the base was taken just after a release on this node"
)

// cyclePeakFields adds to data (an instanceUnloaded event's) the cycle's
// shape, whether it ended normally, and cycle_peaks: for each node of the
// placement, in placement order, its id and name, the most it gave up for
// the instance in the cycle (peak_mib; null with no_peak saying why when no
// free-memory reading was had), and what the plan reserved there when the
// instance was created (share_mib, reserved_mib with the fill reserve, the
// basis; null when the need was not known).
func (c *schedCore) cyclePeakFields(in *inst, data map[string]any) {
	nodes := make([]map[string]any, 0, len(in.placement.Nodes))
	for i, pn := range in.placement.Nodes {
		e := map[string]any{"node_id": string(pn.ID), "node": c.nodeName(pn.ID)}
		if p, ok := in.cyclePeak[pn.ID]; ok {
			e["peak_mib"] = p
		} else {
			e["peak_mib"], e["no_peak"] = nil, noPeakWhy(in, pn.ID)
		}
		if v, ok := in.settleUse[pn.ID]; ok {
			e["settle_use_mib"] = v
		} else {
			why := in.settleWhy[pn.ID]
			if why == "" {
				why = noSettleEnded
			}
			e["settle_use_mib"], e["no_settle"] = nil, why
		}
		if i < len(in.planned) && in.planned[i].ID == pn.ID {
			r := in.planned[i]
			e["share_mib"], e["reserved_mib"], e["basis"] = r.Total-r.Fill, r.Total, r.Basis
		} else {
			e["share_mib"], e["reserved_mib"], e["basis"] = nil, nil, nil
		}
		nodes = append(nodes, e)
	}
	data["shape"] = shapeOf(in.prof)
	data["ended_normally"] = normalStop(in)
	data["cycle_peaks"] = nodes
	data["profile_kind"], data["profile_ctx"], data["profile_parallel"] = string(in.prof.Kind), in.prof.Ctx, max(in.prof.Parallel, 1)
}

func noPeakWhy(in *inst, id core.NodeID) string {
	if _, ok := in.baseFree[id]; !ok {
		return noPeakAtStart
	}
	return noPeakLoaded
}

// noteSettleUse takes, once, what each node of in's placement gives up for
// it one free_average_window after ready (the moment the per-device
// overhead is read, deviceSettleAt): settle_use_mib of the cycle's event. A
// node whose free figure is moving (latest and average more than
// deviceMovingMiB apart), has no reading, or whose base may still show an
// instance that left just before gets none, with the reason. The growth
// from there to the cycle's peak is the context's (GET /api/needs
// peak_cycles growth_readings). Collected only: nothing reads it for a
// decision.
func (c *schedCore) noteSettleUse(in *inst) {
	if in.settleTaken || in.readyAt.IsZero() || c.now.Before(in.readyAt.Add(max(c.cfg.Defaults.FreeAverageWindow, 0))) {
		return
	}
	in.settleTaken = true
	in.settleUse, in.settleWhy = map[core.NodeID]int{}, map[core.NodeID]string{}
	for _, pn := range in.placement.Nodes {
		n, ok := c.raw[pn.ID]
		switch {
		case in.baseUnsettled[pn.ID]:
			in.settleWhy[pn.ID] = noSettleBase
		case !ok || n.FreeUnknown || !n.FreeAvgKnown:
			in.settleWhy[pn.ID] = noSettleReading
		case n.FreeMiB-n.FreeAvgMiB > deviceMovingMiB || n.FreeMiB-n.FreeAvgMiB < -deviceMovingMiB:
			in.settleWhy[pn.ID] = noSettleMoving
		default:
			if used, ok := c.measuredUse(in, pn.ID); ok {
				in.settleUse[pn.ID] = used
			} else {
				in.settleWhy[pn.ID] = noSettleReading
			}
		}
	}
}
