package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/history"
)

// The X-Bench-Run header tags the history records of the load a request
// causes (data.bench_run) and changes nothing else.
func TestBenchRunHeaderTagsLoadHistory(t *testing.T) {
	st := openStore(t)
	e := histEnv(t, st)
	req, _ := http.NewRequest("POST", e.front.URL+"/v1/chat/completions", strings.NewReader(chat("alpha")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Bench-Run", "br-test-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if b := body(t, resp); resp.StatusCode != 200 {
		t.Fatalf("status %d %s", resp.StatusCode, b)
	}
	eventually(t, "tagged loadStarted", func() bool {
		r := st.Query(history.Query{Types: []string{history.TLoadStarted}})
		return len(r.Events) == 1 && r.Events[0].Data["bench_run"] == "br-test-1"
	})
}

func TestBenchRunHeaderIsSanitised(t *testing.T) {
	for in, want := range map[string]string{
		"br-1":                   "br-1",
		"  spaced  ":             "spaced",
		"":                       "",
		"bad\x01char":            "",
		strings.Repeat("x", 100): "",
		"unicode-é":              "",
		"has space":              "",
		"ok_1.2-3":               "ok_1.2-3",
		"semi;colon":             "",
	} {
		if got := benchRunHeader(in); got != want {
			t.Errorf("benchRunHeader(%q) = %q, want %q", in, got, want)
		}
	}
}
