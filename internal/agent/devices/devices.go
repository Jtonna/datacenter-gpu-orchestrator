// Package devices reads the node's llama devices from the llama program's
// `--list-devices` output (agent contract 4.3, D15). The agent does not
// compute device memory; it reports what llama prints.
//
// Format, as printed by the real 0.1.0 llama-server builds (Vulkan on a
// console, CUDA on an Nvidia machine):
//
//	Available devices:
//	  Vulkan0: AMD Custom Zen 2 (Oberon) (RADV GFX1013) (14947 MiB, 14900 MiB free)
//	  CUDA0: NVIDIA GeForce RTX 5060 Laptop GPU (7707 MiB, 7596 MiB free)
//
// One device per line: "<Backend><index>: <description> (<total> MiB, <free> MiB free)".
// The description itself contains parentheses; the memory figures are the
// LAST parenthesised group. Parsing starts after the "Available devices:"
// line and everything before it is ignored (images based on nvidia/cuda print
// a multi-line licence banner on stdout first). Other lines (stderr log
// output, blank lines) are ignored. Without access to the GPU device node
// the list is "  (none)": that is ErrNoDevices, not a parse error.
// rpc-server has no --list-devices (it prints its help): always run
// llama-server. Everything that depends on the format is in headerLine,
// noneLine, deviceLineRE and backendOf, to make a change easy.
package devices

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

const (
	headerLine = "Available devices:" // starts the device list
	noneLine   = "(none)"             // the whole list when no device is usable
)

// ErrNoDevices: llama-server printed an empty device list ("(none)" or no
// device line), which means the agent cannot open the GPU device node, or the
// machine has no supported GPU.
var ErrNoDevices = errors.New("llama-server lists no GPU device: the agent must run as root or in the render and video groups (or this machine has no supported GPU)")

var deviceLineRE =regexp.MustCompile(`^\s*([A-Za-z][A-Za-z_]*?)(\d+)\s*:\s*(.*?)\s*\(\s*(\d+)\s*MiB\s*,\s*(\d+)\s*MiB\s+free\s*\)\s*$`)

// Parsed is one device line.
type Parsed struct {
	Name        string // e.g. "Vulkan0"
	Backend     string // "vulkan", "cuda", ... lower case
	Index       int
	Description string
	TotalMiB    int64
	FreeMiB     int64
}

// backendOf maps llama's device name prefix to a backend word. Devices that
// are not this node's own accelerators (RPC, CPU) return "".
func backendOf(prefix string) string {
	switch strings.ToLower(prefix) {
	case "vulkan":
		return agentproto.BackendVulkan
	case "cuda":
		return agentproto.BackendCUDA
	case "rpc", "cpu", "blas":
		return ""
	}
	return strings.ToLower(prefix)
}

// listBody returns the text after the "Available devices:" line, and whether
// that line was found (else the whole text).
func listBody(out string) (string, bool) {
	i := strings.Index(out, headerLine)
	if i < 0 {
		return out, false
	}
	return out[i+len(headerLine):], true
}

// Parse returns the devices in the order printed. When the output has the
// "Available devices:" line, only what follows it is read.
func Parse(out string) []Parsed {
	body, _ := listBody(out)
	var devs []Parsed
	for _, line := range strings.Split(body, "\n") {
		m := deviceLineRE.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		b := backendOf(m[1])
		if b == "" {
			continue
		}
		idx, _ := strconv.Atoi(m[2])
		tot, _ := strconv.ParseInt(m[4], 10, 64)
		free, _ := strconv.ParseInt(m[5], 10, 64)
		devs = append(devs, Parsed{Name: m[1] + m[2], Backend: b, Index: idx, Description: m[3], TotalMiB: tot, FreeMiB: free})
	}
	return devs
}

// GPU is one GPU of the machine's hardware inventory.
type GPU struct {
	Vendor     string // amd, nvidia, intel, other
	Integrated bool
	PCI        string // "0000:20:00.0", may be empty
}

// VendorOf guesses a device's vendor from llama's description.
func VendorOf(desc string) string {
	d := strings.ToLower(desc)
	switch {
	case strings.Contains(d, "nvidia") || strings.Contains(d, "geforce") || strings.Contains(d, "quadro") || strings.Contains(d, "rtx"):
		return agentproto.VendorNvidia
	case strings.Contains(d, "amd") || strings.Contains(d, "radeon") || strings.Contains(d, "radv"):
		return agentproto.VendorAMD
	case strings.Contains(d, "intel"):
		return agentproto.VendorIntel
	}
	return agentproto.VendorOther
}

// SharesSystemMemory decides shares_system_memory per device.
//
// CONTRACT-QUESTION: 4.3 says shares_system_memory "is true for an
// integrated device" but llama's device list does not say which devices are
// integrated. The agent matches devices to the machine's GPUs with the stats
// contract's rule (5.6): CUDA devices are Nvidia cards (never integrated);
// the n-th Vulkan device of a vendor is the n-th GPU of that vendor ordered
// by PCI address. A device that cannot be matched counts as integrated when
// its description says "integrated", else as not sharing.
func SharesSystemMemory(devs []Parsed, gpus []GPU) []bool {
	byVendor := map[string][]GPU{}
	for _, g := range gpus {
		byVendor[g.Vendor] = append(byVendor[g.Vendor], g)
	}
	for v := range byVendor {
		gs := byVendor[v]
		sort.SliceStable(gs, func(i, j int) bool { return gs[i].PCI < gs[j].PCI })
	}
	seen := map[string]int{}
	out := make([]bool, len(devs))
	for i, d := range devs {
		if d.Backend == agentproto.BackendCUDA {
			continue
		}
		v := VendorOf(d.Description)
		n := seen[v]
		seen[v]++
		if gs := byVendor[v]; n < len(gs) {
			out[i] = gs[n].Integrated
			continue
		}
		out[i] = strings.Contains(strings.ToLower(d.Description), "integrated")
	}
	return out
}

// Runner runs a program and returns its combined output.
type Runner func(ctx context.Context, path string, args, env []string) ([]byte, error)

// Read runs `llama-server --list-devices` and builds the heartbeat devices
// (ReadAgeS is filled in by the caller at heartbeat time). llamaPath must be
// llama-server: rpc-server has no --list-devices.
//
// Errors: ErrNoDevices when the list is "(none)" or holds no device of this
// node's own (a plain message for programs_error and the status screen);
// another error when the output has no "Available devices:" line at all.
func Read(ctx context.Context, run Runner, llamaPath string, env []string, gpus []GPU, timeout time.Duration) ([]agentproto.Device, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := run(c, llamaPath, []string{"--list-devices"}, env)
	_, found := listBody(string(out))
	devs := Parse(string(out))
	if !found {
		if err != nil {
			return nil, fmt.Errorf("%s --list-devices: %w", llamaPath, err)
		}
		return nil, errors.New("--list-devices printed no \"" + headerLine + "\" line: " + firstLine(string(out)))
	}
	if len(devs) == 0 {
		// noneLine, or only RPC/CPU devices: nothing this node can offer.
		return nil, ErrNoDevices
	}
	shares := SharesSystemMemory(devs, gpus)
	res := make([]agentproto.Device, len(devs))
	for i, d := range devs {
		res[i] = agentproto.Device{Name: d.Name, Backend: d.Backend, TotalMiB: d.TotalMiB, FreeMiB: d.FreeMiB, SharesSystemMemory: shares[i]}
	}
	return res, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
