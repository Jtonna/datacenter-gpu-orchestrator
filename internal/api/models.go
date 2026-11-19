package api

import (
	"context"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/bench"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/gguf"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/history"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/proxy"
)

// The model catalog (GET /api/models, GET /api/models/{key}): one entry per
// model FILE the cluster has, not per profile. A profile is one configuration
// of a model (a context size, a placement); a model usually has several, and a
// benchmark matrix adds dozens more. The catalog joins what the other
// endpoints already say (profiles and their dry-run plans, the status, the
// event history, the benchmark aggregates) with the file's own metadata.
// Read-only.

// generatedID is the naming rule for profiles a tool made: the benchmark
// matrix writes bench-<model>-<place>-<ctx>, the earlier sweeps b-* and m-*.
var generatedID = regexp.MustCompile(`^(bench|b|m)-`)

// IsGeneratedProfile reports whether a profile is an auto-generated
// configuration. The profile's own `generated` field decides when it is set;
// otherwise the id does (bench-*, b-*, m-*).
func IsGeneratedProfile(id string, marker *bool) bool {
	if marker != nil {
		return *marker
	}
	return generatedID.MatchString(id)
}

// ModelFile is what is known about one model file.
type ModelFile struct {
	File      string `json:"file"`
	Exists    bool   `json:"exists"`
	SizeBytes int64  `json:"size_bytes"`
	// Name, Family, Parameters and Quant come from the GGUF metadata when it
	// could be read (MetaSource "gguf"), else from the file name ("file_name").
	Name          string `json:"name"`
	Publisher     string `json:"publisher,omitempty"`
	Family        string `json:"family,omitempty"` // general.architecture
	Parameters    string `json:"parameters,omitempty"`
	Quant         string `json:"quant,omitempty"`
	ContextLength int    `json:"context_length,omitempty"` // the model's trained context
	MetaSource    string `json:"meta_source"`
}

var (
	quantRe = regexp.MustCompile(`(?i)(?:^|[-_.])((?:IQ|Q)\d+(?:_[A-Z0-9]+)*|BF16|F16|F32)(?:$|[-_.])`)
	paramRe = regexp.MustCompile(`(?i)(?:^|[-_.])((?:\d+x)?\d+(?:\.\d+)?)b(?:$|[-_.])`)
)

// ModelFileFromName fills a ModelFile from the file name alone: the stem
// without the quantization is the name, "<publisher>_" in front of it is the
// publisher, "<n>B" the parameter label.
func ModelFileFromName(file string) ModelFile {
	mf := ModelFile{File: file, MetaSource: "file_name"}
	stem := strings.TrimSuffix(file, filepath.Ext(file))
	if m := quantRe.FindStringSubmatchIndex(stem); m != nil {
		mf.Quant = strings.ToUpper(stem[m[2]:m[3]])
		stem = strings.TrimRight(stem[:m[2]], "-_.") + stem[m[3]:]
		stem = strings.TrimRight(stem, "-_.")
	}
	if i := strings.Index(stem, "_"); i > 0 && !strings.ContainsAny(stem[:i], "-. ") && i+1 < len(stem) {
		mf.Publisher, stem = stem[:i], stem[i+1:]
	}
	if m := paramRe.FindStringSubmatch(stem); m != nil {
		mf.Parameters = strings.ToUpper(m[1]) + "B"
		mf.Parameters = strings.Replace(mf.Parameters, "X", "x", 1)
	}
	mf.Name = strings.ReplaceAll(stem, "_", " ")
	return mf
}

// ReadModelFile stats one file in dir and reads its GGUF header.
func ReadModelFile(dir, file string) ModelFile {
	mf := ModelFileFromName(file)
	st, err := os.Stat(filepath.Join(dir, file))
	if err != nil || st.IsDir() {
		return mf
	}
	mf.Exists, mf.SizeBytes = true, st.Size()
	meta, err := gguf.ReadMeta(filepath.Join(dir, file))
	if err != nil {
		return mf
	}
	mf.MetaSource = "gguf"
	mf.Family = meta.Architecture
	mf.ContextLength = int(meta.ContextLength)
	str := func(k string) string {
		v, _ := meta.Scalar(k)
		s, _ := v.(string)
		return strings.TrimSpace(s)
	}
	if n := str("general.name"); n != "" {
		mf.Name = n
	}
	if n := str("general.size_label"); n != "" {
		mf.Parameters = n
	}
	if n := str("general.organization"); n != "" {
		mf.Publisher = n
	}
	return mf
}

// modelFileCache remembers the metadata of the files in the models directory,
// keyed by name, size and modification time, so a header is parsed once.
type modelFileCache struct {
	mu   sync.Mutex
	seen map[string]cachedFile
}

type cachedFile struct {
	size int64
	mod  time.Time
	mf   ModelFile
}

// list returns every *.gguf in dir plus the named files (which may be missing).
// It runs on the request's goroutine: the scheduler loop is never involved.
func (c *modelFileCache) list(dir string, named []string) []ModelFile {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = map[string]cachedFile{}
	}
	names := map[string]bool{}
	for _, n := range named {
		if n != "" {
			names[n] = true
		}
	}
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".gguf") {
				names[e.Name()] = true
			}
		}
	}
	out := make([]ModelFile, 0, len(names))
	for n := range names {
		st, err := os.Stat(filepath.Join(dir, n))
		if err != nil || st.IsDir() {
			delete(c.seen, n)
			out = append(out, ModelFileFromName(n))
			continue
		}
		if e, ok := c.seen[n]; ok && e.size == st.Size() && e.mod.Equal(st.ModTime()) {
			out = append(out, e.mf)
			continue
		}
		mf := ReadModelFile(dir, n)
		c.seen[n] = cachedFile{size: st.Size(), mod: st.ModTime(), mf: mf}
		out = append(out, mf)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out
}

// CatalogInput is everything the catalog is joined from. Profiles are the
// entries of GET /api/profiles, Status the GET /api/status document,
// Aggregates the GET /api/bench/aggregates document (nil when benchmarks are
// off); all three in their JSON form, so the dev harness can feed scripted or
// recorded documents through the same code.
type CatalogInput struct {
	Profiles   []map[string]any
	Status     map[string]any
	Events     []history.Event // newest first
	Aggregates map[string]any
	Files      []ModelFile
	HistoryOn  bool
}

// ---- loose JSON readers (a document may come from jsonView or from a decoder) ----

func jmap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func jstr(v any) string         { s, _ := v.(string); return s }
func jnum(v any) float64        { f, _ := history.Num(v); return f }
func jbool(v any) bool          { b, _ := v.(bool); return b }

func jlist(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case []map[string]any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = x[i]
		}
		return out
	case []string:
		out := make([]any, len(x))
		for i := range x {
			out[i] = x[i]
		}
		return out
	case []int:
		out := make([]any, len(x))
		for i := range x {
			out[i] = x[i]
		}
		return out
	}
	return nil
}

func jstrs(v any) []string {
	out := []string{}
	for _, x := range jlist(v) {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func medianOf(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return round1((s[len(s)/2-1] + s[len(s)/2]) / 2)
}

// commandKey is the catalog key of a profile of kind command (it has no model file).
const commandKey = "command:"

type modelAcc struct {
	key      string
	file     ModelFile
	kind     string
	profiles []map[string]any
	ids      map[string]bool
}

// BuildModelCatalog joins the input into one entry per model file (and one
// per command profile), sorted by name. detailKey, when not empty, selects
// one entry and adds its recent events and its benchmarks; the result then
// has at most one element.
func BuildModelCatalog(in CatalogInput, detailKey string) []map[string]any {
	files := map[string]ModelFile{}
	for _, f := range in.Files {
		files[f.File] = f
	}
	accs := map[string]*modelAcc{}
	get := func(key, kind string) *modelAcc {
		a := accs[key]
		if a == nil {
			a = &modelAcc{key: key, kind: kind, ids: map[string]bool{}}
			accs[key] = a
		}
		return a
	}
	profileModel := map[string]string{} // profile id -> catalog key
	profileCtx := map[string]int{}
	for _, p := range in.Profiles {
		id, model := jstr(p["id"]), jstr(p["model"])
		var a *modelAcc
		if jstr(p["kind"]) == "command" || model == "" {
			a = get(commandKey+id, "command")
			a.file = ModelFile{Name: id, MetaSource: "profile"}
		} else {
			a = get(model, "llama")
			if f, ok := files[model]; ok {
				a.file = f
			} else {
				a.file = ModelFileFromName(model)
			}
		}
		a.profiles = append(a.profiles, p)
		a.ids[id] = true
		profileModel[id] = a.key
		profileCtx[id] = int(jnum(p["ctx"]))
	}
	for _, f := range in.Files { // files no profile uses yet
		if accs[f.File] == nil {
			get(f.File, "llama").file = f
		}
	}

	// node id -> hostname, and the budgets an empty cluster offers
	host := map[string]string{}
	var budgets []float64
	view := jmap(in.Status["view"])
	for _, n := range jlist(view["nodes"]) {
		nm := jmap(n)
		if h := jstr(nm["hostname"]); h != "" {
			host[jstr(nm["id"])] = h
		}
		if !jbool(nm["excluded"]) && jbool(nm["eligible"]) {
			if b := jnum(nm["plan_budget_mib"]); b > 0 {
				budgets = append(budgets, b)
			}
		}
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(budgets)))
	// fitNodes: on how few nodes of the empty cluster the need fits (0 = on none).
	fitNodes := func(need float64) int {
		sum := 0.0
		for i, b := range budgets {
			sum += b
			if sum >= need {
				return i + 1
			}
		}
		return 0
	}

	// ---- status: instances, learned needs, refusals ----
	type instList = []map[string]any
	insts := map[string]instList{}
	for _, i := range jlist(in.Status["instances"]) {
		im := jmap(i)
		key, ok := profileModel[jstr(im["profile"])]
		if !ok {
			if mf := jstr(im["model_file"]); mf != "" && accs[mf] != nil {
				key = mf
			} else {
				continue
			}
		}
		nodes := []string{}
		for _, n := range jlist(im["nodes"]) {
			id := jstr(jmap(n)["id"])
			if h := host[id]; h != "" {
				id = h
			}
			nodes = append(nodes, id)
		}
		state := jstr(im["state"])
		e := map[string]any{"profile": im["profile"], "state": state, "nodes": nodes, "memory_mib": math.Round(jnum(im["device_total_mib"])),
			"in_flight": int(jnum(im["in_flight"])), "waiters": int(jnum(im["waiters"])), "requests_served": int(jnum(im["requests_served"])),
			"last_used": im["last_used"], "stale_profile": jbool(im["stale_profile"])}
		if state == "starting" {
			e["since"] = im["created"]
			e["phase"] = im["phase"]
			if v, ok := im["load_progress"]; ok {
				e["load_progress"] = v
			}
			if v, ok := im["load_eta_s"]; ok {
				e["load_eta_s"] = v
			}
		} else {
			e["since"] = im["ready_since"]
			e["load_s"] = im["load_duration"]
		}
		if lr := jmap(im["last_request"]); lr != nil {
			e["last_request_tps"] = lr["tokens_per_second"]
		}
		insts[key] = append(insts[key], e)
	}

	type measure struct {
		nodes   int
		mib     float64
		at      int64
		profile string
		source  string
	}
	measured := map[string]map[int]measure{}
	addMeasure := func(key string, m measure) {
		if m.nodes < 1 || m.mib <= 0 {
			return
		}
		if measured[key] == nil {
			measured[key] = map[int]measure{}
		}
		// the smallest context is the model's floor: keep the lowest figure per node count, the newest among equals
		if cur, ok := measured[key][m.nodes]; !ok || m.mib < cur.mib || (m.mib == cur.mib && m.at > cur.at) {
			measured[key][m.nodes] = m
		}
	}
	for id, v := range jmap(in.Status["learned_needs"]) {
		if key, ok := profileModel[id]; ok {
			lm := jmap(v)
			addMeasure(key, measure{nodes: len(jlist(lm["nodes"])), mib: jnum(lm["measured_mib"]), profile: id, source: "this run"})
		}
	}
	refusal := map[string]map[string]any{}
	noteRefusal := func(key string, at int64, r map[string]any) {
		if cur := refusal[key]; cur == nil || jnum(cur["at"]) < float64(at) {
			r["at"] = at
			refusal[key] = r
		}
	}
	for _, r := range jlist(in.Status["last_refusals"]) {
		rm := jmap(r)
		key, ok := profileModel[jstr(rm["profile"])]
		if !ok {
			continue
		}
		at, _ := time.Parse(time.RFC3339Nano, jstr(rm["at"]))
		noteRefusal(key, at.UnixMilli(), map[string]any{"profile": rm["profile"], "reason": rm["reason"], "message": rm["message"], "status": rm["status"]})
	}

	// ---- history ----
	type loadAgg struct {
		count, ready, failed, cancelled int
		last                            map[string]any
		lastLoaded                      int64
		lastActive                      int64
		served                          int
		unloads                         int
		refusals                        int
		byNodes                         map[int]*struct {
			secs, rates []float64
			lastS       float64
			lastAt      int64
		}
		events []history.Event
	}
	loads := map[string]*loadAgg{}
	la := func(key string) *loadAgg {
		if loads[key] == nil {
			loads[key] = &loadAgg{byNodes: map[int]*struct {
				secs, rates []float64
				lastS       float64
				lastAt      int64
			}{}}
		}
		return loads[key]
	}
	for i := range in.Events {
		e := &in.Events[i]
		key, ok := profileModel[e.Profile]
		if !ok {
			if e.ModelFile == "" || accs[e.ModelFile] == nil {
				continue
			}
			key = e.ModelFile
		}
		a := la(key)
		if detailKey == key && e.Type != history.TNodeMemory && len(a.events) < 200 {
			a.events = append(a.events, *e)
		}
		switch e.Type {
		case history.TLoadFinished:
			a.count++
			outcome := jstr(e.Data["outcome"])
			if a.last == nil {
				a.last = map[string]any{"at": e.Ts, "profile": e.Profile, "nodes": e.Nodes, "outcome": outcome,
					"load_s": e.Data["load_s"], "mb_s": e.Data["avg_mb_s"]}
				if r := jstr(e.Data["reason"]); r != "" {
					a.last["reason"] = r
				}
			}
			switch outcome {
			case "ready":
				a.ready++
				if e.Ts > a.lastLoaded {
					a.lastLoaded = e.Ts
				}
				if e.Ts > a.lastActive {
					a.lastActive = e.Ts
				}
				n := len(e.Nodes)
				b := a.byNodes[n]
				if b == nil {
					b = &struct {
						secs, rates []float64
						lastS       float64
						lastAt      int64
					}{}
					a.byNodes[n] = b
				}
				if s, ok := history.Num(e.Data["load_s"]); ok {
					b.secs = append(b.secs, s)
					if e.Ts > b.lastAt {
						b.lastAt, b.lastS = e.Ts, s
					}
				}
				if r, ok := history.Num(e.Data["avg_mb_s"]); ok && r > 0 {
					b.rates = append(b.rates, r)
				}
				mem := 0.0
				for _, d := range jlist(e.Data["nodes_detail"]) {
					mem += jnum(jmap(d)["memory_mib"])
				}
				addMeasure(key, measure{nodes: n, mib: math.Round(mem), at: e.Ts, profile: e.Profile, source: "history"})
			case "failed":
				a.failed++
			case "cancelled":
				a.cancelled++
			}
		case history.TInstanceUnloaded:
			a.unloads++
			a.served += int(jnum(e.Data["requests_served"]))
			if e.Ts > a.lastActive {
				a.lastActive = e.Ts
			}
		case history.TRequestRefused:
			a.refusals++
			noteRefusal(key, e.Ts, map[string]any{"profile": e.Profile, "reason": e.Data["reason"], "message": e.Data["message"], "status": e.Data["status"]})
		}
	}

	// ---- benchmarks ----
	type benchAcc struct {
		byNodes    []map[string]any
		benchmarks []map[string]any
		ids        []string
		runs       int
	}
	benches := map[string]*benchAcc{}
	ba := func(key string) *benchAcc {
		if benches[key] == nil {
			benches[key] = &benchAcc{}
		}
		return benches[key]
	}
	metric := func(ms map[string]any, name string) any {
		m := jmap(ms[name])
		if m == nil {
			return nil
		}
		return map[string]any{"median": m["median"], "last": m["last"], "count": m["count"]}
	}
	for _, g := range jlist(in.Aggregates["by_model_nodes"]) {
		gm := jmap(g)
		key := jstr(gm["model_file"])
		if accs[key] == nil {
			continue
		}
		ms := jmap(gm["metrics"])
		row := map[string]any{"nodes": int(jnum(gm["nodes"])), "runs": int(jnum(gm["runs"])), "steps": int(jnum(gm["steps"]))}
		for _, name := range []string{"load_s", "load_mb_s", "decode_tps", "prefill_tps", "memory_mib", "ttft_ms"} {
			if v := metric(ms, name); v != nil {
				row[name] = v
			}
		}
		b := ba(key)
		b.byNodes = append(b.byNodes, row)
	}
	for _, g := range jlist(in.Aggregates["benchmarks"]) {
		gm := jmap(g)
		key, ok := profileModel[jstr(gm["profile"])]
		if !ok {
			continue
		}
		b := ba(key)
		b.ids = append(b.ids, jstr(gm["benchmark_id"]))
		b.runs += int(jnum(gm["runs"]))
		ms := jmap(gm["metrics"])
		row := map[string]any{"benchmark_id": gm["benchmark_id"], "name": gm["name"], "profile": gm["profile"], "runs": gm["runs"],
			"pass_rate": gm["pass_rate"], "outcomes": gm["outcomes"], "last_at": gm["last_at"], "last_run_id": gm["last_run_id"]}
		for _, name := range []string{"load_s", "load_mb_s", "decode_tps", "prefill_tps", "memory_mib"} {
			if v := metric(ms, name); v != nil {
				row[name] = v
			}
		}
		b.benchmarks = append(b.benchmarks, row)
	}

	// ---- one entry per model ----
	out := make([]map[string]any, 0, len(accs))
	for key, a := range accs {
		if detailKey != "" && key != detailKey {
			continue
		}
		e := map[string]any{"key": key, "kind": a.kind, "file": a.file.File, "exists": a.file.Exists, "size_bytes": a.file.SizeBytes,
			"name": a.file.Name, "meta_source": a.file.MetaSource}
		if a.kind == "command" {
			e["exists"] = true
		}
		for k, v := range map[string]string{"publisher": a.file.Publisher, "family": a.file.Family, "parameters": a.file.Parameters, "quant": a.file.Quant} {
			if v != "" {
				e[k] = v
			}
		}
		if a.file.ContextLength > 0 {
			e["context_length"] = a.file.ContextLength
		}

		// configurations
		sort.Slice(a.profiles, func(i, j int) bool { return jstr(a.profiles[i]["id"]) < jstr(a.profiles[j]["id"]) })
		profs := make([]map[string]any, 0, len(a.profiles))
		generated, startable := 0, 0
		var need float64
		needBasis, needMeasured := "", false
		for _, p := range a.profiles {
			force := jstrs(p["force_nodes"])
			placement := "automatic"
			if len(force) > 0 {
				placement = "forced"
			}
			gen := jbool(p["generated"])
			if gen {
				generated++
			}
			plan := jmap(p["plan"])
			if jbool(plan["would_start"]) {
				startable++
			}
			pe := map[string]any{"id": p["id"], "ctx": p["ctx"], "parallel": p["parallel"], "placement": placement, "force_nodes": force,
				"tensor_split": jlist(p["tensor_split"]), "aliases": jstrs(p["aliases"]), "generated": gen, "plan": plan}
			if pe["tensor_split"] == nil {
				pe["tensor_split"] = []any{}
			}
			if n := jnum(p["needs_mib"]); n > 0 {
				pe["needs_mib"], pe["needs_basis"], pe["needs_measured"] = p["needs_mib"], p["needs_basis"], jbool(p["needs_measured"])
				// the model's own need is that of its lightest configuration
				if need == 0 || n < need {
					need, needBasis, needMeasured = n, jstr(p["needs_basis"]), jbool(p["needs_measured"])
				}
			}
			profs = append(profs, pe)
		}
		e["profiles"] = profs
		e["profile_count"], e["generated_count"], e["startable_count"] = len(profs), generated, startable

		// instances and state
		il := insts[key]
		if il == nil {
			il = instList{}
		}
		e["instances"] = il
		state := "not_loaded"
		for _, i := range il {
			if i["state"] == "starting" {
				if state == "not_loaded" {
					state = "loading"
				}
			} else {
				state = "ready"
			}
		}
		e["state"] = state

		// measured memory by node count
		ms := []map[string]any{}
		for _, m := range measured[key] {
			row := map[string]any{"nodes": m.nodes, "memory_mib": m.mib, "profile": m.profile, "source": m.source}
			if c := profileCtx[m.profile]; c > 0 {
				row["ctx"] = c
			}
			if m.at > 0 {
				row["at"] = m.at
			}
			ms = append(ms, row)
		}
		sort.Slice(ms, func(i, j int) bool { return ms[i]["nodes"].(int) < ms[j]["nodes"].(int) })
		e["measured"] = ms

		// what it needs: a measured figure when one exists, else the lightest configuration's need
		if a.kind == "llama" {
			fit := map[string]any{}
			if len(ms) > 0 {
				lo := ms[0]
				for _, m := range ms {
					if m["memory_mib"].(float64) < lo["memory_mib"].(float64) {
						lo = m
					}
				}
				fit["need_mib"], fit["basis"], fit["ran_on_nodes"] = lo["memory_mib"], "measured", ms[0]["nodes"]
				need = lo["memory_mib"].(float64)
			} else if need > 0 {
				fit["need_mib"] = need
				fit["basis"] = "estimated"
				if needMeasured {
					fit["basis"] = "configured"
				}
				fit["basis_text"] = needBasis
			}
			if need > 0 && len(budgets) > 0 {
				n := fitNodes(need)
				fit["min_nodes"] = n // 0: more than the whole cluster holds
				fit["node_budget_mib"] = budgets[0]
				total := 0.0
				for _, b := range budgets {
					total += b
				}
				fit["cluster_budget_mib"] = total
			}
			if len(fit) > 0 {
				e["fit"] = fit
			}
		}

		// loads from the history
		l := loads[key]
		if l == nil {
			l = la(key)
		}
		by := []map[string]any{}
		for n, b := range l.byNodes {
			row := map[string]any{"nodes": n, "count": len(b.secs), "median_s": medianOf(b.secs), "last_s": b.lastS, "last_at": b.lastAt}
			if len(b.rates) > 0 {
				t := 0.0
				for _, r := range b.rates {
					t += r
				}
				row["avg_mb_s"] = round1(t / float64(len(b.rates)))
			}
			by = append(by, row)
		}
		sort.Slice(by, func(i, j int) bool { return by[i]["nodes"].(int) < by[j]["nodes"].(int) })
		e["loads"] = map[string]any{"count": l.count, "ready": l.ready, "failed": l.failed, "cancelled": l.cancelled, "unloads": l.unloads,
			"refusals": l.refusals, "last": l.last, "by_nodes": by}
		served := l.served
		lastActive := l.lastActive
		for _, i := range il {
			served += i["requests_served"].(int)
		}
		e["requests_served"] = served
		e["last_loaded"], e["last_active"] = nilIfZero(l.lastLoaded), nilIfZero(lastActive)
		e["history"] = in.HistoryOn
		if r := refusal[key]; r != nil {
			e["last_refusal"] = r
		} else {
			e["last_refusal"] = nil
		}

		// benchmarks
		if b := benches[key]; b != nil {
			sort.Slice(b.byNodes, func(i, j int) bool { return b.byNodes[i]["nodes"].(int) < b.byNodes[j]["nodes"].(int) })
			sort.Strings(b.ids)
			bm := map[string]any{"benchmarks": b.ids, "runs": b.runs, "by_nodes": b.byNodes}
			if b.byNodes == nil {
				bm["by_nodes"] = []any{}
			}
			if detailKey != "" {
				bm["detail"] = b.benchmarks
			}
			e["bench"] = bm
		} else {
			e["bench"] = nil
		}
		if detailKey != "" {
			ev := l.events
			if ev == nil {
				ev = []history.Event{}
			}
			e["events"] = ev
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := strings.ToLower(jstr(out[i]["name"])), strings.ToLower(jstr(out[j]["name"]))
		if a != b {
			return a < b
		}
		return jstr(out[i]["key"]) < jstr(out[j]["key"])
	})
	return out
}

func nilIfZero(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// catalogEventTypes are the history events the catalog reads.
var catalogEventTypes = []string{history.TLoadStarted, history.TLoadFinished, history.TInstanceUnloaded, history.TRequestRefused,
	history.TLearnedNeed, history.TLoadStalled, history.TLoadLowMemory, history.TEviction, history.TChildExited, history.TProfileBackedOff,
	history.TPreflightFailed, history.TNeedsMiBTooLow, history.TBenchStepFinished}

// catalogInput gathers the live documents. The scheduler is only asked what
// GET /api/status and GET /api/profiles already ask it; file metadata is read
// here, on the request's goroutine, and cached.
func (s *Server) catalogInput(ctx context.Context) (CatalogInput, error) {
	in := CatalogInput{HistoryOn: s.hist.On()}
	st, err := s.statusView(ctx)
	if err != nil {
		return in, err
	}
	in.Status, _ = st.(map[string]any)
	in.Profiles = s.profileEntries(ctx)
	in.Events = s.hist.Query(history.Query{Types: catalogEventTypes, Limit: history.MaxLimit}).Events
	if s.opts.Bench != nil {
		if doc, err := s.opts.Bench.Aggregates(bench.AggFilter{}); err == nil {
			in.Aggregates = viaJSON(doc)
		}
	}
	if s.opts.modelFiles != nil {
		in.Files = s.opts.modelFiles()
	} else {
		var named []string
		for _, p := range in.Profiles {
			if jstr(p["kind"]) != "command" {
				named = append(named, jstr(p["model"]))
			}
		}
		in.Files = s.files.list(s.conf().ModelsDir, named)
	}
	return in, nil
}

// handleModelCatalog is GET /api/models: one entry per model file.
func (s *Server) handleModelCatalog(w http.ResponseWriter, r *http.Request) {
	in, err := s.catalogInput(r.Context())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": BuildModelCatalog(in, ""), "models_dir": s.conf().ModelsDir, "history": in.HistoryOn})
}

// handleModelDetail is GET /api/models/{key}: one model with its recent
// events and the benchmarks that use it. The key is the file name, or
// "command:<profile id>" for a profile of kind command.
func (s *Server) handleModelDetail(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	in, err := s.catalogInput(r.Context())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	out := BuildModelCatalog(in, key)
	if len(out) == 0 {
		proxy.WriteError(w, http.StatusNotFound, "no model file "+key+": see GET /api/models", "invalid_request_error", "not found", nil)
		return
	}
	writeJSON(w, http.StatusOK, out[0])
}
