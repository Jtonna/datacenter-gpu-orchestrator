package hw

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGPUsAndBackends(t *testing.T) {
	root := t.TempDir()
	// PS5-like AMD integrated GPU.
	write(t, root, "sys/class/drm/card0/device/vendor", "0x1002\n")
	write(t, root, "sys/class/drm/card0/device/device", "0x13fe\n")
	write(t, root, "sys/class/drm/card0/device/pci_address", "0000:20:00.0")
	write(t, root, "sys/class/drm/card0/device/mem_info_vram_total", "536870912\n")
	write(t, root, "sys/class/drm/card0/device/mem_info_gtt_total", "15137001472\n")
	write(t, root, "sys/class/drm/card0-HDMI-A-1/status", "connected")
	// Nvidia card.
	write(t, root, "sys/class/drm/card1/device/vendor", "0x10de")
	write(t, root, "sys/class/drm/card1/device/pci_address", "0000_01_00.0") // no ":" in Windows file names
	write(t, root, "proc/driver/nvidia/gpus/0000_01_00.0/information", "Model: \t NVIDIA GeForce RTX 5060 Laptop GPU\nIRQ: 1\n")
	// Discrete-sized AMD card.
	write(t, root, "sys/class/drm/card2/device/vendor", "0x1002")
	write(t, root, "sys/class/drm/card2/device/pci_address", "0000:03:00.0")
	write(t, root, "sys/class/drm/card2/device/mem_info_vram_total", "17179869184")
	write(t, root, "sys/class/drm/card2/device/mem_info_gtt_total", "33554432000")
	m := Machine{Root: root}
	g := m.GPUs()
	if len(g) != 3 {
		t.Fatalf("%+v", g)
	}
	by := map[string]GPUInfo{}
	for _, x := range g {
		by[x.PCI] = x
	}
	g = []GPUInfo{by["0000_01_00.0"], by["0000:03:00.0"], by["0000:20:00.0"]}
	if g[0].Vendor != "nvidia" || g[0].Model != "NVIDIA GeForce RTX 5060 Laptop GPU" || g[0].Integrated {
		t.Fatalf("%+v", g[0])
	}
	if g[1].Vendor != "amd" || g[1].Integrated {
		t.Fatalf("discrete amd %+v", g[1])
	}
	if g[2].Vendor != "amd" || !g[2].Integrated || g[2].PCI != "0000:20:00.0" {
		t.Fatalf("integrated amd %+v", g[2])
	}
	if b := m.Backends(g); len(b) != 0 {
		t.Fatalf("no libraries yet: %v", b)
	}
	write(t, root, "usr/lib/x86_64-linux-gnu/libvulkan.so.1", "")
	write(t, root, "usr/share/vulkan/icd.d/radeon_icd.x86_64.json", "{}")
	write(t, root, "etc/ld.so.conf", "include /etc/ld.so.conf.d/*.conf\n")
	write(t, root, "etc/ld.so.conf.d/nvidia.conf", "# driver\n/opt/nvidia/lib\n")
	write(t, root, "opt/nvidia/lib/libcuda.so.1", "")
	b := m.Backends(g)
	if len(b) != 2 || b[0] != "cuda" || b[1] != "vulkan" {
		t.Fatalf("%v", b)
	}
}

func TestLibDirsAndMissing(t *testing.T) {
	root := t.TempDir()
	write(t, root, "custom/libgomp.so.1", "")
	m := Machine{Root: root, LDLibraryPath: "/custom"}
	libs := m.Libs()
	if !libs.Has("libgomp.so.1") || libs.Has("libvulkan.so.1") {
		t.Fatal(libs)
	}
	if _, err := MissingLibs(filepath.Join(root, "custom", "libgomp.so.1"), libs); err == nil {
		t.Fatal("non-ELF must be an error")
	}
}

func TestNetFacts(t *testing.T) {
	root := t.TempDir()
	write(t, root, "proc/net/route", "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"+
		"docker0\t0000FEA9\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n"+
		"wlan0\t00000000\t0102A8C0\t0003\t0\t0\t600\t00000000\t0\t0\t0\n"+
		"enp64s0f2\t00000000\t010200C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n")
	write(t, root, "sys/class/net/enp64s0f2/device/vendor", "0x1")
	write(t, root, "sys/class/net/br0/bridge/x", "")
	write(t, root, "sys/class/net/br0/device/x", "")
	m := Machine{Root: root}
	if got := m.DefaultRouteInterface(); got != "enp64s0f2" {
		t.Fatal(got)
	}
	if !m.IsPhysical("enp64s0f2") || m.IsPhysical("br0") || m.IsPhysical("lo") || m.IsPhysical("veth1") {
		t.Fatal("physical")
	}
}

// WSL2 / Docker Desktop (GPU-PV): no drm card, only /dev/dxg and the
// driver's libcuda.so.1 under /usr/lib/wsl/lib. One Nvidia GPU is reported
// and the cuda backend follows.
func TestGPUsUnderGPUPV(t *testing.T) {
	root := t.TempDir()
	m := Machine{Root: root}
	write(t, root, "dev/dxg", "")
	if g := m.GPUs(); len(g) != 0 {
		t.Fatalf("/dev/dxg without libcuda.so.1 is no Nvidia GPU: %+v", g)
	}
	write(t, root, "usr/lib/wsl/lib/libcuda.so.1", "")
	g := m.GPUs()
	if len(g) != 1 || g[0].Vendor != "nvidia" || g[0].Integrated || g[0].Model == "" {
		t.Fatalf("%+v", g)
	}
	if b := m.Backends(g); len(b) != 1 || b[0] != "cuda" {
		t.Fatalf("%v", b)
	}
	// A machine with drm cards is read as before, /dev/dxg or not.
	write(t, root, "sys/class/drm/card0/device/vendor", "0x1002")
	write(t, root, "sys/class/drm/card0/device/pci_address", "0000:20:00.0")
	if g := m.GPUs(); len(g) != 1 || g[0].Vendor != "amd" {
		t.Fatalf("%+v", g)
	}
	// No /dev/dxg: nothing, even with the library.
	root2 := t.TempDir()
	write(t, root2, "usr/lib/wsl/lib/libcuda.so.1", "")
	if g := (Machine{Root: root2}).GPUs(); len(g) != 0 {
		t.Fatalf("%+v", g)
	}
}
