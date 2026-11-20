package devices

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/harness"
)

func TestMain(m *testing.M) {
	code := m.Run()
	harness.Cleanup()
	os.Exit(code)
}

const fixture = `ggml_vulkan: Found 1 Vulkan devices:
ggml_vulkan: 0 = AMD Custom GPU 0405 (RADV GFX1013) (radv) | uma: 1 | fp16: 1 | warp size: 64
Available devices:
  Vulkan0: AMD Custom GPU 0405 (RADV GFX1013) (14336 MiB, 14010 MiB free)
  CUDA0: NVIDIA GeForce RTX 5060 Laptop GPU (8151 MiB, 7000 MiB free)
  RPC0: 192.0.2.2:50052 (15000 MiB, 14000 MiB free)
  CPU: AMD Ryzen (32000 MiB, 30000 MiB free)
`

func TestParse(t *testing.T) {
	d := Parse(fixture)
	if len(d) != 2 {
		t.Fatalf("%+v", d)
	}
	if d[0].Name != "Vulkan0" || d[0].Backend != "vulkan" || d[0].TotalMiB != 14336 || d[0].FreeMiB != 14010 || d[0].Description != "AMD Custom GPU 0405 (RADV GFX1013)" {
		t.Fatalf("%+v", d[0])
	}
	if d[1].Name != "CUDA0" || d[1].Backend != "cuda" {
		t.Fatalf("%+v", d[1])
	}
	if len(Parse("Available devices:\n")) != 0 {
		t.Fatal("empty list")
	}
}

// Real 0.1.0 outputs, as reported from the machines.
const realVulkan = "Available devices:\n  Vulkan0: AMD Custom Zen 2 (Oberon) (RADV GFX1013) (14947 MiB, 14900 MiB free)\n"
const realCUDA = "Available devices:\n  CUDA0: NVIDIA GeForce RTX 5060 Laptop GPU (7707 MiB, 7596 MiB free)\n"

const cudaBanner = `
==========
== CUDA ==
==========

CUDA Version 13.3.1

Container image Copyright (c) 2016-2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
This container image and its contents are governed by the NVIDIA Deep Learning Container License.
  Fake0: looks like a device (1 MiB, 1 MiB free)
`

func fixed(out string, err error) Runner {
	return func(context.Context, string, []string, []string) ([]byte, error) { return []byte(out), err }
}

func TestRealOutputs(t *testing.T) {
	d := Parse(realVulkan)
	if len(d) != 1 || d[0].Name != "Vulkan0" || d[0].Backend != "vulkan" || d[0].TotalMiB != 14947 || d[0].FreeMiB != 14900 ||
		d[0].Description != "AMD Custom Zen 2 (Oberon) (RADV GFX1013)" || VendorOf(d[0].Description) != "amd" {
		t.Fatalf("%+v", d)
	}
	d = Parse(cudaBanner + realCUDA)
	if len(d) != 1 || d[0].Name != "CUDA0" || d[0].TotalMiB != 7707 || d[0].FreeMiB != 7596 {
		t.Fatalf("banner before the list must be ignored: %+v", d)
	}
	devs, err := Read(context.Background(), fixed(cudaBanner+realCUDA, nil), "llama-server", nil, nil, time.Second)
	if err != nil || len(devs) != 1 || devs[0].SharesSystemMemory {
		t.Fatal(devs, err)
	}
	devs, err = Read(context.Background(), fixed(realVulkan, nil), "llama-server", nil,
		[]GPU{{Vendor: "amd", Integrated: true, PCI: "0000:20:00.0"}}, time.Second)
	if err != nil || len(devs) != 1 || !devs[0].SharesSystemMemory || devs[0].TotalMiB != 14947 {
		t.Fatal(devs, err)
	}
}

func TestNoDeviceAccess(t *testing.T) {
	for _, out := range []string{"Available devices:\n  (none)\n", cudaBanner + "Available devices:\n  (none)\n", "Available devices:\n"} {
		_, err := Read(context.Background(), fixed(out, nil), "llama-server", nil, nil, time.Second)
		if !errors.Is(err, ErrNoDevices) {
			t.Fatalf("%q: %v", out, err)
		}
	}
	// rpc-server prints its help: no list at all is a different error.
	_, err := Read(context.Background(), fixed("usage: rpc-server [options]\n  -H, --host HOST\n", nil), "rpc-server", nil, nil, time.Second)
	if err == nil || errors.Is(err, ErrNoDevices) {
		t.Fatal(err)
	}
}

func TestSharesSystemMemory(t *testing.T) {
	devs := Parse(`  Vulkan0: AMD Radeon Graphics (RADV RAPHAEL) (16384 MiB, 15000 MiB free)
  Vulkan1: NVIDIA GeForce RTX 5060 (8151 MiB, 7000 MiB free)
  CUDA0: NVIDIA GeForce RTX 5060 (8151 MiB, 7000 MiB free)
  Vulkan2: Some Integrated Thing (100 MiB, 50 MiB free)`)
	gpus := []GPU{{Vendor: "nvidia", PCI: "0000:01:00.0"}, {Vendor: "amd", Integrated: true, PCI: "0000:06:00.0"}}
	got := SharesSystemMemory(devs, gpus)
	want := []bool{true, false, false, true}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%d: %v", i, got)
		}
	}
}

func TestReadWithFakeLlama(t *testing.T) {
	bin := harness.BuildFakeLlama(t)
	run := func(ctx context.Context, path string, args, env []string) ([]byte, error) {
		return ExecRunner(ctx, path, args, append(env, "FAKELLAMA_DEVICES=Vulkan0:14336:14010"))
	}
	devs, err := Read(context.Background(), run, bin, nil, []GPU{{Vendor: "amd", Integrated: true}}, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 1 || devs[0].Name != "Vulkan0" || devs[0].TotalMiB != 14336 || devs[0].FreeMiB != 14010 {
		t.Fatalf("%+v", devs)
	}
}

func TestReadErrors(t *testing.T) {
	fail := func(ctx context.Context, path string, args, env []string) ([]byte, error) {
		return []byte("boom"), errors.New("exit 1")
	}
	if _, err := Read(context.Background(), fail, "x", nil, nil, time.Second); err == nil {
		t.Fatal("want error")
	}
	hang := func(ctx context.Context, path string, args, env []string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if _, err := Read(context.Background(), hang, "x", nil, nil, 20*time.Millisecond); err == nil {
		t.Fatal("want timeout")
	}
}
