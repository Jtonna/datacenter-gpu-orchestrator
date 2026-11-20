package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/osx"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/runner"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/agenttest"
)

// ---- a runner whose units survive the agent (systemd), faked ----

type survProc struct {
	unit string
	r    *survRunner
	mu   sync.Mutex
	sigs []osx.Sig
	done chan struct{}
}

func (p *survProc) Unit() string          { return p.unit }
func (p *survProc) PID() int              { return 4242 }
func (p *survProc) Done() <-chan struct{} { return p.done }
func (p *survProc) Exit() runner.Exit {
	<-p.done
	return runner.Exit{Known: true, Code: 137, Signal: "KILL"}
}
func (p *survProc) Signal(s osx.Sig) error {
	p.mu.Lock()
	p.sigs = append(p.sigs, s)
	p.mu.Unlock()
	if p.r != nil && p.r.isStubborn(p.unit) {
		if s == osx.SigKill {
			return errors.New("systemctl kill: failed to connect to bus")
		}
		return nil // SIGTERM is ignored
	}
	p.finish()
	return nil
}
func (p *survProc) finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.done:
	default:
		close(p.done)
	}
}
func (p *survProc) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}
func (p *survProc) signals() []osx.Sig {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]osx.Sig(nil), p.sigs...)
}

type survRunner struct {
	mu        sync.Mutex
	procs     map[string]*survProc
	stopped   []string
	adoptErrs int
	// stubborn units ignore SIGTERM, and systemctl kill and stop fail on them.
	stubborn map[string]bool
	// launchGate, when set, holds the start of a launch unit until it is
	// closed; launchEntered tells that a start is waiting.
	launchGate    chan struct{}
	launchEntered chan struct{}
}

func newSurvRunner(units ...string) *survRunner {
	r := &survRunner{procs: map[string]*survProc{}, stubborn: map[string]bool{}}
	for _, u := range units {
		r.revive(u)
	}
	return r
}

// revive puts a live unit of that name in place, as an agent that died
// without stopping it leaves it.
func (r *survRunner) revive(unit string) *survProc {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := &survProc{unit: unit, r: r, done: make(chan struct{})}
	r.procs[unit] = p
	return p
}

func (r *survRunner) proc(unit string) *survProc {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.procs[unit]
}

func (r *survRunner) isStubborn(unit string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stubborn[unit]
}

func (r *survRunner) setStubborn(unit string) { r.mu.Lock(); r.stubborn[unit] = true; r.mu.Unlock() }

func (r *survRunner) Start(ctx context.Context, s runner.Spec) (runner.Proc, error) {
	r.mu.Lock()
	gate, entered := r.launchGate, r.launchEntered
	if old, ok := r.procs[s.Unit]; ok && old.alive() {
		r.mu.Unlock()
		return nil, runner.ErrExists
	}
	r.mu.Unlock()
	var err error
	if gate != nil && strings.HasPrefix(s.Unit, agentproto.LaunchUnitPrefix) {
		entered <- struct{}{}
		select {
		case <-gate:
		case <-ctx.Done():
			// systemd-run is killed with its context, but systemd has the
			// job: the unit starts all the same.
			err = ctx.Err()
		}
	}
	p := r.revive(s.Unit)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (r *survRunner) Adopt(ctx context.Context, unit, exitPath string) (runner.Proc, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.adoptErrs > 0 {
		r.adoptErrs--
		return nil, errors.New("systemctl show: failed to connect to bus")
	}
	if p, ok := r.procs[unit]; ok {
		return p, nil
	}
	return nil, nil
}

func (r *survRunner) List(ctx context.Context, prefix string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for u, p := range r.procs {
		if p.alive() && strings.HasPrefix(u, prefix) {
			out = append(out, u)
		}
	}
	return out, nil
}

func (r *survRunner) Stop(ctx context.Context, unit string) error {
	r.mu.Lock()
	r.stopped = append(r.stopped, unit)
	p, stubborn := r.procs[unit], r.stubborn[unit]
	r.mu.Unlock()
	if stubborn {
		return errors.New("systemctl stop: failed to connect to bus")
	}
	if p != nil {
		p.finish()
	}
	return nil
}
func (r *survRunner) Kind() string   { return "fake" }
func (r *survRunner) Survives() bool { return true }

func (r *survRunner) wasStopped(unit string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, u := range r.stopped {
		if u == unit {
			return true
		}
	}
	return false
}

// leftLaunch writes the state of a ready worker launch ln-1 as an earlier
// agent process left it, with its lease ending leaseIn from now.
func (e *tenv) leftLaunch(leaseIn time.Duration) {
	e.t.Helper()
	dir := filepath.Join(e.stateDir, "launch")
	os.MkdirAll(dir, 0o755)
	now := int64(e.clock.Now())
	js := fmt.Sprintf(`{"launch_id":"ln-1","role":"worker","label":"m.gguf","release":"0.1.0","port":%d,
"command":["rpc-server"],"build":"a01665d30","unit":"potluck-launch-ln-1","log_path":%q,"exit_path":%q,"lease_path":%q,
"started_at_boottime_ns":%d,"lease_ttl_ns":20000000000,"ready_at_boottime_ns":%d}`,
		freePort(e.t), filepath.Join(dir, "ln-1.log"), filepath.Join(dir, "ln-1.exit"), filepath.Join(dir, "ln-1.lease"), now-int64(time.Minute), now-int64(time.Minute))
	if err := os.WriteFile(filepath.Join(dir, "ln-1.json"), []byte(js), 0o644); err != nil {
		e.t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "ln-1.lease"), []byte(fmt.Sprint(now+int64(leaseIn))), 0o644)
}

type noNet struct{ fakeNet }

func (noNet) SourceFor(net.IP) (net.IP, error) { return nil, errors.New("network is unreachable") }

// Item 1: a launch left by a dead agent gets its lease enforced although the
// identity never resolves, so nothing is bound, checked or registered.
func TestLaunchRecoveredWithoutIdentity(t *testing.T) {
	sr := newSurvRunner("potluck-launch-ln-1")
	e := newTenvWith(t, func(o *Options, d *Deps) { d.Runner, d.Net = sr, noNet{} })
	e.leftLaunch(-time.Second) // the lease ended while the agent was away
	e.start()
	p := sr.proc("potluck-launch-ln-1")
	agenttest.Eventually(t, 5*time.Second, "lease expiry stops the launch", func() bool { return len(p.signals()) >= 1 })
	if e.pl.nRegisters() != 0 {
		t.Fatal("registered without an identity")
	}
	if w := e.do("GET", "/launch", nil, fromPotluck); w.Code != 200 || !strings.Contains(w.Body.String(), `"launch_id":"ln-1"`) {
		t.Fatal(w.Code, w.Body.String())
	}
}

// Item 1: exporter units of the dead agent hold the exporter port; they are
// stopped before the port check, and the registration names the launch.
func TestLeftoverExportersStoppedBeforePortCheck(t *testing.T) {
	sr := newSurvRunner("potluck-launch-ln-1", agentproto.NodeExporterUnit)
	e := newTenvWith(t, func(o *Options, d *Deps) {
		d.Runner = sr
		d.PortFree = func(addr string, port int) error {
			if !sr.wasStopped(agentproto.NodeExporterUnit) {
				return errors.New("address already in use")
			}
			return nil
		}
	})
	e.leftLaunch(time.Hour)
	e.start()
	agenttest.Eventually(t, 5*time.Second, "registration", func() bool { return e.pl.nRegisters() >= 1 })
	if id := e.pl.lastRegister().LaunchID; id == nil || *id != "ln-1" {
		t.Fatalf("registration does not name the running launch: %v", id)
	}
	if len(sr.proc("potluck-launch-ln-1").signals()) != 0 {
		t.Fatal("the running launch was signalled")
	}
}

// Item 3: until the re-adoption is done the API answers 503, never "none",
// and the agent does not register.
func TestNoLaunchStateBeforeRecovery(t *testing.T) {
	sr := newSurvRunner("potluck-launch-ln-1")
	sr.adoptErrs = 3
	e := newTenvWith(t, func(o *Options, d *Deps) { d.Runner, d.RecoverRetry = sr, 150*time.Millisecond })
	e.leftLaunch(time.Hour)
	if w := e.do("GET", "/launch", nil, fromPotluck); w.Code != 503 || !strings.Contains(w.Body.String(), ReasonStarting) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := e.do("POST", "/launch", workerReq("ln-2"), fromPotluck); w.Code != 503 {
		t.Fatal(w.Code)
	}
	e.start()
	for i := 0; i < 500; i++ {
		n := e.pl.nRegisters()
		w := e.do("GET", "/launch", nil, fromPotluck)
		if w.Code == 503 {
			if n != 0 {
				t.Fatal("registered before the launch was re-adopted")
			}
			time.Sleep(5 * time.Millisecond)
			continue
		}
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"ready"`) {
			t.Fatal(w.Code, w.Body.String())
		}
		agenttest.Eventually(t, 5*time.Second, "registration", func() bool { return e.pl.nRegisters() >= 1 })
		if id := e.pl.lastRegister().LaunchID; id == nil || *id != "ln-1" {
			t.Fatalf("registration does not name the launch: %v", id)
		}
		return
	}
	t.Fatal("never recovered")
}

type nodeNet struct{ fakeNet }

func (nodeNet) SourceFor(net.IP) (net.IP, error) { return net.ParseIP("192.0.2.7"), nil }

// Item 5: an address of port 50060 that cannot be bound is tried again
// inside the process; the agent neither exits nor registers meanwhile.
func TestBindRetried(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	fail := map[string]int{"127.0.0.1:50060": 2, "192.0.2.7:50060": 2}
	listen := func(addr string) (net.Listener, error) {
		mu.Lock()
		defer mu.Unlock()
		calls[addr]++
		if fail[addr] > 0 {
			fail[addr]--
			return nil, errors.New("bind: address already in use")
		}
		return net.Listen("tcp", "127.0.0.1:0")
	}
	sr := newSurvRunner("potluck-launch-ln-1")
	e := newTenvWith(t, func(o *Options, d *Deps) {
		d.Runner, d.Net, d.Listen, d.BindRetry = sr, nodeNet{}, listen, 20*time.Millisecond
	})
	e.leftLaunch(-time.Second)
	e.start()
	// The launch's lease is enforced while the port is still taken.
	agenttest.Eventually(t, 5*time.Second, "lease expiry", func() bool { return len(sr.proc("potluck-launch-ln-1").signals()) >= 1 })
	agenttest.Eventually(t, 5*time.Second, "registration", func() bool { return e.pl.nRegisters() >= 1 })
	mu.Lock()
	defer mu.Unlock()
	if calls["127.0.0.1:50060"] != 3 || calls["192.0.2.7:50060"] != 3 {
		t.Fatalf("binds %v", calls)
	}
}

// Item 6: a binary that reports another release than the one it is served
// as is not installed, and not downloaded a second time.
func TestSelfUpdaterMisStampedBinary(t *testing.T) {
	body := []byte("AGENT STAMPED 0.1.0")
	sum := sha256.Sum256(body)
	downloads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/pool/release" {
			fmt.Fprintf(w, `{"release":"0.2.0","agents":{"%s":{"path":"/bin/agent","sha256":"%s"}}}`, goosArch(), hex.EncodeToString(sum[:]))
			return
		}
		downloads++
		w.Write(body)
	}))
	defer srv.Close()
	dir := t.TempDir()
	exe := filepath.Join(dir, "potluck-agent")
	os.WriteFile(exe, []byte("OLD"), 0o755)
	restarts := 0
	u := SelfUpdater{Base: srv.URL, HTTP: srv.Client(), Exe: exe, AttemptPath: filepath.Join(dir, "attempt"),
		Restart: func(string) error { restarts++; return nil },
		Version: func(context.Context, string) (string, error) { return "0.1.0", nil }}
	if err := u.Update(context.Background(), "0.2.0"); err == nil || errors.Is(err, ErrUpdateNotRepeated) {
		t.Fatalf("mis-stamped binary: %v", err)
	}
	if err := u.Update(context.Background(), "0.2.0"); !errors.Is(err, ErrUpdateNotRepeated) {
		t.Fatalf("second attempt: %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "OLD" || restarts != 0 || downloads != 1 {
		t.Fatalf("exe %q, restarts %d, downloads %d", b, restarts, downloads)
	}
	if _, err := os.Stat(exe + ".part"); !os.IsNotExist(err) {
		t.Fatal("part file left")
	}
}

// Item 6: an update that was installed and restarted, and after which the
// agent is still refused, is not done again (no download and exec every
// 60 s); a launch that appeared during the download stops the update.
func TestSelfUpdaterNotRepeatedAndNeverWithLaunch(t *testing.T) {
	body := []byte("NEW AGENT")
	sum := sha256.Sum256(body)
	downloads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/pool/release" {
			fmt.Fprintf(w, `{"release":"0.2.0","agents":{"%s":{"path":"/bin/agent","sha256":"%s"}}}`, goosArch(), hex.EncodeToString(sum[:]))
			return
		}
		downloads++
		w.Write(body)
	}))
	defer srv.Close()
	dir := t.TempDir()
	exe := filepath.Join(dir, "potluck-agent")
	os.WriteFile(exe, []byte("OLD"), 0o755)
	restarts, busy := 0, true
	u := SelfUpdater{Base: srv.URL, HTTP: srv.Client(), Exe: exe, AttemptPath: filepath.Join(dir, "attempt"),
		Restart: func(string) error { restarts++; return nil }, Busy: func() bool { return busy },
		Version: func(context.Context, string) (string, error) { return "0.2.0", nil }}
	if err := u.Update(context.Background(), "0.2.0"); err == nil || errors.Is(err, ErrUpdateNotRepeated) {
		t.Fatalf("update with a launch: %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "OLD" || restarts != 0 {
		t.Fatalf("replaced during a launch: %q %d", b, restarts)
	}
	busy = false // the launch ended: the update is done now
	if err := u.Update(context.Background(), "0.2.0"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "NEW AGENT" || restarts != 1 || downloads != 2 {
		t.Fatalf("exe %q, restarts %d, downloads %d", b, restarts, downloads)
	}
	// The restarted agent is refused again with the same release.
	if err := u.Update(context.Background(), "0.2.0"); !errors.Is(err, ErrUpdateNotRepeated) {
		t.Fatalf("repeated update: %v", err)
	}
	if restarts != 1 || downloads != 2 {
		t.Fatalf("restarts %d, downloads %d", restarts, downloads)
	}
}
