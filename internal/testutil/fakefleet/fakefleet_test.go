package fakefleet_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/testutil/fakefleet"
)

func get(t *testing.T, url string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestFleetEnvelopeAndMembers(t *testing.T) {
	f := fakefleet.New(t)
	f.SetMembers(
		fakefleet.PS5("a8:5e", "ps5-1", "10.0.0.2", 12, 4.5, 14.1, 3.2),
		fakefleet.Relay("alienware-llm", "alienware-llm", "10.0.0.12", 8, 2),
	)
	code, body := get(t, f.URL()+"/api/fleet")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["v"] != float64(1) || doc["count"] != float64(2) || doc["online_count"] != float64(2) {
		t.Fatalf("envelope: %v", doc)
	}
	ms := doc["members"].([]any)
	m0 := ms[0].(map[string]any)
	for _, k := range []string{"id", "hostname", "mac", "ip", "name", "role", "source", "online", "last_seen", "rpc", "rpc_port", "vram_total", "vram_used", "ram_total_gb", "ram_used_gb", "gpu_name"} {
		if _, ok := m0[k]; !ok {
			t.Errorf("member lacks %q", k)
		}
	}
	if f.Requests(fakefleet.EpFleet) != 1 {
		t.Fatalf("request count = %d", f.Requests(fakefleet.EpFleet))
	}
}

func TestNodeEndpoint(t *testing.T) {
	f := fakefleet.New(t)
	f.SetMembers(fakefleet.PS5("a8:5e:45", "ps5-1", "10.0.0.2", 12, 4.5, 14.1, 3.2))
	code, body := get(t, f.URL()+"/api/nodes/a8:5e:45")
	if code != 200 || !strings.Contains(string(body), `"hostname":"ps5-1"`) {
		t.Fatalf("%d %s", code, body)
	}
	if code, _ = get(t, f.URL()+"/api/nodes/nope"); code != 404 {
		t.Fatalf("unknown node status %d", code)
	}
}

func TestSSEConnectPushPingDrop(t *testing.T) {
	f := fakefleet.New(t)
	f.SetMembers(fakefleet.PS5("p1", "ps5-1", "10.0.0.2", 12, 1, 14, 1))
	resp, err := http.Get(f.URL() + "/api/stream?topics=fleet,models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	rd := bufio.NewReader(resp.Body)
	readBlock := func() string {
		var sb strings.Builder
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				t.Fatalf("read: %v (got %q)", err, sb.String())
			}
			if line == "\n" {
				return sb.String()
			}
			sb.WriteString(line)
		}
	}
	if b := readBlock(); !strings.HasPrefix(b, "event: fleet\nid: ") || !strings.Contains(b, "data: {\"v\":1") {
		t.Fatalf("connect fleet block: %q", b)
	}
	if b := readBlock(); !strings.HasPrefix(b, "event: models\n") {
		t.Fatalf("connect models block: %q", b)
	}
	f.Ping()
	if b := readBlock(); b != ": ping\n" {
		t.Fatalf("ping block %q", b)
	}
	f.SetOnline("p1", false)
	f.Push()
	if b := readBlock(); !strings.Contains(b, `"online":false`) {
		t.Fatalf("push block %q", b)
	}
	f.DropStreams()
	if _, err := rd.ReadString('\n'); err == nil {
		t.Fatal("expected stream to end after DropStreams")
	}
	if f.StreamConnections() != 1 {
		t.Fatalf("connections=%d", f.StreamConnections())
	}
}

func TestFailAndHang(t *testing.T) {
	f := fakefleet.New(t)
	f.Fail(fakefleet.EpFleet, 500)
	if code, _ := get(t, f.URL()+"/api/fleet"); code != 500 {
		t.Fatalf("status %d", code)
	}
	f.Heal(fakefleet.EpFleet)
	if code, _ := get(t, f.URL()+"/api/fleet"); code != 200 {
		t.Fatalf("status %d", code)
	}
	f.Hang(fakefleet.EpNode)
	c := http.Client{Timeout: 100 * time.Millisecond}
	if _, err := c.Get(f.URL() + "/api/nodes/x"); err == nil {
		t.Fatal("expected timeout")
	}
}
