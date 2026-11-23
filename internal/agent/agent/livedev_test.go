package agent

import (
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// A discrete Nvidia device's free figure follows nvidia-smi (the exporter)
// at every heartbeat, idle or loaded, instead of the --list-devices read
// taken after the last launch; anything else keeps the list's figure.
func TestLiveDeviceFigures(t *testing.T) {
	age := int64(1500)
	idx0, idx1 := 0, 1
	const mib = 1 << 20
	st := agentproto.Stats{
		Sources: map[string]agentproto.Source{agentproto.SourceNvidia: {Status: agentproto.SourceOK, AgeMS: &age}},
		GPUs: []agentproto.GPUStat{
			{ID: "nvidia:a", Vendor: "nvidia", Index: &idx0, MemTotalBytes: 8192 * mib, MemUsedBytes: 5000 * mib, MemSource: agentproto.MemSourceNvidiaSMI},
			{ID: "nvidia:b", Vendor: "nvidia", Index: &idx1, MemTotalBytes: 8192 * mib, MemUsedBytes: 100 * mib, MemSource: agentproto.MemSourceNvidiaSMI},
		},
	}
	devs := []agentproto.Device{
		{Name: "CUDA0", Backend: "cuda", TotalMiB: 8192, FreeMiB: 7679, ReadAgeS: 871},
		{Name: "CUDA1", Backend: "cuda", TotalMiB: 8192, FreeMiB: 7679, ReadAgeS: 871, SharesSystemMemory: true}, // held to MemAvailable: as listed
		{Name: "Vulkan0", Backend: "vulkan", TotalMiB: 14336, FreeMiB: 14000, ReadAgeS: 871},
	}
	got := liveDevices(devs, st)
	if got[0].FreeMiB != 8192-5000 || got[0].ReadAgeS != 1.5 {
		t.Fatalf("CUDA0 %+v", got[0])
	}
	if got[1].FreeMiB != 7679 || got[2].FreeMiB != 14000 || got[2].ReadAgeS != 871 {
		t.Fatalf("others changed: %+v", got)
	}
	if devs[0].FreeMiB != 7679 {
		t.Fatal("the input was changed")
	}
	// No live source (exporter down or stale): the list's figure stays.
	st.Sources[agentproto.SourceNvidia] = agentproto.Source{Status: "absent"}
	if got := liveDevices(devs, st); got[0].FreeMiB != 7679 || got[0].ReadAgeS != 871 {
		t.Fatalf("%+v", got[0])
	}
}
