// Package modelsrc is the node's model source (agent contract section 8):
// the share mount on a bare-metal install, the folder on a Docker install or
// a local directory, or push; and the share probe.
package modelsrc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/osx"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// Kinds of the --model-source option (section 9).
const (
	KindShare = "share" // share:<host>:<export>
	KindDir   = "dir"   // dir:<path>
	KindPush  = "push"
)

// Spec is a parsed --model-source value.
type Spec struct {
	Kind   string
	Target string // share: "192.0.2.1:/"; dir: the path
}

// ParseSpec parses share:<target>, dir:<path> or push.
func ParseSpec(s string) (Spec, error) {
	if s == KindPush {
		return Spec{Kind: KindPush}, nil
	}
	kind, rest, ok := strings.Cut(s, ":")
	if !ok || rest == "" {
		return Spec{}, fmt.Errorf("model source %q: want share:<host>:<export>, dir:<path> or push", s)
	}
	switch kind {
	case KindShare:
		host, export, ok := strings.Cut(rest, ":")
		if !ok || net.ParseIP(host) == nil || !strings.HasPrefix(export, "/") {
			return Spec{}, fmt.Errorf("share target %q: want <ip>:/<export>", rest)
		}
		return Spec{Kind: KindShare, Target: rest}, nil
	case KindDir:
		if !strings.HasPrefix(rest, "/") {
			return Spec{}, fmt.Errorf("dir %q must be an absolute path", rest)
		}
		return Spec{Kind: KindDir, Target: rest}, nil
	}
	return Spec{}, fmt.Errorf("model source kind %q unknown", kind)
}

// ShareHost returns the server address of a share target.
func (s Spec) ShareHost() string {
	h, _, _ := strings.Cut(s.Target, ":")
	return h
}

// Wire returns the registration's model_source.
//
// CONTRACT-QUESTION: section 9 offers dir:<path> at install, but section 8
// and the registration know only share and push. A dir node holds "the same
// files as the share" (the host machine's model folder itself is listed
// under share), so it is reported as kind share with the folder as target.
func (s Spec) Wire() agentproto.ModelSource {
	if s.Kind == KindPush {
		return agentproto.ModelSource{Kind: agentproto.ModelSourcePush}
	}
	return agentproto.ModelSource{Kind: agentproto.ModelSourceShare, Target: s.Target}
}

// MountArgs is the exact mount command of section 8 (without "mount").
// The server address is the share target's host; clientaddr is the node's
// address that reaches Potluck, computed at mount time.
//
// CONTRACT-QUESTION: the contract writes addr=<potluck ip> and
// "<potluck ip>:/" for the source; the agent uses the host and export of
// the configured share target, which is the same thing on every node the
// contract describes.
func MountArgs(target, nodeIP, mountPoint string) []string {
	host, _, _ := strings.Cut(target, ":")
	return []string{"-t", "nfs4", "-o", "ro,hard,vers=4.2,addr=" + host + ",clientaddr=" + nodeIP + ",rsize=1048576", target, mountPoint}
}

// Mounter mounts and unmounts.
type Mounter interface {
	IsMounted(path string) (bool, error)
	Mount(ctx context.Context, args []string) error
	// Unmount is a plain unmount; the agent never uses a lazy unmount.
	Unmount(ctx context.Context, path string) error
	MkdirAll(path string) error
}

// ProbeHandle is one running probe process.
type ProbeHandle interface {
	Done() <-chan struct{}
	Err() error // valid after Done
	Kill()
}

// ProbeStarter starts one probe process for a folder.
type ProbeStarter interface {
	Start(dir string) (ProbeHandle, error)
}

// Config configures a Source.
type Config struct {
	Spec Spec
	// Dir is the node's model folder: the mount point for a bare-metal share,
	// /models in a Docker install, the path for dir; "" for push.
	Dir string
	// Mount is true when the agent mounts the share itself (bare metal).
	Mount   bool
	NodeIP  func() string // address that reaches Potluck, read at mount time
	Every   time.Duration // probe period; default 5s
	Timeout time.Duration // probe timeout; default 2s
	// RemountAfter failed probes in a row with no launch; default 5.
	RemountAfter int
	MountTimeout time.Duration // default 60s
	Clock        osx.Clock
	Mounter      Mounter
	Prober       ProbeStarter
	LaunchExists func() bool
	Logger       *slog.Logger
	OnError      func(msg string)
}

// Source watches the model source.
type Source struct {
	cfg Config
	log *slog.Logger

	mu          sync.Mutex
	have        bool
	reachable   bool
	checkedAt   time.Duration
	fails       int
	outstanding ProbeHandle
	mounts      int
	unmounts    int
}

// New returns a Source.
func New(cfg Config) *Source {
	if cfg.Every == 0 {
		cfg.Every = 5 * time.Second
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 2 * time.Second
	}
	if cfg.RemountAfter == 0 {
		cfg.RemountAfter = 5
	}
	if cfg.MountTimeout == 0 {
		cfg.MountTimeout = 60 * time.Second
	}
	l := cfg.Logger
	if l == nil {
		l = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Source{cfg: cfg, log: l}
}

// Dir returns the model folder ("" for push).
func (s *Source) Dir() string { return s.cfg.Dir }

// Push reports a push node.
func (s *Source) Push() bool { return s.cfg.Spec.Kind == KindPush }

// Reachable is the result of the last finished probe.
func (s *Source) Reachable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.have && s.reachable
}

// State returns the heartbeat's model_source.
func (s *Source) State() agentproto.ModelSourceState {
	st := agentproto.ModelSourceState{Kind: s.cfg.Spec.Wire().Kind}
	if s.Push() {
		return st
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.have && s.reachable
	st.Reachable = &r
	if s.have {
		age := float64((s.cfg.Clock.Now() - s.checkedAt).Milliseconds()) / 1000
		st.CheckedAgeS = &age
	}
	return st
}

// Counts returns how many mounts and unmounts the source has done (tests).
func (s *Source) Counts() (mounts, unmounts int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mounts, s.unmounts
}

// Start mounts the share when the agent manages it: an existing mount at the
// mount point is used as it is; it never mounts twice.
func (s *Source) Start(ctx context.Context) error {
	if !s.cfg.Mount || s.Push() {
		return nil
	}
	return s.ensureMounted(ctx)
}

func (s *Source) ensureMounted(ctx context.Context) error {
	ok, err := s.cfg.Mounter.IsMounted(s.cfg.Dir)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	if err := s.cfg.Mounter.MkdirAll(s.cfg.Dir); err != nil {
		return err
	}
	ip := ""
	if s.cfg.NodeIP != nil {
		ip = s.cfg.NodeIP()
	}
	if ip == "" {
		return errors.New("no node address for clientaddr")
	}
	c, cancel := context.WithTimeout(ctx, s.cfg.MountTimeout)
	defer cancel()
	s.mu.Lock()
	s.mounts++
	s.mu.Unlock()
	if err := s.cfg.Mounter.Mount(c, MountArgs(s.cfg.Spec.Target, ip, s.cfg.Dir)); err != nil {
		return err
	}
	s.log.Info("share mounted", "target", s.cfg.Spec.Target, "at", s.cfg.Dir)
	return nil
}

// Run probes every Every until ctx ends.
func (s *Source) Run(ctx context.Context) {
	if s.Push() {
		return
	}
	t := time.NewTicker(s.cfg.Every)
	defer t.Stop()
	s.ProbeOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.ProbeOnce(ctx)
		}
	}
}

// record keeps a probe result. One log line at the first result and at each
// change of result; a result that repeats is not logged again.
func (s *Source) record(ok bool, why string) {
	s.mu.Lock()
	changed := !s.have || s.reachable != ok
	s.have, s.reachable, s.checkedAt = true, ok, s.cfg.Clock.Now()
	if ok {
		s.fails = 0
	} else {
		s.fails++
	}
	s.mu.Unlock()
	switch {
	case !changed:
		if !ok {
			s.log.Debug("share probe failed", "why", why)
		}
	case ok:
		s.log.Info("share probe ok: the model source is reachable", "target", s.cfg.Spec.Target, "dir", s.cfg.Dir)
	default:
		s.log.Warn("share probe failed: the model source is not reachable", "target", s.cfg.Spec.Target, "dir", s.cfg.Dir, "why", why)
	}
}

// ProbeOnce runs one probe round. At most one probe process is outstanding:
// while one hangs, the result is "unreachable" and no new probe starts.
func (s *Source) ProbeOnce(ctx context.Context) {
	s.mu.Lock()
	out := s.outstanding
	s.mu.Unlock()
	if out != nil {
		select {
		case <-out.Done():
			s.mu.Lock()
			s.outstanding = nil
			s.mu.Unlock()
		default:
			s.record(false, "previous probe still hangs")
			s.maybeRemount(ctx)
			return
		}
	}
	if s.cfg.Mount {
		if ok, err := s.cfg.Mounter.IsMounted(s.cfg.Dir); err != nil || !ok {
			s.record(false, "share is not mounted")
			s.maybeRemount(ctx)
			return
		}
	}
	h, err := s.cfg.Prober.Start(s.cfg.Dir)
	if err != nil {
		s.record(false, err.Error())
		return
	}
	timer := time.NewTimer(s.cfg.Timeout)
	defer timer.Stop()
	select {
	case <-h.Done():
		if err := h.Err(); err != nil {
			s.record(false, err.Error())
		} else {
			s.record(true, "")
		}
	case <-timer.C:
		h.Kill()
		s.mu.Lock()
		s.outstanding = h
		s.mu.Unlock()
		s.record(false, "probe timed out")
	case <-ctx.Done():
		h.Kill()
		return
	}
	s.maybeRemount(ctx)
}

// maybeRemount applies the remount rule of section 8: only for a mount the
// agent manages, only after RemountAfter failed probes in a row, and never
// while a launch exists. A remount is a plain unmount followed by a mount.
func (s *Source) maybeRemount(ctx context.Context) {
	if !s.cfg.Mount {
		return
	}
	s.mu.Lock()
	fails := s.fails
	s.mu.Unlock()
	if fails < s.cfg.RemountAfter {
		return
	}
	if s.cfg.LaunchExists != nil && s.cfg.LaunchExists() {
		return
	}
	s.mu.Lock()
	s.fails = 0
	s.mu.Unlock()
	if ok, _ := s.cfg.Mounter.IsMounted(s.cfg.Dir); ok {
		c, cancel := context.WithTimeout(ctx, s.cfg.MountTimeout)
		s.mu.Lock()
		s.unmounts++
		s.mu.Unlock()
		err := s.cfg.Mounter.Unmount(c, s.cfg.Dir)
		cancel()
		if err != nil {
			s.log.Error("unmount for remount failed", "err", err)
			if s.cfg.OnError != nil {
				s.cfg.OnError("share unmount failed: " + err.Error())
			}
			return
		}
	}
	if err := s.ensureMounted(ctx); err != nil {
		s.log.Error("mount failed", "err", err)
		if s.cfg.OnError != nil {
			s.cfg.OnError("share mount failed: " + err.Error())
		}
	}
}
