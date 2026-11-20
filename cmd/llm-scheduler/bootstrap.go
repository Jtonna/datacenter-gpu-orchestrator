package main

import (
	"flag"
	"os"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

// Environment variables for the bootstrap values; a flag wins over its variable.
const (
	envListen      = "LLM_SCHEDULER_LISTEN"
	envDataDir     = "LLM_SCHEDULER_DATA_DIR"
	envModelsDir   = "LLM_SCHEDULER_MODELS_DIR"
	envHistoryDir  = "LLM_SCHEDULER_HISTORY_DIR"
	envChildLogDir = "LLM_SCHEDULER_CHILD_LOG_DIR"
)

// Built-in bootstrap defaults (the container layout).
const (
	defaultListen  = ":8080"
	defaultDataDir = "/var/lib/llm-scheduler/data"
	defaultModels  = "/models"
)

type bootFlags struct {
	listen, dataDir, modelsDir, historyDir, childLogDir *string
}

func envOr(name, def string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return def
}

// bindBootstrapDefaults declares the deployment flags. With withDefaults
// false the unset values stay empty (the import command lets the YAML file
// fill them in).
func bindBootstrapDefaults(fs *flag.FlagSet, withDefaults bool) bootFlags {
	d := func(env, def string) string {
		if !withDefaults {
			def = ""
		}
		return envOr(env, def)
	}
	return bootFlags{
		listen:      fs.String("listen", d(envListen, defaultListen), "address to listen on ("+envListen+")"),
		dataDir:     fs.String("data-dir", d(envDataDir, defaultDataDir), "directory of the scheduler database, scheduler.db ("+envDataDir+")"),
		modelsDir:   fs.String("models-dir", d(envModelsDir, defaultModels), "directory holding the model files ("+envModelsDir+")"),
		historyDir:  fs.String("history-dir", envOr(envHistoryDir, ""), "directory for the event history; empty turns history off ("+envHistoryDir+")"),
		childLogDir: fs.String("child-log-dir", envOr(envChildLogDir, ""), "directory for each child full output; empty keeps none ("+envChildLogDir+")"),
	}
}

func bindBootstrap(fs *flag.FlagSet) bootFlags { return bindBootstrapDefaults(fs, true) }

func (b bootFlags) value() config.Bootstrap {
	return config.Bootstrap{Listen: *b.listen, DataDir: *b.dataDir, ModelsDir: *b.modelsDir, HistoryDir: *b.historyDir, ChildLogDir: *b.childLogDir}
}
