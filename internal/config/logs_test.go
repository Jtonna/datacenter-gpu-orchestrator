package config

import (
	"strings"
	"testing"
)

// CR-10: the logging settings are ordinary live settings with these defaults.
func TestLogsSettingsDefaultsAndSchema(t *testing.T) {
	c, err := Build(NewDocument(), Bootstrap{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Logs != (Logs{Enabled: true, RetentionDays: 14, MaxDiskGiB: 20, ClientIntake: true}) {
		t.Fatalf("defaults = %+v", c.Logs)
	}
	if y := mustParse(t, "profiles: {}\n"); y.Logs != DefaultLogs() {
		t.Fatalf("a YAML configuration has %+v", y.Logs)
	}
	want := map[string]any{"logs_enabled": true, "logs_retention_days": 14.0, "logs_max_disk_gib": 20.0, "logs_push_url": "", "logs_client_intake": true}
	for k, def := range want {
		f, ok := SettingFieldByKey(ScopeSettings, k)
		if !ok || f.Section != "logs" || f.Default != def || f.RestartRequired || f.Description == "" {
			t.Errorf("%s field = %+v", k, f)
		}
	}
}

func TestLogsSettingsApplyAndValidate(t *testing.T) {
	d := NewDocument()
	d.Settings = map[string]any{"logs_enabled": false, "logs_retention_days": 30.0, "logs_max_disk_gib": 5.0,
		"logs_push_url": " https://logs.example:8443/push ", "logs_client_intake": false}
	c, err := Build(d, Bootstrap{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Logs != (Logs{Enabled: false, RetentionDays: 30, MaxDiskGiB: 5, PushURL: "https://logs.example:8443/push"}) {
		t.Fatalf("%+v", c.Logs)
	}
	for key, bad := range map[string]any{"logs_retention_days": 0.0, "logs_max_disk_gib": 0.0, "logs_push_url": "logs.example:9428",
		"logs_enabled": "yes", "logs_client_intake": 1.0} {
		d := NewDocument()
		d.Settings[key] = bad
		if _, err := Build(d, Bootstrap{}, Options{}); err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s = %v: want an error that names the key, got %v", key, bad, err)
		}
	}
	for _, bad := range []string{"ftp://logs.example/", "http://user:pw@logs.example/", "http:///x", "http://logs.example/#frag"} {
		d := NewDocument()
		d.Settings["logs_push_url"] = bad
		if _, err := Build(d, Bootstrap{}, Options{}); err == nil {
			t.Errorf("logs_push_url %q accepted", bad)
		}
	}
}
