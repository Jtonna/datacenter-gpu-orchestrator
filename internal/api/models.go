package api

import (
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/modelname"

	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/gguf"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
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
	Name string `json:"name"`
	// RawName is the name as the metadata or the file name wrote it, when Name
	// was tidied (TidyModelName); Brand is the model family for the avatar.
	RawName       string `json:"raw_name,omitempty"`
	Brand         string `json:"brand,omitempty"`
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
	mf.setName(stem, mf.Publisher)
	return mf
}

// setName stores the tidy display name of raw and what it implies.
func (mf *ModelFile) setName(raw, publisher string) {
	name, pub := TidyModelName(raw, publisher)
	mf.Name, mf.Publisher = name, pub
	mf.RawName = ""
	if raw != name {
		mf.RawName = raw
	}
	mf.Brand = BrandOf(name + " " + mf.File)
}

var (
	sizeWord    = regexp.MustCompile(`(?i)^(\d+x)?\d+(\.\d+)?[bmk]$`)
	versionWord = regexp.MustCompile(`^v\d`)
	hasUpper    = regexp.MustCompile(`[A-Z]`)
	// spellings of words that are written in one case in a file name
	nameWords = map[string]string{"it": "IT", "tinyllama": "TinyLlama", "deepseek": "DeepSeek", "glm": "GLM", "moe": "MoE", "ai": "AI",
		"qwq": "QwQ", "gpt": "GPT", "llama": "Llama", "gguf": "GGUF"}
	publisherNames = map[string]string{"mistralai": "Mistral AI", "moonshotai": "Moonshot AI", "qwen": "Qwen", "thudm": "THUDM", "tinyllama": "TinyLlama",
		"google": "Google", "meta": "Meta", "meta-llama": "Meta", "deepseek-ai": "DeepSeek", "deepseek": "DeepSeek", "microsoft": "Microsoft",
		"zai-org": "Z.ai", "openai": "OpenAI", "nvidia": "NVIDIA", "ibm-granite": "IBM"}
)

// TidyModelName is the one rule for a model's display name, whatever wrote
// it (GGUF general.name, or the file name): a leading "<publisher>_" is taken
// off and returned as the publisher, dashes and underscores become spaces, a
// word written all in lower case gets a capital ("mixtral" -> "Mixtral",
// "8x7b" -> "8x7B", "it" -> "IT"), version words stay ("v0.1"), and a word
// that already has a capital is left as written ("DeepSeek", "A3B"). The
// publisher is spelled the usual way when it is a known one.
func TidyModelName(raw, publisher string) (name, pub string) {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "_"); i > 0 && !strings.ContainsAny(s[:i], "-. ") && i+1 < len(s) {
		if publisher == "" {
			publisher = s[:i]
		}
		s = s[i+1:]
	}
	words := strings.FieldsFunc(s, func(r rune) bool { return r == '-' || r == '_' || r == ' ' })
	for i, w := range words {
		lw := strings.ToLower(w)
		switch {
		case sizeWord.MatchString(w):
			words[i] = strings.ToUpper(w[:len(w)-1]) + strings.ToUpper(w[len(w)-1:])
			words[i] = strings.Replace(words[i], "X", "x", 1)
		case nameWords[lw] != "" && (w == lw || lw == "it"):
			words[i] = nameWords[lw]
		case versionWord.MatchString(w) || hasUpper.MatchString(w):
		case w != "":
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	name = strings.Join(words, " ")
	if name == "" {
		name = strings.TrimSpace(raw)
	}
	pub = strings.TrimSpace(publisher)
	if p, ok := publisherNames[strings.ToLower(pub)]; ok {
		pub = p
	}
	return name, pub
}

// brands are the model families the UI draws an avatar for, most specific
// first (Kimi Dev is built on Qwen and TinyLlama on Llama: their own name wins).
var brands = []struct{ brand, words string }{
	{"tinyllama", "tinyllama"}, {"deepseek", "deepseek"}, {"kimi", "kimi|moonshot"}, {"mistral", "mistral|mixtral|devstral|codestral|magistral|ministral|pixtral"},
	{"gemma", "gemma"}, {"qwen", "qwen|qwq"}, {"glm", "glm|chatglm"}, {"llama", "llama"}, {"phi", `\bphi\b`}, {"granite", "granite"},
}

var brandRes = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(brands))
	for i, b := range brands {
		out[i] = regexp.MustCompile(`(?i)` + b.words)
	}
	return out
}()

// BrandOf names the family of a model from its name and file name: gemma,
// qwen, llama, mistral, deepseek, glm, kimi, tinyllama, ... or "other".
func BrandOf(text string) string {
	for i, re := range brandRes {
		if re.MatchString(text) {
			return brands[i].brand
		}
	}
	return "other"
}

// metaReads counts the GGUF headers parsed (a test checks that a warm catalog reads none).
var metaReads atomic.Int64

// ReadModelFile stats one file in dir and reads its GGUF header.
func ReadModelFile(dir, file string) ModelFile {
	mf := ModelFileFromName(file)
	st, err := os.Stat(filepath.Join(dir, file))
	if err != nil || st.IsDir() {
		return mf
	}
	mf.Exists, mf.SizeBytes = true, st.Size()
	metaReads.Add(1)
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
	pub := mf.Publisher // from the file name, unless the metadata names one
	if n := str("general.organization"); n != "" {
		pub = n
	}
	if n := str("general.name"); n != "" {
		mf.setName(n, pub)
	} else if pub != mf.Publisher {
		_, mf.Publisher = TidyModelName("", pub)
	}
	if n := str("general.size_label"); n != "" {
		mf.Parameters = n
	}
	return mf
}

// fileMetaStore remembers file metadata across restarts (the database).
type fileMetaStore interface {
	FileMeta(path string, size, mtime int64, into any) bool
	PutFileMeta(path string, size, mtime int64, meta any) error
}

// metaFormat is bumped when the way a ModelFile is derived changes, so stored
// metadata of an older build is read again.
const metaFormat = 2

type storedMeta struct {
	Format int       `json:"format"`
	File   ModelFile `json:"file"`
}

// modelFileCache remembers the metadata of the files in the models directory,
// keyed by name, size and modification time, so a header is parsed once.
type modelFileCache struct {
	mu    sync.Mutex
	seen  map[string]cachedFile
	store fileMetaStore // optional: the same, kept in the database
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
		full := filepath.Join(dir, n)
		var sm storedMeta
		if c.store != nil && c.store.FileMeta(full, st.Size(), st.ModTime().UnixNano(), &sm) && sm.Format == metaFormat {
			c.seen[n] = cachedFile{size: st.Size(), mod: st.ModTime(), mf: sm.File}
			out = append(out, sm.File)
			continue
		}
		mf := ReadModelFile(dir, n)
		c.seen[n] = cachedFile{size: st.Size(), mod: st.ModTime(), mf: mf}
		if c.store != nil {
			_ = c.store.PutFileMeta(full, st.Size(), st.ModTime().UnixNano(), storedMeta{Format: metaFormat, File: mf})
		}
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
	// Icons is storage key -> etag of the icons the owner set ("model:<key>",
	// "family:<brand>"); Now is the clock for the activity buckets (default time.Now).
	Icons map[string]string
	Now   time.Time
}

// recentLoads is how many marks loads.recent carries (the Models list draws them).
const recentLoads = 12

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
	namer := modelname.NewNamer(catalogFiles(in))
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
			a.file = ModelFile{Name: id, MetaSource: "profile", Brand: "command"}
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
		// A node in no_auto_nodes (auto false) takes no automatic load.
		if !jbool(nm["excluded"]) && jbool(nm["eligible"]) && nm["auto"] != false {
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
		recent []map[string]any // newest first, at most recentLoads
		series []map[string]any // detail only: every load, newest first
		ended  map[string]map[string]any
		hours  map[int64]*bucket // detail only
		days   map[int64]*bucket
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	const hourMS, dayMS = int64(3600e3), int64(86400e3)
	hour0 := now.UnixMilli()/hourMS*hourMS - 23*hourMS
	day0 := now.UnixMilli()/dayMS*dayMS - 6*dayMS
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
		var hb, db *bucket
		if detailKey == key {
			if a.hours == nil {
				a.hours, a.days, a.ended = map[int64]*bucket{}, map[int64]*bucket{}, map[string]map[string]any{}
			}
			if h := e.Ts / hourMS * hourMS; h >= hour0 {
				if a.hours[h] == nil {
					a.hours[h] = &bucket{}
				}
				hb = a.hours[h]
			}
			if d := e.Ts / dayMS * dayMS; d >= day0 {
				if a.days[d] == nil {
					a.days[d] = &bucket{}
				}
				db = a.days[d]
			}
		}
		count := func(f func(*bucket)) {
			if hb != nil {
				f(hb)
			}
			if db != nil {
				f(db)
			}
		}
		switch e.Type {
		case history.TLoadFinished:
			a.count++
			outcome := jstr(e.Data["outcome"])
			mark := map[string]any{"at": e.Ts, "profile": e.Profile, "nodes": len(e.Nodes), "outcome": outcome, "load_s": e.Data["load_s"], "mb_s": e.Data["avg_mb_s"]}
			if len(a.recent) < recentLoads {
				a.recent = append(a.recent, mark)
			}
			count(func(b *bucket) {
				b.loads++
				if outcome != "ready" {
					b.failed++
				}
			})
			if detailKey == key && len(a.series) < 300 {
				row := map[string]any{"at": e.Ts, "profile": e.Profile, "nodes": len(e.Nodes), "outcome": outcome, "load_s": e.Data["load_s"], "mb_s": e.Data["avg_mb_s"]}
				mem := 0.0
				for _, d := range jlist(e.Data["nodes_detail"]) {
					mem += jnum(jmap(d)["memory_mib"])
				}
				if mem > 0 {
					row["memory_mib"] = math.Round(mem)
				}
				if c := profileCtx[e.Profile]; c > 0 {
					row["ctx"] = c
				}
				if r := jstr(e.Data["bench_run"]); r != "" {
					row["bench_run"] = r
				}
				// the unload of this load came later, so it was seen first (newest first)
				if u := a.ended[e.SessionID]; u != nil && e.SessionID != "" {
					row["stayed_s"], row["requests"], row["ended"] = u["loaded_for_s"], u["requests_served"], u["reason"]
				}
				a.series = append(a.series, row)
			}
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
			count(func(b *bucket) { b.requests += int(jnum(e.Data["requests_served"])) })
			if a.ended != nil && e.SessionID != "" {
				a.ended[e.SessionID] = e.Data
			}
			if e.Ts > a.lastActive {
				a.lastActive = e.Ts
			}
		case history.TRequestRefused:
			a.refusals++
			count(func(b *bucket) { b.refusals++ })
			// refusals come in bursts (a client that retries): one mark per run of them
			if n := len(a.recent); n > 0 && a.recent[n-1]["outcome"] == "refused" && a.recent[n-1]["profile"] == e.Profile {
				a.recent[n-1]["count"] = a.recent[n-1]["count"].(int) + 1
			} else if n < recentLoads {
				a.recent = append(a.recent, map[string]any{"at": e.Ts, "profile": e.Profile, "nodes": 0, "outcome": "refused", "count": 1, "reason": e.Data["reason"]})
			}
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
			"pass_rate": gm["pass_rate"], "outcomes": gm["outcomes"], "last_at": gm["last_at"], "last_run_id": gm["last_run_id"], "recent": gm["recent"]}
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
		brand := a.file.Brand
		if brand == "" {
			brand = BrandOf(a.file.Name + " " + a.file.File)
		}
		e := map[string]any{"key": key, "kind": a.kind, "file": a.file.File, "exists": a.file.Exists, "size_bytes": a.file.SizeBytes,
			"name": a.file.Name, "meta_source": a.file.MetaSource, "brand": brand, "icon": nil}
		if a.kind == "command" {
			e["exists"] = true
		}
		if a.kind != "command" {
			if sn := namer.Name(a.file.File); sn != "" {
				e["short_name"] = sn // the page's model name ("gemma-4-27b"); modelname.Namer, as the request rows
			}
		}
		if a.file.RawName != "" {
			e["raw_name"] = a.file.RawName
		}
		// the owner's icon: this model's own, else the one set for its family
		if tag := in.Icons[IconModelKey(key)]; tag != "" {
			e["icon"] = map[string]any{"etag": tag, "scope": "model"}
		} else if tag := in.Icons[IconFamilyKey(brand)]; tag != "" && a.kind == "llama" {
			e["icon"] = map[string]any{"etag": tag, "scope": "family"}
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
			for _, k := range []string{"host", "host_node", "host_overhead_mib", "host_overhead_source", "host_overhead_measured_at", "host_problem", "description", "derived_from_ctx", "derived_slope_mib_per_1k_ctx", "derived_slope_from_ctx"} { // a profile hosted on a node; a derived need
				if v, ok := p[k]; ok {
					pe[k] = v
				}
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
				// what one, two, three ... machines of the empty cluster give together
				cum, sums := 0.0, make([]float64, 0, len(budgets))
				for _, b := range budgets {
					cum += b
					sums = append(sums, cum)
				}
				fit["budgets_mib"] = sums
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
			"refusals": l.refusals, "last": l.last, "by_nodes": by, "recent": nonNilMaps(l.recent)}
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
			hours, days := make([]map[string]any, 0, 24), make([]map[string]any, 0, 7)
			for t := hour0; t < hour0+24*hourMS; t += hourMS {
				hours = append(hours, l.hours[t].row(t))
			}
			for t := day0; t < day0+7*dayMS; t += dayMS {
				days = append(days, l.days[t].row(t))
			}
			// oldest first: a chart reads left to right
			series := make([]map[string]any, 0, len(l.series))
			for i := len(l.series) - 1; i >= 0; i-- {
				series = append(series, l.series[i])
			}
			e["charts"] = map[string]any{"loads": series, "activity": map[string]any{"hours": hours, "days": days}}
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

// bucket counts what happened to one model in one hour or day.
type bucket struct{ loads, failed, refusals, requests int }

func (b *bucket) row(at int64) map[string]any {
	if b == nil {
		b = &bucket{}
	}
	return map[string]any{"at": at, "loads": b.loads, "failed": b.failed, "refusals": b.refusals, "requests": b.requests}
}

func nonNilMaps(v []map[string]any) []map[string]any {
	if v == nil {
		return []map[string]any{}
	}
	return v
}

// IconModelKey and IconFamilyKey are the storage keys of an owner-set icon.
func IconModelKey(key string) string    { return "model:" + key }
func IconFamilyKey(brand string) string { return "family:" + brand }

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

// catalogFiles is every model file of the catalog of in: the files the
// llama profiles name and the files in the models directory. The short
// names are chosen among them (modelname.Namer).
func catalogFiles(in CatalogInput) []string {
	var out []string
	for _, p := range in.Profiles {
		if m := jstr(p["model"]); m != "" && jstr(p["kind"]) != "command" {
			out = append(out, m)
		}
	}
	for _, f := range in.Files {
		out = append(out, f.File)
	}
	return out
}
