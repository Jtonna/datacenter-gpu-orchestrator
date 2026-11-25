package bench

import (
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
)

// thinksFirst is a model that thinks before it answers unless the request
// turns thinking off with chat_template_kwargs: then its whole max_tokens
// budget goes to reasoning_content and the answer is cut ("length").
func thinksFirst(body string) fakeReply {
	if !strings.Contains(body, `"chat_template_kwargs":{"enable_thinking":false}`) {
		return fakeReply{reasoning: strings.Repeat("Let me think about the harbour codes. ", 20), finish: "length"}
	}
	return recallingFill(body)
}

// A model that thinks first gives the planted facts back when the fill
// turns thinking off on its own requests; the profile is not changed.
func TestContextFill_ThinkingModelAnswersWithThinkingOff(t *testing.T) {
	api := newFake().prof("small", fakeProf{chatFn: thinksFirst})
	tm := fillMgr(t, api)
	tm.bench(`{"id":"fill","profile":"small","phases":[{"type":"load"},{"type":"context_fill","marks_pct":[50,90]},{"type":"unload"}]}`)
	tm.suite(`{"id":"s","benchmarks":["fill"],"cooldown_s":0,"only_when_idle":false}`)
	r := tm.finished(tm.run("s").ID)
	if r.Steps[0].Outcome != OutcomeReady {
		t.Fatalf("%s %q", r.Steps[0].Outcome, r.Steps[0].Reason)
	}
	f := fillOf(t, r, 0)
	if f.RecallCorrect == nil || *f.RecallCorrect != f.Recall || f.Recall == 0 {
		t.Fatalf("recall %v of %d, answer %q", f.RecallCorrect, f.Recall, f.Answer)
	}
	if f.FinishReason != "stop" || f.ReasoningChars != 0 || f.RecallNote != "" {
		t.Fatalf("finish %q reasoning %d note %q", f.FinishReason, f.ReasoningChars, f.RecallNote)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	turns := 0
	for _, b := range api.bodies {
		if strings.Contains(b, "harbour") || strings.Contains(b, "Reply with OK") {
			turns++
			if !strings.Contains(b, `"chat_template_kwargs":{"enable_thinking":false}`) {
				t.Fatalf("a fill turn without thinking off: %.200s", b)
			}
		}
	}
	if turns != 2 {
		t.Fatalf("%d fill turns", turns)
	}
	if len(api.made) != 0 {
		t.Fatalf("a profile was made or changed: %v", api.made)
	}
}

// A recipe that asks for thinking keeps it: the fill sends no thinking
// setting, and a recall cut while thinking says so instead of a bare 0.
func TestContextFill_ThinkingAskedIsKeptAndACutAnswerSaysWhy(t *testing.T) {
	api := newFake()
	api.consoles = consoles()
	think := true
	id := benchspec.Inputs{Model: "q.gguf", Ctx: 4096, Thinking: &think}.ProfileID()
	api.prof(id, fakeProf{chatFn: thinksFirst, modelFile: "q.gguf"})
	tm := fillMgr(t, api)
	tm.bench(`{"id":"fillr","name":"Fill","inputs":{"ctx":4096},"phases":[{"type":"load"},{"type":"context_fill","marks_pct":[50]},{"type":"unload"}]}`)
	tm.suite(`{"id":"s","cooldown_s":0,"only_when_idle":false,"benchmarks":[{"benchmark":"fillr","inputs":{"model":"q.gguf","thinking":true}}]}`)
	r := tm.finished(tm.run("s").ID)
	if r.Steps[0].Profile != id {
		t.Fatalf("profile %s, want %s", r.Steps[0].Profile, id)
	}
	if r.Steps[0].Outcome != OutcomeReady {
		t.Fatalf("%s %q", r.Steps[0].Outcome, r.Steps[0].Reason)
	}
	f := fillOf(t, r, 0)
	if f.RecallCorrect == nil || *f.RecallCorrect != 0 || f.FinishReason != "length" || f.ReasoningChars == 0 ||
		!strings.Contains(f.RecallNote, "cut at max_tokens 128 while the model was thinking") {
		t.Fatalf("recall %v finish %q reasoning %d note %q", f.RecallCorrect, f.FinishReason, f.ReasoningChars, f.RecallNote)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, b := range api.bodies {
		if strings.Contains(b, "chat_template_kwargs") {
			t.Fatalf("thinking was asked, yet the fill sent a thinking setting: %.200s", b)
		}
	}
}
