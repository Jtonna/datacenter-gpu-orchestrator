// Package hw finds what the agent reports about the machine at
// registration: GPUs, backends and network interface facts, and checks that
// a program's shared libraries are present (contract 5.2). All reads are of
// files under a root directory (normally "/"), so the logic is tested with
// a fake tree on any OS.
package hw

import (
	"bufio"
	"debug/elf"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/devices"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// Machine reads hardware facts under Root.
type Machine struct {
	Root string // "/" on a real machine
	// LDLibraryPath is the value of LD_LIBRARY_PATH (searched first).
	LDLibraryPath string
}

func (m Machine) p(parts ...string) string {
	root := m.Root
	if root == "" {
		root = "/"
	}
	return filepath.Join(append([]string{root}, parts...)...)
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// GPUInfo is one GPU with what the registration and device matching need.
type GPUInfo struct {
	agentproto.GPU
	PCI string
}

// GPUs lists the GPUs under /sys/class/drm (card<N> entries only). With no
// card at all, a WSL2 or Docker Desktop GPU (/dev/dxg and libcuda.so.1) is
// reported as one Nvidia GPU; a machine with cards is never read that way.
//
// CONTRACT-QUESTION: 4.2 lists gpus with vendor, model and integrated but
// does not say how the agent finds them. The agent reads sysfs: vendor from
// the PCI vendor id, "integrated" for an AMD card with the stats contract's
// rule (VRAM <= 2 GiB and GTT >= 8 x VRAM, from mem_info_vram_total and
// mem_info_gtt_total), always for Intel, never for Nvidia; model from
// product_name, or for Nvidia /proc/driver/nvidia/gpus/<pci>/information,
// else "<vendor> <device id>".
func (m Machine) GPUs() []GPUInfo {
	ents, _ := os.ReadDir(m.p("sys", "class", "drm"))
	var out []GPUInfo
	seen := map[string]bool{}
	for _, e := range ents {
		name := e.Name()
		if !strings.HasPrefix(name, "card") || strings.Contains(name, "-") {
			continue
		}
		if _, err := strconv.Atoi(strings.TrimPrefix(name, "card")); err != nil {
			continue
		}
		dev := m.p("sys", "class", "drm", name, "device")
		vendorID := strings.ToLower(readTrim(filepath.Join(dev, "vendor")))
		if vendorID == "" {
			continue
		}
		pci := ""
		if target, err := filepath.EvalSymlinks(dev); err == nil {
			pci = filepath.Base(target)
		}
		if b := readTrim(filepath.Join(dev, "pci_address")); b != "" { // test trees without symlinks
			pci = b
		}
		if pci != "" && seen[pci] {
			continue
		}
		seen[pci] = true
		g := GPUInfo{PCI: pci}
		devID := readTrim(filepath.Join(dev, "device"))
		switch vendorID {
		case "0x1002":
			g.Vendor = agentproto.VendorAMD
			vram, _ := strconv.ParseInt(readTrim(filepath.Join(dev, "mem_info_vram_total")), 10, 64)
			gtt, _ := strconv.ParseInt(readTrim(filepath.Join(dev, "mem_info_gtt_total")), 10, 64)
			g.Integrated = vram > 0 && vram <= 2<<30 && gtt >= 8*vram
		case "0x10de":
			g.Vendor = agentproto.VendorNvidia
			g.Model = m.nvidiaModel(pci)
		case "0x8086":
			g.Vendor = agentproto.VendorIntel
			g.Integrated = true
		default:
			g.Vendor = agentproto.VendorOther
		}
		if g.Model == "" {
			g.Model = readTrim(filepath.Join(dev, "product_name"))
		}
		if g.Model == "" {
			g.Model = strings.ToUpper(g.Vendor) + " GPU " + devID
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PCI < out[j].PCI })
	if len(out) == 0 && m.gpuPV() {
		out = append(out, GPUInfo{GPU: agentproto.GPU{Vendor: agentproto.VendorNvidia, Model: GPUPVModel}})
	}
	return out
}

// GPUPVModel is the model reported for the GPU of a WSL2 or Docker Desktop
// machine (GPU paravirtualization), where no PCI facts can be read.
const GPUPVModel = "NVIDIA GPU (WSL2 GPU-PV)"

// gpuPV reports a WSL2 or Docker Desktop GPU (GPU paravirtualization): no
// drm card, but /dev/dxg and the Windows driver's libcuda.so.1 (under
// /usr/lib/wsl/lib). Only asked when no drm card was found.
func (m Machine) gpuPV() bool {
	if _, err := os.Stat(m.p("dev", "dxg")); err != nil {
		return false
	}
	return m.Libs().Has("libcuda.so.1")
}

func (m Machine) nvidiaModel(pci string) string {
	f, err := os.Open(m.p("proc", "driver", "nvidia", "gpus", pci, "information"))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), ":"); ok && strings.TrimSpace(k) == "Model" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// DeviceGPUs converts the inventory for device matching.
func DeviceGPUs(gs []GPUInfo) []devices.GPU {
	out := make([]devices.GPU, len(gs))
	for i, g := range gs {
		out[i] = devices.GPU{Vendor: g.Vendor, Integrated: g.Integrated, PCI: g.PCI}
	}
	return out
}

// Backends lists what the machine can run, from the drivers present (4.2):
// "cuda" when an Nvidia GPU and libcuda.so.1 are present; "vulkan" when the
// Vulkan loader and at least one Vulkan driver manifest (ICD) are present.
func (m Machine) Backends(gpus []GPUInfo) []string {
	out := []string{}
	nvidia := false
	for _, g := range gpus {
		if g.Vendor == agentproto.VendorNvidia {
			nvidia = true
		}
	}
	libs := m.Libs()
	if nvidia && libs.Has("libcuda.so.1") {
		out = append(out, agentproto.BackendCUDA)
	}
	if libs.Has("libvulkan.so.1") && m.hasVulkanICD() {
		out = append(out, agentproto.BackendVulkan)
	}
	return out
}

func (m Machine) hasVulkanICD() bool {
	for _, d := range []string{"usr/share/vulkan/icd.d", "etc/vulkan/icd.d", "usr/local/share/vulkan/icd.d"} {
		if ents, err := os.ReadDir(m.p(d)); err == nil {
			for _, e := range ents {
				if strings.HasSuffix(e.Name(), ".json") {
					return true
				}
			}
		}
	}
	return false
}

// IsPhysical reports whether a network interface is backed by a device
// (loopback, bridges, VLANs, docker0 and veth have no device link, or live
// under /sys/devices/virtual).
func (m Machine) IsPhysical(iface string) bool {
	if iface == "" || iface == "lo" {
		return false
	}
	link := m.p("sys", "class", "net", iface)
	if target, err := filepath.EvalSymlinks(link); err == nil && strings.Contains(filepath.ToSlash(target), "/devices/virtual/") {
		return false
	}
	if _, err := os.Stat(filepath.Join(link, "device")); err != nil {
		return false
	}
	for _, virt := range []string{"bridge", "bonding"} {
		if _, err := os.Stat(filepath.Join(link, virt)); err == nil {
			return false
		}
	}
	return true
}

// DefaultRouteInterface reads /proc/net/route and returns the interface of
// the default route with the lowest metric.
func (m Machine) DefaultRouteInterface() string {
	f, err := os.Open(m.p("proc", "net", "route"))
	if err != nil {
		return ""
	}
	defer f.Close()
	best, bestMetric := "", int64(-1)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fl := strings.Fields(sc.Text())
		if len(fl) < 8 || fl[1] != "00000000" || fl[7] != "00000000" {
			continue
		}
		metric, _ := strconv.ParseInt(fl[6], 10, 64)
		if bestMetric < 0 || metric < bestMetric {
			best, bestMetric = fl[0], metric
		}
	}
	return best
}

// LibSet is the set of shared library file names the dynamic loader can find.
type LibSet map[string]bool

// Has reports whether a library is present.
func (s LibSet) Has(name string) bool { return s[name] }

// LibDirs returns the loader's search directories: LD_LIBRARY_PATH, the
// directories of /etc/ld.so.conf (with its includes), and the defaults.
func (m Machine) LibDirs() []string {
	var dirs []string
	for _, d := range strings.Split(m.LDLibraryPath, ":") {
		if d != "" {
			dirs = append(dirs, d)
		}
	}
	dirs = append(dirs, m.ldSoConf("etc/ld.so.conf", 0)...)
	dirs = append(dirs, "/lib", "/usr/lib", "/lib64", "/usr/lib64",
		"/lib/x86_64-linux-gnu", "/usr/lib/x86_64-linux-gnu", "/lib/aarch64-linux-gnu", "/usr/lib/aarch64-linux-gnu",
		"/usr/local/lib", "/usr/local/cuda/lib64", "/usr/local/nvidia/lib64", "/usr/local/nvidia/lib",
		"/usr/lib/wsl/lib") // WSL2 / Docker Desktop: the Windows driver's libcuda.so.1
	return dirs
}

func (m Machine) ldSoConf(rel string, depth int) []string {
	if depth > 4 {
		return nil
	}
	f, err := os.Open(m.p(rel))
	if err != nil {
		return nil
	}
	defer f.Close()
	var dirs []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "include ") {
			pat := strings.TrimSpace(strings.TrimPrefix(line, "include "))
			if !strings.HasPrefix(pat, "/") {
				pat = filepath.ToSlash(filepath.Join(filepath.Dir("/"+rel), pat))
			}
			matches, _ := filepath.Glob(m.p(strings.TrimPrefix(pat, "/")))
			sort.Strings(matches)
			for _, mt := range matches {
				r, err := filepath.Rel(m.p(), mt)
				if err == nil {
					dirs = append(dirs, m.ldSoConf(filepath.ToSlash(r), depth+1)...)
				}
			}
			continue
		}
		dirs = append(dirs, line)
	}
	return dirs
}

// Libs scans the search directories once.
func (m Machine) Libs() LibSet {
	set := LibSet{}
	for _, d := range m.LibDirs() {
		ents, err := os.ReadDir(m.p(strings.TrimPrefix(d, "/")))
		if err != nil {
			continue
		}
		for _, e := range ents {
			if strings.Contains(e.Name(), ".so") {
				set[e.Name()] = true
			}
		}
	}
	return set
}

// NeededLibs returns the DT_NEEDED entries of an ELF program.
func NeededLibs(path string) ([]string, error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ImportedLibraries()
}

// MissingLibs returns the libraries a program needs that the loader cannot
// find (contract 5.2: the GNU OpenMP runtime, the Vulkan loader, the CUDA
// runtime, and the C library). A file that is not ELF yields an error.
func MissingLibs(path string, libs LibSet) ([]string, error) {
	need, err := NeededLibs(path)
	if err != nil {
		return nil, err
	}
	var miss []string
	for _, n := range need {
		if !libs.Has(n) {
			miss = append(miss, n)
		}
	}
	return miss, nil
}
