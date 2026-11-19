package main

import (
	"fmt"
	"sort"
	"strings"
)

// The cluster the harness pretends to be: three PS5s, an excluded laptop and
// the local OMEN GPU, with about a dozen profiles. Everything scripted (status,
// history, benchmark results) refers to these.

type nodeDef struct {
	ID       string
	Host     string
	IP       string
	Local    bool
	Priority int
	TotalMiB int
	Headroom int
}

func (n nodeDef) Addr() string {
	if n.Local {
		return ""
	}
	return n.IP + ":50052"
}

// RPCName is the device buffer name of the node in a load log.
func (n nodeDef) RPCName() string {
	if n.Local {
		return "CUDA0"
	}
	return "RPC0[" + n.Addr() + "]"
}

func (n nodeDef) Budget() int { return n.TotalMiB - n.Headroom }

const ps5TotalMiB = 13722 // 13.4 GiB usable RAM per PS5 as the fleet reports it

var (
	ps5A  = nodeDef{ID: "a8:47:4a:90:32:a3", Host: "ps5-9032a330", IP: "10.0.0.21", Priority: 1, TotalMiB: ps5TotalMiB, Headroom: 1024}
	ps5B  = nodeDef{ID: "a8:47:4a:31:bd:9c", Host: "ps5-31bd9dc4", IP: "10.0.0.22", Priority: 1, TotalMiB: ps5TotalMiB, Headroom: 1024}
	ps5C  = nodeDef{ID: "a8:47:4a:40:7c:89", Host: "ps5-407c8925", IP: "10.0.0.23", Priority: 1, TotalMiB: ps5TotalMiB, Headroom: 1024}
	alien = nodeDef{ID: "6c:2b:59:0e:a1:77", Host: "alienware-m15", IP: "10.0.0.3", Priority: 2, TotalMiB: 8192, Headroom: 256}
	omen  = nodeDef{ID: "omen-runner", Host: "omen-runner", Local: true, Priority: 3, TotalMiB: 8151, Headroom: 450}
)

// allNodes is the order of view.nodes in the scripted status.
var allNodes = []nodeDef{ps5A, ps5B, ps5C, alien, omen}

func nodeByHost(h string) (nodeDef, bool) {
	for _, n := range allNodes {
		if n.Host == h || n.ID == h {
			return n, true
		}
	}
	return nodeDef{}, false
}

type profileDef struct {
	ID          string
	Model       string
	Ctx         int
	Parallel    int
	NeedsMiB    int
	Aliases     []string
	ForceNodes  []string
	TensorSplit []int
	KVQuant     string
	FlashAttn   bool
	TTL         string
}

const (
	fileGemma    = "gemma-4-12b-it-Q4_K_M.gguf"
	fileQwen4    = "Qwen_Qwen3.5-4B-Q4_K_M.gguf"
	fileDevstral = "Devstral-Small-2-24B-Instruct-2512-Q3_K_M.gguf"
	fileQwen27   = "Qwen_Qwen3.8-27B-Q4_K_M.gguf"
	fileKimi     = "Kimi-Dev-72B-IQ2_S.gguf"
	fileLlama70  = "Llama-3.3-70B-Instruct-Q2_K.gguf"
)

// modelBytes is the size each model file pretends to have (for load bytes).
var modelBytes = map[string]int64{
	fileGemma: 7_300_000_000, fileQwen4: 2_740_000_000, fileDevstral: 11_470_000_000,
	fileQwen27: 16_480_000_000, fileKimi: 25_520_000_000, fileLlama70: 26_380_000_000,
}

var profiles = []profileDef{
	{ID: "gemma-4-12b", Model: fileGemma, Ctx: 8192, NeedsMiB: 7900, Aliases: []string{"gemma-4-12b-1pool-8k", "gemma-4-12b-2pool-8k"}},
	{ID: "gemma-4-12b:48k", Model: fileGemma, Ctx: 49152, NeedsMiB: 8250, KVQuant: "q8_0", FlashAttn: true, Aliases: []string{"gemma-4-12b-2pool-48k"}},
	{ID: "qwen3.5-4b", Model: fileQwen4, Ctx: 16384, NeedsMiB: 3400},
	{ID: "devstral-24b", Model: fileDevstral, Ctx: 8192, NeedsMiB: 12100, Aliases: []string{"devstral-24b-q3"}},
	{ID: "qwen3.8-27b", Model: fileQwen27, Ctx: 16384, Parallel: 2, NeedsMiB: 19600, KVQuant: "q8_0", FlashAttn: true, Aliases: []string{"qwen-27b"}},
	{ID: "kimi-dev-72b-iq2s", Model: fileKimi, Ctx: 8192, NeedsMiB: 26800, Aliases: []string{"kimi-dev-72b"}},
	{ID: "llama33-70b-q2", Model: fileLlama70, Ctx: 131072, NeedsMiB: 47800},
	{ID: "bench-gemma-12b-1w", Model: fileGemma, Ctx: 8192, NeedsMiB: 7900, ForceNodes: []string{ps5A.Host}},
	{ID: "bench-gemma-12b-2w", Model: fileGemma, Ctx: 8192, NeedsMiB: 7900, ForceNodes: []string{ps5A.Host, ps5B.Host}, TensorSplit: []int{50, 50}},
	{ID: "bench-gemma-12b-3w", Model: fileGemma, Ctx: 8192, NeedsMiB: 7900, ForceNodes: []string{ps5A.Host, ps5B.Host, ps5C.Host}, TensorSplit: []int{34, 33, 33}},
	{ID: "bench-devstral-24b", Model: fileDevstral, Ctx: 8192, NeedsMiB: 12100},
	{ID: "bench-kimi-72b-3w", Model: fileKimi, Ctx: 8192, NeedsMiB: 26800, ForceNodes: []string{ps5A.Host, ps5B.Host, ps5C.Host}, TensorSplit: []int{34, 33, 33}},
}

func profileByID(id string) (profileDef, bool) {
	for _, p := range profiles {
		if p.ID == id {
			return p, true
		}
		for _, a := range p.Aliases {
			if a == id {
				return p, true
			}
		}
	}
	return profileDef{}, false
}

func modelFileOf(profile string) string {
	if p, ok := profileByID(profile); ok {
		return p.Model
	}
	return ""
}

func yamlList(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = fmt.Sprintf("%q", s)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// profilesYAML is the profiles: block of the generated config.
func profilesYAML() string {
	var b strings.Builder
	ps := append([]profileDef(nil), profiles...)
	sort.Slice(ps, func(i, j int) bool { return ps[i].ID < ps[j].ID })
	for _, p := range ps {
		fmt.Fprintf(&b, "  %q:\n    model: %s\n    ctx: %d\n    needs_mib: %d\n", p.ID, p.Model, p.Ctx, p.NeedsMiB)
		if p.Parallel > 0 {
			fmt.Fprintf(&b, "    parallel: %d\n", p.Parallel)
		}
		if p.KVQuant != "" {
			fmt.Fprintf(&b, "    kv_quant: %s\n", p.KVQuant)
		}
		if p.FlashAttn {
			b.WriteString("    flash_attn: true\n")
		}
		if len(p.Aliases) > 0 {
			fmt.Fprintf(&b, "    aliases: %s\n", yamlList(p.Aliases))
		}
		if len(p.ForceNodes) > 0 {
			fmt.Fprintf(&b, "    force_nodes: %s\n", yamlList(p.ForceNodes))
		}
		if len(p.TensorSplit) > 0 {
			parts := make([]string, len(p.TensorSplit))
			for i, v := range p.TensorSplit {
				parts[i] = fmt.Sprint(v)
			}
			fmt.Fprintf(&b, "    tensor_split: [%s]\n", strings.Join(parts, ", "))
		}
		if p.TTL != "" {
			fmt.Fprintf(&b, "    ttl: %s\n", p.TTL)
		}
	}
	return b.String()
}
