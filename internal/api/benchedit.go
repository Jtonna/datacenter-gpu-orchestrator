package api

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
)

// The editors (board bench-editors): every save of a benchmark or a group
// needs a comment and is one audited change carrying it; a refused save
// names the parts its sentences are about; a run says which of its
// definitions changed since it ran.

// saveBody reads the body of a save of a benchmark or a group. The comment
// (required: the body's "comment", else the X-Comment header) is taken out
// of the body, which the strict parser then reads, and is put in X-Comment
// so the audit entry carries it.
func saveBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, ok := readBody(w, r)
	if !ok {
		return nil, false
	}
	comment := ""
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) == nil {
		if raw, has := m["comment"]; has {
			if err := json.Unmarshal(raw, &comment); err != nil {
				apiErr(w, http.StatusBadRequest, "comment must be text: why this change is made")
				return nil, false
			}
			delete(m, "comment")
			body, _ = json.Marshal(m)
		}
	}
	if strings.TrimSpace(comment) == "" {
		comment = audit.FromRequest(r).Comment
	}
	comment = strings.TrimSpace(comment)
	if comment == "" {
		apiErr(w, http.StatusBadRequest, commentRequired)
		return nil, false
	}
	r.Header.Set("X-Comment", comment)
	return body, true
}

// problem is one sentence of a refused save and the part of the definition
// it names ("phases[2]", "expect.outcome", "benchmarks[0]"; empty when the
// sentence names none).
type problem struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// the roots a refusal sentence of benchspec starts with when it names a part
var problemRoots = map[string]bool{"id": true, "name": true, "description": true, "profile": true, "phases": true, "requests": true,
	"repeats": true, "warmup_requests": true, "timeout_s": true, "expect": true, "tags": true, "inputs": true, "parallel": true,
	"cold": true, "unload_after": true, "recipe": true, "benchmarks": true, "cooldown_s": true, "idle_wait_s": true, "schedule": true,
	"stop_on_failure": true, "only_when_idle": true, "evict_others": true, "status": true, "reason": true, "baseline": true}

var problemPathRe = regexp.MustCompile(`^([a-z_]+)((?:\[\d+\])?(?:\.[a-z_]+(?:\[\d+\])?)*)`)

// problemsOf splits a validation error into its sentences, each with the
// part it names.
func problemsOf(err error) []problem {
	var out []problem
	for _, line := range strings.Split(err.Error(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		p := problem{Message: line}
		if m := problemPathRe.FindStringSubmatch(line); m != nil && problemRoots[m[1]] {
			rest := line[len(m[0]):]
			if rest == "" || rest[0] == ' ' || rest[0] == ':' {
				p.Path = m[0]
			}
		}
		out = append(out, p)
	}
	return out
}

// refuseSave answers 422 with the server's sentences (error.message, as
// before) and the problems the editor marks.
func refuseSave(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"error":    map[string]any{"message": err.Error(), "type": errType(http.StatusUnprocessableEntity), "param": nil, "code": http.StatusUnprocessableEntity, "reason": "invalid"},
		"problems": problemsOf(err)})
}

// defChange is one benchmark a run ran whose stored definition is different
// now (changed) or not stored any more (gone).
type defChange struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Change string `json:"change"`
}

// runOut is a run as GET /api/bench/runs/{id} answers it: with the
// definitions changed since it ran (nil: the run does not keep its
// definitions, an older run).
type runOut struct {
	*bench.Run
	DefinitionsChanged *[]defChange `json:"definitions_changed,omitempty"`
}

// definitionsChanged compares the benchmarks a run ran with the stored ones.
// A recipe's entries (made from the recipe and their inputs when the run was
// planned) are not compared.
func (s *Server) definitionsChanged(run *bench.Run) *[]defChange {
	was := bench.PlanBenchmarks(run)
	if was == nil {
		return nil
	}
	out := []defChange{}
	for id, b := range was {
		if b == nil || b.Recipe != "" {
			continue
		}
		now, ok := s.lookup(id)
		if !ok {
			out = append(out, defChange{ID: id, Name: b.Title(), Change: "gone"})
			continue
		}
		x, _ := json.Marshal(b)
		y, _ := json.Marshal(now)
		if string(x) != string(y) {
			out = append(out, defChange{ID: id, Name: now.Title(), Change: "changed"})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return &out
}
