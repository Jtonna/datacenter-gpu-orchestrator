package scheduler

import (
	"math"
	"testing"
)

// gbMiB is a size in GB (10^9 bytes, as the measurements were written) in MiB.
func gbMiB(gb float64) int { return int(math.Round(gb * 1e9 / (1 << 20))) }

// The reload rule (design, "Reload from cache"). A console has 14 GB shared
// between the model and the file cache; the margin is the tier headroom plus
// the watchdog floor.
func TestReloadFromCacheRule(t *testing.T) {
	console := gbMiB(14)
	const margin = 1000 + 1000
	cases := []struct {
		name    string
		in      ReloadInput
		verdict string
		pct     float64 // expected cached share of the file, for partial
	}{
		// Measured: 7.0 GB file + 7.5 GB model on a 14 GB console. Loading the
		// model pushes part of its own file out; reload 19-23 s, not 2.5 s.
		{"measured: 7.0 GB file + 7.5 GB model on 14 GB", ReloadInput{FileRead: true, Shared: true,
			FileMiB: gbMiB(7.0), ModelMiB: gbMiB(7.5), MemoryMiB: console, MarginMiB: margin}, ReloadPartial, 62.9},
		// Measured: 2.7 GB file + 2.9 GB model: both fit, reload 2.5 s.
		{"measured: 2.7 GB file + 2.9 GB model on 14 GB", ReloadInput{FileRead: true, Shared: true,
			FileMiB: gbMiB(2.7), ModelMiB: gbMiB(2.9), MemoryMiB: console, MarginMiB: margin}, ReloadFull, 0},
		{"exactly fits", ReloadInput{FileRead: true, Shared: true, FileMiB: 4000, ModelMiB: 6000, MemoryMiB: 12000, MarginMiB: 2000}, ReloadFull, 0},
		{"one MiB over", ReloadInput{FileRead: true, Shared: true, FileMiB: 4001, ModelMiB: 6000, MemoryMiB: 12000, MarginMiB: 2000}, ReloadPartial, 99.98},
		{"the model leaves no room", ReloadInput{FileRead: true, Shared: true, FileMiB: 4000, ModelMiB: 10000, MemoryMiB: 12000, MarginMiB: 2000}, ReloadNone, 0},
		{"the model alone does not fit", ReloadInput{FileRead: true, Shared: true, FileMiB: 4000, ModelMiB: 11000, MemoryMiB: 12000, MarginMiB: 2000}, ReloadNone, 0},
		// Dedicated GPU memory: the model sits in VRAM, the file cache in
		// system RAM, so only the file share has to fit in system RAM.
		{"dedicated: file fits in system RAM", ReloadInput{FileRead: true, FileMiB: gbMiB(7.0), ModelMiB: gbMiB(7.5), MemoryMiB: gbMiB(16), MarginMiB: 1000}, ReloadFull, 0},
		{"dedicated: file larger than system RAM", ReloadInput{FileRead: true, FileMiB: 20000, ModelMiB: 20000, MemoryMiB: 16000, MarginMiB: 0}, ReloadPartial, 80},
		{"dedicated: system RAM unknown", ReloadInput{FileRead: true, FileMiB: 4000, ModelMiB: 4000}, ReloadUnknown, 0},
		{"pushed: the workers do not read the file", ReloadInput{Shared: true, FileMiB: 2000, ModelMiB: 2000, MemoryMiB: 14000, MarginMiB: 2000}, ReloadUnknown, 0},
		{"file share unknown", ReloadInput{FileRead: true, Shared: true, ModelMiB: 2000, MemoryMiB: 14000}, ReloadUnknown, 0},
		{"model share unknown", ReloadInput{FileRead: true, Shared: true, FileMiB: 2000, MemoryMiB: 14000}, ReloadUnknown, 0},
		{"memory unknown", ReloadInput{FileRead: true, Shared: true, FileMiB: 2000, ModelMiB: 2000}, ReloadUnknown, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, frac := ReloadFromCache(tc.in)
			if v != tc.verdict {
				t.Fatalf("verdict %s, want %s", v, tc.verdict)
			}
			if v != ReloadPartial {
				if frac != 0 && v != ReloadFull {
					t.Fatalf("fraction %v for %s", frac, v)
				}
				return
			}
			if got := math.Round(frac*10000) / 100; math.Abs(got-tc.pct) > 0.5 {
				t.Fatalf("cached %.2f%%, want about %.2f%%", got, tc.pct)
			}
		})
	}
}
