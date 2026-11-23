package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/release"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/agenttest"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/progtree"
)

// Release 0.1.10 compatibility, both directions, with the real pool's
// registration rule (release comparison, agent_sha256) deciding every
// answer. The fake Potluck keeps serving the harness's programs; only the
// accept or refuse, the release and the agent sha come from the pool.

// compatPool is a pool that serves release rel, whose agent folder holds
// agentBin for this machine's os and arch.
func compatPool(t *testing.T, rel string, agentBin []byte) *pool.Pool {
	t.Helper()
	root := progtree.Write(t, t.TempDir(), progtree.Tree{Release: rel, Llama: "a01665d30de2885879a64eae999067ef3ef5fd4b",
		Folders: map[string]map[string][]byte{
			"agent":                {exeName("potluck-agent-" + goosArch()): agentBin},
			goosArch() + "-vulkan": {exeName("llama-server"): []byte("L"), exeName("rpc-server"): []byte("R")},
		}})
	r, err := release.Load(root, rel)
	if err != nil {
		t.Fatal(err)
	}
	d := config.DefaultDefaults()
	d.Membership = config.MembershipAgents
	cfg := &config.Config{Defaults: d}
	p, err := pool.New(pool.Options{Config: func() *config.Config { return cfg }, Programs: pool.Programs{Release: r},
		PotluckRelease: rel, Clock: time.Now, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// servedBy makes the fake Potluck answer as p decides. Call it inside
// e.pl.set.
func (e *tenv) servedBy(p *pool.Pool, rel string, base func(agentproto.RegisterRequest) agentproto.RegisterAnswer) {
	e.pl.answer = func(req agentproto.RegisterRequest) agentproto.RegisterAnswer {
		pa := p.Register(req)
		if !pa.Accepted {
			return pa
		}
		a := base(req)
		a.Release, a.AgentSHA256 = pa.Release, pa.AgentSHA256
		return a
	}
	e.pl.hbAnswer.Release = rel
}

func withRelease(rel string) func(*Options, *Deps) {
	return func(o *Options, _ *Deps) { o.Release = rel }
}

// A 0.1.9 agent (the field today) against a 0.1.10 Potluck: refused as too
// old with required_release 0.1.10, it asks its updater for 0.1.10 and
// nothing else (no programs, no heartbeat); the 0.1.10 agent it becomes is
// accepted by the same Potluck.
func TestCompat0110OldAgentUpdates(t *testing.T) {
	p := compatPool(t, "0.1.10", []byte("AGENT 0.1.10"))
	e := newTenvWith(t, withRelease("0.1.9"))
	base := e.pl.answer
	e.pl.set(func() { e.servedBy(p, "0.1.10", base) })
	e.start()
	agenttest.Eventually(t, 5*time.Second, "update asked", func() bool { return e.upd.n() >= 1 })
	if e.upd.calls[0] != "0.1.10" {
		t.Fatalf("update to %v", e.upd.calls)
	}
	st := e.a.Status()
	if st.Registered || st.LastError == nil || !hasPrefix(*st.LastError, agentproto.ReasonAgentTooOld) {
		t.Fatalf("%+v", st)
	}
	e.pl.mu.Lock()
	dl, beats := e.pl.downloads, len(e.pl.beats)
	e.pl.mu.Unlock()
	if dl != 0 || beats != 0 {
		t.Fatalf("a refused 0.1.9 agent fetched %d programs and sent %d heartbeats", dl, beats)
	}
	e.stop()

	// The updated agent: same machine, release 0.1.10.
	e2 := newTenvWith(t, withRelease("0.1.10"))
	base2 := e2.pl.answer
	e2.pl.set(func() { e2.servedBy(p, "0.1.10", base2) })
	e2.start()
	e2.waitReady()
	if !e2.a.Status().Registered || e2.upd.n() != 0 {
		t.Fatalf("0.1.10 agent: %+v, updates %v", e2.a.Status(), e2.upd.calls)
	}
}

// A 0.1.10 agent against a 0.1.9 Potluck (the scheduler before the switch):
// refused "Potluck too old", it never updates (agents never downgrade),
// fetches nothing and keeps asking; once Potluck serves 0.1.10 it joins.
func TestCompat0110NewAgentWaitsForPotluck(t *testing.T) {
	old := compatPool(t, "0.1.9", []byte("AGENT 0.1.9"))
	e := newTenvWith(t, withRelease("0.1.10"))
	base := e.pl.answer
	e.pl.set(func() { e.servedBy(old, "0.1.9", base) })
	e.start()
	agenttest.Eventually(t, 5*time.Second, "asks again", func() bool { return e.pl.nRegisters() >= 3 })
	st := e.a.Status()
	if st.Registered || st.LastError == nil || !hasPrefix(*st.LastError, agentproto.ReasonPotluckTooOld) {
		t.Fatalf("%+v", st)
	}
	e.pl.mu.Lock()
	dl, beats := e.pl.downloads, len(e.pl.beats)
	e.pl.mu.Unlock()
	if dl != 0 || beats != 0 || e.upd.n() != 0 {
		t.Fatalf("waiting agent: %d downloads, %d heartbeats, updates %v", dl, beats, e.upd.calls)
	}
	// The scheduler is replaced by the 0.1.10 one.
	cur := compatPool(t, "0.1.10", []byte("AGENT 0.1.10"))
	e.pl.set(func() { e.servedBy(cur, "0.1.10", base) })
	e.waitReady()
	if !e.a.Status().Registered || e.upd.n() != 0 {
		t.Fatalf("after the switch: %+v, updates %v", e.a.Status(), e.upd.calls)
	}
}

// Same-release re-fetch with the real updater: an idle agent whose binary
// is another build of 0.1.10 fetches the served one once; with a launch it
// does not; and it never loops, also when its binary keeps differing.
func TestCompat0110SameReleaseRefetch(t *testing.T) {
	served := []byte("AGENT 0.1.10 BUILD 2")
	p := compatPool(t, "0.1.10", served)
	var mu sync.Mutex
	downloads, restarts := 0, 0
	side := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/pool/release" {
			fmt.Fprintf(w, `{"release":"0.1.10","agents":{"%s":{"path":"/bin/agent","sha256":"%s"}}}`, goosArch(), shaOf(served))
			return
		}
		mu.Lock()
		downloads++
		mu.Unlock()
		w.Write(served)
	}))
	t.Cleanup(side.Close)
	counts := func() (int, int) { mu.Lock(); defer mu.Unlock(); return downloads, restarts }
	exe := filepath.Join(t.TempDir(), "potluck-agent")
	os.WriteFile(exe, []byte("AGENT 0.1.10 BUILD 1"), 0o755)
	// No Busy hook: the agent's own rule (no re-fetch with a launch) is
	// what is tested.
	e := newTenvWith(t, func(o *Options, d *Deps) {
		o.Release = "0.1.10"
		d.Updater = SelfUpdater{Base: side.URL, HTTP: side.Client(), Exe: exe,
			Restart: func(string) error {
				mu.Lock()
				restarts++
				mu.Unlock()
				// The installed binary is lost again (put back by hand, say):
				// the agent must still not fetch it a second time.
				return os.WriteFile(exe, []byte("AGENT 0.1.10 BUILD 1"), 0o755)
			}}
	})
	base := e.pl.answer
	e.pl.set(func() { e.servedBy(p, "0.1.10", base) })

	// 1. With a launch at registration: no re-fetch.
	e.a.RecoverLaunch(context.Background())
	e.start()
	e.waitReady()
	lr := workerReq("ln-1")
	lr.Release = "0.1.10"
	if w := e.do("POST", "/launch", lr, fromPotluck); w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// The first registration was idle: one re-fetch already. Reset the
	// attempt to see the launch rule on its own.
	agenttest.Eventually(t, 5*time.Second, "first re-fetch", func() bool { d, _ := counts(); return d == 1 })
	os.Remove(exe + ".update-attempt")
	regs := e.pl.nRegisters()
	e.pl.set(func() { e.pl.hbStatus = 410 })
	agenttest.Eventually(t, 5*time.Second, "re-registers with the launch", func() bool { return e.pl.nRegisters() >= regs+2 })
	e.pl.set(func() { e.pl.hbStatus = 200 })
	if d, r := counts(); d != 1 || r != 1 {
		t.Fatalf("with a launch: %d downloads, %d restarts", d, r)
	}

	// 2. Idle again: fetched once.
	e.a.Launch.Stop(*e.a.Launch.CurrentID(), true)
	agenttest.Eventually(t, 10*time.Second, "launch ended", func() bool { return e.a.Launch.CurrentID() == nil })
	regs = e.pl.nRegisters()
	e.pl.set(func() { e.pl.hbStatus = 410 })
	agenttest.Eventually(t, 5*time.Second, "re-fetch when idle", func() bool { d, _ := counts(); return d == 2 })

	// 3. No loop: many more registrations, binary still differing.
	agenttest.Eventually(t, 5*time.Second, "many registrations", func() bool { return e.pl.nRegisters() >= regs+10 })
	e.pl.set(func() { e.pl.hbStatus = 200 })
	if d, r := counts(); d != 2 || r != 2 {
		t.Fatalf("looped: %d downloads, %d restarts", d, r)
	}

	// 4. A Docker agent's updater is not a re-fetcher: never asked.
	if _, ok := any(DockerUpdater{}).(Refetcher); ok {
		t.Fatal("DockerUpdater re-fetches")
	}
}

func hasPrefix(s, reason string) bool { return len(s) > len(reason) && s[:len(reason)+1] == reason+":" }

// exeName is a program's file name on this machine's os.
func exeName(n string) string {
	if runtimeGOOS == "windows" {
		return n + ".exe"
	}
	return n
}
