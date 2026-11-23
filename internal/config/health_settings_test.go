package config

import "testing"

// loop_step_warn_ms and models_disk_budget_gb are stored only when set (an
// older image never sees them as unknown fields), take their defaults from
// an empty document, reach the Config, and are bounded.
func TestHealthSettingsDefaultsStorageAndBounds(t *testing.T) {
	for _, c := range []struct {
		yml        string
		warn, budg bool
	}{
		{"history_keep_files: 7\n", false, false},
		{"loop_step_warn_ms: 1000\nmodels_disk_budget_gb: 0\n", false, false},
		{"loop_step_warn_ms: 250\nmodels_disk_budget_gb: 400\n", true, true},
	} {
		imp, err := ParseYAMLImport([]byte(c.yml), Bootstrap{ModelsDir: "/m"}, Options{FileExists: always})
		if err != nil {
			t.Fatal(err)
		}
		_, w := imp.Doc.Settings["loop_step_warn_ms"]
		_, b := imp.Doc.Settings["models_disk_budget_gb"]
		if w != c.warn || b != c.budg {
			t.Fatalf("%q: stored settings %v", c.yml, imp.Doc.Settings)
		}
	}
	cfg, err := Build(NewDocument(), Bootstrap{ModelsDir: "/m"}, Options{FileExists: always})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LoopStepWarnMs != DefaultLoopStepWarnMs || cfg.ModelsDiskBudgetGB != 0 {
		t.Fatalf("defaults %d %d", cfg.LoopStepWarnMs, cfg.ModelsDiskBudgetGB)
	}
	cfg, err = Build(&Document{Settings: map[string]any{"loop_step_warn_ms": 250.0, "models_disk_budget_gb": 400.0}}, Bootstrap{ModelsDir: "/m"}, Options{FileExists: always})
	if err != nil || cfg.LoopStepWarnMs != 250 || cfg.ModelsDiskBudgetGB != 400 {
		t.Fatalf("set: %v %+v", err, cfg)
	}
	for _, bad := range []map[string]any{{"loop_step_warn_ms": 5.0}, {"loop_step_warn_ms": float64(MaxLoopStepWarnMs + 1)}, {"models_disk_budget_gb": -1.0}} {
		if _, err := Build(&Document{Settings: bad}, Bootstrap{ModelsDir: "/m"}, Options{FileExists: always}); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
}
