package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The model source of a new machine is never guessed: it is the request's
// model_source, else the setting default_model_source; with neither the
// create answers 400 and no script is made. The pending list says the
// setting for the dialog.
func TestPendingZone_ModelSourceFromSetting(t *testing.T) {
	e, _, _ := groupsEnv(t)
	got := status(t, e.post("/api/zones/pending", `{"name":"zone-new"}`), http.StatusBadRequest)
	if !strings.Contains(got, NoModelSource) {
		t.Fatalf("%s", got)
	}
	if NoModelSource != "no model source: set the default model source for new machines or give one for this zone" {
		t.Fatal(NoModelSource)
	}
	var list struct {
		Pending            []PendingZone `json:"pending"`
		DefaultModelSource *string       `json:"default_model_source"`
	}
	json.Unmarshal([]byte(status(t, e.get("/api/zones/pending"), 200)), &list)
	if len(list.Pending) != 0 || list.DefaultModelSource == nil || *list.DefaultModelSource != "" {
		t.Fatalf("%+v", list)
	}

	c := *e.sched.Config()
	c.DefaultModelSource = "share:192.0.2.1:/srv/models"
	e.sched.ApplyConfig(&c)
	var z PendingZone
	json.Unmarshal([]byte(status(t, e.post("/api/zones/pending", `{"name":"zone-new"}`), http.StatusCreated)), &z)
	if z.ModelSource != "share:192.0.2.1:/srv/models" {
		t.Fatalf("%+v", z)
	}
	if s := status(t, e.get(z.ScriptPath), 200); !strings.Contains(s, "--model-source 'share:192.0.2.1:/srv/models'") {
		t.Fatalf("script:\n%s", s)
	}
	json.Unmarshal([]byte(status(t, e.post("/api/zones/pending", `{"name":"zone-other","model_source":"dir:/srv/models"}`), http.StatusCreated)), &z)
	if z.ModelSource != "dir:/srv/models" {
		t.Fatalf("the request's own source: %+v", z)
	}
	json.Unmarshal([]byte(status(t, e.get("/api/zones/pending"), 200)), &list)
	if list.DefaultModelSource == nil || *list.DefaultModelSource != "share:192.0.2.1:/srv/models" {
		t.Fatalf("%+v", list)
	}
	for _, bad := range []string{"share:192.0.2.1", "http://192.0.2.1/models", "dir:relative", "share:a b:/x", "pull"} {
		status(t, e.post("/api/zones/pending", `{"name":"zone-bad","model_source":"`+bad+`"}`), http.StatusBadRequest)
	}
}

// default_model_source is a setting: empty by default, the three forms
// taken, anything else refused.
func TestDefaultModelSourceSetting(t *testing.T) {
	e, _, _ := cfgEnvWith(t, func(o *Options) { o.keyExpiryEvery = -1 })
	plain := map[string]string{"Content-Type": "application/json"}
	_, sm := e.cdo("GET", "/api/config/settings", "", nil)
	if v, ok := findSetting(sm, "default_model_source"); !ok || v != "" {
		t.Fatalf("default %v %v", v, ok)
	}
	for _, good := range []string{"share:192.0.2.1:/", "share:192.0.2.1:/srv/models", "dir:/srv/models", "push", ""} {
		if st, m := e.cdo("PATCH", "/api/config/settings", `{"default_model_source":"`+good+`"}`, plain); st != 200 {
			t.Fatalf("%q: %d %v", good, st, m)
		}
	}
	for _, bad := range []string{"share:192.0.2.1", "nfs://192.0.2.1/x", "dir:relative", "share:a'b:/", "push now"} {
		if st, m := e.cdo("PATCH", "/api/config/settings", `{"default_model_source":"`+bad+`"}`, plain); st != 400 {
			t.Fatalf("%q: %d %v", bad, st, m)
		}
	}
}
