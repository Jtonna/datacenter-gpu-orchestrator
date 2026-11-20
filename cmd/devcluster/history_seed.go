package main

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
)

// seedHistory writes about five hours of past events into the REAL history
// store (Store.Record keeps a caller-set Ts). The data keys are those of
// design 9.1; summaries copy the scheduler's wording.
func seedHistory(h *history.Store, now time.Time) {
	for _, e := range pastEvents(now) {
		h.Record(e)
	}
}

type evGen struct {
	out []history.Event
	rnd *rand.Rand
}

func (g *evGen) add(t time.Time, typ, sev, profile string, nodes []string, session, summary string, data map[string]any) {
	if nodes == nil {
		nodes = []string{}
	}
	if data == nil {
		data = map[string]any{}
	}
	g.out = append(g.out, history.Event{Ts: t.UnixMilli(), Type: typ, Severity: sev, Profile: profile, ModelFile: modelFileOf(profile),
		Nodes: nodes, SessionID: session, Summary: summary, Data: data})
}

func joinNames(ns []string) string {
	switch len(ns) {
	case 0:
		return "no node"
	case 1:
		return ns[0]
	}
	return strings.Join(ns[:len(ns)-1], ", ") + " and " + ns[len(ns)-1]
}

func addrsOf(hosts []string) []string {
	var out []string
	for _, h := range hosts {
		if n, _ := nodeByHost(h); !n.Local {
			out = append(out, n.Addr())
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

func (g *evGen) memSample(t time.Time, host, profile, why string, used int) {
	n, ok := nodeByHost(host)
	if !ok || n.Local {
		return
	}
	avail := n.TotalMiB - used
	g.add(t, history.TNodeMemory, history.Info, profile, []string{host}, "",
		fmt.Sprintf("Memory on %s: %d MiB available of %d MiB (%s).", host, avail, n.TotalMiB, strings.ReplaceAll(why, "_", " ")),
		map[string]any{"reason": why, "available_mib": avail, "total_mib": n.TotalMiB, "used_mib": used, "budget_mib": max(avail-n.Headroom, 0)})
}

// load records a whole load session; it returns the ready time.
func (g *evGen) load(t time.Time, profile string, hosts []string, n int, learned bool) (time.Time, string) {
	p, _ := profileByID(profile)
	sess := sessionID(t, n)
	remote := addrsOf(hosts)
	var bytesTotal int64
	if len(remote) > 0 {
		bytesTotal = modelBytes[p.Model]
	}
	g.add(t, history.TLoadStarted, history.Info, profile, hosts, sess, fmt.Sprintf("Loading %s onto %s.", profile, joinNames(hosts)),
		map[string]any{"workers": remote, "bytes_total": bytesTotal, "need_mib": p.NeedsMiB, "need_basis": "measured, needs_mib"})
	for _, h := range hosts {
		g.memSample(t.Add(20*time.Millisecond), h, profile, "load_start", 0)
	}
	rate := (95 + g.rnd.Float64()*12) * float64(max(len(remote), 1))
	loadS := 4.0 + g.rnd.Float64()*3
	if bytesTotal > 0 {
		loadS = round1(float64(bytesTotal)/1e6/rate + 2 + g.rnd.Float64()*2)
	}
	ready := t.Add(time.Duration(loadS * float64(time.Second)))
	total := measuredTotal(profile, len(hosts))
	fr := fractions(p, len(hosts))
	var detail []map[string]any
	for i, h := range hosts {
		d := map[string]any{"node": h, "memory_mib": round1(total * fr[i])}
		if nd, _ := nodeByHost(h); !nd.Local {
			d["bytes"] = int64(float64(bytesTotal) * fr[i])
		}
		detail = append(detail, d)
	}
	data := map[string]any{"outcome": "ready", "reason": "", "load_s": loadS, "bytes": bytesTotal, "bytes_measured": bytesTotal > 0, "nodes_detail": detail}
	sum := fmt.Sprintf("%s finished loading in %.1f s", profile, loadS)
	if bytesTotal > 0 {
		mbs := round1(float64(bytesTotal) / 1e6 / loadS)
		data["avg_mb_s"] = mbs
		sum += fmt.Sprintf(" (%.1f MB/s)", mbs)
	}
	g.add(ready, history.TLoadFinished, history.Info, profile, hosts, sess, sum+".", data)
	if learned {
		var per []map[string]any
		for i, h := range hosts {
			per = append(per, map[string]any{"node": h, "fraction": math.Round(fr[i]*1000) / 1000, "mib": int(math.Ceil(total * fr[i]))})
		}
		g.add(ready.Add(5*time.Millisecond), history.TLearnedNeed, history.Info, profile, hosts, sess,
			fmt.Sprintf("Measured the memory need of %s at %d MiB; fit decisions use it for the rest of this run.", profile, int(total)),
			map[string]any{"measured_mib": int(total), "highest_mib": int(total), "per_node": per})
	}
	for i, h := range hosts {
		g.memSample(ready.Add(10*time.Millisecond), h, profile, "load_end", int(total*fr[i])+180)
	}
	return ready, sess
}

func (g *evGen) unload(t time.Time, profile string, hosts []string, sess, reason string, loadedFor time.Duration, served int) {
	sev := history.Info
	if reason == "node loss" || reason == "child exited" {
		sev = history.Warn
	}
	g.add(t, history.TInstanceUnloaded, sev, profile, hosts, sess,
		fmt.Sprintf("%s was unloaded (%s) after %s loaded and %d requests served.", profile, reason, loadedFor.Round(time.Second), served),
		map[string]any{"reason": reason, "loaded_for_s": round1(loadedFor.Seconds()), "requests_served": served, "exit": "exit status 0"})
}

func pastEvents(now time.Time) []history.Event {
	g := &evGen{rnd: rand.New(rand.NewSource(42))}
	start := now.Add(-5 * time.Hour)
	at := func(min float64) time.Time { return start.Add(time.Duration(min * float64(time.Minute))) }

	g.add(at(0), history.TSchedulerStarted, history.Info, "", nil, "", "llm-scheduler 0.1.0 started with 12 profiles.",
		map[string]any{"version": "0.1.0", "profiles": 12, "listen": ":8080",
			"safety": map[string]any{"split_skew_margin": 0.15, "estimate_margin": 0.05, "load_memory_floor_mib": 768, "forced_fit_check": "off",
				"load_stall_timeout_s": 180, "worker_silence_s": 30, "worker_probe_interval_s": 0, "recovery_enabled": false,
				"recovery_actions": []string{"restart_rpc_server"}, "exclude_nodes": 1},
			"history": map[string]any{"max_file_mib": 16, "keep_files": 20, "memory_events": 5000}})

	type sess struct {
		min     float64
		profile string
		hosts   []string
		hold    float64 // minutes loaded
		served  int
		evicted string // unloaded by eviction for this profile
	}
	rot := []sess{
		{3, "gemma-4-12b", []string{ps5A.Host}, 16, 41, ""},
		{6, "qwen3.5-4b", []string{omen.Host}, 31, 88, ""},
		{24, "qwen3.8-27b", ps3, 22, 17, ""},
		{49, "devstral-24b", []string{ps5B.Host}, 12, 9, ""},
		{52, "gemma-4-12b:48k", []string{ps5A.Host, ps5C.Host}, 11, 6, ""},
		{91, "gemma-4-12b", []string{ps5A.Host}, 23, 64, "qwen3.8-27b"},
		{116, "qwen3.8-27b", ps3, 28, 21, ""},
		{148, "qwen3.5-4b", []string{omen.Host}, 40, 120, ""},
		{151, "devstral-24b", []string{ps5C.Host}, 9, 5, ""},
		{168, "gemma-4-12b", []string{ps5B.Host}, 19, 47, ""},
		{196, "kimi-dev-72b-iq2s", ps3, 14, 3, ""},
		{219, "bench-gemma-12b-1w", []string{ps5A.Host}, 1.5, 6, ""},
		{223, "bench-gemma-12b-2w", []string{ps5A.Host, ps5B.Host}, 1.4, 5, ""},
		{231, "qwen3.8-27b", ps3, 33, 26, ""},
		{268, "gemma-4-12b", []string{ps5C.Host}, 60, 39, ""},
		{272, "qwen3.5-4b", []string{omen.Host}, 60, 15, ""},
	}
	learned := map[string]bool{}
	for i, s := range rot {
		ready, id := g.load(at(s.min), s.profile, s.hosts, 3+i, !learned[s.profile])
		learned[s.profile] = true
		end := ready.Add(time.Duration(s.hold * float64(time.Minute)))
		if end.After(now.Add(-2 * time.Minute)) {
			continue
		}
		reason := "ttl"
		if s.evicted != "" {
			reason = "eviction"
			g.add(end.Add(-1500*time.Millisecond), history.TEviction, history.Info, s.profile, s.hosts, id,
				fmt.Sprintf("%s is being evicted to make room for %s, which waited 12s (1 waiting).", s.profile, s.evicted),
				map[string]any{"victim": s.profile, "for_profile": s.evicted, "waited_s": 12.4, "waiters": 1, "victim_busy": false, "in_flight": 0})
		}
		if strings.HasPrefix(s.profile, "bench-") {
			reason = "api"
		}
		g.unload(end, s.profile, s.hosts, id, reason, end.Sub(ready), s.served)
	}

	// a few periodic memory samples while qwen3.8-27b is loaded
	for k := 0; k < 4; k++ {
		g.memSample(at(240+float64(k)*5), ps5B.Host, "qwen3.8-27b", "periodic", 6520+k*12)
	}

	// needs_mib too low on the 48k variant
	g.add(at(52).Add(68*time.Second), history.TNeedsMiBTooLow, history.Warn, "gemma-4-12b:48k", []string{ps5A.Host, ps5C.Host}, sessionID(at(52), 7),
		"The configured needs_mib of gemma-4-12b:48k (8000 MiB) is too low: the load measured 8206 MiB, which is used from now on.",
		map[string]any{"configured_mib": 8000, "measured_mib": 8206})

	// refusals
	refuse := func(t time.Time, profile, reason string, status int, msg string, extra map[string]any) {
		data := map[string]any{"reason": reason, "status": status, "message": msg, "waiters": 1}
		for k, v := range extra {
			data[k] = v
		}
		g.add(t, history.TRequestRefused, history.Warn, profile, nil, "",
			fmt.Sprintf("Request for %s refused before start (%s): %s.", profile, reason, strings.TrimRight(msg, ".")), data)
	}
	tooBig := "profile llama33-70b-q2 needs 47800 MiB (fit rule requires 54970 MiB) but the cluster can hold at most 45795 MiB; nodes excluded by config and not counted: laptop-2"
	refuse(at(38), "llama33-70b-q2", "does not fit", 503, tooBig, map[string]any{"need_mib": 47800, "required_mib": 54970, "capacity_mib": 45795})
	refuse(at(205), "llama33-70b-q2", "does not fit", 503, tooBig, map[string]any{"need_mib": 47800, "required_mib": 54970, "capacity_mib": 45795, "suppressed_since_last": 2})
	refuse(at(141), "devstral-24b", "cluster busy", 503, "waited 600s without a placement: placer: need 12100 MiB (requires 12100 MiB) does not fit the available nodes",
		map[string]any{"retry_after_s": 30.0})

	// a stalled load of kimi, a failure and a backoff
	kt := at(70)
	ks := sessionID(kt, 2)
	g.add(kt, history.TLoadStarted, history.Info, "kimi-dev-72b-iq2s", ps3, ks, "Loading kimi-dev-72b-iq2s onto console-1, console-2 and console-3.",
		map[string]any{"workers": addrsOf(ps3), "bytes_total": modelBytes[fileKimi], "need_mib": 26800, "need_basis": "measured, needs_mib"})
	w := modelBytes[fileKimi] / 3
	sent := w + w/5
	st := kt.Add(5*time.Minute + 9*time.Second)
	g.add(st, history.TLoadStalled, history.Error, "kimi-dev-72b-iq2s", ps3, ks,
		fmt.Sprintf("Loading kimi-dev-72b-iq2s stalled: the workers acknowledged no new bytes for 3m0s (%d of %d bytes sent).", sent, modelBytes[fileKimi]),
		map[string]any{"stalled_s": 180.0, "limit_s": 180.0, "bytes_sent": sent, "bytes_total": modelBytes[fileKimi],
			"per_worker_bytes": map[string]any{ps5A.Addr(): w, ps5B.Addr(): w / 5, ps5C.Addr(): 0}})
	g.add(st.Add(200*time.Millisecond), history.TLoadFinished, history.Error, "kimi-dev-72b-iq2s", ps3, ks,
		"Loading kimi-dev-72b-iq2s failed after 309.2 s: load stalled: no bytes acknowledged by the workers for 3m0s (sent "+fmt.Sprint(sent)+" of "+fmt.Sprint(modelBytes[fileKimi])+").",
		map[string]any{"outcome": "failed", "reason": fmt.Sprintf("load stalled: no bytes acknowledged by the workers for 3m0s (sent %d of %d)", sent, modelBytes[fileKimi]),
			"load_s": 309.2, "bytes": sent, "bytes_measured": true, "avg_mb_s": round1(float64(sent) / 1e6 / 309.2),
			"nodes_detail": []map[string]any{{"node": ps5A.Host, "bytes": w}, {"node": ps5B.Host, "bytes": w / 5}, {"node": ps5C.Host, "bytes": 0}}})
	g.add(st.Add(300*time.Millisecond), history.TProfileBackedOff, history.Warn, "kimi-dev-72b-iq2s", nil, "",
		"Profile kimi-dev-72b-iq2s is backed off for 1m0s after a failed load (level 1).", map[string]any{"for_s": 60.0, "level": 1})
	refuse(st.Add(20*time.Second), "kimi-dev-72b-iq2s", "backoff", 503, "profile kimi-dev-72b-iq2s is backed off after a failed load", map[string]any{"retry_after_s": 40.0})

	// a hung worker, quarantine, recovery request and outcome
	ht := at(130)
	b := ps5B.Host
	ch := func(up, hb string) map[string]any {
		return map[string]any{"upload": up, "heartbeat": hb, "probe": "unknown"}
	}
	g.add(ht, history.TWorkerHealth, history.Warn, "", []string{b}, "", fmt.Sprintf("Worker %s health is now suspect (was ok).", b),
		map[string]any{"state": "suspect", "previous": "ok", "addr": ps5B.Addr(), "instances": "qwen3.8-27b", "channels": ch("silent", "ok"),
			"silent": "upload silent for 31s", "node_lost": false})
	g.add(ht.Add(31*time.Second), history.TWorkerHealth, history.Error, "", []string{b}, "", fmt.Sprintf("Worker %s is hung (upload silent for 62s, heartbeat silent for 31s); its node is quarantined.", b),
		map[string]any{"state": "hung", "previous": "suspect", "addr": ps5B.Addr(), "instances": "qwen3.8-27b", "channels": ch("silent", "silent"),
			"silent": "upload silent for 62s, heartbeat silent for 31s", "node_lost": false})
	g.add(ht.Add(31*time.Second+5*time.Millisecond), history.TNodeQuarantined, history.Warn, "", []string{b}, "",
		fmt.Sprintf("Node %s was quarantined for 5m0s (level 1): worker hung.", b), map[string]any{"for_s": 300.0, "level": 1, "cause": "worker hung"})
	nonce := "rq-2c81d7e4"
	g.add(ht.Add(32*time.Second), history.TRecoveryRequest, history.Info, "", []string{b}, "",
		fmt.Sprintf("Recovery request restart_rpc_server sent for %s (step 1 of 1): worker hung: upload silent for 62s, heartbeat silent for 31s.", b),
		map[string]any{"action": "restart_rpc_server", "nonce": nonce, "step": 1, "of": 1, "reason": "worker hung: upload silent for 62s, heartbeat silent for 31s",
			"expires_at": ht.Add(77 * time.Second).UnixMilli()})
	g.add(ht.Add(32*time.Second+140*time.Millisecond), history.TRecoveryResponse, history.Info, "", []string{b}, "",
		fmt.Sprintf("The fleet manager answered the restart_rpc_server request for %s with HTTP 200 (stored).", b),
		map[string]any{"action": "restart_rpc_server", "nonce": nonce, "outcome": "stored", "http_status": 200, "body": `{"ok":true}`})
	g.add(ht.Add(81*time.Second), history.TRecoveryOutcome, history.Info, "", []string{b}, "",
		fmt.Sprintf("Node %s reported ok of restart_rpc_server after 8240 ms.", b),
		map[string]any{"action": "restart_rpc_server", "outcome": "ok", "nonce": nonce, "reason": "", "duration_ms": 8240, "ours": true, "at": ht.Add(79 * time.Second).UnixMilli()})
	g.add(ht.Add(106*time.Second), history.TWorkerHealth, history.Info, "", []string{b}, "", fmt.Sprintf("Worker %s recovered after being hung for 1m15s.", b),
		map[string]any{"state": "ok", "previous": "hung", "addr": ps5B.Addr(), "instances": "qwen3.8-27b", "channels": ch("ok", "ok"), "silent": "", "node_lost": false, "hung_for_s": 75.0})
	g.add(ht.Add(331*time.Second), history.TQuarantineEnded, history.Info, "", []string{b}, "", fmt.Sprintf("Node %s was released from quarantine.", b), nil)

	// the fleet view goes stale for a while
	ft := at(160)
	g.add(ft, history.TFleetViewStale, history.Warn, "", nil, "", "The fleet view is stale (61 s old, limit 60 s): remote nodes are not used until the console is heard from.",
		map[string]any{"age_s": 61.0, "stale_after_s": 60.0, "connected": false})
	g.add(ft.Add(34*time.Second), history.TFleetViewRecovered, history.Info, "", nil, "", "The fleet view is live again after 34 s.",
		map[string]any{"stale_for_s": 34.0})
	refuse(ft.Add(12*time.Second), "gemma-4-12b", "fleet view stale", 503, "fleet view stale; placer: need 7900 MiB (requires 7900 MiB) does not fit the available nodes",
		map[string]any{"retry_after_s": 30.0})

	// a pre-flight failure
	g.add(at(186), history.TPreflightFailed, history.Warn, "devstral-24b", []string{ps5C.Host}, "",
		"Pre-flight of devstral-24b failed for console-3 (timeout): list-devices did not finish within 20s.",
		map[string]any{"kind": "timeout", "addr": ps5C.Addr(), "duration_s": 20.0, "err": "list-devices did not finish within 20s"})

	sort.SliceStable(g.out, func(i, j int) bool { return g.out[i].Ts < g.out[j].Ts })
	return g.out
}
