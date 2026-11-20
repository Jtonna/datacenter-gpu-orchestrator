// Package exporters runs the stats exporters as supervised processes
// (agent contract 6.1 and 7.1, stats contract 2.3 and 2.5): systemd
// transient units potluck-node-exporter and potluck-nvidia-exporter on a
// bare-metal install, child processes in a Docker install. An exporter that
// is missing or keeps crashing only makes its stats source absent or stale;
// it never stops the node from taking loads.
package exporters

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/runner"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/stats"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// UnitName returns the unit (or child) name of an exporter.
func UnitName(program string) string {
	switch program {
	case agentproto.ProgramNodeExporter:
		return agentproto.NodeExporterUnit
	case agentproto.ProgramNvidiaGPUExporter:
		return agentproto.NvidiaExporterUnit
	}
	return "potluck-" + strings.ReplaceAll(program, "_", "-")
}

// SourceName maps an exporter program to its stats source name.
func SourceName(program string) string {
	switch program {
	case agentproto.ProgramNodeExporter:
		return agentproto.SourceNode
	case agentproto.ProgramNvidiaGPUExporter:
		return agentproto.SourceNvidia
	}
	return ""
}

// ListenAddr returns the --web.listen-address value of the arguments.
func ListenAddr(args []string) string {
	for i, a := range args {
		if a == "--web.listen-address" && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(a, "--web.listen-address="); ok {
			return v
		}
	}
	return ""
}

// Validate refuses an exporter whose listen address is not on 127.0.0.1
// (stats contract 2.5.2; the agent adds nothing to the arguments).
func Validate(spec agentproto.ExporterSpec) error {
	addr := ListenAddr(spec.Args)
	if !strings.HasPrefix(addr, "127.0.0.1:") || len(addr) == len("127.0.0.1:") {
		return fmt.Errorf("exporter %s: listen address %q is not 127.0.0.1:<port>; not started", spec.Name, addr)
	}
	return nil
}

// Supervisor keeps the configured exporters running.
type Supervisor struct {
	Runner runner.Runner
	// Path returns a verified program path.
	Path       func(name string) (string, bool)
	Logger     *slog.Logger
	MinBackoff time.Duration // default 1s
	MaxBackoff time.Duration // default 60s

	mu      sync.Mutex
	running map[string]*supervised
	errs    []string
}

type supervised struct {
	spec   agentproto.ExporterSpec
	cancel context.CancelFunc
	done   chan struct{}
}

func (s *Supervisor) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Set replaces the running set with specs. It returns the stats sources of
// the exporters it runs and the refusals.
func (s *Supervisor) Set(ctx context.Context, specs []agentproto.ExporterSpec) ([]stats.SourceConfig, []string) {
	s.mu.Lock()
	if s.running == nil {
		s.running = map[string]*supervised{}
	}
	s.mu.Unlock()
	want := map[string]agentproto.ExporterSpec{}
	var srcs []stats.SourceConfig
	var refused []string
	for _, sp := range specs {
		if err := Validate(sp); err != nil {
			refused = append(refused, err.Error())
			s.log().Error("exporter refused", "err", err)
			continue
		}
		if _, ok := s.Path(sp.Name); !ok {
			refused = append(refused, "exporter "+sp.Name+" is not a verified program")
			continue
		}
		want[sp.Name] = sp
		if src := SourceName(sp.Name); src != "" {
			srcs = append(srcs, stats.SourceConfig{Name: src, URL: "http://" + ListenAddr(sp.Args) + "/metrics"})
		}
	}
	s.mu.Lock()
	var stop []*supervised
	for name, r := range s.running {
		if w, ok := want[name]; !ok || strings.Join(w.Args, "\x00") != strings.Join(r.spec.Args, "\x00") {
			stop = append(stop, r)
			delete(s.running, name)
		}
	}
	var start []agentproto.ExporterSpec
	for name, sp := range want {
		if _, ok := s.running[name]; !ok {
			start = append(start, sp)
		}
	}
	s.mu.Unlock()
	for _, r := range stop {
		r.cancel()
		<-r.done
	}
	for _, sp := range start {
		c, cancel := context.WithCancel(ctx)
		r := &supervised{spec: sp, cancel: cancel, done: make(chan struct{})}
		s.mu.Lock()
		s.running[sp.Name] = r
		s.mu.Unlock()
		go s.loop(c, r)
	}
	return srcs, refused
}

// StopAll stops every exporter.
func (s *Supervisor) StopAll() {
	s.mu.Lock()
	var all []*supervised
	for _, r := range s.running {
		all = append(all, r)
	}
	s.running = map[string]*supervised{}
	s.mu.Unlock()
	for _, r := range all {
		r.cancel()
		<-r.done
	}
}

// StopLeftovers stops exporter units a previous agent left running, so their
// ports are free before the agent's port check (contract section 3).
func (s *Supervisor) StopLeftovers(ctx context.Context) {
	for _, u := range []string{agentproto.NodeExporterUnit, agentproto.NvidiaExporterUnit} {
		_ = s.Runner.Stop(ctx, u)
	}
}

func (s *Supervisor) loop(ctx context.Context, r *supervised) {
	defer close(r.done)
	unit := UnitName(r.spec.Name)
	minB, maxB := s.MinBackoff, s.MaxBackoff
	if minB == 0 {
		minB = time.Second
	}
	if maxB == 0 {
		maxB = 60 * time.Second
	}
	backoff := minB
	for {
		path, ok := s.Path(r.spec.Name)
		var proc runner.Proc
		var err error
		if ok {
			// A unit of that name left by a previous agent is replaced.
			_ = s.Runner.Stop(ctx, unit)
			proc, err = s.Runner.Start(ctx, runner.Spec{Unit: unit, Path: path, Args: r.spec.Args})
		} else {
			err = fmt.Errorf("%s is not verified", r.spec.Name)
		}
		if err != nil {
			s.log().Warn("exporter start failed", "name", r.spec.Name, "err", err)
		} else {
			startedAt := time.Now()
			select {
			case <-ctx.Done():
				_ = proc.Signal(15)
				select {
				case <-proc.Done():
				case <-time.After(5 * time.Second):
					_ = proc.Signal(9)
				}
				return
			case <-proc.Done():
				s.log().Warn("exporter exited", "name", r.spec.Name, "exit", proc.Exit())
				if time.Since(startedAt) > 2*maxB {
					backoff = minB // it ran for a while: not a crash loop
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxB {
			backoff = maxB
		}
	}
}
