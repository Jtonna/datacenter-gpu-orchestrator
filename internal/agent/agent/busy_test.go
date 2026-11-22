package agent

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/agenttest"
)

func busyPath(t *testing.T) string { return filepath.Join(t.TempDir(), "busy") }

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func localStatus(t *testing.T, e *tenv) agentproto.Status {
	t.Helper()
	w := httptest.NewRecorder()
	e.a.LocalHandler().ServeHTTP(w, httptest.NewRequest("GET", agentproto.PathStatus, nil))
	var st agentproto.Status
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil || w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"busy":`) || !strings.Contains(w.Body.String(), `"busy_since":`) {
		t.Fatalf("status lacks the busy fields: %s", w.Body.String())
	}
	return st
}

// The busy file exists while a worker launch exists, with the count, and
// GET /status says the same; it goes when the launch is stopped.
func TestBusyFileFollowsTheLaunch(t *testing.T) {
	p := busyPath(t)
	e := newTenvWith(t, func(o *Options, d *Deps) { o.BusyFile = p })
	e.start()
	e.waitReady()
	if exists(p) {
		t.Fatal("an idle node has a busy file")
	}
	if st := localStatus(t, e); st.Busy || st.Launches != 0 || st.BusySince != nil {
		t.Fatalf("%+v", st)
	}
	before := time.Now().Unix()
	if w := e.do("POST", "/launch", workerReq("ln-1"), fromPotluck); w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	agenttest.Eventually(t, 5*time.Second, "busy file", func() bool { return exists(p) })
	b, _ := os.ReadFile(p)
	var n, since, updated, pid int64
	if _, err := fmt.Sscanf(string(b), "launches %d\nsince %d\nupdated %d\nagent_pid %d\n", &n, &since, &updated, &pid); err != nil ||
		n != 1 || since < before || updated < since || pid != int64(os.Getpid()) || !strings.HasSuffix(string(b), "\n") {
		t.Fatalf("%q %v", b, err)
	}
	if st := localStatus(t, e); !st.Busy || st.Launches != 1 || st.BusySince == nil || *st.BusySince != since {
		t.Fatalf("%+v", st)
	}
	if w := e.do("DELETE", "/launch/ln-1", nil, fromPotluck); w.Code >= 300 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	agenttest.Eventually(t, 15*time.Second, "busy file removed", func() bool { return !exists(p) })
	agenttest.Eventually(t, 2*time.Second, "status idle", func() bool { return !localStatus(t, e).Busy })
}

// A busy file left by an earlier agent process is removed at start; a launch
// that is adopted makes it again, with this process's pid.
func TestBusyFileLeftOverAtStart(t *testing.T) {
	p := busyPath(t)
	os.WriteFile(p, []byte("launches 1\nsince 1\nupdated 1\nagent_pid 1\n"), 0o644)
	e := newTenvWith(t, func(o *Options, d *Deps) { o.BusyFile = p })
	e.start()
	agenttest.Eventually(t, 5*time.Second, "left-over removed", func() bool { return !exists(p) })

	p2 := busyPath(t)
	os.WriteFile(p2, []byte("launches 1\nsince 1\nupdated 1\nagent_pid 1\n"), 0o644)
	sr := newSurvRunner("potluck-launch-ln-1")
	e2 := newTenvWith(t, func(o *Options, d *Deps) { d.Runner, o.BusyFile = sr, p2 })
	e2.leftLaunch(time.Hour)
	e2.start()
	want := fmt.Sprintf("agent_pid %d\n", os.Getpid())
	agenttest.Eventually(t, 5*time.Second, "busy file of the adopted launch", func() bool {
		b, _ := os.ReadFile(p2)
		return strings.HasPrefix(string(b), "launches 1\n") && strings.HasSuffix(string(b), want) && !strings.Contains(string(b), "since 1\n")
	})
}

// A busy file that cannot be written does not keep a launch from running.
func TestBusyFileUnwritableDoesNotBreakALaunch(t *testing.T) {
	p := filepath.Join(t.TempDir(), "no-such-dir", "busy")
	e := newTenvWith(t, func(o *Options, d *Deps) { o.BusyFile = p })
	e.start()
	e.waitReady()
	if w := e.do("POST", "/launch", workerReq("ln-1"), fromPotluck); w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var st agentproto.LaunchState
	agenttest.Eventually(t, 20*time.Second, "ready", func() bool {
		json.Unmarshal(e.do("GET", "/launch", nil, fromPotluck).Body.Bytes(), &st)
		return st.State == "ready"
	})
	if s := localStatus(t, e); !s.Busy || s.Launches != 1 {
		t.Fatalf("%+v", s)
	}
}

// A file cache eviction is not a launch: it makes no busy file.
func TestCacheEvictionIsNotBusy(t *testing.T) {
	p := busyPath(t)
	e := newTenvWith(t, func(o *Options, d *Deps) { o.BusyFile, d.FileCache = p, &cacheSys{} })
	e.start()
	e.waitReady()
	if w := e.do("POST", agentproto.PathCacheEvict, agentproto.CacheEvictRequest{File: "m.gguf"}, fromPotluck); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	time.Sleep(3 * busyEvery)
	if exists(p) || localStatus(t, e).Busy {
		t.Fatal("an eviction made the node busy")
	}
}
