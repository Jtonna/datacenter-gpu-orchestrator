package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"reflect"
	"sort"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
)

// Recipes (data model step 2): a step that is the use of a recipe has
// Spec.Recipe and Spec.Applied set and a generated profile id. Just before the
// step the runner makes that profile from the inputs: the model, the context,
// the zones the placement names (force_nodes), and the options. Placeable zones
// are the agent nodes that are not excluded and are not the scheduler's own
// machine (decision D29: placements offer agent zones only).

// Zone is a node a placement may use.
type Zone struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Placeable   bool   `json:"placeable"`
	Unplaced    string `json:"unplaced,omitempty"`
	MemAvailMiB int64  `json:"mem_available_mib,omitempty"`
}

// ProfileMaker is the part of the scheduler API that makes profiles from
// inputs. HTTPAPI has it; a scripted API without it fails every use of a
// recipe with a plain error.
type ProfileMaker interface {
	// Zones lists the placeable zones (agent nodes, not excluded, not the
	// scheduler's machine), whether each can take a load now.
	Zones(ctx context.Context) ([]Zone, error)
	// EnsureProfile creates or replaces a generated profile so that it has
	// these settings; a profile that is already so is left alone.
	EnsureProfile(ctx context.Context, id string, spec map[string]any) error
}

// ErrPlacement is a placement that cannot be met now: the step is refused.
var ErrPlacement = errors.New("placement refused")

// placeOn picks the zones of a placement: nil for auto (Potluck chooses).
func placeOn(p benchspec.Placement, zs []Zone) ([]string, error) {
	var ok []Zone
	for _, c := range zs {
		if c.Placeable {
			ok = append(ok, c)
		}
	}
	sort.SliceStable(ok, func(i, j int) bool {
		if ok[i].MemAvailMiB != ok[j].MemAvailMiB {
			return ok[i].MemAvailMiB > ok[j].MemAvailMiB
		}
		return ok[i].Name < ok[j].Name
	})
	names := func(cs []Zone) []string {
		out := make([]string, len(cs))
		for i, c := range cs {
			out[i] = c.Name
		}
		return out
	}
	switch p.Mode {
	case benchspec.PlaceAuto, "":
		return nil, nil
	case benchspec.PlaceAll:
		if len(ok) == 0 {
			return nil, fmt.Errorf("%w: all zones: no zone can take a load now", ErrPlacement)
		}
		sort.SliceStable(ok, func(i, j int) bool { return ok[i].Name < ok[j].Name })
		return names(ok), nil
	case benchspec.PlaceAny:
		if len(ok) < p.N {
			return nil, fmt.Errorf("%w: any %d zones: only %d can take a load now", ErrPlacement, p.N, len(ok))
		}
		chosen := ok[:p.N]
		sort.SliceStable(chosen, func(i, j int) bool { return chosen[i].Name < chosen[j].Name })
		return names(chosen), nil
	case benchspec.PlaceNodes:
		var out []string
		for _, want := range p.Nodes {
			var found *Zone
			for i := range zs {
				if zs[i].Name == want || zs[i].ID == want {
					found = &zs[i]
				}
			}
			switch {
			case found == nil:
				return nil, fmt.Errorf("%w: %q is not a placeable zone (an agent zone that is not excluded and not the scheduler's machine)", ErrPlacement, want)
			case !found.Placeable:
				why := found.Unplaced
				if why == "" {
					why = "not placeable"
				}
				return nil, fmt.Errorf("%w: zone %q cannot take a load now (%s)", ErrPlacement, want, why)
			}
			out = append(out, found.Name)
		}
		return out, nil
	}
	return nil, fmt.Errorf("%w: placement %q is not known", ErrPlacement, p.String())
}

// ProfileSpecOf is the generated profile's settings for these inputs on these zones.
func ProfileSpecOf(in benchspec.Inputs, nodes []string) map[string]any {
	spec := map[string]any{"model": in.Model, "ctx": in.Ctx, "generated": true, "stage": "test"}
	if len(nodes) > 0 {
		spec["force_nodes"] = nodes
	}
	if in.Thinking != nil && !*in.Thinking {
		spec["extra_args"] = []string{"--reasoning-budget", "0"}
	}
	if in.KVQuant != "" {
		spec["kv_quant"] = in.KVQuant
	}
	if in.FlashAttn != nil {
		spec["flash_attn"] = *in.FlashAttn
	}
	return spec
}

// makeProfile makes the profile of a recipe's use. A placement that cannot be
// met is ErrPlacement (the step is refused); anything else is an error.
func (x *suiteExec) makeProfile(ctx context.Context, b *benchspec.Benchmark) error {
	pm, ok := x.m.o.API.(ProfileMaker)
	if !ok {
		return errors.New("this scheduler API cannot make profiles from inputs")
	}
	var nodes []string
	if p := b.Applied.Place(); p.Mode != benchspec.PlaceAuto {
		zs, err := pm.Zones(ctx)
		if err != nil {
			return fmt.Errorf("list the zones: %w", err)
		}
		if nodes, err = placeOn(p, zs); err != nil {
			return err
		}
	}
	if err := pm.EnsureProfile(ctx, b.Profile, ProfileSpecOf(*b.Applied, nodes)); err != nil {
		return fmt.Errorf("make the profile %s: %w", b.Profile, err)
	}
	return nil
}

// ---- HTTP ----

// Zones implements ProfileMaker: GET /api/nodes.
func (h *HTTPAPI) Zones(ctx context.Context) ([]Zone, error) {
	resp, err := h.do(ctx, "GET", "/api/nodes", nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw := readLimited(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("/api/nodes answered %d: %s", resp.StatusCode, Head(strings.TrimSpace(string(raw)), 300))
	}
	var feed struct {
		Nodes []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			State       string `json:"state"`
			Local       bool   `json:"local"`
			HostAgent   bool   `json:"host_agent"`
			Excluded    bool   `json:"excluded"`
			Placeable   bool   `json:"placeable"`
			Unplaced    string `json:"unplaced"`
			MemAvail    *int64 `json:"mem_available_mib"`
			MemAvailAvg *int64 `json:"mem_available_avg_mib"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &feed); err != nil {
		return nil, fmt.Errorf("/api/nodes: %w", err)
	}
	var out []Zone
	for _, n := range feed.Nodes {
		if n.Local || n.HostAgent || n.Excluded || n.State != "active" {
			continue
		}
		c := Zone{ID: n.ID, Name: n.Name, Placeable: n.Placeable, Unplaced: n.Unplaced}
		if c.Name == "" {
			c.Name = n.ID
		}
		if n.MemAvailAvg != nil {
			c.MemAvailMiB = *n.MemAvailAvg
		} else if n.MemAvail != nil {
			c.MemAvailMiB = *n.MemAvail
		}
		out = append(out, c)
	}
	return out, nil
}

// EnsureProfile implements ProfileMaker over /api/config/profiles. A profile
// with this id that a person wrote (generated: false) is never replaced.
func (h *HTTPAPI) EnsureProfile(ctx context.Context, id string, spec map[string]any) error {
	path := "/api/config/profiles/" + url.PathEscape(id)
	resp, err := h.do(ctx, "GET", path, nil, nil)
	if err != nil {
		return err
	}
	raw := readLimited(resp.Body)
	resp.Body.Close()
	method, body := "", map[string]any{"profile": spec, "comment": "made from a benchmark recipe's inputs"}
	switch resp.StatusCode {
	case 404:
		method, path = "POST", "/api/config/profiles"
		body["id"] = id
	case 200:
		var cur struct {
			Profile map[string]any `json:"profile"`
		}
		if err := json.Unmarshal(raw, &cur); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if g, ok := cur.Profile["generated"].(bool); ok && !g {
			return fmt.Errorf("profile %s exists and was written by a person (generated: false); it is not replaced", id)
		}
		if sameProfile(cur.Profile, spec) {
			return nil
		}
		// A person's marks and description stay (profiles slice B2): a stored
		// stage, display_name and description replace the spec's.
		kept := maps.Clone(spec)
		for _, k := range []string{"stage", "display_name", "description"} {
			if v, ok := cur.Profile[k].(string); ok && v != "" {
				kept[k] = v
			}
		}
		body["profile"] = kept
		method = "PUT"
	default:
		return fmt.Errorf("%s answered %d: %s", path, resp.StatusCode, Head(strings.TrimSpace(string(raw)), 300))
	}
	b, _ := json.Marshal(body)
	resp, err = h.do(ctx, method, path, b, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw = readLimited(resp.Body)
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		var eb errBody
		if json.Unmarshal(raw, &eb) == nil && eb.Error.Message != "" {
			return fmt.Errorf("%s %s answered %d: %s", method, path, resp.StatusCode, eb.Error.Message)
		}
		return fmt.Errorf("%s %s answered %d: %s", method, path, resp.StatusCode, Head(strings.TrimSpace(string(raw)), 300))
	}
	return nil
}

// sameProfile says whether a stored profile has exactly the settings of spec
// among the keys a generated profile sets (the others must be empty).
func sameProfile(cur, spec map[string]any) bool {
	norm := func(m map[string]any) map[string]any {
		b, _ := json.Marshal(m)
		var out map[string]any
		json.Unmarshal(b, &out)
		return out
	}
	c, s := norm(cur), norm(spec)
	empty := func(v any) bool {
		switch t := v.(type) {
		case nil:
			return true
		case []any:
			return len(t) == 0
		case string:
			return t == "" || t == "f16"
		case bool:
			return !t
		}
		return false
	}
	for _, k := range []string{"model", "ctx", "force_nodes", "extra_args", "kv_quant", "flash_attn", "generated"} {
		a, b := c[k], s[k]
		if empty(a) && empty(b) {
			continue
		}
		if !reflect.DeepEqual(a, b) {
			return false
		}
	}
	return true
}
