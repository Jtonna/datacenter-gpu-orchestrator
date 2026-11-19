package config

import "testing"

func TestHistoryConfigDefaultsAndValues(t *testing.T) {
	c := mustParse(t, "profiles: {}\n")
	if c.HistoryDir != "" || c.HistoryMaxFileMiB != 16 || c.HistoryKeepFiles != 20 || c.HistoryMemoryEvents != 5000 {
		t.Fatalf("defaults: %+v", c)
	}
	c = mustParse(t, "history_dir: /var/lib/llm-scheduler/history\nhistory_max_file_mib: 4\nhistory_keep_files: 3\nhistory_memory_events: 100\nprofiles: {}\n")
	if c.HistoryDir != "/var/lib/llm-scheduler/history" || c.HistoryMaxFileMiB != 4 || c.HistoryKeepFiles != 3 || c.HistoryMemoryEvents != 100 {
		t.Fatalf("set: %+v", c)
	}
}
