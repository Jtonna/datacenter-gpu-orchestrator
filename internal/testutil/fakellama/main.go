// Command fakellama stands in for llama-server-rpc in integration tests.
// It accepts the real command line, prints a llama.cpp style --list-devices
// listing, and serves a small llama-server-like HTTP API. Behaviour is
// scripted through FAKELLAMA_* environment variables (see README.md).
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// config is the part of the command line the fake cares about.
type config struct {
	host        string
	port        int
	model       string
	rpc         []string
	device      string
	tensorSplit string
	ctxSize     int
	parallel    int
	verbosity   int // -lv; llama.cpp's default is 3
	listDevices bool
	other       map[string]string // every other flag; value is "" when it had none
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--nvidia-smi" {
		fakeNvidiaSmi(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fakeVersion()
		return
	}
	cfg := parseArgs(os.Args[1:])
	mode := "serve"
	if cfg.listDevices {
		mode = "list-devices"
	}
	appendStartLog(mode)
	handleSignals()

	if cfg.listDevices {
		runListDevices(cfg)
		return
	}
	serve(cfg)
}

// fakeVersion answers `--version` as llama.cpp does: the build number and
// the short commit on stderr, then the compiler line. FAKELLAMA_BUILD sets the
// commit (default a01665d30).
func fakeVersion() {
	commit := os.Getenv("FAKELLAMA_BUILD")
	if commit == "" {
		commit = "a01665d30"
	}
	fmt.Fprintf(os.Stderr, "version: 6123 (%s)\nbuilt with cc (Ubuntu 13.3.0) 13.3.0 for x86_64-linux-gnu\n", commit)
}

// fakeNvidiaSmi stands in for `nvidia-smi --query-gpu=memory.total,memory.used
// --format=csv,noheader,nounits` (the scheduler's local_probe_cmd, design 5.1):
// `fakellama --nvidia-smi [totalMiB [usedMiB]]` prints "total, used" and exits.
// The defaults are an empty 8151 MiB card.
func fakeNvidiaSmi(args []string) {
	total, used := 8151, 0
	if len(args) > 0 {
		total, _ = strconv.Atoi(args[0])
	}
	if len(args) > 1 {
		used, _ = strconv.Atoi(args[1])
	}
	fmt.Printf("%d, %d\n", total, used)
}

func parseArgs(args []string) config {
	c := config{host: "127.0.0.1", port: 8080, ctxSize: 4096, parallel: 1, verbosity: 3, other: map[string]string{}}
	for i := 0; i < len(args); i++ {
		name, val, hasVal := strings.Cut(args[i], "=")
		if !strings.HasPrefix(name, "-") {
			continue // stray positional; tolerate
		}
		if name == "--list-devices" {
			c.listDevices = true
			continue
		}
		if !hasVal && i+1 < len(args) && !looksLikeFlag(args[i+1]) {
			val = args[i+1]
			i++
		}
		switch name {
		case "--host":
			c.host = val
		case "--port":
			c.port, _ = strconv.Atoi(val)
		case "--model", "-m":
			c.model = val
		case "--rpc":
			c.rpc = strings.Split(val, ",")
		case "--device", "-dev":
			c.device = val
		case "--tensor-split", "-ts":
			c.tensorSplit = val
		case "--ctx-size", "-c":
			if n, err := strconv.Atoi(val); err == nil && n > 0 {
				c.ctxSize = n
			}
		case "--parallel", "-np":
			if n, err := strconv.Atoi(val); err == nil && n > 0 {
				c.parallel = n
			}
		case "-lv", "--verbosity", "--log-verbosity":
			if n, err := strconv.Atoi(val); err == nil {
				c.verbosity = n
			}
			c.other[name] = val
		default:
			c.other[name] = val
		}
	}
	return c
}

// looksLikeFlag reports whether s starts a new option rather than being a
// value. Negative numbers such as -1 are values.
func looksLikeFlag(s string) bool {
	if !strings.HasPrefix(s, "-") || len(s) == 1 {
		return false
	}
	_, err := strconv.ParseFloat(s, 64)
	return err != nil
}

// appendStartLog appends one JSON line describing this start to the file named
// by FAKELLAMA_START_LOG, if set.
func appendStartLog(mode string) {
	path := os.Getenv("FAKELLAMA_START_LOG")
	if path == "" {
		return
	}
	env := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "FAKELLAMA_") || strings.HasPrefix(k, "LLAMA_ARG_") {
			env[k] = v
		}
	}
	if v, ok := os.LookupEnv("CUDA_VISIBLE_DEVICES"); ok {
		env["CUDA_VISIBLE_DEVICES"] = v // set-but-empty matters to the pre-flight (H3)
	}
	if v, ok := os.LookupEnv("LLAMA_RPC_FILE_LOAD"); ok {
		env["LLAMA_RPC_FILE_LOAD"] = v
	}
	line, _ := json.Marshal(map[string]any{
		"time": time.Now().UTC().Format(time.RFC3339Nano),
		"pid":  os.Getpid(),
		"mode": mode,
		"argv": os.Args[1:],
		"env":  env,
	})
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakellama: cannot open start log:", err)
		return
	}
	defer f.Close()
	f.Write(append(line, '\n')) // single write keeps lines whole under O_APPEND
}

// handleSignals exits cleanly on SIGTERM/interrupt unless
// FAKELLAMA_IGNORE_SIGTERM is set.
func handleSignals() {
	ignore := os.Getenv("FAKELLAMA_IGNORE_SIGTERM") != ""
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		for s := range ch {
			if ignore {
				fmt.Fprintln(os.Stderr, "fakellama: ignoring", s)
				continue
			}
			fmt.Fprintln(os.Stderr, "fakellama: terminating on", s)
			os.Exit(0)
		}
	}()
}

func envDuration(key string) time.Duration {
	d, _ := time.ParseDuration(os.Getenv(key))
	return d
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return n
	}
	return def
}

// ---- --list-devices ----

type device struct {
	name, desc     string
	totalMiB, free int
}

func runListDevices(cfg config) {
	if os.Getenv("FAKELLAMA_LIST_DEVICES_HANG") != "" || hangsFor(cfg.rpc) {
		select {} // a busy rpc-server blocks the real client forever
	}
	if code := envInt("FAKELLAMA_LIST_DEVICES_EXIT", 0); code != 0 {
		fmt.Fprintln(os.Stderr, "fakellama: simulated --list-devices failure")
		os.Exit(code)
	}
	printDevices(devices(cfg))
}

// hangsFor reports whether any --rpc endpoint is listed in
// FAKELLAMA_LIST_DEVICES_HANG_ADDR (comma separated): one busy worker among
// several answering ones.
func hangsFor(rpc []string) bool {
	for _, want := range strings.Split(os.Getenv("FAKELLAMA_LIST_DEVICES_HANG_ADDR"), ",") {
		want = strings.TrimSpace(want)
		for _, have := range rpc {
			if want != "" && want == have {
				return true
			}
		}
	}
	return false
}

// devices returns FAKELLAMA_DEVICES ("NAME:totalMiB:freeMiB,...", or "none"),
// else one RPC<i> per --rpc endpoint plus CUDA0.
func devices(cfg config) []device {
	if spec := os.Getenv("FAKELLAMA_DEVICES"); spec != "" {
		var out []device
		if spec == "none" {
			return out
		}
		for _, item := range strings.Split(spec, ",") {
			f := strings.Split(item, ":")
			if len(f) != 3 {
				fmt.Fprintf(os.Stderr, "fakellama: bad FAKELLAMA_DEVICES item %q\n", item)
				os.Exit(2)
			}
			total, _ := strconv.Atoi(f[1])
			free, _ := strconv.Atoi(f[2])
			out = append(out, device{f[0], "fake device " + f[0], total, free})
		}
		return out
	}
	var out []device
	for i, ep := range cfg.rpc {
		out = append(out, device{"RPC" + strconv.Itoa(i), ep, 15000, 14000})
	}
	return append(out, device{"CUDA0", "NVIDIA GeForce RTX 5060", 8151, 7000})
}

// printDevices copies common_print_available_devices() in llama.cpp
// common/arg.cpp.
func printDevices(devs []device) {
	fmt.Print("Available devices:\n")
	if len(devs) == 0 {
		fmt.Print("  (none)\n")
		return
	}
	for _, d := range devs {
		fmt.Printf("  %s: %s (%d MiB, %d MiB free)\n", d.name, d.desc, d.totalMiB, d.free)
	}
}

// ---- server ----

func serve(cfg config) {
	s := &server{cfg: cfg}
	s.chunks = envInt("FAKELLAMA_STREAM_CHUNKS", 5)
	s.chunkDelay = envDuration("FAKELLAMA_STREAM_DELAY")
	s.stallAfter = envInt("FAKELLAMA_STALL_AFTER", -1)
	s.countPrompt = os.Getenv("FAKELLAMA_COUNT_PROMPT") != ""

	if os.Getenv("FAKELLAMA_EXIT_CODE") != "" {
		code := envInt("FAKELLAMA_EXIT_CODE", 1)
		time.AfterFunc(envDuration("FAKELLAMA_EXIT_AFTER"), func() {
			fmt.Fprintln(os.Stderr, "fakellama: simulated load failure: failed to load model")
			os.Exit(code)
		})
	}
	if os.Getenv("FAKELLAMA_NEVER_HEALTHY") == "" {
		time.AfterFunc(envDuration("FAKELLAMA_LOAD_DELAY"), func() { s.ready.Store(true) })
	}

	emitLoadLog(cfg)
	addr := fmt.Sprintf("%s:%d", cfg.host, cfg.port)
	fmt.Fprintln(os.Stderr, "fakellama: listening on", addr)
	if err := http.ListenAndServe(addr, s.routes()); err != nil {
		fmt.Fprintln(os.Stderr, "fakellama:", err)
		os.Exit(1)
	}
}
