package config

import "testing"

// The usage retention settings are stored only when someone sets them: an
// import that leaves them out (or gives the default) stores no key, so an
// older image never sees them as unknown fields. An empty document builds
// the defaults.
func TestUsageSettingsAreStoredOnlyWhenSet(t *testing.T) {
	for _, c := range []struct {
		yml       string
		wantStore bool
	}{
		{"history_keep_files: 7\n", false},
		{"usage_request_days: 90\nusage_hourly_days: 396\n", false},
		{"usage_request_days: 30\n", true},
	} {
		imp, err := ParseYAMLImport([]byte(c.yml), Bootstrap{ModelsDir: "/m"}, Options{FileExists: always})
		if err != nil {
			t.Fatal(err)
		}
		_, got := imp.Doc.Settings["usage_request_days"]
		_, hourly := imp.Doc.Settings["usage_hourly_days"]
		if got != c.wantStore || hourly {
			t.Fatalf("%q: stored settings %v", c.yml, imp.Doc.Settings)
		}
	}
	cfg, err := Build(NewDocument(), Bootstrap{ModelsDir: "/m"}, Options{FileExists: always})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UsageRequestDays != DefaultUsageRequestDays || cfg.UsageHourlyDays != DefaultUsageHourlyDays {
		t.Fatalf("defaults %d %d", cfg.UsageRequestDays, cfg.UsageHourlyDays)
	}
	if _, err := Build(&Document{Settings: map[string]any{"usage_hourly_days": 0}}, Bootstrap{ModelsDir: "/m"}, Options{FileExists: always}); err == nil {
		t.Fatal("0 days accepted")
	}
}
