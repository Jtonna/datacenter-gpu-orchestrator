package benchspec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// The benchmark data model, step 2 (potluck-benchmark-data-model.md): a
// benchmark can be a recipe that takes inputs (model, context, placement,
// thinking and the cache options) instead of naming a fixed profile; a group
// (a suite) lists benchmarks with their inputs filled in, matrix entries that
// stand for many, and other groups. When a run is queued the group is
// expanded (ExpandGroup) into a flat list: every use of a recipe becomes an
// inline benchmark with a generated profile id, which the runner creates from
// the inputs just before the step. Plain references to benchmarks with a
// fixed profile are left as they are, so an old suite runs exactly as before.

// Limits of the data model.
const (
	MaxPlacementNodes = 16   // any:N and nodes:...
	MaxMatrixValues   = 64   // values of one matrix list
	MaxMatrixCombos   = 500  // uses one matrix entry may stand for
	MaxExpandedItems  = 1000 // entries of a group once expanded
	MaxGroupDepth     = 8    // groups inside groups
	MaxProfileIDLen   = 100  // generated profile ids
	MinInputCtx       = 256
	MaxInputCtx       = 1 << 20
)

// Placement modes.
const (
	PlaceAuto  = "auto"  // let Potluck choose
	PlaceAll   = "all"   // every console that can take a load
	PlaceAny   = "any"   // any N consoles
	PlaceNodes = "nodes" // these consoles
)

var nodeNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

// Placement says where the model of a recipe goes. Consoles only (decision
// D29): the runner picks among the agent nodes that are not excluded and are
// not the scheduler's own machine. Written as a string: "auto", "all",
// "any:2", "nodes:ps5-1,ps5-4"; an object {"any": 2}, {"all": true},
// {"auto": true} or {"nodes": [...]} is read too.
type Placement struct {
	Mode  string
	N     int
	Nodes []string
}

// String is the written form.
func (p Placement) String() string {
	switch p.Mode {
	case PlaceAny:
		return fmt.Sprintf("any:%d", p.N)
	case PlaceNodes:
		return "nodes:" + strings.Join(p.Nodes, ",")
	case "":
		return PlaceAuto
	}
	return p.Mode
}

// Validate checks the placement.
func (p Placement) Validate() error {
	switch p.Mode {
	case PlaceAuto, PlaceAll:
	case PlaceAny:
		if p.N < 1 || p.N > MaxPlacementNodes {
			return fmt.Errorf("placement any:N needs N from 1 to %d, got %d", MaxPlacementNodes, p.N)
		}
	case PlaceNodes:
		if len(p.Nodes) == 0 || len(p.Nodes) > MaxPlacementNodes {
			return fmt.Errorf("placement nodes: needs 1 to %d console names", MaxPlacementNodes)
		}
		seen := map[string]bool{}
		for _, n := range p.Nodes {
			if !nodeNameRe.MatchString(n) {
				return fmt.Errorf("placement nodes: %q is not a node name or id", n)
			}
			if seen[n] {
				return fmt.Errorf("placement nodes: %q is listed twice", n)
			}
			seen[n] = true
		}
	default:
		return fmt.Errorf("placement %q is not known: auto, all, any:N or nodes:a,b", p.Mode)
	}
	return nil
}

func parsePlacement(s string) (Placement, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == PlaceAuto || s == PlaceAll:
		return Placement{Mode: s}, nil
	case strings.HasPrefix(s, "any:"):
		n, err := strconv.Atoi(strings.TrimPrefix(s, "any:"))
		if err != nil {
			return Placement{}, fmt.Errorf("placement %q: any:N needs a number", s)
		}
		return Placement{Mode: PlaceAny, N: n}, nil
	case strings.HasPrefix(s, "nodes:"):
		var nodes []string
		for _, n := range strings.Split(strings.TrimPrefix(s, "nodes:"), ",") {
			if n = strings.TrimSpace(n); n != "" {
				nodes = append(nodes, n)
			}
		}
		return Placement{Mode: PlaceNodes, Nodes: nodes}, nil
	}
	return Placement{}, fmt.Errorf("placement %q is not known: auto, all, any:N or nodes:a,b", s)
}

type placementObj struct {
	Auto  bool     `json:"auto,omitempty" yaml:"auto,omitempty"`
	All   bool     `json:"all,omitempty" yaml:"all,omitempty"`
	Any   int      `json:"any,omitempty" yaml:"any,omitempty"`
	Nodes []string `json:"nodes,omitempty" yaml:"nodes,omitempty"`
}

func (o placementObj) placement() (Placement, error) {
	var out []Placement
	if o.Auto {
		out = append(out, Placement{Mode: PlaceAuto})
	}
	if o.All {
		out = append(out, Placement{Mode: PlaceAll})
	}
	if o.Any != 0 {
		out = append(out, Placement{Mode: PlaceAny, N: o.Any})
	}
	if o.Nodes != nil {
		out = append(out, Placement{Mode: PlaceNodes, Nodes: o.Nodes})
	}
	if len(out) != 1 {
		return Placement{}, errors.New("placement: give exactly one of auto, all, any or nodes")
	}
	return out[0], nil
}

// MarshalJSON writes the string form.
func (p Placement) MarshalJSON() ([]byte, error) { return json.Marshal(p.String()) }

// UnmarshalJSON reads the string or the object form.
func (p *Placement) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		v, err := parsePlacement(s)
		if err != nil {
			return err
		}
		*p = v
		return nil
	}
	var o placementObj
	if err := DecodeStrict(b, &o); err != nil {
		return fmt.Errorf("placement: %w", err)
	}
	v, err := o.placement()
	if err != nil {
		return err
	}
	*p = v
	return nil
}

// MarshalYAML writes the string form.
func (p Placement) MarshalYAML() (any, error) { return p.String(), nil }

// UnmarshalYAML reads a scalar or a mapping.
func (p *Placement) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		v, err := parsePlacement(n.Value)
		if err != nil {
			return err
		}
		*p = v
		return nil
	}
	var o placementObj
	if err := n.Decode(&o); err != nil {
		return err
	}
	v, err := o.placement()
	if err != nil {
		return err
	}
	*p = v
	return nil
}

func (p Placement) key() string {
	switch p.Mode {
	case PlaceAny:
		return fmt.Sprintf("any%d", p.N)
	case PlaceNodes:
		k := "on-" + slugPart(strings.Join(p.Nodes, "-"))
		if len(k) > 40 {
			return "on-" + shortHash(strings.Join(p.Nodes, ","), 8)
		}
		return k
	case "":
		return PlaceAuto
	}
	return p.Mode
}

// Inputs are what a recipe needs: the model file, the context per slot, the
// placement, and the options thinking (false turns the model's reasoning
// off), kv_quant and flash_attn. On a benchmark they are its defaults; on a
// group entry they fill in or override them.
type Inputs struct {
	Model     string     `json:"model,omitempty" yaml:"model,omitempty"`
	Ctx       int        `json:"ctx,omitempty" yaml:"ctx,omitempty"`
	Placement *Placement `json:"placement,omitempty" yaml:"placement,omitempty"`
	Thinking  *bool      `json:"thinking,omitempty" yaml:"thinking,omitempty"`
	KVQuant   string     `json:"kv_quant,omitempty" yaml:"kv_quant,omitempty"`
	FlashAttn *bool      `json:"flash_attn,omitempty" yaml:"flash_attn,omitempty"`
}

func (in *Inputs) validate(prefix string, add func(string, ...any)) {
	if in == nil {
		return
	}
	if m := in.Model; m != "" {
		if len(m) > 255 || strings.Contains(m, "..") || strings.HasPrefix(m, "/") || strings.Contains(m, `\`) {
			add("%s.model %q must be a model file name under the models directory", prefix, m)
		}
	}
	if in.Ctx != 0 && (in.Ctx < MinInputCtx || in.Ctx > MaxInputCtx) {
		add("%s.ctx must be %d to %d tokens, got %d", prefix, MinInputCtx, MaxInputCtx, in.Ctx)
	}
	switch in.KVQuant {
	case "", "f16", "q8_0", "q4_0":
	default:
		add("%s.kv_quant must be f16, q8_0 or q4_0, got %q", prefix, in.KVQuant)
	}
	if in.Placement != nil {
		if err := in.Placement.Validate(); err != nil {
			add("%s.%v", prefix, err)
		}
	}
}

// Over is in with every field it leaves empty taken from base.
func (in *Inputs) Over(base *Inputs) Inputs {
	var out Inputs
	if base != nil {
		out = *base
	}
	if in == nil {
		return out
	}
	if in.Model != "" {
		out.Model = in.Model
	}
	if in.Ctx != 0 {
		out.Ctx = in.Ctx
	}
	if in.Placement != nil {
		out.Placement = in.Placement
	}
	if in.Thinking != nil {
		out.Thinking = in.Thinking
	}
	if in.KVQuant != "" {
		out.KVQuant = in.KVQuant
	}
	if in.FlashAttn != nil {
		out.FlashAttn = in.FlashAttn
	}
	return out
}

// Missing names the inputs a run needs that are not given (model, ctx).
func (in Inputs) Missing() []string {
	var out []string
	if in.Model == "" {
		out = append(out, "model")
	}
	if in.Ctx == 0 {
		out = append(out, "ctx")
	}
	return out
}

// Place is the placement, auto when none is given.
func (in Inputs) Place() Placement {
	if in.Placement == nil {
		return Placement{Mode: PlaceAuto}
	}
	return *in.Placement
}

func slugPart(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func shortHash(s string, n int) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:n]
}

func ctxKey(ctx int) string {
	if ctx > 0 && ctx%1024 == 0 {
		return fmt.Sprintf("%dk", ctx/1024)
	}
	return strconv.Itoa(ctx)
}

// ProfileID is the id of the profile the runner makes from these inputs:
// bench-<model>-<placement>-<ctx>[-nothink|-think][-kv<quant>][-fa], the
// naming rule of generated profiles. The same inputs always give the same id.
func (in Inputs) ProfileID() string {
	model := strings.TrimSuffix(strings.TrimSuffix(path.Base(in.Model), ".gguf"), ".GGUF")
	stem := slugPart(model)
	rest := "-" + in.Place().key() + "-" + ctxKey(in.Ctx)
	if in.Thinking != nil {
		if *in.Thinking {
			rest += "-think"
		} else {
			rest += "-nothink"
		}
	}
	if in.KVQuant != "" && in.KVQuant != "f16" {
		rest += "-kv" + slugPart(in.KVQuant)
	}
	if in.FlashAttn != nil && *in.FlashAttn {
		rest += "-fa"
	}
	id := "bench-" + stem + rest
	if len(id) > MaxProfileIDLen {
		keep := MaxProfileIDLen - len("bench-") - len(rest) - 7
		if keep < 1 {
			keep = 1
		}
		if keep > len(stem) {
			keep = len(stem)
		}
		id = "bench-" + strings.Trim(stem[:keep], "-") + "-" + shortHash(in.Model, 6) + rest
	}
	return id
}

// Summary is the inputs in words for a step name: "gemma.gguf, 8k, any:2".
func (in Inputs) Summary() string {
	parts := []string{in.Model, ctxKey(in.Ctx), in.Place().String()}
	if in.Thinking != nil {
		if *in.Thinking {
			parts = append(parts, "thinking on")
		} else {
			parts = append(parts, "thinking off")
		}
	}
	if in.KVQuant != "" && in.KVQuant != "f16" {
		parts = append(parts, "kv "+in.KVQuant)
	}
	if in.FlashAttn != nil && *in.FlashAttn {
		parts = append(parts, "flash attention")
	}
	return strings.Join(parts, ", ")
}

// Matrix makes one group entry stand for many: every combination of the
// values given, model outermost, then placement, ctx and thinking.
type Matrix struct {
	Model     []string    `json:"model,omitempty" yaml:"model,omitempty"`
	Placement []Placement `json:"placement,omitempty" yaml:"placement,omitempty"`
	Ctx       []int       `json:"ctx,omitempty" yaml:"ctx,omitempty"`
	Thinking  []bool      `json:"thinking,omitempty" yaml:"thinking,omitempty"`
}

func (m *Matrix) count() int {
	n := 1
	for _, l := range []int{len(m.Model), len(m.Placement), len(m.Ctx), len(m.Thinking)} {
		if l > 0 {
			n *= l
		}
	}
	return n
}

func (m *Matrix) validate(prefix string, add func(string, ...any)) bool {
	if len(m.Model)+len(m.Placement)+len(m.Ctx)+len(m.Thinking) == 0 {
		add("%s.matrix: give at least one list of model, placement, ctx or thinking", prefix)
		return false
	}
	ok := true
	for name, l := range map[string]int{"model": len(m.Model), "placement": len(m.Placement), "ctx": len(m.Ctx), "thinking": len(m.Thinking)} {
		if l > MaxMatrixValues {
			add("%s.matrix.%s: at most %d values", prefix, name, MaxMatrixValues)
			ok = false
		}
	}
	if ok && m.count() > MaxMatrixCombos {
		add("%s.matrix stands for %d uses, more than %d", prefix, m.count(), MaxMatrixCombos)
		return false
	}
	for i, v := range m.Model {
		(&Inputs{Model: v}).validate(fmt.Sprintf("%s.matrix.model[%d]", prefix, i), add)
		if v == "" {
			add("%s.matrix.model[%d] is empty", prefix, i)
		}
	}
	for i, v := range m.Ctx {
		if v < MinInputCtx || v > MaxInputCtx {
			add("%s.matrix.ctx[%d] must be %d to %d tokens, got %d", prefix, i, MinInputCtx, MaxInputCtx, v)
		}
	}
	for i, p := range m.Placement {
		if err := p.Validate(); err != nil {
			add("%s.matrix.placement[%d]: %v", prefix, i, err)
		}
	}
	return ok
}

// Combos is every combination as inputs over base.
func (m *Matrix) Combos(base Inputs) []Inputs {
	out := []Inputs{base}
	if m == nil {
		return out
	}
	mul := func(n int, set func(in *Inputs, i int)) {
		if n == 0 {
			return
		}
		var next []Inputs
		for _, in := range out {
			for i := 0; i < n; i++ {
				c := in
				set(&c, i)
				next = append(next, c)
			}
		}
		out = next
	}
	mul(len(m.Model), func(in *Inputs, i int) { in.Model = m.Model[i] })
	mul(len(m.Placement), func(in *Inputs, i int) { p := m.Placement[i]; in.Placement = &p })
	mul(len(m.Ctx), func(in *Inputs, i int) { in.Ctx = m.Ctx[i] })
	mul(len(m.Thinking), func(in *Inputs, i int) { v := m.Thinking[i]; in.Thinking = &v })
	return out
}

// entryJSON is the object form of a group entry that is not an inline
// benchmark: {"benchmark": id, "inputs": {...}, "matrix": {...}} or {"group": id}.
type entryJSON struct {
	Benchmark string  `json:"benchmark,omitempty" yaml:"benchmark,omitempty"`
	Group     string  `json:"group,omitempty" yaml:"group,omitempty"`
	Inputs    *Inputs `json:"inputs,omitempty" yaml:"inputs,omitempty"`
	Matrix    *Matrix `json:"matrix,omitempty" yaml:"matrix,omitempty"`
}

func (i SuiteItem) isEntry() bool { return i.Group != "" || i.Inputs != nil || i.Matrix != nil }

func (i *SuiteItem) fromEntry(e entryJSON) {
	*i = SuiteItem{Ref: e.Benchmark, Group: e.Group, Inputs: e.Inputs, Matrix: e.Matrix}
}

func hasEntryKey(b []byte) bool {
	var keys map[string]json.RawMessage
	if json.Unmarshal(b, &keys) != nil {
		return false
	}
	_, b1 := keys["benchmark"]
	_, g1 := keys["group"]
	return b1 || g1
}

// GroupLookup finds a stored group (suite) by id.
type GroupLookup func(id string) (*Suite, bool)

// IsRecipe says whether the benchmark takes inputs instead of a fixed profile.
func (b *Benchmark) IsRecipe() bool { return b.Inputs != nil && !b.IsParallel() }

// groupPath checks that group id can be reached without a cycle back to
// self and no deeper than MaxGroupDepth.
func groupPath(self, id string, groups GroupLookup, depth int, seen map[string]bool) error {
	if id == self || seen[id] {
		return fmt.Errorf("group %q contains itself (through %q)", self, id)
	}
	if depth > MaxGroupDepth {
		return fmt.Errorf("groups are nested deeper than %d", MaxGroupDepth)
	}
	g, ok := groups(id)
	if !ok {
		return fmt.Errorf("%q is not a group", id)
	}
	seen[id] = true
	defer delete(seen, id)
	for _, it := range g.Benchmarks {
		if it.Group != "" {
			if err := groupPath(self, it.Group, groups, depth+1, seen); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateEntry checks the data-model parts of a group entry.
func (s *Suite) validateEntry(i int, it SuiteItem, look Lookup, groups GroupLookup, add func(string, ...any)) {
	p := fmt.Sprintf("benchmarks[%d]", i)
	if it.Group != "" {
		switch {
		case it.Ref != "":
			add("%s: an entry names a benchmark or group, not both", p)
		case it.Inputs != nil || it.Matrix != nil:
			add("%s: a group entry takes no inputs or matrix (its own entries have them)", p)
		case groups == nil:
			add("%s: %q is not a group", p, it.Group)
		default:
			if err := groupPath(s.ID, it.Group, groups, 1, map[string]bool{}); err != nil {
				add("%s: %v", p, err)
			}
		}
		return
	}
	it.Inputs.validate(p+".inputs", add)
	if it.Matrix != nil && !it.Matrix.validate(p, add) {
		return
	}
	var b *Benchmark
	if it.Inline != nil {
		b = it.Inline
	} else if look != nil {
		b, _ = look(it.Ref)
	}
	if b == nil {
		return // reported by Validate
	}
	if !b.IsRecipe() {
		if it.Inputs != nil || it.Matrix != nil {
			add("%s: benchmark %q has a fixed profile and takes no inputs", p, b.ID)
		}
		return
	}
	base := it.Inputs.Over(b.Inputs)
	for _, c := range it.Matrix.Combos(base) {
		if miss := c.Missing(); len(miss) > 0 {
			add("%s: benchmark %q needs the input %s (give it in the entry's inputs or matrix, or as the benchmark's default)", p, b.ID, strings.Join(miss, " and "))
			return
		}
	}
}

// ValidateGroup is Validate for a group that may contain other groups:
// groups finds them (nil: a group entry is refused).
func (s *Suite) ValidateGroup(profileOK func(string) bool, look Lookup, groups GroupLookup) error {
	err := s.validate(profileOK, look)
	var errs []error
	if err != nil {
		errs = append(errs, err)
	}
	add := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	for i, it := range s.Benchmarks {
		s.validateEntry(i, it, look, groups, add)
	}
	return errors.Join(errs...)
}

func cloneBenchmark(b *Benchmark) *Benchmark {
	raw, _ := json.Marshal(b)
	var c Benchmark
	_ = json.Unmarshal(raw, &c)
	return &c
}

// ExpandedID is the id of a recipe's use with these inputs: the recipe id and
// a short hash of the inputs, so the same use has the same id in every run.
func ExpandedID(recipe string, in Inputs) string {
	in.Placement = &[]Placement{in.Place()}[0]
	raw, _ := json.Marshal(in)
	base := recipe
	if len(base) > 57 {
		base = base[:57]
	}
	return base + "." + shortHash(string(raw), 6)
}

// Use is the benchmark a recipe becomes with these inputs: an inline copy
// with the generated profile, Recipe and Applied set, and no inputs.
func Use(b *Benchmark, in Inputs) *Benchmark {
	c := cloneBenchmark(b)
	if in.Placement == nil {
		in.Placement = &Placement{Mode: PlaceAuto}
	}
	c.ID = ExpandedID(b.ID, in)
	c.Name = fmt.Sprintf("%s (%s)", b.Title(), in.Summary())
	c.Recipe = b.ID
	c.Applied = &in
	c.Inputs = nil
	c.Profile = in.ProfileID()
	return c
}

// ExpandGroup is the flat list a run of the group executes: nested groups
// spliced in where they stand, every use of a recipe (once per matrix
// combination) an inline benchmark (Use), everything else as it was. The
// group itself is not changed; the copy keeps its options.
func ExpandGroup(s *Suite, look Lookup, groups GroupLookup) (*Suite, error) {
	out := *s
	out.Benchmarks = nil
	var walk func(g *Suite, depth int, path []string) error
	walk = func(g *Suite, depth int, path []string) error {
		for i, it := range g.Benchmarks {
			if len(out.Benchmarks) > MaxExpandedItems {
				return fmt.Errorf("the group expands to more than %d benchmarks", MaxExpandedItems)
			}
			where := fmt.Sprintf("%s: benchmarks[%d]", strings.Join(path, " > "), i)
			if it.Group != "" {
				if depth >= MaxGroupDepth {
					return fmt.Errorf("%s: groups are nested deeper than %d", where, MaxGroupDepth)
				}
				for _, id := range path {
					if id == it.Group {
						return fmt.Errorf("%s: group %q contains itself", where, it.Group)
					}
				}
				var inner *Suite
				var ok bool
				if groups != nil {
					inner, ok = groups(it.Group)
				}
				if !ok {
					return fmt.Errorf("%s: %q is not a group", where, it.Group)
				}
				if err := walk(inner, depth+1, append(path, it.Group)); err != nil {
					return err
				}
				continue
			}
			b := it.Inline
			if b == nil {
				var ok bool
				if look != nil {
					b, ok = look(it.Ref)
				}
				if !ok {
					// Left for the caller to report, as before groups existed.
					out.Benchmarks = append(out.Benchmarks, SuiteItem{Ref: it.Ref})
					continue
				}
			}
			if !b.IsRecipe() {
				if it.Inputs != nil || it.Matrix != nil {
					return fmt.Errorf("%s: benchmark %q has a fixed profile and takes no inputs", where, b.ID)
				}
				out.Benchmarks = append(out.Benchmarks, SuiteItem{Ref: it.Ref, Inline: it.Inline})
				continue
			}
			for _, in := range it.Matrix.Combos(it.Inputs.Over(b.Inputs)) {
				if miss := in.Missing(); len(miss) > 0 {
					return fmt.Errorf("%s: benchmark %q needs the input %s", where, b.ID, strings.Join(miss, " and "))
				}
				out.Benchmarks = append(out.Benchmarks, SuiteItem{Inline: Use(b, in)})
			}
		}
		return nil
	}
	if err := walk(s, 0, []string{s.ID}); err != nil {
		return nil, err
	}
	if len(out.Benchmarks) > MaxExpandedItems {
		return nil, fmt.Errorf("the group expands to %d benchmarks, more than %d", len(out.Benchmarks), MaxExpandedItems)
	}
	return &out, nil
}
