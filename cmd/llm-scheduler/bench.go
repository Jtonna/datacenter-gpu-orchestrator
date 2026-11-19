package main

import (
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/bench"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/config"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/history"
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

// openBench opens the benchmark database and builds the manager. When
// data_dir is unset, bench is disabled, or the database cannot be opened, the
// feature is off (nil, nil): its endpoints answer 503 and nothing else is
// affected.
func openBench(cfg *config.Config, ln net.Listener, hist *history.Store, log *slog.Logger) (*bench.Store, *bench.Manager) {
	if !cfg.Bench.Enabled {
		return nil, nil
	}
	st, err := bench.OpenStore(cfg.DataDir)
	if err != nil {
		log.Warn("the benchmark database cannot be opened; the benchmark feature is off (everything else works)", "data_dir", cfg.DataDir, "err", err)
		return nil, nil
	}
	o := bench.ManagerOptions{Store: st, API: &bench.HTTPAPI{Base: loopbackURL(ln.Addr())}, Version: version, Logger: log,
		Thresholds: bench.Thresholds{LoadPct: cfg.Bench.RegressLoadPct, DecodePct: cfg.Bench.RegressDecodePct},
		MaxRun:     time.Duration(cfg.Bench.MaxRunMinutes) * time.Minute,
		ProfileOK:  func(id string) bool { _, _, ok := cfg.Resolve(id); return ok }}
	if hist.On() {
		o.Record = hist.Record
	}
	m, err := bench.NewManager(o)
	if err != nil {
		log.Warn("the benchmark manager cannot start; the benchmark feature is off", "err", err)
		st.Close()
		return nil, nil
	}
	m.Seed(cfg.Bench.Benchmarks, cfg.Bench.Suites)
	log.Info("benchmark feature on", "database", st.Path(), "benchmarks_seeded", len(cfg.Bench.Benchmarks), "suites_seeded", len(cfg.Bench.Suites))
	return st, m
}
