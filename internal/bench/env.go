package bench

import (
	"context"
	"path"
	"sort"
	"strings"
	"time"
)

// llamaBinary is the program of a child's command line (the first word, without
// its directory), or "" when it cannot be told.
func llamaBinary(cmd string) string {
	f := strings.Fields(cmd)
	if len(f) == 0 {
		return ""
	}
	// an "env X=1 prog" prefix is not a program
	for len(f) > 1 && strings.Contains(f[0], "=") {
		f = f[1:]
	}
	return path.Base(strings.ReplaceAll(f[0], `\`, "/"))
}

// noteEnv merges what a status shows into the run's environment: the workers
// and their rpc-server versions, the configuration revision and, once an
// instance exists, the llama binary.
func (x *exec) noteEnv(s *StatusInfo, in *InstanceInfo) {
	if s == nil {
		return
	}
	x.a.mu.Lock()
	defer x.a.mu.Unlock()
	e := &x.a.env
	if s.ConfigRevision != "" {
		e.ConfigRevision = s.ConfigRevision
	}
	if in != nil && e.LlamaBinary == "" {
		e.LlamaBinary = llamaBinary(in.Cmd)
	}
	have := map[string]int{}
	for i, w := range e.Workers {
		have[w.Node] = i
	}
	for _, w := range s.Workers {
		if w.Local {
			continue
		}
		name := w.Hostname
		if name == "" {
			name = w.ID
		}
		if i, ok := have[name]; ok {
			if w.RPCServerVersion != "" {
				e.Workers[i].RPCServerVersion = w.RPCServerVersion
			}
			continue
		}
		have[name] = len(e.Workers)
		e.Workers = append(e.Workers, WorkerEnv{Node: name, RPCServerVersion: w.RPCServerVersion})
	}
	sort.Slice(e.Workers, func(i, j int) bool { return e.Workers[i].Node < e.Workers[j].Node })
}

// captureEnv reads the scheduler's status once at the start of the run.
func (x *exec) captureEnv() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := x.m.o.API.Status(ctx)
	if err != nil {
		return
	}
	var in *InstanceInfo
	if len(s.Instances) > 0 {
		in = &s.Instances[0]
	}
	x.noteEnv(s, in)
	env := x.envSnapshot() // a copy under the lock: the runner keeps adding to it
	x.a.setRun(func(r *Run) { r.Env = env })
	x.m.o.Store.SetRunEnv(x.a.run.ID, env)
}

func (x *exec) envSnapshot() *Environment {
	x.a.mu.Lock()
	defer x.a.mu.Unlock()
	e := x.a.env
	e.Workers = append([]WorkerEnv(nil), x.a.env.Workers...)
	return &e
}
