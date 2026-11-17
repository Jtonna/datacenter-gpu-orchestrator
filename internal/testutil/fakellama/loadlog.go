package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// emitLoadLog prints, on stderr, the buffer-size lines llama.cpp logs while
// loading (formats at commit 330f0189: src/llama-model.cpp:2109,
// src/llama-kv-cache.cpp:279, src/llama-context.cpp:918). It is scripted by
// FAKELLAMA_LOAD_LOG (and only when the command line has -lv 4 or more): empty = nothing; "auto" = one device per --device name
// (default: every --rpc endpoint, then CUDA0) with 1000/200/100 MiB; otherwise
// "NAME:modelMiB:kvMiB:computeMiB,..." where NAME is a --device name.
//
// As in llama.cpp, an RPC device's buffer name is "RPC0[<endpoint>]" (the
// index is per server, not the global RPC<n> name), and a CPU model buffer
// and some unrelated lines are interleaved to exercise tolerant parsers.
func emitLoadLog(cfg config) {
	spec := os.Getenv("FAKELLAMA_LOAD_LOG")
	// These are INFO messages, which llama.cpp shows only from -lv 4 up
	// (common/log.cpp:551, default threshold 3), so a command line without
	// -lv 4 sees nothing, exactly as on the real hardware.
	if spec == "" || cfg.verbosity < 4 {
		return
	}
	type entry struct {
		name               string
		model, kv, compute float64
	}
	var entries []entry
	if spec == "auto" {
		names := strings.Split(cfg.device, ",")
		if cfg.device == "" {
			names = nil
			for i := range cfg.rpc {
				names = append(names, "RPC"+strconv.Itoa(i))
			}
			names = append(names, "CUDA0")
		}
		for _, n := range names {
			entries = append(entries, entry{n, 1000, 200, 100})
		}
	} else {
		for _, item := range strings.Split(spec, ",") {
			f := strings.Split(item, ":")
			if len(f) != 4 {
				fmt.Fprintf(os.Stderr, "fakellama: bad FAKELLAMA_LOAD_LOG item %q\n", item)
				os.Exit(2)
			}
			var e entry
			e.name = f[0]
			e.model, _ = strconv.ParseFloat(f[1], 64)
			e.kv, _ = strconv.ParseFloat(f[2], 64)
			e.compute, _ = strconv.ParseFloat(f[3], 64)
			entries = append(entries, e)
		}
	}

	bufName := func(dev string) string {
		if strings.HasPrefix(dev, "RPC") {
			if i, err := strconv.Atoi(dev[3:]); err == nil && i < len(cfg.rpc) {
				return "RPC0[" + cfg.rpc[i] + "]"
			}
		}
		return dev
	}

	fmt.Fprintln(os.Stderr, "llama_model_loader: loaded meta data with 35 key-value pairs")
	fmt.Fprintf(os.Stderr, "load_tensors: %12s model buffer size = %8.2f MiB\n", "CPU_Mapped", 360.0)
	for _, e := range entries {
		fmt.Fprintf(os.Stderr, "load_tensors: %12s model buffer size = %8.2f MiB\n", bufName(e.name), e.model)
	}
	fmt.Fprintln(os.Stderr, "llama_context: n_ctx         =", cfg.ctxSize)
	for _, e := range entries {
		fmt.Fprintf(os.Stderr, "llama_kv_cache: %10s KV buffer size = %8.2f MiB\n", bufName(e.name), e.kv)
	}
	fmt.Fprintf(os.Stderr, "llama_context: %10s  output buffer size = %8.2f MiB\n", "CUDA_Host", 0.98)
	for _, e := range entries {
		fmt.Fprintf(os.Stderr, "llama_context: %10s compute buffer size = %8.2f MiB\n", bufName(e.name), e.compute)
	}
}
