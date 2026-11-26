package e2e

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/harness"
)

// TestPageOpenCostAcrossARestart opens pages in a real headless Chrome
// against the real scheduler (fleetCluster: seven agents, 315 profiles)
// while another tab stands on the Models page, as the owner's does, and
// reads the scheduler's loop steps per open (one round of every plan is
// 315). With RESTART=1 the scheduler restarts first, after its catalog
// moved on, and the standing tab reconnects with the page it loaded before;
// RESTART_BIN starts another binary then (an image switch: the tab keeps the
// old image's page). Opt-in (minutes):
//
//	POTLUCK_MEASURE=1 MEASURE_CHROME=<chrome> [RESTART=1] [RESTART_BIN=<llm-scheduler>] go test ./internal/e2e -run TestPageOpenCostAcrossARestart -v -timeout 9m
//
// Measured, v84 both sides, after a restart: Benchmarks 687 and 685 steps,
// Models 1,003, a plain GET /api/models 651 (the standing tab read the whole
// catalog again after every change: its catalog version was the old
// server's, higher than the new one's). This branch: 336 to 346 for every
// page, 326 for GET /api/models, also with the v84 page in the tab.
func TestPageOpenCostAcrossARestart(t *testing.T) {
	chrome := os.Getenv("MEASURE_CHROME")
	if os.Getenv("POTLUCK_MEASURE") == "" || chrome == "" {
		t.Skip("opt-in: POTLUCK_MEASURE=1 MEASURE_CHROME=<chrome>")
	}
	c, steps := fleetCluster(t)
	time.Sleep(10 * time.Second)
	tab := func(route string) *exec.Cmd {
		cmd := exec.Command(chrome, "--headless=new", "--disable-gpu", "--no-first-run", "--user-data-dir="+t.TempDir(),
			"--remote-debugging-port=0", "--window-size=1536,864", c.url+"/ui/#/"+route)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	standing := tab("models")
	t.Cleanup(func() { standing.Process.Kill(); standing.Wait() })
	time.Sleep(20 * time.Second)
	if os.Getenv("RESTART") != "" {
		for i := 0; i < 15; i++ { // the catalog moves on, as a long-running server's has
			_, _, h := c.do("GET", "/api/models", "")
			if i == 14 {
				t.Logf("before the restart: catalog version %s", h.Get("X-Catalog-Version"))
			}
			time.Sleep(time.Second)
		}
		killTree(c.cmd)
		<-c.done
		bin := c.cmd.Path
		if b := os.Getenv("RESTART_BIN"); b != "" {
			bin = b
		}
		cmd := exec.Command(bin, c.cmd.Args[1:]...)
		cmd.Env, cmd.Stderr = c.cmd.Env, c.stderr
		prepareCmd(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		c.cmd, c.done = cmd, done
		go func() { c.exitErr = cmd.Wait(); close(done) }()
		harness.WaitForOK(t, c.url+"/health", 15*time.Second)
		time.Sleep(25 * time.Second) // the agents and the standing tab come back
	}
	measure := func(name string, f func()) {
		time.Sleep(2 * time.Second)
		s0 := steps()
		f()
		time.Sleep(1500 * time.Millisecond)
		n := steps() - s0
		t.Logf("OPEN %-16s %5.0f loop steps (%.1f rounds of %d plans)", name, n, n/fleetProfiles, fleetProfiles)
	}
	for _, route := range []string{"benchmarks", "benchmarks", "models", "overview", "benchmarks"} {
		measure(route, func() {
			p := tab(route)
			time.Sleep(4 * time.Second)
			p.Process.Kill()
			p.Wait()
		})
	}
	measure("GET /api/models", func() { c.get("/api/models") })
}
