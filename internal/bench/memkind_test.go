package bench

import (
	"encoding/csv"
	"strings"
	"testing"
)

// Which memory a node's "available" figure is: system memory on a node whose
// GPU shares it (a console), the GPU's free memory on a node with a dedicated
// GPU when its exporter gives a live figure, system memory (marked) when it
// does not, and the scheduler's own figure (its GPU) on the local node.

// statusDoc is an /api/status document with the given view nodes.
func statusDoc(nodes ...string) []byte {
	return []byte(`{"time":"2026-11-21T10:00:00Z","instances":[],"queue":[],"recent_loads":[],"view":{"nodes":[` + strings.Join(nodes, ",") + `]}}`)
}

const (
	// A console: Vulkan device sharing system memory, an integrated AMD GPU in
	// the stats. The device list's free figure (13800) is not live.
	consoleNode = `{"id":"c1","hostname":"ps5-a","free_mib":13000,"agent":{"mem_available_mib":9500,"platform":"linux-amd64-vulkan",
	  "devices":[{"name":"Vulkan0","backend":"vulkan","total_mib":14336,"free_mib":13800,"shares_system_memory":true,"read_age_s":300}],
	  "stats":{"stats_version":1,"sources":{"node":{"status":"ok","age_ms":500}},"memory":{"total_bytes":16000000000,"available_bytes":9961472000},
	    "gpus":[{"id":"pci:0000:00:01.0","vendor":"amd","kind":"integrated","mem_total_bytes":15032385536,"mem_used_bytes":5368709120,"mem_source":"drm_vram_gtt"}]}}}`
	// A laptop with an 8 GB Nvidia GPU and the nvidia exporter: 8 GiB total,
	// 5 GiB used, 465 MiB reserved, so 2607 MiB free. System RAM: 12700 MiB.
	gpuNode = `{"id":"g1","hostname":"laptop-a","free_mib":7000,"agent":{"mem_available_mib":12700,"platform":"linux-amd64-cuda",
	  "devices":[{"name":"CUDA0","backend":"cuda","total_mib":8151,"free_mib":7600,"shares_system_memory":false,"read_age_s":120}],
	  "stats":{"stats_version":1,"sources":{"node":{"status":"ok","age_ms":500},"nvidia":{"status":"ok","age_ms":400}},
	    "gpus":[{"id":"pci:0000:05:00.0","vendor":"amd","kind":"integrated","mem_total_bytes":4294967296,"mem_used_bytes":1073741824,"mem_source":"drm_vram_gtt"},
	      {"id":"nvidia:GPU-0","vendor":"nvidia","index":0,"kind":"discrete","mem_total_bytes":8589934592,"mem_used_bytes":5368709120,"mem_reserved_bytes":487587840,"mem_source":"nvidia_smi"}]}}}`
	// The same laptop with no nvidia exporter data.
	gpuNodeNoExporter = `{"id":"g2","hostname":"laptop-b","free_mib":7000,"agent":{"mem_available_mib":25000,"platform":"linux-amd64-cuda",
	  "devices":[{"name":"CUDA0","backend":"cuda","total_mib":8151,"free_mib":7600,"shares_system_memory":false}],
	  "stats":{"stats_version":1,"sources":{"node":{"status":"ok"},"nvidia":{"status":"absent"}},"gpus":[]}}}`
	localNode = `{"id":"h1","hostname":"host-1","local":true,"free_mib":5100}`
)

func TestStatusAvailableMemoryKinds(t *testing.T) {
	s, err := ParseStatus(statusDoc(consoleNode, gpuNode, gpuNodeNoExporter, localNode))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		v    int64
		kind string
		note bool
	}{
		"c1": {9500, MemKindSystem, false},
		"g1": {2607, MemKindGPU, false},
		"g2": {25000, MemKindSystem, true},
		"h1": {5100, MemKindGPU, false},
	}
	if len(s.Workers) != len(want) {
		t.Fatalf("workers %+v", s.Workers)
	}
	for _, w := range s.Workers {
		x := want[w.ID]
		if w.AvailMiB == nil || *w.AvailMiB != x.v || w.AvailKind != x.kind || (w.AvailNote != "") != x.note {
			t.Errorf("%s: avail %v kind %q note %q, want %d %s note %v", w.ID, deref(w.AvailMiB), w.AvailKind, w.AvailNote, x.v, x.kind, x.note)
		}
	}
}

// A stale or absent exporter, or a CUDA device whose index matches no entry,
// is not a live GPU figure.
func TestStatusGPUFigureNeedsALiveMatchingEntry(t *testing.T) {
	for name, node := range map[string]string{
		"stale":     strings.Replace(gpuNode, `"nvidia":{"status":"ok","age_ms":400}`, `"nvidia":{"status":"stale","age_ms":40000}`, 1),
		"other gpu": strings.Replace(gpuNode, `"name":"CUDA0"`, `"name":"CUDA1"`, 1),
		"no stats":  strings.Replace(gpuNode, `"stats":{`, `"stats_x":{`, 1),
	} {
		s, err := ParseStatus(statusDoc(node))
		if err != nil {
			t.Fatal(err)
		}
		w := s.Workers[0]
		if w.AvailKind != MemKindSystem || *w.AvailMiB != 12700 || w.AvailNote != MemNoteNoLiveGPU {
			t.Errorf("%s: %+v (avail %v)", name, w, deref(w.AvailMiB))
		}
	}
}

func deref(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// The watch keeps one kind per node; a GPU figure replaces system samples
// taken before it, and the lowest figures of the two kinds are kept apart.
func TestMemWatchKeepsKindsApart(t *testing.T) {
	w := newMemWatch()
	w.observe(&StatusInfo{Workers: []WorkerInfo{
		{ID: "g1", Hostname: "laptop-a", AvailMiB: i64(12000), AvailKind: MemKindSystem, AvailNote: MemNoteNoLiveGPU},
		{ID: "c1", Hostname: "ps5-a", AvailMiB: i64(9000), AvailKind: MemKindSystem},
	}})
	w.observe(&StatusInfo{Workers: []WorkerInfo{
		{ID: "g1", Hostname: "laptop-a", AvailMiB: i64(2600), AvailKind: MemKindGPU},
		{ID: "c1", Hostname: "ps5-a", AvailMiB: i64(4000)},
	}})
	w.observe(&StatusInfo{Workers: []WorkerInfo{{ID: "g1", AvailMiB: i64(1000), AvailKind: MemKindSystem}}}) // ignored: the node has a GPU figure
	got := w.result([]string{"c1", "g1"}, nil)
	if len(got) != 2 || got[0].Kind != MemKindSystem || *got[0].StepMiB != 4000 || got[1].Kind != MemKindGPU || *got[1].StepMiB != 2600 ||
		got[1].Samples != 1 || got[1].Note != "" {
		t.Fatalf("%+v", got)
	}
	st := &Step{Outcome: OutcomeReady, MemLow: got}
	if v := LowestMiB(st); v == nil || *v != 4000 {
		t.Fatalf("lowest system %v", deref(v))
	}
	if v := LowestGPUMiB(st); v == nil || *v != 2600 {
		t.Fatalf("lowest gpu %v", deref(v))
	}
	vals := stepValues(st)
	if vals["lowest_available_mib"] != 4000 || vals["lowest_available_gpu_mib"] != 2600 {
		t.Fatalf("values %v", vals)
	}
	// A row stored before kinds were recorded counts as system memory.
	old := &Step{MemLow: []NodeMemLow{{ID: "x", Node: "x", StepMiB: i64(3000)}}}
	if v := LowestMiB(old); v == nil || *v != 3000 || LowestGPUMiB(old) != nil {
		t.Fatal("an old row is not system memory")
	}
}

// The CSV has the GPU lowest in its own column and each node's figure with its kind.
func TestCSVMemoryKinds(t *testing.T) {
	r := &Run{ID: "r1", SuiteID: "s", Steps: []Step{{Idx: 0, BenchmarkID: "b", Profile: "p", State: StepDone, Outcome: OutcomeReady,
		MemLow: []NodeMemLow{{ID: "c1", Node: "ps5-a", StepMiB: i64(4000), Kind: MemKindSystem}, {ID: "g1", Node: "laptop-a", StepMiB: i64(2600), Kind: MemKindGPU}}}}}
	out, err := ExportCSV(r)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := csv.NewReader(strings.NewReader(out)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	col := map[string]int{}
	for i, h := range recs[0] {
		col[h] = i
	}
	if recs[1][col["lowest_available_mib"]] != "4000" || recs[1][col["lowest_available_gpu_mib"]] != "2600" ||
		recs[1][col["mem_low"]] != "ps5-a:system:4000;laptop-a:gpu:2600" {
		t.Fatalf("row %v", recs[1])
	}
}

// The trends of the two kinds are separate: the system trend leaves out the
// GPU figures, the GPU trend has only them.
func TestTrendKeepsKindsApart(t *testing.T) {
	s, _ := openTemp(t)
	if err := s.CreateRun(&Run{ID: "r1", SuiteID: "s", SuiteName: "S", State: RunCompleted, QueuedAt: 10, StartedAt: 10, Suite: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	st := &Step{RunID: "r1", BenchmarkID: "b", Profile: "p", State: StepDone, Outcome: OutcomeReady,
		MemLow: []NodeMemLow{{ID: "c1", Node: "ps5-a", StepMiB: i64(4000), Kind: MemKindSystem}, {ID: "g1", Node: "laptop-a", StepMiB: i64(2600), Kind: MemKindGPU}}}
	if err := s.AddStep(st); err != nil {
		t.Fatal(err)
	}
	for metric, want := range map[string]float64{"lowest_available_mib": 4000, "lowest_available_gpu_mib": 2600} {
		pts, err := s.Trend("b", metric, 10)
		if err != nil || len(pts) != 1 || pts[0].Value != want {
			t.Fatalf("%s: %+v %v", metric, pts, err)
		}
	}
}
