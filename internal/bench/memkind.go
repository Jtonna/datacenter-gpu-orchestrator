package bench

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// Which memory a node's "available" figure is (NodeMemLow.Kind,
// WorkerInfo.AvailKind). A node whose device shares system memory (a console)
// uses system memory for the model, so its figure is the system's available
// memory. A node whose device has memory of its own (a dedicated GPU) uses the
// GPU's memory, so its figure is the GPU's free memory, when a live figure for
// it exists. The two are never mixed in one aggregate.
const (
	MemKindSystem = "system"
	MemKindGPU    = "gpu"
)

// MemNoteNoLiveGPU says that a node with a dedicated GPU had no live GPU
// figure, so its system figure was recorded instead.
const MemNoteNoLiveGPU = "the node's GPU has memory of its own but reported no live figure (no GPU exporter data); this is its system memory"

// KindOf is the kind of a recorded figure: a row stored before kinds were
// recorded has none and is system memory.
func (n NodeMemLow) KindOf() string {
	if n.Kind == "" {
		return MemKindSystem
	}
	return n.Kind
}

// statusAgent is the part of a view node's agent facts (/api/status,
// view.nodes[].agent) that tells which memory the node's model uses.
type statusAgent struct {
	MemAvailMiB *int64          `json:"mem_available_mib"`
	Platform    string          `json:"platform"`
	Devices     []statusDevice  `json:"devices"`
	Stats       json.RawMessage `json:"stats"`
	// LastSeenAgoS is the age of the agent's last report (its heartbeat,
	// which carries the memory figure and the stats).
	LastSeenAgoS *float64 `json:"last_seen_ago_s"`
}

type statusDevice struct {
	Name               string `json:"name"`
	Backend            string `json:"backend"`
	SharesSystemMemory bool   `json:"shares_system_memory"`
}

// agentAvail is a remote node's available figure: the GPU's free memory from
// the stats object when its device has memory of its own and the GPU exporter
// gives a live reading, else the agent's mem_available_mib (system), with a
// note when a dedicated GPU had no live figure.
//
// The device list's free_mib is not used: the agent reads it only at
// registration and after a launch exits, so it is stale during a load.
func agentAvail(a *statusAgent) (v *int64, kind, note string, ageMS int64) {
	dev, ok := pickStatusDevice(a)
	if ok && !dev.SharesSystemMemory {
		if free, age, ok := liveGPUFree(a.Stats, dev); ok {
			return &free, MemKindGPU, "", age
		}
		if strings.EqualFold(dev.Backend, "cuda") {
			note = MemNoteNoLiveGPU
		}
	}
	if a.MemAvailMiB == nil {
		return nil, "", "", 0
	}
	return a.MemAvailMiB, MemKindSystem, note, 0
}

// pickStatusDevice is the device the node's worker uses, as the pool picks it:
// the first one of the platform's backend, else the first one.
func pickStatusDevice(a *statusAgent) (statusDevice, bool) {
	if len(a.Devices) == 0 {
		return statusDevice{}, false
	}
	backend := a.Platform[strings.LastIndex(a.Platform, "-")+1:]
	for _, d := range a.Devices {
		if backend != "" && strings.EqualFold(d.Backend, backend) {
			return d, true
		}
	}
	return a.Devices[0], true
}

// liveGPUFree is the free memory in MiB of the stats GPU entry that is dev
// (stats contract 5.6: total - used - reserved), when that entry is a
// discrete GPU and its source is ok. A CUDA<n> device is the nvidia entry with
// index n (or the only nvidia entry when none has an index); any other device
// is the only discrete entry, when there is exactly one.
func liveGPUFree(raw json.RawMessage, dev statusDevice) (free, ageMS int64, ok bool) {
	var s agentproto.Stats
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil || s.StatsVersion != agentproto.StatsVersion {
		return 0, 0, false
	}
	var cands []agentproto.GPUStat
	name := strings.ToUpper(dev.Name)
	if strings.EqualFold(dev.Backend, "cuda") || strings.HasPrefix(name, "CUDA") {
		var nv []agentproto.GPUStat
		for _, g := range s.GPUs {
			if g.Vendor == "nvidia" {
				nv = append(nv, g)
			}
		}
		if n, err := strconv.Atoi(strings.TrimPrefix(name, "CUDA")); err == nil {
			for _, g := range nv {
				if g.Index != nil && *g.Index == n {
					cands = append(cands, g)
				}
			}
		}
		if len(cands) == 0 && len(nv) == 1 && nv[0].Index == nil {
			cands = nv
		}
	} else {
		for _, g := range s.GPUs {
			if g.Kind == agentproto.GPUDiscrete {
				cands = append(cands, g)
			}
		}
	}
	if len(cands) != 1 {
		return 0, 0, false
	}
	g := cands[0]
	src := agentproto.SourceNode
	if g.Vendor == "nvidia" {
		src = agentproto.SourceNvidia
	}
	if g.Kind != agentproto.GPUDiscrete || s.Sources[src].Status != agentproto.SourceOK || g.MemTotalBytes <= 0 {
		return 0, 0, false
	}
	if a := s.Sources[src].AgeMS; a != nil && *a > 0 {
		ageMS = *a
	}
	free = g.MemTotalBytes - g.MemUsedBytes
	if g.MemReservedBytes != nil {
		free -= *g.MemReservedBytes
	}
	return max(free, 0) >> 20, ageMS, true
}
