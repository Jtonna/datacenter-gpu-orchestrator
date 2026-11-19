package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// --capture <base-url> records the GET answers of a running scheduler into
// cmd/devcluster/realdata, the data of the "real" scenarios (real.go). It
// only ever sends GET requests: nothing on the recorded system is changed,
// loaded or run.
//
//	go run ./cmd/devcluster --capture http://scheduler-host:8080
//	go run ./cmd/devcluster --capture http://scheduler-host:8080 --capture-status status-loading.json
//
// The second form records only /api/status under another name (a moment with
// a load in progress, say); the scenario real-loading shows it.

var captureMetrics = []string{"load_s", "load_mb_s", "memory_mib", "decode_tps", "prefill_tps", "ttft_ms", "unload_s", "concurrent_wall_s"}

// captureIndex is realdata/index.json: when the recording was made and which
// file answers which request ("<path>" or "<path>?<query>").
type captureIndex struct {
	CapturedAt int64             `json:"captured_at"`
	Entries    map[string]string `json:"entries"`
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func captureGet(base, pathQuery string) ([]byte, error) {
	c := &http.Client{Timeout: 60 * time.Second}
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(base, "/")+pathQuery, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: status %d", pathQuery, resp.StatusCode)
	}
	if !json.Valid(b) {
		return nil, fmt.Errorf("GET %s: the answer is not JSON", pathQuery)
	}
	return b, nil
}

func realdataDir() (string, error) {
	root, err := moduleRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "cmd", "devcluster", "realdata"), nil
}

func runCapture(base, statusOnly string) error {
	dir, err := realdataDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if statusOnly != "" {
		b, err := captureGet(base, "/api/status")
		if err != nil {
			return err
		}
		fmt.Printf("captured /api/status as %s (%d bytes)\n", statusOnly, len(b))
		return os.WriteFile(filepath.Join(dir, filepath.Base(statusOnly)), indentJSON(b), 0o644)
	}
	idx := captureIndex{Entries: map[string]string{}}
	save := func(pathQuery string, must bool) []byte {
		b, err := captureGet(base, pathQuery)
		if err != nil {
			if must {
				fmt.Fprintf(os.Stderr, "capture: %v\n", err)
			}
			return nil
		}
		name := strings.Trim(unsafeName.ReplaceAllString(strings.TrimPrefix(pathQuery, "/api/"), "_"), "_") + ".json"
		if err := os.WriteFile(filepath.Join(dir, name), indentJSON(b), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "capture: %v\n", err)
			return nil
		}
		// the history is seeded into the real store, so its query string is not part of the key
		key := pathQuery
		if strings.HasPrefix(pathQuery, "/api/history?") || strings.HasPrefix(pathQuery, "/api/audit?") {
			key = pathQuery[:strings.Index(pathQuery, "?")]
		}
		idx.Entries[key] = name
		fmt.Printf("  %-70s %8d bytes\n", pathQuery, len(b))
		return b
	}
	var sys struct {
		Now int64 `json:"now"`
	}
	json.Unmarshal(save("/api/system", true), &sys)
	if sys.Now == 0 {
		return fmt.Errorf("%s/api/system did not answer: is this a scheduler?", base)
	}
	idx.CapturedAt = sys.Now
	for _, p := range []string{"/api/status", "/api/profiles", "/api/about", "/api/metrics/activity",
		"/api/config", "/api/config/schema", "/api/config/revisions", "/api/config/settings", "/api/config/profiles", "/api/config/tiers",
		"/api/config/exclude-nodes", "/api/config/node-overrides", "/api/config/recovery", "/api/config/fleet",
		"/api/history?limit=2000", "/api/audit?limit=500",
		"/api/bench/state", "/api/bench/schema", "/api/bench/suites", "/api/bench/benchmarks", "/api/bench/runs", "/api/bench/aggregates", "/api/bench/profiles"} {
		save(p, true)
	}
	ids := func(file, list string) []string {
		b, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			return nil
		}
		var doc map[string]any
		var out []string
		if json.Unmarshal(b, &doc) == nil {
			items, _ := doc[list].([]any)
			for _, it := range items {
				e, _ := it.(map[string]any)
				switch v := e["id"].(type) {
				case string:
					out = append(out, v)
				case float64:
					out = append(out, fmt.Sprint(int64(v)))
				}
				if v, ok := e["revision"].(float64); ok && e["id"] == nil {
					out = append(out, fmt.Sprint(int64(v)))
				}
			}
		}
		sort.Strings(out)
		return out
	}
	for _, id := range ids("bench_suites.json", "suites") {
		save("/api/bench/suites/"+url.PathEscape(id), false)
	}
	for _, id := range ids("bench_runs.json", "runs") {
		save("/api/bench/runs/"+url.PathEscape(id), false)
	}
	for _, id := range ids("bench_benchmarks.json", "benchmarks") {
		save("/api/bench/benchmarks/"+url.PathEscape(id), false)
		for _, m := range captureMetrics {
			save("/api/bench/trends?benchmark="+url.QueryEscape(id)+"&metric="+m, false)
		}
	}
	for _, n := range ids("config_revisions.json", "revisions") {
		save("/api/config/revisions/"+n, false)
	}
	b, _ := json.MarshalIndent(idx, "", " ")
	if err := os.WriteFile(filepath.Join(dir, "index.json"), append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("captured %d answers into %s\n", len(idx.Entries), dir)
	return nil
}

func indentJSON(b []byte) []byte {
	var v any
	if json.Unmarshal(b, &v) != nil {
		return b
	}
	out, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return b
	}
	return append(out, '\n')
}
