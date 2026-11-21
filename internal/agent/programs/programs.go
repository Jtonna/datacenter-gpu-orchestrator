// Package programs fetches and verifies the programs Potluck hands out
// (agent contract 5.3): llama-server, rpc-server and the exporters.
//
// Rules implemented here:
//   - at start, every *.part file in the program folder is deleted (nothing
//     is resumed) and the files held are re-hashed;
//   - fetching happens only while the node is idle;
//   - free space is checked first;
//   - a download goes to <name>.part, is checked against the sha256 from the
//     registration, renamed into place and made executable;
//   - a failed check is retried three times, then reported;
//   - the agent never runs a file whose check failed (Path only returns
//     verified files);
//   - the llama programs' shared libraries are checked before ready (5.2);
//   - an old release folder is deleted only after the new one is verified
//     and when no launch exists.
package programs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// Config configures a Manager.
type Config struct {
	Root     string // folder holding one sub-folder per release
	Release  string // the agent's release
	BaseURL  string // "http://<potluck addr:port>"; program paths are appended
	Client   *http.Client
	Retries  int // attempts per file; default 3
	DiskFree func(path string) (uint64, error)
	// MissingLibs returns the shared libraries a program needs that are
	// absent. Nil skips the check.
	MissingLibs func(path string) ([]string, error)
	// LibChecked lists the programs whose libraries are checked.
	LibChecked []string
	Logger     *slog.Logger
	// RetryWait is the pause between attempts; default 2s.
	RetryWait time.Duration
}

// Manager holds the program set of one release.
type Manager struct {
	cfg Config
	log *slog.Logger

	mu       sync.Mutex
	held     map[string]string                  // name -> sha256 of the file on disk
	verified map[string]agentproto.ProgramEntry // checked against the current answer
	ready    bool
	errMsg   string
}

// New returns a Manager.
func New(cfg Config) *Manager {
	if cfg.Retries <= 0 {
		cfg.Retries = 3
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 30 * time.Minute}
	}
	if cfg.RetryWait == 0 {
		cfg.RetryWait = 2 * time.Second
	}
	l := cfg.Logger
	if l == nil {
		l = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Manager{cfg: cfg, log: l, held: map[string]string{}, verified: map[string]agentproto.ProgramEntry{}}
}

// Dir is the current release's folder.
func (m *Manager) Dir() string { return filepath.Join(m.cfg.Root, m.cfg.Release) }

// Prepare runs at agent start: delete *.part files, re-hash what is held.
// It returns the held programs for the registration.
func (m *Manager) Prepare() ([]agentproto.ProgramHash, error) {
	if err := os.MkdirAll(m.Dir(), 0o755); err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(m.Dir())
	if err != nil {
		return nil, err
	}
	held := map[string]string{}
	for _, e := range ents {
		p := filepath.Join(m.Dir(), e.Name())
		if strings.HasSuffix(e.Name(), ".part") {
			os.Remove(p)
			continue
		}
		if !e.Type().IsRegular() {
			continue
		}
		sum, err := hashFile(p)
		if err != nil {
			continue
		}
		held[e.Name()] = sum
	}
	m.mu.Lock()
	m.held = held
	m.mu.Unlock()
	return m.Held(), nil
}

// Held returns the held programs, sorted by name.
func (m *Manager) Held() []agentproto.ProgramHash {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []agentproto.ProgramHash{}
	for n, s := range m.held {
		out = append(out, agentproto.ProgramHash{Name: n, SHA256: s})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Ready reports programs_ready.
func (m *Manager) Ready() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.ready }

// Error reports programs_error, nil when none.
func (m *Manager) Error() *string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.errMsg == "" {
		return nil
	}
	s := m.errMsg
	return &s
}

// Path returns the path of a verified program.
func (m *Manager) Path(name string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.verified[name]; !ok {
		return "", false
	}
	return filepath.Join(m.Dir(), name), true
}

// Build returns a verified program's build string.
func (m *Manager) Build(name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.verified[name].Build
}

func (m *Manager) setError(format string, a ...any) error {
	msg := fmt.Sprintf(format, a...)
	m.mu.Lock()
	m.errMsg, m.ready = msg, false
	m.mu.Unlock()
	m.log.Error("programs", "error", msg)
	return errors.New(msg)
}

func validName(n string) bool {
	return n != "" && !strings.ContainsAny(n, `/\`) && n != "." && n != ".." && !strings.HasSuffix(n, ".part")
}

// Sync brings the held set in line with the registration answer. waitIdle
// blocks until the node is idle (fetching happens only then) and returns
// false when ctx ended. On success the set is verified and ready; on failure
// programs_error says why.
func (m *Manager) Sync(ctx context.Context, entries []agentproto.ProgramEntry, waitIdle func(context.Context) bool) error {
	m.mu.Lock()
	m.ready = false
	m.mu.Unlock()
	began := time.Now()
	var need []agentproto.ProgramEntry
	verified := map[string]agentproto.ProgramEntry{}
	for _, e := range entries {
		if !validName(e.Name) {
			return m.setError("program name %q is not a plain file name", e.Name)
		}
		m.mu.Lock()
		have := m.held[e.Name]
		m.mu.Unlock()
		if have != "" && strings.EqualFold(have, e.SHA256) {
			verified[e.Name] = e
			continue
		}
		need = append(need, e)
	}
	if len(need) > 0 {
		if waitIdle != nil && !waitIdle(ctx) {
			return ctx.Err()
		}
		var total uint64
		for _, e := range need {
			if e.Bytes > 0 {
				total += uint64(e.Bytes)
			}
		}
		if m.cfg.DiskFree != nil {
			free, err := m.cfg.DiskFree(m.Dir())
			if err != nil {
				return m.setError("cannot read free space of %s: %v", m.Dir(), err)
			}
			if free < total {
				return m.setError("not enough free space in %s: need %d MiB, have %d MiB", m.Dir(), total>>20, free>>20)
			}
		}
		for _, e := range need {
			if err := m.fetch(ctx, e); err != nil {
				return m.setError("%s: %v", e.Name, err)
			}
			verified[e.Name] = e
		}
	}
	// Files the answer does not name are not used.
	m.mu.Lock()
	m.verified = verified
	m.mu.Unlock()
	if m.cfg.MissingLibs != nil {
		var problems []string
		for _, name := range m.cfg.LibChecked {
			p, ok := m.Path(name)
			if !ok {
				continue
			}
			miss, err := m.cfg.MissingLibs(p)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", name, err))
				continue
			}
			if len(miss) > 0 {
				problems = append(problems, fmt.Sprintf("%s needs %s", name, strings.Join(miss, ", ")))
			}
		}
		if len(problems) > 0 {
			return m.setError("missing libraries: %s", strings.Join(problems, "; "))
		}
	}
	m.mu.Lock()
	m.ready, m.errMsg = true, ""
	m.mu.Unlock()
	var fetched int64
	for _, e := range need {
		fetched += e.Bytes
	}
	m.log.Info("programs downloaded and verified", "count", len(verified), "downloaded", len(need), "bytes", fetched,
		"seconds", math.Round(time.Since(began).Seconds()*10)/10)
	return nil
}

func (m *Manager) fetch(ctx context.Context, e agentproto.ProgramEntry) error {
	var last error
	for attempt := 1; attempt <= m.cfg.Retries; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(m.cfg.RetryWait):
			}
		}
		last = m.fetchOnce(ctx, e)
		if last == nil {
			return nil
		}
		m.log.Warn("program download failed", "name", e.Name, "attempt", attempt, "err", last)
	}
	return fmt.Errorf("failed %d times: %w", m.cfg.Retries, last)
}

func (m *Manager) fetchOnce(ctx context.Context, e agentproto.ProgramEntry) error {
	if !strings.HasPrefix(e.Path, "/") {
		return fmt.Errorf("path %q is not relative to Potluck's address", e.Path)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.cfg.BaseURL+e.Path, nil)
	if err != nil {
		return err
	}
	resp, err := m.cfg.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if m.cfg.DiskFree != nil && resp.ContentLength > 0 {
		if free, err := m.cfg.DiskFree(m.Dir()); err == nil && free < uint64(resp.ContentLength) {
			return fmt.Errorf("not enough free space: need %d MiB, have %d MiB", resp.ContentLength>>20, free>>20)
		}
	}
	final := filepath.Join(m.Dir(), e.Name)
	part := final + ".part"
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(part)
		return err
	}
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		os.Remove(part)
		return fmt.Errorf("short download: %d of %d bytes", n, resp.ContentLength)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(sum, e.SHA256) {
		os.Remove(part)
		return fmt.Errorf("sha256 mismatch: got %s, want %s", sum, e.SHA256)
	}
	if err := os.Chmod(part, 0o755); err != nil {
		os.Remove(part)
		return err
	}
	if err := os.Rename(part, final); err != nil {
		os.Remove(part)
		return err
	}
	m.mu.Lock()
	m.held[e.Name] = sum
	m.mu.Unlock()
	return nil
}

// DeleteOldReleases removes every release folder other than the current one,
// only when the current set is verified and no launch exists.
func (m *Manager) DeleteOldReleases(launchExists bool) []string {
	if launchExists || !m.Ready() {
		return nil
	}
	ents, err := os.ReadDir(m.cfg.Root)
	if err != nil {
		return nil
	}
	var removed []string
	for _, e := range ents {
		if !e.IsDir() || e.Name() == m.cfg.Release {
			continue
		}
		if os.RemoveAll(filepath.Join(m.cfg.Root, e.Name())) == nil {
			removed = append(removed, e.Name())
		}
	}
	return removed
}
