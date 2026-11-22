package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgsvc"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/logrelay"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/release"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/startups"
)

// newPool builds the agent membership: the program release is loaded and
// verified first (agent contract 5), then the pool with its stored node state.
func newPool(ctx context.Context, svc *cfgsvc.Service, cs *cfgstore.Store, db *bench.Store, logDir string, logs *logrelay.Relay, log *slog.Logger) (*pool.Pool, error) {
	cfg := svc.Config()
	o := pool.Options{Config: svc.Config, Programs: loadPrograms(ctx, cfg, log), PotluckRelease: potluckRelease,
		Logger: log, LogDir: logDir}
	if cs != nil {
		o.Store = cs
	} else {
		log.Warn("the database is not available: node pause and removal are kept in memory only")
	}
	// Start-up records live in the scheduler database, beside the bench and
	// configuration tables; without it they are kept in memory only.
	if db != nil {
		st, err := startups.Open(db)
		if err != nil {
			log.Warn("start-up records are kept in memory only", "err", err)
		} else {
			o.Startups = st
		}
	}
	if logs != nil {
		o.Logs = logs.Object // CR-10: the logs object of the registration answer
	}
	o.AgentPort = envPort(envTestAgentPort, log)
	o.WorkerPort = envPort(envTestWorkerPort, log)
	log.Info("membership is agents: nodes come from potluck-agent registrations; the fleet feed is not read",
		"release", potluckRelease, "programs_dir", cfg.ProgramsDir)
	return pool.New(o)
}

func envPort(name string, log *slog.Logger) int {
	v := os.Getenv(name)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 || n > 65535 {
		log.Warn("ignoring a bad test port override", "env", name, "value", v)
		return 0
	}
	log.Warn("test port override in effect; never set this in production", "env", name, "port", n)
	return n
}

// loadPrograms loads programs/<release> and checks the same-build rule (5.1).
// A failure does not stop the scheduler: it is logged, shown in /api/system,
// and agents and loads are refused with the reason.
func loadPrograms(ctx context.Context, cfg *config.Config, log *slog.Logger) pool.Programs {
	rel, err := release.Load(cfg.ProgramsDir, potluckRelease)
	if err != nil {
		log.Error("the program release is not served; agents will be refused", "programs_dir", cfg.ProgramsDir, "release", potluckRelease, "err", err)
		return pool.Programs{Err: err.Error()}
	}
	log.Info("program release verified", "release", rel.Release, "llama_build", rel.Builds["llama"], "folders", len(rel.Folders))
	pr := pool.Programs{Release: rel}
	pr.HostBuild, pr.HostErr = hostBuildCheck(ctx, cfg.Defaults.ServerBin, rel.Builds["llama"])
	if pr.HostErr != "" {
		log.Error("same-build rule: loads are refused", "server_bin", cfg.Defaults.ServerBin, "reason", pr.HostErr)
	}
	return pr
}

// hostBuildCheck runs the host program's --version and compares its commit
// with the release's llama build.
func hostBuildCheck(ctx context.Context, bin, want string) (got, problem string) {
	got, err := release.HostBuild(ctx, bin)
	if err != nil {
		return "", fmt.Sprintf("the build of the host program cannot be read, so the same-build rule cannot be checked: %v", err)
	}
	if !release.SameCommit(got, want) {
		return got, fmt.Sprintf("the host program %s is llama build %s, but release %s needs build %s on every member of a load; install the release's llama-server as server_bin",
			bin, got, potluckRelease, want)
	}
	return got, ""
}
