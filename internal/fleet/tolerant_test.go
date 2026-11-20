package fleet

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

const realMember = `{"id":"70:66:2a:a9:a7:65","hostname":"ps5-9032a330","ip":"10.0.0.101","role":"ps5-linux","source":"self","rpc_port":50052,"online":true,"ram_total_gb":14.1,"ram_used_gb":0.91,"vram_total":14.1,"vram_used":0.05,"payload_version":"d466ff91a994-5736b8df","mem_available_mib":13281.54,"rpc_server_mem_mib":74.92,"rpc_server_version":"9958d7ddcab4152720f64df958a876352085dfaf311db66859a6450607ff5ee7","last_recovery_action":null,"future_field":{"a":1}}`

// body serves one fleet payload on GET /api/fleet and as the SSE fleet event.
type payloadServer struct {
	srv *httptest.Server
	mu  sync.Mutex
	doc string
}

func newPayloadServer(t *testing.T, sse bool) *payloadServer {
	p := &payloadServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/fleet", func(w http.ResponseWriter, r *http.Request) {
		if sse {
			http.Error(w, "use the stream", 500)
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		fmt.Fprint(w, p.doc)
	})
	mux.HandleFunc("/api/stream", func(w http.ResponseWriter, r *http.Request) {
		if !sse {
			http.Error(w, "no stream", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		p.mu.Lock()
		fmt.Fprintf(w, "event: fleet\nid: 1\ndata: %s\n\n", p.doc)
		p.mu.Unlock()
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func runPayload(t *testing.T, sse bool, doc string) (*env, *payloadServer) {
	p := newPayloadServer(t, sse)
	p.doc = doc
	e := newEnv(t)
	e.cfg.Fleet.URL = p.srv.URL
	e.start()
	return e, p
}

func forBothPaths(t *testing.T, f func(t *testing.T, sse bool)) {
	t.Run("GET", func(t *testing.T) { f(t, false) })
	t.Run("SSE", func(t *testing.T) { f(t, true) })
}

func TestRealFleetMemberDecodes(t *testing.T) {
	forBothPaths(t, func(t *testing.T, sse bool) {
		e, _ := runPayload(t, sse, `{"members":[`+realMember+`]}`)
		var n core.Node
		eventually(t, func() bool { var ok bool; n, ok = e.nodeByID("70:66:2a:a9:a7:65"); return ok }, "the member")
		if n.FreeMiB != 13281 || n.FreeUnknown {
			t.Errorf("free %d (unknown %v), want 13281 (13281.54 rounded down)", n.FreeMiB, n.FreeUnknown)
		}
		if !n.RPCServerMemKnown || n.RPCServerMemMiB != 74.92 || !strings.HasPrefix(n.RPCServerVersion, "9958d7dd") {
			t.Errorf("rpc-server info: %+v", n)
		}
		if n.Addr != "10.0.0.101:50052" {
			t.Errorf("addr %q", n.Addr)
		}
		if e.log.count("bad fleet payload") != 0 || e.log.count("ignored") != 0 {
			t.Errorf("unexpected warnings:\n%s", e.log.String())
		}
	})
}

func TestOneBadFieldKeepsEveryMember(t *testing.T) {
	bad := `{"id":"b","hostname":"ps5-2","ip":"10.0.0.102","role":"ps5-linux","source":"self","rpc_port":50052.0,"online":true,` +
		`"ram_total_gb":14,"ram_used_gb":4.5,"vram_total":14.1,"vram_used":0.05,"mem_available_mib":"oops","rpc_server_mem_mib":"x"}`
	noID := `{"hostname":"ps5-3","online":true}`
	forBothPaths(t, func(t *testing.T, sse bool) {
		e, _ := runPayload(t, sse, `{"members":[`+realMember+`,`+bad+`,`+noID+`,42]}`)
		var good, b core.Node
		eventually(t, func() bool {
			var ok1, ok2 bool
			good, ok1 = e.nodeByID("70:66:2a:a9:a7:65")
			b, ok2 = e.nodeByID("b")
			return ok1 && ok2
		}, "both members")
		if good.FreeMiB != 13281 {
			t.Errorf("good member free %d", good.FreeMiB)
		}
		// the bad field is ignored: the RAM figure (14 - 4.5 GB) stands, the port float is read
		if b.FreeMiB != 9728 || b.Addr != "10.0.0.102:50052" || b.RPCServerMemKnown {
			t.Errorf("bad-field member: %+v", b)
		}
		if len(e.c.Nodes()) != 2 {
			t.Errorf("want 2 nodes (members without an id skipped), got %d", len(e.c.Nodes()))
		}
		eventually(t, func() bool { return e.log.count(`field=mem_available_mib`) == 1 }, "one warning naming the field")
		if e.log.count("bad fleet payload") != 0 || e.log.count("unusable fleet member skipped") != 1 {
			t.Errorf("logs:\n%s", e.log.String())
		}
	})
}

// A persistent bad field is logged once, not on every poll.
func TestBadFieldWarnIsRateLimited(t *testing.T) {
	e, _ := runPayload(t, false, `{"members":[{"id":"b","hostname":"ps5-2","online":true,"mem_available_mib":"oops"}]}`)
	eventually(t, func() bool { return e.log.count("fleet member field ignored") == 1 }, "first warning")
	e.c.applyDoc([]byte(`{"members":[{"id":"b","hostname":"ps5-2","online":true,"mem_available_mib":"oops"}]}`))
	if n := e.log.count("fleet member field ignored"); n != 1 {
		t.Errorf("want 1 warning, got %d", n)
	}
}

func TestMemberFloatsAndOddValues(t *testing.T) {
	var m Member
	err := m.UnmarshalJSON([]byte(`{"id":"x","rpc_port":50052.0,"last_seen":1763300000000.5,"ram_used_gb":3,"mem_available_mib":-5,"rpc_server_mem_mib":1e300,"rpc_server_version":7}`))
	if err != nil {
		t.Fatal(err)
	}
	if m.RPCPort == nil || *m.RPCPort != 50052 || m.LastSeen != 1763300000000 || *m.RAMUsedGB != 3 {
		t.Errorf("numbers: %+v", m)
	}
	if m.MemAvailableMiB != nil || m.RPCServerMemMiB != nil || m.RPCServerVersion != nil || len(m.problems) != 3 {
		t.Errorf("odd values should be unknown: %+v", m)
	}
	if err := m.UnmarshalJSON([]byte(`{"hostname":"a"}`)); err == nil {
		t.Error("a member without an id must be unusable")
	}
}
