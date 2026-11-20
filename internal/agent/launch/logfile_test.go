package launch

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestLogFileCapKeepsAbsoluteOffsets(t *testing.T) {
	p := filepath.Join(t.TempDir(), "l.log")
	l := &LogFile{Path: p, Cap: 64 << 10}
	write := func(n int, c byte) {
		f, _ := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		f.Write(bytes.Repeat([]byte{c}, n))
		f.Close()
	}
	write(40<<10, 'a')
	if d, _ := l.Enforce(); d {
		t.Fatal("dropped under the cap")
	}
	write(40<<10, 'b') // 80 KiB kept > 64 KiB cap
	if d, err := l.Enforce(); !d || err != nil {
		t.Fatal("not dropped", err)
	}
	if l.Dropped() != 40<<10 { // oldest half of 80 KiB, 4 KiB aligned
		t.Fatalf("dropped %d", l.Dropped())
	}
	// A reader at 0 gets what remains and the count it missed.
	data, next, dropped, err := l.Read(0, 1<<20)
	if err != nil || dropped != 40<<10 || next != 80<<10 || len(data) != 40<<10 || data[0] != 'b' {
		t.Fatalf("read: n=%d next=%d dropped=%d err=%v", len(data), next, dropped, err)
	}
	// Offsets do not move: reading from 80 KiB after more output.
	write(10, 'c')
	data, next, dropped, _ = l.Read(80<<10, 100)
	if string(data) != "cccccccccc" || next != 80<<10+10 || dropped != 0 {
		t.Fatalf("%q %d %d", data, next, dropped)
	}
	// Partial read with max.
	data, next, _, _ = l.Read(40<<10, 5)
	if len(data) != 5 || next != 40<<10+5 {
		t.Fatal(len(data), next)
	}
}

func TestLogFileMissing(t *testing.T) {
	l := &LogFile{Path: filepath.Join(t.TempDir(), "none")}
	data, next, _, err := l.Read(7, 10)
	if err != nil || len(data) != 0 || next != 7 {
		t.Fatal(data, next, err)
	}
}

func TestLogScan(t *testing.T) {
	var s logScan
	var partial []byte
	s.feed(&partial, []byte("Starting RPC server v3.0.0\n[file_load] progress bytes=5 to"))
	if !s.started || s.haveBytes {
		t.Fatal("partial line must wait")
	}
	s.feed(&partial, []byte("tal=10\n"))
	if !s.haveBytes || s.bytesLoaded != 5 || s.bytesTotal != 10 {
		t.Fatalf("%+v", s)
	}
	for i := 0; i < 50; i++ {
		s.feed(&partial, []byte("x\n"))
	}
	if len(s.tail) != 40 {
		t.Fatal(len(s.tail))
	}
	s.feed(&partial, []byte("rpc_file_load_timeout: endpoint=1.2.3.4:50052 command=GET_TENSOR timeout_s=30\n"))
	if !s.timeoutTok {
		t.Fatal("timeout token")
	}
}
