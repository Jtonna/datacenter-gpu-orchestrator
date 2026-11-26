package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/ports"
)

const plansUsage = `potluck-gate plans: every profile's plan under two builds, on the same data.

It copies --data into a work folder once and runs, one after the other, on
that one copy:
  1. OLD  -> dump old-1
  2. NEW  -> dump new-1     (NEW now has touched the data: a migration, say)
  3. OLD  -> dump old-2     (the way back: OLD again on the data NEW touched)
  4. NEW  -> dump new-2
For each run it starts the scheduler, starts the fake fleet when --nodes is
given (potluck-fake-agents --nodes FILE, its agents on --addr-base and the
scheduler's test agent ports), waits until every fake agent is active, and
reads GET /api/profiles until two reads in a row agree (--settle). A dump is,
per profile: the profile's need (needs_mib, needs_basis, needs_measured) and
every field of its plan, flattened: plan.action, plan.need_mib,
plan.memory.nodes[<node>].will_take_mib, .basis, .host_overhead_mib,
.host_overhead_source and every other field of each memory row. Fields whose
name matches --ignore are left out (default: timestamps).

It compares old-1 with new-1 (what the new build changes), old-1 with old-2
and new-1 with new-2 (the way back: both must be equal). For each comparison
it prints the number of profiles whose plan differs in any row and the list
(profile, field, old, new), and writes them all to <out>/plans.json.

The exit status is 0 whatever the differences, unless --fail-on names what
the caller expects to be equal: old-new (no difference between the builds)
and/or way-back. 2 when a run could not be made.

flags:
`

type plansOpts struct {
	old, new                     schedSpec
	data, models, programs, work string
	out, nodes, fakeAgents       string
	addrBase                     string
	settle, ready                time.Duration
	ignore                       *regexp.Regexp
	failOn                       map[string]bool
	args, env                    []string
	print                        int
	dryRun                       bool
}

func plansMain(argv []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("plans", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o plansOpts
	var oldS, newS, ignore, failOn string
	var args, env multiFlag
	fs.StringVar(&oldS, "old", "", "the build now live: a binary, or image:NAME")
	fs.StringVar(&newS, "new", "", "the candidate: a binary, or image:NAME")
	fs.StringVar(&o.data, "data", "", "the data folder to copy (from a stopped scheduler or a snapshot); never written")
	fs.StringVar(&o.models, "models", "", "the models folder")
	fs.StringVar(&o.programs, "programs", "", "the program release folder (--programs-dir); needed for agents")
	fs.StringVar(&o.nodes, "nodes", "", "the fake fleet: a potluck-fake-agents --nodes file (optional)")
	fs.StringVar(&o.fakeAgents, "fake-agents", "", "the potluck-fake-agents binary (with --nodes)")
	fs.StringVar(&o.addrBase, "addr-base", "127.0.0.2", "the first fake agent's address (potluck-fake-agents --addr-base)")
	fs.StringVar(&o.work, "work", "", "the work folder (default: a new temporary folder)")
	fs.StringVar(&o.out, "out", "", "the folder for the dumps and plans.json (default: <work>)")
	fs.DurationVar(&o.settle, "settle", 60*time.Second, "how long to wait for two equal reads of the plans in a row")
	fs.DurationVar(&o.ready, "ready", 3*time.Minute, "how long a start may take (until /health and all fake agents are active)")
	fs.StringVar(&ignore, "ignore", `(^|\.)[a-z_]*(_at|_at_ms)$`, "a regular expression: fields whose path matches are left out")
	fs.StringVar(&failOn, "fail-on", "", "exit 1 when these differ: old-new, way-back (comma separated)")
	fs.IntVar(&o.print, "print", 200, "differing rows printed per comparison (all are in plans.json)")
	fs.Var(&args, "arg", "an extra scheduler flag for both builds (repeatable)")
	fs.Var(&env, "env", "an extra environment variable K=V for both builds (repeatable)")
	fs.BoolVar(&o.dryRun, "dry-run", false, "check the inputs and print the steps; start nothing")
	fs.Usage = func() { fmt.Fprint(fs.Output(), plansUsage); fs.PrintDefaults() }
	if err := fs.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(stdout)
			fs.Usage()
			return 0
		}
		return 2
	}
	var err error
	if o.old, err = parseSpec(oldS); err != nil {
		fmt.Fprintln(stderr, "--old:", err)
		return 2
	}
	if o.new, err = parseSpec(newS); err != nil {
		fmt.Fprintln(stderr, "--new:", err)
		return 2
	}
	if o.ignore, err = regexp.Compile(ignore); err != nil {
		fmt.Fprintln(stderr, "--ignore:", err)
		return 2
	}
	o.failOn = map[string]bool{}
	for _, f := range strings.Split(failOn, ",") {
		switch f = strings.TrimSpace(f); f {
		case "":
		case "old-new", "way-back":
			o.failOn[f] = true
		default:
			fmt.Fprintf(stderr, "--fail-on: unknown %q (old-new, way-back)\n", f)
			return 2
		}
	}
	o.args, o.env = args, env
	if o.data == "" || o.models == "" {
		fmt.Fprintln(stderr, "--data and --models are required")
		return 2
	}
	if (o.nodes == "") != (o.fakeAgents == "") {
		fmt.Fprintln(stderr, "--nodes and --fake-agents go together")
		return 2
	}
	for _, s := range []schedSpec{o.old, o.new} {
		if err := s.check(); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	}
	rep, err := runPlans(context.Background(), o, stdout)
	if err != nil {
		fmt.Fprintln(stderr, "plans could not run:", err)
		return 2
	}
	if rep == nil {
		return 0
	}
	if (o.failOn["old-new"] && rep.OldNew.Profiles > 0) || (o.failOn["way-back"] && (rep.WayBackOld.Profiles > 0 || rep.WayBackNew.Profiles > 0)) {
		return 1
	}
	return 0
}

// dump is one run's plans: profile id -> field path -> the value as JSON.
type dump map[string]map[string]string

// diffRow is one field that differs.
type diffRow struct {
	ID    string `json:"id"`
	Field string `json:"field"`
	Old   string `json:"old"`
	New   string `json:"new"`
}

// comparison is one comparison of two dumps.
type comparison struct {
	Name     string    `json:"name"`
	A        string    `json:"a"`
	B        string    `json:"b"`
	Profiles int       `json:"profiles_differ"`
	IDs      []string  `json:"ids,omitempty"`
	Rows     []diffRow `json:"rows"`
}

type plansReport struct {
	Old        string         `json:"old"`
	New        string         `json:"new"`
	Profiles   map[string]int `json:"profiles"` // per dump
	Unsettled  []string       `json:"unsettled,omitempty"`
	OldNew     comparison     `json:"old_new"`
	WayBackOld comparison     `json:"way_back_old"`
	WayBackNew comparison     `json:"way_back_new"`
	Work       string         `json:"work"`
}

// flatten adds the leaves of v under prefix to out. An array of objects
// that name themselves (node, name or id) is keyed by that name, so a row
// is compared with the same node's row whatever the order.
func flatten(prefix string, v any, ignore *regexp.Regexp, out map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		if len(x) == 0 {
			out[prefix] = "{}"
			return
		}
		for k, e := range x {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			if ignore != nil && ignore.MatchString(p) {
				continue
			}
			flatten(p, e, ignore, out)
		}
	case []any:
		if len(x) == 0 {
			out[prefix] = "[]"
			return
		}
		keys := rowKeys(x)
		for i, e := range x {
			k := strconv.Itoa(i)
			if keys != nil {
				k = keys[i]
			}
			flatten(prefix+"["+k+"]", e, ignore, out)
		}
	default:
		b, _ := json.Marshal(x)
		out[prefix] = string(b)
	}
}

// rowKeys names each element of a by its node, name or id field, when every
// element has one and the names are distinct; nil otherwise.
func rowKeys(a []any) []string {
	for _, f := range []string{"node", "name", "id"} {
		keys := make([]string, 0, len(a))
		seen := map[string]bool{}
		for _, e := range a {
			m, ok := e.(map[string]any)
			if !ok {
				return nil
			}
			s, ok := m[f].(string)
			if !ok || s == "" || seen[s] {
				keys = nil
				break
			}
			seen[s] = true
			keys = append(keys, s)
		}
		if keys != nil {
			return keys
		}
	}
	return nil
}

// dumpOf builds a dump from a GET /api/profiles answer.
func dumpOf(raw []byte, ignore *regexp.Regexp) (dump, error) {
	var doc struct {
		Profiles []map[string]any `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("GET /api/profiles does not parse: %w", err)
	}
	if doc.Profiles == nil {
		return nil, errors.New("GET /api/profiles has no profiles list")
	}
	d := dump{}
	for _, p := range doc.Profiles {
		id, _ := p["id"].(string)
		if id == "" {
			return nil, errors.New("GET /api/profiles: a profile without an id")
		}
		f := map[string]string{}
		for _, k := range []string{"needs_mib", "needs_basis", "needs_measured", "plan"} {
			if v, ok := p[k]; ok {
				flatten(k, v, ignore, f)
			} else {
				f[k] = "(absent)"
			}
		}
		d[id] = f
	}
	return d, nil
}

// compare lists every field that differs between a and b. A profile in one
// dump only is one row, field "(profile)".
func compare(name, an, bn string, a, b dump) comparison {
	c := comparison{Name: name, A: an, B: bn, Rows: []diffRow{}}
	ids := map[string]bool{}
	for id := range a {
		ids[id] = true
	}
	for id := range b {
		ids[id] = true
	}
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
	for _, id := range sorted {
		fa, inA := a[id]
		fb, inB := b[id]
		var rows []diffRow
		switch {
		case !inA:
			rows = append(rows, diffRow{ID: id, Field: "(profile)", Old: "absent", New: "present"})
		case !inB:
			rows = append(rows, diffRow{ID: id, Field: "(profile)", Old: "present", New: "absent"})
		default:
			fields := map[string]bool{}
			for k := range fa {
				fields[k] = true
			}
			for k := range fb {
				fields[k] = true
			}
			fl := make([]string, 0, len(fields))
			for k := range fields {
				fl = append(fl, k)
			}
			sort.Strings(fl)
			for _, k := range fl {
				va, okA := fa[k]
				vb, okB := fb[k]
				if !okA {
					va = "(absent)"
				}
				if !okB {
					vb = "(absent)"
				}
				if va != vb {
					rows = append(rows, diffRow{ID: id, Field: k, Old: va, New: vb})
				}
			}
		}
		if len(rows) > 0 {
			c.Profiles++
			c.IDs = append(c.IDs, id)
			c.Rows = append(c.Rows, rows...)
		}
	}
	return c
}

func (c comparison) print(w io.Writer, n int) {
	fmt.Fprintf(w, "%s (%s vs %s): %d profiles differ, %d rows\n", c.Name, c.A, c.B, c.Profiles, len(c.Rows))
	for i, r := range c.Rows {
		if i == n {
			fmt.Fprintf(w, "  ... %d more rows in plans.json\n", len(c.Rows)-n)
			break
		}
		fmt.Fprintf(w, "  %s  %s  %s -> %s\n", r.ID, r.Field, truncate(r.Old, 200), truncate(r.New, 200))
	}
}

// nodesCount is the number of nodes of a potluck-fake-agents --nodes file
// ({"nodes": [...]} or a bare list).
func nodesCount(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var doc struct {
		Nodes []json.RawMessage `json:"nodes"`
	}
	if json.Unmarshal(b, &doc) == nil && doc.Nodes != nil {
		return len(doc.Nodes), nil
	}
	var list []json.RawMessage
	if err := json.Unmarshal(b, &list); err != nil {
		return 0, fmt.Errorf("%s: not a nodes file: %w", path, err)
	}
	return len(list), nil
}

func runPlans(ctx context.Context, o plansOpts, stdout io.Writer) (*plansReport, error) {
	nNodes := 0
	if o.nodes != "" {
		var err error
		if nNodes, err = nodesCount(o.nodes); err != nil {
			return nil, err
		}
		if fi, err := os.Stat(o.fakeAgents); err != nil || fi.IsDir() {
			return nil, fmt.Errorf("--fake-agents %s: not a file", o.fakeAgents)
		}
	}
	steps := []struct {
		name string
		spec schedSpec
	}{{"old-1", o.old}, {"new-1", o.new}, {"old-2", o.old}, {"new-2", o.new}}
	work := o.work
	if o.dryRun {
		if _, err := os.Stat(o.data); err != nil {
			return nil, fmt.Errorf("data folder: %w", err)
		}
		fmt.Fprintf(stdout, "dry run: copy %s -> %s once\n", o.data, filepath.Join(work, "data"))
		for _, s := range steps {
			fl := ""
			if o.nodes != "" {
				fl = fmt.Sprintf(" with %d fake agents from %s", nNodes, o.nodes)
			}
			fmt.Fprintf(stdout, "dry run: %s: start %s on the copy%s, dump GET /api/profiles\n", s.name, s.spec, fl)
		}
		fmt.Fprintln(stdout, "dry run: compare old-1/new-1 (old-new), old-1/old-2 and new-1/new-2 (way back)")
		return nil, nil
	}
	if work == "" {
		var err error
		if work, err = os.MkdirTemp("", "potluck-gate-plans-"); err != nil {
			return nil, err
		}
	}
	out := o.out
	if out == "" {
		out = work
	}
	copyTo := filepath.Join(work, "data")
	if err := copyDir(o.data, copyTo); err != nil {
		return nil, err
	}
	rep := &plansReport{Old: o.old.String(), New: o.new.String(), Profiles: map[string]int{}, Work: work}
	dumps := map[string]dump{}
	for _, s := range steps {
		d, settled, err := planRun(ctx, o, s.spec, s.name, copyTo, work, nNodes)
		if err != nil {
			return nil, fmt.Errorf("%s (%s): %w", s.name, s.spec, err)
		}
		if !settled {
			rep.Unsettled = append(rep.Unsettled, s.name)
		}
		dumps[s.name] = d
		rep.Profiles[s.name] = len(d)
		if err := writeJSONFile(filepath.Join(out, s.name+".json"), d); err != nil {
			return nil, err
		}
		fmt.Fprintf(stdout, "%s: %s: %d profiles%s\n", s.name, s.spec, len(d), map[bool]string{true: "", false: " (the plans did not settle)"}[settled])
	}
	rep.OldNew = compare("old-new", "old-1", "new-1", dumps["old-1"], dumps["new-1"])
	rep.WayBackOld = compare("way-back-old", "old-1", "old-2", dumps["old-1"], dumps["old-2"])
	rep.WayBackNew = compare("way-back-new", "new-1", "new-2", dumps["new-1"], dumps["new-2"])
	for _, c := range []comparison{rep.OldNew, rep.WayBackOld, rep.WayBackNew} {
		c.print(stdout, o.print)
	}
	if err := writeJSONFile(filepath.Join(out, "plans.json"), rep); err != nil {
		return nil, err
	}
	fmt.Fprintf(stdout, "plans: old-new %d profiles differ | way back OLD %d | way back NEW %d | %s\n",
		rep.OldNew.Profiles, rep.WayBackOld.Profiles, rep.WayBackNew.Profiles, filepath.Join(out, "plans.json"))
	return rep, nil
}

// planRun is one run: the scheduler on the copy, the fake fleet, the dump.
func planRun(ctx context.Context, o plansOpts, spec schedSpec, name, data, work string, nNodes int) (dump, bool, error) {
	listen, err := freeAddr("127.0.0.1")
	if err != nil {
		return nil, false, err
	}
	env := append([]string{}, o.env...)
	var agentPort, workerPort int
	if nNodes > 0 {
		if agentPort, err = freePort(o.addrBase); err != nil {
			return nil, false, fmt.Errorf("a port on %s for the fake agents: %w", o.addrBase, err)
		}
		if workerPort, err = freePort(o.addrBase); err != nil {
			return nil, false, err
		}
		env = append(env, "LLM_SCHEDULER_TEST_AGENT_PORT="+strconv.Itoa(agentPort), "LLM_SCHEDULER_TEST_WORKER_PORT="+strconv.Itoa(workerPort))
	}
	lf, err := os.Create(filepath.Join(work, name+".log"))
	if err != nil {
		return nil, false, err
	}
	defer lf.Close()
	logs := &logBuf{f: lf}
	sched, err := startScheduler(spec, runOpts{Listen: listen, Data: data, Models: o.models, Programs: o.programs, Args: o.args, Env: env, Log: logs})
	if err != nil {
		return nil, false, err
	}
	defer sched.stop(15 * time.Second)
	if err := sched.waitReady(o.ready); err != nil {
		return nil, false, fmt.Errorf("%w\n%s", err, tail(logs.String(), 2000))
	}
	c := newClient(sched.base)
	if nNodes > 0 {
		fa := exec.Command(o.fakeAgents, "--scheduler", sched.base, "--nodes", o.nodes, "--addr-base", o.addrBase,
			"--agent-port", strconv.Itoa(agentPort), "--worker-port", strconv.Itoa(workerPort))
		fa.Stdout, fa.Stderr = lf, lf
		if err := fa.Start(); err != nil {
			return nil, false, fmt.Errorf("starting the fake agents: %w", err)
		}
		defer func() { fa.Process.Kill(); fa.Wait() }()
		if err := waitAgents(ctx, c, nNodes, o.ready); err != nil {
			return nil, false, fmt.Errorf("%w\n%s", err, tail(logs.String(), 2000))
		}
	}
	// two reads in a row that agree: the plans have settled
	var prev dump
	deadline := time.Now().Add(o.settle)
	for {
		st, raw, err := c.call(ctx, http.MethodGet, "/api/profiles", nil, nil)
		if err != nil {
			return nil, false, err
		}
		if st != http.StatusOK {
			return nil, false, fmt.Errorf("GET /api/profiles: HTTP %d", st)
		}
		d, err := dumpOf(raw, o.ignore)
		if err != nil {
			return nil, false, err
		}
		if prev != nil && len(compare("", "", "", prev, d).Rows) == 0 {
			return d, true, nil
		}
		if time.Now().After(deadline) {
			return d, false, nil
		}
		prev = d
		time.Sleep(500 * time.Millisecond)
	}
}

// waitAgents waits until n agents are active in GET /api/status.
func waitAgents(ctx context.Context, c *client, n int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	got := 0
	for time.Now().Before(deadline) {
		var st struct {
			Agents []struct {
				State string `json:"state"`
			} `json:"agents"`
		}
		if code, _, err := c.call(ctx, http.MethodGet, "/api/status", nil, &st); err == nil && code == http.StatusOK {
			got = 0
			for _, a := range st.Agents {
				if a.State == "active" {
					got++
				}
			}
			if got >= n {
				return nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("%d of %d fake agents active after %s", got, n, timeout)
}

// freePort is a port free on host now, outside the ephemeral range, that the
// fake agents bind a moment later (internal/testutil/ports).
func freePort(host string) (int, error) { return ports.FreeOn(host) }
