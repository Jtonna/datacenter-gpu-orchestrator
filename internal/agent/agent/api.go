package agent

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/launch"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// ReasonNotCurrent answers a stop of an id that is not the current launch.
// CONTRACT-QUESTION: 6.5 gives the 409 but no reason word; the agent uses
// "not_current".
const ReasonNotCurrent = "not_current"

// ReasonStarting answers 503 while the agent has not yet re-adopted a launch
// left by an earlier agent process: the launch state is not known yet.
const ReasonStarting = "agent_starting"

// ReasonForbidden answers a request from an address that is not allowed.
const ReasonForbidden = "forbidden"

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func refusalBody(reason, msg string) agentproto.LaunchRefusal {
	return agentproto.LaunchRefusal{Reason: reason, Message: msg}
}

// Allowed reports whether a request's source address may call the agent API
// (contract 2.2): the configured Potluck address, or an --allow-from entry.
func (a *Agent) Allowed(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	if pl, ok := netip.AddrFromSlice(a.potluckIP); ok && pl.Unmap() == ip {
		return true
	}
	for _, p := range a.opt.AllowFrom {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// Handler serves Potluck's calls on the node's address (port 50060).
// GET /status is not served here: it is answered on 127.0.0.1 only.
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /launch", a.postLaunch)
	mux.HandleFunc("GET /launch", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.Launch.State()) })
	mux.HandleFunc("GET /launch/log", a.getLog)
	mux.HandleFunc("POST /launch/{id}/lease", func(w http.ResponseWriter, r *http.Request) {
		if !a.Launch.Renew(r.PathValue("id")) {
			writeJSON(w, 404, refusalBody(ReasonNotCurrent, "not the current launch"))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /launch/{id}", func(w http.ResponseWriter, r *http.Request) {
		st, err := a.Launch.Stop(r.PathValue("id"), r.URL.Query().Get("force") == "1")
		if errors.Is(err, launch.ErrNotCurrent) {
			writeJSON(w, 409, refusalBody(ReasonNotCurrent, "not the current launch; nothing stopped"))
			return
		}
		writeJSON(w, 200, st)
	})
	mux.HandleFunc("POST /reset", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, a.Launch.Reset(r.Context()))
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.Allowed(r.RemoteAddr) {
			writeJSON(w, 403, refusalBody(ReasonForbidden, "source address not allowed"))
			return
		}
		// POST /reset needs no launch state and stays open.
		if r.URL.Path != "/reset" && !a.launchRecovered() {
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusServiceUnavailable, refusalBody(ReasonStarting, "the agent is starting; the launch state is not read yet"))
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// LocalHandler serves 127.0.0.1:50060: GET /status only (6.9). Like the
// Potluck-facing API it does not answer from a launch state that is not read
// yet: right after an agent restart it waits for the re-adoption (a moment),
// and answers 503 when that takes longer than 2 s. A launch that runs is
// never shown as role idle.
func (a *Agent) LocalHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != agentproto.PathStatus {
			writeJSON(w, 403, refusalBody(ReasonForbidden, "only GET /status is answered on 127.0.0.1"))
			return
		}
		wait := a.statusWait
		if wait == 0 {
			wait = 2 * time.Second
		}
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-a.recovered:
		case <-r.Context().Done():
			return
		case <-t.C:
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusServiceUnavailable, refusalBody(ReasonStarting, "the agent is starting; the launch state is not read yet"))
			return
		}
		writeJSON(w, 200, a.Status())
	})
}

func (a *Agent) postLaunch(w http.ResponseWriter, r *http.Request) {
	var req agentproto.LaunchRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, 400, refusalBody(agentproto.ReasonInvalidRequest, "body: "+err.Error()))
		return
	}
	st, ref := a.Launch.Start(r.Context(), req)
	if ref != nil {
		writeJSON(w, ref.Status, ref.Body)
		return
	}
	writeJSON(w, http.StatusAccepted, st)
}

const maxLogChunk = 1 << 20

func (a *Agent) getLog(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	data, next, dropped, ok := a.Launch.Log(since, maxLogChunk)
	if !ok {
		writeJSON(w, 404, refusalBody(ReasonNotCurrent, "no launch"))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set(agentproto.HeaderNextOffset, strconv.FormatInt(next, 10))
	if dropped > 0 {
		w.Header().Set(agentproto.HeaderDroppedBytes, strconv.FormatInt(dropped, 10))
	}
	w.WriteHeader(200)
	w.Write(data)
}
