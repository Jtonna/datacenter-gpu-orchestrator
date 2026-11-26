package bench

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

// F17: with no run limit given, a run may take 720 minutes (a Full pass is
// 6 to 7 hours); a given limit is kept.
func TestMaxRunDefaultIs720Minutes(t *testing.T) {
	store, _ := openTemp(t)
	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := NewManager(ManagerOptions{Store: store, API: &fakeAPI{}, Logger: lg})
	if err != nil {
		t.Fatal(err)
	}
	if m.o.MaxRun != 720*time.Minute || DefaultMaxRun != 720*time.Minute {
		t.Fatalf("default %s", m.o.MaxRun)
	}
	m, err = NewManager(ManagerOptions{Store: store, API: &fakeAPI{}, Logger: lg, MaxRun: 240 * time.Minute})
	if err != nil || m.o.MaxRun != 240*time.Minute {
		t.Fatalf("given 240: %v %s", err, m.o.MaxRun)
	}
}
