package instance

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/config"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
)

// BuildOptions are the policy values the command builder needs besides the
// profile and placement (design 6 defaults, plus models_dir).
type BuildOptions struct {
	ServerBin   string // defaults.server_bin
	ModelsDir   string // models_dir
	LocalDevice string // defaults.local_device
	CacheRAMMiB int    // defaults.cache_ram_mib
}

// OptionsFromConfig extracts BuildOptions from a validated config.
func OptionsFromConfig(c *config.Config) BuildOptions {
	return BuildOptions{
		ServerBin:   c.Defaults.ServerBin,
		ModelsDir:   c.ModelsDir,
		LocalDevice: c.Defaults.LocalDevice,
		CacheRAMMiB: c.Defaults.CacheRAMMiB,
	}
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
		"--model", joinModelPath(o.ModelsDir, p.Model),
		"--n-gpu-layers", "999",
		"--fit", "off",
		"--ctx-size", strconv.Itoa(p.Ctx * parallel),
		"--parallel", strconv.Itoa(parallel),
		"--jinja",
		"--cache-ram", strconv.Itoa(o.CacheRAMMiB),
		// Always raise the log verbosity, in this fixed position (right after
		// --cache-ram, before the placement flags): see LlamaLogVerbosity.
		"-lv", strconv.Itoa(LlamaLogVerbosity),
	}
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
	if p.KVQuant != "" && p.KVQuant != "f16" {
		args = append(args, "-ctk", p.KVQuant, "-ctv", p.KVQuant)
	}
	if p.FlashAttn {
		args = append(args, "--flash-attn", "on")
	}
	return append(args, p.ExtraArgs...), nil
}

// splitWeights returns the per-node tensor split weights in placement order:
// the placement's explicit split, else the profile's, else the node budgets.
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

// joinModelPath joins dir and file with the separator dir already uses, so a
// Linux "/models" never turns into a backslash path on a Windows dev machine.
func joinModelPath(dir, file string) string {
	if dir == "" {
		return file
	}
	sep := "/"
	if strings.Contains(dir, `\`) && !strings.Contains(dir, "/") {
		sep = `\`
	}
	return strings.TrimRight(dir, `/\`) + sep + file
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
