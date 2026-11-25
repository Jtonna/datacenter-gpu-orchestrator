package instance

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

type stopRec struct {
	mu    sync.Mutex
	modes []StopMode
}

func (s *stopRec) stop(m StopMode) { s.mu.Lock(); s.modes = append(s.modes, m); s.mu.Unlock() }
func (s *stopRec) got() []StopMode { s.mu.Lock(); defer s.mu.Unlock(); return slices.Clone(s.modes) }

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	end := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(end) {
			t.Fatal("timed out waiting for " + what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRemoteChildAddressKeyAndLog(t *testing.T) {
	c := NewRemoteChild("192.0.2.11:50061", []string{"llama-server", "--port", "50061"}, "k0123456789abcdef", func(StopMode) {}, nil)
	if AddrOf(c) != "192.0.2.11:50061" || c.Port() != 50061 || c.PID() != 0 || APIKeyOf(c) != "k0123456789abcdef" {
		t.Fatal(AddrOf(c), c.Port(), c.PID(), APIKeyOf(c))
	}
	// The output arrives in pieces from the copied launch log.
	c.Feed([]byte("load_tensors:      Vulkan0 model buffer size =  2000.50 MiB\nload_tensors: RPC0[192.0.2.12:50052] model buf"))
	c.Feed([]byte("fer size =  1000.00 MiB\nllama_context:  Vulkan_Host compute buffer size =  40.00 MiB\n"))
	c.Feed([]byte("llama_kv_cache:    Vulkan0 KV buffer size =   512.00 MiB\n"))
	ld := c.Load()
	if ld.TotalMiB() != 3512.5 || ld.HostMiB() != 40 {
		t.Fatalf("%+v", ld)
	}
	if tail := c.Tail(2); len(tail) != 2 || !strings.Contains(tail[1], "KV buffer") {
		t.Fatal(tail)
	}
	if c.Argv()[0] != "llama-server" {
		t.Fatal(c.Argv())
	}
}

func TestRemoteChildHealth(t *testing.T) {
	t.Run("ready then health ok", func(t *testing.T) {
		var asked []string
		c := NewRemoteChild("192.0.2.11:50061", nil, "", func(StopMode) {}, func(_ context.Context, url string) error {
			asked = append(asked, url)
			return nil
		})
		go c.SetReady()
		if err := c.WaitHealthy(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(asked) != 1 || asked[0] != "http://192.0.2.11:50061/health" {
			t.Fatal(asked)
		}
	})
	t.Run("ready but not reachable from the scheduler", func(t *testing.T) {
		var s stopRec
		c := NewRemoteChild("192.0.2.11:50061", nil, "", s.stop, func(context.Context, string) error { return errors.New("connection refused") })
		c.HealthTries, c.HealthEvery = 3, time.Millisecond
		c.SetReady()
		err := c.WaitHealthy(context.Background())
		if !errors.Is(err, ErrHostUnreachable) {
			t.Fatal(err)
		}
		waitUntil(t, "kill", func() bool { return slices.Equal(s.got(), []StopMode{StopKill}) })
		if tail := c.Tail(1); len(tail) != 1 || !strings.Contains(tail[0], "host_unreachable") {
			t.Fatal(tail)
		}
		c.Finish(ExitStatus{Code: -1, Signal: "KILL", Reason: "forced"})
		st := <-c.Exited()
		if !st.Requested || !strings.Contains(strings.Join(st.Tail, "\n"), "host_unreachable") {
			t.Fatalf("%+v", st)
		}
	})
	t.Run("exits before ready", func(t *testing.T) {
		c := NewRemoteChild("192.0.2.11:50061", nil, "", func(StopMode) {}, nil)
		go c.Finish(ExitStatus{Code: 1, Reason: "crashed: segfault"})
		err := c.WaitHealthy(context.Background())
		if !errors.Is(err, ErrChildExited) || !strings.Contains(err.Error(), "crashed: segfault") {
			t.Fatal(err)
		}
		st := <-c.Exited()
		if st.Requested || st.String() != "exit code 1, crashed: segfault" {
			t.Fatalf("%q", st.String())
		}
		if _, open := <-c.Exited(); open {
			t.Fatal("Exited must close after one status")
		}
	})
	t.Run("context ends", func(t *testing.T) {
		c := NewRemoteChild("192.0.2.11:50061", nil, "", func(StopMode) {}, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := c.WaitHealthy(ctx); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}

func TestRemoteChildStop(t *testing.T) {
	var s stopRec
	c := NewRemoteChild("192.0.2.11:50061", nil, "", s.stop, nil)
	c.Stop(StopGraceful)
	c.Stop(StopGraceful) // repeated: nothing more
	c.Stop(StopKill)     // escalates
	c.Stop(StopKill)
	waitUntil(t, "two stops", func() bool { return len(s.got()) == 2 })
	time.Sleep(20 * time.Millisecond)
	if !slices.Equal(s.got(), []StopMode{StopGraceful, StopKill}) {
		t.Fatal(s.got())
	}
	c.Finish(ExitStatus{Code: 0, Reason: "stopped"})
	c.Finish(ExitStatus{Code: 9}) // ignored
	c.Stop(StopKill)              // after the end: nothing
	st := <-c.Exited()
	if !st.Requested || st.Code != 0 {
		t.Fatalf("%+v", st)
	}
	time.Sleep(20 * time.Millisecond)
	if len(s.got()) != 2 {
		t.Fatal(s.got())
	}
}

// The host launch's extra_args are BuildArgs' own flags without what depends
// on where the program runs (design 8.1 item 5): the agent adds --host,
// --port, the model, the context, --rpc, --device and --tensor-split; a host
// on a node has no prompt cache in RAM and may load without mmap.
func TestHostLaunchArgsMatchTheServerCommand(t *testing.T) {
	p := core.Profile{Name: "x", Kind: core.KindLlama, Model: "m.gguf", Ctx: 8192, Parallel: 2, KVQuant: "q8_0", FlashAttn: true,
		ExtraArgs: []string{"--temp", "0.7"}}
	pl := core.Placement{Nodes: []core.PlacedNode{{ID: "a", Addr: "192.0.2.12:50052", BudgetMiB: 1000}, {ID: "b", Addr: "192.0.2.13:50052", BudgetMiB: 1000}}}
	full, err := BuildArgs(p, pl, 9000, BuildOptions{ServerBin: "llama-server", ModelsDir: "/models", CacheRAMMiB: 2048, CtxCheckpoints: 32})
	if err != nil {
		t.Fatal(err)
	}
	placed := map[string]bool{"--host": true, "--port": true, "--model": true, "--ctx-size": true, "--parallel": true,
		"--rpc": true, "--device": true, "--tensor-split": true}
	var rest []string
	for i := 1; i < len(full); i++ {
		if placed[full[i]] {
			i++
			continue
		}
		rest = append(rest, full[i])
	}
	i := slices.Index(rest, "--cache-ram")
	rest[i+1] = "0"
	if got := HostLaunchArgs(p, HostOptions{CtxCheckpoints: 32}); !slices.Equal(got, rest) {
		t.Fatalf("\n got %v\nwant %v", got, rest)
	}
	withNoMmap := HostLaunchArgs(p, HostOptions{NoMmap: true, CtxCheckpoints: 32})
	if slices.Index(withNoMmap, "--no-mmap") < 0 || len(withNoMmap) != len(rest)+1 {
		t.Fatal(withNoMmap)
	}
	// None of the agent's own flags.
	for _, a := range withNoMmap {
		if placed[a] || a == "-m" || a == "-c" || a == "--api-key" {
			t.Fatal(a)
		}
	}
}
