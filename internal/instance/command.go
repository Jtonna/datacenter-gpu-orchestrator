package instance

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/llamaflags"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/modelpath"
)

// BuildOptions are the policy values the command builder needs besides the
// profile and placement (design 6 defaults, plus models_dir).
type BuildOptions struct {
	ServerBin   string // defaults.server_bin
	ModelsDir   string // models_dir
	LocalDevice string // defaults.local_device
	CacheRAMMiB int    // defaults.cache_ram_mib (a host on the scheduler's machine)
	// CtxCheckpoints is --ctx-checkpoints (defaults.ctx_checkpoints), always
	// passed: 0 is "keep none", not "unset".
	CtxCheckpoints int
}

// OptionsFromConfig extracts BuildOptions from a validated config.
func OptionsFromConfig(c *config.Config) BuildOptions {
	return BuildOptions{
		ServerBin:   c.Defaults.ServerBin,
		ModelsDir:   c.ModelsDir,
		LocalDevice: c.Defaults.LocalDevice,
		CacheRAMMiB: c.Defaults.CacheRAMMiB, CtxCheckpoints: c.Defaults.CtxCheckpoints,
	}
}

// HostOptions is what a host launch on a node takes from the configuration
// (HostLaunchArgs).
type HostOptions struct {
	NoMmap         bool // defaults.host_no_mmap
	CacheRAMMiB    int  // defaults.node_host_cache_ram_mib
	CtxCheckpoints int  // defaults.ctx_checkpoints, as on the scheduler's launch
}

// HostOptionsFromConfig extracts HostOptions from a validated config.
func HostOptionsFromConfig(c *config.Config) HostOptions {
	return HostOptions{NoMmap: c.Defaults.HostNoMmap, CacheRAMMiB: c.Defaults.NodeHostCacheRAMMiB, CtxCheckpoints: c.Defaults.CtxCheckpoints}
}

// LlamaLogVerbosity is the --verbosity (-lv) threshold every kind: llama child
// is started with. It is the MINIMUM that prints the three buffer-size lines
// the load-log reader needs (design 7.4), at llama.cpp
// 330f0189c9fc71ef631f40211660a87d99251cc1:
//
//	src/llama-model.cpp:2109, src/llama-kv-cache.cpp:279 and
//	src/llama-context.cpp:918 all use LLAMA_LOG_INFO (GGML_LOG_LEVEL_INFO).
//	common/log.cpp:548-557 (common_log_get_verbosity, line 551) maps
//	GGML_LOG_LEVEL_INFO to LOG_LEVEL_TRACE, which common/log.h:25 defines as 4,
//	and common_log_default_callback (common/log.cpp:561-565) drops a message whose
//	verbosity is above the threshold. The default threshold is LOG_DEFAULT_LLAMA
//	= LOG_LEVEL_INFO = 3 (common/log.h:32, common/log.cpp:17), so the lines are hidden by default;
//	-lv 4 shows them. Level 5 (debug) is not needed and would also print
//	GGML_LOG_LEVEL_DEBUG lines (common/log.cpp:96), so 4 is exact.
//	Spellings (common/arg.cpp:3934): -lv, --verbosity, --log-verbosity;
//	env LLAMA_ARG_LOG_VERBOSITY (scrubbed from the child's environment).
const LlamaLogVerbosity = 4

// BuildArgs returns the argv for a profile placed on pl, listening on port.
// For kind: llama it follows design 7.4 exactly; for kind: command it expands
// ${PORT} in the profile's cmd and splits it (see ExpandCommand).
func BuildArgs(p core.Profile, pl core.Placement, port int, o BuildOptions) ([]string, error) {
	if p.Kind == core.KindCommand {
		return ExpandCommand(p.Cmd, port)
	}
	if len(pl.Nodes) == 0 {
		return nil, errors.New("instance: placement has no nodes")
	}
	model, err := modelpath.Join(o.ModelsDir, p.Model)
	if err != nil {
		return nil, fmt.Errorf("instance: %w", err)
	}
	split, err := splitWeights(p, pl)
	if err != nil {
		return nil, err
	}
	parallel := p.Parallel
	if parallel < 1 {
		parallel = 1
	}

	var rpc, devices []string
	remotes := 0
	for _, n := range pl.Nodes {
		if n.Local {
			if o.LocalDevice == "" {
				return nil, errors.New("instance: placement includes the local node but no local device is configured")
			}
			devices = append(devices, o.LocalDevice)
			continue
		}
		if n.Addr == "" {
			return nil, fmt.Errorf("instance: remote node %q has no rpc address", n.ID)
		}
		rpc = append(rpc, n.Addr)
		devices = append(devices, "RPC"+strconv.Itoa(remotes))
		remotes++
	}

	args := []string{
		o.ServerBin,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"--model", model,
		"--n-gpu-layers", "999",
		"--fit", "off",
		"--ctx-size", strconv.Itoa(p.Ctx * parallel),
		"--parallel", strconv.Itoa(parallel),
	}
	args = append(args, profileHead(profileHostMemory(p, o.CacheRAMMiB, o.CtxCheckpoints))...)
	if len(rpc) > 0 {
		args = append(args, "--rpc", strings.Join(rpc, ","))
	}
	args = append(args, "--device", strings.Join(devices, ","))
	if len(devices) > 1 {
		parts := make([]string, len(split))
		for i, v := range split {
			parts[i] = strconv.Itoa(v)
		}
		args = append(args, "--tensor-split", strings.Join(parts, ","))
	}
	return append(args, profileTail(p)...), nil
}

// profileHead is the part of a llama command line that follows --parallel
// and comes before the placement flags. Both launch paths use it, so the
// host process's own memory is set the same way on each (HostMemoryArgs).
func profileHead(cacheRAMMiB, ctxCheckpoints int) []string {
	args := append([]string{"--jinja"}, HostMemoryArgs(cacheRAMMiB, ctxCheckpoints)...)
	// Always raise the log verbosity, in this fixed position (after the host
	// memory flags, before the placement flags): see LlamaLogVerbosity.
	return append(args, "-lv", strconv.Itoa(LlamaLogVerbosity))
}

// profileHostMemory is the prompt cache and restore points a launch of p
// passes: the profile's own --cache-ram and --ctx-checkpoints in its
// extra_args (llamaflags) when it has them, else the cluster values of the
// path it is launched on (cacheRAMMiB, ctxCheckpoints).
func profileHostMemory(p core.Profile, cacheRAMMiB, ctxCheckpoints int) (int, int) {
	h, _ := llamaflags.Parse(p.ExtraArgs)
	if h.HasCacheRAM {
		cacheRAMMiB = h.CacheRAMMiB
	}
	if h.HasCtxCheckpoints {
		ctxCheckpoints = h.CtxCheckpoints
	}
	return cacheRAMMiB, ctxCheckpoints
}

// HostMemoryArgs is what a launch says about the host process's own memory,
// always explicit: the prompt cache (--cache-ram, MiB per loaded model) and
// the context checkpoints per slot (--ctx-checkpoints; the server's default
// is 32, which only recurrent and sliding-window models fill).
func HostMemoryArgs(cacheRAMMiB, ctxCheckpoints int) []string {
	return []string{"--cache-ram", strconv.Itoa(cacheRAMMiB), "--ctx-checkpoints", strconv.Itoa(ctxCheckpoints)}
}

// profileTail is the profile's own part, last on the command line.
func profileTail(p core.Profile) []string {
	var args []string
	if p.KVQuant != "" && p.KVQuant != "f16" {
		args = append(args, "-ctk", p.KVQuant, "-ctv", p.KVQuant)
	}
	if p.FlashAttn {
		args = append(args, "--flash-attn", "on")
	}
	// the profile's own --ctx-checkpoints and --cache-ram are passed in
	// profileHead, in place of the cluster value: never twice on one line
	return append(args, llamaflags.Strip(p.ExtraArgs)...)
}

// HostLaunchArgs is the extra_args of a host launch on a node (phase 6): the
// part of BuildArgs that does not depend on where the program runs. The
// agent adds --host, --port, the model, -c, --parallel, --rpc, --device and
// --tensor-split itself. It differs from BuildArgs only in --cache-ram (the
// setting node_host_cache_ram_mib, default 0: no prompt cache in the node's
// RAM) and in --no-mmap, which o.NoMmap adds (the setting host_no_mmap):
// weights then sit in anonymous memory, counted in MemAvailable and watched
// by the watchdog, instead of pages mapped from the share.
func HostLaunchArgs(p core.Profile, o HostOptions) []string {
	args := []string{"--n-gpu-layers", "999", "--fit", "off"}
	args = append(args, profileHead(profileHostMemory(p, o.CacheRAMMiB, o.CtxCheckpoints))...)
	if o.NoMmap {
		args = append(args, "--no-mmap")
	}
	return append(args, profileTail(p)...)
}

// steadySplit: every node of the placement carries a steady split weight
// (core.PlacedNode.SplitMiB, set by the placer's stable order).
func steadySplit(pl core.Placement) bool {
	for _, node := range pl.Nodes {
		if node.SplitMiB <= 0 {
			return false
		}
	}
	return len(pl.Nodes) > 1
}

// splitWeights returns the per-node tensor split weights in placement order:
// the placement's explicit split, else the profile's, else the steady split
// weights when every node has one, else the node budgets.
// For a single node it returns that node's weight without validating it.
func splitWeights(p core.Profile, pl core.Placement) ([]int, error) {
	n := len(pl.Nodes)
	if n == 1 {
		return []int{1}, nil
	}
	var w []int
	switch {
	case len(pl.TensorSplit) > 0:
		w = pl.TensorSplit
	case len(p.TensorSplit) > 0:
		w = p.TensorSplit
	case steadySplit(pl):
		w = make([]int, n)
		for i, node := range pl.Nodes {
			w[i] = node.SplitMiB
		}
	default:
		w = make([]int, n)
		for i, node := range pl.Nodes {
			// Forced placement has no fit check (design 7.3), so a budget
			// can be zero or negative; weigh such a node at the minimum.
			w[i] = max(node.BudgetMiB, 1)
		}
	}
	if len(w) != n {
		return nil, fmt.Errorf("instance: tensor split has %d values for %d nodes", len(w), n)
	}
	sum := 0
	for _, v := range w {
		if v < 0 {
			return nil, errors.New("instance: tensor split has a negative value")
		}
		sum += v
	}
	if sum <= 0 {
		return nil, errors.New("instance: tensor split has no positive value")
	}
	return append([]int(nil), w...), nil
}

// ExpandCommand replaces ${PORT} in cmd and splits it into argv using
// shell-like rules, without invoking a shell: whitespace separates words;
// single quotes are literal; inside double quotes a backslash escapes only
// '"', '\' and '$'; outside quotes a backslash escapes the next character.
// No other expansion ($VAR, globs, ~) is performed.
func ExpandCommand(cmd string, port int) ([]string, error) {
	cmd = strings.ReplaceAll(cmd, "${PORT}", strconv.Itoa(port))
	var (
		args    []string
		cur     strings.Builder
		inWord  bool
		quote   byte // 0, '\'' or '"'
		escaped bool
	)
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case escaped:
			cur.WriteByte(c)
			escaped = false
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case quote == '"':
			switch {
			case c == '"':
				quote = 0
			case c == '\\' && i+1 < len(cmd) && strings.IndexByte("\"\\$", cmd[i+1]) >= 0:
				i++
				cur.WriteByte(cmd[i])
			default:
				cur.WriteByte(c)
			}
		case c == '\\':
			escaped, inWord = true, true
		case c == '\'' || c == '"':
			quote, inWord = c, true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if inWord {
				args = append(args, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if escaped {
		return nil, errors.New("instance: command ends with a backslash")
	}
	if quote != 0 {
		return nil, fmt.Errorf("instance: unterminated %c quote in command", quote)
	}
	if inWord {
		args = append(args, cur.String())
	}
	if len(args) == 0 {
		return nil, errors.New("instance: empty command")
	}
	return args, nil
}

// RenderCommand renders argv as the single string shown in /running. Words
// are joined by single spaces; a word containing anything beyond
// [A-Za-z0-9_@%+=:,./-] is single-quoted, so ordinary llama arguments such as
// "--rpc a:1,b:2" and "--ctx-size 8192" appear verbatim, as the hub's
// regexes (design A.1) require.
func RenderCommand(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = shellQuote(a)
	}
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("_@%+=:,./-", c) >= 0) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ChildEnvOverrides returns the extra environment for a child (StartSpec.Env).
// A kind: llama child whose placement has no local node gets
// CUDA_VISIBLE_DEVICES="" (as the pre-flight probe does): llama.cpp
// initialises its CUDA backend whatever --device says, which costs about
// 102 MiB of the host GPU per child even when only RPC workers are used
// (design 7.4). With the local node in the placement the variable is left as
// inherited. kind: command profiles are never touched.
func ChildEnvOverrides(p core.Profile, pl core.Placement) []string {
	if p.Kind == core.KindCommand {
		return nil
	}
	for _, n := range pl.Nodes {
		if n.Local {
			return nil
		}
	}
	return []string{"CUDA_VISIBLE_DEVICES="}
}
