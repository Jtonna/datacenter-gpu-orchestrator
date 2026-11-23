package benchspec

import (
	"encoding/json"
	"strings"
	"testing"
)

const okSet = `{"format":"potluck-tasks/1","id":"coding","version":1,"title":"Coding","system":"You are an expert Python programmer.","max_tokens":8192,"seed":1,
  "tasks":[{"id":"t1","title":"parse_duration","prompt":"Write parse_duration."},
           {"id":"t2","prompt":"Fix the bug.","max_tokens":2048,"timeout_s":600},
           {"id":"t3","prompt":"No system prompt here.","system":""}]}`

func mustSet(t *testing.T, js string) *TaskSet {
	t.Helper()
	s, err := ParseTaskSetJSON([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTaskSetValidatesAndBuildsFixedRequests(t *testing.T) {
	s := mustSet(t, okSet)
	if len(s.Tasks) != 3 || s.SeedVal() != 1 {
		t.Fatalf("%+v", s)
	}
	r := s.Request(s.Tasks[0])
	b, _ := json.Marshal(r.ChatMessages())
	if string(b) != `[{"role":"system","content":"You are an expert Python programmer."},{"role":"user","content":"Write parse_duration."}]` {
		t.Fatalf("messages %s", b)
	}
	if r.Name != "t1" || r.MaxTok() != 8192 || r.Temp() != 0 || r.SeedVal() != 1 || !r.Streams() || r.CachesPrompt() {
		t.Fatalf("request %+v", r)
	}
	if r := s.Request(s.Tasks[1]); r.MaxTok() != 2048 {
		t.Fatalf("a task's own max_tokens wins: %d", r.MaxTok())
	}
	if msgs := s.Request(s.Tasks[2]).ChatMessages(); len(msgs) != 1 || msgs[0].Role != "user" {
		t.Fatalf("an empty task system prompt means none: %+v", msgs)
	}
	if s.Tasks[1].Timeout().Seconds() != 600 || s.Tasks[0].Timeout() != 0 {
		t.Fatal("task timeouts")
	}
	// No seed and no max_tokens: the defaults.
	d := mustSet(t, `{"id":"d","version":1,"tasks":[{"id":"a","prompt":"x"}]}`)
	if d.Format != TaskSetFormat || d.SeedVal() != DefaultTaskSeed || d.Request(d.Tasks[0]).MaxTok() != DefaultTaskMaxTokens {
		t.Fatalf("defaults %+v", d)
	}
}

// The content hash names what the model is asked, so results stay comparable:
// a new title, description, version or id keeps it; a changed prompt, system
// prompt, token limit or seed changes it.
func TestTaskSetHashFollowsTheContent(t *testing.T) {
	base := mustSet(t, okSet).SHA256()
	if len(base) != 64 {
		t.Fatalf("%q", base)
	}
	same := []string{
		strings.Replace(okSet, `"title":"Coding"`, `"title":"Coding tasks"`, 1),
		strings.Replace(okSet, `"version":1`, `"version":2`, 1),
		strings.Replace(okSet, `"id":"coding"`, `"id":"coding-copy"`, 1),
		strings.Replace(okSet, `"timeout_s":600`, `"timeout_s":900`, 1),
		// the set's default spelled out on the task is the same request
		strings.Replace(okSet, `"prompt":"Write parse_duration."`, `"prompt":"Write parse_duration.","max_tokens":8192`, 1),
	}
	for i, js := range same {
		if h := mustSet(t, js).SHA256(); h != base {
			t.Errorf("same content %d hashed differently", i)
		}
	}
	other := []string{
		strings.Replace(okSet, "Fix the bug.", "Fix the bugs.", 1),
		strings.Replace(okSet, "expert Python", "expert Go", 1),
		strings.Replace(okSet, `"max_tokens":2048`, `"max_tokens":2049`, 1),
		strings.Replace(okSet, `"seed":1`, `"seed":2`, 1),
		strings.Replace(okSet, `"id":"t3"`, `"id":"t9"`, 1),
	}
	for i, js := range other {
		if h := mustSet(t, js).SHA256(); h == base {
			t.Errorf("changed content %d kept the hash", i)
		}
	}
}

func TestTaskSetRejects(t *testing.T) {
	for name, c := range map[string]struct{ js, want string }{
		"unknown field":   {`{"id":"a","version":1,"tasks":[{"id":"x","prompt":"p"}],"extra":1}`, "unknown field"},
		"bad format":      {`{"format":"other/2","id":"a","version":1,"tasks":[{"id":"x","prompt":"p"}]}`, "format"},
		"bad id":          {`{"id":"A B","version":1,"tasks":[{"id":"x","prompt":"p"}]}`, "id"},
		"no version":      {`{"id":"a","tasks":[{"id":"x","prompt":"p"}]}`, "version"},
		"no tasks":        {`{"id":"a","version":1,"tasks":[]}`, "tasks"},
		"empty prompt":    {`{"id":"a","version":1,"tasks":[{"id":"x","prompt":"  "}]}`, "prompt is empty"},
		"duplicate task":  {`{"id":"a","version":1,"tasks":[{"id":"x","prompt":"p"},{"id":"x","prompt":"q"}]}`, "twice"},
		"bad task id":     {`{"id":"a","version":1,"tasks":[{"id":"X!","prompt":"p"}]}`, "tasks[0]"},
		"temperature":     {`{"id":"a","version":1,"temperature":0.7,"tasks":[{"id":"x","prompt":"p"}]}`, "temperature"},
		"max tokens":      {`{"id":"a","version":1,"tasks":[{"id":"x","prompt":"p","max_tokens":999999}]}`, "max_tokens"},
		"task timeout":    {`{"id":"a","version":1,"tasks":[{"id":"x","prompt":"p","timeout_s":-1}]}`, "timeout_s"},
		"trailing":        {`{"id":"a","version":1,"tasks":[{"id":"x","prompt":"p"}]} {}`, "unexpected data"},
		"temperature ok?": {`{"id":"a","version":1,"temperature":0,"tasks":[{"id":"x","prompt":"p"}]}`, ""},
	} {
		s, err := ParseTaskSetJSON([]byte(c.js))
		if err == nil {
			err = s.Validate()
		}
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
}

func TestTasksPhaseValidation(t *testing.T) {
	ok := []string{
		`{"id":"a","profile":"p","phases":[{"type":"load"},{"type":"tasks","task_set":"coding"},{"type":"unload"}]}`,
		`{"id":"a","profile":"p","timeout_s":4000,"phases":[{"type":"tasks","task_set":"coding","task_set_version":2,"timeout_s":3600}]}`,
		`{"id":"a","profile":"p","phases":[{"type":"tasks","label":"inline","tasks":{"id":"mini","version":1,"tasks":[{"id":"q","prompt":"hi"}]}}]}`,
	}
	for _, js := range ok {
		if err := validate(t, js); err != nil {
			t.Errorf("%s: %v", js, err)
		}
	}
	for name, c := range map[string]struct{ js, want string }{
		"neither":         {`{"id":"a","profile":"p","phases":[{"type":"tasks"}]}`, "task_set or tasks is required"},
		"both":            {`{"id":"a","profile":"p","phases":[{"type":"tasks","task_set":"c","tasks":{"id":"m","version":1,"tasks":[{"id":"q","prompt":"hi"}]}}]}`, "not both"},
		"bad ref":         {`{"id":"a","profile":"p","phases":[{"type":"tasks","task_set":"Not A Slug"}]}`, "task_set"},
		"bad inline":      {`{"id":"a","profile":"p","phases":[{"type":"tasks","tasks":{"id":"m","version":1,"tasks":[]}}]}`, "tasks: between 1"},
		"version alone":   {`{"id":"a","profile":"p","phases":[{"type":"tasks","task_set_version":1,"tasks":{"id":"m","version":1,"tasks":[{"id":"q","prompt":"hi"}]}}]}`, "task_set_version"},
		"timeout too big": {`{"id":"a","profile":"p","phases":[{"type":"tasks","task_set":"c","timeout_s":600}]}`, "timeout_s to at least 630"},
		"option on other": {`{"id":"a","profile":"p","phases":[{"type":"load","timeout_s":5}]}`, `option "timeout_s" does not belong`},
		"tasks on other":  {`{"id":"a","profile":"p","phases":[{"type":"unload","task_set":"c"}]}`, `option "task_set" does not belong`},
	} {
		err := validate(t, c.js)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
	b := mustBench(t, `{"id":"a","profile":"p","phases":[{"type":"tasks","task_set":"coding","task_set_version":3},{"type":"tasks","tasks":{"id":"m","version":1,"tasks":[{"id":"q","prompt":"hi"}]}}]}`)
	refs := b.TaskSetRefs()
	if len(refs) != 1 || refs[0].ID != "coding" || refs[0].Version != 3 {
		t.Fatalf("refs %+v", refs)
	}
}

func TestSchemaHasTheTasksPhase(t *testing.T) {
	var ps *PhaseSchema
	for i, p := range Schema().PhaseTypes {
		if p.Type == PhaseTasks {
			ps = &Schema().PhaseTypes[i]
		}
	}
	if ps == nil {
		t.Fatal("no tasks phase in the schema")
	}
	names := map[string]bool{}
	for _, o := range ps.Options {
		names[o.Name] = true
	}
	for _, n := range []string{"task_set", "task_set_version", "tasks", "timeout_s"} {
		if !names[n] {
			t.Errorf("option %s is not described", n)
		}
	}
	if !strings.Contains(ps.Description, "never runs") {
		t.Errorf("the description must say Potluck never runs the answers: %q", ps.Description)
	}
}
