package bench

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
)

func fakeSchedulerServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/plan", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"model":"m","action":"start","reason":"","placement":[{"id":"aa:01","addr":"10.0.0.2:50052"}],"need_mib":7900}`)
	})
	mux.HandleFunc("POST /api/unload", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"msg":"ok","model":"m"}`)
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"time":"2026-11-18T10:00:00.000Z","instances":[{"profile":"m","state":"starting","in_flight":0,"waiters":1,"log_file":"/var/log/llm-children/x.log",
		 "device_total_mib":7900.5,"nodes":[{"id":"aa:01","memory":[{"total_mib":7000},{"total_mib":900.5}]}],
		 "load":{"session_id":"ld-1-1","elapsed_s":4.5,"bytes_total":1000,"bytes_sent":500,"progress":0.5,"eta_s":4,"workers":[]}}],
		 "queue":[{"profile":"x"}],
		 "recent_loads":[{"session_id":"ld-0-1","model":"m","outcome":"ready","reason":"","finished_at":1795055906526,"load_s":66.1,"workers":["10.0.0.2:50052"]}],
		 "view":{"nodes":[{"id":"aa:01","hostname":"ps5-a"}]}}`)
	})
	mux.HandleFunc("GET /api/history", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.RawQuery)
		fmt.Fprint(w, `{"events":[{"type":"loadFinished","data":{"load_s":66.1,"bytes":6700000000,"avg_mb_s":101.4,"nodes_detail":[{"node":"ps5-a","bytes":6700000000,"memory_mib":7900.5}]}}],"history":"on"}`)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, "chat run="+r.Header.Get("X-Bench-Run"))
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		body := string(b[:n])
		switch {
		case strings.Contains(body, `"model":"refuse"`):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(503)
			fmt.Fprint(w, `{"error":{"message":"does not fit: profile refuse needs 99999 MiB","type":"server_error","code":503,"reason":"does not fit"}}`)
		case strings.Contains(body, `"stream":true`):
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			for i := 0; i < 3; i++ {
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"tok%d \"}}]}\n\n", i)
				fl.Flush()
				time.Sleep(5 * time.Millisecond)
			}
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"timings\":{\"prompt_n\":10,\"predicted_n\":3,\"predicted_per_second\":42.5}}\n\ndata: [DONE]\n\n")
		default:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"hello there"},"finish_reason":"stop"}],"timings":{"prompt_n":10,"prompt_ms":5,"prompt_per_second":2000,"predicted_n":3,"predicted_ms":30,"predicted_per_second":100,"cache_n":0}}`)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestHTTPAPIAgainstTheSchedulerShapes(t *testing.T) {
	srv, seen := fakeSchedulerServer(t)
	api := &HTTPAPI{Base: srv.URL}
	ctx := context.Background()
	p, err := api.Plan(ctx, "m")
	if err != nil || p.Action != "start" || len(p.Raw) == 0 {
		t.Fatalf("%+v %v", p, err)
	}
	u, err := api.Unload(ctx, "m", false)
	if err != nil || u.Status != 200 || u.Msg != "ok" {
		t.Fatalf("%+v %v", u, err)
	}
	st, err := api.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	in := st.Instance("m")
	if in == nil || in.LogFile == "" || in.Load == nil || in.Load.SessionID != "ld-1-1" || *in.Load.BytesSent != 500 || in.DeviceTotalMiB != 7900.5 ||
		in.Nodes[0].MemoryMiB != 7900.5 || st.QueueLen != 1 || st.Hostnames["aa:01"] != "ps5-a" || st.RecentLoads[0].LoadS != 66.1 || len(in.Load.Raw) == 0 {
		t.Fatalf("%+v", in)
	}
	h, err := api.LoadHistory(ctx, "ld-0-1")
	if err != nil || h.LoadS != 66.1 || h.Bytes != 6700000000 || h.AvgMBs != 101.4 || h.Nodes[0].Node != "ps5-a" || *h.Nodes[0].MemoryMiB != 7900.5 {
		t.Fatalf("%+v %v", h, err)
	}
	if !strings.Contains((*seen)[0], "session_id=ld-0-1") || !strings.Contains((*seen)[0], "type=loadFinished") {
		t.Fatalf("history query %v", *seen)
	}

	spec := benchspec.Request{Prompt: "hi", MaxTokens: 8}
	cr, err := api.Chat(ctx, "run-1", BuildChatRequest("m", spec), nil)
	if err != nil || cr.Status != 200 || cr.Text != "hello there" || cr.TTFTMS != nil || len(cr.Timings) == 0 {
		t.Fatalf("%+v %v", cr, err)
	}
	if string(cr.Message) != `{"content":"hello there","role":"assistant"}` { // canonical: sorted keys
		t.Fatalf("message %s", cr.Message)
	}
	if HashMessage(cr.Message) == "" || (*seen)[len(*seen)-1] != "chat run=run-1" {
		t.Fatalf("seen %v", *seen)
	}

	stream := true
	spec.Stream = &stream
	var progress []ChatProgress
	cr, err = api.Chat(ctx, "run-1", BuildChatRequest("m", spec), func(p ChatProgress) { progress = append(progress, p) })
	if err != nil || cr.Status != 200 || cr.Text != "tok0 tok1 tok2 " || cr.TTFTMS == nil || *cr.TTFTMS < 0 || len(progress) == 0 || progress[len(progress)-1].Tokens != 3 {
		t.Fatalf("%+v %v %v", cr, err, progress)
	}
	if !strings.Contains(string(cr.Timings), "42.5") || string(cr.Message) != `{"content":"tok0 tok1 tok2 ","role":"assistant"}` {
		t.Fatalf("%s %s", cr.Timings, cr.Message)
	}

	cr, err = api.Chat(ctx, "run-1", BuildChatRequest("refuse", benchspec.Request{Prompt: "hi"}), nil)
	if err != nil || cr.Status != 503 || cr.ErrReason != "does not fit" || !strings.Contains(cr.ErrMessage, "99999") {
		t.Fatalf("%+v %v", cr, err)
	}
}

func TestBuildChatRequestBody(t *testing.T) {
	temp, seed, cache := 0.7, int64(7), true
	b := BuildChatRequest("m", benchspec.Request{Prompt: "hi", MaxTokens: 16, Temperature: &temp, Seed: &seed, CachePrompt: &cache})
	s := string(b.Body)
	for _, want := range []string{`"model":"m"`, `"max_tokens":16`, `"temperature":0.7`, `"seed":7`, `"cache_prompt":true`, `"stream":false`, `"messages":[{"role":"user","content":"hi"}]`} {
		if !strings.Contains(s, want) {
			t.Errorf("%s lacks %s", s, want)
		}
	}
}

func TestHead(t *testing.T) {
	if Head("héllo", 3) != "hél" || Head("ab", 400) != "ab" {
		t.Fatal("Head")
	}
}
