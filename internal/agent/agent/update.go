package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
)

// SelfUpdater replaces the agent binary and restarts it (bare metal and
// RAM-only, 5.4): download the agent of GET /api/agents/release for this
// OS and architecture, check its sha256, rename it over the running binary,
// then call Restart (exec of the new binary with the same arguments).
type SelfUpdater struct {
	Base    string // http://<potluck>
	HTTP    *http.Client
	Exe     string // path of the running binary
	Restart func(exe string) error
}

// Update implements Updater.
func (u SelfUpdater) Update(ctx context.Context, required string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.Base+"/api/agents/release", nil)
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return err
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
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, u.Base+bin.Path, nil)
	resp, err = u.HTTP.Do(req)
	if err != nil {
		return err
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
		return err
	}
	if sum := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(sum, bin.SHA256) {
		os.Remove(part)
		return fmt.Errorf("agent sha256 mismatch: got %s, want %s", sum, bin.SHA256)
	}
	if err := os.Rename(part, u.Exe); err != nil {
		os.Remove(part)
		return err
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
