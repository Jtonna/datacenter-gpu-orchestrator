package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/modelsrc"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/agenttest"
)

// cacheSys pretends the whole file is cached until it is dropped.
type cacheSys struct {
	mu      sync.Mutex
	dropped bool
}

func (s *cacheSys) Cached(f *os.File, size int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dropped {
		return 0, nil
	}
	return size, nil
}

func (s *cacheSys) Drop(f *os.File, size int64) error {
	s.mu.Lock()
	s.dropped = true
	s.mu.Unlock()
	return nil
}

func TestCacheEvictRequest(t *testing.T) {
	e := newTenvWith(t, func(o *Options, d *Deps) { d.FileCache = &cacheSys{} })
	e.a.RecoverLaunch(context.Background())
	if req := e.a.RegisterRequest(); !req.HasCapability(agentproto.CapFileCacheEvict) {
		t.Fatalf("capabilities %v", req.Capabilities)
	}
	w := e.do("POST", agentproto.PathCacheEvict, agentproto.CacheEvictRequest{File: "m.gguf"}, fromPotluck)
	var ans agentproto.CacheEvictAnswer
	json.Unmarshal(w.Body.Bytes(), &ans)
	if w.Code != 200 || ans.File != "m.gguf" || ans.FileBytes != 4 || ans.CachedBefore != 4 || ans.CachedAfter != 0 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// at most one eviction every 10 s (on the agent's own clock)
	w = e.do("POST", agentproto.PathCacheEvict, agentproto.CacheEvictRequest{File: "m.gguf"}, fromPotluck)
	if w.Code != 429 || !strings.Contains(w.Body.String(), `"too_soon"`) || w.Header().Get("Retry-After") == "" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	e.clock.Advance(10 * time.Second)
	for name, want := range map[string]int{"../m.gguf": 400, "/etc/passwd": 400, "missing.gguf": 404} {
		if w := e.do("POST", agentproto.PathCacheEvict, agentproto.CacheEvictRequest{File: name}, fromPotluck); w.Code != want {
			t.Errorf("%q: %d %s", name, w.Code, w.Body.String())
		}
	}
	if w := e.do("POST", agentproto.PathCacheEvict, agentproto.CacheEvictRequest{File: "m.gguf"}, "192.0.2.50:1"); w.Code != 403 {
		t.Fatalf("another address: %d", w.Code)
	}
}

func TestCacheEvictNotAdvertisedWithoutTheCalls(t *testing.T) {
	for name, mod := range map[string]func(*Options, *Deps){
		"no calls": nil,
		"push nodes": func(o *Options, d *Deps) {
			d.FileCache = &cacheSys{}
			o.ModelSource = modelsrc.Spec{Kind: modelsrc.KindPush}
		},
	} {
		e := newTenvWith(t, mod)
		e.a.RecoverLaunch(context.Background())
		if req := e.a.RegisterRequest(); req.HasCapability(agentproto.CapFileCacheEvict) || len(req.Capabilities) != 0 {
			t.Fatalf("%s: capabilities %v", name, req.Capabilities)
		}
		b, _ := json.Marshal(e.a.RegisterRequest())
		if strings.Contains(string(b), "capabilities") {
			t.Fatalf("%s: %s", name, b)
		}
		if w := e.do("POST", agentproto.PathCacheEvict, agentproto.CacheEvictRequest{File: "m.gguf"}, fromPotluck); w.Code != 501 || !strings.Contains(w.Body.String(), "unsupported") {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
}

func TestCacheEvictRefusedWhileALaunchRuns(t *testing.T) {
	sys := &cacheSys{}
	e := newTenvWith(t, func(o *Options, d *Deps) { d.FileCache = sys })
	e.start()
	e.waitReady()
	if w := e.do("POST", "/launch", workerReq("ln-1"), fromPotluck); w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	w := e.do("POST", agentproto.PathCacheEvict, agentproto.CacheEvictRequest{File: "m.gguf"}, fromPotluck)
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"busy"`) || !strings.Contains(w.Body.String(), "ln-1") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if sys.dropped {
		t.Fatal("the file was dropped while a launch ran")
	}
	if w := e.do("DELETE", "/launch/ln-1?force=1", nil, fromPotluck); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	agenttest.Eventually(t, 20*time.Second, "launch gone", func() bool { return e.a.Launch.CurrentID() == nil })
	if w := e.do("POST", agentproto.PathCacheEvict, agentproto.CacheEvictRequest{File: "m.gguf"}, fromPotluck); w.Code != 200 {
		t.Fatalf("after the launch: %d %s", w.Code, w.Body.String())
	}
}
