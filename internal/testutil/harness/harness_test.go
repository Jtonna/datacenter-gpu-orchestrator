package harness

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	code := m.Run()
	Cleanup()
	os.Exit(code)
}

func TestFreePortIsBindable(t *testing.T) {
	p := FreePort(t)
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
	if err != nil {
		t.Fatalf("port %d not bindable: %v", p, err)
	}
	l.Close()
}

func TestWaitForOKSeesEndpointBecomeHealthy(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	WaitForOK(t, srv.URL, 5*time.Second)
	if calls.Load() < 3 {
		t.Fatalf("returned after %d calls, expected at least 3", calls.Load())
	}
}

func TestBuildFakeLlamaReturnsExistingBinary(t *testing.T) {
	p := BuildFakeLlama(t)
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
	if BuildFakeLlama(t) != p {
		t.Fatal("second call built again")
	}
}
