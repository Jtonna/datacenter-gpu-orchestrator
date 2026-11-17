package instance

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Real-format lines at llama.cpp 330f0189 (see loadlog.go for source lines).
const sampleLoadLog = `llama_model_loader: loaded meta data with 35 key-value pairs
load_tensors: offloaded 49/49 layers to GPU
load_tensors:   CPU_Mapped model buffer size =   360.00 MiB
load_tensors:  RPC0[10.0.0.2:50052] model buffer size =  4000.50 MiB
load_tensors:  RPC0[10.0.0.3:50052] model buffer size =  3000.25 MiB
load_tensors:        CUDA0 model buffer size =  1000.00 MiB
llama_context: n_ctx         = 8192
llama_kv_cache:  RPC0[10.0.0.2:50052] KV buffer size =   200.00 MiB
llama_kv_cache:  RPC0[10.0.0.3:50052] KV buffer size =   150.00 MiB
llama_kv_cache:      CUDA0 KV buffer size =    50.00 MiB
llama_kv_cache:      CUDA0 rotation buffer size =     1.00 MiB
llama_context:  CUDA_Host  output buffer size =     0.98 MiB
llama_context: CUDA0 compute buffer size is 266.0000 MiB, matches expectation of 266.0000 MiB
llama_context:      CUDA0 compute buffer size =   266.00 MiB
llama_context:  RPC0[10.0.0.2:50052] compute buffer size =   100.00 MiB
llama_context:  RPC0[10.0.0.3:50052] compute buffer size =    90.00 MiB
llama_context:  CUDA_Host compute buffer size =    20.00 MiB
`

func feedAll(l *LoadLog, s string) {
	for _, line := range strings.Split(s, "\n") {
		l.Feed(line)
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestLoadLogSample(t *testing.T) {
	l := NewLoadLog()
	feedAll(l, sampleLoadLog)
	s := l.Snapshot()

	want := map[string][3]float64{ // model, kv, compute
		"RPC0[10.0.0.2:50052]": {4000.50, 200, 100},
		"RPC0[10.0.0.3:50052]": {3000.25, 150, 90},
		"CUDA0":                {1000, 50, 266},
	}
	got := map[string]DeviceMem{}
	for _, d := range s.Devices {
		got[d.Name] = d
	}
	for name, w := range want {
		d, ok := got[name]
		if !ok {
			t.Fatalf("device %q missing; have %v", name, s.Devices)
		}
		if d.Host || !near(d.ModelMiB, w[0]) || !near(d.KVMiB, w[1]) || !near(d.ComputeMiB, w[2]) {
			t.Errorf("%s = %+v, want %v", name, d, w)
		}
	}
	if !near(got["CUDA0"].TotalMiB(), 1316) {
		t.Errorf("CUDA0 total = %v", got["CUDA0"].TotalMiB())
	}
	// grand sum counts devices only; host buffers are reported separately
	wantTotal := 4000.50 + 200 + 100 + 3000.25 + 150 + 90 + 1000 + 50 + 266
	if !near(s.TotalMiB(), wantTotal) {
		t.Errorf("TotalMiB = %v want %v", s.TotalMiB(), wantTotal)
	}
	if !near(s.HostMiB(), 360+20) {
		t.Errorf("HostMiB = %v want 380", s.HostMiB())
	}
	if len(s.Devices) != 5 { // 3 devices + CPU_Mapped + CUDA_Host
		t.Errorf("devices = %d: %+v", len(s.Devices), s.Devices)
	}
	// first-seen order is stable
	if s.Devices[0].Name != "CPU_Mapped" || s.Devices[1].Name != "RPC0[10.0.0.2:50052]" {
		t.Errorf("order: %+v", s.Devices)
	}
}

func TestLoadLogIgnoresNoise(t *testing.T) {
	l := NewLoadLog()
	for _, line := range []string{
		"", "   ", "garbage", "model buffer size =", "CUDA0 model buffer size = abc MiB",
		"CUDA0 model buffer size = 12.5 GiB", "\x00\x01\x02 binary", strings.Repeat("x", 100000),
		"CUDA0 output buffer size = 5.00 MiB", "CUDA0 rotation buffer size = 5.00 MiB",
		"CUDA0 compute buffer size is 5.0000 MiB, matches expectation of 5.0000 MiB",
		"CUDA0 compute buffer size of 5.0000 MiB, does not match expectation of 6.0000 MiB",
	} {
		l.Feed(line)
	}
	if s := l.Snapshot(); len(s.Devices) != 0 || s.TotalMiB() != 0 {
		t.Fatalf("expected nothing, got %+v", s)
	}
}

func TestLoadLogAccumulatesRepeatedKinds(t *testing.T) {
	// SWA models log two KV buffers per device.
	l := NewLoadLog()
	l.Feed("llama_kv_cache: CUDA0 KV buffer size = 100.00 MiB")
	l.Feed("llama_kv_cache: CUDA0 KV buffer size = 25.50 MiB")
	d := l.Snapshot().Devices[0]
	if !near(d.KVMiB, 125.5) {
		t.Fatalf("KV = %v", d.KVMiB)
	}
}

func TestLoadLogSnapshotIsACopy(t *testing.T) {
	l := NewLoadLog()
	l.Feed("CUDA0 model buffer size = 1.00 MiB")
	s := l.Snapshot()
	s.Devices[0].ModelMiB = 99
	l.Feed("CUDA0 model buffer size = 1.00 MiB")
	if got := l.Snapshot().Devices[0].ModelMiB; !near(got, 2) {
		t.Fatalf("got %v", got)
	}
}

func TestLineWriterSplitsAnywhere(t *testing.T) {
	text := sampleLoadLog
	for chunk := 1; chunk <= 40; chunk += 3 {
		var lines []string
		w := NewLineWriter(func(s string) { lines = append(lines, s) })
		for i := 0; i < len(text); i += chunk {
			end := i + chunk
			if end > len(text) {
				end = len(text)
			}
			w.Write([]byte(text[i:end]))
		}
		w.Close()
		want := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
		if strings.Join(lines, "|") != strings.Join(want, "|") {
			t.Fatalf("chunk %d: got %d lines, want %d", chunk, len(lines), len(want))
		}
	}
}

func TestLineWriterCRLFAndPartialTail(t *testing.T) {
	var lines []string
	w := NewLineWriter(func(s string) { lines = append(lines, s) })
	w.Write([]byte("one\r\ntwo\r\nthr"))
	if len(lines) != 2 || lines[0] != "one" || lines[1] != "two" {
		t.Fatalf("lines = %q", lines)
	}
	w.Close()
	if len(lines) != 3 || lines[2] != "thr" {
		t.Fatalf("after close lines = %q", lines)
	}
	w.Close() // idempotent
	if len(lines) != 3 {
		t.Fatalf("double close emitted: %q", lines)
	}
}

func TestLineWriterBoundsLongLines(t *testing.T) {
	var lines []string
	w := NewLineWriter(func(s string) { lines = append(lines, s) })
	w.Write([]byte(strings.Repeat("a", 3*maxLineBytes) + "\nok\n"))
	if len(lines) != 2 || len(lines[0]) > maxLineBytes || lines[1] != "ok" {
		t.Fatalf("got %d lines, first len %d", len(lines), len(lines[0]))
	}
}

// stdout and stderr feed one LoadLog through separate writers, concurrently.
func TestLoadLogInterleavedStreams(t *testing.T) {
	l := NewLoadLog()
	var wg sync.WaitGroup
	for s := 0; s < 4; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			w := NewLineWriter(l.Feed)
			for i := 0; i < 200; i++ {
				fmt.Fprintf(w, "noise %d\nllama_kv_cache: CUDA0 KV buffer size = 1.00 MiB\n", i)
			}
			w.Close()
		}(s)
	}
	wg.Wait()
	if got := l.Snapshot().Devices[0].KVMiB; !near(got, 800) {
		t.Fatalf("KV = %v want 800", got)
	}
}

// readFixture feeds a testdata log through a LineWriter in odd-sized chunks,
// as a child's pipe would deliver it.
func readFixture(t *testing.T, name string) LoadStats {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	l := NewLoadLog()
	w := NewLineWriter(l.Feed)
	for len(b) > 0 {
		n := min(37, len(b))
		w.Write(b[:n])
		b = b[n:]
	}
	w.Close()
	return l.Snapshot()
}

func devByName(s LoadStats, name string) (DeviceMem, bool) {
	for _, d := range s.Devices {
		if d.Name == name {
			return d, true
		}
	}
	return DeviceMem{}, false
}

// Real lines (with llama.cpp's "0.01.119.248 I " prefix) from the first
// hardware test, run with -lv 4.
func TestLoadLogRealFixturesWithTimestampPrefix(t *testing.T) {
	s := readFixture(t, "llama-lv4-ps5-aw-pool.log")
	a, ok1 := devByName(s, "RPC0[10.0.0.122:50052]")
	b, ok2 := devByName(s, "RPC0[10.0.0.3:50052]")
	c, ok3 := devByName(s, "CPU_Mapped")
	if !ok1 || !ok2 || !ok3 || !near(a.ModelMiB, 4257.94) || !near(b.ModelMiB, 2518.93) || !c.Host || !near(c.ModelMiB, 540) {
		t.Fatalf("devices = %+v", s.Devices)
	}
	if !near(s.TotalMiB(), 4257.94+2518.93) || !near(s.HostMiB(), 540) {
		t.Errorf("total %v host %v", s.TotalMiB(), s.HostMiB())
	}

	s = readFixture(t, "llama-lv4-ps5-single.log")
	if d, ok := devByName(s, "RPC0[10.0.0.101:50052]"); !ok || !near(d.ModelMiB, 6776.84) {
		t.Fatalf("devices = %+v", s.Devices)
	}

	// An OOM tail has no buffer-size lines and must not invent any.
	if s := readFixture(t, "llama-lv4-local-oom.log"); len(s.Devices) != 0 {
		t.Fatalf("devices from an error tail: %+v", s.Devices)
	}
}

// Model, KV (two sliding-window buffers per device) and compute lines
// together reproduce the figures /api/status showed for the run.
func TestLoadLogGemmaTwoDeviceFixture(t *testing.T) {
	s := readFixture(t, "llama-lv4-gemma-aw-local.log")
	rpc, _ := devByName(s, "RPC0[10.0.0.3:50052]")
	cuda, _ := devByName(s, "CUDA0")
	if !near(rpc.TotalMiB(), 3679.04) || !near(cuda.TotalMiB(), 4151.53) {
		t.Fatalf("rpc %+v cuda %+v", rpc, cuda)
	}
	if !near(s.TotalMiB(), 7830.57) || !near(s.HostMiB(), 540+113.11) {
		t.Errorf("total %v host %v", s.TotalMiB(), s.HostMiB())
	}
}

// At -lv 4 the child prints far more; one endless line without a newline
// must not grow the buffer, and the ring must stay at its size.
func TestLineWriterBoundedOnHugeLinesAndRingStaysBounded(t *testing.T) {
	var got []string
	w := NewLineWriter(func(s string) { got = append(got, s) })
	chunk := []byte(strings.Repeat("y", 1<<20))
	for i := 0; i < 20; i++ { // 20 MiB, no newline
		w.Write(chunk)
		if cap(w.buf) > 2*maxLineBytes {
			t.Fatalf("buffer grew to %d", cap(w.buf))
		}
	}
	w.Write([]byte("\nnext\n"))
	if len(got) != 2 || len(got[0]) != maxLineBytes || got[1] != "next" {
		t.Fatalf("got %d lines, first len %d", len(got), len(got[0]))
	}
	r := newRing(50)
	for i := 0; i < 100000; i++ {
		r.Add(fmt.Sprintf("print_info: line %d", i))
	}
	if len(r.Lines()) != 50 || r.Last(1)[0] != "print_info: line 99999" {
		t.Fatalf("ring = %d lines", len(r.Lines()))
	}
}
