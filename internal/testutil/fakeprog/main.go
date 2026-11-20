// Command fakeprog stands in for the programs potluck-agent runs, chosen by
// the base name it is started as:
//
//   - a name containing "exporter" (node_exporter, nvidia_gpu_exporter):
//     serves the file FAKEPROG_FIXTURE at /metrics on --web.listen-address.
//   - anything else (rpc-server): a fake llama rpc-server. It prints the
//     upstream start lines, listens on --host/-H and --port/-p, accepts one
//     client at a time and holds the connection until the client closes it.
//
// Environment (rpc-server mode):
//
//	FAKEPROG_EXIT_AFTER     duration; exit on its own after it (code FAKEPROG_EXIT_CODE, default 1)
//	FAKEPROG_EXIT_CODE      exit code for the above
//	FAKEPROG_NO_LISTEN      non-empty: print the start line but never listen
//	FAKEPROG_IGNORE_TERM    non-empty: ignore SIGTERM (a stop that hangs)
//	FAKEPROG_PROGRESS       "total:steps": after a client connects, print the
//	                        fork's "[file_load] progress bytes=N total=M" lines
//	FAKEPROG_LINES          extra lines printed at start, separated by "|"
//	FAKEPROG_START_LOG      file to which argv is appended, one line per start
//	FAKEPROG_PRINT_ENV      comma-separated names; print "env NAME=value" for each at start
package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	name := strings.ToLower(filepath.Base(os.Args[0]))
	if p := os.Getenv("FAKEPROG_START_LOG"); p != "" {
		if f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644); err == nil {
			fmt.Fprintln(f, strings.Join(os.Args, " "))
			f.Close()
		}
	}
	if strings.Contains(name, "exporter") {
		exporter()
		return
	}
	rpc()
}

func flagValue(args []string, names ...string) string {
	for i := 0; i < len(args); i++ {
		for _, n := range names {
			if args[i] == n && i+1 < len(args) {
				return args[i+1]
			}
			if strings.HasPrefix(args[i], n+"=") {
				return strings.TrimPrefix(args[i], n+"=")
			}
		}
	}
	return ""
}

func exporter() {
	addr := flagValue(os.Args[1:], "--web.listen-address")
	if addr == "" {
		fmt.Fprintln(os.Stderr, "missing --web.listen-address")
		os.Exit(2)
	}
	fixture := os.Getenv("FAKEPROG_FIXTURE")
	http.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		b, err := os.ReadFile(fixture)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Write(b)
	})
	fmt.Fprintln(os.Stderr, "listening on", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func rpc() {
	args := os.Args[1:]
	host := flagValue(args, "--host", "-H")
	if host == "" {
		host = "127.0.0.1"
	}
	port := flagValue(args, "--port", "-p")
	if port == "" {
		port = "50052"
	}
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGTERM, os.Interrupt)
	go func() {
		for s := range sigs {
			if os.Getenv("FAKEPROG_IGNORE_TERM") != "" {
				fmt.Println("ignoring", s)
				continue
			}
			fmt.Println("terminating on", s)
			os.Exit(0)
		}
	}()
	if lines := os.Getenv("FAKEPROG_LINES"); lines != "" {
		for _, l := range strings.Split(lines, "|") {
			fmt.Println(l)
		}
	}
	if d, err := time.ParseDuration(os.Getenv("FAKEPROG_EXIT_AFTER")); err == nil {
		code := 1
		if c, err := strconv.Atoi(os.Getenv("FAKEPROG_EXIT_CODE")); err == nil {
			code = c
		}
		go func() {
			time.Sleep(d)
			fmt.Println("exiting with", code)
			os.Exit(code)
		}()
	}
	for _, name := range strings.Split(os.Getenv("FAKEPROG_PRINT_ENV"), ",") {
		if name != "" {
			fmt.Printf("env %s=%s\n", name, os.Getenv(name))
		}
	}
	fmt.Println("Starting RPC server v3.0.0")
	fmt.Printf("  endpoint       : %s:%s\n", host, port)
	if os.Getenv("FAKEPROG_NO_LISTEN") != "" {
		select {}
	}
	l, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		fmt.Println("bind failed:", err)
		os.Exit(1)
	}
	for {
		c, err := l.Accept()
		if err != nil {
			continue
		}
		fmt.Println("Accepted client connection")
		if p := os.Getenv("FAKEPROG_PROGRESS"); p != "" {
			go progress(p)
		}
		io.Copy(io.Discard, c) // hold until the client closes
		c.Close()
		fmt.Println("Client connection closed")
	}
}

func progress(spec string) {
	tot, steps := int64(1000), int64(4)
	if a, b, ok := strings.Cut(spec, ":"); ok {
		tot, _ = strconv.ParseInt(a, 10, 64)
		steps, _ = strconv.ParseInt(b, 10, 64)
	}
	for i := int64(1); i <= steps; i++ {
		fmt.Printf("[file_load] progress bytes=%d total=%d\n", tot*i/steps, tot)
		time.Sleep(20 * time.Millisecond)
	}
}
