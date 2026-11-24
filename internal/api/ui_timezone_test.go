package api

import (
	"fmt"
	"testing"
	"time"
)

// The Overview in the viewer's zone. The clock is fixed at 06:30 UTC on Tue
// 24 Nov 2026: 22:30 on Mon 23 Nov in Los Angeles. The same answers read as
// Monday there and Tuesday in UTC. Day totals are the 24 local hours; Week,
// Month and All time sum hourly buckets into local days, so a week across the
// end of daylight saving (Sun 1 Nov 2026, a 25-hour day) puts every hour in
// its own local day; a past day's strip runs to the next local midnight.
func TestUINextOverviewTimeZone(t *testing.T) {
	now := time.Date(2026, 11, 24, 6, 30, 0, 0, time.UTC)
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip("no time zone database: ", err)
	}
	ms := func(t time.Time) int64 { return t.UnixMilli() }
	hour := func(utc string, tin int) map[string]any {
		at, _ := time.Parse(time.RFC3339, utc)
		return map[string]any{"t": ms(at), "requests": 1, "tokens_in": tin, "tokens_out": 0}
	}
	fetch := ovFetch(now, ovRequests(now), true)
	fetch["/api/requests/summary"] = map[string]any{"on": true, "history_begins_at": ms(now.Add(-60 * 24 * time.Hour)), "oldest_ts": ms(now.Add(-60 * 24 * time.Hour)),
		"totals": map[string]any{"requests": 4, "tokens_in": 1040, "tokens_out": 0}, "live": map[string]any{"queued": 0, "running": 0},
		"series": []any{
			hour("2026-11-24T05:00:00Z", 700), // 21:00 on Mon 23 Nov in Los Angeles
			hour("2026-10-31T06:00:00Z", 40),  // 23:00 on Fri 30 Oct (summer time)
			hour("2026-11-01T07:00:00Z", 100), // 00:00 on Sun 1 Nov (summer time)
			hour("2026-11-02T07:00:00Z", 200), // 23:00 on Sun 1 Nov (winter time): the 25th hour of that day
		}}
	dayLA := time.Date(2026, 11, 23, 0, 0, 0, 0, la)
	nov1 := time.Date(2026, 11, 1, 0, 0, 0, 0, la)
	week := time.Date(2026, 10, 26, 0, 0, 0, 0, la)
	st := ovStatus(now)
	uiExec(t, map[string]any{"shell": "next", "app": true, "now": now.Format(time.RFC3339), "tz": "America/Los_Angeles", "fetch": fetch, "cases": []any{
		map[string]any{"name": "tz LA: today is Monday", "route": "#/overview", "status": st,
			"expect":       []string{"Mon 23 Nov 2026 · today"},
			"titles":       []string{"21:00–22:00 · 700 in"},
			"fetchedQuery": []string{fmt.Sprintf(`summary\?from=%d&to=%d&step=hour`, ms(dayLA), ms(dayLA.AddDate(0, 0, 1)))}},
		map[string]any{"name": "tz LA: a week across the end of summer time", "route": "#/overview?period=week&date=2026-10-28", "status": st,
			"expect":       []string{"26 Oct – 1 Nov 2026"},
			"titles":       []string{"Fri 30 Oct · 40 in", "Sun 1 Nov · 300 in", "Sat 31 Oct · 0 in"},
			"fetchedQuery": []string{fmt.Sprintf(`summary\?from=%d&to=%d&step=hour`, ms(week), ms(week.AddDate(0, 0, 7)))}},
		map[string]any{"name": "tz LA: the 25-hour day's strip", "route": "#/overview?date=2026-11-01", "status": st,
			"expect":       []string{"Sun 1 Nov 2026"},
			"fetchedQuery": []string{fmt.Sprintf(`availability\?from=%d&to=%d&step=hour`, ms(nov1), ms(nov1.AddDate(0, 0, 1)))}},
	}})
	uiExec(t, map[string]any{"shell": "next", "app": true, "now": now.Format(time.RFC3339), "tz": "UTC", "fetch": fetch, "cases": []any{
		map[string]any{"name": "tz UTC: the same moment is Tuesday", "route": "#/overview", "status": st,
			"expect": []string{"Tue 24 Nov 2026 · today"}, "titles": []string{"05:00–06:00 · 700 in"}},
	}})
}
