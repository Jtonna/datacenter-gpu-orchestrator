package bench

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every step that sends prompts saves its exchange as a file of its own:
// requests, conversation turns (each turn holds only what is new), tasks.
// A step that sends nothing has no transcript.
func TestStepTranscripts(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	tm := newTestMgr(t, api)
	tm.bench(`{"id":"talk","profile":"gemma","phases":[{"type":"load"},
		{"type":"requests","repeats":2,"max_tokens":16,"requests":[{"name":"hi","prompt":"Say hi."}]},
		{"type":"conversation","system":"Be brief.","turns":[{"content":"One?"},{"content":"Two?"}]},
		{"type":"tasks","tasks":{"id":"t","version":1,"tasks":[{"id":"fib","prompt":"Write fib."}]}},
		{"type":"unload"}]}`)
	tm.bench(`{"id":"lo","profile":"gemma","phases":[{"type":"load"},{"type":"unload"}]}`)
	tm.suite(`{"id":"s","cooldown_s":0,"only_when_idle":false,"benchmarks":["talk","lo"]}`)
	r := tm.finished(tm.run("s").ID)
	if r.State != RunCompleted {
		t.Fatalf("%s %s", r.State, r.Error)
	}

	list, err := tm.store.Transcripts(r.ID)
	if err != nil || len(list) != 1 || list[0].Step != 0 || list[0].Entries != 5 || list[0].Bytes == 0 {
		t.Fatalf("list %v %+v", err, list)
	}
	raw, err := tm.store.ReadTranscript(r.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var tr Transcript
	if err := json.Unmarshal(raw, &tr); err != nil {
		t.Fatal(err)
	}
	if tr.Format != TranscriptFormat || tr.RunID != r.ID || tr.BenchmarkID != "talk" || tr.Profile != "gemma" {
		t.Fatalf("%+v", tr)
	}
	var phases []string
	for _, e := range tr.Entries {
		phases = append(phases, e.Phase)
	}
	if strings.Join(phases, ",") != "measured,measured,conversation,conversation,tasks" {
		t.Fatalf("phases %v", phases)
	}
	var turns []TranscriptEntry
	for _, e := range tr.Entries {
		if e.Phase == PhaseConversation {
			turns = append(turns, e)
		}
		if e.Phase != "tasks" && (e.Answer == "" || e.Status != 200 || e.Sampling["temperature"] == nil) {
			t.Errorf("entry %+v", e)
		}
	}
	if len(turns) != 2 || len(turns[0].Messages) != 2 || turns[0].Messages[0].Role != "system" || turns[0].Continues ||
		len(turns[1].Messages) != 1 || turns[1].Messages[0].Content != "Two?" || !turns[1].Continues {
		t.Fatalf("turns %+v", turns)
	}
	last := tr.Entries[len(tr.Entries)-1]
	if last.Phase != "tasks" || last.Name != "fib" || last.PhaseIdx != 3 || last.Messages[len(last.Messages)-1].Content != "Write fib." || last.Answer == "" {
		t.Fatalf("task entry %+v", last)
	}
	md := TranscriptMarkdown(&tr)
	if !strings.Contains(md, "# Transcript: talk") || !strings.Contains(md, "(continues the previous turn)") || !strings.Contains(md, "> Write fib.") {
		t.Fatalf("%s", md)
	}

	if _, err := tm.store.ReadTranscript(r.ID, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a load-only step: %v", err)
	}
	if _, err := tm.store.ReadTranscript("../x", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad id: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tm.store.DataDir(), RunsDirName, r.ID, "step-000.json")); err != nil {
		t.Fatal(err)
	}
}
