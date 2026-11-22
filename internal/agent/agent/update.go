package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// ErrUpdateNotRepeated is returned by SelfUpdater.Update for a release and
// sha256 it has already installed or found wrong: the same binary is not
// downloaded and started again.
var ErrUpdateNotRepeated = errors.New("this agent binary was tried before and is not tried again")

// SelfUpdater replaces the agent binary and restarts it (bare metal and
// RAM-only, 5.4): download the agent of GET /api/pool/release for this
// OS and architecture, check its sha256 and the release it reports, rename
// it over the running binary, then call Restart (exec of the new binary with
// the same arguments).
//
// One release and sha256 is tried once. The attempt is written to
// AttemptPath before the restart, so the restarted agent (which is refused
// again when the binary is stamped with another release than its name says)
// does not download and exec the same binary every 60 s.
type SelfUpdater struct {
	Base    string // http://<potluck>
	HTTP    *http.Client
	Exe     string // path of the running binary
	Restart func(exe string) error
	// Version asks a binary for its release ("<exe> version"). Nil skips the
	// check.
	Version func(ctx context.Context, exe string) (string, error)
	// Busy reports a launch that runs or stops; the binary is never replaced
	// then (5.4: an agent waits for the launch to end).
	Busy func() bool
	// AttemptPath holds "<release> <sha256>" of the last attempt; default
	// Exe + ".update-attempt".
	AttemptPath string
	Logger      *slog.Logger
}

// ExecVersion runs "<exe> version".
func ExecVersion(ctx context.Context, exe string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, exe, "version").Output()
	return strings.TrimSpace(string(out)), err
}

func (u SelfUpdater) attemptPath() string {
	if u.AttemptPath != "" {
		return u.AttemptPath
	}
	return u.Exe + ".update-attempt"
}

// Update implements Updater.
func (u SelfUpdater) Update(ctx context.Context, required string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.Base+"/api/pool/release", nil)
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("release answer: %w", err)
	}
	var info struct {
		Release string `json:"release"`
		Agents  map[string]struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
		} `json:"agents"`
	}
	err = json.NewDecoder(resp.Body).Decode(&info)
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("release answer: %w", err)
	}
	if required != "" && info.Release != required {
		return fmt.Errorf("Potluck serves %s, registration asked for %s", info.Release, required)
	}
	bin, ok := info.Agents[runtime.GOOS+"-"+runtime.GOARCH]
	if !ok || !strings.HasPrefix(bin.Path, "/") {
		return fmt.Errorf("no agent for %s-%s in release %s", runtime.GOOS, runtime.GOARCH, info.Release)
	}
	attempt := info.Release + " " + strings.ToLower(bin.SHA256)
	if b, err := os.ReadFile(u.attemptPath()); err == nil && strings.TrimSpace(string(b)) == attempt {
		return fmt.Errorf("%w: release %s, sha256 %s", ErrUpdateNotRepeated, info.Release, bin.SHA256)
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, u.Base+bin.Path, nil)
	resp, err = u.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	part := u.Exe + ".part"
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(part)
		return fmt.Errorf("download: %w", err)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(sum, bin.SHA256) {
		os.Remove(part)
		return fmt.Errorf("agent sha256 mismatch: got %s, want %s", sum, bin.SHA256)
	}
	// From here the binary is the one Potluck serves for this release: what
	// happens with it is remembered, whatever it is.
	if err := os.WriteFile(u.attemptPath(), []byte(attempt+"\n"), 0o644); err != nil {
		os.Remove(part)
		return fmt.Errorf("cannot record the update attempt, so no update: %w", err)
	}
	if u.Version != nil {
		got, err := u.Version(ctx, part)
		if err != nil || got != info.Release {
			os.Remove(part)
			return fmt.Errorf("the agent binary of release %s reports release %q (%v); not installed", info.Release, got, err)
		}
	}
	if u.Busy != nil && u.Busy() {
		os.Remove(part)
		os.Remove(u.attemptPath()) // nothing was tried; update after the launch
		return errors.New("a launch exists; the update waits for it to end")
	}
	if err := os.Rename(part, u.Exe); err != nil {
		os.Remove(part)
		os.Remove(u.attemptPath())
		return fmt.Errorf("install: %w", err)
	}
	if u.Logger != nil {
		u.Logger.Info("agent binary replaced; restarting", "release", info.Release, "sha256", bin.SHA256)
	}
	if u.Restart == nil {
		return nil
	}
	return u.Restart(u.Exe)
}

// DockerUpdater exits with code 75: a container cannot durably replace its
// own binary; the setup script or a pull of the new tag replaces the image.
type DockerUpdater struct {
	Exit func(code int)
}

// Update implements Updater.
func (d DockerUpdater) Update(ctx context.Context, required string) error {
	d.Exit(75)
	return nil
}
