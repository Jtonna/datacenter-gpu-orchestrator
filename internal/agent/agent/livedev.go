package agent

import (
	"strconv"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// liveDevices returns devs with the free figure of each discrete Nvidia
// device (backend cuda, CUDA<n>, not held to MemAvailable) taken from the
// nvidia exporter's latest scrape: total less used of the GPU whose
// nvidia-smi index is n (launches run with CUDA_DEVICE_ORDER=PCI_BUS_ID, so
// CUDA<n> is that GPU), and read_age_s the scrape's age. --list-devices is
// read only after the programs are verified and after a launch ends, so
// without this the figure would stay the one from before a load for as long
// as the load runs, and idle for as long as nothing launches. Only a source
// that is ok counts; otherwise the listed figure stays. devs is not changed.
func liveDevices(devs []agentproto.Device, st agentproto.Stats) []agentproto.Device {
	out := append([]agentproto.Device(nil), devs...)
	src, ok := st.Sources[agentproto.SourceNvidia]
	if !ok || src.Status != agentproto.SourceOK || src.AgeMS == nil {
		return out
	}
	for i, d := range out {
		if d.Backend != agentproto.BackendCUDA || d.SharesSystemMemory || !strings.HasPrefix(d.Name, "CUDA") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(d.Name, "CUDA"))
		if err != nil {
			continue
		}
		for _, g := range st.GPUs {
			if g.Vendor != "nvidia" || g.MemSource != agentproto.MemSourceNvidiaSMI || g.Index == nil || *g.Index != n || g.MemTotalBytes <= 0 {
				continue
			}
			free := (g.MemTotalBytes - g.MemUsedBytes) >> 20
			out[i].FreeMiB = min(max(free, 0), max(d.TotalMiB, 0))
			if d.TotalMiB <= 0 {
				out[i].FreeMiB = max(free, 0)
			}
			out[i].ReadAgeS = float64(*src.AgeMS) / 1000
			break
		}
	}
	return out
}
