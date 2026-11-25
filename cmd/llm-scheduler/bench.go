package main

import (
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgsvc"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
)

// loopbackURL is where the benchmark runner reaches the scheduler's own API.
func loopbackURL(ln net.Addr) string {
	host, port := "127.0.0.1", "8080"
	if a, ok := ln.(*net.TCPAddr); ok {
		port = fmt.Sprint(a.Port)
		if a.IP != nil && !a.IP.IsUnspecified() {
			host = a.IP.String()
			if a.IP.To4() == nil {
				host = "[" + host + "]"
			}
		}
	}
	return "http://" + host + ":" + port
}

// benchmarksUsing lists the benchmarks that refer to a profile (for the delete warning).
func benchmarksUsing(st *bench.Store, profile string) []string {
	if st == nil || st.BenchOff() != nil { // a newer version's bench tables are not read
		return nil
	}
	bms, err := st.ListBenchmarks()
	if err != nil {
		return nil
	}
	var out []string
	for _, b := range bms {
		if b.Spec != nil && b.Spec.Profile == profile {
			out = append(out, b.Spec.ID)
		}
	}
	return out
}

// openBench builds the benchmark manager on the shared database. When bench is
// disabled by its setting or the database is not open, the feature is off
// (nil, nil): its endpoints answer 503 and nothing else is affected.
func openBench(svc *cfgsvc.Service, st *bench.Store, ln net.Listener, hist *history.Store, log *slog.Logger) (*bench.Store, *bench.Manager) {
	cfg := svc.Config()
	if !cfg.Bench.Enabled || st == nil || st.BenchOff() != nil { // BenchOff: warned at start (main.go)
		return nil, nil
	}
	o := bench.ManagerOptions{Store: st, API: &bench.HTTPAPI{Base: loopbackURL(ln.Addr())}, Version: version, Logger: log,
		Thresholds: bench.Thresholds{LoadPct: cfg.Bench.RegressLoadPct, DecodePct: cfg.Bench.RegressDecodePct},
		MaxRun:     time.Duration(cfg.Bench.MaxRunMinutes) * time.Minute,
		// Read when each run starts, so a change of bench_max_run_minutes applies to the next run without a restart.
		MaxRunSetting: func() time.Duration { return time.Duration(svc.Config().Bench.MaxRunMinutes) * time.Minute },
		ProfileOK:     func(id string) bool { _, _, ok := svc.Config().Resolve(id); return ok },
		// Read at every check, so a change of bench_retention_days applies without a restart.
		Retention: func() time.Duration { return time.Duration(svc.Config().Bench.RetentionDays) * 24 * time.Hour }}
	if hist.On() {
		o.Record = hist.Record
	}
	m, err := bench.NewManager(o)
	if err != nil {
		log.Warn("the benchmark manager cannot start; the benchmark feature is off", "err", err)
		return nil, nil
	}
	log.Info("benchmark feature on", "database", st.Path())
	return st, m
}
