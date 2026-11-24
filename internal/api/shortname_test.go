package api

import (
	"encoding/json"
	"testing"
)

// /api/models entries carry short_name (modelname.Short of the file); a
// request row carries short_name and the profile's ctx as it was when the
// request came in.
func TestShortName_OnModelsAndRequestRows(t *testing.T) {
	e, st := usageEnv(t, chatOK, nil)

	var doc struct {
		Models []struct {
			Key       string `json:"key"`
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
	if len(got) != 3 || got["a.gguf"] != "a" || got["b.gguf"] != "b" || got["g.gguf"] != "g" {
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
