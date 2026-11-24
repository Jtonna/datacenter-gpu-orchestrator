package api

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/usage"
)

func modelShortNames(t *testing.T, e *env) map[string]string {
	t.Helper()
	var doc struct {
		Models []struct {
			File      string `json:"file"`
			ShortName string `json:"short_name"`
		} `json:"models"`
	}
	if err := json.Unmarshal([]byte(body(t, e.get("/api/models"))), &doc); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range doc.Models {
		got[m.File] = m.ShortName
	}
	return got
}

// /api/models entries carry short_name; a request row carries short_name and
// the profile's ctx as it was when the request came in.
func TestShortName_OnModelsAndRequestRows(t *testing.T) {
	e, st := usageEnv(t, chatOK, nil)
	if got := modelShortNames(t, e); len(got) != 3 || got["a.gguf"] != "a" || got["b.gguf"] != "b" || got["g.gguf"] != "g" {
		t.Fatalf("short names %v", got)
	}

	e.post("/v1/chat/completions", chat("alpha")).Body.Close()
	e.post("/v1/chat/completions", chat("beta:64k")).Body.Close()
	e.post("/v1/chat/completions", chat("nope")).Body.Close()
	eventually(t, "three ended", func() bool { return e.api.reqs.Ended() == 3 })
	flushT(t, st)
	rows := map[string][2]any{}
	for _, r := range listRequests(t, e, "").Requests {
		rows[r.Profile+"|"+r.Requested] = [2]any{r.ShortName, r.Ctx}
	}
	if rows["alpha|alpha"] != [2]any{"a", 4096} || rows["beta:64k|beta:64k"] != [2]any{"b", 65536} || rows["|nope"] != [2]any{"", 0} {
		t.Fatalf("rows %v", rows)
	}
}

// The suffix rule in one place: /api/models and the request rows name a
// file with the same modelname.Namer, built from the catalog's files. "-it"
// is dropped while no other catalog file would share the name; once the
// base model is in the catalog, the -it file keeps its suffix in both
// places. short_name is derived on read, so a row written before the base
// model arrived reads "gemma-4-27b-it" afterwards, as the catalog does.
func TestShortName_TheSuffixRuleIsTheCatalogsForModelsAndRows(t *testing.T) {
	e, st := usageEnv(t, chatOK, nil)
	it := "/models/Gemma-4-27B-it-Q4_K_M.gguf"
	base := "/models/gemma-4-27b-Q8_0.gguf"
	in := CatalogInput{Profiles: []map[string]any{{"id": "gemma", "model": it, "ctx": 32768.0}}}
	e.api.catalog.publish(newCatalogSnap(in, nil, "/models"))

	// A row of the -it file, written while the catalog has no base model.
	b := usage.Begin{Kind: usage.KindV1, Path: "/v1/chat/completions", Profile: "gemma", Requested: "gemma", ModelFile: it, Ctx: 32768,
		Caller: "anonymous", CallerKind: "default"}
	tr := e.api.reqs.Queued(b)
	tr.Ended(usage.End{Status: usage.StatusDone, HTTPStatus: 200, TokensIn: 1, TokensOut: 1, PrefillTPS: -1, DecodeTPS: -1})
	flushT(t, st)
	rowName := func() string {
		for _, r := range listRequests(t, e, "").Requests {
			if r.ModelFile == it {
				return r.ShortName
			}
		}
		return "(no row)"
	}
	if got := shortNamesOf(BuildModelCatalog(in, "")); got[it] != "gemma-4-27b" || rowName() != "gemma-4-27b" {
		t.Fatalf("alone: catalog %v, row %q", got, rowName())
	}

	// The base model arrives.
	in.Profiles = append(in.Profiles, map[string]any{"id": "gemma-base", "model": base, "ctx": 8192.0})
	e.api.catalog.publish(newCatalogSnap(in, nil, "/models"))
	if got := shortNamesOf(BuildModelCatalog(in, "")); got[it] != "gemma-4-27b-it" || got[base] != "gemma-4-27b" || rowName() != "gemma-4-27b-it" {
		t.Fatalf("with the base model: catalog %v, row %q", got, rowName())
	}
	var one usage.Request
	json.Unmarshal([]byte(body(t, e.get(fmt.Sprintf("/api/requests/%d", tr.ID())))), &one)
	if one.ShortName != "gemma-4-27b-it" || one.Ctx != 32768 {
		t.Fatalf("GET one row: %+v", one)
	}
}

func shortNamesOf(entries []map[string]any) map[string]string {
	out := map[string]string{}
	for _, e := range entries {
		out[jstr(e["file"])], _ = e["short_name"].(string)
	}
	return out
}
