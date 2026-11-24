package api

import (
	"strings"
	"testing"
	"time"
)

// The Requests page's Zones column (next/views/requests.js; no mockup for
// this page, and the Overview's runs table in Main.dc.html has only the
// Zones toggle, never a list of machines): a request served by three or six
// machines wrapped nowhere, so the column pushed "Sent by" out of the card
// and the page scrolled sideways (at 1536, three machines with real names).
// Up to three machines the cell wraps between machines, each machine kept
// whole on one line (span.mc, nowrap); beyond three it reads "<host> Host +
// N helpers" with the full list on hover (the reviewer's rule when no
// mockup decides). The other columns keep their width.
func TestUIRequestsZonesWrap(t *testing.T) {
	css := uiText(t, uiNextDir+"app.css")
	for _, want := range []string{
		".runs td.rq-zones{white-space:normal;min-width:0;padding-top:4px;padding-bottom:4px;line-height:1.45}",
		".rq-zones .mc{white-space:nowrap}",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css lacks %s", want)
		}
	}

	now := time.Now()
	ms := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }
	machines := func(n int) []any {
		out := []any{}
		for i := 1; i <= n; i++ {
			role, share := "helper", 1.0/float64(n)
			if i == 1 {
				role = "host"
			}
			out = append(out, map[string]any{"id": "02:00:00:00:02:0" + string(rune('0'+i)), "name": "console-0" + string(rune('0'+i)), "role": role, "share": share})
		}
		return out
	}
	row := func(id, n int) map[string]any {
		return map[string]any{"id": id, "kind": "v1", "profile": "alpha", "status": "done", "caller": "anonymous",
			"queued_at": ms(-time.Duration(id) * time.Minute), "started_at": ms(-time.Duration(id) * time.Minute), "duration_ms": 3900,
			"tokens_in": 32, "prefill_tps": 53.3, "tokens_out": 160, "decode_tps": 48.0, "nodes": machines(n)}
	}
	rows := []any{row(21, 1), row(22, 2), row(23, 3), row(24, 6)}
	fetch := ovFetch(now, rows, true)
	st := ovStatus(now)
	for _, w := range []int{1536, 1280} {
		uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
			map[string]any{"name": "requests: long zone lists wrap between machines", "route": "#/requests", "status": st, "width": w,
				"expect": []string{"Zones", "Sent by", "console-01 Host 100%",
					"console-01 Host 33%, console-02 Helper 33%, console-03 Helper 33%",
					// more than three: shortened, the full list on hover
					"console-01 Host + 5 helpers"},
				"absent": []string{"console-06 Helper 17%"},
				"has": []string{
					"tr[data-req='21'] td.rq-zones span[data-machines='1'] span.mc",
					"tr[data-req='22'] td.rq-zones span[data-machines='2'] span.mc",
					"tr[data-req='23'] td.rq-zones span[data-machines='3'] span.mc",
					"tr[data-req='24'] td.rq-zones span[data-machines='6'][title^='console-01'][title*='console-06'] span.mc",
				}},
		}})
	}
}
