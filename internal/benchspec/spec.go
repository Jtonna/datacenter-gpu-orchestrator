// Package benchspec defines what a benchmark and a suite are (design 9.3): the
// JSON and YAML shapes, their defaults and their strict validation. It has no
// dependencies on the scheduler, the store or the config package, so the
// config loader, the REST handlers and the runner share one definition.
package benchspec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Limits and defaults.
const (
	DefaultTimeoutS    = 600
	MaxTimeoutS        = 86400
	MaxRequests        = 64
	MaxRepeats         = 100
	MaxWarmups         = 10
	MaxTokensLimit     = 65536
	DefaultMaxTokens   = 128
	DefaultSeed        = 42
	DefaultCooldownS   = 5
	DefaultIdleWaitS   = 600
	MaxSuiteItems      = 100
	MinScheduleEvery   = 10 * time.Minute
	MaxParallelMembers = 8
)

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)
var atRe = regexp.MustCompile(`^([01][0-9]|2[0-3]):([0-5][0-9])$`)

// Expected outcomes of a benchmark (expect.outcome).
const (
	OutcomeReady   = "ready"
	OutcomeRefused = "refused"
	OutcomeFailed  = "failed"
)

// Message is one chat message.
type Message struct {
	Role    string `json:"role" yaml:"role"`
	Content string `json:"content" yaml:"content"`
}

// Request is one request of a benchmark.
type Request struct {
	Name        string    `json:"name,omitempty" yaml:"name,omitempty"`
	Messages    []Message `json:"messages,omitempty" yaml:"messages,omitempty"`
	Prompt      string    `json:"prompt,omitempty" yaml:"prompt,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
	Temperature *float64  `json:"temperature,omitempty" yaml:"temperature,omitempty"`
	Seed        *int64    `json:"seed,omitempty" yaml:"seed,omitempty"`
	CachePrompt *bool     `json:"cache_prompt,omitempty" yaml:"cache_prompt,omitempty"`
	Stream      *bool     `json:"stream,omitempty" yaml:"stream,omitempty"`
}

// Temp is the sampling temperature (default 0).
func (r Request) Temp() float64 {
	if r.Temperature == nil {
		return 0
	}
	return *r.Temperature
}

// SeedVal is the seed (default 42).
func (r Request) SeedVal() int64 {
	if r.Seed == nil {
		return DefaultSeed
	}
	return *r.Seed
}

// CachesPrompt is cache_prompt (default false: every request does its full prefill).
func (r Request) CachesPrompt() bool { return r.CachePrompt != nil && *r.CachePrompt }

// Streams is stream (default false).
func (r Request) Streams() bool { return r.Stream != nil && *r.Stream }

// Deterministic says the output should be the same every time: temperature 0 (the seed always applies).
func (r Request) Deterministic() bool { return r.Temp() == 0 }

// MaxTok is max_tokens with its default.
func (r Request) MaxTok() int {
	if r.MaxTokens <= 0 {
		return DefaultMaxTokens
	}
	return r.MaxTokens
}

// ChatMessages is what is sent: the messages, or the prompt as one user message.
func (r Request) ChatMessages() []Message {
	if len(r.Messages) > 0 {
		return r.Messages
	}
	return []Message{{Role: "user", Content: r.Prompt}}
}

// Expect are the optional checks of a benchmark.
type Expect struct {
	Outcome      string   `json:"outcome,omitempty" yaml:"outcome,omitempty"`
	MinDecodeTPS *float64 `json:"min_decode_tps,omitempty" yaml:"min_decode_tps,omitempty"`
	MaxLoadS     *float64 `json:"max_load_s,omitempty" yaml:"max_load_s,omitempty"`
	OutputSHA256 string   `json:"output_sha256,omitempty" yaml:"output_sha256,omitempty"`
}

// Want is the expected outcome (default ready).
func (e *Expect) Want() string {
	if e == nil || e.Outcome == "" {
		return OutcomeReady
	}
	return e.Outcome
}

// Benchmark is a test case, or (with Parallel) a concurrent step.
type Benchmark struct {
	ID             string    `json:"id" yaml:"id"`
	Name           string    `json:"name,omitempty" yaml:"name,omitempty"`
	Description    string    `json:"description,omitempty" yaml:"description,omitempty"`
	Profile        string    `json:"profile,omitempty" yaml:"profile,omitempty"`
	Cold           *bool     `json:"cold,omitempty" yaml:"cold,omitempty"`
	Requests       []Request `json:"requests,omitempty" yaml:"requests,omitempty"`
	Repeats        int       `json:"repeats,omitempty" yaml:"repeats,omitempty"`
	WarmupRequests *int      `json:"warmup_requests,omitempty" yaml:"warmup_requests,omitempty"`
	TimeoutS       int       `json:"timeout_s,omitempty" yaml:"timeout_s,omitempty"`
	Expect         *Expect   `json:"expect,omitempty" yaml:"expect,omitempty"`
	UnloadAfter    *bool     `json:"unload_after,omitempty" yaml:"unload_after,omitempty"`
	Tags           []string  `json:"tags,omitempty" yaml:"tags,omitempty"`
	// Parallel makes this a concurrent step: the benchmarks named here start at
	// the same moment. It excludes every other field except id, name, description and tags.
	Parallel []string `json:"parallel,omitempty" yaml:"parallel,omitempty"`
}

// IsParallel says whether this is a concurrent step.
func (b *Benchmark) IsParallel() bool { return len(b.Parallel) > 0 }

// IsCold is cold (default true).
func (b *Benchmark) IsCold() bool { return b.Cold == nil || *b.Cold }

// UnloadsAfter is unload_after (default true).
func (b *Benchmark) UnloadsAfter() bool { return b.UnloadAfter == nil || *b.UnloadAfter }

// RepeatCount is repeats (default 1).
func (b *Benchmark) RepeatCount() int {
	if b.Repeats <= 0 {
		return 1
	}
	return b.Repeats
}

// Warmups is warmup_requests (default 1).
func (b *Benchmark) Warmups() int {
	if b.WarmupRequests == nil {
		return 1
	}
	return *b.WarmupRequests
}

// Timeout is timeout_s (default 600): the whole benchmark, load included.
func (b *Benchmark) Timeout() time.Duration {
	if b.TimeoutS <= 0 {
		return DefaultTimeoutS * time.Second
	}
	return time.Duration(b.TimeoutS) * time.Second
}

// Title is the name, else the id.
func (b *Benchmark) Title() string {
	if b.Name != "" {
		return b.Name
	}
	return b.ID
}

// DecodeStrict decodes JSON that must contain no unknown fields and no trailing data.
func DecodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after the JSON value")
	}
	return nil
}

// ParseBenchmarkJSON decodes a benchmark strictly. Call Validate next.
func ParseBenchmarkJSON(data []byte) (*Benchmark, error) {
	var b Benchmark
	if err := DecodeStrict(data, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// Validate checks a benchmark and fills the defaults that are stored with it
// (name). profileOK says whether a profile id (or alias) exists.
func (b *Benchmark) Validate(profileOK func(string) bool) error {
	var errs []error
	add := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if !slugRe.MatchString(b.ID) {
		add("id %q must be 1 to 64 characters: lower-case letters, digits, '.', '-' or '_', starting with a letter or digit", b.ID)
	}
	if len(b.Name) > 120 {
		add("name is longer than 120 characters")
	}
	if len(b.Description) > 2000 {
		add("description is longer than 2000 characters")
	}
	if b.Name == "" {
		b.Name = b.ID
	}
	if len(b.Tags) > 20 {
		add("at most 20 tags")
	}
	for _, t := range b.Tags {
		if !slugRe.MatchString(t) {
			add("tag %q must be a slug (lower-case letters, digits, '.', '-', '_')", t)
		}
	}
	if b.IsParallel() {
		if b.Profile != "" || len(b.Requests) > 0 || b.Cold != nil || b.Repeats != 0 || b.WarmupRequests != nil ||
			b.Expect != nil || b.UnloadAfter != nil || b.TimeoutS != 0 {
			add("a parallel step names the benchmarks to start together and nothing else (no profile, requests, cold, repeats, warmup_requests, timeout_s, expect or unload_after)")
		}
		if len(b.Parallel) < 2 || len(b.Parallel) > MaxParallelMembers {
			add("parallel needs 2 to %d benchmark ids", MaxParallelMembers)
		}
		seen := map[string]bool{}
		for _, id := range b.Parallel {
			switch {
			case !slugRe.MatchString(id):
				add("parallel: %q is not a benchmark id", id)
			case id == b.ID:
				add("parallel: a step cannot contain itself")
			case seen[id]:
				add("parallel: %q is listed twice", id)
			}
			seen[id] = true
		}
		return errors.Join(errs...)
	}
	switch {
	case b.Profile == "":
		add("profile is required")
	case profileOK != nil && !profileOK(b.Profile):
		add("profile %q is not a profile of this scheduler", b.Profile)
	}
	if b.Repeats < 0 || b.Repeats > MaxRepeats {
		add("repeats must be 0 to %d", MaxRepeats)
	}
	if b.WarmupRequests != nil && (*b.WarmupRequests < 0 || *b.WarmupRequests > MaxWarmups) {
		add("warmup_requests must be 0 to %d", MaxWarmups)
	}
	if b.TimeoutS < 0 || b.TimeoutS > MaxTimeoutS {
		add("timeout_s must be 0 to %d", MaxTimeoutS)
	}
	if len(b.Requests) == 0 || len(b.Requests) > MaxRequests {
		add("requests: between 1 and %d are required", MaxRequests)
	}
	for i := range b.Requests {
		r := &b.Requests[i]
		if r.Name == "" {
			r.Name = fmt.Sprintf("request-%d", i+1)
		}
		w := func(f string, a ...any) { add("requests[%d] (%s): %s", i, r.Name, fmt.Sprintf(f, a...)) }
		if len(r.Name) > 80 {
			w("name is longer than 80 characters")
		}
		switch {
		case r.Prompt != "" && len(r.Messages) > 0:
			w("give either messages or prompt, not both")
		case r.Prompt == "" && len(r.Messages) == 0:
			w("messages or prompt is required")
		}
		if len(r.Prompt) > 1<<20 {
			w("prompt is larger than 1 MiB")
		}
		for j, m := range r.Messages {
			if m.Role != "system" && m.Role != "user" && m.Role != "assistant" {
				w("messages[%d].role must be system, user or assistant, got %q", j, m.Role)
			}
			if m.Content == "" {
				w("messages[%d].content is empty", j)
			}
		}
		if r.MaxTokens < 0 || r.MaxTokens > MaxTokensLimit {
			w("max_tokens must be 1 to %d (default %d)", MaxTokensLimit, DefaultMaxTokens)
		}
		if r.Temperature != nil && (*r.Temperature < 0 || *r.Temperature > 2) {
			w("temperature must be 0 to 2")
		}
	}
	if e := b.Expect; e != nil {
		switch e.Outcome {
		case "", OutcomeReady, OutcomeRefused, OutcomeFailed:
		default:
			add("expect.outcome must be ready, refused or failed, got %q", e.Outcome)
		}
		if (e.Outcome == OutcomeRefused || e.Outcome == OutcomeFailed) && (e.MinDecodeTPS != nil || e.OutputSHA256 != "") {
			add("expect.min_decode_tps and expect.output_sha256 only make sense when the outcome is ready")
		}
		if e.MinDecodeTPS != nil && *e.MinDecodeTPS < 0 {
			add("expect.min_decode_tps must not be negative")
		}
		if e.MaxLoadS != nil && *e.MaxLoadS <= 0 {
			add("expect.max_load_s must be positive")
		}
		if e.OutputSHA256 != "" && !sha256Re.MatchString(e.OutputSHA256) {
			add("expect.output_sha256 must be 64 lower-case hex characters")
		}
	}
	return errors.Join(errs...)
}

// Schedule runs a suite by itself: every interval, or at a local time of day.
type Schedule struct {
	Every string `json:"every,omitempty" yaml:"every,omitempty"`
	At    string `json:"at,omitempty" yaml:"at,omitempty"`
}

func (s *Schedule) validate() error {
	switch {
	case s.Every != "" && s.At != "":
		return errors.New("schedule: give every or at, not both")
	case s.Every == "" && s.At == "":
		return errors.New("schedule: every or at is required")
	case s.At != "":
		if !atRe.MatchString(s.At) {
			return fmt.Errorf("schedule.at %q must be a local time like \"03:00\"", s.At)
		}
	default:
		d, err := time.ParseDuration(s.Every)
		if err != nil {
			return fmt.Errorf("schedule.every %q is not a duration like 24h", s.Every)
		}
		if d < MinScheduleEvery {
			return fmt.Errorf("schedule.every must be at least %s", MinScheduleEvery)
		}
	}
	return nil
}

// Next is when the suite is next due. from is now. last is the start of the
// suite's last scheduled run (zero when it never ran); it only matters for
// every. An every schedule that is overdue is due at from.
func (s *Schedule) Next(from, last time.Time) (time.Time, bool) {
	if s == nil {
		return time.Time{}, false
	}
	if s.At != "" {
		m := atRe.FindStringSubmatch(s.At)
		if m == nil {
			return time.Time{}, false
		}
		var h, mi int
		fmt.Sscanf(m[1], "%d", &h)
		fmt.Sscanf(m[2], "%d", &mi)
		t := time.Date(from.Year(), from.Month(), from.Day(), h, mi, 0, 0, from.Location())
		if !t.After(from) {
			t = time.Date(from.Year(), from.Month(), from.Day()+1, h, mi, 0, 0, from.Location())
		}
		return t, true
	}
	d, err := time.ParseDuration(s.Every)
	if err != nil || d <= 0 {
		return time.Time{}, false
	}
	if last.IsZero() {
		return from.Add(d), true
	}
	t := last.Add(d)
	if t.Before(from) {
		t = from
	}
	return t, true
}

// SuiteItem is one entry of a suite: the id of a stored benchmark, or an inline benchmark.
type SuiteItem struct {
	Ref    string
	Inline *Benchmark
}

// MarshalJSON writes a reference as a string and an inline benchmark as an object.
func (i SuiteItem) MarshalJSON() ([]byte, error) {
	if i.Inline != nil {
		return json.Marshal(i.Inline)
	}
	return json.Marshal(i.Ref)
}

// UnmarshalJSON reads a string or an object.
func (i *SuiteItem) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		i.Inline = nil
		return json.Unmarshal(b, &i.Ref)
	}
	var bm Benchmark
	if err := DecodeStrict(b, &bm); err != nil {
		return err
	}
	i.Ref, i.Inline = "", &bm
	return nil
}

// UnmarshalYAML reads a scalar or a mapping.
func (i *SuiteItem) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		i.Inline = nil
		return n.Decode(&i.Ref)
	}
	raw, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var bm Benchmark
	if err := dec.Decode(&bm); err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	i.Ref, i.Inline = "", &bm
	return nil
}

// Suite is an ordered group of benchmarks that run together.
type Suite struct {
	ID            string      `json:"id" yaml:"id"`
	Name          string      `json:"name,omitempty" yaml:"name,omitempty"`
	Description   string      `json:"description,omitempty" yaml:"description,omitempty"`
	Benchmarks    []SuiteItem `json:"benchmarks" yaml:"benchmarks"`
	StopOnFailure bool        `json:"stop_on_failure,omitempty" yaml:"stop_on_failure,omitempty"`
	CooldownS     *int        `json:"cooldown_s,omitempty" yaml:"cooldown_s,omitempty"`
	OnlyWhenIdle  *bool       `json:"only_when_idle,omitempty" yaml:"only_when_idle,omitempty"`
	IdleWaitS     *int        `json:"idle_wait_s,omitempty" yaml:"idle_wait_s,omitempty"`
	Schedule      *Schedule   `json:"schedule,omitempty" yaml:"schedule,omitempty"`
	Baseline      string      `json:"baseline,omitempty" yaml:"baseline,omitempty"`
}

// Cooldown is the pause between benchmarks (default 5 s).
func (s *Suite) Cooldown() time.Duration {
	if s.CooldownS == nil {
		return DefaultCooldownS * time.Second
	}
	return time.Duration(*s.CooldownS) * time.Second
}

// WaitsForIdle is only_when_idle (default true).
func (s *Suite) WaitsForIdle() bool { return s.OnlyWhenIdle == nil || *s.OnlyWhenIdle }

// IdleWait is how long to wait for the scheduler to be idle (default 600 s).
func (s *Suite) IdleWait() time.Duration {
	if s.IdleWaitS == nil {
		return DefaultIdleWaitS * time.Second
	}
	return time.Duration(*s.IdleWaitS) * time.Second
}

// Title is the name, else the id.
func (s *Suite) Title() string {
	if s.Name != "" {
		return s.Name
	}
	return s.ID
}

// ParseSuiteJSON decodes a suite strictly. Call Validate next.
func ParseSuiteJSON(data []byte) (*Suite, error) {
	var s Suite
	if err := DecodeStrict(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// MarshalSuite is the stored JSON form of a suite.
func MarshalSuite(s *Suite) ([]byte, error) { return json.Marshal(s) }

// Lookup finds a stored benchmark by id.
type Lookup func(id string) (*Benchmark, bool)

// Validate checks a suite. References must resolve through look; inline
// benchmarks are validated and given the id "<suite>-<position>" when they
// have none. Parallel members must be non-parallel benchmarks.
func (s *Suite) Validate(profileOK func(string) bool, look Lookup) error {
	var errs []error
	add := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if !slugRe.MatchString(s.ID) {
		add("id %q must be 1 to 64 characters: lower-case letters, digits, '.', '-' or '_', starting with a letter or digit", s.ID)
	}
	if len(s.Name) > 120 {
		add("name is longer than 120 characters")
	}
	if s.Name == "" {
		s.Name = s.ID
	}
	if len(s.Description) > 2000 {
		add("description is longer than 2000 characters")
	}
	if len(s.Benchmarks) == 0 || len(s.Benchmarks) > MaxSuiteItems {
		add("benchmarks: between 1 and %d entries are required", MaxSuiteItems)
	}
	if s.CooldownS != nil && (*s.CooldownS < 0 || *s.CooldownS > 3600) {
		add("cooldown_s must be 0 to 3600")
	}
	if s.IdleWaitS != nil && (*s.IdleWaitS < 0 || *s.IdleWaitS > 86400) {
		add("idle_wait_s must be 0 to 86400")
	}
	if s.Schedule != nil {
		if err := s.Schedule.validate(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.Baseline != "" && len(s.Baseline) > 80 {
		add("baseline is not a run id")
	}
	known := map[string]*Benchmark{}
	resolve := func(id string) (*Benchmark, bool) {
		if b, ok := known[id]; ok {
			return b, true
		}
		if look == nil {
			return nil, false
		}
		return look(id)
	}
	for i := range s.Benchmarks {
		it := &s.Benchmarks[i]
		if it.Inline == nil {
			continue
		}
		if it.Inline.ID == "" {
			it.Inline.ID = fmt.Sprintf("%s-%d", s.ID, i+1)
		}
		if err := it.Inline.Validate(profileOK); err != nil {
			add("benchmarks[%d] (inline): %v", i, err)
		}
		known[it.Inline.ID] = it.Inline
	}
	for i, it := range s.Benchmarks {
		if it.Inline != nil {
			if it.Inline.IsParallel() {
				s.checkMembers(i, it.Inline, resolve, add)
			}
			continue
		}
		b, ok := resolve(it.Ref)
		if !ok {
			add("benchmarks[%d]: %q is not a benchmark", i, it.Ref)
			continue
		}
		if b.IsParallel() {
			s.checkMembers(i, b, resolve, add)
		}
	}
	return errors.Join(errs...)
}

func (s *Suite) checkMembers(i int, b *Benchmark, resolve Lookup, add func(string, ...any)) {
	for _, id := range b.Parallel {
		m, ok := resolve(id)
		switch {
		case !ok:
			add("benchmarks[%d]: parallel member %q is not a benchmark", i, id)
		case m.IsParallel():
			add("benchmarks[%d]: parallel member %q is itself a parallel step", i, id)
		}
	}
}

// Slug reports whether s is a valid benchmark, suite or tag id.
func Slug(s string) bool { return slugRe.MatchString(s) }

// FirstLine trims an error for a one-line message.
func FirstLine(err error) string {
	if err == nil {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(err.Error(), "\n", 2)[0])
}
