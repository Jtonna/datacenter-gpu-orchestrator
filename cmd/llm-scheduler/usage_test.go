package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/usage"
)

// A damaged usage.db never makes the scheduler unhealthy: it is moved aside
// with one WARN, the request log starts empty and says when its history
// begins; a missing one is created with one WARN.
func TestDamagedOrMissingUsageFileStartsHealthy(t *testing.T) {
	for _, damaged := range []bool{true, false} {
		data := t.TempDir()
		if damaged {
			os.WriteFile(filepath.Join(data, usage.FileName), bytes.Repeat([]byte("garbage!"), 1000), 0o644)
		}
		addr, logs, stop := startOn(t, data)
		hcode, hbody := get(t, "http://"+addr+"/health")
		code, body := get(t, "http://"+addr+"/api/requests/summary")
		if c := stop(); c != 0 {
			t.Fatalf("exit %d", c)
		}
		if hcode != 200 {
			t.Fatalf("damaged=%v: /health %d %s\n%s", damaged, hcode, hbody, logs.String())
		}
		var sum usage.Summary
		if code != 200 || json.Unmarshal([]byte(body), &sum) != nil || !sum.On || sum.HistoryBeginsAt == nil || *sum.HistoryBeginsAt <= 0 {
			t.Fatalf("damaged=%v: summary %d %s", damaged, code, body)
		}
		warns := 0
		for _, l := range strings.Split(logs.String(), "\n") {
			if strings.Contains(l, "level=WARN") && strings.Contains(l, "usage history") {
				warns++
			}
		}
		if warns != 1 || damaged != strings.Contains(logs.String(), "moved_to=") {
			t.Fatalf("damaged=%v: want one usage WARN:\n%s", damaged, logs.String())
		}
		matches, _ := filepath.Glob(filepath.Join(data, usage.FileName+".damaged-*"))
		if damaged != (len(matches) == 1) {
			t.Fatalf("damaged=%v: moved-aside files %v", damaged, matches)
		}
	}
}

// With the usage store off (here: a usage.db a newer image migrated), the
// scheduler is healthy and the status, node and UI routes answer as before;
// the request log and the availability route answer from memory.
func TestUsageStoreOffLeavesTheOtherRoutesAlone(t *testing.T) {
	data := t.TempDir()
	st := usage.Open(usage.Options{Dir: data})
	if !st.On() || st.DB() == nil {
		t.Fatal("setup")
	}
	if _, err := st.DB().Exec(`INSERT INTO usage_migrations (version, name, applied_at) VALUES (99, 'future', 0)`); err != nil {
		t.Fatal(err)
	}
	st.Close()
	addr, logs, stop := startOn(t, data)
	codes := map[string]int{}
	bodies := map[string]string{}
	for _, p := range []string{"/health", "/api/status", "/api/nodes", "/ui/", "/ui/classic/", "/api/requests", "/api/nodes/availability", "/api/system"} {
		codes[p], bodies[p] = get(t, "http://"+addr+p)
	}
	if c := stop(); c != 0 {
		t.Fatalf("exit %d", c)
	}
	for p, c := range codes {
		if c != 200 && !(p == "/api/nodes" && c == 503) { // /api/nodes answers 503 in fleet membership, as before
			t.Errorf("%s: %d %s", p, c, bodies[p])
		}
	}
	if !strings.Contains(bodies["/api/requests"], `"on":false`) || !strings.Contains(bodies["/api/system"], `"usage":{"on":false`) {
		t.Fatalf("requests %s\nsystem %s", bodies["/api/requests"], bodies["/api/system"])
	}
	if !strings.Contains(logs.String(), "newer scheduler") {
		t.Fatal(logs.String())
	}
}
