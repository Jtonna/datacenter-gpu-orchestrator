package api

import (
	"testing"
	"time"
)

// The Model column of a request row names the PROFILE (the reviewer's
// ruling, 26 Nov): its shown name through profileLabel ("TEST - p4-qwen4b-107"
// for a test profile) with the model's short name beside it, instead of the
// made-up "qwen3.5-4b:2k" (the file's short name and the ctx) that read like
// a production profile id that does not exist. A run-scoped profile is named
// by its production profile with "@<scope>". A request whose profile the
// configuration no longer has prints its stored id with "profile no longer
// exists". On the Requests page, the Overview's runs and Activity's recent
// requests.
func TestUINextRequestRowsNameTheProfile(t *testing.T) {
	now := time.Now()
	ms := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }
	row := func(id int, profile string) map[string]any {
		return map[string]any{"id": id, "kind": "v1", "profile": profile, "status": "done", "caller": "anonymous", "nodes": []any{},
			"queued_at": ms(-time.Duration(id) * time.Minute), "started_at": ms(-time.Duration(id) * time.Minute), "duration_ms": 3900,
			"tokens_in": 32, "prefill_tps": 53.3, "tokens_out": 160, "decode_tps": 48.0}
	}
	rows := []any{row(31, "p4-qwen4b-107"), row(32, "qwen3.5-4b"), row(33, "qwen3.5-4b@e2"), row(34, "p2-old-test")}
	fetch := ovFetch(now, rows, true)
	fetch["/api/models"] = map[string]any{"version": 1, "models": []any{
		map[string]any{"key": "Qwen3.5-4B-Q8_0.gguf", "kind": "llama", "exists": true, "file": "/models/Qwen3.5-4B-Q8_0.gguf", "short_name": "qwen3.5-4b", "profile_count": 2,
			"profiles": []any{
				map[string]any{"id": "qwen3.5-4b", "ctx": 16384, "stage": "production", "shown_name": "qwen3.5-4b"},
				map[string]any{"id": "p4-qwen4b-107", "ctx": 2048, "stage": "test", "shown_name": "TEST - p4-qwen4b-107"},
			}},
	}}
	fetch["/api/metrics/activity"] = map[string]any{"data": []any{
		map[string]any{"id": 1, "timestamp": now.Add(-time.Minute).UTC().Format(time.RFC3339), "model": "p4-qwen4b-107", "resp_status_code": 200, "duration_ms": 1200,
			"tokens": map[string]any{"input_tokens": 10, "output_tokens": 20, "tokens_per_second": 30.0}},
		map[string]any{"id": 2, "timestamp": now.Add(-2 * time.Minute).UTC().Format(time.RFC3339), "model": "p2-old-test", "resp_status_code": 200, "duration_ms": 1200,
			"tokens": map[string]any{"input_tokens": 10, "output_tokens": 20, "tokens_per_second": 30.0}},
	}}
	st := ovStatus(now)
	cell := func(id string) string { return "tr[data-req='" + id + "'] td.mono " }
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "requests: the Model column names the profile", "route": "#/requests", "status": st, "width": 1536,
			"expect": []string{"TEST - p4-qwen4b-107 · qwen3.5-4b", "qwen3.5-4b@e2", "p2-old-test · profile no longer exists"},
			// the Model filter lists the shown names, the profile id as the value
			"page":   []string{"Model: TEST - p4-qwen4b-107", "Model: p2-old-test"},
			"absent": []string{"qwen3.5-4b:2k", "qwen3.5-4b:16k"},
			"has": []string{cell("31") + "span[data-shown]", cell("31") + "span[data-short]", cell("34") + "span[data-gone]",
				cell("32") + "span[data-shown]"},
			"none": []string{cell("32") + "span[data-short]", cell("31") + "span[data-gone]"}}, // the production profile has the model's name
		map[string]any{"name": "overview: the runs name the profile", "route": "#/overview", "status": st, "width": 1536,
			"expect": []string{"TEST - p4-qwen4b-107 · qwen3.5-4b", "p2-old-test · profile no longer exists"},
			"absent": []string{"qwen3.5-4b:2k"}},
		map[string]any{"name": "activity: recent requests name the profile", "route": "#/activity", "status": st, "width": 1536,
			"expect": []string{"TEST - p4-qwen4b-107 · qwen3.5-4b", "p2-old-test · profile no longer exists"}},
	}})
}
