package access

import (
	"strings"
	"testing"
)

func TestGrants_CanValidateMergeAndCheck(t *testing.T) {
	if got := Full().Can(); got != "Observe · switch zones · load and unload · change settings" {
		t.Errorf("full: %q", got)
	}
	if got := (Grants{Off, Allow, Ask, Off}).Can(); got != "Switch zones" {
		t.Errorf("zones only: %q", got)
	}
	if got := (Grants{Off, Off, Off, Off}).Can(); got != "Nothing" {
		t.Errorf("none: %q", got)
	}
	if err := (Grants{Ask, Off, Off, Off}).Validate(); err == nil {
		t.Error("observe may not be ask")
	}
	if _, err := Default().Merge(map[string]any{"models": " ASK "}); err != nil {
		t.Errorf("merge: %v", err)
	}
	if _, err := Default().Merge(map[string]any{"owner": "allow"}); err == nil {
		t.Error("merge of an unknown permission")
	}
	if g, err := ParseJSON(Default().JSON()); err != nil || g != Default() {
		t.Errorf("round trip: %v %v", g, err)
	}
	g := Grant{Name: "bot", Grants: Grants{Allow, Ask, Off, Off}}
	if Check(g, Observe) != nil || Check(g, Agents) != nil || Check(g, ModelAPI) != nil || Check(g, Open) != nil {
		t.Error("allowed or unchecked scopes refused")
	}
	for sc, typ := range map[Scope]string{Zones: "approval_required", Models: "permission_denied", Owner: "owner_only"} {
		r := Check(g, sc)
		if r == nil || r.Status != 403 || r.Type != typ || !strings.Contains(r.Message, "bot") {
			t.Errorf("%s: %+v", sc, r)
		}
	}
}
