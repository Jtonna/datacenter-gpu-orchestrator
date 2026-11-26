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
	"strings"
	"time"
)

const crashUsage = `potluck-gate crash: the readiness and crash test of one build.

It copies --data into a work folder and starts the scheduler on the copy.
Then it runs --cycles cycles. A cycle is one configuration write followed by
the reads:
  write  a gate profile (potluck-gate-crash-0..2, on the model of the data's
         first profile or --model): created, then changed (ctx 4096 -> 2048),
         then deleted, in turn
  reads  GET /api/config/profiles, /api/profiles, /api/models, /api/needs
On --kills cycles chosen at random (--seed), the write is sent and the
scheduler is killed (SIGKILL; docker kill for an image) at a random moment
within --kill-window of it, then started again on the same copy.

It asserts:
  - every read answers 200 and parses (a bad read otherwise)
  - the stored profiles equal the acknowledged writes: an acknowledged write
    is there whole; the write in flight at a kill is there whole or absent;
    no other profile changed (a bad write otherwise)
  - the configuration revision never goes back, and every acknowledged write
    has a revision above the one before it
  - the three listings agree: /api/profiles, /api/config/profiles and the
    profiles of the model catalog (/api/models) hold the same number
  - no panic line and no ERROR line in the scheduler's log (--allow-log
    exempts a line the caller expects, say a copy without a program release)
  - after the last stop, every database of the copy passes SQLite's
    integrity check

Output: one line (cycles, writes, reads, kills, listings by kind, bad reads,
bad writes, panics, errors, PASS or FAIL) and a JSON file (--out). Exit 0 on
PASS, 1 on FAIL, 2 when the test could not run.

flags:
`

type crashOpts struct {
	spec                         schedSpec
	data, models, programs, work string
	history                      string
	out, model, listen           string
	cycles, kills                int
	seed                         int64
	window, ready                time.Duration
	allow                        []*regexp.Regexp
	args, env                    []string
	dryRun                       bool
}

func crashMain(argv []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("crash", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o crashOpts
	var spec string
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
	fs.IntVar(&o.cycles, "cycles", 200, "write-and-read cycles")
	fs.IntVar(&o.kills, "kills", 8, "cycles whose write is cut by a kill and a restart")
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
	o.args, o.env = args, env
	if o.data == "" || o.models == "" {
		fmt.Fprintln(stderr, "--data and --models are required")
		return 2
	}
	if o.cycles < 1 || o.kills < 0 || o.kills >= o.cycles {
		fmt.Fprintln(stderr, "--cycles must be at least 1 and --kills fewer than --cycles")
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
	Result       string         `json:"result"` // PASS or FAIL
	Scheduler    string         `json:"scheduler"`
	Seed         int64          `json:"seed"`
	Cycles       int            `json:"cycles"`
	Writes       int            `json:"writes"`
	Reads        int            `json:"reads"`
	Kills        int            `json:"kills"`
	KillAcked    int            `json:"kills_after_ack"` // the write was acknowledged before the kill
	Listings     map[string]int `json:"listings"`        // the last counts, by kind
	BadReads     int            `json:"bad_reads"`
	BadWrites    int            `json:"bad_writes"`
	ListingWaits int            `json:"listing_waits"` // listings that agreed only on a second look
	LastRevision int            `json:"last_revision"`
	Log          logScan        `json:"log"`
	Databases    []dbCheck      `json:"databases"`          // after the last stop: integrity, need tables, ledgers
	Problems     []string       `json:"problems,omitempty"` // the first 50
	DurationS    float64        `json:"duration_s"`
	Work         string         `json:"work"`
}

func (r *crashResult) line() string {
	kinds := make([]string, 0, len(r.Listings))
	for k := range r.Listings {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	var ls []string
	for _, k := range kinds {
		ls = append(ls, fmt.Sprintf("%s=%d", k, r.Listings[k]))
	}
	integrity := "ok"
	if !dbsOK(r.Databases) {
		integrity = "BAD"
	}
	return fmt.Sprintf("crash: %s cycles=%d writes=%d reads=%d kills=%d listings[%s] bad_reads=%d bad_writes=%d panics=%d errors=%d integrity=%s seed=%d",
		r.Result, r.Cycles, r.Writes, r.Reads, r.Kills, strings.Join(ls, " "), r.BadReads, r.BadWrites, r.Log.Panics, r.Log.Errors, integrity, r.Seed)
}

func (r *crashResult) problem(f string, a ...any) {
	if len(r.Problems) < 50 {
		r.Problems = append(r.Problems, fmt.Sprintf(f, a...))
	}
}

// gate profiles: the only profiles the test writes.
const gateSlots = 3

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

// nextOp is the write a gate profile is due: create, change, delete.
func nextOp(id, model string, cur string) op {
	spec := func(ctx int) map[string]any {
		return map[string]any{"model": model, "ctx": ctx, "needs_mib": 1000, "description": "potluck-gate crash test"}
	}
	switch {
	case cur == "":
		body := spec(4096)
		after := canonOf(body)
		body["id"] = id
		return op{id: id, method: http.MethodPost, path: "/api/config/profiles", body: body, before: cur, after: after}
	case cur == canonOf(spec(4096)):
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

// runCrash runs the test. A nil result with a nil error is a dry run.
func runCrash(ctx context.Context, o crashOpts, stdout io.Writer) (*crashResult, error) {
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
		fmt.Fprintf(stdout, "dry run: %d cycles, %d kills within %s of a write, seed %d, result %s\n", o.cycles, o.kills, o.window, o.seed, o.out)
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
	logs := &logBuf{f: lf}
	ro.Log = logs

	t0 := time.Now()
	res := &crashResult{Scheduler: o.spec.String(), Seed: o.seed, Work: work, Listings: map[string]int{}}
	rng := rand.New(rand.NewSource(o.seed))
	killAt := map[int]bool{}
	for _, i := range rng.Perm(o.cycles - 1)[:o.kills] {
		killAt[i+1] = true // never the first cycle
	}

	sched, err := startScheduler(o.spec, ro)
	if err != nil {
		return nil, err
	}
	defer func() { sched.stop(15 * time.Second) }()
	if err := sched.waitReady(o.ready); err != nil {
		return nil, fmt.Errorf("%w\n%s", err, tail(logs.String(), 2000))
	}
	c := newClient(sched.base)
	expected, lastRev, err := c.stored(ctx)
	if err != nil {
		return nil, err
	}
	model := o.model
	if model == "" {
		ids := make([]string, 0, len(expected))
		for id := range expected {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			var p struct {
				Model string `json:"model"`
			}
			if json.Unmarshal([]byte(expected[id]), &p) == nil && p.Model != "" {
				model = p.Model
				break
			}
		}
		if model == "" {
			return nil, errors.New("the data has no profile with a model file; give --model")
		}
	}

	for cyc := 0; cyc < o.cycles; cyc++ {
		res.Cycles++
		w := nextOp(gateID(cyc%gateSlots), model, expected[gateID(cyc%gateSlots)])
		res.Writes++
		if killAt[cyc] {
			ch := make(chan writeAns, 1)
			go func() { ch <- c.write(ctx, w) }()
			time.Sleep(time.Duration(rng.Int63n(int64(o.window) + 1)))
			if err := sched.kill(); err != nil {
				return nil, err
			}
			ans := <-ch
			res.Kills++
			acked := ans.err == nil
			if acked {
				res.KillAcked++
				if ans.rev <= lastRev {
					res.BadWrites++
					res.problem("cycle %d: %s %s acknowledged with revision %d, not above %d", cyc, w.method, w.path, ans.rev, lastRev)
				}
				lastRev = max(lastRev, ans.rev)
			}
			if sched, err = startScheduler(o.spec, ro); err != nil {
				return nil, err
			}
			if err := sched.waitReady(o.ready); err != nil {
				return nil, fmt.Errorf("restart after the kill of cycle %d: %w\n%s", cyc, err, tail(logs.String(), 2000))
			}
			c = newClient(sched.base)
			res.Reads++
			got, rev, err := c.stored(ctx)
			if err != nil {
				res.BadReads++
				res.problem("cycle %d: after the restart: %v", cyc, err)
				continue
			}
			if rev < lastRev {
				res.BadWrites++
				res.problem("cycle %d: after the restart the revision is %d, below the acknowledged %d", cyc, rev, lastRev)
			}
			lastRev = max(lastRev, rev)
			switch g := got[w.id]; {
			case acked && g != w.after:
				res.BadWrites++
				res.problem("cycle %d: the write acknowledged before the kill is not stored whole: %s %s: stored %q, want %q", cyc, w.method, w.id, g, w.after)
			case !acked && g != w.before && g != w.after:
				res.BadWrites++
				res.problem("cycle %d: the write cut by the kill is half there: %s %s: stored %q, want %q or %q", cyc, w.method, w.id, g, w.before, w.after)
			}
			if got[w.id] == "" {
				delete(expected, w.id)
			} else {
				expected[w.id] = got[w.id]
			}
			res.BadWrites += compareStored(res, cyc, expected, got, w.id)
			continue
		}
		ans := c.write(ctx, w)
		if ans.err != nil {
			res.BadWrites++
			res.problem("cycle %d: %s %s: %v", cyc, w.method, w.path, ans.err)
		} else {
			if ans.rev <= lastRev {
				res.BadWrites++
				res.problem("cycle %d: %s %s acknowledged with revision %d, not above %d", cyc, w.method, w.path, ans.rev, lastRev)
			}
			lastRev = max(lastRev, ans.rev)
			if w.after == "" {
				delete(expected, w.id)
			} else {
				expected[w.id] = w.after
			}
		}
		res.Reads++
		got, rev, err := c.stored(ctx)
		if err != nil {
			res.BadReads++
			res.problem("cycle %d: %v", cyc, err)
		} else {
			if rev < lastRev {
				res.BadReads++
				res.problem("cycle %d: GET /api/config/profiles revision %d, below the acknowledged %d", cyc, rev, lastRev)
			}
			res.BadWrites += compareStored(res, cyc, expected, got, "")
			for id, v := range got { // keep a write that failed, if it is stored whole
				if id == w.id && (v == w.before || v == w.after) {
					expected[id] = v
				}
			}
			if _, ok := got[w.id]; !ok && ans.err != nil {
				delete(expected, w.id)
			}
		}
		res.Reads += 4
		n, err := c.listings(ctx)
		if err == nil && !agree(n) {
			// the catalog may be built a moment after the write: one more look
			time.Sleep(time.Second)
			res.ListingWaits++
			res.Reads += 4
			n, err = c.listings(ctx)
		}
		if err != nil {
			res.BadReads++
			res.problem("cycle %d: %v", cyc, err)
			continue
		}
		res.Listings = n
		if !agree(n) {
			res.BadReads++
			res.problem("cycle %d: the listings disagree: /api/profiles %d, /api/config/profiles %d, catalog %d", cyc, n["profiles"], n["config_profiles"], n["catalog_profiles"])
		}
	}
	res.LastRevision = lastRev
	sched.stop(15 * time.Second)
	res.Log = scanLog(logs.String(), o.allow)
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
