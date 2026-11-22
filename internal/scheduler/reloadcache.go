package scheduler

import (
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
)

// Load modes: how the nodes of a load get the model's weights.
const (
	// LoadModeFileRead: the workers read the model file themselves
	// (worker_model_dir, LLAMA_RPC_FILE_LOAD=required).
	LoadModeFileRead = "file_read"
	// LoadModePushed: the host reads the file and pushes the weights over the
	// worker connection.
	LoadModePushed = "pushed"
	// LoadModeLocal: no remote worker; the host reads the file itself.
	LoadModeLocal = "local"
)

// Reload from cache verdicts (design, "Reload from cache"). They say how much
// of the node's file share CAN stay in the file cache next to the loaded
// model, not that the next load will be warm: the cache is shared with the
// files of models loaded earlier, and after other large models the first
// reloads still read part of the file again (measured on consoles).
const (
	ReloadFull    = "full"    // all of the node's file share can stay cached next to the loaded model
	ReloadPartial = "partial" // only part of the file share can stay cached
	ReloadNone    = "none"    // the loaded model leaves no room for the file in the cache
	ReloadUnknown = "unknown" // a figure is missing, or the node does not read the file
)

// ReloadInput is what the reload rule needs about one node of a placement,
// in MiB. A zero figure is unknown.
type ReloadInput struct {
	FileRead  bool // the node reads its share of the model file itself
	Shared    bool // the model sits in the same memory as the file cache
	FileMiB   int  // the node's share of the model file
	ModelMiB  int  // the node's share of the loaded model
	MemoryMiB int  // the memory the file cache lives in (system RAM)
	MarginMiB int  // kept free: tier headroom plus watchdog floor
}

// ReloadFromCache says whether the node's share of the model file can stay in
// its page cache while the model is loaded, so that the next load of the same
// model reads the file from memory (measured on consoles: 2.5 s instead of
// 19-23 s). cached is the estimated fraction of the file share that stays
// cached (1 for full, 0 for none and unknown).
//
// On a node whose model shares memory with the file cache (a console, an
// integrated GPU) both must fit at once: full when file + model <= memory -
// margin; otherwise the load pushes part of its own file (and other models'
// files) out of the cache. On a node with dedicated GPU memory the model sits
// in VRAM and the cache in system RAM, so only the file share has to fit in
// system RAM.
func ReloadFromCache(in ReloadInput) (verdict string, cached float64) {
	if !in.FileRead || in.FileMiB <= 0 || in.ModelMiB <= 0 || in.MemoryMiB <= 0 {
		return ReloadUnknown, 0
	}
	room := in.MemoryMiB - in.MarginMiB
	if in.Shared {
		room -= in.ModelMiB
	}
	switch {
	case room >= in.FileMiB:
		return ReloadFull, 1
	case room <= 0:
		return ReloadNone, 0
	}
	return ReloadPartial, float64(room) / float64(in.FileMiB)
}

// CacheRoom compares the room the reload rule leaves for the file share
// (memory - margin, minus the model share on a shared-memory node) with what
// the node's file cache holds now (cachedMiB, the agent's cached_bytes):
// room is what is left once the current cache is counted, and occupied is
// true when that is less than the file share. It is not folded into the
// verdict: the current cache figure cannot tell whether the cached data is
// this model's own file. ok is false when an input is unknown or the node does
// not read the file.
func CacheRoom(in ReloadInput, cachedMiB int) (room int, occupied, ok bool) {
	if !in.FileRead || in.FileMiB <= 0 || in.ModelMiB <= 0 || in.MemoryMiB <= 0 || cachedMiB < 0 {
		return 0, false, false
	}
	room = in.MemoryMiB - in.MarginMiB
	if in.Shared {
		room -= in.ModelMiB
	}
	room -= cachedMiB
	return room, room < in.FileMiB, true
}

// NodeReload is the reload rule's answer for one node of a placement, as the
// plan and the status show it.
type NodeReload struct {
	ID       core.NodeID
	Verdict  string
	Shared   bool
	FileMiB  int
	ModelMiB int
	// MemoryMiB and MarginMiB are the figures the rule used; 0 when unknown.
	MemoryMiB int
	MarginMiB int
	// CachedPct is the estimated share of the node's file share that stays
	// cached, 0 to 100, for a partial verdict.
	CachedPct *float64
	// CachedNowMiB is what the node's file cache holds now (its agent's
	// cached_bytes), CacheRoomMiB the rule's room for the file minus that, and
	// CacheOccupied is set when CacheRoomMiB is below FileMiB: the first
	// reloads are likely to read part of the file again (CacheRoom). Nil and
	// false when the node reports no cache figure.
	CachedNowMiB  *int
	CacheRoomMiB  *int
	CacheOccupied bool
}

// loadModeFor is how the nodes of a placement would get the weights.
func (c *schedCore) loadModeFor(p *core.Profile, pl core.Placement) string {
	if p.Kind == core.KindCommand {
		return ""
	}
	remote := false
	for _, pn := range pl.Nodes {
		if !pn.Local {
			remote = true
		}
	}
	switch {
	case !remote:
		return LoadModeLocal
	case c.agents || c.cfg.AgentsMode():
		if c.cfg.Defaults.WorkerModelDir {
			return LoadModeFileRead
		}
		return LoadModePushed
	}
	// Fleet workers: the host child inherits LLAMA_RPC_FILE_LOAD from the
	// scheduler's environment. "required" makes the workers read the file;
	// unset or "0" pushes; "1" (where offered) depends on the workers.
	switch c.envFileLoad {
	case "required":
		return LoadModeFileRead
	case "", "0":
		return LoadModePushed
	}
	return ""
}

// reloadPlan is the reload rule for every node of a placement. The host
// (local node) always reads the file itself; with pushed weights it reads the
// whole file, since it sends every worker its share.
func (c *schedCore) reloadPlan(p *core.Profile, pl core.Placement, mode string) []NodeReload {
	if p.Kind == core.KindCommand || len(pl.Nodes) == 0 {
		return nil
	}
	info, err := c.need(p)
	if err != nil {
		return nil
	}
	shares := c.expectShares(p, pl)
	fracs := []float64{1}
	if len(pl.Nodes) > 1 {
		fracs = instance.SplitFractions(*p, pl)
	}
	const mib = 1 << 20
	out := make([]NodeReload, 0, len(pl.Nodes))
	for i, pn := range pl.Nodes {
		n, _ := c.node(pn.ID)
		r := NodeReload{ID: pn.ID, Shared: n.SharedMemory}
		frac := 0.0
		if i < len(fracs) {
			frac = fracs[i]
		}
		read := mode == LoadModeFileRead || mode == LoadModeLocal || pn.Local
		if pn.Local && mode == LoadModePushed {
			frac = 1
		}
		if info.FileBytes > 0 {
			r.FileMiB = int(math.Ceil(float64(info.FileBytes) * frac / mib))
		}
		if i < len(shares) {
			r.ModelMiB = shares[i]
		}
		sys := n.SystemMiB
		if pn.Local && sys <= 0 {
			sys = c.localSysMiB
		}
		r.MarginMiB = n.WatchdogFloorMiB
		if n.SharedMemory {
			r.MarginMiB += n.HeadroomMiB
			if sys <= 0 {
				sys = n.TotalMiB // the device memory is system memory
			}
		}
		r.MemoryMiB = sys
		ri := ReloadInput{FileRead: read, Shared: n.SharedMemory, FileMiB: r.FileMiB, ModelMiB: r.ModelMiB,
			MemoryMiB: r.MemoryMiB, MarginMiB: r.MarginMiB}
		v, cached := ReloadFromCache(ri)
		r.Verdict = v
		if v == ReloadPartial {
			pct := math.Round(cached * 100)
			r.CachedPct = &pct
		}
		if n.CachedKnown {
			if room, occ, ok := CacheRoom(ri, n.CachedMiB); ok {
				now := n.CachedMiB
				r.CachedNowMiB, r.CacheRoomMiB, r.CacheOccupied = &now, &room, occ
			}
		}
		out = append(out, r)
	}
	return out
}

// localSystemMiB is the scheduler machine's own memory (MemTotal of
// /proc/meminfo) in MiB, 0 when it cannot be read (not Linux).
func localSystemMiB() int {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	return parseMemTotalMiB(string(b))
}

// parseMemTotalMiB reads "MemTotal: <n> kB" from a /proc/meminfo text.
func parseMemTotalMiB(s string) int {
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "MemTotal:" {
			kb, err := strconv.Atoi(f[1])
			if err != nil {
				return 0
			}
			return kb / 1024
		}
	}
	return 0
}
