package cfgsvc

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/modelpath"
)

// realisticProfiles are the shapes live's profiles have: a kind llama model
// is a plain file name (dots and underscores inside the name, the
// quantisation after a dot or a dash), command profiles have no model (their
// cmd may name absolute paths under /models: the rule is for `model` only),
// aliases, forced profiles with a tensor split, a load timeout.
var realisticProfiles = map[string]string{
	"tiny":           `{"model":"tinyllama-1.1b-chat-v1.0.Q4_K_M.gguf","ctx":2048,"needs_mib":900}`,
	"mixtral":        `{"model":"mixtral-8x7b-instruct-v0.1.Q4_K_M.gguf","ctx":4096}`,
	"deepseek-lite":  `{"model":"DeepSeek-V2-Lite-Chat.Q4_K_M.gguf","ctx":8192,"needs_mib":12000}`,
	"qwen-4b":        `{"model":"Qwen_Qwen3.5-4B-Q4_K_M.gguf","ctx":32768,"aliases":["qwen-small","qwen-4b-32k"]}`,
	"glm-32b":        `{"model":"THUDM_GLM-4-32B-0414-Q4_K_M.gguf","ctx":8192,"needs_mib":20000}`,
	"kimi-dev":       `{"model":"moonshotai_Kimi-Dev-72B-IQ2_S.gguf","ctx":4096}`,
	"devstral-2ps5":  `{"model":"Devstral-Small-2-24B-Instruct-2512-Q4_K_M.gguf","ctx":2048,"force_nodes":["console-1","console-2"],"tensor_split":[50,50],"load_timeout":"3h0m0s"}`,
	"gemma-12b-q8kv": `{"model":"gemma-4-12b-it-Q4_K_M.gguf","ctx":65536,"kv_quant":"q8_0","flash_attn":true,"allow_tiers":[1,2]}`,
	"image-server":   `{"kind":"command","cmd":"sd-server --listen-port ${PORT} --diffusion-model /models/flux-dev/model-Q4_K_S.gguf --vae /models/flux/ae.safetensors","check_endpoint":"/v1/models","needs_mib":7500,"stall_timeout":"0s"}`,
}

// Every profile of a realistic configuration still validates under the model
// path rule, through the real write path (an import, every file present),
// and comes back after a restart. Two sets: the shapes above, and the
// sanitised copy of a real configuration the dev cluster runs on
// (cmd/devcluster/realdata/config.json, 254 profiles, 13 model files).
func TestModelPathRule_RealisticConfigurationsStillValidate(t *testing.T) {
	t.Run("shapes", func(t *testing.T) {
		d := config.NewDocument()
		d.Profiles = map[string]*config.ProfileSpec{}
		for id, js := range realisticProfiles {
			p, err := ParseProfile([]byte(js))
			if err != nil {
				t.Fatalf("%s: %v", id, err)
			}
			d.Profiles[id] = p
		}
		d.Tiers = []config.TierSpec{{Match: config.MatchSpec{Hostname: "console-*"}, Priority: 1}, {Match: config.MatchSpec{Hostname: "laptop-*"}, Priority: 2}}
		importAndRestart(t, d)
	})
	t.Run("the real-data copy", func(t *testing.T) {
		raw, err := os.ReadFile(filepath.Join("..", "..", "cmd", "devcluster", "realdata", "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		var view struct {
			Profiles map[string]json.RawMessage `json:"profiles"`
			Tiers    []config.TierSpec          `json:"tiers"`
		}
		if err := json.Unmarshal(raw, &view); err != nil {
			t.Fatal(err)
		}
		if len(view.Profiles) < 200 {
			t.Fatalf("only %d profiles in the copy", len(view.Profiles))
		}
		d := config.NewDocument()
		d.Profiles = map[string]*config.ProfileSpec{}
		models := map[string]bool{}
		for id, js := range view.Profiles {
			p, err := ParseProfile(js)
			if err != nil {
				t.Fatalf("%s: %v", id, err)
			}
			d.Profiles[id] = p
			if p.Model != "" {
				models[p.Model] = true
			}
		}
		for m := range models {
			if strings.Contains(m, "/") || modelpath.Check(m) != nil {
				t.Errorf("model %q is not a plain file name", m)
			}
		}
		d.Tiers = view.Tiers
		importAndRestart(t, d)
	})
}

// importAndRestart imports d into a new store (every model file present),
// checks every profile is in the configuration, then opens a new service on
// the same store, as at the next start, and checks it again.
func importAndRestart(t *testing.T, d *config.Document) {
	t.Helper()
	ctx := context.Background()
	st, err := bench.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cs, err := cfgstore.Open(st)
	if err != nil {
		t.Fatal(err)
	}
	all := func(string) bool { return true }
	opts := Options{Store: cs, Boot: config.Bootstrap{ModelsDir: "/models"}, Parse: config.Options{FileExists: all}}
	svc := New(ctx, opts)
	if _, err := svc.Import(ctx, ui("x"), d, true); err != nil {
		t.Fatalf("import of %d profiles: %v", len(d.Profiles), err)
	}
	want := sortedIDs(d.Profiles)
	for i, s := range []*Service{svc, New(ctx, opts)} {
		if e := s.LoadError(); e != "" {
			t.Fatalf("service %d: %s", i, e)
		}
		got := sortedIDs(s.Document().Profiles)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("service %d: %d profiles, want %d", i, len(got), len(want))
		}
		if n := len(s.Config().Profiles); n != len(want) {
			t.Fatalf("service %d: %d profiles in the built configuration, want %d", i, n, len(want))
		}
	}
}

func sortedIDs(m map[string]*config.ProfileSpec) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
