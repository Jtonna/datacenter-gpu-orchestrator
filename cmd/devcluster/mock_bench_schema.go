// DEV-ONLY MOCK of GET /api/bench/schema. mock_bench_schema.json is the
// output of the real benchspec.Schema() (feat/bench-phases), served as is;
// the mock also reads it to check benchmark bodies.

package main

import (
	_ "embed"
	"encoding/json"
)

//go:embed mock_bench_schema.json
var benchSchemaJSON []byte

type benchOption struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"` // string, text, integer, number, boolean, list, request
	ItemType string   `json:"item_type"`
	Required bool     `json:"required"`
	Min      *float64 `json:"min"`
	Max      *float64 `json:"max"`
}

type benchPhaseType struct {
	Type    string        `json:"type"`
	Title   string        `json:"title"`
	Options []benchOption `json:"options"`
}

type benchSchemaDoc struct {
	PhaseTypes       []benchPhaseType   `json:"phase_types"`
	RequestOptions   []benchOption      `json:"request_options"`
	TurnOptions      []benchOption      `json:"turn_options"`
	BenchmarkOptions []benchOption      `json:"benchmark_options"`
	Limits           map[string]float64 `json:"limits"`
}

var benchSchema = func() benchSchemaDoc {
	var d benchSchemaDoc
	if err := json.Unmarshal(benchSchemaJSON, &d); err != nil {
		panic("mock_bench_schema.json: " + err.Error())
	}
	return d
}()

func phaseTypeDef(t string) (benchPhaseType, bool) {
	for _, p := range benchSchema.PhaseTypes {
		if p.Type == t {
			return p, true
		}
	}
	return benchPhaseType{}, false
}

func optionIn(l []benchOption, name string) (benchOption, bool) {
	for _, o := range l {
		if o.Name == name {
			return o, true
		}
	}
	return benchOption{}, false
}

func fp(v float64) *float64 { return &v }
