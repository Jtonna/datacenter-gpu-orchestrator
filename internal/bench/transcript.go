package bench

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
)

// Transcripts (data model step 4): every step that sends prompts saves the
// full exchange as a file of its own, in a folder per run under the data
// directory: <data>/bench-runs/<run id>/step-NNN.json. What was sent, what
// came back, in order, with the timing of each request. Reports link to it;
// it is not part of a report.

// TranscriptFormat is the format id of a transcript file.
const TranscriptFormat = "potluck-transcript/1"

// RunsDirName is the folder under the data directory with a folder per run.
const RunsDirName = "bench-runs"

// TranscriptEntry is one request and its answer.
type TranscriptEntry struct {
	Seq      int    `json:"seq,omitempty"` // the request's seq in the step (not for tasks)
	Phase    string `json:"phase"`         // warmup, measured, conversation, hold, concurrent, tasks
	PhaseIdx int    `json:"phase_idx"`
	Turn     int    `json:"turn,omitempty"`
	Name     string `json:"name"`
	// StartedAt is unix ms.
	StartedAt int64          `json:"started_at"`
	Sampling  map[string]any `json:"sampling,omitempty"`
	// Messages is what was sent. For a later turn of a conversation only the
	// messages that are new since the previous entry of the phase (Continues):
	// the earlier ones are that entry's messages and answer.
	Messages         []benchspec.Message `json:"messages"`
	Continues        bool                `json:"continues,omitempty"`
	Status           int                 `json:"status,omitempty"`
	Error            string              `json:"error,omitempty"`
	Answer           string              `json:"answer,omitempty"`
	Reasoning        string              `json:"reasoning,omitempty"`
	FinishReason     string              `json:"finish_reason,omitempty"`
	PromptTokens     int                 `json:"prompt_tokens,omitempty"`
	CompletionTokens int                 `json:"completion_tokens,omitempty"`
	TTFTMS           *float64            `json:"ttft_ms,omitempty"`
	WallMS           float64             `json:"wall_ms,omitempty"`
	Timings          json.RawMessage     `json:"timings,omitempty"`
}

// Transcript is the file of one step.
type Transcript struct {
	Format      string            `json:"format"`
	RunID       string            `json:"run_id"`
	Step        int               `json:"step"`
	BenchmarkID string            `json:"benchmark_id"`
	Name        string            `json:"name"`
	Profile     string            `json:"profile"`
	Model       string            `json:"model,omitempty"`
	StartedAt   int64             `json:"started_at"`
	FinishedAt  int64             `json:"finished_at"`
	Entries     []TranscriptEntry `json:"entries"`
}

// TranscriptInfo lists a stored transcript.
type TranscriptInfo struct {
	Step    int   `json:"step"`
	Bytes   int64 `json:"bytes"`
	Entries int   `json:"entries"`
}

// transcriptEntry makes the entry of a request and its answer (resp may be
// nil after a transport error). conv is the number of messages of the phase
// that earlier entries already hold, or -1 outside a conversation.
func transcriptEntry(rr *RequestResult, r benchspec.Request, resp *ChatResponse, conv int) TranscriptEntry {
	e := TranscriptEntry{Seq: rr.Seq, Phase: rr.Phase, PhaseIdx: rr.PhaseIdx, Turn: rr.Turn, Name: r.Name, StartedAt: rr.StartedAt,
		Sampling: map[string]any{"max_tokens": r.MaxTok(), "temperature": r.Temp(), "seed": r.SeedVal(), "stream": r.Streams(), "cache_prompt": r.CachesPrompt()},
		Messages: r.ChatMessages(), Status: rr.Status, Error: rr.Error, TTFTMS: rr.TTFTMS, WallMS: rr.WallMS, Timings: rr.Timings}
	if conv > 0 && conv <= len(e.Messages) {
		e.Messages, e.Continues = e.Messages[conv:], true
	}
	if resp != nil {
		e.Answer, e.Reasoning, e.FinishReason = resp.Text, resp.Reasoning, resp.FinishReason
		e.PromptTokens, e.CompletionTokens = resp.PromptTokens, resp.CompletionTokens
		if e.Status == 0 {
			e.Status = resp.Status
		}
	}
	return e
}

// addTranscript keeps an entry for the step's transcript.
func (br *benchRun) addTranscript(e TranscriptEntry) {
	br.mu.Lock()
	br.transcript = append(br.transcript, e)
	br.mu.Unlock()
}

// convSeen is how many messages of conversation phase pidx the transcript
// already holds (-1 when it is not a later turn).
func (br *benchRun) convSeen(phase string, pidx, turn int) int {
	if phase != PhaseConversation || turn <= 1 {
		return -1
	}
	br.mu.Lock()
	defer br.mu.Unlock()
	return br.conv[pidx]
}

// convAdd notes that a conversation turn's messages and answer are held.
func (br *benchRun) convAdd(phase string, pidx int, sent int) {
	if phase != PhaseConversation {
		return
	}
	br.mu.Lock()
	if br.conv == nil {
		br.conv = map[int]int{}
	}
	br.conv[pidx] = sent + 1 // the messages sent and the answer
	br.mu.Unlock()
}

// saveTranscript writes the step's transcript when it sent anything.
func (x *suiteExec) saveTranscript(br *benchRun) {
	br.mu.Lock()
	entries := append([]TranscriptEntry(nil), br.transcript...)
	br.mu.Unlock()
	if len(entries) == 0 {
		return
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].StartedAt < entries[j].StartedAt })
	st := br.st
	t := &Transcript{Format: TranscriptFormat, RunID: st.RunID, Step: st.Idx, BenchmarkID: st.BenchmarkID, Name: st.Name, Profile: st.Profile,
		Model: st.ModelFile, StartedAt: st.StartedAt, FinishedAt: st.FinishedAt, Entries: entries}
	if err := x.m.o.Store.WriteTranscript(t); err != nil {
		x.m.log.Warn("cannot save a step's transcript", "run", st.RunID, "step", st.Idx, "err", err)
	}
}

// ---- files ----

var safeRunID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// DataDir is the directory of the database.
func (s *Store) DataDir() string { return filepath.Dir(s.path) }

// RunDir is the folder of a run's files (transcripts).
func (s *Store) RunDir(runID string) (string, error) {
	if !safeRunID.MatchString(runID) || strings.Contains(runID, "..") {
		return "", fmt.Errorf("%w: run id %q", ErrNotFound, runID)
	}
	return filepath.Join(s.DataDir(), RunsDirName, runID), nil
}

func transcriptName(step int) string { return fmt.Sprintf("step-%03d.json", step) }

// WriteTranscript stores a step's transcript (written to a temporary file
// and renamed, so a reader never sees half a file).
func (s *Store) WriteTranscript(t *Transcript) error {
	dir, err := s.RunDir(t.RunID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, transcriptName(t.Step))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadTranscript is a step's transcript file as stored.
func (s *Store) ReadTranscript(runID string, step int) ([]byte, error) {
	dir, err := s.RunDir(runID)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, transcriptName(step)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: no transcript for step %d of run %s (the step sent no prompt, or the run is older than transcripts)", ErrNotFound, step, runID)
	}
	return b, err
}

// Transcripts lists a run's transcripts, by step.
func (s *Store) Transcripts(runID string) ([]TranscriptInfo, error) {
	dir, err := s.RunDir(runID)
	if err != nil {
		return nil, err
	}
	des, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []TranscriptInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []TranscriptInfo{}
	for _, de := range des {
		name := de.Name()
		if !strings.HasPrefix(name, "step-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "step-"), ".json"))
		if err != nil {
			continue
		}
		info := TranscriptInfo{Step: n}
		if fi, err := de.Info(); err == nil {
			info.Bytes = fi.Size()
		}
		if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			var t struct {
				Entries []json.RawMessage `json:"entries"`
			}
			if json.Unmarshal(b, &t) == nil {
				info.Entries = len(t.Entries)
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Step < out[j].Step })
	return out, nil
}

// removeRunFiles deletes a run's folder.
func (s *Store) removeRunFiles(runID string) error {
	dir, err := s.RunDir(runID)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// TranscriptMarkdown renders a transcript as a readable chat.
func TranscriptMarkdown(t *Transcript) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Transcript: %s\n\nRun `%s`, step %d (`%s`), profile `%s`", t.Name, t.RunID, t.Step+1, t.BenchmarkID, t.Profile)
	if t.Model != "" {
		fmt.Fprintf(&b, ", model `%s`", t.Model)
	}
	b.WriteString(".\n")
	for k, e := range t.Entries {
		fmt.Fprintf(&b, "\n## %d. %s, phase %d (%s)", k+1, e.Name, e.PhaseIdx+1, e.Phase)
		if e.Turn > 0 {
			fmt.Fprintf(&b, ", turn %d", e.Turn)
		}
		b.WriteString("\n\n")
		var facts []string
		if e.Status != 0 {
			facts = append(facts, fmt.Sprintf("status %d", e.Status))
		}
		if e.TTFTMS != nil {
			facts = append(facts, fmt.Sprintf("first token %.0f ms", *e.TTFTMS))
		}
		if e.WallMS > 0 {
			facts = append(facts, fmt.Sprintf("wall %.0f ms", e.WallMS))
		}
		if e.CompletionTokens > 0 {
			facts = append(facts, fmt.Sprintf("%d tokens out", e.CompletionTokens))
		}
		if e.FinishReason != "" {
			facts = append(facts, "finish "+e.FinishReason)
		}
		if len(facts) > 0 {
			b.WriteString("_" + strings.Join(facts, ", ") + "_\n\n")
		}
		if e.Continues {
			b.WriteString("_(continues the previous turn)_\n\n")
		}
		for _, m := range e.Messages {
			fmt.Fprintf(&b, "**%s:**\n\n%s\n\n", m.Role, quoteBlock(m.Content))
		}
		if e.Reasoning != "" {
			fmt.Fprintf(&b, "**reasoning:**\n\n%s\n\n", quoteBlock(e.Reasoning))
		}
		if e.Error != "" {
			fmt.Fprintf(&b, "**error:** %s\n\n", e.Error)
		} else {
			fmt.Fprintf(&b, "**assistant:**\n\n%s\n\n", quoteBlock(e.Answer))
		}
	}
	return b.String()
}

// quoteBlock puts text in a Markdown block quote so headings inside it stay text.
func quoteBlock(s string) string {
	if s == "" {
		return "> (empty)"
	}
	return "> " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n> ")
}
