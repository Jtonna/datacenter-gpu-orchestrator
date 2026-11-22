package modelsrc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unsafe"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/osx"
)

// ProbeCommand is the agent's hidden sub-command that runs one probe.
const ProbeCommand = "probe-read"

// ErrNoModelFile is returned by ProbeRead for a folder with no file.
//
// CONTRACT-QUESTION: the probe "reads the first 4 KiB of one model file"; a
// folder with no file cannot be probed. The agent treats it as unreachable,
// which also catches an empty mount point when the share is not mounted
// (for example a Docker install whose machine-side mount failed under
// "nofail").
var ErrNoModelFile = errors.New("no model file in the folder")

// ProbeRead is the body of the probe process: it reads the first 4 KiB of
// one model file with O_DIRECT, so the answer comes from the server and not
// from a cache. It prefers *.gguf files, in name order.
func ProbeRead(dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range ents {
		if e.Type().IsRegular() && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return ErrNoModelFile
	}
	sort.SliceStable(names, func(i, j int) bool {
		gi, gj := strings.HasSuffix(names[i], ".gguf"), strings.HasSuffix(names[j], ".gguf")
		if gi != gj {
			return gi
		}
		return names[i] < names[j]
	})
	p := filepath.Join(dir, names[0])
	f, err := osx.OpenDirect(p)
	if err != nil {
		// Some file systems (tmpfs) refuse O_DIRECT; a dir: source on one
		// of those is read normally.
		f, err = os.Open(p)
		if err != nil {
			return err
		}
	}
	defer f.Close()
	buf := alignedBuf(4096)
	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		return err
	}
	if n == 0 {
		st, _ := f.Stat()
		if st != nil && st.Size() > 0 {
			return fmt.Errorf("read 0 bytes of %s", p)
		}
	}
	return nil
}

// alignedBuf returns a buffer aligned to its size, as O_DIRECT needs.
func alignedBuf(size int) []byte {
	raw := make([]byte, 2*size)
	off := int(uintptr(unsafe.Pointer(&raw[0])) & uintptr(size-1))
	if off != 0 {
		off = size - off
	}
	return raw[off : off+size]
}

// ExecProber runs each probe as a separate short-lived process:
// "<Exe> probe-read <dir>".
type ExecProber struct {
	Exe string
}

type execProbe struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func (p *execProbe) Done() <-chan struct{} { return p.done }
func (p *execProbe) Err() error            { <-p.done; return p.err }
func (p *execProbe) Kill() {
	select {
	case <-p.done:
	default:
		p.cmd.Process.Kill() // a process in D state on a hard mount dies when the server answers
	}
}

// Start implements ProbeStarter.
func (e ExecProber) Start(dir string) (ProbeHandle, error) {
	cmd := exec.Command(e.Exe, ProbeCommand, dir)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &execProbe{cmd: cmd, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		if err != nil {
			msg := strings.TrimSpace(stderr.String())
			if msg != "" {
				err = fmt.Errorf("%w: %s", err, msg)
			}
		}
		p.err = err
		close(p.done)
	}()
	return p, nil
}

// ExecMounter mounts with the mount and umount commands.
type ExecMounter struct {
	Cmd       osx.Commander
	MountInfo string // default /proc/self/mountinfo
}

// IsMounted implements Mounter.
func (m ExecMounter) IsMounted(path string) (bool, error) { return osx.IsMountPoint(m.MountInfo, path) }

// MountSource implements Mounter.
func (m ExecMounter) MountSource(path string) (string, bool, error) {
	return osx.MountSource(m.MountInfo, path)
}

// Mount implements Mounter.
func (m ExecMounter) Mount(ctx context.Context, args []string) error {
	_, err := m.Cmd.Run(ctx, "mount", args...)
	return err
}

// Unmount implements Mounter (never lazy).
func (m ExecMounter) Unmount(ctx context.Context, path string) error {
	_, err := m.Cmd.Run(ctx, "umount", path)
	return err
}

// MkdirAll implements Mounter.
func (ExecMounter) MkdirAll(path string) error { return os.MkdirAll(path, 0o755) }
