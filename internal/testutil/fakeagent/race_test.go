package fakeagent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// The setters run while heartbeats and registrations are marshalled: under
// -race (the Linux gate) this fails if a body sent shares memory with what a
// setter writes. Without -race it checks the figures arrive.
func TestSettersWhileHeartbeating(t *testing.T) {
	var beats atomic.Int64
	var mu sync.Mutex
	var lastAvail int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, agentproto.PathRegister) {
			json.NewEncoder(w).Encode(agentproto.RegisterAnswer{Accepted: true})
			return
		}
		var hb struct {
			MemAvail int64               `json:"mem_available_mib"`
			Devices  []agentproto.Device `json:"devices"`
		}
		json.Unmarshal(b, &hb)
		mu.Lock()
		lastAvail = hb.MemAvail
		mu.Unlock()
		beats.Add(1)
		json.NewEncoder(w).Encode(agentproto.HeartbeatAnswer{})
	}))
	t.Cleanup(srv.Close)
	a := &Agent{ID: "02:00:00:00:00:01", Name: "n1", Addr: "127.0.0.1", Potluck: srv.URL, HeartbeatEvery: time.Millisecond,
		GPUs:    []agentproto.GPU{{Vendor: "amd", Integrated: true}},
		Devices: []agentproto.Device{{Name: "Vulkan0", Backend: "vulkan", TotalMiB: 14336, FreeMiB: 14000, SharesSystemMemory: true}}}
	devices := a.Devices // the caller's slice: a setter must not write into it
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)

	stop := time.Now().Add(500 * time.Millisecond)
	for i := int64(0); time.Now().Before(stop) || beats.Load() < 20; i++ {
		a.SetMemAvail(1000+i%5000, json.RawMessage(`{"stats_version":1}`))
		a.SetHeartbeats(true)
		a.SilentFor(0)
		_ = a.LaunchState()
		if time.Now().After(stop.Add(10 * time.Second)) {
			t.Fatalf("only %d heartbeats in 10 s", beats.Load())
		}
	}
	a.SetMemAvail(4321, nil)
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		got := lastAvail
		mu.Unlock()
		if got == 4321 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the last figure never arrived (last %d)", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if devices[0].FreeMiB != 14000 {
		t.Fatalf("the caller's Devices slice was written: %d", devices[0].FreeMiB)
	}
}
