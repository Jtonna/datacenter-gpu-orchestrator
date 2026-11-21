package logrelay

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The store's retention and disk cap are start flags of the store
// (-retentionPeriod, -retention.maxDiskSpaceUsageBytes), set in the compose
// file. Potluck cannot change them while the store runs: the settings
// logs_retention_days and logs_max_disk_gib only say what they should be.
// Potluck reads what the store runs with from its /flags page and shows a
// warning in /api/status logs, and in its own log, when they differ.
//
// /flags lists the flags that were set on the command line, one per line as
// -name="value" (VictoriaLogs v1.54.0, lib/flagutil WriteFlags, which uses
// flag.Visit). A flag that is not listed has its default: retention 7d
// (app/vlstorage/main.go) and no disk cap.

// StoreSettings is what the store runs with, as read from its /flags page.
type StoreSettings struct {
	// RetentionDays and MaxDiskGiB: nil when the value could not be read;
	// MaxDiskGiB 0 means no disk cap.
	RetentionDays *float64 `json:"retention_days"`
	MaxDiskGiB    *float64 `json:"max_disk_gib"`
	ReadError     string   `json:"read_error,omitempty"`
	ReadAt        string   `json:"read_at,omitempty"` // RFC 3339, Potluck's clock
}

type storeFlags struct {
	cur    atomic.Pointer[StoreSettings]
	mu     sync.Mutex
	logged string // the last warning logged, so it is logged once per change
}

// flagRefresh is how often the store's flags are read again; a failed read is
// tried again every flagCheck.
const (
	flagRefresh = 10 * time.Minute
	flagCheck   = time.Minute
)

// WatchStore reads the store's flags now and then and logs a warning when
// they differ from the settings, until ctx ends. Without a store it returns
// at once. It never blocks anything else: it is its own goroutine with its own
// short requests.
func (r *Relay) WatchStore(ctx context.Context) {
	if r.base == "" {
		return
	}
	var last time.Time
	t := time.NewTicker(flagCheck)
	defer t.Stop()
	for {
		cur := r.flags.cur.Load()
		if last.IsZero() || cur == nil || cur.ReadError != "" || time.Since(last) >= flagRefresh {
			r.ReadStoreFlags(ctx)
			last = time.Now()
		}
		r.warnSettings()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ReadStoreFlags reads the store's /flags page once and keeps the result.
func (r *Relay) ReadStoreFlags(ctx context.Context) StoreSettings {
	st := StoreSettings{ReadAt: r.o.Now().UTC().Format(time.RFC3339)}
	text, err := r.fetchFlags(ctx)
	if err != nil {
		st.ReadError = err.Error()
	} else {
		st.RetentionDays, st.MaxDiskGiB = parseStoreFlags(text)
	}
	r.flags.cur.Store(&st)
	return st
}

func (r *Relay) fetchFlags(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.base+"/flags", nil)
	if err != nil {
		return "", err
	}
	resp, err := r.read.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("the log store's /flags did not answer: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the log store's /flags answered %d", resp.StatusCode)
	}
	return string(b), nil
}

// parseStoreFlags reads -retentionPeriod and -retention.maxDiskSpaceUsageBytes
// from a /flags page. An absent flag has the store's default.
func parseStoreFlags(text string) (retentionDays, maxDiskGiB *float64) {
	vals := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		name, v, ok := strings.Cut(strings.TrimPrefix(line, "-"), "=")
		if !ok {
			continue
		}
		if u, err := strconv.Unquote(v); err == nil {
			v = u
		}
		vals[name] = v
	}
	f := func(v float64) *float64 { return &v }
	if v, ok := vals["retentionPeriod"]; !ok {
		retentionDays = f(7)
	} else if d, ok := parseRetention(v); ok {
		retentionDays = f(d)
	}
	if v, ok := vals["retention.maxDiskSpaceUsageBytes"]; !ok {
		maxDiskGiB = f(0)
	} else if n, ok := parseBytes(v); ok {
		maxDiskGiB = f(n / (1 << 30))
	}
	return
}

// parseRetention reads a -retentionPeriod value in days: a number of months
// (bare, or with M; a month is 31 days, as the store counts it), or numbers
// with s, h, d, w or y (365 days), for example 14d or 1w2d.
func parseRetention(v string) (float64, bool) {
	v = strings.TrimSpace(v)
	if m, ok := strings.CutSuffix(v, "M"); ok {
		v = m
	}
	if n, err := strconv.ParseFloat(v, 64); err == nil {
		return n * 31, true
	}
	days := map[byte]float64{'s': 1.0 / 86400, 'h': 1.0 / 24, 'd': 1, 'w': 7, 'y': 365}
	total, num := 0.0, ""
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= '0' && c <= '9' || c == '.':
			num += string(c)
		default:
			k, ok := days[c]
			n, err := strconv.ParseFloat(num, 64)
			if !ok || err != nil {
				return 0, false
			}
			total += n * k
			num = ""
		}
	}
	if num != "" || total <= 0 {
		return 0, false
	}
	return total, true
}

// parseBytes reads a byte count as the store does (KB, MB, GB, TB powers of
// 1000; KiB, MiB, GiB, TiB powers of 1024; else plain bytes).
func parseBytes(v string) (float64, bool) {
	v = strings.ReplaceAll(strings.ToUpper(strings.TrimSpace(v)), "I", "i")
	mult := 1.0
	for _, u := range []struct {
		s string
		m float64
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40}, {"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"TB", 1e12}} {
		if s, ok := strings.CutSuffix(v, u.s); ok {
			v, mult = s, u.m
			break
		}
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n * mult, true
}

// settingsWarnings compares the settings with what the store runs with.
func (r *Relay) settingsWarnings() (*StoreSettings, []string) {
	st := r.flags.cur.Load()
	if st == nil || r.base == "" {
		return st, nil
	}
	c := r.o.Config()
	var out []string
	if st.ReadError != "" {
		out = append(out, "the log store's retention and disk cap could not be read ("+st.ReadError+"); they are what the compose file sets")
		return st, out
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 0.01 }
	if st.RetentionDays != nil && !near(*st.RetentionDays, float64(c.RetentionDays)) {
		out = append(out, fmt.Sprintf("logs_retention_days is %d, but the log store keeps lines %s days: the store's start flag -retentionPeriod applies; change it in the compose file and restart the store", c.RetentionDays, trimFloat(*st.RetentionDays)))
	}
	if st.MaxDiskGiB != nil && !near(*st.MaxDiskGiB, float64(c.MaxDiskGiB)) {
		have := trimFloat(*st.MaxDiskGiB) + " GiB"
		if *st.MaxDiskGiB == 0 {
			have = "no cap"
		}
		out = append(out, fmt.Sprintf("logs_max_disk_gib is %d, but the log store's disk cap is %s: the store's start flag -retention.maxDiskSpaceUsageBytes applies; change it in the compose file and restart the store", c.MaxDiskGiB, have))
	}
	return st, out
}

func trimFloat(f float64) string { return strconv.FormatFloat(math.Round(f*100)/100, 'f', -1, 64) }

// warnSettings logs the warnings once each time they change.
func (r *Relay) warnSettings() {
	_, ws := r.settingsWarnings()
	key := strings.Join(ws, "\n")
	r.flags.mu.Lock()
	same := key == r.flags.logged
	r.flags.logged = key
	r.flags.mu.Unlock()
	if same {
		return
	}
	for _, w := range ws {
		r.log.Warn("central logging: " + w)
	}
}
