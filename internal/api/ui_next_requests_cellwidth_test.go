package api

import (
	"strings"
	"testing"
	"time"
)

// The Model cell of a request row never widens its table (v88's picture
// check on a copy of live: the Overview at 1280 was 1294 px wide once the
// Model cell named live's long test profiles, "Out tok/s", "Zones" and "Sent
// by" cut at the card's edge). The cell is one line: the profile's shown
// name stays whole as long as the column allows, the model's short name gives
// way first; each ends in "…"; the whole text is on hover. Its width is the
// room the other columns leave (max-width:0 keeps its text out of the table's
// sizing), up to 40ch in the Overview card and 48ch on the Requests page; the
// Requests table scrolls inside its card on a phone. Measured in Chrome with
// the fixture's --long-names rows (these three names) at 1536, 1280 and 390:
// Overview, Requests and Activity have scrollWidth == the window's width.
func TestUINextRequestModelCellNeverWidensItsTable(t *testing.T) {
	css := uiText(t, uiNextDir+"app.css")
	for _, want := range []string{
		"table.runs{width:100%;border-collapse:collapse;position:relative}",
		".runs td.rq-mc{max-width:0;width:100%}",
		".runs td.rq-model{min-width:calc(20ch + 16px)}",
		".ov-req .rpc{max-width:40ch}",
		".rq-model .rpc{max-width:48ch}",
		".rpc{display:flex;min-width:0;overflow:hidden;white-space:nowrap}",
		".rpc>span{min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:pre}",
		".rpc-n{flex:0 0 auto;max-width:100%}",
		".rpc-m{flex:0 1 auto}",
		".rq-scroll{overflow-x:auto}",
		".rq-bar select.fs{max-width:100%;text-overflow:ellipsis}",
		".tbl td.clip{max-width:0;width:100%}",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css lacks %s", want)
		}
	}
	// the cell no longer wraps
	if strings.Contains(css, "td.rq-model{white-space:normal;overflow-wrap:anywhere") {
		t.Error("the Requests Model cell still wraps")
	}

	now := time.Now()
	ms := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }
	type long struct{ id, stage, file, short, cell string }
	longs := []long{
		{"bench-tinyllama-1-1b-chat-v1-0-q4-k-m-on-box-0a1b2c3d-2304", "test", "tinyllama-1.1b-chat-v1.0.Q4_K_M.gguf", "tinyllama-1.1b-chat-v1.0",
			"TEST - bench-tinyllama-1-1b-chat-v1-0-q4-k-m-on-box-0a1b2c3d-2304 · tinyllama-1.1b-chat-v1.0"},
		{"p4-qwen4b-107", "test", "Qwen3.5-4B-Q4_K_M.gguf", "qwen3.5-4b", "TEST - p4-qwen4b-107 · qwen3.5-4b"},
		{"qwen3-coder-30b-a3b-instruct:128k", "production", "Qwen3-Coder-30B-A3B-Instruct-Q4_K_M.gguf", "qwen3-coder-30b-a3b-instruct",
			"qwen3-coder-30b-a3b-instruct:128k · qwen3-coder-30b-a3b-instruct"},
	}
	var rows, models, activity []any
	for i, l := range longs {
		id := 41 + i
		rows = append(rows, map[string]any{"id": id, "kind": "v1", "profile": l.id, "status": "done", "caller": "anonymous", "nodes": []any{},
			"queued_at": ms(-time.Duration(i+1) * time.Minute), "started_at": ms(-time.Duration(i+1) * time.Minute), "duration_ms": 3800,
			"tokens_in": 1834, "prefill_tps": 1034.0, "tokens_out": 1160, "decode_tps": 214.6})
		shown := l.id
		if l.stage == "test" {
			shown = "TEST - " + l.id
		}
		models = append(models, map[string]any{"key": l.file, "kind": "llama", "exists": true, "file": "/models/" + l.file, "short_name": l.short, "profile_count": 1,
			"profiles": []any{map[string]any{"id": l.id, "ctx": 2048, "stage": l.stage, "shown_name": shown}}})
		activity = append(activity, map[string]any{"id": id, "timestamp": now.Add(-time.Duration(i+1) * time.Minute).UTC().Format(time.RFC3339), "model": l.id,
			"resp_status_code": 200, "duration_ms": 3800, "tokens": map[string]any{"input_tokens": 1834, "output_tokens": 1160, "tokens_per_second": 214.6}})
	}
	fetch := ovFetch(now, rows, true)
	fetch["/api/models"] = map[string]any{"version": 1, "models": models}
	fetch["/api/metrics/activity"] = map[string]any{"data": activity}
	st := ovStatus(now)

	// each row's cell: the class that keeps it out of the table's sizing, one
	// span.rpc with the whole text on hover, the shown name and the model apart
	// (the test DOM's selectors split on spaces: the hover is checked by its
	// space-free parts, the whole line by "expect")
	cell := func(req string, l long) []string {
		td := "tr[data-req='" + req + "'] td.mono.rq-mc"
		return []string{td + " span.rpc[title*='" + l.id + "'][title*='" + l.short + "'][title*='" + l.file + "'] span.rpc-n[data-shown]",
			td + " span.rpc span.rpc-m.mut[data-short]", "tr[data-req='" + req + "'] td.rq-mc[title*='" + l.file + "']"}
	}
	var reqHas []string
	for i, l := range longs {
		reqHas = append(reqHas, cell([]string{"41", "42", "43"}[i], l)...)
	}
	texts := []string{longs[0].cell, longs[1].cell, longs[2].cell}
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "requests: live's longest Model cells", "route": "#/requests", "status": st, "width": 1280,
			"expect": texts, "has": append(append([]string{}, reqHas...), "div.rq-scroll table.runs", "tr[data-req='41'] td.rq-mc.rq-model")},
		map[string]any{"name": "overview: live's longest Model cells", "route": "#/overview", "status": st, "width": 1280,
			"expect": texts, "has": reqHas, "none": []string{"td.rq-model"}},
		map[string]any{"name": "activity: live's longest Model cells", "route": "#/activity", "status": st, "width": 1280,
			"expect": texts, "has": []string{"td.clip span.rpc[title^=TEST][title*='" + longs[0].id + "'][title*='" + longs[0].short + "'] span.rpc-n[data-shown]",
				"td.clip span.rpc[title*='" + longs[1].id + "'] span.rpc-m[data-short]", "td.clip span.rpc[title^='" + longs[2].id + "'][title*='" + longs[2].short + "'] span.rpc-m[data-short]"}},
	}})
}
