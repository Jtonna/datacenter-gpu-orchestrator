package config

import (
	"strings"
	"testing"
	"time"
)

// The reservation settings default to the behaviour of the release before
// them: no fill reserve, placement on measured free memory, margins unset.
func TestReserveSettingsDefaults(t *testing.T) {
	d := DefaultDefaults()
	if d.FillReserveKiBPerToken != 0 || d.PlacementUsesReserved || d.MarginMeasuredMiB != 0 ||
		d.MarginEstimatedPercent != 0 || d.MarginEstimatedMinMiB != 0 {
		t.Fatalf("a reservation setting is on by default: %+v", d)
	}
	if d.ReservedDriftWarnMiB != 500 || d.FreeAverageWindow != 30*time.Second {
		t.Fatalf("drift warn %d, window %v", d.ReservedDriftWarnMiB, d.FreeAverageWindow)
	}
	for _, k := range []string{"fill_reserve_kib_per_token", "placement_uses_reserved", "reserved_drift_warn_mib",
		"margin_measured_mib", "margin_estimated_percent", "margin_estimated_min_mib", "free_average_window"} {
		f, ok := SettingFieldByKey(ScopeSettings, k)
		if !ok || f.RestartRequired {
			t.Errorf("%s: in schema %v, restart required %v (it must change at run time)", k, ok, f.RestartRequired)
		}
	}
}

// The settings are validated: no negative figure, the percent 0 to 50, and
// they are set through the configuration document (the configuration API).
func TestReserveSettingsValidation(t *testing.T) {
	bad := []struct {
		key  string
		v    any
		want string
	}{
		{"fill_reserve_kib_per_token", -1.0, "negative"},
		{"reserved_drift_warn_mib", -1.0, "negative"},
		{"margin_measured_mib", -300.0, "negative"},
		{"margin_estimated_min_mib", -1.0, "negative"},
		{"margin_estimated_percent", 51.0, "between 0 and 50"},
		{"margin_estimated_percent", -1.0, "between 0 and 50"},
		{"free_average_window", "-5s", "negative"},
		{"fill_reserve_kib_per_token", 1025.0, "at most 1024"},
		{"free_average_window", "11m", "at most 10m"},
	}
	for _, b := range bad {
		doc := NewDocument()
		doc.Settings[b.key] = b.v
		_, err := Build(doc, Bootstrap{}, Options{FileExists: existsAll})
		if err == nil || !strings.Contains(err.Error(), b.key) || !strings.Contains(err.Error(), b.want) {
			t.Errorf("%s = %v: err %v, want %q", b.key, b.v, err, b.want)
		}
	}
	doc := NewDocument()
	for k, v := range map[string]any{"fill_reserve_kib_per_token": 8.0, "placement_uses_reserved": true, "reserved_drift_warn_mib": 0.0,
		"margin_measured_mib": 300.0, "margin_estimated_percent": 50.0, "margin_estimated_min_mib": 256.0, "free_average_window": "10s"} {
		doc.Settings[k] = v
	}
	c, err := Build(doc, Bootstrap{}, Options{FileExists: existsAll})
	if err != nil {
		t.Fatal(err)
	}
	d := c.Defaults
	if d.FillReserveKiBPerToken != 8 || !d.PlacementUsesReserved || d.ReservedDriftWarnMiB != 0 || d.MarginMeasuredMiB != 300 ||
		d.MarginEstimatedPercent != 50 || d.MarginEstimatedMinMiB != 256 || d.FreeAverageWindow != 10*time.Second {
		t.Fatalf("applied %+v", d)
	}
	// The YAML import reads them too.
	y := baseHeader + "defaults: {fill_reserve_kib_per_token: 8, margin_measured_mib: 300, free_average_window: 20s}\nprofiles: {}\n"
	yc, err := ParseWithOptions([]byte(y), Options{FileExists: existsAll})
	if err != nil {
		t.Fatal(err)
	}
	if yc.Defaults.FillReserveKiBPerToken != 8 || yc.Defaults.MarginMeasuredMiB != 300 || yc.Defaults.FreeAverageWindow != 20*time.Second ||
		yc.Defaults.ReservedDriftWarnMiB != 500 {
		t.Fatalf("yaml %+v", yc.Defaults)
	}
}
