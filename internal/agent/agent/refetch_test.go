package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/agenttest"
)

func shaOf(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// Another build of the release the agent runs: the registration answer names
// the served agent's sha256, and an agent whose own binary differs fetches
// it once (5.4), behind the same once-per-(release, sha) guard as an update.
func TestSelfUpdaterRefetchSameRelease(t *testing.T) {
	served := []byte("AGENT 0.1.9 BUILD 2")
	downloads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/pool/release" {
			fmt.Fprintf(w, `{"release":"0.1.9","agents":{"%s":{"path":"/bin/agent","sha256":"%s"}}}`, goosArch(), shaOf(served))
			return
		}
		downloads++
		w.Write(served)
	}))
	defer srv.Close()
	exe := filepath.Join(t.TempDir(), "potluck-agent")
	restarts := 0
	u := SelfUpdater{Base: srv.URL, HTTP: srv.Client(), Exe: exe, Restart: func(string) error { restarts++; return nil }}

	// The same build: nothing is fetched.
	os.WriteFile(exe, served, 0o755)
	if err := u.Refetch(context.Background(), "0.1.9", shaOf(served)); err != nil || downloads != 0 || restarts != 0 {
		t.Fatalf("same build: %v, %d downloads, %d restarts", err, downloads, restarts)
	}
	// Another build of 0.1.9: fetched, installed, restarted.
	os.WriteFile(exe, []byte("AGENT 0.1.9 BUILD 1"), 0o755)
	if err := u.Refetch(context.Background(), "0.1.9", shaOf(served)); err != nil || downloads != 1 || restarts != 1 {
		t.Fatalf("other build: %v, %d downloads, %d restarts", err, downloads, restarts)
	}
	if b, _ := os.ReadFile(exe); string(b) != string(served) {
		t.Fatalf("installed %q", b)
	}
	// Once per (release, sha): a binary that still differs afterwards (put
	// back by hand, say) is not fetched again for the same served sha.
	os.WriteFile(exe, []byte("AGENT 0.1.9 BUILD 1"), 0o755)
	if err := u.Refetch(context.Background(), "0.1.9", shaOf(served)); !errors.Is(err, ErrUpdateNotRepeated) || downloads != 1 {
		t.Fatalf("repeat: %v, %d downloads", err, downloads)
	}
	// A sha Potluck does not serve (the answer and the release disagree): refused.
	u.AttemptPath = filepath.Join(t.TempDir(), "attempt")
	if err := u.Refetch(context.Background(), "0.1.9", shaOf([]byte("something else"))); err == nil || downloads != 1 {
		t.Fatalf("a sha that is not served: %v, %d downloads", err, downloads)
	}
}

type refetchUpdater struct {
	fakeUpdater
	rmu  sync.Mutex
	refs []string
}

func (u *refetchUpdater) Refetch(_ context.Context, release, sha string) error {
	u.rmu.Lock()
	defer u.rmu.Unlock()
	u.refs = append(u.refs, release+" "+sha)
	return nil
}

func (u *refetchUpdater) nRefs() int { u.rmu.Lock(); defer u.rmu.Unlock(); return len(u.refs) }

// The agent asks its updater to re-fetch when the answer names an agent sha;
// an updater that cannot (a Docker install: the binary is its image) is
// never asked, and no answer sha means nothing to do.
func TestAgentRefetchesOnAnotherBuildOfItsRelease(t *testing.T) {
	e := newTenv(t)
	ru := &refetchUpdater{}
	e.a.deps.Updater = ru
	e.pl.set(func() {
		old := e.pl.answer
		e.pl.answer = func(req agentproto.RegisterRequest) agentproto.RegisterAnswer {
			a := old(req)
			a.AgentSHA256 = "ab12"
			return a
		}
	})
	e.start()
	agenttest.Eventually(t, 5*time.Second, "refetch asked", func() bool { return ru.nRefs() >= 1 })
	if ru.refs[0] != e.pl.hbAnswer.Release+" ab12" && ru.refs[0] != "0.1.0 ab12" {
		t.Fatal(ru.refs)
	}
	if ru.n() != 0 {
		t.Fatal("Update called for a same-release build")
	}
}
