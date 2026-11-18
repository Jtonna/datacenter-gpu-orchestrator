//go:build linux

package instance

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// These tests run only on Linux (CI uses Docker). A helper process, this
// test binary re-run on TestSenderHelper, sends bytes to a listener the test
// owns, the way llama.cpp's RPC client does, and the tests check what the
// kernel and /proc report about it.

const sendMiB = 64

// TestSenderHelper is the helper process; it does nothing in a normal run.
// SENDER_ADDR is where to connect, SENDER_MODE is "send" (sendto(2), what
// llama.cpp uses) or "write" (write(2)). It prints "wchar=<n>" and
// "sent <bytes>" and then waits for a minute.
func TestSenderHelper(t *testing.T) {
	addr := os.Getenv("SENDER_ADDR")
	if addr == "" {
		t.Skip("helper process only")
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		fmt.Println("dial:", err)
		os.Exit(1)
	}
	raw, _ := c.(*net.TCPConn).SyscallConn()
	buf := make([]byte, 1<<20)
	for i := 0; i < sendMiB; i++ {
		if os.Getenv("SENDER_MODE") == "write" {
			if _, err := c.Write(buf); err != nil {
				fmt.Println("write:", err)
				os.Exit(1)
			}
			continue
		}
		var serr error
		n := 0 // outside the callback: after EAGAIN it is called again and must carry on, not restart
		raw.Write(func(fd uintptr) bool {
			for n < len(buf) {
				r, _, e := syscall.Syscall6(syscall.SYS_SENDTO, fd, uintptr(unsafe.Pointer(&buf[n])), uintptr(len(buf)-n), 0, 0, 0)
				if e == syscall.EAGAIN {
					return false
				}
				if e != 0 {
					serr = e
					return true
				}
				n += int(r)
			}
			return true
		})
		if serr != nil {
			fmt.Println("sendto:", serr)
			os.Exit(1)
		}
	}
	ioStats, _ := os.ReadFile("/proc/self/io")
	for _, l := range strings.Split(string(ioStats), "\n") {
		if strings.HasPrefix(l, "wchar:") {
			fmt.Println("wchar=" + strings.TrimSpace(strings.TrimPrefix(l, "wchar:")))
		}
	}
	fmt.Printf("sent %d\n", sendMiB<<20)
	time.Sleep(time.Minute)
}

// sink accepts connections and discards what arrives.
func sink(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(io.Discard, c); c.Close() }()
		}
	}()
	return ln.Addr().String()
}

func helperSpec(addr, mode string) StartSpec {
	return StartSpec{
		Build: func(int) ([]string, error) {
			return []string{os.Args[0], "-test.run=^TestSenderHelper$", "-test.v"}, nil
		},
		Env: []string{"SENDER_ADDR=" + addr, "SENDER_MODE=" + mode},
	}
}

func waitTail(t *testing.T, c Child, sub string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if s := strings.Join(c.Tail(100), "\n"); strings.Contains(s, sub) {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("child never printed %q; tail %q", sub, c.Tail(100))
	return ""
}

func wchar(t *testing.T, tail string) int64 {
	t.Helper()
	for _, l := range strings.Split(tail, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "wchar="); ok {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
	}
	t.Fatalf("no wchar in %q", tail)
	return 0
}

// The reason /proc/<pid>/io was not used: wchar counts bytes sent with
// write(2) but not bytes sent with send(2)/sendto(2).
func TestProcIOWcharMissesSend(t *testing.T) {
	addr := sink(t)
	l := newLauncher(t)

	cw := start(t, l, helperSpec(addr, "write"))
	viaWrite := wchar(t, waitTail(t, cw, "sent "))
	cs := start(t, l, helperSpec(addr, "send"))
	viaSend := wchar(t, waitTail(t, cs, "sent "))

	t.Logf("%d MiB sent: wchar via write() = %d, via sendto() = %d", sendMiB, viaWrite, viaSend)
	if viaWrite < sendMiB<<20 {
		t.Fatalf("wchar via write() = %d, want at least %d", viaWrite, sendMiB<<20)
	}
	if viaSend > (sendMiB<<20)/10 {
		t.Fatalf("wchar via sendto() = %d: it counts sent bytes after all, so /proc/<pid>/io would do", viaSend)
	}
}

func TestSentBytesCountsSendToThePeer(t *testing.T) {
	addr := sink(t)
	l := newLauncher(t)
	c := start(t, l, helperSpec(addr, "send"))
	bc, ok := c.(ByteCounter)
	if !ok {
		t.Fatal("a process child is not a ByteCounter")
	}

	// Before the helper has connected nothing is found; no error either.
	waitTail(t, c, "sent ")

	var got, peer int64
	var socks int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		sc, err := bc.SentBytes([]string{addr})
		if err != nil {
			t.Fatal(err)
		}
		got, socks, peer = sc.Bytes, sc.Sockets, sc.Peers[addr]
		if got >= sendMiB<<20 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	want := int64(sendMiB << 20)
	if socks != 1 || got < want || got > want+4096 {
		t.Fatalf("SentBytes = %d bytes over %d sockets, want %d..%d over 1", got, socks, want, want+4096)
	}
	if peer != got {
		t.Fatalf("per-peer figure %d, want the sum %d (one worker)", peer, got)
	}
	t.Logf("SentBytes = %d for %d sent (%d socket)", got, want, socks)

	// A different peer address matches no socket of the child.
	if sc, err := bc.SentBytes([]string{"127.0.0.1:1"}); err != nil || sc.Sockets != 0 || sc.Bytes != 0 || len(sc.Peers) != 0 {
		t.Fatalf("other peer: %+v, err %v", sc, err)
	}
	// Peer given as a worker address list with garbage in it still works.
	if sc, err := bc.SentBytes([]string{"not an address", addr}); err != nil || sc.Sockets != 1 || sc.Bytes < want || sc.Peers[addr] < want {
		t.Fatalf("mixed peers: %+v, err %v", sc, err)
	}
	if _, err := bc.SentBytes(nil); err == nil {
		t.Fatal("no peers: want an error")
	}
}

// A process outside the child's group is never counted, even though it
// talks to the same peer.
func TestSentBytesIgnoresOtherProcesses(t *testing.T) {
	addr := sink(t)
	other := exec.Command(os.Args[0], "-test.run=^TestSenderHelper$")
	other.Env = append(os.Environ(), "SENDER_ADDR="+addr, "SENDER_MODE=send")
	out, _ := other.StdoutPipe()
	other.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Kill(-other.Process.Pid, syscall.SIGKILL); other.Wait() })
	buf := make([]byte, 4096)
	var seen string
	for !strings.Contains(seen, "sent ") {
		n, err := out.Read(buf)
		seen += string(buf[:n])
		if err != nil {
			t.Fatalf("helper: %v %q", err, seen)
		}
	}

	l := newLauncher(t)
	idle := start(t, l, StartSpec{Build: func(int) ([]string, error) { return []string{"sleep", "30"}, nil }})
	idleSent, err := idle.(ByteCounter).SentBytes([]string{addr})
	if err != nil || idleSent.Sockets != 0 || idleSent.Bytes != 0 {
		t.Fatalf("idle child: %+v, err %v", idleSent, err)
	}
	var got int64
	var socks int
	// The helper itself is countable by its own pid.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var sc SentCount
		sc, err = socketBytesAcked(other.Process.Pid, []string{addr})
		got, socks = sc.Bytes, sc.Sockets
		if err == nil && got >= sendMiB<<20 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || socks != 1 || got < sendMiB<<20 {
		t.Fatalf("helper: %d bytes, %d sockets, err %v", got, socks, err)
	}
}

func TestSentBytesUnknownProcess(t *testing.T) {
	if _, err := socketBytesAcked(1<<22+12345, []string{"127.0.0.1:1"}); err == nil {
		t.Fatal("want an error for a process that does not exist")
	}
}
