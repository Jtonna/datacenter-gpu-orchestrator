package logship

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// The ring drops its oldest lines first, of whatever key, and the next batch
// of a key that lost lines starts with one line that says how many.
func TestRingDropsOldestAndSaysHowMany(t *testing.T) {
	one := (&Line{Msg: "line-00"}).size()
	r := NewRing(10 * one)
	launchKey := Key{Source: agentproto.LogSourceLaunch, LaunchID: "ln-1"}
	for i := 0; i < 15; i++ {
		k := AgentKey
		if i%3 == 2 {
			k = launchKey
		}
		r.Add(k, Line{Msg: fmt.Sprintf("line-%02d", i)})
	}
	if r.Len() != 10 || r.Bytes() > r.Max() || r.DroppedTotal() != 5 {
		t.Fatalf("len %d bytes %d dropped %d", r.Len(), r.Bytes(), r.DroppedTotal())
	}
	// Lines 0..4 went: 0,1,3,4 of the agent and 2 of the launch.
	k, lines, dropped, ok := r.Take(1<<20, estimate(8192))
	if !ok || k != launchKey || dropped != 1 || lines[0].Msg != "line-05" {
		t.Fatalf("first batch: %v %d %v", k, dropped, lines)
	}
	k, lines, dropped, ok = r.Take(1<<20, estimate(8192))
	if !ok || k != AgentKey || dropped != 4 || lines[0].Msg != "line-06" || len(lines) != 6 {
		t.Fatalf("second batch: %v %d %v", k, dropped, lines)
	}
	if _, _, _, ok := r.Take(1<<20, estimate(8192)); ok || r.Bytes() != 0 {
		t.Fatal("empty ring gave a batch")
	}

	// Through the shipper the count becomes the batch's first line.
	s := New(Config{Now: func() time.Duration { return 0 }, BufferBytes: 3 * one})
	s.Registered(testNode, testObj(1), T0.UnixMilli(), 0, 0)
	for i := 0; i < 5; i++ {
		s.Add(AgentKey, Line{Msg: fmt.Sprintf("line-%02d", i)})
	}
	b := s.take(s.conf.Load())
	if b == nil || len(b.chunks) != 1 || len(b.chunks[0].lines) != 4 {
		t.Fatalf("batch %+v", b)
	}
	var first map[string]any
	json.Unmarshal(b.chunks[0].lines[0], &first)
	if !strings.Contains(msgOf(first), "2 lines of this source were dropped") || first["level"] != "warn" {
		t.Fatalf("first line %v", first)
	}
}

// The 8 MiB bound holds under a flood from several producers of any line
// length, counted by the ring and measured on the heap.
func TestRingBoundHoldsUnderFlood(t *testing.T) {
	r := NewRing(DefaultBufferBytes)
	rnd := rand.New(rand.NewSource(1))
	keys := []Key{AgentKey, JournalKey, {Source: agentproto.LogSourceLaunch, LaunchID: "ln-1"}}
	big := strings.Repeat("x", 8192)
	var ms runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms)
	base := ms.HeapAlloc
	for i := 0; i < 400_000; i++ {
		n := rnd.Intn(300)
		if i%1000 == 0 {
			n = 8192
		}
		r.Add(keys[i%3], Line{Msg: big[:n], Level: "info", Unit: "unit.service"})
		if i%10_000 == 0 && r.Bytes() > r.Max() {
			t.Fatalf("ring holds %d bytes, bound %d", r.Bytes(), r.Max())
		}
	}
	if r.Bytes() > r.Max() || r.DroppedTotal() == 0 {
		t.Fatalf("bytes %d dropped %d", r.Bytes(), r.DroppedTotal())
	}
	runtime.GC()
	runtime.ReadMemStats(&ms)
	grown := int64(ms.HeapAlloc) - int64(base)
	// The strings share one backing array here, so the heap holds the line
	// structs and queue slots: within the counted bound, with slack for the
	// slices' spare capacity.
	if grown > 3*DefaultBufferBytes {
		t.Fatalf("heap grew by %d bytes for an %d byte ring", grown, DefaultBufferBytes)
	}
	t.Logf("ring %d bytes, %d lines, %d dropped; heap grew %d", r.Bytes(), r.Len(), r.DroppedTotal(), grown)
}

// A line over max_line_bytes is cut to it, on a character boundary, with
// truncated: true, and stays valid JSON; the store would skip it otherwise.
func TestLineCutAndMarked(t *testing.T) {
	msg := strings.Repeat("ab\"c\\é\x01 ", 1000) // escapes and a two-byte character
	for _, max := range []int{256, 300, 1024, 8192} {
		var buf bytes.Buffer
		l := Line{Msg: msg, At: T0.UnixNano(), Level: "info", Unit: "u.service"}
		encodeLine(&buf, &l, max)
		if buf.Len() > max || buf.Bytes()[buf.Len()-1] != '\n' {
			t.Fatalf("max %d: %d bytes", max, buf.Len())
		}
		var m map[string]any
		if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
			t.Fatalf("max %d: %v %q", max, err, buf.String())
		}
		got := msgOf(m)
		if m["truncated"] != true || !strings.HasPrefix(msg, got) || !utf8.ValidString(got) || len(got) < max/4 {
			t.Fatalf("max %d: %v", max, m)
		}
	}
	// A short line is whole and unmarked.
	var buf bytes.Buffer
	encodeLine(&buf, &Line{Msg: "hello <world> & \"you\"", At: T0.UnixNano()}, 1024)
	var m map[string]any
	json.Unmarshal(buf.Bytes(), &m)
	if msgOf(m) != "hello <world> & \"you\"" || m["truncated"] != nil || m["_time"] != "2026-11-21T10:00:00Z" {
		t.Fatalf("%s", buf.String())
	}
	// Through the shipper: cut when added too, marked once.
	s := New(Config{Now: func() time.Duration { return 0 }})
	obj := testObj(1)
	obj.Limits.MaxLineBytes = 300
	s.Registered(testNode, obj, T0.UnixMilli(), 0, 0)
	s.Add(AgentKey, Line{Msg: strings.Repeat("z", 5000)})
	b := s.take(s.conf.Load())
	if l := b.chunks[0].lines[0]; len(l) > 300 || !bytes.Contains(l, []byte(`"truncated":true`)) {
		t.Fatalf("%s", l)
	}
}
