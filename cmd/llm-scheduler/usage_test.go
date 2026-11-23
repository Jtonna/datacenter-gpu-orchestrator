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
		if code != 200 || json.Unmarshal([]byte(body), &sum) != nil || !sum.On || sum.HistoryBeginsAt == 0 {
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
