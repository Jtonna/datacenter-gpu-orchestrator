package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/api"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
)

// The "real" scenarios replay a recording of a real scheduler (realdata/,
// written by --capture, GET answers only): 254 profiles, the real node ids,
// worker versions and reason strings, the real history, benchmark runs and
// audit trail. They exist so the web UI can be developed and photographed
// against real-shaped data without touching the real system.
//
// How it is served, DEV-ONLY:
//
//   - the recorded history events are written into the harness's real history
//     store at startup, so /api/history, its summary and export, and their
//     filters are the real server's own;
//   - every other recorded GET is answered from the recording, with all
//     timestamps moved by the time that passed since the recording, so
//     "9 min ago" in the recording reads "9 min ago" at startup and ages
//     from there. The status document is the exception: it is frozen at the
//     recorded moment (a load at 97% stays at 97%);
//   - GET /api/models is built by the real catalog code from the recording;
//   - POST /api/plan (a dry run the recorder may not send) is derived from the
//     recorded profile list;
//   - any other write is refused: the recording is read-only.
//
// They are startup scenarios (--scenario real): the history is seeded once and
// the seeded configuration of the other scenarios is not written.

//go:embed realdata
var realFS embed.FS

// realReplay is the loaded recording; nil unless the harness started in a real scenario.
var realReplay *replay

type replay struct {
	t0    int64 // when the recording was made (unix ms)
	delta int64 // startup minus t0
	docs  map[string]any
	files map[string]string
}

func loadReplay(start time.Time) (*replay, error) {
	b, err := realFS.ReadFile("realdata/index.json")
	if err != nil {
		return nil, fmt.Errorf("no recording (run --capture first): %w", err)
	}
	var idx captureIndex
	if err := json.Unmarshal(b, &idx); err != nil {
		return nil, err
	}
	rp := &replay{t0: idx.CapturedAt, delta: start.UnixMilli() - idx.CapturedAt, docs: map[string]any{}, files: idx.Entries}
	for key, file := range idx.Entries {
		v, err := rp.readFile(file)
		if err != nil {
			return nil, err
		}
		rp.docs[key] = v
	}
	return rp, nil
}

func (rp *replay) readFile(name string) (any, error) {
	b, err := realFS.ReadFile("realdata/" + name)
	if err != nil {
		return nil, err
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("realdata/%s: %w", name, err)
	}
	return v, nil
}

var isoTime = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z$`)

// shift returns a copy of v with every timestamp moved by delta ms: numbers
// that are unix milliseconds near the recording, and RFC 3339 strings.
func shift(v any, t0, delta int64) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = shift(e, t0, delta)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = shift(e, t0, delta)
		}
		return out
	case float64:
		if x == math.Trunc(x) && x > float64(t0)-90*86400e3 && x < float64(t0)+2*86400e3 {
			return x + float64(delta)
		}
	case string:
		if isoTime.MatchString(x) {
			if t, err := time.Parse(time.RFC3339Nano, x); err == nil {
				return t.Add(time.Duration(delta) * time.Millisecond).UTC().Format("2006-01-02T15:04:05.000Z")
			}
		}
	}
	return v
}

// doc is a recorded answer, aged by the time since startup.
func (rp *replay) doc(key string) (any, bool) {
	v, ok := rp.docs[key]
	if !ok {
		return nil, false
	}
	return shift(v, rp.t0, rp.delta), true
}

// status is a recorded status document frozen at its own moment.
func (rp *replay) status(file string) (map[string]any, error) {
	v, err := rp.readFile(file)
	if err != nil {
		return nil, err
	}
	m, _ := v.(map[string]any)
	t, err := time.Parse(time.RFC3339Nano, fmt.Sprint(m["time"]))
	if err != nil {
		return nil, fmt.Errorf("realdata/%s has no time", file)
	}
	out, _ := shift(m, t.UnixMilli(), time.Now().UnixMilli()-t.UnixMilli()).(map[string]any)
	return out, nil
}

// seedHistory writes the recorded events, oldest first, at their aged times.
func (rp *replay) seedHistory(h *history.Store) {
	v, _ := rp.doc("/api/history")
	b, _ := json.Marshal(v)
	var doc struct {
		Events []history.Event `json:"events"`
	}
	json.Unmarshal(b, &doc)
	sort.SliceStable(doc.Events, func(i, j int) bool { return doc.Events[i].Ts < doc.Events[j].Ts })
	for _, e := range doc.Events {
		e.Seq = 0
		h.Record(e)
	}
}

func (rp *replay) profiles() []map[string]any {
	v, _ := rp.doc("/api/profiles")
	m, _ := v.(map[string]any)
	list, _ := m["profiles"].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, p := range list {
		if pm, ok := p.(map[string]any); ok {
			if _, has := pm["generated"]; !has { // a recording older than the field
				pm["generated"] = api.IsGeneratedProfile(fmt.Sprint(pm["id"]), nil)
			}
			out = append(out, pm)
		}
	}
	return out
}

// modelFiles is what the recording says about the model files: every file a
// profile names exists (the scheduler validated that at its start), its size
// is the upload size of a recorded load when there was one, and the rest comes
// from the file name (the recorder cannot read the files themselves).
func (rp *replay) modelFiles(events []history.Event) []api.ModelFile {
	size := map[string]int64{}
	for _, e := range events {
		if e.Type == history.TLoadStarted && e.ModelFile != "" && size[e.ModelFile] == 0 {
			if n, ok := history.Num(e.Data["bytes_total"]); ok {
				size[e.ModelFile] = int64(n)
			}
		}
	}
	seen := map[string]bool{}
	var out []api.ModelFile
	for _, p := range rp.profiles() {
		f, _ := p["model"].(string)
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		mf := api.ModelFileFromName(f)
		mf.Exists, mf.SizeBytes = true, size[f]
		out = append(out, mf)
	}
	return out
}

// plan derives the answer of POST /api/plan from the recorded profile list.
func (rp *replay) plan(model string, status map[string]any) (map[string]any, bool) {
	var p map[string]any
	for _, x := range rp.profiles() {
		if x["id"] == model {
			p = x
		}
	}
	if p == nil {
		return nil, false
	}
	plan, _ := p["plan"].(map[string]any)
	need, _ := p["needs_mib"].(float64)
	basis, _ := p["needs_basis"].(string)
	fit := need
	if strings.HasPrefix(basis, "estimated") {
		margin := 0.05
		if sys, ok := rp.docs["/api/system"].(map[string]any); ok {
			if s, ok := sys["safety"].(map[string]any); ok {
				if m, ok := s["estimate_margin"].(float64); ok {
					margin = m
				}
			}
		}
		fit = math.Round(need * (1 + margin))
	}
	placement := []any{}
	view, _ := status["view"].(map[string]any)
	nodes, _ := view["nodes"].([]any)
	names, _ := plan["nodes"].([]any)
	for _, name := range names {
		for _, n := range nodes {
			nm, _ := n.(map[string]any)
			if nm["hostname"] == name || nm["id"] == name {
				placement = append(placement, map[string]any{"id": nm["id"], "addr": nm["addr"], "local": nm["local"], "budget_mib": nm["live_budget_mib"]})
			}
		}
	}
	out := map[string]any{"model": model, "alias": false, "action": plan["action"], "instance": plan["instance"], "placement": placement,
		"victims": []any{}, "reason": plan["reason"], "need_mib": need, "measured": p["needs_measured"], "fit_need_mib": fit, "need_basis": basis,
		"warning": "", "required_mib": 0, "capacity_mib": 0}
	if w, ok := plan["warning"].(string); ok {
		out["warning"] = w
	}
	if m := regexp.MustCompile(`\(requires (\d+) MiB\) exceeds cluster capacity (\d+) MiB`).FindStringSubmatch(fmt.Sprint(plan["reason"])); m != nil {
		out["required_mib"], _ = strconv.Atoi(m[1])
		out["capacity_mib"], _ = strconv.Atoi(m[2])
	}
	return out, true
}

// audit answers GET /api/audit and /api/audit/{id} from the recorded trail,
// with the filters the page uses.
func (rp *replay) audit(w http.ResponseWriter, r *http.Request, own []any) {
	v, _ := rp.doc("/api/audit")
	m, _ := v.(map[string]any)
	rec, _ := m["entries"].([]any)
	all := append(append([]any{}, own...), rec...) // what was done in this harness is newer than the recording
	if id := strings.TrimPrefix(r.URL.Path, "/api/audit"); id != "" && id != "/" {
		for _, e := range all {
			if em, _ := e.(map[string]any); fmt.Sprint(em["id"]) == strings.TrimPrefix(id, "/") || jsonNumber(em["id"]) == strings.TrimPrefix(id, "/") {
				writeJSON(w, 200, em)
				return
			}
		}
		writeErr(w, 404, "no such audit entry", "not found")
		return
	}
	q := r.URL.Query()
	tsOf := func(s string) int64 {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.UnixMilli()
		}
		return 0
	}
	since, until := tsOf(q.Get("since")), tsOf(q.Get("until"))
	text := strings.ToLower(q.Get("q"))
	out := []any{}
	for _, e := range all {
		em, _ := e.(map[string]any)
		ts, _ := em["ts"].(float64)
		if (since > 0 && int64(ts) < since) || (until > 0 && int64(ts) > until) {
			continue
		}
		skip := false
		for _, k := range []string{"source", "actor", "section", "target", "action"} {
			if want := q.Get(k); want != "" && fmt.Sprint(em[k]) != want {
				skip = true
			}
		}
		if want := q.Get("agent"); want != "" && em["agent_id"] != want {
			skip = true
		}
		if text != "" {
			b, _ := json.Marshal(em)
			skip = skip || !strings.Contains(strings.ToLower(string(b)), text)
		}
		if !skip {
			out = append(out, em)
		}
	}
	total := len(out)
	if limit := qInt(r, "limit", 100); limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	writeJSON(w, 200, map[string]any{"entries": out, "total_matching": total})
}

func jsonNumber(v any) string {
	if f, ok := v.(float64); ok {
		return strconv.FormatInt(int64(f), 10)
	}
	return ""
}

// serve answers a request from the recording; false means "not mine" (the
// real server answers: the history, the UI files, the live channel).
func (h *harness) serveReal(w http.ResponseWriter, r *http.Request) bool {
	rp := h.rp
	p := r.URL.Path
	if !strings.HasPrefix(p, "/api/") && !strings.HasPrefix(p, "/v1/") && p != "/mcp" {
		return false
	}
	// The parts that are not a recording: the model catalog (the real code on
	// the recorded data), the icons, the agents and the MCP tool list are the
	// real server's own, with its own database.
	if p == "/api/models" || (strings.HasPrefix(p, "/api/models/") && p != "/api/models/unload" && !strings.HasPrefix(p, "/api/models/unload/")) ||
		p == "/api/agents" || strings.HasPrefix(p, "/api/agents/") || p == "/api/mcp/tools" {
		return false
	}
	if strings.HasPrefix(p, "/api/history") || p == "/api/events" {
		return false
	}
	status := func() map[string]any {
		v, _ := h.st.statusSource(r.Context())
		m, _ := v.(map[string]any)
		return m
	}
	if r.Method == http.MethodPost && p == "/api/plan" {
		var req struct {
			Model string `json:"model"`
		}
		if !readJSONBody(w, r, &req) {
			return true
		}
		if out, ok := rp.plan(req.Model, status()); ok {
			writeJSON(w, 200, out)
		} else {
			writeUnknownModel(w, req.Model)
		}
		return true
	}
	if r.Method != http.MethodGet {
		writeErr(w, 405, "this is a recording of a real scheduler (scenario real): it is read-only, nothing can be changed or run here", "read-only recording")
		return true
	}
	switch {
	case p == "/api/status":
		writeJSON(w, 200, status())
		return true
	case p == "/api/profiles":
		writeJSON(w, 200, map[string]any{"profiles": rp.profiles()})
		return true
	case p == "/api/audit" || strings.HasPrefix(p, "/api/audit/"):
		rp.audit(w, r, h.ownAudit(r.Context()))
		return true
	case p == "/api/bench/aggregates":
		writeJSON(w, 200, h.realAggregates())
		return true
	case strings.HasPrefix(p, "/api/bench/benchmarks/") && strings.HasSuffix(p, "/results"):
		id, _ := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(p, "/api/bench/benchmarks/"), "/results"))
		writeJSON(w, 200, map[string]any{"benchmark": id, "results": bench.ResultsOf(rp.runs(), id)})
		return true
	case strings.HasPrefix(p, "/api/config/profiles/"):
		id, _ := url.PathUnescape(strings.TrimPrefix(p, "/api/config/profiles/"))
		if v, ok := rp.doc("/api/config/profiles"); ok {
			m, _ := v.(map[string]any)
			ps, _ := m["profiles"].(map[string]any)
			if prof, ok := ps[id]; ok {
				writeJSON(w, 200, map[string]any{"revision": m["revision"], "id": id, "profile": prof})
				return true
			}
		}
		writeErr(w, 404, "no such profile", "not found")
		return true
	}
	if p == "/api/bench/trends" {
		q := r.URL.Query()
		if v, ok := rp.doc(p + "?benchmark=" + url.QueryEscape(q.Get("benchmark")) + "&metric=" + q.Get("metric")); ok {
			writeJSON(w, 200, v)
			return true
		}
	}
	if v, ok := rp.doc(r.URL.EscapedPath()); ok {
		writeJSON(w, 200, v)
		return true
	}
	if v, ok := rp.doc(p); ok {
		writeJSON(w, 200, v)
		return true
	}
	writeErr(w, 404, "the recording has no answer for "+p, "not found")
	return true
}

// realCatalogInput gathers the catalog's input from the recording; the events
// are the recorded ones, read back from the real history store.
func (h *harness) realCatalogInput(status map[string]any) api.CatalogInput {
	ev := h.c.hist.Query(history.Query{Limit: history.MaxLimit}).Events
	in := api.CatalogInput{Status: status, Profiles: h.rp.profiles(), Events: ev, HistoryOn: h.c.hist.On(), Files: h.rp.modelFiles(ev)}
	in.Aggregates = h.realAggregates()
	return in
}

func init() {
	raw := func(file string) func() (map[string]any, error) {
		return func() (map[string]any, error) {
			if realReplay == nil {
				return nil, fmt.Errorf("the recording is only loaded with --scenario real at startup")
			}
			return realReplay.status(file)
		}
	}
	registerScenario(scenario{Name: "real", Real: true, StartupOnly: true, Raw: raw("status.json"),
		Description: "A recording of the real cluster, idle: 254 profiles (241 of them generated benchmark configurations) of 13 model files, three PS5s, the real history, benchmark runs and audit trail. Read-only. Only with --scenario real at startup."})
	registerScenario(scenario{Name: "real-loading", Real: true, StartupOnly: true, Raw: raw("status-loading.json"),
		Description: "The same recording at the moment a benchmark loaded Gemma 12B across the three PS5s (97%, two workers done). Read-only. Only after --scenario real or real-loading at startup."})
}
