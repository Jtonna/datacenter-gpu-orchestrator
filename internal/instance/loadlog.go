package instance

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// maxLineBytes bounds one buffered output line; longer lines are cut.
const maxLineBytes = 16 << 10

// Log line formats at llama.cpp 330f0189c9fc71ef631f40211660a87d99251cc1
// (all LLAMA_LOG_INFO, printed only at -lv 4 or more, which BuildArgs always
// passes, see LlamaLogVerbosity; %s is a buffer or buffer-type name; real
// lines also carry a "0.01.119.248 I " timestamp/level prefix):
//
//	src/llama-model.cpp:2109        "%s: %12s model buffer size = %8.2f MiB\n"
//	src/llama-kv-cache.cpp:279      "%s: %10s KV buffer size = %8.2f MiB\n"
//	src/llama-context.cpp:918       "%s: %10s compute buffer size = %8.2f MiB\n"
//
// Not counted: "output buffer size" (llama-context.cpp:530), "rotation buffer
// size" (llama-kv-cache.cpp:411), "RS buffer size" (llama-memory-recurrent.cpp:122),
// and the DEBUG/WARN "compute buffer size is/of ..." comparison lines
// (llama-context.cpp:651,654), which have no '='.
//
// The name is ggml_backend_buffer_name / ggml_backend_buft_name. For a local
// GPU it is the device name ("CUDA0"). For an RPC device it is
// "RPC<k>[<endpoint>]" where k is the device's index within ITS OWN server
// (ggml-rpc.cpp:1327, 1357), not the global RPC<n> name that --device uses,
// so with two workers both appear as "RPC0[...]" and only the endpoint tells
// them apart. Host buffers are "CPU", "CPU_Mapped", "CUDA_Host", ...
var bufferLineRE = regexp.MustCompile(`(\S+)\s+(model|KV|compute) buffer size\s*=\s*([0-9]+(?:\.[0-9]+)?) MiB`)

// DeviceMem is the memory llama.cpp reported for one buffer name while
// loading, in MiB. Repeated lines of one kind accumulate (sliding-window
// models log two KV buffers per device).
type DeviceMem struct {
	Name       string
	Host       bool // host RAM buffer (CPU*, *_Host), not device memory
	ModelMiB   float64
	KVMiB      float64
	ComputeMiB float64
}

// TotalMiB is the sum of the three buffer kinds.
func (d DeviceMem) TotalMiB() float64 { return d.ModelMiB + d.KVMiB + d.ComputeMiB }

// LoadStats is a snapshot of what the load log has reported so far, in first
// seen order.
type LoadStats struct {
	Devices []DeviceMem
}

// TotalMiB is the grand sum over device buffers: the figure to use as
// needs_mib (design 7.2). Host buffers are excluded because needs_mib is
// compared against device budgets; see HostMiB.
func (s LoadStats) TotalMiB() float64 {
	var t float64
	for _, d := range s.Devices {
		if !d.Host {
			t += d.TotalMiB()
		}
	}
	return t
}

// DeviceCount is the number of GPU/RPC devices that reported buffers. Host
// rows (CPU, CPU_Mapped, *_Host) are not devices the model was placed on.
func (s LoadStats) DeviceCount() int {
	n := 0
	for _, d := range s.Devices {
		if !d.Host {
			n++
		}
	}
	return n
}

// HostMiB is the sum over host buffers.
func (s LoadStats) HostMiB() float64 {
	var t float64
	for _, d := range s.Devices {
		if d.Host {
			t += d.TotalMiB()
		}
	}
	return t
}

// LoadLog accumulates buffer sizes from a child's log output. Feed it
// complete lines; it ignores everything else. Safe for concurrent use.
type LoadLog struct {
	mu    sync.Mutex
	index map[string]int
	devs  []DeviceMem
}

// NewLoadLog returns an empty LoadLog.
func NewLoadLog() *LoadLog { return &LoadLog{index: map[string]int{}} }

// Feed examines one line of output.
func (l *LoadLog) Feed(line string) {
	if !strings.Contains(line, "buffer size") {
		return
	}
	m := bufferLineRE.FindStringSubmatch(line)
	if m == nil {
		return
	}
	mib, err := strconv.ParseFloat(m[3], 64)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer supervise.Unlocked("instance", l.mu.Unlock)
	i, ok := l.index[m[1]]
	if !ok {
		i = len(l.devs)
		l.index[m[1]] = i
		l.devs = append(l.devs, DeviceMem{Name: m[1], Host: isHostBuffer(m[1])})
	}
	switch m[2] {
	case "model":
		l.devs[i].ModelMiB += mib
	case "KV":
		l.devs[i].KVMiB += mib
	case "compute":
		l.devs[i].ComputeMiB += mib
	}
}

// Snapshot returns a copy of the current totals.
func (l *LoadLog) Snapshot() LoadStats {
	l.mu.Lock()
	defer supervise.Unlocked("instance", l.mu.Unlock)
	return LoadStats{Devices: append([]DeviceMem(nil), l.devs...)}
}

func isHostBuffer(name string) bool {
	return strings.HasPrefix(name, "CPU") || strings.HasSuffix(name, "_Host")
}

// LineWriter is an io.Writer that splits a byte stream into lines (LF or
// CRLF) and passes each complete line to a callback. Partial lines are kept
// until completed; Close emits a trailing partial line. Lines longer than
// maxLineBytes are cut (the rest of the line is dropped). Use one LineWriter
// per stream; a LineWriter is not safe for concurrent Write calls, but
// several of them may share one callback if it is safe for concurrent use.
type LineWriter struct {
	emit    func(string)
	buf     []byte
	dropped bool // discarding the rest of an over-long line
	closed  bool
}

// NewLineWriter returns a LineWriter calling emit for each line.
func NewLineWriter(emit func(string)) *LineWriter { return &LineWriter{emit: emit} }

// Write implements io.Writer. It never fails.
func (w *LineWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			w.append(p)
			break
		}
		w.append(p[:i])
		w.flushLine()
		p = p[i+1:]
	}
	return n, nil
}

// Close emits any partial last line. It is idempotent.
func (w *LineWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if len(w.buf) > 0 || w.dropped {
		w.flushLine()
	}
	return nil
}

func (w *LineWriter) append(b []byte) {
	if w.dropped {
		return
	}
	room := maxLineBytes - len(w.buf)
	if len(b) > room {
		b = b[:room]
		w.dropped = true
	}
	w.buf = append(w.buf, b...)
}

func (w *LineWriter) flushLine() {
	line := strings.TrimSuffix(string(w.buf), "\r")
	w.buf = w.buf[:0]
	w.dropped = false
	w.emit(line)
}
