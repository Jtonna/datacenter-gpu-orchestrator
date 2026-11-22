package benchspec

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func okAll(string) bool { return true }

func validate(t *testing.T, js string) error {
	t.Helper()
	b, err := ParseBenchmarkJSON([]byte(js))
	if err != nil {
		return err
	}
	return b.Validate(okAll)
}

func phaseTypes(ps []Phase) string {
	var s []string
	for _, p := range ps {
		s = append(s, p.Type)
	}
	return strings.Join(s, ",")
}

func TestShorthandExpandsToPhases(t *testing.T) {
	b := mustBench(t, okBench)
	if err := b.Validate(okAll); err != nil {
		t.Fatal(err)
	}
	ps := b.Expand()
	if phaseTypes(ps) != "load,warmup,requests,unload" {
		t.Fatalf("%s", phaseTypes(ps))
	}
	if ps[1].WarmupCount() != 1 || ps[1].Request == nil || ps[1].Request.Name != "short" || ps[2].RepeatCount() != 1 {
		t.Fatalf("%+v", ps)
	}
	b = mustBench(t, `{"id":"x","profile":"p","cold":false,"warmup_requests":0,"unload_after":false,"repeats":3,"requests":[{"prompt":"hi"}]}`)
	if err := b.Validate(okAll); err != nil {
		t.Fatal(err)
	}
	ps = b.Expand()
	if phaseTypes(ps) != "requests" || ps[0].RepeatCount() != 3 {
		t.Fatalf("%s %+v", phaseTypes(ps), ps)
	}
	if got := (&Benchmark{Parallel: []string{"a", "b"}}).Expand(); got != nil {
		t.Fatal("a parallel step has no phases")
	}
}

func TestPhasesValidation(t *testing.T) {
	good := map[string]string{
		"load only":    `{"id":"a","profile":"p","phases":[{"type":"load"},{"type":"unload"}]}`,
		"conversation": `{"id":"a","profile":"p","phases":[{"type":"load"},{"type":"conversation","system":"be brief","stream":true,"max_tokens":50,"turns":[{"content":"hi"},{"content":"more","max_tokens":10}]}]}`,
		"hold":         `{"id":"a","profile":"p","phases":[{"type":"requests","requests":[{"prompt":"x"}]},{"type":"hold","seconds":30}]}`,
		"hold own req": `{"id":"a","profile":"p","phases":[{"type":"hold","seconds":30,"request":{"prompt":"x"}}]}`,
		"concurrent":   `{"id":"a","profile":"p","phases":[{"type":"concurrent_requests","n":3,"request":{"prompt":"x"}}]}`,
		"warmup":       `{"id":"a","profile":"p","phases":[{"type":"warmup","count":2,"request":{"prompt":"x"}},{"type":"requests","requests":[{"prompt":"x"}]}]}`,
		"labels":       `{"id":"a","description":"d","profile":"p","phases":[{"type":"load","label":"cold load"}]}`,
	}
	for name, js := range good {
		if err := validate(t, js); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := map[string]struct{ js, want string }{
		"unknown type":      {`{"id":"a","profile":"p","phases":[{"type":"dance"}]}`, `type "dance" is not one of`},
		"missing type":      {`{"id":"a","profile":"p","phases":[{}]}`, "is not one of"},
		"mixes forms":       {`{"id":"a","profile":"p","repeats":2,"phases":[{"type":"load"}]}`, "phases replaces"},
		"mixes requests":    {`{"id":"a","profile":"p","requests":[{"prompt":"x"}],"phases":[{"type":"load"}]}`, "phases replaces"},
		"option on load":    {`{"id":"a","profile":"p","phases":[{"type":"load","seconds":3}]}`, `option "seconds" does not belong`},
		"turns on requests": {`{"id":"a","profile":"p","phases":[{"type":"requests","requests":[{"prompt":"x"}],"turns":[{"content":"x"}]}]}`, `option "turns" does not belong`},
		"requests empty":    {`{"id":"a","profile":"p","phases":[{"type":"requests"}]}`, "requests: between 1 and"},
		"request no prompt": {`{"id":"a","profile":"p","phases":[{"type":"requests","requests":[{"name":"q"}]}]}`, "messages or prompt is required"},
		"no turns":          {`{"id":"a","profile":"p","phases":[{"type":"conversation"}]}`, "turns: between 1 and"},
		"empty turn":        {`{"id":"a","profile":"p","phases":[{"type":"conversation","turns":[{"content":" "}]}]}`, "turns[0].content is empty"},
		"hold too long":     {`{"id":"a","profile":"p","phases":[{"type":"hold","seconds":99999,"request":{"prompt":"x"}}]}`, "seconds must be 1 to 3600"},
		"hold no request":   {`{"id":"a","profile":"p","phases":[{"type":"hold","seconds":5}]}`, "request is required here"},
		"concurrent n 1":    {`{"id":"a","profile":"p","phases":[{"type":"concurrent_requests","n":1,"request":{"prompt":"x"}}]}`, "n must be 2 to 16"},
		"concurrent n big":  {`{"id":"a","profile":"p","phases":[{"type":"concurrent_requests","n":99,"request":{"prompt":"x"}}]}`, "n must be 2 to 16"},
		"warmup too many":   {`{"id":"a","profile":"p","phases":[{"type":"warmup","count":99,"request":{"prompt":"x"}}]}`, "count must be 0 to 10"},
		"bad temperature":   {`{"id":"a","profile":"p","phases":[{"type":"requests","temperature":5,"requests":[{"prompt":"x"}]}]}`, "temperature must be 0 to 2"},
		"unknown field":     {`{"id":"a","profile":"p","phases":[{"type":"load","bogus":1}]}`, "unknown field"},
		"long description":  {`{"id":"a","profile":"p","description":"` + strings.Repeat("x", 2001) + `","phases":[{"type":"load"}]}`, "description is longer"},
	}
	for name, tc := range bad {
		err := validate(t, tc.js)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", name, err, tc.want)
		}
	}
	var many []string
	for i := 0; i < MaxPhases+1; i++ {
		many = append(many, `{"type":"load"}`)
	}
	if err := validate(t, `{"id":"a","profile":"p","phases":[`+strings.Join(many, ",")+`]}`); err == nil || !strings.Contains(err.Error(), "at most 32") {
		t.Errorf("too many phases: %v", err)
	}
}

func TestExpectKeysNeedTheirPhases(t *testing.T) {
	bad := map[string]struct{ js, want string }{
		"ttft without stream": {`{"id":"a","profile":"p","expect":{"max_ttft_ms":500},"phases":[{"type":"requests","requests":[{"prompt":"x"}]}]}`, "max_ttft_ms needs a phase that sets stream"},
		"unload time":         {`{"id":"a","profile":"p","expect":{"max_unload_s":5},"phases":[{"type":"load"}]}`, "max_unload_s needs an unload phase"},
		"reuse":               {`{"id":"a","profile":"p","expect":{"min_reused_tokens":5},"phases":[{"type":"load"}]}`, "needs a conversation phase"},
		"concurrent wall":     {`{"id":"a","profile":"p","expect":{"max_concurrent_wall_s":5},"phases":[{"type":"load"}]}`, "needs a concurrent_requests phase"},
		"decode on load only": {`{"id":"a","profile":"p","expect":{"min_decode_tps":5},"phases":[{"type":"load"}]}`, "min_decode_tps needs"},
		"hash on conv":        {`{"id":"a","profile":"p","expect":{"output_sha256":"` + strings.Repeat("a", 64) + `"},"phases":[{"type":"conversation","turns":[{"content":"x"}]}]}`, "needs a requests phase"},
		"metric on refusal":   {`{"id":"a","profile":"p","expect":{"outcome":"refused","max_ttft_ms":5},"phases":[{"type":"requests","stream":true,"requests":[{"prompt":"x"}]}]}`, "only make sense when the outcome is ready"},
		"negative":            {`{"id":"a","profile":"p","expect":{"max_errors":-1},"phases":[{"type":"concurrent_requests","request":{"prompt":"x"}}]}`, "max_errors must not be negative"},
	}
	for name, tc := range bad {
		if err := validate(t, tc.js); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", name, err, tc.want)
		}
	}
	ok := `{"id":"a","profile":"p","expect":{"max_load_s":120,"min_decode_tps":3,"min_prefill_tps":10,"max_ttft_ms":900,"max_unload_s":10,"min_reused_tokens":8,"max_concurrent_wall_s":60,"max_errors":0},
	 "phases":[{"type":"load"},{"type":"requests","stream":true,"requests":[{"prompt":"x"}]},{"type":"conversation","turns":[{"content":"x"}]},{"type":"concurrent_requests","request":{"prompt":"x"}},{"type":"unload"}]}`
	if err := validate(t, ok); err != nil {
		t.Fatal(err)
	}
}

func TestPhaseDefaultsAndRequestBorrowing(t *testing.T) {
	b := mustBench(t, `{"id":"a","profile":"p","phases":[{"type":"requests","max_tokens":33,"temperature":0.5,"stream":true,"requests":[{"name":"q","prompt":"x"},{"name":"r","prompt":"y","max_tokens":7}]},
	  {"type":"hold","seconds":2},{"type":"conversation","turns":[{"content":"a"},{"content":"b","max_tokens":5}]}]}`)
	if err := b.Validate(okAll); err != nil {
		t.Fatal(err)
	}
	ps := b.Expand()
	rs := ps[0].PhaseRequests()
	if rs[0].MaxTok() != 33 || rs[1].MaxTok() != 7 || rs[0].Temp() != 0.5 || !rs[0].Streams() {
		t.Fatalf("%+v", rs)
	}
	r, ok := b.RequestOf(&ps[1])
	if !ok || r.Name != "q" || r.MaxTok() != 33 {
		t.Fatalf("hold borrows the first request with the phase defaults: %+v", r)
	}
	if !ps[2].CachesConversation() {
		t.Fatal("conversations cache the prompt by default")
	}
	cr := ps[2].ConversationRequest(ps[2].Turns[1])
	if cr.MaxTok() != 5 || !cr.CachesPrompt() || ps[2].Turns[0].Name != "turn-1" {
		t.Fatalf("%+v %+v", cr, ps[2].Turns)
	}
}

func TestPhasesYAML(t *testing.T) {
	var b Benchmark
	doc := `
id: conv
profile: p
description: talk
phases:
  - type: load
  - type: conversation
    turns:
      - content: hi
      - content: again
  - type: unload
`
	dec := yaml.NewDecoder(strings.NewReader(doc))
	dec.KnownFields(true)
	if err := dec.Decode(&b); err != nil {
		t.Fatal(err)
	}
	if err := b.Validate(okAll); err != nil {
		t.Fatal(err)
	}
	if phaseTypes(b.Phases) != "load,conversation,unload" || len(b.Phases[1].Turns) != 2 {
		t.Fatalf("%+v", b.Phases)
	}
}

func TestSchemaDescribesEveryPhaseAndTemplatesAreValid(t *testing.T) {
	d := Schema()
	if len(d.PhaseTypes) != len(PhaseTypes) {
		t.Fatalf("%d phase types", len(d.PhaseTypes))
	}
	for i, p := range d.PhaseTypes {
		if p.Type != PhaseTypes[i] || p.Description == "" || p.Title == "" || len(p.Records) == 0 {
			t.Errorf("phase %+v", p)
		}
		for _, o := range p.Options {
			if o.Name == "" || o.Type == "" || o.Description == "" {
				t.Errorf("%s option %+v", p.Type, o)
			}
		}
		// every option the validator accepts is described
		have := map[string]bool{}
		for _, o := range p.Options {
			have[o.Name] = true
		}
		for _, n := range allowed[p.Type] {
			if !have[n] {
				t.Errorf("phase %s: option %q is accepted but not described", p.Type, n)
			}
		}
	}
	for _, k := range d.Expect {
		if k.Name == "" || k.Description == "" {
			t.Errorf("%+v", k)
		}
	}
	if len(d.Templates) != 6 {
		t.Fatalf("%d templates", len(d.Templates))
	}
	for _, tp := range d.Templates {
		b, err := ParseBenchmarkJSON(tp.Benchmark)
		if err != nil {
			t.Errorf("%s: %v", tp.ID, err)
			continue
		}
		if err := b.Validate(okAll); err != nil {
			t.Errorf("template %s does not validate: %v", tp.ID, err)
		}
		if tp.Name == "" || tp.Description == "" {
			t.Errorf("%+v", tp)
		}
	}
}

func TestHoldsMustFitInTheTimeout(t *testing.T) {
	err := validate(t, `{"id":"a","profile":"p","phases":[{"type":"hold","seconds":900,"request":{"prompt":"x"}}]}`)
	if err == nil || !strings.Contains(err.Error(), "does not fit in timeout_s (600 s): raise timeout_s to at least 930") {
		t.Fatalf("%v", err)
	}
	if err := validate(t, `{"id":"a","profile":"p","timeout_s":1000,"phases":[{"type":"hold","seconds":900,"request":{"prompt":"x"}}]}`); err != nil {
		t.Fatal(err)
	}
}
