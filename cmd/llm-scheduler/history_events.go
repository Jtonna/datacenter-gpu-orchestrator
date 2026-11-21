package main

import (
	"fmt"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
)

// startedEvent is the schedulerStarted history record: the version, how many
// profiles there are and the safety settings the run starts with.
func startedEvent(cfg *config.Config, version string, at time.Time) history.Event {
	data := map[string]any{
		"version": version, "profiles": len(cfg.Profiles), "listen": cfg.Listen,
		"safety":  safetySummary(cfg),
		"history": map[string]any{"max_file_mib": cfg.HistoryMaxFileMiB, "keep_files": cfg.HistoryKeepFiles, "memory_events": cfg.HistoryMemoryEvents},
	}
	return history.Event{Ts: at.UnixMilli(), Type: history.TSchedulerStarted, Severity: history.Info, Data: data,
		Summary: fmt.Sprintf("llm-scheduler %s started with %d profiles.", version, len(cfg.Profiles))}
}

// stoppingEvent is the schedulerStopping record, written when shutdown begins.
func stoppingEvent(signal string, at time.Time) history.Event {
	return history.Event{Ts: at.UnixMilli(), Type: history.TSchedulerStopping, Severity: history.Info,
		Data:    map[string]any{"reason": signal},
		Summary: fmt.Sprintf("llm-scheduler is stopping (%s).", signal)}
}

// safetySummary is the safety settings as the schedulerStarted event and
// GET /api/system show them.
func safetySummary(cfg *config.Config) map[string]any {
	d := cfg.Defaults
	return map[string]any{
		"split_skew_margin": d.SplitSkewMargin, "estimate_margin": d.EstimateMargin,
		"load_memory_floor_mib": d.LoadMemoryFloorMiB, "forced_fit_check": d.ForcedFitCheck,
		"load_stall_timeout_s": d.LoadStallTimeout.Seconds(), "worker_silence_s": d.WorkerSilence.Seconds(),
		"worker_probe_interval_s": d.WorkerProbeInterval.Seconds(),
		"recovery_enabled":        cfg.Recovery.Enabled, "recovery_actions": cfg.Recovery.Actions,
		"exclude_nodes": len(cfg.ExcludeNodes),
		"no_auto_nodes": len(cfg.NoAutoNodes),
	}
}
