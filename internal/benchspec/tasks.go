package benchspec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PhaseTasks sends each task of a task set once, with the profile loaded, and
// keeps every answer for a scorer outside Potluck. Potluck never runs what a
// model wrote and does not judge it.
const PhaseTasks = "tasks"

// Task set format and limits.
const (
	TaskSetFormat        = "potluck-tasks/1"
	MaxTasks             = 64
	MaxTaskPromptBytes   = 1 << 20
	DefaultTaskMaxTokens = 4096
	DefaultTaskSeed      = 1
	// TasksPhaseMargin is the time a tasks phase leaves its benchmark for
	// what follows (the unload): a phase timeout must fit in timeout_s with it.
	TasksPhaseMargin = 30
)

// Task is one fixed prompt of a task set.
type Task struct {
	ID    string `json:"id" yaml:"id"`
	Title string `json:"title,omitempty" yaml:"title,omitempty"`
	// Prompt is the user message, sent exactly as written.
	Prompt string `json:"prompt" yaml:"prompt"`
	// System overrides the set's system prompt; "" means no system message.
	System *string `json:"system,omitempty" yaml:"system,omitempty"`
	// MaxTokens overrides the set's max_tokens.
	MaxTokens int `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
	// TimeoutS limits this task's answer (0: only the phase's timeout).
	TimeoutS int `json:"timeout_s,omitempty" yaml:"timeout_s,omitempty"`
}

// Timeout is timeout_s, 0 when the task has none.
func (t Task) Timeout() time.Duration { return time.Duration(t.TimeoutS) * time.Second }

// TaskSet is a versioned list of fixed tasks: data, not code. Sampling is
// fixed: temperature 0 and a seed (default 1), streamed, no prompt cache.
type TaskSet struct {
	Format      string `json:"format,omitempty" yaml:"format,omitempty"`
	ID          string `json:"id" yaml:"id"`
	Version     int    `json:"version" yaml:"version"`
	Title       string `json:"title,omitempty" yaml:"title,omitempty"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	// System is the system prompt of every task that does not set its own.
	System string `json:"system,omitempty" yaml:"system,omitempty"`
	// MaxTokens is the answer limit of every task that does not set its own (default 4096).
	MaxTokens int    `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
	Seed      *int64 `json:"seed,omitempty" yaml:"seed,omitempty"`
	// Temperature may be given, but only as 0: it is fixed.
	Temperature *float64 `json:"temperature,omitempty" yaml:"temperature,omitempty"`
	Tasks       []Task   `json:"tasks" yaml:"tasks"`
}

// ParseTaskSetJSON decodes a task set strictly. Call Validate next.
func ParseTaskSetJSON(data []byte) (*TaskSet, error) {
	var s TaskSet
	if err := DecodeStrict(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// SeedVal is the seed (default 1).
func (s *TaskSet) SeedVal() int64 {
	if s.Seed == nil {
		return DefaultTaskSeed
	}
	return *s.Seed
}

func (s *TaskSet) maxTok(t Task) int {
	switch {
	case t.MaxTokens > 0:
		return t.MaxTokens
	case s.MaxTokens > 0:
		return s.MaxTokens
	}
	return DefaultTaskMaxTokens
}

func (s *TaskSet) system(t Task) string {
	if t.System != nil {
		return *t.System
	}
	return s.System
}

// Request is what a task sends: the system prompt (when there is one) and the
// prompt, temperature 0, the set's seed, streamed (for the time to first
// token), no prompt cache. Thinking is left to the profile: nothing here turns
// it on or off.
func (s *TaskSet) Request(t Task) Request {
	var msgs []Message
	if sys := s.system(t); sys != "" {
		msgs = append(msgs, Message{Role: "system", Content: sys})
	}
	msgs = append(msgs, Message{Role: "user", Content: t.Prompt})
	zero, seed, on, off := 0.0, s.SeedVal(), true, false
	return Request{Name: t.ID, Messages: msgs, MaxTokens: s.maxTok(t), Temperature: &zero, Seed: &seed, Stream: &on, CachePrompt: &off}
}

// SHA256 identifies what the model is asked: the format, the seed and, per
// task, its id, system prompt, prompt and token limit, with the defaults
// applied. Titles, descriptions, the set's id and version and the timeouts
// are left out, so renaming a set or giving it more time keeps results
// comparable. The hash is of a fixed JSON shape and never changes for a set
// whose content does not.
func (s *TaskSet) SHA256() string {
	type ct struct {
		ID        string `json:"id"`
		System    string `json:"system"`
		Prompt    string `json:"prompt"`
		MaxTokens int    `json:"max_tokens"`
	}
	c := struct {
		Format string `json:"format"`
		Seed   int64  `json:"seed"`
		Tasks  []ct   `json:"tasks"`
	}{Format: TaskSetFormat, Seed: s.SeedVal()}
	for _, t := range s.Tasks {
		c.Tasks = append(c.Tasks, ct{ID: t.ID, System: s.system(t), Prompt: t.Prompt, MaxTokens: s.maxTok(t)})
	}
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Validate checks a task set and fills its format.
func (s *TaskSet) Validate() error {
	var errs []error
	add := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	switch s.Format {
	case "":
		s.Format = TaskSetFormat
	case TaskSetFormat:
	default:
		add("format %q is not known: leave it out or use %q", s.Format, TaskSetFormat)
	}
	if !slugRe.MatchString(s.ID) {
		add("id %q must be 1 to 64 characters: lower-case letters, digits, '.', '-' or '_', starting with a letter or digit", s.ID)
	}
	if s.Version < 1 || s.Version > 1_000_000 {
		add("version must be a whole number from 1 (give the set a new version whenever a prompt changes)")
	}
	if len(s.Title) > 120 {
		add("title is longer than 120 characters")
	}
	if len(s.Description) > 4000 {
		add("description is longer than 4000 characters")
	}
	if len(s.System) > MaxTaskPromptBytes {
		add("system is larger than 1 MiB")
	}
	if s.MaxTokens < 0 || s.MaxTokens > MaxTokensLimit {
		add("max_tokens must be 1 to %d (default %d)", MaxTokensLimit, DefaultTaskMaxTokens)
	}
	if s.Temperature != nil && *s.Temperature != 0 {
		add("temperature is fixed at 0 for tasks, so answers can be compared; leave it out")
	}
	if len(s.Tasks) == 0 || len(s.Tasks) > MaxTasks {
		add("tasks: between 1 and %d are required", MaxTasks)
	}
	seen := map[string]bool{}
	for i, t := range s.Tasks {
		w := func(f string, a ...any) { add("tasks[%d] (%s): %s", i, t.ID, fmt.Sprintf(f, a...)) }
		if !slugRe.MatchString(t.ID) {
			w("id must be 1 to 64 characters: lower-case letters, digits, '.', '-' or '_'")
		}
		if seen[t.ID] {
			w("id is used twice")
		}
		seen[t.ID] = true
		if len(t.Title) > 200 {
			w("title is longer than 200 characters")
		}
		if strings.TrimSpace(t.Prompt) == "" {
			w("prompt is empty")
		}
		if len(t.Prompt) > MaxTaskPromptBytes {
			w("prompt is larger than 1 MiB")
		}
		if t.System != nil && len(*t.System) > MaxTaskPromptBytes {
			w("system is larger than 1 MiB")
		}
		if t.MaxTokens < 0 || t.MaxTokens > MaxTokensLimit {
			w("max_tokens must be 1 to %d", MaxTokensLimit)
		}
		if t.TimeoutS < 0 || t.TimeoutS > MaxTimeoutS {
			w("timeout_s must be 1 to %d", MaxTimeoutS)
		}
	}
	return errors.Join(errs...)
}

// TaskSetRef names a stored task set; Version 0 is its newest version.
type TaskSetRef struct {
	ID      string
	Version int
}

// TaskSetRefs lists the stored task sets the tasks phases name.
func (b *Benchmark) TaskSetRefs() []TaskSetRef {
	var out []TaskSetRef
	for _, p := range b.Expand() {
		if p.Type == PhaseTasks && p.TaskSet != "" {
			out = append(out, TaskSetRef{ID: p.TaskSet, Version: p.TaskSetVersion})
		}
	}
	return out
}

// PhaseTimeout is a tasks phase's timeout_s, 0 when it has none (the phase
// then has what the benchmark's timeout leaves, minus TasksPhaseMargin).
func (p *Phase) PhaseTimeout() time.Duration {
	if p.TimeoutS == nil {
		return 0
	}
	return time.Duration(*p.TimeoutS) * time.Second
}

func (b *Benchmark) validateTasksPhase(p *Phase, w func(string, ...any)) {
	switch {
	case p.TaskSet == "" && p.Tasks == nil:
		w("task_set or tasks is required: name a stored task set, or give the set inline")
	case p.TaskSet != "" && p.Tasks != nil:
		w("give task_set (a stored set) or tasks (an inline set), not both")
	case p.TaskSet != "" && !slugRe.MatchString(p.TaskSet):
		w("task_set %q is not a task set id", p.TaskSet)
	}
	if p.TaskSetVersion < 0 {
		w("task_set_version must be 1 or more (leave it out for the newest version)")
	}
	if p.TaskSetVersion != 0 && p.TaskSet == "" {
		w("task_set_version only goes with task_set")
	}
	if p.Tasks != nil {
		if err := p.Tasks.Validate(); err != nil {
			for _, e := range strings.Split(err.Error(), "\n") {
				w("tasks: %s", e)
			}
		}
	}
	if p.TimeoutS != nil {
		limit := int(b.Timeout().Seconds())
		switch {
		case *p.TimeoutS < 1 || *p.TimeoutS > MaxTimeoutS:
			w("timeout_s must be 1 to %d", MaxTimeoutS)
		case *p.TimeoutS+TasksPhaseMargin > limit:
			w("timeout_s (%d s) does not fit in the benchmark's timeout_s (%d s) with %d s left for the unload: raise the benchmark's timeout_s to at least %d", *p.TimeoutS, limit, TasksPhaseMargin, *p.TimeoutS+TasksPhaseMargin)
		}
	}
}
