package main

import (
	"log/slog"
	"os"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/api"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/logrelay"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/selflog"
)

// envLogsStoreURL is the address at which Potluck reaches the log store
// (CR-10), for example http://potluck-logs:9428. It is a deployment fact, so
// it is not part of the stored configuration. Unset: central logging is off
// (no logs object is handed out and the log routes answer 404), unless the
// setting logs_push_url names another address for the senders.
const envLogsStoreURL = "LLM_SCHEDULER_LOGS_STORE_URL"

// newLogRelay builds the pass-through in front of the log store. conf and
// getPool are read on every log request and must not block. A store address
// that cannot be used is logged and taken as unset: Potluck schedules exactly
// as before, without central logging.
func newLogRelay(conf func() *config.Config, getPool func() *pool.Pool, log *slog.Logger) *logrelay.Relay {
	node, launch := api.LogLookups(getPool)
	o := logrelay.Options{StoreURL: strings.TrimSpace(os.Getenv(envLogsStoreURL)), Logger: log, Node: node, Launch: launch,
		Config: func() config.Logs { return conf().Logs }}
	r, err := logrelay.New(o)
	if err != nil {
		log.Error("the log store address is not usable; central logging is off", "env", envLogsStoreURL, "value", o.StoreURL, "err", err)
		o.StoreURL = ""
		if r, err = logrelay.New(o); err != nil {
			log.Error("central logging", "err", err)
			return nil
		}
	}
	switch {
	case r.HasStore():
		log.Info("central logging: log lines are passed through to the log store", "store", o.StoreURL, "enabled", r.On())
	case r.On():
		log.Info("central logging: senders are told to push to logs_push_url; Potluck has no log store address of its own", "env", envLogsStoreURL)
	default:
		log.Info("central logging is off: no log store address", "env", envLogsStoreURL)
	}
	return r
}

// newSelfLog builds Potluck's own log shipper (CR-10, design 4.5): its lines
// (source scheduler) and its children's output (source host) go straight to
// the store's intake while the relay has a store and logs_enabled is on.
// relay and conf may return nil until they exist.
func newSelfLog(relay func() *logrelay.Relay, conf func() *config.Config) *selflog.Shipper {
	return selflog.New(selflog.Options{
		On: func() bool { r := relay(); return r != nil && r.StoreOn() },
		InsertURL: func(f []logrelay.Field) string {
			if r := relay(); r != nil {
				return r.InsertURL(f)
			}
			return ""
		},
		Node: func() (string, string) {
			// The local node, as the pool and the fleet client name it.
			if c := conf(); c != nil && c.Fleet.LocalNode != "" {
				return c.Fleet.LocalNode, c.Fleet.LocalNode
			}
			return "local", "local"
		},
		Release: potluckRelease,
	})
}
