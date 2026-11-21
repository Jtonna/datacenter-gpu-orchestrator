package config

import (
	"strings"
	"testing"
)

// CR-9 review, A3: the description of forced_fit_check says that agent
// machines are checked whatever the setting is.
func TestForcedFitCheckDescriptionNamesTheAgentRefusal(t *testing.T) {
	f, ok := SettingFieldByKey("settings", "forced_fit_check")
	if !ok {
		t.Fatal("no forced_fit_check field")
	}
	if !strings.Contains(f.Description, "agents") || !strings.Contains(f.Description, "cannot be switched off") {
		t.Fatalf("description %q", f.Description)
	}
}
