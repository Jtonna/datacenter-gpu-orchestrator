package scheduler

import (
	"context"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/release"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// Trust in stored needs depends on two llama builds: the host program's and
// the one the served release's release.json names (needcount.go, trusted).
// When either cannot be read at start (a slow share right after a boot, a
// release.json briefly unreadable), they are read again in the background
// until both are known, and trust is decided then (fourth review, FS2).

const (
	buildReadFirst = 5 * time.Second
	buildReadMax   = 5 * time.Minute
)

// startBuildReads starts the background reads when a build is missing and
// can be read again. It runs before the event loop; the reading goroutine
// gets copies and touches no core state.
func (s *Scheduler) startBuildReads() {
	c := s.core
	if !c.persist {
		return
	}
	host, served := c.llamaBuild, c.servedBuild
	hostRead, manifest, rel := s.opts.HostBuild, c.releaseBuild, c.buildRelease
	canHost := host != "" || hostRead != nil
	canServed := served != "" || (manifest != nil && rel != "")
	if (host != "" && served != "") || !canHost || !canServed {
		return
	}
	// A failure restarts the reads alone: they hold only the copies taken
	// here and what they found so far.
	s.wg.Add(1)
	supervise.Go(s.bgCtx, supervise.Spec{Name: "scheduler.build_reads", Policy: supervise.Restart, Done: s.wg.Done}, func(ctx context.Context) {
		wait := buildReadFirst
		for host == "" || served == "" {
			if !sleepOn(ctx, s.opts.Clock, wait) {
				return
			}
			if host == "" {
				if b, err := hostRead(ctx); err == nil {
					host = b
				}
			}
			if served == "" {
				served = manifest(rel)
			}
			wait = min(wait*2, buildReadMax)
		}
		s.post(ctx, evBuilds{host: host, served: served})
	})
}

// sleepOn waits d on clk; false when ctx ended first.
func sleepOn(ctx context.Context, clk Clock, d time.Duration) bool {
	ch := make(chan struct{})
	stop := clk.AfterFunc(d, func() { close(ch) })
	select {
	case <-ch:
		return true
	case <-ctx.Done():
		stop()
		return false
	}
}

// buildsKnown takes the builds a background read found, decides trust
// again, stamps the rows measured before builds were recorded, gives the
// measurements of this run that have no build the host's, and logs once.
func (c *schedCore) buildsKnown(host, served string) {
	if c.llamaBuild == "" {
		c.llamaBuild = host
	}
	if c.servedBuild == "" {
		c.servedBuild = served
	}
	c.changed = true
	if !c.buildsAgree(c.llamaBuild) {
		c.log.Warn("llama builds known, but the host program's is not the served release's: stored memory needs only raise the estimates",
			"host_build", c.llamaBuild, "release", c.buildRelease, "release_build", c.servedBuild)
		return
	}
	for _, ls := range c.learned {
		for _, l := range ls {
			if !l.restored && l.build == "" {
				l.build = c.llamaBuild
				c.storeLearned(l)
			}
		}
	}
	c.stampBuilds()
	trusted := 0
	for _, ls := range c.learned {
		for _, l := range ls {
			if l.restored && c.trusted(l) {
				trusted++
			}
		}
	}
	c.log.Info("llama builds known: stored memory needs of this build are trusted", "host_build", c.llamaBuild,
		"release", c.buildRelease, "release_build", c.servedBuild, "trusted", trusted)
}

// untrustedWhy is why a stored measurement is not trusted, "" when it is.
func (c *schedCore) untrustedWhy(l *learnedNeed) string {
	switch {
	case c.trusted(l):
		return ""
	case c.llamaBuild == "":
		return "the host program's llama build is not known (yet): it is read again in the background"
	case c.servedBuild == "":
		return "the served release's llama build is not known (yet; programs/" + c.buildRelease + "/release.json): it is read again in the background"
	case !release.SameCommit(c.llamaBuild, c.servedBuild):
		return "the host program's llama build and the served release's differ"
	case l.build == "":
		return "measured before llama builds were recorded, and the release.json of " + releaseWords(l.release) + " does not name this build"
	}
	return "measured on llama build " + l.build + ", not the one that runs now"
}

func releaseWords(rel string) string {
	if rel == "" {
		return "its release (not recorded)"
	}
	return "release " + rel
}
