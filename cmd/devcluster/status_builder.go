package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/api"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// statusBuilder assembles a scripted scheduler.Status whose numbers agree with
// each other: what an instance holds on a node is taken off that node's free
// memory and budget, the node names the instance as its holder, and so on.
// The result is converted by the real api.StatusJSON.
type statusBuilder struct {
	now   time.Time
	st    scheduler.Status
	nodes map[string]*scheduler.NodeStatus // by hostname
	used  map[string]int                   // MiB an instance holds, by hostname
	last  map[string]map[string]any        // last_request by profile
	port  int
	// staleProfile marks instances whose profile was edited after they
	// loaded (stale_profile: true in the status document).
	staleProfile map[string]bool
}

func newStatus(now time.Time) *statusBuilder {
	b := &statusBuilder{now: now, nodes: map[string]*scheduler.NodeStatus{}, used: map[string]int{}, last: map[string]map[string]any{}, port: 5801}
	b.st = scheduler.Status{Time: now, Instances: []scheduler.InstanceStatus{}, Queue: []scheduler.EntryStatus{},
		Quarantines: []scheduler.QuarantineStatus{}, Backoffs: []scheduler.BackoffStatus{}, RecentLoads: []scheduler.LoadOutcome{},
		LearnedNeeds: map[string]scheduler.LearnedNeed{},
		View:         scheduler.ViewStatus{Age: 2100 * time.Millisecond, Connected: true}}
	for _, n := range allNodes {
		ns := &scheduler.NodeStatus{ID: core.NodeID(n.ID), Hostname: n.Host, Local: n.Local, Online: true, Addr: n.Addr(),
			Priority: n.Priority, Eligible: true, TotalMiB: n.TotalMiB, EmptyMiB: n.TotalMiB, PlanBudgetMiB: n.Budget()}
		if n.Host == alien.Host {
			ns.Excluded, ns.Eligible = true, false
			ns.Addr = "192.0.2.3:50052"
			ns.PlanBudgetMiB = n.TotalMiB - n.Headroom
		}
		b.nodes[n.Host] = ns
	}
	return b
}

// measuredTotal is what the load log of a profile sums to, by worker count.
func measuredTotal(profile string, workers int) float64 {
	switch modelFileOf(profile) {
	case fileGemma:
		return map[int]float64{1: 7514, 2: 7831, 3: 7852}[max(1, min(workers, 3))]
	case fileQwen4:
		return 3343
	case fileDevstral:
		return 12020 + 140*float64(workers-1)
	case fileQwen27:
		return 19240 + 120*float64(workers-1)
	case fileKimi:
		return 26510
	case fileLlama70:
		return 47200
	}
	return 4000
}

func fractions(p profileDef, n int) []float64 {
	out := make([]float64, n)
	if len(p.TensorSplit) == n {
		sum := 0
		for _, v := range p.TensorSplit {
			sum += v
		}
		for i, v := range p.TensorSplit {
			out[i] = float64(v) / float64(sum)
		}
		return out
	}
	for i := range out {
		out[i] = 1 / float64(n)
	}
	return out
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func (b *statusBuilder) cmdLine(p profileDef, hosts []string, port int) string {
	var rpc, dev []string
	local := false
	for i, h := range hosts {
		n, _ := nodeByHost(h)
		if n.Local {
			local = true
			continue
		}
		rpc = append(rpc, n.Addr())
		dev = append(dev, fmt.Sprintf("RPC%d", i))
	}
	if local {
		dev = append(dev, "CUDA0")
	}
	par := max(p.Parallel, 1)
	s := fmt.Sprintf("llama-server-rpc --host 127.0.0.1 --port %d --model /models/%s --n-gpu-layers 999 --fit off --ctx-size %d --parallel %d --jinja --cache-ram 2048 -lv 4",
		port, p.Model, p.Ctx*par, par)
	if p.KVQuant != "" {
		s += " --cache-type-k " + p.KVQuant + " --cache-type-v " + p.KVQuant
	}
	if p.FlashAttn {
		s += " --flash-attn on"
	}
	if len(rpc) > 0 {
		s += " --rpc " + strings.Join(rpc, ",")
	}
	s += " --device " + strings.Join(dev, ",")
	if len(hosts) > 1 {
		fr := fractions(p, len(hosts))
		parts := make([]string, len(fr))
		for i, f := range fr {
			parts[i] = fmt.Sprint(int(math.Round(f * 100)))
		}
		s += " --tensor-split " + strings.Join(parts, ",")
	}
	return s
}

type instOpts struct {
	readyAgo  time.Duration // ready since now - this
	createdAt time.Time     // for a starting instance
	served    int
	inFlight  int
	waiters   int
	lastTPS   float64 // 0: no last_request
	lastAgo   time.Duration
	victimOf  string
	state     string // "" = ready; draining
}

// place records an instance's memory on its nodes and returns the placed nodes.
func (b *statusBuilder) place(p profileDef, hosts []string, withMemory, kvAndCompute bool) ([]scheduler.PlacedNodeStatus, float64) {
	fr := fractions(p, len(hosts))
	total := measuredTotal(p.ID, len(hosts))
	var out []scheduler.PlacedNodeStatus
	var sum float64
	for i, h := range hosts {
		n, _ := nodeByHost(h)
		share := total * fr[i]
		ns := b.nodes[h]
		ns.Holder = p.ID
		b.used[h] += int(math.Round(share))
		pn := scheduler.PlacedNodeStatus{ID: core.NodeID(n.ID), Addr: n.Addr(), Local: n.Local, BudgetMiB: ns.PlanBudgetMiB, Memory: []scheduler.DeviceMemory{}}
		if withMemory {
			model := round1(share * 0.86)
			kv, comp := 0.0, 0.0
			if kvAndCompute {
				kv = round1(share * 0.09)
				comp = round1(share - model - kv)
			}
			pn.Memory = append(pn.Memory, scheduler.DeviceMemory{Name: n.RPCName(), ModelMiB: model, KVMiB: kv, ComputeMiB: comp, TotalMiB: round1(model + kv + comp)})
			sum += model + kv + comp
		}
		out = append(out, pn)
	}
	return out, round1(sum)
}

func (b *statusBuilder) health(host, state string, since time.Time, upload, heartbeat string, silent map[string]float64) {
	if silent == nil {
		silent = map[string]float64{}
	}
	b.nodes[host].Health = &scheduler.WorkerHealthStatus{State: state, Since: since.UnixMilli(),
		Channels: scheduler.WorkerChannels{Upload: upload, Heartbeat: heartbeat, Probe: "unknown"}, SilentForS: silent}
}

// ready adds a ready (or draining) instance.
func (b *statusBuilder) ready(profile string, hosts []string, o instOpts) {
	p, _ := profileByID(profile)
	placed, devTotal := b.place(p, hosts, true, true)
	port := b.port
	b.port++
	readySince := b.now.Add(-o.readyAgo)
	loadS := 30 + float64(len(p.Model)%7)*9
	if mb, ok := modelBytes[p.Model]; ok {
		loadS = round1(float64(mb)/(100e6*float64(len(hosts))) + 6)
	}
	created := readySince.Add(-time.Duration(loadS * float64(time.Second)))
	state := "ready"
	if o.state != "" {
		state = o.state
	}
	lastUsed := b.now.Add(-o.lastAgo)
	if o.inFlight > 0 {
		lastUsed = b.now.Add(-3 * time.Second)
	}
	in := scheduler.InstanceStatus{Profile: p.ID, State: state, RunningState: scheduler.RunningReady, Addr: fmt.Sprintf("127.0.0.1:%d", port),
		Cmd: b.cmdLine(p, hosts, port), LogFile: fmt.Sprintf("/var/log/llm-children/%s-%s-%d.log", created.UTC().Format("20060102T150405Z"), p.ID, port-5790),
		Created: created, ReadySince: readySince, LastUsed: lastUsed, ModelFile: p.Model, RequestsServed: o.served,
		LoadDuration: time.Duration(loadS * float64(time.Second)), InFlight: o.inFlight, Waiters: o.waiters, VictimOf: o.victimOf,
		Nodes: placed, HostMemory: []scheduler.DeviceMemory{{Name: "CPU_Mapped", ModelMiB: 360, TotalMiB: 360}}, Unmatched: []scheduler.DeviceMemory{},
		DeviceTotalMiB: devTotal}
	b.st.Instances = append(b.st.Instances, in)
	for _, h := range hosts {
		if n, _ := nodeByHost(h); !n.Local {
			b.health(h, "ok", readySince.Add(-time.Duration(loadS*float64(time.Second))), "unknown", "ok", nil)
		}
	}
	if o.lastTPS > 0 {
		out := 180 + o.served%160
		dur := float64(out) / o.lastTPS * 1000
		b.last[p.ID] = map[string]any{"tokens_per_second": o.lastTPS, "output_tokens": out, "input_tokens": 412 + o.served%300,
			"status": 200, "duration_ms": int64(dur + 640), "finished_at": b.now.Add(-o.lastAgo).UTC().Format("2006-01-02T15:04:05.000Z07:00")}
	}
}

type loadOpts struct {
	startedAgo time.Duration
	progress   float64 // 0..1 of all bytes
	stalledS   float64 // >0: stalled, no eta
	waiters    int
	rateMBs    float64 // for the eta
	source     string  // measured (default) or estimated
}

func i64(v int64) *int64                  { return &v }
func f64(v float64) *float64              { return &v }
func strp(v string) *string               { return &v }
func ms(t time.Time) int64                { return t.UnixMilli() }
func sessionID(t time.Time, n int) string { return fmt.Sprintf("ld-%d-%d", t.UnixMilli(), n) }

// loading adds a starting instance in phase loading with a measured load report.
func (b *statusBuilder) loading(profile string, hosts []string, o loadOpts) {
	p, _ := profileByID(profile)
	placed, devTotal := b.place(p, hosts, true, false)
	port := b.port
	b.port++
	created := b.now.Add(-o.startedAgo)
	total := modelBytes[p.Model]
	fr := fractions(p, len(hosts))
	sent := int64(float64(total) * o.progress)
	var workers []scheduler.LoadWorker
	remaining := sent
	for i, h := range hosts {
		n, _ := nodeByHost(h)
		if n.Local {
			continue
		}
		wt := int64(float64(total) * fr[i])
		ws := min(remaining, wt)
		remaining -= ws
		state := scheduler.WorkerReceiving
		switch {
		case ws >= wt:
			state = scheduler.WorkerDone
		case ws <= 0:
			state = scheduler.WorkerWaiting
		}
		workers = append(workers, scheduler.LoadWorker{Addr: n.Addr(), BytesTotal: wt, BytesSent: i64(ws), State: state})
	}
	src := o.source
	if src == "" {
		src = "measured"
	}
	elapsed := round1(o.startedAgo.Seconds())
	prog := math.Round(o.progress*1000) / 1000
	lr := &scheduler.LoadReport{SessionID: sessionID(created, 7), StartedAt: created.Add(40 * time.Millisecond).UnixMilli(), ElapsedS: elapsed,
		BytesTotal: i64(total), BytesSent: i64(sent), Progress: f64(prog), Source: strp(src), Workers: workers}
	var eta *float64
	if o.stalledS > 0 {
		lr.StalledS = f64(round1(o.stalledS))
	} else {
		rate := o.rateMBs
		if rate <= 0 {
			rate = 80
		}
		eta = f64(math.Round(float64(total-sent) / (rate * 1e6)))
		lr.EtaS = eta
	}
	linkS := float64(total) / (100e6 * float64(len(hosts)))
	in := scheduler.InstanceStatus{Profile: p.ID, State: "starting", RunningState: scheduler.RunningStarting, Phase: "loading",
		Addr: fmt.Sprintf("127.0.0.1:%d", port), Cmd: b.cmdLine(p, hosts, port),
		LogFile: fmt.Sprintf("/var/log/llm-children/%s-%s-%d.log", created.UTC().Format("20060102T150405Z"), p.ID, port-5790),
		Created: created, ModelFile: p.Model, LoadDeadline: created.Add(time.Duration((linkS*2 + 300) * float64(time.Second))).Truncate(time.Second),
		Waiters: o.waiters, Nodes: placed, HostMemory: []scheduler.DeviceMemory{{Name: "CPU_Mapped", ModelMiB: 360, TotalMiB: 360}},
		Unmatched: []scheduler.DeviceMemory{}, DeviceTotalMiB: devTotal,
		LoadElapsedS: f64(elapsed), LoadBytesTotal: i64(total), LoadBytesSent: i64(sent), LoadProgress: f64(prog), LoadEtaS: eta,
		LoadProgressSource: strp(src), Load: lr}
	b.st.Instances = append(b.st.Instances, in)
	for i, h := range hosts {
		if n, _ := nodeByHost(h); !n.Local {
			upload := "ok"
			if i < len(workers) && workers[i].State == scheduler.WorkerWaiting {
				upload = "unknown"
			}
			b.health(h, "ok", created, upload, "ok", nil)
		}
	}
}

func (b *statusBuilder) queue(e scheduler.EntryStatus) {
	if e.Claim == nil {
		e.Claim = []core.NodeID{}
	}
	if e.Victims == nil {
		e.Victims = []string{}
	}
	e.Age = b.now.Sub(e.Created)
	if !e.Claimed {
		e.Clock, e.ClockRunning = e.Age, true
	} else {
		e.Clock = e.Age - 40*time.Second
		if e.Clock < 0 {
			e.Clock = 0
		}
	}
	for _, id := range e.Claim {
		for _, ns := range b.nodes {
			if string(ns.ID) == string(id) {
				ns.ClaimedBy = e.Profile
			}
		}
	}
	b.st.Queue = append(b.st.Queue, e)
}

func (b *statusBuilder) learned(profile string, hosts []string) {
	p, _ := profileByID(profile)
	total := measuredTotal(profile, len(hosts))
	fr := fractions(p, len(hosts))
	var nn []scheduler.LearnedNodeNeed
	for i, h := range hosts {
		n, _ := nodeByHost(h)
		nn = append(nn, scheduler.LearnedNodeNeed{Node: core.NodeID(n.ID), Fraction: math.Round(fr[i]*1000) / 1000, MiB: int(math.Round(total * fr[i]))})
	}
	b.st.LearnedNeeds[profile] = scheduler.LearnedNeed{MeasuredMiB: int(math.Round(total)), NeedMiB: p.NeedsMiB, Source: "measured, needs_mib",
		ConfiguredMiB: p.NeedsMiB, Nodes: nn}
}

// finish computes the node memory figures and returns the JSON document,
// with last_request added per instance as statusView does.
func (b *statusBuilder) finish() map[string]any {
	for _, n := range allNodes {
		ns := b.nodes[n.Host]
		if !ns.Online && ns.FreeMiB == 0 && ns.MissingSince.IsZero() {
			ns.MissingSince = b.now
		}
		base := n.TotalMiB
		if n.Host == alien.Host {
			base = 7782
		}
		ns.FreeMiB = max(base-b.used[n.Host], 0)
		ns.LiveBudgetMiB = max(ns.FreeMiB-n.Headroom, 0)
		if n.Host == alien.Host {
			ns.LiveBudgetMiB = 7526
			ns.PlanBudgetMiB = 7936
			ns.EmptyMiB = 8192
		}
		if !ns.Online {
			ns.Eligible = false
		}
		if !ns.Quarantined.IsZero() {
			ns.Eligible = false
		}
		b.st.View.Nodes = append(b.st.View.Nodes, *ns)
	}
	sort.Slice(b.st.View.Nodes, func(i, j int) bool { return b.st.View.Nodes[i].ID < b.st.View.Nodes[j].ID })
	v := api.StatusJSON(b.st).(map[string]any)
	if insts, ok := v["instances"].([]any); ok {
		for _, i := range insts {
			im := i.(map[string]any)
			if lr, ok := b.last[im["profile"].(string)]; ok {
				im["last_request"] = lr
			}
			im["stale_profile"] = b.staleProfile[im["profile"].(string)]
		}
	}
	return v
}

// finishStatus is finish for callers that want the struct (plans).
func (b *statusBuilder) finishStatus() scheduler.Status {
	b.finish()
	return b.st
}
