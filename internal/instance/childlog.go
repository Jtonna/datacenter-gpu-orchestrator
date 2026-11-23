package instance

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// DefaultLogKeep is how many child log files are kept when LogKeep is unset.
const DefaultLogKeep = 200

// LogFiler is implemented by a Child that keeps its full output in a file.
// LogFile is empty when it does not (the feature is off, or the file could
// not be created).
type LogFiler interface{ LogFile() string }

// childLogName matches the files this package creates, so retention never
// touches anything else in the directory.
var childLogName = regexp.MustCompile(`^\d{8}T\d{6}Z-.+\.log$`)

// childLogFileName is <UTC time>-<profile>-<instance id>.log, so a listing
// sorts by time and shows the profile. Anything in the profile name that is
// not safe in a file name (such as the ':' of gemma-4-12b:48k) becomes '_'.
func childLogFileName(at time.Time, profile string, id uint64) string {
	var b strings.Builder
	for _, r := range profile {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := b.String()
	if len(name) > 80 {
		name = name[:80]
	}
	if name == "" {
		name = "child"
	}
	return fmt.Sprintf("%s-%s-%d.log", at.UTC().Format("20060102T150405Z"), name, id)
}

// PrepareLogDir checks, at startup, that LogDir exists or can be created and
// can be written to. It does nothing when LogDir is empty.
func (l *ProcessLauncher) PrepareLogDir() error {
	if l.LogDir == "" {
		return nil
	}
	if err := os.MkdirAll(l.LogDir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(l.LogDir, ".probe-*")
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(f.Name())
}

func (l *ProcessLauncher) logger() *slog.Logger {
	if l.Logger != nil {
		return l.Logger
	}
	return slog.Default()
}

// openChildLog creates the log file for one child start and applies
// retention. It returns nil when the feature is off or the file cannot be
// created; in that case it warns once and the child starts without a file.
func (l *ProcessLauncher) openChildLog(profile string, id uint64) *fileSink {
	if l.LogDir == "" {
		return nil
	}
	f, err := l.createLogFile(profile, id)
	if err != nil {
		l.warnOnce.Do(func() {
			l.logger().Warn("cannot create a child log file; children run without one", "dir", l.LogDir, "err", err)
		})
		return nil
	}
	keep := l.LogKeep
	if keep <= 0 {
		keep = DefaultLogKeep
	}
	l.prune(keep, filepath.Base(f.Name()))
	return &fileSink{f: f, path: f.Name(), log: l.logger()}
}

func (l *ProcessLauncher) createLogFile(profile string, id uint64) (*os.File, error) {
	if err := os.MkdirAll(l.LogDir, 0o755); err != nil {
		return nil, err
	}
	name := childLogFileName(time.Now(), profile, id)
	for n := 0; ; n++ {
		p := filepath.Join(l.LogDir, name)
		if n > 0 {
			p = filepath.Join(l.LogDir, strings.TrimSuffix(name, ".log")+fmt.Sprintf("-%d.log", n))
		}
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			return f, nil
		}
		if !os.IsExist(err) || n >= 100 {
			return nil, err
		}
	}
}

// prune deletes the oldest of our log files beyond keep. The file just
// created is never deleted. Failures are ignored: retention is best effort.
func (l *ProcessLauncher) prune(keep int, current string) {
	ents, err := os.ReadDir(l.LogDir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range ents {
		if !e.IsDir() && childLogName.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // the time prefix makes name order time order
	for i := 0; len(names)-i > keep; i++ {
		if names[i] == current {
			continue
		}
		os.Remove(filepath.Join(l.LogDir, names[i]))
	}
}

// fileSink is an io.Writer that copies a child's output to its log file. It
// never fails: if the disk fills or the file goes bad it warns once, closes
// the file and drops the rest, so the child is never affected.
type fileSink struct {
	mu     sync.Mutex
	f      *os.File
	path   string
	log    *slog.Logger
	failed bool
}

func (s *fileSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer supervise.Unlocked("instance", s.mu.Unlock)
	if s.f == nil {
		return len(p), nil
	}
	if _, err := s.f.Write(p); err != nil && !s.failed {
		s.failed = true
		s.log.Warn("writing a child log file failed; the rest of this child's output is not kept in it", "file", s.path, "err", err)
		s.f.Close()
		s.f = nil
	}
	return len(p), nil
}

func (s *fileSink) Close() {
	s.mu.Lock()
	defer supervise.Unlocked("instance", s.mu.Unlock)
	if s.f != nil {
		s.f.Close()
		s.f = nil
	}
}
