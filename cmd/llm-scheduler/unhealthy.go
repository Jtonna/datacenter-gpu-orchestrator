package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// storeFailure is the reason a database that exists cannot be used, for
// /health and the log.
func storeFailure(err error) string {
	var ne *bench.NewerSchemaError
	if errors.As(err, &ne) {
		return ne.Error()
	}
	return "the database cannot be opened: " + err.Error()
}

// serveUnhealthy is how the scheduler runs when its database or stored
// configuration exists but cannot be used (a newer version migrated it, the
// file is damaged, the stored configuration is invalid): never healthy with
// an empty configuration, which an unattended rollback would take for
// success. It loads nothing and serves nothing that could write: GET
// /health and every other route answer 503 with the reason, until a signal
// stops it (exit 1: it never served). The reason is logged at ERROR.
func serveUnhealthy(boot config.Bootstrap, log *slog.Logger, sigs <-chan os.Signal, reason string) int {
	ln, err := net.Listen("tcp", listenAddr(boot))
	if err != nil {
		log.Error("cannot listen", "addr", listenAddr(boot), "err", err)
		return 1
	}
	log.Error("the scheduler is not serving: "+reason+"; /health answers 503 with this reason, nothing is loaded and nothing can be written; start the version that wrote the database, or restore a copy this version can read",
		"version", version, "listen", ln.Addr().String(), "data_dir", boot.DataDir)
	m := http.NewServeMux()
	m.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("unhealthy: " + reason + "\n"))
	})
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		enc.Encode(map[string]any{"error": map[string]any{"message": reason, "type": "unhealthy", "version": version}})
	})
	srv := &http.Server{Handler: m, ReadHeaderTimeout: 10 * time.Second}
	// If the server fails there is nothing left to serve: the process
	// ends (exit 1), as main's own server failure does.
	serveErr := make(chan error, 1)
	go supervise.Once("http.serve_unhealthy", func() { serveErr <- srv.Serve(ln) },
		func(p any) { serveErr <- fmt.Errorf("internal error: %v", p) })
	select {
	case <-sigs:
	case err := <-serveErr:
		log.Error("HTTP server failed; stopping (unhealthy: it never served)", "err", err, "reason", reason)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	log.Info("stopped (unhealthy: it never served)")
	return 1
}

// listenAddr is the listen address the configuration would give.
func listenAddr(b config.Bootstrap) string {
	if b.Listen == "" {
		return ":8080"
	}
	return b.Listen
}
