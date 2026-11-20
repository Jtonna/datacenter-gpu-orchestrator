package stats

import (
	"strings"
	"testing"
)

func TestParseKeepsOnlyNamedFamilies(t *testing.T) {
	in := `# HELP node_load1 1m load average.
# TYPE node_load1 gauge
node_load1 1.69
node_unknown_thing{foo="bar"} 3
go_gc_duration_seconds{quantile="0.5"} 1.2e-05
nvidia_smi_memory_used_bytes{uuid="u"} 2.097152e+06
`
	m, bad, err := Parse(strings.NewReader(in), keepNode)
	if err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Fatalf("bad lines = %d, want 0", bad)
	}
	if len(m) != 1 {
		t.Fatalf("kept families = %v, want only node_load1", m)
	}
	if v := m["node_load1"][0].Value; v != 1.69 {
		t.Fatalf("node_load1 = %v", v)
	}
}

func TestParseScientificNotationAndTimestamp(t *testing.T) {
	in := "node_drm_memory_gtt_size_bytes{card=\"card0\"} 1.5137001472e+10\n" +
		"node_load1 0.5 1700000000000\n" +
		"node_load5 2E-1\r\n" // CRLF tolerated
	m, bad, err := Parse(strings.NewReader(in), keepNode)
	if err != nil || bad != 0 {
		t.Fatalf("err=%v bad=%d", err, bad)
	}
	if v := m["node_drm_memory_gtt_size_bytes"][0].Value; v != 15137001472 {
		t.Fatalf("gtt = %v", v)
	}
	if v := m["node_load1"][0].Value; v != 0.5 {
		t.Fatalf("load1 with timestamp = %v", v)
	}
	if v := m["node_load5"][0].Value; v != 0.2 {
		t.Fatalf("load5 = %v", v)
	}
}

func TestParseNaNAndInfAreSkipped(t *testing.T) {
	in := `node_load1 NaN
node_load5 +Inf
node_load15 -Inf
node_hwmon_temp_celsius{chip="a",sensor="temp1"} Inf
node_hwmon_temp_celsius{chip="a",sensor="temp2"} 40
`
	m, bad, err := Parse(strings.NewReader(in), keepNode)
	if err != nil || bad != 0 {
		t.Fatalf("err=%v bad=%d", err, bad)
	}
	for _, n := range []string{"node_load1", "node_load5", "node_load15"} {
		if len(m[n]) != 0 {
			t.Fatalf("%s kept: %v", n, m[n])
		}
	}
	if got := m["node_hwmon_temp_celsius"]; len(got) != 1 || got[0].Labels["sensor"] != "temp2" {
		t.Fatalf("temps = %v", got)
	}
}

func TestParseEscapedLabels(t *testing.T) {
	in := `node_hwmon_sensor_label{chip="a\"b\\c,d", label = "x, y\nz" ,sensor="temp1",} 1
node_network_up{} 1
node_network_up{device="eth0"}	1
`
	m, bad, err := Parse(strings.NewReader(in), keepNode)
	if err != nil || bad != 0 {
		t.Fatalf("err=%v bad=%d", err, bad)
	}
	l := m["node_hwmon_sensor_label"][0].Labels
	if l["chip"] != `a"b\c,d` || l["label"] != "x, y\nz" || l["sensor"] != "temp1" {
		t.Fatalf("labels = %#v", l)
	}
	if len(m["node_network_up"]) != 2 || m["node_network_up"][1].Labels["device"] != "eth0" {
		t.Fatalf("network_up = %v", m["node_network_up"])
	}
}

// Decision (documented on Parse): a malformed line is skipped and counted; it
// never fails the whole scrape.
func TestParseGarbageLinesSkipped(t *testing.T) {
	in := `node_load1 {{{
node_load5
node_memory_MemTotal_bytes abc
node_hwmon_temp_celsius{chip="a,sensor="temp1"} 40
node_hwmon_temp_celsius{chip="a" sensor="temp1"} 40
node_load15 1 notatimestamp
{}{} 3
node_memory_MemAvailable_bytes 1.2e+10
node_load1 0.7
`
	m, bad, err := Parse(strings.NewReader(in), keepNode)
	if err != nil {
		t.Fatal(err)
	}
	if bad != 7 {
		t.Fatalf("bad = %d, want 7", bad)
	}
	if v := m["node_memory_MemAvailable_bytes"][0].Value; v != 1.2e10 {
		t.Fatalf("avail = %v", v)
	}
	if got := m["node_load1"]; len(got) != 1 || got[0].Value != 0.7 {
		t.Fatalf("load1 = %v", got)
	}
	if len(m["node_hwmon_temp_celsius"]) != 0 || len(m["node_load15"]) != 0 {
		t.Fatalf("garbage kept: %v", m)
	}
}

func TestParseFixturesHaveNoBadLines(t *testing.T) {
	for _, f := range []struct {
		file string
		keep func(string) bool
	}{
		{"ps5_node.prom", keepNode},
		{"host_node.prom", keepNode},
		{"host_nvidia.prom", keepNvidia},
	} {
		m, bad := parseFile(t, f.file, f.keep)
		if bad != 0 || len(m) == 0 {
			t.Fatalf("%s: bad=%d families=%d", f.file, bad, len(m))
		}
	}
}
