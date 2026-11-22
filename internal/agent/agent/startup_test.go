package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/startup"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/agenttest"
)

type fakeSystemd struct {
	f     startup.Facts
	err   error
	calls atomic.Int32 // the agent's goroutine counts, the test reads
}

func (s *fakeSystemd) Show(context.Context) (startup.Facts, error) { s.calls.Add(1); return s.f, s.err }

// The agent reports its start-up: the registration carries the report with
// systemd's figures, the heartbeats carry it with every mark until Potluck
// has acknowledged them all, and the marks file is passed through.
func TestStartupReport(t *testing.T) {
	up, soft := 2.5, int64(0)
	sd := &fakeSystemd{f: startup.Facts{UserspaceStartS: &up, SoftReboots: &soft}}
	marks := filepath.Join(t.TempDir(), "startup-marks")
	if err := os.WriteFile(marks, []byte("boot_progress_start 1.25\nnetwork_ok 9.5\nsome_new_mark 12\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := newTenvWith(t, func(o *Options, d *Deps) {
		d.Systemd, d.MarksEvery = sd, 10*time.Millisecond
		d.BootID = func() string { return "0123456789abcdef0123456789abcdef" }
		o.MarksFile = marks
	})
	e.pl.set(func() {
		e.pl.ackStartup = true
		answer := e.pl.answer
		e.pl.answer = func(req agentproto.RegisterRequest) agentproto.RegisterAnswer {
			a := answer(req)
			a.StartupAck = req.Startup.Count()
			return a
		}
	})
	e.start()
	e.waitReady()
	reg := e.pl.lastRegister()
	st := reg.Startup
	if st == nil || st.BootID != "0123456789abcdef0123456789abcdef" || st.AgentStartS != 3600 || st.UserspaceStartS == nil ||
		*st.UserspaceStartS != 2.5 || st.SoftReboots == nil || *st.SoftReboots != 0 || st.Marks["agent_start"] != 3600 {
		t.Fatalf("registration report %+v", st)
	}
	if _, ok := st.Marks["registered"]; ok {
		t.Fatal("the first registration already says registered")
	}
	if n := sd.calls.Load(); n != 1 {
		t.Fatalf("systemd asked %d times", n)
	}
	// Every mark reaches Potluck in a heartbeat.
	var all *agentproto.Startup
	agenttest.Eventually(t, 20*time.Second, "every mark in a heartbeat", func() bool {
		e.pl.mu.Lock()
		defer e.pl.mu.Unlock()
		for _, hb := range e.pl.beats {
			s := hb.Startup
			if s == nil {
				continue
			}
			ok := len(s.ExternalMarks) == 3
			for _, m := range []string{"agent_start", "registered", "programs_ready", "share_mounted", "exporter_up", "first_stats"} {
				_, has := s.Marks[m]
				ok = ok && has
			}
			if ok {
				all = s
				return true
			}
		}
		return false
	})
	// On the agent's clock, which the test does not move: all at 3600 s.
	for name, v := range all.Marks {
		if v != 3600 {
			t.Errorf("mark %s at %v, want 3600 (the agent's clock)", name, v)
		}
	}
	if all.ExternalMarks["network_ok"] != 9.5 || all.ExternalMarks["some_new_mark"] != 12 {
		t.Fatalf("external marks %v", all.ExternalMarks)
	}
	// Once Potluck has them all, the heartbeats no longer carry the report.
	agenttest.Eventually(t, 10*time.Second, "heartbeats without the report", func() bool {
		n := e.pl.nBeats()
		return n > 2 && e.pl.lastBeat().Startup == nil
	})
	// A new mark in the file is sent again.
	if err := os.WriteFile(marks+".tmp", []byte("boot_progress_start 1.25\nnetwork_ok 9.5\nsome_new_mark 12\ndashboard_up 80\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(marks+".tmp", marks); err != nil {
		t.Fatal(err)
	}
	agenttest.Eventually(t, 10*time.Second, "the new external mark", func() bool {
		s := e.pl.lastBeat().Startup
		return s != nil && s.ExternalMarks["dashboard_up"] == 80
	})
	// The file was only read.
	if _, err := os.Stat(marks); err != nil {
		t.Fatal(err)
	}
}

// A Potluck that does not take start-up reports (it answers without
// startup_ack) gets the report in registrations only; systemd that cannot be
// asked leaves its fields out.
func TestStartupReportToAnOlderPotluck(t *testing.T) {
	sd := &fakeSystemd{err: errors.New("no systemd")}
	e := newTenvWith(t, func(o *Options, d *Deps) { d.Systemd = sd })
	e.start()
	e.waitReady()
	st := e.pl.lastRegister().Startup
	if st == nil || st.UserspaceStartS != nil || st.SoftReboots != nil {
		t.Fatalf("%+v", st)
	}
	n := e.pl.nBeats()
	agenttest.Eventually(t, 10*time.Second, "more heartbeats", func() bool { return e.pl.nBeats() > n+3 })
	e.pl.mu.Lock()
	defer e.pl.mu.Unlock()
	for i, hb := range e.pl.beats {
		if hb.Startup != nil {
			t.Fatalf("heartbeat %d carries the report to a Potluck that takes none", i)
		}
	}
}
