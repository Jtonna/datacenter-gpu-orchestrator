package api

import (
	"strings"
	"testing"
)

// Central logging in the web UI (CR-10): a "Logs" entry and links that open
// the log store's search page with a query filled in, each hidden while
// logging is off.
func TestUILogsEntryAndLinks(t *testing.T) {
	logs := uiText(t, "core/logs.js")
	for _, want := range []string{
		"'/logs/select/vmui/'",                                   // the store's search page, under Potluck's /logs/
		"st.logs.enabled && st.logs.store",                       // shown only while logging is on
		"'#/?'", "'query'", "'g0.end_input'", "'g0.range_input'", // the search page's own link format
		"'g0.relative_time', 'none'", "'g0.relative_time', 'last_1_hour'",
		"'node_id:=' + quote(id)", "'profile:=' + quote(name)",
		"target: '_blank', rel: 'noopener'", // a new tab
		"if (!logsOn(st)) return null",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("core/logs.js lacks %s", want)
		}
	}
	shell := uiText(t, uiNextDir+"core/shell.js")
	for _, want := range []string{"{ id: 'logs', label: 'Logs', external: true", "!logsOn(state.status)) return null", "target: '_blank'"} {
		if !strings.Contains(shell, want) {
			t.Errorf("next/core/shell.js lacks %s", want)
		}
	}
	// every link goes through logsLink, which returns null while logging is off
	for file, want := range map[string]string{
		"views/nodes.js":              "logsLink(state.status, ' Logs', nodeQuery(n.id)",
		"views/models/common.js":      "logsLink(st, 'Logs', profileQuery(inst.profile)",
		uiNextDir + "views/models.js": "logsLink(st, 'Logs', profileQuery(l.model), from, to",
		"views/benchmarks/runs.js":    "logsLink(state.status, 'Logs', '*', from, to",
	} {
		text := uiText(t, file)
		if !strings.Contains(text, want) {
			t.Errorf("%s lacks %s", file, want)
		}
		if strings.Contains(text, "/logs/select") {
			t.Errorf("%s builds a log link itself: use core/logs.js", file)
		}
	}
}
