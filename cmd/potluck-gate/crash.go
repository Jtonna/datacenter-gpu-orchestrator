package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const crashUsage = `potluck-gate crash: the readiness and crash test of one build.

It copies --data into a work folder and starts the scheduler on the copy.
Then it runs cycles of configuration writes, --cycles of them or as many as
--duration allows, while reader goroutines (--readers) read listings without
a pause. A cycle takes one gate profile (potluck-gate-crash-0..N, --slots;
on the model of the data's first profile or --model) from absent back to
absent:
  POST   create it (ctx 4096)
  PUT    change it (ctx 2048)       (left out with --change=false)
  DELETE delete it
With --verify write, every write is followed by a read of the stored
profiles; with --verify cycle, one read ends the cycle. After every cycle
the listings are counted.

Kills: on --kills cycles (chosen at random with --cycles, spread evenly in
time with --duration) one write of the cycle is sent and the scheduler is
killed (SIGKILL; docker kill for an image) at a random moment within
--kill-window of it, then started again on the same copy; the readers wait
while it is down, and the cycle goes on from what was stored.

It asserts:
  - every read answers 200 and parses, the readers' included (a bad read)
  - every write answers 2xx with a revision above the one before it, and
    the stored profiles equal the acknowledged writes: an acknowledged write
    is there whole; the write in flight at a kill is there whole or absent;
    no other profile changed (a bad write otherwise)
  - the configuration revision never goes back
  - the listings agree: /api/profiles, /api/config/profiles and the
    profiles of the model catalog (/api/models) hold the same number
  - no panic line and no ERROR line in the scheduler's log (--allow-log
    exempts a line the caller expects, say a copy without a program release)
  - after the last stop, every database of the copy passes SQLite's
    integrity check

Output: one line and a JSON file (--out). The line gives the cycles, the
writes, the kills, the readers' reads by kind (listings{list, models,
needs}), the bad reads, the bad writes, the listing counts, the panic and
ERROR lines and the integrity. Exit 0 on PASS, 1 on FAIL, 2 when the test
could not run.

The old readiness test's figures: it ran 40 s of POST-and-DELETE pairs on
nine profiles beside the same readers, with no kill and no read-back:
  potluck-gate crash --duration 40s --change=false --kills 0 --verify cycle
runs the same cycles and reads (a cycle is one pair) and is comparable.

flags:
`

type crashOpts struct {
	spec                         schedSpec
	data, models, programs, work string
	history                      string
	out, model, listen           string
	cycles, kills, slots         int
	duration                     time.Duration
	change                       bool
	verify                       string // write or cycle
	readers                      map[string]int
	seed                         int64
	window, ready                time.Duration
	allow                        []*regexp.Regexp
	args, env                    []string
	dryRun                       bool
}

// readerPaths: what each kind of reader reads.
var readerPaths = map[string]string{"list": "/api/profiles", "models": "/v1/models", "needs": "/api/needs", "catalog": "/api/models", "config": "/api/config/profiles"}

// parseReaders reads "list=3,models=1,needs=1".
func parseReaders(s string) (map[string]int, error) {
	out := map[string]int{}
	if strings.TrimSpace(s) == "" || s == "none" {
		return out, nil
	}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		n, err := strconv.Atoi(v)
		if _, known := readerPaths[k]; !ok || !known || err != nil || n < 0 {
			return nil, fmt.Errorf("%q: want kind=count, kinds list, models, needs, catalog, config", part)
		}
		out[k] = n
	}
	return out, nil
}

func crashMain(argv []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("crash", flag.ContinueOnError)
	fs.SetOutput(stderr)
	o := crashOpts{change: true}
	var spec, readers string
	var allow, args, env multiFlag
	fs.StringVar(&spec, "scheduler", "", "the build under test: a binary, or image:NAME (docker run --network host)")
	fs.StringVar(&o.data, "data", "", "the data folder to copy; never written (its databases are copied with SQLite, safe while a scheduler runs on it)")
	fs.StringVar(&o.models, "models", "", "the models folder (mounted read-only for an image)")
	fs.StringVar(&o.programs, "programs", "", "the program release folder (--programs-dir), optional")
	fs.StringVar(&o.history, "history", "", "the event history folder to copy (--history-dir), optional; never written")
	fs.StringVar(&o.work, "work", "", "the work folder for the copy, the log and the result (default: a new temporary folder)")
	fs.StringVar(&o.out, "out", "", "the JSON result (default: <work>/crash.json)")
	fs.StringVar(&o.model, "model", "", "the model file of the gate profiles (default: the model of the data's first profile)")
	fs.StringVar(&o.listen, "listen", "", "the scheduler's address (default: a free port on 127.0.0.1)")
	fs.IntVar(&o.cycles, "cycles", 200, "cycles to run (unless --duration is given)")
	fs.DurationVar(&o.duration, "duration", 0, "run cycles for this long instead of --cycles (the old test: 40s)")
	fs.IntVar(&o.kills, "kills", 8, "cycles with a write cut by a kill and a restart")
	fs.IntVar(&o.slots, "slots", 9, "gate profiles written in turn")
	fs.BoolVar(&o.change, "change", true, "a cycle changes the profile between its create and its delete")
	fs.StringVar(&o.verify, "verify", "write", "read the stored profiles after every write, or once per cycle (write, cycle)")
	fs.StringVar(&readers, "readers", "list=3,models=1,needs=1", "concurrent readers by kind: list (/api/profiles), models (/v1/models), needs (/api/needs), catalog (/api/models), config (/api/config/profiles); none for no reader")
	fs.Int64Var(&o.seed, "seed", 0, "seed of the kill cycles and moments (default: the time)")
	fs.DurationVar(&o.window, "kill-window", 400*time.Millisecond, "the kill lands at a random moment this long after the write is sent")
	fs.DurationVar(&o.ready, "ready", 3*time.Minute, "how long a start may take until /health answers")
	fs.Var(&allow, "allow-log", "a regular expression: a log line that matches it is not counted (repeatable)")
	fs.Var(&args, "arg", "an extra scheduler flag (repeatable)")
	fs.Var(&env, "env", "an extra environment variable K=V for the scheduler (repeatable)")
	fs.BoolVar(&o.dryRun, "dry-run", false, "check the inputs and print what would run; start nothing")
	fs.Usage = func() { fmt.Fprint(fs.Output(), crashUsage); fs.PrintDefaults() }
	if err := fs.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(stdout)
			fs.Usage()
			return 0
		}
		return 2
	}
	var err error
	if o.spec, err = parseSpec(spec); err != nil {
		fmt.Fprintln(stderr, "--scheduler:", err)
		return 2
	}
	if o.allow, err = compileAll(allow); err != nil {
		fmt.Fprintln(stderr, "--allow-log:", err)
		return 2
	}
	if o.readers, err = parseReaders(readers); err != nil {
		fmt.Fprintln(stderr, "--readers:", err)
		return 2
	}
	o.args, o.env = args, env
	if o.data == "" || o.models == "" {
		fmt.Fprintln(stderr, "--data and --models are required")
		return 2
	}
	if o.duration < 0 || (o.duration == 0 && (o.cycles < 1 || o.kills >= o.cycles)) || o.kills < 0 || o.slots < 1 {
		fmt.Fprintln(stderr, "--cycles must be at least 1 and --kills fewer than --cycles (or give --duration); --slots at least 1")
		return 2
	}
	if o.verify != "write" && o.verify != "cycle" {
		fmt.Fprintln(stderr, "--verify: write or cycle")
		return 2
	}
	if err := o.spec.check(); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if o.seed == 0 {
		o.seed = time.Now().UnixNano()
	}
	res, err := runCrash(context.Background(), o, stdout)
	if err != nil {
		fmt.Fprintln(stderr, "crash test could not run:", err)
		return 2
	}
	if res == nil { // dry run
		return 0
	}
	fmt.Fprintln(stdout, res.line())
	if res.Result != "PASS" {
		return 1
	}
	return 0
}

// crashResult is the JSON result.
type crashResult struct {
	Result        string         `json:"result"` // PASS or FAIL
	Scheduler     string         `json:"scheduler"`
	Seed          int64          `json:"seed"`
	Cycles        int            `json:"cycles"` // gate profiles taken from absent back to absent
	Writes        int            `json:"writes"`
	Kills         int            `json:"kills"`
	KillAcked     int            `json:"kills_after_ack"` // the write was acknowledged before the kill
	Listings      map[string]int `json:"listings"`        // the readers' reads, by kind (the old test's "listings")
	Checks        int            `json:"checks"`          // the test's own reads (stored profiles and listing counts)
	BadReads      int            `json:"bad_reads"`
	BadWrites     int            `json:"bad_writes"`
	ListingCounts map[string]int `json:"listing_counts"` // the last counts of the listings, by kind
	ListingWaits  int            `json:"listing_waits"`  // listings that agreed only on a second look
	LastRevision  int            `json:"last_revision"`
	Log           logScan        `json:"log"`
	Databases     []dbCheck      `json:"databases"`          // after the last stop: integrity, need tables, ledgers
	Problems      []string       `json:"problems,omitempty"` // the first 50
	DurationS     float64        `json:"duration_s"`
	Work          string         `json:"work"`

	mu sync.Mutex
}

func sortedKV(m map[string]int) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var out []string
	for _, k := range ks {
		out = append(out, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(out, " ")
}

func (r *crashResult) line() string {
	integrity := "ok"
	if !dbsOK(r.Databases) {
		integrity = "BAD"
	}
	return fmt.Sprintf("crash: %s cycles=%d writes=%d kills=%d listings{%s} bad_reads=%d bad_writes=%d counts[%s] panics=%d errors=%d integrity=%s seed=%d",
		r.Result, r.Cycles, r.Writes, r.Kills, sortedKV(r.Listings), r.BadReads, r.BadWrites, sortedKV(r.ListingCounts), r.Log.Panics, r.Log.Errors, integrity, r.Seed)
}

func (r *crashResult) problem(f string, a ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.Problems) < 50 {
		r.Problems = append(r.Problems, fmt.Sprintf(f, a...))
	}
}

func (r *crashResult) badRead(f string, a ...any) {
	r.mu.Lock()
	r.BadReads++
	r.mu.Unlock()
	r.problem(f, a...)
}

func gateID(i int) string { return fmt.Sprintf("potluck-gate-crash-%d", i) }

// canon is a JSON value in one spelling (keys sorted), for comparison.
func canon(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "unparsable:" + string(raw)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func canonOf(v any) string {
	b, _ := json.Marshal(v)
	return canon(b)
}

// op is one configuration write of a gate profile.
type op struct {
	id     string
	method string
	path   string
	body   any
	before string // canon of the stored spec before ("" absent)
	after  string // canon after
}

// nextOp is the write a gate profile is due: create; change (when change);
// delete.
func nextOp(id, model string, cur string, change bool) op {
	spec := func(ctx int) map[string]any {
		return map[string]any{"model": model, "ctx": ctx, "needs_mib": 1000, "description": "potluck-gate crash test"}
	}
	switch {
	case cur == "":
		body := spec(4096)
		after := canonOf(body)
		body["id"] = id
		return op{id: id, method: http.MethodPost, path: "/api/config/profiles", body: body, before: cur, after: after}
	case change && cur == canonOf(spec(4096)):
		return op{id: id, method: http.MethodPut, path: "/api/config/profiles/" + url.PathEscape(id), body: spec(2048), before: cur, after: canonOf(spec(2048))}
	default:
		return op{id: id, method: http.MethodDelete, path: "/api/config/profiles/" + url.PathEscape(id), before: cur, after: ""}
	}
}

type writeAns struct {
	status int
	rev    int
	err    error
}

func (c *client) write(ctx context.Context, w op) writeAns {
	var ans struct {
		Revision int `json:"revision"`
	}
	st, raw, err := c.call(ctx, w.method, w.path, w.body, &ans)
	if err == nil && (st < 200 || st > 299) {
		err = fmt.Errorf("HTTP %d: %s", st, truncate(string(raw), 300))
	}
	return writeAns{status: st, rev: ans.Revision, err: err}
}

// stored reads GET /api/config/profiles: every profile's canon spec and the
// revision.
func (c *client) stored(ctx context.Context) (map[string]string, int, error) {
	var doc struct {
		Profiles map[string]json.RawMessage `json:"profiles"`
		Revision *int                       `json:"revision"`
	}
	st, _, err := c.call(ctx, http.MethodGet, "/api/config/profiles", nil, &doc)
	if err != nil {
		return nil, 0, err
	}
	if st != http.StatusOK || doc.Profiles == nil || doc.Revision == nil {
		return nil, 0, fmt.Errorf("GET /api/config/profiles: HTTP %d without profiles and revision", st)
	}
	out := make(map[string]string, len(doc.Profiles))
	for id, raw := range doc.Profiles {
		out[id] = canon(raw)
	}
	return out, *doc.Revision, nil
}

// listings reads the three listings and the needs and counts them.
func (c *client) listings(ctx context.Context) (map[string]int, error) {
	var prof struct {
		Profiles []struct {
			ID string `json:"id"`
		} `json:"profiles"`
	}
	var cat struct {
		Models []struct {
			Profiles []struct {
				ID string `json:"id"`
			} `json:"profiles"`
		} `json:"models"`
	}
	var cfg struct {
		Profiles map[string]json.RawMessage `json:"profiles"`
	}
	var needs struct {
		Needs []json.RawMessage `json:"needs"`
	}
	for _, r := range []struct {
		path string
		out  any
	}{{"/api/profiles", &prof}, {"/api/models", &cat}, {"/api/config/profiles", &cfg}, {"/api/needs", &needs}} {
		st, _, err := c.call(ctx, http.MethodGet, r.path, nil, r.out)
		if err != nil {
			return nil, err
		}
		if st != http.StatusOK {
			return nil, fmt.Errorf("GET %s: HTTP %d", r.path, st)
		}
	}
	n := map[string]int{"profiles": len(prof.Profiles), "config_profiles": len(cfg.Profiles), "models": len(cat.Models), "needs": len(needs.Needs)}
	for _, m := range cat.Models {
		n["catalog_profiles"] += len(m.Profiles)
	}
	return n, nil
}

func agree(n map[string]int) bool {
	return n["profiles"] == n["config_profiles"] && n["profiles"] == n["catalog_profiles"]
}

// crashRun is the state of one run of the test.
type crashRun struct {
	o        crashOpts
	ro       runOpts
	res      *crashResult
	rng      *rand.Rand
	sched    *running
	c        *client
	logs     *logBuf
	expected map[string]string
	lastRev  int
	model    string
	// down is held for writing while the scheduler is killed and restarted;
	// every reader read holds it for reading, so no read lands on a
	// scheduler the test killed itself.
	down    sync.RWMutex
	readsBy map[string]*atomic.Int64
}

// runCrash runs the test. A nil result with a nil error is a dry run.
func runCrash(ctx context.Context, o crashOpts, stdout io.Writer) (*crashResult, error) {
	if o.slots < 1 {
		o.slots = 9
	}
	if o.verify == "" {
		o.verify = "write"
	}
	work := o.work
	if work == "" && !o.dryRun {
		var err error
		if work, err = os.MkdirTemp("", "potluck-gate-crash-"); err != nil {
			return nil, err
		}
	}
	if o.out == "" {
		o.out = filepath.Join(work, "crash.json")
	}
	copyTo := filepath.Join(work, "data")
	listen := o.listen
	if listen == "" {
		var err error
		if listen, err = freeAddr("127.0.0.1"); err != nil {
			return nil, err
		}
	}
	ro := runOpts{Listen: listen, Data: copyTo, Models: o.models, Programs: o.programs, Args: o.args, Env: o.env}
	if o.history != "" {
		ro.History = filepath.Join(work, "history")
	}
	if o.dryRun {
		if _, err := os.Stat(o.data); err != nil {
			return nil, fmt.Errorf("data folder: %w", err)
		}
		if _, err := os.Stat(o.models); err != nil {
			return nil, fmt.Errorf("models folder: %w", err)
		}
		prog, args, _ := o.spec.argv(ro)
		fmt.Fprintf(stdout, "dry run: copy %s -> %s\n", o.data, copyTo)
		fmt.Fprintf(stdout, "dry run: start %s %s\n", prog, strings.Join(args, " "))
		length := fmt.Sprintf("%d cycles", o.cycles)
		if o.duration > 0 {
			length = "cycles for " + o.duration.String()
		}
		fmt.Fprintf(stdout, "dry run: %s on %d gate profiles (change %v, verify %s), readers %s, %d kills within %s of a write, seed %d, result %s\n",
			length, o.slots, o.change, o.verify, sortedKV(o.readers), o.kills, o.window, o.seed, o.out)
		return nil, nil
	}
	if err := copyDir(o.data, copyTo); err != nil {
		return nil, err
	}
	if o.history != "" {
		if err := copyDir(o.history, ro.History); err != nil {
			return nil, err
		}
	}
	lf, err := os.Create(filepath.Join(work, "scheduler.log"))
	if err != nil {
		return nil, err
	}
	defer lf.Close()
	r := &crashRun{o: o, ro: ro, logs: &logBuf{f: lf}, rng: rand.New(rand.NewSource(o.seed)), readsBy: map[string]*atomic.Int64{},
		res: &crashResult{Scheduler: o.spec.String(), Seed: o.seed, Work: work, Listings: map[string]int{}, ListingCounts: map[string]int{}}}
	r.ro.Log = r.logs
	t0 := time.Now()
	if err := r.start(); err != nil {
		return nil, err
	}
	defer func() { r.sched.stop(15 * time.Second) }()
	if err := r.setup(ctx); err != nil {
		return nil, err
	}
	if err := r.loop(ctx, t0); err != nil {
		return nil, err
	}
	res := r.res
	res.LastRevision = r.lastRev
	r.sched.stop(15 * time.Second)
	res.Log = scanLog(r.logs.String(), o.allow)
	res.Databases = checkDBs(copyTo)
	res.DurationS = time.Since(t0).Seconds()
	res.Result = "PASS"
	if res.BadReads+res.BadWrites+res.Log.Panics+res.Log.Errors > 0 || !dbsOK(res.Databases) {
		res.Result = "FAIL"
	}
	if err := writeJSONFile(o.out, res); err != nil {
		return nil, err
	}
	return res, nil
}

func (r *crashRun) start() error {
	s, err := startScheduler(r.o.spec, r.ro)
	if err != nil {
		return err
	}
	r.sched = s
	if err := s.waitReady(r.o.ready); err != nil {
		return fmt.Errorf("%w\n%s", err, tail(r.logs.String(), 2000))
	}
	r.c = newClient(s.base)
	return nil
}

// setup reads the stored profiles and picks the gate profiles' model.
func (r *crashRun) setup(ctx context.Context) error {
	var err error
	if r.expected, r.lastRev, err = r.c.stored(ctx); err != nil {
		return err
	}
	r.model = r.o.model
	if r.model != "" {
		return nil
	}
	ids := make([]string, 0, len(r.expected))
	for id := range r.expected {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		var p struct {
			Model string `json:"model"`
		}
		if json.Unmarshal([]byte(r.expected[id]), &p) == nil && p.Model != "" {
			r.model = p.Model
			return nil
		}
	}
	return errors.New("the data has no profile with a model file; give --model")
}

// readers starts the concurrent readers; stop ends them.
func (r *crashRun) readers(ctx context.Context) (stop func()) {
	var stopped atomic.Bool
	var wg sync.WaitGroup
	hc := &http.Client{Timeout: 30 * time.Second, Transport: transport}
	base := r.c.base
	for kind, n := range r.o.readers {
		ctr := &atomic.Int64{}
		r.readsBy[kind] = ctr
		path := readerPaths[kind]
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for !stopped.Load() {
					r.down.RLock()
					st, body, err := getRaw(ctx, hc, base+path)
					r.down.RUnlock()
					ctr.Add(1)
					switch {
					case err != nil:
						r.res.badRead("reader %s: %v", path, err)
					case st != http.StatusOK:
						r.res.badRead("reader %s: HTTP %d", path, st)
					case !json.Valid(body):
						r.res.badRead("reader %s: the answer does not parse", path)
					}
				}
			}()
		}
	}
	return func() {
		stopped.Store(true)
		wg.Wait()
		for k, c := range r.readsBy {
			r.res.Listings[k] = int(c.Load())
		}
	}
}

func getRaw(ctx context.Context, hc *http.Client, u string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

// loop runs the cycles.
func (r *crashRun) loop(ctx context.Context, t0 time.Time) error {
	o, res := r.o, r.res
	killAt := map[int]bool{}
	if o.duration == 0 {
		for _, i := range r.rng.Perm(o.cycles - 1)[:o.kills] {
			killAt[i+1] = true // never the first cycle
		}
	}
	nextKill := 0 // with --duration: the kills done so far
	stop := r.readers(ctx)
	defer stop()
	for cyc := 0; ; cyc++ {
		if o.duration > 0 && time.Since(t0) >= o.duration {
			break
		}
		if o.duration == 0 && cyc >= o.cycles {
			break
		}
		kill := killAt[cyc]
		if o.duration > 0 && nextKill < o.kills && time.Since(t0) >= o.duration*time.Duration(nextKill+1)/time.Duration(o.kills+1) {
			kill, nextKill = true, nextKill+1
		}
		id := gateID(cyc % o.slots)
		// the writes of the cycle: from what is stored back to absent
		var steps int
		if r.expected[id] == "" {
			steps = 2
			if o.change {
				steps = 3
			}
		}
		killStep := -1
		if kill {
			killStep = r.rng.Intn(max(steps, 1))
		}
		for step := 0; ; step++ {
			cur := r.expected[id]
			if step > 0 && cur == "" {
				break
			}
			w := nextOp(id, r.model, cur, o.change)
			res.Writes++
			if step == killStep {
				if err := r.killDuring(ctx, cyc, w); err != nil {
					return err
				}
			} else {
				r.writeOne(ctx, cyc, w)
			}
			if step > 4 { // a write that keeps failing: go on with the next cycle
				break
			}
		}
		res.Cycles++
		if o.verify == "cycle" {
			r.checkStored(ctx, cyc, "")
		}
		r.checkListings(ctx, cyc)
	}
	return nil
}

// writeOne sends one write and, with --verify write, reads it back.
func (r *crashRun) writeOne(ctx context.Context, cyc int, w op) {
	res := r.res
	ans := r.c.write(ctx, w)
	if ans.err != nil {
		res.BadWrites++
		res.problem("cycle %d: %s %s: %v", cyc, w.method, w.path, ans.err)
	} else {
		if ans.rev <= r.lastRev {
			res.BadWrites++
			res.problem("cycle %d: %s %s acknowledged with revision %d, not above %d", cyc, w.method, w.path, ans.rev, r.lastRev)
		}
		r.lastRev = max(r.lastRev, ans.rev)
		if w.after == "" {
			delete(r.expected, w.id)
		} else {
			r.expected[w.id] = w.after
		}
	}
	if r.o.verify == "write" || ans.err != nil {
		got := r.checkStored(ctx, cyc, "")
		if ans.err != nil && got != nil { // a write that failed: go on from what is stored, if it is whole
			if v, ok := got[w.id]; ok && (v == w.before || v == w.after) {
				r.expected[w.id] = v
			} else if !ok {
				delete(r.expected, w.id)
			}
		}
	}
}

// checkStored compares the stored profiles with the acknowledged writes
// (skip: a profile judged apart) and returns them (nil on a bad read).
func (r *crashRun) checkStored(ctx context.Context, cyc int, skip string) map[string]string {
	res := r.res
	res.Checks++
	got, rev, err := r.c.stored(ctx)
	if err != nil {
		res.badRead("cycle %d: %v", cyc, err)
		return nil
	}
	if rev < r.lastRev {
		res.badRead("cycle %d: GET /api/config/profiles revision %d, below the acknowledged %d", cyc, rev, r.lastRev)
	}
	res.BadWrites += compareStored(res, cyc, r.expected, got, skip)
	return got
}

// killDuring sends w, kills the scheduler within the kill window, starts it
// again and judges what was stored of w.
func (r *crashRun) killDuring(ctx context.Context, cyc int, w op) error {
	res := r.res
	r.down.Lock()
	defer r.down.Unlock()
	ch := make(chan writeAns, 1)
	go func() { ch <- r.c.write(ctx, w) }()
	time.Sleep(time.Duration(r.rng.Int63n(int64(r.o.window) + 1)))
	if err := r.sched.kill(); err != nil {
		return err
	}
	ans := <-ch
	res.Kills++
	acked := ans.err == nil
	if acked {
		res.KillAcked++
		if ans.rev <= r.lastRev {
			res.BadWrites++
			res.problem("cycle %d: %s %s acknowledged with revision %d, not above %d", cyc, w.method, w.path, ans.rev, r.lastRev)
		}
		r.lastRev = max(r.lastRev, ans.rev)
	}
	if err := r.start(); err != nil {
		return fmt.Errorf("restart after the kill of cycle %d: %w", cyc, err)
	}
	res.Checks++
	got, rev, err := r.c.stored(ctx)
	if err != nil {
		res.badRead("cycle %d: after the restart: %v", cyc, err)
		return nil
	}
	if rev < r.lastRev {
		res.BadWrites++
		res.problem("cycle %d: after the restart the revision is %d, below the acknowledged %d", cyc, rev, r.lastRev)
	}
	r.lastRev = max(r.lastRev, rev)
	switch g := got[w.id]; {
	case acked && g != w.after:
		res.BadWrites++
		res.problem("cycle %d: the write acknowledged before the kill is not stored whole: %s %s: stored %q, want %q", cyc, w.method, w.id, g, w.after)
	case !acked && g != w.before && g != w.after:
		res.BadWrites++
		res.problem("cycle %d: the write cut by the kill is half there: %s %s: stored %q, want %q or %q", cyc, w.method, w.id, g, w.before, w.after)
	}
	if got[w.id] == "" {
		delete(r.expected, w.id)
	} else {
		r.expected[w.id] = got[w.id]
	}
	res.BadWrites += compareStored(res, cyc, r.expected, got, w.id)
	return nil
}

// checkListings counts the listings; they must agree (one second look is
// allowed: the catalog may be built a moment after a write).
func (r *crashRun) checkListings(ctx context.Context, cyc int) {
	res := r.res
	res.Checks += 4
	n, err := r.c.listings(ctx)
	if err == nil && !agree(n) {
		time.Sleep(time.Second)
		res.ListingWaits++
		res.Checks += 4
		n, err = r.c.listings(ctx)
	}
	if err != nil {
		res.badRead("cycle %d: %v", cyc, err)
		return
	}
	res.ListingCounts = n
	if !agree(n) {
		res.badRead("cycle %d: the listings disagree: /api/profiles %d, /api/config/profiles %d, catalog %d", cyc, n["profiles"], n["config_profiles"], n["catalog_profiles"])
	}
}

// compareStored counts the profiles whose stored spec differs from the
// expected one (skip: the profile already judged).
func compareStored(res *crashResult, cyc int, expected, got map[string]string, skip string) int {
	bad := 0
	for id, want := range expected {
		if id != skip && got[id] != want {
			bad++
			res.problem("cycle %d: profile %s stored %q, the acknowledged writes say %q", cyc, id, truncate(got[id], 200), truncate(want, 200))
		}
	}
	for id := range got {
		if _, ok := expected[id]; !ok && id != skip {
			bad++
			res.problem("cycle %d: profile %s is stored, no acknowledged write made it", cyc, id)
		}
	}
	return bad
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
