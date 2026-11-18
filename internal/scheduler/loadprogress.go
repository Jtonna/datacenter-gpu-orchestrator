package scheduler

import (
	"errors"
	"math"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/instance"
)

// Load progress. A child that loads a model onto remote workers prints
// nothing while it uploads, so the scheduler measures it from outside when
// it can (the bytes the workers have acknowledged, read by the child's
// ByteCounter) and otherwise estimates it from the tier link speed.

const (
	sampleEvery  = time.Second      // least gap between two byte counts of one instance
	loadLogEvery = 30 * time.Second // "load progress" log line period
	sampleWindow = 20 * time.Second // measured rate is taken over this much history
	minRateSpan  = 2 * time.Second  // least history for a measured rate
	maxProgress  = 0.99             // progress is 1 only when the instance is ready
)

type byteSample struct {
	at    time.Time
	bytes int64
}

// initLoadProgress records what must be uploaded and to whom, when the
// child is about to start (the same shares the load timeout uses).
func (c *schedCore) initLoadProgress(in *inst) {
	in.remoteBytes, in.linkMBs, in.peers, in.workerEst = 0, 0, nil, nil
	p := in.prof
	if p.Kind == core.KindCommand {
		return // runs on the local node: no upload
	}
	info, err := c.need(p)
	if err != nil {
		return
	}
	shares := instance.RemoteShares(*p, in.placement, func(id core.NodeID) float64 {
		n, _ := c.node(id)
		return n.LinkMBs
	})
	var frac float64
	for _, s := range shares {
		frac += s.Fraction
		if s.LinkMBs > 0 && (in.linkMBs == 0 || s.LinkMBs < in.linkMBs) {
			in.linkMBs = s.LinkMBs
		}
	}
	in.remoteBytes = int64(math.Round(float64(info.FileBytes) * frac))
	fracs := instance.SplitFractions(*p, in.placement)
	for i, n := range in.placement.Nodes {
		if !n.Local && n.Addr != "" {
			in.peers = append(in.peers, n.Addr)
			in.workerEst = append(in.workerEst, int64(math.Round(float64(info.FileBytes)*fracs[i])))
		}
	}
	if len(in.peers) == 0 {
		in.remoteBytes = 0 // no remote worker, no upload
	}
}

// wantsSample: the instance is loading an upload that its child can be asked about.
func (c *schedCore) wantsSample(in *inst) bool {
	if in.remoteBytes <= 0 || in.noSample || in.child == nil {
		return false
	}
	_, ok := in.child.(instance.ByteCounter)
	return ok
}

// loadTimers runs the time-based rules of a loading instance: ask for a
// byte count (at most one at a time, at most once per sampleEvery) and log
// the progress line.
func (c *schedCore) loadTimers(in *inst) {
	if in.state != stStarting || in.phase != phLoading {
		return
	}
	if c.wantsSample(in) && !in.sampling && !c.now.Before(in.nextSample) {
		in.sampling = true
		in.nextSample = c.now.Add(sampleEvery)
		c.effects = append(c.effects, effSample{iid: in.iid, counter: in.child.(instance.ByteCounter), peers: in.peers})
	}
	if !c.now.Before(in.nextLoadLog) {
		in.nextLoadLog = c.now.Add(loadLogEvery)
		v := c.loadView(in)
		attrs := []any{"profile", in.prof.Name, "elapsed", c.now.Sub(in.created).Round(time.Second)}
		if v.total != nil {
			attrs = append(attrs, "bytes_total", *v.total)
		}
		if v.sent != nil {
			attrs = append(attrs, "bytes_sent", *v.sent)
		}
		if v.progress != nil {
			attrs = append(attrs, "percent", math.Round(*v.progress*1000)/10, "source", *v.source)
		}
		if v.eta != nil {
			attrs = append(attrs, "eta", (time.Duration(*v.eta * float64(time.Second))).Round(time.Second))
		}
		c.log.Info("load progress", attrs...)
	}
}

// loadSampled takes the result of a byte count. Any error, or no connection
// to a worker yet, puts the instance back on the estimate.
func (c *schedCore) loadSampled(iid uint64, bytes int64, sockets int, peers map[string]int64, err error) {
	in := c.instByIID(iid)
	if in == nil || in.state != stStarting || in.phase != phLoading {
		return
	}
	in.sampling = false
	switch {
	case errors.Is(err, instance.ErrSentBytesUnsupported):
		in.noSample, in.measured = true, false
	case err != nil:
		in.measured = false
		c.log.Debug("counting upload bytes failed; estimating", "profile", in.prof.Name, "err", err)
	case sockets == 0:
		in.measured = false
	default:
		in.measured = true
		if bytes > in.sentBytes {
			in.sentBytes = bytes // never backwards: a closed socket takes its count with it
		}
		in.peerKnown = peers != nil
		for addr, b := range peers {
			if b > in.peerSent[addr] {
				in.peerSent[addr] = b // never backwards, per worker too
			}
		}
		in.samples = append(in.samples, byteSample{at: c.now, bytes: in.sentBytes})
		for len(in.samples) > 2 && c.now.Sub(in.samples[0].at) > sampleWindow {
			in.samples = in.samples[1:]
		}
	}
}

// loadsUploading counts the loads whose uploads share the host's link now.
func (c *schedCore) loadsUploading() int {
	n := 0
	for _, in := range c.insts {
		if in.state == stStarting && in.phase == phLoading && in.remoteBytes > 0 {
			n++
		}
	}
	return n
}

// loadViewData is the load progress of one starting instance; nil pointers
// are not known or not applicable.
type loadViewData struct {
	elapsed  float64
	total    *int64
	sent     *int64
	progress *float64
	eta      *float64
	source   *string
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

// loadView computes what the status shows for a starting instance.
func (c *schedCore) loadView(in *inst) loadViewData {
	v := loadViewData{elapsed: round(c.now.Sub(in.created).Seconds(), 1)}
	if in.remoteBytes <= 0 {
		return v
	}
	total := in.remoteBytes
	v.total = &total
	estRate := 0.0 // bytes per second
	if in.linkMBs > 0 {
		estRate = in.linkMBs * 1e6 / float64(max(c.loadsUploading(), 1))
	}
	var sent float64
	var rate float64
	switch {
	case in.measured:
		src := "measured"
		v.source = &src
		s := in.sentBytes
		v.sent = &s
		sent, rate = float64(s), c.measuredRate(in, estRate)
	case estRate > 0:
		src := "estimated"
		v.source = &src
		if in.phase == phLoading {
			sent = c.now.Sub(in.loadStart).Seconds() * estRate
		}
		rate = estRate
	default:
		return v // no measurement and no link speed to estimate from
	}
	p := math.Min(sent/float64(total), maxProgress)
	p = round(p, 4)
	v.progress = &p
	if rate > 0 {
		eta := round(math.Max(float64(total)-sent, 0)/rate, 0)
		v.eta = &eta
	}
	return v
}

// measuredRate is bytes per second over the recent samples, or fallback
// when there is too little history or no movement.
func (c *schedCore) measuredRate(in *inst, fallback float64) float64 {
	if n := len(in.samples); n >= 2 {
		a, b := in.samples[0], in.samples[n-1]
		if span := b.at.Sub(a.at); span >= minRateSpan && b.bytes > a.bytes {
			return float64(b.bytes-a.bytes) / span.Seconds()
		}
	}
	return fallback
}
